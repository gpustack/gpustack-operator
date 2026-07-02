package worker

import (
	"context"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

const (
	_InstanceTypeResource = "instancetypes"
)

// InstanceTypeHandler handles v1.InstanceType objects.
//
// InstanceTypeHandler proxies the v1.InstanceType to the v1alpha1.InstanceType CRD.
// The backing ClusterQueue and the three-view status are reconciled by the
// InstanceTypeReconciler; this handler only serves the CRD storage.
type InstanceTypeHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *InstanceTypeHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &workercore.InstanceType{}, "metadata.name",
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			return []string{obj.GetName()}
		})
	if err != nil {
		return schema.GroupVersionResource{}, srs, err
	}

	// Declare GVR.
	gvr = worker.SchemeGroupVersionResource(_InstanceTypeResource)

	// Create table converter to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Entrance",
				Type: "string",
			},
			Template: "{.status.entrance}",
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Accel.(Exclusive)",
				Type: "string",
			},
			Template: "{.status.accelerator.onceMaxRequest}/{.status.accelerator.remaining}",
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Accel.(Shared)",
				Type: "string",
			},
			Template: "{.status.acceleratorShared.onceMaxRequest}/{.status.acceleratorShared.remaining}",
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Accel.(Sliced)",
				Type: "string",
			},
			Template: "{.status.acceleratorSliced.onceMaxRequest}/{.status.acceleratorSliced.remaining}",
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "CPU",
				Type: "string",
			},
			Template: "{.status.cpu.onceMaxRequest}/{.status.cpu.remaining}",
		},
		extensionapi.JSONPathTemplateTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Phase",
				Type: "string",
			},
			Template: "{.status.phase}",
		})
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &worker.InstanceType{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.InstanceType, *worker.InstanceTypeList, *workercore.InstanceType, *workercore.InstanceTypeList,
	](tc, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*InstanceTypeHandler)(nil)
	_ rest.Creater           = (*InstanceTypeHandler)(nil)
	_ rest.Lister            = (*InstanceTypeHandler)(nil)
	_ rest.Watcher           = (*InstanceTypeHandler)(nil)
	_ rest.Getter            = (*InstanceTypeHandler)(nil)
	_ rest.Updater           = (*InstanceTypeHandler)(nil)
	_ rest.Patcher           = (*InstanceTypeHandler)(nil)
	_ rest.GracefulDeleter   = (*InstanceTypeHandler)(nil)
	_ rest.CollectionDeleter = (*InstanceTypeHandler)(nil)
)

func (h *InstanceTypeHandler) New() runtime.Object {
	return &worker.InstanceType{}
}

func (h *InstanceTypeHandler) Destroy() {}

func (h *InstanceTypeHandler) NewList() runtime.Object {
	return &worker.InstanceTypeList{}
}

func (h *InstanceTypeHandler) NewListForProxy() runtime.Object {
	return &workercore.InstanceTypeList{}
}

func (h *InstanceTypeHandler) CastObjectTo(do *worker.InstanceType) (uo *workercore.InstanceType) {
	return (*workercore.InstanceType)(do)
}

func (h *InstanceTypeHandler) CastObjectFrom(_ context.Context, uo *workercore.InstanceType) (do *worker.InstanceType) {
	return (*worker.InstanceType)(uo)
}
