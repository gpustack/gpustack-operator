package thead

import (
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// migCapability names hardware partitioning in a preflight row, in the vendor library's own
// vocabulary.
const migCapability = "mig-partitioning"

// migNotDeclaredReason explains a not-declared mig-partitioning row. AcceleratorPhysicalSliced's
// own doc comment is the source of truth this mirrors: "Profiles is empty when the accelerator
// does not support, or has not enabled, hard slicing." The driver having been asked as well is
// what earns the claim: see check for why an empty inventory alone does not.
const migNotDeclaredReason = "the accelerator declares no physical-slice profile and its driver " +
	"reports no live gpu instance on it, so partitioning is unavailable or off on it"

// migInventoryShortFormat explains an unavailable mig-partitioning row on an accelerator whose
// detect-time inventory contradicts its driver: nothing is declared, yet partitions are live on it.
const migInventoryShortFormat = "the accelerator declares no physical-slice profile, yet the vendor " +
	"library reports %d live gpu instance(s) on it: the profile inventory taken at detection is " +
	"short, so every partitioned allocation is refused on an accelerator that is in fact partitioned"

// NewPreflighter returns the partition preflighter, which enumerates one accelerator's live GPU
// instances — the same vendor subtree every partitioned allocation reads before it decides whether
// to adopt a leftover or create a fresh one.
//
// It drives the same seam the allocator's own reservation core does, so what it reports is what an
// allocation would find, taken with no workload on the node.
//
// Nothing here is toggled and put back: this manufacturer's only allocation-time write is
// ActuatePhysicalSliced creating a hardware partition, and that method is never reached through this
// seam (see PreflightResponder). There is consequently nothing here a toggle could
// safely establish — inventing one for symmetry would either be a no-op dressed up as one, or would
// carve a partition a preflight has no business creating.
func NewPreflighter(opts device.PreflighterOptions) device.AcceleratorPreflighter {
	logger := opts.Logger.WithName(Manufacturer)
	return &preflighter{logger: logger, mig: newMigDriver()}
}

type preflighter struct {
	logger klog.Logger
	mig    migDriver
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

// PreflightResponder returns the T-Head responder for mode, built over a partition driver that
// records what it was asked to create or destroy instead of doing either.
//
// The responder is the allocator's own, assembled by the same newServer an allocation is served by,
// so the injection it produces is the injection an allocation produces. Only GetContainerAllocateResponse
// is reachable through the returned value — ActuatePhysicalSliced, the method that actually carves a
// hardware partition, is a distinct interface (PhysicalSlicedResponder) this seam never returns and
// never calls. GetContainerAllocateResponse itself never reads the partition driver at all except in
// the ".sliced" family, which enforces its quota entirely inside the container and touches the driver
// not once, so the recording driver below is never exercised through this seam today — it is kept as
// the same defense-in-depth the other manufacturers carry, so a later change to what
// GetContainerAllocateResponse reads cannot silently start writing through this path.
//
// The ".sliced" family does write to the host through deviceplugin.OperatorPodsDir (the per-container
// usage-region directory), so a caller simulating that mode must still bracket the call with
// deviceplugin.RedirectHostWrites.
func (p *preflighter) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect()
	if err != nil {
		return nil, nil, err
	}

	srv := newServer(p.logger, mode, &recordingMigDriver{read: p.mig})

	responder, ok := srv.(deviceplugin.ContainerAllocateResponder)
	if !ok {
		restore()
		return nil, nil, fmt.Errorf("thead %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}

// recordedCreate is one CreateInstance call a recordingMigDriver refused to make, kept in full so a
// caller can assert exactly what a responder tried to carve.
type recordedCreate struct {
	cardUUID                    string
	profile                     string
	computeSlices, memorySlices int32
	slot                        migPlacement
}

// recordingMigDriver answers reads from the driver underneath and records create/destroy attempts
// rather than making them.
//
// Reading through is what keeps the simulation honest: a responder over a driver that invented its
// answers would take whichever branch the invention chose, not the one this host would send it down.
// A create is refused with an error rather than fabricated success, because a caller that went on to
// treat a fabricated instance as real would be acting on hardware that does not exist; a destroy is
// accepted silently, mirroring DestroyInstance's own idempotent "already gone" contract, since nothing
// this seam reaches ever destroys a real instance to report an outcome for.
type recordingMigDriver struct {
	read migDriver

	creates  []recordedCreate
	destroys []migInstance
}

var _ migDriver = (*recordingMigDriver)(nil)

func (d *recordingMigDriver) CardState(cardUUID, profile string, computeSlices, memorySlices int32) (migCardState, error) {
	return d.read.CardState(cardUUID, profile, computeSlices, memorySlices)
}

func (d *recordingMigDriver) CreateInstance(
	cardUUID, profile string, computeSlices, memorySlices int32, slot migPlacement,
) (migInstance, error) {
	d.creates = append(d.creates, recordedCreate{
		cardUUID: cardUUID, profile: profile,
		computeSlices: computeSlices, memorySlices: memorySlices, slot: slot,
	})
	return migInstance{}, fmt.Errorf("thead preflight: refusing to create a partition through the simulated responder")
}

func (d *recordingMigDriver) DestroyInstance(_ string, inst migInstance) error {
	d.destroys = append(d.destroys, inst)
	return nil
}

func (d *recordingMigDriver) ListInstances() ([]migLiveInstance, error) {
	return d.read.ListInstances()
}

func (d *recordingMigDriver) InstanceProcesses(cardUUID string, inst migInstance) (int, error) {
	return d.read.InstanceProcesses(cardUUID, inst)
}

func (d *recordingMigDriver) CardInstances(cardUUID string) ([]migInstance, error) {
	return d.read.CardInstances(cardUUID)
}

// check reads one accelerator's live partition subtree — the precondition every partitioned
// allocation's reservation core reads before deciding whether to adopt a leftover instance or carve a
// fresh one.
//
// The driver is asked even about an accelerator that declares nothing, and that is the whole reason
// the read is not skipped there. An empty profile inventory is not the single fact it looks like:
// detectMigProfiles (detector/thead/mig_profile.go) publishes an empty list when the accelerator
// genuinely offers no profile AND when the probe or the derivation failed on every one it does
// offer — those failures are logged and the inventory is published short. Taking the empty list at
// face value would report an accelerator whose catalog was unreadable at detection as one without
// the capability, and exit 0 on it.
//
// Asking the driver separates them, because CardInstances reads the partition-mode flag itself
// (cardInstances in mig_driver_linux.go) before it walks anything: a flag it cannot read is an
// error here, where detection published an empty inventory and moved on.
func (p *preflighter) check(accel *workercore.Accelerator) device.PreflightCheck {
	c := device.PreflightCheck{
		Accelerator: accel.ID,
		Capability:  migCapability,
		Mode:        device.PreflightModeOf(workercore.DeviceAllocationModePartitioned),
	}

	if accel.ID == "" {
		c.State = device.PreflightStateUnavailable
		c.Reason = "the accelerator reports no unique id, so no vendor handle addresses it"
		return c
	}

	declared := len(accel.Status.PhysicalSliced.Profiles) > 0

	instances, err := p.mig.CardInstances(accel.ID)
	c.State, c.Detail, c.Reason = classifyCardInstancesCall(declared, instances, err)
	return c
}

// classifyCardInstancesCall maps one CardInstances call's outcome onto the preflight state it means,
// mirroring how reserveMigInstance itself treats the same read: CardState (and so CardInstances)
// failing anywhere is propagated straight out as a failure that refuses the allocation, never as an
// empty accelerator, so any error here is unavailable and never not-declared.
//
// There is no sentinel this call can carry that would mean "no such capability" — that verdict comes
// from whether the accelerator declares a physical-slice profile at all, which is what declared
// carries. A driver that CAN be asked and reports zero live instances is exactly what a declared
// accelerator with partitioning turned off looks like at this call, and that is still ok: proving the
// subtree readable and empty is the very read an allocation's adoption search depends on, so it must
// not be confused with a capability that does not exist.
//
// A failed read is classified before an empty inventory, and the order is load-bearing: those are the
// two ways the same accelerator can arrive here when detection could not read its catalog, and
// testing the inventory first would report every one of them as a capability the hardware does not
// have.
//
// Between them sits the contradiction neither source can express alone: nothing declared, yet the
// driver reports live partitions. The allocator would refuse a physical-slice allocation here on a
// record the hardware itself disproves, so the row is unavailable rather than not-declared — what is
// wrong is the node's inventory, and reporting the accelerator as incapable would exit 0 on exactly
// the accelerator that needs re-detecting.
func classifyCardInstancesCall(
	declared bool, instances []migInstance, err error,
) (state device.PreflightState, detail, reason string) {
	switch {
	case err != nil:
		return device.PreflightStateUnavailable, "", err.Error()
	case !declared && len(instances) > 0:
		return device.PreflightStateUnavailable, "", fmt.Sprintf(migInventoryShortFormat, len(instances))
	case !declared:
		return device.PreflightStateNotDeclared, "", migNotDeclaredReason
	}
	return device.PreflightStateOK,
		fmt.Sprintf("the partition subtree is readable and carries %d live gpu instance(s)", len(instances)), ""
}
