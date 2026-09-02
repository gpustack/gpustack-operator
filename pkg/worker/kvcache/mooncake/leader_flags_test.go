package mooncake

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
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
			name: "extraArgs come last, in key order",
			leader: workercore.KVCacheBackendLeader{
				AllocationStrategy: "FreeRatioFirst",
				ExtraArgs: map[string]string{
					"offload_cap_ratio": "0.5",
					"enable_offload":    "true",
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
				"-enable_offload=true",
				"-offload_cap_ratio=0.5",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, RenderLeaderFlags(c.leader))
		})
	}
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

	first := RenderLeaderFlags(leader)
	for range 20 {
		assert.Equal(t, first, RenderLeaderFlags(leader))
	}
}

// TestRenderLeaderFlags_OmitsWhatThisScopeDoesNotRun pins the absences. They are not an oversight:
// the metadata plane is peer-to-peer so there is no store to point at, electing a leader among
// several is a different subject, and -port is deprecated. A flag appearing here later would be a
// behavior change nobody asked for, so the test names each one.
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
	}

	got := strings.Join(RenderLeaderFlags(workercore.KVCacheBackendLeader{
		Replicas:           ptr.To[int32](1),
		AllocationStrategy: "FreeRatioFirst",
	}), " ")

	for _, flag := range absent {
		assert.NotContains(t, got, flag+"=", "%s must not be rendered", flag)
	}

	// -port is a prefix of -pod_name and of nothing else here; assert it exactly so the check
	// above cannot pass by accident.
	require.NotContains(t, strings.Split(got, " "), "-port=50051")
}
