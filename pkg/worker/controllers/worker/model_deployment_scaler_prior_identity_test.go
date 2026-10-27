package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workerapi "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

// The qualified InferenceX snapshot this matching serves: DeepSeek-R1-0528 fp8
// on eight AMD Instinct MI325X accelerators, SGLang pinned by image digest with
// the source build string recorded as provenance.
const (
	inferenceXImage       = "lmsysorg/sglang:v0.5.22-rocm720-mi30x@sha256:58b6bc333f527c88fe5ff1b4bd8d17e82214eabcb3044549117159878967c930"
	inferenceXBuild       = "sglang 0.5.22 (baa9befa381511cd4d56f83980951e5a45fffc6d)"
	inferenceXModel       = "deepseek-ai/DeepSeek-R1-0528"
	inferenceXRevision    = "4236a6af538feda4548eca9ab308586007567f52"
	inferenceXAccelerator = "AMD Instinct MI325X"
)

func inferenceXIdentity() priors.Identity {
	return priors.Identity{
		Model: priors.ModelIdentity{
			Name:         inferenceXModel,
			Revision:     inferenceXRevision,
			Tokenizer:    inferenceXModel,
			Quantization: "fp8",
		},
		Engine: priors.EngineIdentity{
			Image: inferenceXImage,
			Build: inferenceXBuild,
		},
		Hardware: priors.HardwareIdentity{
			Accelerator: inferenceXAccelerator,
			MemoryGB:    256,
			FormFactor:  "OAM Module",
		},
	}
}

// inferenceXLaunchArguments are the launch arguments the pinned snapshot
// recorded. The measured run itself carries no --dtype and no offload
// argument; those shapes are exercised by their own staged cases.
func inferenceXLaunchArguments() []string {
	return []string{
		"--tensor-parallel-size", "8",
		"--mem-fraction-static", "0.68",
		"--cuda-graph-max-bs-decode", "128",
		"--chunked-prefill-size", "131072",
		"--num-continuous-decode-steps", "4",
		"--max-prefill-tokens", "131072",
		"--kv-cache-dtype", "fp8_e4m3",
		"--attention-backend", "aiter",
		"--disable-radix-cache",
		"--data-parallel-size", "1",
		"--expert-parallel-size", "1",
	}
}

// inferenceXVLLMLaunchArguments returns the launch arguments of the engine whose
// parallelism reader consumes the environment.
//
// IT TURNS THE CACHE SWITCH INTO THE SPELLING THE ENGINE READS, because the
// gate refuses a cache flag the engine does not own.
func inferenceXVLLMLaunchArguments() []string {
	args := make([]string, 0, len(inferenceXLaunchArguments()))
	for _, arg := range inferenceXLaunchArguments() {
		if arg == "--disable-radix-cache" {
			args = append(args, "--no-enable-prefix-caching")
			continue
		}
		args = append(args, arg)
	}
	return args
}

// inferenceXDeployment builds the target the prior is matched against, with
// the identity annotations the controller derives from the declared identity.
func inferenceXDeployment(image string) *workercore.ModelDeployment {
	identity := inferenceXIdentity()
	modelID, err := priors.Digest(identity.Model)
	if err != nil {
		panic(err)
	}
	hardwareID, err := priors.Digest(identity.Hardware)
	if err != nil {
		panic(err)
	}
	return &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{
			Name:      "dsr1-mi325x",
			Namespace: "comparison",
			Annotations: map[string]string{
				workercore.ModelDeploymentScalerModelIDAnnotationKey:    "model:" + modelID,
				workercore.ModelDeploymentScalerHardwareIDAnnotationKey: "hardware:" + hardwareID,
				workercore.ModelDeploymentScalerPrecisionAnnotationKey:  identity.Model.Quantization,
			},
		},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{
				Name:        identity.Model.Name,
				ArtifactRef: &core.LocalObjectReference{Name: "dsr1-weights"},
			},
			Engine: workercore.ModelDeploymentEngine{
				Name:  workercore.ModelDeploymentEngineSGLang,
				Image: image,
			},
			Roles: []workercore.ModelDeploymentRole{{
				Name:         "server",
				Kind:         workercore.ModelDeploymentRoleKindServer,
				InstanceType: "mi325x-oam",
				ExtraArgs:    inferenceXLaunchArguments(),
			}},
		},
	}
}

