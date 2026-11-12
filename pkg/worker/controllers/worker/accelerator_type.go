package worker

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
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
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/mapx"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/worker/apistatus"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// AcceleratorTypeReconciler owns the lifecycle of worker.gpustack.ai AcceleratorType CRs and
// coordinates the shared canonical pool Kueue ClusterQueue alongside InstanceTypeReconciler.
type AcceleratorTypeReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

var _ ctrlreconcile.Reconciler = (*AcceleratorTypeReconciler)(nil)

func (r *AcceleratorTypeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	at := new(workercore.AcceleratorType)
	err := r.Client.Get(ctx, req.NamespacedName, at, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "fetch accelerator type")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if at.DeletionTimestamp != nil {
		return r.teardownAcceleratorType(ctx, at)
	}

	cqName := AcceleratorTypePoolQueueName(ctx, at)
	locked := systemmeta.Lock(at)
	labelsChanged := alignAdmissionQueueLabels(
		at.Labels, AcceleratorTypeScheduleLabels(ctx, at), nodefeature.FormatLocalQueueName(cqName),
	)
	if !locked || labelsChanged {
		if err = r.Client.Update(ctx, at); err != nil {
			logger.Error(err, "lock accelerator type")
			return ctrl.Result{}, err
		}
	}

	cq, err := r.ensureClusterQueue(ctx, at)
	if err != nil {
		logger.Error(err, "ensure cluster queue")
		return ctrl.Result{}, err
	}
	if cq == nil {
		if err = r.Client.Delete(ctx, at); err != nil && !kerrors.IsNotFound(err) {
			logger.Error(err, "delete accelerator type after cluster queue deletion")
			return ctrl.Result{}, err
		}
		logger.V(2).Info("deleted accelerator type after cluster queue deletion")
		return ctrl.Result{}, nil
	}
	if cq.DeletionTimestamp != nil {
		deleteAT, err := shouldDeleteDerivedWithQueue(ctx, r.Client, r.APIReader, at, cq.Name)
		if err != nil {
			logger.Error(err, "check sibling fronting objects on terminating cluster queue")
			return ctrl.Result{}, err
		}
		if deleteAT {
			if err = r.Client.Delete(ctx, at); err != nil && !kerrors.IsNotFound(err) {
				logger.Error(err, "delete accelerator type after cluster queue deletion")
				return ctrl.Result{}, err
			}
			logger.V(2).Info("deleted accelerator type after cluster queue deletion")
			return ctrl.Result{}, nil
		}
		logger.V(2).Info("cluster queue terminating; requeue in 15s")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	changed, err := r.syncInactive(ctx, at, cq)
	if err != nil {
		return objectWriteResult(logger, err, "sync accelerator type inactive with cluster queue stop policy",
			_requeueAfterConflict)
	}
	if changed && at.Status.Entrance == nodefeature.FormatLocalQueueName(cq.Name) {
		return ctrl.Result{}, nil
	}

	if prevCQName := acceleratorTypePreviousPoolQueueName(ctx, at, cq.Name); prevCQName != "" {
		if _, err = reapDisplacedPoolClusterQueue(ctx, r.Client, r.APIReader, prevCQName, at); err != nil {
			logger.Error(err, "reap displaced cluster queue", "clusterQueue", prevCQName)
			return ctrl.Result{}, err
		}
	}

	desiredStatus := r.computeStatus(ctx, at, cq)
	if !kubemeta.DeepEqual(desiredStatus, at.Status) {
		at.Status = desiredStatus
		if err = r.Client.Status().Update(ctx, at); err != nil {
			return objectWriteResult(logger, err, "update accelerator type status", _requeueAfterConflict)
		}
		logger.V(2).Info("refreshed accelerator type status")
	}
	return ctrl.Result{}, nil
}

func (r *AcceleratorTypeReconciler) syncInactive(
	ctx context.Context, at *workercore.AcceleratorType, cq *kueue.ClusterQueue,
) (bool, error) {
	_, otherActive, err := poolHasActiveFrontingObjects(ctx, r.Client, cq.Name, at)
	if err != nil {
		return false, err
	}
	allInactive := at.Spec.Inactive && !otherActive

	if cq.Annotations[_TASQueueMigrationPhaseAnnotation] != "" {
		saved := cq.Annotations[_TASQueueMigrationStopPolicyAnnotation]
		want := saved
		switch {
		case allInactive && (saved == string(kueue.None) || saved == _TASQueueMigrationStopPolicyUnset):
			want = string(kueue.Hold)
		case !allInactive && saved == string(kueue.Hold):
			want = string(kueue.None)
		}
		if want == saved {
			return false, nil
		}
		cq.Annotations[_TASQueueMigrationStopPolicyAnnotation] = want
		return true, r.Client.Update(ctx, cq)
	}
	if cq.Annotations[_TASQueueEmptyPlanHoldAnnotation] != "" {
		return false, nil
	}

	switch ptr.Deref(cq.Spec.StopPolicy, kueue.None) {
	case kueue.None:
		if allInactive {
			cq.Spec.StopPolicy = ptr.To(kueue.Hold)
			return true, r.Client.Update(ctx, cq)
		}
	case kueue.Hold:
		if !allInactive {
			if len(cq.Spec.ResourceGroups) == 0 {
				if cq.Annotations == nil {
					cq.Annotations = make(map[string]string)
				}
				cq.Annotations[_TASQueueEmptyPlanHoldAnnotation] = "true"
			} else {
				cq.Spec.StopPolicy = ptr.To(kueue.None)
			}
			return true, r.Client.Update(ctx, cq)
		}
	}

	if ptr.Deref(cq.Spec.StopPolicy, kueue.None) != kueue.None && !at.Spec.Inactive {
		at.Spec.Inactive = true
		return true, r.Client.Update(ctx, at)
	}

	return false, nil
}

func shouldDeleteDerivedWithQueue(
	ctx context.Context, cli ctrlcli.Client, apiReader ctrlcli.Reader, at *workercore.AcceleratorType, cqName string,
) (bool, error) {
	if at.Labels[_InstanceTypeDerivedFromNodeLabel] != "true" {
		return false, nil
	}
	anyNonDeleting, _, err := poolHasActiveFrontingObjectsWithFallback(ctx, cli, apiReader, cqName, at)
	if err != nil {
		return false, err
	}
	return !anyNonDeleting, nil
}

func (r *AcceleratorTypeReconciler) ensureClusterQueue(
	ctx context.Context, at *workercore.AcceleratorType,
) (*kueue.ClusterQueue, error) {
	logger := ctrllog.FromContext(ctx)

	cqName := AcceleratorTypePoolQueueName(ctx, at)
	cq := new(kueue.ClusterQueue)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: cqName}, cq, ctrlclix.WithoutQuorum)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, err
		}
		currEntrance := nodefeature.FormatLocalQueueName(cqName)
		foundUncached := false
		if at.Status.Entrance == currEntrance && r.APIReader != nil {
			err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: cqName}, cq)
			if err == nil {
				foundUncached = true
			} else if !kerrors.IsNotFound(err) {
				return nil, err
			}
		}
		if !foundUncached {
			if at.Status.Entrance == currEntrance {
				deleteAT, derr := shouldDeleteDerivedWithQueue(ctx, r.Client, r.APIReader, at, cqName)
				if derr != nil {
					return nil, derr
				}
				if deleteAT {
					return nil, nil
				}
			}
			return r.createClusterQueue(ctx, at)
		}
	}

	if cq.DeletionTimestamp != nil {
		return cq, nil
	}

	eLabels := AcceleratorTypeScheduleLabels(ctx, at)
	changed := !systemmeta.NoteResource(cq, _ClusterQueueResType, nil)
	for k := range cq.Labels {
		if _, want := eLabels[k]; want {
			continue
		}
		if strings.HasPrefix(k, nodefeature.GeneralFeatureLabelPrefix) ||
			strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix) {
			delete(cq.Labels, k)
			changed = true
		}
	}
	if !mapx.Contain(cq.Labels, eLabels) {
		if cq.Labels == nil {
			cq.Labels = make(map[string]string)
		}
		for k, v := range eLabels {
			cq.Labels[k] = v
		}
		changed = true
	}
	if changed {
		if err = r.Client.Update(ctx, cq); err != nil {
			logger.Error(err, "align cluster queue")
			return nil, err
		}
		logger.V(2).Info("aligned cluster queue")
	}
	return cq, nil
}

