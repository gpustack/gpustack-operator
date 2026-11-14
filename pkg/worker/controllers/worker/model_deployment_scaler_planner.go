package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

// plan computes the replica vector of a scaler and writes the state it observed.
//
// THE BEFORE SNAPSHOT IS THE ONE THE RECONCILE TOOK, because the status of the scaler is
// written once at the end of the plan and the write has to carry every change the reconcile
// made, not only the ones the plan made.
func (r *ModelDeploymentScalerReconciler) plan(
	ctx context.Context,
	before *workercore.ModelDeploymentScaler,
	scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
	now time.Time,
	active bool,
) (ctrlreconcile.Result, error) {
	return r.planWithPriors(ctx, before, scaler, md, nil, now, active)
}

func (r *ModelDeploymentScalerReconciler) planWithPriors(
	ctx context.Context,
	before *workercore.ModelDeploymentScaler,
	scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
	preResolved *ModelDeploymentScalerCapacityPriors,
	now time.Time,
	active bool,
) (ctrlreconcile.Result, error) {
	var (
		resolved      *ModelDeploymentScalerCapacityPriors
		resolutionErr error
	)
	if preResolved != nil {
		resolved = preResolved
	} else {
		resolved, resolutionErr = ResolveModelDeploymentScalerCapacityPriors(ctx, r.priorReader(), scaler, md)
	}
	r.applyCapacityPriorResolution(scaler, resolved, resolutionErr, now)
	pods, pendingByRole, cohorts, err := r.executionState(ctx, md)
	if err != nil {
		return ctrlreconcile.Result{}, err
	}
	observations := r.sampler.SampleRoleMetrics(ctx, scaler, md, pods, now)
	samples := r.sampler.RoleSamples(scaler)
	current := deploymentReplicaVector(md)
	hasSurplusPods := hasSurplusDeploymentPod(pods, current)
	retirementInFlight := md.Status.Retirement != nil &&
		md.Status.Retirement.State != workercore.ModelDeploymentRetirementStateAborted &&
		md.Status.Retirement.State != workercore.ModelDeploymentRetirementStateCompleted
	hasActiveRetirement := retirementInFlight || hasSurplusPods
	interval := r.interval(scaler)
	r.resetRebalanceForGenerationChange(scaler, md)

	if scaler.Status.PendingCohort != nil {
		cohort := scaler.Status.PendingCohort
		switch {
		case cohort.Term != scalerTerm(scaler):
			scaler.Status.PendingCohort = nil
		case addedOrdinalsAdmitted(cohort, cohorts):
			scaler.Status.PendingCohort = nil
		case active && cohort.StartedAt != nil && !now.Before(cohort.StartedAt.Add(r.pendingTimeout(scaler))):
			owned, err := r.ensureOwnedBinding(ctx, before, scaler, md, now)
			if err != nil {
				return ctrlreconcile.Result{}, err
			}
			if !owned {
				return ctrlreconcile.Result{RequeueAfter: interval}, nil
			}
			target := cloneReplicaVector(cohort.BaselineVector)
			r.recordRebalanceExhaustion(scaler, cohort, cohorts)
			decisionID, err := r.writeVector(ctx, scaler, md, target, current, now)
			if err != nil {
				return ctrlreconcile.Result{}, err
			}
			scaler.Status.LastCommit = &workercore.ModelDeploymentScalerCommitStatus{
				DecisionID: decisionID, Term: scalerTerm(scaler), Vector: cloneReplicaVector(target),
				BaselineVector: cloneReplicaVector(current), Timestamp: timePointer(now), TargetGeneration: md.Generation + 1,
			}
			scaler.Status.PendingCohort = nil
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked, meta.ConditionTrue,
				workercore.ModelDeploymentScalerReasonResourceExhausted, "the scale-up cohort stayed unadmitted past its deadline and was retracted", now)
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeMetricsReady, meta.ConditionFalse,
				workercore.ModelDeploymentScalerReasonResourceExhausted, "the scale-up cohort was retracted", now)
			scaler.Status.ObservedGeneration = scaler.Generation
			if r.Recorder != nil {
				r.Recorder.Eventf(scaler, core.EventTypeWarning, "CohortRetracted", "retracted unadmitted scale-up cohort to %v", target)
			}
			return ctrlreconcile.Result{RequeueAfter: interval}, errors.Join(
				modelDeploymentScalerPriorAPIError(resolutionErr), r.updateStatus(ctx, before, scaler),
			)
		}
	}

	if retirement := md.Status.Retirement; active &&
		shouldRestoreScaleDownAbort(retirement, scaler.Status.LastScaleDown, scaler.Status.LastCommit, current) {
		owned, err := r.ensureOwnedBinding(ctx, before, scaler, md, now)
		if err != nil {
			return ctrlreconcile.Result{}, err
		}
		if !owned {
			return ctrlreconcile.Result{RequeueAfter: interval}, nil
		}
		target := cloneReplicaVector(scaler.Status.LastScaleDown.PreScaleDownVector)
		if scaler.Status.Rebalance != nil && scaler.Status.Rebalance.DonorRole == retirement.RoleName {
			scaler.Status.Rebalance = nil
		}
		decisionID, err := r.writeVector(ctx, scaler, md, target, current, now)
		if err != nil {
			return ctrlreconcile.Result{}, err
		}
		cooldown := time.Duration(scaler.Spec.Behavior.ScaleDown.CooldownAfterAbortSeconds) * time.Second
		until := now.Add(cooldown)
		scaler.Status.LastScaleDown.Outcome = workercore.ModelDeploymentScalerScaleDownOutcomeAborted
		scaler.Status.LastScaleDown.AbortedAt = timePointer(now)
		scaler.Status.LastScaleDown.CooldownUntil = timePointer(until)
		scaler.Status.LastCommit = &workercore.ModelDeploymentScalerCommitStatus{
			DecisionID: decisionID, Term: scalerTerm(scaler), Vector: cloneReplicaVector(target),
			BaselineVector: cloneReplicaVector(current), Timestamp: timePointer(now), TargetGeneration: md.Generation + 1,
		}
		r.setCondition(
			scaler, workercore.ModelDeploymentScalerConditionTypeBlocked, meta.ConditionTrue,
			workercore.ModelDeploymentScalerReasonScaleDownAborted,
			"the retirement aborted and the pre-scale-down vector was restored: "+retirement.Reason, now,
		)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeMetricsReady, meta.ConditionTrue,
			"Observed", "metrics were observed", now)
		scaler.Status.ObservedGeneration = scaler.Generation
		if r.Recorder != nil {
			r.Recorder.Eventf(
				scaler, core.EventTypeWarning, "ScaleDownAbortedRestore",
				"restored replica vector after retirement of %s ordinal %d aborted",
				retirement.RoleName, retirement.ReplicaOrdinal,
			)
		}
		return ctrlreconcile.Result{RequeueAfter: interval}, errors.Join(
			modelDeploymentScalerPriorAPIError(resolutionErr), r.updateStatus(ctx, before, scaler),
		)
	}

	if active && !hasActiveRetirement &&
		scaler.Status.LastScaleDown != nil &&
		scaler.Status.LastScaleDown.Outcome == workercore.ModelDeploymentScalerScaleDownOutcomeCommitted &&
		((md.Status.Retirement != nil &&
			(md.Status.Retirement.State == workercore.ModelDeploymentRetirementStateCompleted ||
				(md.Status.Retirement.State == workercore.ModelDeploymentRetirementStateAborted && len(pods) > 0))) ||
			(md.Status.Retirement == nil && len(pods) > 0)) {
		scaler.Status.LastScaleDown = nil
	}

	if resolutionErr != nil {
		return r.stopForCapacityPrior(ctx, before, scaler, resolutionErr, now)
	}

	if reason := targetRolesEligibility(scaler.Spec, md.Spec.Roles); reason != "" {
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked, meta.ConditionTrue,
			workercore.ModelDeploymentScalerReasonBoundsConflict, reason, now)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeMetricsReady, meta.ConditionUnknown,
			workercore.ModelDeploymentScalerReasonBoundsConflict, "planning is held until target roles are eligible", now)
		scaler.Status.ObservedGeneration = scaler.Generation
		if err := r.updateStatus(ctx, before, scaler); err != nil {
			return ctrlreconcile.Result{}, err
		}
		return ctrlreconcile.Result{RequeueAfter: interval}, nil
	}

	if reason := currentVectorConflict(scaler.Spec, md.Spec.Roles, current); reason != "" {
		blockedReason := workercore.ModelDeploymentScalerReasonRatioConflict
		if reason == workercore.ModelDeploymentScalerReasonBoundsConflict {
			blockedReason = workercore.ModelDeploymentScalerReasonBoundsConflict
		}
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked, meta.ConditionTrue,
			blockedReason, "the current replica vector is outside the scaler policy", now)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeMetricsReady, meta.ConditionUnknown,
			blockedReason, "planning is held until the current vector satisfies the scaler policy", now)
		scaler.Status.ObservedGeneration = scaler.Generation
		if err := r.updateStatus(ctx, before, scaler); err != nil {
			return ctrlreconcile.Result{}, err
		}
		return ctrlreconcile.Result{RequeueAfter: interval}, nil
	}

	var (
		algorithmByRole map[string]algorithm.RoleRecommendation
		predictorByRole map[string]*algorithm.PredictorState
		states          []ModelDeploymentScalerRoleState
		roleStatuses    []workercore.ModelDeploymentScalerRoleStatus
		metricsReady    bool
		metricsReasons  []string
		wholeUnit       map[string]int32
	)
	if resolved != nil && resolved.WholeUnit != nil {
		wholeUnit = resolved.WholeUnit.UnitVector
		kCur, ok := wholeUnitMultiplier(md.Spec.Roles, wholeUnit)
		if !ok {
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked, meta.ConditionTrue,
				workercore.ModelDeploymentScalerReasonRatioConflict,
				"the current replica vector is not an integer multiple of the whole-unit vector", now)
			r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeMetricsReady, meta.ConditionUnknown,
				workercore.ModelDeploymentScalerReasonRatioConflict,
				"planning is held until the current vector aligns to the whole-unit vector", now)
			scaler.Status.ObservedGeneration = scaler.Generation
			if err := r.updateStatus(ctx, before, scaler); err != nil {
				return ctrlreconcile.Result{}, err
			}
			return ctrlreconcile.Result{RequeueAfter: interval}, nil
		}
		var err error
		algorithmByRole, predictorByRole, states, roleStatuses, metricsReady, metricsReasons, err = r.planWholeUnitStates(
			scaler, md, resolved, observations, pendingByRole, kCur, now,
		)
		if err != nil {
			return ctrlreconcile.Result{}, err
		}
	} else {
		algorithmByRole = make(map[string]algorithm.RoleRecommendation, len(md.Spec.Roles))
		predictorByRole = make(map[string]*algorithm.PredictorState, len(md.Spec.Roles))
		states = make([]ModelDeploymentScalerRoleState, 0, len(md.Spec.Roles))
		roleStatuses = make([]workercore.ModelDeploymentScalerRoleStatus, 0, len(md.Spec.Roles))
		metricsReady = true
		metricsReasons = make([]string, 0)
		for i := range md.Spec.Roles {
			role := &md.Spec.Roles[i]
			observation, found := observations[role.Name]
			if !found {
				observation = algorithm.RoleObservation{Role: role.Name, CurrentReplicas: role.Replicas, Kind: modelDeploymentScalerRoleKind(role.Kind)}
			}
			observation.Role = role.Name
			observation.CurrentReplicas = role.Replicas
			obsSample := samples[role.Name]
			engineAlgorithm, err := modelDeploymentScalerAlgorithmForRole(resolved.Algorithms[role.Name], role.Name)
			if err != nil {
				return ctrlreconcile.Result{}, err
			}
			history := r.sampler.StabilizationHistory(scaler, role.Name)
			prior := r.sampler.PredictorState(scaler, role.Name)
			recommendation, next := engineAlgorithm.Recommend(observation, prior)
			algorithmByRole[role.Name] = recommendation
			predictorByRole[role.Name] = next
			if recommendation.Reason == algorithm.ReasonWaitingForMetrics ||
				(recommendation.Reason != algorithm.ReasonRatioCoupled && !observation.CompleteCoverage) {
				metricsReady = false
				metricsReasons = append(metricsReasons, role.Name+": "+strings.Join(obsSample.Reasons, "; "))
			}
			roleStatuses = append(roleStatuses, roleStatusFromObservation(
				scaler, role, observation, obsSample.Reasons, recommendation, history, next != nil,
			))
			states = append(states, ModelDeploymentScalerRoleState{
				Name: role.Name, Kind: observation.Kind, CurrentReplicas: role.Replicas,
				RawRecommendedReplicas:       recommendation.DesiredReplicas,
				HoldScaleDown:                recommendation.HoldScaleDown,
				CompleteCoverage:             observation.CompleteCoverage || recommendation.Reason == algorithm.ReasonRatioCoupled,
				HasPendingUnadmittedWorkload: pendingByRole[role.Name] || scaler.Status.PendingCohort != nil,
				LastScaleUpTime:              scalerRoleLastScaleUpTime(scaler.Status.Roles, role.Name),
				StabilizationHistory:         history,
			})
		}
	}

	inAbortCooldown := scaler.Status.LastScaleDown != nil &&
		scaler.Status.LastScaleDown.CooldownUntil != nil &&
		now.Before(scaler.Status.LastScaleDown.CooldownUntil.Time)
	constraint := SolveModelDeploymentScalerConstraints(ModelDeploymentScalerConstraintInput{
		Spec:                scaler.Spec,
		Roles:               states,
		WholeUnit:           wholeUnit,
		HasActiveRetirement: hasActiveRetirement,
		InAbortCooldown:     inAbortCooldown,
		Now:                 now,
	})
	for i := range roleStatuses {
		status := &roleStatuses[i]
		status.ConstrainedReplicas = constraint.Replicas[status.Name]
		if reason := constraint.Reasons[status.Name]; reason != "" {
			status.Reason = reason
		}
		if status.LastScaleUpTime == nil {
			status.LastScaleUpTime = scalerRoleLastScaleUp(scaler.Status.Roles, status.Name)
		}
		status.StabilizationHistory = constraint.StabilizationHistory[status.Name]
	}
	// The windows of the roles are kept in memory, so that they grow on every tick whether
	// or not the status of the scaler is written.
	r.sampler.RecordStabilizationHistory(scaler, constraint.StabilizationHistory)
	r.sampler.RecordPredictorStates(scaler, predictorByRole)
	vector := cloneReplicaVector(constraint.Replicas)
	var rebalanceReason string
	vector, rebalanceReason, err = r.planRebalance(ctx, scaler, md, current, vector, algorithmByRole, now, active)
	if err != nil {
		return ctrlreconcile.Result{}, err
	}
	scaler.Status.Roles = roleStatuses
	scaler.Status.Predictors = scalerPredictorStatuses(predictorByRole)
	// THE SCALER SAYS WHICH QUANTILE IT IS HELD TO WHERE THE STATUS IS WRITTEN, because
	// the quantile belongs to the specification and every path that writes the status
	// has to say it, not only the paths that planned.
	if metricsReady {
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeMetricsReady, meta.ConditionTrue,
			"Observed", "all required role signals are available", now)
	} else {
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeMetricsReady, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonWaitingForMetrics, strings.Join(metricsReasons, "; "), now)
	}
	blockingReason := constraint.BlockingReason
	if rebalanceReason != "" {
		blockingReason = rebalanceReason
	}
	if blockingReason != "" {
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBlocked, meta.ConditionTrue,
			blockingReason, "the recommendation is held by bounds, ratios, pacing, or quota-pool rebalance", now)
	} else {
		r.setCondition(
			scaler, workercore.ModelDeploymentScalerConditionTypeBlocked, meta.ConditionFalse,
			"Ready", "no planning constraint is blocking a replica change", now,
		)
	}

	if active && !equalReplicaVectors(current, vector) {
		if err := r.recheckCapacityPriors(ctx, scaler, md, resolved); err != nil {
			r.applyCapacityPriorResolution(scaler, nil, err, now)
			return r.stopForCapacityPrior(ctx, before, scaler, err, now)
		}
		owned, err := r.ensureOwnedBinding(ctx, before, scaler, md, now)
		if err != nil {
			return ctrlreconcile.Result{}, err
		}
		if !owned {
			return ctrlreconcile.Result{RequeueAfter: interval}, nil
		}
		decisionID, err := r.writeVector(ctx, scaler, md, vector, current, now)
		if err != nil {
			return ctrlreconcile.Result{}, err
		}
		scaler.Status.LastCommit = &workercore.ModelDeploymentScalerCommitStatus{
			DecisionID: decisionID, Term: scalerTerm(scaler), Vector: cloneReplicaVector(vector),
			BaselineVector: cloneReplicaVector(current), Timestamp: timePointer(now), TargetGeneration: md.Generation + 1,
		}
		anyScaleUp, anyScaleDown := false, false
		for role, count := range vector {
			if count > current[role] {
				anyScaleUp = true
				for i := range scaler.Status.Roles {
					if scaler.Status.Roles[i].Name == role {
						scaler.Status.Roles[i].LastScaleUpTime = timePointer(now)
					}
				}
			}
			if count < current[role] {
				anyScaleDown = true
			}
		}
		if anyScaleUp {
			if scaler.Status.LastScaleDown != nil &&
				scaler.Status.LastScaleDown.Outcome == workercore.ModelDeploymentScalerScaleDownOutcomeCommitted {
				scaler.Status.LastScaleDown = nil
			}
			started := meta.NewTime(now)
			scaler.Status.PendingCohort = &workercore.ModelDeploymentScalerPendingCohortStatus{
				DecisionID: decisionID, Term: scalerTerm(scaler), BaselineVector: cloneReplicaVector(current),
				TargetVector: cloneReplicaVector(vector), StartedAt: &started,
			}
		}
		if anyScaleDown {
			role, ordinal := firstScaleDown(current, vector)
			scaler.Status.LastScaleDown = &workercore.ModelDeploymentScalerScaleDownStatus{
				Role: role, Ordinal: ordinal, PreScaleDownVector: cloneReplicaVector(current), Outcome: workercore.ModelDeploymentScalerScaleDownOutcomeCommitted,
			}
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(scaler, core.EventTypeNormal, "ReplicaVectorCommitted", "committed replica vector %v", vector)
		}
	}
	scaler.Status.ObservedGeneration = scaler.Generation
	if err := r.updateStatus(ctx, before, scaler); err != nil {
		return ctrlreconcile.Result{}, err
	}
	if scaler.Status.PendingCohort != nil && scaler.Status.PendingCohort.StartedAt != nil {
		remaining := scaler.Status.PendingCohort.StartedAt.Add(r.pendingTimeout(scaler)).Sub(now)
		if remaining < modelDeploymentScalerMinimumPendingRequeue {
			remaining = modelDeploymentScalerMinimumPendingRequeue
		}
		if remaining < interval {
			interval = remaining
		}
	}
	return ctrlreconcile.Result{RequeueAfter: interval}, nil
}

