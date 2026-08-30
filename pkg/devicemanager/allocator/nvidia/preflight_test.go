package nvidia

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// classifyMigOutcome is what turns a MIG read into the three states an operator acts on. The two
// failure verdicts have opposite consequences at allocation time — not-declared lets every
// non-physical-slice allocation mode proceed, unavailable refuses the accelerator outright — so
// both are pinned here on the exact terms profileGeometry and reserveMigInstance (mig.go) read
// them on.
func TestClassifyMigOutcome(t *testing.T) {
	testCases := []struct {
		name               string
		declared           bool
		live               int
		err                error
		wantState          device.PreflightState
		wantDetail         string
		wantReasonContains string
	}{
		{
			name:       "a declared accelerator with no live partition is ok",
			declared:   true,
			wantState:  device.PreflightStateOK,
			wantDetail: "the mig subtree is readable and carries 0 live gpu instance(s)",
		},
		{
			name:       "a declared accelerator carrying live partitions is ok",
			declared:   true,
			live:       2,
			wantState:  device.PreflightStateOK,
			wantDetail: "the mig subtree is readable and carries 2 live gpu instance(s)",
		},
		{
			// A consumer card reports MIG as "[N/A]" on the host's own CLI; its detect-time record
			// carries no physical-slice profile, and the driver confirms it holds nothing. That is a
			// correct answer, not a failure.
			name:      "an undeclared accelerator whose driver answers nothing is not-declared",
			declared:  false,
			wantState: device.PreflightStateNotDeclared,
		},
		{
			name:      "a declared accelerator the driver could not be asked about is unavailable",
			declared:  true,
			err:       errors.New("card GPU-0: get nvml handle for GPU-0: ERROR_UNKNOWN"),
			wantState: device.PreflightStateUnavailable,
		},
		{
			// A driver error outranks an empty declaration, and it has to: detectMigProfiles publishes
			// the same empty inventory for an accelerator without the capability and for one whose
			// catalog it could not read. Deciding on the declaration first would report every
			// unreadable accelerator as one that simply has no MIG, and exit 0 on it.
			name:      "an undeclared accelerator the driver could not be asked about is unavailable",
			declared:  false,
			err:       errors.New("card GPU-0: get gpu instance profile info 9: ERROR_UNKNOWN"),
			wantState: device.PreflightStateUnavailable,
		},
		{
			// The hardware disproving the node's own record: nothing declared, yet partitions are live
			// on it. Reporting that as a capability the accelerator does not have would exit 0 on the
			// one accelerator that needs re-detecting.
			name:               "an undeclared accelerator carrying live partitions is unavailable",
			declared:           false,
			live:               3,
			wantState:          device.PreflightStateUnavailable,
			wantReasonContains: "nvml reports 3 live gpu instance(s) on it",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			state, detail, reason := classifyMigOutcome(tc.declared, tc.live, tc.err)
			assert.Equal(t, tc.wantState, state, "state")
			assert.Equal(t, tc.wantDetail, detail, "detail")
			if tc.wantReasonContains != "" {
				assert.Contains(t, reason, tc.wantReasonContains, "reason")
			}
			switch {
			case tc.err != nil:
				assert.Equal(t, tc.err.Error(), reason, "the driver's own words are the reason")
			case tc.wantState == device.PreflightStateNotDeclared:
				assert.Equal(t, migNotDeclaredReason, reason, "not-declared carries its own fixed reason")
			case tc.wantState == device.PreflightStateOK:
				assert.Empty(t, reason, "a call that answered carries no reason")
			default:
				assert.NotEmpty(t, reason, "a check that did not pass must say why")
			}
		})
	}
}

