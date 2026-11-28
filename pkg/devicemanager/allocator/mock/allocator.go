package mock

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/controllers"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/gox"
)

var (
	_ device.Allocator                        = (*Allocator)(nil)
	_ deviceplugin.ContainerAllocateResponder = (*server)(nil)
	_ deviceplugin.PhysicalSlicedResponder    = (*server)(nil)
)

type options struct {
	reconciler *deviceplugin.DevicesReconciler
	partitions *PartitionStore
	specStore  *mockspec.Store
	nodeName   string
}

// Option configures a mock Allocator or RDMAAllocator.
type Option func(*options)

// WithReconciler overrides the DevicesReconciler used by the mock device-plugin servers and
// partition reclaim loop. When unset, constructors resolve controllers.Get[*deviceplugin.DevicesReconciler]().
func WithReconciler(reconciler *deviceplugin.DevicesReconciler) Option {
	return func(o *options) {
		o.reconciler = reconciler
	}
}

// WithPartitionStore attaches a shared PartitionStore to the mock Allocator. When unset and
// hardware partitioning is enabled for the manufacturer, NewAllocator creates an isolated store.
func WithPartitionStore(store *PartitionStore) Option {
	return func(o *options) {
		o.partitions = store
	}
}

// WithSpecStore attaches a hot-reloading mockspec.Store and nodeName to the allocator.
func WithSpecStore(store *mockspec.Store, nodeName string) Option {
	return func(o *options) {
		o.specStore = store
		o.nodeName = nodeName
	}
}

func resolveOptions(mockOpts []Option) options {
	var cfg options
	for _, opt := range mockOpts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.reconciler == nil {
		cfg.reconciler = controllers.Get[*deviceplugin.DevicesReconciler]()
	}
	return cfg
}

// Allocator implements device.Allocator for one manufacturer in CPU-only mock mode.
// It registers real pkg/deviceplugin.ResourceServer instances across all enabled allocation modes
// and drives dead-Pod reclamation on its in-memory PartitionStore when partitioning is enabled.
type Allocator struct {
	manufacturer string
	logger       klog.Logger
	kubeSocket   string
	reconciler   *deviceplugin.DevicesReconciler
	partitions   *PartitionStore
	specStore    *mockspec.Store
	nodeName     string
	partitioned  bool
	modes        []workercore.DeviceAllocationMode
	servers      []deviceplugin.Server
	lifecycle    gox.Lifecycle
}

// NewAllocator constructs a CPU-only mock Allocator for manufacturer.
func NewAllocator(
	manufacturer string,
	opts device.AllocatorOptions,
	mockOpts ...Option,
) *Allocator {
	cfg := resolveOptions(mockOpts)
	logger := opts.Logger.WithName("mock").WithName(manufacturer)

	partitioned := !opts.NoPartitioned &&
		nodefeature.GetAcceleratableResourceName(manufacturer, workercore.DeviceAllocationModePartitioned) != ""
	partitions := cfg.partitions
	if partitioned && partitions == nil {
		partitions = NewPartitionStore()
	}

	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
	}
	if !opts.NoShared {
		modes = append(modes, workercore.DeviceAllocationModeShared)
	}
	if !opts.NoSliced {
		modes = append(modes, workercore.DeviceAllocationModeSliced)
	}
	if partitioned {
		modes = append(modes, workercore.DeviceAllocationModePartitioned)
	}
	modes = append(modes, workercore.DeviceAllocationModeVisibility)

	servers := make([]deviceplugin.Server, 0, len(modes))
	for _, mode := range modes {
		var modePartitions *PartitionStore
		if mode == workercore.DeviceAllocationModePartitioned || mode == workercore.DeviceAllocationModeVisibility {
			modePartitions = partitions
		}
		servers = append(servers, newServer(logger, manufacturer, mode, cfg.reconciler, modePartitions))
	}

	return &Allocator{
		manufacturer: manufacturer,
		logger:       logger,
		kubeSocket:   opts.KubeSocket,
		reconciler:   cfg.reconciler,
		partitions:   partitions,
		specStore:    cfg.specStore,
		nodeName:     cfg.nodeName,
		partitioned:  partitioned,
		modes:        modes,
		servers:      servers,
	}
}

// AllocatorCreator returns a creator function matching the allocator registry signature in
// pkg/devicemanager/allocator.
func AllocatorCreator(
	manufacturer string, mockOpts ...Option,
) func(device.AllocatorOptions) device.Allocator {
	copied := slices.Clone(mockOpts)
	return func(opts device.AllocatorOptions) device.Allocator {
		return NewAllocator(manufacturer, opts, copied...)
	}
}

