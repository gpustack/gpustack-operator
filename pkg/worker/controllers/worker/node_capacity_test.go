package worker

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
)

var (
	nvidiaSlicedUnits  = nodefeature.GetAcceleratableSlicedUnitsResourceName(nodefeature.ManufacturerNVIDIA)
	nvidiaSlicedCores  = nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(nodefeature.ManufacturerNVIDIA)
	nvidiaSlicedMemPct = nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(nodefeature.ManufacturerNVIDIA)
	nvidiaSlicedMemMib = nodefeature.GetAcceleratableSlicedMemoryMibResourceName(nodefeature.ManufacturerNVIDIA)

	nvidiaPartitionedUnits = nodefeature.GetAcceleratablePartitionedUnitsResourceName(nodefeature.ManufacturerNVIDIA)
)

// Per-card VRAM of the test models, in MiB (the ".memory" feature label is a
// resource.Quantity like "24Gi", which the reconciler reads as MiB).
const (
	a10gMib       = 24 * 1024 // 24Gi
	t4Mib         = 16 * 1024 // 16Gi
	mthreadsMib   = 48 * 1024 // 48Gi
	ascend910bMib = 64 * 1024 // 64Gi
)

// acceleratableNode builds a managed Node carrying the acceleratable feature labels
// (presence, count, and optionally per-card memory) for one model.
func acceleratableNode(name, aKey, count, memory string, managed bool) *core.Node {
	labels := map[string]string{
		nodefeature.AcceleratableFeatureLabelPrefix + aKey:            "true",
		nodefeature.AcceleratableFeatureLabelPrefix + aKey + ".count": count,
	}
	if memory != "" {
		labels[nodefeature.AcceleratableFeatureLabelPrefix+aKey+".memory"] = memory
	}
	if managed {
		labels[systemname.ManagedLabelKey] = "true"
	}
	return &core.Node{ObjectMeta: meta.ObjectMeta{Name: name, Labels: labels}}
}

// withModel adds another acceleratable model's feature labels to the node.
func withModel(nd *core.Node, aKey, count, memory string) *core.Node {
	nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+aKey] = "true"
	nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+aKey+".count"] = count
	if memory != "" {
		nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+aKey+".memory"] = memory
	}
	return nd
}

// slicedWant builds the expected four ".sliced.*" capacities for one manufacturer:
// cards × the per-card constants, plus the (already summed) per-card VRAM total. The
// cores-percentage honors the overcommit flag — cards × maxSlices × 100 when compute may
// be overcommitted, else a single per-card cards × 100.
func slicedWant(manufacturer string, cards, totalMemMib, maxSlices int64, overcommit bool) map[string]int64 {
	cores := cards * 100
	if overcommit {
		cores = cards * maxSlices * 100
	}
	return map[string]int64{
		string(nodefeature.GetAcceleratableSlicedUnitsResourceName(manufacturer)):            cards * nodefeature.ResourceMaxUnits,
		string(nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manufacturer)):  cores,
		string(nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(manufacturer)): cards * 100,
		string(nodefeature.GetAcceleratableSlicedMemoryMibResourceName(manufacturer)):        totalMemMib,
	}
}

func mergeWant(maps ...map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// withSlicedPool advertises the device-plugin's bare "<mfr>.sliced" token pool on the
// node — the presence gate desiredAcceleratorCapacity reads before emitting ".sliced.*".
func withSlicedPool(nd *core.Node, manufacturer string, tokens int64) *core.Node {
	return withPool(nd, manufacturer, workercore.DeviceAllocationModeSliced, tokens)
}

// withPartitionedPool advertises the bare "<mfr>.partitioned" token pool — the presence gate for
// the hardware-partition counting keys.
func withPartitionedPool(nd *core.Node, manufacturer string, tokens int64) *core.Node {
	return withPool(nd, manufacturer, workercore.DeviceAllocationModePartitioned, tokens)
}

func withPool(nd *core.Node, manufacturer string, mode workercore.DeviceAllocationMode, tokens int64) *core.Node {
	if nd.Status.Capacity == nil {
		nd.Status.Capacity = core.ResourceList{}
	}
	nd.Status.Capacity[nodefeature.GetAcceleratableResourceName(manufacturer, mode)] = *resource.NewQuantity(tokens, resource.DecimalSI)
	return nd
}

// vendorSlicing is one device group's per-card logical-slicing capability for a Devices
// fixture: the group ID (which must equal the model part of the node's aKey,
// "${manufacturer}-${id}"), the manufacturer, the number of soft cards, and each card's
// per-card slice count / overcommit. A maxSlices of 0 models a non-sliceable group (cards
// carrying no slice budget).
type vendorSlicing struct {
	manufacturer string
	id           string
	cards        int32
	maxSlices    int32
	overcommit   bool
}

// devicesWithSlicing builds a same-named Devices CR whose groups carry per-card soft-slice
// status, resolved by "${manufacturer}-${group ID}" so desiredAcceleratorCapacity maps each node
// model to its own group. Each vendor contributes v.cards soft cards, matching the count its
// model's ".count" label advertises on the node.
func devicesWithSlicing(name string, vendors ...vendorSlicing) *workercore.Devices {
	d := &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: name}}
	for _, v := range vendors {
		cards := make([]workercore.Accelerator, 0, v.cards)
		for i := int32(0); i < v.cards; i++ {
			cards = append(cards, softCard(strconv.Itoa(int(i)), v.maxSlices, v.overcommit))
		}
		d.Spec.Groups = append(d.Spec.Groups, slicedGroup(v.manufacturer, v.id, cards...))
	}
	return d
}

