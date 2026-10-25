package worker

import (
	"maps"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// renderModelDeploymentServices renders every Service a deployment owns: the one it is reached
// through, and one per role.
//
// The deployment-wide one is FIRST, and the order is part of the contract: the caller garbage
// collects by name, and status reads its endpoint.
//
// manufacturers carries each role's accelerator manufacturer, keyed by role name, with a missing
// entry read as "not known to be Ascend": the KV event ports are part of the render only where the
// replicas behind the Service publish, and that decision is the connector's.
func renderModelDeploymentServices(
	md *workercore.ModelDeployment, manufacturers map[string]string, pods []core.Pod,
) []*core.Service {
	svcs := make([]*core.Service, 0, len(md.Spec.Roles)+1)
	svcs = append(svcs, renderModelDeploymentService(md, pods))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		standing := modelDeploymentReplicaServiceOrdinals(pods, md, role)
		svcs = append(svcs,
			renderModelDeploymentRoleService(md, role, manufacturers,
				modelDeploymentStandingMembers(pods, role, standing),
				modelDeploymentNeedsDeployedRouting(pods, md, role)))

		// ONE HEADLESS SERVICE PER REPLICA, AND ONLY WHERE A REPLICA HAS PEERS TO ADDRESS. It is
		// what publishes the members' names in DNS, so a replica whose members are alone needs none
		// -- rendering one anyway would put an object per replica on the cluster for no consumer.
		//
		// They come AFTER the role's own Service in this list, which matters because the caller
		// garbage collects by comparing this list against what exists. A replica keeps its Service
		// while it is desired or while its deployed members still need peer records.
		//
		// AN ORDINAL GETS ONE WHEN THE DESIRED SIZE SAYS IT WILL HAVE PEERS OR WHILE A MEMBER OF IT
		// IS STILL RUNNING. A role edited from four members to one leaves the four-member replica
		// serving while its replacement is built, and deleting its Service at that moment takes the
		// peer records out from under a running collective: the members cannot resolve each other,
		// and a collective that cannot resolve its peers does not fail visibly -- it hangs. So the
		// retention is by observed membership and ends when the last such member has gone, one pass
		// after the last departure.
		ordinals := standing
		for ordinal := range int(role.Replicas) {
			if modelDeploymentRoleSize(role) == 1 {
				break
			}
			ordinals[ordinal] = true
		}
		for _, ordinal := range slices.Sorted(maps.Keys(ordinals)) {
			svcs = append(svcs, renderModelDeploymentReplicaService(md, role, ordinal))
		}
	}

	return svcs
}

// modelDeploymentReplicaServiceOrdinals returns the ordinals whose running members still need peer
// records, whether or not the desired size would render them.
//
// A TERMINATING MEMBER COUNTS. The Service publishes unready addresses precisely so a member can
// find its peers before it is ready, and deleting the record while a departing member still runs
// is the same loss one instant later.
//
// THE EVIDENCE IS THE GROUP TOTAL THE MEMBER CARRIES, not the member index: the index is written at
// every size, so its presence says a Pod was rendered, not that it has peers.
func modelDeploymentReplicaServiceOrdinals(
	pods []core.Pod, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) map[int]bool {
	ordinals := make(map[int]bool)
	for i := range pods {
		pod := &pods[i]
		if !modelDeploymentOwns(pod, md) || modelDeploymentPodRole(pod) != role.Name {
			continue
		}
		total, err := strconvx.Atoi[int](pod.Annotations[kueuepodconst.GroupTotalCountAnnotation])
		if err != nil || total <= 1 {
			// A replica of one member has nobody to address.
			continue
		}
		if ordinal, ok := modelDeploymentPodOrdinal(pod); ok {
			ordinals[ordinal] = true
		}
	}

	return ordinals
}

// renderModelDeploymentReplicaService renders the headless Service one replica's members are
// published behind, which is the subdomain each of them names.
//
// IT IS HEADLESS BECAUSE THE POINT IS THE PER-POD RECORDS, not a virtual address. A ClusterIP would
// load-balance across the members, which is the one thing a rank must never do: a collective's
// member addresses its peers individually, by name, and an address that resolves to "whichever
// member" is an address for none of them.
//
// IT SELECTS ON THE ORDINAL, so a replica's Service publishes that replica's members and nobody
// else's. The role's identity labels alone would make every replica's Service front every member of
// the role, and each member would then resolve its peers to the wrong replica's Pods -- a collective
// that forms across instance boundaries and serves wrong answers rather than failing.
//
// IT PUBLISHES UNREADY MEMBERS. Without publishNotReadyAddresses a member's record does not exist
// until the kubelet calls it ready, and a rank cannot become ready until it has joined the
// collective it needs the record to find: the group would deadlock on its own readiness. This is
// the same circularity the gating has, resolved the same way -- by not waiting.
func renderModelDeploymentReplicaService(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, ordinal int,
) *core.Service {
	svc := renderModelDeploymentServiceFor(
		md, role, modelDeploymentReplicaServiceName(md, role.Name, ordinal),
	)

	svc.Spec.ClusterIP = core.ClusterIPNone
	svc.Spec.PublishNotReadyAddresses = true
	svc.Spec.Selector[modelDeploymentReplicaOrdinalLabel] = strconvx.Itoa(ordinal)

	return svc
}

