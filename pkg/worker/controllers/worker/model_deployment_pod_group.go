package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// NO LABEL OF OUR OWN CARRIES THE ROLE'S NAME, and that is a decision rather than an omission.
// app.kubernetes.io/component already holds the role's name on every replica: it is in the selector
// labels the Service selects on, and it is what modelDeploymentPodRole reads to attribute a Pod to a
// role in status. A second selectable carrier would be two answers to one question, written by two
// functions, agreeing today -- and the one that drifted would be the one nothing reads on the
// failure path, whose symptom is a Service with no endpoints or a role reporting nobody ready.
//
// ONE KEY IN THIS PROJECT'S DOMAIN NOW CARRIES THE ROLE'S KIND: modelDeploymentLabelKeyRoleKind.
// That is not the case above and does not weaken it. A kind is a closed set of three values that
// something in front of the replicas selects on to tell a prefiller from a decoder; a name is a
// user's free-form string that only this deployment's own objects ever match. So the kind key has
// no second writer to drift from -- it is derived from the same ModelDeploymentEffectiveRoleKind
// the status echo uses -- while a name key would have had app.kubernetes.io/component.
//
// Adding that key was additive, which is why it was available to add: removing one already
// published breaks whoever selected on it.

const (
	// modelDeploymentPodGroupNamePrefix marks a group name this operator derived rather than took
	// verbatim. It is the escape nodefeature.FormatLocalQueueName takes for an over-long
	// ClusterQueue name, spelled the same way on purpose so one form is recognizable everywhere.
	//
	// That function is not called: its subject is a ClusterQueue name, and reusing it here would
	// make a group's identity read as a queue's. What is shared is the shape, not the derivation.
	modelDeploymentPodGroupNamePrefix = "gpustack-fnv64-"

	// modelDeploymentReplicaOrdinalLabel carries the ordinal a replica was rendered at, which is
	// the only per-replica identity the converger keys on: the group name is derived from it, the
	// spec-hash covers it, and a scale-down decides which replicas stay by it.
	//
	// IT IS BUILT FROM systemname.LabelPrefix RATHER THAN SPELLED, because the prefix is the one
	// place this project states its label domain; a second spelling would drift with nothing
	// failing.
	modelDeploymentReplicaOrdinalLabel = "modeldeployment." + systemname.LabelPrefix + "pod-ordinal"

	// modelDeploymentMemberIndexLabel carries which Pod of its replica this one is, counting from
	// zero. The ordinal label above says which replica; this says which member of it, and the two
	// together name exactly one Pod of the deployment.
	//
	// IT IS A LABEL RATHER THAN AN ENVIRONMENT VALUE THE RENDERER WRITES, and that is what keeps
	// one Pod template per replica. The container reads its own rank through a fieldRef naming this
	// key, so the container spec is identical in every member while the value differs -- a renderer
	// writing the index into env directly would make two members of one replica hash differently
	// and every member read as a pending rollout.
	//
	// IT IS ALSO WHAT MAKES A LEADER SELECTABLE. A Service selector cannot express "the Pod whose
	// name ends in -m0", so fronting only the member that serves the API needs the index to be
	// matchable, and index zero is the leader by construction rather than by election.
	modelDeploymentMemberIndexLabel = "modeldeployment." + systemname.LabelPrefix + "member-index"

	// modelDeploymentLeaderMemberIndex is the member every replica has and the only one the role's
	// Service fronts. A replica of size one is all leader; above that, the others serve no API.
	modelDeploymentLeaderMemberIndex = 0
)

// ModelDeploymentPodGroupMeta is the Kueue group metadata one replica's Pod carries.
//
// LABELS AND ANNOTATIONS ARE RETURNED TOGETHER AND NOTHING MAY APPLY A SUBSET. The membership label
// is what puts a Pod in the group, and the total-count annotation is what tells Kueue how many to
// wait for; a Pod carrying the first without the second joins a group whose size is unknown, and
// Kueue then refuses to compose the Workload with an error naming neither the Pod nor the field.
// The failure is silence, so the two travel as one value.
type ModelDeploymentPodGroupMeta struct {
	// Labels go on the Pod's metadata.labels.
	Labels map[string]string

	// Annotations go on the Pod's metadata.annotations.
	Annotations map[string]string
}

