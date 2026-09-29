package mooncake

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// haBackend is the shared fixture with Kubernetes election and three replicas.
func haBackend(mutate ...func(*workercore.KVCacheBackend)) *workercore.KVCacheBackend {
	all := append([]func(*workercore.KVCacheBackend){
		func(kvcb *workercore.KVCacheBackend) {
			kvcb.Spec.Connection.Managed.Leader.ElectionBackend = "Kubernetes"
			kvcb.Spec.Connection.Managed.Leader.HighAvailability = &workercore.KVCacheBackendLeaderHighAvailability{}
			kvcb.Spec.Connection.Managed.Leader.Replicas = ptr.To[int32](3)
		},
	}, mutate...)
	return testBackend(all...)
}

func TestLeaderElectionChoice(t *testing.T) {
	cases := []struct {
		name     string
		backend  string
		replicas int32
		want     bool
		count    int32
	}{
		{name: "default at one replica", replicas: 1, want: true, count: 1},
		{name: "explicit Kubernetes at one replica", backend: "Kubernetes", replicas: 1, want: true, count: 1},
		{name: "None at one replica", backend: "None", replicas: 1, count: 1},
		{name: "default at three replicas", replicas: 3, want: true, count: 3},
		{name: "None is clamped without admission", backend: "None", replicas: 3, count: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kvcb := testBackend(func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Leader.ElectionBackend = tc.backend
				k.Spec.Connection.Managed.Leader.Replicas = ptr.To(tc.replicas)
			})
			deploy := RenderLeaderDeployment(kvcb, "mooncake:v0.3.13")
			assert.Equal(t, tc.count, *deploy.Spec.Replicas)
			assert.Equal(t, tc.want, LeaderTemplateElects(deploy.Spec.Template))
			assert.Equal(t, tc.want, RenderLeaderRBAC(kvcb).Wanted())
			assert.Equal(t, tc.want, RenderMemberRBAC(kvcb).Wanted())
		})
	}
}

// TestTheElectionPersistsAtOneReplica pins the startup shape needed for a later scale-up.
func TestTheElectionPersistsAtOneReplica(t *testing.T) {
	one := testBackend(func(kvcb *workercore.KVCacheBackend) {
		kvcb.Spec.Connection.Managed.Leader.ElectionBackend = "Kubernetes"
	})

	assert.Contains(t, RenderLeaderFlags(one), "-enable_ha=true")
	assert.True(t, RenderLeaderRBAC(one).Wanted())
	assert.True(t, RenderMemberRBAC(one).Wanted())
	assert.NotEmpty(t, leaderServiceAccountName(one))
	assert.NotEmpty(t, memberServiceAccountName(one))
	assert.Equal(t, LeaderServiceHost(one)+":50051", MemberMasterEntry(one),
		"and the member is pointed at the Service, not at a Lease nobody takes")

	deploy := RenderLeaderDeployment(one, "mooncake:v0.3.13")
	require.NotNil(t, deploy.Spec.Replicas)
	assert.Equal(t, int32(1), *deploy.Spec.Replicas)
	for _, e := range deploy.Spec.Template.Spec.Containers[0].Env {
		if e.Name == LeaderPodIPEnv {
			assert.NotEmpty(t, e.ValueFrom)
		}
	}

	two := testBackend(func(kvcb *workercore.KVCacheBackend) {
		kvcb.Spec.Connection.Managed.Leader.ElectionBackend = "Kubernetes"
		kvcb.Spec.Connection.Managed.Leader.Replicas = ptr.To[int32](2)
	})
	assert.Contains(t, RenderLeaderFlags(two), "-enable_ha=true",
		"the same field turns live the moment there is something to elect")
	assert.True(t, RenderLeaderRBAC(two).Wanted())
	assert.Equal(t, LeaderServiceHost(two)+":50051", MemberMasterEntry(two))

	// The reader the aligner judges a live template with agrees with the renderer both ways.
	assert.True(t, LeaderTemplateElects(deploy.Spec.Template))
	assert.True(t, LeaderTemplateElects(RenderLeaderDeployment(two, "mooncake:v0.3.13").Spec.Template))
}

