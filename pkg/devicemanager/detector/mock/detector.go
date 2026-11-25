package mock

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
)

var (
	_ device.Detector                     = (*Detector)(nil)
	_ device.AcceleratorProcessDetector   = (*Detector)(nil)
	_ device.AcceleratorPartitionDetector = (*Detector)(nil)
)

// PartitionProvider resolves live telemetry for a requested virtual hardware partition.
// Returning found as false falls back to static partition entries in the mock specification.
type PartitionProvider interface {
	LookupPartition(manufacturer string, req device.AcceleratorPartitionRequest) (device.AcceleratorPartition, bool)
}

// PartitionProviderFunc adapts a function to PartitionProvider.
type PartitionProviderFunc func(
	manufacturer string, req device.AcceleratorPartitionRequest,
) (device.AcceleratorPartition, bool)

// LookupPartition calls f(manufacturer, req).
func (f PartitionProviderFunc) LookupPartition(
	manufacturer string, req device.AcceleratorPartitionRequest,
) (device.AcceleratorPartition, bool) {
	if f == nil {
		return device.AcceleratorPartition{}, false
	}
	return f(manufacturer, req)
}

// Option configures a mock Detector.
type Option func(*Detector)

// WithLogger sets the logger used by the mock Detector.
func WithLogger(logger klog.Logger) Option {
	return func(d *Detector) {
		d.logger = logger
	}
}

// WithPartitionProvider registers a live virtual partition telemetry provider.
func WithPartitionProvider(provider PartitionProvider) Option {
	return func(d *Detector) {
		d.partitionProvider = provider
	}
}

type cachedAccelerator struct {
	group mockspec.GroupMeta
	accel device.Accelerator
}

type cachedMetrics struct {
	id     string
	memory uint64
}

// Detector is a thread-safe, CPU-only device.Detector driven by a hot-reloaded mockspec.Store.
type Detector struct {
	manufacturer      string
	nodeName          string
	store             *mockspec.Store
	logger            klog.Logger
	partitionProvider PartitionProvider

	mu              sync.Mutex
	initRevision    uint64
	initFailures    int
	cachedDetected  map[uint32]cachedAccelerator
	cachedMonitored map[uint32]cachedMetrics
}

// NewDetector creates a CPU-only mock Detector for manufacturer on nodeName backed by store.
func NewDetector(manufacturer, nodeName string, store *mockspec.Store, opts ...Option) *Detector {
	d := &Detector{
		manufacturer:    manufacturer,
		nodeName:        nodeName,
		store:           store,
		logger:          klog.Background().WithName("mock-detector").WithValues("manufacturer", manufacturer),
		cachedDetected:  make(map[uint32]cachedAccelerator),
		cachedMonitored: make(map[uint32]cachedMetrics),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(d)
		}
	}
	return d
}

// NewDetectors creates one CPU-only mock Detector per manufacturer in caller order.
func NewDetectors(
	manufacturers []string, nodeName string, store *mockspec.Store, opts ...Option,
) []device.Detector {
	out := make([]device.Detector, 0, len(manufacturers))
	for _, mfr := range manufacturers {
		out = append(out, NewDetector(mfr, nodeName, store, opts...))
	}
	return out
}

// Name returns the manufacturer name handled by this mock Detector.
func (d *Detector) Name() string {
	return d.manufacturer
}

