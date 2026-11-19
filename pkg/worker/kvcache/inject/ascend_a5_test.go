package inject

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	productascend "gpustack.ai/gpustack/pkg/devicemanager/product/ascend"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/json"
)

func TestRender_AscendA5FamilyConstantMatchesProduct(t *testing.T) {
	assert.Equal(t, "950", nodefeature.AcceleratorFamilyAscend950)
	assert.Equal(t, nodefeature.AcceleratorFamilyAscend950, productascend.Family)
}

func assertNoForbiddenAscendA5Artifacts(t *testing.T, result *Result) {
	t.Helper()

	forbiddenEnv := []string{
		"ASCEND_LOCAL_COMM_RES_PATH",
		"HCCL_INTRA_ROCE_ENABLE",
		"ASCEND_ENABLE_USE_FABRIC_MEM",
	}
	for _, name := range forbiddenEnv {
		assert.NotContains(t, envNames(result.Env), name)
		assert.NotContains(t, envNames(result.DefaultedEnv), name)
	}
	for _, arg := range result.Args {
		assert.NotContains(t, arg, "protocol_desc")
		assert.NotContains(t, arg, "roce:device")
	}
}

func vllmTransferDoc(t *testing.T, result *Result) map[string]any {
	t.Helper()

	at := slices.Index(result.Args, vllmTransferConfigArg)
	require.NotEqual(t, -1, at, "expected %s in args %v", vllmTransferConfigArg, result.Args)
	require.Less(t, at+1, len(result.Args))

	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Args[at+1]), &doc))
	return doc
}

func TestRender_VLLMAscendA5UB(t *testing.T) {
	ascendStore := testConnectionFor(EngineVLLMAscend)
	parallelism := ParallelismPair{
		Prefill: Parallelism{TensorParallel: 2, DataParallel: 1},
		Decode:  Parallelism{TensorParallel: 1, DataParallel: 2},
	}

	testCases := []struct {
		name             string
		in               Input
		wantConnector    string
		wantDefaultedEnv []core.EnvVar
	}{
		{
			name: "prefill HIXL without store",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				Parallelism:       parallelism,
			},
			wantConnector: "MooncakeConnectorV1",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_AUTO_CONNECT", Value: "1"},
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
		{
			name: "decode HIXL without store",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleDecode,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				Parallelism:       parallelism,
			},
			wantConnector: "MooncakeConnectorV1",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_AUTO_CONNECT", Value: "1"},
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
		{
			name: "prefill Mooncake without store",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendMooncake,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				Parallelism:       parallelism,
			},
			wantConnector: "MooncakeConnectorV1",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
		{
			name: "decode empty backend without store",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleDecode,
				KVTransfer:        true,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				Parallelism:       parallelism,
			},
			wantConnector: "MooncakeConnectorV1",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
		{
			name: "standalone KVCache RoleNone",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleNone,
				Connection:        ascendStore,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
			},
			wantConnector: "AscendStoreConnector",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
		{
			name: "standalone KVCache RolePrefill",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				Connection:        ascendStore,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
			},
			wantConnector: "AscendStoreConnector",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
		{
			name: "standalone KVCache RoleDecode",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleDecode,
				Connection:        ascendStore,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
			},
			wantConnector: "AscendStoreConnector",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
		{
			name: "prefill HIXL plus KVCache MultiConnector",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				Connection:        ascendStore,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				Parallelism:       parallelism,
			},
			wantConnector: "MultiConnector",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_AUTO_CONNECT", Value: "1"},
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
		{
			name: "prefill Mooncake plus KVCache MultiConnector",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				Connection:        ascendStore,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendMooncake,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				Parallelism:       parallelism,
			},
			wantConnector: "MultiConnector",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
		{
			name: "decode HIXL plus KVCache renders direct connector only",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleDecode,
				Connection:        ascendStore,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				Parallelism:       parallelism,
			},
			wantConnector: "MooncakeConnectorV1",
			wantDefaultedEnv: []core.EnvVar{
				{Name: "ASCEND_AUTO_CONNECT", Value: "1"},
				{Name: "ASCEND_LOCAL_COMM_RES", Value: `{"version":"1.3"}`},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(tc.in)
			require.NoError(t, err)

			assert.Equal(t, tc.wantDefaultedEnv, result.DefaultedEnv)
			assert.NotContains(t, envNames(result.Env), "ASCEND_LOCAL_COMM_RES",
				"ASCEND_LOCAL_COMM_RES must be defaulted rather than owned so user role env overrides work")
			assertNoForbiddenAscendA5Artifacts(t, result)

			assert.NotContains(t, volumeNames(result.Volumes), "gpustack-ascend-hixlep")
			for _, m := range result.VolumeMounts {
				assert.NotEqual(t, "/etc/hixlep", m.MountPath)
			}

			doc := vllmTransferDoc(t, result)
			assert.Equal(t, tc.wantConnector, doc["kv_connector"])
			if extra, ok := doc["kv_connector_extra_config"].(map[string]any); ok {
				assert.NotContains(t, extra, "ascend_local_comm_res_path")
				if children, ok := extra["connectors"].([]any); ok {
					for _, raw := range children {
						child, ok := raw.(map[string]any)
						require.True(t, ok)
						if childExtra, ok := child["kv_connector_extra_config"].(map[string]any); ok {
							assert.NotContains(t, childExtra, "ascend_local_comm_res_path")
						}
					}
				}
			}
		})
	}
}

