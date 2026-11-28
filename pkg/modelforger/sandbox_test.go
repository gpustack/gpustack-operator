package modelforger

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelkernelkey"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/json"
)

func sampleForger() *workercore.ModelDeploymentForger {
	return &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{
			Name:      "qwen-decode-forger",
			Namespace: "team-a",
			UID:       types.UID("forger-uid-123"),
		},
	}
}

func sampleNodeHardware() modelkernelkey.NodeHardware {
	return modelkernelkey.NodeHardware{
		Key:          "nvidia-h100",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Arch:         "sm_90a",
		SelectorLabels: map[string]string{
			"feature.node.kubernetes.io/gpustack-acceleratable-nvidia-h100":        "true",
			"feature.node.kubernetes.io/gpustack-acceleratable-nvidia-h100.comcap": "9.0a",
		},
	}
}

func assertRestrictedPSACompliant(t *testing.T, pod *core.Pod) {
	t.Helper()

	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	require.False(t, *pod.Spec.AutomountServiceAccountToken)
	require.Equal(t, core.RestartPolicyNever, pod.Spec.RestartPolicy)
	require.NotNil(t, pod.Spec.SecurityContext)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	require.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsUser)
	require.Equal(t, int64(65532), *pod.Spec.SecurityContext.RunAsUser)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsGroup)
	require.Equal(t, int64(65532), *pod.Spec.SecurityContext.RunAsGroup)
	require.NotNil(t, pod.Spec.SecurityContext.FSGroup)
	require.Equal(t, int64(65532), *pod.Spec.SecurityContext.FSGroup)
	require.NotNil(t, pod.Spec.SecurityContext.SeccompProfile)
	require.Equal(t, core.SeccompProfileTypeRuntimeDefault, pod.Spec.SecurityContext.SeccompProfile.Type)

	for _, vol := range pod.Spec.Volumes {
		require.Nil(t, vol.HostPath, "ForgeSandbox must never mount hostPath volume %q", vol.Name)
	}

	allContainers := append([]core.Container{}, pod.Spec.InitContainers...)
	allContainers = append(allContainers, pod.Spec.Containers...)
	for _, c := range allContainers {
		require.NotNil(t, c.SecurityContext, "container %q missing SecurityContext", c.Name)
		require.NotNil(t, c.SecurityContext.AllowPrivilegeEscalation)
		require.False(t, *c.SecurityContext.AllowPrivilegeEscalation)
		require.NotNil(t, c.SecurityContext.RunAsNonRoot)
		require.True(t, *c.SecurityContext.RunAsNonRoot)
		require.NotNil(t, c.SecurityContext.RunAsUser)
		require.Equal(t, int64(65532), *c.SecurityContext.RunAsUser)
		require.NotNil(t, c.SecurityContext.RunAsGroup)
		require.Equal(t, int64(65532), *c.SecurityContext.RunAsGroup)
		require.NotNil(t, c.SecurityContext.Capabilities)
		require.Equal(t, []core.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop)
		require.NotNil(t, c.SecurityContext.SeccompProfile)
		require.Equal(t, core.SeccompProfileTypeRuntimeDefault, c.SecurityContext.SeccompProfile.Type)
	}
}

