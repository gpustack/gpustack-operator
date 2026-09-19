package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"
	kueueadmissioncheck "sigs.k8s.io/kueue/pkg/util/admissioncheck"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const (
	// _JointAdmissionCheckName is the AdmissionCheck object every operator-owned ClusterQueue
	// references, and _JointAdmissionControllerName claims it for the reconciler in this file: Kueue
	// routes a Workload's check to whoever declares that name, so no other controller answers it.
	//
	// BOTH ARE TAKEN FROM THE PACKAGE THAT INSTALLS THE OBJECT rather than spelled again here. Two
	// copies of a contract agree until someone renames one of them, and neither side reports the
	// disagreement: the queues go on admitting, and the only thing that notices is the deployment
	// that needed the barrier.
	_JointAdmissionCheckName = kuberess.ModelDeploymentJointAdmissionCheckName

	_JointAdmissionControllerName = kuberess.ModelDeploymentJointAdmissionControllerName

	// _JointAdmissionFieldOwner owns this controller's entries in a Workload's admissionChecks.
	_JointAdmissionFieldOwner = "worker.gpustack.ai/model-deployment-joint"

	// _JointAdmissionParkedMarker is the phrase the park message carries and the one the status
	// reader looks for.
	//
	// spec.active=false DOES NOT SAY WHO WROTE IT. Kueue deactivates a Workload of its own accord and
	// an operator can pause one group by hand, so the flag alone cannot tell the status layer that the
	// bound is what stopped this deployment. This marker is a term of the message rather than a second
	// field because the message is already the durable record of the verdict; adding a field would put
	// the same fact in two places that can disagree.
	_JointAdmissionParkedMarker = "parked rather than held"

	// _JointAdmissionUndecidedMessage is what the check says while this controller cannot tell
	// whether the Workload is one group of a multi-group deployment.
	//
	// IT MUST NOT READ AS AN ORDINARY HOLD, and the message is the only thing that can carry the
	// difference: both states are Pending, so a reader with only the state learns that something is
	// waiting and not what for. A hold names the groups being waited on and clears itself as they
	// reserve quota. This one says a read failed, which is either a group mid-rebuild -- transient,
	// and nothing to do -- or replicas that are gone for good, which an operator has to act on.
	//
	// SAYING IT IS RETRYING IS PART OF THE DIFFERENCE. Without that an operator cannot tell this
	// from a verdict that has settled, and the whole point of the state is that it has not.
	_JointAdmissionUndecidedMessage = "cannot decide yet: some of this workload's owner replicas " +
		"could not be read, so whether it is one group of a multi-group model deployment is unknown. " +
		"Holding rather than admitting on a guess, and retrying"

	// _JointAdmissionInfeasibleAfter is how long a deployment's set may fail to assemble before the
	// barrier stops holding it and parks it instead.
	//
	// THE NUMBER IS A WAIT AN OPERATOR WOULD ACCEPT, not a measurement of anything. A set that cannot
	// assemble is usually waiting for capacity, and capacity arrives on a human timescale: a node
	// rejoining, a neighboring workload finishing, a quota being raised. Half an hour is long enough
	// that none of those is cut short and short enough that a deployment nobody is watching does not
	// sit on reserved quota overnight.
	//
	// WHAT MAKES IT SAFE TO BE WRONG IS WHICH WAY IT FAILS. Too long and quota is held that could have
	// been released; too short and a deployment that would have started is parked, and the operator
	// clears it. Neither loses work, and the parked state says what to do.
	_JointAdmissionInfeasibleAfter = 30 * time.Minute
)

// ModelDeploymentJointAdmissionCheckReconciler marks the joint-admission AdmissionCheck Active.
//
// Kueue turns a ClusterQueue referencing an inactive check inactive, so a check nobody activates
// stops the queue admitting anything at all. Activation is therefore the statement that this
// controller is running, and it is made here rather than at install time for exactly that reason.
type ModelDeploymentJointAdmissionCheckReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*ModelDeploymentJointAdmissionCheckReconciler)(nil)

