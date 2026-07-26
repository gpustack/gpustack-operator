package nvidia

import (
	"context"
	"fmt"
	"path/filepath"
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
	"gpustack.ai/gpustack/pkg/utils/osx"
)

const Manufacturer = nodefeature.ManufacturerNVIDIA

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
	// The hardware-partitioning server serves "<base>.partitioned" — MIG, under NVIDIA's own
	// name for it. A manufacturer with no partition kind has no such resource name at all, so
	// it registers no server rather than one advertising an empty name.
	partitioned := !opts.NoPartitioned &&
		nodefeature.GetAcceleratableResourceName(Manufacturer, workercore.DeviceAllocationModePartitioned) != ""
	if partitioned {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModePartitioned),
		)
	}
	// The visibility server co-allocates the SSH sidecar to the same physical GPU(s) its
	// workload container was granted; for any non-sliced mode the responder emits only
	// NVIDIA_VISIBLE_DEVICES, which is exactly what the sidecar needs (device-cgroup access,
	// no HAMi logical-slicing artifacts).
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility),
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
	// partitioned reports whether a Partitioned server is registered, gating the per-vendor
	// MIG reclaim loop: the loop exists to free the instances that server creates.
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
	// A device-plugin pool has no Release callback, so MIG GPU/compute instances are reclaimed
	// by a level-based loop fed the reconciler's broadcast live-pod set plus a resync ticker.
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

// liveClaimsFrom adapts the reconciler's annotation-derived live physical-slice occupancy into
// the reclaimer's per-card-UUID placement view (the Resource Device field is the card UUID for
// NVIDIA). It is the attribution self-check source: reclaim never destroys an instance a running
// Pod still claims.
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

	// mig is the NVML MIG actuator seam the partitioned responder drives for a
	// "<base>.partitioned.mig-<profile>" request; nil for every other mode.
	mig migDriver
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
	if mode == workercore.DeviceAllocationModePartitioned {
		s.mig = newMigDriver()
	}
	s.Responder = s

	return s
}

// _AllocatedAccelerator pairs an allocated GPU with its group; the group carries the
// memory + CUDA runtime version that drive the sliced per-card limits and the libvgpu
// subdir.
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
	// Single pass over the allocated GPUs in devs order (= NVIDIA_VISIBLE_DEVICES /
	// per-card CUDA_DEVICE_MEMORY_LIMIT_<i> order): collect the UUIDs and the
	// accelerator/group pairs the sliced path needs for the per-card limits.
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
			accelerators = append(accelerators,
				_AllocatedAccelerator{
					group: devsGroup,
					accel: devsAccelerator,
				},
			)
		}
	}

	// Sliced containers get real logical-slicing isolation (HAMi-core preload + quota);
	// exclusive/shared keep the plain device-visibility response below.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, ids, accelerators)
	}

	// Delegate to container runtime for device injection,
	// use GPU UUID as NVIDIA_VISIBLE_DEVICES value.
	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"NVIDIA_VISIBLE_DEVICES": strings.Join(ids, ","),
		},
	}
	return ctrResp, nil
}

// In-container paths the HAMi-core logical-slicing runtime expects.
const (
	ctrLdPreloadPath   = "/etc/ld.so.preload"
	ctrVgpuLibPath     = "/usr/local/vgpu/libvgpu.so"
	ctrVgpuLockPath    = "/tmp/vgpulock"
	ctrVgpuCacheDir    = "/tmp/vgpu"
	ctrVgpuSharedCache = "/tmp/vgpu/cudevshr.cache"
	ctrDevShmPath      = "/dev/shm"
)

// hostVgpuLockPath is the host directory HAMi-core uses for its cross-process lock,
// shared across containers via the host /tmp mount. It is a var (not a const) so
// tests can redirect it off the real /tmp.
var hostVgpuLockPath = "/tmp/vgpulock"

