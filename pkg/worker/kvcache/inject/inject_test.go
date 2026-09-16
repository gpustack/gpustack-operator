package inject

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// envNames returns the names of a rendered environment, in order.
func envNames(env []core.EnvVar) []string {
	out := make([]string, 0, len(env))
	for i := range env {
		out = append(out, env[i].Name)
	}
	return out
}

// envValue returns a rendered variable by name.
func envValue(t *testing.T, env []core.EnvVar, name string) core.EnvVar {
	t.Helper()

	for i := range env {
		if env[i].Name == name {
			return env[i]
		}
	}
	require.Failf(t, "variable not rendered", "%q is not among %v", name, envNames(env))
	return core.EnvVar{}
}

// TestRender_VLLMFamilyVehicleIsAFile covers the whole rendering for the engines that take a file, one
// row per role. It asserts the final Result rather than the calls that built it.
func TestRender_VLLMFamilyVehicleIsAFile(t *testing.T) {
	// wantConnector differs per engine and that is the point of carrying it as data: the two engines
	// share this renderer and this vehicle, but NOT a connector registry, and a single expected value
	// here would pass while one of them could not start. See vllmConnectorFor.
	testCases := []struct {
		name          string
		engine        Engine
		role          Role
		wantKVRole    string
		wantConnector string
	}{
		{name: "vllm, no role", engine: EngineVLLM, role: RoleNone, wantKVRole: "kv_both", wantConnector: "MooncakeStoreConnector"},
		{name: "vllm, prefill", engine: EngineVLLM, role: RolePrefill, wantKVRole: "kv_producer", wantConnector: "MooncakeStoreConnector"},
		{name: "vllm, decode", engine: EngineVLLM, role: RoleDecode, wantKVRole: "kv_consumer", wantConnector: "MooncakeStoreConnector"},
		{name: "vllm-ascend, no role", engine: EngineVLLMAscend, role: RoleNone, wantKVRole: "kv_both", wantConnector: "AscendStoreConnector"},
		{name: "vllm-ascend, prefill", engine: EngineVLLMAscend, role: RolePrefill, wantKVRole: "kv_producer", wantConnector: "AscendStoreConnector"},
		{name: "vllm-ascend, decode", engine: EngineVLLMAscend, role: RoleDecode, wantKVRole: "kv_consumer", wantConnector: "AscendStoreConnector"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(Input{
				Engine: tc.engine, Role: tc.role, Connection: testConnectionFor(tc.engine),
			})
			require.NoError(t, err)

			assert.Equal(t, []core.EnvVar{
				{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"},
			}, result.Env, "the variable names the file; the configuration is never in it")

			require.Len(t, result.Args, 2)
			assert.Equal(t, "--kv-transfer-config", result.Args[0])
			assert.JSONEq(t,
				`{"kv_connector":"`+tc.wantConnector+`","kv_role":"`+tc.wantKVRole+`"}`,
				result.Args[1])

			require.Len(t, result.Volumes, 1)
			volume := result.Volumes[0]
			require.NotNil(t, volume.DownwardAPI,
				"the file is projected from the Pod's own annotation, so the webhook creates no object")
			require.Len(t, volume.DownwardAPI.Items, 1)
			assert.Equal(t, "mooncake.json", volume.DownwardAPI.Items[0].Path)
			require.NotNil(t, volume.DownwardAPI.Items[0].FieldRef)
			assert.Equal(t, "metadata.annotations['kvcache.gpustack.ai/client-config']",
				volume.DownwardAPI.Items[0].FieldRef.FieldPath)

			assert.Equal(t, []core.VolumeMount{
				{Name: volume.Name, MountPath: "/etc/gpustack/kvcache", ReadOnly: true},
			}, result.VolumeMounts)

			assert.Contains(t, result.PodAnnotations, "kvcache.gpustack.ai/client-config",
				"the projection and the annotation it reads are two halves of one thing")
		})
	}
}

