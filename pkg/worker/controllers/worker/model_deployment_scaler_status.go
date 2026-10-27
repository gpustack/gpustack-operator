package worker

import (
	"context"
	"math"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

const modelDeploymentScalerPredictorCheckpointStep = int32(16)

// modelDeploymentScalerStatusChanged reports whether the control-plane state of a scaler
// changed between two observations.
//
// IT IGNORES THE SLIDING WINDOWS AND THE RAW METRICS, because those change on every
// sampling tick while the control-plane state does not: a scaler that is steady is not
// observed to be different, and a write per sampling tick puts every scaler of the cluster
// on the write path of the apiserver once per interval.
//
// THE WINDOWS ARE NOT LOST BY THIS, because the sampler keeps them in memory and grows them
// on every tick whether the status is written or not. Predictor state is additionally
// checkpointed every modelDeploymentScalerPredictorCheckpointStep samples during steady
// state so an operator restart bounds Kalman filter state loss without writing per tick.
func modelDeploymentScalerStatusChanged(before, after *workercore.ModelDeploymentScalerStatus) bool {
	if before == nil || after == nil {
		return true
	}
	if before.ObservedGeneration != after.ObservedGeneration {
		return true
	}
	// THE QUANTILE IS COMPARED, because it changes when the configuration
	// changes and the status has to say which quantile the scaler is held to.
	if before.LatencyQuantile != after.LatencyQuantile {
		return true
	}
	if !kubemeta.DeepEqual(before.ResolvedCapacityPriors, after.ResolvedCapacityPriors) {
		return true
	}
	if !kubemeta.DeepEqual(before.Binding, after.Binding) {
		return true
	}
	if !kubemeta.DeepEqual(before.LastCommit, after.LastCommit) {
		return true
	}
	if !kubemeta.DeepEqual(before.PendingCohort, after.PendingCohort) {
		return true
	}
	if !kubemeta.DeepEqual(before.Rebalance, after.Rebalance) {
		return true
	}
	if !kubemeta.DeepEqual(before.LastScaleDown, after.LastScaleDown) {
		return true
	}
	if !equalScalerConditions(before.Conditions, after.Conditions) {
		return true
	}
	if !equalScalerPredictorCheckpoints(before.Predictors, after.Predictors) {
		return true
	}
	return !equalScalerRoleStatuses(before.Roles, after.Roles)
}

func equalScalerPredictorCheckpoints(
	before, after []workercore.ModelDeploymentScalerPredictorStatus,
) bool {
	if len(before) != len(after) {
		if len(before) == 0 {
			for i := range after {
				if after[i].SampleCount >= modelDeploymentScalerPredictorCheckpointStep {
					return false
				}
			}
			return true
		}
		return false
	}
	for i := range before {
		if before[i].Name != after[i].Name {
			return false
		}
		if after[i].SampleCount < before[i].SampleCount {
			return false
		}
		if after[i].SampleCount-before[i].SampleCount >= modelDeploymentScalerPredictorCheckpointStep {
			return false
		}
	}
	return true
}

// equalScalerConditions reports whether two lists of conditions describe the same state.
//
// IT IGNORES THE TRANSITION TIME of a condition, because a condition that did not change
// its status did not transition.
func equalScalerConditions(before, after []gpustack.Condition) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		if before[i].Type != after[i].Type ||
			before[i].Status != after[i].Status ||
			before[i].Reason != after[i].Reason ||
			before[i].Message != after[i].Message ||
			before[i].ObservedGeneration != after[i].ObservedGeneration {
			return false
		}
	}
	return true
}

// equalScalerRoleStatuses reports whether two lists of role statuses describe the same
// control-plane state.
//
// IT IGNORES THE METRICS AND THE STABILIZATION HISTORY of a role, because they are read on
// every tick and change on every tick.
func equalScalerRoleStatuses(before, after []workercore.ModelDeploymentScalerRoleStatus) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		if before[i].Name != after[i].Name ||
			before[i].CurrentReplicas != after[i].CurrentReplicas ||
			before[i].ReadyReplicas != after[i].ReadyReplicas ||
			before[i].SignalState != after[i].SignalState ||
			before[i].RawRecommendedReplicas != after[i].RawRecommendedReplicas ||
			before[i].ConstrainedReplicas != after[i].ConstrainedReplicas ||
			before[i].Reason != after[i].Reason {
			return false
		}
		if !equalScalerTime(before[i].LastScaleUpTime, after[i].LastScaleUpTime) {
			return false
		}
	}
	return true
}

