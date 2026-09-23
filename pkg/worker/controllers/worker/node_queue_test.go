package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

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
	"gpustack.ai/gpustack/pkg/setting/settingtest"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// enableInstanceTypeDrainWhenNoFlavors seeds the delegated settings Secret so
// InstanceTypeDrainWhenNoFlavors resolves to true. ShouldValueBool returns false for any
// setting whose key is absent from the Secret (the read errors and the bool default is not
// applied), so the drain=true branch is only reachable once the key is present. The value
// caches for ~30s once read, so this is a one-way setup step — never flipped mid-test.
func enableInstanceTypeDrainWhenNoFlavors(t *testing.T) {
	t.Helper()
	settingtest.MergeDelegatedSettings(t, map[string]string{"instance-type-drain-when-no-flavors": "true"})
	require.True(t, settings.InstanceTypeDrainWhenNoFlavors.ShouldValueBool(context.Background()),
		"setting must read true after enabling")
}

// buildNodeQueueClient builds a fake client for the NodeQueueReconciler. Unlike the
// InstanceType client it carries no ResourceFlavor→node-queue field index (the reconciler
// lists flavors by MatchingLabels). It registers the ClusterQueue status subresource so
// topology readiness updates exercise the same API path as a real cluster; fixture status is
// retained for the reservation counters hasReserved reads.
func buildNodeQueueClient(objs ...ctrlcli.Object) ctrlcli.Client {
	profile := topologyProfile([]string{core.LabelHostname})
	topologyNames := make(map[string]struct{})
	var generated []ctrlcli.Object
	for _, obj := range objs {
		rf, ok := obj.(*kueue.ResourceFlavor)
		if !ok {
			continue
		}
		if rf.Labels == nil {
			rf.Labels = make(map[string]string)
		}
		rf.Labels[TopologyProfileLabel] = profile
		if rf.Spec.NodeLabels == nil {
			rf.Spec.NodeLabels = map[string]string{
				systemname.ManagedLabelKey: "true",
				core.LabelOSStable:         rf.Labels[core.LabelOSStable],
				core.LabelArchStable:       rf.Labels[core.LabelArchStable],
				TopologyProfileLabel:       profile,
			}
			for key, value := range rf.Labels {
				if key == nodefeature.NodeAcceleratableLabelKey ||
					(value == "true" && (strings.HasPrefix(key, nodefeature.GeneralFeatureLabelPrefix) ||
						strings.HasPrefix(key, nodefeature.AcceleratableFeatureLabelPrefix))) {
					rf.Spec.NodeLabels[key] = value
					if count := rf.Labels[key+_ResourceFlavorCountLabelSuffix]; count != "" {
						rf.Spec.NodeLabels[key+_ResourceFlavorCountLabelSuffix] = count
					}
				}
			}
		}
		if rf.Spec.TopologyName == nil {
			name := kueue.TopologyReference(topologyName(profile))
			rf.Spec.TopologyName = &name
		}
		topologyNames[string(*rf.Spec.TopologyName)] = struct{}{}
		if rf.DeletionTimestamp != nil {
			continue
		}

		count := parseResourceFlavorCount(rf)
		capacity := parseResourceFlavorCapacity(rf)
		for i := int64(0); count > 0 && i < capacity/count; i++ {
			labels := make(map[string]string, len(rf.Spec.NodeLabels)+1)
			for key, value := range rf.Spec.NodeLabels {
				labels[key] = value
			}
			labels[core.LabelHostname] = fmt.Sprintf("%s-%d", rf.Name, i)
			generated = append(generated, &core.Node{ObjectMeta: meta.ObjectMeta{
				Name:   labels[core.LabelHostname],
				Labels: labels,
			}})
		}
	}
	for name := range topologyNames {
		generated = append(generated, &kueue.Topology{
			ObjectMeta: meta.ObjectMeta{Name: name},
			Spec:       kueue.TopologySpec{Levels: []kueue.TopologyLevel{{NodeLabel: core.LabelHostname}}},
		})
	}
	objs = append(objs, generated...)
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&kueue.ClusterQueue{}).
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

