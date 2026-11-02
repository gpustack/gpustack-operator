// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// RDMADeviceOwner says who allocates a host verbs device.
//
// Only authoritative driver or transport evidence about the device itself moves a record off the
// GPUStack default; a device's name, speed, or the number of networks around it never does.
type RDMADeviceOwner string

const (
	// RDMADeviceOwnerGPUStack marks a device as an ordinary candidate under the existing
	// allocation and health rules. Discovery writes it whenever no external evidence names the
	// device.
	RDMADeviceOwnerGPUStack RDMADeviceOwner = "GPUStack"
	// RDMADeviceOwnerExternal marks a device that authoritative evidence places under another
	// owner's allocation, such as the AWS EFA driver. The observation still records the device.
	RDMADeviceOwnerExternal RDMADeviceOwner = "External"
)

// RDMASnapshot is one node-local observation of the host's verbs devices.
//
// It is memory-only state inside one device manager: not an API inventory, not a route, and not
// persisted. A nil snapshot is the initial and post-restart state and means discovery has not
// completed on this process yet — unknown, not empty.
type RDMASnapshot struct {
	// Devices are the verbs devices observed in the RDMA class, one record per device.
	Devices []RDMADevice

	// Complete reports whether Devices came from a successful enumeration. False keeps the
	// last successful devices, revision, and time in place and says so through Message.
	Complete bool

	// Revision is RDMASnapshotRevision over Devices — a digest of allocation-relevant facts
	// only, so it is nonempty for every successful enumeration, including an empty inventory.
	Revision string

	// LastSuccessfulTime is when the last successful enumeration behind this snapshot
	// completed. A failed pass retains the previous value.
	LastSuccessfulTime time.Time

	// Message is why Complete is false, and empty otherwise.
	Message string
}

// RDMADevice is one host verbs device observed in the RDMA class.
//
// Every field is an observation of this pass. A name, path, or GUID is evidence, not a claim
// that the hardware behind two equal observations is the same hardware.
type RDMADevice struct {
	// Name is the kernel's RDMA device name, e.g. mlx5_0, as observed this pass.
	Name string

	// AllocationUnit is the observed coordinate of the verbs function this record describes.
	// For a PCI device it is the function's own BDF, a VF's own included. It collides for two
	// verbs devices sharing one parent — the records stay distinct by Name — and it is empty
	// for a device with no PCI coordinate. It is not an allocation identity by itself.
	AllocationUnit string

	// Owner is who allocates the device, judged on Driver evidence.
	Owner RDMADeviceOwner

	// Driver is the kernel driver bound to the device this RDMA device hangs from, read from
	// its driver symlink. Empty when there is no such device or it could not be read.
	Driver string

	// PciBusID is the PCI address of the device this RDMA device hangs from. Empty for a
	// device reached over no PCI parent — software RDMA included.
	PciBusID string

	// ParentPciBusID is the physical function's PCI address when VirtualFunction is set and
	// its physfn link resolved.
	ParentPciBusID string

	// VirtualFunction reports that this device serves a SR-IOV virtual function, read from
	// the physfn link's presence.
	VirtualFunction bool

	// NodeGuid is the node GUID the device reports, when it published one.
	NodeGuid string

	// SysImageGuid is the system image GUID the device reports, when it published one.
	SysImageGuid string

	// NumaAffinity is the NUMA node the device is attached to. Empty means unknown.
	NumaAffinity string

	// CpuAffinity is the CPU cores close to the device. Empty means unknown.
	CpuAffinity string

	// Ports are the device's ports, one record per port directory, ordered by number.
	Ports []RDMAPort

	// Message carries this device's observation diagnostics — an optional read that failed,
	// a bounded list that was truncated. It never bears on Complete.
	Message string
}

