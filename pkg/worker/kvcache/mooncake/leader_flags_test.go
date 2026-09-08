package mooncake

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
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
				"-allocation_strategy=free_ratio_first",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
		{
			// The keys here are offload TUNING knobs, which stay on the hatch. The tier's own
			// switch became a field and is refused here at admission, so a case setting it would
			// describe an object no API server would accept.
			name: "extraArgs come last, in key order",
			leader: workercore.KVCacheBackendLeader{
				AllocationStrategy: "FreeRatioFirst",
				ExtraArgs: map[string]string{
					"offload_cap_ratio": "0.5",
					"promotion_on_hit":  "true",
					"client_ttl":        "30",
				},
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-allocation_strategy=free_ratio_first",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
				"-client_ttl=30",
				"-offload_cap_ratio=0.5",
				"-promotion_on_hit=true",
			},
		},
		{
			name: "the disk tier's leader half",
			leader: workercore.KVCacheBackendLeader{
				AllocationStrategy: "FreeRatioFirst",
				Offload:            &workercore.KVCacheBackendLeaderOffload{Enabled: true},
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-allocation_strategy=free_ratio_first",
				"-enable_offload=true",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
		{
			name: "the disk tier deferring its writes to eviction time",
			leader: workercore.KVCacheBackendLeader{
				AllocationStrategy: "FreeRatioFirst",
				Offload: &workercore.KVCacheBackendLeaderOffload{
					Enabled: true,
					OnEvict: true,
				},
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-allocation_strategy=free_ratio_first",
				"-enable_offload=true",
				"-offload_on_evict=true",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
		{
			// Admission refuses this pairing, and the renderer drops it too rather than emitting a
			// flag the artifact ands away. Belt and braces on purpose: alone it would be accepted,
			// echoed back in the startup log, and do nothing — the exact shape of a setting that
			// reads as taken and is not.
			name: "onEvict without its switch renders neither flag",
			leader: workercore.KVCacheBackendLeader{
				AllocationStrategy: "FreeRatioFirst",
				Offload:            &workercore.KVCacheBackendLeaderOffload{OnEvict: true},
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-allocation_strategy=free_ratio_first",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
		{
			name: "an offload block that asks for nothing renders nothing",
			leader: workercore.KVCacheBackendLeader{
				AllocationStrategy: "FreeRatioFirst",
				Offload:            &workercore.KVCacheBackendLeaderOffload{},
			},
			want: []string{
				"-rpc_port=50051",
				"-metrics_port=9003",
				"-allocation_strategy=free_ratio_first",
				"-pod_name=$(KUBERNETES_POD_NAME)",
				"-pod_namespace=$(KUBERNETES_POD_NAMESPACE)",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, RenderLeaderFlags(leaderBackend(c.leader)))
		})
	}
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
// the Lease hands to members. Its 0.0.0.0 default would give every replica one identity and send
// every member to an address that resolves back to itself.
func TestRenderLeaderFlags_HighAvailability(t *testing.T) {
	kvcb := leaderBackend(workercore.KVCacheBackendLeader{
		Replicas:           ptr.To[int32](3),
		AllocationStrategy: "FreeRatioFirst",
		HighAvailability:   &workercore.KVCacheBackendLeaderHighAvailability{},
	})

	assert.Equal(t, []string{
		"-rpc_port=50051",
		"-metrics_port=9003",
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
	withHA := RenderLeaderFlags(testBackend(func(kvcb *workercore.KVCacheBackend) {
		kvcb.Spec.Connection.Managed.Leader.HighAvailability = &workercore.KVCacheBackendLeaderHighAvailability{}
	}))
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
	ha := &workercore.KVCacheBackendLeaderHighAvailability{}

	render := func(name string) []string {
		kvcb := testBackend(func(kvcb *workercore.KVCacheBackend) {
			kvcb.Name = name
			kvcb.Spec.Connection.Managed.Leader.HighAvailability = ha
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
// reconciler converges the leader Deployment on every pass, so an argv whose order wandered — a Go
// map range, for instance — would rewrite the object forever.
func TestRenderLeaderFlags_IsDeterministic(t *testing.T) {
	leader := workercore.KVCacheBackendLeader{
		AllocationStrategy: "FreeRatioFirst",
		ExtraArgs: map[string]string{
			"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6", "g": "7", "h": "8",
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
		// The disk tier's two flags. A backend that never declared a tier must not carry them:
		// -enable_offload alone makes the leader queue offload work for members that registered no
		// local disk segment, which its own guard then drops with nothing on the object saying so.
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
