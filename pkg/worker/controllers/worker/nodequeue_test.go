package worker

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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

// enableInstanceTypeDrainWhenNoFlavors seeds the delegated settings Secret so
// InstanceTypeDrainWhenNoFlavors resolves to true. ShouldValueBool returns false for any
// setting whose key is absent from the Secret (the read errors and the bool default is not
// applied), so the drain=true branch is only reachable once the key is present. The value
// caches for ~30s once read, so this is a one-way setup step — never flipped mid-test — and
// merges the key rather than replacing the Secret data other settings share.
func enableInstanceTypeDrainWhenNoFlavors(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	cli := system.LoopbackCtrlClient.Get()
	const key = "instance-type-drain-when-no-flavors"
	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      setting.DelegatedSecretName,
			Namespace: setting.DelegatedSecretNamespace,
		},
		Data: map[string][]byte{key: []byte("true")},
	}
	if err := cli.Create(ctx, sec); err != nil {
		got := new(core.Secret)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(sec), got))
		if got.Data == nil {
			got.Data = map[string][]byte{}
		}
		got.Data[key] = []byte("true")
		require.NoError(t, cli.Update(ctx, got))
	}
	require.True(t, settings.InstanceTypeDrainWhenNoFlavors.ShouldValueBool(ctx),
		"setting must read true after enabling")
}

// buildNodeQueueClient builds a fake client for the NodeQueueReconciler. Unlike the
// InstanceType client it carries no ResourceFlavor→node-queue field index (the reconciler
// lists flavors by MatchingLabels) and registers no status subresource, so a fixture
// ClusterQueue keeps any preset .status (the reservation counters hasReserved reads).
func buildNodeQueueClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
}

// reconcileNodeQueueN reconciles the ClusterQueue by name n times and returns the last
// result, so a test can assert both the converged state and the requeue signal.
func reconcileNodeQueueN(t *testing.T, cli ctrlcli.Client, name string, n int) ctrlreconcile.Result {
	t.Helper()
	r := &NodeQueueReconciler{Client: cli}
	var res ctrlreconcile.Result
	for range n {
		var err error
		res, err = r.Reconcile(context.Background(),
			ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
		require.NoError(t, err)
	}
	return res
}

// newInstanceTypeQueue builds an operator-owned backing ClusterQueue the way the
// InstanceTypeReconciler leaves it: the pool's schedule labels (feature key + os + arch),
// the "instancetypes" resType, StopPolicy None, and the given resource groups (none by
// default). The NodeQueueReconciler owns its quota from here.
func newInstanceTypeQueue(key string, acceleratable bool, groups ...kueue.ResourceGroup) *kueue.ClusterQueue {
	name := nodeQueueName(key)
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				featureKeyLabel(acceleratable, key):   "true",
				nodefeature.NodeAcceleratableLabelKey: strconv.FormatBool(acceleratable),
				core.LabelOSStable:                    "linux",
				core.LabelArchStable:                  "amd64",
			},
		},
		Spec: kueue.ClusterQueueSpec{
			NamespaceSelector: &meta.LabelSelector{},
			StopPolicy:        ptr.To(kueue.None),
			ResourceGroups:    groups,
		},
	}
	systemmeta.NoteResource(cq, _ClusterQueueResType, nil)
	return cq
}

