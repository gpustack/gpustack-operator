package worker

import (
	"context"
	"fmt"
	"sort"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	_InstanceTypeFlavorResource = "instancetypeflavors"

	// _ResourceFlavorResType is the systemmeta resource type of the operator-owned Kueue
	// ResourceFlavors this handler aggregates (stamped by the NodeFlavorReconciler).
	_ResourceFlavorResType = "nodes"
)

// InstanceTypeFlavorHandler serves v1.InstanceTypeFlavor objects read-only (get, list and watch).
//
// It has no backing CRD: OnList aggregates the operator-owned Kueue ResourceFlavors,
// parsing each pool's note.gpustack.ai/* annotations into one InstanceTypeFlavor,
// deduplicating identical entries and sorting by manufacturer, product, then memory. OnWatch
// projects the ResourceFlavor watch onto the same deduplicated catalog, emitting a flavor ADDED
// only when its first backing ResourceFlavor appears and a DELETED only when the last is gone.
// OnGet resolves a single flavor by name against the same aggregated OnList output.
type InstanceTypeFlavorHandler struct {
	extensionapi.ObjectInfo
	extensionapi.ListWatchOperation
	extensionapi.GetOperation

	Client    ctrlcli.WithWatch
	APIReader ctrlcli.Reader
}

func (h *InstanceTypeFlavorHandler) SetupHandler(
	_ context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Declare GVR.
	gvr = worker.SchemeGroupVersionResource(_InstanceTypeFlavorResource)

	// Create table convertor to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTableConvertor(
		instanceTypeFlavorColumn("GeneralGroup", ".spec.generalGroup"),
		instanceTypeFlavorColumn("AcceleratorGroup", ".spec.acceleratorGroup"),
		instanceTypeFlavorColumn("Acceleratable", ".spec.acceleratable"),
		instanceTypeFlavorColumn("Manufacturer", ".spec.manufacturer"),
		instanceTypeFlavorColumn("Product", ".spec.product"),
		instanceTypeFlavorColumn("Memory", ".spec.memory"),
		instanceTypeFlavorColumn("Cores", ".spec.cores"),
	)
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &worker.InstanceTypeFlavor{}
	h.ListWatchOperation = extensionapi.WithListWatch(tc, h)
	h.GetOperation = extensionapi.WithGet(h)
	h.Client = opts.Manager.GetClient().(ctrlcli.WithWatch)
	h.APIReader = opts.Manager.GetAPIReader()

	return gvr, srs, err
}

func instanceTypeFlavorColumn(name, jsonPath string) extensionapi.JSONPathTableColumnDefinition {
	return extensionapi.JSONPathTableColumnDefinition{
		TableColumnDefinition: meta.TableColumnDefinition{Name: name, Type: "string"},
		JSONPath:              jsonPath,
	}
}

var (
	_ rest.Storage = (*InstanceTypeFlavorHandler)(nil)
	_ rest.Lister  = (*InstanceTypeFlavorHandler)(nil)
	_ rest.Watcher = (*InstanceTypeFlavorHandler)(nil)
	_ rest.Getter  = (*InstanceTypeFlavorHandler)(nil)
)

func (h *InstanceTypeFlavorHandler) New() runtime.Object {
	return &worker.InstanceTypeFlavor{}
}

func (h *InstanceTypeFlavorHandler) Destroy() {}

func (h *InstanceTypeFlavorHandler) NewList() runtime.Object {
	return &worker.InstanceTypeFlavorList{}
}