// renderModelDeploymentService renders the one Service a deployment is reached through.
//
// It is deliberately NOT what instance.go does. convertServiceFromInstance renders one NodePort Service
// PER POD, which is right for an Instance — a single addressable development box a user connects
// to. N interchangeable replicas behind one address is a different shape: a per-Pod NodePort would
// publish N addresses with no load balancing across them, and burn one node port per replica.
//
// IT STILL FRONTS THE FIRST ROLE, and with P/D roles that stays true rather than being decided
// otherwise. The single-role spec left the choice to this one; this one's Non-Goals answer it by
// declining to route: nothing here steers a request between a prefiller and a decoder, so no role is
// a front door in the sense that would make choosing between them meaningful. A Service selecting
// EVERY role would be worse than arbitrary — it would round-robin a request onto a process
// configured as a producer and one configured as a consumer, which is the silent wrong answer this
// spec's whole kind field exists to prevent. Per-role addressability is what this task adds; a real
// front door needs the router, and that is a later spec.
// modelDeploymentEffectiveRoleForEligibility returns the role as the replicas standing in this
// cluster see it.
//
// The gate is about the replicas serving, not about the role the spec states now. A role turned into
// a take-over one while its old managed replicas are still running has not lost the group-forward
// observation for them: their command lines are the ones this operator wrote. Clearing the desired
// command line while any such replica stands keeps the gate, and the selector narrowed by it, from
// being withdrawn on the pass that turned the role over.
func modelDeploymentEffectiveRoleForEligibility(
	role *workercore.ModelDeploymentRole, members []*core.Pod,
) *workercore.ModelDeploymentRole {
	if !modelDeploymentAnyMemberRunsCommand(members) {
		return role
	}
	effective := *role
	effective.Command = nil

	return &effective
}

// modelDeploymentStandingMembers returns the live members of the replicas of one role that still
// hold a seat, which is what the gate and the membership rules are read from.
func modelDeploymentStandingMembers(
	pods []core.Pod, role *workercore.ModelDeploymentRole, standing map[int]bool,
) []*core.Pod {
	members := make([]*core.Pod, 0, len(standing))
	for _, view := range modelDeploymentGroupPods(pods, true) {
		if modelDeploymentPodRole(view.Members[0]) != role.Name || !standing[view.Ordinal] {
			continue
		}
		members = append(members, view.Members...)
	}

	return members
}

func renderModelDeploymentService(
	md *workercore.ModelDeployment, pods []core.Pod,
) *core.Service {
	front := modelDeploymentFrontRole(md)
	svc := renderModelDeploymentServiceFor(md, front, md.Name)
	modelDeploymentFrontLeadersOnly(svc, front,
		modelDeploymentRoleHasMultiMemberReplica(pods, md, front))
	modelDeploymentSelectAnsweringMembers(svc, modelDeploymentNeedsDeployedRouting(pods, md, front))
	// ONLY THE FRONT ROLE'S OWN STANDING MEMBERS: the Service routes that role alone, so a sibling
	// role's managed Pod, or a Pod already leaving, must not lift the take-over refusal for it.
	var frontStanding []*core.Pod
	for _, view := range modelDeploymentGroupPods(pods, true) {
		if modelDeploymentPodRole(view.Members[0]) == front.Name {
			frontStanding = append(frontStanding, view.Members...)
		}
	}
	if modelDeploymentEligibilitySelectorActive(md,
		modelDeploymentEffectiveRoleForEligibility(front, frontStanding), nil,
		modelDeploymentEligibilityDecided(md)) {
		modelDeploymentSelectEligibleEndpoints(svc, md, front)
	}

	return svc
}

// modelDeploymentFrontRole is the role the deployment's own address belongs to: the one replica group
// the deployment-wide Service routes and the published endpoint describes.
//
// AN ENCODE ROLE IS NEVER IT. An encoder turns media into embeddings for a prefiller and answers no
// request of its own, so a front door pointed at one sends traffic to a process that serves none of
// it -- with endpoints that resolve and answer health checks, which is what makes the failure quiet.
// The encoder's position in the list is therefore irrelevant: first, middle or last, it is skipped.
//
// A DEPLOYMENT WITH NO OTHER ROLE ANSWERS NOBODY either, and admission refuses one rather than
// creating it. The first role is returned in that case so a spec the refusal did not reach still
// renders an address instead of nothing.
func modelDeploymentFrontRole(md *workercore.ModelDeployment) *workercore.ModelDeploymentRole {
	for i := range md.Spec.Roles {
		if ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) != workercore.ModelDeploymentRoleKindEncode {
			return &md.Spec.Roles[i]
		}
	}

	return &md.Spec.Roles[0]
}

