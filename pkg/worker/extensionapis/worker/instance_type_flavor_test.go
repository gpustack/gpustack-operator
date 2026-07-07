package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// flavorWithNotes builds an operator-owned (resType "nodes") ResourceFlavor carrying the
// given note.gpustack.ai/* annotations.
func flavorWithNotes(name string, notes map[string]string) *kueue.ResourceFlavor {
	return flavorOfType(name, "nodes", notes)
}

// flavorOfType builds a ResourceFlavor of the given systemmeta resource type carrying the
// given notes, so a non-operator flavor can be exercised against the operator-owned scoping.
func flavorOfType(name, resType string, notes map[string]string) *kueue.ResourceFlavor {
	rf := &kueue.ResourceFlavor{ObjectMeta: meta.ObjectMeta{Name: name}}
	systemmeta.NoteResource(rf, resType, notes)
	return rf
}

// TestInstanceTypeFlavorHandler_OnList pins the aggregation contract: every ResourceFlavor
// carrying a "group" note collapses (by full spec) into one InstanceTypeFlavor, flavors
// without a group note are skipped, the descriptor notes (incl. acceleratable/cores/
// sliceable) project onto the spec, and the list sorts by manufacturer → product → memory.
func TestInstanceTypeFlavorHandler_OnList(t *testing.T) {
	objs := []ctrlcli.Object{
		// Two flavors of the same accelerated pool (differing only in per-node count)
		// must collapse to one row.
		flavorWithNotes("gpustack-nvidia-a10g-linux-amd64-2d", map[string]string{
			"group": "nvidia-a10g", "acceleratable": "true", "manufacturer": "nvidia",
			"product": "NVIDIA A10G", "family": "ampere", "memory": "24Gi", "cores": "9216", "sliceable": "true",
		}),
		flavorWithNotes("gpustack-nvidia-a10g-linux-amd64-4d", map[string]string{
			"group": "nvidia-a10g", "acceleratable": "true", "manufacturer": "nvidia",
			"product": "NVIDIA A10G", "family": "ampere", "memory": "24Gi", "cores": "9216", "sliceable": "true",
		}),
		flavorWithNotes("gpustack-nvidia-h100-linux-amd64-8d", map[string]string{
			"group": "nvidia-h100", "acceleratable": "true", "manufacturer": "nvidia",
			"product": "NVIDIA H100", "family": "hopper", "memory": "80Gi", "cores": "16896", "sliceable": "true",
		}),
		// A generic CPU-only pool: acceleratable=false, no memory/cores.
		flavorWithNotes("gpustack-generic-linux-amd64-8c", map[string]string{
			"group": "generic", "acceleratable": "false", "manufacturer": "generic",
		}),
		// No group note → not an operator pool → skipped.
		flavorWithNotes("orphan-no-group", map[string]string{"manufacturer": "nvidia"}),
		// Owned by a different subsystem (resType != nodes): excluded by the operator-owned
		// scoping even though it carries a group note.
		flavorOfType("stray-other-type", "instances", map[string]string{"group": "stray", "manufacturer": "acme"}),
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	h := &InstanceTypeFlavorHandler{APIReader: cli}

	obj, err := h.OnList(context.Background(), ctrlcli.ListOptions{})
	require.NoError(t, err)
	list, ok := obj.(*worker.InstanceTypeFlavorList)
	require.True(t, ok, "OnList must return an InstanceTypeFlavorList")

	// Deduped to three pools; the group-less orphan and the non-operator flavor are skipped.
	require.Len(t, list.Items, 3, "one row per distinct operator-owned pool")
	for _, itf := range list.Items {
		assert.NotEqual(t, "stray", itf.Spec.Group, "a non-operator flavor must not surface")
	}

	// Sorted manufacturer → product → memory: generic first, then nvidia A10G before H100.
	assert.Equal(t, "generic", list.Items[0].Spec.Group)
	assert.Equal(t, "nvidia-a10g", list.Items[1].Spec.Group)
	assert.Equal(t, "nvidia-h100", list.Items[2].Spec.Group)

	// Generic surfaces as acceleratable=false with empty memory/cores.
	assert.False(t, list.Items[0].Spec.Acceleratable)
	assert.Empty(t, list.Items[0].Spec.Memory)
	assert.Empty(t, list.Items[0].Spec.Cores)

	// The accelerated descriptor fields (incl. sliceable) project through.
	a10g := list.Items[1]
	assert.Equal(t, "nvidia-a10g", a10g.Name, "the row is named by its group")
	assert.True(t, a10g.Spec.Acceleratable)
	assert.Equal(t, "nvidia", a10g.Spec.Manufacturer)
	assert.Equal(t, "NVIDIA A10G", a10g.Spec.Product)
	assert.Equal(t, "ampere", a10g.Spec.Family)
	assert.Equal(t, "24Gi", a10g.Spec.Memory)
	assert.Equal(t, "9216", a10g.Spec.Cores)
	assert.True(t, a10g.Spec.Sliceable)
}
