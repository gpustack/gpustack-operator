package nodefeature

import (
	"sort"
	"strings"

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
	// It is the ONE key of this set that a ResourceFlavor selector can afford. That selector is
	// equality-matched and capped at eight entries, of which the pool discriminators already claim
	// up to five and the flavor's own accelerator count one more — so this feature's budget is
	// about two keys, and only a key a selector can pin is worth spending one on.
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
)

// ConstructRDMANodeLabels constructs the RDMA feature labels from one detect pass's inventory.
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
// It returns an empty map rather than nil for a node with no such interface, and the caller's
// removal of stale keys is what makes that absence take effect — the labels are additive at the
// object they are written to, so a key that stops being reported does not stop existing.
func ConstructRDMANodeLabels(
	groups device.DevicesGroupList, interfaces []device.Interface,
) map[string]string {
	capable := rdmaEndpoints(interfaces)

	labels := map[string]string{}
	if len(capable) == 0 {
		return labels
	}
	labels[NodeRDMACapableLabelKey] = "true"

	// The NUMA set is a COMPOSITE value, so the sanitizer below is not a safety net for it: it caps
	// a label value at 63 characters, and truncating "0_1_..._31" publishes a SUBSET as if it were
	// the set — a wrong fact rather than a degraded one. A node with enough NUMA nodes to exceed the
	// cap therefore gets no NUMA key at all. `rdma.capable` still says the node has RDMA, and the
	// per-endpoint affinities are in `Devices`, which carries no such limit.
	if numa := rdmaNumaNodes(capable); numa != "" && kubemeta.SanitizeLabelValue(numa) == numa {
		labels[NodeRDMANumaLabelKey] = numa
	}
	// Reported only when an accelerator was there to measure from. A distance is a statement about
	// a PAIR, so a node with RDMA and no accelerators has no distance rather than an unknown one.
	if distance, ok := closestAcceleratorDistance(groups, capable); ok {
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