func inferenceXObjects(t *testing.T, deployment *workercore.ModelDeployment) []ctrlcli.Object {
	t.Helper()
	artifact := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Name: "dsr1-weights", Namespace: deployment.Namespace},
		Spec: workercore.ModelArtifactSpec{
			Source: workercore.ModelArtifactSource{
				HuggingFace: &workercore.ModelArtifactHubSource{Repository: inferenceXModel},
			},
		},
		Status: workercore.ModelArtifactStatus{
			Resolved: &workercore.ModelArtifactResolved{Revision: inferenceXRevision},
		},
	}
	instance := &workerapi.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "mi325x-oam"},
		Spec:       workercore.InstanceTypeSpec{Acceleratable: true},
		Status: workercore.InstanceTypeStatus{
			Detail: workercore.InstanceTypeDetail{
				Product:                       inferenceXAccelerator,
				InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{Memory: "256Gi"},
			},
		},
	}
	return []ctrlcli.Object{artifact, instance}
}

func inferenceXClient(t *testing.T, deployment *workercore.ModelDeployment) ctrlcli.Client {
	t.Helper()
	objects := inferenceXObjects(t, deployment)
	return ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
}

// inferenceXRequest builds the request the prior is resolved against.
//
// IT READS THE DECLARATION THE DEPLOYMENT STATES FIRST, because the catalog is
// selected by what the deployment states and not the other way round.
func inferenceXRequest(
	t *testing.T, client ctrlcli.Client, deployment *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole, profile *priors.Profile,
) (priors.Request, error) {
	t.Helper()
	declaration, err := modelDeploymentScalerPriorDeclarationOf(context.Background(), client, deployment, role)
	if err != nil {
		return priors.Request{}, err
	}
	return modelDeploymentScalerPriorRequest(deployment, role, profile, declaration)
}

// TestModelDeploymentScalerPriorRequestAcceptsBuildProvenance asserts the
// qualified InferenceX deployment matches on the immutable image digest while
// the source build string is carried as recorded provenance, not compared to
// the digest.
func TestModelDeploymentScalerPriorRequestAcceptsBuildProvenance(t *testing.T) {
	identity := inferenceXIdentity()
	role := workercore.ModelDeploymentRole{
		Name:         "server",
		Kind:         workercore.ModelDeploymentRoleKindServer,
		InstanceType: "mi325x-oam",
		Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
		ExtraArgs: append(inferenceXLaunchArguments(),
			"--dtype", "bf16", "--cpu-offload-gb", "0"),
	}
	deployment := inferenceXDeployment(inferenceXImage)
	client := inferenceXClient(t, deployment)

	request, err := inferenceXRequest(t, client, deployment, &role,
		&priors.Profile{
			Identity:      identity,
			ServingConfig: priors.ServingConfig{EngineEnvComplete: true},
		})
	require.NoError(t, err)
	assert.Equal(t, inferenceXImage, request.Identity.Engine.Image)
	// THE BUILD IS RECORDED PROVENANCE AND NOT A CONSTRAINT, because a
	// deployment states the image it runs and not the build behind it.
	assert.Empty(t, request.Identity.Engine.Build)
	assert.Equal(t, "fp8_e4m3", request.ServingConfig.KVCacheDType)
	assert.Equal(t, "disabled", request.ServingConfig.PrefixCaching)
	assert.Equal(t, 8, request.Topology.GPUsPerReplica)
	assert.Equal(t, 8, request.Topology.TensorParallel)
}

