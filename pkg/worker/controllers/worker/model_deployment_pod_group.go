package worker

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
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

// ModelDeploymentPodGroup returns the group metadata for one role's replicas.
//
// It is PURE: same deployment, same role, same result, no client and no clock. Every Pod of every
// role of one deployment joins ONE Kueue pod group, and Kueue then builds one Workload whose PodSets
// are the roles and admits it as a unit -- which is the whole point of the group.
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
	return ModelDeploymentPodGroupMeta{
		Labels: map[string]string{
			kueuepodconst.GroupNameLabel: modelDeploymentPodGroupName(md),
		},
		Annotations: map[string]string{
			kueuepodconst.GroupTotalCountAnnotation: strconvx.Itoa(int(modelDeploymentPodGroupTotalCount(md))),
			// THE ROLE HASH IS LOAD-BEARING, NOT COSMETIC. Kueue reads this annotation verbatim when
			// present and otherwise derives a digest of the Pod spec's SHAPE -- containers,
			// nodeSelector, affinity, tolerations. Two roles that happen to render identically, same
			// image and same request and same (or no) acceleratorKey, would derive the same digest
			// and collapse into ONE PodSet of both their replicas. Per-role counting, per-role flavor
			// assignment and per-role status all disappear at that point, with nothing erroring.
			// Writing the role's own name here makes the PodSet identity the role's identity by
			// construction, which is also why that name is validated to Kueue's PodSetReference
			// pattern and to uniqueness.
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
	wl, err := r.findModelDeploymentGroupWorkload(ctx, md, pods)
	if err != nil {
		return err
	}
	if wl == nil {
		return nil
	}

	if err = r.Client.Delete(ctx, wl); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete workload %s: %w", wl.Name, err)
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
func modelDeploymentGroupIsResizing(md *workercore.ModelDeployment, pods []core.Pod) bool {
	want := strconvx.Itoa(int(modelDeploymentPodGroupTotalCount(md)))

	perRole := make(map[string]string, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		perRole[md.Spec.Roles[i].Name] = strconvx.Itoa(int(md.Spec.Roles[i].Replicas))
	}

	for i := range pods {
		if pods[i].Annotations[kueuepodconst.GroupTotalCountAnnotation] != want {
			return true
		}

		declared, named := perRole[modelDeploymentPodRole(&pods[i])]
		if !named || pods[i].Annotations[modelDeploymentRoleReplicasAnnotation] != declared {
			return true
		}
	}

	return false
}

// modelDeploymentPodGroupTotalCount is how many Pods the group declares: the sum of every role's
// replicas.
//
// Kueue will not compose a Workload until it has seen this many runnable Pods, so the number is what
// makes admission all-or-nothing rather than a property anything enforces.
//
// Replicas is summed VERBATIM rather than defaulted to one, matching how the reconciler and the
// status builder read it. The schema defaults the field and bounds it at one, so a zero only reaches
// here from a value built in Go -- and a total that disagreed with the number of Pods the reconciler
// then creates from the same field would be worse than a zero.
func modelDeploymentPodGroupTotalCount(md *workercore.ModelDeployment) int32 {
	var total int32
	for i := range md.Spec.Roles {
		total += md.Spec.Roles[i].Replicas
	}

	return total
}

// modelDeploymentPodGroupName is the group's identity, shared by every Pod of every role.
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
