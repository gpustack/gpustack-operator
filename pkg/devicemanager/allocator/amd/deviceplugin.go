package amd

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

const Manufacturer = nodefeature.ManufacturerAMD

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

// In-container paths the AMD ROCm logical-slicing shim is mounted at.
//
// ctrVrocmLibPath is one contract with the single line of
// pack/gpustack-operator/rootfs/etc/gpustack/lib/amd/ld.so.preload, which is bind-mounted over the
// container's own preload file: the loader reads the path out of that file, so the two must not
// drift. The tools ride the library's directory rather than PATH, as the Ascend and THead readers do.
const (
	ctrLdPreloadPath  = "/etc/ld.so.preload"
	ctrVrocmLibPath   = "/usr/local/vrocm/libvrocm.so"
	ctrVrocmMonPath   = "/usr/local/vrocm/rocm-monitor"
	ctrVrocmCheckPath = "/usr/local/vrocm/rocm-cumask-check"
	ctrLedgerDir      = "/var/run/vrocm"
	ctrLedgerPath     = ctrLedgerDir + "/ledger"
)

// readTopologyFn is the accelerator-topology reader. It is a var so a test can supply one: the real
// implementation is a cgo seam that exists only on linux, while every decision above it is integer
// arithmetic that must stay testable with no accelerator and no ROCm.
var readTopologyFn = readTopology

// _AllocatedAccelerator pairs a granted accelerator with the group carrying its VRAM figure.
type _AllocatedAccelerator struct {
	group *workercore.DevicesGroup
	accel *workercore.Accelerator
}

// allocatedAccelerators lists the accelerators this container was granted, in the Devices spec's
// own order.
//
// That order is load-bearing rather than incidental: it is the order ROCR_VISIBLE_DEVICES is emitted
// in, and ROCr applies that variable before it enumerates agents — so it is also the index space
// HSA_CU_MASK's GPU_list and VROCM_DEVICE_MEMORY_LIMIT_<i> both live in. Every consumer below walks
// this one slice, because three consumers walking their own order is exactly how the tuple
// misaligns, and a misaligned tuple caps and masks an accelerator the container was never given.
func allocatedAccelerators(
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) []_AllocatedAccelerator {
	accels := make([]_AllocatedAccelerator, 0, len(allocated))
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
			accels = append(accels, _AllocatedAccelerator{group: devGroup, accel: devsAccelerator})
		}
	}
	return accels
}

// acceleratorIDPrefix is what the detector puts in front of an accelerator's ASIC serial to form its
// ID. The ROCm runtime wants the whole thing; the container runtime wants the serial alone.
const acceleratorIDPrefix = "GPU-"

// rocrVisibleDevices is the value ROCR_VISIBLE_DEVICES carries: the accelerator IDs as the detector
// recorded them, which is byte-for-byte the "GPU-<serial>" UUID ROCr matches an agent by.
//
// Its order is load-bearing beyond this variable: HSA_CU_MASK's GPU_list index is a position in this
// list, so every producer below has to walk the same slice in the same order.
func rocrVisibleDevices(accels []_AllocatedAccelerator) string {
	ids := make([]string, 0, len(accels))
	for i := range accels {
		ids = append(ids, accels[i].accel.ID)
	}
	return strings.Join(ids, ",")
}

// runtimeVisibleDevices is the value AMD_VISIBLE_DEVICES carries: the same accelerators, in the same
// order, with the ID prefix stripped.
//
// The two variables cannot share one string, measured: amd-container-runtime accepts "all", "none",
// an index range, or the bare serial, and rejects a "GPU-"prefixed one. It rejects it the worst way
// available — it logs the value as an invalid range, adds no device to the OCI spec, and still
// reports the container configured for accelerator access — so the container starts with no
// /dev/kfd and no render node while every other layer believes it was served. Since this allocator
// returns no DeviceSpec of its own and leaves device injection entirely to the runtime, this
// spelling is the only thing standing between an admitted claim and an accelerator the container can
// actually reach.
//
// The serial rather than the index, deliberately: an index is only meaningful in the enumeration the
// runtime happens to use, and this host's DRM ordering already runs opposite to the vendor tool's.
func runtimeVisibleDevices(accels []_AllocatedAccelerator) string {
	serials := make([]string, 0, len(accels))
	for i := range accels {
		serials = append(serials, strings.TrimPrefix(accels[i].accel.ID, acceleratorIDPrefix))
	}
	return strings.Join(serials, ",")
}

