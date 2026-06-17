package worker

import (
	"context"
	"fmt"
	"strconv"
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
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
)

// ResourceFlavorReconciler reconciles kueue.ResourceFlavor objects keyed by their own name,
// driven by both ResourceFlavor and Kubernetes Node changes, to finish the following tasks:
//   - When a Node still uses the flavor's profile, (re)build the kueue.ResourceFlavor from
//     the Node and clear the drain mark.
//   - When no Node references the flavor's profile, never delete it; mark it draining
//     (a zero-quota tombstone) so the downstream ClusterQueue can drain gracefully.
//
// Watching ResourceFlavor with For(...) means a full resync on start-up re-evaluates every
// flavor, so orphans left behind by a profile-name switch are drained even though no Node
// event would ever enqueue them.
type ResourceFlavorReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*ResourceFlavorReconciler)(nil)

const (
	// ScheduleLabelPrefix prefixes the schedule label/annatation keys.
	ScheduleLabelPrefix = "schedule." + systemname.LabelPrefix

	// _ResourceFlavorQueueNameAnnoKey is for the queue name of a resource flavor,
	// whose value represents the queue that the resource flavor belongs to.
	//
	// NB: annotations rather than labels, because the queue/cohort names
	// carry the general(CPU) and acceleratable(device) keys and may exceed
	// the 63-character label value limit.
	_ResourceFlavorQueueNameAnnoKey = ScheduleLabelPrefix + "queue"
	// _ResourceFlavorCohortNameAnnoKey is for the cohort name of a resource flavor,
	// whose value represents the cohort that the resource flavor's queue longs to.
	_ResourceFlavorCohortNameAnnoKey = ScheduleLabelPrefix + "cohort"
	// _ResourceFlavorDrainAnnoKey marks a ResourceFlavor that no longer has any
	// associated Node and is being drained. The flavor is never deleted; it is
	// kept as a zero-quota tombstone, and the annotation is removed once a Node
	// starts using the flavor's profile again.
	_ResourceFlavorDrainAnnoKey = ScheduleLabelPrefix + "drain" // value "true"
)

