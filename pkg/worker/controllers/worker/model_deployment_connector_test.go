package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// connectorInput is one pool-and-domain fixture every case renders from.
//
// It carries a NON-EMPTY Domain on purpose: the cases asserting that no tenant key is rendered are
// only worth anything if a domain was available to render.
func connectorInput(engine, manufacturer string) ModelDeploymentConnectorInput {
	return ModelDeploymentConnectorInput{
		Engine:              engine,
		Manufacturer:        manufacturer,
		Domain:              "team-a-shared",
		MasterServerAddress: "shared-kv-master.gpustack-system.svc:50051",
		MetadataServer:      "P2PHANDSHAKE",
		Protocol:            "TCP",
		DeviceName:          "",
	}
}

func TestSynthesizeModelDeploymentConnector(t *testing.T) {
	// The two vLLM-family readers load configuration from a file, because their loader uses the
	// environment only to locate the path. Their key sets differ by exactly one key: `mode` exists
	// on vLLM alone. Both are written out in full rather than derived from each other, so that the
	// one difference is visible instead of being a line of test logic.
	//
	// The size values are JSON numbers, matching the int the config classes declare.
	wantVLLMConfig := map[string]any{
		"master_server_address": "shared-kv-master.gpustack-system.svc:50051",
		"metadata_server":       "P2PHANDSHAKE",
		"protocol":              "tcp",
		"device_name":           "",
		"global_segment_size":   0,
		"local_buffer_size":     134217728,
		"mode":                  "standalone-store",
	}
	wantAscendConfig := map[string]any{
		"master_server_address": "shared-kv-master.gpustack-system.svc:50051",
		"metadata_server":       "P2PHANDSHAKE",
		"protocol":              "tcp",
		"device_name":           "",
		"global_segment_size":   0,
		"local_buffer_size":     134217728,
	}

	testCases := []struct {
		name             string
		engine           string
		manufacturer     string
		wantArgs         []string
		wantEnv          []core.EnvVar
		wantDefaultedEnv []core.EnvVar
		wantConfig       map[string]any
	}{
		{
			name:         "vllm_golden",
			engine:       workercore.ModelDeploymentEngineVLLM,
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantArgs: []string{
				"--kv-transfer-config",
				`{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_both"}`,
			},
			wantEnv: []core.EnvVar{
				{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"},
			},
			wantDefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
			wantConfig:       wantVLLMConfig,
		},
		{
			// SAME ENGINE VALUE AS THE CASE ABOVE, different hardware. That is the whole point of
			// narrowing the enum: the connector name changed and nothing about the engine did.
			name:         "vllm_on_ascend_golden",
			engine:       workercore.ModelDeploymentEngineVLLM,
			manufacturer: nodefeature.ManufacturerAscend,
			wantArgs: []string{
				"--kv-transfer-config",
				`{"kv_connector":"AscendStoreConnector","kv_role":"kv_both"}`,
			},
			wantEnv: []core.EnvVar{
				{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"},
			},
			wantDefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
			wantConfig:       wantAscendConfig,
		},
		{
			// SGLang takes the environment, and every part of this case is load-bearing: no
			// extra-config argument and no config-path variable, because either one diverts the
			// engine onto a loader whose per-key fallbacks are compile-time literals; and no
			// ClientConfig, because a mounted file nothing reads claims a wiring that is not
			// happening. The one value that cannot be known at admission time arrives as a
			// fieldRef, which is the whole reason this engine does not get a file.
			name:         "sglang_golden",
			engine:       workercore.ModelDeploymentEngineSGLang,
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantArgs: []string{
				"--hicache-storage-backend", "mooncake",
			},
			wantEnv: []core.EnvVar{
				{Name: "MOONCAKE_MASTER", Value: "shared-kv-master.gpustack-system.svc:50051"},
				{Name: "MOONCAKE_TE_META_DATA_SERVER", Value: "P2PHANDSHAKE"},
				{Name: "MOONCAKE_PROTOCOL", Value: "tcp"},
				{Name: "MOONCAKE_DEVICE", Value: ""},
				{Name: "MOONCAKE_GLOBAL_SEGMENT_SIZE", Value: "0"},
				{
					Name: "MOONCAKE_LOCAL_HOSTNAME",
					ValueFrom: &core.EnvVarSource{
						FieldRef: &core.ObjectFieldSelector{FieldPath: "status.podIP"},
					},
				},
			},
			wantDefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
			wantConfig:       nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SynthesizeModelDeploymentConnector(connectorInput(tc.engine, tc.manufacturer))
			require.NoError(t, err)

			assert.Equal(t, tc.wantArgs, got.Args)
			assert.Equal(t, tc.wantEnv, got.Env)
			assert.Equal(t, tc.wantDefaultedEnv, got.DefaultedEnv)
			// Exact equality, not containment: "exactly the keys this engine's reader reads" is a
			// claim about what is absent as much as about what is present.
			assert.Equal(t, tc.wantConfig, got.ClientConfig)
		})
	}
}

// TestSynthesizeModelDeploymentConnector_SGLangEnvironmentCarrier states the four properties that
// make the environment carrier correct, each of which a plausible-looking edit would break.
//
// It exists as its own test rather than as more rows in the golden case because the golden case
// asserts the whole rendering by equality, and an equality assertion tells whoever broke it WHAT
// changed but not WHY it mattered. These carry the why.
func TestSynthesizeModelDeploymentConnector_SGLangEnvironmentCarrier(t *testing.T) {
	got, err := SynthesizeModelDeploymentConnector(
		connectorInput(workercore.ModelDeploymentEngineSGLang, nodefeature.ManufacturerNVIDIA))
	require.NoError(t, err)

	// The engine resolves its config source in the order extra-config, then config path, then
	// environment. Either of the first two wins over the environment, so rendering one does not
	// add a fallback, it REPLACES this configuration with one whose missing keys become literals.
	assert.NotContains(t, got.Args, "--hicache-storage-backend-extra-config",
		"its loader is key-for-key identical to the file loader and sits at higher precedence")
	for _, e := range got.Env {
		assert.NotEqual(t, "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH", e.Name,
			"setting it selects the file loader, whose fallback for local_hostname is the literal localhost")
	}

	// The identity has to be evaluated by kubelet at container start, because no Pod IP exists when
	// the object is admitted. A literal here is the defect being avoided, INCLUDING a literal that
	// looks correct, so the assertion is on the fieldRef and on the absence of a value.
	var hostname *core.EnvVar
	for i := range got.Env {
		if got.Env[i].Name == "MOONCAKE_LOCAL_HOSTNAME" {
			hostname = &got.Env[i]
		}
	}
	require.NotNil(t, hostname, "SGLang reads this key and its own fallback is the literal localhost")
	assert.Empty(t, hostname.Value, "a literal identity registers every replica as the same peer")
	require.NotNil(t, hostname.ValueFrom)
	require.NotNil(t, hostname.ValueFrom.FieldRef)
	assert.Equal(t, "status.podIP", hostname.ValueFrom.FieldRef.FieldPath)

	// A pure client contributes no storage segment. The value must be an explicit zero: this
	// engine's own fallback is the string "4gb", so an absent key is a 4 GiB in-process member.
	// The store accepts zero for exactly this purpose -- its setup_internal skips mounting a
	// segment and its validator requires zero or at least MIN_SEGMENT_SIZE.
	assert.Contains(t, got.Env, core.EnvVar{Name: "MOONCAKE_GLOBAL_SEGMENT_SIZE", Value: "0"})

	// No file is rendered, so no ConfigMap is created for this engine.
	assert.Nil(t, got.ClientConfig)
}

// TestSynthesizeModelDeploymentConnector_KeysNeverRendered states, per key, WHY the operator leaves
// it out. Exact map equality above already fails if any of them appears; these cases exist so that
// whoever deletes one has to read the reason first.
//
// BOTH REMAINING REASONS ARE DECISIONS. Three others used to pin a defect instead, and the
// distinction was marked inline precisely so that this could happen: a green case is not by itself
// an endorsement of the behavior it asserts, so each one carries its reason rather than just its
// key, and the three whose reason stopped holding were deleted rather than left green.
func TestSynthesizeModelDeploymentConnector_KeysNeverRendered(t *testing.T) {
	testCases := []struct {
		name string
		key  string
		why  string
	}{
		{
			name: "no_tenant_id",
			key:  "tenant_id",
			why: "no supported engine passes a tenant to the cache client: it is setup()'s 11th " +
				"parameter and every engine calls setup() positionally with seven or eight " +
				"arguments, so rendering the key would document a wiring that is not happening",
		},
		{
			name: "no_local_hostname",
			key:  "local_hostname",
			why: "these two readers derive it from their own process, so a deployment-wide file " +
				"holding a value that differs per replica would be wrong for all but one. " +
				"THIS IS TRUE OF THESE TWO ONLY -- SGLang reads the key from the file and falls " +
				"back to a literal, which is why it takes the environment instead",
		},
		// THREE MORE CASES USED TO SIT HERE, pinning the absence of `global_segment_size`,
		// `local_buffer_size` and `mode` as a KNOWN DEFECT. They are gone because the defect is
		// fixed: those keys declare a role rather than sizing a contribution, and the coherent
		// group is now rendered. What replaced them is a POSITIVE assertion in
		// TestSynthesizeModelDeploymentConnector_FileCarrierDeclaresPureClient -- the direction
		// that fails when a key goes missing, rather than the direction that passes.
	}

	// SGLANG IS DELIBERATELY NOT IN THIS LIST, and leaving it in would be worse than useless. It
	// renders no ClientConfig at all, so every NotContains below would pass against a nil map --
	// vacuously, proving nothing, while reading as three more engines' worth of coverage. Its own
	// keys are asserted positively in TestSynthesizeModelDeploymentConnector_SGLangEnvironmentCarrier,
	// where an absent key fails instead of passing.
	// Both file carriers, which is now one engine on two backends rather than two engines.
	carriers := []struct{ engine, manufacturer string }{
		{workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA},
		{workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerAscend},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range carriers {
				got, err := SynthesizeModelDeploymentConnector(connectorInput(c.engine, c.manufacturer))
				// A nil map would make every assertion below vacuous, so the carrier itself is
				// checked first: this test only means anything where a file is rendered.
				require.NoError(t, err)
				require.NotEmpty(t, got.ClientConfig, "%s on %s renders a file carrier", c.engine, c.manufacturer)
				assert.NotContains(t, got.ClientConfig, tc.key, "%s on %s: %s", c.engine, c.manufacturer, tc.why)
			}
		})
	}
}