func TestModelDeploymentScalerPriorRequestEngineEnv(t *testing.T) {
	recorded := []string{"SGLANG_AITER_MLA_PERSIST=1", "SGLANG_USE_AITER=1"}
	effectiveDefaults := map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"}

	requestWith := func(t *testing.T, env []workercore.ModelDeploymentEnvVar) (priors.Request, error) {
		identity := inferenceXIdentity()
		role := workercore.ModelDeploymentRole{
			Name:         "server",
			Kind:         workercore.ModelDeploymentRoleKindServer,
			InstanceType: "mi325x-oam",
			Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
			Env:          env,
			ExtraArgs:    inferenceXLaunchArguments(),
		}
		deployment := inferenceXDeployment(inferenceXImage)
		client := inferenceXClient(t, deployment)
		profile := priors.Profile{Identity: identity}
		profile.ServingConfig = priors.ServingConfig{
			Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
			PrefixCaching: "disabled", SpeculativeDecoding: "none",
			EngineEnv: recorded, EngineEnvComplete: true, EffectiveDefaults: effectiveDefaults,
		}
		return inferenceXRequest(t, client, deployment, &role, &profile)
	}

	t.Run("an exact engine environment matches", func(t *testing.T) {
		env := []workercore.ModelDeploymentEnvVar{
			{Name: "SGLANG_USE_AITER", Value: "1"},
			{Name: "SGLANG_AITER_MLA_PERSIST", Value: "1"},
		}
		request, err := requestWith(t, env)
		require.NoError(t, err)
		assert.Equal(t, recorded, request.ServingConfig.EngineEnv)
	})

	t.Run("a differing env value is refused", func(t *testing.T) {
		env := []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_USE_AITER", Value: "2"}}
		_, err := requestWith(t, env)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SGLANG_USE_AITER")
	})

	t.Run("an unrecorded env var is refused", func(t *testing.T) {
		env := []workercore.ModelDeploymentEnvVar{{Name: "EXTRA_VAR", Value: "1"}}
		_, err := requestWith(t, env)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not record")
	})

	t.Run("a recorded env var missing from the deployment is refused", func(t *testing.T) {
		env := []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_USE_AITER", Value: "1"}}
		_, err := requestWith(t, env)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SGLANG_AITER_MLA_PERSIST")
	})

	t.Run("a differing env value is reported by name only", func(t *testing.T) {
		env := []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_USE_AITER", Value: "super-secret-token"}}
		_, err := requestWith(t, env)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SGLANG_USE_AITER")
		assert.NotContains(t, err.Error(), "super-secret-token")
	})

	t.Run("a runtime expansion ref in a deployment env value is refused", func(t *testing.T) {
		env := []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_USE_AITER", Value: "$(OTHER_VAR)"}}
		_, err := requestWith(t, env)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "runtime expansion")
	})

	t.Run("an escaped dollar env value is refused", func(t *testing.T) {
		env := []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_USE_AITER", Value: "$$1"}}
		_, err := requestWith(t, env)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "runtime expansion")
	})
}

func TestModelDeploymentScalerPriorRequestOmittedDefaults(t *testing.T) {
	baseEffectiveDefaults := map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"}

	requestWith := func(t *testing.T, effectiveDefaults map[string]string) error {
		identity := inferenceXIdentity()
		role := workercore.ModelDeploymentRole{
			Name:         "server",
			Kind:         workercore.ModelDeploymentRoleKindServer,
			InstanceType: "mi325x-oam",
			Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
			ExtraArgs:    inferenceXLaunchArguments(),
		}
		deployment := inferenceXDeployment(inferenceXImage)
		client := inferenceXClient(t, deployment)
		profile := priors.Profile{Identity: identity}
		profile.ServingConfig = priors.ServingConfig{
			Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
			PrefixCaching: "disabled", SpeculativeDecoding: "none",
			EngineEnvComplete: true, EffectiveDefaults: effectiveDefaults,
		}
		_, err := inferenceXRequest(t, client, deployment, &role, &profile)
		return err
	}

	t.Run("recorded effective defaults cover the omitted dtype and offload", func(t *testing.T) {
		require.NoError(t, requestWith(t, baseEffectiveDefaults))
	})

	t.Run("an omitted dtype without a recorded effective default is accepted", func(t *testing.T) {
		err := requestWith(t, map[string]string{"--cpu-offload-gb": "0"})
		require.NoError(t, err)
	})
}