func (r *ModelDeploymentScalerReconciler) ensureOwnedBinding(
	ctx context.Context,
	before, scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
	now time.Time,
) (bool, error) {
	binding, ok, err := readScalerBinding(md)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "the target's scaler binding annotation cannot be read", "target", md.Name)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonInvalidBinding, "the target's scaler binding annotation cannot be read", now)
		r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeActive, meta.ConditionFalse,
			workercore.ModelDeploymentScalerReasonInvalidBinding, "the target's scaler binding annotation cannot be read", now)
		scaler.Status.ObservedGeneration = scaler.Generation
		if err := r.updateStatus(ctx, before, scaler); err != nil {
			return false, err
		}
		return false, nil
	}
	if ok && binding.ScalerUID == string(scaler.UID) && binding.Term == scalerTerm(scaler) {
		return true, nil
	}
	r.setCondition(scaler, workercore.ModelDeploymentScalerConditionTypeBound, meta.ConditionFalse,
		workercore.ModelDeploymentScalerReasonAlreadyBound, "the target binding changed while the recommendation was computed", now)
	scaler.Status.ObservedGeneration = scaler.Generation
	return false, r.updateStatus(ctx, before, scaler)
}

func (r *ModelDeploymentScalerReconciler) executionState(
	ctx context.Context, md *workercore.ModelDeployment,
) ([]core.Pod, map[string]bool, map[string]map[int32]bool, error) {
	pods, err := readModelDeploymentPods(ctx, md, r.Client)
	if err != nil {
		return nil, nil, nil, err
	}
	pendingByRole := make(map[string]bool)
	cohorts := make(map[string]map[int32]bool)
	if len(pods) == 0 {
		return pods, pendingByRole, cohorts, nil
	}
	podByName := make(map[string]*core.Pod, len(pods))
	for i := range pods {
		podByName[pods[i].Name] = &pods[i]
	}
	workloadList := new(kueue.WorkloadList)
	if err := r.Client.List(
		ctx, workloadList, ctrlcli.InNamespace(md.Namespace),
		ctrlclix.WithoutQuorum, ctrlcli.UnsafeDisableDeepCopy,
	); err != nil {
		return nil, nil, nil, err
	}
	for i := range workloadList.Items {
		wl := &workloadList.Items[i]
		var pod *core.Pod
		for _, owner := range wl.OwnerReferences {
			if owner.Kind == "Pod" {
				pod = podByName[owner.Name]
				if pod != nil && owner.UID != "" && pod.UID != owner.UID {
					pod = nil
				}
				if pod != nil {
					break
				}
			}
		}
		if pod == nil {
			continue
		}
		role := modelDeploymentPodRole(pod)
		ordinal, ok := modelDeploymentPodOrdinal(pod)
		if !kueueworkload.HasQuotaReservation(wl) || !kueueworkload.IsAdmitted(wl) {
			pendingByRole[role] = true
		}
		if ok && kueueworkload.HasQuotaReservation(wl) && kueueworkload.IsAdmitted(wl) {
			if cohorts[role] == nil {
				cohorts[role] = make(map[int32]bool)
			}
			cohorts[role][int32(ordinal)] = true
		}
	}
	return pods, pendingByRole, cohorts, nil
}

