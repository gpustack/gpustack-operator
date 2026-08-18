package ascend

import (
	"bufio"
	"context"
	"fmt"
	"os"
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
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const Manufacturer = nodefeature.ManufacturerAscend

func New(opts device.AllocatorOptions) device.Allocator {
	logger := opts.Logger.WithName(Manufacturer)
	// Every mode that can put a second container on one accelerator drives the same
	// container-share seam, so it is built once and shared by them. Exclusive gets nil: it owns
	// whole accelerators and must not touch the flag.
	share := newShareDriver(logger)

	servers := []deviceplugin.Server{
		newServer(logger, workercore.DeviceAllocationModeExclusive, nil),
	}
	if !opts.NoShared {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModeShared, share),
		)
	}
	if !opts.NoSliced {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModeSliced, share),
		)
	}
	// The visibility server co-allocates a container to the same physical NPU(s) its owner
	// container was granted; for any non-sliced mode the responder emits only
	// ASCEND_VISIBLE_DEVICES with the exact allocated index(es) — Ascend has no `all`
	// wildcard — which is exactly what a device-cgroup grant needs (no vcann-rt
	// logical-slicing artifacts).
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility, share),
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

	// share is the dcmi container-share seam the responder turns on for the accelerators it hands
	// out. It is nil for exclusive alone, which owns whole accelerators and must not touch the flag.
	share shareDriver
}

func newServer(
	logger klog.Logger,
	mode workercore.DeviceAllocationMode,
	share shareDriver,
) deviceplugin.Server {
	logger = logger.WithName(strings.ToLower(mode.String()))

	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logger,
			Manufacturer:   Manufacturer,
			AllocationMode: mode,
			Reconciler:     controllers.Get[*deviceplugin.DevicesReconciler](),
		},
		share: share,
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
	// The allocated accelerators, ordered the way the container numbers them.
	// TODO: mount HCCL topo file for 950.
	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)

	// Sliced containers get real logical-slicing isolation (vcann-rt preload + quota); every
	// other mode returns the plain device-visibility response below, with only the
	// container-share preflight in between.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, accelerators)
	}

	// Rendered before the preflight below, so an accelerator carrying no dcmi addressing fails
	// the allocation before anything reaches the host.
	visible, err := visibleDevices(accelerators)
	if err != nil {
		return nil, err
	}

	// Shared and visibility put a second container on an accelerator, which the driver refuses
	// unless container-share mode is on, so they need the same preflight a slice does. Unlike a
	// slice they may hold several accelerators, hence the loop. Exclusive is named out on purpose:
	// one container owns the whole accelerator, so there is nothing to permit and no reason to
	// drop the driver's own single-container guard.
	switch s.AllocationMode {
	case workercore.DeviceAllocationModeShared, workercore.DeviceAllocationModeVisibility:
		for i := range accelerators {
			if err := s.ensureShareEnabled(accelerators[i].Accel); err != nil {
				return nil, err
			}
		}
	}

	// Delegate to container runtime for device injection,
	// use the driver indexes as ASCEND_VISIBLE_DEVICES value,
	// do not support Atlas200I for now.
	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"ASCEND_VISIBLE_DEVICES": visible,
		},
	}
	return ctrResp, nil
}

// driverIndex returns the number the driver knows an accelerator by, which for Ascend is the dcmi
// physical id the detector recorded in PhysicalIndexes.
//
// It is that number, not the operator's own logical index, that a vendor runtime resolves:
// ascend-docker-runtime reads each visible device as a physical id and converts it to a logic id
// itself before naming the /dev/davinci node the container gets, and vcann-rt keys its quota config
// by the physical id too, as the vendored dsmi patch under pack/ records from hardware. The two
// coincide only while every accelerator on the host was detected, the logical index counting the
// ones that were, so an accelerator failing a probe leaves every later logical index below its
// physical id.
//
// A record carrying no dcmi addressing is malformed rather than degraded, so it is rejected instead
// of being allocated against a guessed number that would name another accelerator.
func driverIndex(accel *workercore.Accelerator) (uint32, error) {
	if len(accel.PhysicalIndexes) == 0 {
		return 0, fmt.Errorf("accelerator %q carries no dcmi physical index", accel.ID)
	}
	return accel.PhysicalIndexes[0], nil
}

// visibleDevices renders the ASCEND_VISIBLE_DEVICES value: every allocated accelerator's driver
// index, comma-joined in the order the container numbers them.
func visibleDevices(accels []deviceplugin.AllocatedAccelerator) (string, error) {
	indexes := make([]string, 0, len(accels))
	for i := range accels {
		index, err := driverIndex(accels[i].Accel)
		if err != nil {
			return "", err
		}
		indexes = append(indexes, strconvx.FormatUint(index, 10))
	}
	return strings.Join(indexes, ","), nil
}

