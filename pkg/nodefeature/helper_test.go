package nodefeature

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/systemname"
)

// mergeLabels merges the given label maps left to right.
func mergeLabels(ms ...map[string]string) map[string]string {
	ret := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			ret[k] = v
		}
	}
	return ret
}

// cpuModelLabels mirrors the NFD cpu source labels that drive the
// general(CPU) node key, here an AMD family 25 model 1 (-> "amd-25-1").
func cpuModelLabels() map[string]string {
	return map[string]string{
		_NFDCPUModelVendorIDLabelKey: "AMD",
		_NFDCPUModelFamilyLabelKey:   "25",
		_NFDCPUModelIDLabelKey:       "1",
	}
}

func TestConstructAcceleratableNodeLabels(t *testing.T) {
	cases := []struct {
		name     string
		groups   device.DevicesGroupList
		expected map[string]string
	}{
		{
			name:     "no groups",
			groups:   nil,
			expected: map[string]string{},
		},
		{
			name: "group without accelerators is skipped",
			groups: device.DevicesGroupList{
				{
					ID:           "tesla-t4",
					Manufacturer: "nvidia",
					Name:         "Tesla T4",
					Memory:       15360,
				},
			},
			expected: map[string]string{},
		},
		{
			name: "full group",
			groups: device.DevicesGroupList{
				{
					ID:                "tesla-t4",
					Manufacturer:      "nvidia",
					Name:              "Tesla T4", // exercises space -> dash sanitization
					Memory:            15360,
					Cores:             2560,
					DriverVersion:     "580.126.09",
					RuntimeVersion:    "13.0",
					ComputeCapability: "7.5",
					Family:            "Turing",
					Accelerators: []device.Accelerator{
						{ID: "GPU-0", Index: 0},
						{ID: "GPU-1", Index: 1},
						{ID: "GPU-2", Index: 2},
						{ID: "GPU-3", Index: 3},
					},
				},
			},
			expected: map[string]string{
				FeatureLabelPrefix + "acceleratable":                        "true",
				AcceleratableFeatureLabelPrefix + "nvidia":                  "true",
				AcceleratableFeatureLabelPrefix + "nvidia.driver-version":   "580.126.09",
				AcceleratableFeatureLabelPrefix + "nvidia.runtime-version":  "13.0",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4":         "true",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.product": "Tesla-T4",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.memory":  "15Gi",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.cores":   "2560",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.family":  "Turing",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.comcap":  "7.5",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.count":   "4",
			},
		},
		{
			name: "minimal group without optional fields",
			groups: device.DevicesGroupList{
				{
					ID:           "h100",
					Manufacturer: "nvidia",
					Name:         "H100",
					Memory:       81920,
					Accelerators: []device.Accelerator{
						{ID: "GPU-0", Index: 0},
					},
				},
			},
			expected: map[string]string{
				FeatureLabelPrefix + "acceleratable":                    "true",
				AcceleratableFeatureLabelPrefix + "nvidia":              "true",
				AcceleratableFeatureLabelPrefix + "nvidia-h100":         "true",
				AcceleratableFeatureLabelPrefix + "nvidia-h100.product": "H100",
				AcceleratableFeatureLabelPrefix + "nvidia-h100.memory":  "80Gi",
				AcceleratableFeatureLabelPrefix + "nvidia-h100.cores":   "0",
				AcceleratableFeatureLabelPrefix + "nvidia-h100.count":   "1",
			},
		},
		{
			name: "multiple groups, one skipped",
			groups: device.DevicesGroupList{
				{
					ID:           "tesla-t4",
					Manufacturer: "nvidia",
					Name:         "Tesla-T4",
					Memory:       15360,
					Accelerators: []device.Accelerator{
						{ID: "GPU-0", Index: 0},
					},
				},
				{
					// Skipped — no accelerators.
					ID:           "skipped",
					Manufacturer: "nvidia",
					Name:         "Skipped",
				},
				{
					ID:           "mi300x",
					Manufacturer: "amd",
					Name:         "MI300X",
					Memory:       196608,
					Accelerators: []device.Accelerator{
						{ID: "GPU-0", Index: 0},
						{ID: "GPU-1", Index: 1},
					},
				},
			},
			expected: map[string]string{
				FeatureLabelPrefix + "acceleratable":                        "true",
				AcceleratableFeatureLabelPrefix + "nvidia":                  "true",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4":         "true",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.product": "Tesla-T4",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.memory":  "15Gi",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.cores":   "0",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.count":   "1",
				AcceleratableFeatureLabelPrefix + "amd":                     "true",
				AcceleratableFeatureLabelPrefix + "amd-mi300x":              "true",
				AcceleratableFeatureLabelPrefix + "amd-mi300x.product":      "MI300X",
				AcceleratableFeatureLabelPrefix + "amd-mi300x.memory":       "192Gi",
				AcceleratableFeatureLabelPrefix + "amd-mi300x.cores":        "0",
				AcceleratableFeatureLabelPrefix + "amd-mi300x.count":        "2",
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ConstructAcceleratableNodeLabels(cs.groups)
			assert.Equal(t, cs.expected, actual, "unexpected node labels")
		})
	}
}