func addedOrdinalsAdmitted(
	cohort *workercore.ModelDeploymentScalerPendingCohortStatus,
	admitted map[string]map[int32]bool,
) bool {
	if cohort == nil || len(cohort.TargetVector) == 0 {
		return false
	}
	added := false
	for role, target := range cohort.TargetVector {
		baseline := cohort.BaselineVector[role]
		for ordinal := baseline; ordinal < target; ordinal++ {
			added = true
			if !admitted[role][ordinal] {
				return false
			}
		}
	}
	return added
}

func (r *ModelDeploymentScalerReconciler) pendingTimeout(scaler *workercore.ModelDeploymentScaler) time.Duration {
	seconds := scaler.Spec.Sampling.PendingTimeoutSeconds
	if seconds <= 0 {
		return modelDeploymentScalerPendingTimeoutDefault
	}
	return time.Duration(seconds) * time.Second
}

func modelDeploymentScalerAlgorithmForRole(
	spec workercore.ModelDeploymentScalerAlgorithmSpec,
	role string,
) (algorithm.Algorithm, error) {
	switch spec.Type {
	case workercore.ModelDeploymentScalerAlgorithmTypeProportional:
		if spec.Proportional == nil {
			return nil, fmt.Errorf("missing parameters for Proportional algorithm")
		}
		var target float64
		hasTarget := false
		for _, value := range spec.Proportional.RoleTargets {
			if value.Name == role {
				target = value.Target.AsApproximateFloat64()
				hasTarget = true
				break
			}
		}
		if !hasTarget {
			return ratioCoupledAlgorithm{}, nil
		}
		tolerance := 0.0
		if spec.Proportional.Tolerance != nil {
			tolerance = spec.Proportional.Tolerance.AsApproximateFloat64()
		}
		return algorithm.Proportional{Metric: algorithm.ProportionalMetric(spec.Proportional.Metric), TargetValue: target, Tolerance: tolerance}, nil
	case workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput:
		if spec.TokenThroughput == nil {
			return nil, fmt.Errorf("missing parameters for TokenThroughput algorithm")
		}
		return tokenThroughputAlgorithm(*spec.TokenThroughput), nil
	case workercore.ModelDeploymentScalerAlgorithmTypePredictive:
		if spec.Predictive == nil {
			return nil, fmt.Errorf("missing parameters for Predictive algorithm")
		}
		parameters := spec.Predictive
		return algorithm.Predictive{
			TokenThroughput:                tokenThroughputAlgorithm(parameters.TokenThroughput),
			HorizonSeconds:                 float64(parameters.HorizonSeconds),
			PrefillTokenCapacityPerReplica: modelDeploymentScalerQuantityValue(parameters.PrefillTokenCapacityPerReplica),
			DecodeTokenCapacityPerReplica:  modelDeploymentScalerQuantityValue(parameters.DecodeTokenCapacityPerReplica),
			ServerTokenCapacityPerReplica:  modelDeploymentScalerQuantityValue(parameters.ServerTokenCapacityPerReplica),
			QLevel:                         modelDeploymentScalerQuantityValue(parameters.QLevel),
			QTrend:                         modelDeploymentScalerQuantityValue(parameters.QTrend),
			R:                              modelDeploymentScalerQuantityValue(parameters.MeasurementNoise),
			MinPoints:                      parameters.MinPoints,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported scaler algorithm %q", spec.Type)
	}
}

func tokenThroughputAlgorithm(spec workercore.ModelDeploymentScalerTokenThroughputSpec) algorithm.TokenThroughput {
	threshold := modelDeploymentScalerQuantityValue(spec.PanicThreshold)
	return algorithm.TokenThroughput{
		PrefillTargetTokensPerSecond: modelDeploymentScalerQuantityValue(spec.PrefillTargetTokensPerSecond),
		DecodeTargetTokensPerSecond:  modelDeploymentScalerQuantityValue(spec.DecodeTargetTokensPerSecond),
		ServerTargetTokensPerSecond:  modelDeploymentScalerQuantityValue(spec.ServerTargetTokensPerSecond),
		StableWindowSeconds:          float64(spec.StableWindowSeconds), PanicWindowSeconds: float64(spec.PanicWindowSeconds),
		PanicThreshold: threshold, TTFTTargetMilliseconds: modelDeploymentScalerQuantityValue(spec.TTFTTargetMilliseconds),
		ITLTargetMilliseconds: modelDeploymentScalerQuantityValue(spec.ITLTargetMilliseconds),
	}
}

type ratioCoupledAlgorithm struct{}

func (ratioCoupledAlgorithm) Name() string { return algorithm.ReasonRatioCoupled }

func (ratioCoupledAlgorithm) Recommend(
	obs algorithm.RoleObservation, prior *algorithm.PredictorState,
) (algorithm.RoleRecommendation, *algorithm.PredictorState) {
	return algorithm.RoleRecommendation{
		DesiredReplicas: obs.CurrentReplicas,
		Reason:          algorithm.ReasonRatioCoupled,
	}, prior
}

func deploymentReplicaVector(md *workercore.ModelDeployment) map[string]int32 {
	vector := make(map[string]int32, len(md.Spec.Roles))
	for _, role := range md.Spec.Roles {
		vector[role.Name] = role.Replicas
	}
	return vector
}

func targetRolesEligibility(
	spec workercore.ModelDeploymentScalerSpec,
	roles []workercore.ModelDeploymentRole,
) string {
	declared := make(map[string]workercore.ModelDeploymentRole, len(roles))
	for _, role := range roles {
		declared[role.Name] = role
	}
	for _, bound := range spec.Roles {
		role, ok := declared[bound.Name]
		if !ok {
			return fmt.Sprintf("the target role %q does not exist", bound.Name)
		}
		if len(role.Command) > 0 {
			return fmt.Sprintf("the target role %q runs a custom command and cannot be autoscaled", bound.Name)
		}
		if role.ElasticEP != nil {
			return fmt.Sprintf("the target role %q runs elastic-EP and cannot be autoscaled", bound.Name)
		}
	}
	for _, ratio := range spec.Ratios {
		for _, name := range []string{ratio.NumeratorRole, ratio.DenominatorRole} {
			if name == "" {
				continue
			}
			role, ok := declared[name]
			if !ok {
				return fmt.Sprintf("the ratio role %q does not exist", name)
			}
			if len(role.Command) > 0 {
				return fmt.Sprintf("the ratio role %q runs a custom command and cannot be autoscaled", name)
			}
			if role.ElasticEP != nil {
				return fmt.Sprintf("the ratio role %q runs elastic-EP and cannot be autoscaled", name)
			}
		}
	}
	return ""
}

func currentVectorConflict(
	spec workercore.ModelDeploymentScalerSpec,
	roles []workercore.ModelDeploymentRole,
	vector map[string]int32,
) string {
	if len(spec.Roles) != len(roles) {
		return workercore.ModelDeploymentScalerReasonBoundsConflict
	}
	for _, bound := range spec.Roles {
		current, ok := vector[bound.Name]
		minimum := bound.MinReplicas
		if minimum <= 0 {
			minimum = 1
		}
		if !ok || current < minimum || current > bound.MaxReplicas {
			return workercore.ModelDeploymentScalerReasonBoundsConflict
		}
	}
	if !ratiosSatisfied(vector, spec.Ratios) {
		return workercore.ModelDeploymentScalerReasonRatioConflict
	}
	return ""
}

func firstScaleDown(before, after map[string]int32) (string, int32) {
	roles := make([]string, 0, len(after))
	for role := range after {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if after[role] < before[role] {
			return role, after[role]
		}
	}
	return "", 0
}

func timePointer(now time.Time) *meta.Time {
	value := meta.NewTime(now)
	return &value
}

func hasSurplusDeploymentPod(pods []core.Pod, current map[string]int32) bool {
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp == nil && (pod.Status.Phase == core.PodFailed || pod.Status.Phase == core.PodSucceeded) {
			continue
		}
		role := modelDeploymentPodRole(pod)
		if role == "" {
			continue
		}
		target, ok := current[role]
		if !ok {
			continue
		}
		ordinal, ok := modelDeploymentPodOrdinal(pod)
		if ok && int32(ordinal) >= target {
			return true
		}
	}
	return false
}

