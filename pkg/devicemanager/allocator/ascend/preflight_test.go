package ascend

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

// classifyShareCall is what turns a driver's answer into the three states an operator acts on, and
// the two failure verdicts have opposite consequences at allocation time -- one refuses the
// accelerator, the other lets a co-tenant onto it. Both sentinels are therefore pinned here, along
// with the ordinary failures that must reach neither.
func TestClassifyShareCall(t *testing.T) {
	testCases := []struct {
		name       string
		enabled    bool
		err        error
		wantState  device.PreflightState
		wantDetail string
	}{
		{
			enabled:    true,
			name:       "a flag that is on is ok",
			wantState:  device.PreflightStateOK,
			wantDetail: shareDetailEnabled,
		},
		{
			// The allocator turns the flag on itself when a co-tenant lands, so an accelerator
			// carrying no slice yet has nothing wrong with it.
			name:       "a flag that is off is still ok",
			wantState:  device.PreflightStateOK,
			wantDetail: shareDetailDisabled,
		},
		{
			name:      "a generation that declares no such flag is not-declared",
			err:       fmt.Errorf("dcmi get device share enable: NOT_SUPPORT: %w", errShareNotDeclared),
			wantState: device.PreflightStateNotDeclared,
		},
		{
			name:      "a driver with no such entry point is unavailable",
			err:       fmt.Errorf("dcmi get device share enable: FUNCTION_NOT_FOUND: %w", errShareUnsupported),
			wantState: device.PreflightStateUnavailable,
		},
		{
			name:      "any other failure is unavailable",
			err:       errors.New("dcmi get device share enable: OPER_NOT_PERMITTED"),
			wantState: device.PreflightStateUnavailable,
		},
		{
			// The undeclared verdict marks a failure, so a classification that tested for a failure
			// first would report every one of them as merely unreadable -- and an unreadable flag
			// refuses the allocation this one lets through.
			name:      "an undeclared flag outranks the failure it also is",
			enabled:   true,
			err:       fmt.Errorf("dcmi get device share enable: NOT_SUPPORT: %w", errShareNotDeclared),
			wantState: device.PreflightStateNotDeclared,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			state, detail, reason := classifyShareCall(tc.enabled, tc.err)
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

// preflightOneAccelerator runs a preflight over a single accelerator addressed as dcmi card 0,
// device 0, and returns its one check together with the driver that served it.
func preflightOneAccelerator(t *testing.T, share *fakeShareDriver) device.PreflightCheck {
	t.Helper()

	p := &preflighter{logger: klog.Background(), share: share}
	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           "910b2",
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{
			{ID: testAccelID0, Index: 0, PhysicalIndexes: []uint32{3, 0, 0}},
		},
	}})

	require.Equal(t, Manufacturer, grp.Manufacturer)
	require.Len(t, grp.Checks, 1, "one accelerator yields one check")
	require.Equal(t, testAccelID0, grp.Checks[0].Accelerator)
	require.Equal(t, shareCapability, grp.Checks[0].Capability)
	return grp.Checks[0]
}

