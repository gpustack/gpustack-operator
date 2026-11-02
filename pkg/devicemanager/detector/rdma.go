// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package detector

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpustack.ai/gpustack/pkg/device"
)

// rdmaPortAttrMaxEntries bounds the per-port GID and P-Key tables and their netdev association
// entries. A port publishing more than this carries the first entries and a diagnostic saying so,
// so an empty value never has to stand in for "the rest would not fit".
const rdmaPortAttrMaxEntries = 256

// RDMAInterfaceSnapshot returns a copy-isolated view of this process's latest host verbs
// observation, or nil while discovery has not completed here — the initial and post-restart
// state, which is unknown rather than empty.
func (d *Detector) RDMAInterfaceSnapshot() *device.RDMASnapshot {
	return d.rdmaInterfaceSnapshot.Load().DeepCopy()
}

// refreshRDMAInterfaces folds one discovery pass into the local snapshot and stores it.
//
// A failed pass retains the last successful devices, revision, and time — a read that failed is
// not evidence the hardware left — and marks the snapshot incomplete with the failure in Message.
// With no observation retained there is nothing to retain, and the state stays unknown. A
// successful pass replaces the observation whole and stamps the pass's completion time; the
// revision covers the allocation-relevant facts only, so unchanged hardware keeps its revision
// while the time moves.
//
// The stored snapshot and the returned value are copies: the caller keeps its input and its
// return, so neither may reach the stored state by reference.
func (d *Detector) refreshRDMAInterfaces(detected []device.RDMADevice, detectedErr error, now time.Time) *device.RDMASnapshot {
	current := d.rdmaInterfaceSnapshot.Load()

	var next *device.RDMASnapshot
	switch {
	case detectedErr != nil && current == nil:
		return nil
	case detectedErr != nil:
		retained := current.DeepCopy()
		retained.Complete = false
		retained.Message = detectedErr.Error()
		next = retained
	default:
		next = (&device.RDMASnapshot{
			Devices:            detected,
			Complete:           true,
			Revision:           device.RDMASnapshotRevision(detected),
			LastSuccessfulTime: now,
		}).DeepCopy()
	}
	d.rdmaInterfaceSnapshot.Store(next)
	return next.DeepCopy()
}

// rdmaInterfacesChanged reports whether a fresh observation must take a detect round.
//
// A failed read takes the round only to mark incompleteness over a stored complete observation:
// with no observation retained there is nothing to revise, and with one already incomplete the
// failure is already reported, so repeating on every tick would spin without new evidence.
// Recovery is the first successful read, which differs from an incomplete record on Complete
// alone; after that only the revision — the allocation-relevant facts — triggers, so a timestamp
// or a diagnostic alone never does. The comparison only decides whether an observation also
// takes a detect round: the detect pass and every monitor round save the read they made, so a
// diagnostic-only difference reaches the stored record without one.
func rdmaInterfacesChanged(current *device.RDMASnapshot, detected []device.RDMADevice, detectedErr error) bool {
	if detectedErr != nil {
		return current != nil && current.Complete
	}
	if current == nil || !current.Complete {
		return true
	}
	return current.Revision != device.RDMASnapshotRevision(detected)
}

// enumerateRDMAInterfaces walks the RDMA class at root independently of the interface pass and
// returns one record per verbs device, ordered by name.
//
// Only a class that is truly absent (ENOENT) enumerates as an EMPTY inventory; any other failure
// to establish what the class holds ends the pass with an error instead, because publishing an
// empty list would claim hardware that is not there. A class that exists but cannot be listed,
// or an entry whose necessary parts cannot be read, ends the pass the same way. An OPTIONAL read
// failing — an attribute, a GID, a netdev association — only leaves that value absent and says
// so in the record's Message.
func enumerateRDMAInterfaces(root string) ([]device.RDMADevice, error) {
	s, err := newSysfsTree(root)
	if err != nil {
		return nil, fmt.Errorf("open sysfs: %w", err)
	}

	present, err := rdmaPathPresent(s, sysfsInfinibandDir)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", sysfsInfinibandDir, err)
	}
	if !present {
		return []device.RDMADevice{}, nil
	}
	entries, err := s.readDir(sysfsInfinibandDir)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", sysfsInfinibandDir, err)
	}

	devices := make([]device.RDMADevice, 0, len(entries))
	for _, entry := range entries {
		entryRel := filepath.Join(sysfsInfinibandDir, entry.Name())
		dir, err := rdmaEntryDir(s, entryRel)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", entry.Name(), err)
		}
		if !dir {
			continue
		}
		dev, err := enumerateRDMAInterface(s, entry.Name())
		if err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	sort.Slice(devices, func(a, b int) bool { return devices[a].Name < devices[b].Name })
	return devices, nil
}