// creditsValue is the credits nominal quota a pool of `cards` whole cards materializes.
func creditsValue(cards int64) int64 {
	q := nodefeature.CardsToCredits(*resource.NewQuantity(cards, resource.DecimalSI))
	return q.Value()
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

// cpuResourceGroup is a one-flavor CPU resource group standing in for a queue that still
// carries quota (its exact contents do not matter to the drain/empty path).
func cpuResourceGroup(flavorName string, cpu int64) kueue.ResourceGroup {
	return kueue.ResourceGroup{
		CoveredResources: []core.ResourceName{core.ResourceCPU},
		Flavors: []kueue.FlavorQuotas{{
			Name: kueue.ResourceFlavorReference(flavorName),
			Resources: []kueue.ResourceQuota{{
				Name:         core.ResourceCPU,
				NominalQuota: *resource.NewQuantity(cpu, resource.DecimalSI),
			}},
		}},
	}
}

// TestNodeQueueReconciler_FillsAndSortsByCount pins that the reconciler fills the resource
// groups from the pool's flavors — a CPU-only queue covers only cpu (nominal = capacity
// cores), an accelerated queue covers only the manufacturer's credits (nominal = capacity ×
// M) — and orders the flavors smallest per-node count first, with no borrow/lend limit on
// the cohort-less queue.
func TestNodeQueueReconciler_FillsAndSortsByCount(t *testing.T) {
	creditsName := nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA)

	cases := []struct {
		name string

		key           string
		acceleratable bool
		// flavors are listed largest per-node count first, so the ascending sort is observable.
		flavors []*kueue.ResourceFlavor

		wantCovered      core.ResourceName
		wantFirstName    string
		wantFirstNominal int64
	}{
		{
			name: "cpu-only queue covers cpu, smallest per-node count first",
			key:  "generic",
			flavors: []*kueue.ResourceFlavor{
				newNodesFlavor("gpustack-generic-linux-amd64-8c", "generic", 8, 8),
				newNodesFlavor("gpustack-generic-linux-amd64-4c", "generic", 4, 4),
			},
			wantCovered:      core.ResourceCPU,
			wantFirstName:    "gpustack-generic-linux-amd64-4c",
			wantFirstNominal: 4,
		},
		{
			name:          "accelerated queue covers credits, smallest per-node count first",
			key:           "nvidia-a10g",
			acceleratable: true,
			flavors: []*kueue.ResourceFlavor{
				newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-2d", "nvidia-a10g", 2, 4,
					accelerated(nodefeature.ManufacturerNVIDIA)),
				newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", "nvidia-a10g", 1, 3,
					accelerated(nodefeature.ManufacturerNVIDIA)),
			},
			wantCovered:      creditsName,
			wantFirstName:    "gpustack-nvidia-a10g-linux-amd64-1d",
			wantFirstNominal: creditsValue(3),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			objs := []ctrlcli.Object{
				newInstanceTypeQueue(c.key, c.acceleratable),
			}
			for _, rf := range c.flavors {
				objs = append(objs, rf)
			}
			cli := buildNodeQueueClient(objs...)
			name := nodeQueueName(c.key)

			reconcileNodeQueueN(t, cli, name, 2)

			cq, err := getClusterQueue(t, cli, name)
			require.NoError(t, err)
			require.Len(t, cq.Spec.ResourceGroups, 1, "one resource group")
			rg := cq.Spec.ResourceGroups[0]
			require.Len(t, rg.CoveredResources, 1, "one covered resource")
			assert.Equal(t, c.wantCovered, rg.CoveredResources[0], "covered resource")
			require.Len(t, rg.Flavors, len(c.flavors), "one flavor quota per feeding flavor")

			assert.Equal(t, c.wantFirstName, string(rg.Flavors[0].Name),
				"smallest per-node count feeds first")
			rq := rg.Flavors[0].Resources[0]
			assert.Equal(t, c.wantFirstNominal, rq.NominalQuota.Value(), "first flavor nominal quota")
			assert.Nil(t, rq.BorrowingLimit, "no borrowingLimit on a cohort-less queue")
			assert.Nil(t, rq.LendingLimit, "no lendingLimit on a cohort-less queue")
		})
	}
}

// TestNodeQueueReconciler_AcceleratedFillsDespiteGeneralKey pins that an accelerated pool's queue
// fills its credits quota even though each accelerated ResourceFlavor also carries the
// general.<gKey> selector label WITHOUT a .capacity sibling. parseResourceFlavorCapacity must read
// the acceleratable key's capacity, not the general key's missing one — the map's random iteration
// order otherwise dropped the quota to 0 on roughly half of the reconciles.
func TestNodeQueueReconciler_AcceleratedFillsDespiteGeneralKey(t *testing.T) {
	const aKey = "nvidia-a10g"
	name := nodeQueueName(aKey)
	rf := newNodesFlavor("gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d", aKey, 1, 1,
		accelerated(nodefeature.ManufacturerNVIDIA), withGeneralKey("amd-epyc-7r32"))

	// The bug was nondeterministic (map iteration order), so exercise several independent reconciles
	// on a fresh client each round; every one must land the same non-zero credits quota.
	for range 10 {
		cli := buildNodeQueueClient(newInstanceTypeQueue(aKey, true), rf.DeepCopy())
		reconcileNodeQueueN(t, cli, name, 1)
		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		require.Len(t, got.Spec.ResourceGroups, 1,
			"accelerated queue must fill despite the general.<gKey> label")
		rq := got.Spec.ResourceGroups[0].Flavors[0].Resources[0]
		assert.Equal(t, creditsValue(1), rq.NominalQuota.Value(),
			"credits from the acceleratable key's capacity, not the missing general one")
	}
}

