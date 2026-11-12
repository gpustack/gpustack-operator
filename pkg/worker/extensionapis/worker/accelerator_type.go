package worker

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
)

const (
	_AcceleratorTypeResource = "acceleratortypes"
)

// AcceleratorTypeHandler handles v1.AcceleratorType objects.
//
// AcceleratorTypeHandler proxies the v1.AcceleratorType to the v1alpha1.AcceleratorType CRD.
// The backing ClusterQueue and the four-view status are reconciled by the
// AcceleratorTypeReconciler; this handler only serves the CRD storage.
type AcceleratorTypeHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *AcceleratorTypeHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &workercore.AcceleratorType{}, "metadata.name",
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
	gvr = worker.SchemeGroupVersionResource(_AcceleratorTypeResource)

	// Create table converter to pretty the kubectl's output.
	tc, err := acceleratorTypeTable()
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &worker.AcceleratorType{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.AcceleratorType, *worker.AcceleratorTypeList, *workercore.AcceleratorType, *workercore.AcceleratorTypeList,
	](tc, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())

	return gvr, srs, err
}

func acceleratorTypeTable() (rest.TableConvertor, error) {
	// The four-view column can only show the partition view as a single scalar, which is a best
	// case over profiles competing for the same physical slices. This column is the per-profile
	// menu behind it — which profiles the pool can still host, a profile it offers but cannot
	// currently build shown at zero. Priority 1 keeps it to `-o wide`: it is long, and empty for
	// every pool with no partitioned card.
	partitions := extensionapi.RenderColumn("Partitions", func(obj *worker.AcceleratorType) string {
		profiles := obj.Status.AcceleratorPartitioned.RemainingProfiles
		if len(profiles) == 0 {
			// A pool with no partitioned card offers no menu. Render the same placeholder the
			// jsonpath columns use for a missing value, so the wide table stays uniform.
			return "-"
		}
		parts := make([]string, 0, len(profiles))
		for i := range profiles {
			parts = append(parts, fmt.Sprintf("%s:%d", profiles[i].Name, profiles[i].Count))
		}
		return strings.Join(parts, " ")
	})
	partitions.Priority = 1

	return extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Entrance", "{.status.entrance}"),
		// Fold the exclusive/shared/sliced/partitioned four-view into one column: four
		// space-separated onceMaxRequest/remaining groups, in the order named
		// by the column header.
		extensionapi.JSONPathTemplateColumn("Accelerator(EX/SH/SL/PT)",
			"{.status.accelerator.onceMaxRequest}/{.status.accelerator.remaining} "+
				"{.status.acceleratorShared.onceMaxRequest}/{.status.acceleratorShared.remaining} "+
				"{.status.acceleratorSliced.onceMaxRequest}/{.status.acceleratorSliced.remaining} "+
				"{.status.acceleratorPartitioned.onceMaxRequest}/{.status.acceleratorPartitioned.remaining}"),
		partitions,
		extensionapi.JSONPathTemplateColumn("Phase", "{.status.phase}"),
	)
}

var (
	_ rest.Storage           = (*AcceleratorTypeHandler)(nil)
	_ rest.Creater           = (*AcceleratorTypeHandler)(nil)
	_ rest.Lister            = (*AcceleratorTypeHandler)(nil)
	_ rest.Watcher           = (*AcceleratorTypeHandler)(nil)
	_ rest.Getter            = (*AcceleratorTypeHandler)(nil)
	_ rest.Updater           = (*AcceleratorTypeHandler)(nil)
	_ rest.Patcher           = (*AcceleratorTypeHandler)(nil)
	_ rest.GracefulDeleter   = (*AcceleratorTypeHandler)(nil)
	_ rest.CollectionDeleter = (*AcceleratorTypeHandler)(nil)
)

func (h *AcceleratorTypeHandler) New() runtime.Object {
	return &worker.AcceleratorType{}
}

func (h *AcceleratorTypeHandler) Destroy() {}

func (h *AcceleratorTypeHandler) NewList() runtime.Object {
	return &worker.AcceleratorTypeList{}
}

func (h *AcceleratorTypeHandler) NewListForProxy() runtime.Object {
	return &workercore.AcceleratorTypeList{}
}

func (h *AcceleratorTypeHandler) CastObjectTo(do *worker.AcceleratorType) (uo *workercore.AcceleratorType) {
	return (*workercore.AcceleratorType)(do)
}

func (h *AcceleratorTypeHandler) CastObjectFrom(_ context.Context, uo *workercore.AcceleratorType) (do *worker.AcceleratorType) {
	return (*worker.AcceleratorType)(uo)
}
