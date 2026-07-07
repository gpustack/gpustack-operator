package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
)

// newManagedCPUNode builds a managed Node carrying the general(CPU) feature labels
// and the status capacity the reconciler reads — enough for ExtractNodeFlavors to
// emit exactly one CPU flavor named "gpustack-generic-linux-amd64-${cpu}c".
func newManagedCPUNode(name string, cpu, memGi, stgGi int64) *core.Node {
	nd := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				systemname.ManagedLabelKey: "true",
				core.LabelOSStable:         "linux",
				core.LabelArchStable:       "amd64",
			},
		},
		Status: core.NodeStatus{
			Capacity: core.ResourceList{
				core.ResourceCPU:              *resource.NewQuantity(cpu, resource.DecimalSI),
				core.ResourceMemory:           *resource.NewQuantity(memGi*(1<<30), resource.BinarySI),
				core.ResourceEphemeralStorage: *resource.NewQuantity(stgGi*(1<<30), resource.BinarySI),
			},
		},
	}
	// Record the general(CPU) key presence so the node also matches by the feature
	// key label, mirroring what the NodeFeature reconciler writes onto the node.
	gKey := nodefeature.ExtractGeneralNodeKey(nd)
	nd.Labels[nodefeature.GeneralFeatureLabelPrefix+gKey] = "true"
	// The general .count label ConstructNodeCapacityLabels stamps; ExtractNodeFlavors
	// reads the CPU flavor size from it, so the fixture must carry it.
	nd.Labels[nodefeature.GeneralFeatureLabelPrefix+gKey+".count"] = itoa(cpu)
	return nd
}

// newManagedAccelNode builds a managed Node carrying one nvidia-a10g accelerator
// (count cards, per-card VRAM) plus its CPU capacity. ExtractNodeFlavors emits two
// flavors: the device flavor "gpustack-nvidia-a10g-linux-amd64-${count}d" and a CPU
// flavor; the umbrella acceleratable label marks the node accelerated.
func newManagedAccelNode(name string, count int64) *core.Node {
	nd := newManagedCPUNode(name, 48, 192, 100)
	aKey := "nvidia-a10g"
	p := nodefeature.AcceleratableFeatureLabelPrefix + aKey
	nd.Labels[nodefeature.NodeAcceleratableLabelKey] = "true"
	nd.Labels[p] = "true"
	nd.Labels[p+".count"] = itoa(count)
	nd.Labels[p+".product"] = "NVIDIA A10G"
	nd.Labels[p+".memory"] = "24Gi"
	nd.Labels[p+".family"] = "ampere"
	nd.Labels[p+".cores"] = "9216"
	return nd
}

func itoa(v int64) string {
	return resource.NewQuantity(v, resource.DecimalSI).String()
}

func buildNodeFlavorClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&core.Node{}, IndexingNodeByScheduleFlavor, indexNodeByScheduleFlavor).
		Build()
}

func reconcileNodeFlavor(t *testing.T, cli ctrlcli.Client, name string) {
	t.Helper()
	r := &NodeFlavorReconciler{Client: cli}
	_, err := r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	require.NoError(t, err)
}

// cpuFlavorName derives the CPU ResourceFlavor name a node contributes to.
func cpuFlavorName(nd *core.Node) string {
	for _, f := range nodefeature.ExtractNodeFlavors(nd) {
		if !f.Acceleratable {
			return f.Name
		}
	}
	return ""
}

// deviceFlavorName derives the device ResourceFlavor name a node contributes to.
func deviceFlavorName(nd *core.Node) string {
	for _, f := range nodefeature.ExtractNodeFlavors(nd) {
		if f.Acceleratable {
			return f.Name
		}
	}
	return ""
}

func getResourceFlavor(t *testing.T, cli ctrlcli.Client, name string) (*kueue.ResourceFlavor, error) {
	t.Helper()
	rf := new(kueue.ResourceFlavor)
	err := cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, rf)
	return rf, err
}

