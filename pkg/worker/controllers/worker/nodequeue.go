package worker

import (
	"context"
	"fmt"
	"sort"
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

	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// _ClusterQueueResType is the systemmeta resource type carried by the
// ClusterQueues (node queues / InstanceTypes) this reconciler owns.
const _ClusterQueueResType = "instancetypes"

// NodeQueueReconciler reconciles kueue.ClusterQueue objects (the node queues /
// InstanceTypes) driven by kueue.ResourceFlavor changes:
//   - Aggregate every ResourceFlavor sharing the queue's (key, os, arch) into one
//     ClusterQueue: an accelerated queue advertises only credits (= Σ capacity×M
//     across its flavors), a CPU-only queue only cpu (= Σ capacity cores). The
//     unit spec (unitCPU/unitRAM/localStorage) and per-card VRAM live in notes.
//   - Tear the queue down — HoldAndDrain, wait for eviction, then delete — when no
//     ResourceFlavor backs it or an external Delete has set HoldAndDrain.
//
// It reads only ResourceFlavor labels/notes (never Nodes): the capacity is
// pre-computed onto the flavor by NodeFlavorReconciler, so a node change reaches
// the queue through the flavor.
type NodeQueueReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeQueueReconciler)(nil)

func (r *NodeQueueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch the ResourceFlavors feeding this queue.
	rfList := new(kueue.ResourceFlavorList)
	err := r.Client.List(ctx, rfList,
		ctrlcli.MatchingFields{IndexingResourceFlavorByNodeQueue: req.Name},
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list resource flavors by node queue")
		return ctrl.Result{}, err
	}

	// Fetch the ClusterQueue (may not exist yet).
	cq := new(kueue.ClusterQueue)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: req.Name}, cq, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch cluster queue")
			return ctrl.Result{}, err
		}
		cq = nil
	}
	if cq != nil && cq.DeletionTimestamp != nil {
		logger.V(3).Info("skip deleted cluster queue")
		return ctrl.Result{}, nil
	}

	// Read instance-type-derived-from-node per-reconcile: when false the admin owns
	// the ClusterQueue, so the operator never auto-creates it and never auto-tears
	// it down.
	derived := settings.InstanceTypeDerivedFromNode.ShouldValueBool(ctx)

	// A HoldAndDrain set externally (the InstanceType API turns a Delete into a
	// HoldAndDrain) drains and deletes regardless of the derived setting — an
	// explicit delete must complete.
	if cq != nil && ptr.Deref(cq.Spec.StopPolicy, kueue.None) == kueue.HoldAndDrain {
		return r.teardownClusterQueue(ctx, cq)
	}

	// No ResourceFlavor backs the queue.
	if len(rfList.Items) == 0 {
		if cq == nil || !derived {
			// Nothing to do: either never created, or admin-owned (leave it).
			return ctrl.Result{}, nil
		}
		return r.teardownClusterQueue(ctx, cq)
	}

	// Admin-owned (instance-type-derived-from-node false): skip auto-creating a
	// queue the admin has not defined.
	if cq == nil && !derived {
		logger.V(3).Info("instance-type-derived-from-node is false, skip auto-create")
		return ctrl.Result{}, nil
	}

	// Stable order so the aggregated queue is deterministic.
	sort.Slice(rfList.Items, func(i, j int) bool {
		if rfList.Items[i].CreationTimestamp.Equal(&rfList.Items[j].CreationTimestamp) {
			return rfList.Items[i].Name < rfList.Items[j].Name
		}
		return rfList.Items[i].CreationTimestamp.Before(&rfList.Items[j].CreationTimestamp)
	})

	_, firstNotes := systemmeta.DescribeResource(&rfList.Items[0])
	acceleratable := firstNotes["acceleratable"] == "true"
	manufacturer := firstNotes["manufacturer"]

	eResGroups := buildResourceGroups(rfList, acceleratable, manufacturer, derived)
	eNotes := assembleClusterQueueNotes(cq, rfList, firstNotes)

	// Stamp the same schedule discriminator labels the flavors carry — the feature
	// key plus kubernetes.io/os|arch — so the queue's ResourceFlavors (and, with
	// gpustack.ai/managed=true added, its Devices) are reverse-looked-up from the
	// ClusterQueue by a label selector. Count/capacity stay on the flavors only.
	key, os, arch, _ := parseResourceFlavorSchedule(&rfList.Items[0])
	eLabels := map[string]string{
		featureKeyLabel(acceleratable, key): "true",
		core.LabelOSStable:                  os,
		core.LabelArchStable:                arch,
	}

	eCq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{
			Name:   req.Name,
			Labels: eLabels,
		},
		Spec: kueue.ClusterQueueSpec{
			NamespaceSelector: &meta.LabelSelector{},
			// Empty cohortName keeps the queue isolated: no cross-queue borrowing to
			// broker, so an externally managed cohort can never pull its quota out.
			CohortName: "",
			StopPolicy: ptr.To(kueue.None),
			FlavorFungibility: &kueue.FlavorFungibility{
				WhenCanBorrow:  kueue.TryNextFlavor,
				WhenCanPreempt: kueue.MayStopSearch,
			},
			// No cohort means no reclaim/borrow across queues; only in-queue
			// lower-priority preemption applies.
			Preemption: &kueue.ClusterQueuePreemption{
				ReclaimWithinCohort: kueue.PreemptionPolicyNever,
				BorrowWithinCohort: &kueue.BorrowWithinCohort{
					Policy: kueue.BorrowWithinCohortPolicyNever,
				},
				WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
			},
			ResourceGroups: eResGroups,
		},
	}
	systemmeta.NoteResource(eCq, _ClusterQueueResType, eNotes)

	cqAlignFn := func(aCq *kueue.ClusterQueue) (_ *kueue.ClusterQueue, skip bool, err error) {
		skip = true
		// Always converge the resource groups: the operator owns the credit/cpu
		// quota derived from the flavors' pooled capacity.
		if !kubemeta.DeepEqual(aCq.Spec.ResourceGroups, eCq.Spec.ResourceGroups) {
			aCq.Spec.ResourceGroups = eCq.Spec.ResourceGroups
			skip = false
		}
		// Always converge notes.
		if !systemmeta.EqualResourceTypeAndNotes(aCq, eCq) {
			systemmeta.NoteResource(aCq, _ClusterQueueResType, eNotes)
			skip = false
		}
		// Always converge the schedule discriminator labels (the reverse-lookup keys).
		if !mapx.Contain(aCq.Labels, eLabels) {
			if aCq.Labels == nil {
				aCq.Labels = make(map[string]string)
			}
			for k, v := range eLabels {
				aCq.Labels[k] = v
			}
			skip = false
		}
		// Enforce isolation + active state only for auto-derived queues; an
		// admin-owned queue (instance-type-derived-from-node false) keeps its own
		// cohort/preemption policy.
		if derived {
			if aCq.Spec.CohortName != "" {
				aCq.Spec.CohortName = ""
				skip = false
			}
			if ptr.Deref(aCq.Spec.StopPolicy, kueue.None) != kueue.None {
				aCq.Spec.StopPolicy = ptr.To(kueue.None)
				skip = false
			}
			if !kubemeta.DeepEqual(aCq.Spec.Preemption, eCq.Spec.Preemption) {
				aCq.Spec.Preemption = eCq.Spec.Preemption
				skip = false
			}
			if !kubemeta.DeepEqual(aCq.Spec.FlavorFungibility, eCq.Spec.FlavorFungibility) {
				aCq.Spec.FlavorFungibility = eCq.Spec.FlavorFungibility
				skip = false
			}
			if aCq.Spec.NamespaceSelector == nil {
				aCq.Spec.NamespaceSelector = eCq.Spec.NamespaceSelector
				skip = false
			}
		}
		return aCq, skip, nil
	}
	_, err = kubeclientset.CreateWithCtrlClient(ctx, r.Client, eCq,
		kubeclientset.WithUpdateIfExisted(cqAlignFn))
	if err != nil {
		logger.Error(err, "sync cluster queue")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("synced cluster queue")
	return ctrl.Result{}, nil
}

