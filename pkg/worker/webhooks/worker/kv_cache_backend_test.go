package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// newKVCacheBackend builds a managed backend that passes every rule, so a case mutates exactly the
// one thing it is about. The image is set on the object, which keeps these cases off the settings
// read.
func newKVCacheBackend() *workercore.KVCacheBackend {
	return &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "mooncake-dram"},
		Spec: workercore.KVCacheBackendSpec{
			Type:  "Mooncake",
			Image: "example.com/mooncake:v0",
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Leader: workercore.KVCacheBackendLeader{
						Replicas:           ptr.To[int32](1),
						AllocationStrategy: "FreeRatioFirst",
					},
					Members: []workercore.KVCacheBackendMember{{
						NodeSelector:      map[string]string{"kvcache-dram": "true"},
						Medium:            "DRAM",
						CapacityPerMember: resource.MustParse("500Gi"),
					}},
				},
			},
		},
	}
}

// newExternalKVCacheBackendSpec is an external backend naming both endpoint roles.
func newExternalKVCacheBackendSpec() workercore.KVCacheBackendSpec {
	return workercore.KVCacheBackendSpec{
		Type:  "Mooncake",
		Image: "example.com/mooncake:v0",
		Connection: workercore.KVCacheBackendConnection{
			External: &workercore.KVCacheBackendExternal{
				Endpoints: []workercore.KVCacheBackendEndpoint{
					{Name: workercore.KVCacheBackendEndpointNameClient, Address: "mc.example:50051"},
					{Name: workercore.KVCacheBackendEndpointNameAdmin, Address: "mc.example:9003"},
				},
			},
		},
	}
}

// kvCacheBackendCase is one admission case. wantMsg empty means the object must be ACCEPTED;
// otherwise it is the substring the refusal must carry.
//
// A refusal is asserted by its message and not merely by being an error. These rules live in a
// webhook rather than in the schema precisely because the message can say what to do about them, so
// a test that only checks "an error happened" would not be testing the reason the code is here.
type kvCacheBackendCase struct {
	name    string
	mutate  func(kvcb *workercore.KVCacheBackend)
	wantMsg string
}

func runKVCacheBackendCases(
	t *testing.T, cases []kvCacheBackendCase,
	validate func(wh *KVCacheBackendWebhook, oldKvcb, newKvcb *workercore.KVCacheBackend) error,
) {
	t.Helper()

	wh := &KVCacheBackendWebhook{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
			c.mutate(newKvcb)

			err := validate(wh, oldKvcb, newKvcb)
			if c.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), c.wantMsg)
		})
	}
}

