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

// TestModelDeploymentPodGroup covers what every Pod of a role's replica carries.
func TestModelDeploymentPodGroup(t *testing.T) {
	md := podGroupDeployment()

	prefill := ModelDeploymentPodGroup(md, &md.Spec.Roles[0], 0, 0)
	decode := ModelDeploymentPodGroup(md, &md.Spec.Roles[1], 0, 0)

	// TWO REPLICAS NEVER SHARE A GROUP, whatever the axis that separates them: a role is one, and
	// within one role the ordinal is another. Two replicas of one role sharing a name are one group
	// to Kueue, and each then waits for the other's Pods.
	assert.NotEqual(t, prefill.Labels[kueuepodconst.GroupNameLabel],
		decode.Labels[kueuepodconst.GroupNameLabel],
		"a role is one axis of separation: two roles sharing a name are one group to Kueue")

	first := ModelDeploymentPodGroup(md, &md.Spec.Roles[0], 0, 0)
	second := ModelDeploymentPodGroup(md, &md.Spec.Roles[0], 1, 0)
	assert.NotEqual(t, first.Labels[kueuepodconst.GroupNameLabel],
		second.Labels[kueuepodconst.GroupNameLabel],
		"the ordinal is the other: two replicas of ONE role are two groups, or the group waits "+
			"for Pods that are never coming")

	assert.Equal(t, "1", first.Annotations[kueuepodconst.GroupTotalCountAnnotation],
		"the group is this replica and nobody else's, so its total is one whatever the role "+
			"declares -- and a replicas change moves no total any member carries")
	assert.Equal(t, "1", second.Annotations[kueuepodconst.GroupTotalCountAnnotation])
	assert.Equal(t, "1", decode.Annotations[kueuepodconst.GroupTotalCountAnnotation])

	assert.Equal(t, "prefill", first.Annotations[kueuepodconst.RoleHashAnnotation])
	assert.Equal(t, "decode", decode.Annotations[kueuepodconst.RoleHashAnnotation])

	// THE GROUP CARRIES EXACTLY TWO LABELS: membership and ordinal. A third selectable carrier of
	// the replica's identity would be a second answer to one question -- the ordinal label already
	// exists for the readers that need it, so asserting the exact key set here is what keeps a
	// second one from being added by an edit that looks harmless.
	assert.ElementsMatch(t,
		[]string{modelDeploymentReplicaOrdinalLabel, kueuepodconst.GroupNameLabel},
		slices.Collect(maps.Keys(first.Labels)),
		"the group contributes exactly two labels: ordinal and membership")

	// THE GROUP CARRIES EXACTLY THREE ANNOTATIONS. A fourth would be a second fingerprint of the
	// shape beside the total, and the total is fixed at one already.
	assert.Equal(t, []string{
		kueuepodconst.GroupServingAnnotationKey,
		kueuepodconst.GroupTotalCountAnnotation,
		kueuepodconst.RoleHashAnnotation,
	}, slices.Sorted(maps.Keys(first.Annotations)),
		"membership, total and PodSet name travel as one value")

	// THE ORDINAL LABEL IS THE PLAIN DIGITS, because it is the one per-replica value read back: the
	// group name is never parsed, and a reader that wanted the ordinal out of it would be the
	// coupling this label exists to make unnecessary.
	assert.Equal(t, "0", first.Labels[modelDeploymentReplicaOrdinalLabel])
	assert.Equal(t, "1", second.Labels[modelDeploymentReplicaOrdinalLabel])

	assert.Equal(t, kueuepodconst.GroupServingAnnotationValue,
		first.Annotations[kueuepodconst.GroupServingAnnotationKey],
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

	left := ModelDeploymentPodGroup(md, &md.Spec.Roles[0], 0, 0)
	right := ModelDeploymentPodGroup(md, &md.Spec.Roles[1], 0, 0)

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

	group := ModelDeploymentPodGroup(md, &md.Spec.Roles[0], 0, 0)

	assert.NotContains(t, group.Annotations, kueuepodconst.GroupFastAdmissionAnnotationKey,
		"setting %q admits a group from its first Pod alone, short of the total it declares",
		kueuepodconst.GroupFastAdmissionAnnotationKey)
	assert.NotContains(t, group.Labels, kueuepodconst.GroupFastAdmissionAnnotationKey,
		"and it must not arrive as a label either")
}

// TestModelDeploymentReplicaGroupName covers the derivation's one obligation and its one form.
func TestModelDeploymentReplicaGroupName(t *testing.T) {
	md := podGroupDeployment()

	got := modelDeploymentReplicaGroupName(md, "prefill", 0)

	assert.Empty(t, validation.IsValidLabelValue(got),
		"the name is a label value and, because Kueue names the group's Workload after it verbatim, "+
			"an object name too: it owes the RFC 1123 subdomain charset whatever else it owes")
	assert.True(t, strings.HasPrefix(got, modelDeploymentPodGroupNamePrefix),
		"the name is always the derived form, so it always carries the prefix that says so: %s", got)
	assert.NotContains(t, got, "qwen-72b",
		"no readable spelling rides along -- the prefix and the digest are the whole name")
}

// TestModelDeploymentReplicaGroupName_HashCoversTheNamespace pins what the hash is taken over.
//
// The group name is only ever compared against other Pods' group names, so two deployments sharing
// a name in two namespaces must not derive alike -- their Pods would read as one group, and each
// would then be waiting for the other's replicas.
func TestModelDeploymentReplicaGroupName_HashCoversTheNamespace(t *testing.T) {
	here := podGroupDeployment()
	there := podGroupDeployment(func(md *workercore.ModelDeployment) { md.Namespace = "team-b" })

	assert.NotEqual(t,
		modelDeploymentReplicaGroupName(here, "prefill", 0),
		modelDeploymentReplicaGroupName(there, "prefill", 0))
}

// TestModelDeploymentReplicaGroupName_ShapeIndependentOfRoleCount covers the property the
// sole-role branch used to break: a first role's replicas name their groups identically whether
// that role is the only one or one of several, so gaining or losing a SIBLING ROLE moves no group
// name and turns nothing over.
func TestModelDeploymentReplicaGroupName_ShapeIndependentOfRoleCount(t *testing.T) {
	one := podGroupDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles = md.Spec.Roles[:1]
	})
	two := podGroupDeployment()

	for ordinal := range one.Spec.Roles[0].Replicas {
		assert.Equalf(t,
			modelDeploymentReplicaGroupName(one, "prefill", int(ordinal)),
			modelDeploymentReplicaGroupName(two, "prefill", int(ordinal)),
			"ordinal %d: a role's group names are its own, not a function of how many roles the "+
				"deployment declares beside it", ordinal)
	}
}