// DetectAccelerator returns mock device groups for the configured manufacturer and node.
func (d *Detector) DetectAccelerator(_ bool) (device.DevicesGroupList, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	mfrSpec, err := d.resolveManufacturerLocked()
	if err != nil {
		return nil, err
	}
	if mfrSpec == nil || len(mfrSpec.Slots) == 0 {
		d.clearCacheLocked()
		return nil, nil
	}

	var grpList device.DevicesGroupList
	nextDetected := make(map[uint32]cachedAccelerator, len(mfrSpec.Slots))
	nextMonitored := make(map[uint32]cachedMetrics, len(mfrSpec.Slots))
	unqueryableSlots := make(map[uint32]struct{})
	queriedCount := 0
	var lastUnqueryableErr error

	for i := range mfrSpec.Slots {
		slot := &mfrSpec.Slots[i]
		switch slot.QueryState {
		case mockspec.QueryStateFatalError:
			return nil, fmt.Errorf(
				"fatal mock query error on %s index %d (%s)",
				d.manufacturer, slot.Index, slot.Accelerator.ID,
			)
		case mockspec.QueryStateUnqueryable:
			lastUnqueryableErr = fmt.Errorf(
				"unqueryable mock device on %s index %d (%s)",
				d.manufacturer, slot.Index, slot.Accelerator.ID,
			)
			d.logger.V(2).Info("skipping unqueryable mock device",
				"index", slot.Index, "id", slot.Accelerator.ID)
			d.restoreCachedDetectedLocked(slot.Index, unqueryableSlots, &grpList)
			continue
		default:
			accel := *slot.Accelerator.DeepCopy()
			appendToGroup(&grpList, slot.Group, accel)
			nextDetected[slot.Index] = cachedAccelerator{
				group: slot.Group,
				accel: *slot.Accelerator.DeepCopy(),
			}
			nextMonitored[slot.Index] = cachedMetrics{
				id:     slot.Accelerator.ID,
				memory: slot.Group.Memory,
			}
			queriedCount++
		}
	}

	if queriedCount == 0 {
		return nil, lastUnqueryableErr
	}

	d.commitDetectedCacheLocked(nextDetected, nextMonitored, unqueryableSlots)
	device.SetGroupSlicedDetails(grpList)
	return grpList, nil
}

// MonitorAccelerator returns mock metrics groups for the configured manufacturer and node.
func (d *Detector) MonitorAccelerator(_ bool) (device.MetricsGroupList, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	mfrSpec, err := d.resolveManufacturerLocked()
	if err != nil {
		return nil, err
	}
	if mfrSpec == nil || len(mfrSpec.Slots) == 0 {
		d.clearCacheLocked()
		return nil, nil
	}

	metricsGrp := device.MetricsGroup{
		Manufacturer: d.manufacturer,
		Timestamp:    time.Now(),
		Accelerators: make([]device.AcceleratorMetrics, 0, len(mfrSpec.Slots)),
	}
	nextMonitored := make(map[uint32]cachedMetrics, len(mfrSpec.Slots))
	unqueryableSlots := make(map[uint32]struct{})
	queriedCount := 0
	var lastUnqueryableErr error

	for i := range mfrSpec.Slots {
		slot := &mfrSpec.Slots[i]
		switch slot.QueryState {
		case mockspec.QueryStateFatalError:
			return nil, fmt.Errorf(
				"fatal mock query error on %s index %d (%s)",
				d.manufacturer, slot.Index, slot.Accelerator.ID,
			)
		case mockspec.QueryStateUnqueryable:
			lastUnqueryableErr = fmt.Errorf(
				"unqueryable mock device on %s index %d (%s)",
				d.manufacturer, slot.Index, slot.Accelerator.ID,
			)
			d.logger.V(2).Info("skipping unqueryable mock device in monitor",
				"index", slot.Index, "id", slot.Accelerator.ID)
			d.restoreCachedMonitoredLocked(slot.Index, unqueryableSlots, &metricsGrp.Accelerators)
			continue
		default:
			metricsGrp.Accelerators = append(metricsGrp.Accelerators, slot.Metrics)
			nextMonitored[slot.Index] = cachedMetrics{
				id:     slot.Metrics.ID,
				memory: slot.Metrics.Memory,
			}
			queriedCount++
		}
	}

	if queriedCount == 0 {
		return nil, lastUnqueryableErr
	}

	d.commitMonitoredCacheLocked(nextMonitored, unqueryableSlots)
	return device.MetricsGroupList{metricsGrp}, nil
}

func (d *Detector) resolveManufacturerLocked() (*mockspec.ResolvedManufacturerSpec, error) {
	if d.store == nil {
		return nil, errors.New("mock devices store is nil")
	}
	snap, err := d.store.Refresh()
	if snap == nil {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("mock devices store has no valid snapshot")
	}

	nodeSpec := snap.Document.ResolveNode(d.nodeName)
	mfrSpec := nodeSpec.Manufacturers[d.manufacturer]
	if mfrSpec == nil {
		return nil, nil
	}

	if err := d.checkManufacturerFaultsLocked(snap.Revision, mfrSpec.Fault); err != nil {
		return nil, err
	}
	return mfrSpec, nil
}

