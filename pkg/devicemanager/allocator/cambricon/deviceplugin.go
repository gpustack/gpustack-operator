package cambricon

import (
	"context"
	"fmt"
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

const Manufacturer = nodefeature.ManufacturerCambricon

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

	return &aggregated{
		logger:     logger,
		servers:    servers,
		kubeSocket: opts.KubeSocket,
		sliced:     !opts.NoSliced,
	}
}

type aggregated struct {
	logger     klog.Logger
	servers    []deviceplugin.Server
	kubeSocket string
	// sliced reports whether a Sliced server is registered, gating the per-manufacturer
	// stateful sMLU reclaim loop.
	sliced bool
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
	// A sliced pool has no Release callback, so sMLU instances are reclaimed by a
	// level-based loop fed the reconciler's broadcast live-pod set plus a resync ticker.
	if in.sliced {
		tasks = append(tasks, func(ctx context.Context) error {
			reconciler := controllers.Get[*deviceplugin.DevicesReconciler]()
			r := newReclaimer(newSMLUDriver(), deviceplugin.OperatorPodsDir, in.logger.WithName("reclaim"))
			deviceplugin.RunReclaimLoop(ctx, reconciler, Manufacturer,
				workercore.DeviceAllocationModeSliced, r.reconcile)
			return nil
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

	// smlu is the cnDev seam the sliced responder drives; nil for non-sliced modes.
	smlu smluDriver
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
	if mode == workercore.DeviceAllocationModeSliced {
		s.smlu = newSMLUDriver()
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
	// Sliced containers get real sMLU logical-slicing isolation (a cnDev instance + its device
	// nodes); exclusive/shared/visibility keep the plain device-visibility response below.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, devs, allocated)
	}

	// The allocated accelerators, ordered the way the container numbers them.
	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)

	// Rendered before any node is probed, so an accelerator carrying no cnDev index fails the
	// allocation first. The env is what a deployment running cambricon-container-runtime keys on,
	// and it carries the same index the injected nodes are named with, so a container cannot be
	// handed one card's nodes and pointed at another card by its environment.
	visible, err := visibleDevices(accelerators)
	if err != nil {
		return nil, err
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{
		Envs: map[string]string{
			"CAMBRICON_VISIBLE_DEVICES": visible,
		},
	}
	for i := range accelerators {
		if err = appendCardDevices(ctrResp, accelerators[i].Accel); err != nil {
			return nil, err
		}
	}
	appendNodeDevices(ctrResp)

	return ctrResp, nil
}

// Device-node paths read from the Cambricon device plugin v2.2.0, commit cc0f7735f208, function
// PrepareResponse in device-plugin/pkg/mlu/server.go with the path constants in
// device-plugin/pkg/mlu/const.go. An older or newer Neuware may name them differently.
const (
	cardDevicePathFormat     = "/dev/cambricon_dev%d"
	cardIPCMDevicePathFormat = "/dev/cambricon_ipcm%d"
)

var (
	// optionalCardDevicePathFormats are the per-card nodes only some driver builds expose. Each is
	// a format taking the card's driver index, so every one of them carries a %d. The sliced path
	// injects the IPC node too, from the same format, so a name cannot be corrected in one place
	// and left wrong in the other.
	optionalCardDevicePathFormats = []string{
		"/dev/cambr-msgq:%d",
		"/dev/cambr-rpc:%d",
		"/dev/cmsg_ctrl%d",
		"/dev/commu%d",
		cardIPCMDevicePathFormat,
	}
	// nodeDevicePaths belong to the host rather than to any one card: cnDev enumerates through the
	// control node, and GPUDirect RDMA needs the GDR node. No index names them, and a response
	// carries them once however many cards it holds.
	nodeDevicePaths = []string{
		"/dev/cambricon_ctl",
		"/dev/cambricon_gdr",
	}
)

// cardDevicePath returns the node that is the accelerator itself, named by its driver index.
func cardDevicePath(index uint32) string {
	return fmt.Sprintf(cardDevicePathFormat, index)
}

// cardIPCMDevicePath returns one card's IPC node, named by its driver index. Both the whole-card
// and the sliced response carry it, the whole-card one among the optional nodes.
func cardIPCMDevicePath(index uint32) string {
	return fmt.Sprintf(cardIPCMDevicePathFormat, index)
}

// optionalCardDevicePaths returns one card's optional nodes, named by its driver index.
func optionalCardDevicePaths(index uint32) []string {
	paths := make([]string, 0, len(optionalCardDevicePathFormats))
	for _, format := range optionalCardDevicePathFormats {
		paths = append(paths, fmt.Sprintf(format, index))
	}
	return paths
}

// driverIndex returns the number the driver knows an accelerator by, which for Cambricon is the
// cnDev device index the detector recorded in PhysicalIndexes.
//
// It is that number, not the operator's own logical index, that names the accelerator's char
// devices, that the vendor's own runtime path resolves, and that an operator repairing the sMLU mode
// by hand has to address. The two coincide only while every card on the host was detected, the
// logical index counting the ones that were, so a card failing a probe leaves every later logical
// index below its cnDev index.
//
// A record without the cnDev index is malformed rather than degraded, so it is rejected -- as the
// Ascend responder rejects one missing its dcmi addressing -- instead of guessing an index that
// would name another card.
func driverIndex(accel *workercore.Accelerator) (uint32, error) {
	if len(accel.PhysicalIndexes) == 0 {
		return 0, fmt.Errorf("accelerator %q carries no cnDev device index", accel.ID)
	}
	return accel.PhysicalIndexes[0], nil
}

// visibleDevices renders the CAMBRICON_VISIBLE_DEVICES value: every allocated accelerator's driver
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

// appendCardDevices appends one allocated card's device nodes.
//
// The card's own node is required: a card the host exposes no node for cannot be handed to a
// container at all, so the allocation fails naming it rather than starting a container that has no
// accelerator and no way to say why. The optional nodes are injected where the host has them, which
// is the existence check every device constructor here already makes.
func appendCardDevices(resp *deviceplugin.ContainerAllocateResponse, accel *workercore.Accelerator) error {
	index, err := driverIndex(accel)
	if err != nil {
		return err
	}

	ownPath := cardDevicePath(index)
	pDev := deviceplugin.NewRWDevice(ownPath)
	if pDev == nil {
		return fmt.Errorf("accelerator %q of card %q has no device node %q",
			accel.ID, accel.Topology.PciBusID, ownPath)
	}
	resp.Devices = append(resp.Devices, pDev)

	for _, path := range optionalCardDevicePaths(index) {
		if pDev = deviceplugin.NewRWDevice(path); pDev != nil {
			resp.Devices = append(resp.Devices, pDev)
		}
	}

	return nil
}

// appendNodeDevices appends the node-level control devices, which a response carries once however
// many cards it was allocated.
func appendNodeDevices(resp *deviceplugin.ContainerAllocateResponse) {
	for _, path := range nodeDevicePaths {
		if pDev := deviceplugin.NewRWDevice(path); pDev != nil {
			resp.Devices = append(resp.Devices, pDev)
		}
	}
}

// getSlicedContainerAllocateResponse renders the sMLU logical-slicing injection for a sliced
// container: it reserves a cnDev sMLU instance (a profile with the compute quota + VRAM
// cap, instantiated) via the driver seam, writing the correlation + profile marker under
// the pod work dir, and injects the instance's device node plus the accelerator's control nodes.
// A VIRTUAL_DEVICES env is set as the fallback for --use-runtime deployments (sMLU/mim do
// not support CDI).
//
// An sMLU request is 1 pod / 1 container / 1 accelerator, so a multi-accelerator sliced
// allocation is rejected (the Ascend single-accelerator pattern) rather than silently slicing
// only one.
func (s *server) getSlicedContainerAllocateResponse(
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	var (
		group *workercore.DevicesGroup
		accel *workercore.Accelerator
		count int
	)
	for i := range devs.Spec.Groups {
		g := &devs.Spec.Groups[i]
		for j := range g.Accelerators {
			a := &g.Accelerators[j]
			if _, existed := allocated[deviceplugin.Resource{Group: g.ID, Device: a.ID}]; !existed {
				continue
			}
			count++
			group, accel = g, a
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}
	if count > 1 {
		return nil, fmt.Errorf("sliced container %q allocated %d cards, but Cambricon sMLU slicing is single-card", ctr.Name, count)
	}

	// The card's identity before the request's contents, as the Ascend responder also does, so a
	// malformed record and a malformed request always report the record first.
	slot, err := driverIndex(accel)
	if err != nil {
		return nil, err
	}
	card := accel.Topology.PciBusID

	// Compute and VRAM are independent dimensions; both come straight from the shared
	// helpers (the percent is used directly as the sMLU mluQuota, no CU conversion).
	coresPct := deviceplugin.SlicedCoresPercent(ctr,
		nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer))
	memMib, err := deviceplugin.SlicedMemoryMib(ctr,
		nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer),
		nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer),
		int64(group.Memory))
	if err != nil {
		return nil, fmt.Errorf("derive sliced memory limit: %w", err)
	}

	inst, err := reserveInstance(s.smlu, string(pod.UID), ctr.Name, card, int(slot), coresPct, memMib, s.Logger)
	if err != nil {
		return nil, fmt.Errorf("reserve cambricon smlu instance: %w", err)
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{}
	if pDev := deviceplugin.NewRWDevice(cardDevicePath(slot)); pDev != nil {
		ctrResp.Devices = append(ctrResp.Devices, pDev)
	}
	if pDev := deviceplugin.NewRWDevice(cardIPCMDevicePath(slot)); pDev != nil {
		ctrResp.Devices = append(ctrResp.Devices, pDev)
	}
	if inst.devNode != "" {
		if pDev := deviceplugin.NewRWDevice(inst.devNode); pDev != nil {
			ctrResp.Devices = append(ctrResp.Devices, pDev)
		}
	}
	// A slice needs the node-level control devices for the same reasons a whole card does, and the
	// vendor's own sMLU branch grants them too. The same helper serves both paths, so the two cannot
	// come to disagree about what is node-level.
	appendNodeDevices(ctrResp)
	// The VIRTUAL_DEVICES env is the --use-runtime fallback: it names the sMLU instance's
	// device node for a runtime that maps devices by env rather than by injected node. Set
	// it only when the readback populated a device node — an empty value would misconfigure
	// a runtime that keys on it (the node mount above is guarded the same way).
	if inst.devNode != "" {
		ctrResp.Envs = map[string]string{"VIRTUAL_DEVICES": inst.devNode}
	}
	return ctrResp, nil
}
