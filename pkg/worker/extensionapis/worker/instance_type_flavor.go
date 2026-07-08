package worker

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	_InstanceTypeFlavorResource = "instancetypeflavors"

	// _ResourceFlavorResType is the systemmeta resource type of the operator-owned Kueue
	// ResourceFlavors this handler aggregates (stamped by the NodeFlavorReconciler).
	_ResourceFlavorResType = "nodes"
)

// InstanceTypeFlavorHandler serves v1.InstanceTypeFlavor objects list-only.
//
// It has no backing CRD: OnList aggregates the operator-owned Kueue ResourceFlavors,
// parsing each pool's note.gpustack.ai/* annotations into one InstanceTypeFlavor,
// deduplicating identical entries and sorting by manufacturer, product, then memory.
type InstanceTypeFlavorHandler struct {
	extensionapi.ObjectInfo
	extensionapi.ListOperation

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
		instanceTypeFlavorColumn("Sliceable", ".spec.sliceable"),
	)
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &worker.InstanceTypeFlavor{}
	h.ListOperation = extensionapi.WithList(tc, h)
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

	aware := settings.InstanceTypeAwareCPUManufacturer.ShouldValueFromRemote(ctx) == "true"

	visited := sets.New[worker.InstanceTypeFlavorSpec]()
	list := &worker.InstanceTypeFlavorList{Items: make([]worker.InstanceTypeFlavor, 0, len(rfList.Items))}
	for i := range rfList.Items {
		// Skip a flavor Kueue is finalizing (DeletionTimestamp set): its pool is draining away,
		// so the catalog must not advertise a pool being torn down.
		if rfList.Items[i].DeletionTimestamp != nil {
			continue
		}
		_, notes := systemmeta.DescribeResource(&rfList.Items[i])
		spec := instanceTypeFlavorSpec(notes, aware)
		if spec.GeneralGroup == "" && spec.AcceleratorGroup == "" {
			continue // not an operator pool flavor (no group identity)
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

// instanceTypeFlavorSpec builds one catalog row from a ResourceFlavor's notes, grouped by the
// awareness setting. A non-accelerated flavor becomes a per-CPU row (aware) or the single
// CPU-agnostic "generic" row (unaware); an accelerated flavor keeps its device descriptors (which
// are identical across CPU variants, so they deduplicate) and carries the CPU key only when aware.
func instanceTypeFlavorSpec(notes map[string]string, aware bool) worker.InstanceTypeFlavorSpec {
	if notes["acceleratable"] != "true" {
		if aware {
			return worker.InstanceTypeFlavorSpec{
				GeneralGroup: notes["generalGroup"],
				Manufacturer: notes["manufacturer"],
				Product:      notes["product"],
				Family:       notes["family"],
			}
		}
		return worker.InstanceTypeFlavorSpec{
			GeneralGroup: nodefeature.GeneralGroupGeneric,
			Manufacturer: nodefeature.GeneralManufacturerGeneric,
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
		Sliceable:        notes["sliceable"] == "true",
	}
	if aware {
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
