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

// podGroupDeployment builds a two-role deployment, prefill 2 and decode 2 on ONE instanceType, which
// is the shape every case below varies from. Two roles on one type are two groups: the type neither
// splits nor joins them.
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

// TestModelDeploymentPodGroup covers what every Pod of a role's group carries.
func TestModelDeploymentPodGroup(t *testing.T) {
	md := podGroupDeployment()

	prefill := ModelDeploymentPodGroup(md, &md.Spec.Roles[0])
	decode := ModelDeploymentPodGroup(md, &md.Spec.Roles[1])

	assert.NotEqual(t, prefill.Labels[kueuepodconst.GroupNameLabel],
		decode.Labels[kueuepodconst.GroupNameLabel],
		"one role is one group: two roles sharing a name are one group to Kueue, and neither "+
			"reaches its declared total")

	assert.Equal(t, "2", prefill.Annotations[kueuepodconst.GroupTotalCountAnnotation],
		"the total is the role's own declared count -- the group is that role and nobody else, even "+
			"where two roles share one instanceType")
	assert.Equal(t, "2", decode.Annotations[kueuepodconst.GroupTotalCountAnnotation])

	assert.Equal(t, "prefill", prefill.Annotations[kueuepodconst.RoleHashAnnotation])
	assert.Equal(t, "decode", decode.Annotations[kueuepodconst.RoleHashAnnotation])

	// The group adds NO label of its own naming the role. The renderer's selector labels already
	// carry it, so a second selectable carrier would be a second answer to one question -- asserted
	// here rather than left implicit, because adding one is the kind of edit that looks harmless.
	assert.Equal(t, []string{kueuepodconst.GroupNameLabel}, slices.Sorted(maps.Keys(prefill.Labels)),
		"the group contributes exactly one label: membership")

	// THE GROUP CARRIES EXACTLY THREE ANNOTATIONS. A fourth would be a second fingerprint of the
	// shape beside the total, and the total is the role's count already -- the per-role share an
	// earlier shape stamped beside it went with that shape, and this is what keeps it gone.
	assert.Equal(t, []string{
		kueuepodconst.GroupServingAnnotationKey,
		kueuepodconst.GroupTotalCountAnnotation,
		kueuepodconst.RoleHashAnnotation,
	}, slices.Sorted(maps.Keys(prefill.Annotations)),
		"membership, total and PodSet name travel as one value")

	assert.Equal(t, kueuepodconst.GroupServingAnnotationValue,
		prefill.Annotations[kueuepodconst.GroupServingAnnotationKey],
		"an inference deployment never finishes; without this Kueue reclaims the quota of a Pod "+
			"that reached Succeeded while the deployment is still meant to be serving")
}

// TestModelDeploymentPodGroup_RoleHashIsTheRoleName is the case that would pass by accident.
//
// The two roles here differ ONLY in name, which is exactly the input that makes Kueue's derived role
// hash -- a digest of the Pod spec's shape -- identical for both. The Workload's PodSets are what
// per-role flavor assignment and per-role status join to the roles, and the join is by NAME: an
// opaque digest names a PodSet after nothing an operator wrote, and the join breaks while nothing
// errors. Asserting each role's own name on two identically-shaped roles is what makes the
// annotation load-bearing rather than assumed.
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
		"the PodSet join is by name, so the annotation must carry the role's own name")
}