// rdmaPathPresent reports whether a path exists, distinguishing a true absence (ENOENT, the
// ordinary "not there") from a stat that failed for any other reason and leaves the caller
// unable to say what the path holds.
func rdmaPathPresent(s *sysfsTree, rel string) (bool, error) {
	_, err := os.Lstat(filepath.Join(s.realRoot, rel))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

// rdmaEntryDir reports whether one entry a class or ports listing produced is a directory, at
// the boundary where the entry is a NECESSARY part of the enumeration.
//
// An entry whose NAME is already gone is skipped: the listing answered what existed when it ran,
// and a device removed in between is an ordinary race the next pass observes. An entry whose
// name still exists but whose target cannot be resolved — a dangling link, a path leaving the
// root — is not an absence, and a stat failing for any other reason is not one either. All of
// those end the pass with an error, the same boundary the class and ports directories
// themselves sit behind: folding any of them into a skip would publish the remaining entries as
// the whole inventory.
func rdmaEntryDir(s *sysfsTree, rel string) (bool, error) {
	_, err := os.Lstat(filepath.Join(s.realRoot, rel))
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
	resolved, err := s.resolve(rel)
	if err != nil {
		return false, fmt.Errorf("still listed but unreadable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// enumerateRDMAInterface reads one verbs device: its identity evidence, its parent's coordinates,
// and its ports.
//
// The device link to a parent is optional — software RDMA has none to speak of — so a missing one
// leaves the PCI fields empty as a kind of device, while one that exists but cannot be read is a
// diagnostic on the record rather than a dropped field. Every entry is kept under its observed
// name, so two verbs devices sharing a parent stay two records.
func enumerateRDMAInterface(s *sysfsTree, name string) (device.RDMADevice, error) {
	dev := device.RDMADevice{Name: name}
	var notes []string

	devRel := filepath.Join(sysfsInfinibandDir, name)
	dev.NodeGuid = rdmaOptionalAttr(s, filepath.Join(devRel, "node_guid"), "node_guid", &notes)
	dev.SysImageGuid = rdmaOptionalAttr(s, filepath.Join(devRel, "sys_image_guid"), "sys_image_guid", &notes)

	resolved, err := s.resolve(filepath.Join(devRel, "device"))
	switch {
	case err != nil && os.IsNotExist(err):
		// No parent device: software RDMA and its peers. The record keeps its observed name.
	case err != nil:
		notes = append(notes, fmt.Sprintf("device link: %v", err))
	default:
		readRDMAParent(s, resolved, &dev, &notes)
	}

	ports, err := enumerateRDMAPorts(s, devRel, &notes)
	if err != nil {
		return device.RDMADevice{}, err
	}
	dev.Ports = ports
	dev.Owner = rdmaDeviceOwner(dev.Driver)
	dev.Message = strings.Join(notes, "; ")
	return dev, nil
}

// rdmaOptionalAttr reads one optional attribute for the record's notes: a true absence is silent
// and empty, and any other read failure is a diagnostic rather than a silent unknown.
func rdmaOptionalAttr(s *sysfsTree, rel, what string, notes *[]string) string {
	v, err := s.attr(rel)
	switch {
	case err == nil:
		return v
	case os.IsNotExist(err):
		return ""
	default:
		*notes = append(*notes, fmt.Sprintf("%s: %v", what, err))
		return ""
	}
}

// readRDMAParent fills the parent-derived identity of one verbs device from the resolved device
// directory: PCI coordinates, bound driver, NUMA and CPU affinity, and the physical-function
// relation for a virtual function.
func readRDMAParent(s *sysfsTree, resolved string, dev *device.RDMADevice, notes *[]string) {
	rel, err := s.relTo(resolved)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("device path: %v", err))
		return
	}
	if bus, virtual := s.classifyBus(resolved); bus == "pci" && !virtual {
		dev.PciBusID = filepath.Base(rel)
		dev.AllocationUnit = dev.PciBusID
	}
	dev.Driver = rdmaDriverOf(s, rel, notes)
	if numa := rdmaOptionalAttr(s, filepath.Join(rel, "numa_node"), "numa_node", notes); numa != "" && numa != "-1" {
		dev.NumaAffinity = numa
	}
	dev.CpuAffinity = rdmaOptionalAttr(s, filepath.Join(rel, "local_cpulist"), "local_cpulist", notes)

	physfnPresent, err := rdmaPathPresent(s, filepath.Join(rel, "physfn"))
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("physfn: %v", err))
		return
	}
	if !physfnPresent {
		return
	}
	dev.VirtualFunction = true
	pfResolved, err := s.resolve(filepath.Join(rel, "physfn"))
	if err != nil {
		if !os.IsNotExist(err) {
			*notes = append(*notes, fmt.Sprintf("physfn link: %v", err))
		}
		return
	}
	if pfRel, err := s.relTo(pfResolved); err == nil {
		if bus, virtual := s.classifyBus(pfResolved); bus == "pci" && !virtual {
			dev.ParentPciBusID = filepath.Base(pfRel)
		}
	}
}

