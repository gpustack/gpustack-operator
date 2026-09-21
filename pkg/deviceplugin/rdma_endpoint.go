package deviceplugin

import (
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/pkg/utils/slicex"
)

// rdmaEndpoint is one allocatable RDMA endpoint of a network interface, with the facts an
// advertisement and an allocation read from the inventory record.
//
// It is derived, never stored: every field is read out of Devices.spec.interfaces[] on each call,
// so a fact the detector has since corrected cannot survive into a response built from an older
// one.
type rdmaEndpoint struct {
	// Resource is the locator naming the endpoint, built by endpointOf.
	Resource Resource

	// RDMADevice is the bound RDMA device's name, which an allocation resolves to a character
	// device. Non-empty by construction: an endpoint with no name has nothing to hand over and
	// is not returned as an endpoint at all.
	RDMADevice string

	// Topology is the endpoint's NUMA hint, built by numaTopologyOf. Nil when neither the
	// endpoint nor its parent reported an affinity.
	Topology *deviceplugin.TopologyInfo

	// Link is the endpoint's link verdict, which an advertisement's health gate reads. Nil when
	// no verdict was recorded.
	Link *workercore.DeviceInterfaceLink
}

// endpointOf returns the Resource naming one RDMA endpoint: its physical function, and the
// endpoint itself. The two are equal for an interface serving a whole-function mode, and differ
// for a virtual function, which is what lets one lookup reach both without a second key.
//
// It is a locator, not a record: everything a response needs — the RDMA device name, the NUMA
// affinity, the link verdict — is read back out of Devices.spec.interfaces[] at the time it is
// needed, so a token can never carry a stale copy of a fact the detector has since corrected.
func endpointOf(iface *workercore.DeviceInterface, endpoint string) Resource {
	return Resource{Group: iface.Name, Device: endpoint}
}

// rdmaEndpoints returns the RDMA endpoints the interface advertises under the allocation mode,
// with the mode read off the interface's recorded state rather than chosen.
//
// An SR-IOV physical function with virtual functions configured serves Partitioned only, each
// virtual function one endpoint: the node was put into that partitioned state before this process
// started and nothing here can change it at run time, so the interface does not also serve the
// whole-function modes. Every other interface — a physical function with zero virtual functions
// configured, or not a physical function at all — serves Exclusive and Shared, itself the one
// endpoint.
//
// Being a physical function and having virtual functions configured are separate facts in the
// record, and both are read: deciding the branch from the virtual-function count alone would send
// a record carrying functions without the physical-function fact into Partitioned, and reading the
// fact alone would send a physical function with no virtual functions there, where it has nothing
// to offer.
//
// An endpoint with no RDMA device name is not returned, in any mode: the name is what an
// allocation resolves to a character device, so an endpoint without one has nothing to hand over.
// Visibility, None and any unrecognized mode likewise return nothing — a visibility allocation
// names an endpoint another container of the same Pod holds, and no RDMA allocation record is
// written to answer that from, and an unknown mode must not silently take the whole-function
// branch.
func rdmaEndpoints(iface *workercore.DeviceInterface, mode workercore.DeviceAllocationMode) []rdmaEndpoint {
	partitioned := iface.SRIOV && len(iface.VirtualFunctions) > 0

	switch mode {
	case workercore.DeviceAllocationModePartitioned:
		if !partitioned {
			return nil
		}
		endpoints := make([]rdmaEndpoint, 0, len(iface.VirtualFunctions))
		for i := range iface.VirtualFunctions {
			vf := &iface.VirtualFunctions[i]
			if vf.RDMADevice == "" {
				continue
			}
			endpoints = append(endpoints, rdmaEndpoint{
				Resource:   endpointOf(iface, vf.Name),
				RDMADevice: vf.RDMADevice,
				Topology:   numaTopologyOf(vf.NumaAffinity, iface.NumaAffinity),
				Link:       vf.Link,
			})
		}
		return endpoints

	case workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared:
		if partitioned || iface.RDMADevice == "" {
			return nil
		}
		return []rdmaEndpoint{{
			Resource:   endpointOf(iface, iface.Name),
			RDMADevice: iface.RDMADevice,
			Topology:   numaTopologyOf(iface.NumaAffinity, ""),
			Link:       iface.Link,
		}}
	}

	return nil
}

// numaTopologyOf builds the NUMA hint for one RDMA endpoint from its own affinity, falling back
// to the parent's when the endpoint's own is blank — a blank reading is the kernel declining to
// answer, not a different answer, and a virtual function sits on the same physical function as
// its parent.
//
// It returns nil when neither answers: an empty affinity is the -1 a virtualized host can report
// for every device it has, and no node is named rather than node 0, which under the
// single-numa-node policy would decide admission against a proximity nobody measured.
//
// The affinity is read through the same range parser the accelerator path reads its topology
// with, so the two cannot come to disagree about what an affinity string means.
func numaTopologyOf(own, parent string) *deviceplugin.TopologyInfo {
	affinity := own
	if affinity == "" {
		affinity = parent
	}

	var topology *deviceplugin.TopologyInfo
	if numa := binding.StrRangeToList(affinity); len(numa) > 0 {
		topology = &deviceplugin.TopologyInfo{
			Nodes: slicex.Transform(numa, func(n int) *deviceplugin.NUMANode {
				return &deviceplugin.NUMANode{
					ID: int64(n),
				}
			}),
		}
	}
	return topology
}
