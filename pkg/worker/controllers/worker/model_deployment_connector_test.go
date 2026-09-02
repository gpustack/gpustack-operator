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
	// The client JSON is the same on all three engines, which is the point rather than an
	// oversight: the three readers' key sets differ only in keys the operator does not choose.
	wantConfig := map[string]string{
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
		},
		{
			name:   "sglang_golden",
			engine: workercore.ModelDeploymentEngineSGLang,
			wantArgs: []string{
				"--hicache-storage-backend", "mooncake",
				"--hicache-storage-backend-extra-config",
				`{"master_server_address":"shared-kv-master.gpustack-system.svc:50051"}`,
			},
			wantEnv: []core.EnvVar{
				{Name: "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"},
			},
			wantDefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
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
			assert.Equal(t, wantConfig, got.ClientConfig)
		})
	}
}

// TestSynthesizeModelDeploymentConnector_KeysNeverRendered states, per key, WHY the operator leaves
// it out. Exact map equality above already fails if any of them appears; these cases exist so that
// whoever deletes one has to read the reason first.
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
			why: "every engine derives it from its own process, and one deployment-wide file " +
				"cannot hold a value that differs per replica",
		},
		{
			name: "no_segment_sizes_global",
			key:  "global_segment_size",
			why:  "it sizes a replica's own contribution to the pool, so a wrong operator-chosen value is a silent capacity error",
		},
		{
			name: "no_segment_sizes_local",
			key:  "local_buffer_size",
			why:  "same as the global segment size: not the operator's to invent",
		},
		{
			name: "no_mode",
			key:  "mode",
			why: "vLLM's mode carries a cross-field rule against global_segment_size, so choosing " +
				"one half of a pair whose other half is not ours to choose is how an engine " +
				"refuses its own configuration",
		},
	}

	engines := []string{
		workercore.ModelDeploymentEngineVLLM,
		workercore.ModelDeploymentEngineVLLMAscend,
		workercore.ModelDeploymentEngineSGLang,
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, engine := range engines {
				got, err := SynthesizeModelDeploymentConnector(connectorInput(engine))
				require.NoError(t, err)
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
			name:     "sglang_owns_its_own_config_path_only",
			engine:   workercore.ModelDeploymentEngineSGLang,
			env:      "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH",
			wantsEnv: true,
		},
		{
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