func markClusterQueueStopped(t *testing.T, cli ctrlcli.Client, name string) {
	t.Helper()
	cq, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	cq.Status.Conditions = append(cq.Status.Conditions, meta.Condition{
		Type:               kueue.ClusterQueueActive,
		Status:             meta.ConditionFalse,
		Reason:             kueue.ClusterQueueActiveReasonStopped,
		ObservedGeneration: cq.Generation,
	})
	require.NoError(t, cli.Status().Update(context.Background(), cq))
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
	if len(groups) > 0 {
		cq.Annotations = map[string]string{_TASQueueAnnotation: "true"}
	}
	systemmeta.NoteResource(cq, _ClusterQueueResType, nil)
	return cq
}

// creditsValue is the credits nominal quota a pool of `cards` whole cards materializes.
func creditsValue(cards int64) int64 {
	q := nodefeature.AcceleratorsToCredits(*resource.NewQuantity(cards, resource.DecimalSI))
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

func TestValidateTASFlavors_CPUOnlyNodeMatchesFlavor(t *testing.T) {
	node := newManagedCPUNode("node-a", 4, 16, 32)
	name := cpuFlavorName(node)
	flavorClient := buildNodeFlavorClient(node)
	reconcileNodeFlavor(t, flavorClient, name)
	flavor, err := getResourceFlavor(t, flavorClient, name)
	require.NoError(t, err)
	profile := node.Labels[TopologyProfileLabel]
	topology := &kueue.Topology{
		ObjectMeta: meta.ObjectMeta{Name: topologyName(profile)},
		Spec:       kueue.TopologySpec{Levels: topologyLevelObjects([]string{core.LabelHostname})},
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(node, flavor, topology).Build()
	r := &NodeQueueReconciler{Client: cli}
	queue := &kueue.ClusterQueue{}
	flavors := &kueue.ResourceFlavorList{Items: []kueue.ResourceFlavor{*flavor}}
	failure, err := r.validateTASFlavors(context.Background(), queue, flavors)
	require.NoError(t, err)
	assert.Nil(t, failure)

	delete(node.Labels, nodefeature.NodeCPUOnlyLabelKey)
	require.NoError(t, cli.Update(context.Background(), node))
	failure, err = r.validateTASFlavors(context.Background(), queue, flavors)
	require.NoError(t, err)
	require.NotNil(t, failure)
	assert.Equal(t, "NonConservedQuota", failure.reason)
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

func TestNodeQueueReconciler_TASFlavorLimit(t *testing.T) {
	for _, flavorCount := range []int{0, 1, 16, 17, 64, 65} {
		t.Run(strconv.Itoa(flavorCount), func(t *testing.T) {
			const key = "generic"
			name := nodeQueueName(key)
			objs := []ctrlcli.Object{newInstanceTypeQueue(key, false)}
			for i := 1; i <= flavorCount; i++ {
				flavorName := fmt.Sprintf("gpustack-generic-linux-amd64-%dc-profile", i)
				objs = append(objs, newNodesFlavor(flavorName, key, int64(i), int64(i)))
			}
			cli := buildNodeQueueClient(objs...)

			_, err := (&NodeQueueReconciler{Client: cli}).Reconcile(context.Background(),
				ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
			require.NoError(t, err)
			got, err := getClusterQueue(t, cli, name)
			require.NoError(t, err)
			if flavorCount == 65 {
				assert.Empty(t, got.Spec.ResourceGroups, "an oversized queue is not partially filled")
				assert.True(t, nodeQueueConditionTopologyReady.IsFalse(got))
				assert.Equal(t, "TooManyFlavors", nodeQueueConditionTopologyReady.GetReason(got))
				return
			}
			if flavorCount == 0 {
				assert.Empty(t, got.Spec.ResourceGroups)
				return
			}
			require.Len(t, got.Spec.ResourceGroups, 1, "one covered resource has one group")
			assert.Len(t, got.Spec.ResourceGroups[0].Flavors, flavorCount)
			var actualQuota int64
			for _, flavor := range got.Spec.ResourceGroups[0].Flavors {
				actualQuota += flavor.Resources[0].NominalQuota.Value()
			}
			assert.Equal(t, int64(flavorCount*(flavorCount+1)/2), actualQuota,
				"quota equals the independently counted capacity of one Node per flavor")
			assert.True(t, nodeQueueConditionTopologyReady.IsTrue(got))
		})
	}
}

func TestNodeQueueReconciler_RejectsInvalidTASQueueInputs(t *testing.T) {
	const key = "generic"
	name := nodeQueueName(key)

	t.Run("overlapping selectors", func(t *testing.T) {
		first := newNodesFlavor("gpustack-generic-linux-amd64-4c-a", key, 4, 4)
		second := newNodesFlavor("gpustack-generic-linux-amd64-4c-b", key, 4, 4)
		cli := buildNodeQueueClient(newInstanceTypeQueue(key, false), first, second)
		reconcileNodeQueueN(t, cli, name, 1)
		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		assert.Empty(t, got.Spec.ResourceGroups)
		assert.Equal(t, "OverlappingSelectors", nodeQueueConditionTopologyReady.GetReason(got))
	})

	t.Run("missing topology", func(t *testing.T) {
		rf := newNodesFlavor("gpustack-generic-linux-amd64-4c", key, 4, 4)
		cli := buildNodeQueueClient(newInstanceTypeQueue(key, false), rf)
		require.NoError(t, cli.Delete(context.Background(), &kueue.Topology{
			ObjectMeta: meta.ObjectMeta{Name: string(*rf.Spec.TopologyName)},
		}))
		result := reconcileNodeQueueN(t, cli, name, 1)
		assert.Positive(t, result.RequeueAfter)
		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		assert.Empty(t, got.Spec.ResourceGroups)
		assert.Equal(t, "MissingTopology", nodeQueueConditionTopologyReady.GetReason(got))
	})

	t.Run("non-conserved quota", func(t *testing.T) {
		rf := newNodesFlavor("gpustack-generic-linux-amd64-4c", key, 4, 4)
		cli := buildNodeQueueClient(newInstanceTypeQueue(key, false), rf)
		stored := new(kueue.ResourceFlavor)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: rf.Name}, stored))
		for label := range stored.Labels {
			if strings.HasSuffix(label, _ResourceFlavorCapacityLabelSuffix) {
				stored.Labels[label] = "8"
			}
		}
		require.NoError(t, cli.Update(context.Background(), stored))
		reconcileNodeQueueN(t, cli, name, 1)
		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		assert.Empty(t, got.Spec.ResourceGroups)
		assert.Equal(t, "NonConservedQuota", nodeQueueConditionTopologyReady.GetReason(got))
	})
}

func TestNodeQueueReconciler_RetriesMissingTopologyDuringMigration(t *testing.T) {
	const key = "generic"
	name := nodeQueueName(key)
	flavor := newNodesFlavor("gpustack-generic-linux-amd64-4c-p-new", key, 4, 4)
	queue := newInstanceTypeQueue(key, false, cpuResourceGroup("gpustack-generic-linux-amd64-4c-p-old", 4))
	cli := buildNodeQueueClient(queue, flavor)
	reconcileNodeQueueN(t, cli, name, 1)

	topology := &kueue.Topology{ObjectMeta: meta.ObjectMeta{Name: string(*flavor.Spec.TopologyName)}}
	require.NoError(t, cli.Delete(context.Background(), topology))
	result := reconcileNodeQueueN(t, cli, name, 1)
	assert.Positive(t, result.RequeueAfter)
	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	assert.Equal(t, _TASQueueMigrationPhaseDraining, got.Annotations[_TASQueueMigrationPhaseAnnotation])
	assert.Equal(t, kueue.HoldAndDrain, ptr.Deref(got.Spec.StopPolicy, kueue.None))
	assert.Equal(t, "MissingTopology", nodeQueueConditionTopologyReady.GetReason(got))
}

func TestNodeQueueReconciler_DoesNotRewriteExistingQueue(t *testing.T) {
	const key = "generic"
	name := nodeQueueName(key)
	original := cpuResourceGroup("legacy-non-tas", 4)
	cq := newInstanceTypeQueue(key, false, original)
	delete(cq.Annotations, _TASQueueAnnotation)
	rf := newNodesFlavor("gpustack-generic-linux-amd64-4c", key, 4, 4)
	cli := buildNodeQueueClient(cq, rf)

	reconcileNodeQueueN(t, cli, name, 1)

	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.Len(t, got.Spec.ResourceGroups, 1)
	require.Len(t, got.Spec.ResourceGroups[0].Flavors, 1)
	assert.Equal(t, kueue.ResourceFlavorReference("legacy-non-tas"), got.Spec.ResourceGroups[0].Flavors[0].Name)
	assert.Equal(t, int64(4), got.Spec.ResourceGroups[0].Flavors[0].Resources[0].NominalQuota.Value())
	assert.Equal(t, "UnsupportedExistingObject", nodeQueueConditionTopologyReady.GetReason(got))
}

func TestNodeQueueReconciler_RejectsDuplicateCoveredResource(t *testing.T) {
	const key = "generic"
	name := nodeQueueName(key)
	cq := newInstanceTypeQueue(key, false,
		cpuResourceGroup("first", 2),
		cpuResourceGroup("second", 2))
	rf := newNodesFlavor("gpustack-generic-linux-amd64-4c", key, 4, 4)
	cli := buildNodeQueueClient(cq, rf)

	reconcileNodeQueueN(t, cli, name, 1)

	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	assert.Len(t, got.Spec.ResourceGroups, 2, "invalid existing quota is untouched")
	assert.Equal(t, "DuplicateCoveredResource", nodeQueueConditionTopologyReady.GetReason(got))
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
	want := nodefeature.AcceleratorsToCredits(*resource.NewQuantity(6, resource.DecimalSI))
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

// TestNodeQueueReconciler_KeepsAdminHoldStickyOnFlavorReturn pins that an admin-set Hold (the
// InstanceType-owned Inactive state) drained to empty is left sticky when the pool's flavors
// return: only a NodeQueue-owned HoldAndDrain is reactivated, so the queue must stay Hold here
// rather than flip to None (which would briefly admit workloads onto an Inactive type).
func TestNodeQueueReconciler_KeepsAdminHoldStickyOnFlavorReturn(t *testing.T) {
	key := "nvidia-a10g"
	name := nodeQueueName(key)

	cq := newInstanceTypeQueue(key, true)
	cq.Spec.StopPolicy = ptr.To(kueue.Hold)
	cq.Spec.ResourceGroups = nil

	rf := newNodesFlavor("gpustack-nvidia-a10g-linux-amd64-1d", key, 1, 4, accelerated(nodefeature.ManufacturerNVIDIA))
	cli := buildNodeQueueClient(cq, rf)

	reconcileNodeQueueN(t, cli, name, 2)

	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	require.NotNil(t, got.Spec.StopPolicy)
	assert.Equal(t, kueue.Hold, *got.Spec.StopPolicy, "admin Hold stays sticky across flavor return")
	require.Len(t, got.Spec.ResourceGroups, 1, "resource groups still refilled under Hold")
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
		assert.Equal(t, _TASQueueMigrationRequeueAfter, res.RequeueAfter, "requeues to re-check the drain")
	})

	t.Run("no reservations: groups emptied", func(t *testing.T) {
		key := "generic"
		name := nodeQueueName(key)
		cq := newInstanceTypeQueue(key, false, cpuResourceGroup("gpustack-generic-linux-amd64-4c", 4))

		cli := buildNodeQueueClient(cq) // no flavors, nothing reserved

		reconcileNodeQueueN(t, cli, name, 1)
		markClusterQueueStopped(t, cli, name)
		reconcileNodeQueueN(t, cli, name, 1)
		markClusterQueueStopped(t, cli, name)
		reconcileNodeQueueN(t, cli, name, 1)

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

		reconcileNodeQueueN(t, cli, name, 1)
		got, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		if ptr.Deref(got.Spec.StopPolicy, kueue.None) == kueue.HoldAndDrain {
			markClusterQueueStopped(t, cli, name)
			reconcileNodeQueueN(t, cli, name, 1)
			markClusterQueueStopped(t, cli, name)
			reconcileNodeQueueN(t, cli, name, 1)
		}

		got, err = getClusterQueue(t, cli, name)
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

func TestNodeQueueReconciler_MigratesFlavorPlanWithoutReplacingQueue(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	oldFlavor := "gpustack-generic-linux-amd64-4c-p-old"
	newFlavor := "gpustack-generic-linux-amd64-4c-p-new"

	tests := []struct {
		name         string
		stopPolicy   *kueue.StopPolicy
		wantRestored *kueue.StopPolicy
	}{
		{name: "restores None", stopPolicy: ptr.To(kueue.None), wantRestored: ptr.To(kueue.None)},
		{name: "restores admin Hold", stopPolicy: ptr.To(kueue.Hold), wantRestored: ptr.To(kueue.Hold)},
		{name: "restores unset", stopPolicy: nil, wantRestored: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cq := newInstanceTypeQueue(key, false, cpuResourceGroup(oldFlavor, 4))
			cq.UID = "stable-queue-uid"
			cq.Generation = 7
			cq.Spec.StopPolicy = tc.stopPolicy
			cq.Status.AdmittedWorkloads = 1
			newRF := newNodesFlavor(newFlavor, key, 4, 4)
			cli := buildNodeQueueClient(cq, newRF)

			res := reconcileNodeQueueN(t, cli, name, 1)
			assert.Positive(t, res.RequeueAfter)
			got, err := getClusterQueue(t, cli, name)
			require.NoError(t, err)
			assert.Equal(t, kueue.HoldAndDrain, ptr.Deref(got.Spec.StopPolicy, kueue.None))
			assert.Equal(t, oldFlavor, string(got.Spec.ResourceGroups[0].Flavors[0].Name),
				"the old plan remains while Kueue has not drained reservations")

			markClusterQueueStopped(t, cli, name)
			res = reconcileNodeQueueN(t, cli, name, 1)
			assert.Positive(t, res.RequeueAfter)
			got, err = getClusterQueue(t, cli, name)
			require.NoError(t, err)
			assert.Equal(t, oldFlavor, string(got.Spec.ResourceGroups[0].Flavors[0].Name),
				"an observed stop is insufficient while a reservation remains")

			got.Status.AdmittedWorkloads = 0
			require.NoError(t, cli.Status().Update(context.Background(), got))
			reconcileNodeQueueN(t, cli, name, 1)
			got, err = getClusterQueue(t, cli, name)
			require.NoError(t, err)
			assert.Equal(t, newFlavor, string(got.Spec.ResourceGroups[0].Flavors[0].Name))
			assert.Equal(t, kueue.HoldAndDrain, ptr.Deref(got.Spec.StopPolicy, kueue.None),
				"the replacement plan remains held until Kueue observes it")

			markClusterQueueStopped(t, cli, name)
			reconcileNodeQueueN(t, cli, name, 1)
			got, err = getClusterQueue(t, cli, name)
			require.NoError(t, err)
			assert.Equal(t, "stable-queue-uid", string(got.UID))
			assert.Equal(t, tc.wantRestored, got.Spec.StopPolicy)
		})
	}
}

func TestNodeQueueReconciler_WaitsForCurrentStoppedGeneration(t *testing.T) {
	key := "generic"
	name := nodeQueueName(key)
	oldFlavor := "gpustack-generic-linux-amd64-4c-p-old"
	newFlavor := "gpustack-generic-linux-amd64-4c-p-new"
	cq := newInstanceTypeQueue(key, false, cpuResourceGroup(oldFlavor, 4))
	cq.Generation = 7
	newRF := newNodesFlavor(newFlavor, key, 4, 4)
	cli := buildNodeQueueClient(cq, newRF)

	reconcileNodeQueueN(t, cli, name, 1)
	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	got.Status.Conditions = append(got.Status.Conditions, meta.Condition{
		Type:               kueue.ClusterQueueActive,
		Status:             meta.ConditionFalse,
		Reason:             kueue.ClusterQueueActiveReasonStopped,
		ObservedGeneration: got.Generation - 1,
	})
	require.NoError(t, cli.Status().Update(context.Background(), got))

	res := reconcileNodeQueueN(t, cli, name, 1)
	assert.Positive(t, res.RequeueAfter)
	got, err = getClusterQueue(t, cli, name)
	require.NoError(t, err)
	assert.Equal(t, oldFlavor, string(got.Spec.ResourceGroups[0].Flavors[0].Name),
		"stale stopped status must not authorize a flavor switch")
}

func TestNodeQueueReconciler_FlavorReturnsDuringNoFlavorDrain(t *testing.T) {
	enableInstanceTypeDrainWhenNoFlavors(t)
	key := "generic"
	name := nodeQueueName(key)
	oldFlavor := "gpustack-generic-linux-amd64-4c-p-old"
	newFlavor := newNodesFlavor("gpustack-generic-linux-amd64-4c-p-new", key, 4, 4)
	cq := newInstanceTypeQueue(key, false, cpuResourceGroup(oldFlavor, 4))
	cli := buildNodeQueueClient(cq, newFlavor)
	require.NoError(t, cli.Delete(context.Background(), newFlavor))

	reconcileNodeQueueN(t, cli, name, 1)
	got, err := getClusterQueue(t, cli, name)
	require.NoError(t, err)
	assert.Equal(t, _TASQueueMigrationPhaseDraining,
		got.Annotations[_TASQueueMigrationPhaseAnnotation])
	assert.Equal(t, kueue.None,
		kueue.StopPolicy(got.Annotations[_TASQueueMigrationStopPolicyAnnotation]))
	markClusterQueueStopped(t, cli, name)

	newFlavor.ResourceVersion = ""
	newFlavor.DeletionTimestamp = nil
	newFlavor.Finalizers = nil
	require.NoError(t, cli.Create(context.Background(), newFlavor))
	reconcileNodeQueueN(t, cli, name, 1)
	got, err = getClusterQueue(t, cli, name)
	require.NoError(t, err)
	assert.Equal(t, newFlavor.Name, string(got.Spec.ResourceGroups[0].Flavors[0].Name))
	assert.Equal(t, kueue.HoldAndDrain, ptr.Deref(got.Spec.StopPolicy, kueue.None))

	markClusterQueueStopped(t, cli, name)
	reconcileNodeQueueN(t, cli, name, 1)
	got, err = getClusterQueue(t, cli, name)
	require.NoError(t, err)
	assert.Equal(t, kueue.None, ptr.Deref(got.Spec.StopPolicy, kueue.None))
	assert.NotContains(t, got.Annotations, _TASQueueMigrationPhaseAnnotation)
}

// jointCheck builds the joint-admission AdmissionCheck, Active or not.
func jointCheck(active bool) *kueue.AdmissionCheck {
	ac := &kueue.AdmissionCheck{
		ObjectMeta: meta.ObjectMeta{Name: _JointAdmissionCheckName},
		Spec:       kueue.AdmissionCheckSpec{ControllerName: _JointAdmissionControllerName},
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

// TestNodeQueueReconciler_ReferencesTheJointCheckFromEveryQueue pins the difference that makes the
// joint barrier whole.
//
// THE CPU-ONLY QUEUE IS THE CASE, AND IT IS THE ONLY ONE THAT DISCRIMINATES. The node-devices
// reference is gated on `acceleratable`; borrowing that gate here would look correct on every
// accelerated queue and leave a multi-group deployment's CPU-pool group ungated -- a barrier with a
// hole in it, opening for exactly the deployment it was supposed to hold.
func TestNodeQueueReconciler_ReferencesTheJointCheckFromEveryQueue(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	cases := []struct {
		name          string
		acceleratable bool
	}{
		{name: "accelerated_queue", acceleratable: true},
		{name: "cpu_only_queue", acceleratable: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := "nvidia-a10g"
			if !c.acceleratable {
				key = "generic"
			}
			name := nodeQueueName(key)

			opts := []flavorOpt{}
			if c.acceleratable {
				opts = append(opts, accelerated(nodefeature.ManufacturerNVIDIA))
			}
			rf := newNodesFlavor("gpustack-"+key+"-linux-amd64-1d", key, 1, 4, opts...)

			cli := buildNodeQueueClient(
				newInstanceTypeQueue(key, c.acceleratable), rf, jointCheck(true))

			reconcileNodeQueueN(t, cli, name, 2)

			got, err := getClusterQueue(t, cli, name)
			require.NoError(t, err)
			require.NotNil(t, got.Spec.AdmissionChecksStrategy,
				"every operator-owned queue carries the joint check, accelerated or not")

			var found bool
			for _, rule := range got.Spec.AdmissionChecksStrategy.AdmissionChecks {
				if rule.Name == kueue.AdmissionCheckReference(_JointAdmissionCheckName) {
					found = true
				}
			}
			assert.True(t, found, "the joint check is referenced from a %s queue", c.name)
		})
	}
}

// TestNodeQueueReconciler_TheJointCheckWaitsForActive is the other half: Kueue turns a queue listing
// an inactive check inactive, so referencing one that is not Active would stop the queue admitting
// anything at all -- the opposite of what a barrier is for.
func TestNodeQueueReconciler_TheJointCheckWaitsForActive(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	key := "generic"
	rf := newNodesFlavor("gpustack-generic-linux-amd64-1d", key, 1, 4)
	cli := buildNodeQueueClient(newInstanceTypeQueue(key, false), rf, jointCheck(false))

	reconcileNodeQueueN(t, cli, nodeQueueName(key), 2)

	got, err := getClusterQueue(t, cli, nodeQueueName(key))
	require.NoError(t, err)
	assert.Nil(t, got.Spec.AdmissionChecksStrategy, "an inactive check is not referenced")
}
