package cambricon

import (
	"errors"
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// smluCapability names the sMLU mode in a preflight row, in cnDev's own vocabulary.
const smluCapability = "smlu-mode"

const (
	smluDetailEnabled  = "sMLU mode is on"
	smluDetailDisabled = "sMLU mode is off, and this driver accepted being asked to turn it on, so " +
		"the allocator will manage to when a slice lands on this accelerator; it was put back the " +
		"way it was found"
	smluDetailNotAsked = "sMLU mode is off and was left that way: asking it on to see whether this " +
		"driver accepts is a write, and this is a dry run"

	smluDetailNotRestored = "sMLU mode is off, and this driver accepted being asked to turn it on " +
		"-- but putting it back failed, so the card is in sMLU mode now and stays there until it " +
		"is turned off"
)

// NewPreflighter returns the sMLU-mode preflighter, which reads the card mode every Cambricon
// logical slice depends on before any profile or instance can exist on the accelerator.
//
// It drives the same seam the allocator's sliced responder does, so what it reports is what an
// allocation would find, taken with no workload on the node.
func NewPreflighter(opts device.PreflighterOptions) device.AcceleratorPreflighter {
	logger := opts.Logger.WithName(Manufacturer)
	return &preflighter{
		logger: logger,
		smlu:   newSMLUDriver(),
		dryRun: opts.DryRun,
	}
}

type preflighter struct {
	logger klog.Logger
	smlu   smluDriver
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

// PreflightResponder returns the Cambricon responder for mode, built by the allocator's own
// newServer, with the sliced responder's sMLU driver swapped for one that records what it was
// asked to write instead of writing it.
//
// Only the sliced mode drives an sMLU driver at all -- newServer wires one in only for
// workercore.DeviceAllocationModeSliced -- so the other modes come back from newServer entirely
// unchanged; there is nothing on their path for a substitution to touch.
//
// This does not make the whole pass read-only on its own: the sliced path also writes a
// correlation + profile marker under the pod work dir, which the caller redirects with
// deviceplugin.RedirectHostWrites.
func (p *preflighter) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect()
	if err != nil {
		return nil, nil, err
	}

	srv := newServer(p.logger, mode)
	if s, ok := srv.(*server); ok && s.smlu != nil {
		s.smlu = &recordingSMLUDriver{read: p.smlu}
	}

	responder, ok := srv.(deviceplugin.ContainerAllocateResponder)
	if !ok {
		restore()
		return nil, nil, fmt.Errorf("cambricon %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}

// profileRequest is one (card, compute%, VRAM MiB) a responder asked recordingSMLUDriver to cut
// a profile for.
type profileRequest struct {
	card     string
	coresPct int
	memMiB   int64
}

// instanceRequest is one (card, profile, name) a responder asked recordingSMLUDriver to
// instantiate.
type instanceRequest struct {
	card      string
	profileID int32
	name      string
}

// recordingSMLUDriver answers reads from the driver underneath and records writes rather than
// making them.
//
// Cambricon is the one manufacturer where a naive simulated pass would provision real hardware
// state: reserveInstance does not stop at a boolean mode flag the way Ascend's container-share
// path does -- on a card whose mode is off it cuts an sMLU profile and instantiates it, a
// resource that outlives the container and has to be torn down again. Every write reserveInstance
// can make -- the mode flag, the profile, the instance -- is therefore recorded here instead of
// reaching the driver. Reading through (GetSMLUMode, ListInstances, ListProfiles) is what keeps
// the simulation honest: a responder over a driver that invented its answers would take whichever
// branch the invention chose, not the one this host would send it down.
type recordingSMLUDriver struct {
	read smluDriver

	// modeWrites names each card a responder asked to turn sMLU mode on for, in call order.
	modeWrites []string
	// profileCreates names each profile a responder asked to cut, in call order.
	profileCreates []profileRequest
	// instanceCreates names each instance a responder asked to create, in call order.
	instanceCreates []instanceRequest
	// nextProfileID hands back a distinct profile ID per withheld create, so a sliced pass that
	// creates more than one instance in one call does not collide two of them onto the same ID.
	nextProfileID int32
}

func (d *recordingSMLUDriver) GetSMLUMode(card string) (bool, error) {
	return d.read.GetSMLUMode(card)
}

func (d *recordingSMLUDriver) SetSMLUMode(card string, _ bool) error {
	d.modeWrites = append(d.modeWrites, card)
	return nil
}

func (d *recordingSMLUDriver) CreateProfile(card string, coresPct int, memMiB int64) (int32, error) {
	d.profileCreates = append(d.profileCreates, profileRequest{card: card, coresPct: coresPct, memMiB: memMiB})
	d.nextProfileID++
	return d.nextProfileID, nil
}

func (d *recordingSMLUDriver) DestroyProfile(string, int32) error {
	return nil
}

func (d *recordingSMLUDriver) CreateInstance(card string, profileID int32, name string) (smluInstance, error) {
	d.instanceCreates = append(d.instanceCreates, instanceRequest{card: card, profileID: profileID, name: name})
	// No device node: the real one is assigned by the driver at create time, which this call
	// deliberately never reaches. A responder that needs it (VIRTUAL_DEVICES, the instance's own
	// node) simply omits what it cannot know, the same way it omits them today when the driver
	// reports none.
	return smluInstance{card: card, profileID: profileID, name: name}, nil
}

func (d *recordingSMLUDriver) DestroyInstance(string, string) error {
	return nil
}

func (d *recordingSMLUDriver) ListInstances() ([]smluInstance, error) {
	return d.read.ListInstances()
}

func (d *recordingSMLUDriver) ListProfiles() ([]profileKey, error) {
	return d.read.ListProfiles()
}

// check establishes one accelerator's sMLU mode: read it, and where it is off, ask the driver to
// turn it on and put it straight back.
//
// Reading alone cannot answer the question. A mode that is off is not a card that cannot serve --
// the allocator turns it on itself when a slice lands -- so a read that stops there leaves the
// operator no wiser about whether the allocation would have worked. The toggle is safe precisely
// because it only happens where the mode was off: no slice exists on the card, so nothing can notice
// the window.
//
// A read that failed is not written past, and this is where a preflight parts company with
// ensureSMLUModeEnabled. The allocator writes past a bad read because it wants sMLU mode ON and
// leaves it there, so the worst case is a mode that was already on. This command has to put back
// what it found, and its restore is unconditionally OFF -- so writing past a failed read would end
// with a transient cnDev error having turned off a mode that was serving live instances. Reporting
// the card unreadable is the lesser error.
//
// A restore that fails is the one host mutation this command could not undo, so it is a failure of
// the row rather than a note on it: the detail says the card was left in sMLU mode, and the state
// makes the command exit non-zero.
func (p *preflighter) check(accel *workercore.Accelerator) device.PreflightCheck {
	c := device.PreflightCheck{Accelerator: accel.ID, Capability: smluCapability, Mode: device.PreflightModeOf(workercore.DeviceAllocationModeSliced)}

	// LogicalSliced.Count is the accelerator's own advertised logical-slicing capacity: zero on a
	// card the driver disclaims the capability for (or one currently in a hardware-partitioned
	// mode), which the allocator's sliced path never even attempts. There is no sMLU sentinel for
	// this -- cnDev serves one generation of the API -- so the accelerator's own declared
	// capability is what tells not-declared apart from a driver that could not be asked.
	if accel.Status.LogicalSliced.Count == 0 {
		c.State = device.PreflightStateNotDeclared
		c.Reason = "the accelerator reports no logical-slicing capability, so no sMLU instance is carved on it"
		return c
	}

	// cnDev addresses the card by its PCI bus ID, which is what the allocator passes it too.
	card := accel.Topology.PciBusID
	if card == "" {
		c.State = device.PreflightStateUnavailable
		c.Reason = "the accelerator reports no pci bus id, so cnDev cannot address it"
		return c
	}

	enabled, readErr := p.smlu.GetSMLUMode(card)
	c.State, c.Detail, c.Reason = classifySMLUCall(enabled, readErr)

	switch {
	case errors.Is(readErr, errSMLUUnsupported):
		// A library this code cannot query is one it cannot manage, and the allocator refuses on
		// it without touching the card. Writing here would touch it.
		return c
	case readErr != nil:
		// Any other failed read leaves the mode's own state unknown, and the restore below is
		// unconditionally off. Writing here could turn off a mode that was already on.
		return c
	case enabled:
		return c
	case p.dryRun:
		// Asking the mode on is a write, however briefly it is held, and a restore that fails leaves
		// the card in sMLU mode. A dry run promises the host nothing of the sort.
		c.State, c.Detail, c.Reason = device.PreflightStateOK, smluDetailNotAsked, ""
		return c
	}

	if err := p.smlu.SetSMLUMode(card, true); err != nil {
		c.State, c.Detail, c.Reason = classifySMLUCall(false, err)
		return c
	}
	// The panic path, and only it. Every ordinary outcome below sets its own row and marks this
	// done; what is left over is a vendor library that crashed with the card in a mode this process
	// never chose. Panic containment is one of this command's own features, and what it does is
	// report the panic -- it cannot undo what the panicking code already did, and a card left in
	// sMLU mode because a driver crashed is not something to report and walk away from. The read
	// above established the mode was off, so off is where it goes back to.
	done := false
	defer func() {
		if done {
			return
		}
		if err := p.smlu.SetSMLUMode(card, false); err != nil {
			p.logger.Error(err, "could not put sMLU mode back after a panic",
				"accelerator", accel.ID, "card", card)
		}
	}()

	if err := p.smlu.SetSMLUMode(card, false); err != nil {
		done = true
		p.logger.Error(err, "could not put sMLU mode back", "accelerator", accel.ID, "card", card)
		c.State, c.Detail, c.Reason = device.PreflightStateUnavailable, smluDetailNotRestored, err.Error()
		return c
	}
	done = true
	p.logger.Info("sMLU mode accepted being turned on, and was put back",
		"accelerator", accel.ID, "card", card)
	c.State, c.Detail, c.Reason = device.PreflightStateOK, smluDetailDisabled, ""
	return c
}

// classifySMLUCall maps one sMLU-mode driver call's outcome onto the preflight state it means. It
// mirrors ensureSMLUModeEnabled's own classification of the same outcomes, so a preflight row and
// the allocation it predicts cannot disagree:
//
//   - a call that answered is ok, and what it answered is the detail -- a mode that is off is
//     still ok, because the allocator turns it on itself;
//   - any failure, including one carrying errSMLUUnsupported, is unavailable, and an allocation is
//     refused on it.
//
// There is no not-declared branch here, unlike Ascend's classifyShareCall: cnDev serves one
// generation of the sMLU API, with a single sentinel that always means "this library or driver
// cannot be asked at all" -- the same refusal ensureSMLUModeEnabled gives every other failure --
// rather than a second, differently-consequenced verdict. What is not-declared for Cambricon is
// decided in check, from the accelerator's own advertised capability, before the driver is ever
// called.
func classifySMLUCall(enabled bool, err error) (state device.PreflightState, detail, reason string) {
	switch {
	case err != nil:
		return device.PreflightStateUnavailable, "", err.Error()
	case enabled:
		return device.PreflightStateOK, smluDetailEnabled, ""
	default:
		return device.PreflightStateOK, smluDetailDisabled, ""
	}
}
