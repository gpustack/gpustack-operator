package mthreads

import (
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// noDriverNote says, in this manufacturer's own terms, why PreflightAccelerator returns no checks.
// The MThreads server (deviceplugin.go) reads no driver for any mode: every response is env vars
// only -- MTHREADS_VISIBLE_DEVICES for exclusive/shared/visibility, plus a QoS memory cap and
// compute weight for a sliced container, both derived purely from that container's own resource
// request in getSlicedContainerAllocateResponse. The host sGPU kmod and MThreads container runtime
// enforce them; no files, mounts, or device nodes are staged and no driver call is made. There is
// consequently no allocation-time precondition here for a preflight to read.
const noDriverNote = "the mthreads allocator reads no driver at allocation time: every mode's " +
	"response is env vars only -- mthreads_visible_devices plus, for a sliced container, a qos " +
	"memory cap and compute weight derived purely from the container's own resource request -- " +
	"the host sgpu kmod and mthreads container runtime enforce them, and no driver call is made " +
	"to serve an allocation"

// NewPreflighter returns the MThreads preflighter. It carries no driver seam because the allocator
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

// PreflightResponder returns the MThreads responder for mode, built by the same newServer an
// allocation is served by.
//
// Only GetContainerAllocateResponse is driven, and that method writes nothing to the host in any
// mode -- its whole response, sliced included, is env vars derived from the request it was given.
// It reaches neither of the two shared host paths nor any path of its own, so there is no host
// write for a caller to redirect: the server newServer returns is already exactly what a simulated
// pass needs. The shared redirect is still opened, for the same defense-in-depth reason AMD's does
// despite the same absence of a write: a future change that did add one would land inside a window
// this seam already opens, rather than reaching the host silently.
func (p *preflighter) PreflightResponder(
	mode workercore.DeviceAllocationMode,
) (deviceplugin.ContainerAllocateResponder, func(), error) {
	_, restore, err := deviceplugin.NewPreflightRedirect()
	if err != nil {
		return nil, nil, err
	}

	srv := newServer(p.logger, mode)

	responder, ok := srv.(deviceplugin.ContainerAllocateResponder)
	if !ok {
		restore()
		return nil, nil, fmt.Errorf("mthreads %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}