// collapsedGenericQueue builds an operator-owned generic ClusterQueue the way the
// InstanceTypeReconciler leaves it for a non-accelerated pool: the acceleratable=false
// discriminator plus os/arch, and — only when aware — the general.<gKey> key. StopPolicy None.
func collapsedGenericQueue(name, generalGroup string) *kueue.ClusterQueue {
	labels := map[string]string{
		nodefeature.NodeAcceleratableLabelKey: "false",
		core.LabelOSStable:                    "linux",
		core.LabelArchStable:                  "amd64",
	}
	if generalGroup != "" {
		labels[nodefeature.GeneralFeatureLabelPrefix+generalGroup] = "true"
	}
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: name, Labels: labels},
		Spec: kueue.ClusterQueueSpec{
			NamespaceSelector: &meta.LabelSelector{},
			StopPolicy:        ptr.To(kueue.None),
		},
	}
	systemmeta.NoteResource(cq, _ClusterQueueResType, nil)
	return cq
}

// TestNodeQueueReconciler_GenericCollapsedFillsFromAllCPUFlavors pins that a collapsed generic
// queue (carrying only the acceleratable=false discriminator, no general.* key — the unaware
// shape) fills from every CPU ResourceFlavor of its os/arch regardless of the CPU key, so all
// CPUs pool together.
func TestNodeQueueReconciler_GenericCollapsedFillsFromAllCPUFlavors(t *testing.T) {
	name := "gpustack--generic-linux-amd64"
	cq := collapsedGenericQueue(name, "") // no general key: fully collapsed
	// Two CPU flavors of different CPU manufacturers.
	rf1 := newNodesFlavor("gpustack--amd-epyc-7763-linux-amd64-8c", "amd-epyc-7763", 8, 8)
	rf2 := newNodesFlavor("gpustack--intel-xeon-8358-linux-amd64-4c", "intel-xeon-8358", 4, 4)
	cli := buildNodeQueueClient(cq, rf1, rf2)

	reconcileNodeQueueN(t, cli, name, 2)

	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.Len(t, got.Spec.ResourceGroups, 1, "one resource group")
	rg := got.Spec.ResourceGroups[0]
	assert.Equal(t, core.ResourceCPU, rg.CoveredResources[0], "covers cpu")
	assert.Len(t, rg.Flavors, 2, "both CPU flavors pool into the collapsed generic queue")
}

// TestNodeQueueReconciler_AwareGenericExcludesAcceleratedFlavor pins the selector isolation: an
// aware generic queue (general.<gKey>=true + acceleratable=false) never fills from an accelerated
// flavor that happens to carry the same general.<gKey> — the acceleratable=false discriminator
// excludes it. Without that boolean guard the general.<gKey> key alone would wrongly match the
// accelerated flavor and pollute the queue's quota.
func TestNodeQueueReconciler_AwareGenericExcludesAcceleratedFlavor(t *testing.T) {
	const gKey = "amd-epyc-7763"
	name := "gpustack--" + gKey + "-linux-amd64"
	cq := collapsedGenericQueue(name, gKey) // aware generic: carries general.<gKey>

	cpuRF := newNodesFlavor("gpustack--"+gKey+"-linux-amd64-8c", gKey, 8, 8)
	// A same-CPU accelerated flavor: it carries the paired general.<gKey> presence (Task 1) plus
	// acceleratable=true — exactly the case the boolean guard must exclude.
	accelRF := newNodesFlavor("gpustack--"+gKey+"--nvidia-a10g-linux-amd64-1d", "nvidia-a10g", 1, 4,
		accelerated(nodefeature.ManufacturerNVIDIA))
	accelRF.Labels[nodefeature.GeneralFeatureLabelPrefix+gKey] = "true"
	cli := buildNodeQueueClient(cq, cpuRF, accelRF)

	reconcileNodeQueueN(t, cli, name, 2)

	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.Len(t, got.Spec.ResourceGroups, 1, "one resource group")
	rg := got.Spec.ResourceGroups[0]
	assert.Equal(t, core.ResourceCPU, rg.CoveredResources[0], "covers cpu, not credits")
	require.Len(t, rg.Flavors, 1, "only the CPU flavor feeds; the accelerated flavor is excluded")
	assert.Equal(t, cpuRF.Name, string(rg.Flavors[0].Name), "the CPU flavor, not the accelerated one")
}

