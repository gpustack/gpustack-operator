package detector

import (
	"fmt"
	"path/filepath"
	"strings"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

// interfacesChanged reports whether a freshly enumerated interface record differs from the one the
// last report published, which is what tells the monitor loop to take another detect round.
//
// reportedKnown says whether reported is a baseline at all, and it is a separate argument because
// the slice cannot carry that fact. An empty baseline and no baseline are the same slice, while
// they call for opposite answers: with no baseline nothing has been published, so any successful
// enumeration has to take the round. Without this argument that case answers "unchanged" on every
// host whose triggering subset is empty -- every interface virtual, none carrying an RDMA record,
// which is what a device manager sees in a Pod network namespace -- and the inventory then stays
// absent for as long as the accelerator keys hold still.
//
// A FAILED enumeration is not a change, and that outranks the missing baseline: taking the loop
// round on an unreadable read would re-report on no evidence, and the report path already refuses
// to publish an empty inventory after one — so the two agree on treating a failed read as "nothing
// established". The recovery is therefore driven by the first enumeration that SUCCEEDS, not by the
// failure itself, which is also what keeps a permanently unreadable sysfs from spinning the loop.
//
// First-seen times take no part in the comparison, and they are stripped HERE rather than left to
// the caller to keep out. They come from the stored object rather than from the machine, so a
// comparison that included them would differ on every tick and re-report forever with correct data
// in the object the whole time -- a defect whose only symptom is write volume. Making that hold
// locally means a future caller cannot reintroduce it by handing in a record that has been merged.
func interfacesChanged(
	reported []workercore.DeviceInterface, reportedKnown bool,
	detected []workercore.DeviceInterface, detectedErr error,
) bool {
	if detectedErr != nil {
		return false
	}
	if !reportedKnown {
		return true
	}
	return !kubemeta.DeepEqual(
		withoutLinkFirstSeen(triggersDetect(reported)),
		withoutLinkFirstSeen(triggersDetect(detected)))
}

// triggersDetect keeps the interfaces whose change is worth another detect round.
//
// Ephemeral virtual endpoints are dropped, and that omission is the point. Every Pod that starts or
// stops on a node adds or removes a `veth` under /sys/devices/virtual/net, so comparing the whole
// list turned Pod lifecycle into a hardware-change signal: the loop would leave the monitor, rerun
// the manufacturer's driver detection and rewrite the cluster-scoped Devices object on every Pod
// event, once per manufacturer DaemonSet. On a busy node that never settles. It is the same class of
// cost this comparison was introduced to stop paying, reached from the other direction — and unlike
// the timestamp case, the data really did change, so no value in the object looks wrong.
//
// A virtual interface carrying an RDMA device or a link verdict is KEPT regardless. The predicate is
// "cannot affect the gate", not "is virtual", so a bonded or overlay interface with an RDMA verdict
// takes the loop round like any other.
//
// No such interface exists today: readInterface returns a virtual one before resolving RDMA,
// because every path to an RDMA device there starts from a PCI device directory. This branch is
// therefore unreachable, and kept because the predicate is the correct one — a reader that later
// reaches RDMA some other way needs no change here. Stated so it is not mistaken for coverage.
//
// The published inventory still records every interface including the ephemeral ones, which is a
// boundary this feature keeps: what is narrowed is only what forces a re-read. The cost is that a
// purely virtual interface's arrival is not published until some other change reports it, and that
// is the trade — a veth list accurate to the second is worth less than a node that stops rewriting
// its object every time a Pod starts.
func triggersDetect(in []workercore.DeviceInterface) []workercore.DeviceInterface {
	out := make([]workercore.DeviceInterface, 0, len(in))
	for i := range in {
		if in[i].Virtual && !carriesRDMARecord(&in[i]) {
			continue
		}
		out = append(out, in[i])
	}
	return out
}

// carriesRDMARecord reports whether this interface or any of its virtual functions has an RDMA
// device bound or a link verdict recorded — the two things the node label is derived from.
func carriesRDMARecord(iface *workercore.DeviceInterface) bool {
	if iface.RDMA || iface.Link != nil {
		return true
	}
	for i := range iface.VirtualFunctions {
		if vf := &iface.VirtualFunctions[i]; vf.RDMA || vf.Link != nil {
			return true
		}
	}
	return false
}

// withoutLinkFirstSeen returns a copy of the record with every link's first-seen time cleared, at
// both levels that carry one.
func withoutLinkFirstSeen(in []workercore.DeviceInterface) []workercore.DeviceInterface {
	out := cloneInterfaces(in)
	for i := range out {
		if out[i].Link != nil {
			out[i].Link.FirstSeenTime = nil
		}
		for j := range out[i].VirtualFunctions {
			if vf := &out[i].VirtualFunctions[j]; vf.Link != nil {
				vf.Link.FirstSeenTime = nil
			}
		}
	}
	return out
}

// cloneInterfaces returns a record that shares nothing with the one given, so the copy the monitor
// loop compares against cannot be changed by the merge that runs after it is taken.
func cloneInterfaces(in []workercore.DeviceInterface) []workercore.DeviceInterface {
	if in == nil {
		return nil
	}
	out := make([]workercore.DeviceInterface, len(in))
	for i := range in {
		in[i].DeepCopyInto(&out[i])
	}
	return out
}

// sysfsEnumName returns the name half of a sysfs enum reading, which the RDMA subsystem writes as
// "<ordinal>: <NAME>" — "4: ACTIVE", "5: LinkUp". A value carrying no ordinal is returned as it is.
//
// The comparison has to be on the whole name, because these names are prefixes of one another:
// matching ACTIVE as a SUBSTRING also accepts ACTIVE_DEFER, a port that has lost its link and is
// not carrying user traffic. Reporting that as `ok` publishes the node's RDMA label over a link
// nothing can use — and overclaiming a working link is the error nothing downstream can catch,
// which is the same reason this file never reaches `failed` from a file it could not read.
func sysfsEnumName(v string) string {
	if _, name, found := strings.Cut(v, ":"); found {
		return strings.TrimSpace(name)
	}
	return strings.TrimSpace(v)
}

// checkRDMALink verifies the link of the RDMA device at the given path, relative to the sysfs root.
//
// The check is a pair of attribute reads per port — the port's transport state and its physical
// link state — and it is deliberately NOT dispatched per manufacturer. The prior art this was taken
// from reads exactly these two files for the one platform whose NPU RoCE port needed checking, and
// they are the RDMA subsystem's own attributes rather than any vendor's: a table keyed by
// manufacturer would have exactly one entry, and would bring into existence a state ("no check is
// implemented for this manufacturer") that does not otherwise occur.
//
// Both attributes are consulted because they answer different questions: a port can report the
// transport layer active while the physical link is administratively off.
//
// The state an unreadable file yields is the load-bearing decision here. `failed` withholds a node
// label, so it is reached only when every port was read and none of them carried the link. A port
// that could not be read leaves "all ports are down" unestablished, and reporting it as failed
// would remove a node from scheduling on the strength of a file that could not be opened.
//
// The verdict is per DEVICE, and the caller copies it to every interface that resolved to that
// device. That is right while the device's ports back one netdev — an unused port says nothing about
// the interface — and WRONG for the InfiniBand-mode shape, where one device's two ports map to two
// IPoIB netdevs: a healthy port then masks a down sibling and the interface's row overstates it.
// Attributing a netdev to its own port needs ports/*/gid_attrs/ndevs/*, and no machine available
// here presents that shape, so the mapping would ship unexercised in a position whose failure mode
// is withholding a label. The limit is named rather than half-fixed.
func (s *sysfsTree) checkRDMALink(rdmaRel string) *workercore.DeviceInterfaceLink {
	portsRel := filepath.Join(rdmaRel, "ports")

	// The error is discarded rather than branched on: an unreadable port directory, an empty one
	// and one whose every port refuses to be read are the same fact — no port state was read — and
	// they reach it through the loop below yielding nothing.
	entries, _ := s.readDir(portsRel)

	// os.ReadDir returns entries sorted by name, so the reason below is ordered by construction.
	// Sorting again here would be a second guarantee of the same thing, and an unsorted reason
	// would compare unequal on passes where nothing changed.
	var (
		observed   []string
		unreadable []string
	)
	for _, entry := range entries {
		portRel := filepath.Join(portsRel, entry.Name())
		state, serr := s.attr(filepath.Join(portRel, "state"))
		physState, perr := s.attr(filepath.Join(portRel, "phys_state"))
		if serr != nil || perr != nil {
			unreadable = append(unreadable, entry.Name())
			continue
		}

		// Both values go into the reason verbatim. They are the kernel's own words, and an
		// operator reading a withheld label needs what was read rather than our summary of it.
		observed = append(observed,
			fmt.Sprintf("port %s: state=%q phys_state=%q", entry.Name(), state, physState))

		if sysfsEnumName(state) == "ACTIVE" && sysfsEnumName(physState) == "LinkUp" {
			return &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK}
		}
	}

	switch {
	case len(observed) == 0:
		return &workercore.DeviceInterfaceLink{
			State:  workercore.DeviceInterfaceLinkStateUnverified,
			Reason: fmt.Sprintf("the RDMA port state could not be read under %q", portsRel),
		}
	case len(unreadable) > 0:
		return &workercore.DeviceInterfaceLink{
			State: workercore.DeviceInterfaceLinkStateUnverified,
			Reason: fmt.Sprintf("the RDMA link could not be verified: %s; unreadable: %s",
				strings.Join(observed, "; "), strings.Join(unreadable, ", ")),
		}
	default:
		return &workercore.DeviceInterfaceLink{
			State:  workercore.DeviceInterfaceLinkStateFailed,
			Reason: "the RDMA link is not usable: " + strings.Join(observed, "; "),
		}
	}
}

