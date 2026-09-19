package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	node "k8s.io/api/node/v1"
	rbac "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	kmeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	kubeapistatus "gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubediscovery"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// ModelDeploymentReconciler reconciles v1alpha1.ModelDeployment objects to finish the following
// tasks:
//   - Render one Kubernetes Pod per role, owned by the ModelDeployment and carrying the entrance
//     label that routes it into the role's pool, as the template every replica of that role is
//     created from. The API server names each replica, so a replacement never inherits the name of
//     the replica it replaces.
//   - Converge that set continuously: create a replica the role is short of once Kueue asks for it,
//     remove one beyond the role's declared count, and replace one whose spec no longer matches
//     what it was built from.
//
// It creates NO Instance. An Instance renders exactly one Pod and its spec is immutable after
// creation, so routing replicas through it would make "one replica, several Pods" inexpressible and
// degenerate every rollout into recreate-everything. The admission chain keys on Pods, and a plain
// Pod is a first-class citizen of it.
type ModelDeploymentReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
	Recorder  ctrlrecord.EventRecorder

	// CacheScraper reads each replica's own account of its cache client. It is an interface rather
	// than a dial this reconciler makes, because every case the condition it feeds has to get right
	// is a failure, and a real dial cannot be made to fail on demand.
	//
	// A nil scraper is every replica unreadable, which the condition reports as Unknown. It is nil
	// today: the concrete per-engine reader is not written, and inventing a metric name would be the
	// exact assumption the condition exists to refuse.
	CacheScraper ModelDeploymentCacheScraper
}

var _ ctrlreconcile.Reconciler = (*ModelDeploymentReconciler)(nil)

func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	md := new(workercore.ModelDeployment)
	err := r.Client.Get(ctx, req.NamespacedName, md)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch model deployment")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Clean up if the ModelDeployment is marked as deleted.
	if md.DeletionTimestamp != nil {
		return r.teardownModelDeployment(ctx, md)
	}

	// Lock.
	if !systemmeta.Lock(md) {
		err = r.Client.Update(ctx, md)
		if err != nil {
			logger.Error(err, "lock model deployment")
			return ctrl.Result{}, err
		}
	}

	return r.convergeModelDeployment(ctx, md)
}

// teardownModelDeployment deletes the replicas and releases the finalizer once they are gone.
//
// The finalizer is held until the last Pod has actually left rather than dropped as soon as the
// deletes are issued, because a replica that outlives its owner keeps holding accelerators that the
// admission ledger has already stopped accounting for.
func (r *ModelDeploymentReconciler) teardownModelDeployment(
	ctx context.Context, md *workercore.ModelDeployment,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	if systemmeta.IsLocked(md) {
		pods, err := r.listModelDeploymentPods(ctx, md)
		if err != nil {
			logger.Error(err, "list replicas")
			return ctrl.Result{}, err
		}

		if len(pods) > 0 {
			// Report the phase before issuing the deletes, so an operator watching the object sees
			// Deleting rather than the last Ready it happened to reach. The Binding is deliberately
			// not re-read: a teardown pass has no question to ask it, and the domain a replica is
			// still writing into is the one that was last observed.
			if err = r.syncModelDeploymentStatus(ctx, md, pods, nil, nil); err != nil {
				logger.Error(err, "update model deployment status to deleting")
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}

			for i := range pods {
				if pods[i].DeletionTimestamp != nil {
					continue
				}
				if err = r.Client.Delete(ctx, &pods[i]); err != nil && !kerrors.IsNotFound(err) {
					logger.Error(err, "delete replica", "pod", pods[i].Name)
					return ctrl.Result{}, err
				}
			}

			// Deleting the replicas is not enough to make them leave. Kueue holds a finalizer on every
			// Pod of a group it manages, and the group this operator builds is annotated as SERVING —
			// which Kueue defines as a group that never finishes, so the terminal replicas are read as
			// awaiting replacement and their finalizers are never released. The one path that does
			// release them is the Workload being deleted.
			//
			// Nothing else breaks the cycle. That Workload's only owners are the very Pods that cannot
			// leave, and they own it without a controller reference, so garbage collection waits for
			// all of them; there is no replacement timeout to fall back on. Left alone the deployment
			// stays in Deleting forever, which is what a run measured before this delete was added.
			if err = r.deleteModelDeploymentGroupWorkload(ctx, md, pods); err != nil {
				logger.Error(err, "delete group workload")
				return ctrl.Result{}, err
			}

			logger.V(3).Info("replica deletion in progress; requeue in 2s", "replicas", len(pods))

			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}

		// The last replica has left, so the claim on the Binding can go. Released any earlier, the
		// authorization could be deleted from under a process that is still writing through it.
		if err := r.releaseModelDeploymentBinding(ctx, md); err != nil {
			logger.Error(err, "release kv cache pool binding")
			return ctrl.Result{}, err
		}
	}

	// Unlock.
	if systemmeta.Unlock(md) {
		logger.V(3).Info("skip deleted model deployment")
		return ctrl.Result{}, nil
	}

	err := r.Client.Update(ctx, md)
	if err != nil {
		logger.Error(err, "unlock model deployment")
	}

	return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
}