func TestExtractAcceleratableNodeKeys(t *testing.T) {
	cases := []struct {
		name     string
		node     *core.Node
		expected []string
	}{
		{
			name: "empty labels",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			expected: nil,
		},
		{
			name: "non-empty labels without feature label",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{
						"foo": "bar",
					},
				},
			},
			expected: nil,
		},
		{
			name: "non-empty labels with feature label",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{
						"foo": "bar",
						AcceleratableFeatureLabelPrefix + "nvidia":                  "true",
						AcceleratableFeatureLabelPrefix + "nvidia.driver-version":   "580.126.09",
						AcceleratableFeatureLabelPrefix + "nvidia.runtime-version":  "13.0",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4":         "true",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.count":   "4",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.comcap":  "7.5",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.cores":   "2560",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.family":  "Turing",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.memory":  "15Gi",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.product": "Tesla-T4",
					},
				},
			},
			expected: []string{
				"nvidia-tesla-t4",
			},
		},
		{
			name: "non-empty labels with unknown manufacturer",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{
						"foo": "bar",
						AcceleratableFeatureLabelPrefix + "unknown":                  "true",
						AcceleratableFeatureLabelPrefix + "unknown.driver-version":   "580.126.09",
						AcceleratableFeatureLabelPrefix + "unknown.runtime-version":  "13.0",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4":         "true",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.count":   "4",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.comcap":  "7.5",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.cores":   "2560",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.family":  "Turing",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.memory":  "15Gi",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.product": "Tesla-T4",
					},
				},
			},
			expected: nil,
		},
		{
			name: "old-prefix labels are ignored",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{
						FeatureLabelPrefix + "nvidia":          "true",
						FeatureLabelPrefix + "nvidia-tesla-t4": "true",
					},
				},
			},
			expected: nil,
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ExtractAcceleratableNodeKeys(cs.node)
			assert.Equal(t, cs.expected, actual, "unexpected node keys")
		})
	}
}

