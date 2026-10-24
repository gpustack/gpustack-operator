// Whole-group health qualification: the predicate that decides whether a replica's endpoints may
// be selected, at instance granularity rather than per member.
//
// A replica is admitted to selection only when the WHOLE group qualifies. A single member's
// readiness is not a statement about the group: a member that is up while a peer is down serves
// nothing, because the group is one forward path. This file evaluates that path as five named
// legs and keeps three-valued answers, because the two non-passing answers have to behave
// differently and a boolean would merge them into one.
//
// THE THREE VERDICTS ARE THREE DIFFERENT INSTRUCTIONS. A FAILED leg is a definite health fault
// and may revoke immediately. An UNKNOWN leg is an observation this operator cannot make, and it
// HOLDS: it never revokes on its own and it never restores. A VERIFIED leg has been evaluated
// and passed. Collapsing FAILED and UNKNOWN would either blackhole a healthy deployment on a
// missing observation or grant eligibility the spec forbids granting on a weaker signal.
//
// THE GROUP-FORWARD LEG IS NOW OBSERVABLE FOR SOME SHAPES, and where it is not, the file says
// which obstacle is in the way rather than that observation is impossible. A non-disaggregated
// multi-rank replica is verified by a one-token request bound to this operator's own identifier,
// which is described in the probe file. A single-member replica has no cross-member path to verify
// and is not applicable. A disaggregated replica stays unsupported, on each engine for its own
// measured reason, because on both of them one request cannot reach across the roles.
package worker

