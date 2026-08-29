package cambricon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// preflightAccelerator builds the one-accelerator fixture PreflightAccelerator is exercised
// against: addressed as cnDev card testCard0, with a configurable declared logical-slicing
// capacity so a case can put the not-declared branch on or off the table.
func preflightAccelerator(logicalSlicedCount int32, card string) workercore.Accelerator {
	return workercore.Accelerator{
		ID:    "MLU-0",
		Index: 0,
		Topology: workercore.DeviceTopology{
			PciBusID: card,
		},
		Status: workercore.AcceleratorStatus{
			LogicalSliced: workercore.AcceleratorLogicalSliced{Count: logicalSlicedCount},
		},
	}
}

// preflightOneAccelerator runs a preflight over a single accelerator addressed as cnDev card
// testCard0, declaring one logical slice unless the case overrides it, and returns its one check.
func preflightOneAccelerator(t *testing.T, smlu smluDriver, accel workercore.Accelerator) device.PreflightCheck {
	t.Helper()

	p := &preflighter{logger: klog.Background(), smlu: smlu}
	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           testGroupID,
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{accel},
	}})

	require.Equal(t, Manufacturer, grp.Manufacturer)
	require.Len(t, grp.Checks, 1, "one accelerator yields one check")
	require.Equal(t, accel.ID, grp.Checks[0].Accelerator)
	require.Equal(t, smluCapability, grp.Checks[0].Capability)
	return grp.Checks[0]
}

// classifySMLUCall is what turns a driver's answer into the two states a driver call itself can
// reach -- unlike Ascend, cnDev carries one sentinel and it means the same thing (refuse) wherever
// it surfaces, so there is no ordering hazard between two sentinels to pin here. What the
// ordering hazard becomes for Cambricon -- not-declared outranking a driver call that was never
// even made -- is pinned in TestPreflightAccelerator_NotDeclaredOutranksMissingPciBusID instead,
// because that branch is decided in check before the driver is ever reached.
func TestClassifySMLUCall(t *testing.T) {
	testCases := []struct {
		name       string
		enabled    bool
		err        error
		wantState  device.PreflightState
		wantDetail string
	}{
		{
			name:       "a mode that is on is ok",
			enabled:    true,
			wantState:  device.PreflightStateOK,
			wantDetail: smluDetailEnabled,
		},
		{
			// The allocator turns the mode on itself when a slice lands, so an accelerator carrying
			// no slice yet has nothing wrong with it.
			name:       "a mode that is off is still ok",
			wantState:  device.PreflightStateOK,
			wantDetail: smluDetailDisabled,
		},
		{
			name:      "an ordinary read failure is unavailable",
			err:       errors.New("get smlu mode: TIME_OUT"),
			wantState: device.PreflightStateUnavailable,
		},
		{
			// Cambricon carries one sentinel, not two: an absent sMLU API always means "this
			// library or driver cannot be asked at all", which is the same refusal every other
			// failure gets. There is no second, differently-consequenced verdict for it to
			// outrank, unlike Ascend's errShareNotDeclared.
			name:      "an absent sMLU API is unavailable too, not a distinct verdict",
			enabled:   true,
			err:       fmt.Errorf("get smlu mode: FUNCTION_NOT_FOUND: %w", errSMLUUnsupported),
			wantState: device.PreflightStateUnavailable,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			state, detail, reason := classifySMLUCall(tc.enabled, tc.err)
			assert.Equal(t, tc.wantState, state, "state")
			assert.Equal(t, tc.wantDetail, detail, "detail")
			if tc.err == nil {
				assert.Empty(t, reason, "a call that answered carries no reason")
				return
			}
			assert.Equal(t, tc.err.Error(), reason, "the driver's own words are the reason")
		})
	}
}

