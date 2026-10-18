package worker

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// epdSharedClaim is the directory an ec-example leg carries its embeddings through. The encoder and
// the prefiller mount one claim here, which is the shape that row admits.
var epdSharedClaim = workercore.ModelDeploymentAdditionalVolume{
	MountPath:             "/shared/encodings",
	PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "encodings"},
}

// epdCommandOf reconciles a full encoder deployment on one platform and returns each role's serving
// Pod. The reconciler is what produces these, so the assertions below observe the whole chain --
// the row, the leg, the render and the sidecar -- rather than the renderer called with a hand-built
// input that already contains the answer being tested.
//
// The serving port is uniform because the router requires it to be: a deployment whose roles
// disagree on the port is refused before any of this is rendered.
func epdCommandOf(
	t *testing.T, engine, manufacturer string, order []workercore.ModelDeploymentRoleKind,
	mutate ...func(*workercore.ModelDeployment),
) map[string]*core.Pod {
	t.Helper()

	// The engine's image is the fallback every role in this fixture overrides, so it only has to be
	// an image the engine actually runs: a deployment naming the wrong one would be a fixture that
	// cannot be applied.
	image := "vllm/vllm-openai:v0.30.0"
	if engine == workercore.ModelDeploymentEngineSGLang {
		image = "lmsysorg/sglang:v0.5.18"
	}
	md := epdDeployment(t, order)
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	md.Spec.Engine = workercore.ModelDeploymentEngine{Name: engine, Image: image}
	for i := range md.Spec.Roles {
		md.Spec.Roles[i].Replicas = 1
		md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{{Port: 8000, Protocol: core.ProtocolTCP}}
		md.Spec.Roles[i].ExtraArgs = nil
	}
	for _, m := range mutate {
		m(md)
	}

	cli := newModelDeploymentClient(md, newRenderInstanceType(
		func(it *worker.InstanceType) { it.Status.Detail.Manufacturer = manufacturer }))
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	pods := map[string]*core.Pod{}
	for i := range replicaPods(t, cli) {
		pod := replicaPods(t, cli)[i]
		pods[modelDeploymentPodRole(&pod)] = pod.DeepCopy()
	}

	return pods
}

// commandValue returns the entry following flag on the command line, or empty when the flag is
// absent. A JSON document is returned verbatim; the caller decides whether to decode it.
func commandValue(command []string, flag string) string {
	at := slices.Index(command, flag)
	if at < 0 || at+1 >= len(command) {
		return ""
	}

	return command[at+1]
}

// sidecarArgs returns the arguments of the named init container, or nil when the Pod runs none.
func sidecarArgs(pod *core.Pod, name string) []string {
	for _, container := range pod.Spec.InitContainers {
		if container.Name == name {
			return container.Args
		}
	}

	return nil
}

// hasArgPrefix reports whether any entry starts with prefix, so a flag spelled with "=" is found
// without pinning the separator the renderer happened to choose.
func hasArgPrefix(args []string, prefix string) bool {
	return slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, prefix) })
}