// modelDeploymentPodGroupSpec is one role's entry in the deployment's scheduling picture: the role,
// a name that role's share of the picture is keyed by, and the total the role declares.
//
// THE NAME IS A JOIN KEY RATHER THAN A WAY OF FINDING ANYTHING. A Pod's Kueue group is that one
// replica's own, derived per (role, ordinal) by modelDeploymentReplicaGroupName, so no Pod carries
// this name in its membership label. What still consumes this shape -- the status paths that key a
// role's Workloads by group name -- joins through it, which is the remaining coupling the per-replica
// split left behind and is refined by the status work that follows it.
type modelDeploymentPodGroupSpec struct {
	// Role is the grouping key: one entry is one role and nobody else's, and the total below is
	// that role's declared count.
	Role string

	// InstanceType is carried, not keyed on. It is the role's own type, held here so every message
	// and condition that names a cluster queue can name the queue this role schedules into: the
	// ClusterQueue is named after the InstanceType, and naming the role instead would send an
	// operator to an object that does not exist.
	InstanceType string

	// Name is the per-role join key described on the struct. It is stable for a role and distinct
	// between roles, which is all a join key owes.
	Name string

	// TotalCount is THIS role's declared replicas and no other role's.
	TotalCount int32
}

// modelDeploymentPodGroups returns the deployment's roles as scheduling entries, one per role in the
// roles' order.
//
// It is PURE: same deployment, same result, no client and no clock.
//
// THE GROUPING KEY IS THE ROLE, NOT THE instanceType. Kueue admits each replica as its own unit and
// everything the role-level picture exists to carry -- per-role counting, per-role flavor
// assignment, per-role status -- still hangs off that grouping. Keying on the type instead puts two
// roles on one instanceType into one entry, and their counts then disagree with nothing the total
// can express: moving prefill 2 / decode 2 to prefill 1 / decode 3 leaves the sum at four.
//
// THE ORDER IS THE ROLES' ORDER, so two passes over an unchanged spec return the same names in the
// same places. A map's iteration order would not, and these names key status maps.
func modelDeploymentPodGroups(md *workercore.ModelDeployment) []modelDeploymentPodGroupSpec {
	groups := make([]modelDeploymentPodGroupSpec, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		groups = append(groups, modelDeploymentPodGroupSpec{
			Role:         role.Name,
			InstanceType: role.InstanceType,
			TotalCount:   role.Replicas,
			Name:         modelDeploymentPodGroupNameOf(md, role.Name),
		})
	}

	return groups
}

// modelDeploymentRoleSize is how many Pods one replica of this role is made of.
//
// IT FLOORS AT ONE RATHER THAN TRUSTING THE FIELD. The schema defaults the field to one, so an
// object that went through the API server always carries at least that -- but a role built in
// memory, which every test and several callers do, carries the zero value. A zero reaching the
// total-count annotation would declare a group of no members, and Kueue composes no Workload at
// all for one: the replica would sit gated forever with nothing naming a cause.
func modelDeploymentRoleSize(role *workercore.ModelDeploymentRole) int {
	if role.ReplicaSize < 1 {
		return 1
	}

	return int(role.ReplicaSize)
}

