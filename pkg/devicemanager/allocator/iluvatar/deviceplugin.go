package iluvatar

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

const Manufacturer = nodefeature.ManufacturerIluvatar

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
	// The sliced server serves "<base>.sliced": a share of one accelerator, enforced inside the
	// container by the HAMi-core preload pair this operator's image stages, not by the driver.
	// corex's ix-container-runtime still injects the accelerator, so the slice needs no partition
	// driver.
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
	// The allocated accelerators, ordered the way the container numbers them, and their UUIDs —
	// the IX_VISIBLE_DEVICES value. A sliced request here is single-accelerator, so the order
	// carries nothing extra for the memory limit; it is the same collection either way.
	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	ids := deviceplugin.AllocatedAcceleratorIDs(accelerators)

	// Sliced containers get real logical-slicing isolation (HAMi-core preload + quota);
	// exclusive/shared/visibility keep the plain device-visibility response below.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, ids, accelerators)
	}

	// Delegate to container runtime for device injection,
	// use GPU UUID as IX_VISIBLE_DEVICES value.
	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"IX_VISIBLE_DEVICES": strings.Join(ids, ","),
		},
	}
	return ctrResp, nil
}

// In-container paths the HAMi-core logical-slicing runtime expects. libvgpu.so sits where the
// iluvatar ld.so.preload asset names it, so that file and these constants are one contract.
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

// getSlicedContainerAllocateResponse renders the HAMi-core logical-slicing injection for a sliced
// container: a compute (SM) limit from the container's ".sliced.cores-percentage" and a
// per-accelerator VRAM limit from its ".sliced.memory-percentage"/".sliced.memory-mib"
// (independent dimensions, no single ratio), plus the mounts that preload libvgpu.so and provide
// the shared lock/cache. The accelerator stays visible via IX_VISIBLE_DEVICES (corex's
// ix-container-runtime injects it); HAMi-core, preloaded through /etc/ld.so.preload, enforces the
// limits at runtime. corex presents a single CUDA-compatible driver level, so one
// operator-staged HAMi-core libvgpu serves every accelerator — no per-accelerator CUDA-major
// reconciliation as in the NVIDIA branch.
func (s *server) getSlicedContainerAllocateResponse(
	pod *core.Pod,
	ctr *core.Container,
	ids []string,
	accels []deviceplugin.AllocatedAccelerator,
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

	// Envs: SM (compute) percent from .sliced.cores-percentage and a per-accelerator VRAM limit
	// (MiB) from .sliced.memory-percentage / .sliced.memory-mib (independent dimensions), plus the
	// shared cache. Admission pins a logical slice to a single accelerator, so the loop runs once and
	// emits CUDA_DEVICE_MEMORY_LIMIT_0; it is written for several anyway, exactly as the NVIDIA
	// branch's is.
	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	memMibRes := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer)
	envs := map[string]string{
		"IX_VISIBLE_DEVICES":              strings.Join(ids, ","),
		"CUDA_DEVICE_SM_LIMIT":            strconv.Itoa(deviceplugin.SlicedCoresPercent(ctr, coresRes)),
		"CUDA_DEVICE_MEMORY_SHARED_CACHE": ctrVgpuSharedCache,
	}
	for i := range accels {
		limit, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(accels[i].Group.Memory))
		if err != nil {
			return nil, fmt.Errorf("derive sliced memory limit: %w", err)
		}
		envs["CUDA_DEVICE_MEMORY_LIMIT_"+strconv.Itoa(i)] = strconv.FormatInt(limit, 10) + "m"
	}

	// Quiet HAMi-core's per-call interception logging by default: its LIBCUDA_LOG_LEVEL defaults to
	// 1, which prints [HAMI-core Msg ...] lines on every intercepted call. A container that sets
	// LIBCUDA_LOG_LEVEL itself keeps its value — the debugging escape hatch — so only inject the
	// quiet default when the workload has not declared one.
	if !deviceplugin.ContainerEnvDeclared(ctr, "LIBCUDA_LOG_LEVEL") {
		envs["LIBCUDA_LOG_LEVEL"] = "0"
	}

	libDir := filepath.Join(deviceplugin.OperatorLibDir, Manufacturer)
	mounts := []*deviceplugin.Mount{
		{ContainerPath: ctrVgpuLockPath, HostPath: hostVgpuLockPath, ReadOnly: false},
		{ContainerPath: ctrLdPreloadPath, HostPath: filepath.Join(libDir, "ld.so.preload"), ReadOnly: true},
		{ContainerPath: ctrVgpuLibPath, HostPath: filepath.Join(libDir, "libvgpu.so"), ReadOnly: true},
		{ContainerPath: ctrVgpuCacheDir, HostPath: vgpuCacheDir, ReadOnly: false},
		{ContainerPath: ctrDevShmPath, HostPath: ctrDevShmPath, ReadOnly: false},
	}

	return &deviceplugin.ContainerAllocateResponse{
		Envs:   envs,
		Mounts: mounts,
	}, nil
}