func shouldRestoreScaleDownAbort(
	retirement *workercore.ModelDeploymentRetirementStatus,
	lastScaleDown *workercore.ModelDeploymentScalerScaleDownStatus,
	lastCommit *workercore.ModelDeploymentScalerCommitStatus,
	current map[string]int32,
) bool {
	if retirement == nil || retirement.State != workercore.ModelDeploymentRetirementStateAborted {
		return false
	}
	if lastScaleDown == nil || lastScaleDown.Outcome != workercore.ModelDeploymentScalerScaleDownOutcomeCommitted {
		return false
	}
	if len(lastScaleDown.PreScaleDownVector) == 0 {
		return false
	}
	// Ignore a historical Aborted status recorded prior to the current scale-down commit.
	if lastCommit != nil && lastCommit.Timestamp != nil {
		retTime := retirement.PhaseStartedAt
		if retTime.IsZero() {
			retTime = retirement.StartedAt
		}
		if !retTime.IsZero() && retTime.Before(lastCommit.Timestamp) {
			return false
		}
	}
	if preCount, ok := lastScaleDown.PreScaleDownVector[retirement.RoleName]; ok &&
		retirement.ReplicaOrdinal >= current[retirement.RoleName] &&
		retirement.ReplicaOrdinal < preCount {
		return true
	}
	return lastScaleDown.Role == retirement.RoleName && lastScaleDown.Ordinal == retirement.ReplicaOrdinal
}

