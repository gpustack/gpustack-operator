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
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

// ResourceFlavorReconciler reconciles the kueue.ResourceFlavor object,
// and manages corresponding ClusterQueue.
type ResourceFlavorReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*ResourceFlavorReconciler)(nil)

func (r *ResourceFlavorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	resFlv := new(kueue.ResourceFlavor)
	err := r.Client.Get(ctx, req.NamespacedName, resFlv)
	if err != nil {
		logger.Error(err, "fetch resource flavor")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Extract generate name of ClusterQueue.
	cqGN := systemmeta.DescribeResourceNote(resFlv, "clusterqueue-generate-name")
	if cqGN == "" {
		logger.V(2).Info("skip resource flavor without clusterqueue-generate-name note")
		return ctrl.Result{}, nil
	}

	// Fetch referring ClusterQueue.
	refCqs, err := r.getReferringClusterQueues(ctx, resFlv)
	if err != nil {
		logger.Error(err, "fetch referring cluster queues for resource flavor")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// If the ResourceFlavor is being deleted,
	// remove the ResourceFlavor from referring ClusterQueues.
	if resFlv.DeletionTimestamp != nil {
		var requeue bool

		for i := range refCqs {
			refCq := &refCqs[i]

			logger := logger.WithValues("cluster queue", refCq.Name)

			rgIndex, flvIndex, found := indexOfResourceFlavorOfClusterQueue(resFlv, refCq)
			if !found {
				logger.Error(nil, "resource flavor is not found")
				continue
			}

			if isResourceFlavorReserved(resFlv, refCq) {
				logger.Error(nil, "cannot remove reserved resource flavor, requeue in 15s")
				requeue = true
				continue
			}

			err = r.removeResourceFlavor(ctx, refCq, rgIndex, flvIndex)
			if err != nil {
				logger.Error(err, "remove resource flavor")
				return ctrl.Result{}, err
			}
			logger.V(2).Info("removed resource flavor")
		}

		if requeue {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}

	// Fetch Node.
	nd, err := r.fetchNode(ctx, resFlv)
	if err != nil {
		logger.Error(err, "fetch node for resource flavor")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	} else if nd == nil {
		logger.Error(nil, "skip resource flavor without node name label")
		return ctrl.Result{}, nil
	}

	// Fetch Cohort.
	co, err := r.fetchCohort(ctx, resFlv)
	if err != nil {
		logger.Error(err, "fetch cohort for resource flavor")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	} else if co == nil {
		logger.Error(nil, "skip resource flavor without cohort name label")
		return ctrl.Result{}, nil
	}

	// For the referring ClusterQueues,
	// which are not having the same generate name as the ResourceFlavor,
	// zero the ResourceFlavor from the ClusterQueues if not sliced,
	// or remove the ResourceFlavor from the ClusterQueues if sliced.
	var refCqCurrent *kueue.ClusterQueue
	{
		var requeue bool

		for i := range refCqs {
			refCq := &refCqs[i]

			if refCq.GenerateName == cqGN {
				refCqCurrent = refCq
				continue
			}

			logger := logger.WithValues("cluster queue", refCq.Name)

			rgIndex, flvIndex, found := indexOfResourceFlavorOfClusterQueue(resFlv, refCq)
			if !found {
				logger.Error(nil, "resource flavor is not found")
				continue
			}

			if isResourceFlavorReserved(resFlv, refCq) {
				logger.Error(nil, "cannot clear resource flavor as reserved, requeue in 15s")
				requeue = true
				continue
			}

			if !strings.Contains(refCq.GenerateName, "-sliced-") {
				err, cleared := r.clearResourceFlavorQuota(ctx, refCq, rgIndex, flvIndex)
				if err != nil {
					logger.Error(err, "clear resource flavor quota")
					return ctrl.Result{}, err
				}
				if cleared {
					logger.V(2).Info("cleared resource flavor quota")
				}
				continue
			}

			err = r.removeResourceFlavor(ctx, refCq, rgIndex, flvIndex)
			if err != nil {
				logger.Error(err, "remove resource flavor")
				return ctrl.Result{}, err
			}
			logger.V(2).Info("removed resource flavor")
		}

		if requeue {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}

	// Extract node feature from the Node.
	ndf := devicefeature.ExtractNodeFeatureByKey(nd, strings.TrimPrefix(co.Name, "gpustack-"))

	if refCqCurrent != nil {
		logger := logger.WithValues("cluster queue", refCqCurrent.Name)

		rgIndex, flvIndex, found := indexOfResourceFlavorOfClusterQueue(resFlv, refCqCurrent)
		if !found {
			logger.Error(nil, "resource flavor is not found")
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}

		if isResourceFlavorReserved(resFlv, refCqCurrent) {
			if isResourceFlavorBorrowing(resFlv.Name, refCqCurrent) {
				logger.Error(nil, "cannot reset reserved resource flavor, requeue in 15s")
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
		}

		err, reset := r.resetResourceFlavorQuota(ctx, refCqCurrent, rgIndex, flvIndex, constructFlavorQuotas(resFlv, ndf))
		if err != nil {
			logger.Error(err, "reset resource flavor quota")
			return ctrl.Result{}, err
		}
		if reset {
			logger.V(2).Info("reset resource flavor quota")
		}
		return ctrl.Result{}, nil
	}

	// Group ClusterQueues by generate name.
	cqsByGN, err := r.groupClusterQueuesByGenerateName(ctx, co)
	if err != nil {
		logger.Error(err, "group cluster queues by generate name")
		return ctrl.Result{}, err
	}

	if cqs, ok := cqsByGN[cqGN]; ok {
		// If there is already a ClusterQueue with the same generate name,
		// find a slot to place the ResourceFlavor in the ClusterQueue.
		// And then, update the ClusterQueue.
		cqIndex, rgIndex, found := findSlotOfClusterQueues(cqs)
		if found {
			logger = logger.WithValues("cluster queue", cqs[cqIndex].Name)
			cq := cqs[cqIndex]
			{
				rg := &cq.Spec.ResourceGroups[rgIndex]
				rg.Flavors = append(rg.Flavors, constructFlavorQuotas(resFlv, ndf))
			}
			err = r.Client.Update(ctx, cq)
			if err != nil {
				logger.Error(err, "add resource flavor")
				return ctrl.Result{}, err
			}
			logger.V(2).Info("added resource flavor")
			return ctrl.Result{}, nil
		}
	}

	// If there is no ClusterQueue with the same generate name,
	// or there is no slot for the ResourceFlavor in the ClusterQueues with the same generate name,
	// create a new ClusterQueue for the ResourceFlavor.
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{
			GenerateName: cqGN,
		},
		Spec: kueue.ClusterQueueSpec{
			NamespaceSelector: &meta.LabelSelector{},
			CohortName:        kueue.CohortReference(co.Name),
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
			ResourceGroups: []kueue.ResourceGroup{
				constructResourceGroup(resFlv, ndf),
			},
		},
	}
	systemmeta.NoteResource(cq, "instancetypes", map[string]string{
		"acceleratable":     strconv.FormatBool(!strings.Contains(cqGN, devicefeature.DisfeaturedNodeKey)),
		"manufacturer":      ndf.Manufacturer,
		"product":           ndf.Product,
		"memory":            ndf.Memory,
		"family":            ndf.Family,
		"computeCapability": ndf.ComputeCapability,
		"sliced":            ndf.Sliced,
	})
	err = r.Client.Create(ctx, cq)
	if err != nil {
		logger.Error(err, "create cluster queue for resource flavor")
		return ctrl.Result{}, err
	}
	logger.V(2).Info("created cluster queue for resource flavor", "cluster queue", cq.Name)
	return ctrl.Result{}, nil
}

func (r *ResourceFlavorReconciler) fetchNode(
	ctx context.Context, resFlv *kueue.ResourceFlavor,
) (*core.Node, error) {
	ndName := resFlv.Labels[_ResourceFlavorNodeNameLabelKey]
	if ndName == "" {
		return nil, nil
	}

	nd := new(core.Node)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: ndName}, nd,
		ctrlcli.UnsafeDisableDeepCopy,
		&ctrlcli.GetOptions{
			Raw: &meta.GetOptions{
				ResourceVersion: "0",
			},
		})
	if err != nil {
		return nil, err
	}
	return nd, nil
}

func (r *ResourceFlavorReconciler) fetchCohort(
	ctx context.Context, resFlv *kueue.ResourceFlavor,
) (*kueue.Cohort, error) {
	coName := resFlv.Labels[_ResourceFlavorCohortNameLabelKey]
	if coName == "" {
		return nil, nil
	}

	co := new(kueue.Cohort)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: coName}, co,
		ctrlcli.UnsafeDisableDeepCopy,
		&ctrlcli.GetOptions{
			Raw: &meta.GetOptions{
				ResourceVersion: "0",
			},
		})
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, err
		}
		co = &kueue.Cohort{
			ObjectMeta: meta.ObjectMeta{
				Name: coName,
			},
		}
		err = r.Client.Create(ctx, co)
		if err != nil {
			if kerrors.IsAlreadyExists(err) {
				return r.fetchCohort(ctx, resFlv)
			}
		}
	}

	return co, err
}

