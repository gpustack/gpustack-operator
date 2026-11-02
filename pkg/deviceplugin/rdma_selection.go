// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"maps"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

// rdmaAppliedSelection holds the active HCA candidate set resolved for this node.
type rdmaAppliedSelection struct {
	selected map[string]struct{}
}

// clone returns a private copy of the applied selection.
func (s *rdmaAppliedSelection) clone() *rdmaAppliedSelection {
	if s == nil {
		return nil
	}
	return &rdmaAppliedSelection{
		selected: maps.Clone(s.selected),
	}
}

// rdmaInterfaceSnapshot is one capture of the node's interface inventory, verbs inventory, and
// active policy selection, from which an entire response — an advertisement, a preferred allocation,
// or one container's allocation — is derived.
//
// Both RDMA response paths share one rule chain: capture the inventory once per response, derive
// every endpoint the response names from that capture, and resolve every granted token against
// it. The capture is what keeps a response from being built out of two revisions of the record: a
// detector write or policy change that lands after the capture corrects the next response, never
// rearranges the one being built.
//
// The interfaces are a private deep copy, not a reference into the live record. A verdict the
// detector corrects in place, a virtual function it rebinds, an interface it replaces or an
// entry it appends after the capture share no memory with the capture, so an endpoint derived
// late in the response's construction reads the same facts one derived early did.
type rdmaInterfaceSnapshot struct {
	// interfaces mirrors Devices.spec.interfaces at capture time, augmented with any netdev-free
	// GPUStack-owned verbs devices from the local RDMA snapshot. Unexported and never written
	// after capture, so everything derived from it is frozen too.
	interfaces []workercore.DeviceInterface

	// externalHCAs names devices whose authoritative owner is external to GPUStack.
	externalHCAs map[string]struct{}

	// selectionKnown reports whether an active RDMA selection has been established on the
	// reconciler. When false, every non-external HCA is eligible under the default rules.
	selectionKnown bool

	// selectedHCAs is the set of HCA names eligible when selectionKnown is true.
	selectedHCAs map[string]struct{}
}

// captureRDMAInterfaceSnapshot freezes the node's interface inventory from Devices.spec.interfaces
// alone.
func captureRDMAInterfaceSnapshot(devs *workercore.Devices) rdmaInterfaceSnapshot {
	return captureRDMAInterfaceSnapshotFrom(devs, nil, nil)
}