// TestRenderLeaderRBAC_FollowsTheField pins that the access exists exactly when the election does.
//
// Both directions are asserted, because they fail differently and only one of them is loud. Without
// the objects a leader that asked for HA retries its election forever and never becomes ready; with
// them on a backend that did not ask, an account able to take a Lease outlives the reason it was
// granted.
func TestRenderLeaderRBAC_FollowsTheField(t *testing.T) {
	assert.False(t, RenderLeaderRBAC(testBackend()).Wanted(),
		"a backend with electionBackend None renders no API access")

	got := RenderLeaderRBAC(haBackend())
	require.True(t, got.Wanted())
	require.NotNil(t, got.ServiceAccount)
	require.NotNil(t, got.Role)
	require.NotNil(t, got.RoleBinding)

	// One name for all three, and it is the name the PodSpec asks for. A binding that names an
	// account nobody runs as grants nothing and reports nothing.
	for _, name := range []string{
		got.ServiceAccount.Name, got.Role.Name, got.RoleBinding.Name,
		got.RoleBinding.Subjects[0].Name,
	} {
		assert.Equal(t, "mooncake-dram-leader", name)
	}
	assert.Equal(t, "mooncake-dram-leader", leaderServiceAccountName(haBackend()))
	assert.Empty(t, leaderServiceAccountName(testBackend()),
		"without election the PodSpec names no account")

	for _, ns := range []string{
		got.ServiceAccount.Namespace, got.Role.Namespace, got.RoleBinding.Namespace,
		got.RoleBinding.Subjects[0].Namespace,
	} {
		assert.Equal(t, kuberess.SystemNamespaceName, ns)
	}
}

// TestRenderLeaderRBAC_GrantsExactlyWhatTheBackendCalls asserts the WHOLE rule set rather than
// probing it for the verbs the store needs.
//
// A subset assertion cannot fail on the error that matters here. Every functional test passes just
// as well against a rule that over-grants -- "leases: *" would satisfy any check written as "can it
// elect" -- so the only assertion with information in it is one that also fails when a verb is
// added.
//
// LIMITED: exactness is all this can hold. It cannot tell whether the set is the RIGHT one, because
// the renderer and this list are written from the same reading and agree by construction; an
// earlier draft of both carried `watch`, which the store's Go wrapper offers and its C++ side never
// calls. Deciding the set is a question for the store's sources, not for a test.
func TestRenderLeaderRBAC_GrantsExactlyWhatTheBackendCalls(t *testing.T) {
	got := RenderLeaderRBAC(haBackend())
	require.NotNil(t, got.Role)

	assert.Equal(t, []rbac.PolicyRule{
		{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{"leases"},
			Verbs:     []string{"create", "get", "update"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"patch"},
		},
	}, got.Role.Rules)

	// A Role, never a ClusterRole: everything it names lives in one namespace and nothing reads
	// across. Asserted on the rendered type because "we meant to use a Role" is not enforceable.
	assert.Equal(t, "Role", got.RoleBinding.RoleRef.Kind)
	assert.Equal(t, rbac.GroupName, got.RoleBinding.RoleRef.APIGroup)
	assert.Equal(t, rbac.ServiceAccountKind, got.RoleBinding.Subjects[0].Kind)
}