func wholeUnitMultiplier(roles []workercore.ModelDeploymentRole, unitVector map[string]int32) (int32, bool) {
	if len(unitVector) == 0 || len(unitVector) != len(roles) {
		return 0, false
	}
	var kCur int32
	for i := range roles {
		role := roles[i]
		unit, ok := unitVector[role.Name]
		if !ok || unit <= 0 || role.Replicas < unit || role.Replicas%unit != 0 {
			return 0, false
		}
		k := role.Replicas / unit
		if k < 1 {
			return 0, false
		}
		if i == 0 {
			kCur = k
		} else if k != kCur {
			return 0, false
		}
	}
	return kCur, kCur >= 1
}

func combineWholeUnitThroughputState(p, d algorithm.SignalState) algorithm.SignalState {
	if p == "" || p == algorithm.SignalMissing || d == "" || d == algorithm.SignalMissing {
		return algorithm.SignalMissing
	}
	if p == algorithm.SignalStale || d == algorithm.SignalStale {
		return algorithm.SignalStale
	}
	if p == algorithm.SignalFresh || d == algorithm.SignalFresh {
		return algorithm.SignalFresh
	}
	return algorithm.SignalIdle
}

func combineWholeUnitLatencyState(p, d algorithm.SignalState, pHasTTFT, dHasITL bool) algorithm.SignalState {
	if pHasTTFT || dHasITL {
		return algorithm.SignalFresh
	}
	if p == "" || p == algorithm.SignalMissing || d == "" || d == algorithm.SignalMissing {
		return algorithm.SignalMissing
	}
	if p == algorithm.SignalStale || d == algorithm.SignalStale {
		return algorithm.SignalStale
	}
	return algorithm.SignalIdle
}