func TestRender_VLLMPublishesKVEvents(t *testing.T) {
	result, err := Render(Input{
		Engine:          EngineVLLM,
		Role:            RolePrefill,
		Connection:      testConnection(),
		PublishKVEvents: true,
		KVEventsHost:    "qwen-prefill.team-a.svc",
	})
	require.NoError(t, err)

	require.Len(t, result.Args, 4)
	assert.Equal(t, "--kv-events-config", result.Args[2])
	assert.JSONEq(t, `{
		"enable_kv_cache_events": true,
		"publisher": "zmq",
		"endpoint": "tcp://*:5557",
		"replay_endpoint": "tcp://*:5558",
		"buffer_steps": 10000,
		"hwm": 100000,
		"max_queue_size": 100000,
		"topic": "kv@"
	}`, result.Args[3])
	assert.Equal(t, []core.ContainerPort{
		{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: 5557},
		{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: 5558},
	}, result.Ports)
	require.NotNil(t, result.KVEvents)
	assert.Equal(t, "tcp://qwen-prefill.team-a.svc:5557", result.KVEvents.Endpoint)
	assert.Equal(t, "tcp://qwen-prefill.team-a.svc:5558", result.KVEvents.ReplayEndpoint)
	assert.Equal(t, "kv@", result.KVEvents.Topic)
}

func TestRender_VLLMDoesNotPublishKVEventsUnlessRequested(t *testing.T) {
	result, err := Render(Input{
		Engine: EngineVLLM, Role: RoleDecode, Connection: testConnection(),
	})
	require.NoError(t, err)

	assert.NotContains(t, result.Args, "--kv-events-config")
	assert.Empty(t, result.Ports)
	assert.Nil(t, result.KVEvents)
}

// TestRender_SGLangVehicleIsTheEnvironment is the counterpart, and its negative half carries as much
// weight as its positive one: an emitted config-path variable would push SGLang onto the file branch
// and void the whole injection.
func TestRender_SGLangVehicleIsTheEnvironment(t *testing.T) {
	result, err := Render(Input{Engine: EngineSGLang, Connection: testConnection()})
	require.NoError(t, err)

	assert.Empty(t, result.Volumes, "the environment is the whole vehicle")
	assert.Empty(t, result.VolumeMounts)
	assert.Empty(t, result.PodAnnotations, "nothing to project, so nothing to carry an annotation for")

	assert.NotContains(t, envNames(result.Env), "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH",
		"setting it would select the file branch, whose reader cannot resolve a Pod's IP")

	assert.Equal(t, []string{"--hicache-storage-backend", "mooncake"}, result.Args)
}

// TestRender_SGLangVariablesAreOnesTheEngineReads is the key-set gate on the environment side.
func TestRender_SGLangVariablesAreOnesTheEngineReads(t *testing.T) {
	result, err := Render(Input{Engine: EngineSGLang, Connection: testConnection()})
	require.NoError(t, err)

	rendered := sets.New(envNames(result.Env)...)
	assert.Empty(t, rendered.Difference(sglangReadableVariables).UnsortedList(),
		"these variables are not in SGLang's load_from_env, so nothing would read them")
}

// TestRender_SGLangMetadataVariableSpelling pins the name byte for byte, and deliberately not through
// the package's constant.
//
// META_DATA carries an underscore the readable METADATA does not. A misspelling does not error: the
// key falls back to its default and the metadata plane degrades silently. Asserting against the
// constant would pass whatever the constant held, which is exactly the failure being guarded.
func TestRender_SGLangMetadataVariableSpelling(t *testing.T) {
	result, err := Render(Input{Engine: EngineSGLang, Connection: testConnection()})
	require.NoError(t, err)

	assert.Contains(t, envNames(result.Env), "MOONCAKE_TE_META_DATA_SERVER")
	assert.NotContains(t, envNames(result.Env), "MOONCAKE_TE_METADATA_SERVER",
		"the readable spelling is the wrong one, and it fails silently")
}

