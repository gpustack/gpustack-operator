package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const ledgerD = int32(nodefeature.ResourceMaxUnits)

// cardFree is a whole, untouched card (Remaining seeded at D).
func cardFree() workercore.AcceleratorAllocation {
	return workercore.AcceleratorAllocation{Mode: workercore.DeviceAllocationModeNone, Remaining: ledgerD}
}

// cardExclusive is a card wholly taken for exclusive use (no Remaining).
func cardExclusive() workercore.AcceleratorAllocation {
	return workercore.AcceleratorAllocation{Mode: workercore.DeviceAllocationModeExclusive, Remaining: 0}
}

// cardShared keeps `slots` of the SharedResourceMaxSize ownership shares free.
func cardShared(slots int32) workercore.AcceleratorAllocation {
	unit := ledgerD / int32(nodefeature.SharedResourceMaxSize)
	return workercore.AcceleratorAllocation{Mode: workercore.DeviceAllocationModeShared, Remaining: slots * unit}
}

// cardSliced keeps `pct` of the 100 VRAM-percent units free.
func cardSliced(pct int32) workercore.AcceleratorAllocation {
	unit := ledgerD / 100
	return workercore.AcceleratorAllocation{Mode: workercore.DeviceAllocationModeSliced, Remaining: pct * unit}
}

func repeatCard(n int, c workercore.AcceleratorAllocation) []workercore.AcceleratorAllocation {
	out := make([]workercore.AcceleratorAllocation, n)
	for i := range out {
		out[i] = c
	}
	return out
}

func concatCards(groups ...[]workercore.AcceleratorAllocation) []workercore.AcceleratorAllocation {
	var out []workercore.AcceleratorAllocation
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// nodeDevices builds a single-node Devices object whose status ledger holds cards.
func nodeDevices(name string, labels map[string]string, cards ...workercore.AcceleratorAllocation) workercore.Devices {
	return workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: name, Labels: labels},
		Status: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID:           "g0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: cards,
			}},
		},
	}
}

// profileCount is shorthand for one entry of a card's per-profile partition ledger.
func profileCount(name string, count int32) workercore.AcceleratorProfileCount {
	return workercore.AcceleratorProfileCount{Name: name, Count: count}
}

// cardPartitionedLedger is the ledger entry of a card in a hardware partitioning mode. Its
// scalar Remaining stays a whole card — the scalar ledger does not model partitions, which is
// exactly why an unscoped whole-card view would count it as free — and the per-profile
// remaining counts carry the real availability.
func cardPartitionedLedger(profiles ...workercore.AcceleratorProfileCount) workercore.AcceleratorAllocation {
	return workercore.AcceleratorAllocation{
		Mode:              workercore.DeviceAllocationModePartitioned,
		Remaining:         ledgerD,
		RemainingProfiles: profiles,
	}
}

// nodeCard pairs one card's reported capability (Devices.spec side) with its ledger entry
// (Devices.status side).
type nodeCard struct {
	capability workercore.AcceleratorStatus
	alloc      workercore.AcceleratorAllocation
}

// unpartitionedCard is a card offering logical slicing only.
func unpartitionedCard(alloc workercore.AcceleratorAllocation) nodeCard {
	return nodeCard{
		capability: workercore.AcceleratorStatus{
			LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 10},
		},
		alloc: alloc,
	}
}

// partitionedCard is a card in a hardware partitioning mode: ceiling is its physical-slice
// ceiling (the largest instance count across its profiles), profiles its remaining ledger.
func partitionedCard(ceiling int32, profiles ...workercore.AcceleratorProfileCount) nodeCard {
	return nodeCard{
		capability: workercore.AcceleratorStatus{
			PhysicalSliced: workercore.AcceleratorPhysicalSliced{Count: ceiling},
		},
		alloc: cardPartitionedLedger(profiles...),
	}
}

// nodeDevicesWithCapability builds a single-node Devices carrying both sides the views join:
// the capability in spec.groups[].accelerators[].status and the ledger in
// status.groups[].accelerators[], paired by card ID.
func nodeDevicesWithCapability(name string, cards ...nodeCard) workercore.Devices {
	specCards := make([]workercore.Accelerator, len(cards))
	ledgerCards := make([]workercore.AcceleratorAllocation, len(cards))
	for i, c := range cards {
		id := fmt.Sprintf("card-%d", i)
		specCards[i] = workercore.Accelerator{ID: id, Index: uint32(i), Status: c.capability}
		ledgerCards[i] = c.alloc
		ledgerCards[i].ID = id
		ledgerCards[i].Index = uint32(i)
	}
	dev := nodeDevices(name, nil, ledgerCards...)
	dev.Spec = workercore.DevicesSpec{
		Groups: []workercore.DevicesGroup{{
			ID:           "g0",
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: specCards,
		}},
	}
	return dev
}

func wantView(t *testing.T, v workercore.InstanceTypeResource, orm, rem, capacity int64, label string) {
	t.Helper()
	assert.Equalf(t, orm, v.OnceMaxRequest.Value(), "%s OnceMaxRequest", label)
	assert.Equalf(t, rem, v.Remaining.Value(), "%s Remaining", label)
	assert.Equalf(t, capacity, v.Capacity.Value(), "%s Capacity", label)
}

// TestAcceleratorThreeViews is the end-to-end acceptance oracle: the five-step
// pooling sequence on an 8× A10G node must reproduce the three-view progression
// exactly. On a single node OnceMaxRequest == Remaining for the exclusive and shared
// views; the sliced OnceMaxRequest is per-card (the freest card's percent), so it stays
// 100 while any card is free. Capacity stays the whole pool (8 / ×10 / ×100) throughout.
func TestAcceleratorThreeViews(t *testing.T) {
	cases := []struct {
		name                 string
		cards                []workercore.AcceleratorAllocation
		excl, shared, sliced int64
	}{
		{
			name:  "init: 8 free",
			cards: repeatCard(8, cardFree()),
			excl:  8, shared: 80, sliced: 800,
		},
		{
			name:  "step 1: 2 exclusive, 6 free",
			cards: concatCards(repeatCard(2, cardExclusive()), repeatCard(6, cardFree())),
			excl:  6, shared: 60, sliced: 600,
		},
		{
			name:  "step 2: 2 exclusive, 2 shared (9 free each), 4 free",
			cards: concatCards(repeatCard(2, cardExclusive()), repeatCard(2, cardShared(9)), repeatCard(4, cardFree())),
			excl:  4, shared: 58, sliced: 400,
		},
		{
			name:  "step 3: +2 sliced (80% free each), 2 free",
			cards: concatCards(repeatCard(2, cardExclusive()), repeatCard(2, cardShared(9)), repeatCard(2, cardSliced(80)), repeatCard(2, cardFree())),
			excl:  2, shared: 38, sliced: 360,
		},
		{
			name:  "step 4: the two sliced cards drop to 78% free",
			cards: concatCards(repeatCard(2, cardExclusive()), repeatCard(2, cardShared(9)), repeatCard(2, cardSliced(78)), repeatCard(2, cardFree())),
			excl:  2, shared: 38, sliced: 356,
		},
		{
			name:  "step 5: +1 exclusive, 1 free",
			cards: concatCards(repeatCard(3, cardExclusive()), repeatCard(2, cardShared(9)), repeatCard(2, cardSliced(78)), repeatCard(1, cardFree())),
			excl:  1, shared: 28, sliced: 256,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			devices := []workercore.Devices{nodeDevices("node-a", nil, c.cards...)}
			excl, shared, sliced, _ := getAcceleratorResources(devices)
			wantView(t, excl, c.excl, c.excl, 8, "exclusive")
			wantView(t, shared, c.shared, c.shared, 80, "shared")
			// Every step above leaves at least one free card, so the freest-card sliced OnceMaxRequest is 100.
			wantView(t, sliced, 100, c.sliced, 800, "sliced")
		})
	}
}