// TestModelDeploymentEncoderStage_RendersTheAdmittedRow is the whole wiring under test: an admitted
// combination must reach BOTH halves of the encoder stage and the decode sidecar, each from the one
// row, in the vocabulary its own process reads.
//
// THE ROW IS THE ONLY INPUT. Every expectation below is read back through the table rather than
// written as a literal, so a case that passed because the renderer hardcoded a value would fail
// here the moment the table said something else. The two literals that remain are the encoder-cache
// connector names themselves, which are what the engine resolves and what a user reads in an engine
// log.
func TestModelDeploymentEncoderStage_RendersTheAdmittedRow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		router       string
		manufacturer string
		claim        bool
		connector    string
		sidecar      string
		wantArgs     []string
		wantEnv      map[string][]string
		absentEnv    []string
	}{
		{
			name: "vllm_on_rocm_pulls_over_the_network", router: workercore.ModelDeploymentRouterLLMD,
			manufacturer: nodefeature.ManufacturerAMD,
			connector:    ModelDeploymentECConnectorCPU, sidecar: modelDeploymentECConnectorNIXL,
			// The network leg has no directory, so it states the buffer the connector requires
			// and the flag that makes it read the side channel instead of a claim.
			wantArgs: []string{`"ec_enable_nixl":true`, `"ec_cpu_bytes":1073741824`},
			// KEYED BY ROLE because the two halves do not own the same keys: the side channel is
			// published by the producer and dialed by the consumer, so it belongs to the encoder
			// alone.
			wantEnv: map[string][]string{
				"encode": {
					"VLLM_USE_V2_MODEL_RUNNER", "VLLM_EC_SIDE_CHANNEL_HOST",
					"VLLM_EC_SIDE_CHANNEL_PORT",
				},
				"prefill": {"VLLM_USE_V2_MODEL_RUNNER"},
			},
		},
		{
			name: "vllm_on_nvidia_pulls_over_the_network", router: workercore.ModelDeploymentRouterLLMD,
			manufacturer: nodefeature.ManufacturerNVIDIA,
			connector:    ModelDeploymentECConnectorCPU, sidecar: modelDeploymentECConnectorNIXL,
			wantArgs: []string{`"ec_enable_nixl":true`, `"ec_cpu_bytes":1073741824`},
			wantEnv: map[string][]string{
				"encode": {
					"VLLM_USE_V2_MODEL_RUNNER", "VLLM_EC_SIDE_CHANNEL_HOST",
					"VLLM_EC_SIDE_CHANNEL_PORT",
				},
				"prefill": {"VLLM_USE_V2_MODEL_RUNNER"},
			},
		},
		{
			name:         "vllm_behind_vllm_router_on_rocm_pulls_over_the_network",
			router:       workercore.ModelDeploymentRouterVLLM,
			manufacturer: nodefeature.ManufacturerAMD,
			connector:    ModelDeploymentECConnectorCPU,
			wantArgs:     []string{`"ec_enable_nixl":true`, `"ec_cpu_bytes":1073741824`},
			wantEnv: map[string][]string{
				"encode": {
					"VLLM_USE_V2_MODEL_RUNNER", "VLLM_EC_SIDE_CHANNEL_HOST",
					"VLLM_EC_SIDE_CHANNEL_PORT",
				},
				"prefill": {"VLLM_USE_V2_MODEL_RUNNER"},
			},
		},
		{
			name:         "vllm_behind_vllm_router_on_nvidia_pulls_over_the_network",
			router:       workercore.ModelDeploymentRouterVLLM,
			manufacturer: nodefeature.ManufacturerNVIDIA,
			connector:    ModelDeploymentECConnectorCPU,
			wantArgs:     []string{`"ec_enable_nixl":true`, `"ec_cpu_bytes":1073741824`},
			wantEnv: map[string][]string{
				"encode": {
					"VLLM_USE_V2_MODEL_RUNNER", "VLLM_EC_SIDE_CHANNEL_HOST",
					"VLLM_EC_SIDE_CHANNEL_PORT",
				},
				"prefill": {"VLLM_USE_V2_MODEL_RUNNER"},
			},
		},
		{
			name: "vllm_on_ascend_reads_the_shared_directory", router: workercore.ModelDeploymentRouterLLMD,
			manufacturer: nodefeature.ManufacturerAscend,
			claim:        true,
			connector:    ModelDeploymentECConnectorExample, sidecar: modelDeploymentECConnectorExample,
			wantArgs: []string{`"shared_storage_path":"/shared/encodings"`},
			// THE SHARED ROW OWNS NO ENVIRONMENT AT ALL: the two ends meet in a directory, so
			// there is no address to publish and no socket to bind.
			wantEnv: map[string][]string{},
			// THE MODEL RUNNER IS THE NETWORK ROW'S PRECONDITION AND NOTHING ELSE ON THIS ONE.
			// vllm-ascend's second model runner carries no multimodal encoder path, so an encoder
			// running on it starts, serves nothing, and reports no error naming the cause. Every
			// role is checked rather than the encoder alone, because the runner is a property of
			// the row and not of the half of the leg a role carries.
			absentEnv: []string{"VLLM_USE_V2_MODEL_RUNNER"},
		},
	} {
		for _, position := range []struct {
			name  string
			order []workercore.ModelDeploymentRoleKind
		}{
			{
				name: "encode_first",
				order: []workercore.ModelDeploymentRoleKind{
					workercore.ModelDeploymentRoleKindEncode,
					workercore.ModelDeploymentRoleKindPrefill,
					workercore.ModelDeploymentRoleKindDecode,
				},
			},
			{
				name: "encode_middle",
				order: []workercore.ModelDeploymentRoleKind{
					workercore.ModelDeploymentRoleKindPrefill,
					workercore.ModelDeploymentRoleKindEncode,
					workercore.ModelDeploymentRoleKindDecode,
				},
			},
			{
				name: "encode_last",
				order: []workercore.ModelDeploymentRoleKind{
					workercore.ModelDeploymentRoleKindDecode,
					workercore.ModelDeploymentRoleKindPrefill,
					workercore.ModelDeploymentRoleKindEncode,
				},
			},
		} {
			t.Run(tc.name+"/"+position.name, func(t *testing.T) {
				mutate := []func(*workercore.ModelDeployment){
					func(md *workercore.ModelDeployment) {
						md.Spec.Router.Name = tc.router
					},
				}
				if tc.claim {
					mutate = append(mutate, mountSharedClaimOnEncoderAndPrefill)
				}

				pods := epdCommandOf(t, workercore.ModelDeploymentEngineVLLM,
					tc.manufacturer, position.order, mutate...)
				require.Len(t, pods, 3, "every role renders a Pod, whatever its position")

				// THE PRODUCER AND THE CONSUMER CARRY THE SAME DOCUMENT BUT OPPOSITE HALVES. One
				// connector for the stage and one role each: a document carrying both halves would
				// have two processes answering for the same leg.
				encode := commandValue(pods["encode"].Spec.Containers[0].Command, "--ec-transfer-config")
				prefill := commandValue(pods["prefill"].Spec.Containers[0].Command, "--ec-transfer-config")
				require.NotEmpty(t, encode, "the encoder carries the producer half")
				require.NotEmpty(t, prefill, "the prefiller carries the consumer half")
				assert.Contains(t, encode, `"ec_role":"ec_producer"`)
				assert.Contains(t, prefill, `"ec_role":"ec_consumer"`)

				for _, half := range []string{encode, prefill} {
					assert.Contains(t, half, `"ec_connector":"`+tc.connector+`"`,
						"both halves are named from the one admitted row")
					for _, want := range tc.wantArgs {
						assert.Contains(t, half, want)
					}
				}

				// The decoder runs the sidecar under llm-d-router alone, and it is the sidecar
				// that reads the flag. Under vllm-router the router itself drives the three-stage
				// flow, so the decoder renders no sidecar at all.
				if tc.sidecar != "" {
					assert.Contains(t, sidecarArgs(pods["decode"], "routing-proxy"),
						"--ec-connector="+ModelDeploymentECSidecarConnector(
							ModelDeploymentEPDConnector(tc.router,
								workercore.ModelDeploymentEngineVLLM, tc.manufacturer)),
						"the sidecar reads the admitted row, not the platform")
				} else {
					assert.Nil(t, sidecarArgs(pods["decode"], "routing-proxy"),
						"vllm-router drives the three stages directly without a decode sidecar")
				}

				// The decoder holds no half of the encoder stage: it reads the embeddings out of
				// the KV cache the prefiller already built.
				assert.Empty(t, commandValue(pods["decode"].Spec.Containers[0].Command,
					"--ec-transfer-config"), "the decoder carries no half of the encoder leg")

				for role, want := range tc.wantEnv {
					for _, name := range want {
						assert.True(t, hasEnv(pods[role].Spec.Containers[0].Env, name),
							"%s renders %s", role, name)
					}
				}

				for _, name := range tc.absentEnv {
					for role, pod := range pods {
						assert.False(t, hasEnv(pod.Spec.Containers[0].Env, name),
							"%s must not render %s on this row", role, name)
					}
				}
			})
		}
	}
}

