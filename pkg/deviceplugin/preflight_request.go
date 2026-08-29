package deviceplugin

import (
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// PreflightPodName, PreflightPodNamespace and PreflightPodUID name the synthetic Pod
// NewPreflightAllocationRequest fabricates. They are fixed rather than generated so that two calls
// with the same inputs — and any host path a responder derives from the Pod's UID and container
// name — agree.
const (
	PreflightPodName      = "preflight"
	PreflightPodNamespace = "default"
	PreflightPodUID       = types.UID("preflight")
	// PreflightContainerName names the single container the fabricated Pod carries.
	PreflightContainerName = "preflight"
)

// NewPreflightAllocationRequest fabricates the (*core.Pod, *core.Container, *workercore.Devices,
// map[Resource]int32) quadruple every ContainerAllocateResponder, LogicalSlicedResponder and
// PhysicalSlicedResponder takes, from what `device-manager preflight` has in hand instead of a
// kubelet Allocate call: the device groups a detect pass found, the manufacturer and allocation
// mode being probed, and the per-accelerator quota that mode commits — the units a real request
// would fold into the manufacturer's ".sliced.units" or ".partitioned.units" resource.
//
// It is the only place a preflight request is built, so every manufacturer's simulated depth asks
// the responder the same question. The Container's resource names come from pkg/nodefeature,
// exactly as a real Pod webhook would shape them; the returned Devices carries groups verbatim, as
// the ledger snapshot a real Allocate hands the responder does. Two calls with the same inputs
// always return equal values.
//
// It returns an error, never a request that silently under- or over-asks, when groups is empty,
// when quota is not positive or exceeds one whole accelerator, when manufacturer is not one this
// operator knows, when no group belongs to manufacturer, when manufacturer has no accelerator among
// those groups, when manufacturer names no resource for mode (a mode the manufacturer has no kind
// for — e.g. Sliced on a manufacturer with no logical slicing), or when mode is Partitioned, which
// no request built here could name a profile for and which this command answers by reading the
// driver instead.
func NewPreflightAllocationRequest(
	groups []workercore.DevicesGroup,
	manufacturer string,
	mode workercore.DeviceAllocationMode,
	quota int32,
) (*core.Pod, *core.Container, *workercore.Devices, map[Resource]int32, error) {
	if len(groups) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("no device groups given")
	}
	if quota <= 0 {
		return nil, nil, nil, nil, fmt.Errorf("quota must be positive, got %d", quota)
	}
	// Refused rather than clamped, because only one of the three places quota lands could be: the
	// allocation map charges whole accelerators and would cap, while the units limit and the two
	// percentages are figures the responder renders from and would carry the oversized ask through
	// -- a request asking for 150% of an accelerator whose allocation map says it got one.
	if quota > nodefeature.ResourceMaxUnits {
		return nil, nil, nil, nil, fmt.Errorf(
			"quota must not exceed one whole accelerator (%d units), got %d",
			nodefeature.ResourceMaxUnits, quota)
	}

	// Checked before any resource name is derived, because deriving one cannot catch it: the mode
	// suffixes are appended to whatever the lookup returned, and an unknown manufacturer's lookup
	// returns "" -- so asking about Sliced yields ".sliced", which is not empty and passes a check
	// on the derived name. Every key built below would then be a suffix with no vendor in front.
	if !nodefeature.IsKnownAcceleratableManufacturer(manufacturer) {
		return nil, nil, nil, nil, fmt.Errorf(
			"manufacturer %q is not an acceleratable manufacturer this operator knows", manufacturer)
	}

	// Checked here for the same reason the manufacturer is: deriving from it cannot catch it. Both
	// derivations answer an unrecognized mode from a default arm -- the resource name comes back as
	// the bare exclusive key and the unit count as a whole accelerator -- so None, which this type's
	// own doc calls unknown, and any value past the enum would be served as a silent request for the
	// entire card rather than refused.
	switch mode {
	case workercore.DeviceAllocationModeExclusive, workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced, workercore.DeviceAllocationModeVisibility:

	case workercore.DeviceAllocationModePartitioned:
		// Refused rather than served, because what this builder could produce for it is a request no
		// allocation accepts. A partition is named by a "<kind>-<profile>" limit that
		// choosePartitionCards reads back through partitionProfileOf, and there is no profile here to
		// write one from -- the sliced arm below has exactly such a block, and the partitioned case
		// has nothing to put in one. The request would carry the bare ".partitioned" key, pass every
		// check above, and be refused at allocation for naming no partition profile: a malformed
		// synthetic request dressed as a successful build.
		//
		// Nothing is lost by refusing. This command never simulates a partition allocation by design
		// -- PreflightResponder deliberately exposes no PhysicalSlicedResponder, because actuating one
		// would carve a partition no Pod owns -- so the partitioned capability is established by
		// reading the driver's own partition subtree instead. A caller who needs this mode needs a
		// profile parameter first, and this error says so rather than handing them a request that
		// fails later somewhere else.
		return nil, nil, nil, nil, fmt.Errorf(
			"allocation mode %s cannot be asked about through a preflight request: a partition is "+
				"named by a <kind>-<profile> limit this builder has no profile to write, and a "+
				"request carrying only the bare resource is refused at allocation for naming no "+
				"partition profile", mode)

	default:
		return nil, nil, nil, nil, fmt.Errorf(
			"allocation mode %s (%d) is not a mode a preflight can ask about", mode, uint32(mode))
	}

	// Non-empty for every mode still reachable here, and provably so rather than by inspection:
	// IsKnownAcceleratableManufacturer above is defined as this manufacturer having a resource name
	// at all, and Partitioned -- the one mode whose derivation can answer "" on top of that, for a
	// manufacturer with no partition kind -- is refused before this line.
	resName := nodefeature.GetAcceleratableResourceName(manufacturer, mode)

	var found bool
	allocation := make(map[Resource]int32)
	for i := range groups {
		grp := &groups[i]
		if grp.Manufacturer != manufacturer {
			continue
		}
		found = true
		for j := range grp.Accelerators {
			res := Resource{Group: grp.ID, Device: grp.Accelerators[j].ID}
			allocation[res] = preflightAllocatedUnits(mode, quota)
		}
	}
	if !found {
		return nil, nil, nil, nil, fmt.Errorf("no device group for manufacturer %q", manufacturer)
	}
	if len(allocation) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("manufacturer %q has no accelerator in the given groups", manufacturer)
	}

	limits := core.ResourceList{
		resName: *resource.NewQuantity(int64(len(allocation)), resource.DecimalSI),
	}
	// The units key is asked of the server itself rather than derived again here. A second switch
	// over the same two fields is a copy, and a copy of the key a request is shaped around is the
	// drift this whole request exists to avoid.
	srv := &ResourceServer{Manufacturer: manufacturer, AllocationMode: mode}
	if unitsName := srv.unitsResourceName(); unitsName != "" {
		limits[unitsName] = *resource.NewQuantity(int64(quota), resource.DecimalSI)
	}

	// A sliced request carries the two dimensions the slice is actually cut along, on top of the
	// units that count it. They are not decoration: SlicedMemoryMib refuses a container that asks
	// for neither memory figure, so a responder handed a request without them fails to render a
	// slice at all — measured on hardware, "container \"preflight\" has no
	// amd.com/gpu.sliced.memory-percentage or amd.com/gpu.sliced.memory-mib request", which reads
	// as a broken node rather than an under-specified ask.
	//
	// Both are derived from quota rather than written as a figure of their own, so the units the
	// request commits and the slice it describes cannot disagree: a half-accelerator quota asks for
	// half the memory and half the compute.
	if mode == workercore.DeviceAllocationModeSliced {
		// That agreement is only reachable for a quota the percentages can carry, and they carry
		// whole numbers only. Refused rather than rounded: rounding is what breaks the guarantee
		// above, leaving the request committing one figure in units and describing another in
		// percent -- 1234 units asking for the 1% that is 16000 of them, or 20000 rounded down to
		// the same 1%. Serving an ask nobody made is worse here than refusing one, because the
		// units are the half a scheduler charges against.
		if int64(quota)*100%nodefeature.ResourceMaxUnits != 0 {
			return nil, nil, nil, nil, fmt.Errorf(
				"a sliced quota must be a whole percent of an accelerator (a multiple of %d units), got %d",
				nodefeature.ResourceMaxUnits/100, quota)
		}
		pct := int64(quota) * 100 / nodefeature.ResourceMaxUnits
		limits[nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(manufacturer)] = *resource.NewQuantity(pct, resource.DecimalSI)
		limits[nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manufacturer)] = *resource.NewQuantity(pct, resource.DecimalSI)
	}

	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      PreflightPodName,
			Namespace: PreflightPodNamespace,
			UID:       PreflightPodUID,
		},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name:      PreflightContainerName,
				Resources: core.ResourceRequirements{Limits: limits},
			}},
		},
	}

	devs := &workercore.Devices{
		Spec: workercore.DevicesSpec{Groups: groups},
	}

	return pod, &pod.Spec.Containers[0], devs, allocation, nil
}

// preflightAllocatedUnits reports the per-accelerator units one simulated token commits under
// mode, mirroring ResourceServer.accumulateAllocation's own per-mode charge: a whole accelerator
// for Exclusive (and any other family with no unit shape of its own), one Shared-family share, or
// quota itself for Sliced/Partitioned — the two families whose real cost the units resource
// carries.
//
// quota is already known not to exceed one whole accelerator: its caller refuses one that does,
// because capping here while the units limit and the percentages kept the oversized figure is
// exactly the disagreement that refusal exists to prevent.
func preflightAllocatedUnits(mode workercore.DeviceAllocationMode, quota int32) int32 {
	var allocated int32
	switch mode {
	default:
		allocated = nodefeature.ResourceMaxUnits
	case workercore.DeviceAllocationModeShared:
		allocated = nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize
	case workercore.DeviceAllocationModeSliced, workercore.DeviceAllocationModePartitioned:
		allocated = quota
	}
	return allocated
}
