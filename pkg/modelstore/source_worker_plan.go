package modelstore

import (
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// ManufacturerMixed marks a node whose acceleratable keys name more than one manufacturer: it
// has no implicit source image, and the caller refuses it rather than picking a side.
const ManufacturerMixed = "mixed"

// NodeSourceManufacturer reads the node's source-transport manufacturer: the single
// manufacturer its acceleratable keys name, NVIDIA for a node with no accelerator at all —
// the host-memory source path is the no-device candidate, which is a routing fact about where
// a source can run, not a hardware qualification — and ManufacturerMixed when the keys
// disagree. Manufacturers come from node features, never from host runtime versions.
func NodeSourceManufacturer(node *core.Node) string {
	manufacturer := ""
	for _, key := range nodefeature.ExtractAcceleratableNodeKeys(node) {
		manu, _, _ := strings.Cut(key, "-")
		if manufacturer != "" && manufacturer != manu {
			return ManufacturerMixed
		}
		manufacturer = manu
	}
	if manufacturer == "" {
		return nodefeature.ManufacturerNVIDIA
	}

	return manufacturer
}

// SourceWorkerPlan selects source images independently from disk warm-up images, per target
// node's manufacturer. Build it with PlanSourceWorkers; the image map comes in already parsed
// from its Setting, so this package needs no worker settings import.
type SourceWorkerPlan struct {
	images        map[string]string
	manufacturers map[string]string
}

// PlanSourceWorkers maps every target node to its source manufacturer. A mixed-manufacturer
// target is refused by omission — it carries no entry, so nothing on it is ever resolved — and
// no cross-vendor fallback exists: an image missing for a node's manufacturer stays missing.
func PlanSourceWorkers(images map[string]string, nodes []core.Node, targets []string) SourceWorkerPlan {
	plan := SourceWorkerPlan{images: images, manufacturers: map[string]string{}}
	onTarget := make(map[string]bool, len(targets))
	for _, target := range targets {
		onTarget[target] = true
	}
	for i := range nodes {
		node := &nodes[i]
		if !onTarget[node.Name] {
			continue
		}
		if manufacturer := NodeSourceManufacturer(node); manufacturer != ManufacturerMixed {
			plan.manufacturers[node.Name] = manufacturer
		}
	}

	return plan
}

// Resolve returns the configured image and the currently qualified device requests for the
// node's manufacturer. Ascend keeps its device-backed path until the host-only transport is
// qualified; a manufacturer with no image — or a node with no entry, mixed included — resolves
// to not-ok.
func (p SourceWorkerPlan) Resolve(node string) (string, core.ResourceList, bool) {
	manufacturer := p.manufacturers[node]
	image := p.images[manufacturer]
	if image == "" {
		return "", nil, false
	}
	if manufacturer == nodefeature.ManufacturerAscend {
		return image, core.ResourceList{
			nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerAscend,
				workercore.DeviceAllocationModeExclusive): resource.MustParse("1"),
			nodefeature.GetRDMAResourceName(workercore.DeviceAllocationModeExclusive): resource.MustParse("1"),
		}, true
	}

	return image, nil, true
}