func TestRender_VLLMAscendA5HostRoCE(t *testing.T) {
	ascendStore := testConnectionFor(EngineVLLMAscend)
	parallelism := ParallelismPair{
		Prefill: Parallelism{TensorParallel: 2, DataParallel: 1},
		Decode:  Parallelism{TensorParallel: 1, DataParallel: 2},
	}

	testCases := []struct {
		name             string
		in               Input
		wantConnector    string
		wantJSON         string
		wantDefaultedEnv []core.EnvVar
		wantMulti        bool
	}{
		{
			name: "standalone MooncakeConnectorV1 prefill HIXL",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				AscendHostRoCE:    true,
				Parallelism:       parallelism,
			},
			wantConnector: "MooncakeConnectorV1",
			wantJSON: `{
				"kv_connector": "MooncakeConnectorV1",
				"kv_role": "kv_producer",
				"kv_port": 8998,
				"kv_connector_extra_config": {
					"prefill": {"tp_size": 2, "dp_size": 1},
					"decode": {"tp_size": 1, "dp_size": 2},
					"ascend_local_comm_res_path": "/etc/hixlep"
				}
			}`,
			wantDefaultedEnv: []core.EnvVar{{Name: "ASCEND_AUTO_CONNECT", Value: "1"}},
		},
		{
			name: "standalone MooncakeConnectorV1 decode Mooncake",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleDecode,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendMooncake,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				AscendHostRoCE:    true,
				Parallelism:       parallelism,
			},
			wantConnector: "MooncakeConnectorV1",
			wantJSON: `{
				"kv_connector": "MooncakeConnectorV1",
				"kv_role": "kv_consumer",
				"kv_port": 8998,
				"kv_connector_extra_config": {
					"prefill": {"tp_size": 2, "dp_size": 1},
					"decode": {"tp_size": 1, "dp_size": 2},
					"ascend_local_comm_res_path": "/etc/hixlep"
				}
			}`,
		},
		{
			name: "standalone MooncakeConnectorV1 decode HIXL with store",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleDecode,
				Connection:        ascendStore,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				AscendHostRoCE:    true,
				Parallelism:       parallelism,
			},
			wantConnector: "MooncakeConnectorV1",
			wantJSON: `{
				"kv_connector": "MooncakeConnectorV1",
				"kv_role": "kv_consumer",
				"kv_port": 8998,
				"kv_connector_extra_config": {
					"prefill": {"tp_size": 2, "dp_size": 1},
					"decode": {"tp_size": 1, "dp_size": 2},
					"ascend_local_comm_res_path": "/etc/hixlep"
				}
			}`,
			wantDefaultedEnv: []core.EnvVar{{Name: "ASCEND_AUTO_CONNECT", Value: "1"}},
		},
		{
			name: "standalone AscendStoreConnector RoleNone",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleNone,
				Connection:        ascendStore,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				AscendHostRoCE:    true,
			},
			wantConnector: "AscendStoreConnector",
			wantJSON: `{
				"kv_connector": "AscendStoreConnector",
				"kv_role": "kv_both",
				"kv_connector_extra_config": {
					"ascend_local_comm_res_path": "/etc/hixlep"
				}
			}`,
		},
		{
			name: "standalone AscendStoreConnector RolePrefill",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				Connection:        ascendStore,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				AscendHostRoCE:    true,
			},
			wantConnector: "AscendStoreConnector",
			wantJSON: `{
				"kv_connector": "AscendStoreConnector",
				"kv_role": "kv_producer",
				"kv_connector_extra_config": {
					"ascend_local_comm_res_path": "/etc/hixlep"
				}
			}`,
		},
		{
			name: "MultiConnector prefill HIXL plus KVCache sets path on outer config only",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				Connection:        ascendStore,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				AscendHostRoCE:    true,
				Parallelism:       parallelism,
			},
			wantConnector: "MultiConnector",
			wantJSON: `{
				"kv_connector": "MultiConnector",
				"kv_role": "kv_producer",
				"kv_connector_extra_config": {
					"connectors": [
						{
							"kv_connector": "MooncakeConnectorV1",
							"kv_role": "kv_producer",
							"kv_port": 8998,
							"kv_connector_extra_config": {
								"prefill": {"tp_size": 2, "dp_size": 1},
								"decode": {"tp_size": 1, "dp_size": 2}
							}
						},
						{
							"kv_connector": "AscendStoreConnector",
							"kv_role": "kv_both"
						}
					],
					"ascend_local_comm_res_path": "/etc/hixlep"
				}
			}`,
			wantDefaultedEnv: []core.EnvVar{{Name: "ASCEND_AUTO_CONNECT", Value: "1"}},
			wantMulti:        true,
		},
		{
			name: "MultiConnector prefill Mooncake plus KVCache sets path on outer config only",
			in: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				Connection:        ascendStore,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendMooncake,
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				AscendHostRoCE:    true,
				Parallelism:       parallelism,
			},
			wantConnector: "MultiConnector",
			wantJSON: `{
				"kv_connector": "MultiConnector",
				"kv_role": "kv_producer",
				"kv_connector_extra_config": {
					"connectors": [
						{
							"kv_connector": "MooncakeConnectorV1",
							"kv_role": "kv_producer",
							"kv_port": 8998,
							"kv_connector_extra_config": {
								"prefill": {"tp_size": 2, "dp_size": 1},
								"decode": {"tp_size": 1, "dp_size": 2}
							}
						},
						{
							"kv_connector": "AscendStoreConnector",
							"kv_role": "kv_both"
						}
					],
					"ascend_local_comm_res_path": "/etc/hixlep"
				}
			}`,
			wantMulti: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(tc.in)
			require.NoError(t, err)

			assert.Equal(t, tc.wantDefaultedEnv, result.DefaultedEnv)
			assert.NotContains(t, envNames(result.Env), "ASCEND_LOCAL_COMM_RES")
			assert.NotContains(t, envNames(result.DefaultedEnv), "ASCEND_LOCAL_COMM_RES")
			assertNoForbiddenAscendA5Artifacts(t, result)

			// Verify /etc/hixlep HostPathDirectory read-only mount.
			var hixlepVol *core.Volume
			for i := range result.Volumes {
				if result.Volumes[i].Name == "gpustack-ascend-hixlep" {
					hixlepVol = &result.Volumes[i]
					break
				}
			}
			require.NotNil(t, hixlepVol, "expected gpustack-ascend-hixlep volume in %v", volumeNames(result.Volumes))
			require.NotNil(t, hixlepVol.HostPath)
			assert.Equal(t, "/etc/hixlep", hixlepVol.HostPath.Path)
			require.NotNil(t, hixlepVol.HostPath.Type)
			assert.Equal(t, core.HostPathDirectory, *hixlepVol.HostPath.Type)
			assert.NotEqual(t, core.HostPathDirectoryOrCreate, *hixlepVol.HostPath.Type)

			assert.Contains(t, result.VolumeMounts, core.VolumeMount{
				Name:      "gpustack-ascend-hixlep",
				MountPath: "/etc/hixlep",
				ReadOnly:  true,
			})

			at := slices.Index(result.Args, vllmTransferConfigArg)
			require.NotEqual(t, -1, at)
			assert.JSONEq(t, tc.wantJSON, result.Args[at+1])

			doc := vllmTransferDoc(t, result)
			assert.Equal(t, tc.wantConnector, doc["kv_connector"])
			outerExtra, ok := doc["kv_connector_extra_config"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "/etc/hixlep", outerExtra["ascend_local_comm_res_path"])

			if tc.wantMulti {
				children, ok := outerExtra["connectors"].([]any)
				require.True(t, ok)
				require.Len(t, children, 2)
				for idx, raw := range children {
					child, ok := raw.(map[string]any)
					require.True(t, ok)
					if childExtra, hasExtra := child["kv_connector_extra_config"].(map[string]any); hasExtra {
						assert.NotContains(t, childExtra, "ascend_local_comm_res_path",
							"child connector %d must not carry ascend_local_comm_res_path", idx)
					}
				}
			}
		})
	}
}