func roleStatusFromObservation(
	scaler *workercore.ModelDeploymentScaler,
	role *workercore.ModelDeploymentRole,
	observation algorithm.RoleObservation,
	obsReasons []string,
	recommendation algorithm.RoleRecommendation,
	history []workercore.ModelDeploymentScalerStabilizationSample,
	hasPredictor bool,
) workercore.ModelDeploymentScalerRoleStatus {
	readyCount := int32(math.Max(0, float64(observation.ReadyReplicas)))
	status := workercore.ModelDeploymentScalerRoleStatus{
		Name: role.Name, CurrentReplicas: role.Replicas, ReadyReplicas: readyCount,
		SignalState:               scalerObservationState(observation),
		PromptTokensPerSecond:     quantityPointerFloat(observation.PromptTokensPerSecond),
		GenerationTokensPerSecond: quantityPointerFloat(observation.GenerationTokensPerSecond),
		RunningRequests:           int32(math.Max(0, math.Round(observation.RunningRequests))),
		WaitingRequests:           int32(math.Max(0, math.Round(observation.WaitingRequests))),
		KVCacheUsage:              quantityPointerFloat(observation.KVCacheUsage),
		TTFTMilliseconds:          modelDeploymentScalerLatencyQuantity(observation.TTFTMilliseconds, observation.TTFTUnmeasured),
		ITLMilliseconds:           modelDeploymentScalerLatencyQuantity(observation.ITLMilliseconds, observation.ITLUnmeasured),
		RawRecommendedReplicas:    recommendation.DesiredReplicas,
		Reason:                    recommendation.Reason,
	}
	if recommendation.Reason == algorithm.ReasonWaitingForMetrics && len(obsReasons) != 0 {
		status.Reason += ": " + strings.Join(obsReasons, "; ")
	}
	if hasPredictor {
		status.StabilizationHistory = history
		status.LastScaleUpTime = scalerRoleLastScaleUp(scaler.Status.Roles, role.Name)
	}
	return status
}

