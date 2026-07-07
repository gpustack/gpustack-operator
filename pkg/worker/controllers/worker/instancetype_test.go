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
			excl, shared, sliced := getAcceleratorResources(devices)
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
	excl, shared, sliced := getAcceleratorResources(devices)
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
	_, _, sliced := getAcceleratorResources(devices)
	wantView(t, sliced, 40, 90, 300, "sliced")
}

// TestAcceleratorThreeViews_Empty pins that a pool with no Devices yields zeroed views
// rather than panicking.
func TestAcceleratorThreeViews_Empty(t *testing.T) {
	excl, shared, sliced := getAcceleratorResources(nil)
	wantView(t, excl, 0, 0, 0, "exclusive")
	wantView(t, shared, 0, 0, 0, "shared")
	wantView(t, sliced, 0, 0, 0, "sliced")
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
			key:                  valueTrue,
			key + ".count":       "8",
			key + ".capacity":    "16",
			core.LabelOSStable:   "linux",
			core.LabelArchStable: "amd64",
		})
		assert.Equal(t, map[string]string{
			key:                        valueTrue,
			core.LabelOSStable:         "linux",
			core.LabelArchStable:       "amd64",
			systemname.ManagedLabelKey: valueTrue,
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
		WithIndex(&kueue.ResourceFlavor{}, IndexingResourceFlavorByNodeQueue, indexResourceFlavorByNodeQueue).
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

// TestInstanceTypeReconciler_CreatesClusterQueue pins that an admin InstanceType with
// a unit spec gets a backing ClusterQueue carrying its schedule labels + the unit
// notes, and is held by the finalizer.
func TestInstanceTypeReconciler_CreatesClusterQueue(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				featureKeyLabel(false, key): valueTrue,
				core.LabelOSStable:          "linux",
				core.LabelArchStable:        "amd64",
			},
		},
		Spec: workercore.InstanceTypeSpec{
			// A non-accelerated type's unit is one CPU core (unitCPU is pinned to 1).
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "8Gi"},
			LocalStorage:  "64Gi",
		},
	}
	cli := buildInstanceTypeClient(it)
	reconcileInstanceTypeN(t, cli, name, 3)

	cq := new(kueue.ClusterQueue)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, cq))
	assert.Equal(t, valueTrue, cq.Labels[featureKeyLabel(false, key)], "queue carries the feature key")
	assert.Equal(t, nodefeature.FormatLocalQueueName(name), cq.Labels[QueueEntranceLabelKey],
		"queue advertises its entrance LocalQueue name")
	_, notes := systemmeta.DescribeResource(cq)
	assert.NotContains(t, notes, "unitCPU", "unit spec is not a queue note")
	assert.NotContains(t, notes, "unitRAM", "unit spec is not a queue note")
	assert.NotContains(t, notes, "localStorage", "unit spec is not a queue note")

	got := new(workercore.InstanceType)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, got))
	assert.True(t, systemmeta.IsLocked(got), "finalizer held")
}

// TestInstanceTypeReconciler_DerivedAuthorsInstanceType pins that, with
// instance-type-derived-from-node enabled, a ResourceFlavor pool that has no
// InstanceType gets one authored (marked derived, carrying the schedule labels).
func TestInstanceTypeReconciler_DerivedAuthorsInstanceType(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)
	key := "nvidia-a10g"
	name := nodeQueueName(key)
	rf := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", key, 1, 4, accelerated(nodefeature.ManufacturerNVIDIA))
	cli := buildInstanceTypeClient(rf)

	reconcileInstanceTypeN(t, cli, name, 1)

	it := new(workercore.InstanceType)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, it))
	assert.Equal(t, valueTrue, it.Labels[_InstanceTypeDerivedFromNodeLabel], "marked derived")
	assert.Equal(t, valueTrue, it.Labels[featureKeyLabel(true, key)], "carries the feature key")
}

