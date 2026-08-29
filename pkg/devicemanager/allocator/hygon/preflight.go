package hygon

import (
	"fmt"
	"time"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// noDriverNote says, in this manufacturer's own terms, why PreflightAccelerator returns no checks.
// The Hygon server (deviceplugin.go) reads no driver for its whole-card modes -- their device set
// is the fixed control-node pair plus a card's own drm nodes, both named from paths this package
// declares. The sliced path (vdev.go, getSlicedContainerAllocateResponse) derives its CU-mask and
// VRAM cap purely from allocateVdev's own on-disk vdev.conf ledger: it scans the pods directory it
// also writes to, never a DCU driver or dcmi call. There is consequently no allocation-time
// precondition here for a preflight to read.
const noDriverNote = "the hygon allocator reads no driver at allocation time: exclusive/shared/" +
	"visibility device sets are host device-node paths declared in this package, and the sliced " +
	"path derives its cu-mask and vram cap from the on-disk vdev.conf ledger it scans and writes " +
	"itself -- no dcu driver or dcmi call is made to serve an allocation"

// NewPreflighter returns the Hygon preflighter. It carries no driver seam because the allocator
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

// PreflightResponder returns the Hygon responder for mode, built by the same newServer an
// allocation is served by.
//
// Only GetContainerAllocateResponse is driven, and that method reaches the host only through the
// two paths every manufacturer shares -- PodWorkDir under deviceplugin.OperatorPodsDir, which the
// sliced path's vdev.conf ledger is written under. Hygon carries no host path of its own beyond
// that pair, so the shared redirect is returned unwrapped: nothing here needs a driver stand-in,
// and nothing needs a restore beyond the one NewPreflightRedirect already returns.
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
		return nil, nil, fmt.Errorf("hygon %s server serves no container allocate response", mode)
	}
	return responder, restore, nil
}
