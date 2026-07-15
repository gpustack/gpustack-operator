package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
)

var (
	nvidiaSlicedUnits  = nodefeature.GetAcceleratableSlicedUnitsResourceName(nodefeature.ManufacturerNVIDIA)
	nvidiaSlicedCores  = nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(nodefeature.ManufacturerNVIDIA)
	nvidiaSlicedMemPct = nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(nodefeature.ManufacturerNVIDIA)
	nvidiaSlicedMemMib = nodefeature.GetAcceleratableSlicedMemoryMibResourceName(nodefeature.ManufacturerNVIDIA)
)

// Per-card VRAM of the test models, in MiB (the ".memory" feature label is a
// resource.Quantity like "24Gi", which the reconciler reads as MiB).
const (
	a10gMib  = 24 * 1024  // 24Gi
	t4Mib    = 16 * 1024  // 16Gi
	mi300Mib = 192 * 1024 // 192Gi
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
// cards × the per-card constants, plus the (already summed) per-card VRAM total.
func slicedWant(manufacturer string, cards, totalMemMib int64) map[string]int64 {
	return map[string]int64{
		string(nodefeature.GetAcceleratableSlicedUnitsResourceName(manufacturer)):            cards * nodefeature.ResourceMaxUnits,
		string(nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manufacturer)):  cards * nodefeature.SlicedResourceMaxSize * 100,
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

func TestDesiredSlicedCapacity(t *testing.T) {
	cases := []struct {
		name string
		node *core.Node
		want map[string]int64 // resource name → value; nil → empty
	}{
		{
			name: "managed acceleratable model reports four per-card keys",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib),
		},
		{
			name: "unmanaged node reports nothing",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", false),
			want: nil,
		},
		{
			// Slicing is no longer gated by ".sliced.partitions": a model with no
			// such label still reports the four keys.
			name: "model without sliced.partitions still reports",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib),
		},
		{
			// Two models of the same manufacturer sum cards/cores/mem-% into one
			// key set; ".sliced.memory-mib" is weighted per model's own VRAM.
			name: "same manufacturer models sum, memory-mib weighted per model",
			node: withModel(acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true), "nvidia-t4", "2", "16Gi"),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 6, 4*a10gMib+2*t4Mib),
		},
		{
			// A model missing its ".memory" label contributes 0 to memory-mib but
			// still the other three keys.
			name: "missing VRAM yields zero memory-mib, other keys intact",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "", true),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 4, 0),
		},
		{
			name: "non-positive count is skipped",
			node: acceleratableNode("node-5", "nvidia-a10g", "0", "24Gi", true),
			want: nil,
		},
		{
			name: "distinct manufacturers report distinct key sets",
			node: withModel(acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true), "amd-mi300", "2", "192Gi"),
			want: mergeWant(
				slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib),
				slicedWant(nodefeature.ManufacturerAMD, 2, 2*mi300Mib),
			),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := desiredSlicedCapacity(c.node)
			assert.Len(t, got, len(c.want))
			for name, val := range c.want {
				q, ok := got[core.ResourceName(name)]
				require.Truef(t, ok, "missing %s", name)
				assert.Equalf(t, val, q.Value(), "%s value", name)
			}
		})
	}
}

// TestDesiredSlicedCapacityIgnoresPartitions pins that the legacy ".sliced.partitions"
// label no longer affects capacity (slicing is always available).
func TestDesiredSlicedCapacityIgnoresPartitions(t *testing.T) {
	nd := acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true)
	// An invalid partitions value would have suppressed capacity before; now ignored.
	nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-a10g.sliced.partitions"] = "3"

	got := desiredSlicedCapacity(nd)
	want := slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib)
	assert.Len(t, got, len(want))
	for name, val := range want {
		q, ok := got[core.ResourceName(name)]
		require.Truef(t, ok, "missing %s", name)
		assert.Equalf(t, val, q.Value(), "%s value", name)
	}
}

func TestBuildSlicedCapacityPatch(t *testing.T) {
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
			name:    "bare .sliced and .shared device-plugin keys are left untouched",
			desired: nil,
			current: mkCap(map[string]string{"nvidia.com/gpu.sliced": "2048", "nvidia.com/gpu.shared": "10", "cpu": "8"}),
			want:    nil,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := buildSlicedCapacityPatch(c.desired, c.current)
			if c.want == nil {
				assert.Nil(t, got)
				return
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestSlicedCapacityChanged(t *testing.T) {
	cap4 := func(unitsVal, coresVal string) core.ResourceList {
		rl := core.ResourceList{core.ResourceCPU: resource.MustParse("8")}
		if unitsVal != "" {
			rl[nvidiaSlicedUnits] = resource.MustParse(unitsVal)
		}
		if coresVal != "" {
			rl[nvidiaSlicedCores] = resource.MustParse(coresVal)
		}
		return rl
	}
	assert.False(t, slicedCapacityChanged(cap4("6400000", "204800"), cap4("6400000", "204800")), "unchanged")
	assert.True(t, slicedCapacityChanged(cap4("6400000", "204800"), cap4("3200000", "204800")), "units changed")
	assert.True(t, slicedCapacityChanged(cap4("6400000", "204800"), cap4("6400000", "102400")), "cores changed")
	assert.True(t, slicedCapacityChanged(cap4("6400000", "204800"), cap4("", "")), "removed")
	assert.True(t, slicedCapacityChanged(cap4("", ""), cap4("6400000", "204800")), "added")
	assert.False(t, slicedCapacityChanged(cap4("", ""), cap4("", "")), "only cpu, no change")
}

// TestNodeCapacityReconciler_Reconcile verifies the end-to-end node status patch:
// a managed acceleratable node gains the four ".sliced.*" keys, a re-run is a no-op,
// unmanaging the node removes them, and an unmanaged node is left alone.
func TestNodeCapacityReconciler_Reconcile(t *testing.T) {
	const node = "node-5"
	wantUnits := int64(4) * nodefeature.ResourceMaxUnits
	wantMemMib := int64(4) * a10gMib

	build := func(nd *core.Node) ctrlcli.Client {
		nd.Status.Capacity = core.ResourceList{core.ResourceCPU: resource.MustParse("8")}
		return ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(nd).
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
		cli := build(acceleratableNode(node, "nvidia-a10g", "4", "24Gi", true))

		require.NoError(t, reconcile(cli))
		capn := get(cli).Status.Capacity
		assert.Equal(t, wantUnits, val(capn, nvidiaSlicedUnits), "sliced units capacity")
		assert.Equal(t, int64(4)*nodefeature.SlicedResourceMaxSize*100, val(capn, nvidiaSlicedCores), "cores-percentage")
		assert.Equal(t, int64(4)*100, val(capn, nvidiaSlicedMemPct), "memory-percentage")
		assert.Equal(t, wantMemMib, val(capn, nvidiaSlicedMemMib), "memory-mib")
		cpu := capn[core.ResourceCPU]
		assert.Equal(t, "8", cpu.String(), "cpu capacity untouched")

		// Second reconcile is a no-op (capacity already matches).
		require.NoError(t, reconcile(cli))
		assert.Equal(t, wantUnits, val(get(cli).Status.Capacity, nvidiaSlicedUnits), "still present after no-op reconcile")
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