func (r *AcceleratorTypeReconciler) createClusterQueue(
	ctx context.Context, at *workercore.AcceleratorType,
) (*kueue.ClusterQueue, error) {
	logger := ctrllog.FromContext(ctx)

	cqName := AcceleratorTypePoolQueueName(ctx, at)
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{
			Name:        cqName,
			Labels:      AcceleratorTypeScheduleLabels(ctx, at),
			Annotations: map[string]string{_TASQueueEmptyPlanHoldAnnotation: "true"},
		},
		Spec: kueue.ClusterQueueSpec{
			NamespaceSelector: &meta.LabelSelector{},
			StopPolicy:        ptr.To(kueue.Hold),
			FlavorFungibility: &kueue.FlavorFungibility{
				WhenCanBorrow:  kueue.TryNextFlavor,
				WhenCanPreempt: kueue.MayStopSearch,
			},
			Preemption: &kueue.ClusterQueuePreemption{
				ReclaimWithinCohort: kueue.PreemptionPolicyNever,
				BorrowWithinCohort:  &kueue.BorrowWithinCohort{Policy: kueue.BorrowWithinCohortPolicyNever},
				WithinClusterQueue:  kueue.PreemptionPolicyLowerPriority,
			},
		},
	}
	systemmeta.NoteResource(cq, _ClusterQueueResType, nil)
	if err := r.Client.Create(ctx, cq); err != nil {
		if kerrors.IsAlreadyExists(err) {
			err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: cqName}, cq, ctrlclix.WithoutQuorum)
			if kerrors.IsNotFound(err) && r.APIReader != nil {
				err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: cqName}, cq)
			}
			return cq, err
		}
		return nil, err
	}
	logger.V(2).Info("created cluster queue")
	return cq, nil
}

