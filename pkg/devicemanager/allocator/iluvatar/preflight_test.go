package iluvatar

import (
	"context"
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
		ID:           "bi150",
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{{ID: testGPUUUID0, Index: 0}},
	}})

	assert.Equal(t, Manufacturer, grp.Manufacturer)
	assert.Empty(t, grp.Checks, "the iluvatar allocator reads no driver, so there is nothing to check")
	assert.NotEmpty(t, grp.Note, "an empty group with no note reads as a node that passed")
	assert.Contains(t, grp.Note, "no driver", "the note must say the allocator reads no driver")
}

// The reason this manufacturer's seam hands back a restore at all. HAMi-core's cross-process lock
// directory is this package's own variable, not one of the two every manufacturer shares, so the
// shared redirect cannot reach it -- and the sliced responder creates it. Without the redirect
// below, a pass that reports itself as having changed nothing creates /tmp/vgpulock on the node it
// was inspecting.
func TestPreflightResponder_RedirectsTheHamiLockDirectoryAndPutsItBack(t *testing.T) {
	original := hostVgpuLockPath
	require.Equal(t, "/tmp/vgpulock", original,
		"the real path this test exists to keep a preflight away from")

	p := &preflighter{logger: klog.Background()}
	responder, restore, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)

	require.NotEqual(t, original, hostVgpuLockPath, "the lock directory is redirected for the pass")
	redirected := hostVgpuLockPath

	pod, ctr := slicedPod("lockdir", "train", 25, 50)
	resp, err := responder.GetContainerAllocateResponse(context.Background(), pod, ctr,
		iluvatarDevices(32768, testGPUUUID0),
		map[deviceplugin.Resource]int32{{Group: "bi150", Device: testGPUUUID0}: 1})
	require.NoError(t, err)

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

// The restore that PreflightResponder returns must put both the shared paths and this
// manufacturer's own lock-directory var back, or a pass after it points at directories that no
// longer exist.
func TestPreflightResponder_RestoreRestoresSharedPathsToo(t *testing.T) {
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
			devs := iluvatarDevices(32768, testGPUUUID0)
			allocated := map[deviceplugin.Resource]int32{{Group: "bi150", Device: testGPUUUID0}: 1}

			p := &preflighter{logger: klog.Background()}
			preSrv, restore, err := p.PreflightResponder(mode)
			require.NoError(t, err)
			defer restore()

			prodSrv, ok := newServer(klog.Background(), mode).(deviceplugin.ContainerAllocateResponder)
			require.True(t, ok)

			prodPod, prodCtr := slicedPod("drift", "train", 25, 50)
			want, err := prodSrv.GetContainerAllocateResponse(context.Background(), prodPod, prodCtr,
				devs, allocated)
			require.NoError(t, err)

			prePod, preCtr := slicedPod("drift", "train", 25, 50)
			got, err := preSrv.GetContainerAllocateResponse(context.Background(), prePod, preCtr,
				devs, allocated)
			require.NoError(t, err)

			// Guarded against passing by comparing two empty responses: every mode emits at least
			// IX_VISIBLE_DEVICES, so an injection carrying no envs is a broken fixture rather than
			// an agreement.
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