// TestKVCacheBackendWebhook_ValidateCreate pins the rules a CRD schema cannot express. The enums and
// defaults are deliberately absent from this table: structural schema validation runs in
// rest.BeforeCreate and the validating admission chain runs after it, so a value outside an enum
// never reaches this handler and a case for one here would assert nothing.
func TestKVCacheBackendWebhook_ValidateCreate(t *testing.T) {
	runKVCacheBackendCases(t, []kvCacheBackendCase{
		{"the canonical managed backend", func(*workercore.KVCacheBackend) {}, ""},

		// The branch choice.
		{"neither branch", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed = nil
		}, "describes nothing"},
		{"both branches", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.External = newExternalKVCacheBackendSpec().Connection.External
		}, "describes two"},

		// The two scope limits.
		{"replicas 1", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.Replicas = ptr.To[int32](1)
		}, ""},
		{"replicas unset, which the schema defaults", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.Replicas = nil
		}, ""},
		{"replicas 3 names the follow-on subject", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.Replicas = ptr.To[int32](3)
		}, "leader high-availability subject"},
		// The second group is DRAM so this case trips the group limit and nothing else. Giving it a
		// medium would trip the medium rule too, and the case would stop saying which rule it pins.
		{"two member groups", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members = append(k.Spec.Connection.Managed.Members,
				workercore.KVCacheBackendMember{
					NodeSelector:      map[string]string{"kvcache-dram-cold": "true"},
					Medium:            "DRAM",
					CapacityPerMember: resource.MustParse("10Ti"),
				})
		}, "a second group is a second medium tier"},

		// A medium the schema accepts because the store supports it, and admission refuses because
		// nothing here renders it. All four are listed: the rule is per medium, not "not DRAM".
		{"a LocalDisk group", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Medium = "LocalDisk"
		}, `only "DRAM" is reconciled`},
		{"a NoF group", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Medium = "NoF"
		}, `only "DRAM" is reconciled`},
		{"a CXL group", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Medium = "CXL"
		}, `only "DRAM" is reconciled`},
		{"a DFS group says what would have to render it", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Medium = "DFS"
		}, "the leader's file or DAX flags and a mount on the member"},

		// Two quantities the schema carries as strings, so no marker can bound them and this is the
		// only place either can be refused.
		{"capacityPerMember of zero", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("0")
		}, "must be greater than 0"},
		{"capacityPerMember negative", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("-1Gi")
		}, "must be greater than 0"},
		{"localBufferSize unset, which is no staging buffer", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].LocalBufferSize = resource.MustParse("0")
		}, ""},
		{"localBufferSize negative", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].LocalBufferSize = resource.MustParse("-1Gi")
		}, "must not be negative"},

		// The upper end of the same two fields. Quantity.Value() does not fail on a number it
		// cannot hold — it answers wrongly, and differently by how the number was written: measured,
		// one below comes back as the MINIMUM int64 and the other as 0. Both then read as "not
		// positive", so the renderer omits the segment size and the member mounts nothing.
		{"capacityPerMember one above the maximum", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("9223372036854775808")
		}, "must not exceed"},
		{"capacityPerMember in a notation that overflows to zero", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("1e30")
		}, "must not exceed"},
		{"capacityPerMember AT the maximum is admitted", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("9223372036854775807")
		}, ""},
		{"localBufferSize one above the maximum", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].LocalBufferSize = resource.MustParse("9223372036854775808")
		}, "must not exceed"},
		{
			// ParseQuantity saturates a binary suffix at the maximum, so by the time admission runs
			// the field holds that maximum and Value() reports it faithfully. Nothing is left to
			// refuse — pinned so a future guard is not written against a case that cannot occur.
			"a binary suffix past the maximum has already been saturated",
			func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("100Ei")
			}, "",
		},

		// An image of blanks is refused at both levels rather than falling back to what it overrides.
		{"an image of blanks", func(k *workercore.KVCacheBackend) {
			k.Spec.Image = "   "
		}, "must not be blank"},
		{"a member image override", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Image = "example.com/mooncake:member"
		}, ""},
		{"a member image override of blanks", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Image = " \t "
		}, "must not be blank"},

		// The escape hatch: a knob this API does not name goes through, one it derives does not.
		{"leader extraArgs of an undeclared flag", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"enable_offload": "true"}
		}, ""},
		{"leader extraArgs colliding with a derived flag", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"allocation_strategy": "random"}
		}, "derived from a field of this spec"},
		{"leader extraArgs pointing the artifact at a config file", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"config_path": "/etc/mc.yaml"}
		}, "silently discarded"},
		// The connector URI is derived and so is caught by the rule above it. The TYPE is not
		// rendered at all — multi-tenancy rides on "file" being the artifact's own default — so
		// without its own entry this is the one key that can move the policy store out from under
		// the seeded file while every rendered flag still looks right.
		{"leader extraArgs changing the kind of quota policy store", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{
				"tenant_quota_connector_type": "etcd",
			}
		}, "a store nothing reads"},
		{"leader extraArgs with both rpc_address and rpc_interface", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{
				"rpc_address":   "10.0.0.1:50051",
				"rpc_interface": "eth0",
			}
		}, "mutually exclusive"},
		{"leader extraArgs with only rpc_interface", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"rpc_interface": "eth0"}
		}, ""},
		{"member extraArgs of a tiering knob", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{"enable_ssd_offload": "true"}
		}, ""},
		{"member extraArgs colliding with a derived config key", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{"global_segment_size": "1Gi"}
		}, "derived from a field of this spec"},

		// The object's own name is a DNS subdomain; the objects rendered from it are DNS-1035
		// labels. The gap between those two rules is what these pin.
		{"a name with a dot in it", func(k *workercore.KVCacheBackend) {
			k.Name = "team.cache"
		}, `renders an object named "team.cache-leader"`},
		{"a name whose leader name would be 64 characters", func(k *workercore.KVCacheBackend) {
			k.Name = strings.Repeat("a", 57)
		}, "renders an object named"},
		// 56 + "-leader" is exactly 63 and passes; 56 + "-member-0" is 65 and does not. The member
		// names are checked too, and this is the only width that proves it.
		{"a name the leader fits but a member does not", func(k *workercore.KVCacheBackend) {
			k.Name = strings.Repeat("a", 56)
		}, "-member-0"},
		{"a name at the widest that renders", func(k *workercore.KVCacheBackend) {
			k.Name = strings.Repeat("a", 54)
		}, ""},
		{"an external backend renders nothing, so only its own rule applies", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Name = "team.cache"
		}, ""},

		// An endpoint address is host:port. The schema types it as a bounded string, which cannot
		// say that, so these are the only place any of it is refused.
		{"an external endpoint with a blank address", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = ""
		}, "must be host:port"},
		{"an external endpoint carrying no port", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = "mc.example"
		}, "must be host:port"},
		{"an external endpoint carrying no host", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = ":50051"
		}, "no host"},
		{"an external endpoint whose port is not a number", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = "mc.example:rpc"
		}, `port "rpc" is not a number`},
		{"an external endpoint whose port is out of range", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = "mc.example:70000"
		}, "outside 1-65535"},
		{"an external endpoint on an IPv6 literal", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = "[2001:db8::1]:50051"
		}, ""},
		// SplitHostPort splits on the last colon and asks nothing about what it split, so this one
		// parses cleanly and then fails where the address is used.
		{"an external endpoint whose host cannot form a URL", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = "bad host:9003"
		}, "cannot be used as an address"},
		// And parsing is not the end of it: url.Parse REINTERPRETS these rather than refusing them.
		// Each was measured to reach a client as something other than what was written — a
		// different host on a different port — so admission has to require that the authority it
		// parsed is the address as given.
		{"an external endpoint carrying a path", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = "bad/path:9003"
		}, `is read as host "bad"`},
		{"an external endpoint carrying a query", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = "mc.example?a=b:9003"
		}, `is read as host "mc.example"`},
		{"an external endpoint carrying a fragment", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = "mc.example#frag:9003"
		}, `is read as host "mc.example"`},
		// The userinfo form needs no rule of its own: a parsed authority never carries it, so the
		// same comparison refuses it — and the message names the host that would have been dialed.
		{"an external endpoint carrying user information", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = "user@mc.example:9003"
		}, `is read as host "mc.example:9003"`},
		// Refused rather than trimmed, because a validating webhook cannot write: normalising for
		// the check alone would admit the untrimmed value, which is what gets stored, copied into
		// status and dialed. http.NewRequest rejects it on every observation thereafter.
		{"an external endpoint padded with spaces", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints[0].Address = " mc.example:9003 "
		}, "must not begin or end with a space"},

		// Kubernetes takes any non-empty image on a Pod, so a string that is not a reference is
		// admitted, rendered verbatim, and fails in the kubelet as InvalidImageName — per Pod, on a
		// node, long after the object was accepted.
		{"a backend image that is not a reference", func(k *workercore.KVCacheBackend) {
			k.Spec.Image = "not a valid image"
		}, "is not a container image reference"},
		{"a member image that is not a reference", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Image = "REGISTRY.example.com/Mooncake:v1"
		}, "is not a container image reference"},

		// LocalObjectReference's name is optional for compatibility, so the schema takes an entry of
		// {} and Pod validation takes it too — it only checks for surrounding whitespace. The
		// reference is copied into both Pod specs and fails at image pull, on a node.
		{"a pull secret with no name", func(k *workercore.KVCacheBackend) {
			k.Spec.ImagePullSecrets = []core.LocalObjectReference{{}}
		}, "name of the referenced secret must be specified"},
		{"a pull secret named something no Secret could be", func(k *workercore.KVCacheBackend) {
			k.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "Registry_Creds"}}
		}, "must consist of lower case alphanumeric characters"},
		{"a pull secret that is fine", func(k *workercore.KVCacheBackend) {
			k.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "registry-creds"}}
		}, ""},

		// The selector is copied into the DaemonSet's pod template, and the API server refuses one
		// that is not made of labels — as a create error inside a reconcile, where the only trace
		// is a line in a log and the group simply never comes up.
		{"a nodeSelector key that is not a label key", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].NodeSelector = map[string]string{"not a key!": "true"}
		}, "is not a label key"},
		{"a nodeSelector value that is not a label value", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].NodeSelector = map[string]string{"kvcache": "not a value!"}
		}, "is not a label value"},

		// An extraArgs key is a bare name. Dashed, it would miss the rule tables and still reach
		// the artifact as the flag they protect — gflags reads "--rpc_port" as "rpc_port".
		{"a leader extraArgs key that is already dashed", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"-rpc_port": "60000"}
		}, "a key is the bare name of a setting"},
		{"a leader extraArgs key dashed around a forbidden flag", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"-config_path": "/etc/mc.yaml"}
		}, "a key is the bare name of a setting"},
		{"a member extraArgs key that is already dashed", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{"--global_segment_size": "1Gi"}
		}, "a key is the bare name of a setting"},
		// The other way a key stops being bare. The renderer joins key and value with "=", so
		// "rpc_port=1" reaches gflags as the flag named before the FIRST one — "rpc_port", the
		// flag the tables exist to protect, which the tables never saw.
		{"a leader extraArgs key carrying its own equals sign", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"rpc_port=1": "60000"}
		}, "a key is the bare name of a setting"},
		{"an extraArgs key carrying a space", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"rpc port": "60000"}
		}, "a key is the bare name of a setting"},
		{"a blank extraArgs key", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"": "60000"}
		}, "a key is the bare name of a setting"},

		// Multi-tenancy is rendered from spec.leader.multiTenancy, so reaching it through the
		// escape hatch is the same two-sources ambiguity every other derived key has. It matters
		// more than most: another CRD's webhook reads the FIELD to decide whether a quota ledger
		// exists, and would never see a string typed here.
		{"a leader extraArgs key that duplicates the multi-tenancy field", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"enable_multi_tenants": "true"}
		}, "this key is derived from a field of this spec"},

		// The external branch needs both roles, because two readers want different addresses.
		{"external naming both roles", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
		}, ""},
		{"external naming only the client role", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints = k.Spec.Connection.External.Endpoints[:1]
		}, `entry named "Admin" is required`},
		{"external naming no role", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
			k.Spec.Connection.External.Endpoints = nil
		}, `entry named "Client" is required`},
	}, func(wh *KVCacheBackendWebhook, _, newKvcb *workercore.KVCacheBackend) error {
		_, err := wh.ValidateCreate(context.Background(), newKvcb)
		return err
	})
}

