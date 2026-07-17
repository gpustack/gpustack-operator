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

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
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
// node — the presence gate desiredSlicedCapacity reads before emitting ".sliced.*".
func withSlicedPool(nd *core.Node, manufacturer string, tokens int64) *core.Node {
	if nd.Status.Capacity == nil {
		nd.Status.Capacity = core.ResourceList{}
	}
	name := nodefeature.GetAcceleratableResourceName(manufacturer, workercore.DeviceAllocationModeSliced)
	nd.Status.Capacity[name] = *resource.NewQuantity(tokens, resource.DecimalSI)
	return nd
}

// vendorSlicing is one device group's logical-slicing capability for a Devices fixture: the
// group ID (which must equal the model part of the node's aKey, "${manufacturer}-${id}"), the
// manufacturer, and the per-device slice count / overcommit. A maxSlices of 0 models a
// non-sliceable group.
type vendorSlicing struct {
	manufacturer string
	id           string
	maxSlices    int32
	overcommit   bool
}

// devicesWithSlicing builds a same-named Devices CR whose groups carry the given per-model
// logical-slicing capability, resolved by "${manufacturer}-${group ID}" so desiredSlicedCapacity
// maps each node model to its own group.
func devicesWithSlicing(name string, vendors ...vendorSlicing) *workercore.Devices {
	d := &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: name}}
	for _, v := range vendors {
		d.Spec.Groups = append(d.Spec.Groups, workercore.DevicesGroup{
			ID:           v.id,
			Manufacturer: v.manufacturer,
			AcceleratorsFeature: workercore.AcceleratorsFeature{
				LogicalSliced: workercore.AcceleratorSliced{
					MaxSize:                   v.maxSlices,
					CoresPercentageOvercommit: v.overcommit,
					MemoryPercentageStep:      1,
				},
			},
		})
	}
	return d
}

