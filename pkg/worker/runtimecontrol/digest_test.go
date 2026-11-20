package runtimecontrol

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func int32Ptr(v int32) *int32 {
	return &v
}

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestCanonicalQuantityDecimal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "milli half", raw: "500m", want: "0.5"},
		{name: "decimal half", raw: "0.5", want: "0.5"},
		{name: "trailing zero decimal half", raw: "0.50", want: "0.5"},
		{name: "milli one", raw: "1000m", want: "1"},
		{name: "integer one", raw: "1", want: "1"},
		{name: "decimal one", raw: "1.00", want: "1"},
		{name: "zero integer", raw: "0", want: "0"},
		{name: "zero decimal", raw: "0.00", want: "0"},
		{name: "small milli", raw: "50m", want: "0.05"},
		{name: "small decimal", raw: "0.050", want: "0.05"},
		{name: "fractional above one", raw: "1250m", want: "1.25"},
		{name: "decimal above one", raw: "1.250", want: "1.25"},
		{name: "kilo suffix", raw: "1k", want: "1000"},
		{name: "negative milli", raw: "-500m", want: "-0.5"},
	}

	require.Empty(t, CanonicalQuantityDecimal(nil))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CanonicalQuantityDecimal(quantityPtr(tc.raw))
			require.Equal(t, tc.want, got)
		})
	}
}

func TestComputeAppliedConfigDigest_CanonicalEquivalence(t *testing.T) {
	t.Parallel()

	mk := func(maxReq, cons, cacheThresh, single, acc, budget string) *workercore.ModelDeploymentRoleRuntimeControl {
		return &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				ParamSchedulingMaxActiveRequests:        maxReq,
				ParamSchedulingScheduleConservativeness: cons,
				ParamSchedulingCacheAwareThreshold:      cacheThresh,
				ParamSpeculativeAcceptThresholdSingle:   single,
				ParamSpeculativeAcceptThresholdAcc:      acc,
				ParamSpeculativeDraftBudgetFraction:     budget,
			},
		}
	}

	d1, err := ComputeAppliedConfigDigest(mk("32", "0.5", "0.2", "0.8", "0.9", "1.0"), false)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(d1, "sha256:"))
	require.Len(t, d1, len("sha256:")+64)

	d2, err := ComputeAppliedConfigDigest(mk("+32", "500m", "200m", "800m", "900m", "1000m"), false)
	require.NoError(t, err)
	require.Equal(t, d1, d2)

	d3, err := ComputeAppliedConfigDigest(mk("0032", "0.50", "0.200", "0.80", "0.90", "1"), false)
	require.NoError(t, err)
	require.Equal(t, d1, d3)

	d4, err := ComputeAppliedConfigDigest(mk("32", "0.6", "0.2", "0.8", "0.9", "1.0"), false)
	require.NoError(t, err)
	require.NotEqual(t, d1, d4)
}

func TestComputeAppliedConfigDigest_PausedAndElasticExclusion(t *testing.T) {
	t.Parallel()

	nilDigest, err := ComputeAppliedConfigDigest(nil, false)
	require.NoError(t, err)

	emptyDigest, err := ComputeAppliedConfigDigest(&workercore.ModelDeploymentRoleRuntimeControl{}, false)
	require.NoError(t, err)
	require.Equal(t, nilDigest, emptyDigest)

	// elastic.dataParallelWidth is reconciled by Elastic EP, not /gpustack/v1/runtime_config.
	elasticOnlyDigest, err := ComputeAppliedConfigDigest(&workercore.ModelDeploymentRoleRuntimeControl{
		Parameters: map[string]string{
			ParamElasticDataParallelWidth: "8",
		},
	}, false)
	require.NoError(t, err)
	require.Equal(t, nilDigest, elasticOnlyDigest)

	withSentinelA, err := ComputeAppliedConfigDigest(&workercore.ModelDeploymentRoleRuntimeControl{
		Parameters: map[string]string{
			ParamSchedulingMaxActiveRequests: "16",
			ParamElasticDataParallelWidth:    "4",
		},
	}, false)
	require.NoError(t, err)

	withSentinelB, err := ComputeAppliedConfigDigest(&workercore.ModelDeploymentRoleRuntimeControl{
		Parameters: map[string]string{
			ParamSchedulingMaxActiveRequests: "16",
			ParamElasticDataParallelWidth:    "8",
		},
	}, false)
	require.NoError(t, err)
	require.Equal(t, withSentinelA, withSentinelB)
	require.NotEqual(t, nilDigest, withSentinelA)

	// Paused state is part of the digest.
	pausedDigest, err := ComputeAppliedConfigDigest(nil, true)
	require.NoError(t, err)
	require.NotEqual(t, nilDigest, pausedDigest)

	pausedWithSentinel, err := ComputeAppliedConfigDigest(&workercore.ModelDeploymentRoleRuntimeControl{
		Parameters: map[string]string{
			ParamSchedulingMaxActiveRequests: "16",
		},
	}, true)
	require.NoError(t, err)
	require.NotEqual(t, withSentinelA, pausedWithSentinel)
}

func TestComputeAppliedConfigDigest_ErrorsOnInvalidParameters(t *testing.T) {
	t.Parallel()

	_, err := ComputeAppliedConfigDigest(&workercore.ModelDeploymentRoleRuntimeControl{
		Parameters: map[string]string{"scheduling.unknown": "1"},
	}, false)
	require.ErrorIs(t, err, ErrInvalidArgument)

	_, err = ComputeAppliedConfigDigest(&workercore.ModelDeploymentRoleRuntimeControl{
		Parameters: map[string]string{ParamSchedulingMaxActiveRequests: "not-int"},
	}, false)
	require.ErrorIs(t, err, ErrInvalidArgument)
}
