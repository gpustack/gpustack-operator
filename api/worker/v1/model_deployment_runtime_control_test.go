package v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	applycorev1 "gpustack.ai/gpustack/pkg/kubeclients/applyconfiguration/core/v1"
	applyv1 "gpustack.ai/gpustack/pkg/kubeclients/applyconfiguration/worker/v1"
	applyv1alpha1 "gpustack.ai/gpustack/pkg/kubeclients/applyconfiguration/worker/v1alpha1"
)

func fullRuntimeControlFixture() *ModelDeployment {
	return &ModelDeployment{
		ObjectMeta: meta.ObjectMeta{
			Name:       "qwen-runtime-control",
			Namespace:  "default",
			Generation: 3,
		},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{
				Name:        "Qwen/Qwen2.5-72B-Instruct",
				ArtifactRef: &core.LocalObjectReference{Name: "qwen-weights"},
				LoRAAdapters: []workercore.ModelDeploymentLoRAAdapter{
					{
						Name:        "sql-helper",
						ArtifactRef: core.LocalObjectReference{Name: "sql-helper-lora"},
						SubPath:     "adapters/sql",
						Pinned:      true,
					},
					{
						Name:        "code-review",
						ArtifactRef: core.LocalObjectReference{Name: "code-review-lora"},
					},
				},
			},
			Engine: workercore.ModelDeploymentEngine{
				Name:  workercore.ModelDeploymentEngineVLLM,
				Image: "vllm/vllm-openai:v0.25.1",
			},
			Roles: []workercore.ModelDeploymentRole{
				{
					Name:            "server",
					Kind:            workercore.ModelDeploymentRoleKindServer,
					Replicas:        1,
					ReplicaSize:     1,
					AcceleratorType: "h20-8x",
					Pause:           true,
					RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							"scheduling.maxActiveRequests":         "128",
							"scheduling.maxBatchedTokens":          "4096",
							"scheduling.longPrefillTokenThreshold": "1024",
							"speculative.acceptThresholdSingle":    "0.3",
							"elastic.dataParallelWidth":            "4",
						},
					},
				},
			},
		},
		Status: workercore.ModelDeploymentStatus{
			Roles: []workercore.ModelDeploymentRoleStatus{
				{
					Name:          "server",
					Desired:       1,
					Ready:         1,
					QuotaReserved: 1,
					Kind:          workercore.ModelDeploymentRoleKindServer,
					RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControlStatus{
						ObservedGeneration:  3,
						Synced:              true,
						Reason:              "Synced",
						Message:             "all answering pods converged",
						Paused:              true,
						AppliedConfigDigest: "sha256:0123456789abcdef",
						ConvergedPods:       1,
						TotalAnsweringPods:  1,
						UnsupportedFields:   []string{"speculative.acceptThresholdSingle"},
					},
				},
			},
		},
	}
}

func baselineModelDeploymentFixture() *ModelDeployment {
	return &ModelDeployment{
		ObjectMeta: meta.ObjectMeta{
			Name:      "qwen-baseline",
			Namespace: "default",
		},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{
				Name: "Qwen/Qwen2.5-72B-Instruct",
			},
			Engine: workercore.ModelDeploymentEngine{
				Name:  workercore.ModelDeploymentEngineVLLM,
				Image: "vllm/vllm-openai:v0.25.1",
			},
			Roles: []workercore.ModelDeploymentRole{
				{
					Name:            "server",
					Kind:            workercore.ModelDeploymentRoleKindServer,
					Replicas:        1,
					ReplicaSize:     1,
					AcceleratorType: "h20-8x",
					Resources: &workercore.ModelDeploymentRoleResources{
						Accelerator: ptr.To(resource.MustParse("1")),
					},
					ImagePullSecrets: []core.LocalObjectReference{{Name: "reg-secret"}},
				},
			},
		},
		Status: workercore.ModelDeploymentStatus{
			Roles: []workercore.ModelDeploymentRoleStatus{
				{
					Name:          "server",
					Desired:       1,
					Ready:         1,
					QuotaReserved: 1,
					Kind:          workercore.ModelDeploymentRoleKindServer,
				},
			},
		},
	}
}

