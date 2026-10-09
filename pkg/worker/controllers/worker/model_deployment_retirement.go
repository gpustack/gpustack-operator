// The retirement reservation: the one protocol that is allowed to delete a replica, and the gate
// that keeps every other path's hands off it while it runs.
//
// A reservation is a persisted decision about ONE replica. It is written into the status by
// whatever admits it, and this file carries it to its end. Everything the protocol must not get
// wrong is expressed as three-valued answers and holds: a replica that might still be serving is
// never deleted on the strength of an observation that failed to arrive.
//
// TWO DECISIONS, NEVER ONE. Hold is the interception predicate -- whether a delete naming one of
// the target's UIDs must be refused this pass, whatever the path that wants it. DeletesAllowed is
// the protocol's own permission to commit its delete. They are separate because they are separate
// questions, and a single flag would make the two indistinguishable in the status: a path refused
// because the replica is reserved looks identical to a path the protocol itself is holding, and an
// operator reading the status could not tell which happened.
//
// WITH NO RESERVATION PRESENT NOTHING HERE RUNS. The plan is empty, Hold is false and every delete
// path below behaves exactly as it did before this file existed. That is a property this task has
// to earn per path rather than assert, because it is the property that makes adding a protocol to
// a controller safe.
//
// THE STATES ARE LEVELS, NOT STEPS. Every pass re-enters at the persisted state and either stays,
// advances or aborts, so a pass that was never told what happened makes the same decision a pass
// that was. Nothing is done because a previous pass said so, and a controller restart re-enters
// rather than re-decides -- which is what persisting phaseStartedAt buys.
package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// THE PROTOCOL'S BUDGETS, frozen. A phase ends at the earlier of the overall deadline and its own
// budget measured from phaseStartedAt, so no phase outlives the operation and a restart cannot
// extend one.
//
// THEY ARE NOT THE PRESTOP HOOK'S. The hook carries its own settle and exit reserve
// (modelDeploymentDrainSettleSeconds and modelDeploymentDrainExitSeconds) and runs inside the
// container on SIGTERM, while these run in the controller against a replica that is still
// untouched. The two are separate mechanisms and reading either number as the other's is an easy
// and completely silent error, so they are declared here rather than borrowed from there.
const (
	// modelDeploymentRetirementWithdrawalBudget is one withdrawal phase in two steps: the target
	// leaves selection, and then the answer confirming it serves nothing is read. It is ONE budget
	// because the spec names one, and splitting it would double the time a group spends out of
	// selection for no decision that does not already exist.
	modelDeploymentRetirementWithdrawalBudget = 30 * time.Second

	// modelDeploymentRetirementDrainBudget is how long the protocol waits for the engine's own
	// in-flight queues to reach zero, with every member still running.
	modelDeploymentRetirementDrainBudget = 240 * time.Second

	// modelDeploymentRetirementSettleBudget is how long the protocol waits for the accelerator and
	// the quota to be released after the delete, which is a convergence rather than a decision.
	modelDeploymentRetirementSettleBudget = 30 * time.Second

	// modelDeploymentRetirementOverallBudget bounds the whole operation.
	modelDeploymentRetirementOverallBudget = 300 * time.Second
)

// modelDeploymentRetirementRetryAnnotation is the explicit directive that retries an aborted
// reservation. It is a directive and not a timer: an abort is terminal until something asks for it
// again, which is what stops a replica that cannot drain from being deleted once per reconcile.
//
// The value is roleName:replicaOrdinal:opaqueRetryToken. The token is opaque by contract -- its
// origin is not provable across deployments, so no provenance is read out of it and a new string is
// simply a new request.
const modelDeploymentRetirementRetryAnnotation = "modeldeployment.gpustack.ai/retirement-retry"

// modelDeploymentRetirementPlan is what one pass learned about the single reservation in flight.
//
// A zero plan is the no-reservation plan, and it is what every delete path sees when no operation
// is running: Hold false, DeletesAllowed true, nothing held.
type modelDeploymentRetirementPlan struct {
	// Reservation is the state this pass ends at, to be persisted with the rest of the status. It
	// is nil both when there is no operation and when a completed one has just been cleared, and
	// the two are told apart by Clear.
	Reservation *workercore.ModelDeploymentRetirementStatus

	// Target is the replica the reservation names, resolved against the Pods as they exist now.
	Target modelDeploymentReplicaView

	// held is the set of member UIDs the reservation claims. It is separate from Target so a path
	// can ask about one Pod without rebuilding the grouping, which is the question every delete
	// site actually asks.
	held sets.Set[types.UID]

	// Hold is whether a delete naming a held UID must be refused this pass.
	Hold bool

	// DeletesAllowed is the protocol's own permission to commit its delete. It is true at Deleting
	// and Settling and false everywhere the protocol is still withdrawing or draining.
	DeletesAllowed bool

	// Clear is that a completed operation has ended and the reservation is to be removed.
	Clear bool

	// InFlight is that the operation is still running, so the pass must come back to advance it.
	// Nothing else wakes a deployment whose replica is merely waiting for a queue to empty.
	InFlight bool

	// Deletes is the target the protocol itself is deleting this pass. It is empty at every other
	// state, including the ones where DeletesAllowed is true but the delete was already issued on
	// an earlier pass.
	Deletes []core.Pod
}

// holds reports whether a Pod is one of the operation's targets.
func (p modelDeploymentRetirementPlan) holds(uid types.UID) bool {
	return p.Hold && p.held.Has(uid)
}

// names reports whether the reservation claims this replica by name.
//
// IT IS ABOUT THE SLOT, NOT THE MEMBERS, and the distinction matters at the rollout. The protocol
// holds individual Pods by UID, so a rollout that condemned a replica would delete only the members
// it did not hold -- turning a per-replica decision into a partial one, and leaving the operation
// retiring a replica whose other members the rollout has already taken. A rollout asked to act on
// a reserved replica is asked to step over it entirely.
func (p modelDeploymentRetirementPlan) names(role string, ordinal int) bool {
	return p.InFlight && p.Reservation != nil && p.Reservation.RoleName == role &&
		int(p.Reservation.ReplicaOrdinal) == ordinal
}

// planModelDeploymentRetirement evaluates the reservation as a level and returns what this pass
// should persist and what it may do.
//
// A NO-RESERVATION PASS IS NOT AN ERROR AND IS NOT A HELD ONE. It returns the zero plan, and every
// caller treats that as "carry on exactly as before".
func (r *ModelDeploymentReconciler) planModelDeploymentRetirement(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) *modelDeploymentRetirementPlan {
	reservation := md.Status.Retirement
	if reservation == nil {
		return &modelDeploymentRetirementPlan{DeletesAllowed: true}
	}

	plan := &modelDeploymentRetirementPlan{
		Reservation:    reservation.DeepCopy(),
		DeletesAllowed: true,
		InFlight:       true,
	}
	plan.Target, plan.held = modelDeploymentRetirementTarget(reservation, pods)

	// A COMPLETED OPERATION IS CLEARED, and clearing it is the only thing this pass does about it.
	// The reservation is not left behind as a tombstone because a lingering reservation keeps
	// refusing the health predicate and keeps holding a slot no operation occupies.
	if reservation.State == workercore.ModelDeploymentRetirementStateCompleted {
		plan.Clear = true
		plan.InFlight = false

		return plan
	}

	// AT AND PAST Deleting AN EMPTY TARGET IS THE DELETE LANDING, not a loss. The states from
	// Deleting on are about the target being absent, so an absent target is the only thing they
	// can report; treating it as external termination there would abort the operation it had just
	// finished and record a retention that never happened.
	if reservation.State != workercore.ModelDeploymentRetirementStateAdmitted &&
		reservation.State != workercore.ModelDeploymentRetirementStateDisqualified &&
		reservation.State != workercore.ModelDeploymentRetirementStateWithdrawing &&
		reservation.State != workercore.ModelDeploymentRetirementStateDraining &&
		reservation.State != workercore.ModelDeploymentRetirementStateAborted {
		// FROM Deleting ON, THE HOLD STAYS ON. The operation has committed to the delete and is
		// counting on the group it deletes from still being there, so every other path must keep
		// off the target for the whole of the settle -- and, more sharply, the ordinary
		// convergence is looking at the same Pods this pass, and a hold that lifted here would let
		// the surplus rule collect the target along with everything else the spec no longer
		// declares. A retirement deletes a replica member by member; the convergence's removal is
		// a per-replica decision, and the two disagree in exactly this state.
		plan.Hold = true
		// The caller's context travels with it. A pass that was canceled has not read the server,
		// and a post-delete step that ran on a fresh background context would answer a question
		// about the operation that the caller already stopped asking.
		r.advanceModelDeploymentRetirementPastDeletion(ctx, md, plan)

		return plan
	}

	// THE TARGET IS GONE, OR GONE IN THE ONLY WAY A TARGET CAN LEAVE WITHOUT A DELETE. A replica in a
	// terminal phase has already departed: the kubelet evicted it, or the engine exited, and a
	// terminal Pod never runs again. There is nothing for the protocol to withdraw and nothing for
	// it to drain, so continuing to hold the target would keep an aborting operation alive against a
	// Pod that is already dead -- and the drain it is waiting for would be a queue belonging to an
	// engine that has stopped serving. So the operation says what happened and releases the target,
	// and the ordinary terminal-pod path finishes the departure the protocol can no longer prevent.
	//
	// The terminal state is Aborted and the reason says why, because the frozen state set has no
	// separate encoding for it and the clean abort's wording would claim three retentions that did
	// not happen. The hold is released with it: a target that cannot be retained must not be held.
	if plan.held.Len() == 0 || modelDeploymentRetirementTargetTerminal(plan) {
		plan.Reservation.State = workercore.ModelDeploymentRetirementStateAborted
		plan.Reservation.Reason = "the target was terminated outside this operator; " +
			"nothing could be retained"
		plan.Hold = false
		plan.InFlight = false

		return plan
	}

	// FROM HERE THE OPERATION HOLDS. Every state that is not yet the committed delete keeps the
	// target, including the aborted one: an abort retains capacity deliberately, and a retained
	// replica that a later scale-down then deleted would not be retained at all.
	plan.Hold = true

	// A RE-DECLARED, HEALTHY, SAME-UID TARGET IS ADOPTED, not retired. A scale-up that names the
	// ordinal again while the operation is aborted means the replica is wanted after all, and the
	// answer is to cancel the operation and let ordinary convergence keep the Pods it already has.
	// A different UID at the ordinal is a different group and is external termination above, so
	// adoption cannot silently absorb a replacement.
	if r.adoptsModelDeploymentRetirement(ctx, md, plan) {
		plan.Clear = true
		plan.Hold = false
		plan.InFlight = false

		return plan
	}

	// A RETRY DIRECTIVE IS AN EXPLICIT INSTRUCTION, and it is consumed before the state machine
	// looks at the state at all, so a retry from Aborted resumes at Admitted rather than resuming
	// the step that aborted.
	if r.consumeModelDeploymentRetry(ctx, md, plan) {
		// The state was written and the annotation cleared by the consumption itself, on purpose
		// and in that order. The pass re-enters on the next one so the ordering is observable
		// rather than assumed.
		plan.InFlight = true

		return plan
	}

	switch reservation.State {
	case workercore.ModelDeploymentRetirementStateAborted:
		// AN ABORT IS TERMINAL UNTIL NEW INTENT. Re-entering the step that aborted on every pass
		// would make a replica that cannot drain a delete once per reconcile, which is the opposite
		// of what an abort is for.
		plan.InFlight = false

	case workercore.ModelDeploymentRetirementStateAdmitted:
		r.advanceModelDeploymentRetirementToDisqualified(ctx, md, plan)

	case workercore.ModelDeploymentRetirementStateDisqualified:
		r.advanceModelDeploymentRetirementToWithdrawing(ctx, md, plan)

	case workercore.ModelDeploymentRetirementStateWithdrawing:
		r.advanceModelDeploymentRetirementToDraining(ctx, md, plan)

	case workercore.ModelDeploymentRetirementStateDraining:
		r.advanceModelDeploymentRetirementToDeleting(ctx, md, plan)

	case workercore.ModelDeploymentRetirementStateDeleting:
		r.advanceModelDeploymentRetirementToSettling(ctx, md, plan)

	case workercore.ModelDeploymentRetirementStateSettling:
		r.advanceModelDeploymentRetirementToCompleted(ctx, md, plan)
	}

	return plan
}

