package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

var _ extensionapi.CurdProxyOnCreateBeforeAdvice[*worker.ModelKernelArtifact] = (*ModelKernelArtifactHandler)(nil)

// ModelKernelArtifactHandler exposes the v1 view of a v1alpha1.ModelKernelArtifact.
//
// No writable status subresource is served: the proxy writes with the worker's identity, which the
// v1alpha1 status guard admits, and kernel CSI mounts trust status. Status is read through the
// main resource.
type ModelKernelArtifactHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *ModelKernelArtifactHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("modelkernelartifacts")
	table, err := modelKernelArtifactTable()
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.ModelKernelArtifact{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelKernelArtifact, *worker.ModelKernelArtifactList,
		*workercore.ModelKernelArtifact, *workercore.ModelKernelArtifactList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

func modelKernelArtifactTable() (rest.TableConvertor, error) {
	return extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.RenderColumn("Source", modelKernelArtifactSourceColumn),
		extensionapi.JSONPathTemplateColumn("Engine", "{.spec.compatibility.engine}"),
		extensionapi.JSONPathTemplateColumn("Arch", "{.spec.compatibility.architecture}"),
		extensionapi.JSONPathTemplateColumn("Size", "{.status.resolved.sizeBytes}"),
		extensionapi.JSONPathTemplateColumn("Ready", "{.status.nodes.ready}"),
		extensionapi.JSONPathTemplateColumn("Resolved", "{.status.conditions[?(@.type=='Resolved')].status}"),
		extensionapi.RenderColumn("Quarantined", func(obj *worker.ModelKernelArtifact) string {
			return strconv.FormatBool(obj.Status.Quarantined)
		}),
	)
}

func modelKernelArtifactSourceColumn(obj *worker.ModelKernelArtifact) string {
	switch s := obj.Spec.Source; {
	case s.NodeStore != nil:
		digest := s.NodeStore.ManifestDigest
		if len(digest) > 19 {
			digest = digest[:19]
		}
		return fmt.Sprintf("nodestore/%s", digest)
	case s.HuggingFace != nil:
		return s.HuggingFace.Repository
	case s.Image != nil:
		return s.Image.Reference
	}
	return ""
}

func (h *ModelKernelArtifactHandler) New() runtime.Object { return &worker.ModelKernelArtifact{} }
func (h *ModelKernelArtifactHandler) Destroy()            {}
func (h *ModelKernelArtifactHandler) NewList() runtime.Object {
	return &worker.ModelKernelArtifactList{}
}

func (h *ModelKernelArtifactHandler) NewListForProxy() runtime.Object {
	return &workercore.ModelKernelArtifactList{}
}

func (h *ModelKernelArtifactHandler) CastObjectTo(
	obj *worker.ModelKernelArtifact,
) *workercore.ModelKernelArtifact {
	return (*workercore.ModelKernelArtifact)(obj)
}

func (h *ModelKernelArtifactHandler) CastObjectFrom(
	_ context.Context, obj *workercore.ModelKernelArtifact,
) *worker.ModelKernelArtifact {
	return (*worker.ModelKernelArtifact)(obj)
}

// BeforeOnCreate rejects spec.source.nodeStore on the public v1 proxy before forwarding with the
// worker's own identity, preserving the worker-only creation boundary enforced on v1alpha1.
func (h *ModelKernelArtifactHandler) BeforeOnCreate(
	_ context.Context, obj *worker.ModelKernelArtifact, _ *ctrlcli.CreateOptions,
) error {
	if obj != nil && obj.Spec.Source.NodeStore != nil {
		return kerrors.NewForbidden(
			worker.SchemeGroupVersionResource("modelkernelartifacts").GroupResource(),
			obj.Name,
			errors.New("NodeStoreSourceForbidden: only the operator worker may create a ModelKernelArtifact with spec.source.nodeStore"),
		)
	}
	return nil
}