// OnList aggregates the operator-owned ResourceFlavors into deduplicated, sorted
// InstanceTypeFlavors, grouped the same way the derived ClusterQueue/InstanceType are — governed
// by instance-type-aware-cpu-manufacturer (read from the remote; the aggregated apiserver has no
// local settings indexer). With awareness off a generic pool collapses to one "generic" row and an
// accelerated pool to one row per accelerator (CPU ignored); with awareness on both split by the
// CPU key. Identical specs collapse to one entry, so a model spanning several nodes or os/arch
// appears once.
func (h *InstanceTypeFlavorHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	// List.
	rfList := new(kueue.ResourceFlavorList)
	err := h.APIReader.List(ctx, rfList,
		convertResourceFlavorListOptsFromInstanceTypeFlavorListOpts(opts),
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		return nil, err
	}

	// Read the awareness setting as a bool (matching the reconcilers' ShouldValueBool), not a literal
	// "true": the setting's AllowBool admission also accepts "1"/"TRUE"/etc., and comparing to "true"
	// alone would leave the catalog collapsed while the reconciled ClusterQueues/InstanceTypes split.
	cpuAware := settings.InstanceTypeAwareCPUManufacturer.ShouldValueBoolFromRemote(ctx)

	visited := sets.New[worker.InstanceTypeFlavorSpec]()
	list := &worker.InstanceTypeFlavorList{Items: make([]worker.InstanceTypeFlavor, 0, len(rfList.Items))}
	for i := range rfList.Items {
		spec, ok := resourceFlavorToSpec(&rfList.Items[i], cpuAware)
		if !ok {
			continue
		}
		if visited.Has(spec) {
			continue
		}
		visited.Insert(spec)
		list.Items = append(list.Items, worker.InstanceTypeFlavor{
			ObjectMeta: meta.ObjectMeta{Name: instanceTypeFlavorName(spec)},
			Spec:       spec,
		})
	}

	sort.Slice(list.Items, func(i, j int) bool {
		a, b := &list.Items[i].Spec, &list.Items[j].Spec
		if a.Manufacturer != b.Manufacturer {
			return a.Manufacturer < b.Manufacturer
		}
		if a.Product != b.Product {
			return a.Product < b.Product
		}
		if a.Memory != b.Memory {
			return lessInstanceTypeFlavorMemory(a.Memory, b.Memory)
		}
		// Stable tiebreak by group identity, so per-CPU rows (aware) order deterministically.
		if a.GeneralGroup != b.GeneralGroup {
			return a.GeneralGroup < b.GeneralGroup
		}
		return a.AcceleratorGroup < b.AcceleratorGroup
	})

	return list, nil
}

// OnGet resolves a single flavor by name against the same aggregated catalog OnList produces. The
// catalog is derived and has no per-object store, so Get lists, then filters by name. A flavor name
// is derived from group identity alone and is not guaranteed unique, so the first match in OnList's
// deterministic (manufacturer, product, memory) order is returned; NotFound when no pool matches.
func (h *InstanceTypeFlavorHandler) OnGet(ctx context.Context, key types.NamespacedName, _ ctrlcli.GetOptions) (runtime.Object, error) {
	listObj, err := h.OnList(ctx, ctrlcli.ListOptions{})
	if err != nil {
		return nil, err
	}

	list := listObj.(*worker.InstanceTypeFlavorList)
	for i := range list.Items {
		if list.Items[i].Name == key.Name {
			return &list.Items[i], nil
		}
	}
	return nil, kerrors.NewNotFound(worker.Resource(_InstanceTypeFlavorResource), key.Name)
}

// OnWatch projects the operator-owned ResourceFlavor watch onto the deduplicated InstanceTypeFlavor
// catalog. Because the projection is many-ResourceFlavor -> one-flavor, it cannot map events 1:1: a
// flavor is added only when its first backing ResourceFlavor appears and deleted only when its last
// backing ResourceFlavor is gone. It seeds a backing-count multiset from the current flavors so the
// upstream watch's initial replay is suppressed (the client already has that state from the list),
// then folds each ResourceFlavor event into the multiset and emits the resulting flavor deltas.
//
// It mirrors SettingHandler.OnWatch: the awareness setting is read once at watch start (a client
// reconnect re-derives if it later changes), and the outer ListWatchOperation.Watch wrapper handles
// the resource-version dedup and bookmark passthrough, so every emitted flavor carries the backing
// ResourceFlavor's resource version.
func (h *InstanceTypeFlavorHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	// Read the awareness setting once at watch start (matching OnList's read).
	cpuAware := settings.InstanceTypeAwareCPUManufacturer.ShouldValueBoolFromRemote(ctx)

	// Seed the backing-count multiset from the current ResourceFlavors so the upstream watch's
	// initial replay (an ADDED per existing flavor) is suppressed as already-known.
	rfList := new(kueue.ResourceFlavorList)
	err := h.APIReader.List(ctx, rfList,
		convertResourceFlavorListOptsFromInstanceTypeFlavorListOpts(opts),
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		return nil, err
	}
	state := newInstanceTypeFlavorWatchState()
	for i := range rfList.Items {
		if spec, ok := resourceFlavorToSpec(&rfList.Items[i], cpuAware); ok {
			state.seed(rfList.Items[i].Name, spec)
		}
	}

	// Watch the operator-owned ResourceFlavors (same scoping the list uses).
	uw, err := h.Client.Watch(ctx, new(kueue.ResourceFlavorList),
		convertResourceFlavorListOptsFromInstanceTypeFlavorListOpts(opts))
	if err != nil {
		return nil, err
	}

	c := make(chan watch.Event)
	dw := watch.NewProxyWatcher(c)
	gox.Go(func() {
		defer close(c)
		defer uw.Stop()

		for {
			select {
			case <-ctx.Done():
				// Cancel by context.
				return
			case <-dw.StopChan():
				// Stop by downstream.
				return
			case e, ok := <-uw.ResultChan():
				if !ok {
					// Close by upstream.
					return
				}

				// Nothing to do.
				if e.Object == nil {
					c <- e
					continue
				}

				// Type assert.
				rf, ok := e.Object.(*kueue.ResourceFlavor)
				if !ok {
					c <- e
					continue
				}

				// Process bookmark: carry the resource version through on a placeholder flavor.
				if e.Type == watch.Bookmark {
					e.Object = &worker.InstanceTypeFlavor{
						ObjectMeta: meta.ObjectMeta{ResourceVersion: rf.ResourceVersion},
					}
					c <- e
					continue
				}

				// Fold the ResourceFlavor event into the dedup multiset and dispatch the deltas.
				for _, fe := range state.apply(e.Type, rf, cpuAware) {
					c <- fe
				}
			}
		}
	})

	return dw, nil
}