// advanceModelDeploymentRetirementPastDeletion runs the states from Deleting on, which are reached
// on the strength of the target being ABSENT rather than on any observation of it.
//
// The hold is already in force on entry, so nothing outside this protocol deletes the target while
// the delete is in flight; that is what makes the delete below a single decision rather than a race
// with the ordinary convergence.
func (r *ModelDeploymentReconciler) advanceModelDeploymentRetirementPastDeletion(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) {
	if err := ctx.Err(); err != nil {
		plan.Clear = false
		plan.Reservation.Reason = fmt.Sprintf("the pass was cancelled, so the release is not observed: %v", err)

		return
	}
	switch plan.Reservation.State {
	case workercore.ModelDeploymentRetirementStateDeleting:
		r.advanceModelDeploymentRetirementToSettling(ctx, md, plan)
	case workercore.ModelDeploymentRetirementStateSettling:
		r.advanceModelDeploymentRetirementToCompleted(ctx, md, plan)
	}
}

// commitModelDeploymentRetirement issues the operation's delete, and it is the ONLY place in this
// controller that deletes a replica on the protocol's own authority.
//
// EVERY DELETE CARRIES ITS UID AS A PRECONDITION, which is what keeps a same-name replacement from
// being deleted in the target's place. The reservation holds members by UID precisely so that this
// is enforceable, and a delete without the precondition would make the UID set advisory -- a target
// deleted and a replacement created between the plan and the call would see the delete succeed
// against the replacement and report the operation complete while the original is still running.
//
// THE WORKLOAD GOES WITH THE PODS, as one decision, for the same reason it does everywhere else in
// this file: it is what releases the finalizer Kueue holds on every member of the group, and a Pod
// delete without it leaves the replica terminating forever.
func (r *ModelDeploymentReconciler) commitModelDeploymentRetirement(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
	pods []core.Pod,
) error {
	logger := ctrllog.FromContext(ctx)
	if !plan.DeletesAllowed || len(plan.Deletes) == 0 {
		return nil
	}

	for i := range plan.Deletes {
		pod := &plan.Deletes[i]
		uid := pod.UID
		if err := r.Client.Delete(ctx, pod,
			ctrlcli.Preconditions{UID: &uid}); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete retired replica %s: %w", pod.Name, err)
		}
		logger.Info("deleting retired replica member", "pod", pod.Name, "state", plan.Reservation.State)
	}

	return r.commitModelDeploymentRetirementWorkload(ctx, md, plan, pods)
}

// commitModelDeploymentRetirementWorkload deletes the Workloads that would otherwise hold the
// retired members' finalizers, and it is the subtle half of the delete.
//
// THE WORKLOAD IS THE KILL SWITCH AND IT IS GROUP-SCOPED. Kueue holds a deleted Pod of a managed
// group until that GROUP's Workload goes, and a group is a role's serving set rather than one
// replica's: four replicas of a role can share one Workload. Deleting it for the sake of one
// retiring replica releases the finalizer on all four, and the retirement takes the whole
// deployment down with it. The Pod delete above is per-member and safe; this one is not, which is
// exactly why it is asked as a question rather than issued.
//
// SO A WORKLOAD IS DELETED ONLY WHEN IT OWNS NOTHING OUTSIDE THE TARGET. A replica that is the
// whole group goes, and its Workload goes with it because it is about to be nothing anyway. A
// replica that is one of several leaves its Workload alone: the group is still serving, the
// remaining members keep their finalizer behind a Workload that still describes them, and the
// retired member is released when the group is next composed without it.
//
// The question is asked about the deployment's LIVE Pods rather than about the Workload's own
// status, so a Workload whose membership could not be read from its own object is not mistaken for
// one that describes nothing else. Being unable to prove the group is the target means not deleting.
func (r *ModelDeploymentReconciler) commitModelDeploymentRetirementWorkload(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
	pods []core.Pod,
) error {
	workloads, err := r.findModelDeploymentGroupWorkloads(ctx, md, plan.Deletes)
	if err != nil {
		return err
	}

	target := sets.New[types.UID]()
	for i := range plan.Deletes {
		target.Insert(plan.Deletes[i].UID)
	}
	outsiders := sets.New[types.UID]()
	for i := range pods {
		if !target.Has(pods[i].UID) {
			outsiders.Insert(pods[i].UID)
		}
	}

	for _, workload := range workloads {
		if modelDeploymentWorkloadOwnsAny(workload, outsiders) {
			continue
		}

		uid := workload.UID
		if err = r.Client.Delete(ctx, workload,
			ctrlcli.Preconditions{UID: &uid}); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete retired replica's workload %s: %w", workload.Name, err)
		}
	}

	return nil
}

// persistModelDeploymentRetirement writes the state this pass reached, and writes nothing when the
// pass reached the state that was already there.
//
// IT IS ITS OWN WRITE rather than a field set on the object before the pass's status sync, and the
// reason is that the sync compares the derived status with the observed one to decide whether to
// write at all. A field assigned in place would be equal to itself by the time the comparison ran,
// so the operation's progress would never reach the API server and every pass would re-enter at
// the state the first one reached. A pass that changed nothing writes nothing, which is what keeps
// a controller running on every Pod event from rewriting the object continuously.
//
// A NO-RESERVATION PASS NEVER WRITES: both sides are nil, and the comparison is the whole answer.
func (r *ModelDeploymentReconciler) persistModelDeploymentRetirement(
	ctx context.Context, md *workercore.ModelDeployment,
	previous *workercore.ModelDeploymentRetirementStatus, plan *modelDeploymentRetirementPlan,
) error {
	next := plan.Reservation
	if plan.Clear {
		next = nil
	}

	return r.writeModelDeploymentRetirement(ctx, md, previous, next)
}