func (r *AcceleratorTypeReconciler) teardownAcceleratorType(
	ctx context.Context, at *workercore.AcceleratorType,
) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	if !systemmeta.IsLocked(at) {
		return ctrl.Result{}, nil
	}

	cqName := AcceleratorTypePoolQueueName(ctx, at)
	anyNonDeleting, anyActive, err := poolHasActiveFrontingObjectsWithFallback(ctx, r.Client, r.APIReader, cqName, at)
	if err != nil {
		logger.Error(err, "check remaining fronting objects on pool cluster queue")
		return ctrl.Result{}, err
	}
	cq := new(kueue.ClusterQueue)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: cqName}, cq, ctrlclix.WithoutQuorum)
	switch {
	case err == nil:
		if !anyNonDeleting || cq.DeletionTimestamp != nil {
			if at.Status.Phase != InstanceTypePhaseInactive {
				at.Status = workercore.AcceleratorTypeStatus{
					Phase:        InstanceTypePhaseInactive,
					PhaseMessage: "accelerator type is terminating",
					Entrance:     nodefeature.FormatLocalQueueName(cq.Name),
				}
				if err = r.Client.Status().Update(ctx, at); err != nil {
					return objectWriteResult(logger, err, "mark accelerator type inactive while terminating", _requeueAfterConflict)
				}
			}
			if cq.DeletionTimestamp == nil {
				if err = r.Client.Delete(ctx, cq); err != nil && !kerrors.IsNotFound(err) {
					logger.Error(err, "delete cluster queue")
					return ctrl.Result{}, err
				}
				logger.V(2).Info("requested cluster queue deletion; the node queue drains it")
			}
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		if !anyActive &&
			cq.Annotations[_TASQueueMigrationPhaseAnnotation] == "" &&
			ptr.Deref(cq.Spec.StopPolicy, kueue.None) == kueue.None {
			cq.Spec.StopPolicy = ptr.To(kueue.Hold)
			if err = r.Client.Update(ctx, cq); err != nil {
				return objectWriteResult(logger, err, "hold cluster queue after last active fronting object deleted", _requeueAfterConflict)
			}
		}
	case !kerrors.IsNotFound(err):
		logger.Error(err, "fetch cluster queue")
		return ctrl.Result{}, err
	}

	if prevCQName := acceleratorTypePreviousPoolQueueName(ctx, at, cqName); prevCQName != "" {
		stillPresent, rerr := reapDisplacedPoolClusterQueue(ctx, r.Client, r.APIReader, prevCQName, at)
		if rerr != nil {
			logger.Error(rerr, "reap displaced cluster queue during teardown", "clusterQueue", prevCQName)
			return ctrl.Result{}, rerr
		}
		if stillPresent {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}

	systemmeta.Unlock(at)
	if err = r.Client.Update(ctx, at); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	logger.V(2).Info("released drained accelerator type")
	return ctrl.Result{}, nil
}

func acceleratorTypePreviousPoolQueueName(ctx context.Context, at *workercore.AcceleratorType, currCQName string) string {
	if at == nil || at.Status.Entrance == "" || at.Status.Entrance == nodefeature.FormatLocalQueueName(currCQName) {
		return ""
	}
	altAware := !settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx)
	for _, candidate := range []string{
		nodefeature.CanonicalPoolQueueName(
			true, altAware,
			at.Spec.GeneralGroup, at.Spec.AcceleratorGroup,
			at.Spec.OS, at.Spec.Arch,
		),
		at.Name,
	} {
		if candidate != "" && candidate != currCQName && nodefeature.FormatLocalQueueName(candidate) == at.Status.Entrance {
			return candidate
		}
	}
	return ""
}