// The mode is established, not merely read. A mode that is off is not a card that cannot serve --
// the allocator turns it on itself when a slice lands -- so a read that stops there leaves the
// operator no wiser. Asking the driver, and putting the mode straight back, is what turns that into
// an answer.
//
// The toggle only happens where the mode was off, so no slice exists on the card and nothing can
// notice the window. What must never happen quietly is the card being left in sMLU mode.
func TestPreflightAccelerator_Write(t *testing.T) {
	unsupportedRead := fmt.Errorf("get smlu mode: FUNCTION_NOT_FOUND: %w", errSMLUUnsupported)
	unsupportedWrite := fmt.Errorf("set smlu mode: FUNCTION_NOT_FOUND: %w", errSMLUUnsupported)
	untrusted := errors.New("get smlu mode: TIME_OUT")

	testCases := []struct {
		name             string
		enabled          bool
		getErr           error
		setErr           error
		failNthModeWrite int
		wantLeftEnabled  bool
		wantState        device.PreflightState
		wantDetail       string
		wantSetCalls     int
	}{
		{
			name:            "a mode already on is reported and left alone",
			enabled:         true,
			wantLeftEnabled: true,
			wantState:       device.PreflightStateOK,
			wantDetail:      smluDetailEnabled,
		},
		{
			name:         "a mode that is off is asked on and put back",
			wantState:    device.PreflightStateOK,
			wantDetail:   smluDetailDisabled,
			wantSetCalls: 2,
		},
		{
			// A library this code cannot query is one it cannot manage; the allocator refuses
			// without touching the card, so the preflight must not touch it either.
			name:      "a library with no sMLU API is never touched",
			getErr:    unsupportedRead,
			wantState: device.PreflightStateUnavailable,
		},
		{
			// ensureSMLUModeEnabled may write past a bad read because it wants the mode ON and
			// leaves it there. A preflight has to put back what it found -- and after a failed read
			// it does not know what that was, while the restore it would make is unconditionally
			// OFF. So a transient cnDev failure on a card already in sMLU mode would end with this
			// command having taken it out of that mode, and live instances with it.
			name:      "a read it could not trust is not written past",
			getErr:    untrusted,
			wantState: device.PreflightStateUnavailable,
		},
		{
			name:         "an ask the driver refused is unavailable, and leaves nothing to put back",
			setErr:       errors.New("set smlu mode: OPER_NOT_PERMITTED"),
			wantState:    device.PreflightStateUnavailable,
			wantSetCalls: 1,
		},
		{
			// cnDev looks the getter and the setter up independently, so the absence can surface on
			// the ask after a read that worked. It still refuses -- unlike Ascend, there is no
			// not-declared verdict this side of the driver for it to fall into instead: an absent
			// setter is a driver that cannot be asked, the same as an absent getter.
			name:         "an ask refused for want of the API is unavailable, not not-declared",
			setErr:       unsupportedWrite,
			wantState:    device.PreflightStateUnavailable,
			wantSetCalls: 1,
		},
		{
			// The one outcome that must never be quiet: the card is in sMLU mode and nobody asked
			// for it to be, so the row is the only thing that can send someone to turn it off.
			// Saying it in the detail is not enough -- an ok row exits zero, and the automation
			// that runs this reads the exit code, not the prose.
			name:             "a restore that failed is a failure, not a detail",
			setErr:           errors.New("set smlu mode: OPER_NOT_PERMITTED"),
			failNthModeWrite: 2,
			wantLeftEnabled:  true,
			wantState:        device.PreflightStateUnavailable,
			wantDetail:       smluDetailNotRestored,
			wantSetCalls:     2,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			smlu := newFakeDriver()
			smlu.modes[testCard0] = tc.enabled
			smlu.failModeRead, smlu.failModeWrite = tc.getErr, tc.setErr
			smlu.failNthModeWrite = tc.failNthModeWrite

			check := preflightOneAccelerator(t, smlu, preflightAccelerator(1, testCard0))

			assert.Equal(t, tc.wantState, check.State, "state")
			assert.Equal(t, tc.wantDetail, check.Detail, "detail")
			_, writes := smlu.modeCalls()
			assert.Equal(t, tc.wantSetCalls, writes, "writes")
			// The observable end state is what matters: a probe that asked the mode on has to have
			// put it back, or the card stays carved for slices nobody scheduled.
			assert.Equal(t, tc.wantLeftEnabled, smlu.modes[testCard0],
				"the mode was not left the way it was found")
			if tc.wantState == device.PreflightStateOK {
				assert.Empty(t, check.Reason, "a check that passed carries no reason")
			} else {
				assert.NotEmpty(t, check.Reason, "a check that did not pass must say why")
			}
		})
	}
}

