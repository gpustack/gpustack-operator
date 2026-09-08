package mooncake

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbac "k8s.io/api/rbac/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// haBackend is the shared fixture with the election turned on.
func haBackend(mutate ...func(*workercore.KVCacheBackend)) *workercore.KVCacheBackend {
	all := append([]func(*workercore.KVCacheBackend){
		func(kvcb *workercore.KVCacheBackend) {
			kvcb.Spec.Connection.Managed.Leader.HighAvailability = &workercore.KVCacheBackendLeaderHighAvailability{}
		},
	}, mutate...)
	return testBackend(all...)
}

// TestRenderLeaderRBAC_FollowsTheField pins that the access exists exactly when the election does.
//
// Both directions are asserted, because they fail differently and only one of them is loud. Without
// the objects a leader that asked for HA retries its election forever and never becomes ready; with
// them on a backend that did not ask, an account able to take a Lease outlives the reason it was
// granted.
func TestRenderLeaderRBAC_FollowsTheField(t *testing.T) {
	assert.False(t, RenderLeaderRBAC(testBackend()).Wanted(),
		"a backend without highAvailability renders no API access")

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
		"without HA the PodSpec names no account, which is what it did before this existed")

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
// A member is a client: it reads the Lease holder to find the leader and re-reads it to follow an
// election. Sharing the leader's account would hand every member `create` and `update`, which is
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

// TestMemberMasterEntry_TakesOneOfTwoForms pins F4: HA changes the SCHEME, not the variable.
//
// Both forms are asserted, and the non-HA one exactly, because "unchanged when the feature is off"
// is the guarantee that keeps every existing deployment working -- and it is the one an added
// prefix breaks silently, since the member would still start and simply never find a leader.
func TestMemberMasterEntry_TakesOneOfTwoForms(t *testing.T) {
	assert.Equal(t, LeaderServiceHost(testBackend())+":50051", MemberMasterEntry(testBackend()),
		"without HA it is the leader Service and the RPC port, as it was before HA existed")

	assert.Equal(t, "k8s://"+kuberess.SystemNamespaceName+"/mooncake-dram-leader",
		MemberMasterEntry(haBackend()),
		"with HA it names the Lease, which is what the client reads the leader out of")

	// The two halves have to agree: the entry a member is given must be the Lease the leader takes,
	// or the member follows an election nobody is holding. Compared against the leader's own
	// rendered flag rather than against a second literal, because two literals can drift apart.
	assert.Contains(t, RenderLeaderFlags(haBackend()),
		"-ha_backend_connstring="+kuberess.SystemNamespaceName+"/mooncake-dram-leader")
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
