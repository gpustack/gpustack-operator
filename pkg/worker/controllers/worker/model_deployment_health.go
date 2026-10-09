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
func qualifyModelDeploymentInstances(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
	pending modelDeploymentPendingReplacement, fetch modelDeploymentGroupForwardFetch,
) []modelDeploymentInstanceQualification {
	roles := make(map[string]*workercore.ModelDeploymentRole, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		roles[md.Spec.Roles[i].Name] = &md.Spec.Roles[i]
	}

	views := modelDeploymentGroupPodsByReplica(pods)
	qualifications := make([]modelDeploymentInstanceQualification, 0, len(views))
	for _, view := range views {
		role := roles[view.Role]
		if role == nil {
			// A replica of a role the spec no longer names is leaving; it qualifies for nothing
			// and reporting a held leg for it would be a reason nobody can act on.
			continue
		}

		qualifications = append(qualifications, qualifyModelDeploymentInstance(
			ctx, md, view, role, pending, fetch,
		))
	}

	return qualifications
}

// qualifyModelDeploymentInstance evaluates one replica's five legs.
func qualifyModelDeploymentInstance(
	ctx context.Context,
	md *workercore.ModelDeployment,
	view modelDeploymentReplicaView,
	role *workercore.ModelDeploymentRole,
	pending modelDeploymentPendingReplacement,
	fetch modelDeploymentGroupForwardFetch,
) modelDeploymentInstanceQualification {
	// THE CHEAP LEGS ARE DECIDED BEFORE THE OBSERVATION IS ASKED FOR. A replica that is short a
	// member or holds an unready one is already condemned by facts the informer cache carries, so
	// it never pays for a live observation, and more importantly its definite fault is never
	// softened into a hold by an observation that happens to be missing.
	//
	// Completeness is measured against the group total the members declare, never the role's
	// declared size, so a replica the role has been edited away from stays whole.
	// A shape the members cannot establish holds; a replica short of its total still fails.
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
	case unready == 0:
		membersReady.Reason = "every member reports ready"
	default:
		membersReady.Verdict = modelDeploymentLegFailed
		membersReady.Reason = strconvx.Itoa(unready) + " of " +
			strconvx.Itoa(len(view.Members)) + " members report not ready"
	}

	// A replica condemned by a definite leg owes nothing to a live probe: the request would
	// spend engine tokens on an answer that cannot soften the fault. The observation is still
	// ASKED, with no transport: answers that never needed the network (a one-member replica is
	// NotApplicable, an unverified shape is Unsupported) are unchanged, and only the probe leg
	// comes back as an absence of evidence.
	fetchForReplica := fetch
	if membersComplete.Verdict == modelDeploymentLegFailed ||
		membersReady.Verdict == modelDeploymentLegFailed {
		fetchForReplica = nil
	}
	groupForward := observeModelDeploymentGroupForward(ctx, md, role, view, fetchForReplica)

	qualification := modelDeploymentInstanceQualification{
		View:         view,
		Generation:   modelDeploymentGenerationOf(view),
		GroupForward: groupForward,
	}
	for _, member := range view.Members {
		if modelDeploymentMemberReadiness(member) != modelDeploymentLegUnknown {
			qualification.Observed = true

			break
		}
	}

	// COMPLETENESS IS NOT REDUNDANT WITH READINESS. A replica short of a declared member can
	// have every member it does hold reporting ready, while Kueue has composed no Workload for
	// it and it is admitted by nothing. It is absent with some Pods lying around, not partly
	// working.
	qualification.Legs = append(qualification.Legs, membersComplete)
	qualification.Legs = append(qualification.Legs, membersReady)

	qualification.Legs = append(qualification.Legs, modelDeploymentQualificationLeg{
		Name:    modelDeploymentLegGroupForward,
		Verdict: groupForwardVerdict(groupForward.State),
		Reason:  groupForward.Reason,
	})

	qualification.Legs = append(qualification.Legs, modelDeploymentQualificationLeg{
		Name:    modelDeploymentLegNoPlannedRetirement,
		Verdict: retirementVerdict(md, view),
		Reason:  retirementReason(md, view),
	})

	replacing := modelDeploymentQualificationLeg{
		Name:    modelDeploymentLegNoPendingReplacement,
		Verdict: modelDeploymentLegVerified,
		Reason:  "the replica is not condemned for replacement by this pass",
	}
	if view.Seated && pending.modelDeploymentPending(view.Role, view.Ordinal) {
		replacing.Verdict = modelDeploymentLegFailed
		replacing.Reason = "the replica is condemned for replacement by this pass"
	}
	qualification.Legs = append(qualification.Legs, replacing)

	return qualification
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
func modelDeploymentMemberReadiness(pod *core.Pod) modelDeploymentLegVerdict {
	for _, condition := range pod.Status.Conditions {
		if condition.Type != core.PodReady {
			continue
		}
		if condition.Status == core.ConditionTrue {
			return modelDeploymentLegVerified
		}
		if condition.Status == core.ConditionFalse {
			return modelDeploymentLegFailed
		}

		return modelDeploymentLegUnknown
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