// TestSynthesizeModelDeploymentConnector_FileCarrierDeclaresPureClient asserts the group that makes
// a replica a client of the pool rather than a member of it.
//
// It is stated positively and per key, because the failure it guards is a MISSING key: every
// engine's config class defaults `global_segment_size` to 4 GiB, so an absent key does not fall
// back to "no contribution", it selects the wrong role and does so without any error. An assertion
// that a key is absent cannot catch that; only one that it is present with the right value can.
//
// The values are asserted as JSON numbers. All three engines run these keys through a parser that
// would also accept "0", but the type the config classes declare is int, and matching the
// declaration means the rendering does not depend on that parser staying where it is.
func TestSynthesizeModelDeploymentConnector_FileCarrierDeclaresPureClient(t *testing.T) {
	testCases := []struct {
		name         string
		manufacturer string
		wantMode     bool
	}{
		{
			// vLLM proper has `mode`, and it is one half of a cross-field rule its own
			// __post_init__ enforces in both directions: embedded rejects a zero segment,
			// standalone-store rejects a non-zero one. Rendering the segment without the mode
			// raises at startup.
			name:         "vllm_on_nvidia_declares_standalone_store",
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantMode:     true,
		},
		{
			// The Ascend package has no `mode` field at all, so the segment size stands alone.
			// Rendering a mode there would be a key its reader ignores -- and note this row and
			// the one above share an ENGINE and differ only in hardware.
			name:         "vllm_on_ascend_has_no_mode_to_declare",
			manufacturer: nodefeature.ManufacturerAscend,
			wantMode:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SynthesizeModelDeploymentConnector(
				connectorInput(workercore.ModelDeploymentEngineVLLM, tc.manufacturer))
			require.NoError(t, err)

			assert.Equal(t, 0, got.ClientConfig["global_segment_size"],
				"a positive value, including the 4 GiB default, makes this replica a store member")
			assert.Equal(t, 128*1024*1024, got.ClientConfig["local_buffer_size"],
				"the documented client staging size; a non-positive value is rejected outright")

			if tc.wantMode {
				assert.Equal(t, "standalone-store", got.ClientConfig["mode"])
			} else {
				assert.NotContains(t, got.ClientConfig, "mode",
					"this engine has no such field, so the key would document nothing")
			}
		})
	}
}