func TestBuildInputConfigMap_SuccessAndSizeLimit(t *testing.T) {
	t.Parallel()

	forger := sampleForger()
	validKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRMSNorm)
	hidden := int64(4096)
	def := Definition{
		Name:           "rmsnorm_hidden4096",
		OpType:         modelkernelkey.OpFamilyRMSNorm,
		LocalKernelKey: validKey,
		Axes: map[string]AxisSpec{
			"hidden_size": {Type: "const", Value: &hidden},
			"num_tokens":  {Type: "var"},
		},
		Inputs: map[string]TensorSpec{
			"x": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
		},
		Outputs: map[string]TensorSpec{
			"out": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"},
		},
		Reference: "def run(x): return x",
	}
	wl := Workload{
		UUID:               "wl-1",
		Definition:         def.Name,
		LocalKernelKey:     validKey,
		Axes:               map[string]int64{"num_tokens": 128},
		Inputs:             map[string]WorkloadInput{"x": {Type: "random"}},
		ObservedDurationUs: 88.5,
		CallCount:          4,
	}
	req := ForgeRequest{
		Engine:           "vLLM",
		EngineVersion:    "0.8.5",
		Manufacturer:     "nvidia",
		Architecture:     "sm_90a",
		TargetKernelKeys: []string{validKey},
		MinSpeedupRatio:  1.05,
		Atol:             1e-2,
		Rtol:             1e-2,
	}

	cm, items, err := BuildInputConfigMap(InputConfigMapSpec{
		Forger:      forger,
		Name:        "qwen-decode-forger-input-gen-1",
		RoleName:    "decode",
		Generation:  1,
		Request:     req,
		Definitions: []Definition{def},
		Workloads:   []Workload{wl},
	})
	require.NoError(t, err)
	require.Equal(t, "team-a", cm.Namespace)
	require.True(t, *cm.Immutable)
	require.Len(t, cm.OwnerReferences, 1)
	require.Equal(t, forger.UID, cm.OwnerReferences[0].UID)
	require.True(t, *cm.OwnerReferences[0].Controller)

	// Verify flat ConfigMap keys and nested KeyToPath projections.
	for k := range cm.Data {
		require.NotContains(t, k, "/")
	}
	require.Equal(t, ConfigMapVolumeItems(cm), items)

	paths := make(map[string]string, len(items))
	for _, it := range items {
		paths[it.Path] = it.Key
	}
	require.Contains(t, paths, ForgeRequestFileName)
	require.Contains(t, paths, "definitions/rmsnorm_hidden4096.json")
	require.Contains(t, paths, "workloads/rmsnorm_hidden4096.jsonl")

	// Reject ConfigMap exceeding 512 KiB.
	hugeDef := def
	hugeDef.Description = strings.Repeat("x", MaxInputConfigMapBytes)
	_, _, err = BuildInputConfigMap(InputConfigMapSpec{
		Forger:      forger,
		Name:        "too-big",
		RoleName:    "decode",
		Generation:  1,
		Request:     req,
		Definitions: []Definition{hugeDef},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds")
}

func TestBuildInputConfigMap_TrimsLowestDurationWorkloadsBeforeFailingSizeCap(t *testing.T) {
	t.Parallel()

	forger := sampleForger()
	rmsKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRMSNorm)
	lnKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyLayerNorm)
	hidden := int64(4096)

	defA := Definition{
		Name:           "rmsnorm_hidden4096",
		OpType:         modelkernelkey.OpFamilyRMSNorm,
		LocalKernelKey: rmsKey,
		Axes: map[string]AxisSpec{
			"hidden_size": {Type: "const", Value: &hidden},
			"num_tokens":  {Type: "var"},
		},
		Inputs:    map[string]TensorSpec{"x": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"}},
		Outputs:   map[string]TensorSpec{"out": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"}},
		Reference: "def run(x): return x",
	}
	defB := Definition{
		Name:           "layernorm_hidden4096",
		OpType:         modelkernelkey.OpFamilyLayerNorm,
		LocalKernelKey: lnKey,
		Axes: map[string]AxisSpec{
			"hidden_size": {Type: "const", Value: &hidden},
			"num_tokens":  {Type: "var"},
		},
		Inputs:    map[string]TensorSpec{"x": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"}},
		Outputs:   map[string]TensorSpec{"out": {Shape: []string{"num_tokens", "hidden_size"}, Dtype: "bfloat16"}},
		Reference: "def run(x): return x",
	}
	req := ForgeRequest{
		Engine:           "vLLM",
		EngineVersion:    "0.8.5",
		Manufacturer:     "nvidia",
		Architecture:     "sm_90a",
		TargetKernelKeys: []string{rmsKey, lnKey},
		MinSpeedupRatio:  1.05,
		Atol:             1e-2,
		Rtol:             1e-2,
	}

	padding := strings.Repeat("p", 80<<10)
	durations := []float64{90.0, 20.0, 50.0, 50.0, 10.0, 80.0, 70.0}
	uuids := []string{"wl-b-90", "wl-b-20", "wl-b-50-a", "wl-b-50-b", "wl-b-10", "wl-b-80", "wl-b-70"}
	workloads := make([]Workload, 0, 1+len(durations))
	workloads = append(workloads, Workload{
		UUID:               "wl-defA-sole",
		Definition:         defA.Name,
		LocalKernelKey:     rmsKey,
		Axes:               map[string]int64{"num_tokens": 16},
		Inputs:             map[string]WorkloadInput{"x": {Type: "random", Value: padding}},
		ObservedDurationUs: 1.0, // lowest globally, but must be preserved as sole workload of defA
		CallCount:          1,
	})
	for i := range durations {
		workloads = append(workloads, Workload{
			UUID:               uuids[i],
			Definition:         defB.Name,
			LocalKernelKey:     lnKey,
			Axes:               map[string]int64{"num_tokens": int64(32 + i)},
			Inputs:             map[string]WorkloadInput{"x": {Type: "random", Value: padding}},
			ObservedDurationUs: durations[i],
			CallCount:          2,
		})
	}

	spec := InputConfigMapSpec{
		Forger:      forger,
		Name:        "near-cap-input",
		RoleName:    "decode",
		Generation:  1,
		Request:     req,
		Definitions: []Definition{defA, defB},
		Workloads:   workloads,
	}
	cm1, _, err := BuildInputConfigMap(spec)
	require.NoError(t, err)
	cm2, _, err := BuildInputConfigMap(spec)
	require.NoError(t, err)
	require.Equal(t, cm1.Data, cm2.Data, "workload trimming must be deterministic")

	totalBytes := 0
	for k, v := range cm1.Data {
		totalBytes += len(k) + len(v)
	}
	require.LessOrEqual(t, totalBytes, MaxInputConfigMapBytes)

	// defA's sole workload must be preserved despite having the lowest duration overall.
	require.Contains(t, cm1.Data["wl-rmsnorm_hidden4096.jsonl"], "wl-defA-sole")

	// defB must keep its top-duration workloads and drop the lowest-duration ones.
	defBJSONL := cm1.Data["wl-layernorm_hidden4096.jsonl"]
	require.Contains(t, defBJSONL, "wl-b-90")
	require.Contains(t, defBJSONL, "wl-b-80")
	require.Contains(t, defBJSONL, "wl-b-70")
	require.NotContains(t, defBJSONL, "wl-b-10")
	require.NotContains(t, defBJSONL, "wl-b-20")

	var encodedReq ForgeRequest
	require.NoError(t, json.Unmarshal([]byte(cm1.Data[ForgeRequestFileName]), &encodedReq))
	require.Positive(t, encodedReq.EvictedWorkloads, "forge-request.json must record evictedWorkloads count")

	// Irreducible single workload exceeding 512 KiB still fails with size-cap error.
	hugePadding := strings.Repeat("z", MaxInputConfigMapBytes)
	_, _, err = BuildInputConfigMap(InputConfigMapSpec{
		Forger:      forger,
		Name:        "irreducible-input",
		RoleName:    "decode",
		Generation:  1,
		Request:     req,
		Definitions: []Definition{defA},
		Workloads: []Workload{
			{
				UUID:               "wl-huge-sole",
				Definition:         defA.Name,
				LocalKernelKey:     rmsKey,
				Axes:               map[string]int64{"num_tokens": 16},
				Inputs:             map[string]WorkloadInput{"x": {Type: "random", Value: hugePadding}},
				ObservedDurationUs: 10.0,
			},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds")
}

func TestBuildSandboxPod_MockAndExternalModes(t *testing.T) {
	t.Parallel()

	forger := sampleForger()
	hw := sampleNodeHardware()
	cpuQty := resource.MustParse("4")
	ramQty := resource.MustParse("16Gi")
	outLimit := resource.MustParse("4Gi")
	thresholds := VerificationThresholds{MinSpeedupRatio: 1.10, Atol: 1e-3, Rtol: 2e-3}
	accelRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)

	t.Run("Mock mode builds restricted 1-GPU pipeline with operator helpers", func(t *testing.T) {
		t.Parallel()

		pod, err := BuildSandboxPod(SandboxPodSpec{
			Forger:             forger,
			Name:               "qwen-decode-forger-sb-1",
			RoleName:           "decode",
			Generation:         1,
			InputConfigMapName: "qwen-decode-forger-input-1",
			EntranceQueue:      "nvidia-h100-lq",
			Hardware:           hw,
			OperatorImage:      "gpustack/operator:v1.0.0",
			SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
				RunnerMode:            workercore.ModelDeploymentForgerRunnerModeMock,
				PriorityClassName:     "forge-low",
				ActiveDeadlineSeconds: 900,
				OutputSizeLimit:       &outLimit,
				Resources: &workercore.ModelDeploymentForgerSandboxResources{
					CPU: &cpuQty,
					RAM: &ramQty,
				},
			},
			Thresholds:      &thresholds,
			HoldMaxDuration: 5 * time.Minute,
		})
		require.NoError(t, err)
		assertRestrictedPSACompliant(t, pod)

		require.Equal(t, "nvidia-h100-lq", pod.Labels[kueuectrlconst.QueueLabel])
		require.Equal(t, "forge-low", pod.Spec.PriorityClassName)
		require.Equal(t, int64(900), *pod.Spec.ActiveDeadlineSeconds)
		require.Equal(t, hw.SelectorLabels, pod.Spec.NodeSelector)
		require.Len(t, pod.Spec.InitContainers, 2)
		require.Equal(t, RunnerContainerName, pod.Spec.InitContainers[0].Name)
		require.Equal(t, []string{"model-forger", "mock", "--input-dir", InputMountPath, "--output-dir", OutputMountPath}, pod.Spec.InitContainers[0].Args)
		require.Equal(t, VerifierContainerName, pod.Spec.InitContainers[1].Name)
		require.Equal(t, []string{"model-forger", "verify"}, pod.Spec.InitContainers[1].Args[:2])
		require.Len(t, pod.Spec.Containers, 1)
		require.Equal(t, HoldContainerName, pod.Spec.Containers[0].Name)
		require.Equal(t, []string{"model-forger", "hold", "--max-duration", "5m0s"}, pod.Spec.Containers[0].Args)

		// Exactly 1 whole accelerator card on runner and verifier, 0 on hold container.
		one := resource.MustParse("1")
		require.True(t, one.Equal(pod.Spec.InitContainers[0].Resources.Limits[accelRes]))
		require.True(t, one.Equal(pod.Spec.InitContainers[1].Resources.Limits[accelRes]))
		require.Empty(t, pod.Spec.Containers[0].Resources.Limits)

		// forge-verifier mounts forge-output read-only.
		var verifierOutMount *core.VolumeMount
		for i := range pod.Spec.InitContainers[1].VolumeMounts {
			vm := &pod.Spec.InitContainers[1].VolumeMounts[i]
			if vm.Name == OutputVolumeName {
				verifierOutMount = vm
			}
		}
		require.NotNil(t, verifierOutMount)
		require.True(t, verifierOutMount.ReadOnly)

		// Verify emptyDir size limits.
		volByName := make(map[string]core.Volume, len(pod.Spec.Volumes))
		for _, v := range pod.Spec.Volumes {
			volByName[v.Name] = v
		}
		require.True(t, outLimit.Equal(*volByName[OutputVolumeName].EmptyDir.SizeLimit))
		require.True(t, verifiedSizeLimitQty.Equal(*volByName[VerifiedVolumeName].EmptyDir.SizeLimit))
	})

	t.Run("External mode stages engine-sentinel via emptyDir without hostPath", func(t *testing.T) {
		t.Parallel()

		runnerSpec := &workercore.ModelDeploymentForgerSandboxRunner{
			Image:           "registry.local/forge-runner:v0.2.0",
			ImagePullPolicy: core.PullAlways,
			Command:         []string{"loopwright-forger"},
			Args:            []string{"--budget", "4"},
			Env: []workercore.ModelDeploymentForgerSandboxEnvVar{
				{Name: "LOOPWRIGHT_MODEL", Value: "gpt-5"},
				{
					Name: "LOOPWRIGHT_CONFIG_KEY",
					ValueFrom: &workercore.ModelDeploymentForgerSandboxEnvVarSource{
						ConfigMapKeyRef: &core.ConfigMapKeySelector{
							LocalObjectReference: core.LocalObjectReference{Name: "runner-cm"},
							Key:                  "endpoint",
						},
					},
				},
				{
					Name: "LOOPWRIGHT_TOKEN",
					ValueFrom: &workercore.ModelDeploymentForgerSandboxEnvVarSource{
						SecretKeyRef: &core.SecretKeySelector{
							LocalObjectReference: core.LocalObjectReference{Name: "runner-secret"},
							Key:                  "api-key",
						},
					},
				},
				{Name: "GPUSTACK_FORGE_INPUT_DIR", Value: "/evil/input"},
				{Name: "gpustack_forge_custom", Value: "filtered"},
			},
			EnvFrom: []core.EnvFromSource{
				{
					Prefix: "CFG_",
					ConfigMapRef: &core.ConfigMapEnvSource{
						LocalObjectReference: core.LocalObjectReference{Name: "runner-env-cm"},
					},
				},
				{
					SecretRef: &core.SecretEnvSource{
						LocalObjectReference: core.LocalObjectReference{Name: "runner-env-sec"},
					},
				},
			},
		}

		pod, err := BuildSandboxPod(SandboxPodSpec{
			Forger:             forger,
			Name:               "qwen-decode-forger-sb-ext-2",
			RoleName:           "decode",
			Generation:         2,
			InputConfigMapName: "qwen-decode-forger-input-2",
			EntranceQueue:      "nvidia-h100-lq",
			Hardware:           hw,
			OperatorImage:      "gpustack/operator:v1.0.0",
			EngineImage:        "vllm/vllm-openai:v0.8.5",
			ImagePullSecrets: []core.LocalObjectReference{
				{Name: "engine-regcred"},
				{Name: "shared-regcred"},
			},
			SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
				RunnerMode: workercore.ModelDeploymentForgerRunnerModeExternal,
				Runner:     runnerSpec,
				ImagePullSecrets: []core.LocalObjectReference{
					{Name: "shared-regcred"},
					{Name: "runner-regcred"},
				},
			},
		})
		require.NoError(t, err)
		assertRestrictedPSACompliant(t, pod)

		require.Equal(t, DefaultSandboxTimeoutSeconds, *pod.Spec.ActiveDeadlineSeconds)
		require.Equal(t, []core.LocalObjectReference{
			{Name: "engine-regcred"},
			{Name: "shared-regcred"},
			{Name: "runner-regcred"},
		}, pod.Spec.ImagePullSecrets)

		require.Len(t, pod.Spec.InitContainers, 3)
		runnerCtr := pod.Spec.InitContainers[0]
		stageCtr := pod.Spec.InitContainers[1]
		verifierCtr := pod.Spec.InitContainers[2]
		holdCtr := pod.Spec.Containers[0]

		require.Equal(t, RunnerContainerName, runnerCtr.Name)
		require.Equal(t, "registry.local/forge-runner:v0.2.0", runnerCtr.Image)
		require.Equal(t, core.PullAlways, runnerCtr.ImagePullPolicy)
		require.Equal(t, []string{"loopwright-forger"}, runnerCtr.Command)
		require.Equal(t, []string{"--budget", "4"}, runnerCtr.Args)
		require.Len(t, runnerCtr.EnvFrom, 2)
		require.Equal(t, "runner-env-cm", runnerCtr.EnvFrom[0].ConfigMapRef.Name)
		require.Equal(t, "CFG_", runnerCtr.EnvFrom[0].Prefix)
		require.Equal(t, "runner-env-sec", runnerCtr.EnvFrom[1].SecretRef.Name)

		require.Equal(t, []core.EnvVar{
			{Name: envForgeInputDir, Value: InputMountPath},
			{Name: envForgeOutputDir, Value: OutputMountPath},
			{Name: "LOOPWRIGHT_MODEL", Value: "gpt-5"},
			{
				Name: "LOOPWRIGHT_CONFIG_KEY",
				ValueFrom: &core.EnvVarSource{
					ConfigMapKeyRef: &core.ConfigMapKeySelector{
						LocalObjectReference: core.LocalObjectReference{Name: "runner-cm"},
						Key:                  "endpoint",
					},
				},
			},
			{
				Name: "LOOPWRIGHT_TOKEN",
				ValueFrom: &core.EnvVarSource{
					SecretKeyRef: &core.SecretKeySelector{
						LocalObjectReference: core.LocalObjectReference{Name: "runner-secret"},
						Key:                  "api-key",
					},
				},
			},
		}, runnerCtr.Env)

		// Mutating input slices after BuildSandboxPod must not mutate rendered container fields.
		runnerSpec.Command[0] = "mutated"
		runnerSpec.Args[0] = "mutated"
		runnerSpec.EnvFrom[0].ConfigMapRef.Name = "mutated"
		require.Equal(t, "loopwright-forger", runnerCtr.Command[0])
		require.Equal(t, "--budget", runnerCtr.Args[0])
		require.Equal(t, "runner-env-cm", runnerCtr.EnvFrom[0].ConfigMapRef.Name)

		// Stage-sentinel, verifier, and hold containers must never receive runner overrides.
		require.Equal(t, StageSentinelContainerName, stageCtr.Name)
		require.Empty(t, stageCtr.Env)
		require.Empty(t, stageCtr.EnvFrom)
		require.Empty(t, stageCtr.Args)
		require.Empty(t, stageCtr.ImagePullPolicy)

		require.Equal(t, VerifierContainerName, verifierCtr.Name)
		require.Equal(t, "vllm/vllm-openai:v0.8.5", verifierCtr.Image)
		require.Equal(t, []string{"python3", "-m", "gpustack_engine_sentinel.forge_verifier"}, verifierCtr.Command)
		require.NotContains(t, verifierCtr.Args, "--budget")
		require.Empty(t, verifierCtr.EnvFrom)
		require.Empty(t, verifierCtr.ImagePullPolicy)
		for _, ev := range verifierCtr.Env {
			require.NotContains(t, []string{"LOOPWRIGHT_MODEL", "LOOPWRIGHT_CONFIG_KEY", "LOOPWRIGHT_TOKEN"}, ev.Name)
		}

		require.Equal(t, HoldContainerName, holdCtr.Name)
		require.Empty(t, holdCtr.Env)
		require.Empty(t, holdCtr.EnvFrom)
		require.NotContains(t, holdCtr.Args, "--budget")
		require.Empty(t, holdCtr.ImagePullPolicy)

		// Default output emptyDir size limit is 2Gi.
		var outVol *core.Volume
		for i := range pod.Spec.Volumes {
			if pod.Spec.Volumes[i].Name == OutputVolumeName {
				outVol = &pod.Spec.Volumes[i]
			}
		}
		require.NotNil(t, outVol)
		require.True(t, defaultOutputSizeLimitQty.Equal(*outVol.EmptyDir.SizeLimit))
	})

	t.Run("Accelerator Exclusive, Sliced, Partitioned, and LocalStorage resources", func(t *testing.T) {
		t.Parallel()

		localStorageQty := resource.MustParse("20Gi")
		sliceableType := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "nvidia-h100-sliceable"},
			Status: workercore.InstanceTypeStatus{
				Detail: workercore.InstanceTypeDetail{
					Manufacturer: nodefeature.ManufacturerNVIDIA,
					InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
						SlicedDetail: workercore.AcceleratorSlicedDetail{
							Logical: workercore.AcceleratorSlicedLogicalDetail{Count: 4},
						},
					},
				},
			},
		}
		nonSliceableType := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "nvidia-h100-exclusive"},
			Status: workercore.InstanceTypeStatus{
				Detail: workercore.InstanceTypeDetail{
					Manufacturer: nodefeature.ManufacturerNVIDIA,
				},
			},
		}

		// Sliced mode + LocalStorage
		slicedPod, err := BuildSandboxPod(SandboxPodSpec{
			Forger:             forger,
			Name:               "sb-sliced",
			RoleName:           "decode",
			Generation:         1,
			InputConfigMapName: "cm-1",
			EntranceQueue:      "nvidia-h100-lq",
			AcceleratorType:    sliceableType,
			Hardware:           hw,
			OperatorImage:      "gpustack/operator:v1.0.0",
			SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
				RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock,
				Resources: &workercore.ModelDeploymentForgerSandboxResources{
					CPU:                               &cpuQty,
					RAM:                               &ramQty,
					LocalStorage:                      &localStorageQty,
					AcceleratorSlicedMemoryPercentage: 50,
					AcceleratorSlicedCoresPercentage:  25,
				},
			},
		})
		require.NoError(t, err)

		slicedCardRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)
		slicedMemRes := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(nodefeature.ManufacturerNVIDIA)
		slicedCoresRes := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(nodefeature.ManufacturerNVIDIA)
		one := resource.MustParse("1")
		fifty := resource.MustParse("50")
		twentyFive := resource.MustParse("25")

		for _, ctr := range slicedPod.Spec.InitContainers {
			require.True(t, one.Equal(ctr.Resources.Requests[slicedCardRes]))
			require.True(t, one.Equal(ctr.Resources.Limits[slicedCardRes]))
			require.True(t, fifty.Equal(ctr.Resources.Requests[slicedMemRes]))
			require.True(t, fifty.Equal(ctr.Resources.Limits[slicedMemRes]))
			require.True(t, twentyFive.Equal(ctr.Resources.Requests[slicedCoresRes]))
			require.True(t, twentyFive.Equal(ctr.Resources.Limits[slicedCoresRes]))
			require.True(t, localStorageQty.Equal(ctr.Resources.Requests[core.ResourceEphemeralStorage]))
			require.True(t, localStorageQty.Equal(ctr.Resources.Limits[core.ResourceEphemeralStorage]))
			require.NotContains(t, ctr.Resources.Requests, accelRes)
		}

		// Sliced mode rejected when AcceleratorType is not logically sliceable.
		_, err = BuildSandboxPod(SandboxPodSpec{
			Forger:             forger,
			Name:               "sb-sliced-bad",
			RoleName:           "decode",
			Generation:         1,
			InputConfigMapName: "cm-1",
			EntranceQueue:      "nvidia-h100-lq",
			AcceleratorType:    nonSliceableType,
			Hardware:           hw,
			OperatorImage:      "gpustack/operator:v1.0.0",
			SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
				RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock,
				Resources: &workercore.ModelDeploymentForgerSandboxResources{
					AcceleratorSlicedMemoryPercentage: 50,
				},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "logical slicing")

		// Partitioned mode renders .partitioned and .partitioned.<kind>-<profile> = 1.
		partPod, err := BuildSandboxPod(SandboxPodSpec{
			Forger:             forger,
			Name:               "sb-partitioned",
			RoleName:           "decode",
			Generation:         1,
			InputConfigMapName: "cm-1",
			EntranceQueue:      "nvidia-h100-lq",
			AcceleratorType:    nonSliceableType,
			Hardware:           hw,
			OperatorImage:      "gpustack/operator:v1.0.0",
			SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
				RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock,
				Resources: &workercore.ModelDeploymentForgerSandboxResources{
					AcceleratorPartitionedProfile: "3g.40gb",
				},
			},
		})
		require.NoError(t, err)

		partCardRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModePartitioned)
		partProfRes := nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, "3g.40gb")
		require.Equal(t, core.ResourceName("nvidia.com/gpu.partitioned.mig-3g.40gb"), partProfRes)
		for _, ctr := range partPod.Spec.InitContainers {
			require.True(t, one.Equal(ctr.Resources.Requests[partCardRes]))
			require.True(t, one.Equal(ctr.Resources.Limits[partCardRes]))
			require.True(t, one.Equal(ctr.Resources.Requests[partProfRes]))
			require.True(t, one.Equal(ctr.Resources.Limits[partProfRes]))
			require.NotContains(t, ctr.Resources.Requests, accelRes)
		}
	})

	t.Run("validation rejects missing required inputs", func(t *testing.T) {
		t.Parallel()

		badSpecs := []SandboxPodSpec{
			{},
			{Forger: &workercore.ModelDeploymentForger{}},
			{Forger: forger, Name: "", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw},
			{Forger: forger, Name: "sb", RoleName: "", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw},
			{Forger: forger, Name: "sb", RoleName: "decode", Generation: 0, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw},
			{Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw},
			{Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "", OperatorImage: "op", Hardware: hw},
			{Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "", Hardware: hw},
			{
				Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw,
				SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{RunnerMode: "Bogus"},
			},
			{
				Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw,
				SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{RunnerMode: workercore.ModelDeploymentForgerRunnerModeExternal},
			},
			{
				Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw,
				SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
					RunnerMode: workercore.ModelDeploymentForgerRunnerModeExternal,
					Runner:     &workercore.ModelDeploymentForgerSandboxRunner{Image: "runner:v1"},
				},
			},
			{
				Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw,
				SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
					RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock,
					Runner:     &workercore.ModelDeploymentForgerSandboxRunner{Image: "runner:v1"},
				},
			},
			{
				Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw,
				SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
					RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock,
					Resources: &workercore.ModelDeploymentForgerSandboxResources{
						AcceleratorSlicedCoresPercentage: 50,
					},
				},
			},
			{
				Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw,
				SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
					RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock,
					Resources: &workercore.ModelDeploymentForgerSandboxResources{
						AcceleratorPartitionedProfile:     "3g.40gb",
						AcceleratorSlicedMemoryPercentage: 50,
					},
				},
			},
			{
				Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op",
				Manufacturer: nodefeature.ManufacturerAMD,
				Hardware: modelkernelkey.NodeHardware{
					Manufacturer:   nodefeature.ManufacturerAMD,
					SelectorLabels: map[string]string{"feature.node.kubernetes.io/gpustack-acceleratable-amd-mi300x": "true"},
				},
				SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
					RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock,
					Resources: &workercore.ModelDeploymentForgerSandboxResources{
						AcceleratorPartitionedProfile: "3g.40gb",
					},
				},
			},
			{
				Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op",
				Manufacturer: "nvidia",
				SandboxSpec:  workercore.ModelDeploymentForgerSandboxSpec{RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock},
			},
			{
				Forger: forger, Name: "sb", RoleName: "decode", Generation: 1, InputConfigMapName: "cm", EntranceQueue: "lq", OperatorImage: "op", Hardware: hw,
				Thresholds:  &VerificationThresholds{MinSpeedupRatio: 0.9},
				SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock},
			},
		}
		for _, spec := range badSpecs {
			_, err := BuildSandboxPod(spec)
			require.Error(t, err)
		}

		// ConfigMap builder validation edge cases.
		_, _, err := BuildInputConfigMap(InputConfigMapSpec{})
		require.Error(t, err)
		_, _, err = BuildInputConfigMap(InputConfigMapSpec{Forger: forger, Name: "", RoleName: "decode", Generation: 1})
		require.Error(t, err)
		_, _, err = BuildInputConfigMap(InputConfigMapSpec{Forger: forger, Name: "cm", RoleName: "decode", Generation: 0})
		require.Error(t, err)
		require.Nil(t, ConfigMapVolumeItems(nil))
		require.Equal(t, "op", sanitizeConfigMapSegment("///"))
		require.Len(t, sanitizeConfigMapSegment(strings.Repeat("a", 200)), 120)
	})
}