// TestInstanceTypeReconciler_MaterializesStatus pins that the accelerated three-view
// is written to the InstanceType status from the Devices ledger, and the hardware
// descriptors are refreshed from the queue notes.
func TestInstanceTypeReconciler_MaterializesStatus(t *testing.T) {
	key := "nvidia-a10g"
	name := nodeQueueName(key)
	featureKey := featureKeyLabel(true, key)
	poolLabels := map[string]string{
		featureKey:           valueTrue,
		core.LabelOSStable:   "linux",
		core.LabelArchStable: "amd64",
	}

	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: name, Labels: poolLabels}}
	systemmeta.NoteResource(cq, _ClusterQueueResType, map[string]string{
		"acceleratable": valueTrue,
		"manufacturer":  nodefeature.ManufacturerNVIDIA,
		"memory":        "24576Mi",
		"sliceable":     valueTrue,
	})

	devLabels := map[string]string{
		featureKey:                 valueTrue,
		core.LabelOSStable:         "linux",
		core.LabelArchStable:       "amd64",
		systemname.ManagedLabelKey: valueTrue,
	}
	dev := nodeDevices("node-a", devLabels, repeatCard(8, cardFree())...)

	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       name,
			Labels:     poolLabels,
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
	}
	cli := buildInstanceTypeClient(it, cq, &dev)
	reconcileInstanceTypeN(t, cli, name, 4)

	got := new(workercore.InstanceType)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, got))

	assert.True(t, got.Spec.Acceleratable, "descriptor refreshed from notes")
	assert.True(t, got.Spec.Sliceable, "sliceable refreshed")
	assert.Equal(t, "24576Mi", got.Spec.Memory, "memory refreshed")
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
		featureKey:           valueTrue,
		core.LabelOSStable:   "linux",
		core.LabelArchStable: "amd64",
	}

	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: name, Labels: poolLabels}}
	systemmeta.NoteResource(cq, _ClusterQueueResType, map[string]string{
		"acceleratable": valueTrue,
		"manufacturer":  nodefeature.ManufacturerNVIDIA,
		"memory":        "24576Mi",
		"sliceable":     valueTrue,
	})

	devLabels := map[string]string{
		featureKey:                 valueTrue,
		core.LabelOSStable:         "linux",
		core.LabelArchStable:       "amd64",
		systemname.ManagedLabelKey: valueTrue,
	}
	dev := nodeDevices("node-a", devLabels, repeatCard(8, cardFree())...)

	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       name,
			Labels:     poolLabels,
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
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

// TestInstanceTypeReconciler_TeardownFinalizer pins the delete handshake: deleting an
// InstanceType drives the backing queue to HoldAndDrain, then — the reconciler being
// the sole queue owner — drains it, deletes it itself, and releases the finalizer.
func TestInstanceTypeReconciler_TeardownFinalizer(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       name,
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
	}
	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: name}}
	systemmeta.NoteResource(cq, _ClusterQueueResType, map[string]string{"acceleratable": "false"})
	cli := buildInstanceTypeClient(it, cq)

	// Deleting an InstanceType with a finalizer stamps its deletion timestamp.
	require.NoError(t, cli.Delete(context.Background(), it))

	// First reconcile drives the queue to HoldAndDrain.
	reconcileInstanceTypeN(t, cli, name, 1)
	gotCQ, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.NotNil(t, gotCQ.Spec.StopPolicy)
	assert.Equal(t, kueue.HoldAndDrain, *gotCQ.Spec.StopPolicy, "delete sets the queue to HoldAndDrain")

	// Subsequent reconciles drain (nothing reserved), delete the queue, then release
	// the finalizer.
	reconcileInstanceTypeN(t, cli, name, 3)
	_, err = getClusterQueue(t, cli, name)
	assert.Truef(t, kerrors.IsNotFound(err), "drained queue must be deleted, got err=%v", err)
	err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, new(workercore.InstanceType))
	assert.True(t, kerrors.IsNotFound(err), "instance type released once the queue is gone")
}

