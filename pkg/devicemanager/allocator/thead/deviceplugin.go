package thead

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

const Manufacturer = nodefeature.ManufacturerTHead

func New(opts device.AllocatorOptions) device.Allocator {
	logger := opts.Logger.WithName(Manufacturer)

	// The hardware-partitioning server serves "<base>.partitioned" — the vendor calls its own
	// partitioning MIG, so it shares that word. A manufacturer with no partition kind has no such
	// resource name at all, so it registers no server rather than one advertising an empty name.
	partitioned := !opts.NoPartitioned &&
		nodefeature.GetAcceleratableResourceName(Manufacturer, workercore.DeviceAllocationModePartitioned) != ""
	// The partition driver takes a vendor management-library init at construction, so it is built once
	// — only where partitions are served — and shared by the two servers that address them: the
	// partitioned server materializes an instance, the visibility server proves one is still live
	// before naming it again. A node serving no partitioning initializes nothing.
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
	// The sliced server serves "<base>.sliced": a share of one card, enforced inside the container by
	// the preload pair this operator's image builds rather than by the driver. It takes no partition
	// driver — a logical slice never touches the partitioning surface.
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
	// access only, no slicing artifacts). On a partition-backed card that response must name the
	// owner's partition rather than the parent card, which is what the shared driver is for.
	servers = append(servers,
		newServer(logger, workercore.DeviceAllocationModeVisibility, mig),
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
	// partitioned reports whether a Partitioned server is registered, gating the per-vendor partition
	// reclaim loop: the loop exists to free the instances that server creates.
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
	// A device-plugin pool has no Release callback, so the GPU/compute instances the partitioned
	// server creates are reclaimed by a level-based loop fed the reconciler's broadcast live-pod set
	// plus a resync ticker. The loop takes its own driver instance, mirroring the blueprint rather
	// than sharing the servers' one.
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

// liveClaimsFrom adapts the reconciler's annotation-derived live physical-slice occupancy into the
// reclaimer's per-card placement view (the Resource Device field is the card UUID for this vendor). It
// is the attribution self-check source: reclaim never destroys an instance a running Pod still claims.
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

	// mig is the vendor partition seam: the partitioned responder drives it for a
	// "<base>.partitioned.mig-<profile>" request, and the visibility responder reads it to prove a
	// co-allocated partition is still live. It is nil for every other mode, and on a node that serves
	// no partitioning at all.
	mig migDriver
}

func newServer(logger klog.Logger, mode workercore.DeviceAllocationMode, mig migDriver) deviceplugin.Server {
	logger = logger.WithName(strings.ToLower(mode.String()))

	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logger,
			Manufacturer:   Manufacturer,
			AllocationMode: mode,
			Reconciler:     controllers.Get[*deviceplugin.DevicesReconciler](),
		},
		mig: mig,
	}
	s.Responder = s

	return s
}

// _AllocatedAccelerator pairs an allocated card with its group; the group carries the VRAM the
// sliced path derives that card's own memory budget from.
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
	ctrResp := &deviceplugin.ContainerAllocateResponse{}

	// Mount control devices. They are needed once per container rather than per card, and every node
	// here is required: this vendor has no container-runtime hook, so the injected nodes are the whole
	// of the container's access, and a set missing one of them would start a container that cannot
	// address its card at all. They are resolved through the same fail-closed helper the partition path
	// uses, rather than the shared device-spec helper, which returns nil for a path that does not exist
	// and would turn a missing node into a SUCCESSFUL allocation carrying a silently incomplete set.
	for _, p := range sharedControlNodePaths() {
		pDev, err := requireDeviceNode(p)
		if err != nil {
			return nil, err
		}
		ctrResp.Devices = append(ctrResp.Devices, pDev)
	}

	// Mount specified devices. The pass also collects each allocated card with its group, in devs
	// order, which is the order the sliced path indexes its per-card memory figures by.
	var accelerators []_AllocatedAccelerator
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

			// The vendor names a card's device node after the card's ordinal — its accelerator index —
			// and that the ordinal reaches the card the detector measured is proven rather than assumed,
			// through the same guard the partition path uses: the node it names must carry the minor
			// number the detector recorded for this card. A card that cannot be proven is refused rather
			// than answered with a device set that is silently short of its card, or that carries a
			// neighboring card's node.
			_, cardNode, err := requireCardNode(devs, devsAccelerator.ID)
			if err != nil {
				return nil, err
			}
			ctrResp.Devices = append(ctrResp.Devices, cardNode)
			accelerators = append(accelerators,
				_AllocatedAccelerator{
					group: devGroup,
					accel: devsAccelerator,
				},
			)
		}
	}

	// A sliced container gets that same device set plus the shim's preload and quota; every other
	// mode's response is the device set alone, because this vendor has no container-runtime hook to
	// interpret anything else.
	if s.AllocationMode == workercore.DeviceAllocationModeSliced {
		return s.getSlicedContainerAllocateResponse(pod, ctr, ctrResp.Devices, accelerators)
	}

	return ctrResp, nil
}