// modelDeploymentFrontLeadersOnly narrows a Service that serves the OpenAI API to the one member of
// each replica that answers it.
//
// EVERY MEMBER CARRIES THE ROLE'S IDENTITY LABELS -- they have to, since that is what attributes a
// Pod to its role in status, in the converger and in every other selector -- so a selector written
// when a replica was one Pod fronts all `size` of them. Above size one the members that are not the
// leader serve no API at all, so a plain round-robin fails for (size-1)/size of requests, with the
// successes and failures interleaved rather than the Service being visibly broken.
//
// AT SIZE ONE THE SELECTOR IS LEFT EXACTLY AS IT WAS, and not merely equivalently: a single-Member
// replica's Pod carries no member-index label, so adding the term there would select nothing and
// empty every Service of every deployment already running.
//
// WHILE A ROLE IS MID-EDIT, THE TERMS COME FROM WHAT IS STANDING RATHER THAN FROM THE DESIRED
// SIZE. A replica edited from four members down to one still has four members running, and a
// selector computed from the new size alone would drop the leader term and front all four of them
// -- which is the exact failure above, reappearing for a deployment that was working. So the term
// is applied whenever the desired size is above one OR any actual member of the role carries a
// member index, and dropped only once the last such member has gone.
func modelDeploymentFrontLeadersOnly(
	svc *core.Service, role *workercore.ModelDeploymentRole, multiMemberStanding bool,
) {
	if modelDeploymentRoleSize(role) == 1 && !multiMemberStanding {
		return
	}

	svc.Spec.Selector[modelDeploymentMemberIndexLabel] = strconvx.Itoa(modelDeploymentLeaderMemberIndex)
}