// softCard builds one logically-sliceable (soft) accelerator carrying per-card sliced status.
func softCard(id string, logicalCount int32, overcommit bool) workercore.Accelerator {
	return workercore.Accelerator{
		ID: id,
		Status: workercore.AcceleratorStatus{
			LogicalSliced: workercore.AcceleratorLogicalSliced{
				Count:                     logicalCount,
				CoresPercentageOvercommit: overcommit,
			},
		},
	}
}

// migCard builds one MIG-enabled (physically-sliceable) accelerator with the given physical
// ceiling — no logical (soft) budget.
func migCard(id string, physicalCount int32) workercore.Accelerator {
	return workercore.Accelerator{
		ID: id,
		Status: workercore.AcceleratorStatus{
			PhysicalSliced: workercore.AcceleratorPhysicalSliced{Count: physicalCount},
		},
	}
}

// migCardWithProfiles builds one MIG-enabled accelerator carrying per-card physical profiles
// (name→count), so the group aggregate produces Detail.Physical.Profiles that NodeCapacity
// advertises as ".sliced.mig-<profile>" keys. PhysicalSliced.Count is the largest profile count
// (the pool ceiling), keeping the card sliceable.
func migCardWithProfiles(id string, profiles map[string]int32) workercore.Accelerator {
	ps := make([]workercore.AcceleratorPhysicalSlicedProfile, 0, len(profiles))
	var ceiling int32
	for name, count := range profiles {
		ps = append(ps, workercore.AcceleratorPhysicalSlicedProfile{Name: name, Count: count})
		if count > ceiling {
			ceiling = count
		}
	}
	return workercore.Accelerator{
		ID: id,
		Status: workercore.AcceleratorStatus{
			PhysicalSliced: workercore.AcceleratorPhysicalSliced{Profiles: ps, Count: ceiling},
		},
	}
}

// nvidiaPartitioned is the NVIDIA per-profile hardware-partition capacity key for a profile.
func nvidiaPartitioned(profile string) string {
	return string(nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, profile))
}

// slicedGroup builds a new-format Devices group from its per-card statuses, deriving the
// aggregated AcceleratorSlicedDetail exactly as the detector's SetGroupSlicedDetails does.
func slicedGroup(mfr, id string, cards ...workercore.Accelerator) workercore.DevicesGroup {
	return workercore.DevicesGroup{
		ID:                      id,
		Manufacturer:            mfr,
		Accelerators:            cards,
		AcceleratorSlicedDetail: device.AggregateAcceleratorSlicedDetail(cards),
	}
}

// withPartitionLedger attaches the runtime allocation ledger for one card: the partition
// instances it currently holds and the ones it can still host. The capability lives on the
// Devices spec and the occupancy on its status, so the per-profile capacity key is a join of the
// two — a fixture that sets only the spec exercises the no-ledger fallback instead.
func withPartitionLedger(
	devs *workercore.Devices, groupID, cardID string, allocated, remaining map[string]int32,
) *workercore.Devices {
	devs.Status.Groups = append(devs.Status.Groups, workercore.DevicesAllocationGroup{
		ID:           groupID,
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Accelerators: []workercore.AcceleratorAllocation{{
			ID:                cardID,
			AllocatedProfiles: device.ProfileCountSlice(allocated),
			RemainingProfiles: device.ProfileCountSlice(remaining),
		}},
	})
	return devs
}

// devicesWithGroups builds a same-named Devices CR from prebuilt new-format groups.
func devicesWithGroups(name string, groups ...workercore.DevicesGroup) *workercore.Devices {
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec:       workercore.DevicesSpec{Groups: groups},
	}
}

