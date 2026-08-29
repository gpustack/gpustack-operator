package thead

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

func TestNewPreflighter(t *testing.T) {
	p := NewPreflighter(device.PreflighterOptions{Logger: klog.Background()})
	pf, ok := p.(*preflighter)
	require.True(t, ok)
	assert.NotNil(t, pf.mig, "the preflighter drives the same vendor driver seam an allocation does")
}

// classifyCardInstancesCall is what turns the accelerator's own declaration and the driver's answer
// into one of the three states a row can carry. Neither input decides alone: an empty declaration is
// what the detector publishes both for an accelerator without the capability and for one whose
// catalog it could not read, so the driver is asked either way and the two are told apart by what
// it says.
func TestClassifyCardInstancesCall(t *testing.T) {
	testCases := []struct {
		name               string
		declared           bool
		instances          []migInstance
		err                error
		wantState          device.PreflightState
		wantDetail         string
		wantReasonContains string
	}{
		{
			name:       "a readable subtree with no live instance is ok",
			declared:   true,
			wantState:  device.PreflightStateOK,
			wantDetail: "the partition subtree is readable and carries 0 live gpu instance(s)",
		},
		{
			name:       "a readable subtree carrying live instances is ok",
			declared:   true,
			instances:  []migInstance{{GiID: 1}, {GiID: 2}},
			wantState:  device.PreflightStateOK,
			wantDetail: "the partition subtree is readable and carries 2 live gpu instance(s)",
		},
		{
			name:      "a driver that cannot be asked is unavailable",
			declared:  true,
			err:       errors.New("card ppu-0: list live gpu instances of profile 3: OPER_NOT_PERMITTED"),
			wantState: device.PreflightStateUnavailable,
		},
		{
			name:               "an undeclared accelerator whose driver answers nothing is not-declared",
			wantState:          device.PreflightStateNotDeclared,
			wantReasonContains: "declares no physical-slice profile and its driver reports no live gpu instance",
		},
		{
			// The reason detection's empty inventory is not taken at face value: the same emptiness is
			// published when the catalog could not be read, and reading it here is what tells them
			// apart. Classified before the declaration, or every unreadable accelerator would pass as
			// one without the capability and exit 0.
			name:      "an undeclared accelerator whose driver cannot be asked is unavailable, not not-declared",
			err:       errors.New("card ppu-0: read partition mode: OPER_NOT_PERMITTED"),
			wantState: device.PreflightStateUnavailable,
		},
		{
			// The hardware disproving the node's own record. Reporting it as a capability the
			// accelerator does not have would exit 0 on the one accelerator that needs re-detecting.
			name:               "an undeclared accelerator carrying live partitions is unavailable",
			instances:          []migInstance{{GiID: 1}, {GiID: 2}, {GiID: 3}},
			wantState:          device.PreflightStateUnavailable,
			wantReasonContains: "reports 3 live gpu instance(s) on it: the profile inventory taken at detection is short",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			state, detail, reason := classifyCardInstancesCall(tc.declared, tc.instances, tc.err)
			assert.Equal(t, tc.wantState, state, "state")
			assert.Equal(t, tc.wantDetail, detail, "detail")
			if tc.wantReasonContains != "" {
				assert.Contains(t, reason, tc.wantReasonContains, "reason")
			}
			switch {
			case tc.err != nil:
				assert.Equal(t, tc.err.Error(), reason, "the driver's own words are the reason")
			case tc.wantState == device.PreflightStateOK:
				assert.Empty(t, reason, "a call that answered carries no reason")
			default:
				assert.NotEmpty(t, reason, "a check that did not pass must say why")
			}
		})
	}
}

// preflightOneAccelerator runs a preflight over a single accelerator and returns its one check.
func preflightOneAccelerator(t *testing.T, mig migDriver, accel workercore.Accelerator) device.PreflightCheck {
	t.Helper()

	p := &preflighter{logger: klog.Background(), mig: mig}
	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           "ppu",
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{accel},
	}})

	require.Equal(t, Manufacturer, grp.Manufacturer)
	require.Len(t, grp.Checks, 1, "one accelerator yields one check")
	require.Equal(t, accel.ID, grp.Checks[0].Accelerator)
	require.Equal(t, migCapability, grp.Checks[0].Capability)
	return grp.Checks[0]
}

// withProfile returns an accelerator declaring one physical-slice profile at cardUUID, the same shape
// theadDevices builds for every other package test.
func withProfile(cardUUID string) workercore.Accelerator {
	return theadDevices(testProfile, 1, 2, cardUUID).Spec.Groups[0].Accelerators[0]
}

