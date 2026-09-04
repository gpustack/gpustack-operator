package detector

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/binding"
)

const (
	// sysfsAttrMaxBytes caps one attribute read. A sysfs attribute is a single value or a short
	// list; anything larger means the path is not the attribute it was taken for, and reading it
	// wholesale into memory is what this cap prevents.
	sysfsAttrMaxBytes = 64 << 10

	sysfsDevicesDir     = "devices"
	sysfsNetClassDir    = "class/net"
	sysfsInfinibandDir  = "class/infiniband"
	sysfsVirtualDevices = "virtual"
)

// sysfsTree reads attributes under one sysfs root, resolving the root's own symlinks once.
//
// The root is a parameter rather than a compiled-in constant for a reason that is not
// configurability: the file selecting the real root is constrained to Linux and therefore does not
// compile — or get linted, or get tested — on the platform this code is written on. Passing the
// root in is what lets the whole pass run against a fixture, which is the only verification
// available before it reaches a node.
type sysfsTree struct {
	realRoot string

	// rdmaClass is the reverse RDMA layout, read at most once for this tree. See rdmaClassIndex.
	rdmaClass *rdmaClassIndex
}

// rdmaClassIndex is what one listing of the RDMA class directory established.
//
// It answers the reverse layout's question — "which RDMA device points back at this PCI device?" —
// for every interface and virtual function in a pass, from one listing.
type rdmaClassIndex struct {
	// listed is whether the class directory could be listed at all.
	listed bool
	// existsUnlisted is whether it exists and could NOT be listed, which is a failed read rather
	// than an absence.
	existsUnlisted bool
	// byDevice maps each candidate's RESOLVED device path to the RDMA directory holding it.
	byDevice map[string]string
	// unresolvable is whether some candidate's device link would not resolve. A miss in byDevice
	// then cannot be read as "no RDMA device for this interface", because the one that did not
	// resolve might have been the match.
	unresolvable bool
}

func newSysfsTree(root string) (*sysfsTree, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve sysfs root %q: %w", root, err)
	}
	return &sysfsTree{realRoot: realRoot}, nil
}

// resolve turns a path relative to the sysfs root into an absolute one, following symlinks and
// then checking where they landed.
//
// Following symlinks is unavoidable here — every interesting path in sysfs is one — so validating
// the RESOLVED path is the whole of the discipline. Without it, a symlink in a tree this process
// does not own decides which file it reads.
func (s *sysfsTree) resolve(rel string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(s.realRoot, rel))
	if err != nil {
		return "", err
	}
	if resolved != s.realRoot && !strings.HasPrefix(resolved, s.realRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("sysfs path %q resolves to %q, outside %q", rel, resolved, s.realRoot)
	}
	return resolved, nil
}

// relTo turns an already-resolved absolute path back into one relative to the root, so every
// subsequent read goes through resolve rather than around it.
func (s *sysfsTree) relTo(abs string) (string, error) {
	rel, err := filepath.Rel(s.realRoot, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside %q", abs, s.realRoot)
	}
	return rel, nil
}

// exists reports whether a path is present WITHOUT following it, which is what separates "there is
// no RDMA here" from "the RDMA tree is there and could not be read".
func (s *sysfsTree) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(s.realRoot, rel))
	return err == nil
}