func (s *server) GetContainerAllocateResponse(
	_ context.Context,
	_ *core.Pod,
	_ *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	accels := allocatedAccelerators(devs, allocated)
	// Refuse an accelerator with no identity, for the same reason the sliced path does: this variable
	// is the only filter the container is given, and an empty entry widens it to every accelerator on
	// the node rather than narrowing it to the granted one. Failing the claim visibly is the safe
	// direction — the alternative is a container quietly running against hardware nobody granted it.
	for i := range accels {
		if accels[i].accel.ID == "" {
			return nil, fmt.Errorf("card at index %d reports no unique id, so it cannot be granted",
				accels[i].accel.Index)
		}
	}
	// Delegate to container runtime for device injection, naming the accelerators the way that
	// runtime accepts — see runtimeVisibleDevices for why the spelling is the whole contract here.
	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"AMD_VISIBLE_DEVICES": runtimeVisibleDevices(accels),
		},
	}
	return ctrResp, nil
}

// PlaceLogicalSliced derives this container's CU-mask window on each granted accelerator and places
// it beside the windows the node's live allocations already hold.
//
// It runs under the device-plugin's node allocate mutex, so it does no I/O: the topology read is a
// cached agent-info query and everything else is integer arithmetic. The window it returns is
// published into the reservation before that mutex is released, which is what stops two concurrent
// allocations from being handed the same compute units.
func (s *server) PlaceLogicalSliced(
	_ context.Context,
	_ *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	occupied deviceplugin.Placements,
) (deviceplugin.Placements, error) {
	accels := allocatedAccelerators(devs, allocated)
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}

	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	coresPct := deviceplugin.SlicedCoresPercent(ctr, coresRes)

	placed := make(deviceplugin.Placements, len(accels))
	for i := range accels {
		accel := accels[i].accel
		if accel.ID == "" {
			// The ID is the identity both visible-devices variables carry; an empty one would
			// widen the container to every accelerator on the node rather than narrow it to this one.
			return nil, fmt.Errorf("card at index %d reports no unique id, so it cannot be sliced", accel.Index)
		}
		res := deviceplugin.Resource{Group: accels[i].group.ID, Device: accel.ID}
		topo, err := readTopologyFn(accel.Topology.PciBusID, accel.ID)
		if err != nil {
			return nil, fmt.Errorf("read topology of card %s: %w", accel.ID, err)
		}
		length, err := WindowCUs(topo, coresPct)
		if err != nil {
			return nil, fmt.Errorf("derive the compute window of card %s: %w", accel.ID, err)
		}
		// A request that does not land on the accelerator's allocation atom is aligned DOWN, and the
		// refusal path below it is loud while this one would otherwise be mute. Say it: the tenant
		// is charged for what it asked and served what the hardware can express, and on an accelerator
		// with many shader engines those differ by several points.
		if delivered := length * 100 / topo.CU; delivered != coresPct {
			s.Logger.Info("compute request aligned down to the card's allocation atom",
				"card", accel.ID, "requested", coresPct, "delivered", delivered,
				"atomCUs", topo.Quantum(), "cardCUs", topo.CU)
		}
		placed[res] = []workercore.AcceleratorPlacement{
			PackWindow(topo, length, occupied[res]),
		}
	}
	return placed, nil
}