// teardownClusterQueue gracefully removes a ClusterQueue: it switches the queue to
// HoldAndDrain so Kueue evicts admitted workloads and cancels reservations, waits
// until nothing is reserved, then deletes it. The unit spec lives in notes, so the
// frozen ResourceGroups need no recompute while draining.
func (r *NodeQueueReconciler) teardownClusterQueue(ctx context.Context, cq *kueue.ClusterQueue) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Phase 1: ensure HoldAndDrain so Kueue evicts admitted workloads.
	if ptr.Deref(cq.Spec.StopPolicy, kueue.None) != kueue.HoldAndDrain {
		cq.Spec.StopPolicy = ptr.To(kueue.HoldAndDrain)
		if err := r.Client.Update(ctx, cq); err != nil {
			logger.Error(err, "set cluster queue to hold and drain")
			return ctrl.Result{}, err
		}
		logger.V(2).Info("set cluster queue to hold and drain; requeue in 15s")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Phase 2: wait until Kueue has drained all reservations, then delete.
	if hasReserved(cq) {
		logger.V(2).Info("cluster queue still has reserved resources; requeue in 15s")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if err := r.Client.Delete(ctx, cq); err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "delete drained cluster queue")
			return ctrl.Result{}, err
		}
	}
	logger.V(2).Info("deleted drained cluster queue")
	return ctrl.Result{}, nil
}

