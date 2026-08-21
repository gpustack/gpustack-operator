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

// injectionConfig is this manufacturer's vocabulary for the shared device-injection resolver: the
// variable nvidia-container-runtime reads a device list from, and the kind nvidia-ctk publishes whole
// accelerators under. A MIG partition carries a kind of its own, which is why the partitioned family
// never takes the CDI channel.
var injectionConfig = deviceplugin.InjectionConfig{
	Manufacturer:      Manufacturer,
	CDIKind:           "nvidia.com/gpu",
	VisibleDevicesEnv: "NVIDIA_VISIBLE_DEVICES",
}

// defaultInjectionResolver is what a responder built without one uses. One instance rather than one
// per call, so its "which channel was chosen" line is still logged once.
var defaultInjectionResolver = deviceplugin.DefaultInjectionResolver(injectionConfig)

func New(opts device.AllocatorOptions) device.Allocator {
	logger := opts.Logger.WithName(Manufacturer)

	// The hardware-partitioning server serves "<base>.partitioned" — MIG, under NVIDIA's own
	// name for it. A manufacturer with no partition kind has no such resource name at all, so
	// it registers no server rather than one advertising an empty name.
	partitioned := !opts.NoPartitioned &&
		nodefeature.GetAcceleratableResourceName(Manufacturer, workercore.DeviceAllocationModePartitioned) != ""
	// The MIG driver takes an NVML init at construction, so it is built once — only where
	// partitions are served — and shared by the two servers that address them: the partitioned
	// server materializes an instance, the visibility server proves one is still live before
	// naming it again. A node serving no partitioning initializes nothing.
	var mig migDriver
	if partitioned {
		mig = newMigDriver()
	}

	// One resolver for the node: it caches what it reads off the host, and logs the channel it settled
	// on once rather than once per responder. A misconfigured strategy is reported and the node
	// keeps the default, because refusing to start the allocator would take the accelerators with it.
	injection, err := deviceplugin.NewInjectionResolver(injectionConfig)
	if err != nil {
		logger.Error(err, "keeping the default device-injection strategy")
		injection = deviceplugin.DefaultInjectionResolver(injectionConfig)
	}

	servers := []deviceplugin.Server{
		newServer(logger, workercore.DeviceAllocationModeExclusive, nil, injection),
	}
	if !opts.NoShared {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModeShared, nil, injection),
		)
	}
	if !opts.NoSliced {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModeSliced, nil, injection),
		)
	}
	if partitioned {
		// The partitioned responder never consults the resolver: a MIG instance is materialized at
		// Allocate time and no pre-generated CDI specification can name it.
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModePartitioned, mig, nil),
		)
	}
	// The visibility server co-allocates a container to the same physical GPU(s) its owner
	// container was granted; for any non-sliced mode the responder emits only
	// NVIDIA_VISIBLE_DEVICES, which is exactly what a device-cgroup grant needs (no HAMi
	// logical-slicing artifacts). On a partition-backed accelerator that env must name the owner's
	// partition, not the parent accelerator, which is what the shared MIG driver is for.
	// A partition-backed visibility response is rendered by the MIG path, which never consults the
	// resolver; the resolver is here for the plain case, where the co-allocated container is made to
	// see a whole accelerator exactly as its owner does.
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility, mig, injection),
	)

	return &aggregated{
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
	// lifecycle owns the context the tasks below run under, so that stopping this allocator ends
	// every one of them and not only the ones watching a server.
	lifecycle gox.Lifecycle
}

func (*aggregated) Name() string {
	return Manufacturer
}

func (in *aggregated) Start(ctx context.Context) error {
	in.logger.Info("starting")

	tasks := make([]func(context.Context) error, 0, len(in.servers)+1)
	for i := range in.servers {
		srv := in.servers[i]
		tasks = append(tasks, func(ctx context.Context) error {
			return srv.Start(ctx, in.kubeSocket)
		})
	}
	// A device-plugin pool has no Release callback, so MIG GPU/compute instances are reclaimed
	// by a level-based loop fed the reconciler's broadcast live-pod set plus a resync ticker.
	if in.partitioned {
		tasks = append(tasks, func(ctx context.Context) error {
			reconciler := controllers.Get[*deviceplugin.DevicesReconciler]()
			r := newReclaimer(newMigDriver(), deviceplugin.OperatorPodsDir, in.logger.WithName("reclaim"),
				liveClaimsFrom(ctx, reconciler))
			deviceplugin.RunReclaimLoop(ctx, reconciler, Manufacturer,
				workercore.DeviceAllocationModePartitioned, r.reconcile)
			return nil
		})
	}

	return in.lifecycle.Start(ctx, tasks...)
}

// liveClaimsFrom adapts the reconciler's annotation-derived live physical-slice occupancy into
// the reclaimer's per-accelerator-UUID placement view (the Resource's accelerator field is the GPU
// UUID for NVIDIA). It is the attribution self-check source: reclaim never destroys an instance a
// running Pod still claims.
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

// Stop ends every task Start launched and does not return until they have. The servers are not
// walked here: canceling the context they serve under is what retires them, and walking them
// afterwards would report a completed teardown as a server that was never started.
func (in *aggregated) Stop() {
	in.logger.Info("stopping")

	in.lifecycle.Stop()
}