func TestNodeFlavorReconciler_Reconcile(t *testing.T) {
	// The CPU flavor name depends only on the general profile, so derive it once.
	cpuName := cpuFlavorName(newManagedCPUNode("probe", 4, 16, 32))

	cases := []struct {
		name string

		nodes      int  // number of contributing managed CPU nodes
		withFlavor bool // an existing RF with the cpu name is present

		wantExists   bool
		wantCapacity string // the feature key's .capacity label; "" → not asserted
	}{
		{
			// One node contributes: the flavor is created, active, capacity = 1×4 = 4.
			name:         "creates flavor for one node",
			nodes:        1,
			wantExists:   true,
			wantCapacity: "4",
		},
		{
			// Capacity scales with the pooled node count: 3 nodes of count 4 → 12.
			name:         "capacity scales with node count",
			nodes:        3,
			wantExists:   true,
			wantCapacity: "12",
		},
		{
			// No node contributes and no flavor exists: a no-op, nothing is created.
			name: "not found and unused is noop",
		},
		{
			// An existing flavor that no node contributes to is DELETED (no drain).
			name:       "deletes unused flavor",
			withFlavor: true,
			wantExists: false,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.withFlavor {
				objs = append(objs, newNodesFlavor(cpuName, "generic", 4, 4))
			}
			for i := 0; i < c.nodes; i++ {
				objs = append(objs, newManagedCPUNode("node-"+itoa(int64(i)), 4, 16, 32))
			}
			cli := buildNodeFlavorClient(objs...)

			reconcileNodeFlavor(t, cli, cpuName)

			got, err := getResourceFlavor(t, cli, cpuName)
			if !c.wantExists {
				assert.Truef(t, kerrors.IsNotFound(err),
					"flavor must not exist, got err=%v", err)
				return
			}
			require.NoError(t, err, "flavor must be created/kept")
			if c.wantCapacity != "" {
				capacity := parseResourceFlavorCapacity(got)
				assert.Equal(t, c.wantCapacity, itoa(capacity), "capacity label")
			}
		})
	}
}

// TestNodeFlavorReconciler_ActiveShape pins the full shape an active CPU flavor is
// materialized with: schedule labels, pinned nodeLabels, a blanket toleration, and
// the "nodes" notes.
func TestNodeFlavorReconciler_ActiveShape(t *testing.T) {
	nd := newManagedCPUNode("node-0", 4, 16, 32)
	cpuName := cpuFlavorName(nd)
	cli := buildNodeFlavorClient(nd)

	reconcileNodeFlavor(t, cli, cpuName)

	rf, err := getResourceFlavor(t, cli, cpuName)
	require.NoError(t, err)

	// Schedule labels carry the flavor identity (feature key, full os/arch) and the
	// per-key count/capacity (1 node × 4).
	gKey := nodefeature.GeneralFeatureLabelPrefix + "generic"
	assert.Equal(t, "true", rf.Labels[gKey], "feature key label")
	assert.Equal(t, "linux", rf.Labels[core.LabelOSStable], "os label (full)")
	assert.Equal(t, "amd64", rf.Labels[core.LabelArchStable], "arch label (full)")
	assert.Equal(t, "4", rf.Labels[gKey+_ResourceFlavorCountLabelSuffix], "count label")
	assert.Equal(t, "4", rf.Labels[gKey+_ResourceFlavorCapacityLabelSuffix], "capacity label")

	// nodeLabels pin the pooled nodes; a blanket Exists toleration is set.
	assert.Equal(t, "true", rf.Spec.NodeLabels[systemname.ManagedLabelKey], "managed pinned")
	assert.Equal(t, "linux", rf.Spec.NodeLabels[core.LabelOSStable], "os pinned (full)")
	assert.Equal(t, "amd64", rf.Spec.NodeLabels[core.LabelArchStable], "arch pinned (full)")
	assert.Equal(t, "true", rf.Spec.NodeLabels[gKey], "feature key pinned")
	assert.Equal(t, "4", rf.Spec.NodeLabels[gKey+_ResourceFlavorCountLabelSuffix], "count pinned in nodeLabels")
	require.Len(t, rf.Spec.Tolerations, 1, "blanket toleration set")
	assert.Equal(t, core.TolerationOpExists, rf.Spec.Tolerations[0].Operator, "tolerates any taint")

	// Notes carry the descriptive device fields under resType "nodes"; the unit spec is
	// no longer a flavor note (it is a fixed default on the InstanceType).
	resType, notes := systemmeta.DescribeResource(rf)
	assert.Equal(t, _ResourceFlavorResType, resType, "resType")
	assert.Equal(t, "false", notes["acceleratable"], "acceleratable note")
	assert.Equal(t, "generic", notes["manufacturer"], "manufacturer note")
	assert.Equal(t, "generic", notes["group"], "group note")
	assert.Empty(t, notes["cores"], "no cores for a cpu flavor")
	assert.NotContains(t, notes, "unitCPU", "no unit spec in flavor notes")
	assert.NotContains(t, notes, "unitRAM", "no unit spec in flavor notes")
	assert.NotContains(t, notes, "localStorage", "no unit spec in flavor notes")
}