func (r *ModelDeploymentJointAdmissionCheckReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	ac := new(kueue.AdmissionCheck)
	if err := r.Client.Get(ctx, req.NamespacedName, ac); err != nil {
		logger.Error(err, "fetch admission check")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	if ac.Spec.ControllerName != _JointAdmissionControllerName {
		logger.V(3).Info("skip admission check not owned by this controller")
		return ctrl.Result{}, nil
	}

	if kubemeta.IsConditionTrue(ac.Status.Conditions, kueue.AdmissionCheckActive) {
		return ctrl.Result{}, nil
	}

	kubemeta.SetCondition(&ac.Status.Conditions, meta.Condition{
		Type:    kueue.AdmissionCheckActive,
		Status:  meta.ConditionTrue,
		Reason:  "Ready",
		Message: "the model deployment joint admission check controller is running",
	})
	if err := r.Client.Status().Update(ctx, ac); err != nil {
		logger.Error(err, "mark admission check active")
		return ctrl.Result{}, err
	}

	logger.V(2).Info("activated model deployment joint admission check", "name", ac.Name)

	return ctrl.Result{}, nil
}

func (r *ModelDeploymentJointAdmissionCheckReconciler) SetupController(
	_ context.Context, opts controller.SetupOptions,
) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modeldeploymentjointadmissioncheck").
		For(
			&kueue.AdmissionCheck{},
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
				ac := obj.(*kueue.AdmissionCheck)

				return ac.Spec.ControllerName == _JointAdmissionControllerName
			})),
		).
		Complete(r)
}

// ModelDeploymentJointAdmissionReconciler gates every group of one ModelDeployment on the whole set
// having reserved quota.
//
// WITH ONE WORKLOAD PER GROUP, KUEUE'S OWN ATOMICITY NO LONGER COVERS THE DEPLOYMENT. A pod group is
// admitted as a unit, which is what makes a single-group deployment all-or-nothing; a deployment
// spread over two instanceTypes is two groups and two Workloads, and nothing relates them. Without
// this check a prefiller can be admitted and serving while its decoder waits for capacity that never
// arrives, which is a deployment that looks half-started and is in fact never going to finish.
//
// IT ANSWERS Ready OR Pending AND NEVER Retry. A Retry does not leave a Workload sitting on its
// reservation: Kueue EVICTS it, resetting the checks and dropping the reservation in two separate
// writes, and its scheduler then refuses to reserve again while a check reads Retry. Two siblings
// waiting on each other through Retry would trade the same quota indefinitely, each one's wait
// destroying the reservation the other was waiting to observe. Pending holds, which is the whole
// reason it is the answer here.
//
// IT NEVER REJECTS AND NEVER PREEMPTS. A deployment whose set is permanently infeasible is bounded
// elsewhere; within this controller a set that cannot assemble waits.
type ModelDeploymentJointAdmissionReconciler struct {
	Client ctrlcli.Client

	// Clock measures how long a set has failed to assemble. It is a field so the bound can be
	// exercised rather than waited on: a test that slept for the real bound would be one nobody runs,
	// and a bound nobody runs is a bound nobody has checked.
	Clock clock.PassiveClock
}

// now reads the clock, defaulting to the real one so a reconciler built without it still works.
func (r *ModelDeploymentJointAdmissionReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}

	return r.Clock.Now()
}

var _ ctrlreconcile.Reconciler = (*ModelDeploymentJointAdmissionReconciler)(nil)