func (r *ResourceFlavorReconciler) groupClusterQueuesByGenerateName(
	ctx context.Context, co *kueue.Cohort,
) (map[string][]*kueue.ClusterQueue, error) {
	cqList := new(kueue.ClusterQueueList)
	err := r.Client.List(ctx, cqList,
		ctrlcli.MatchingFields{
			IndexingByCohortName: co.Name,
		})
	if err != nil {
		return nil, err
	}

	cqsByGn := make(map[string][]*kueue.ClusterQueue)
	for i := range cqList.Items {
		if cqList.Items[i].DeletionTimestamp != nil {
			continue
		}
		cq := &cqList.Items[i]
		cqsByGn[cq.GenerateName] = append(cqsByGn[cq.GenerateName], cq)
	}
	return cqsByGn, nil
}

func (r *ResourceFlavorReconciler) getReferringClusterQueues(
	ctx context.Context, resFlv *kueue.ResourceFlavor,
) ([]kueue.ClusterQueue, error) {
	cqList := new(kueue.ClusterQueueList)
	err := r.Client.List(ctx, cqList,
		ctrlcli.MatchingFields{
			IndexingByResourceFlavorName: resFlv.Name,
		})
	if err != nil {
		return nil, err
	}
	return cqList.Items, nil
}

func (r *ResourceFlavorReconciler) removeResourceFlavor(
	ctx context.Context, cq *kueue.ClusterQueue, rgIndex, flvIndex int,
) error {
	rg := &cq.Spec.ResourceGroups[rgIndex]
	rg.Flavors = append(rg.Flavors[:flvIndex], rg.Flavors[flvIndex+1:]...)
	if len(rg.Flavors) == 0 {
		cq.Spec.ResourceGroups = append(cq.Spec.ResourceGroups[:rgIndex], cq.Spec.ResourceGroups[rgIndex+1:]...)
	}
	if len(cq.Spec.ResourceGroups) == 0 {
		return r.Client.Delete(ctx, cq)
	}
	return r.Client.Update(ctx, cq)
}

