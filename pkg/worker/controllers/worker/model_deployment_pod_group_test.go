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
	"k8s.io/apimachinery/pkg/util/sets"
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
		"the total is over the group, not over the role -- and these two roles share one instanceType, "+
			"so the group is the whole deployment")
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

// TestModelDeploymentPodGroupTotalCount covers the sum a group declares.
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
		{
			// Replicas is summed VERBATIM rather than defaulted to one: the schema defaults and
			// bounds the field, so a zero reaches here only from a value built in Go -- and a total
			// disagreeing with the number of Pods the reconciler creates from the same field would be
			// worse than a zero.
			name: "a_zero_is_summed_as_written",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[1].Replicas = 0
			}),
			want: 2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			groups := modelDeploymentPodGroups(tc.md)

			require.Len(t, groups, 1, "every role here names one instanceType")
			assert.Equal(t, tc.want, groups[0].TotalCount)
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

// twoTypeDeployment is prefill 2 and decode 3 on two instanceTypes, the shape the split exists for.
func twoTypeDeployment(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := podGroupDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[1].Replicas = 3
		md.Spec.Roles[1].InstanceType = "a100-8x"
	})
	for _, m := range mutate {
		m(md)
	}

	return md
}

// TestModelDeploymentPodGroups covers the set of groups a deployment forms.
//
// THE GROUPING KEY IS THE instanceType AND NOT THE ROLE, and the three-role case is what says so: an
// implementation keyed on the role passes every other case here and produces three groups for it.
//
// THE SINGLE-TYPE CASE IS A REGRESSION BASELINE, not evidence of the feature. It passes against the
// code that had no notion of a group set at all, which is exactly why it is here.
func TestModelDeploymentPodGroups(t *testing.T) {
	type group struct {
		instanceType string
		total        int32
	}

	cases := []struct {
		name string
		md   *workercore.ModelDeployment
		want []group
	}{
		{
			name: "one_type_two_roles",
			md:   podGroupDeployment(),
			want: []group{{"h20-8x", 4}},
		},
		{
			name: "two_types_one_role_each",
			md:   twoTypeDeployment(),
			want: []group{{"h20-8x", 2}, {"a100-8x", 3}},
		},
		{
			// Two roles on one type and a third on another are TWO groups, and the first group's
			// total counts both of its roles.
			name: "three_roles_two_types",
			md: twoTypeDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
					Name: "prefill-2", Replicas: 5, InstanceType: "h20-8x",
				})
			}),
			want: []group{{"h20-8x", 7}, {"a100-8x", 3}},
		},
		{
			// The order is first mention, so a type named again later does not move.
			name: "order_is_first_mention",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles = []workercore.ModelDeploymentRole{
					{Name: "a", Replicas: 1, InstanceType: "a100-8x"},
					{Name: "b", Replicas: 1, InstanceType: "h20-8x"},
					{Name: "c", Replicas: 1, InstanceType: "a100-8x"},
				}
			}),
			want: []group{{"a100-8x", 2}, {"h20-8x", 1}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups := modelDeploymentPodGroups(tc.md)

			got := make([]group, 0, len(groups))
			for _, g := range groups {
				got = append(got, group{g.InstanceType, g.TotalCount})
			}
			assert.Equal(t, tc.want, got, "one group per instanceType, in the order the roles name them")
		})
	}
}

// TestModelDeploymentPodGroups_NamesAreUniqueAndStable covers the identity half.
//
// A name that was unique per deployment has to become unique per deployment AND instanceType: two
// groups sharing one would be read by Kueue as one group, each waiting for the other's Pods.
func TestModelDeploymentPodGroups_NamesAreUniqueAndStable(t *testing.T) {
	md := twoTypeDeployment()

	first := modelDeploymentPodGroups(md)
	require.Len(t, first, 2)
	assert.NotEqual(t, first[0].Name, first[1].Name,
		"two groups sharing a name are one group to Kueue, and neither reaches its total")

	second := modelDeploymentPodGroups(md)
	assert.Equal(t, first, second, "the name is written into Pods, so two passes must agree")

	for _, g := range first {
		assert.Empty(t, validation.IsValidLabelValue(g.Name), "the name goes in a label value")
	}

	// The same type under a different deployment is a different group.
	other := twoTypeDeployment(func(md *workercore.ModelDeployment) { md.Name = "qwen-7b" })
	assert.NotEqual(t, first[0].Name, modelDeploymentPodGroups(other)[0].Name)

	// And the same deployment name in another namespace likewise.
	elsewhere := twoTypeDeployment(func(md *workercore.ModelDeployment) { md.Namespace = "team-b" })
	assert.NotEqual(t, first[0].Name, modelDeploymentPodGroups(elsewhere)[0].Name)
}

// TestModelDeploymentPodGroups_ASoleGroupKeepsTheReadableName pins that the split did not rename the
// case that existed before it. The fixtures assert this through the rendered Pod; this asserts it on
// the function, so a failure says which of the two moved.
func TestModelDeploymentPodGroups_ASoleGroupKeepsTheReadableName(t *testing.T) {
	groups := modelDeploymentPodGroups(podGroupDeployment())

	require.Len(t, groups, 1)
	assert.Equal(t, "qwen-72b", groups[0].Name, "one group keeps the deployment's own name")

	// A deployment with several groups does not, and the reason is collision rather than taste: a
	// readable composite would share a namespace with deployment names.
	for _, g := range modelDeploymentPodGroups(twoTypeDeployment()) {
		assert.True(t, strings.HasPrefix(g.Name, modelDeploymentPodGroupNamePrefix),
			"a derived name carries the prefix that says it was derived: %s", g.Name)
	}
}