func TestModelDeploymentRuntimeControlJSONRoundTripAndOmission(t *testing.T) {
	t.Run("full lean runtimeControl, pause, loraAdapters, and status round-trip in v1 and v1alpha1", func(t *testing.T) {
		v1Obj := fullRuntimeControlFixture()
		v1Alpha1Obj := (*workercore.ModelDeployment)(v1Obj.DeepCopy())

		cases := []struct {
			name string
			src  any
			dst  func() any
			eq   func(a, b any) bool
		}{
			{
				name: "v1",
				src:  v1Obj,
				dst:  func() any { return &ModelDeployment{} },
				eq: func(a, b any) bool {
					return equality.Semantic.DeepEqual(a, b)
				},
			},
			{
				name: "v1alpha1",
				src:  v1Alpha1Obj,
				dst:  func() any { return &workercore.ModelDeployment{} },
				eq: func(a, b any) bool {
					return equality.Semantic.DeepEqual(a, b)
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				raw, err := json.Marshal(tc.src)
				require.NoError(t, err)

				got := tc.dst()
				require.NoError(t, json.Unmarshal(raw, got))
				assert.True(t, tc.eq(tc.src, got), "JSON round-trip must be lossless")
			})
		}
	})

	t.Run("omitted pause, loraAdapters, and runtimeControl produce zero JSON diff", func(t *testing.T) {
		base := baselineModelDeploymentFixture()
		baseRaw, err := json.Marshal(base)
		require.NoError(t, err)

		var doc map[string]any
		require.NoError(t, json.Unmarshal(baseRaw, &doc))

		modelMap := doc["spec"].(map[string]any)["model"].(map[string]any)
		assert.NotContains(t, modelMap, "loraAdapters")

		roles := doc["spec"].(map[string]any)["roles"].([]any)
		roleMap := roles[0].(map[string]any)
		assert.NotContains(t, roleMap, "pause")
		assert.NotContains(t, roleMap, "runtimeControl")
		assert.NotContains(t, roleMap, "runtimeBounds")
		assert.NotContains(t, roleMap, "elasticEp")

		statusRoles := doc["status"].(map[string]any)["roles"].([]any)
		statusRoleMap := statusRoles[0].(map[string]any)
		assert.NotContains(t, statusRoleMap, "runtimeControl")

		var decoded ModelDeployment
		require.NoError(t, json.Unmarshal(baseRaw, &decoded))
		reEncoded, err := json.Marshal(&decoded)
		require.NoError(t, err)
		assert.JSONEq(t, string(baseRaw), string(reEncoded))
	})

	t.Run("loraAdapters decode distinguishes nil vs explicit empty slice", func(t *testing.T) {
		cases := []struct {
			name    string
			payload string
			wantNil bool
		}{
			{
				name:    "absent loraAdapters decodes to nil",
				payload: `{"name":"Qwen/Qwen2.5-72B-Instruct"}`,
				wantNil: true,
			},
			{
				name:    "null loraAdapters decodes to nil",
				payload: `{"name":"Qwen/Qwen2.5-72B-Instruct","loraAdapters":null}`,
				wantNil: true,
			},
			{
				name:    "explicit empty loraAdapters decodes to non-nil empty slice",
				payload: `{"name":"Qwen/Qwen2.5-72B-Instruct","loraAdapters":[]}`,
				wantNil: false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var model workercore.ModelDeploymentModel
				require.NoError(t, json.Unmarshal([]byte(tc.payload), &model))
				if tc.wantNil {
					assert.Nil(t, model.LoRAAdapters)
				} else {
					assert.NotNil(t, model.LoRAAdapters)
					assert.Empty(t, model.LoRAAdapters)
				}
			})
		}
	})

	t.Run("runtimeControl parameters decode distinguishes nil vs explicit empty map", func(t *testing.T) {
		cases := []struct {
			name    string
			payload string
			wantNil bool
		}{
			{
				name:    "absent parameters decodes to nil",
				payload: `{}`,
				wantNil: true,
			},
			{
				name:    "null parameters decodes to nil",
				payload: `{"parameters":null}`,
				wantNil: true,
			},
			{
				name:    "explicit empty parameters decodes to non-nil empty map",
				payload: `{"parameters":{}}`,
				wantNil: false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var rc workercore.ModelDeploymentRoleRuntimeControl
				require.NoError(t, json.Unmarshal([]byte(tc.payload), &rc))
				if tc.wantNil {
					assert.Nil(t, rc.Parameters)
				} else {
					assert.NotNil(t, rc.Parameters)
					assert.Empty(t, rc.Parameters)
				}
			})
		}
	})
}