type server struct {
	deviceplugin.ResourceServer

	// mig is the NVML MIG seam: the partitioned responder drives it for a
	// "<base>.partitioned.mig-<profile>" request, and the visibility responder reads it to prove
	// a co-allocated partition is still live. It is nil for every other mode, and on a node that
	// serves no partitioning at all.
	mig migDriver

	// injection decides which channel carries a granted accelerator to the container. One resolver
	// serves every responder on the node. Nil means the default strategy, so a responder built
	// without one behaves as this package did before the strategy existed. The partitioned and
	// partition-backed visibility responders do not consult it at all — see mig.go for why a
	// partition cannot travel over CDI.
	injection *deviceplugin.InjectionResolver
}

// resolver returns this responder's injection resolver, or a default one when it was built without
// any.
func (s *server) resolver() *deviceplugin.InjectionResolver {
	if s.injection == nil {
		return defaultInjectionResolver
	}

	return s.injection
}

func newServer(
	logger klog.Logger,
	mode workercore.DeviceAllocationMode,
	mig migDriver,
	injection *deviceplugin.InjectionResolver,
) deviceplugin.Server {
	logger = logger.WithName(strings.ToLower(mode.String()))

	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logger,
			Manufacturer:   Manufacturer,
			AllocationMode: mode,
			Reconciler:     controllers.Get[*deviceplugin.DevicesReconciler](),
		},
		mig:       mig,
		injection: injection,
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
	// The allocated GPUs in the order the container will number them, which for this vendor is by
	// ascending PCI bus id: NVML enumerates the visible cards that way, and CUDA is held to the
	// same order by the PCI_BUS_ID injected below. That is what each positional
	// CUDA_DEVICE_MEMORY_LIMIT_<i> is read against, so emitting them in any other order caps a
	// card at another card's budget.
	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	ids := deviceplugin.AllocatedAcceleratorIDs(accelerators)

	// Sliced containers get real logical-slicing isolation (HAMi-core preload + quota);
	// exclusive/shared keep the plain device-visibility response below.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, ids, accelerators)
	}

	// Refuse a grant that resolved to nothing, as the sliced path above does. Neither channel below
	// reports it: the environment variable would carry an empty value and the annotation an empty
	// device list, so the response would be a success the container cannot use.
	if len(accelerators) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for container %q", ctr.Name)
	}

	// Name the granted accelerators over whichever channel this node honors: the container runtime's
	// environment variable, or a CDI request the container engine resolves itself. Exactly one of
	// them, never both.
	ctrResp := &deviceplugin.ContainerAllocateResponse{}
	if err := s.resolver().Apply(s.Logger, ctrResp, pod, ids); err != nil {
		return nil, err
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
// and a per-accelerator VRAM limit from its ".sliced.memory-percentage"/".sliced.memory-mib"
// (independent dimensions, no single ratio), plus the mounts that preload libvgpu.so
// and provide the shared lock/cache. The accelerator stays visible via NVIDIA_VISIBLE_DEVICES;
// HAMi-core (preloaded through /etc/ld.so.preload) enforces the limits at runtime.
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

	// Envs: SM (compute) percent from .sliced.cores-percentage and a per-accelerator VRAM
	// limit (MiB) from .sliced.memory-percentage / .sliced.memory-mib (independent
	// dimensions), plus the shared cache.
	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	memMibRes := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer)
	envs := map[string]string{
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

	// The per-accelerator limits above are keyed by position, and HAMi-core fills its limit
	// table from those keys in NVML enumeration order but reads a limit back by the CUDA
	// ordinal of the calling context. Those two numberings coincide only under PCI_BUS_ID:
	// CUDA's default orders by a performance heuristic, which on a node carrying more than
	// one accelerator model can hand a card the budget computed for another. The same
	// invariant governs any integer a workload derives from an NVML index and hands to CUDA,
	// CUDA_VISIBLE_DEVICES included. A workload that declares its own ordering keeps it: the
	// value it sets on its own container reaches CUDA either way, so overwriting it here
	// would hide the choice rather than settle it.
	// Keeping it is not the same as tolerating it once a slice can hold several accelerators. A
	// declared FASTEST_FIRST would then transpose the caps among the accelerators the container
	// holds, and a cap that lands on a SHARED accelerator lets its holder consume past its
	// entitlement and starve the co-tenant — an isolation hole, not merely a mis-served tenant.
	// Admission holds a slice to one accelerator today, and one visible accelerator is ordinal 0
	// under either ordering, so there is nothing to transpose. Whoever lifts that gate has to make
	// the ordering an admission requirement and REFUSE a conflicting declaration: injecting it
	// harder cannot work, because the kubelet appends the container's own environment after this
	// one and the runtime lets the later value win.
	if !deviceplugin.ContainerEnvDeclared(ctr, "CUDA_DEVICE_ORDER") {
		envs["CUDA_DEVICE_ORDER"] = "PCI_BUS_ID"
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
	cudaDir := nvidiaCUDADir(accels[0].Group.RuntimeVersion)
	for i := range accels {
		if d := nvidiaCUDADir(accels[i].Group.RuntimeVersion); d != cudaDir {
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

	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs:   envs,
		Mounts: mounts,
	}
	// The accelerator's visibility is independent of the slicing artifacts above: HAMi-core is
	// preloaded and reads its quota from the environment either way, so a slice works over whichever
	// channel this node honors.
	if err := s.resolver().Apply(s.Logger, ctrResp, pod, ids); err != nil {
		return nil, err
	}

	return ctrResp, nil
}

// nvidiaCUDADir returns the HAMi-core library subdirectory for an accelerator's CUDA runtime
// version ("cuda-<major>"), defaulting to "cuda-12" when the version is unknown.
func nvidiaCUDADir(runtimeVersion string) string {
	return "cuda-" + device.RuntimeMajor(runtimeVersion, "12")
}
