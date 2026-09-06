package device

// TopologyDistance is how far apart two devices are, as an ordered level rather than a boolean.
//
// The vocabulary is the one the GPUStack runtime component already publishes
// (https://github.com/gpustack/runtime), reusing the words an operator reads out of a vendor
// topology matrix. It is copied rather than imported: that component is Python and is not a
// dependency of this module, so the two are held together only by the test that pins these values
// against its table.
//
// Eight values, of which six are distances between two DIFFERENT devices: TopologyDistanceSelf is a
// device against itself, and TopologyDistanceUnknown is the absence of an answer.
//
// The values are spaced rather than consecutive so a level discovered later can be given a number
// between two existing ones, leaving every persisted number valid.
//
// A smaller value is closer, so "at least this close" is one comparison.
type TopologyDistance int32

const (
	// TopologyDistanceSelf is the device itself.
	TopologyDistanceSelf TopologyDistance = 0
	// TopologyDistanceLink is a direct connection over the manufacturer's own high-speed
	// accelerator interconnect. It is NOT a bus distance, and it cannot be derived from bus
	// coordinates: accelerators at this distance can sit on different NUMA nodes, where every
	// bus-derived answer rates them furthest apart rather than closest.
	TopologyDistanceLink TopologyDistance = 5
	// TopologyDistancePIX is at most one PCIe bridge between the two devices.
	TopologyDistancePIX TopologyDistance = 10
	// TopologyDistancePXB is several PCIe bridges, without crossing the host bridge.
	TopologyDistancePXB TopologyDistance = 20
	// TopologyDistancePHB crosses the PCIe host bridge, typically the CPU.
	TopologyDistancePHB TopologyDistance = 30
	// TopologyDistanceNode crosses the interconnect between PCIe host bridges WITHIN one NUMA node.
	// It is the furthest two devices can be while still sharing a NUMA node, which is why it ranks
	// closer than TopologyDistanceSys and not further: the name is the vendor matrix's word for the
	// NUMA node the pair shares, never for a boundary between two of them.
	TopologyDistanceNode TopologyDistance = 40
	// TopologyDistanceSys crosses the SMP interconnect BETWEEN NUMA nodes, the vendor matrix's
	// QPI/UPI hop.
	TopologyDistanceSys TopologyDistance = 50
	// TopologyDistanceUnknown is the absence of an answer. It is deliberately far from every real
	// level so that it can never be mistaken for one, and it must not be normalised to a distance:
	// "we could not tell" and "they are far apart" are different facts.
	TopologyDistanceUnknown TopologyDistance = 100
)

// String renders the level as the word an operator already reads out of a vendor topology matrix.
//
// A value outside the vocabulary renders as UNK. The rendering reaches a node label, so returning
// an empty string would publish a key whose value means nothing, which is worse than honestly
// reporting that the distance is unknown.
func (in TopologyDistance) String() string {
	switch in {
	case TopologyDistanceSelf:
		return "SELF"
	case TopologyDistanceLink:
		return "LINK"
	case TopologyDistancePIX:
		return "PIX"
	case TopologyDistancePXB:
		return "PXB"
	case TopologyDistancePHB:
		return "PHB"
	case TopologyDistanceNode:
		return "NODE"
	case TopologyDistanceSys:
		return "SYS"
	default:
		return "UNK"
	}
}

// BusDistance reports how far an accelerator is from a network interface, derived from the bus
// coordinates both of them publish.
//
// It answers on the bus axis only, and therefore never returns TopologyDistanceLink or
// TopologyDistanceSelf: a manufacturer's own high-speed interconnect is invisible here, and an
// accelerator is never the same device as a NIC.
//
// TopologyDistancePHB is never returned either, and that absence is a measurement limit rather
// than an omission. Telling PHB from NODE requires seeing whether the two devices share a PCIe
// host bridge, and the coordinates published here stop at the outermost PCI bridge — the host
// bridge's own component is not among them. Where the two are indistinguishable this reports the
// FURTHER of them, because the value feeds a proximity claim and overclaiming closeness is the
// error that cannot be caught downstream.
//
// A device with no PCI coordinates yields TopologyDistanceUnknown rather than a distance. An
// interface on a non-PCI interconnect is the case this matters for, and it is a kind of interface
// rather than a failed lookup — so "we cannot tell" is the answer, never "they are far apart".
func BusDistance(accelerator Topology, iface Interface) TopologyDistance {
	if accelerator.PciBusID == "" || iface.PciBusID == "" ||
		accelerator.PciRootID == "" || iface.PciRootID == "" {
		return TopologyDistanceUnknown
	}

	// The innermost bridge is the tightest shared scope the coordinates can express: two devices
	// behind it are one hop apart.
	if len(accelerator.PciSwitches) > 0 && len(iface.PciSwitches) > 0 &&
		accelerator.PciSwitches[0] == iface.PciSwitches[0] {
		return TopologyDistancePIX
	}

	// PciRootID is the OUTERMOST bridge, so sharing it means the path stayed inside one bridge
	// subtree while diverging somewhere below it.
	if accelerator.PciRootID == iface.PciRootID {
		return TopologyDistancePXB
	}

	// Beyond the bridge subtree, NUMA is the only coordinate left. An unknown NUMA on either side
	// cannot be read as "the same node": treating it as node 0 would assert an affinity the kernel
	// denied.
	//
	// Both sides now encode "unknown" the same way, and that is what makes this guard sufficient
	// rather than half of one. The interface side always mapped the kernel's `-1` sentinel to
	// empty; the ACCELERATOR side used to return "0" for node 0, for `-1` and for an unparseable
	// reading alike, so an accelerator with no affinity arrived here indistinguishable from one on
	// node 0 and this comparison answered NODE against an interface genuinely on node 0. The fix
	// could not live here — the information was gone before the call — so it is in the shared bus
	// helper (`binding.GetNumaNodeByBDF`), which now reports unknown as unknown.
	if accelerator.NumaAffinity == "" || iface.NumaAffinity == "" {
		return TopologyDistanceUnknown
	}
	if accelerator.NumaAffinity == iface.NumaAffinity {
		return TopologyDistanceNode
	}
	return TopologyDistanceSys
}