func TestModelDeploymentRuntimeControlProtobufRoundTrip(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, workercore.AddToScheme(scheme))
	require.NoError(t, AddToScheme(scheme))
	pbSerializer := protobuf.NewSerializer(scheme, scheme)

	v1Obj := fullRuntimeControlFixture()
	v1Alpha1Obj := (*workercore.ModelDeployment)(v1Obj.DeepCopy())

	cases := []struct {
		name string
		obj  runtime.Object
		into func() runtime.Object
	}{
		{
			name: "v1",
			obj:  v1Obj,
			into: func() runtime.Object { return &ModelDeployment{} },
		},
		{
			name: "v1alpha1",
			obj:  v1Alpha1Obj,
			into: func() runtime.Object { return &workercore.ModelDeployment{} },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := runtime.Encode(pbSerializer, tc.obj)
			require.NoError(t, err)

			decoded := tc.into()
			_, _, err = pbSerializer.Decode(wire, nil, decoded)
			require.NoError(t, err)

			switch want := tc.obj.(type) {
			case *ModelDeployment:
				got := decoded.(*ModelDeployment)
				assert.Equal(t, want.ObjectMeta, got.ObjectMeta)
				assert.True(t, equality.Semantic.DeepEqual(want.Spec, got.Spec),
					"protobuf round-trip must preserve Spec (including loraAdapters, pause, and runtimeControl)")
				assert.True(t, equality.Semantic.DeepEqual(want.Status, got.Status),
					"protobuf round-trip must preserve Status (including status.roles[*].runtimeControl)")
			case *workercore.ModelDeployment:
				got := decoded.(*workercore.ModelDeployment)
				assert.Equal(t, want.ObjectMeta, got.ObjectMeta)
				assert.True(t, equality.Semantic.DeepEqual(want.Spec, got.Spec),
					"protobuf round-trip must preserve Spec (including loraAdapters, pause, and runtimeControl)")
				assert.True(t, equality.Semantic.DeepEqual(want.Status, got.Status),
					"protobuf round-trip must preserve Status (including status.roles[*].runtimeControl)")
			}
		})
	}
}

func TestModelDeploymentRuntimeControlConversionRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		public *ModelDeployment
	}{
		{
			name:   "baseline deployment preserves absent pause, loraAdapters, and runtimeControl",
			public: baselineModelDeploymentFixture(),
		},
		{
			name:   "full loraAdapters, pause, runtimeControl, and role runtimeControl status round-trip",
			public: fullRuntimeControlFixture(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var storage workercore.ModelDeployment
			require.NoError(t, Convert_v1_ModelDeployment_To_v1alpha1_ModelDeployment(tc.public, &storage, nil))

			var roundTrip ModelDeployment
			require.NoError(t, Convert_v1alpha1_ModelDeployment_To_v1_ModelDeployment(&storage, &roundTrip, nil))
			assert.True(t, equality.Semantic.DeepEqual(tc.public, &roundTrip),
				"v1 <-> v1alpha1 conversion must be lossless")
		})
	}
}