// The guarantee the whole toggle rests on: where the read did not establish that the mode was off,
// the driver is not written to at all. The fake would fail a write too, and the point is that no
// write is reached to fail -- because this command's restore is unconditionally off, and putting a
// mode "back" to a state that was never read is how a live capability gets turned off.
func TestPreflightAccelerator_AFailedReadIsNeverWrittenPast(t *testing.T) {
	smlu := newFakeDriver()
	smlu.failModeRead = errors.New("get smlu mode: TIME_OUT")
	smlu.failModeWrite = errors.New("set smlu mode: OPER_NOT_PERMITTED")

	check := preflightOneAccelerator(t, smlu, preflightAccelerator(1, testCard0))

	assert.Equal(t, device.PreflightStateUnavailable, check.State)
	assert.Contains(t, check.Reason, "TIME_OUT", "the read's own failure, which is all that happened")
	assert.NotContains(t, check.Reason, "OPER_NOT_PERMITTED", "no write was attempted to fail")
	assert.Empty(t, smlu.modeWrites, "the driver was not written to at all")
}

// A dry run promises, in the flag's own help and on two documentation pages, to write nothing to the
// host — driver state included. Asking sMLU mode on is a write, and a restore that fails leaves the
// card in it, so the ask is withheld and the row says the capability was not established.
func TestPreflightAccelerator_DryRunAsksNothingOfTheDriver(t *testing.T) {
	smlu := newFakeDriver() // mode off: the case a non-dry run would write on

	p := &preflighter{logger: klog.Background(), smlu: smlu, dryRun: true}
	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           testGroupID,
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{preflightAccelerator(4, testCard0)},
	}})

	require.Len(t, grp.Checks, 1)
	_, writes := smlu.modeCalls()
	assert.Zero(t, writes, "a dry run asked the driver to change state")
	assert.Equal(t, device.PreflightStateOK, grp.Checks[0].State)
	assert.Contains(t, grp.Checks[0].Detail, "dry run",
		"the row has to say the capability was not established, not that it works")
}

// An accelerator that declares no logical-slicing capability is not-declared, and the driver is
// never even asked about it: there is nothing to enable on a card the allocator's sliced path
// would never attempt to slice in the first place.
func TestPreflightAccelerator_NoLogicalSlicingIsNotDeclared(t *testing.T) {
	smlu := newFakeDriver()

	check := preflightOneAccelerator(t, smlu, preflightAccelerator(0, testCard0))

	assert.Equal(t, device.PreflightStateNotDeclared, check.State)
	assert.NotEmpty(t, check.Reason)
	reads, writes := smlu.modeCalls()
	assert.Zero(t, reads, "not-declared is decided before the driver is asked")
	assert.Zero(t, writes, "and before anything could be written")
}

// An accelerator the detector recorded with no PCI bus ID cannot be addressed by cnDev at all,
// which is a failure of this check and not a capability the driver disclaimed.
func TestPreflightAccelerator_MissingPciBusIdIsUnavailable(t *testing.T) {
	smlu := newFakeDriver()

	check := preflightOneAccelerator(t, smlu, preflightAccelerator(1, ""))

	assert.Equal(t, device.PreflightStateUnavailable, check.State)
	reads, writes := smlu.modeCalls()
	assert.Zero(t, reads, "an accelerator with no address is never asked about")
	assert.Zero(t, writes, "and never written to")
}