// rdmaDriverOf names the driver bound to the device directory, read from its driver symlink.
// A missing link is the ordinary no-driver case; a link that cannot be resolved for any other
// reason is a diagnostic, because ownership is judged on what it names.
func rdmaDriverOf(s *sysfsTree, deviceRel string, notes *[]string) string {
	resolved, err := s.resolve(filepath.Join(deviceRel, "driver"))
	if err != nil {
		if !os.IsNotExist(err) {
			*notes = append(*notes, fmt.Sprintf("driver link: %v", err))
		}
		return ""
	}
	return filepath.Base(resolved)
}

// rdmaDeviceOwner judges ownership on driver evidence alone: a driver this project knows is
// another owner's names the device external, and everything else — including an unreadable
// driver and an unknown network purpose — stays an ordinary candidate.
func rdmaDeviceOwner(driver string) device.RDMADeviceOwner {
	if driver == "efa" {
		return device.RDMADeviceOwnerExternal
	}
	return device.RDMADeviceOwnerGPUStack
}

// enumerateRDMAPorts reads the device's ports, ordered by number. A ports directory that is truly
// absent enumerates as portless — an observation, not a failure — while any other failure to
// establish it, or a directory that cannot be listed, ends the pass: a portless record read from
// a failed stat would claim the device has no ports.
func enumerateRDMAPorts(s *sysfsTree, devRel string, notes *[]string) ([]device.RDMAPort, error) {
	portsRel := filepath.Join(devRel, "ports")
	present, err := rdmaPathPresent(s, portsRel)
	if err != nil {
		return nil, fmt.Errorf("stat ports of %s: %w", filepath.Base(devRel), err)
	}
	if !present {
		return nil, nil
	}
	entries, err := s.readDir(portsRel)
	if err != nil {
		return nil, fmt.Errorf("list ports of %s: %w", filepath.Base(devRel), err)
	}

	ports := make([]device.RDMAPort, 0, len(entries))
	for _, entry := range entries {
		number, err := strconv.ParseUint(entry.Name(), 10, 32)
		if err != nil {
			continue
		}
		entryRel := filepath.Join(portsRel, entry.Name())
		dir, err := rdmaEntryDir(s, entryRel)
		if err != nil {
			return nil, fmt.Errorf("inspect port %s of %s: %w", entry.Name(), filepath.Base(devRel), err)
		}
		if !dir {
			continue
		}
		port := enumerateRDMAPort(s, entryRel, uint32(number), notes)
		ports = append(ports, port)
	}
	sort.Slice(ports, func(a, b int) bool { return ports[a].Number < ports[b].Number })
	return ports, nil
}

