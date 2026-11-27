package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

func TestModelDeploymentEffectiveEngineVersion_AndSpecHash(t *testing.T) {
	t.Run("stored role version overrides engine version while empty stays empty without parsing image", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Engine.Image = "gpustack/runner:cuda13.0-vllm0.34.0"
			md.Spec.Engine.Version = ""
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{
					Name:            "inherit",
					Kind:            workercore.ModelDeploymentRoleKindPrefill,
					AcceleratorType: "h20-8x",
					Image:           "gpustack/runner:cuda13.0-vllm0.34.0",
				},
				{
					Name:            "override",
					Kind:            workercore.ModelDeploymentRoleKindDecode,
					AcceleratorType: "h20-8x",
					Image:           "gpustack/runner:cuda13.0-vllm0.32.0",
					EngineVersion:   "0.32.0",
				},
			}
		})

		assert.Equal(t, "", ModelDeploymentEffectiveEngineVersion(md, &md.Spec.Roles[0]),
			"empty stored version must remain empty and never re-parse runner image tags")
		assert.Equal(t, "0.32.0", ModelDeploymentEffectiveEngineVersion(md, &md.Spec.Roles[1]))

		at := newRenderAcceleratorType()
		legacyHash := renderOne(t, newRenderDeployment(), at).Annotations[modelDeploymentPodSpecHashAnnotation]
		require.NotEmpty(t, legacyHash)

		md.Spec.Engine.Version = "0.34.0"
		assert.Equal(t, "0.34.0", ModelDeploymentEffectiveEngineVersion(md, &md.Spec.Roles[0]))
		assert.Equal(t, "0.32.0", ModelDeploymentEffectiveEngineVersion(md, &md.Spec.Roles[1]))

		inheritBefore, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[0], AcceleratorType: at,
		})
		require.NoError(t, err)
		overrideBefore, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: md, Role: &md.Spec.Roles[1], AcceleratorType: at,
		})
		require.NoError(t, err)

		mdBumped := md.DeepCopy()
		mdBumped.Spec.Engine.Version = "0.35.0"
		inheritAfter, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: mdBumped, Role: &mdBumped.Spec.Roles[0], AcceleratorType: at,
		})
		require.NoError(t, err)
		overrideAfter, err := renderModelDeploymentPod(context.Background(), ModelDeploymentRenderInput{
			Deployment: mdBumped, Role: &mdBumped.Spec.Roles[1], AcceleratorType: at,
		})
		require.NoError(t, err)

		assert.NotEqual(t,
			inheritBefore.Annotations[modelDeploymentPodSpecHashAnnotation],
			inheritAfter.Annotations[modelDeploymentPodSpecHashAnnotation],
			"editing spec.engine.version must roll roles that inherit it",
		)
		assert.Equal(t,
			overrideBefore.Annotations[modelDeploymentPodSpecHashAnnotation],
			overrideAfter.Annotations[modelDeploymentPodSpecHashAnnotation],
			"editing spec.engine.version must not roll roles with explicit role.engineVersion",
		)
	})

	t.Run("Ascend P/D uses MooncakeConnectorV1 on empty or <0.30.0 and MooncakeConnectorV2 at >=0.30.0", func(t *testing.T) {
		ascendAT := newLegacyRenderAscendAcceleratorType()
		qty1 := resource.MustParse("1")
		cases := []struct {
			name          string
			engineImage   string
			engineVersion string
			wantConnector string
		}{
			{
				name:          "empty legacy version stays on V1 even when image tag is runner 0.32.0",
				engineImage:   "gpustack/runner:cann9.2-a3-vllm0.32.0",
				engineVersion: "",
				wantConnector: "MooncakeConnectorV1",
			},
			{
				name:          "explicit 0.23.0 renders V1",
				engineImage:   "quay.io/ascend/vllm-ascend:v0.23.0",
				engineVersion: "0.23.0",
				wantConnector: "MooncakeConnectorV1",
			},
			{
				name:          "explicit 0.30.0rc1 renders V2",
				engineImage:   "gpustack/runner:cann9.2-a3-vllm0.30.0rc1",
				engineVersion: "0.30.0rc1",
				wantConnector: "MooncakeConnectorV2",
			},
			{
				name:          "explicit 0.32.0 renders V2",
				engineImage:   "gpustack/runner:cann9.2-a3-vllm0.32.0",
				engineVersion: "0.32.0",
				wantConnector: "MooncakeConnectorV2",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				md := newRenderDeployment(func(md *workercore.ModelDeployment) {
					md.Spec.KVCache = nil
					md.Spec.Engine.Image = tc.engineImage
					md.Spec.Engine.Version = tc.engineVersion
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
					md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendHIXL}
					md.Spec.Roles = []workercore.ModelDeploymentRole{
						{
							Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
							AcceleratorType: ascendAT.Name, Replicas: 1, ReplicaSize: 1,
							Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
						{
							Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
							AcceleratorType: ascendAT.Name, Replicas: 1, ReplicaSize: 1,
							Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
						},
					}
				})
				cli := newModelDeploymentClient(md, ascendAT)
				r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
				pods, err := r.renderModelDeploymentRolePods(context.Background(), md, nil, nil, nil, false)
				require.NoError(t, err)

				for _, roleName := range []string{"prefill", "decode"} {
					cmd := strings.Join(pods[roleName][0][0].Spec.Containers[0].Command, " ")
					assert.Contains(t, cmd, `"kv_connector":"`+tc.wantConnector+`"`)
				}
				prefillCmd := strings.Join(pods["prefill"][0][0].Spec.Containers[0].Command, " ")
				assert.Contains(t, prefillCmd, `"topic":"kv@pod@Qwen/Qwen2.5-72B-Instruct"`)
			})
		}
	})

	t.Run("vLLM EPD ECCPUConnector defaults VLLM_USE_V2_MODEL_RUNNER below 0.34.0, omits at 0.34.0, and allows user overrides", func(t *testing.T) {
		assert.True(t, ModelDeploymentDefaultsEnv(inject.VLLMUseV2ModelRunnerEnv))
		assert.True(t, ModelDeploymentDefaultsEnv(inject.VLLMECSideChannelPortEnv))

		at := newRenderAcceleratorType()
		qty1 := resource.MustParse("1")
		buildEPD := func(version string, userEnv []workercore.ModelDeploymentEnvVar) *workercore.ModelDeployment {
			return newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
				md.Spec.Engine.Version = version
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendNIXL}
				md.Spec.Roles = []workercore.ModelDeploymentRole{
					{
						Name: "encode", Kind: workercore.ModelDeploymentRoleKindEncode,
						AcceleratorType: at.Name, Replicas: 1, ReplicaSize: 1, Env: userEnv,
						Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
					},
					{
						Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
						AcceleratorType: at.Name, Replicas: 1, ReplicaSize: 1, Env: userEnv,
						Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
					},
					{
						Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
						AcceleratorType: at.Name, Replicas: 1, ReplicaSize: 1,
						Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
					},
				}
			})
		}

		md032 := buildEPD("0.32.0", nil)
		cli032 := newModelDeploymentClient(md032, at)
		r032 := &ModelDeploymentReconciler{Client: cli032, APIReader: cli032}
		pods032, err := r032.renderModelDeploymentRolePods(context.Background(), md032, nil, nil, nil, false)
		require.NoError(t, err)
		for _, roleName := range []string{"encode", "prefill"} {
			v2, hasV2 := envValue(pods032[roleName][0][0], inject.VLLMUseV2ModelRunnerEnv)
			assert.True(t, hasV2, roleName)
			assert.Equal(t, "1", v2, roleName)
		}
		encodeCtr032 := pods032["encode"][0][0].Spec.Containers[0]
		port032, hasPort032 := envValue(pods032["encode"][0][0], inject.VLLMECSideChannelPortEnv)
		assert.True(t, hasPort032)
		assert.Equal(t, "5601", port032)
		assert.NotNil(t, envByName(encodeCtr032.Env, inject.VLLMECSideChannelHostEnv))

		md034 := buildEPD("0.34.0", nil)
		cli034 := newModelDeploymentClient(md034, at)
		r034 := &ModelDeploymentReconciler{Client: cli034, APIReader: cli034}
		pods034, err := r034.renderModelDeploymentRolePods(context.Background(), md034, nil, nil, nil, false)
		require.NoError(t, err)
		for _, roleName := range []string{"encode", "prefill"} {
			_, hasV2 := envValue(pods034[roleName][0][0], inject.VLLMUseV2ModelRunnerEnv)
			assert.False(t, hasV2, "%s at 0.34.0 must omit retired VLLM_USE_V2_MODEL_RUNNER", roleName)
		}
		port034, hasPort034 := envValue(pods034["encode"][0][0], inject.VLLMECSideChannelPortEnv)
		assert.True(t, hasPort034)
		assert.Equal(t, "5601", port034)

		mdOverride := buildEPD("0.32.0", []workercore.ModelDeploymentEnvVar{
			{Name: inject.VLLMUseV2ModelRunnerEnv, Value: "0"},
			{Name: inject.VLLMECSideChannelPortEnv, Value: "5609"},
		})
		cliOverride := newModelDeploymentClient(mdOverride, at)
		rOverride := &ModelDeploymentReconciler{Client: cliOverride, APIReader: cliOverride}
		podsOverride, err := rOverride.renderModelDeploymentRolePods(context.Background(), mdOverride, nil, nil, nil, false)
		require.NoError(t, err)
		for _, roleName := range []string{"encode", "prefill"} {
			v2, _ := envValue(podsOverride[roleName][0][0], inject.VLLMUseV2ModelRunnerEnv)
			port, _ := envValue(podsOverride[roleName][0][0], inject.VLLMECSideChannelPortEnv)
			assert.Equal(t, "0", v2, roleName)
			assert.Equal(t, "5609", port, roleName)
		}
		assert.True(t, ModelDeploymentDefaultsEnv(inject.VLLMUseV2ModelRunnerEnv))
		assert.True(t, ModelDeploymentDefaultsEnv(inject.VLLMECSideChannelPortEnv))

		// Encode HTTP on 5601 renders cleanly without port conflict when VLLM_EC_SIDE_CHANNEL_PORT is 0 or 5602.
		for _, tc := range []struct {
			name             string
			sideChannelValue string
			wantPorts        []core.ContainerPort
		}{
			{
				name:             "ephemeral 0 omits ec-side-channel container port",
				sideChannelValue: "0",
				wantPorts: []core.ContainerPort{
					{Name: "tcp-5601", ContainerPort: 5601, Protocol: core.ProtocolTCP},
				},
			},
			{
				name:             "custom 5602 emits ec-side-channel on 5602 alongside HTTP on 5601",
				sideChannelValue: "5602",
				wantPorts: []core.ContainerPort{
					{Name: "tcp-5601", ContainerPort: 5601, Protocol: core.ProtocolTCP},
					{Name: "ec-side-channel", ContainerPort: 5602, Protocol: core.ProtocolTCP},
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				mdPort := buildEPD("0.34.0", nil)
				mdPort.Spec.Roles[0].Ports = []workercore.ModelDeploymentPort{
					{Port: 5601, Protocol: core.ProtocolTCP},
				}
				mdPort.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
					{Name: inject.VLLMECSideChannelPortEnv, Value: tc.sideChannelValue},
				}
				cliPort := newModelDeploymentClient(mdPort, at)
				rPort := &ModelDeploymentReconciler{Client: cliPort, APIReader: cliPort}
				podsPort, err := rPort.renderModelDeploymentRolePods(context.Background(), mdPort, nil, nil, nil, false)
				require.NoError(t, err)
				encodePod := podsPort["encode"][0][0]
				assert.Equal(t, tc.wantPorts, encodePod.Spec.Containers[0].Ports)
				gotEnv, ok := envValue(encodePod, inject.VLLMECSideChannelPortEnv)
				assert.True(t, ok)
				assert.Equal(t, tc.sideChannelValue, gotEnv)
			})
		}
	})
}

