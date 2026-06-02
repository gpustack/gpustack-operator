package worker

import (
	"context"
	"time"

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
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

type LocalQueueReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*LocalQueueReconciler)(nil)

func (r *LocalQueueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
		logger.V(3).Info("skip deleted cluster queue")
		return ctrl.Result{}, nil
	}

	// List Namespaces.
	nsList := new(core.NamespaceList)
	err = r.Client.List(ctx, nsList,
		ctrlclix.NonQuorum,
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list namespaces")
		return ctrl.Result{}, err
	}

	var errs []error

	// Iterate Namespaces to create LocalQueue in each Namespace if not exists.
	for i := range nsList.Items {
		ns := &nsList.Items[i]

		// Skip if deleted.
		if ns.DeletionTimestamp != nil {
			logger.V(3).Info("skip deleted namespace")
			continue
		}

		// Skip if system namespace.
		if ns.Name == kuberess.SystemNamespaceName {
			logger.V(3).Info("skip system namespace")
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
		_, err = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, lq,
			kubeclientset.WithCreateIfNotExisted[*kueue.LocalQueue]())
		if err != nil {
			logger.Error(err, "sync local queue", "namespace", ns.Name)
			errs = append(errs, err)
		}
	}

	return ctrl.Result{}, multierr.Combine(errs...)
}

func (r *LocalQueueReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("localqueue").
		For(
			&kueue.ClusterQueue{},
			ctrlbuilder.WithPredicates(
				// Interested in relevant ClusterQueue objects.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "instancetypes")
				}),
				// Trigger reconciliation when a ClusterQueue is:
				// - created.
				ctrlpredicate.Not(ctrlpredicate.Funcs{
					CreateFunc: func(e ctrlevent.CreateEvent) bool {
						return false
					},
				}),
			),
		).
		Watches(
			// Watch Namespaces and enqueue the corresponding ClusterQueue.
			&core.Namespace{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(
				5*time.Second,
				r.enqueueClusterQueueWhenNamespaceCreated,
			),
			ctrlbuilder.WithPredicates(
				// Interested in Namespace objects:
				// - created.
				ctrlpredicate.Not(ctrlpredicate.Funcs{
					CreateFunc: func(e ctrlevent.CreateEvent) bool {
						return false
					},
				}),
			),
		).
		Complete(r)
}

func (r *LocalQueueReconciler) enqueueClusterQueueWhenNamespaceCreated(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx)

	ns := obj.(*core.Namespace)

	// Skip if deleted.
	if ns.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted namespace")
		return nil
	}

	// Skip if system namespace.
	if ns.Name == kuberess.SystemNamespaceName {
		logger.V(3).Info("skip system namespace")
		return nil
	}

	cqList := new(kueue.ClusterQueueList)
	err := r.Client.List(ctx, cqList,
		ctrlclix.NonQuorum,
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list cluster queues")
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(cqList.Items))
	for i := range cqList.Items {
		cq := &cqList.Items[i]

		// Skip if ClusterQueue is terminating.
		if cq.DeletionTimestamp != nil {
			continue
		}

		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{
				Name: cq.Name,
			},
		})
	}
	logger.V(2).Info("enqueue cluster queue for namespace", "requests", reqs)
	return reqs
}
