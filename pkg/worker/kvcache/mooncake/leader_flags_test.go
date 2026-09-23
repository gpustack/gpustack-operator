package mooncake

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// TestRenderLeaderFlags asserts the whole argv element by element rather than probing it for
// substrings. A renderer is only diffable if its output is exact, and a golden list is the only
// assertion that fails when a flag is silently added.
func TestRenderLeaderFlags(t *testing.T) {
	cases := []struct {
		name   string
		leader workercore.KVCacheBackendLeader
		want   []string
	}{
		{
			name: "the canonical leader, as admission leaves it",
			leader: workercore.KVCacheBackendLeader{
				Replicas:           ptr.To[int32](1),
				AllocationStrategy: "FreeRatioFirst",
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-default_kv_lease_ttl=5m",
				"-allocation_strategy=free_ratio_first",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
		{
			name: "the other strategy maps to the artifact's own spelling",
			leader: workercore.KVCacheBackendLeader{
				AllocationStrategy: "Random",
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-default_kv_lease_ttl=5m",
				"-allocation_strategy=random",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
		{
			name:   "an unset strategy renders no flag rather than a guess",
			leader: workercore.KVCacheBackendLeader{},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-default_kv_lease_ttl=5m",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
		{
			// The connector URI is rendered WITH the switch and not separately. The master builds
			// its quota policy store when multi-tenancy is on, the file connector refuses the empty
			// URI that is the flag's own default, and the constructor rethrows that refusal — so the
			// switch without the URI is a process that does not start, every time.
			name: "multi-tenancy renders the flag the tenant ledger needs, and the URI without which it will not start",
			leader: workercore.KVCacheBackendLeader{
				Replicas:           ptr.To[int32](1),
				AllocationStrategy: "FreeRatioFirst",
				MultiTenancy:       true,
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-default_kv_lease_ttl=5m",
				"-allocation_strategy=free_ratio_first",
				"-enable_multi_tenants=true",
				"-tenant_quota_connector_uri=/var/lib/mooncake/tenant-quota-policy.yaml",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
		{
			// Both flags are absent rather than rendered false and empty, so a backend nobody asked
			// to be multi-tenant runs the command line it ran before this field existed.
			name: "multi-tenancy off renders nothing at all",
			leader: workercore.KVCacheBackendLeader{
				Replicas:           ptr.To[int32](1),
				AllocationStrategy: "FreeRatioFirst",
				MultiTenancy:       false,
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-default_kv_lease_ttl=5m",
				"-allocation_strategy=free_ratio_first",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
		{
			// The keys here are offload TUNING knobs, which stay on the hatch. The tier's own
			// switch is derived from members[].localDisks and is refused here at admission, so a
			// case setting it would describe an object no API server would accept.
			//
			// Entries carry their own dashes and render verbatim in the order written, which is
			// the order this list spells rather than key order.
			name: "extraArgs come last, in the order written",
			leader: workercore.KVCacheBackendLeader{
				AllocationStrategy: "FreeRatioFirst",
				ExtraArgs: []string{
					"-offload_cap_ratio=0.5",
					"-client_ttl=30",
					"-promotion_on_hit=true",
				},
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-default_kv_lease_ttl=5m",
				"-allocation_strategy=free_ratio_first",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
				"-offload_cap_ratio=0.5",
				"-client_ttl=30",
				"-promotion_on_hit=true",
			},
		},
		{
			// A boolean flag written as one token renders as itself, which is the artifact's own
			// reading of a bare flag. Asserted so a future edit that starts joining an "=value" on
			// cannot pass silently.
			name: "a one-token boolean entry renders as itself",
			leader: workercore.KVCacheBackendLeader{
				AllocationStrategy: "FreeRatioFirst",
				ExtraArgs:          []string{"-client_verbose_logging"},
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-default_kv_lease_ttl=5m",
				"-allocation_strategy=free_ratio_first",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
				"-client_verbose_logging",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, RenderLeaderFlags(leaderBackend(c.leader)))
		})
	}
}

// TestRenderLeaderFlags_DiskTier asserts the offload pair, which is the one part of this argv
// derived from the MEMBERS rather than from the leader spec: a group declaring localDisks is what
// turns the tier on, and no field on the leader says anything about it.
//
// The two flags are asserted as a contiguous pair, because there is no object that renders one
// without the other: -offload_on_evict selects the deferred mode, the only one the store protects,
// so the pair is one decision and renders as one.
func TestRenderLeaderFlags_DiskTier(t *testing.T) {
	withTier := testBackend(func(kvcb *workercore.KVCacheBackend) {
		kvcb.Spec.Connection.Managed.Members[0].LocalDisks = []workercore.KVCacheBackendMemberLocalDisk{{
			Path:     "/var/lib/kvcache",
			Capacity: resource.MustParse("4Ti"),
		}}
	})

	flags := strings.Join(RenderLeaderFlags(withTier), " ")
	assert.Contains(t, flags, "-enable_offload=true -offload_on_evict=true",
		"both flags, together, derived from the one group that declares a tier")

	withoutTier := strings.Join(RenderLeaderFlags(testBackend()), " ")
	assert.NotContains(t, withoutTier, "-enable_offload=",
		"a backend whose groups declare no tier must not queue offload work")
	assert.NotContains(t, withoutTier, "-offload_on_evict=",
		"and never half of a pair whose other half says why")
}

// leaderBackend puts a leader spec on the shared fixture, which is what carries the identity the
// election flags are derived from. The fixture rather than a bare object, so a rename of the
// backend or a change to LeaderObjectName reaches every case here.
func leaderBackend(leader workercore.KVCacheBackendLeader) *workercore.KVCacheBackend {
	return testBackend(func(kvcb *workercore.KVCacheBackend) {
		kvcb.Spec.Connection.Managed.Leader = leader
	})
}

// TestRenderLeaderFlags_HighAvailability asserts the election group, which is the one part of this
// argv derived from the OBJECT rather than from the leader spec.
//
// The five flags are asserted as a contiguous group in order, not probed for individually: they are
// rendered together or not at all, because -enable_ha without a connection string is a leader that
// exits at startup and a connection string without -enable_ha is accepted and then ignored.
//
// -rpc_address is in the group for a reason that is not obvious from its name: the artifact folds it
// with -rpc_port into the string it campaigns with, so it is the election's identity AND the address
// the Lease hands to members that explicitly choose Lease addressing. Its 0.0.0.0 default would
// give every replica one identity and send those members to an address that resolves to itself.
func TestRenderLeaderFlags_HighAvailability(t *testing.T) {
	kvcb := leaderBackend(workercore.KVCacheBackendLeader{
		Replicas:           ptr.To[int32](3),
		AllocationStrategy: "FreeRatioFirst",
		HighAvailability:   &workercore.KVCacheBackendLeaderHighAvailability{},
	})

	assert.Equal(t, []string{
		"-rpc_port=50051",
		"-metrics_port=9003",
		"-default_kv_lease_ttl=5m",
		"-enable_ha=true",
		"-ha_backend_type=k8s",
		"-ha_backend_connstring=" + kuberess.SystemNamespaceName + "/mooncake-dram-leader",
		"-rpc_address=$(KUBERNETES_POD_IP)",
		"-cluster_id=mooncake-dram-leader",
		"-allocation_strategy=free_ratio_first",
		"-pod_name=$(KUBERNETES_POD_NAME)",
		"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
	}, RenderLeaderFlags(kvcb))
}

// TestRenderLeaderFlags_AdvertisedAddressIsPerReplica pins that the address the election campaigns
// with is a REFERENCE the Pod resolves, and that it appears only where an election reads it.
//
// A literal here would be the defect this flag exists to prevent, and it would read perfectly in the
// golden list above: every replica would advertise the same string. The assertion is therefore that
// the value is unresolved argv, paired with the Deployment defining the variable it names.
func TestRenderLeaderFlags_AdvertisedAddressIsPerReplica(t *testing.T) {
	withHA := RenderLeaderFlags(haBackend())
	assert.Contains(t, withHA, "-rpc_address=$(KUBERNETES_POD_IP)",
		"a value the Pod resolves, not one this operator could know when it renders")

	deploy := RenderLeaderDeployment(haBackend(), "mooncake:v0.3.13")
	var podIP *core.EnvVar
	for i, e := range deploy.Spec.Template.Spec.Containers[0].Env {
		if e.Name == LeaderPodIPEnv {
			podIP = &deploy.Spec.Template.Spec.Containers[0].Env[i]
		}
	}
	require.NotNil(t, podIP, "the flag names a variable the workload has to define")
	require.NotNil(t, podIP.ValueFrom)
	require.NotNil(t, podIP.ValueFrom.FieldRef)
	assert.Equal(t, "status.podIP", podIP.ValueFrom.FieldRef.FieldPath,
		"and it comes from the Pod's own address, which is the only per-replica value that another "+
			"Pod can reach")

	withoutHA := RenderLeaderFlags(testBackend())
	for _, flag := range withoutHA {
		assert.NotContains(t, flag, "-rpc_address=",
			"without an election nothing reads it, and rendering it would move the bind address")
	}
	for _, e := range RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13").
		Spec.Template.Spec.Containers[0].Env {
		assert.NotEqual(t, LeaderPodIPEnv, e.Name,
			"and a variable no flag reads is dead weight in the manifest")
	}
}

// TestRenderLeaderFlags_ElectionTargetsAreDistinctPerBackend pins that two backends never elect
// through the same Lease or share a cluster_id.
//
// It compares two renders rather than asserting either against a literal, because the failure it
// guards is SAMENESS: a connstring built from a constant, or from a field both objects share, reads
// correctly in a single golden list and puts two stores in one election.
func TestRenderLeaderFlags_ElectionTargetsAreDistinctPerBackend(t *testing.T) {
	render := func(name string) []string {
		kvcb := haBackend(func(kvcb *workercore.KVCacheBackend) {
			kvcb.Name = name
		})
		return RenderLeaderFlags(kvcb)
	}

	first := strings.Join(render("alpha"), " ")
	second := strings.Join(render("beta"), " ")

	require.Contains(t, first, "-ha_backend_connstring="+kuberess.SystemNamespaceName+"/alpha-leader")
	require.Contains(t, second, "-ha_backend_connstring="+kuberess.SystemNamespaceName+"/beta-leader")
	assert.Contains(t, first, "-cluster_id=alpha-leader")
	assert.Contains(t, second, "-cluster_id=beta-leader")
}

// TestRenderLeaderFlags_IsDeterministic pins that one spec renders identically every time. The
// reconciler converges the leader Deployment on every pass, so an argv whose order wandered — a
// sort that looked at the entry rather than the list, for instance — would rewrite the object
// forever.
func TestRenderLeaderFlags_IsDeterministic(t *testing.T) {
	leader := workercore.KVCacheBackendLeader{
		AllocationStrategy: "FreeRatioFirst",
		ExtraArgs: []string{
			"-a=1", "-b=2", "-c=3", "-d=4", "-e=5", "-f=6", "-g=7", "-h=8",
		},
	}

	first := RenderLeaderFlags(leaderBackend(leader))
	for range 20 {
		assert.Equal(t, first, RenderLeaderFlags(leaderBackend(leader)))
	}
}

// TestRenderLeaderFlags_OmitsWhatThisScopeDoesNotRun pins the absences. They are not an oversight:
// the metadata plane is peer-to-peer so there is no store to point at, and -port is deprecated. A
// flag appearing here later would be a behavior change nobody asked for, so the test names each one.
//
// The four election flags are in this list for a DIFFERENT reason than the rest, and it is the one
// worth keeping: a backend that does not set leader.highAvailability must render the argv it
// rendered before that field existed. Every other entry is absent because nothing renders it at
// all; these four are absent because the object did not ask.
func TestRenderLeaderFlags_OmitsWhatThisScopeDoesNotRun(t *testing.T) {
	absent := []string{
		"-etcd_endpoints",
		"-enable_ha",
		"-ha_backend_type",
		"-ha_backend_connstring",
		"-enable_http_metadata_server",
		"-http_metadata_server_host",
		"-http_metadata_server_port",
		"-cluster_id",
		"-port",
		// The disk tier's two flags. A backend none of whose groups declares a tier must not carry
		// them: -enable_offload alone makes the leader queue offload work for members that
		// registered no local disk segment, which its own guard then drops with nothing on the
		// object saying so. They are derived from members[].localDisks, and a tier-less object
		// derives neither.
		"-enable_offload",
		"-offload_on_evict",
	}

	got := strings.Join(RenderLeaderFlags(leaderBackend(workercore.KVCacheBackendLeader{
		Replicas:           ptr.To[int32](1),
		AllocationStrategy: "FreeRatioFirst",
	})), " ")

	for _, flag := range absent {
		assert.NotContains(t, got, flag+"=", "%s must not be rendered", flag)
	}

	// -port is a prefix of -pod_name and of nothing else here; assert it exactly so the check
	// above cannot pass by accident.
	require.NotContains(t, strings.Split(got, " "), "-port=50051")
}

// TestRenderLeaderFlags_LeaseTTLStaysOverridable holds the two mechanics that let an operator
// replace the lease this renderer supplies. Neither is visible from the rendered argv alone, and
// each fails silently on its own: reserving the key turns the operator's entry into an admission
// refusal, and rendering ours after theirs turns it into a value the artifact discards.
//
// The golden lists above already assert that the flag is rendered. What is asserted here is that
// it can still be taken back, which is the half a later edit is liable to remove while every other
// test stays green.
func TestRenderLeaderFlags_LeaseTTLStaysOverridable(t *testing.T) {
	const key = "default_kv_lease_ttl"

	// Reserving the key would make admission refuse the very entry the renderer leaves room for.
	// This is a deliberate omission rather than one nobody has gotten to yet -- see the note above
	// LeaderExtraArgsRules, which records it so the next completeness pass does not add it.
	assert.NotContains(t, LeaderExtraArgsRules.Derived, key,
		"%s must stay off the derived list, because reserving it is what would refuse an "+
			"operator's own value and leave the setting reachable from nowhere", key)

	// The artifact takes the last of two duplicate flags, so an operator's entry wins only by
	// landing after ours. Asserted on the index rather than on membership: both are present either
	// way, and the order is the entire difference between an override and a discarded token.
	got := RenderLeaderFlags(leaderBackend(workercore.KVCacheBackendLeader{
		Replicas:           ptr.To[int32](1),
		AllocationStrategy: "FreeRatioFirst",
		ExtraArgs:          []string{"-" + key + "=90s"},
	}))

	ours := slices.Index(got, "-"+key+"="+LeaderKVLeaseTTL)
	theirs := slices.Index(got, "-"+key+"=90s")
	require.NotEqual(t, -1, ours, "the rendered default must still be present")
	require.NotEqual(t, -1, theirs, "the operator's entry must reach the argv intact")
	assert.Less(t, ours, theirs,
		"the operator's entry must render after the default, because the artifact reads the "+
			"last of two duplicate flags and the earlier one is what it discards")
}
