package worker

import (
	"context"
	"maps"
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
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
)

// newManagedCPUNode builds a managed Node carrying the general(CPU) feature labels
// and the status capacity the reconciler reads — enough for ExtractNodeFlavors to
// emit exactly one CPU flavor named "gpustack--generic-linux-amd64-${cpu}c" (the
// fixture reports no cpu-model, so the general key falls back to "generic").
func newManagedCPUNode(name string, cpu, memGi, stgGi int64) *core.Node {
	nd := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				systemname.ManagedLabelKey: "true",
				core.LabelOSStable:         "linux",
				core.LabelArchStable:       "amd64",
				TopologyProfileLabel:       topologyProfile([]string{core.LabelHostname}),
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
	nd.Labels[nodefeature.NodeCPUOnlyLabelKey] = "true"
	return nd
}

// newManagedAccelNode builds a managed Node carrying one nvidia-a10g accelerator
// (count cards, per-card VRAM) plus its CPU capacity. ExtractNodeFlavors emits two
// flavors: the device flavor "gpustack--generic--nvidia-a10g-linux-amd64-${count}d"
// (the CPU key is "generic" as the fixture reports no cpu-model) and a CPU flavor; the
// umbrella acceleratable label marks the node accelerated.
func newManagedAccelNode(name string, count int64) *core.Node {
	return newManagedAccelNodeOf(name, count, "nvidia-a10g", "NVIDIA-A10G")
}

// newManagedAccelNodeOf is newManagedAccelNode over an arbitrary accelerator key and product.
// The product must be written in the sanitized form the real label constructor emits — a space
// becomes "-" (kubemeta.SanitizeLabelValue) — so the fixture drives the same string a live node
// carries, which is what the preset lookup normalizes.
func newManagedAccelNodeOf(name string, count int64, aKey, product string) *core.Node {
	nd := newManagedCPUNode(name, 48, 192, 100)
	delete(nd.Labels, nodefeature.NodeCPUOnlyLabelKey)
	p := nodefeature.AcceleratableFeatureLabelPrefix + aKey
	nd.Labels[nodefeature.NodeAcceleratableLabelKey] = "true"
	nd.Labels[p] = "true"
	nd.Labels[p+".count"] = itoa(count)
	nd.Labels[p+".product"] = product
	nd.Labels[p+".memory"] = "24Gi"
	nd.Labels[p+".family"] = "ampere"
	nd.Labels[p+".cores"] = "9216"
	return nd
}

// newDetectedAccelNode builds a managed Node whose accelerator labels are produced by the real
// label constructor from a detector-shaped DevicesGroup, rather than hand-written. That is what
// makes it exercise the sanitize-and-length-cap step a live node's product label goes through.
func newDetectedAccelNode(name, manufacturer, product string, memoryMib uint64, count int) *core.Node {
	nd := newManagedCPUNode(name, 48, 192, 100)
	delete(nd.Labels, nodefeature.NodeCPUOnlyLabelKey)
	nd.Labels[nodefeature.NodeAcceleratableLabelKey] = "true"
	maps.Copy(nd.Labels, nodefeature.ConstructAcceleratableNodeLabels(device.DevicesGroupList{{
		ID:           device.ConstructGroupID(manufacturer, product, memoryMib),
		Manufacturer: manufacturer,
		Name:         product,
		Memory:       memoryMib,
		Accelerators: make([]workercore.Accelerator, count),
	}}))
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
			return topologyQualifiedFlavorName(f.Name, nd.Labels[TopologyProfileLabel])
		}
	}
	return ""
}