// TestModelDeploymentScalerPriorRequestMatchesTheEngineFamily asserts the
// deployment matches the profile on the engine family rather than on the
// image digest.
//
// IT MATTERS BECAUSE A DEPLOYMENT NAMES THE IMAGE IT RUNS AND A SOURCE NAMES
// THE IMAGE IT MEASURED, and the two differ in a tag or a digest while the
// engine is the same one.
func TestModelDeploymentScalerPriorRequestMatchesTheEngineFamily(t *testing.T) {
	tests := []struct {
		name  string
		image string
	}{
		{
			name:  "a different image digest matches",
			image: "lmsysorg/sglang:v0.5.22-rocm720-mi30x@sha256:" + strings.Repeat("9", 64),
		},
		{
			name:  "an unpinned image matches",
			image: "lmsysorg/sglang:v0.5.22-rocm720-mi30x",
		},
		{
			name: "an uppercase digest matches",
			image: "lmsysorg/sglang:v0.5.22-rocm720-mi30x@sha256:" + strings.ToUpper(
				inferenceXImage[strings.LastIndex(inferenceXImage, "@sha256:")+len("@sha256:"):],
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := inferenceXIdentity()
			role := workercore.ModelDeploymentRole{
				Name:         "server",
				Kind:         workercore.ModelDeploymentRoleKindServer,
				InstanceType: "mi325x-oam",
				Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
				ExtraArgs: append(inferenceXLaunchArguments(),
					"--dtype", "bf16", "--cpu-offload-gb", "0"),
			}
			deployment := inferenceXDeployment(tt.image)
			client := inferenceXClient(t, deployment)

			_, err := inferenceXRequest(t, client, deployment, &role,
				&priors.Profile{Identity: identity})
			require.NoError(t, err)
		})
	}
}

// TestModelDeploymentScalerPriorRequestRefusesAnotherEngineFamily asserts a
// deployment that runs another engine is refused.
//
// IT MATTERS BECAUSE A MEASUREMENT TAKEN WITH ONE ENGINE SAYS NOTHING ABOUT
// ANOTHER ONE.
func TestModelDeploymentScalerPriorRequestRefusesAnotherEngineFamily(t *testing.T) {
	identity := inferenceXIdentity()
	role := workercore.ModelDeploymentRole{
		Name:         "server",
		Kind:         workercore.ModelDeploymentRoleKindServer,
		InstanceType: "mi325x-oam",
		Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
		ExtraArgs:    inferenceXVLLMLaunchArguments(),
	}
	deployment := inferenceXDeployment("vllm/vllm-openai:v0.8.5")
	deployment.Spec.Engine.Name = workercore.ModelDeploymentEngineVLLM
	deployment.Spec.Roles[0].ExtraArgs = inferenceXVLLMLaunchArguments()
	client := inferenceXClient(t, deployment)

	_, err := inferenceXRequest(t, client, deployment, &role,
		&priors.Profile{Identity: identity})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "engine family")
}

// TestModelDeploymentScalerPriorRequestAcceptsAnIncompleteEnvironment asserts
// a profile whose environment the source did not observe completely is still
// matched.
//
// IT MATTERS BECAUSE A PUBLIC SOURCE RARELY CAPTURES THE WHOLE CONTAINER
// ENVIRONMENT, and a profile that could never be matched would make the
// catalog empty for every public source.
//
// IT STILL REFUSES A VARIABLE THE RECORD CARRIES AND THE DEPLOYMENT
// CONTRADICTS, because the variables the record does carry are the ones the
// measurement ran under.
func TestModelDeploymentScalerPriorRequestAcceptsAnIncompleteEnvironment(t *testing.T) {
	identity := inferenceXIdentity()
	role := workercore.ModelDeploymentRole{
		Name:         "server",
		Kind:         workercore.ModelDeploymentRoleKindServer,
		InstanceType: "mi325x-oam",
		Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
		Env:          []workercore.ModelDeploymentEnvVar{{Name: "EXTRA_VAR", Value: "1"}},
		ExtraArgs:    inferenceXLaunchArguments(),
	}
	deployment := inferenceXDeployment(inferenceXImage)
	client := inferenceXClient(t, deployment)
	profile := priors.Profile{Identity: identity}
	profile.ServingConfig = priors.ServingConfig{
		Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
		PrefixCaching: "disabled", SpeculativeDecoding: "none",
		EffectiveDefaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"},
	}

	_, err := inferenceXRequest(t, client, deployment, &role, &profile)
	require.NoError(t, err)
}

