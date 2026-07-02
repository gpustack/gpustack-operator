package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// flavorOpt mutates the notes a test ResourceFlavor carries, so a fixture can be
// flipped accelerated or given a specific unit spec.
type flavorOpt func(notes map[string]string)

func accelerated(manufacturer string) flavorOpt {
	return func(notes map[string]string) {
		notes["acceleratable"] = "true"
		notes["manufacturer"] = manufacturer
	}
}

func unitSpec(unitCPU, unitRAM, localStorage string) flavorOpt {
	return func(notes map[string]string) {
		notes["unitCPU"] = unitCPU
		notes["unitRAM"] = unitRAM
		notes["localStorage"] = localStorage
	}
}

// newNodesFlavor builds a "nodes" ResourceFlavor carrying the schedule labels the
// NodeQueueReconciler reads (the feature key, kubernetes.io/os|arch, and the key's
// .count/.capacity siblings) and the per-flavor notes. Its name is
// "gpustack-${key}-linux-amd64-${count}{c|d}", so it feeds the queue
// "gpustack-${key}-linux-amd64"; the device ("d") suffix is used when accelerated.
func newNodesFlavor(name, key string, count, capacity int64, opts ...flavorOpt) *kueue.ResourceFlavor {
	notes := map[string]string{
		"acceleratable": "false",
		"manufacturer":  "generic",
		"product":       "",
		"family":        "",
		"memory":        "",
		"unitCPU":       "1",
		"unitRAM":       "4",
		"localStorage":  "32",
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

// nodeQueueName is the ClusterQueue name a flavor with the given key feeds.
func nodeQueueName(key string) string {
	return fmt.Sprintf("gpustack-%s-linux-amd64", key)
}

func buildNodeQueueClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&kueue.ResourceFlavor{}, IndexingResourceFlavorByNodeQueue, indexResourceFlavorByNodeQueue).
		Build()
}

func reconcileNodeQueue(t *testing.T, cli ctrlcli.Client, name string) (ctrl.Result, error) {
	t.Helper()
	r := &NodeQueueReconciler{Client: cli}
	return r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
}

func getClusterQueue(t *testing.T, cli ctrlcli.Client, name string) (*kueue.ClusterQueue, error) {
	t.Helper()
	cq := new(kueue.ClusterQueue)
	err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, cq)
	return cq, err
}

// enableInstanceTypeDerivedFromNode writes the delegated settings Secret into the
// shared loopback client so InstanceTypeDerivedFromNode resolves to true. The
// setting value caches for 30s once read successfully, so this is one-way within a
// test binary: once any test enables it the value stays true, which is why the
// derived=false skip-auto-create branch is not asserted here (see the note on
// TestNodeQueueReconciler_DerivedTrueAutoCreates).
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

// TestNodeQueueReconciler_DerivedTrueAutoCreates pins that with
// instance-type-derived-from-node enabled, a feeding flavor with no existing queue
// triggers an auto-create.
//
// The complementary derived=false skip-auto-create branch is NOT asserted here: the
// setting value is cached process-globally for 30s in pkg/setting once read true,
// and that cache is not flushable from this package, so a false assertion cannot be
// kept deterministic in a shared test binary where another test enables the setting
// (it fails under `go test -shuffle`). The switch contract (default true, editable,
// env override to false) is pinned by pkg/worker/settings instead.
func TestNodeQueueReconciler_DerivedTrueAutoCreates(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	key := "switch"
	cqName := nodeQueueName(key)
	rf := newNodesFlavor("gpustack-"+key+"-linux-amd64-4c", key, 4, 4)
	cli := buildNodeQueueClient(rf)

	_, err := reconcileNodeQueue(t, cli, cqName)
	require.NoError(t, err)

	_, err = getClusterQueue(t, cli, cqName)
	assert.NoError(t, err, "queue must be auto-created when derived is true")
}

