package service

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/workergateway/manager"
)

// instanceTypePhaseActive is the InstanceType status phase that marks a candidate as schedulable.
// Only Active candidates contribute to the aggregated OnceMaxRequest/Remaining totals; non-Active
// candidates stay listed but count as zero.
//
// It must stay in lockstep with the "Active" summary produced by apistatus.GetSummaryOfClusterQueue
// (and the InstanceType phase constants in the worker controllers); the literal is duplicated here
// because the workergateway is a separate binary with no dependency on those packages.
const instanceTypePhaseActive = "Active"

// normalizeAggregatedInstanceTypeSpec zeroes the fields that must not split the cross-cluster
// grouping identity. Inactive, DisplayName and Description describe an administrative/presentation
// state that varies per cluster, so the same hardware collapses into one aggregated item regardless
// of them.
func normalizeAggregatedInstanceTypeSpec(spec AggregatedInstanceTypeSpec) AggregatedInstanceTypeSpec {
	spec.Inactive = false
	spec.DisplayName = ""
	spec.Description = ""
	return spec
}

// buildAggregatedInstanceTypeName derives a stable, spec-only identity so the same hardware profile
// reads identically across clusters regardless of which cluster's InstanceType (or its arbitrary
// object name) was seen first. Accelerated types encode the accelerator group and the per-unit CPU;
// a non-accelerated type omits both (its unit CPU is webhook-fixed to 1). The Gi suffix on
// RAM/LocalStorage is stripped best-effort, keeping the raw value when the suffix is absent.
func buildAggregatedInstanceTypeName(spec AggregatedInstanceTypeSpec) string {
	ram := strings.TrimSuffix(spec.UnitResources.RAM, "Gi")
	localStorage := strings.TrimSuffix(spec.LocalStorage, "Gi")
	if spec.Acceleratable {
		return fmt.Sprintf("%s-%s-%s-%s-%sc-%sg-%sg",
			spec.GeneralGroup, spec.AcceleratorGroup, spec.OS, spec.Arch,
			spec.UnitResources.CPU, ram, localStorage)
	}
	return fmt.Sprintf("%s-%s-%s-%sg-%sg", spec.GeneralGroup, spec.OS, spec.Arch, ram, localStorage)
}

type ListClusterInstanceTypeFlavors struct {
	list ClusterInstanceTypeFlavorList
}

func OpListClusterInstanceTypeFlavors() *ListClusterInstanceTypeFlavors {
	return &ListClusterInstanceTypeFlavors{
		list: ClusterInstanceTypeFlavorList{
			Items: make([]ClusterInstanceTypeFlavor, 0),
		},
	}
}

func (in *ListClusterInstanceTypeFlavors) Next(cluster string, obj runtime.Object) error {
	flavor, ok := obj.(*worker.InstanceTypeFlavor)
	if !ok {
		return fmt.Errorf("object is not of type InstanceTypeFlavor")
	}

	item := ClusterInstanceTypeFlavor{
		InstanceTypeFlavor: *flavor,
		Cluster:            cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstanceTypeFlavors) Result() ClusterInstanceTypeFlavorList {
	return in.list
}

type ListAggregateInstanceTypeFlavors struct {
	list        AggregatedInstanceTypeFlavorList
	itemIndexer map[worker.InstanceTypeFlavorSpec]int
}

func OpListAggregateInstanceTypeFlavors() *ListAggregateInstanceTypeFlavors {
	return &ListAggregateInstanceTypeFlavors{
		list: AggregatedInstanceTypeFlavorList{
			Items: make([]AggregatedInstanceTypeFlavor, 0),
		},
		itemIndexer: make(map[worker.InstanceTypeFlavorSpec]int),
	}
}

// Next deduplicates flavors by Spec across clusters, so identical pools served by several
// clusters collapse to one entry. The Name is carried from the cluster-computed flavor, which
// is deterministic from the Spec. Every cluster contributing the flavor is recorded in
// Spec.Clusters; Result sorts that list so the observable order is deterministic.
func (in *ListAggregateInstanceTypeFlavors) Next(cluster string, obj runtime.Object) error {
	flavor, ok := obj.(*worker.InstanceTypeFlavor)
	if !ok {
		return fmt.Errorf("object is not of type InstanceTypeFlavor")
	}

	itemIndex, existed := in.itemIndexer[flavor.Spec]
	if !existed {
		itemIndex = len(in.list.Items)
		in.itemIndexer[flavor.Spec] = itemIndex
		in.list.Items = append(in.list.Items, AggregatedInstanceTypeFlavor{
			Name: flavor.Name,
			Spec: AggregatedInstanceTypeFlavorSpec{
				InstanceTypeFlavorSpec: flavor.Spec,
				Clusters:               make([]string, 0),
			},
		})
	}

	item := &in.list.Items[itemIndex]
	item.Spec.Clusters = append(item.Spec.Clusters, cluster)
	return nil
}

// Result returns the aggregated flavor list. When sorted, accelerated flavors come first, then
// ascending by Name, which is deterministic within both the accelerated and generic groups.
// Each item's Clusters are always sorted ascending so the list is deterministic regardless of
// cluster iteration order.
func (in *ListAggregateInstanceTypeFlavors) Result(sorted bool) AggregatedInstanceTypeFlavorList {
	if sorted {
		sort.Slice(in.list.Items, func(i, j int) bool {
			a, b := in.list.Items[i], in.list.Items[j]
			if a.Spec.Acceleratable != b.Spec.Acceleratable {
				return a.Spec.Acceleratable
			}
			return a.Name < b.Name
		})
	}

	for i := range in.list.Items {
		sort.Strings(in.list.Items[i].Spec.Clusters)
	}
	return in.list
}

type HandleClusterInstanceTypeFlavor struct{}

func OpHandleClusterInstanceTypeFlavor() *HandleClusterInstanceTypeFlavor {
	return &HandleClusterInstanceTypeFlavor{}
}

func (in *HandleClusterInstanceTypeFlavor) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		flavor, ok := evt.Object.(*worker.InstanceTypeFlavor)
		if !ok {
			return nil
		}
		if flavor.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterFlavor := &ClusterInstanceTypeFlavor{
			InstanceTypeFlavor: *flavor,
			Cluster:            evt.Cluster,
		}
		clusterFlavor.ManagedFields = nil
		evt.Object = clusterFlavor
	}
	return []*manager.WorkerEvent{evt}
}

