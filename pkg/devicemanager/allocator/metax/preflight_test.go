package metax

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

// preflightAccelerator builds the one-accelerator fixture PreflightAccelerator is exercised
// against, addressed as sysfs bdf testBDF0 unless the case overrides it.
func preflightAccelerator(bdf string) workercore.Accelerator {
	return workercore.Accelerator{
		ID:       "GPU-0",
		Index:    0,
		Topology: workercore.DeviceTopology{PciBusID: bdf},
	}
}

// preflightOneAccelerator runs a preflight over a single accelerator and returns its one check.
func preflightOneAccelerator(t *testing.T, mgr sgpuManager, accel workercore.Accelerator) device.PreflightCheck {
	t.Helper()

	p := &preflighter{logger: klog.Background(), sgpu: mgr}
	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           "c500",
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{accel},
	}})

	require.Equal(t, Manufacturer, grp.Manufacturer)
	require.Len(t, grp.Checks, 1, "one accelerator yields one check")
	require.Equal(t, accel.ID, grp.Checks[0].Accelerator)
	require.Equal(t, sgpuCapability, grp.Checks[0].Capability)
	return grp.Checks[0]
}

// classifySGPUListCall is what turns a registry read into the two states the read itself can
// reach -- MetaX's own detector declares every accelerator sgpu-capable unconditionally, and
// sgpuManager carries no sentinel for a generation that lacks the capability, so there is no
// not-declared verdict to pin here (unlike Ascend's classifyShareCall).
func TestClassifySGPUListCall(t *testing.T) {
	testCases := []struct {
		name       string
		registry   []sgpuSubdevice
		bdf        string
		err        error
		wantState  device.PreflightState
		wantDetail string
	}{
		{
			name:       "a card already hosting a subdevice is already in sgpu mode",
			registry:   []sgpuSubdevice{{bdf: testBDF0, index: 0}},
			bdf:        testBDF0,
			wantState:  device.PreflightStateOK,
			wantDetail: sgpuDetailEnabled,
		},
		{
			// The allocator puts the accelerator into sgpu mode itself when the first slice
			// lands, so an accelerator carrying no subdevice yet has nothing wrong with it.
			name:       "a card with no subdevice yet is still ok",
			registry:   nil,
			bdf:        testBDF0,
			wantState:  device.PreflightStateOK,
			wantDetail: sgpuDetailDisabled,
		},
		{
			name:       "a registry that a different card hosts a subdevice on is still not yet in sgpu mode",
			registry:   []sgpuSubdevice{{bdf: testBDF1, index: 0}},
			bdf:        testBDF0,
			wantState:  device.PreflightStateOK,
			wantDetail: sgpuDetailDisabled,
		},
		{
			name:      "a registry read failure is unavailable",
			bdf:       testBDF0,
			err:       errors.New("list sgpu subdevices: read /sys/bus/pci/devices: permission denied"),
			wantState: device.PreflightStateUnavailable,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			state, detail, reason := classifySGPUListCall(tc.registry, tc.bdf, tc.err)
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

// fakeSGPUManagerWithFailures wraps fakeSGPUManager to inject failures on List, EnsureModel and
// SetSchedClass -- the three calls the allocator can make -- and records each
// EnsureModel/SetSchedClass call so a test can assert the write did or did not happen.
type fakeSGPUManagerWithFailures struct {
	*fakeSGPUManager
	listErr       error
	ensureErr     error
	schedClassErr error

	ensureModelCalls   []string
	setSchedClassCalls []string
}

func (f *fakeSGPUManagerWithFailures) List() ([]sgpuSubdevice, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.fakeSGPUManager.List()
}

func (f *fakeSGPUManagerWithFailures) EnsureModel(bdf string) error {
	f.ensureModelCalls = append(f.ensureModelCalls, bdf)
	if f.ensureErr != nil {
		return f.ensureErr
	}
	return f.fakeSGPUManager.EnsureModel(bdf)
}

func (f *fakeSGPUManagerWithFailures) SetSchedClass(bdf string, c schedClass) error {
	f.setSchedClassCalls = append(f.setSchedClassCalls, bdf)
	if f.schedClassErr != nil {
		return f.schedClassErr
	}
	return f.fakeSGPUManager.SetSchedClass(bdf, c)
}

// Reaching sgpu mode is not a toggle: EnsureModel plus SetSchedClass create a subdevice and pick a
// scheduler for it, a resource that outlives the call and has to be torn down again. The two
// manufacturers whose preflight asks a driver to flip a mode can put it straight back; there is
// nothing to put back here, so this check reads the registry and stops -- and what it must never do
// is leave a subdevice on a node it was asked to inspect.
func TestPreflightAccelerator_ReadsAndWritesNothing(t *testing.T) {
	testCases := []struct {
		name          string
		seedSubdevice bool
		listErr       error
		wantState     device.PreflightState
		wantDetail    string
	}{
		{
			name:          "a card already in sgpu mode is reported as such",
			seedSubdevice: true,
			wantState:     device.PreflightStateOK,
			wantDetail:    sgpuDetailEnabled,
		},
		{
			name:       "a card not yet in sgpu mode is reported as such",
			wantState:  device.PreflightStateOK,
			wantDetail: sgpuDetailDisabled,
		},
		{
			name:      "a registry that cannot be read is unavailable",
			listErr:   errors.New("list sgpu subdevices: TIME_OUT"),
			wantState: device.PreflightStateUnavailable,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &fakeSGPUManagerWithFailures{
				fakeSGPUManager: newFakeMgr(),
				listErr:         tc.listErr,
			}
			if tc.seedSubdevice {
				mgr.seed(testBDF0, 0, "")
			}

			check := preflightOneAccelerator(t, mgr, preflightAccelerator(testBDF0))

			assert.Equal(t, tc.wantState, check.State, "state")
			assert.Equal(t, tc.wantDetail, check.Detail, "detail")
			assert.Empty(t, mgr.ensureModelCalls, "the check put a card into sgpu mode")
			assert.Empty(t, mgr.setSchedClassCalls, "the check picked a scheduler for a card")
		})
	}
}

// An accelerator the detector recorded with no PCI bus ID cannot be addressed by sysfs at all,
// which is a failure of this check and not a capability the driver disclaimed.
func TestPreflightAccelerator_MissingPciBusIdIsUnavailable(t *testing.T) {
	mgr := newFakeMgr()

	check := preflightOneAccelerator(t, mgr, preflightAccelerator(""))

	assert.Equal(t, device.PreflightStateUnavailable, check.State)
	assert.NotEmpty(t, check.Reason)
	assert.Zero(t, mgr.creates, "an accelerator with no address is never asked about")
}

// The simulated depth exists to answer what an allocation would inject without becoming one. Its
// whole claim rests on the card being left as it was found -- and unlike Ascend's boolean flag,
// MetaX's write can put the accelerator into sgpu mode and create a subdevice, a resource that
// outlives the container. What a pass wrote is asserted directly on the real (fake) driver's own
// call log, rather than inferred from the response it produced.
func TestPreflightResponder_WritesNothing(t *testing.T) {
	defer deviceplugin.RedirectHostWrites(t.TempDir())()

	// No subdevice exists yet, which is the case an allocation would write on: a pass over an
	// already-provisioned card would leave it untouched for the wrong reason.
	mgr := newFakeMgr()
	p := &preflighter{logger: klog.Background(), sgpu: mgr}

	responder, restore, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)
	defer restore()

	pod, ctr := slicedPod("simulated", "train", 60, 50)
	_, err = responder.GetContainerAllocateResponse(context.Background(), pod, ctr,
		metaxDevicesFixture(), allocateGPU0())
	require.NoError(t, err)

	assert.Zero(t, mgr.creates, "a simulated pass must not create an sgpu subdevice")
}

// The recording manager stands between the responder and the device, so what it recorded is the
// evidence that the responder did try to write -- without which the test above could pass simply
// because nothing ever reached the driver.
func TestPreflightResponder_RecordsWhatItWithheld(t *testing.T) {
	defer deviceplugin.RedirectHostWrites(t.TempDir())()

	rec := &recordingSGPUManager{read: newFakeMgr()}
	srv := newSlicedServer(rec)

	pod, ctr := slicedPod("recorded", "train", 60, 50) // 60% compute, 50% of 65536 = 32768 MiB
	_, err := srv.GetContainerAllocateResponse(context.Background(), pod, ctr,
		metaxDevicesFixture(), allocateGPU0())
	require.NoError(t, err)

	assert.Equal(t, []string{testBDF0}, rec.ensureModelWrites,
		"the accelerator the allocation would have put into sgpu mode")
	require.Len(t, rec.setSchedClassWrites, 1)
	assert.Equal(t, schedClassRequest{bdf: testBDF0, class: schedFixedShare}, rec.setSchedClassWrites[0])
	require.Len(t, rec.creates, 1)
	assert.Equal(t, createRequest{bdf: testBDF0, index: 0, vramMiB: 32768, alias: encodeAlias("recorded")},
		rec.creates[0])
	assert.NotZero(t, rec.listCalls,
		"the responder must still read the real driver, or it is asserting on an invented branch")
}

// The anti-drift test. A preflight answer is only worth anything while the injection it reports is
// the one an allocation emits: the same request through a production server and through a preflight
// responder must produce the same injection, differing in nothing.
//
// What keeps that true is the seam itself -- PreflightResponder hands back a server built by the
// allocator's own newServer, so there is no second construction to drift. This test does not
// establish that, and cannot: a hand-built but presently equivalent server passes it. It pins the
// behavior, and the one line calling newServer pins the mechanism.
func TestPreflightResponder_MatchesTheProductionResponder(t *testing.T) {
	redirectDevicePaths(t)

	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeVisibility,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			devs := metaxDevicesFixture()
			allocated := allocateGPU0()

			// The seam is entered first, and both responders are driven inside the window it
			// opens. The host root a slice's mounts are rendered under is an input to the
			// injection, not part of its identity, so it is held constant across both paths.
			p := &preflighter{logger: klog.Background(), sgpu: newFakeMgr()}
			preSrv, restore, err := p.PreflightResponder(mode)
			require.NoError(t, err)
			defer restore()

			prodSrv, ok := newServer(klog.Background(), mode).(deviceplugin.ContainerAllocateResponder)
			require.True(t, ok)
			prodPod, prodCtr := slicedPod("drift", "train", 60, 50)
			want, err := prodSrv.GetContainerAllocateResponse(context.Background(), prodPod, prodCtr,
				devs, allocated)
			require.NoError(t, err)

			prePod, preCtr := slicedPod("drift", "train", 60, 50)
			got, err := preSrv.GetContainerAllocateResponse(context.Background(), prePod, preCtr,
				devs, allocated)
			require.NoError(t, err)

			// Guarded against passing by comparing two empty responses: every non-sliced mode
			// emits at least the node/card devices, so an injection carrying nothing is a broken
			// fixture rather than an agreement.
			require.NotEmpty(t, want.Devices, "the production injection must carry something to compare")
			assert.Equal(t, want, got,
				"a preflight answer and the allocation it predicts must not be able to disagree")
		})
	}
}

