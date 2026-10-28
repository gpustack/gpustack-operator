package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestModelDeploymentScalerLatencyQuantileJSONRoundTrip states how the latency
// quantile of a scaler is written and read.
//
// IT MATTERS BECAUSE THE QUANTILE IS THE CALLER'S CHOICE and it travels through
// the apiserver as a quantity, so what the caller wrote is what the controller
// has to read back.
//
// IT KEEPS THE QUANTITY WHOLE, because a quantity that lost its value on the
// way through the apiserver would hold a scaler to a percentile nobody asked
// for.
func TestModelDeploymentScalerLatencyQuantileJSONRoundTrip(t *testing.T) {
	t.Parallel()

	quantile := func(raw string) *resource.Quantity {
		value := resource.MustParse(raw)
		return &value
	}

	cases := []struct {
		name          string
		scaler        ModelDeploymentScaler
		wantSubstring []string
		wantAbsent    []string
	}{
		{
			name: "a scaler that names no quantile writes no quantile",
			scaler: ModelDeploymentScaler{
				ObjectMeta: meta.ObjectMeta{Name: "default-scaler", Namespace: "default"},
				Spec: ModelDeploymentScalerSpec{
					TargetRef: ModelDeploymentScalerTargetReference{Name: "target-md"},
					Roles: []ModelDeploymentScalerRoleBounds{
						{Name: "server", MinReplicas: 1, MaxReplicas: 4},
					},
					Algorithm: ModelDeploymentScalerAlgorithmSpec{
						Type: ModelDeploymentScalerAlgorithmTypeTokenThroughput,
						TokenThroughput: &ModelDeploymentScalerTokenThroughputSpec{
							ServerTargetTokensPerSecond: quantile("1200"),
						},
					},
				},
			},
			wantSubstring: []string{`"serverTargetTokensPerSecond":"1200"`},
			wantAbsent:    []string{`"latencyQuantile"`},
		},
		{
			name: "a scaler that names a quantile writes it",
			scaler: ModelDeploymentScaler{
				ObjectMeta: meta.ObjectMeta{Name: "quantile-scaler", Namespace: "default"},
				Spec: ModelDeploymentScalerSpec{
					TargetRef: ModelDeploymentScalerTargetReference{Name: "target-md"},
					Roles: []ModelDeploymentScalerRoleBounds{
						{Name: "server", MinReplicas: 1, MaxReplicas: 4},
					},
					Algorithm: ModelDeploymentScalerAlgorithmSpec{
						Type: ModelDeploymentScalerAlgorithmTypeTokenThroughput,
						TokenThroughput: &ModelDeploymentScalerTokenThroughputSpec{
							ServerTargetTokensPerSecond: quantile("1200"),
							LatencyQuantile:             quantile("0.9"),
						},
					},
				},
			},
			wantSubstring: []string{`"latencyQuantile":"900m"`},
			wantAbsent:    []string{`"requestedQuantile"`},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(testCase.scaler)
			require.NoError(t, err)
			for _, fragment := range testCase.wantSubstring {
				assert.Contains(t, string(data), fragment)
			}
			for _, fragment := range testCase.wantAbsent {
				assert.NotContains(t, string(data), fragment)
			}

			var read ModelDeploymentScaler
			require.NoError(t, json.Unmarshal(data, &read))
			assert.True(t, equality.Semantic.DeepEqual(
				testCase.scaler.Spec.Algorithm, read.Spec.Algorithm,
			), "the algorithm survives the round trip")
		})
	}
}

// TestModelDeploymentScalerLatencyQuantileStatusRoundTrip states how the
// quantile of a scaler is written to its status.
//
// IT MATTERS BECAUSE THE STATUS IS WHAT AN OPERATOR READS, and it has to say
// which percentile the scaler is held to and which percentile the evidence
// behind the prior was measured at.
func TestModelDeploymentScalerLatencyQuantileStatusRoundTrip(t *testing.T) {
	t.Parallel()

	status := ModelDeploymentScalerStatus{
		LatencyQuantile: "0.9",
		ResolvedCapacityPriors: []ModelDeploymentScalerResolvedCapacityPriorStatus{
			{
				Name:              "server",
				Quantile:          "0.95",
				RequestedQuantile: "0.9",
			},
		},
	}

	data, err := json.Marshal(status)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"latencyQuantile":"0.9"`)
	assert.Contains(t, string(data), `"quantile":"0.95"`)
	assert.Contains(t, string(data), `"requestedQuantile":"0.9"`)

	var read ModelDeploymentScalerStatus
	require.NoError(t, json.Unmarshal(data, &read))
	assert.Equal(t, status.LatencyQuantile, read.LatencyQuantile)
	require.Len(t, read.ResolvedCapacityPriors, 1)
	assert.Equal(t, "0.95", read.ResolvedCapacityPriors[0].Quantile)
	assert.Equal(t, "0.9", read.ResolvedCapacityPriors[0].RequestedQuantile)
}