func (r *ResourceFlavorReconciler) clearResourceFlavorQuota(
	ctx context.Context, cq *kueue.ClusterQueue, rgIndex, flvIndex int,
) (error, bool) {
	zeroQuantity := *resource.NewQuantity(0, resource.DecimalSI)
	rg := &cq.Spec.ResourceGroups[rgIndex]
	flv := &rg.Flavors[flvIndex]
	flvOld := flv.DeepCopy()
	for res := range flv.Resources {
		flv.Resources[res].NominalQuota = zeroQuantity
	}
	if !kubemeta.DeepEqual(flv, flvOld) {
		annotateResourceFlavorBorrowing(string(flv.Name), cq)
		return r.Client.Update(ctx, cq), true
	}
	return nil, false
}

func (r *ResourceFlavorReconciler) resetResourceFlavorQuota(
	ctx context.Context, cq *kueue.ClusterQueue, rgIndex, flvIndex int, resQuotas kueue.FlavorQuotas,
) (error, bool) {
	rg := &cq.Spec.ResourceGroups[rgIndex]

	if !kubemeta.DeepEqual(rg.Flavors[flvIndex], resQuotas) {
		rg.Flavors[flvIndex] = resQuotas
		annotateResourceFlavorUnborrowing(string(resQuotas.Name), cq)
		return r.Client.Update(ctx, cq), true
	}
	return nil, false
}

