package worker

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
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

// withDiskTier declares a complete local disk tier: the member's half and the leader's.
//
// It is one helper rather than two because a tier declared on one side only is a REFUSAL, so a
// fixture that set one half would make every case built on it fail for that reason instead of its
// own. Cases that want a half-declared tier remove one explicitly, which reads as the mutation it
// is.
func withDiskTier() func(*workercore.KVCacheBackend) {
	return func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members[0].LocalDisk = &workercore.KVCacheBackendMemberLocalDisk{
			Path:     "/var/lib/kvcache",
			Capacity: resource.MustParse("4Ti"),
		}
		k.Spec.Connection.Managed.Leader.Offload = &workercore.KVCacheBackendLeaderOffload{Enabled: true}
	}
}

// withDiskPath is a complete tier whose path is the thing under test.
func withDiskPath(path string) func(*workercore.KVCacheBackend) {
	return func(k *workercore.KVCacheBackend) {
		withDiskTier()(k)
		k.Spec.Connection.Managed.Members[0].LocalDisk.Path = path
	}
}

// withGrace sets a scale-in grace on a backend that has a tier for it to reach.
func withGrace(seconds int32) func(*workercore.KVCacheBackend) {
	return func(k *workercore.KVCacheBackend) {
		withDiskTier()(k)
		k.Spec.Connection.Managed.ScaleIn = &workercore.KVCacheBackendScaleIn{GracePeriodSeconds: seconds}
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

// oneBucketMsg is the capacity refusal, built from the same constant the rule reads rather than
// from a copy of the number. The value itself is pinned once, in the renderer's own package, against
// the store's defaults it has to stay under — a literal here would only pin it a second time, and
// would go red on the edit that legitimately moved it.
var oneBucketMsg = fmt.Sprintf("must be at least %s, which is one bucket", memberBucketSize.String())

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

// TestKVCacheBackend_LeaderReplicaCeilingIsTheSameInBothLayers holds the webhook's threshold equal
// to the schema's, because the ceiling is expressed twice in two different languages.
//
// It asserts EQUALITY, never the value 1. A test pinning both to 1 would be edited away by the same
// change that raises the ceiling, which is exactly when this needs to fire; a test pinning them to
// each other reddens when one moves without the other. The pairing it prevents is the worst one:
// admission accepting a value the schema then rejects, which surfaces as an error against a field
// the user did not get wrong.
//
// LIMITED: this reads the FIRST version carrying a schema. The CRD has one today, so the two are the
// same thing; once it has several, a ceiling that drifted in any later version would not redden
// here.
func TestKVCacheBackend_LeaderReplicaCeilingIsTheSameInBothLayers(t *testing.T) {
	crd, ok := workercore.GetCustomResourceDefinitions()["KVCacheBackend"]
	require.True(t, ok, "the KVCacheBackend CRD is generated under this key")

	var schema *apiext.JSONSchemaProps
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Schema != nil && crd.Spec.Versions[i].Schema.OpenAPIV3Schema != nil {
			schema = crd.Spec.Versions[i].Schema.OpenAPIV3Schema
			break
		}
	}
	require.NotNil(t, schema, "the CRD carries a structural schema")

	// spec.connection.managed.leader.replicas, walked rather than matched as a string so that a
	// field moving is a failure here instead of a silently skipped assertion.
	node := schema
	for _, step := range []string{"spec", "connection", "managed", "leader", "replicas"} {
		next, found := node.Properties[step]
		require.True(t, found, "the schema still has a %q under this path", step)
		node = &next
	}

	require.NotNil(t, node.Maximum,
		"the schema carries the ceiling too, so it still holds when the webhook is not installed")
	assert.Equal(t, float64(MaxLeaderReplicas), *node.Maximum,
		"the webhook and the schema must be raised together, or one admits what the other rejects")
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
		// The pairing rule, in both directions. Refusing the first without accepting the second
		// would be a rule nobody can satisfy, and accepting the second without refusing the first
		// is the configuration it exists to prevent: several leaders, each one serving.
		{"replicas 3 without the field that elects", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.Replicas = ptr.To[int32](3)
		}, "requires leader.highAvailability"},
		{"replicas 3 with it", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.Replicas = ptr.To[int32](3)
			k.Spec.Connection.Managed.Leader.HighAvailability = &workercore.KVCacheBackendLeaderHighAvailability{}
		}, ""},
		// The ceiling still applies WITH the field: HA lifts the pairing rule, not the bound.
		{"replicas past the ceiling, even with the field", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.Replicas = ptr.To[int32](MaxLeaderReplicas + 1)
			k.Spec.Connection.Managed.Leader.HighAvailability = &workercore.KVCacheBackendLeaderHighAvailability{}
		}, "at most"},
		// One replica with the field is accepted rather than refused as pointless. A Lease held by a
		// single leader is what lets its replacement pick the same record up after a restart.
		{"replicas 1 with the field", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.Replicas = ptr.To[int32](1)
			k.Spec.Connection.Managed.Leader.HighAvailability = &workercore.KVCacheBackendLeaderHighAvailability{}
		}, ""},
		// The oplog key: refused because the leader cannot START with it, not because this operator
		// took a view on what it writes. The message says which backend is missing, because the flag
		// itself is supported upstream and a message denying that sends the reader to the wrong
		// project.
		{"enable_oplog in the leader's extraArgs", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"enable_oplog": "true"}
		}, "refuses to start"},
		{"enable_oplog set to false is refused too", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"enable_oplog": "false"}
		}, "refuses to start"},
		// The election's four flags are rendered from the field, so reaching them through the hatch
		// is the ambiguity every other derived key is refused for.
		{"enable_ha through the escape hatch", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"enable_ha": "true"}
		}, "derived from a field"},
		{"ha_backend_connstring through the escape hatch", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"ha_backend_connstring": "other/lease"}
		}, "derived from a field"},
		{"cluster_id through the escape hatch", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"cluster_id": "shared"}
		}, "derived from a field"},
		// A second group is admitted. It used to be refused as "a second medium tier", which the
		// tiering work has now answered — a tier is a layer on a group rather than a group of its
		// own, so a second group is just more nodes and nothing here has to arbitrate between them.
		{"two member groups", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members = append(k.Spec.Connection.Managed.Members,
				workercore.KVCacheBackendMember{
					NodeSelector:      map[string]string{"kvcache-dram-cold": "true"},
					Medium:            "DRAM",
					CapacityPerMember: resource.MustParse("10Ti"),
				})
		}, ""},

		// There is deliberately NO case here for a medium outside the enum. The schema carries one
		// value, so LocalDisk, NoF, CXL and DFS are refused in rest.BeforeCreate and never reach
		// this handler — the four cases that used to live here asserted a rule that no request can
		// reach any more, and a test for one would pass against a webhook that had stopped running.

		// The disk tier's two halves. Each of these refuses a combination the store ACCEPTS and
		// then quietly does not honor, which is why they are here rather than in the schema. Both
		// directions are covered, because each half alone fails in its own way and an operator who
		// set the other one needs to hear which is missing.
		{"a disk tier with its leader half", withDiskTier(), ""},
		{"a disk tier without the leader half", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Leader.Offload = nil
		}, "the leader is what decides a key goes to disk"},
		{"the leader half with no disk tier anywhere", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.Offload = &workercore.KVCacheBackendLeaderOffload{Enabled: true}
		}, "would queue offload work for members that have nowhere to put it"},
		{"onEvict without enabled", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Leader.Offload = &workercore.KVCacheBackendLeaderOffload{OnEvict: true}
		}, "the store ands the two together"},
		{"onEvict with enabled", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Leader.Offload = &workercore.KVCacheBackendLeaderOffload{Enabled: true, OnEvict: true}
		}, ""},

		// The path becomes a hostPath, so what the kubelet would refuse inside a reconcile is
		// refused here instead, where the message reaches the person who wrote it.
		{"a relative disk path", withDiskPath("var/lib/kvcache"), "must be an absolute path"},
		{
			"a disk path that is the root directory", withDiskPath("/"),
			"must not be the root directory",
		},
		{
			// Spelled with a trailing "/." rather than with "..", because a path containing ".."
			// is refused one rule earlier and would no longer reach this one.
			"a disk path that is the root directory by another spelling", withDiskPath("/."),
			"must not be the root directory",
		},
		{
			"a disk path with surrounding space", withDiskPath("/var/lib/kvcache "),
			"must not begin or end with whitespace",
		},
		{
			// A tab, not a space: the rule is TrimSpace and the message says whitespace, so the
			// case that pins them together must use something a reader would not call a space.
			"a disk path with a leading tab", withDiskPath("\t/var/lib/kvcache"),
			"must not begin or end with whitespace",
		},
		// The store refuses a path with a ".." component statically, before it looks at the
		// filesystem at all — so one admitted here produces a member that starts, never mounts its
		// tier, and says why only in a container log.
		{
			"a disk path with a parent-directory component", withDiskPath("/var/lib/../tier"),
			`must not contain a ".." component`,
		},
		{
			// Climbing all the way out is the same rule, not the root-directory one: the check runs
			// on the raw components, before Clean would resolve this to "/".
			"a disk path that climbs to the root", withDiskPath("/var/lib/../../"),
			`must not contain a ".." component`,
		},
		{
			// A directory whose NAME merely starts with dots is not traversal. Projected volumes
			// mount exactly this, so a substring check for ".." would refuse a legitimate path.
			"a disk path with a dot-prefixed directory name", withDiskPath("/var/lib/..data"),
			"",
		},

		// The tier and the RDMA device tree are mounted into ONE container, so an overlap is
		// resolved by the kubelet — one shadows the other — rather than reported on this object.
		// Refused whatever the transport is today, because the transport is editable.
		{
			"a disk path that is the RDMA device tree", withDiskPath("/dev/infiniband"),
			"must not overlap /dev/infiniband",
		},
		{
			"a disk path inside the RDMA device tree", withDiskPath("/dev/infiniband/tier"),
			"must not overlap /dev/infiniband",
		},
		{
			"a disk path that CONTAINS the RDMA device tree", withDiskPath("/dev"),
			"must not overlap /dev/infiniband",
		},
		{
			"a disk path that is the EFA libfabric tree", withDiskPath("/opt/amazon/efa"),
			"must not overlap /dev/infiniband or /opt/amazon/efa",
		},
		{
			"a disk path inside the EFA libfabric tree", withDiskPath("/opt/amazon/efa/tier"),
			"must not overlap /dev/infiniband or /opt/amazon/efa",
		},
		{
			"a disk path that CONTAINS the EFA libfabric tree", withDiskPath("/opt/amazon"),
			"must not overlap /dev/infiniband or /opt/amazon/efa",
		},
		{
			// Same shape as the infiniband sibling below: a name that shares a prefix is not an
			// overlap.
			"a disk path that is a sibling of the EFA libfabric tree",
			withDiskPath("/opt/amazon/efa-backup"), "",
		},
		{
			// A sibling whose name merely starts with the same letters is not an overlap. A plain
			// string-prefix test would refuse this one.
			"a disk path that is a sibling of the RDMA device tree",
			withDiskPath("/dev/infiniband-cache"), "",
		},
		{
			// Pins the ORDER of two rules that both match. A traversal that happens to point at the
			// device tree must be refused as traversal, because that is the arm the store's own
			// check corresponds to — and because the overlap arm runs on the CLEANED path, so a
			// reader could otherwise conclude that `..` reaches the store whenever it also
			// collides. Swap the two arms and this case reports the wrong reason while staying
			// green on "an error happened".
			"a traversal that points at the RDMA device tree is refused as traversal",
			withDiskPath("/dev/infiniband/../foo"), `must not contain a ".." component`,
		},
		{"an empty disk path", withDiskPath(""), "a directory on the node is required"},
		{"a negative disk capacity", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("-1Gi")
		}, "must not be negative"},
		{"a disk capacity below one bucket", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("1Mi")
		}, oneBucketMsg},
		{"a fractional disk capacity below one bucket", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("1e-3")
		}, oneBucketMsg},
		{"a disk capacity of exactly one bucket", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = memberBucketSize
		}, ""},
		{"a disk capacity of zero, which is the store's own ceiling", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("0")
		}, ""},

		// The key ceiling, which the store checks in the same breath as the byte one.
		{"a key limit below one bucket's worth", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.KeyLimit = mooncake.MemberBucketKeysLimit - 1
		}, "one bucket's worth of keys"},
		{"a key limit of exactly one bucket's worth", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.KeyLimit = mooncake.MemberBucketKeysLimit
		}, ""},
		{"a key limit of zero, which is the store's own ceiling", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.KeyLimit = 0
		}, ""},

		// The segment a bucket is assembled in. The floor applies only where a tier exists, which is
		// what the second case holds open: without it, a rule refusing every small group would
		// satisfy the first case just as well.
		{"capacityPerMember below one bucket, with a tier", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("1Mi")
		}, "one bucket, the unit the tier is written in"},
		{"capacityPerMember below one bucket, with no tier", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("1Mi")
		}, ""},
		{"capacityPerMember of exactly one bucket, with a tier", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].CapacityPerMember = memberBucketSize
		}, ""},

		// One disk tier per backend, and the bound is the capacity contract rather than the
		// rendering. Two groups where only ONE carries a tier is admitted, which is the case that
		// keeps this rule from being read as "two groups are refused again".
		{"two groups, one disk tier", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members = append(k.Spec.Connection.Managed.Members,
				workercore.KVCacheBackendMember{
					NodeSelector:      map[string]string{"kvcache-plain": "true"},
					Medium:            "DRAM",
					CapacityPerMember: resource.MustParse("10Ti"),
				})
		}, ""},
		{"two groups, two disk tiers", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			second := k.Spec.Connection.Managed.Members[0].DeepCopy()
			second.NodeSelector = map[string]string{"kvcache-cold": "true"}
			second.LocalDisk.Path = "/var/lib/kvcache-cold"
			k.Spec.Connection.Managed.Members = append(k.Spec.Connection.Managed.Members, *second)
		}, "could not say which figure belonged to which group"},
		// The same rule, reached by the OTHER configuration it happens to protect against. This case
		// is not about capacity attribution: both groups name the SAME host directory, so two
		// members meeting on one node would run two stores over one tier. Nothing refuses that on
		// its own — the capacity rule catches it first, for a reason that has nothing to do with it.
		//
		// It is a separate case, with a name that says the other reason, so that relaxing the
		// capacity rule — the day status.capacity can attribute per group — turns THIS one red and
		// tells whoever is relaxing it what else was riding on it.
		{"two groups pointing at the same host directory", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			second := k.Spec.Connection.Managed.Members[0].DeepCopy()
			second.NodeSelector = map[string]string{"kvcache-cold": "true"}
			k.Spec.Connection.Managed.Members = append(k.Spec.Connection.Managed.Members, *second)
		}, "only one member group may declare localDisk"},

		// The grace the departing member waits for. The upper bound is the member endpoint's own:
		// above it the call is answered with a 400, so the hook would fail every time it ran.
		{"a grace period within the endpoint's ceiling", withGrace(30), ""},
		{"a grace period of zero, which deregisters without waiting", withGrace(0), ""},
		{
			"a grace period above the endpoint's ceiling", withGrace(3601),
			"would render a shutdown hook that fails every time it runs",
		},
		{"a negative grace period", withGrace(-1), "must not be negative"},
		// Inert rather than refused: nothing is rendered for it, and refusing it would make
		// declaring a policy depend on the order two independent fields are edited in.
		{"a grace period on a backend with no disk tier", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.ScaleIn = &workercore.KVCacheBackendScaleIn{GracePeriodSeconds: 30}
		}, ""},

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
		// The example is an offload TUNING knob on purpose — the tier's switch itself became a
		// field, and the knobs around it deliberately did not, so this is the distinction the
		// hatch now has to carry.
		{"leader extraArgs of an undeclared flag", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"offload_cap_ratio": "0.7"}
		}, ""},
		{"leader extraArgs reaching for the tier's own switch", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"enable_offload": "true"}
		}, "derived from a field of this spec"},
		{"leader extraArgs reaching for the tier's eviction-time switch", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"offload_on_evict": "true"}
		}, "derived from a field of this spec"},
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
		// Both halves of the advertised address, and neither is reachable any more. They were a
		// mutually-exclusive pair until the election started rendering rpc_address: the artifact
		// folds it into the string it campaigns with, so a value from here decides which host every
		// member connects to — and cannot know whether the Pod answers there.
		{"leader extraArgs with rpc_address", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{
				"rpc_address": "10.0.0.1:50051",
			}
		}, "derived from a field of this spec"},
		{"leader extraArgs with rpc_interface", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"rpc_interface": "eth0"}
		}, "derived from a field of this spec"},
		// A key deliberately left reachable, and the one with the strongest reason: the renderer
		// leaves MOONCAKE_DEVICE unset because one DaemonSet covers every node its group selects
		// and an RDMA device is named per host, so no single name could be rendered for the group.
		// The hatch is how an operator on heterogeneous hardware gets in.
		{"member extraArgs of a key left reachable on purpose", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{"device_name": "mlx5_0"}
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

		// The disk tier's MEMBER half, which is the side an override actually wins on: a member's
		// extraArgs renders as the entrypoint's own -D, applied after the environment. Left
		// reachable, ssd_offload_path takes a host path that never went through any of the rules
		// above — absolute, not root, no "..", clear of the RDMA device tree — and
		// enable_ssd_offload switches the tier off while the leader keeps queueing offload work.
		// The leader's two halves are covered further up; these are here because a fix on one side
		// of a pair is not a fix.
		{"member extraArgs redirecting the tier's host path", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{
				"ssd_offload_path": "/etc",
			}
		}, "derived from a field of this spec"},
		{"member extraArgs switching the tier off", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{
				"enable_ssd_offload": "false",
			}
		}, "derived from a field of this spec"},
		// The tier's third rendered key is NOT derived, and this pins the difference: it has no
		// field on the client's config object, so a -D of that name sets a key nothing reads and
		// the tier's ceiling is untouched. Refusing it would be refusing a no-op.
		{"member extraArgs naming the tier's size limit", func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			k.Spec.Connection.Managed.Members[0].ExtraArgs = map[string]string{
				"offload_total_size_limit_bytes": "1",
			}
		}, ""},

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