// TestAcceleratorThreeViews_MultiNode pins the per-node rollup: Remaining sums across nodes;
// exclusive/shared OnceMaxRequest is the largest single node (one allocation can span a node's
// cards), while sliced OnceMaxRequest is per-card (the freest card — 100 here, all free).
func TestAcceleratorThreeViews_MultiNode(t *testing.T) {
	devices := []workercore.Devices{
		nodeDevices("a", nil, repeatCard(4, cardFree())...),
		nodeDevices("b", nil, repeatCard(2, cardFree())...),
	}
	excl, shared, sliced, _ := getAcceleratorResources(devices)
	wantView(t, excl, 4, 6, 6, "exclusive")
	wantView(t, shared, 40, 60, 60, "shared")
	wantView(t, sliced, 100, 600, 600, "sliced")
}

// TestAcceleratorThreeViews_SlicedOnceMaxIsPerCard pins that the sliced OnceMaxRequest is the
// freest single card's percent — not a node's card-sum. With no free card it is the largest
// per-card remainder (40 here, not 40+30+20), while Remaining still sums the whole pool.
func TestAcceleratorThreeViews_SlicedOnceMaxIsPerCard(t *testing.T) {
	devices := []workercore.Devices{
		nodeDevices("a", nil, cardSliced(40), cardSliced(30), cardSliced(20)),
	}
	_, _, sliced, _ := getAcceleratorResources(devices)
	wantView(t, sliced, 40, 90, 300, "sliced")
}

// TestAcceleratorThreeViews_Empty pins that a pool with no Devices yields zeroed views
// rather than panicking.
func TestAcceleratorThreeViews_Empty(t *testing.T) {
	excl, shared, sliced, partitioned := getAcceleratorResources(nil)
	wantView(t, excl, 0, 0, 0, "exclusive")
	wantView(t, shared, 0, 0, 0, "shared")
	wantView(t, sliced, 0, 0, 0, "sliced")
	wantView(t, partitioned, 0, 0, 0, "partitioned")
}

// TestAcceleratorViews_LogicalOnlyPool pins that a pool of unpartitioned cards keeps the
// whole-card and logical-slice views it always reported, and reports no partition capacity.
func TestAcceleratorViews_LogicalOnlyPool(t *testing.T) {
	devices := []workercore.Devices{
		nodeDevicesWithCapability("a",
			unpartitionedCard(cardFree()),
			unpartitionedCard(cardSliced(80)),
		),
	}
	excl, shared, sliced, partitioned := getAcceleratorResources(devices)
	wantView(t, excl, 1, 1, 2, "exclusive")
	wantView(t, shared, 10, 10, 20, "shared")
	wantView(t, sliced, 100, 180, 200, "sliced")
	wantView(t, partitioned, 0, 0, 0, "partitioned")
}

// TestAcceleratorViews_PartitionOnlyPool pins that a pool whose cards are all in a hardware
// partitioning mode serves no whole-card or logical-slice claim — the scalar ledger of an empty
// partitioned card still reads a whole card, so an unscoped view would advertise capacity that
// cannot be admitted.
func TestAcceleratorViews_PartitionOnlyPool(t *testing.T) {
	devices := []workercore.Devices{
		nodeDevicesWithCapability("a",
			partitionedCard(7, profileCount("1g.5gb", 7), profileCount("2g.10gb", 3), profileCount("3g.20gb", 2)),
			partitionedCard(7, profileCount("1g.5gb", 7), profileCount("2g.10gb", 3), profileCount("3g.20gb", 2)),
		),
	}
	excl, shared, sliced, partitioned := getAcceleratorResources(devices)
	wantView(t, excl, 0, 0, 0, "exclusive")
	wantView(t, shared, 0, 0, 0, "shared")
	wantView(t, sliced, 0, 0, 0, "sliced")
	wantView(t, partitioned, 7, 14, 14, "partitioned")
}

// TestAcceleratorViews_MixedPool pins that every card contributes to exactly one side: the
// unpartitioned card alone backs the whole-card and logical-slice views, the partitioned card
// alone backs the partition view.
func TestAcceleratorViews_MixedPool(t *testing.T) {
	devices := []workercore.Devices{
		nodeDevicesWithCapability("a",
			unpartitionedCard(cardFree()),
			partitionedCard(7, profileCount("1g.5gb", 7), profileCount("2g.10gb", 3)),
		),
	}
	excl, shared, sliced, partitioned := getAcceleratorResources(devices)
	wantView(t, excl, 1, 1, 1, "exclusive")
	wantView(t, shared, 10, 10, 10, "shared")
	wantView(t, sliced, 100, 100, 100, "sliced")
	wantView(t, partitioned, 7, 7, 7, "partitioned")
}

// TestAcceleratorViews_PartitionSurvivesFirstAllocation pins that a card holding one small
// partition still advertises the further instances it can host — the view must not collapse to
// zero on the first allocation.
func TestAcceleratorViews_PartitionSurvivesFirstAllocation(t *testing.T) {
	devices := []workercore.Devices{
		nodeDevicesWithCapability("a",
			partitionedCard(7, profileCount("1g.5gb", 6), profileCount("2g.10gb", 2), profileCount("3g.20gb", 1)),
		),
	}
	_, _, _, partitioned := getAcceleratorResources(devices)
	wantView(t, partitioned, 6, 6, 7, "partitioned")
}

