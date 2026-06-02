package devicefeature

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/systemname"
)

func TestConstructNodeDeviceLabels(t *testing.T) {
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
				FeatureLabelPrefix + "nvidia":                             "true",
				FeatureLabelPrefix + "nvidia.driver-version":              "580.126.09",
				FeatureLabelPrefix + "nvidia.runtime-version":             "13.0",
				FeatureLabelPrefix + "nvidia-tesla-t4":                    "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product":            "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.memory":             "15Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.cores":              "2560",
				FeatureLabelPrefix + "nvidia-tesla-t4.family":             "Turing",
				FeatureLabelPrefix + "nvidia-tesla-t4.compute-capability": "7.5",
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
				FeatureLabelPrefix + "nvidia":              "true",
				FeatureLabelPrefix + "nvidia-h100":         "true",
				FeatureLabelPrefix + "nvidia-h100.product": "H100",
				FeatureLabelPrefix + "nvidia-h100.memory":  "80Gi",
				FeatureLabelPrefix + "nvidia-h100.cores":   "0",
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
				FeatureLabelPrefix + "nvidia":                  "true",
				FeatureLabelPrefix + "nvidia-tesla-t4":         "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product": "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.memory":  "15Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.cores":   "0",
				FeatureLabelPrefix + "amd":                     "true",
				FeatureLabelPrefix + "amd-mi300x":              "true",
				FeatureLabelPrefix + "amd-mi300x.product":      "MI300X",
				FeatureLabelPrefix + "amd-mi300x.memory":       "192Gi",
				FeatureLabelPrefix + "amd-mi300x.cores":        "0",
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ConstructNodeDeviceLabels(cs.groups)
			assert.Equal(t, cs.expected, actual, "unexpected node labels")
		})
	}
}