// modelDeploymentNeedsDeployedRouting reports whether desired routing differs from a running member.
// Use observed membership from the first edit, including before a replacement appears.
func modelDeploymentNeedsDeployedRouting(
	pods []core.Pod, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	desiredManaged := len(role.Command) == 0
	desiredExternal := desiredManaged && modelDeploymentRoleExternalDP(md, role)
	for i := range pods {
		pod := &pods[i]
		if !modelDeploymentOwns(pod, md) || modelDeploymentPodRole(pod) != role.Name {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		managed, readable := modelDeploymentMemberExecution(pod)
		if !readable {
			continue
		}
		if managed != desiredManaged {
			return true
		}
		if !managed {
			continue
		}
		// A member whose parallelism cannot be read says nothing about the routing it was built with.
		if external, known := modelDeploymentMembersExternalDP(
			[]*core.Pod{pod}, md.Spec.Engine.Name,
		); known && external != desiredExternal {
			return true
		}
	}

	return false
}

// modelDeploymentSelectAnsweringMembers narrows an HTTP Service to the members whose replicas
// answer it under the mode they were built with.
//
// Use the term while a running mode or answering shape differs from the desired configuration.
// Remove it once every member follows the desired routing rule.
func modelDeploymentSelectAnsweringMembers(
	svc *core.Service, mixed bool,
) {
	if !mixed {
		return
	}

	svc.Spec.Selector[modelDeploymentLabelKeyAPIAnswering] = modelDeploymentAPIAnsweringValue
	delete(svc.Spec.Selector, modelDeploymentLabelKeyEndpointEligible)
	delete(svc.Spec.Selector, modelDeploymentMemberIndexLabel)
}

// modelDeploymentRoleHasMultiMemberReplica reports whether any actual member of a role still
// belongs to a replica built at more than one member, which is what makes it a member rather than
// a whole replica.
func modelDeploymentRoleHasMultiMemberReplica(
	pods []core.Pod, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	for i := range pods {
		pod := &pods[i]
		if !modelDeploymentOwns(pod, md) || modelDeploymentPodRole(pod) != role.Name {
			continue
		}
		if total, err := strconvx.Atoi[int](pod.Annotations[kueuepodconst.GroupTotalCountAnnotation]); err == nil &&
			total > 1 {
			return true
		}
	}

	return false
}

// modelDeploymentGroupForwardActivatable reports whether this role's group-forward capability
// can ever reach Verified or NotApplicable — the two states that activate eligibility. A shape
// the operator has no observation for (an engine absent from the matrix) answers Unsupported by
// construction and can never activate, so an eligibility selector on its Services would select
// zero endpoints forever: those Services render WITHOUT the term, preserving the routing a
// deployment had before first enable.
//
// A P/D ROLE ACTIVATES WHEN THE OPERATOR HAS AN OBSERVATION FOR ITS PAIR. The check is asked
// about the routes the operator still knows, because the gate is about the replicas serving: a
// role that took its command line over while a managed member of it still stands has not lost the
// route the operator knows, and a partner that took its own over does not take this role's
// barrier away — the qualification answers Unsupported for that pair, so the group's existing
// eligibility is held rather than revoked.
func modelDeploymentGroupForwardActivatable(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	if _, known := modelDeploymentGroupForwardEngines[md.Spec.Engine.Name]; !known {
		return false
	}
	if len(role.Command) > 0 {
		return false
	}
	kind := ModelDeploymentEffectiveRoleKind(role)
	if kind != workercore.ModelDeploymentRoleKindPrefill &&
		kind != workercore.ModelDeploymentRoleKindDecode {
		return true
	}
	unsupported, _ := modelDeploymentGroupForwardPairSupport(modelDeploymentPairableDeployment(md))

	return !unsupported
}

// modelDeploymentRoleJudged reports whether the operator controls this role's eligibility, in the
// stable sense: the requirement is in force before any observation is read, so a selector that
// names it does not move when the gate decides.
//
// A ROLE THIS OPERATOR CANNOT ASK ABOUT IS NOT JUDGED. Its route is one this operator does not
// know, so there is no group of it to qualify and no requirement to carry.
//
// A PROTECTED CLASS IS JUDGED WHEN THE OPERATOR CAN ASK ABOUT IT, and its requirement is in force
// from the first enable, decided or not. THE PROTECTED CLASS AND THE JUDGED CLASS ARE TWO
// DIFFERENT QUESTIONS: a protected class the operator cannot ask about keeps the requirement, and
// the native selectors still require eligibility, while the judgment is not there to decide it.
//
// A GROUP THIS OPERATOR CAN ANSWER WITHOUT AN OBSERVATION IS JUDGED TOO: a replica of one member
// has no cross-member path, and an encoder serves no tokens.
//
// A MANAGED PAIR IS JUDGED WHEN ITS PROBE PATH IS AVAILABLE, and what the pair's own observation
// decides then is whether the group is qualified, not whether the requirement is in force. AN
// UNAVAILABLE PROBE PATH LEAVES A PROTECTED PAIR UNJUDGED.
//
// THE PAIR ARM OF THE SWITCH BELOW IS NOT REACHED. A pair that is not protected is a pair whose
// command line the user took over, and such a role is not activatable, so the first branch has
// already answered it.
func modelDeploymentRoleJudged(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	if !modelDeploymentGroupForwardActivatable(md, role) {
		return false
	}
	if modelDeploymentProtectedRole(md, role) {
		return true
	}
	switch ModelDeploymentEffectiveRoleKind(role) {
	case workercore.ModelDeploymentRoleKindPrefill, workercore.ModelDeploymentRoleKindDecode:
		return false
	case workercore.ModelDeploymentRoleKindEncode:
		return true
	}

	return modelDeploymentRoleSize(role) <= 1
}

// modelDeploymentPairableDeployment returns the deployment as the pair support check must read it
// for the gate: its Prefill and Decode roles carry the operator's own command lines.
//
// THE COPY EXISTS FOR THAT ONE DECISION, and it is never handed to the qualification, the probe,
// a render or the caller's spec. The check reads the copy, whose P/D command lines are cleared, so
// it asks about the pair the operator configured and not about a take-over. The gate is about the
// replicas serving: a role that took its command line over while a managed member of it still
// stands has not lost the route the operator knows.
func modelDeploymentPairableDeployment(md *workercore.ModelDeployment) *workercore.ModelDeployment {
	pairable := md.DeepCopy()
	for i := range pairable.Spec.Roles {
		switch ModelDeploymentEffectiveRoleKind(&pairable.Spec.Roles[i]) {
		case workercore.ModelDeploymentRoleKindPrefill, workercore.ModelDeploymentRoleKindDecode:
			pairable.Spec.Roles[i].Command = nil
		}
	}

	return pairable
}

// modelDeploymentEligibilityDecided reports whether a pass has actually answered the whole-group
// predicate: the EndpointEligibility condition is True (qualified -- narrow and let the labels
// admit) or False (revoked -- narrow and let the labels withdraw). A decided condition is the
// operator's own act, so acting on it -- including against legacy routing that predates the gate
// -- is not a regression the reviewer traced. An Unknown or absent condition has decided nothing,
// and a from-scratch render on it never narrows.
func modelDeploymentEligibilityDecided(md *workercore.ModelDeployment) bool {
	for i := range md.Status.Conditions {
		condition := &md.Status.Conditions[i]
		if condition.Type == string(ModelDeploymentConditionEndpointEligibility) {
			return condition.Status == meta.ConditionTrue || condition.Status == meta.ConditionFalse
		}
	}

	return false
}

// modelDeploymentGroupProtection names how one group's selection is governed.
type modelDeploymentGroupProtection int

const (
	// modelDeploymentGroupUnprotected is a group this operator keeps its existing behavior for.
	modelDeploymentGroupUnprotected modelDeploymentGroupProtection = iota
	// modelDeploymentGroupProtected is a managed group whose selection is narrowed by the group's own
	// eligibility, and whose restoration waits for a fresh observation of the group itself.
	modelDeploymentGroupProtected
)

// modelDeploymentGroupProtectionOf names how one group's selection is governed.
//
// THE CLASSES ARE DECIDED BY THE SHAPE HELPERS RATHER THAN BY MATCHING A REASON STRING:
//
//   - a MANAGED PROTECTED GROUP -- a multi-member ordinary replica, an External DP one, or a
//     Prefill or Decode one -- is narrowed by the group's own eligibility;
//   - an UNSUPPORTED CUSTOM ROUTE, or an engine outside the capability matrix, keeps the existing
//     compatibility carve-out, because this operator cannot judge the route at all;
//   - an ORDINARY SINGLE-MEMBER SERVER keeps its existing behavior, including its observation
//     compatibility, because there is no group to protect.
//
// THE ROLE IS READ AS THE EFFECTIVE ROLE, the one the standing members are running: a role that
// took its command line over is not the managed role the spec still names, unless a standing member
// still runs the operator's command.
func modelDeploymentGroupProtectionOf(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, view modelDeploymentReplicaView,
) modelDeploymentGroupProtection {
	effective := *role
	if modelDeploymentReplicaRunsCommand(view.Members) {
		effective.Command = nil
	}
	if len(effective.Command) > 0 {
		return modelDeploymentGroupUnprotected
	}
	if _, known := modelDeploymentGroupForwardEngines[md.Spec.Engine.Name]; !known {
		return modelDeploymentGroupUnprotected
	}
	kind := ModelDeploymentEffectiveRoleKind(&effective)
	if kind == workercore.ModelDeploymentRoleKindPrefill || kind == workercore.ModelDeploymentRoleKindDecode {
		return modelDeploymentGroupProtected
	}
	if modelDeploymentRoleExternalDP(md, &effective) {
		return modelDeploymentGroupProtected
	}
	if len(view.Members) > 1 {
		return modelDeploymentGroupProtected
	}

	return modelDeploymentGroupUnprotected
}

// modelDeploymentProtectedRole reports whether a role's groups are narrowed by their own
// eligibility.
//
// IT IS THE ROLE-SIDE ANSWER, and it is read from the role's own shape: a Prefill or a Decode role,
// an External DP one, or a role whose members share more than one seat. The group-side answer also
// reads the standing members, and the two agree for every role but a take-over.
func modelDeploymentProtectedRole(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	// A ROLE THAT TOOK ITS COMMAND LINE OVER IS NOT THE MANAGED ROLE THE SPEC STILL NAMES, and the
	// compatibility carve-out is the point of the class: narrowing a route nobody can judge would
	// select nothing at all.
	if len(role.Command) > 0 {
		return false
	}
	kind := ModelDeploymentEffectiveRoleKind(role)
	if kind == workercore.ModelDeploymentRoleKindPrefill || kind == workercore.ModelDeploymentRoleKindDecode {
		return true
	}
	if modelDeploymentRoleExternalDP(md, role) {
		return true
	}

	return role.ReplicaSize > 1
}

// modelDeploymentEligibilitySelectorActive is the ONE decision for whether a surface names
// endpoint-eligible endpoints.
//
// THE COMPATIBILITY CARVE-OUT COMES FIRST, because it is the one class that never narrows: an engine
// outside the matrix, or a role that took its command line over, has no observation this operator can
// make, and a surface naming eligible endpoints it can never qualify would select nothing.
//
// THE MANAGED PROTECTED CLASS IS PRESENT FROM THE FIRST ENABLE, including an undecided pass. A
// surface without the requirement does not read the eligibility labels at all, so a group that has
// never been qualified would keep serving on readiness alone.
//
// THE REMAINING CLASSES KEEP THE EXISTING THREE HALVES: the static half (the shape must be able to
// activate at all), the activation half (a pass must have recorded a decided predicate -- True
// (qualified) or False (revoked) -- because narrowing before any pass has answered would select zero
// endpoints and strand whatever routing the deployment already had), and the retention half (once a
// surface carries the term, the live selector keeps it while the shape stays activatable).
func modelDeploymentEligibilitySelectorActive(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	liveSelector map[string]string, passDecided bool,
) bool {
	if _, known := modelDeploymentGroupForwardEngines[md.Spec.Engine.Name]; !known {
		return false
	}
	if len(role.Command) > 0 {
		return false
	}
	if modelDeploymentProtectedRole(md, role) {
		return true
	}
	if !modelDeploymentGroupForwardActivatable(md, role) {
		return false
	}
	if passDecided || modelDeploymentEligibilityDecided(md) {
		return true
	}

	return liveSelector != nil &&
		liveSelector[modelDeploymentLabelKeyEndpointEligible] == modelDeploymentEndpointEligibleValue
}

// modelDeploymentEligibilityActiveAnnotation retains gate activation while mixed Services use membership.
const modelDeploymentEligibilityActiveAnnotation = "modeldeployment.gpustack.ai/eligibility-active"

// modelDeploymentSelectEligibleEndpoints selects qualified HTTP endpoints.
// External roles serve every rank; Internal roles retain their leader restriction.
// Headless peer Services never use endpoint eligibility.
func modelDeploymentSelectEligibleEndpoints(
	svc *core.Service, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) {
	if svc.Spec.Selector[modelDeploymentLabelKeyAPIAnswering] == modelDeploymentAPIAnsweringValue {
		// Membership already combines each deployed mode's qualification and answering shape.
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		svc.Annotations[modelDeploymentEligibilityActiveAnnotation] = modelDeploymentEndpointEligibleValue
		return
	}
	svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
	// Every qualified External-DP member serves HTTP. The eligibility label already
	// excludes unhealthy groups, so the legacy leader restriction must not hide ranks.
	if modelDeploymentRoleExternalDP(md, role) {
		delete(svc.Spec.Selector, modelDeploymentMemberIndexLabel)
	}
}

// renderModelDeploymentRoleService renders the Service that fronts ONE role.
//
// A P/D deployment needs it because a decoder has to be reachable AS a decoder: the two roles are
// deliberately different configurations, so an address that resolves to "whichever replica" is not
// an address for either of them. Nothing in this spec calls it — no router exists yet — and that is
// exactly why it is rendered now: the pairing spec that will call it should find the addresses
// already there rather than have to change this object's shape to get them.
//
// The name is <deployment>-<role>, and a Service name is a DNS-1035 label that stops at 63
// characters while an object name runs to 253.
//
// THIS IS A NEW CLASS OF FAILURE, NOT AN INHERITED ONE — an earlier version of this comment claimed
// otherwise and was wrong. The deployment-wide Service fails only for a deployment whose own name is
// too long; this one fails for a 40-character deployment with a 30-character role, both of which are
// legal on their own. The reachable input set changed, which is what decides it, rather than whether
// the failure has a familiar shape.
//
// So the webhook refuses the combination at admission — validateModelDeploymentRoleServiceNames —
// where the message can name both halves. Without that the Pod-side of the deployment works, the
// per-role Service is rejected on every create, and the reconciler retries forever with the cause
// two objects away from the field that caused it.
func renderModelDeploymentRoleService(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	manufacturers map[string]string, standingMembers []*core.Pod, mixedCommandMode bool,
) *core.Service {
	svc := renderModelDeploymentServiceFor(md, role, md.Name+"-"+role.Name)
	modelDeploymentFrontLeadersOnly(svc, role, len(standingMembers) > 0)
	modelDeploymentSelectAnsweringMembers(svc, mixedCommandMode)
	if modelDeploymentEligibilitySelectorActive(md,
		modelDeploymentEffectiveRoleForEligibility(role, standingMembers), nil,
		modelDeploymentEligibilityDecided(md)) {
		modelDeploymentSelectEligibleEndpoints(svc, md, role)
	}
	if modelDeploymentPublishesKVEvents(md, role, manufacturers[role.Name]) {
		for _, port := range inject.KVEventsPorts() {
			svc.Spec.Ports = append(svc.Spec.Ports, core.ServicePort{
				Name: port.Name, Protocol: port.Protocol, Port: port.ContainerPort,
				TargetPort: intstr.FromInt32(port.ContainerPort),
			})
		}
	}

	return svc
}

// renderModelDeploymentServiceFor is the shape both Services share: a ClusterIP fronting one role's
// replicas, on that role's port, owned by the deployment.
//
// THE PORT AND THE TARGET PORT ARE TWO FIGURES. The port is what a caller dials, and it stays on the
// declared or default port; the target is where the replicas actually listen, which a role declaring
// no ports moves with its own --port.
func renderModelDeploymentServiceFor(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, name string,
) *core.Service {
	port := modelDeploymentServicePort(role)
	target := ModelDeploymentRoleServingPort(md, role)

	svc := &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Name:      name,
			Namespace: md.Namespace,
			Labels:    modelDeploymentSelectorLabels(md, role),
		},
		Spec: core.ServiceSpec{
			Type: core.ServiceTypeClusterIP,
			// The selector carries identity only. A Service's selector is mutable, unlike a
			// Deployment's, but moving it would orphan every replica it used to front — so nothing
			// a spec update can change is in it, and the entrance label in particular is not.
			//
			// It names the DEPLOYMENT as well as the role. A selector on the role alone would make
			// two deployments in one namespace, each running a role called "decode", share endpoints.
			Selector: modelDeploymentSelectorLabels(md, role),
			Ports: []core.ServicePort{{
				Name:       port.Name,
				Protocol:   port.Protocol,
				Port:       port.ContainerPort,
				TargetPort: intstr.FromInt32(target),
			}},
		},
	}

	// The selector this leaves is the role's identity and nothing else, which selects EVERY member
	// of every replica. Each caller then narrows it to what it is for: the two ClusterIP Services
	// front leaders (modelDeploymentFrontLeadersOnly), and a replica's headless Service narrows to
	// that replica instead. Deciding it here would give one of the three the wrong endpoints.

	systemmeta.NoteResource(svc, ModelDeploymentResourceType, map[string]string{
		ModelDeploymentResourceNoteRole: role.Name,
	})
	kubemeta.ControlOnWithoutBlock(svc, md, workercore.SchemeGroupVersionKind("ModelDeployment"))

	return svc
}