// In-container paths the PPU slicing shim expects. The two shared objects sit exactly where the
// ld.so.preload asset names them, so that file and these constants are one contract.
const (
	ctrLdPreloadPath = "/etc/ld.so.preload"
	ctrDlsymHookPath = "/usr/local/vppu/hgml_dlsym_hook.so"
	ctrQuotaLibPath  = "/usr/local/vppu/hggc_quota.so"
	ctrMonitorPath   = "/usr/local/vppu/ppu-monitor"
	ctrLedgerDir     = "/var/run/vppu"
	ctrLedgerPath    = ctrLedgerDir + "/ledger"
)

// getSlicedContainerAllocateResponse renders the logical-slicing injection for a sliced container:
// the quota figures the shim reads at load, the mounts that preload it, and the writable directory
// its cross-process usage region lives in. devices is the container's already-verified device set,
// which a slice takes unchanged — the vendor has no container-runtime hook, so the nodes are the
// whole of the container's access whether it holds a share of a card or all of it.
func (s *server) getSlicedContainerAllocateResponse(
	pod *core.Pod,
	ctr *core.Container,
	devices []*deviceplugin.DeviceSpec,
	accels []_AllocatedAccelerator,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if len(accels) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for sliced container %q", ctr.Name)
	}

	// The usage region is per container rather than per node, unlike the NVIDIA branch's host
	// /dev/shm: it is addressed by container-local card index, so a shared location would let two
	// containers' index 0 charge one slot. Under the pod work dir, so the existing per-pod reclaim
	// removes it with the pod. The shim creates the region file itself; this is only its directory,
	// world-writable because the workload's user is not ours to predict.
	ledgerDir := filepath.Join(deviceplugin.PodWorkDir(string(pod.UID), ctr.Name), "run/vppu")
	if err := osx.MkdirAll(ledgerDir, 0o777); err != nil {
		return nil, fmt.Errorf("create %q: %w", ledgerDir, err)
	}

	// The variable shape is HAMi-core's, deliberately: a per-card memory limit indexed by loop
	// position, and ONE un-indexed compute limit which the shim reads as the cap for every card
	// carrying no figure of its own. Admission pins a logical slice to a single card, so the index
	// is 0 today; the loop is written for several anyway, exactly as the NVIDIA branch's is.
	//
	// What is NOT copied from HAMi-core is what an absent compute figure means. It defaults to a
	// whole card's compute there and makes the card unusable here, and SlicedCoresPercent returns
	// 100 when nothing was requested — so the figure is emitted even at 100, because omitting it
	// would be indistinguishable from "no compute quota" to everything downstream.
	coresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(Manufacturer)
	memPctRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(Manufacturer)
	memMibRes := nodefeature.GetAcceleratableSlicedMemoryMibResourceName(Manufacturer)
	envs := map[string]string{
		"HGGC_DEVICE_SM_LIMIT": strconv.Itoa(deviceplugin.SlicedCoresPercent(ctr, coresRes)),
		"HGGC_LEDGER_PATH":     ctrLedgerPath,
	}
	for i := range accels {
		// The MiB figure carries no unit suffix, unlike the NVIDIA branch's "…m": HAMi-core parses
		// a suffix, the shim parses a bare MiB integer.
		memMib, err := deviceplugin.SlicedMemoryMib(ctr, memPctRes, memMibRes, int64(accels[i].group.Memory))
		if err != nil {
			return nil, fmt.Errorf("derive sliced memory limit: %w", err)
		}
		envs["HGGC_DEVICE_MEMORY_LIMIT_"+strconv.Itoa(i)] = strconv.FormatInt(memMib, 10)
	}

	// State the verbosity the slice runs at rather than inheriting it: level 1 reports denials and
	// errors, which is the shim's own default today, and naming it keeps the level a property of the
	// allocation instead of a library default a later shim change could move underneath it. A
	// container that sets LIBHGGC_LOG_LEVEL itself keeps its value — the debugging escape hatch.
	if !deviceplugin.ContainerEnvDeclared(ctr, "LIBHGGC_LOG_LEVEL") {
		envs["LIBHGGC_LOG_LEVEL"] = "1"
	}

	libDir := filepath.Join(deviceplugin.OperatorLibDir, "thead")
	return &deviceplugin.ContainerAllocateResponse{
		Devices: devices,
		Envs:    envs,
		Mounts: []*deviceplugin.Mount{
			{ContainerPath: ctrLdPreloadPath, HostPath: filepath.Join(libDir, "ld.so.preload"), ReadOnly: true},
			{ContainerPath: ctrDlsymHookPath, HostPath: filepath.Join(libDir, "hgml_dlsym_hook.so"), ReadOnly: true},
			{ContainerPath: ctrQuotaLibPath, HostPath: filepath.Join(libDir, "hggc_quota.so"), ReadOnly: true},
			{ContainerPath: ctrMonitorPath, HostPath: filepath.Join(libDir, "ppu-monitor"), ReadOnly: true},
			{ContainerPath: ctrLedgerDir, HostPath: ledgerDir, ReadOnly: false},
		},
	}, nil
}
