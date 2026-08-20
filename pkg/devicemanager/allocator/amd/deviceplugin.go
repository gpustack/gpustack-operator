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
	// returns the same plain response as the non-sliced modes — the owner's own device nodes, and
	// none of the slicing artifacts.
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility),
	)

	return &aggregated{
		logger:     logger,
		servers:    servers,
		kubeSocket: opts.KubeSocket,
	}
}

type aggregated struct {
	logger     klog.Logger
	servers    []deviceplugin.Server
	kubeSocket string
	// lifecycle owns the context the tasks below run under, so that stopping this allocator ends
	// every one of them and not only the ones watching a server.
	lifecycle gox.Lifecycle
}

func (*aggregated) Name() string {
	return Manufacturer
}

func (in *aggregated) Start(ctx context.Context) error {
	in.logger.Info("starting")

	tasks := make([]func(context.Context) error, 0, len(in.servers))
	for i := range in.servers {
		srv := in.servers[i]
		tasks = append(tasks, func(ctx context.Context) error {
			return srv.Start(ctx, in.kubeSocket)
		})
	}

	return in.lifecycle.Start(ctx, tasks...)
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

// Device-node paths read from the AMD device plugin, function Allocate in
// internal/pkg/plugin/plugin.go of rocm/k8s-device-plugin.
//
// They are vars rather than consts so a test can point them at a temporary tree instead of the host's
// own nodes — the seam the THead partitioning responder already uses for the same reason. It is what
// lets this package assert the device set a grant CARRIES, not only the refusals: every path here is
// required, so on a machine with no ROCm driver every successful response would otherwise be
// unreachable, taking the sliced compute mask and memory-limit cases down with it.
//
// Every one of them is required, unlike the optional per-card nodes other vendors expose. The shared
// device-spec constructor returns nil for a path that does not exist, and a responder appending only
// what is non-nil would turn a missing node into a SUCCESSFUL allocation carrying a silently
// incomplete set — a container that cannot reach the accelerator it was charged for, with nothing
// anywhere reporting a problem.
var (
	// nodeDevicePaths belong to the host rather than to any one accelerator: /dev/kfd is the compute
	// driver's single entry point, so a response carries it once however many accelerators it holds.
	nodeDevicePaths = []string{"/dev/kfd"}
	// cardDevicePathFormat and renderDevicePathFormat name an accelerator's own two drm nodes. Each is
	// a format taking that node's drm minor, so both of them carry a %d.
	cardDevicePathFormat   = "/dev/dri/card%d"
	renderDevicePathFormat = "/dev/dri/renderD%d"
)

// cardDevicePath returns the drm card node of one accelerator, named by its drm minor.
func cardDevicePath(minor uint32) string {
	return fmt.Sprintf(cardDevicePathFormat, minor)
}

// renderDevicePath returns the drm render node of one accelerator, named by its drm minor.
func renderDevicePath(minor uint32) string {
	return fmt.Sprintf(renderDevicePathFormat, minor)
}

// driverMinors returns the two numbers the driver knows an accelerator by, which for AMD are the drm
// card and render minors the detector recorded in PhysicalIndexes.
//
// Both numbers or neither. The detector reads them from the amdgpu driver's own sysfs tree, which
// yields the pair, the card number alone, or nothing at all — so a pair is not something this can
// assume, and indexing an absent one panics the single process that serves every allocation on the
// node, for every manufacturer. A record short of the pair is malformed rather than degraded, so it
// is rejected instead of named by a guessed minor that would address another accelerator.
func driverMinors(accel *workercore.Accelerator) (card, render uint32, err error) {
	if len(accel.PhysicalIndexes) < 2 {
		return 0, 0, fmt.Errorf("card %s records no drm card and render node, so the device nodes it "+
			"needs cannot be named", accel.ID)
	}

	return accel.PhysicalIndexes[0], accel.PhysicalIndexes[1], nil
}

// appendDeviceNodes appends the device set a grant of these accelerators needs: the node-level
// compute node once, then each accelerator's own drm pair, in the order they were granted.
//
// This is the set the AMD device plugin injects, and it is what lets a ROCm container run with the
// driver alone — no container runtime has to interpret an environment variable for the accelerator to
// arrive.
func appendDeviceNodes(
	resp *deviceplugin.ContainerAllocateResponse,
	accels []deviceplugin.AllocatedAccelerator,
) error {
	if err := appendNodeDevices(resp); err != nil {
		return err
	}

	// Which accelerator claimed each drm node, so a collision can name both sides of the disagreement.
	claimed := make(map[string]*workercore.Accelerator, 2*len(accels))
	for i := range accels {
		if err := appendCardDevices(resp, accels[i].Accel, claimed); err != nil {
			return err
		}
	}

	return nil
}

// appendNodeDevices appends the node-level device nodes, which a response carries once however many
// accelerators it was granted.
func appendNodeDevices(resp *deviceplugin.ContainerAllocateResponse) error {
	for _, path := range nodeDevicePaths {
		pDev := deviceplugin.NewRWDevice(path)
		if pDev == nil {
			return fmt.Errorf("the host has no device node %q, so no accelerator can be granted", path)
		}
		resp.Devices = append(resp.Devices, pDev)
	}

	return nil
}

// appendCardDevices appends one granted accelerator's own device nodes, and records them in claimed so
// a later accelerator naming the same node is refused.
//
// Two accelerators naming one node means the recorded minors do not identify an accelerator, so
// neither grant can be trusted to reach the hardware it was charged for. Measured hardware records one
// card and one render node per bus address, and this has nothing to fire on; a part that enumerates
// differently is refused rather than served a set that may belong to its neighbor.
//
// A refusal, not a silent de-duplication, because the two readings are not distinguishable from here:
// a detector that mis-associated a bus address with the wrong accelerator looks exactly like a part
// that genuinely shares one node between two logical accelerators, and only the first is safe to
// serve. The message names both bus addresses and the sysfs directory to read, so whoever hits it can
// tell which they have.
func appendCardDevices(
	resp *deviceplugin.ContainerAllocateResponse,
	accel *workercore.Accelerator,
	claimed map[string]*workercore.Accelerator,
) error {
	card, render, err := driverMinors(accel)
	if err != nil {
		return err
	}

	for _, path := range []string{cardDevicePath(card), renderDevicePath(render)} {
		if owner, ok := claimed[path]; ok {
			return fmt.Errorf("cards %s and %s both record drm node %q, so neither can be granted; "+
				"compare their /sys/module/amdgpu/drivers/pci:amdgpu/%s/drm and "+
				"/sys/module/amdgpu/drivers/pci:amdgpu/%s/drm listings",
				owner.ID, accel.ID, path, owner.Topology.PciBusID, accel.Topology.PciBusID)
		}
		claimed[path] = accel

		pDev := deviceplugin.NewRWDevice(path)
		if pDev == nil {
			return fmt.Errorf("card %s has no device node %q on the host", accel.ID, path)
		}
		resp.Devices = append(resp.Devices, pDev)
	}

	return nil
}

// runtimeVisibleDevicesNone is what AMD_VISIBLE_DEVICES carries now that the responder injects the
// device nodes itself: an explicit instruction to amd-container-runtime to add nothing.
//
// The two channels do not reconcile, they union — measured. With the runtime installed and the
// variable naming an accelerator the injected nodes do not, the container reaches BOTH: the one it
// was granted and the one the variable named, with no error anywhere. Nothing in this package can
// produce that disagreement today, since the serial and the drm indexes are two fields of one
// detector record, but the variable buys nothing to offset the risk — whatever it names is already
// injected above. Saying "none" removes the second grant channel instead of leaving it live and
// hoping the two never drift.
//
// Stated rather than omitted: the runtime injects nothing when the variable is absent either, but
// that is its default, and a default is not a contract. "none" is.
const runtimeVisibleDevicesNone = "none"

// rocrVisibleDevices is the value ROCR_VISIBLE_DEVICES carries: the accelerator IDs as the detector
// recorded them, which is byte-for-byte the "GPU-<serial>" UUID ROCr matches an agent by.
//
// Its order is load-bearing beyond this variable: ROCr applies it before enumerating agents, so it
// is also the index space HSA_CU_MASK's GPU_list and VROCM_DEVICE_MEMORY_LIMIT_<i> both live in.
// Every producer here walks the one collected slice, because three producers walking their own order
// is exactly how the tuple misaligns — and a misaligned tuple caps and masks an accelerator the
// container was never given. This vendor therefore holds together under any order the collector
// picks; it needs the collector to keep one, not to keep a particular one.
func rocrVisibleDevices(accels []deviceplugin.AllocatedAccelerator) string {
	ids := make([]string, 0, len(accels))
	for i := range accels {
		ids = append(ids, accels[i].Accel.ID)
	}
	return strings.Join(ids, ",")
}

func (s *server) GetContainerAllocateResponse(
	_ context.Context,
	_ *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	accels := deviceplugin.AllocatedAccelerators(devs, allocated)
	// Refuse a grant that resolved to nothing, as the sliced path does. The device set below would
	// still carry the node-level compute node, so without this the response is a success the
	// container cannot use: no accelerator, and no error to say so.
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for container %q", ctr.Name)
	}
	// Refuse an accelerator with no identity, for the same reason the sliced path does: the ID is
	// what ROCr matches an agent by, and an empty entry does not narrow the container to the granted
	// accelerator. Failing the claim visibly is the safe direction — the alternative is a container
	// quietly running against hardware nobody granted it.
	for i := range accels {
		if accels[i].Accel.ID == "" {
			return nil, fmt.Errorf("card at index %d reports no unique id, so it cannot be granted",
				accels[i].Accel.Index)
		}
	}
	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"AMD_VISIBLE_DEVICES": runtimeVisibleDevicesNone,
		},
	}
	// Inject the accelerator's own device nodes, so the grant does not depend on a container runtime
	// being installed to interpret anything.
	if err := appendDeviceNodes(ctrResp, accels); err != nil {
		return nil, err
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
	accels := deviceplugin.AllocatedAccelerators(devs, allocated)
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}

	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	coresPct := deviceplugin.SlicedCoresPercent(ctr, coresRes)

	placed := make(deviceplugin.Placements, len(accels))
	for i := range accels {
		accel := accels[i].Accel
		if accel.ID == "" {
			// The ID is the identity both visible-devices variables carry; an empty one would
			// widen the container to every accelerator on the node rather than narrow it to this one.
			return nil, fmt.Errorf("card at index %d reports no unique id, so it cannot be sliced", accel.Index)
		}
		res := deviceplugin.Resource{Group: accels[i].Group.ID, Device: accel.ID}
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
	accels := deviceplugin.AllocatedAccelerators(devs, allocated)
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}

	// A slice reaches its accelerator through the same device nodes a whole one does; what makes it a
	// slice is the quota and the compute mask below, not a narrower device set. The same helper serves
	// both paths, so the two cannot come to disagree about what a grant carries. Resolved before the
	// directory below, so a refusal leaves nothing behind on the host to clean up.
	ctrResp := &deviceplugin.ContainerAllocateResponse{}
	if err := appendDeviceNodes(ctrResp, accels); err != nil {
		return nil, err
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
		// The ROCm user-space runtime reads ROCR_VISIBLE_DEVICES to filter and order its agents, and
		// it must name exactly the accelerators whose nodes were injected above: an entry ROCr cannot
		// resolve to a visible agent does not drop that entry, it yields ZERO GPU agents, measured and
		// silent. Both are rendered from the one collected slice, which is what keeps them equal.
		"AMD_VISIBLE_DEVICES":  runtimeVisibleDevicesNone,
		"ROCR_VISIBLE_DEVICES": rocrVisibleDevices(accels),
		"VROCM_LEDGER_PATH":    ctrLedgerPath,
	}

	masks := make([]string, 0, len(accels))
	for i := range accels {
		accel := accels[i].Accel

		// The MiB figure carries no unit suffix, unlike the NVIDIA branch's "…m": HAMi-core parses
		// a suffix, this shim parses a bare MiB integer.
		memMib, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(accels[i].Group.Memory))
		if err != nil {
			return nil, fmt.Errorf("derive sliced memory limit: %w", err)
		}
		envs["VROCM_DEVICE_MEMORY_LIMIT_"+strconv.Itoa(i)] = strconv.FormatInt(memMib, 10)

		res := deviceplugin.Resource{Group: accels[i].Group.ID, Device: accel.ID}
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
	ctrResp.Envs = envs
	ctrResp.Mounts = []*deviceplugin.Mount{
		{ContainerPath: ctrLdPreloadPath, HostPath: filepath.Join(libDir, "ld.so.preload"), ReadOnly: true},
		{ContainerPath: ctrVrocmLibPath, HostPath: filepath.Join(libDir, "libvrocm.so"), ReadOnly: true},
		{ContainerPath: ctrVrocmMonPath, HostPath: filepath.Join(libDir, "rocm-monitor"), ReadOnly: true},
		{ContainerPath: ctrVrocmCheckPath, HostPath: filepath.Join(libDir, "rocm-cumask-check"), ReadOnly: true},
		{ContainerPath: ctrLedgerDir, HostPath: ledgerDir, ReadOnly: false},
	}

	return ctrResp, nil
}