// TestRender_SGLangLocalHostnameIsAFieldRef is the assertion the vehicle decision exists for. A literal
// here would be a value this package cannot know: a Pod has no IP when a mutating webhook runs.
func TestRender_SGLangLocalHostnameIsAFieldRef(t *testing.T) {
	result, err := Render(Input{Engine: EngineSGLang, Connection: testConnection()})
	require.NoError(t, err)

	hostname := envValue(t, result.Env, "MOONCAKE_LOCAL_HOSTNAME")
	assert.Empty(t, hostname.Value, "a literal would be a guess; the kubelet resolves this one")
	require.NotNil(t, hostname.ValueFrom)
	require.NotNil(t, hostname.ValueFrom.FieldRef)
	assert.Equal(t, "status.podIP", hostname.ValueFrom.FieldRef.FieldPath)
}

// TestRender_SGLangHasNoModeOrLocalBuffer pins the two keys SGLang's reader does not have. Emitting
// either would write something nothing reads, which is indistinguishable from working.
func TestRender_SGLangHasNoModeOrLocalBuffer(t *testing.T) {
	result, err := Render(Input{Engine: EngineSGLang, Connection: testConnection()})
	require.NoError(t, err)

	for _, absent := range []string{
		"MOONCAKE_MODE", "MOONCAKE_LOCAL_BUFFER_SIZE", "mode", "local_buffer_size",
	} {
		assert.NotContains(t, envNames(result.Env), absent)
	}
}

// TestRender_SGLangSegmentSizeIsAnExplicitZero. SGLang defaults an absent segment size to "4gb", so
// omitting the variable makes every client contribute 4 GiB of host memory it never requested.
func TestRender_SGLangSegmentSizeIsAnExplicitZero(t *testing.T) {
	result, err := Render(Input{Engine: EngineSGLang, Connection: testConnection()})
	require.NoError(t, err)

	assert.Equal(t, "0", envValue(t, result.Env, "MOONCAKE_GLOBAL_SEGMENT_SIZE").Value)
}

// TestRender_SGLangCarriesTheResolvedConnection.
// TestRender_SGLangTenantEnvNameFollowsEmission pins the second half of TenantEnvName's contract -
// "empty when none was produced". The field exists so a caller can tell "our precedence rule dropped
// the tenant" from "there was no tenant to drop", and those two are told apart by the variable being
// named but absent from the container. Naming a variable this render never wrote therefore invents a
// dropped tenant. The current caller only ever narrows its answer with this field, so it is unharmed
// either way; the contract is what the next caller reads.
func TestRender_SGLangTenantEnvNameFollowsEmission(t *testing.T) {
	testCases := []struct {
		name         string
		domain       string
		wantEnvName  string
		wantInjected bool
	}{
		{name: "a domain is emitted", domain: "team-a-chat", wantEnvName: "MOONCAKE_TENANT_ID", wantInjected: true},
		{name: "no domain, nothing emitted", domain: "", wantEnvName: "", wantInjected: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(Input{Engine: EngineSGLang, Domain: tc.domain, Connection: testConnection()})
			require.NoError(t, err)
			assert.Equal(t, tc.wantEnvName, result.TenantEnvName)
			assert.Equal(t, tc.wantInjected, result.TenantInjected,
				"the name and the action move together: a named variable is one this render wrote")
		})
	}
}

func TestRender_SGLangCarriesTheResolvedConnection(t *testing.T) {
	conn := testConnection()
	result, err := Render(Input{Engine: EngineSGLang, Connection: conn})
	require.NoError(t, err)

	assert.Equal(t, conn.MasterAddress, envValue(t, result.Env, "MOONCAKE_MASTER").Value)
	assert.Equal(t, conn.Protocol, envValue(t, result.Env, "MOONCAKE_PROTOCOL").Value)
	assert.Equal(t, "P2PHANDSHAKE", envValue(t, result.Env, "MOONCAKE_TE_META_DATA_SERVER").Value)

	device := envValue(t, result.Env, "MOONCAKE_DEVICE")
	assert.Equal(t, "", device.Value)
	assert.Nil(t, device.ValueFrom, "empty and written, not omitted")
}

