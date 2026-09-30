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

// TopologySourceHandler proxies a v1alpha1.TopologySource as the public TopologySource resource.
//
// No status proxy is exposed because the status is the topology controller's record of the
// inventory, while the proxy writes with the worker's identity. The storage resource's status
// subresource keeps the status as it is on main-resource writes, so the status is read through
// the main resource.
type TopologySourceHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *TopologySourceHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("topologysources")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Kind", "{.status.sourceKind}"),
		extensionapi.JSONPathTemplateColumn("Revision", "{.status.lastSuccessfulRevision}"),
		extensionapi.JSONPathTemplateColumn("Ready", "{.status.conditions[?(@.type=='Ready')].status}"))
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.TopologySource{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.TopologySource, *worker.TopologySourceList,
		*workercore.TopologySource, *workercore.TopologySourceList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

func (h *TopologySourceHandler) New() runtime.Object     { return &worker.TopologySource{} }
func (h *TopologySourceHandler) Destroy()                {}
func (h *TopologySourceHandler) NewList() runtime.Object { return &worker.TopologySourceList{} }
func (h *TopologySourceHandler) NewListForProxy() runtime.Object {
	return &workercore.TopologySourceList{}
}

func (h *TopologySourceHandler) CastObjectTo(obj *worker.TopologySource) *workercore.TopologySource {
	return (*workercore.TopologySource)(obj)
}

func (h *TopologySourceHandler) CastObjectFrom(
	_ context.Context, obj *workercore.TopologySource,
) *worker.TopologySource {
	return (*worker.TopologySource)(obj)
}
