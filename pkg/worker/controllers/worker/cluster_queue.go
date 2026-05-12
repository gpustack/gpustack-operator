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

// ClusterQueueReconciler reconciles the kueue.ClusterQueue object,
// and manages corresponding LocalQueue.
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

	// Create corresponding LocalQueue if not exists.
	nsList := new(core.NamespaceList)
	err = r.Client.List(ctx, nsList,
		&ctrlcli.ListOptions{
			Raw: &meta.ListOptions{
				ResourceVersion: "0",
			},
		},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list namespaces")
		return ctrl.Result{}, err
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

	if len(errs) > 0 {
		return ctrl.Result{}, multierr.Combine(errs...)
	}
	return ctrl.Result{}, nil
}

func (r *ClusterQueueReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("worker.manage.cluster_queues").
		For(
			// Focus on the Kueue ClusterQueue,
			// when the ClusterQueue is created.
			&kueue.ClusterQueue{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "instancetypes")
				}),
				ctrlpredicate.Not(ctrlpredicate.Funcs{
					CreateFunc: func(e ctrlevent.CreateEvent) bool {
						return false
					},
				}),
			),
		).
		Complete(r)
}