func (r *AcceleratorTypeReconciler) computeStatus(
	ctx context.Context, at *workercore.AcceleratorType, cq *kueue.ClusterQueue,
) workercore.AcceleratorTypeStatus {
	draining := ptr.Deref(cq.Spec.StopPolicy, kueue.None) == kueue.HoldAndDrain

	var st workercore.AcceleratorTypeStatus
	devices := r.listFlavorPoolDevices(ctx, cq)
	if !draining {
		st.Accelerator, st.AcceleratorShared, st.AcceleratorSliced, st.AcceleratorPartitioned = getAcceleratorResources(devices, at.Spec.AcceleratorGroup)
	}
	st.Detail = r.computeDetail(ctx, at, devices)
	publishAcceleratorTypePartitionProfileNames(&st)
	st.Entrance = nodefeature.FormatLocalQueueName(cq.Name)
	st.Phase, st.PhaseMessage = apistatus.GetSummaryOfClusterQueue(cq)
	if at.Spec.Inactive && st.Phase == InstanceTypePhaseActive {
		st.Phase = InstanceTypePhaseInactive
		st.PhaseMessage = "Deactivated by administrator"
	}
	return st
}

func publishAcceleratorTypePartitionProfileNames(st *workercore.AcceleratorTypeStatus) {
	manufacturer := st.Detail.Manufacturer
	if manufacturer == "" {
		return
	}

	profiles := st.Detail.SlicedDetail.Physical.Profiles
	for i := range profiles {
		profiles[i].Name = nodefeature.PublishPartitionedProfileName(manufacturer, profiles[i].Name)
	}
	for _, counts := range [][]workercore.AcceleratorProfileCount{
		st.AcceleratorPartitioned.AllocatedProfiles,
		st.AcceleratorPartitioned.RemainingProfiles,
	} {
		for i := range counts {
			counts[i].Name = nodefeature.PublishPartitionedProfileName(manufacturer, counts[i].Name)
		}
		slices.SortFunc(counts, func(a, b workercore.AcceleratorProfileCount) int {
			return cmp.Compare(a.Name, b.Name)
		})
	}
}