// writeModelDeploymentRetirement persists the reservation and nothing else.
//
// The three early retirement writes share one shape, and they share one failure mode if they do
// not share one writer: a full-object status write sends the whole typed status back to the API
// server, including every field the object happens to carry, and the server validates all of it.
// A stored object written by an older version can carry enum values the current schema does not
// accept in fields this operation never touches, and then a write that only means "move the
// operation one phase on" is refused for something it did not say. Patching only the retirement
// field sends only what this operation asserts.
//
// The patch is optimistic: it carries the resourceVersion this pass read, so a stored object that
// moved underneath the write is a Conflict rather than a silent overwrite. A no-op writes nothing
// at all, and clearing a reservation writes an explicit null, because a patch that omits the field
// leaves whatever was there.
//
// The object is updated from the server's response, so a later write in the same pass carries the
// version this one produced rather than the one this pass started with.
func (r *ModelDeploymentReconciler) writeModelDeploymentRetirement(
	ctx context.Context, md *workercore.ModelDeployment,
	previous, next *workercore.ModelDeploymentRetirementStatus,
) error {
	if kubemeta.DeepEqual(previous, next) {
		return nil
	}

	// The base is the object as the server last had it, which is why the caller hands both sides
	// over. A base copied after the caller assigned the new value would already contain it, and the
	// diff between the two would be empty.
	base := md.DeepCopy()
	base.Status.Retirement = previous
	candidate := base.DeepCopy()
	candidate.Status.Retirement = next

	optimistic := ctrlcli.MergeFromWithOptions(base, ctrlcli.MergeFromWithOptimisticLock{})
	if err := r.Client.Status().Patch(ctx, candidate, optimistic); err != nil {
		return err
	}

	// The patch decodes the server's response into the candidate, so the version this write produced
	// is the version the next write in the pass carries. Nothing is read back, because that would
	// ask the client for a Get this writer does not otherwise depend on.
	//
	// The candidate is copied back only once the server has accepted it. A write that fails leaves
	// the caller's object describing what the server still holds, which is what the rest of the pass
	// reads to decide whether a token was consumed and whether a delete is allowed.
	md.ResourceVersion = candidate.ResourceVersion
	md.Status = candidate.Status

	return nil
}

// modelDeploymentRetirementTarget resolves a reservation to the replica it names, by UID.
//
// IT IS RESOLVED BY UID RATHER THAN BY (role, ordinal) ALONE, which is the whole reason the wire
// carries member UIDs. The slot can be refilled while the operation runs -- a rollout replacing
// the ordinal, a hand creating a Pod, a member re-rendered -- and a target read off the ordinal
// would then name the replacement and delete it in the reserved member's place. A UID names one
// object for as long as that object exists, so a target that has been replaced resolves to nothing
// and is reported as external termination rather than silently retargeted.
func modelDeploymentRetirementTarget(
	reservation *workercore.ModelDeploymentRetirementStatus, pods []core.Pod,
) (modelDeploymentReplicaView, sets.Set[types.UID]) {
	claimed := sets.New[string](reservation.TargetMemberUIDs...)
	held := sets.New[types.UID]()

	view := modelDeploymentReplicaView{Role: reservation.RoleName, Ordinal: int(reservation.ReplicaOrdinal)}
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || !claimed.Has(string(pod.UID)) {
			continue
		}

		// The UID set is authoritative, but the view is built from the labels so the protocol can
		// ask the questions that are about a replica -- is it seated, is it complete -- rather than
		// about a set of UIDs. A member whose labels disagree with the reservation is still held,
		// because the UID is the thing being protected.
		if ordinal, ok := modelDeploymentPodOrdinal(pod); ok {
			view.Seated = true
			view.Ordinal = ordinal
		}
		view.Members = append(view.Members, pod)
		held.Insert(pod.UID)
	}

	return view, held
}

// advanceModelDeploymentRetirementToDisqualified moves the operation from Admitted to
// Disqualified, which is where it waits for the target to be out of selection.
//
// THE WAIT IS FOR THE LABEL TO BE ABSENT, not for this function to remove it. The eligibility
// write already runs on the reserved replica's own account -- the health predicate's
// NoPlannedRetirement leg is a definite failure for exactly the replica a reservation names -- so
// the label leaves on the ordinary convergence and this only observes that it did. Rewriting the
// label here would put a second writer on a key the reconciler already owns, and two writers to
// one key are two answers to one question.
func (r *ModelDeploymentReconciler) advanceModelDeploymentRetirementToDisqualified(
	_ context.Context, _ *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) {
	// The phase start is not stamped here. Admitted is polled on every pass while the target
	// carries the eligibility key, so a stamp on entry would reset the budget on every pass.
	// Admission already persists the moment the phase began.
	now := r.modelDeploymentNow()
	if modelDeploymentRetirementBudgetSpent(plan.Reservation, modelDeploymentRetirementWithdrawalBudget, now) {
		r.abortModelDeploymentRetirement(plan, now,
			"the withdrawal budget was exhausted before the target left selection")

		return
	}

	for _, member := range plan.Target.Members {
		if member.Labels[modelDeploymentLabelKeyEndpointEligible] == modelDeploymentEndpointEligibleValue {
			plan.Reservation.Reason = "the target is still in endpoint selection"

			return
		}
	}

	plan.Reservation.State = workercore.ModelDeploymentRetirementStateDisqualified
	plan.Reservation.Reason = "the target has left endpoint selection"
}

// advanceModelDeploymentRetirementToWithdrawing moves the operation from Disqualified to
// Withdrawing, where it waits for the ACTUAL serving answer rather than for the label to be gone.
//
// The two are different facts. The label is what this operator wrote; the serving answer is what
// the Routers say about requests already in flight, and a request routed a moment before the label
// left is still occupying the target. Withdrawing is one phase with Disqualified and so keeps the
// phaseStartedAt Disqualified wrote rather than taking a fresh budget here.
func (r *ModelDeploymentReconciler) advanceModelDeploymentRetirementToWithdrawing(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) {
	now := r.modelDeploymentNow()
	if modelDeploymentRetirementBudgetSpent(plan.Reservation, modelDeploymentRetirementWithdrawalBudget, now) {
		r.abortModelDeploymentRetirement(plan, now,
			"the withdrawal budget was exhausted before the target stopped serving")

		return
	}

	residual, held := r.observeModelDeploymentRetirementResidual(ctx, md, plan.held)
	if held {
		plan.Reservation.Reason = residual

		return
	}

	plan.Reservation.State = workercore.ModelDeploymentRetirementStateWithdrawing
	plan.Reservation.Reason = "the target is out of selection and nothing serves it"
}

// advanceModelDeploymentRetirementToDraining moves the operation to Draining, where every member
// is still running and the protocol reads the engine's own queues.
func (r *ModelDeploymentReconciler) advanceModelDeploymentRetirementToDraining(
	_ context.Context, _ *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) {
	now := r.modelDeploymentNow()
	plan.Reservation.PhaseStartedAt = meta.NewTime(now)

	if modelDeploymentRetirementBudgetSpent(plan.Reservation, modelDeploymentRetirementDrainBudget, now) {
		r.abortModelDeploymentRetirement(plan, now,
			"the drain budget was exhausted before the target left deletion")

		return
	}

	plan.Reservation.State = workercore.ModelDeploymentRetirementStateDraining
	plan.Reservation.Reason = "reading the target's in-flight queues with every member retained"
}

// advanceModelDeploymentRetirementToDeleting reads every member's queues and, when they are idle,
// commits the delete. It is the only place the protocol deletes anything.
func (r *ModelDeploymentReconciler) advanceModelDeploymentRetirementToDeleting(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) {
	now := r.modelDeploymentNow()
	if modelDeploymentRetirementBudgetSpent(plan.Reservation, modelDeploymentRetirementDrainBudget, now) {
		r.abortModelDeploymentRetirement(plan, now,
			"the drain budget was exhausted before the target's queues reached zero")

		return
	}

	if reason, drained := r.observeModelDeploymentRetirementDrained(ctx, md, plan); !drained {
		plan.Reservation.Reason = reason

		return
	}

	// THE RELEASE EVIDENCE IS CAPTURED AND PERSISTED BEFORE THE DELETE IS AUTHORIZED. This is the
	// last moment the members, their allocation records and their Workloads are all still present:
	// a member deleted here cannot be reconstructed from the pods that remain, and an operation
	// that cannot say what it held cannot later say the hold came back. A capture that fails holds
	// the operation, because deleting now would destroy the very evidence completion needs.
	record, err := r.captureModelDeploymentRetirementRelease(ctx, md, plan)
	if err != nil {
		plan.Reservation.Reason = fmt.Sprintf(
			"the target's release cannot be recorded, so nothing is deleted: %v", err)
		plan.DeletesAllowed = false
		plan.Deletes = nil

		return
	}
	if err := r.persistModelDeploymentRetirementRelease(ctx, md, record); err != nil {
		plan.Reservation.Reason = fmt.Sprintf(
			"the target's release could not be written, so nothing is deleted: %v", err)
		plan.DeletesAllowed = false
		plan.Deletes = nil

		return
	}
	// The identity is re-read after the write, so a record that did not land for THIS operation
	// stops here rather than being discovered missing once the delete is committed.
	if _, err := r.loadModelDeploymentRetirementRelease(md, plan); err != nil {
		plan.Reservation.Reason = fmt.Sprintf(
			"the target's release was not recorded for this operation, so nothing is deleted: %v", err)
		plan.DeletesAllowed = false
		plan.Deletes = nil

		return
	}

	plan.Reservation.State = workercore.ModelDeploymentRetirementStateDeleting
	plan.Reservation.PhaseStartedAt = meta.NewTime(now)
	plan.Reservation.Reason = "the target's queues are empty; deleting every member together"
	plan.DeletesAllowed = true
	plan.Deletes = modelDeploymentRetirementDeleteSet(plan.Target)
}

