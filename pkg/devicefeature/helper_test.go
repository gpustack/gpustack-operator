package devicefeature

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
)

func TestConstructNodeLabels(t *testing.T) {
	nodeName := "node1"
	cpuCapacity := *resource.NewQuantity(4, resource.DecimalSI)
	ramCapacity := *resource.NewQuantity(16*quantityx.Gi, resource.BinarySI)
	localStorageCapacity := *resource.NewQuantity(100*quantityx.Gi, resource.BinarySI)

	node := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: nodeName,
			Labels: map[string]string{
				core.LabelHostname: nodeName,
			},
		},
		Status: core.NodeStatus{
			Capacity: core.ResourceList{
				core.ResourceCPU:              cpuCapacity,
				core.ResourceMemory:           ramCapacity,
				core.ResourceEphemeralStorage: localStorageCapacity,
			},
			Allocatable: core.ResourceList{
				core.ResourceCPU:              cpuCapacity,
				core.ResourceMemory:           ramCapacity,
				core.ResourceEphemeralStorage: localStorageCapacity,
			},
		},
	}

	cases := []struct {
		name     string
		node     *core.Node
		groups   device.DevicesGroupList
		expected map[string]string
	}{
		{
			name:   "no groups",
			node:   node,
			groups: nil,
			expected: map[string]string{
				systemname.ManagedLabelKey: "true",
			},
		},
		{
			name: "group without accelerators is skipped",
			node: node,
			groups: device.DevicesGroupList{
				{
					ID:           "tesla-t4",
					Manufacturer: "nvidia",
					Name:         "Tesla T4",
					Memory:       15360,
				},
			},
			expected: map[string]string{
				systemname.ManagedLabelKey: "true",
			},
		},
		{
			name: "full group",
			node: node,
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
				systemname.ManagedLabelKey:                          "true",
				FeatureLabelPrefix + "nvidia":                       "true",
				FeatureLabelPrefix + "nvidia.driver-version":        "580.126.09",
				FeatureLabelPrefix + "nvidia.runtime-version":       "13.0",
				FeatureLabelPrefix + "nvidia.compute-capability":    "7.5",
				FeatureLabelPrefix + "nvidia-tesla-t4":              "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.memory":       "15Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.cores":        "2560",
				FeatureLabelPrefix + "nvidia-tesla-t4.family":       "Turing",
				FeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "4",
				FeatureLabelPrefix + "nvidia-tesla-t4.unit-cpu":     "1",
				FeatureLabelPrefix + "nvidia-tesla-t4.unit-ram":     "1Gi",
			},
		},
		{
			name: "minimal group without optional fields",
			node: node,
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
				systemname.ManagedLabelKey:                      "true",
				FeatureLabelPrefix + "nvidia":                   "true",
				FeatureLabelPrefix + "nvidia-h100":              "true",
				FeatureLabelPrefix + "nvidia-h100.product":      "H100",
				FeatureLabelPrefix + "nvidia-h100.memory":       "80Gi",
				FeatureLabelPrefix + "nvidia-h100.cores":        "0",
				FeatureLabelPrefix + "nvidia-h100.accelerators": "1",
				FeatureLabelPrefix + "nvidia-h100.unit-cpu":     "2",
				FeatureLabelPrefix + "nvidia-h100.unit-ram":     "12Gi",
			},
		},
		{
			name: "multiple groups, one skipped",
			node: node,
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
				systemname.ManagedLabelKey:                          "true",
				FeatureLabelPrefix + "nvidia":                       "true",
				FeatureLabelPrefix + "nvidia-tesla-t4":              "true",
				FeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
				FeatureLabelPrefix + "nvidia-tesla-t4.memory":       "15Gi",
				FeatureLabelPrefix + "nvidia-tesla-t4.cores":        "0",
				FeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "1",
				FeatureLabelPrefix + "nvidia-tesla-t4.unit-cpu":     "2",
				FeatureLabelPrefix + "nvidia-tesla-t4.unit-ram":     "12Gi",
				FeatureLabelPrefix + "amd":                          "true",
				FeatureLabelPrefix + "amd-mi300x":                   "true",
				FeatureLabelPrefix + "amd-mi300x.product":           "MI300X",
				FeatureLabelPrefix + "amd-mi300x.memory":            "192Gi",
				FeatureLabelPrefix + "amd-mi300x.cores":             "0",
				FeatureLabelPrefix + "amd-mi300x.accelerators":      "2",
				FeatureLabelPrefix + "amd-mi300x.unit-cpu":          "1",
				FeatureLabelPrefix + "amd-mi300x.unit-ram":          "6Gi",
			},
		},
		{
			// No managed label on the node — defaults to "true".
			name: "managed label unset on node",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						core.LabelHostname: nodeName,
					},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
					Allocatable: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
				},
			},
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
				systemname.ManagedLabelKey:                      "true",
				FeatureLabelPrefix + "nvidia":                   "true",
				FeatureLabelPrefix + "nvidia-h100":              "true",
				FeatureLabelPrefix + "nvidia-h100.product":      "H100",
				FeatureLabelPrefix + "nvidia-h100.memory":       "80Gi",
				FeatureLabelPrefix + "nvidia-h100.cores":        "0",
				FeatureLabelPrefix + "nvidia-h100.accelerators": "1",
				FeatureLabelPrefix + "nvidia-h100.unit-cpu":     "2",
				FeatureLabelPrefix + "nvidia-h100.unit-ram":     "12Gi",
			},
		},
		{
			// Explicit managed=true on the node — preserved.
			name: "managed label is true on node",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						core.LabelHostname:         nodeName,
						systemname.ManagedLabelKey: "true",
					},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
					Allocatable: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
				},
			},
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
				systemname.ManagedLabelKey:                      "true",
				FeatureLabelPrefix + "nvidia":                   "true",
				FeatureLabelPrefix + "nvidia-h100":              "true",
				FeatureLabelPrefix + "nvidia-h100.product":      "H100",
				FeatureLabelPrefix + "nvidia-h100.memory":       "80Gi",
				FeatureLabelPrefix + "nvidia-h100.cores":        "0",
				FeatureLabelPrefix + "nvidia-h100.accelerators": "1",
				FeatureLabelPrefix + "nvidia-h100.unit-cpu":     "2",
				FeatureLabelPrefix + "nvidia-h100.unit-ram":     "12Gi",
			},
		},
		{
			// Explicit managed=false on the node — overrides the default
			// "true", but device labels are still emitted because the
			// processing loop continues regardless of the managed value.
			name: "managed label is false on node",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						core.LabelHostname:         nodeName,
						systemname.ManagedLabelKey: "false",
					},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
					Allocatable: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
				},
			},
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
				systemname.ManagedLabelKey:                      "false",
				FeatureLabelPrefix + "nvidia":                   "true",
				FeatureLabelPrefix + "nvidia-h100":              "true",
				FeatureLabelPrefix + "nvidia-h100.product":      "H100",
				FeatureLabelPrefix + "nvidia-h100.memory":       "80Gi",
				FeatureLabelPrefix + "nvidia-h100.cores":        "0",
				FeatureLabelPrefix + "nvidia-h100.accelerators": "1",
				FeatureLabelPrefix + "nvidia-h100.unit-cpu":     "2",
				FeatureLabelPrefix + "nvidia-h100.unit-ram":     "12Gi",
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ConstructNodeLabels(cs.node, cs.groups)
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
			expected: []string{DisfeaturedNodeKey},
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
			expected: []string{DisfeaturedNodeKey},
		},
		{
			name: "non-empty labels with feature label",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{
						"foo":                         "bar",
						FeatureLabelPrefix + "nvidia": "true",
						FeatureLabelPrefix + "nvidia.compute-capability":    "7.5",
						FeatureLabelPrefix + "nvidia.driver-version":        "580.126.09",
						FeatureLabelPrefix + "nvidia.runtime-version":       "13.0",
						FeatureLabelPrefix + "nvidia-tesla-t4":              "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "4",
						FeatureLabelPrefix + "nvidia-tesla-t4.cores":        "2560",
						FeatureLabelPrefix + "nvidia-tesla-t4.family":       "Turing",
						FeatureLabelPrefix + "nvidia-tesla-t4.memory":       "15Gi",
						FeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
						FeatureLabelPrefix + "nvidia-tesla-t4.unit-cpu":     "1",
						FeatureLabelPrefix + "nvidia-tesla-t4.unit-ram":     "4Gi",
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

func TestExtractNodeFeatureByKey(t *testing.T) {
	nodeName := "node1"
	cpuCapacity := *resource.NewQuantity(4, resource.DecimalSI)
	ramCapacity := *resource.NewQuantity(16*quantityx.Gi, resource.BinarySI)
	localStorageCapacity := *resource.NewQuantity(100*quantityx.Gi, resource.BinarySI)

	cases := []struct {
		name     string
		node     *core.Node
		key      string
		expected NodeFeature
	}{
		{
			name: "empty labels",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						core.LabelHostname: nodeName,
					},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
					Allocatable: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
				},
			},
			key: DisfeaturedNodeKey,
			expected: NodeFeature{
				NodeLabels: map[string]string{
					core.LabelHostname: nodeName,
				},
				CPU:          cpuCapacity,
				RAM:          ramCapacity,
				LocalStorage: localStorageCapacity,
			},
		},
		{
			name: "non-empty labels without feature label",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						core.LabelHostname: nodeName,
						"foo":              "bar",
					},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
					Allocatable: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
					},
				},
			},
			key: DisfeaturedNodeKey,
			expected: NodeFeature{
				NodeLabels: map[string]string{
					core.LabelHostname: nodeName,
				},
				CPU:          cpuCapacity,
				RAM:          ramCapacity,
				LocalStorage: localStorageCapacity,
			},
		},
		{
			name: "non-empty labels with feature label",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						core.LabelHostname:                                  nodeName,
						FeatureLabelPrefix + "nvidia":                       "true",
						FeatureLabelPrefix + "nvidia.compute-capability":    "7.5",
						FeatureLabelPrefix + "nvidia.driver-version":        "580.126.09",
						FeatureLabelPrefix + "nvidia.runtime-version":       "13.0",
						FeatureLabelPrefix + "nvidia-tesla-t4":              "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "4",
						FeatureLabelPrefix + "nvidia-tesla-t4.cores":        "2560",
						FeatureLabelPrefix + "nvidia-tesla-t4.family":       "Turing",
						FeatureLabelPrefix + "nvidia-tesla-t4.memory":       "15Gi",
						FeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
						FeatureLabelPrefix + "nvidia-tesla-t4.unit-cpu":     "1",
						FeatureLabelPrefix + "nvidia-tesla-t4.unit-ram":     "4Gi",
					},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
						"nvidia.com/gpu":              *resource.NewQuantity(4, resource.DecimalSI),
					},
					Allocatable: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
						"nvidia.com/gpu":              *resource.NewQuantity(2, resource.DecimalSI),
					},
				},
			},
			key: "nvidia-tesla-t4",
			expected: NodeFeature{
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                      "true",
					FeatureLabelPrefix + "nvidia-tesla-t4":          "true",
					FeatureLabelPrefix + "nvidia-tesla-t4.unit-cpu": "1",
					FeatureLabelPrefix + "nvidia-tesla-t4.unit-ram": "4Gi",
				},
				Sliced:            "",
				Manufacturer:      "nvidia",
				Product:           "Tesla-T4",
				Memory:            "15Gi",
				Cores:             "2560",
				ComputeCapability: "7.5",
				Family:            "Turing",
				Accelerator:       *resource.NewQuantity(2, resource.DecimalSI),
				// CPU/RAM = unit (1 / 4Gi) × Accelerator (2) = 2000m / 8Gi.
				CPU:          *resource.NewMilliQuantity(2000, resource.DecimalSI),
				RAM:          *resource.NewQuantity(8*quantityx.Gi, resource.BinarySI),
				LocalStorage: localStorageCapacity,
				UnitResources: DeviceUnitResources{
					CPU: resource.MustParse("1"),
					RAM: resource.MustParse("4Gi"),
				},
			},
		},
		{
			name: "with taints",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						core.LabelHostname:                                  nodeName,
						FeatureLabelPrefix + "nvidia":                       "true",
						FeatureLabelPrefix + "nvidia.compute-capability":    "7.5",
						FeatureLabelPrefix + "nvidia.driver-version":        "580.126.09",
						FeatureLabelPrefix + "nvidia.runtime-version":       "13.0",
						FeatureLabelPrefix + "nvidia-tesla-t4":              "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "4",
						FeatureLabelPrefix + "nvidia-tesla-t4.cores":        "2560",
						FeatureLabelPrefix + "nvidia-tesla-t4.family":       "Turing",
						FeatureLabelPrefix + "nvidia-tesla-t4.memory":       "15Gi",
						FeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
						FeatureLabelPrefix + "nvidia-tesla-t4.unit-cpu":     "1",
						FeatureLabelPrefix + "nvidia-tesla-t4.unit-ram":     "4Gi",
					},
				},
				Spec: core.NodeSpec{
					Taints: []core.Taint{
						{
							Key:       DeviceLabelPrefix + "acclerator.sliced",
							Value:     "2",
							Effect:    core.TaintEffectNoSchedule,
							TimeAdded: &meta.Time{Time: meta.Now().Time},
						},
					},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
						"nvidia.com/gpu":              *resource.NewQuantity(4, resource.DecimalSI),
					},
					Allocatable: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
						"nvidia.com/gpu":              *resource.NewQuantity(2, resource.DecimalSI),
					},
				},
			},
			key: "nvidia-tesla-t4",
			expected: NodeFeature{
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                      "true",
					FeatureLabelPrefix + "nvidia-tesla-t4":          "true",
					FeatureLabelPrefix + "nvidia-tesla-t4.unit-cpu": "1",
					FeatureLabelPrefix + "nvidia-tesla-t4.unit-ram": "4Gi",
				},
				Tolerations: []core.Toleration{
					{
						Key:      DeviceLabelPrefix + "acclerator.sliced",
						Operator: core.TolerationOpEqual,
						Value:    "2",
						Effect:   core.TaintEffectNoSchedule,
					},
				},
				Sliced:            "2",
				Manufacturer:      "nvidia",
				Product:           "Tesla-T4",
				Memory:            "15Gi",
				Cores:             "2560",
				ComputeCapability: "7.5",
				Family:            "Turing",
				Accelerator:       *resource.NewQuantity(2, resource.DecimalSI),
				// CPU/RAM = unit (1 / 4Gi) × Accelerator (2) = 2000m / 8Gi.
				CPU:          *resource.NewMilliQuantity(2000, resource.DecimalSI),
				RAM:          *resource.NewQuantity(8*quantityx.Gi, resource.BinarySI),
				LocalStorage: localStorageCapacity,
				UnitResources: DeviceUnitResources{
					CPU: resource.MustParse("1"),
					RAM: resource.MustParse("4Gi"),
				},
			},
		},
		{
			// Labels absent, four accelerators on a 4C/16Gi host: per-device
			// share after the 1C/2Gi reservation is 750m / 3584Mi, which
			// drives suggested CPU to 0 — GetDeviceUnitResources falls back
			// to the 1C/1Gi default.
			name: "four allocatable accelerators, labels absent — defaults",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						core.LabelHostname:                                  nodeName,
						FeatureLabelPrefix + "nvidia":                       "true",
						FeatureLabelPrefix + "nvidia-tesla-t4":              "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
						FeatureLabelPrefix + "nvidia-tesla-t4.memory":       "15Gi",
						FeatureLabelPrefix + "nvidia-tesla-t4.cores":        "2560",
						FeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "4",
					},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
						"nvidia.com/gpu":              *resource.NewQuantity(4, resource.DecimalSI),
					},
					Allocatable: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
						"nvidia.com/gpu":              *resource.NewQuantity(4, resource.DecimalSI),
					},
				},
			},
			key: "nvidia-tesla-t4",
			expected: NodeFeature{
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                      "true",
					FeatureLabelPrefix + "nvidia-tesla-t4":          "true",
					FeatureLabelPrefix + "nvidia-tesla-t4.unit-cpu": "",
					FeatureLabelPrefix + "nvidia-tesla-t4.unit-ram": "",
				},
				Manufacturer: "nvidia",
				Product:      "Tesla-T4",
				Memory:       "15Gi",
				Cores:        "2560",
				Accelerator:  *resource.NewQuantity(4, resource.DecimalSI),
				// CPU/RAM = unit (1 / 1Gi) × Accelerator (4) = 4000m / 4Gi.
				CPU:          *resource.NewMilliQuantity(4000, resource.DecimalSI),
				RAM:          *resource.NewQuantity(4*quantityx.Gi, resource.BinarySI),
				LocalStorage: localStorageCapacity,
				UnitResources: DeviceUnitResources{
					CPU: *resource.NewQuantity(1, resource.DecimalSI),
					RAM: *resource.NewQuantity(quantityx.Gi, resource.BinarySI),
				},
			},
		},
		{
			// Labels absent, zero allocatable accelerators (e.g. all
			// pre-assigned): GetDeviceUnitResources falls back to n=1 with
			// per-device share 3000m / 14336Mi → suggested CPU=2, the 6:1
			// ratio induces 12288Mi which fits strictly under 14336Mi.
			name: "zero allocatable accelerators, labels absent — n=1 fallback",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						core.LabelHostname:                                  nodeName,
						FeatureLabelPrefix + "nvidia":                       "true",
						FeatureLabelPrefix + "nvidia-tesla-t4":              "true",
						FeatureLabelPrefix + "nvidia-tesla-t4.product":      "Tesla-T4",
						FeatureLabelPrefix + "nvidia-tesla-t4.memory":       "15Gi",
						FeatureLabelPrefix + "nvidia-tesla-t4.cores":        "2560",
						FeatureLabelPrefix + "nvidia-tesla-t4.accelerators": "4",
					},
				},
				Status: core.NodeStatus{
					Capacity: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
						"nvidia.com/gpu":              *resource.NewQuantity(4, resource.DecimalSI),
					},
					Allocatable: core.ResourceList{
						core.ResourceCPU:              cpuCapacity,
						core.ResourceMemory:           ramCapacity,
						core.ResourceEphemeralStorage: localStorageCapacity,
						"nvidia.com/gpu":              *resource.NewQuantity(0, resource.DecimalSI),
					},
				},
			},
			key: "nvidia-tesla-t4",
			expected: NodeFeature{
				NodeLabels: map[string]string{
					systemname.ManagedLabelKey:                      "true",
					FeatureLabelPrefix + "nvidia-tesla-t4":          "true",
					FeatureLabelPrefix + "nvidia-tesla-t4.unit-cpu": "",
					FeatureLabelPrefix + "nvidia-tesla-t4.unit-ram": "",
				},
				Manufacturer: "nvidia",
				Product:      "Tesla-T4",
				Memory:       "15Gi",
				Cores:        "2560",
				Accelerator:  *resource.NewQuantity(0, resource.DecimalSI),
				// Accelerator=0 zeros the per-node CPU/RAM regardless of the
				// per-device unit — no accelerator slots, no booked headroom.
				CPU:          *resource.NewMilliQuantity(0, resource.DecimalSI),
				RAM:          *resource.NewQuantity(0, resource.BinarySI),
				LocalStorage: localStorageCapacity,
				UnitResources: DeviceUnitResources{
					CPU: *resource.NewQuantity(2, resource.DecimalSI),
					RAM: *resource.NewQuantity(12*quantityx.Gi, resource.BinarySI),
				},
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := ExtractNodeFeatureByKey(cs.node, cs.key)
			assert.Equal(t, cs.expected, actual, "unexpected node feature")
		})
	}
}