// HandleAggregatedInstanceTypeFlavor maintains the deduplicated, cross-cluster flavor catalog for a
// watch stream, mirroring the OpListAggregateInstanceTypeFlavors grouping: flavors are grouped by
// their full Spec and each group records its contributing clusters. Unlike the InstanceType stream
// there are no tiers or candidate math — a group is created when its first cluster contributes
// (Added), updated as clusters join or leave (Modified), and removed when its last cluster leaves
// (Deleted).
//
// Each event is self-contained: the worker's flavor watch keys on the full Spec, so a spec change
// arrives as a Deleted (carrying the old Spec) followed by an Added (the new Spec) rather than a
// single in-place Modified. The group is therefore located from the event's own Spec, with no need
// to track prior per-flavor membership.
type HandleAggregatedInstanceTypeFlavor struct {
	state AggregatedInstanceTypeFlavorList
}

func OpHandleAggregatedInstanceTypeFlavor(state AggregatedInstanceTypeFlavorList) *HandleAggregatedInstanceTypeFlavor {
	return &HandleAggregatedInstanceTypeFlavor{
		state: state,
	}
}

// groupIndex returns the index of the aggregated item holding spec, or -1. The catalog is small
// (one entry per distinct hardware pool), so a linear scan avoids an index map that item removals
// would invalidate.
func (in *HandleAggregatedInstanceTypeFlavor) groupIndex(spec worker.InstanceTypeFlavorSpec) int {
	for i := range in.state.Items {
		if in.state.Items[i].Spec.InstanceTypeFlavorSpec == spec {
			return i
		}
	}
	return -1
}

func (in *HandleAggregatedInstanceTypeFlavor) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	var evts []*manager.WorkerEvent

	if evt.Type == manager.WorkerEventDeleted && evt.Object == nil {
		// Drop the cluster from every group it contributes to. Splice deleted items in place and
		// step the index back so a Modified event's pointer (into an earlier, unshifted slot) stays
		// valid until the caller encodes it.
		for i := 0; i < len(in.state.Items); i++ {
			item := &in.state.Items[i]
			kept := dropCluster(item.Spec.Clusters, evt.Cluster)
			if len(kept) == len(item.Spec.Clusters) {
				continue
			}
			if len(kept) == 0 {
				deleted := deletedFlavor(item)
				in.state.Items = append(in.state.Items[:i], in.state.Items[i+1:]...)
				i--
				evts = append(evts, &manager.WorkerEvent{
					Type:   manager.WorkerEventDeleted,
					Object: deleted,
				})
				continue
			}
			item.Spec.Clusters = kept
			evts = append(evts, &manager.WorkerEvent{
				Type:   manager.WorkerEventModified,
				Object: item,
			})
		}
		return evts
	}

	flavor, ok := evt.Object.(*worker.InstanceTypeFlavor)
	if !ok {
		return nil
	}
	if flavor.DeletionTimestamp != nil {
		evt.Type = manager.WorkerEventDeleted
	}

	if evt.Type == manager.WorkerEventDeleted {
		// The deleted flavor object carries the Spec it contributed, so its group is found directly.
		return in.removeCluster(evt.Cluster, flavor.Spec)
	}

	// Added or Modified: ensure the cluster contributes to the flavor's group.
	return in.addCluster(evt.Cluster, flavor.Name, flavor.Spec)
}

// addCluster records cluster as a contributor to spec's group, creating the group (Added) when it is
// the first contributor or extending the cluster list (Modified). A cluster already present yields
// no event. Clusters are kept sorted so the observable order is deterministic.
func (in *HandleAggregatedInstanceTypeFlavor) addCluster(cluster, name string, spec worker.InstanceTypeFlavorSpec) []*manager.WorkerEvent {
	idx := in.groupIndex(spec)
	if idx == -1 {
		in.state.Items = append(in.state.Items, AggregatedInstanceTypeFlavor{
			Name: name,
			Spec: AggregatedInstanceTypeFlavorSpec{
				InstanceTypeFlavorSpec: spec,
				Clusters:               []string{cluster},
			},
		})
		return []*manager.WorkerEvent{{
			Type:   manager.WorkerEventAdded,
			Object: &in.state.Items[len(in.state.Items)-1],
		}}
	}

	item := &in.state.Items[idx]
	for _, c := range item.Spec.Clusters {
		if c == cluster {
			return nil
		}
	}
	item.Spec.Clusters = append(item.Spec.Clusters, cluster)
	sort.Strings(item.Spec.Clusters)
	return []*manager.WorkerEvent{{
		Type:   manager.WorkerEventModified,
		Object: item,
	}}
}

// removeCluster drops cluster from spec's group, deleting the group (Deleted) when it was the last
// contributor or shrinking the cluster list (Modified). A cluster that was not a contributor, or an
// unknown group, yields no event.
func (in *HandleAggregatedInstanceTypeFlavor) removeCluster(cluster string, spec worker.InstanceTypeFlavorSpec) []*manager.WorkerEvent {
	idx := in.groupIndex(spec)
	if idx == -1 {
		return nil
	}

	item := &in.state.Items[idx]
	kept := dropCluster(item.Spec.Clusters, cluster)
	if len(kept) == len(item.Spec.Clusters) {
		return nil
	}
	if len(kept) == 0 {
		deleted := deletedFlavor(item)
		in.state.Items = append(in.state.Items[:idx], in.state.Items[idx+1:]...)
		return []*manager.WorkerEvent{{
			Type:   manager.WorkerEventDeleted,
			Object: deleted,
		}}
	}
	item.Spec.Clusters = kept
	return []*manager.WorkerEvent{{
		Type:   manager.WorkerEventModified,
		Object: item,
	}}
}