// ModelDeploymentPodGroup returns the group metadata for ONE member of ONE replica of a role: the
// member at the given index, of the replica at the given ordinal.
//
// It is PURE: same deployment, role, ordinal, member, same result, no client and no clock. Every
// replica joins a Kueue pod group of its own -- its members, one Workload, one admission -- which
// is what makes a scale or a replacement a change to that replica alone rather than a change every
// sibling has to agree on before Kueue composes anything.
//
// THE ORDINAL IS WHAT TELLS TWO REPLICAS OF ONE ROLE APART, in this metadata and nowhere else: the
// group name is derived from it and the ordinal label carries it in plain digits, so a Pod's
// membership and its identity say the same thing twice, once for Kueue and once for the converger.
// The group name is NEVER parsed back -- the labels are the only things read back, and the name
// owes nothing to a reader beyond uniqueness.
//
// THE MEMBER INDEX DOES NOT ENTER THE GROUP NAME, and that is the whole point of the group: the
// members of one replica share a name because they are one admission, and what separates them is a
// label Kueue does not read. Folding the index into the name would give every member its own group
// of one, which is the shape this design exists to avoid.
//
// THE FAST-ADMISSION ANNOTATION IS NEVER SET, and its absence is asserted rather than assumed.
// Kueue's fast path takes the FIRST runnable Pod of the group, sets that PodSet's Count to the whole
// group's total, and returns. At a total of one it buys nothing; above one it is actively wrong,
// admitting a group of several on the strength of one member being runnable -- which is the
// opposite of the fate-sharing the total expresses. A test names it and states that.
func ModelDeploymentPodGroup(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, ordinal, member int,
) ModelDeploymentPodGroupMeta {
	labels := map[string]string{
		kueuepodconst.GroupNameLabel:       modelDeploymentReplicaGroupName(md, role.Name, ordinal),
		modelDeploymentReplicaOrdinalLabel: strconvx.Itoa(ordinal),
	}
	// THE MEMBER INDEX IS WRITTEN ONLY WHERE IT DISTINGUISHES SOMETHING, and the reason is that a
	// replica of one member renders the Pod it rendered before this label existed -- byte for byte,
	// which a test pins. Adding a label every single-Member replica would carry would move every
	// one of their fingerprints and roll every deployment on the cluster on the pass this landed,
	// to record an index that has exactly one possible value.
	//
	// Readers get that value anyway: modelDeploymentPodMemberIndex answers with the leader's index
	// when the label is absent, which is what the label would have said.
	if modelDeploymentRoleSize(role) > 1 {
		labels[modelDeploymentMemberIndexLabel] = strconvx.Itoa(member)
	}

	return ModelDeploymentPodGroupMeta{
		Labels: labels,
		Annotations: map[string]string{
			// THE TOTAL IS THE REPLICA'S SIZE, AND A REPLICA COUNT CHANGE NEVER MOVES IT: the group
			// is this replica and nobody else's, so adding or removing replicas adds or removes
			// whole groups rather than editing the total any running member carries. That is what
			// turns a resize into a trim rather than a rebuild, and it survives sizes above one
			// only because the size itself is frozen at creation -- a mutable size would put this
			// number back under two writers, which is the defect the per-replica split removed.
			kueuepodconst.GroupTotalCountAnnotation: strconvx.Itoa(modelDeploymentRoleSize(role)),
			// THE ROLE HASH IS LOAD-BEARING, NOT COSMETIC. Kueue reads this annotation verbatim when
			// present and otherwise derives a digest of the Pod spec's SHAPE -- containers,
			// nodeSelector, affinity, tolerations -- and an opaque digest names the PodSet after
			// nothing an operator wrote. Per-role flavor assignment and per-role status both join a
			// Workload's PodSets to the roles by this name, so a digest breaks the join while nothing
			// errors. Writing the role's own name here makes the PodSet identity the role's identity
			// by construction, which is also why that name is validated to Kueue's PodSetReference
			// pattern and to uniqueness.
			//
			// IT STAYS THE ROLE'S NAME AND DOES NOT GAIN THE ordinal. This is what Kueue groups
			// PodSets by and what status reads to attribute a Pod, so folding the ordinal in would
			// give every replica its own PodSet identity and break the join the annotation exists
			// to carry.
			kueuepodconst.RoleHashAnnotation: role.Name,
			// An inference deployment never finishes. Without this, Kueue applies BATCH semantics to
			// it: a Pod reaching Succeeded is reported as reclaimable and its quota is handed back
			// while the deployment is still meant to be serving.
			//
			// IT HAS A COST THE DEPARTURE PATHS PAY. Kueue reads a serving group as one that is
			// never finished, so it never releases the finalizer it holds on the group's Pods; only
			// the Workload being deleted does. deleteModelDeploymentGroupWorkload is what pays it,
			// and removing this annotation without removing that call would leak Workloads.
			kueuepodconst.GroupServingAnnotationKey: kueuepodconst.GroupServingAnnotationValue,
		},
	}
}

// modelDeploymentPodOrdinal reads the ordinal a replica was rendered at.
//
// IT REPORTS ABSENCE RATHER THAN GUESSING ZERO: a Pod carrying no ordinal label -- one rendered
// before the per-replica groups existed, or built by a hand -- is not any ordinal's to claim, and
// the converger treats such a Pod as outdated rather than adopting it onto an ordinal it never
// carried.
func modelDeploymentPodOrdinal(pod *core.Pod) (int, bool) {
	ordinal, err := strconvx.Atoi[int](pod.Labels[modelDeploymentReplicaOrdinalLabel])
	if err != nil || ordinal < 0 {
		return 0, false
	}

	return ordinal, true
}