// TestDesiredAcceleratorCapacity covers per-card sliced capacity across the aggregation and presence
// gates: cross-model and cross-manufacturer summation, per-model memory-mib weighting, the managed
// / sliced-pool / reported-capability gates, colliding group IDs staying isolated, and a
// non-sliceable model contributing nothing. TestDesiredAcceleratorCapacityNewFormat covers the
// mixed-logical/MIG, all-MIG, and lossy-VRAM edge cases.
func TestDesiredAcceleratorCapacity(t *testing.T) {
	cases := []struct {
		name string
		node *core.Node
		devs *workercore.Devices
		want map[string]int64 // resource name → value; nil → empty
	}{
		{
			// Overcommit vendor (NVIDIA 128): cores-percentage = cards × per-card count × 100.
			name: "nvidia overcommit reports cards × count × 100 cores",
			node: withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "8", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 8*128),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 8, 128, true}),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 8, 8*a10gMib, 128, true),
		},
		{
			// Partition vendor (MThreads 16, no overcommit): cores-percentage = cards × 100.
			name: "mthreads no-overcommit caps cores at cards × 100",
			node: withSlicedPool(acceleratableNode("node-5", "mthreads-s4000", "8", "48Gi", true),
				nodefeature.ManufacturerMThreads, 8*16),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerMThreads, "s4000", 8, 16, false}),
			want: slicedWant(nodefeature.ManufacturerMThreads, 8, 8*mthreadsMib, 16, false),
		},
		{
			name: "unmanaged node reports nothing",
			node: withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", false),
				nodefeature.ManufacturerNVIDIA, 4*128),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 4, 128, true}),
			want: nil,
		},
		{
			// The device-plugin advertises no ".sliced" pool → presence gate fails.
			name: "no sliced token pool advertised is gated out",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 4, 128, true}),
			want: nil,
		},
		{
			// The pool is present, but the Devices CR has not reported the capability yet.
			name: "sliced pool present but Devices reports no capability is gated out",
			node: withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 4*128),
			devs: nil,
			want: nil,
		},
		{
			// Two models of the same manufacturer sum cards/cores/mem-% into one
			// key set; ".sliced.memory-mib" is weighted per model's own VRAM.
			name: "same manufacturer models sum, memory-mib weighted per model",
			node: withSlicedPool(
				withModel(acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true), "nvidia-t4", "2", "16Gi"),
				nodefeature.ManufacturerNVIDIA, 6*128),
			devs: devicesWithSlicing("node-5",
				vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 4, 128, true},
				vendorSlicing{nodefeature.ManufacturerNVIDIA, "t4", 2, 128, true}),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 6, 4*a10gMib+2*t4Mib, 128, true),
		},
		{
			// A model missing its ".memory" label contributes 0 to memory-mib but
			// still the other three keys.
			name: "missing VRAM yields zero memory-mib, other keys intact",
			node: withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "4", "", true),
				nodefeature.ManufacturerNVIDIA, 4*128),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 4, 128, true}),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 4, 0, 128, true),
		},
		{
			// Distinct manufacturers report distinct key sets, each with its own
			// overcommit behavior.
			name: "distinct manufacturers report distinct key sets",
			node: withSlicedPool(
				withSlicedPool(
					withModel(acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true), "mthreads-s4000", "2", "48Gi"),
					nodefeature.ManufacturerNVIDIA, 4*128),
				nodefeature.ManufacturerMThreads, 2*16),
			devs: devicesWithSlicing("node-5",
				vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 4, 128, true},
				vendorSlicing{nodefeature.ManufacturerMThreads, "s4000", 2, 16, false}),
			want: mergeWant(
				slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib, 128, true),
				slicedWant(nodefeature.ManufacturerMThreads, 2, 2*mthreadsMib, 16, false),
			),
		},
		{
			// Two manufacturers whose group IDs collide (ConstructGroupID strips the vendor
			// prefix, so both normalize to "gpu"): keying slicing features by the full
			// "${manufacturer}-${id}" keeps each model's own count/overcommit — a bare-ID
			// key would let the second group overwrite the first and cross-contaminate them.
			name: "colliding group IDs across manufacturers stay isolated",
			node: withSlicedPool(
				withSlicedPool(
					withModel(acceleratableNode("node-5", "nvidia-gpu", "4", "24Gi", true), "mthreads-gpu", "2", "48Gi"),
					nodefeature.ManufacturerNVIDIA, 4*128),
				nodefeature.ManufacturerMThreads, 2*16),
			devs: devicesWithSlicing("node-5",
				vendorSlicing{nodefeature.ManufacturerNVIDIA, "gpu", 4, 128, true},
				vendorSlicing{nodefeature.ManufacturerMThreads, "gpu", 2, 16, false}),
			want: mergeWant(
				slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib, 128, true),
				slicedWant(nodefeature.ManufacturerMThreads, 2, 2*mthreadsMib, 16, false),
			),
		},
		{
			// A manufacturer with a sliceable model (Ascend 910B) and a non-sliceable one
			// (Ascend 310, count 0) counts only the sliceable model's cards into ".sliced.*";
			// the 310's two cards are excluded even though the bare pool is manufacturer-wide.
			name: "mixed sliceable and non-sliceable models count only the sliceable",
			node: withSlicedPool(
				withModel(acceleratableNode("node-5", "ascend-910b", "4", "64Gi", true), "ascend-310", "2", "24Gi"),
				nodefeature.ManufacturerAscend, 4*63),
			devs: devicesWithSlicing("node-5",
				vendorSlicing{nodefeature.ManufacturerAscend, "910b", 4, 63, true},
				vendorSlicing{nodefeature.ManufacturerAscend, "310", 2, 0, false}),
			want: slicedWant(nodefeature.ManufacturerAscend, 4, 4*ascend910bMib, 63, true),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := desiredAcceleratorCapacity(c.node, c.devs)
			assert.Len(t, got, len(c.want))
			for name, val := range c.want {
				q, ok := got[core.ResourceName(name)]
				require.Truef(t, ok, "missing %s", name)
				assert.Equalf(t, val, q.Value(), "%s value", name)
			}
		})
	}
}

// TestDesiredAcceleratorCapacityIgnoresPartitions pins that the legacy ".sliced.partitions"
// label no longer affects capacity (slicing capability is sourced from Devices).
func TestDesiredAcceleratorCapacityIgnoresPartitions(t *testing.T) {
	nd := withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true),
		nodefeature.ManufacturerNVIDIA, 4*128)
	// An invalid partitions value would have suppressed capacity before; now ignored.
	nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-a10g.sliced.partitions"] = "3"
	devs := devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 4, 128, true})

	got := desiredAcceleratorCapacity(nd, devs)
	want := slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib, 128, true)
	assert.Len(t, got, len(want))
	for name, val := range want {
		q, ok := got[core.ResourceName(name)]
		require.Truef(t, ok, "missing %s", name)
		assert.Equalf(t, val, q.Value(), "%s value", name)
	}
}