const (
	IndexingByCohortName         = "clusterqueues.cohortName"
	IndexingByResourceFlavorName = "clusterqueues.resourceGroups[*].flavors[*].name"
)

func (r *ResourceFlavorReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &kueue.ClusterQueue{}, IndexingByCohortName,
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			cq := obj.(*kueue.ClusterQueue)
			if cq.Spec.CohortName == "" {
				return nil
			}
			return []string{string(cq.Spec.CohortName)}
		})
	if err != nil {
		return fmt.Errorf("index cluster queue '%s': %w", IndexingByCohortName, err)
	}
	err = fi.IndexField(ctx, &kueue.ClusterQueue{}, IndexingByResourceFlavorName,
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			cq := obj.(*kueue.ClusterQueue)
			var resFlvNames []string
			for _, rg := range cq.Spec.ResourceGroups {
				for _, flv := range rg.Flavors {
					resFlvNames = append(resFlvNames, string(flv.Name))
				}
			}
			sort.Strings(resFlvNames)
			return resFlvNames
		})
	if err != nil {
		return fmt.Errorf("index cluster queue '%s': %w", IndexingByResourceFlavorName, err)
	}

	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("worker.manage.resource_flavors").
		For(
			// Focus on the Kueue ResourceFlavor,
			// when the ResourceFlavor is updated.
			&kueue.ResourceFlavor{},
			ctrlbuilder.WithPredicates(
				// Only reconcile when the ResourceFlavor is related to the worker Node,
				// which is identified by the "nodes" resource note.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, "nodes")
				}),
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldResFlv, newResFlv := e.ObjectOld.(*kueue.ResourceFlavor), e.ObjectNew.(*kueue.ResourceFlavor)
						if newResFlv.GetDeletionTimestamp() == nil {
							return !kubemeta.DeepEqual(oldResFlv.Labels, newResFlv.Labels) ||
								!kubemeta.DeepEqual(oldResFlv.Annotations, newResFlv.Annotations)
						}
						return true
					},
					GenericFunc: func(e ctrlevent.GenericEvent) bool {
						return false
					},
				},
			),
		).
		Complete(r)
}

func findSlotOfClusterQueues(cqs []*kueue.ClusterQueue) (cqIndex, rgIndex int, found bool) {
	for cqIndex = range cqs {
		cq := cqs[cqIndex]
		for rgIndex = range cq.Spec.ResourceGroups {
			rg := &cq.Spec.ResourceGroups[rgIndex]
			if len(rg.Flavors) < 64 {
				return cqIndex, rgIndex, true
			}
		}
		if len(cq.Spec.ResourceGroups) < 4 {
			rg := kueue.ResourceGroup{
				CoveredResources: cq.Spec.ResourceGroups[0].CoveredResources,
			}
			cq.Spec.ResourceGroups = append(cq.Spec.ResourceGroups, rg)
			return cqIndex, len(cq.Spec.ResourceGroups) - 1, true
		}
	}
	return 0, 0, false
}

func indexOfResourceFlavorOfClusterQueue(resFlv *kueue.ResourceFlavor, cq *kueue.ClusterQueue) (rgIndex, flvIndex int, found bool) {
	for rgIndex = range cq.Spec.ResourceGroups {
		rg := &cq.Spec.ResourceGroups[rgIndex]
		for flvIndex = range rg.Flavors {
			flv := &rg.Flavors[flvIndex]
			if flv.Name == kueue.ResourceFlavorReference(resFlv.Name) {
				return rgIndex, flvIndex, true
			}
		}
	}
	return -1, -1, false
}

func isResourceFlavorReserved(resFlv *kueue.ResourceFlavor, cq *kueue.ClusterQueue) bool {
	for i := range cq.Status.FlavorsReservation {
		flv := &cq.Status.FlavorsReservation[i]
		if flv.Name != kueue.ResourceFlavorReference(resFlv.Name) {
			continue
		}
		for j := range flv.Resources {
			flvRes := &flv.Resources[j]
			if !flvRes.Total.IsZero() {
				return true
			}
			if !flvRes.Borrowed.IsZero() {
				return true
			}
		}
	}
	return false
}