// In-container paths the vcann-rt logical-slicing runtime expects.
const (
	ctrLdPreloadPath = "/etc/ld.so.preload"
	ctrVruntimePath  = "/opt/enpu/vcann-rt/lib/libvruntime.so"
	ctrMonitorPath   = "/opt/enpu/vcann-rt/tools/enpu-monitor"
	ctrConfigPath    = "/etc/enpu/vcann-rt/npu_info.config"
	ctrDevShmPath    = "/dev/shm"

	// vcann-rt scheduling policies: 1=fixed-share, 2=elastic (default), 3=best-effort.
	vcannSchedulingPolicy = 2
)

// getSlicedContainerAllocateResponse renders the vcann-rt logical-slicing injection for
// a sliced container: a per-container npu_info.config carrying the compute/memory
// quota derived from the container's ".sliced.units" request, plus the mounts that
// preload libvruntime.so and expose the config. The real driver libdcmi/HAL bind at
// runtime; this only stages quota + library mounts.
//
// vcann-rt's npu_info.config models a single physical NPU, so a sliced Ascend
// container maps to one accelerator, and ASCEND_VISIBLE_DEVICES names that one.
func (s *server) getSlicedContainerAllocateResponse(
	pod *core.Pod,
	ctr *core.Container,
	accels []deviceplugin.AllocatedAccelerator,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}
	// vcann-rt's npu_info.config models a single physical NPU, so a sliced Ascend
	// container maps to exactly one accelerator. Reject a multi-accelerator allocation
	// rather than silently quota-isolating only the first accelerator while exposing the rest.
	if len(accels) > 1 {
		return nil, fmt.Errorf("sliced container %q allocated %d accelerators, but vcann-rt logical slicing is single-NPU", ctr.Name, len(accels))
	}

	// vcann-rt is single-NPU; configure the first allocated accelerator.
	group, accel := accels[0].Group, accels[0].Accel
	npuID, err := driverIndex(accel)
	if err != nil {
		return nil, err
	}

	// SM (aicore) and VRAM are independent dimensions (no single ratio).
	coresPct := deviceplugin.SlicedCoresPercent(ctr,
		nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer))
	memMib, err := deviceplugin.SlicedMemoryMib(ctr,
		nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer),
		nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer),
		int64(group.Memory))
	if err != nil {
		return nil, fmt.Errorf("derive sliced memory limit: %w", err)
	}

	// Only once the request itself is known good, since this writes host driver state.
	if err = s.ensureShareEnabled(accel); err != nil {
		return nil, err
	}

	podWorkDir := deviceplugin.PodWorkDir(string(pod.UID), ctr.Name)
	if err = osx.MkdirAll(podWorkDir, 0o777); err != nil {
		return nil, fmt.Errorf("create pod work dir %q: %w", podWorkDir, err)
	}

	configHostPath := filepath.Join(podWorkDir, "etc/enpu/vcann-rt/npu_info.config")
	vnpuID := lowestFreeVNPUID(deviceplugin.OperatorPodsDir, npuID, configHostPath)
	cfg := renderNPUInfoConfig(npuID, vnpuID, coresPct, memMib, accel.ID)
	if err = osx.WriteFile(configHostPath, stringx.ToBytes(&cfg), 0o644); err != nil {
		return nil, fmt.Errorf("write npu_info.config %q: %w", configHostPath, err)
	}

	cannDir := ascendCANNDir(group.RuntimeVersion, group.Family)
	libDir := filepath.Join(deviceplugin.OperatorLibDir, "ascend")
	mounts := []*deviceplugin.Mount{
		{ContainerPath: ctrLdPreloadPath, HostPath: filepath.Join(libDir, "ld.so.preload"), ReadOnly: true},
		{ContainerPath: ctrVruntimePath, HostPath: filepath.Join(libDir, cannDir, "lib/libvruntime.so"), ReadOnly: true},
		{ContainerPath: ctrMonitorPath, HostPath: filepath.Join(libDir, cannDir, "tools/enpu-monitor"), ReadOnly: true},
		{ContainerPath: ctrConfigPath, HostPath: configHostPath, ReadOnly: true},
		{ContainerPath: ctrDevShmPath, HostPath: ctrDevShmPath, ReadOnly: false},
	}

	// Single-NPU, so the visibility env names this one accelerator by its driver index.
	envs := map[string]string{
		"ASCEND_VISIBLE_DEVICES": strconvx.FormatUint(npuID, 10),
	}

	// Quiet vcann-rt's per-call interception logging by default: its ENPU_LOG_LEVEL
	// defaults to 3 (verbose info). A container that sets ENPU_LOG_LEVEL itself keeps its
	// value — the debugging escape hatch — so only inject the quiet default when the
	// workload has not declared one.
	if !deviceplugin.ContainerEnvDeclared(ctr, "ENPU_LOG_LEVEL") {
		envs["ENPU_LOG_LEVEL"] = "1"
	}

	// Show the slice in `npu-smi info` by default: our vendored vcann-rt patch defines
	// dsmi_get_hbm_info, but leaves it off unless ENPU_DSMI_HOOK opts in, so a bare
	// vcann-rt user sees no change. A container that names the variable in its own Env
	// keeps its value — including an opt-out — so only inject the default when it has not.
	// An envFrom-sourced value is invisible here (see ContainerEnvDeclared), so opting out
	// that way needs an explicit Env entry.
	if !deviceplugin.ContainerEnvDeclared(ctr, "ENPU_DSMI_HOOK") {
		envs["ENPU_DSMI_HOOK"] = "1"
	}

	return &deviceplugin.ContainerAllocateResponse{
		Envs:   envs,
		Mounts: mounts,
	}, nil
}

