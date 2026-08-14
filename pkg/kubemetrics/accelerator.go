package kubemetrics

import (
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/mathx"
)

// grantLogger records the one refusal a caller cannot see in the result: an accelerator two
// containers of a Pod were separately granted, which yields no figures of its own.
var grantLogger = klog.Background().WithName("instance-accelerator-metrics")

// acceleratorKey identifies one GRANT across the manufacturers a Pod can hold at once: a device ID
// is unique to its own manufacturer's library and to nothing beyond it.
type acceleratorKey struct {
	manufacturer string
	deviceID     string

	// partitionID separates two hardware partitions of ONE card, which are two grants and must be
	// two entries — the entry list is keyed by id, and a partition names itself, so the two are
	// distinguishable there. It is empty in every other mode: a logical slice has no identity of its
	// own, so two slices of one card would collide on that key and cannot be two entries at all.
	partitionID string
}

// acceleratorGrant is what one Pod was granted on one accelerator, as the allocation recorded it,
// and the container that holds it. The container is what the producer's records are keyed by, so the
// grant and the measurement are two halves of one record rather than two lookups that could
// disagree.
type acceleratorGrant struct {
	container string
	mode      workercore.DeviceAllocationMode

	// partitionID is the partition this grant holds, as the allocation recorded it, and empty in
	// every other mode. It names the entry even when nothing could measure the partition, which is
	// what keeps two unmeasurable partitions of one card from collapsing onto one identifier.
	partitionID string

	// allocatedUnits is the normalized per-accelerator units the allocation charged against the card,
	// on the basis nodefeature.ResourceMaxUnits defines — the same memory-anchored basis admission
	// counted credits on. It is what a logical slice's memory quota is folded back out of.
	allocatedUnits int32

	// coresCapPercent is the share of the WHOLE card's compute a logical slice is capped at, read
	// from whoever enforced it: the container's own compute request. It is the denominator the
	// measured share is restated against, and never a figure folded out of the units above, because a
	// logical slice's compute and memory budgets are independent — a container may hold a tenth of the
	// VRAM and half the compute.
	coresCapPercent uint32

	// ambiguous marks a grant two of the Pod's containers were separately given — the same whole
	// card, or the same partition of one. One entry cannot carry two grants, and picking or summing
	// them would report a quota nobody was granted.
	ambiguous bool
}

// AcceleratorGrants indexes what one Pod was granted on each accelerator it holds, so that a surface
// reporting its figures can state them in the Instance's own terms.
//
// It is built once per Pod and asked per accelerator, because the Pod-wide allocation view the
// accelerator list is filtered by cannot say which container holds which grant — and the producer's
// records are keyed by container.
//
// It lives here, beside the quota arithmetic, because both surfaces reporting these figures make the
// same joins: the metrics subresource per request, and the device manager's exporter per scrape. Two
// implementations would agree today and drift on the cases neither exercises — an accelerator two
// containers carved, a partition that named itself, a compute share restated against its cap.
type AcceleratorGrants struct {
	podUID string
	grants map[acceleratorKey]acceleratorGrant
}

// NewAcceleratorGrants indexes the Pod's allocations by accelerator, reading each logical slice's
// compute cap off the container it was enforced on.
func NewAcceleratorGrants(pod *core.Pod, allocations deviceplugin.PodAllocations) *AcceleratorGrants {
	grants := &AcceleratorGrants{
		podUID: string(pod.UID),
		grants: make(map[acceleratorKey]acceleratorGrant),
	}

	for container, allocation := range allocations {
		for _, grp := range allocation.Devices.Groups {
			for _, accelerator := range grp.Accelerators {
				// Visibility grants no resources at all: it lets one container see the devices a
				// SIBLING was granted. Indexing it would make every such Pod look like two containers
				// claiming one card, and the sibling that really holds it is indexed here already.
				if accelerator.Mode == workercore.DeviceAllocationModeVisibility {
					continue
				}

				// A partition is keyed by itself, so two containers holding two partitions of one
				// card are two grants rather than one contested card.
				partitionID := ""
				if accelerator.Mode == workercore.DeviceAllocationModePartitioned {
					partitionID = accelerator.AllocatedPhysicalID
				}

				key := acceleratorKey{
					manufacturer: grp.Manufacturer,
					deviceID:     accelerator.ID,
					partitionID:  partitionID,
				}
				if held, ok := grants.grants[key]; ok && held.container != container {
					grantLogger.V(2).Info(
						"two containers of one pod were granted the same accelerator,"+
							" reporting no figures of its own",
						"pod", pod.Namespace+"/"+pod.Name, "manufacturer", key.manufacturer,
						"device", key.deviceID, "partition", key.partitionID)
					held.ambiguous = true
					grants.grants[key] = held
					continue
				}
				grants.grants[key] = acceleratorGrant{
					container:      container,
					mode:           accelerator.Mode,
					partitionID:    partitionID,
					allocatedUnits: accelerator.Allocated,
					coresCapPercent: slicedCoresCapPercentOf(pod, container, grp.Manufacturer,
						accelerator.Mode),
				}
			}
		}
	}

	return grants
}

