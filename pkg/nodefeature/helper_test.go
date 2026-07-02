package nodefeature

import (
	"testing"

	"github.com/stretchr/testify/assert"
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

func Test_extractGeneralNodeKey(t *testing.T) {
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
		name        string
		withCPUName bool
		node        *core.Node
		expected    string
	}{
		{
			// With blending off the CPU identity is ignored entirely; the key is
			// just the "generic" manufacturer (no os/arch tail).
			name:     "disabled blending keys off generic",
			node:     newNode(mergeLabels(cpuModelLabels(), osArchLabels), nil),
			expected: "generic",
		},
		{
			// Blending on, but no vendor/name/family — the manufacturer falls
			// back to "generic" and no id prefix is blended in.
			name:        "no cpu information keys off generic",
			withCPUName: true,
			node:        newNode(osArchLabels, nil),
			expected:    "generic",
		},
		{
			// No cpu-name annotation, so the cpu-model family/id labels supply
			// the id prefix — the rare fallback path.
			name:        "cpu-model family/id is the fallback id prefix",
			withCPUName: true,
			node:        newNode(mergeLabels(cpuModelLabels(), osArchLabels), nil),
			expected:    "amd-25-1",
		},
		{
			name:        "cpu-name annotation leads the id",
			withCPUName: true,
			node: newNode(
				mergeLabels(cpuModelLabels(), osArchLabels),
				map[string]string{
					FeatureLabelPrefix + "cpu-name": "AMD EPYC 7763 64-Core Processor",
				},
			),
			expected: "amd-epyc-7763",
		},
		{
			name:        "unresolved cpu-name annotation falls back to cpu-model labels",
			withCPUName: true,
			node: newNode(
				mergeLabels(cpuModelLabels(), osArchLabels),
				map[string]string{
					FeatureLabelPrefix + "cpu-name": "@cpu.model.name",
				},
			),
			expected: "amd-25-1",
		},
		{
			name:        "trademark markers and frequency are stripped from the cpu-name",
			withCPUName: true,
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
			name:        "unknown vendor keys off generic",
			withCPUName: true,
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
			name:        "vendor without family keys off manufacturer",
			withCPUName: true,
			node: newNode(mergeLabels(osArchLabels, map[string]string{
				_NFDCPUModelVendorIDLabelKey: "AMD",
				_NFDCPUModelIDLabelKey:       "1",
			}), nil),
			expected: "amd",
		},
		{
			name:        "vendor without id keys off manufacturer",
			withCPUName: true,
			node: newNode(mergeLabels(osArchLabels, map[string]string{
				_NFDCPUModelVendorIDLabelKey: "AMD",
				_NFDCPUModelFamilyLabelKey:   "25",
			}), nil),
			expected: "amd",
		},
		{
			// The cpu-name sanitizes to empty, so no id prefix is blended; the
			// known vendor still leads the key.
			name:        "cpu-name sanitized to empty keys off manufacturer",
			withCPUName: true,
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
			actual := extractGeneralNodeKey(cs.node, cs.withCPUName)
			assert.Equal(t, cs.expected, actual, "unexpected general node key")
		})
	}
}

