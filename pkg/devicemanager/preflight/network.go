package preflight

import (
	"fmt"
	"time"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
)

type (
	// NetworkCheck is one RDMA-capable interface's or virtual function's link verdict.
	//
	// State is the detector's own link vocabulary rather than PreflightState, and that is
	// deliberate: the three link values already exist, are already what decides whether the node
	// label is emitted, and mapping them onto the three allocation states would create a second
	// vocabulary for the same fact — with different consequences attached to each value. The row a
	// reader sees here is the value the node publishes.
	NetworkCheck struct {
		// Interface is this row's identity: the kernel interface name for a NIC, and
		// `<interface>/<vf bus id>` for one of its virtual functions.
		Interface string `json:"interface" yaml:"interface"`
		// RDMADevice is the RDMA device bound to it, so a reader can go look at the same tree.
		RDMADevice string `json:"rdmaDevice" yaml:"rdmaDevice"`
		// State is what the link check concluded.
		State workercore.DeviceInterfaceLinkState `json:"state" yaml:"state"`
		// Depth is how far the answer was taken, and it is always `declared`: no host CLI is run
		// for a link row. It is carried rather than omitted so the answer cannot be read as having
		// been measured.
		//
		// `declared` covers one row it fits less exactly — an RDMA-bound entry the inventory holds
		// no verdict for, where this section synthesizes `unverified` because sysfs did not answer
		// rather than because it answered. The vocabulary has no value for that, and the row's own
		// reason says which case it is; the alternative was inventing a fourth depth for a
		// distinction only this section can draw.
		Depth device.PreflightDepth `json:"depth" yaml:"depth"`
		// Reason is the checker's own words, empty exactly when the link verified.
		Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
	}

	// NetworkReport is what this pass established about the node's RDMA links.
	//
	// It is a section of its own rather than rows inside a manufacturer's group, because a NIC
	// belongs to the machine: the per-accelerator row type requires an accelerator id and an
	// allocation mode, and a network interface has neither. Filling those with empty strings would
	// make a link row indistinguishable from a malformed accelerator row.
	NetworkReport struct {
		// Timestamp is when the links were read. A preflight reports mutable host state as it
		// stands, so the reading is only worth what its time claims.
		Timestamp time.Time `json:"timestamp" yaml:"timestamp"`
		// Checks holds one row per RDMA-capable interface and per RDMA-capable virtual function, in
		// the inventory's order — which is by interface name, sorted by the pass that produced it,
		// with each interface's virtual functions following it.
		Checks []NetworkCheck `json:"checks,omitempty" yaml:"checks,omitempty"`
		// Note says what the rows cannot: that the enumeration failed, or that it succeeded and
		// found no RDMA-capable interface. An empty list on its own reads as a node that passed,
		// which is the one shape a diagnostic must never produce.
		Note string `json:"note,omitempty" yaml:"note,omitempty"`
	}
)

// PreflightNetwork reads this node's interfaces and reports their RDMA link states.
//
// It calls the detector's own inventory pass rather than reimplementing it, and that buys one
// property: the two readings INTERPRET the host identically, so no state vocabulary, no reason
// wording and no bus coordinate can drift between a preflight row and a published record. It does
// NOT make the two agree. This is a fresh sysfs read taken now; the record was written by whichever
// pass last had a reason to run, and a link that went down since is exactly the case this section
// exists to surface. Same computation, two observations — they can differ, and when they do the
// difference is the host's, not an interpretation's.
//
// It takes no options, and DryRun in particular does not reach it: this is a pure read that
// configures nothing and brings no link up, so there is no action for a dry run to withhold.
func PreflightNetwork() NetworkReport {
	interfaces, err := detector.DetectInterfaces()
	return networkReport(interfaces, err, time.Now())
}

// networkReport turns one inventory pass into the report's network section.
//
// The pass's error is a parameter rather than something handled by the caller because the two
// outcomes it separates both belong in the document: a failed enumeration and a node with no RDMA
// hardware produce the same empty row list and must not produce the same words.
func networkReport(
	interfaces []workercore.DeviceInterface, err error, now time.Time,
) NetworkReport {
	report := NetworkReport{Timestamp: now}

	if err != nil {
		report.Note = "the network interfaces could not be enumerated, so nothing is claimed " +
			"about this node's RDMA links: " + err.Error()
		return report
	}

	virtualFunctions := 0
	for i := range interfaces {
		iface := &interfaces[i]
		if hasLinkToReport(iface.RDMA, iface.Link) {
			report.Checks = append(report.Checks,
				linkCheck(iface.Name, iface.RDMADevice, iface.Link))
		}

		// Virtual functions carry the same verdict and are visited for the same reason the node
		// labels traverse them: SR-IOV is how RDMA reaches containers on many setups, and a VF is
		// removed from the top-level inventory. Reading only the top level made a node whose every
		// RDMA device is a VF report that no device is bound at all -- while the label derived from
		// the same inventory said the node was capable. A preflight contradicting the node's own
		// record is worse than one that says nothing.
		virtualFunctions += len(iface.VirtualFunctions)
		for j := range iface.VirtualFunctions {
			vf := &iface.VirtualFunctions[j]
			if !hasLinkToReport(vf.RDMA, vf.Link) {
				continue
			}
			report.Checks = append(report.Checks,
				linkCheck(vfInterfaceName(iface.Name, vf), vf.RDMADevice, vf.Link))
		}
	}

	if len(report.Checks) == 0 {
		report.Note = fmt.Sprintf(
			"none of the %d interfaces on this node, nor any of their %d virtual functions, has an "+
				"RDMA device bound, so there is no link to check; this is an answer about the "+
				"hardware, not a check that was skipped",
			len(interfaces), virtualFunctions)
	}

	return report
}

// hasLinkToReport reports whether one inventory entry has anything for this section to say.
//
// `rdma: false` with no verdict is the settled negative: the row would be a verdict on a check that
// was never in question, and the inventory already carries the fact.
//
// A verdict with no RDMA device is NOT that case. It is the one where the RDMA tree exists and could
// not be read, so whether this entry has an RDMA device is unestablished. Skipping it would drop the
// one diagnostic this section exists for, on the node where it is hardest to get any other way.
func hasLinkToReport(rdma bool, link *workercore.DeviceInterfaceLink) bool {
	return rdma || link != nil
}

// linkCheck renders one inventory entry's verdict as a row.
func linkCheck(
	name, rdmaDevice string, link *workercore.DeviceInterfaceLink,
) NetworkCheck {
	check := NetworkCheck{
		Interface:  name,
		RDMADevice: rdmaDevice,
		Depth:      device.PreflightDepthDeclared,
	}
	if link != nil {
		check.State, check.Reason = link.State, link.Reason
		return check
	}

	// An RDMA-bound entry the inventory carries no verdict for. Dropping the row would be the
	// omission this section exists to avoid, and calling it ok would invent one.
	check.State = workercore.DeviceInterfaceLinkStateUnverified
	check.Reason = "the inventory carries no link state for this interface"
	return check
}

// vfInterfaceName names a virtual function's row.
//
// The physical function is part of it because a reader with a flat list of rows has no other way to
// tell a VF row from a NIC's own, and the bus id is preferred over the VF's name for the same reason
// the outage clock keys on it: a VF with no net device configured has no name at all.
func vfInterfaceName(ifaceName string, vf *workercore.DeviceInterfaceVirtualFunction) string {
	id := vf.PciBusID
	if id == "" {
		id = vf.Name
	}
	return ifaceName + "/" + id
}