// TestModelDeploymentPodGroup_FastAdmissionIsAbsent asserts an ABSENCE, and the absence is the point.
//
// Setting kueue.x-k8s.io/pod-group-fast-admission makes Kueue compose the Workload from the FIRST
// runnable Pod alone, counting that PodSet at the whole group's total. The Workload then exists
// while the group is still short of the total it declares, and the remaining replicas arrive as
// members of an already-admitted set rather than as the set -- admission stops being the unitary
// decision the group exists to make. Nothing errors, and the Workload looks well formed.
func TestModelDeploymentPodGroup_FastAdmissionIsAbsent(t *testing.T) {
	md := podGroupDeployment()

	group := ModelDeploymentPodGroup(md, &md.Spec.Roles[0])

	assert.NotContains(t, group.Annotations, kueuepodconst.GroupFastAdmissionAnnotationKey,
		"setting %q admits a group from its first Pod alone, short of the total it declares",
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

// TestModelDeploymentPodGroupTotalCount covers the count each group declares: its own role's,
// verbatim, and no other role's.
func TestModelDeploymentPodGroupTotalCount(t *testing.T) {
	type want struct {
		role  string
		total int32
	}

	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
		want []want
	}{
		{
			// The single-role shape, which must keep declaring exactly its own replicas.
			name: "one_role_declares_only_its_own",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles = md.Spec.Roles[:1]
			}),
			want: []want{{"prefill", 2}},
		},
		{
			// Two roles on ONE type declare TWO counts. A group claiming the pair's sum waits for
			// Pods that are never coming.
			name: "two_roles_on_one_type_declare_two_counts",
			md:   podGroupDeployment(),
			want: []want{{"prefill", 2}, {"decode", 2}},
		},
		{
			name: "uneven_counts_stay_per_role",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = 3
			}),
			want: []want{{"prefill", 3}, {"decode", 2}},
		},
		{
			// Replicas is read VERBATIM rather than defaulted to one: the schema defaults and bounds
			// the field, so a zero reaches here only from a value built in Go -- and a total
			// disagreeing with the number of Pods the reconciler creates from the same field would be
			// worse than a zero.
			name: "a_zero_is_carried_as_written",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[1].Replicas = 0
			}),
			want: []want{{"prefill", 2}, {"decode", 0}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			groups := modelDeploymentPodGroups(tc.md)

			got := make([]want, 0, len(groups))
			for _, g := range groups {
				got = append(got, want{g.Role, g.TotalCount})
			}
			assert.Equal(t, tc.want, got,
				"a group's total is its own role's declared replicas, and nobody else's")
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
// THE GROUPING KEY IS THE ROLE AND NOT THE instanceType, and the first case is what says so: two
// roles on ONE type are TWO groups, and an implementation keyed on the type passes every other case
// here and produces one for it.
func TestModelDeploymentPodGroups(t *testing.T) {
	type group struct {
		role         string
		instanceType string
		total        int32
	}

	cases := []struct {
		name string
		md   *workercore.ModelDeployment
		want []group
	}{
		{
			name: "two_roles_one_type",
			md:   podGroupDeployment(),
			want: []group{{"prefill", "h20-8x", 2}, {"decode", "h20-8x", 2}},
		},
		{
			name: "two_types_one_role_each",
			md:   twoTypeDeployment(),
			want: []group{{"prefill", "h20-8x", 2}, {"decode", "a100-8x", 3}},
		},
		{
			// Two roles on one type and a third on another are THREE groups -- one per role, never
			// one per type -- and no group's total counts any other role's replicas.
			name: "three_roles_two_types",
			md: twoTypeDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
					Name: "prefill-2", Replicas: 5, InstanceType: "h20-8x",
				})
			}),
			want: []group{{"prefill", "h20-8x", 2}, {"decode", "a100-8x", 3}, {"prefill-2", "h20-8x", 5}},
		},
		{
			// The order is the roles' order, so a type named by two roles does not collapse them.
			name: "order_is_the_roles_order",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles = []workercore.ModelDeploymentRole{
					{Name: "a", Replicas: 1, InstanceType: "a100-8x"},
					{Name: "b", Replicas: 1, InstanceType: "h20-8x"},
					{Name: "c", Replicas: 1, InstanceType: "a100-8x"},
				}
			}),
			want: []group{{"a", "a100-8x", 1}, {"b", "h20-8x", 1}, {"c", "a100-8x", 1}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups := modelDeploymentPodGroups(tc.md)

			got := make([]group, 0, len(groups))
			for _, g := range groups {
				got = append(got, group{g.Role, g.InstanceType, g.TotalCount})
			}
			assert.Equal(t, tc.want, got, "one group per role, in the order the spec names them")
		})
	}
}

