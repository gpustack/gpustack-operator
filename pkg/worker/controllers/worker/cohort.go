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
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
)

// CohortReconciler reconciles kueue.Cohort objects driven by Kubernetes Node and
// kueue.ClusterQueue changes to finish the following tasks:
//   - When a Node references a cohort profile, create the matching kueue.Cohort.
//   - When no Node references it, keep the kueue.Cohort while any ClusterQueue
//     still belongs to it (deleting it would cascade-delete those queues via the
//     ownerRef), and delete it only once neither a Node nor a ClusterQueue
//     references it.
type CohortReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*CohortReconciler)(nil)

func (r *CohortReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// A Node still references this cohort profile: ensure the Cohort exists.
	ndList := new(core.NodeList)
	err := r.Client.List(ctx, ndList,
		ctrlcli.MatchingFields{IndexingNodeByCohortProfile: req.Name},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list nodes by cohort profile")
		return ctrl.Result{}, err
	}
	if len(ndList.Items) > 0 {
		return r.ensureCohort(ctx, req.Name)
	}

	// No Node references the cohort, but keep it while any ClusterQueue still
	// does (e.g. a queue draining): deleting the Cohort would cascade-delete its
	// ClusterQueues via the ownerRef and disrupt running workloads. Only delete
	// once the cohort is fully idle (no Node and no ClusterQueue).
	cqList := new(kueue.ClusterQueueList)
	err = r.Client.List(ctx, cqList,
		ctrlcli.MatchingFields{IndexingClusterQueuesByCohortName: req.Name},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list cluster queues by cohort name")
		return ctrl.Result{}, err
	}
	if len(cqList.Items) > 0 {
		logger.V(3).Info("cohort has no node but still has cluster queues; keep it")
		return ctrl.Result{}, nil
	}

	// Fully idle: delete the orphaned cohort.
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

// ensureCohort creates the named Cohort if it does not exist yet.
func (r *CohortReconciler) ensureCohort(ctx context.Context, name string) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	co := new(kueue.Cohort)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: name}, co)
	if err == nil {
		if co.DeletionTimestamp != nil {
			logger.V(3).Info("cohort is being deleted; requeue in 15s")
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
			Name: name,
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
	IndexingNodeByCohortProfile       = "nodes.labels['feature.gpustack.ai/*.z-cohort']"
	IndexingClusterQueuesByCohortName = "clusterqueues.spec.cohortName"
)

// indexNodeByCohortProfile is the field-index extractor for
// IndexingNodeByCohortProfile: it maps a managed Node to the cohort profile
// names it currently uses. Nodes that are being deleted or are not managed are
// excluded, so they drop out of the index.
func indexNodeByCohortProfile(obj ctrlcli.Object) []string {
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
}

// indexClusterQueueByCohortName is the field-index extractor for
// IndexingClusterQueuesByCohortName: it maps a managed ClusterQueue to the
// cohort name it belongs to, so the reconciler can tell whether a cohort still
// has any ClusterQueue before reclaiming it.
func indexClusterQueueByCohortName(obj ctrlcli.Object) []string {
	if obj == nil {
		return nil
	}

	cq := obj.(*kueue.ClusterQueue)
	if !systemmeta.MatchResource(cq, "instancetypes") {
		return nil
	}
	if cq.Spec.CohortName == "" {
		return nil
	}
	return []string{string(cq.Spec.CohortName)}
}

func (r *CohortReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &core.Node{}, IndexingNodeByCohortProfile, indexNodeByCohortProfile)
	if err != nil {
		return fmt.Errorf("index node '%s': %w", IndexingNodeByCohortProfile, err)
	}
	err = fi.IndexField(ctx, &kueue.ClusterQueue{}, IndexingClusterQueuesByCohortName, indexClusterQueueByCohortName)
	if err != nil {
		return fmt.Errorf("index cluster queue '%s': %w", IndexingClusterQueuesByCohortName, err)
	}

	r.Client = opts.Manager.GetClient()

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("cohort").
		Watches(
			// Watch Nodes and enqueue the corresponding Cohort.
			&core.Node{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueCohortWhenNodeChanged,
			),
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a Node is:
				// - created.
				// - deleted.
				// - updated if its managed mark or feature labels have changed.
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp == nil {
							// Fire when the managed mark or feature labels have changed.
							return !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								systemname.ManagedLabelKey,
								nodefeature.FeatureLabelPrefix,
								nodefeature.GeneralFeatureLabelPrefix,
								nodefeature.AcceleratableFeatureLabelPrefix)
						}
						// Fire when the deletion timestamp changes.
						if oldNd.DeletionTimestamp == nil {
							return true
						}
						return !oldNd.DeletionTimestamp.Equal(newNd.DeletionTimestamp)
					},
				},
			),
		).
		Watches(
			// Watch kueue.ClusterQueues and enqueue their Cohort, so a cohort
			// that just lost its last ClusterQueue (drained away) is re-evaluated
			// for deletion. Creation never reclaims a cohort, but it harmlessly
			// re-asserts an existing one.
			&kueue.ClusterQueue{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueCohortWhenClusterQueueChanged,
			),
			ctrlbuilder.WithPredicates(
				// Interested in relevant ClusterQueue objects.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "instancetypes")
				}),
				// Trigger reconciliation when a ClusterQueue is:
				// - created.
				// - deleted (so a cohort losing its last queue gets reclaimed).
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						return false
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

func (r *CohortReconciler) enqueueCohortWhenClusterQueueChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("cluster queue", ctrlcli.ObjectKeyFromObject(obj))

	cq := obj.(*kueue.ClusterQueue)

	cohortName := string(cq.Spec.CohortName)
	if cohortName == "" {
		logger.V(2).Info("cluster queue has no cohort")
		return nil
	}

	logger.V(2).Info("enqueue cohort from cluster queue", "cohort", cohortName)
	return []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKey{
				Name: cohortName,
			},
		},
	}
}