func TestModelDeploymentRuntimeControlApplyConfigurationRoundTrip(t *testing.T) {
	buildModelSpec := func() *applyv1alpha1.ModelDeploymentModelApplyConfiguration {
		return applyv1alpha1.ModelDeploymentModel().
			WithName("Qwen/Qwen2.5-72B-Instruct").
			WithArtifactRef(applycorev1.LocalObjectReference().WithName("qwen-weights")).
			WithLoRAAdapters(
				applyv1alpha1.ModelDeploymentLoRAAdapter().
					WithName("sql-helper").
					WithArtifactRef(applycorev1.LocalObjectReference().WithName("sql-helper-lora")).
					WithSubPath("adapters/sql").
					WithPinned(true),
				applyv1alpha1.ModelDeploymentLoRAAdapter().
					WithName("code-review").
					WithArtifactRef(applycorev1.LocalObjectReference().WithName("code-review-lora")),
			)
	}

	buildRoleSpec := func() *applyv1alpha1.ModelDeploymentRoleApplyConfiguration {
		return applyv1alpha1.ModelDeploymentRole().
			WithName("server").
			WithKind(workercore.ModelDeploymentRoleKindServer).
			WithReplicas(1).
			WithReplicaSize(1).
			WithAcceleratorType("h20-8x").
			WithPause(true).
			WithRuntimeControl(
				applyv1alpha1.ModelDeploymentRoleRuntimeControl().
					WithParameters(map[string]string{
						"scheduling.maxActiveRequests":         "128",
						"scheduling.maxBatchedTokens":          "4096",
						"scheduling.longPrefillTokenThreshold": "1024",
						"speculative.acceptThresholdSingle":    "0.3",
						"elastic.dataParallelWidth":            "4",
					}),
			)
	}

	buildStatus := func() *applyv1alpha1.ModelDeploymentStatusApplyConfiguration {
		return applyv1alpha1.ModelDeploymentStatus().
			WithRoles(
				applyv1alpha1.ModelDeploymentRoleStatus().
					WithName("server").
					WithDesired(1).
					WithReady(1).
					WithQuotaReserved(1).
					WithKind(workercore.ModelDeploymentRoleKindServer).
					WithRuntimeControl(
						applyv1alpha1.ModelDeploymentRoleRuntimeControlStatus().
							WithObservedGeneration(3).
							WithSynced(true).
							WithReason("Synced").
							WithMessage("all answering pods converged").
							WithPaused(true).
							WithAppliedConfigDigest("sha256:0123456789abcdef").
							WithConvergedPods(1).
							WithTotalAnsweringPods(1).
							WithUnsupportedFields("speculative.acceptThresholdSingle"),
					),
			)
	}

	want := fullRuntimeControlFixture()
	want.Generation = 0

	t.Run("v1alpha1 applyconfiguration", func(t *testing.T) {
		cfg := applyv1alpha1.ModelDeployment("qwen-runtime-control", "default").
			WithSpec(
				applyv1alpha1.ModelDeploymentSpec().
					WithModel(buildModelSpec()).
					WithEngine(applyv1alpha1.ModelDeploymentEngine().
						WithName(workercore.ModelDeploymentEngineVLLM).
						WithImage("vllm/vllm-openai:v0.25.1")).
					WithRoles(buildRoleSpec()),
			).
			WithStatus(buildStatus())

		raw, err := json.Marshal(cfg)
		require.NoError(t, err)

		var got workercore.ModelDeployment
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.True(t, equality.Semantic.DeepEqual(want.Spec.Model, got.Spec.Model))
		assert.Equal(t, want.Spec.Roles[0].Pause, got.Spec.Roles[0].Pause)
		assert.True(t, equality.Semantic.DeepEqual(want.Spec.Roles[0].RuntimeControl, got.Spec.Roles[0].RuntimeControl))
		assert.True(t, equality.Semantic.DeepEqual(want.Status.Roles[0].RuntimeControl, got.Status.Roles[0].RuntimeControl))
	})

	t.Run("v1 applyconfiguration", func(t *testing.T) {
		cfg := applyv1.ModelDeployment("qwen-runtime-control", "default").
			WithSpec(
				applyv1alpha1.ModelDeploymentSpec().
					WithModel(buildModelSpec()).
					WithEngine(applyv1alpha1.ModelDeploymentEngine().
						WithName(workercore.ModelDeploymentEngineVLLM).
						WithImage("vllm/vllm-openai:v0.25.1")).
					WithRoles(buildRoleSpec()),
			).
			WithStatus(buildStatus())

		raw, err := json.Marshal(cfg)
		require.NoError(t, err)

		var got ModelDeployment
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.True(t, equality.Semantic.DeepEqual(want.Spec.Model, got.Spec.Model))
		assert.Equal(t, want.Spec.Roles[0].Pause, got.Spec.Roles[0].Pause)
		assert.True(t, equality.Semantic.DeepEqual(want.Spec.Roles[0].RuntimeControl, got.Spec.Roles[0].RuntimeControl))
		assert.True(t, equality.Semantic.DeepEqual(want.Status.Roles[0].RuntimeControl, got.Status.Roles[0].RuntimeControl))
	})
}