// deviceFlavorName derives the device ResourceFlavor name a node contributes to.
func deviceFlavorName(nd *core.Node) string {
	for _, f := range nodefeature.ExtractNodeFlavors(nd) {
		if f.Acceleratable {
			return topologyQualifiedFlavorName(f.Name, nd.Labels[TopologyProfileLabel])
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
			// An existing flavor that no node contributes to is deleted. Kueue's
			// resource-in-use finalizer preserves it while a queue still references it;
			// NodeQueue then drops the terminating flavor from that queue.
			name:       "deletes unused flavor",
			withFlavor: true,
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

// TestNodeFlavorReconciler_DeletesReferencedUnusedFlavor pins the handoff to Kueue: deleting an
// orphan does not remove a flavor that is still in a ClusterQueue immediately. Its finalizer keeps
// it observable with a deletion timestamp so NodeQueue can drop that exact reference first.
func TestNodeFlavorReconciler_DeletesReferencedUnusedFlavor(t *testing.T) {
	name := cpuFlavorName(newManagedCPUNode("probe", 4, 16, 32))
	rf := newNodesFlavor(name, "generic", 4, 4)
	rf.Finalizers = []string{"kueue.x-k8s.io/resource-in-use"}
	queue := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: "queue"}, Spec: kueue.ClusterQueueSpec{
		ResourceGroups: []kueue.ResourceGroup{{Flavors: []kueue.FlavorQuotas{{
			Name: kueue.ResourceFlavorReference(name),
		}}}},
	}}
	cli := buildNodeFlavorClient(rf, queue)

	reconcileNodeFlavor(t, cli, name)

	got, err := getResourceFlavor(t, cli, name)
	require.NoError(t, err, "Kueue's finalizer must keep the referenced flavor until the queue drops it")
	assert.NotNil(t, got.DeletionTimestamp, "the orphan must be terminating, not eligible for a fresh queue")
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
	// The generic-vs-accelerated discriminator: a CPU flavor is "false".
	assert.Equal(t, "false", rf.Labels[nodefeature.NodeAcceleratableLabelKey], "acceleratable discriminator")

	// nodeLabels pin the pooled nodes; a blanket Exists toleration is set.
	assert.Equal(t, "true", rf.Spec.NodeLabels[systemname.ManagedLabelKey], "managed pinned")
	assert.Equal(t, "linux", rf.Spec.NodeLabels[core.LabelOSStable], "os pinned (full)")
	assert.Equal(t, "amd64", rf.Spec.NodeLabels[core.LabelArchStable], "arch pinned (full)")
	assert.Equal(t, "true", rf.Spec.NodeLabels[gKey], "feature key pinned")
	profile := nd.Labels[TopologyProfileLabel]
	assert.Equal(t, profile, rf.Spec.NodeLabels[TopologyProfileLabel], "topology profile pinned")
	require.NotNil(t, rf.Spec.TopologyName)
	assert.Equal(t, topologyName(profile), string(*rf.Spec.TopologyName))
	assert.Equal(t, "4", rf.Spec.NodeLabels[gKey+_ResourceFlavorCountLabelSuffix], "count pinned in nodeLabels")
	require.Len(t, rf.Spec.Tolerations, 1, "blanket toleration set")
	assert.Equal(t, core.TolerationOpExists, rf.Spec.Tolerations[0].Operator, "tolerates any taint")

	// Notes carry the descriptive device fields under resType "nodes"; the unit spec is
	// no longer a flavor note (it is a fixed default on the InstanceType).
	resType, notes := systemmeta.DescribeResource(rf)
	assert.Equal(t, _ResourceFlavorResType, resType, "resType")
	assert.Equal(t, "false", notes["acceleratable"], "acceleratable note")
	assert.Equal(t, "generic", notes["manufacturer"], "manufacturer note")
	assert.Equal(t, "generic", notes["generalGroup"], "generalGroup note")
	assert.Empty(t, notes["acceleratorGroup"], "no acceleratorGroup for a cpu flavor")
	assert.Empty(t, notes["cores"], "no cores for a cpu flavor")
	// A CPU flavor always carries the raw CPU detail (here only the manufacturer is
	// reported, so the JSON is minimal but present).
	assert.NotEmpty(t, notes["cpuDetail"], "cpu flavor always records cpuDetail")
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
	// The generic-vs-accelerated discriminator: a device flavor is "true".
	assert.Equal(t, "true", rf.Labels[nodefeature.NodeAcceleratableLabelKey], "acceleratable discriminator")
	// The paired CPU key's presence (the fixture node reports no cpu-model, so "generic"),
	// so an aware (CPU-split) pool can select it.
	assert.Equal(t, "true", rf.Labels[nodefeature.GeneralFeatureLabelPrefix+"generic"], "paired cpu key presence")
	profile := nd.Labels[TopologyProfileLabel]
	assert.Equal(t, profile, rf.Spec.NodeLabels[TopologyProfileLabel], "topology profile pinned")
	require.NotNil(t, rf.Spec.TopologyName)
	assert.Equal(t, topologyName(profile), string(*rf.Spec.TopologyName))

	_, notes := systemmeta.DescribeResource(rf)
	assert.Equal(t, "true", notes["acceleratable"], "acceleratable note")
	assert.Equal(t, "nvidia", notes["manufacturer"], "manufacturer note")
	assert.Equal(t, "generic", notes["generalGroup"], "generalGroup note (paired cpu key)")
	assert.Equal(t, "nvidia-a10g", notes["acceleratorGroup"], "acceleratorGroup note")
	assert.Equal(t, "NVIDIA-A10G", notes["product"], "product note")
	assert.Equal(t, "ampere", notes["family"], "family note")
	assert.Equal(t, "24Gi", notes["memory"], "per-card VRAM note")
	assert.Equal(t, "9216", notes["cores"], "per-card cores note")
	// With CPU-manufacturer awareness off (the unit binary's resolved default), an
	// accelerated flavor does not record cpuDetail — the CPU is not a scheduling axis.
	assert.NotContains(t, notes, "cpuDetail", "accel cpuDetail gated off when unaware")
}

func TestNodeFlavorReconcilerSplitsHardwareByTopologyProfile(t *testing.T) {
	zoneNode := newManagedCPUNode("zone-node", 4, 16, 32)
	zoneProfile := topologyProfile([]string{core.LabelTopologyZone, core.LabelHostname})
	zoneNode.Labels[TopologyProfileLabel] = zoneProfile
	hostNode := newManagedCPUNode("host-node", 4, 16, 32)
	hostProfile := topologyProfile([]string{core.LabelHostname})
	hostNode.Labels[TopologyProfileLabel] = hostProfile
	base := nodefeature.ExtractNodeFlavors(zoneNode)[0].Name
	zoneName := topologyQualifiedFlavorName(base, zoneProfile)
	hostName := topologyQualifiedFlavorName(base, hostProfile)
	cli := buildNodeFlavorClient(zoneNode, hostNode)

	reconcileNodeFlavor(t, cli, zoneName)
	reconcileNodeFlavor(t, cli, hostName)
	reconcileNodeFlavor(t, cli, zoneName)
	reconcileNodeFlavor(t, cli, hostName)

	zoneFlavor, err := getResourceFlavor(t, cli, zoneName)
	require.NoError(t, err)
	hostFlavor, err := getResourceFlavor(t, cli, hostName)
	require.NoError(t, err)
	assert.Equal(t, zoneProfile, zoneFlavor.Spec.NodeLabels[TopologyProfileLabel])
	assert.Equal(t, hostProfile, hostFlavor.Spec.NodeLabels[TopologyProfileLabel])
	assert.NotEqual(t, zoneFlavor.Spec.NodeLabels[TopologyProfileLabel], hostFlavor.Spec.NodeLabels[TopologyProfileLabel])
	assert.Equal(t, int64(4), parseResourceFlavorCapacity(zoneFlavor))
	assert.Equal(t, int64(4), parseResourceFlavorCapacity(hostFlavor))
}

func TestNodeFlavorReconcilerMigratesReferencedProfileWithoutReplacingQueue(t *testing.T) {
	node := newManagedCPUNode("node-a", 4, 16, 32)
	base := nodefeature.ExtractNodeFlavors(node)[0].Name
	oldProfile := node.Labels[TopologyProfileLabel]
	oldName := topologyQualifiedFlavorName(base, oldProfile)
	cli := buildNodeFlavorClient(node)
	reconcileNodeFlavor(t, cli, oldName)
	oldFlavor, err := getResourceFlavor(t, cli, oldName)
	require.NoError(t, err)
	oldFlavor.Finalizers = []string{"kueue.x-k8s.io/resource-in-use"}
	require.NoError(t, cli.Update(context.Background(), oldFlavor))
	queue := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: "queue"}, Spec: kueue.ClusterQueueSpec{
		ResourceGroups: []kueue.ResourceGroup{{Flavors: []kueue.FlavorQuotas{{Name: kueue.ResourceFlavorReference(oldName)}}}},
	}}
	require.NoError(t, cli.Create(context.Background(), queue))
	wantQueue := queue.DeepCopy()
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(node), node))
	newProfile := topologyProfile([]string{core.LabelTopologyZone, core.LabelHostname})
	node.Labels[TopologyProfileLabel] = newProfile
	require.NoError(t, cli.Update(context.Background(), node))
	newName := topologyQualifiedFlavorName(base, newProfile)

	r := &NodeFlavorReconciler{Client: cli}
	_, err = r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: newName}})
	require.NoError(t, err)
	_, err = getResourceFlavor(t, cli, newName)
	require.NoError(t, err, "the replacement profile flavor must exist before retiring its sibling")
	oldFlavor, err = getResourceFlavor(t, cli, oldName)
	require.NoError(t, err)
	assert.NotNil(t, oldFlavor.DeletionTimestamp, "the unbacked sibling flavor must be terminating")
	gotQueue := new(kueue.ClusterQueue)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(queue), gotQueue))
	assert.Equal(t, wantQueue.Spec, gotQueue.Spec, "NodeFlavor must leave queue migration to NodeQueue")
}

