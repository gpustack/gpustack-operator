package worker

import (
	"context"
	"strings"
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
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
)

// NodeDevicesReconciler keeps the control-plane labels the worker owns — gpustack.ai/managed and the
// node's real general(CPU) feature key — in sync on each Devices object. A Devices object is
// cluster-scoped and named after the node, so the two share a key. The DeviceManager stamps a Devices
// object's os/arch/accelerator-key selector labels but deliberately leaves these to the worker: node
// management is a control-plane decision the per-node device-manager must not assert, and the CPU key
// (ExtractGeneralNodeKey) needs the node's CPU labels the device-manager's NodeFeature does not carry
// (so it can only ever guess the "generic" sentinel). Mirroring both lets a queue's Devices be
// selected by "<feature key> + kubernetes.io/os|arch + gpustack.ai/managed=true", and — the reason the
// CPU key matters — lets the node-devices AdmissionCheck locate a pool's Devices by the accelerated
// ResourceFlavor's nodeLabels, which carry the same real CPU key.
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

	// Read the node. A missing node leaves the Devices untouched: it is about to be
	// garbage-collected with the node.
	nd := new(core.Node)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: req.Name}, nd)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "fetch node")
		return ctrl.Result{}, err
	}

	// Already in sync — same managed value and same general(CPU) key(s), with no stale one of
	// either kind — leaves the Devices untouched (the DeviceManager owns the rest of the labels).
	want := nodeDevicesControlLabels(nd)
	if nodeDevicesControlInSync(devs.Labels, want) {
		logger.V(3).Info("control labels already synced, skip")
		return ctrl.Result{}, nil
	}

	// Converge: keep the DeviceManager's labels, replacing the worker-owned ones with what the node
	// dictates (a node change may have retired a stale managed mark or a stale CPU key).
	devs.Labels = mapx.Merge(mapx.Filter(devs.Labels, func(k, _ string) bool {
		return !nodeDevicesControlLabelKey(k)
	}), want)
	if err = r.Client.Update(ctx, devs); err != nil {
		logger.Error(err, "sync control labels onto devices")
		return ctrl.Result{}, err
	}

	logger.V(2).Info("synced control labels onto devices", "labels", want)
	return ctrl.Result{}, nil
}

// nodeDevicesControlLabelKey reports whether a label key is one the worker owns on a Devices object:
// the managed mark or a general(CPU) feature key (bare or a .count/.capacity sibling).
func nodeDevicesControlLabelKey(k string) bool {
	return k == systemname.ManagedLabelKey || strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix)
}

// nodeDevicesControlLabels are the labels the worker mirrors from a Node onto its Devices: the managed
// mark (only when the node carries one) and the node's real general(CPU) feature key.
func nodeDevicesControlLabels(nd *core.Node) map[string]string {
	out := make(map[string]string, 2)
	if v := nd.Labels[systemname.ManagedLabelKey]; v != "" {
		out[systemname.ManagedLabelKey] = v
	}
	if gKey := nodefeature.ExtractGeneralNodeKey(nd); gKey != "" {
		out[nodefeature.GeneralFeatureLabelPrefix+gKey] = "true"
	}
	return out
}

// nodeDevicesControlInSync reports whether two label sets agree on the worker-owned control labels:
// the managed mark and every general(CPU) key. It drives both the skip check (Devices vs desired) and
// the watch predicates (old vs new). The DeviceManager-owned labels — notably the accelerator
// (acceleratable.) selector keys the detector stamps — are deliberately ignored, so an
// accelerator-only change never triggers a control re-sync. This is the mirror of the detector's
// acceleratableDevicesSelectorLabels, which keeps those accelerator keys and drops the general key.
func nodeDevicesControlInSync(a, b map[string]string) bool {
	return mapx.EqualWithKey(a, b, systemname.ManagedLabelKey) &&
		mapx.EqualWithStringPrefix(a, b, nodefeature.GeneralFeatureLabelPrefix)
}

func (r *NodeDevicesReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodedevices").
		For(
			// Reconcile each Devices object by its own name (= the node name). A
			// start-up resync stamps every Devices, and an update is observed only when
			// a worker-owned control label drifts (the DeviceManager rewrites other labels often).
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
						return !nodeDevicesControlInSync(oldDevs.Labels, newDevs.Labels)
					},
				},
			),
		).
		Watches(
			// Watch Nodes and enqueue the same-named Devices when a control label changes.
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
						return !nodeDevicesControlInSync(oldNd.Labels, newNd.Labels)
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