// The flag is established, not merely read. A flag that is off is not a node that cannot serve --
// the allocator turns it on itself when a second container lands -- so a read that stops there
// reports "off" and leaves the operator no wiser. Asking the driver, and putting the flag straight
// back, is what turns that into an answer, and it is exactly the call the allocator would make.
//
// The toggle only happens where the flag was off, so nothing on the node is sharing this accelerator
// and nothing can notice the window. What must never happen quietly is the accelerator being left
// on: that admits a second container nobody scheduled, so the row says so.
func TestPreflightAccelerator_Write(t *testing.T) {
	notDeclared := fmt.Errorf("dcmi get device share enable: NOT_SUPPORT: %w", errShareNotDeclared)
	unsupported := fmt.Errorf("dcmi: no such entry point: %w", errShareUnsupported)
	untrusted := errors.New("dcmi get device share enable: TIME_OUT")

	testCases := []struct {
		name            string
		enabled         bool
		getErr          error
		setErr          error
		failNthSet      int
		wantLeftEnabled bool
		wantState       device.PreflightState
		wantDetail      string
		wantSetCalls    int
	}{
		{
			name:            "a flag already on is reported and left alone",
			enabled:         true,
			wantLeftEnabled: true,
			wantState:       device.PreflightStateOK,
			wantDetail:      shareDetailEnabled,
		},
		{
			name:         "a flag that is off is asked on and put back",
			wantState:    device.PreflightStateOK,
			wantDetail:   shareDetailDisabled,
			wantSetCalls: 2,
		},
		{
			// The allocator may write past a bad read because it wants the flag ON and leaves it
			// there. A preflight has to put back what it found -- and after a failed read it does
			// not know what that was, while the restore it would make is unconditionally OFF. So a
			// transient read failure on an accelerator whose flag was already on would end with
			// this command having turned it off. Reporting a node unreadable is the lesser error.
			name:      "a read it could not trust is not written past",
			getErr:    untrusted,
			wantState: device.PreflightStateUnavailable,
		},
		{
			// The generation has no flag, so there is nothing an ask could add -- and asking would
			// be a host mutation made for a capability that does not exist.
			name:      "a generation that declares no flag is never touched",
			getErr:    notDeclared,
			wantState: device.PreflightStateNotDeclared,
		},
		{
			// A driver this code cannot query is one it cannot manage; the allocator refuses
			// without touching the device, so the preflight must not touch it either.
			name:      "a driver with no such entry point is never touched",
			getErr:    unsupported,
			wantState: device.PreflightStateUnavailable,
		},
		{
			name:         "an ask refused for want of the flag is not-declared",
			setErr:       fmt.Errorf("dcmi set device share enable: NOT_SUPPORT: %w", errShareNotDeclared),
			wantState:    device.PreflightStateNotDeclared,
			wantSetCalls: 1,
		},
		{
			name:         "an ask the driver refused is unavailable, and leaves nothing to put back",
			setErr:       errors.New("dcmi set device share enable: OPER_NOT_PERMITTED"),
			wantState:    device.PreflightStateUnavailable,
			wantSetCalls: 1,
		},
		{
			// The one outcome that must never be quiet: the accelerator is on and nobody asked for
			// it to be, so the row is the only thing that can send someone to turn it off. Saying
			// it in the detail is not enough -- an ok row exits zero, and the automation that runs
			// this reads the exit code, not the prose.
			name:            "a restore that failed is a failure, not a detail",
			setErr:          errors.New("dcmi set device share enable: OPER_NOT_PERMITTED"),
			failNthSet:      2,
			wantLeftEnabled: true,
			wantState:       device.PreflightStateUnavailable,
			wantDetail:      shareDetailNotRestored,
			wantSetCalls:    2,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			share := &fakeShareDriver{
				enabled:    map[[2]int32]bool{{0, 0}: tc.enabled},
				getErr:     tc.getErr,
				setErr:     tc.setErr,
				failNthSet: tc.failNthSet,
			}

			check := preflightOneAccelerator(t, share)

			assert.Equal(t, tc.wantState, check.State, "state")
			assert.Equal(t, tc.wantDetail, check.Detail, "detail")
			assert.Len(t, share.setCalls, tc.wantSetCalls, "writes")
			// The observable end state is what matters: a probe that asked the flag on has to have
			// put it back, or the accelerator admits a second container nobody scheduled.
			assert.Equal(t, tc.wantLeftEnabled, share.enabled[[2]int32{0, 0}],
				"the flag was not left the way it was found")
			if tc.wantState == device.PreflightStateOK {
				assert.Empty(t, check.Reason, "a check that passed carries no reason")
			} else {
				assert.NotEmpty(t, check.Reason, "a check that did not pass must say why")
			}
		})
	}
}

// The guarantee the whole toggle rests on: where the read did not establish that the flag was off,
// the driver is not written to at all. The fake would fail a write too, and the point is that no
// write is reached to fail -- because this command's restore is unconditionally off, and putting a
// flag "back" to a state that was never read is how a live capability gets turned off.
func TestPreflightAccelerator_AFailedReadIsNeverWrittenPast(t *testing.T) {
	share := &fakeShareDriver{
		enabled: map[[2]int32]bool{},
		getErr:  errors.New("dcmi get device share enable: TIME_OUT"),
		setErr:  errors.New("dcmi set device share enable: OPER_NOT_PERMITTED"),
	}

	check := preflightOneAccelerator(t, share)

	assert.Equal(t, device.PreflightStateUnavailable, check.State)
	assert.Contains(t, check.Reason, "TIME_OUT", "the read's own failure, which is all that happened")
	assert.NotContains(t, check.Reason, "OPER_NOT_PERMITTED", "no write was attempted to fail")
	assert.Empty(t, share.setCalls, "the driver was not written to at all")
}

