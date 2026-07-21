package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	worker "gpustack.ai/gpustack/api/worker/v1"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// acceleratorFeatureNote is a marshaled AcceleratorsFeature (LogicalSliced 128 / overcommit / step
// 1) a sliceable pool's ResourceFlavor carries in its acceleratorFeature note.
const acceleratorFeatureNote = `{"logicalSliced":{"maxSize":128,"coresPercentageOvercommit":true,"memoryPercentageStep":1}}`

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
			"memory": "24Gi", "cores": "9216", "acceleratorFeature": acceleratorFeatureNote,
		}),
		flavorWithNotes("gpustack--intel-xeon-8358--nvidia-a10g-linux-amd64-4d", map[string]string{
			"generalGroup": "intel-xeon-8358", "acceleratorGroup": "nvidia-a10g", "acceleratable": "true",
			"manufacturer": "nvidia", "product": "NVIDIA A10G", "family": "ampere",
			"memory": "24Gi", "cores": "9216", "acceleratorFeature": acceleratorFeatureNote,
		}),
		flavorWithNotes("gpustack--amd-epyc-7763--nvidia-h100-linux-amd64-8d", map[string]string{
			"generalGroup": "amd-epyc-7763", "acceleratorGroup": "nvidia-h100", "acceleratable": "true",
			"manufacturer": "nvidia", "product": "NVIDIA H100", "family": "hopper",
			"memory": "80Gi", "cores": "16896", "acceleratorFeature": acceleratorFeatureNote,
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
}

// TestInstanceTypeFlavorHandler_OnGet pins that Get resolves a single flavor by name against the
// same aggregated catalog OnList produces, and returns NotFound for a name no pool derives to.
func TestInstanceTypeFlavorHandler_OnGet(t *testing.T) {
	system.LoopbackKubeClient.Configure(kubefake.NewSimpleClientset())

	objs := []ctrlcli.Object{
		a10gFlavor("gpustack--amd-epyc-7763--nvidia-a10g-linux-amd64-1d", "amd-epyc-7763"),
		flavorWithNotes("gpustack--amd-epyc-7763-linux-amd64-8c", map[string]string{
			"generalGroup": "amd-epyc-7763", "acceleratable": "false", "manufacturer": "amd",
		}),
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	h := &InstanceTypeFlavorHandler{APIReader: cli}

	t.Run("returns the flavor matching the name", func(t *testing.T) {
		obj, err := h.OnGet(context.Background(),
			types.NamespacedName{Name: "gpustack--nvidia-a10g"}, ctrlcli.GetOptions{})
		require.NoError(t, err)
		f, ok := obj.(*worker.InstanceTypeFlavor)
		require.True(t, ok, "OnGet must return an *InstanceTypeFlavor, got %T", obj)
		assert.Equal(t, "gpustack--nvidia-a10g", f.Name)
		assert.Equal(t, "nvidia-a10g", f.Spec.AcceleratorGroup)
	})

	t.Run("returns NotFound for a name no pool derives to", func(t *testing.T) {
		_, err := h.OnGet(context.Background(),
			types.NamespacedName{Name: "gpustack--nvidia-h100"}, ctrlcli.GetOptions{})
		assert.True(t, kerrors.IsNotFound(err), "expected NotFound, got %v", err)
	})
}

// a10gFlavor / h100Flavor build operator-owned accelerated ResourceFlavors. With awareness off the
// generalGroup is dropped from the derived spec, so two a10g flavors on different CPUs collapse to
// one catalog flavor — the dedup the watch state must respect.
func a10gFlavor(name, generalGroup string) *kueue.ResourceFlavor {
	return flavorWithNotes(name, map[string]string{
		"generalGroup": generalGroup, "acceleratorGroup": "nvidia-a10g", "acceleratable": "true",
		"manufacturer": "nvidia", "product": "NVIDIA A10G", "family": "ampere",
		"memory": "24Gi", "cores": "9216", "acceleratorFeature": acceleratorFeatureNote,
	})
}

func h100Flavor(name, generalGroup string) *kueue.ResourceFlavor {
	return flavorWithNotes(name, map[string]string{
		"generalGroup": generalGroup, "acceleratorGroup": "nvidia-h100", "acceleratable": "true",
		"manufacturer": "nvidia", "product": "NVIDIA H100", "family": "hopper",
		"memory": "80Gi", "cores": "16896", "acceleratorFeature": acceleratorFeatureNote,
	})
}

func draining(rf *kueue.ResourceFlavor) *kueue.ResourceFlavor {
	now := meta.Now()
	rf.DeletionTimestamp = &now
	return rf
}

// seedFlavorState primes a watch state with the derived specs of the given ResourceFlavors (awareness
// off), standing in for the current catalog the upstream watch's initial replay would re-announce.
func seedFlavorState(rfs ...*kueue.ResourceFlavor) *instanceTypeFlavorWatchState {
	s := newInstanceTypeFlavorWatchState()
	for _, rf := range rfs {
		if spec, ok := resourceFlavorToSpec(rf, false); ok {
			s.seed(rf.Name, spec)
		}
	}
	return s
}

// TestInstanceTypeFlavorWatchState_Apply pins the many-ResourceFlavor -> one-flavor dedup: a flavor
// is added only on its first backing ResourceFlavor and deleted only when its last is gone, so a
// duplicate backer or a partial delete emits nothing. This is the correctness core the synthetic
// watch depends on, unit-tested apart from the client/proxy plumbing it shares with SettingHandler.
func TestInstanceTypeFlavorWatchState_Apply(t *testing.T) {
	type wantEvt struct {
		typ  watch.EventType
		name string
	}
	cases := []struct {
		name string
		seed []*kueue.ResourceFlavor
		evt  watch.EventType
		rf   *kueue.ResourceFlavor
		want []wantEvt
	}{
		{
			name: "first backing flavor emits Added",
			evt:  watch.Added,
			rf:   a10gFlavor("rf-a", "amd-epyc-7763"),
			want: []wantEvt{{watch.Added, "gpustack--nvidia-a10g"}},
		},
		{
			name: "duplicate backing of a present flavor emits nothing",
			seed: []*kueue.ResourceFlavor{a10gFlavor("rf-a", "amd-epyc-7763")},
			evt:  watch.Added,
			rf:   a10gFlavor("rf-b", "intel-xeon-8358"),
			want: nil,
		},
		{
			name: "deleting one of several backers emits nothing",
			seed: []*kueue.ResourceFlavor{a10gFlavor("rf-a", "amd-epyc-7763"), a10gFlavor("rf-b", "intel-xeon-8358")},
			evt:  watch.Deleted,
			rf:   a10gFlavor("rf-a", "amd-epyc-7763"),
			want: nil,
		},
		{
			name: "deleting the last backer emits Deleted",
			seed: []*kueue.ResourceFlavor{a10gFlavor("rf-a", "amd-epyc-7763")},
			evt:  watch.Deleted,
			rf:   a10gFlavor("rf-a", "amd-epyc-7763"),
			want: []wantEvt{{watch.Deleted, "gpustack--nvidia-a10g"}},
		},
		{
			name: "modifying a backer without a spec change emits nothing",
			seed: []*kueue.ResourceFlavor{a10gFlavor("rf-a", "amd-epyc-7763")},
			evt:  watch.Modified,
			rf:   a10gFlavor("rf-a", "amd-epyc-7763"),
			want: nil,
		},
		{
			// ADDED comes before DELETED so both survive the wrapper's version dedup (they share the
			// backing ResourceFlavor's version).
			name: "a backer that changes flavor releases the old and acquires the new",
			seed: []*kueue.ResourceFlavor{a10gFlavor("rf-a", "amd-epyc-7763")},
			evt:  watch.Modified,
			rf:   h100Flavor("rf-a", "amd-epyc-7763"),
			want: []wantEvt{{watch.Added, "gpustack--nvidia-h100"}, {watch.Deleted, "gpustack--nvidia-a10g"}},
		},
		{
			name: "a draining last backer emits Deleted",
			seed: []*kueue.ResourceFlavor{a10gFlavor("rf-a", "amd-epyc-7763")},
			evt:  watch.Modified,
			rf:   draining(a10gFlavor("rf-a", "amd-epyc-7763")),
			want: []wantEvt{{watch.Deleted, "gpustack--nvidia-a10g"}},
		},
		{
			name: "deleting an untracked backer emits nothing",
			evt:  watch.Deleted,
			rf:   a10gFlavor("ghost", "amd-epyc-7763"),
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := seedFlavorState(c.seed...)
			evts := s.apply(c.evt, c.rf, false)

			var got []wantEvt
			for _, e := range evts {
				f, ok := e.Object.(*worker.InstanceTypeFlavor)
				require.True(t, ok, "event Object must be *InstanceTypeFlavor, got %T", e.Object)
				if e.Type == watch.Deleted {
					assert.NotNil(t, f.DeletionTimestamp,
						"Deleted flavor events must carry a DeletionTimestamp so the wrapper's version dedup passes them")
				}
				got = append(got, wantEvt{e.Type, f.Name})
			}
			assert.Equal(t, c.want, got)
		})
	}
}

// TestResourceFlavorToSpec pins the ok=false gates the watch and list share: a draining flavor and a
// flavor without operator pool identity contribute nothing to the catalog.
func TestResourceFlavorToSpec(t *testing.T) {
	t.Run("a draining flavor contributes nothing", func(t *testing.T) {
		_, ok := resourceFlavorToSpec(draining(a10gFlavor("rf-a", "amd-epyc-7763")), false)
		assert.False(t, ok)
	})

	t.Run("a flavor without a group identity contributes nothing", func(t *testing.T) {
		// Awareness on, no generalGroup note: neither group key resolves, so it is not a pool.
		_, ok := resourceFlavorToSpec(flavorWithNotes("orphan", map[string]string{"manufacturer": "nvidia"}), true)
		assert.False(t, ok)
	})

	t.Run("an operator accelerated flavor contributes its spec", func(t *testing.T) {
		spec, ok := resourceFlavorToSpec(a10gFlavor("rf-a", "amd-epyc-7763"), false)
		require.True(t, ok)
		assert.Equal(t, "nvidia-a10g", spec.AcceleratorGroup)
		assert.Empty(t, spec.GeneralGroup, "awareness off drops the CPU key")
	})
}

// fakeFlavorWatchClient serves the seed list from its embedded reader but hands OnWatch a
// caller-controlled watch.Interface, so the event loop and bookmark passthrough can be driven
// deterministically without the ctrlfake client's limited Watch (per the spec's test scaffolding).
type fakeFlavorWatchClient struct {
	ctrlcli.WithWatch
	fw *watch.FakeWatcher
}

func (c *fakeFlavorWatchClient) Watch(context.Context, ctrlcli.ObjectList, ...ctrlcli.ListOption) (watch.Interface, error) {
	return c.fw, nil
}

// TestInstanceTypeFlavorHandler_OnWatch drives the OnWatch event loop end to end: a ResourceFlavor
// event folds through the dedup multiset into a flavor delta on the returned stream, and a bookmark
// is passed through as a placeholder flavor carrying the backing resource version. This is the
// net-new plumbing the apply()-level test cannot reach (channels, type assertion, bookmark rewrite).
func TestInstanceTypeFlavorHandler_OnWatch(t *testing.T) {
	// ShouldValueBoolFromRemote reads the loopback client; a Secret-less fake resolves awareness to
	// the "false" default instead of panicking.
	system.LoopbackKubeClient.Configure(kubefake.NewSimpleClientset())

	// Empty seed so the first ResourceFlavor event drives a 0->1 transition (a real flavor Added),
	// rather than being suppressed as already-known.
	base := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	fw := watch.NewFake()
	h := &InstanceTypeFlavorHandler{
		APIReader: base,
		Client:    &fakeFlavorWatchClient{WithWatch: base, fw: fw},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dw, err := h.OnWatch(ctx, ctrlcli.ListOptions{})
	require.NoError(t, err)
	defer dw.Stop()

	recv := func() watch.Event {
		t.Helper()
		select {
		case e, ok := <-dw.ResultChan():
			require.True(t, ok, "watch channel closed unexpectedly")
			return e
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a watch event")
			return watch.Event{}
		}
	}

	t.Run("a first backing ResourceFlavor surfaces a flavor Added", func(t *testing.T) {
		fw.Add(a10gFlavor("rf-a", "amd-epyc-7763"))

		e := recv()
		assert.Equal(t, watch.Added, e.Type)
		f, ok := e.Object.(*worker.InstanceTypeFlavor)
		require.True(t, ok, "Object must be *InstanceTypeFlavor, got %T", e.Object)
		assert.Equal(t, "gpustack--nvidia-a10g", f.Name)
	})

	t.Run("a bookmark carries the resource version on a placeholder flavor", func(t *testing.T) {
		rf := a10gFlavor("rf-a", "amd-epyc-7763")
		rf.ResourceVersion = "4242"
		fw.Action(watch.Bookmark, rf)

		e := recv()
		assert.Equal(t, watch.Bookmark, e.Type)
		f, ok := e.Object.(*worker.InstanceTypeFlavor)
		require.True(t, ok, "bookmark Object must be a placeholder *InstanceTypeFlavor, got %T", e.Object)
		assert.Equal(t, "4242", f.ResourceVersion)
	})
}