// TestRender_TenantGoesToEveryEngineThatReadsOne pins the vehicle each engine uses for the reuse
// domain. The vLLM family reads tenant_id from its file; SGLang reads MOONCAKE_TENANT_ID.
func TestRender_TenantGoesToEveryEngineThatReadsOne(t *testing.T) {
	const domain = "team-a-chat"

	for _, engine := range Engines() {
		t.Run(string(engine), func(t *testing.T) {
			result, err := Render(Input{
				Engine: engine, Domain: domain, Connection: testConnectionFor(engine),
			})
			require.NoError(t, err)

			assert.True(t, result.TenantInjected, "the renderer reports the action it took")
			switch engine {
			case EngineSGLang:
				assert.Equal(t, domain, envValue(t, result.Env, "MOONCAKE_TENANT_ID").Value,
					"the reuse domain is what the engine is told to write under")
			default:
				assert.Equal(t, domain, renderedConfig(t, Input{
					Engine: engine, Domain: domain, Connection: testConnectionFor(engine),
				})["tenant_id"], "the reuse domain is written into the file the engine reads")
			}
		})
	}
}

// TestRender_TenantOmittedForAnEmptyDomain. An empty value is normalised back to the store default by
// the engine, so emitting one would be indistinguishable from not setting it - while still looking, on
// the Pod, like something was configured.
func TestRender_TenantOmittedForAnEmptyDomain(t *testing.T) {
	for _, engine := range Engines() {
		t.Run(string(engine), func(t *testing.T) {
			result, err := Render(Input{Engine: engine, Connection: testConnectionFor(engine)})
			require.NoError(t, err)

			assert.False(t, result.TenantInjected)
			assert.NotContains(t, envNames(result.Env), "MOONCAKE_TENANT_ID")
			if engine != EngineSGLang {
				assert.NotContains(t, renderedConfig(t, Input{
					Engine: engine, Connection: testConnectionFor(engine),
				}), "tenant_id", "an empty domain emits no tenant key")
			}
		})
	}
}

func TestRender_VLLMDirectTransferComposesPointToPointAndStore(t *testing.T) {
	for _, tc := range []struct {
		name      string
		role      Role
		pointRole string
		storeRole string
	}{
		{name: "prefill", role: RolePrefill, pointRole: "kv_producer", storeRole: "kv_both"},
		{name: "decode", role: RoleDecode, pointRole: "kv_consumer", storeRole: "kv_consumer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(Input{
				Engine: EngineVLLM, Role: tc.role, Domain: "team-a-chat",
				Connection: testConnection(), DirectTransfer: true,
			})
			require.NoError(t, err)
			require.Len(t, result.Args, 2)
			assert.Equal(t, vllmTransferConfigArg, result.Args[0])
			assert.JSONEq(t, fmt.Sprintf(`{
				"kv_connector":"MultiConnector",
				"kv_role":%q,
				"kv_connector_extra_config":{"connectors":[
					{"kv_connector":"MooncakeConnector","kv_role":%q,
					 "kv_connector_extra_config":{"mooncake_protocol":"tcp"}},
					{"kv_connector":"MooncakeStoreConnector","kv_role":%q}
				]}
			}`, tc.pointRole, tc.pointRole, tc.storeRole), result.Args[1])
			assert.True(t, result.DirectTransfer)
			for _, port := range result.Ports {
				assert.Empty(t, utilvalidation.IsValidPortName(port.Name),
					"rendered port %q must be accepted by the Kubernetes API", port.Name)
			}
		})
	}
}

func TestRender_VLLMDirectTransferWithoutStore(t *testing.T) {
	result, err := Render(Input{
		Engine: EngineVLLM, Role: RolePrefill, DirectTransfer: true,
	})
	require.NoError(t, err)
	require.Len(t, result.Args, 2)
	assert.Equal(t, vllmTransferConfigArg, result.Args[0])
	assert.JSONEq(t, `{
		"kv_connector":"MooncakeConnector",
		"kv_role":"kv_producer",
		"kv_connector_extra_config":{"mooncake_protocol":"tcp"}
	}`, result.Args[1])
	assert.True(t, result.DirectTransfer)
	assert.False(t, result.TenantInjected)
	assert.NotContains(t, envNames(result.Env), vllmConfigPathEnv)
	assert.Empty(t, result.Volumes)
	assert.Empty(t, result.VolumeMounts)
	assert.Empty(t, result.PodAnnotations)
	assert.Contains(t, envNames(result.Env), "VLLM_MOONCAKE_BOOTSTRAP_PORT")
}

