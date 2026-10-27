package worker

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
)

const (
	modelDeploymentScalerPendingTimeoutDefault = 300 * time.Second
	modelDeploymentScalerInitialTerm           = int64(1)
	modelDeploymentScalerMinimumPendingRequeue = time.Second
)

// ModelDeploymentScalerReconciler plans replica vectors for ModelDeployments.
type ModelDeploymentScalerReconciler struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
	Recorder  ctrlrecord.EventRecorder

	clock   func() time.Time
	sampler *modelDeploymentScalerSampler
}

func (r *ModelDeploymentScalerReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()
	r.Recorder = opts.Manager.GetEventRecorderFor("modeldeploymentscaler")
	r.clock = time.Now
	r.sampler = &modelDeploymentScalerSampler{HTTPClient: defaultModelDeploymentScalerHTTPClient}

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modeldeploymentscaler").
		For(&workercore.ModelDeploymentScaler{}, ctrlbuilder.WithPredicates(modelDeploymentScalerPredicate())).
		Watches(
			&workercore.ModelDeployment{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapTargetDeployment),
		).
		Watches(&core.ConfigMap{}, ctrlhandler.EnqueueRequestsFromMapFunc(r.mapCapacityPriorCatalog)).
		Watches(
			&kueue.Workload{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.mapTargetWorkload),
			ctrlbuilder.WithPredicates(modelDeploymentScalerWorkloadPredicate()),
		).
		Complete(r)
}