// enumerateRDMAPort reads one port: the attributes it publishes and the netdevs its GID
// attributes associate with it. Every attribute is optional: a true absence means the port did
// not report a value this pass, and an unreadable one is a diagnostic.
func enumerateRDMAPort(s *sysfsTree, portRel string, number uint32, notes *[]string) device.RDMAPort {
	port := device.RDMAPort{Number: number}

	port.LinkLayer = rdmaOptionalAttr(s, filepath.Join(portRel, "link_layer"), fmt.Sprintf("port %d link_layer", number), notes)
	if v := rdmaOptionalAttr(s, filepath.Join(portRel, "state"), fmt.Sprintf("port %d state", number), notes); v != "" {
		port.State = sysfsEnumName(v)
	}
	if v := rdmaOptionalAttr(s, filepath.Join(portRel, "phys_state"), fmt.Sprintf("port %d phys_state", number), notes); v != "" {
		port.PhysicalState = sysfsEnumName(v)
	}
	port.Rate = rdmaOptionalAttr(s, filepath.Join(portRel, "rate"), fmt.Sprintf("port %d rate", number), notes)
	port.LID = rdmaOptionalAttr(s, filepath.Join(portRel, "lid"), fmt.Sprintf("port %d lid", number), notes)
	port.SMLID = rdmaOptionalAttr(s, filepath.Join(portRel, "sm_lid"), fmt.Sprintf("port %d sm_lid", number), notes)

	port.GIDs = enumerateRDMAPortEntries(s, filepath.Join(portRel, "gids"), number, "gids", notes)
	port.PKeys = enumerateRDMAPortEntries(s, filepath.Join(portRel, "pkeys"), number, "pkeys", notes)
	port.NetDevs = enumerateRDMAPortNetdevs(s, portRel, number, notes)
	return port
}

// enumerateRDMAPortEntries reads a bounded per-port entry table — GIDs or P-Keys — in index
// order. The bound is on entries ATTEMPTED, not values collected, so a table longer than the
// bound reports the same truncation whatever share of its entries read empty. Unreadable entries
// and truncation are diagnostics on the device record; they never fail the pass.
func enumerateRDMAPortEntries(s *sysfsTree, tableRel string, number uint32, kind string, notes *[]string) []string {
	present, err := rdmaPathPresent(s, tableRel)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("port %d %s: %v", number, kind, err))
		return nil
	}
	if !present {
		return nil
	}
	entries, err := s.readDir(tableRel)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("port %d %s: %v", number, kind, err))
		return nil
	}

	indexes := make([]int, 0, len(entries))
	for _, entry := range entries {
		index, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	values := make([]string, 0, len(indexes))
	truncated := false
	for attempted := 0; attempted < len(indexes); attempted++ {
		if attempted == rdmaPortAttrMaxEntries {
			truncated = true
			break
		}
		v, err := s.attr(filepath.Join(tableRel, strconv.Itoa(indexes[attempted])))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			*notes = append(*notes, fmt.Sprintf("port %d %s/%d: %v", number, kind, indexes[attempted], err))
			continue
		}
		if v != "" {
			values = append(values, v)
		}
	}
	if truncated {
		*notes = append(*notes,
			fmt.Sprintf("port %d %s truncated at %d entries", number, kind, rdmaPortAttrMaxEntries))
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

// enumerateRDMAPortNetdevs names the network interfaces the port's GID attributes associate with
// it, deduplicated and ordered by name. Each entry is a text attribute whose content is the
// netdev's name, as the kernel writes it; the bound is on entries attempted, not names kept. A
// port with no association table — a native InfiniBand port without one — reports none, and an
// entry that cannot be read is a diagnostic.
func enumerateRDMAPortNetdevs(s *sysfsTree, portRel string, number uint32, notes *[]string) []string {
	ndevsRel := filepath.Join(portRel, "gid_attrs", "ndevs")
	present, err := rdmaPathPresent(s, ndevsRel)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("port %d netdevs: %v", number, err))
		return nil
	}
	if !present {
		return nil
	}
	entries, err := s.readDir(ndevsRel)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("port %d netdevs: %v", number, err))
		return nil
	}

	seen := make(map[string]struct{}, len(entries))
	var names []string
	truncated := false
	for attempted := 0; attempted < len(entries); attempted++ {
		if attempted == rdmaPortAttrMaxEntries {
			truncated = true
			break
		}
		name, err := s.attr(filepath.Join(ndevsRel, entries[attempted].Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			*notes = append(*notes, fmt.Sprintf("port %d netdev %s: %v", number, entries[attempted].Name(), err))
			continue
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if truncated {
		*notes = append(*notes,
			fmt.Sprintf("port %d netdevs truncated at %d entries", number, rdmaPortAttrMaxEntries))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	return names
}