func TestNodeQueueReconciler_Aggregate(t *testing.T) {
	// All cases below need derived=true so the queue is materialized.
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
			cqName := nodeQueueName(c.key)
			objs := make([]ctrlcli.Object, 0, len(c.flavors))
			for _, rf := range c.flavors {
				objs = append(objs, rf)
			}
			cli := buildNodeQueueClient(objs...)

			_, err := reconcileNodeQueue(t, cli, cqName)
			require.NoError(t, err)

			cq, err := getClusterQueue(t, cli, cqName)
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

			fq := rg.Flavors[0]
			rq := fq.Resources[0]
			if c.wantAccelerated {
				assert.Equal(t, creditsName, rg.CoveredResources[0], "covers credits only")
				wantNominal := nodefeature.CardsToCredits(*resource.NewQuantity(c.capacity, resource.DecimalSI))
				assert.Equal(t, wantNominal.Value(), rq.NominalQuota.Value(), "credits nominal = capacity × M")
			} else {
				assert.Equal(t, core.ResourceCPU, rg.CoveredResources[0], "covers cpu only")
				assert.Equal(t, c.capacity, rq.NominalQuota.Value(), "cpu nominal = capacity cores")
			}
			// Auto-derived quota lends nothing.
			require.NotNil(t, rq.LendingLimit, "lendingLimit set")
			assert.Equal(t, int64(0), rq.LendingLimit.Value(), "lendingLimit 0")

			// Notes carry the descriptive fields + the unit spec under "instancetypes".
			resType, notes := systemmeta.DescribeResource(cq)
			assert.Equal(t, _ClusterQueueResType, resType, "resType")
			// unitCPU/unitRAM/localStorage and manufacturer/acceleratable are always
			// populated; memory/product/family are empty for these fixtures and empty
			// note values are not round-tripped (dropped on encode).
			for _, k := range []string{"unitCPU", "unitRAM", "localStorage", "manufacturer", "acceleratable"} {
				_, ok := notes[k]
				assert.Truef(t, ok, "note %q present", k)
			}
		})
	}
}

// TestNodeQueueReconciler_AggregateScalesCapacity pins that multiple flavors
// differing only in per-node count aggregate into one queue, with the credits
// nominal summed across their capacities.
func TestNodeQueueReconciler_AggregateScalesCapacity(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)
	key := "nvidia-a10g"
	cqName := nodeQueueName(key)

	// Two device flavors of the same key: capacities 2 and 4 → 6 cards total.
	rf1 := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", key, 1, 2, accelerated(nodefeature.ManufacturerNVIDIA))
	rf2 := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-2d", key, 2, 4, accelerated(nodefeature.ManufacturerNVIDIA))
	cli := buildNodeQueueClient(rf1, rf2)

	_, err := reconcileNodeQueue(t, cli, cqName)
	require.NoError(t, err)

	cq, err := getClusterQueue(t, cli, cqName)
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

// TestNodeQueueReconciler_UnitSpecMin pins the unit-spec aggregation: the queue
// notes take the min positive unitCPU/unitRAM/localStorage across feeding flavors,
// but an existing admin-set unit spec on the queue is preserved.
func TestNodeQueueReconciler_UnitSpecMin(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)
	key := "generic"
	cqName := nodeQueueName(key)

	mkFlavors := func() []ctrlcli.Object {
		return []ctrlcli.Object{
			newNodesFlavor("gpustack-generic-linux-amd64-4c", key, 4, 4, unitSpec("1", "8", "64")),
			newNodesFlavor("gpustack-generic-linux-amd64-8c", key, 8, 8, unitSpec("1", "4", "32")),
		}
	}

	t.Run("derives min positive across flavors", func(t *testing.T) {
		cli := buildNodeQueueClient(mkFlavors()...)

		_, err := reconcileNodeQueue(t, cli, cqName)
		require.NoError(t, err)

		cq, err := getClusterQueue(t, cli, cqName)
		require.NoError(t, err)
		_, notes := systemmeta.DescribeResource(cq)
		assert.Equal(t, "1", notes["unitCPU"], "min unitCPU")
		assert.Equal(t, "4", notes["unitRAM"], "min unitRAM")
		assert.Equal(t, "32", notes["localStorage"], "min localStorage")
	})

	t.Run("preserves an admin-set unit spec", func(t *testing.T) {
		// A pre-existing queue already carrying a unitCPU note (admin-set via the
		// InstanceType API): the reconciler must not recompute it.
		cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: cqName}}
		systemmeta.NoteResource(cq, _ClusterQueueResType, map[string]string{
			"unitCPU": "2", "unitRAM": "16", "localStorage": "128",
		})
		objs := append(mkFlavors(), cq)
		cli := buildNodeQueueClient(objs...)

		_, err := reconcileNodeQueue(t, cli, cqName)
		require.NoError(t, err)

		got, err := getClusterQueue(t, cli, cqName)
		require.NoError(t, err)
		_, notes := systemmeta.DescribeResource(got)
		assert.Equal(t, "2", notes["unitCPU"], "admin unitCPU preserved")
		assert.Equal(t, "16", notes["unitRAM"], "admin unitRAM preserved")
		assert.Equal(t, "128", notes["localStorage"], "admin localStorage preserved")
	})
}

