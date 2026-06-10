package ascend

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
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

const Manufacturer = nodefeature.ManufacturerAscend

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
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	indexes := make([]string, 0, len(allocated))
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
			indexes = append(indexes, strconvx.FormatUint(devsAccelerator.Index, 10))
		}
		// TODO: mount HCCL topo file for 910D.
	}

	// Delegate to container runtime for device injection,
	// use indexes as ASCEND_VISIBLE_DEVICES value,
	// do not support Atlas200I for now.
	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"ASCEND_VISIBLE_DEVICES": strings.Join(indexes, ","),
		},
	}
	return ctrResp, nil
}
