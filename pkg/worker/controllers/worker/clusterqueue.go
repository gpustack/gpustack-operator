package worker

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/stringx"
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

	// Determine whether the queue should drain: either no ResourceFlavor backs
	// it, or every backing flavor is draining (no Node uses its profile).
	allDrained := slicex.All(rfList.Items, func(i int) bool {
		return rfList.Items[i].Annotations[_ResourceFlavorDrainAnnoKey] == "true"
	})

	// Drain and remove the ClusterQueue when it has no active ResourceFlavor.
	if len(rfList.Items) == 0 || allDrained {
		cq := &kueue.ClusterQueue{
			ObjectMeta: meta.ObjectMeta{
				Name: queueName,
			},
		}
		err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: queueName}, cq,
			ctrlclix.WithoutQuorum)
		if err != nil {
			// Maybe the ClusterQueue is not cached yet,
			// try to fetch directly from API server to avoid the cache staleness.
			if kerrors.IsNotFound(err) {
				err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: queueName}, cq,
					ctrlclix.WithoutQuorum)
			}
			if err != nil {
				logger.Error(err, "fetch cluster queue")
				return ctrlreconcile.Result{}, ctrlcli.IgnoreNotFound(err)
			}
		}

		// Skip if already being deleted.
		if cq.DeletionTimestamp != nil {
			logger.V(3).Info("skip deleted cluster queue")
			return ctrlreconcile.Result{}, nil
		}

		// Phase 1: switch the queue to HoldAndDrain so Kueue evicts admitted
		// workloads and cancels reservations. The ResourceGroups are frozen
		// as-is — the orphaned flavors carry ~zero quota, and recomputing would
		// only churn the spec and reset the StopPolicy.
		if ptr.Deref(cq.Spec.StopPolicy, kueue.None) != kueue.HoldAndDrain {
			cq.Spec.StopPolicy = ptr.To(kueue.HoldAndDrain)
			err = r.Client.Update(ctx, cq)
			if err != nil {
				logger.Error(err, "set cluster queue to hold and drain")
				return ctrlreconcile.Result{}, err
			}
			logger.V(2).Info("set cluster queue to hold and drain; requeue in 15s")
			return ctrlreconcile.Result{RequeueAfter: 15 * time.Second}, nil
		}

		// Phase 2: wait until Kueue has drained all reservations, then delete.
		if hasReserved(cq) {
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
		logger.V(2).Info("deleted drained cluster queue")
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
	eResGroups, eNotes := r.constructResourceGroups(ctx, queueName, rfList)
	eCq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{
			Name: queueName,
		},
		Spec: kueue.ClusterQueueSpec{
			NamespaceSelector: &meta.LabelSelector{},
			CohortName:        kueue.CohortReference(cohortName), // Updated if changed.
			// Active flavors back this queue: keep it running, lifting any
			// previous drain by forcing the StopPolicy back to None.
			StopPolicy: ptr.To(kueue.None),
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

// hasReserved reports whether the ClusterQueue still holds reserved quota or
// admitted/reserving workloads. The queue must not be deleted until Kueue has
// finished draining; the workload counters guard against the flavor
// reservation snapshot momentarily reading zero while eviction is still in
// flight. Pending workloads are intentionally not counted — they hold no
// reservation, and gating on them would block deletion forever.
func hasReserved(cq *kueue.ClusterQueue) bool {
	if cq.Status.ReservingWorkloads != 0 || cq.Status.AdmittedWorkloads != 0 {
		return true
	}
	return slicex.Any(cq.Status.FlavorsReservation, func(i int) bool {
		return slicex.Any(cq.Status.FlavorsReservation[i].Resources, func(j int) bool {
			return !cq.Status.FlavorsReservation[i].Resources[j].Total.IsZero() ||
				!cq.Status.FlavorsReservation[i].Resources[j].Borrowed.IsZero()
		})
	})
}

func (r *ClusterQueueReconciler) constructResourceGroups(
	ctx context.Context,
	queueName string,
	rfList *kueue.ResourceFlavorList,
) (groups []kueue.ResourceGroup, notes map[string]string) {
	generalKey, accKey, spec, ok := nodefeature.ParseNodeProfile(queueName)
	if !ok {
		return groups, notes
	}

	logger := ctrllog.FromContext(ctx)

	// Sliced queues never borrow.
	var (
		acceleratable bool
		manufacturer  string
		borLimit      *resource.Quantity
	)
	{
		acceleratable = accKey != "" && spec.Accelerator != ""
		if acceleratable {
			manufacturer, _, _ = strings.Cut(accKey, "-")
			if spec.SlicedAccelerator != "" {
				borLimit = resource.NewQuantity(0, resource.DecimalSI)
			}
		} else {
			manufacturer, _, _ = strings.Cut(generalKey, "-")
		}
	}

	for i := range rfList.Items {
		rf := &rfList.Items[i]

		resType, rfNotes := systemmeta.DescribeResource(rf)
		if resType != "nodes" {
			logger.V(2).Info("skip resource flavor with mismatched resource type",
				"resource flavor", rf.Name, "resource type", resType)
			continue
		}

		var cpuQ, ramQ, stgQ, accQ resource.Quantity
		{
			ndList := new(core.NodeList)
			err := r.Client.List(ctx, ndList,
				ctrlcli.MatchingFields{IndexingNodeByFlavorProfile: rf.Name},
				ctrlcli.UnsafeDisableDeepCopy)
			if err != nil {
				logger.Error(err, "fetch nodes by flavor profile",
					"flavor profile", rf.Name)
				continue
			}

			ndCount := int64(len(ndList.Items))
			if ndCount != 0 {
				// Construct notes from the first matched Node of the first-wins ResourceFlavor.
				if len(notes) == 0 {
					nq, ok := nodefeature.ExtractNodeQueue(&ndList.Items[0], accKey)
					if !ok {
						continue
					}
					notes = map[string]string{
						"acceleratable": strconv.FormatBool(acceleratable),
						"manufacturer":  manufacturer,
						"product":       nq.Product,
						"family":        nq.Family,
						"os":            nq.OS,
						"arch":          nq.Arch,
						"unitCPU":       spec.CPU,
						"unitRAM":       spec.RAM,
					}
					var detailBs []byte
					if acceleratable {
						detailBs = json.ShouldMarshal(nq.NodeResourceFlavorAccelerator)
						if spec.SlicedAccelerator != "" {
							notes["slicedAccelerator"] = spec.SlicedAccelerator
						}
					} else {
						detailBs = json.ShouldMarshal(nq.NodeResourceFlavorCPU)
					}
					notes["detail"] = stringx.FromBytes(&detailBs)
				}

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
				stgQ = funcx.NoError(resource.ParseQuantity(rfNotes["localStorage"]))
				if stgQ.Sign() <= 0 {
					logger.V(2).Info("skip resource flavor with non-positive local storage",
						"localStorage", rfNotes["localStorage"])
					continue
				}

				cpuQ = quantityx.Multiply(cpuQ, ndCount)
				ramQ = quantityx.Multiply(ramQ, ndCount)
				stgQ = quantityx.Multiply(stgQ, ndCount)
				if acceleratable {
					accResName := nodefeature.GetAcceleratableResourceName(manufacturer, workercore.DeviceAllocationModeExclusive)
					for j := range ndList.Items {
						accQ.Add(ndList.Items[j].Status.Allocatable[accResName])
					}
				}
			}
		}

		// Extend resource group if the last one has 16 flavors already,
		// which is the maximum number of flavors in a resource group.
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
			if acceleratable {
				rg.CoveredResources = append(rg.CoveredResources,
					nodefeature.GetAcceleratableCreditsResourceName(manufacturer),
				)
			}
		}
		rg.Flavors = append(rg.Flavors, kueue.FlavorQuotas{
			Name: kueue.ResourceFlavorReference(rf.Name),
			Resources: []kueue.ResourceQuota{
				{
					Name:           core.ResourceCPU,
					NominalQuota:   cpuQ,
					BorrowingLimit: borLimit,
				},
				{
					Name:           core.ResourceMemory,
					NominalQuota:   ramQ,
					BorrowingLimit: borLimit,
				},
				{
					Name:           core.ResourceEphemeralStorage,
					NominalQuota:   stgQ,
					BorrowingLimit: borLimit,
				},
			},
		})
		if acceleratable {
			rg.Flavors[len(rg.Flavors)-1].Resources = append(rg.Flavors[len(rg.Flavors)-1].Resources,
				kueue.ResourceQuota{
					Name:           nodefeature.GetAcceleratableCreditsResourceName(manufacturer),
					NominalQuota:   accQ,
					BorrowingLimit: borLimit,
				},
			)
		}
	}

	return groups, notes
}

const (
	IndexingResourceFlavorsByQueueName = "resourceflavors.annotations['device.gpustack.ai/queue']"
)

// indexResourceFlavorByQueueName mirrors the field index registered by
// ClusterQueueReconciler.SetupController, so the fake client resolves
// MatchingFields{IndexingResourceFlavorsByQueueName} as the manager does.
func indexResourceFlavorByQueueName(obj ctrlcli.Object) []string {
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
	if !kubemeta.HasAnnotations(rf, _ResourceFlavorQueueNameAnnoKey, _ResourceFlavorCohortNameAnnoKey) {
		return nil
	}

	return []string{rf.Annotations[_ResourceFlavorQueueNameAnnoKey]}
}

func (r *ClusterQueueReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &kueue.ResourceFlavor{}, IndexingResourceFlavorsByQueueName, indexResourceFlavorByQueueName)
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
				// - updated if its drain mark has changed, so the queue enters
				//   HoldAndDrain once every flavor drains and leaves it once a
				//   flavor becomes active again.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return true
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldRf, newRf := e.ObjectOld.(*kueue.ResourceFlavor), e.ObjectNew.(*kueue.ResourceFlavor)
						if newRf.DeletionTimestamp == nil {
							// Fire when the drain mark has changed.
							return !mapx.EqualWithKey(oldRf.Annotations, newRf.Annotations,
								_ResourceFlavorDrainAnnoKey)
						}
						// Fire when the deletion timestamp changes.
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
				// Trigger reconciliation when a Node is:
				// - created.
				// - deleted.
				// - updated if its feature labels or allocatable resources have changed.
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						if aggressive {
							return true
						}

						oldNd, newNd := e.ObjectOld.(*core.Node), e.ObjectNew.(*core.Node)
						if newNd.DeletionTimestamp == nil {
							// Fire when feature labels have changed.
							if !mapx.EqualWithStringPrefix(oldNd.Labels, newNd.Labels,
								systemname.LabelPrefix,
								nodefeature.FeatureLabelPrefix,
								nodefeature.GeneralFeatureLabelPrefix,
								nodefeature.AcceleratableFeatureLabelPrefix) {
								return true
							}
							// Fire when allocatable resources have changed.
							for cn := range newNd.Status.Allocatable {
								switch {
								default:
									continue
								case nodefeature.IsKnownAcceleratableResourceName(cn):
								case cn == core.ResourceCPU:
								case cn == core.ResourceMemory:
								case cn == core.ResourceEphemeralStorage:
								}
								if !oldNd.Status.Allocatable[cn].Equal(newNd.Status.Allocatable[cn]) {
									return true
								}
							}
							return false
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

	// Ensure the ResourceFlavor has cohort and queue annotations.
	if !kubemeta.HasAnnotations(obj,
		_ResourceFlavorCohortNameAnnoKey,
		_ResourceFlavorQueueNameAnnoKey) {
		logger.V(2).Info("missing cohort or queue annotation")
		return nil
	}

	rf := obj.(*kueue.ResourceFlavor)

	var reqs []ctrlreconcile.Request
	{
		cohortName := rf.Annotations[_ResourceFlavorCohortNameAnnoKey]
		queueName := rf.Annotations[_ResourceFlavorQueueNameAnnoKey]
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

	logger.V(2).Info("enqueue queue from resource flavor", "requests", reqs)
	return reqs
}

func (r *ClusterQueueReconciler) enqueueCohortWhenNodeChanged(
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

	logger.V(2).Info("enqueue queue from node", "requests", reqs)
	return reqs
}