// mountSharedClaimOnEncoderAndPrefill attaches the one directory an ec-example leg carries its
// embeddings through to both roles that mount it, which is the pair admission compares.
func mountSharedClaimOnEncoderAndPrefill(md *workercore.ModelDeployment) {
	for i := range md.Spec.Roles {
		switch ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) {
		case workercore.ModelDeploymentRoleKindEncode, workercore.ModelDeploymentRoleKindPrefill:
			md.Spec.Roles[i].AdditionalVolumes = []workercore.ModelDeploymentAdditionalVolume{
				epdSharedClaim,
			}
		}
	}
}

// hasEnv reports whether the container declares the named variable.
func hasEnv(env []core.EnvVar, name string) bool {
	return slices.ContainsFunc(env, func(e core.EnvVar) bool { return e.Name == name })
}

// TestModelDeploymentEncoderStage_GivesBothHalvesTheSameDirectory pins the property that makes the
// shared leg work at all: two Pods carrying two different numbers would each look at its own
// directory and the embeddings would never arrive.
func TestModelDeploymentEncoderStage_GivesBothHalvesTheSameDirectory(t *testing.T) {
	pods := epdCommandOf(t, workercore.ModelDeploymentEngineVLLM,
		nodefeature.ManufacturerAscend,
		[]workercore.ModelDeploymentRoleKind{
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
		},
		mountSharedClaimOnEncoderAndPrefill)

	pathOf := func(role string) string {
		var document struct {
			Extra struct {
				SharedStoragePath string `json:"shared_storage_path"`
			} `json:"ec_connector_extra_config"`
		}
		document.Extra.SharedStoragePath = ""
		require.NoError(t, json.Unmarshal([]byte(
			commandValue(pods[role].Spec.Containers[0].Command, "--ec-transfer-config")), &document))

		return document.Extra.SharedStoragePath
	}

	// THE ENCODER'S OWN MOUNT IS THE ONE BOTH READ, not each role's. The two are equal here
	// because admission refuses a deployment whose claims disagree, so reading one of them keeps
	// the render from becoming a second place the pair could be spelled differently.
	require.Equal(t, epdSharedClaim.MountPath, pathOf("encode"))
	assert.Equal(t, pathOf("encode"), pathOf("prefill"),
		"both ends of the leg must name the same directory or the embeddings never arrive")
}