// TestKVCacheBackendWebhook_AGrandfatheredExtraArgIsNotRefusedOnEveryUpdate pins that a forbidden
// key already on an admitted object does not make that object un-updatable.
//
// The failure this prevents is not a lenient rule, it is a STRANDED OBJECT. This list grows --
// `enable_oplog` joined it with the high-availability work -- and every addition retroactively
// condemns objects admitted before it existed. Not every update is the user's: the reconciler
// removes this object's finalizer through one, so a refusal there leaves an object that owns nothing
// and cannot be deleted, after teardown has already removed its workloads.
//
// Three cases, because only the three together say where the line is: untouched is admitted, touched
// is refused, and a create is refused whatever it carries.
func TestKVCacheBackendWebhook_AGrandfatheredExtraArgIsNotRefusedOnEveryUpdate(t *testing.T) {
	wh := &KVCacheBackendWebhook{}

	withOplog := func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"enable_oplog": "true"}
	}

	t.Run("an update that does not touch the map is admitted", func(t *testing.T) {
		oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
		withOplog(oldKvcb)
		withOplog(newKvcb)
		// An edit somewhere else entirely, which is what an ordinary update looks like.
		newKvcb.Spec.Image = "example.com/mooncake:v1"

		_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
		require.NoError(t, err,
			"a key admitted before the rule existed must not refuse every later update")
	})

	t.Run("an update that does not change anything is admitted", func(t *testing.T) {
		oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
		withOplog(oldKvcb)
		withOplog(newKvcb)

		// This is the reconciler removing the finalizer: the spec is untouched. Refusing it is what
		// strands the object.
		_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
		require.NoError(t, err)
	})

	t.Run("an update that touches the map is refused", func(t *testing.T) {
		oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
		withOplog(oldKvcb)
		newKvcb.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{
			"enable_oplog": "false",
		}

		_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
		require.Error(t, err, "editing the value is touching the map, so the rule applies")
		require.Contains(t, err.Error(), "refuses to start")
	})

	t.Run("a create carrying it is refused", func(t *testing.T) {
		kvcb := newKVCacheBackend()
		withOplog(kvcb)

		_, err := wh.ValidateCreate(context.Background(), kvcb)
		require.Error(t, err)
		require.Contains(t, err.Error(), "refuses to start")
	})

	// The exemption's own blind spot: switching high availability on is what MAKES these keys
	// derived, so an update that moves the field has to re-read a map it did not touch. Left
	// exempt, `enable_ha=false` grandfathered from before the key was derived reaches the renderer,
	// which appends the escape hatch after the derived flags -- so it wins over the `-enable_ha=true`
	// the election needs, and several unelected masters are admitted by a rule that only ever asked
	// whether the map had moved.
	t.Run("turning high availability on re-reads a map it did not touch", func(t *testing.T) {
		oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
		grandfathered := map[string]string{"enable_ha": "false"}
		oldKvcb.Spec.Connection.Managed.Leader.ExtraArgs = grandfathered
		newKvcb.Spec.Connection.Managed.Leader.ExtraArgs = maps.Clone(grandfathered)
		newKvcb.Spec.Connection.Managed.Leader.HighAvailability = &workercore.KVCacheBackendLeaderHighAvailability{}

		_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
		require.Error(t, err, "the field moved, so the keys it derives are read again")
		require.Contains(t, err.Error(), "derived from a field of this spec")
	})

	t.Run("an unrelated update still leaves that same map alone", func(t *testing.T) {
		oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
		grandfathered := map[string]string{"enable_ha": "false"}
		oldKvcb.Spec.Connection.Managed.Leader.ExtraArgs = grandfathered
		newKvcb.Spec.Connection.Managed.Leader.ExtraArgs = maps.Clone(grandfathered)
		newKvcb.Spec.Image = "example.com/mooncake:v1"

		// The positive baseline for the case above: without it, a rule that refused every update
		// would pass that assertion just as well, and this is the update the exemption exists for.
		_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
		require.NoError(t, err)
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
		// The schema enumerates one medium, so this rule cannot fire against any object an API
		// server would accept today, and the value below is deliberately not a medium name that
		// ever existed — a real-looking one would read as though the enum still carried it. The
		// rule and this case are both kept for the day the enum widens, when a medium would
		// otherwise become quietly mutable under segments already mounted from it.
		{"member medium changed", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].Medium = "SomeFutureMedium"
		}, "medium is immutable"},

		// The disk tier is frozen in whether it exists and where it lives, because both strand
		// what the members already wrote. What it may HOLD is editable: that re-renders one
		// environment variable and the tier's contents survive the restart.
		// Only the "added" direction fits this table, whose old object never has a tier. The other
		// three each need an old object that already carries one, and live in their own test.
		{
			"disk tier added to a running group", withDiskTier(),
			"cannot be added to the group at this position",
		},

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
			k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{"offload_cap_ratio": "0.7"}
		}, ""},
		{"transport protocol changed", func(k *workercore.KVCacheBackend) {
			k.Spec.Transport.Protocol = "RDMA"
		}, ""},
	}, func(wh *KVCacheBackendWebhook, oldKvcb, newKvcb *workercore.KVCacheBackend) error {
		_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
		return err
	})
}