// equalScalerTime reports whether two timestamps are the same moment.
func equalScalerTime(before, after *meta.Time) bool {
	if before == nil || after == nil {
		return before == after
	}
	return before.Time.Equal(after.Time)
}

func modelDeploymentScalerConditionReason(scaler *workercore.ModelDeploymentScaler, conditionType string) string {
	for _, condition := range scaler.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Reason
		}
	}
	return ""
}

func (r *ModelDeploymentScalerReconciler) resetTargetState(scaler *workercore.ModelDeploymentScaler) {
	scaler.Status.ResolvedCapacityPriors = nil
	scaler.Status.Roles = nil
	scaler.Status.LastCommit = nil
	scaler.Status.PendingCohort = nil
	scaler.Status.Rebalance = nil
	scaler.Status.LastScaleDown = nil
	scaler.Status.Predictors = nil
}

func (r *ModelDeploymentScalerReconciler) setCondition(
	scaler *workercore.ModelDeploymentScaler, conditionType string, status meta.ConditionStatus,
	reason, message string, now time.Time,
) {
	for i := range scaler.Status.Conditions {
		condition := &scaler.Status.Conditions[i]
		if condition.Type != conditionType {
			continue
		}
		if condition.Status != status {
			condition.LastTransitionTime = meta.NewTime(now)
		}
		condition.Status = status
		condition.Reason = reason
		condition.Message = message
		condition.ObservedGeneration = scaler.Generation
		return
	}
	scaler.Status.Conditions = append(scaler.Status.Conditions, gpustack.Condition{
		Type: conditionType, Status: status, LastTransitionTime: meta.NewTime(now),
		Reason: reason, Message: message, ObservedGeneration: scaler.Generation,
	})
}

// updateStatus writes the status of a scaler when its control-plane state changed.
//
// IT WRITES NOTHING WHEN THE SCALER IS STEADY, because the sliding windows and the raw
// metrics of a scaler change on every sampling tick and writing them would put every
// scaler of the cluster on the write path of the apiserver once per interval.
//
// IT SAYS WHICH QUANTILE THE SCALER IS HELD TO, and it says the one the specification names,
// because a status that keeps the quantile of another configuration describes a quantile the
// scaler is no longer held to. The quantile is read here, where every status is written, so
// that a scaler whose planning did not run still says which quantile its specification names.
//
// IT LEAVES THE GENERATION UNOBSERVED WHEN THE QUANTILE CANNOT BE READ, because a scaler that
// cannot say which quantile it is held to has not observed the specification it was given.
func (r *ModelDeploymentScalerReconciler) updateStatus(
	ctx context.Context, before, scaler *workercore.ModelDeploymentScaler,
) error {
	quantile, err := ModelDeploymentScalerLatencyQuantile(&scaler.Spec.Algorithm)
	if err != nil {
		scaler.Status.ObservedGeneration = before.Status.ObservedGeneration
	} else {
		scaler.Status.LatencyQuantile = priors.FormatLatencyQuantile(quantile)
	}
	if !modelDeploymentScalerStatusChanged(&before.Status, &scaler.Status) {
		return nil
	}
	return r.Client.Status().Update(ctx, scaler)
}

func modelDeploymentScalerQuantityValue(quantity *resource.Quantity) float64 {
	if quantity == nil {
		return 0
	}
	return quantity.AsApproximateFloat64()
}

func quantityPointerFloat(value float64) *resource.Quantity {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	quantity := *resource.NewMilliQuantity(int64(math.Round(value*1000)), resource.DecimalSI)
	return &quantity
}

// modelDeploymentScalerLatencyQuantity returns the latency a role was observed at, and nothing
// when the latency was not measured.
//
// IT LEAVES THE VALUE OUT, because a latency nobody measured is not a latency of no time and
// a status that carries one says the engine answered in no time.
func modelDeploymentScalerLatencyQuantity(milliseconds float64, unmeasured bool) *resource.Quantity {
	if unmeasured {
		return nil
	}
	return quantityPointerFloat(milliseconds)
}