import (
	"context"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// modelDeploymentLegVerdict is one leg's answer.
type modelDeploymentLegVerdict int

const (
	// modelDeploymentLegVerified is a leg that was evaluated and passed.
	modelDeploymentLegVerified modelDeploymentLegVerdict = iota

	// modelDeploymentLegFailed is a definite health fault. It revokes, and it revokes whether or
	// not the group predicate is otherwise verifiable, because a group known to be broken needs
	// no further evidence.
	modelDeploymentLegFailed

	// modelDeploymentLegUnknown is a leg this operator could not evaluate. It holds.
	modelDeploymentLegUnknown
)

// modelDeploymentQualificationLegName is one condition of the whole-group predicate. It is
// carried by name so a held instance can report WHICH leg is unverified, rather than that
// something is.
type modelDeploymentQualificationLegName string

const (
	modelDeploymentLegMemberSetComplete    modelDeploymentQualificationLegName = "MemberSetComplete"
	modelDeploymentLegMembersReady         modelDeploymentQualificationLegName = "MembersReady"
	modelDeploymentLegGroupForward         modelDeploymentQualificationLegName = "GroupForward"
	modelDeploymentLegNoPlannedRetirement  modelDeploymentQualificationLegName = "NoPlannedRetirement"
	modelDeploymentLegNoPendingReplacement modelDeploymentQualificationLegName = "NoPendingReplacement"
)

// modelDeploymentQualificationLeg is one named condition and its answer.
type modelDeploymentQualificationLeg struct {
	Name    modelDeploymentQualificationLegName
	Verdict modelDeploymentLegVerdict
	Reason  string
}

// modelDeploymentMembershipGeneration names exactly which Pods a verification was taken against.
//
// IT IS DERIVED AND NEVER STORED. A generation that had to be persisted would need a writer, a
// place to keep it, and an ordering rule, and every one of those is a way for a restart or a
// missed write to leave a verification standing against a group that no longer exists. Deriving
// it from the member UID set makes that impossible: a replaced member is a different set, so the
// prior verification simply does not apply to the new one.
//
// IT IS ORDER-INDEPENDENT, which a raw concatenation would not be. The member list comes from a
// Pod listing, and a listing that returns the same Pods in a different order describes the same
// group; an order-sensitive generation would read that as a new membership and hold a healthy
// deployment with nothing to explain why.
type modelDeploymentMembershipGeneration string

// modelDeploymentGenerationOf derives the generation of a replica's current membership.
func modelDeploymentGenerationOf(view modelDeploymentReplicaView) modelDeploymentMembershipGeneration {
	uids := make([]string, 0, len(view.Members))
	for _, member := range view.Members {
		uids = append(uids, string(member.UID))
	}
	slices.Sort(uids)

	return modelDeploymentMembershipGeneration(strings.Join(uids, "/"))
}

// modelDeploymentPendingReplacement records which ordinals of which roles this pass has already
// condemned as outdated, so the qualification can refuse to restore a replica that is on its way
// out. It is read from the rollout the same pass already computed, never recomputed from the
// render, because the two answers to "is this replica being replaced" have to be one answer.
type modelDeploymentPendingReplacement struct {
	// ordinals is keyed by role name and then by replica ordinal.
	ordinals map[string]map[int]bool
}

// modelDeploymentPending reports whether the named replica is condemned for replacement.
func (p modelDeploymentPendingReplacement) modelDeploymentPending(role string, ordinal int) bool {
	if p.ordinals == nil {
		return false
	}

	return p.ordinals[role][ordinal]
}

// modelDeploymentGroupForwardState is what an engine-level group-forward observation can say.
// The two states this operator produces today are NotApplicable and Unsupported; the middle ones
// exist so a future observation has somewhere honest to land.
type modelDeploymentGroupForwardState string

const (
	// modelDeploymentGroupForwardNotApplicable is a replica with no cross-member path at all, so
	// there is nothing to observe. It is a different answer from Unsupported, and conflating
	// them would hold every ordinary single-member deployment forever.
	modelDeploymentGroupForwardNotApplicable modelDeploymentGroupForwardState = "NotApplicable"

	modelDeploymentGroupForwardVerified modelDeploymentGroupForwardState = "Verified"

	modelDeploymentGroupForwardFailed modelDeploymentGroupForwardState = "Failed"

	// modelDeploymentGroupForwardUnknown is an observation that could not be made or was not
	// fresh enough to inform the answer.
	modelDeploymentGroupForwardUnknown modelDeploymentGroupForwardState = "Unknown"

	// modelDeploymentGroupForwardUnsupported is a shape no engine-level group-forward
	// observation exists for.
	modelDeploymentGroupForwardUnsupported modelDeploymentGroupForwardState = "Unsupported"
)

// modelDeploymentGroupForward is one replica's group-forward evidence.
type modelDeploymentGroupForward struct {
	State       modelDeploymentGroupForwardState
	Reason      string
	Generation  modelDeploymentMembershipGeneration
	Observation string
}

// modelDeploymentInstanceQualification is one replica's whole-group answer.
type modelDeploymentInstanceQualification struct {
	// View is the replica this qualifies, so the caller can reach the members it covers.
	View modelDeploymentReplicaView
	// Generation is the membership the legs below were evaluated against.
	Generation modelDeploymentMembershipGeneration
	// GroupForward is kept whole rather than flattened into a leg, because activation is
	// decided from it and the status reports which state produced the hold.
	GroupForward modelDeploymentGroupForward
	// PairAttempt is the directed pair attempt that verified this replica, when one did. It is
	// retained so a reader can tell a proxy-shape answer from a rank-covering one without
	// re-deriving it, and so the requests the answer was bound to can be read.
	PairAttempt *modelDeploymentGroupForwardPairAttempt
	// Observed is whether any member's readiness has actually been reported. A replica this
	// pass just created has not, and a deployment made entirely of those has made no eligibility
	// observation at all -- which is what the status already calls NotObserved, and which must not
	// be restated as a hold.
	Observed bool
	Legs     []modelDeploymentQualificationLeg
}

// Leg reads one named leg.
func (q modelDeploymentInstanceQualification) Leg(
	name modelDeploymentQualificationLegName,
) modelDeploymentQualificationLeg {
	for _, leg := range q.Legs {
		if leg.Name == name {
			return leg
		}
	}

	return modelDeploymentQualificationLeg{
		Name:    name,
		Verdict: modelDeploymentLegUnknown,
		Reason:  "the leg was not evaluated",
	}
}

// modelDeploymentGroupState is the state a group's qualification stands in.
//
// THE NAMES ARE THE ONES THE STATUS PUBLISHES, because this operator reports one state per group
// and a reader learns nothing from two vocabularies for one thing.
type modelDeploymentGroupState string

const (
	// modelDeploymentGroupStateEligible is a group every leg verified.
	modelDeploymentGroupStateEligible modelDeploymentGroupState = "Eligible"
	// modelDeploymentGroupStateWithdrawn is a group a definite fault withdrew.
	modelDeploymentGroupStateWithdrawn modelDeploymentGroupState = "Withdrawn"
	// modelDeploymentGroupStateAwaitingMembers is a group whose member set is not established.
	modelDeploymentGroupStateAwaitingMembers modelDeploymentGroupState = "AwaitingMembers"
	// modelDeploymentGroupStateAwaitingReady is a group whose members are not all Ready=True.
	modelDeploymentGroupStateAwaitingReady modelDeploymentGroupState = "AwaitingReady"
	// modelDeploymentGroupStateAwaitingProbe is a group whose forward path could not be observed.
	modelDeploymentGroupStateAwaitingProbe modelDeploymentGroupState = "AwaitingProbe"
	// modelDeploymentGroupStateUnsupported is a shape no observation exists for.
	modelDeploymentGroupStateUnsupported modelDeploymentGroupState = "Unsupported"
	// modelDeploymentGroupStateUnknown is a group held on a fact this operator cannot name.
	modelDeploymentGroupStateUnknown modelDeploymentGroupState = "Unknown"
)

// modelDeploymentQualificationLegOrder is the order the legs are evaluated in, and the order a held
// group reports its blocking leg in.
//
// THE ORDER IS THE EVALUATION ORDER, so the first leg that is not verified is the one that would
// have to change before the group could qualify.
var modelDeploymentQualificationLegOrder = []modelDeploymentQualificationLegName{
	modelDeploymentLegMemberSetComplete,
	modelDeploymentLegMembersReady,
	modelDeploymentLegGroupForward,
	modelDeploymentLegNoPlannedRetirement,
	modelDeploymentLegNoPendingReplacement,
}

// MembersReady reports whether every member of the group reports Ready=True.
//
// IT IS THE FAIL-CLOSED ANSWER TO THE MEMBER QUESTION. Ready=True is the only positive answer, so
// a member that has not reported its readiness, or has reported it as anything but True, makes the
// group unsafe. The leg itself stays three-valued, because a group that is still starting is HELD
// and not REVOKED, but the group is withdrawn either way.
func (q modelDeploymentInstanceQualification) MembersReady() bool {
	return q.Leg(modelDeploymentLegMembersReady).Verdict == modelDeploymentLegVerified
}

// GroupState classifies the group.
//
// THE BLOCKING LEG IS SELECTED ONCE, so the state and the reason can never disagree about what
// holds the group. The precedence reads a revocation above an unsupported shape above a hold, which
// is the precedence the status already publishes, so the classification here and the status there
// cannot disagree about which of two facts came first.
func (q modelDeploymentInstanceQualification) GroupState() modelDeploymentGroupState {
	if q.Eligible() {
		return modelDeploymentGroupStateEligible
	}
	if q.HasFailure() {
		return modelDeploymentGroupStateWithdrawn
	}
	if q.GroupForward.State == modelDeploymentGroupForwardUnsupported {
		return modelDeploymentGroupStateUnsupported
	}
	for _, name := range modelDeploymentQualificationLegOrder {
		if q.Leg(name).Verdict == modelDeploymentLegVerified {
			continue
		}
		switch name {
		case modelDeploymentLegMemberSetComplete:
			return modelDeploymentGroupStateAwaitingMembers
		case modelDeploymentLegMembersReady:
			return modelDeploymentGroupStateAwaitingReady
		case modelDeploymentLegGroupForward:
			return modelDeploymentGroupStateAwaitingProbe
		}

		// THE FIRST LEG THAT IS NOT VERIFIED NAMES THE STATE, so a group held on the retirement or
		// the replacement leg reports the state this operator has no finer name for.
		return modelDeploymentGroupStateUnknown
	}

	return modelDeploymentGroupStateUnknown
}

// Reason names the fact that holds the group, or says that it qualifies.
//
// IT NAMES THE SAME LEG THE STATE NAMES, so a reader is never told that a group is awaiting one
// thing and given the reason for another.
func (q modelDeploymentInstanceQualification) Reason() string {
	if q.Eligible() {
		// Proxy responses do not prove individual rank coverage or KV transfer.
		if q.PairAttempt != nil && q.PairAttempt.Shape == modelDeploymentGroupForwardPairShapeViaRoutingProxy {
			return "routing proxy returned the expected content with the probe request ID while naming " +
				"the prefiller; individual rank coverage and KV transfer remain unproven"
		}

		return "every leg of the group verified"
	}
	failed := q.HasFailure()
	for _, name := range modelDeploymentQualificationLegOrder {
		leg := q.Leg(name)
		if failed && leg.Verdict == modelDeploymentLegFailed {
			return leg.Reason
		}
	}
	if q.GroupForward.State == modelDeploymentGroupForwardUnsupported {
		return q.GroupForward.Reason
	}
	for _, name := range modelDeploymentQualificationLegOrder {
		if leg := q.Leg(name); leg.Verdict != modelDeploymentLegVerified {
			return leg.Reason
		}
	}

	return q.GroupForward.Reason
}

// HasFailure reports a definite health fault. It is the signal that revokes unconditionally and
// is deliberately NOT a question about whether the group predicate is available: a group known
// to be broken does not also need to be verifiable.
func (q modelDeploymentInstanceQualification) HasFailure() bool {
	for _, leg := range q.Legs {
		if leg.Verdict == modelDeploymentLegFailed {
			return true
		}
	}

	return false
}

// Eligible is the one question the eligibility write asks. Any UNKNOWN leg holds, so a held
// instance can never be restored by a leg that was merely skipped.
func (q modelDeploymentInstanceQualification) Eligible() bool {
	for _, leg := range q.Legs {
		if leg.Verdict != modelDeploymentLegVerified {
			return false
		}
	}

	return true
}

// Conclusive reports whether the predicate reached a DEFINITE answer for this replica, as opposed
// to leaving a question open. It is what separates a revoked replica from a held one: a revoked
// replica is conclusively not eligible and stays that way, while a held one may still qualify once
// the missing observation lands.
//
// Only a conclusive replica may contribute to a published qualified count. Counting a held
// replica's current false as if it were final would publish a number the next pass has to take
// back, and would report a group as fully disqualified when it is really unobserved.
func (q modelDeploymentInstanceQualification) Conclusive() bool {
	for _, leg := range q.Legs {
		if leg.Verdict == modelDeploymentLegUnknown {
			return false
		}
	}

	return true
}

// HeldLegs names the legs that are not Verified, in a stable order, so the reported reason does
// not change between two passes over an unchanged group.
func (q modelDeploymentInstanceQualification) HeldLegs() []modelDeploymentQualificationLegName {
	held := make([]modelDeploymentQualificationLegName, 0, len(q.Legs))
	for _, leg := range q.Legs {
		if leg.Verdict != modelDeploymentLegVerified {
			held = append(held, leg.Name)
		}
	}
	slices.Sort(held)

	return held
}

// Activated reports whether this replica may have its eligibility RESTORED. Withdrawal is not
// gated on it and never is.
//
// THE GATE IS A POSITIVE ANSWER, NOT THE ABSENCE OF ONE. Restore runs when the group-forward leg is
// Verified, or when the replica is NotApplicable and so has no group to verify. An Unknown leg
// holds: the probe was made and could not be understood, or the member was never asked, and
// treating that as permission would restore an endpoint on no evidence at all — which is the whole
// failure this leg exists to prevent. Reading it as "not Unsupported" would have done exactly that
// the moment an observation became possible to fail.
//
// The gate exists because the group-forward leg is Unsupported for a shape no probe can reach. With
// the capability unavailable, the reconciler may not put an endpoint back into selection, because
// doing so would grant eligibility on a weaker signal than the spec requires. It may still take an
// endpoint out, because that is the direction the health faults all point.
func (q modelDeploymentInstanceQualification) Activated() bool {
	return q.GroupForward.State == modelDeploymentGroupForwardVerified ||
		q.GroupForward.State == modelDeploymentGroupForwardNotApplicable
}

// qualifyModelDeploymentInstances evaluates the whole-group predicate once per replica.
//
// THE CHEAP LEGS ARE EVALUATED FIRST AND THE OBSERVATION LAST, which is an ordering with a
// consequence rather than a preference. A replica that is short a member or holds an unready one
// is already condemned by facts the informer cache carries, so it never pays for a live
// observation, and more importantly its definite fault is never softened into a hold by an
// observation that happens to be missing.
//
// THE DIRECTED PAIR TRANSPORT IS OPTIONAL. A Prefill or a Decode group is served by a pair, and
// without that transport this operator has no observation of a pair at all, so such a group
// answers Unsupported rather than being asked a question it cannot answer. A caller that must see
// the pair answered has to wire the transport, and a caller that does not gets the answer the
// ordinary group's shape implies.
func qualifyModelDeploymentInstances(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
	pending modelDeploymentPendingReplacement, fetch modelDeploymentGroupForwardFetch,
	pairFetch ...modelDeploymentGroupForwardPairFetch,
) []modelDeploymentInstanceQualification {
	roles := make(map[string]*workercore.ModelDeploymentRole, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		roles[md.Spec.Roles[i].Name] = &md.Spec.Roles[i]
	}

	views := modelDeploymentGroupPodsByReplica(pods)
	locals := make([]modelDeploymentReplicaLocal, 0, len(views))
	for _, view := range views {
		role := roles[view.Role]
		if role == nil {
			// A replica of a role the spec no longer names is leaving; it qualifies for nothing
			// and reporting a held leg for it would be a reason nobody can act on.
			continue
		}

		locals = append(locals, newModelDeploymentReplicaLocal(view, role))
	}

	answers := modelDeploymentGroupForwardAnswers(
		ctx, md, locals, fetch, modelDeploymentPairTransport(pairFetch),
	)

	qualifications := make([]modelDeploymentInstanceQualification, 0, len(locals))
	for i, local := range locals {
		qualifications = append(qualifications, local.modelDeploymentQualification(
			md, pending, answers[i],
		))
	}

	return qualifications
}

// modelDeploymentPairTransport reads the directed pair transport out of the optional argument.
//
// AN ABSENT TRANSPORT IS AN UNWIRED CAPABILITY and not a license to ask the pair anyway: the
// caller that does not wire one has told this operator it has no way to observe a pair.
func modelDeploymentPairTransport(
	pairFetch []modelDeploymentGroupForwardPairFetch,
) modelDeploymentGroupForwardPairFetch {
	if len(pairFetch) == 0 {
		return nil
	}

	return pairFetch[0]
}

// modelDeploymentReplicaLocal is the part of one replica's answer that needs no observation.
//
// THE LOCAL LEGS ARE HERE AND NOT IN THE QUALIFICATION because the observation needs to know them
// before it asks anything: a pair can only be asked about two replicas that are both ready and
// complete, and a replica that is neither is never asked about at all.
type modelDeploymentReplicaLocal struct {
	// view is the replica these legs belong to.
	view modelDeploymentReplicaView
	// role is the role the replica belongs to.
	role *workercore.ModelDeploymentRole
	// membersComplete and membersReady are the two member legs.
	membersComplete modelDeploymentQualificationLeg
	membersReady    modelDeploymentQualificationLeg
}

// newModelDeploymentReplicaLocal decides the two member legs of one replica.
//
// COMPLETENESS IS MEASURED AGAINST THE GROUP TOTAL THE MEMBERS DECLARE, never the role's declared
// size, so a replica the role has been edited away from stays whole. A shape the members cannot
// establish holds; a replica short of its total still fails.
func newModelDeploymentReplicaLocal(
	view modelDeploymentReplicaView, role *workercore.ModelDeploymentRole,
) modelDeploymentReplicaLocal {
	shape := modelDeploymentDeployedReplicaShape(view)
	membersComplete := modelDeploymentQualificationLeg{
		Name:    modelDeploymentLegMemberSetComplete,
		Verdict: shape.modelDeploymentReplicaLegVerdict(),
		Reason:  modelDeploymentReplicaHoldsReason(view, shape),
	}

	unready, unobserved := 0, 0
	for _, member := range view.Members {
		switch modelDeploymentMemberReadiness(member) {
		case modelDeploymentLegVerified:
		case modelDeploymentLegFailed:
			unready++
		default:
			unobserved++
		}
	}
	membersReady := modelDeploymentQualificationLeg{
		Name:    modelDeploymentLegMembersReady,
		Verdict: modelDeploymentLegVerified,
	}
	switch {
	case unobserved == len(view.Members) && len(view.Members) > 0:
		// NOTHING HAS BEEN OBSERVED YET, which is not a fault. A replica this pass just created has
		// no PodReady condition at all, and reading that as "not ready" would condemn a healthy
		// deployment the instant it was born -- and would move the reported answer on the first pass
		// after the create, which is a write an unchanged spec should not issue.
		membersReady.Verdict = modelDeploymentLegUnknown
		membersReady.Reason = "no member's readiness has been observed yet"
	case unready == 0 && unobserved > 0:
		// ONE MEMBER'S READINESS IS NOT THE WHOLE GROUP'S. A replica with a ready member and a
		// sibling still inside its startup window has not made the observation this leg names,
		// so it holds: reading it as "every member reports ready" would grant eligibility on a
		// partial answer.
		membersReady.Verdict = modelDeploymentLegUnknown
		membersReady.Reason = strconvx.Itoa(unobserved) + " of " +
			strconvx.Itoa(len(view.Members)) + " members are still starting"
	case unready == 0:
		membersReady.Reason = "every member reports ready"
	default:
		membersReady.Verdict = modelDeploymentLegFailed
		membersReady.Reason = strconvx.Itoa(unready) + " of " +
			strconvx.Itoa(len(view.Members)) + " members report not ready"
	}

	return modelDeploymentReplicaLocal{
		view:            view,
		role:            role,
		membersComplete: membersComplete,
		membersReady:    membersReady,
	}
}

// localReady reports whether the replica is complete and every member of it reports Ready=True.
//
// IT IS THE CONDITION A PAIR IS ASKED UNDER, and it is stricter than the ordinary group's: a pair
// speaks for two replicas at once, so a replica that is not serving must not be one half of it.
func (l modelDeploymentReplicaLocal) localReady() bool {
	return l.membersComplete.Verdict == modelDeploymentLegVerified &&
		l.membersReady.Verdict == modelDeploymentLegVerified
}

// localFault names the member leg that stopped the replica from being asked about.
func (l modelDeploymentReplicaLocal) localFault() string {
	if l.membersComplete.Verdict != modelDeploymentLegVerified {
		return l.membersComplete.Reason
	}

	return l.membersReady.Reason
}

// modelDeploymentQualification assembles one replica's whole-group answer from its local legs and
// its observation.
//
// THE LEG ORDER IS UNCHANGED, because the order the legs are read in is the order a reader of the
// status reads them in.
func (l modelDeploymentReplicaLocal) modelDeploymentQualification(
	md *workercore.ModelDeployment, pending modelDeploymentPendingReplacement,
	answer modelDeploymentGroupForwardAnswer,
) modelDeploymentInstanceQualification {
	qualification := modelDeploymentInstanceQualification{
		View:         l.view,
		Generation:   modelDeploymentGenerationOf(l.view),
		GroupForward: answer.forward,
		PairAttempt:  answer.attempt,
	}
	for _, member := range l.view.Members {
		if modelDeploymentMemberReadiness(member) != modelDeploymentLegUnknown {
			qualification.Observed = true

			break
		}
	}

	// COMPLETENESS IS NOT REDUNDANT WITH READINESS. A replica short of a declared member can
	// have every member it does hold reporting ready, while Kueue has composed no Workload for
	// it and it is admitted by nothing. It is absent with some Pods lying around, not partly
	// working.
	qualification.Legs = append(qualification.Legs, l.membersComplete)
	qualification.Legs = append(qualification.Legs, l.membersReady)

	qualification.Legs = append(qualification.Legs, modelDeploymentQualificationLeg{
		Name:    modelDeploymentLegGroupForward,
		Verdict: groupForwardVerdict(answer.forward.State),
		Reason:  answer.forward.Reason,
	})

	qualification.Legs = append(qualification.Legs, modelDeploymentQualificationLeg{
		Name:    modelDeploymentLegNoPlannedRetirement,
		Verdict: retirementVerdict(md, l.view),
		Reason:  retirementReason(md, l.view),
	})

	replacing := modelDeploymentQualificationLeg{
		Name:    modelDeploymentLegNoPendingReplacement,
		Verdict: modelDeploymentLegVerified,
		Reason:  "the replica is not condemned for replacement by this pass",
	}
	if l.view.Seated && pending.modelDeploymentPending(l.view.Role, l.view.Ordinal) {
		replacing.Verdict = modelDeploymentLegFailed
		replacing.Reason = "the replica is condemned for replacement by this pass"
	}
	qualification.Legs = append(qualification.Legs, replacing)

	return qualification
}

// modelDeploymentGroupForwardAnswer is one replica's observation and the evidence behind it.
type modelDeploymentGroupForwardAnswer struct {
	// forward is the observation itself.
	forward modelDeploymentGroupForward
	// attempt is the directed pair attempt that verified this replica, when a verified pair did.
	attempt *modelDeploymentGroupForwardPairAttempt
}

// modelDeploymentGroupForwardAnswers asks the observation for every replica.
//
// THE ORDINARY ROLES ARE ASKED ONE BY ONE, and the Prefill and the Decode roles are asked AS PAIRS,
// because neither half of a pair serves on its own: the prefiller computes the KV and the decoder
// consumes it, so one request that comes back with the copied answer is evidence about both.
func modelDeploymentGroupForwardAnswers(
	ctx context.Context, md *workercore.ModelDeployment,
	locals []modelDeploymentReplicaLocal,
	fetch modelDeploymentGroupForwardFetch,
	pairFetch modelDeploymentGroupForwardPairFetch,
) []modelDeploymentGroupForwardAnswer {
	answers := make([]modelDeploymentGroupForwardAnswer, len(locals))

	var prefills, decodes []int
	for i, local := range locals {
		switch ModelDeploymentEffectiveRoleKind(local.role) {
		case workercore.ModelDeploymentRoleKindPrefill:
			prefills = append(prefills, i)

			continue
		case workercore.ModelDeploymentRoleKindDecode:
			decodes = append(decodes, i)

			continue
		}

		// A REPLICA CONDEMNED BY A DEFINITE MEMBER LEG OWES NOTHING TO A LIVE PROBE: the request
		// would spend engine tokens on an answer that cannot soften the fault. The observation is
		// still ASKED, with no transport: answers that never needed the network (a one-member
		// replica is NotApplicable, an unverified shape is Unsupported) are unchanged, and only the
		// probe leg comes back as an absence of evidence.
		transport := fetch
		if local.membersComplete.Verdict == modelDeploymentLegFailed ||
			local.membersReady.Verdict == modelDeploymentLegFailed {
			transport = nil
		}
		answers[i] = modelDeploymentGroupForwardAnswer{
			forward: observeModelDeploymentGroupForward(ctx, md, local.role, local.view, transport),
		}
	}

	for i, answer := range modelDeploymentPairAnswers(ctx, md, locals, prefills, decodes, pairFetch) {
		answers[i] = answer
	}

	return answers
}

// modelDeploymentPairAnswers answers the Prefill and the Decode replicas of one deployment.
//
// THE CONFIGURATION IS DECIDED ONCE FOR BOTH ROLES, because a pair is a fact about the two roles
// together: a role whose route this operator does not know cannot be paired with anything, and
// neither can its opposite.
func modelDeploymentPairAnswers(
	ctx context.Context, md *workercore.ModelDeployment,
	locals []modelDeploymentReplicaLocal,
	prefills, decodes []int,
	fetch modelDeploymentGroupForwardPairFetch,
) map[int]modelDeploymentGroupForwardAnswer {
	answers := map[int]modelDeploymentGroupForwardAnswer{}
	if len(prefills) == 0 && len(decodes) == 0 {
		return answers
	}

	unsupported, unsupportedReason := modelDeploymentGroupForwardPairSupport(md)

	// THE CANDIDATES ARE THE ONES THAT MAY BE ASKED ABOUT. Every replica that is not a candidate
	// already has its answer: the shape has no observation of it, or the replica is not ready
	// enough to be asked about, or the transport is unwired.
	candidates := func(group []int) []int {
		ready := make([]int, 0, len(group))
		for _, i := range group {
			local := locals[i]
			switch {
			case unsupported:
				answers[i] = modelDeploymentGroupForwardAnswer{
					forward: modelDeploymentGroupForwardUnsupportedFor(unsupportedReason),
				}
			case !local.localReady():
				answers[i] = modelDeploymentGroupForwardAnswer{
					forward: modelDeploymentGroupForwardUnknownFor(
						"the group is not complete and ready, so the pair was not asked: " +
							local.localFault()),
				}
			case fetch == nil:
				answers[i] = modelDeploymentGroupForwardAnswer{
					forward: modelDeploymentGroupForwardUnsupportedFor(
						"no directed pair transport is wired, so this operator has no observation " +
							"of a prefill-decode pair"),
				}
			default:
				ready = append(ready, i)
			}
		}

		return ready
	}
	readyPrefills := candidates(prefills)
	readyDecodes := candidates(decodes)

	// A PAIR WITH NO PARTNER IS NOT A PAIR, so a replica with no ready counterpart holds with the
	// reason that names what is missing.
	if len(readyPrefills) == 0 || len(readyDecodes) == 0 {
		for _, i := range append(append([]int{}, readyPrefills...), readyDecodes...) {
			answers[i] = modelDeploymentGroupForwardAnswer{
				forward: modelDeploymentGroupForwardUnknownFor(
					"no ready replica of the other role is available to complete the pair"),
			}
		}

		return answers
	}

	// THE RECIPES ARE ASKED IN A FIXED ORDER, so one pass asks the same pairs in the same order
	// every time.
	slices.SortStableFunc(readyPrefills, func(a, b int) int {
		return locals[a].view.Ordinal - locals[b].view.Ordinal
	})
	slices.SortStableFunc(readyDecodes, func(a, b int) int {
		return locals[a].view.Ordinal - locals[b].view.Ordinal
	})

	verified := map[int]bool{}
	for _, prefill := range readyPrefills {
		for _, decode := range readyDecodes {
			if verified[prefill] && verified[decode] {
				// NEITHER SIDE STILL NEEDS THIS PAIR, so asking it again could not change either
				// answer and would spend engine tokens to learn nothing.
				continue
			}

			result := observeModelDeploymentGroupForwardPair(
				ctx, md, locals[prefill].view, locals[decode].view, fetch,
			)

			// THE TWO SIDES ARE FOLDED INDEPENDENTLY, because the driver answers each side on its
			// own account. A VERIFIED ANSWER IS NEVER DISPLACED by a later unsuccessful partner,
			// and the attempt that verified a replica is the evidence kept beside it.
			if !verified[prefill] {
				answers[prefill] = modelDeploymentGroupForwardAnswer{forward: result.Prefill}
			}
			if !verified[decode] {
				answers[decode] = modelDeploymentGroupForwardAnswer{forward: result.Decode}
			}
			if result.Prefill.State == modelDeploymentGroupForwardVerified {
				verified[prefill] = true
				answers[prefill] = modelDeploymentGroupForwardAnswer{
					forward: result.Prefill, attempt: &result.Attempt,
				}
			}
			if result.Decode.State == modelDeploymentGroupForwardVerified {
				verified[decode] = true
				answers[decode] = modelDeploymentGroupForwardAnswer{
					forward: result.Decode, attempt: &result.Attempt,
				}
			}
		}
	}

	return answers
}

// modelDeploymentGroupForwardPairSupport reports whether this operator has NO verified observation for
// the directed pair this deployment's shape implies, and why it has none.
//
// THE CHECKS ARE NARROW AND NAMED, because a general engine-support table would be a second place
// where engine differences are decided, and the pair driver already decides most of them.
//
// THE FIRST ANSWER MEANS UNSUPPORTED, so that the call site reads as the question it is.
func modelDeploymentGroupForwardPairSupport(
	md *workercore.ModelDeployment,
) (bool, string) {
	// THE ENGINE IS THE PAIR DRIVER'S OWN FACT, and the driver states it the same way.
	if _, known := modelDeploymentGroupForwardPairEngines[md.Spec.Engine.Name]; !known {
		return true, "engine " + md.Spec.Engine.Name +
			" has no group-forward observation this operator has verified"
	}

	// A ROLE THAT TOOK OVER ITS COMMAND LINE SERVES A ROUTE THIS OPERATOR DOES NOT KNOW, and a
	// pair cannot be asked at a route nobody knows. The check covers BOTH halves of the pair,
	// because a half that cannot be paired with is not made pairable by a partner that can.
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		switch ModelDeploymentEffectiveRoleKind(role) {
		case workercore.ModelDeploymentRoleKindPrefill, workercore.ModelDeploymentRoleKindDecode:
		default:
			continue
		}
		if len(role.Command) > 0 {
			return true, "role " + role.Name +
				" took over its command line, so the route its pair would be asked at is one this " +
				"operator does not know"
		}
	}

	// THE VLLM PAIR IS OBSERVABLE ONLY THROUGH THE ROUTING PROXY, because the connector's
	// handshake names the prefiller by an engine id that comes out of the prefiller's own transfer
	// registry, and this operator does not read that registry.
	if md.Spec.Engine.Name == workercore.ModelDeploymentEngineVLLM {
		if decodeRole := modelDeploymentGroupForwardPairDecodeRole(md); decodeRole != nil &&
			!ModelDeploymentRoleFrontedByProxy(md, decodeRole) {
			return true, "the prefiller's engine id is held in the prefiller's own transfer registry " +
				"and this operator does not read that registry, so a vLLM pair that no routing " +
				"proxy drives cannot be observed"
		}
	}

	return false, ""
}