// TestModelDeploymentScalerPriorRequestRefusesAContradictedRecordedVariable
// asserts a variable the incomplete record carries is still compared.
//
// IT MATTERS BECAUSE THE VARIABLES THE RECORD CARRIES ARE THE ONES THE
// MEASUREMENT RAN UNDER, and a deployment that contradicts one of them runs a
// different engine.
func TestModelDeploymentScalerPriorRequestRefusesAContradictedRecordedVariable(t *testing.T) {
	identity := inferenceXIdentity()
	role := workercore.ModelDeploymentRole{
		Name:         "server",
		Kind:         workercore.ModelDeploymentRoleKindServer,
		InstanceType: "mi325x-oam",
		Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
		Env:          []workercore.ModelDeploymentEnvVar{{Name: "SGLANG_USE_AITER", Value: "2"}},
		ExtraArgs:    inferenceXLaunchArguments(),
	}
	deployment := inferenceXDeployment(inferenceXImage)
	client := inferenceXClient(t, deployment)
	profile := priors.Profile{Identity: identity}
	profile.ServingConfig = priors.ServingConfig{
		Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
		PrefixCaching: "disabled", SpeculativeDecoding: "none",
		EngineEnv:         []string{"SGLANG_USE_AITER=1"},
		EffectiveDefaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"},
	}

	_, err := inferenceXRequest(t, client, deployment, &role, &profile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SGLANG_USE_AITER")
}

// TestModelDeploymentScalerPriorRequestMalformedEnvironment refuses a recorded
// environment entry that names no variable.
//
// IT MATTERS BECAUSE THE ENTRY IS READ BY A REFUSAL THAT REACHES THE STATUS,
// and an entry carries the value of the variable.
func TestModelDeploymentScalerPriorRequestMalformedEnvironment(t *testing.T) {
	identity := inferenceXIdentity()
	role := workercore.ModelDeploymentRole{
		Name:         "server",
		Kind:         workercore.ModelDeploymentRoleKindServer,
		InstanceType: "mi325x-oam",
		Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
		ExtraArgs:    inferenceXLaunchArguments(),
	}
	deployment := inferenceXDeployment(inferenceXImage)
	client := inferenceXClient(t, deployment)
	profile := priors.Profile{Identity: identity}
	profile.ServingConfig = priors.ServingConfig{
		Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
		PrefixCaching: "disabled", SpeculativeDecoding: "none",
		EngineEnv:         []string{"=review-only-secret-marker"},
		EngineEnvComplete: true,
		EffectiveDefaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"},
	}

	_, err := inferenceXRequest(t, client, deployment, &role, &profile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed engine environment entry")
	assert.NotContains(t, err.Error(), "review-only-secret-marker")
}

// TestModelDeploymentScalerPriorRequestRedactsTheEnvironmentValue holds the
// refusal against the reader that consumed the environment.
//
// IT MATTERS BECAUSE THE PARALLELISM READER REPORTS THE VALUE IT READ, and the
// refusal reaches the conditions of the scaler, where a value the user wrote
// belongs to nobody else.
func TestModelDeploymentScalerPriorRequestRedactsTheEnvironmentValue(t *testing.T) {
	identity := inferenceXIdentity()
	role := workercore.ModelDeploymentRole{
		Name:         "server",
		Kind:         workercore.ModelDeploymentRoleKindServer,
		InstanceType: "mi325x-oam",
		Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
		Env:          []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "review-only-marker"}},
		ExtraArgs:    inferenceXVLLMLaunchArguments(),
	}
	deployment := inferenceXDeployment(inferenceXImage)
	deployment.Spec.Engine.Name = workercore.ModelDeploymentEngineVLLM
	deployment.Spec.Roles[0].ExtraArgs = inferenceXVLLMLaunchArguments()
	client := inferenceXClient(t, deployment)
	profile := priors.Profile{Identity: identity}
	profile.ServingConfig = priors.ServingConfig{
		Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
		PrefixCaching: "disabled", SpeculativeDecoding: "none",
		EngineEnv:         []string{"VLLM_DP_SIZE=review-only-marker"},
		EngineEnvComplete: true,
		EffectiveDefaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"},
	}

	_, err := inferenceXRequest(t, client, deployment, &role, &profile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VLLM_DP_SIZE")
	assert.NotContains(t, err.Error(), "review-only-marker")
}

// TestModelDeploymentScalerPriorRequestOmittedDefaultsValue holds the omitted
// defaults gate against the values it records.
//
// IT MATTERS BECAUSE A DEFAULT THAT CONTRADICTS THE SERVING CONFIGURATION
// DESCRIBES AN ENGINE THAT WAS NEVER MEASURED, and a deployment that relied
// on it would be scaled by a capacity that came from somewhere else.
//
// IT ACCEPTS A DEFAULT THE CATALOG DOES NOT RECORD, because a flag the
// deployment leaves out runs at the engine default and a catalog that says
// nothing about a default it did not record cannot refuse it.
func TestModelDeploymentScalerPriorRequestOmittedDefaultsValue(t *testing.T) {
	cases := []struct {
		name     string
		defaults map[string]string
		wantIn   string
	}{
		{
			name:     "an empty default is accepted",
			defaults: map[string]string{"--dtype": "", "--cpu-offload-gb": "0"},
		},
		{
			name:     "a missing default is accepted",
			defaults: map[string]string{"--dtype": "bf16"},
		},
		{
			name:     "a default that is not a whole number is refused",
			defaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "many"},
			wantIn:   "not a whole number",
		},
		{
			name:     "an offloaded default is refused while the offload was none",
			defaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "64"},
			wantIn:   "offloads the KV cache",
		},
		{
			name:     "a dtype the engine does not accept is refused",
			defaults: map[string]string{"--dtype": "not-a-dtype", "--cpu-offload-gb": "0"},
			wantIn:   "does not accept",
		},
		{
			name:     "a usable default is accepted",
			defaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			identity := inferenceXIdentity()
			role := workercore.ModelDeploymentRole{
				Name:         "server",
				Kind:         workercore.ModelDeploymentRoleKindServer,
				InstanceType: "mi325x-oam",
				Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
				ExtraArgs:    inferenceXLaunchArguments(),
			}
			deployment := inferenceXDeployment(inferenceXImage)
			client := inferenceXClient(t, deployment)
			profile := priors.Profile{Identity: identity}
			profile.ServingConfig = priors.ServingConfig{
				Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
				PrefixCaching: "disabled", SpeculativeDecoding: "none",
				EngineEnvComplete: true, EffectiveDefaults: testCase.defaults,
			}

			_, err := inferenceXRequest(t, client, deployment, &role, &profile)
			if testCase.wantIn == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.wantIn)
		})
	}
}