// The declared-capability check runs before the card is even addressed, so an accelerator that
// fails both (no logical slicing AND no PCI bus ID) is reported not-declared, not unavailable.
// Reversing the order would tell an operator to go fix an address on a card the allocator would
// never try to slice regardless.
func TestPreflightAccelerator_NotDeclaredOutranksMissingPciBusID(t *testing.T) {
	smlu := newFakeDriver()

	check := preflightOneAccelerator(t, smlu, preflightAccelerator(0, ""))

	assert.Equal(t, device.PreflightStateNotDeclared, check.State)
}

// The simulated depth exists to answer what an allocation would inject without becoming one. Its
// whole claim rests on the card being left as it was found -- and unlike Ascend's boolean flag,
// Cambricon's write can cut and instantiate an sMLU profile, a resource that outlives the
// container. What a pass wrote is asserted directly on the real driver's own call log, rather
// than inferred from the response it produced.
func TestPreflightResponder_WritesNothing(t *testing.T) {
	defer deviceplugin.RedirectHostWrites(t.TempDir())()

	// The mode is off, which is the case an allocation would write on: a pass over an
	// already-enabled card would leave it untouched for the wrong reason.
	smlu := newFakeDriver()
	p := &preflighter{logger: klog.Background(), smlu: smlu}

	responder, restore5, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)
	defer restore5()
	require.NoError(t, err)

	pod, ctr := slicedPod("simulated", "train", 25, 50)
	_, err = responder.GetContainerAllocateResponse(context.Background(), pod, ctr,
		cambriconDevicesFixture(), allocateMLU0())
	require.NoError(t, err)

	assert.Zero(t, smlu.profileCreates, "a simulated pass must not cut an sMLU profile")
	assert.Zero(t, smlu.instanceCreates, "a simulated pass must not instantiate an sMLU instance")
	reads, writes := smlu.modeCalls()
	assert.Zero(t, writes, "a simulated pass must not write the sMLU-mode flag")
	assert.NotZero(t, reads, "and it must still read the real driver, or it is asserting on an invented branch")
}

// The recording driver stands between the responder and the device, so what it recorded is the
// evidence that the responder did try to write -- without which the test above could pass simply
// because nothing ever reached the driver.
func TestPreflightResponder_RecordsWhatItWithheld(t *testing.T) {
	defer deviceplugin.RedirectHostWrites(t.TempDir())()

	rec := &recordingSMLUDriver{read: newFakeDriver()}
	srv := newSlicedServer(rec)

	pod, ctr := slicedPod("recorded", "train", 25, 50) // 25% compute, 50% of 49152 = 24576 MiB
	_, err := srv.GetContainerAllocateResponse(context.Background(), pod, ctr,
		cambriconDevicesFixture(), allocateMLU0())
	require.NoError(t, err)

	assert.Equal(t, []string{testCard0}, rec.modeWrites,
		"the accelerator the allocation would have enabled sMLU mode on")
	require.Len(t, rec.profileCreates, 1)
	assert.Equal(t, profileRequest{card: testCard0, coresPct: 25, memMiB: 24576}, rec.profileCreates[0])
	require.Len(t, rec.instanceCreates, 1)
	assert.Equal(t, testCard0, rec.instanceCreates[0].card)
	assert.Equal(t, encodeInstanceName("recorded", "train"), rec.instanceCreates[0].name)
}

