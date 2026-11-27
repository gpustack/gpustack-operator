package v1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/utils/ptr"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	applycorev1 "gpustack.ai/gpustack/pkg/kubeclients/applyconfiguration/core/v1"
	applyv1 "gpustack.ai/gpustack/pkg/kubeclients/applyconfiguration/worker/v1"
	applyv1alpha1 "gpustack.ai/gpustack/pkg/kubeclients/applyconfiguration/worker/v1alpha1"
)

func fullKernelArtifactFixture() *ModelKernelArtifact {
	now := meta.NewTime(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC))
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	return &ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{
			Name:       "qwen-decode-gen-1",
			Namespace:  "team-a",
			Generation: 1,
		},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
					ManifestDigest:         digestA,
					DispatchManifestDigest: digestB,
					SizeBytes:              8192,
					FileCount:              4,
					SeedNode:               "gpu-node-1",
					ForgerRef: workercore.ModelKernelArtifactForgerReference{
						Name: "qwen-decode-forger",
						UID:  "forger-uid-1",
					},
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        workercore.ModelDeploymentEngineVLLM,
				EngineVersion: "0.8",
				Manufacturer:  "nvidia",
				Architecture:  "sm_90a",
				KernelKeys:    []string{"vllm/0.8/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
			},
		},
		Status: workercore.ModelKernelArtifactStatus{
			ObservedGeneration: 1,
			Resolved: &workercore.ModelKernelArtifactResolved{
				ManifestDigest:         digestA,
				DispatchManifestDigest: digestB,
				FileCount:              4,
				SizeBytes:              8192,
				ResolvedTime:           now,
				LastValidatedTime:      &now,
				DigestSource:           workercore.ModelKernelArtifactDigestSourceForge,
			},
			Nodes: &workercore.ModelArtifactNodes{
				Ready:       2,
				Downloading: 0,
				Failed:      0,
			},
			Quarantined:       true,
			QuarantineReason:  "CanaryReadinessTimeout",
			QuarantineMessage: "canary replica failed readiness check",
			Conditions: []gpustack.Condition{
				{
					Type:               workercore.ModelKernelArtifactConditionTypeResolved,
					Status:             meta.ConditionTrue,
					Reason:             workercore.ModelKernelArtifactReasonResolved,
					LastTransitionTime: now,
				},
			},
		},
	}
}