// TestDesiredAcceleratorCapacityNewFormat covers the new per-card sourcing: the sliceable-vs-soft
// split (units count every sliceable card, the three logical keys only soft cards), all-MIG
// (logical keys omitted), the lossy VRAM label, and the Devices-wins-over-label cardinality.
func TestDesiredAcceleratorCapacityNewFormat(t *testing.T) {
	const node = "node-5"
	unitsFor := func(cards int64) int64 { return cards * nodefeature.ResourceMaxUnits }

	cases := []struct {
		name string
		node *core.Node
		devs *workercore.Devices
		want map[string]int64
	}{
		{
			// Non-MIG identity: 8 soft cards each with LogicalSliced.Count = group maxSlices
			// (128) reproduce today's numbers exactly on the new per-card path.
			name: "non-mig soft cards match today's numbers",
			node: withSlicedPool(acceleratableNode(node, "nvidia-a10g", "8", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 8*128),
			devs: devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g",
				softCard("0", 128, true), softCard("1", 128, true), softCard("2", 128, true), softCard("3", 128, true),
				softCard("4", 128, true), softCard("5", 128, true), softCard("6", 128, true), softCard("7", 128, true))),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 8, 8*a10gMib, 128, true),
		},
		{
			// Mixed group: 2 logically sliceable + 1 partitioned card. Every key counts only its
			// own population, so no card is charged to both families.
			name: "mixed logical + partitioned: each family counts only its own cards",
			node: withPartitionedPool(withSlicedPool(acceleratableNode(node, "nvidia-a10g", "3", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 2*128), nodefeature.ManufacturerNVIDIA, 7),
			devs: devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g",
				softCard("0", 128, true), softCard("1", 128, true), migCard("2", 7))),
			want: map[string]int64{
				string(nvidiaSlicedUnits):      unitsFor(2),
				string(nvidiaSlicedCores):      2 * 128 * 100,
				string(nvidiaSlicedMemPct):     2 * 100,
				string(nvidiaSlicedMemMib):     2 * a10gMib,
				string(nvidiaPartitionedUnits): unitsFor(1),
			},
		},
		{
			// A partitioned card advertises nothing while the plugin serves no partition pool
			// here, exactly as a logically sliceable card advertises nothing without ".sliced".
			name: "partition keys are gated on the partition pool",
			node: withSlicedPool(acceleratableNode(node, "nvidia-a10g", "1", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 128),
			devs: devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g",
				migCard("0", 7))),
			want: nil,
		},
		{
			// All-partitioned group: only the partition units key; every logical key is omitted
			// entirely, so a stale one is reverse-patched.
			name: "all-partitioned: partition units only, logical keys omitted",
			node: withPartitionedPool(acceleratableNode(node, "nvidia-a10g", "2", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 2*7),
			devs: devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g",
				migCard("0", 7), migCard("1", 7))),
			want: map[string]int64{string(nvidiaPartitionedUnits): unitsFor(2)},
		},
		{
			// With no ledger reported yet, each per-profile key degrades to the card's static
			// capability ceiling summed across the cards — an empty node's honest answer.
			name: "no ledger yet: per-profile keys degrade to the static ceiling",
			node: withPartitionedPool(acceleratableNode(node, "nvidia-a100", "2", "40Gi", true),
				nodefeature.ManufacturerNVIDIA, 2*7),
			devs: devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a100",
				migCardWithProfiles("0", map[string]int32{"1g.5gb": 7, "2g.10gb": 3, "3g.20gb": 2}),
				migCardWithProfiles("1", map[string]int32{"1g.5gb": 7, "2g.10gb": 3, "3g.20gb": 2}))),
			want: map[string]int64{
				string(nvidiaPartitionedUnits): unitsFor(2),
				nvidiaPartitioned("1g.5gb"):    14,
				nvidiaPartitioned("2g.10gb"):   6,
				nvidiaPartitioned("3g.20gb"):   4,
			},
		},
		{
			// The ledger-derived value on a card holding one mid-size partition. Each key is
			// allocated + remaining, so the scheduler — which subtracts the requests of the Pods
			// already on the node — arrives at the free count rather than subtracting each live
			// instance twice. A profile the geometry can no longer fit reads zero even though its
			// static ceiling is one.
			name: "ledger-derived per-profile value on a partly carved card",
			node: withPartitionedPool(acceleratableNode(node, "nvidia-a100", "1", "80Gi", true),
				nodefeature.ManufacturerNVIDIA, 7),
			devs: withPartitionLedger(
				devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a100",
					migCardWithProfiles("0", map[string]int32{"1g.10gb": 7, "3g.40gb": 2, "7g.80gb": 1}))),
				"a100", "0",
				map[string]int32{"3g.40gb": 1},
				map[string]int32{"1g.10gb": 4, "3g.40gb": 1},
			),
			want: map[string]int64{
				string(nvidiaPartitionedUnits): unitsFor(1),
				nvidiaPartitioned("1g.10gb"):   4, // 0 allocated + 4 that still fit
				nvidiaPartitioned("3g.40gb"):   2, // 1 allocated + 1 that still fits
				nvidiaPartitioned("7g.80gb"):   0, // the whole-card profile no longer fits
			},
		},
		{
			// Mixed in one group: the logical keys count the 2 unpartitioned cards and the
			// partition keys the 1 partitioned card, each family's units disjoint from the other.
			name: "mixed populations advertise both families, no card counted twice",
			node: withPartitionedPool(withSlicedPool(acceleratableNode(node, "nvidia-a10g", "3", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 2*128), nodefeature.ManufacturerNVIDIA, 7),
			devs: devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g",
				softCard("0", 128, true), softCard("1", 128, true),
				migCardWithProfiles("2", map[string]int32{"1g.10gb": 7}))),
			want: map[string]int64{
				string(nvidiaSlicedUnits):      unitsFor(2),
				string(nvidiaSlicedCores):      2 * 128 * 100,
				string(nvidiaSlicedMemPct):     2 * 100,
				string(nvidiaSlicedMemMib):     2 * a10gMib,
				string(nvidiaPartitionedUnits): unitsFor(1),
				nvidiaPartitioned("1g.10gb"):   7,
			},
		},
		{
			// Non-overcommit (partition) model: cores = softCards × 100, not Detail.Logical.Count.
			name: "non-overcommit soft cards cap cores at softCards × 100",
			node: withSlicedPool(acceleratableNode(node, "mthreads-s4000", "4", "48Gi", true),
				nodefeature.ManufacturerMThreads, 4*16),
			devs: devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerMThreads, "s4000",
				softCard("0", 16, false), softCard("1", 16, false), softCard("2", 16, false), softCard("3", 16, false))),
			want: slicedWant(nodefeature.ManufacturerMThreads, 4, 4*mthreadsMib, 16, false),
		},
		{
			// R4: per-card VRAM comes from the lossy ".memory" label, never the exact
			// DevicesGroup.Memory. The label rounds to 42Gi while the true size is 43238 MiB;
			// memory-mib must follow the label (43008 MiB), not the decoy group Memory.
			name: "memory-mib follows the lossy label, not exact Devices.Memory",
			node: withSlicedPool(acceleratableNode(node, "nvidia-a10g", "1", "42Gi", true),
				nodefeature.ManufacturerNVIDIA, 128),
			devs: func() *workercore.Devices {
				g := slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g", softCard("0", 128, true))
				g.Memory = 43238 // decoy: exact MiB the reconciler must NOT read
				return devicesWithGroups(node, g)
			}(),
			want: map[string]int64{
				string(nvidiaSlicedUnits):  unitsFor(1),
				string(nvidiaSlicedCores):  128 * 100,
				string(nvidiaSlicedMemPct): 100,
				string(nvidiaSlicedMemMib): 42 * 1024, // 42Gi from the label, not 43238
			},
		},
		{
			// Node-vs-Devices skew: the ".count" label claims 8, but the Devices ledger holds 4
			// soft cards. The ledger wins for cardinality (the label only finds the group).
			name: "devices ledger wins cardinality over the .count label",
			node: withSlicedPool(acceleratableNode(node, "nvidia-a10g", "8", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 8*128),
			devs: devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g",
				softCard("0", 128, true), softCard("1", 128, true), softCard("2", 128, true), softCard("3", 128, true))),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib, 128, true),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := desiredAcceleratorCapacity(c.node, c.devs)
			assert.Len(t, got, len(c.want))
			for name, val := range c.want {
				q, ok := got[core.ResourceName(name)]
				require.Truef(t, ok, "missing %s", name)
				assert.Equalf(t, val, q.Value(), "%s value", name)
			}
		})
	}
}