// TestAcceleratorViews_PartitionOnceMaxIsPerCard pins that the partition OnceMaxRequest is the
// freest single card — a partition request targets one card — not a node's card-sum.
func TestAcceleratorViews_PartitionOnceMaxIsPerCard(t *testing.T) {
	devices := []workercore.Devices{
		nodeDevicesWithCapability("a",
			partitionedCard(7, profileCount("1g.5gb", 4)),
			partitionedCard(7, profileCount("1g.5gb", 2)),
		),
		nodeDevicesWithCapability("b",
			partitionedCard(7, profileCount("1g.5gb", 5)),
		),
	}
	_, _, _, partitioned := getAcceleratorResources(devices)
	wantView(t, partitioned, 5, 11, 21, "partitioned")
}

// TestClusterQueueCPUResource pins the non-accelerated CPU view: capacity is the
// summed nominal CPU quota, remaining subtracts the reservation, and once-max is the
// largest single node's core count (from a flavor name) bounded by remaining.
func TestClusterQueueCPUResource(t *testing.T) {
	mkCQ := func(reservation *resource.Quantity) *kueue.ClusterQueue {
		cq := &kueue.ClusterQueue{
			Spec: kueue.ClusterQueueSpec{
				ResourceGroups: []kueue.ResourceGroup{{
					CoveredResources: []core.ResourceName{core.ResourceCPU},
					Flavors: []kueue.FlavorQuotas{{
						Name:      "gpustack-generic-linux-amd64-16c",
						Resources: []kueue.ResourceQuota{{Name: core.ResourceCPU, NominalQuota: resource.MustParse("48")}},
					}},
				}},
			},
		}
		if reservation != nil {
			cq.Status.FlavorsReservation = []kueue.FlavorUsage{{
				Name:      "gpustack-generic-linux-amd64-16c",
				Resources: []kueue.ResourceUsage{{Name: core.ResourceCPU, Total: *reservation}},
			}}
		}
		return cq
	}

	t.Run("no reservation: orm is the per-node core count", func(t *testing.T) {
		got := getCPUResource(mkCQ(nil), false)
		wantView(t, got, 16, 48, 48, "cpu")
	})
	t.Run("reservation reduces remaining; orm stays at the per-node count", func(t *testing.T) {
		got := getCPUResource(mkCQ(ptr.To(resource.MustParse("8"))), false)
		wantView(t, got, 16, 40, 48, "cpu")
	})
	t.Run("orm is bounded by remaining when little is left", func(t *testing.T) {
		got := getCPUResource(mkCQ(ptr.To(resource.MustParse("40"))), false)
		wantView(t, got, 8, 8, 48, "cpu")
	})
}

func TestPoolDevicesSelector(t *testing.T) {
	key := nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g"
	t.Run("extracts feature key + os/arch and adds managed", func(t *testing.T) {
		sel := poolDevicesSelector(map[string]string{
			key:                  "true",
			key + ".count":       "8",
			key + ".capacity":    "16",
			core.LabelOSStable:   "linux",
			core.LabelArchStable: "amd64",
		})
		assert.Equal(t, map[string]string{
			key:                        "true",
			core.LabelOSStable:         "linux",
			core.LabelArchStable:       "amd64",
			systemname.ManagedLabelKey: "true",
		}, sel)
	})
	t.Run("nil when no feature key (never matches every Devices)", func(t *testing.T) {
		assert.Nil(t, poolDevicesSelector(map[string]string{
			core.LabelOSStable:   "linux",
			core.LabelArchStable: "amd64",
		}))
	})
}

func TestParseNodeFlavorCount(t *testing.T) {
	cases := map[string]int64{
		"gpustack-generic-linux-amd64-16c":       16,
		"gpustack-nvidia-a10g-linux-amd64-4d":    4,
		"gpustack-amd-epyc-7763-linux-amd64-64c": 64,
		"gpustack-generic-linux-amd64":           0,
		"":                                       0,
		"garbage":                                0,
	}
	for name, want := range cases {
		assert.Equalf(t, want, parseNodeFlavorCount(name), "name %q", name)
	}
}

func buildInstanceTypeClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(&workercore.InstanceType{}).
		Build()
}

func reconcileInstanceTypeN(t *testing.T, cli ctrlcli.Client, name string, n int) {
	t.Helper()
	r := &InstanceTypeReconciler{Client: cli, APIReader: cli}
	for range n {
		_, err := r.Reconcile(context.Background(),
			ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
		require.NoError(t, err)
	}
}

// TestInstanceTypeReconciler_CreatesClusterQueue pins that an admin InstanceType gets a
// backing ClusterQueue carrying the schedule labels derived from its spec identity, the
// entrance label, StopPolicy None, and the fixed no-borrow isolation policy stamped into the
// spec at creation (not gated by derived-from-node) — but no resource groups (the
// NodeQueueReconciler owns the quota) and no unit-spec notes. The finalizer holds the type.
func TestInstanceTypeReconciler_CreatesClusterQueue(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.InstanceTypeSpec{
			// The schedule labels are derived from the spec identity, not the metadata labels.
			GeneralGroup: key,
			OS:           "linux",
			Arch:         "amd64",
			// A non-accelerated type's unit is one CPU core.
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "8Gi"},
			LocalStorage:  "64Gi",
		},
	}
	cli := buildInstanceTypeClient(it)
	reconcileInstanceTypeN(t, cli, name, 3)

	cq := new(kueue.ClusterQueue)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, cq))
	// Unaware (the unit binary default): a generic pool collapses to the acceleratable=false
	// discriminator, carrying no general.* key.
	assert.Equal(t, "false", cq.Labels[nodefeature.NodeAcceleratableLabelKey], "queue carries the acceleratable=false discriminator")
	assert.NotContains(t, cq.Labels, featureKeyLabel(false, key), "collapsed pool carries no general key")
	require.NotNil(t, cq.Spec.StopPolicy)
	assert.Equal(t, kueue.None, *cq.Spec.StopPolicy, "created active (StopPolicy None)")
	assert.Empty(t, cq.Spec.ResourceGroups, "no resource groups (the NodeQueueReconciler owns the quota)")

	// The fixed no-borrow isolation policy is stamped at creation even for an admin
	// (non-derived) InstanceType.
	assert.Empty(t, cq.Spec.CohortName, "no cohort (isolated)")
	require.NotNil(t, cq.Spec.FlavorFungibility)
	assert.Equal(t, kueue.TryNextFlavor, cq.Spec.FlavorFungibility.WhenCanBorrow, "try next flavor before borrowing")
	require.NotNil(t, cq.Spec.Preemption)
	assert.Equal(t, kueue.PreemptionPolicyNever, cq.Spec.Preemption.ReclaimWithinCohort, "never reclaim within cohort")
	assert.Equal(t, kueue.PreemptionPolicyLowerPriority, cq.Spec.Preemption.WithinClusterQueue, "preempt lower priority in-queue")

	_, notes := systemmeta.DescribeResource(cq)
	assert.NotContains(t, notes, "unitCPU", "unit spec is not a queue note")
	assert.NotContains(t, notes, "unitRAM", "unit spec is not a queue note")
	assert.NotContains(t, notes, "localStorage", "unit spec is not a queue note")

	got := new(workercore.InstanceType)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, got))
	assert.True(t, systemmeta.IsLocked(got), "finalizer held")
}