// TestInstanceTypeReconciler_TeardownWaitsForDrain pins that a queue still holding an
// admitted workload is not deleted, and the finalizer keeps holding the InstanceType.
func TestInstanceTypeReconciler_TeardownWaitsForDrain(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:       name,
			Finalizers: []string{systemmeta.LockedResourceFinalizer},
		},
	}
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec:       kueue.ClusterQueueSpec{StopPolicy: ptr.To(kueue.HoldAndDrain)},
		Status:     kueue.ClusterQueueStatus{AdmittedWorkloads: 1},
	}
	systemmeta.NoteResource(cq, _ClusterQueueResType, map[string]string{"acceleratable": "false"})
	cli := buildInstanceTypeClient(it, cq)
	require.NoError(t, cli.Delete(context.Background(), it))

	reconcileInstanceTypeN(t, cli, name, 3)
	_, err := getClusterQueue(t, cli, name)
	require.NoError(t, err, "queue with a reservation must not be deleted")
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, new(workercore.InstanceType)),
		"instance type held until the queue drains")
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

// newNodesFlavor builds a "nodes" ResourceFlavor carrying the schedule labels the
// reconciler reads (the feature key, kubernetes.io/os|arch, and the key's
// .count/.capacity siblings) and the per-flavor notes. Its name is
// "gpustack-${key}-linux-amd64-${count}{c|d}", so it feeds the pool
// "gpustack-${key}-linux-amd64"; the device ("d") suffix is used when accelerated.
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
	keyLabel := featureKeyLabel(notes["acceleratable"] == "true", key)
	rf := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				keyLabel:             "true",
				core.LabelOSStable:   "linux",
				core.LabelArchStable: "amd64",
				keyLabel + _ResourceFlavorCountLabelSuffix:    itoa(count),
				keyLabel + _ResourceFlavorCapacityLabelSuffix: itoa(capacity),
			},
		},
	}
	systemmeta.NoteResource(rf, _ResourceFlavorResType, notes)
	return rf
}

// nodeQueueName is the pool (ClusterQueue / InstanceType) name a flavor with the given
// key feeds.
func nodeQueueName(key string) string {
	return fmt.Sprintf("gpustack-%s-linux-amd64", key)
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

// TestInstanceTypeReconciler_AlignsClusterQueue pins that a derived pool's flavors
// materialize an isolated backing ClusterQueue: a CPU-only pool covers only cpu, an
// accelerated pool covers only credits (= capacity × M), each quota lends nothing.
func TestInstanceTypeReconciler_AlignsClusterQueue(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)
	creditsName := nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA)

	cases := []struct {
		name string

		key      string
		flavors  []*kueue.ResourceFlavor
		capacity int64 // expected aggregate capacity feeding the queue

		wantAccelerated bool
	}{
		{
			name: "cpu-only queue covers only cpu, nominal = capacity cores",
			key:  "generic",
			flavors: []*kueue.ResourceFlavor{
				newNodesFlavor("gpustack-generic-linux-amd64-4c", "generic", 4, 12),
			},
			capacity: 12,
		},
		{
			name: "accelerated queue covers only credits, nominal = capacity × M",
			key:  "nvidia-a10g",
			flavors: []*kueue.ResourceFlavor{
				newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", "nvidia-a10g", 1, 3,
					accelerated(nodefeature.ManufacturerNVIDIA)),
			},
			capacity:        3,
			wantAccelerated: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			name := nodeQueueName(c.key)
			objs := make([]ctrlcli.Object, 0, len(c.flavors))
			for _, rf := range c.flavors {
				objs = append(objs, rf)
			}
			cli := buildInstanceTypeClient(objs...)

			// Reconcile authors the derived InstanceType, then creates + aligns its CQ.
			reconcileInstanceTypeN(t, cli, name, 4)

			cq, err := getClusterQueue(t, cli, name)
			require.NoError(t, err)

			// Isolation + active state.
			assert.Empty(t, cq.Spec.CohortName, "cohortName empty (isolated)")
			require.NotNil(t, cq.Spec.StopPolicy)
			assert.Equal(t, kueue.None, *cq.Spec.StopPolicy, "active (StopPolicy None)")

			// Exactly one resource group with one covered resource.
			require.Len(t, cq.Spec.ResourceGroups, 1, "one resource group")
			rg := cq.Spec.ResourceGroups[0]
			require.Len(t, rg.CoveredResources, 1, "one covered resource")
			require.Len(t, rg.Flavors, len(c.flavors), "one flavor quota per feeding flavor")

			rq := rg.Flavors[0].Resources[0]
			if c.wantAccelerated {
				assert.Equal(t, creditsName, rg.CoveredResources[0], "covers credits only")
				wantNominal := nodefeature.CardsToCredits(*resource.NewQuantity(c.capacity, resource.DecimalSI))
				assert.Equal(t, wantNominal.Value(), rq.NominalQuota.Value(), "credits nominal = capacity × M")
			} else {
				assert.Equal(t, core.ResourceCPU, rg.CoveredResources[0], "covers cpu only")
				assert.Equal(t, c.capacity, rq.NominalQuota.Value(), "cpu nominal = capacity cores")
			}
			// No borrowing/lending limit: the queue keeps an empty cohort, and Kueue
			// rejects a limit on a cohort-less queue. The empty cohort is the isolation.
			assert.Nil(t, rq.BorrowingLimit, "no borrowingLimit on a cohort-less queue")
			assert.Nil(t, rq.LendingLimit, "no lendingLimit on a cohort-less queue")

			// Notes carry the descriptive device fields under "instancetypes"; the unit
			// spec is not a queue note (it lives on the InstanceType).
			resType, notes := systemmeta.DescribeResource(cq)
			assert.Equal(t, _ClusterQueueResType, resType, "resType")
			for _, k := range []string{"manufacturer", "acceleratable"} {
				_, ok := notes[k]
				assert.Truef(t, ok, "note %q present", k)
			}
			for _, k := range []string{"unitCPU", "unitRAM", "localStorage"} {
				assert.NotContainsf(t, notes, k, "unit spec is not a queue note (%q)", k)
			}
		})
	}
}

