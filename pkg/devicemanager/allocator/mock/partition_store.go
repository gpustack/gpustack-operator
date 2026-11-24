package mock

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// VirtualPartition describes one live virtual hardware partition tracked by PartitionStore.
type VirtualPartition struct {
	// ID is the deterministic synthetic partition handle.
	ID string
	// Manufacturer is the parent accelerator's manufacturer name.
	Manufacturer string
	// GroupID is the parent accelerator's DevicesGroup ID.
	GroupID string
	// DeviceID is the parent accelerator's unique ID.
	DeviceID string
	// CardIndex is the parent accelerator's enumeration index.
	CardIndex uint32
	// Profile is the physical-slice profile name in the manufacturer's internal spelling.
	Profile string
	// ComputeSlices is the compute-slice count of the profile.
	ComputeSlices int32
	// MemorySlices is the memory-slice count of the profile.
	MemorySlices int32
	// MemoryMib is the partition's memory capacity in MiB.
	MemoryMib int64
	// Placement is the [Start, Start+Length) slice interval occupied on the parent card.
	Placement workercore.AcceleratorPlacement
	// PodUID is the UID of the Pod whose workload container owns this partition.
	PodUID string
	// Container is the name of the workload container that owns this partition.
	Container string
}

// SyntheticPartitionID builds a deterministic partition handle from manufacturer, cardIndex,
// profile, and placement start. Recreating a partition of the same profile at the same placement
// on the same card produces the same handle, matching real MIG UUID behavior.
func SyntheticPartitionID(manufacturer string, cardIndex uint32, profile string, start int32) string {
	if manufacturer == "" {
		return fmt.Sprintf("MOCK-MIG-%d-%s-%d", cardIndex, profile, start)
	}
	return fmt.Sprintf("MOCK-MIG-%s-%d-%s-%d", manufacturer, cardIndex, profile, start)
}

type partitionCardKey struct {
	manufacturer string
	deviceID     string
}

// PartitionStore maintains an in-memory, concurrency-safe ledger of virtual hardware partitions
// across parent cards, supporting legal placement selection, overlap exclusion, idempotent retry
// rebinding, visibility lookup, and dead-Pod reclamation.
type PartitionStore struct {
	mu     sync.RWMutex
	byCard map[partitionCardKey][]VirtualPartition
	byID   map[string]VirtualPartition
}

// NewPartitionStore returns an empty PartitionStore ready for concurrent use.
func NewPartitionStore() *PartitionStore {
	return &PartitionStore{
		byCard: make(map[partitionCardKey][]VirtualPartition),
		byID:   make(map[string]VirtualPartition),
	}
}