// advanceModelDeploymentRetirementToSettling watches for the delete to have actually happened.
//
// THE STATE IS ABOUT THE API SERVER, NOT ABOUT THE DELETE CALL. A delete that was issued is a
// request, and Kueue holds a deleted member of a serving group on the server until that group's
// Workload goes, so the Pod outlives its own delete by a margin this pass cannot see. Settling
// begins when the target is absent from the server, which is the only moment the capacity is
// genuinely free.
func (r *ModelDeploymentReconciler) advanceModelDeploymentRetirementToSettling(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) {
	now := r.modelDeploymentNow()
	plan.DeletesAllowed = true

	// THE HELD SET IS NOT ABSENCE. It skips objects with a deletion timestamp, so a member that is
	// terminating empties it while the object is still on the server and still carved. The delete
	// is re-issued from it, and the transition waits on a fresh read of the captured UIDs instead.
	//
	// The observation is deliberately limited to the members here. The accelerator and quota
	// release is observed at Settling, where the ledger and the Workloads can be compared against
	// what was captured; this step only establishes that the target is really gone.
	reason, absent := r.observeModelDeploymentRetirementTargetsGone(ctx, md, plan)
	if !absent {
		plan.Reservation.Reason = reason
		plan.Deletes = modelDeploymentRetirementDeleteSet(plan.Target)

		return
	}

	plan.Reservation.State = workercore.ModelDeploymentRetirementStateSettling
	plan.Reservation.PhaseStartedAt = meta.NewTime(now)
	plan.Reservation.Reason = "the target has left; observing the accelerator and quota release"
}

// advanceModelDeploymentRetirementToCompleted ends the operation once the release is observed.
//
// NOTHING AT OR PAST Deleting ABORTS. A budget exhausted there cannot retain anything, because the
// target is already gone, and an abort reporting retained members, a retained Workload and
// retained capacity would be reporting three things that do not exist. The operation stays in the
// state it reached and records the reason, which is the only honest answer available once the
// deletion is committed.
func (r *ModelDeploymentReconciler) advanceModelDeploymentRetirementToCompleted(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) {
	now := r.modelDeploymentNow()

	// THE RELEASE IS OBSERVED BEFORE THE BUDGET IS CONSULTED. A budget bounds waiting; it is not a
	// license to claim a result. An operation that observed the release late has observed it, and
	// reporting it as unobserved because the deadline passed would be the same fabrication this
	// predicate replaced. Only when the release has not been observed does the deadline decide what
	// to say, and then it says so without clearing anything.
	reason, released := r.observeModelDeploymentRetirementReleased(ctx, md, plan)
	if !released {
		if modelDeploymentRetirementBudgetSpent(plan.Reservation, modelDeploymentRetirementSettleBudget, now) {
			plan.Reservation.Reason = fmt.Sprintf(
				"the settle budget was exhausted and the release has not been observed: %s", reason)
		} else {
			plan.Reservation.Reason = reason
		}

		return
	}

	plan.Reservation.State = workercore.ModelDeploymentRetirementStateCompleted
	plan.Reservation.Reason = "the target is deleted and its accelerator and quota release were observed"
	plan.Clear = true
	plan.InFlight = false
	plan.Hold = false
}

// abortModelDeploymentRetirement ends a budget expiry that happened before anything was deleted.
//
// IT IS REACHED ONLY FROM A PRE-DELETE STEP. Those are the steps where the target, its Workload and
// its capacity are all still standing, which is the only point at which retaining them is
// available at all -- and the only point at which reporting them as retained is true.
//
// The phaseStartedAt written by the phase that expired is LEFT ALONE, so an aborted reservation
// carries the moment the last phase began rather than the moment it gave up. That is what a retry
// directive needs to report, and rewriting it here would make the two indistinguishable.
func (r *ModelDeploymentReconciler) abortModelDeploymentRetirement(
	plan *modelDeploymentRetirementPlan, now time.Time, reason string,
) {
	plan.Reservation.State = workercore.ModelDeploymentRetirementStateAborted
	plan.Reservation.Reason = reason + "; members, workload and capacity are retained"
	plan.Reservation.Deadline = meta.NewTime(now)
	plan.InFlight = false
}

// modelDeploymentRetirementBudgetSpent reports whether the phase this reservation is in has run out
// of time.
//
// THE PHASE ENDS AT THE EARLIER OF THE TWO DEADLINES. A phase budget longer than the remaining
// overall budget must not extend the operation past it, and an operation whose overall budget has
// passed must not be given a fresh phase by a generous per-phase number.
//
// `now` is the reconciler's clock, so a test can drive the budget by moving time rather than by
// waiting for it. Equality is exhausted: a phase ending exactly now has run out.
func modelDeploymentRetirementBudgetSpent(
	reservation *workercore.ModelDeploymentRetirementStatus, budget time.Duration, now time.Time,
) bool {
	phaseEnd := reservation.PhaseStartedAt.Add(budget)
	overallEnd := reservation.Deadline.Time
	if overallEnd.Before(phaseEnd) {
		phaseEnd = overallEnd
	}

	return !now.Before(phaseEnd)
}

// modelDeploymentRetirementTargetTerminal reports whether every member the reservation still holds
// has reached a terminal phase.
//
// EVERY MEMBER, because a replica is one group: one member that died while the others serve is a
// group that is no longer what the operation was withdrawing, and Kueue's own answer to a deleted
// member of an admitted group is to hold the survivors. The operation would then be draining a
// group whose forward path no longer exists, which is the same false claim as a target that is
// gone.
func modelDeploymentRetirementTargetTerminal(plan *modelDeploymentRetirementPlan) bool {
	if plan.held.Len() == 0 {
		return true
	}

	for _, member := range plan.Target.Members {
		if member.Status.Phase != core.PodFailed && member.Status.Phase != core.PodSucceeded {
			return false
		}
	}

	return true
}

// modelDeploymentRetirementDeleteSet is the Pod set the protocol deletes as ONE decision.
//
// The whole replica goes together, never a member of it, and that is not a simplification: the
// members share a single Kueue group, so a delete of some of them leaves a group Kueue will not
// admit and will not release, and the Workload delete that would follow stops the survivors anyway.
func modelDeploymentRetirementDeleteSet(view modelDeploymentReplicaView) []core.Pod {
	deletes := make([]core.Pod, 0, len(view.Members))
	for _, member := range view.Members {
		deletes = append(deletes, *member)
	}

	return deletes
}

// modelDeploymentNow reads the clock, which is a field rather than a call to time.Now so a test
// can place a budget — a reservation's phases or an elastic retry's backoff — at a moment of its
// choosing instead of waiting for one.
func (r *ModelDeploymentReconciler) modelDeploymentNow() time.Time {
	if r.clock == nil {
		return time.Now()
	}

	return r.clock()
}

// observeModelDeploymentRetirementResidual reports whether any Router still claims to be serving
// one of the target's members, and whether that question could be answered at all.
//
// THE QUESTION IS ASKED PER MEMBER RATHER THAN THROUGH THE DEPLOYMENT-WIDE ANSWER, and the reason
// is that the aggregate cannot express it. A deployment of three replicas has a serving count of
// two once the target stops serving, and reading "confirmed, value two" as "the target is still
// serving" would hold the operation until the rest of the deployment went too -- a wait with no
// exit. The aggregate is still consulted, for the state, the freshness and the generation rules
// it alone enforces; what this adds is the membership question the aggregate deliberately has no
// vocabulary for.
//
// THE COLLECTION IS THE T6 SHAPE, REUSED RATHER THAN REINVENTED: the same strict parse, the same
// identity-at-collection-time binding, the same aggregation. Only the final filter differs.
func (r *ModelDeploymentReconciler) observeModelDeploymentRetirementResidual(
	ctx context.Context, md *workercore.ModelDeployment, held sets.Set[types.UID],
) (string, bool) {
	// No router is no reading of the ingress. Service convergence stops new-connection selection,
	// but established connections keep delivering requests and nothing readable reports whether one
	// has gone quiet. So the answer is a hold rather than the absence of a residual.
	if md.Spec.Router == nil {
		return "this deployment declares no router, so the established direct-Service connections " +
			"this target's service may still hold cannot be observed; the operation holds", true
	}
	ctx, cancel := r.modelDeploymentRouterObservationContext(ctx, md)
	defer cancel()

	pods, err := r.listModelDeploymentPods(ctx, md)
	if err != nil {
		return "the deployment's pods could not be listed: " + err.Error(), true
	}
	live := make([]*core.Pod, 0, len(pods))
	for i := range pods {
		live = append(live, &pods[i])
	}

	// The same collection the serving answer uses, under the same single total budget. The residual
	// asks a different question of the result, not a different question of the Routers.
	observations, failure := r.collectModelDeploymentRouterObservations(ctx, md, live)
	if failure != "" {
		return failure, true
	}

	// The aggregate rules come first and are not optional: a view that is stale, unusable or from a
	// router that rolled mid-observation cannot support a per-member claim either, however well
	// the members happen to line up.
	answer := aggregateModelDeploymentServing(true, observations, modelDeploymentServingFreshness)
	switch answer.State {
	case workercore.ModelDeploymentServingStateUnknown:
		return answer.Reason, true
	case workercore.ModelDeploymentServingStateNotConverged:
		return answer.Reason, true
	}

	serving := sets.New[types.UID]()
	// The journal is added to what has to be checked, never to the serving count. A worker the
	// router removed from its selection is not serving, and counting it as such would keep every
	// other member of the deployment waiting on a count that is wrong. What it does mean is that a
	// dispatch reached this target, so releasing it now releases a member that may still be
	// serving. That is evidence enough to hold, and it is evidence about the target alone, so an
	// unrelated survivor's dispatch never holds the captured target.
	dispatched := sets.New[types.UID]()
	for _, observation := range observations {
		for _, endpoint := range observation.Bound {
			if endpoint.Serving {
				serving.Insert(endpoint.PodUID)
			}
		}
		for _, endpoint := range observation.RecentDispatch {
			dispatched.Insert(endpoint.PodUID)
		}
	}

	residual := sets.New[types.UID]()
	for uid := range serving {
		if held.Has(uid) {
			residual.Insert(uid)
		}
	}
	for uid := range dispatched {
		if held.Has(uid) {
			residual.Insert(uid)
		}
	}
	if residual.Len() == 0 {
		return "", false
	}

	names := make([]string, 0, residual.Len())
	for uid := range residual {
		names = append(names, string(uid))
	}
	slices.Sort(names)

	return strconvx.Itoa(len(names)) + " target members are still served: " +
		strings.Join(names, ", "), true
}