// epdRoleOrders are the three positions the encoder can hold in a deployment's role list.
//
// THE ORDER IS AN AXIS THE RESOLVER HAS TO GET RIGHT, not a cosmetic variant: the encoder's Service
// address is built by scanning the list for the Encode kind, so a resolver that answered with the
// first role would be right in exactly one of the three and wrong in the other two -- silently,
// handing the prefiller a URL naming a role that serves no embeddings.
var epdRoleOrders = []struct {
	name  string
	order []workercore.ModelDeploymentRoleKind
}{
	{
		name: "encode_first",
		order: []workercore.ModelDeploymentRoleKind{
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
		},
	},
	{
		name: "encode_middle",
		order: []workercore.ModelDeploymentRoleKind{
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindDecode,
		},
	},
	{
		name: "encode_last",
		order: []workercore.ModelDeploymentRoleKind{
			workercore.ModelDeploymentRoleKindDecode,
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindEncode,
		},
	},
}

// TestModelDeploymentEncoderStage_SGLangTakesAddressesRatherThanAConnector pins the other
// transport, which is the engine's own and not a row's, in every role order.
//
// THE ENGINE IS NOT SPLIT BY A CONNECTOR HERE: it runs the encoder as a server of its own and is
// handed the addresses to dial, so there is no matrix row to name a connector and the decode
// sidecar carries no encoder flag at all. The asymmetry is the engine's, and it is the reason the
// encoder's reachability cannot be gated on a row that names a connector.
//
// SGLANG IS THE ENGINE THAT CAN OBSERVE THE ADDRESS SELECTION. The vLLM renderers ignore
// `EncoderURLs` outright, so their three orderings prove the roles render but cannot prove which
// role's URL was chosen. This case is the one that reads the value back, and it is built in all
// three orders for that reason.
func TestModelDeploymentEncoderStage_SGLangTakesAddressesRatherThanAConnector(t *testing.T) {
	for _, row := range []struct {
		name         string
		router       string
		manufacturer string
	}{
		{
			name:         "llmd_on_nvidia",
			router:       workercore.ModelDeploymentRouterLLMD,
			manufacturer: nodefeature.ManufacturerNVIDIA,
		},
		{
			name:         "sglang_gateway_on_ascend",
			router:       workercore.ModelDeploymentRouterSGLang,
			manufacturer: nodefeature.ManufacturerAscend,
		},
	} {
		for _, position := range epdRoleOrders {
			t.Run(row.name+"/"+position.name, func(t *testing.T) {
				pods := epdCommandOf(t, workercore.ModelDeploymentEngineSGLang,
					row.manufacturer, position.order, func(md *workercore.ModelDeployment) {
						md.Spec.Router.Name = row.router
					})
				require.Len(t, pods, 3)

				// The encoder builds no language model of its own; without this flag it starts as a
				// server that serves nothing the prefiller asks for.
				assert.Contains(t, pods["encode"].Spec.Containers[0].Command, "--encoder-only")

				// ONE URL PER SERVICE, not per replica: the address is what a consumer inside the
				// cluster can resolve, and a replica address would be one Pod's worth of an encoder
				// stage the caller has no way to choose between.
				//
				// THE URL NAMES THE ENCODER, NOT THE FIRST ROLE. Every role here serves on 8000, so
				// only the name distinguishes them, and a resolver answering with role 0 would hand
				// the prefiller a Service that resolves to a prefiller or a decoder.
				prefill := pods["prefill"].Spec.Containers[0].Command
				assert.Contains(t, prefill, "--language-only")
				require.Equal(t, "http://qwen-encode:8000", commandValue(prefill, "--encoder-urls"),
					"the prefiller dials the encoder's Service, whichever position the encoder holds")
				assert.Equal(t, "prefill", commandValue(prefill, "--disaggregation-mode"),
					"the prefiller still renders its half of the P/D split on every admitted row")

				decode := pods["decode"].Spec.Containers[0].Command
				assert.Equal(t, "decode", commandValue(decode, "--disaggregation-mode"),
					"the decoder renders its half of the P/D split beside the language-only prefiller")

				// The decoder reads the embeddings out of the KV cache the prefiller already built,
				// so it is handed neither half.
				assert.NotContains(t, prefill, "--encoder-only")
				for _, arg := range sidecarArgs(pods["decode"], "routing-proxy") {
					assert.NotContains(t, arg, "--ec-connector",
						"this engine splits its encoder by address, and the sidecar carries no such flag")
				}
			})
		}
	}
}