func (r *AcceleratorTypeReconciler) computeDetail(
	ctx context.Context, at *workercore.AcceleratorType, devices []workercore.Devices,
) workercore.InstanceTypeDetail {
	lbs := systemmeta.GetResourcesLabelSetOfType[ctrlcli.MatchingLabels](_ResourceFlavorResType)
	for k, v := range AcceleratorTypeScheduleLabels(ctx, at) {
		lbs[k] = v
	}
	rfList := new(kueue.ResourceFlavorList)
	err := r.Client.List(ctx, rfList, lbs,
		ctrlclix.WithoutQuorum, ctrlcli.UnsafeDisableDeepCopy, ctrlcli.Limit(1))
	if err != nil {
		ctrllog.FromContext(ctx).Error(err, "list resource flavor for accelerator type detail")
		return workercore.InstanceTypeDetail{}
	}
	if len(rfList.Items) == 0 {
		ctrllog.FromContext(ctx).V(3).
			Info("no matching resource flavor for accelerator type detail; refresh on later reconcile")
		return workercore.InstanceTypeDetail{}
	}
	_, notes := systemmeta.DescribeResource(&rfList.Items[0])

	detail := workercore.InstanceTypeDetail{
		Manufacturer:    notes["manufacturer"],
		Product:         notes["product"],
		Family:          notes["family"],
		Memory:          notes["memory"],
		Cores:           notes["cores"],
		SlicedDetail:    poolAcceleratorSlicedDetail(devices, at.Spec.AcceleratorGroup),
		RuntimeVersions: poolAcceleratorRuntimeVersions(devices, at.Spec.AcceleratorGroup),
	}
	if len(detail.RuntimeVersions) > 0 {
		detail.RuntimeVersion = detail.RuntimeVersions[0]
	}
	if settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx) {
		foldDetailCPU(&detail, notes["cpuDetail"], true)
	}
	return detail
}

func (r *AcceleratorTypeReconciler) listFlavorPoolDevices(ctx context.Context, cq *kueue.ClusterQueue) []workercore.Devices {
	sel := poolDevicesSelector(cq.Labels)
	if len(sel) == 0 {
		return nil
	}
	list := new(workercore.DevicesList)
	if err := r.Client.List(ctx, list, ctrlcli.MatchingLabels(sel),
		ctrlclix.WithoutQuorum, ctrlcli.UnsafeDisableDeepCopy); err != nil {
		return nil
	}
	return list.Items
}

// AcceleratorTypePoolQueueName returns the canonical ClusterQueue name backing the given AcceleratorType.
// When the spec identity fields are unset (as in bare unit-test fixtures), it falls back to the
// AcceleratorType's own name.
func AcceleratorTypePoolQueueName(ctx context.Context, at *workercore.AcceleratorType) string {
	if at == nil {
		return ""
	}
	if at.Spec.OS == "" || at.Spec.Arch == "" || at.Spec.AcceleratorGroup == "" {
		return at.Name
	}
	return nodefeature.CanonicalPoolQueueName(
		true,
		settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx),
		at.Spec.GeneralGroup,
		at.Spec.AcceleratorGroup,
		at.Spec.OS,
		at.Spec.Arch,
	)
}

// AcceleratorTypeScheduleLabels builds the schedule discriminator labels stamped on the backing
// queue from the AcceleratorType spec identity and the CPU-manufacturer awareness setting.
func AcceleratorTypeScheduleLabels(ctx context.Context, at *workercore.AcceleratorType) map[string]string {
	cpuAware := settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx)
	return nodefeature.PoolScheduleLabels(
		true, cpuAware,
		at.Spec.GeneralGroup, at.Spec.AcceleratorGroup,
		at.Spec.OS, at.Spec.Arch,
	)
}