func (d *Detector) checkManufacturerFaultsLocked(
	revision uint64, fault mockspec.ManufacturerFaultSpec,
) error {
	if d.initRevision != revision {
		d.initRevision = revision
		d.initFailures = 0
	}
	if fault.InitError.Enabled {
		msg := cmp.Or(fault.InitError.Message, "mock library initialization failed")
		if fault.InitError.FailCount == 0 {
			return fmt.Errorf("initialize %s mock library: %s", d.manufacturer, msg)
		}
		if d.initFailures < fault.InitError.FailCount {
			d.initFailures++
			return fmt.Errorf("initialize %s mock library: %s", d.manufacturer, msg)
		}
	}
	if fault.CountError.Enabled {
		msg := cmp.Or(fault.CountError.Message, "mock device count failed")
		return fmt.Errorf("get %s mock device count: %s", d.manufacturer, msg)
	}
	if fault.TotalFailure.Enabled {
		msg := cmp.Or(fault.TotalFailure.Message, "mock total query failure")
		return fmt.Errorf("query %s mock devices: %s", d.manufacturer, msg)
	}
	return nil
}

func appendToGroup(
	grpList *device.DevicesGroupList, meta mockspec.GroupMeta, accel device.Accelerator,
) {
	grpIndex := slices.IndexFunc(*grpList, func(g device.DevicesGroup) bool {
		return g.ID == meta.ID && g.Name == meta.Name && g.Memory == meta.Memory
	})
	if grpIndex < 0 {
		*grpList = append(*grpList, device.DevicesGroup{
			ID:                meta.ID,
			Manufacturer:      meta.Manufacturer,
			Name:              meta.Name,
			Memory:            meta.Memory,
			Cores:             meta.Cores,
			DriverVersion:     meta.DriverVersion,
			RuntimeVersion:    meta.RuntimeVersion,
			ComputeCapability: meta.ComputeCapability,
			Family:            meta.Family,
		})
		grpIndex = len(*grpList) - 1
	}
	(*grpList)[grpIndex].Accelerators = append((*grpList)[grpIndex].Accelerators, accel)
}

func (d *Detector) restoreCachedDetectedLocked(
	slot uint32, unqueryableSlots map[uint32]struct{}, grpList *device.DevicesGroupList,
) {
	unqueryableSlots[slot] = struct{}{}
	cached, ok := d.cachedDetected[slot]
	if !ok {
		return
	}
	accel := *cached.accel.DeepCopy()
	accel.Status.Unhealthy = true
	appendToGroup(grpList, cached.group, accel)
}

func (d *Detector) restoreCachedMonitoredLocked(
	slot uint32, unqueryableSlots map[uint32]struct{}, accels *[]device.AcceleratorMetrics,
) {
	unqueryableSlots[slot] = struct{}{}
	cached, ok := d.cachedMonitored[slot]
	if !ok {
		return
	}
	*accels = append(*accels, device.AcceleratorMetrics{
		ID:        cached.id,
		Memory:    cached.memory,
		Unhealthy: true,
	})
}

func (d *Detector) commitDetectedCacheLocked(
	nextDetected map[uint32]cachedAccelerator,
	nextMonitored map[uint32]cachedMetrics,
	unqueryableSlots map[uint32]struct{},
) {
	for slot := range unqueryableSlots {
		if _, exists := nextDetected[slot]; !exists {
			if cached, ok := d.cachedDetected[slot]; ok {
				nextDetected[slot] = cached
			}
		}
		if _, exists := nextMonitored[slot]; !exists {
			if cached, ok := d.cachedMonitored[slot]; ok {
				nextMonitored[slot] = cached
			}
		}
	}
	d.cachedDetected = nextDetected
	d.cachedMonitored = nextMonitored
}

func (d *Detector) commitMonitoredCacheLocked(
	nextMonitored map[uint32]cachedMetrics,
	unqueryableSlots map[uint32]struct{},
) {
	for slot := range unqueryableSlots {
		if _, exists := nextMonitored[slot]; !exists {
			if cached, ok := d.cachedMonitored[slot]; ok {
				nextMonitored[slot] = cached
			}
		}
	}
	d.cachedMonitored = nextMonitored

	for slot := range d.cachedDetected {
		if _, ok := nextMonitored[slot]; ok {
			continue
		}
		delete(d.cachedDetected, slot)
	}
}

func (d *Detector) clearCacheLocked() {
	clear(d.cachedDetected)
	clear(d.cachedMonitored)
}