func annotateResourceFlavorBorrowing(resFlvName string, cq *kueue.ClusterQueue) {
	if cq.Annotations == nil {
		cq.Annotations = make(map[string]string)
	}
	cq.Annotations[resFlvName] = "borrowing"
}

func annotateResourceFlavorUnborrowing(resFlvName string, cq *kueue.ClusterQueue) {
	if cq.Annotations != nil {
		delete(cq.Annotations, resFlvName)
	}
}

func isResourceFlavorBorrowing(resFlvName string, cq *kueue.ClusterQueue) bool {
	for name := range cq.Annotations {
		if name == resFlvName {
			return cq.Annotations[name] == "borrowing"
		}
	}
	return false
}

func constructResourceGroup(resFlv *kueue.ResourceFlavor, ndf devicefeature.NodeFeature) kueue.ResourceGroup {
	accResName := devicefeature.GetCreditsResourceName(ndf.Manufacturer)
	borLimit := funcx.Ternary(ndf.Sliced != "", resource.NewQuantity(0, resource.DecimalSI), nil)

	rg := kueue.ResourceGroup{}
	if ndf.Manufacturer != "" {
		rg.CoveredResources = make([]core.ResourceName, 0, 4)
		rg.Flavors = []kueue.FlavorQuotas{
			{
				Name:      kueue.ResourceFlavorReference(resFlv.Name),
				Resources: make([]kueue.ResourceQuota, 0, 4),
			},
		}
	} else {
		rg.CoveredResources = make([]core.ResourceName, 0, 3)
		rg.Flavors = []kueue.FlavorQuotas{
			{
				Name:      kueue.ResourceFlavorReference(resFlv.Name),
				Resources: make([]kueue.ResourceQuota, 0, 3),
			},
		}
	}

	rg.CoveredResources = []core.ResourceName{
		core.ResourceCPU,
		core.ResourceMemory,
		core.ResourceEphemeralStorage,
	}
	rg.Flavors[0].Resources = []kueue.ResourceQuota{
		{
			Name:           core.ResourceCPU,
			NominalQuota:   ndf.CPU,
			BorrowingLimit: borLimit,
		},
		{
			Name:           core.ResourceMemory,
			NominalQuota:   ndf.RAM,
			BorrowingLimit: borLimit,
		},
		{
			Name:           core.ResourceEphemeralStorage,
			NominalQuota:   ndf.LocalStorage,
			BorrowingLimit: borLimit,
		},
	}
	if ndf.Manufacturer != "" {
		rg.CoveredResources = append(rg.CoveredResources, accResName)
		rg.Flavors[0].Resources = append(rg.Flavors[0].Resources, kueue.ResourceQuota{
			Name:           accResName,
			NominalQuota:   ndf.Accelerator,
			BorrowingLimit: borLimit,
		})
	}
	return rg
}

func constructFlavorQuotas(resFlv *kueue.ResourceFlavor, ndf devicefeature.NodeFeature) kueue.FlavorQuotas {
	accResName := devicefeature.GetCreditsResourceName(ndf.Manufacturer)
	borLimit := funcx.Ternary(ndf.Sliced != "", resource.NewQuantity(0, resource.DecimalSI), nil)

	flv := kueue.FlavorQuotas{
		Name: kueue.ResourceFlavorReference(resFlv.Name),
	}
	if ndf.Manufacturer != "" {
		flv.Resources = make([]kueue.ResourceQuota, 0, 4)
	} else {
		flv.Resources = make([]kueue.ResourceQuota, 0, 3)
	}

	flv.Resources = []kueue.ResourceQuota{
		{
			Name:           core.ResourceCPU,
			NominalQuota:   ndf.CPU,
			BorrowingLimit: borLimit,
		},
		{
			Name:           core.ResourceMemory,
			NominalQuota:   ndf.RAM,
			BorrowingLimit: borLimit,
		},
		{
			Name:           core.ResourceEphemeralStorage,
			NominalQuota:   ndf.LocalStorage,
			BorrowingLimit: borLimit,
		},
	}
	if ndf.Manufacturer != "" {
		flv.Resources = append(flv.Resources, kueue.ResourceQuota{
			Name:           accResName,
			NominalQuota:   ndf.Accelerator,
			BorrowingLimit: borLimit,
		})
	}
	return flv
}