// TestModelDeploymentScalerPriorRequestAcceptsAMountedVolume holds the prior
// against a role that mounts a volume.
//
// IT MATTERS BECAUSE A DEPLOYMENT MOUNTS THE WEIGHTS, THE CACHES AND THE LOGS
// OF ITS OWN ROLE, and a prior that refused every mounted volume would never
// resolve for a deployment that stores its weights.
func TestModelDeploymentScalerPriorRequestAcceptsAMountedVolume(t *testing.T) {
	identity := inferenceXIdentity()
	role := workercore.ModelDeploymentRole{
		Name:         "server",
		Kind:         workercore.ModelDeploymentRoleKindServer,
		InstanceType: "mi325x-oam",
		Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
		ExtraArgs:    inferenceXLaunchArguments(),
		AdditionalVolumes: []workercore.ModelDeploymentAdditionalVolume{{
			MountPath: "/var/lib/gpustack/weights",
			ReadOnly:  true,
			ConfigMap: &core.LocalObjectReference{Name: "deployment-weights"},
		}},
	}
	deployment := inferenceXDeployment(inferenceXImage)
	client := inferenceXClient(t, deployment)
	profile := priors.Profile{Identity: identity}
	profile.ServingConfig = priors.ServingConfig{
		Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
		PrefixCaching: "disabled", SpeculativeDecoding: "none",
		EngineEnvComplete: true,
		EffectiveDefaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"},
	}

	_, err := inferenceXRequest(t, client, deployment, &role, &profile)
	require.NoError(t, err)
}

