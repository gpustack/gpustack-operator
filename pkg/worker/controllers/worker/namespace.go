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
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// NamespaceReconciler reconciles the core.Namespace object,
// and manages corresponding LocalQueue.
type NamespaceReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NamespaceReconciler)(nil)

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrlreconcile.Request) (ctrlreconcile.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	ns := new(core.Namespace)
	err := r.Client.Get(ctx, req.NamespacedName, ns)
	if err != nil {
		logger.Error(err, "fetch namespace")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Skip if deleted.
	if ns.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Skip if Namespace is system namespace.
	if ns.Name == kuberess.SystemNamespaceName {
		return ctrl.Result{}, nil
	}

	cqList := new(kueue.ClusterQueueList)
	err = r.Client.List(ctx, cqList,
		kubeclientset.NonQuorum,
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list cluster queues")
		return ctrl.Result{}, err
	}

	var errs []error

	// Iterate ClusterQueues to create LocalQueue in the Namespace if not exists.
	for i := range cqList.Items {
		cq := &cqList.Items[i]

		// Skip if ClusterQueue is terminating.
		if cq.DeletionTimestamp != nil {
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

func (r *NamespaceReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("worker.manage.namesapces").
		For(
			// Focus on the Kubernetes Namespace,
			// when the Namespace is created.
			&core.Namespace{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.Not(ctrlpredicate.Funcs{
					CreateFunc: func(e ctrlevent.CreateEvent) bool {
						return false
					},
				}),
			),
		).
		Complete(r)
}