// modelDeploymentServicePort is the port the deployment is published on: the role's first declared
// entry, or the default every supported engine's OpenAI-compatible server listens on. It is the
// Service's own port and the published endpoint's; where the replicas listen is
// modelDeploymentTargetPort.
//
// It reads the same render the replicas get rather than the fields directly, so the Service and
// the containers behind it cannot name different ports.
func modelDeploymentServicePort(role *workercore.ModelDeploymentRole) core.ContainerPort {
	if len(role.Ports) == 0 {
		return core.ContainerPort{
			Name:          modelDeploymentDefaultPortName,
			Protocol:      core.ProtocolTCP,
			ContainerPort: modelDeploymentDefaultPort,
		}
	}

	return modelDeploymentContainerPorts(role)[0]
}

// modelDeploymentTargetPort is the container port a role's replicas serve on: the one its Service
// targets, its gates grade and a router dials.
//
// A ROLE DECLARING NO PORTS MOVES IT WITH ITS OWN --port, in any spelling the engine reads as it.
// The operator fills --port only where the role passed none, so that argument is where the engine
// listens, and a target left on the default would send every request to a port nothing opens.
//
// A DECLARED PORT IS NOT MOVED. The declared port is what the operator renders into the engine's
// --port, and admission refuses a role whose own --port names another one.
//
// A DIRECT DECODER'S IS NOT MOVED EITHER, because fronted reports that its routing proxy owns the
// serving port: the role's --port there names the engine behind the proxy, not the Pod's front door.
func modelDeploymentTargetPort(
	engine string, role *workercore.ModelDeploymentRole, fronted bool,
) core.ContainerPort {
	port := modelDeploymentServicePort(role)
	if len(role.Ports) > 0 || fronted {
		return port
	}
	if listen, ok := ModelDeploymentRoleListenPort(engine, role); ok {
		port.ContainerPort = listen
	}

	return port
}