// deletedFlavor builds the payload for a DELETED event: the removed group's Name plus its grouping
// Spec. The Spec is carried because a flavor name is derived from group identity alone and can
// collide across distinct InstanceTypeFlavorSpecs, so Name by itself would not let a watch consumer
// tell which group was removed. Clusters is an empty (non-nil) slice — none remain, but the field
// has no omitempty, so a non-nil slice keeps it serializing as [] like every other event rather than
// null. It copies the identity, so the caller may splice the source item away immediately after.
func deletedFlavor(item *AggregatedInstanceTypeFlavor) *AggregatedInstanceTypeFlavor {
	return &AggregatedInstanceTypeFlavor{
		Name: item.Name,
		Spec: AggregatedInstanceTypeFlavorSpec{
			InstanceTypeFlavorSpec: item.Spec.InstanceTypeFlavorSpec,
			Clusters:               []string{},
		},
	}
}

// dropCluster returns clusters with cluster removed, preserving order so a sorted list stays sorted.
// The result shares no backing array with the input.
func dropCluster(clusters []string, cluster string) []string {
	kept := make([]string, 0, len(clusters))
	for _, c := range clusters {
		if c != cluster {
			kept = append(kept, c)
		}
	}
	return kept
}

// lessTierByPrimary returns whether tier i should come before tier j when sorting
// ascending by the primary dimension: Accelerator if Spec.Acceleratable, otherwise CPU.
// Bind as item.lessTierByPrimary and pass to sort.Slice for consistent ordering.
func (in *AggregatedInstanceType) lessTierByPrimary(i, j int) bool {
	if in.Spec.Acceleratable {
		return in.Status.Tiers[i].OnceMaxRequest.Accelerator.Cmp(in.Status.Tiers[j].OnceMaxRequest.Accelerator) < 0
	}
	return in.Status.Tiers[i].OnceMaxRequest.CPU.Cmp(in.Status.Tiers[j].OnceMaxRequest.CPU) < 0
}

// Recompute rebuilds the item-level OnceMaxRequest and Remaining overviews from the tiers.
//
// OnceMaxRequest is the resource bundle of the tier whose primary dimension is the largest.
// The primary dimension is Accelerator when Spec.Acceleratable is true, otherwise CPU.
// The whole bundle (Accelerator/CPU/AcceleratorShared/AcceleratorSliced) is copied from the winning tier
// so the result corresponds to a real, achievable single allocation rather than a
// per-dimension maximum across tiers.
//
// The first tier seeds the bundle; a later tier replaces it only when it wins on the
// primary dimension. Seeding guarantees a real bundle is picked even when the primary
// dimension is zero (e.g. a fully-sliced accelerator whose whole-card OnceMaxRequest is
// zero but AcceleratorSliced is not).
//
// Remaining is the per-dimension sum across all tiers, representing the total requestable
// resources of the AggregatedInstanceType. It is an aggregate and may not be achievable
// in a single allocation.
func (in *AggregatedInstanceType) Recompute() {
	var newOnceMaxRequest, newRemaining AggregatedInstanceTypeOverviewResource

	seeded := false
	for i := range in.Status.Tiers {
		tier := &in.Status.Tiers[i]

		// A tier whose bundle is entirely zero (e.g. one holding only inactive candidates, since
		// tier Recompute zeroes those) offers no single allocation, so it must not seed the winner:
		// otherwise it would mask a real bundle from a tier that ties it on the primary dimension
		// (both zero), as a fully-sliced active tier does. Such a tier still adds its (zero)
		// Remaining, so excluding it from the seed changes nothing but the OnceMaxRequest bundle.
		if !overviewResourceIsZero(tier.OnceMaxRequest) {
			wins := !seeded
			if !wins {
				if in.Spec.Acceleratable {
					wins = newOnceMaxRequest.Accelerator.Cmp(tier.OnceMaxRequest.Accelerator) < 0
				} else {
					wins = newOnceMaxRequest.CPU.Cmp(tier.OnceMaxRequest.CPU) < 0
				}
			}
			if wins {
				newOnceMaxRequest = tier.OnceMaxRequest
			}
			seeded = true
		}

		newRemaining.Accelerator.Add(tier.Remaining.Accelerator)
		newRemaining.CPU.Add(tier.Remaining.CPU)
		newRemaining.AcceleratorShared.Add(tier.Remaining.AcceleratorShared)
		newRemaining.AcceleratorSliced.Add(tier.Remaining.AcceleratorSliced)
	}

	in.Status.OnceMaxRequest = newOnceMaxRequest
	in.Status.Remaining = newRemaining

	// Fold the whole-fleet slicing view into Detail.SlicedDetail: the direct Σ of every tier's slicing
	// capability. The identity fields are owned by adoptDetailIdentity (set at ingestion) and left
	// untouched here; only SlicedDetail is rebuilt, from a fresh sum that aliases no tier's slice.
	var newSliced workercore.AcceleratorSlicedDetail
	profileIndex := make(map[string]int)
	for i := range in.Status.Tiers {
		addAcceleratorSlicedDetail(&newSliced, in.Status.Tiers[i].AcceleratorSlicedDetail, profileIndex)
	}
	in.Status.Detail.SlicedDetail = newSliced
}

// overviewResourceIsZero reports whether every dimension of an overview bundle is zero, i.e. the
// bundle offers no single allocation on any dimension.
func overviewResourceIsZero(r AggregatedInstanceTypeOverviewResource) bool {
	return r.Accelerator.IsZero() && r.AcceleratorShared.IsZero() &&
		r.AcceleratorSliced.IsZero() && r.CPU.IsZero()
}

