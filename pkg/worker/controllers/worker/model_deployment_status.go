package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"

	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const (
	// ModelDeploymentPhaseStarting is reported while replicas are still coming up and none is ready.
	ModelDeploymentPhaseStarting = "Starting"
	// ModelDeploymentPhaseReady is reported when every role's ready count equals its desired count.
	ModelDeploymentPhaseReady = "Ready"
	// ModelDeploymentPhaseDegraded is reported when at least one replica is ready and at least one
	// is not. It is a distinct phase rather than a shade of Starting because it is the state a
	// deployment sits in when it is serving and under-provisioned at the same time.
	ModelDeploymentPhaseDegraded = "Degraded"
	// ModelDeploymentPhaseDeleting is reported while the replicas are being torn down.
	ModelDeploymentPhaseDeleting = "Deleting"
)

// The condition types a ModelDeployment reports, one per axis. They are independent: "quota
// reserved but cache not attached" is a real and actionable state, which is what a single phase
// string cannot carry.
//
// DomainRegistered is declared beside the rule that resolves the Binding, CacheAttached beside the
// reading that judges it, and ReplicasUpToDate beside the convergence that decides it, so that each
// condition's vocabulary sits with the code that can actually observe it.
const (
	ModelDeploymentConditionQuotaReserved kubeapistatus.ConditionType = "QuotaReserved"

	// ModelDeploymentConditionRoleKindsReady reports whether every role kind this deployment
	// declares has at least one ready replica.
	//
	// IT IS NOT REPLICA COMPLETENESS, which is the question it sits closest to and the one it will
	// be read as. "Every role has all the replicas it asked for" is what the phase already answers,
	// by summing every role's counts before judging — and a sum cannot express this one: a
	// deployment missing an entire kind and a deployment merely one replica short produce the same
	// sum, so the same phase.
	//
	// IT IS ALSO NOT AVAILABILITY, and the distance is larger than it looks. Whether a request can
	// be served depends on what the engine does with a role it was told is a producer, and on
	// whether a Service's endpoints reach the replicas that are ready. Neither is observable from
	// this object. So this reports readiness BY KIND and claims nothing past it, which is also what
	// keeps it correct whichever way those two questions are later answered.
	ModelDeploymentConditionRoleKindsReady kubeapistatus.ConditionType = "RoleKindsReady"

	// ModelDeploymentConditionRouterReady reports whether the managed router rendered and has a
	// ready replica. A render refusal is projected HERE rather than returned: it is a spec problem,
	// and failing the whole status pass over it would freeze every other axis while naming none.
	// The convergence path still returns the same error, so the pass is still retried.
	ModelDeploymentConditionRouterReady kubeapistatus.ConditionType = "RouterReady"

	// ModelDeploymentConditionWeightsReady reports whether every engine role's weights are available:
	// a claim mounted, or an engine's own download finished. It is True with NotApplicable for a
	// deployment naming no artifact.
	ModelDeploymentConditionWeightsReady kubeapistatus.ConditionType = "WeightsReady"
)

// The reasons RoleKindsReady carries. Three rather than two, because "no kind is missing" and "no
// role has been accounted for yet" are different answers that a False would merge into one.
const (
	modelDeploymentReasonAllKindsReady = "AllKindsReady"

	modelDeploymentReasonKindsNotReady = "KindsNotReady"

	// modelDeploymentReasonNoRoleStatuses is the Unknown case: the pass accounted for no role at
	// all, so there is nothing to judge. Reporting False here would say a kind is missing, which is
	// a claim this pass cannot make.
	modelDeploymentReasonNoRoleStatuses = "NoRoleStatuses"
)

// modelDeploymentReasonPodGroupIncomplete is QuotaReserved's reason for a group that is short of the
// total it declares.
//
// It exists because that state has NO Workload at all — Kueue declines to compose one for a group it
// has not fully seen — so it is indistinguishable, from the Workload's side, from admission that has
// simply not happened yet. The two need telling apart: one clears in a moment, the other is a
// deployment sitting with gated Pods and an empty `kubectl get workloads` until something creates
// the missing replica.
const modelDeploymentReasonPodGroupIncomplete = "PodGroupIncomplete"

// modelDeploymentReasonPreemptedInPart is QuotaReserved's reason for a deployment a higher-priority
// workload has taken part of.
//
// IT IS A SEPARATE REASON BECAUSE THE OPERATOR ACTION IS SEPARATE. Every other wait this condition
// reports resolves itself or names something in the deployment to fix; this one resolves only when
// capacity elsewhere frees up, and until then the surviving groups hold accelerators for a
// deployment that is short of what it was admitted for, and, when the reclaimed groups were every
// group of some role kind, for one that has lost that role entirely. Waiting is right for the
// others and is
// a decision here, because how long to wait
// depends on what preempted it — which is outside this object, and outside this operator.
//
// A REASON IS THE MACHINE-READABLE CLASSIFICATION AND A MESSAGE IS NOT A CONTRACT. An alert rule or a
// runbook that wants to tell "wait for it" from "go and look at what took the quota" has to branch on
// something stable, and substring-matching a sentence is not that.
const modelDeploymentReasonPreemptedInPart = "PreemptedInPart"

// The reasons RouterReady carries. NotApplicable is True for the same reason KVEventsPublishing
// reports one: an absent condition and an inapplicable one are different states, and a reader
// should not have to infer the second from the first.
const (
	modelDeploymentReasonRouterReady          = "Ready"
	modelDeploymentReasonRouterNotApplicable  = "NotApplicable"
	modelDeploymentReasonRouterRenderFailed   = "RenderFailed"
	modelDeploymentReasonRouterNotDeployed    = "NotDeployed"
	modelDeploymentReasonRouterNoReadyReplica = "NoReadyReplicas"
)

// syncModelDeploymentStatus rebuilds the status from what was observed this pass and writes it only
// if it differs from what is stored.
//
// It is REBUILT rather than patched, so a stale field cannot survive a disagreement with the Pods:
// a role that was Ready and is not any more reports the count that was just measured, not the one
// that was true when it last changed. Anything a later task adds must fold into this one function
// for the same reason — a second writer would be free to leave its own field behind.
// The domain is what THIS pass observed about the referenced Binding, or nil for a pass that did not
// look. Nil leaves the domain projection and its condition exactly as they were, because a teardown
// pass reporting "not observed" would read as the domain having gone away.
//
// THE ROLLOUT RECORD IS NIL ON THE SAME OCCASIONS AND ANSWERS THE OPPOSITE WAY, which is the one
// asymmetry here worth knowing before adding a caller. Nil, or a record that accounted for no
// replica, reports Unknown rather than leaving the stored condition alone. The difference is whose
// object it is: the Binding belongs to somebody else, so a pass that did not read it has no question
// to answer, while the replicas are this controller's own, so a pass that could account for none has
// an answer and that answer is "could not tell". Left alone instead, a steady deployment's True
// survives the very pass that deletes every replica.
func (r *ModelDeploymentReconciler) syncModelDeploymentStatus(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod, domain *modelDeploymentDomain,
	rollout *modelDeploymentRollout, weights *modelArtifactWeights,
) error {
	desired, err := r.computeModelDeploymentStatus(ctx, md, pods, domain, rollout, weights)
	if err != nil {
		return err
	}

	// kubemeta.DeepEqual covers LastTransitionTime too, and that is correct rather than incidental:
	// the condition accessors move it only when the condition's value changes, so an unchanged pass
	// leaves it alone and compares equal.
	if kubemeta.DeepEqual(*desired, md.Status) {
		return nil
	}

	md.Status = *desired

	return r.Client.Status().Update(ctx, md)
}

