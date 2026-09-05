package worker

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// podGroupDeployment builds a two-role deployment, prefill 2 and decode 2, which is the shape every
// case below varies from.
func podGroupDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen-72b", Namespace: "team-a"},
		Spec: workercore.ModelDeploymentSpec{
			Roles: []workercore.ModelDeploymentRole{
				{Name: "prefill", Replicas: 2, InstanceType: "h20-8x"},
				{Name: "decode", Replicas: 2, InstanceType: "h20-8x"},
			},
		},
	}
	for _, m := range mutate {
		m(md)
	}

	return md
}

// TestModelDeploymentPodGroup covers what every Pod of the group carries.
func TestModelDeploymentPodGroup(t *testing.T) {
	md := podGroupDeployment()

	prefill := ModelDeploymentPodGroup(md, &md.Spec.Roles[0])
	decode := ModelDeploymentPodGroup(md, &md.Spec.Roles[1])

	assert.Equal(t, "qwen-72b", prefill.Labels[kueuepodconst.GroupNameLabel])
	assert.Equal(t, prefill.Labels[kueuepodconst.GroupNameLabel],
		decode.Labels[kueuepodconst.GroupNameLabel],
		"one deployment is one group: two roles disagreeing on the name are two groups, and neither "+
			"reaches its declared total")

	assert.Equal(t, "4", prefill.Annotations[kueuepodconst.GroupTotalCountAnnotation],
		"the total is over the whole deployment, not over the role")
	assert.Equal(t, prefill.Annotations[kueuepodconst.GroupTotalCountAnnotation],
		decode.Annotations[kueuepodconst.GroupTotalCountAnnotation])

	assert.Equal(t, "prefill", prefill.Annotations[kueuepodconst.RoleHashAnnotation])
	assert.Equal(t, "decode", decode.Annotations[kueuepodconst.RoleHashAnnotation])

	// The group adds NO label of its own naming the role. The renderer's selector labels already
	// carry it, so a second selectable carrier would be a second answer to one question -- asserted
	// here rather than left implicit, because adding one is the kind of edit that looks harmless.
	assert.Equal(t, []string{kueuepodconst.GroupNameLabel}, slices.Sorted(maps.Keys(prefill.Labels)),
		"the group contributes exactly one label: membership")

	assert.Equal(t, kueuepodconst.GroupServingAnnotationValue,
		prefill.Annotations[kueuepodconst.GroupServingAnnotationKey],
		"an inference deployment never finishes; without this Kueue reclaims the quota of a Pod "+
			"that reached Succeeded while the deployment is still meant to be serving")
}

// TestModelDeploymentPodGroup_RoleHashIsTheRoleName is the case that would pass by accident.
//
// The two roles here differ ONLY in name, which is exactly the input that makes Kueue's derived role
// hash -- a digest of the Pod spec's shape -- identical for both. Without the annotation they would
// collapse into one PodSet of four Pods, and per-role counting, per-role flavor assignment and
// per-role status would all disappear with nothing erroring. Asserting two different values on two
// identically-shaped roles is what makes the annotation's presence load-bearing rather than assumed.
func TestModelDeploymentPodGroup_RoleHashIsTheRoleName(t *testing.T) {
	md := podGroupDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles = []workercore.ModelDeploymentRole{
			{Name: "left", Replicas: 2, InstanceType: "h20-8x"},
			{Name: "right", Replicas: 2, InstanceType: "h20-8x"},
		}
	})

	left := ModelDeploymentPodGroup(md, &md.Spec.Roles[0])
	right := ModelDeploymentPodGroup(md, &md.Spec.Roles[1])

	assert.NotEqual(t,
		left.Annotations[kueuepodconst.RoleHashAnnotation],
		right.Annotations[kueuepodconst.RoleHashAnnotation],
		"two roles whose Pod specs are identical must still be two PodSets")
}

