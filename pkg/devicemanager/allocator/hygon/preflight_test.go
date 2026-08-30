package hygon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// The allocator reads no driver, so a preflight over any set of accelerators carries no checks --
// only the note explaining why, in words. An empty group with no note would read as a node that
// passed, which is the failure this pins against.
func TestPreflightAccelerator_NoChecksCarriesNote(t *testing.T) {
	p := &preflighter{logger: klog.Background()}

	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           groupID,
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{
			{ID: "dcu-uuid-0", Index: 0, PhysicalIndexes: []uint32{0, 128}},
		},
	}})

	assert.Equal(t, Manufacturer, grp.Manufacturer)
	assert.Empty(t, grp.Checks, "the hygon allocator reads no driver, so there is nothing to check")
	assert.NotEmpty(t, grp.Note, "an empty group with no note reads as a node that passed")
	assert.Contains(t, grp.Note, "no driver", "the note must say the allocator reads no driver")
}

// The simulated depth exists to answer what an allocation would inject without becoming one. Its
// whole claim rests on the device being left as it was found: the vdev.conf the sliced path writes
// must land under the scratch root PreflightResponder opened, and nothing must remain once the
// caller's restore runs.
func TestPreflightResponder_WritesOnlyUnderTheRedirect(t *testing.T) {
	origPods := deviceplugin.OperatorPodsDir
	p := &preflighter{logger: klog.Background()}

	responder, restore, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)

	scratchPods := deviceplugin.OperatorPodsDir
	require.NotEqual(t, origPods, scratchPods, "the redirect must be in effect")

	pod, ctr := slicedPod("simulated", "train", 25, 50)
	_, err = responder.GetContainerAllocateResponse(context.Background(), pod, ctr,
		hygonDevicesFixture(), map[deviceplugin.Resource]int32{{Group: groupID, Device: "dcu-uuid-0"}: 1})
	require.NoError(t, err)

	confPath := filepath.Join(deviceplugin.PodWorkDir("simulated", "train"), "etc/vdev/docker", "vdev0.conf")
	require.FileExists(t, confPath, "the vdev.conf must land under the scratch root")

	restore()

	assert.Equal(t, origPods, deviceplugin.OperatorPodsDir, "the restore puts the pods dir back")
	assert.NoDirExists(t, scratchPods, "and takes the scratch tree -- including the conf -- with it")
}

// The restore that PreflightResponder returns must actually restore, or every pass after it points
// at a directory that no longer exists.
func TestPreflightResponder_RestoreRestores(t *testing.T) {
	origLib, origPods := deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir

	p := &preflighter{logger: klog.Background()}
	_, restore, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)

	assert.NotEqual(t, origLib, deviceplugin.OperatorLibDir, "the redirect is in effect")
	assert.NotEqual(t, origPods, deviceplugin.OperatorPodsDir, "the redirect is in effect")

	restore()

	assert.Equal(t, origLib, deviceplugin.OperatorLibDir, "the restore puts the lib dir back")
	assert.Equal(t, origPods, deviceplugin.OperatorPodsDir, "the restore puts the pods dir back")
}

// The anti-drift test. A preflight answer is only worth anything while the injection it reports is
// the one an allocation emits: the same request through a production server and through a preflight
// responder must produce the same injection, differing in nothing.
//
// The seam is entered first, and both responders are driven inside the window it opens, so both
// render under the same host root -- the root a slice's mounts are rendered under is an input to
// the injection, not part of its identity, and comparing two different roots would only measure
// that difference.
func TestPreflightResponder_MatchesTheProductionResponder(t *testing.T) {
	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeSliced,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			// The whole-card modes name the host's real drm nodes, which do not exist on the test
			// host; redirecting them to a fixture tree is what gives their response something to
			// compare, exactly as TestDeviceSet does for the production responder alone.
			redirectDevicePaths(t)

			devs := hygonDevicesFixture()
			allocated := map[deviceplugin.Resource]int32{{Group: groupID, Device: "dcu-uuid-0"}: 1}

			p := &preflighter{logger: klog.Background()}
			preSrv, restore, err := p.PreflightResponder(mode)
			require.NoError(t, err)
			defer restore()

			prodSrv, ok := newServer(klog.Background(), mode, nil).(deviceplugin.ContainerAllocateResponder)
			require.True(t, ok)

			prodPod, prodCtr := slicedPod("drift", "train", 25, 50)
			want, err := prodSrv.GetContainerAllocateResponse(context.Background(), prodPod, prodCtr,
				devs, allocated)
			require.NoError(t, err)

			prePod, preCtr := slicedPod("drift", "train", 25, 50)
			got, err := preSrv.GetContainerAllocateResponse(context.Background(), prePod, preCtr,
				devs, allocated)
			require.NoError(t, err)

			// Guarded against passing by comparing two empty responses: every mode carries the
			// accelerator's own drm devices (redirectDevicePaths gives them somewhere real to
			// resolve to), so a response carrying neither devices nor mounts is a broken fixture
			// rather than an agreement.
			require.True(t, len(want.Devices) > 0 || len(want.Mounts) > 0,
				"the production injection must carry something to compare")
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
