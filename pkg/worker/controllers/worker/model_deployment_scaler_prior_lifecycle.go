package worker

import (
	"context"
	"errors"
	"maps"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

func (r *ModelDeploymentScalerReconciler) priorReader() ctrlcli.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func modelDeploymentScalerPriorFingerprints(statuses []workercore.ModelDeploymentScalerResolvedCapacityPriorStatus) map[string]string {
	fingerprints := make(map[string]string, len(statuses))
	for _, status := range statuses {
		fingerprints[status.Name] = status.CompatibilityFingerprint
	}
	return fingerprints
}

func (r *ModelDeploymentScalerReconciler) clearCapacityPriorState(scaler *workercore.ModelDeploymentScaler) {
	if !modelDeploymentScalerPriorRequested(scaler) && len(scaler.Status.ResolvedCapacityPriors) == 0 {
		return
	}
	scaler.Status.ResolvedCapacityPriors = nil
	scaler.Status.Predictors = nil
	for i := range scaler.Status.Roles {
		scaler.Status.Roles[i].StabilizationHistory = nil
	}
	if r.sampler != nil {
		r.sampler.Forget(scaler)
	}
}

func (r *ModelDeploymentScalerReconciler) invalidateCapacityPriorState(
	scaler *workercore.ModelDeploymentScaler, reason, message string, now time.Time,
) {
	if !modelDeploymentScalerPriorRequested(scaler) && len(scaler.Status.ResolvedCapacityPriors) == 0 &&
		modelDeploymentScalerConditionReason(scaler, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady) == "" {
		return
	}
	r.clearCapacityPriorState(scaler)
	r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady, meta.ConditionUnknown, reason, message, now)
}

func modelDeploymentScalerPriorAPIError(err error) error {
	var refusal *priors.RefusalError
	if errors.As(err, &refusal) {
		return nil
	}
	return err
}

func (r *ModelDeploymentScalerReconciler) applyCapacityPriorResolution(
	scaler *workercore.ModelDeploymentScaler, resolved *ModelDeploymentScalerCapacityPriors, resolutionErr error, now time.Time,
) {
	if resolutionErr != nil {
		r.clearCapacityPriorState(scaler)
		reason, message := workercore.ModelDeploymentScalerReasonCapacityPriorUnresolved, "capacity prior dependencies could not be read"
		var refusal *priors.RefusalError
		if errors.As(resolutionErr, &refusal) {
			reason, message = string(refusal.Reason), refusal.Error()
		}
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady, meta.ConditionFalse, reason, message, now)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse, reason, message, now)
		return
	}
	if !maps.Equal(
		modelDeploymentScalerPriorFingerprints(scaler.Status.ResolvedCapacityPriors), modelDeploymentScalerPriorFingerprints(resolved.Status),
	) {
		r.clearCapacityPriorState(scaler)
	}
	scaler.Status.ResolvedCapacityPriors = resolved.Status
	if modelDeploymentScalerPriorRequested(scaler) {
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady, meta.ConditionTrue,
			"Resolved", "all required capacities are explicit or resolved from compatible catalog profiles", now)
	} else if modelDeploymentScalerConditionReason(scaler, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady) != "" {
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady, meta.ConditionTrue,
			"Manual", "every capacity is stated by hand", now)
	}
}

func (r *ModelDeploymentScalerReconciler) stopForCapacityPrior(
	ctx context.Context, before, scaler *workercore.ModelDeploymentScaler, resolutionErr error, now time.Time,
) (ctrlreconcile.Result, error) {
	var refusal *priors.RefusalError
	if !errors.As(resolutionErr, &refusal) {
		scaler.Status.ObservedGeneration = scaler.Generation
		return ctrlreconcile.Result{}, errors.Join(resolutionErr, r.updateStatus(ctx, before, scaler))
	}
	r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked, meta.ConditionTrue,
		string(refusal.Reason), refusal.Error(), now)
	scaler.Status.ObservedGeneration = scaler.Generation
	if err := r.updateStatus(ctx, before, scaler); err != nil {
		return ctrlreconcile.Result{}, err
	}
	return ctrlreconcile.Result{RequeueAfter: r.interval(scaler)}, nil
}

func (r *ModelDeploymentScalerReconciler) recheckCapacityPriors(
	ctx context.Context, scaler *workercore.ModelDeploymentScaler, target *workercore.ModelDeployment, planned *ModelDeploymentScalerCapacityPriors,
) error {
	if !modelDeploymentScalerPriorRequested(scaler) {
		return nil
	}
	latestScaler := new(workercore.ModelDeploymentScaler)
	if err := r.priorReader().Get(ctx, ctrlcli.ObjectKeyFromObject(scaler), latestScaler); err != nil {
		return err
	}
	if latestScaler.UID != scaler.UID || latestScaler.Generation != scaler.Generation ||
		latestScaler.Spec.Suspend || latestScaler.Spec.Mode != workercore.ModelDeploymentScalerModeActive {
		return modelDeploymentScalerPriorRefusal("", "scaler configuration changed while planning; retry resolution")
	}
	latestTarget := new(workercore.ModelDeployment)
	if err := r.priorReader().Get(ctx, ctrlcli.ObjectKeyFromObject(target), latestTarget); err != nil {
		return err
	}
	resolved, err := ResolveModelDeploymentScalerCapacityPriors(ctx, r.priorReader(), latestScaler, latestTarget)
	if err != nil {
		return err
	}
	if !maps.Equal(modelDeploymentScalerPriorFingerprints(planned.Status), modelDeploymentScalerPriorFingerprints(resolved.Status)) {
		return modelDeploymentScalerPriorRefusal("", "capacity compatibility changed while planning; retry resolution")
	}
	return nil
}

func (r *ModelDeploymentScalerReconciler) mapCapacityPriorCatalog(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	scalers := new(workercore.ModelDeploymentScalerList)
	if err := r.Client.List(ctx, scalers, ctrlcli.InNamespace(obj.GetNamespace()), ctrlclix.WithoutQuorum); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list ModelDeploymentScalers for catalog event", "catalog", obj.GetName())
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range scalers.Items {
		scaler := &scalers.Items[i]
		if modelDeploymentScalerPriorRequested(scaler) && scaler.Spec.Algorithm.CapacityPrior.CatalogRef != nil &&
			scaler.Spec.Algorithm.CapacityPrior.CatalogRef.Name == obj.GetName() {
			requests = append(requests, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(scaler)})
		}
	}
	return requests
}
