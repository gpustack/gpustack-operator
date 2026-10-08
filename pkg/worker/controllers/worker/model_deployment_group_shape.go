package worker

import (
	core "k8s.io/api/core/v1"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// A replica's deployed shape is read from the group total and member indexes its members carry,
// never from the role's declared size.

// modelDeploymentReplicaShapeState is how far a replica's members establish its shape.
type modelDeploymentReplicaShapeState int

const (
	// modelDeploymentReplicaShapeUnreadable is a shape the members do not establish: totals that
	// disagree, seats that are duplicated, out of range or unparsable, or a multi-member group with
	// no seat evidence.
	modelDeploymentReplicaShapeUnreadable modelDeploymentReplicaShapeState = iota
	// modelDeploymentReplicaShapeIncomplete is a readable shape whose members are short of the
	// total they declare.
	modelDeploymentReplicaShapeIncomplete
	// modelDeploymentReplicaShapeComplete is a readable shape the replica fills.
	modelDeploymentReplicaShapeComplete
)

// modelDeploymentReplicaShape is one replica's deployed size and how well its members establish it.
type modelDeploymentReplicaShape struct {
	// Total is the group size the members declare. It is zero only when the shape is unreadable.
	Total int
	// State is what the members establish.
	State modelDeploymentReplicaShapeState
	// Reason names the contradiction or the shortfall. It is the text a hold carries, so it says
	// which fact was read and which was expected.
	Reason string
}

// modelDeploymentReplicaIsWhole reports whether the members fill the total they declare.
func (s modelDeploymentReplicaShape) modelDeploymentReplicaIsWhole() bool {
	return s.State == modelDeploymentReplicaShapeComplete
}

// modelDeploymentReplicaLegVerdict is the leg verdict this shape implies.
//
// A replica short of its total lost members and is a fault. Members that contradict each other
// describe nothing, which is an unknown rather than a fault: this operator has established nothing
// about them, including that they are broken.
func (s modelDeploymentReplicaShape) modelDeploymentReplicaLegVerdict() modelDeploymentLegVerdict {
	switch s.State {
	case modelDeploymentReplicaShapeComplete:
		return modelDeploymentLegVerified
	case modelDeploymentReplicaShapeIncomplete:
		return modelDeploymentLegFailed
	default:
		return modelDeploymentLegUnknown
	}
}

// modelDeploymentDeployedReplicaShape reads one replica's shape from its own members. It is PURE.
func modelDeploymentDeployedReplicaShape(view modelDeploymentReplicaView) modelDeploymentReplicaShape {
	if len(view.Members) == 0 {
		return modelDeploymentReplicaShape{
			State:  modelDeploymentReplicaShapeUnreadable,
			Reason: "the replica holds no members, so it has no deployed shape to read",
		}
	}

	total, reason := modelDeploymentReplicaDeclaredTotal(view.Members)
	if reason != "" {
		return modelDeploymentReplicaShape{State: modelDeploymentReplicaShapeUnreadable, Reason: reason}
	}

	if reason = modelDeploymentReplicaSeatReason(view.Members, total); reason != "" {
		return modelDeploymentReplicaShape{State: modelDeploymentReplicaShapeUnreadable, Reason: reason}
	}

	if held := len(view.Members); held != total {
		return modelDeploymentReplicaShape{
			Total: total,
			State: modelDeploymentReplicaShapeIncomplete,
			Reason: "the replica holds " + strconvx.Itoa(held) + " of " + strconvx.Itoa(total) +
				" members it declared, so it is short of the group it was built at",
		}
	}

	return modelDeploymentReplicaShape{Total: total, State: modelDeploymentReplicaShapeComplete}
}

// modelDeploymentReplicaHoldsReason says how a replica's membership stands against its deployed
// total. An unreadable shape speaks for itself, because a count would be a claim the members do
// not support.
func modelDeploymentReplicaHoldsReason(
	view modelDeploymentReplicaView, shape modelDeploymentReplicaShape,
) string {
	if shape.State == modelDeploymentReplicaShapeUnreadable {
		return shape.Reason
	}

	return "the replica holds " + strconvx.Itoa(len(view.Members)) + " of " +
		strconvx.Itoa(shape.Total) + " declared members"
}

// modelDeploymentReplicaDeclaredTotal is the group size every member agrees on, or why they do not.
//
// A member declaring no total is a replica of one: that is what a Pod from before the annotation
// existed is, and modelDeploymentPodDescription has always read it so. A member declaring one is
// held to it, so a group mixing the two contradicts itself.
func modelDeploymentReplicaDeclaredTotal(members []*core.Pod) (int, string) {
	total := 0
	for _, member := range members {
		declared, ok := modelDeploymentMemberDeclaredTotal(member)
		if !ok {
			return 0, "member " + member.Name + " declares a group total that is not a replica size, " +
				"so the replica's shape could not be established"
		}

		if total == 0 {
			total = declared

			continue
		}
		if total != declared {
			return 0, "the replica's members declare group totals of " + strconvx.Itoa(total) +
				" and " + strconvx.Itoa(declared) + ", so its shape could not be established"
		}
	}

	return total, ""
}

// modelDeploymentMemberDeclaredTotal reads one member's own group total, and reports false for a
// value no replica could have been built at.
//
// ABSENCE IS NOT A FAILURE and reads as a replica of one; a value that is PRESENT and is not a size
// is. One is a Pod from before the annotation existed, the other is a claim this operator cannot
// act on.
func modelDeploymentMemberDeclaredTotal(member *core.Pod) (int, bool) {
	raw, present := member.Annotations[kueuepodconst.GroupTotalCountAnnotation]
	if !present {
		return 1, true
	}

	total, err := strconvx.Atoi[int](raw)
	if err != nil || total < 1 {
		return 0, false
	}

	return total, true
}

// modelDeploymentReplicaSeatReason reports why a replica's members cannot be told apart as seats,
// or "" when they can.
//
// No seat evidence is accepted except for a provable legacy single-member replica: the render has
// always written a seat onto every member, so a larger group with none was never produced here. A
// seat label that is present and unparsable is a defect, not that legacy case.
func modelDeploymentReplicaSeatReason(members []*core.Pod, total int) string {
	seats := make(map[int]string, len(members))
	claimed, invalid := 0, 0
	for _, member := range members {
		index, state := modelDeploymentMemberSeat(member)
		switch state {
		case modelDeploymentSeatInvalid:
			invalid++

			continue
		case modelDeploymentSeatAbsent:

			continue
		}

		claimed++
		if holder, taken := seats[index]; taken {
			return "members " + holder + " and " + member.Name + " both claim seat " +
				strconvx.Itoa(index) + " of a replica declaring " + strconvx.Itoa(total) +
				" members, so the replica cannot be told apart from itself"
		}
		seats[index] = member.Name

		if index >= total {
			return "member " + member.Name + " claims seat " + strconvx.Itoa(index) +
				" of a replica declaring only " + strconvx.Itoa(total) +
				" members, so the replica cannot be told apart from itself"
		}
	}

	if invalid > 0 {
		return strconvx.Itoa(invalid) + " of " + strconvx.Itoa(len(members)) +
			" members declare a member index that is not one, so the replica cannot be told apart " +
			"from itself"
	}
	if claimed == 0 {
		if total == 1 && len(members) == 1 {
			return ""
		}

		return "no member declares which seat it holds, and a replica of " + strconvx.Itoa(total) +
			" members is a shape whose seats were always written, so the replica cannot be told " +
			"apart from itself"
	}
	if claimed != len(members) {
		return strconvx.Itoa(claimed) + " of " + strconvx.Itoa(len(members)) +
			" members declare which seat they hold and the rest do not, so the replica cannot be " +
			"told apart from itself"
	}

	return ""
}

// modelDeploymentSeatState is what a member's seat label carries: a usable seat, none at all, or a
// claim that is not a seat.
//
// IT IS NOT A BOOL because absent and invalid are different facts that must not collapse. Absent
// marks a Pod from before the label existed; invalid marks a claim this operator cannot use.
type modelDeploymentSeatState int

const (
	modelDeploymentSeatValid modelDeploymentSeatState = iota
	modelDeploymentSeatAbsent
	modelDeploymentSeatInvalid
)

// modelDeploymentMemberSeat reads which seat a member claims.
//
// IT IS NOT modelDeploymentPodMemberIndex, which answers "which member does the engine call me" and
// defaults an absent label to the leader index. That default is right there and wrong here: it would
// make every member of a legacy replica a duplicate of the leader.
func modelDeploymentMemberSeat(member *core.Pod) (int, modelDeploymentSeatState) {
	raw, present := member.Labels[modelDeploymentMemberIndexLabel]
	if !present {
		return 0, modelDeploymentSeatAbsent
	}

	index, err := strconvx.Atoi[int](raw)
	if err != nil || index < 0 {
		return 0, modelDeploymentSeatInvalid
	}

	return index, modelDeploymentSeatValid
}

// modelDeploymentReplicaExecution is who built the command line a replica is running.
//
// It is a question about the running Pod. The role as declared now describes the replica about to
// be built and says nothing about the one still serving.
type modelDeploymentReplicaExecution int

const (
	// modelDeploymentExecutionUnreadable is a replica whose command line could not be attributed,
	// because a member carries no engine container to read.
	modelDeploymentExecutionUnreadable modelDeploymentReplicaExecution = iota
	// modelDeploymentExecutionManaged is a replica running a command line this operator built.
	modelDeploymentExecutionManaged
	// modelDeploymentExecutionTakeover is a replica running a command line the role supplied in
	// full, which this operator contributed no arguments, no environment and no connector to.
	modelDeploymentExecutionTakeover
)

// modelDeploymentDeployedReplicaExecution reports who built the command line this replica is
// running, read from its own members. It is PURE.
//
// The evidence is the serving-port annotation, the same one the group-forward probe relies on: the
// render stamps it only for roles running the operator's own command line. It is not the drain
// hook, which the Elastic realization clears from every Ray worker it renders.
func modelDeploymentDeployedReplicaExecution(members []*core.Pod) (modelDeploymentReplicaExecution, string) {
	if len(members) == 0 {
		return modelDeploymentExecutionUnreadable,
			"the replica holds no members, so it has no running command line to read"
	}

	// EVERY MEMBER IS ASKED. One with no engine container has not been read, and skipping it would
	// attribute a replica from the members that happened to be legible.
	built := false
	for _, member := range members {
		managed, ok := modelDeploymentMemberExecution(member)
		if !ok {
			return modelDeploymentExecutionUnreadable, "member " + member.Name + " carries no engine " +
				"container to read, so the replica's execution could not be established"
		}
		if member != members[0] && managed != built {
			return modelDeploymentExecutionUnreadable, "the replica's members disagree on who built " +
				"their command line, so its execution could not be established"
		}

		built = managed
	}

	if built {
		return modelDeploymentExecutionManaged, ""
	}

	return modelDeploymentExecutionTakeover, ""
}

// modelDeploymentMemberExecution reads whether this operator built one member's command line, and
// reports false for a member carrying no engine container to read.
func modelDeploymentMemberExecution(member *core.Pod) (managed bool, ok bool) {
	if _, found := modelDeploymentDrainContainerOf(member, modelDeploymentMainContainerName); !found {
		return false, false
	}

	return member.Annotations["prometheus.io/port"] != "", true
}