func TestNodeFlavorRetiresUnreferencedGeneratedTopology(t *testing.T) {
	profile := topologyProfile([]string{core.LabelTopologyZone, core.LabelHostname})
	name := topologyName(profile)
	flavorName := topologyQualifiedFlavorName("gpustack--generic-linux-amd64-4c", profile)
	tests := []struct {
		name         string
		activeNode   bool
		activeFlavor bool
		managed      bool
		wantDeleted  bool
	}{
		{name: "unreferenced managed topology", managed: true, wantDeleted: true},
		{name: "node still uses profile", activeNode: true, managed: true},
		{name: "another flavor still references topology", activeFlavor: true, managed: true},
		{name: "foreign topology", wantDeleted: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			topology := &kueue.Topology{ObjectMeta: meta.ObjectMeta{Name: name}}
			if tc.managed {
				systemmeta.NoteResource(topology, topologyResourceType, nil)
			}
			objects := []ctrlcli.Object{topology}
			if tc.activeNode {
				objects = append(objects, &core.Node{ObjectMeta: meta.ObjectMeta{
					Name: "node-a", Labels: map[string]string{TopologyProfileLabel: profile},
				}})
			}
			if tc.activeFlavor {
				ref := kueue.TopologyReference(name)
				objects = append(objects, &kueue.ResourceFlavor{
					ObjectMeta: meta.ObjectMeta{Name: "another-flavor"},
					Spec:       kueue.ResourceFlavorSpec{TopologyName: &ref},
				})
			}
			cli := buildNodeFlavorClient(objects...)
			r := &NodeFlavorReconciler{Client: cli}
			_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{
				NamespacedName: ctrlcli.ObjectKey{Name: flavorName},
			})
			require.NoError(t, err)
			err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, new(kueue.Topology))
			if tc.wantDeleted {
				assert.True(t, kerrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestNodeFlavorReconcilerDoesNotRewriteImmutableFlavor(t *testing.T) {
	node := newManagedCPUNode("node-a", 4, 16, 32)
	name := cpuFlavorName(node)
	cli := buildNodeFlavorClient(node)
	reconcileNodeFlavor(t, cli, name)
	flavor, err := getResourceFlavor(t, cli, name)
	require.NoError(t, err)
	drifted := kueue.TopologyReference("foreign-topology")
	flavor.Spec.TopologyName = &drifted
	require.NoError(t, cli.Update(context.Background(), flavor))

	r := &NodeFlavorReconciler{Client: cli}
	_, err = r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	require.ErrorContains(t, err, "immutable topology drift")
	got, getErr := getResourceFlavor(t, cli, name)
	require.NoError(t, getErr)
	require.NotNil(t, got.Spec.TopologyName)
	assert.Equal(t, drifted, *got.Spec.TopologyName)
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

func TestNodeFlavorReconciler_RecreatesFlavorAfterMixingSelectorDrift(t *testing.T) {
	node := newManagedCPUNode("node-a", 4, 16, 32)
	name := cpuFlavorName(node)
	cli := buildNodeFlavorClient(node)
	reconcileNodeFlavor(t, cli, name)

	flavor, err := getResourceFlavor(t, cli, name)
	require.NoError(t, err)
	delete(flavor.Spec.NodeLabels, nodefeature.NodeCPUOnlyLabelKey)
	require.NoError(t, cli.Update(context.Background(), flavor))

	r := &NodeFlavorReconciler{Client: cli}
	_, err = r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	require.NoError(t, err)
	_, err = getResourceFlavor(t, cli, name)
	assert.True(t, kerrors.IsNotFound(err), "stale immutable selector must be retired")

	reconcileNodeFlavor(t, cli, name)
	flavor, err = getResourceFlavor(t, cli, name)
	require.NoError(t, err)
	assert.Equal(t, "true", flavor.Spec.NodeLabels[nodefeature.NodeCPUOnlyLabelKey])
}

func TestNodeFlavorReconciler_KeepsFlavorWhileProfileIsMissing(t *testing.T) {
	node := newManagedCPUNode("node-a", 4, 16, 32)
	name := cpuFlavorName(node)
	cli := buildNodeFlavorClient(node)
	reconcileNodeFlavor(t, cli, name)

	delete(node.Labels, TopologyProfileLabel)
	require.NoError(t, cli.Update(context.Background(), node))
	r := &NodeFlavorReconciler{Client: cli}
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	require.NoError(t, err)
	_, err = getResourceFlavor(t, cli, name)
	assert.NoError(t, err)
}

// TestNodeFlavorReconciler_AuthorsDerivedInstanceType pins that, with
// instance-type-derived-from-node enabled, syncing a pool's flavor authors the pool's
// InstanceType: marked derived, stamped with the pool identity (group/acceleratable/os/arch) and
// the creation-time unit spec — a fixed 1c/2Gi for a CPU-only pool, the per-product preset for
// an accelerated one — and only ever created, so an existing (admin) type is left untouched.
// (The off branch is not asserted: the setting caches once enabled in the shared test binary.)
func TestNodeFlavorReconciler_AuthorsDerivedInstanceType(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	t.Run("accelerated pool: derived marker, spec identity, unit spec", func(t *testing.T) {
		nd := newManagedAccelNode("node-g", 1)
		cli := buildNodeFlavorClient(nd)

		reconcileNodeFlavor(t, cli, deviceFlavorName(nd))

		it := new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Name: nodeQueueName("nvidia-a10g")}, it))
		assert.Equal(t, "true", it.Labels[_InstanceTypeDerivedFromNodeLabel], "marked derived")
		assert.Equal(t, "nvidia-a10g", it.Spec.AcceleratorGroup, "spec carries the accelerator group")
		// Unaware (the unit binary default): the accelerated pool collapses across CPUs, so the
		// general group is the "generic" sentinel.
		assert.Equal(t, "generic", it.Spec.GeneralGroup, "spec carries the generic sentinel general group")
		assert.True(t, it.Spec.Acceleratable, "spec marked acceleratable")
		assert.Equal(t, "linux", it.Spec.OS, "spec os")
		assert.Equal(t, "amd64", it.Spec.Arch, "spec arch")
		assert.Equal(t, "NVIDIA-A10G", it.Spec.DisplayName, "DisplayName stamped from the flavor product at derivation")
		assert.Equal(t, "8", it.Spec.UnitResources.CPU, "accelerated unit CPU, from the A10G preset")
		assert.Equal(t, "64Gi", it.Spec.UnitResources.RAM, "accelerated unit RAM, from the A10G preset")
		assert.Equal(t, "100Gi", it.Spec.LocalStorage, "unit localStorage default")
		// The feature-key metadata label is not stamped — it derives from the spec.
		assert.NotContains(t, it.Labels, featureKeyLabel(true, "nvidia-a10g"))
	})

	t.Run("accelerated pool: a covered product is sized from its preset", func(t *testing.T) {
		nd := newManagedAccelNodeOf("node-h", 8, "nvidia-h100", "NVIDIA-H100-80GB-HBM3")
		cli := buildNodeFlavorClient(nd)

		reconcileNodeFlavor(t, cli, deviceFlavorName(nd))

		it := new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Name: nodeQueueName("nvidia-h100")}, it))
		assert.Equal(t, "12", it.Spec.UnitResources.CPU, "accelerated unit CPU from the preset table")
		assert.Equal(t, "192Gi", it.Spec.UnitResources.RAM, "accelerated unit RAM from the preset table")
		assert.Equal(t, "100Gi", it.Spec.LocalStorage, "localStorage is never preset")
	})

	t.Run("accelerated pool: an unrecognized product keeps the historical value", func(t *testing.T) {
		nd := newManagedAccelNodeOf("node-x", 1, "nvidia-contoso9000", "NVIDIA-Contoso-9000")
		cli := buildNodeFlavorClient(nd)

		reconcileNodeFlavor(t, cli, deviceFlavorName(nd))

		it := new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Name: nodeQueueName("nvidia-contoso9000")}, it))
		assert.Equal(t, "4", it.Spec.UnitResources.CPU, "unmatched products keep the pre-preset unit CPU")
		assert.Equal(t, "16Gi", it.Spec.UnitResources.RAM, "unmatched products keep the pre-preset unit RAM")
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
		assert.Equal(t, "generic", it.Spec.GeneralGroup, "cpu-only pool collapses to the generic general group")
		assert.Empty(t, it.Spec.AcceleratorGroup, "no accelerator group for a cpu-only pool")
		assert.Equal(t, cpuOnlyDisplayName, it.Spec.DisplayName, "collapsed generic pool DisplayName is the CPU-only sentinel")
		assert.Equal(t, "1", it.Spec.UnitResources.CPU, "cpu-only unit CPU default")
		assert.Equal(t, "2Gi", it.Spec.UnitResources.RAM, "cpu-only unit RAM default")
		assert.Equal(t, "100Gi", it.Spec.LocalStorage, "unit localStorage default")
	})

	t.Run("create-only: an existing InstanceType is left untouched", func(t *testing.T) {
		nd := newManagedCPUNode("node-0", 4, 16, 32)
		existing := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: nodeQueueName("generic")},
			Spec: workercore.InstanceTypeSpec{
				GeneralGroup:  "generic",
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

	t.Run("create-only: a re-reconcile never re-sizes, a re-author picks up the current preset", func(t *testing.T) {
		nd := newManagedAccelNodeOf("node-h2", 8, "nvidia-h100", "NVIDIA-H100-80GB-HBM3")
		cli := buildNodeFlavorClient(nd)
		flavor, name := deviceFlavorName(nd), nodeQueueName("nvidia-h100")

		reconcileNodeFlavor(t, cli, flavor)

		// Stand in for a pool authored before presets existed. In production the unit spec is
		// immutable, so the only way a type can carry a stale one is to have been created with
		// it — which is exactly what an operator upgrade leaves behind.
		it := new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, it))
		it.Spec.UnitResources = workercore.InstanceTypeUnitResources{CPU: "4", RAM: "16Gi"}
		require.NoError(t, cli.Update(context.Background(), it))

		reconcileNodeFlavor(t, cli, flavor)

		it = new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, it))
		assert.Equal(t, "4", it.Spec.UnitResources.CPU, "an existing derived type is never re-sized")
		assert.Equal(t, "16Gi", it.Spec.UnitResources.RAM, "an existing derived type is never re-sized")

		require.NoError(t, cli.Delete(context.Background(), it))
		reconcileNodeFlavor(t, cli, flavor)

		it = new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, it))
		assert.Equal(t, "12", it.Spec.UnitResources.CPU, "a re-authored type takes the current preset")
		assert.Equal(t, "192Gi", it.Spec.UnitResources.RAM, "a re-authored type takes the current preset")
	})
}

