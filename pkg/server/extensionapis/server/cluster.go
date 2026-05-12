package server

import (
	"context"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

const _ClusterResource = "clusters"

// ClusterHandler handles v1.Cluster objects.
//
// ClusterHandler proxies the server v1alpha1.Cluster objects.
type ClusterHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *ClusterHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &servercore.Cluster{}, "metadata.name",
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			return []string{obj.GetName()}
		})
	if err != nil {
		return schema.GroupVersionResource{}, nil, err
	}

	// Declare GVR.
	gvr = server.SchemeGroupVersionResource(_ClusterResource)

	// Create table convertor to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTableConvertor(
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Type",
				Type: "string",
			},
			JSONPath: ".spec.type",
		},
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Phase",
				Type: "string",
			},
			JSONPath: ".status.phase",
		})
	if err != nil {
		return gvr, nil, err
	}

	// As storage.
	h.ObjectInfo = &server.Cluster{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*server.Cluster, *server.ClusterList, *servercore.Cluster, *servercore.ClusterList,
	](tc, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())

	// Create subresource handlers.
	srs = map[string]rest.Storage{
		"config":       newClusterConfigHandler(h.ObjectInfo, opts),
		"importconfig": newClusterImportConfigHandler(h.ObjectInfo, opts),
	}

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*ClusterHandler)(nil)
	_ rest.Creater           = (*ClusterHandler)(nil)
	_ rest.Lister            = (*ClusterHandler)(nil)
	_ rest.Watcher           = (*ClusterHandler)(nil)
	_ rest.Getter            = (*ClusterHandler)(nil)
	_ rest.Updater           = (*ClusterHandler)(nil)
	_ rest.Patcher           = (*ClusterHandler)(nil)
	_ rest.GracefulDeleter   = (*ClusterHandler)(nil)
	_ rest.CollectionDeleter = (*ClusterHandler)(nil)
)

func (h *ClusterHandler) New() runtime.Object {
	return &server.Cluster{}
}

func (h *ClusterHandler) Destroy() {
}

func (h *ClusterHandler) NewList() runtime.Object {
	return &server.ClusterList{}
}

func (h *ClusterHandler) NewListForProxy() runtime.Object {
	return &servercore.ClusterList{}
}

func (h *ClusterHandler) CastObjectTo(do *server.Cluster) (uo *servercore.Cluster) {
	return (*servercore.Cluster)(do)
}

func (h *ClusterHandler) CastObjectFrom(uo *servercore.Cluster) (do *server.Cluster) {
	return (*server.Cluster)(uo)
}