// migAccelerator builds an accelerator fixture carrying a physical-slice profile (or none), the
// shape check reads before it ever asks the driver.
func migAccelerator(id string, declared bool) workercore.Accelerator {
	accel := workercore.Accelerator{ID: id}
	if declared {
		accel.Status.PhysicalSliced.Profiles = []workercore.AcceleratorPhysicalSlicedProfile{
			{Name: "1g.10gb", ComputeSlices: 1, MemorySlices: 2},
		}
	}
	return accel
}

// preflightOneAccelerator runs a preflight over a single accelerator and returns its one check.
func preflightOneAccelerator(t *testing.T, mig migDriver, accel workercore.Accelerator) device.PreflightCheck {
	t.Helper()

	p := &preflighter{logger: klog.Background(), mig: mig}
	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           "a10g",
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{accel},
	}})

	require.Equal(t, Manufacturer, grp.Manufacturer)
	require.Len(t, grp.Checks, 1, "one accelerator yields one check")
	require.Equal(t, accel.ID, grp.Checks[0].Accelerator)
	require.Equal(t, migCapability, grp.Checks[0].Capability)
	return grp.Checks[0]
}

// The read is the whole check, and what decides whether the driver is even asked is pinned per
// accelerator shape. The driver is asked about an undeclared accelerator too: CardInstances cannot
// carry a not-declared verdict on its own (it returns an empty, error-free list for both "no MIG
// here" and "MIG here, nothing live"), but neither can the declaration, which detection publishes
// empty for an accelerator without the capability and for one whose catalog it could not read
// alike. Only an accelerator no handle addresses is refused before the driver is reached.
func TestPreflightAccelerator(t *testing.T) {
	testCases := []struct {
		name               string
		accel              workercore.Accelerator
		live               []migInstance
		listErr            error
		wantState          device.PreflightState
		wantDetail         string
		wantReasonContains string
		wantAsked          bool
	}{
		{
			name:      "an undeclared accelerator whose driver answers nothing is not-declared",
			accel:     migAccelerator(testGPUUUID0, false),
			wantState: device.PreflightStateNotDeclared,
			wantAsked: true,
		},
		{
			name:      "an undeclared accelerator the driver could not be asked about is unavailable",
			accel:     migAccelerator(testGPUUUID0, false),
			listErr:   errors.New("card GPU-0: get gpu instance profile info 9: ERROR_UNKNOWN"),
			wantState: device.PreflightStateUnavailable,
			wantAsked: true,
		},
		{
			name:  "an undeclared accelerator carrying a live partition is unavailable",
			accel: migAccelerator(testGPUUUID0, false),
			live: []migInstance{
				{GiID: 1, CiID: 1, ComputeSlices: 1, UUID: "MIG-" + testGPUUUID0 + "-1"},
			},
			wantState:          device.PreflightStateUnavailable,
			wantReasonContains: "the profile inventory taken at detection is short",
			wantAsked:          true,
		},
		{
			name:      "a declared accelerator with no unique id is unavailable and never asks the driver",
			accel:     migAccelerator("", true),
			wantState: device.PreflightStateUnavailable,
		},
		{
			name:       "a declared accelerator with no live partition reads ok",
			accel:      migAccelerator(testGPUUUID0, true),
			wantState:  device.PreflightStateOK,
			wantDetail: "the mig subtree is readable and carries 0 live gpu instance(s)",
			wantAsked:  true,
		},
		{
			name:  "a declared accelerator carrying a live partition reads ok",
			accel: migAccelerator(testGPUUUID0, true),
			live: []migInstance{
				{GiID: 1, CiID: 1, ComputeSlices: 1, UUID: "MIG-" + testGPUUUID0 + "-1"},
			},
			wantState:  device.PreflightStateOK,
			wantDetail: "the mig subtree is readable and carries 1 live gpu instance(s)",
			wantAsked:  true,
		},
		{
			name:      "a declared accelerator the driver could not be asked about is unavailable",
			accel:     migAccelerator(testGPUUUID0, true),
			listErr:   errors.New("card GPU-0: get max mig device count: ERROR_UNKNOWN"),
			wantState: device.PreflightStateUnavailable,
			wantAsked: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mig := newFakeMigDriver()
			mig.listErr = tc.listErr
			for _, inst := range tc.live {
				mig.seedLive(tc.accel.ID, inst)
			}

			check := preflightOneAccelerator(t, mig, tc.accel)

			assert.Equal(t, tc.wantState, check.State, "state")
			assert.Equal(t, tc.wantDetail, check.Detail, "detail")
			if tc.wantReasonContains != "" {
				assert.Contains(t, check.Reason, tc.wantReasonContains, "reason")
			}
			if tc.wantState == device.PreflightStateOK {
				assert.Empty(t, check.Reason, "a check that passed carries no reason")
			} else {
				assert.NotEmpty(t, check.Reason, "a check that did not pass must say why")
			}
			if tc.wantAsked {
				assert.Equal(t, 1, mig.cardListCalls, "the driver must be asked exactly once")
			} else {
				assert.Zero(t, mig.cardListCalls, "an accelerator the check refused on its own shape is never asked about")
			}
		})
	}
}