// computeModelDeploymentStatus derives the whole status from the spec and the observed Pods.
func (r *ModelDeploymentReconciler) computeModelDeploymentStatus(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod, domain *modelDeploymentDomain,
	rollout *modelDeploymentRollout, weights *modelArtifactWeights,
) (*workercore.ModelDeploymentStatus, error) {
	// The condition accessors mutate the object they are given, so they work on a copy of the
	// observed status: conditions carry a LastTransitionTime that must not be reset on every pass,
	// and only the accessors know when it should move.
	holder := &workercore.ModelDeployment{Status: *md.Status.DeepCopy()}

	// EVERY Workload of the deployment, resolved once: both the per-role flavor answers and the
	// quota verdicts come from this same read. Reading it twice would let two halves of the status
	// describe two moments.
	wls, err := r.findModelDeploymentGroupWorkloads(ctx, md, pods)
	if err != nil {
		return nil, err
	}
	// EACH REPLICA'S OWN WORKLOAD, RESOLVED ONCE. There is no such thing as "the deployment's
	// Workload" once each replica composes one of its own, and a per-role view that answered for a
	// role from whichever Workload sorts first would report the survivors' answer for a replica
	// whose own is missing.
	wlByReplica := modelDeploymentReplicaWorkloads(pods, wls)

	holder.Status.Roles = modelDeploymentRoleStatuses(md, pods, wlByReplica)
	if md.Spec.Router == nil {
		holder.Status.Endpoint = modelDeploymentEndpoint(md)
	} else {
		holder.Status.Endpoint = ""
	}

	observeModelDeploymentDomain(holder, domain)

	observeModelDeploymentQuota(md, pods, wls, wlByReplica, holder)

	r.observeModelDeploymentCache(ctx, md, pods, domain, holder)

	observeModelDeploymentRollout(holder, md, pods, wlByReplica, rollout)

	observeModelDeploymentRoleKinds(holder)

	var nodeModels map[string]*workercore.NodeModelStoreModel
	if weights != nil && weights.Render != nil && weights.Render.Delivery == workercore.ModelDeploymentModelDeliveryNode {
		if nodeModels, err = modelArtifactNodeModels(ctx, r.Client, pods, weights.Render.ManifestDigest); err != nil {
			return nil, err
		}
	}
	observeModelDeploymentWeights(holder, pods, weights, nodeModels)

	// Resolved once for both routed-path observers, and nil for an unrouted deployment, which has
	// nothing that reads a manufacturer.
	manufacturers := r.modelDeploymentRoleManufacturers(ctx, md)

	observeModelDeploymentKVEvents(md, holder, manufacturers)

	routerReady, err := r.observeModelDeploymentRouter(ctx, md, domain, holder, manufacturers)
	if err != nil {
		return nil, err
	}
	holder.Status.RoleSummary = modelDeploymentRoleSummary(md.Spec.Router, routerReady, holder.Status.Roles)

	deriveModelDeploymentPhase(md, holder, routerReady > 0)
	annotateModelDeploymentPhase(holder, pods)

	return &holder.Status, nil
}

func modelDeploymentRoleSummary(
	router *workercore.ModelDeploymentRouter, routerReady int32,
	roles []workercore.ModelDeploymentRoleStatus,
) string {
	var summary strings.Builder
	if router != nil {
		fmt.Fprintf(&summary, "%dR", routerReady)
	}

	counts := map[workercore.ModelDeploymentRoleKind]int32{}
	var other strings.Builder
	for _, role := range roles {
		switch role.Kind {
		case workercore.ModelDeploymentRoleKindServer,
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode:
			counts[role.Kind] += role.Ready
		default:
			fmt.Fprintf(&other, "%d%s", role.Ready, strings.ToUpper(string(role.Kind)))
		}
	}
	for _, kind := range []struct {
		name workercore.ModelDeploymentRoleKind
		code string
	}{
		{workercore.ModelDeploymentRoleKindServer, "S"},
		{workercore.ModelDeploymentRoleKindPrefill, "P"},
		{workercore.ModelDeploymentRoleKindDecode, "D"},
	} {
		if count, declared := counts[kind.name]; declared {
			fmt.Fprintf(&summary, "%d%s", count, kind.code)
		}
	}
	summary.WriteString(other.String())

	return summary.String()
}

func (r *ModelDeploymentReconciler) observeModelDeploymentRouter(
	ctx context.Context, md *workercore.ModelDeployment, domain *modelDeploymentDomain,
	holder *workercore.ModelDeployment, manufacturers map[string]string,
) (int32, error) {
	if md.Spec.Router == nil {
		holder.Status.Router = nil
		ModelDeploymentConditionRouterReady.True(holder,
			modelDeploymentReasonRouterNotApplicable, "the deployment declares no router")
		return 0, nil
	}

	objects, err := renderModelDeploymentRouterObjects(ctx, md, manufacturers)
	if err != nil {
		// A render refusal is a spec problem, not a cluster one: it is projected as the condition
		// and the rest of the status computes anyway, because returning here would freeze every
		// other field while naming none of them. The convergence path returns the same error, so
		// the pass is still retried.
		ModelDeploymentConditionRouterReady.False(holder,
			modelDeploymentReasonRouterRenderFailed, err.Error())
		return 0, nil
	}
	poolEndpoint := ""
	if domain != nil && domain.KVCache != nil {
		pool := new(workercore.KVCachePool)
		err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: domain.KVCache.Pool}, pool)
		if err != nil && !kerrors.IsNotFound(err) {
			return 0, err
		}
		if err == nil {
			poolEndpoint = pool.Status.ClientEndpoint
		}
	}
	status := projectModelDeploymentRouterStatus(&objects.Contract, poolEndpoint)
	holder.Status.Router = status

	deployment := new(app.Deployment)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: objects.Deployment.Name}, deployment)
	if kerrors.IsNotFound(err) {
		ModelDeploymentConditionRouterReady.Unknown(holder,
			modelDeploymentReasonRouterNotDeployed, "the router's Deployment does not exist yet")
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if deployment.Status.ReadyReplicas == 0 {
		ModelDeploymentConditionRouterReady.False(holder,
			modelDeploymentReasonRouterNoReadyReplica,
			fmt.Sprintf("router %q has no ready replicas", md.Spec.Router.Name))
		return 0, nil
	}

	ModelDeploymentConditionRouterReady.True(holder,
		modelDeploymentReasonRouterReady,
		fmt.Sprintf("router %q has %d ready replicas", md.Spec.Router.Name, deployment.Status.ReadyReplicas))
	holder.Status.Endpoint = status.Endpoint
	return deployment.Status.ReadyReplicas, nil
}

func projectModelDeploymentRouterStatus(
	contract *workercore.ModelDeploymentRouterStatus, poolEndpoint string,
) *workercore.ModelDeploymentRouterStatus {
	status := contract.DeepCopy()
	status.PoolEndpoint = poolEndpoint

	return status
}