func TestSandboxLabels_DefensivelyTruncateLongForgerAndRoleNames(t *testing.T) {
	t.Parallel()

	longForgerName := strings.Repeat("forger-name-segment-", 5) // 100 chars
	longRoleName := strings.Repeat("decode-role-segment-", 5)   // 100 chars
	forger := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{
			Name:      longForgerName,
			Namespace: "team-a",
			UID:       types.UID("forger-uid-long"),
		},
	}
	validKey := sampleValidKernelKey(t, modelkernelkey.OpFamilyRMSNorm)
	req := ForgeRequest{
		Engine:           "vLLM",
		EngineVersion:    "0.8.5",
		Manufacturer:     "nvidia",
		Architecture:     "sm_90a",
		TargetKernelKeys: []string{validKey},
		MinSpeedupRatio:  1.05,
		Atol:             1e-2,
		Rtol:             1e-2,
	}

	cm, _, err := BuildInputConfigMap(InputConfigMapSpec{
		Forger:     forger,
		Name:       "long-forger-input-1",
		RoleName:   longRoleName,
		Generation: 1,
		Request:    req,
	})
	require.NoError(t, err)
	for k, v := range cm.Labels {
		require.LessOrEqual(t, len(v), kvalidation.DNS1123LabelMaxLength, "ConfigMap label %s=%q exceeds 63 chars", k, v)
		require.Empty(t, kvalidation.IsValidLabelValue(v), "ConfigMap label %s=%q is not a valid K8s label value", k, v)
	}

	pod, err := BuildSandboxPod(SandboxPodSpec{
		Forger:             forger,
		Name:               "long-forger-sb-1",
		RoleName:           longRoleName,
		Generation:         1,
		InputConfigMapName: "long-forger-input-1",
		EntranceQueue:      "nvidia-h100-lq",
		Hardware:           sampleNodeHardware(),
		OperatorImage:      "gpustack/operator:v1.0.0",
		SandboxSpec: workercore.ModelDeploymentForgerSandboxSpec{
			RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock,
		},
	})
	require.NoError(t, err)
	for k, v := range pod.Labels {
		require.LessOrEqual(t, len(v), kvalidation.DNS1123LabelMaxLength, "Pod label %s=%q exceeds 63 chars", k, v)
		require.Empty(t, kvalidation.IsValidLabelValue(v), "Pod label %s=%q is not a valid K8s label value", k, v)
	}
	require.Equal(t, cm.Labels[SandboxLabelInstance], pod.Labels[SandboxLabelInstance])
	require.Equal(t, cm.Labels[SandboxLabelRole], pod.Labels[SandboxLabelRole])
}