// isDir reports whether rel resolves to a directory inside the tree. Unlike exists it follows the
// link, because what the caller needs to know is what the entry IS, not that the name is taken.
//
// The error is returned rather than folded into the bool, and that is the whole reason this has two
// results. A single false collapses "proven not to be a directory" into "could not be read", and the
// caller skips the entry either way — which turns an unreadable interface into an absent one and
// publishes a partial inventory. F3 forbids exactly that: a partial absence reads as hardware that
// is not there, so an unreadable entry has to end the pass instead of shortening the list.
//
// A vanished entry is not a failure. An interface can be removed between the directory listing and
// this call, and that is an ordinary race rather than an unreadable tree, so os.IsNotExist yields
// (false, nil) — skip it, do not fail the pass.
func (s *sysfsTree) isDir(rel string) (bool, error) {
	p, err := s.resolve(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}

func (s *sysfsTree) readDir(rel string) ([]os.DirEntry, error) {
	p, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(p)
}

// attr reads one attribute, capped and trimmed.
func (s *sysfsTree) attr(rel string) (string, error) {
	p, err := s.resolve(rel)
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	buf, err := io.ReadAll(io.LimitReader(f, sysfsAttrMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(buf) > sysfsAttrMaxBytes {
		return "", fmt.Errorf("sysfs attribute %q exceeds %d bytes", rel, sysfsAttrMaxBytes)
	}
	return strings.TrimSpace(string(buf)), nil
}

// attrOrEmpty reads an attribute, reporting an unreadable one as empty. Empty means UNKNOWN
// everywhere this is used, which is why no caller needs the error.
func (s *sysfsTree) attrOrEmpty(rel string) string {
	v, err := s.attr(rel)
	if err != nil {
		return ""
	}
	return v
}

// attrInt32 reads a numeric attribute. An unreadable or unparseable one is zero, and zero means
// "not read" for every field this fills — no interface reports an MTU of zero.
func (s *sysfsTree) attrInt32(rel string) int32 {
	v, err := s.attr(rel)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}

// numaAffinity reads a NUMA node id, mapping the kernel's "no affinity" sentinel to empty.
//
// Empty means unknown. It must not be normalised to node 0, which would publish an affinity nobody
// read, and the sentinel must not be published either — "-1" is not a node id.
func (s *sysfsTree) numaAffinity(rel string) string {
	if v := s.attrOrEmpty(rel); v != "" && v != "-1" {
		return v
	}
	return ""
}

// readUp reads an interface's operational state.
//
// operstate is authoritative when it can be read; carrier is the fallback. An unreadable pair
// reports not-up: this value only ever describes the interface, never gates a label, so guessing
// up from an absent file would be inventing a state.
func (s *sysfsTree) readUp(ifaceRel string) bool {
	if v, err := s.attr(filepath.Join(ifaceRel, "operstate")); err == nil {
		return v == "up"
	}
	if v, err := s.attr(filepath.Join(ifaceRel, "carrier")); err == nil {
		return v == "1"
	}
	return false
}

// classifyBus names the bus from where the interface's directory sits in the device tree, which is
// also how a virtual interface is recognized: the kernel puts those under devices/virtual.
//
// An interface that is neither virtual nor on PCI reports its own bus rather than nothing, so its
// missing PCI coordinates read as a KIND of interface instead of a failed lookup. That case is the
// reason this pass enumerates interfaces first: a PCI-rooted walk cannot see it at all.
func (s *sysfsTree) classifyBus(resolvedIfaceDir string) (bus string, virtual bool) {
	rel, err := s.relTo(resolvedIfaceDir)
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) < 2 || parts[0] != sysfsDevicesDir {
		return "", false
	}
	switch segment := parts[1]; {
	case segment == sysfsVirtualDevices:
		return sysfsVirtualDevices, true
	case strings.HasPrefix(segment, "pci"):
		return "pci", false
	default:
		return segment, false
	}
}

// busFromSubsystem names the bus from the device's own `subsystem` link, whose target is the bus
// directory under /sys/bus — so its base is the bus name the kernel assigned.
//
// Empty means the link could not be resolved, which the caller reads as "no better answer than the
// path already gave" rather than as a bus of its own.
func (s *sysfsTree) busFromSubsystem(deviceRel string) string {
	target, err := s.resolve(filepath.Join(deviceRel, "subsystem"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// resolveRDMA finds the RDMA device bound to a PCI device, returning its directory relative to the
// sysfs root — which carries both facts a caller needs, since the device's name is that path's base.
//
// Returning the path rather than the name is what lets the link check read the ports without
// assuming a layout: this function already knows which of the two layouts answered, and the check
// would otherwise have to guess again and get it wrong on the layout the other branch found.
//
// It returns every layout it tried, so a negative answer can say what was looked for. readFailed
// separates "there is no RDMA device here", which is an ordinary answer, from "the tree is present
// and could not be read", which is not — reporting the second as the first would publish a node as
// having no RDMA on the strength of a failed read.
func (s *sysfsTree) resolveRDMA(pciRel string) (dir string, tried []string, readFailed bool) {
	// The layout where the RDMA device hangs under the PCI device.
	underDevice := filepath.Join(pciRel, "infiniband")
	tried = append(tried, underDevice)
	entries, err := s.readDir(underDevice)
	switch {
	case err == nil && len(entries) > 0:
		return filepath.Join(underDevice, entries[0].Name()), tried, false
	case err != nil && s.exists(underDevice):
		readFailed = true
	}

	// The layout where only the reverse binding is exposed: walk the RDMA class and match each
	// entry's device back to this one.
	//
	// Every failure on this path sets readFailed, for the same reason the device-local layout does:
	// returning false here would publish `rdma: false` — "this interface has no RDMA device" — on
	// the strength of a directory that exists and could not be read. That is the same unsupported
	// claim as publishing an empty inventory after a failed enumeration, and this feature refuses
	// it everywhere else.
	tried = append(tried, sysfsInfinibandDir)
	index := s.rdmaClassIndex()
	switch {
	case index.existsUnlisted:
		readFailed = true
	case index.listed:
		want, werr := s.resolve(pciRel)
		if werr != nil {
			// This interface's own device path could not be resolved, so no entry can be matched
			// back to it. Whether an entry would have matched is unestablished, not answered no.
			return "", tried, true
		}
		if dir, ok := index.byDevice[want]; ok {
			return dir, tried, false
		}
		// No match, so a candidate that would not resolve is now load-bearing: it might have been
		// the one. Consulted only after the miss, which is why finding a match still reports no
		// failure even on a tree where some other entry was unreadable.
		if index.unresolvable {
			readFailed = true
		}
	}

	return "", tried, readFailed
}

// rdmaClassIndex reads the reverse RDMA layout once and answers every later query from the result.
//
// Built once per TREE rather than per call, and the tree lives for exactly one enumeration pass.
// resolveRDMA is called once per interface and once per virtual function, and the previous shape
// listed this directory and resolved every entry under it on each of those calls — so a host using
// this layout with N RDMA virtual functions performed N listings and up to N*N path resolutions,
// and it did so on every pass. The pass now runs on the device manager's monitor cadence rather
// than once per accelerator rediscovery, which multiplies that cost by the tick rate.
//
// The lifetime is what keeps this a memo rather than a cache. An index outliving a pass would
// answer with hardware that has since gone away, and the inventory would report a device nothing
// can open; a per-pass tree makes that impossible instead of unlikely.
func (s *sysfsTree) rdmaClassIndex() *rdmaClassIndex {
	if s.rdmaClass != nil {
		return s.rdmaClass
	}

	index := &rdmaClassIndex{byDevice: map[string]string{}}
	entries, err := s.readDir(sysfsInfinibandDir)
	switch {
	case err != nil:
		// Absent is an answer; present-and-unreadable is not one, and only the second may reach a
		// caller as a failed read.
		index.existsUnlisted = s.exists(sysfsInfinibandDir)
	default:
		index.listed = true
		for _, entry := range entries {
			dir := filepath.Join(sysfsInfinibandDir, entry.Name())
			device, derr := s.resolve(filepath.Join(dir, "device"))
			if derr != nil {
				index.unresolvable = true
				continue
			}
			// First writer wins, matching the loop this replaced: it returned the first entry whose
			// device matched, and os.ReadDir orders entries by name.
			if _, seen := index.byDevice[device]; !seen {
				index.byDevice[device] = dir
			}
		}
	}

	s.rdmaClass = index
	return index
}

// readVirtualFunctions collects a physical function's virtual functions, nested under it and
// ordered by bus id (P8/P9).
//
// A failed read is returned here rather than swallowed, unlike in the attribute readers around it.
// What separates them is what the degraded record would CLAIM: an unreadable attribute degrades to
// "unknown", which is true, while an unreadable virtfn set degrades to "this physical function has
// no virtual functions". The caller has already committed `sriov: true` from a file that answered,
// so that reads as "a PF with zero VFs configured" — the fact this inventory keeps deliberately
// distinct from "not a PF". A dropped virtfn understates the same count for the same reason, so
// those are returned too and not skipped past.
//
// On a host that exposes RDMA only through its virtual functions, that absence is every RDMA device
// the node has, and it would withdraw `rdma.capable` on the strength of a directory that could not
// be listed. The pass's error path already preserves the previous inventory and labels, which is
// the outcome this one belongs to.
func (s *sysfsTree) readVirtualFunctions(
	pciRel string,
) ([]workercore.DeviceInterfaceVirtualFunction, error) {
	entries, err := s.readDir(pciRel)
	if err != nil {
		return nil, fmt.Errorf("list the virtual functions of %s: %w", pciRel, err)
	}

	var out []workercore.DeviceInterfaceVirtualFunction
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "virtfn") {
			continue
		}
		vfDir, err := s.resolve(filepath.Join(pciRel, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("resolve %s of %s: %w", entry.Name(), pciRel, err)
		}
		vfRel, err := s.relTo(vfDir)
		if err != nil {
			return nil, fmt.Errorf("locate %s of %s under the sysfs root: %w", entry.Name(), pciRel, err)
		}

		vf := workercore.DeviceInterfaceVirtualFunction{
			PciBusID:     filepath.Base(vfDir),
			NumaAffinity: s.numaAffinity(filepath.Join(vfRel, "numa_node")),
			CpuAffinity:  s.attrOrEmpty(filepath.Join(vfRel, "local_cpulist")),
		}
		if netEntries, nerr := s.readDir(filepath.Join(vfRel, "net")); nerr == nil && len(netEntries) > 0 {
			vf.Name = netEntries[0].Name()
			ifaceRel := filepath.Join(vfRel, "net", vf.Name)
			vf.MTU = s.attrInt32(filepath.Join(ifaceRel, "mtu"))
			vf.Up = s.readUp(ifaceRel)
		}
		// The link is checked here for the same reason it is on a top-level interface, and the
		// omission mattered more than usual: a VF is removed from the top-level list, so a nil
		// verdict here is a bound RDMA device with no verdict ANYWHERE in the record. On a setup
		// that exposes RDMA only through its virtual functions, that is every RDMA device the node
		// has.
		if rdmaDir, tried, readFailed := s.resolveRDMA(vfRel); rdmaDir != "" {
			vf.RDMA, vf.RDMADevice = true, filepath.Base(rdmaDir)
			vf.Link = s.checkRDMALink(rdmaDir)
		} else if readFailed {
			vf.Link = &workercore.DeviceInterfaceLink{
				State:  workercore.DeviceInterfaceLinkStateUnverified,
				Reason: "the RDMA tree is present but could not be read; tried " + strings.Join(tried, ", "),
			}
		}
		out = append(out, vf)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].PciBusID < out[j].PciBusID })
	return out, nil
}

// readInterface builds one interface's record.
//
// isVF reports that this interface belongs to a virtual function, which the caller skips at top
// level because it is collected under its physical function instead.
//
// An ATTRIBUTE that cannot be read degrades the record rather than dropping it: the interface still
// has a name, and a name with empty attributes says "unknown", while omitting the interface would
// say "not present" — a different and worse claim.
//
// A failure that would make the record CLAIM something instead is returned, and ends the pass. Two
// reach that: the virtual function set, which has no way to say "unknown" (see
// readVirtualFunctions), and the device link below. On a non-virtual interface an unresolvable
// `device` leaves `rdma: false` with no verdict, and that is not "unknown" — it is the same record
// a plain Ethernet NIC produces, so the node's `rdma.capable` is withdrawn on the strength of a
// symlink that could not be read. Synthesizing `unverified` instead would fail the other way, by
// inventing an endpoint on a node that may carry no RDMA at all. Neither claim is available, so the
// pass fails and the previously published inventory and labels stand.
func (s *sysfsTree) readInterface(
	name string,
) (iface workercore.DeviceInterface, isVF bool, err error) {
	iface.Name = name
	ifaceRel := filepath.Join(sysfsNetClassDir, name)
	iface.MTU = s.attrInt32(filepath.Join(ifaceRel, "mtu"))
	iface.Up = s.readUp(ifaceRel)

	resolvedIface, rerr := s.resolve(ifaceRel)
	if rerr != nil {
		return iface, false, nil //nolint:nilerr // degraded on purpose: a named interface, coordinates unknown
	}
	iface.Bus, iface.Virtual = s.classifyBus(resolvedIface)
	if iface.Virtual {
		// A virtual interface stops here, BEFORE the RDMA resolution below, and that is a limit
		// worth naming: every path to an RDMA device in this reader starts from a PCI device
		// directory, and a bond, a VLAN or a veth has none. So this reader cannot produce a
		// virtual interface carrying an RDMA device or a link verdict, on any host.
		//
		// triggersDetect nonetheless exempts only virtual interfaces with NO RDMA record, which
		// reads as if such records existed. It is written on the verdict rather than on virtuality
		// deliberately: the predicate is "cannot affect the gate", so if a future reader ever
		// reaches RDMA through something other than a PCI device, the trigger already handles it
		// and does not have to be found and changed. Today that branch is unreachable, and saying
		// so here is the point — an exemption for a shape nothing produces otherwise reads as
		// coverage.
		return iface, false, nil
	}

	deviceDir, rerr := s.resolve(filepath.Join(ifaceRel, "device"))
	if rerr != nil {
		return iface, false, fmt.Errorf("resolve the device of %s: %w", name, rerr)
	}
	deviceRel, rerr := s.relTo(deviceDir)
	if rerr != nil {
		return iface, false, fmt.Errorf("locate the device of %s under the sysfs root: %w", name, rerr)
	}

	// A virtual function names its physical function. Finding one settles that this interface is
	// not a top-level entry.
	if s.exists(filepath.Join(deviceRel, "physfn")) {
		return iface, true, nil
	}

	// The path told us where the device SITS; its own `subsystem` link is the kernel's answer about
	// what it IS, and the two disagree for anything bridged off PCI. A USB NIC resolves through
	// devices/pci0000:00/.../usb1/1-1/1-1:1.0/net/eth0, so the path-derived answer calls it PCI —
	// after which the basename of its device directory ("1-1:1.0") gets stored as a PCI address and
	// a bridge walk runs on a device that has none, publishing coordinates a distance is then
	// computed from. The subsystem answer is preferred whenever it reads; when it does not, the
	// path-derived one stands, because an unreadable link is not evidence of a different bus.
	if bus := s.busFromSubsystem(deviceRel); bus != "" {
		iface.Bus = bus
	}

	iface.NumaAffinity = s.numaAffinity(filepath.Join(deviceRel, "numa_node"))
	iface.CpuAffinity = s.attrOrEmpty(filepath.Join(deviceRel, "local_cpulist"))

	if iface.Bus == "pci" {
		// Shared with the accelerator side rather than derived again here. These coordinates are
		// only useful when they compare equal to the ones the manufacturers write, so one
		// implementation is what makes the two identical by construction instead of by discipline.
		iface.PciBusID = filepath.Base(deviceDir)
		iface.PciRootID, iface.PciSwitches = binding.ResolvePCITopology(deviceDir)
		iface.PciVendor = s.attrOrEmpty(filepath.Join(deviceRel, "vendor"))
		iface.PciDevice = s.attrOrEmpty(filepath.Join(deviceRel, "device"))
	}

	// sriov_numvfs is present exactly on a physical function, including one with no VFs
	// configured. That is why the flag is read from the file's presence and the count is not
	// inferred from it: "a PF with zero VFs" and "not a PF" are different facts.
	//
	// Presence, not readability. Reading the file and taking a nil error for the answer made an
	// unreadable-but-present marker report "not a PF", which then skips VF enumeration entirely —
	// and on a host whose RDMA lives only on its virtual functions that withdraws `rdma.capable`
	// on a failed read. With the presence check the marker still says "PF", and any failure to
	// list the functions propagates from readVirtualFunctions instead of being swallowed here.
	if s.exists(filepath.Join(deviceRel, "sriov_numvfs")) {
		iface.SRIOV = true
		vfs, verr := s.readVirtualFunctions(deviceRel)
		if verr != nil {
			return iface, false, fmt.Errorf("read the virtual functions of %s: %w", name, verr)
		}
		iface.VirtualFunctions = vfs
	}

	rdmaDir, tried, readFailed := s.resolveRDMA(deviceRel)
	switch {
	case rdmaDir != "":
		iface.RDMA, iface.RDMADevice = true, filepath.Base(rdmaDir)
		// A bound RDMA device is not a working link, and the two differ on real hardware: a port
		// can be fully configured, with an address and a gateway, while its link is down.
		iface.Link = s.checkRDMALink(rdmaDir)
	case readFailed:
		// The tree is there and unreadable. Unverified, never failed: failed withholds a node
		// label, and a label must never be withheld because a file could not be read.
		iface.Link = &workercore.DeviceInterfaceLink{
			State:  workercore.DeviceInterfaceLinkStateUnverified,
			Reason: "the RDMA tree is present but could not be read; tried " + strings.Join(tried, ", "),
		}
	}

	return iface, false, nil
}

// enumerateInterfaces reports every network interface under the given sysfs root, sorted by name.
//
// It enumerates interfaces and resolves each one's PCI device as an attribute, rather than walking
// the PCI bus and correlating back — an interface on a non-PCI interconnect is invisible to the
// latter, and that is the platform this inventory is most needed for.
//
// A failure to enumerate is an error, never an empty list. An empty inventory is indistinguishable
// from a node that was never profiled, so returning no interfaces and no error is the one failure
// mode that would pass by omission. That applies to a PARTIAL absence for the same reason, which is
// why one interface's unreadable virtual function set ends the pass rather than shortening a list.
func enumerateInterfaces(root string) ([]workercore.DeviceInterface, error) {
	tree, err := newSysfsTree(root)
	if err != nil {
		return nil, err
	}

	entries, err := tree.readDir(sysfsNetClassDir)
	if err != nil {
		return nil, fmt.Errorf("enumerate network interfaces: %w", err)
	}

	out := make([]workercore.DeviceInterface, 0, len(entries))
	for _, entry := range entries {
		// The net class directory is not exclusively interfaces. With the bonding driver loaded it
		// also holds `bonding_masters`, a regular FILE that is the driver's control surface, and on
		// two RDMA hosts it was the only non-symlink entry there. Reading it as an interface
		// published an entry carrying a name and nothing else, which reads as an interface whose
		// bus could not be determined.
		//
		// Skipping it is not the "publish an absence" this pass refuses: an entry that does not
		// resolve to a directory is not an interface at all, so recording one INVENTS a presence,
		// which is the same error in the other direction. Only a PROVEN non-directory is skipped —
		// an entry that could not be classified ends the pass, because skipping that one would
		// publish the absence after all.
		isDir, derr := tree.isDir(filepath.Join(sysfsNetClassDir, entry.Name()))
		if derr != nil {
			return nil, fmt.Errorf("classify the net class entry %s: %w", entry.Name(), derr)
		}
		if !isDir {
			continue
		}

		iface, isVF, ierr := tree.readInterface(entry.Name())
		if ierr != nil {
			return nil, ierr
		}
		if isVF {
			continue
		}
		out = append(out, iface)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