// TestModelDeploymentScalerPriorRequestRedactsANegativeNumber holds the
// refusal against a value that starts with a minus sign.
//
// IT MATTERS BECAUSE A WORD BOUNDARY EXISTS NEXT TO A WORD CHARACTER, and a
// value that starts with a minus sign has none at its front, so a redactor
// that looks for a word boundary lets the value through.
func TestModelDeploymentScalerPriorRequestRedactsANegativeNumber(t *testing.T) {
	identity := inferenceXIdentity()
	role := workercore.ModelDeploymentRole{
		Name:         "server",
		Kind:         workercore.ModelDeploymentRoleKindServer,
		InstanceType: "mi325x-oam",
		Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
		Env:          []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "-123456789"}},
		ExtraArgs:    inferenceXVLLMLaunchArguments(),
	}
	deployment := inferenceXDeployment(inferenceXImage)
	deployment.Spec.Engine.Name = workercore.ModelDeploymentEngineVLLM
	deployment.Spec.Roles[0].ExtraArgs = inferenceXVLLMLaunchArguments()
	client := inferenceXClient(t, deployment)
	profile := priors.Profile{Identity: identity}
	profile.ServingConfig = priors.ServingConfig{
		Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
		PrefixCaching: "disabled", SpeculativeDecoding: "none",
		EngineEnv:         []string{"VLLM_DP_SIZE=-123456789"},
		EngineEnvComplete: true,
		EffectiveDefaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"},
	}

	_, err := inferenceXRequest(t, client, deployment, &role, &profile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VLLM_DP_SIZE")
	assert.NotContains(t, err.Error(), "-123456789")
}

// TestModelDeploymentScalerOperatorEnvGate holds the operator against the
// environment variables it writes into the engine.
//
// IT MATTERS BECAUSE THE DEPLOYMENT SIDE READS THE ROLE'S OWN ENTRIES, and the
// operator writes more of them, and a variable the operator writes and the
// measurement did not run under is a difference between the two engine
// configurations.
//
// IT CHECKS THE ACTUAL RENDER AND NOT ONLY THE DECLARED TABLE, because the
// render is the document the engine runs with, and a name the renderer starts
// writing reaches the container whether the table carries it or not.
func TestModelDeploymentScalerOperatorEnvGate(t *testing.T) {
	deployment := inferenceXDeployment(inferenceXImage)
	role := &deployment.Spec.Roles[0]
	require.NoError(t, modelDeploymentScalerOperatorEnvGate(
		workercore.ModelDeploymentEngineSGLang, deployment, role,
	), "the operator writes only declared variables")

	deployment.Spec.Model.ArtifactRef = nil
	deployment.Spec.Engine.Name = workercore.ModelDeploymentEngineVLLM
	require.NoError(t, modelDeploymentScalerOperatorEnvGate(
		workercore.ModelDeploymentEngineVLLM, deployment, role,
	), "the operator writes only declared variables")

	err := modelDeploymentScalerAccountForInjectedEnv([]string{"VLLM_A_VARIABLE_THE_OPERATOR_STARTS_WRITING"})
	require.Error(t, err, "a variable the operator has not declared neutral is refused")
	require.Contains(t, err.Error(), "VLLM_A_VARIABLE_THE_OPERATOR_STARTS_WRITING",
		"the refusal names the variable")
}

