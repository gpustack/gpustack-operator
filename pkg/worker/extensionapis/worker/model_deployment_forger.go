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

// ModelDeploymentForgerHandler exposes the v1 view of a v1alpha1.ModelDeploymentForger.
//
// No writable status subresource is served: status carries activeSandbox proof guarded to the
// worker identity on v1alpha1, so v1 exposes status read-only through the main object.
type ModelDeploymentForgerHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *ModelDeploymentForgerHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("modeldeploymentforgers")
	table, err := modelDeploymentForgerTable()
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.ModelDeploymentForger{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelDeploymentForger, *worker.ModelDeploymentForgerList,
		*workercore.ModelDeploymentForger, *workercore.ModelDeploymentForgerList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

func modelDeploymentForgerTable() (rest.TableConvertor, error) {
	return extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Target", "{.spec.targetRef.name}"),
		extensionapi.JSONPathTemplateColumn("Mode", "{.spec.mode}"),
		extensionapi.JSONPathTemplateColumn("Bound", "{.status.conditions[?(@.type=='Bound')].status}"),
		extensionapi.JSONPathTemplateColumn("Active", "{.status.conditions[?(@.type=='Active')].status}"),
		extensionapi.JSONPathTemplateColumn("Blocked", "{.status.conditions[?(@.type=='Blocked')].status}"),
		extensionapi.JSONPathTemplateColumn("Ready", "{.status.conditions[?(@.type=='Ready')].status}"),
	)
}

func (h *ModelDeploymentForgerHandler) New() runtime.Object { return &worker.ModelDeploymentForger{} }
func (h *ModelDeploymentForgerHandler) Destroy()            {}
func (h *ModelDeploymentForgerHandler) NewList() runtime.Object {
	return &worker.ModelDeploymentForgerList{}
}

func (h *ModelDeploymentForgerHandler) NewListForProxy() runtime.Object {
	return &workercore.ModelDeploymentForgerList{}
}

func (h *ModelDeploymentForgerHandler) CastObjectTo(
	obj *worker.ModelDeploymentForger,
) *workercore.ModelDeploymentForger {
	return (*workercore.ModelDeploymentForger)(obj)
}

func (h *ModelDeploymentForgerHandler) CastObjectFrom(
	_ context.Context, obj *workercore.ModelDeploymentForger,
) *worker.ModelDeploymentForger {
	return (*worker.ModelDeploymentForger)(obj)
}
