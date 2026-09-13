package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// NO LABEL OF OUR OWN CARRIES THE ROLE, and that is a decision rather than an omission.
// app.kubernetes.io/component already holds the role's name on every replica: it is in the selector
// labels the Service selects on, and it is what modelDeploymentPodRole reads to attribute a Pod to a
// role in status. A second selectable carrier would be two answers to one question, written by two
// functions, agreeing today -- and the one that drifted would be the one nothing reads on the
// failure path, whose symptom is a Service with no endpoints or a role reporting nobody ready.
//
// A key in this project's own domain stays available: adding one later is additive, while removing
// one already published breaks whoever selected on it.

const (
	// modelDeploymentPodGroupNamePrefix marks a group name this operator derived rather than took
	// verbatim. It is the escape nodefeature.FormatLocalQueueName takes for an over-long
	// ClusterQueue name, spelled the same way on purpose so one form is recognizable everywhere.
	//
	// That function is not called: its subject is a ClusterQueue name, and reusing it here would
	// make a group's identity read as a queue's. What is shared is the shape, not the derivation.
	modelDeploymentPodGroupNamePrefix = "gpustack-fnv64-"

	// modelDeploymentRoleReplicasAnnotation carries the replica count the role declared when this
	// Pod was built, which is the half of the group's shape the total cannot express.
	//
	// The total alone is not a fingerprint of the shape. Moving prefill 2 / decode 2 to prefill 1 /
	// decode 3 leaves it at four, so a predicate reading only the sum sees no change and lets the
	// converge loop trim one Pod and add another IN THE SAME PASS -- producing exactly the mixed
	// group the rebuild exists to avoid, and reaching a correct end state only because the next pass
	// notices a terminating replica.
	//
	// A REPLICA THAT PREDATES IT COUNTS AS DISAGREEING, so the first pass after this ships rebuilds
	// every existing group once. That is accepted rather than overlooked. The alternative is
	// backfilling the annotation in place, which is a mutation path whose only job is to serve
	// deployments that exist solely on a branch — ModelDeployment is in no released version — and the
	// bound is one rebuild, once. It is also the same treatment the group-total annotation already
	// gives a replica that predates it.
	modelDeploymentRoleReplicasAnnotation = "modeldeployment." + systemname.LabelPrefix + "role-replicas"
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

// modelDeploymentPodGroupSpec is one scheduling group: the roles naming one instanceType, the name
// their Pods share, and the total Kueue waits for before it composes a Workload.
type modelDeploymentPodGroupSpec struct {
	// InstanceType is the grouping key, and the reason the group exists at all.
	InstanceType string

	// Name is what every Pod of this group carries in the membership label.
	Name string

	// TotalCount sums the replicas of THIS group's roles and of no others. A group claiming the
	// deployment-wide total waits for Pods that are never coming.
	TotalCount int32
}

// modelDeploymentPodGroups returns the deployment's scheduling groups, one per distinct
// instanceType.
//
// It is PURE: same deployment, same result, no client and no clock.
//
// THE SPLIT IS FORCED RATHER THAN CHOSEN. A queue name is derived from the instanceType and one
// Workload carries one queue name, so two instanceTypes cannot be one Workload however much one
// deployment they are.
//
// THE GROUPING KEY IS THE instanceType, NOT THE ROLE. Two roles on one type are two PodSets of ONE
// group; a third on another type is a second group. Keying on the role instead would produce three,
// each separately admitted, and the atomicity a group exists for would cover one role at a time.
//
// THE ORDER IS THE ROLES' ORDER, first mention winning, so two passes over an unchanged spec return
// the same names in the same places. A map's iteration order would not, and the group name is
// written into Pods.
func modelDeploymentPodGroups(md *workercore.ModelDeployment) []modelDeploymentPodGroupSpec {
	groups := make([]modelDeploymentPodGroupSpec, 0, 1)
	at := make(map[string]int, 1)

	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]

		idx, seen := at[role.InstanceType]
		if !seen {
			idx = len(groups)
			at[role.InstanceType] = idx
			groups = append(groups, modelDeploymentPodGroupSpec{InstanceType: role.InstanceType})
		}
		groups[idx].TotalCount += role.Replicas
	}

	sole := len(groups) == 1
	for i := range groups {
		groups[i].Name = modelDeploymentPodGroupNameOf(md, groups[i].InstanceType, sole)
	}

	return groups
}

// modelDeploymentPodGroupFor returns the group a given instanceType forms within this deployment.
func modelDeploymentPodGroupFor(
	md *workercore.ModelDeployment, instanceType string,
) modelDeploymentPodGroupSpec {
	groups := modelDeploymentPodGroups(md)
	for i := range groups {
		if groups[i].InstanceType == instanceType {
			return groups[i]
		}
	}

	// A type no role names forms no group. The groups are derived from the roles and every caller
	// passes the type one of them named, so the empty spec is what "there is no such group" means
	// rather than a case to handle.
	return modelDeploymentPodGroupSpec{InstanceType: instanceType}
}