// modelDeploymentMemberReadiness reads one member's own readiness as three answers rather than
// the two podIsReady returns.
//
// THE ABSENCE OF A CONDITION IS NOT A NEGATIVE ANSWER. A Pod that has just been created carries no
// PodReady condition at all, and podIsReady calls that not-ready. For a question about whether a
// group is healthy that is the wrong reading twice over: it condemns a deployment for existing,
// and it makes the answer move on the pass after the create, which is a status write an unchanged
// spec has no reason to issue. A condition that is present and False is a real observation and is
// reported as one.
//
// AN UNKNOWN CONDITION IS ABSENCE OF EVIDENCE TOO. Kubernetes legitimately writes PodReady=Unknown
// (a kubelet that is restarting, a node gone quiet, an eviction in progress), and at that moment
// nothing is known about the member's health; reading it as Failed would report a definite fault
// the cluster never observed. Only False is the definite answer.
//
// A CONTAINER STILL IN ITS STARTUP WINDOW IS NOT A FAULT EITHER. A running container kubelet has
// not marked Started is one whose startupProbe has not yet succeeded, which is the state a large
// model can sit in for a long time while it compiles graphs; reading it as Failed would revoke a
// replica for starting slowly and restart the very compilation the startup budget exists to
// protect. A container stuck in a terminal waiting reason is the opposite: a definite fault
// reported as one even before any readiness answer exists, because a group known to be broken
// needs no further evidence.
func modelDeploymentMemberReadiness(pod *core.Pod) modelDeploymentLegVerdict {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == core.PodReady && condition.Status == core.ConditionTrue {
			return modelDeploymentLegVerified
		}
	}

	for _, statuses := range [][]core.ContainerStatus{
		pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses,
	} {
		for _, status := range statuses {
			if status.State.Waiting != nil &&
				isTerminalWaitReason(status.State.Waiting.Reason) {
				return modelDeploymentLegFailed
			}
		}
	}

	if pod.Status.Phase == core.PodRunning {
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Running != nil && (status.Started == nil || !*status.Started) {
				return modelDeploymentLegUnknown
			}
		}
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == core.PodReady {
			if condition.Status == core.ConditionFalse {
				return modelDeploymentLegFailed
			}

			return modelDeploymentLegUnknown
		}
	}

	return modelDeploymentLegUnknown
}

