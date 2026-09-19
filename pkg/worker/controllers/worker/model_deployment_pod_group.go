package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
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

// ModelDeploymentPodGroup returns the group metadata for ONE replica of a role: the replica at the
// given ordinal.
//
// It is PURE: same deployment, role, ordinal, same result, no client and no clock. Every replica
// joins a Kueue pod group of its own -- one member, one Workload, one admission -- which is what
// makes a scale or a replacement a change to that replica alone rather than a change every sibling
// has to agree on before Kueue composes anything.
//
// THE ORDINAL IS WHAT TELLS TWO REPLICAS OF ONE ROLE APART, in this metadata and nowhere else: the
// group name is derived from it and the ordinal label carries it in plain digits, so a Pod's
// membership and its identity say the same thing twice, once for Kueue and once for the converger.
// The group name is NEVER parsed back -- the ordinal label is the only thing read back, and the name
// owes nothing to a reader beyond uniqueness.
//
// THE FAST-ADMISSION ANNOTATION IS NEVER SET, and its absence is asserted rather than assumed.
// Kueue's fast path takes the FIRST runnable Pod of the group, sets that PodSet's Count to the whole
// group's total, and returns -- with a total of one the fast path buys nothing, and the annotation
// remains a trap for the day a group grows a second member. A test names it and states that.
func ModelDeploymentPodGroup(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, ordinal int,
) ModelDeploymentPodGroupMeta {
	return ModelDeploymentPodGroupMeta{
		Labels: map[string]string{
			kueuepodconst.GroupNameLabel:       modelDeploymentReplicaGroupName(md, role.Name, ordinal),
			modelDeploymentReplicaOrdinalLabel: strconvx.Itoa(ordinal),
		},
		Annotations: map[string]string{
			// THE TOTAL IS ONE AND STAYS ONE: the group is this replica and nobody else's, so a
			// replica count change moves no total any member carries. That is what turns a resize
			// into a trim rather than a rebuild.
			kueuepodconst.GroupTotalCountAnnotation: "1",
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

// modelDeploymentPodsInGroup selects the replicas carrying one group's membership label.
func modelDeploymentPodsInGroup(pods []core.Pod, group string) []core.Pod {
	var members []core.Pod
	for i := range pods {
		if pods[i].Labels[kueuepodconst.GroupNameLabel] == group {
			members = append(members, pods[i])
		}
	}

	return members
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