// An accelerator the detector recorded without dcmi's card/device pair cannot be addressed at all,
// which is a failure of this check and not a capability the driver disclaimed.
func TestPreflightAccelerator_WithoutDcmiIndex(t *testing.T) {
	share := &fakeShareDriver{enabled: map[[2]int32]bool{}}
	p := &preflighter{logger: klog.Background(), share: share}

	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           "910b2",
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{{ID: testAccelID0, PhysicalIndexes: []uint32{3}}},
	}})

	require.Len(t, grp.Checks, 1)
	assert.Equal(t, device.PreflightStateUnavailable, grp.Checks[0].State)
	assert.Empty(t, share.getCalls, "an accelerator with no address is never asked about")
	assert.Empty(t, share.setCalls, "and never written to")
}

// A dry run promises, in the flag's own help and on two documentation pages, to write nothing to the
// host — driver state included. Asking the flag on is a write, however briefly it is held, and a
// restore that fails leaves the card enabled. So the ask is withheld, and the row says the capability
// was not established rather than reporting it as one that was checked and found working.
func TestPreflightAccelerator_DryRunAsksNothingOfTheDriver(t *testing.T) {
	share := &fakeShareDriver{enabled: map[[2]int32]bool{}} // off: the case a non-dry run would write on
	p := &preflighter{logger: klog.Background(), share: share, dryRun: true}

	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           "910b2",
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{{ID: testAccelID0, PhysicalIndexes: []uint32{0, 0, 0}}},
	}})

	require.Len(t, grp.Checks, 1)
	assert.Empty(t, share.setCalls, "a dry run asked the driver to change state")
	assert.Equal(t, device.PreflightStateOK, grp.Checks[0].State)
	assert.Contains(t, grp.Checks[0].Detail, "dry run",
		"the row has to say the capability was not established, not that it works")
}

// The simulated depth exists to answer what an allocation would inject without becoming one. Its
// whole claim rests on the device being left as it was found, so what a pass wrote is asserted
// directly rather than inferred from the response it produced.
func TestPreflightResponder_WritesNothing(t *testing.T) {
	defer deviceplugin.RedirectHostWrites(t.TempDir())()

	// The flag is off on both accelerators, which is the case an allocation would write on: a pass
	// over an already-enabled device would leave it untouched for the wrong reason.
	share := &fakeShareDriver{enabled: map[[2]int32]bool{}}
	p := &preflighter{logger: klog.Background(), share: share}

	responder, restore3, err := p.PreflightResponder(workercore.DeviceAllocationModeShared)
	require.NoError(t, err)
	defer restore3()
	require.NoError(t, err)

	pod, ctr := slicedPod("simulated", "train", 10, 25)
	_, err = responder.GetContainerAllocateResponse(context.Background(), pod, ctr,
		ascendDevicesFixture(), map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)

	assert.Empty(t, share.setCalls, "a simulated pass must not write the container-share flag")
	assert.NotEmpty(t, share.getCalls,
		"and it must still read the real driver, or it is asserting on an invented branch")
}

// The recording driver stands between the responder and the device, so what it recorded is the
// evidence that the responder did try to write -- without which the test above could pass simply
// because nothing ever reached the driver.
func TestPreflightResponder_RecordsTheWriteItWithheld(t *testing.T) {
	defer deviceplugin.RedirectHostWrites(t.TempDir())()

	rec := &recordingShareDriver{read: &fakeShareDriver{enabled: map[[2]int32]bool{}}}
	srv, ok := newServer(klog.Background(), workercore.DeviceAllocationModeShared, rec,
		newFakeProductResolver()).(*server)
	require.True(t, ok)

	pod, ctr := slicedPod("recorded", "train", 10, 25)
	_, err := srv.GetContainerAllocateResponse(context.Background(), pod, ctr, ascendDevicesFixture(),
		map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1})
	require.NoError(t, err)

	assert.Equal(t, [][2]int32{{0, 0}}, rec.writes,
		"the accelerator the allocation would have enabled, named by its dcmi card and device")
}