// groupForwardVerdict maps the observation's own state onto the leg's verdict. An Unsupported
// observation is UNKNOWN rather than FAILED: nothing is known to be wrong with the group, and
// reporting Failed would claim a health fault this operator cannot see.
func groupForwardVerdict(state modelDeploymentGroupForwardState) modelDeploymentLegVerdict {
	switch state {
	case modelDeploymentGroupForwardNotApplicable, modelDeploymentGroupForwardVerified:
		return modelDeploymentLegVerified
	case modelDeploymentGroupForwardFailed:
		return modelDeploymentLegFailed
	default:
		return modelDeploymentLegUnknown
	}
}

// retirementVerdict reports whether a retirement is planned for this replica. A reservation is
// read here and NEVER written, and only to say that a planned departure is not a group fault:
// health revocation and member replacement revoke independently of any reservation, so nothing
// downstream of a reservation can keep an unhealthy group eligible.
func retirementVerdict(
	md *workercore.ModelDeployment, view modelDeploymentReplicaView,
) modelDeploymentLegVerdict {
	if retirementTargets(md, view) {
		return modelDeploymentLegFailed
	}

	return modelDeploymentLegVerified
}

// retirementReason names the reservation that claims this replica, or says there is none.
func retirementReason(
	md *workercore.ModelDeployment, view modelDeploymentReplicaView,
) string {
	if !retirementTargets(md, view) {
		return "no retirement reservation claims this replica"
	}

	return "a retirement reservation names this replica"
}

