package cambricon

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
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

const Manufacturer = nodefeature.ManufacturerCambricon

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
	// sliced reports whether a Sliced server is registered, gating the per-manufacturer
	// stateful sMLU reclaim loop.
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
	// A sliced pool has no Release callback, so sMLU instances are reclaimed by a
	// level-based loop fed the reconciler's broadcast live-pod set plus a resync ticker.
	if in.sliced {
		gp.Go(func(ctx context.Context) error {
			reconciler := controllers.Get[*deviceplugin.DevicesReconciler]()
			r := newReclaimer(newSMLUDriver(), deviceplugin.OperatorPodsDir, in.logger.WithName("reclaim"))
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

	// smlu is the cnDev seam the sliced responder drives; nil for non-sliced modes.
	smlu smluDriver
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
		s.smlu = newSMLUDriver()
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
	// Sliced containers get real sMLU logical-slicing isolation (a cnDev instance + its device
	// nodes); exclusive/shared/visibility keep the plain device-visibility response below.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, devs, allocated)
	}

	// The allocated accelerators, ordered the way the container numbers them, as the driver
	// indexes CAMBRICON_VISIBLE_DEVICES carries.
	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	indexes := make([]string, 0, len(accelerators))
	for i := range accelerators {
		indexes = append(indexes, strconvx.FormatUint(accelerators[i].Accel.Index, 10))
	}

	// Delegate to container runtime for device injection,
	// use indexes as CAMBRICON_VISIBLE_DEVICES value.
	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"CAMBRICON_VISIBLE_DEVICES": strings.Join(indexes, ","),
		},
	}
	return ctrResp, nil
}

// getSlicedContainerAllocateResponse renders the sMLU logical-slicing injection for a sliced
// container: it reserves a cnDev sMLU instance (a profile with the compute quota + VRAM
// cap, instantiated) via the driver seam, writing the correlation + profile marker under
// the pod work dir, and injects the instance's device node plus the accelerator's control nodes.
// A VIRTUAL_DEVICES env is set as the fallback for --use-runtime deployments (sMLU/mim do
// not support CDI).
//
// An sMLU request is 1 pod / 1 container / 1 accelerator, so a multi-accelerator sliced
// allocation is rejected (the Ascend single-accelerator pattern) rather than silently slicing
// only one.
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
		return nil, fmt.Errorf("sliced container %q allocated %d cards, but Cambricon sMLU slicing is single-card", ctr.Name, count)
	}

	// Compute and VRAM are independent dimensions; both come straight from the shared
	// helpers (the percent is used directly as the sMLU mluQuota, no CU conversion).
	coresPct := deviceplugin.SlicedCoresPercent(ctr,
		nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer))
	memMib, err := deviceplugin.SlicedMemoryMib(ctr,
		nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer),
		nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer),
		int64(group.Memory))
	if err != nil {
		return nil, fmt.Errorf("derive sliced memory limit: %w", err)
	}

	// The Cambricon detector records the cnDev device index in PhysicalIndexes, and it names both
	// the accelerator's char devices and the card an operator repairing the sMLU mode by hand has to
	// address. A record without it is malformed rather than degraded, so it is rejected here — as
	// the Ascend responder rejects one missing its dcmi addressing — instead of guessing an index
	// that would send the operator to another card.
	if len(accel.PhysicalIndexes) == 0 {
		return nil, fmt.Errorf("accelerator %q carries no cnDev device index", accel.ID)
	}
	card, slot := accel.Topology.PciBusID, accel.PhysicalIndexes[0]

	inst, err := reserveInstance(s.smlu, string(pod.UID), ctr.Name, card, int(slot), coresPct, memMib, s.Logger)
	if err != nil {
		return nil, fmt.Errorf("reserve cambricon smlu instance: %w", err)
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{}
	if pDev := deviceplugin.NewRWDevicef("/dev/cambricon_dev%d", slot); pDev != nil {
		ctrResp.Devices = append(ctrResp.Devices, pDev)
	}
	if pDev := deviceplugin.NewRWDevicef("/dev/cambricon_ipcm%d", slot); pDev != nil {
		ctrResp.Devices = append(ctrResp.Devices, pDev)
	}
	if inst.devNode != "" {
		if pDev := deviceplugin.NewRWDevice(inst.devNode); pDev != nil {
			ctrResp.Devices = append(ctrResp.Devices, pDev)
		}
	}
	// The VIRTUAL_DEVICES env is the --use-runtime fallback: it names the sMLU instance's
	// device node for a runtime that maps devices by env rather than by injected node. Set
	// it only when the readback populated a device node — an empty value would misconfigure
	// a runtime that keys on it (the node mount above is guarded the same way).
	if inst.devNode != "" {
		ctrResp.Envs = map[string]string{"VIRTUAL_DEVICES": inst.devNode}
	}
	return ctrResp, nil
}