// Recompute rebuilds the tier-level OnceMaxRequest and Remaining overviews from the candidates.
//
// OnceMaxRequest is the resource bundle of the candidate whose primary dimension is the largest.
// The primary dimension is Accelerator when acceleratable is true, otherwise CPU.
// All candidates in one tier share the same accelerator OnceMaxRequest, so the
// acceleratable branch effectively picks the first candidate; the CPU branch is the
// one that does real work for CPU-only items where the per-candidate CPU may vary.
//
// The first candidate seeds the bundle; a later candidate replaces it only when it wins
// on the primary dimension. Seeding guarantees a real bundle is picked even when the
// primary dimension is zero (e.g. a fully-sliced accelerator whose whole-card
// OnceMaxRequest is zero but AcceleratorSliced is not).
//
// Remaining is the per-dimension sum across all candidates, representing the total
// requestable resources of the tier. It is an aggregate and may not be achievable in
// a single allocation.
func (in *AggregatedInstanceTypeOnceMaxRequestTier) Recompute(acceleratable bool) {
	var newOnceMaxRequest, newRemaining AggregatedInstanceTypeOverviewResource

	seeded := false
	for i := range in.Candidates {
		candidate := &in.Candidates[i]

		// Only Active candidates count toward the totals; a non-Active candidate stays listed but
		// contributes zero, so an inactive/transitioning pool never inflates the fleet overview.
		if candidate.Phase != instanceTypePhaseActive {
			continue
		}

		// The first Active candidate seeds the bundle so a real bundle is picked even when the
		// primary dimension is zero (e.g. a fully-sliced accelerator).
		wins := !seeded
		if !wins {
			if acceleratable {
				wins = newOnceMaxRequest.Accelerator.Cmp(candidate.Accelerator.OnceMaxRequest) < 0
			} else {
				wins = newOnceMaxRequest.CPU.Cmp(candidate.CPU.OnceMaxRequest) < 0
			}
		}
		if wins {
			newOnceMaxRequest = AggregatedInstanceTypeOverviewResource{
				Accelerator:       candidate.Accelerator.OnceMaxRequest,
				CPU:               candidate.CPU.OnceMaxRequest,
				AcceleratorShared: candidate.AcceleratorShared.OnceMaxRequest,
				AcceleratorSliced: candidate.AcceleratorSliced.OnceMaxRequest,
			}
		}
		seeded = true

		newRemaining.Accelerator.Add(candidate.Accelerator.Remaining)
		newRemaining.CPU.Add(candidate.CPU.Remaining)
		newRemaining.AcceleratorShared.Add(candidate.AcceleratorShared.Remaining)
		newRemaining.AcceleratorSliced.Add(candidate.AcceleratorSliced.Remaining)
	}

	in.OnceMaxRequest = newOnceMaxRequest
	in.Remaining = newRemaining

	// Sum every candidate's slicing capability by profile name into the tier-level view — a pure
	// capability aggregate, so unlike OnceMaxRequest/Remaining it counts all candidates regardless of
	// Phase. The item folds these tier sums into Status.Detail.SlicedDetail.
	var newSliced workercore.AcceleratorSlicedDetail
	profileIndex := make(map[string]int)
	for i := range in.Candidates {
		addAcceleratorSlicedDetail(&newSliced, in.Candidates[i].AcceleratorSlicedDetail, profileIndex)
	}
	in.AcceleratorSlicedDetail = newSliced
}

// newAggregatedTier builds a candidate-less tier seeded from an InstanceType's status: OnceMaxRequest/
// Remaining are the candidate's raw values (Recompute later replaces them with the tier aggregate).
// A tier groups candidates by accelerator OnceMaxRequest and carries no identity — the observed
// descriptor lives only on the item Status.Detail. The caller appends candidates.
func newAggregatedTier(instType *worker.InstanceType) AggregatedInstanceTypeOnceMaxRequestTier {
	return AggregatedInstanceTypeOnceMaxRequestTier{
		OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
			Accelerator:       instType.Status.Accelerator.OnceMaxRequest,
			CPU:               instType.Status.CPU.OnceMaxRequest,
			AcceleratorShared: instType.Status.AcceleratorShared.OnceMaxRequest,
			AcceleratorSliced: instType.Status.AcceleratorSliced.OnceMaxRequest,
		},
		Remaining: AggregatedInstanceTypeOverviewResource{
			Accelerator:       instType.Status.Accelerator.Remaining,
			CPU:               instType.Status.CPU.Remaining,
			AcceleratorShared: instType.Status.AcceleratorShared.Remaining,
			AcceleratorSliced: instType.Status.AcceleratorSliced.Remaining,
		},
	}
}

// adoptDetailIdentity maintains the item's observed descriptor identity at the status level. It adopts
// an ingested candidate's descriptor whenever that candidate has reconciled hardware (a non-empty
// Manufacturer, the API's not-yet-synced signal), so a descriptor first seen during the pre-reconcile
// window self-heals as soon as any candidate reports its hardware. Identity is uniform across an
// item's candidates (same hardware), so adopting any ready one is deterministic; the existing
// SlicedDetail Σ (owned by Recompute) is preserved.
func adoptDetailIdentity(status *AggregatedInstanceTypeStatus, instType *worker.InstanceType) {
	if !instType.Status.Detail.AcceleratorReady() {
		return
	}
	sliced := status.Detail.SlicedDetail
	status.Detail = instType.Status.Detail
	status.Detail.SlicedDetail = sliced
}

// newAggregatedCandidate builds a candidate from an InstanceType's status, carrying its observed
// slicing capability (Status.Detail.SlicedDetail) so the tier and item can sum it by profile name.
func newAggregatedCandidate(cluster, name string, instType *worker.InstanceType) AggregatedInstanceTypeOnceMaxRequestCandidate {
	return AggregatedInstanceTypeOnceMaxRequestCandidate{
		Cluster:                 cluster,
		Name:                    name,
		Phase:                   instType.Status.Phase,
		Accelerator:             instType.Status.Accelerator,
		CPU:                     instType.Status.CPU,
		AcceleratorShared:       instType.Status.AcceleratorShared,
		AcceleratorSliced:       instType.Status.AcceleratorSliced,
		AcceleratorSlicedDetail: instType.Status.Detail.SlicedDetail,
	}
}