func TestModelDeploymentVLLMRouterKVAware_WiringHashAndScalerGate(t *testing.T) {
	at := newRenderAcceleratorType()
	qty1 := resource.MustParse("1")

	renderPDPods := func(t *testing.T, extraArgs []string, userEnv []workercore.ModelDeploymentEnvVar) (*workercore.ModelDeployment, map[string]map[int][]*core.Pod) {
		t.Helper()
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Engine.Version = "0.32.0"
			md.Spec.Router = &workercore.ModelDeploymentRouter{
				Name:      workercore.ModelDeploymentRouterVLLM,
				ExtraArgs: extraArgs,
			}
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{
					Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
					AcceleratorType: at.Name, Replicas: 1, ReplicaSize: 1, Env: userEnv,
					Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				},
				{
					Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
					AcceleratorType: at.Name, Replicas: 1, ReplicaSize: 1, Env: userEnv,
					Resources: &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				},
			}
		})
		cli := newModelDeploymentClient(md, at)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		pods, err := r.renderModelDeploymentRolePods(context.Background(), md, nil, nil, nil, false)
		require.NoError(t, err)
		return md, pods
	}

	hasContainerPort := func(pod *core.Pod, port int32) bool {
		for _, p := range pod.Spec.Containers[0].Ports {
			if p.ContainerPort == port {
				return true
			}
		}
		return false
	}

	userOverride := []workercore.ModelDeploymentEnvVar{
		{Name: inject.VLLMKVEventsUseIntBlockHashesEnv, Value: "1"},
	}

	// 1. Default cache_aware: no KV events, user VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES=1 is preserved,
	//    and unrelated router.extraArgs edits leave both role pod-spec-hashes untouched.
	mdBase, basePods := renderPDPods(t, nil, userOverride)
	_, unrelatedPods := renderPDPods(t, []string{"--max-concurrent-requests", "256"}, userOverride)
	for _, roleName := range []string{"prefill", "decode"} {
		pod := basePods[roleName][0][0]
		cmd := strings.Join(pod.Spec.Containers[0].Command, " ")
		assert.NotContains(t, cmd, "--kv-events-config")
		assert.NotContains(t, cmd, inject.VLLMPrefixCachingHashAlgoArg)
		assert.False(t, hasContainerPort(pod, 5557))
		assert.False(t, hasContainerPort(pod, 5558))
		val, ok := envValue(pod, inject.VLLMKVEventsUseIntBlockHashesEnv)
		assert.True(t, ok)
		assert.Equal(t, "1", val, "non-kv_aware role must preserve user VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES")
		assert.Equal(t,
			pod.Annotations[modelDeploymentPodSpecHashAnnotation],
			unrelatedPods[roleName][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
			"unrelated router.extraArgs must not roll %s Pods", roleName,
		)
	}
	baseSvcs := renderModelDeploymentServices(mdBase, nil, nil)
	assert.Len(t, baseSvcs[1].Spec.Ports, 1)
	assert.Len(t, baseSvcs[2].Spec.Ports, 1)

	// 2. --prefill-policy kv_aware: ONLY prefill gets KV events, sha256_cbor, stripped env=0, 5557/5558,
	//    and a moved pod-spec-hash; decode stays untouched!
	mdPrefillKV, prefillKVPods := renderPDPods(t, []string{"--prefill-policy", "kv_aware"}, userOverride)
	prefillPod := prefillKVPods["prefill"][0][0]
	decodePod := prefillKVPods["decode"][0][0]
	prefillCmd := strings.Join(prefillPod.Spec.Containers[0].Command, " ")
	assert.Contains(t, prefillCmd, `"topic":"kv@pod@Qwen/Qwen2.5-72B-Instruct"`)
	assert.Contains(t, prefillCmd, "--prefix-caching-hash-algo sha256_cbor")
	assert.True(t, hasContainerPort(prefillPod, 5557))
	assert.True(t, hasContainerPort(prefillPod, 5558))
	prefillIntHash, ok := envValue(prefillPod, inject.VLLMKVEventsUseIntBlockHashesEnv)
	assert.True(t, ok)
	assert.Equal(t, "0", prefillIntHash, "kv_aware role must strip user override and enforce 0")

	decodeIntHash, ok := envValue(decodePod, inject.VLLMKVEventsUseIntBlockHashesEnv)
	assert.True(t, ok)
	assert.Equal(t, "1", decodeIntHash, "inactive decode role must keep user env")
	assert.NotEqual(t,
		basePods["prefill"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
		prefillPod.Annotations[modelDeploymentPodSpecHashAnnotation],
		"enabling --prefill-policy kv_aware must roll prefill",
	)
	assert.Equal(t,
		basePods["decode"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
		decodePod.Annotations[modelDeploymentPodSpecHashAnnotation],
		"enabling --prefill-policy kv_aware must NOT roll decode",
	)

	// 3. --policy=kv_aware and --decode-policy=kv_aware in PD mode: vllm-router@v0.1.17 subscribes
	//    to Prefill workers only and never queries KV placement on Decode, so Decode never gets KV
	//    events, ports 5557/5558, sha256_cbor, or status.router.roles[decode].kvEvents.
	mdPolicyKV, policyKVPods := renderPDPods(t, []string{"--policy=kv_aware"}, userOverride)
	polDecPod := policyKVPods["decode"][0][0]
	polDecCmd := strings.Join(polDecPod.Spec.Containers[0].Command, " ")
	assert.NotContains(t, polDecCmd, "--kv-events-config")
	assert.NotContains(t, polDecCmd, inject.VLLMPrefixCachingHashAlgoArg)
	assert.False(t, hasContainerPort(polDecPod, 5557))
	assert.False(t, hasContainerPort(polDecPod, 5558))
	assert.Equal(t,
		basePods["decode"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
		polDecPod.Annotations[modelDeploymentPodSpecHashAnnotation],
		"enabling --policy=kv_aware must NOT roll decode",
	)

	polSvcs := renderModelDeploymentServices(mdPolicyKV, nil, nil)
	assert.Len(t, polSvcs[1].Spec.Ports, 3, "prefill Service exposes 8000 + 5557 + 5558 under --policy=kv_aware")
	assert.Len(t, polSvcs[2].Spec.Ports, 1, "decode Service never exposes KV event ports")

	routerObjs, err := renderModelDeploymentRouterObjects(context.Background(), mdPolicyKV, nil, false)
	require.NoError(t, err)
	require.Len(t, routerObjs.Contract.Roles, 2)
	require.NotNil(t, routerObjs.Contract.Roles[0].KVEvents)
	assert.Equal(t, "kv@pod@Qwen/Qwen2.5-72B-Instruct", routerObjs.Contract.Roles[0].KVEvents.Topic)
	assert.Nil(t, routerObjs.Contract.Roles[1].KVEvents)

	_, decodeKVPods := renderPDPods(t, []string{"--decode-policy=kv_aware"}, userOverride)
	assert.Equal(t,
		basePods["prefill"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
		decodeKVPods["prefill"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
	)
	assert.Equal(t,
		basePods["decode"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
		decodeKVPods["decode"][0][0].Annotations[modelDeploymentPodSpecHashAnnotation],
	)

	// Verify scaler operator env gate accepts VLLM_KV_EVENTS_USE_INT_BLOCK_HASHES on kv_aware roles.
	mdPrefillClean, _ := renderPDPods(t, []string{"--policy=kv_aware"}, nil)
	for i := range mdPrefillClean.Spec.Roles {
		require.NoError(t, modelDeploymentScalerOperatorEnvGateWithManufacturer(
			workercore.ModelDeploymentEngineVLLM, mdPrefillClean, &mdPrefillClean.Spec.Roles[i],
			nodefeature.ManufacturerNVIDIA, "",
		))
	}
	_ = mdPrefillKV
}

func TestModelDeploymentMultiDP_KVEventsReplayPort(t *testing.T) {
	at := newRenderAcceleratorType()
	qty2 := resource.MustParse("2")

	t.Run("vLLM single-Pod --data-parallel-size 2 offsets replay port to 5559 on Pod, Service, and llm-d-router epp.yaml", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Engine.Version = "0.32.0"
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			md.Spec.Roles = []workercore.ModelDeploymentRole{{
				Name:            "server",
				Kind:            workercore.ModelDeploymentRoleKindServer,
				AcceleratorType: at.Name,
				Replicas:        1,
				ReplicaSize:     1,
				ExtraArgs:       []string{"--data-parallel-size", "2"},
				Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty2)},
			}}
		})

		cli := newModelDeploymentClient(md, at)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		pods, err := r.renderModelDeploymentRolePods(context.Background(), md, nil, nil, nil, false)
		require.NoError(t, err)
		pod := pods["server"][0][0]
		cmd := strings.Join(pod.Spec.Containers[0].Command, " ")
		assert.Contains(t, cmd, `"endpoint":"tcp://*:5557"`)
		assert.Contains(t, cmd, `"replay_endpoint":"tcp://*:5559"`)
		assert.Contains(t, cmd, `"topic":"kv@pod@Qwen/Qwen2.5-72B-Instruct"`)
		assert.Contains(t, pod.Spec.Containers[0].Ports, core.ContainerPort{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: 5557})
		assert.Contains(t, pod.Spec.Containers[0].Ports, core.ContainerPort{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5559})

		svcs := renderModelDeploymentServices(md, nil, nil)
		assert.Contains(t, svcs[1].Spec.Ports, core.ServicePort{
			Name: "kv-replay", Protocol: core.ProtocolTCP, Port: 5559, TargetPort: intstr.FromInt32(5559),
		})

		routerObjs, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		assert.Contains(t, routerObjs.ConfigMap.Data[modelDeploymentRouterConfigKey], "socketPort: 5557")
		assert.Contains(t, routerObjs.ConfigMap.Data[modelDeploymentRouterConfigKey], "replaySocketPort: 5559")
		assert.Contains(t, routerObjs.ConfigMap.Data[modelDeploymentRouterConfigKey], "topicFilter: kv@")
		require.NotNil(t, routerObjs.Contract.Roles[0].KVEvents)
		assert.Equal(t, "tcp://qwen-server.team-a.svc:5559", routerObjs.Contract.Roles[0].KVEvents.ReplayEndpoint)
	})

	t.Run("SGLang single-Pod --dp-size 2 and --attn-dp-size 2 offset replay port to 5559", func(t *testing.T) {
		for _, extraArgs := range [][]string{{"--dp-size", "2"}, {"--attn-dp-size", "2"}} {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
				md.Spec.Engine = workercore.ModelDeploymentEngine{
					Name:    workercore.ModelDeploymentEngineSGLang,
					Image:   "gpustack/runner:cuda13.0-sglang0.5.23",
					Version: "0.5.23",
				}
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterSGL}
				md.Spec.Roles = []workercore.ModelDeploymentRole{{
					Name:            "server",
					Kind:            workercore.ModelDeploymentRoleKindServer,
					AcceleratorType: at.Name,
					Replicas:        1,
					ReplicaSize:     1,
					ExtraArgs:       extraArgs,
					Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty2)},
				}}
			})
			cli := newModelDeploymentClient(md, at)
			r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
			pods, err := r.renderModelDeploymentRolePods(context.Background(), md, nil, nil, nil, true)
			require.NoError(t, err)
			pod := pods["server"][0][0]
			cmd := strings.Join(pod.Spec.Containers[0].Command, " ")
			assert.Contains(t, cmd, `"endpoint":"tcp://*:5557"`)
			assert.Contains(t, cmd, `"replay_endpoint":"tcp://*:5559"`)
			assert.Contains(t, pod.Spec.Containers[0].Ports, core.ContainerPort{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5559})
		}
	})

	t.Run("SGLang --dp-size 4 --nnodes 2 --pp-size 2 keeps full global DP span (replay 5561) without dividing by nnodes", func(t *testing.T) {
		qty4 := resource.MustParse("4")
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Engine = workercore.ModelDeploymentEngine{
				Name:    workercore.ModelDeploymentEngineSGLang,
				Image:   "gpustack/runner:cuda13.0-sglang0.5.23",
				Version: "0.5.23",
			}
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterSGL}
			md.Spec.Roles = []workercore.ModelDeploymentRole{{
				Name:            "server",
				Kind:            workercore.ModelDeploymentRoleKindServer,
				AcceleratorType: at.Name,
				Replicas:        1,
				ReplicaSize:     2,
				ExtraArgs:       []string{"--dp-size", "4", "--nnodes", "2", "--pp-size", "2"},
				Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty4)},
			}}
		})
		assert.Equal(t, int32(4), ModelDeploymentKVEventsLocalDP(md, &md.Spec.Roles[0]))

		cli := newModelDeploymentClient(md, at)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		pods, err := r.renderModelDeploymentRolePods(context.Background(), md, nil, nil, nil, true)
		require.NoError(t, err)
		pod := pods["server"][0][0]
		cmd := strings.Join(pod.Spec.Containers[0].Command, " ")
		assert.Contains(t, cmd, `"endpoint":"tcp://*:5557"`)
		assert.Contains(t, cmd, `"replay_endpoint":"tcp://*:5561"`)
		assert.Contains(t, pod.Spec.Containers[0].Ports, core.ContainerPort{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5561})

		routerObjs, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		assert.Nil(t, routerObjs.Contract.Roles[0].KVEvents,
			"sgl-router role Services omit ZMQ ports so status.router.roles[].kvEvents stays nil")
	})

	t.Run("vLLM External DP and Hybrid DP offset replay to 5561 under vllm-router kv_aware and report NotApplicable under llm-d-router", func(t *testing.T) {
		shapes := []struct {
			name        string
			replicaSize int32
			extraArgs   []string
		}{
			{
				name:        "External DP",
				replicaSize: 4,
				extraArgs:   []string{"--data-parallel-size", "4", "--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"},
			},
			{
				name:        "Hybrid DP",
				replicaSize: 2,
				extraArgs:   []string{"--data-parallel-size", "4", "--data-parallel-size-local", "2", "--data-parallel-hybrid-lb"},
			},
		}
		for _, shape := range shapes {
			t.Run(shape.name, func(t *testing.T) {
				// 1. vllm-router kv_aware: publishes KV events with replay_endpoint tcp://*:5561.
				mdKVAware := newRenderDeployment(func(md *workercore.ModelDeployment) {
					md.Spec.KVCache = nil
					md.Spec.Engine.Version = "0.32.0"
					md.Spec.Router = &workercore.ModelDeploymentRouter{
						Name:      workercore.ModelDeploymentRouterVLLM,
						ExtraArgs: []string{"--policy=kv_aware"},
					}
					md.Spec.Roles = []workercore.ModelDeploymentRole{{
						Name:            "server",
						Kind:            workercore.ModelDeploymentRoleKindServer,
						AcceleratorType: at.Name,
						Replicas:        1,
						ReplicaSize:     shape.replicaSize,
						ExtraArgs:       shape.extraArgs,
						Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty2)},
					}}
				})
				assert.True(t, ModelDeploymentRolePublishesKVEvents(mdKVAware, &mdKVAware.Spec.Roles[0]))
				assert.Equal(t, int32(4), ModelDeploymentKVEventsLocalDP(mdKVAware, &mdKVAware.Spec.Roles[0]))

				cliKV := newModelDeploymentClient(mdKVAware, at)
				rKV := &ModelDeploymentReconciler{Client: cliKV, APIReader: cliKV}
				podsKV, err := rKV.renderModelDeploymentRolePods(context.Background(), mdKVAware, nil, nil, nil, false)
				require.NoError(t, err)
				podKV := podsKV["server"][0][0]
				cmdKV := strings.Join(podKV.Spec.Containers[0].Command, " ")
				assert.Contains(t, cmdKV, `"endpoint":"tcp://*:5557"`)
				assert.Contains(t, cmdKV, `"replay_endpoint":"tcp://*:5561"`)
				assert.Contains(t, podKV.Spec.Containers[0].Ports, core.ContainerPort{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5561})

				holderKV := new(workercore.ModelDeployment)
				observeModelDeploymentKVEvents(mdKVAware, holderKV, nil)
				assert.Equal(t, "True", ModelDeploymentConditionKVEventsPublishing.GetStatus(holderKV))
				assert.Equal(t, modelDeploymentReasonKVEventsPublishing, ModelDeploymentConditionKVEventsPublishing.GetReason(holderKV))

				// 2. llm-d-router: disables broken single-rank ZMQ subscriptions (NotApplicable).
				mdLLMD := mdKVAware.DeepCopy()
				mdLLMD.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				assert.False(t, ModelDeploymentRolePublishesKVEvents(mdLLMD, &mdLLMD.Spec.Roles[0]))

				cliLLMD := newModelDeploymentClient(mdLLMD, at)
				rLLMD := &ModelDeploymentReconciler{Client: cliLLMD, APIReader: cliLLMD}
				podsLLMD, err := rLLMD.renderModelDeploymentRolePods(context.Background(), mdLLMD, nil, nil, nil, false)
				require.NoError(t, err)
				podLLMD := podsLLMD["server"][0][0]
				cmdLLMD := strings.Join(podLLMD.Spec.Containers[0].Command, " ")
				assert.NotContains(t, cmdLLMD, "--kv-events-config")
				for _, p := range podLLMD.Spec.Containers[0].Ports {
					assert.NotEqual(t, int32(5557), p.ContainerPort)
					assert.NotEqual(t, int32(5561), p.ContainerPort)
				}

				routerObjs, err := renderModelDeploymentRouterObjects(context.Background(), mdLLMD, nil, false)
				require.NoError(t, err)
				assert.Contains(t, routerObjs.ConfigMap.Data[modelDeploymentRouterConfigKey], "approx-prefix-cache-producer")
				assert.NotContains(t, routerObjs.ConfigMap.Data[modelDeploymentRouterConfigKey], "precise-prefix-cache-producer")
				assert.Nil(t, routerObjs.Contract.Roles[0].KVEvents)

				holderLLMD := new(workercore.ModelDeployment)
				observeModelDeploymentKVEvents(mdLLMD, holderLLMD, nil)
				assert.Equal(t, "True", ModelDeploymentConditionKVEventsPublishing.GetStatus(holderLLMD))
				assert.Equal(t, modelDeploymentReasonKVEventsNotApplicable, ModelDeploymentConditionKVEventsPublishing.GetReason(holderLLMD))
			})
		}
	})

	t.Run("llm-d-router computes ReplayPort stride strictly across KV-event-publishing roles", func(t *testing.T) {
		qty1 := resource.MustParse("1")
		qty4 := resource.MustParse("4")

		cases := []struct {
			name               string
			roles              []workercore.ModelDeploymentRole
			expectedLocalDP    int32
			expectedReplayPort int32
			publishingRoles    []string
		}{
			{
				name: "EPD Encode(dp=1) Prefill(dp=2) Decode(dp=1) uses replayPort 5559",
				roles: []workercore.ModelDeploymentRole{
					{
						Name:            "prefill",
						Kind:            workercore.ModelDeploymentRoleKindPrefill,
						AcceleratorType: at.Name,
						Replicas:        1,
						ReplicaSize:     1,
						ExtraArgs:       []string{"--data-parallel-size", "2"},
						Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty2)},
					},
					{
						Name:            "decode",
						Kind:            workercore.ModelDeploymentRoleKindDecode,
						AcceleratorType: at.Name,
						Replicas:        1,
						ReplicaSize:     1,
						Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
					},
				},
				expectedLocalDP:    2,
				expectedReplayPort: 5559,
				publishingRoles:    []string{"prefill"},
			},
			{
				name: "PD Decode(dp=4) Prefill(dp=1) uses replayPort 5558",
				roles: []workercore.ModelDeploymentRole{
					{
						Name:            "decode",
						Kind:            workercore.ModelDeploymentRoleKindDecode,
						AcceleratorType: at.Name,
						Replicas:        1,
						ReplicaSize:     1,
						ExtraArgs:       []string{"--data-parallel-size", "4"},
						Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty4)},
					},
					{
						Name:            "prefill",
						Kind:            workercore.ModelDeploymentRoleKindPrefill,
						AcceleratorType: at.Name,
						Replicas:        1,
						ReplicaSize:     1,
						Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
					},
				},
				expectedLocalDP:    1,
				expectedReplayPort: 5558,
				publishingRoles:    []string{"prefill"},
			},
			{
				name: "multi-Server ServerA(dp=1) ServerB(dp=2) agrees on replayPort 5559",
				roles: []workercore.ModelDeploymentRole{
					{
						Name:            "server-a",
						Kind:            workercore.ModelDeploymentRoleKindServer,
						AcceleratorType: at.Name,
						Replicas:        1,
						ReplicaSize:     1,
						Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
					},
					{
						Name:            "server-b",
						Kind:            workercore.ModelDeploymentRoleKindServer,
						AcceleratorType: at.Name,
						Replicas:        1,
						ReplicaSize:     1,
						ExtraArgs:       []string{"--data-parallel-size", "2"},
						Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty2)},
					},
				},
				expectedLocalDP:    2,
				expectedReplayPort: 5559,
				publishingRoles:    []string{"server-a", "server-b"},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				md := newRenderDeployment(func(md *workercore.ModelDeployment) {
					md.Spec.KVCache = nil
					md.Spec.Engine.Version = "0.32.0"
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
					md.Spec.Roles = tc.roles
				})
				assert.Equal(t, tc.expectedLocalDP, ModelDeploymentLLMDRouterKVEventsLocalDP(md))

				cli := newModelDeploymentClient(md, at)
				r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
				pods, err := r.renderModelDeploymentRolePods(context.Background(), md, nil, nil, nil, false)
				require.NoError(t, err)

				svcs := renderModelDeploymentServices(md, nil, nil)
				svcByName := map[string]*core.Service{}
				for _, s := range svcs {
					svcByName[s.Name] = s
				}

				routerObjs, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
				require.NoError(t, err)
				eppYAML := routerObjs.ConfigMap.Data[modelDeploymentRouterConfigKey]
				assert.Contains(t, eppYAML, "socketPort: 5557")
				assert.Contains(t, eppYAML, fmt.Sprintf("replaySocketPort: %d", tc.expectedReplayPort))

				contractByRole := map[string]workercore.ModelDeploymentRouterRoleStatus{}
				for _, rs := range routerObjs.Contract.Roles {
					contractByRole[rs.Name] = rs
				}

				expectedReplayArg := fmt.Sprintf(`"replay_endpoint":"tcp://*:%d"`, tc.expectedReplayPort)
				for _, roleName := range tc.publishingRoles {
					pod := pods[roleName][0][0]
					cmd := strings.Join(pod.Spec.Containers[0].Command, " ")
					assert.Contains(t, cmd, `"endpoint":"tcp://*:5557"`)
					assert.Contains(t, cmd, expectedReplayArg)
					assert.Contains(t, pod.Spec.Containers[0].Ports, core.ContainerPort{
						Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: 5557,
					})
					assert.Contains(t, pod.Spec.Containers[0].Ports, core.ContainerPort{
						Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: tc.expectedReplayPort,
					})

					roleSvc := svcByName[md.Name+"-"+roleName]
					require.NotNil(t, roleSvc)
					assert.Contains(t, roleSvc.Spec.Ports, core.ServicePort{
						Name: "kv-replay", Protocol: core.ProtocolTCP,
						Port: tc.expectedReplayPort, TargetPort: intstr.FromInt32(tc.expectedReplayPort),
					})

					rs := contractByRole[roleName]
					require.NotNil(t, rs.KVEvents)
					assert.Equal(t,
						fmt.Sprintf("tcp://%s-%s.%s.svc:%d", md.Name, roleName, md.Namespace, tc.expectedReplayPort),
						rs.KVEvents.ReplayEndpoint,
					)
				}
			})
		}
	})
}

