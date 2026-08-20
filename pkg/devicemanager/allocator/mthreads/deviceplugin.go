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
	// The visibility server co-allocates a container to the same physical device(s) its owner
	// container was granted: its Allocate reuses the owner's reserved device and the responder
	// returns the same plain device-visibility response as the non-sliced modes (device-cgroup
	// access only, no slicing artifacts).
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility),
	)

	return &aggregated{
		logger:     logger,
		servers:    servers,
		kubeSocket: opts.KubeSocket,
	}
}

type aggregated struct {
	logger     klog.Logger
	servers    []deviceplugin.Server
	kubeSocket string
	// lifecycle owns the context the tasks below run under, so that stopping this allocator ends
	// every one of them and not only the ones watching a server.
	lifecycle gox.Lifecycle
}

func (*aggregated) Name() string {
	return Manufacturer
}

func (in *aggregated) Start(ctx context.Context) error {
	in.logger.Info("starting")

	tasks := make([]func(context.Context) error, 0, len(in.servers))
	for i := range in.servers {
		srv := in.servers[i]
		tasks = append(tasks, func(ctx context.Context) error {
			return srv.Start(ctx, in.kubeSocket)
		})
	}

	return in.lifecycle.Start(ctx, tasks...)
}

// Stop ends every task Start launched and does not return until they have. The servers are not
// walked here: canceling the context they serve under is what retires them, and walking them
// afterwards would report a completed teardown as a server that was never started.
func (in *aggregated) Stop() {
	in.logger.Info("stopping")

	in.lifecycle.Stop()
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

func (s *server) GetContainerAllocateResponse(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	// The allocated devices, ordered the way the container numbers them, and their IDs — the
	// MTHREADS_VISIBLE_DEVICES value.
	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	ids := deviceplugin.AllocatedAcceleratorIDs(accelerators)

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

// getSlicedContainerAllocateResponse renders the MThreads QoS logical-slicing injection for
// a sliced container: MTHREADS_QOS_MEMORY_LIMIT is a hard per-accelerator VRAM cap (bytes)
// derived from the container's ".sliced.memory-percentage"/".sliced.memory-mib", while
// MTHREADS_QOS_COMPUTING_POWER_WEIGHT is a relative compute weight from
// ".sliced.cores-percentage" (best-effort, not a hard cap). The accelerator stays visible via
// MTHREADS_VISIBLE_DEVICES; the host sGPU kmod + MThreads container runtime enforce the
// QoS at runtime, so no files, mounts, or device nodes are staged.
func (s *server) getSlicedContainerAllocateResponse(
	_ *core.Pod,
	ctr *core.Container,
	ids []string,
	accels []deviceplugin.AllocatedAccelerator,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}

	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	memMibRes := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer)
	// A sliced allocation is single-accelerator, and every accelerator in a DevicesGroup shares
	// the same VRAM (Memory is a group property), so the allocated accelerator's group VRAM is the
	// cap basis.
	memMib, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(accels[0].Group.Memory))
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
