package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// modelDeploymentScaler builds a scaler that plans for one role of one deployment, which every
// case then makes wrong in exactly one way.
func modelDeploymentScaler(mutate ...func(*workercore.ModelDeploymentScaler)) *workercore.ModelDeploymentScaler {
	s := &workercore.ModelDeploymentScaler{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-scaler"},
		Spec: workercore.ModelDeploymentScalerSpec{
			TargetRef: workercore.ModelDeploymentScalerTargetReference{Name: "qwen-72b"},
			Roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "server", MinReplicas: 1, MaxReplicas: 8},
			},
			Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					ServerTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
				},
			},
		},
	}
	for _, m := range mutate {
		m(s)
	}

	return s
}

// modelDeploymentScalerTarget builds the deployment a scaler plans for.
func modelDeploymentScalerTarget(roles ...workercore.ModelDeploymentRole) *workercore.ModelDeployment {
	if len(roles) == 0 {
		roles = []workercore.ModelDeploymentRole{{
			Name: "server", Replicas: 2, InstanceType: "h20-8x",
		}}
	}

	return &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-72b"},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B-Instruct"},
			Engine: workercore.ModelDeploymentEngine{
				Name: "SGLang", Image: "lmsysorg/sglang:latest",
			},
			Roles: roles,
		},
	}
}

// modelDeploymentScalerWebhook builds the handler with the target it reads, or with none.
func modelDeploymentScalerWebhook(targets ...*workercore.ModelDeployment) *ModelDeploymentScalerWebhook {
	builder := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme)
	for _, target := range targets {
		builder = builder.WithObjects(target)
	}

	return &ModelDeploymentScalerWebhook{APIReader: builder.Build()}
}

// TestModelDeploymentScalerWebhookDefaultFillsTheValuesTheAlgorithmNeeds pins the defaults.
//
// A SCALER THAT OMITS A VALUE THE ALGORITHM NEEDS would otherwise plan with a zero, and a zero
// window is a window that never fills.
func TestModelDeploymentScalerWebhookDefaultFillsTheValuesTheAlgorithmNeeds(t *testing.T) {
	s := modelDeploymentScaler()
	s.Spec.Roles[0].MinReplicas = 0

	w := new(ModelDeploymentScalerWebhook)
	require.NoError(t, w.Default(context.Background(), s))

	assert.Equal(t, workercore.ModelDeploymentScalerModeShadow, s.Spec.Mode)
	assert.Equal(t, int32(15), s.Spec.Sampling.IntervalSeconds)
	assert.Equal(t, int32(300), s.Spec.Sampling.PendingTimeoutSeconds)
	assert.Equal(t, workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput, s.Spec.Algorithm.Type)
	require.NotNil(t, s.Spec.Algorithm.TokenThroughput)
	assert.Equal(t, int32(180), s.Spec.Algorithm.TokenThroughput.StableWindowSeconds)
	assert.Equal(t, int32(60), s.Spec.Algorithm.TokenThroughput.PanicWindowSeconds)
	require.NotNil(t, s.Spec.Algorithm.TokenThroughput.PanicThreshold)
	assert.Equal(t, "2", s.Spec.Algorithm.TokenThroughput.PanicThreshold.String())
	require.NotNil(t, s.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds)
	assert.Equal(t, "500", s.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds.String())
	require.NotNil(t, s.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds)
	assert.Equal(t, "50", s.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds.String())
	assert.Equal(t, int32(1), s.Spec.Roles[0].MinReplicas)
	assert.Equal(t, int32(4), s.Spec.Behavior.ScaleUp.MaxStepReplicas)
	assert.Equal(t, int32(100), s.Spec.Behavior.ScaleUp.MaxStepPercent)
	assert.Equal(t, int32(300), s.Spec.Behavior.ScaleDown.StabilizationWindowSeconds)
	assert.Equal(t, int32(300), s.Spec.Behavior.ScaleDown.CooldownAfterAbortSeconds)
	assert.Equal(t, int32(120), s.Spec.Behavior.ScaleDown.DecodePostScaleUpGraceSeconds)
	assert.Equal(t, workercore.ModelDeploymentScalerRebalancePolicyOpposing, s.Spec.Rebalance.Policy)
}

// TestModelDeploymentScalerWebhookDefaultTakesTheAlgorithmFromTheBlock pins that a scaler that
// names only one block runs that block's algorithm.
//
// A SCALER NAMING A PREDICTIVE BLOCK AND NOTHING ELSE would otherwise run the token throughput
// algorithm with the predictive block ignored, which is a different plan from the one that was
// asked for.
func TestModelDeploymentScalerWebhookDefaultTakesTheAlgorithmFromTheBlock(t *testing.T) {
	s := modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
		s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{}
	})

	w := new(ModelDeploymentScalerWebhook)
	require.NoError(t, w.Default(context.Background(), s))

	assert.Equal(t, workercore.ModelDeploymentScalerAlgorithmTypePredictive, s.Spec.Algorithm.Type)
	require.NotNil(t, s.Spec.Algorithm.Predictive)
	assert.Equal(t, int32(180), s.Spec.Algorithm.Predictive.TokenThroughput.StableWindowSeconds)
}