// TestModelDeploymentEncoderStage_LeavesADeploymentWithoutAnEncoderAlone is the compatibility half.
// Every field the encoder stage adds is zero for a deployment declaring no Encode role, which is
// what keeps a prefill/decode pair rendering exactly as it did before any of this existed.
func TestModelDeploymentEncoderStage_LeavesADeploymentWithoutAnEncoderAlone(t *testing.T) {
	pods := epdCommandOf(t, workercore.ModelDeploymentEngineVLLM,
		nodefeature.ManufacturerAscend,
		[]workercore.ModelDeploymentRoleKind{
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
		})
	require.Len(t, pods, 2, "the deployment declares no encoder, so none is rendered")

	for role, pod := range pods {
		assert.Empty(t, commandValue(pod.Spec.Containers[0].Command, "--ec-transfer-config"),
			"%s renders no encoder leg from a deployment declaring no encoder", role)
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			assert.False(t, hasArgPrefix(container.Args, "--ec-connector"),
				"%s renders no sidecar encoder flag from a deployment declaring no encoder",
				container.Name)
		}
	}
}

// TestModelDeploymentEncoderStage_RefusesALegWithNoDirectoryAtAll is the loud direction. The
// shared leg names ONE directory both ends must arrive at, so a deployment where neither role
// mounts a claim cannot be given a path -- and the render refuses rather than emitting a leg whose
// two ends would each wait on a directory nobody mounted.
func TestModelDeploymentEncoderStage_RefusesALegWithNoDirectoryAtAll(t *testing.T) {
	md := epdDeployment(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindEncode,
		workercore.ModelDeploymentRoleKindPrefill,
		workercore.ModelDeploymentRoleKindDecode,
	})
	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	md.Spec.Engine = workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: "vllm/vllm-openai:v0.30.0"}
	for i := range md.Spec.Roles {
		md.Spec.Roles[i].Replicas = 1
		md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{{Port: 8000, Protocol: core.ProtocolTCP}}
		md.Spec.Roles[i].ExtraArgs = nil
	}

	cli := newModelDeploymentClient(md, ascendRenderInstanceType())
	_, err := reconcileModelDeployment(t, cli)
	require.Error(t, err,
		"a leg with no directory on either end cannot be rendered, and the reconcile says so")
	assert.Contains(t, err.Error(), "shared-directory encoder-cache connector",
		"the refusal names the leg rather than a file path")
}

