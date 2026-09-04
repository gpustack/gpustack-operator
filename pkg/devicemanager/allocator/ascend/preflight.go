package ascend

import (
	"errors"
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// shareCapability names the container-share flag in a preflight row, in dcmi's own vocabulary.
const shareCapability = "container-share"

const (
	shareDetailEnabled  = "container-share is enabled"
	shareDetailDisabled = "container-share is disabled, and this driver accepted being asked to " +
		"turn it on, so the allocator will manage to when a second container lands on this " +
		"accelerator; it was put back the way it was found"
	shareDetailNotAsked = "container-share is disabled and was left that way: asking it on to see whether " +
		"this driver accepts is a write, and this is a dry run"

	shareDetailNotRestored = "container-share is disabled, and this driver accepted being asked to " +
		"turn it on -- but putting it back failed, so it is ON now and this accelerator will admit " +
		"a second container until it is turned off"
)

// NewPreflighter returns the container-share preflighter, which reads the one driver flag every
// Ascend allocation that puts a second container on an accelerator depends on.
//
// It drives the same seam the allocator's servers do, so what it reports is what an allocation
// would find, taken with no workload on the node.
func NewPreflighter(opts device.PreflighterOptions) device.AcceleratorPreflighter {
	logger := opts.Logger.WithName(Manufacturer)
	return &preflighter{
		logger: logger,
		share:  newShareDriver(logger),
		topo:   newHcclTopoResolver(newTopoDriver(logger)),
		dryRun: opts.DryRun,
	}
}

type preflighter struct {
	logger klog.Logger
	share  shareDriver
	// topo is the real resolver, not a recording stand-in: naming the topology file is a read, so a
	// simulated allocation can take it as it is without writing anything to the host.
	topo   *hcclTopoResolver
	dryRun bool
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

// PreflightResponder returns the Ascend responder for mode, built over a container-share driver
// that records what it was asked to write instead of writing it.
//
// The responder is the allocator's own, assembled by the same newServer an allocation is served by,
// so the injection it produces is the injection an allocation produces. The only difference is the
// driver underneath — and that driver still answers reads from the real one, so the responder takes
// the branch the hardware would actually send it down rather than a branch a fixture chose.
//
// Swapping the driver is not the whole of it: the sliced path renders its quota config to a host
// path as well, so the redirect is set up here and handed back for the caller to defer. Ascend
// carries no host path beyond the two every manufacturer shares, so the shared redirect is returned
// unwrapped.
func (p *preflighter) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect()
	if err != nil {
		return nil, nil, err
	}

	srv := newServer(p.logger, mode, &recordingShareDriver{read: p.share}, p.topo)

	responder, ok := srv.(deviceplugin.ContainerAllocateResponder)
	if !ok {
		restore()
		return nil, nil, fmt.Errorf("ascend %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}

// recordingShareDriver answers reads from the driver underneath and records writes rather than
// making them.
//
// Reading through is what keeps the simulation honest: a responder over a driver that invented its
// answers would take whichever branch the invention chose, not the one this host would send it
// down. The write is reported as having succeeded because that is the branch an allocation would
// continue on; whether it truly would is exactly the thing only the measured depth can establish.
type recordingShareDriver struct {
	read shareDriver
	// writes names each (card, device) pair a responder asked to enable, in call order. It is what
	// a caller asserts on to establish that a pass left the device as it found it.
	writes [][2]int32
}

func (d *recordingShareDriver) GetShareEnabled(cardID, deviceID int32) (bool, error) {
	return d.read.GetShareEnabled(cardID, deviceID)
}

func (d *recordingShareDriver) SetShareEnabled(cardID, deviceID int32, _ bool) error {
	d.writes = append(d.writes, [2]int32{cardID, deviceID})
	return nil
}

// check establishes one accelerator's container-share flag: read it, and where it is off, ask the
// driver to turn it on and put it straight back.
//
// Reading alone cannot answer the question. A flag that is off is not a node that cannot serve --
// the allocator turns it on itself when a second container lands -- so a read that stops there
// reports "off" and leaves the operator no wiser about whether the allocation would have worked.
// Asking the driver is what turns that into an answer, and it is exactly the call the allocator
// would make.
//
// The toggle is safe precisely because it only happens where the read established the flag was off:
// nothing on the node is sharing this accelerator, so nothing can notice the window between turning
// it on and putting it back. Where it is already on, nothing is touched.
//
// A read that failed is not written past, and this is where a preflight parts company with the
// allocator. The allocator writes past a bad read because it wants the flag ON and leaves it there,
// so the worst case is a flag that was already on. This command has to put back what it found, and
// its restore is unconditionally OFF -- so writing past a failed read would end with a transient
// dcmi error having turned off a flag that was serving a live container. Reporting the accelerator
// unreadable is the lesser error, and the reason carries the driver's own message.
//
// A restore that fails is the one host mutation this command could not undo, so it is a failure of
// the row rather than a note on it: the detail says the accelerator was left on, and the state makes
// the command exit non-zero, because the automation that runs this reads the exit code.
func (p *preflighter) check(accel *workercore.Accelerator) device.PreflightCheck {
	c := device.PreflightCheck{Accelerator: accel.ID, Capability: shareCapability, Mode: device.PreflightModeOf(workercore.DeviceAllocationModeSliced)}

	// The Ascend detector records dcmi's own addressing in PhysicalIndexes as
	// {physical id, card id, device id in card}.
	if len(accel.PhysicalIndexes) < 3 {
		c.State = device.PreflightStateUnavailable
		c.Reason = "the accelerator carries no dcmi card/device index"
		return c
	}
	cardID, deviceID := int32(accel.PhysicalIndexes[1]), int32(accel.PhysicalIndexes[2])

	enabled, readErr := p.share.GetShareEnabled(cardID, deviceID)
	c.State, c.Detail, c.Reason = classifyShareCall(enabled, readErr)

	switch {
	case c.State == device.PreflightStateNotDeclared:
		// There is no flag on this generation, so there is nothing a write could add.
		return c
	case errors.Is(readErr, errShareUnsupported):
		// A driver this code cannot query is one it cannot manage, and the allocator refuses on
		// it without touching the device. Writing here would touch it.
		return c
	case readErr != nil:
		// Any other failed read leaves the flag's own state unknown, and the restore below is
		// unconditionally off. Writing here could turn off a flag that was already on.
		return c
	case enabled:
		return c
	case p.dryRun:
		// Asking the flag on is a write, however briefly it is held, and a restore that fails leaves
		// the card enabled. A dry run promises the host nothing of the sort, so the row reports what
		// was read and says plainly that the rest was not established.
		c.State, c.Detail, c.Reason = device.PreflightStateOK, shareDetailNotAsked, ""
		return c
	}

	if err := p.share.SetShareEnabled(cardID, deviceID, true); err != nil {
		c.State, c.Detail, c.Reason = classifyShareCall(false, err)
		return c
	}

	// The panic path, and only it. Every ordinary outcome below sets its own row and marks this
	// done; what is left over is a vendor library that crashed with the flag in a state this process
	// never chose. Panic containment is one of this command's own features, and what it does is
	// report the panic -- it cannot undo what the panicking code already did, and a card left
	// sharing because a driver crashed is not something to report and walk away from. The read above
	// established the flag was off, so off is where it goes back to.
	done := false
	defer func() {
		if done {
			return
		}
		if err := p.share.SetShareEnabled(cardID, deviceID, false); err != nil {
			p.logger.Error(err, "could not put container-share back after a panic",
				"accelerator", accel.ID, "card", cardID, "device", deviceID)
		}
	}()

	if err := p.share.SetShareEnabled(cardID, deviceID, false); err != nil {
		done = true
		p.logger.Error(err, "could not put container-share back",
			"accelerator", accel.ID, "card", cardID, "device", deviceID)
		c.State, c.Detail, c.Reason = device.PreflightStateUnavailable, shareDetailNotRestored, err.Error()
		return c
	}
	done = true
	p.logger.Info("container-share accepted being turned on, and was put back",
		"accelerator", accel.ID, "card", cardID, "device", deviceID)
	c.State, c.Detail, c.Reason = device.PreflightStateOK, shareDetailDisabled, ""
	return c
}

// classifyShareCall maps one container-share driver call's outcome onto the preflight state it
// means. It mirrors the three-way classification ensureShareEnabled performs on the same
// outcomes, so a preflight row and the allocation it predicts cannot disagree:
//
//   - a generation that declares no such flag is not-declared, and an allocation proceeds on it;
//   - any other failure is unavailable, and an allocation is refused on it;
//   - a call that answered is ok, and what it answered is the detail — a flag that is off is still
//     ok, because the allocator turns it on itself.
//
// The order of the first two is load-bearing: errShareNotDeclared marks a failure, so testing for
// a failure first would report every one of them as unreadable.
func classifyShareCall(enabled bool, err error) (state device.PreflightState, detail, reason string) {
	switch {
	case errors.Is(err, errShareNotDeclared):
		return device.PreflightStateNotDeclared, "", err.Error()
	case err != nil:
		return device.PreflightStateUnavailable, "", err.Error()
	case enabled:
		return device.PreflightStateOK, shareDetailEnabled, ""
	default:
		return device.PreflightStateOK, shareDetailDisabled, ""
	}
}