// addAcceleratorSlicedDetail folds src into dst by direct summation: logical/physical counts add,
// physical profiles sum by name (profileIndex tracks each name's slot in dst.Physical.Profiles), and
// the logical overcommit flag is OR-ed from any logically sliceable contributor. It lifts the detector's
// card→group aggregation (device.AggregateAcceleratorSlicedDetail) one level up, so a tier/item
// slicing view is the plain sum of its members' capability, independent of candidate Phase.
func addAcceleratorSlicedDetail(dst *workercore.AcceleratorSlicedDetail, src workercore.AcceleratorSlicedDetail, profileIndex map[string]int) {
	dst.Logical.Count += src.Logical.Count
	if src.Logical.Count > 0 && src.Logical.CoresPercentageOvercommit {
		dst.Logical.CoresPercentageOvercommit = true
	}

	dst.Physical.Count += src.Physical.Count
	for _, p := range src.Physical.Profiles {
		if idx, ok := profileIndex[p.Name]; ok {
			dst.Physical.Profiles[idx].Count += p.Count
			continue
		}
		profileIndex[p.Name] = len(dst.Physical.Profiles)
		dst.Physical.Profiles = append(dst.Physical.Profiles, workercore.AcceleratorSlicedPhysicalDetailProfile{
			Name:  p.Name,
			Count: p.Count,
		})
	}
}

type ListAggregateInstanceTypes struct {
	list            AggregatedInstanceTypeList
	itemIndexer     map[AggregatedInstanceTypeSpec]int
	itemTierIndexer []map[string]int
}

func OpListAggregateInstanceTypes() *ListAggregateInstanceTypes {
	return &ListAggregateInstanceTypes{
		list: AggregatedInstanceTypeList{
			Items: make([]AggregatedInstanceType, 0),
		},
		itemIndexer: make(map[AggregatedInstanceTypeSpec]int),
	}
}

func (in *ListAggregateInstanceTypes) Next(cluster string, obj runtime.Object) error {
	instType, ok := obj.(*worker.InstanceType)
	if !ok {
		return fmt.Errorf("object is not of type InstanceType")
	}

	// Group by a normalized spec so the same hardware collapses across clusters regardless of
	// per-cluster Inactive/DisplayName; the stored item keeps the first-seen (full) spec.
	itemKey := normalizeAggregatedInstanceTypeSpec(instType.Spec)
	itemIndex, existed := in.itemIndexer[itemKey]
	if !existed {
		itemIndex = len(in.list.Items)
		in.itemIndexer[itemKey] = itemIndex
		in.itemTierIndexer = append(in.itemTierIndexer, make(map[string]int))
		item := AggregatedInstanceType{
			Name: buildAggregatedInstanceTypeName(instType.Spec),
			Spec: instType.Spec,
		}
		in.list.Items = append(in.list.Items, item)
	}

	item := &in.list.Items[itemIndex]
	tierIndexer := in.itemTierIndexer[itemIndex]

	// Maintain the item descriptor identity at the status level from any reconciled candidate.
	adoptDetailIdentity(&item.Status, instType)

	tierIndexKey := instType.Status.Accelerator.OnceMaxRequest.String()
	tierIndex, existed := tierIndexer[tierIndexKey]
	if !existed {
		tierIndex = len(item.Status.Tiers)
		tierIndexer[tierIndexKey] = tierIndex
		item.Status.Tiers = append(item.Status.Tiers, newAggregatedTier(instType))
	}

	tier := &item.Status.Tiers[tierIndex]
	tier.Candidates = append(tier.Candidates, newAggregatedCandidate(cluster, instType.Name, instType))

	return nil
}

func (in *ListAggregateInstanceTypes) Result(sorted bool) AggregatedInstanceTypeList {
	if sorted {
		// Sorted by acceleratable and name for better readability.
		sort.Slice(in.list.Items, func(i, j int) bool {
			if in.list.Items[i].Spec.Acceleratable == in.list.Items[j].Spec.Acceleratable && in.list.Items[i].Spec.Acceleratable {
				return in.list.Items[i].Name < in.list.Items[j].Name
			}
			return in.list.Items[i].Spec.Acceleratable
		})
	}

	for i := range in.list.Items {
		item := &in.list.Items[i]

		// Recompute each tier first so Phase filtering is reflected before ordering: an all-inactive
		// tier recomputes to zero and must sort by that, not by its raw first-candidate seed.
		for j := range item.Status.Tiers {
			tier := &item.Status.Tiers[j]
			tier.Recompute(item.Spec.Acceleratable)
		}

		// Sorted ascending by the primary dimension for better readability.
		sort.Slice(item.Status.Tiers, item.lessTierByPrimary)

		// Calculate the once max request of the item.
		item.Recompute()
	}
	return in.list
}

type HandleAggregatedInstanceType struct {
	state AggregatedInstanceTypeList
}

func OpHandleAggregatedInstanceType(state AggregatedInstanceTypeList) *HandleAggregatedInstanceType {
	return &HandleAggregatedInstanceType{
		state: state,
	}
}

