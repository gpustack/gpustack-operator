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

// ModelPrefetchHandler exposes the v1 view of a v1alpha1.ModelPrefetch.
//
// It proxies the v1alpha1 resource for every verb and serves no subresource: the per-node facts the
// status aggregates already live on the nodes' own reports, so a progress-style extra path would
// name nothing the aggregates need.
type ModelPrefetchHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *ModelPrefetchHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("modelprefetches")
	table, err := modelPrefetchTable()
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.ModelPrefetch{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelPrefetch, *worker.ModelPrefetchList,
		*workercore.ModelPrefetch, *workercore.ModelPrefetchList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

// modelPrefetchTable is the view's kubectl table: who is warming what, and how far it is.
func modelPrefetchTable() (rest.TableConvertor, error) {
	return extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.RenderColumn("Artifact", func(obj *worker.ModelPrefetch) string {
			return obj.Spec.ArtifactRef.Name
		}),
		extensionapi.RenderColumn("Binding", func(obj *worker.ModelPrefetch) string {
			return obj.Spec.BindingRef.Name
		}),
		extensionapi.JSONPathTemplateColumn("Ready", "{.status.readyNodes}"),
		extensionapi.JSONPathTemplateColumn("Desired", "{.status.desiredNodes}"),
		extensionapi.JSONPathTemplateColumn("Available", "{.status.conditions[?(@.type=='Available')].status}"))
}

func (h *ModelPrefetchHandler) New() runtime.Object     { return &worker.ModelPrefetch{} }
func (h *ModelPrefetchHandler) Destroy()                {}
func (h *ModelPrefetchHandler) NewList() runtime.Object { return &worker.ModelPrefetchList{} }
func (h *ModelPrefetchHandler) NewListForProxy() runtime.Object {
	return &workercore.ModelPrefetchList{}
}

func (h *ModelPrefetchHandler) CastObjectTo(obj *worker.ModelPrefetch) *workercore.ModelPrefetch {
	return (*workercore.ModelPrefetch)(obj)
}

func (h *ModelPrefetchHandler) CastObjectFrom(
	_ context.Context, obj *workercore.ModelPrefetch,
) *worker.ModelPrefetch {
	return (*worker.ModelPrefetch)(obj)
}