func TestExtractGeneralNodeKey(t *testing.T) {
	newNode := func(labels, annotations map[string]string) *core.Node {
		return &core.Node{
			ObjectMeta: meta.ObjectMeta{
				Labels:      labels,
				Annotations: annotations,
			},
		}
	}

	osArchLabels := map[string]string{
		core.LabelOSStable:   "linux",
		core.LabelArchStable: "amd64",
	}

	cases := []struct {
		name     string
		node     *core.Node
		expected string
	}{
		{
			// No vendor/name/family — the manufacturer falls back to "generic" and
			// no id prefix is blended in (graceful degradation when NFD is absent).
			name:     "no cpu information keys off generic",
			node:     newNode(osArchLabels, nil),
			expected: "generic",
		},
		{
			// No cpu-name annotation, so the cpu-model family/id labels supply
			// the id prefix — the fallback path.
			name:     "cpu-model family/id is the fallback id prefix",
			node:     newNode(mergeLabels(cpuModelLabels(), osArchLabels), nil),
			expected: "amd-25-1",
		},
		{
			name: "cpu-name annotation leads the id",
			node: newNode(
				mergeLabels(cpuModelLabels(), osArchLabels),
				map[string]string{
					FeatureLabelPrefix + "cpu-name": "AMD EPYC 7763 64-Core Processor",
				},
			),
			expected: "amd-epyc-7763",
		},
		{
			name: "unresolved cpu-name annotation falls back to cpu-model labels",
			node: newNode(
				mergeLabels(cpuModelLabels(), osArchLabels),
				map[string]string{
					FeatureLabelPrefix + "cpu-name": "@cpu.model.name",
				},
			),
			expected: "amd-25-1",
		},
		{
			name: "trademark markers and frequency are stripped from the cpu-name",
			node: newNode(
				mergeLabels(osArchLabels, map[string]string{
					_NFDCPUModelVendorIDLabelKey: "Intel",
				}),
				map[string]string{
					FeatureLabelPrefix + "cpu-name": "Intel(R) Xeon(R) Platinum 8358 CPU @ 2.60GHz",
				},
			),
			expected: "intel-xeon-platinum-8358",
		},
		{
			name: "unknown vendor keys off generic",
			node: newNode(
				mergeLabels(osArchLabels, map[string]string{
					_NFDCPUModelVendorIDLabelKey: "VendorUnknown",
				}),
				map[string]string{
					FeatureLabelPrefix + "cpu-name": "Kunpeng 920",
				},
			),
			expected: "generic-kunpeng-920",
		},
		{
			// Vendor is known but the family/id pair is incomplete and there is
			// no cpu-name — no id prefix is blended, so the key is just the
			// "${manufacturer}".
			name: "vendor without family keys off manufacturer",
			node: newNode(mergeLabels(osArchLabels, map[string]string{
				_NFDCPUModelVendorIDLabelKey: "AMD",
				_NFDCPUModelIDLabelKey:       "1",
			}), nil),
			expected: "amd",
		},
		{
			name: "vendor without id keys off manufacturer",
			node: newNode(mergeLabels(osArchLabels, map[string]string{
				_NFDCPUModelVendorIDLabelKey: "AMD",
				_NFDCPUModelFamilyLabelKey:   "25",
			}), nil),
			expected: "amd",
		},
		{
			// The cpu-name sanitizes to empty, so no id prefix is blended; the
			// known vendor still leads the key.
			name: "cpu-name sanitized to empty keys off manufacturer",
			node: newNode(
				mergeLabels(osArchLabels, map[string]string{
					_NFDCPUModelVendorIDLabelKey: "AMD",
				}),
				map[string]string{
					FeatureLabelPrefix + "cpu-name": "(TM)",
				},
			),
			expected: "amd",
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ExtractGeneralNodeKey(cs.node)
			assert.Equal(t, cs.expected, actual, "unexpected general node key")
		})
	}
}

// TestExtractGeneralNodeKey_BudgetLeavesRoomForSuffix pins that the general key is truncated with
// room for its longest sibling suffix: the key is used bare AND with a ".count"/".capacity" suffix
// as a Kubernetes label NAME segment (the part after "general.feature.gpustack.ai/"), which must
// stay within 63 characters. A pathologically long CPU name must therefore not fill the whole 63.
func TestExtractGeneralNodeKey_BudgetLeavesRoomForSuffix(t *testing.T) {
	node := &core.Node{ObjectMeta: meta.ObjectMeta{
		Labels: map[string]string{_NFDCPUModelVendorIDLabelKey: "AMD"},
		Annotations: map[string]string{
			// A CPU name far longer than the label budget, so truncation is exercised.
			FeatureLabelPrefix + "cpu-name": strings.Repeat("epyc-9654-96-core-processor-", 4),
		},
	}}

	gKey := ExtractGeneralNodeKey(node)
	require.NotEmpty(t, gKey)
	assert.True(t, strings.HasPrefix(gKey, "amd-"), "the manufacturer still leads the key: %q", gKey)
	// The binding constraint is the longest suffixed form, not the bare key.
	assert.LessOrEqual(t, len(gKey+".capacity"), 63,
		"the key plus its longest suffix must fit a 63-char label name segment: %q", gKey)
}