// TestPreflightAccelerator pins the precondition every partitioned allocation's reservation core reads
// before it decides whether to adopt a leftover instance or carve a fresh one: the partition subtree
// must be readable whole. It never writes -- there is no capability here to toggle, only
// a subtree to prove readable -- so wantCardInstancesCalls also pins when the driver is asked at all.
func TestPreflightAccelerator(t *testing.T) {
	testCases := []struct {
		name                   string
		accel                  workercore.Accelerator
		seed                   func(drv *fakeMigDriver)
		wantState              device.PreflightState
		wantDetail             string
		wantReasonContains     string
		wantCardInstancesCalls int
	}{
		{
			// Nothing can be established about an accelerator no vendor handle addresses -- including
			// that it has no capability, which is a claim this command now makes only after the driver
			// has confirmed it.
			name:      "an accelerator declaring neither profile nor id is unavailable",
			accel:     workercore.Accelerator{},
			wantState: device.PreflightStateUnavailable,
		},
		{
			name:               "an accelerator declaring no physical-slice profile is not-declared",
			accel:              workercore.Accelerator{ID: testPPUUUID0},
			wantState:          device.PreflightStateNotDeclared,
			wantReasonContains: "declares no physical-slice profile and its driver reports no live gpu instance",
			// Asked even though nothing is declared: an empty inventory is also what detection
			// publishes when it could not read the catalog, and the driver is the only thing that
			// tells the two apart.
			wantCardInstancesCalls: 1,
		},
		{
			name:  "an accelerator declaring no profile whose driver cannot be asked is unavailable",
			accel: workercore.Accelerator{ID: testPPUUUID0},
			seed: func(drv *fakeMigDriver) {
				drv.listErr = errors.New("card ppu-0: read partition mode failed")
			},
			wantState:              device.PreflightStateUnavailable,
			wantReasonContains:     "read partition mode failed",
			wantCardInstancesCalls: 1,
		},
		{
			name:  "an accelerator declaring no profile that carries live partitions is unavailable",
			accel: workercore.Accelerator{ID: testPPUUUID0},
			seed: func(drv *fakeMigDriver) {
				drv.seedLive(testPPUUUID0, migInstance{GiID: 1, UUID: "MIG-1"})
			},
			wantState:              device.PreflightStateUnavailable,
			wantReasonContains:     "the profile inventory taken at detection is short",
			wantCardInstancesCalls: 1,
		},
		{
			name: "an accelerator with no unique id is unavailable",
			accel: func() workercore.Accelerator {
				a := withProfile(testPPUUUID0)
				a.ID = ""
				return a
			}(),
			wantState: device.PreflightStateUnavailable,
		},
		{
			name:                   "a readable subtree carrying no live instance is ok",
			accel:                  withProfile(testPPUUUID0),
			wantState:              device.PreflightStateOK,
			wantDetail:             "the partition subtree is readable and carries 0 live gpu instance(s)",
			wantCardInstancesCalls: 1,
		},
		{
			name:  "a readable subtree carrying live instances is ok",
			accel: withProfile(testPPUUUID0),
			seed: func(drv *fakeMigDriver) {
				drv.seedLive(testPPUUUID0, migInstance{GiID: 1, UUID: "MIG-1"})
			},
			wantState:              device.PreflightStateOK,
			wantDetail:             "the partition subtree is readable and carries 1 live gpu instance(s)",
			wantCardInstancesCalls: 1,
		},
		{
			name:  "an unreadable subtree is unavailable",
			accel: withProfile(testPPUUUID0),
			seed: func(drv *fakeMigDriver) {
				drv.listErr = errors.New("card ppu-0: enumeration incomplete")
			},
			wantState:              device.PreflightStateUnavailable,
			wantReasonContains:     "enumeration incomplete",
			wantCardInstancesCalls: 1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			drv := newFakeMigDriver()
			if tc.seed != nil {
				tc.seed(drv)
			}

			check := preflightOneAccelerator(t, drv, tc.accel)

			assert.Equal(t, tc.wantState, check.State, "state")
			assert.Equal(t, tc.wantDetail, check.Detail, "detail")
			assert.Equal(t, tc.wantCardInstancesCalls, drv.cardListCalls, "driver calls")
			if tc.wantReasonContains != "" {
				assert.Contains(t, check.Reason, tc.wantReasonContains)
			}
			if tc.wantState == device.PreflightStateOK {
				assert.Empty(t, check.Reason, "a check that passed carries no reason")
			} else {
				assert.NotEmpty(t, check.Reason, "a check that did not pass must say why")
			}
		})
	}
}