// TestModelDeploymentEncoderStage_SGLangPrefillerGetsWiderProbes pins which roles carry the wider
// gates. A prefiller that dials the encoder by address answers its health route late while the
// encoder is busy or restarting, so only that role, in a deployment that declares an encoder, is
// given more room. Every other role keeps the defaults, which the legacy render pins byte for byte.
func TestModelDeploymentEncoderStage_SGLangPrefillerGetsWiderProbes(t *testing.T) {
	type gates struct{ timeout, readiness, liveness int32 }
	defaults := gates{
		timeout:   modelDeploymentProbeTimeoutSeconds,
		readiness: modelDeploymentReadinessFailureThreshold,
		liveness:  modelDeploymentLivenessFailureThreshold,
	}
	relaxed := gates{timeout: 5, readiness: 6, liveness: 12}

	testCases := []struct {
		name   string
		engine string
		order  []workercore.ModelDeploymentRoleKind
		want   map[string]gates
	}{
		{
			name:   "sglang with an encoder",
			engine: workercore.ModelDeploymentEngineSGLang,
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			want: map[string]gates{"encode": defaults, "prefill": relaxed, "decode": defaults},
		},
		{
			name:   "sglang without an encoder",
			engine: workercore.ModelDeploymentEngineSGLang,
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			want: map[string]gates{"prefill": defaults, "decode": defaults},
		},
		{
			name:   "vllm with an encoder",
			engine: workercore.ModelDeploymentEngineVLLM,
			order: []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			},
			want: map[string]gates{"encode": defaults, "prefill": defaults, "decode": defaults},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			manufacturer := nodefeature.ManufacturerAMD
			if tc.engine == workercore.ModelDeploymentEngineSGLang {
				manufacturer = nodefeature.ManufacturerNVIDIA
			}
			pods := epdCommandOf(t, tc.engine, manufacturer, tc.order)
			require.Len(t, pods, len(tc.want))

			for role, want := range tc.want {
				container := pods[role].Spec.Containers[0]
				require.NotNil(t, container.StartupProbe, role)
				require.NotNil(t, container.ReadinessProbe, role)
				require.NotNil(t, container.LivenessProbe, role)
				for _, probe := range []*core.Probe{
					container.StartupProbe, container.ReadinessProbe, container.LivenessProbe,
				} {
					assert.Equal(t, want.timeout, probe.TimeoutSeconds, role)
				}
				assert.Equal(t, want.readiness, container.ReadinessProbe.FailureThreshold, role)
				assert.Equal(t, want.liveness, container.LivenessProbe.FailureThreshold, role)
				assert.Equal(t, modelDeploymentStartupFailureThreshold,
					container.StartupProbe.FailureThreshold, "%s keeps its load budget", role)
			}
		})
	}
}

// TestModelDeploymentEncoderStage_TakenOverPrefillerKeepsTheDefaultProbes holds the wider gates to
// a command the operator built: a role whose command the user took over keeps the defaults.
func TestModelDeploymentEncoderStage_TakenOverPrefillerKeepsTheDefaultProbes(t *testing.T) {
	md := epdDeployment(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindEncode,
		workercore.ModelDeploymentRoleKindPrefill,
		workercore.ModelDeploymentRoleKindDecode,
	})
	md.Spec.Engine = workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineSGLang, Image: "lmsysorg/sglang:v0.5.18"}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		assert.Equal(t, role.Kind == workercore.ModelDeploymentRoleKindPrefill,
			modelDeploymentProbesEncoderStage(md, role, false), role.Name)
		assert.False(t, modelDeploymentProbesEncoderStage(md, role, true), role.Name)
	}
}

// TestModelDeploymentEncoderStage_PublishesTheSideChannelOnItsFixedPort is what the reserved port
// is reserved against. The producer binds a ZMQ ROUTER socket on a fixed number and publishes both
// halves of its address, so a consumer is configured by the operator rather than by the user and a
// Pod answering on that number would collide with it.
func TestModelDeploymentEncoderStage_PublishesTheSideChannelOnItsFixedPort(t *testing.T) {
	pods := epdCommandOf(t, workercore.ModelDeploymentEngineVLLM,
		nodefeature.ManufacturerAMD,
		[]workercore.ModelDeploymentRoleKind{
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
		})
	require.Len(t, pods, 3)

	container := pods["encode"].Spec.Containers[0]
	require.True(t, hasEnv(container.Env, "VLLM_EC_SIDE_CHANNEL_PORT"))
	require.True(t, hasEnv(container.Env, "VLLM_EC_SIDE_CHANNEL_HOST"))

	published := false
	for _, port := range container.Ports {
		if port.ContainerPort == inject.VLLMECSideChannelPort {
			published = true
		}
	}
	require.True(t, published,
		"the side channel is published on the port the consumer is told to dial")
}

