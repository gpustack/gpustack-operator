package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// connectorInput is one pool-and-domain fixture every case renders from.
//
// It carries a NON-EMPTY Domain on purpose: the cases asserting that no tenant key is rendered are
// only worth anything if a domain was available to render.
func connectorInput(engine string) ModelDeploymentConnectorInput {
	return ModelDeploymentConnectorInput{
		Engine:              engine,
		Domain:              "team-a-shared",
		MasterServerAddress: "shared-kv-master.gpustack-system.svc:50051",
		MetadataServer:      "P2PHANDSHAKE",
		Protocol:            "TCP",
		DeviceName:          "",
	}
}

func TestSynthesizeModelDeploymentConnector(t *testing.T) {
	// The file carrier's key set is shared by the two vLLM-family readers, which load configuration
	// from a file because their loader uses the environment only to locate the path.
	wantFileConfig := map[string]string{
		"master_server_address": "shared-kv-master.gpustack-system.svc:50051",
		"metadata_server":       "P2PHANDSHAKE",
		"protocol":              "tcp",
		"device_name":           "",
	}

	testCases := []struct {
		name             string
		engine           string
		wantArgs         []string
		wantEnv          []core.EnvVar
		wantDefaultedEnv []core.EnvVar
		wantConfig       map[string]string
	}{
		{
			name:   "vllm_golden",
			engine: workercore.ModelDeploymentEngineVLLM,
			wantArgs: []string{
				"--kv-transfer-config",
				`{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_both"}`,
			},
			wantEnv: []core.EnvVar{
				{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"},
			},
			wantDefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
			wantConfig:       wantFileConfig,
		},
		{
			name:   "vllm_ascend_golden",
			engine: workercore.ModelDeploymentEngineVLLMAscend,
			wantArgs: []string{
				"--kv-transfer-config",
				`{"kv_connector":"AscendStoreConnector","kv_role":"kv_both"}`,
			},
			wantEnv: []core.EnvVar{
				{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"},
			},
			wantDefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
			wantConfig:       wantFileConfig,
		},
		{
			// SGLang takes the environment, and every part of this case is load-bearing: no
			// extra-config argument and no config-path variable, because either one diverts the
			// engine onto a loader whose per-key fallbacks are compile-time literals; and no
			// ClientConfig, because a mounted file nothing reads claims a wiring that is not
			// happening. The one value that cannot be known at admission time arrives as a
			// fieldRef, which is the whole reason this engine does not get a file.
			name:   "sglang_golden",
			engine: workercore.ModelDeploymentEngineSGLang,
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
			got, err := SynthesizeModelDeploymentConnector(connectorInput(tc.engine))
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
	got, err := SynthesizeModelDeploymentConnector(connectorInput(workercore.ModelDeploymentEngineSGLang))
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
// TWO OF THE FIVE REASONS ARE DECISIONS AND THREE PIN A DEFECT, and the difference is marked
// inline. A green case here is not by itself an endorsement of the behavior it asserts — which is
// exactly why each one has to carry its reason instead of just its key.
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
		// THE NEXT THREE PIN A KNOWN DEFECT, NOT A DECISION. They are kept so that the fix is
		// noticed as a change rather than slipping in, and they must be DELETED by the task that
		// wires the connector into the replicas.
		//
		// The reason recorded here originally was that these keys size a replica's contribution and
		// the operator must not invent a capacity. Measured on vLLM v0.25.1
		// (.../mooncake/store/worker.py's config class), that is backwards: the pair DECLARES A
		// ROLE. `mode` defaults to "embedded", `global_segment_size` defaults to 4 GiB, and
		// __post_init__ requires a positive global segment in embedded mode — so omitting all three
		// makes the engine rank an in-process store MEMBER contributing 4 GiB, when it should be a
		// pure client. A pure client on vLLM is the coherent triple
		// mode=standalone-store + global_segment_size=0 + local_buffer_size>0; on vllm-ascend there
		// is no `mode` field, so it is the pair.
		//
		// The old reason was right about the COUPLING and wrong about the conclusion: a cross-field
		// pair is dangerous when split, which is an argument for rendering the whole triple, not for
		// rendering none of it.
		//
		// THE LAST TECHNICAL BLOCKER IS GONE, so what remains is only ownership. The store's own
		// setup_internal accepts a zero global segment and skips mounting one, commented "A size of
		// 0 keeps the pure client/server setup semantics", and its validator requires the value to
		// be zero or at least MIN_SEGMENT_SIZE. The fix belongs to the shared rendering package
		// that takes this synthesis over, because fixing it here means re-verifying it there.
		// SGLang already carries the equivalent, as MOONCAKE_GLOBAL_SEGMENT_SIZE=0 on its own
		// carrier -- so this defect is now the vLLM family's alone.
		{
			name: "no_segment_sizes_global",
			key:  "global_segment_size",
			why:  "KNOWN DEFECT: omitting it selects the embedded role, contributing 4 GiB",
		},
		{
			name: "no_segment_sizes_local",
			key:  "local_buffer_size",
			why: "KNOWN DEFECT: omitting it takes the 4 GiB default rather than a client staging " +
				"buffer. The 128 MiB replacement is a vLLM-family value only: SGLang has no such " +
				"key and passes a hardcoded 16 MiB to setup()",
		},
		{
			name: "no_mode",
			key:  "mode",
			why:  "KNOWN DEFECT: omitting it leaves vLLM's default, which is embedded",
		},
	}

	// SGLANG IS DELIBERATELY NOT IN THIS LIST, and leaving it in would be worse than useless. It
	// renders no ClientConfig at all, so every NotContains below would pass against a nil map --
	// vacuously, proving nothing, while reading as three more engines' worth of coverage. Its own
	// keys are asserted positively in TestSynthesizeModelDeploymentConnector_SGLangEnvironmentCarrier,
	// where an absent key fails instead of passing.
	engines := []string{
		workercore.ModelDeploymentEngineVLLM,
		workercore.ModelDeploymentEngineVLLMAscend,
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, engine := range engines {
				got, err := SynthesizeModelDeploymentConnector(connectorInput(engine))
				// A nil map would make every assertion below vacuous, so the carrier itself is
				// checked first: this test only means anything where a file is rendered.
				require.NoError(t, err)
				require.NotEmpty(t, got.ClientConfig, "%s renders a file carrier", engine)
				assert.NotContains(t, got.ClientConfig, tc.key, "%s: %s", engine, tc.why)
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
	_, err := SynthesizeModelDeploymentConnector(connectorInput("tensorrt-llm"))
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
	engines := []string{
		workercore.ModelDeploymentEngineVLLM,
		workercore.ModelDeploymentEngineVLLMAscend,
		workercore.ModelDeploymentEngineSGLang,
	}

	for _, engine := range engines {
		t.Run(engine, func(t *testing.T) {
			got, err := SynthesizeModelDeploymentConnector(connectorInput(engine))
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
			// vllm-ascend is a vLLM plugin and shares its entry point.
			name:   "vllm_ascend_shares_vllms_entrypoint",
			engine: workercore.ModelDeploymentEngineVLLMAscend,
			model:  "Qwen/Qwen2.5-72B-Instruct",
			want:   []string{"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct"},
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
