package mthreads

import (
	"context"
	"fmt"
	"strconv"
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

const Manufacturer = nodefeature.ManufacturerMThreads

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
	// The visibility server co-allocates the SSH sidecar to the same physical device(s) its
	// workload container (main) was granted: its Allocate reuses main's reserved device and
	// the responder returns the same plain device-visibility response as the non-sliced modes
	// (device-cgroup access only, no slicing artifacts).
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility),
	)

	return aggregated{
		logger:     logger,
		servers:    servers,
		kubeSocket: opts.KubeSocket,
	}
}

type aggregated struct {
	logger     klog.Logger
	servers    []deviceplugin.Server
	kubeSocket string
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
	s.Responder = s

	return s
}

// _AllocatedAccelerator pairs an allocated device with its group; the group carries the
// VRAM that drives the sliced per-card memory cap.
type _AllocatedAccelerator struct {
	group *workercore.DevicesGroup
	accel *workercore.Accelerator
}

func (s *server) GetContainerAllocateResponse(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	// Single pass over the allocated devices in devs order (= MTHREADS_VISIBLE_DEVICES
	// order): collect the visible IDs and the accelerator/group pairs the sliced path
	// needs for the per-card VRAM cap.
	var (
		ids          = make([]string, 0, len(allocated))
		accelerators []_AllocatedAccelerator
	)
	for i := range devs.Spec.Groups {
		devsGroup := &devs.Spec.Groups[i]
		for j := range devsGroup.Accelerators {
			devsAccelerator := &devsGroup.Accelerators[j]
			res := deviceplugin.Resource{
				Group:  devsGroup.ID,
				Device: devsAccelerator.ID,
			}
			if _, existed := allocated[res]; !existed {
				continue
			}
			ids = append(ids, devsAccelerator.ID)
			accelerators = append(accelerators, _AllocatedAccelerator{group: devsGroup, accel: devsAccelerator})
		}
	}

	// Sliced containers get real per-slice QoS isolation (a hard VRAM cap + a relative
	// compute weight); exclusive/shared/visibility keep the plain device-visibility
	// response below.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, ids, accelerators)
	}

	// Delegate to the container runtime for device injection,
	// using the device IDs as the MTHREADS_VISIBLE_DEVICES value.
	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"MTHREADS_VISIBLE_DEVICES": strings.Join(ids, ","),
		},
	}
	return ctrResp, nil
}

// getSlicedContainerAllocateResponse renders the MThreads QoS soft-slicing injection for
// a sliced container: MTHREADS_QOS_MEMORY_LIMIT is a hard per-card VRAM cap (bytes)
// derived from the container's ".sliced.memory-percentage"/".sliced.memory-mib", while
// MTHREADS_QOS_COMPUTING_POWER_WEIGHT is a relative compute weight from
// ".sliced.cores-percentage" (best-effort, not a hard cap). The card stays visible via
// MTHREADS_VISIBLE_DEVICES; the host sGPU kmod + MThreads container runtime enforce the
// QoS at runtime, so no files, mounts, or device nodes are staged.
func (s *server) getSlicedContainerAllocateResponse(
	_ *core.Pod,
	ctr *core.Container,
	ids []string,
	accels []_AllocatedAccelerator,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}

	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	memMibRes := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer)
	// A sliced allocation is single-card, and every accelerator in a DevicesGroup shares the
	// same VRAM (Memory is a group property), so the allocated card's group VRAM is the cap
	// basis.
	memMib, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(accels[0].group.Memory))
	if err != nil {
		return nil, fmt.Errorf("derive sliced memory limit: %w", err)
	}

	return &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"MTHREADS_VISIBLE_DEVICES":            strings.Join(ids, ","),
			"MTHREADS_QOS_MEMORY_LIMIT":           strconv.FormatInt(memMib*1024*1024, 10),
			"MTHREADS_QOS_COMPUTING_POWER_WEIGHT": strconv.Itoa(deviceplugin.SlicedCoresPercent(ctr, coresRes)),
		},
	}, nil
}