// RDMAPort is one port of a host verbs device.
//
// Every attribute is optional: absent means it was not reported or not read this pass, never a
// fabricated negative fact.
type RDMAPort struct {
	// Number is the port number as the kernel names the directory — the N in ports/N.
	Number uint32

	// NetDevs are the network interfaces associated with this port through its GID
	// attributes, ordered by name. A port can back several and none.
	NetDevs []string

	// LinkLayer is the link layer the port reports, as the kernel writes it.
	LinkLayer string

	// State is the port's operational state, the name half of the kernel's reading.
	State string

	// PhysicalState is the port's physical link state, the name half of the same reading.
	PhysicalState string

	// Rate is the port's reported rate, verbatim.
	Rate string

	// LID is the port's local identifier, a subnet-local transport observation.
	LID string

	// SMLID is the LID of the subnet manager for the port's subnet.
	SMLID string

	// GIDs are the port's GIDs in index order, bounded by discovery.
	GIDs []string

	// PKeys are the port's partition keys in index order, bounded by discovery.
	PKeys []string
}

// DeepCopy returns a copy that shares no slice or backing array with the receiver, so a reader
// can hold and mutate it without touching the state the discovery pass stores. A nil receiver
// copies as nil.
func (s *RDMASnapshot) DeepCopy() *RDMASnapshot {
	if s == nil {
		return nil
	}
	out := *s
	out.Devices = copyRDMADevices(s.Devices)
	return &out
}

// DeepCopy returns a copy of one device record that shares no slice with the receiver.
func (d *RDMADevice) DeepCopy() *RDMADevice {
	if d == nil {
		return nil
	}
	out := *d
	out.Ports = copyRDMAPorts(d.Ports)
	return &out
}

// DeepCopy returns a copy of one port record that shares no slice with the receiver.
func (p *RDMAPort) DeepCopy() *RDMAPort {
	if p == nil {
		return nil
	}
	out := *p
	out.NetDevs = copyStrings(p.NetDevs)
	out.GIDs = copyStrings(p.GIDs)
	out.PKeys = copyStrings(p.PKeys)
	return &out
}

func copyRDMADevices(in []RDMADevice) []RDMADevice {
	if in == nil {
		return nil
	}
	out := make([]RDMADevice, len(in))
	for i := range in {
		out[i] = *in[i].DeepCopy()
	}
	return out
}

func copyRDMAPorts(in []RDMAPort) []RDMAPort {
	if in == nil {
		return nil
	}
	out := make([]RDMAPort, len(in))
	for i := range in {
		out[i] = *in[i].DeepCopy()
	}
	return out
}

func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// RDMASnapshotRevision digests the allocation-relevant facts of a device list into a stable
// short hex revision.
//
// The digest is independent of observation order and of everything that is not a fact: devices
// are taken by name, ports by number, and netdev aliases by name, while diagnostic messages and
// the snapshot's completeness and time never enter. An empty or nil list still digests, which is
// what lets a successful empty inventory publish a nonempty revision. Two lists carrying the
// same facts produce the same value. It is a compression, not an identity proof: a differing
// fact is expected to change it, and the tests pin the changes this project relies on, but equal
// digests alone never certify equal facts.
func RDMASnapshotRevision(devices []RDMADevice) string {
	canonical := make([]RDMADevice, 0, len(devices))
	for i := range devices {
		d := devices[i]
		d.Message = ""
		ports := make([]RDMAPort, 0, len(d.Ports))
		ports = append(ports, d.Ports...)
		sort.Slice(ports, func(a, b int) bool { return ports[a].Number < ports[b].Number })
		for j := range ports {
			p := ports[j]
			p.NetDevs = copyStrings(p.NetDevs)
			sort.Strings(p.NetDevs)
			ports[j] = p
		}
		d.Ports = ports
		canonical = append(canonical, d)
	}
	sort.Slice(canonical, func(a, b int) bool { return canonical[a].Name < canonical[b].Name })

	raw, err := json.Marshal(canonical)
	if err != nil {
		// The carried types are all JSON-marshalable values, so this cannot happen; a digest
		// still has to return something well-formed rather than panic.
		return "unmarshalable"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