// lowestFreeVNPUID picks the lowest virtual-npu-id not already used by another
// container's config on the same physical NPU (level-based, survives restart). If
// selfConfigPath already exists with a parsable vnpu id, that id is reused so a
// re-allocation is idempotent.
func lowestFreeVNPUID(podsDir string, npuId uint32, selfConfigPath string) int {
	if phy, virt, ok := parseNPUInfoConfig(selfConfigPath); ok && phy == int(npuId) {
		return virt
	}

	used := make(map[int]bool)
	matches, _ := filepath.Glob(filepath.Join(podsDir, "*", "*", "etc/enpu/vcann-rt/npu_info.config"))
	for _, f := range matches {
		if f == selfConfigPath {
			continue
		}
		if phy, virt, ok := parseNPUInfoConfig(f); ok && phy == int(npuId) {
			used[virt] = true
		}
	}
	for i := 0; ; i++ {
		if !used[i] {
			return i
		}
	}
}

// renderNPUInfoConfig builds the vcann-rt npu_info.config body:
//
//   - physical-npu-id is the accelerator's driver index, the number vcann-rt resolves; the
//     operator's own logical index would name another accelerator on a host where any of them
//     failed detection.
//   - aicore-quota is the compute percent (.sliced.cores-percentage).
//   - memory-quota is the per-accelerator VRAM MiB
//     (.sliced.memory-percentage/.sliced.memory-mib).
//   - shm-id is the accelerator ID with spaces replaced by '-' (the hyphen-joined
//     VDie-ID form vcann-rt expects).
//   - scheduling-policy is fixed to elastic (2).
func renderNPUInfoConfig(npuId uint32, vnpuId, aicoreQuota int, memoryQuotaMib int64, acceleratorID string) string {
	shmID := strings.ReplaceAll(acceleratorID, " ", "-")
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "physical-npu-id=%d\n", npuId)
	_, _ = fmt.Fprintf(&b, "virtual-npu-id=%d\n", vnpuId)
	_, _ = fmt.Fprintf(&b, "aicore-quota=%d\n", aicoreQuota)
	_, _ = fmt.Fprintf(&b, "memory-quota=%d\n", memoryQuotaMib)
	_, _ = fmt.Fprintf(&b, "shm-id=%s\n", shmID)
	_, _ = fmt.Fprintf(&b, "scheduling-policy=%d\n", vcannSchedulingPolicy)
	return b.String()
}

// parseNPUInfoConfig reads physical-npu-id and virtual-npu-id from a vcann-rt config.
func parseNPUInfoConfig(path string) (npuId, vnpuId int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = f.Close() }()

	phy, vnpu := -1, -1
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, val, found := strings.Cut(scanner.Text(), "=")
		if !found {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "physical-npu-id":
			phy = n
		case "virtual-npu-id":
			vnpu = n
		}
	}
	if phy < 0 || vnpu < 0 {
		return 0, 0, false
	}
	return phy, vnpu, true
}

// ascendCANNDir returns the vcann-rt library subdirectory for an accelerator's CANN runtime
// version and family ("cann-<major>-<family>"), defaulting the major to "cann-8" when
// the version is unknown. Family is lower-cased (e.g. "910B" -> "910b").
func ascendCANNDir(runtimeVersion, family string) string {
	return "cann-" + device.RuntimeMajor(runtimeVersion, "8") + "-" + strings.ToLower(family)
}
