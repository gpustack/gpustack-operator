package main

import (
	"fmt"
	"strconv"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const (
	// kueueTransformationsBlockName names the Kueue managerConfig
	// "resources.transformations" block.
	kueueTransformationsBlockName = "kueue-transformations"
	// nfdPciVendorIDsBlockName names the NodeFeatureRule PCI vendor ID list block.
	nfdPciVendorIDsBlockName = "nfd-pci-vendor-ids"
)

// blocks returns every block this command generates, in a stable order.
func blocks() []Block {
	return []Block{
		{Name: kueueTransformationsBlockName, Content: kueueTransformations()},
		{Name: nfdPciVendorIDsBlockName, Content: nfdPciVendorIDs()},
	}
}

// kueueTransformations renders the Kueue managerConfig "resources.transformations"
// list: one Replace rule per accelerator manufacturer pkg/nodefeature knows,
// converting its exclusive/shared/sliced-units/partitioned-units resource keys
// into that manufacturer's single credits resource, on the integer credit basis
// pkg/nodefeature defines. CreditsPerCard equals ResourceMaxUnits, so an exclusive
// (whole-card) request is worth CreditsPerCard credits, a shared ownership is worth
// CreditsPerCard/SharedResourceMaxSize, and each fine-grained counting-key unit
// (sliced or partitioned) is worth CreditsPerCard/ResourceMaxUnits credits before
// its family-specific multiplyBy is applied.
//
// The list covers every manufacturer nodefeature knows, regardless of which ones a
// cluster's "manufacturers" value enables: Kueue never sees a disabled
// manufacturer's resource requested, so its rule is simply inert.
func kueueTransformations() string {
	const (
		exclusiveFactor = nodefeature.CreditsPerCard
		sharedFactor    = nodefeature.CreditsPerCard / nodefeature.SharedResourceMaxSize
		unitsFactor     = nodefeature.CreditsPerCard / nodefeature.ResourceMaxUnits
	)

	var b strings.Builder
	b.WriteString("transformations:\n")
	for _, manu := range nodefeature.GetKnownAcceleratableManufacturers() {
		credits := string(nodefeature.GetAcceleratableCreditsResourceName(manu))
		exclusive := string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeExclusive))
		shared := string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeShared))
		sliced := string(nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeSliced))
		slicedUnits := string(nodefeature.GetAcceleratableSlicedUnitsResourceName(manu))

		fmt.Fprintf(&b, "- input: %s\n  strategy: Replace\n  outputs:\n    %s: %q\n",
			exclusive, credits, strconv.Itoa(exclusiveFactor))
		fmt.Fprintf(&b, "- input: %s\n  strategy: Replace\n  outputs:\n    %s: %q\n",
			shared, credits, strconv.Itoa(sharedFactor))
		fmt.Fprintf(&b, "- input: %s\n  strategy: Replace\n  multiplyBy: %s\n  outputs:\n    %s: %q\n",
			slicedUnits, sliced, credits, strconv.Itoa(unitsFactor))

		// Physical partitions (MIG) count disjoint cards from the logical/sliced
		// family, so a manufacturer with no partition kind gets no rule here at all.
		if partitionedUnits := string(nodefeature.GetAcceleratablePartitionedUnitsResourceName(manu)); partitionedUnits != "" {
			fmt.Fprintf(&b, "- input: %s\n  strategy: Replace\n  outputs:\n    %s: %q\n",
				partitionedUnits, credits, strconv.Itoa(unitsFactor))
		}
	}
	return b.String()
}

// nfdPciVendorIDs renders the PCI vendor ID list the operator chart's own
// NodeFeatureRule matches against to detect an acceleratable device, annotated
// with the manufacturer each ID belongs to for readability. The PCI device-class
// prefixes the same rule matches on are not manufacturer data — they classify a
// PCI device's function (display/3D/accelerator), not its vendor — so they stay a
// plain, hand-maintained default in the chart's own values.
func nfdPciVendorIDs() string {
	nameByID := make(map[string]string)
	for _, manu := range nodefeature.GetKnownAcceleratableManufacturers() {
		nameByID[nodefeature.GetPciVendorID(manu)] = manu
	}

	var b strings.Builder
	b.WriteString("pciVendorIDs:\n")
	for _, id := range nodefeature.GetAcceleratablePciVendorIDs() {
		fmt.Fprintf(&b, "- %q # %s\n", id, nameByID[id])
	}
	return b.String()
}