func wholeUnitObservedAt(prefillObs, decodeObs algorithm.RoleObservation, now time.Time) time.Time {
	unitObservedAt := now
	if !prefillObs.ObservedAt.IsZero() && (decodeObs.ObservedAt.IsZero() || !decodeObs.ObservedAt.After(prefillObs.ObservedAt)) {
		unitObservedAt = prefillObs.ObservedAt
	} else if !decodeObs.ObservedAt.IsZero() {
		unitObservedAt = decodeObs.ObservedAt
	}
	return unitObservedAt
}

func buildWholeUnitObservation(
	md *workercore.ModelDeployment,
	uPrior *ModelDeploymentScalerWholeUnitPrior,
	observations map[string]algorithm.RoleObservation,
	kCur int32,
	now time.Time,
) (map[string]algorithm.RoleObservation, algorithm.RoleObservation, bool) {
	roleObs := make(map[string]algorithm.RoleObservation, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		obs, found := observations[role.Name]
		if !found {
			obs = algorithm.RoleObservation{
				Role:            role.Name,
				CurrentReplicas: role.Replicas,
				Kind:            modelDeploymentScalerRoleKind(role.Kind),
			}
		}
		obs.Role = role.Name
		obs.CurrentReplicas = role.Replicas
		roleObs[role.Name] = obs
	}

	prefillObs := roleObs[uPrior.PrefillRole]
	decodeObs := roleObs[uPrior.DecodeRole]
	unitReady := prefillObs.ReadyReplicas / uPrior.UnitVector[uPrior.PrefillRole]
	if dReady := decodeObs.ReadyReplicas / uPrior.UnitVector[uPrior.DecodeRole]; dReady < unitReady {
		unitReady = dReady
	}
	encodeReady := true
	if uPrior.EncodeRole != "" {
		encodeObs := roleObs[uPrior.EncodeRole]
		if eReady := encodeObs.ReadyReplicas / uPrior.UnitVector[uPrior.EncodeRole]; eReady < unitReady {
			unitReady = eReady
		}
		encodeReady = encodeObs.ReadyReplicas >= encodeObs.CurrentReplicas
	}
	if unitReady < 0 {
		unitReady = 0
	}

	prefillHasTTFT := !prefillObs.TTFTUnmeasured && prefillObs.LatencyState == algorithm.SignalFresh
	decodeHasITL := !decodeObs.ITLUnmeasured && decodeObs.LatencyState == algorithm.SignalFresh
	ttftMs := 0.0
	if prefillHasTTFT {
		ttftMs = prefillObs.TTFTMilliseconds
	}
	itlMs := 0.0
	if decodeHasITL {
		itlMs = decodeObs.ITLMilliseconds
	}

	unitCompleteCoverage := prefillObs.CompleteCoverage && decodeObs.CompleteCoverage && encodeReady
	unitObs := algorithm.RoleObservation{
		Role:                      uPrior.DecodeRole,
		Kind:                      algorithm.RoleKindServer,
		CurrentReplicas:           kCur,
		ReadyReplicas:             unitReady,
		RunningRequests:           prefillObs.RunningRequests + decodeObs.RunningRequests,
		WaitingRequests:           prefillObs.WaitingRequests + decodeObs.WaitingRequests,
		KVCacheUsage:              math.Max(prefillObs.KVCacheUsage, decodeObs.KVCacheUsage),
		PromptTokensPerSecond:     prefillObs.PromptTokensPerSecond,
		GenerationTokensPerSecond: decodeObs.GenerationTokensPerSecond,
		TTFTMilliseconds:          ttftMs,
		TTFTUnmeasured:            !prefillHasTTFT,
		ITLMilliseconds:           itlMs,
		ITLUnmeasured:             !decodeHasITL,
		ThroughputState:           combineWholeUnitThroughputState(prefillObs.ThroughputState, decodeObs.ThroughputState),
		LatencyState:              combineWholeUnitLatencyState(prefillObs.LatencyState, decodeObs.LatencyState, prefillHasTTFT, decodeHasITL),
		CompleteCoverage:          unitCompleteCoverage,
		ObservedAt:                wholeUnitObservedAt(prefillObs, decodeObs, now),
	}
	return roleObs, unitObs, encodeReady
}

