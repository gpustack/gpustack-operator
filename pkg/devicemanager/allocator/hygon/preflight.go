package hygon

import (
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// noDriverNote says, in this manufacturer's own terms, why PreflightAccelerator returns no checks.
// The Hygon server (deviceplugin.go) reads no driver for its whole-card modes -- their device set is
// the fixed control-node pair plus a card's own drm nodes, both named from paths this package
// declares. The sliced path (vdev.go, getSlicedContainerAllocateResponse) derives its CU-mask and
// VRAM cap purely from allocateVdev's own on-disk vdev.conf ledger: it scans the pods directory it
// also writes to, never a DCU driver or dcmi call.
//
// The partition path DOES read a driver, and is deliberately not checked here. It only exists on a
// node whose Multi-Instance mode an administrator turned on out of band, and on such a node the
// detector already publishes no partition profile at all when the library cannot be reached -- so a
// card that would fail this check advertises no partition capacity to fail an allocation with. The
// simulated responder below still exercises the whole reservation path against the real driver's
// reads, which is where an unreachable library shows up as the error it is.
const noDriverNote = "the hygon allocator reads no driver to serve a whole-card, shared or sliced " +
	"allocation: those device sets are host device-node paths declared in this package, and the " +
	"sliced path derives its cu-mask and vram cap from the on-disk vdev.conf ledger it scans and " +
	"writes itself. only the partition path reads the multi-instance library, and only on a node " +
	"an administrator has put into multi-instance mode"

// NewPreflighter returns the Hygon preflighter. It carries no driver seam because the allocator
// reads none: PreflightAccelerator reports that in words via Note, and PreflightResponder is the
// serviceable half, handing back the allocator's own responder for the simulated and measured
// depths.
func NewPreflighter(opts device.PreflighterOptions) device.AcceleratorPreflighter {
	return &preflighter{logger: opts.Logger.WithName(Manufacturer)}
}

type preflighter struct {
	logger klog.Logger
}

// PreflightAccelerator returns a group carrying no checks and noDriverNote. See NewPreflighter.
func (p *preflighter) PreflightAccelerator(_ device.DevicesGroupList) device.PreflightGroup {
	return device.PreflightGroup{
		Manufacturer: Manufacturer,
		Timestamp:    time.Now(),
		Note:         noDriverNote,
	}
}

// PreflightResponder returns the Hygon responder for mode, built by the same newServer an
// allocation is served by.
//
// The whole-card and sliced paths reach the host only through the two paths every manufacturer
// shares -- PodWorkDir under deviceplugin.OperatorPodsDir, which the sliced path's vdev.conf ledger
// is written under -- so the shared redirect covers them unwrapped.
//
// The partition path is given a driver that reads through to the real one and refuses to carve, so a
// simulation takes the branch this host would actually take without leaving an instance behind.
func (p *preflighter) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect()
	if err != nil {
		return nil, nil, err
	}

	srv := newServer(p.logger, mode, &recordingMigDriver{read: newMigDriver()})

	responder, ok := srv.(deviceplugin.ContainerAllocateResponder)
	if !ok {
		restore()
		return nil, nil, fmt.Errorf("hygon %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}

// recordedMigCreate is one CreateInstance call a recordingMigDriver refused to make, kept in full so
// a caller can assert exactly what a responder tried to carve.
type recordedMigCreate struct {
	pciBusID string
	profile  string
	slot     migPlacement
}

// recordingMigDriver answers reads from the driver underneath and records create attempts rather
// than making them.
//
// Reading through is what keeps the simulation honest: a responder over a driver that invented its
// answers would take whichever branch the invention chose, not the one this host would send it down.
// A create is refused with an error rather than fabricated success, because a caller that went on to
// treat a fabricated instance as real would be acting on hardware that does not exist; a destroy is
// accepted silently, mirroring DestroyInstance's own idempotent "already gone" contract, since
// nothing this seam reaches ever destroys a real instance to report an outcome for.
type recordingMigDriver struct {
	read migDriver

	creates  []recordedMigCreate
	destroys []migInstance
}

var _ migDriver = (*recordingMigDriver)(nil)

func (d *recordingMigDriver) CardState(pciBusID, profile string) (migCardState, error) {
	return d.read.CardState(pciBusID, profile)
}

func (d *recordingMigDriver) CreateInstance(
	pciBusID, profile string, slot migPlacement,
) (migInstance, error) {
	d.creates = append(d.creates, recordedMigCreate{pciBusID: pciBusID, profile: profile, slot: slot})
	return migInstance{}, fmt.Errorf(
		"hygon preflight: refusing to create a partition through the simulated responder")
}

func (d *recordingMigDriver) DestroyInstance(_ string, inst migInstance) error {
	d.destroys = append(d.destroys, inst)
	return nil
}

func (d *recordingMigDriver) ListInstances() ([]migLiveInstance, error) {
	return d.read.ListInstances()
}