// TestModelDeploymentPodGroup_FastAdmissionIsAbsent asserts an ABSENCE, and the absence is the point.
//
// Setting kueue.x-k8s.io/pod-group-fast-admission makes Kueue compose the Workload from the FIRST
// runnable Pod alone, giving that single PodSet the whole group's total count. Every role then
// collapses into one PodSet: the per-role split this spec exists to create is erased, and per-role
// flavor assignment -- which is what lets prefill and decode land on two accelerator models -- goes
// with it. Nothing errors, and the Workload looks well formed.
func TestModelDeploymentPodGroup_FastAdmissionIsAbsent(t *testing.T) {
	md := podGroupDeployment()

	group := ModelDeploymentPodGroup(md, &md.Spec.Roles[0])

	assert.NotContains(t, group.Annotations, kueuepodconst.GroupFastAdmissionAnnotationKey,
		"setting %q collapses every role into one PodSet and erases per-role flavor assignment",
		kueuepodconst.GroupFastAdmissionAnnotationKey)
	assert.NotContains(t, group.Labels, kueuepodconst.GroupFastAdmissionAnnotationKey,
		"and it must not arrive as a label either")
}

// TestModelDeploymentPodGroupName covers both forms of the group's identity.
func TestModelDeploymentPodGroupName(t *testing.T) {
	// 64 characters: one past what a label value takes, and well within what an object name does.
	overLong := strings.Repeat("a", 64)

	testCases := []struct {
		name   string
		md     *workercore.ModelDeployment
		want   string
		hashed bool
	}{
		{
			// The readable form is kept where it fits, because this label is the first thing an
			// operator greps for.
			name: "name_fits_a_label_value",
			md:   podGroupDeployment(),
			want: "qwen-72b",
		},
		{
			name: "name_too_long_for_a_label_value",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Name = overLong
			}),
			hashed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := modelDeploymentPodGroupName(tc.md)

			assert.Empty(t, validation.IsValidLabelValue(got),
				"whichever form is taken, the result must be a legal label value: it is a label")

			if !tc.hashed {
				assert.Equal(t, tc.want, got)

				return
			}

			assert.True(t, strings.HasPrefix(got, modelDeploymentPodGroupNamePrefix),
				"an over-long name falls back to the hashed form, got %q", got)
			assert.NotContains(t, got, tc.md.Name)
		})
	}
}

// TestModelDeploymentPodGroupName_HashCoversTheNamespace pins what the hash is taken over.
//
// The group name is only ever compared against other Pods' group names, so two deployments sharing a
// name in two namespaces must not hash alike -- their Pods would read as one group, and each would
// then be short of a total that counts the other's replicas.
func TestModelDeploymentPodGroupName_HashCoversTheNamespace(t *testing.T) {
	overLong := strings.Repeat("a", 64)

	here := podGroupDeployment(func(md *workercore.ModelDeployment) { md.Name = overLong })
	there := podGroupDeployment(func(md *workercore.ModelDeployment) {
		md.Name, md.Namespace = overLong, "team-b"
	})

	assert.NotEqual(t, modelDeploymentPodGroupName(here), modelDeploymentPodGroupName(there))
}

// TestModelDeploymentPodGroupTotalCount covers the sum the group declares.
func TestModelDeploymentPodGroupTotalCount(t *testing.T) {
	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
		want int32
	}{
		{
			name: "two_roles",
			md:   podGroupDeployment(),
			want: 4,
		},
		{
			name: "uneven_roles",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = 3
			}),
			want: 5,
		},
		{
			// The single-role shape, which must keep declaring exactly its own replicas.
			name: "one_role",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles = md.Spec.Roles[:1]
			}),
			want: 2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, modelDeploymentPodGroupTotalCount(tc.md))
		})
	}
}

