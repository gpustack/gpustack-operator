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

// KVCachePoolBindingHandler proxies a v1alpha1.KVCachePoolBinding as the public KVCachePoolBinding
// resource.
//
// No status proxy is exposed because the status carries the effective quota grant and the
// over-quota evidence, written by the pool's controller, while the proxy writes with the worker's
// identity. The storage resource's status subresource keeps the status as it is on main-resource
// writes, so the status is read through the main resource.
type KVCachePoolBindingHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *KVCachePoolBindingHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("kvcachepoolbindings")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.RenderColumn("Pool", func(obj *worker.KVCachePoolBinding) string {
			return obj.Spec.PoolRef.Name
		}),
		extensionapi.RenderColumn("Domain", func(obj *worker.KVCachePoolBinding) string {
			return obj.Spec.Domain.Name
		}),
		extensionapi.JSONPathTemplateColumn("BlockSize", "{.spec.domain.blockSize}"),
		extensionapi.JSONPathTemplateColumn("Dtype", "{.spec.domain.dtype}"),
		extensionapi.JSONPathTemplateColumn("Effective", "{.status.effectiveQuota}"),
		extensionapi.JSONPathTemplateColumn("Usage", "{.status.usage}"),
		extensionapi.JSONPathTemplateColumn("Phase", "{.status.phase}"))
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.KVCachePoolBinding{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.KVCachePoolBinding, *worker.KVCachePoolBindingList,
		*workercore.KVCachePoolBinding, *workercore.KVCachePoolBindingList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

func (h *KVCachePoolBindingHandler) New() runtime.Object { return &worker.KVCachePoolBinding{} }
func (h *KVCachePoolBindingHandler) Destroy()            {}
func (h *KVCachePoolBindingHandler) NewList() runtime.Object {
	return &worker.KVCachePoolBindingList{}
}

func (h *KVCachePoolBindingHandler) NewListForProxy() runtime.Object {
	return &workercore.KVCachePoolBindingList{}
}

func (h *KVCachePoolBindingHandler) CastObjectTo(
	obj *worker.KVCachePoolBinding,
) *workercore.KVCachePoolBinding {
	return (*workercore.KVCachePoolBinding)(obj)
}

func (h *KVCachePoolBindingHandler) CastObjectFrom(
	_ context.Context, obj *workercore.KVCachePoolBinding,
) *worker.KVCachePoolBinding {
	return (*worker.KVCachePoolBinding)(obj)
}