// modelDeploymentRoleStatuses counts, per role, how many replicas the spec asks for, how many of
// them are Ready, how many hold a quota reservation, and which flavors the assigned ones landed on.
//
// A role's replicas are identified by the Pod's resource note rather than by parsing its name,
// because a name is a rendering and a note is what the renderer recorded.
//
// THE COUNTS AND THE FLAVOR SET ARE TAKEN OVER THE REPLICAS THAT ARE STAYING. A replica on its way
// out still holds its workload and its quota, and counting it would let a shortfall be met by
// replicas the deployment is shedding; its flavor is leaving with it, and naming it would answer
// "where is this role landing" with a placement the role is moving off of.
//
// THE QUOTA COUNT AND THE FLAVOR SET READ THE SAME RESOLUTION BUT ASK DIFFERENT QUESTIONS, so the
// second is not gated on the first: a workload carries its assignment after the reservation it came
// with is gone -- a preemption in flight reads QuotaReserved false with the admission still naming
// the flavor -- and "which model did this replica land on" is a question that moment still has an
// answer to. What the two cannot do is disagree about WHICH replicas were looked at.
func modelDeploymentRoleStatuses(
	md *workercore.ModelDeployment, pods []core.Pod,
	wlByReplica map[types.UID]*kueue.Workload,
) []workercore.ModelDeploymentRoleStatus {
	// EVERY FIGURE HERE IS PER REPLICA, WHICH IS WHY THE PODS ARE GROUPED FIRST. Both counts read
	// wrong when taken per Pod once a replica may be several of them, and they read wrong in
	// different directions: Ready would report a replica that is half up as partly serving, when a
	// group whose members are not all up serves nothing at all; QuotaReserved would report one
	// reservation once per member, because the members of a replica share a single Workload, so a
	// role of two replicas of four Pods would claim eight reservations against a desired count of
	// two. At one member per replica the grouping is the identity, which is why this reads exactly
	// as it did for the shape that came before.
	//
	size := make(map[string]int, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		size[md.Spec.Roles[i].Name] = modelDeploymentRoleSize(&md.Spec.Roles[i])
	}

	ready := make(map[string]int32, len(md.Spec.Roles))
	reserved := make(map[string]int32, len(md.Spec.Roles))
	flavors := make(map[string]sets.Set[string], len(md.Spec.Roles))
	for _, view := range modelDeploymentGroupPodsByReplica(pods) {
		role := view.Role

		// A REPLICA IS READY WHEN IT HOLDS EVERY MEMBER IT DECLARES AND ALL OF THEM ARE READY. The
		// completeness half is not redundant with the readiness half: a replica short of a member
		// can have every member it DOES hold reporting ready, while Kueue has composed no Workload
		// for it and it is admitted by nothing.
		allReady := modelDeploymentReplicaIsComplete(view, size[role])
		for _, pod := range view.Members {
			allReady = allReady && podIsReady(pod)
		}
		if allReady {
			ready[role]++
		}

		// THE REPLICA'S OWN WORKLOAD, not one resolved for its role: each replica composes one of
		// its own, and a missing one is indistinguishable from an unassigned one through any
		// coarser view. Its members all resolve to that same Workload, so it is read ONCE per
		// replica -- reading it per member is what would multiply the count by the size.
		var wl *kueue.Workload
		for _, pod := range view.Members {
			if wl = wlByReplica[pod.UID]; wl != nil {
				break
			}
		}
		if wl == nil {
			continue
		}
		if kubeapistatus.ConditionType(kueue.WorkloadQuotaReserved).IsTrue(wl) {
			reserved[role]++
		}
		if flavor := assignedFlavor(wl, kueue.PodSetReference(role)); flavor != "" {
			if flavors[role] == nil {
				flavors[role] = sets.New[string]()
			}
			flavors[role].Insert(string(flavor))
		}
	}

	statuses := make([]workercore.ModelDeploymentRoleStatus, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		statuses = append(statuses, workercore.ModelDeploymentRoleStatus{
			Name: role.Name,
			// The kind is ECHOED so an operator reading status alone sees which role is which. It is
			// resolved rather than copied: this field is required and enumerated, so writing an
			// unset spec kind through would be refused, and the API server refuses the status write
			// rather than the one field.
			Kind:          ModelDeploymentEffectiveRoleKind(role),
			Desired:       role.Replicas,
			Ready:         ready[role.Name],
			QuotaReserved: reserved[role.Name],
			// A role that replaced the whole command line got no synthesized argument and no client
			// environment, so nothing here can claim it is attached to the cache.
			Unmanaged:       len(role.Command) > 0,
			AssignedFlavors: modelDeploymentAssignedFlavors(flavors[role.Name]),
		})
	}

	return statuses
}

// modelDeploymentAssignedFlavors is the sorted set of flavors Kueue assigned this role's replicas,
// or nil when no assigned replica named one.
//
// NIL RATHER THAN EMPTY: "no replica holds an assignment" is a fact, and a present-but-empty list
// would read as an assignment to a flavor with no name — the same collapse the scalar form of this
// field refused with its pointer.
func modelDeploymentAssignedFlavors(flavors sets.Set[string]) []string {
	if flavors.Len() == 0 {
		return nil
	}

	return sets.List(flavors)
}

// modelDeploymentPodRole reads which role a replica belongs to.
func modelDeploymentPodRole(pod *core.Pod) string {
	return pod.Labels[modelDeploymentLabelKeyComponent]
}