// convergeModelDeployment brings the rendered replicas in line with the spec.
//
// It is level-based: it renders what the spec says should exist, compares that with what does, and
// issues only the difference. A pass over a spec that has not changed writes nothing, which is what
// keeps a controller that runs on every Pod event from rewriting the world.
func (r *ModelDeploymentReconciler) convergeModelDeployment(
	ctx context.Context, md *workercore.ModelDeployment,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Resolved first, and its verdict deliberately does NOT gate the convergence below. A Binding
	// that is missing or briefly not usable — a store leader restart makes every Binding not-Ready
	// for tens of seconds — leaves the replicas serving; the condition is the signal. Refusing to
	// converge here would turn a routine upgrade of the store into an outage of every deployment on
	// it.
	domain, err := r.resolveModelDeploymentDomain(ctx, md)
	if err != nil {
		logger.Error(err, "resolve kv cache pool binding")
		return ctrl.Result{}, err
	}

	// Claimed whatever the verdict was, so long as the Binding could be read at all: the claim is
	// what holds an admin's delete off a deployment that is still writing, and a claim that came and
	// went with readiness would open exactly that window.
	if err = r.claimModelDeploymentBinding(ctx, md); err != nil {
		logger.Error(err, "claim kv cache pool binding")
		return ctrl.Result{}, err
	}

	// Resolved once per pass, not per role: the endpoint and the transport belong to the pool and its
	// backend. A nil result means there is nothing to connect to yet, and the replicas are rendered
	// without a connector rather than with a partial one.
	connection, err := r.resolveModelDeploymentConnection(ctx, domain)
	if err != nil {
		logger.Error(err, "resolve kv cache connection")
		return ctrl.Result{}, err
	}

	desired, err := r.renderModelDeploymentPods(ctx, md, connection)
	if err != nil {
		// A render failure is the InstanceType not being ready, or a role the renderer cannot build
		// a container from. The pass aborts before any status is written, so an Event is the only
		// place the cause reaches a reader: the conditions still say "no replica has been created
		// yet", which describes a slow start and a permanent failure identically.
		// Guarded like both other emission paths in this file: Recorder is populated only by
		// SetupController, so a reconciler built directly -- which every test here does -- may
		// legitimately carry none.
		if r.Recorder != nil {
			r.Recorder.Event(md, core.EventTypeWarning, modelDeploymentEventRenderFailed, err.Error())
		}
		logger.Error(err, "render replicas")

		return ctrl.Result{}, err
	}

	actual, err := r.listModelDeploymentPods(ctx, md)
	if err != nil {
		logger.Error(err, "list replicas")
		return ctrl.Result{}, err
	}

	// F10: a change to any role's replicas, or to the set of roles, moves the group's declared total,
	// and Kueue refuses to compose a Workload for a group whose Pods disagree on it. So such a change
	// is a REBUILD of the whole group rather than a per-replica rollout: while any Pod still declares
	// the old total, the survivors are deleted and NOTHING is created.
	//
	// The create is what has to wait, and that is the whole point of the flag. Deleting alone would
	// not be enough: a Pod that has been asked to terminate is still a member of the group until it
	// actually goes, so adding the new replica beside it produces exactly the mixed-total state this
	// exists to avoid -- and that state's symptom is no Workload at all, which reads like the
	// operator having stopped rather than like a rollout in progress.
	//
	// The predicate reads the terminating Pods too, for the same reason.
	//
	// IT ANSWERS PER GROUP. A deployment whose roles sit on different instanceTypes is several groups,
	// and one group's shape moving is no reason to restart a group that did not move.
	resizing := modelDeploymentGroupsResizing(md, actual)

	// A DEPARTING REPLICA NO LONGER REBUILDS THE GROUP, and what opened the replacement path is the
	// naming above rather than anything Kueue grew. Measured on the version this project runs:
	// Kueue keeps a group's Workload admitted when a member disappears, reports the gap as the
	// WaitingForReplacementPods condition on that Workload, and finalizes the departed Pod as soon
	// as a replacement carrying the same role hash exists -- so replacing one replica is a path
	// Kueue supports. The condition is READ below rather than re-derived, because the predicate
	// deciding whether a departed Pod still counts is Kueue's, it has three branches, and a copy of
	// it here would agree until the day it did not.
	//
	// What used to close that path was this operator's own naming: a slot name derived from the
	// ordinal was held by the departed Pod until its finalizer released, and the replacement could
	// not be created under it. Replacements now carry a fresh server-assigned name, so nothing
	// blocks the create, and a departure costs the group nothing. Deleting the Workload -- which
	// stops every member -- is paid by a RESIZE alone, where the totals baked into the members
	// leave no lesser option.
	//
	// IT IS STILL PER GROUP. A departure is a question for the group's own Workload and no other's:
	// the finalizer that holds the departed Pod is held by that Workload alone.
	rebuild := resizing

	// The survivors are deleted here rather than left to the stop Kueue performs when the Workload
	// goes. Relying on that would make the group's teardown a side effect of another controller's
	// answer to our delete, observable only on a cluster that runs it.

	// NO SUPPRESSION SET STANDS BETWEEN THE DELETES AND THE CREATES, and the reason is the shape of
	// the decisions rather than a guard beside them. Every count below is taken from the member list
	// this pass started from, so a role this pass removes a member from still sits at its declared
	// count in that list and creates nothing -- and a role short of its declared count is one whose
	// members were left standing, so the pass deletes nothing from it. A group cannot receive a
	// create and a delete from the same pass, and the mixed-member state the old guard existed to
	// keep out of the group can no longer be assembled.

	// What this pass decides about replicas carrying an earlier spec, for the condition that reports
	// it. A rebuild pass records nothing: it deletes every replica without comparing a hash, so it
	// has no answer to give and passes nil below rather than a zeroed one.
	var rollout modelDeploymentRollout

	// The live members are collected by role first, because the comparison this convergence makes is
	// per role: how many Pods the role owns, whether each matches the template the role renders, and
	// how many the spec declares. A Pod already on its way out is counted by neither side -- it is
	// still a member of its group, but it is not one the spec can keep.
	liveByRole := make(map[string][]*core.Pod, len(md.Spec.Roles))
	for i := range actual {
		pod := &actual[i]
		if pod.DeletionTimestamp != nil {
			// Already on its way out; a create issued now would race the delete and be rejected.
			continue
		}

		if group := pod.Labels[kueuepodconst.GroupNameLabel]; rebuild.Has(group) {
			logger.Info("rebuilding the pod group",
				"pod", pod.Name, "group", group, "resizing", resizing.Has(group))
			if err = r.Client.Delete(ctx, pod); err != nil && !kerrors.IsNotFound(err) {
				logger.Error(err, "delete replica of the rebuilt group", "pod", pod.Name)
				return ctrl.Result{}, err
			}

			continue
		}

		role := modelDeploymentPodRole(pod)
		if _, wanted := desired[role]; !wanted {
			// Scaled away, or renamed by a role rename.
			logger.Info("removing replica of a role no longer in the spec", "pod", pod.Name)
			if err = r.Client.Delete(ctx, pod); err != nil && !kerrors.IsNotFound(err) {
				logger.Error(err, "delete replica", "pod", pod.Name)
				return ctrl.Result{}, err
			}

			continue
		}

		liveByRole[role] = append(liveByRole[role], pod)
	}

	// A delete this pass issued does not end it: the observations below run either way. Returning
	// instead would mean that during exactly the window a departure event is for — a replica on its
	// way out — no event and no status were written at all.
	var requeue bool
	if rebuild.Len() > 0 {
		// The new group is created by the pass that finds the old one gone, in one go, exactly as a
		// deployment that never had one is. Requeuing rather than returning keeps the status write
		// below on the path, so the rebuild is visible while it happens.
		requeue = true

		// Without this the deletes above are requests nobody can honor: the replicas carry Kueue's
		// finalizer, and this Workload is the only thing whose removal releases it.
		//
		// ONE CALL PER REBUILDING GROUP, scoped to that group's own replicas. The lookup matches a
		// Workload by the Pods it owns, so handing it the whole set would reach every group's
		// Workload -- including those of groups this pass is leaving alone, whose replicas would then
		// be stopped by Kueue for a change that did not concern them.
		for _, group := range sets.List(rebuild) {
			members := modelDeploymentPodsInGroup(actual, group)
			if len(members) == 0 {
				// A group named only because a role is arriving in it: it has no replicas yet, so
				// there is no Workload and nothing to release.
				continue
			}
			if err = r.deleteModelDeploymentGroupWorkload(ctx, md, members); err != nil {
				logger.Error(err, "delete group workload", "group", group)
				return ctrl.Result{}, err
			}
		}
	}

	// The convergence itself, one role at a time, and THE COUNT MOVES BEFORE THE CURRENCY: a role
	// short of its declared count does not roll its outdated members in the same breath, because
	// deleting from a group that is already short widens exactly the gap the create gate below is
	// waiting to close. A role's currency is worth nothing until its count is right.
	// ONE LIST OF WORKLOADS PER PASS, TAKEN ONLY IF A ROLE ASKS FOR IT. The create gate below reads
	// a Kueue-written condition, so the read stays uncached -- the informer lags the writer and the
	// gate would act on the previous pass's answer. What is not defensible is issuing it once per
	// short role: the list is the namespace's and the answer is the same for every role, while this
	// pass requeues every couple of seconds for as long as any role is short. Deferring it to first
	// need keeps a deployment that is merely rolling from paying for a read nobody looks at.
	listWorkloads := sync.OnceValues(func() ([]kueue.Workload, error) {
		wlList := new(kueue.WorkloadList)
		if err := r.APIReader.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace)); err != nil {
			return nil, fmt.Errorf("list workloads: %w", err)
		}

		return wlList.Items, nil
	})

	createCount := make(map[string]int, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		group := modelDeploymentPodGroupFor(md, role.Name)
		live := liveByRole[role.Name]
		declared := int(role.Replicas)
		wantHash := desired[role.Name].Annotations[modelDeploymentPodSpecHashAnnotation]

		// THE SURPLUS IS SHED NEWEST FIRST, and before any hash is compared, so the members the
		// count keeps are the longest-serving ones. This is the state a replacement created while
		// the departed Pod was still active leaves behind -- one more live Pod than the group's
		// PodSet declares -- and leaving it standing would have Kueue evict a member of its own
		// choosing on every pass.
		var shed bool
		if surplus := len(live) - declared; surplus > 0 {
			modelDeploymentSortReplicasNewestFirst(live)
			for _, pod := range live[:surplus] {
				logger.Info("removing replica beyond the role's declared count", "pod", pod.Name)
				if err = r.Client.Delete(ctx, pod); err != nil && !kerrors.IsNotFound(err) {
					logger.Error(err, "delete surplus replica", "pod", pod.Name)
					return ctrl.Result{}, err
				}
			}
			live = live[surplus:]
			shed = true
			requeue = true
		}

		// Counted before the comparison rather than after: reaching this line is what makes the pass
		// able to answer at all, and a pass that answers "nothing outdated" without having got here
		// is reporting that it looked, not what it found.
		var outdated []*core.Pod
		for _, pod := range live {
			rollout.accounted++

			if pod.Annotations[modelDeploymentPodSpecHashAnnotation] == wantHash {
				continue
			}

			rollout.outdated++
			outdated = append(outdated, pod)
		}

		if connection == nil && md.Status.KVCache != nil && len(outdated) > 0 {
			// LOSING THE CONNECTOR IS NOT A REASON TO REBUILD A REPLICA, and without this the hash
			// makes it one. A pass that cannot resolve the connection renders replicas without a
			// connector, so every running replica's hash differs from the desired one and the
			// recreate below would fire on all of them at once.
			//
			// The guard is BOTH halves. A deployment that never resolved a domain has no connector
			// to lose, so a hash difference there comes from the spec and must still roll out --
			// suppressing it on `connection == nil` alone breaks the ordinary rollout of every
			// deployment that does not use a cache at all, which is what the spec-change test says
			// when this branch is written without the second half.
			//
			// What reaches that state is ordinary rather than exotic: a store leader restart makes
			// every Binding on the pool briefly not-Ready -- measured at 3.5 to 32 seconds in this
			// project -- and a deployment whose Binding is not usable resolves no connection. So a
			// few seconds of store unavailability would delete every replica of every deployment on
			// that pool, and each one then reloads its weights. The blip becomes the outage.
			//
			// The cost is that this also withholds a spec change that moves a replica's rendered Pod
			// without moving the group's shape, until the connection returns.
			//
			// THE SPLIT THAT WOULD AVOID IT IS REFUSED, and not for the price it was first parked on.
			// That design keeps a base hash over the spec without the connector beside a separate
			// connector fingerprint, so a spec edit rolls from the base hash while only the fingerprint
			// comparison is skipped here. Acting on that precision is what costs: the base hash
			// difference deletes a replica, any replica leaving rebuilds the whole group, the group is
			// rebuilt by a pass that still has no connector to give it, and when the store returns the
			// desired render regains the connector and rebuilds it again. Where an edit is waiting, the
			// split pays two whole-group reloads in place of one.
			//
			// The operator still has that trade, which is what bounds the wait: a change to the replica
			// counts or to the set of roles moves the group annotations and takes the rebuild branch
			// above, which runs before this guard and proceeds during an outage, carrying the withheld
			// edit at exactly that price. Refused here is making the trade on behalf of someone who was
			// not told what it costs.
			//
			// Leaving them alone is also what this design already decided for the neighboring case:
			// an admin deleting the Binding leaves running Pods running, because tearing down a
			// serving deployment because an admin object vanished is worse than serving without a
			// cache. The same reasoning covers a Binding that is merely unwell.
			//
			// Nothing is lost when it comes back. The connector resolves to the same values, the
			// desired hash returns to what these replicas already carry, and this branch stops
			// firing. If it comes back with a DIFFERENT endpoint, the hash differs from the one they
			// carry and they are recreated on that pass -- which is the rollout that should happen.
			//
			// New replicas are still created below, without a connector: a replica that does not
			// exist yet cannot be given an address that does not exist yet either.
			logger.V(3).Info("no connection this pass; leaving the replicas as built", "role", role.Name)
			rollout.held += len(outdated)
			outdated = nil
		}

		// The rollout is recreate rather than surge: a replica built before a spec change is deleted
		// here and replaced by a later pass. The cost is this replica's cached blocks, which its
		// siblings lose when it goes.
		//
		// AT MOST ONE PER ROLE PER PASS, never while the role is short of its count, and never
		// beside a surplus this pass already shed. Kueue admits the group as one Workload, and a
		// replacement created before the departed member goes inactive reads as one member over the
		// count: Kueue's answer to that excess is to delete the newest un-finalized gated Pod, which
		// is the replacement itself. One departure at a time, waited out, is the whole cadence.
		if len(outdated) > 0 && !shed && len(live) == declared {
			modelDeploymentSortReplicasNewestFirst(outdated)
			pod := outdated[0]
			logger.Info("recreating replica built from an earlier spec", "pod", pod.Name)
			if err = r.Client.Delete(ctx, pod); err != nil && !kerrors.IsNotFound(err) {
				logger.Error(err, "delete outdated replica", "pod", pod.Name)
				return ctrl.Result{}, err
			}
			requeue = true
		}

		// THE CREATE GATE. A role short of its declared count is created for on Kueue's own ask, or
		// freely when no Workload exists to ask: a group short of its total composes no Workload at
		// all, so waiting on a condition that cannot exist would deadlock a deployment's first pass.
		// A group this pass is rebuilding creates nothing either -- its members are still draining,
		// and a create beside them is the mixed-total state the rebuild exists to avoid.
		if short := declared - len(live); short > 0 && !rebuild.Has(group.Name) {
			// A GROUP WITH NO MEMBERS IS NOT ASKED, and the check sits here rather than inside the
			// call so that it also skips the read: there is no Workload to have an opinion yet, and
			// the branch below creates freely, so paying for an API-server list would buy nothing.
			var asks, found bool
			if members := modelDeploymentPodsInGroup(actual, group.Name); len(members) > 0 {
				workloads, askErr := listWorkloads()
				if askErr != nil {
					logger.Error(askErr, "read the group workload", "group", group.Name)
					return ctrl.Result{}, askErr
				}
				asks, found = modelDeploymentGroupWorkloadAsksForReplacement(workloads, members)
			}

			switch {
			case found && !asks:
				// Kueue has not asked for a replacement. Creating one now would put one more active
				// Pod in the group than its PodSet declares, and Kueue's answer to that excess is
				// to delete the newest un-finalized Pod -- the replacement this pass just built. The
				// pass comes back instead; the ask it waits for is a condition flip nothing here
				// watches, and the requeue is what covers it.
				requeue = true
			default:
				createCount[role.Name] = short
			}
		}
	}

	// A FAILED CREATE DOES NOT END THE PASS EITHER, and that is what makes the incomplete group a
	// REPORTED state rather than a silent one. Returning here would skip the status write below, so
	// the one pass that knows the group is short of its total would be the one pass that says
	// nothing — and the symptom of an incomplete group is already silence: Pods exist, they are
	// gated, and Kueue composes no Workload at all.
	//
	// The remaining creates are still issued, because the group needs every one of them before
	// anything is admitted; stopping at the first failure would leave the group shorter than it had
	// to be. The error is returned after the status is written, so the pass is still not treated as
	// successful and the missing replicas are retried.
	var createErr error
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		for range createCount[role.Name] {
			// One fresh object per replica: the template is every replica of the role's, and the
			// API server names each instance separately.
			pod := desired[role.Name].DeepCopy()
			if err = r.Client.Create(ctx, pod); err != nil {
				logger.Error(err, "create replica", "role", role.Name)
				if createErr == nil {
					createErr = err
				}

				continue
			}
			logger.Info("created replica", "pod", pod.Name)
			// A replica this pass just rendered and created is current by construction, so the pass
			// can answer for it. Without this a deployment's first pass could not answer at all, and
			// its second pass would write a status for a spec nobody changed.
			rollout.accounted++
		}
	}

	if err = r.syncModelDeploymentService(ctx, md); err != nil {
		logger.Error(err, "sync service")
		return ctrl.Result{}, err
	}
	// A ROUTER SYNC FAILURE DOES NOT ABORT THE PASS, for the same reason a failed create does not:
	// the cause is projected onto the status written below, and returning here would skip the one
	// write that says what is wrong. The error is still returned after that write, so the pass is
	// retried exactly as if it had failed here.
	routerErr := r.syncModelDeploymentRouter(ctx, md)
	if routerErr != nil {
		logger.Error(routerErr, "sync router")
	}

	// Read the replicas back rather than reusing the list this pass started from: status must
	// describe what exists now, not the snapshot the convergence decided against.
	actual, err = r.listModelDeploymentPods(ctx, md)
	if err != nil {
		logger.Error(err, "list replicas")
		return ctrl.Result{}, err
	}

	r.recordModelDeploymentDepartures(md, actual)

	if err = r.syncModelDeploymentStatus(ctx, md, actual, domain, &rollout); err != nil {
		logger.Error(err, "sync status")
		return ctrl.Result{}, err
	}

	// Returned only now, so the pass is not treated as successful and the missing replicas are
	// retried — but after the status has told a reader why the group is short.
	if createErr != nil {
		return ctrl.Result{}, createErr
	}

	if routerErr != nil {
		return ctrl.Result{}, routerErr
	}

	if requeue {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// recordModelDeploymentDepartures records one event per replica that has left, so an operator
// correlating a burst of failed requests with a preemption has the correlation written down rather
// than having to infer it.
func (r *ModelDeploymentReconciler) recordModelDeploymentDepartures(
	md *workercore.ModelDeployment, pods []core.Pod,
) {
	if r.Recorder == nil {
		return
	}

	for _, d := range modelDeploymentReplicaDepartures(pods) {
		r.Recorder.Event(md, core.EventTypeWarning, d.reason, d.message)
	}
}

// recordModelDeploymentRuntimeVersionSkew records that a role's pool disagrees on a runtime
// version, and does so only for a role whose image the operator SYNTHESIZES.
//
// A role that states its own image is unaffected by the pool's version spread: the operator did not
// choose that tag and the spread tells its owner nothing actionable. Emitting it anyway would train
// readers to ignore the reason they need when the image IS synthesized.
//
// It fires on every pass while the disagreement lasts, which is deliberate. A driver rollout is a
// standing condition rather than an edge, and the API server folds repeats of one
// (object, reason, message) into a single event with a count -- so a standing warning stays visible
// for as long as it is true instead of scrolling away, which is precisely what an operator
// diagnosing an ImagePullBackOff hours later needs.
func (r *ModelDeploymentReconciler) recordModelDeploymentRuntimeVersionSkew(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, instType *worker.InstanceType,
) {
	if r.Recorder == nil {
		return
	}
	if role.Image != "" {
		return
	}

	message, ok := modelDeploymentRuntimeVersionSkew(role.Name, instType.Status.Detail)
	if !ok {
		return
	}

	r.Recorder.Event(md, core.EventTypeWarning, modelDeploymentEventRuntimeVersionSkew, message)
}

// syncModelDeploymentService converges every Service the deployment owns: the one it is reached
// through, and one per role.
//
// It aligns an existing Service rather than replacing it, because the allocated ClusterIP is state
// the operator did not write and every client that resolved the name is still using. It is also why
// a Service is NOT rebuilt when the group is: the group's shape decides which Pods exist, and the
// address they answer on must survive that.
func (r *ModelDeploymentReconciler) syncModelDeploymentService(
	ctx context.Context, md *workercore.ModelDeployment,
) error {
	rendered := renderModelDeploymentServices(md, r.modelDeploymentRoleManufacturers(ctx, md))
	expected := make([]ctrlcli.Object, len(rendered))
	for i := range rendered {
		expected[i] = rendered[i]
	}

	return r.syncModelDeploymentOwnedChildren(
		ctx,
		md,
		expected,
		func() ctrlcli.Object { return new(core.Service) },
		new(core.ServiceList),
		func(actual, expected ctrlcli.Object) bool {
			// The metadata align is what keeps a role-owned Service inside the prune gate: the
			// resource note is written at Create only, and the gate exempts a child whose note is
			// gone.
			changed := alignModelDeploymentService(actual.(*core.Service), expected.(*core.Service))
			return alignModelDeploymentChildMetadata(actual, expected) || changed
		},
		ModelDeploymentResourceNoteRole,
		"service",
	)
}

// syncModelDeploymentOwnedChildren creates, aligns and prunes one kind of rendered child.
func (r *ModelDeploymentReconciler) syncModelDeploymentOwnedChildren(
	ctx context.Context,
	md *workercore.ModelDeployment,
	expected []ctrlcli.Object,
	newObject func() ctrlcli.Object,
	actualList ctrlcli.ObjectList,
	align func(actual, expected ctrlcli.Object) bool,
	resourceNote,
	kind string,
) error {
	wanted := sets.New[string]()
	for _, want := range expected {
		wanted.Insert(want.GetName())

		actual := newObject()
		err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(want), actual, ctrlclix.WithoutQuorum)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return err
			}

			if err = r.Client.Create(ctx, want); err != nil && !kerrors.IsAlreadyExists(err) {
				return err
			}
			continue
		}

		if !modelDeploymentOwns(actual, md) {
			return fmt.Errorf("%s %s/%s is not owned by this deployment",
				kind, actual.GetNamespace(), actual.GetName())
		}

		if align(actual, want) {
			if err = r.Client.Update(ctx, actual); err != nil {
				return err
			}
		}
	}

	logger := ctrllog.FromContext(ctx)
	err := r.Client.List(ctx, actualList,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
		},
		ctrlclix.WithoutQuorum)
	if err != nil {
		return err
	}

	return kmeta.EachListItem(actualList, func(obj runtime.Object) error {
		actual := obj.(ctrlcli.Object)
		if wanted.Has(actual.GetName()) ||
			!modelDeploymentOwns(actual, md) ||
			systemmeta.DescribeResourceNote(actual, resourceNote) == "" {
			return nil
		}

		logger.Info("removing a child no longer in the spec", kind, actual.GetName())
		if err = r.Client.Delete(ctx, actual); err != nil && !kerrors.IsNotFound(err) {
			return err
		}

		return nil
	})
}

