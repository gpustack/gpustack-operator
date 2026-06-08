package worker

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"gpustack.ai/gpustack/pkg/devicefeature"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
)

// ClusterQueueReconciler reconciles kueue.ClusterQueue objects driven by kueue.ResourceFlavor
// and Kubernetes Node changes to finish the following tasks:
//   - When a ResourceFlavor is created/deleted, or a Node is created/deleted or its feature labels are updated,
//     construct the kueue.ClusterQueue by aggregating all ResourceFlavors that share the same queue name,
//     or delete the kueue.ClusterQueue when no ResourceFlavor references the queue.
type ClusterQueueReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

var _ ctrlreconcile.Reconciler = (*ClusterQueueReconciler)(nil)

func (r *ClusterQueueReconciler) Reconcile(ctx context.Context, req ctrlreconcile.Request) (ctrlreconcile.Result, error) {
	cohortName, queueName := req.Namespace, req.Name

	logger := ctrllog.FromContext(ctx)

	// Fetch ResourceFlavor objects with same queue name.
	rfList := new(kueue.ResourceFlavorList)
	err := r.Client.List(ctx, rfList,
		ctrlcli.MatchingFields{IndexingResourceFlavorsByQueueName: queueName})
	if err != nil {
		logger.Error(err, "fetch resource flavors by queue name")
		return ctrlreconcile.Result{}, err
	}

	// Remove ClusterQueue if no ResourceFlavor.
	if len(rfList.Items) == 0 {
		cq := &kueue.ClusterQueue{
			ObjectMeta: meta.ObjectMeta{
				Name: queueName,
			},
		}
		err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: queueName}, cq)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				logger.Error(err, "fetch cluster queue")
				return ctrlreconcile.Result{}, err
			}
			return ctrlreconcile.Result{}, nil
		}
		var reserved bool
		for i := range cq.Status.FlavorsReservation {
			fr := &cq.Status.FlavorsReservation[i]
			for j := range fr.Resources {
				if !fr.Resources[j].Total.IsZero() ||
					!fr.Resources[j].Borrowed.IsZero() {
					reserved = true
					break
				}
			}
			if reserved {
				break
			}
		}
		if reserved {
			logger.V(2).Info("cluster queue still has reserved resources; requeue in 15s")
			return ctrlreconcile.Result{RequeueAfter: 15 * time.Second}, nil
		}
		err = r.Client.Delete(ctx, cq)
		if err != nil {
			if !kerrors.IsNotFound(err) {
				logger.Error(err, "delete cluster queue")
				return ctrlreconcile.Result{}, err
			}
		}
		logger.V(2).Info("deleted cluster queue: no resource flavors")
		return ctrlreconcile.Result{}, nil
	}

	// Sort ResourceFlavors by creation time to make sure the constructed ClusterQueue is stable.
	sort.Slice(rfList.Items, func(i, j int) bool {
		if rfList.Items[i].CreationTimestamp.Equal(&rfList.Items[j].CreationTimestamp) {
			return rfList.Items[i].Name < rfList.Items[j].Name
		}
		return rfList.Items[i].CreationTimestamp.Before(&rfList.Items[j].CreationTimestamp)
	})

	// Fetch Cohort.
	co := new(kueue.Cohort)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: cohortName}, co,
		ctrlclix.WithoutQuorum,
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "fetch cohort")
		return ctrlreconcile.Result{}, err
	}

	// Skip if Cohort is being deleted.
	if co.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted cohort")
		return ctrlreconcile.Result{}, nil
	}

	// Sync ClusterQueue.
	eResGroups, eNotes := r.constructResourceGroups(ctx, rfList)
	if len(eResGroups) == 0 {
		logger.Error(nil, "no valid resource flavors, retry later")
		return ctrlreconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}

	eCq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{
			Name: queueName,
		},
		Spec: kueue.ClusterQueueSpec{
			NamespaceSelector: &meta.LabelSelector{},
			CohortName:        kueue.CohortReference(cohortName), // Updated if changed.
			// Prefer using nominal quota instead of borrowing.
			FlavorFungibility: &kueue.FlavorFungibility{
				WhenCanBorrow:  kueue.TryNextFlavor,
				WhenCanPreempt: kueue.MayStopSearch,
			},
			// Never preempt within cohort.
			Preemption: &kueue.ClusterQueuePreemption{
				ReclaimWithinCohort: kueue.PreemptionPolicyNever,
				BorrowWithinCohort: &kueue.BorrowWithinCohort{
					Policy: kueue.BorrowWithinCohortPolicyNever,
				},
				WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
			},
			ResourceGroups: eResGroups, // Updated if changed.
		},
	}
	kubemeta.ControlOnWithoutBlock(eCq, co, kueue.SchemeGroupVersion.WithKind("Cohort"))
	systemmeta.NoteResource(eCq, "instancetypes", eNotes)
	cqAlignFn := func(aCq *kueue.ClusterQueue) (_ *kueue.ClusterQueue, skip bool, err error) {
		skip = true
		// Update labels.
		if !mapx.Contain(aCq.Labels, eCq.Labels) {
			if aCq.Labels == nil {
				aCq.Labels = make(map[string]string)
			}
			for k, v := range eCq.Labels {
				aCq.Labels[k] = v
			}
			skip = false
		}
		// Update spec.
		if !kubemeta.DeepEqual(aCq.Spec, eCq.Spec) {
			aCq.Spec = eCq.Spec
			skip = false
		}
		// Update notes.
		if !systemmeta.EqualResourceTypeAndNotes(aCq, eCq) {
			systemmeta.NoteResource(aCq, "instancetypes", eNotes)
			skip = false
		}
		// Update owner reference.
		if !kubemeta.IsControlledBy(aCq, co) {
			kubemeta.ControlOnWithoutBlock(aCq, co, kueue.SchemeGroupVersion.WithKind("Cohort"))
			skip = false
		}
		return aCq, skip, nil
	}
	_, err = kubeclientset.CreateWithCtrlClient(ctx, r.Client, eCq,
		kubeclientset.WithUpdateIfExisted(cqAlignFn))
	if err != nil {
		logger.Error(err, "sync cluster queue")
		return ctrlreconcile.Result{}, err
	}
	logger.V(2).Info("synced cluster queue")
	return ctrlreconcile.Result{}, nil
}

