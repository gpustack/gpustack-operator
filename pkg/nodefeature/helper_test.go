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
			// With blending off the CPU identity is ignored entirely; only the
			// manufacturer "generic" and the os/arch tail form the key.
			name:     "disabled blending keys off generic with os/arch",
			node:     newNode(mergeLabels(cpuModelLabels(), osArchLabels), nil),
			expected: "generic-ln-x64",
		},
		{
			// Blending on, but no vendor/name/family — the manufacturer falls
			// back to "generic" and no id prefix is blended in.
			name:        "no cpu information keys off generic",
			withCPUName: true,
			node:        newNode(osArchLabels, nil),
			expected:    "generic-ln-x64",
		},
		{
			// No cpu-name annotation, so the cpu-model family/id labels supply
			// the id prefix — the rare fallback path.
			name:        "cpu-model family/id is the fallback id prefix",
			withCPUName: true,
			node:        newNode(mergeLabels(cpuModelLabels(), osArchLabels), nil),
			expected:    "amd-25-1-ln-x64",
		},
		{
			name:        "arm64 arch abbreviates to a64",
			withCPUName: true,
			node: newNode(mergeLabels(cpuModelLabels(), map[string]string{
				core.LabelOSStable:   "linux",
				core.LabelArchStable: "arm64",
			}), nil),
			expected: "amd-25-1-ln-a64",
		},
		{
			name:        "unmapped os/arch pass through unchanged",
			withCPUName: true,
			node: newNode(mergeLabels(cpuModelLabels(), map[string]string{
				core.LabelOSStable:   "js",
				core.LabelArchStable: "386",
			}), nil),
			expected: "amd-25-1-js-386",
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
			expected: "amd-epyc-7763-ln-x64",
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
			expected: "amd-25-1-ln-x64",
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
			expected: "intel-xeon-platinum-8358-ln-x64",
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
			expected: "generic-kunpeng-920-ln-x64",
		},
		{
			// Vendor is known but the family/id pair is incomplete and there is
			// no cpu-name — no id prefix is blended, so the key is just
			// "${manufacturer}-${os}-${arch}".
			name:        "vendor without family keys off manufacturer and os/arch",
			withCPUName: true,
			node: newNode(mergeLabels(osArchLabels, map[string]string{
				_NFDCPUModelVendorIDLabelKey: "AMD",
				_NFDCPUModelIDLabelKey:       "1",
			}), nil),
			expected: "amd-ln-x64",
		},
		{
			name:        "vendor without id keys off manufacturer and os/arch",
			withCPUName: true,
			node: newNode(mergeLabels(osArchLabels, map[string]string{
				_NFDCPUModelVendorIDLabelKey: "AMD",
				_NFDCPUModelFamilyLabelKey:   "25",
			}), nil),
			expected: "amd-ln-x64",
		},
		{
			// The cpu-name sanitizes to empty, so no id prefix is blended; the
			// known vendor still leads the key.
			name:        "cpu-name sanitized to empty keys off manufacturer and os/arch",
			withCPUName: true,
			node: newNode(
				mergeLabels(osArchLabels, map[string]string{
					_NFDCPUModelVendorIDLabelKey: "AMD",
				}),
				map[string]string{
					FeatureLabelPrefix + "cpu-name": "(TM)",
				},
			),
			expected: "amd-ln-x64",
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
	// Gi; storage rounds down).
	//
	// All cluster-1 nodes carry the NFD cpu-model labels of an AMD family 25
	// model 1 CPU and are linux/amd64, so with the CPU-name blending enabled
	// below their general(CPU) node key is "amd-25-1-ln-x64". The generic
	// fallback of the key derivation itself is covered by
	// Test_extractGeneralNodeKey.
	generalNodeKeyWithCPUName = true

	gPfx := GeneralFeatureLabelPrefix + "amd-25-1-ln-x64"
	genericPfx := GeneralFeatureLabelPrefix + "generic-ln-x64"
	t4Pfx := AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"
	a10gPfx := AcceleratableFeatureLabelPrefix + "nvidia-a10g"

	// ConstructNodeCapacityLabels reads accelerator count from the
	// ${aKey}.count label (emitted by applyAcceleratorLabels), not from
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
		// os/arch are well-known labels always present on a node; they are part
		// of the general node key, so every node here is a linux/amd64 box and
		// its general key trails with "-ln-x64".
		labels[core.LabelOSStable] = "linux"
		labels[core.LabelArchStable] = "amd64"
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
		aKey := AcceleratableFeatureLabelPrefix + "nvidia-" + id
		return map[string]string{
			systemname.ManagedLabelKey:                 "true",
			AcceleratableFeatureLabelPrefix + "nvidia": "true",
			aKey:              "true",
			aKey + ".product": product,
			aKey + ".memory":  memory,
			aKey + ".cores":   "0",
			aKey + ".count":   accelerators,
		}
	}

	// generalExpected is the general(CPU) view of an amd-25-1 node.
	generalExpected := func(cpu, ram, stg, flavor, unit string) map[string]string {
		return map[string]string{
			systemname.ManagedLabelKey:        "true",
			GeneralFeatureLabelPrefix + "amd": "true",
			gPfx:                              "true",
			gPfx + ".cpu":                     cpu,
			gPfx + ".ram":                     ram,
			gPfx + ".storage":                 stg,
			gPfx + ".z-flavor":                flavor,
			gPfx + ".z-queue":                 unit,
			gPfx + ".z-cohort":                unit,
		}
	}

	// deviceExpected is the acceleratable(device) view added by the device loop.
	deviceExpected := func(pfx, cpu, ram, stg, flavor, queue, cohort string) map[string]string {
		return map[string]string{
			pfx + ".cpu":      cpu,
			pfx + ".ram":      ram,
			pfx + ".storage":  stg,
			pfx + ".z-flavor": flavor,
			pfx + ".z-queue":  queue,
			pfx + ".z-cohort": cohort,
		}
	}

	// withoutZ strips the .z-* labels from an expected view, mirroring an
	// explicit-zero opt-out.
	withoutZ := func(m map[string]string, pfx string) map[string]string {
		delete(m, pfx+".z-flavor")
		delete(m, pfx+".z-queue")
		delete(m, pfx+".z-cohort")
		return m
	}

	// genericExpected is the general(CPU) view keyed off "generic-ln-x64".
	genericExpected := func(cpu, ram, stg, flavor, unit string) map[string]string {
		return map[string]string{
			systemname.ManagedLabelKey:            "true",
			GeneralFeatureLabelPrefix + "generic": "true",
			genericPfx:                            "true",
			genericPfx + ".cpu":                   cpu,
			genericPfx + ".ram":                   ram,
			genericPfx + ".storage":               stg,
			genericPfx + ".z-flavor":              flavor,
			genericPfx + ".z-queue":               unit,
			genericPfx + ".z-cohort":              unit,
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
			// No NFD cpu information at all — the general key falls back to
			// "generic" and the general view is emitted under it.
			name:     "cpu-only node without cpu information falls back to generic view",
			node:     newNode("cluster-1-node-1", "16", "31Gi", "97Gi", nil),
			expected: genericExpected("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
		},
		{
			name: "cpu-only node with Intel cpu-model labels",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi", map[string]string{
				_NFDCPUModelVendorIDLabelKey: "Intel",
				_NFDCPUModelFamilyLabelKey:   "6",
				_NFDCPUModelIDLabelKey:       "143",
			}),
			expected: map[string]string{
				systemname.ManagedLabelKey:                                "true",
				GeneralFeatureLabelPrefix + "intel":                       "true",
				GeneralFeatureLabelPrefix + "intel-6-143-ln-x64":          "true",
				GeneralFeatureLabelPrefix + "intel-6-143-ln-x64.cpu":      "16",
				GeneralFeatureLabelPrefix + "intel-6-143-ln-x64.ram":      "32Gi",
				GeneralFeatureLabelPrefix + "intel-6-143-ln-x64.storage":  "96Gi",
				GeneralFeatureLabelPrefix + "intel-6-143-ln-x64.z-flavor": "16c-32g-96g",
				GeneralFeatureLabelPrefix + "intel-6-143-ln-x64.z-queue":  "1c-2g",
				GeneralFeatureLabelPrefix + "intel-6-143-ln-x64.z-cohort": "1c-2g",
			},
		},
		{
			// The cpu-name annotation and the os/arch labels reshape the
			// general key, all general labels move under the new key.
			name: "cpu-name annotation and os/arch labels lead the general key",
			node: func() *core.Node {
				nd := newNode("cluster-1-node-1", "16", "31Gi", "97Gi",
					mergeLabels(cpuModelLabels(), map[string]string{
						core.LabelOSStable:   "linux",
						core.LabelArchStable: "amd64",
					}))
				nd.Annotations = map[string]string{
					FeatureLabelPrefix + "cpu-name": "AMD EPYC 7763 64-Core Processor",
				}
				return nd
			}(),
			expected: func() map[string]string {
				pfx := GeneralFeatureLabelPrefix + "amd-epyc-7763-ln-x64"
				return map[string]string{
					systemname.ManagedLabelKey:        "true",
					GeneralFeatureLabelPrefix + "amd": "true",
					pfx:                               "true",
					pfx + ".cpu":                      "16",
					pfx + ".ram":                      "32Gi",
					pfx + ".storage":                  "96Gi",
					pfx + ".z-flavor":                 "16c-32g-96g",
					pfx + ".z-queue":                  "1c-2g",
					pfx + ".z-cohort":                 "1c-2g",
				}
			}(),
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
			// across both devices, so the per-device z-cohort unit is
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
			// is integer-divided: 188/48 = 3. storage 196Gi is even,
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
			// User-supplied ${aKey}.sliced.partitions=8 appends "-8s"
			// to z-flavor and z-queue. z-cohort is the
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
			// silently ignored — z-flavor stays without the suffix.
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
					gPfx + ".cpu":     "8",
					gPfx + ".ram":     "16Gi",
					gPfx + ".storage": "50Gi",
				}),
			),
			expected: generalExpected("8", "16Gi", "50Gi", "8c-16g-50g", "1c-2g"),
		},
		{
			// No Status.Capacity — exercises fallback defaults: cpu→1,
			// ram→cpu (so 1Gi), storage→15Gi when cpu==1.
			name: "missing Status.Capacity falls back to defaults",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: "blank-node",
					Labels: mergeLabels(cpuModelLabels(), map[string]string{
						core.LabelOSStable:   "linux",
						core.LabelArchStable: "amd64",
					}),
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
			name:     "missing storage falls back to 15Gi * cpuC",
			node:     newNode("cluster-1-node-1", "16", "31Gi", "0", cpuModelLabels()),
			expected: generalExpected("16", "32Gi", "240Gi", "16c-32g-240g", "1c-2g"),
		},
		{
			// Per-device storage now scales with accC, not cpuC. With
			// ephemeral-storage absent and accC=2, device storage falls
			// back to 15Gi * accC = 30Gi — independent of general's
			// 15Gi * cpuC = 120Gi. Confirms the device loop no longer
			// inherits stgC from the general view.
			name: "missing storage on accelerated node uses 15Gi * accC for device",
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
			// Accelerated feature label is set but the ${aKey}.count
			// label is absent — the per-device loop reads accelerator count
			// strictly from that label, so the device is skipped entirely
			// (no fallback). Only the general capacity labels are emitted.
			name: "accelerated feature label without .count is skipped",
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
			// general .ram, .z-flavor, .z-queue and
			// .z-cohort all reflect generalRamC = 2*cpuC = 32Gi,
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
			// z-flavor=16c-32g-96g, z-queue=1c-2g. Demonstrates
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
			// device .ram=2Gi, z-queue cpuUnit=8/2=4 and
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
			// device .ram=64Gi, z-queue cpuUnit=8/2=4 / ramUnit=64/2=32.
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
		{
			// An explicit general .cpu=0 label opts the general view out of
			// Kueue exposure: the zero is echoed as-is (keeping the opt-out
			// sticky across reconciles) and no .z-* labels are emitted, so
			// no general flavor/queue is built downstream.
			name: "explicit general .cpu=0 opts the general view out",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), map[string]string{
					gPfx + ".cpu": "0",
				})),
			expected: withoutZ(generalExpected("0", "32Gi", "96Gi", "", ""), gPfx),
		},
		{
			// Same opt-out via an explicit general .ram=0 label.
			name: "explicit general .ram=0 opts the general view out",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), map[string]string{
					gPfx + ".ram": "0",
				})),
			expected: withoutZ(generalExpected("16", "0", "96Gi", "", ""), gPfx),
		},
		{
			// Same opt-out via an explicit general .storage=0 label.
			name: "explicit general .storage=0 opts the general view out",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), map[string]string{
					gPfx + ".storage": "0",
				})),
			expected: withoutZ(generalExpected("16", "32Gi", "0", "", ""), gPfx),
		},
		{
			// An explicit device .cpu=0 label opts only that acceleratable
			// view out — the general view keeps its .z-* labels.
			name: "explicit device .cpu=0 opts the device view out",
			node: newNode(
				"cluster-1-node-2", "4", "15Gi", "97Gi",
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "1"), map[string]string{
					t4Pfx + ".cpu": "0",
				}),
			),
			expected: mergeLabels(
				generalExpected("4", "16Gi", "96Gi", "4c-16g-96g", "1c-4g"),
				withoutZ(deviceExpected(t4Pfx, "0", "16Gi", "96Gi", "", "", ""), t4Pfx),
			),
		},
		{
			// Conversely, opting out the general view leaves the device view
			// fully exposed.
			name: "explicit general .cpu=0 leaves the device view exposed",
			node: newNode(
				"cluster-1-node-2", "4", "15Gi", "97Gi",
				mergeLabels(cpuModelLabels(), deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "1"), map[string]string{
					gPfx + ".cpu": "0",
				}),
			),
			expected: mergeLabels(
				withoutZ(generalExpected("0", "16Gi", "96Gi", "", ""), gPfx),
				deviceExpected(t4Pfx, "4", "16Gi", "96Gi", "4c-16g-96g-1d", "4c-16g-1d", "4c-16g-1d"),
			),
		},
		{
			// An unparsable capacity label is not an opt-out — it falls back
			// to Status.Capacity as before.
			name: "unparsable general .cpu label falls back to Status.Capacity",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi",
				mergeLabels(cpuModelLabels(), map[string]string{
					gPfx + ".cpu": "garbage",
				})),
			expected: generalExpected("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
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
	//   ndfs[0]  = the CPU/general flavor, emitted only when the general(CPU)
	//              node key is derivable and all six general capacity labels
	//              (cpu, ram, storage, z-flavor, z-queue,
	//              z-cohort) are present under it.
	//   ndfs[1:] = one flavor per accelerated node key for which the
	//              per-device .accelerators, .cpu, .ram, .storage,
	//              .z-flavor, .z-queue, and .z-cohort
	//              labels are all present. The Accelerator field is read
	//              directly from the .accelerators label. Each device
	//              flavor is paired with the node's general(CPU) key and
	//              pins it via NodeLabels.
	//
	// The accelerated-key loop iterates a Go map, so the device-flavor
	// order is non-deterministic. We compare the head directly and the
	// tail as a multiset via ElementsMatch.

	// All capacity labels below assume the CPU-name blending is enabled and the
	// nodes are linux/amd64, so the general(CPU) node key derives from the NFD
	// cpu-model labels plus the os/arch tail. The generic fallback of the key
	// derivation itself is covered by Test_extractGeneralNodeKey.
	generalNodeKeyWithCPUName = true

	gKey := "amd-25-1-ln-x64"
	gPfx := GeneralFeatureLabelPrefix + gKey
	genericPfx := GeneralFeatureLabelPrefix + "generic-ln-x64"
	nvPfx := AcceleratableFeatureLabelPrefix + "nvidia"
	t4Pfx := AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"
	a10gPfx := AcceleratableFeatureLabelPrefix + "nvidia-a10g"
	amdPfx := AcceleratableFeatureLabelPrefix + "amd"
	mi300xPfx := AcceleratableFeatureLabelPrefix + "amd-mi300x"

	expectedToleration := []core.Toleration{
		{Operator: core.TolerationOpExists},
	}

	// generalLabels is the post-ConstructNodeCapacityLabels general view,
	// together with the NFD cpu-model labels that derive the "amd-25-1" key.
	generalLabels := func(cpu, ram, stg, flavor, unit string) map[string]string {
		return mergeLabels(cpuModelLabels(), map[string]string{
			gPfx + ".cpu":      cpu,
			gPfx + ".ram":      ram,
			gPfx + ".storage":  stg,
			gPfx + ".z-flavor": flavor,
			gPfx + ".z-queue":  unit,
			gPfx + ".z-cohort": unit,
		})
	}

	// cpuFlavor is the expected general(CPU) flavor of an amd-25-1 node.
	cpuFlavor := func(flavor, unit, cpu, ram, stg string) NodeResourceFlavor {
		return NodeResourceFlavor{
			ProfileCohort: "gpustack--" + gKey + "-" + unit,
			ProfileQueue:  "gpustack--" + gKey + "-" + unit,
			ProfileFlavor: "gpustack--" + gKey + "-" + flavor,
			Manufacturer:  "amd",
			NodeLabels: map[string]string{
				systemname.ManagedLabelKey: "true",
				gPfx + ".z-queue":          unit,
			},
			Tolerations:  expectedToleration,
			CPU:          cpu,
			RAM:          ram,
			LocalStorage: stg,
		}
	}

	cases := []struct {
		name string
		// labels populates node.ObjectMeta.Labels. The per-device
		// Accelerator field sources from the ${aKey}.count
		// label written by applyAcceleratorLabels.
		labels          map[string]string
		wantCPU         NodeResourceFlavor
		wantDevices     []NodeResourceFlavor
		wantEmpty       bool
		wantDevicesOnly bool
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
					t4Pfx + ".count":           "1",
					t4Pfx + ".cpu":             "4",
					t4Pfx + ".ram":             "16Gi",
					t4Pfx + ".storage":         "96Gi",
					t4Pfx + ".z-flavor":        "4c-16g-96g-1d",
					t4Pfx + ".z-queue":         "4c-16g-1d",
					t4Pfx + ".z-cohort":        "4c-16g-1d",
				},
				generalLabels("4", "16Gi", "96Gi", "4c-16g-96g", "1c-4g"),
			),
			wantCPU: cpuFlavor("4c-16g-96g", "1c-4g", "4", "16Gi", "96Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					ProfileCohort: "gpustack--amd-25-1-ln-x64-4c-16g--nvidia-tesla-t4-1d",
					ProfileQueue:  "gpustack--amd-25-1-ln-x64-4c-16g--nvidia-tesla-t4-1d",
					ProfileFlavor: "gpustack--amd-25-1-ln-x64-4c-16g-96g--nvidia-tesla-t4-1d",
					Manufacturer:  "nvidia",
					Acceleratable: true,
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						t4Pfx + ".z-queue":         "4c-16g-1d",
					},
					Tolerations:  expectedToleration,
					Accelerator:  "1",
					CPU:          "4",
					RAM:          "16Gi",
					LocalStorage: "96Gi",
				},
			},
		},
		{
			// 2xT4 — ProfileFlavor differs from node-2 (absolute capacities)
			// but ProfileCohort matches node-2 (same per-device shape:
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
					t4Pfx + ".count":           "2",
					t4Pfx + ".cpu":             "8",
					t4Pfx + ".ram":             "32Gi",
					t4Pfx + ".storage":         "96Gi",
					t4Pfx + ".z-flavor":        "8c-32g-96g-2d",
					t4Pfx + ".z-queue":         "4c-16g-1d",
					t4Pfx + ".z-cohort":        "4c-16g-1d",
				},
				generalLabels("8", "32Gi", "96Gi", "8c-32g-96g", "1c-4g"),
			),
			wantCPU: cpuFlavor("8c-32g-96g", "1c-4g", "8", "32Gi", "96Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					ProfileCohort: "gpustack--amd-25-1-ln-x64-4c-16g--nvidia-tesla-t4-1d", // == node-2's device queue
					ProfileQueue:  "gpustack--amd-25-1-ln-x64-4c-16g--nvidia-tesla-t4-1d",
					ProfileFlavor: "gpustack--amd-25-1-ln-x64-8c-32g-96g--nvidia-tesla-t4-2d",
					Manufacturer:  "nvidia",
					Acceleratable: true,
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						t4Pfx + ".z-queue":         "4c-16g-1d",
					},
					Tolerations:  expectedToleration,
					Accelerator:  "2",
					CPU:          "8",
					RAM:          "32Gi",
					LocalStorage: "96Gi",
				},
			},
		},
		{
			// 4xA10G — 48-cpu / 188Gi-ram totals, integer-divided per-device
			// ram-unit = 188/4 = 47Gi.
			name: "cluster-1/node-5 A10G 4D 48C/188Gi/196Gi",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey: "true",
					core.LabelHostname:         "cluster-1-node-5",
					nvPfx:                      "true",
					a10gPfx:                    "true",
					a10gPfx + ".product":       "A10G",
					a10gPfx + ".memory":        "23Gi",
					a10gPfx + ".cores":         "0",
					a10gPfx + ".count":         "4",
					a10gPfx + ".cpu":           "48",
					a10gPfx + ".ram":           "188Gi",
					a10gPfx + ".storage":       "196Gi",
					a10gPfx + ".z-flavor":      "48c-188g-196g-4d",
					a10gPfx + ".z-queue":       "12c-47g-1d",
					a10gPfx + ".z-cohort":      "12c-47g-1d",
				},
				generalLabels("48", "188Gi", "196Gi", "48c-188g-196g", "1c-3g"),
			),
			wantCPU: cpuFlavor("48c-188g-196g", "1c-3g", "48", "188Gi", "196Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					ProfileCohort: "gpustack--amd-25-1-ln-x64-12c-47g--nvidia-a10g-1d",
					ProfileQueue:  "gpustack--amd-25-1-ln-x64-12c-47g--nvidia-a10g-1d",
					ProfileFlavor: "gpustack--amd-25-1-ln-x64-48c-188g-196g--nvidia-a10g-4d",
					Manufacturer:  "nvidia",
					Acceleratable: true,
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						a10gPfx + ".z-queue":       "12c-47g-1d",
					},
					Tolerations:  expectedToleration,
					Accelerator:  "4",
					CPU:          "48",
					RAM:          "188Gi",
					LocalStorage: "196Gi",
				},
			},
		},
		{
			// Synthetic hybrid node (NVIDIA T4 + AMD MI300X) — exercises
			// the device-key loop with multiple manufacturers. Loop
			// iteration order is map-dependent, so we rely on
			// ElementsMatch.
			name: "hybrid NVIDIA T4 + AMD MI300X",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey: "true",
					nvPfx:                      "true",
					nvPfx + ".driver-version":  "580.126.09",
					nvPfx + ".runtime-version": "13.0",
					t4Pfx:                      "true",
					t4Pfx + ".product":         "Tesla-T4",
					t4Pfx + ".memory":          "15Gi",
					t4Pfx + ".cores":           "2560",
					t4Pfx + ".family":          "Turing",
					t4Pfx + ".comcap":          "7.5",
					t4Pfx + ".count":           "1",
					amdPfx:                     "true",
					mi300xPfx:                  "true",
					mi300xPfx + ".product":     "MI300X",
					mi300xPfx + ".memory":      "192Gi",
					mi300xPfx + ".cores":       "0",
					mi300xPfx + ".count":       "2",
					t4Pfx + ".cpu":             "32",
					t4Pfx + ".ram":             "128Gi",
					t4Pfx + ".storage":         "200Gi",
					t4Pfx + ".z-flavor":        "32c-128g-200g-1d",
					t4Pfx + ".z-queue":         "32c-128g-1d",
					t4Pfx + ".z-cohort":        "32c-128g-1d",
					mi300xPfx + ".cpu":         "32",
					mi300xPfx + ".ram":         "128Gi",
					mi300xPfx + ".storage":     "200Gi",
					mi300xPfx + ".z-flavor":    "32c-128g-200g-2d",
					mi300xPfx + ".z-queue":     "16c-64g-1d",
					mi300xPfx + ".z-cohort":    "16c-64g-1d",
				},
				generalLabels("32", "128Gi", "200Gi", "32c-128g-200g", "1c-4g"),
			),
			wantCPU: cpuFlavor("32c-128g-200g", "1c-4g", "32", "128Gi", "200Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					ProfileCohort: "gpustack--amd-25-1-ln-x64-32c-128g--nvidia-tesla-t4-1d",
					ProfileQueue:  "gpustack--amd-25-1-ln-x64-32c-128g--nvidia-tesla-t4-1d",
					ProfileFlavor: "gpustack--amd-25-1-ln-x64-32c-128g-200g--nvidia-tesla-t4-1d",
					Manufacturer:  "nvidia",
					Acceleratable: true,
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						t4Pfx + ".z-queue":         "32c-128g-1d",
					},
					Tolerations:  expectedToleration,
					Accelerator:  "1",
					CPU:          "32",
					RAM:          "128Gi",
					LocalStorage: "200Gi",
				},
				{
					ProfileCohort: "gpustack--amd-25-1-ln-x64-16c-64g--amd-mi300x-1d",
					ProfileQueue:  "gpustack--amd-25-1-ln-x64-16c-64g--amd-mi300x-1d",
					ProfileFlavor: "gpustack--amd-25-1-ln-x64-32c-128g-200g--amd-mi300x-2d",
					Manufacturer:  "amd",
					Acceleratable: true,
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						mi300xPfx + ".z-queue":     "16c-64g-1d",
					},
					Tolerations:  expectedToleration,
					Accelerator:  "2",
					CPU:          "32",
					RAM:          "128Gi",
					LocalStorage: "200Gi",
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
			// Capacity-label gate: only the general .storage is missing.
			name: "missing general .storage returns no flavors",
			labels: func() map[string]string {
				lbs := mergeLabels(
					map[string]string{systemname.ManagedLabelKey: "true"},
					generalLabels("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
				)
				delete(lbs, gPfx+".storage")
				return lbs
			}(),
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only the general .z-flavor is missing.
			name: "missing general .z-flavor returns no flavors",
			labels: func() map[string]string {
				lbs := mergeLabels(
					map[string]string{systemname.ManagedLabelKey: "true"},
					generalLabels("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
				)
				delete(lbs, gPfx+".z-flavor")
				return lbs
			}(),
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only the general .z-cohort is missing.
			name: "missing general .z-cohort returns no flavors",
			labels: func() map[string]string {
				lbs := mergeLabels(
					map[string]string{systemname.ManagedLabelKey: "true"},
					generalLabels("16", "32Gi", "96Gi", "16c-32g-96g", "1c-2g"),
				)
				delete(lbs, gPfx+".z-cohort")
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
				t4Pfx + ".count":           "1",
			},
			wantEmpty: true,
		},
		{
			// Per-device capacity gate: general capacity is complete so
			// the CPU/general flavor is emitted, but the T4's per-device
			// .cpu / .ram / .storage / .z-flavor / .z-cohort have
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
					t4Pfx + ".count":           "1",
					// No per-device .cpu / .ram / .storage / .profile-* → device is skipped.
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
					t4Pfx + ".cpu":      "4",
					t4Pfx + ".ram":      "16Gi",
					t4Pfx + ".storage":  "96Gi",
					t4Pfx + ".z-flavor": "4c-16g-96g-1d",
					t4Pfx + ".z-queue":  "4c-16g-1d",
					t4Pfx + ".z-cohort": "4c-16g-1d",
				},
				generalLabels("4", "16Gi", "96Gi", "4c-16g-96g", "1c-4g"),
			),
			wantCPU:     cpuFlavor("4c-16g-96g", "1c-4g", "4", "16Gi", "96Gi"),
			wantDevices: nil,
		},
		{
			// Per-device gate is independent across devices: T4 has full
			// per-device capacity → its flavor is emitted; MI300X has
			// only a .cpu label, no .ram / .storage / .z-flavor /
			// .z-cohort → it is skipped. Only CPU + T4 appear in the
			// result.
			name: "partial per-device capacity only emits the complete device",
			labels: mergeLabels(
				map[string]string{
					systemname.ManagedLabelKey: "true",
					nvPfx:                      "true",
					t4Pfx:                      "true",
					t4Pfx + ".product":         "Tesla-T4",
					t4Pfx + ".memory":          "15Gi",
					t4Pfx + ".cores":           "0",
					t4Pfx + ".count":           "1",
					amdPfx:                     "true",
					mi300xPfx:                  "true",
					mi300xPfx + ".product":     "MI300X",
					mi300xPfx + ".memory":      "192Gi",
					mi300xPfx + ".cores":       "0",
					mi300xPfx + ".count":       "2",
					t4Pfx + ".cpu":             "32",
					t4Pfx + ".ram":             "128Gi",
					t4Pfx + ".storage":         "200Gi",
					t4Pfx + ".z-flavor":        "32c-128g-200g-1d",
					t4Pfx + ".z-queue":         "32c-128g-1d",
					t4Pfx + ".z-cohort":        "32c-128g-1d",
					mi300xPfx + ".cpu":         "32", // remaining per-device labels intentionally absent
				},
				generalLabels("32", "128Gi", "200Gi", "32c-128g-200g", "1c-4g"),
			),
			wantCPU: cpuFlavor("32c-128g-200g", "1c-4g", "32", "128Gi", "200Gi"),
			wantDevices: []NodeResourceFlavor{
				{
					ProfileCohort: "gpustack--amd-25-1-ln-x64-32c-128g--nvidia-tesla-t4-1d",
					ProfileQueue:  "gpustack--amd-25-1-ln-x64-32c-128g--nvidia-tesla-t4-1d",
					ProfileFlavor: "gpustack--amd-25-1-ln-x64-32c-128g-200g--nvidia-tesla-t4-1d",
					Manufacturer:  "nvidia",
					Acceleratable: true,
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						gPfx:                       "true",
						t4Pfx + ".z-queue":         "32c-128g-1d",
					},
					Tolerations:  expectedToleration,
					Accelerator:  "1",
					CPU:          "32",
					RAM:          "128Gi",
					LocalStorage: "200Gi",
				},
			},
		},
		{
			// Without any NFD cpu information the general key falls back to
			// "generic": the general flavor is skipped (no generic-prefixed
			// capacity labels) but the device flavor pairs with the "generic"
			// segment and pins the generic general(CPU) identity.
			name: "device pairs with generic when cpu information is missing",
			labels: map[string]string{
				systemname.ManagedLabelKey: "true",
				nvPfx:                      "true",
				t4Pfx:                      "true",
				t4Pfx + ".product":         "Tesla-T4",
				t4Pfx + ".memory":          "15Gi",
				t4Pfx + ".cores":           "0",
				t4Pfx + ".count":           "1",
				t4Pfx + ".cpu":             "4",
				t4Pfx + ".ram":             "16Gi",
				t4Pfx + ".storage":         "96Gi",
				t4Pfx + ".z-flavor":        "4c-16g-96g-1d",
				t4Pfx + ".z-queue":         "4c-16g-1d",
				t4Pfx + ".z-cohort":        "4c-16g-1d",
			},
			wantDevicesOnly: true,
			wantDevices: []NodeResourceFlavor{
				{
					ProfileCohort: "gpustack--generic-ln-x64-4c-16g--nvidia-tesla-t4-1d",
					ProfileQueue:  "gpustack--generic-ln-x64-4c-16g--nvidia-tesla-t4-1d",
					ProfileFlavor: "gpustack--generic-ln-x64-4c-16g-96g--nvidia-tesla-t4-1d",
					Manufacturer:  "nvidia",
					Acceleratable: true,
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey: "true",
						genericPfx:                 "true",
						t4Pfx + ".z-queue":         "4c-16g-1d",
					},
					Tolerations:  expectedToleration,
					Accelerator:  "1",
					CPU:          "4",
					RAM:          "16Gi",
					LocalStorage: "96Gi",
				},
			},
		},
		{
			// A node whose general key fell back to "generic" (no usable
			// cpu information) emits the general flavor from the
			// generic-prefixed capacity labels.
			name: "general flavor keys off generic when cpu information is missing",
			labels: map[string]string{
				systemname.ManagedLabelKey: "true",
				core.LabelHostname:         "cluster-1-node-1",
				genericPfx:                 "true",
				genericPfx + ".cpu":        "16",
				genericPfx + ".ram":        "32Gi",
				genericPfx + ".storage":    "96Gi",
				genericPfx + ".z-flavor":   "16c-32g-96g",
				genericPfx + ".z-queue":    "1c-2g",
				genericPfx + ".z-cohort":   "1c-2g",
			},
			wantCPU: NodeResourceFlavor{
				ProfileCohort: "gpustack--generic-ln-x64-1c-2g",
				ProfileQueue:  "gpustack--generic-ln-x64-1c-2g",
				ProfileFlavor: "gpustack--generic-ln-x64-16c-32g-96g",
				Manufacturer:  GeneralManufacturerGeneric,
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey: "true",
					genericPfx + ".z-queue":    "1c-2g",
				},
				Tolerations:  expectedToleration,
				CPU:          "16",
				RAM:          "32Gi",
				LocalStorage: "96Gi",
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			// os/arch are part of the general node key; add them to every
			// non-nil fixture so the key trails with "-ln-x64". The nil-labels
			// case is left untouched to exercise the empty-labels guard.
			if cs.labels != nil {
				cs.labels[core.LabelOSStable] = "linux"
				cs.labels[core.LabelArchStable] = "amd64"
			}
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
			if cs.wantDevicesOnly {
				assert.ElementsMatch(t, cs.wantDevices, got, "unexpected device flavors")
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

func Test_extractNodeQueue(t *testing.T) {
	t4Pfx := AcceleratableFeatureLabelPrefix + "nvidia-tesla-t4"

	cpuAnnotations := map[string]string{
		FeatureLabelPrefix + "cpu-name":             "AMD EPYC 7763 64-Core Processor",
		FeatureLabelPrefix + "cpu-family":           "25",
		FeatureLabelPrefix + "cpu-physical-cores":   "64",
		FeatureLabelPrefix + "cpu-threads-per-core": "2",
		FeatureLabelPrefix + "cpu-logical-cores":    "128",
		FeatureLabelPrefix + "cpu-stepping":         "1",
		FeatureLabelPrefix + "cpu-cache-line":       "64",
		FeatureLabelPrefix + "cpu-hz":               "2450000000",
		FeatureLabelPrefix + "cpu-boost-freq":       "3500000000",
		FeatureLabelPrefix + "cpu-cache-l1i":        "32768",
		FeatureLabelPrefix + "cpu-cache-l1d":        "32768",
		FeatureLabelPrefix + "cpu-cache-l2":         "524288",
		FeatureLabelPrefix + "cpu-cache-l3":         "33554432",
	}

	fullGeneralDetail := NodeResourceFlavorCPU{
		PhysicalCores:          "64",
		ThreadsPerPhysicalCore: "2",
		LogicalCores:           "128",
		Stepping:               "1",
		CacheLine:              "64",
		ClockSpeed:             "2450000000",
		MaxClockSpeed:          "3500000000",
		Cache: NodeResourceFlavorCPUCache{
			L1I: "32768",
			L1D: "32768",
			L2:  "524288",
			L3:  "33554432",
		},
	}

	newNode := func(labels, annotations map[string]string) *core.Node {
		return &core.Node{
			ObjectMeta: meta.ObjectMeta{
				Labels:      labels,
				Annotations: annotations,
			},
		}
	}

	cases := []struct {
		name        string
		withCPUName bool
		node        *core.Node
		accKey      string
		expected    NodeQueue
		wantOK      bool
	}{
		{
			name:        "general(CPU) queue",
			withCPUName: true,
			node: newNode(
				map[string]string{
					core.LabelOSStable:           "linux",
					core.LabelArchStable:         "amd64",
					_NFDCPUModelVendorIDLabelKey: "AMD",
				},
				cpuAnnotations,
			),
			wantOK: true,
			expected: NodeQueue{
				Product:               "AMD EPYC 7763 64-Core Processor",
				Family:                "25",
				OS:                    "linux",
				Arch:                  "amd64",
				NodeResourceFlavorCPU: fullGeneralDetail,
			},
		},
		{
			// With the CPU-name blending disabled the general queue still
			// records the node's os/arch (they are part of the generic key, so
			// every pooled node shares them) but omits the CPU identity and
			// details, which would be misleading across heterogeneous CPUs.
			name: "general(CPU) queue without cpu-name blending records only os/arch",
			node: newNode(
				map[string]string{
					core.LabelOSStable:           "linux",
					core.LabelArchStable:         "amd64",
					_NFDCPUModelVendorIDLabelKey: "AMD",
				},
				cpuAnnotations,
			),
			wantOK: true,
			expected: NodeQueue{
				OS:   "linux",
				Arch: "amd64",
			},
		},
		{
			name:        "acceleratable queue",
			withCPUName: true,
			node: newNode(
				map[string]string{
					core.LabelOSStable:           "linux",
					core.LabelArchStable:         "amd64",
					_NFDCPUModelVendorIDLabelKey: "AMD",
					t4Pfx:                        "true",
					t4Pfx + ".product":           "Tesla-T4",
					t4Pfx + ".family":            "Turing",
					t4Pfx + ".memory":            "15Gi",
					t4Pfx + ".cores":             "2560",
					t4Pfx + ".comcap":            "7.5",
				},
				cpuAnnotations,
			),
			accKey: "nvidia-tesla-t4",
			wantOK: true,
			expected: NodeQueue{
				Product: "Tesla-T4",
				Family:  "Turing",
				OS:      "linux",
				Arch:    "amd64",
				NodeResourceFlavorAccelerator: NodeResourceFlavorAccelerator{
					Memory:            "15Gi",
					Cores:             "2560",
					ComputeCapability: "7.5",
					CPU: NodeResourceFlavorAcceleratorCPU{
						Manufacturer:          "amd",
						NodeResourceFlavorCPU: fullGeneralDetail,
						Product:               "AMD EPYC 7763 64-Core Processor",
						Family:                "25",
					},
				},
			},
		},
		{
			// With the CPU-name blending disabled the acceleratable queue still
			// carries the device fields and the node's os/arch, but omits the
			// paired CPU.
			name: "acceleratable queue without cpu-name blending omits the paired cpu",
			node: newNode(
				map[string]string{
					core.LabelOSStable:           "linux",
					core.LabelArchStable:         "amd64",
					_NFDCPUModelVendorIDLabelKey: "AMD",
					t4Pfx:                        "true",
					t4Pfx + ".product":           "Tesla-T4",
					t4Pfx + ".family":            "Turing",
					t4Pfx + ".memory":            "15Gi",
					t4Pfx + ".cores":             "2560",
					t4Pfx + ".comcap":            "7.5",
				},
				cpuAnnotations,
			),
			accKey: "nvidia-tesla-t4",
			wantOK: true,
			expected: NodeQueue{
				Product: "Tesla-T4",
				Family:  "Turing",
				OS:      "linux",
				Arch:    "amd64",
				NodeResourceFlavorAccelerator: NodeResourceFlavorAccelerator{
					Memory:            "15Gi",
					Cores:             "2560",
					ComputeCapability: "7.5",
				},
			},
		},
		{
			name:        "unresolved cpu annotations are treated as empty",
			withCPUName: true,
			node: newNode(
				nil,
				map[string]string{
					FeatureLabelPrefix + "cpu-name":           "@cpu.model.name",
					FeatureLabelPrefix + "cpu-family":         "@cpu.model.family",
					FeatureLabelPrefix + "cpu-physical-cores": "@cpu.model.physical_cores",
				},
			),
			wantOK:   true,
			expected: NodeQueue{},
		},
		{
			name:   "unlabeled acceleratable node key",
			node:   newNode(nil, cpuAnnotations),
			accKey: "nvidia-tesla-t4",
			wantOK: false,
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual, ok := extractNodeQueue(cs.node, cs.accKey, cs.withCPUName)
			assert.Equal(t, cs.wantOK, ok, "unexpected ok")
			if !ok {
				return
			}
			assert.Equal(t, cs.expected, actual, "unexpected node queue")
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
