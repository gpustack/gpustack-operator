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
		NFDCPUModelLabelPrefix + "vendor_id": "AMD",
		NFDCPUModelLabelPrefix + "family":    "25",
		NFDCPUModelLabelPrefix + "id":        "1",
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
				FeatureLabelPrefix + "acceleratable":                                   "true",
				AcceleratableFeatureLabelPrefix + "nvidia":                             "true",
				AcceleratableFeatureLabelPrefix + "nvidia.driver-version":              "580.126.09",
				AcceleratableFeatureLabelPrefix + "nvidia.runtime-version":             "13.0",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4":                    "true",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.product":            "Tesla-T4",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.memory":             "15Gi",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.cores":              "2560",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.family":             "Turing",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.compute-capability": "7.5",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.accelerators":       "4",
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
				FeatureLabelPrefix + "acceleratable":                         "true",
				AcceleratableFeatureLabelPrefix + "nvidia":                   "true",
				AcceleratableFeatureLabelPrefix + "nvidia-h100":              "true",
				AcceleratableFeatureLabelPrefix + "nvidia-h100.product":      "H100",
				AcceleratableFeatureLabelPrefix + "nvidia-h100.memory":       "80Gi",
				AcceleratableFeatureLabelPrefix + "nvidia-h100.cores":        "0",
				AcceleratableFeatureLabelPrefix + "nvidia-h100.accelerators": "1",
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
				FeatureLabelPrefix + "acceleratable":                             "true",
				AcceleratableFeatureLabelPrefix + "nvidia":                       "true",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4":              "true",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.memory":       "15Gi",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.cores":        "0",
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "1",
				AcceleratableFeatureLabelPrefix + "amd":                          "true",
				AcceleratableFeatureLabelPrefix + "amd-mi300x":                   "true",
				AcceleratableFeatureLabelPrefix + "amd-mi300x.product":           "MI300X",
				AcceleratableFeatureLabelPrefix + "amd-mi300x.memory":            "192Gi",
				AcceleratableFeatureLabelPrefix + "amd-mi300x.cores":             "0",
				AcceleratableFeatureLabelPrefix + "amd-mi300x.accelerators":      "2",
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
						AcceleratableFeatureLabelPrefix + "nvidia":                             "true",
						AcceleratableFeatureLabelPrefix + "nvidia.driver-version":              "580.126.09",
						AcceleratableFeatureLabelPrefix + "nvidia.runtime-version":             "13.0",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4":                    "true",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.accelerators":       "4",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.compute-capability": "7.5",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.cores":              "2560",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.family":             "Turing",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.memory":             "15Gi",
						AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.product":            "Tesla-T4",
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
						AcceleratableFeatureLabelPrefix + "unknown":                             "true",
						AcceleratableFeatureLabelPrefix + "unknown.driver-version":              "580.126.09",
						AcceleratableFeatureLabelPrefix + "unknown.runtime-version":             "13.0",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4":                    "true",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.accelerators":       "4",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.compute-capability": "7.5",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.cores":              "2560",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.family":             "Turing",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.memory":             "15Gi",
						AcceleratableFeatureLabelPrefix + "unknown-tesla-t4.product":            "Tesla-T4",
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
	newNode := func(labels map[string]string) *core.Node {
		return &core.Node{
			ObjectMeta: meta.ObjectMeta{
				Labels: labels,
			},
		}
	}

	cases := []struct {
		name     string
		node     *core.Node
		expected string
	}{
		{
			name:     "no cpu-model labels falls back to generic",
			node:     newNode(map[string]string{}),
			expected: "generic",
		},
		{
			name:     "AMD family 25 model 1",
			node:     newNode(cpuModelLabels()),
			expected: "amd-25-1",
		},
		{
			name: "GenuineIntel normalizes to intel",
			node: newNode(map[string]string{
				NFDCPUModelLabelPrefix + "vendor_id": "GenuineIntel",
				NFDCPUModelLabelPrefix + "family":    "6",
				NFDCPUModelLabelPrefix + "id":        "143",
			}),
			expected: "intel-6-143",
		},
		{
			name: "vendor without family falls back to generic",
			node: newNode(map[string]string{
				NFDCPUModelLabelPrefix + "vendor_id": "AMD",
				NFDCPUModelLabelPrefix + "id":        "1",
			}),
			expected: "generic",
		},
		{
			name: "vendor without id falls back to generic",
			node: newNode(map[string]string{
				NFDCPUModelLabelPrefix + "vendor_id": "AMD",
				NFDCPUModelLabelPrefix + "family":    "25",
			}),
			expected: "generic",
		},
		{
			name: "unrecognized characters are stripped",
			node: newNode(map[string]string{
				NFDCPUModelLabelPrefix + "vendor_id": " Foo Bar! ",
				NFDCPUModelLabelPrefix + "family":    "25",
				NFDCPUModelLabelPrefix + "id":        "1",
			}),
			expected: "foobar-25-1",
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			assert.Equal(t, cs.expected, ExtractGeneralNodeKey(cs.node), "unexpected general node key")
		})
	}
}

func TestExtractGeneralNodeKeys(t *testing.T) {
	cases := []struct {
		name     string
		labels   map[string]string
		expected []string
	}{
		{
			name:     "no labels",
			labels:   nil,
			expected: nil,
		},
		{
			name: "single general key",
			labels: map[string]string{
				GeneralFeatureLabelPrefix + "amd":                     "true",
				GeneralFeatureLabelPrefix + "amd-25-1":                "true",
				GeneralFeatureLabelPrefix + "amd-25-1.cpu":            "16",
				GeneralFeatureLabelPrefix + "amd-25-1.profile-flavor": "16c-32g-96g",
				GeneralFeatureLabelPrefix + "amd-25-1.profile-queue":  "1c-2g",
			},
			expected: []string{"amd-25-1"},
		},
		{
			name: "multiple general keys are sorted",
			labels: map[string]string{
				GeneralFeatureLabelPrefix + "intel-6-143.profile-flavor": "8c-16g-96g",
				GeneralFeatureLabelPrefix + "amd-25-1.profile-flavor":    "16c-32g-96g",
			},
			expected: []string{"amd-25-1", "intel-6-143"},
		},
		{
			name: "acceleratable and old-prefix labels are ignored",
			labels: map[string]string{
				AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4.profile-flavor": "4c-16g-96g-1d",
				FeatureLabelPrefix + "general.profile-flavor":                      "16c-32g-96g",
			},
			expected: nil,
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			node := &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Labels: cs.labels,
				},
			}
			assert.Equal(t, cs.expected, ExtractGeneralNodeKeys(node), "unexpected general node keys")
		})
	}
}

