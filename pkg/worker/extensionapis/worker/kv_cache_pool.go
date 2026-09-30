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

// KVCachePoolHandler proxies a v1alpha1.KVCachePool as the public KVCachePool resource.
//
// No status proxy is exposed because the status carries the granted quota, the usage and the
// domain registry the pool's finalizer enforces on, all written by the pool's controller, while
// the proxy writes with the worker's identity. The storage resource's status subresource keeps
// the status as it is on main-resource writes, so the status is read through the main resource.
type KVCachePoolHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *KVCachePoolHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("kvcachepools")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Quota", "{.spec.quota.total}"),
		extensionapi.JSONPathTemplateColumn("Usage", "{.status.usage.total}"),
		extensionapi.JSONPathTemplateColumn("Phase", "{.status.phase}"),
		extensionapi.JSONPathTemplateColumn("Endpoint", "{.status.clientEndpoint}"))
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.KVCachePool{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.KVCachePool, *worker.KVCachePoolList,
		*workercore.KVCachePool, *workercore.KVCachePoolList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

func (h *KVCachePoolHandler) New() runtime.Object     { return &worker.KVCachePool{} }
func (h *KVCachePoolHandler) Destroy()                {}
func (h *KVCachePoolHandler) NewList() runtime.Object { return &worker.KVCachePoolList{} }
func (h *KVCachePoolHandler) NewListForProxy() runtime.Object {
	return &workercore.KVCachePoolList{}
}

func (h *KVCachePoolHandler) CastObjectTo(obj *worker.KVCachePool) *workercore.KVCachePool {
	return (*workercore.KVCachePool)(obj)
}

func (h *KVCachePoolHandler) CastObjectFrom(
	_ context.Context, obj *workercore.KVCachePool,
) *worker.KVCachePool {
	return (*worker.KVCachePool)(obj)
}
