package devicefeature

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
					FeatureLabelPrefix + "nvidia-tesla-t4": "true",
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
			expected: NodeFeature{
				NodeLabels: map[string]string{
					FeatureLabelPrefix + "nvidia-tesla-t4": "true",
				},
				Tolerations: []core.Toleration{
					{
						Key:      DeviceLabelPrefix + "acclerator.sliced",
						Operator: core.TolerationOpEqual,
						Value:    "2",
						Effect:   core.TaintEffectNoSchedule,
					},
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