// plainAllocation builds a minimal Pod/Container/Devices/allocated set for the non-sliced
// (exclusive/shared) response path, which needs no .sliced resource requests.
func plainAllocation(uid string) (*core.Pod, *core.Container, *workercore.Devices, map[deviceplugin.Resource]int32) {
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "pod-" + uid, UID: types.UID(uid)},
		Spec:       core.PodSpec{Containers: []core.Container{{Name: "train"}}},
	}
	devs := nvidiaDevices("12.4", 24576, testGPUUUID0)
	allocated := map[deviceplugin.Resource]int32{{Group: "a10g", Device: testGPUUUID0}: 1}
	return pod, &pod.Spec.Containers[0], devs, allocated
}

// modeAllocation builds the request a mode's GetContainerAllocateResponse is driven with: the
// sliced path needs its .sliced resource requests, every other mode needs only the plain fixture.
func modeAllocation(
	mode workercore.DeviceAllocationMode, uid string,
) (*core.Pod, *core.Container, *workercore.Devices, map[deviceplugin.Resource]int32) {
	if mode == workercore.DeviceAllocationModeSliced {
		devs := nvidiaDevices("12.4", 24576, testGPUUUID0)
		pod, ctr := slicedPod(uid, "train", 10, 25)
		allocated := map[deviceplugin.Resource]int32{{Group: "a10g", Device: testGPUUUID0}: 1}
		return pod, ctr, devs, allocated
	}
	return plainAllocation(uid)
}

// The anti-drift test. A preflight answer is only worth anything while the injection it reports is
// the one an allocation emits: the same request through a production server and through a
// preflight responder must produce the same injection, differing in nothing.
//
// What keeps that true is the seam itself — PreflightResponder hands back a server built by the
// package's own newServer, so there is no second construction to drift. This test does not
// establish that, and cannot: a hand-built but presently equivalent server passes it. It pins the
// behavior, and the one line calling newServer pins the mechanism.
func TestPreflightResponder_MatchesTheProductionResponder(t *testing.T) {
	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			injection := deviceplugin.DefaultInjectionResolver(injectionConfig)
			pod, container, devs, allocated := modeAllocation(mode, "drift-"+mode.String())

			// The seam is entered first, and both responders are driven inside the window it opens.
			// The host root a slice's mounts are rendered under is an input to the injection, not part
			// of its identity: a simulated pass deliberately renders under a scratch root, and comparing
			// one root against another would only measure that difference.
			p := &preflighter{logger: klog.Background(), mig: newFakeMigDriver(), injection: injection}
			preSrv, restore, err := p.PreflightResponder(mode)
			require.NoError(t, err)
			defer restore()

			// Production: the server an allocation is served by.
			prodSrv, ok := newServer(klog.Background(), mode, nil, injection).(deviceplugin.ContainerAllocateResponder)
			require.True(t, ok)
			want, err := prodSrv.GetContainerAllocateResponse(context.Background(), pod, container, devs, allocated)
			require.NoError(t, err)

			got, err := preSrv.GetContainerAllocateResponse(context.Background(), pod, container, devs, allocated)
			require.NoError(t, err)

			// Guarded against passing by comparing two empty responses: every NVIDIA mode emits at
			// least the visibility env, so an injection carrying nothing is a broken fixture rather
			// than an agreement.
			require.NotEmpty(t, want.Envs, "the production injection must carry something to compare")
			assert.Equal(t, want, got,
				"a preflight answer and the allocation it predicts must not be able to disagree")
		})
	}
}