// The anti-drift test. A preflight answer is only worth anything while the injection it reports is
// the one an allocation emits: the same request through a production server and through a preflight
// responder must produce the same injection, differing in nothing.
//
// What keeps that true is the seam itself -- PreflightResponder hands back a server built by the
// allocator's own newServer, so there is no second construction to drift. This test does not
// establish that, and cannot: a hand-built but presently equivalent server passes it. It pins the
// behavior, and the one line calling newServer pins the mechanism. Both are load-bearing, and a
// reviewer who deletes the second should not expect this to notice.
func TestPreflightResponder_MatchesTheProductionResponder(t *testing.T) {
	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			enabled := map[[2]int32]bool{{0, 0}: true, {1, 0}: true}
			devs := ascendDevicesFixture()
			allocated := map[deviceplugin.Resource]int32{{Group: "910b2", Device: testAccelID0}: 1}

			// The seam is entered first, and both responders are driven inside the window it opens.
			// The host root a slice's mounts are rendered under is an input to the injection, not
			// part of its identity: a simulated pass deliberately renders under a scratch root, and
			// comparing one root against another would only measure that difference. Holding it
			// constant is what leaves the comparison about the injection.
			p := &preflighter{
				logger: klog.Background(), share: &fakeShareDriver{enabled: enabled},
				product: newFakeProductResolver(),
			}
			preSrv, restore, err := p.PreflightResponder(mode)
			require.NoError(t, err)
			defer restore()

			// Production: the server an allocation is served by, over a driver that answers.
			prodSrv, ok := newServer(klog.Background(), mode, &fakeShareDriver{enabled: enabled},
				newFakeProductResolver()).(deviceplugin.ContainerAllocateResponder)
			require.True(t, ok)
			prodPod, prodCtr := slicedPod("drift", "train", 10, 25)
			want, err := prodSrv.GetContainerAllocateResponse(context.Background(), prodPod, prodCtr,
				devs, allocated)
			require.NoError(t, err)

			prePod, preCtr := slicedPod("drift", "train", 10, 25)
			got, err := preSrv.GetContainerAllocateResponse(context.Background(), prePod, preCtr,
				devs, allocated)
			require.NoError(t, err)

			// Guarded against passing by comparing two empty responses: every Ascend mode emits at
			// least the visibility env, so an injection carrying nothing is a broken fixture rather
			// than an agreement.
			require.NotEmpty(t, want.Envs, "the production injection must carry something to compare")
			assert.Equal(t, want, got,
				"a preflight answer and the allocation it predicts must not be able to disagree")
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

// panickingShareDriver crashes on one nominated write, the way a vendor library can.
type panickingShareDriver struct {
	*fakeShareDriver
	panicOnNthSet int
}

func (d *panickingShareDriver) SetShareEnabled(cardID, deviceID int32, enabled bool) error {
	if len(d.setCalls)+1 == d.panicOnNthSet {
		d.setCalls = append(d.setCalls, [2]int32{cardID, deviceID})
		panic("dcmi crashed")
	}
	return d.fakeShareDriver.SetShareEnabled(cardID, deviceID, enabled)
}

// Panic containment reports a panic; it cannot undo what the panicking code already did. This pass
// turns the flag on knowing nothing else on the node shares the accelerator, so a driver that
// crashes before it goes back leaves a card admitting a second container -- reported as a panic, and
// left that way. The restore has to survive the crash, not merely precede it.
func TestCheck_APanicDoesNotLeaveContainerShareOn(t *testing.T) {
	share := &panickingShareDriver{
		fakeShareDriver: &fakeShareDriver{enabled: map[[2]int32]bool{}},
		panicOnNthSet:   2, // the restore itself, which is where it escapes from
	}
	p := &preflighter{logger: klog.Background(), share: share}

	assert.Panics(t, func() {
		p.PreflightAccelerator(device.DevicesGroupList{{
			ID:           "910b2",
			Manufacturer: Manufacturer,
			Accelerators: []workercore.Accelerator{
				{ID: testAccelID0, Index: 0, PhysicalIndexes: []uint32{3, 0, 0}},
			},
		}})
	}, "the driver crashed and this did not propagate it")

	assert.False(t, share.enabled[[2]int32{0, 0}],
		"the accelerator was left sharing after the driver crashed putting the flag back")
}
