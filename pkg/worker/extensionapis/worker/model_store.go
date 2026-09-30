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

// ModelStoreHandler proxies a v1alpha1.ModelStore as the public ModelStore resource.
//
// No status proxy is exposed because the status is the controller's observation of the matched
// nodes' residency, while the proxy writes with the worker's identity. The storage resource's
// status subresource keeps the status as it is on main-resource writes, so the status is read
// through the main resource.
type ModelStoreHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *ModelStoreHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("modelstores")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Nodes", "{.status.nodes}"),
		extensionapi.JSONPathTemplateColumn("High", "{.spec.watermarks.highPercent}"),
		extensionapi.JSONPathTemplateColumn("Low", "{.spec.watermarks.lowPercent}"))
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.ModelStore{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelStore, *worker.ModelStoreList,
		*workercore.ModelStore, *workercore.ModelStoreList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

func (h *ModelStoreHandler) New() runtime.Object     { return &worker.ModelStore{} }
func (h *ModelStoreHandler) Destroy()                {}
func (h *ModelStoreHandler) NewList() runtime.Object { return &worker.ModelStoreList{} }
func (h *ModelStoreHandler) NewListForProxy() runtime.Object {
	return &workercore.ModelStoreList{}
}

func (h *ModelStoreHandler) CastObjectTo(obj *worker.ModelStore) *workercore.ModelStore {
	return (*workercore.ModelStore)(obj)
}

func (h *ModelStoreHandler) CastObjectFrom(
	_ context.Context, obj *workercore.ModelStore,
) *worker.ModelStore {
	return (*worker.ModelStore)(obj)
}
