package amd

import (
	"fmt"
	"sync"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/hsa"
)

// The HSA handle and the per-card topology it answered with, both guarded by one mutex.
//
// The cache is not an optimization, it is what makes the caller's contract true. readTopology is
// invoked from PlaceLogicalSliced, which runs under the device-plugin's node-wide allocate mutex,
// and an uncached read is a dlopen, an agent walk and nine cgo queries per agent — held there, it
// would stall Allocate for every OTHER vendor on this node, not only for AMD. A card's topology is
// immutable for the life of the process (compute units, shader engines and XCCs do not change
// under a running driver), so one read per card is all that is ever needed.
//
// The handle is memoized only on SUCCESS. Latching a transient initialization failure would refuse
// every sliced allocation on the node until the device-manager restarts, which is a much longer
// outage than the fault that caused it.
var (
	hsaMutex      sync.Mutex
	hsaLib        *hsa.HSA
	topologyCache = map[string]Topology{}
)

// readTopology returns the HSA-reported topology of the card at pciBusID, whose accelerator UUID
// is cardUUID.
//
// The card is matched to its agent by PCI BDF first and by UUID only as a fallback, which is how
// the AMD detector pairs the two enumerations; the fallback exists because an agent that could not
// report its BDF is keyed by its UUID instead. A card no agent answers for is an error naming the
// card: a zero-valued Topology is a card with no compute units, and reporting one as a topology
// would hand the arithmetic a shape it can only refuse for the wrong reason.
//
// The numeric fields arrive as whatever binding/hsa could read, an unreadable one as zero. Judging
// them is Topology.Validate's job, not this one's.
func readTopology(pciBusID, cardUUID string) (Topology, error) {
	hsaMutex.Lock()
	defer hsaMutex.Unlock()

	if topo, cached := topologyCache[cardUUID]; cached {
		return topo, nil
	}

	if hsaLib == nil {
		// The logger is passed for the same reason the detector passes one: a dlopen or dlsym
		// failure inside the binding is otherwise silent, and this is the only caller that would
		// have reported it.
		lib := hsa.New(binding.WithLogger(klog.Background().WithName("amd").WithName("cumask")))
		if ret := lib.Init(); !ret.IsSuccess() {
			return Topology{}, fmt.Errorf("hsa init failed: %w", ret)
		}
		hsaLib = lib
	}

	agents := hsaLib.GetAgents()
	agent, ok := agents[pciBusID]
	if !ok {
		agent, ok = agents[cardUUID]
	}
	if !ok {
		return Topology{}, fmt.Errorf("no hsa agent reports card %s (pci bus id %q)", cardUUID, pciBusID)
	}

	topo := Topology{
		Name:    agent.Name,
		CU:      int(agent.ComputeUnitCount),
		SE:      int(agent.NumShaderEngines),
		SAPerSE: int(agent.NumShaderArraysPerSE),
		XCC:     int(agent.NumXcc),
	}
	topologyCache[cardUUID] = topo
	return topo, nil
}