func (r *ModelDeploymentJointAdmissionReconciler) Reconcile(
	ctx context.Context, req ctrl.Request,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	wl := new(kueue.Workload)
	if err := r.Client.Get(ctx, req.NamespacedName, wl); err != nil {
		logger.Error(err, "fetch workload")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// The same gates the node-devices check states: before reservation there is nothing to confirm,
	// and after eviction or finish the verdict is moot. An evicted Workload still reports a
	// reservation for the window between Kueue's two writes, and overwriting that reset wedges it.
	if !kueueworkload.HasQuotaReservation(wl) || kueueworkload.IsEvicted(wl) ||
		kueueworkload.IsFinished(wl) || !kueueworkload.IsActive(wl) {
		logger.V(3).Info("skip workload not holding quota reservation, evicted, finished or deactivated")

		return ctrl.Result{}, nil
	}
	if kueueworkload.IsAdmitted(wl) {
		logger.V(3).Info("skip already-admitted workload; the barrier has already opened")

		return ctrl.Result{}, nil
	}

	checks, err := kueueadmissioncheck.FilterForController(
		ctx, r.Client, wl.Status.AdmissionChecks, _JointAdmissionControllerName)
	if err != nil {
		logger.Error(err, "filter admission checks for controller")
		return ctrl.Result{}, err
	}
	if len(checks) == 0 {
		return ctrl.Result{}, nil
	}

	md, decided, err := r.workloadModelDeployment(ctx, wl)
	if err != nil {
		logger.Error(err, "resolve the workload's model deployment")
		return ctrl.Result{}, err
	}
	// NOT KNOWING IS ANSWERED BY WAITING, NOT BY OPENING. Some of this Workload's owner replicas
	// could not be read, so whether it belongs to a multi-group deployment is unknown -- and the
	// Ready below is the one verdict that cannot be taken back once Kueue has admitted on it.
	//
	// THE WAIT IS WRITTEN DOWN AS WELL AS TAKEN. Requeueing alone leaves an operator looking at a
	// Workload that is not being admitted and carries no verdict from this controller at all -- the
	// state the paragraph above and this file's other comments say the check exists to prevent. The
	// requeue stays: the verdict is Pending rather than final, because the state it describes is
	// real and usually brief, and answering it definitively is the defect this branch removed.
	if !decided {
		logger.V(3).Info("requeue workload whose owning replicas could not be resolved", "workload", wl.Name)

		if err := r.applyVerdict(
			ctx, wl, checks, kueue.CheckStatePending, _JointAdmissionUndecidedMessage,
		); err != nil {
			logger.Error(err, "write the undecided verdict")
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// EVERYTHING THAT IS NOT A MULTI-ROLE ModelDeployment IS READY AT ONCE, AND THAT CASE HAS TO BE
	// PRESENT OR THE CHECK PARKS THE CLUSTER. The check is referenced from every operator-owned
	// queue, so every Workload in them carries it -- including ones this operator did not create. A
	// controller that answered only about its own objects would leave all the others Pending forever
	// with nothing naming the cause.
	//
	// THE EXEMPTION COUNTS ROLES WHILE THE VERDICT COUNTS REPLICAS, and the asymmetry is the point:
	// modelDeploymentPodGroups returns one entry per role, so what is measured here is how many
	// roles the deployment spreads over. Counting replicas instead would pull a single-role
	// deployment of several replicas inside the barrier, where it would gain partial-quota holds
	// and exposure to the park bound for no atomicity at all -- there is no second role whose
	// admission has to coincide with its.
	if md == nil || len(modelDeploymentPodGroups(md)) < 2 {
		return ctrl.Result{}, r.applyVerdict(ctx, wl, checks, kueue.CheckStateReady,
			"nothing to wait for: this workload is not one replica of a multi-role model deployment")
	}

	held, err := r.jointVerdict(ctx, md)
	if err != nil {
		logger.Error(err, "judge the deployment's groups")
		return ctrl.Result{}, err
	}

	// PAST THE BOUND THE HOLD BECOMES A PARK. Waiting is the right answer while the set might still
	// assemble; after long enough it is a deployment sitting on reserved quota forever, and every
	// pass of the reserve-hold cycle costs a scheduling round for a set nothing is going to complete.
	// A deployment still short of its own replicas is excluded; see jointHeld.Settled for why.
	if held.State == kueue.CheckStatePending && held.Settled &&
		r.heldPast(wl, _JointAdmissionInfeasibleAfter) {
		logger.Info("parking a deployment whose groups have not assembled", "workload", wl.Name)

		return ctrl.Result{}, r.park(ctx, wl, checks, held.Message)
	}

	return ctrl.Result{}, r.applyVerdict(ctx, wl, checks, held.State, held.Message)
}

// heldPast reports whether this controller's check has been Pending for longer than the bound.
//
// THE CLOCK IS THE CHECK'S OWN LastTransitionTime, which Kueue stamps when the state changes and
// leaves alone when it does not. That makes the elapsed time durable on the Workload rather than
// something this controller has to remember: a restart loses no count, and a set that briefly became
// feasible and then stopped being feasible starts again from the transition, which is what "has not
// assembled for half an hour" has to mean.
func (r *ModelDeploymentJointAdmissionReconciler) heldPast(wl *kueue.Workload, bound time.Duration) bool {
	acs := jointCheckEntry(wl)
	if acs == nil || acs.State != kueue.CheckStatePending {
		return false
	}
	if acs.LastTransitionTime.IsZero() {
		// Never stamped, so nothing has elapsed yet. The pass that writes Pending stamps it.
		return false
	}

	return r.now().Sub(acs.LastTransitionTime.Time) > bound
}

// park stops the reserve-and-hold cycle and says what clears it.
//
// IT DEACTIVATES THE WORKLOAD RATHER THAN DELETING IT, and the difference is whether the cycle stops.
// A deleted Workload is composed again by Kueue from the very Pods that are still there, so deleting
// removes one turn of the loop and nothing else. spec.active is durable state the scheduler reads:
// while it is false the Workload is not considered, and the group is not rebuilt behind it.
//
// THE MESSAGE NAMES THE ACTION THAT CLEARS IT, because the obvious one does not work. Re-applying the
// same manifest bumps no resourceVersion and delivers no event, so an operator told only to "try
// again" watches nothing happen and concludes the operator is wedged. What clears it is deleting the
// deployment and creating it again, or making the capacity available and reactivating the workload.
func (r *ModelDeploymentJointAdmissionReconciler) park(
	ctx context.Context,
	wl *kueue.Workload,
	checks []kueue.AdmissionCheckReference,
	waiting string,
) error {
	// The marker is spliced in rather than spelled out, so the phrase the status layer looks for and
	// the phrase written here cannot drift apart.
	parked := fmt.Sprintf(
		"%s. This has not changed for %s, so the deployment is %s: its workloads "+
			"are deactivated and no longer ask for quota. An identical re-apply does not clear this — "+
			"free the capacity the waiting groups need and reactivate the workloads, or delete the "+
			"deployment and create it again",
		waiting, _JointAdmissionInfeasibleAfter, _JointAdmissionParkedMarker)

	if err := r.applyVerdict(ctx, wl, checks, kueue.CheckStatePending, parked); err != nil {
		return err
	}

	if !kueueworkload.IsActive(wl) {
		return nil
	}

	patched := wl.DeepCopy()
	patched.Spec.Active = ptr.To(false)
	if err := r.Client.Patch(ctx, patched, ctrlcli.MergeFrom(wl)); err != nil {
		return fmt.Errorf("deactivate workload %s: %w", wl.Name, err)
	}

	return nil
}

// jointHeld is what the barrier decided about one deployment, and why.
type jointHeld struct {
	// State and Message are written to every check this controller owns on the Workload.
	State   kueue.CheckState
	Message string

	// Settled reports that the deployment has stopped changing shape: every role has every replica
	// it declares, and what it is waiting for is capacity rather than its own replicas.
	//
	// ONLY A SETTLED DEPLOYMENT MAY BE PARKED. The infeasibility bound exists for a set that cannot
	// assemble, and a deployment still assembling or replacing a replica is short of its totals for
	// an ordinary reason that resolves on its own. Without this distinction a replacement slower
	// than the bound -- a large image on a cold node is enough -- parks a deployment that was about
	// to start, and nothing in the cluster ever sets spec.active back to true, so the park is
	// permanent.
	//
	// IT ERRS TOWARD HOLDING RATHER THAN PARKING, which is the direction that loses nothing: holding
	// costs reserved quota until an operator looks, while parking a healthy rollout costs the rollout.
	// The residue is that a deployment which finishes assembling into a genuinely infeasible cluster
	// can be parked on the pass right after, because the bound is measured from the check's own
	// transition and that transition did not move. Parking is recoverable and the message says how.
	Settled bool
}

// jointVerdict answers Ready when every replica of every role of the deployment holds a quota
// reservation.
//
// THE RESERVATION IS THE OBSERVABLE, not the admission: a sibling still held by this very check has
// reserved and not been admitted, so waiting for admission would be a deadlock in which every
// replica waits for a state only the others opening can produce.
//
// A REPLICA WITH NO WORKLOAD YET IS NOT READY AND IS NOT AN ERROR. Kueue composes a replica's
// Workload only once it has seen that replica's Pod, so an ordinal still being created simply has
// not got there, and the message names the role that is waiting.
func (r *ModelDeploymentJointAdmissionReconciler) jointVerdict(
	ctx context.Context, md *workercore.ModelDeployment,
) (jointHeld, error) {
	byGroup, liveByGroup, err := modelDeploymentReplicaGroups(ctx, r.Client, md)
	if err != nil {
		return jointHeld{}, err
	}

	wlList := new(kueue.WorkloadList)
	if err = r.Client.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace), ctrlclix.WithoutQuorum); err != nil {
		return jointHeld{}, fmt.Errorf("list workloads: %w", err)
	}

	// THE VERDICT COUNTS REPLICAS, ONE JUDGEMENT PER (role, ordinal) THE SPEC DECLARES, because the
	// replica is what reserves quota now: a role's replicas reserve separately and either of them
	// can be the one the set is waiting on. A slot is found by deriving its group name through the
	// same pure derivation the renderer stamped the Pod with, so a Pod meets its slot by
	// construction and never by parsing anything back out of a name.
	//
	// A ROLE SHORT OF A LIVE REPLICA IS STILL ASSEMBLING, and that is measured rather than assumed
	// because it is what separates a rollout from a dead end for the park bound. A role whose
	// replica is being replaced is short by construction -- the pass deletes the Pod and a later
	// pass creates its replacement -- and while it is short Kueue may have composed no Workload for
	// that ordinal, so the siblings read it as waiting. The two look identical from the waiting
	// side, and only the live count tells them apart.
	//
	// THE LISTS NAME ROLES, NOT INSTANCE TYPES. Two roles can name one instance type: a list of
	// types then names a queue two roles share and identifies neither, while a role names exactly
	// the replicas that are waiting. The queue an operator has to free follows from the named
	// role's own instance type.
	var waiting, assembling []string
	reserved, replicas := 0, 0
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]

		var roleWaiting, roleAssembling bool
		for ordinal := range int(role.Replicas) {
			replicas++
			group := modelDeploymentReplicaGroupName(md, role.Name, ordinal)
			members := byGroup[group]
			// ASSEMBLING IS MEASURED OVER THE REPLICAS THAT ARE STAYING. A Pod already asked to go
			// is not a member the role will have, so counting it lets a role in the middle of a
			// replacement read as complete -- and a complete-looking role is exactly what lets
			// Settled turn true and park the healthy replacement this guard exists to protect.
			// Quota holding below still reads every replica, terminating ones included, because a
			// Workload holding a leaving Pod is holding it.
			if liveByGroup[group].Len() == 0 {
				roleAssembling = true
			}
			if anyWorkloadHoldsQuotaFor(wlList.Items, members) ||
				jointReplicaDepartingRollout(md, wlList.Items, members, liveByGroup[group]) {
				reserved++
				continue
			}
			roleWaiting = true
		}

		if roleWaiting {
			waiting = append(waiting, role.Name)
		}
		if roleAssembling {
			assembling = append(assembling, role.Name)
		}
	}
	// READY IS ANSWERED BEFORE ASSEMBLING IS CONSULTED, and the order is the whole relationship
	// between this verdict and a rollout: an ordinal whose predecessor is draining reads as present
	// below, so a deployment rolling -- every replica either holding quota or mid-replacement --
	// answers Ready rather than being dragged back to Pending by its own rollout. Assembling is
	// what remains when something is missing without that evidence.
	if len(waiting) == 0 {
		return jointHeld{
			State: kueue.CheckStateReady,
			Message: "every replica of this deployment has reserved quota or is being replaced by " +
				"this operator's own rollout",
		}, nil
	}
	if len(assembling) > 0 {
		return jointHeld{
			State: kueue.CheckStatePending,
			Message: fmt.Sprintf(
				"holding this replica while the deployment is still assembling: the roles "+
					"%s do not yet have every replica they declare, so Kueue has composed no "+
					"workload for them yet. Nothing is wrong with the cluster; this resolves itself as "+
					"the replicas appear",
				strings.Join(assembling, ", ")),
		}, nil
	}

	return jointHeld{
		State: kueue.CheckStatePending,
		Message: fmt.Sprintf(
			"holding this replica until the whole deployment can run: %d of %d replicas have reserved quota, "+
				"and the ones still waiting belong to the roles %s. Every replica keeps the quota it has "+
				"reserved while it waits",
			reserved, replicas, strings.Join(waiting, ", ")),
		Settled: true,
	}, nil
}

