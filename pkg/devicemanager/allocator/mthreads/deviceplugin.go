package mthreads

import (
	"context"
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

func (s *server) GetContainerAllocateResponse(
	_ context.Context,
	_ *core.Pod,
	_ *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	ids := make([]string, 0, len(allocated))
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
			ids = append(ids, devsAccelerator.ID)
		}
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"MTHREADS_VISIBLE_DEVICES": strings.Join(ids, ","),
		},
	}
	return ctrResp, nil
}