// TestKVCacheBackendWebhook_MultiTenancyCannotBeWithdrawnFromAClaimedBackend is the second of the
// three exits from the deadlock issue 164 reports: a create-time invariant that never protected the
// later edit.
//
// A KVCachePool is refused at creation when its backend runs without a tenant ledger, and nothing
// refused taking the ledger away afterwards — which strands the pool's own finalizer on a master that
// has no ledger to release from.
//
// The refusal needs its POSITIVE baseline, which is why the table carries the unclaimed backend and
// the opposite direction: a rule that refused every edit to this field, or every edit to a claimed
// backend, would satisfy the one negative case on its own — and would block the very patch the third
// exit exists to make work.
func TestKVCacheBackendWebhook_MultiTenancyCannotBeWithdrawnFromAClaimedBackend(t *testing.T) {
	claim := workercore.KVCacheObjectReference{Kind: "KVCachePool", Name: "team-a-pool"}

	testCases := []struct {
		name string
		// on is what the old object carries, and off what the new one asks for. Spelling both out is
		// what lets the table hold the opposite direction next to the refused one.
		was, now bool
		// claims is what status.usedBy names on BOTH objects. One entry is the ordinary case; the
		// oversized one is what reaches the sampling branch.
		claims []workercore.KVCacheObjectReference
		// external replaces both specs with the branch that carries no leader at all, which is the
		// only way to put a nil Managed in front of this rule.
		external bool
		mutate   func(*workercore.KVCacheBackend)

		wantMsg    string
		wantNotMsg string
	}{
		{
			name: "withdrawn from a backend a pool holds", was: true,
			claims:  []workercore.KVCacheObjectReference{claim},
			wantMsg: "multi-tenancy cannot be turned off while KVCachePool/team-a-pool",
		},
		{
			name: "withdrawn from a backend nothing holds", was: true,
		},
		{
			name: "turned back on while a pool holds it", now: true,
			claims: []workercore.KVCacheObjectReference{claim},
		},
		{
			name: "left on while a pool holds it", was: true, now: true,
			claims: []workercore.KVCacheObjectReference{claim},
		},
		{
			name:   "an unrelated edit to a claimed backend that never had it",
			claims: []workercore.KVCacheObjectReference{claim},
			mutate: func(k *workercore.KVCacheBackend) { k.Spec.Image = "example.com/mooncake:v1" },
		},
		// The refusal names its claimants, and usedBy has no item bound, so past the cap the message
		// samples. Asserted from both ends: the last name that fits is there and the first one past
		// it is not, which a message that simply stopped counting would fail.
		{
			name: "the refusal samples the claimants past the cap", was: true,
			claims:     manyKVCachePoolClaims(kvCacheBackendMaxConsumerNames + 5),
			wantMsg:    "KVCachePool/pool-20 and 5 more",
			wantNotMsg: "KVCachePool/pool-21",
		},
		// The nil-Managed guard. An external backend has no leader block to carry the flag, so this
		// rule has nothing to read — and without the guard it would not decline, it would panic.
		{
			name: "an external backend a pool holds is still updatable", external: true,
			claims: []workercore.KVCacheObjectReference{claim},
			mutate: func(k *workercore.KVCacheBackend) { k.Spec.Image = "example.com/mooncake:v1" },
		},
	}

	wh := &KVCacheBackendWebhook{}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
			if tc.external {
				oldKvcb.Spec, newKvcb.Spec = newExternalKVCacheBackendSpec(), newExternalKVCacheBackendSpec()
			} else {
				oldKvcb.Spec.Connection.Managed.Leader.MultiTenancy = tc.was
				newKvcb.Spec.Connection.Managed.Leader.MultiTenancy = tc.now
			}
			// On BOTH, because status is a subresource: an update to the spec carries whatever status
			// the API server already holds, so a fixture that put the claim on only one of them would
			// be describing a request no client can send.
			oldKvcb.Status.UsedBy = tc.claims
			newKvcb.Status.UsedBy = tc.claims
			if tc.mutate != nil {
				tc.mutate(newKvcb)
			}

			_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
			if tc.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantMsg,
				"an operator whose edit is refused needs to know what to go and remove")
			if tc.wantNotMsg != "" {
				require.NotContains(t, err.Error(), tc.wantNotMsg,
					"past the cap the message samples rather than naming every claimant")
			}
		})
	}
}