func fullDeploymentForgerFixture() *ModelDeploymentForger {
	now := meta.NewTime(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC))
	digestA := "sha256:" + strings.Repeat("a", 64)
	return &ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{
			Name:       "qwen-decode-forger",
			Namespace:  "team-a",
			Generation: 2,
		},
		Spec: workercore.ModelDeploymentForgerSpec{
			TargetRef: core.LocalObjectReference{Name: "qwen-pd"},
			Mode:      workercore.ModelDeploymentForgerModeActive,
			Suspend:   false,
			Roles: []workercore.ModelDeploymentForgerRoleSpec{
				{
					Name:                    "decode",
					MaxKernelsPerGeneration: 4,
					MinSpeedupRatio:         "1.05",
				},
			},
			Probe: workercore.ModelDeploymentForgerProbeSpec{
				Steps:             4,
				CooldownSeconds:   600,
				MinActiveRequests: 1,
			},
			Sandbox: workercore.ModelDeploymentForgerSandboxSpec{
				RunnerMode: workercore.ModelDeploymentForgerRunnerModeExternal,
				Runner: &workercore.ModelDeploymentForgerSandboxRunner{
					Image:           "gpustack/forge-runner:v0.1.0",
					ImagePullPolicy: core.PullIfNotPresent,
					Command:         []string{"loopwright-forger"},
					Args:            []string{"--budget", "4"},
					Env: []workercore.ModelDeploymentForgerSandboxEnvVar{
						{Name: "LOOPWRIGHT_MODEL", Value: "gpt-5"},
						{
							Name: "LOOPWRIGHT_PROVIDER_TOKEN",
							ValueFrom: &workercore.ModelDeploymentForgerSandboxEnvVarSource{
								SecretKeyRef: &core.SecretKeySelector{
									LocalObjectReference: core.LocalObjectReference{Name: "llm-creds"},
									Key:                  "token",
								},
							},
						},
					},
					EnvFrom: []core.EnvFromSource{
						{
							ConfigMapRef: &core.ConfigMapEnvSource{
								LocalObjectReference: core.LocalObjectReference{Name: "forger-config"},
							},
						},
					},
				},
				PriorityClassName:     "system-cluster-critical",
				ActiveDeadlineSeconds: 1800,
				OutputSizeLimit:       ptr.To(resource.MustParse("2Gi")),
				ImagePullSecrets: []core.LocalObjectReference{
					{Name: "regcred"},
				},
				Resources: &workercore.ModelDeploymentForgerSandboxResources{
					CPU:                               ptr.To(resource.MustParse("8")),
					RAM:                               ptr.To(resource.MustParse("32Gi")),
					LocalStorage:                      ptr.To(resource.MustParse("20Gi")),
					AcceleratorSlicedMemoryPercentage: 50,
					AcceleratorSlicedCoresPercentage:  50,
				},
			},
			Rollout: workercore.ModelDeploymentForgerRolloutSpec{
				MaxRetainedGenerations: 3,
				FailureTimeoutSeconds:  600,
			},
		},
		Status: workercore.ModelDeploymentForgerStatus{
			ObservedGeneration: 2,
			Binding: &workercore.ModelDeploymentForgerBindingStatus{
				TargetUID:  "md-uid-1",
				TargetName: "qwen-pd",
				Term:       1,
			},
			Roles: []workercore.ModelDeploymentForgerRoleStatus{
				{
					Name:                   "decode",
					Phase:                  workercore.ModelDeploymentForgerRolePhasePromoting,
					ActiveGeneration:       2,
					ActiveKernelArtifact:   "qwen-decode-forger-decode-gen-2",
					PreviousKernelArtifact: "qwen-decode-forger-decode-gen-1",
					ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
						PodName:       "qwen-decode-forger-sandbox-2",
						PodUID:        "pod-uid-2",
						NodeName:      "gpu-node-1",
						ConfigMapName: "qwen-decode-forger-input-2",
						StartedAt:     &now,
					},
					ObservedBottlenecks: []workercore.ModelDeploymentForgerBottleneck{
						{
							LocalKernelKey:      "vllm/0.8/nvidia/sm_90a/rmsnorm/0123456789abcdef",
							OpFamily:            "rmsnorm",
							GPUTimeSharePercent: "12.5",
							ObservedAt:          now,
						},
					},
					Generations: []workercore.ModelDeploymentForgerGenerationStatus{
						{
							Generation:     1,
							ArtifactName:   "qwen-decode-forger-decode-gen-1",
							ManifestDigest: digestA,
							Outcome:        workercore.ModelDeploymentForgerGenerationOutcomePromoted,
							Reason:         "Converged",
							Message:        "all decode replicas confirmed loaded kernels",
							StartedAt:      &now,
							FinishedAt:     &now,
						},
					},
				},
			},
			Conditions: []gpustack.Condition{
				{
					Type:               workercore.ModelDeploymentForgerConditionTypeBound,
					Status:             meta.ConditionTrue,
					Reason:             workercore.ModelDeploymentForgerReasonBound,
					LastTransitionTime: now,
				},
			},
		},
	}
}

