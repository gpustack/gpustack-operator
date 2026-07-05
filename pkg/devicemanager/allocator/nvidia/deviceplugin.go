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
	// The visibility server co-allocates the SSH sidecar to the same physical GPU(s) its
	// workload container was granted; for any non-sliced mode the responder emits only
	// NVIDIA_VISIBLE_DEVICES, which is exactly what the sidecar needs (device-cgroup access,
	// no HAMi soft-slicing artifacts).
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

	// Sliced containers get real soft-slicing isolation (HAMi-core preload + quota);
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

// In-container paths the HAMi-core soft-slicing runtime expects.
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

// getSlicedContainerAllocateResponse renders the HAMi-core soft-slicing injection for
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