func TestSynthesizeModelDeploymentConnector_DeviceSpelling(t *testing.T) {
	// The same fact has three spellings across three surfaces. Only the JSON one applies here, and
	// asserting the other two are absent is what keeps a future edit from picking the wrong one.
	got, err := SynthesizeModelDeploymentConnector(ModelDeploymentConnectorInput{
		Engine:              workercore.ModelDeploymentEngineVLLM,
		Domain:              "team-a-shared",
		MasterServerAddress: "master:50051",
		MetadataServer:      "P2PHANDSHAKE",
		Protocol:            "RDMA",
		DeviceName:          "mlx5_0",
	})
	require.NoError(t, err)

	assert.Equal(t, "mlx5_0", got.ClientConfig["device_name"])
	assert.NotContains(t, got.ClientConfig, "rdma_devices", "that is setup()'s positional spelling, not the file's")
	assert.NotContains(t, got.ClientConfig, "MOONCAKE_DEVICE", "that is Mooncake's own environment spelling, not the file's")
}

func TestSynthesizeModelDeploymentConnector_UnsupportedEngine(t *testing.T) {
	_, err := SynthesizeModelDeploymentConnector(connectorInput("tensorrt-llm", nodefeature.ManufacturerNVIDIA))
	require.Error(t, err)
}

