package worker

import (
	"context"
	"fmt"
	"strconv"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/setting"
	workersettings "gpustack.ai/gpustack/pkg/worker/settings"
)

// The cells this view's own columns occupy. The framework books the object's name first and the
// age last, so the columns between them are addressed by position.
const (
	modelStoreHighCell = 2
	modelStoreLowCell  = 3
)

// ModelStoreHandler proxies a v1alpha1.ModelStore as the public ModelStore resource.
//
// No status proxy is exposed because the status is the controller's observation of the matched
// nodes' residency, while the proxy writes with the worker's identity. The storage resource's
// status subresource keeps the status as it is on main-resource writes, so the status is read
// through the main resource.
type ModelStoreHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations
}

func (h *ModelStoreHandler) SetupHandler(
	_ context.Context, opts extensionapi.SetupOptions,
) (schema.GroupVersionResource, map[string]rest.Storage, error) {
	gvr := worker.SchemeGroupVersionResource("modelstores")
	table, err := modelStoreTable(opts.Manager.GetAPIReader())
	if err != nil {
		return gvr, nil, err
	}
	h.ObjectInfo = &worker.ModelStore{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.ModelStore, *worker.ModelStoreList,
		*workercore.ModelStore, *workercore.ModelStoreList,
	](table, h, opts.Manager.GetClient().(ctrlcli.WithWatch), opts.Manager.GetAPIReader())
	return gvr, map[string]rest.Storage{}, nil
}

// modelStoreTable is the view's kubectl table: the columns a ModelStore's own object answers, with
// the two watermark cells completed from the cluster Settings wherever the pool states none.
//
// It states the pool's own cache policy, which is the layer above the cluster defaults. It is not a
// report of what a node has applied: a node's own kubelet thresholds can cap the high watermark
// below the pool's, and only that node's NodeModelStore shows the result.
func modelStoreTable(reader ctrlcli.Reader) (rest.TableConvertor, error) {
	columns, err := extensionapi.NewJSONPathTemplateTableConvertor(
		extensionapi.JSONPathTemplateColumn("Nodes", "{.status.nodes}"),
		extensionapi.JSONPathTemplateColumn("High", "{.spec.watermarks.highPercent}"),
		extensionapi.JSONPathTemplateColumn("Low", "{.spec.watermarks.lowPercent}"))
	if err != nil {
		return nil, err
	}
	return modelStoreTableConvertor{columns: columns, reader: reader}, nil
}

// modelStoreTableConvertor fills the two watermark cells of the rows whose pool states none, from
// one reading of the cluster Settings.
//
// It wraps the view's own columns rather than replacing them, so the headers, the list metadata and
// the object each row carries are the ones the table framework already produces. The cluster values
// are read once, at the first row that needs them, and the rest of the rows share that reading, so
// a request that inherits nothing never reads the Secret at all. Only the cells are written: the
// object a pool was read from is left as it is, so a pool that inherits still states no watermarks
// and the table's own copy of the object says the same.
type modelStoreTableConvertor struct {
	columns rest.TableConvertor
	reader  ctrlcli.Reader
}

func (c modelStoreTableConvertor) ConvertToTable(
	ctx context.Context, obj, tableOptions runtime.Object,
) (*meta.Table, error) {
	table, err := c.columns.ConvertToTable(ctx, obj, tableOptions)
	if err != nil {
		return nil, err
	}

	var (
		cluster workercore.NodeModelStoreWatermarks
		read    bool
	)
	for i := range table.Rows {
		store, ok := table.Rows[i].Object.Object.(*worker.ModelStore)
		if !ok || store.Spec.Watermarks != nil {
			continue
		}
		if !read {
			if cluster, err = readClusterWatermarks(ctx, c.reader); err != nil {
				return nil, err
			}
			read = true
		}
		table.Rows[i].Cells[modelStoreHighCell] = strconv.Itoa(int(cluster.HighPercent))
		table.Rows[i].Cells[modelStoreLowCell] = strconv.Itoa(int(cluster.LowPercent))
	}
	return table, nil
}

// readClusterWatermarks reads the watermarks the cluster Settings hold.
//
// It reads through the API reader rather than the Settings package, whose read cache answers for up
// to thirty seconds: a table should show the values stored now. A Secret that does not exist, and a
// key it does not carry, fall back to the Setting's own default, which is the cluster layer a pool
// states nothing over. Any other read failure, and a stored value that is not a percent, are
// errors: a table that cannot say which cluster values stand must not present a default as if an
// operator had chosen it.
func readClusterWatermarks(ctx context.Context, reader ctrlcli.Reader) (workercore.NodeModelStoreWatermarks, error) {
	sec := new(core.Secret)
	key := ctrlcli.ObjectKey{Namespace: setting.DelegatedSecretNamespace, Name: setting.DelegatedSecretName}
	if err := reader.Get(ctx, key, sec); ctrlcli.IgnoreNotFound(err) != nil {
		return workercore.NodeModelStoreWatermarks{}, fmt.Errorf("read the settings secret: %w", err)
	}
	// A Secret that was not found leaves sec empty, so every key falls back to its default.

	var wm workercore.NodeModelStoreWatermarks
	var err error
	if wm.HighPercent, err = watermarkPercent(sec.Data, workersettings.ModelStoreHighWatermark); err != nil {
		return workercore.NodeModelStoreWatermarks{}, err
	}
	if wm.LowPercent, err = watermarkPercent(sec.Data, workersettings.ModelStoreLowWatermark); err != nil {
		return workercore.NodeModelStoreWatermarks{}, err
	}
	return wm, nil
}

// watermarkPercent reads one watermark's stored value, or the Setting's default when the Secret
// carries no value for it. Only the two watermark keys are read, so a malformed value in some
// other Setting is that Setting's business and does not keep a table from rendering.
func watermarkPercent(data map[string][]byte, s setting.Setting) (int32, error) {
	value := s.DefaultValue()
	if v, ok := data[s.Name()]; ok {
		value = string(v)
	}
	percent, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("setting %s: %w", s.Name(), err)
	}
	return int32(percent), nil
}

func (h *ModelStoreHandler) New() runtime.Object     { return &worker.ModelStore{} }
func (h *ModelStoreHandler) Destroy()                {}
func (h *ModelStoreHandler) NewList() runtime.Object { return &worker.ModelStoreList{} }
func (h *ModelStoreHandler) NewListForProxy() runtime.Object {
	return &workercore.ModelStoreList{}
}

func (h *ModelStoreHandler) CastObjectTo(obj *worker.ModelStore) *workercore.ModelStore {
	return (*workercore.ModelStore)(obj)
}

func (h *ModelStoreHandler) CastObjectFrom(
	_ context.Context, obj *workercore.ModelStore,
) *worker.ModelStore {
	return (*worker.ModelStore)(obj)
}
