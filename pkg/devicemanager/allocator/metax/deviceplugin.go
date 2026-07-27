package metax

import (
	"context"
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/controllers"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

const Manufacturer = nodefeature.ManufacturerMetaX

func New(opts device.AllocatorOptions) device.Allocator {
	logger := opts.Logger.WithName(Manufacturer)
	servers := []deviceplugin.Server{
		newServer(logger, workercore.DeviceAllocationModeExclusive),
	}
	if !opts.NoShared {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModeShared),
		)
	}
	if !opts.NoSliced {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModeSliced),
		)
	}
	// The visibility server co-allocates a container to the same physical device(s) its owner
	// container was granted: its Allocate reuses the owner's reserved device and the responder
	// returns the same plain device-visibility response as the non-sliced modes (device-cgroup
	// access only, no slicing artifacts).
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility),
	)

	return aggregated{
		logger:     logger,
		servers:    servers,
		kubeSocket: opts.KubeSocket,
		sliced:     !opts.NoSliced,
	}
}

type aggregated struct {
	logger     klog.Logger
	servers    []deviceplugin.Server
	kubeSocket string
	// sliced reports whether a Sliced server is registered, gating the per-vendor
	// stateful sgpu reclaim loop.
	sliced bool
}

func (aggregated) Name() string {
	return Manufacturer
}

func (in aggregated) Start(ctx context.Context) error {
	in.logger.Info("starting")

	gp := gox.GroupWithContextIn(ctx)
	for i := range in.servers {
		srv := in.servers[i]
		gp.Go(func(ctx context.Context) error {
			return srv.Start(ctx, in.kubeSocket)
		})
	}
	// A sliced pool has no Release callback, so sgpu subdevices are reclaimed by a
	// level-based loop fed the reconciler's broadcast live-pod set plus a resync ticker.
	if in.sliced {
		gp.Go(func(ctx context.Context) error {
			reconciler := controllers.Get[*deviceplugin.DevicesReconciler]()
			r := newReclaimer(newSysfsSGPUManager(), deviceplugin.OperatorPodsDir, in.logger.WithName("reclaim"))
			deviceplugin.RunReclaimLoop(ctx, reconciler, Manufacturer,
				workercore.DeviceAllocationModeSliced, r.reconcile)
			return nil
		})
	}
	return gp.Wait()
}

func (in aggregated) Stop() {
	in.logger.Info("stopping")

	for i := range in.servers {
		srv := in.servers[i]
		srv.Stop()
	}
}

type server struct {
	deviceplugin.ResourceServer

	// sgpu is the sysfs seam the sliced responder drives; nil for non-sliced modes.
	sgpu sgpuManager
}

func newServer(logger klog.Logger, mode workercore.DeviceAllocationMode) deviceplugin.Server {
	logger = logger.WithName(strings.ToLower(mode.String()))

	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logger,
			Manufacturer:   Manufacturer,
			AllocationMode: mode,
			Reconciler:     controllers.Get[*deviceplugin.DevicesReconciler](),
		},
	}
	if mode == workercore.DeviceAllocationModeSliced {
		s.sgpu = newSysfsSGPUManager()
	}
	s.Responder = s

	return s
}

func (s *server) GetContainerAllocateResponse(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	// Sliced containers get real sgpu logical-slicing isolation (a subdevice + METAX_SGPUS);
	// exclusive/shared/visibility keep the plain device-visibility response below.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, devs, allocated)
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{}

	// Mount control devices.
	for _, p := range []string{
		"/dev/mxcd",
		"/dev/mxnd",
		"/dev/mxgd",
	} {
		if pDev := deviceplugin.NewRWDevice(p); pDev != nil {
			ctrResp.Devices = append(ctrResp.Devices, pDev)
		}
	}

	// Mount specified devices.
	for i := range devs.Spec.Groups {
		devGroup := &devs.Spec.Groups[i]
		for j := range devGroup.Accelerators {
			devsAccelerator := &devGroup.Accelerators[j]
			res := deviceplugin.Resource{
				Group:  devGroup.ID,
				Device: devsAccelerator.ID,
			}
			if _, existed := allocated[res]; !existed {
				continue
			}

			if pDev := deviceplugin.NewRWDevicef("/dev/dri/card%d", devsAccelerator.PhysicalIndexes[0]); pDev != nil {
				ctrResp.Devices = append(ctrResp.Devices, pDev)
			}
			if len(ctrResp.Devices) == 1 {
				continue
			}

			if pDev := deviceplugin.NewRWDevicef("/dev/dri/renderD%d", devsAccelerator.PhysicalIndexes[1]); pDev != nil {
				ctrResp.Devices = append(ctrResp.Devices, pDev)
			}
		}
	}

	return ctrResp, nil
}

// getSlicedContainerAllocateResponse renders the sgpu logical-slicing injection for a
// sliced container: it reserves a per-card sgpu subdevice (fixed-share compute quota +
// VRAM cap) via the sysfs seam, writing the correlation + slot marker under the pod
// work dir, and returns METAX_SGPUS plus the control (/dev/mxcd) and per-card render
// (/dev/dri/renderD*) device nodes. A whole-card slice takes the native path (no sgpu
// subdevice, no METAX_SGPUS) but still records an occupancy marker.
//
// MetaX sgpu slicing partitions a single card, and the per-container marker records
// one card, so a multi-card sliced allocation is rejected (the Ascend single-card
// pattern) rather than silently slicing only one.
func (s *server) getSlicedContainerAllocateResponse(
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	var (
		group *workercore.DevicesGroup
		accel *workercore.Accelerator
		count int
	)
	for i := range devs.Spec.Groups {
		g := &devs.Spec.Groups[i]
		for j := range g.Accelerators {
			a := &g.Accelerators[j]
			if _, existed := allocated[deviceplugin.Resource{Group: g.ID, Device: a.ID}]; !existed {
				continue
			}
			count++
			group, accel = g, a
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}
	if count > 1 {
		return nil, fmt.Errorf("sliced container %q allocated %d cards, but MetaX sgpu slicing is single-card", ctr.Name, count)
	}

	// Compute and VRAM are independent dimensions (no single ratio); both come straight
	// from the shared helpers (the percent is used directly, no CU conversion).
	coresPct := deviceplugin.SlicedCoresPercent(ctr,
		nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer))
	memMib, err := deviceplugin.SlicedMemoryMib(ctr,
		nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer),
		nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer),
		int64(group.Memory))
	if err != nil {
		return nil, fmt.Errorf("derive sliced memory limit: %w", err)
	}

	bdf := accel.Topology.PciBusID
	wholeCard := coresPct >= 100 && memMib >= int64(group.Memory)
	res, err := reserveSlice(s.sgpu, string(pod.UID), ctr.Name, bdf, coresPct, memMib, wholeCard)
	if err != nil {
		return nil, fmt.Errorf("reserve metax sgpu slice: %w", err)
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{}
	if pDev := deviceplugin.NewRWDevice("/dev/mxcd"); pDev != nil {
		ctrResp.Devices = append(ctrResp.Devices, pDev)
	}
	if len(accel.PhysicalIndexes) >= 2 {
		if pDev := deviceplugin.NewRWDevicef("/dev/dri/renderD%d", accel.PhysicalIndexes[1]); pDev != nil {
			ctrResp.Devices = append(ctrResp.Devices, pDev)
		}
	}
	// A partial slice injects METAX_SGPUS; a whole card takes the native path (no env).
	if !res.wholeCard {
		ctrResp.Envs = map[string]string{"METAX_SGPUS": res.envValue}
	}
	return ctrResp, nil
}