// carryLinkFirstSeen fills in each detected link's first-seen time from what is already stored,
// rewriting detected in place.
//
// It must run BEFORE the detected inventory is compared against the stored one, and that ordering
// is the whole reason this function exists. Two of this feature's requirements collide otherwise: a
// failure's first-seen time has to stay stable while the failure persists, and a pass that found
// nothing new has to compare equal to what is stored. Taking the current instant on every pass
// keeps the first requirement's letter and breaks the second — the comparison would never match, so
// the object would be rewritten on every pass, forever, holding correct data the whole time. Write
// volume would be the only symptom.
//
// Only a failure carries a time, and it is keyed on the STATE rather than on the reason: a second
// port going down changes the reason and is still the same outage, which is what "how long has this
// been broken?" asks about. A stored failure with no time is stamped rather than carried, so an
// object written before this field existed converges instead of staying blank.
//
// Passing a nil stored inventory is the create path: every failure is then seen for the first time.
func carryLinkFirstSeen(stored, detected []workercore.DeviceInterface, now meta.Time) {
	previous := make(map[string]*workercore.DeviceInterfaceLink, len(stored))
	for i := range stored {
		if stored[i].Link != nil {
			previous[stored[i].Name] = stored[i].Link
		}
		// Virtual functions get the same treatment, because they carry the same verdict. A VF's
		// clock left to be taken fresh each pass would rewrite the object forever, and the record
		// would be one whose only wrong value is a timestamp nobody compares.
		for j := range stored[i].VirtualFunctions {
			vf := &stored[i].VirtualFunctions[j]
			if vf.Link != nil {
				previous[vfLinkKey(stored[i].Name, vf)] = vf.Link
			}
		}
	}

	for i := range detected {
		carryOneLinkFirstSeen(previous, detected[i].Name, detected[i].Link, now)
		for j := range detected[i].VirtualFunctions {
			vf := &detected[i].VirtualFunctions[j]
			carryOneLinkFirstSeen(previous, vfLinkKey(detected[i].Name, vf), vf.Link, now)
		}
	}
}