var commonResourceGroupNotesKeys = []string{
	"sliced",
	"manufacturer",
	"product",
	"memory",
	"cores",
	"family",
	"computeCapability",
}

func (r *ClusterQueueReconciler) constructResourceGroups(
	ctx context.Context, rfList *kueue.ResourceFlavorList,
) (groups []kueue.ResourceGroup, notes map[string]string) {
	logger := ctrllog.FromContext(ctx)

	var borLimit *resource.Quantity

	for i := range rfList.Items {
		rf := &rfList.Items[i]

		logger := logger.WithValues("resource flavor", ctrlcli.ObjectKeyFromObject(rf))

		resType, rfNotes := systemmeta.DescribeResource(rf)
		if resType != "nodes" {
			logger.V(2).Info("skip resource flavor with mismatched resource type",
				"resource type", resType)
			continue
		}

		var nodeCount int64
		{
			ndList := new(core.NodeList)
			err := r.Client.List(ctx, ndList,
				ctrlcli.MatchingFields{IndexingNodeByFlavorProfile: rf.Name},
				ctrlcli.UnsafeDisableDeepCopy)
			if err != nil {
				logger.Error(err, "fetch nodes by flavor profile")
				continue
			}
			nodeCount = int64(len(ndList.Items))
		}

		manu := rfNotes["manufacturer"]

		var cpuQ, ramQ, lsQ, accQ resource.Quantity
		{
			cpuQ = funcx.NoError(resource.ParseQuantity(rfNotes["cpu"]))
			if cpuQ.Sign() <= 0 {
				logger.V(2).Info("skip resource flavor with non-positive cpu",
					"cpu", rfNotes["cpu"])
				continue
			}
			ramQ = funcx.NoError(resource.ParseQuantity(rfNotes["ram"]))
			if ramQ.Sign() <= 0 {
				logger.V(2).Info("skip resource flavor with non-positive ram",
					"ram", rfNotes["ram"])
				continue
			}
			lsQ = funcx.NoError(resource.ParseQuantity(rfNotes["localStorage"]))
			if lsQ.Sign() <= 0 {
				logger.V(2).Info("skip resource flavor with non-positive local storage",
					"localStorage", rfNotes["localStorage"])
				continue
			}
			if manu != "" {
				accQ = funcx.NoError(resource.ParseQuantity(rfNotes["accelerator"]))
				if accQ.Sign() < 0 {
					logger.V(2).Info("skip resource flavor with non-positive accelerator",
						"accelerator", rfNotes["accelerator"])
					continue
				}
			}
		}

		// Construct notes.
		if len(notes) == 0 {
			notes = map[string]string{
				"acceleratable": strconv.FormatBool(manu != ""),
			}
			for _, k := range commonResourceGroupNotesKeys {
				notes[k] = rfNotes[k]
			}

			borLimit = funcx.Ternary(notes["sliced"] != "", resource.NewQuantity(0, resource.DecimalSI), nil)
		}

		if len(groups) == 0 {
			groups = []kueue.ResourceGroup{{}}
		}
		if len(groups[len(groups)-1].Flavors) >= 16 {
			groups = append(groups, kueue.ResourceGroup{})
		}

		// Fill resource group.
		rg := &groups[len(groups)-1]
		if len(rg.CoveredResources) == 0 {
			rg.CoveredResources = []core.ResourceName{
				core.ResourceCPU,
				core.ResourceMemory,
				core.ResourceEphemeralStorage,
			}
			if manu != "" {
				rg.CoveredResources = append(rg.CoveredResources,
					devicefeature.GetCreditsResourceName(manu),
				)
			}
		}
		rg.Flavors = append(rg.Flavors, kueue.FlavorQuotas{
			Name: kueue.ResourceFlavorReference(rf.Name),
			Resources: []kueue.ResourceQuota{
				{
					Name:           core.ResourceCPU,
					NominalQuota:   quantityx.Multiply(cpuQ, nodeCount),
					BorrowingLimit: borLimit,
				},
				{
					Name:           core.ResourceMemory,
					NominalQuota:   quantityx.Multiply(ramQ, nodeCount),
					BorrowingLimit: borLimit,
				},
				{
					Name:           core.ResourceEphemeralStorage,
					NominalQuota:   quantityx.Multiply(lsQ, nodeCount),
					BorrowingLimit: borLimit,
				},
			},
		})
		if manu != "" {
			rg.Flavors[len(rg.Flavors)-1].Resources = append(rg.Flavors[len(rg.Flavors)-1].Resources,
				kueue.ResourceQuota{
					Name:           devicefeature.GetCreditsResourceName(manu),
					NominalQuota:   quantityx.Multiply(accQ, nodeCount),
					BorrowingLimit: borLimit,
				},
			)
		}
	}

	return groups, notes
}

