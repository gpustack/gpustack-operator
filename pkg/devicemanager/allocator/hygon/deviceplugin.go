package hygon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	core "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/controllers"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

const Manufacturer = nodefeature.ManufacturerHygon

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
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	// Sliced containers get per-accelerator vdev.conf + CU-mask isolation; exclusive/shared/
	// visibility keep the whole-accelerator passthrough below.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, devs, allocated)
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{}

	// Mount control devices.
	for _, p := range []string{
		"/dev/kfd",
		"/dev/mkfd",
	} {
		if pDev := deviceplugin.NewDevice(p, "rwm"); pDev != nil {
			ctrResp.Devices = append(ctrResp.Devices, pDev)
		}
	}

	// Mount drivers, libraries and tools.
	if pMount := deviceplugin.NewROMount("/opt/hyhal"); pMount != nil {
		ctrResp.Mounts = append(ctrResp.Mounts, pMount)
	}

	// Mount specified devices.
	for _, allocatedAccel := range deviceplugin.AllocatedAccelerators(devs, allocated) {
		devsAccelerator := allocatedAccel.Accel

		// Each recorded index is guarded by its own length, as the sliced path below already
		// does. The detector reads them from this manufacturer's sysfs drm directory, which
		// yields both numbers, the drm card<N> number alone, or nothing at all — so a pair is
		// not something this can assume. Indexing an absent one panics, and no interceptor
		// recovers a panic in this handler: the process that serves every allocation on the node
		// dies with it, for every manufacturer, over one accelerator whose drm directory could
		// not be read.
		if len(devsAccelerator.PhysicalIndexes) > 0 {
			if pDev := deviceplugin.NewRWDevicef("/dev/dri/card%d", devsAccelerator.PhysicalIndexes[0]); pDev != nil {
				ctrResp.Devices = append(ctrResp.Devices, pDev)
			}
		} else {
			s.Logger.Info("no recorded drm index for an allocated card; its device nodes cannot be "+
				"named and are not injected",
				"card", devsAccelerator.ID)
		}
		if len(devsAccelerator.PhysicalIndexes) > 1 {
			if pDev := deviceplugin.NewRWDevicef("/dev/dri/renderD%d", devsAccelerator.PhysicalIndexes[1]); pDev != nil {
				ctrResp.Devices = append(ctrResp.Devices, pDev)
			}
		}
	}

	return ctrResp, nil
}

// In-container paths + host DTK/hyhal runtime dirs the vdev.conf slice needs.
const (
	ctrVdevDir    = "/etc/vdev/docker"
	ctrHygonDrvr  = "/opt/hygondriver"
	hostHygonPath = "/opt/dtk" // HYGONPATH default
	hostHyhalDir  = "/opt/hyhal"
)