// renderModelDeploymentPods renders one Pod per role, keyed by role name.
//
// ONE POD PER ROLE rather than one per replica: the spec declares a count of interchangeable
// replicas and carries no identity for any one of them, so what the render produces is the template
// the count is created from, and the API server names each instance. The converge loop compares and
// creates by role and count for exactly that reason -- a suffix the API server assigns is nothing a
// name-keyed loop could predict or match.
func (r *ModelDeploymentReconciler) renderModelDeploymentPods(
	ctx context.Context, md *workercore.ModelDeployment,
	connection *ModelDeploymentConnectorInput,
) (map[string]*core.Pod, error) {
	// The overcommit setting is the Instance path's, deliberately: it decides how a declared
	// resource becomes a request, and this renderer derives the same values the Instance webhook
	// does. A second knob for one translation would let the two disagree on one cluster.
	overcommit := settings.InstanceGeneralResourcesOvercommit.ShouldValueBool(ctx)

	// Resolved once per reconcile rather than per role: the answer is the cluster's, not the
	// role's, and it reads the version snapshot configured at startup, so it costs no API call.
	clusterVersion := system.LoopbackKubeVersion.Get()
	nativeSidecar := kubediscovery.SupportsFeature(&clusterVersion, kubediscovery.FeatureNativeSidecar)

	desired := make(map[string]*core.Pod)
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]

		instType, err := r.getModelDeploymentInstanceType(ctx, role.InstanceType)
		if err != nil {
			return nil, err
		}

		// Recorded here rather than in a pass of its own, because this is the one place that
		// already holds both the role and its InstanceType: a second loop would read the same
		// object again for a message.
		r.recordModelDeploymentRuntimeVersionSkew(md, role, instType)

		in := ModelDeploymentRenderInput{
			Deployment:                 md,
			Role:                       role,
			InstanceType:               instType,
			RuntimeClassName:           r.getModelDeploymentRuntimeClassName(ctx, instType),
			GeneralResourcesOvercommit: overcommit,
			NativeSidecar:              nativeSidecar,
		}

		// The connector is synthesized PER ROLE even though its connection is per deployment,
		// because the accelerator is the role's: it selects the store connector the engine
		// registers, and only the role's InstanceType knows it.
		//
		kvTransfer := modelDeploymentUsesKVTransfer(md, role, instType.Status.Detail.Manufacturer)
		publishKVEvents := modelDeploymentPublishesKVEvents(md, role, instType.Status.Detail.Manufacturer)
		if connection != nil || kvTransfer || publishKVEvents {
			roleConnection := ModelDeploymentConnectorInput{}
			if connection != nil {
				roleConnection = *connection
			}
			roleConnection.Engine = md.Spec.Engine.Name
			roleConnection.Manufacturer = instType.Status.Detail.Manufacturer
			// The kind is the role's, and it is the only per-role term in the synthesized
			// configuration: it is what makes a prefiller and a decoder two configurations rather
			// than two copies of one, which is what the atomic admission of the pair is FOR.
			roleConnection.Kind = role.Kind
			roleConnection.KVTransfer = kvTransfer
			if md.Spec.KVTransfer != nil {
				roleConnection.KVTransferProtocol = md.Spec.KVTransfer.Protocol
			}
			roleConnection.PublishKVEvents = publishKVEvents
			if roleConnection.PublishKVEvents {
				roleConnection.KVEventsHost = md.Name + "-" + role.Name + "." + md.Namespace + ".svc"
			}

			connector, err := SynthesizeModelDeploymentConnector(roleConnection)
			if err != nil {
				return nil, fmt.Errorf("role %q cannot be given a cache client: %w", role.Name, err)
			}
			in.Connector = connector
		}

		pod, err := renderModelDeploymentPod(ctx, in)
		if err != nil {
			return nil, err
		}
		desired[role.Name] = pod
	}

	return desired, nil
}