func TestDesiredSlicedCapacity(t *testing.T) {
	cases := []struct {
		name string
		node *core.Node
		devs *workercore.Devices
		want map[string]int64 // resource name → value; nil → empty
	}{
		{
			// Overcommit vendor (NVIDIA 128): cores-percentage = cards × maxSlices × 100.
			name: "nvidia overcommit reports cards × maxSlices × 100 cores",
			node: withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "8", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 8*128),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true}),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 8, 8*a10gMib, 128, true),
		},
		{
			// Partition vendor (MThreads 16, no overcommit): cores-percentage = cards × 100.
			name: "mthreads no-overcommit caps cores at cards × 100",
			node: withSlicedPool(acceleratableNode("node-5", "mthreads-s4000", "8", "48Gi", true),
				nodefeature.ManufacturerMThreads, 8*16),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerMThreads, "s4000", 16, false}),
			want: slicedWant(nodefeature.ManufacturerMThreads, 8, 8*mthreadsMib, 16, false),
		},
		{
			name: "unmanaged node reports nothing",
			node: withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", false),
				nodefeature.ManufacturerNVIDIA, 4*128),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true}),
			want: nil,
		},
		{
			// The device-plugin advertises no ".sliced" pool → presence gate fails.
			name: "no sliced token pool advertised is gated out",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true}),
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
				vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true},
				vendorSlicing{nodefeature.ManufacturerNVIDIA, "t4", 128, true}),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 6, 4*a10gMib+2*t4Mib, 128, true),
		},
		{
			// A model missing its ".memory" label contributes 0 to memory-mib but
			// still the other three keys.
			name: "missing VRAM yields zero memory-mib, other keys intact",
			node: withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "4", "", true),
				nodefeature.ManufacturerNVIDIA, 4*128),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true}),
			want: slicedWant(nodefeature.ManufacturerNVIDIA, 4, 0, 128, true),
		},
		{
			name: "non-positive count is skipped",
			node: withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "0", "24Gi", true),
				nodefeature.ManufacturerNVIDIA, 128),
			devs: devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true}),
			want: nil,
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
				vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true},
				vendorSlicing{nodefeature.ManufacturerMThreads, "s4000", 16, false}),
			want: mergeWant(
				slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib, 128, true),
				slicedWant(nodefeature.ManufacturerMThreads, 2, 2*mthreadsMib, 16, false),
			),
		},
		{
			// Two manufacturers whose group IDs collide (ConstructGroupID strips the vendor
			// prefix, so both normalize to "gpu"): keying slicing features by the full
			// "${manufacturer}-${id}" keeps each model's own maxSlices/overcommit — a bare-ID
			// key would let the second group overwrite the first and cross-contaminate them.
			name: "colliding group IDs across manufacturers stay isolated",
			node: withSlicedPool(
				withSlicedPool(
					withModel(acceleratableNode("node-5", "nvidia-gpu", "4", "24Gi", true), "mthreads-gpu", "2", "48Gi"),
					nodefeature.ManufacturerNVIDIA, 4*128),
				nodefeature.ManufacturerMThreads, 2*16),
			devs: devicesWithSlicing("node-5",
				vendorSlicing{nodefeature.ManufacturerNVIDIA, "gpu", 128, true},
				vendorSlicing{nodefeature.ManufacturerMThreads, "gpu", 16, false}),
			want: mergeWant(
				slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib, 128, true),
				slicedWant(nodefeature.ManufacturerMThreads, 2, 2*mthreadsMib, 16, false),
			),
		},
		{
			// A manufacturer with a sliceable model (Ascend 910B) and a non-sliceable one
			// (Ascend 310, MaxSize 0) counts only the sliceable model's cards into ".sliced.*";
			// the 310's two cards are excluded even though the bare pool is manufacturer-wide.
			name: "mixed sliceable and non-sliceable models count only the sliceable",
			node: withSlicedPool(
				withModel(acceleratableNode("node-5", "ascend-910b", "4", "64Gi", true), "ascend-310", "2", "24Gi"),
				nodefeature.ManufacturerAscend, 4*63),
			devs: devicesWithSlicing("node-5",
				vendorSlicing{nodefeature.ManufacturerAscend, "910b", 63, true},
				vendorSlicing{nodefeature.ManufacturerAscend, "310", 0, false}),
			want: slicedWant(nodefeature.ManufacturerAscend, 4, 4*ascend910bMib, 63, true),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := desiredSlicedCapacity(c.node, c.devs)
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
// label no longer affects capacity (slicing capability is sourced from Devices).
func TestDesiredSlicedCapacityIgnoresPartitions(t *testing.T) {
	nd := withSlicedPool(acceleratableNode("node-5", "nvidia-a10g", "4", "24Gi", true),
		nodefeature.ManufacturerNVIDIA, 4*128)
	// An invalid partitions value would have suppressed capacity before; now ignored.
	nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-a10g.sliced.partitions"] = "3"
	devs := devicesWithSlicing("node-5", vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true})

	got := desiredSlicedCapacity(nd, devs)
	want := slicedWant(nodefeature.ManufacturerNVIDIA, 4, 4*a10gMib, 128, true)
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

func TestSlicedFamilyCapacityChanged(t *testing.T) {
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
	assert.False(t, slicedFamilyCapacityChanged(units, units), "owned key unchanged")
	assert.True(t, slicedFamilyCapacityChanged(units, unitsLess), "owned key value changed")
	assert.True(t, slicedFamilyCapacityChanged(units, none), "owned key removed")
	assert.True(t, slicedFamilyCapacityChanged(none, units), "owned key added")
	// Bare device-plugin ".sliced" token pool (the presence gate).
	assert.True(t, slicedFamilyCapacityChanged(none, pool), "bare .sliced pool added")
	assert.True(t, slicedFamilyCapacityChanged(pool, none), "bare .sliced pool removed")
	assert.True(t, slicedFamilyCapacityChanged(pool, poolLess), "bare .sliced pool value changed")
	// Only CPU changes are ignored.
	assert.False(t, slicedFamilyCapacityChanged(none, none), "only cpu, no change")
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
		devs := devicesWithSlicing(node, vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true})
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
		devs := devicesWithSlicing(node, vendorSlicing{nodefeature.ManufacturerNVIDIA, "a10g", 128, true})
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
