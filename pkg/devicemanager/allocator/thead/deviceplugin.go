package thead

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

const Manufacturer = nodefeature.ManufacturerTHead

func New(opts device.AllocatorOptions) device.Allocator {
	logger := opts.Logger.WithName(Manufacturer)

	// The hardware-partitioning server serves "<base>.partitioned" — the vendor calls its own
	// partitioning MIG, so it shares that word. A manufacturer with no partition kind has no such
	// resource name at all, so it registers no server rather than one advertising an empty name.
	partitioned := !opts.NoPartitioned &&
		nodefeature.GetAcceleratableResourceName(Manufacturer, workercore.DeviceAllocationModePartitioned) != ""
	// The partition driver takes a vendor management-library init at construction, so it is built once
	// — only where partitions are served — and shared by the two servers that address them: the
	// partitioned server materializes an instance, the visibility server proves one is still live
	// before naming it again. A node serving no partitioning initializes nothing.
	var mig migDriver
	if partitioned {
		mig = newMigDriver()
	}

	servers := []deviceplugin.Server{
		newServer(logger, workercore.DeviceAllocationModeExclusive, nil),
	}
	if !opts.NoShared {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModeShared, nil),
		)
	}
	if partitioned {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModePartitioned, mig),
		)
	}
	// The visibility server co-allocates a container to the same physical device(s) its owner
	// container was granted: its Allocate reuses the owner's reserved device and the responder
	// returns the same plain device-visibility response as the non-sliced modes (device-cgroup
	// access only, no slicing artifacts). On a partition-backed card that response must name the
	// owner's partition rather than the parent card, which is what the shared driver is for.
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility, mig),
	)

	return aggregated{
		logger:      logger,
		servers:     servers,
		kubeSocket:  opts.KubeSocket,
		partitioned: partitioned,
	}
}

type aggregated struct {
	logger     klog.Logger
	servers    []deviceplugin.Server
	kubeSocket string
	// partitioned reports whether a Partitioned server is registered, gating the per-vendor partition
	// reclaim loop: the loop exists to free the instances that server creates.
	partitioned bool
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
	// A device-plugin pool has no Release callback, so the GPU/compute instances the partitioned
	// server creates are reclaimed by a level-based loop fed the reconciler's broadcast live-pod set
	// plus a resync ticker. The loop takes its own driver instance, mirroring the blueprint rather
	// than sharing the servers' one.
	if in.partitioned {
		gp.Go(func(ctx context.Context) error {
			reconciler := controllers.Get[*deviceplugin.DevicesReconciler]()
			r := newReclaimer(newMigDriver(), deviceplugin.OperatorPodsDir, in.logger.WithName("reclaim"),
				liveClaimsFrom(ctx, reconciler))
			deviceplugin.RunReclaimLoop(ctx, reconciler, Manufacturer,
				workercore.DeviceAllocationModePartitioned, r.reconcile)
			return nil
		})
	}
	return gp.Wait()
}

// liveClaimsFrom adapts the reconciler's annotation-derived live physical-slice occupancy into the
// reclaimer's per-card placement view (the Resource Device field is the card UUID for this vendor). It
// is the attribution self-check source: reclaim never destroys an instance a running Pod still claims.
func liveClaimsFrom(ctx context.Context, reconciler *deviceplugin.DevicesReconciler) func() (map[string][]migPlacement, error) {
	return func() (map[string][]migPlacement, error) {
		occupied, err := reconciler.LivePhysicalOccupied(ctx)
		if err != nil {
			return nil, err
		}
		claims := make(map[string][]migPlacement, len(occupied))
		for res, placements := range occupied {
			for i := range placements {
				claims[res.Device] = append(claims[res.Device],
					migPlacement{Start: placements[i].Start, Length: placements[i].Length})
			}
		}
		return claims, nil
	}
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

	// mig is the vendor partition seam: the partitioned responder drives it for a
	// "<base>.partitioned.mig-<profile>" request, and the visibility responder reads it to prove a
	// co-allocated partition is still live. It is nil for every other mode, and on a node that serves
	// no partitioning at all.
	mig migDriver
}

func newServer(logger klog.Logger, mode workercore.DeviceAllocationMode, mig migDriver) deviceplugin.Server {
	logger = logger.WithName(strings.ToLower(mode.String()))

	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logger,
			Manufacturer:   Manufacturer,
			AllocationMode: mode,
			Reconciler:     controllers.Get[*deviceplugin.DevicesReconciler](),
		},
		mig: mig,
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
	ctrResp := &deviceplugin.ContainerAllocateResponse{}

	// Mount control devices.
	for _, p := range []string{
		"/dev/alixpu",
		"/dev/alixpu_ctl",
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

			if pDev := deviceplugin.NewRWDevicef("/dev/alixpu_ppu%d", devsAccelerator.Index); pDev != nil {
				ctrResp.Devices = append(ctrResp.Devices, pDev)
			}
		}
	}

	return ctrResp, nil
}