// observeModelDeploymentQuota reports whether EVERY ONE of the deployment's replicas holds quota.
//
// Each replica is its own workload and reserves on its own, so `True` is an answer about the whole
// set rather than about whichever workload sorts first, and the branches below are what make it one:
// half a deployment holding quota is not the deployment holding quota, and reporting a survivor's
// answer for a replica whose own reservation is gone was a defect a cluster found.
//
// It reads those workloads' OWN conditions rather than asking the admission gate. The gate stops
// evaluating a workload once it is admitted, so anything derived from the gate would answer for
// the moment of admission and never again — and a workload that has been preempted since would still
// read as reserved.
//
// THE ORDER OF THE BRANCHES IS THE POINT, and PodGroupIncomplete has to come before "no workload
// yet". A replica that does not exist composes NO workload by construction — Kueue refuses to
// compose one and says so unretryably on the Pods — so the two states look identical from here and
// mean opposite things: one resolves itself in a moment, the other is the deployment sitting with
// gated Pods and an empty `kubectl get workloads` forever. Reporting the second as the first is
// the failure this reason exists to name.
func observeModelDeploymentQuota(
	md *workercore.ModelDeployment, pods []core.Pod, wls []*kueue.Workload,
	wlByReplica map[types.UID]*kueue.Workload, holder *workercore.ModelDeployment,
) {
	// A PARKED DEPLOYMENT IS REPORTED FIRST, because every other answer below would describe it
	// wrongly. Its groups are complete and their Workloads deactivated, which the vocabulary here
	// otherwise reads as "waiting for admission" -- the opposite of the truth once the bound has
	// fired, and the reading that sends an operator to wait for something that is never coming.
	//
	// IT IS OBSERVED HERE RATHER THAN WRITTEN BY THE CONTROLLER THAT PARKED IT. Status is rebuilt from
	// observed state on every pass by this one function, so a second writer would leave its own field
	// behind the moment this one disagreed. The bound is measured on the Workload; this reads what
	// that measurement left there.
	if parked := parkedModelDeploymentWorkloads(wls); len(parked) > 0 {
		ModelDeploymentConditionQuotaReserved.False(holder, "Parked", fmt.Sprintf(
			"the deployment's groups could not all be placed, so %d of its workloads are deactivated "+
				"and no longer ask for quota: %s. An identical re-apply does not clear this — free the "+
				"capacity the deployment needs and reactivate them, or delete the deployment and "+
				"create it again",
			len(parked), strings.Join(parked, ", ")))

		return
	}

	// A DEPLOYMENT PREEMPTED IN PART IS REPORTED BEFORE ANY OTHER WAIT, and it is a different fact
	// from every one of them: part of it is still admitted and serving, holding accelerators for a
	// deployment that is no longer whole, while the rest waits for quota that was taken.
	//
	// EVERY OTHER BRANCH DESCRIBES IT WRONGLY, and one of them says something FALSE. The multi-group
	// wait below ends with "no role is admitted until the whole set can run"; here a role IS admitted,
	// and it is the reason the state matters. The completeness branch would report the preempted
	// group as merely short of its replicas, which sends an operator to look for a scheduling problem
	// rather than at the higher-priority workload that took the quota.
	//
	// THE REASON CARRIES THE CLASSIFICATION RATHER THAN THE MESSAGE. Telling this from an ordinary
	// wait decides what an operator does -- wait, or go and look at what preempted it -- and a
	// consumer that has to substring-match a message to make that decision is reading something that
	// is not a contract.
	//
	// THE WORKLOAD SURVIVES PREEMPTION, WHICH IS WHAT MAKES THIS READABLE AT ALL, and it is read out of
	// Kueue rather than assumed: its preemption path calls workload.Evict with the Preempted reason
	// and a prepare step that sets the Preempted condition, and Evict patches status -- nothing on
	// that path deletes the object. Both conditions are therefore present to be read.
	//
	// WHAT IS STILL ASSUMED IS WHAT HAPPENS TO THE REPLICAS, and this branch is placed before the
	// completeness check so that the answer cannot change what gets reported. If a preempted serving
	// group's Pods are removed, this is what keeps that group from being announced as merely short of
	// its total; if they stay, the ordering is inert. Both ways the report is right, and the
	// assumption decides only whether this ordering is load-bearing.
	lost, kept, lostReplicas := modelDeploymentPreemptedInPart(md, pods, wlByReplica)
	// WHAT ELSE IS WRONG STILL GETS REPORTED, and the preemption is carried down with it. A role
	// preempted while no sibling survived is not the state PreemptedInPart names -- nothing is being
	// held -- but "something took a replica's quota" is a fact that would otherwise be dropped, and
	// without it the answer sends the reader to investigate the wrong thing.
	//
	// EVERY ANSWER THAT CAN BE GIVEN WHILE A REPLICA OF THIS DEPLOYMENT IS PREEMPTED CARRIES IT, and
	// the ones that do not are listed here with the reason, because "deliberately not carried" and
	// "forgotten" read the same in code:
	//
	//   - NoReplicas cannot be reached with a preemption observed. The Workload of a replica is
	//     resolved through that replica's ownership, so a deployment with no Pods resolves no Workload
	//     and there is nothing on which a preemption could have been seen.
	//   - NoQueueInReservedNamespace is the same in a different way: a reserved namespace carries no
	//     LocalQueue, so a deployment there is never scheduled, never holds quota, and has none to lose.
	//   - Parked supersedes it rather than missing it. Those Workloads are deactivated and no longer ask
	//     for quota at all, so who took some of it earlier does not decide anything the reader does
	//     next. It is also answered before this is computed, which is why it reads as an omission.
	//
	// AllReplicasTerminating DOES carry it, and it is the one that looks unreachable and is not: a
	// replica's workload is resolved whether or not it is terminating, so a preempted replica whose
	// Pod is on its way out lands exactly there.
	taken := modelDeploymentPreemptionNote(lost)
	if len(lost) > 0 && len(kept) > 0 {
		// WHAT WAS LOST DEPENDS ON THE SHAPE, so it is computed rather than asserted. Losing some
		// replicas of every kind costs capacity; losing every replica of one kind costs that role
		// outright, and an operator's next step differs between them.
		//
		// NEITHER SENTENCE SAYS THE DEPLOYMENT STOPPED, and the earlier one that did was wrong. A
		// prefill role supplies ingress throughput while a decode role's nodes can complete a
		// request by themselves, so a deployment that keeps decode is degraded and still serving.
		// Which losses leave it able to answer is a question about the roles and the routing in
		// front of them, not one this branch can settle from quota alone -- and the phase already
		// carries availability, by summing replicas across roles without branching on kind.
		//
		// THE SENTENCE MUST NOT IMPLY THE KIND WAS EVER ADMITTED. A kind counts here when no replica
		// of it is admitted, and a role that never was -- one still short of its declared count,
		// so Kueue has composed nothing for its missing replicas -- reaches this branch too, because
		// the preemption of a sibling kind's replica is answered before incompleteness is.
		effect := "the deployment still serves, without the capacity those replicas provided"
		if unserved := modelDeploymentKindsWithoutAdmittedReplica(md, pods, wlByReplica); len(unserved) > 0 {
			effect = fmt.Sprintf(
				"the deployment has no admitted replica of role kind %s at all, which is the loss of "+
					"that role rather than of capacity",
				strings.Join(unserved, ", "))
		}

		ModelDeploymentConditionQuotaReserved.False(holder, modelDeploymentReasonPreemptedInPart,
			fmt.Sprintf(
				"a higher-priority workload reclaimed the quota of %d of this deployment's %d replicas: "+
					"the replicas of roles %s. The replicas of roles %s are still admitted and hold "+
					"their accelerators, and %s. They are released only by deleting the deployment "+
					"or by the reclaimed replicas being admitted again once the capacity returns",
				lostReplicas, modelDeploymentDeclaredReplicas(md), strings.Join(lost, ", "),
				strings.Join(kept, ", "), effect))

		return
	}

	if len(pods) == 0 {
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "NoReplicas",
			"no replica has been created yet")

		return
	}

	// IN REPLICAS, NOT PODS, because every message below spells this figure "replicas" and compares
	// it against a declared count that is in replicas. The two agree only while a replica is one
	// Pod.
	live := len(modelDeploymentGroupPodsByReplica(pods))
	all := len(modelDeploymentGroupPodsIncludingDeparting(pods))

	// EVERY REPLICA TERMINATING IS NOT "ALL RESERVED". The early return above guards an empty LIST;
	// this guards an empty RESULT, and they are different emptinesses. A recreate rollout or a group
	// rebuild hands this function a non-empty slice whose every member is skipped, and reporting on
	// what is left would claim a guarantee over a set it just finished emptying.
	//
	// Unknown rather than False: no replica holds quota, but none is being refused any either.
	if live == 0 {
		// THE PREEMPTION IS CARRIED HERE TOO, and this is the branch where it matters most: replicas
		// on their way out is what a reclaimed group looks like from the Pod side, so "all of them are
		// terminating" without the reason why sends the reader to look for who deleted them.
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "AllReplicasTerminating", fmt.Sprintf(
			"all %d replicas are terminating, so none holds quota to report on%s", all, taken))

		return
	}

	// THE COUNT IS PER ROLE, and so is the queue it names. Each replica composes its own workload
	// once its one-member group is seen, so a role short of its declared count has replicas Kueue
	// has composed nothing for while a sibling role's are all admitted, and comparing the
	// deployment-wide sum answers about neither: it reads a complete role as short whenever a
	// sibling is, and reports a queue that is not the one holding anything back.
	//
	// The ClusterQueue is named after the InstanceType, so the queue a refusal points at is read off
	// the spec rather than resolved: the LocalQueue the entrance label names is derived from it.
	// The message names the ROLE beside that queue, because the queue and the role are not the same
	// thing to identify: two roles can name one instance type, so a queue names the pool two roles
	// share and identifies neither, while the role names exactly the one that is short.
	// A replica is attributed to its role through the SAME LABEL every other figure on this status
	// reads, so a Pod cannot be counted in one place and not another -- and a replica that predates
	// the label, or one still being built, is counted against the role it belongs to rather than
	// against none.
	// THE COUNT IS OF COMPLETE REPLICAS, NOT OF PODS, and the distinction is what keeps this branch
	// working above one member per replica. group.TotalCount is a count of REPLICAS, so comparing a
	// tally of Pods against it reads a role of two replicas of four Pods as having eight of two --
	// the condition never fires, and the state it exists to name goes unreported precisely where it
	// is most likely: a group short of a member is one Kueue composes nothing for.
	views := modelDeploymentGroupPodsByReplica(pods)
	groups := modelDeploymentPodGroups(md)
	for _, group := range groups {
		size := 1
		for i := range md.Spec.Roles {
			if md.Spec.Roles[i].Name == group.Role {
				size = modelDeploymentRoleSize(&md.Spec.Roles[i])

				break
			}
		}

		var alive int
		for _, view := range views {
			if view.Role == group.Role && modelDeploymentReplicaIsComplete(view, size) {
				alive++
			}
		}

		if want := int(group.TotalCount); alive < want {
			ModelDeploymentConditionQuotaReserved.False(holder, modelDeploymentReasonPodGroupIncomplete,
				fmt.Sprintf(
					"%d of %d replicas role %q declares exist, so Kueue composes no workload for the "+
						"missing ones at all and there is nothing in cluster queue %q to hold quota "+
						"for them%s",
					alive, want, group.Role, group.InstanceType, taken))

			return
		}
	}

	// Every role is at its declared count, so what is left to report is whether every replica holds
	// quota.
	//
	// THE ANSWER IS OVER EVERY REPLICA, NOT OVER THE FIRST WORKLOAD. A deployment spread over
	// several instanceTypes has one workload per replica, and answering a role from whichever sorts
	// first says the deployment holds quota while half of it does not. Measured on a cluster: one
	// group reserved, its sibling's queue was held, and this condition read Reserved with a message
	// naming the deployment's whole replica count -- a reading that is wrong in both halves at once.
	// A SINGLE-ROLE DEPLOYMENT HAS ONE QUEUE AND ITS WORDING SAYS SO; a multi-role one has no single
	// queue to name, and naming the first role's is a statement about one role offered as a statement
	// about the deployment. So the queue is read only where the branch has already established there
	// is one role, and the multi-role branches name the roles they are actually talking about.
	queue := md.Spec.Roles[0].InstanceType

	if uncomposedRoles, uncomposed := modelDeploymentRolesWithUncomposedReplicas(md, pods, wlByReplica); uncomposed > 0 {
		if kuberess.IsReservedNamespace(md.Namespace) {
			ModelDeploymentConditionQuotaReserved.False(holder, "NoQueueInReservedNamespace", fmt.Sprintf(
				"the deployment is in reserved namespace %q and will never be scheduled", md.Namespace))

			return
		}
		if len(groups) == 1 {
			ModelDeploymentConditionQuotaReserved.Unknown(holder, "AdmissionInFlight", fmt.Sprintf(
				"%d of this deployment's %d replicas have no workload yet in cluster queue %q%s",
				uncomposed, modelDeploymentDeclaredReplicas(md), queue, taken))

			return
		}
		ModelDeploymentConditionQuotaReserved.Unknown(holder, "AdmissionInFlight", fmt.Sprintf(
			"%d of this deployment's %d replicas have no workload yet: the replicas of roles %s%s",
			uncomposed, modelDeploymentDeclaredReplicas(md),
			strings.Join(uncomposedRoles, ", "), taken))

		return
	}

	waitingRoles, waiting := modelDeploymentRolesWithUnquotaedReplicas(md, pods, wlByReplica)
	if waiting == 0 {
		// The single-role wording is kept close to what an operator reading this condition today
		// already recognizes, because for one role it is exactly right.
		if len(groups) == 1 {
			ModelDeploymentConditionQuotaReserved.True(holder, "Reserved", fmt.Sprintf(
				"all %d replicas have quota reserved in cluster queue %q", live, queue))

			return
		}
		ModelDeploymentConditionQuotaReserved.True(holder, "Reserved", fmt.Sprintf(
			"all %d of this deployment's replicas have quota reserved", live))

		return
	}
	downstream := modelDeploymentInadmissibleWorkloadDetails(pods, wlByReplica)

	if len(groups) == 1 {
		ModelDeploymentConditionQuotaReserved.False(holder, "Pending", fmt.Sprintf(
			"%d of this deployment's %d replicas are waiting for quota in cluster queue %q%s%s",
			waiting, modelDeploymentDeclaredReplicas(md), queue, taken, downstream))

		return
	}
	// THE BARRIER IS STATED ABOUT THE REPLICAS STILL WAITING, not about the deployment. A scale-up
	// reaches this branch with the replicas that were already running still admitted and only the new
	// ones queued, so a sentence saying no replica is admitted contradicts the count beside it and
	// tells an operator nothing is serving while the deployment serves.
	ModelDeploymentConditionQuotaReserved.False(holder, "Pending", fmt.Sprintf(
		"%d of this deployment's %d replicas are waiting for quota: the replicas of roles %s. Those "+
			"replicas are admitted only once the whole set can run%s%s",
		waiting, modelDeploymentDeclaredReplicas(md), strings.Join(waitingRoles, ", "), taken, downstream))
}