// Reconcile makes the scaler's target binding and status match its current mode and observations.
func (r *ModelDeploymentScalerReconciler) Reconcile(
	ctx context.Context, req ctrlreconcile.Request,
) (ctrlreconcile.Result, error) {
	scaler := new(workercore.ModelDeploymentScaler)
	if err := r.Client.Get(ctx, req.NamespacedName, scaler); err != nil {
		return ctrlreconcile.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	now := r.now()

	if !scaler.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, scaler)
	}
	if !slices.Contains(scaler.Finalizers, workercore.ModelDeploymentScalerFinalizerKey) {
		scaler.Finalizers = append(scaler.Finalizers, workercore.ModelDeploymentScalerFinalizerKey)
		if err := r.Client.Update(ctx, scaler); err != nil {
			return ctrlreconcile.Result{}, err
		}
		return ctrlreconcile.Result{Requeue: true}, nil
	}

	md := new(workercore.ModelDeployment)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: scaler.Namespace, Name: scaler.Spec.TargetRef.Name}, md); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrlreconcile.Result{}, err
		}
		before := scaler.DeepCopy()
		r.invalidateCapacityPriorState(scaler, workercore.ModelDeploymentScalerReasonTargetNotFound, "the target does not exist", now)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonTargetNotFound, "the target ModelDeployment does not exist", now)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonTargetNotFound, "the target ModelDeployment does not exist", now)
		scaler.Status.ObservedGeneration = scaler.Generation
		if err := r.updateStatus(ctx, before, scaler); err != nil {
			return ctrlreconcile.Result{}, err
		}
		return ctrlreconcile.Result{RequeueAfter: r.interval(scaler)}, nil
	}

	if scaler.Status.Binding != nil && scaler.Status.Binding.TargetUID != "" && scaler.Status.Binding.TargetUID != string(md.UID) {
		if scaler.Status.ObservedGeneration >= scaler.Generation {
			before := scaler.DeepCopy()
			r.invalidateCapacityPriorState(scaler, workercore.ModelDeploymentScalerReasonTargetReplaced, "the target was replaced", now)
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonTargetReplaced, "the target was replaced; edit the Scaler spec to bind the new target", now)
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonTargetReplaced, "the target was replaced", now)
			scaler.Status.ObservedGeneration = scaler.Generation
			if err := r.updateStatus(ctx, before, scaler); err != nil {
				return ctrlreconcile.Result{}, err
			}
			prevReason := modelDeploymentScalerConditionReason(before, workercore.ModelDeploymentScalerConditionTypeBound)
			if r.Recorder != nil && prevReason != workercore.ModelDeploymentScalerReasonTargetReplaced {
				r.Recorder.Event(
					scaler, core.EventTypeWarning,
					workercore.ModelDeploymentScalerReasonTargetReplaced,
					"the target was replaced; edit the scaler spec to bind the new target",
				)
			}
			return ctrlreconcile.Result{RequeueAfter: r.interval(scaler)}, nil
		}
		r.resetTargetState(scaler)
		r.sampler.Forget(scaler)
	}
	if scaler.Spec.Suspend || scaler.Spec.Mode == workercore.ModelDeploymentScalerModeShadow || scaler.Spec.Mode == "" {
		if err := r.releaseOwnedBinding(ctx, scaler, md); err != nil {
			return ctrlreconcile.Result{}, err
		}
		if scaler.Spec.Suspend {
			before := scaler.DeepCopy()
			r.invalidateCapacityPriorState(scaler, workercore.ModelDeploymentScalerReasonSuspended, "the scaler is suspended", now)
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonSuspended, "the scaler is suspended", now)
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonSuspended, "the scaler is suspended", now)
			scaler.Status.ObservedGeneration = scaler.Generation
			if err := r.updateStatus(ctx, before, scaler); err != nil {
				return ctrlreconcile.Result{}, err
			}
			return ctrlreconcile.Result{}, nil
		}
		if binding, ok, err := readScalerBinding(md); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "the target's scaler binding annotation cannot be read", "target", md.Name)
			if r.Recorder != nil {
				r.Recorder.Eventf(scaler, core.EventTypeWarning, workercore.ModelDeploymentScalerReasonInvalidBinding,
					"the target's scaler binding annotation cannot be read: %v", err)
			}
			before := scaler.DeepCopy()
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonInvalidBinding, "the target's scaler binding annotation cannot be read", now)
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonInvalidBinding, "the target's scaler binding annotation cannot be read", now)
			scaler.Status.ObservedGeneration = scaler.Generation
			if err := r.updateStatus(ctx, before, scaler); err != nil {
				return ctrlreconcile.Result{}, err
			}
			return ctrlreconcile.Result{RequeueAfter: r.interval(scaler)}, nil
		} else if ok && binding.ScalerUID != string(scaler.UID) {
			before := scaler.DeepCopy()
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonAlreadyBound, "another scaler owns the target", now)
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonAlreadyBound, "another scaler owns the target", now)
			scaler.Status.ObservedGeneration = scaler.Generation
			if err := r.updateStatus(ctx, before, scaler); err != nil {
				return ctrlreconcile.Result{}, err
			}
			return ctrlreconcile.Result{RequeueAfter: r.interval(scaler)}, nil
		}
		before := scaler.DeepCopy()
		if scaler.Status.Binding == nil {
			scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: string(md.UID), TargetName: md.Name}
		} else {
			scaler.Status.Binding.TargetUID = string(md.UID)
			scaler.Status.Binding.TargetName = md.Name
		}
		r.recoverDecision(ctx, scaler, md)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionTrue, "Bound", "the target is available", now)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonShadowMode, "the scaler is observing without changing the target", now)
		scaler.Status.ObservedGeneration = scaler.Generation
		result, err := r.plan(ctx, before, scaler, md, now, false)
		if err != nil {
			return ctrlreconcile.Result{}, err
		}
		return result, nil
	}

	binding, hasBinding, err := readScalerBinding(md)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "the target's scaler binding annotation cannot be read", "target", md.Name)
		if r.Recorder != nil {
			r.Recorder.Eventf(scaler, core.EventTypeWarning, workercore.ModelDeploymentScalerReasonInvalidBinding,
				"the target's scaler binding annotation cannot be read: %v", err)
		}
		before := scaler.DeepCopy()
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonInvalidBinding, "the target's scaler binding annotation cannot be read", now)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonInvalidBinding, "the target's scaler binding annotation cannot be read", now)
		scaler.Status.ObservedGeneration = scaler.Generation
		if err := r.updateStatus(ctx, before, scaler); err != nil {
			return ctrlreconcile.Result{}, err
		}
		return ctrlreconcile.Result{RequeueAfter: r.interval(scaler)}, nil
	}
	if hasBinding && binding.ScalerUID != string(scaler.UID) {
		before := scaler.DeepCopy()
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonAlreadyBound, "another scaler owns the target", now)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonAlreadyBound, "another scaler owns the target", now)
		scaler.Status.ObservedGeneration = scaler.Generation
		if err := r.updateStatus(ctx, before, scaler); err != nil {
			return ctrlreconcile.Result{}, err
		}
		return ctrlreconcile.Result{RequeueAfter: r.interval(scaler)}, nil
	}
	term := modelDeploymentScalerInitialTerm
	if hasBinding {
		term = maxInt64(modelDeploymentScalerInitialTerm, binding.Term)
		if scaler.Status.Binding != nil {
			term = maxInt64(term, scaler.Status.Binding.Term)
		}
	} else if scaler.Status.Binding != nil {
		term = maxInt64(modelDeploymentScalerInitialTerm, scaler.Status.Binding.Term+1)
	}
	if !hasBinding || binding.ScalerUID != string(scaler.UID) || binding.Term != term {
		resolved, resolutionErr := ResolveModelDeploymentScalerCapacityPriors(ctx, r.priorReader(), scaler, md)
		if resolutionErr != nil {
			before := scaler.DeepCopy()
			r.applyCapacityPriorResolution(scaler, resolved, resolutionErr, now)
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonCapacityPriorUnresolved, "capacity priors must resolve before acquiring a binding", now)
			return r.stopForCapacityPrior(ctx, before, scaler, resolutionErr, now)
		}
		if md.Annotations == nil {
			md.Annotations = make(map[string]string)
		}
		value, marshalErr := json.Marshal(workercore.ModelDeploymentScalerBinding{
			ScalerName: scaler.Name, ScalerUID: string(scaler.UID), Term: term,
		})
		if marshalErr != nil {
			return ctrlreconcile.Result{}, marshalErr
		}
		md.Annotations[workercore.ModelDeploymentScalerBindingAnnotationKey] = string(value)
		if err := r.Client.Update(ctx, md); err != nil {
			return ctrlreconcile.Result{}, err
		}
		if err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(md), md); err != nil {
			return ctrlreconcile.Result{}, err
		}
	}
	before := scaler.DeepCopy()
	scaler.Status.Binding = &workercore.ModelDeploymentScalerBindingStatus{TargetUID: string(md.UID), TargetName: md.Name, Term: term}
	r.recoverDecision(ctx, scaler, md)
	r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionTrue, "Bound", "the scaler owns the target", now)
	r.setCondition(
		scaler, workercore.ModelDeploymentScalerConditionTypeActive,
		meta.ConditionTrue, "Active", "the scaler is planning replica changes", now,
	)

	return r.plan(ctx, before, scaler, md, now, true)
}