// TestInstanceTypeReconciler_RecreatesDeletedClusterQueue pins that an accidental delete of a
// live InstanceType's backing ClusterQueue self-heals: the next reconcile recreates the queue
// (the reconciler cares that the queue exists, not that it was never touched).
func TestInstanceTypeReconciler_RecreatesDeletedClusterQueue(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.InstanceTypeSpec{
			GeneralGroup:  key,
			OS:            "linux",
			Arch:          "amd64",
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
			LocalStorage:  "100Gi",
		},
	}
	cli := buildInstanceTypeClient(it)
	reconcileInstanceTypeN(t, cli, name, 2)

	// A user accidentally deletes the backing queue while the InstanceType still lives.
	cq, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.NoError(t, cli.Delete(context.Background(), cq))
	_, err = getClusterQueue(t, cli, name)
	require.True(t, kerrors.IsNotFound(err), "queue is gone before the reconcile")

	reconcileInstanceTypeN(t, cli, name, 1)

	_, err = getClusterQueue(t, cli, name)
	require.NoError(t, err, "backing queue recreated after an accidental deletion")
}

// TestInstanceTypeReconciler_RepointsFeatureKeyOnGroupChange pins that changing an InstanceType's
// accelerator group (mutable — only unitResources/localStorage are frozen on update) re-points its
// queue's feature-key label instead of leaving the stale one. A leftover key would make the flavor
// and device selectors (which AND every feature-key label on the queue) match no pool and strand
// it. (Accelerated groups always surface an acceleratable.* key regardless of awareness, so this
// exercises the prune deterministically in the unit binary.)
func TestInstanceTypeReconciler_RepointsFeatureKeyOnGroupChange(t *testing.T) {
	name := "gpustack--nvidia-a10g-linux-amd64"
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.InstanceTypeSpec{
			AcceleratorGroup: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64",
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
			LocalStorage:  "100Gi",
		},
	}
	cli := buildInstanceTypeClient(it)
	reconcileInstanceTypeN(t, cli, name, 2)

	cq, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.Equal(t, "true", cq.Labels[featureKeyLabel(true, "nvidia-a10g")], "initial feature key")

	// Admin re-points the accelerator group (group is not frozen by the update webhook).
	got := getInstanceType(t, cli, name)
	got.Spec.AcceleratorGroup = "nvidia-a10g-v2"
	require.NoError(t, cli.Update(context.Background(), got))
	reconcileInstanceTypeN(t, cli, name, 2)

	cq, err = getClusterQueue(t, cli, name)
	require.NoError(t, err)
	assert.Equal(t, "true", cq.Labels[featureKeyLabel(true, "nvidia-a10g-v2")], "feature key re-pointed")
	assert.NotContains(t, cq.Labels, featureKeyLabel(true, "nvidia-a10g"), "stale feature key pruned")
}

// TestInstanceTypeReconciler_RequeuesWhileBackingQueueTerminating pins that a backing queue
// caught mid-deletion (held by a finalizer, as Kueue holds it while draining) under a live
// InstanceType makes the reconcile requeue — it does not recreate under the same name yet, nor
// refresh status from the dying queue. Recreation happens on the later reconcile that finds it
// gone (covered by _RecreatesDeletedClusterQueue).
func TestInstanceTypeReconciler_RequeuesWhileBackingQueueTerminating(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: name, Finalizers: []string{systemmeta.LockedResourceFinalizer}},
		Spec: workercore.InstanceTypeSpec{
			GeneralGroup: key, OS: "linux", Arch: "amd64",
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
			LocalStorage:  "100Gi",
		},
	}
	cq := newInstanceTypeQueue(key, false)
	now := meta.Now()
	cq.DeletionTimestamp = &now
	cq.Finalizers = []string{"kueue.x-k8s.io/resource-in-use"} // held (draining) so it lingers
	cli := buildInstanceTypeClient(it, cq)

	r := &InstanceTypeReconciler{Client: cli, APIReader: cli}
	res, err := r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "requeues while the backing queue is terminating")

	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err, "terminating queue left to finish, not recreated under the same name")
	assert.NotNil(t, got.DeletionTimestamp, "the same terminating queue is still there")
}

// TestInstanceTypeReconciler_MaterializesStatus pins that the accelerated three-view is
// written to the InstanceType status from the Devices ledger. computeStatus reads
// acceleratable-ness from the spec (the defaulting webhook fills it at admission; a fake
// client does not), so the fixture sets it.Spec.Acceleratable directly. The reconciler no
// longer refreshes the hardware-descriptor spec from the queue notes.
func TestInstanceTypeReconciler_MaterializesStatus(t *testing.T) {
	key := "nvidia-a10g"
	name := nodeQueueName(key)
	featureKey := featureKeyLabel(true, key)
	poolLabels := map[string]string{
		featureKey:           "true",
		core.LabelOSStable:   "linux",
		core.LabelArchStable: "amd64",
	}

	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: name, Labels: poolLabels}}
	systemmeta.NoteResource(cq, _ClusterQueueResType, nil)

	devLabels := map[string]string{
		featureKey:                 "true",
		core.LabelOSStable:         "linux",
		core.LabelArchStable:       "amd64",
		systemname.ManagedLabelKey: "true",
	}
	dev := nodeDevices("node-a", devLabels, repeatCard(8, cardFree())...)

	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       name,
			Labels:     poolLabels,
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
		Spec: workercore.InstanceTypeSpec{
			AcceleratorGroup: key,
			Acceleratable:    true,
			OS:               "linux",
			Arch:             "amd64",
		},
	}
	cli := buildInstanceTypeClient(it, cq, &dev)
	reconcileInstanceTypeN(t, cli, name, 4)

	got := new(workercore.InstanceType)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, got))

	assert.Equal(t, nodefeature.FormatLocalQueueName(name), got.Status.Entrance,
		"status advertises the entrance LocalQueue name")

	assert.Equal(t, int64(8), got.Status.Accelerator.Remaining.Value(), "8 free cards exclusive")
	assert.Equal(t, int64(8), got.Status.Accelerator.Capacity.Value())
	assert.Equal(t, int64(80), got.Status.AcceleratorShared.Capacity.Value(), "×10 shared capacity")
	assert.Equal(t, int64(800), got.Status.AcceleratorSliced.Capacity.Value(), "×100 sliced capacity")
}