// modelDeploymentInadmissibleWorkloadDetails preserves Kueue's explanation while adding the role
// and replica identity an operator needs to find the affected group. Kueue's reason and message are
// downstream prose, not a stable ModelDeployment reason vocabulary, so they are displayed but
// never parsed or promoted into a new condition reason.
func modelDeploymentInadmissibleWorkloadDetails(
	pods []core.Pod, wlByReplica map[types.UID]*kueue.Workload,
) string {
	type detail struct {
		role, workload, message string
		ordinal                 int
	}
	var details []detail
	for _, view := range modelDeploymentGroupPodsByReplica(pods) {
		if !view.Seated {
			continue
		}
		wl := modelDeploymentReplicaWorkload(view, wlByReplica)
		if wl == nil || kubeapistatus.ConditionType(kueue.WorkloadQuotaReserved).IsTrue(wl) {
			continue
		}
		for _, condition := range wl.Status.Conditions {
			if condition.Type == kueue.WorkloadQuotaReserved && condition.Message != "" {
				details = append(details, detail{
					role: view.Role, ordinal: view.Ordinal, workload: wl.Name, message: condition.Message,
				})
				break
			}
		}
	}
	slices.SortFunc(details, func(a, b detail) int {
		if byRole := strings.Compare(a.role, b.role); byRole != 0 {
			return byRole
		}
		if a.ordinal != b.ordinal {
			return a.ordinal - b.ordinal
		}
		return strings.Compare(a.workload, b.workload)
	})
	if len(details) == 0 {
		return ""
	}
	parts := make([]string, 0, len(details))
	for _, item := range details {
		parts = append(parts, fmt.Sprintf("role %q replica %d workload %q: %s",
			item.role, item.ordinal, item.workload, item.message))
	}
	return "; downstream admission: " + strings.Join(parts, "; ")
}