// kueueWorkloadWaitingForReplacementPods is the condition Kueue's pod integration sets on a group's
// Workload while the group holds fewer active Pods than its PodSet declares.
//
// IT IS SPELLED HERE RATHER THAN IMPORTED because of where the constant lives in the kueue this
// project compiles against: pkg/controller/jobs/pod, a whole controller implementation whose import
// would buy one string. Kueue v0.18 moved it into apis/kueue/v1beta2, so an upgrade replaces this
// with the imported constant rather than a hunt.
const kueueWorkloadWaitingForReplacementPods = "WaitingForReplacementPods"

// modelDeploymentGroupWorkloadAsksForReplacement reports whether the group's Workload exists, and
// whether it asks for replacement Pods.
//
// THE READ GOES TO THE API SERVER RATHER THAN THE CACHE, and the short-role path is the one place
// in the pass that pays for it. The condition is the input that decides whether this pass creates,
// it is written by a controller nothing here watches, and the pass is woken by Pod events -- so a
// cached copy may simply not have seen Kueue's write yet. Reading through the API server makes the
// requeue's verdict authoritative; the alternative is a poll that lags its own two seconds by an
// unbounded cache delay in exactly the state that ends by being fixed.
//
// IT FINDS THE WORKLOAD THE WAY THE STATUS PATH DOES -- a plain owner reference to any member of
// the group, which is Kueue's own shape for a pod group -- and scans every match rather than the
// first, for the reason findModelDeploymentGroupWorkloads states: a Workload left behind still
// holds finalizers on replicas that cannot otherwise leave. A group with no member at all has no
// Workload to ask, and the caller creates freely.
// THE WORKLOADS ARE HANDED IN RATHER THAN READ HERE, because the answer to "which Workloads are in
// this namespace" is the same for every role of the deployment and the caller asks once for all of
// them. Reading inside would put an unfiltered list on the hot path once per short role per pass.
func modelDeploymentGroupWorkloadAsksForReplacement(
	workloads []kueue.Workload, pods []core.Pod,
) (asks, found bool) {
	if len(pods) == 0 {
		return false, false
	}

	ours := sets.New[types.UID]()
	for i := range pods {
		ours.Insert(pods[i].UID)
	}

	for i := range workloads {
		wl := &workloads[i]
		if !modelDeploymentWorkloadOwnsAny(wl, ours) {
			continue
		}
		found = true
		if kubeapistatus.ConditionType(kueueWorkloadWaitingForReplacementPods).IsTrue(wl) {
			asks = true
		}
	}

	return asks, found
}