// The anti-drift test, adapted to the one place Cambricon cannot be Ascend: the instance's
// device node. A real create reads its device node back from the driver, which the recording
// driver deliberately never reaches (that read-back is exactly the hardware state a simulated
// pass must not provision), so the concrete node -- and consequently VIRTUAL_DEVICES -- cannot be
// asked to agree between the two paths without also making the simulated one create for real.
//
// What can and must agree, without exception, is what the two paths ASKED the driver to do: they
// must reserve the identical instance -- the same card, the same compute/VRAM quota, the same
// encoded name -- because that request is entirely computed by the allocator's own code before it
// ever reaches the driver seam, and it is the seam alone that the preflight driver replaces.
// PreflightResponder hands back a server built by the allocator's own newServer, so there is no
// second construction of that code to drift; this test does not establish that and cannot, but it
// pins the observable behavior the mechanism is supposed to guarantee.
func TestPreflightResponder_RequestsTheSameInstanceProductionWould(t *testing.T) {
	devs := cambriconDevicesFixture()
	allocated := allocateMLU0()

	// Production: the server an allocation is served by, over a driver that answers and really
	// creates. A distinct pod UID from the preflight side below, so the two runs never share a
	// correlation marker on disk.
	restoreProd := deviceplugin.RedirectHostWrites(t.TempDir())
	prodDriver := newFakeDriver()
	prodSrv := newSlicedServer(prodDriver)
	prodPod, prodCtr := slicedPod("drift-prod", "train", 25, 50)
	_, err := prodSrv.GetContainerAllocateResponse(context.Background(), prodPod, prodCtr, devs, allocated)
	require.NoError(t, err)
	restoreProd()

	wantName := encodeInstanceName("drift-prod", "train")
	wantInst, ok := prodDriver.instances[wantName]
	require.True(t, ok, "the production injection must actually create something to compare")
	wantQuota := prodDriver.profiles[profileKey{card: wantInst.card, profileID: wantInst.profileID}]

	// Preflight: the same server, reached through the seam, over a driver seeded identically (a
	// fresh, empty one, so it reuses no profile the production run happened to cut).
	defer deviceplugin.RedirectHostWrites(t.TempDir())()
	realDriver := newFakeDriver()
	p := &preflighter{logger: klog.Background(), smlu: realDriver}
	preSrv, restore6, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)
	defer restore6()
	require.NoError(t, err)
	prePod, preCtr := slicedPod("drift-pre", "train", 25, 50)
	_, err = preSrv.GetContainerAllocateResponse(context.Background(), prePod, preCtr, devs, allocated)
	require.NoError(t, err)

	s, ok := preSrv.(*server)
	require.True(t, ok)
	rec, ok := s.smlu.(*recordingSMLUDriver)
	require.True(t, ok, "PreflightResponder must hand the sliced server a recording driver")

	// Guarded against passing by comparing two empty requests: a preflight that asked the driver
	// for nothing would trivially "agree" with production about nothing.
	require.Len(t, rec.instanceCreates, 1, "the preflight path must ask to create exactly one instance")
	got := rec.instanceCreates[0]
	assert.Equal(t, wantInst.card, got.card, "the same card")
	// The name differs by design: it encodes the pod UID, and the two runs deliberately use
	// different ones so their correlation markers never collide on disk. What must agree is that
	// the preflight side asked for its own request's encoded name, not the production side's.
	assert.Equal(t, encodeInstanceName("drift-pre", "train"), got.name)
	require.Len(t, rec.profileCreates, 1)
	gotQuota := rec.profileCreates[0]
	assert.Equal(t, wantInst.card, gotQuota.card)
	assert.Equal(t, wantQuota.cores, gotQuota.coresPct, "the same compute quota")
	assert.Equal(t, wantQuota.mem, gotQuota.memMiB, "the same VRAM quota")

	// And the driver underneath the recording one -- the one standing in for real hardware --
	// never saw any of it.
	assert.Zero(t, realDriver.profileCreates, "no profile reached the real driver")
	assert.Zero(t, realDriver.instanceCreates, "no instance reached the real driver")
	_, writes := realDriver.modeCalls()
	assert.Zero(t, writes, "no mode write reached the real driver")
}