// TestInstanceTypeReconciler_StatusFreshOnLedgerChange pins the watch-freshness
// contract the materialized CRD buys (Direction 2): a pod alloc/free moves the Devices
// ledger, and the reconcile that observes the change rewrites .status — so a native
// watch on the InstanceType sees the new three-view. A reconcile that observes no
// ledger change writes nothing (the DeepEqual guard), so watchers get no spurious event.
func TestInstanceTypeReconciler_StatusFreshOnLedgerChange(t *testing.T) {
	key := "nvidia-a10g"
	name := nodeQueueName(key)
	featureKey := featureKeyLabel(true, key)
	poolLabels := map[string]string{
		featureKey:           "true",
		core.LabelOSStable:   "linux",
		core.LabelArchStable: "amd64",
	}

	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: name, Labels: poolLabels}}
	systemmeta.NoteResource(cq, _ClusterQueueResType, nil)

	devLabels := map[string]string{
		featureKey:                 "true",
		core.LabelOSStable:         "linux",
		core.LabelArchStable:       "amd64",
		systemname.ManagedLabelKey: "true",
	}
	dev := nodeDevices("node-a", devLabels, repeatCard(8, cardFree())...)

	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       name,
			Labels:     poolLabels,
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
		Spec: workercore.InstanceTypeSpec{
			AcceleratorGroup: key,
			Acceleratable:    true,
			OS:               "linux",
			Arch:             "amd64",
		},
	}
	cli := buildInstanceTypeClient(it, cq, &dev)

	// Settle: 8 free cards → the exclusive view reports 8 free whole cards.
	reconcileInstanceTypeN(t, cli, name, 4)
	assert.Equal(t, int64(8), getInstanceType(t, cli, name).Status.Accelerator.Remaining.Value(),
		"initial exclusive view = 8 free cards")

	// A no-op reconcile (ledger unchanged) must not rewrite .status: no write means no
	// spurious watch event.
	rv := getInstanceType(t, cli, name).ResourceVersion
	reconcileInstanceTypeN(t, cli, name, 2)
	assert.Equal(t, rv, getInstanceType(t, cli, name).ResourceVersion,
		"unchanged ledger writes no status (DeepEqual guard)")

	// A pod alloc moves the ledger (2 exclusive, 2 shared with 9 shares free, 4 free =
	// the step-2 oracle); the whole three-view moves within the reconcile.
	setNodeDevicesLedger(t, cli, "node-a",
		concatCards(repeatCard(2, cardExclusive()), repeatCard(2, cardShared(9)), repeatCard(4, cardFree())))
	reconcileInstanceTypeN(t, cli, name, 2)
	moved := getInstanceType(t, cli, name)
	assert.Equal(t, int64(4), moved.Status.Accelerator.Remaining.Value(), "alloc drops exclusive to 4")
	assert.Equal(t, int64(58), moved.Status.AcceleratorShared.Remaining.Value(), "alloc moves shared to 58")
	assert.Equal(t, int64(400), moved.Status.AcceleratorSliced.Remaining.Value(), "alloc moves sliced to 400")
	assert.NotEqual(t, rv, moved.ResourceVersion, "status rewritten within the reconcile")

	// A pod free returns the ledger to 8 free cards; the view recovers.
	setNodeDevicesLedger(t, cli, "node-a", repeatCard(8, cardFree()))
	reconcileInstanceTypeN(t, cli, name, 2)
	assert.Equal(t, int64(8), getInstanceType(t, cli, name).Status.Accelerator.Remaining.Value(),
		"free restores the exclusive view to 8")
}