// TestPreflightResponder_NeverWritesAPartition drives every mode's server through the one universal
// entry point the preflight seam is allowed to reach, and asserts the recording driver underneath
// never saw a create or a destroy. GetContainerAllocateResponse does not read the partition driver at
// all today -- only ActuatePhysicalSliced does, and it is never reachable through PreflightResponder --
// so this pins that absence structurally rather than trusting a reading of the source that a later
// change could invalidate silently.
func TestPreflightResponder_NeverWritesAPartition(t *testing.T) {
	redirectNodeRoots(t)
	writeCardNodes(t)
	defer deviceplugin.RedirectHostWrites(t.TempDir())()

	devs := slicedDevices(98304, testPPUUUID0, testPPUUUID1)
	allocated := allocatedOn(testPPUUUID0)

	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	rec := &recordingMigDriver{read: drv}

	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced,
		workercore.DeviceAllocationModePartitioned,
		workercore.DeviceAllocationModeVisibility,
	}
	for _, mode := range modes {
		srv, ok := newServer(klog.Background(), mode, rec).(deviceplugin.ContainerAllocateResponder)
		require.True(t, ok)

		pod, ctr := slicedPod("simulated", 10, 25, 0)
		_, err := srv.GetContainerAllocateResponse(context.Background(), pod, ctr, devs, allocated)
		require.NoError(t, err, "mode %s", mode)
	}

	assert.Empty(t, rec.creates, "a simulated pass must never attempt to create a partition")
	assert.Empty(t, rec.destroys, "and never attempt to destroy one")
}

// The recording driver stands between the responder and the vendor library, so what it recorded (or
// did not) is the evidence that the responder reached, or never reached, the driver at all --
// without which the test above could pass simply because nothing on this path calls migDriver.
func TestRecordingMigDriver_CreateInstanceRefusesRatherThanFabricates(t *testing.T) {
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	rec := &recordingMigDriver{read: drv}

	_, err := rec.CreateInstance(testPPUUUID0, testProfile, 1, 2, migPlacement{Start: 0, Length: 2})
	require.Error(t, err, "a create must never fabricate success")
	require.Len(t, rec.creates, 1)
	assert.Equal(t, recordedCreate{
		cardUUID: testPPUUUID0, profile: testProfile,
		computeSlices: 1, memorySlices: 2, slot: migPlacement{Start: 0, Length: 2},
	}, rec.creates[0])
	assert.Zero(t, drv.createCalls, "the real driver underneath must never be asked to create")
}

func TestRecordingMigDriver_ReadsThroughToTheRealDriver(t *testing.T) {
	drv := newFakeMigDriver()
	drv.seedCard(testPPUUUID0)
	drv.seedLive(testPPUUUID0, migInstance{GiID: 1, UUID: "MIG-1"})
	rec := &recordingMigDriver{read: drv}

	insts, err := rec.CardInstances(testPPUUUID0)
	require.NoError(t, err)
	assert.Equal(t, []migInstance{{GiID: 1, UUID: "MIG-1"}}, insts,
		"a driver that invented its answers would send a caller down a branch the hardware would not")
	assert.Equal(t, 1, drv.cardListCalls)
}

// The anti-drift test. A preflight answer is only worth anything while the injection it reports is the
// one an allocation emits: the same request through a production server and through a preflight
// responder must produce the same injection, differing in nothing.
//
// What keeps that true is the seam itself -- PreflightResponder hands back a server built by the
// allocator's own newServer, so there is no second construction to drift. This test does not establish
// that, and cannot: a hand-built but presently equivalent server passes it. It pins the behavior, and
// the one line calling newServer in PreflightResponder pins the mechanism.
func TestPreflightResponder_MatchesTheProductionResponder(t *testing.T) {
	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced,
		workercore.DeviceAllocationModePartitioned,
		workercore.DeviceAllocationModeVisibility,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			redirectNodeRoots(t)
			writeCardNodes(t)
			devs := slicedDevices(98304, testPPUUUID0, testPPUUUID1)
			allocated := allocatedOn(testPPUUUID0)

			drv := newFakeMigDriver()
			drv.seedCard(testPPUUUID0)

			// The seam is entered first, and both responders are driven inside the window it opens.
			// The host root a slice's mounts are rendered under is an input to the injection, not part
			// of its identity: a simulated pass deliberately renders under a scratch root, and comparing
			// one root against another would only measure that difference.
			// Preflight: the same server, reached through the seam, over the recording driver.
			p := &preflighter{logger: klog.Background(), mig: drv}
			preSrv, restore, err := p.PreflightResponder(mode)
			require.NoError(t, err)
			defer restore()

			// Production: the server an allocation is served by.
			prodSrv, ok := newServer(klog.Background(), mode, drv).(deviceplugin.ContainerAllocateResponder)
			require.True(t, ok)
			prodPod, prodCtr := slicedPod("drift", 10, 25, 0)
			want, err := prodSrv.GetContainerAllocateResponse(context.Background(), prodPod, prodCtr, devs, allocated)
			require.NoError(t, err)

			prePod, preCtr := slicedPod("drift", 10, 25, 0)
			got, err := preSrv.GetContainerAllocateResponse(context.Background(), prePod, preCtr, devs, allocated)
			require.NoError(t, err)

			// Guarded against passing by comparing two empty responses: every mode emits at least the
			// device set, so a response carrying no device is a broken fixture rather than an agreement.
			require.NotEmpty(t, want.Devices, "the production injection must carry something to compare")
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