// TestModelDeploymentScalerOperatorEnvGateRefusesTheRenderedName holds the
// gate against a name the renderer writes and the declared table does not
// carry.
//
// IT MATTERS BECAUSE A TABLE THE OPERATOR WRITES BY HAND SAYS NOTHING ABOUT THE
// RENDER THAT RUNS, and a name that reaches the container without a
// declaration is a variable the measurement did not run under.
func TestModelDeploymentScalerOperatorEnvGateRefusesTheRenderedName(t *testing.T) {
	original := modelDeploymentRuntimeCaches
	defer func() { modelDeploymentRuntimeCaches = original }()
	modelDeploymentRuntimeCaches = append(
		append([]modelDeploymentRuntimeCache(nil), original...),
		modelDeploymentRuntimeCache{Name: "AN_ENVIRONMENT_THE_RENDER_STARTS_WRITING", SubDir: "review"},
	)

	deployment := inferenceXDeployment(inferenceXImage)
	err := modelDeploymentScalerOperatorEnvGate(
		workercore.ModelDeploymentEngineSGLang, deployment, &deployment.Spec.Roles[0],
	)
	require.Error(t, err, "a variable the render writes and the table does not declare is refused")
	require.Contains(t, err.Error(), "AN_ENVIRONMENT_THE_RENDER_STARTS_WRITING",
		"the refusal names the variable")
}

// TestModelDeploymentScalerPriorAcceptsTheIdentityForms asserts the model and
// the hardware identity are accepted as a name or as a digest.
//
// IT MATTERS BECAUSE A DEPLOYMENT MAY NAME THE IDENTITY OR PIN IT, and a form
// this operator cannot read is a form it cannot check.
func TestModelDeploymentScalerPriorAcceptsTheIdentityForms(t *testing.T) {
	identity := inferenceXIdentity()
	modelDigest, err := priors.Digest(identity.Model)
	if err != nil {
		t.Fatalf("digest the model identity: %v", err)
	}
	hardwareDigest, err := priors.Digest(identity.Hardware)
	if err != nil {
		t.Fatalf("digest the hardware identity: %v", err)
	}

	cases := []struct {
		name       string
		modelID    string
		hardwareID string
		wantErr    string
	}{
		{
			name:       "the digests of the identities",
			modelID:    "model:" + modelDigest,
			hardwareID: "hardware:" + hardwareDigest,
		},
		{
			name:       "the names of the identities",
			modelID:    inferenceXModel,
			hardwareID: "mi325x",
		},
		{
			name:       "the bare name of the model and the product line of the accelerator",
			modelID:    "DeepSeek-R1-0528",
			hardwareID: "AMD Instinct MI325X",
		},
		{
			name: "no annotation at all",
		},
		{
			name:    "another model is refused",
			modelID: "another/model",
			wantErr: "model-id",
		},
		{
			name:       "another hardware is refused",
			hardwareID: "h100",
			wantErr:    "hardware-id",
		},
		{
			name:    "another digest is refused",
			modelID: "model:" + strings.Repeat("0", 64),
			wantErr: "model-id",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			role := workercore.ModelDeploymentRole{
				Name:         "server",
				Kind:         workercore.ModelDeploymentRoleKindServer,
				InstanceType: "mi325x-oam",
				Resources:    &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("8")},
				ExtraArgs:    inferenceXLaunchArguments(),
			}
			deployment := inferenceXDeployment(inferenceXImage)
			deployment.Annotations[workercore.ModelDeploymentScalerModelIDAnnotationKey] = testCase.modelID
			deployment.Annotations[workercore.ModelDeploymentScalerHardwareIDAnnotationKey] = testCase.hardwareID
			client := inferenceXClient(t, deployment)
			profile := priors.Profile{Identity: identity}
			profile.ServingConfig = priors.ServingConfig{
				Router: "none", KVCacheDType: "fp8_e4m3", Offload: "none",
				PrefixCaching: "disabled", SpeculativeDecoding: "none",
				EffectiveDefaults: map[string]string{"--dtype": "bf16", "--cpu-offload-gb": "0"},
			}

			_, err := inferenceXRequest(t, client, deployment, &role, &profile)
			if testCase.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.wantErr)
		})
	}
}