// ModelDeploymentRoleListenPort returns the value of the last --port a role passes, in any spelling
// the engine reads as it, and false when it passes none or one it cannot read. The engine's base
// argv and the connector's arguments carry no listen flag, so the role's own arguments are the only
// place one can come from.
func ModelDeploymentRoleListenPort(engine string, role *workercore.ModelDeploymentRole) (int32, bool) {
	port, err := modelDeploymentCommandPort(engine, ModelDeploymentRoleArgs(role))

	return port, err == nil
}

// ModelDeploymentRoleFrontedByProxy reports whether a role is a direct decoder whose routing proxy
// owns its serving port, which is decided by the spec alone: the proxy renders for every decoder
// under the router that drives it, and never for a role that took over its command line.
func ModelDeploymentRoleFrontedByProxy(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	return len(role.Command) == 0 && modelDeploymentFrontsDecodeWithSidecar(md, role)
}

// ModelDeploymentRoleServingPort returns the port a role's Service targets.
func ModelDeploymentRoleServingPort(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) int32 {
	return modelDeploymentTargetPort(
		md.Spec.Engine.Name, role, ModelDeploymentRoleFrontedByProxy(md, role),
	).ContainerPort
}

// ModelDeploymentRoleServingProtocol returns the protocol a role's Service publishes.
func ModelDeploymentRoleServingProtocol(role *workercore.ModelDeploymentRole) core.Protocol {
	return modelDeploymentServicePort(role).Protocol
}