// TestPartitionLedgerIsManufacturerQualified pins that the runtime ledger is looked up by
// manufacturer AND group ID. ConstructGroupID strips the vendor prefix, so two manufacturers can
// carry the same bare group ID on one node; keyed by ID alone one vendor's ledger would be read
// for the other's cards, and its live allocations would vanish from the advertised capacity.
func TestPartitionLedgerIsManufacturerQualified(t *testing.T) {
	profiles := map[string]int32{"1g.10gb": 7, "3g.40gb": 2}
	nvidiaGroup := slicedGroup(nodefeature.ManufacturerNVIDIA, "x", migCardWithProfiles("0", profiles))
	mthreadsGroup := slicedGroup(nodefeature.ManufacturerMThreads, "x", migCardWithProfiles("0", profiles))

	devs := devicesWithGroups("node-collide", nvidiaGroup, mthreadsGroup)
	// Only the MThreads card is carved; the NVIDIA card sharing its bare group ID is untouched.
	devs.Status.Groups = append(devs.Status.Groups, workercore.DevicesAllocationGroup{
		ID: "x", Manufacturer: nodefeature.ManufacturerMThreads,
		Accelerators: []workercore.AcceleratorAllocation{{
			ID:                "0",
			AllocatedProfiles: device.ProfileCountSlice(map[string]int32{"3g.40gb": 1}),
			RemainingProfiles: device.ProfileCountSlice(map[string]int32{"1g.10gb": 4, "3g.40gb": 1}),
		}},
	})

	ledger := devicesLedgerByGroup(devs)
	nvidia := partitionInstancesByProfile(&nvidiaGroup, ledger[nodefeature.ManufacturerNVIDIA+"-x"])
	mthreads := partitionInstancesByProfile(&mthreadsGroup, ledger[nodefeature.ManufacturerMThreads+"-x"])

	assert.Equal(t, map[string]int64{"1g.10gb": 7, "3g.40gb": 2}, nvidia,
		"the untouched vendor keeps its full ceiling")
	assert.Equal(t, map[string]int64{"1g.10gb": 4, "3g.40gb": 2}, mthreads,
		"the carved vendor reports what its own ledger says")
}