// TestModelDeploymentEncoderStage_CoLocatesTheRolesOnOneHost pins the soft affinity a deployment
// declaring an encoder renders onto every replica.
//
// THE WEIGHTS FOLLOW THE TRANSFER LEG. Prefill and decode exchange KV cache on every token, so
// their pair is the one co-location buys the most and carries the largest weight; the encoder's
// output reaches the prefiller once per request, so that pair is worth less. Both are preferences
// and never requirements -- a cluster too tight to co-locate a pair still schedules it, merely
// spread out.
//
// THE EXPECTATIONS ARE WRITTEN AS LITERALS, not read back through the label algebra: the weight,
// the topology key and every selector entry are pinned here so a renderer that computed any of them
// differently fails by name.
func TestModelDeploymentEncoderStage_CoLocatesTheRolesOnOneHost(t *testing.T) {
	pods := epdCommandOf(t, workercore.ModelDeploymentEngineVLLM,
		nodefeature.ManufacturerAMD,
		[]workercore.ModelDeploymentRoleKind{
			workercore.ModelDeploymentRoleKindEncode,
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
		})
	require.Len(t, pods, 3)

	peerTerm := func(roleKind string, weight int32) core.WeightedPodAffinityTerm {
		return core.WeightedPodAffinityTerm{
			Weight: weight,
			PodAffinityTerm: core.PodAffinityTerm{
				LabelSelector: &meta.LabelSelector{MatchLabels: map[string]string{
					modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
					modelDeploymentLabelKeyInstance: "qwen",
					modelDeploymentLabelKeyRoleKind: roleKind,
				}},
				TopologyKey: "kubernetes.io/hostname",
			},
		}
	}

	for _, tc := range []struct {
		role string
		want []core.WeightedPodAffinityTerm
	}{
		{
			role: "encode",
			want: []core.WeightedPodAffinityTerm{
				peerTerm("prefill", 50),
			},
		},
		{
			role: "prefill",
			want: []core.WeightedPodAffinityTerm{
				peerTerm("decode", 100),
				peerTerm("encode", 50),
			},
		},
		{
			role: "decode",
			want: []core.WeightedPodAffinityTerm{
				peerTerm("prefill", 100),
			},
		},
	} {
		t.Run(tc.role, func(t *testing.T) {
			pod := pods[tc.role]
			require.NotNil(t, pod.Spec.Affinity,
				"%s carries a co-location preference", tc.role)
			require.NotNil(t, pod.Spec.Affinity.PodAffinity,
				"%s carries a co-location preference", tc.role)
			require.NotNil(t, pod.Spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution,
				"%s carries a co-location preference", tc.role)

			assert.Nil(t, pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
				"%s co-locates by preference, never by requirement", tc.role)
			assert.Equal(t, tc.want,
				pod.Spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution,
				"%s co-locates with exactly the peers the transfer leg pairs it with", tc.role)
		})
	}
}

// TestModelDeploymentCreateRoleOrder_IssuesTheEncoderLast pins the order a pass creates Pods in.
// The prefiller and the decoder carry the heavier co-location weight, so they must exist before the
// encoder is placed, whichever position the encoder holds in the declared list.
func TestModelDeploymentCreateRoleOrder_IssuesTheEncoderLast(t *testing.T) {
	for _, position := range epdRoleOrders {
		t.Run(position.name, func(t *testing.T) {
			md := epdDeployment(t, position.order)

			var wantPair []string
			got := make([]string, 0, len(position.order))
			for _, role := range modelDeploymentCreateRoleOrder(md) {
				got = append(got, role.Name)
			}
			for _, kind := range position.order {
				if kind != workercore.ModelDeploymentRoleKindEncode {
					wantPair = append(wantPair, strings.ToLower(string(kind)))
				}
			}
			assert.Equal(t, append(wantPair, "encode"), got,
				"the others keep their declared relative order and the encoder follows them")
		})
	}
}

// TestModelDeploymentCreateRoleOrder_LeavesADeploymentWithoutAnEncoderInDeclaredOrder holds the
// compatibility half: no encoder, no reordering.
func TestModelDeploymentCreateRoleOrder_LeavesADeploymentWithoutAnEncoderInDeclaredOrder(t *testing.T) {
	md := epdDeployment(t, []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindDecode,
		workercore.ModelDeploymentRoleKindPrefill,
	})

	got := make([]string, 0, len(md.Spec.Roles))
	for _, role := range modelDeploymentCreateRoleOrder(md) {
		got = append(got, role.Name)
	}
	assert.Equal(t, []string{"decode", "prefill"}, got)
}