// Reserve allocates or rebinds one virtual partition of profile on acc for (podUID, container).
// When the same (podUID, container) already owns a live partition on acc with the same profile,
// Reserve returns that partition with rebound=true. Otherwise it picks the lowest-start legal
// placement from acc.Status.PhysicalSliced.Profiles that overlaps no existing partition on acc.
func (s *PartitionStore) Reserve(
	manufacturer, podUID, container string,
	grp *workercore.DevicesGroup,
	acc *workercore.Accelerator,
	profile string,
) (VirtualPartition, bool, error) {
	if s == nil {
		return VirtualPartition{}, false, errors.New("virtual partition store is nil")
	}
	if acc == nil || acc.ID == "" {
		return VirtualPartition{}, false, errors.New("parent accelerator has an empty id")
	}
	if podUID == "" || container == "" {
		return VirtualPartition{}, false, errors.New("pod UID and container name are required")
	}
	if profile == "" {
		return VirtualPartition{}, false, errors.New("partition profile name is empty")
	}

	profSpec, ok := lookupPhysicalProfile(acc, profile)
	if !ok {
		return VirtualPartition{}, false, fmt.Errorf("card %s offers no physical-slice profile %q", acc.ID, profile)
	}
	if len(profSpec.Placements) == 0 {
		return VirtualPartition{}, false, fmt.Errorf("card %s profile %q declares no legal placements", acc.ID, profile)
	}

	var groupID string
	if grp != nil {
		groupID = grp.ID
	}
	cardKey := partitionCardKey{manufacturer: manufacturer, deviceID: acc.ID}

	s.mu.Lock()
	defer s.mu.Unlock()

	live := s.byCard[cardKey]
	for i := range live {
		existing := live[i]
		if existing.PodUID != podUID || existing.Container != container {
			continue
		}
		if existing.Profile != profile {
			return VirtualPartition{}, true, fmt.Errorf(
				"card %s already holds partition %s with profile %q for pod %s container %q, cannot rebind as %q",
				acc.ID, existing.ID, existing.Profile, podUID, container, profile,
			)
		}
		if !containsPlacement(profSpec.Placements, existing.Placement) {
			return VirtualPartition{}, true, fmt.Errorf(
				"card %s no longer offers placement %+v for profile %q",
				acc.ID, existing.Placement, profile,
			)
		}
		return existing, true, nil
	}

	occupied := make([]workercore.AcceleratorPlacement, 0, len(live))
	for i := range live {
		occupied = append(occupied, live[i].Placement)
	}

	slot, found := pickLowestFreePlacement(profSpec.Placements, occupied)
	if !found {
		return VirtualPartition{}, false, fmt.Errorf(
			"card %s has no free placement for profile %q", acc.ID, profile,
		)
	}

	id := SyntheticPartitionID(manufacturer, acc.Index, profile, slot.Start)
	if prev, collision := s.byID[id]; collision {
		return VirtualPartition{}, false, fmt.Errorf(
			"synthetic partition id %q collides with live partition on card %s", id, prev.DeviceID,
		)
	}

	created := VirtualPartition{
		ID:            id,
		Manufacturer:  manufacturer,
		GroupID:       groupID,
		DeviceID:      acc.ID,
		CardIndex:     acc.Index,
		Profile:       profile,
		ComputeSlices: profSpec.ComputeSlices,
		MemorySlices:  profSpec.MemorySlices,
		MemoryMib:     profSpec.MemoryMib,
		Placement:     slot,
		PodUID:        podUID,
		Container:     container,
	}
	s.byCard[cardKey] = append(live, created)
	s.byID[id] = created
	return created, false, nil
}

// Release removes a specific partition iff it is still owned by (podUID, container) with matching
// partitionID, returning true when removed. Rollback paths use Release so a partition already
// reclaimed and recreated for another Pod is never removed.
func (s *PartitionStore) Release(manufacturer, deviceID, partitionID, podUID, container string) bool {
	if s == nil {
		return false
	}
	cardKey := partitionCardKey{manufacturer: manufacturer, deviceID: deviceID}

	s.mu.Lock()
	defer s.mu.Unlock()

	live := s.byCard[cardKey]
	for i := range live {
		p := live[i]
		if p.ID == partitionID && p.PodUID == podUID && p.Container == container {
			live = slices.Delete(live, i, i+1)
			if len(live) == 0 {
				delete(s.byCard, cardKey)
			} else {
				s.byCard[cardKey] = live
			}
			delete(s.byID, partitionID)
			return true
		}
	}
	return false
}

// Reclaim deletes every virtual partition across all manufacturers whose PodUID is absent from
// livePodUIDs, returning the number of reclaimed partitions.
func (s *PartitionStore) Reclaim(livePodUIDs []string) int {
	return s.ReclaimManufacturer("", livePodUIDs, nil)
}

// ReclaimManufacturer deletes virtual partitions of manufacturer (or all manufacturers when
// manufacturer is empty) whose PodUID is absent from livePodUIDs, skipping any partition whose
// placement still overlaps an entry in occupied (attribution self-check). It returns the number
// of reclaimed partitions.
func (s *PartitionStore) ReclaimManufacturer(
	manufacturer string,
	livePodUIDs []string,
	occupied deviceplugin.Placements,
) int {
	if s == nil {
		return 0
	}
	live := sets.New[string](livePodUIDs...)

	s.mu.Lock()
	defer s.mu.Unlock()

	reclaimed := 0
	for cardKey, parts := range s.byCard {
		if manufacturer != "" && cardKey.manufacturer != manufacturer {
			continue
		}
		kept := parts[:0]
		for i := range parts {
			p := parts[i]
			if live.Has(p.PodUID) {
				kept = append(kept, p)
				continue
			}
			if occupied != nil {
				res := deviceplugin.Resource{Group: p.GroupID, Device: p.DeviceID}
				if placementOverlaps(p.Placement, occupied[res]) {
					kept = append(kept, p)
					continue
				}
			}
			delete(s.byID, p.ID)
			reclaimed++
		}
		if len(kept) == 0 {
			delete(s.byCard, cardKey)
		} else {
			s.byCard[cardKey] = kept
		}
	}
	return reclaimed
}

