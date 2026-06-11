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
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// NodeFeatureReconciler reconciles nfd.NodeFeature objects driven by Kubernetes Node changes to finish the following tasks:
//   - When a Node's labels are updated,
//     create/update the corresponding nfd.NodeFeature.
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
				"app.kubernetes.io/part-of":     "gpustack-operator-worker",
			},
		},
		Spec: func() nfd.NodeFeatureSpec {
			nfs := nfd.NewNodeFeatureSpec()
			nfs.Labels = nodefeature.ConstructNodeCapacityLabels(nd, nodefeature.OverrideGeneralRAMGiPerCPU(2))
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
			logger.V(2).Info("node feature spec changed, need update",
				"old", aNf.Spec.Labels,
				"new", eNf.Spec.Labels)
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
		return ctrl.Result{}, err
	}
	logger.V(2).Info("synced node feature")
	return ctrl.Result{}, nil
}

func (r *NodeFeatureReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	aggressive := opts.Manager.AllowAggressiveEventFiltering()
	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodefeature").
		For(
			&core.Node{},
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a Node is:
				// - created.
				// - updated if labels have changed.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						if aggressive {
							return e.ObjectNew.GetDeletionTimestamp() == nil
						}

						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp == nil {
							// Don't need to consider the changes of
							// - "feature.node.kubernetes.io/cpu-model.*" labels
							// - "feature.gpustack.ai/cpu-*" annotations,
							// As they are managed by NFD and stable enough.
							if !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								systemname.ManagedLabelKey,
								nodefeature.FeatureLabelPrefix,
								nodefeature.GeneralFeatureLabelPrefix,
								nodefeature.AcceleratableFeatureLabelPrefix) {
								return true
							}
						}
						return false
					},
				},
			),
		).
		Complete(r)
}