// modelDeploymentSortReplicasNewestFirst orders replicas for deletion: newest by creationTimestamp
// first, and by name when the timestamps tie.
//
// THE TIEBREAK IS FOR DETERMINISM RATHER THAN MEANING. creationTimestamp has second granularity,
// so the replicas of one create pass all carry the same one, and a rule with no total order would
// pick a different victim on every pass over the same state. The name is arbitrary -- the API
// server assigns it -- but it is stable, which is the only property a level-based choice needs.
//
// NEWEST FIRST SO THE LONGEST-SERVING SURVIVE: a replica that has been serving holds the cached
// blocks its siblings reference, and a scale-down that shed the eldest would drop the most state
// the deployment still pays for.
func modelDeploymentSortReplicasNewestFirst(pods []*core.Pod) {
	slices.SortFunc(pods, func(a, b *core.Pod) int {
		if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
			return c
		}

		return strings.Compare(b.Name, a.Name)
	})
}

// getModelDeploymentInstanceType reads the InstanceType a role is admitted against.
//
// A missing type is an error rather than a render without one: the type supplies both how to spell
// the accelerator keys and the per-card resources the host request is derived from, so a Pod
// rendered without it would ask for something other than what the role declared.
func (r *ModelDeploymentReconciler) getModelDeploymentInstanceType(
	ctx context.Context, name string,
) (*worker.InstanceType, error) {
	instType := new(worker.InstanceType)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: name}, instType,
		ctrlclix.WithoutQuorum)
	if err == nil {
		return instType, nil
	}
	if !kerrors.IsNotFound(err) {
		return nil, fmt.Errorf("get instance type %s: %w", name, err)
	}

	// Maybe the InstanceType is not cached yet; read through to the API server rather than treat a
	// cold cache as a missing type.
	err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: name}, instType,
		ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, fmt.Errorf("get instance type %s: %w", name, err)
	}

	return instType, nil
}

