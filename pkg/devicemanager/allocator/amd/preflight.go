package amd

import (
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// cuMaskCapability names the HSA topology read in a preflight row: the CU mask is what it is read
// for, and an accelerator whose topology cannot be derived into one cannot be sliced.
const cuMaskCapability = "cu-mask-topology"

// NewPreflighter returns the CU-mask preflighter, which reads each accelerator's HSA topology
// through the same seam PlaceLogicalSliced does (readTopologyFn) and puts it through the same
// Topology.Validate every sliced allocation is refused or served on.
//
// Nothing here is a driver flag that could be toggled and put back, which is how Ascend and
// Cambricon establish theirs. A CU mask is HSA_CU_MASK, an environment variable handed to a
// container for the life of that one allocation; there is no persistent driver state upstream of it
// to prepare, so a toggle here would be inventing one the allocator does not have.
func NewPreflighter(opts device.PreflighterOptions) device.AcceleratorPreflighter {
	return &preflighter{
		logger:       opts.Logger.WithName(Manufacturer),
		readTopology: readTopologyFn,
	}
}

type preflighter struct {
	logger klog.Logger
	// readTopology is captured from readTopologyFn at construction, so a test can point one
	// preflighter at a stub topology without mutating the package-level seam every other test in
	// this package also uses.
	readTopology func(pciBusID, cardUUID string) (Topology, error)
}

func (p *preflighter) PreflightAccelerator(groups device.DevicesGroupList) device.PreflightGroup {
	grp := device.PreflightGroup{
		Manufacturer: Manufacturer,
		Timestamp:    time.Now(),
	}
	for i := range groups {
		accels := groups[i].Accelerators
		for j := range accels {
			grp.Checks = append(grp.Checks, p.check(&accels[j]))
		}
	}
	return grp
}

// PreflightResponder returns the AMD responder for mode, built by the same newServer an allocation
// is served by.
//
// Only GetContainerAllocateResponse is driven, per the AcceleratorInjectionPreflighter contract,
// and that method is what makes this preflighter's second half trivial: it calls neither
// readTopologyFn nor any host path under deviceplugin.OperatorLibDir / OperatorPodsDir — both live
// only behind PlaceLogicalSliced / GetLogicalSlicedResponse, which this seam does not reach. There
// is consequently no vendor driver seam here to swap for a recording stand-in, and no host write for
// a caller to redirect: the server newServer returns is already exactly what a simulated pass needs.
func (p *preflighter) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect()
	if err != nil {
		return nil, nil, err
	}

	srv := newServer(p.logger, mode)

	responder, ok := srv.(deviceplugin.ContainerAllocateResponder)
	if !ok {
		restore()
		return nil, nil, fmt.Errorf("amd %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}

// check reads one accelerator's HSA topology and validates it exactly as PlaceLogicalSliced does
// before WindowCUs derives a mask from it: read the topology, then Validate it. Neither step is
// attempted if the accelerator carries no ID, since PlaceLogicalSliced itself refuses such a record
// before ever calling readTopologyFn.
func (p *preflighter) check(accel *workercore.Accelerator) device.PreflightCheck {
	c := device.PreflightCheck{Accelerator: accel.ID, Capability: cuMaskCapability, Mode: device.PreflightModeOf(workercore.DeviceAllocationModeSliced)}

	if accel.ID == "" {
		c.State = device.PreflightStateUnavailable
		c.Reason = "the accelerator reports no unique id, so no hsa agent can be matched to it"
		return c
	}

	topo, err := p.readTopology(accel.Topology.PciBusID, accel.ID)
	c.State, c.Detail, c.Reason = classifyTopologyCall(topo, err)
	return c
}

// classifyTopologyCall maps one topology read onto the preflight state it means. It mirrors
// PlaceLogicalSliced's own two-way classification of the same read:
//
//   - a readTopologyFn error, or a topology that read but that Topology.Validate refuses, both
//     refuse the allocation identically -- PlaceLogicalSliced wraps and returns either verbatim,
//     with no branch that proceeds without a mask. So both are unavailable here too;
//   - a topology that reads and validates is ok, and what it validated to is the detail.
//
// There is no not-declared state, and that is deliberate rather than an omission: unlike Ascend's
// container-share flag, whose driver names a distinct "not supported on this generation" outcome
// the allocator proceeds past, nothing in PlaceLogicalSliced treats an unreadable or unvalidatable
// topology as "there is no such capability here, carry on without it." Every failure this call can
// produce -- HSA failing to initialize, no HSA agent naming this card, or an agent whose numbers
// Validate refuses -- takes the accelerator down the same refusal PlaceLogicalSliced does. Reporting
// a three-way split here would let a preflight row disagree with the allocation it predicts.
func classifyTopologyCall(topo Topology, err error) (state device.PreflightState, detail, reason string) {
	if err != nil {
		return device.PreflightStateUnavailable, "", err.Error()
	}
	if err := topo.Validate(); err != nil {
		return device.PreflightStateUnavailable, "", err.Error()
	}
	return device.PreflightStateOK,
		fmt.Sprintf("%s reports %d compute units in %d-cu allocation atoms", topo.Name, topo.CU, topo.Quantum()),
		""
}