// manyKVCachePoolClaims builds n pool claims named pool-1 … pool-n, in the order status.usedBy holds
// them, so a case can assert which of them the refusal names.
func manyKVCachePoolClaims(n int) []workercore.KVCacheObjectReference {
	refs := make([]workercore.KVCacheObjectReference, 0, n)
	for i := 1; i <= n; i++ {
		refs = append(refs, workercore.KVCacheObjectReference{
			Kind: "KVCachePool", Name: fmt.Sprintf("pool-%d", i),
		})
	}
	return refs
}

// TestKVCacheBackendWebhook_DiskTierIsFrozenExceptItsCapacity pins the three edits a group that
// ALREADY has a disk tier can and cannot make.
//
// It needs its own old object, which is why it is not in the table above: that table's old object
// never carries a tier, so every case built on it exercises the "added" rule and nothing else. A
// capacity case run there would have passed for the wrong reason — refused as an addition while
// claiming to prove the capacity is frozen.
func TestKVCacheBackendWebhook_DiskTierIsFrozenExceptItsCapacity(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*workercore.KVCacheBackend)
		wantMsg     string
		oldCapacity string
	}{
		{"capacity raised", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("8Ti")
		}, "", ""},
		// Both directions, because the rule is "the ceiling is not part of the identity" and not
		// "the ceiling may grow". Testing only the raise leaves a lowering free to be refused by a
		// later edit with nothing going red, and the documentation says either way is allowed.
		{"capacity lowered", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("1Ti")
		}, "", ""},
		{"path moved", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].LocalDisk.Path = "/var/lib/elsewhere"
		}, "the path is immutable", ""},
		{"tier removed", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].LocalDisk = nil
			k.Spec.Connection.Managed.Leader.Offload = nil
		}, "cannot be removed from the group at this position", ""},
		{"nothing changed", func(*workercore.KVCacheBackend) {}, "", ""},
		{"a grandfathered sub-bucket capacity is unchanged", func(k *workercore.KVCacheBackend) {
			k.Spec.Image = "example.com/mooncake:v1"
		}, "", "1Mi"},
		{"a grandfathered sub-bucket capacity is changed", func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("2Mi")
		}, oneBucketMsg, "1Mi"},
	}

	wh := &KVCacheBackendWebhook{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
			withDiskTier()(oldKvcb)
			withDiskTier()(newKvcb)
			if c.oldCapacity != "" {
				oldKvcb.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse(c.oldCapacity)
				newKvcb.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse(c.oldCapacity)
			}
			c.mutate(newKvcb)

			_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
			if c.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), c.wantMsg)
		})
	}
}

