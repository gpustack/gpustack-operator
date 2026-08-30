package nvidia

import (
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// migCapability names hardware partitioning in a preflight row, in NVML's own vocabulary.
const migCapability = "mig-partitioning"

// migNotDeclaredReason explains a not-declared mig-partitioning row. AcceleratorPhysicalSliced's
// own doc comment is the source of truth this mirrors: "Profiles is empty when the accelerator
// does not support, or has not enabled, hard slicing." The driver having been asked as well is
// what earns the claim: see check for why an empty inventory alone does not.
const migNotDeclaredReason = "the accelerator declares no physical-slice profile and its driver " +
	"reports no live gpu instance on it, so mig is unavailable or off on it"

// migInventoryShortFormat explains an unavailable mig-partitioning row on an accelerator whose
// detect-time inventory contradicts its driver: nothing is declared, yet partitions are live on it.
const migInventoryShortFormat = "the accelerator declares no physical-slice profile, yet nvml " +
	"reports %d live gpu instance(s) on it: the profile inventory taken at detection is short, so " +
	"every partitioned allocation is refused on an accelerator that is in fact partitioned"

// NewPreflighter returns the MIG preflighter, which enumerates one accelerator's live GPU
// instances — the same NVML subtree every partitioned allocation walks before it creates one — and
// hands back the plain (non-partitioned) responder the allocator itself serves.
//
// Nothing here is toggled and put back the way Ascend's and Cambricon's modes are: see
// PreflightResponder for why there is no such write to mirror.
func NewPreflighter(opts device.PreflighterOptions) device.AcceleratorPreflighter {
	logger := opts.Logger.WithName(Manufacturer)

	// Mirror New()'s own resolver construction, so PreflightResponder's non-sliced/non-partitioned
	// responses take the same channel (env var or CDI) a real allocation on this host would.
	injection, err := deviceplugin.NewInjectionResolver(injectionConfig)
	if err != nil {
		logger.Error(err, "keeping the default device-injection strategy")
		injection = deviceplugin.DefaultInjectionResolver(injectionConfig)
	}

	return &preflighter{
		logger:    logger,
		mig:       newMigDriver(),
		injection: injection,
	}
}

type preflighter struct {
	logger    klog.Logger
	mig       migDriver
	injection *deviceplugin.InjectionResolver
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

// PreflightResponder returns the NVIDIA responder for mode, built by the package's own newServer —
// the same construction New() uses — so the injection it produces is the injection an allocation
// produces.
//
// mig is passed nil rather than swapped for a recording stand-in. GetContainerAllocateResponse,
// the one method this seam drives, never reads the server's mig field for any mode: a MIG driver
// only feeds ActuatePhysicalSliced and GetPhysicalSlicedVisibilityResponse, both declared on
// PhysicalSlicedResponder — a capability this seam does not expose and never calls (see
// AcceleratorInjectionPreflighter's own doc comment for why). There is consequently nothing to
// record, and passing nil rather than a real driver means a future change that made
// GetContainerAllocateResponse consult it would fail here rather than quietly reach the device
// through a pass that reports itself as having changed nothing.
//
// This manufacturer is the reason the contract hands a restore back. Its sliced responder creates
// the per-container work directory — under the two paths every manufacturer shares — and, on top of
// that, the HAMi-core cross-process lock directory, which is this package's own variable and
// world-writable. Redirecting only the shared pair would leave a pass that calls itself read-only
// creating /tmp/vgpulock on the node it was inspecting, so it is handed to the redirect as a private
// path — moved and restored alongside the shared pair, and reported through
// deviceplugin.PreflightRehosts so an emitted command names the lock directory a real allocation
// would rather than a scratch one that no longer exists.
func (p *preflighter) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect(&hostVgpuLockPath)
	if err != nil {
		return nil, nil, err
	}

	srv := newServer(p.logger, mode, nil, p.injection)

	responder, ok := srv.(deviceplugin.ContainerAllocateResponder)
	if !ok {
		restore()
		return nil, nil, fmt.Errorf("nvidia %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}

// check reads one accelerator's live MIG-instance subtree, read-only.
//
// There is no write to attempt: a MIG partition is materialized for a workload and belongs to it
// (ActuatePhysicalSliced in mig.go), so creating one outside an allocation would leave the node
// carrying a partition no Pod owns — unlike Ascend's container-share flag, which is a property of
// the accelerator itself and safe to turn on ahead of any workload.
//
// The driver is asked even about an accelerator that declares nothing, and that is the whole reason
// the read is not skipped there. An empty profile inventory is not the single fact it looks like:
// detectMigProfiles (detector/nvidia/mig_profile.go) publishes an empty list when the accelerator
// genuinely offers no profile AND when the probe, the placement query or the derivation failed on
// every one it does offer — those failures are logged and the inventory is published short. Taking
// the empty list at face value would report an accelerator whose catalog was unreadable at
// detection as one without the capability, and exit 0 on it.
//
// Asking the driver separates them, because it is stricter than detection was: cardProfiles
// (mig_driver_linux.go) fails the whole probe on an id it could not read, where detection skipped
// it. So a catalog this command cannot read fails here rather than passing as "no such
// capability", and an accelerator carrying live partitions it does not declare is caught by the
// contradiction between the two.
func (p *preflighter) check(accel *workercore.Accelerator) device.PreflightCheck {
	c := device.PreflightCheck{
		Accelerator: accel.ID,
		Capability:  migCapability,
		Mode:        device.PreflightModeOf(workercore.DeviceAllocationModePartitioned),
	}

	if accel.ID == "" {
		c.State = device.PreflightStateUnavailable
		c.Reason = "the accelerator reports no unique id, so no nvml handle addresses it"
		return c
	}

	// profileGeometry (mig.go) reads this same field as the allocator's own precondition for
	// whether an accelerator offers physical slicing at all, and refuses to actuate without
	// consulting the driver when it is empty. A live NVML read cannot supply this verdict on its
	// own either: CardInstances answers an empty, error-free list both when MIG is off (or
	// unsupported — the "[N/A]" a consumer card reports) and when MIG is on but the accelerator
	// currently holds no partition. Neither source settles it alone, so the row is decided from
	// both.
	declared := len(accel.Status.PhysicalSliced.Profiles) > 0

	instances, err := p.mig.CardInstances(accel.ID)
	c.State, c.Detail, c.Reason = classifyMigOutcome(declared, len(instances), err)
	return c
}

// classifyMigOutcome maps one accelerator's MIG outcome onto the preflight state it means. It
// mirrors the two preconditions the allocator itself reads on the same terms before it actuates a
// partition:
//
//   - reserveMigInstance (mig.go) fails the allocation when CardState/CardInstances itself errors —
//     unavailable;
//   - profileGeometry (mig.go) refuses without ever touching the driver when the accelerator's
//     detect-time capability record (AcceleratorPhysicalSliced.Profiles) is empty — not-declared,
//     and an allocation of a physical-slice profile is refused on it, but every other allocation
//     mode proceeds;
//   - a driver that answered, on a declared accelerator, is ok — what it answered (how many live
//     GPU instances) is the detail, never a failure: an accelerator with no live partition today is
//     exactly the case a create fills.
//
// A failed read is classified before an empty inventory, and the order is load-bearing: those are
// the two ways the same accelerator can arrive here when detection could not read its catalog,
// and testing the inventory first would report every one of them as a capability the hardware does
// not have.
//
// Between them sits the contradiction neither precondition can express: nothing declared, yet the
// driver reports live partitions. The allocator would refuse a physical-slice allocation here on a
// record that the hardware itself disproves, so the row is unavailable rather than not-declared —
// what is wrong is the node's inventory, and reporting the accelerator as incapable would exit 0 on
// exactly the accelerator that needs re-detecting.
func classifyMigOutcome(declared bool, liveInstances int, err error) (state device.PreflightState, detail, reason string) {
	switch {
	case err != nil:
		return device.PreflightStateUnavailable, "", err.Error()
	case !declared && liveInstances > 0:
		return device.PreflightStateUnavailable, "", fmt.Sprintf(migInventoryShortFormat, liveInstances)
	case !declared:
		return device.PreflightStateNotDeclared, "", migNotDeclaredReason
	default:
		return device.PreflightStateOK,
			fmt.Sprintf("the mig subtree is readable and carries %d live gpu instance(s)", liveInstances), ""
	}
}
