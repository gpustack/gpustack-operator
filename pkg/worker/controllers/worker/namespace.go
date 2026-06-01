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

// NamespaceReconciler reconciles all Kubernetes Namespace objects to finish the following tasks:
// - When a Namespace is created, create corresponding LocalQueue for each ClusterQueue if not exists.
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
		logger.V(2).Info("skip system namespace", "namespace", ns.Name)
		return ctrl.Result{}, nil
	}

	// Create LocalQueue.
	err = r.createLocalQueue(ctx, ns)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *NamespaceReconciler) createLocalQueue(ctx context.Context, ns *core.Namespace) error {
	logger := ctrllog.FromContext(ctx)

	cqList := new(kueue.ClusterQueueList)
	err := r.Client.List(ctx, cqList,
		kubeclientset.NonQuorum,
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list cluster queues")
		return err
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

	return multierr.Combine(errs...)
}

func (r *NamespaceReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("worker.manage.namesapces").
		For(
			&core.Namespace{},
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when Namespace is:
				// - created.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						return false
					},
					GenericFunc: func(e ctrlevent.GenericEvent) bool {
						return false
					},
				},
			),
		).
		Complete(r)
}