// modelDeploymentReplicaWorkloads resolves, for every replica this pass observed, the one Workload
// owning it.
//
// THE WORKLOAD IS READ BY OWNERSHIP AND BY NOTHING ELSE. The name this operator derives for a
// group moves with the spec, and whatever composes Workloads names them by its own rules -- Kueue
// today, something else if the substrate ever changes -- so a reader that reconstructed the name
// would not error on the day the two diverge; it would find nothing and report an empty status.
// Ownership is the one relation both sides keep, so it is the only thing this matches on.
//
// ONE ENTRY PER REPLICA RATHER THAN PER ROLE, because a role's replicas are one workload each now,
// and every question the status asks -- how many hold quota, which flavors they landed on, which
// slot of which role is empty -- is a question about replicas. A per-role view that answered for a
// role from whichever workload owned any of its Pods could not tell a role that lost one replica's
// workload from one that kept them all: the lookup hits either way, and the figures it fed carried
// the survivors' answer for the missing replica too.
//
// THE INDEX COVERS EVERY POD, TERMINATING ONES INCLUDED, because a workload holding a departing
// replica is still the thing holding its quota and its finalizer, and the readers that must not
// count a departing replica skip it at their own branch rather than the index pre-deciding for
// them.
//
// The Workloads arrive in name order, so a replica briefly owned by two during a rebuild resolves
// to the same one on every pass rather than flipping while the old one drains.
//
// THE OWNERSHIP IS INVERTED ONCE rather than searched per Pod. A Workload names its members, so
// reading the references forward builds the whole answer in one walk; asking each Pod which Workload
// claims it walks every Workload's references again for every Pod, and a deployment's Pods and its
// Workloads both grow with it. Keeping the FIRST Workload to claim a UID is what preserves the name
// order above: the walk visits the Workloads in that order, so the entry a contested replica lands
// on is the same one the per-Pod search would have stopped at.
func modelDeploymentReplicaWorkloads(
	pods []core.Pod, wls []*kueue.Workload,
) map[types.UID]*kueue.Workload {
	owners := make(map[types.UID]*kueue.Workload, len(pods))
	for _, wl := range wls {
		for _, ref := range wl.OwnerReferences {
			if !modelDeploymentOwnerRefNamesAPod(ref) {
				continue
			}
			if _, claimed := owners[ref.UID]; !claimed {
				owners[ref.UID] = wl
			}
		}
	}

	byReplica := make(map[types.UID]*kueue.Workload, len(pods))
	for i := range pods {
		if wl := owners[pods[i].UID]; wl != nil {
			byReplica[pods[i].UID] = wl
		}
	}

	return byReplica
}

// modelDeploymentPreemptedInPart names the roles with a replica whose quota a higher-priority
// workload reclaimed, and the roles with a replica that is still admitted, when BOTH lists are
// non-empty, and counts the reclaimed replicas.
//
// THE SECOND LIST IS THE WHOLE PREDICATE. A deployment every one of whose replicas was preempted
// is an ordinary state: it holds nothing, serves nothing, and is waiting for capacity exactly as a
// deployment that never started is. What makes this one different is that part of it kept its quota
// and is running — so accelerators are held for a deployment that is no longer whole, and neither
// half of that is visible from the other half alone. Whether the rest still serves is a question
// about the KINDS the reclaimed replicas carried, answered separately and never assumed. Reading
// only "was anything preempted" answers the same for both, which is the shape this is written
// against.
//
// A REPLICA WITH NO WORKLOAD COUNTS AS NEITHER, and that asymmetry is deliberate. Kueue composes
// no workload for a replica that does not exist, so such a replica holds nothing and there is
// nothing on it to read: it cannot be a survivor holding accelerators, and it cannot be shown to
// have been preempted. Two consequences follow, and they are not the same.
//
// WITH A SURVIVOR PRESENT THE PREEMPTION WINS THE REPORT, even though another replica is still
// assembling. Something took capacity this deployment was admitted for, which is the one fact an
// operator can act on, and the assembling replica resolves itself.
//
// WITH NO SURVIVOR THERE IS NOTHING TO HOLD, so this is not the state this reason names and the
// report falls through to the branch that describes what else is wrong. That branch used to say
// nothing about the preemption at all -- it sent the reader to investigate missing replicas when
// something had taken a replica's quota -- so the fact is carried down instead of the reason being
// widened to cover a state whose harm it does not describe.
//
// A ROLE CAN SIT ON BOTH LISTS AT ONCE now that each of its replicas is its own workload: one
// replica reclaimed beside a sibling still admitted is exactly the fragmentation this reason
// names, and sorting the role onto one list or the other would drop one of its two facts.
//
// PREEMPTION IS ASKED OF KUEUE RATHER THAN INFERRED. Kueue names it: an evicted workload carries a
// reason, and Preempted is one value among PodsReadyTimeout, AdmissionCheck and the queue-stopped
// ones. Inferring it from "lost its reservation" would fold every one of those into this answer, and
// they need different actions.
func modelDeploymentPreemptedInPart(
	md *workercore.ModelDeployment, pods []core.Pod, wlByReplica map[types.UID]*kueue.Workload,
) (lost, kept []string, lostReplicas int) {
	// Grouped without dropping the departing, unlike the figures above: replicas on their way out
	// is what a reclaimed group looks like from the Pod side, so skipping them here would hide the
	// preemption this function exists to name.
	views := modelDeploymentGroupPodsIncludingDeparting(pods)
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		var lostRole, keptRole bool
		for _, view := range views {
			if view.Role != role.Name {
				continue
			}
			wl := modelDeploymentReplicaWorkload(view, wlByReplica)
			if wl == nil {
				continue
			}
			switch {
			case modelDeploymentWorkloadPreempted(wl):
				lostRole = true
				lostReplicas++
			case kubeapistatus.ConditionType(kueue.WorkloadAdmitted).IsTrue(wl):
				keptRole = true
			}
		}
		if lostRole {
			lost = append(lost, role.Name)
		}
		if keptRole {
			kept = append(kept, role.Name)
		}
	}

	return lost, kept, lostReplicas
}

// modelDeploymentWorkloadPreempted reports whether Kueue took this Workload's quota back for a
// higher-priority one.
//
// BOTH CONDITIONS ARE READ because they are written at different moments and either can be the one
// still standing: Kueue sets Preempted when it decides, and Evicted with the Preempted reason when it
// acts. A reader of one alone answers differently depending on which write it arrives between.
func modelDeploymentWorkloadPreempted(wl *kueue.Workload) bool {
	if kubeapistatus.ConditionType(kueue.WorkloadPreempted).IsTrue(wl) {
		return true
	}

	for i := range wl.Status.Conditions {
		c := &wl.Status.Conditions[i]
		if c.Type == kueue.WorkloadEvicted && c.Status == meta.ConditionTrue &&
			c.Reason == kueue.WorkloadEvictedByPreemption {
			return true
		}
	}

	return false
}

// modelDeploymentKindsWithoutAdmittedReplica names the role kinds no admitted replica carries, in
// the order the roles first mention them.
//
// IT IS WHAT DECIDES WHETHER A PARTIALLY PREEMPTED DEPLOYMENT IS STILL SERVING, and it has to be
// asked about KINDS rather than about roles. Two roles can share one kind, so losing one role's
// every replica while a sibling kind's survive leaves the deployment able to serve that half, and
// counting roles answers neither question.
//
// A ROLE WHOSE REPLICAS HAVE NO WORKLOAD COUNTS AS NOT ADMITTED. Kueue composes none for a replica
// that does not exist, and a replica that is not admitted is not serving whatever the reason. That
// is the accurate reading here, unlike in the preemption predicate next door, where the same state
// means "nothing to report about this replica" rather than "this replica is gone".
func modelDeploymentKindsWithoutAdmittedReplica(
	md *workercore.ModelDeployment, pods []core.Pod, wlByReplica map[types.UID]*kueue.Workload,
) []string {
	order := make([]string, 0, len(md.Spec.Roles))
	served := make(map[string]bool, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		kind := string(ModelDeploymentEffectiveRoleKind(role))
		if _, seen := served[kind]; !seen {
			order = append(order, kind)
			served[kind] = false
		}

		for j := range pods {
			if modelDeploymentPodRole(&pods[j]) != role.Name {
				continue
			}
			if wl := wlByReplica[pods[j].UID]; wl != nil &&
				kubeapistatus.ConditionType(kueue.WorkloadAdmitted).IsTrue(wl) {
				served[kind] = true

				break
			}
		}
	}

	var unserved []string
	for _, kind := range order {
		if !served[kind] {
			unserved = append(unserved, kind)
		}
	}

	return unserved
}