// modelDeploymentPodMemberIndex reads which member of its replica a Pod was rendered as.
//
// IT DEFAULTS TO THE LEADER RATHER THAN REPORTING ABSENCE, which is the opposite of the ordinal
// above, and the asymmetry is deliberate. A missing ordinal means the Pod belongs to no replica the
// converger can name, so guessing one would adopt it onto a slot it never held. A missing member
// index means something narrower: every replica rendered before multi-Member groups existed has
// exactly one Pod, and that Pod is its replica's only member -- which is the leader. Reading it as
// the leader is therefore what the label would have said, and treating those Pods as unplaceable
// would turn every existing single-Member replica into a rollout on the pass this label landed.
func modelDeploymentPodMemberIndex(pod *core.Pod) int {
	member, err := strconvx.Atoi[int](pod.Labels[modelDeploymentMemberIndexLabel])
	if err != nil || member < 0 {
		return modelDeploymentLeaderMemberIndex
	}

	return member
}

// deleteModelDeploymentGroupWorkload deletes the Workload Kueue composed for this deployment's group,
// which is what releases the finalizer Kueue holds on every replica.
//
// IT IS THE TEARDOWN'S ONLY WAY OUT, not an optimization. The group is annotated as serving, so Kueue
// never reads it as finished and never finalizes its Pods on its own; the Workload being deleted is
// the single trigger that does. Since that Workload is owned by the very Pods it is holding, and
// owned without a controller reference, garbage collection cannot reach it either -- deleting the
// replicas alone leaves the deployment in Deleting with nothing erroring anywhere.
//
// Absence is success. A group short of its declared total composes no Workload at all, and a pass
// that runs after the previous one already deleted it finds nothing; both are the state this wants.
func (r *ModelDeploymentReconciler) deleteModelDeploymentGroupWorkload(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) error {
	wls, err := r.findModelDeploymentGroupWorkloads(ctx, md, pods)
	if err != nil {
		return err
	}

	// EVERY Workload owning these Pods, not the first. A deployment whose roles sit on different
	// instanceTypes has one Workload per group, and each holds Kueue's finalizer on its own replicas;
	// deleting one of them releases one group and leaves the others unable to leave at all.
	for _, wl := range wls {
		if err = r.Client.Delete(ctx, wl); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete workload %s: %w", wl.Name, err)
		}
	}

	return nil
}

// releaseModelDeploymentStrandedWorkloads deletes the Workload of a replica that is on its way out
// with no member of its group left standing.
//
// IT EXISTS BECAUSE A DEPARTURE THIS OPERATOR DID NOT INITIATE HAS NOTHING TO DELETE THE WORKLOAD.
// Every path this operator drives -- teardown, a role disappearing, a scale-down, a rollout
// replacement -- deletes the Workload of the replica it removes. A replica removed by a hand, a node
// drain or an eviction has no such path, and a serving group's finalizer is released by nothing but
// its Workload being deleted. Without this the Pod stays terminating behind the finalizer, its
// Workload stays admitted holding quota, the ordinal stays occupied, and the create gate never opens
// for it: no replacement is ever made and it takes a hand to recover.
//
// NEITHER TERMINAL PHASE RECOVERS ON ITS OWN, and they fail differently, which is why the remedy is
// here rather than at the create gate. A container killed for exceeding its grace period reaches
// Failed, which Kueue counts as INACTIVE, so a replacement created beside it would push the group
// over its total and Kueue would finalize the dead member -- that one could have been left to the
// create gate. A container that exits cleanly on SIGTERM reaches Succeeded, which Kueue counts as
// ACTIVE, so the same replacement is the excess member and Kueue deletes the REPLACEMENT instead,
// again and again. Deleting the Workload is the one remedy that covers both.
//
// THE LIVE MEMBERS ARE WHAT MAKE IT SAFE. Kueue answers a deleted Workload by stopping the whole
// group, so this deletes only a Workload that owns no Pod still standing; a group with a member
// still serving is left alone even while a sibling of it drains. One replica per group is the shape
// this operator renders, which is what makes the two readings coincide here, but the condition is
// written about the members rather than about the count so that a group ever holding more than one
// is not stopped by this path.
//
// Absence is success, on the terms deleteModelDeploymentGroupWorkload states: a pass that runs after
// a previous one already deleted the Workload finds nothing, and that is the state this wants.
func (r *ModelDeploymentReconciler) releaseModelDeploymentStrandedWorkloads(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) error {
	departing := make([]core.Pod, 0, len(pods))
	standing := sets.New[types.UID]()
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			departing = append(departing, pods[i])

			continue
		}
		standing.Insert(pods[i].UID)
	}
	if len(departing) == 0 {
		return nil
	}

	wls, err := r.findModelDeploymentGroupWorkloads(ctx, md, departing)
	if err != nil {
		return err
	}

	for _, wl := range wls {
		if modelDeploymentWorkloadOwnsAny(wl, standing) {
			continue
		}
		if err = r.Client.Delete(ctx, wl); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete stranded workload %s: %w", wl.Name, err)
		}
	}

	return nil
}