func TestConstructNodeCapacityLabels(t *testing.T) {
	// ConstructNodeCapacityLabels has been gutted: it no longer derives any
	// per-unit capacity (.cpu/.ram/.storage) or Kueue-profile (.z-flavor/
	// .z-queue/.z-cohort) labels onto the node. It now emits only the managed
	// mark plus general(CPU) key presence — the manufacturer label, the full key
	// label, and the full key's .count sibling (the rounded-up CPU capacity) — so
	// the NodeFlavorReconciler can pool CPU-only nodes and pin a flavor to
	// same-sized nodes. The flavor derivation moved to ExtractNodeFlavors; the unit
	// spec is a fixed default on the InstanceType, no longer derived here.
	//
	// The cpu-model labels of an AMD family 25 model 1 CPU derive the general key
	// "amd-25-1" (no os/arch tail). The generic fallback of the key derivation itself
	// is covered by TestExtractGeneralNodeKey.

	// newNode builds a node with the given labels and a representative
	// status.capacity. Capacity is intentionally populated to prove it does NOT
	// leak into the emitted labels.
	newNode := func(labels map[string]string) *core.Node {
		if labels == nil {
			labels = map[string]string{}
		}
		labels[core.LabelOSStable] = "linux"
		labels[core.LabelArchStable] = "amd64"
		capacity := core.ResourceList{
			core.ResourceCPU:              resource.MustParse("16"),
			core.ResourceMemory:           resource.MustParse("31Gi"),
			core.ResourceEphemeralStorage: resource.MustParse("97Gi"),
		}
		return &core.Node{
			ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: labels},
			Status:     core.NodeStatus{Capacity: capacity, Allocatable: capacity},
		}
	}

	// deviceLabels mirrors what ConstructAcceleratableNodeLabels would have put
	// on the node before ConstructNodeCapacityLabels is invoked. They must not
	// add any general capacity labels to the output.
	deviceLabels := func(id, product, memory, accelerators string) map[string]string {
		aKey := AcceleratableFeatureLabelPrefix + "nvidia-" + id
		return map[string]string{
			NodeAcceleratableLabelKey:                  "true",
			AcceleratableFeatureLabelPrefix + "nvidia": "true",
			aKey:              "true",
			aKey + ".product": product,
			aKey + ".memory":  memory,
			aKey + ".cores":   "0",
			aKey + ".count":   accelerators,
		}
	}

	// generalPresence is the entire general(CPU) view ConstructNodeCapacityLabels
	// now emits: the managed mark plus the manufacturer/key presence labels.
	generalPresence := func(manu, key string) map[string]string {
		return map[string]string{
			systemname.ManagedLabelKey:                 "true",
			GeneralFeatureLabelPrefix + manu:           "true",
			GeneralFeatureLabelPrefix + key:            "true",
			GeneralFeatureLabelPrefix + key + ".count": "16",
		}
	}

	cases := []struct {
		name     string
		node     *core.Node
		opts     []ConstructNodeCapacityLabelsOption
		expected map[string]string
	}{
		{
			// AMD cpu-model labels → general key "amd-25-1".
			name:     "amd cpu-model node emits general presence only",
			node:     newNode(cpuModelLabels()),
			expected: generalPresence("amd", "amd-25-1"),
		},
		{
			// No NFD cpu information at all — the general key falls back to
			// "generic".
			name:     "cpu-only node without cpu information falls back to generic",
			node:     newNode(nil),
			expected: generalPresence("generic", "generic"),
		},
		{
			// Intel cpu-model labels → general key "intel-6-143".
			name: "intel cpu-model node keys off intel-6-143",
			node: newNode(map[string]string{
				_NFDCPUModelVendorIDLabelKey: "Intel",
				_NFDCPUModelFamilyLabelKey:   "6",
				_NFDCPUModelIDLabelKey:       "143",
			}),
			expected: generalPresence("intel", "intel-6-143"),
		},
		{
			// The cpu-name annotation leads the general key.
			name: "cpu-name annotation leads the general key",
			node: func() *core.Node {
				nd := newNode(cpuModelLabels())
				nd.Annotations = map[string]string{
					FeatureLabelPrefix + "cpu-name": "AMD EPYC 7763 64-Core Processor",
				}
				return nd
			}(),
			expected: generalPresence("amd", "amd-epyc-7763"),
		},
		{
			// Accelerator labels are present, but the device contributes no
			// general capacity labels — only the general presence is emitted.
			name: "accelerated node still emits general presence only",
			node: newNode(
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "2")),
			),
			expected: generalPresence("amd", "amd-25-1"),
		},
		{
			// Existing managed=true on the node is preserved verbatim.
			name: "managed label is true on node",
			node: newNode(mergeLabels(cpuModelLabels(), map[string]string{
				systemname.ManagedLabelKey: "true",
			})),
			expected: generalPresence("amd", "amd-25-1"),
		},
		{
			// Existing managed=false on the node overrides the default "true".
			name: "managed label is false on node",
			node: newNode(mergeLabels(cpuModelLabels(), map[string]string{
				systemname.ManagedLabelKey: "false",
			})),
			expected: mergeLabels(
				generalPresence("amd", "amd-25-1"),
				map[string]string{systemname.ManagedLabelKey: "false"},
			),
		},
	}

	// forbiddenSuffixes are the per-unit/profile label suffixes the gutted
	// function must no longer emit under any key.
	forbiddenSuffixes := []string{".cpu", ".ram", ".storage", ".z-flavor", ".z-queue", ".z-cohort"}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ConstructNodeCapacityLabels(cs.node, cs.opts...)
			assert.Equal(t, cs.expected, actual, "unexpected capacity labels")
			// None of the removed per-unit/profile labels may appear.
			for k := range actual {
				for _, sfx := range forbiddenSuffixes {
					assert.NotContains(t, k, sfx, "removed capacity label must not be emitted")
				}
			}
		})
	}
}

