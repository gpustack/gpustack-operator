package metax

import (
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// sgpuCapability names the sgpu mode a preflight row reports, in the sysfs seam's own vocabulary.
const sgpuCapability = "sgpu-mode"

const (
	sgpuDetailEnabled  = "the accelerator already hosts an sgpu subdevice and is in sgpu mode"
	sgpuDetailDisabled = "the accelerator is not yet in sgpu mode; the allocator puts it there when " +
		"the first slice lands"
)

// NewPreflighter returns the sgpu-mode preflighter, which reads the sgpu subdevice registry every
// MetaX logical slice depends on before any subdevice can be created on the accelerator.
//
// It drives the same seam the allocator's sliced responder does, so what it reports is what an
// allocation would find, taken with no workload on the node.
func NewPreflighter(opts device.PreflighterOptions) device.AcceleratorPreflighter {
	logger := opts.Logger.WithName(Manufacturer)
	return &preflighter{
		logger: logger,
		sgpu:   newSysfsSGPUManager(),
	}
}

type preflighter struct {
	logger klog.Logger
	sgpu   sgpuManager
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

// PreflightResponder returns the MetaX responder for mode, built by the allocator's own newServer,
// with the sliced responder's sgpu manager swapped for one that records what it was asked to write
// or create instead of doing it.
//
// Only the sliced mode drives an sgpu manager at all -- newServer wires one in only for
// workercore.DeviceAllocationModeSliced -- so the other modes come back from newServer entirely
// unchanged; there is nothing on their path for a substitution to touch.
//
// This does not make the whole pass read-only on its own: the sliced path also writes a
// correlation + slot marker under the pod work dir, which the caller redirects with
// deviceplugin.NewPreflightRedirect. MetaX carries no host path beyond the two every manufacturer
// shares (its marker lives under deviceplugin.OperatorPodsDir; it never touches OperatorLibDir), so
// the shared redirect is returned unwrapped.
func (p *preflighter) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect()
	if err != nil {
		return nil, nil, err
	}

	srv := newServer(p.logger, mode)
	if s, ok := srv.(*server); ok && s.sgpu != nil {
		s.sgpu = &recordingSGPUManager{read: p.sgpu}
	}

	responder, ok := srv.(deviceplugin.ContainerAllocateResponder)
	if !ok {
		restore()
		return nil, nil, fmt.Errorf("metax %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}

// schedClassRequest is one (bdf, class) a responder asked recordingSGPUManager to set the QoS
// scheduling class to.
type schedClassRequest struct {
	bdf   string
	class schedClass
}

// createRequest is one subdevice a responder asked recordingSGPUManager to create.
type createRequest struct {
	bdf     string
	index   int
	vramMiB int64
	alias   string
}

// recordingSGPUManager answers reads from the manager underneath and records writes rather than
// making them.
//
// MetaX is the one manufacturer, besides Cambricon, where a naive simulated pass would provision
// real hardware state: reserveSlice does not stop at a boolean mode flag -- on a card with no
// existing subdevice it puts the accelerator into sgpu mode, sets its QoS class, and creates a
// subdevice, a resource that outlives the container and has to be torn down again. Every write
// reserveSlice can make -- EnsureModel, SetSchedClass, Create, and the Remove it issues on its own
// rollback path -- is therefore recorded here instead of reaching the driver. Reading through
// (List) is what keeps the simulation honest: a responder over a manager that invented its answers
// would take whichever branch the invention chose, not the one this host would send it down.
type recordingSGPUManager struct {
	read sgpuManager

	// ensureModelWrites names each bdf a responder asked to put into sgpu mode, in call order.
	ensureModelWrites []string
	// setSchedClassWrites names each (bdf, class) a responder asked to set, in call order.
	setSchedClassWrites []schedClassRequest
	// creates names each subdevice a responder asked to create, in call order.
	creates []createRequest
	// removes names each (bdf, index) a responder asked to destroy, in call order.
	removes []subdevKey
	// listCalls counts how many times the real (fake) driver's registry was actually read, so a
	// caller can assert the simulation took the branch the hardware would have sent it down rather
	// than an invented one.
	listCalls int
}

func (d *recordingSGPUManager) EnsureModel(bdf string) error {
	d.ensureModelWrites = append(d.ensureModelWrites, bdf)
	return nil
}

func (d *recordingSGPUManager) SetSchedClass(bdf string, c schedClass) error {
	d.setSchedClassWrites = append(d.setSchedClassWrites, schedClassRequest{bdf: bdf, class: c})
	return nil
}

func (d *recordingSGPUManager) Create(bdf string, index int, vramMiB int64, alias string) error {
	d.creates = append(d.creates, createRequest{bdf: bdf, index: index, vramMiB: vramMiB, alias: alias})
	return nil
}

func (d *recordingSGPUManager) Remove(bdf string, index int) error {
	d.removes = append(d.removes, subdevKey{bdf: bdf, index: index})
	return nil
}

func (d *recordingSGPUManager) List() ([]sgpuSubdevice, error) {
	d.listCalls++
	return d.read.List()
}

// check reads one accelerator's sgpu registry and reports what it says. It writes nothing.
//
// Unlike the two manufacturers whose preflight asks a driver to toggle a mode and puts it straight
// back, there is nothing here that can be put back. Reaching sgpu mode means EnsureModel plus
// SetSchedClass -- writes to the device's own sysfs that create a subdevice and pick a scheduler
// for it, which is a resource that outlives the call and has to be torn down again, not a flag with
// two positions. A read command does not leave one of those behind on a node it was asked to
// inspect, so the answer stops at what the registry says and says why it goes no further.
func (p *preflighter) check(accel *workercore.Accelerator) device.PreflightCheck {
	c := device.PreflightCheck{Accelerator: accel.ID, Capability: sgpuCapability, Mode: device.PreflightModeOf(workercore.DeviceAllocationModeSliced)}

	// sysfsSGPUManager addresses a card by its PCI bus ID, which is what reserveSlice passes it
	// too.
	bdf := accel.Topology.PciBusID
	if bdf == "" {
		c.State = device.PreflightStateUnavailable
		c.Reason = "the accelerator reports no pci bus id, so sysfs cannot address it"
		return c
	}

	registry, err := p.sgpu.List()
	c.State, c.Detail, c.Reason = classifySGPUListCall(registry, bdf, err)
	return c
}

// classifySGPUListCall maps the sgpu registry read onto the preflight state it means. It mirrors
// reserveSlice's own classification of the same read:
//
//   - a registry read failure refuses the allocation identically for every accelerator --
//     reserveSlice wraps and returns the error verbatim, with no branch that proceeds without the
//     registry -- so it is unavailable here too;
//   - a read that succeeds is ok, and whether this accelerator's own bdf already carries a
//     subdevice is the detail: the same cardHasSubdevice test reserveSlice runs before deciding
//     whether to call EnsureModel/SetSchedClass at all.
//
// There is no not-declared state, and that is deliberate rather than an omission: unlike Ascend's
// container-share flag, MetaX's own detector declares every accelerator sgpu-capable
// unconditionally (LogicalSliced.Count is hardcoded to 16; see
// pkg/devicemanager/detector/metax/device.go), and sgpuManager carries no sentinel comparable to
// errShareNotDeclared or errSMLUUnsupported for a generation that lacks the capability. Reporting a
// three-way split here would invent a verdict reserveSlice itself never reaches -- the same reason
// AMD's classifyTopologyCall stays two-way.
func classifySGPUListCall(registry []sgpuSubdevice, bdf string, err error) (state device.PreflightState, detail, reason string) {
	if err != nil {
		return device.PreflightStateUnavailable, "", err.Error()
	}
	if cardHasSubdevice(registry, bdf) {
		return device.PreflightStateOK, sgpuDetailEnabled, ""
	}
	return device.PreflightStateOK, sgpuDetailDisabled, ""
}