func TestForgerAndKernelArtifactJSONProtobufAndConversionRoundTrip(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, workercore.AddToScheme(scheme))
	require.NoError(t, AddToScheme(scheme))
	pbSerializer := protobuf.NewSerializer(scheme, scheme)

	t.Run("ModelKernelArtifact JSON, Protobuf, and Conversion round-trip", func(t *testing.T) {
		v1Obj := fullKernelArtifactFixture()

		raw, err := json.Marshal(v1Obj)
		require.NoError(t, err)
		var gotJSON ModelKernelArtifact
		require.NoError(t, json.Unmarshal(raw, &gotJSON))
		assert.True(t, equality.Semantic.DeepEqual(v1Obj, &gotJSON))

		wire, err := runtime.Encode(pbSerializer, v1Obj)
		require.NoError(t, err)
		var gotPB ModelKernelArtifact
		_, _, err = pbSerializer.Decode(wire, nil, &gotPB)
		require.NoError(t, err)
		assert.True(t, equality.Semantic.DeepEqual(v1Obj.Spec, gotPB.Spec))
		assert.True(t, equality.Semantic.DeepEqual(v1Obj.Status, gotPB.Status))

		var storage workercore.ModelKernelArtifact
		require.NoError(t, Convert_v1_ModelKernelArtifact_To_v1alpha1_ModelKernelArtifact(v1Obj, &storage, nil))
		var roundTrip ModelKernelArtifact
		require.NoError(t, Convert_v1alpha1_ModelKernelArtifact_To_v1_ModelKernelArtifact(&storage, &roundTrip, nil))
		assert.True(t, equality.Semantic.DeepEqual(v1Obj, &roundTrip))
	})

	t.Run("ModelDeploymentForger JSON, Protobuf, and Conversion round-trip", func(t *testing.T) {
		v1Obj := fullDeploymentForgerFixture()

		raw, err := json.Marshal(v1Obj)
		require.NoError(t, err)
		var gotJSON ModelDeploymentForger
		require.NoError(t, json.Unmarshal(raw, &gotJSON))
		assert.True(t, equality.Semantic.DeepEqual(v1Obj, &gotJSON))

		wire, err := runtime.Encode(pbSerializer, v1Obj)
		require.NoError(t, err)
		var gotPB ModelDeploymentForger
		_, _, err = pbSerializer.Decode(wire, nil, &gotPB)
		require.NoError(t, err)
		assert.True(t, equality.Semantic.DeepEqual(v1Obj.Spec, gotPB.Spec))
		assert.True(t, equality.Semantic.DeepEqual(v1Obj.Status, gotPB.Status))

		var storage workercore.ModelDeploymentForger
		require.NoError(t, Convert_v1_ModelDeploymentForger_To_v1alpha1_ModelDeploymentForger(v1Obj, &storage, nil))
		var roundTrip ModelDeploymentForger
		require.NoError(t, Convert_v1alpha1_ModelDeploymentForger_To_v1_ModelDeploymentForger(&storage, &roundTrip, nil))
		assert.True(t, equality.Semantic.DeepEqual(v1Obj, &roundTrip))
	})

	t.Run("ModelDeployment kernelArtifactRef and role status kernelArtifact round-trip", func(t *testing.T) {
		md := baselineModelDeploymentFixture()
		baseRaw, err := json.Marshal(md)
		require.NoError(t, err)
		assert.NotContains(t, string(baseRaw), "kernelArtifactRef")
		assert.NotContains(t, string(baseRaw), "kernelArtifact")

		md.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "qwen-decode-gen-1"}
		md.Status.Roles[0].KernelArtifact = &workercore.ModelDeploymentRoleKernelArtifactStatus{
			Artifact:               "qwen-decode-gen-1",
			ManifestDigest:         "sha256:" + strings.Repeat("a", 64),
			DispatchManifestDigest: "sha256:" + strings.Repeat("b", 64),
			Delivery:               workercore.ModelDeploymentKernelDeliveryNode,
			LoadedKernels:          3,
			Quarantined:            false,
		}

		wire, err := runtime.Encode(pbSerializer, md)
		require.NoError(t, err)
		var gotPB ModelDeployment
		_, _, err = pbSerializer.Decode(wire, nil, &gotPB)
		require.NoError(t, err)
		assert.True(t, equality.Semantic.DeepEqual(md.Spec, gotPB.Spec))
		assert.True(t, equality.Semantic.DeepEqual(md.Status, gotPB.Status))

		cfg := applyv1.ModelDeployment("qwen-baseline", "default").WithSpec(
			applyv1alpha1.ModelDeploymentSpec().WithRoles(
				applyv1alpha1.ModelDeploymentRole().
					WithName("server").
					WithKernelArtifactRef(applycorev1.LocalObjectReference().WithName("qwen-decode-gen-1")),
			),
		)
		cfgRaw, err := json.Marshal(cfg)
		require.NoError(t, err)
		assert.Contains(t, string(cfgRaw), `"kernelArtifactRef":{"name":"qwen-decode-gen-1"}`)
	})
}
