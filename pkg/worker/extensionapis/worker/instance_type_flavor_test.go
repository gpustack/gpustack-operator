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
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/system"
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

// TestInstanceTypeFlavorHandler_OnList pins the aggregation contract with CPU-manufacturer
// awareness OFF (the value the aggregated apiserver reads from the remote resolves to the "false"
// default here): every operator ResourceFlavor collapses by the awareness setting — accelerated
// flavors pool per accelerator (CPU ignored, so different-CPU variants of one accelerator dedup to
// one row) and non-accelerated flavors pool into one CPU-agnostic "generic" row — a flavor with no
// group note is skipped, a non-operator flavor is excluded, and the list sorts
// manufacturer → product → memory. (The aware=true split is an e2e case: the editable setting
// caches globally in the shared test binary.)
func TestInstanceTypeFlavorHandler_OnList(t *testing.T) {
	// ShouldValueFromRemote reads the loopback kube client; configure a fake one with no setting
	// Secret so the read falls back to the "false" default (aware off) instead of panicking.
	system.LoopbackKubeClient.Configure(kubefake.NewSimpleClientset())

	objs := []ctrlcli.Object{
		// Two accelerated a10g flavors on different CPUs: with awareness off the CPU is ignored,
		// so both collapse to one "nvidia-a10g" row.
		flavorWithNotes("gpustack--amd-epyc-7763--nvidia-a10g-linux-amd64-2d", map[string]string{
			"generalGroup": "amd-epyc-7763", "acceleratorGroup": "nvidia-a10g", "acceleratable": "true",
			"manufacturer": "nvidia", "product": "NVIDIA A10G", "family": "ampere",
			"memory": "24Gi", "cores": "9216", "sliceable": "true",
		}),
		flavorWithNotes("gpustack--intel-xeon-8358--nvidia-a10g-linux-amd64-4d", map[string]string{
			"generalGroup": "intel-xeon-8358", "acceleratorGroup": "nvidia-a10g", "acceleratable": "true",
			"manufacturer": "nvidia", "product": "NVIDIA A10G", "family": "ampere",
			"memory": "24Gi", "cores": "9216", "sliceable": "true",
		}),
		flavorWithNotes("gpustack--amd-epyc-7763--nvidia-h100-linux-amd64-8d", map[string]string{
			"generalGroup": "amd-epyc-7763", "acceleratorGroup": "nvidia-h100", "acceleratable": "true",
			"manufacturer": "nvidia", "product": "NVIDIA H100", "family": "hopper",
			"memory": "80Gi", "cores": "16896", "sliceable": "true",
		}),
		// Two generic CPU-only flavors of different CPUs: with awareness off they collapse to one
		// CPU-agnostic "generic" row.
		flavorWithNotes("gpustack--amd-epyc-7763-linux-amd64-8c", map[string]string{
			"generalGroup": "amd-epyc-7763", "acceleratable": "false", "manufacturer": "amd",
		}),
		flavorWithNotes("gpustack--intel-xeon-8358-linux-amd64-16c", map[string]string{
			"generalGroup": "intel-xeon-8358", "acceleratable": "false", "manufacturer": "intel",
		}),
		// No group note → not an operator pool → skipped.
		flavorWithNotes("orphan-no-group", map[string]string{"manufacturer": "nvidia"}),
		// Owned by a different subsystem (resType != nodes): excluded by the operator-owned
		// scoping even though it carries group notes.
		flavorOfType("stray-other-type", "instances", map[string]string{"generalGroup": "stray", "manufacturer": "acme"}),
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	h := &InstanceTypeFlavorHandler{APIReader: cli}

	obj, err := h.OnList(context.Background(), ctrlcli.ListOptions{})
	require.NoError(t, err)
	list, ok := obj.(*worker.InstanceTypeFlavorList)
	require.True(t, ok, "OnList must return an InstanceTypeFlavorList")

	// Collapsed to three pools: one generic (both CPUs), one per accelerator (both CPU variants
	// of a10g dedup). The group-less orphan and the non-operator flavor are skipped.
	require.Len(t, list.Items, 3, "one row per distinct collapsed pool")
	for _, itf := range list.Items {
		assert.NotEqual(t, "stray", itf.Spec.AcceleratorGroup, "a non-operator flavor must not surface")
	}

	// Sorted manufacturer → product → memory: generic first, then nvidia A10G before H100.
	assert.Equal(t, "gpustack--generic", list.Items[0].Name)
	assert.Equal(t, "gpustack--nvidia-a10g", list.Items[1].Name)
	assert.Equal(t, "gpustack--nvidia-h100", list.Items[2].Name)

	// Generic collapses to one CPU-agnostic row: acceleratable=false, GeneralGroup="generic",
	// no manufacturer sentinel, and no accelerator group / memory / cores.
	generic := list.Items[0]
	assert.False(t, generic.Spec.Acceleratable)
	assert.Equal(t, "generic", generic.Spec.GeneralGroup)
	assert.Empty(t, generic.Spec.Manufacturer, "CPU-agnostic row carries no manufacturer sentinel")
	assert.Empty(t, generic.Spec.AcceleratorGroup)
	assert.Empty(t, generic.Spec.Memory)
	assert.Empty(t, generic.Spec.Cores)

	// The accelerated row carries the accelerator group + device descriptors; with awareness off
	// it carries no CPU (GeneralGroup empty), so the two CPU variants collapsed into it.
	a10g := list.Items[1]
	assert.Equal(t, "nvidia-a10g", a10g.Spec.AcceleratorGroup)
	assert.Empty(t, a10g.Spec.GeneralGroup, "CPU ignored when awareness is off")
	assert.True(t, a10g.Spec.Acceleratable)
	assert.Equal(t, "nvidia", a10g.Spec.Manufacturer)
	assert.Equal(t, "NVIDIA A10G", a10g.Spec.Product)
	assert.Equal(t, "ampere", a10g.Spec.Family)
	assert.Equal(t, "24Gi", a10g.Spec.Memory)
	assert.Equal(t, "9216", a10g.Spec.Cores)
	assert.True(t, a10g.Spec.Sliceable)
}