func (in *HandleAggregatedInstanceType) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	var evts []*manager.WorkerEvent

	if evt.Type == manager.WorkerEventDeleted && evt.Object == nil {
		// Delete all instance types of the cluster.
		for i := 0; i < len(in.state.Items); i++ {
			item := &in.state.Items[i]
			itemChanged := false
			for j := 0; j < len(item.Status.Tiers); j++ {
				tier := &item.Status.Tiers[j]
				tierChanged := false
				// Keep the candidates of other clusters and delete the candidates of the cluster.
				newCandidates := make([]AggregatedInstanceTypeOnceMaxRequestCandidate, 0, len(tier.Candidates))
				for k := range tier.Candidates {
					candidate := tier.Candidates[k]
					if candidate.Cluster != evt.Cluster {
						newCandidates = append(newCandidates, tier.Candidates[k])
						continue
					}
					tierChanged = true
				}
				if !tierChanged {
					continue
				}
				itemChanged = true
				if len(newCandidates) == 0 {
					item.Status.Tiers = append(item.Status.Tiers[:j], item.Status.Tiers[j+1:]...)
					j--
					continue
				}
				tier.Candidates = newCandidates

				// Recompute the tier.
				tier.Recompute(item.Spec.Acceleratable)
			}
			if !itemChanged {
				continue
			}
			if len(item.Status.Tiers) == 0 {
				itemName := item.Name
				in.state.Items = append(in.state.Items[:i], in.state.Items[i+1:]...)
				i--

				// Report a deleted event.
				evts = append(evts, &manager.WorkerEvent{
					Type:   manager.WorkerEventDeleted,
					Object: &AggregatedInstanceType{Name: itemName},
				})
				continue
			}

			// Recompute the item.
			item.Recompute()

			// Report a modified event.
			evts = append(evts, &manager.WorkerEvent{
				Type:   manager.WorkerEventModified,
				Object: item,
			})
		}

		return evts
	}

	instType, ok := evt.Object.(*worker.InstanceType)
	if !ok {
		return nil
	}
	if instType.DeletionTimestamp != nil {
		evt.Type = manager.WorkerEventDeleted
	}

	index := [3]int{-1, -1, -1} // item index, tier index, candidate index

	// Match items on the normalized spec so a candidate whose only difference is Inactive/DisplayName
	// still lands on the aggregated item that groups the same hardware.
	instTypeKey := normalizeAggregatedInstanceTypeSpec(instType.Spec)
	for i := 0; i < len(in.state.Items); i++ {
		if normalizeAggregatedInstanceTypeSpec(in.state.Items[i].Spec) != instTypeKey {
			continue
		}
		index[0] = i

		item := &in.state.Items[index[0]]

		// Maintain the item descriptor identity at the status level from any reconciled candidate; a
		// delete carries no fresher hardware, so it never changes the descriptor.
		if evt.Type != manager.WorkerEventDeleted {
			adoptDetailIdentity(&item.Status, instType)
		}

		for j := 0; j < len(item.Status.Tiers); j++ {
			tier := &item.Status.Tiers[j]
			// Match tiers on a candidate's raw accelerator OnceMaxRequest (which Recompute never
			// mutates), not the tier's recomputed OnceMaxRequest, since Phase filtering may zero the
			// latter for an all-inactive tier and would then spawn a duplicate tier.
			if tier.Candidates[0].Accelerator.OnceMaxRequest.Equal(instType.Status.Accelerator.OnceMaxRequest) {
				index[1] = j
			}
			for k := range tier.Candidates {
				if tier.Candidates[k].Cluster == evt.Cluster && tier.Candidates[k].Name == instType.Name {
					index[1] = j
					index[2] = k
					break
				}
			}
			if index[2] != -1 {
				break
			}
		}

		// Found the same candidate.
		if index[2] != -1 {
			tier := &item.Status.Tiers[index[1]]

			if evt.Type == manager.WorkerEventDeleted {
				// Remove candidate from the original tier.
				tier.Candidates = append(tier.Candidates[:index[2]], tier.Candidates[index[2]+1:]...)

				// Delete the original tier if no candidate.
				if len(tier.Candidates) == 0 {
					item.Status.Tiers = append(item.Status.Tiers[:index[1]], item.Status.Tiers[index[1]+1:]...)
				} else {
					// Recompute the tier.
					tier.Recompute(item.Spec.Acceleratable)
				}

				// Delete the original item if no tier.
				if len(item.Status.Tiers) == 0 {
					itemName := item.Name
					in.state.Items = append(in.state.Items[:index[0]], in.state.Items[index[0]+1:]...)

					//  Report a deleted event also.
					evts = append(evts, &manager.WorkerEvent{
						Type:   manager.WorkerEventDeleted,
						Object: &AggregatedInstanceType{Name: itemName},
					})
				} else {
					// Recompute the item.
					item.Recompute()

					// Report a modified event.
					evts = append(evts, &manager.WorkerEvent{
						Type:   manager.WorkerEventModified,
						Object: item,
					})
				}

				// Return immediately since the deleted candidate will not be moved to another tier.
				return evts
			}

			candidate := newAggregatedCandidate(evt.Cluster, instType.Name, instType)

			if tier.Candidates[0].Accelerator.OnceMaxRequest.Equal(candidate.Accelerator.OnceMaxRequest) {
				// If the once max request has not changed, update the candidate in place.
				tier.Candidates[index[2]] = candidate

				// Recompute the tier.
				tier.Recompute(item.Spec.Acceleratable)
			} else {
				// Remove candidate from the original tier.
				tier.Candidates = append(tier.Candidates[:index[2]], tier.Candidates[index[2]+1:]...)

				// Delete the original tier if no candidate.
				if len(tier.Candidates) == 0 {
					item.Status.Tiers = append(item.Status.Tiers[:index[1]], item.Status.Tiers[index[1]+1:]...)
				} else {
					// Recompute the tier.
					tier.Recompute(item.Spec.Acceleratable)
				}

				// Find the new tier to move in, matching on a candidate's raw accelerator OnceMaxRequest
				// so a Phase-zeroed tier still resolves to its stable identity.
				newTierIndex := -1
				for j := 0; j < len(item.Status.Tiers); j++ {
					if item.Status.Tiers[j].Candidates[0].Accelerator.OnceMaxRequest.Equal(instType.Status.Accelerator.OnceMaxRequest) {
						newTierIndex = j
						break
					}
				}
				if newTierIndex != -1 {
					// Move to the new tier.
					tier = &item.Status.Tiers[newTierIndex]
					tier.Candidates = append(tier.Candidates, candidate)

					// Recompute the tier.
					tier.Recompute(item.Spec.Acceleratable)
				} else {
					// Append a new tier if not found.
					newTier := newAggregatedTier(instType)
					newTier.Candidates = []AggregatedInstanceTypeOnceMaxRequestCandidate{candidate}
					item.Status.Tiers = append(item.Status.Tiers, newTier)

					// Recompute the just-appended tier so a moved non-Active candidate does not leak
					// its raw capacity into the item overview, and the sort key is Phase-filtered.
					item.Status.Tiers[len(item.Status.Tiers)-1].Recompute(item.Spec.Acceleratable)

					// Sorted ascending by the primary dimension.
					sort.Slice(item.Status.Tiers, item.lessTierByPrimary)
				}
			}

			// Recompute the item.
			item.Recompute()

			// Report a modified event.
			evts = append(evts, &manager.WorkerEvent{
				Type:   manager.WorkerEventModified,
				Object: item,
			})

			return evts
		}

		// Found the same tier, but not in any candidate.
		if index[1] != -1 {
			if evt.Type == manager.WorkerEventDeleted {
				// The candidate to delete does not exist; nothing to do.
				return evts
			}

			tier := &item.Status.Tiers[index[1]]

			tier.Candidates = append(tier.Candidates, newAggregatedCandidate(evt.Cluster, instType.Name, instType))

			// Recompute the tier.
			tier.Recompute(item.Spec.Acceleratable)

			// Recompute the item.
			item.Recompute()

			// Report a modified event.
			evts = append(evts, &manager.WorkerEvent{
				Type:   manager.WorkerEventModified,
				Object: item,
			})

			return evts
		}

		// Found the same item, but not in any tier.
		break
	}

	if evt.Type != manager.WorkerEventDeleted {
		tier := newAggregatedTier(instType)
		tier.Candidates = []AggregatedInstanceTypeOnceMaxRequestCandidate{
			newAggregatedCandidate(evt.Cluster, instType.Name, instType),
		}

		// Found the same item but not in any tier.
		if index[0] != -1 {
			item := &in.state.Items[index[0]]
			item.Status.Tiers = append(item.Status.Tiers, tier)

			// Recompute the just-appended tier so Phase filtering applies before sorting/aggregation.
			item.Status.Tiers[len(item.Status.Tiers)-1].Recompute(item.Spec.Acceleratable)

			// Sorted ascending by the primary dimension.
			sort.Slice(item.Status.Tiers, item.lessTierByPrimary)

			// Recompute the item.
			item.Recompute()

			// Report a modified event.
			evts = append(evts, &manager.WorkerEvent{
				Type:   manager.WorkerEventModified,
				Object: item,
			})

			return evts
		}

		// Recompute the tier so a non-Active first candidate does not seed the new item's overview,
		// and its AcceleratorSlicedDetail holds the candidate's slicing sum before it is folded up.
		tier.Recompute(instType.Spec.Acceleratable)

		// The item descriptor identity comes from this first candidate (empty during its pre-reconcile
		// window, self-healed by a later adopt), and its SlicedDetail is the single tier's Σ.
		detail := instType.Status.Detail
		detail.SlicedDetail = tier.AcceleratorSlicedDetail

		// Not found the same item, tier and candidate, create a new item with a new tier and candidate.
		in.state.Items = append(in.state.Items, AggregatedInstanceType{
			Name: buildAggregatedInstanceTypeName(instType.Spec),
			Spec: instType.Spec,
			Status: AggregatedInstanceTypeStatus{
				Detail:         detail,
				OnceMaxRequest: tier.OnceMaxRequest,
				Remaining:      tier.Remaining,
				Tiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					tier,
				},
			},
		})

		item := &in.state.Items[len(in.state.Items)-1]

		// Report an added event.
		evts = append(evts, &manager.WorkerEvent{
			Type:   manager.WorkerEventAdded,
			Object: item,
		})

		return evts
	}

	return evts
}

