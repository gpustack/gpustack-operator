package metax

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
)

const Manufacturer = nodefeature.ManufacturerMetaX

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
	// stateful sgpu reclaim loop.
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
	// A sliced pool has no Release callback, so sgpu subdevices are reclaimed by a
	// level-based loop fed the reconciler's broadcast live-pod set plus a resync ticker.
	if in.sliced {
		tasks = append(tasks, func(ctx context.Context) error {
			reconciler := controllers.Get[*deviceplugin.DevicesReconciler]()
			r := newReclaimer(newSysfsSGPUManager(), deviceplugin.OperatorPodsDir, in.logger.WithName("reclaim"))
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

	// sgpu is the sysfs seam the sliced responder drives; nil for non-sliced modes.
	sgpu sgpuManager
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
		s.sgpu = newSysfsSGPUManager()
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
	// Sliced containers get real sgpu logical-slicing isolation (a subdevice + METAX_SGPUS);
	// exclusive/shared/visibility keep the plain device-visibility response below.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, devs, allocated)
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{}

	appendNodeDevices(ctrResp, s.Logger)

	for _, allocatedAccel := range deviceplugin.AllocatedAccelerators(devs, allocated) {
		appendCardDevices(ctrResp, allocatedAccel.Accel, s.Logger)
	}

	return ctrResp, nil
}

// Device-node paths read from this manufacturer's own container documentation. They are vars rather
// than consts so a test can point them at a temporary tree instead of the host's own nodes — the seam
// the AMD, Hygon and THead responders use for the same reason.
var (
	// nodeDevicePaths belong to the host rather than to any one accelerator, so a response carries
	// them once however many accelerators it holds. The sliced path takes only the first of them: an
	// sgpu subdevice is reached through the control node and its own render node, and the vendor's own
	// slicing branch grants no more than that.
	nodeDevicePaths = []string{controlDevicePath, "/dev/mxnd", "/dev/mxgd"}
	// controlDevicePath is the one node-level device the sliced path carries as well: an sgpu
	// subdevice is reached through it and through the subdevice's own render node, and nothing else.
	controlDevicePath = "/dev/mxcd"
	// cardDevicePathFormat and renderDevicePathFormat name an accelerator's own two drm nodes. Each is
	// a format taking that node's drm minor, so both of them carry a %d. The whole-card and the sliced
	// path render from the same two formats, so a name cannot be corrected in one place and left wrong
	// in the other.
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

// appendNodeDevices appends the node-level control devices, which a response carries once however many
// accelerators it was granted.
func appendNodeDevices(resp *deviceplugin.ContainerAllocateResponse, logger klog.Logger) {
	for _, path := range nodeDevicePaths {
		appendNodeDevice(resp, logger, path)
	}
}

// appendNodeDevice appends one node-level device, or says why the response will not carry it.
func appendNodeDevice(resp *deviceplugin.ContainerAllocateResponse, logger klog.Logger, path string) {
	pDev := deviceplugin.NewRWDevice(path)
	if pDev == nil {
		logger.Info("the host exposes no control device node, so the container will not receive it",
			"path", path)

		return
	}
	resp.Devices = append(resp.Devices, pDev)
}

// appendCardDevices appends one allocated accelerator's own drm nodes, where the host has them.
//
// Each recorded index is guarded by its own length. The detector reads them from this manufacturer's
// sysfs drm directory, which yields both numbers, the drm card<N> number alone, or nothing at all — so
// a pair is not something this can assume. Indexing an absent one panics, and no interceptor recovers
// a panic in this handler: the process that serves every allocation on the node dies with it, for
// every manufacturer, over one accelerator whose drm directory could not be read.
//
// The device set here is best-effort, where the AMD responder's is fail-closed: a node this cannot
// name, or that the host does not have, is left out and the allocation still succeeds. This package
// has always behaved that way and no hardware is available to confirm what a container is left able to
// do without those nodes, so the shape is kept — and every omission is said out loud instead. A
// container that reaches its accelerator with less than the full set is then something the log
// explains, rather than something an operator has to infer from the symptom.
func appendCardDevices(
	resp *deviceplugin.ContainerAllocateResponse,
	accel *workercore.Accelerator,
	logger klog.Logger,
) {
	if len(accel.PhysicalIndexes) == 0 {
		logger.Info("an allocated card records no drm index, so none of its device nodes can be named "+
			"and the container will receive none of them",
			"card", accel.ID)

		return
	}
	appendCardDevice(resp, accel, logger, cardDevicePath(accel.PhysicalIndexes[0]))

	if len(accel.PhysicalIndexes) < 2 {
		logger.Info("an allocated card records no drm render index, so its render node cannot be named "+
			"and the container will not receive it",
			"card", accel.ID)

		return
	}
	appendCardDevice(resp, accel, logger, renderDevicePath(accel.PhysicalIndexes[1]))
}

// appendCardDevice appends one node of an allocated accelerator, or says why the response will not
// carry it.
func appendCardDevice(
	resp *deviceplugin.ContainerAllocateResponse,
	accel *workercore.Accelerator,
	logger klog.Logger,
	path string,
) {
	pDev := deviceplugin.NewRWDevice(path)
	if pDev == nil {
		logger.Info("the host has no device node an allocated card names, so the container will not "+
			"receive it",
			"card", accel.ID, "path", path)

		return
	}
	resp.Devices = append(resp.Devices, pDev)
}

// getSlicedContainerAllocateResponse renders the sgpu logical-slicing injection for a
// sliced container: it reserves a per-accelerator sgpu subdevice (fixed-share compute quota +
// VRAM cap) via the sysfs seam, writing the correlation + slot marker under the pod
// work dir, and returns METAX_SGPUS plus the control (/dev/mxcd) and per-accelerator render
// (/dev/dri/renderD*) device nodes. A whole-accelerator slice takes the native path (no sgpu
// subdevice, no METAX_SGPUS) but still records an occupancy marker.
//
// MetaX sgpu slicing partitions a single accelerator, and the per-container marker records
// one accelerator, so a multi-accelerator sliced allocation is rejected (the Ascend
// single-accelerator pattern) rather than silently slicing only one.
func (s *server) getSlicedContainerAllocateResponse(
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	accels := deviceplugin.AllocatedAccelerators(devs, allocated)
	if count := len(accels); count == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	} else if count > 1 {
		return nil, fmt.Errorf("sliced container %q allocated %d cards, but MetaX sgpu slicing is single-card", ctr.Name, count)
	}
	group, accel := accels[0].Group, accels[0].Accel

	// Compute and VRAM are independent dimensions (no single ratio); both come straight
	// from the shared helpers (the percent is used directly, no CU conversion).
	coresPct := deviceplugin.SlicedCoresPercent(ctr,
		nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer))
	memMib, err := deviceplugin.SlicedMemoryMib(ctr,
		nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer),
		nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer),
		int64(group.Memory))
	if err != nil {
		return nil, fmt.Errorf("derive sliced memory limit: %w", err)
	}

	bdf := accel.Topology.PciBusID
	wholeCard := coresPct >= 100 && memMib >= int64(group.Memory)
	res, err := reserveSlice(s.sgpu, string(pod.UID), ctr.Name, bdf, coresPct, memMib, wholeCard)
	if err != nil {
		return nil, fmt.Errorf("reserve metax sgpu slice: %w", err)
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{}
	// The control node and this subdevice's own render node, from the same helpers the whole-card path
	// uses so the two cannot come to name a node differently. An sgpu slice takes no more than these.
	appendNodeDevice(ctrResp, s.Logger, controlDevicePath)
	if len(accel.PhysicalIndexes) < 2 {
		s.Logger.Info("an allocated card records no drm render index, so its render node cannot be named "+
			"and the sliced container will not receive it",
			"card", accel.ID)
	} else {
		appendCardDevice(ctrResp, accel, s.Logger, renderDevicePath(accel.PhysicalIndexes[1]))
	}
	// A partial slice injects METAX_SGPUS; a whole accelerator takes the native path (no env).
	if !res.wholeCard {
		ctrResp.Envs = map[string]string{"METAX_SGPUS": res.envValue}
	}
	return ctrResp, nil
}