// TestNodeFlavorReconciler_EnqueueResourceFlavorsWhenDerivedInstanceTypeDeleted covers the only
// signal the reconciler has that an InstanceType it authored is gone: the flavors that derive
// them never change on their own, so without this mapping a deleted type is never authored again.
func TestNodeFlavorReconciler_EnqueueResourceFlavorsWhenDerivedInstanceTypeDeleted(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	deleted := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:   nodeQueueName("generic"),
			Labels: map[string]string{_InstanceTypeDerivedFromNodeLabel: "true"},
		},
	}

	t.Run("enqueues the managed flavors", func(t *testing.T) {
		nd := newManagedCPUNode("node-0", 4, 16, 32)
		cli := buildNodeFlavorClient(nd)
		// Reconcile once so the managed ResourceFlavor exists to be enqueued.
		reconcileNodeFlavor(t, cli, cpuFlavorName(nd))

		r := &NodeFlavorReconciler{Client: cli}
		reqs := r.enqueueResourceFlavorsWhenDerivedInstanceTypeDeleted(context.Background(), deleted)

		assert.Equal(t, []ctrlreconcile.Request{
			{NamespacedName: ctrlcli.ObjectKey{Name: cpuFlavorName(nd)}},
		}, reqs)
	})

	t.Run("enqueues nothing without a managed flavor", func(t *testing.T) {
		r := &NodeFlavorReconciler{Client: buildNodeFlavorClient()}
		reqs := r.enqueueResourceFlavorsWhenDerivedInstanceTypeDeleted(context.Background(), deleted)

		assert.Empty(t, reqs)
	})
}

