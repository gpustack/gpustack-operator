package worker

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

// ModelArtifactHandler exposes the v1 view of a v1alpha1.ModelArtifact and its progress
// subresource.
//
// NO STATUS SUBRESOURCE IS SERVED: the proxy writes with the worker's identity, which the
// ModelArtifact status guard admits, so a writable v1 status would let any caller write what mounts
// are authorized by. An update through the main resource leaves the status as it is, the way the
// resource's status subresource makes the API server treat it.
type ModelArtifactHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *ModelArtifactHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("modelartifacts")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.RenderColumn("Source", modelArtifactSourceColumn),
		extensionapi.RenderColumn("Revision", func(obj *worker.ModelArtifact) string {
			if r := obj.Status.Resolved; r != nil && len(r.Revision) > 12 {
				return r.Revision[:12]
			}
			return ""
		}),
		extensionapi.JSONPathTemplateColumn("Size", "{.status.resolved.sizeBytes}"),
		extensionapi.JSONPathTemplateColumn("Ready", "{.status.nodes.ready}"),
		extensionapi.JSONPathTemplateColumn("Downloading", "{.status.nodes.downloading}"),
		extensionapi.JSONPathTemplateColumn("Resolved", "{.status.conditions[?(@.type=='Resolved')].status}"))
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.ModelArtifact{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelArtifact, *worker.ModelArtifactList,
		*workercore.ModelArtifact, *workercore.ModelArtifactList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{
		"progress": newModelArtifactProgressHandler(h.ObjectInfo, opts),
	}, nil
}

// modelArtifactSourceColumn names where the weights come from: the repository, or the claim.
func modelArtifactSourceColumn(obj *worker.ModelArtifact) string {
	switch s := obj.Spec.Source; {
	case s.HuggingFace != nil:
		return s.HuggingFace.Repository
	case s.ModelScope != nil:
		return s.ModelScope.Repository
	case s.PersistentVolumeClaim != nil:
		return fmt.Sprintf("claim/%s", s.PersistentVolumeClaim.ClaimName)
	}
	return ""
}

func (h *ModelArtifactHandler) New() runtime.Object     { return &worker.ModelArtifact{} }
func (h *ModelArtifactHandler) Destroy()                {}
func (h *ModelArtifactHandler) NewList() runtime.Object { return &worker.ModelArtifactList{} }
func (h *ModelArtifactHandler) NewListForProxy() runtime.Object {
	return &workercore.ModelArtifactList{}
}

func (h *ModelArtifactHandler) CastObjectTo(obj *worker.ModelArtifact) *workercore.ModelArtifact {
	return (*workercore.ModelArtifact)(obj)
}

func (h *ModelArtifactHandler) CastObjectFrom(
	_ context.Context, obj *workercore.ModelArtifact,
) *worker.ModelArtifact {
	return (*worker.ModelArtifact)(obj)
}