// TestNodeQueueReconciler_AggregatesCapacity pins that multiple flavors of one pool differing
// only in per-node count aggregate into a single queue, their credits summed across capacities.
func TestNodeQueueReconciler_AggregatesCapacity(t *testing.T) {
	key := "nvidia-a10g"
	name := nodeQueueName(key)

	// Two device flavors of the same key: capacities 2 and 4 → 6 cards total.
	rf1 := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", key, 1, 2, accelerated(nodefeature.ManufacturerNVIDIA))
	rf2 := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-2d", key, 2, 4, accelerated(nodefeature.ManufacturerNVIDIA))
	cli := buildNodeQueueClient(newInstanceTypeQueue(key, true), rf1, rf2)

	reconcileNodeQueueN(t, cli, name, 2)

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

// TestNodeQueueReconciler_ReactivatesOnFlavorReturn pins that a queue previously drained to
// empty (StopPolicy HoldAndDrain, no resource groups) is reactivated — StopPolicy back to
// None and the groups refilled — once its pool's flavors return.
func TestNodeQueueReconciler_ReactivatesOnFlavorReturn(t *testing.T) {
	key := "nvidia-a10g"
	name := nodeQueueName(key)

	cq := newInstanceTypeQueue(key, true)
	cq.Spec.StopPolicy = ptr.To(kueue.HoldAndDrain)
	cq.Spec.ResourceGroups = nil

	rf := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", key, 1, 4, accelerated(nodefeature.ManufacturerNVIDIA))
	cli := buildNodeQueueClient(cq, rf)

	reconcileNodeQueueN(t, cli, name, 2)

	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.NotNil(t, got.Spec.StopPolicy)
	assert.Equal(t, kueue.None, *got.Spec.StopPolicy, "reactivated once the flavors returned")
	require.Len(t, got.Spec.ResourceGroups, 1, "resource groups refilled")
}

// TestNodeQueueReconciler_DrainThenEmptyRespectsReservations pins the no-flavors path: while
// the queue still holds a reservation the quota is kept and the queue is driven to
// HoldAndDrain (requeued), and only once nothing is reserved are the resource groups emptied.
// instance-type-drain-when-no-flavors is seeded true once at setup (its key is otherwise
// absent, which ShouldValueBool reads as false); the value then caches, so only the drain=true
// path is exercised — it is never flipped mid-run.
func TestNodeQueueReconciler_DrainThenEmptyRespectsReservations(t *testing.T) {
	enableInstanceTypeDrainWhenNoFlavors(t)

	t.Run("reservations present: drained, groups kept", func(t *testing.T) {
		key := "generic"
		name := nodeQueueName(key)
		cq := newInstanceTypeQueue(key, false, cpuResourceGroup("gpustack-generic-linux-amd64-4c", 4))
		cq.Status.AdmittedWorkloads = 1 // hasReserved → true

		cli := buildNodeQueueClient(cq) // no flavors

		res := reconcileNodeQueueN(t, cli, name, 1)

		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		require.NotNil(t, got.Spec.StopPolicy)
		assert.Equal(t, kueue.HoldAndDrain, *got.Spec.StopPolicy, "held and draining while reserved")
		assert.NotEmpty(t, got.Spec.ResourceGroups, "groups not emptied while reserved")
		assert.Equal(t, 60*time.Second, res.RequeueAfter, "requeues to re-check the drain")
	})

	t.Run("no reservations: groups emptied", func(t *testing.T) {
		key := "generic"
		name := nodeQueueName(key)
		cq := newInstanceTypeQueue(key, false, cpuResourceGroup("gpustack-generic-linux-amd64-4c", 4))

		cli := buildNodeQueueClient(cq) // no flavors, nothing reserved

		reconcileNodeQueueN(t, cli, name, 2)

		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		assert.Empty(t, got.Spec.ResourceGroups, "groups emptied once nothing is reserved")
	})
}

// TestNodeQueueReconciler_DoesNotReactivateHeldQueueWithQuota pins that a stopped queue that
// still carries quota is never auto-reactivated — reactivation only fires on a queue drained to
// *empty*. This is the sole guard that keeps the InstanceType-agnostic reconciler from fighting
// a teardown: the teardown holds the queue (HoldAndDrain) while its resource groups are still
// filled, and the reconciler must leave that StopPolicy alone even with the pool's flavors present.
func TestNodeQueueReconciler_DoesNotReactivateHeldQueueWithQuota(t *testing.T) {
	key := "nvidia-a10g"
	name := nodeQueueName(key)

	// Held, but still carrying quota (the shape a teardown leaves while draining).
	cq := newInstanceTypeQueue(key, true,
		cpuResourceGroup("gpustack-nvidia-a10g-linux-amd64-1d", 4))
	cq.Spec.StopPolicy = ptr.To(kueue.HoldAndDrain)

	// Flavors present: only the still-filled groups keep the reconciler from reactivating.
	rf := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", key, 1, 4, accelerated(nodefeature.ManufacturerNVIDIA))
	cli := buildNodeQueueClient(cq, rf)

	reconcileNodeQueueN(t, cli, name, 2)

	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.NotNil(t, got.Spec.StopPolicy)
	assert.Equal(t, kueue.HoldAndDrain, *got.Spec.StopPolicy,
		"a held queue that still carries quota is not reactivated")
}

// TestNodeQueueReconciler_DrainsOnDelete pins that a queue marked for deletion is driven to
// HoldAndDrain (so Kueue evicts its admitted workloads and can then drop its own finalizer and
// remove the queue) — unconditionally, without consulting instance-type-drain-when-no-flavors —
// and that an already-draining deleting queue is a no-op.
func TestNodeQueueReconciler_DrainsOnDelete(t *testing.T) {
	key := "nvidia-a10g"
	name := nodeQueueName(key)

	newDeleting := func(sp kueue.StopPolicy) *kueue.ClusterQueue {
		cq := newInstanceTypeQueue(key, true,
			cpuResourceGroup("gpustack-nvidia-a10g-linux-amd64-1d", 4))
		cq.Spec.StopPolicy = ptr.To(sp)
		now := meta.Now()
		cq.DeletionTimestamp = &now
		// A fake client keeps a deleting object only while it carries a finalizer; in the
		// cluster Kueue's own ResourceInUse finalizer plays that role until the queue is empty.
		cq.Finalizers = []string{"kueue.x-k8s.io/resource-in-use"}
		return cq
	}

	t.Run("deleting queue is driven to HoldAndDrain", func(t *testing.T) {
		cq := newDeleting(kueue.None)
		cq.Status.AdmittedWorkloads = 1 // still holds an admitted workload
		cli := buildNodeQueueClient(cq)

		reconcileNodeQueueN(t, cli, name, 1)

		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		require.NotNil(t, got.Spec.StopPolicy)
		assert.Equal(t, kueue.HoldAndDrain, *got.Spec.StopPolicy, "a deleting queue is drained")
	})

	t.Run("already draining deleting queue is a no-op", func(t *testing.T) {
		cq := newDeleting(kueue.HoldAndDrain)
		cli := buildNodeQueueClient(cq)

		res := reconcileNodeQueueN(t, cli, name, 1)
		assert.Zero(t, res.RequeueAfter, "no requeue once draining")

		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		require.NotNil(t, got.Spec.StopPolicy)
		assert.Equal(t, kueue.HoldAndDrain, *got.Spec.StopPolicy)
	})
}

// TestNodeQueueReconciler_ReferencesAdmissionCheckWhenActive pins that an accelerated derived
// queue references the node-devices AdmissionCheck only once the check reports Active — Kueue
// turns a queue that lists an inactive check inactive, so the reference must wait for Active.
func TestNodeQueueReconciler_ReferencesAdmissionCheckWhenActive(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	key := "nvidia-a10g"
	name := nodeQueueName(key)

	newCheck := func(active bool) *kueue.AdmissionCheck {
		ac := &kueue.AdmissionCheck{
			ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName},
			Spec:       kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName},
		}
		if active {
			ac.Status.Conditions = []meta.Condition{{
				Type:   kueue.AdmissionCheckActive,
				Status: meta.ConditionTrue,
				Reason: "Ready",
			}}
		}
		return ac
	}

	cases := []struct {
		name    string
		active  bool
		wantRef bool
	}{
		{name: "check active: referenced", active: true, wantRef: true},
		{name: "check inactive: not referenced", active: false, wantRef: false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rf := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", key, 1, 4, accelerated(nodefeature.ManufacturerNVIDIA))
			cli := buildNodeQueueClient(
				newInstanceTypeQueue(key, true),
				rf, newCheck(c.active),
			)

			reconcileNodeQueueN(t, cli, name, 2)

			got, err := getClusterQueue(t, cli, name)
			require.NoError(t, err)
			if !c.wantRef {
				assert.Nil(t, got.Spec.AdmissionChecksStrategy, "an inactive check is not referenced")
				return
			}
			require.NotNil(t, got.Spec.AdmissionChecksStrategy, "an active check is referenced")
			require.Len(t, got.Spec.AdmissionChecksStrategy.AdmissionChecks, 1)
			assert.Equal(t, kueue.AdmissionCheckReference(_NodeDevicesAdmissionCheckName),
				got.Spec.AdmissionChecksStrategy.AdmissionChecks[0].Name, "references the node-devices check")
		})
	}
}