// TestInstanceTypeReconciler_AggregatesCapacity pins that multiple flavors differing
// only in per-node count aggregate into one queue, credits summed across capacities.
func TestInstanceTypeReconciler_AggregatesCapacity(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)
	key := "nvidia-a10g"
	name := nodeQueueName(key)

	// Two device flavors of the same key: capacities 2 and 4 → 6 cards total.
	rf1 := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", key, 1, 2, accelerated(nodefeature.ManufacturerNVIDIA))
	rf2 := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-2d", key, 2, 4, accelerated(nodefeature.ManufacturerNVIDIA))
	cli := buildInstanceTypeClient(rf1, rf2)

	reconcileInstanceTypeN(t, cli, name, 4)

	cq, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.Len(t, cq.Spec.ResourceGroups, 1)
	rg := cq.Spec.ResourceGroups[0]
	require.Len(t, rg.Flavors, 2, "both flavors feed the queue")

	var total int64
	for _, fq := range rg.Flavors {
		total += fq.Resources[0].NominalQuota.Value()
	}
	want := nodefeature.CardsToCredits(*resource.NewQuantity(6, resource.DecimalSI))
	assert.Equal(t, want.Value(), total, "summed credits nominal = (2+4) cards × M")
}

// TestInstanceTypeReconciler_UnitSpecDerivation pins the non-accelerated unit spec on the
// InstanceType: a derived type gets the fixed 1c/2Gi/100Gi default regardless of the node
// hardware behind its flavors, and an admin's unit spec is preserved in full (it is
// admin-authored and immutable, no longer pinned to a single CPU core).
func TestInstanceTypeReconciler_UnitSpecDerivation(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)

	// Two CPU flavors of differing size feed the pool; their per-node notes must not leak
	// into the unit spec — it is a fixed default, not node-derived.
	mkFlavors := func() []ctrlcli.Object {
		return []ctrlcli.Object{
			newNodesFlavor("gpustack-generic-linux-amd64-4c", key, 4, 4),
			newNodesFlavor("gpustack-generic-linux-amd64-8c", key, 8, 8),
		}
	}

	t.Run("derived type gets the fixed 1c/2Gi/100Gi default", func(t *testing.T) {
		enableInstanceTypeDerivedFromNode(t)
		cli := buildInstanceTypeClient(mkFlavors()...)

		reconcileInstanceTypeN(t, cli, name, 5)

		it := getInstanceType(t, cli, name)
		assert.Equal(t, "1", it.Spec.UnitResources.CPU, "fixed default unitCPU")
		assert.Equal(t, "2Gi", it.Spec.UnitResources.RAM, "fixed default unitRAM")
		assert.Equal(t, "100Gi", it.Spec.LocalStorage, "fixed default localStorage")
	})

	t.Run("admin unit spec wins in full", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					featureKeyLabel(false, key): valueTrue,
					core.LabelOSStable:          "linux",
					core.LabelArchStable:        "amd64",
				},
			},
			Spec: workercore.InstanceTypeSpec{
				UnitResources: workercore.InstanceTypeUnitResources{CPU: "2", RAM: "16Gi"},
				LocalStorage:  "128Gi",
			},
		}
		cli := buildInstanceTypeClient(append(mkFlavors(), it)...)

		reconcileInstanceTypeN(t, cli, name, 4)

		got := getInstanceType(t, cli, name)
		assert.Equal(t, "2", got.Spec.UnitResources.CPU, "admin unitCPU wins (no longer pinned)")
		assert.Equal(t, "16Gi", got.Spec.UnitResources.RAM, "admin unitRAM wins")
		assert.Equal(t, "128Gi", got.Spec.LocalStorage, "admin localStorage wins")
	})
}