const (
	IndexingResourceFlavorsByQueueName = "resourceflavors.labels['device.gpustack.ai/queue']"
)

func (r *ClusterQueueReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &kueue.ResourceFlavor{}, IndexingResourceFlavorsByQueueName,
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}

			rf := obj.(*kueue.ResourceFlavor)
			if rf.DeletionTimestamp != nil {
				return nil
			}
			if !systemmeta.MatchResource(rf, "nodes") {
				return nil
			}
			if !kubemeta.HasLabels(rf,
				_ResourceFlavorQueueNameLabelKey,
				_ResourceFlavorCohortNameLabelKey) {
				return nil
			}

			return []string{
				rf.Labels[_ResourceFlavorQueueNameLabelKey],
			}
		})
	if err != nil {
		return fmt.Errorf("index resource flavor '%s': %w", IndexingResourceFlavorsByQueueName, err)
	}

	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()
	aggressive := opts.Manager.AllowAggressiveEventFiltering()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("clusterqueue").
		Watches(
			// Watch kueue.ResourceFlavors and enqueue the corresponding Cohort/ClusterQueue.
			&kueue.ResourceFlavor{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueCohortWhenResourceFlavorChanged,
			),
			ctrlbuilder.WithPredicates(
				// Interested in relevant ResourceFlavor objects.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "nodes")
				}),
				// Trigger reconciliation when a ResourceFlavor is:
				// - created.
				// - deleted.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return true
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldRf, newRf := e.ObjectOld.(*kueue.ResourceFlavor), e.ObjectNew.(*kueue.ResourceFlavor)
						if newRf.DeletionTimestamp == nil {
							return false
						}
						if oldRf.DeletionTimestamp == nil {
							return true
						}
						return !oldRf.DeletionTimestamp.Equal(newRf.DeletionTimestamp)
					},
					GenericFunc: func(e ctrlevent.GenericEvent) bool {
						return false
					},
				},
			),
		).
		Watches(
			// Watch Nodes and enqueue the corresponding Cohort/ClusterQueue.
			&core.Node{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueCohortWhenNodeChanged,
			),
			ctrlbuilder.WithPredicates(
				// Interested in Node objects:
				// - created.
				// - deleted.
				// - updated if labels have changed.
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						if aggressive {
							return true
						}

						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp == nil {
							return !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								systemname.LabelPrefix,
								devicefeature.FeatureLabelPrefix)
						}
						if oldNd.DeletionTimestamp == nil {
							return true
						}
						return !oldNd.DeletionTimestamp.Equal(newNd.DeletionTimestamp)
					},
				},
			),
		).
		Complete(r)
}