// The anti-drift test for the sliced mode, adapted to the one place MetaX cannot compare
// byte-for-byte with production: like Cambricon's sMLU instance, a real Create's subdevice index is
// what a live driver would derive at create time, and the recording manager deliberately never
// creates for real. What can and must agree, without exception, is what the two paths ASKED the
// driver to do: the same bdf, the same VRAM quota, the same alias, and the same METAX_SGPUS entry
// derived from an identical index -- because that request is entirely computed by the allocator's
// own code before it ever reaches the sgpu seam, and it is the seam alone that the preflight
// manager replaces. Both runs use a fresh, empty fake registry, so the derived index is
// deterministic (index 0) and the METAX_SGPUS env string can be compared byte-for-byte too.
func TestPreflightResponder_SlicedMatchesTheProductionResponder(t *testing.T) {
	redirectDevicePaths(t)

	devs := metaxDevicesFixture()
	allocated := allocateGPU0()

	restoreProd := deviceplugin.RedirectHostWrites(t.TempDir())
	prodMgr := newFakeMgr()
	prodSrv := newSlicedServer(prodMgr)
	prodPod, prodCtr := slicedPod("drift", "train", 60, 50)
	want, err := prodSrv.GetContainerAllocateResponse(context.Background(), prodPod, prodCtr, devs, allocated)
	require.NoError(t, err)
	restoreProd()

	defer deviceplugin.RedirectHostWrites(t.TempDir())()
	p := &preflighter{logger: klog.Background(), sgpu: newFakeMgr()}
	preSrv, restore, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)
	defer restore()
	prePod, preCtr := slicedPod("drift", "train", 60, 50)
	got, err := preSrv.GetContainerAllocateResponse(context.Background(), prePod, preCtr, devs, allocated)
	require.NoError(t, err)

	require.NotEmpty(t, want.Devices, "the production injection must carry something to compare")
	require.NotEmpty(t, want.Envs["METAX_SGPUS"], "the production injection must actually carry something to compare")
	assert.Equal(t, want, got,
		"a preflight answer and the allocation it predicts must not be able to disagree")

	// And the driver underneath the recording one -- the one standing in for real hardware --
	// never saw the write/create.
	s, ok := preSrv.(*server)
	require.True(t, ok)
	rec, ok := s.sgpu.(*recordingSGPUManager)
	require.True(t, ok, "PreflightResponder must hand the sliced server a recording manager")
	realMgr, ok := rec.read.(*fakeSGPUManager)
	require.True(t, ok)
	assert.Zero(t, realMgr.creates, "no subdevice reached the real driver")
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