// observeModelDeploymentRetirementDrained reports whether every member of the target has been read
// idle, twice, and returns the reason it was not when it was not.
//
// THE SECOND RETURN IS WHETHER IT DRAINED, so every way of NOT draining returns false and a reason.
// A convention where the reason carried the meaning would have a caller that checked only the
// boolean, and a caller that checked only the reason would read the empty string as success.
//
// THE TWO READS ARE CONSECUTIVE WITHIN THE PASS, and that is what makes the rule about an
// observation rather than about a sample. A single read of a queue gauge that happens to catch the
// moment between two requests says zero while the replica is serving, and a protocol that deleted
// on it would cut live traffic. Two reads taken back to back in the same pass cannot both land in
// that window by chance, and they cost no persistence: a counter that survived between passes would
// need a field the wire does not have, and a counter that did not survive would make a replica whose
// reads alternate between idle and busy impossible to retire at all.
//
// THE READER IS NOT TRUSTED ALONE. A reader may claim completeness while having scraped two of the
// four SGLang gauges, and summing what it did find is exactly the naive read this protocol exists
// to refuse, so the expected series are checked here against the one list in the tree.
func (r *ModelDeploymentReconciler) observeModelDeploymentRetirementDrained(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) (string, bool) {
	// THE ROLE IS NOT REQUIRED TO BE DECLARED, and insisting on it is a deadlock rather than a
	// precaution. Admission hands over a removal the spec has already made -- a role that is no
	// longer declared, a count that is lower -- so the role the target belonged to is very often
	// GONE by the time the protocol reads its queues. The role is consulted for one thing, the
	// engine, and the engine is a property of the replica being retired: its own Pods still
	// describe it, and they are the very objects the reservation holds. A role lookup that refuses
	// would make the role-removal case the one case the protocol can never finish.
	role := modelDeploymentRetirementRole(md, plan)

	// A DISAGGREGATED REPLICA IS NOT MEASURED BY THE PREFILLER'S QUEUES ALONE. A decoder waiting to
	// receive KV is in the prefiller's transfer queue, and the prefiller reports that work while the
	// decoder that will serve it reports none -- so a decoder-side zero read off its own gauges is
	// zero on an engine that is about to be handed the whole model. The shape stays refused until a
	// decoder-side release can be verified, because the other answer deletes a decode cluster in
	// the middle of a prefill.
	// The disaggregation shape is asked only where the spec can still answer it. A role the spec
	// no longer declares cannot be read, and an absent answer is not a disaggregated one: the
	// replica's own Pods say what they run, and that is what the drain reader needs anyway.
	//
	// THE MEMBERS ANSWER, NOT THE ROLE. The role describes the replica about to replace this one, so
	// a flag added to the role would refuse to drain a leader-served replica that is still running a
	// supported command, and clearing it would let a replica that IS disaggregated through a guard
	// written to catch exactly that. What the replica is is on the members.
	//
	// A COMMAND NO MEMBER CAN SUPPLY IS A HOLD rather than a fallback to the role. The role's answer
	// here would be about a group nobody has read, and this guard exists to avoid deleting live
	// traffic on a shape it has not established.
	externalDP, renderReason := modelDeploymentRetirementRenderedExternalDP(md, plan.Target.Members)
	if renderReason != "" {
		return renderReason, false
	}
	// A member whose command cannot be read holds; the role is not consulted here at all.
	if len(plan.Target.Members) == 0 {
		return "the replica has no members to read", false
	}
	if !externalDP.known {
		return "a member carries no engine container to read, so the replica's own command line could " +
			"not be established", false
	}
	if externalDP.answer {
		return "a disaggregated replica has no verifiable decoder-side release", false
	}

	// WHICH MEMBERS ARE READ IS THE SHAPE'S DECISION, not the member list's. A leader-served replica
	// of several Pods answers through its leader alone, because the followers join a collective and
	// serve no metrics endpoint; reading them would hold the operation on gauges that do not exist.
	// The set the reservation bound is untouched, so the delete preconditions still cover every
	// member this operation admitted.
	answering, reason := r.modelDeploymentRetirementDrainMembers(md, plan)
	if reason != "" {
		return reason, false
	}

	reader := r.modelDeploymentDrainReaderOf()
	for _, member := range answering {
		target := modelDeploymentRetirementDrainTarget(md, role, plan, member)
		// TWO CONSECUTIVE READS, both of which must be idle, complete, and matched to this member by
		// every expected series. The first is dropped on the floor either way; it is taken so that a
		// single lucky zero cannot carry the protocol past a member that is still working.
		for read := 1; read <= 2; read++ {
			answer, err := reader.Drain(ctx, target)
			if err != nil {
				return fmt.Sprintf("member %s could not be read: %v", member.Name, err), false
			}
			if reason, idle := modelDeploymentDrainMemberIdle(member, target, answer); !idle {
				return reason, false
			}
		}
	}

	return "", true
}

// modelDeploymentRetirementRenderedExternalDP reads whether a replica is disaggregated from every
// member's own rendered command.
//
// EVERY MEMBER IS ASKED. A member whose engine container is not there has not been read, and a
// replica is not classifiable from the members that happened to be legible. The answer is reported
// alongside whether every member gave one, because callers hold on the missing case.
func modelDeploymentRetirementRenderedExternalDP(
	md *workercore.ModelDeployment, members []*core.Pod,
) (modelDeploymentRetirementCommand, string) {
	var answer modelDeploymentRetirementCommand
	var missing bool
	for _, member := range members {
		externalDP, known := modelDeploymentRetirementMemberExternalDP(md, member)
		if !known {
			missing = true

			continue
		}
		if answer.known && answer.answer != externalDP {
			return modelDeploymentRetirementCommand{}, "the replica's members disagree on whether it " +
				"is disaggregated, so its shape could not be established"
		}
		answer = modelDeploymentRetirementCommand{answer: externalDP, known: true}
	}
	// ONE UNREAD MEMBER MAKES THE WHOLE REPLICA UNREADABLE, and a later readable member does not
	// repair that: the members this operator could not read are still the ones it would delete.
	answer.known = answer.known && !missing

	return answer, ""
}

// modelDeploymentRetirementCommand is what the members said, and whether all of them said it.
type modelDeploymentRetirementCommand struct {
	answer bool
	known  bool
}

// modelDeploymentRetirementAnsweringShape classifies the replica from what the drain actually holds:
// the member Pods.
func modelDeploymentRetirementAnsweringShape(
	md *workercore.ModelDeployment, members []*core.Pod,
) (modelDeploymentAnsweringShape, string) {
	externalDP, size, reason := modelDeploymentRetirementReplicaFacts(md, members)
	if reason != "" {
		return modelDeploymentAnsweringUnknown, reason
	}

	return modelDeploymentAnsweringShapeOf(externalDP, size), ""
}

// modelDeploymentRetirementReplicaFacts are the two facts the shape is derived from, or the reason
// they could not be established. Both are read from the members; a replica no member can answer for
// is refused rather than answered from the role.
func modelDeploymentRetirementReplicaFacts(
	md *workercore.ModelDeployment, members []*core.Pod,
) (externalDP bool, size int, reason string) {
	size = len(members)
	if size == 0 {
		return false, 0, "the replica has no members to read"
	}

	// Every member must answer; skipping one would classify the replica from the members that
	// happened to be legible.
	command, cmdReason := modelDeploymentRetirementRenderedExternalDP(md, members)
	if cmdReason != "" {
		return false, 0, cmdReason
	}
	if !command.known {
		return false, 0, "a member carries no engine container to read, so the replica's own command " +
			"line could not be established"
	}

	// The replica is measured against its own declared shape, not the role: a role being edited
	// describes the replica about to be built. Seats are included because the leader search below
	// counts only seat zero, so a group of three declaring three with seats 0, 1 and 1 has a leader
	// and would otherwise drain.
	shape := modelDeploymentDeployedReplicaShape(modelDeploymentReplicaView{Members: members})
	if !shape.modelDeploymentReplicaIsWhole() {
		return false, 0, shape.Reason
	}

	return command.answer, size, ""
}

