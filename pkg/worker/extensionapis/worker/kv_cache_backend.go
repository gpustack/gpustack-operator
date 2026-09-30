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

// KVCacheBackendHandler proxies a v1alpha1.KVCacheBackend as the public KVCacheBackend resource.
//
// No status proxy is exposed because the status is the backend controller's record of endpoint
// publication and member accounting, while the proxy writes with the worker's identity. The
// storage resource's status subresource keeps the status as it is on main-resource writes, so
// the status is read through the main resource.
type KVCacheBackendHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *KVCacheBackendHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("kvcachebackends")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Type", "{.spec.type}"),
		extensionapi.JSONPathTemplateColumn("Phase", "{.status.phase}"),
		extensionapi.JSONPathTemplateColumn("Endpoint", "{.status.endpoints[?(@.name=='Client')].address}"),
		extensionapi.JSONPathTemplateColumn("Capacity", "{.status.capacity.total}"))
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.KVCacheBackend{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.KVCacheBackend, *worker.KVCacheBackendList,
		*workercore.KVCacheBackend, *workercore.KVCacheBackendList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

func (h *KVCacheBackendHandler) New() runtime.Object     { return &worker.KVCacheBackend{} }
func (h *KVCacheBackendHandler) Destroy()                {}
func (h *KVCacheBackendHandler) NewList() runtime.Object { return &worker.KVCacheBackendList{} }
func (h *KVCacheBackendHandler) NewListForProxy() runtime.Object {
	return &workercore.KVCacheBackendList{}
}

func (h *KVCacheBackendHandler) CastObjectTo(obj *worker.KVCacheBackend) *workercore.KVCacheBackend {
	return (*workercore.KVCacheBackend)(obj)
}

func (h *KVCacheBackendHandler) CastObjectFrom(
	_ context.Context, obj *workercore.KVCacheBackend,
) *worker.KVCacheBackend {
	return (*worker.KVCacheBackend)(obj)
}