func (r *ModelDeploymentScalerReconciler) planWholeUnitStates(
	scaler *workercore.ModelDeploymentScaler,
	md *workercore.ModelDeployment,
	resolved *ModelDeploymentScalerCapacityPriors,
	observations map[string]algorithm.RoleObservation,
	pendingByRole map[string]bool,
	kCur int32,
	now time.Time,
) (
	map[string]algorithm.RoleRecommendation,
	map[string]*algorithm.PredictorState,
	[]ModelDeploymentScalerRoleState,
	[]workercore.ModelDeploymentScalerRoleStatus,
	bool,
	[]string,
	error,
) {
	uPrior := resolved.WholeUnit
	samples := r.sampler.RoleSamples(scaler)
	roleObs, unitObs, encodeReady := buildWholeUnitObservation(md, uPrior, observations, kCur, now)

	engineAlgorithm, err := modelDeploymentScalerAlgorithmForRole(resolved.Algorithms[uPrior.DecodeRole], uPrior.DecodeRole)
	if err != nil {
		return nil, nil, nil, nil, false, nil, err
	}
	switch typed := engineAlgorithm.(type) {
	case algorithm.TokenThroughput:
		typed.ServerTargetTokensPerSecond = uPrior.UnitTargetTokensPerSecond
		engineAlgorithm = typed
	case algorithm.Predictive:
		typed.ServerTargetTokensPerSecond = uPrior.UnitTargetTokensPerSecond
		typed.ServerTokenCapacityPerReplica = uPrior.UnitTargetTokensPerSecond
		engineAlgorithm = typed
	}

	unitRec, unitNext := engineAlgorithm.Recommend(unitObs, r.sampler.PredictorState(scaler, uPrior.DecodeRole))
	metricsReady := true
	metricsReasons := make([]string, 0)
	if unitRec.Reason == algorithm.ReasonWaitingForMetrics || !unitObs.CompleteCoverage {
		metricsReady = false
		for _, roleName := range []string{uPrior.PrefillRole, uPrior.DecodeRole} {
			if s := samples[roleName]; len(s.Reasons) > 0 {
				metricsReasons = append(metricsReasons, roleName+": "+strings.Join(s.Reasons, "; "))
			}
		}
		if !encodeReady && uPrior.EncodeRole != "" {
			metricsReasons = append(metricsReasons, uPrior.EncodeRole+": waiting for encode replicas to become ready")
		}
	}

	maxUnit := int32(1)
	for _, unit := range uPrior.UnitVector {
		if unit > maxUnit {
			maxUnit = unit
		}
	}
	maxSafeUnits := int32(math.MaxInt32) / maxUnit
	rawUnits := unitRec.DesiredReplicas
	if rawUnits > 0 {
		rawUnits = clamp(rawUnits, 1, maxSafeUnits)
	}

	algorithmByRole := make(map[string]algorithm.RoleRecommendation, len(md.Spec.Roles))
	predictorByRole := map[string]*algorithm.PredictorState{uPrior.DecodeRole: unitNext}
	states := make([]ModelDeploymentScalerRoleState, 0, len(md.Spec.Roles))
	roleStatuses := make([]workercore.ModelDeploymentScalerRoleStatus, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		obs := roleObs[role.Name]
		unit := uPrior.UnitVector[role.Name]
		rawReplicas := checkedMulInt32(rawUnits, unit)
		roleReason := unitRec.Reason
		if role.Name == uPrior.EncodeRole && unitRec.Reason != algorithm.ReasonWaitingForMetrics && encodeReady {
			roleReason = algorithm.ReasonRatioCoupled
		}
		rec := algorithm.RoleRecommendation{
			DesiredReplicas: rawReplicas,
			Reason:          roleReason,
			HoldScaleDown:   unitRec.HoldScaleDown,
			InPanic:         unitRec.InPanic,
		}
		algorithmByRole[role.Name] = rec
		history := r.sampler.StabilizationHistory(scaler, role.Name)
		obsReasons := samples[role.Name].Reasons
		if role.Name == uPrior.EncodeRole && roleReason != algorithm.ReasonWaitingForMetrics {
			obsReasons = nil
		}
		roleStatuses = append(roleStatuses, roleStatusFromObservation(
			scaler, role, obs, obsReasons, rec, history, unitNext != nil,
		))
		states = append(states, ModelDeploymentScalerRoleState{
			Name: role.Name, Kind: obs.Kind, CurrentReplicas: role.Replicas,
			RawRecommendedReplicas:       rawReplicas,
			HoldScaleDown:                unitRec.HoldScaleDown,
			CompleteCoverage:             unitObs.CompleteCoverage,
			HasPendingUnadmittedWorkload: pendingByRole[role.Name] || scaler.Status.PendingCohort != nil,
			LastScaleUpTime:              scalerRoleLastScaleUpTime(scaler.Status.Roles, role.Name),
			StabilizationHistory:         history,
		})
	}
	return algorithmByRole, predictorByRole, states, roleStatuses, metricsReady, metricsReasons, nil
}