// TestAcceleratorDetailChanged pins the Devices-watch predicate: it fires on anything the
// desired capacity is computed from — the per-card population split, the logical count, the
// profile inventory, and now the runtime ledger the per-profile key is derived from — and stays
// quiet on churn that cannot move a capacity value, such as a health flip.
func TestAcceleratorDetailChanged(t *testing.T) {
	const node = "node-w"
	base := devicesWithGroups(node,
		slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g", softCard("0", 128, true), softCard("1", 128, true)))
	assert.False(t, acceleratorDetailChanged(base, base), "identical devices do not fire")

	health := devicesWithGroups(node,
		slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g", softCard("0", 128, true), softCard("1", 128, true)))
	health.Spec.Groups[0].Accelerators[0].Status.Unhealthy = true
	assert.False(t, acceleratorDetailChanged(base, health), "a health flip cannot move a capacity value")

	toMig := devicesWithGroups(node,
		slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g", softCard("0", 128, true), migCard("1", 7)))
	assert.True(t, acceleratorDetailChanged(base, toMig), "a card moving between populations fires")

	fewer := devicesWithGroups(node,
		slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g", softCard("0", 64, true), softCard("1", 64, true)))
	assert.True(t, acceleratorDetailChanged(base, fewer), "a logical-count change fires")

	added := devicesWithGroups(node,
		slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g", softCard("0", 128, true), softCard("1", 128, true)),
		slicedGroup(nodefeature.ManufacturerMThreads, "s4000", softCard("0", 16, false)))
	assert.True(t, acceleratorDetailChanged(base, added), "a new group fires")

	// A re-partition keeps the card count but changes the profile inventory, which the population
	// split alone would miss.
	reslice := devicesWithGroups(node,
		slicedGroup(nodefeature.ManufacturerNVIDIA, "a100", migCardWithProfiles("0", map[string]int32{"1g.10gb": 7})))
	resliced := devicesWithGroups(node,
		slicedGroup(nodefeature.ManufacturerNVIDIA, "a100", migCardWithProfiles("0", map[string]int32{"2g.20gb": 3})))
	assert.False(t, acceleratorDetailChanged(reslice, reslice), "identical profiles do not fire")
	assert.True(t, acceleratorDetailChanged(reslice, resliced), "a re-partition fires")

	// The ledger side matters now: carving a partition moves the per-profile key with the spec
	// untouched, so the watch must fire on it. This is the churn the predicate used to ignore
	// outright, and ignoring it would leave the node advertising room it no longer has.
	//
	// It has to be a card offering more than one profile shape. On a single-profile card
	// allocated + remaining is invariant under carving — that is exactly why the key sums both
	// terms — so only a profile whose room another profile's instance consumed actually moves.
	multi := map[string]int32{"1g.10gb": 7, "3g.40gb": 2, "7g.80gb": 1}
	empty := devicesWithGroups(node,
		slicedGroup(nodefeature.ManufacturerNVIDIA, "a100", migCardWithProfiles("0", multi)))
	carved := withPartitionLedger(
		devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a100", migCardWithProfiles("0", multi))),
		"a100", "0",
		map[string]int32{"3g.40gb": 1},
		map[string]int32{"1g.10gb": 4, "3g.40gb": 1})
	assert.True(t, acceleratorDetailChanged(empty, carved), "an allocation that shrinks another profile fires")
	assert.False(t, acceleratorDetailChanged(carved, carved), "an unchanged ledger does not fire")
}

// TestEnqueueNodeWhenDevicesChanged pins that a Devices ledger enqueues its name-identical Node.
func TestEnqueueNodeWhenDevicesChanged(t *testing.T) {
	r := &NodeCapacityReconciler{}
	devs := &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: "node-5"}}
	reqs := r.enqueueNodeWhenDevicesChanged(context.Background(), devs)
	require.Len(t, reqs, 1)
	assert.Equal(t, "node-5", reqs[0].Name)
	assert.Empty(t, reqs[0].Namespace, "node is cluster-scoped")
}

func TestIsOwnedCapacityKey(t *testing.T) {
	cases := []struct {
		name string
		in   core.ResourceName
		want bool
	}{
		{"owned units", "nvidia.com/gpu.sliced.units", true},
		{"owned cores-percentage", "nvidia.com/gpu.sliced.cores-percentage", true},
		{"owned memory-mib", "nvidia.com/gpu.sliced.memory-mib", true},
		{"owned partitioned units", "nvidia.com/gpu.partitioned.units", true},
		{"owned partition profile", "nvidia.com/gpu.partitioned.mig-1g.10gb", true},
		// A foreign extended resource that merely looks like a per-profile key is NOT owned, so
		// it is never nulled out when absent from desired — the reason to parse the key rather
		// than match a raw infix.
		{"foreign profile-shaped key not owned", "example.com/foo.partitioned.mig-1g.10gb", false},
		{"empty-profile key not owned", "nvidia.com/gpu.partitioned.mig-", false},
		// A fixed suffix is only ours behind a known accelerator base; another plugin's
		// same-shaped resource must never be claimed and then nulled out.
		{"foreign partitioned units not owned", "example.com/foo.partitioned.units", false},
		{"foreign sliced units not owned", "example.com/foo.sliced.units", false},
		{"bare sliced pool not owned", "nvidia.com/gpu.sliced", false},
		{"bare partitioned pool not owned", "nvidia.com/gpu.partitioned", false},
		{"bare shared pool not owned", "nvidia.com/gpu.shared", false},
		// The superseded MIG key is owned by nobody: nothing writes it and nothing strips it.
		{"superseded mig key not owned", "nvidia.com/gpu.sliced.mig-1g.10gb", false},
		{"unrelated resource", "cpu", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isOwnedCapacityKey(c.in))
		})
	}
}