func TestConstructNodeCapacityLabels(t *testing.T) {
	// Cluster cluster-1 has 5 nodes. Their raw physical resources are:
	//
	//   node-1:           16C /  32G / 100G
	//   node-2: T4   1D    4C /  16G / 100G
	//   node-3: T4   1D    8C /  32G / 100G
	//   node-4: T4   2D    8C /  32G / 100G
	//   node-5: A10G 4D   48C / 192G / 200G
	//
	// kubelet's Node.Status.Capacity does not perfectly mirror physical
	// resources — the kernel reserves some RAM, the filesystem reserves
	// inodes/superblocks for the boot volume, etc. — so simulated capacity
	// is intentionally slightly less than each node's advertised total.
	// This exercises the odd-Gi rounding (RAM rounds up to the next even
	// Gi; local-storage rounds down).
	//
	// All cluster-1 nodes carry the NFD cpu-model labels of an AMD family 25
	// model 1 CPU, so their general(CPU) node key is "amd-25-1".

	gPfx := GeneralFeatureLabelPrefix + "amd-25-1"
	t4Pfx := AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"
	a10gPfx := AcceleratableFeatureLabelPrefix + "nvidia-a10g"

	// ConstructNodeCapacityLabels reads accelerator count from the
	// ${nodeKey}.accelerators label (emitted by applyAcceleratorLabels), not from
	// Node.Status.Capacity[<gpu resource>]. The capacity passed to newNode
	// only carries CPU / RAM / ephemeral-storage; device count is steered
	// entirely through deviceLabels(..., accelerators).
	newNode := func(name, cpu, mem, storage string, labels map[string]string) *core.Node {
		capacity := core.ResourceList{
			core.ResourceCPU:              resource.MustParse(cpu),
			core.ResourceMemory:           resource.MustParse(mem),
			core.ResourceEphemeralStorage: resource.MustParse(storage),
		}
		if labels == nil {
			labels = map[string]string{}
		}
		labels[core.LabelHostname] = name
		return &core.Node{
			ObjectMeta: meta.ObjectMeta{
				Name:   name,
				Labels: labels,
			},
			Status: core.NodeStatus{
				Capacity:    capacity,
				Allocatable: capacity,
			},
		}
	}

	// deviceLabels mirrors what ConstructAcceleratableNodeLabels would have put on
	// the node before ConstructNodeCapacityLabels is invoked.
	deviceLabels := func(id, product, memory, accelerators string) map[string]string {
		ndKey := AcceleratableFeatureLabelPrefix + "nvidia-" + id
		return map[string]string{
			systemname.ManagedLabelKey:                 "true",
			AcceleratableFeatureLabelPrefix + "nvidia": "true",
			ndKey:                   "true",
			ndKey + ".product":      product,
			ndKey + ".memory":       memory,
			ndKey + ".cores":        "0",
			ndKey + ".accelerators": accelerators,
		}
	}

	// generalExpected is the general(CPU) view of an amd-25-1 node.
	generalExpected := func(cpu, ram, stg, flavor, unit string) map[string]string {
		return map[string]string{
			systemname.ManagedLabelKey:        "true",
			GeneralFeatureLabelPrefix + "amd": "true",
			gPfx:                              "true",
			gPfx + ".family":                  "25",
			gPfx + ".cpu":                     cpu,
			gPfx + ".ram":                     ram,
			gPfx + ".local-storage":           stg,
			gPfx + ".profile-flavor":          flavor,
			gPfx + ".profile-queue":           unit,
			gPfx + ".profile-cohort":          unit,
		}
	}

	// deviceExpected is the acceleratable(device) view added by the device loop.
	deviceExpected := func(pfx, cpu, ram, stg, flavor, queue, cohort string) map[string]string {
		return map[string]string{
			pfx + ".cpu":            cpu,
			pfx + ".ram":            ram,
			pfx + ".local-storage":  stg,
			pfx + ".profile-flavor": flavor,
			pfx + ".profile-queue":  queue,
			pfx + ".profile-cohort": cohort,
		}
	}

	cases := []struct {
		name     string
		node     *core.Node
		opts     []ConstructNodeCapacityLabelsOption
		expected map[string]string
	}{
		{
			// 32G RAM is exposed by kubelet as 31Gi (kernel reservation),
			// odd, rounds up to 32Gi. 100G disk is exposed as 97Gi, odd,
			// rounds down to 96Gi.
			name:     "cluster-1/node-1 cpu-only 16C/32G/100G",
			node:     newNode("cluster-1-node-1", "16", "31Gi", "97Gi", cpuModelLabels()),
			expected: generalExpected("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
		},
		{
			// No NFD cpu-model labels — the general key falls back to
			// "generic": single marker label, no .family label.
			name: "cpu-only node without cpu-model labels uses generic key",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi", nil),
			expected: map[string]string{
				systemname.ManagedLabelKey:                           "true",
				GeneralFeatureLabelPrefix + "generic":                "true",
				GeneralFeatureLabelPrefix + "generic.cpu":            "16",
				GeneralFeatureLabelPrefix + "generic.ram":            "32Gi",
				GeneralFeatureLabelPrefix + "generic.local-storage":  "96Gi",
				GeneralFeatureLabelPrefix + "generic.profile-flavor": "16c-32g-96g",
				GeneralFeatureLabelPrefix + "generic.profile-queue":  "1c-2g",
				GeneralFeatureLabelPrefix + "generic.profile-cohort": "1c-2g",
			},
		},
		{
			// GenuineIntel vendor string is normalized to "intel".
			name: "cpu-only node with GenuineIntel cpu-model labels",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi", map[string]string{
				NFDCPUModelLabelPrefix + "vendor_id": "GenuineIntel",
				NFDCPUModelLabelPrefix + "family":    "6",
				NFDCPUModelLabelPrefix + "id":        "143",
			}),
			expected: map[string]string{
				systemname.ManagedLabelKey:                               "true",
				GeneralFeatureLabelPrefix + "intel":                      "true",
				GeneralFeatureLabelPrefix + "intel-6-143":                "true",
				GeneralFeatureLabelPrefix + "intel-6-143.family":         "6",
				GeneralFeatureLabelPrefix + "intel-6-143.cpu":            "16",
				GeneralFeatureLabelPrefix + "intel-6-143.ram":            "32Gi",
				GeneralFeatureLabelPrefix + "intel-6-143.local-storage":  "96Gi",
				GeneralFeatureLabelPrefix + "intel-6-143.profile-flavor": "16c-32g-96g",
				GeneralFeatureLabelPrefix + "intel-6-143.profile-queue":  "1c-2g",
				GeneralFeatureLabelPrefix + "intel-6-143.profile-cohort": "1c-2g",
			},
		},
		{
			// 1xT4 on a small 4C/16G box. Per-device cpu/ram equals the
			// general values since there is exactly one accelerator.
			name: "cluster-1/node-2 T4 1D 4C/16G/100G",
			node: newNode(
				"cluster-1-node-2", "4", "15Gi", "97Gi",
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "1")),
			),
			expected: mergeLabels(
				generalExpected("4", "16Gi", "96Gi", "4c-16g-96g", "1c-4g"),
				deviceExpected(t4Pfx, "4", "16Gi", "96Gi", "4c-16g-96g-1d", "4c-16g-1d", "4c-16g-1d"),
			),
		},
		{
			// Same GPU model as node-2 but on a larger 8C/32G box.
			name: "cluster-1/node-3 T4 1D 8C/32G/100G",
			node: newNode(
				"cluster-1-node-3", "8", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "1")),
			),
			expected: mergeLabels(
				generalExpected("8", "32Gi", "96Gi", "8c-32g-96g", "1c-4g"),
				deviceExpected(t4Pfx, "8", "32Gi", "96Gi", "8c-32g-96g-1d", "8c-32g-1d", "8c-32g-1d"),
			),
		},
		{
			// 2xT4 on the same box as node-3. Per-device cpu/ram is shared
			// across both devices, so the per-device profile-cohort unit is
			// halved by the device count.
			name: "cluster-1/node-4 T4 2D 8C/32G/100G",
			node: newNode(
				"cluster-1-node-4", "8", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "2")),
			),
			expected: mergeLabels(
				generalExpected("8", "32Gi", "96Gi", "8c-32g-96g", "1c-4g"),
				deviceExpected(t4Pfx, "8", "32Gi", "96Gi", "8c-32g-96g-2d", "4c-16g-1d", "4c-16g-1d"),
			),
		},
		{
			// 4xA10G on a large box. 192G → 188Gi (even, stays). ram-unit
			// is integer-divided: 188/48 = 3. local-storage 196Gi is even,
			// stays as-is. Per-device ram-unit: 188/4 = 47.
			name: "cluster-1/node-5 A10G 4D 48C/192G/200G",
			node: newNode(
				"cluster-1-node-5", "48", "188Gi", "196Gi",
				mergeLabels(cpuModelLabels(), deviceLabels("a10g", "A10G", "23Gi", "4")),
			),
			expected: mergeLabels(
				generalExpected("48", "188Gi", "196Gi", "48c-188g-196g", "1c-3g"),
				deviceExpected(a10gPfx, "48", "188Gi", "196Gi", "48c-188g-196g-4d", "12c-47g-1d", "12c-47g-1d"),
			),
		},
		{
			// User-supplied ${nodeKey}.sliced.partitions=8 appends "-8s"
			// to profile-flavor and profile-queue. profile-cohort is the
			// cohort-level per-unit view and is never sliced-suffixed —
			// it is the matching key cross-flavor at the cohort level.
			name: "cluster-1/node-5 A10G 4D 48C/192G/200G with sliced.partitions=8",
			node: func() *core.Node {
				lbs := mergeLabels(cpuModelLabels(), deviceLabels("a10g", "A10G", "23Gi", "4"))
				lbs[a10gPfx+".sliced.partitions"] = "8"
				return newNode("cluster-1-node-5", "48", "188Gi", "196Gi", lbs)
			}(),
			expected: mergeLabels(
				generalExpected("48", "188Gi", "196Gi", "48c-188g-196g", "1c-3g"),
				deviceExpected(a10gPfx, "48", "188Gi", "196Gi", "48c-188g-196g-4d-8s", "12c-47g-1d-8s", "12c-47g-1d"),
			),
		},
		{
			// Non-positive / unparsable sliced.partitions values are
			// silently ignored — profile-flavor stays without the suffix.
			name: "cluster-1/node-5 A10G with sliced.partitions=0 yields no suffix",
			node: func() *core.Node {
				lbs := mergeLabels(cpuModelLabels(), deviceLabels("a10g", "A10G", "23Gi", "4"))
				lbs[a10gPfx+".sliced.partitions"] = "0"
				return newNode("cluster-1-node-5", "48", "188Gi", "196Gi", lbs)
			}(),
			expected: mergeLabels(
				generalExpected("48", "188Gi", "196Gi", "48c-188g-196g", "1c-3g"),
				deviceExpected(a10gPfx, "48", "188Gi", "196Gi", "48c-188g-196g-4d", "12c-47g-1d", "12c-47g-1d"),
			),
		},
		{
			// Existing capacity labels take precedence over Status.Capacity
			// — covers idempotent re-runs and operator overrides.
			name: "existing capacity labels override Status.Capacity",
			node: newNode(
				"cluster-1-node-1", "16", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), map[string]string{
					gPfx + ".cpu":           "8",
					gPfx + ".ram":           "16Gi",
					gPfx + ".local-storage": "50Gi",
				}),
			),
			expected: generalExpected("8", "16Gi", "50Gi", "8c-16g-50g", "1c-2g"),
		},
		{
			// No Status.Capacity — exercises fallback defaults: cpu→1,
			// ram→cpu (so 1Gi), local-storage→15Gi when cpu==1.
			name: "missing Status.Capacity falls back to defaults",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name:   "blank-node",
					Labels: cpuModelLabels(),
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{},
				},
			},
			expected: generalExpected("1", "1Gi", "15Gi", "1c-1g-15g", "1c-1g"),
		},
		{
			// cpu present but ephemeral-storage absent → fallback is
			// 15Gi * cpuC when cpuC > 1.
			name:     "missing local-storage falls back to 15Gi * cpuC",
			node:     newNode("cluster-1-node-1", "16", "31Gi", "0", cpuModelLabels()),
			expected: generalExpected("16", "32Gi", "240Gi", "16c-32g-240g", "1c-2g"),
		},
		{
			// Per-device storage now scales with accC, not cpuC. With
			// ephemeral-storage absent and accC=2, device storage falls
			// back to 15Gi * accC = 30Gi — independent of general's
			// 15Gi * cpuC = 120Gi. Confirms the device loop no longer
			// inherits stgC from the general view.
			name: "missing local-storage on accelerated node uses 15Gi * accC for device",
			node: newNode(
				"cluster-1-node-4", "8", "31Gi", "0",
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "2")),
			),
			expected: mergeLabels(
				generalExpected("8", "32Gi", "120Gi", "8c-32g-120g", "1c-4g"),
				deviceExpected(t4Pfx, "8", "32Gi", "30Gi", "8c-32g-30g-2d", "4c-16g-1d", "4c-16g-1d"),
			),
		},
		{
			// Accelerated feature label is set but the ${nodeKey}.accelerators
			// label is absent — the per-device loop reads accelerator count
			// strictly from that label, so the device is skipped entirely
			// (no fallback). Only the general capacity labels are emitted.
			name: "accelerated feature label without .accelerators is skipped",
			node: newNode(
				"cluster-1-node-2", "4", "15Gi", "97Gi",
				mergeLabels(cpuModelLabels(), map[string]string{
					systemname.ManagedLabelKey:                 "true",
					AcceleratableFeatureLabelPrefix + "nvidia": "true",
					t4Pfx: "true",
				}),
			),
			expected: generalExpected("4", "16Gi", "96Gi", "4c-16g-96g", "1c-4g"),
		},
		{
			// Existing managed=true on the node is preserved verbatim.
			name: "managed label is true on node",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), map[string]string{
					systemname.ManagedLabelKey: "true",
				})),
			expected: generalExpected("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
		},
		{
			// Existing managed=false on the node overrides the default "true"
			// — capacity labels are still emitted regardless.
			name: "managed label is false on node",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), map[string]string{
					systemname.ManagedLabelKey: "false",
				})),
			expected: mergeLabels(
				generalExpected("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
				map[string]string{systemname.ManagedLabelKey: "false"},
			),
		},
		{
			// OverrideGeneralRAMGiPerCPU(2) replaces the whole general view —
			// general .ram, .profile-flavor, .profile-queue and
			// .profile-cohort all reflect generalRamC = 2*cpuC = 32Gi,
			// regardless of the underlying ramC (here floored to cpuC=16
			// because Memory=0).
			name:     "override RAM-Gi-per-CPU rewrites the whole general view",
			node:     newNode("cluster-1-node-1", "16", "0", "97Gi", cpuModelLabels()),
			opts:     []ConstructNodeCapacityLabelsOption{OverrideGeneralRAMGiPerCPU(2)},
			expected: generalExpected("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
		},
		{
			// Status.Capacity Memory=64Gi → real ramC=64, but the override
			// supersedes it across the entire general view: ram=32Gi,
			// profile-flavor=16c-32g-96g, profile-queue=1c-2g. Demonstrates
			// that the override pre-empts Status.Capacity for general.
			name:     "override RAM-Gi-per-CPU pre-empts Status.Capacity for general view",
			node:     newNode("cluster-1-node-1", "16", "64Gi", "97Gi", cpuModelLabels()),
			opts:     []ConstructNodeCapacityLabelsOption{OverrideGeneralRAMGiPerCPU(2)},
			expected: generalExpected("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
		},
		{
			// An explicit operator general .ram label wins over the override:
			// the override is only consulted at first discovery when no prior
			// label has been written. Here ramC=48 from the label drives the
			// whole general view; the override is bypassed.
			name: "explicit general .ram label wins over override",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), map[string]string{
					gPfx + ".ram": "48Gi",
				})),
			opts:     []ConstructNodeCapacityLabelsOption{OverrideGeneralRAMGiPerCPU(2)},
			expected: generalExpected("16", "48Gi", "96Gi", "16c-48g-96g", "1c-3g"),
		},
		{
			// 0 is the unset sentinel — the override branch is skipped and
			// the general labels fall back to the real ramC.
			name:     "zero override RAM-Gi-per-CPU is treated as unset",
			node:     newNode("cluster-1-node-1", "16", "31Gi", "97Gi", cpuModelLabels()),
			opts:     []ConstructNodeCapacityLabelsOption{OverrideGeneralRAMGiPerCPU(0)},
			expected: generalExpected("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
		},
		{
			// Negative override is also treated as unset (the branch guard
			// is `> 0`), guarding against accidental negative inputs.
			name:     "negative override RAM-Gi-per-CPU is treated as unset",
			node:     newNode("cluster-1-node-1", "16", "31Gi", "97Gi", cpuModelLabels()),
			opts:     []ConstructNodeCapacityLabelsOption{OverrideGeneralRAMGiPerCPU(-4)},
			expected: generalExpected("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
		},
		{
			// 1-cpu node, override=2 → generalRamC = 2*1 = 2.
			// Confirms the override scales by the resolved cpuC and applies
			// uniformly across the general view.
			name:     "override RAM-Gi-per-CPU=2 on 1C node yields 2Gi general view",
			node:     newNode("blank-node", "1", "0", "0", cpuModelLabels()),
			opts:     []ConstructNodeCapacityLabelsOption{OverrideGeneralRAMGiPerCPU(2)},
			expected: generalExpected("1", "2Gi", "15Gi", "1c-2g-15g", "1c-2g"),
		},
		{
			// Override is scoped to general only. With cpuC=8 and 2
			// accelerators, the general view shows generalRamC=16
			// (=2*8). Per-device labels derive independently: with
			// Memory=0 the device ramC falls back to accC=2, so
			// device .ram=2Gi, profile-queue cpuUnit=8/2=4 and
			// ramUnit=2/2=1 → 4c-1g-1d. The general view's 16Gi is
			// not leaking into the device view.
			name: "override RAM-Gi-per-CPU does not affect per-device labels",
			node: newNode(
				"cluster-1-node-4", "8", "0", "97Gi",
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "2")),
			),
			opts: []ConstructNodeCapacityLabelsOption{OverrideGeneralRAMGiPerCPU(2)},
			expected: mergeLabels(
				generalExpected("8", "16Gi", "96Gi", "8c-16g-96g", "1c-2g"),
				deviceExpected(t4Pfx, "8", "2Gi", "96Gi", "8c-2g-96g-2d", "4c-1g-1d", "4c-1g-1d"),
			),
		},
		{
			// Even when Status.Capacity reports a real Memory value, the
			// override leaves per-device labels untouched. cpuC=8, real
			// ramC=64 from Memory=64Gi, override=2 → general view shows
			// 16Gi (=2*8) but the device labels reflect real ramC=64:
			// device .ram=64Gi, profile-queue cpuUnit=8/2=4 / ramUnit=64/2=32.
			name: "override RAM-Gi-per-CPU leaves per-device labels at real capacity",
			node: newNode(
				"cluster-1-node-4", "8", "64Gi", "97Gi",
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "2")),
			),
			opts: []ConstructNodeCapacityLabelsOption{OverrideGeneralRAMGiPerCPU(2)},
			expected: mergeLabels(
				generalExpected("8", "16Gi", "96Gi", "8c-16g-96g", "1c-2g"),
				deviceExpected(t4Pfx, "8", "64Gi", "96Gi", "8c-64g-96g-2d", "4c-32g-1d", "4c-32g-1d"),
			),
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ConstructNodeCapacityLabels(cs.node, cs.opts...)
			assert.Equal(t, cs.expected, actual, "unexpected capacity labels")
		})
	}
}

