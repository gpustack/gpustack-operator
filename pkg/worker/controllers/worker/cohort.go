package worker

import (
	"context"
	"fmt"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
)

// CohortReconciler reconciles kueue.Cohort objects driven by Kubernetes Node changes to finish the following tasks:
//   - When a Node is created/deleted or its feature labels are updated,
//     create the kueue.Cohort matching the Node's cohort profile,
//     or delete an orphaned kueue.Cohort when no Node references it.
type CohortReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*CohortReconciler)(nil)

func (r *CohortReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	ndList := new(core.NodeList)
	err := r.Client.List(ctx, ndList,
		ctrlcli.MatchingFields{IndexingNodeByCohortProfile: req.Name},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list nodes by cohort profile")
		return ctrl.Result{}, err
	}

	if len(ndList.Items) == 0 {
		co := new(kueue.Cohort)
		err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: req.Name}, co)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				logger.Error(err, "fetch cohort")
				return ctrl.Result{}, err
			}
			logger.V(3).Info("cohort already deleted, skip")
			return ctrl.Result{}, nil
		}
		if co.DeletionTimestamp != nil {
			logger.V(3).Info("skip deleted cohort")
			return ctrl.Result{}, nil
		}
		logger.Info("deleting orphaned cohort")
		err = r.Client.Delete(ctx, co)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				logger.Error(err, "delete cohort")
				return ctrl.Result{}, err
			}
		}
		logger.V(2).Info("deleted orphaned cohort")
		return ctrl.Result{}, nil
	}

	co := new(kueue.Cohort)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: req.Name}, co)
	if err == nil {
		if co.DeletionTimestamp != nil {
			logger.V(3).Info("cohort is being deleted, retry later")
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		logger.V(3).Info("cohort already exists")
		return ctrl.Result{}, nil
	}
	if !kerrors.IsNotFound(err) {
		logger.Error(err, "fetch cohort")
		return ctrl.Result{}, err
	}
	logger.Info("cohort not found, creating it")
	co = &kueue.Cohort{
		ObjectMeta: meta.ObjectMeta{
			Name: req.Name,
		},
	}
	err = r.Client.Create(ctx, co)
	if err != nil {
		logger.Error(err, "create cohort")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("created cohort")
	return ctrl.Result{}, nil
}

const (
	IndexingNodeByCohortProfile = "nodes.labels['feature.gpustack.ai/*.z-cohort']"
)

func (r *CohortReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &core.Node{}, IndexingNodeByCohortProfile,
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}

			nd := obj.(*core.Node)
			if nd.DeletionTimestamp != nil {
				return nil
			}
			if !kubemeta.IsLabeled(nd, systemname.ManagedLabelKey, "true") {
				return nil
			}

			profiles := nodefeature.ExtractNodeProfiles(nd)
			return slicex.Transform(profiles, func(p nodefeature.NodeProfile) string {
				return p.Cohort
			})
		})
	if err != nil {
		return fmt.Errorf("index node '%s': %w", IndexingNodeByCohortProfile, err)
	}

	r.Client = opts.Manager.GetClient()

	aggressive := opts.Manager.AllowAggressiveEventFiltering()
	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("cohort").
		Watches(
			// Watch Nodes and enqueue the corresponding Cohort.
			&core.Node{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(
				5*time.Second,
				r.enqueueCohortWhenNodeChanged,
			),
			ctrlbuilder.WithPredicates(
				// Interested in Node objects:
				// - created.
				// - deleted.
				// - updated if labels have changed.
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						if aggressive {
							return true
						}

						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp == nil {
							return !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								nodefeature.FeatureLabelPrefix,
								nodefeature.GeneralFeatureLabelPrefix,
								nodefeature.AcceleratableFeatureLabelPrefix)
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

func (r *CohortReconciler) enqueueCohortWhenNodeChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("node", ctrlcli.ObjectKeyFromObject(obj))

	nd := obj.(*core.Node)

	profiles := nodefeature.ExtractNodeProfiles(nd)
	if len(profiles) == 0 {
		logger.V(2).Info("node has no profile")
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(profiles))
	for i := range profiles {
		if profiles[i].Cohort == "" {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{
				Name: profiles[i].Cohort,
			},
		})
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueue cohort from node", "requests", reqs)
	return reqs
}
