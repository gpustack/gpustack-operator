package modelstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func manufacturerNode(name, keys string) core.Node {
	labels := map[string]string{}
	if keys != "" {
		labels[nodefeature.AcceleratableFeatureLabelPrefix+keys] = "true"
	}
	return core.Node{ObjectMeta: meta.ObjectMeta{Name: name, Labels: labels}}
}

func TestPlanSourceWorkers(t *testing.T) {
	images := map[string]string{
		nodefeature.ManufacturerNVIDIA: "registry.example/mt:nvidia",
		nodefeature.ManufacturerAscend: "registry.example/mt:ascend",
	}
	nodes := []core.Node{
		// manufacturer:nvidia-0 carries an NVIDIA accelerator key.
		manufacturerNode("nvidia-node", "nvidia-0"),
		manufacturerNode("ascend-node", "ascend-0"),
		// Two keys of one manufacturer are still one manufacturer's node.
		func() core.Node {
			nd := manufacturerNode("twin-nvidia-node", "nvidia-0")
			nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-1"] = "true"
			return nd
		}(),
		// No accelerator at all: the host-memory source path routes through the NVIDIA image.
		manufacturerNode("cpu-node", ""),
		// Keys of two manufacturers have no implicit selection.
		func() core.Node {
			nd := manufacturerNode("mixed-node", "nvidia-0")
			nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"ascend-0"] = "true"
			return nd
		}(),
	}
	targets := []string{"nvidia-node", "ascend-node", "twin-nvidia-node", "cpu-node", "mixed-node", "ghost-node"}

	plan := PlanSourceWorkers(images, nodes, targets)

	t.Run("single-manufacturer targets resolve their own image", func(t *testing.T) {
		image, _, ok := plan.Resolve("nvidia-node")
		require.True(t, ok)
		assert.Equal(t, "registry.example/mt:nvidia", image)

		image, _, ok = plan.Resolve("ascend-node")
		require.True(t, ok)
		assert.Equal(t, "registry.example/mt:ascend", image)
	})

	t.Run("accelerator-free target keeps the NVIDIA host-memory candidate", func(t *testing.T) {
		image, devices, ok := plan.Resolve("cpu-node")
		require.True(t, ok)
		assert.Equal(t, "registry.example/mt:nvidia", image,
			"a no-device node is a host-memory source candidate, not a hardware qualification")
		assert.Empty(t, devices, "the CUDA source path is device-free")
	})

	t.Run("mixed manufacturer is refused, not resolved to a side", func(t *testing.T) {
		_, _, ok := plan.Resolve("mixed-node")
		assert.False(t, ok, "no cross-vendor fallback exists")
	})

	t.Run("a manufacturer with no image resolves not-ok", func(t *testing.T) {
		plan := PlanSourceWorkers(map[string]string{nodefeature.ManufacturerNVIDIA: "registry.example/mt:nvidia"},
			nodes, targets)
		_, _, ok := plan.Resolve("ascend-node")
		assert.False(t, ok, "an unconfigured manufacturer stays unconfigured")
	})

	t.Run("an off-target node resolves nothing", func(t *testing.T) {
		_, _, ok := plan.Resolve("ghost-node")
		assert.False(t, ok)
	})

	t.Run("ascend carries the qualified device requests", func(t *testing.T) {
		_, devices, ok := plan.Resolve("ascend-node")
		require.True(t, ok)
		one := resource.MustParse("1")
		assert.Equal(t, core.ResourceList{
			nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerAscend,
				workercore.DeviceAllocationModeExclusive): one,
			nodefeature.GetRDMAResourceName(workercore.DeviceAllocationModeExclusive): one,
		}, devices)
	})
}

func ptrNode(nd core.Node) *core.Node { return &nd }

func TestNodeSourceManufacturer(t *testing.T) {
	assert.Equal(t, nodefeature.ManufacturerNVIDIA, NodeSourceManufacturer(ptrNode(manufacturerNode("cpu", ""))),
		"no accelerator is the host-memory NVIDIA candidate")
	assert.Equal(t, "nvidia", NodeSourceManufacturer(ptrNode(manufacturerNode("gpu", "nvidia-3"))))
	assert.Equal(t, "nvidia", NodeSourceManufacturer(ptrNode(func() core.Node {
		nd := manufacturerNode("twin", "nvidia-3")
		nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-4"] = "true"
		return nd
	}())), "one manufacturer over several keys is still one manufacturer")
	assert.Equal(t, ManufacturerMixed, NodeSourceManufacturer(ptrNode(func() core.Node {
		nd := manufacturerNode("mixed", "nvidia-3")
		nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"ascend-0"] = "true"
		return nd
	}())))
}