// TestInstanceTypeReconciler_ComputesDetail pins the observed Status.Detail backfill (T8): an
// accelerated type gains its hardware descriptor (manufacturer/product/family + per-card
// memory/cores + the pool-aggregated SlicedDetail) from the matched ResourceFlavor's notes and
// the Devices ledger, the DisplayName defaults to the observed Product, and the Detail is
// recomputed every reconcile (it lives in computeStatus, so a second pass never erases it). A
// CPU-manufacturer-agnostic collapsed pool has no representative flavor, so its Detail stays empty
// and its DisplayName defaults to the "CPU-only" sentinel — its queue still activates.
func TestInstanceTypeReconciler_ComputesDetail(t *testing.T) {
	t.Run("accelerated type gains accelerator Detail and it persists across reconciles", func(t *testing.T) {
		key := "nvidia-a10g"
		name := nodeQueueName(key)
		featureKey := featureKeyLabel(true, key)
		poolLabels := map[string]string{
			featureKey:           "true",
			core.LabelOSStable:   "linux",
			core.LabelArchStable: "amd64",
		}

		cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: name, Labels: poolLabels}}
		systemmeta.NoteResource(cq, _ClusterQueueResType, nil)

		rf := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-2d", key, 2, 2,
			accelerated(nodefeature.ManufacturerNVIDIA),
			withNotes(map[string]string{
				"product": "A10G", "family": "Ampere", "memory": "24576Mi", "cores": "9216",
			}))

		// The Devices ledger carries the per-card slicing detail (Spec.Groups) the reconciler
		// aggregates into SlicedDetail, plus the pool labels listFlavorPoolDevices selects on. The
		// group ID is the bare model ("a10g"); Manufacturer+"-"+ID reconstructs the accelerator key.
		dev := devicesWithGroups("node-a", slicedGroup(nodefeature.ManufacturerNVIDIA, "a10g",
			logicalCard("0", 128, true), logicalCard("1", 128, true)))
		dev.Labels = map[string]string{
			featureKey:                 "true",
			core.LabelOSStable:         "linux",
			core.LabelArchStable:       "amd64",
			systemname.ManagedLabelKey: "true",
		}

		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{
				Name:       name,
				Labels:     poolLabels,
				Finalizers: []string{systemmeta.LockedResourceFinalizer},
			},
			Spec: workercore.InstanceTypeSpec{
				AcceleratorGroup: key, Acceleratable: true, OS: "linux", Arch: "amd64",
			},
		}
		cli := buildInstanceTypeClient(it, cq, rf, dev)
		reconcileInstanceTypeN(t, cli, name, 4)

		got := getInstanceType(t, cli, name)
		d := got.Status.Detail
		assert.Equal(t, nodefeature.ManufacturerNVIDIA, d.Manufacturer, "manufacturer from the flavor note")
		assert.Equal(t, "A10G", d.Product, "product from the flavor note")
		assert.Equal(t, "Ampere", d.Family, "family from the flavor note")
		assert.Equal(t, "24576Mi", d.Memory, "per-card VRAM from the flavor note")
		assert.Equal(t, "9216", d.Cores, "cores from the flavor note")
		assert.Equal(t, int32(256), d.SlicedDetail.Logical.Count,
			"SlicedDetail sums the two logically sliceable cards' counts (2×128)")
		assert.True(t, d.SlicedDetail.Logical.CoresPercentageOvercommit,
			"SlicedDetail carries the overcommit flag")

		// The Detail lives in computeStatus, so later reconciles recompute it identically — never
		// erase it — and a stable status writes nothing (the DeepEqual guard).
		rv := got.ResourceVersion
		reconcileInstanceTypeN(t, cli, name, 2)
		again := getInstanceType(t, cli, name)
		assert.Equal(t, rv, again.ResourceVersion, "a stable Detail writes nothing (DeepEqual guard)")
		assert.Equal(t, "A10G", again.Status.Detail.Product, "Detail is not erased on a later reconcile")
		assert.Equal(t, int32(256), again.Status.Detail.SlicedDetail.Logical.Count,
			"SlicedDetail is not erased on a later reconcile")
	})

	t.Run("generic collapsed pool: empty Detail, queue activates (not deadlocked)", func(t *testing.T) {
		key := "generic"
		name := nodeQueueName(key)
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: name, Finalizers: []string{systemmeta.LockedResourceFinalizer}},
			Spec: workercore.InstanceTypeSpec{
				GeneralGroup: key, OS: "linux", Arch: "amd64",
				UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
				LocalStorage:  "100Gi",
			},
		}
		cli := buildInstanceTypeClient(it)
		reconcileInstanceTypeN(t, cli, name, 4)

		got := getInstanceType(t, cli, name)
		assert.Equal(t, workercore.InstanceTypeDetail{}, got.Status.Detail,
			"a collapsed generic pool has no representative flavor, so Detail stays empty")

		cq, err := getClusterQueue(t, cli, name)
		require.NoError(t, err, "the queue is created despite the empty Detail (not deadlocked)")
		require.NotNil(t, cq.Spec.StopPolicy)
		assert.Equal(t, kueue.None, *cq.Spec.StopPolicy, "queue is active")
	})
}

// TestFoldDetailCPU pins the cpuDetail note → Status.Detail folding: an accelerated flavor's note
// (an InstanceTypeAcceleratorCPU carrying the CPU's own manufacturer/product/family) folds into
// the accelerator's CPU; a CPU flavor's note (a plain InstanceTypeCPU) folds into the top-level
// CPU; a malformed or empty note leaves the Detail untouched. (The awareness gate that selects
// which branch runs mirrors the flavor reconciler's cpuDetail producer, covered there.)
func TestFoldDetailCPU(t *testing.T) {
	t.Run("accelerated note folds into the accelerator CPU", func(t *testing.T) {
		note := string(json.ShouldMarshal(workercore.InstanceTypeAcceleratorCPU{
			Manufacturer:    "amd",
			Product:         "EPYC 7763",
			InstanceTypeCPU: workercore.InstanceTypeCPU{PhysicalCores: "64"},
		}))
		var d workercore.InstanceTypeDetail
		foldDetailCPU(&d, note, true)
		assert.Equal(t, "amd", d.CPU.Manufacturer)
		assert.Equal(t, "EPYC 7763", d.CPU.Product)
		assert.Equal(t, "64", d.CPU.PhysicalCores)
		assert.Empty(t, d.PhysicalCores, "the top-level CPU is untouched on the accelerated path")
	})
	t.Run("cpu-only note folds into the top-level CPU", func(t *testing.T) {
		note := string(json.ShouldMarshal(workercore.InstanceTypeCPU{PhysicalCores: "32", LogicalCores: "64"}))
		var d workercore.InstanceTypeDetail
		foldDetailCPU(&d, note, false)
		assert.Equal(t, "32", d.PhysicalCores)
		assert.Equal(t, "64", d.LogicalCores)
	})
	t.Run("empty and malformed notes leave the detail untouched", func(t *testing.T) {
		var d workercore.InstanceTypeDetail
		foldDetailCPU(&d, "", false)
		foldDetailCPU(&d, "{not json", false)
		foldDetailCPU(&d, "{not json", true)
		assert.Equal(t, workercore.InstanceTypeDetail{}, d)
	})
}

// TestInstanceTypeReconciler_TeardownFinalizer pins the delete handshake: deleting an
// InstanceType deletes the backing queue (the NodeQueueReconciler drains it in-cluster) and
// releases the finalizer once the queue has actually disappeared.
func TestInstanceTypeReconciler_TeardownFinalizer(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       name,
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
	}
	// No finalizer on the queue: our delete removes it at once (in-cluster Kueue's finalizer
	// would hold it until drained).
	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: name}}
	systemmeta.NoteResource(cq, _ClusterQueueResType, map[string]string{"acceleratable": "false"})
	cli := buildInstanceTypeClient(it, cq)

	// Deleting an InstanceType with a finalizer stamps its deletion timestamp.
	require.NoError(t, cli.Delete(context.Background(), it))

	// First reconcile deletes the queue; the second finds it gone and releases the finalizer.
	reconcileInstanceTypeN(t, cli, name, 2)

	_, err := getClusterQueue(t, cli, name)
	assert.Truef(t, kerrors.IsNotFound(err), "backing queue must be deleted, got err=%v", err)
	err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, new(workercore.InstanceType))
	assert.True(t, kerrors.IsNotFound(err), "instance type released once the queue is gone")
}