// TestNodeFlavorReconciler_ActiveShapeAccelerated pins a device flavor's notes: it
// is marked acceleratable and carries the per-card manufacturer/product/family/VRAM.
func TestNodeFlavorReconciler_ActiveShapeAccelerated(t *testing.T) {
	nd := newManagedAccelNode("node-g", 2)
	devName := deviceFlavorName(nd)
	cli := buildNodeFlavorClient(nd)

	reconcileNodeFlavor(t, cli, devName)

	rf, err := getResourceFlavor(t, cli, devName)
	require.NoError(t, err)

	aKey := nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g"
	assert.Equal(t, "true", rf.Labels[aKey], "device feature key label")
	assert.Equal(t, "2", rf.Labels[aKey+_ResourceFlavorCountLabelSuffix], "device count label")
	// capacity = 1 node × 2 cards.
	assert.Equal(t, "2", rf.Labels[aKey+_ResourceFlavorCapacityLabelSuffix], "device capacity label")

	_, notes := systemmeta.DescribeResource(rf)
	assert.Equal(t, "true", notes["acceleratable"], "acceleratable note")
	assert.Equal(t, "nvidia", notes["manufacturer"], "manufacturer note")
	assert.Equal(t, "nvidia-a10g", notes["group"], "group note")
	assert.Equal(t, "NVIDIA A10G", notes["product"], "product note")
	assert.Equal(t, "ampere", notes["family"], "family note")
	assert.Equal(t, "24Gi", notes["memory"], "per-card VRAM note")
	assert.Equal(t, "9216", notes["cores"], "per-card cores note")
}

// TestNodeFlavorReconciler_MixingDisabledExcludesAccelNode pins the
// instance-type-mixed-on-node switch. The unit-test binary resolves the setting to
// false (the empty loopback client makes it fall back to its on-error default), and
// flipping a cached setting back to true is not deterministic in a shared binary
// (the value caches for 30s), so only the false branch is asserted here: an
// accelerated node does NOT contribute to a CPU flavor, so reconciling that node's
// CPU flavor name creates nothing.
func TestNodeFlavorReconciler_MixingDisabledExcludesAccelNode(t *testing.T) {
	nd := newManagedAccelNode("node-g", 1)
	cpuName := cpuFlavorName(nd)
	require.NotEmpty(t, cpuName, "accel node must expose a CPU flavor name")
	cli := buildNodeFlavorClient(nd)

	reconcileNodeFlavor(t, cli, cpuName)

	_, err := getResourceFlavor(t, cli, cpuName)
	assert.Truef(t, kerrors.IsNotFound(err),
		"accelerated node must not contribute to a CPU flavor when mixing is off, got err=%v", err)

	// The device flavor is still materialized — mixing only suppresses the CPU side.
	reconcileNodeFlavor(t, cli, deviceFlavorName(nd))
	_, err = getResourceFlavor(t, cli, deviceFlavorName(nd))
	assert.NoError(t, err, "device flavor must still be created")
}