// TestRender_DirectTransferProtocolIsNotTheMembers pins the split between the two data planes one
// backend feeds. The STORE plane keeps following the backend's transport; the direct
// prefill-to-decode leg does not read it, because it is engine to engine and never traverses the
// store. All three assertions ride on ONE backend, because the change being pinned is that one
// value's consumers came apart: an edit that switched BOTH legs to tcp -- or both to the members'
// transport -- would pass an assertion on either leg alone.
func TestRender_DirectTransferProtocolIsNotTheMembers(t *testing.T) {
	backend := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "mooncake-dram"},
		Spec: workercore.KVCacheBackendSpec{
			Transport: workercore.KVCacheBackendTransport{Protocol: "RDMA"},
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Members: []workercore.KVCacheBackendMember{{
						NodeSelector:      map[string]string{"kvcache-dram": "true"},
						Medium:            "DRAM",
						CapacityPerMember: resource.MustParse("500Gi"),
						LocalBufferSize:   resource.MustParse("4Gi"),
					}},
				},
			},
		},
	}
	conn := testConnection()
	conn.Protocol = mooncake.MemberProtocol(backend)
	require.Equal(t, "rdma", conn.Protocol,
		"the fixture is the case under test: a backend whose members run a fabric transport")

	// The member side MUST NOT move: the same backend's DaemonSet still runs the transport its
	// members were given, with the host access that transport takes.
	member := mooncake.RenderMemberDaemonSet(backend, 0, "mooncake:test")
	require.Len(t, member.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "rdma",
		envValue(t, member.Spec.Template.Spec.Containers[0].Env, "MOONCAKE_PROTOCOL").Value)
	assert.True(t, member.Spec.Template.Spec.HostNetwork)

	// The direct leg came apart: handed that same backend's transport, it renders its own
	// declaration rather than inheriting a fabric this operator gives engine Pods no access to.
	result, err := Render(Input{
		Engine: EngineVLLM, Role: RolePrefill, Connection: conn, DirectTransfer: true,
	})
	require.NoError(t, err)
	require.Len(t, result.Args, 2)
	assert.Equal(t, vllmTransferConfigArg, result.Args[0])
	assert.JSONEq(t, `{
		"kv_connector":"MultiConnector",
		"kv_role":"kv_producer",
		"kv_connector_extra_config":{"connectors":[
			{"kv_connector":"MooncakeConnector","kv_role":"kv_producer",
			 "kv_connector_extra_config":{"mooncake_protocol":"tcp"}},
			{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_both"}
		]}
	}`, result.Args[1])

	// The STORE plane of the very same render still follows the backend: only the direct leg
	// moved, so the engine reaches its pool over the transport the pool runs.
	assert.Equal(t, "rdma", renderedConfig(t, Input{
		Engine: EngineVLLM, Role: RolePrefill, Connection: conn, DirectTransfer: true,
	})["protocol"])
}

// TestRender_DirectTransferProtocolDeclared pins the declared source of the direct leg's
// transport: a caller naming a protocol gets it VERBATIM -- including one no enum would admit,
// because the accepted set belongs to the engine image's mooncake build -- and the store plane of
// the same render is untouched. The unset case is pinned by the two tests above: everything they
// assert rides on the renderer's default.
func TestRender_DirectTransferProtocolDeclared(t *testing.T) {
	for _, protocol := range []string{"rdma", "hip"} {
		t.Run(protocol, func(t *testing.T) {
			conn := testConnection()
			conn.Protocol = "tcp"

			result, err := Render(Input{
				Engine: EngineVLLM, Role: RoleDecode, Connection: conn,
				DirectTransfer: true, DirectTransferProtocol: protocol,
			})
			require.NoError(t, err)
			require.Len(t, result.Args, 2)
			assert.JSONEq(t, fmt.Sprintf(`{
				"kv_connector":"MultiConnector",
				"kv_role":"kv_consumer",
				"kv_connector_extra_config":{"connectors":[
					{"kv_connector":"MooncakeConnector","kv_role":"kv_consumer",
					 "kv_connector_extra_config":{"mooncake_protocol":%q}},
					{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_consumer"}
				]}
			}`, protocol), result.Args[1])

			assert.Equal(t, "tcp", renderedConfig(t, Input{
				Engine: EngineVLLM, Role: RoleDecode, Connection: conn,
				DirectTransfer: true, DirectTransferProtocol: protocol,
			})["protocol"], "the declared value moves the direct leg alone")
		})
	}
}