func TestNodeQueueReconciler_Teardown(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)
	key := "generic"
	cqName := nodeQueueName(key)

	cases := []struct {
		name string

		feedFlavor bool              // a flavor still feeds the queue
		stopPolicy *kueue.StopPolicy // initial queue StopPolicy
		status     kueue.ClusterQueueStatus

		wantRequeueAfter time.Duration
		wantDeleted      bool
		wantStopPolicy   *kueue.StopPolicy
	}{
		{
			// No flavor feeds the queue: phase 1 sets HoldAndDrain and requeues.
			name:             "no flavor sets HoldAndDrain and requeues",
			stopPolicy:       ptr.To(kueue.None),
			wantRequeueAfter: 15 * time.Second,
			wantStopPolicy:   ptr.To(kueue.HoldAndDrain),
		},
		{
			// Already HoldAndDrain and drained (nothing reserved): delete.
			name:        "drained queue is deleted",
			stopPolicy:  ptr.To(kueue.HoldAndDrain),
			wantDeleted: true,
		},
		{
			// HoldAndDrain but still admitting workloads: wait, do not delete.
			name:             "draining queue with reservation waits",
			stopPolicy:       ptr.To(kueue.HoldAndDrain),
			status:           kueue.ClusterQueueStatus{AdmittedWorkloads: 1},
			wantRequeueAfter: 15 * time.Second,
		},
		{
			// An external Delete sets HoldAndDrain while a flavor still feeds it: the
			// drain→delete path runs regardless, and (drained) deletes.
			name:        "externally set HoldAndDrain drains even with a feeding flavor",
			feedFlavor:  true,
			stopPolicy:  ptr.To(kueue.HoldAndDrain),
			wantDeleted: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cq := &kueue.ClusterQueue{
				ObjectMeta: meta.ObjectMeta{Name: cqName},
				Spec:       kueue.ClusterQueueSpec{StopPolicy: c.stopPolicy},
				Status:     c.status,
			}
			systemmeta.NoteResource(cq, _ClusterQueueResType, map[string]string{"acceleratable": "false"})
			objs := []ctrlcli.Object{cq}
			if c.feedFlavor {
				objs = append(objs, newNodesFlavor("gpustack-generic-linux-amd64-4c", key, 4, 4))
			}
			cli := buildNodeQueueClient(objs...)

			res, err := reconcileNodeQueue(t, cli, cqName)
			require.NoError(t, err)
			if c.wantRequeueAfter != 0 {
				assert.Equal(t, c.wantRequeueAfter, res.RequeueAfter, "RequeueAfter")
			}

			got, err := getClusterQueue(t, cli, cqName)
			if c.wantDeleted {
				assert.Truef(t, kerrors.IsNotFound(err),
					"drained queue must be deleted, got err=%v", err)
				return
			}
			require.NoError(t, err, "queue must not be deleted")
			if c.wantStopPolicy != nil {
				require.NotNil(t, got.Spec.StopPolicy)
				assert.Equal(t, *c.wantStopPolicy, *got.Spec.StopPolicy, "StopPolicy")
			}
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
