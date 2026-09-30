package worker

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

// ModelStoreBindingHandler proxies a v1alpha1.ModelStoreBinding as the public ModelStoreBinding
// resource.
//
// No status proxy is exposed because the status carries the namespace's consumption against its
// budget, written by the prefetch controller, while the proxy writes with the worker's identity.
// The storage resource's status subresource keeps the status as it is on main-resource writes, so
// the status is read through the main resource.
type ModelStoreBindingHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *ModelStoreBindingHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("modelstorebindings")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Quota", "{.spec.quota.bytes}"),
		extensionapi.JSONPathTemplateColumn("AllowPinned", "{.spec.allowPinned}"),
		extensionapi.JSONPathTemplateColumn("Used", "{.status.usedBytes}"),
		extensionapi.JSONPathTemplateColumn("Phase", "{.status.phase}"))
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.ModelStoreBinding{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelStoreBinding, *worker.ModelStoreBindingList,
		*workercore.ModelStoreBinding, *workercore.ModelStoreBindingList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

func (h *ModelStoreBindingHandler) New() runtime.Object     { return &worker.ModelStoreBinding{} }
func (h *ModelStoreBindingHandler) Destroy()                {}
func (h *ModelStoreBindingHandler) NewList() runtime.Object { return &worker.ModelStoreBindingList{} }
func (h *ModelStoreBindingHandler) NewListForProxy() runtime.Object {
	return &workercore.ModelStoreBindingList{}
}

func (h *ModelStoreBindingHandler) CastObjectTo(
	obj *worker.ModelStoreBinding,
) *workercore.ModelStoreBinding {
	return (*workercore.ModelStoreBinding)(obj)
}

func (h *ModelStoreBindingHandler) CastObjectFrom(
	_ context.Context, obj *workercore.ModelStoreBinding,
) *worker.ModelStoreBinding {
	return (*worker.ModelStoreBinding)(obj)
}
