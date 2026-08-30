package iluvatar

import (
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// noDriverNote says, in this manufacturer's own terms, why PreflightAccelerator returns no checks.
// The Iluvatar server (deviceplugin.go) reads no driver for any mode: exclusive/shared/visibility
// hand device visibility to corex's ix-container-runtime by naming IX_VISIBLE_DEVICES, and the
// sliced path (getSlicedContainerAllocateResponse) derives its SM and VRAM limits purely from the
// container's own resource request, then mounts the operator-staged HAMi-core libvgpu.so -- never
// a corex driver call. There is consequently no allocation-time precondition here for a preflight
// to read.
const noDriverNote = "the iluvatar allocator reads no driver at allocation time: exclusive/" +
	"shared/visibility hand device visibility to corex's ix-container-runtime, and the sliced " +
	"path's sm/vram limits are hami-core preload envs derived purely from the container's own " +
	"resource request -- no corex driver call is made to serve an allocation"

// NewPreflighter returns the Iluvatar preflighter. It carries no driver seam because the allocator
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

// PreflightResponder returns the Iluvatar responder for mode, built by the same newServer an
// allocation is served by.
//
// Only GetContainerAllocateResponse is driven. Its sliced path reaches the host through the two
// paths every manufacturer shares -- PodWorkDir under deviceplugin.OperatorPodsDir, and the
// libvgpu.so mount source under deviceplugin.OperatorLibDir -- and, beyond that pair, through
// hostVgpuLockPath, this package's own variable naming HAMi-core's cross-process lock directory
// under the host's real /tmp. Redirecting only the shared pair would leave a pass that calls itself
// read-only creating /tmp/vgpulock on the node it was inspecting, so it is handed to the redirect as
// a private path -- moved and restored alongside the shared pair, and reported through
// deviceplugin.PreflightRehosts so an emitted command names the lock directory a real allocation
// would rather than a scratch one that no longer exists.
func (p *preflighter) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect(&hostVgpuLockPath)
	if err != nil {
		return nil, nil, err
	}

	srv := newServer(p.logger, mode)

	responder, ok := srv.(deviceplugin.ContainerAllocateResponder)
	if !ok {
		restore()
		return nil, nil, fmt.Errorf("iluvatar %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}
