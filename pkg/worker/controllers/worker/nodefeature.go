package worker

import (
	"context"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	nfd "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// NodeFeatureReconciler reconciles all Kubernetes Node objects to finish the following tasks:
//   - When the labels or capacities of a Node are updated,
//     create/update corresponding nfd.NodeResourceFlavor.
type NodeFeatureReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeFeatureReconciler)(nil)

func (r *NodeFeatureReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	nd := new(core.Node)
	err := r.Client.Get(ctx, req.NamespacedName, nd)
	if err != nil {
		logger.Error(err, "fetch node")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Skip if deleted.
	if nd.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted node")
		return ctrl.Result{}, nil
	}

	eNf := &nfd.NodeFeature{
		ObjectMeta: meta.ObjectMeta{
			Name:      nd.Name + "-gpustack-worker",
			Namespace: kuberess.SystemNamespaceName,
			Labels: map[string]string{
				nfd.NodeFeatureObjNodeNameLabel: nd.Name,
			},
		},
		Spec: func() nfd.NodeFeatureSpec {
			nfs := nfd.NewNodeFeatureSpec()
			nfs.Labels = devicefeature.ConstructNodeCapacityLabels(nd)
			return *nfs
		}(),
	}
	kubemeta.ControlOnWithoutBlock(eNf, nd, core.SchemeGroupVersion.WithKind("Node"))
	nfAlignFn := func(aNf *nfd.NodeFeature) (_ *nfd.NodeFeature, skip bool, err error) {
		skip = true
		if aNf.Labels == nil {
			aNf.Labels = make(map[string]string)
		}
		if aNf.Spec.Labels == nil {
			aNf.Spec.Labels = make(map[string]string)
		}
		// Update labels.
		for k, v := range eNf.Labels {
			if aNf.Labels[k] != v {
				aNf.Labels[k] = v
				skip = false
			}
		}
		// Update spec.
		if !kubemeta.DeepEqual(aNf.Spec, eNf.Spec) {
			aNf.Spec = eNf.Spec
			skip = false
		}
		// Update owner reference.
		if !kubemeta.IsControlledBy(aNf, nd) {
			kubemeta.ControlOnWithoutBlock(aNf, nd, core.SchemeGroupVersion.WithKind("Node"))
			skip = false
		}
		return aNf, skip, nil
	}
	_, err = kubeclientset.CreateWithCtrlClient(ctx, r.Client, eNf,
		kubeclientset.WithUpdateIfExisted(nfAlignFn))
	if err != nil {
		logger.Error(err, "sync node feature")
	}
	return ctrl.Result{}, err
}

func (r *NodeFeatureReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodefeature").
		For(
			&core.Node{},
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a Node is:
				// - created.
				// - updated if labels or external capacities have changed.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp == nil {
							// Check if labels have changed.
							if !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								devicefeature.FeatureLabelPrefix) {
								return true
							}
							// Check if capacities have changed.
							for cn := range newNd.Status.Capacity {
								switch cn {
								default:
									continue
								case core.ResourceCPU:
								case core.ResourceMemory:
								case core.ResourceEphemeralStorage:
									if !oldNd.Status.Capacity[cn].Equal(newNd.Status.Capacity[cn]) {
										return true
									}
								}
							}
						}
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