// TestNodeQueueReconciler_IgnoresTerminatingFlavor pins that a ResourceFlavor Kueue is still
// finalizing (DeletionTimestamp set) is treated as absent when the reconciler decides whether to
// fill or drain. In the cluster the NodeFlavor reconciler deletes a flavor whose nodes left the
// pool, but Kueue holds the flavor's resource-in-use finalizer until no ClusterQueue references
// it — so keeping it in the resource groups re-holds that finalizer and deadlocks its removal. A
// fake client cannot surface that deadlock (its Delete removes the object immediately, with no
// Kueue finalizer), so this guards the real-cluster behavior directly.
func TestNodeQueueReconciler_IgnoresTerminatingFlavor(t *testing.T) {
	terminating := func(rf *kueue.ResourceFlavor) *kueue.ResourceFlavor {
		now := meta.Now()
		rf.DeletionTimestamp = &now
		rf.Finalizers = []string{"kueue.x-k8s.io/resource-in-use"} // the finalizer Kueue holds while referenced
		return rf
	}

	t.Run("all flavors terminating: queue empties, breaking the deadlock", func(t *testing.T) {
		key := "generic"
		name := nodeQueueName(key)
		// The queue still lists the flavor in its groups — the deadlock shape — with nothing reserved.
		cq := newInstanceTypeQueue(key, false, cpuResourceGroup("gpustack-generic-linux-amd64-4c", 4))
		rf := terminating(newNodesFlavor("gpustack-generic-linux-amd64-4c", key, 4, 4))
		cli := buildNodeQueueClient(cq, rf)

		reconcileNodeQueueN(t, cli, name, 2)

		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		assert.Empty(t, got.Spec.ResourceGroups,
			"a terminating flavor is treated as absent, so the queue drops the reference and empties")
	})

	t.Run("partial pool: terminating flavor dropped, live flavor kept", func(t *testing.T) {
		key := "generic"
		name := nodeQueueName(key)
		cq := newInstanceTypeQueue(key, false)
		live := newNodesFlavor("gpustack-generic-linux-amd64-8c", key, 8, 8)
		dead := terminating(newNodesFlavor("gpustack-generic-linux-amd64-4c", key, 4, 4))
		cli := buildNodeQueueClient(cq, live, dead)

		reconcileNodeQueueN(t, cli, name, 2)

		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		require.Len(t, got.Spec.ResourceGroups, 1)
		names := make([]string, 0, len(got.Spec.ResourceGroups[0].Flavors))
		for _, fq := range got.Spec.ResourceGroups[0].Flavors {
			names = append(names, string(fq.Name))
		}
		assert.Equal(t, []string{"gpustack-generic-linux-amd64-8c"}, names,
			"only the live flavor feeds the queue; the terminating one is dropped")
	})
}