// PreflightResponder wires a substituted driver into only the sliced responder, because only the
// sliced responder drives an sMLU driver at all. The other modes hand out whole cards over no
// driver of their own, so newServer's own construction must come back unchanged for them --
// there is nothing on their path for a substitution to touch, and wrapping one in regardless
// would be a construction the allocator itself never runs.
//
// A malformed record (no cnDev device index) is what makes this comparable without real
// hardware: every non-sliced GetContainerAllocateResponse call on a host with no Cambricon card
// refuses for want of a device node it cannot find, which is host-dependent and not what this
// test is about. The missing-index refusal is decided before any device node is looked for, so
// it is the one outcome both paths reach identically on any machine this suite runs on.
func TestPreflightResponder_NonSlicedModesAreProductionUnchanged(t *testing.T) {
	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeVisibility,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			devs := cambriconDevicesFixture()
			devs.Spec.Groups[0].Accelerators[0].PhysicalIndexes = nil
			pod, ctr := wholeCardPod("drift-whole", "train")

			prodSrv := newWholeCardServer(mode)
			_, wantErr := prodSrv.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
			require.Error(t, wantErr, "the production injection must actually fail to compare against")

			p := &preflighter{logger: klog.Background(), smlu: newFakeDriver()}
			preSrv, restore7, err := p.PreflightResponder(mode)
			require.NoError(t, err)
			defer restore7()
			require.NoError(t, err)
			_, gotErr := preSrv.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocateMLU0())
			require.Error(t, gotErr)

			assert.Equal(t, wantErr.Error(), gotErr.Error(),
				"a preflight answer and the allocation it predicts must not be able to disagree")
			assert.Contains(t, gotErr.Error(), "carries no cnDev device index")
		})
	}
}

// The redirect the seam sets up is the whole of its read-only promise, and it is the one thing the
// tests around it cannot see: each of them opens a redirect of its own before calling, so a seam
// that had stopped redirecting would still write nowhere and still pass. This asserts the seam's
// own redirect directly, with no outer one to mask it.
//
// It goes through the exported constructor rather than the struct literal, so it also pins that
// what the registry hands out serves the injection seam at all.
func TestPreflightResponder_RedirectsTheSharedHostPathsAndPutsThemBack(t *testing.T) {
	origLib, origPods := deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir

	p, ok := NewPreflighter(device.PreflighterOptions{Logger: klog.Background()}).(deviceplugin.AcceleratorInjectionPreflighter)
	require.True(t, ok, "the registered preflighter must serve the injection seam")

	_, restore, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)

	assert.NotEqual(t, origLib, deviceplugin.OperatorLibDir,
		"a responder driven here must render under a scratch root, never the host's")
	assert.NotEqual(t, origPods, deviceplugin.OperatorPodsDir)

	restore()

	assert.Equal(t, origLib, deviceplugin.OperatorLibDir,
		"and the restore puts them back, or the rest of this process points at a directory that is gone")
	assert.Equal(t, origPods, deviceplugin.OperatorPodsDir)
}

// panickingSMLUDriver crashes on one nominated mode write, the way a vendor library can.
type panickingSMLUDriver struct {
	*fakeSMLUDriver
	panicOnNthWrite int
}

func (d *panickingSMLUDriver) SetSMLUMode(card string, enabled bool) error {
	d.mu.Lock()
	d.modeWrites++
	nth := d.modeWrites
	d.mu.Unlock()
	if nth == d.panicOnNthWrite {
		panic("cnDev crashed")
	}
	d.mu.Lock()
	d.modes[card] = enabled
	d.mu.Unlock()
	return nil
}

// Panic containment reports a panic; it cannot undo what the panicking code already did. This pass
// puts the card into sMLU mode knowing the read established it was off, so a driver that crashes
// before it goes back leaves persistent host state changed -- reported as a panic, and left that
// way. The restore has to survive the crash, not merely precede it.
func TestCheck_APanicDoesNotLeaveSMLUModeOn(t *testing.T) {
	smlu := &panickingSMLUDriver{
		fakeSMLUDriver:  newFakeDriver(),
		panicOnNthWrite: 2, // the restore itself, which is where it escapes from
	}
	p := &preflighter{logger: klog.Background(), smlu: smlu}

	assert.Panics(t, func() {
		p.PreflightAccelerator(device.DevicesGroupList{{
			ID:           testGroupID,
			Manufacturer: Manufacturer,
			Accelerators: []workercore.Accelerator{preflightAccelerator(1, testCard0)},
		}})
	}, "the driver crashed and this did not propagate it")

	assert.False(t, smlu.modes[testCard0],
		"the card was left in sMLU mode after the driver crashed putting it back")
}