// modelDeploymentRetirementMemberExternalDP reads one member's own balance shape, and reports false
// for known when the member's engine container is not there to be read.
func modelDeploymentRetirementMemberExternalDP(
	md *workercore.ModelDeployment, member *core.Pod,
) (externalDP, known bool) {
	container, found := modelDeploymentDrainContainerOf(member, modelDeploymentMainContainerName)
	if !found {
		return false, false
	}

	_, disaggregated := modelDeploymentDrainDisaggregated(
		modelDeploymentRetirementEngine(md), container)

	return disaggregated, true
}

// modelDeploymentRetirementDrainMembers is the set of members the drain reads for this replica.
//
// THE SELECTION LIVES HERE AND NOT IN TARGET RESOLUTION. The reservation's member UIDs are the delete
// precondition binding and the set that must all be gone before the operation can complete, so
// narrowing them would make the commit delete less than admission bound and would let the operation
// report success with a member still running. What the drain reads is a measurement, and a
// measurement is this function's business alone.
//
// THE CLASSIFIER'S OWN REASON IS THE HOLD REASON. A replica whose members disagree, whose members
// do not match their declared total, or whose command cannot be read at all, is a different
// operator problem from one another, and collapsing them into a single "shape could not be
// established" would leave the reservation naming a symptom with no cause behind it.
func (r *ModelDeploymentReconciler) modelDeploymentRetirementDrainMembers(
	md *workercore.ModelDeployment,
	plan *modelDeploymentRetirementPlan,
) ([]*core.Pod, string) {
	shape, reason := modelDeploymentRetirementAnsweringShape(md, plan.Target.Members)
	if reason != "" {
		return nil, reason
	}

	return modelDeploymentAnsweringMembers(shape, plan.Target.Members)
}

// modelDeploymentRetirementDrainTarget is the one read the protocol asks about a member.
//
// IT IS BUILT HERE RATHER THAN INLINE because every field of it is a decision the reader is not
// allowed to make for itself. The container is carried rather than looked up, which is what the
// target's own comment requires: a reader handed a member and a container answers for exactly that
// pair and cannot be quietly pointed at a sibling container of the same Pod. A target built with it
// empty is a type that lies about itself, and nothing downstream fails loudly on it.
func modelDeploymentRetirementDrainTarget(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	plan *modelDeploymentRetirementPlan, member *core.Pod,
) modelDeploymentDrainTarget {
	return modelDeploymentDrainTarget{
		PodUID: member.UID,
		// The engine container is the one the renderer names, so the read is aimed at the engine
		// this replica runs rather than at whatever container happens to be first.
		Container: modelDeploymentMainContainerName,
		Role:      modelDeploymentRetirementRoleName(role, plan),
		Engine:    modelDeploymentRetirementEngine(md),
		// The operation's own deployment identity, so a live Pod that no longer belongs to it is a
		// member this operation never held rather than one it may read.
		DeploymentUID: md.UID,
	}
}

// modelDeploymentDrainMemberIdle is one member's half of the drain rule, kept apart from the loop
// so that every way a read fails to be an idle observation is a branch with a reason attached
// rather than a condition folded into one boolean.
func modelDeploymentDrainMemberIdle(
	member *core.Pod, target modelDeploymentDrainTarget, answer modelDeploymentDrainAnswer,
) (string, bool) {
	switch answer.State {
	case modelDeploymentDrainUnsupported:
		return "member " + member.Name + " reports no in-flight measurement: " + answer.Reason, false
	case modelDeploymentDrainUnknown:
		return "member " + member.Name + " is unobserved: " + answer.Reason, false
	case modelDeploymentDrainBusy:
		return "member " + member.Name + " still holds " +
			modelDeploymentDrainSeriesTotal(answer.Series), false
	}

	// A reader may claim completeness while having scraped two of the four SGLang gauges, and summing
	// what it did find is exactly the naive read this protocol exists to refuse, so the expected
	// series are checked here against the one list in the tree rather than trusted from the reader.
	if !answer.Complete {
		return "member " + member.Name + " returned an incomplete envelope", false
	}
	if missing := modelDeploymentDrainMissingSeries(target.Engine, answer.Series); len(missing) > 0 {
		return "member " + member.Name + " is missing " + strings.Join(missing, ", "), false
	}

	// A READER IS NOT TRUSTED WITH ITS OWN VERDICT. It is an interface, and a custom or forged one can
	// claim Idle over any numbers it likes; the protocol decides what activity means rather than
	// asking whether the reader agrees. A value that is not a count, or a count below zero, is not
	// evidence of anything, and an Idle claim resting on one is refused.
	if invalid := modelDeploymentDrainUnusableSeries(target.Engine, answer.Series); len(invalid) > 0 {
		return "member " + member.Name + " reported unusable activity in " +
			strings.Join(invalid, ", "), false
	}

	// IDLE MEANS ZERO, and it means zero on every expected gauge rather than on their sum. A sum
	// cannot be zero for a reader that never looked at one of them, and the sum of nonneg counts is
	// zero only when each is, so the check is made per gauge and the sum is never consulted.
	if busy := modelDeploymentDrainActiveSeries(target.Engine, answer.Series); len(busy) > 0 {
		return "member " + member.Name + " still holds activity in " + strings.Join(busy, ", "), false
	}

	return "", true
}

// modelDeploymentDrainActivityValue reports whether a gauge value can be read as activity at all.
//
// A count of work in flight is a nonnegative finite number, and anything else is an answer this
// protocol cannot use: NaN compares false against every bound, so it slips past a zero test while
// being no measurement of anything, and an infinity or a negative count is a signed row that says
// more about the scrape than about the queue. The parser and the protocol share this one predicate
// so a value refused at the envelope is refused identically where it is judged.
func modelDeploymentDrainActivityValue(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// modelDeploymentDrainUnusableSeries names the expected gauges whose values are not activity.
func modelDeploymentDrainUnusableSeries(engine string, series map[string]float64) []string {
	expected, known := modelDeploymentInFlightMetrics[engine]
	if !known {
		return []string{"a measurement for engine " + engine}
	}

	unusable := make([]string, 0, len(expected))
	for _, metric := range expected {
		if value, present := series[metric]; present && !modelDeploymentDrainActivityValue(value) {
			unusable = append(unusable, metric)
		}
	}
	sort.Strings(unusable)

	return unusable
}

// modelDeploymentDrainActiveSeries names the expected gauges that are not exactly zero.
func modelDeploymentDrainActiveSeries(engine string, series map[string]float64) []string {
	expected, known := modelDeploymentInFlightMetrics[engine]
	if !known {
		return []string{"a measurement for engine " + engine}
	}

	active := make([]string, 0, len(expected))
	for _, metric := range expected {
		if value := series[metric]; value != 0 {
			active = append(active, metric)
		}
	}
	sort.Strings(active)

	return active
}

// modelDeploymentDrainMissingSeries names the expected gauges a read did not carry.
//
// THE EXPECTED LIST IS READ FROM THE ONE MAP IN THE TREE rather than declared here. A second list
// is a second answer to "which gauges count as in flight", and the collector written against this
// seam would be free to disagree with the protocol about it without either noticing.
func modelDeploymentDrainMissingSeries(engine string, series map[string]float64) []string {
	expected, known := modelDeploymentInFlightMetrics[engine]
	if !known {
		// An engine with no expected list has no measurement to compare against, and inventing one
		// here is precisely the assumption the Unsupported answer exists to refuse.
		return []string{"a measurement for engine " + engine}
	}

	missing := make([]string, 0, len(expected))
	for _, metric := range expected {
		if _, present := series[metric]; !present {
			missing = append(missing, metric)
		}
	}

	return missing
}

// modelDeploymentDrainSeriesTotal sums what a read observed, for the reason a held member reports.
func modelDeploymentDrainSeriesTotal(series map[string]float64) string {
	total := 0.0
	for _, value := range series {
		total += value
	}

	return fmt.Sprintf("%.0f", total) + " in flight"
}

// modelDeploymentRetirementEngine is the engine whose gauges describe the target's work.
//
// The default is vLLM because a role that names no engine is rendered as one, and a drain target
// with an engine no gauge list exists for would be silently unmeasured -- which is the one answer
// this protocol must never reach by accident.
func modelDeploymentRetirementEngine(md *workercore.ModelDeployment) string {
	// THE ENGINE IS THE DEPLOYMENT'S, not the role's, and that is what makes a departed role
	// retirable. Removing a role from the spec does not change what engine the deployment runs, so
	// the gauge list a drained replica is compared against is still known after the spec has
	// forgotten the role that replica belonged to.
	if md.Spec.Engine.Name == "" {
		return workercore.ModelDeploymentEngineVLLM
	}

	return md.Spec.Engine.Name
}

// modelDeploymentRetirementRoleName is the role the target belonged to, whether or not the spec
// still declares it. The reservation carries the name, so the reservation alone answers it.
func modelDeploymentRetirementRoleName(
	role *workercore.ModelDeploymentRole, plan *modelDeploymentRetirementPlan,
) string {
	if role != nil {
		return role.Name
	}

	return plan.Reservation.RoleName
}

// modelDeploymentRetirementRole resolves the role the target belongs to, or nil when the spec no
// longer names it.
func modelDeploymentRetirementRole(
	md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) *workercore.ModelDeploymentRole {
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].Name == plan.Reservation.RoleName {
			return &md.Spec.Roles[i]
		}
	}

	return nil
}