// hasReserved reports whether the ClusterQueue still holds reserved quota or
// admitted/reserving workloads. The queue must not be deleted until Kueue has
// finished draining; the workload counters guard against the flavor reservation
// snapshot momentarily reading zero while eviction is still in flight. Pending
// workloads are intentionally not counted — they hold no reservation, and gating
// on them would block deletion forever.
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

// buildResourceGroups builds the ClusterQueue resource groups from the feeding
// flavors. An accelerated queue covers only the manufacturer's credits resource
// (nominal = capacity×M per flavor); a CPU-only queue covers only cpu (nominal =
// capacity cores). When isolate is true (auto-derived queue) every quota also sets
// lendingLimit 0, so even if pulled into a cohort the queue lends nothing.
func buildResourceGroups(rfList *kueue.ResourceFlavorList, acceleratable bool, manufacturer string, isolate bool) []kueue.ResourceGroup {
	covered := []core.ResourceName{core.ResourceCPU}
	if acceleratable {
		covered = []core.ResourceName{nodefeature.GetAcceleratableCreditsResourceName(manufacturer)}
	}

	var groups []kueue.ResourceGroup
	for i := range rfList.Items {
		rf := &rfList.Items[i]
		_, _, _, capacity := parseResourceFlavorSchedule(rf)
		if capacity <= 0 {
			continue
		}

		var lendingLimit *resource.Quantity
		if isolate {
			lendingLimit = resource.NewQuantity(0, resource.DecimalSI)
		}
		nominal := *resource.NewQuantity(capacity, resource.DecimalSI)
		if acceleratable {
			nominal = nodefeature.CardsToCredits(nominal)
		}

		// A resource group holds at most 16 flavors.
		if len(groups) == 0 || len(groups[len(groups)-1].Flavors) >= 16 {
			groups = append(groups, kueue.ResourceGroup{CoveredResources: covered})
		}
		g := &groups[len(groups)-1]
		g.Flavors = append(g.Flavors, kueue.FlavorQuotas{
			Name: kueue.ResourceFlavorReference(rf.Name),
			Resources: []kueue.ResourceQuota{
				{
					Name:         covered[0],
					NominalQuota: nominal,
					LendingLimit: lendingLimit,
				},
			},
		})
	}
	return groups
}