// TestKVCacheBackendWebhook_ValidateUpdate pins what is frozen under a running backend and, just as
// importantly, what is not: an image, a selector, a capacity, an extraArgs entry and the transport
// block all converge on the next reconcile pass, so freezing them would turn an ordinary edit into
// a recreate.
func TestKVCacheBackendWebhook_ValidateUpdate(t *testing.T) {
	runKVCacheBackendCases(t, []kvCacheBackendCase{
		{"no change", func(*workercore.KVCacheBackend) {}, ""},

		// Frozen.
		{"type changed", func(k *workercore.KVCacheBackend) {
			k.Spec.Type = "Other"
		}, "type is immutable"},
		{"branch switched to external", func(k *workercore.KVCacheBackend) {
			k.Spec = newExternalKVCacheBackendSpec()
		}, "connection branch is immutable"},
		{"member medium changed", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Medium = "LocalDisk"
		}, "medium is immutable"},

		// Editable.
		{"image changed", func(k *workercore.KVCacheBackend) {
			k.Spec.Image = "example.com/mooncake:v1"
		}, ""},
		{"member image set", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Image = "example.com/mooncake-npu:v1"
		}, ""},
		{"node selector widened", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].NodeSelector = map[string]string{"tier": "hot"}
		}, ""},
		{"capacity raised", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("1Ti")
		}, ""},
		{"extraArgs added", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"enable_offload": "true"}
		}, ""},
		{"transport protocol changed", func(k *workercore.KVCacheBackend) {
			k.Spec.Transport.Protocol = "RDMA"
		}, ""},
	}, func(wh *KVCacheBackendWebhook, oldKvcb, newKvcb *workercore.KVCacheBackend) error {
		_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
		return err
	})
}