// getSlicedContainerAllocateResponse renders the HAMi-core logical-slicing injection for
// a sliced container: a compute (SM) limit from the container's ".sliced.cores-percentage"
// and a per-card VRAM limit from its ".sliced.memory-percentage"/".sliced.memory-mib"
// (independent dimensions, no single ratio), plus the mounts that preload libvgpu.so
// and provide the shared lock/cache. The card stays visible via NVIDIA_VISIBLE_DEVICES;
// HAMi-core (preloaded through /etc/ld.so.preload) enforces the limits at runtime.
func (s *server) getSlicedContainerAllocateResponse(
	pod *core.Pod,
	ctr *core.Container,
	ids []string,
	accels []_AllocatedAccelerator,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}

	// Per-container working directories + the shared cross-process lock.
	podWorkDir := deviceplugin.PodWorkDir(string(pod.UID), ctr.Name)
	vgpuCacheDir := filepath.Join(podWorkDir, "tmp/vgpu")
	for _, dir := range []string{hostVgpuLockPath, podWorkDir, vgpuCacheDir} {
		if err := osx.MkdirAll(dir, 0o777); err != nil {
			return nil, fmt.Errorf("create %q: %w", dir, err)
		}
	}

	// Envs: SM (compute) percent from .sliced.cores-percentage and a per-card VRAM
	// limit (MiB) from .sliced.memory-percentage / .sliced.memory-mib (independent
	// dimensions), plus the shared cache.
	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	memMibRes := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer)
	envs := map[string]string{
		"NVIDIA_VISIBLE_DEVICES":          strings.Join(ids, ","),
		"CUDA_DEVICE_SM_LIMIT":            strconv.Itoa(deviceplugin.SlicedCoresPercent(ctr, coresRes)),
		"CUDA_DEVICE_MEMORY_SHARED_CACHE": ctrVgpuSharedCache,
	}
	for i := range accels {
		limit, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(accels[i].group.Memory))
		if err != nil {
			return nil, fmt.Errorf("derive sliced memory limit: %w", err)
		}
		envs["CUDA_DEVICE_MEMORY_LIMIT_"+strconv.Itoa(i)] = strconv.FormatInt(limit, 10) + "m"
	}

	// Quiet HAMi-core's per-call interception logging by default: its LIBCUDA_LOG_LEVEL
	// defaults to 1, which prints [HAMI-core Msg ...] init/cleanup lines on every intercepted
	// call. A container that sets LIBCUDA_LOG_LEVEL itself keeps its value — the debugging
	// escape hatch — so only inject the quiet default when the workload has not declared one.
	if !deviceplugin.ContainerEnvDeclared(ctr, "LIBCUDA_LOG_LEVEL") {
		envs["LIBCUDA_LOG_LEVEL"] = "0"
	}

	// libvgpu.so is mounted at a single fixed container path, so every allocated GPU
	// must share the same CUDA runtime major; reject a mix (e.g. GPUs from groups with
	// different CUDA majors) rather than mounting a libvgpu incompatible with some.
	cudaDir := nvidiaCUDADir(accels[0].group.RuntimeVersion)
	for i := range accels {
		if d := nvidiaCUDADir(accels[i].group.RuntimeVersion); d != cudaDir {
			return nil, fmt.Errorf("sliced container %q spans CUDA majors %s and %s; a single libvgpu cannot serve both", ctr.Name, cudaDir, d)
		}
	}
	libDir := filepath.Join(deviceplugin.OperatorLibDir, "nvidia")
	mounts := []*deviceplugin.Mount{
		{ContainerPath: ctrVgpuLockPath, HostPath: hostVgpuLockPath, ReadOnly: false},
		{ContainerPath: ctrLdPreloadPath, HostPath: filepath.Join(libDir, "ld.so.preload"), ReadOnly: true},
		{ContainerPath: ctrVgpuLibPath, HostPath: filepath.Join(libDir, cudaDir, "libvgpu.so"), ReadOnly: true},
		{ContainerPath: ctrVgpuCacheDir, HostPath: vgpuCacheDir, ReadOnly: false},
		{ContainerPath: ctrDevShmPath, HostPath: ctrDevShmPath, ReadOnly: false},
	}

	return &deviceplugin.ContainerAllocateResponse{
		Envs:   envs,
		Mounts: mounts,
	}, nil
}

// nvidiaCUDADir returns the HAMi-core library subdirectory for a card's CUDA runtime
// version ("cuda-<major>"), defaulting to "cuda-12" when the version is unknown.
func nvidiaCUDADir(runtimeVersion string) string {
	return "cuda-" + device.RuntimeMajor(runtimeVersion, "12")
}