// TestRender_Refusals covers every case where rendering anything would produce a container that starts
// normally and does not use the cache.
func TestRender_Refusals(t *testing.T) {
	testCases := []struct {
		name  string
		input Input
		want  Reason
	}{
		{
			name:  "unknown engine",
			input: Input{Engine: "tensorrt", Connection: testConnection()},
			want:  ReasonEngineUnknown,
		},
		{
			name:  "engine unset",
			input: Input{Connection: testConnection()},
			want:  ReasonEngineUnknown,
		},
		{
			name:  "no master address",
			input: Input{Engine: EngineVLLM, Connection: Connection{Protocol: "tcp"}},
			want:  ReasonConnectionIncomplete,
		},
		{
			name:  "no protocol",
			input: Input{Engine: EngineVLLM, Connection: Connection{MasterAddress: "master:50051"}},
			want:  ReasonConnectionIncomplete,
		},
		{
			name:  "role on sglang",
			input: Input{Engine: EngineSGLang, Role: RolePrefill, Connection: testConnection()},
			want:  ReasonRoleUnsupported,
		},
		{
			name:  "unknown role on vllm",
			input: Input{Engine: EngineVLLM, Role: "both", Connection: testConnection()},
			want:  ReasonRoleUnknown,
		},
		{
			// The connection here is COMPLETE, which is why this reason is its own: every value is
			// present and legal, and it is the pair that no container can run.
			name:  "a transport the engine's store backend refuses",
			input: Input{Engine: EngineVLLMAscend, Connection: testConnection()},
			want:  ReasonTransportUnsupported,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(tc.input)
			assert.Nil(t, result, "a refusal renders nothing; a partial result would be applied")
			assert.Equal(t, tc.want, reasonOf(t, err))
		})
	}
}

// TestRender_TransportIsCheckedAtTheFunnel is why the check lives in Render rather than at either
// caller.
//
// TWO surfaces render a client and only one of them can name vLLM-Ascend today: the Pod admission
// webhook takes the engine from an annotation, which ParseEngine restricts to the selectable set,
// while the ModelDeployment reconciler DERIVES it from the role's accelerator. A check placed on the
// caller that can reach the pair would leave the other admitting it the day its inputs widen, and a
// check on both would be two implementations of one table. Render is the single point both pass
// through, so this is where the pair is refused and where the refusal has to be pinned.
//
// The positive rows are the load-bearing half. With only the refusal, a Render that declined every
// vLLM-Ascend input -- or every tcp one -- would pass, and both of those break a deployment that
// works today.
func TestRender_TransportIsCheckedAtTheFunnel(t *testing.T) {
	testCases := []struct {
		name     string
		engine   Engine
		protocol string
		rendered bool
	}{
		// #172: nobody chose tcp. Auto is the schema's default and the backend resolves it, so a pool
		// left alone is what hands this engine the value it refuses.
		{name: "vllm-ascend on a default pool's transport", engine: EngineVLLMAscend, protocol: "tcp"},
		{name: "vllm-ascend on ascend", engine: EngineVLLMAscend, protocol: "ascend", rendered: true},
		{name: "vllm on a default pool's transport", engine: EngineVLLM, protocol: "tcp", rendered: true},
		{name: "sglang on a default pool's transport", engine: EngineSGLang, protocol: "tcp", rendered: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			conn := testConnection()
			conn.Protocol = tc.protocol

			result, err := Render(Input{Engine: tc.engine, Connection: conn})
			if tc.rendered {
				require.NoError(t, err)
				assert.NotEmpty(t, result.Args, "an admitted pair renders the engine's argument")
				return
			}

			assert.Nil(t, result, "a refusal renders nothing; a partial result would be applied")
			assert.Equal(t, ReasonTransportUnsupported, reasonOf(t, err))
		})
	}
}