// adoptsModelDeploymentRetirement reports whether an aborted operation has been overtaken by the
// spec naming the same replica again.
//
// ADOPTION NEEDS ALL THREE, and each half rules out a different wrong answer. The spec must declare
// the ordinal, or there is no intent to adopt. The live members must carry the reservation's UIDs,
// or the slot holds a different group and absorbing it would cancel a reservation about replicas
// that are still there. And the group must QUALIFY, or the reservation was aborting a group that
// cannot serve anyway and canceling it would let the ordinary path replace a broken replica
// through a rollout rather than through a retirement.
//
// It is therefore a cancellation and never a duplicate: the Pods are already there, they are
// healthy, and the next pass finds the replica current and creates nothing.
func (r *ModelDeploymentReconciler) adoptsModelDeploymentRetirement(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) bool {
	if plan.Reservation.State != workercore.ModelDeploymentRetirementStateAborted ||
		plan.held.Len() == 0 {
		return false
	}

	role := modelDeploymentRetirementRole(md, plan)
	if role == nil || int(role.Replicas) <= int(plan.Reservation.ReplicaOrdinal) {
		return false
	}

	members := make([]core.Pod, 0, len(plan.Target.Members))
	for _, member := range plan.Target.Members {
		members = append(members, *member)
	}
	qualifications := qualifyModelDeploymentInstances(
		ctx, md, members, modelDeploymentPendingReplacement{}, defaultGroupForwardFetch,
	)
	for _, qualification := range qualifications {
		if qualification.View.Role == plan.Reservation.RoleName &&
			qualification.View.Ordinal == int(plan.Reservation.ReplicaOrdinal) {
			return modelDeploymentRetirementAdoptable(qualification)
		}
	}

	return false
}

// modelDeploymentRetirementAdoptable reports whether a retained replica qualifies on every ground
// EXCEPT the reservation naming it.
//
// IT CANNOT ASK THE PREDICATE'S OWN ANSWER, and that is the whole reason this function exists rather
// than a call to Eligible. The NoPlannedRetirement leg is a definite failure for exactly the
// replica a reservation names -- that is what the leg is for -- so a reserved replica is
// unconditionally ineligible and asking would make adoption unreachable. A reservation would then
// never be canceled by a scale-up that names the ordinal again, and the held group would occupy
// its ordinal against a spec that wants it, which is the deadlock adoption exists to prevent.
//
// Asking about the other legs instead is the honest form of the question. The spec names the
// ordinal, the group is healthy on every ground that is not the reservation, and canceling is
// therefore taking the reservation's own objection away rather than ignoring a health fault.
func modelDeploymentRetirementAdoptable(qualification modelDeploymentInstanceQualification) bool {
	for _, leg := range qualification.Legs {
		if leg.Name == modelDeploymentLegNoPlannedRetirement {
			continue
		}
		if leg.Verdict != modelDeploymentLegVerified {
			return false
		}
	}

	return true
}

// consumeModelDeploymentRetry acts on a retry directive, and its ORDER is the whole contract.
//
//  1. A token equal to the one already consumed is a REPLAY. The annotation is cleared and nothing
//     else changes, which is what makes a directive that was delivered twice cost nothing.
//  2. Otherwise the token and the transition back to Admitted are PERSISTED TOGETHER, as a status
//     update that has actually reached the API server.
//  3. Only then is the annotation cleared.
//
// A crash between 2 and 3 leaves the token CONSUMED in the reservation with the annotation still
// present, and the next pass takes branch 1 and clears it: a no-op rather than a second retry. A
// crash before 2 leaves the token UNCONSUMED, and it may be consumed once. Reordering the two
// writes would make that window a LOST directive, which is the one outcome the ordering exists to
// make impossible.
//
// THE ROLE, ORDINAL AND UIDS MUST ALL MATCH, and each refusal is a different one. A malformed value
// is left exactly as it was found: a directive half-consumed is worse than a directive ignored. A
// token naming a different replica is left for whoever put it there, because consuming it would
// resume an operation about something else. A token naming a target that no longer resolves is
// external termination, which the state machine reports on the next pass.
func (r *ModelDeploymentReconciler) consumeModelDeploymentRetry(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) bool {
	logger := ctrllog.FromContext(ctx)

	directive, present := md.Annotations[modelDeploymentRetirementRetryAnnotation]
	if !present {
		return false
	}
	roleName, ordinal, token, ok := parseModelDeploymentRetirementRetry(directive)
	if !ok {
		logger.Info("refusing a malformed retirement retry directive", "directive", directive)

		return false
	}
	if roleName != plan.Reservation.RoleName || ordinal != int(plan.Reservation.ReplicaOrdinal) {
		logger.Info("refusing a retirement retry directive that names another replica",
			"directive", directive)

		return false
	}

	// THE REPLAY NO-OP, AND IT IS CHECKED BEFORE THE STATE ON PURPOSE. A token equal to the one
	// already consumed has been applied whatever the operation has done since, so clearing the
	// annotation is the only thing left to be done about it -- and it has to be reachable in every
	// state, because the crash window this branch exists for leaves the annotation behind on a
	// reservation that has already moved on to Admitted. Gating the check on the operation still
	// being aborted would leave that annotation on the object forever, read by nobody, cleared by
	// nobody: the one ordering the annotation's own contract has to survive.
	if plan.Reservation.LastConsumedRetryToken == token {
		if err := r.clearModelDeploymentRetirementRetry(ctx, md); err != nil {
			logger.Error(err, "clear a consumed retirement retry directive")
		}

		return false
	}

	// AN UNCONSUMED DIRECTIVE IS REFUSED UNLESS THE OPERATION IS ABORTED. There is nothing to retry
	// otherwise, and consuming it would resume a step that never finished. Leaving it alone is the
	// honest handling: it may be a directive for an operation that has not reached its abort yet.
	if plan.Reservation.State != workercore.ModelDeploymentRetirementStateAborted {
		return false
	}

	now := r.modelDeploymentNow()
	reservation := plan.Reservation.DeepCopy()
	reservation.LastConsumedRetryToken = token
	reservation.State = workercore.ModelDeploymentRetirementStateAdmitted
	reservation.Reason = "retry directive accepted"
	reservation.StartedAt = meta.NewTime(now)
	reservation.Deadline = meta.NewTime(now.Add(modelDeploymentRetirementOverallBudget))
	reservation.PhaseStartedAt = meta.NewTime(now)

	// THE PERSISTENCE THAT MATTERS. This write is what makes the token consumed; the annotation
	// clear below is only tidying that happens once it is true.
	if err := r.writeModelDeploymentRetirement(ctx, md, md.Status.Retirement, reservation); err != nil {
		logger.Error(err, "persist a consumed retirement retry token")

		return false
	}

	if err := r.clearModelDeploymentRetirementRetry(ctx, md); err != nil {
		logger.Error(err, "clear a consumed retirement retry directive")
	}

	plan.Reservation = reservation
	plan.Hold = true
	plan.InFlight = true
	plan.Deletes = nil
	plan.DeletesAllowed = false

	return true
}

