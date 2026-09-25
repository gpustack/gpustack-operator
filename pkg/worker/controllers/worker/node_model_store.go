package worker

import (
	"context"
	"slices"
	"time"

	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// nodeModelStoreInvalidRetry is how soon a node whose computed configuration fails its check is
// looked at again. Admission refuses such values, so it takes two racing writes to reach here.
const nodeModelStoreInvalidRetry = time.Minute

// NodeModelStoreReconciler keeps one NodeModelStore per node the model-manager plugin registered on,
// and keeps its spec equal to the node's effective configuration.
//
// THE OBJECT'S LIFETIME IS THE NODE'S, NOT THE PLUGIN REGISTRATION'S. It is created once the node's
// CSINode lists the plugin's driver, kubelet's own record that the plugin runs there, and it is not
// deleted when the driver leaves CSINode: a plugin restart or a rolling upgrade unregisters the driver
// for a moment, and deleting on that would erase and rewrite every node's object on every upgrade.
// The owner reference to the Node collects it with the Node. The component's removal is read at the
// cluster level instead: while the CSIDriver object does not exist, every NodeModelStore is deleted.
//
// It writes spec only. Status belongs to the plugin on the node.
type NodeModelStoreReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeModelStoreReconciler)(nil)

func (r *NodeModelStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	installed, err := r.driverInstalled(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !installed {
		nms := &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: req.Name}}
		if err := r.Client.Delete(ctx, nms); ctrlcli.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	nd := new(core.Node)
	if err := r.Client.Get(ctx, req.NamespacedName, nd); err != nil {
		// A deleted Node takes its NodeModelStore with it through the owner reference.
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	if nd.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	registered, err := r.driverRegistered(ctx, nd.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	existing := new(workercore.NodeModelStore)
	switch err := r.Client.Get(ctx, req.NamespacedName, existing); {
	case kerrors.IsNotFound(err):
		if !registered {
			return ctrl.Result{}, nil
		}
	case err != nil:
		return ctrl.Result{}, err
	}

	spec, err := r.effectiveSpec(ctx)
	if err != nil {
		logger.Error(err, "the node model cache's configuration fails its check; the node's spec is left as it is")
		return ctrl.Result{RequeueAfter: nodeModelStoreInvalidRetry}, nil
	}

	eNms := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: nd.Name},
		Spec:       spec,
	}
	kubemeta.ControlOnWithoutBlock(eNms, nd, core.SchemeGroupVersion.WithKind("Node"))
	alignFn := func(aNms *workercore.NodeModelStore) (_ *workercore.NodeModelStore, skip bool, err error) {
		skip = true
		if !kubemeta.DeepEqual(aNms.Spec, eNms.Spec) {
			aNms.Spec = eNms.Spec
			skip = false
		}
		if !kubemeta.IsControlledBy(aNms, nd) {
			kubemeta.ControlOnWithoutBlock(aNms, nd, core.SchemeGroupVersion.WithKind("Node"))
			skip = false
		}
		return aNms, skip, nil
	}
	if _, err := kubeclientset.CreateWithCtrlClient(ctx, r.Client, eNms,
		kubeclientset.WithUpdateIfExisted(alignFn)); err != nil {
		// The watch on this object passes spec changes only, so a conflict with the plugin's status
		// write would not come back as an event.
		return objectWriteResult(logger, err, "sync node model store", _requeueAfterConflict)
	}

	return ctrl.Result{}, nil
}

// driverInstalled reports whether the plugin's CSIDriver object exists.
func (r *NodeModelStoreReconciler) driverInstalled(ctx context.Context) (bool, error) {
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: modelstore.DriverName}, new(storage.CSIDriver))
	switch {
	case err == nil:
		return true, nil
	case kerrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// driverRegistered reports whether the node's CSINode lists the plugin's driver.
func (r *NodeModelStoreReconciler) driverRegistered(ctx context.Context, node string) (bool, error) {
	csiNode := new(storage.CSINode)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: node}, csiNode); err != nil {
		return false, ctrlcli.IgnoreNotFound(err)
	}

	return csiNodeListsModelDriver(csiNode), nil
}

func csiNodeListsModelDriver(csiNode *storage.CSINode) bool {
	return slices.ContainsFunc(csiNode.Spec.Drivers, func(d storage.CSINodeDriver) bool {
		return d.Name == modelstore.DriverName
	})
}

// effectiveSpec merges the configuration layers into a node's spec and checks it. The one layer so
// far is the cluster's Settings, read from the Settings store this controller watches rather than
// through the Settings package's read cache, so a change reaches every node on its own event.
func (r *NodeModelStoreReconciler) effectiveSpec(ctx context.Context) (workercore.NodeModelStoreSpec, error) {
	sec := new(core.Secret)
	key := ctrlcli.ObjectKey{Namespace: setting.DelegatedSecretNamespace, Name: setting.DelegatedSecretName}
	if err := r.Client.Get(ctx, key, sec); ctrlcli.IgnoreNotFound(err) != nil {
		return workercore.NodeModelStoreSpec{}, err
	}
	value := func(s setting.Setting) string {
		if v, ok := sec.Data[s.Name()]; ok {
			return string(v)
		}
		return s.DefaultValue()
	}

	cluster, err := settings.ModelStoreLayer(value)
	if err != nil {
		return workercore.NodeModelStoreSpec{}, err
	}
	spec := modelstore.Merge(cluster)

	return spec, modelstore.Validate(spec)
}

func (r *NodeModelStoreReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodemodelstore").
		For(
			&storage.CSINode{},
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
					return csiNodeListsModelDriver(e.ObjectOld.(*storage.CSINode)) !=
						csiNodeListsModelDriver(e.ObjectNew.(*storage.CSINode))
				},
			}),
		).
		Watches(
			&workercore.NodeModelStore{},
			&ctrlhandler.EnqueueRequestForObject{},
			ctrlbuilder.WithPredicates(ctrlpredicate.GenerationChangedPredicate{}),
		).
		Watches(
			&storage.CSIDriver{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueEveryNode),
			ctrlbuilder.WithPredicates(namePredicate(modelstore.DriverName)),
		).
		Watches(
			&core.Secret{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueEveryNode),
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(o ctrlcli.Object) bool {
				return o.GetNamespace() == setting.DelegatedSecretNamespace && o.GetName() == setting.DelegatedSecretName
			})),
		).
		Complete(r)
}

// enqueueEveryNode enqueues every node that has a NodeModelStore or whose CSINode lists the driver:
// a Settings change reaches every node's spec, and the CSIDriver's removal every node's object.
func (r *NodeModelStoreReconciler) enqueueEveryNode(ctx context.Context, _ ctrlcli.Object) []ctrlreconcile.Request {
	names := map[string]struct{}{}

	stores := new(workercore.NodeModelStoreList)
	if err := r.Client.List(ctx, stores); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list node model stores")
	}
	for i := range stores.Items {
		names[stores.Items[i].Name] = struct{}{}
	}
	csiNodes := new(storage.CSINodeList)
	if err := r.Client.List(ctx, csiNodes); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list csi nodes")
	}
	for i := range csiNodes.Items {
		if csiNodeListsModelDriver(&csiNodes.Items[i]) {
			names[csiNodes.Items[i].Name] = struct{}{}
		}
	}

	reqs := make([]ctrlreconcile.Request, 0, len(names))
	for name := range names {
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
	}

	return reqs
}

func namePredicate(name string) ctrlpredicate.Predicate {
	return ctrlpredicate.NewPredicateFuncs(func(o ctrlcli.Object) bool { return o.GetName() == name })
}
