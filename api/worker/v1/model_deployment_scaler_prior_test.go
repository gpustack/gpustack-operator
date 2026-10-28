package v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestModelDeploymentScalerPublicStorageConversionRoundTrip(t *testing.T) {
	q := resource.MustParse
	qPtr := func(raw string) *resource.Quantity {
		val := resource.MustParse(raw)
		return &val
	}

	cases := []struct {
		name   string
		public ModelDeploymentScaler
	}{
		{
			name: "absent capacityPrior preserves manual token throughput fields",
			public: ModelDeploymentScaler{
				ObjectMeta: meta.ObjectMeta{
					Name:      "manual-v1-scaler",
					Namespace: "default",
				},
				Spec: workercore.ModelDeploymentScalerSpec{
					TargetRef: workercore.ModelDeploymentScalerTargetReference{Name: "target-md"},
					Roles: []workercore.ModelDeploymentScalerRoleBounds{
						{Name: "server", MinReplicas: 1, MaxReplicas: 4},
					},
					Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
						Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
						TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
							ServerTargetTokensPerSecond: qPtr("1800"),
						},
					},
				},
			},
		},
		{
			name: "capacityPrior with explicit predictive capacities",
			public: ModelDeploymentScaler{
				ObjectMeta: meta.ObjectMeta{
					Name:      "prior-v1-scaler",
					Namespace: "default",
				},
				Spec: workercore.ModelDeploymentScalerSpec{
					TargetRef: workercore.ModelDeploymentScalerTargetReference{Name: "target-md"},
					Roles: []workercore.ModelDeploymentScalerRoleBounds{
						{Name: "server", MinReplicas: 1, MaxReplicas: 4},
					},
					Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
						Type: workercore.ModelDeploymentScalerAlgorithmTypePredictive,
						Predictive: &workercore.ModelDeploymentScalerPredictiveSpec{
							TokenThroughput: workercore.ModelDeploymentScalerTokenThroughputSpec{
								ServerTargetTokensPerSecond: qPtr("1800"),
							},
							ServerTokenCapacityPerReplica: qPtr("2000"),
						},
						CapacityPrior: &workercore.ModelDeploymentScalerCapacityPriorSpec{},
					},
				},
			},
		},
		{
			name: "catalog capacityPrior and resolvedCapacityPriors status survive public storage roundtrip",
			public: ModelDeploymentScaler{
				ObjectMeta: meta.ObjectMeta{
					Name:      "catalog-v1-scaler",
					Namespace: "default",
				},
				Spec: workercore.ModelDeploymentScalerSpec{
					TargetRef: workercore.ModelDeploymentScalerTargetReference{Name: "target-md"},
					Mode:      workercore.ModelDeploymentScalerModeShadow,
					Roles: []workercore.ModelDeploymentScalerRoleBounds{
						{Name: "server", MinReplicas: 1, MaxReplicas: 6},
					},
					Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
						Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
						TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
							TTFTTargetMilliseconds: qPtr("500"),
							ITLTargetMilliseconds:  qPtr("30"),
						},
						CapacityPrior: &workercore.ModelDeploymentScalerCapacityPriorSpec{
							CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{
								Name: "catalog-cm",
								Key:  "priors.json",
							},
							Headroom: qPtr("0.75"),
						},
					},
				},
				Status: workercore.ModelDeploymentScalerStatus{
					ObservedGeneration: 5,
					ResolvedCapacityPriors: []workercore.ModelDeploymentScalerResolvedCapacityPriorStatus{
						{
							Name:                     "server",
							SourceID:                 "inferencex-2026-10",
							SourceRevision:           "rev-2026-10-26",
							CatalogDigest:            "sha256:9999999999999999999999999999999999999999999999999999999999999999",
							ProfileDigest:            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
							CompatibilityFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
							Metric:                   "CacheMissedTotalTokensPerSecond",
							Quantile:                 "p99",
							RequestedQuantile:        "p95",
							LatencyBounds: []workercore.ModelDeploymentScalerResolvedLatencyBoundStatus{
								{
									Metric:   "TTFT",
									Quantile: "p95",
								},
								{
									Metric:       "ITL",
									Quantile:     "p99",
									Conservative: true,
								},
							},
							WorkloadID:          "chat-1k-512",
							WorkloadRevision:    "v1",
							Denominator:         "PerGPU",
							GPUsPerReplica:      8,
							MeasuredReplicas:    1,
							Headroom:            qPtr("0.75"),
							SeedTokensPerSecond: qPtr("2400"),
							Fields: []workercore.ModelDeploymentScalerResolvedCapacityFieldStatus{
								{
									Field:                    "serverTargetTokensPerSecond",
									Source:                   workercore.ModelDeploymentScalerCapacityFieldSourceCatalog,
									EffectiveTokensPerSecond: q("1800"),
								},
							},
						},
					},
					Conditions: []gpustack.Condition{
						{
							Type:   workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady,
							Status: meta.ConditionFalse,
							Reason: workercore.ModelDeploymentScalerReasonCapacityPriorUnresolved,
						},
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var storage workercore.ModelDeploymentScaler
			require.NoError(t, Convert_v1_ModelDeploymentScaler_To_v1alpha1_ModelDeploymentScaler(&tc.public, &storage, nil))

			var roundTrip ModelDeploymentScaler
			require.NoError(t, Convert_v1alpha1_ModelDeploymentScaler_To_v1_ModelDeploymentScaler(&storage, &roundTrip, nil))
			assert.True(t, equality.Semantic.DeepEqual(tc.public, roundTrip))

			raw, err := json.Marshal(roundTrip)
			require.NoError(t, err)
			var fromJSON ModelDeploymentScaler
			require.NoError(t, json.Unmarshal(raw, &fromJSON))
			assert.True(t, equality.Semantic.DeepEqual(tc.public, fromJSON))
		})
	}
}
