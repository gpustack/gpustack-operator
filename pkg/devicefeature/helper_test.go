package devicefeature

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/systemname"
)

func TestConstructNodeLabels(t *testing.T) {
	nodeName := "node1"
	cpuCapacity := *resource.NewQuantity(4, resource.DecimalSI)
	ramCapacity := *resource.NewQuantity(16*1024*1024*1024, resource.BinarySI)
	localStorageCapacity := *resource.NewQuantity(100*1024*1024*1024, resource.BinarySI)

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
				FeatureLabelPrefix + "nvidia-tesla-t4.cpu":          "1",
				FeatureLabelPrefix + "nvidia-tesla-t4.ram":          "4Gi",
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
				FeatureLabelPrefix + "nvidia-h100.cpu":          "4",
				FeatureLabelPrefix + "nvidia-h100.ram":          "16Gi",
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
				FeatureLabelPrefix + "nvidia-tesla-t4.cpu":          "4",
				FeatureLabelPrefix + "nvidia-tesla-t4.ram":          "16Gi",
				FeatureLabelPrefix + "amd":                          "true",
				FeatureLabelPrefix + "amd-mi300x":                   "true",
				FeatureLabelPrefix + "amd-mi300x.product":           "MI300X",
				FeatureLabelPrefix + "amd-mi300x.memory":            "192Gi",
				FeatureLabelPrefix + "amd-mi300x.cores":             "0",
				FeatureLabelPrefix + "amd-mi300x.accelerators":      "2",
				FeatureLabelPrefix + "amd-mi300x.cpu":               "2",
				FeatureLabelPrefix + "amd-mi300x.ram":               "8Gi",
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
				FeatureLabelPrefix + "nvidia-h100.cpu":          "4",
				FeatureLabelPrefix + "nvidia-h100.ram":          "16Gi",
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
				FeatureLabelPrefix + "nvidia-h100.cpu":          "4",
				FeatureLabelPrefix + "nvidia-h100.ram":          "16Gi",
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
				FeatureLabelPrefix + "nvidia-h100.cpu":          "4",
				FeatureLabelPrefix + "nvidia-h100.ram":          "16Gi",
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
						FeatureLabelPrefix + "nvidia-tesla-t4.cpu":          "1",
						FeatureLabelPrefix + "nvidia-tesla-t4.ram":          "4Gi",
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
	ramCapacity := *resource.NewQuantity(16*1024*1024*1024, resource.BinarySI)
	localStorageCapacity := *resource.NewQuantity(100*1024*1024*1024, resource.BinarySI)

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
						FeatureLabelPrefix + "nvidia-tesla-t4.cpu":          "1",
						FeatureLabelPrefix + "nvidia-tesla-t4.ram":          "4Gi",
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
					systemname.ManagedLabelKey:                 "true",
					FeatureLabelPrefix + "nvidia-tesla-t4":     "true",
					FeatureLabelPrefix + "nvidia-tesla-t4.cpu": "1",
					FeatureLabelPrefix + "nvidia-tesla-t4.ram": "4Gi",
				},
				Sliced:            "",
				Manufacturer:      "nvidia",
				Product:           "Tesla-T4",
				Memory:            "15Gi",
				Cores:             "2560",
				ComputeCapability: "7.5",
				Family:            "Turing",
				Accelerator:       *resource.NewQuantity(2, resource.DecimalSI),
				CPU:               cpuCapacity,
				RAM:               ramCapacity,
				LocalStorage:      localStorageCapacity,
				UnitResources: DeviceUnitResources{
					ActualCPU:  "2000m",
					ActualRAM:  "8192Mi",
					DisplayCPU: "2",
					DisplayRAM: "8Gi",
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
						FeatureLabelPrefix + "nvidia-tesla-t4.cpu":          "1",
						FeatureLabelPrefix + "nvidia-tesla-t4.ram":          "4Gi",
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
					systemname.ManagedLabelKey:                 "true",
					FeatureLabelPrefix + "nvidia-tesla-t4":     "true",
					FeatureLabelPrefix + "nvidia-tesla-t4.cpu": "1",
					FeatureLabelPrefix + "nvidia-tesla-t4.ram": "4Gi",
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
				CPU:               cpuCapacity,
				RAM:               ramCapacity,
				LocalStorage:      localStorageCapacity,
				UnitResources: DeviceUnitResources{
					ActualCPU:  "2000m",
					ActualRAM:  "8192Mi",
					DisplayCPU: "2",
					DisplayRAM: "8Gi",
				},
			},
		},
		{
			// Four allocatable accelerators on the same 4C/16Gi host —
			// per-device units should fall to 1000m / 4096Mi (1 / 4Gi display).
			name: "four allocatable accelerators",
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
						FeatureLabelPrefix + "nvidia-tesla-t4.cpu":          "1",
						FeatureLabelPrefix + "nvidia-tesla-t4.ram":          "4Gi",
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
					systemname.ManagedLabelKey:                 "true",
					FeatureLabelPrefix + "nvidia-tesla-t4":     "true",
					FeatureLabelPrefix + "nvidia-tesla-t4.cpu": "1",
					FeatureLabelPrefix + "nvidia-tesla-t4.ram": "4Gi",
				},
				Manufacturer: "nvidia",
				Product:      "Tesla-T4",
				Memory:       "15Gi",
				Cores:        "2560",
				Accelerator:  *resource.NewQuantity(4, resource.DecimalSI),
				CPU:          cpuCapacity,
				RAM:          ramCapacity,
				LocalStorage: localStorageCapacity,
				UnitResources: DeviceUnitResources{
					ActualCPU:  "1000m",
					ActualRAM:  "4096Mi",
					DisplayCPU: "1",
					DisplayRAM: "4Gi",
				},
			},
		},
		{
			// Zero allocatable accelerators (e.g. all pre-assigned): the
			// GetDeviceUnitResources call falls back to n=1, so each "device" reports
			// the whole host's per-device units.
			name: "zero allocatable accelerators falls back to host budget",
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
						FeatureLabelPrefix + "nvidia-tesla-t4.cpu":          "1",
						FeatureLabelPrefix + "nvidia-tesla-t4.ram":          "4Gi",
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
					systemname.ManagedLabelKey:                 "true",
					FeatureLabelPrefix + "nvidia-tesla-t4":     "true",
					FeatureLabelPrefix + "nvidia-tesla-t4.cpu": "1",
					FeatureLabelPrefix + "nvidia-tesla-t4.ram": "4Gi",
				},
				Manufacturer: "nvidia",
				Product:      "Tesla-T4",
				Memory:       "15Gi",
				Cores:        "2560",
				Accelerator:  *resource.NewQuantity(0, resource.DecimalSI),
				CPU:          cpuCapacity,
				RAM:          ramCapacity,
				LocalStorage: localStorageCapacity,
				UnitResources: DeviceUnitResources{
					ActualCPU:  "4000m",
					ActualRAM:  "16384Mi",
					DisplayCPU: "4",
					DisplayRAM: "16Gi",
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
	cases := []struct {
		name        string
		cpu         resource.Quantity
		ram         resource.Quantity
		deviceCount int64
		expected    DeviceUnitResources
	}{
		{
			name:        "even split: 4C / 16Gi / 4 devices",
			cpu:         resource.MustParse("4"),
			ram:         resource.MustParse("16Gi"),
			deviceCount: 4,
			expected: DeviceUnitResources{
				ActualCPU:  "1000m",
				ActualRAM:  "4096Mi",
				DisplayCPU: "1",
				DisplayRAM: "4Gi",
			},
		},
		{
			name:        "single device gets whole node: 4C / 16Gi / 1 device",
			cpu:         resource.MustParse("4"),
			ram:         resource.MustParse("16Gi"),
			deviceCount: 1,
			expected: DeviceUnitResources{
				ActualCPU:  "4000m",
				ActualRAM:  "16384Mi",
				DisplayCPU: "4",
				DisplayRAM: "16Gi",
			},
		},
		{
			// T4(x4): 48C / 192Gi / 4 devices.
			name:        "T4(x4) 48C/192Gi over 4 devices",
			cpu:         resource.MustParse("48"),
			ram:         resource.MustParse("192Gi"),
			deviceCount: 4,
			expected: DeviceUnitResources{
				ActualCPU:  "12000m",
				ActualRAM:  "49152Mi",
				DisplayCPU: "12",
				DisplayRAM: "48Gi",
			},
		},
		{
			// T4(x8): 96C / 384Gi / 8 devices — same per-device units as T4(x4).
			name:        "T4(x8) 96C/384Gi over 8 devices",
			cpu:         resource.MustParse("96"),
			ram:         resource.MustParse("384Gi"),
			deviceCount: 8,
			expected: DeviceUnitResources{
				ActualCPU:  "12000m",
				ActualRAM:  "49152Mi",
				DisplayCPU: "12",
				DisplayRAM: "48Gi",
			},
		},
		{
			// 24314504Ki = 23744.6 MiB -> floored to 23744 MiB; display ceils
			// 23744 MiB to 24 GiB.
			name:        "RAM floor: 24314504Ki -> 23744Mi actual / 24Gi display, single device",
			cpu:         resource.MustParse("10"),
			ram:         resource.MustParse("24314504Ki"),
			deviceCount: 1,
			expected: DeviceUnitResources{
				ActualCPU:  "10000m",
				ActualRAM:  "23744Mi",
				DisplayCPU: "10",
				DisplayRAM: "24Gi",
			},
		},
		{
			// Actual floor prevents overcommit: 4*2500m = 10000m <= 10C,
			// 4*5888Mi = 23552Mi <= 23Gi. Display ceils both per device.
			name:        "floor down: 10C / 23Gi over 4 devices",
			cpu:         resource.MustParse("10"),
			ram:         resource.MustParse("23Gi"),
			deviceCount: 4,
			expected: DeviceUnitResources{
				ActualCPU:  "2500m",
				ActualRAM:  "5888Mi",
				DisplayCPU: "3",
				DisplayRAM: "6Gi",
			},
		},
		{
			// Neighbor 8C host collapses to the same display 6Gi as the
			// 10C/23Gi case above (5888 Mi -> ceil 6Gi); actual CPU differs.
			name:        "neighbour host: 8C / 23Gi over 4 devices",
			cpu:         resource.MustParse("8"),
			ram:         resource.MustParse("23Gi"),
			deviceCount: 4,
			expected: DeviceUnitResources{
				ActualCPU:  "2000m",
				ActualRAM:  "5888Mi",
				DisplayCPU: "2",
				DisplayRAM: "6Gi",
			},
		},
		{
			// Sub-core actual stays in milli (10500m, no host-level rounding);
			// display ceils to 11 cores.
			name:        "fractional CPU: 10500m / 16Gi / 1 device",
			cpu:         resource.MustParse("10500m"),
			ram:         resource.MustParse("16Gi"),
			deviceCount: 1,
			expected: DeviceUnitResources{
				ActualCPU:  "10500m",
				ActualRAM:  "16384Mi",
				DisplayCPU: "11",
				DisplayRAM: "16Gi",
			},
		},
		{
			// 2C/2Gi over 4 devices: actual is 500m/512Mi (no minimum); display
			// clamps both up to the 1C / 1Gi floor.
			name:        "minimum on display only: 2C / 2Gi over 4 devices",
			cpu:         resource.MustParse("2"),
			ram:         resource.MustParse("2Gi"),
			deviceCount: 4,
			expected: DeviceUnitResources{
				ActualCPU:  "500m",
				ActualRAM:  "512Mi",
				DisplayCPU: "1",
				DisplayRAM: "1Gi",
			},
		},
		{
			// Zero-quantity host: actual stays at 0, display clamps to 1/1Gi.
			name:        "minimum on display only: zero quantities",
			cpu:         resource.Quantity{},
			ram:         resource.Quantity{},
			deviceCount: 1,
			expected: DeviceUnitResources{
				ActualCPU:  "0m",
				ActualRAM:  "0Mi",
				DisplayCPU: "1",
				DisplayRAM: "1Gi",
			},
		},
		{
			// deviceCount<=0 is treated as 1 so callers don't divide by zero.
			name:        "deviceCount=0 treated as 1",
			cpu:         resource.MustParse("4"),
			ram:         resource.MustParse("16Gi"),
			deviceCount: 0,
			expected: DeviceUnitResources{
				ActualCPU:  "4000m",
				ActualRAM:  "16384Mi",
				DisplayCPU: "4",
				DisplayRAM: "16Gi",
			},
		},
	}

	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			actual := GetDeviceUnitResources(cs.cpu, cs.ram, cs.deviceCount)
			assert.Equal(t, cs.expected, actual, "unexpected per-device units")
		})
	}
}