// instanceTypeFlavorWatchState tracks which ResourceFlavors currently back each catalog flavor spec,
// so the many-ResourceFlavor -> one-flavor projection emits a flavor ADDED only when its first
// backing ResourceFlavor appears and a DELETED only when its last backing ResourceFlavor is gone.
// It is owned by a single OnWatch goroutine and needs no locking.
type instanceTypeFlavorWatchState struct {
	rfSpec   map[string]worker.InstanceTypeFlavorSpec // ResourceFlavor name -> the spec it backs
	specRefs map[worker.InstanceTypeFlavorSpec]int    // flavor spec -> number of backing ResourceFlavors
}

func newInstanceTypeFlavorWatchState() *instanceTypeFlavorWatchState {
	return &instanceTypeFlavorWatchState{
		rfSpec:   make(map[string]worker.InstanceTypeFlavorSpec),
		specRefs: make(map[worker.InstanceTypeFlavorSpec]int),
	}
}

// seed records a backing without emitting an event, so the upstream watch's initial replay of the
// already-listed flavors surfaces no duplicate ADDED.
func (s *instanceTypeFlavorWatchState) seed(rfName string, spec worker.InstanceTypeFlavorSpec) {
	s.rfSpec[rfName] = spec
	s.specRefs[spec]++
}

// apply folds one ResourceFlavor event into the multiset and returns the flavor events it triggers.
// An ADDED fires when a spec gains its first backer; a DELETED fires when it loses its last. A
// backing whose derived spec is unchanged produces nothing; one that changes spec releases the old
// (possibly DELETED) and acquires the new (possibly ADDED).
//
// When a single ResourceFlavor event yields both events (a spec-change move), they share the backing
// ResourceFlavor's resource version, so the outer ListWatchOperation.Watch wrapper's version dedup
// would drop the second. Two things keep both: the ADDED is emitted first so it wins the version
// swap, and every DELETED carries a DeletionTimestamp (via flavorWatchEvent), which the wrapper lets
// through regardless of the version. Emitted flavors carry the ResourceFlavor's resource version.
func (s *instanceTypeFlavorWatchState) apply(evtType watch.EventType, rf *kueue.ResourceFlavor, cpuAware bool) []watch.Event {
	newSpec, present := resourceFlavorToSpec(rf, cpuAware)
	oldSpec, tracked := s.rfSpec[rf.Name]

	var added, deleted []watch.Event
	release := func() {
		if !tracked {
			return
		}
		delete(s.rfSpec, rf.Name)
		s.specRefs[oldSpec]--
		if s.specRefs[oldSpec] <= 0 {
			delete(s.specRefs, oldSpec)
			deleted = append(deleted, flavorWatchEvent(watch.Deleted, oldSpec, rf.ResourceVersion))
		}
	}
	acquire := func() {
		s.rfSpec[rf.Name] = newSpec
		s.specRefs[newSpec]++
		if s.specRefs[newSpec] == 1 {
			added = append(added, flavorWatchEvent(watch.Added, newSpec, rf.ResourceVersion))
		}
	}

	switch evtType {
	case watch.Deleted:
		release()
	case watch.Added, watch.Modified:
		switch {
		case !present:
			// The ResourceFlavor is draining or lost its pool identity: drop its backing.
			release()
		case tracked && oldSpec == newSpec:
			// The backing is unchanged: the dedup set is untouched.
		default:
			release() // decrement the old spec first so its entry is gone before the new one lands
			acquire() // increment the new spec (may emit ADDED)
		}
	}

	// ADDED before DELETED so both survive the wrapper's version dedup on a move (see the doc above).
	return append(added, deleted...)
}