// SupportedAllocatorCreators builds a manufacturer-to-creator map covering all known
// acceleratable manufacturers.
func SupportedAllocatorCreators(
	mockOpts ...Option,
) map[string]func(device.AllocatorOptions) device.Allocator {
	known := nodefeature.GetKnownAcceleratableManufacturers()
	creators := make(map[string]func(device.AllocatorOptions) device.Allocator, len(known))
	for _, mfr := range known {
		creators[mfr] = AllocatorCreator(mfr, mockOpts...)
	}
	return creators
}

// Name returns the manufacturer served by this mock Allocator.
func (a *Allocator) Name() string {
	return a.manufacturer
}

// Modes returns a copy of the allocation modes served by this Allocator.
func (a *Allocator) Modes() []workercore.DeviceAllocationMode {
	return slices.Clone(a.modes)
}

// Servers returns a copy of the underlying deviceplugin.Server instances.
func (a *Allocator) Servers() []deviceplugin.Server {
	return slices.Clone(a.servers)
}

// PartitionStore returns the virtual partition store used by partitioned and visibility modes,
// or nil when partitioning is disabled and no store was supplied.
func (a *Allocator) PartitionStore() *PartitionStore {
	return a.partitions
}

// SpecStore returns the optional mockspec.Store attached to this Allocator.
func (a *Allocator) SpecStore() *mockspec.Store {
	return a.specStore
}

// Start launches every registered ResourceServer against the configured kubelet socket and,
// when partitioning is enabled, runs the level-based virtual partition reclaim loop until ctx
// is canceled or Stop is called.
func (a *Allocator) Start(ctx context.Context) error {
	a.logger.Info("starting mock allocator")

	tasks := make([]func(context.Context) error, 0, len(a.servers)+1)
	for i := range a.servers {
		srv := a.servers[i]
		tasks = append(tasks, func(ctx context.Context) error {
			return srv.Start(ctx, a.kubeSocket)
		})
	}
	if a.partitioned && a.partitions != nil && a.reconciler != nil {
		tasks = append(tasks, func(ctx context.Context) error {
			reclaimFn := reconcilerReclaimFunc(
				ctx,
				a.logger.WithName("reclaim"),
				a.reconciler,
				a.partitions,
				a.manufacturer,
			)
			deviceplugin.RunReclaimLoop(
				ctx,
				a.reconciler,
				a.manufacturer,
				workercore.DeviceAllocationModePartitioned,
				reclaimFn,
			)
			return nil
		})
	}

	return a.lifecycle.Start(ctx, tasks...)
}

// Stop terminates all running ResourceServers and the partition reclaim loop cleanly.
func (a *Allocator) Stop() {
	a.logger.Info("stopping mock allocator")
	a.lifecycle.Stop()
}

type server struct {
	deviceplugin.ResourceServer
	partitions *PartitionStore
}

func newServer(
	logger klog.Logger,
	manufacturer string,
	mode workercore.DeviceAllocationMode,
	reconciler *deviceplugin.DevicesReconciler,
	partitions *PartitionStore,
) deviceplugin.Server {
	s := &server{
		ResourceServer: deviceplugin.ResourceServer{
			Logger:         logger.WithName(strings.ToLower(mode.String())),
			Manufacturer:   manufacturer,
			AllocationMode: mode,
			Reconciler:     reconciler,
		},
		partitions: partitions,
	}
	s.Responder = s
	return s
}

// GetContainerAllocateResponse validates allocated accelerators through deviceplugin.AllocatedAccelerators
// and injects diagnostic mock and vendor visible-devices environment variables with zero host
// device nodes and zero host mounts.
func (s *server) GetContainerAllocateResponse(
	_ context.Context,
	_ *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if devs == nil {
		return nil, errors.New("devices inventory is nil")
	}

	var ctrName string
	if ctr != nil {
		ctrName = ctr.Name
	}

	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	if len(accelerators) == 0 {
		return nil, fmt.Errorf("no allocated accelerator found for container %q", ctrName)
	}
	if len(accelerators) != len(allocated) {
		return nil, fmt.Errorf(
			"allocated accelerators resolved %d of %d requested resources for container %q",
			len(accelerators), len(allocated), ctrName,
		)
	}
	for i := range accelerators {
		if accelerators[i].Accel.ID == "" {
			return nil, fmt.Errorf("card at index %d reports an empty id", accelerators[i].Accel.Index)
		}
	}

	ids := deviceplugin.AllocatedAcceleratorIDs(accelerators)
	return &deviceplugin.ContainerAllocateResponse{
		Envs: buildMockAllocateEnvs(s.Manufacturer, s.AllocationMode, ids, false),
	}, nil
}

