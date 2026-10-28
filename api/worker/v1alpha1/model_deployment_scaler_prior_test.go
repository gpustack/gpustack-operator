package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpustack "gpustack.ai/gpustack/api/v1"
)

func TestModelDeploymentScalerCapacityPriorConstantsSpelling(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "explicit field source",
			got:  string(ModelDeploymentScalerCapacityFieldSourceExplicit),
			want: "Explicit",
		},
		{
			name: "catalog field source",
			got:  string(ModelDeploymentScalerCapacityFieldSourceCatalog),
			want: "Catalog",
		},
		{
			name: "capacity priors ready condition type",
			got:  ModelDeploymentScalerConditionTypeCapacityPriorsReady,
			want: "CapacityPriorsReady",
		},
		{
			name: "capacity prior unresolved reason",
			got:  ModelDeploymentScalerReasonCapacityPriorUnresolved,
			want: "CapacityPriorUnresolved",
		},
		{
			name: "insufficient load coverage reason",
			got:  ModelDeploymentScalerReasonInsufficientLoadCoverage,
			want: "InsufficientLoadCoverage",
		},
		{
			name: "incompatible metric reason",
			got:  ModelDeploymentScalerReasonIncompatibleMetric,
			want: "IncompatibleMetric",
		},
		{
			name: "model id annotation key",
			got:  ModelDeploymentScalerModelIDAnnotationKey,
			want: "worker.gpustack.ai/model-id",
		},
		{
			name: "hardware id annotation key",
			got:  ModelDeploymentScalerHardwareIDAnnotationKey,
			want: "worker.gpustack.ai/hardware-id",
		},
		{
			name: "precision annotation key",
			got:  ModelDeploymentScalerPrecisionAnnotationKey,
			want: "worker.gpustack.ai/precision",
		},
		{
			name: "prefill hardware id annotation key",
			got:  ModelDeploymentScalerPrefillHardwareIDAnnotationKey,
			want: "worker.gpustack.ai/prefill-hardware-id",
		},
		{
			name: "decode hardware id annotation key",
			got:  ModelDeploymentScalerDecodeHardwareIDAnnotationKey,
			want: "worker.gpustack.ai/decode-hardware-id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.got)
		})
	}
}

