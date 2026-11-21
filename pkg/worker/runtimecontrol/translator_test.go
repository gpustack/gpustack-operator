package runtimecontrol

import (
	"testing"

	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestRegistryMetadataAndSentinelKeys(t *testing.T) {
	t.Parallel()

	all := AllParameterKeys()
	require.Len(t, all, 15)
	require.Contains(t, all, ParamElasticDataParallelWidth)

	allSentinel := AllSentinelParameterKeys("")
	require.Len(t, allSentinel, 14)
	require.NotContains(t, allSentinel, ParamElasticDataParallelWidth)

	vllmKeys := AllSentinelParameterKeys(workercore.ModelDeploymentEngineVLLM)
	require.Equal(t, []string{
		ParamSchedulingCacheAwareThreshold,
		ParamSchedulingCacheAwareWindow,
		ParamSchedulingLongPrefillTokenThreshold,
		ParamSchedulingMaxActiveRequests,
		ParamSchedulingMaxAsyncKVLoadsPerStep,
		ParamSchedulingMaxBatchedTokens,
		ParamSchedulingMaxInflightKVLoads,
	}, vllmKeys)

	sglangKeys := AllSentinelParameterKeys(workercore.ModelDeploymentEngineSGLang)
	require.Equal(t, []string{
		ParamSchedulingMaxActiveRequests,
		ParamSchedulingMaxBatchedTokens,
		ParamSchedulingPPMaxMicroBatchSize,
		ParamSchedulingPrefillDecodeInterval,
		ParamSchedulingPrefillMaxRequests,
		ParamSchedulingScheduleConservativeness,
		ParamSpeculativeAcceptThresholdAcc,
		ParamSpeculativeAcceptThresholdSingle,
		ParamSpeculativeDraftBudgetFraction,
	}, sglangKeys)

	require.Empty(t, AllSentinelParameterKeys("UnknownEngine"))

	// LookupParameter returns defensive copies.
	spec, ok := LookupParameter(ParamSchedulingMaxActiveRequests)
	require.True(t, ok)
	require.Equal(t, ParameterKindInt, spec.Kind)
	require.True(t, spec.SentinelManaged)
	require.NotNil(t, spec.MinInt)
	require.Equal(t, int64(1), *spec.MinInt)
	spec.Engines[0] = "mutated"
	*spec.MinInt = 999
	spec.CeilingFlagsByEngine[workercore.ModelDeploymentEngineVLLM] = "--mutated"

	specAgain, ok := LookupParameter(ParamSchedulingMaxActiveRequests)
	require.True(t, ok)
	require.Equal(t, int64(1), *specAgain.MinInt)
	require.Equal(t, "--max-num-seqs", specAgain.CeilingFlagsByEngine[workercore.ModelDeploymentEngineVLLM])
	require.NotContains(t, specAgain.Engines, "mutated")

	_, ok = LookupParameter("scheduling.doesNotExist")
	require.False(t, ok)

	// SentinelParameterKeys and ComputeResetFields omit elastic.dataParallelWidth.
	require.Nil(t, SentinelParameterKeys(nil))
	require.Nil(t, SentinelParameterKeys(map[string]string{ParamElasticDataParallelWidth: "4"}))
	require.Equal(t, []string{
		ParamSchedulingMaxActiveRequests,
		ParamSchedulingMaxBatchedTokens,
	}, SentinelParameterKeys(map[string]string{
		ParamSchedulingMaxBatchedTokens:  "2048",
		ParamSchedulingMaxActiveRequests: "32",
		ParamElasticDataParallelWidth:    "4",
	}))

	require.Nil(t, ComputeResetFields(nil, nil))
	require.Equal(t, []string{
		ParamSchedulingCacheAwareWindow,
		ParamSchedulingMaxBatchedTokens,
	}, ComputeResetFields(
		map[string]string{
			ParamSchedulingMaxActiveRequests: "16",
			ParamElasticDataParallelWidth:    "4",
		},
		[]string{
			ParamSchedulingMaxBatchedTokens,
			ParamSchedulingMaxActiveRequests,
			ParamSchedulingCacheAwareWindow,
			ParamSchedulingCacheAwareWindow,
			ParamElasticDataParallelWidth,
			"unknown.key",
		},
	))
}

func TestElasticWidthHelpers(t *testing.T) {
	t.Parallel()

	require.False(t, RoleHasElasticEP(nil))
	require.False(t, RoleHasElasticEP(&workercore.ModelDeploymentRole{}))
	require.False(t, RoleHasElasticEP(&workercore.ModelDeploymentRole{
		RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{ParamSchedulingMaxActiveRequests: "16"},
		},
	}))
	require.False(t, RoleHasElasticEP(&workercore.ModelDeploymentRole{
		RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{ParamElasticDataParallelWidth: "  "},
		},
	}))

	// SetElasticWidth initializes nested structs and preserves unrelated keys.
	SetElasticWidth(nil, 4)
	role := &workercore.ModelDeploymentRole{}
	SetElasticWidth(role, 4)
	require.True(t, RoleHasElasticEP(role))
	require.Equal(t, "4", role.RuntimeControl.Parameters[ParamElasticDataParallelWidth])

	role.RuntimeControl.Parameters[ParamSchedulingMaxActiveRequests] = "32"
	SetElasticWidth(role, 8)
	require.Equal(t, "8", role.RuntimeControl.Parameters[ParamElasticDataParallelWidth])
	require.Equal(t, "32", role.RuntimeControl.Parameters[ParamSchedulingMaxActiveRequests])

	cases := []struct {
		name      string
		role      *workercore.ModelDeploymentRole
		wantWidth int32
		wantOK    bool
		wantErr   bool
	}{
		{name: "nil role", role: nil, wantOK: false},
		{name: "nil runtimeControl", role: &workercore.ModelDeploymentRole{}, wantOK: false},
		{
			name: "key absent",
			role: &workercore.ModelDeploymentRole{
				RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
					Parameters: map[string]string{ParamSchedulingMaxActiveRequests: "16"},
				},
			},
			wantOK: false,
		},
		{
			name: "min bound 2",
			role: &workercore.ModelDeploymentRole{
				RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
					Parameters: map[string]string{ParamElasticDataParallelWidth: "2"},
				},
			},
			wantWidth: 2,
			wantOK:    true,
		},
		{
			name: "max bound 64",
			role: &workercore.ModelDeploymentRole{
				RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
					Parameters: map[string]string{ParamElasticDataParallelWidth: "64"},
				},
			},
			wantWidth: 64,
			wantOK:    true,
		},
		{
			name: "below min 1",
			role: &workercore.ModelDeploymentRole{
				RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
					Parameters: map[string]string{ParamElasticDataParallelWidth: "1"},
				},
			},
			wantOK:  true,
			wantErr: true,
		},
		{
			name: "above max 65",
			role: &workercore.ModelDeploymentRole{
				RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
					Parameters: map[string]string{ParamElasticDataParallelWidth: "65"},
				},
			},
			wantOK:  true,
			wantErr: true,
		},
		{
			name: "non-integer",
			role: &workercore.ModelDeploymentRole{
				RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
					Parameters: map[string]string{ParamElasticDataParallelWidth: "4.5"},
				},
			},
			wantOK:  true,
			wantErr: true,
		},
		{
			name: "empty value",
			role: &workercore.ModelDeploymentRole{
				RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
					Parameters: map[string]string{ParamElasticDataParallelWidth: ""},
				},
			},
			wantOK:  true,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, ok, err := ParseElasticWidth(tc.role)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantWidth, w)
		})
	}
}