// ModelDeploymentRoleServingScheme returns the scheme a role's engine endpoint publishes.
func ModelDeploymentRoleServingScheme(engine string, role *workercore.ModelDeploymentRole) core.URIScheme {
	scheme, _ := modelDeploymentEngineTransport(engine, ModelDeploymentRoleArgs(role))

	return scheme
}

// modelDeploymentEndpoint is the one address every replica serves behind.
//
// It is derived rather than read back off the Service, because the in-cluster DNS name is decided by
// the Service's name and namespace — both of which this operator chose — and a ClusterIP is neither
// the address callers use nor stable across a recreate.
//
// THE SCHEME IS READ FROM THE SAME ROLE THE PORT IS, and it has to be: a role serving over TLS that
// is published as http:// is an address no client can use, while the replica behind it is graded
// Ready by a probe that did speak TLS. The two would then be separate facts, which is exactly what
// the gates exist to prevent. The front role decides both, because it is the role this Service
// fronts.
//
// It is read from the role's OWN arguments rather than from a rendered command line, which this
// function has no way to build. That is sound only because the operator's own contributions carry no
// listen or TLS flag, which is asserted by a test rather than assumed here.
func modelDeploymentEndpoint(md *workercore.ModelDeployment) string {
	role := modelDeploymentFrontRole(md)
	port := modelDeploymentServicePort(role)

	// ONLY THE SCHEME IS WANTED HERE, and discarding the other answer is deliberate rather than an
	// oversight. It reports whether a PROBE may grade the listener, which is a different question
	// from what a client should dial: a role that moved its host still serves behind this address,
	// and one demanding a client certificate still serves TLS. Neither is an error, and turning this
	// into one would refuse to publish an address that is correct.
	//
	// THE PORT IS THE PUBLISHED ONE even where the role moved its engine's port: the Service's
	// targetPort follows the engine, so the address a caller already dials keeps working.
	scheme, _ := modelDeploymentEngineTransport(md.Spec.Engine.Name, ModelDeploymentRoleArgs(role))

	return strings.ToLower(string(scheme)) + "://" +
		md.Name + "." + md.Namespace + ".svc:" + strconvx.Itoa(int(port.ContainerPort))
}

