package inject

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
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

// TestRender_TenantGoesOnlyToAnEngineThatReadsOne replaces an earlier test that asserted no tenant
// reached any artifact at all. That assertion encoded a fact which turned out to be wrong for the
// SGLang build this project actually deploys: its config class carries a tenant_id, its environment
// loader reads MOONCAKE_TENANT_ID, and its store call forwards a non-default value. vLLM's has none
// of that, so a variable written for it would be decoration that reads as a guarantee.
//
// The two engines are therefore the two sides of this control, and neither half means anything alone:
// the negative one passes against a renderer that emits nothing at all, the positive one against a
// renderer that emits everywhere.
func TestRender_TenantGoesOnlyToAnEngineThatReadsOne(t *testing.T) {
	const domain = "team-a-chat"

	testCases := []struct {
		engine Engine
		want   bool
	}{
		{engine: EngineVLLM, want: false},
		{engine: EngineVLLMAscend, want: false},
		{engine: EngineSGLang, want: true},
	}
	require.NotEqual(t, testCases[0].want, testCases[len(testCases)-1].want,
		"both answers must appear, or this cannot tell a rendered value from a constant")

	for _, tc := range testCases {
		t.Run(string(tc.engine), func(t *testing.T) {
			result, err := Render(Input{
				Engine: tc.engine, Domain: domain, Connection: testConnectionFor(tc.engine),
			})
			require.NoError(t, err)

			rendered, err := json.Marshal(result)
			require.NoError(t, err)

			assert.Equal(t, tc.want, result.TenantInjected,
				"the renderer reports the action it took")
			// There is no file-carried case here any more, and it did NOT come back when this package
			// started rendering AscendStoreConnector for vLLM-Ascend. An earlier revision predicted it
			// would - "if the Ascend connector is ever rendered, this branch comes back" - and the
			// prediction was wrong because it read the tenant answer off the connector NAME. The
			// pinned v0.19.1rc1 has no tenant anywhere (grep over vllm_ascend/, excluding tests: zero
			// hits), so selecting that connector fixes a startup failure and forwards nothing.
			// Which connector we render and whether a tenant travels are independent facts.
			switch {
			case tc.want:
				assert.Equal(t, domain, envValue(t, result.Env, "MOONCAKE_TENANT_ID").Value,
					"the reuse domain is what the engine is told to write under")
			default:
				// Asserted on the emitted Env and the rendered file, never on the marshaled Result:
				// that struct also carries TenantEnvName, which names the variable without emitting
				// it. Scanning the whole document for a substring caught that metadata and called it
				// an emission - the second time a "document does not contain X" assertion has failed
				// on something the document only describes.
				assert.NotContains(t, envNames(result.Env), "MOONCAKE_TENANT_ID",
					"this engine reads no tenant, so a variable for it would be decoration")
				assert.NotContains(t, renderedConfig(t, Input{
					Engine: tc.engine, Domain: domain, Connection: testConnectionFor(tc.engine),
				}), "tenant_id", "nor a key in its file")
			}
			_ = rendered
		})
	}
}

// TestRender_TenantFollowsTheFactsTable substitutes the measured answer and requires the emission to
// change with it.
//
// Without this, the table would be documentation: the renderer could hardcode "SGLang gets a tenant"
// and every other test would still pass, because SGLang is the only engine whose entry says true. The
// entry is what a reviewer re-reads when a new build ships, so the code has to be reading it too.
func TestRender_TenantFollowsTheFactsTable(t *testing.T) {
	const domain = "team-a-chat"

	emitted := func(t *testing.T) bool {
		t.Helper()
		result, err := Render(Input{Engine: EngineSGLang, Domain: domain, Connection: testConnection()})
		require.NoError(t, err)
		return result.TenantInjected
	}

	assert.True(t, emitted(t), "the shipped entry measures this engine as forwarding")

	original := engineTenantSupport[EngineSGLang]
	t.Cleanup(func() { engineTenantSupport[EngineSGLang] = original })
	truncating := original
	truncating.ForwardsTenant = false
	engineTenantSupport[EngineSGLang] = truncating

	assert.False(t, emitted(t),
		"an entry measured as truncating must stop the emission, or the table is not being read")
}

// TestRender_TenantOmittedForAnEmptyDomain. An empty value is normalised back to the store default by
// the engine, so emitting one would be indistinguishable from not setting it - while still looking, on
// the Pod, like something was configured.
func TestRender_TenantOmittedForAnEmptyDomain(t *testing.T) {
	result, err := Render(Input{Engine: EngineSGLang, Domain: "", Connection: testConnection()})
	require.NoError(t, err)

	assert.False(t, result.TenantInjected)
	assert.NotContains(t, envNames(result.Env), "MOONCAKE_TENANT_ID",
		"an empty domain emits no tenant variable at all")
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