// The sliced responder writes host directories (the per-container work dir and the shared
// HAMi-core lock dir) but never consults the MIG driver — PreflightResponder passes nil for it, so
// any future GetContainerAllocateResponse read of it panics instead of silently reaching a device.
// That panic is what this test is standing in for: it proves a full round trip through every mode
// succeeds with mig left nil, which it could not if any of them dereferenced it.
func TestPreflightResponder_NeverConsultsTheMigDriver(t *testing.T) {
	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			redirectLogicalSliceDirs(t)

			p := &preflighter{
				logger:    klog.Background(),
				mig:       newFakeMigDriver(),
				injection: deviceplugin.DefaultInjectionResolver(injectionConfig),
			}
			responder, restore9, err := p.PreflightResponder(mode)
			require.NoError(t, err)
			defer restore9()
			require.NoError(t, err)

			pod, container, devs, allocated := modeAllocation(mode, "nomig-"+mode.String())

			// A nil mig field inside the server built for this responder would panic on any
			// dereference; reaching here without panicking is the proof that none occurred.
			_, err = responder.GetContainerAllocateResponse(context.Background(), pod, container, devs, allocated)
			require.NoError(t, err)
		})
	}
}

// The reason this manufacturer's seam hands back a restore at all. HAMi-core's cross-process lock
// directory is this package's own variable, not one of the two every manufacturer shares, so the
// shared redirect cannot reach it -- and the sliced responder creates it world-writable. Without
// the redirect below, a pass that reports itself as having changed nothing creates /tmp/vgpulock on
// the node it was inspecting.
func TestPreflightResponder_RedirectsTheHamiLockDirectoryAndPutsItBack(t *testing.T) {
	original := hostVgpuLockPath
	require.Equal(t, "/tmp/vgpulock", original,
		"the real path this test exists to keep a preflight away from")

	injection := deviceplugin.DefaultInjectionResolver(injectionConfig)
	pod, container, devs, allocated := modeAllocation(
		workercore.DeviceAllocationModeSliced, "lockdir")

	p := &preflighter{logger: klog.Background(), mig: newFakeMigDriver(), injection: injection}
	responder, restore, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)

	require.NotEqual(t, original, hostVgpuLockPath, "the lock directory is redirected for the pass")
	redirected := hostVgpuLockPath

	resp, err := responder.GetContainerAllocateResponse(context.Background(), pod, container, devs, allocated)
	require.NoError(t, err)

	// The responder creates the directory, so the proof is where it landed.
	require.DirExists(t, redirected, "the responder did create it -- under the scratch root")
	var mounted bool
	for _, m := range resp.Mounts {
		if m.ContainerPath == ctrVgpuLockPath {
			mounted = true
			assert.Equal(t, redirected, m.HostPath,
				"and the injection mounts the scratch directory, not the host's")
		}
	}
	assert.True(t, mounted, "the sliced injection must carry the lock mount, or this proves nothing")

	restore()

	assert.Equal(t, original, hostVgpuLockPath,
		"the restore puts it back: a redirect left in place would point the rest of this process "+
			"at a directory that no longer exists")
	assert.NoDirExists(t, redirected, "and takes the scratch directory with it")
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