// Resolve reports what the Instance holds on one accelerator, in the Instance's own terms: what the
// allocation granted as the totals, what the producer measured of that grant as the used figures, and
// the mode that produced both.
//
// ONE ENTRY PER GRANT, NOT PER CARD. A card serves one grant in every mode but Partitioned, where the
// Instance may hold several of its partitions at once — one per container — and each is its own grant
// with its own identity, capacity and usage. Collapsing those onto the parent card would report one
// tenant of a card as if it were the card.
//
// An Instance holding the whole device reads the device's own figures, because the device is the
// grant. One holding a carved share reads the share's quota and the share's measured usage, and
// reads NO memory or compute figure at all where that measurement could not be taken — the device's
// would count every other tenant on the card, which is the one substitution this whole feature
// exists to refuse. The whole-device readings that are not resource usage — temperature, power draw
// and health — are reported in every mode, because a share of a card has none of its own.
//
// The measured figures come from the section verbatim, absence and zero alike: the section holds that
// decision so both surfaces reporting these figures make it identically. A section that cannot answer
// for this accelerator at all — a producer predating slice reporting, a schema it does not know, an
// accelerator it did not cover — leaves them absent, which is the honest claim: the grant is still
// known, and nothing measured the usage.
func (g *AcceleratorGrants) Resolve(
	section *detector.MonitorSliceSection,
	manufacturer string,
	card *device.AcceleratorMetrics,
) []worker.InstanceAcceleratorMetrics {
	// Ordered by the partition each grant holds, so a scrape and a subresource read of one Pod list
	// its entries the same way twice running. Every mode but Partitioned yields exactly one.
	keys := make([]acceleratorKey, 0, 1)
	for key := range g.grants {
		if key.manufacturer == manufacturer && key.deviceID == card.ID {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		// The accelerator list is filtered by this Pod's own allocation, so nothing it carries should
		// miss the index. One that does is an accelerator nobody can say the Instance's share of, and
		// the device's figures are not that share — so the entry names the card and measures nothing.
		return []worker.InstanceAcceleratorMetrics{cardEntry(card)}
	}
	slices.SortFunc(keys, func(a, b acceleratorKey) int {
		return strings.Compare(a.partitionID, b.partitionID)
	})

	entries := make([]worker.InstanceAcceleratorMetrics, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, g.resolveGrant(section, manufacturer, card, g.grants[key]))
	}
	return entries
}

// resolveGrant answers for one grant on one card.
func (g *AcceleratorGrants) resolveGrant(
	section *detector.MonitorSliceSection,
	manufacturer string,
	card *device.AcceleratorMetrics,
	grant acceleratorGrant,
) worker.InstanceAcceleratorMetrics {
	entry := cardEntry(card)
	entry.Mode = grant.mode.String()

	// A partition names itself even when nothing could measure it: the allocation recorded which one
	// this is, and reporting it under the parent card's identifier would put two partitions of one
	// card on one identifier — which the entry list, keyed by id, cannot carry.
	if grant.partitionID != "" {
		entry.ID = grant.partitionID
	}
	if grant.ambiguous {
		return entry
	}

	if !carvedAllocationMode(grant.mode) {
		entry.MemoryTotalMiB = ptr.To(card.Memory)
		entry.MemoryUsedMiB = ptr.To(card.MemoryUsage)
		entry.MemoryUtilizationPercent = ptr.To(card.MemoryUtilization)
		entry.CoresUtilizationPercent = ptr.To(card.CoresUtilization)
		return entry
	}

	var figures detector.SliceFigures
	if measured, answered := section.Figures(
		manufacturer, g.podUID, grant.container, card.ID,
	); answered {
		figures = measured
	}

	// A hardware partition names and sizes ITSELF: the driver read both off the partition's own
	// handle, so the capacity comes from the producer rather than from the allocation, and the
	// driver's own naming wins over the recorded one where it answered. A logical slice has neither —
	// it is a quota carved out of the card the entry is already reported under.
	if figures.ID != "" {
		entry.ID = figures.ID
	}
	if grant.mode == workercore.DeviceAllocationModePartitioned {
		entry.MemoryTotalMiB = figures.MemoryTotalMiB
	} else {
		entry.MemoryTotalMiB = slicedMemoryTotalMiB(grant.allocatedUnits, card.Memory)
	}
	entry.MemoryUsedMiB = figures.MemoryUsedMiB
	entry.MemoryUtilizationPercent = utilizationPercent(entry.MemoryUsedMiB, entry.MemoryTotalMiB)
	entry.CoresUtilizationPercent = ownCoresUtilizationPercent(
		figures.CoresUtilizationPercent, grant.coresCapPercent)
	return entry
}

