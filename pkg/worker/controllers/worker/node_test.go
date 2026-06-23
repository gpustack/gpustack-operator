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

var nvidiaSlicedUnits = nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)

// acceleratableNode builds a managed Node carrying the acceleratable feature
// labels (presence, count, and optionally sliced.partitions) for one model.
func acceleratableNode(name, aKey, count, partitions string, managed bool) *core.Node {
	labels := map[string]string{
		nodefeature.AcceleratableFeatureLabelPrefix + aKey:            "true",
		nodefeature.AcceleratableFeatureLabelPrefix + aKey + ".count": count,
	}
	if managed {
		labels[systemname.ManagedLabelKey] = "true"
	}
	if partitions != "" {
		labels[nodefeature.AcceleratableFeatureLabelPrefix+aKey+".sliced.partitions"] = partitions
	}
	return &core.Node{ObjectMeta: meta.ObjectMeta{Name: name, Labels: labels}}
}

// withModel adds another acceleratable model's feature labels to the node.
func withModel(nd *core.Node, aKey, count, partitions string) *core.Node {
	nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+aKey] = "true"
	nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+aKey+".count"] = count
	if partitions != "" {
		nd.Labels[nodefeature.AcceleratableFeatureLabelPrefix+aKey+".sliced.partitions"] = partitions
	}
	return nd
}

func TestDesiredSlicedUnitsCapacity(t *testing.T) {
	cases := []struct {
		name string
		node *core.Node
		want map[string]int64 // resource name → value; nil → empty
	}{
		{
			name: "managed sliced model reports D times card count",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "8", true),
			want: map[string]int64{string(nvidiaSlicedUnits): 4 * nodefeature.ResourceMaxUnits},
		},
		{
			name: "unmanaged node reports nothing",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "8", false),
			want: nil,
		},
		{
			name: "no sliced.partitions label reports nothing",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "", true),
			want: nil,
		},
		{
			name: "invalid (non power of two) partitions is ignored",
			node: acceleratableNode("node-5", "nvidia-a10g", "4", "3", true),
			want: nil,
		},
		{
			// Two sliced models of the same manufacturer sum into one key.
			name: "same manufacturer sliced models sum",
			node: withModel(acceleratableNode("node-5", "nvidia-a10g", "4", "8", true), "nvidia-t4", "2", "4"),
			want: map[string]int64{string(nvidiaSlicedUnits): 6 * nodefeature.ResourceMaxUnits},
		},
		{
			// Only the sliced model counts; the non-sliced one is excluded.
			name: "mixed models: only the sliced one counts",
			node: withModel(acceleratableNode("node-5", "nvidia-a10g", "4", "8", true), "nvidia-t4", "2", ""),
			want: map[string]int64{string(nvidiaSlicedUnits): 4 * nodefeature.ResourceMaxUnits},
		},
		{
			// Distinct manufacturers produce distinct keys.
			name: "distinct manufacturers report distinct keys",
			node: withModel(acceleratableNode("node-5", "nvidia-a10g", "4", "8", true), "amd-mi300", "2", "8"),
			want: map[string]int64{
				string(nvidiaSlicedUnits): 4 * nodefeature.ResourceMaxUnits,
				string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerAMD, workercore.DeviceAllocationModeSliced)): 2 * nodefeature.ResourceMaxUnits,
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := desiredSlicedUnitsCapacity(c.node)
			assert.Len(t, got, len(c.want))
			for name, val := range c.want {
				q, ok := got[core.ResourceName(name)]
				require.Truef(t, ok, "missing %s", name)
				assert.Equalf(t, val, q.Value(), "%s value", name)
			}
		})
	}
}