// AcceleratorTypePartitionProfileMemoryPercent reports the share of one card's VRAM the requested
// hardware partition profile occupies on an AcceleratorType, as a percentage in [1,100].
func AcceleratorTypePartitionProfileMemoryPercent(
	at *workercore.AcceleratorType, profile string,
) (pct int64, sizeable bool) {
	if profile == "" {
		return 0, true
	}
	prof, _, found := AcceleratorTypePartitionProfile(at, profile)
	if !found {
		return 0, true
	}
	if prof.MemoryMib <= 0 {
		return 0, false
	}
	cardVRAMMib, err := AcceleratorTypeCardVRAMMib(at)
	if err != nil || cardVRAMMib <= 0 {
		return 0, false
	}
	return min(max(prof.MemoryMib*100/cardVRAMMib, 1), 100), true
}

// AcceleratorTypePartitionProfile finds a partition profile in an AcceleratorType's observed
// physical-slice inventory (Status.Detail).
func AcceleratorTypePartitionProfile(
	at *workercore.AcceleratorType, profile string,
) (prof workercore.AcceleratorSlicedPhysicalDetailProfile, ready, found bool) {
	ready = at.Status.Detail.AcceleratorReady()
	for _, p := range at.Status.Detail.SlicedDetail.Physical.Profiles {
		if p.Name == profile {
			return p, ready, true
		}
	}
	return workercore.AcceleratorSlicedPhysicalDetailProfile{}, ready, false
}

// AcceleratorTypeCardVRAMMib parses the per-card VRAM (MiB) from an AcceleratorType's observed
// Status.Detail.Memory.
func AcceleratorTypeCardVRAMMib(at *workercore.AcceleratorType) (int64, error) {
	memStr := at.Status.Detail.Memory
	if memStr == "" {
		return 0, fmt.Errorf("accelerator type %s has no per-card memory yet (detail not ready)", at.Name)
	}
	q, err := resource.ParseQuantity(memStr)
	if err != nil {
		return 0, fmt.Errorf("parse memory %q of accelerator type %s: %w", memStr, at.Name, err)
	}
	mib := q.Value() / quantityx.Mi
	if mib <= 0 {
		return 0, fmt.Errorf("accelerator type %s has non-positive memory %q", at.Name, memStr)
	}
	return mib, nil
}

func (r *AcceleratorTypeReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	dedupWindow := ctrlhandlerx.NewDedupWindow[ctrlreconcile.Request]()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("acceleratortype").
		For(
			&workercore.AcceleratorType{},
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				DeleteFunc: func(ctrlevent.DeleteEvent) bool { return false },
				UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
					oldAt, newAt := e.ObjectOld.(*workercore.AcceleratorType), e.ObjectNew.(*workercore.AcceleratorType)
					if !oldAt.DeletionTimestamp.Equal(newAt.DeletionTimestamp) {
						return true
					}
					return oldAt.Generation != newAt.Generation
				},
			}),
		).
		Watches(
			&kueue.ClusterQueue{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueAcceleratorTypeWhenClusterQueueChanged,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return systemmeta.MatchResource(obj, _ClusterQueueResType)
				}),
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						oldCQ, ok1 := e.ObjectOld.(*kueue.ClusterQueue)
						newCQ, ok2 := e.ObjectNew.(*kueue.ClusterQueue)
						return !ok1 || !ok2 || clusterQueueUpdateAffectsAcceleratorType(oldCQ, newCQ)
					},
				},
			),
		).
		Watches(
			&workercore.Devices{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFuncWithWindow(
				3*time.Second,
				dedupWindow,
				r.enqueueAcceleratorTypeWhenDevicesChanged,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					return obj.GetLabels()[systemname.ManagedLabelKey] == "true"
				}),
			),
		).
		Complete(r)
}