// TestModelDeploymentReplicaGroupName_UniqueAcrossTheDeployment pins the set property a per-replica
// derivation owes: no two replicas of the deployment may share a group, on any axis.
func TestModelDeploymentReplicaGroupName_UniqueAcrossTheDeployment(t *testing.T) {
	md := twoTypeDeployment()

	seen := sets.New[string]()
	for i := range md.Spec.Roles {
		for ordinal := range md.Spec.Roles[i].Replicas {
			seen.Insert(modelDeploymentReplicaGroupName(md, md.Spec.Roles[i].Name, int(ordinal)))
		}
	}
	assert.Equal(t, 5, seen.Len(),
		"two roles declaring 2 and 3 form five replicas, and every one of them is its own group")
}

// TestModelDeploymentPodOrdinal covers the reader the converger keys on.
//
// THE ABSENCE IS AN ANSWER: a Pod rendered before the per-replica groups carried no ordinal label,
// and reading one out of it -- a zero, a guess -- would adopt it onto a slot it never held. The
// converger treats an ordinal-less Pod as outdated instead, which is the turnover that ends with
// the Pod replaced by one that carries the label.
func TestModelDeploymentPodOrdinal(t *testing.T) {
	meta := ModelDeploymentPodGroup(podGroupDeployment(), &podGroupDeployment().Spec.Roles[0], 3, 0)

	stamped := &core.Pod{}
	stamped.Labels = meta.Labels
	ordinal, ok := modelDeploymentPodOrdinal(stamped)
	assert.True(t, ok)
	assert.Equal(t, 3, ordinal)

	for _, label := range []string{"", "-1", "not-a-number", "3.0"} {
		pod := &core.Pod{}
		if label != "" {
			pod.Labels = map[string]string{modelDeploymentReplicaOrdinalLabel: label}
		}
		ordinal, ok := modelDeploymentPodOrdinal(pod)
		assert.Falsef(t, ok, "label %q must not read as an ordinal", label)
		assert.Zero(t, ordinal)
	}
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

// TestModelDeploymentPodGroups_ANamesAreAlwaysDerived pins the property the sole-role branch used to
// break: a role's join-key name is derived the same way whether it is the only role or one of
// several. The key is carried by no Pod -- it joins the status paths -- but a key that moved with
// the role count would rename the first role's key when a sibling arrived, and a key that moves
// under its consumers is worse than one that was always opaque.
func TestModelDeploymentPodGroups_ANamesAreAlwaysDerived(t *testing.T) {
	for _, md := range []*workercore.ModelDeployment{
		podGroupDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles = md.Spec.Roles[:1] }),
		podGroupDeployment(),
		twoTypeDeployment(),
	} {
		for _, g := range modelDeploymentPodGroups(md) {
			assert.Truef(t, strings.HasPrefix(g.Name, modelDeploymentPodGroupNamePrefix),
				"a derived name carries the prefix that says it was derived: %s", g.Name)
		}
	}

	// AND THE NAME IS THE ROLE'S OWN, not a function of the company it keeps: the sole-role
	// deployment and the two-role one derive the same key for the same first role.
	sole := podGroupDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles = md.Spec.Roles[:1] })
	assert.Equal(t,
		modelDeploymentPodGroups(sole)[0].Name,
		modelDeploymentPodGroups(podGroupDeployment())[0].Name,
		"gaining a sibling role renames nothing")
}