// TestModelDeploymentPodGroups_NamesAreUniqueAndStable covers the identity half.
//
// A name that was unique per deployment has to become unique per deployment AND role: two groups
// sharing one would be read by Kueue as one group, each waiting for the other's Pods.
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

	// TWO ROLES ON ONE instanceType ARE TWO HASHED NAMES. Colliding there is the same failure as
	// anywhere else -- Kueue reads both roles as one group and each waits for the other's replicas
	// -- so the same-type shape is asserted, not only the two-type one.
	sameType := modelDeploymentPodGroups(podGroupDeployment())
	require.Len(t, sameType, 2)
	assert.NotEqual(t, sameType[0].Name, sameType[1].Name,
		"two roles on one instanceType are two groups, so their names must differ too")
	for _, g := range sameType {
		assert.True(t, strings.HasPrefix(g.Name, modelDeploymentPodGroupNamePrefix),
			"a derived name carries the prefix that says it was derived: %s", g.Name)
	}

	// The same role under a different deployment is a different group.
	other := twoTypeDeployment(func(md *workercore.ModelDeployment) { md.Name = "qwen-7b" })
	assert.NotEqual(t, first[0].Name, modelDeploymentPodGroups(other)[0].Name)

	// And the same deployment name in another namespace likewise.
	elsewhere := twoTypeDeployment(func(md *workercore.ModelDeployment) { md.Namespace = "team-b" })
	assert.NotEqual(t, first[0].Name, modelDeploymentPodGroups(elsewhere)[0].Name)
}