// modelDeploymentScalerPredicate decides which changes of a scaler wake its reconciler.
//
// IT IGNORES THE STATUS OF A SCALER, because a status write is the reconciler's own doing:
// a scaler that wrote its status would otherwise wake itself, and a scaler that samples
// every few seconds would reconcile as fast as it writes instead of as fast as it samples.
func modelDeploymentScalerPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.Funcs{
		CreateFunc:  func(ctrlevent.CreateEvent) bool { return true },
		DeleteFunc:  func(ctrlevent.DeleteEvent) bool { return true },
		GenericFunc: func(ctrlevent.GenericEvent) bool { return true },
		UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
				return true
			}
			if e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero() {
				return true
			}
			return !slices.Equal(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers())
		},
	}
}

// modelDeploymentScalerWorkloadPredicate filters Workload events to those belonging to a ModelDeployment.
func modelDeploymentScalerWorkloadPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
		return obj.GetLabels()[modelDeploymentLabelKeyInstance] != ""
	})
}

func (r *ModelDeploymentScalerReconciler) reconcileDeletion(
	ctx context.Context, scaler *workercore.ModelDeploymentScaler,
) (ctrlreconcile.Result, error) {
	if r.sampler != nil {
		r.sampler.Forget(scaler)
	}
	if slices.Contains(scaler.Finalizers, workercore.ModelDeploymentScalerFinalizerKey) {
		md := new(workercore.ModelDeployment)
		err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: scaler.Namespace, Name: scaler.Spec.TargetRef.Name}, md)
		if err == nil {
			if err := r.releaseOwnedBinding(ctx, scaler, md); err != nil {
				return ctrlreconcile.Result{}, err
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrlreconcile.Result{}, err
		}
		scaler.Finalizers = slices.DeleteFunc(scaler.Finalizers, func(value string) bool {
			return value == workercore.ModelDeploymentScalerFinalizerKey
		})
		if err := r.Client.Update(ctx, scaler); err != nil {
			return ctrlreconcile.Result{}, err
		}
	}
	return ctrlreconcile.Result{}, nil
}

func (r *ModelDeploymentScalerReconciler) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

func (r *ModelDeploymentScalerReconciler) interval(scaler *workercore.ModelDeploymentScaler) time.Duration {
	seconds := scaler.Spec.Sampling.IntervalSeconds
	if seconds <= 0 {
		seconds = modelDeploymentScalerSamplerDefaultIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (r *ModelDeploymentScalerReconciler) mapTargetDeployment(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	list := new(workercore.ModelDeploymentScalerList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(obj.GetNamespace()), ctrlclix.WithoutQuorum); err != nil {
		ctrl.LoggerFrom(ctx).Error(
			err, "failed to list ModelDeploymentScalers for ModelDeployment event", "target", obj.GetName(),
		)
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range list.Items {
		scaler := &list.Items[i]
		if scaler.Spec.TargetRef.Name == obj.GetName() {
			requests = append(requests, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(scaler)})
		}
	}
	return requests
}

func (r *ModelDeploymentScalerReconciler) mapTargetWorkload(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	list := new(workercore.ModelDeploymentScalerList)
	if err := r.Client.List(ctx, list, ctrlcli.InNamespace(obj.GetNamespace()), ctrlclix.WithoutQuorum); err != nil {
		ctrl.LoggerFrom(ctx).Error(
			err, "failed to list ModelDeploymentScalers for Workload event", "workload", obj.GetName(),
		)
		return nil
	}
	if len(list.Items) == 0 {
		return nil
	}
	targetName := obj.GetLabels()[modelDeploymentLabelKeyInstance]
	if targetName == "" {
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range list.Items {
		scaler := &list.Items[i]
		if scaler.Spec.TargetRef.Name == targetName {
			requests = append(requests, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(scaler)})
		}
	}
	return requests
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func cloneReplicaVector(source map[string]int32) map[string]int32 {
	if source == nil {
		return nil
	}
	result := make(map[string]int32, len(source))
	for role, count := range source {
		result[role] = count
	}
	return result
}

func equalReplicaVectors(a, b map[string]int32) bool {
	if len(a) != len(b) {
		return false
	}
	for role, count := range a {
		if b[role] != count {
			return false
		}
	}
	return true
}

var _ ctrlreconcile.Reconciler = (*ModelDeploymentScalerReconciler)(nil)