// modelDeploymentReplicaGroupName derives the Kueue group name of one replica: the replica of the
// given role at the given ordinal.
//
// THE NAME IS ALWAYS THE HASHED FORM, and that is a decision rather than an escape. Kueue names a
// group's Workload after the group VERBATIM, and a Workload is an object name -- a lowercase RFC
// 1123 subdomain -- so the group name owes that charset whatever a readable composite would have
// bought; a separator the charset forbids has no readable form to offer, and a separator it allows
// (the hyphen) cannot decompose, which was never needed anyway. The hash covers NAMESPACE, NAME,
// ROLE AND ORDINAL: the group name is only ever compared with other Pods' -- in one namespace, but
// two deployments of one name in two namespaces must not read as one group.
//
// THE NAME IS UNIQUE, NOT PARSEABLE, and the difference is the point: nothing ever derives the
// ordinal back from it. The ordinal travels in its own label, and the name owes a reader uniqueness
// and nothing else -- which is also why the derivation has no shape that varies with how many roles
// the deployment declares: a first role's replica names its group the same way whether it is the
// only role or one of several.
func modelDeploymentReplicaGroupName(
	md *workercore.ModelDeployment, role string, ordinal int,
) string {
	return modelDeploymentPodGroupNamePrefix + stringx.SumByFNV64a(
		md.Namespace, "/", md.Name, "/", role, "/", strconvx.Itoa(ordinal))
}

// modelDeploymentReplicaServiceName is the headless Service that publishes one replica's members,
// and the subdomain every one of those members names.
//
// IT IS READABLE RATHER THAN HASHED, unlike the Kueue group name above, and the reason is who reads
// it. A group name is compared and never shown; this one is half of an address a person debugging a
// collective types, and it is also what an engine's logs quote when a rank cannot reach its leader.
// The cost of readability is a length budget, since a Service name is a DNS-1035 label of 63
// characters. Nothing here enforces it: the budget is checked at ADMISSION, against the longest name
// the role's declared counts can produce, so a deployment whose members could not be named is
// refused rather than rendered into objects the API server rejects one at a time.
//
// IT EXISTS ONLY ABOVE SIZE ONE. A replica of one member has nobody to address, so rendering a
// Service for it would create an object per replica for no consumer.
func modelDeploymentReplicaServiceName(
	md *workercore.ModelDeployment, role string, ordinal int,
) string {
	return md.Name + "-" + role + "-r" + strconvx.Itoa(ordinal)
}

// modelDeploymentMemberName is the name of one member of one replica, which is also its hostname and
// therefore half of the DNS record its siblings resolve it by.
//
// THE MEMBER INDEX IS LAST SO THE LEADER IS RECOGNIZABLE, and it counts from zero so that member
// zero is the leader of every replica by construction. Nothing elects it and nothing stores the
// decision: a reader with the deployment, the role and the ordinal can write down the leader's
// address without consulting the cluster, which is what makes the address available to a container
// before any member of the group has started.
//
// THE NAME IS NEVER PARSED BACK. The ordinal and the member index each travel in a label of their
// own, and this string owes a reader uniqueness and recognizability rather than structure.
func modelDeploymentMemberName(
	md *workercore.ModelDeployment, role string, ordinal, member int,
) string {
	return modelDeploymentReplicaServiceName(md, role, ordinal) + "-m" + strconvx.Itoa(member)
}

// modelDeploymentPodGroupNameOf is a role's name in the group-name-keyed shapes the status paths
// still consume: a per-role join key, derived the same way a replica's group name is but without the
// ordinal, and carried by no Pod.
//
// IT IS ALWAYS THE HASHED FORM, on the same terms modelDeploymentReplicaGroupName states, and for a
// second reason that one does not have: a derivation whose shape varied with the number of roles
// would rename the first role's key when a second role arrived, and a key that moves under its
// consumers is worse than one that was always opaque.
func modelDeploymentPodGroupNameOf(md *workercore.ModelDeployment, role string) string {
	return modelDeploymentPodGroupNamePrefix +
		stringx.SumByFNV64a(md.Namespace, "/", md.Name, "/", role)
}