// TestModelDeploymentPodGroups_ASoleGroupKeepsTheReadableName pins that the one-role shape keeps the
// name it always had. The fixtures assert this through the rendered Pod; this asserts it on the
// function, so a failure says which of the two moved.
func TestModelDeploymentPodGroups_ASoleGroupKeepsTheReadableName(t *testing.T) {
	groups := modelDeploymentPodGroups(podGroupDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles = md.Spec.Roles[:1]
	}))

	require.Len(t, groups, 1)
	assert.Equal(t, "qwen-72b", groups[0].Name, "a one-role deployment keeps the deployment's own name")

	// Every multi-role deployment does not -- on ONE instanceType exactly as on several -- and the
	// reason is collision rather than taste: a readable composite would share a namespace with
	// deployment names.
	for _, md := range []*workercore.ModelDeployment{podGroupDeployment(), twoTypeDeployment()} {
		for _, g := range modelDeploymentPodGroups(md) {
			assert.True(t, strings.HasPrefix(g.Name, modelDeploymentPodGroupNamePrefix),
				"a derived name carries the prefix that says it was derived: %s", g.Name)
		}
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
		"two roles are two groups: a role's replicas carry that role's name and never a sibling's")

	assert.Equal(t, "2", prefill.Annotations[kueuepodconst.GroupTotalCountAnnotation],
		"a group claiming another role's count waits for Pods that are never coming")
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
	// A REPLICA AS THE RENDERER WOULD HAVE PRODUCED IT, taken from the same function that stamps the
	// group metadata onto a real Pod rather than from literals written here.
	//
	// THE FORMAT OF THESE ANNOTATIONS IS OWNED BY NEITHER SIDE OF THE PAIR THIS TEST EXERCISES. The
	// renderer formats the total, and the predicate below formats what it expects, in two separate
	// expressions that agree today by coincidence. With literals in this fixture, a renderer that
	// changed the format would keep its own tests green -- they would be updated with it -- while
	// this one went on comparing the old spelling, and both halves would pass while the reconciler
	// rebuilt every group on every pass forever. Coverage shows both sides covered, and mutating
	// either implementation goes red; only changing the FORMAT slips through, because the format has
	// no owner. Sourcing the fixture from the renderer gives it one.
	//
	// The disagreeing cases perturb a rendered replica rather than hand-building one, so what they
	// vary is visible as a difference from what the spec asks for.
	rendered := func(md *workercore.ModelDeployment, roleName string) core.Pod {
		var role *workercore.ModelDeploymentRole
		for i := range md.Spec.Roles {
			if md.Spec.Roles[i].Name == roleName {
				role = &md.Spec.Roles[i]
			}
		}
		require.NotNil(t, role, "no role %q on this fixture deployment", roleName)

		meta := ModelDeploymentPodGroup(md, role)
		p := core.Pod{}
		p.Labels = map[string]string{modelDeploymentLabelKeyComponent: roleName}
		for k, v := range meta.Labels {
			p.Labels[k] = v
		}
		p.Annotations = map[string]string{}
		for k, v := range meta.Annotations {
			p.Annotations[k] = v
		}

		return p
	}

	// perturbed renders the replica and then overrides one annotation, which is how a Pod that
	// disagrees with the spec arises in a cluster: it was rendered against an earlier spec.
	perturbed := func(p core.Pod, key, value string) core.Pod {
		if value == "" {
			delete(p.Annotations, key)
		} else {
			p.Annotations[key] = value
		}

		return p
	}

	// A replica of a role the deployment no longer declares: it was rendered when the role existed,
	// so everything about it is as the renderer left it and only its role is now unknown.
	renamedRole := func(p core.Pod, role string) core.Pod {
		p.Labels[modelDeploymentLabelKeyComponent] = role

		return p
	}

	// The one-role shape the sole-group cases render from: its single group keeps the readable
	// deployment name, which is what the want values below use.
	sole := podGroupDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles = md.Spec.Roles[:1]
	})

	// The single-type two-role shape the sibling cases render from. It is built once so that a case
	// whose deployment has MOVED ON still renders its replicas from the shape they were created
	// under, which is what a Pod disagreeing with its spec actually is.
	one := podGroupDeployment()
	oneGroups := modelDeploymentPodGroups(one)
	require.Len(t, oneGroups, 2)
	prefillName := oneGroups[0].Name

	// The two-type shape's group names are hashes, so the cases name them through the function that
	// derives them rather than by writing a hash into the test.
	two := twoTypeDeployment()
	twoGroups := modelDeploymentPodGroups(two)
	require.Len(t, twoGroups, 2)
	prefillGroup := twoGroups[0].Name

	cases := []struct {
		name string
		md   *workercore.ModelDeployment
		pods []core.Pod
		want []string
	}{
		{
			// The one-role baseline: a sole group whose Pod carries its total is not resizing.
			name: "one_role_agreeing",
			md:   sole,
			pods: []core.Pod{rendered(sole, "prefill")},
		},
		{
			// Two roles on ONE type both agreeing: neither group is resizing, and the deployment-wide
			// sum would match neither group's total if it were read instead.
			name: "each_role_agreeing_on_one_type",
			md:   one,
			pods: []core.Pod{rendered(one, "prefill"), rendered(one, "decode")},
		},
		{
			// THE CASE THE DEPLOYMENT-WIDE SUM FAILS: each Pod carries its own group's total, and a
			// predicate reading the sum of both groups matches neither.
			name: "two_groups_agreeing",
			md:   two,
			pods: []core.Pod{rendered(two, "prefill"), rendered(two, "decode")},
		},
		{
			// ONLY THE GROUP THAT MOVED. The sibling agrees with its own total and must be left alone;
			// a boolean predicate would restart it too.
			name: "two_groups_one_moved",
			md:   two,
			pods: []core.Pod{
				perturbed(rendered(two, "prefill"), kueuepodconst.GroupTotalCountAnnotation, "9"),
				rendered(two, "decode"),
			},
			want: []string{prefillGroup},
		},
		{
			// ONE ROLE'S COUNT MOVED, ON ONE TYPE, under a deployment-wide sum that did not: prefill
			// went 2 to 1 while decode kept its two, so only prefill's group is resizing. This is the
			// redistribution the per-role share annotation used to exist for; one role per group
			// makes the total carry it alone.
			name: "one_roles_count_moved_leaves_the_sibling_alone",
			md: podGroupDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = 1
			}),
			pods: []core.Pod{rendered(one, "prefill"), rendered(one, "decode")},
			want: []string{prefillName},
		},
		{
			name: "a_pod_predating_the_annotations",
			md:   sole,
			pods: []core.Pod{
				perturbed(rendered(sole, "prefill"), kueuepodconst.GroupTotalCountAnnotation, ""),
			},
			want: []string{"qwen-72b"},
		},
		{
			// The rename catch: no total moves, no count moves, and the predicate still has to come
			// down on the group the departed role's Pods are sitting in.
			name: "a_pod_of_a_role_the_deployment_no_longer_has",
			md:   sole,
			pods: []core.Pod{renamedRole(rendered(sole, "prefill"), "gone")},
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