// assembleClusterQueueNotes builds the queue's "instancetypes" notes. The
// descriptive fields always reflect the current flavors; the unit spec is the min
// across the feeding flavors, but is left untouched when the ClusterQueue already
// carries one (an admin set it via the InstanceType API — never clobber it).
func assembleClusterQueueNotes(cq *kueue.ClusterQueue, rfList *kueue.ResourceFlavorList, firstNotes map[string]string) map[string]string {
	notes := map[string]string{
		"acceleratable": firstNotes["acceleratable"],
		"manufacturer":  firstNotes["manufacturer"],
		"product":       firstNotes["product"],
		"family":        firstNotes["family"],
		"memory":        firstNotes["memory"],
	}

	// Preserve an existing unit spec (admin-set or previously derived); only derive
	// when the queue carries none yet.
	if cq != nil {
		_, cqNotes := systemmeta.DescribeResource(cq)
		if cqNotes["unitCPU"] != "" {
			notes["unitCPU"] = cqNotes["unitCPU"]
			notes["unitRAM"] = cqNotes["unitRAM"]
			notes["localStorage"] = cqNotes["localStorage"]
			return notes
		}
	}

	var unitCPU, unitRAM, localStorage string
	for i := range rfList.Items {
		_, n := systemmeta.DescribeResource(&rfList.Items[i])
		unitCPU = minPositiveNumeric(unitCPU, n["unitCPU"])
		unitRAM = minPositiveNumeric(unitRAM, n["unitRAM"])
		localStorage = minPositiveNumeric(localStorage, n["localStorage"])
	}
	notes["unitCPU"] = unitCPU
	notes["unitRAM"] = unitRAM
	notes["localStorage"] = localStorage
	return notes
}

// minPositiveNumeric returns the smaller of cur and v compared as numeric strings,
// ignoring non-positive candidates (so a flavor reporting no value never lowers the
// min). cur is the running min ("" until the first positive value).
func minPositiveNumeric(cur, v string) string {
	if stringx.CompareNumeric(v, "0") <= 0 {
		return cur
	}
	if cur == "" || stringx.CompareNumeric(v, cur) < 0 {
		return v
	}
	return cur
}

const (
	IndexingResourceFlavorByNodeQueue = "resourceflavors.schedule.gpustack.ai/node-queue"
)

// resourceFlavorNodeQueueName is the name of the ClusterQueue a flavor feeds:
// "gpustack-${key}-${os}-${arch}", the flavor name with its "-${count}{c|d}"
// suffix dropped, so flavors differing only in per-node count aggregate together.
// It returns "" when the flavor lacks the schedule labels.
func resourceFlavorNodeQueueName(rf *kueue.ResourceFlavor) string {
	key, os, arch, _ := parseResourceFlavorSchedule(rf)
	if key == "" || os == "" || arch == "" {
		return ""
	}
	return fmt.Sprintf("gpustack-%s-%s-%s", key, os, arch)
}

// parseResourceFlavorSchedule reads a flavor's schedule labels: its feature key
// (the bare "general."/"acceleratable." prefixed label whose value is "true"), the
// kubernetes.io/os|arch values, and the key's ".capacity" sibling (the pooled
// capacity = nodes × count). Missing fields come back empty/zero.
func parseResourceFlavorSchedule(rf *kueue.ResourceFlavor) (key, os, arch string, capacity int64) {
	os = rf.Labels[core.LabelOSStable]
	arch = rf.Labels[core.LabelArchStable]
	for k, v := range rf.Labels {
		if v != "true" {
			continue
		}
		switch {
		case strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix):
			key = strings.TrimPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix)
		case strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix):
			key = strings.TrimPrefix(k, nodefeature.GeneralFeatureLabelPrefix)
		default:
			continue
		}
		capacity, _ = strconvx.Atoi[int64](rf.Labels[k+_ResourceFlavorCapacityLabelSuffix])
		break
	}
	return key, os, arch, capacity
}