func TestParseExtraArgsBootCeilings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		engine    string
		extraArgs []string
		want      BootCeilings
	}{
		{
			name:   "vllm flags with equals, space, underscore, short pp, kv connector",
			engine: workercore.ModelDeploymentEngineVLLM,
			extraArgs: []string{
				"--max-num-seqs", "64",
				"--max_num_batched_tokens=4096",
				"--elastic_ep_max_dp_size", "+16",
				"-pp", "2",
				"--kv_transfer_config", `{"kv_connector":"Mooncake"}`,
				"--", "--max-num-seqs=1",
			},
			want: BootCeilings{
				MaxActiveRequests:      int32Ptr(64),
				MaxBatchedTokens:       int32Ptr(4096),
				ElasticMaxDataParallel: int32Ptr(16),
				PipelineParallelSize:   int32Ptr(2),
				HasKVConnector:         true,
			},
		},
		{
			name:   "vllm last flag wins and dotted kv transfer config",
			engine: workercore.ModelDeploymentEngineVLLM,
			extraArgs: []string{
				"--max-num-seqs=128",
				"--max-num-seqs=32",
				"--kv-transfer-config.kv_connector=Mooncake",
				"--pipeline-parallel-size=4",
			},
			want: BootCeilings{
				MaxActiveRequests:    int32Ptr(32),
				PipelineParallelSize: int32Ptr(4),
				HasKVConnector:       true,
			},
		},
		{
			name:   "vllm ignores malformed or missing flag values",
			engine: workercore.ModelDeploymentEngineVLLM,
			extraArgs: []string{
				"--max-num-seqs", "--dtype", "bfloat16",
				"--max-num-batched-tokens=notanint",
				"--elastic-ep-max-dp-size=-4",
				"--pipeline-parallel-size",
			},
			want: BootCeilings{},
		},
		{
			name:   "sglang flags",
			engine: workercore.ModelDeploymentEngineSGLang,
			extraArgs: []string{
				"--max-running-requests", "128",
				"--chunked-prefill-size=8192",
				"--pp-size=4",
				"--", "--pp-size=1",
			},
			want: BootCeilings{
				MaxActiveRequests:    int32Ptr(128),
				MaxBatchedTokens:     int32Ptr(8192),
				PipelineParallelSize: int32Ptr(4),
			},
		},
		{
			name:   "sglang pipeline-parallel-size alias",
			engine: workercore.ModelDeploymentEngineSGLang,
			extraArgs: []string{
				"--pipeline-parallel-size", "2",
			},
			want: BootCeilings{
				PipelineParallelSize: int32Ptr(2),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseExtraArgsBootCeilings(tc.engine, tc.extraArgs)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestValidateParameters(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		ctx         ValidationContext
		params      map[string]string
		wantErrs    int
		wantKey     string
		wantReason  ParameterErrorReason
		wantContain string
	}{
		{
			name:     "empty params ok",
			ctx:      ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:   nil,
			wantErrs: 0,
		},
		{
			name: "valid vLLM NVIDIA full set",
			ctx: ValidationContext{
				Engine:         workercore.ModelDeploymentEngineVLLM,
				Manufacturer:   nodefeature.ManufacturerNVIDIA,
				ExtraArgs:      []string{"--max-num-seqs=64", "--max-num-batched-tokens=4096", "--elastic-ep-max-dp-size=8"},
				HasKVConnector: true,
			},
			params: map[string]string{
				ParamSchedulingMaxActiveRequests:         "32",
				ParamSchedulingMaxBatchedTokens:          "2048",
				ParamSchedulingLongPrefillTokenThreshold: "1024",
				ParamSchedulingCacheAwareWindow:          "0",
				ParamSchedulingCacheAwareThreshold:       "500m",
				ParamSchedulingMaxInflightKVLoads:        "4",
				ParamSchedulingMaxAsyncKVLoadsPerStep:    "2",
				ParamElasticDataParallelWidth:            "8",
			},
			wantErrs: 0,
		},
		{
			name: "valid vLLM Ascend with extraArgs kv-transfer-config",
			ctx: ValidationContext{
				Engine:       workercore.ModelDeploymentEngineVLLM,
				Manufacturer: nodefeature.ManufacturerAscend,
				ExtraArgs:    []string{"--kv-transfer-config", `{"kv_connector":"Mooncake"}`},
			},
			params: map[string]string{
				ParamSchedulingMaxActiveRequests:      "16",
				ParamSchedulingMaxInflightKVLoads:     "1",
				ParamSchedulingMaxAsyncKVLoadsPerStep: "1",
				ParamSchedulingCacheAwareThreshold:    "0.0",
				ParamElasticDataParallelWidth:         "2",
			},
			wantErrs: 0,
		},
		{
			name: "valid SGLang full set",
			ctx: ValidationContext{
				Engine:       workercore.ModelDeploymentEngineSGLang,
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				ExtraArgs:    []string{"--max-running-requests=64", "--chunked-prefill-size=4096", "--pp-size=2"},
			},
			params: map[string]string{
				ParamSchedulingMaxActiveRequests:        "32",
				ParamSchedulingMaxBatchedTokens:         "2048",
				ParamSchedulingPrefillMaxRequests:       "16",
				ParamSchedulingScheduleConservativeness: "1.5",
				ParamSchedulingPrefillDecodeInterval:    "0",
				ParamSchedulingPPMaxMicroBatchSize:      "16",
				ParamSpeculativeAcceptThresholdSingle:   "0.0",
				ParamSpeculativeAcceptThresholdAcc:      "1000m",
				ParamSpeculativeDraftBudgetFraction:     "1m",
			},
			wantErrs: 0,
		},
		{
			name:        "unknown parameter typo rejected",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:      map[string]string{"scheduling.maxActiveRequest": "16"},
			wantErrs:    1,
			wantKey:     "scheduling.maxActiveRequest",
			wantReason:  ParameterErrorUnknown,
			wantContain: "unknown",
		},
		{
			name:        "SGLang parameter rejected on vLLM",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:      map[string]string{ParamSchedulingPrefillMaxRequests: "8"},
			wantErrs:    1,
			wantKey:     ParamSchedulingPrefillMaxRequests,
			wantReason:  ParameterErrorForbidden,
			wantContain: "not supported on engine",
		},
		{
			name:        "vLLM elastic parameter rejected on SGLang",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineSGLang},
			params:      map[string]string{ParamElasticDataParallelWidth: "4"},
			wantErrs:    1,
			wantKey:     ParamElasticDataParallelWidth,
			wantReason:  ParameterErrorForbidden,
			wantContain: "not supported on engine",
		},
		{
			name: "unsupported manufacturer rejected on vLLM",
			ctx: ValidationContext{
				Engine:       workercore.ModelDeploymentEngineVLLM,
				Manufacturer: nodefeature.ManufacturerAMD,
			},
			params:      map[string]string{ParamSchedulingMaxActiveRequests: "16"},
			wantErrs:    1,
			wantKey:     ParamSchedulingMaxActiveRequests,
			wantReason:  ParameterErrorForbidden,
			wantContain: "manufacturer",
		},
		{
			name:        "non-integer rejected for int parameter",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:      map[string]string{ParamSchedulingMaxActiveRequests: "16.5"},
			wantErrs:    1,
			wantKey:     ParamSchedulingMaxActiveRequests,
			wantReason:  ParameterErrorInvalid,
			wantContain: "base-10 integer",
		},
		{
			name:        "zero rejected for >= 1 int parameter",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:      map[string]string{ParamSchedulingMaxActiveRequests: "0"},
			wantErrs:    1,
			wantKey:     ParamSchedulingMaxActiveRequests,
			wantReason:  ParameterErrorInvalid,
			wantContain: "greater than or equal to 1",
		},
		{
			name:        "negative rejected for >= 0 int parameter",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:      map[string]string{ParamSchedulingCacheAwareWindow: "-1"},
			wantErrs:    1,
			wantKey:     ParamSchedulingCacheAwareWindow,
			wantReason:  ParameterErrorInvalid,
			wantContain: "greater than or equal to 0",
		},
		{
			name:        "elastic width below min rejected",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:      map[string]string{ParamElasticDataParallelWidth: "1"},
			wantErrs:    1,
			wantKey:     ParamElasticDataParallelWidth,
			wantReason:  ParameterErrorInvalid,
			wantContain: "between 2 and 64",
		},
		{
			name:        "elastic width above max rejected",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:      map[string]string{ParamElasticDataParallelWidth: "65"},
			wantErrs:    1,
			wantKey:     ParamElasticDataParallelWidth,
			wantReason:  ParameterErrorInvalid,
			wantContain: "between 2 and 64",
		},
		{
			name:        "scheduleConservativeness below 0.1 rejected",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineSGLang},
			params:      map[string]string{ParamSchedulingScheduleConservativeness: "0.09"},
			wantErrs:    1,
			wantKey:     ParamSchedulingScheduleConservativeness,
			wantReason:  ParameterErrorInvalid,
			wantContain: "[0.1, 2.0]",
		},
		{
			name:        "scheduleConservativeness above 2.0 rejected",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineSGLang},
			params:      map[string]string{ParamSchedulingScheduleConservativeness: "2.01"},
			wantErrs:    1,
			wantKey:     ParamSchedulingScheduleConservativeness,
			wantReason:  ParameterErrorInvalid,
			wantContain: "[0.1, 2.0]",
		},
		{
			name:        "draftBudgetFraction strictly > 0 rejects 0",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineSGLang},
			params:      map[string]string{ParamSpeculativeDraftBudgetFraction: "0.0"},
			wantErrs:    1,
			wantKey:     ParamSpeculativeDraftBudgetFraction,
			wantReason:  ParameterErrorInvalid,
			wantContain: "(0.0, 1.0]",
		},
		{
			name:        "draftBudgetFraction rejects binary SI suffix",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineSGLang},
			params:      map[string]string{ParamSpeculativeDraftBudgetFraction: "1Ki"},
			wantErrs:    1,
			wantKey:     ParamSpeculativeDraftBudgetFraction,
			wantReason:  ParameterErrorInvalid,
			wantContain: "valid decimal quantity",
		},
		{
			name:        "cacheAwareThreshold malformed decimal rejected",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:      map[string]string{ParamSchedulingCacheAwareThreshold: "not-a-number"},
			wantErrs:    1,
			wantKey:     ParamSchedulingCacheAwareThreshold,
			wantReason:  ParameterErrorInvalid,
			wantContain: "valid decimal quantity",
		},
		{
			name:        "maxInflightKVLoads requires KV connector",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineVLLM},
			params:      map[string]string{ParamSchedulingMaxInflightKVLoads: "4"},
			wantErrs:    1,
			wantKey:     ParamSchedulingMaxInflightKVLoads,
			wantReason:  ParameterErrorForbidden,
			wantContain: "KV connector",
		},
		{
			name:        "ppMaxMicroBatchSize requires ppSize > 1",
			ctx:         ValidationContext{Engine: workercore.ModelDeploymentEngineSGLang, ExtraArgs: []string{"--pp-size=1"}},
			params:      map[string]string{ParamSchedulingPPMaxMicroBatchSize: "2"},
			wantErrs:    1,
			wantKey:     ParamSchedulingPPMaxMicroBatchSize,
			wantReason:  ParameterErrorInvalid,
			wantContain: "pipeline parallelism",
		},
		{
			name: "vLLM maxActiveRequests exceeds --max-num-seqs ceiling",
			ctx: ValidationContext{
				Engine:    workercore.ModelDeploymentEngineVLLM,
				ExtraArgs: []string{"--max-num-seqs=32"},
			},
			params:      map[string]string{ParamSchedulingMaxActiveRequests: "33"},
			wantErrs:    1,
			wantKey:     ParamSchedulingMaxActiveRequests,
			wantReason:  ParameterErrorInvalid,
			wantContain: "--max-num-seqs",
		},
		{
			name: "SGLang maxBatchedTokens exceeds --chunked-prefill-size ceiling",
			ctx: ValidationContext{
				Engine:    workercore.ModelDeploymentEngineSGLang,
				ExtraArgs: []string{"--chunked-prefill-size=2048"},
			},
			params:      map[string]string{ParamSchedulingMaxBatchedTokens: "4096"},
			wantErrs:    1,
			wantKey:     ParamSchedulingMaxBatchedTokens,
			wantReason:  ParameterErrorInvalid,
			wantContain: "--chunked-prefill-size",
		},
		{
			name: "elastic width exceeds --elastic-ep-max-dp-size ceiling",
			ctx: ValidationContext{
				Engine:    workercore.ModelDeploymentEngineVLLM,
				ExtraArgs: []string{"--elastic-ep-max-dp-size=8"},
			},
			params:      map[string]string{ParamElasticDataParallelWidth: "16"},
			wantErrs:    1,
			wantKey:     ParamElasticDataParallelWidth,
			wantReason:  ParameterErrorInvalid,
			wantContain: "--elastic-ep-max-dp-size",
		},
		{
			name: "longPrefillTokenThreshold exceeds runtime maxBatchedTokens",
			ctx: ValidationContext{
				Engine:    workercore.ModelDeploymentEngineVLLM,
				ExtraArgs: []string{"--max-num-batched-tokens=4096"},
			},
			params: map[string]string{
				ParamSchedulingMaxBatchedTokens:          "1024",
				ParamSchedulingLongPrefillTokenThreshold: "2048",
			},
			wantErrs:    1,
			wantKey:     ParamSchedulingLongPrefillTokenThreshold,
			wantReason:  ParameterErrorInvalid,
			wantContain: "effective maxBatchedTokens (1024)",
		},
		{
			name: "longPrefillTokenThreshold exceeds extraArgs --max-num-batched-tokens",
			ctx: ValidationContext{
				Engine:    workercore.ModelDeploymentEngineVLLM,
				ExtraArgs: []string{"--max-num-batched-tokens=1024"},
			},
			params: map[string]string{
				ParamSchedulingLongPrefillTokenThreshold: "1025",
			},
			wantErrs:    1,
			wantKey:     ParamSchedulingLongPrefillTokenThreshold,
			wantReason:  ParameterErrorInvalid,
			wantContain: "effective maxBatchedTokens (1024)",
		},
		{
			name: "prefillMaxRequests exceeds runtime maxActiveRequests",
			ctx: ValidationContext{
				Engine:    workercore.ModelDeploymentEngineSGLang,
				ExtraArgs: []string{"--max-running-requests=64"},
			},
			params: map[string]string{
				ParamSchedulingMaxActiveRequests:  "16",
				ParamSchedulingPrefillMaxRequests: "20",
			},
			wantErrs:    1,
			wantKey:     ParamSchedulingPrefillMaxRequests,
			wantReason:  ParameterErrorInvalid,
			wantContain: "effective maxActiveRequests (16)",
		},
		{
			name: "ppMaxMicroBatchSize exceeds effectiveMaxActiveRequests / ppSize",
			ctx: ValidationContext{
				Engine:    workercore.ModelDeploymentEngineSGLang,
				ExtraArgs: []string{"--max-running-requests=32", "--pp-size=4"},
			},
			params: map[string]string{
				ParamSchedulingPPMaxMicroBatchSize: "9",
			},
			wantErrs:    1,
			wantKey:     ParamSchedulingPPMaxMicroBatchSize,
			wantReason:  ParameterErrorInvalid,
			wantContain: "32 / 4 = 8",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := ValidateParameters(tc.ctx, tc.params)
			require.Len(t, errs, tc.wantErrs)
			if tc.wantErrs > 0 {
				require.Equal(t, tc.wantKey, errs[0].Key)
				require.Equal(t, tc.wantReason, errs[0].Reason)
				require.Contains(t, errs[0].Message, tc.wantContain)
				require.Contains(t, errs[0].Error(), tc.wantKey)
			}
		})
	}
}