func (r *ClusterQueueReconciler) enqueueCohortWhenResourceFlavorChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("resource flavor", ctrlcli.ObjectKeyFromObject(obj))

	if !systemmeta.MatchResource(obj, "nodes") {
		logger.Error(nil, "mismatched resource type")
		return nil
	}

	// Ensure the ResourceFlavor has cohort and queue labels.
	if !kubemeta.HasLabels(obj,
		_ResourceFlavorCohortNameLabelKey,
		_ResourceFlavorQueueNameLabelKey) {
		logger.V(2).Info("missing cohort or queue label")
		return nil
	}

	rf := obj.(*kueue.ResourceFlavor)

	var reqs []ctrlreconcile.Request
	{
		cohortName := rf.Labels[_ResourceFlavorCohortNameLabelKey]
		queueName := rf.Labels[_ResourceFlavorQueueNameLabelKey]
		if cohortName != "" && queueName != "" {
			reqs = []ctrlreconcile.Request{
				{
					NamespacedName: ctrlcli.ObjectKey{
						Name:      queueName,
						Namespace: cohortName,
					},
				},
			}
		}
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueue cohort and queue from resource flavor", "requests", reqs)
	return reqs
}

func (r *ClusterQueueReconciler) enqueueCohortWhenNodeChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("node", ctrlcli.ObjectKeyFromObject(obj))

	nd := obj.(*core.Node)

	profiles := devicefeature.ExtractNodeProfiles(nd)
	if len(profiles) == 0 {
		logger.V(2).Info("node has no profile")
		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(profiles))
	for i := range profiles {
		if profiles[i].Queue == "" || profiles[i].Cohort == "" {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{
				Name:      profiles[i].Queue,
				Namespace: profiles[i].Cohort,
			},
		})
	}
	if len(reqs) == 0 {
		return nil
	}

	logger.V(2).Info("enqueue cohort and queue from node", "requests", reqs)
	return reqs
}
