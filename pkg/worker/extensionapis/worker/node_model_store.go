package worker

import (
	"context"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

// NodeModelStoreHandler exposes the v1 view of a v1alpha1.NodeModelStore: get, list, watch and
// delete, never create or update. Its writers, the worker and the node's plugin, write the v1alpha1
// resource, whose status guard judges each by its own identity; a writable proxy would write with
// the worker's.
//
// DELETE IS SERVED FOR THE GARBAGE COLLECTOR. It watches each resource at the group's preferred
// version, v1 here, and only resources that version can delete: without delete no NodeModelStore
// is collected with its Node (measured on Kubernetes 1.36). A deleted object is created again by
// the worker while its node runs the plugin.
type NodeModelStoreHandler struct {
	extensionapi.ObjectInfo
	extensionapi.ListWatchOperation
	extensionapi.GetOperation
	extensionapi.DeleteOperation
}

func (h *NodeModelStoreHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("nodemodelstores")
	table, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Ready", "{.status.conditions[?(@.type=='Ready')].status}"),
		extensionapi.JSONPathTemplateColumn("Used", "{.status.capacity.usedPercent}"),
		extensionapi.RenderColumn("Models", func(obj *worker.NodeModelStore) string {
			return strconv.Itoa(len(obj.Status.Models))
		}),
		extensionapi.RenderColumn("Downloading", func(obj *worker.NodeModelStore) string {
			n := 0
			for _, m := range obj.Status.Models {
				if m.State == workercore.NodeModelStoreModelStateDownloading {
					n++
				}
			}
			return strconv.Itoa(n)
		}))
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.NodeModelStore{}
	// The proxy serves every verb; only its reads and deletes are exposed.
	ops := extensionapi.WithCurdProxy[
		*worker.NodeModelStore, *worker.NodeModelStoreList,
		*workercore.NodeModelStore, *workercore.NodeModelStoreList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	h.ListWatchOperation, h.GetOperation = ops.ListWatchOperation, ops.GetOperation
	h.DeleteOperation = ops.DeleteOperation

	return gvr, nil, nil
}

func (h *NodeModelStoreHandler) New() runtime.Object     { return &worker.NodeModelStore{} }
func (h *NodeModelStoreHandler) Destroy()                {}
func (h *NodeModelStoreHandler) NewList() runtime.Object { return &worker.NodeModelStoreList{} }
func (h *NodeModelStoreHandler) NewListForProxy() runtime.Object {
	return &workercore.NodeModelStoreList{}
}

func (h *NodeModelStoreHandler) CastObjectTo(obj *worker.NodeModelStore) *workercore.NodeModelStore {
	return (*workercore.NodeModelStore)(obj)
}

func (h *NodeModelStoreHandler) CastObjectFrom(
	_ context.Context, obj *workercore.NodeModelStore,
) *worker.NodeModelStore {
	return (*worker.NodeModelStore)(obj)
}