// modelDeploymentRoleManufacturers resolves each role's accelerator manufacturer, keyed by role
// name, for the routed-path decisions that depend on it. An unrouted deployment gets nil, because
// nothing it renders reads a manufacturer.
//
// The read is BEST-EFFORT, and deliberately narrower than getModelDeploymentInstanceType's: a role
// whose type cannot be read simply has no entry, and every consumer treats a missing entry as "not
// known to be Ascend" -- the answer these paths gave before they looked. The pod render already
// fails the pass on an unreadable type, so a gap here means the type went away mid-pass, and one
// pass with the old answer beats failing a sync over it.
func (r *ModelDeploymentReconciler) modelDeploymentRoleManufacturers(
	ctx context.Context, md *workercore.ModelDeployment,
) map[string]string {
	if md.Spec.Router == nil {
		return nil
	}

	manufacturers := make(map[string]string, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		instType := new(worker.InstanceType)
		if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: md.Spec.Roles[i].InstanceType}, instType,
			ctrlclix.WithoutQuorum); err != nil {
			continue
		}
		manufacturers[md.Spec.Roles[i].Name] = instType.Status.Detail.Manufacturer
	}

	return manufacturers
}

// getModelDeploymentRuntimeClassName reports the runtime class an accelerated replica needs, and ""
// when it needs none or the cluster does not have it.
//
// A missing class is not an error: it is how a cluster that runs the vendor's runtime under a
// different name, or not at all, reports itself. It is looked up rather than assumed because a
// Pod naming a RuntimeClass that does not exist is rejected outright.
func (r *ModelDeploymentReconciler) getModelDeploymentRuntimeClassName(
	ctx context.Context, instType *worker.InstanceType,
) string {
	if !instType.Spec.Acceleratable {
		return ""
	}

	name := nodefeature.GetAcceleratableRuntimeName(instType.Status.Detail.Manufacturer)
	if name == "" {
		return ""
	}

	rc := new(node.RuntimeClass)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: name}, rc, ctrlclix.WithoutQuorum); err != nil {
		return ""
	}

	return name
}