// TestKVCacheBackendWebhook_ASubBucketGroupCannotAcquireATier holds the one way the bucket floor on
// capacityPerMember could be walked around, which is that the floor only applies to a group with a
// tier while the value itself carries the usual "this update did not move it" exemption.
//
// A group admitted with a sub-bucket capacityPerMember and no tier could, in principle, gain one
// without touching the value that the new tier makes it fail. It cannot — but NOT because of the
// floor: adding a tier to an existing position is refused by the tier's own immutability rule, and a
// group that arrives WITH a tier arrives at a new position, where there is no old value to be exempt
// against. Two rules in different validators cover this between them and neither says so.
//
// So this test exists to make that coverage a standing fact rather than a coincidence. If the tier
// immutability rule is ever relaxed — there are reasons someone might want to — the first case here
// goes green for the wrong reason and the floor needs the exemption narrowed in the same edit.
func TestKVCacheBackendWebhook_ASubBucketGroupCannotAcquireATier(t *testing.T) {
	subBucket := resource.MustParse("1Mi")

	t.Run("a tier added in place, leaving capacityPerMember alone", func(t *testing.T) {
		oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
		oldKvcb.Spec.Connection.Managed.Members[0].CapacityPerMember = subBucket
		withDiskTier()(newKvcb)
		newKvcb.Spec.Connection.Managed.Members[0].CapacityPerMember = subBucket

		_, err := (&KVCacheBackendWebhook{}).ValidateUpdate(context.Background(), oldKvcb, newKvcb)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot be added to the group at this position",
			"the tier immutability rule is what refuses this today; if that changes, the "+
				"capacityPerMember floor's unchanged-value exemption becomes the hole")
	})

	t.Run("a group appended with a tier and a sub-bucket segment", func(t *testing.T) {
		oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
		appended := workercore.KVCacheBackendMember{
			NodeSelector:      map[string]string{"kvcache-cold": "true"},
			Medium:            "DRAM",
			CapacityPerMember: subBucket,
			LocalDisk: &workercore.KVCacheBackendMemberLocalDisk{
				Path: "/var/lib/kvcache", Capacity: resource.MustParse("4Ti"),
			},
		}
		newKvcb.Spec.Connection.Managed.Members = append(
			newKvcb.Spec.Connection.Managed.Members, appended)
		newKvcb.Spec.Connection.Managed.Leader.Offload = &workercore.KVCacheBackendLeaderOffload{Enabled: true}

		_, err := (&KVCacheBackendWebhook{}).ValidateUpdate(context.Background(), oldKvcb, newKvcb)
		require.Error(t, err)
		require.Contains(t, err.Error(), "one bucket, the unit the tier is written in",
			"an appended position has no old value, so the exemption cannot apply and the floor "+
				"is what refuses it")
	})
}