// TestSupportsRole_AgreesWithRender is what keeps SupportsRole from becoming a second opinion.
//
// An admission handler refuses a role by asking the table, and the container is configured by
// asking Render. If the two disagreed, one direction would refuse a role that renders fine and the
// other would admit a role that cannot be rendered at all -- and neither would fail anywhere else.
// The vLLM branch is where a disagreement could actually appear: renderSGLang reads the table, while
// vllmKVRole maps the roles in its own switch.
//
// The pairs are enumerated from Engines() and the whole role set rather than listed, so an engine
// added to the package without a table entry fails here instead of silently reporting false.
func TestSupportsRole_AgreesWithRender(t *testing.T) {
	roles := map[string]Role{"none": RoleNone, "prefill": RolePrefill, "decode": RoleDecode}

	for _, engine := range Engines() {
		for label, role := range roles {
			t.Run(string(engine)+"/"+label, func(t *testing.T) {
				// The connection follows the engine, so this measures the ROLE axis alone. With one
				// transport for every engine, vLLM-Ascend would be refused on all three roles and
				// this would report a role disagreement that is not there.
				_, err := Render(Input{
					Engine: engine, Role: role, Connection: testConnectionFor(engine),
				})

				assert.Equal(t, err == nil, SupportsRole(engine, role),
					"the table and the renderer must answer alike for %q/%q", engine, role)
			})
		}
	}
}

// TestRender_RefusesAHalfConnection pins the gate that renderVLLM's own "has store" test depends on.
//
// That renderer asks only whether an address is present, while this gate asks whether either field
// is. The two agree only because a connection carrying one without the other never gets past here,
// and nothing in the renderer would notice if that stopped being true: it would silently reclassify
// such an input as store-less and return a Result carrying no arguments at all.
func TestRender_RefusesAHalfConnection(t *testing.T) {
	cases := []struct {
		name string
		conn Connection
		want string
	}{
		{
			name: "a transport with no address",
			conn: Connection{Protocol: "tcp"},
			want: "published no client endpoint",
		},
		{
			name: "an address with no transport",
			conn: Connection{MasterAddress: "master:50051"},
			want: "published no transport",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Render(Input{Engine: EngineVLLM, Connection: c.conn})
			require.Error(t, err, "half a connection must not reach a renderer")
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// TestRender_RefusesDirectTransferOnEnginesThatDropIt covers the capabilities only vLLM renders.
//
// The positive baseline matters more than the refusals: without it a renderer that refused every
// engine would pass this test, and the whole point is that vLLM must still be accepted.
func TestRender_RefusesDirectTransferOnEnginesThatDropIt(t *testing.T) {
	store := Connection{MasterAddress: "master:50051", Protocol: "tcp"}
	cases := []struct {
		name     string
		engine   Engine
		role     Role
		direct   bool
		publish  bool
		accepted bool
	}{
		{name: "sglang cannot render direct transfer", engine: EngineSGLang, direct: true},
		{name: "sglang cannot publish KV events", engine: EngineSGLang, publish: true},
		{
			name:   "vllm-ascend cannot render direct transfer",
			engine: EngineVLLMAscend, role: RolePrefill, direct: true,
		},
		{
			name: "vllm renders both", engine: EngineVLLM, role: RolePrefill,
			direct: true, publish: true, accepted: true,
		},
		{
			// The baseline that makes the refusals mean something: SGLang is still a supported
			// engine, and asking for neither capability must still render.
			name: "sglang with neither is still accepted", engine: EngineSGLang, accepted: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn := store
			if c.engine == EngineVLLMAscend {
				conn.Protocol = "ascend"
			}
			in := Input{
				Engine: c.engine, Role: c.role, Connection: conn,
				DirectTransfer: c.direct, PublishKVEvents: c.publish,
			}
			if c.publish {
				in.KVEventsHost = "role.ns.svc"
			}
			_, err := Render(in)
			if c.accepted {
				require.NoError(t, err)
				return
			}
			require.Error(t, err, "a capability the renderer drops must be refused, not ignored")
			assert.Contains(t, err.Error(), "moves nothing")
		})
	}
}