// TestModelDeploymentScalerWebhookDefaultKeepsAValueThatWasWritten pins that a default never
// replaces a value the owner wrote.
func TestModelDeploymentScalerWebhookDefaultKeepsAValueThatWasWritten(t *testing.T) {
	s := modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
		s.Spec.Mode = workercore.ModelDeploymentScalerModeActive
		s.Spec.Sampling.IntervalSeconds = 30
		s.Spec.Roles[0].MinReplicas = 3
		s.Spec.Behavior.ScaleUp.MaxStepReplicas = 9
		s.Spec.Rebalance.Policy = workercore.ModelDeploymentScalerRebalancePolicyDisabled
	})

	w := new(ModelDeploymentScalerWebhook)
	require.NoError(t, w.Default(context.Background(), s))

	assert.Equal(t, workercore.ModelDeploymentScalerModeActive, s.Spec.Mode)
	assert.Equal(t, int32(30), s.Spec.Sampling.IntervalSeconds)
	assert.Equal(t, int32(3), s.Spec.Roles[0].MinReplicas)
	assert.Equal(t, int32(9), s.Spec.Behavior.ScaleUp.MaxStepReplicas)
	assert.Equal(t, workercore.ModelDeploymentScalerRebalancePolicyDisabled, s.Spec.Rebalance.Policy)
}

// TestModelDeploymentScalerWebhookValidatesTheObjectAlone holds the rules that do not need the
// target.
//
// A SCALER MAY BE WRITTEN BEFORE ITS TARGET EXISTS, so a rule that read a target that is not
// there would refuse an object that is merely early.
func TestModelDeploymentScalerWebhookValidatesTheObjectAlone(t *testing.T) {
	cases := []struct {
		name    string
		scaler  *workercore.ModelDeploymentScaler
		refused bool
	}{
		{
			name:   "a scaler that names a target is admitted",
			scaler: modelDeploymentScaler(),
		},
		{
			name: "a scaler that names no target is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.TargetRef.Name = ""
			}),
			refused: true,
		},
		{
			name: "an unknown mode is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Mode = "invalid"
			}),
			refused: true,
		},
		{
			name: "an interval below the floor is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Sampling.IntervalSeconds = 4
			}),
			refused: true,
		},
		{
			name: "an interval above the ceiling is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Sampling.IntervalSeconds = 301
			}),
			refused: true,
		},
		{
			name: "a pending timeout below the floor is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Sampling.PendingTimeoutSeconds = 59
			}),
			refused: true,
		},
		{
			name: "a pending timeout above the ceiling is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Sampling.PendingTimeoutSeconds = 1201
			}),
			refused: true,
		},
		{
			name: "two roles of one name are refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = append(s.Spec.Roles,
					workercore.ModelDeploymentScalerRoleBounds{Name: "server", MinReplicas: 1, MaxReplicas: 4})
			}),
			refused: true,
		},
		{
			name: "a floor above the ceiling is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles[0].MinReplicas = 9
				s.Spec.Roles[0].MaxReplicas = 8
			}),
			refused: true,
		},
		{
			name: "a ceiling above the limit is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles[0].MaxReplicas = 1025
			}),
			refused: true,
		},
		{
			name: "a ratio between two known roles is admitted",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = append(s.Spec.Roles,
					workercore.ModelDeploymentScalerRoleBounds{Name: "prefill", MinReplicas: 1, MaxReplicas: 8})
				s.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
					NumeratorRole: "prefill", DenominatorRole: "server",
					Min: ptr.To(resource.MustParse("1")), Max: ptr.To(resource.MustParse("2")),
				}}
			}),
		},
		{
			name: "a ratio that names an unknown role is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
					NumeratorRole: "prefill", DenominatorRole: "server",
					Min: ptr.To(resource.MustParse("1")), Max: ptr.To(resource.MustParse("2")),
				}}
			}),
			refused: true,
		},
		{
			name: "a ratio whose lower bound is above its upper bound is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
					NumeratorRole: "server", DenominatorRole: "server",
					Min: ptr.To(resource.MustParse("2")), Max: ptr.To(resource.MustParse("1")),
				}}
			}),
			refused: true,
		},
		{
			name: "a ratio that compares a role with itself is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
					NumeratorRole: "server", DenominatorRole: "server",
					Min: ptr.To(resource.MustParse("1")), Max: ptr.To(resource.MustParse("2")),
				}}
			}),
			refused: true,
		},
		{
			name: "a ratio whose target is outside its band is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
					NumeratorRole: "server", DenominatorRole: "server",
					Target: ptr.To(resource.MustParse("3")),
					Min:    ptr.To(resource.MustParse("1")), Max: ptr.To(resource.MustParse("2")),
				}}
			}),
			refused: true,
		},
		{
			name: "a ratio that no vector inside the bounds can satisfy is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
					{Name: "prefill", MinReplicas: 1, MaxReplicas: 1},
					{Name: "server", MinReplicas: 1, MaxReplicas: 1},
				}
				s.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
					NumeratorRole: "prefill", DenominatorRole: "server",
					Min: ptr.To(resource.MustParse("2")), Max: ptr.To(resource.MustParse("2")),
				}}
			}),
			refused: true,
		},
		{
			name: "a ratio a vector inside the bounds satisfies is admitted",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
					{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
					{Name: "server", MinReplicas: 1, MaxReplicas: 4},
				}
				s.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
					NumeratorRole: "prefill", DenominatorRole: "server",
					Min: ptr.To(resource.MustParse("1")), Max: ptr.To(resource.MustParse("1")),
				}}
			}),
		},
		{
			name: "an algorithm that names no block is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypeProportional
			}),
			refused: true,
		},
		{
			name: "a threshold that is not positive is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
					PanicThreshold: ptr.To(resource.MustParse("0")),
				}
			}),
			refused: true,
		},
		{
			name: "a tolerance above one is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypeProportional
				s.Spec.Algorithm.Proportional = &workercore.ModelDeploymentScalerProportionalSpec{
					Metric:    workercore.ModelDeploymentScalerProportionalMetricQueueDepth,
					Tolerance: ptr.To(resource.MustParse("2")),
				}
			}),
			refused: true,
		},
		{
			name: "a rebalance order that names an unknown role is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Rebalance.Priority = []string{"prefill"}
			}),
			refused: true,
		},
		{
			name: "a rebalance order that names a known role is admitted",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Rebalance.Priority = []string{"server"}
			}),
		},
		{
			name: "a step policy that names no unit is refused",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Behavior.ScaleUp.StepPolicy = &workercore.ModelDeploymentScalerStepPolicy{Value: 2}
			}),
			refused: true,
		},
	}

	w := modelDeploymentScalerWebhook()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.ValidateCreate(context.Background(), tc.scaler)
			if tc.refused {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestModelDeploymentScalerWebhookRefusesToMoveTheTarget pins that the target of a scaler is
// fixed once it is set.
func TestModelDeploymentScalerWebhookRefusesToMoveTheTarget(t *testing.T) {
	old := modelDeploymentScaler()
	moved := modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
		s.Spec.TargetRef.Name = "other-72b"
	})

	w := modelDeploymentScalerWebhook()
	_, err := w.ValidateUpdate(context.Background(), old, moved)
	assert.Error(t, err)

	kept := modelDeploymentScaler()
	_, err = w.ValidateUpdate(context.Background(), old, kept)
	assert.NoError(t, err)
}