// TestModelDeploymentPodGroup_StampsTheRolesOwnGroup covers what reaches a Pod when there are two
// groups: each role's replicas must carry THEIR group's name and THEIR group's total.
func TestModelDeploymentPodGroup_StampsTheRolesOwnGroup(t *testing.T) {
	md := twoTypeDeployment()

	prefill := ModelDeploymentPodGroup(md, &md.Spec.Roles[0])
	decode := ModelDeploymentPodGroup(md, &md.Spec.Roles[1])

	assert.NotEqual(t, prefill.Labels[kueuepodconst.GroupNameLabel],
		decode.Labels[kueuepodconst.GroupNameLabel],
		"two instanceTypes cannot be one Workload, because one Workload carries one queue name")

	assert.Equal(t, "2", prefill.Annotations[kueuepodconst.GroupTotalCountAnnotation],
		"a group claiming the deployment-wide total waits for Pods that are never coming")
	assert.Equal(t, "3", decode.Annotations[kueuepodconst.GroupTotalCountAnnotation])

	// The role hash stays the role's name and does not gain the type: Kueue groups PodSets by it and
	// status reads it to attribute a Pod.
	assert.Equal(t, "prefill", prefill.Annotations[kueuepodconst.RoleHashAnnotation])
	assert.Equal(t, "decode", decode.Annotations[kueuepodconst.RoleHashAnnotation])
}

// TestModelDeploymentGroupsResizing covers the predicate the converge loop reads.
//
// EACH POD IS JUDGED AGAINST ITS OWN GROUP'S TOTAL. Against the deployment-wide sum a two-group
// deployment reads as resizing on every pass forever -- a rebuild loop rather than a wrong number,
// and one that nothing reports.
//
// THE ANSWER IS A SET, AND WHICH GROUPS ARE IN IT IS THE ASSERTION. A predicate that named every
// group whenever any one moved would pass a test asserting only "something is resizing", while
// restarting roles that nothing asked to restart.
func TestModelDeploymentGroupsResizing(t *testing.T) {
	pod := func(group, role, total, replicas string) core.Pod {
		p := core.Pod{ObjectMeta: meta.ObjectMeta{
			Labels: map[string]string{
				modelDeploymentLabelKeyComponent: role,
				kueuepodconst.GroupNameLabel:     group,
			},
			Annotations: map[string]string{},
		}}
		if total != "" {
			p.Annotations[kueuepodconst.GroupTotalCountAnnotation] = total
		}
		if replicas != "" {
			p.Annotations[modelDeploymentRoleReplicasAnnotation] = replicas
		}

		return p
	}

	// The two-type shape's group names are hashes, so the cases name them through the function that
	// derives them rather than by writing a hash into the test.
	two := twoTypeDeployment()
	twoGroups := modelDeploymentPodGroups(two)
	require.Len(t, twoGroups, 2)
	prefillGroup, decodeGroup := twoGroups[0].Name, twoGroups[1].Name

	cases := []struct {
		name string
		md   *workercore.ModelDeployment
		pods []core.Pod
		want []string
	}{
		{
			name: "one_group_agreeing",
			md:   podGroupDeployment(),
			pods: []core.Pod{pod("qwen-72b", "prefill", "4", "2"), pod("qwen-72b", "decode", "4", "2")},
		},
		{
			// THE CASE THE DEPLOYMENT-WIDE SUM FAILS: each Pod carries its own group's total, and a
			// predicate reading the sum of both groups matches neither.
			name: "two_groups_agreeing",
			md:   two,
			pods: []core.Pod{
				pod(prefillGroup, "prefill", "2", "2"),
				pod(decodeGroup, "decode", "3", "3"),
			},
		},
		{
			// ONLY THE GROUP THAT MOVED. The sibling agrees with its own total and must be left alone;
			// a boolean predicate would restart it too.
			name: "two_groups_one_moved",
			md:   two,
			pods: []core.Pod{
				pod(prefillGroup, "prefill", "9", "2"),
				pod(decodeGroup, "decode", "3", "3"),
			},
			want: []string{prefillGroup},
		},
		{
			// A role that moved onto another instanceType: the group it LEAVES and the group it JOINS
			// both come down, and neither name is derivable from the other.
			name: "a_role_moved_between_types",
			md:   two,
			pods: []core.Pod{
				pod(decodeGroup, "prefill", "3", "2"),
				pod(decodeGroup, "decode", "3", "3"),
			},
			want: []string{decodeGroup, prefillGroup},
		},
		{
			// The sum is unchanged at 4 while the split moved, which is what the per-role share is
			// beside the total for.
			name: "shares_moved_under_an_unchanged_total",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas, md.Spec.Roles[1].Replicas = 1, 3
			}),
			pods: []core.Pod{pod("qwen-72b", "prefill", "4", "2"), pod("qwen-72b", "decode", "4", "2")},
			want: []string{"qwen-72b"},
		},
		{
			name: "a_pod_predating_the_annotations",
			md:   podGroupDeployment(),
			pods: []core.Pod{pod("qwen-72b", "prefill", "", "")},
			want: []string{"qwen-72b"},
		},
		{
			name: "a_pod_of_a_role_the_deployment_no_longer_has",
			md:   podGroupDeployment(),
			pods: []core.Pod{pod("qwen-72b", "gone", "4", "2")},
			want: []string{"qwen-72b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.ElementsMatch(t, tc.want,
				sets.List(modelDeploymentGroupsResizing(tc.md, tc.pods)))
		})
	}
}