func TestModelDeploymentClientProtocol(t *testing.T) {
	testCases := []struct {
		name  string
		given string
		want  string
	}{
		{name: "auto_resolves_to_tcp", given: "Auto", want: "tcp"},
		{name: "empty_resolves_to_tcp", given: "", want: "tcp"},
		{name: "tcp_lowercased", given: "TCP", want: "tcp"},
		{name: "rdma_lowercased", given: "RDMA", want: "rdma"},
		{name: "hip_lowercased", given: "HIP", want: "hip"},
		{name: "ascend_lowercased", given: "Ascend", want: "ascend"},
		// The client warns and carries on for a name it does not know, so refusing here would
		// refuse a transport the client would have accepted.
		{name: "unknown_passes_through_lowercased", given: "Efa", want: "efa"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, modelDeploymentClientProtocol(tc.given))
		})
	}
}

func TestModelDeploymentArgName(t *testing.T) {
	testCases := []struct {
		name  string
		given string
		want  string
	}{
		{name: "bare_flag", given: "--kv-transfer-config", want: "--kv-transfer-config"},
		{name: "flag_with_inline_value", given: `--kv-transfer-config={"a":1}`, want: "--kv-transfer-config"},
		{name: "value_only", given: "32768", want: "32768"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ModelDeploymentArgName(tc.given))
		})
	}
}