func TestConstructNodeCapacityLabels(t *testing.T) {
	// ConstructNodeCapacityLabels has been gutted: it no longer derives any
	// per-unit capacity (.cpu/.ram/.storage) or Kueue-profile (.z-flavor/
	// .z-queue/.z-cohort) labels onto the node. It now emits only the managed
	// mark plus general(CPU) key presence — the manufacturer label and the full
	// key label — so the NodeFlavorReconciler can pool CPU-only nodes. The
	// unit spec moved onto the ResourceFlavor (see DeriveNodeUnitSpec) and the
	// flavor derivation moved to ExtractNodeFlavors.
	//
	// With CPU-name blending enabled, the cpu-model labels of an AMD family 25
	// model 1 CPU derive the general key "amd-25-1" (no os/arch tail anymore).
	// The generic fallback of the key derivation itself is covered by
	// Test_extractGeneralNodeKey.
	generalNodeKeyWithCPUName = true

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
			systemname.ManagedLabelKey:       "true",
			GeneralFeatureLabelPrefix + manu: "true",
			GeneralFeatureLabelPrefix + key:  "true",
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
// gate (switch ①): without it the managed label is auto-injected; with it the label
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

func TestParseNodeProfile(t *testing.T) {
	// ParseNodeProfile consumes the full profile string emitted by
	// FormatNodeProfile:
	//
	//   "gpustack--${generalKey}-${cpu}c-${ram}g[-${stg}g][--${accKey}-${acc}d[-${sliced}s]]"
	//
	// The "gpustack--" prefix and a non-empty general key are required.
	// cpu and ram are required; localStorage and the whole acceleratable
	// segment are optional. Sliced only appears inside the acceleratable
	// segment, after the accelerator count.

	cases := []struct {
		name           string
		profile        string
		wantGeneralKey string
		wantAccKey     string
		wantSpec       NodeProfileSpec
		wantOK         bool
	}{
		// --- valid shapes ---
		{
			name:           "general profile with localStorage",
			profile:        "gpustack--amd-25-1-16c-32g-88g",
			wantGeneralKey: "amd-25-1",
			wantSpec:       NodeProfileSpec{CPU: "16", RAM: "32", LocalStorage: "88"},
			wantOK:         true,
		},
		{
			name:           "general profile without localStorage",
			profile:        "gpustack--generic-1c-2g",
			wantGeneralKey: "generic",
			wantSpec:       NodeProfileSpec{CPU: "1", RAM: "2"},
			wantOK:         true,
		},
		{
			name:           "single-digit values",
			profile:        "gpustack--generic-1c-1g-15g",
			wantGeneralKey: "generic",
			wantSpec:       NodeProfileSpec{CPU: "1", RAM: "1", LocalStorage: "15"},
			wantOK:         true,
		},
		{
			name:           "acceleratable profile with localStorage and accelerator",
			profile:        "gpustack--amd-25-1-4c-16g-88g--nvidia-tesla-t4-1d",
			wantGeneralKey: "amd-25-1",
			wantAccKey:     "nvidia-tesla-t4",
			wantSpec:       NodeProfileSpec{CPU: "4", RAM: "16", LocalStorage: "88", Accelerator: "1"},
			wantOK:         true,
		},
		{
			name:           "acceleratable profile with accelerator (no localStorage)",
			profile:        "gpustack--amd-25-1-4c-16g--nvidia-tesla-t4-1d",
			wantGeneralKey: "amd-25-1",
			wantAccKey:     "nvidia-tesla-t4",
			wantSpec:       NodeProfileSpec{CPU: "4", RAM: "16", Accelerator: "1"},
			wantOK:         true,
		},
		{
			name:           "acceleratable profile with accelerator and sliced",
			profile:        "gpustack--amd-25-1-12c-48g--nvidia-a10g-1d-8s",
			wantGeneralKey: "amd-25-1",
			wantAccKey:     "nvidia-a10g",
			wantSpec:       NodeProfileSpec{CPU: "12", RAM: "48", Accelerator: "1", SlicedAccelerator: "8"},
			wantOK:         true,
		},
		{
			name:           "acceleratable profile with localStorage, accelerator, and sliced",
			profile:        "gpustack--intel-6-143-48c-192g-88g--nvidia-a10g-4d-8s",
			wantGeneralKey: "intel-6-143",
			wantAccKey:     "nvidia-a10g",
			wantSpec:       NodeProfileSpec{CPU: "48", RAM: "192", LocalStorage: "88", Accelerator: "4", SlicedAccelerator: "8"},
			wantOK:         true,
		},
		{
			// Acceleratable key may itself contain a segment that ends with
			// "g" — the accelerator detector only inspects the trailing
			// segments of its own dash-separated parts.
			name:           "acc key segment ending in g is preserved",
			profile:        "gpustack--generic-4c-16g--nvidia-a10g-1d",
			wantGeneralKey: "generic",
			wantAccKey:     "nvidia-a10g",
			wantSpec:       NodeProfileSpec{CPU: "4", RAM: "16", Accelerator: "1"},
			wantOK:         true,
		},
		// --- prefix rejections ---
		{
			name:    "empty string is rejected",
			profile: "",
			wantOK:  false,
		},
		{
			name:    "old single-dash format is rejected",
			profile: "gpustack-general-16c-32g-96g",
			wantOK:  false,
		},
		{
			name:    "missing gpustack-- prefix",
			profile: "general-16c-32g-96g",
			wantOK:  false,
		},
		{
			name:    "wrong leading prefix",
			profile: "foo--bar-16c-32g-96g",
			wantOK:  false,
		},
		// --- segment count rejections ---
		{
			name:    "three segments are rejected",
			profile: "gpustack--generic-4c-16g--nvidia-t4-1d--foo-1d",
			wantOK:  false,
		},
		// --- key rejections ---
		{
			name:    "missing general key (cpu directly after prefix)",
			profile: "gpustack--16c-32g",
			wantOK:  false,
		},
		{
			name:    "missing acceleratable key (acc directly after separator)",
			profile: "gpustack--generic-4c-16g--1d",
			wantOK:  false,
		},
		// --- cpu / ram rejections ---
		{
			name:    "only ram (cpu missing)",
			profile: "gpustack--generic-32g",
			wantOK:  false,
		},
		{
			name:    "cpu without c suffix",
			profile: "gpustack--generic-16-32g-96g",
			wantOK:  false,
		},
		{
			name:    "ram without g suffix",
			profile: "gpustack--generic-16c-32-96g",
			wantOK:  false,
		},
		{
			name:    "bare cpu (just c)",
			profile: "gpustack--generic-c-32g-96g",
			wantOK:  false,
		},
		{
			name:    "bare ram (just g)",
			profile: "gpustack--generic-16c-g-96g",
			wantOK:  false,
		},
		{
			name:    "non-numeric cpu",
			profile: "gpustack--generic-xc-32g-96g",
			wantOK:  false,
		},
		{
			name:    "non-numeric ram",
			profile: "gpustack--generic-16c-yg-96g",
			wantOK:  false,
		},
		{
			name:    "non-numeric storage",
			profile: "gpustack--generic-16c-32g-zg",
			wantOK:  false,
		},
		// --- accelerator rejections ---
		{
			name:    "acceleratable segment without accelerator count",
			profile: "gpustack--generic-4c-16g--nvidia-tesla-t4",
			wantOK:  false,
		},
		{
			name:    "bare accelerator (just d)",
			profile: "gpustack--generic-4c-16g--nvidia-tesla-t4-d",
			wantOK:  false,
		},
		{
			name:    "non-numeric accelerator",
			profile: "gpustack--generic-4c-16g--nvidia-tesla-t4-xd",
			wantOK:  false,
		},
		// --- sliced rejections ---
		{
			name:    "sliced in the general segment",
			profile: "gpustack--generic-4c-16g-8s",
			wantOK:  false,
		},
		{
			name:    "bare sliced (just s)",
			profile: "gpustack--generic-4c-16g--nvidia-tesla-t4-1d-s",
			wantOK:  false,
		},
		{
			name:    "non-numeric sliced",
			profile: "gpustack--generic-4c-16g--nvidia-tesla-t4-1d-xs",
			wantOK:  false,
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			generalKey, accKey, spec, ok := ParseNodeProfile(cs.profile)
			assert.Equal(t, cs.wantOK, ok, "unexpected ok")
			assert.Equal(t, cs.wantGeneralKey, generalKey, "unexpected general key")
			assert.Equal(t, cs.wantAccKey, accKey, "unexpected acc key")
			assert.Equal(t, cs.wantSpec, spec, "unexpected spec")
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
		name        string
		withCPUName bool
		node        *core.Node
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
			// (a) CPU-only node with blending off: one CPU flavor keyed off
			// "generic", Acceleratable=false, no Product/Family.
			name:        "cpu-only node yields a single generic CPU flavor",
			withCPUName: false,
			node:        newNode(cpuModelLabels(), nil, 8),
			wantCPU: &NodeFlavor{
				Name:         "gpustack-generic-linux-amd64-8c",
				Key:          "generic",
				OS:           "linux",
				Arch:         "amd64",
				Count:        8,
				Manufacturer: "generic",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:            "true",
					core.LabelOSStable:                    "linux",
					core.LabelArchStable:                  "amd64",
					GeneralFeatureLabelPrefix + "generic": "true",
				},
			},
		},
		{
			// (b) Accelerated node with blending on: a CPU flavor (Product/Family
			// populated from the cpu-name/cpu-family annotations) plus one device
			// flavor sized by the .count label. Verifies full os/arch in both Names
			// and the device Memory/Product/Family fields.
			name:        "accelerated node yields CPU plus device flavors",
			withCPUName: true,
			node: newNode(
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "Turing", "15Gi", "2")),
				map[string]string{
					FeatureLabelPrefix + "cpu-name":   "AMD EPYC 7763 64-Core Processor",
					FeatureLabelPrefix + "cpu-family": "25",
				},
				8,
			),
			wantCPU: &NodeFlavor{
				Name:         "gpustack-amd-epyc-7763-linux-amd64-8c",
				Key:          "amd-epyc-7763",
				OS:           "linux",
				Arch:         "amd64",
				Count:        8,
				Manufacturer: "amd",
				Product:      "AMD EPYC 7763 64-Core Processor",
				Family:       "25",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                  "true",
					core.LabelOSStable:                          "linux",
					core.LabelArchStable:                        "amd64",
					GeneralFeatureLabelPrefix + "amd-epyc-7763": "true",
				},
			},
			wantDevices: []NodeFlavor{
				{
					Name:          "gpustack-nvidia-tesla-t4-linux-amd64-2d",
					Key:           "nvidia-tesla-t4",
					OS:            "linux",
					Arch:          "amd64",
					Count:         2,
					Acceleratable: true,
					Manufacturer:  "nvidia",
					Product:       "Tesla-T4",
					Family:        "Turing",
					Memory:        "15Gi",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey:                          "true",
						core.LabelOSStable:                                  "linux",
						core.LabelArchStable:                                "amd64",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4": "true",
					},
				},
			},
		},
		{
			// (c) Node with no CPU capacity: the CPU flavor is skipped. With no
			// device labels either, the result is empty.
			name:        "node without cpu capacity yields no flavors",
			withCPUName: false,
			node:        newNode(cpuModelLabels(), nil, 0),
			wantEmpty:   true,
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			generalNodeKeyWithCPUName = cs.withCPUName
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

func TestDeriveNodeUnitSpec(t *testing.T) {
	// newNode builds a node carrying only the resources named in capacity, so a
	// case can omit memory/storage to exercise the absent-resource paths.
	newNode := func(cpu, mem, storage string) *core.Node {
		capacity := core.ResourceList{}
		if cpu != "" {
			capacity[core.ResourceCPU] = resource.MustParse(cpu)
		}
		if mem != "" {
			capacity[core.ResourceMemory] = resource.MustParse(mem)
		}
		if storage != "" {
			capacity[core.ResourceEphemeralStorage] = resource.MustParse(storage)
		}
		return &core.Node{Status: core.NodeStatus{Capacity: capacity}}
	}

	cases := []struct {
		name             string
		node             *core.Node
		count            int64
		acceleratable    bool
		wantCPU          string
		wantRAM          string
		wantLocalStorage string
	}{
		{
			// Accelerated: unitCPU = cpu/count, unitRAM = ramGi/count, storage
			// is the node total (never divided).
			name:             "accelerated divides cpu and ram by device count",
			node:             newNode("8", "32Gi", "100Gi"),
			count:            4,
			acceleratable:    true,
			wantCPU:          "2",
			wantRAM:          "8",
			wantLocalStorage: "100",
		},
		{
			// Non-accelerated flavor: the factory default unit spec is a fixed
			// 1 core / 2 Gi regardless of the node's cpu/ram; storage is the node total.
			name:             "non-accelerated flavor uses the factory 1c-2g default",
			node:             newNode("8", "32Gi", "100Gi"),
			count:            8,
			acceleratable:    false,
			wantCPU:          "1",
			wantRAM:          "2",
			wantLocalStorage: "100",
		},
		{
			// RAM rounds up to an even Gi (31 -> 32). Storage absent -> 0.
			name:             "ram rounds up to even and absent storage is zero",
			node:             newNode("4", "31Gi", ""),
			count:            4,
			acceleratable:    true,
			wantCPU:          "1",
			wantRAM:          "8",
			wantLocalStorage: "0",
		},
		{
			// Storage rounds down to an even Gi (101 -> 100).
			name:             "storage rounds down to even",
			node:             newNode("8", "32Gi", "101Gi"),
			count:            4,
			acceleratable:    true,
			wantCPU:          "2",
			wantRAM:          "8",
			wantLocalStorage: "100",
		},
		{
			// localStorage is never divided by count: 200Gi stays 200, not 25.
			name:             "local storage is not divided by count",
			node:             newNode("8", "64Gi", "200Gi"),
			count:            8,
			acceleratable:    true,
			wantCPU:          "1",
			wantRAM:          "8",
			wantLocalStorage: "200",
		},
		{
			// max(...,1) guards: cpu/count and ram/count both floor to 0 but are
			// clamped to 1.
			name:             "unit cpu and ram floor to one",
			node:             newNode("2", "4Gi", "10Gi"),
			count:            8,
			acceleratable:    true,
			wantCPU:          "1",
			wantRAM:          "1",
			wantLocalStorage: "10",
		},
		{
			// count <= 0 is treated as 1, so nothing is divided.
			name:             "non-positive count is treated as one",
			node:             newNode("8", "32Gi", "100Gi"),
			count:            0,
			acceleratable:    true,
			wantCPU:          "8",
			wantRAM:          "32",
			wantLocalStorage: "100",
		},
		{
			// CPU flavor keeps unitCPU=1 regardless of count, and a 1Gi storage
			// rounds down to 0 (covering the max(stgGi,0) guard).
			name:             "cpu flavor with tiny storage clamps to zero",
			node:             newNode("1", "1Gi", "1Gi"),
			count:            1,
			acceleratable:    false,
			wantCPU:          "1",
			wantRAM:          "2",
			wantLocalStorage: "0",
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			unitCPU, unitRAM, localStorage := DeriveNodeUnitSpec(cs.node, cs.count, cs.acceleratable)
			assert.Equal(t, cs.wantCPU, unitCPU, "unexpected unit cpu")
			assert.Equal(t, cs.wantRAM, unitRAM, "unexpected unit ram")
			assert.Equal(t, cs.wantLocalStorage, localStorage, "unexpected local storage")
		})
	}
}
