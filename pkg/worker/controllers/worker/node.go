package worker

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/multierr"
	core "k8s.io/api/core/v1"
	kresource "k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/util/resource"

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

// NodeReconciler reconciles the Kubernetes Node object.
// and manages corresponding ResourceFlavor.
type NodeReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeReconciler)(nil)

const (
	_ResourceFlavorNodeNameLabelKey   = core.LabelHostname
	_ResourceFlavorCohortNameLabelKey = devicefeature.DeviceLabelPrefix + "cohort"
)

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
		// The Node is being deleted,
		// the corresponding ResourceFlavors will be deleted by garbage collection.
		return ctrl.Result{}, nil
	}

	ndKeys := devicefeature.ExtractNodeKeys(nd)
	if len(ndKeys) == 0 {
		return ctrl.Result{}, nil
	}
	logger.V(2).Info("reconcile node", "keys", ndKeys)

	// Fetch existing items.
	aResFlvsIndex := map[string]*kueue.ResourceFlavor{} // resource flavor name -> resource flavor
	{
		aResFlvList := new(kueue.ResourceFlavorList)
		err = r.Client.List(ctx, aResFlvList,
			ctrlcli.MatchingLabels{
				_ResourceFlavorNodeNameLabelKey: nd.Name,
			})
		if err != nil {
			logger.Error(err, "list resource flavors with node label")
			return ctrl.Result{}, err
		}
		for i := range aResFlvList.Items {
			if aResFlvList.Items[i].DeletionTimestamp != nil {
				continue
			}
			aResFlvsIndex[aResFlvList.Items[i].Name] = &aResFlvList.Items[i]
		}
	}

	// Prepare expected items and their names,
	// if the node is labeled with "gpustack.ai/managed: false", no resource flavor will be expected.
	eResFlvsIndex := map[string]*kueue.ResourceFlavor{} // resource flavor name -> resource flavor
	if !kubemeta.IsLabeled(nd, systemname.ManagedLabelKey, "false") {
		for _, ndKey := range ndKeys {
			ndf := devicefeature.ExtractNodeFeatureByKey(nd, ndKey)
			// Make same device below in the same cohort,
			// so that they can be scheduled together if needed.
			cohortName := "gpustack-" + ndKey
			eResFlv := &kueue.ResourceFlavor{
				ObjectMeta: meta.ObjectMeta{
					Name: strings.ToLower(nd.Name + "-gpustack-" + ndKey),
					Labels: map[string]string{
						_ResourceFlavorNodeNameLabelKey:   nd.Name,
						_ResourceFlavorCohortNameLabelKey: cohortName,
					},
				},
				Spec: kueue.ResourceFlavorSpec{
					NodeLabels:  ndf.NodeLabels,
					Tolerations: ndf.Tolerations,
				},
			}
			kubemeta.ControlOnWithoutBlock(eResFlv, nd, core.SchemeGroupVersion.WithKind("Node"))
			// NB(thxCode): Use to trigger configuring ClusterQueue automatically.
			clusterQueueNamePrefix := cohortName
			if ndKey != devicefeature.DisfeaturedNodeKey {
				// For featured nodes, use resource flavor name to indicate the resource type and amount,
				// so that the corresponding ClusterQueue can be easily identified and configured.
				unitCPU := kresource.MustParse(ndf.UnitResources.CPU)
				unitRAM := kresource.MustParse(ndf.UnitResources.RAM)
				clusterQueueNamePrefix += strings.ToLower(fmt.Sprintf("-%sc-%s",
					unitCPU.String(), unitRAM.String()))
			}
			systemmeta.NoteResource(eResFlv, "nodes", map[string]string{
				"clusterqueue-generate-name": funcx.TernaryFunc(
					func() bool { return ndf.Sliced == "" },
					func() string { return clusterQueueNamePrefix + "-" },
					func() string { return clusterQueueNamePrefix + "-sliced-" + ndf.Sliced + "-" },
				),
				"accelerators": ndf.Accelerator.String(),
			})
			eResFlvsIndex[eResFlv.Name] = eResFlv
		}
	}

	var errs []error

	// Update existing items and delete unexpected items.
	for resFlvName, aResFlv := range aResFlvsIndex {
		// Update items that are expected.
		if eResFlv, found := eResFlvsIndex[resFlvName]; found {
			delete(eResFlvsIndex, resFlvName)
			if !kubemeta.DeepEqual(aResFlv.Labels, eResFlv.Labels) ||
				!kubemeta.DeepEqual(aResFlv.Annotations, eResFlv.Annotations) ||
				!kubemeta.DeepEqual(aResFlv.Spec, eResFlv.Spec) ||
				!kubemeta.IsControlledBy(aResFlv, nd) {
				aResFlv.Labels = eResFlv.Labels
				aResFlv.Annotations = eResFlv.Annotations
				aResFlv.Spec = eResFlv.Spec
				kubemeta.ControlOnWithoutBlock(aResFlv, nd, core.SchemeGroupVersion.WithKind("Node"))
				if err = r.Client.Update(ctx, aResFlv); err != nil {
					logger.Error(err, "update resource flavor", "name", resFlvName)
					errs = append(errs, err)
					continue
				}
				logger.V(2).Info("update resource flavor", "name", resFlvName)
			}
			continue
		}

		// Delete items that are not expected.
		if err = r.Client.Delete(ctx, aResFlv); err != nil {
			logger.Error(err, "delete resource flavor", "name", resFlvName)
			errs = append(errs, err)
			continue
		}
		logger.V(2).Info("delete resource flavor", "name", resFlvName)
	}

	// Create items that are expected but not existing.
	for resFlvName := range eResFlvsIndex {
		eResFlv := eResFlvsIndex[resFlvName]
		if err = r.Client.Create(ctx, eResFlv); err != nil {
			logger.Error(err, "create resource flavor", "name", resFlvName)
			errs = append(errs, err)
			continue
		}
		logger.V(2).Info("create resource flavor", "name", resFlvName)
	}

	if len(errs) > 0 {
		return ctrl.Result{}, multierr.Combine(errs...)
	}
	return ctrl.Result{}, nil
}

func (r *NodeReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("worker.manage.nodes").
		For(
			// Focus on the Kubernetes Node,
			// when the Node is updated with labels, taints or allocatable changes.
			&core.Node{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.GetDeletionTimestamp() == nil {
							// Check if labels or taints have changed.
							if !kubemeta.DeepEqual(oldNd.Spec.Taints, newNd.Spec.Taints) ||
								!kubemeta.DeepEqual(oldNd.Labels, newNd.Labels) {
								return true
							}
							// Check if extended resources have changed.
							for cn := range newNd.Status.Capacity {
								if !resource.IsExtendedResourceName(cn) {
									continue
								}
								if !oldNd.Status.Capacity[cn].Equal(newNd.Status.Capacity[cn]) {
									return true
								}
								if !oldNd.Status.Allocatable[cn].Equal(newNd.Status.Allocatable[cn]) {
									return true
								}
							}
						}
						return false
					},
				},
			),
		).
		Complete(r)
}