func modelDeploymentRoleEndpoint(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) string {
	port := modelDeploymentServicePort(role)
	scheme, _ := modelDeploymentEngineTransport(md.Spec.Engine.Name, ModelDeploymentRoleArgs(role))

	return strings.ToLower(string(scheme)) + "://" + md.Name + "-" + role.Name + "." +
		md.Namespace + ".svc:" + strconvx.Itoa(int(port.ContainerPort))
}

// alignModelDeploymentService folds the rendered Service onto the observed one and reports whether
// anything changed.
//
// It aligns rather than replaces because a Service carries state the operator did not write and
// must not discard — the allocated ClusterIP above all, which every client that resolved it is
// still using.
func alignModelDeploymentService(actual, expected *core.Service) (changed bool) {
	key := modelDeploymentEligibilityActiveAnnotation
	if actual.Annotations[key] != expected.Annotations[key] {
		if expected.Annotations[key] == "" {
			delete(actual.Annotations, key)
		} else {
			if actual.Annotations == nil {
				actual.Annotations = map[string]string{}
			}
			actual.Annotations[key] = expected.Annotations[key]
		}
		changed = true
	}
	if !maps.Equal(actual.Spec.Selector, expected.Spec.Selector) {
		actual.Spec.Selector = expected.Spec.Selector
		changed = true
	}

	if !equalModelDeploymentServicePorts(actual.Spec.Ports, expected.Spec.Ports) {
		actual.Spec.Ports = expected.Spec.Ports
		changed = true
	}

	// THE TYPE IS CONVERGED TOO, and it became reachable when this controller began watching the
	// Service: an out-of-band edit to NodePort or LoadBalancer now wakes this reconciler, and
	// leaving the type alone would mean the wake-up observed the drift and corrected everything
	// except it. A deployment's front door is a ClusterIP by design -- exposing every replica set on
	// a node port is a decision this operator does not make on a user's behalf.
	//
	// The ports are replaced along with it rather than compared: moving away from NodePort has to
	// drop the `nodePort` the API server assigned, and the port comparison above deliberately
	// ignores that field, so it would not notice.
	if actual.Spec.Type != expected.Spec.Type {
		actual.Spec.Type = expected.Spec.Type
		actual.Spec.Ports = expected.Spec.Ports
		changed = true
	}

	// CONVERGED BECAUSE IT IS MUTABLE AND LOAD-BEARING. A replica's headless Service publishes its
	// members before the kubelet calls them ready, and it has to: a member cannot become ready until
	// it has joined a collective it needs these records to find, so a group whose records wait for
	// readiness deadlocks on its own. Turning the field off out of band breaks exactly that, and the
	// symptom is a replica that never starts serving with every object looking correct.
	//
	// clusterIP is NOT converged beside it, and the asymmetry is the API's rather than a choice: it
	// is immutable after creation, so a write here would fail the update rather than fix anything.
	if actual.Spec.PublishNotReadyAddresses != expected.Spec.PublishNotReadyAddresses {
		actual.Spec.PublishNotReadyAddresses = expected.Spec.PublishNotReadyAddresses
		changed = true
	}

	return changed
}

// equalModelDeploymentServicePorts compares the fields this operator renders, and only those. A
// Service's ports carry defaults the API server fills in, so comparing the whole slice would report
// a difference on every pass and rewrite the object forever.
func equalModelDeploymentServicePorts(actual, expected []core.ServicePort) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i := range expected {
		if actual[i].Name != expected[i].Name ||
			actual[i].Port != expected[i].Port ||
			actual[i].Protocol != expected[i].Protocol ||
			actual[i].TargetPort != expected[i].TargetPort {
			return false
		}
	}

	return true
}