// getSlicedContainerAllocateResponse renders the Hygon logical-slicing injection for a sliced
// container: one vdev.conf per allocated accelerator carrying a cores%-derived CU bitmask and
// a per-accelerator VRAM cap, published into the pod work dir and mounted at /etc/vdev/docker/,
// plus the DTK/hyhal runtime dirs and per-accelerator device nodes. The host DTK/hyhal
// user-space runtime reads the vdev.conf to enforce the slice. A whole-accelerator slice still
// writes a full-mask / full-memory vdev.conf occupancy marker (allocateVdev never skips a
// write), so the on-disk scanner never misses a taken accelerator.
func (s *server) getSlicedContainerAllocateResponse(
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	// The allocated DCUs, ordered the way the container numbers them, which is also
	// vdev0.conf, vdev1.conf, ... order.
	accels := deviceplugin.AllocatedAccelerators(devs, allocated)
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}

	coresPct := deviceplugin.SlicedCoresPercent(ctr,
		nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer))
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	memMibRes := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer)

	ctrResp := &deviceplugin.ContainerAllocateResponse{}

	// Control device nodes (the compute path, shared by every allocated accelerator).
	for _, p := range []string{"/dev/kfd", "/dev/mkfd"} {
		if pDev := deviceplugin.NewDevice(p, "rwm"); pDev != nil {
			ctrResp.Devices = append(ctrResp.Devices, pDev)
		}
	}

	// DTK/hyhal user-space runtime dirs (HYGONPATH -> /opt/hygondriver, /opt/hyhal ro).
	if osx.Exists(hostHygonPath) {
		ctrResp.Mounts = append(ctrResp.Mounts,
			&deviceplugin.Mount{ContainerPath: ctrHygonDrvr, HostPath: hostHygonPath, ReadOnly: true})
	}
	if pMount := deviceplugin.NewROMount(hostHyhalDir); pMount != nil {
		ctrResp.Mounts = append(ctrResp.Mounts, pMount)
	}

	// The per-pod vdev dir (holding the vdev<i>.conf files) mounted read-only at
	// /etc/vdev/docker/: the DTK/hyhal runtime only reads it and the operator writes it
	// host-side, so the container needs no write access. These confs are also the
	// authoritative slot ledger allocateVdev scans, so a read-write mount would let a
	// co-tenant tamper with another slice's CU/vdev/pipe accounting. allocateVdev creates
	// the host dir when it writes the first conf.
	vdevHostDir := filepath.Join(deviceplugin.PodWorkDir(string(pod.UID), ctr.Name), "etc/vdev/docker")
	ctrResp.Mounts = append(ctrResp.Mounts,
		&deviceplugin.Mount{ContainerPath: ctrVdevDir, HostPath: vdevHostDir, ReadOnly: true})

	// One vdev.conf per allocated accelerator, each independently slotted; a whole-accelerator
	// slice (cores% >= 100 && memMiB >= accelerator VRAM) resolves to a full-mask / full-memory
	// conf. The loop index i is the container-local device_id (the DTK device_id semantics are a
	// hardware-validation item).
	//
	// This position is the one that OUTLIVES its allocation: it names a file on the host, and
	// allocateVdev reuses a slot only when the file at that path already names the same
	// accelerator. Admission holds a sliced request to exactly one accelerator, so the path is
	// always vdev0.conf and the accelerator it names cannot move. Whoever lifts that gate has to
	// keep an existing path-to-accelerator assignment rather than re-deriving it from the order
	// here: a retry that renumbers finds a sibling conf holding the accelerator it is now placing,
	// counts it as occupied, and either refuses a full slice or grants a second quota beside the
	// first.
	//
	// A partial failure part-way through a multi-accelerator
	// allocation leaves the earlier accelerators' confs on disk: intentional under the
	// level-based design — an idempotent kubelet retry reuses them and podDirGC reclaims them
	// once the pod is gone, so no rollback is attempted.
	for i := range accels {
		group, accel := accels[i].Group, accels[i].Accel
		memMib, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(group.Memory))
		if err != nil {
			return nil, fmt.Errorf("derive sliced memory limit: %w", err)
		}
		selfPath := filepath.Join(vdevHostDir, fmt.Sprintf("vdev%d.conf", i))
		if _, err := allocateVdev(deviceplugin.OperatorPodsDir, selfPath, accel.Topology.PciBusID, group.Cores, coresPct, memMib, i); err != nil {
			return nil, fmt.Errorf("allocate vdev for card %s: %w", accel.Topology.PciBusID, err)
		}

		// Per-accelerator DRM device nodes.
		if len(accel.PhysicalIndexes) > 0 {
			if pDev := deviceplugin.NewRWDevicef("/dev/dri/card%d", accel.PhysicalIndexes[0]); pDev != nil {
				ctrResp.Devices = append(ctrResp.Devices, pDev)
			}
		}
		if len(accel.PhysicalIndexes) > 1 {
			if pDev := deviceplugin.NewRWDevicef("/dev/dri/renderD%d", accel.PhysicalIndexes[1]); pDev != nil {
				ctrResp.Devices = append(ctrResp.Devices, pDev)
			}
		}
	}

	return ctrResp, nil
}