// TestNodeFlavorReconciler_PresetPipeline drives detector-shaped product names through the whole
// path a live cluster takes — DevicesGroup → ConstructAcceleratableNodeLabels → ExtractNodeFlavors
// → authorDerivedInstanceType — and asserts the unit spec that lands on the InstanceType. It is
// the only test exercising the label sanitize-and-length-cap step that happens before the preset
// lookup normalizes, and the only one that would catch a table entry written against a marketing
// name no detector actually emits.
func TestNodeFlavorReconciler_PresetPipeline(t *testing.T) {
	enableInstanceTypeDerivedFromNode(t)

	cases := []struct {
		name         string
		manufacturer string
		product      string
		memoryMib    uint64
		cpu          string
		ram          string
	}{
		{"nvidia hopper", nodefeature.ManufacturerNVIDIA, "NVIDIA H100 80GB HBM3", 81559, "12", "192Gi"},
		{"nvidia ampere sku split", nodefeature.ManufacturerNVIDIA, "NVIDIA A100-SXM4-40GB", 40960, "8", "64Gi"},
		{"nvidia turing", nodefeature.ManufacturerNVIDIA, "Tesla T4", 15360, "8", "32Gi"},
		{"nvidia geforce", nodefeature.ManufacturerNVIDIA, "NVIDIA GeForce RTX 4090", 24564, "8", "64Gi"},
		{"ascend bare chip name", nodefeature.ManufacturerAscend, "910B2", 65536, "8", "64Gi"},
		{"amd instinct", nodefeature.ManufacturerAMD, "AMD Instinct MI300X", 196608, "12", "192Gi"},
		{"cambricon", nodefeature.ManufacturerCambricon, "MLU370-X8", 49152, "8", "64Gi"},
		{"hygon", nodefeature.ManufacturerHygon, "K100_AI", 65536, "12", "128Gi"},
		{"metax", nodefeature.ManufacturerMetaX, "MXC500", 65536, "8", "64Gi"},
		{"mthreads", nodefeature.ManufacturerMThreads, "MTT S4000", 49152, "8", "64Gi"},
		{"iluvatar", nodefeature.ManufacturerIluvatar, "Iluvatar BI-V150", 32768, "8", "64Gi"},
		{"thead", nodefeature.ManufacturerTHead, "PPU-ZW810E", 98304, "8", "64Gi"},
		{"an unrecognized product", nodefeature.ManufacturerNVIDIA, "NVIDIA Contoso 9000", 8192, "4", "16Gi"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nd := newDetectedAccelNode("node-p", c.manufacturer, c.product, c.memoryMib, 8)
			cli := buildNodeFlavorClient(nd)

			reconcileNodeFlavor(t, cli, deviceFlavorName(nd))

			it := new(workercore.InstanceType)
			require.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Name: derivedInstanceTypeName(nd)}, it))
			assert.Equal(t, c.cpu, it.Spec.UnitResources.CPU, "unit CPU")
			assert.Equal(t, c.ram, it.Spec.UnitResources.RAM, "unit RAM")
		})
	}

	t.Run("a product whose tail the label cap cuts falls to its entry's base tier", func(t *testing.T) {
		// The label value is capped at 63 characters before the preset lookup ever sees it, and
		// the cap does not preserve token boundaries. A capacity discriminator cut in half must
		// leave the family's base tier, never a wrong variant tier.
		const product = "NVIDIA A100 SXM4 Some Very Long Marketing Qualifier Extended 80GB"
		nd := newDetectedAccelNode("node-t", nodefeature.ManufacturerNVIDIA, product, 81559, 8)
		cli := buildNodeFlavorClient(nd)

		flavor := nodefeature.ExtractNodeFlavors(nd)
		require.Len(t, flavor, 2)
		for _, f := range flavor {
			if f.Acceleratable {
				require.Len(t, f.Product, 63, "the fixture must actually reach the label cap")
			}
		}

		reconcileNodeFlavor(t, cli, deviceFlavorName(nd))

		it := new(workercore.InstanceType)
		require.NoError(t, cli.Get(context.Background(),
			ctrlcli.ObjectKey{Name: derivedInstanceTypeName(nd)}, it))
		assert.Equal(t, "8", it.Spec.UnitResources.CPU, "the base tier, not the 80GB variant")
		assert.Equal(t, "64Gi", it.Spec.UnitResources.RAM, "the base tier, not the 80GB variant")
	})
}

