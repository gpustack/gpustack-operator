package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TestModelDeploymentScalerWebhookHoldsTheLatencyQuantileToItsRange states
// which latency quantiles a scaler may be held to.
//
// IT MATTERS BECAUSE A QUANTILE OUTSIDE THE UNIT INTERVAL NAMES NO PERCENTILE,
// and a scaler held to a percentile nobody can name cannot be measured against
// any target.
func TestModelDeploymentScalerWebhookHoldsTheLatencyQuantileToItsRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		quantile string
		refused  bool
	}{
		{
			name: "a scaler that names no quantile is admitted",
		},
		{
			name:     "the first quartile is admitted",
			quantile: "0.25",
		},
		{
			name:     "the median is admitted",
			quantile: "0.5",
		},
		{
			name:     "the ninety fifth percentile is admitted",
			quantile: "0.95",
		},
		{
			name:     "the ninety ninth percentile is admitted",
			quantile: "0.99",
		},
		{
			name:     "a custom quantile is admitted",
			quantile: "0.97505",
		},
		{
			name:     "the largest quantile is admitted",
			quantile: "1",
		},
		{
			name:     "a quantile of zero names no percentile and is refused",
			quantile: "0",
			refused:  true,
		},
		{
			name:     "a negative quantile names no percentile and is refused",
			quantile: "-0.5",
			refused:  true,
		},
		{
			name:     "a quantile above one is refused",
			quantile: "1.5",
			refused:  true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			scaler := modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput
				if testCase.quantile != "" {
					s.Spec.Algorithm.TokenThroughput.LatencyQuantile = ptr.To(
						resource.MustParse(testCase.quantile),
					)
				}
			})

			_, err := modelDeploymentScalerWebhook().ValidateCreate(context.Background(), scaler)
			if testCase.refused {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "latencyQuantile",
					"the refusal names the quantile that was refused")
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestModelDeploymentScalerWebhookHoldsThePredictiveLatencyQuantileToItsRange
// states that the nested token throughput of the predictive algorithm is held to
// the same range.
//
// IT MATTERS BECAUSE THE PREDICTIVE ALGORITHM READS ITS LATENCY TARGETS FROM A
// NESTED BLOCK, and a quantile the outer block refuses must not be admitted
// through the inner one.
func TestModelDeploymentScalerWebhookHoldsThePredictiveLatencyQuantileToItsRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		quantile string
		refused  bool
	}{
		{
			name:     "a predictive scaler that names a quantile is admitted",
			quantile: "0.9",
		},
		{
			name:     "a predictive scaler that names a quantile above one is refused",
			quantile: "1.5",
			refused:  true,
		},
		{
			name:     "a predictive scaler that names a quantile of zero is refused",
			quantile: "0",
			refused:  true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			scaler := modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				s.Spec.Algorithm.TokenThroughput = nil
				s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					TokenThroughput: workercore.ModelDeploymentScalerTokenThroughputSpec{
						LatencyQuantile: ptr.To(resource.MustParse(testCase.quantile)),
					},
				}
			})

			_, err := modelDeploymentScalerWebhook().ValidateCreate(context.Background(), scaler)
			if testCase.refused {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "latencyQuantile",
					"the refusal names the quantile that was refused")
				return
			}
			assert.NoError(t, err)
		})
	}
}