// Count returns the total number of live virtual partitions in the store.
func (s *PartitionStore) Count() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

// Get returns the live virtual partition with id, if present.
func (s *PartitionStore) Get(id string) (VirtualPartition, bool) {
	if s == nil {
		return VirtualPartition{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	return p, ok
}

// GetByOwner returns the live virtual partition owned by (podUID, container) on (manufacturer, deviceID).
func (s *PartitionStore) GetByOwner(manufacturer, podUID, container, deviceID string) (VirtualPartition, bool) {
	if s == nil {
		return VirtualPartition{}, false
	}
	cardKey := partitionCardKey{manufacturer: manufacturer, deviceID: deviceID}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.byCard[cardKey] {
		if p.PodUID == podUID && p.Container == container {
			return p, true
		}
	}
	return VirtualPartition{}, false
}

// List returns a deterministic snapshot of all live virtual partitions sorted by manufacturer,
// card index, placement start, and ID.
func (s *PartitionStore) List() []VirtualPartition {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	out := make([]VirtualPartition, 0, len(s.byID))
	for _, p := range s.byID {
		out = append(out, p)
	}
	s.mu.RUnlock()
	sortVirtualPartitions(out)
	return out
}

// ListByCard returns a deterministic snapshot of live virtual partitions on (manufacturer, deviceID)
// sorted by placement start.
func (s *PartitionStore) ListByCard(manufacturer, deviceID string) []VirtualPartition {
	if s == nil {
		return nil
	}
	cardKey := partitionCardKey{manufacturer: manufacturer, deviceID: deviceID}
	s.mu.RLock()
	out := slices.Clone(s.byCard[cardKey])
	s.mu.RUnlock()
	sortVirtualPartitions(out)
	return out
}

// ListByManufacturer returns a deterministic snapshot of live virtual partitions for manufacturer.
func (s *PartitionStore) ListByManufacturer(manufacturer string) []VirtualPartition {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	var out []VirtualPartition
	for key, parts := range s.byCard {
		if key.manufacturer == manufacturer {
			out = append(out, parts...)
		}
	}
	s.mu.RUnlock()
	sortVirtualPartitions(out)
	return out
}

// LookupPartition resolves live idle telemetry for a virtual partition addressed by req,
// satisfying pkg/devicemanager/detector/mock.PartitionProvider so live mock partitions
// automatically answer MonitorAcceleratorPartitions queries.
func (s *PartitionStore) LookupPartition(
	manufacturer string, req device.AcceleratorPartitionRequest,
) (device.AcceleratorPartition, bool) {
	if s == nil || req.ID == "" {
		return device.AcceleratorPartition{}, false
	}

	s.mu.RLock()
	vp, ok := s.byID[req.ID]
	s.mu.RUnlock()
	if !ok {
		return device.AcceleratorPartition{}, false
	}
	if manufacturer != "" && vp.Manufacturer != manufacturer {
		return device.AcceleratorPartition{}, false
	}
	if req.DeviceID != "" && vp.DeviceID != req.DeviceID {
		return device.AcceleratorPartition{}, false
	}
	if req.Profile != "" && vp.Profile != req.Profile {
		return device.AcceleratorPartition{}, false
	}
	if len(req.Placements) > 0 && !slices.Equal(req.Placements, []workercore.AcceleratorPlacement{vp.Placement}) {
		return device.AcceleratorPartition{}, false
	}

	var memTotalBytes *uint64
	if vp.MemoryMib > 0 {
		total := uint64(vp.MemoryMib) << 20
		memTotalBytes = &total
	}
	var memUsedBytes uint64

	ans := device.AcceleratorPartition{
		DeviceID:         vp.DeviceID,
		Profile:          cmp.Or(req.Profile, vp.Profile),
		Placements:       []workercore.AcceleratorPlacement{vp.Placement},
		ID:               vp.ID,
		MemoryTotalBytes: memTotalBytes,
		MemoryUsedBytes:  &memUsedBytes,
	}
	if vp.Manufacturer == nodefeature.ManufacturerHygon {
		var coresIdle uint32
		ans.CoresPercent = &coresIdle
	} else {
		ans.CoresReason = device.AcceleratorProcessReasonUnsupported
	}
	return ans, true
}

func sortVirtualPartitions(parts []VirtualPartition) {
	slices.SortFunc(parts, func(a, b VirtualPartition) int {
		if c := cmp.Compare(a.Manufacturer, b.Manufacturer); c != 0 {
			return c
		}
		if c := cmp.Compare(a.CardIndex, b.CardIndex); c != 0 {
			return c
		}
		if c := cmp.Compare(a.DeviceID, b.DeviceID); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Placement.Start, b.Placement.Start); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

func lookupPhysicalProfile(
	acc *workercore.Accelerator, profile string,
) (workercore.AcceleratorPhysicalSlicedProfile, bool) {
	if acc == nil || !device.IsPartitioned(acc.Status) {
		return workercore.AcceleratorPhysicalSlicedProfile{}, false
	}
	for i := range acc.Status.PhysicalSliced.Profiles {
		p := acc.Status.PhysicalSliced.Profiles[i]
		if p.Name == profile {
			return p, true
		}
	}
	return workercore.AcceleratorPhysicalSlicedProfile{}, false
}

func containsPlacement(
	candidates []workercore.AcceleratorPlacement, target workercore.AcceleratorPlacement,
) bool {
	for i := range candidates {
		if candidates[i].Start == target.Start && candidates[i].Length == target.Length {
			return true
		}
	}
	return false
}

func pickLowestFreePlacement(
	possible, occupied []workercore.AcceleratorPlacement,
) (workercore.AcceleratorPlacement, bool) {
	sorted := slices.Clone(possible)
	slices.SortFunc(sorted, func(a, b workercore.AcceleratorPlacement) int {
		return cmp.Compare(a.Start, b.Start)
	})
	for i := range sorted {
		slot := sorted[i]
		if slot.Length <= 0 {
			continue
		}
		if !placementOverlaps(slot, occupied) {
			return slot, true
		}
	}
	return workercore.AcceleratorPlacement{}, false
}

func placementOverlaps(
	slot workercore.AcceleratorPlacement, occupied []workercore.AcceleratorPlacement,
) bool {
	slotEnd := slot.Start + slot.Length
	for i := range occupied {
		occ := occupied[i]
		if slot.Start < occ.Start+occ.Length && occ.Start < slotEnd {
			return true
		}
	}
	return false
}

// reconcilerReclaimFunc builds the callback passed to deviceplugin.RunReclaimLoop.
// It unions the broadcast livePodUIDs with the current Pods listed on reconciler.NodeName so an
// in-flight Allocate broadcast (fired by reserveDevices before the Pod's allocation annotation is
// patched) never reclaims a newly created partition of an existing pending Pod, and checks
// reconciler.LivePhysicalOccupied as an attribution self-check.
func reconcilerReclaimFunc(
	ctx context.Context,
	logger klog.Logger,
	reconciler *deviceplugin.DevicesReconciler,
	store *PartitionStore,
	manufacturer string,
) func(livePodUIDs []string) {
	return func(livePodUIDs []string) {
		if store == nil {
			return
		}
		liveSet := sets.New[string](livePodUIDs...)
		var occupied deviceplugin.Placements
		if reconciler != nil && reconciler.Client != nil {
			podList := new(core.PodList)
			if err := reconciler.Client.List(
				ctx,
				podList,
				ctrlcli.MatchingFields{deviceplugin.IndexingPodsByNodeName: reconciler.NodeName},
				ctrlcli.UnsafeDisableDeepCopy,
			); err != nil {
				logger.Error(err, "reclaim: list node pods, skipping pass")
				return
			}
			for i := range podList.Items {
				liveSet.Insert(string(podList.Items[i].UID))
			}

			var err error
			occupied, err = reconciler.LivePhysicalOccupied(ctx)
			if err != nil {
				logger.Error(err, "reclaim: read live physical claims, skipping pass")
				return
			}
		}

		reclaimed := store.ReclaimManufacturer(manufacturer, sets.List(liveSet), occupied)
		if reclaimed > 0 {
			logger.Info("reclaimed dead pod virtual partitions", "count", reclaimed)
		}
	}
}