// derivedInstanceTypeName returns the name of the InstanceType a node's device flavor is
// summarized into.
func derivedInstanceTypeName(nd *core.Node) string {
	for _, f := range nodefeature.ExtractNodeFlavors(nd) {
		if f.Acceleratable {
			return nodeQueueName(f.AcceleratorKey)
		}
	}
	return ""
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

// TestSyncNodeFlavorNotes pins that the flavor's operator note set is replaced wholesale, not just
// overwritten — the regression behind the awareness-gated cpuDetail note. NoteResource cannot delete,
// so before this the note-sync used a subset comparison that (a) never added cpuDetail when awareness
// flipped on (existing notes stayed a subset of the desired set) and (b) never removed it when
// awareness flipped off (the stale note lingered). It must add, remove, no-op when equal, drop a
// retired note, and never touch a non-operator annotation.
func TestSyncNodeFlavorNotes(t *testing.T) {
	const detail = `{"product":"AMD EPYC 7R32"}`
	// rf builds a ResourceFlavor carrying the given operator notes plus a foreign annotation the
	// sync must preserve.
	rf := func(notes map[string]string) *kueue.ResourceFlavor {
		f := &kueue.ResourceFlavor{ObjectMeta: meta.ObjectMeta{
			Name:        "gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d",
			Annotations: map[string]string{"example.com/keep": "yes"},
		}}
		systemmeta.NoteResource(f, _ResourceFlavorResType, notes)
		return f
	}

	cases := []struct {
		name        string
		have, want  map[string]string
		wantChanged bool
	}{
		{
			name:        "adds a now-desired note (cpuDetail after awareness flips on)",
			have:        map[string]string{"acceleratable": "true", "acceleratorGroup": "nvidia-a10g"},
			want:        map[string]string{"acceleratable": "true", "acceleratorGroup": "nvidia-a10g", "cpuDetail": detail},
			wantChanged: true,
		},
		{
			name:        "removes a no-longer-desired note (cpuDetail after awareness flips off)",
			have:        map[string]string{"acceleratable": "true", "acceleratorGroup": "nvidia-a10g", "cpuDetail": detail},
			want:        map[string]string{"acceleratable": "true", "acceleratorGroup": "nvidia-a10g"},
			wantChanged: true,
		},
		{
			name:        "no change when the note set already matches",
			have:        map[string]string{"acceleratable": "true", "cpuDetail": detail},
			want:        map[string]string{"acceleratable": "true", "cpuDetail": detail},
			wantChanged: false,
		},
		{
			name:        "drops a retired note not in the desired set",
			have:        map[string]string{"acceleratable": "true", "group": "nvidia-a10g"},
			want:        map[string]string{"acceleratable": "true", "acceleratorGroup": "nvidia-a10g"},
			wantChanged: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := rf(c.have)
			changed := syncNodeFlavorNotes(f, c.want)
			assert.Equal(t, c.wantChanged, changed)

			rt, got := systemmeta.DescribeResource(f)
			assert.Equal(t, _ResourceFlavorResType, rt, "resource type stays stamped")
			assert.Equal(t, c.want, got, "notes equal the desired set exactly")
			assert.Equal(t, "yes", f.Annotations["example.com/keep"], "a non-operator annotation is preserved")
		})
	}
}