// TestInstanceTypeReconciler_TeardownWaitsForQueueRemoval pins that while the backing queue is
// still terminating (held by a finalizer — as Kueue holds it while draining), the InstanceType's
// own finalizer keeps holding it: the teardown requests the delete once and waits.
func TestInstanceTypeReconciler_TeardownWaitsForQueueRemoval(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       name,
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
	}
	// A finalizer on the queue simulates Kueue holding it (draining) after our delete request,
	// so it lingers as terminating rather than vanishing.
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: name, Finalizers: []string{"kueue.x-k8s.io/resource-in-use"}},
	}
	systemmeta.NoteResource(cq, _ClusterQueueResType, map[string]string{"acceleratable": "false"})
	cli := buildInstanceTypeClient(it, cq)
	require.NoError(t, cli.Delete(context.Background(), it))

	reconcileInstanceTypeN(t, cli, name, 3)

	gotCQ, err := getClusterQueue(t, cli, name)
	require.NoError(t, err, "queue held by its finalizer must not vanish")
	assert.NotNil(t, gotCQ.DeletionTimestamp, "teardown requested the queue's deletion")
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, new(workercore.InstanceType)),
		"instance type held until the queue is gone")
}

// TestInstanceTypeReconciler_EnqueuesInstanceTypesFromDevices pins that a Devices change enqueues
// every InstanceType whose pool the node serves, resolved by the schedule labels the Default
// webhook stamps (feature key + os/arch) rather than by name — so admin-named types are found, a
// node serving both its CPU and device pool enqueues both, and a mismatched os/arch is excluded.
func TestInstanceTypeReconciler_EnqueuesInstanceTypesFromDevices(t *testing.T) {
	cpuIT := &workercore.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "admin-cpu", Labels: map[string]string{
		featureKeyLabel(false, "generic"): "true",
		core.LabelOSStable:                "linux",
		core.LabelArchStable:              "amd64",
	}}}
	gpuIT := &workercore.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "admin-gpu", Labels: map[string]string{
		featureKeyLabel(true, "nvidia-a10g"): "true",
		core.LabelOSStable:                   "linux",
		core.LabelArchStable:                 "amd64",
	}}}
	// Same general group but a different arch: must not be enqueued for a linux/amd64 node.
	otherIT := &workercore.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "other-arch", Labels: map[string]string{
		featureKeyLabel(false, "generic"): "true",
		core.LabelOSStable:                "linux",
		core.LabelArchStable:              "arm64",
	}}}
	cli := buildInstanceTypeClient(cpuIT, gpuIT, otherIT)
	r := &InstanceTypeReconciler{Client: cli, APIReader: cli}

	// A GPU node's Devices carries both feature keys (it serves both pools) plus os/arch + managed.
	devices := &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: "node-g", Labels: map[string]string{
		systemname.ManagedLabelKey:           "true",
		featureKeyLabel(false, "generic"):    "true",
		featureKeyLabel(true, "nvidia-a10g"): "true",
		core.LabelOSStable:                   "linux",
		core.LabelArchStable:                 "amd64",
	}}}

	reqs := r.enqueueInstanceTypeWhenDevicesChanged(context.Background(), devices)

	names := make([]string, 0, len(reqs))
	for _, req := range reqs {
		names = append(names, req.Name)
	}
	assert.ElementsMatch(t, []string{"admin-cpu", "admin-gpu"}, names,
		"both pools the node serves are enqueued by label; the arm64 type is excluded")
}

// TestInstanceTypeReconciler_SyncInactive pins the Inactive<->StopPolicy truth table: the forward
// direction drives the Hold<->None pair from Spec.Inactive, the one-way mirror backfills
// Inactive=true for a stopped queue, a HoldAndDrain (owned by the NodeQueueReconciler) is never
// downgraded to Hold, and every row converges to a state that reconciles without further writes.
func TestInstanceTypeReconciler_SyncInactive(t *testing.T) {
	cases := []struct {
		name string

		startPolicy kueue.StopPolicy
		inactive    bool

		wantPolicy   kueue.StopPolicy
		wantInactive bool
	}{
		{"active stays active", kueue.None, false, kueue.None, false},
		{"inactive holds the active queue", kueue.None, true, kueue.Hold, true},
		{"held inactive is stable", kueue.Hold, true, kueue.Hold, true},
		{"cleared inactive releases the hold", kueue.Hold, false, kueue.None, false},
		{"draining inactive is not downgraded", kueue.HoldAndDrain, true, kueue.HoldAndDrain, true},
		{"draining backfills inactive (drain wins)", kueue.HoldAndDrain, false, kueue.HoldAndDrain, true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			key := "nvidia-a10g"
			name := nodeQueueName(key)
			it := &workercore.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: name, Finalizers: []string{systemmeta.LockedResourceFinalizer}},
				Spec: workercore.InstanceTypeSpec{
					AcceleratorGroup: key,
					Acceleratable:    true,
					OS:               "linux",
					Arch:             "amd64",
					Inactive:         c.inactive,
					UnitResources:    workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
					LocalStorage:     "100Gi",
				},
			}
			cq := newInstanceTypeQueue(key, true)
			cq.Spec.StopPolicy = ptr.To(c.startPolicy)
			cli := buildInstanceTypeClient(it, cq)

			// Converge.
			reconcileInstanceTypeN(t, cli, name, 4)

			gotCQ, err := getClusterQueue(t, cli, name)
			require.NoError(t, err)
			require.NotNil(t, gotCQ.Spec.StopPolicy)
			assert.Equal(t, c.wantPolicy, *gotCQ.Spec.StopPolicy, "converged StopPolicy")
			assert.Equal(t, c.wantInactive, getInstanceType(t, cli, name).Spec.Inactive, "converged Inactive")

			// Stable: once converged, further reconciles write nothing (no oscillation).
			cqRV := gotCQ.ResourceVersion
			itRV := getInstanceType(t, cli, name).ResourceVersion
			reconcileInstanceTypeN(t, cli, name, 2)
			stableCQ, err := getClusterQueue(t, cli, name)
			require.NoError(t, err)
			assert.Equal(t, cqRV, stableCQ.ResourceVersion, "StopPolicy stable (no write)")
			assert.Equal(t, itRV, getInstanceType(t, cli, name).ResourceVersion, "Inactive stable (no write)")
		})
	}
}

// --- shared ResourceFlavor fixtures (the pool the reconciler aggregates) ---

// flavorOpt mutates the notes a test ResourceFlavor carries, so a fixture can be
// flipped accelerated or given a specific unit spec.
type flavorOpt func(notes map[string]string)

func accelerated(manufacturer string) flavorOpt {
	return func(notes map[string]string) {
		notes["acceleratable"] = "true"
		notes["manufacturer"] = manufacturer
	}
}

// withNotes overlays additional flavor notes (product/family/memory/cores/cpuDetail), so a
// fixture can carry the descriptor the reconciler folds into Status.Detail.
func withNotes(kv map[string]string) flavorOpt {
	return func(notes map[string]string) {
		for k, v := range kv {
			notes[k] = v
		}
	}
}