func TestGetDeviceUnitResources(t *testing.T) {
	const nodeKey = "nvidia-tesla-t4"
	selfLabelKey := FeatureLabelPrefix + nodeKey

	// Match the function's compute-path construction so reflect.DeepEqual
	// works against the Quantity's internal state (empty cache).
	cores := func(n int64) resource.Quantity {
		return *resource.NewQuantity(n, resource.DecimalSI)
	}
	gi := func(n int64) resource.Quantity {
		return *resource.NewQuantity(n*quantityx.Gi, resource.BinarySI)
	}
	defaultUnits := DeviceUnitResources{CPU: cores(1), RAM: gi(1)}

	cases := []struct {
		name        string
		labels      map[string]string
		allocCPU    resource.Quantity
		allocRAM    resource.Quantity
		deviceCount int64
		expected    DeviceUnitResources
	}{
		{
			// Labels are returned as parsed Quantities, format preserved.
			name: "labels present in canonical form",
			labels: map[string]string{
				selfLabelKey + ".unit-cpu": "2000m",
				selfLabelKey + ".unit-ram": "8192Mi",
			},
			// Allocatable and deviceCount are ignored on the read path.
			deviceCount: 99,
			expected: DeviceUnitResources{
				CPU: resource.MustParse("2000m"),
				RAM: resource.MustParse("8192Mi"),
			},
		},
		{
			// Operator-written cores / Gi form is preserved verbatim — the
			// read path no longer reformats.
			name: "labels present in core/Gi form — preserved",
			labels: map[string]string{
				selfLabelKey + ".unit-cpu": "4",
				selfLabelKey + ".unit-ram": "16Gi",
			},
			deviceCount: 4,
			expected: DeviceUnitResources{
				CPU: resource.MustParse("4"),
				RAM: resource.MustParse("16Gi"),
			},
		},
		{
			// Both labels must parse — if either is malformed, the function
			// falls through to the compute path.
			name: "labels malformed — falls through to compute path",
			labels: map[string]string{
				selfLabelKey + ".unit-cpu": "not-a-quantity",
				selfLabelKey + ".unit-ram": "still-bad",
			},
			allocCPU:    resource.MustParse("4"),
			allocRAM:    resource.MustParse("16Gi"),
			deviceCount: 1,
			// per-device share 3000m / 14336Mi after the 1C/2Gi
			// reservation. Suggested CPU = (3000-1)/1000 = 2; 2*8/2*7 Gi
			// both meet or exceed 14336Mi, 2*6 Gi = 12288Mi fits strictly
			// — 2C/12Gi.
			expected: DeviceUnitResources{CPU: cores(2), RAM: gi(12)},
		},
		{
			// Only one of the two labels set: the read path requires both to
			// be non-empty, so this falls through to compute.
			name: "only cpu label set — falls through to compute path",
			labels: map[string]string{
				selfLabelKey + ".unit-cpu": "2000m",
			},
			allocCPU:    resource.MustParse("4"),
			allocRAM:    resource.MustParse("16Gi"),
			deviceCount: 1,
			expected:    DeviceUnitResources{CPU: cores(2), RAM: gi(12)},
		},
		{
			// T4(x4) 48C/192Gi: per-device share 11750m / 48640Mi after
			// the 1C/2Gi reservation. Suggested CPU = (11750-1)/1000 = 11;
			// ratios 8..5 induce >55Gi which exceeds the budget, 11*4 Gi =
			// 45056Mi fits strictly under 48640Mi.
			name:        "no labels — T4(x4) 48C/192Gi over 4 devices",
			allocCPU:    resource.MustParse("48"),
			allocRAM:    resource.MustParse("192Gi"),
			deviceCount: 4,
			expected:    DeviceUnitResources{CPU: cores(11), RAM: gi(44)},
		},
		{
			// User-reported scenario: 47810m / 192757188Ki over 4 devices.
			// availCPU=46810m, availRAM=186191Mi → per-device 11702m /
			// 46547Mi. Suggested CPU = (11702-1)/1000 = 11; 11*5 Gi =
			// 56320Mi exceeds 46547Mi, 11*4 Gi = 45056Mi fits — 11C/44Gi.
			name:        "no labels — 47810m / 192757188Ki over 4 devices",
			allocCPU:    resource.MustParse("47810m"),
			allocRAM:    resource.MustParse("192757188Ki"),
			deviceCount: 4,
			expected:    DeviceUnitResources{CPU: cores(11), RAM: gi(44)},
		},
		{
			// T4(x8) 96C/384Gi: per-device share 11875m / 48896Mi.
			// Suggested CPU = 11; 11*4 Gi = 45056Mi fits.
			name:        "no labels — T4(x8) 96C/384Gi over 8 devices",
			allocCPU:    resource.MustParse("96"),
			allocRAM:    resource.MustParse("384Gi"),
			deviceCount: 8,
			expected:    DeviceUnitResources{CPU: cores(11), RAM: gi(44)},
		},
		{
			// 64C/256Gi/8: per-device share 7875m / 32512Mi. Suggested CPU
			// = (7875-1)/1000 = 7; 7*5 Gi = 35840Mi exceeds 32512Mi, 7*4
			// Gi = 28672Mi fits — 7C/28Gi.
			name:        "no labels — 64C/256Gi over 8 devices",
			allocCPU:    resource.MustParse("64"),
			allocRAM:    resource.MustParse("256Gi"),
			deviceCount: 8,
			expected:    DeviceUnitResources{CPU: cores(7), RAM: gi(28)},
		},
		{
			// 4C/16Gi/1: per-device share 3000m / 14336Mi. Suggested CPU =
			// 2; 2*8/2*7 Gi both meet or exceed 14336Mi, 2*6 Gi = 12288Mi
			// fits strictly — 2C/12Gi.
			name:        "no labels — 4C/16Gi over 1 device",
			allocCPU:    resource.MustParse("4"),
			allocRAM:    resource.MustParse("16Gi"),
			deviceCount: 1,
			expected:    DeviceUnitResources{CPU: cores(2), RAM: gi(12)},
		},
		{
			// 4C/16Gi/4: per-device share 750m / 3584Mi — suggested CPU
			// reaches 0, so the default is returned.
			name:        "no labels — 4C/16Gi over 4 devices defaults",
			allocCPU:    resource.MustParse("4"),
			allocRAM:    resource.MustParse("16Gi"),
			deviceCount: 4,
			expected:    defaultUnits,
		},
		{
			// Reservation drains a tiny host completely; avail clamps to 0
			// and the default is returned.
			name:        "no labels — reservation exceeds allocatable",
			allocCPU:    resource.MustParse("1"),
			allocRAM:    resource.MustParse("1Gi"),
			deviceCount: 1,
			expected:    defaultUnits,
		},
		{
			// deviceCount<=0 collapses to 1 so callers don't divide by zero.
			name:        "no labels — deviceCount=0 treated as 1",
			allocCPU:    resource.MustParse("4"),
			allocRAM:    resource.MustParse("16Gi"),
			deviceCount: 0,
			expected:    DeviceUnitResources{CPU: cores(2), RAM: gi(12)},
		},
		{
			// Zero allocatable: avail clamps to 0, no positive suggestion,
			// default.
			name:        "no labels — zero allocatable",
			deviceCount: 1,
			expected:    defaultUnits,
		},
		{
			// CPU budget exactly on an integer-core boundary: the (x-1)/1000
			// form yields one core less than the strict floor, leaving
			// headroom. 13C/64Gi/1 → per-device 12000m / 63488Mi → suggested
			// CPU 11 → 11*6 Gi = 67584Mi exceeds 63488Mi, 11*5 Gi = 56320Mi
			// fits.
			name:        "no labels — exact-integer CPU yields headroom",
			allocCPU:    resource.MustParse("13"),
			allocRAM:    resource.MustParse("64Gi"),
			deviceCount: 1,
			expected:    DeviceUnitResources{CPU: cores(11), RAM: gi(55)},
		},
		{
			// CPU plentiful but RAM is tight relative to the smallest ratio:
			// 100C/10Gi/1 → per-device 99000m / 8192Mi → suggested CPU 98 →
			// 98*2 Gi = 200704Mi exceeds 8192Mi, all ratios fail, default.
			name:        "no labels — ram too tight for any ratio",
			allocCPU:    resource.MustParse("100"),
			allocRAM:    resource.MustParse("10Gi"),
			deviceCount: 1,
			expected:    defaultUnits,
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			node := &core.Node{
				ObjectMeta: meta.ObjectMeta{Labels: cs.labels},
				Status: core.NodeStatus{
					Allocatable: core.ResourceList{
						core.ResourceCPU:    cs.allocCPU,
						core.ResourceMemory: cs.allocRAM,
					},
				},
			}
			actual := GetDeviceUnitResources(node, nodeKey, cs.deviceCount)
			assert.Equal(t, cs.expected, actual, "unexpected per-device units")
		})
	}
}