func TestExtractNodeKeys(t *testing.T) {
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
						"foo":                         "bar",
						FeatureLabelPrefix + "nvidia": "true",
						FeatureLabelPrefix + "nvidia.driver-version":              "580.126.09",
						FeatureLabelPrefix + "nvidia.runtime-version":             "13.0",
						FeatureLabelPrefix + "nvidia-tesla-t4":                    "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.accelerators":       "4",
						FeatureLabelPrefix + "nvidia-tesla-t4.compute-capability": "7.5",
						FeatureLabelPrefix + "nvidia-tesla-t4.cores":              "2560",
						FeatureLabelPrefix + "nvidia-tesla-t4.family":             "Turing",
						FeatureLabelPrefix + "nvidia-tesla-t4.memory":             "15Gi",
						FeatureLabelPrefix + "nvidia-tesla-t4.product":            "Tesla-T4",
					},
				},
			},
			expected: []string{
				"nvidia-tesla-t4",
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ExtractNodeKeys(cs.node)
			assert.Equal(t, cs.expected, actual, "unexpected node keys")
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

	gpuResource := GetResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)

	newNode := func(name, cpu, mem, storage string, gpu int64, labels map[string]string) *core.Node {
		capacity := core.ResourceList{
			core.ResourceCPU:              resource.MustParse(cpu),
			core.ResourceMemory:           resource.MustParse(mem),
			core.ResourceEphemeralStorage: resource.MustParse(storage),
		}
		if gpu > 0 {
			capacity[gpuResource] = *resource.NewQuantity(gpu, resource.DecimalSI)
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

	// deviceLabels mirrors what ConstructNodeDeviceLabels would have put on
	// the node before ConstructNodeCapacityLabels is invoked.
	deviceLabels := func(id, product, memory, accelerators string) map[string]string {
		ndKey := FeatureLabelPrefix + "nvidia-" + id
		return map[string]string{
			systemname.ManagedLabelKey:    "true",
			FeatureLabelPrefix + "nvidia": "true",
			ndKey:                         "true",
			ndKey + ".product":            product,
			ndKey + ".memory":             memory,
			ndKey + ".cores":              "0",
			ndKey + ".accelerators":       accelerators,
		}
	}

	cases := []struct {
		name     string
		node     *core.Node
		expected map[string]string
	}{
		{
			// 32G RAM is exposed by kubelet as 31Gi (kernel reservation),
			// odd, rounds up to 32Gi. 100G disk is exposed as 97Gi, odd,
			// rounds down to 96Gi.
			name: "cluster-1/node-1 cpu-only 16C/32G/100G",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi", 0, nil),
			expected: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "16",
				FeatureLabelPrefix + "general.ram":            "32Gi",
				FeatureLabelPrefix + "general.local-storage":  "96Gi",
				FeatureLabelPrefix + "general.profile-flavor": "16c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
		},
		{
			// 1xT4 on a small 4C/16G box. Per-device cpu/ram equals the
			// general values since there is exactly one accelerator.
			name: "cluster-1/node-2 T4 1D 4C/16G/100G",
			node: newNode(
				"cluster-1-node-2", "4", "15Gi", "97Gi", 1,
				deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "1"),
			),
			expected: map[string]string{
				systemname.ManagedLabelKey:                            "true",
				FeatureLabelPrefix + "general.cpu":                    "4",
				FeatureLabelPrefix + "general.ram":                    "16Gi",
				FeatureLabelPrefix + "general.local-storage":          "96Gi",
				FeatureLabelPrefix + "general.profile-flavor":         "4c-16g-96g",
				FeatureLabelPrefix + "general.profile-queue":          "1c-4g",
				FeatureLabelPrefix + "general.profile-cohort":         "1c-4g",
				FeatureLabelPrefix + "nvidia-tesla-t4.cpu":            "4",
				FeatureLabelPrefix + "nvidia-tesla-t4.ram":            "16Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.local-storage":  "96Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-flavor": "4c-16g-96g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue":  "4c-16g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-cohort": "4c-16g-1d",
			},
		},
		{
			// Same GPU model as node-2 but on a larger 8C/32G box.
			name: "cluster-1/node-3 T4 1D 8C/32G/100G",
			node: newNode(
				"cluster-1-node-3", "8", "31Gi", "97Gi", 1,
				deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "1"),
			),
			expected: map[string]string{
				systemname.ManagedLabelKey:                            "true",
				FeatureLabelPrefix + "general.cpu":                    "8",
				FeatureLabelPrefix + "general.ram":                    "32Gi",
				FeatureLabelPrefix + "general.local-storage":          "96Gi",
				FeatureLabelPrefix + "general.profile-flavor":         "8c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":          "1c-4g",
				FeatureLabelPrefix + "general.profile-cohort":         "1c-4g",
				FeatureLabelPrefix + "nvidia-tesla-t4.cpu":            "8",
				FeatureLabelPrefix + "nvidia-tesla-t4.ram":            "32Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.local-storage":  "96Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-flavor": "8c-32g-96g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue":  "8c-32g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-cohort": "8c-32g-1d",
			},
		},
		{
			// 2xT4 on the same box as node-3. Per-device cpu/ram is shared
			// across both devices, so the per-device profile-cohort unit is
			// halved by the device count.
			name: "cluster-1/node-4 T4 2D 8C/32G/100G",
			node: newNode(
				"cluster-1-node-4", "8", "31Gi", "97Gi", 2,
				deviceLabels("tesla-t4", "Tesla-T4", "15Gi", "2"),
			),
			expected: map[string]string{
				systemname.ManagedLabelKey:                            "true",
				FeatureLabelPrefix + "general.cpu":                    "8",
				FeatureLabelPrefix + "general.ram":                    "32Gi",
				FeatureLabelPrefix + "general.local-storage":          "96Gi",
				FeatureLabelPrefix + "general.profile-flavor":         "8c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":          "1c-4g",
				FeatureLabelPrefix + "general.profile-cohort":         "1c-4g",
				FeatureLabelPrefix + "nvidia-tesla-t4.cpu":            "8",
				FeatureLabelPrefix + "nvidia-tesla-t4.ram":            "32Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.local-storage":  "96Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-flavor": "8c-32g-96g-2d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue":  "4c-16g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-cohort": "4c-16g-1d",
			},
		},
		{
			// 4xA10G on a large box. 192G → 188Gi (even, stays). ram-unit
			// is integer-divided: 188/48 = 3. local-storage 196Gi is even,
			// stays as-is. Per-device ram-unit: 188/4 = 47.
			name: "cluster-1/node-5 A10G 4D 48C/192G/200G",
			node: newNode(
				"cluster-1-node-5", "48", "188Gi", "196Gi", 4,
				deviceLabels("a10g", "A10G", "23Gi", "4"),
			),
			expected: map[string]string{
				systemname.ManagedLabelKey:                        "true",
				FeatureLabelPrefix + "general.cpu":                "48",
				FeatureLabelPrefix + "general.ram":                "188Gi",
				FeatureLabelPrefix + "general.local-storage":      "196Gi",
				FeatureLabelPrefix + "general.profile-flavor":     "48c-188g-196g",
				FeatureLabelPrefix + "general.profile-queue":      "1c-3g",
				FeatureLabelPrefix + "general.profile-cohort":     "1c-3g",
				FeatureLabelPrefix + "nvidia-a10g.cpu":            "48",
				FeatureLabelPrefix + "nvidia-a10g.ram":            "188Gi",
				FeatureLabelPrefix + "nvidia-a10g.local-storage":  "196Gi",
				FeatureLabelPrefix + "nvidia-a10g.profile-flavor": "48c-188g-196g-4d",
				FeatureLabelPrefix + "nvidia-a10g.profile-queue":  "12c-47g-1d",
				FeatureLabelPrefix + "nvidia-a10g.profile-cohort": "12c-47g-1d",
			},
		},
		{
			// User-supplied ${nodeKey}.sliced.partitions=8 appends "-8s"
			// to profile-flavor and profile-queue. profile-cohort is the
			// cohort-level per-unit view and is never sliced-suffixed —
			// it is the matching key cross-flavor at the cohort level.
			name: "cluster-1/node-5 A10G 4D 48C/192G/200G with sliced.partitions=8",
			node: func() *core.Node {
				lbs := deviceLabels("a10g", "A10G", "23Gi", "4")
				lbs[FeatureLabelPrefix+"nvidia-a10g.sliced.partitions"] = "8"
				return newNode("cluster-1-node-5", "48", "188Gi", "196Gi", 4, lbs)
			}(),
			expected: map[string]string{
				systemname.ManagedLabelKey:                        "true",
				FeatureLabelPrefix + "general.cpu":                "48",
				FeatureLabelPrefix + "general.ram":                "188Gi",
				FeatureLabelPrefix + "general.local-storage":      "196Gi",
				FeatureLabelPrefix + "general.profile-flavor":     "48c-188g-196g",
				FeatureLabelPrefix + "general.profile-queue":      "1c-3g",
				FeatureLabelPrefix + "general.profile-cohort":     "1c-3g",
				FeatureLabelPrefix + "nvidia-a10g.cpu":            "48",
				FeatureLabelPrefix + "nvidia-a10g.ram":            "188Gi",
				FeatureLabelPrefix + "nvidia-a10g.local-storage":  "196Gi",
				FeatureLabelPrefix + "nvidia-a10g.profile-flavor": "48c-188g-196g-4d-8s",
				FeatureLabelPrefix + "nvidia-a10g.profile-queue":  "12c-47g-1d-8s",
				FeatureLabelPrefix + "nvidia-a10g.profile-cohort": "12c-47g-1d",
			},
		},
		{
			// Non-positive / unparsable sliced.partitions values are
			// silently ignored — profile-flavor stays without the suffix.
			name: "cluster-1/node-5 A10G with sliced.partitions=0 yields no suffix",
			node: func() *core.Node {
				lbs := deviceLabels("a10g", "A10G", "23Gi", "4")
				lbs[FeatureLabelPrefix+"nvidia-a10g.sliced.partitions"] = "0"
				return newNode("cluster-1-node-5", "48", "188Gi", "196Gi", 4, lbs)
			}(),
			expected: map[string]string{
				systemname.ManagedLabelKey:                        "true",
				FeatureLabelPrefix + "general.cpu":                "48",
				FeatureLabelPrefix + "general.ram":                "188Gi",
				FeatureLabelPrefix + "general.local-storage":      "196Gi",
				FeatureLabelPrefix + "general.profile-flavor":     "48c-188g-196g",
				FeatureLabelPrefix + "general.profile-queue":      "1c-3g",
				FeatureLabelPrefix + "general.profile-cohort":     "1c-3g",
				FeatureLabelPrefix + "nvidia-a10g.cpu":            "48",
				FeatureLabelPrefix + "nvidia-a10g.ram":            "188Gi",
				FeatureLabelPrefix + "nvidia-a10g.local-storage":  "196Gi",
				FeatureLabelPrefix + "nvidia-a10g.profile-flavor": "48c-188g-196g-4d",
				FeatureLabelPrefix + "nvidia-a10g.profile-queue":  "12c-47g-1d",
				FeatureLabelPrefix + "nvidia-a10g.profile-cohort": "12c-47g-1d",
			},
		},
		{
			// Existing capacity labels take precedence over Status.Capacity
			// — covers idempotent re-runs and operator overrides.
			name: "existing capacity labels override Status.Capacity",
			node: newNode(
				"cluster-1-node-1", "16", "31Gi", "97Gi", 0,
				map[string]string{
					FeatureLabelPrefix + "general.cpu":           "8",
					FeatureLabelPrefix + "general.ram":           "16Gi",
					FeatureLabelPrefix + "general.local-storage": "50Gi",
				},
			),
			expected: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "8",
				FeatureLabelPrefix + "general.ram":            "16Gi",
				FeatureLabelPrefix + "general.local-storage":  "50Gi",
				FeatureLabelPrefix + "general.profile-flavor": "8c-16g-50g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
		},
		{
			// No Status.Capacity — exercises fallback defaults: cpu→1,
			// ram→cpu (so 1Gi), local-storage→15Gi when cpu==1.
			name: "missing Status.Capacity falls back to defaults",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name:   "blank-node",
					Labels: map[string]string{},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{},
				},
			},
			expected: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "1",
				FeatureLabelPrefix + "general.ram":            "1Gi",
				FeatureLabelPrefix + "general.local-storage":  "15Gi",
				FeatureLabelPrefix + "general.profile-flavor": "1c-1g-15g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-1g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-1g",
			},
		},
		{
			// cpu present but ephemeral-storage absent → fallback is
			// 15Gi * cpuC when cpuC > 1.
			name: "missing local-storage falls back to 15Gi * cpuC",
			node: newNode("cluster-1-node-1", "16", "31Gi", "0", 0, nil),
			expected: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "16",
				FeatureLabelPrefix + "general.ram":            "32Gi",
				FeatureLabelPrefix + "general.local-storage":  "240Gi",
				FeatureLabelPrefix + "general.profile-flavor": "16c-32g-240g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
		},
		{
			// Accelerated feature label is set but Status.Capacity has no
			// matching GPU resource — the per-device loop now skips the
			// device entirely (no fallback to accC=1). Only the general
			// capacity labels are emitted.
			name: "accelerated feature label without Status.Capacity GPU is skipped",
			node: newNode(
				"cluster-1-node-2", "4", "15Gi", "97Gi", 0,
				map[string]string{
					systemname.ManagedLabelKey:             "true",
					FeatureLabelPrefix + "nvidia":          "true",
					FeatureLabelPrefix + "nvidia-tesla-t4": "true",
				},
			),
			expected: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "4",
				FeatureLabelPrefix + "general.ram":            "16Gi",
				FeatureLabelPrefix + "general.local-storage":  "96Gi",
				FeatureLabelPrefix + "general.profile-flavor": "4c-16g-96g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-4g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-4g",
			},
		},
		{
			// Existing managed=true on the node is preserved verbatim.
			name: "managed label is true on node",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi", 0,
				map[string]string{
					systemname.ManagedLabelKey: "true",
				}),
			expected: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "16",
				FeatureLabelPrefix + "general.ram":            "32Gi",
				FeatureLabelPrefix + "general.local-storage":  "96Gi",
				FeatureLabelPrefix + "general.profile-flavor": "16c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
		},
		{
			// Existing managed=false on the node overrides the default "true"
			// — capacity labels are still emitted regardless.
			name: "managed label is false on node",
			node: newNode("cluster-1-node-1", "16", "31Gi", "97Gi", 0,
				map[string]string{
					systemname.ManagedLabelKey: "false",
				}),
			expected: map[string]string{
				systemname.ManagedLabelKey:                    "false",
				FeatureLabelPrefix + "general.cpu":            "16",
				FeatureLabelPrefix + "general.ram":            "32Gi",
				FeatureLabelPrefix + "general.local-storage":  "96Gi",
				FeatureLabelPrefix + "general.profile-flavor": "16c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ConstructNodeCapacityLabels(cs.node)
			assert.Equal(t, cs.expected, actual, "unexpected capacity labels")
		})
	}
}