// captureRDMAInterfaceSnapshotFrom freezes the node's interface inventory, augmenting it with any
// netdev-free GPUStack-owned verbs devices from inv and binding the active policy selection sel.
func captureRDMAInterfaceSnapshotFrom(
	devs *workercore.Devices,
	inv *device.RDMASnapshot,
	sel *rdmaAppliedSelection,
) rdmaInterfaceSnapshot {
	var interfaces []workercore.DeviceInterface
	if devs != nil && len(devs.Spec.Interfaces) > 0 {
		interfaces = make([]workercore.DeviceInterface, 0, len(devs.Spec.Interfaces))
		for i := range devs.Spec.Interfaces {
			interfaces = append(interfaces, *devs.Spec.Interfaces[i].DeepCopy())
		}
	}

	var externalHCAs map[string]struct{}
	if inv != nil && inv.Complete {
		boundHCAs := make(map[string]struct{}, len(interfaces))
		pfByPCI := make(map[string]int, len(interfaces))
		for i := range interfaces {
			iface := &interfaces[i]
			if iface.RDMADevice != "" {
				boundHCAs[iface.RDMADevice] = struct{}{}
			}
			if iface.PciBusID != "" {
				pfByPCI[iface.PciBusID] = i
			}
			for j := range iface.VirtualFunctions {
				vf := &iface.VirtualFunctions[j]
				if vf.RDMADevice != "" {
					boundHCAs[vf.RDMADevice] = struct{}{}
				}
			}
		}

		for i := range inv.Devices {
			dev := &inv.Devices[i]
			if dev.Name == "" {
				continue
			}
			if dev.Owner == device.RDMADeviceOwnerExternal {
				if externalHCAs == nil {
					externalHCAs = make(map[string]struct{})
				}
				externalHCAs[dev.Name] = struct{}{}
				continue
			}
			if dev.VirtualFunction {
				continue
			}
			if _, bound := boundHCAs[dev.Name]; bound {
				if dev.PciBusID != "" {
					if _, exists := pfByPCI[dev.PciBusID]; !exists {
						for idx := range interfaces {
							if interfaces[idx].RDMADevice == dev.Name {
								pfByPCI[dev.PciBusID] = idx
								break
							}
						}
					}
				}
				continue
			}
			interfaces = append(interfaces, workercore.DeviceInterface{
				Name:         dev.Name,
				PciBusID:     dev.PciBusID,
				NumaAffinity: dev.NumaAffinity,
				CpuAffinity:  dev.CpuAffinity,
				RDMA:         true,
				RDMADevice:   dev.Name,
				Link:         rdmaLinkFromPorts(dev.Ports),
			})
			boundHCAs[dev.Name] = struct{}{}
			if dev.PciBusID != "" {
				if _, exists := pfByPCI[dev.PciBusID]; !exists {
					pfByPCI[dev.PciBusID] = len(interfaces) - 1
				}
			}
		}

		for i := range inv.Devices {
			dev := &inv.Devices[i]
			if dev.Name == "" || dev.Owner == device.RDMADeviceOwnerExternal || !dev.VirtualFunction {
				continue
			}
			if _, bound := boundHCAs[dev.Name]; bound {
				continue
			}
			vf := workercore.DeviceInterfaceVirtualFunction{
				Name:         dev.Name,
				PciBusID:     dev.PciBusID,
				NumaAffinity: dev.NumaAffinity,
				CpuAffinity:  dev.CpuAffinity,
				RDMA:         true,
				RDMADevice:   dev.Name,
				Link:         rdmaLinkFromPorts(dev.Ports),
			}
			if dev.ParentPciBusID != "" {
				if pfIdx, ok := pfByPCI[dev.ParentPciBusID]; ok {
					interfaces[pfIdx].SRIOV = true
					interfaces[pfIdx].VirtualFunctions = append(interfaces[pfIdx].VirtualFunctions, vf)
					boundHCAs[dev.Name] = struct{}{}
					continue
				}
			}
			parentName := dev.ParentPciBusID
			if parentName == "" {
				parentName = dev.Name
			}
			interfaces = append(interfaces, workercore.DeviceInterface{
				Name:             parentName,
				PciBusID:         dev.ParentPciBusID,
				NumaAffinity:     dev.NumaAffinity,
				SRIOV:            true,
				RDMA:             true,
				VirtualFunctions: []workercore.DeviceInterfaceVirtualFunction{vf},
			})
			if dev.ParentPciBusID != "" {
				pfByPCI[dev.ParentPciBusID] = len(interfaces) - 1
			}
			boundHCAs[dev.Name] = struct{}{}
		}
	}

	snap := rdmaInterfaceSnapshot{
		interfaces:   interfaces,
		externalHCAs: externalHCAs,
	}
	if sel != nil {
		snap.selectionKnown = true
		snap.selectedHCAs = maps.Clone(sel.selected)
	}
	return snap
}

// isHCAEligible reports whether the named HCA is eligible for healthy advertisement, preferred
// allocation, and container device injection under this snapshot.
func (s rdmaInterfaceSnapshot) isHCAEligible(hca string) bool {
	if hca == "" {
		return false
	}
	if _, external := s.externalHCAs[hca]; external {
		return false
	}
	if s.selectionKnown {
		_, ok := s.selectedHCAs[hca]
		return ok
	}
	return true
}

// rdmaLinkFromPorts derives the link verdict for a synthesized verbs device from its observed
// ports, following the same rule the detector uses for netdev-bound RDMA links.
func rdmaLinkFromPorts(ports []device.RDMAPort) *workercore.DeviceInterfaceLink {
	if len(ports) == 0 {
		return &workercore.DeviceInterfaceLink{
			State: workercore.DeviceInterfaceLinkStateUnverified,
		}
	}
	unreadable := false
	for i := range ports {
		p := &ports[i]
		if p.State == "ACTIVE" && p.PhysicalState == "LinkUp" {
			return &workercore.DeviceInterfaceLink{
				State: workercore.DeviceInterfaceLinkStateOK,
			}
		}
		if p.State == "" || p.PhysicalState == "" {
			unreadable = true
		}
	}
	if unreadable {
		return &workercore.DeviceInterfaceLink{
			State: workercore.DeviceInterfaceLinkStateUnverified,
		}
	}
	return &workercore.DeviceInterfaceLink{
		State: workercore.DeviceInterfaceLinkStateFailed,
	}
}