// jointReplicaDepartingRollout reports whether a replica that holds no quota reservation is absent
// because this operator is replacing it, which the barrier reads as presence rather than as a
// missing member.
//
// THE EVIDENCE IS THE SHAPE A REPLACEMENT LEAVES BEHIND, not a hash comparison: whether the
// departing Pod was built from the current spec is something only the convergence loop can judge,
// because the desired hash comes out of the render it performs. What the barrier can read is that a
// member of the ordinal's group is on its way out while no Workload at all owns the group's
// members -- the operator deletes exactly that pair when it frees a replica's slot. THE TEST IS THE
// ABSENCE OF EVERY WORKLOAD AND NOT ONLY OF A RESERVATION, because a Workload that survives
// without its reservation names a preemption -- Kueue's preemption stops a group by deleting its
// Pods while the Workload stands -- and reading a preempted replica as present would open the
// barrier exactly when the set cannot run. A deployment being deleted is kept out by its own
// deletion timestamp, so a teardown -- which leaves the same shape a replacement does -- cannot
// read as a rollout that will finish.
//
// TWO STATES THIS CANNOT TELL APART, STATED SO THE NEXT READER KNOWS THEY ARE CONFLATED: a replica
// whose Pod and Workload were both deleted by hand also reads as a rollout, which is tolerable
// because the convergence loop recreates a missing ordinal freely and the recomposed Workload
// reserves on its own; and a rollout interrupted by deleting the deployment reads as absence
// rather than as a rollout, which holds instead of opening -- the safer answer for a deployment
// nothing is going to reassemble.
func jointReplicaDepartingRollout(
	md *workercore.ModelDeployment,
	wls []kueue.Workload,
	members, live sets.Set[types.UID],
) bool {
	if md.DeletionTimestamp != nil || members.Len() == 0 || members.Len() == live.Len() {
		return false
	}

	return !anyWorkloadOwnsAny(wls, members)
}