func TestModelDeploymentOwnership(t *testing.T) {
	testCases := []struct {
		name     string
		engine   string
		arg      string
		env      string
		wantsArg bool
		wantsEnv bool
	}{
		{
			name:     "vllm_owns_its_transfer_config",
			engine:   workercore.ModelDeploymentEngineVLLM,
			arg:      "--kv-transfer-config",
			wantsArg: true,
		},
		{
			name:     "ownership_ignores_an_inline_value",
			engine:   workercore.ModelDeploymentEngineVLLM,
			arg:      `--kv-transfer-config={"kv_connector":"Other"}`,
			wantsArg: true,
		},
		{
			name:     "vllm_does_not_own_an_ordinary_argument",
			engine:   workercore.ModelDeploymentEngineVLLM,
			arg:      "--max-model-len=32768",
			wantsArg: false,
		},
		{
			name:     "sglang_owns_its_hicache_arguments",
			engine:   workercore.ModelDeploymentEngineSGLang,
			arg:      "--hicache-storage-backend-extra-config",
			wantsArg: true,
		},
		{
			// Ownership is per (engine, key). SGLang's key is an ordinary user argument on vLLM,
			// and refusing it there would refuse something harmless.
			name:     "vllm_does_not_own_sglangs_key",
			engine:   workercore.ModelDeploymentEngineVLLM,
			arg:      "--hicache-storage-backend-extra-config",
			wantsArg: false,
		},
		{
			name:     "sglang_does_not_own_vllms_key",
			engine:   workercore.ModelDeploymentEngineSGLang,
			arg:      "--kv-transfer-config",
			wantsArg: false,
		},
		{
			name:     "vllm_owns_its_config_path",
			engine:   workercore.ModelDeploymentEngineVLLM,
			env:      "MOONCAKE_CONFIG_PATH",
			wantsEnv: true,
		},
		{
			// Owned even though the operator never sets it: setting it would divert the engine onto
			// the file loader, whose per-key fallbacks are literals. Ownership here is for what a
			// user entry would DESTROY, not for what it would duplicate.
			name:     "sglang_owns_the_config_path_it_deliberately_leaves_unset",
			engine:   workercore.ModelDeploymentEngineSGLang,
			env:      "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH",
			wantsEnv: true,
		},
		{
			name:     "sglang_owns_its_identity_variable",
			engine:   workercore.ModelDeploymentEngineSGLang,
			env:      "MOONCAKE_LOCAL_HOSTNAME",
			wantsEnv: true,
		},
		{
			name:     "sglang_owns_its_segment_size",
			engine:   workercore.ModelDeploymentEngineSGLang,
			env:      "MOONCAKE_GLOBAL_SEGMENT_SIZE",
			wantsEnv: true,
		},
		{
			// Per (engine, key) again, in the direction that is easy to get wrong: vLLM loads no
			// value from the environment, so these names carry nothing there and refusing them
			// would refuse something harmless.
			name:     "vllm_does_not_own_sglangs_environment",
			engine:   workercore.ModelDeploymentEngineVLLM,
			env:      "MOONCAKE_GLOBAL_SEGMENT_SIZE",
			wantsEnv: false,
		},
		{
			// SGLang owns six MOONCAKE_-prefixed names and NOT this one, which is the case that
			// fails if ownership is ever reduced to a prefix test. MOONCAKE_CONFIG_PATH is the
			// vLLM family's config path; this engine does not read it.
			name:     "sglang_does_not_own_the_mooncake_config_path",
			engine:   workercore.ModelDeploymentEngineSGLang,
			env:      "MOONCAKE_CONFIG_PATH",
			wantsEnv: false,
		},
		{
			// A defaulted key is deliberately not owned: duplication is harmless because last-wins
			// is well defined, so a user turning metrics off gets their value rather than a refusal.
			name:     "the_metrics_switch_is_not_owned",
			engine:   workercore.ModelDeploymentEngineVLLM,
			env:      "MC_TE_METRIC",
			wantsEnv: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.arg != "" {
				assert.Equal(t, tc.wantsArg, ModelDeploymentOwnsArg(tc.engine, tc.arg))
			}
			if tc.env != "" {
				assert.Equal(t, tc.wantsEnv, ModelDeploymentOwnsEnv(tc.engine, tc.env))
			}
		})
	}
}