// TestModelDeploymentScalerWebhookValidatesAgainstTheTarget holds the rules that need the
// deployment the scaler plans for.
func TestModelDeploymentScalerWebhookValidatesAgainstTheTarget(t *testing.T) {
	cases := []struct {
		name    string
		target  *workercore.ModelDeployment
		scaler  *workercore.ModelDeploymentScaler
		refused bool
	}{
		{
			name:   "a role the target has is admitted",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(),
		},
		{
			name:    "a role the target does not have is refused",
			target:  modelDeploymentScalerTarget(workercore.ModelDeploymentRole{Name: "other", Replicas: 1}),
			scaler:  modelDeploymentScaler(),
			refused: true,
		},
		{
			name: "a role that runs a command of its own is refused",
			target: modelDeploymentScalerTarget(workercore.ModelDeploymentRole{
				Name: "server", Replicas: 1, Command: []string{"python", "serve.py"},
			}),
			scaler:  modelDeploymentScaler(),
			refused: true,
		},
		{
			name: "a role that runs the elastic-EP profile is refused",
			target: modelDeploymentScalerTarget(workercore.ModelDeploymentRole{
				Name: "server", Replicas: 1, ElasticEP: &workercore.ModelDeploymentRoleElasticEP{Width: 2},
			}),
			scaler:  modelDeploymentScaler(),
			refused: true,
		},
		{
			name: "a bound whose names do not fit is refused",
			target: modelDeploymentScalerTarget(workercore.ModelDeploymentRole{
				Name: "a-role-name-long-enough-to-push-the-name-past-the-label-limit", Replicas: 1,
			}),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles[0].Name = "a-role-name-long-enough-to-push-the-name-past-the-label-limit"
			}),
			refused: true,
		},
		{
			name:   "a server role without a target is refused",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
					PrefillTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
				}
			}),
			refused: true,
		},
		{
			name:   "a server role with a target is admitted",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
					ServerTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
				}
			}),
		},
		{
			name: "an encode role coupled to a prefill role is admitted",
			target: modelDeploymentScalerTarget(
				workercore.ModelDeploymentRole{Name: "server", Replicas: 1},
				workercore.ModelDeploymentRole{
					Name: "encode", Replicas: 1, Kind: workercore.ModelDeploymentRoleKindEncode,
				},
				workercore.ModelDeploymentRole{
					Name: "prefill", Replicas: 1, Kind: workercore.ModelDeploymentRoleKindPrefill,
				},
			),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
					{Name: "server", MinReplicas: 1, MaxReplicas: 4},
					{Name: "encode", MinReplicas: 1, MaxReplicas: 4},
					{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
				}
				s.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{{
					NumeratorRole: "encode", DenominatorRole: "prefill",
					Min: ptr.To(resource.MustParse("1")), Max: ptr.To(resource.MustParse("1")),
				}}
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
					ServerTargetTokensPerSecond:  ptr.To(resource.MustParse("1000")),
					PrefillTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
					DecodeTargetTokensPerSecond:  ptr.To(resource.MustParse("1000")),
				}
			}),
		},
		{
			name: "an encode role coupled to nothing is refused",
			target: modelDeploymentScalerTarget(
				workercore.ModelDeploymentRole{Name: "server", Replicas: 1},
				workercore.ModelDeploymentRole{
					Name: "encode", Replicas: 1, Kind: workercore.ModelDeploymentRoleKindEncode,
				},
			),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
					{Name: "server", MinReplicas: 1, MaxReplicas: 4},
					{Name: "encode", MinReplicas: 1, MaxReplicas: 4},
				}
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
					ServerTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
				}
			}),
			refused: true,
		},
		{
			name:   "a proportional role without a target of its own is refused",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypeProportional
				s.Spec.Algorithm.TokenThroughput = nil
				s.Spec.Algorithm.Proportional = &workercore.ModelDeploymentScalerProportionalSpec{
					Metric: workercore.ModelDeploymentScalerProportionalMetricQueueDepth,
				}
			}),
			refused: true,
		},
		{
			name:   "a proportional role with a target of its own is admitted",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypeProportional
				s.Spec.Algorithm.TokenThroughput = nil
				s.Spec.Algorithm.Proportional = &workercore.ModelDeploymentScalerProportionalSpec{
					Metric: workercore.ModelDeploymentScalerProportionalMetricQueueDepth,
					RoleTargets: []workercore.ModelDeploymentScalerRoleTarget{{
						Name: "server", Target: resource.MustParse("4"),
					}},
				}
			}),
		},
		{
			name:   "a proportional scaler that also carries a tokenThroughput block is refused",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypeProportional
				s.Spec.Algorithm.Proportional = &workercore.ModelDeploymentScalerProportionalSpec{
					Metric: workercore.ModelDeploymentScalerProportionalMetricQueueDepth,
					RoleTargets: []workercore.ModelDeploymentScalerRoleTarget{{
						Name: "server", Target: resource.MustParse("4"),
					}},
				}
			}),
			refused: true,
		},
		{
			name:   "a tokenThroughput scaler whose stable window exceeds 600s is refused",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput.StableWindowSeconds = 1800
			}),
			refused: true,
		},
		{
			name:   "a tokenThroughput scaler whose panic window is below 10s is refused",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput.PanicWindowSeconds = 5
			}),
			refused: true,
		},
		{
			name:   "a predictive scaler with non-positive capacity is refused",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				s.Spec.Algorithm.TokenThroughput = nil
				s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					ServerTokenCapacityPerReplica: ptr.To(resource.MustParse("-5")),
				}
			}),
			refused: true,
		},
		{
			name:   "a step policy with non-positive value is refused",
			target: modelDeploymentScalerTarget(),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Behavior = workercore.ModelDeploymentScalerBehaviorSpec{
					ScaleUp: workercore.ModelDeploymentScalerScaleUpRules{
						StepPolicy: &workercore.ModelDeploymentScalerStepPolicy{
							Type:  workercore.ModelDeploymentScalerStepPolicyTypePods,
							Value: 0,
						},
					},
				}
			}),
			refused: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := modelDeploymentScalerWebhook(tc.target)
			_, err := w.ValidateCreate(context.Background(), tc.scaler)
			if tc.refused {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestModelDeploymentScalerWebhookReturnsInternalErrorWhenAPIReaderIsNil(t *testing.T) {
	w := &ModelDeploymentScalerWebhook{APIReader: nil}
	scaler := modelDeploymentScaler()
	_, err := w.ValidateCreate(context.Background(), scaler)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the scaler target cannot be read, because this handler has no reader")
}