func TestModelDeploymentSGLRouter_EndToEndWiringAndReconcile(t *testing.T) {
	at := newRenderAcceleratorType()
	qty1 := resource.MustParse("1")

	t.Run("store-less single Server behind sgl-router publishes kv-events, stamps role-kind on role Service only, and configures Envoy metrics/observer", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Engine = workercore.ModelDeploymentEngine{
				Name:    workercore.ModelDeploymentEngineSGLang,
				Image:   "gpustack/runner:cuda13.0-sglang0.5.23",
				Version: "0.5.23",
			}
			md.Spec.Router = &workercore.ModelDeploymentRouter{
				Name: workercore.ModelDeploymentRouterSGL,
			}
			md.Spec.Roles = []workercore.ModelDeploymentRole{{
				Name:            "main",
				Kind:            workercore.ModelDeploymentRoleKindServer,
				AcceleratorType: at.Name,
				Replicas:        2,
				ReplicaSize:     2,
				ExtraArgs:       []string{"--attn-dp-size", "2", "--nnodes", "2"},
				Env:             []workercore.ModelDeploymentEnvVar{{Name: modelDeploymentSGLangRustServerEnv, Value: "1"}},
				Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
			}}
		})

		cli := newModelDeploymentClient(md, at)
		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		// Verify rendered SGLang worker Pod has --kv-events-config with topic "kv-events" and 5557/5559 ports (attn-dp-size=2).
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		pods, err := r.renderModelDeploymentRolePods(context.Background(), md, nil, nil, nil, true)
		require.NoError(t, err)
		workerPod := pods["main"][0][0]
		workerCmd := strings.Join(workerPod.Spec.Containers[0].Command, " ")
		assert.Contains(t, workerCmd, "--kv-events-config")
		assert.Contains(t, workerCmd, `"topic":"kv-events"`)
		assert.Contains(t, workerPod.Spec.Containers[0].Ports, core.ContainerPort{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: 5557})
		assert.Contains(t, workerPod.Spec.Containers[0].Ports, core.ContainerPort{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5559})

		// Verify Services: ONLY qwen-main has role-kind=server; qwen, qwen-main-r0/r1, and qwen-router do NOT.
		// sgl-router per-role Services must expose ONLY the HTTP serving port (no 5557/5559) so
		// Kubernetes EndpointSlice MD5 port sorting never puts a ZMQ port at ports[0].
		svcList := new(core.ServiceList)
		require.NoError(t, cli.List(context.Background(), svcList, ctrlcli.InNamespace(md.Namespace)))
		byName := map[string]*core.Service{}
		for i := range svcList.Items {
			byName[svcList.Items[i].Name] = &svcList.Items[i]
		}
		require.Contains(t, byName, "qwen")
		require.Contains(t, byName, "qwen-main")
		require.Contains(t, byName, "qwen-main-r0")
		require.Contains(t, byName, "qwen-router")
		assert.Equal(t, "server", byName["qwen-main"].Labels[modelDeploymentLabelKeyRoleKind])
		assert.NotContains(t, byName["qwen"].Labels, modelDeploymentLabelKeyRoleKind)
		assert.NotContains(t, byName["qwen-main-r0"].Labels, modelDeploymentLabelKeyRoleKind)
		assert.NotContains(t, byName["qwen-router"].Labels, modelDeploymentLabelKeyRoleKind)
		assert.Equal(t, []core.ServicePort{{
			Name:       modelDeploymentDefaultPortName,
			Protocol:   core.ProtocolTCP,
			Port:       modelDeploymentDefaultPort,
			TargetPort: intstr.FromInt32(modelDeploymentDefaultPort),
		}}, byName["qwen-main"].Spec.Ports, "sgl-router role Service must expose only the HTTP serving port")

		// Verify Role RBAC grants discovery.k8s.io/endpointslices get,list,watch.
		roleObj := new(rbac.Role)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "qwen-router"}, roleObj))
		assert.Equal(t, []rbac.PolicyRule{{
			APIGroups: []string{"discovery.k8s.io"},
			Resources: []string{"endpointslices"},
			Verbs:     []string{"get", "list", "watch"},
		}}, roleObj.Rules)

		// Verify Router Objects: Rust-server External DP skips engine-sentinel so sgl-router runs
		// single-container on :8000 without Envoy while still carrying --dp-aware, --no-tokenizer,
		// and EndpointSlice role-kind=server selection.
		rustRouterObjs, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		assert.Nil(t, rustRouterObjs.Contract.Roles[0].KVEvents,
			"sgl-router role Services omit ZMQ ports so status.router.roles[].kvEvents must not advertise unreachable Service endpoints")
		assert.Equal(t, "8000", rustRouterObjs.Deployment.Spec.Template.Annotations["prometheus.io/port"])
		require.Len(t, rustRouterObjs.Deployment.Spec.Template.Spec.Containers, 1)
		rustRouterCtr := rustRouterObjs.Deployment.Spec.Template.Spec.Containers[0]
		assert.Equal(t, []string{"sgl-router"}, rustRouterCtr.Command)
		assert.Contains(t, rustRouterCtr.Args, "--dp-aware")
		assert.Contains(t, rustRouterCtr.Args, "--no-tokenizer")
		assert.Contains(t, rustRouterCtr.Args, modelDeploymentLabelKeyRoleKind+"=server")

		// Verify Python SGLang DP (--dp-size 2 without SGLANG_RUST_SERVER) injects engine-sentinel
		// and wraps sgl-router in the Envoy loopback sidecar (:9091 observer probe, :9090 metrics
		// listener, --dp-aware, --no-tokenizer, EndpointSlice selector).
		mdPython := md.DeepCopy()
		mdPython.Spec.Roles[0].Env = nil
		mdPython.Spec.Roles[0].ReplicaSize = 1
		mdPython.Spec.Roles[0].ExtraArgs = []string{"--dp-size", "2"}
		routerObjs, err := renderModelDeploymentRouterObjects(context.Background(), mdPython, nil, false)
		require.NoError(t, err)
		assert.Nil(t, routerObjs.Contract.Roles[0].KVEvents)

		podTpl := routerObjs.Deployment.Spec.Template
		assert.Equal(t, "9090", podTpl.Annotations["prometheus.io/port"])
		require.Len(t, podTpl.Spec.Containers, 2)
		envoyCtr := podTpl.Spec.Containers[0]
		routerCtr := podTpl.Spec.Containers[1]
		assert.Contains(t, envoyCtr.Ports, core.ContainerPort{Name: "http", ContainerPort: 8000})
		assert.Contains(t, envoyCtr.Ports, core.ContainerPort{Name: "metrics", ContainerPort: 9090})
		assert.Equal(t, []core.ContainerPort{{Name: "http-upstream", ContainerPort: 8001}}, routerCtr.Ports)
		assert.Equal(t, intstr.FromInt32(9091), routerCtr.ReadinessProbe.HTTPGet.Port)
		assert.Equal(t, observerURLPath, routerCtr.ReadinessProbe.HTTPGet.Path)

		assert.Equal(t, []string{"sgl-router"}, routerCtr.Command)
		assert.Contains(t, routerCtr.Args, "--host=127.0.0.1")
		assert.Contains(t, routerCtr.Args, "--port=8001")
		assert.Contains(t, routerCtr.Args, "--dp-aware")
		assert.Contains(t, routerCtr.Args, "--no-tokenizer")
		assert.Contains(t, routerCtr.Args, modelDeploymentLabelKeyRoleKind+"=server")
		for _, arg := range routerCtr.Args {
			assert.NotContains(t, arg, modelDeploymentMemberIndexLabel)
			assert.NotContains(t, arg, modelDeploymentLabelKeyEndpointEligible)
		}

		// Verify Envoy config forwards GET /metrics on :9090 and blocks GET /metrics on :8000.
		var parsed parsedEnvoyBootstrap
		require.NoError(t, yaml.Unmarshal([]byte(routerObjs.ConfigMap.Data[modelDeploymentRouterEnvoyConfigKey]), &parsed))
		require.Len(t, parsed.StaticResources.Listeners, 3)
		metricsListener := parsed.StaticResources.Listeners[2]
		assert.Equal(t, int32(9090), metricsListener.Address.SocketAddress.PortValue)
		metricsHCM := metricsListener.FilterChains[0].Filters[0].TypedConfig
		assert.True(t, simulateEnvoyRoute(t, metricsHCM, "GET", "/metrics").Forwarded)
		assert.Equal(t, 403, simulateEnvoyRoute(t, metricsHCM, "POST", "/metrics").Status)
		assert.Equal(t, 403, simulateEnvoyRoute(t, metricsHCM, "GET", "/v1/models").Status)

		// Verify non-proxy single-container sgl-router advertises port 8000 and omits --no-tokenizer when cache_aware is in extraArgs.
		mdNoProxy := mdPython.DeepCopy()
		mdNoProxy.Spec.Router.ExtraArgs = []string{"--policy=cache_aware"}
		noProxyObjs, err := renderModelDeploymentRouterObjectsWithProxy(context.Background(), mdNoProxy, nil, false, false)
		require.NoError(t, err)
		assert.Equal(t, "8000", noProxyObjs.Deployment.Spec.Template.Annotations["prometheus.io/port"])
		require.Len(t, noProxyObjs.Deployment.Spec.Template.Spec.Containers, 1)
		singleCtr := noProxyObjs.Deployment.Spec.Template.Spec.Containers[0]
		assert.Equal(t, []core.ContainerPort{{Name: "http", ContainerPort: 8000}}, singleCtr.Ports)
		assert.NotContains(t, singleCtr.Args, "--no-tokenizer")
	})

	t.Run("EPD behind sgl-router emits --kv-events-config before --encoder-urls on Prefill only and matches per-role EndpointSlice labels", func(t *testing.T) {
		md := epdDeployment(t, []workercore.ModelDeploymentRoleKind{
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
		}, func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Engine = workercore.ModelDeploymentEngine{
				Name:    workercore.ModelDeploymentEngineSGLang,
				Image:   "gpustack/runner:cuda13.0-sglang0.5.23",
				Version: "0.5.23",
			}
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterSGL}
			md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendMooncake}
			for i := range md.Spec.Roles {
				md.Spec.Roles[i].Replicas = 1
				md.Spec.Roles[i].Image = ""
				md.Spec.Roles[i].ExtraArgs = nil
				md.Spec.Roles[i].Ports = nil
				md.Spec.Roles[i].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)}
			}
		})

		cli := newModelDeploymentClient(md, at)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		pods, err := r.renderModelDeploymentRolePods(context.Background(), md, nil, nil, nil, true)
		require.NoError(t, err)

		prefillCmd := pods["prefill"][0][0].Spec.Containers[0].Command
		kvIdx := slices.Index(prefillCmd, "--kv-events-config")
		encIdx := slices.Index(prefillCmd, "--encoder-urls")
		require.NotEqual(t, -1, kvIdx, "prefill must carry --kv-events-config")
		require.NotEqual(t, -1, encIdx, "prefill must carry --encoder-urls")
		assert.Less(t, kvIdx, encIdx, "--kv-events-config must precede nargs='+' --encoder-urls")

		assert.NotContains(t, pods["decode"][0][0].Spec.Containers[0].Command, "--kv-events-config")
		assert.NotContains(t, pods["encode"][0][0].Spec.Containers[0].Command, "--kv-events-config")
		assert.Contains(t, pods["prefill"][0][0].Spec.Containers[0].Ports, core.ContainerPort{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: 5557})
		assert.Contains(t, pods["prefill"][0][0].Spec.Containers[0].Ports, core.ContainerPort{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5558})

		svcs := renderModelDeploymentServices(md, nil, nil)
		svcByName := map[string]*core.Service{}
		for _, s := range svcs {
			svcByName[s.Name] = s
		}
		assert.NotContains(t, svcByName["qwen"].Labels, modelDeploymentLabelKeyRoleKind)
		assert.Equal(t, "encode", svcByName["qwen-encode"].Labels[modelDeploymentLabelKeyRoleKind])
		assert.Equal(t, "prefill", svcByName["qwen-prefill"].Labels[modelDeploymentLabelKeyRoleKind])
		assert.Equal(t, "decode", svcByName["qwen-decode"].Labels[modelDeploymentLabelKeyRoleKind])
		assert.Equal(t, []core.ServicePort{{
			Name:       modelDeploymentDefaultPortName,
			Protocol:   core.ProtocolTCP,
			Port:       modelDeploymentDefaultPort,
			TargetPort: intstr.FromInt32(modelDeploymentDefaultPort),
		}}, svcByName["qwen-prefill"].Spec.Ports, "sgl-router prefill Service must expose only the HTTP serving port")

		routerObjs, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		args := routerContainerArgs(routerObjs.Deployment.Spec.Template.Spec.Containers)
		assert.Contains(t, args, "--prefill-selector")
		assert.Contains(t, args, "--decode-selector")
		assert.Contains(t, args, modelDeploymentLabelKeyRoleKind+"=prefill")
		assert.Contains(t, args, modelDeploymentLabelKeyRoleKind+"=decode")
		assert.NotContains(t, args, modelDeploymentLabelKeyRoleKind+"=encode")
		assert.NotContains(t, args, "--dp-aware")

		// When a PD role declares attention DP (--attn-dp-size >= 2 or --dp-size >= 2), sgl-router emits --dp-aware.
		mdPDDP := md.DeepCopy()
		mdPDDP.Spec.Roles[2].ExtraArgs = []string{"--tp-size", "2", "--attn-dp-size", "2"}
		routerObjsDP, err := renderModelDeploymentRouterObjects(context.Background(), mdPDDP, nil, false)
		require.NoError(t, err)
		assert.Contains(t, routerContainerArgs(routerObjsDP.Deployment.Spec.Template.Spec.Containers), "--dp-aware")
	})

	t.Run("role Service retains endpoint-eligible=true and role-kind across quiet reconciles", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Engine = workercore.ModelDeploymentEngine{
				Name:    workercore.ModelDeploymentEngineSGLang,
				Image:   "gpustack/runner:cuda13.0-sglang0.5.23",
				Version: "0.5.23",
			}
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterSGL}
			md.Spec.Roles = []workercore.ModelDeploymentRole{{
				Name:            "server",
				Kind:            workercore.ModelDeploymentRoleKindServer,
				AcceleratorType: at.Name,
				Replicas:        1,
				ReplicaSize:     1,
				Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
			}}
		})
		cli := newModelDeploymentClient(md, at)
		r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
		// Initial pass creates Services; second pass records decided eligibility; third quiet pass retains it.
		require.NoError(t, r.syncModelDeploymentService(context.Background(), md, false, nil))
		require.NoError(t, r.syncModelDeploymentService(context.Background(), md, true, nil))
		require.NoError(t, r.syncModelDeploymentRouter(context.Background(), md, true))

		// Quiet reconcile pass with eligibilityDecided=false must retain endpoint-eligible=true on qwen-server.
		require.NoError(t, r.syncModelDeploymentService(context.Background(), md, false, nil))
		require.NoError(t, r.syncModelDeploymentRouter(context.Background(), md, false))

		roleSvc := new(core.Service)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "qwen-server"}, roleSvc))
		assert.Equal(t, "server", roleSvc.Labels[modelDeploymentLabelKeyRoleKind])
		assert.Equal(t, modelDeploymentEndpointEligibleValue, roleSvc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible])

		// sgl-router container receives writable HOME and HF_HOME in /tmp for --policy=cache_aware tokenizers.
		routerObjs, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		var routerCtr *core.Container
		for i := range routerObjs.Deployment.Spec.Template.Spec.Containers {
			if routerObjs.Deployment.Spec.Template.Spec.Containers[i].Name == "router" {
				routerCtr = &routerObjs.Deployment.Spec.Template.Spec.Containers[i]
				break
			}
		}
		require.NotNil(t, routerCtr)
		assert.Contains(t, routerCtr.Env, core.EnvVar{Name: "HOME", Value: "/tmp"})
		assert.Contains(t, routerCtr.Env, core.EnvVar{Name: "HF_HOME", Value: "/tmp/huggingface"})
	})

	t.Run("vllm-router PD does not require endpoint-eligible=true when GroupForward is Unsupported", func(t *testing.T) {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.KVCache = nil
			md.Spec.Engine.Version = "0.34.0"
			md.Spec.Router = &workercore.ModelDeploymentRouter{
				Name:      workercore.ModelDeploymentRouterVLLM,
				ExtraArgs: []string{"--policy=kv_aware"},
			}
			md.Spec.Roles = []workercore.ModelDeploymentRole{
				{
					Name:            "prefill",
					Kind:            workercore.ModelDeploymentRoleKindPrefill,
					AcceleratorType: at.Name,
					Replicas:        1,
					ReplicaSize:     1,
					Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				},
				{
					Name:            "decode",
					Kind:            workercore.ModelDeploymentRoleKindDecode,
					AcceleratorType: at.Name,
					Replicas:        1,
					ReplicaSize:     1,
					Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: ptr.To(qty1)},
				},
			}
		})
		assert.False(t, modelDeploymentEligibilitySelectorActive(md, &md.Spec.Roles[0], nil, false))
		assert.False(t, modelDeploymentEligibilitySelectorActive(md, &md.Spec.Roles[1], nil, false))

		routerObjs, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
		require.NoError(t, err)
		args := strings.Join(routerContainerArgs(routerObjs.Deployment.Spec.Template.Spec.Containers), " ")
		assert.NotContains(t, args, modelDeploymentLabelKeyEndpointEligible)
	})
}