// flavorWatchEvent builds a watch event for one catalog flavor, stamping the backing ResourceFlavor's
// resource version so the outer ListWatchOperation.Watch wrapper's version dedup passes it through. A
// DELETED also carries a DeletionTimestamp so the wrapper passes it even when it shares a resource
// version with a preceding event (a spec-change move emits both under one version).
func flavorWatchEvent(t watch.EventType, spec worker.InstanceTypeFlavorSpec, rv string) watch.Event {
	obj := &worker.InstanceTypeFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name:            instanceTypeFlavorName(spec),
			ResourceVersion: rv,
		},
		Spec: spec,
	}
	if t == watch.Deleted {
		now := meta.Now()
		obj.DeletionTimestamp = &now
	}
	return watch.Event{Type: t, Object: obj}
}

// resourceFlavorToSpec derives the catalog spec a ResourceFlavor contributes, or ok=false when the
// flavor is draining (DeletionTimestamp set, so the catalog must not advertise a pool being torn
// down) or carries no operator pool identity.
func resourceFlavorToSpec(rf *kueue.ResourceFlavor, cpuAware bool) (worker.InstanceTypeFlavorSpec, bool) {
	if rf.DeletionTimestamp != nil {
		return worker.InstanceTypeFlavorSpec{}, false
	}
	_, notes := systemmeta.DescribeResource(rf)
	spec := instanceTypeFlavorSpec(notes, cpuAware)
	if spec.GeneralGroup == "" && spec.AcceleratorGroup == "" {
		return worker.InstanceTypeFlavorSpec{}, false
	}
	return spec, true
}

// instanceTypeFlavorSpec builds one catalog row from a ResourceFlavor's notes, grouped by the
// awareness setting. A non-accelerated flavor becomes a per-CPU row (aware) or the single
// CPU-agnostic "generic" row (unaware); an accelerated flavor keeps its device descriptors (which
// are identical across CPU variants, so they deduplicate) and carries the CPU key only when aware.
func instanceTypeFlavorSpec(notes map[string]string, cpuAware bool) worker.InstanceTypeFlavorSpec {
	if notes["acceleratable"] != "true" {
		if cpuAware {
			return worker.InstanceTypeFlavorSpec{
				GeneralGroup: notes["generalGroup"],
				Manufacturer: notes["manufacturer"],
				Product:      notes["product"],
				Family:       notes["family"],
			}
		}
		return worker.InstanceTypeFlavorSpec{
			GeneralGroup: nodefeature.GeneralGroupGeneric,
		}
	}
	spec := worker.InstanceTypeFlavorSpec{
		AcceleratorGroup: notes["acceleratorGroup"],
		Acceleratable:    true,
		Manufacturer:     notes["manufacturer"],
		Product:          notes["product"],
		Family:           notes["family"],
		Memory:           notes["memory"],
		Cores:            notes["cores"],
	}
	if cpuAware {
		spec.GeneralGroup = notes["generalGroup"]
	}
	return spec
}

// instanceTypeFlavorName names a catalog row from its group identity, matching the derived
// InstanceType names: gpustack--${aKey} / gpustack--${gKey}--${aKey} (accelerated, unaware/aware)
// and gpustack--generic / gpustack--${gKey} (generic, unaware/aware).
func instanceTypeFlavorName(spec worker.InstanceTypeFlavorSpec) string {
	if spec.Acceleratable {
		if spec.GeneralGroup != "" {
			return fmt.Sprintf("gpustack--%s--%s", spec.GeneralGroup, spec.AcceleratorGroup)
		}
		return fmt.Sprintf("gpustack--%s", spec.AcceleratorGroup)
	}
	return fmt.Sprintf("gpustack--%s", spec.GeneralGroup)
}

// lessInstanceTypeFlavorMemory orders two VRAM notes numerically when both parse as
// quantities (so "8Gi" < "16Gi"), falling back to a lexical compare otherwise (e.g. an
// empty generic memory).
func lessInstanceTypeFlavorMemory(a, b string) bool {
	qa, ea := resource.ParseQuantity(a)
	qb, eb := resource.ParseQuantity(b)
	if ea == nil && eb == nil {
		return qa.Cmp(qb) < 0
	}
	return a < b
}

func convertResourceFlavorListOptsFromInstanceTypeFlavorListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_ResourceFlavorResType); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}