// TestDeleteModelDeploymentGroupWorkload covers the delete that lets a teardown finish.
//
// The behavior it guards is not visible in this package at all: Kueue holds a finalizer on every Pod
// of a group it manages and releases it only when the group finishes or the Workload is deleted, and
// a group annotated as SERVING -- which every group here is -- never finishes. The Workload's only
// owners are those same Pods, and they own it without a controller reference, so garbage collection
// waits for them too. Deleting the replicas and waiting, which is what the teardown did before this,
// therefore waits forever with nothing erroring: measured on a live cluster, the deployment sat in
// Deleting with four Failed Pods and an admitted Workload for as long as it was left there.
//
// So the assertion is on the WORKLOAD SET that survives, not on an error: every failure mode of this
// function is a Workload that is still there, or somebody else's that is not.
func TestDeleteModelDeploymentGroupWorkload(t *testing.T) {
	md := newRenderDeployment()
	ours := readyReplica(md, 0, true)
	stranger := types.UID("uid-not-ours")

	// A Workload owning one of the Pods this pass observed, and one owning nobody we rendered.
	groupWL := func() *kueue.Workload {
		wl := &kueue.Workload{}
		wl.Name, wl.Namespace = "group-wl", md.Namespace
		wl.OwnerReferences = []meta.OwnerReference{
			{APIVersion: "v1", Kind: "Pod", Name: ours.Name, UID: ours.UID},
		}

		return wl
	}
	// THE NAME SORTS BEFORE THE GROUP'S, AND THAT IS THE WHOLE TEST. The lookup takes the
	// lowest-named candidate, so a filter that matched every Workload would still land on "group-wl"
	// if this one were named further down the alphabet -- and the case below would pass against a
	// deployment that deletes its neighbours' Workloads. Measured: with the ownership check stubbed to
	// true, the earlier name "someone-elses-wl" left every case green.
	foreignWL := func() *kueue.Workload {
		wl := &kueue.Workload{}
		wl.Name, wl.Namespace = "aaa-someone-elses-wl", md.Namespace
		wl.OwnerReferences = []meta.OwnerReference{
			{APIVersion: "v1", Kind: "Pod", Name: "theirs", UID: stranger},
		}

		return wl
	}

	testCases := []struct {
		name    string
		objects []ctrlcli.Object
		pods    []core.Pod
		want    []string
		why     string
	}{
		{
			name:    "the group's workload is deleted",
			objects: []ctrlcli.Object{groupWL()},
			pods:    []core.Pod{*ours},
			want:    nil,
			why:     "this delete is the only trigger that releases Kueue's finalizer on the replicas",
		},
		{
			name:    "a workload owning nobody of ours survives",
			objects: []ctrlcli.Object{groupWL(), foreignWL()},
			pods:    []core.Pod{*ours},
			want:    []string{"aaa-someone-elses-wl"},
			why:     "one namespace holds every tenant's Workloads and only ours is ours to delete",
		},
		{
			name: "observing no pods deletes nothing",
			// The last pass already saw the replicas leave. Deleting on an empty observation would
			// mean deleting by name-matching instead, which is what the ownership scan replaces.
			objects: []ctrlcli.Object{groupWL()},
			pods:    nil,
			want:    []string{"group-wl"},
			why:     "with no Pod to trace ownership from there is no group to identify",
		},
		{
			name:    "nothing to delete is not an error",
			objects: nil,
			pods:    []core.Pod{*ours},
			want:    nil,
			why:     "a group short of its total composes no Workload, and a repeat pass finds none",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cli := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).WithObjects(tc.objects...).Build()
			r := &ModelDeploymentReconciler{Client: cli}

			require.NoError(t,
				r.deleteModelDeploymentGroupWorkload(context.Background(), md, tc.pods), tc.why)

			list := new(kueue.WorkloadList)
			require.NoError(t, cli.List(context.Background(), list))

			left := make([]string, 0, len(list.Items))
			for i := range list.Items {
				left = append(left, list.Items[i].Name)
			}
			assert.ElementsMatch(t, tc.want, left, tc.why)
		})
	}
}