// cardEntry is the part of an entry that comes from the whole card in every mode: its identity as a
// starting point, and the readings a share of a card has none of its own.
func cardEntry(card *device.AcceleratorMetrics) worker.InstanceAcceleratorMetrics {
	return worker.InstanceAcceleratorMetrics{
		ID:                 card.ID,
		TemperatureCelsius: ptr.To(card.Temperature),
		PowerUsageWatts:    ptr.To(card.PowerUsage),
		Unhealthy:          ptr.To(card.Unhealthy),
	}
}

// carvedAllocationMode reports whether an allocation holds a share of an accelerator rather than the
// whole of it. Shared is not carved: it grants the device to several holders with no quota between
// them, so the device's figures are the only ones its holder has.
func carvedAllocationMode(mode workercore.DeviceAllocationMode) bool {
	switch mode {
	case workercore.DeviceAllocationModeSliced, workercore.DeviceAllocationModePartitioned:
		return true
	}
	return false
}

// slicedMemoryTotalMiB folds a logical slice's allocated units back into MiB, inverting the fold that
// produced them (nodefeature.MemoryMibToUnits) so the reported total cannot disagree with the quota
// that was granted. A record claiming more than a whole card is held at the whole card, the same
// bound the device plugin applies when it writes one.
//
// A GRANTED QUOTA NEVER FOLDS BACK TO NOTHING. The units are a fraction of a fixed scale, so a small
// enough slice of a large enough card divides to zero — a 1 MiB request on an 80 GiB card is granted
// 19 of 1,600,000 units, which fold back to 0 MiB. Zero is not a small quota here but the absence of
// one, so the smallest quota this can report is 1 MiB. Only an allocation that charged nothing, or a
// card whose capacity never reached this pass, has no quota to state — and then it states none.
func slicedMemoryTotalMiB(units int32, cardMemoryMiB uint64) *uint64 {
	if units <= 0 || cardMemoryMiB == 0 {
		return nil
	}
	if units > nodefeature.ResourceMaxUnits {
		units = nodefeature.ResourceMaxUnits
	}
	return ptr.To(max(uint64(units)*cardMemoryMiB/nodefeature.ResourceMaxUnits, 1))
}

// utilizationPercent states a Used/Total pair as a percentage, and states none where either half is
// missing. It is computed from the two figures published beside it rather than taken from any
// source of its own, so the three can never disagree.
func utilizationPercent(usedMiB, totalMiB *uint64) *uint32 {
	if usedMiB == nil || totalMiB == nil || *totalMiB == 0 {
		return nil
	}
	return ptr.To(device.CalculateUtilization(*usedMiB, *totalMiB))
}

// ownCoresUtilizationPercent restates a measured share of the WHOLE card as a share of the holder's
// own compute allowance, which is what makes one field mean one thing in every mode: a slice capped
// at a fifth of a card and saturating that fifth reads 100 here, exactly as an Instance saturating a
// whole card does.
//
// The quotient rounds UP for the reason every other conversion in this package does: the
// manufacturers measure the card in whole percent, so a small cap makes the numerator coarse, and
// flooring would publish a slice that is measurably busy as one doing nothing. A cap of the whole
// card — which is what a slice requesting no compute budget is granted — passes the figure through
// untouched.
func ownCoresUtilizationPercent(measuredPercent *uint32, capPercent uint32) *uint32 {
	if measuredPercent == nil {
		return nil
	}
	if capPercent == 0 || capPercent >= 100 {
		return measuredPercent
	}
	return ptr.To(uint32(mathx.CeilDiv(uint64(*measuredPercent)*100, uint64(capPercent)))) // nolint: gosec
}

// slicedCoresCapPercentOf reads the compute cap the allocator enforced on one container of the Pod,
// from the request it was granted. A container the Pod spec no longer carries, and one requesting no
// compute budget, are capped at the whole card — which is what the allocator itself applies.
//
// It answers only for a logical slice. That default is why: a hardware partition makes no compute
// request, so reading one for it would cap every partition at the whole card.
func slicedCoresCapPercentOf(
	pod *core.Pod, container, manufacturer string, mode workercore.DeviceAllocationMode,
) uint32 {
	if mode != workercore.DeviceAllocationModeSliced {
		return 0
	}
	resName := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manufacturer)

	// All three container lists, because an allocation is recorded for whichever kind of container
	// holds it and the default for a container this search misses is the WHOLE CARD — so a search
	// narrower than the allocations it reads would publish a cap nobody granted. The attribution
	// index and the started-container gate walk the same three for the same reason.
	ctr := &core.Container{}
	for _, containers := range [][]core.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		if i := slices.IndexFunc(containers, func(c core.Container) bool {
			return c.Name == container
		}); i >= 0 {
			ctr = &containers[i]
			break
		}
	}
	if i := slices.IndexFunc(pod.Spec.EphemeralContainers, func(c core.EphemeralContainer) bool {
		return c.Name == container
	}); i >= 0 {
		ctr = (*core.Container)(&pod.Spec.EphemeralContainers[i].EphemeralContainerCommon)
	}
	return uint32(deviceplugin.SlicedCoresPercent(ctr, resName)) // nolint: gosec
}