// withGeneralKey stamps the general.<gKey>=true selector label an accelerated flavor also carries
// (so an aware generic pool can be excluded from it) — without a .capacity sibling, mirroring the
// real NodeFlavorReconciler. It is applied by newNodesFlavor after the labels are built.
func withGeneralKey(gKey string) flavorOpt {
	return func(notes map[string]string) {
		notes["_generalKey"] = gKey
	}
}

// newNodesFlavor builds a "nodes" ResourceFlavor carrying the schedule labels the
// reconciler reads (the feature key, the generic-vs-accelerated boolean, kubernetes.io/os|arch,
// and the key's .count/.capacity siblings) and the per-flavor notes. The device ("d") suffix is
// used when accelerated.
func newNodesFlavor(name, key string, count, capacity int64, opts ...flavorOpt) *kueue.ResourceFlavor {
	notes := map[string]string{
		"acceleratable": "false",
		"manufacturer":  "generic",
		"product":       "",
		"family":        "",
		"memory":        "",
	}
	for _, o := range opts {
		o(notes)
	}
	acceleratable := notes["acceleratable"] == "true"
	keyLabel := featureKeyLabel(acceleratable, key)
	labels := map[string]string{
		keyLabel:                                      "true",
		nodefeature.NodeAcceleratableLabelKey:         notes["acceleratable"],
		core.LabelOSStable:                            "linux",
		core.LabelArchStable:                          "amd64",
		keyLabel + _ResourceFlavorCountLabelSuffix:    itoa(count),
		keyLabel + _ResourceFlavorCapacityLabelSuffix: itoa(capacity),
	}
	// An accelerated flavor also carries the general.<gKey> selector label (no .capacity sibling);
	// withGeneralKey requests it via a sentinel note that never reaches the stored notes.
	if gk := notes["_generalKey"]; gk != "" {
		labels[nodefeature.GeneralFeatureLabelPrefix+gk] = "true"
		delete(notes, "_generalKey")
	}
	rf := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{Name: name, Labels: labels},
	}
	systemmeta.NoteResource(rf, _ResourceFlavorResType, notes)
	return rf
}

// nodeQueueName is the pool (ClusterQueue / InstanceType) name a flavor with the given key feeds
// when CPU-manufacturer awareness is off (the unit binary default): a generic CPU pool collapses
// under "generic" and an accelerated pool under its accelerator key, both named
// "gpustack--${key}-linux-amd64".
func nodeQueueName(key string) string {
	return fmt.Sprintf("gpustack--%s-linux-amd64", key)
}

func getClusterQueue(t *testing.T, cli ctrlcli.Client, name string) (*kueue.ClusterQueue, error) {
	t.Helper()
	cq := new(kueue.ClusterQueue)
	err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, cq)
	return cq, err
}

func getInstanceType(t *testing.T, cli ctrlcli.Client, name string) *workercore.InstanceType {
	t.Helper()
	it := new(workercore.InstanceType)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, it))
	return it
}

// setNodeDevicesLedger replaces a Devices object's per-card ledger, simulating a pod
// alloc/free below the reconciler.
func setNodeDevicesLedger(t *testing.T, cli ctrlcli.Client, node string, cards []workercore.AcceleratorAllocation) {
	t.Helper()
	dev := new(workercore.Devices)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node}, dev))
	dev.Status = nodeDevices(node, dev.Labels, cards...).Status
	require.NoError(t, cli.Update(context.Background(), dev))
}

// enableInstanceTypeDerivedFromNode writes the delegated settings Secret into the
// shared loopback client so InstanceTypeDerivedFromNode resolves to true (it gates the
// reconciler's derived authoring + queue isolation). The setting value caches for 30s
// once read successfully, so this is one-way within a test binary: once any test
// enables it the value stays true.
func enableInstanceTypeDerivedFromNode(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	cli := system.LoopbackCtrlClient.Get()
	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      setting.DelegatedSecretName,
			Namespace: setting.DelegatedSecretNamespace,
		},
		Data: map[string][]byte{"instance-type-derived-from-node": []byte("true")},
	}
	if err := cli.Create(ctx, sec); err != nil {
		got := new(core.Secret)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(sec), got))
		got.Data = sec.Data
		require.NoError(t, cli.Update(ctx, got))
	}
	require.True(t, settings.InstanceTypeDerivedFromNode.ShouldValueBool(ctx),
		"setting must read true after enabling")
}

// TestInstanceTypeReconciler_AlignsClusterQueue pins the InstanceType-owned metadata of a
// derived pool's backing ClusterQueue: it is isolated (empty cohort) and Active (StopPolicy
// None) — but the InstanceTypeReconciler never fills the resource groups (the
// NodeQueueReconciler owns the quota) and never writes the unit spec as a queue note.
func TestInstanceTypeReconciler_AlignsClusterQueue(t *testing.T) {
	cases := []struct {
		name          string
		key           string
		acceleratable bool
	}{
		{name: "cpu-only pool", key: "generic"},
		{name: "accelerated pool", key: "nvidia-a10g", acceleratable: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			name := nodeQueueName(c.key)
			spec := workercore.InstanceTypeSpec{
				Acceleratable: c.acceleratable,
				OS:            "linux",
				Arch:          "amd64",
				UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
				LocalStorage:  "100Gi",
			}
			if c.acceleratable {
				spec.AcceleratorGroup = c.key
			} else {
				spec.GeneralGroup = c.key
			}
			it := &workercore.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: name},
				Spec:       spec,
			}
			cli := buildInstanceTypeClient(it)

			// Reconcile creates + isolates the backing CQ from the InstanceType spec identity.
			reconcileInstanceTypeN(t, cli, name, 4)

			cq, err := getClusterQueue(t, cli, name)
			require.NoError(t, err)

			// Isolation + active state.
			assert.Empty(t, cq.Spec.CohortName, "cohortName empty (isolated)")
			require.NotNil(t, cq.Spec.StopPolicy)
			assert.Equal(t, kueue.None, *cq.Spec.StopPolicy, "active (StopPolicy None)")

			// The InstanceTypeReconciler does not fill the quota — the NodeQueueReconciler does.
			assert.Empty(t, cq.Spec.ResourceGroups, "no resource groups (the NodeQueueReconciler owns the quota)")

			// The unit spec is never a queue note (it lives on the InstanceType).
			resType, notes := systemmeta.DescribeResource(cq)
			assert.Equal(t, _ClusterQueueResType, resType, "resType")
			for _, k := range []string{"unitCPU", "unitRAM", "localStorage"} {
				assert.NotContainsf(t, notes, k, "unit spec is not a queue note (%q)", k)
			}
		})
	}
}