// TestInstanceTypeReconciler_DerivedInitializesUnitSpec pins that a derived
// InstanceType — authored without an admin unit spec — carries the fixed default
// unit spec chosen by acceleratable-ness (accelerated 4/16Gi/100Gi, non-accelerated
// 1/2Gi/100Gi), independent of the node hardware behind the flavor. The Instance
// webhook and the table read the unit from the spec, so the derived type must carry it.
func TestInstanceTypeReconciler_DerivedInitializesUnitSpec(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	cases := []struct {
		name   string
		key    string
		flavor *kueue.ResourceFlavor

		wantCPU     string
		wantRAM     string
		wantStorage string
	}{
		{
			name: "accelerated pool gets the fixed 4c/16Gi/100Gi default",
			key:  "nvidia-a10g",
			flavor: newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", "nvidia-a10g", 1, 4,
				accelerated(nodefeature.ManufacturerNVIDIA)),
			wantCPU:     "4",
			wantRAM:     "16Gi",
			wantStorage: "100Gi",
		},
		{
			name:        "cpu-only pool gets the fixed 1c/2Gi/100Gi default",
			key:         "generic",
			flavor:      newNodesFlavor("gpustack-generic-linux-amd64-4c", "generic", 4, 4),
			wantCPU:     "1", // a non-accelerated unit is always a single CPU core
			wantRAM:     "2Gi",
			wantStorage: "100Gi",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			name := nodeQueueName(c.key)
			cli := buildInstanceTypeClient(c.flavor)

			// Author the derived InstanceType (stamped with the fixed unit spec),
			// create + align its queue, then materialize status.
			reconcileInstanceTypeN(t, cli, name, 5)

			it := getInstanceType(t, cli, name)
			require.Equal(t, valueTrue, it.Labels[_InstanceTypeDerivedFromNodeLabel], "authored derived")
			assert.Equal(t, c.wantCPU, it.Spec.UnitResources.CPU, "fixed default unitCPU")
			assert.Equal(t, c.wantRAM, it.Spec.UnitResources.RAM, "fixed default unitRAM (Gi suffix)")
			assert.Equal(t, c.wantStorage, it.Spec.LocalStorage, "fixed default localStorage (Gi suffix)")
		})
	}
}