// clusterQueueUpdateAffectsAcceleratorType filters out steady-state Kueue reservation status churn
// (AdmittedWorkloads, PendingWorkloads, FlavorsReservation) on active accelerated queues: unlike
// CPU-only InstanceTypes, AcceleratorType reads capacity from Devices and reads only metadata,
// StopPolicy/ResourceGroups, and phase/message summary from ClusterQueue.
func clusterQueueUpdateAffectsAcceleratorType(oldCQ, newCQ *kueue.ClusterQueue) bool {
	if !oldCQ.DeletionTimestamp.Equal(newCQ.DeletionTimestamp) {
		return true
	}
	if oldCQ.Generation != newCQ.Generation ||
		ptr.Deref(oldCQ.Spec.StopPolicy, kueue.None) != ptr.Deref(newCQ.Spec.StopPolicy, kueue.None) ||
		(len(oldCQ.Spec.ResourceGroups) == 0) != (len(newCQ.Spec.ResourceGroups) == 0) {
		return true
	}
	if !maps.Equal(oldCQ.Labels, newCQ.Labels) || !maps.Equal(oldCQ.Annotations, newCQ.Annotations) {
		return true
	}
	oldPhase, oldMsg := apistatus.GetSummaryOfClusterQueue(oldCQ)
	newPhase, newMsg := apistatus.GetSummaryOfClusterQueue(newCQ)
	return oldPhase != newPhase || oldMsg != newMsg
}

func (r *AcceleratorTypeReconciler) enqueueAcceleratorTypeWhenClusterQueueChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("cluster queue", ctrlcli.ObjectKeyFromObject(obj))

	seen := map[string]bool{obj.GetName(): true}
	reqs := []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKeyFromObject(obj),
		},
	}

	atList := new(workercore.AcceleratorTypeList)
	err := r.Client.List(ctx, atList, ctrlclix.WithoutQuorum, ctrlcli.UnsafeDisableDeepCopy)
	if err != nil && r.APIReader != nil {
		err = r.APIReader.List(ctx, atList, ctrlclix.WithoutQuorum)
	}
	if err != nil {
		logger.Error(err, "list accelerator types for cluster queue")
		return reqs
	}
	for i := range atList.Items {
		at := &atList.Items[i]
		if seen[at.Name] || AcceleratorTypePoolQueueName(ctx, at) != obj.GetName() {
			continue
		}
		seen[at.Name] = true
		reqs = append(reqs, ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{Name: at.Name},
		})
	}

	logger.V(2).Info("enqueued accelerator type from cluster queue", "requests", reqs)
	return reqs
}

func (r *AcceleratorTypeReconciler) enqueueAcceleratorTypeWhenDevicesChanged(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx).
		WithValues("devices", ctrlcli.ObjectKeyFromObject(obj))

	labels := obj.GetLabels()
	os, arch := labels[core.LabelOSStable], labels[core.LabelArchStable]

	var accelKeys []string
	for k, v := range labels {
		if v == "true" && strings.HasPrefix(k, nodefeature.AcceleratableFeatureLabelPrefix) {
			accelKeys = append(accelKeys, k)
		}
	}
	if len(accelKeys) == 0 {
		logger.V(3).Info("devices has no schedule labels or no matching accelerator type, skip")
		return nil
	}

	var opts []ctrlcli.ListOption
	match := ctrlcli.MatchingLabels{}
	if os != "" {
		match[core.LabelOSStable] = os
	}
	if arch != "" {
		match[core.LabelArchStable] = arch
	}
	if len(match) > 0 {
		opts = append(opts, match)
	}
	opts = append(opts, ctrlclix.WithoutQuorum, ctrlcli.UnsafeDisableDeepCopy)

	atList := new(workercore.AcceleratorTypeList)
	if err := r.Client.List(ctx, atList, opts...); err != nil {
		logger.Error(err, "list accelerator types by devices labels")
		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range atList.Items {
		atLabels := atList.Items[i].Labels
		for _, k := range accelKeys {
			if atLabels[k] == "true" {
				reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: atList.Items[i].Name}})
				break
			}
		}
	}
	if len(reqs) == 0 {
		logger.V(3).Info("devices has no schedule labels or no matching accelerator type, skip")
		return nil
	}

	logger.V(2).Info("enqueued accelerator types from devices", "requests", reqs)
	return reqs
}