// vfLinkKey identifies one virtual function's link record across passes.
//
// The bus id is the identity rather than the name: a VF with no net device configured has no name
// at all, and two of those under one physical function would otherwise share a key and hand each
// other's outage clock around.
//
// That the bus id is always there is the reader's doing, not this function's: readVirtualFunctions
// takes it from the sysfs directory name, so it cannot be missing for a record this package
// produced. A record built without one falls back to the name, and one with NEITHER collapses to a
// shared key — the very collision the paragraph above avoids — so the reader setting the bus id is
// load-bearing rather than incidental. Synthesizing an identity here instead would have to be
// stable across passes, and nothing about such a record is.
//
// The preflight report names a VF row by the same rule (vfInterfaceName), independently rather than
// through this function: one keys an outage clock and the other addresses a reader, and they agree
// today because the bus id is the best answer to both questions.
func vfLinkKey(ifaceName string, vf *workercore.DeviceInterfaceVirtualFunction) string {
	id := vf.PciBusID
	if id == "" {
		id = vf.Name
	}
	return ifaceName + "/" + id
}

// carryOneLinkFirstSeen applies the rule to one link record.
//
// Matched by the caller's key, which is the record's identity. Matching by position would hand one
// interface's outage clock to another as soon as the inventory gained or lost an entry.
func carryOneLinkFirstSeen(
	previous map[string]*workercore.DeviceInterfaceLink,
	key string, link *workercore.DeviceInterfaceLink, now meta.Time,
) {
	if link == nil {
		return
	}
	if link.State != workercore.DeviceInterfaceLinkStateFailed {
		// Not an outage, so there is nothing for a first-seen time to be the start of.
		link.FirstSeenTime = nil
		return
	}

	if was := previous[key]; was != nil &&
		was.State == workercore.DeviceInterfaceLinkStateFailed && was.FirstSeenTime != nil {
		link.FirstSeenTime = was.FirstSeenTime.DeepCopy()
		return
	}
	link.FirstSeenTime = now.DeepCopy()
}