func TestExtractNodeResourceFlavors(t *testing.T) {
	// Output shape:
	//   ndfs[0]  = the CPU/general flavor, emitted only when all six
	//              general capacity labels (cpu, ram, local-storage,
	//              profile-flavor, profile-queue, profile-cohort) are
	//              present under the general(CPU) node key recorded by
	//              ConstructNodeCapacityLabels.
	//   ndfs[1:] = one flavor per accelerated node key for which the
	//              per-device .accelerators, .cpu, .ram, .local-storage,
	//              .profile-flavor, .profile-queue, and .profile-cohort
	//              labels are all present. The Accelerator field is read
	//              directly from the .accelerators label. Each device
	//              flavor is paired with the node's general(CPU) key and
	//              pins it via NodeLabels.
	//
	// The accelerated-key loop iterates a Go map, so the device-flavor
	// order is non-deterministic. We compare the head directly and the
	// tail as a multiset via ElementsMatch.

	gKey := "amd-25-1"
	gPfx := GeneralFeatureLabelPrefix + gKey
	nvPfx := AcceleratableFeatureLabelPrefix + "nvidia"
	t4Pfx := AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"
	a10gPfx := AcceleratableFeatureLabelPrefix + "nvidia-a10g"
	amdPfx := AcceleratableFeatureLabelPrefix + "amd"
	mi300xPfx := AcceleratableFeatureLabelPrefix + "amd-mi300x"

	expectedToleration := []core.Toleration{
		{Operator: core.TolerationOpExists},
	}

	// generalLabels is the post-ConstructNodeCapacityLabels general view.
	generalLabels := func(cpu, ram, stg, flavor, unit string) map[string]string {
		return map[string]string{
			gPfx + ".cpu":            cpu,
			gPfx + ".ram":            ram,
			gPfx + ".local-storage":  stg,
			gPfx + ".profile-flavor": flavor,
			gPfx + ".profile-queue":  unit,
			gPfx + ".profile-cohort": unit,
		}
	}

	// cpuFlavor is the expected general(CPU) flavor of an amd-25-1 node.
	cpuFlavor := func(flavor, unit, cpu, ram, stg string) NodeResourceFlavor {
		return NodeResourceFlavor{
			GeneralKey:        gKey,
			ProfileFlavorSpec: flavor,
			ProfileQueueSpec:  unit,
			ProfileCohortSpec: unit,
			NodeLabels: map[string]string{
				systemname.ManagedLabelKey: "true",
				gPfx + ".profile-queue":    unit,
			},
			Tolerations:     expectedToleration,
			CPUManufacturer: "amd",
			CPUFamily:       "25",
			CPUID:           "1",
			CPU:             cpu,
			RAM:             ram,
			LocalStorage:    stg,
		}
	}

	cases := []struct {
		name string
		// labels populates node.ObjectMeta.Labels. The per-device
		// Accelerator field sources from the ${nodeKey}.accelerators
		// label written by applyAcceleratorLabels.
		labels      map[string]string
		wantCPU     NodeResourceFlavor
		wantDevices []NodeResourceFlavor
		wantEmpty   bool
	}{
		{
			// CPU-only node — only the general flavor is emitted.
			name: "cluster-1/node-1 cpu-only 16C/32Gi/96Gi",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey: "true",
					core.LabelHostname:         "cluster-1-node-1",
				},
				generalLabels("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
			),
			wantCPU: cpuFlavor("16c-32g-96g", "1c-2g", "16", "32Gi", "96Gi"),
		},
		{
			// 1xT4 — CPU/general + one nvidia-tesla-t4 device flavor.
			name: "cluster-1/node-2 T4 1D 4C/16Gi/96Gi",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey: "true",
					core.LabelHostname:         "cluster-1-node-2",
					nvPfx:                      "true",
					t4Pfx:                      "true",
					t4Pfx + ".product":         "Tesla-T4",
					t4Pfx + ".memory":          "15Gi",
					t4Pfx + ".cores":           "0",
					t4Pfx + ".accelerators":    "1",
					t4Pfx + ".cpu":             "4",
					t4Pfx + ".ram":             "16Gi",
					t4Pfx + ".local-storage":   "96Gi",
					t4Pfx + ".profile-flavor":  "4c-16g-96g-1d",
					t4Pfx + ".profile-queue":   "4c-16g-1d",
					t4Pfx + ".profile-cohort":  "4c-16g-1d",
				},
				generalLabels("4", "16Gi", "96Gi", "4c-16g-96g", "1c-4g"),
			),
			wantCPU: cpuFlavor("4c-16g-96g", "1c-4g", "4", "16Gi", "96Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					GeneralKey:        gKey,
					Key:               "nvidia-tesla-t4",
					ProfileFlavorSpec: "4c-16g-96g-1d",
					ProfileQueueSpec:  "4c-16g-1d",
					ProfileCohortSpec: "4c-16g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						t4Pfx + ".profile-queue":   "4c-16g-1d",
					},
					Tolerations:     expectedToleration,
					Acceleratable:   true,
					CPUManufacturer: "amd",
					CPUFamily:       "25",
					CPUID:           "1",
					Manufacturer:    "nvidia",
					Product:         "Tesla-T4",
					Memory:          "15Gi",
					Cores:           "0",
					Accelerator:     "1",
					CPU:             "4",
					RAM:             "16Gi",
					LocalStorage:    "96Gi",
				},
			},
		},
		{
			// 2xT4 — ProfileFlavorSpec differs from node-2 (absolute capacities)
			// but ProfileCohortSpec matches node-2 (same per-device shape:
			// 4 cpu / 16Gi ram per device), demonstrating the Kueue pooling
			// invariant.
			name: "cluster-1/node-4 T4 2D 8C/32Gi/96Gi",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey: "true",
					core.LabelHostname:         "cluster-1-node-4",
					nvPfx:                      "true",
					t4Pfx:                      "true",
					t4Pfx + ".product":         "Tesla-T4",
					t4Pfx + ".memory":          "15Gi",
					t4Pfx + ".cores":           "0",
					t4Pfx + ".accelerators":    "2",
					t4Pfx + ".cpu":             "8",
					t4Pfx + ".ram":             "32Gi",
					t4Pfx + ".local-storage":   "96Gi",
					t4Pfx + ".profile-flavor":  "8c-32g-96g-2d",
					t4Pfx + ".profile-queue":   "4c-16g-1d",
					t4Pfx + ".profile-cohort":  "4c-16g-1d",
				},
				generalLabels("8", "32Gi", "96Gi", "8c-32g-96g", "1c-4g"),
			),
			wantCPU: cpuFlavor("8c-32g-96g", "1c-4g", "8", "32Gi", "96Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					GeneralKey:        gKey,
					Key:               "nvidia-tesla-t4",
					ProfileFlavorSpec: "8c-32g-96g-2d",
					ProfileQueueSpec:  "4c-16g-1d", // == node-2's device per-unit profile
					ProfileCohortSpec: "4c-16g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						t4Pfx + ".profile-queue":   "4c-16g-1d",
					},
					Tolerations:     expectedToleration,
					Acceleratable:   true,
					CPUManufacturer: "amd",
					CPUFamily:       "25",
					CPUID:           "1",
					Manufacturer:    "nvidia",
					Product:         "Tesla-T4",
					Memory:          "15Gi",
					Cores:           "0",
					Accelerator:     "2",
					CPU:             "8",
					RAM:             "32Gi",
					LocalStorage:    "96Gi",
				},
			},
		},
		{
			// 4xA10G — 48-cpu / 188Gi-ram totals, integer-divided per-device
			// ram-unit = 188/4 = 47Gi.
			name: "cluster-1/node-5 A10G 4D 48C/188Gi/196Gi",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey:  "true",
					core.LabelHostname:          "cluster-1-node-5",
					nvPfx:                       "true",
					a10gPfx:                     "true",
					a10gPfx + ".product":        "A10G",
					a10gPfx + ".memory":         "23Gi",
					a10gPfx + ".cores":          "0",
					a10gPfx + ".accelerators":   "4",
					a10gPfx + ".cpu":            "48",
					a10gPfx + ".ram":            "188Gi",
					a10gPfx + ".local-storage":  "196Gi",
					a10gPfx + ".profile-flavor": "48c-188g-196g-4d",
					a10gPfx + ".profile-queue":  "12c-47g-1d",
					a10gPfx + ".profile-cohort": "12c-47g-1d",
				},
				generalLabels("48", "188Gi", "196Gi", "48c-188g-196g", "1c-3g"),
			),
			wantCPU: cpuFlavor("48c-188g-196g", "1c-3g", "48", "188Gi", "196Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					GeneralKey:        gKey,
					Key:               "nvidia-a10g",
					ProfileFlavorSpec: "48c-188g-196g-4d",
					ProfileQueueSpec:  "12c-47g-1d",
					ProfileCohortSpec: "12c-47g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						a10gPfx + ".profile-queue": "12c-47g-1d",
					},
					Tolerations:     expectedToleration,
					Acceleratable:   true,
					CPUManufacturer: "amd",
					CPUFamily:       "25",
					CPUID:           "1",
					Manufacturer:    "nvidia",
					Product:         "A10G",
					Memory:          "23Gi",
					Cores:           "0",
					Accelerator:     "4",
					CPU:             "48",
					RAM:             "188Gi",
					LocalStorage:    "196Gi",
				},
			},
		},
		{
			// Synthetic hybrid node (NVIDIA T4 + AMD MI300X) — exercises
			// the device-key loop with multiple manufacturers. Loop
			// iteration order is map-dependent, so we rely on
			// ElementsMatch.
			//
			// Also covers the "full" device-label shape (driver-version,
			// runtime-version, family, compute-capability) for the T4
			// to exercise every NodeResourceFlavor field.
			name: "hybrid NVIDIA T4 + AMD MI300X",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey:    "true",
					nvPfx:                         "true",
					nvPfx + ".driver-version":     "580.126.09",
					nvPfx + ".runtime-version":    "13.0",
					t4Pfx:                         "true",
					t4Pfx + ".product":            "Tesla-T4",
					t4Pfx + ".memory":             "15Gi",
					t4Pfx + ".cores":              "2560",
					t4Pfx + ".family":             "Turing",
					t4Pfx + ".compute-capability": "7.5",
					t4Pfx + ".accelerators":       "1",
					amdPfx:                        "true",
					mi300xPfx:                     "true",
					mi300xPfx + ".product":        "MI300X",
					mi300xPfx + ".memory":         "192Gi",
					mi300xPfx + ".cores":          "0",
					mi300xPfx + ".accelerators":   "2",
					t4Pfx + ".cpu":                "32",
					t4Pfx + ".ram":                "128Gi",
					t4Pfx + ".local-storage":      "200Gi",
					t4Pfx + ".profile-flavor":     "32c-128g-200g-1d",
					t4Pfx + ".profile-queue":      "32c-128g-1d",
					t4Pfx + ".profile-cohort":     "32c-128g-1d",
					mi300xPfx + ".cpu":            "32",
					mi300xPfx + ".ram":            "128Gi",
					mi300xPfx + ".local-storage":  "200Gi",
					mi300xPfx + ".profile-flavor": "32c-128g-200g-2d",
					mi300xPfx + ".profile-queue":  "16c-64g-1d",
					mi300xPfx + ".profile-cohort": "16c-64g-1d",
				},
				generalLabels("32", "128Gi", "200Gi", "32c-128g-200g", "1c-4g"),
			),
			wantCPU: cpuFlavor("32c-128g-200g", "1c-4g", "32", "128Gi", "200Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					GeneralKey:        gKey,
					Key:               "nvidia-tesla-t4",
					ProfileFlavorSpec: "32c-128g-200g-1d",
					ProfileQueueSpec:  "32c-128g-1d",
					ProfileCohortSpec: "32c-128g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						t4Pfx + ".profile-queue":   "32c-128g-1d",
					},
					Tolerations:       expectedToleration,
					Acceleratable:     true,
					CPUManufacturer:   "amd",
					CPUFamily:         "25",
					CPUID:             "1",
					Manufacturer:      "nvidia",
					Product:           "Tesla-T4",
					Memory:            "15Gi",
					Cores:             "2560",
					Family:            "Turing",
					ComputeCapability: "7.5",
					Accelerator:       "1",
					CPU:               "32",
					RAM:               "128Gi",
					LocalStorage:      "200Gi",
				},
				{
					GeneralKey:        gKey,
					Key:               "amd-mi300x",
					ProfileFlavorSpec: "32c-128g-200g-2d",
					ProfileQueueSpec:  "16c-64g-1d",
					ProfileCohortSpec: "16c-64g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey:   "true",
						gPfx:                         "true",
						mi300xPfx + ".profile-queue": "16c-64g-1d",
					},
					Tolerations:     expectedToleration,
					Acceleratable:   true,
					CPUManufacturer: "amd",
					CPUFamily:       "25",
					CPUID:           "1",
					Manufacturer:    "amd",
					Product:         "MI300X",
					Memory:          "192Gi",
					Cores:           "0",
					Accelerator:     "2",
					CPU:             "32",
					RAM:             "128Gi",
					LocalStorage:    "200Gi",
				},
			},
		},
		{
			// Capacity-label gate: nil labels — nothing for the function
			// to work with, returns nil.
			name:      "nil labels return no flavors",
			labels:    nil,
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only the general .cpu is missing.
			name: "missing general .cpu returns no flavors",
			labels: func() map[string]string {
				lbs := mergeLabels(
					map[string]string{systemname.ManagedLabelKey: "true"},
					generalLabels("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
				)
				delete(lbs, gPfx+".cpu")
				return lbs
			}(),
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only the general .ram is missing.
			name: "missing general .ram returns no flavors",
			labels: func() map[string]string {
				lbs := mergeLabels(
					map[string]string{systemname.ManagedLabelKey: "true"},
					generalLabels("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
				)
				delete(lbs, gPfx+".ram")
				return lbs
			}(),
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only the general .local-storage is missing.
			name: "missing general .local-storage returns no flavors",
			labels: func() map[string]string {
				lbs := mergeLabels(
					map[string]string{systemname.ManagedLabelKey: "true"},
					generalLabels("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
				)
				delete(lbs, gPfx+".local-storage")
				return lbs
			}(),
			wantEmpty: true,
		},
		{
			// The general key is discovered through the .profile-flavor
			// label — without it the general flavor is not recognized at
			// all.
			name: "missing general .profile-flavor returns no flavors",
			labels: func() map[string]string {
				lbs := mergeLabels(
					map[string]string{systemname.ManagedLabelKey: "true"},
					generalLabels("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
				)
				delete(lbs, gPfx+".profile-flavor")
				return lbs
			}(),
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only the general .profile-cohort is missing.
			name: "missing general .profile-cohort returns no flavors",
			labels: func() map[string]string {
				lbs := mergeLabels(
					map[string]string{systemname.ManagedLabelKey: "true"},
					generalLabels("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
				)
				delete(lbs, gPfx+".profile-cohort")
				return lbs
			}(),
			wantEmpty: true,
		},
		{
			// Device keys without per-device capacity labels emit nothing —
			// and with no general capacity either, the result is empty.
			name: "device labels without general capacity still returns no flavors",
			labels: map[string]string{
				systemname.ManagedLabelKey: "true",
				nvPfx:                      "true",
				t4Pfx:                      "true",
				t4Pfx + ".product":         "Tesla-T4",
				t4Pfx + ".accelerators":    "1",
			},
			wantEmpty: true,
		},
		{
			// Per-device capacity gate: general capacity is complete so
			// the CPU/general flavor is emitted, but the T4's per-device
			// .cpu / .ram / .local-storage / .profile-flavor / .profile-cohort have
			// not been computed yet — the device flavor is skipped rather
			// than emitted with empty fields.
			name: "device skipped when its per-device labels are missing",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey: "true",
					nvPfx:                      "true",
					t4Pfx:                      "true",
					t4Pfx + ".product":         "Tesla-T4",
					t4Pfx + ".memory":          "15Gi",
					t4Pfx + ".cores":           "0",
					t4Pfx + ".accelerators":    "1",
					// No per-device .cpu / .ram / .local-storage / .profile-* → device is skipped.
				},
				generalLabels("4", "16Gi", "96Gi", "4c-16g-96g", "1c-4g"),
			),
			wantCPU:     cpuFlavor("4c-16g-96g", "1c-4g", "4", "16Gi", "96Gi"),
			wantDevices: nil,
		},
		{
			// Per-device gate requires the .accelerators label: even when
			// the rest of the per-device capacity labels are present, a
			// missing .accelerators causes the device to be skipped.
			// Accelerator count is no longer fetched from
			// Status.Allocatable — the label is the sole source.
			name: "device skipped when its .accelerators label is missing",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey: "true",
					nvPfx:                      "true",
					t4Pfx:                      "true",
					t4Pfx + ".product":         "Tesla-T4",
					t4Pfx + ".memory":          "15Gi",
					t4Pfx + ".cores":           "0",
					// No .accelerators → device is skipped.
					t4Pfx + ".cpu":            "4",
					t4Pfx + ".ram":            "16Gi",
					t4Pfx + ".local-storage":  "96Gi",
					t4Pfx + ".profile-flavor": "4c-16g-96g-1d",
					t4Pfx + ".profile-queue":  "4c-16g-1d",
					t4Pfx + ".profile-cohort": "4c-16g-1d",
				},
				generalLabels("4", "16Gi", "96Gi", "4c-16g-96g", "1c-4g"),
			),
			wantCPU:     cpuFlavor("4c-16g-96g", "1c-4g", "4", "16Gi", "96Gi"),
			wantDevices: nil,
		},
		{
			// Per-device gate is independent across devices: T4 has full
			// per-device capacity → its flavor is emitted; MI300X has
			// only a .cpu label, no .ram / .local-storage / .profile-flavor /
			// .profile-cohort → it is skipped. Only CPU + T4 appear in the
			// result.
			name: "partial per-device capacity only emits the complete device",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey:  "true",
					nvPfx:                       "true",
					t4Pfx:                       "true",
					t4Pfx + ".product":          "Tesla-T4",
					t4Pfx + ".memory":           "15Gi",
					t4Pfx + ".cores":            "0",
					t4Pfx + ".accelerators":     "1",
					amdPfx:                      "true",
					mi300xPfx:                   "true",
					mi300xPfx + ".product":      "MI300X",
					mi300xPfx + ".memory":       "192Gi",
					mi300xPfx + ".cores":        "0",
					mi300xPfx + ".accelerators": "2",
					t4Pfx + ".cpu":              "32",
					t4Pfx + ".ram":              "128Gi",
					t4Pfx + ".local-storage":    "200Gi",
					t4Pfx + ".profile-flavor":   "32c-128g-200g-1d",
					t4Pfx + ".profile-queue":    "32c-128g-1d",
					t4Pfx + ".profile-cohort":   "32c-128g-1d",
					mi300xPfx + ".cpu":          "32", // remaining per-device labels intentionally absent
				},
				generalLabels("32", "128Gi", "200Gi", "32c-128g-200g", "1c-4g"),
			),
			wantCPU: cpuFlavor("32c-128g-200g", "1c-4g", "32", "128Gi", "200Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					GeneralKey:        gKey,
					Key:               "nvidia-tesla-t4",
					ProfileFlavorSpec: "32c-128g-200g-1d",
					ProfileQueueSpec:  "32c-128g-1d",
					ProfileCohortSpec: "32c-128g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						t4Pfx + ".profile-queue":   "32c-128g-1d",
					},
					Tolerations:     expectedToleration,
					Acceleratable:   true,
					CPUManufacturer: "amd",
					CPUFamily:       "25",
					CPUID:           "1",
					Manufacturer:    "nvidia",
					Product:         "Tesla-T4",
					Memory:          "15Gi",
					Cores:           "0",
					Accelerator:     "1",
					CPU:             "32",
					RAM:             "128Gi",
					LocalStorage:    "200Gi",
				},
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			node := &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name:   "cluster-1-node",
					Labels: cs.labels,
				},
			}
			got := ExtractNodeResourceFlavors(node)
			if cs.wantEmpty {
				assert.Empty(t, got, "expected no flavors")
				return
			}
			if len(got) == 0 {
				t.Fatalf("ExtractNodeResourceFlavors returned no flavors but at least the CPU flavor was expected")
			}
			assert.Equal(t, cs.wantCPU, got[0], "unexpected CPU/general flavor")
			assert.ElementsMatch(t, cs.wantDevices, got[1:], "unexpected device flavors")
		})
	}
}