// anyWorkloadOwnsAny reports whether one of these Workloads owns any of the given replicas, quota
// or no quota. It is the presence half of the rollout test above, where anyWorkloadHoldsQuotaFor
// is its reservation half: a preempted replica's Workload owns it and holds nothing, and that
// difference is what keeps a preemption from reading as a rollout.
func anyWorkloadOwnsAny(wls []kueue.Workload, members sets.Set[types.UID]) bool {
	for i := range wls {
		if modelDeploymentWorkloadOwnsAny(&wls[i], members) {
			return true
		}
	}

	return false
}

// modelDeploymentReplicaGroups indexes a deployment's own replicas by the pod group each one joined.
//
// THE LIST IS SCOPED BY THE DEPLOYMENT'S OWN LABEL, not by the namespace. Its callers run on a
// controller that watches every Workload in the cluster, so an unscoped list here is an unbounded
// read in a hot path -- it would cost the whole namespace on every gated Workload's event, and the
// namespaces this runs in are the ones with the most Pods in them.
//
// THE LABEL NARROWS THE READ; OWNERSHIP STILL DECIDES MEMBERSHIP. A label is a value anyone can copy
// onto a Pod, and an owner reference is not.
//
// IT RETURNS TWO INDEXES BECAUSE ITS TWO CALLERS ASK DIFFERENT QUESTIONS, and one answer would be
// wrong for one of them. "Is this group assembled" counts only replicas that are staying, since a
// Pod already asked to go is not a member the group will have. "Which Workloads own these replicas"
// counts every one, since a terminating Pod is still owned and the Workload holding it is still the
// one a sibling's event has to reach. Collapsing the two either reads a rebuilding group as complete
// or drops a Workload from the mapping while its last Pod is leaving.
func modelDeploymentReplicaGroups(
	ctx context.Context, cli ctrlcli.Client, md *workercore.ModelDeployment,
) (all, live map[string]sets.Set[types.UID], err error) {
	podList := new(core.PodList)
	if err = cli.List(ctx, podList,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{modelDeploymentLabelKeyInstance: md.Name},
		ctrlclix.WithoutQuorum); err != nil {
		return nil, nil, fmt.Errorf("list replicas: %w", err)
	}

	all, live = make(map[string]sets.Set[types.UID]), make(map[string]sets.Set[types.UID])
	for i := range podList.Items {
		pod := &podList.Items[i]
		if !modelDeploymentOwns(pod, md) {
			continue
		}
		group := pod.Labels[kueuepodconst.GroupNameLabel]
		if all[group] == nil {
			all[group] = sets.New[types.UID]()
		}
		all[group].Insert(pod.UID)
		if pod.DeletionTimestamp != nil {
			continue
		}
		if live[group] == nil {
			live[group] = sets.New[types.UID]()
		}
		live[group].Insert(pod.UID)
	}

	return all, live, nil
}