// ActuatePhysicalSliced materializes virtual partitions in PartitionStore for each allocated
// parent card and returns a ContainerAllocateResponse carrying only environment variables.
func (s *server) ActuatePhysicalSliced(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	profile string,
) (*deviceplugin.PhysicalSlicedAllocation, error) {
	if s.partitions == nil {
		return nil, errors.New("mock virtual partition store is not configured")
	}
	if devs == nil {
		return nil, errors.New("devices inventory is nil")
	}
	if pod == nil || ctr == nil {
		return nil, errors.New("pod and container are required for physical-slice actuation")
	}

	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	if len(accelerators) == 0 {
		return nil, fmt.Errorf("no allocated card for physical-slice container %q", ctr.Name)
	}
	if len(accelerators) != len(allocated) {
		return nil, fmt.Errorf(
			"physical-slice container %q resolved %d of %d allocated cards",
			ctr.Name, len(accelerators), len(allocated),
		)
	}

	podUID := string(pod.UID)
	slotName := deviceplugin.CanonicalSlotContainerName(pod, ctr)
	type reservedItem struct {
		part    VirtualPartition
		rebound bool
	}
	reserved := make([]reservedItem, 0, len(accelerators))
	rollback := func() {
		for i := range reserved {
			item := reserved[i]
			if item.rebound {
				continue
			}
			s.partitions.Release(
				s.Manufacturer,
				item.part.DeviceID,
				item.part.ID,
				podUID,
				slotName,
			)
		}
	}

	placements := make(deviceplugin.Placements, len(accelerators))
	ids := make(map[deviceplugin.Resource]string, len(accelerators))
	partIDs := make([]string, 0, len(accelerators))

	for i := range accelerators {
		grp := accelerators[i].Group
		acc := accelerators[i].Accel
		vp, rebound, err := s.partitions.Reserve(s.Manufacturer, podUID, slotName, grp, acc, profile)
		if err != nil {
			rollback()
			return nil, err
		}
		reserved = append(reserved, reservedItem{part: vp, rebound: rebound})

		res := deviceplugin.Resource{Group: grp.ID, Device: acc.ID}
		placements[res] = []workercore.AcceleratorPlacement{vp.Placement}
		ids[res] = vp.ID
		partIDs = append(partIDs, vp.ID)

		s.Logger.Info("granted mock virtual partition",
			"pod", kubemeta.GetNamespacedNameKey(pod),
			"container", ctr.Name,
			"card", acc.ID,
			"profile", profile,
			"placement", vp.Placement,
			"partitionID", vp.ID,
			"rebound", rebound,
		)
	}

	return &deviceplugin.PhysicalSlicedAllocation{
		Profile:    profile,
		Placements: placements,
		IDs:        ids,
		Response: &deviceplugin.ContainerAllocateResponse{
			Envs: buildMockAllocateEnvs(s.Manufacturer, workercore.DeviceAllocationModePartitioned, partIDs, true),
		},
		Rollback: rollback,
	}, nil
}

// GetPhysicalSlicedVisibilityResponse resolves the live virtual partitions held by owner on the
// co-allocated parent cards and returns a visibility response with zero host devices or mounts.
func (s *server) GetPhysicalSlicedVisibilityResponse(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	owner string,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if s.partitions == nil {
		return nil, fmt.Errorf("mock virtual partition store is not configured; cannot resolve owner %q", owner)
	}
	if devs == nil {
		return nil, errors.New("devices inventory is nil")
	}
	if pod == nil || ctr == nil {
		return nil, errors.New("pod and container are required for visibility co-allocation")
	}

	accelerators := deviceplugin.AllocatedAccelerators(devs, allocated)
	if len(accelerators) == 0 {
		return nil, fmt.Errorf("no allocated card for visibility container %q", ctr.Name)
	}
	if len(accelerators) != len(allocated) {
		return nil, fmt.Errorf(
			"visibility container %q resolved %d of %d allocated cards",
			ctr.Name, len(accelerators), len(allocated),
		)
	}

	podUID := string(pod.UID)
	partIDs := make([]string, 0, len(accelerators))
	for i := range accelerators {
		acc := accelerators[i].Accel
		vp, ok := s.partitions.GetByOwner(s.Manufacturer, podUID, owner, acc.ID)
		if !ok {
			return nil, fmt.Errorf(
				"no live virtual partition held by container %q on card %s",
				owner, acc.ID,
			)
		}
		profSpec, offered := lookupPhysicalProfile(acc, vp.Profile)
		if !offered || !containsPlacement(profSpec.Placements, vp.Placement) {
			return nil, fmt.Errorf(
				"card %s no longer offers profile %q placement %+v recorded for container %q",
				acc.ID, vp.Profile, vp.Placement, owner,
			)
		}
		partIDs = append(partIDs, vp.ID)
	}

	return &deviceplugin.ContainerAllocateResponse{
		Envs: buildMockAllocateEnvs(s.Manufacturer, workercore.DeviceAllocationModeVisibility, partIDs, true),
	}, nil
}
