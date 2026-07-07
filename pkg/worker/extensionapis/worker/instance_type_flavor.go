package worker

import (
	"context"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/systemmeta"
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
		instanceTypeFlavorColumn("Group", ".spec.group"),
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
// InstanceTypeFlavors. It lists only the flavors carrying the operator's resource-type label,
// then keeps those with a non-empty "group" note (stamped by the NodeFlavorReconciler for
// every pool, generic or accelerated) as a secondary guard. Identical specs collapse to one
// entry, so a model spanning several nodes or os/arch appears once.
func (h *InstanceTypeFlavorHandler) OnList(ctx context.Context, _ ctrlcli.ListOptions) (runtime.Object, error) {
	rfList := new(kueue.ResourceFlavorList)
	if err := h.APIReader.List(ctx, rfList,
		ctrlcli.MatchingLabels{systemmeta.ResourceTypeLabel: _ResourceFlavorResType},
		ctrlcli.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}

	seen := make(map[worker.InstanceTypeFlavorSpec]struct{}, len(rfList.Items))
	list := &worker.InstanceTypeFlavorList{Items: make([]worker.InstanceTypeFlavor, 0, len(rfList.Items))}
	for i := range rfList.Items {
		_, notes := systemmeta.DescribeResource(&rfList.Items[i])
		group := notes["group"]
		if group == "" {
			continue
		}
		spec := worker.InstanceTypeFlavorSpec{
			Group:         group,
			Acceleratable: notes["acceleratable"] == "true",
			Manufacturer:  notes["manufacturer"],
			Product:       notes["product"],
			Family:        notes["family"],
			Memory:        notes["memory"],
			Cores:         notes["cores"],
			Sliceable:     notes["sliceable"] == "true",
		}
		if _, dup := seen[spec]; dup {
			continue
		}
		seen[spec] = struct{}{}
		list.Items = append(list.Items, worker.InstanceTypeFlavor{
			ObjectMeta: meta.ObjectMeta{Name: group},
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
		return lessInstanceTypeFlavorMemory(a.Memory, b.Memory)
	})

	return list, nil
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