// TestConstructNodeCapacityLabels_ManualNodeManagement pins the node-management-manual
// gate: without it the managed label is auto-injected; with it the label
// is only present when the admin set it explicitly on the node.
func TestConstructNodeCapacityLabels_ManualNodeManagement(t *testing.T) {
	base := func(labels map[string]string) *core.Node {
		return &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", Labels: labels}}
	}
	managed := func(node *core.Node, opts ...ConstructNodeCapacityLabelsOption) (string, bool) {
		v, ok := ConstructNodeCapacityLabels(node, opts...)[systemname.ManagedLabelKey]
		return v, ok
	}

	// Auto (default): managed=true is injected.
	v, ok := managed(base(nil))
	assert.True(t, ok, "auto mode injects the managed label")
	assert.Equal(t, "true", v)

	// Manual: no auto-inject on a node without an explicit label.
	_, ok = managed(base(nil), WithManualNodeManagement(true))
	assert.False(t, ok, "manual mode must not auto-inject the managed label")

	// Manual: an explicit admin opt-in/opt-out is still honored.
	v, ok = managed(base(map[string]string{systemname.ManagedLabelKey: "true"}), WithManualNodeManagement(true))
	assert.True(t, ok)
	assert.Equal(t, "true", v, "explicit opt-in preserved under manual mode")

	v, ok = managed(base(map[string]string{systemname.ManagedLabelKey: "false"}), WithManualNodeManagement(true))
	assert.True(t, ok)
	assert.Equal(t, "false", v, "explicit opt-out preserved under manual mode")
}