// modelDeploymentDeclaredReplicas sums the replicas every role declares, which is the denominator
// the per-replica answers name.
func modelDeploymentDeclaredReplicas(md *workercore.ModelDeployment) int {
	declared := 0
	for i := range md.Spec.Roles {
		declared += int(md.Spec.Roles[i].Replicas)
	}

	return declared
}

// modelDeploymentRolesWithUncomposedReplicas names the roles that have a live replica Kueue has
// composed no workload for, and counts those replicas. A live replica in that state is
// mid-admission; a missing one is the completeness branch's answer above, which is why the two are
// not the same list.
//
// DEPARTING REPLICAS ARE NOT COUNTED, for the same reason every figure beside this one skips them:
// a replica on its way out is not waiting for a workload, and counting it would hold the condition
// on a member the deployment is shedding.
func modelDeploymentRolesWithUncomposedReplicas(
	md *workercore.ModelDeployment, pods []core.Pod, wlByReplica map[types.UID]*kueue.Workload,
) (roles []string, uncomposed int) {
	views := modelDeploymentGroupPodsByReplica(pods)
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		var short int
		for _, view := range views {
			if view.Role != role.Name {
				continue
			}
			if modelDeploymentReplicaWorkload(view, wlByReplica) == nil {
				short++
			}
		}
		if short > 0 {
			roles = append(roles, role.Name)
			uncomposed += short
		}
	}

	return roles, uncomposed
}

// EVERY FIGURE THIS FILE PUBLISHES IS IN REPLICAS, because that is the unit `desired` is in and the
// unit every message names. A Pod-keyed loop produces a figure in Pods, which is the same number
// only while a replica is one Pod: a role of two replicas of two would report "4 of this
// deployment's 2 replicas are waiting", a statement about a deployment that cannot exist.
//
// The members of a replica share one Workload, so a Pod-keyed loop over an unreserved replica also
// counts that one Workload once per member -- the miscount is a multiplication by `size` rather
// than a stray increment, and it grows with exactly the field this shape was added for.

// modelDeploymentReplicaWorkload is the Workload the members of one replica share, or nil when Kueue
// has composed none -- which is what an incomplete group looks like, since Kueue composes nothing
// until a group holds every member it declares.
//
// It is read from the first member that answers rather than from a fixed one: a replica short of
// its leader still has the Workload its surviving members joined.
func modelDeploymentReplicaWorkload(
	view modelDeploymentReplicaView, wlByReplica map[types.UID]*kueue.Workload,
) *kueue.Workload {
	for _, pod := range view.Members {
		if wl := wlByReplica[pod.UID]; wl != nil {
			return wl
		}
	}

	return nil
}

// modelDeploymentRolesWithUnquotaedReplicas names the roles that have a live replica whose
// workload holds no quota reservation, and counts those replicas.
//
// A REPLICA IS ANSWERED BY THE WORKLOAD OWNING IT. Answering a role from one workload for the
// whole of it cannot distinguish a set that is fully reserved from one where one replica's
// reservation is gone -- a state one workload per role could not even express -- and those two are
// the difference between a deployment that is about to run and one that is partly held.
func modelDeploymentRolesWithUnquotaedReplicas(
	md *workercore.ModelDeployment, pods []core.Pod, wlByReplica map[types.UID]*kueue.Workload,
) (roles []string, waiting int) {
	views := modelDeploymentGroupPodsByReplica(pods)
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		var short int
		for _, view := range views {
			if view.Role != role.Name {
				continue
			}
			if wl := modelDeploymentReplicaWorkload(view, wlByReplica); wl == nil ||
				!kubeapistatus.ConditionType(kueue.WorkloadQuotaReserved).IsTrue(wl) {
				short++
			}
		}
		if short > 0 {
			roles = append(roles, role.Name)
			waiting += short
		}
	}

	return roles, waiting
}

// findModelDeploymentGroupWorkloads returns EVERY Workload owning any of these Pods, in name order.
//
// THERE IS NO SINGULAR FORM OF THIS, deliberately. One that returned the first was here and had no
// production caller left: every question is about one replica, and which Workload answers for which
// replica is decided by the ownership each one carries (modelDeploymentReplicaWorkloads)
// rather than by sort order. A correctly written accessor that only serves a world model this
// package has abandoned is not dead weight, it is an invitation to reintroduce the defect it
// enables -- and it draws no review comment, because it is not wrong.
//
// IT MATCHES ON A PLAIN OWNER REFERENCE, NOT A CONTROLLER REFERENCE, and that is not a relaxation —
// it is the difference between finding the Workload and never finding one. Kueue sets a CONTROLLER
// reference when it builds a Workload from a single Pod, and plain owner references to EVERY member
// when it builds one from a pod group (`SetOwnerReference` per Pod, not `SetControllerReference`).
// So the moment the replicas became a group, a controller-reference filter started matching nothing,
// and every deployment would have reported "no workload yet" forever while being admitted normally.
//
// The API version is checked with the kind: "Pod" is not a reserved word, and a resource of that
// kind in another group would match on the kind alone.
//
// A rebuild can briefly leave the OLD group's Workload owning Pods that are on their way out. The
// scan is over the Pods this pass observed, and the result is taken in name order so that two
// candidates cannot make the reported state flip between passes while the old one drains. A caller
// that takes only the first leaves the rest in place -- and each of those still holds Kueue's
// finalizer on its own replicas, which is the one thing that keeps them from ever leaving. The
// symptom is a deployment stuck in Deleting with nothing erroring.
//
// Callers that scope the Pods scope the result: passing one group's replicas returns that group's
// Workload and no other, which is how a rebuild reaches exactly the group it is rebuilding.
func (r *ModelDeploymentReconciler) findModelDeploymentGroupWorkloads(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) ([]*kueue.Workload, error) {
	if len(pods) == 0 {
		return nil, nil
	}

	ours := sets.New[types.UID]()
	for i := range pods {
		ours.Insert(pods[i].UID)
	}

	wlList := new(kueue.WorkloadList)
	err := r.Client.List(ctx, wlList, ctrlcli.InNamespace(md.Namespace), ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}

	var found []*kueue.Workload
	for i := range wlList.Items {
		wl := &wlList.Items[i]
		if modelDeploymentWorkloadOwnsAny(wl, ours) {
			found = append(found, wl)
		}
	}
	slices.SortFunc(found, func(a, b *kueue.Workload) int { return strings.Compare(a.Name, b.Name) })

	return found, nil
}

// modelDeploymentWorkloadOwnsAny reports whether the Workload names any of these Pods as an owner.
func modelDeploymentWorkloadOwnsAny(wl *kueue.Workload, pods sets.Set[types.UID]) bool {
	for _, ref := range wl.OwnerReferences {
		if modelDeploymentOwnerRefNamesAPod(ref) && pods.Has(ref.UID) {
			return true
		}
	}

	return false
}

// modelDeploymentOwnerRefNamesAPod reports whether an owner reference names a Pod, which is the only
// kind of owner a replica's Workload is matched on.
//
// THE KIND IS CHECKED BECAUSE A UID ALONE IS NOT A RELATION. A Workload carries whatever owners its
// composer gave it, and a reference to some other kind that happened to match a Pod's UID would make
// an unrelated object answer for a replica. Every reader of this relation goes through here -- the
// status and the rollout guard matching Workloads to replicas, and the Workload watch and the joint
// admission barrier walking a Workload back to its deployment -- so that none of them can come to
// disagree about what owning a replica means.
func modelDeploymentOwnerRefNamesAPod(ref meta.OwnerReference) bool {
	return ref.Kind == "Pod" && ref.APIVersion == "v1"
}

