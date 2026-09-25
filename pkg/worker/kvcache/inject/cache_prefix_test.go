package inject

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCachePrefix = "m-0123456789abcdef0123456789abcdef"

// transferDoc decodes the --kv-transfer-config document of a rendered vLLM command line.
func transferDoc(t *testing.T, args []string) vllmTransferConfig {
	t.Helper()
	i := slices.Index(args, "--kv-transfer-config")
	require.GreaterOrEqual(t, i, 0, "no --kv-transfer-config in %v", args)
	var doc vllmTransferConfig
	require.NoError(t, json.Unmarshal([]byte(args[i+1]), &doc))

	return doc
}

func TestRenderCachePrefix(t *testing.T) {
	cases := []struct {
		name   string
		input  Input
		assert func(t *testing.T, doc vllmTransferConfig)
	}{
		{
			name:  "the store connector carries the prefix",
			input: Input{Engine: EngineVLLM, Role: RoleNone, Connection: testConnectionFor(EngineVLLM), CachePrefix: testCachePrefix},
			assert: func(t *testing.T, doc vllmTransferConfig) {
				require.NotNil(t, doc.KVConnectorExtraConfig)
				assert.Equal(t, testCachePrefix, doc.KVConnectorExtraConfig.CachePrefix)
			},
		},
		{
			name: "inside the composite, the store connector carries it and the leg does not",
			input: Input{
				Engine: EngineVLLM, Role: RolePrefill, Disaggregated: true, KVTransfer: true,
				Connection: testConnectionFor(EngineVLLM), CachePrefix: testCachePrefix,
			},
			assert: func(t *testing.T, doc vllmTransferConfig) {
				require.Equal(t, "MultiConnector", doc.KVConnector)
				connectors := doc.KVConnectorExtraConfig.Connectors
				require.Len(t, connectors, 2)
				assert.Equal(t, "MooncakeConnector", connectors[0].KVConnector)
				assert.Empty(t, connectors[0].KVConnectorExtraConfig.CachePrefix)
				assert.Equal(t, "MooncakeStoreConnector", connectors[1].KVConnector)
				require.NotNil(t, connectors[1].KVConnectorExtraConfig)
				assert.Equal(t, testCachePrefix, connectors[1].KVConnectorExtraConfig.CachePrefix)
				assert.Empty(t, doc.KVConnectorExtraConfig.CachePrefix, "the outer document is no store")
			},
		},
		{
			name:  "no prefix renders no extra configuration",
			input: Input{Engine: EngineVLLM, Role: RoleNone, Connection: testConnectionFor(EngineVLLM)},
			assert: func(t *testing.T, doc vllmTransferConfig) {
				assert.Nil(t, doc.KVConnectorExtraConfig)
			},
		},
		{
			name:  "the Ascend store connector is left alone",
			input: Input{Engine: EngineVLLMAscend, Role: RoleNone, Connection: testConnectionFor(EngineVLLMAscend), CachePrefix: testCachePrefix},
			assert: func(t *testing.T, doc vllmTransferConfig) {
				assert.Nil(t, doc.KVConnectorExtraConfig)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := Render(c.input)
			require.NoError(t, err)
			c.assert(t, transferDoc(t, result.Args))
		})
	}
}

func TestRenderBackendTag(t *testing.T) {
	cases := []struct {
		name  string
		input Input
		want  string
	}{
		{
			name:  "the store gets the tag as its only extra configuration",
			input: Input{Engine: EngineSGLang, Role: RoleNone, Connection: testConnectionFor(EngineSGLang), CachePrefix: testCachePrefix},
			want:  `{"extra_backend_tag":"` + testCachePrefix + `"}`,
		},
		{
			name:  "no prefix renders no extra configuration",
			input: Input{Engine: EngineSGLang, Role: RoleNone, Connection: testConnectionFor(EngineSGLang)},
		},
		{
			name:  "no store renders no extra configuration",
			input: Input{Engine: EngineSGLang, Role: RolePrefill, Disaggregated: true, KVTransfer: true, CachePrefix: testCachePrefix},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := Render(c.input)
			require.NoError(t, err)
			i := slices.Index(result.Args, sglangBackendExtraConfigArg)
			if c.want == "" {
				assert.Equal(t, -1, i, "%v", result.Args)
				return
			}
			require.GreaterOrEqual(t, i, 0)
			assert.JSONEq(t, c.want, result.Args[i+1])
			// The store's settings still come from the environment: the extra configuration names
			// no address, so SGLang does not select the loader that would read them from it.
			assert.NotContains(t, result.Args[i+1], "master_server_address")
			assert.NotEmpty(t, envValue(t, result.Env, sglangMasterEnv).Value)
		})
	}
}

func TestRenderRefusesACachePrefixWithASeparator(t *testing.T) {
	cases := []struct {
		name   string
		engine Engine
		prefix string
	}{
		{name: "vLLM, an at sign", engine: EngineVLLM, prefix: "m-a@b"},
		{name: "vLLM, an underscore", engine: EngineVLLM, prefix: "m-a_b"},
		{name: "SGLang, a colon", engine: EngineSGLang, prefix: "m-a:b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Render(Input{Engine: c.engine, Role: RoleNone, Connection: testConnectionFor(c.engine), CachePrefix: c.prefix})
			refusal, ok := errors.AsType[*RefusalError](err)
			require.True(t, ok, "want a refusal, got %v", err)
			assert.Equal(t, ReasonCachePrefixInvalid, refusal.Reason)
		})
	}
}