func TestCanonicalizeAndBuildSentinelParameters(t *testing.T) {
	t.Parallel()

	cNil, err := CanonicalizeParameters(nil)
	require.NoError(t, err)
	require.Nil(t, cNil)

	canon, err := CanonicalizeParameters(map[string]string{
		ParamSchedulingMaxActiveRequests:        "+16",
		ParamSchedulingScheduleConservativeness: "0.50",
		ParamSpeculativeAcceptThresholdSingle:   "500m",
		ParamSpeculativeAcceptThresholdAcc:      "1000m",
		ParamElasticDataParallelWidth:           "04",
	})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		ParamSchedulingMaxActiveRequests:        "16",
		ParamSchedulingScheduleConservativeness: "0.5",
		ParamSpeculativeAcceptThresholdSingle:   "0.5",
		ParamSpeculativeAcceptThresholdAcc:      "1",
		ParamElasticDataParallelWidth:           "4",
	}, canon)

	_, err = CanonicalizeParameters(map[string]string{"unknown.key": "1"})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = CanonicalizeParameters(map[string]string{ParamSchedulingMaxActiveRequests: "abc"})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = CanonicalizeParameters(map[string]string{ParamSchedulingCacheAwareThreshold: "abc"})
	require.ErrorIs(t, err, ErrInvalidArgument)

	// BuildSentinelParameters converts to typed JSON numbers and omits elastic.dataParallelWidth.
	sNil, err := BuildSentinelParameters(nil)
	require.NoError(t, err)
	require.Nil(t, sNil)

	sElasticOnly, err := BuildSentinelParameters(map[string]string{
		ParamElasticDataParallelWidth: "8",
	})
	require.NoError(t, err)
	require.Nil(t, sElasticOnly)

	sentinel, err := BuildSentinelParameters(map[string]string{
		ParamSchedulingMaxActiveRequests:    "32",
		ParamSchedulingCacheAwareThreshold:  "500m",
		ParamSpeculativeDraftBudgetFraction: "0.250",
		ParamElasticDataParallelWidth:       "8",
	})
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		ParamSchedulingMaxActiveRequests:    int64(32),
		ParamSchedulingCacheAwareThreshold:  float64(0.5),
		ParamSpeculativeDraftBudgetFraction: float64(0.25),
	}, sentinel)

	_, err = BuildSentinelParameters(map[string]string{"unknown.key": "1"})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = BuildSentinelParameters(map[string]string{ParamSchedulingMaxActiveRequests: "bad"})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = BuildSentinelParameters(map[string]string{ParamSchedulingCacheAwareThreshold: "bad"})
	require.ErrorIs(t, err, ErrInvalidArgument)
}

func TestValidateParametersRejectsHugeDecimalExponents(t *testing.T) {
	ctx := ValidationContext{
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Manufacturer: "nvidia",
	}
	for _, raw := range []string{"1e100000000", "1e-100000000", "5e50000000"} {
		errs := ValidateParameters(ctx, map[string]string{
			ParamSchedulingCacheAwareThreshold: raw,
		})
		require.Len(t, errs, 1, "raw=%s", raw)
		require.Equal(t, ParameterErrorInvalid, errs[0].Reason)
	}
}