func (r *ResourceFlavorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	rf := new(kueue.ResourceFlavor)
	err := r.Client.Get(ctx, req.NamespacedName, rf)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch resource flavor")
			return ctrl.Result{}, err
		}
		// Going to create a new flavor if needed.
		rf = nil
	}

	// Skip if deleted.
	if rf != nil && rf.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted resource flavor")
		return ctrl.Result{}, nil
	}

	// List the Nodes still using this flavor's profile.
	ndList := new(core.NodeList)
	err = r.Client.List(ctx, ndList,
		ctrlcli.MatchingFields{IndexingNodeByFlavorProfile: req.Name},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list nodes by flavor profile")
		return ctrl.Result{}, err
	}

	if len(ndList.Items) == 0 {
		// No Node uses the profile: a flavor that does not exist yet is a no-op;
		// an existing flavor is kept as a zero-quota tombstone and marked draining.
		if rf == nil {
			logger.V(3).Info("resource flavor not found and unused, skip")
			return ctrl.Result{}, nil
		}
		if rf.Annotations[_ResourceFlavorDrainAnnoKey] == "true" {
			logger.V(3).Info("orphaned resource flavor already draining")
			return ctrl.Result{}, nil
		}
		if rf.Annotations == nil {
			rf.Annotations = make(map[string]string)
		}
		rf.Annotations[_ResourceFlavorDrainAnnoKey] = "true"
		err = r.Client.Update(ctx, rf)
		if err != nil {
			logger.Error(err, "mark orphaned resource flavor draining")
			return ctrl.Result{}, err
		}
		logger.V(2).Info("marked orphaned resource flavor draining")
		return ctrl.Result{}, nil
	}

	// Active: build (or rebuild) the flavor from a matching Node and clear the drain mark.
	nd := &ndList.Items[0]
	var ndf *nodefeature.NodeResourceFlavor
	for _, candidate := range nodefeature.ExtractNodeResourceFlavors(nd) {
		if candidate.ProfileFlavor == req.Name {
			c := candidate
			ndf = &c
			break
		}
	}
	if ndf == nil {
		// Defensive: the index or labels may be transiently inconsistent.
		logger.V(2).Info("matched node carries no flavor for this profile, skip")
		return ctrl.Result{}, nil
	}

	eRf := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name: ndf.ProfileFlavor,
			Annotations: map[string]string{
				_ResourceFlavorQueueNameAnnoKey:  ndf.ProfileQueue,
				_ResourceFlavorCohortNameAnnoKey: ndf.ProfileCohort,
			},
		},
		Spec: kueue.ResourceFlavorSpec{
			NodeLabels:  ndf.NodeLabels,
			Tolerations: ndf.Tolerations,
		},
	}
	eNotes := map[string]string{
		"acceleratable": strconv.FormatBool(ndf.Acceleratable),
		"manufacturer":  ndf.Manufacturer,
		"accelerator":   ndf.Accelerator,
		"cpu":           ndf.CPU,
		"ram":           ndf.RAM,
		"localStorage":  ndf.LocalStorage,
	}
	systemmeta.NoteResource(eRf, "nodes", eNotes)
	rfAlignFn := func(aRf *kueue.ResourceFlavor) (_ *kueue.ResourceFlavor, skip bool, err error) {
		skip = true
		// Clear the drain mark: a Node uses this profile again.
		if _, ok := aRf.Annotations[_ResourceFlavorDrainAnnoKey]; ok {
			delete(aRf.Annotations, _ResourceFlavorDrainAnnoKey)
			skip = false
		}
		// Update annotations.
		if !mapx.Contain(aRf.Annotations, eRf.Annotations) {
			if aRf.Annotations == nil {
				aRf.Annotations = make(map[string]string)
			}
			for k, v := range eRf.Annotations {
				aRf.Annotations[k] = v
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
	_, err = kubeclientset.CreateWithCtrlClient(ctx, r.Client, eRf,
		kubeclientset.WithUpdateIfExisted(rfAlignFn))
	if err != nil {
		logger.Error(err, "sync resource flavor")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("synced resource flavor")
	return ctrl.Result{}, nil
}

const (
	IndexingNodeByFlavorProfile = "nodes.labels['feature.gpustack.ai/*.z-flavor']"
)

// indexNodeByFlavorProfile is the field-index extractor for
// IndexingNodeByFlavorProfile: it maps a managed Node to the flavor profile
// names it currently uses. Nodes that are being deleted or are not managed are
// excluded, so they drop out of the index — which is exactly how the reconciler
// detects that a flavor has no Node left (nodes == 0 → drain).
func indexNodeByFlavorProfile(obj ctrlcli.Object) []string {
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
}

func (r *ResourceFlavorReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &core.Node{}, IndexingNodeByFlavorProfile, indexNodeByFlavorProfile)
	if err != nil {
		return fmt.Errorf("index node '%s': %w", IndexingNodeByFlavorProfile, err)
	}

	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("resourceflavor").
		For(
			// Reconcile relevant ResourceFlavor objects keyed by their own name.
			// A full resync on start-up re-evaluates every flavor, so orphans left
			// behind by a profile-name switch get drained even though no Node event
			// would ever enqueue them.
			&kueue.ResourceFlavor{},
			ctrlbuilder.WithPredicates(
				// Interested in relevant ResourceFlavor objects.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "nodes")
				}),
				// Trigger reconciliation when a ResourceFlavor is:
				// - created (incl. the start-up resync).
				// - updated if its spec, schedule annotations (queue/cohort/drain)
				//   or notes have changed.
				// Never react to deletion: the reconciler never deletes a flavor.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldRf, newRf := e.ObjectOld.(*kueue.ResourceFlavor), e.ObjectNew.(*kueue.ResourceFlavor)
						if newRf.DeletionTimestamp == nil {
							// Fire when spec has changed.
							if !kubemeta.DeepEqual(oldRf.Spec, newRf.Spec) {
								return true
							}
							// Fire when annotations have changed.
							if !mapx.EqualWithKey(oldRf.Annotations, newRf.Annotations,
								_ResourceFlavorQueueNameAnnoKey,
								_ResourceFlavorCohortNameAnnoKey,
								_ResourceFlavorDrainAnnoKey) {
								return true
							}
							// Fire when notes have changed.
							if !systemmeta.EqualResourceTypeAndNotes(oldRf, newRf) {
								return true
							}
						}
						return false
					},
				},
			),
		).
		Watches(
			// Watch Nodes and enqueue the corresponding ResourceFlavor by profile.
			&core.Node{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(
				5*time.Second,
				r.enqueueResourceFlavorWhenNodeChanged,
			),
			ctrlbuilder.WithPredicates(
				// Trigger reconciliation when a Node is:
				// - created.
				// - deleted (so a flavor losing its last Node gets drained).
				// - updated if its managed mark, feature labels or taints have
				//   changed (a node leaving management drains its orphaned flavors).
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp == nil {
							// Fire when the managed mark or feature labels have changed.
							if !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								systemname.ManagedLabelKey,
								nodefeature.FeatureLabelPrefix,
								nodefeature.GeneralFeatureLabelPrefix,
								nodefeature.AcceleratableFeatureLabelPrefix) {
								return true
							}
							// Fire when taints have changed.
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

func (r *ResourceFlavorReconciler) enqueueResourceFlavorWhenNodeChanged(
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
		if profiles[i].Flavor == "" {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{
				Name: profiles[i].Flavor,
			},
		})
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueue resource flavor from node", "requests", reqs)
	return reqs
}
