package worker

import (
	"context"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
)

// NodeDevicesReconciler keeps the gpustack.ai/managed label on each Devices object
// in sync with its Node. A Devices object is cluster-scoped and named after the
// node, so the two share a key. The DeviceManager stamps a Devices object's
// os/arch/feature-key selector labels but deliberately leaves the managed mark to
// this reconciler — node management is a control-plane decision the per-node
// device-manager must not assert. Mirroring it onto the Devices lets a queue's
// Devices be selected by "<feature key> + kubernetes.io/os|arch + gpustack.ai/managed=true".
type NodeDevicesReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeDevicesReconciler)(nil)

func (r *NodeDevicesReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch the Devices (named after the node). Gone → nothing to sync; it is
	// garbage-collected with its owning NodeFeature/Node.
	devs := new(workercore.Devices)
	err := r.Client.Get(ctx, req.NamespacedName, devs)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch devices")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if devs.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted devices")
		return ctrl.Result{}, nil
	}

	// Read the node's managed mark. A missing node leaves the Devices untouched: it
	// is about to be garbage-collected with the node.
	nd := new(core.Node)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: req.Name}, nd)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "fetch node")
		return ctrl.Result{}, err
	}
	want := nd.Labels[systemname.ManagedLabelKey]

	// Converge: mirror the node's value onto the Devices, deleting the label when the
	// node carries none.
	if devs.Labels[systemname.ManagedLabelKey] == want {
		logger.V(3).Info("managed label already synced, skip", "managed", want)
		return ctrl.Result{}, nil
	}

	if want == "" {
		delete(devs.Labels, systemname.ManagedLabelKey)
		logger.V(2).Info("removed managed label from devices")
	} else {
		if devs.Labels == nil {
			devs.Labels = make(map[string]string)
		}
		devs.Labels[systemname.ManagedLabelKey] = want
		logger.V(2).Info("added managed label onto devices", "managed", want)
	}
	if err = r.Client.Update(ctx, devs); err != nil {
		logger.Error(err, "sync managed label onto devices")
		return ctrl.Result{}, err
	}

	logger.V(2).Info("synced managed label onto devices")
	return ctrl.Result{}, nil
}

func (r *NodeDevicesReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodedevices").
		For(
			// Reconcile each Devices object by its own name (= the node name). A
			// start-up resync stamps every Devices, and an update is observed only when
			// the managed mark drifts (the DeviceManager rewrites other labels often).
			&workercore.Devices{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldDevs, newDevs := e.ObjectOld.(*workercore.Devices), e.ObjectNew.(*workercore.Devices)
						if newDevs.DeletionTimestamp != nil {
							return false
						}
						return oldDevs.Labels[systemname.ManagedLabelKey] != newDevs.Labels[systemname.ManagedLabelKey]
					},
				},
			),
		).
		Watches(
			// Watch Nodes and enqueue the same-named Devices when the managed mark changes.
			&core.Node{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(
				5*time.Second,
				r.enqueueDevicesWhenNodeChanged,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp != nil {
							return false
						}
						return oldNd.Labels[systemname.ManagedLabelKey] != newNd.Labels[systemname.ManagedLabelKey]
					},
				},
			),
		).
		Complete(r)
}

func (r *NodeDevicesReconciler) enqueueDevicesWhenNodeChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("node", ctrlcli.ObjectKeyFromObject(obj))

	reqs := []ctrlreconcile.Request{
		{NamespacedName: ctrlcli.ObjectKey{Name: obj.GetName()}},
	}

	logger.V(2).Info("enqueue devices from node", "requests", reqs)
	return reqs
}