// TestModelDeploymentEncoderStage_RendersNoPodAffinityWithoutAnEncoder is the compatibility half
// of the co-location preference. A prefill/decode pair runs no encoder stage, so it renders no
// affinity at all and its Pods stay byte-identical to what they rendered before the preference
// existed -- which is what keeps the legacy golden fixtures true.
func TestModelDeploymentEncoderStage_RendersNoPodAffinityWithoutAnEncoder(t *testing.T) {
	pods := epdCommandOf(t, workercore.ModelDeploymentEngineVLLM,
		nodefeature.ManufacturerAscend,
		[]workercore.ModelDeploymentRoleKind{
			workercore.ModelDeploymentRoleKindPrefill,
			workercore.ModelDeploymentRoleKindDecode,
		})
	require.Len(t, pods, 2, "the deployment declares no encoder, so none is rendered")

	for role, pod := range pods {
		assert.Nil(t, pod.Spec.Affinity,
			"%s renders no affinity from a deployment declaring no encoder", role)
	}
}

// TestModelDeploymentRouterObjects_TheEncoderListReachesTheVLLMRouterAlone observes the encoder
// stage through the whole render chain: the reconciled shape's router container carries the
// encoder endpoints as one document, and no other router carries one at all.
//
// THE URL IS READ BACK FROM THE CONTRACT, not restated here. The status publishes each role's
// Service endpoint, and the document must carry that same value -- anything else would be a second
// spelling of one address, and two spellings of one address is the drift this suite exists to
// catch.
func TestModelDeploymentRouterObjects_TheEncoderListReachesTheVLLMRouterAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		router string
		engine string
		encode bool
		want   []string
	}{
		{
			name:   "the vllm router with an encoder",
			router: workercore.ModelDeploymentRouterVLLM,
			engine: workercore.ModelDeploymentEngineVLLM,
			encode: true,
			want:   []string{"http://qwen-encode.team-a.svc:8000"},
		},
		{
			name:   "the vllm router without an encoder",
			router: workercore.ModelDeploymentRouterVLLM,
			engine: workercore.ModelDeploymentEngineVLLM,
			want:   nil,
		},
		{
			name:   "the gateway beside an encoder",
			router: workercore.ModelDeploymentRouterSGLang,
			engine: workercore.ModelDeploymentEngineSGLang,
			encode: true,
			want:   nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindPrefill,
				workercore.ModelDeploymentRoleKindDecode,
			}
			if tc.encode {
				order = append([]workercore.ModelDeploymentRoleKind{
					workercore.ModelDeploymentRoleKindEncode,
				}, order...)
			}
			md := epdDeployment(t, order)
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: tc.router}
			md.Spec.Engine = workercore.ModelDeploymentEngine{Name: tc.engine, Image: "vllm/vllm-openai:v0.30.0"}
			for i := range md.Spec.Roles {
				md.Spec.Roles[i].Replicas = 1
				md.Spec.Roles[i].Ports = []workercore.ModelDeploymentPort{
					{Port: 8000, Protocol: core.ProtocolTCP},
				}
				md.Spec.Roles[i].ExtraArgs = nil
			}

			objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
			require.NoError(t, err)

			routerContainer := objects.Deployment.Spec.Template.Spec.Containers[0]
			require.Equal(t, "router", routerContainer.Name,
				"an argv router renders its program in one container")

			document := ""
			for _, arg := range routerContainer.Args {
				if rest, found := strings.CutPrefix(arg, "--epd-config="); found {
					document = rest
				}
			}
			if tc.want == nil {
				assert.Empty(t, document, "no encoder document is rendered here")
				assert.Contains(t, routerContainer.Args, "--prefill-selector",
					"the positive control: the divided shape is rendered, minus the encoder stage")

				return
			}

			var carried struct {
				EncoderURLs []string `json:"encoder_urls"`
			}
			require.NoError(t, json.Unmarshal([]byte(document), &carried))
			for _, arg := range routerContainer.Args {
				assert.NotContains(t, arg, "consumer_zmq_addrs",
					"selector discovery cannot name prefillers, so the document claims none")
				assert.NotContains(t, arg, "--enable-igw",
					"the router stays out of the gateway mode upstream refuses beside EPD")
			}

			contractEndpoint := ""
			for _, role := range objects.Contract.Roles {
				if role.Name == "encode" {
					contractEndpoint = role.Endpoint
				}
			}
			require.NotEmpty(t, contractEndpoint, "the contract publishes the encoder role")
			assert.Equal(t, tc.want, carried.EncoderURLs)
			assert.Equal(t, []string{contractEndpoint}, carried.EncoderURLs,
				"the document carries the endpoint the contract publishes")
		})
	}
}