// TestIsAcceleratableCreditsResourceName pins the predicate that tells an accelerator's quota
// resource from any other. A podset's flavor assignment is keyed by covered resource, so this is
// what picks the entry that scopes an accelerator demand — reading a CPU entry instead would scope
// the demand to a flavor covering no card.
//
// The round trip with GetAcceleratableCreditsResourceName is the first case: the two must agree, or
// a name this operator writes would not be recognized when it is read back.
func TestIsAcceleratableCreditsResourceName(t *testing.T) {
	cases := []struct {
		name string
		in   core.ResourceName
		want bool
	}{
		{"the name this package writes", GetAcceleratableCreditsResourceName(ManufacturerNVIDIA), true},
		{"another known manufacturer", "credits.gpustack.ai/amd", true},
		{"an unknown manufacturer is not ours", "credits.gpustack.ai/acme", false},
		{"the prefix alone names no manufacturer", "credits.gpustack.ai/", false},
		{"a device resource is not a credits resource", "nvidia.com/gpu", false},
		{"cpu is not a credits resource", core.ResourceCPU, false},
		// The shorthand a fixture reaches for, which is why it is worth pinning: it is NOT the
		// resource Kueue accounts, so a test using it would exercise a different path than
		// production does.
		{"the bare word is not the resource name", "credits", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, IsAcceleratableCreditsResourceName(c.in))
		})
	}
}

func TestFormatLocalQueueName(t *testing.T) {
	// The longest realistic ClusterQueue name still maps to a fixed
	// 31-character LocalQueue name, far below the 63-character label value
	// limit of "kueue.x-k8s.io/queue-name".
	long := FormatLocalQueueName("gpustack--intel-6-143-12c-48g--nvidia-rtx-6000-ada-generation-1d-8s")
	assert.Len(t, long, 31, "unexpected local queue name length")
	assert.Regexp(t, "^gpustack-fnv64-[0-9a-f]{16}$", long, "unexpected local queue name shape")

	// Deterministic: same input, same output.
	assert.Equal(t, long, FormatLocalQueueName("gpustack--intel-6-143-12c-48g--nvidia-rtx-6000-ada-generation-1d-8s"))

	// Distinct inputs map to distinct names.
	other := FormatLocalQueueName("gpustack--amd-25-1-1c-2g")
	assert.NotEqual(t, long, other, "different cluster queues must not collide")
}