// TestKVCacheBackendWebhook_DiskTierEviction pins the pair rules the eviction block needs, each of
// which refuses a combination the store ACCEPTS and then does not act on.
//
// Every case carries its positive counterpart, because each of these rules is one a blunter version
// would satisfy too: a validator refusing every eviction block, or every disabled one, or every
// watermark, passes the negative cases on its own and forbids the configurations the feature exists
// for.
func TestKVCacheBackendWebhook_DiskTierEviction(t *testing.T) {
	withEviction := func(
		mutate func(*workercore.KVCacheBackendMemberLocalDiskEviction),
	) func(*workercore.KVCacheBackend) {
		return func(k *workercore.KVCacheBackend) {
			withDiskTier()(k)
			eviction := &workercore.KVCacheBackendMemberLocalDiskEviction{}
			mutate(eviction)
			k.Spec.Connection.Managed.Members[0].LocalDisk.Eviction = eviction
		}
	}

	runKVCacheBackendCases(t, []kvCacheBackendCase{
		{"no eviction block at all", withDiskTier(), ""},
		{"an empty block, which the schema reads as enabled", withEviction(
			func(*workercore.KVCacheBackendMemberLocalDiskEviction) {}), ""},
		{"a policy on its own", withEviction(
			func(e *workercore.KVCacheBackendMemberLocalDiskEviction) { e.Policy = "LRU" }), ""},

		// Disabled: the two settings that would read as taken.
		{"disabled on its own", withEviction(
			func(e *workercore.KVCacheBackendMemberLocalDiskEviction) { e.Enabled = ptr.To(false) }), ""},
		{"disabled with a policy", withEviction(
			func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Enabled, e.Policy = ptr.To(false), "FIFO"
			}), "no order in which nothing leaves"},
		{"disabled with a watermark", withEviction(
			func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Enabled = ptr.To(false)
				e.Watermark = &workercore.KVCacheBackendMemberLocalDiskEvictionWatermark{High: 90, Low: 80}
			}), "nothing for these marks to start and stop"},
		// Enabled explicitly, which must not trip either of the two rules above.
		{"enabled with both", withEviction(
			func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Enabled, e.Policy = ptr.To(true), "FIFO"
				e.Watermark = &workercore.KVCacheBackendMemberLocalDiskEvictionWatermark{High: 90, Low: 80}
			}), ""},

		// The band itself.
		{"marks in the wrong order", withEviction(
			func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Watermark = &workercore.KVCacheBackendMemberLocalDiskEvictionWatermark{High: 70, Low: 80}
			}), "must be below high"},
		{"equal marks", withEviction(
			func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Watermark = &workercore.KVCacheBackendMemberLocalDiskEvictionWatermark{High: 80, Low: 80}
			}), "must be below high"},

		// The band is a percentage OF the capacity, and the store's own quota default is a value its
		// eviction path reads as switched off — so a band without one would never fire while reading
		// as configured.
		{"a watermark with no capacity", func(k *workercore.KVCacheBackend) {
			withEviction(func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Watermark = &workercore.KVCacheBackendMemberLocalDiskEvictionWatermark{High: 90, Low: 80}
			})(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("0")
		}, "a capacity is required"},
		{"a policy with no capacity, which needs none", func(k *workercore.KVCacheBackend) {
			withEviction(func(e *workercore.KVCacheBackendMemberLocalDiskEviction) {
				e.Policy = "FIFO"
			})(k)
			k.Spec.Connection.Managed.Members[0].LocalDisk.Capacity = resource.MustParse("0")
		}, ""},
	}, func(wh *KVCacheBackendWebhook, _, newKvcb *workercore.KVCacheBackend) error {
		_, err := wh.ValidateCreate(context.Background(), newKvcb)
		return err
	})
}