// anyWorkloadHoldsQuotaFor reports whether one of these Workloads owns any of the group's replicas
// and holds a reservation.
func anyWorkloadHoldsQuotaFor(wls []kueue.Workload, members sets.Set[types.UID]) bool {
	for i := range wls {
		wl := &wls[i]
		if modelDeploymentWorkloadOwnsAny(wl, members) && kueueworkload.HasQuotaReservation(wl) {
			return true
		}
	}

	return false
}

// workloadModelDeployment resolves the deployment a Workload's replicas belong to, or nil when the
// Workload is not one of this operator's.
//
// KUEUE OWNS A GROUP'S WORKLOAD FROM ITS MEMBERS, with a plain owner reference per Pod and no
// controller reference, so the path runs Workload to Pod to ModelDeployment. Any owner is enough:
// every member of a group belongs to the same deployment by construction.
func (r *ModelDeploymentJointAdmissionReconciler) workloadModelDeployment(
	ctx context.Context, wl *kueue.Workload,
) (md *workercore.ModelDeployment, decided bool, err error) {
	// UNRESOLVABLE IS NOT THE SAME ANSWER AS NOT OURS, and collapsing them opens the barrier. A group
	// being rebuilt has its replicas deleted while its Workload still references them, so every Get
	// below misses and the walk ends with nothing -- which, read as "not one of ours", makes this
	// controller answer Ready and admit a set that is mid-teardown. The same shape arrives from an
	// informer cache that has not caught up.
	//
	// A WORKLOAD WITH NO Pod OWNER AT ALL IS GENUINELY NOT OURS, and that is the case this has to keep
	// answering, because the check is referenced from every operator-owned queue: without it every
	// foreign Workload in those queues waits forever with nothing naming the cause.
	missing := false

	for _, ref := range wl.OwnerReferences {
		if ref.Kind != "Pod" || ref.APIVersion != "v1" {
			continue
		}

		pod := new(core.Pod)
		err := r.Client.Get(ctx,
			ctrlcli.ObjectKey{Namespace: wl.Namespace, Name: ref.Name}, pod, ctrlclix.WithoutQuorum)
		if err != nil {
			if ctrlcli.IgnoreNotFound(err) != nil {
				return nil, false, err
			}
			missing = true

			continue
		}

		owner := meta.GetControllerOf(pod)
		if owner == nil || owner.Kind != "ModelDeployment" {
			continue
		}

		md := new(workercore.ModelDeployment)
		err = r.Client.Get(ctx,
			ctrlcli.ObjectKey{Namespace: wl.Namespace, Name: owner.Name}, md, ctrlclix.WithoutQuorum)
		if err != nil {
			if ctrlcli.IgnoreNotFound(err) != nil {
				return nil, false, err
			}
			missing = true

			continue
		}

		return md, true, nil
	}

	return nil, !missing, nil
}