// TestNodeFlavorReconciler_AuthorsDerivedInstanceType pins that, with
// instance-type-derived-from-node enabled, syncing a pool's flavor authors the pool's
// InstanceType: marked derived, stamped with the pool identity (group/acceleratable/os/arch) and
// the fixed default unit spec chosen by acceleratable-ness — and only ever created, so an
// existing (admin) type is left untouched. (The off branch is not asserted: the setting caches
// once enabled in the shared test binary.)
func TestNodeFlavorReconciler_AuthorsDerivedInstanceType(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	t.Run("accelerated pool: derived marker, spec identity, 4c/16Gi/100Gi default", func(t *testing.T) {
		nd := newManagedAccelNode("node-g", 1)
		cli := buildNodeFlavorClient(nd)

		reconcileNodeFlavor(t, cli, deviceFlavorName(nd))

		it := new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Name: nodeQueueName("nvidia-a10g")}, it))
		assert.Equal(t, "true", it.Labels[_InstanceTypeDerivedFromNodeLabel], "marked derived")
		assert.Equal(t, "nvidia-a10g", it.Spec.Group, "spec carries the pool group")
		assert.True(t, it.Spec.Acceleratable, "spec marked acceleratable")
		assert.Equal(t, "linux", it.Spec.OS, "spec os")
		assert.Equal(t, "amd64", it.Spec.Arch, "spec arch")
		assert.Equal(t, "4", it.Spec.UnitResources.CPU, "accelerated unit CPU default")
		assert.Equal(t, "16Gi", it.Spec.UnitResources.RAM, "accelerated unit RAM default")
		assert.Equal(t, "100Gi", it.Spec.LocalStorage, "unit localStorage default")
		// The feature-key metadata label is not stamped — it derives from the spec.
		assert.NotContains(t, it.Labels, featureKeyLabel(true, "nvidia-a10g"))
	})

	t.Run("cpu-only pool: 1c/2Gi/100Gi default", func(t *testing.T) {
		nd := newManagedCPUNode("node-0", 4, 16, 32)
		cli := buildNodeFlavorClient(nd)

		reconcileNodeFlavor(t, cli, cpuFlavorName(nd))

		it := new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Name: nodeQueueName("generic")}, it))
		assert.Equal(t, "true", it.Labels[_InstanceTypeDerivedFromNodeLabel], "marked derived")
		assert.False(t, it.Spec.Acceleratable, "spec not acceleratable")
		assert.Equal(t, "1", it.Spec.UnitResources.CPU, "cpu-only unit CPU default")
		assert.Equal(t, "2Gi", it.Spec.UnitResources.RAM, "cpu-only unit RAM default")
		assert.Equal(t, "100Gi", it.Spec.LocalStorage, "unit localStorage default")
	})

	t.Run("create-only: an existing InstanceType is left untouched", func(t *testing.T) {
		nd := newManagedCPUNode("node-0", 4, 16, 32)
		existing := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: nodeQueueName("generic")},
			Spec: workercore.InstanceTypeSpec{
				Group:         "generic",
				OS:            "linux",
				Arch:          "amd64",
				UnitResources: workercore.InstanceTypeUnitResources{CPU: "2", RAM: "16Gi"},
				LocalStorage:  "128Gi",
			},
		}
		cli := buildNodeFlavorClient(nd, existing)

		reconcileNodeFlavor(t, cli, cpuFlavorName(nd))

		it := new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Name: nodeQueueName("generic")}, it))
		assert.Equal(t, "2", it.Spec.UnitResources.CPU, "admin unit spec preserved (create-only)")
		assert.Equal(t, "16Gi", it.Spec.UnitResources.RAM, "admin unit spec preserved")
		assert.Equal(t, "128Gi", it.Spec.LocalStorage, "admin unit spec preserved")
		assert.NotContains(t, it.Labels, _InstanceTypeDerivedFromNodeLabel,
			"an existing type is not re-marked derived")
	})
}

func TestIndexNodeByScheduleFlavor(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(nd *core.Node)
		present bool // the node's CPU flavor name is in the index
	}{
		{
			name:    "managed schedulable node is indexed",
			present: true,
		},
		{
			name:    "unmanaged node is excluded",
			mutate:  func(nd *core.Node) { nd.Labels[systemname.ManagedLabelKey] = "false" },
			present: false,
		},
		{
			name: "tainted node is still indexed (taints are ignored)",
			mutate: func(nd *core.Node) {
				nd.Spec.Taints = append(nd.Spec.Taints, core.Taint{
					Key:    core.TaintNodeUnreachable,
					Effect: core.TaintEffectNoSchedule,
				})
			},
			present: true,
		},
		{
			name: "deleting node still counts as present",
			mutate: func(nd *core.Node) {
				now := meta.Now()
				nd.DeletionTimestamp = &now
				nd.Finalizers = []string{"gpustack.ai/test"}
			},
			present: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			nd := newManagedCPUNode("node-0", 4, 16, 32)
			want := cpuFlavorName(nd)
			if c.mutate != nil {
				c.mutate(nd)
			}
			got := indexNodeByScheduleFlavor(nd)
			assert.Equal(t, c.present, contains(got, want),
				"index membership of %q in %v", want, got)
		})
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