func scalerObservationState(observation algorithm.RoleObservation) string {
	states := []algorithm.SignalState{observation.GaugeState, observation.ThroughputState, observation.LatencyState}
	missing := false
	fresh := false
	for _, state := range states {
		switch state {
		case algorithm.SignalMissing:
			missing = true
		case algorithm.SignalFresh:
			fresh = true
		case algorithm.SignalStale:
			return string(algorithm.SignalStale)
		}
	}
	if missing || !observation.CompleteCoverage {
		return string(algorithm.SignalMissing)
	}
	if fresh {
		return string(algorithm.SignalFresh)
	}
	return string(algorithm.SignalIdle)
}

func scalerPredictor(
	statuses []workercore.ModelDeploymentScalerPredictorStatus,
	role string,
) *algorithm.PredictorState {
	for i := range statuses {
		status := &statuses[i]
		if status.Name != role {
			continue
		}
		state := &algorithm.PredictorState{SampleCount: status.SampleCount, History: algorithm.NewDemandHistory()}
		if status.Level != nil {
			state.Level = status.Level.AsApproximateFloat64()
		}
		if status.Trend != nil {
			state.Trend = status.Trend.AsApproximateFloat64()
		}
		if status.Covariance != nil {
			state.Covariance = [4]float64{
				modelDeploymentScalerQuantityValue(status.Covariance.P00), modelDeploymentScalerQuantityValue(status.Covariance.P01),
				modelDeploymentScalerQuantityValue(status.Covariance.P10), modelDeploymentScalerQuantityValue(status.Covariance.P11),
			}
		}
		for _, sample := range status.History {
			state.History.Push(algorithm.DemandSample{
				Timestamp: sample.Timestamp.Time, TokensPerSecond: sample.TokensPerSecond.AsApproximateFloat64(),
			})
		}
		return state
	}
	return nil
}

func scalerPredictorStatuses(states map[string]*algorithm.PredictorState) []workercore.ModelDeploymentScalerPredictorStatus {
	roles := make([]string, 0, len(states))
	for role := range states {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	result := make([]workercore.ModelDeploymentScalerPredictorStatus, 0, len(roles))
	for _, role := range roles {
		state := states[role]
		if state == nil {
			continue
		}
		status := workercore.ModelDeploymentScalerPredictorStatus{
			Name: role, Level: quantityPointerFloat(state.Level), Trend: quantityPointerFloat(state.Trend), SampleCount: state.SampleCount,
			Covariance: &workercore.ModelDeploymentScalerKalmanCovariance{
				P00: quantityPointerFloat(state.Covariance[0]), P01: quantityPointerFloat(state.Covariance[1]),
				P10: quantityPointerFloat(state.Covariance[2]), P11: quantityPointerFloat(state.Covariance[3]),
			},
		}
		if state.History != nil {
			for _, sample := range state.History.Data() {
				tps := quantityPointerFloat(sample.TokensPerSecond)
				if tps == nil {
					continue
				}
				status.History = append(status.History, workercore.ModelDeploymentScalerDemandSample{
					Timestamp: meta.NewTime(sample.Timestamp), TokensPerSecond: *tps,
				})
			}
		}
		result = append(result, status)
	}
	return result
}

func scalerStabilizationHistory(
	roles []workercore.ModelDeploymentScalerRoleStatus,
	name string,
) []workercore.ModelDeploymentScalerStabilizationSample {
	for i := range roles {
		if roles[i].Name == name {
			return append([]workercore.ModelDeploymentScalerStabilizationSample(nil), roles[i].StabilizationHistory...)
		}
	}
	return nil
}

func scalerRoleLastScaleUp(roles []workercore.ModelDeploymentScalerRoleStatus, name string) *meta.Time {
	for i := range roles {
		if roles[i].Name == name && roles[i].LastScaleUpTime != nil {
			cloned := *roles[i].LastScaleUpTime
			return &cloned
		}
	}
	return nil
}

func scalerRoleLastScaleUpTime(roles []workercore.ModelDeploymentScalerRoleStatus, name string) time.Time {
	if value := scalerRoleLastScaleUp(roles, name); value != nil {
		return value.Time
	}
	return time.Time{}
}