// applyVerdict writes the state for every check this controller owns.
//
// The whole owned set is applied rather than only what changed, for the reason the node-devices
// check states: admissionChecks is a list keyed by name under a server-side apply, so an entry this
// field owner stops claiming is pruned.
func (r *ModelDeploymentJointAdmissionReconciler) applyVerdict(
	ctx context.Context,
	wl *kueue.Workload,
	checks []kueue.AdmissionCheckReference,
	state kueue.CheckState,
	message string,
) error {
	desired, changed := desiredCheckStates(wl, checks, state, message)
	if !changed {
		return nil
	}

	return kueueworkload.PatchStatus(ctx, r.Client, wl, ctrlcli.FieldOwner(_JointAdmissionFieldOwner),
		func(w *kueue.Workload) (bool, error) {
			for i := range desired {
				kueueworkload.SetAdmissionCheckState(&w.Status.AdmissionChecks, desired[i], clock.RealClock{})
			}

			return true, nil
		})
}

// jointCheckEntry returns this controller's admission check entry on a Workload, or nil when the
// Workload does not carry it. Read off the object with no I/O.
//
// EVERY READER OF THAT ENTRY GOES THROUGH HERE, so the name is compared in one place and callers
// get the entry rather than a yes-or-no. The questions asked of it differ -- presence, how long it
// has been Pending, what its message says, and in tests which state it reached -- and a helper
// returning only a bool would leave the rest rescanning the slice for the entry they need.
func jointCheckEntry(wl *kueue.Workload) *kueue.AdmissionCheckState {
	return kueueadmissioncheck.FindAdmissionCheck(
		wl.Status.AdmissionChecks, kueue.AdmissionCheckReference(_JointAdmissionCheckName))
}