func TestFormatNodeProfile(t *testing.T) {
	cases := []struct {
		name       string
		generalKey string
		accKey     string
		spec       string
		expected   string
	}{
		{
			name:       "general flavor profile",
			generalKey: "amd-25-1",
			spec:       "16c-32g-88g",
			expected:   "gpustack--amd-25-1-16c-32g-88g",
		},
		{
			name:       "general queue profile",
			generalKey: "generic",
			spec:       "1c-2g",
			expected:   "gpustack--generic-1c-2g",
		},
		{
			name:       "acceleratable flavor profile",
			generalKey: "amd-25-1",
			accKey:     "nvidia-tesla-t4",
			spec:       "4c-16g-88g-1d",
			expected:   "gpustack--amd-25-1-4c-16g-88g--nvidia-tesla-t4-1d",
		},
		{
			name:       "acceleratable queue profile with sliced",
			generalKey: "amd-25-1",
			accKey:     "nvidia-a10g",
			spec:       "12c-47g-1d-8s",
			expected:   "gpustack--amd-25-1-12c-47g--nvidia-a10g-1d-8s",
		},
		{
			name:       "acceleratable cohort profile",
			generalKey: "intel-6-143",
			accKey:     "nvidia-a10g",
			spec:       "12c-47g-1d",
			expected:   "gpustack--intel-6-143-12c-47g--nvidia-a10g-1d",
		},
		{
			name:       "acc key without accelerator segment falls back to general format",
			generalKey: "amd-25-1",
			accKey:     "nvidia-tesla-t4",
			spec:       "4c-16g",
			expected:   "gpustack--amd-25-1-4c-16g",
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := FormatNodeProfile(cs.generalKey, cs.accKey, cs.spec)
			assert.Equal(t, cs.expected, actual, "unexpected profile")
		})
	}
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
			name:    "non-numeric local-storage",
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