// clearModelDeploymentRetirementRetry removes the directive annotation.
//
// It is a merge patch carrying the resourceVersion of the object as it was read, so a directive
// somebody else has just replaced is not deleted out from under them: the patch conflicts, the
// pass retries, and the replacement survives. A missing annotation is success, because the only
// outcome this function exists to produce is its absence.
func (r *ModelDeploymentReconciler) clearModelDeploymentRetirementRetry(
	ctx context.Context, md *workercore.ModelDeployment,
) error {
	if _, present := md.Annotations[modelDeploymentRetirementRetryAnnotation]; !present {
		return nil
	}

	patched := md.DeepCopy()
	delete(patched.Annotations, modelDeploymentRetirementRetryAnnotation)
	if err := r.Client.Patch(ctx, patched, ctrlcli.MergeFromWithOptions(
		md, ctrlcli.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("clear the retirement retry directive: %w", err)
	}
	md.Annotations = patched.Annotations

	return nil
}

// parseModelDeploymentRetirementRetry reads the directive's three parts.
//
// EXACTLY THREE, and no more forgiving shapes. The ordinal and the token are taken from the right
// and the role is whatever remains on the left, so a role name containing a colon still parses.
// Anything with fewer than three parts, an ordinal that is not a number, or an empty role or token
// is malformed and is refused rather than repaired.
func parseModelDeploymentRetirementRetry(directive string) (string, int, string, bool) {
	parts := strings.Split(directive, ":")
	if len(parts) != 3 {
		return "", 0, "", false
	}

	ordinal, err := strconvx.Atoi[int](parts[1])
	if err != nil || ordinal < 0 {
		return "", 0, "", false
	}
	if parts[0] == "" || parts[2] == "" {
		return "", 0, "", false
	}

	return parts[0], ordinal, parts[2], true
}

// errModelDeploymentReservationUnnameable is a target that cannot be identified, so no reservation
// can name it. It holds the removal rather than declining it, because the only way to be sure the
// removal is safe is to have an identity to check it against.
var errModelDeploymentReservationUnnameable = errors.New(
	"the target has no uid, so a reservation cannot name it")

// refuseModelDeploymentRemoval is the admission leg: it decides, at the moment a path is about to
// delete a replica it no longer wants, whether the retirement protocol may take that deletion over.
//
// IT ALWAYS HOLDS, and that is the whole of it. A removal intent either starts the protocol -- the
// first one, with nothing in flight -- or is refused because an operation already holds a replica,
// and in both cases the delete does not happen on this pass. The one thing it never does is delete.
//
// WHY THE PROTOCOL HAS TO OWN THE DELETION. A replica is removed by deleting its Pod and the
// Workload Kueue holds its finalizer behind. That is an instantaneous cut: the engine is signaled
// and the request it was serving is refused mid-flight, with no observation of what the target was
// actually doing. The protocol exists to spend a withdrawal and a drain before that cut, so a path
// that is about to delete must hand the decision to it first. Without this leg the reservation
// object has no writer, a 2-to-1 scale-down deletes immediately as it always did, and no accepted
// answer about what the target was serving can ever be reached.
//
// ONE OPERATION AT A TIME, AND A SECOND INTENT IS REFUSED RATHER THAN QUEUED. The wire holds one
// reservation per deployment, and two operations withdrawing at once would be two answers to one
// question with no way to merge them. So while a reservation is in flight every other removal holds,
// and it holds on a LATER PASS rather than being recorded: nothing is lost by waiting, because the
// removal intent is still there when the operation ends, and the pass after that admits it. That is
// what makes this level-based -- a pass that holds is not a pass that failed, and a removal blocked
// today is a removal that proceeds once the protocol is finished.
//
// A POD CLAIMING NO ORDINAL IS NOT ADMITTED, because the wire names its target by (role, ordinal)
// and a Pod belonging to no replica has none to name. It is also not held, because a hold with no
// reservation to eventually clear is a replica nothing will ever remove. The rollout is what removes
// it, which is where a Pod with no seat has always been removed from.
func (r *ModelDeploymentReconciler) refuseModelDeploymentRemoval(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
	pod *core.Pod, pods, members []core.Pod,
) bool {
	logger := ctrllog.FromContext(ctx)
	role := modelDeploymentPodRole(pod)
	ordinal, seated := modelDeploymentPodOrdinal(pod)

	// A reserved pod is refused before it is asked whether it is seated. The reservation names its
	// members by UID, and the ordinal label is mutable: strip it and the pod is no longer a replica
	// the wire can name, while the operation still holds that very pod. The unseated return below
	// would hand it to a path that deletes on the role. The check is narrow, and only a pod the
	// reservation actually holds is refused here.
	if plan.holds(pod.UID) {
		logger.V(3).Info("holding a removal of a pod the reservation already holds",
			"pod", pod.Name, "role", role, "ordinal", ordinal, "seated", seated,
			"state", plan.Reservation.State)

		return true
	}

	if !seated {
		return false
	}

	// A removal is refused while the protocol runs and while it retains. InFlight covers only the
	// first: an abort clears it while keeping the target, so a second intent would overwrite the
	// retained reservation. Hold is true exactly when something is retained, so both together
	// refuse a conflict without freezing the exits that legitimately replace the reservation.
	if plan.InFlight || plan.Hold {
		logger.V(3).Info("holding a removal while a retirement reservation is in flight or retained",
			"pod", pod.Name, "role", role, "ordinal", ordinal,
			"state", plan.Reservation.State, "retained", plan.Hold)

		return true
	}

	if err := r.admitModelDeploymentRetirement(ctx, md, plan, pod, pods, members); err != nil {
		// A reservation that could not be written is a hold, not a delete. The pass comes back
		// because the requeue that produces the retry is set on the plan either way, and a
		// deletion issued instead would be the cut this protocol exists to prevent.
		logger.Error(err, "admit a retirement reservation", "pod", pod.Name)

		return true
	}

	return true
}

// admitModelDeploymentRetirement writes the reservation for a replica a removal path wants gone.
//
// THE MEMBER SET IS THE CALLER'S TO NARROW, and that is the difference between a retirement and a
// shed. An ordinal the spec no longer declares is retired whole, so the members are the replica's.
// The surplus rule's decision is finer: it sheds one MEMBER of an ordinal that is still wanted, and
// binding the whole ordinal there would delete the member the current render still describes --
// which is precisely the member the surplus rule exists to keep. A caller that knows a narrower set
// passes it, and the wire's member UIDs are what express it.
//
// IT BINDS THE REPLICA AS IT IS NOW, not as the spec describes it. The member UIDs are what a later
// delete's precondition is checked against, and the Workload UID is what releases the finalizer
// holding each member, so both are read at admission rather than recomputed later: a slot refilled
// between admission and the delete would otherwise be deleted in the reserved member's place.
//
// THE PASS THAT ADMITS DELETES NOTHING, which is the caller's half: this function returns, the
// caller holds, and because the plan now carries a reservation every later removal on the same pass
// is refused by the branch above.
func (r *ModelDeploymentReconciler) admitModelDeploymentRetirement(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
	pod *core.Pod, pods, members []core.Pod,
) error {
	role := modelDeploymentPodRole(pod)
	ordinal, _ := modelDeploymentPodOrdinal(pod)
	now := r.modelDeploymentNow()

	if members == nil {
		members = modelDeploymentRetirementReplicaMembers(pods, role, ordinal)
	}
	workloadUID := ""
	// A replica short of its declared total composes no Workload at all, which is the same state
	// every other reader of the group's Workload already tolerates as absence. Binding the empty
	// string rather than inventing one is what keeps admission from failing on a group Kueue has
	// not composed yet.
	if len(members) > 0 {
		workloads, err := r.findModelDeploymentGroupWorkloads(ctx, md, members)
		if err != nil {
			return err
		}
		if len(workloads) > 0 {
			workloadUID = string(workloads[0].UID)
		}
	}

	// A MEMBER WITH NO UID CANNOT BE NAMED, and a reservation that binds the empty string names
	// EVERY POD IN THE DEPLOYMENT: the target is resolved by UID, so an unnamed target resolves to
	// the whole list and the protocol deletes all of it. A cluster always assigns a UID before this
	// controller ever sees a Pod, so this is a fixture and a defensive check rather than a branch
	// production takes -- which is exactly why the branch HOLDS rather than falls through to the
	// deletion. Failing open here would delete a deployment over a missing identity.
	uids := make([]string, 0, len(members))
	for _, member := range members {
		if member.UID == "" {
			return errModelDeploymentReservationUnnameable
		}
		uids = append(uids, string(member.UID))
	}
	slices.Sort(uids)

	reservation := &workercore.ModelDeploymentRetirementStatus{
		RoleName:           role,
		ReplicaOrdinal:     int32(ordinal),
		ObservedGeneration: md.Generation,
		TargetMemberUIDs:   uids,
		TargetWorkloadUID:  workloadUID,
		State:              workercore.ModelDeploymentRetirementStateAdmitted,
		Reason:             "admitted for a replica this pass no longer declares",
		StartedAt:          meta.NewTime(now),
		Deadline:           meta.NewTime(now.Add(modelDeploymentRetirementOverallBudget)),
		PhaseStartedAt:     meta.NewTime(now),
	}

	if err := r.writeModelDeploymentRetirement(ctx, md, md.Status.Retirement, reservation); err != nil {
		return fmt.Errorf("persist an admitted retirement reservation: %w", err)
	}

	plan.Reservation = reservation
	plan.Hold = true
	plan.InFlight = true
	plan.Target = modelDeploymentReplicaView{Role: role, Ordinal: ordinal, Seated: true}
	plan.held = sets.New[types.UID]()
	for _, member := range members {
		plan.held.Insert(member.UID)
		plan.Target.Members = append(plan.Target.Members, &member)
	}

	return nil
}

// modelDeploymentRetirementReplicaMembers are the live Pods seated on one replica, resolved through
// the tree's own grouping so that admission and the protocol's later target resolution cannot
// disagree about what one replica is.
func modelDeploymentRetirementReplicaMembers(
	pods []core.Pod, role string, ordinal int,
) []core.Pod {
	members := make([]core.Pod, 0, len(pods))
	for _, view := range modelDeploymentGroupPodsByReplica(pods) {
		if view.Role != role || !view.Seated || view.Ordinal != ordinal {
			continue
		}
		for _, member := range view.Members {
			members = append(members, *member)
		}
	}

	return members
}

// modelDeploymentShedMembers is a shed set in the value form admission binds. The shed is a list
// of Pod pointers because the surplus rule works in pointers, and a reservation binds values, so
// the conversion is done once here rather than at each call site.
func modelDeploymentShedMembers(shed []*core.Pod) []core.Pod {
	members := make([]core.Pod, 0, len(shed))
	for _, pod := range shed {
		members = append(members, *pod)
	}

	return members
}

// modelDeploymentRetirementShedSeat reports whether a shed set has a replica the protocol can name,
// and returns its first member's ordinal. A shed can be empty even where the count is over -- the
// pick is by slot and by what the current render describes, and a set it cannot separate yields
// nothing -- and an empty set is not a removal to hand over.
func modelDeploymentRetirementShedSeat(shed []*core.Pod) (int, bool) {
	if len(shed) == 0 {
		return 0, false
	}

	return modelDeploymentPodOrdinal(shed[0])
}