// TestKVCacheBackendWebhook_ExtraEnvs pins the environment hatch's two rules.
//
// The accepted cases are what make the rule a rule rather than a ban: this hatch exists so the
// store's environment-only settings are reachable at all, and a validator refusing anything that
// looks like one of ours would take the feature away while passing every refusal below.
func TestKVCacheBackendWebhook_ExtraEnvs(t *testing.T) {
	withEnvs := func(envs map[string]string) func(*workercore.KVCacheBackend) {
		return func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members[0].ExtraEnvs = envs
		}
	}

	runKVCacheBackendCases(t, []kvCacheBackendCase{
		{"a variable this operator does not render", withEnvs(map[string]string{
			"MOONCAKE_OFFLOAD_USE_URING": "true",
		}), ""},
		{"several of them", withEnvs(map[string]string{
			"MOONCAKE_OFFLOAD_USE_URING":                  "true",
			"MOONCAKE_OFFLOAD_HEARTBEAT_INTERVAL_SECONDS": "5",
		}), ""},

		// One derived name per layer of the rendering, so a list that lost a whole layer reddens.
		{"the tier's own switch", withEnvs(map[string]string{
			"MOONCAKE_OFFLOAD_ENABLED": "false",
		}), "rendered from this spec"},
		{"the bucket size this operator chose", withEnvs(map[string]string{
			"MOONCAKE_OFFLOAD_BUCKET_SIZE_LIMIT_BYTES": "268435456",
		}), "rendered from this spec"},
		{"the address every member connects to", withEnvs(map[string]string{
			"MOONCAKE_MASTER": "elsewhere:50051",
		}), "rendered from this spec"},
		// Rendered only on the EFA path, and reserved on every path: the transport is editable, so a
		// backend switched onto that fabric later would otherwise carry two definitions of it.
		{"the library path only one transport renders", withEnvs(map[string]string{
			"LD_LIBRARY_PATH": "/opt/mine/lib",
		}), "rendered from this spec"},

		// A name the strict rule refuses. Checked before the derived list, because a malformed name
		// cannot collide with one while still reaching the container.
		{"a name with a space", withEnvs(map[string]string{
			"MOONCAKE OFFLOAD": "1",
		}), "is not an environment variable name"},
		{"a name starting with a digit", withEnvs(map[string]string{
			"1MOONCAKE": "1",
		}), "is not an environment variable name"},
		{"an empty name", withEnvs(map[string]string{"": "1"}), "is not an environment variable name"},
		// A dash IS allowed, by the strict rule as well as the relaxed one. Held here so that
		// tightening the check further has to be a deliberate edit rather than a side effect.
		{"a name with a dash", withEnvs(map[string]string{
			"MOONCAKE-OFFLOAD-MINE": "1",
		}), ""},
	}, func(wh *KVCacheBackendWebhook, _, newKvcb *workercore.KVCacheBackend) error {
		_, err := wh.ValidateCreate(context.Background(), newKvcb)
		return err
	})
}

// TestKVCacheBackendWebhook_AMemberGroupCannotMove covers the gap the two per-position rules leave:
// a group's position is its identity, and swapping two groups that differ only in editable fields
// passes every field-wise check while handing each running DaemonSet the other group's spec.
//
// THE POSITIVE CASES ARE THE POINT OF THIS TABLE. The refusal is easy to get for free — freezing
// nodeSelector, or refusing any list whose length or content moved, satisfies every negative case
// here and forbids appending a group, widening a selector to gain nodes, and taking a group out of
// service in place. Those three are what the rule has to keep working, so they are cases rather than
// a comment.
func TestKVCacheBackendWebhook_AMemberGroupCannotMove(t *testing.T) {
	hot := workercore.KVCacheBackendMember{
		NodeSelector:      map[string]string{"tier": "hot"},
		Medium:            "DRAM",
		CapacityPerMember: resource.MustParse("8Gi"),
	}
	cold := workercore.KVCacheBackendMember{
		NodeSelector:      map[string]string{"tier": "cold"},
		Medium:            "DRAM",
		CapacityPerMember: resource.MustParse("32Gi"),
	}
	third := workercore.KVCacheBackendMember{
		NodeSelector:      map[string]string{"tier": "archive"},
		Medium:            "DRAM",
		CapacityPerMember: resource.MustParse("64Gi"),
	}
	members := func(groups ...workercore.KVCacheBackendMember) func(*workercore.KVCacheBackend) {
		return func(k *workercore.KVCacheBackend) {
			k.Spec.Connection.Managed.Members = groups
		}
	}

	cases := []struct {
		name    string
		old     func(*workercore.KVCacheBackend)
		new     func(*workercore.KVCacheBackend)
		wantMsg string
	}{
		// The reported case: neither group has a tier, so the two rules that compare positions have
		// nothing to compare and the swap used to pass.
		{
			"two tier-less groups swapped",
			members(hot, cold), members(cold, hot),
			"moves a group rather than editing one",
		},
		{
			// The other shape the same mistake takes. Removing the middle group shifts the third up,
			// so position 1 now carries what position 2 held.
			"a middle group removed, shifting the one after it",
			members(hot, cold, third), members(hot, third),
			"moves a group rather than editing one",
		},

		{"nothing changed", members(hot, cold), members(hot, cold), ""},
		{
			"a group appended",
			members(hot, cold), members(hot, cold, third),
			"",
		},
		{
			// An append that duplicates an existing group is still an append: positions past the old
			// list's end are never examined, so nothing here depends on the new group being novel.
			"a group appended that copies an existing one",
			members(hot, cold), members(hot, cold, hot),
			"",
		},
		{
			"the last group removed",
			members(hot, cold, third), members(hot, cold),
			"",
		},
		{
			"a nodeSelector widened in place, which is how a group gains nodes",
			members(hot, cold),
			members(workercore.KVCacheBackendMember{
				NodeSelector:      map[string]string{},
				Medium:            "DRAM",
				CapacityPerMember: resource.MustParse("8Gi"),
			}, cold),
			"",
		},
		{
			// The documented way to take a middle group out of service: narrow it until it selects
			// nothing. Its position, its DaemonSet and every later group stay exactly as they are.
			"a middle group narrowed until it matches no node",
			members(hot, cold, third),
			members(hot, workercore.KVCacheBackendMember{
				NodeSelector:      map[string]string{"tier": "cold", "drained": "true"},
				Medium:            "DRAM",
				CapacityPerMember: resource.MustParse("32Gi"),
			}, third),
			"",
		},
		{
			// The group that position 1 now resembles never left position 1, so nothing moved. A
			// rule keyed on "resembles another position" rather than on "another position was
			// vacated" would refuse this edit on the strength of a group that did not change.
			"a group edited to match a second that stays where it is",
			members(hot, cold), members(cold, cold),
			"",
		},
		{
			// The same shape with the two ends reversed, so the case does not pass by being at a
			// particular index.
			"the later group edited to match the earlier one",
			members(hot, cold), members(hot, hot),
			"",
		},
		{
			// A quantity respelled is the same group, so the comparison has to be semantic. A
			// structural one would read this as an edit and let a swap written the same way through.
			"a capacity respelled to the same quantity",
			members(hot, cold),
			members(workercore.KVCacheBackendMember{
				NodeSelector:      map[string]string{"tier": "hot"},
				Medium:            "DRAM",
				CapacityPerMember: resource.MustParse("8192Mi"),
			}, cold),
			"",
		},
	}

	wh := &KVCacheBackendWebhook{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
			c.old(oldKvcb)
			c.new(newKvcb)

			_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
			if c.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), c.wantMsg)
		})
	}
}