// observeModelDeploymentRoleKinds reports whether every role kind has a ready replica.
//
// IT READS THE ROLE STATUSES THIS PASS JUST COMPUTED rather than the spec, so its answer cannot
// disagree with the counts published beside it. Reading the spec instead would let the condition
// speak about a role whose replicas this pass failed to account for.
func observeModelDeploymentRoleKinds(holder *workercore.ModelDeployment) {
	roles := holder.Status.Roles
	if len(roles) == 0 {
		ModelDeploymentConditionRoleKindsReady.Unknown(holder, modelDeploymentReasonNoRoleStatuses,
			"no role has been accounted for yet")

		return
	}

	missing := modelDeploymentKindsWithoutReady(roles)
	if len(missing) == 0 {
		ModelDeploymentConditionRoleKindsReady.True(holder, modelDeploymentReasonAllKindsReady,
			"every role kind of this deployment has at least one ready replica")

		return
	}

	ModelDeploymentConditionRoleKindsReady.False(holder, modelDeploymentReasonKindsNotReady,
		fmt.Sprintf("no replica is ready for role kinds %s", strings.Join(missing, ", ")))
}

// modelDeploymentKindsWithoutReady names the role kinds no ready replica was counted for, in the
// order the roles first mention them.
//
// It is PURE: same statuses, same result, no client and no clock.
//
// THE THRESHOLD IS ONE READY REPLICA, NEVER ALL OF THEM. "All of them" is replica completeness,
// which the phase already derives; asking it here would put one question in two fields, and the two
// would be free to disagree the moment either is edited. One is what makes this a question about
// the deployment's SHAPE: whether each half of a disaggregated deployment is represented at all.
//
// THE ORDER IS THE ROLES' ORDER, first mention winning, so two passes over unchanged statuses
// produce the same message. A map's iteration order would not, and this list is rendered into a
// condition message that is compared against the stored one to decide whether to write at all.
//
// AN UNMANAGED ROLE'S READY REPLICAS COUNT, and that is a decision rather than an oversight. Such a
// role replaced the whole command line, so the renderer attached no probes to it and its Pods are
// Ready once their containers start — a weaker statement than a probed role's Ready. It still
// counts, because the operator did not build that command line and has nothing better to judge it
// by, and because discounting it would leave a deployment whose roles all took over reporting a
// missing kind forever. What it costs is that this condition's True rests, for such a kind, on the
// kubelet's container-start signal alone; status.roles[].Unmanaged is published per role, so a
// reader needing the stronger reading can apply it without this field folding it in.
func modelDeploymentKindsWithoutReady(roles []workercore.ModelDeploymentRoleStatus) []string {
	order := make([]string, 0, len(roles))
	ready := make(map[string]bool, len(roles))

	for i := range roles {
		kind := string(roles[i].Kind)
		if _, seen := ready[kind]; !seen {
			order = append(order, kind)
			ready[kind] = false
		}
		if roles[i].Ready > 0 {
			ready[kind] = true
		}
	}

	var missing []string
	for _, kind := range order {
		if !ready[kind] {
			missing = append(missing, kind)
		}
	}

	return missing
}

// deriveModelDeploymentPhase summarizes the counts into the one field a human reads first.
//
// Degraded means SOME replicas are ready and some are not, which is the state worth telling apart:
// the deployment is serving, at less than the capacity that was asked for. A deployment with nothing
// ready is Starting whatever the reason — to a reader, a replica that has not been admitted and one
// that has not finished loading its weights are the same state, and phaseMessage is what
// distinguishes them.
func deriveModelDeploymentPhase(md, holder *workercore.ModelDeployment, routerReady bool) {
	if md.DeletionTimestamp != nil {
		holder.Status.Phase = ModelDeploymentPhaseDeleting
		holder.Status.PhaseMessage = "the replicas are being torn down"

		return
	}

	var desired, ready int32
	for _, rs := range holder.Status.Roles {
		desired += rs.Desired
		ready += rs.Ready
	}

	switch {
	case ready == desired && md.Spec.Router != nil && !routerReady:
		holder.Status.Phase = ModelDeploymentPhaseDegraded
		// The observer always sets the condition on the paths that report not-ready, and its
		// message is the accurate one: it can say the router failed to render, which "has no ready
		// replicas" cannot.
		holder.Status.PhaseMessage = ModelDeploymentConditionRouterReady.GetMessage(holder)
	case ready == desired:
		holder.Status.Phase = ModelDeploymentPhaseReady
		holder.Status.PhaseMessage = ""
	case ready > 0:
		holder.Status.Phase = ModelDeploymentPhaseDegraded
		holder.Status.PhaseMessage = fmt.Sprintf("%d of %d replicas are ready", ready, desired)
	default:
		holder.Status.Phase = ModelDeploymentPhaseStarting
		holder.Status.PhaseMessage = ModelDeploymentConditionQuotaReserved.GetMessage(holder)
	}
}

// annotateModelDeploymentPhase says in the phase message what the phase alone cannot: why a
// deployment waiting on its weights has no replica, and which replica was evicted. The weights'
// wait is checked first because while it holds nothing is created, so it is the whole answer.
func annotateModelDeploymentPhase(holder *workercore.ModelDeployment, pods []core.Pod) {
	if holder.Status.Phase == ModelDeploymentPhaseReady || holder.Status.Phase == ModelDeploymentPhaseDeleting {
		return
	}
	if ModelDeploymentConditionWeightsReady.IsFalse(holder) && len(pods) == 0 {
		holder.Status.PhaseMessage = ModelDeploymentConditionWeightsReady.GetMessage(holder)
		return
	}
	if note := modelDeploymentEvictionNote(pods); note != "" {
		if holder.Status.PhaseMessage == "" {
			holder.Status.PhaseMessage = note
			return
		}
		holder.Status.PhaseMessage += "; " + note
	}
}

// parkedModelDeploymentWorkloads names the deployment's Workloads the joint barrier has deactivated.
//
// spec.active is what the barrier writes when it gives up holding a set, and it is durable state the
// scheduler reads: while it is false the Workload is not considered and the group is not rebuilt
// behind it.
//
// BUT THAT FLAG ALONE DOES NOT SAY WHO WROTE IT. Kueue deactivates a Workload of its own accord, and
// so can an operator pausing one group by hand. Reporting either as "parked for infeasibility" tells
// the reader the deployment's set could not be placed and instructs them to free capacity or delete
// and recreate -- an answer to a question nobody asked, about a state somebody chose. So the barrier's
// OWN check has to be carrying its verdict on the same Workload for this to be the barrier's doing.
func parkedModelDeploymentWorkloads(wls []*kueue.Workload) []string {
	var parked []string
	for _, wl := range wls {
		if !kueueworkload.IsActive(wl) && jointBarrierParked(wl) {
			parked = append(parked, wl.Name)
		}
	}

	return parked
}

// jointBarrierParked reports whether the joint-admission check is the reason this Workload is
// inactive, by reading the verdict that controller left on it.
func jointBarrierParked(wl *kueue.Workload) bool {
	acs := jointCheckEntry(wl)

	return acs != nil && strings.Contains(acs.Message, _JointAdmissionParkedMarker)
}

// modelDeploymentPreemptionNote is the clause the other quota answers carry when a group of this
// deployment was preempted but the state is not the one PreemptedInPart names.
//
// IT IS A SUFFIX RATHER THAN A REASON OF ITS OWN, because it is not what the condition is about: the
// branch it lands on has already said what is wrong, and this adds who took a group's quota. Giving
// it a reason would mean two classifications of one state, and the one that resolves the reader's
// next action is the branch's own.
func modelDeploymentPreemptionNote(lost []string) string {
	if len(lost) == 0 {
		return ""
	}

	return fmt.Sprintf(
		". A higher-priority workload has also reclaimed the quota of the replicas of roles %s",
		strings.Join(lost, ", "))
}