type ListClusterInstanceTypes struct {
	list ClusterInstanceTypeList
}

func OpListClusterInstanceTypes() *ListClusterInstanceTypes {
	return &ListClusterInstanceTypes{
		list: ClusterInstanceTypeList{
			Items: make([]ClusterInstanceType, 0),
		},
	}
}

func (in *ListClusterInstanceTypes) Next(cluster string, obj runtime.Object) error {
	instType, ok := obj.(*worker.InstanceType)
	if !ok {
		return fmt.Errorf("object is not of type InstanceType")
	}

	item := ClusterInstanceType{
		InstanceType: *instType,
		Cluster:      cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstanceTypes) Result() ClusterInstanceTypeList {
	return in.list
}

type HandleClusterInstanceType struct{}

func OpHandleClusterInstanceType() *HandleClusterInstanceType {
	return &HandleClusterInstanceType{}
}

func (in *HandleClusterInstanceType) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		instType, ok := evt.Object.(*worker.InstanceType)
		if !ok {
			return nil
		}
		if instType.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterInstType := &ClusterInstanceType{
			InstanceType: *instType,
			Cluster:      evt.Cluster,
		}
		clusterInstType.ManagedFields = nil
		evt.Object = clusterInstType
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstances struct {
	list ClusterInstanceList
}

func OpListClusterInstances() *ListClusterInstances {
	return &ListClusterInstances{
		list: ClusterInstanceList{
			Items: make([]ClusterInstance, 0),
		},
	}
}

func (in *ListClusterInstances) Next(cluster string, obj runtime.Object) error {
	inst, ok := obj.(*worker.Instance)
	if !ok {
		return fmt.Errorf("object is not of type Instance")
	}

	item := ClusterInstance{
		Instance: *inst,
		Cluster:  cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstances) Result() ClusterInstanceList {
	return in.list
}

type HandleClusterInstance struct {
	namespace string
}

func OpHandleClusterInstance(namespace string) *HandleClusterInstance {
	return &HandleClusterInstance{namespace: namespace}
}

func (in *HandleClusterInstance) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		inst, ok := evt.Object.(*worker.Instance)
		if !ok {
			return nil
		}
		if in.namespace != "" && inst.Namespace != in.namespace {
			return nil
		}
		if inst.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterInst := &ClusterInstance{
			Instance: *inst,
			Cluster:  evt.Cluster,
		}
		clusterInst.ManagedFields = nil
		evt.Object = clusterInst
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstancePersistentVolumeTypes struct {
	list ClusterInstancePersistentVolumeTypeList
}

func OpListClusterInstancePersistentVolumeTypes() *ListClusterInstancePersistentVolumeTypes {
	return &ListClusterInstancePersistentVolumeTypes{
		list: ClusterInstancePersistentVolumeTypeList{
			Items: make([]ClusterInstancePersistentVolumeType, 0),
		},
	}
}

func (in *ListClusterInstancePersistentVolumeTypes) Next(cluster string, obj runtime.Object) error {
	volType, ok := obj.(*worker.InstancePersistentVolumeType)
	if !ok {
		return fmt.Errorf("object is not of type InstancePersistentVolumeType")
	}

	item := ClusterInstancePersistentVolumeType{
		InstancePersistentVolumeType: *volType,
		Cluster:                      cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstancePersistentVolumeTypes) Result() ClusterInstancePersistentVolumeTypeList {
	return in.list
}

type HandleClusterInstancePersistentVolumeType struct{}

func OpHandleClusterInstancePersistentVolumeType() *HandleClusterInstancePersistentVolumeType {
	return &HandleClusterInstancePersistentVolumeType{}
}

func (in *HandleClusterInstancePersistentVolumeType) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		volType, ok := evt.Object.(*worker.InstancePersistentVolumeType)
		if !ok {
			return nil
		}
		if volType.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterVolType := &ClusterInstancePersistentVolumeType{
			InstancePersistentVolumeType: *volType,
			Cluster:                      evt.Cluster,
		}
		clusterVolType.ManagedFields = nil
		evt.Object = clusterVolType
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstancePersistentVolumes struct {
	list ClusterInstancePersistentVolumeList
}

func OpListClusterInstancePersistentVolumes() *ListClusterInstancePersistentVolumes {
	return &ListClusterInstancePersistentVolumes{}
}

func (in *ListClusterInstancePersistentVolumes) Next(cluster string, obj runtime.Object) error {
	vol, ok := obj.(*worker.InstancePersistentVolume)
	if !ok {
		return fmt.Errorf("object is not of type InstancePersistentVolume")
	}

	item := ClusterInstancePersistentVolume{
		InstancePersistentVolume: *vol,
		Cluster:                  cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstancePersistentVolumes) Result() ClusterInstancePersistentVolumeList {
	return in.list
}

type HandleClusterInstancePersistentVolume struct {
	namespace string
}

func OpHandleClusterInstancePersistentVolume(namespace string) *HandleClusterInstancePersistentVolume {
	return &HandleClusterInstancePersistentVolume{namespace: namespace}
}

func (in *HandleClusterInstancePersistentVolume) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		vol, ok := evt.Object.(*worker.InstancePersistentVolume)
		if !ok {
			return nil
		}
		if in.namespace != "" && vol.Namespace != in.namespace {
			return nil
		}
		if vol.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterVol := &ClusterInstancePersistentVolume{
			InstancePersistentVolume: *vol,
			Cluster:                  evt.Cluster,
		}
		clusterVol.ManagedFields = nil
		evt.Object = clusterVol
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstanceImagePullSecrets struct {
	list ClusterInstanceImagePullSecretList
}

func OpListClusterInstanceImagePullSecrets() *ListClusterInstanceImagePullSecrets {
	return &ListClusterInstanceImagePullSecrets{
		list: ClusterInstanceImagePullSecretList{
			Items: make([]ClusterInstanceImagePullSecret, 0),
		},
	}
}

func (in *ListClusterInstanceImagePullSecrets) Next(cluster string, obj runtime.Object) error {
	secret, ok := obj.(*worker.InstanceImagePullSecret)
	if !ok {
		return fmt.Errorf("object is not of type InstanceImagePullSecret")
	}

	item := ClusterInstanceImagePullSecret{
		InstanceImagePullSecret: *secret,
		Cluster:                 cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstanceImagePullSecrets) Result() ClusterInstanceImagePullSecretList {
	return in.list
}

type HandleClusterInstanceImagePullSecret struct {
	namespace string
}

func OpHandleClusterInstanceImagePullSecret(namespace string) *HandleClusterInstanceImagePullSecret {
	return &HandleClusterInstanceImagePullSecret{namespace: namespace}
}

func (in *HandleClusterInstanceImagePullSecret) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		secret, ok := evt.Object.(*worker.InstanceImagePullSecret)
		if !ok {
			return nil
		}
		if in.namespace != "" && secret.Namespace != in.namespace {
			return nil
		}
		if secret.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterSecret := &ClusterInstanceImagePullSecret{
			InstanceImagePullSecret: *secret,
			Cluster:                 evt.Cluster,
		}
		clusterSecret.ManagedFields = nil
		evt.Object = clusterSecret
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstanceSSHPublicKeys struct {
	list ClusterInstanceSSHPublicKeyList
}

func OpListClusterInstanceSSHPublicKeys() *ListClusterInstanceSSHPublicKeys {
	return &ListClusterInstanceSSHPublicKeys{
		list: ClusterInstanceSSHPublicKeyList{
			Items: make([]ClusterInstanceSSHPublicKey, 0),
		},
	}
}

func (in *ListClusterInstanceSSHPublicKeys) Next(cluster string, obj runtime.Object) error {
	key, ok := obj.(*worker.InstanceSSHPublicKey)
	if !ok {
		return fmt.Errorf("object is not of type InstanceSSHPublicKey")
	}

	item := ClusterInstanceSSHPublicKey{
		InstanceSSHPublicKey: *key,
		Cluster:              cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstanceSSHPublicKeys) Result() ClusterInstanceSSHPublicKeyList {
	return in.list
}

type HandleClusterInstanceSSHPublicKey struct {
	namespace string
}

func OpHandleClusterInstanceSSHPublicKey(namespace string) *HandleClusterInstanceSSHPublicKey {
	return &HandleClusterInstanceSSHPublicKey{namespace: namespace}
}

func (in *HandleClusterInstanceSSHPublicKey) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		key, ok := evt.Object.(*worker.InstanceSSHPublicKey)
		if !ok {
			return nil
		}
		if in.namespace != "" && key.Namespace != in.namespace {
			return nil
		}
		if key.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterKey := &ClusterInstanceSSHPublicKey{
			InstanceSSHPublicKey: *key,
			Cluster:              evt.Cluster,
		}
		clusterKey.ManagedFields = nil
		evt.Object = clusterKey
	}
	return []*manager.WorkerEvent{evt}
}