// TestRenderMemberRBAC_ReadsAndNothingElse is the whole point of the member having an account of
// its own rather than reusing the leader's.
//
// A member using Lease addressing reads the holder and re-reads it to follow an election. Sharing
// the leader's account would hand every member `create` and `update`, which is
// the ability to TAKE the Lease -- so the assertion that matters is not that the read is granted
// but that nothing else is.
func TestRenderMemberRBAC_ReadsAndNothingElse(t *testing.T) {
	assert.False(t, RenderMemberRBAC(testBackend()).Wanted(),
		"without HA a member connects to an address and needs no API access")

	got := RenderMemberRBAC(haBackend())
	require.True(t, got.Wanted())
	require.NotNil(t, got.Role)

	assert.Equal(t, []rbac.PolicyRule{
		{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{"leases"},
			Verbs:     []string{"get"},
		},
	}, got.Role.Rules)

	// A different account from the leader's, asserted by name. Same-name would be the failure this
	// separation exists to prevent, and it would pass every test written as "can a member read".
	leader := RenderLeaderRBAC(haBackend())
	assert.NotEqual(t, leader.ServiceAccount.Name, got.ServiceAccount.Name)
	assert.Equal(t, "mooncake-dram-member", got.ServiceAccount.Name)
	assert.Equal(t, "mooncake-dram-member", memberServiceAccountName(haBackend()))
	assert.Empty(t, memberServiceAccountName(testBackend()))
}

// TestMemberMasterEntry_UsesServiceWithAndWithoutHA pins the member address across both modes.
func TestMemberMasterEntry_UsesServiceWithAndWithoutHA(t *testing.T) {
	assert.Equal(t, LeaderServiceHost(testBackend())+":50051", MemberMasterEntry(testBackend()),
		"without HA it is the leader Service and the RPC port, as it was before HA existed")

	assert.Equal(t, LeaderServiceHost(haBackend())+":50051", MemberMasterEntry(haBackend()),
		"with HA it reaches the elected leader through the Service")

	assert.Contains(t, RenderLeaderFlags(haBackend()),
		"-ha_backend_connstring="+kuberess.SystemNamespaceName+"/mooncake-dram-leader")
}

// TestMemberMasterEntry_AddressingSelectsExplicitLease checks the default and both field values.
func TestMemberMasterEntry_AddressingSelectsExplicitLease(t *testing.T) {
	addressed := func(value string) *workercore.KVCacheBackend {
		return haBackend(func(kvcb *workercore.KVCacheBackend) {
			kvcb.Spec.Connection.Managed.Leader.HighAvailability.MemberAddressing = value
		})
	}

	service := LeaderServiceHost(testBackend()) + ":50051"
	lease := "k8s://" + kuberess.SystemNamespaceName + "/mooncake-dram-leader"

	assert.Equal(t, service, MemberMasterEntry(addressed("")))
	assert.Equal(t, lease, MemberMasterEntry(addressed(MemberAddressingLease)))
	assert.Equal(t, service, MemberMasterEntry(addressed(MemberAddressingService)),
		"the Service publishes only ready endpoints, and a standby is not ready")

	accountFor := func(value string) string {
		return RenderMemberDaemonSet(addressed(value), 0, "mooncake:v0.3.13").
			Spec.Template.Spec.ServiceAccountName
	}
	assert.NotEmpty(t, accountFor(MemberAddressingLease),
		"the equality below is worth nothing if both arms render no account at all")
	assert.Equal(t, accountFor(MemberAddressingLease), accountFor(MemberAddressingService),
		"the member account stays mounted under both values")
}

// TestRenderLeaderRBAC_IsPerBackend pins that two backends do not share an account.
//
// The failure it guards is sameness, which a single-object assertion cannot see: a name built from
// a constant reads correctly on its own and gives every leader in the namespace the same identity,
// so one backend's leader could hold another's Lease.
func TestRenderLeaderRBAC_IsPerBackend(t *testing.T) {
	first := RenderLeaderRBAC(haBackend(func(kvcb *workercore.KVCacheBackend) {
		kvcb.Name = "alpha"
	}))
	second := RenderLeaderRBAC(haBackend(func(kvcb *workercore.KVCacheBackend) {
		kvcb.Name = "beta"
	}))

	assert.Equal(t, "alpha-leader", first.ServiceAccount.Name)
	assert.Equal(t, "beta-leader", second.ServiceAccount.Name)
	assert.NotEqual(t, first.RoleBinding.Subjects[0].Name, second.RoleBinding.Subjects[0].Name)
}