func TestBuildSlicedUnitsCapacityPatch(t *testing.T) {
	const key = "nvidia.com/gpu.sliced.units"
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
			name:    "set on a node without the key",
			desired: mkCap(map[string]string{key: "51200"}),
			current: mkCap(map[string]string{"cpu": "8"}),
			want:    map[string]any{key: "51200"},
		},
		{
			name:    "no change when already equal",
			desired: mkCap(map[string]string{key: "51200"}),
			current: mkCap(map[string]string{key: "51200", "cpu": "8"}),
			want:    nil,
		},
		{
			name:    "update when value differs",
			desired: mkCap(map[string]string{key: "51200"}),
			current: mkCap(map[string]string{key: "25600"}),
			want:    map[string]any{key: "51200"},
		},
		{
			name:    "remove stale key when slicing is disabled",
			desired: nil,
			current: mkCap(map[string]string{key: "51200", "cpu": "8"}),
			want:    map[string]any{key: nil},
		},
		{
			name:    "unrelated keys are left untouched",
			desired: nil,
			current: mkCap(map[string]string{"nvidia.com/gpu.shared": "10", "cpu": "8"}),
			want:    nil,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := buildSlicedUnitsCapacityPatch(c.desired, c.current)
			if c.want == nil {
				assert.Nil(t, got)
				return
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestSlicedUnitsCapacityChanged(t *testing.T) {
	mkCap := func(v string) core.ResourceList {
		if v == "" {
			return core.ResourceList{core.ResourceCPU: resource.MustParse("8")}
		}
		return core.ResourceList{
			core.ResourceCPU:  resource.MustParse("8"),
			nvidiaSlicedUnits: resource.MustParse(v),
		}
	}
	assert.False(t, slicedUnitsCapacityChanged(mkCap("51200"), mkCap("51200")), "unchanged")
	assert.True(t, slicedUnitsCapacityChanged(mkCap("51200"), mkCap("25600")), "value changed")
	assert.True(t, slicedUnitsCapacityChanged(mkCap("51200"), mkCap("")), "removed")
	assert.True(t, slicedUnitsCapacityChanged(mkCap(""), mkCap("51200")), "added")
	assert.False(t, slicedUnitsCapacityChanged(mkCap(""), mkCap("")), "only cpu, no change")
}

// TestNodeCapacityReconciler_Reconcile verifies the end-to-end node status patch:
// a managed sliced node gains ".sliced.units", a re-run is a no-op, disabling
// slicing removes it, and an unmanaged node is left alone.
func TestNodeCapacityReconciler_Reconcile(t *testing.T) {
	const node = "node-5"
	wantUnits := int64(4) * nodefeature.ResourceMaxUnits

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

	t.Run("managed sliced node is patched then idempotent", func(t *testing.T) {
		cli := build(acceleratableNode(node, "nvidia-a10g", "4", "8", true))

		require.NoError(t, reconcile(cli))
		q := get(cli).Status.Capacity[nvidiaSlicedUnits]
		assert.Equal(t, wantUnits, q.Value(), "sliced units capacity")
		cpu := get(cli).Status.Capacity[core.ResourceCPU]
		assert.Equal(t, "8", cpu.String(), "cpu capacity untouched")

		// Second reconcile is a no-op (capacity already matches).
		require.NoError(t, reconcile(cli))
		q = get(cli).Status.Capacity[nvidiaSlicedUnits]
		assert.Equal(t, wantUnits, q.Value(), "still present after no-op reconcile")
	})

	t.Run("disabling slicing removes the capacity", func(t *testing.T) {
		nd := acceleratableNode(node, "nvidia-a10g", "4", "8", true)
		nd.Status.Capacity = core.ResourceList{
			core.ResourceCPU:  resource.MustParse("8"),
			nvidiaSlicedUnits: *resource.NewQuantity(wantUnits, resource.DecimalSI),
		}
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(nd).WithStatusSubresource(&core.Node{}).Build()
		// Drop the partitions label → slicing disabled.
		delete(nd.Labels, nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-a10g.sliced.partitions")
		require.NoError(t, cli.Update(context.Background(), nd))

		require.NoError(t, reconcile(cli))
		_, present := get(cli).Status.Capacity[nvidiaSlicedUnits]
		assert.False(t, present, "sliced units removed")
		cpu := get(cli).Status.Capacity[core.ResourceCPU]
		assert.Equal(t, "8", cpu.String(), "cpu capacity intact")
	})

	t.Run("unmanaged node is left alone", func(t *testing.T) {
		cli := build(acceleratableNode(node, "nvidia-a10g", "4", "8", false))
		require.NoError(t, reconcile(cli))
		_, present := get(cli).Status.Capacity[nvidiaSlicedUnits]
		assert.False(t, present, "unmanaged node not patched")
	})
}