func TestRender_VLLMAscendA5EncodeUntouched(t *testing.T) {
	baseline, err := Render(Input{
		Engine:       EngineVLLMAscend,
		Role:         RoleEncode,
		EncoderCache: sharedLeg(),
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		hostRoCE bool
	}{
		{name: "A5 UB encode", hostRoCE: false},
		{name: "A5 Host RoCE encode", hostRoCE: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleEncode,
				EncoderCache:      sharedLeg(),
				AcceleratorFamily: nodefeature.AcceleratorFamilyAscend950,
				AscendHostRoCE:    tc.hostRoCE,
			})
			require.NoError(t, err)

			assert.Equal(t, baseline, got)
			assert.Empty(t, got.DefaultedEnv)
			assert.Empty(t, got.Volumes)
			assert.Empty(t, got.VolumeMounts)
			assert.NotContains(t, got.Args, vllmTransferConfigArg)
		})
	}
}

func TestRender_SGLangAscendA5(t *testing.T) {
	testCases := []struct {
		name         string
		role         Role
		family       string
		hostRoCE     bool
		wantProtocol string
	}{
		{
			name:         "A5 prefill HIXL defaults device_urma",
			role:         RolePrefill,
			family:       nodefeature.AcceleratorFamilyAscend950,
			hostRoCE:     false,
			wantProtocol: "device_urma",
		},
		{
			name:         "A5 decode HIXL defaults device_urma",
			role:         RoleDecode,
			family:       nodefeature.AcceleratorFamilyAscend950,
			hostRoCE:     false,
			wantProtocol: "device_urma",
		},
		{
			name:         "A5 prefill HIXL ignores AscendHostRoCE and never mounts hixlep",
			role:         RolePrefill,
			family:       nodefeature.AcceleratorFamilyAscend950,
			hostRoCE:     true,
			wantProtocol: "device_urma",
		},
		{
			name:         "A5 decode HIXL ignores AscendHostRoCE and never mounts hixlep",
			role:         RoleDecode,
			family:       nodefeature.AcceleratorFamilyAscend950,
			hostRoCE:     true,
			wantProtocol: "device_urma",
		},
		{
			name:         "A2 910B prefill HIXL keeps sdma",
			role:         RolePrefill,
			family:       "910B",
			wantProtocol: "sdma",
		},
		{
			name:         "A3 910C decode HIXL keeps sdma",
			role:         RoleDecode,
			family:       "910C",
			wantProtocol: "sdma",
		},
		{
			name:         "empty family prefill HIXL keeps sdma",
			role:         RolePrefill,
			family:       "",
			wantProtocol: "sdma",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(Input{
				Engine:            EngineSGLang,
				Role:              tc.role,
				Disaggregated:     true,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
				AcceleratorFamily: tc.family,
				AscendHostRoCE:    tc.hostRoCE,
			})
			require.NoError(t, err)

			assert.Equal(t, []core.EnvVar{
				{Name: "ASCEND_MF_TRANSFER_PROTOCOL", Value: tc.wantProtocol},
			}, result.DefaultedEnv)
			assert.NotContains(t, envNames(result.Env), "ASCEND_MF_TRANSFER_PROTOCOL",
				"ASCEND_MF_TRANSFER_PROTOCOL must be defaulted rather than owned so user role env overrides work")
			assert.NotContains(t, envNames(result.Env), "ASCEND_LOCAL_COMM_RES")
			assert.NotContains(t, envNames(result.DefaultedEnv), "ASCEND_LOCAL_COMM_RES")
			assertNoForbiddenAscendA5Artifacts(t, result)

			assert.Equal(t, []string{"gpustack-ascend-driver"}, volumeNames(result.Volumes))
			assert.Equal(t, []core.VolumeMount{
				{Name: "gpustack-ascend-driver", MountPath: "/usr/local/Ascend/driver", ReadOnly: true},
			}, result.VolumeMounts)
			for _, arg := range result.Args {
				assert.NotContains(t, arg, "ascend_local_comm_res_path")
			}
		})
	}
}