func TestExtractNodeResourceFlavors(t *testing.T) {
	// Output shape:
	//   ndfs[0]  = the CPU/general flavor, emitted only when all five
	//              general capacity labels (cpu, ram, local-storage,
	//              profile-flavor, profile-cohort) are present. Otherwise the
	//              function returns nil.
	//   ndfs[1:] = one flavor per accelerated node key for which the same
	//              five per-device labels are present.
	//
	// The accelerated-key loop iterates a Go map, so the device-flavor
	// order is non-deterministic. We compare the head directly and the
	// tail as a multiset via ElementsMatch.

	expectedToleration := []core.Toleration{
		{Operator: core.TolerationOpExists},
	}

	cases := []struct {
		name string
		// labels populates node.ObjectMeta.Labels.
		labels map[string]string
		// allocatable populates node.Status.Allocatable. The per-device
		// Accelerator field sources from here — keys should be GPU
		// resource names (e.g., GetResourceName("nvidia", Exclusive)).
		allocatable map[core.ResourceName]int64
		wantCPU     NodeResourceFlavor
		wantDevices []NodeResourceFlavor
		wantEmpty   bool
	}{
		{
			// CPU-only node — only the general flavor is emitted.
			name: "cluster-1/node-1 cpu-only 16C/32Gi/96Gi",
			labels: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				core.LabelHostname:                            "cluster-1-node-1",
				FeatureLabelPrefix + "general.cpu":            "16",
				FeatureLabelPrefix + "general.ram":            "32Gi",
				FeatureLabelPrefix + "general.local-storage":  "96Gi",
				FeatureLabelPrefix + "general.profile-flavor": "16c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
			wantCPU: NodeResourceFlavor{
				Key:               "general",
				ProfileFlavorSpec: "16c-32g-96g",
				ProfileQueueSpec:  "1c-2g",
				ProfileCohortSpec: "1c-2g",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                   "true",
					FeatureLabelPrefix + "general.profile-queue": "1c-2g",
				},
				Tolerations:  expectedToleration,
				Accelerator:  "",
				CPU:          "16",
				RAM:          "32Gi",
				LocalStorage: "96Gi",
			},
		},
		{
			// 1xT4 — CPU/general + one nvidia-tesla-t4 device flavor.
			name: "cluster-1/node-2 T4 1D 4C/16Gi/96Gi",
			labels: map[string]string{
				systemname.ManagedLabelKey:                            "true",
				core.LabelHostname:                                    "cluster-1-node-2",
				FeatureLabelPrefix + "nvidia":                         "true",
				FeatureLabelPrefix + "nvidia-tesla-t4":                "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product":        "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.memory":         "15Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.cores":          "0",
				FeatureLabelPrefix + "nvidia-tesla-t4.accelerators":   "1",
				FeatureLabelPrefix + "general.cpu":                    "4",
				FeatureLabelPrefix + "general.ram":                    "16Gi",
				FeatureLabelPrefix + "general.local-storage":          "96Gi",
				FeatureLabelPrefix + "general.profile-flavor":         "4c-16g-96g",
				FeatureLabelPrefix + "general.profile-queue":          "1c-4g",
				FeatureLabelPrefix + "general.profile-cohort":         "1c-4g",
				FeatureLabelPrefix + "nvidia-tesla-t4.cpu":            "4",
				FeatureLabelPrefix + "nvidia-tesla-t4.ram":            "16Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.local-storage":  "96Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-flavor": "4c-16g-96g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue":  "4c-16g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-cohort": "4c-16g-1d",
			},
			allocatable: map[core.ResourceName]int64{
				GetResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive): 1,
			},
			wantCPU: NodeResourceFlavor{
				Key:               "general",
				ProfileFlavorSpec: "4c-16g-96g",
				ProfileQueueSpec:  "1c-4g",
				ProfileCohortSpec: "1c-4g",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                   "true",
					FeatureLabelPrefix + "general.profile-queue": "1c-4g",
				},
				Tolerations:  expectedToleration,
				Accelerator:  "",
				CPU:          "4",
				RAM:          "16Gi",
				LocalStorage: "96Gi",
			},
			wantDevices: []NodeResourceFlavor{
				{
					Key:               "nvidia-tesla-t4",
					ProfileFlavorSpec: "4c-16g-96g-1d",
					ProfileQueueSpec:  "4c-16g-1d",
					ProfileCohortSpec: "4c-16g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey:                           "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue": "4c-16g-1d",
					},
					Tolerations:  expectedToleration,
					Manufacturer: "nvidia",
					Product:      "Tesla-T4",
					Memory:       "15Gi",
					Cores:        "0",
					Accelerator:  "1",
					CPU:          "4",
					RAM:          "16Gi",
					LocalStorage: "96Gi",
				},
			},
		},
		{
			// 2xT4 — ProfileFlavorSpec differs from node-2 (absolute capacities)
			// but ProfileCohortSpec matches node-2 (same per-device shape:
			// 4 cpu / 16Gi ram per device), demonstrating the Kueue pooling
			// invariant.
			name: "cluster-1/node-4 T4 2D 8C/32Gi/96Gi",
			labels: map[string]string{
				systemname.ManagedLabelKey:                            "true",
				core.LabelHostname:                                    "cluster-1-node-4",
				FeatureLabelPrefix + "nvidia":                         "true",
				FeatureLabelPrefix + "nvidia-tesla-t4":                "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product":        "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.memory":         "15Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.cores":          "0",
				FeatureLabelPrefix + "nvidia-tesla-t4.accelerators":   "2",
				FeatureLabelPrefix + "general.cpu":                    "8",
				FeatureLabelPrefix + "general.ram":                    "32Gi",
				FeatureLabelPrefix + "general.local-storage":          "96Gi",
				FeatureLabelPrefix + "general.profile-flavor":         "8c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":          "1c-4g",
				FeatureLabelPrefix + "general.profile-cohort":         "1c-4g",
				FeatureLabelPrefix + "nvidia-tesla-t4.cpu":            "8",
				FeatureLabelPrefix + "nvidia-tesla-t4.ram":            "32Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.local-storage":  "96Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-flavor": "8c-32g-96g-2d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue":  "4c-16g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-cohort": "4c-16g-1d",
			},
			allocatable: map[core.ResourceName]int64{
				GetResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive): 2,
			},
			wantCPU: NodeResourceFlavor{
				Key:               "general",
				ProfileFlavorSpec: "8c-32g-96g",
				ProfileQueueSpec:  "1c-4g",
				ProfileCohortSpec: "1c-4g",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                   "true",
					FeatureLabelPrefix + "general.profile-queue": "1c-4g",
				},
				Tolerations:  expectedToleration,
				Accelerator:  "",
				CPU:          "8",
				RAM:          "32Gi",
				LocalStorage: "96Gi",
			},
			wantDevices: []NodeResourceFlavor{
				{
					Key:               "nvidia-tesla-t4",
					ProfileFlavorSpec: "8c-32g-96g-2d",
					ProfileQueueSpec:  "4c-16g-1d", // == node-2's device per-unit profile
					ProfileCohortSpec: "4c-16g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey:                           "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue": "4c-16g-1d",
					},
					Tolerations:  expectedToleration,
					Manufacturer: "nvidia",
					Product:      "Tesla-T4",
					Memory:       "15Gi",
					Cores:        "0",
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
			labels: map[string]string{
				systemname.ManagedLabelKey:                        "true",
				core.LabelHostname:                                "cluster-1-node-5",
				FeatureLabelPrefix + "nvidia":                     "true",
				FeatureLabelPrefix + "nvidia-a10g":                "true",
				FeatureLabelPrefix + "nvidia-a10g.product":        "A10G",
				FeatureLabelPrefix + "nvidia-a10g.memory":         "23Gi",
				FeatureLabelPrefix + "nvidia-a10g.cores":          "0",
				FeatureLabelPrefix + "nvidia-a10g.accelerators":   "4",
				FeatureLabelPrefix + "general.cpu":                "48",
				FeatureLabelPrefix + "general.ram":                "188Gi",
				FeatureLabelPrefix + "general.local-storage":      "196Gi",
				FeatureLabelPrefix + "general.profile-flavor":     "48c-188g-196g",
				FeatureLabelPrefix + "general.profile-queue":      "1c-3g",
				FeatureLabelPrefix + "general.profile-cohort":     "1c-3g",
				FeatureLabelPrefix + "nvidia-a10g.cpu":            "48",
				FeatureLabelPrefix + "nvidia-a10g.ram":            "188Gi",
				FeatureLabelPrefix + "nvidia-a10g.local-storage":  "196Gi",
				FeatureLabelPrefix + "nvidia-a10g.profile-flavor": "48c-188g-196g-4d",
				FeatureLabelPrefix + "nvidia-a10g.profile-queue":  "12c-47g-1d",
				FeatureLabelPrefix + "nvidia-a10g.profile-cohort": "12c-47g-1d",
			},
			allocatable: map[core.ResourceName]int64{
				GetResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive): 4,
			},
			wantCPU: NodeResourceFlavor{
				Key:               "general",
				ProfileFlavorSpec: "48c-188g-196g",
				ProfileQueueSpec:  "1c-3g",
				ProfileCohortSpec: "1c-3g",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                   "true",
					FeatureLabelPrefix + "general.profile-queue": "1c-3g",
				},
				Tolerations:  expectedToleration,
				Accelerator:  "",
				CPU:          "48",
				RAM:          "188Gi",
				LocalStorage: "196Gi",
			},
			wantDevices: []NodeResourceFlavor{
				{
					Key:               "nvidia-a10g",
					ProfileFlavorSpec: "48c-188g-196g-4d",
					ProfileQueueSpec:  "12c-47g-1d",
					ProfileCohortSpec: "12c-47g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey:                       "true",
						FeatureLabelPrefix + "nvidia-a10g.profile-queue": "12c-47g-1d",
					},
					Tolerations:  expectedToleration,
					Manufacturer: "nvidia",
					Product:      "A10G",
					Memory:       "23Gi",
					Cores:        "0",
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
			//
			// Also covers the "full" device-label shape (driver-version,
			// runtime-version, family, compute-capability) for the T4
			// to exercise every NodeResourceFlavor field.
			name: "hybrid NVIDIA T4 + AMD MI300X",
			labels: map[string]string{
				systemname.ManagedLabelKey:                                "true",
				FeatureLabelPrefix + "nvidia":                             "true",
				FeatureLabelPrefix + "nvidia.driver-version":              "580.126.09",
				FeatureLabelPrefix + "nvidia.runtime-version":             "13.0",
				FeatureLabelPrefix + "nvidia-tesla-t4":                    "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product":            "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.memory":             "15Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.cores":              "2560",
				FeatureLabelPrefix + "nvidia-tesla-t4.family":             "Turing",
				FeatureLabelPrefix + "nvidia-tesla-t4.compute-capability": "7.5",
				FeatureLabelPrefix + "nvidia-tesla-t4.accelerators":       "1",
				FeatureLabelPrefix + "amd":                                "true",
				FeatureLabelPrefix + "amd-mi300x":                         "true",
				FeatureLabelPrefix + "amd-mi300x.product":                 "MI300X",
				FeatureLabelPrefix + "amd-mi300x.memory":                  "192Gi",
				FeatureLabelPrefix + "amd-mi300x.cores":                   "0",
				FeatureLabelPrefix + "amd-mi300x.accelerators":            "2",
				FeatureLabelPrefix + "general.cpu":                        "32",
				FeatureLabelPrefix + "general.ram":                        "128Gi",
				FeatureLabelPrefix + "general.local-storage":              "200Gi",
				FeatureLabelPrefix + "general.profile-flavor":             "32c-128g-200g",
				FeatureLabelPrefix + "general.profile-queue":              "1c-4g",
				FeatureLabelPrefix + "general.profile-cohort":             "1c-4g",
				FeatureLabelPrefix + "nvidia-tesla-t4.cpu":                "32",
				FeatureLabelPrefix + "nvidia-tesla-t4.ram":                "128Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.local-storage":      "200Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-flavor":     "32c-128g-200g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue":      "32c-128g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-cohort":     "32c-128g-1d",
				FeatureLabelPrefix + "amd-mi300x.cpu":                     "32",
				FeatureLabelPrefix + "amd-mi300x.ram":                     "128Gi",
				FeatureLabelPrefix + "amd-mi300x.local-storage":           "200Gi",
				FeatureLabelPrefix + "amd-mi300x.profile-flavor":          "32c-128g-200g-2d",
				FeatureLabelPrefix + "amd-mi300x.profile-queue":           "16c-64g-1d",
				FeatureLabelPrefix + "amd-mi300x.profile-cohort":          "16c-64g-1d",
			},
			allocatable: map[core.ResourceName]int64{
				GetResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive): 1,
				GetResourceName(ManufacturerAMD, workercore.DeviceAllocationModeExclusive):    2,
			},
			wantCPU: NodeResourceFlavor{
				Key:               "general",
				ProfileFlavorSpec: "32c-128g-200g",
				ProfileQueueSpec:  "1c-4g",
				ProfileCohortSpec: "1c-4g",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                   "true",
					FeatureLabelPrefix + "general.profile-queue": "1c-4g",
				},
				Tolerations:  expectedToleration,
				Accelerator:  "",
				CPU:          "32",
				RAM:          "128Gi",
				LocalStorage: "200Gi",
			},
			wantDevices: []NodeResourceFlavor{
				{
					Key:               "nvidia-tesla-t4",
					ProfileFlavorSpec: "32c-128g-200g-1d",
					ProfileQueueSpec:  "32c-128g-1d",
					ProfileCohortSpec: "32c-128g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey:                           "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue": "32c-128g-1d",
					},
					Tolerations:       expectedToleration,
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
					Key:               "amd-mi300x",
					ProfileFlavorSpec: "32c-128g-200g-2d",
					ProfileQueueSpec:  "16c-64g-1d",
					ProfileCohortSpec: "16c-64g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey:                      "true",
						FeatureLabelPrefix + "amd-mi300x.profile-queue": "16c-64g-1d",
					},
					Tolerations:  expectedToleration,
					Manufacturer: "amd",
					Product:      "MI300X",
					Memory:       "192Gi",
					Cores:        "0",
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
			// Capacity-label gate: only general.cpu is missing.
			name: "missing general.cpu returns no flavors",
			labels: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.ram":            "32Gi",
				FeatureLabelPrefix + "general.local-storage":  "96Gi",
				FeatureLabelPrefix + "general.profile-flavor": "16c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only general.ram is missing.
			name: "missing general.ram returns no flavors",
			labels: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "16",
				FeatureLabelPrefix + "general.local-storage":  "96Gi",
				FeatureLabelPrefix + "general.profile-flavor": "16c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only general.local-storage is missing.
			name: "missing general.local-storage returns no flavors",
			labels: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "16",
				FeatureLabelPrefix + "general.ram":            "32Gi",
				FeatureLabelPrefix + "general.profile-flavor": "16c-32g-96g",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only general.profile-flavor is missing.
			name: "missing general.profile-flavor returns no flavors",
			labels: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "16",
				FeatureLabelPrefix + "general.ram":            "32Gi",
				FeatureLabelPrefix + "general.local-storage":  "96Gi",
				FeatureLabelPrefix + "general.profile-queue":  "1c-2g",
				FeatureLabelPrefix + "general.profile-cohort": "1c-2g",
			},
			wantEmpty: true,
		},
		{
			// Capacity-label gate: only general.profile-cohort is missing.
			name: "missing general.profile-cohort returns no flavors",
			labels: map[string]string{
				systemname.ManagedLabelKey:                    "true",
				FeatureLabelPrefix + "general.cpu":            "16",
				FeatureLabelPrefix + "general.ram":            "32Gi",
				FeatureLabelPrefix + "general.local-storage":  "96Gi",
				FeatureLabelPrefix + "general.profile-flavor": "16c-32g-96g",
			},
			wantEmpty: true,
		},
		{
			// Capacity-label gate also suppresses device flavors: even
			// when accelerated device keys are present, missing general
			// capacity zeroes out the whole result.
			name: "device labels without general capacity still returns no flavors",
			labels: map[string]string{
				systemname.ManagedLabelKey:                          "true",
				FeatureLabelPrefix + "nvidia":                       "true",
				FeatureLabelPrefix + "nvidia-tesla-t4":              "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "1",
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
			labels: map[string]string{
				systemname.ManagedLabelKey:                          "true",
				FeatureLabelPrefix + "nvidia":                       "true",
				FeatureLabelPrefix + "nvidia-tesla-t4":              "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.memory":       "15Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.cores":        "0",
				FeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "1",
				FeatureLabelPrefix + "general.cpu":                  "4",
				FeatureLabelPrefix + "general.ram":                  "16Gi",
				FeatureLabelPrefix + "general.local-storage":        "96Gi",
				FeatureLabelPrefix + "general.profile-flavor":       "4c-16g-96g",
				FeatureLabelPrefix + "general.profile-queue":        "1c-4g",
				FeatureLabelPrefix + "general.profile-cohort":       "1c-4g",
				// No nvidia-tesla-t4.cpu / .ram / .local-storage / .profile-flavor / .profile-cohort → device is skipped.
			},
			wantCPU: NodeResourceFlavor{
				Key:               "general",
				ProfileFlavorSpec: "4c-16g-96g",
				ProfileQueueSpec:  "1c-4g",
				ProfileCohortSpec: "1c-4g",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                   "true",
					FeatureLabelPrefix + "general.profile-queue": "1c-4g",
				},
				Tolerations:  expectedToleration,
				Accelerator:  "",
				CPU:          "4",
				RAM:          "16Gi",
				LocalStorage: "96Gi",
			},
			wantDevices: nil,
		},
		{
			// Per-device gate is independent across devices: T4 has full
			// per-device capacity → its flavor is emitted; MI300X has
			// only a .cpu label, no .ram / .local-storage / .profile-flavor /
			// .profile-cohort → it is skipped. Only CPU + T4 appear in the
			// result.
			name: "partial per-device capacity only emits the complete device",
			labels: map[string]string{
				systemname.ManagedLabelKey:                            "true",
				FeatureLabelPrefix + "nvidia":                         "true",
				FeatureLabelPrefix + "nvidia-tesla-t4":                "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product":        "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.memory":         "15Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.cores":          "0",
				FeatureLabelPrefix + "nvidia-tesla-t4.accelerators":   "1",
				FeatureLabelPrefix + "amd":                            "true",
				FeatureLabelPrefix + "amd-mi300x":                     "true",
				FeatureLabelPrefix + "amd-mi300x.product":             "MI300X",
				FeatureLabelPrefix + "amd-mi300x.memory":              "192Gi",
				FeatureLabelPrefix + "amd-mi300x.cores":               "0",
				FeatureLabelPrefix + "amd-mi300x.accelerators":        "2",
				FeatureLabelPrefix + "general.cpu":                    "32",
				FeatureLabelPrefix + "general.ram":                    "128Gi",
				FeatureLabelPrefix + "general.local-storage":          "200Gi",
				FeatureLabelPrefix + "general.profile-flavor":         "32c-128g-200g",
				FeatureLabelPrefix + "general.profile-queue":          "1c-4g",
				FeatureLabelPrefix + "general.profile-cohort":         "1c-4g",
				FeatureLabelPrefix + "nvidia-tesla-t4.cpu":            "32",
				FeatureLabelPrefix + "nvidia-tesla-t4.ram":            "128Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.local-storage":  "200Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-flavor": "32c-128g-200g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue":  "32c-128g-1d",
				FeatureLabelPrefix + "nvidia-tesla-t4.profile-cohort": "32c-128g-1d",
				FeatureLabelPrefix + "amd-mi300x.cpu":                 "32", // remaining per-device labels intentionally absent
			},
			allocatable: map[core.ResourceName]int64{
				GetResourceName(ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive): 1,
			},
			wantCPU: NodeResourceFlavor{
				Key:               "general",
				ProfileFlavorSpec: "32c-128g-200g",
				ProfileQueueSpec:  "1c-4g",
				ProfileCohortSpec: "1c-4g",
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                   "true",
					FeatureLabelPrefix + "general.profile-queue": "1c-4g",
				},
				Tolerations:  expectedToleration,
				Accelerator:  "",
				CPU:          "32",
				RAM:          "128Gi",
				LocalStorage: "200Gi",
			},
			wantDevices: []NodeResourceFlavor{
				{
					Key:               "nvidia-tesla-t4",
					ProfileFlavorSpec: "32c-128g-200g-1d",
					ProfileQueueSpec:  "32c-128g-1d",
					ProfileCohortSpec: "32c-128g-1d",
					NodeLabels: map[string]string{
						systemname.ManagedLabelKey:                           "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.profile-queue": "32c-128g-1d",
					},
					Tolerations:  expectedToleration,
					Manufacturer: "nvidia",
					Product:      "Tesla-T4",
					Memory:       "15Gi",
					Cores:        "0",
					Accelerator:  "1",
					CPU:          "32",
					RAM:          "128Gi",
					LocalStorage: "200Gi",
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
			if len(cs.allocatable) > 0 {
				alloc := core.ResourceList{}
				for k, v := range cs.allocatable {
					alloc[k] = *resource.NewQuantity(v, resource.DecimalSI)
				}
				node.Status.Allocatable = alloc
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

func TestParseNodeProfile(t *testing.T) {
	// ParseNodeProfile consumes the full profile string emitted by
	// FormatNodeProfile:
	//
	//   "gpustack-${key}-${cpu}c-${ram}g[-${stg}g][-${acc}d][-${sliced}s]"
	//
	// The "gpustack-" prefix and a non-empty key are required. cpu and ram
	// are required; localStorage, accelerator, and sliced are optional.
	// Sliced is only valid when accelerator is also present.

	cases := []struct {
		name     string
		profile  string
		wantKey  string
		wantSpec NodeProfileSpec
		wantOK   bool
	}{
		// --- valid shapes ---
		{
			name:     "general profile with localStorage",
			profile:  "gpustack-general-16c-32g-88g",
			wantKey:  "general",
			wantSpec: NodeProfileSpec{CPU: "16", RAM: "32", LocalStorage: "88"},
			wantOK:   true,
		},
		{
			name:     "general profile without localStorage",
			profile:  "gpustack-general-1c-2g",
			wantKey:  "general",
			wantSpec: NodeProfileSpec{CPU: "1", RAM: "2"},
			wantOK:   true,
		},
		{
			name:     "single-digit values",
			profile:  "gpustack-general-1c-1g-15g",
			wantKey:  "general",
			wantSpec: NodeProfileSpec{CPU: "1", RAM: "1", LocalStorage: "15"},
			wantOK:   true,
		},
		{
			name:     "per-device profile with localStorage and accelerator",
			profile:  "gpustack-nvidia-tesla-t4-4c-16g-88g-1d",
			wantKey:  "nvidia-tesla-t4",
			wantSpec: NodeProfileSpec{CPU: "4", RAM: "16", LocalStorage: "88", Accelerator: "1"},
			wantOK:   true,
		},
		{
			name:     "per-device profile with accelerator (no localStorage)",
			profile:  "gpustack-nvidia-tesla-t4-4c-16g-1d",
			wantKey:  "nvidia-tesla-t4",
			wantSpec: NodeProfileSpec{CPU: "4", RAM: "16", Accelerator: "1"},
			wantOK:   true,
		},
		{
			name:     "per-device profile with accelerator and sliced",
			profile:  "gpustack-nvidia-a10g-12c-48g-1d-8s",
			wantKey:  "nvidia-a10g",
			wantSpec: NodeProfileSpec{CPU: "12", RAM: "48", Accelerator: "1", SlicedAccelerator: "8"},
			wantOK:   true,
		},
		{
			name:     "per-device profile with localStorage, accelerator, and sliced",
			profile:  "gpustack-nvidia-a10g-48c-192g-88g-4d-8s",
			wantKey:  "nvidia-a10g",
			wantSpec: NodeProfileSpec{CPU: "48", RAM: "192", LocalStorage: "88", Accelerator: "4", SlicedAccelerator: "8"},
			wantOK:   true,
		},
		{
			name:     "large values",
			profile:  "gpustack-nvidia-a10g-48c-188g-196g-4d",
			wantKey:  "nvidia-a10g",
			wantSpec: NodeProfileSpec{CPU: "48", RAM: "188", LocalStorage: "196", Accelerator: "4"},
			wantOK:   true,
		},
		{
			// Key may itself contain a segment that ends with "d" — the
			// accelerator detector only inspects the final segment.
			name:     "key segment ending in d is preserved",
			profile:  "gpustack-amd-mi300x-4c-16g",
			wantKey:  "amd-mi300x",
			wantSpec: NodeProfileSpec{CPU: "4", RAM: "16"},
			wantOK:   true,
		},
		{
			// Key may contain a segment that ends with "g"; ram detection
			// only inspects the trailing segments after sliced/acc are
			// consumed.
			name:     "key segment ending in g is preserved",
			profile:  "gpustack-nvidia-a10g-4c-16g",
			wantKey:  "nvidia-a10g",
			wantSpec: NodeProfileSpec{CPU: "4", RAM: "16"},
			wantOK:   true,
		},
		// --- prefix rejections ---
		{
			name:    "empty string is rejected",
			profile: "",
			wantOK:  false,
		},
		{
			name:    "missing gpustack- prefix",
			profile: "general-16c-32g-96g",
			wantOK:  false,
		},
		{
			name:    "wrong leading prefix",
			profile: "foo-bar-16c-32g-96g",
			wantOK:  false,
		},
		// --- key rejections ---
		{
			name:    "missing key (cpu directly after prefix)",
			profile: "gpustack-16c-32g",
			wantOK:  false,
		},
		{
			name:    "missing key with localStorage",
			profile: "gpustack-16c-32g-96g",
			wantOK:  false,
		},
		// --- cpu / ram rejections ---
		{
			name:    "only ram (cpu missing)",
			profile: "gpustack-general-32g",
			wantOK:  false,
		},
		{
			name:    "cpu without c suffix",
			profile: "gpustack-general-16-32g-96g",
			wantOK:  false,
		},
		{
			name:    "ram without g suffix",
			profile: "gpustack-general-16c-32-96g",
			wantOK:  false,
		},
		{
			name:    "local-storage without g suffix",
			profile: "gpustack-general-16c-32g-96",
			wantOK:  false,
		},
		{
			name:    "bare cpu (just c)",
			profile: "gpustack-general-c-32g-96g",
			wantOK:  false,
		},
		{
			name:    "bare ram (just g)",
			profile: "gpustack-general-16c-g-96g",
			wantOK:  false,
		},
		{
			name:    "bare local-storage (just g)",
			profile: "gpustack-general-16c-32g-g",
			wantOK:  false,
		},
		{
			name:    "non-numeric cpu",
			profile: "gpustack-general-xc-32g-96g",
			wantOK:  false,
		},
		{
			name:    "non-numeric ram",
			profile: "gpustack-general-16c-yg-96g",
			wantOK:  false,
		},
		{
			name:    "non-numeric local-storage",
			profile: "gpustack-general-16c-32g-zg",
			wantOK:  false,
		},
		// --- accelerator rejections ---
		{
			name:    "bare accelerator (just d)",
			profile: "gpustack-nvidia-tesla-t4-4c-16g-d",
			wantOK:  false,
		},
		{
			name:    "non-numeric accelerator",
			profile: "gpustack-nvidia-tesla-t4-4c-16g-xd",
			wantOK:  false,
		},
		// --- sliced rejections ---
		{
			name:    "sliced without accelerator",
			profile: "gpustack-general-4c-16g-8s",
			wantOK:  false,
		},
		{
			name:    "sliced without accelerator (with localStorage)",
			profile: "gpustack-general-4c-16g-88g-8s",
			wantOK:  false,
		},
		{
			name:    "bare sliced (just s)",
			profile: "gpustack-nvidia-tesla-t4-4c-16g-1d-s",
			wantOK:  false,
		},
		{
			name:    "non-numeric sliced",
			profile: "gpustack-nvidia-tesla-t4-4c-16g-1d-xs",
			wantOK:  false,
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			key, spec, ok := ParseNodeProfile(cs.profile)
			assert.Equal(t, cs.wantOK, ok, "unexpected ok")
			assert.Equal(t, cs.wantKey, key, "unexpected key")
			assert.Equal(t, cs.wantSpec, spec, "unexpected spec")
		})
	}
}