func TestModelDeploymentScalerCapacityPriorJSONRoundTrip(t *testing.T) {
	q := resource.MustParse
	qPtr := func(raw string) *resource.Quantity {
		val := resource.MustParse(raw)
		return &val
	}

	cases := []struct {
		name               string
		scaler             ModelDeploymentScaler
		wantSubstring      []string
		wantAbsentFragment []string
	}{
		{
			name: "absent capacityPrior preserves manual fields and omits prior",
			scaler: ModelDeploymentScaler{
				ObjectMeta: meta.ObjectMeta{
					Name:      "manual-scaler",
					Namespace: "default",
				},
				Spec: ModelDeploymentScalerSpec{
					TargetRef: ModelDeploymentScalerTargetReference{Name: "target-md"},
					Roles: []ModelDeploymentScalerRoleBounds{
						{Name: "server", MinReplicas: 1, MaxReplicas: 4},
					},
					Algorithm: ModelDeploymentScalerAlgorithmSpec{
						Type: ModelDeploymentScalerAlgorithmTypeTokenThroughput,
						TokenThroughput: &ModelDeploymentScalerTokenThroughputSpec{
							ServerTargetTokensPerSecond: qPtr("1200"),
							TTFTTargetMilliseconds:      qPtr("500"),
							ITLTargetMilliseconds:       qPtr("30"),
						},
					},
				},
			},
			wantSubstring: []string{
				`"serverTargetTokensPerSecond":"1200"`,
			},
			wantAbsentFragment: []string{
				`"capacityPrior"`,
				`"resolvedCapacityPriors"`,
			},
		},
		{
			name: "a present capacity prior carries no policy and keeps manual fields",
			scaler: ModelDeploymentScaler{
				ObjectMeta: meta.ObjectMeta{
					Name:      "prior-scaler",
					Namespace: "default",
				},
				Spec: ModelDeploymentScalerSpec{
					TargetRef: ModelDeploymentScalerTargetReference{Name: "target-md"},
					Roles: []ModelDeploymentScalerRoleBounds{
						{Name: "server", MinReplicas: 1, MaxReplicas: 4},
					},
					Algorithm: ModelDeploymentScalerAlgorithmSpec{
						Type: ModelDeploymentScalerAlgorithmTypeTokenThroughput,
						TokenThroughput: &ModelDeploymentScalerTokenThroughputSpec{
							ServerTargetTokensPerSecond: qPtr("1500"),
						},
						CapacityPrior: &ModelDeploymentScalerCapacityPriorSpec{},
					},
				},
			},
			wantSubstring: []string{
				`"capacityPrior":{}`,
				`"serverTargetTokensPerSecond":"1500"`,
			},
			wantAbsentFragment: []string{
				`"policy"`,
				`"catalogRef"`,
				`"headroom"`,
			},
		},
		{
			name: "a capacity prior and resolvedCapacityPriors status roundtrip all fields",
			scaler: ModelDeploymentScaler{
				ObjectMeta: meta.ObjectMeta{
					Name:      "catalog-prior-scaler",
					Namespace: "default",
				},
				Spec: ModelDeploymentScalerSpec{
					TargetRef: ModelDeploymentScalerTargetReference{Name: "target-md"},
					Mode:      ModelDeploymentScalerModeActive,
					Roles: []ModelDeploymentScalerRoleBounds{
						{Name: "prefill", MinReplicas: 1, MaxReplicas: 8},
						{Name: "decode", MinReplicas: 1, MaxReplicas: 8},
					},
					Algorithm: ModelDeploymentScalerAlgorithmSpec{
						Type: ModelDeploymentScalerAlgorithmTypePredictive,
						Predictive: &ModelDeploymentScalerPredictiveSpec{
							TokenThroughput: ModelDeploymentScalerTokenThroughputSpec{
								PrefillTargetTokensPerSecond: qPtr("2400"),
								TTFTTargetMilliseconds:       qPtr("400"),
								ITLTargetMilliseconds:        qPtr("25"),
							},
							HorizonSeconds: 120,
						},
						CapacityPrior: &ModelDeploymentScalerCapacityPriorSpec{
							CatalogRef: &ModelDeploymentScalerCatalogReference{
								Name: "custom-priors",
								Key:  "priors.json",
							},
							Headroom: qPtr("0.75"),
						},
					},
				},
				Status: ModelDeploymentScalerStatus{
					ObservedGeneration: 3,
					ResolvedCapacityPriors: []ModelDeploymentScalerResolvedCapacityPriorStatus{
						{
							Name:                     "prefill",
							SourceID:                 "inferencex-2026-10",
							SourceRevision:           "rev-2026-10-26",
							CatalogDigest:            "sha256:0000000000000000000000000000000000000000000000000000000000000000",
							ProfileDigest:            "sha256:1111111111111111111111111111111111111111111111111111111111111111",
							CompatibilityFingerprint: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
							Metric:                   "CacheMissedInputTokensPerSecond",
							Quantile:                 "p99",
							RequestedQuantile:        "p95",
							LatencyBounds: []ModelDeploymentScalerResolvedLatencyBoundStatus{
								{
									Metric:       "TTFT",
									Quantile:     "p99",
									Conservative: true,
								},
							},
							WorkloadID:          "chat-1k-512",
							WorkloadRevision:    "v1",
							Denominator:         "PerGPU",
							GPUsPerReplica:      4,
							MeasuredReplicas:    2,
							Headroom:            qPtr("0.75"),
							SeedTokensPerSecond: qPtr("3600"),
							Fields: []ModelDeploymentScalerResolvedCapacityFieldStatus{
								{
									Field:                    "prefillTargetTokensPerSecond",
									Source:                   ModelDeploymentScalerCapacityFieldSourceExplicit,
									EffectiveTokensPerSecond: q("2400"),
								},
								{
									Field:                    "prefillTokenCapacityPerReplica",
									Source:                   ModelDeploymentScalerCapacityFieldSourceCatalog,
									EffectiveTokensPerSecond: q("2700"),
								},
							},
						},
					},
					Conditions: []gpustack.Condition{
						{
							Type:   ModelDeploymentScalerConditionTypeCapacityPriorsReady,
							Status: meta.ConditionTrue,
							Reason: "Ready",
						},
					},
				},
			},
			wantSubstring: []string{
				`"catalogRef":{"name":"custom-priors","key":"priors.json"}`,
				`"requestedQuantile":"p95"`,
				`"headroom":"750m"`,
				`"resolvedCapacityPriors"`,
				`"catalogDigest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"`,
				`"sourceRevision":"rev-2026-10-26"`,
				`"workloadRevision":"v1"`,
				`"gpusPerReplica":4`,
				`"measuredReplicas":2`,
				`"latencyBounds":[{"metric":"TTFT","quantile":"p99","conservative":true}]`,
				`"compatibilityFingerprint":"sha256:2222222222222222222222222222222222222222222222222222222222222222"`,
				`"seedTokensPerSecond":"3600"`,
				`"field":"prefillTokenCapacityPerReplica"`,
				`"source":"Catalog"`,
				`"effectiveTokensPerSecond":"2700"`,
			},
			wantAbsentFragment: []string{
				`"profileID"`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.scaler)
			require.NoError(t, err)

			encoded := string(raw)
			for _, sub := range tc.wantSubstring {
				assert.Contains(t, encoded, sub)
			}
			for _, absent := range tc.wantAbsentFragment {
				assert.NotContains(t, encoded, absent)
			}

			var decoded ModelDeploymentScaler
			require.NoError(t, json.Unmarshal(raw, &decoded))
			assert.True(t, equality.Semantic.DeepEqual(tc.scaler, decoded))
		})
	}
}