// indexResourceFlavorByNodeQueue maps a managed ResourceFlavor to the node-queue
// name it feeds, so the reconciler resolves a queue's flavors with a single List.
func indexResourceFlavorByNodeQueue(obj ctrlcli.Object) []string {
	rf, ok := obj.(*kueue.ResourceFlavor)
	if !ok || rf == nil {
		return nil
	}
	if rf.DeletionTimestamp != nil {
		return nil
	}
	if !systemmeta.MatchResource(rf, _ResourceFlavorResType) {
		return nil
	}
	name := resourceFlavorNodeQueueName(rf)
	if name == "" {
		return nil
	}
	return []string{name}
}

func (r *NodeQueueReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &kueue.ResourceFlavor{}, IndexingResourceFlavorByNodeQueue, indexResourceFlavorByNodeQueue)
	if err != nil {
		return fmt.Errorf("index resource flavor '%s': %w", IndexingResourceFlavorByNodeQueue, err)
	}

	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodequeue").
		For(
			// Reconcile relevant ClusterQueue objects keyed by their own name, so a
			// start-up resync re-evaluates every queue and an external Delete that
			// sets HoldAndDrain is observed and completed.
			&kueue.ClusterQueue{},
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, _ClusterQueueResType)
				}),
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return false
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldCq, newCq := e.ObjectOld.(*kueue.ClusterQueue), e.ObjectNew.(*kueue.ClusterQueue)
						if newCq.DeletionTimestamp != nil {
							return false
						}
						// Fire when spec (incl. StopPolicy) or notes have changed.
						if !kubemeta.DeepEqual(oldCq.Spec, newCq.Spec) {
							return true
						}
						return !systemmeta.EqualResourceTypeAndNotes(oldCq, newCq)
					},
				},
			),
		).
		Watches(
			// Watch ResourceFlavors and enqueue the ClusterQueue they feed.
			&kueue.ResourceFlavor{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(
				3*time.Second,
				r.enqueueClusterQueueWhenResourceFlavorChanged,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, _ResourceFlavorResType)
				}),
				// Trigger when a ResourceFlavor is created, deleted, or its schedule
				// labels (esp. capacity) or notes have changed.
				ctrlpredicate.Funcs{
					DeleteFunc: func(e ctrlevent.DeleteEvent) bool {
						return true
					},
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldRf, newRf := e.ObjectOld.(*kueue.ResourceFlavor), e.ObjectNew.(*kueue.ResourceFlavor)
						if newRf.DeletionTimestamp == nil {
							if !mapx.EqualWithStringPrefix(oldRf.Labels, newRf.Labels,
								nodefeature.GeneralFeatureLabelPrefix,
								nodefeature.AcceleratableFeatureLabelPrefix,
								core.LabelOSStable,
								core.LabelArchStable) {
								return true
							}
							return !systemmeta.EqualResourceTypeAndNotes(oldRf, newRf)
						}
						if oldRf.DeletionTimestamp == nil {
							return true
						}
						return !oldRf.DeletionTimestamp.Equal(newRf.DeletionTimestamp)
					},
				},
			),
		).
		Complete(r)
}

func (r *NodeQueueReconciler) enqueueClusterQueueWhenResourceFlavorChanged(
	ctx context.Context,
	obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("resource flavor", ctrlcli.ObjectKeyFromObject(obj))

	rf, ok := obj.(*kueue.ResourceFlavor)
	if !ok {
		return nil
	}
	name := resourceFlavorNodeQueueName(rf)
	if name == "" {
		logger.V(2).Info("resource flavor has no node-queue name")
		return nil
	}

	logger.V(2).Info("enqueue cluster queue from resource flavor", "name", name)
	return []ctrlreconcile.Request{
		{NamespacedName: ctrlcli.ObjectKey{Name: name}},
	}
}
