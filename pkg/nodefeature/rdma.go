package nodefeature

import (
	"sort"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

const (
	// RDMAFeatureLabelPrefix prefixes every label this file writes, and is what makes the set
	// removable as a set: a pass that no longer reports one of these keys needs the stale key
	// deleted, and a prefix is the only way to know which keys were ours.
	RDMAFeatureLabelPrefix = FeatureLabelPrefix + "rdma."

	// NodeRDMACapableLabelKey says this node has at least one RDMA interface whose link was not
	// found broken.
	//
	// It is the ONE key of this set a selector can pin; the other two are informational. No
	// operator-generated ResourceFlavor pins it: a workload that needs a usable link selects on it
	// through its own nodeSelector or affinity.
	//
	// Node-level, so it loses information on purpose. "Some interface on this node has a working
	// link" is not "the accelerators this workload gets are near a working link", and for a sliced
	// or shared accelerator the fraction handed out need not be the close one. A consumer needing
	// the finer answer reads the Devices object, which carries the per-interface truth.
	NodeRDMACapableLabelKey = RDMAFeatureLabelPrefix + "capable"

	// NodeRDMADistanceLabelKey is the CLOSEST bus distance any accelerator on this node has to an
	// RDMA-capable interface, as one of the distance vocabulary's words.
	//
	// Informational only. The selector is equality-matched, so an ordered value is readable through
	// it but not comparable: "at least NUMA-level" cannot be expressed, and only "distance == PIX"
	// can be asked. Publishing it costs nothing and answers an operator's question; it cannot
	// answer a scheduler's.
	NodeRDMADistanceLabelKey = RDMAFeatureLabelPrefix + "distance"

	// NodeRDMANumaLabelKey is the set of NUMA nodes carrying an RDMA-capable interface, ordered
	// and joined with an underscore. A node with RDMA on both sockets carries both values.
	//
	// The separator is an underscore because a COMMA IS NOT A VALID LABEL VALUE CHARACTER. A
	// comma-joined value does not fail validation — it is silently stripped by the sanitizer every
	// other label value goes through, so {0,1} would publish as "01" and read as NUMA node 01. A
	// dash was rejected in turn because "0-1" reads as a range rather than a pair. The rule this
	// leaves behind is the general one: a sanitizer applied to a COMPOSITE value destroys its
	// structure without complaining, so the value has to be valid before it is sanitized.
	//
	// Informational only, for the same reason as the distance above.
	NodeRDMANumaLabelKey = RDMAFeatureLabelPrefix + "numa"

	// rdmaNumaSeparator joins the NUMA node ids. See NodeRDMANumaLabelKey for why it is not a
	// comma, which is the separator this would otherwise obviously use.
	rdmaNumaSeparator = "_"

	// NodeRDMASoftwareLabelKey says that every usable RDMA interface on this node is
	// software-carried, as DeviceInterface.RDMASoftware marks one. It is set only when at least one
	// interface is usable, so a node whose only software port is down carries neither this key nor
	// the capable key.
	//
	// It means software ONLY. A node with a usable hardware adapter beside a software device does
	// not carry it: a reader that excludes software RDMA must not lose a node that has hardware.
	//
	// Software RDMA performs far below a hardware adapter, so a workload that needs hardware
	// excludes this key through its own nodeSelector or affinity. No operator-generated
	// ResourceFlavor pins it, the same as the capable key.
	NodeRDMASoftwareLabelKey = RDMAFeatureLabelPrefix + "software"

	// NodeRDMAExternalLabelKey says this node has an RDMA device under another owner's allocation,
	// one GPUStack never hands out, as device.RDMADeviceOwnerExternal marks the device. It is set
	// on ownership evidence alone: the device's driver names another owner, today the AWS EFA
	// driver, and a usable interface is not required. An EFA device sits on a PCI function with no
	// netdev, so no interface record names it, and a node whose only RDMA is that device carries
	// this key with neither capable nor software.
	//
	// It answers a different question from NodeRDMASoftwareLabelKey: that key says the RDMA
	// GPUStack hands out on this node is software, this one says there is RDMA here GPUStack never
	// allocates. Both may be true at once — an EFA device beside a software one publishes capable,
	// software and external — because hiding the software fact on the strength of an unallocatable
	// device would send a workload that needs hardware RDMA to a node whose only allocatable RDMA
	// is the software one.
	//
	// Informational only, and pinned by no operator-generated ResourceFlavor. It never changes the
	// other four keys: those are reduced over the interfaces alone, so an external device beside a
	// usable one moves none of them.
	NodeRDMAExternalLabelKey = RDMAFeatureLabelPrefix + "external"
)

// rdmaResourceNameBase is the base of the RDMA resource keys: the operator's own device
// prefix, under which every node-level resource this operator advertises itself lives, plus
// the family name. It carries no manufacturer, because a network interface belongs to the
// node rather than to a vendor.
const rdmaResourceNameBase = VisibilityResourceNamePrefix + "rdma"

// RDMAEndpointPoolSize is how many concurrent holders one RDMA endpoint is advertised for in the
// shared family.
//
// The figure is taken from the ecosystem this replaces rather than invented here: the shared-RDMA
// device plugin an operator would otherwise install ships 63 as the documented default in its
// vendor's own deployment guide, so a workload written against that plugin meets no tighter
// ceiling here. 64 is that number at the next power of two, which is the shape the rest of this
// package's sizes take.
//
// What it is not:
//
//   - Not a hardware limit. An HCA carries far more queue pairs than this, and several processes
//     using one at once is ordinary RDMA practice, with the isolation done by the firmware and the
//     kernel rather than by how many tokens kubelet was handed. The reference default is a
//     configuration choice too, which is why matching it is a compatibility argument and not a
//     physical one.
//   - Not a quota. These keys are node-level resources the quota chain never classifies as an
//     accelerator family, so nothing downstream reads this number into a capacity or an admission
//     decision.
//
// Raising it costs nothing but permitting more containers onto one endpoint, which is what an
// endpoint is for. Deriving it from a machine's shape is the thing to avoid: a figure reached by
// multiplying one node's card count stops being right on the next node, and it fails silently, the
// endpoint simply ceasing to be schedulable partway up a denser machine.
const RDMAEndpointPoolSize = 64

// GetRDMAResourceName returns the node-level RDMA resource key the given allocation mode is
// served under. There are three: "device.gpustack.ai/rdma", "device.gpustack.ai/rdma.shared"
// and "device.gpustack.ai/rdma.partitioned".
//
// Sliced names no key, and that absence is the deliberate part. It once returned a ".sliced"
// key whose contract was word for word the shared one, over the same HCAs, with the two not
// decrementing each other. Two keys over one pool is not what two extended resources mean to a
// scheduler: one endpoint then admitted a full complement of holders under each name, so the
// ceiling either key appeared to set was not a ceiling at all. The symmetry it bought -- letting
// a workload spell its RDMA request with the same suffix as its accelerator request -- did not
// pay for a number that does not hold.
//
// Visibility names none either, for a different reason: a visibility allocation names an
// endpoint another container of the same Pod holds, and no RDMA allocation record is written to
// answer that from, so the mode is unserved rather than half-served. None and any unrecognized
// mode likewise return "", because an unknown mode must not silently name the whole-function key.
func GetRDMAResourceName(mode workercore.DeviceAllocationMode) core.ResourceName {
	switch mode {
	case workercore.DeviceAllocationModeExclusive:
		return rdmaResourceNameBase
	case workercore.DeviceAllocationModeShared:
		return rdmaResourceNameBase + SharedResourceNameSuffix
	case workercore.DeviceAllocationModePartitioned:
		return rdmaResourceNameBase + PartitionedResourceNameSuffix
	default:
		return ""
	}
}

// ConstructRDMANodeLabels constructs the RDMA feature labels from one detect pass's inventory.
// The devices argument is the pass's host verbs inventory, and it decides one key alone: the
// external-owner key, which is set on ownership evidence rather than on any usable endpoint.
//
// An endpoint counts as RDMA-capable when its link check did not conclude that the link is broken,
// falling back to whether a device is bound only when no verdict was reached at all. So `failed` is
// the one state that withholds NodeRDMACapableLabelKey, and `unverified` emits it — including the
// unverified record the detector writes for a tree it could not read, which carries no bound
// device. Withholding there would turn "this port's state could not be read" into "this node has no
// RDMA".
//
// The aggregation is EXISTENTIAL: the label is emitted when at least one endpoint is capable, not
// when every one is, so a node with a broken NIC beside a working one keeps it and can still serve
// an RDMA workload. Which endpoint is broken lives in `Devices`, per interface and per virtual
// function; a node label cannot express it.
//
// It returns a non-nil map that may carry only the external key — a node with no usable interface
// and an externally owned device is exactly that — and is otherwise empty for a node with no such
// interface and no such device. The caller's removal of stale keys is what makes that absence take
// effect — the labels are additive at the object they are written to, so a key that stops being
// reported does not stop existing.
func ConstructRDMANodeLabels(
	groups device.DevicesGroupList, interfaces []device.Interface, devices []device.RDMADevice,
) map[string]string {
	capable := rdmaEndpoints(interfaces)

	labels := map[string]string{}
	// The external key is decided on the verbs inventory alone, so it is set before the early
	// return below rather than after the locality logic: a node whose only RDMA is externally owned
	// has no usable endpoint, takes that return, and is the one node this key exists for. It never
	// feeds the other four keys, so computing it here changes none of them.
	for i := range devices {
		if devices[i].Owner == device.RDMADeviceOwnerExternal {
			labels[NodeRDMAExternalLabelKey] = "true"
			break
		}
	}
	if len(capable) == 0 {
		return labels
	}
	labels[NodeRDMACapableLabelKey] = "true"

	// A software-carried endpoint stays out of both locality values. The coordinates it may carry
	// are its netdev's, and they place no RDMA hardware an accelerator can sit near. Reducing over
	// the hardware endpoints alone keeps a software-only node free of both keys rather than
	// publishing an unknown distance as if it were a measurement.
	hardware := make([]device.Interface, 0, len(capable))
	for i := range capable {
		if !capable[i].RDMASoftware {
			hardware = append(hardware, capable[i])
		}
	}
	// Software ONLY: at least one usable endpoint, and every usable endpoint software-carried. The
	// early return above settles the first half; an empty set here settles the second.
	if len(hardware) == 0 {
		labels[NodeRDMASoftwareLabelKey] = "true"
	}

	// The NUMA set is a COMPOSITE value, so the sanitizer below is not a safety net for it: it caps
	// a label value at 63 characters, and truncating "0_1_..._31" publishes a SUBSET as if it were
	// the set — a wrong fact rather than a degraded one. A node with enough NUMA nodes to exceed the
	// cap therefore gets no NUMA key at all. `rdma.capable` still says the node has RDMA, and the
	// per-endpoint affinities are in `Devices`, which carries no such limit.
	if numa := rdmaNumaNodes(hardware); numa != "" && kubemeta.SanitizeLabelValue(numa) == numa {
		labels[NodeRDMANumaLabelKey] = numa
	}
	// Reported only when an accelerator was there to measure from. A distance is a statement about
	// a PAIR, so a node with RDMA and no accelerators has no distance rather than an unknown one.
	if distance, ok := closestAcceleratorDistance(groups, hardware); ok {
		labels[NodeRDMADistanceLabelKey] = distance.String()
	}

	for k := range labels {
		labels[k] = kubemeta.SanitizeLabelValue(labels[k])
	}
	return labels
}

// rdmaEndpoints lists this node's usable RDMA endpoints, one entry per interface and one per
// virtual function, so the values below are reduced over the things that are actually usable.
//
// A virtual function is its OWN endpoint rather than being folded into its parent, because it
// carries its own NUMA affinity: the sysfs reader answers `numa_node` from the VF's own device
// directory, and reducing over the parent would publish the parent's socket for a set of endpoints
// that never contributed to it. An earlier revision folded them in and justified it by claiming the
// VF type carries no coordinates of its own — it carries a bus id, a NUMA node and a CPU list.
//
// The PCI PATH is inherited from the parent, which is accurate rather than approximate: a virtual
// function sits behind the same bridges as the function it belongs to, so the distance to an
// accelerator is the parent's. An empty VF affinity leaves the parent's in place, because a blank
// reading is the kernel declining to answer and not a different answer.
//
// SR-IOV is why this matters at all: a VF is removed from the top-level inventory, so on the setups
// where RDMA reaches containers through virtual functions, these entries are every RDMA endpoint
// the node has.
func rdmaEndpoints(interfaces []device.Interface) []device.Interface {
	endpoints := make([]device.Interface, 0, len(interfaces))
	for i := range interfaces {
		iface := interfaces[i]
		if rdmaUsable(iface.RDMA, iface.Link) {
			endpoints = append(endpoints, iface)
		}

		for j := range iface.VirtualFunctions {
			vf := &iface.VirtualFunctions[j]
			if !rdmaUsable(vf.RDMA, vf.Link) {
				continue
			}
			endpoint := iface
			// Dropped so an endpoint cannot be walked into again by a future reader of this slice.
			endpoint.VirtualFunctions = nil
			if vf.NumaAffinity != "" {
				endpoint.NumaAffinity = vf.NumaAffinity
			}
			endpoints = append(endpoints, endpoint)
		}
	}

	return endpoints
}

// rdmaUsable applies the one rule both levels share: an RDMA endpoint whose link was not concluded
// broken.
//
// An EXPLICIT verdict outranks the `rdma` flag, and that ordering is the whole of this function.
// The detector records an RDMA tree that is present but unreadable as `unverified` while leaving
// `rdma` false — it will not claim a device it could not read — so testing `rdma` first turned "this
// port's state could not be read" into "this node has no RDMA" and withheld the label on the
// strength of a file that could not be opened. That is the one outcome this feature's every other
// unreadable-read rule exists to prevent, and it was reached here from the other direction.
//
// Only the no-verdict case falls back to `rdma`: nothing was concluded, so a bound device counts and
// a plain NIC does not.
func rdmaUsable(rdma bool, link *workercore.DeviceInterfaceLink) bool {
	if link != nil {
		return link.State != workercore.DeviceInterfaceLinkStateFailed
	}
	return rdma
}

// rdmaNumaNodes lists the NUMA nodes carrying an RDMA-capable interface, ordered and joined with
// the separator NodeRDMANumaLabelKey documents.
//
// Ordered because the value is published and compared: an unordered set would differ between passes
// that found the same hardware, and every such pass would write.
func rdmaNumaNodes(capable []device.Interface) string {
	seen := make(map[string]bool, len(capable))
	nodes := make([]string, 0, len(capable))
	for i := range capable {
		numa := capable[i].NumaAffinity
		// Empty means the kernel gave no affinity. Publishing it as a member would assert a NUMA
		// node nobody read, and normalising it to 0 would assert the one the kernel denied.
		if numa == "" || seen[numa] {
			continue
		}
		seen[numa] = true
		nodes = append(nodes, numa)
	}
	sort.Strings(nodes)
	return strings.Join(nodes, rdmaNumaSeparator)
}

// closestAcceleratorDistance reports the tightest bus distance between any accelerator on this node
// and any RDMA-capable interface, and whether there was a pair to measure at all.
//
// The minimum over all pairs is the honest node-level reduction of a per-pair fact: it says "this
// node has a pairing this close", which is what an operator asks. It does NOT say the accelerators
// a workload receives will be that close — see the label's own comment.
func closestAcceleratorDistance(
	groups device.DevicesGroupList, capable []device.Interface,
) (device.TopologyDistance, bool) {
	closest, found := device.TopologyDistanceUnknown, false
	for gi := range groups {
		for ai := range groups[gi].Accelerators {
			topology := groups[gi].Accelerators[ai].Topology
			for ii := range capable {
				found = true
				if d := device.BusDistance(topology, capable[ii]); d < closest {
					closest = d
				}
			}
		}
	}
	return closest, found
}