// retirementTargets reports whether the deployment's single reservation names this replica. A
// reservation in any state claims it, because a held or completed operation is still a decision
// about this replica rather than an absence of one.
func retirementTargets(
	md *workercore.ModelDeployment, view modelDeploymentReplicaView,
) bool {
	reservation := md.Status.Retirement
	if reservation == nil {
		return false
	}
	if reservation.RoleName != view.Role {
		return false
	}

	return view.Seated && int(reservation.ReplicaOrdinal) == view.Ordinal
}

// modelDeploymentQualificationsByMember indexes the qualifications by the UID of every member
// they cover, so the eligibility write, which runs per Pod, can reach its replica's answer
// without regrouping anything a second time.
func modelDeploymentQualificationsByMember(
	qualifications []modelDeploymentInstanceQualification,
) map[types.UID]modelDeploymentInstanceQualification {
	byMember := make(map[types.UID]modelDeploymentInstanceQualification)
	for _, qualification := range qualifications {
		for _, member := range qualification.View.Members {
			byMember[member.UID] = qualification
		}
	}

	return byMember
}

// modelDeploymentRoleQualified counts the replicas of each role whose whole group qualified.
//
// THE COUNT IS NIL WHENEVER ANY REPLICA OF THE ROLE IS INCONCLUSIVE, which is the distinction
// the wire makes between an unobserved list and an observed empty one. A role with three
// qualified replicas and one held does not report three eligible endpoints: it reports nothing it
// can stand behind, because a reader seeing three would conclude the role is qualified while a
// fourth replica is not known.
//
// A REPLICAS THAT IS CONCLUSIVELY NOT QUALIFIED STILL COUNTS. It is a definite answer to the
// question, so a role of two replicas with one qualified and one revoked reports one, and a role
// whose every replica is known and none qualifies reports an EXPLICIT ZERO -- the list is
// complete and it is empty, which is what a revoked replica looks like. A count is published for
// every role the predicate looked at, so a role that qualifies nothing reports zero rather than
// vanishing from the status.
func modelDeploymentRoleQualified(
	qualifications []modelDeploymentInstanceQualification,
) map[string]*int32 {
	qualified := make(map[string]int32)
	seen := make(map[string]bool)
	conclusive := make(map[string]bool)
	for _, qualification := range qualifications {
		role := qualification.View.Role
		if !seen[role] {
			seen[role] = true
			conclusive[role] = true
		}
		// The flag is INTERSECTED ACROSS EVERY REPLICA, so one inconclusive replica demotes the
		// role however many of its siblings qualified. Setting it on the first conclusive replica
		// instead would publish a partial count and hide the open question behind it.
		conclusive[role] = conclusive[role] && qualification.Conclusive()
		// A replica can only be eligible if every leg verified, which already makes it
		// conclusive, so no separate guard is needed here.
		if qualification.Eligible() {
			qualified[role]++
		}
	}

	counts := make(map[string]*int32, len(seen))
	for role := range seen {
		if !conclusive[role] {
			continue
		}

		value := qualified[role]
		counts[role] = &value
	}

	return counts
}