func TestBuildAcceleratorCapacityPatch(t *testing.T) {
	const (
		units = "nvidia.com/gpu.sliced.units"
		mib   = "nvidia.com/gpu.sliced.memory-mib"
	)
	mkCap := func(entries map[string]string) core.ResourceList {
		out := core.ResourceList{}
		for k, v := range entries {
			out[core.ResourceName(k)] = resource.MustParse(v)
		}
		return out
	}

	cases := []struct {
		name    string
		desired core.ResourceList
		current core.ResourceList
		want    map[string]any // nil → no patch
	}{
		{
			// The patch carries the quantity's canonical string (6400000 → "6400k").
			name:    "set on a node without the key",
			desired: mkCap(map[string]string{units: "6400000"}),
			current: mkCap(map[string]string{"cpu": "8"}),
			want:    map[string]any{units: "6400k"},
		},
		{
			name:    "no change when already equal",
			desired: mkCap(map[string]string{units: "6400000"}),
			current: mkCap(map[string]string{units: "6400000", "cpu": "8"}),
			want:    nil,
		},
		{
			name:    "update when value differs",
			desired: mkCap(map[string]string{units: "6400000"}),
			current: mkCap(map[string]string{units: "3200000"}),
			want:    map[string]any{units: "6400k"},
		},
		{
			// Every owned ".sliced.*" suffix is nulled when it drops out of desired.
			name:    "remove stale sliced keys when the model disappears",
			desired: nil,
			current: mkCap(map[string]string{units: "6400000", mib: "98304", "cpu": "8"}),
			want:    map[string]any{units: nil, mib: nil},
		},
		{
			// A per-profile ".sliced.mig-<profile>" key is owned too, so it is reverse-patched to
			// null once the last card offering that profile leaves (MIG disabled).
			name:    "remove a stale per-profile key when the last card offering it leaves",
			desired: nil,
			current: mkCap(map[string]string{"nvidia.com/gpu.partitioned.mig-1g.10gb": "7", "cpu": "8"}),
			want:    map[string]any{"nvidia.com/gpu.partitioned.mig-1g.10gb": nil},
		},
		{
			// Nothing owns the superseded key any more, so it is left exactly where it is on a
			// node an earlier build wrote it onto.
			name:    "a superseded mig key is left alone, not reverse-patched",
			desired: nil,
			current: mkCap(map[string]string{"nvidia.com/gpu.sliced.mig-1g.10gb": "7", "cpu": "8"}),
			want:    nil,
		},
		{
			name:    "bare .sliced and .shared device-plugin keys are left untouched",
			desired: nil,
			current: mkCap(map[string]string{"nvidia.com/gpu.sliced": "2048", "nvidia.com/gpu.shared": "10", "cpu": "8"}),
			want:    nil,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := buildAcceleratorCapacityPatch(c.desired, c.current)
			if c.want == nil {
				assert.Nil(t, got)
				return
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestAcceleratorFamilyCapacityChanged(t *testing.T) {
	nvidiaSliced := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)
	mk := func(entries map[core.ResourceName]string) core.ResourceList {
		rl := core.ResourceList{core.ResourceCPU: resource.MustParse("8")}
		for k, v := range entries {
			rl[k] = resource.MustParse(v)
		}
		return rl
	}
	none := mk(nil)
	units := mk(map[core.ResourceName]string{nvidiaSlicedUnits: "6400000"})
	unitsLess := mk(map[core.ResourceName]string{nvidiaSlicedUnits: "3200000"})
	pool := mk(map[core.ResourceName]string{nvidiaSliced: "1024"})
	poolLess := mk(map[core.ResourceName]string{nvidiaSliced: "512"})

	// Owned ".sliced.*" keys.
	assert.False(t, acceleratorFamilyCapacityChanged(units, units), "owned key unchanged")
	assert.True(t, acceleratorFamilyCapacityChanged(units, unitsLess), "owned key value changed")
	assert.True(t, acceleratorFamilyCapacityChanged(units, none), "owned key removed")
	assert.True(t, acceleratorFamilyCapacityChanged(none, units), "owned key added")
	// Bare device-plugin ".sliced" token pool (the presence gate).
	assert.True(t, acceleratorFamilyCapacityChanged(none, pool), "bare .sliced pool added")
	assert.True(t, acceleratorFamilyCapacityChanged(pool, none), "bare .sliced pool removed")
	assert.True(t, acceleratorFamilyCapacityChanged(pool, poolLess), "bare .sliced pool value changed")
	// Only CPU changes are ignored.
	assert.False(t, acceleratorFamilyCapacityChanged(none, none), "only cpu, no change")
}

// TestNodeCapacityReconciler_Reconcile verifies the end-to-end node status patch:
// a managed acceleratable node gains the four ".sliced.*" keys, a re-run is a no-op,
// unmanaging the node removes them, and an unmanaged node is left alone.
func TestNodeCapacityReconciler_Reconcile(t *testing.T) {
	const node = "node-5"
	wantUnits := int64(4) * nodefeature.ResourceMaxUnits
	wantCores := int64(4) * 128 * 100 // NVIDIA overcommit: cards × maxSlices × 100
	wantMemMib := int64(4) * a10gMib

	build := func(nd *core.Node, objs ...ctrlcli.Object) ctrlcli.Client {
		if nd.Status.Capacity == nil {
			nd.Status.Capacity = core.ResourceList{}
		}
		nd.Status.Capacity[core.ResourceCPU] = resource.MustParse("8")
		return ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(append([]ctrlcli.Object{nd}, objs...)...).
			WithStatusSubresource(&core.Node{}).
			Build()
	}
	reconcile := func(cli ctrlcli.Client) error {
		r := &NodeCapacityReconciler{Client: cli}
		_, err := r.Reconcile(context.Background(),
			ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: node}})
		return err
	}
	get := func(cli ctrlcli.Client) *core.Node {
		nd := new(core.Node)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node}, nd))
		return nd
	}
	// val reads an extended-resource value; a map index is not addressable, so the
	// quantity must be copied to a local before calling its pointer method.
	val := func(rl core.ResourceList, n core.ResourceName) int64 {
		q := rl[n]
		return q.Value()
	}

	t.Run("managed node is patched with four keys then idempotent", func(t *testing.T) {
		nd := withSlicedPool(acceleratableNode(node, "nvidia-a10g", "4", "24Gi", true),
			nodefeature.ManufacturerNVIDIA, 4*128)
		devs := devicesWithSlicing(node, vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 4, 128, true})
		cli := build(nd, devs)

		require.NoError(t, reconcile(cli))
		capn := get(cli).Status.Capacity
		assert.Equal(t, wantUnits, val(capn, nvidiaSlicedUnits), "sliced units capacity")
		assert.Equal(t, wantCores, val(capn, nvidiaSlicedCores), "cores-percentage (overcommit)")
		assert.Equal(t, int64(4)*100, val(capn, nvidiaSlicedMemPct), "memory-percentage")
		assert.Equal(t, wantMemMib, val(capn, nvidiaSlicedMemMib), "memory-mib")
		cpu := capn[core.ResourceCPU]
		assert.Equal(t, "8", cpu.String(), "cpu capacity untouched")

		// Second reconcile is a no-op (capacity already matches).
		require.NoError(t, reconcile(cli))
		assert.Equal(t, wantUnits, val(get(cli).Status.Capacity, nvidiaSlicedUnits), "still present after no-op reconcile")
	})

	t.Run("mixed group converges each family onto its own cards", func(t *testing.T) {
		// End-to-end via the Devices watch's data source: 2 unpartitioned + 1 partitioned card.
		// Each family's units count only its own population, so the two never sum to more than
		// the node's cards.
		nd := withPartitionedPool(withSlicedPool(acceleratableNode(node, "nvidia-a10g", "3", "24Gi", true),
			nodefeature.ManufacturerNVIDIA, 2*128), nodefeature.ManufacturerNVIDIA, 7)
		devs := devicesWithGroups(node, slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g",
			softCard("0", 128, true), softCard("1", 128, true), migCard("2", 7)))
		cli := build(nd, devs)

		require.NoError(t, reconcile(cli))
		capn := get(cli).Status.Capacity
		assert.Equal(t, int64(2)*nodefeature.ResourceMaxUnits, val(capn, nvidiaSlicedUnits), "sliced units count the unpartitioned cards")
		assert.Equal(t, int64(1)*nodefeature.ResourceMaxUnits, val(capn, nvidiaPartitionedUnits), "partition units count the partitioned card")
		assert.Equal(t, int64(2)*128*100, val(capn, nvidiaSlicedCores), "cores from the 2 unpartitioned cards")
		assert.Equal(t, int64(2)*100, val(capn, nvidiaSlicedMemPct), "memory-percentage from the unpartitioned cards")
		assert.Equal(t, int64(2)*a10gMib, val(capn, nvidiaSlicedMemMib), "memory-mib from the unpartitioned cards")

		// A second pass with an unchanged ledger emits no patch at all.
		before := get(cli).ResourceVersion
		require.NoError(t, reconcile(cli))
		assert.Equal(t, before, get(cli).ResourceVersion, "an unchanged ledger must not re-patch the node")
	})

	t.Run("removing the sliced token pool reverse-patches all four keys", func(t *testing.T) {
		// A node that already carries the four keys but whose device-plugin dropped the
		// bare ".sliced" pool (gate fails) has them reverse-patched, even though the
		// Devices CR still reports the capability.
		nd := acceleratableNode(node, "nvidia-a10g", "4", "24Gi", true)
		nd.Status.Capacity = core.ResourceList{
			core.ResourceCPU:   resource.MustParse("8"),
			nvidiaSlicedUnits:  *resource.NewQuantity(wantUnits, resource.DecimalSI),
			nvidiaSlicedCores:  *resource.NewQuantity(wantCores, resource.DecimalSI),
			nvidiaSlicedMemPct: *resource.NewQuantity(int64(4)*100, resource.DecimalSI),
			nvidiaSlicedMemMib: *resource.NewQuantity(wantMemMib, resource.DecimalSI),
		}
		devs := devicesWithSlicing(node, vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 4, 128, true})
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(nd, devs).WithStatusSubresource(&core.Node{}).Build()

		require.NoError(t, reconcile(cli))
		capn := get(cli).Status.Capacity
		for _, n := range []core.ResourceName{nvidiaSlicedUnits, nvidiaSlicedCores, nvidiaSlicedMemPct, nvidiaSlicedMemMib} {
			_, ok := capn[n]
			assert.Falsef(t, ok, "%s reverse-patched when the sliced pool is absent", n)
		}
	})

	t.Run("unmanaging the node removes all sliced capacity", func(t *testing.T) {
		nd := acceleratableNode(node, "nvidia-a10g", "4", "24Gi", true)
		nd.Status.Capacity = core.ResourceList{
			core.ResourceCPU:   resource.MustParse("8"),
			nvidiaSlicedUnits:  *resource.NewQuantity(wantUnits, resource.DecimalSI),
			nvidiaSlicedMemMib: *resource.NewQuantity(wantMemMib, resource.DecimalSI),
		}
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(nd).WithStatusSubresource(&core.Node{}).Build()
		// Drop the managed label → node is no longer auto-managed.
		delete(nd.Labels, systemname.ManagedLabelKey)
		require.NoError(t, cli.Update(context.Background(), nd))

		require.NoError(t, reconcile(cli))
		capn := get(cli).Status.Capacity
		_, hasUnits := capn[nvidiaSlicedUnits]
		_, hasMib := capn[nvidiaSlicedMemMib]
		assert.False(t, hasUnits, "sliced units removed")
		assert.False(t, hasMib, "sliced memory-mib removed")
		cpu := capn[core.ResourceCPU]
		assert.Equal(t, "8", cpu.String(), "cpu capacity intact")
	})

	t.Run("unmanaged node is left alone", func(t *testing.T) {
		cli := build(acceleratableNode(node, "nvidia-a10g", "4", "24Gi", false))
		require.NoError(t, reconcile(cli))
		_, present := get(cli).Status.Capacity[nvidiaSlicedUnits]
		assert.False(t, present, "unmanaged node not patched")
	})
}