// TestKVCacheBackendWebhook_ValidateImageFallback pins the one rule that needs a cross-object read:
// the image may come from the object or from the cluster-wide setting, and a backend naming neither
// is refused with a message pointing at both places.
//
// The two halves run in one test and in this order on purpose. A setting value caches for 30s once
// it reads successfully, so seeding the delegated Secret is one-way within a test binary: the
// unset half has to be asserted before anything sets it.
func TestKVCacheBackendWebhook_ValidateImageFallback(t *testing.T) {
	ctx := context.Background()
	wh := &KVCacheBackendWebhook{}

	kvcb := newKVCacheBackend()
	kvcb.Spec.Image = ""

	t.Run("neither the object nor the setting names an image", func(t *testing.T) {
		_, err := wh.ValidateCreate(ctx, kvcb)
		require.Error(t, err)
		require.Contains(t, err.Error(), `"kv-cache-backend-image"`)
		require.Contains(t, err.Error(), "neither carries one")
	})

	// Also before the setting is written, and for the same reason — this subtest needs it unset.
	//
	// The state it reproduces is the one that strands an object forever: it was admitted while the
	// setting carried a value, an admin cleared the setting afterwards, and now every update to it
	// is refused. The reconciler's own removal of the finalizer is an update, and it happens AFTER
	// teardown has already deleted every workload — so refusing it leaves an object that owns
	// nothing and cannot be deleted, by any means short of editing etcd.
	t.Run("an update that leaves the image alone survives a cleared setting", func(t *testing.T) {
		admitted := newKVCacheBackend()
		admitted.Spec.Image = ""
		admitted.Finalizers = []string{systemmeta.LockedResourceFinalizer}

		released := admitted.DeepCopy()
		released.Finalizers = nil

		_, err := wh.ValidateUpdate(ctx, admitted, released)
		require.NoError(t, err,
			"releasing the finalizer must not be refused over a setting that moved after admission")

		// The counterpart, so this is a narrowing and not a hole: an update that MOVES spec.image
		// re-asks the question, because that update is what makes the answer matter again.
		named := newKVCacheBackend()
		cleared := named.DeepCopy()
		cleared.Spec.Image = ""

		_, err = wh.ValidateUpdate(ctx, named, cleared)
		require.Error(t, err, "giving up the object's own image re-asks for the fallback")
		require.Contains(t, err.Error(), "neither carries one")
	})

	// Runs BEFORE the subtest below writes the setting, which is the state that matters: the
	// setting ships blank on purpose, so this is what a default installation looks like. The
	// external branch renders nothing and the reconciler resolves no image for it, so asking the
	// question at all refused every external backend — the shape the documentation shows — on
	// every cluster where nobody had pinned an image yet.
	t.Run("an external backend needs no image from either place", func(t *testing.T) {
		external := newKVCacheBackend()
		external.Spec = newExternalKVCacheBackendSpec()
		external.Spec.Image = ""

		_, err := wh.ValidateCreate(ctx, external)
		require.NoError(t, err)
	})

	t.Run("the setting names one", func(t *testing.T) {
		cli := system.LoopbackCtrlClient.Get()
		sec := &core.Secret{
			ObjectMeta: meta.ObjectMeta{
				Name:      setting.DelegatedSecretName,
				Namespace: setting.DelegatedSecretNamespace,
			},
			Data: map[string][]byte{"kv-cache-backend-image": []byte("example.com/mooncake:pinned")},
		}
		if err := cli.Create(ctx, sec); err != nil {
			got := new(core.Secret)
			require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(sec), got))
			got.Data = sec.Data
			require.NoError(t, cli.Update(ctx, got))
		}

		_, err := wh.ValidateCreate(ctx, kvcb)
		require.NoError(t, err)
	})

	t.Run("an object naming its own image needs no setting", func(t *testing.T) {
		own := newKVCacheBackend()
		_, err := wh.ValidateCreate(ctx, own)
		require.NoError(t, err)
	})
}
