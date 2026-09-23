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

// ModelDeploymentHandler exposes the v1 view of a v1alpha1.ModelDeployment.
type ModelDeploymentHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *ModelDeploymentHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("modeldeployments")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Engine", "{.spec.engine.name}"),
		extensionapi.JSONPathTemplateColumn("Roles", "{.status.roleSummary}"),
		extensionapi.JSONPathTemplateColumn("Phase", "{.status.phase}"))
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.ModelDeployment{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelDeployment, *worker.ModelDeploymentList,
		*workercore.ModelDeployment, *workercore.ModelDeploymentList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{
		"metrics": newModelDeploymentMetricsHandler(h.ObjectInfo, opts),
	}, nil
}

func (h *ModelDeploymentHandler) New() runtime.Object     { return &worker.ModelDeployment{} }
func (h *ModelDeploymentHandler) Destroy()                {}
func (h *ModelDeploymentHandler) NewList() runtime.Object { return &worker.ModelDeploymentList{} }
func (h *ModelDeploymentHandler) NewListForProxy() runtime.Object {
	return &workercore.ModelDeploymentList{}
}

func (h *ModelDeploymentHandler) CastObjectTo(obj *worker.ModelDeployment) *workercore.ModelDeployment {
	return (*workercore.ModelDeployment)(obj)
}

func (h *ModelDeploymentHandler) CastObjectFrom(
	_ context.Context, obj *workercore.ModelDeployment,
) *worker.ModelDeployment {
	return (*worker.ModelDeployment)(obj)
}
