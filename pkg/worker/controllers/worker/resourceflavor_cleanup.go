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
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
)

// ResourceFlavorCleanupReconciler reconciles kueue.ResourceFlavor objects driven by Kubernetes Node changes to finish the following tasks:
//   - When a Node is created/deleted or its feature labels are updated,
//     delete the corresponding kueue.ResourceFlavor when no Node references it.
type ResourceFlavorCleanupReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*ResourceFlavorCleanupReconciler)(nil)

func (r *ResourceFlavorCleanupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	rf := new(kueue.ResourceFlavor)
	err := r.Client.Get(ctx, req.NamespacedName, rf)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch resource flavor")
			return ctrl.Result{}, err
		}
		logger.V(3).Info("resource flavor already deleted, skip")
		return ctrl.Result{}, nil
	}

	// Skip if deleted.
	if rf.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted resource flavor")
		return ctrl.Result{}, nil
	}

	ndList := new(core.NodeList)
	err = r.Client.List(ctx, ndList,
		ctrlcli.MatchingFields{IndexingNodeByFlavorProfile: rf.Name},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list nodes by flavor profile")
		return ctrl.Result{}, err
	}

	if len(ndList.Items) == 0 {
		logger.Info("deleting orphaned resource flavor")
		err = r.Client.Delete(ctx, rf)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				logger.Error(err, "delete orphaned resource flavor")
				return ctrl.Result{}, err
			}
		}
		logger.V(2).Info("deleted orphaned resource flavor")
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

func (r *ResourceFlavorCleanupReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("resourceflavor.cleanup").
		Watches(
			// Watch Nodes and enqueue the corresponding ResourceFlavor.
			&core.Node{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(
				5*time.Second,
				r.enqueueResourceFlavorWhenNodeChanged,
			),
			ctrlbuilder.WithPredicates(
				// Interested in Node objects:
				// - created.
				// - deleted.
				// - updated if labels have changed.
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp == nil {
							return !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								devicefeature.FeatureLabelPrefix)
						}
						if oldNd.DeletionTimestamp == nil {
							return true
						}
						return !oldNd.DeletionTimestamp.Equal(newNd.DeletionTimestamp)
					},
				},
			),
		).
		Complete(r)
}

func (r *ResourceFlavorCleanupReconciler) enqueueResourceFlavorWhenNodeChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("node", ctrlcli.ObjectKeyFromObject(obj))

	nd := obj.(*core.Node)

	profiles := devicefeature.ExtractNodeProfiles(nd)
	if len(profiles) == 0 {
		logger.V(2).Info("node has no profile")
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(profiles))
	for i := range profiles {
		if profiles[i].Flavor == "" {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{
				Name: profiles[i].Flavor,
			},
		})
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueue resource flavor from node", "requests", reqs)
	return reqs
}