func TestExtractNodeFlavors(t *testing.T) {
	// newNode builds a node with the given labels/annotations and a CPU
	// capacity (cpuCores; 0 means no cpu capacity at all). os/arch are added
	// for every non-nil label map since they feed the flavor Name and
	// NodeLabels; the nil-labels case is left untouched to exercise the guard.
	newNode := func(labels, annotations map[string]string, cpuCores int64) *core.Node {
		if labels != nil {
			labels[core.LabelOSStable] = "linux"
			labels[core.LabelArchStable] = "amd64"
		}
		capacity := core.ResourceList{}
		if cpuCores > 0 {
			capacity[core.ResourceCPU] = *resource.NewQuantity(cpuCores, resource.DecimalSI)
		}
		return &core.Node{
			ObjectMeta: meta.ObjectMeta{
				Name:        "node-1",
				Labels:      labels,
				Annotations: annotations,
			},
			Status: core.NodeStatus{Capacity: capacity},
		}
	}

	// deviceLabels mirrors what ConstructAcceleratableNodeLabels would have put
	// on the node for one accelerator model.
	deviceLabels := func(id, product, family, memory, count string) map[string]string {
		aKey := AcceleratableFeatureLabelPrefix + "nvidia-" + id
		return map[string]string{
			NodeAcceleratableLabelKey:                  "true",
			AcceleratableFeatureLabelPrefix + "nvidia": "true",
			aKey:              "true",
			aKey + ".product": product,
			aKey + ".family":  family,
			aKey + ".memory":  memory,
			aKey + ".cores":   "0",
			aKey + ".count":   count,
		}
	}

	cases := []struct {
		name string
		node *core.Node
		// wantCPU, when non-nil, is the expected leading CPU/general flavor.
		// nil means no CPU flavor is expected.
		wantCPU     *NodeFlavor
		wantDevices []NodeFlavor
		wantEmpty   bool
	}{
		{
			// nil labels — nothing to derive.
			name:      "nil labels return no flavors",
			node:      newNode(nil, nil, 8),
			wantEmpty: true,
		},
		{
			// (a) CPU-only node: one CPU flavor keyed by the node's real cpu-model
			// ("amd-25-1"), Acceleratable=false. Product/Family are empty here because
			// no cpu-name/cpu-family annotation is reported.
			name: "cpu-only node yields a single CPU flavor keyed by its cpu-model",
			node: newNode(cpuModelLabels(), nil, 8),
			wantCPU: &NodeFlavor{
				Name:         "gpustack--amd-25-1-linux-amd64-8c",
				GeneralKey:   "amd-25-1",
				OS:           "linux",
				Arch:         "amd64",
				Count:        8,
				Manufacturer: "amd",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                        "true",
					core.LabelOSStable:                                "linux",
					core.LabelArchStable:                              "amd64",
					GeneralFeatureLabelPrefix + "amd-25-1":            "true",
					GeneralFeatureLabelPrefix + "amd-25-1" + ".count": "8",
				},
			},
		},
		{
			// (b) Accelerated node: a CPU flavor (Product/Family populated from the
			// cpu-name/cpu-family annotations) plus one device flavor sized by the
			// .count label. The device flavor's Name encodes both the CPU key and the
			// accelerator key, and its NodeLabels pin the paired CPU-key presence.
			name: "accelerated node yields CPU plus device flavors",
			node: newNode(
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "Turing", "15Gi", "2")),
				map[string]string{
					FeatureLabelPrefix + "cpu-name":   "AMD EPYC 7763 64-Core Processor",
					FeatureLabelPrefix + "cpu-family": "25",
				},
				8,
			),
			wantCPU: &NodeFlavor{
				Name:         "gpustack--amd-epyc-7763-linux-amd64-8c",
				GeneralKey:   "amd-epyc-7763",
				OS:           "linux",
				Arch:         "amd64",
				Count:        8,
				Manufacturer: "amd",
				Product:      "AMD EPYC 7763 64-Core Processor",
				Family:       "25",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                             "true",
					core.LabelOSStable:                                     "linux",
					core.LabelArchStable:                                   "amd64",
					GeneralFeatureLabelPrefix + "amd-epyc-7763":            "true",
					GeneralFeatureLabelPrefix + "amd-epyc-7763" + ".count": "8",
				},
			},
			wantDevices: []NodeFlavor{
				{
					Name:           "gpustack--amd-epyc-7763--nvidia-tesla-t4-linux-amd64-2d",
					AcceleratorKey: "nvidia-tesla-t4",
					GeneralKey:     "amd-epyc-7763",
					OS:             "linux",
					Arch:           "amd64",
					Count:          2,
					Acceleratable:  true,
					Manufacturer:   "nvidia",
					Product:        "Tesla-T4",
					Family:         "Turing",
					Memory:         "15Gi",
					Cores:          "0",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey:                                     "true",
						core.LabelOSStable:                                             "linux",
						core.LabelArchStable:                                           "amd64",
						GeneralFeatureLabelPrefix + "amd-epyc-7763":                    "true",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4":            "true",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4" + ".count": "2",
					},
				},
			},
		},
		{
			// (c) Node with no CPU capacity: the CPU flavor is skipped. With no
			// device labels either, the result is empty.
			name:      "node without cpu capacity yields no flavors",
			node:      newNode(cpuModelLabels(), nil, 0),
			wantEmpty: true,
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			// Stamp the general .count label ConstructNodeCapacityLabels writes in
			// production, so ExtractNodeFlavors can read the CPU flavor size from it.
			if cs.wantCPU != nil {
				cs.node.Labels[GeneralFeatureLabelPrefix+cs.wantCPU.GeneralKey+".count"] = strconv.FormatInt(cs.wantCPU.Count, 10)
			}
			got := ExtractNodeFlavors(cs.node)
			if cs.wantEmpty {
				assert.Empty(t, got, "expected no flavors")
				return
			}
			if cs.wantCPU != nil {
				if len(got) == 0 {
					t.Fatalf("expected a CPU flavor but got none")
				}
				assert.Equal(t, *cs.wantCPU, got[0], "unexpected CPU flavor")
				assert.ElementsMatch(t, cs.wantDevices, got[1:], "unexpected device flavors")
				return
			}
			assert.ElementsMatch(t, cs.wantDevices, got, "unexpected device flavors")
		})
	}
}