func TestRender_AscendLegacyAndNonAscendAreByteIdentical(t *testing.T) {
	ascendStore := testConnectionFor(EngineVLLMAscend)
	vllmStore := testConnectionFor(EngineVLLM)

	testCases := []struct {
		name     string
		baseline Input
		variants []struct {
			label    string
			family   string
			hostRoCE bool
		}
	}{
		{
			name: "vllm-ascend standalone KVCache",
			baseline: Input{
				Engine:     EngineVLLMAscend,
				Role:       RoleNone,
				Connection: ascendStore,
			},
			variants: []struct {
				label    string
				family   string
				hostRoCE bool
			}{
				{label: "910B", family: "910B"},
				{label: "910C", family: "910C"},
				{label: "910C with AscendHostRoCE ignored", family: "910C", hostRoCE: true},
				{label: "empty with AscendHostRoCE ignored", family: "", hostRoCE: true},
			},
		},
		{
			name: "vllm-ascend prefill HIXL plus KVCache MultiConnector",
			baseline: Input{
				Engine:            EngineVLLMAscend,
				Role:              RolePrefill,
				Connection:        ascendStore,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
			},
			variants: []struct {
				label    string
				family   string
				hostRoCE bool
			}{
				{label: "910B", family: "910B"},
				{label: "910C", family: "910C"},
				{label: "910B with AscendHostRoCE ignored", family: "910B", hostRoCE: true},
			},
		},
		{
			name: "vllm-ascend decode HIXL",
			baseline: Input{
				Engine:            EngineVLLMAscend,
				Role:              RoleDecode,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
			},
			variants: []struct {
				label    string
				family   string
				hostRoCE bool
			}{
				{label: "910B", family: "910B"},
				{label: "910C", family: "910C"},
				{label: "empty with AscendHostRoCE ignored", family: "", hostRoCE: true},
			},
		},
		{
			name: "sglang prefill HIXL",
			baseline: Input{
				Engine:            EngineSGLang,
				Role:              RolePrefill,
				Disaggregated:     true,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendHIXL,
			},
			variants: []struct {
				label    string
				family   string
				hostRoCE bool
			}{
				{label: "910B", family: "910B"},
				{label: "910C", family: "910C"},
				{label: "910C with AscendHostRoCE ignored", family: "910C", hostRoCE: true},
			},
		},
		{
			name: "sglang store only on A5 unchanged",
			baseline: Input{
				Engine:     EngineSGLang,
				Role:       RoleNone,
				Connection: vllmStore,
			},
			variants: []struct {
				label    string
				family   string
				hostRoCE bool
			}{
				{label: "950", family: nodefeature.AcceleratorFamilyAscend950},
				{label: "950 with AscendHostRoCE ignored", family: nodefeature.AcceleratorFamilyAscend950, hostRoCE: true},
				{label: "910C", family: "910C"},
			},
		},
		{
			name: "vllm NVIDIA/AMD prefill plus KVCache",
			baseline: Input{
				Engine:            EngineVLLM,
				Role:              RolePrefill,
				Connection:        vllmStore,
				KVTransfer:        true,
				KVTransferBackend: KVTransferBackendNIXL,
			},
			variants: []struct {
				label    string
				family   string
				hostRoCE bool
			}{
				{label: "NVIDIA Hopper", family: "Hopper"},
				{label: "AMD CDNA3", family: "CDNA3"},
				{label: "non-ascend engine ignores 950 and AscendHostRoCE", family: nodefeature.AcceleratorFamilyAscend950, hostRoCE: true},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := Render(tc.baseline)
			require.NoError(t, err)

			for _, v := range tc.variants {
				t.Run(v.label, func(t *testing.T) {
					in := tc.baseline
					in.AcceleratorFamily = v.family
					in.AscendHostRoCE = v.hostRoCE

					got, err := Render(in)
					require.NoError(t, err)
					assert.Equal(t, want, got)
				})
			}
		})
	}
}