// TestInstanceTypeReconciler_RefreshesOSArch pins that the descriptor refresh writes
// the InstanceType's spec.OS / spec.Arch. A derived InstanceType is authored carrying
// os/arch only as schedule labels, yet the Instance webhook and the table read them
// from the spec, so the reconcile must materialize them there.
func TestInstanceTypeReconciler_RefreshesOSArch(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	cases := []struct {
		name   string
		key    string
		flavor *kueue.ResourceFlavor
	}{
		{
			name: "accelerated pool",
			key:  "nvidia-a10g",
			flavor: newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", "nvidia-a10g", 1, 4,
				accelerated(nodefeature.ManufacturerNVIDIA)),
		},
		{
			name:   "cpu-only pool",
			key:    "generic",
			flavor: newNodesFlavor("gpustack-generic-linux-amd64-4c", "generic", 4, 4),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			name := nodeQueueName(c.key)
			cli := buildInstanceTypeClient(c.flavor)

			// Author the derived InstanceType, create + align its queue, then refresh
			// its descriptor spec from the pool.
			reconcileInstanceTypeN(t, cli, name, 5)

			it := getInstanceType(t, cli, name)
			require.Equal(t, valueTrue, it.Labels[_InstanceTypeDerivedFromNodeLabel], "authored derived")
			assert.Equal(t, "linux", it.Spec.OS, "spec.OS refreshed from the pool")
			assert.Equal(t, "amd64", it.Spec.Arch, "spec.Arch refreshed from the pool")
		})
	}
}

func TestHasReserved(t *testing.T) {
	reserved := func(total, borrowed string) kueue.ClusterQueueStatus {
		return kueue.ClusterQueueStatus{
			FlavorsReservation: []kueue.FlavorUsage{{
				Name: kueue.ResourceFlavorReference("f"),
				Resources: []kueue.ResourceUsage{{
					Name:     core.ResourceCPU,
					Total:    resource.MustParse(total),
					Borrowed: resource.MustParse(borrowed),
				}},
			}},
		}
	}

	cases := []struct {
		name   string
		status kueue.ClusterQueueStatus
		want   bool
	}{
		{"empty", kueue.ClusterQueueStatus{}, false},
		{"reserving workloads", kueue.ClusterQueueStatus{ReservingWorkloads: 1}, true},
		{"admitted workloads", kueue.ClusterQueueStatus{AdmittedWorkloads: 1}, true},
		{"pending workloads do not count", kueue.ClusterQueueStatus{PendingWorkloads: 3}, false},
		{"reserved total", reserved("2", "0"), true},
		{"borrowed total", reserved("0", "1"), true},
		{"zero reservation, zero workloads", reserved("0", "0"), false},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cq := &kueue.ClusterQueue{Status: c.status}
			assert.Equal(t, c.want, hasReserved(cq))
		})
	}
}

func TestIndexResourceFlavorByNodeQueue(t *testing.T) {
	cases := []struct {
		name string
		rf   *kueue.ResourceFlavor
		want []string
	}{
		{
			name: "managed flavor feeds its node queue",
			rf:   newNodesFlavor("gpustack-generic-linux-amd64-4c", "generic", 4, 4),
			want: []string{nodeQueueName("generic")},
		},
		{
			name: "flavor differing only in count feeds the same queue",
			rf:   newNodesFlavor("gpustack-generic-linux-amd64-8c", "generic", 8, 8),
			want: []string{nodeQueueName("generic")},
		},
		{
			name: "flavor without schedule labels is not indexed",
			rf:   &kueue.ResourceFlavor{ObjectMeta: meta.ObjectMeta{Name: "bare"}},
			want: nil,
		},
		{
			name: "deleting flavor is not indexed",
			rf: func() *kueue.ResourceFlavor {
				rf := newNodesFlavor("gpustack-generic-linux-amd64-4c", "generic", 4, 4)
				now := meta.Now()
				rf.DeletionTimestamp = &now
				rf.Finalizers = []string{"gpustack.ai/test"}
				return rf
			}(),
			want: nil,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, indexResourceFlavorByNodeQueue(c.rf))
		})
	}
}