// TestModelDeploymentPodGroup_StampsTheRolesOwnGroup covers what reaches a Pod: each replica must
// carry ITS OWN group's name, on both axes that separate replicas.
func TestModelDeploymentPodGroup_StampsTheRolesOwnGroup(t *testing.T) {
	md := twoTypeDeployment()

	prefill := ModelDeploymentPodGroup(md, &md.Spec.Roles[0], 0, 0)
	decode := ModelDeploymentPodGroup(md, &md.Spec.Roles[1], 0, 0)

	assert.NotEqual(t, prefill.Labels[kueuepodconst.GroupNameLabel],
		decode.Labels[kueuepodconst.GroupNameLabel],
		"two roles are two axes of separation: a role's replicas carry that role's slots and never "+
			"a sibling's")

	assert.Equal(t, "1", prefill.Annotations[kueuepodconst.GroupTotalCountAnnotation],
		"a group claiming another replica's count waits for Pods that are never coming")
	assert.Equal(t, "1", decode.Annotations[kueuepodconst.GroupTotalCountAnnotation])

	// The role hash stays the role's name and does not gain the type or the ordinal: Kueue groups
	// PodSets by it and status reads it to attribute a Pod.
	assert.Equal(t, "prefill", prefill.Annotations[kueuepodconst.RoleHashAnnotation])
	assert.Equal(t, "decode", decode.Annotations[kueuepodconst.RoleHashAnnotation])
}

// TestModelDeploymentPodSpecHash_CoversTheGroupNameAndOrdinal is the mechanical pin the per-replica
// split stands on: the fingerprint covers the labels, the group name among them, so two ordinals of
// one role hash differently and a replica is current only against the render of its own slot.
//
// THE HASH IS WHAT KEEPS A SCALE-UP FROM ROLLING THE SURVIVORS: a role grown from two to three
// renders ordinals 0 and 1 exactly as before, and only a hash that covered the per-replica group
// name can say so. An implementation hashing the template alone -- the thing every replica of a role
// shares -- would return one hash for all three, call the two running replicas current against a
// render that names a different group, and pass every count-shaped assertion in this file.
func TestModelDeploymentPodSpecHash_CoversTheGroupNameAndOrdinal(t *testing.T) {
	md := newRenderDeployment()
	role := &md.Spec.Roles[0]

	stamp := func(ordinal int) *core.Pod {
		pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: role, InstanceType: newRenderInstanceType(),
		})
		require.NoError(t, err)
		stampModelDeploymentPod(pod, md, role, ordinal, 0)

		return pod
	}

	first, second := stamp(0), stamp(1)
	assert.NotEqual(t,
		first.Annotations[modelDeploymentPodSpecHashAnnotation],
		second.Annotations[modelDeploymentPodSpecHashAnnotation],
		"two ordinals of one role are two renders: their group names differ and the hash says so")

	again := stamp(0)
	assert.Equal(t,
		first.Annotations[modelDeploymentPodSpecHashAnnotation],
		again.Annotations[modelDeploymentPodSpecHashAnnotation],
		"the same ordinal renders the same hash, or no running replica would ever read as current")
}