// listModelDeploymentPods returns the replicas this deployment owns.
//
// It selects on the identity labels and then confirms the controller reference, so a Pod that
// carries the labels but belongs to a deployment of the same name that has since been recreated is
// not adopted by the new one.
func (r *ModelDeploymentReconciler) listModelDeploymentPods(
	ctx context.Context, md *workercore.ModelDeployment,
) ([]core.Pod, error) {
	podList := new(core.PodList)
	err := r.Client.List(ctx, podList,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
		},
		ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, err
	}

	owned := make([]core.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		if !modelDeploymentOwns(&podList.Items[i], md) {
			continue
		}
		owned = append(owned, podList.Items[i])
	}

	return owned, nil
}

// modelDeploymentOwns reports whether the object is controlled by this deployment.
func modelDeploymentOwns(obj ctrlcli.Object, md *workercore.ModelDeployment) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID == md.UID {
			return true
		}
	}

	return false
}

func modelDeploymentOwnedResource(obj ctrlcli.Object) bool {
	if !systemmeta.MatchResource(obj, ModelDeploymentResourceType) {
		return false
	}

	return systemmeta.DescribeResourceNote(obj, ModelDeploymentResourceNoteRole) != "" ||
		systemmeta.DescribeResourceNote(obj, modelDeploymentResourceNoteRouter) != ""
}