func TestModelDeploymentRuntimeControlCRDSchemaAndScalerReason(t *testing.T) {
	crd := workercore.GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd, "ModelDeployment CRD must be embedded")
	require.Len(t, crd.Spec.Versions, 1)

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	specModel := root.Properties["spec"].Properties["model"]
	specRole := root.Properties["spec"].Properties["roles"].Items.Schema
	statusRole := root.Properties["status"].Properties["roles"].Items.Schema

	t.Run("spec.model.loraAdapters schema", func(t *testing.T) {
		loras, ok := specModel.Properties["loraAdapters"]
		require.True(t, ok, "loraAdapters must exist on spec.model")
		require.NotNil(t, loras.XListType)
		assert.Equal(t, "map", *loras.XListType)
		assert.Equal(t, []string{"name"}, loras.XListMapKeys)

		item := loras.Items.Schema
		assert.ElementsMatch(t, []string{"name", "artifactRef"}, item.Required)
		for _, f := range []string{"name", "artifactRef", "subPath", "pinned"} {
			assert.Contains(t, item.Properties, f)
		}
		assert.NotContains(t, item.Properties, "path")
		require.NotNil(t, item.Properties["name"].MinLength)
		assert.Equal(t, int64(1), *item.Properties["name"].MinLength)
	})

	t.Run("spec.roles[] removes runtimeBounds and elasticEp and adds pause", func(t *testing.T) {
		assert.NotContains(t, specRole.Properties, "runtimeBounds")
		assert.NotContains(t, specRole.Properties, "elasticEp")

		pauseProp, ok := specRole.Properties["pause"]
		require.True(t, ok, "pause must exist on spec.roles[]")
		assert.Equal(t, "boolean", pauseProp.Type)
	})

	t.Run("spec.roles[].runtimeControl schema", func(t *testing.T) {
		rc, ok := specRole.Properties["runtimeControl"]
		require.True(t, ok, "runtimeControl must exist on spec.roles[]")

		assert.NotContains(t, rc.Properties, "enabled")
		assert.NotContains(t, rc.Properties, "scheduling")
		assert.NotContains(t, rc.Properties, "speculative")
		assert.NotContains(t, rc.Properties, "lifecycle")
		assert.NotContains(t, rc.Properties, "loraAdapters")

		params, ok := rc.Properties["parameters"]
		require.True(t, ok, "parameters must exist on spec.roles[].runtimeControl")
		assert.Equal(t, "object", params.Type)
		require.NotNil(t, params.AdditionalProperties)
		require.NotNil(t, params.AdditionalProperties.Schema)
		assert.Equal(t, "string", params.AdditionalProperties.Schema.Type)
	})

	t.Run("status.roles[].runtimeControl schema", func(t *testing.T) {
		rcStatus, ok := statusRole.Properties["runtimeControl"]
		require.True(t, ok, "runtimeControl must exist on status.roles[]")

		for _, f := range []string{
			"observedGeneration", "synced", "reason", "message",
			"paused", "appliedConfigDigest", "convergedPods", "totalAnsweringPods", "unsupportedFields",
		} {
			assert.Contains(t, rcStatus.Properties, f)
		}
		assert.NotContains(t, rcStatus.Properties, "powerState")
		assert.NotContains(t, rcStatus.Properties, "schedulingState")
		assert.NotContains(t, rcStatus.Properties, "activePowerState")
		assert.NotContains(t, rcStatus.Properties, "activeSchedulingState")
		assert.NotContains(t, rcStatus.Properties, "loadedLoRAAdapters")
		assert.ElementsMatch(t, []string{"synced", "convergedPods", "totalAnsweringPods"}, rcStatus.Required)
		assert.Equal(t, "boolean", rcStatus.Properties["paused"].Type)
		require.NotNil(t, rcStatus.Properties["unsupportedFields"].XListType)
		assert.Equal(t, "atomic", *rcStatus.Properties["unsupportedFields"].XListType)
	})

	t.Run("scaler RolePaused reason constant", func(t *testing.T) {
		assert.Equal(t, "RolePaused", workercore.ModelDeploymentScalerReasonRolePaused)
	})
}