// carriesJointCheck reports whether a Workload carries this controller's admission check, read off
// the object with no I/O.
//
// IT IS A NECESSARY CONDITION AND NOT A SUFFICIENT ONE, which is why nothing downstream of it is
// removed: the check is referenced from every operator-owned queue, so plenty of Workloads carry it
// that this operator did not create. What it rules out cheaply is everything in every OTHER queue.
func carriesJointCheck(wl *kueue.Workload) bool {
	return jointCheckEntry(wl) != nil
}

// jointSiblings maps a Workload that changed to the sibling replicas' workloads of the same
// deployment.
//
// WITHOUT IT THE BARRIER ONLY EVER CLOSES. A held replica's verdict is a statement about a DIFFERENT
// object: it stays Pending until the last sibling reserves quota, and that reservation is written to
// the sibling's Workload. Watching only this controller's own object delivers that event to the
// sibling alone, so the held replica is never judged again and a barrier that correctly refused a
// half-feasible set never opens when the set becomes feasible.
//
// IT HIDES IN A CLUSTER WHERE THE REPLICAS RESERVE AT NEARLY THE SAME MOMENT, which is the ordinary
// case and not a guarantee: each replica's own event then arrives after the others have already
// reserved, and every replica opens on its own watch.
func (r *ModelDeploymentJointAdmissionReconciler) jointSiblings(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx)

	wl, ok := obj.(*kueue.Workload)
	if !ok {
		return nil
	}

	// THE CHEAPEST FILTER RUNS FIRST AND COSTS NO READ AT ALL. This mapping is called for every
	// Workload event in the cluster, and everything below it reads objects: a Get per owner replica,
	// then a list of the namespace's Workloads. A Workload that does not carry this controller's check
	// cannot be one this barrier holds, and that is answerable from the object already in hand.
	if !carriesJointCheck(wl) {
		return nil
	}

	// An undecided resolution maps to nothing rather than requeueing: this is an event mapping, and
	// the Workload whose replicas could not be read gets its own pass through Reconcile, which is
	// where waiting belongs.
	md, _, err := r.workloadModelDeployment(ctx, wl)
	if err != nil {
		logger.Error(err, "resolve the workload's model deployment for sibling mapping")
		return nil
	}
	if md == nil || len(modelDeploymentPodGroups(md)) < 2 {
		return nil
	}

	// Every replica, terminating ones included: this maps an event to the siblings that have to see
	// it, and a Workload still holding a leaving Pod is still one of them.
	byGroup, _, err := modelDeploymentReplicaGroups(ctx, r.Client, md)
	if err != nil {
		logger.Error(err, "index the deployment's replicas for sibling mapping")
		return nil
	}
	members := sets.New[types.UID]()
	for _, group := range byGroup {
		members = members.Union(group)
	}

	wlList := new(kueue.WorkloadList)
	if err = r.Client.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace), ctrlclix.WithoutQuorum); err != nil {
		logger.Error(err, "list workloads for sibling mapping")
		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range wlList.Items {
		sibling := &wlList.Items[i]
		// The changed Workload is left out because the watch below already enqueues it.
		if sibling.Name == wl.Name || !modelDeploymentWorkloadOwnsAny(sibling, members) {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sibling.Namespace, Name: sibling.Name},
		})
	}

	return reqs
}

func (r *ModelDeploymentJointAdmissionReconciler) SetupController(
	_ context.Context, opts controller.SetupOptions,
) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modeldeploymentjointadmission").
		For(&kueue.Workload{}).
		Watches(&kueue.Workload{}, ctrlhandler.EnqueueRequestsFromMapFunc(r.jointSiblings)).
		Complete(r)
}