func (r *ModelDeploymentReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()
	r.Recorder = opts.Manager.GetEventRecorderFor("modeldeployment")

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modeldeployment").
		For(
			&workercore.ModelDeployment{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.GenerationChangedPredicate{},
			),
		).
		Owns(
			// Watch the replicas this deployment renders. Ownership rather than a label match is
			// what enqueues here, so a Pod that has lost its owner cannot keep waking a deployment
			// that no longer claims it.
			&core.Pod{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(modelDeploymentOwnedResource),
			),
		).
		Owns(
			// The Service is reconciled by this controller, so it has to be watched by it. Without
			// this, an externally deleted or edited Service is corrected only when something else
			// happens to wake the deployment, or at the next resync hours away: the endpoint stays
			// broken while reconcile code that would realign it never runs.
			//
			// A watch is what makes the convergence level-based rather than a one-shot at creation.
			// The gap does not show up in manual testing, because nobody deletes the Service by
			// hand -- it shows up when something else in the cluster does.
			&core.Service{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(modelDeploymentOwnedResource),
			),
		).
		Owns(
			&app.Deployment{},
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(modelDeploymentOwnedResource)),
		).
		Owns(
			&core.ConfigMap{},
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(modelDeploymentOwnedResource)),
		).
		Owns(
			&core.ServiceAccount{},
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(modelDeploymentOwnedResource)),
		).
		Owns(
			&rbac.Role{},
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(modelDeploymentOwnedResource)),
		).
		Owns(
			&rbac.RoleBinding{},
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(modelDeploymentOwnedResource)),
		).
		Watches(
			// A Binding's readiness is observed rather than declared, so the deployment has to be
			// woken when it moves. Without this the transition would only be noticed at the next
			// resync, which is hours away: a store leader restart would leave every deployment on it
			// reporting a domain it had already regained.
			&workercore.KVCachePoolBinding{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentBinding),
		).
		Watches(
			// The InstanceType's OBSERVED detail is now an input to the rendered Pod: a role that
			// names no image has one synthesized from the pool's accelerator runtime version, so a
			// driver rollout changes the image every replica should be running. Without this watch
			// that change would be picked up only when something else woke the deployment, and the
			// spec-hash comparison would go on matching a Pod built from a version the pool no
			// longer reports.
			//
			// No GenerationChangedPredicate here, and that is the point: what moves is the status,
			// which does not bump the generation. The predicate on the primary object would filter
			// out exactly the updates this watch exists for.
			&worker.InstanceType{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentInstanceType),
		).
		Watches(
			// The pool's published client endpoint is the address every replica dials, and it is
			// rendered into the Pod. It MOVES: a store leader restart or a recreated Service gives
			// the pool a new address, and the deployment's own Binding can stay Ready across that,
			// so the Binding watch above does not cover it. Without this watch the spec hash goes on
			// matching replicas built from an address nobody answers, which is the one failure this
			// design refuses to render on purpose -- from outside the Pod it is indistinguishable
			// from a cache miss.
			&workercore.KVCachePool{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentPool),
		).
		Watches(
			// The backend's transport protocol is the connector's other rendered input, and
			// `spec.transport` is absent from the backend webhook's immutability rule, so an admin
			// can edit it on a running pool. Same staleness, same silence.
			&workercore.KVCacheBackend{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapModelDeploymentBackend),
		).
		Complete(r)
}

// mapModelDeploymentPool enqueues every deployment attached to a pool, through the Bindings that
// grant access to it.
//
// It goes pool -> Bindings -> deployments rather than listing every deployment and resolving each
// one's Binding: the Binding is what names the pool, and a cluster has far fewer Bindings than the
// N+1 reads that walk would cost. Each deployment matches exactly one Binding, so no request is
// emitted twice.
func (r *ModelDeploymentReconciler) mapModelDeploymentPool(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	kvcpbList := new(workercore.KVCachePoolBindingList)
	if err := r.Client.List(ctx, kvcpbList, ctrlclix.WithoutQuorum); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list kv cache pool bindings for kv cache pool",
			"kv cache pool", ctrlcli.ObjectKeyFromObject(obj))

		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range kvcpbList.Items {
		kvcpb := &kvcpbList.Items[i]
		if kvcpb.Spec.PoolRef.Name != obj.GetName() {
			continue
		}
		reqs = append(reqs, r.mapModelDeploymentBinding(ctx, kvcpb)...)
	}

	return reqs
}

// mapModelDeploymentBackend enqueues every deployment on a pool that names this backend.
//
// One more hop than the pool's, for the same reason: the deployment references a Binding, the
// Binding a pool, and the pool a backend. A pool admits exactly one backend, but the field is a
// list, so membership is tested rather than equality.
func (r *ModelDeploymentReconciler) mapModelDeploymentBackend(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	kvcpList := new(workercore.KVCachePoolList)
	if err := r.Client.List(ctx, kvcpList, ctrlclix.WithoutQuorum); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list kv cache pools for kv cache backend",
			"kv cache backend", ctrlcli.ObjectKeyFromObject(obj))

		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range kvcpList.Items {
		kvcp := &kvcpList.Items[i]
		if !slices.Contains(kvcp.Spec.Backends, obj.GetName()) {
			continue
		}
		reqs = append(reqs, r.mapModelDeploymentPool(ctx, kvcp)...)
	}

	return reqs
}

// mapModelDeploymentInstanceType enqueues every deployment with a role admitted against the type.
//
// The scan crosses namespaces, unlike the Binding's, and it has to: an InstanceType is
// cluster-scoped, so the deployments referencing one are not confined to any namespace. The set
// walked is every ModelDeployment on the cluster, which is bounded by how many a cluster has rather
// than by how many nodes or Pods it runs.
func (r *ModelDeploymentReconciler) mapModelDeploymentInstanceType(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	mdList := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mdList, ctrlclix.WithoutQuorum); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model deployments for instance type",
			"instance type", ctrlcli.ObjectKeyFromObject(obj))

		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range mdList.Items {
		md := &mdList.Items[i]
		for j := range md.Spec.Roles {
			if md.Spec.Roles[j].InstanceType != obj.GetName() {
				continue
			}
			reqs = append(reqs, ctrlreconcile.Request{
				NamespacedName: ctrlcli.ObjectKeyFromObject(md),
			})

			break // one request per deployment, however many of its roles match
		}
	}

	return reqs
}

// mapModelDeploymentBinding enqueues the deployments in a Binding's namespace that reference it.
//
// The scan is a namespaced List rather than an index because the reference is a plain field on a
// namespaced object: the query never crosses a namespace, so the set it walks is one team's
// deployments.
func (r *ModelDeploymentReconciler) mapModelDeploymentBinding(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	mdList := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mdList,
		ctrlcli.InNamespace(obj.GetNamespace()), ctrlclix.WithoutQuorum); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model deployments for kv cache pool binding",
			"kv cache pool binding", ctrlcli.ObjectKeyFromObject(obj))

		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range mdList.Items {
		md := &mdList.Items[i]
		if md.Spec.KVCache == nil || md.Spec.KVCache.PoolRef.Name != obj.GetName() {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKeyFromObject(md),
		})
	}

	return reqs
}