// TestKVCacheBackendWebhook_ADiskTierLeavesOnlyWithTheLastGroup pins the one way a declared tier can
// be taken off, because the documentation now tells operators which position they need to be in.
//
// The immutability rules pair groups BY POSITION and stop at the end of the new list, so dropping the
// LAST group takes its tier with it, while dropping an earlier one compares every position after it
// against a different group's spec and is refused. A backend whose only group carries a tier has no
// exit at all: `members` requires an entry, so the group cannot go, and replacing it in place is the
// forbidden edit.
//
// Without these cases the exits are an accident of a loop bound rather than a contract, and the page
// that describes them would drift the first time someone iterates the old list instead of the new.
func TestKVCacheBackendWebhook_ADiskTierLeavesOnlyWithTheLastGroup(t *testing.T) {
	plain := workercore.KVCacheBackendMember{
		NodeSelector:      map[string]string{"kvcache-plain": "true"},
		Medium:            "DRAM",
		CapacityPerMember: resource.MustParse("10Ti"),
	}
	// A two-group backend whose tier is on the SECOND group, which is the position the exit needs.
	tierOnTheLastGroup := func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Members = []workercore.KVCacheBackendMember{
			plain, k.Spec.Connection.Managed.Members[0],
		}
		k.Spec.Connection.Managed.Members[1].LocalDisk = &workercore.KVCacheBackendMemberLocalDisk{
			Path: "/var/lib/kvcache", Capacity: resource.MustParse("4Ti"),
		}
		k.Spec.Connection.Managed.Leader.Offload = &workercore.KVCacheBackendLeaderOffload{Enabled: true}
	}

	cases := []struct {
		name    string
		old     func(*workercore.KVCacheBackend)
		new     func(*workercore.KVCacheBackend)
		wantMsg string
	}{
		{
			"the last group carries the tier and is dropped with the leader's offload",
			tierOnTheLastGroup,
			func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members = []workercore.KVCacheBackendMember{plain}
				k.Spec.Connection.Managed.Leader.Offload = nil
			},
			"",
		},
		{
			// The trap on the way to the case above, and the reason the exit is ONE edit rather
			// than two: the pair rule holds across the whole object, so an update that drops the
			// only tier while leaving the leader offloading is refused for the other half.
			"the last group is dropped but the leader keeps offloading",
			tierOnTheLastGroup,
			func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members = []workercore.KVCacheBackendMember{plain}
			},
			"the leader would queue offload work for members that have nowhere to put it",
		},
		{
			"the first group carries the tier and is dropped",
			func(k *workercore.KVCacheBackend) {
				withDiskTier()(k)
				k.Spec.Connection.Managed.Members = append(k.Spec.Connection.Managed.Members, plain)
			},
			func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members = []workercore.KVCacheBackendMember{plain}
			},
			"cannot be removed from the group at this position",
		},
		{
			"the only group carries the tier and is replaced in place",
			withDiskTier(),
			func(k *workercore.KVCacheBackend) {
				k.Spec.Connection.Managed.Members = []workercore.KVCacheBackendMember{plain}
			},
			"cannot be removed from the group at this position",
		},
	}

	wh := &KVCacheBackendWebhook{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oldKvcb, newKvcb := newKVCacheBackend(), newKVCacheBackend()
			c.old(oldKvcb)
			c.old(newKvcb)
			c.new(newKvcb)

			_, err := wh.ValidateUpdate(context.Background(), oldKvcb, newKvcb)
			if c.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), c.wantMsg)
		})
	}
}

// TestKVCacheBackendWebhook_ReportsEveryViolationAtOnce pins that a spec breaking several unrelated
// rules is refused with all of them named, rather than one at a time.
//
// An operator fixing a manifest by re-applying it reads one message per round trip, so a validator
// that returns on its first finding turns a three-rule mistake into three apply-and-read cycles. The
// three chosen here are independent and land in different validators — a path rule, a quantity rule
// and a scale-in bound — so the assertion is about the aggregation and not about any one of them.
//
// LIMITED: it does not cover the two scale-in bounds accumulating, and passes if either of them
// returns early again. No grace is both above the ceiling and negative, so only one of those bounds
// can fire whatever they do with the error. What this covers is any of these validators returning on
// its first error rather than collecting.
func TestKVCacheBackendWebhook_ReportsEveryViolationAtOnce(t *testing.T) {
	kvcb := newKVCacheBackend()
	withDiskTier()(kvcb)
	kvcb.Spec.Connection.Managed.Members[0].LocalDisk.Path = "var/lib/relative"
	kvcb.Spec.Connection.Managed.Members[0].CapacityPerMember = resource.MustParse("0")
	kvcb.Spec.Connection.Managed.ScaleIn = &workercore.KVCacheBackendScaleIn{GracePeriodSeconds: -1}

	wh := &KVCacheBackendWebhook{}
	_, err := wh.ValidateCreate(context.Background(), kvcb)
	require.Error(t, err)

	for _, want := range []string{
		"must be an absolute path",
		"must be greater than 0",
		"must not be negative",
	} {
		require.Contains(t, err.Error(), want,
			"every violated rule has to be named in one refusal, or fixing a manifest costs one "+
				"apply per mistake")
	}
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
