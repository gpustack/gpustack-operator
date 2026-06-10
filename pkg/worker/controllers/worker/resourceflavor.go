package worker

import (
	"context"
	"fmt"
	"strconv"

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
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
)

// ResourceFlavorReconciler reconciles kueue.ResourceFlavor objects driven by Kubernetes Node changes to finish the following tasks:
//   - When a Node's labels or taints are updated,
//     create/update the kueue.ResourceFlavor derived from the Node's device profile.
type ResourceFlavorReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*ResourceFlavorReconciler)(nil)

const (
	// The label key for the queue name of a resource flavor,
	// whose value represents the queue that the resource flavor belongs to.
	_ResourceFlavorQueueNameLabelKey = nodefeature.DeviceLabelPrefix + "queue"
	// The label key for the cohort name of a resource flavor,
	// whose value represents the cohort that the resource flavor's queue longs to.
	_ResourceFlavorCohortNameLabelKey = nodefeature.DeviceLabelPrefix + "cohort"
)

func (r *ResourceFlavorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	var errs []error

	ndfs := nodefeature.ExtractNodeResourceFlavors(nd)
	for _, ndf := range ndfs {
		// "gpustack-${key}-${profile-flavor}"
		flavorProfile := nodefeature.FormatNodeProfile(ndf.Key, ndf.ProfileFlavorSpec)
		// "gpustack-${key}-${profile-queue}"
		queueProfile := nodefeature.FormatNodeProfile(ndf.Key, ndf.ProfileQueueSpec)
		// "gpustack-${key}-${profile-cohort}"
		cohortProfile := nodefeature.FormatNodeProfile(ndf.Key, ndf.ProfileCohortSpec)
		eRf := &kueue.ResourceFlavor{
			ObjectMeta: meta.ObjectMeta{
				Name: flavorProfile,
				Labels: map[string]string{
					_ResourceFlavorQueueNameLabelKey:  queueProfile,
					_ResourceFlavorCohortNameLabelKey: cohortProfile,
				},
			},
			Spec: kueue.ResourceFlavorSpec{
				NodeLabels:  ndf.NodeLabels,
				Tolerations: ndf.Tolerations,
			},
		}
		eNotes := map[string]string{
			"acceleratable":     strconv.FormatBool(ndf.Acceleratable),
			"manufacturer":      ndf.Manufacturer,
			"product":           ndf.Product,
			"memory":            ndf.Memory,
			"cores":             ndf.Cores,
			"family":            ndf.Family,
			"computeCapability": ndf.ComputeCapability,
			"accelerator":       ndf.Accelerator,
			"cpu":               ndf.CPU,
			"ram":               ndf.RAM,
			"localStorage":      ndf.LocalStorage,
		}
		systemmeta.NoteResource(eRf, "nodes", eNotes)
		rfAlignFn := func(aRf *kueue.ResourceFlavor) (_ *kueue.ResourceFlavor, skip bool, err error) {
			skip = true
			// Update labels.
			if !mapx.Contain(aRf.Labels, eRf.Labels) {
				if aRf.Labels == nil {
					aRf.Labels = make(map[string]string)
				}
				for k, v := range eRf.Labels {
					aRf.Labels[k] = v
				}
				skip = false
			}
			// Update spec.
			if !kubemeta.DeepEqual(aRf.Spec, eRf.Spec) {
				aRf.Spec = eRf.Spec
				skip = false
			}
			// Update notes.
			if !systemmeta.EqualResourceTypeAndNotes(aRf, eRf) {
				systemmeta.NoteResource(aRf, "nodes", eNotes)
				skip = false
			}
			return aRf, skip, nil
		}
		_, err := kubeclientset.CreateWithCtrlClient(ctx, r.Client, eRf,
			kubeclientset.WithUpdateIfExisted(rfAlignFn))
		if err != nil {
			logger.Error(err, "sync resource flavor")
			errs = append(errs, err)
			continue
		}
		logger.V(2).Info("synced resource flavor")
	}

	return ctrl.Result{}, multierr.Combine(errs...)
}

const (
	IndexingNodeByFlavorProfile = "nodes.labels['feature.gpustack.ai/*.profile-flavor']"
)

func (r *ResourceFlavorReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &core.Node{}, IndexingNodeByFlavorProfile,
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
				return p.Flavor
			})
		})
	if err != nil {
		return fmt.Errorf("index node '%s': %w", IndexingNodeByFlavorProfile, err)
	}

	r.Client = opts.Manager.GetClient()

	aggressive := opts.Manager.AllowAggressiveEventFiltering()
	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("resourceflavor").
		For(
			&core.Node{},
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a Node is:
				// - created.
				// - updated if labels or taints have changed.
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
							// Check if labels have changed.
							if !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								nodefeature.FeatureLabelPrefix) {
								return true
							}
							// Check if taints have changed.
							if !kubemeta.DeepEqual(oldNd.Spec.Taints, newNd.Spec.Taints) {
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