// TestModelDeploymentOwnedAndDefaultedCannotDisagree is the invariant that keeps the refusal and the
// render from drifting apart. It reads the renderer's actual output rather than the table, so a key
// added to one and forgotten in the other fails here instead of in production.
func TestModelDeploymentOwnedAndDefaultedCannotDisagree(t *testing.T) {
	// Every (engine, backend) the renderer can be called with, not every engine. The Ascend row is
	// the load-bearing one: the ownership table is keyed on the ENGINE alone, so this is what
	// proves that claim - if any key the Ascend backend renders were not in vLLM's own entry, this
	// fails, and the table would have to be re-keyed.
	carriers := []struct{ engine, manufacturer string }{
		{workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA},
		{workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerAscend},
		{workercore.ModelDeploymentEngineSGLang, nodefeature.ManufacturerNVIDIA},
	}

	for _, c := range carriers {
		t.Run(c.engine+"_on_"+c.manufacturer, func(t *testing.T) {
			engine := c.engine
			got, err := SynthesizeModelDeploymentConnector(connectorInput(engine, c.manufacturer))
			require.NoError(t, err)

			for _, arg := range got.Args {
				name := ModelDeploymentArgName(arg)
				if !isFlag(name) {
					continue // a flag's value, not a key ownership applies to
				}
				assert.True(t, ModelDeploymentOwnsArg(engine, name),
					"the renderer emits %q but the table does not own it", name)
			}

			for _, env := range got.Env {
				assert.True(t, ModelDeploymentOwnsEnv(engine, env.Name),
					"the renderer emits %q as owned but the table does not own it", env.Name)
				assert.False(t, ModelDeploymentDefaultsEnv(env.Name),
					"%q is both owned and defaulted, so a user supplying it is both refused and honoured", env.Name)
			}

			for _, env := range got.DefaultedEnv {
				assert.True(t, ModelDeploymentDefaultsEnv(env.Name),
					"the renderer defaults %q but the table does not list it", env.Name)
				assert.False(t, ModelDeploymentOwnsEnv(engine, env.Name),
					"%q is both defaulted and owned", env.Name)
			}
		})
	}
}

func isFlag(s string) bool {
	return len(s) > 2 && s[0] == '-' && s[1] == '-'
}

func TestModelDeploymentEngineCommand(t *testing.T) {
	testCases := []struct {
		name    string
		engine  string
		model   string
		want    []string
		wantErr bool
	}{
		{
			// The model is POSITIONAL on vLLM's serve subcommand, not a flag.
			name:   "vllm_serves_the_model_positionally",
			engine: workercore.ModelDeploymentEngineVLLM,
			model:  "Qwen/Qwen2.5-72B-Instruct",
			want:   []string{"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct"},
		},
		{
			// The former enum value, kept as a case so that it stays refused. It adds no execution
			// path the unsupported_engine case below does not already cover -- what it records is
			// the DECISION: the runner's service dimension has no such value, every Ascend image is
			// service=vllm, and putting it back would hang the connector on the engine again.
			// An Ascend pool gets its command from the vllm row above, because the entry point does
			// not vary with the backend even though the connector does.
			name:    "the_former_vllm_ascend_value_is_not_an_engine",
			engine:  "vllm-ascend",
			model:   "Qwen/Qwen2.5-72B-Instruct",
			wantErr: true,
		},
		{
			name:   "sglang_launches_a_module_with_model_path",
			engine: workercore.ModelDeploymentEngineSGLang,
			model:  "Qwen/Qwen2.5-72B-Instruct",
			want: []string{
				"python3", "-m", "sglang.launch_server",
				"--model-path", "Qwen/Qwen2.5-72B-Instruct",
			},
		},
		{
			name:    "unsupported_engine",
			engine:  "tensorrt-llm",
			model:   "Qwen/Qwen2.5-72B-Instruct",
			wantErr: true,
		},
		{
			// An empty model would render an argv the engine refuses at startup, which is a
			// failure one layer away from its cause.
			name:    "empty_model",
			engine:  workercore.ModelDeploymentEngineVLLM,
			model:   "",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ModelDeploymentEngineCommand(tc.engine, tc.model)
			if tc.wantErr {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