// ModelDeploymentPodGroup returns the group metadata for one role's replicas.
//
// It is PURE: same deployment, same role, same result, no client and no clock. Every Pod of every
// role sharing one instanceType joins ONE Kueue pod group, and Kueue then builds one Workload whose
// PodSets are those roles and admits it as a unit -- which is the whole point of the group.
//
// IT TAKES NO ORDINAL, and that is a statement rather than an omission: nothing here varies with the
// replica. The group's identity is the deployment's and the PodSet's identity is the role's, so two
// replicas of one role are deliberately indistinguishable to Kueue -- they are two Pods of one
// PodSet. The ordinal lives in the Pod's NAME, where the reconciler needs it to decide which
// replicas a scale-down removes.
//
// THE FAST-ADMISSION ANNOTATION IS NEVER SET, and its absence is asserted rather than assumed.
// Kueue's fast path takes the FIRST runnable Pod of the group, sets that single PodSet's Count to
// the whole group's total, and returns -- one PodSet then carries every role's Pods, the per-role
// split this design exists to create is erased, and with it per-role flavor assignment. The
// annotation is a trap for exactly this design, which is why a test names it and states that.
func ModelDeploymentPodGroup(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) ModelDeploymentPodGroupMeta {
	group := modelDeploymentPodGroupFor(md, role.InstanceType)

	return ModelDeploymentPodGroupMeta{
		Labels: map[string]string{
			kueuepodconst.GroupNameLabel: group.Name,
		},
		Annotations: map[string]string{
			kueuepodconst.GroupTotalCountAnnotation: strconvx.Itoa(int(group.TotalCount)),
			// THE ROLE HASH IS LOAD-BEARING, NOT COSMETIC. Kueue reads this annotation verbatim when
			// present and otherwise derives a digest of the Pod spec's SHAPE -- containers,
			// nodeSelector, affinity, tolerations.
			//
			// Two roles that happen to render identically -- same image, same request, and now
			// necessarily the same instanceType, since a differing one puts them in different groups
			// -- would derive the same digest and collapse into ONE PodSet of both their replicas.
			// Per-role counting, per-role flavor assignment and per-role status all disappear at that
			// point, with nothing erroring. Writing the role's own name here makes the PodSet
			// identity the role's identity by construction, which is also why that name is validated
			// to Kueue's PodSetReference pattern and to uniqueness.
			//
			// IT STAYS THE ROLE'S NAME AND DOES NOT GAIN THE instanceType. This is what Kueue groups
			// PodSets by and what status reads to attribute a Pod, so folding the type in would
			// change PodSet identity and break both. The type is carried in the group name instead.
			kueuepodconst.RoleHashAnnotation: role.Name,
			// An inference deployment never finishes. Without this, Kueue applies BATCH semantics to
			// it: a Pod reaching Succeeded is reported as reclaimable and its quota is handed back
			// while the deployment is still meant to be serving.
			//
			// IT HAS A COST THE TEARDOWN PATH PAYS. Kueue reads a serving group as one that is never
			// finished, so it never releases the finalizer it holds on the group's Pods; only the
			// Workload being deleted does. deleteModelDeploymentGroupWorkload is what pays it, and
			// removing this annotation without removing that call would leak Workloads.
			kueuepodconst.GroupServingAnnotationKey: kueuepodconst.GroupServingAnnotationValue,
			// Ours rather than Kueue's, and the only entry here Kueue does not read. It records what
			// the total cannot: which share of the group this role declared.
			modelDeploymentRoleReplicasAnnotation: strconvx.Itoa(int(role.Replicas)),
		},
	}
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

// modelDeploymentGroupIsResizing reports whether any Pod the deployment still owns declares a group
// total other than the one the spec now asks for.
//
// It is the predicate the rebuild policy turns on. A change to any role's replicas, or to the set of
// roles, moves the total that EVERY Pod of the group carries, and Kueue refuses to compose a
// Workload for a group whose Pods disagree on it -- unretryably, with no Workload and no condition
// naming the cause.
//
// A TERMINATING POD STILL COUNTS. It remains a member of the group until it is actually gone, so
// asking only about the survivors would let a new replica be created beside one that has merely been
// asked to leave, which is the mixed state this predicate exists to keep the reconciler out of.
//
// A Pod carrying no total at all counts as disagreeing: it predates the group and cannot be joined
// to one, so it is replaced rather than adopted.
// THE TOTAL IS NOT ENOUGH ON ITS OWN, so the per-role share is compared beside it. Moving prefill 2
// / decode 2 to prefill 1 / decode 3 leaves the total at four: a sum-only predicate answers no, and
// the converge loop then trims one replica and creates another in the SAME pass, which is the mixed
// group this predicate exists to keep it out of. A role RENAMED without changing any count moves
// neither number, and is caught by the same comparison finding no entry for the old role's name.
// THE TOTAL A POD IS JUDGED AGAINST IS ITS OWN GROUP'S, not the deployment's. With two groups the
// deployment-wide sum matches neither of them, so a predicate reading it answers "resizing" on every
// pass forever -- a rebuild loop rather than a wrong number, and one that nothing reports.
//
// THE ANSWER IS A SET OF GROUPS RATHER THAN A YES. One group's shape moving is no reason to tear down
// a group that did not move, and a boolean cannot say which is which: every role of the deployment
// would restart because one role's replica count changed.
//
// A POD IS JUDGED AGAINST THE GROUP IT ACTUALLY JOINED, read off its own membership label, AND the
// group its role belongs to now is named beside it. A role moved onto another instanceType leaves one
// group short and arrives in another that never had it. Both have to come down, and neither name is
// derivable from the other: one is written on the Pod, the other is in the spec.
func modelDeploymentGroupsResizing(
	md *workercore.ModelDeployment, pods []core.Pod,
) sets.Set[string] {
	wantByRole := make(map[string]string, len(md.Spec.Roles))
	shareByRole := make(map[string]string, len(md.Spec.Roles))
	groupByRole := make(map[string]string, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		group := modelDeploymentPodGroupFor(md, role.InstanceType)
		groupByRole[role.Name] = group.Name
		wantByRole[role.Name] = strconvx.Itoa(int(group.TotalCount))
		shareByRole[role.Name] = strconvx.Itoa(int(role.Replicas))
	}

	resizing := sets.New[string]()
	for i := range pods {
		joined := pods[i].Labels[kueuepodconst.GroupNameLabel]
		name := modelDeploymentPodRole(&pods[i])

		want, named := wantByRole[name]
		if !named {
			// Its role is gone -- renamed, or removed -- so it belongs to no group the spec now
			// forms. The group it is sitting in is the one that has to come down.
			resizing.Insert(joined)

			continue
		}

		if pods[i].Annotations[kueuepodconst.GroupTotalCountAnnotation] != want ||
			pods[i].Annotations[modelDeploymentRoleReplicasAnnotation] != shareByRole[name] {
			resizing.Insert(joined, groupByRole[name])
		}
	}

	return resizing
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

// modelDeploymentPodGroupNameOf is a group's identity, shared by every Pod of every role naming one
// instanceType.
//
// A DEPLOYMENT WITH ONE GROUP KEEPS THE NAME IT ALWAYS HAD, which is compatibility and also the
// better name: see modelDeploymentPodGroupName on why a readable one is worth having.
//
// A DEPLOYMENT WITH SEVERAL HASHES ALL OF THEM, the type included. A readable composite such as
// "<name>-<type>" would share a namespace with deployment names and could equal one -- a deployment
// "a" with a type "b-c" and a deployment "a-b" with a type "c" write the same label, and Kueue then
// reads their replicas as one group. The hashed form carries the prefix that already marks a derived
// name, so the two spaces never meet.
//
// WHICH SHAPE A GROUP GETS DEPENDS ON HOW MANY THE DEPLOYMENT HAS, so gaining a second instanceType
// would rename the first group and rebuild it. That is unreachable rather than tolerated: the set of
// roles and each role's instanceType are frozen after creation. If that freeze is ever relaxed, this
// is one of the places that stops being safe.
func modelDeploymentPodGroupNameOf(
	md *workercore.ModelDeployment, instanceType string, sole bool,
) string {
	if sole {
		return modelDeploymentPodGroupName(md)
	}

	return modelDeploymentPodGroupNamePrefix +
		stringx.SumByFNV64a(md.Namespace, "/", md.Name, "/", instanceType)
}

// modelDeploymentPodGroupName is the identity of a deployment's ONLY group.
//
// It is the deployment's own name when that is a valid label value, because this label is the first
// thing an operator greps for and a hash tells them nothing. An over-long name -- an object name may
// run to 253 characters while a label value stops at 63 -- falls back to the hashed form.
//
// The hash covers NAMESPACE AND NAME, not the name alone: the group name is only ever compared with
// other Pods', and two deployments of the same name in two namespaces must not be read as one group.
func modelDeploymentPodGroupName(md *workercore.ModelDeployment) string {
	if len(validation.IsValidLabelValue(md.Name)) == 0 {
		return md.Name
	}

	return modelDeploymentPodGroupNamePrefix + stringx.SumByFNV64a(md.Namespace, "/", md.Name)
}
