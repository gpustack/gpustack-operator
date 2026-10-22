package worker

import (
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

func modelDeploymentScalerQuantityValue(quantity *resource.Quantity) float64 {
	if quantity == nil {
		return 0
	}
	return quantity.AsApproximateFloat64()
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
