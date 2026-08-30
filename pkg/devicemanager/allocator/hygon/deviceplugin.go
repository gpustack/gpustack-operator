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

	// The hardware-partitioning server serves "<base>.partitioned" -- this vendor calls its own
	// partitioning MIG, in its API and its tooling alike, so it shares that word. A manufacturer with
	// no partition kind has no such resource name at all, so it registers no server rather than one
	// advertising an empty name.
	partitioned := !opts.NoPartitioned &&
		nodefeature.GetAcceleratableResourceName(Manufacturer, workercore.DeviceAllocationModePartitioned) != ""
	// The partition driver takes a vendor library load at construction, so it is built once -- only
	// where partitions are served -- and shared by the two servers that address them: the partitioned
	// server materializes an instance, the visibility server proves one is still live before naming it
	// again. A node serving no partitioning loads nothing.
	var mig migDriver
	if partitioned {
		mig = newMigDriver()
	}

	servers := []deviceplugin.Server{
		newServer(logger, workercore.DeviceAllocationModeExclusive, nil),
	}
	if !opts.NoShared {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModeShared, nil),
		)
	}
	if !opts.NoSliced {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModeSliced, nil),
		)
	}
	if partitioned {
		servers = append(servers,
			newServer(logger, workercore.DeviceAllocationModePartitioned, mig),
		)
	}
	// The visibility server co-allocates a container to the same physical device(s) its owner
	// container was granted: its Allocate reuses the owner's reserved device and the responder
	// returns the same plain device-visibility response as the non-sliced modes (device-cgroup
	// access only, no slicing artifacts).
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility, mig),
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
	// partitioned reports whether a Partitioned server is registered, gating the partition reclaim
	// loop: the loop exists to free the instances that server creates, and nothing else on this node
	// can free them -- the device-plugin protocol has no release callback, and this vendor cannot
	// leave Multi-Instance mode while any instance survives.
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
	if in.partitioned {
		tasks = append(tasks, func(ctx context.Context) error {
			r := newMigReclaimer(newMigDriver(), deviceplugin.OperatorPodsDir, in.logger.WithName("reclaim"))
			deviceplugin.RunReclaimLoop(ctx, controllers.Get[*deviceplugin.DevicesReconciler](),
				Manufacturer, workercore.DeviceAllocationModePartitioned, r.reconcile)
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
	// mig is the partition driver, set only on the two servers that address a partition: the
	// partitioned server and the visibility server that names an owner's partition again. It is nil
	// everywhere else, and the partition entry points refuse rather than proceed when it is.
	mig migDriver
}

func newServer(
	logger klog.Logger, mode workercore.DeviceAllocationMode, mig migDriver,
) deviceplugin.Server {
	logger = logger.WithName(strings.ToLower(mode.String()))

	s := &server{
		mig: mig,
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

	// Refuse a grant that resolved to nothing, as the sliced path below does. The response would still
	// carry the node-level control devices and the runtime mount, so without this it is a success the
	// container cannot use: no accelerator, and no error to say so.
	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	if len(accelerators) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for container %q", ctr.Name)
	}

	ctrResp := &deviceplugin.ContainerAllocateResponse{}

	appendNodeDevices(ctrResp, s.Logger)

	// Mount drivers, libraries and tools.
	if pMount := deviceplugin.NewROMount(hostHyhalDir); pMount != nil {
		ctrResp.Mounts = append(ctrResp.Mounts, pMount)
	}

	for i := range accelerators {
		appendCardDevices(ctrResp, accelerators[i].Accel, s.Logger)
	}

	return ctrResp, nil
}

// Device-node paths read from this manufacturer's own container documentation, which injects the two
// control nodes plus each accelerator's drm pair. They are vars rather than consts so a test can point
// them at a temporary tree instead of the host's own nodes — the seam the AMD and THead responders use
// for the same reason, and what lets this package assert the device set a grant carries rather than
// only the paths it does not name.
var (
	// nodeDevicePaths belong to the host rather than to any one accelerator: they are the compute
	// path every allocated accelerator shares, so a response carries them once however many it holds.
	nodeDevicePaths = []string{"/dev/kfd", "/dev/mkfd"}
	// cardDevicePathFormat and renderDevicePathFormat name an accelerator's own two drm nodes. Each is
	// a format taking that node's drm minor, so both of them carry a %d. The whole-card and the sliced
	// path render from the same two formats, so a name cannot be corrected in one place and left wrong
	// in the other.
	cardDevicePathFormat   = "/dev/dri/card%d"
	renderDevicePathFormat = "/dev/dri/renderD%d"
)

// nodeDevicePermissions is what this manufacturer's own documentation grants the control nodes: "rwm"
// rather than the "rw" an accelerator's own nodes take.
const nodeDevicePermissions = "rwm"

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
	pDev := deviceplugin.NewDevice(path, nodeDevicePermissions)
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

	// Control device nodes (the compute path, shared by every allocated accelerator), from the same
	// helper the whole-card path uses so the two sets cannot drift apart.
	appendNodeDevices(ctrResp, s.Logger)

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

		// Per-accelerator DRM device nodes, from the same helper the whole-card path uses.
		appendCardDevices(ctrResp, accel, s.Logger)
	}

	return ctrResp, nil
}
