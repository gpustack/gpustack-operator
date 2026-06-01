package worker

import (
	"context"

	"go.uber.org/multierr"
	core "k8s.io/api/core/v1"
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
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// ClusterQueueReconciler reconciles all kueue.ClusterQueue objects to finish the following tasks:
// - When a ClusterQueue is created, create corresponding LocalQueue in each Namespace if not exists.
type ClusterQueueReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*ClusterQueueReconciler)(nil)

func (r *ClusterQueueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	cq := new(kueue.ClusterQueue)
	err := r.Client.Get(ctx, req.NamespacedName, cq)
	if err != nil {
		logger.Error(err, "fetch cluster queue")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Skip if deleted.
	if cq.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Create LocalQueue.
	err = r.createLocalQueue(ctx, cq)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ClusterQueueReconciler) createLocalQueue(ctx context.Context, cq *kueue.ClusterQueue) error {
	logger := ctrllog.FromContext(ctx)

	nsList := new(core.NamespaceList)
	err := r.Client.List(ctx, nsList,
		kubeclientset.NonQuorum,
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list namespaces")
		return err
	}

	var errs []error

	// Iterate Namespaces to create LocalQueue in each Namespace if not exists.
	for i := range nsList.Items {
		ns := &nsList.Items[i]

		// Skip if Namespace is terminating.
		if ns.DeletionTimestamp != nil {
			continue
		}

		// Skip if Namespace is system toolkit namespace.
		if ns.Name == kuberess.SystemNamespaceName {
			logger.V(2).Info("skip system namespace", "namespace", ns.Name)
			continue
		}

		lq := &kueue.LocalQueue{
			ObjectMeta: meta.ObjectMeta{
				Name:      cq.Name,
				Namespace: ns.Name,
			},
			Spec: kueue.LocalQueueSpec{
				ClusterQueue: kueue.ClusterQueueReference(cq.Name),
			},
		}
		kubemeta.ControlOnWithoutBlock(lq, cq, kueue.SchemeGroupVersion.WithKind("ClusterQueue"))
		_, err = kubeclientset.CreateWithCtrlClient(ctx, r.Client, lq)
		if err != nil {
			logger.Error(err, "create local queue", "namespace", ns.Name)
			errs = append(errs, err)
		}
	}

	return multierr.Combine(errs...)
}

func (r *ClusterQueueReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("worker.manage.cluster_queues").
		For(
			&kueue.ClusterQueue{},
			ctrlbuilder.WithPredicates(
				// Interested in relevant ClusterQueue objects.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "instancetypes")
				}),
				// Trigger reconciliation when a ClusterQueue is:
				// - created.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						return false
					},
					GenericFunc: func(e ctrlevent.GenericEvent) bool { return false },
				},
			),
		).
		Complete(r)
}
