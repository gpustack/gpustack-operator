package mock

import (
	"context"
	"slices"
	"strconv"
	"strings"

	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

// RDMAFamilyName is the resource family name served by RDMAAllocator.
const RDMAFamilyName = "rdma"

var _ device.Allocator = (*RDMAAllocator)(nil)

// RDMAAllocator implements device.Allocator for mock RDMA resources in CPU-only mode.
type RDMAAllocator struct {
	logger     klog.Logger
	kubeSocket string
	modes      []workercore.DeviceAllocationMode
	servers    []deviceplugin.Server
	lifecycle  gox.Lifecycle
}

// NewRDMAAllocator constructs a CPU-only mock RDMA allocator serving enabled RDMA allocation modes.
func NewRDMAAllocator(opts device.AllocatorOptions, mockOpts ...Option) *RDMAAllocator {
	cfg := resolveOptions(mockOpts)
	logger := opts.Logger.WithName("mock").WithName(RDMAFamilyName)

	candidateModes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
	}
	if !opts.NoShared {
		candidateModes = append(candidateModes, workercore.DeviceAllocationModeShared)
	}
	if !opts.NoPartitioned {
		candidateModes = append(candidateModes, workercore.DeviceAllocationModePartitioned)
	}

	modes := make([]workercore.DeviceAllocationMode, 0, len(candidateModes))
	servers := make([]deviceplugin.Server, 0, len(candidateModes))
	for _, mode := range candidateModes {
		srv, err := NewRDMAServer(logger, mode, cfg.reconciler, mockOpts...)
		if err != nil {
			logger.Error(err, "skipping mock RDMA server for mode", "mode", mode.String())
			continue
		}
		modes = append(modes, mode)
		servers = append(servers, srv)
	}

	return &RDMAAllocator{
		logger:     logger,
		kubeSocket: opts.KubeSocket,
		modes:      modes,
		servers:    servers,
	}
}

// RDMAAllocatorCreator returns a creator function matching rdmaAllocatorCreator in
// pkg/devicemanager/allocator.
func RDMAAllocatorCreator(mockOpts ...Option) func(device.AllocatorOptions) device.Allocator {
	copied := slices.Clone(mockOpts)
	return func(opts device.AllocatorOptions) device.Allocator {
		return NewRDMAAllocator(opts, copied...)
	}
}

// Name returns RDMAFamilyName ("rdma").
func (*RDMAAllocator) Name() string {
	return RDMAFamilyName
}

// Modes returns a copy of the allocation modes served by this RDMAAllocator.
func (a *RDMAAllocator) Modes() []workercore.DeviceAllocationMode {
	return slices.Clone(a.modes)
}

// Servers returns a copy of the underlying RDMA device-plugin servers.
func (a *RDMAAllocator) Servers() []deviceplugin.Server {
	return slices.Clone(a.servers)
}

// Start runs all registered mock RDMA servers against the configured kubelet socket.
func (a *RDMAAllocator) Start(ctx context.Context) error {
	a.logger.Info("starting mock rdma allocator")

	tasks := make([]func(context.Context) error, 0, len(a.servers))
	for i := range a.servers {
		srv := a.servers[i]
		tasks = append(tasks, func(ctx context.Context) error {
			return srv.Start(ctx, a.kubeSocket)
		})
	}
	return a.lifecycle.Start(ctx, tasks...)
}

// Stop terminates all running mock RDMA servers cleanly.
func (a *RDMAAllocator) Stop() {
	a.logger.Info("stopping mock rdma allocator")
	a.lifecycle.Stop()
}

// NewRDMAServer constructs one CPU-only mock RDMA device-plugin server for mode.
// It reuses deviceplugin.NewRDMAServer for serving lifecycle, ListAndWatch,
// GetPreferredAllocation, RDMA selection policy filtering, and pinned-port NCCL_IB_HCA
// formatting while injecting diagnostic GPUSTACK_MOCK_* environment variables with zero
// /dev/infiniband/* device nodes and zero host mounts.
func NewRDMAServer(
	logger klog.Logger,
	mode workercore.DeviceAllocationMode,
	reconciler *deviceplugin.DevicesReconciler,
	mockOpts ...Option,
) (deviceplugin.Server, error) {
	var cfg options
	for _, opt := range mockOpts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if reconciler == nil && cfg.reconciler != nil {
		reconciler = cfg.reconciler
	}

	dpOpts := make([]deviceplugin.RDMAServerOption, 0, 2)
	dpOpts = append(dpOpts, deviceplugin.WithRDMAContainerInjector(
		func(_ *workercore.Devices, grants []string, resp *deviceplugin.ContainerAllocateResponse) error {
			names := strings.Join(grants, ",")
			resp.Envs[EnvMockAllocatedDevices] = names
			resp.Envs[EnvMockAllocationMode] = device.PreflightModeOf(mode)
			resp.Envs[EnvMockManufacturer] = RDMAFamilyName
			return nil
		},
	))
	if cfg.specStore != nil {
		dpOpts = append(dpOpts, deviceplugin.WithRDMAInterfaceInventory(
			mockRDMAInventoryReader(cfg.specStore, cfg.nodeName, reconciler),
		))
	}

	return deviceplugin.NewRDMAServer(logger, mode, reconciler, dpOpts...)
}

func mockRDMAInventoryReader(
	store *mockspec.Store,
	nodeName string,
	reconciler *deviceplugin.DevicesReconciler,
) func() *device.RDMASnapshot {
	return func() *device.RDMASnapshot {
		snap := store.Current()
		if snap == nil || snap.Document == nil {
			return nil
		}
		targetNode := nodeName
		if targetNode == "" && reconciler != nil {
			targetNode = reconciler.NodeName
		}
		resolved := snap.Document.ResolveNode(targetNode)
		return &device.RDMASnapshot{
			Revision: strconv.FormatUint(snap.Revision, 10),
			Complete: true,
			Devices:  slices.Clone(resolved.RDMADevices),
		}
	}
}