// GetLogicalSlicedResponse renders the injection for a sliced container: the quota figures the shim
// reads at load, the compute mask ROCr reads at its own initialization, and the mounts that preload
// the library and give its cross-process usage region a writable directory.
//
// The placements are consumed, never recomputed. What the container is told and what the node's
// ledger recorded have to be the same window, and only one of the two can be authoritative.
func (s *server) GetLogicalSlicedResponse(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	placements deviceplugin.Placements,
) (*deviceplugin.ContainerAllocateResponse, error) {
	accels := allocatedAccelerators(devs, allocated)
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}

	// The usage region is per container rather than per node: it is addressed by the accelerator's
	// position in ROCR_VISIBLE_DEVICES, which is container-local, so a shared location would let
	// two containers' index 0 — two different physical accelerators — charge one slot. Under the pod
	// work dir, so the existing per-pod reclaim removes it with the pod. The shim creates the region
	// file itself; this is only its directory, world-writable because the workload's user is not
	// ours to predict.
	ledgerDir := filepath.Join(deviceplugin.PodWorkDir(string(pod.UID), ctr.Name), "run/vrocm")
	if err := osx.MkdirAll(ledgerDir, 0o777); err != nil {
		return nil, fmt.Errorf("create %q: %w", ledgerDir, err)
	}

	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	memMibRes := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer)

	envs := map[string]string{
		// The container runtime reads the first to inject /dev/kfd and the accelerator's render node;
		// the ROCm user-space runtime reads the second to filter and order its agents. Neither is read
		// by the other, and they spell an accelerator differently — bare serial against
		// "GPU-<serial>" — so each gets its own rendering of the same accelerators in the same order.
		"AMD_VISIBLE_DEVICES":  runtimeVisibleDevices(accels),
		"ROCR_VISIBLE_DEVICES": rocrVisibleDevices(accels),
		"VROCM_LEDGER_PATH":    ctrLedgerPath,
	}

	masks := make([]string, 0, len(accels))
	for i := range accels {
		accel := accels[i].accel

		// The MiB figure carries no unit suffix, unlike the NVIDIA branch's "…m": HAMi-core parses
		// a suffix, this shim parses a bare MiB integer.
		memMib, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(accels[i].group.Memory))
		if err != nil {
			return nil, fmt.Errorf("derive sliced memory limit: %w", err)
		}
		envs["VROCM_DEVICE_MEMORY_LIMIT_"+strconv.Itoa(i)] = strconv.FormatInt(memMib, 10)

		res := deviceplugin.Resource{Group: accels[i].group.ID, Device: accel.ID}
		window, ok := placements[res]
		if !ok || len(window) == 0 {
			return nil, fmt.Errorf("no compute window was placed for card %s", accel.ID)
		}
		// The GPU_list index is i, the accelerator's position in the list above — never its physical
		// ordinal, and never its UUID: a GPU_list that is not a decimal index is discarded whole,
		// silently, leaving the container on the entire accelerator.
		masks = append(masks, Mask(i, window[0]))
	}
	// One segment per accelerator, as the runtime's own grammar spells it: GPU_list:CU_list[;...].
	envs["HSA_CU_MASK"] = strings.Join(masks, ";")

	// State the verbosity the slice runs at rather than inheriting it: level 1 reports denials and
	// errors, which is the shim's own default today, and naming it keeps the level a property of
	// the allocation instead of a library default a later shim change could move underneath it. A
	// container that sets LIBVROCM_LOG_LEVEL itself keeps its value — the debugging escape hatch.
	if !deviceplugin.ContainerEnvDeclared(ctr, "LIBVROCM_LOG_LEVEL") {
		envs["LIBVROCM_LOG_LEVEL"] = "1"
	}

	libDir := filepath.Join(deviceplugin.OperatorLibDir, "amd")
	return &deviceplugin.ContainerAllocateResponse{
		Envs: envs,
		Mounts: []*deviceplugin.Mount{
			{ContainerPath: ctrLdPreloadPath, HostPath: filepath.Join(libDir, "ld.so.preload"), ReadOnly: true},
			{ContainerPath: ctrVrocmLibPath, HostPath: filepath.Join(libDir, "libvrocm.so"), ReadOnly: true},
			{ContainerPath: ctrVrocmMonPath, HostPath: filepath.Join(libDir, "rocm-monitor"), ReadOnly: true},
			{ContainerPath: ctrVrocmCheckPath, HostPath: filepath.Join(libDir, "rocm-cumask-check"), ReadOnly: true},
			{ContainerPath: ctrLedgerDir, HostPath: ledgerDir, ReadOnly: false},
		},
	}, nil
}
