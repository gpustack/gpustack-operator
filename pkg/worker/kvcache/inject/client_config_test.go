package inject

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
)

// vllmReadableKeys is every key vLLM's own file reader takes, transcribed from `worker.py:129-142`.
//
// It is a literal list rather than anything derived from the renderer, and that independence is the
// whole point: a subset assertion against a list built from the code under test would accept whatever
// that code emitted. This list is the engine's contract, so a key the renderer adds without the engine
// growing a reader for it fails here.
var vllmReadableKeys = sets.New(
	"metadata_server",
	"master_server_address",
	"protocol",
	"device_name",
	"mode",
	"global_segment_size",
	"local_buffer_size",
	"enable_offload",
)

// sglangReadableVariables is every variable SGLang's `load_from_env` reads, transcribed from
// v0.5.18 `mooncake_store.py:170-210` with the names from that release's Mooncake store block in
// `environ.py:698-712`. A literal list, for the same reason as above.
var sglangReadableVariables = sets.New(
	"MOONCAKE_LOCAL_HOSTNAME",
	"MOONCAKE_TE_META_DATA_SERVER",
	"MOONCAKE_GLOBAL_SEGMENT_SIZE",
	"MOONCAKE_PROTOCOL",
	"MOONCAKE_DEVICE",
	"MOONCAKE_MASTER",
	"MOONCAKE_MASTER_METRICS_PORT",
	"MOONCAKE_CHECK_SERVER",
	"MOONCAKE_STANDALONE_STORAGE",
	"MOONCAKE_CLIENT",
)

// testConnection is a resolved connection, as the caller would hand one over.
func testConnection() Connection {
	return Connection{MasterAddress: "kvcache-master.gpustack-system.svc:50051", Protocol: "tcp"}
}

// renderedConfig runs a vLLM-family render and returns the projected file, decoded.
func renderedConfig(t *testing.T, in Input) map[string]any {
	t.Helper()

	result, err := Render(in)
	require.NoError(t, err)

	raw, ok := result.PodAnnotations[ClientConfigAnnotationKey]
	require.True(t, ok, "the file vehicle carries its content on the annotation it projects from")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
	return decoded
}

// TestVLLMConfig_KeysAreOnesTheEngineReads is the key-set gate. A key outside the engine's reader is
// not a harmless addition: that reader takes its keys one by one with no passthrough, so nothing ever
// sees it, and the value it was meant to carry silently falls to a default.
func TestVLLMConfig_KeysAreOnesTheEngineReads(t *testing.T) {
	for _, engine := range []Engine{EngineVLLM, EngineVLLMAscend} {
		t.Run(string(engine), func(t *testing.T) {
			config := renderedConfig(t, Input{Engine: engine, Connection: testConnectionFor(engine)})

			rendered := sets.KeySet(config)
			assert.Empty(t, rendered.Difference(vllmReadableKeys).UnsortedList(),
				"these keys are not in %s's reader, so nothing would ever read them", engine)
		})
	}
}

// TestVLLMConfig_UsesTheFileSpellingNotTheSignature pins the two names the file and the client's
// setup() signature disagree on. Getting either backwards is silent: the reader ignores the key it
// does not recognize and takes its default.
func TestVLLMConfig_UsesTheFileSpellingNotTheSignature(t *testing.T) {
	config := renderedConfig(t, Input{Engine: EngineVLLM, Connection: testConnection()})

	assert.Contains(t, config, "master_server_address")
	assert.NotContains(t, config, "master_server_addr", "that spelling is the setup() parameter's")

	assert.Contains(t, config, "device_name")
	assert.NotContains(t, config, "rdma_devices", "that spelling is the setup() parameter's")
}

// TestVLLMConfig_RoleDeclaringKeysMoveTogether asserts the three as one unit.
//
// They are not three sizes. Together they declare that the instance is a pure client; any one of them
// alone declares something else, and the engine either raises at startup or quietly becomes a store
// member. So no test asserts one without the others.
func TestVLLMConfig_RoleDeclaringKeysMoveTogether(t *testing.T) {
	config := renderedConfig(t, Input{Engine: EngineVLLM, Connection: testConnection()})

	assert.Equal(t, float64(0), config["global_segment_size"])
	assert.Equal(t, "standalone-store", config["mode"])
	assert.Equal(t, float64(128*1024*1024), config["local_buffer_size"])

	// Above zero is a separate contract from being the constant: a zero here declares a pure SERVER,
	// which may not Get or Put. That is a silently useless client rather than an error.
	buffer, ok := config["local_buffer_size"].(float64)
	require.True(t, ok)
	assert.Positive(t, buffer)
}

// TestVLLMConfig_ConstantsAreLiteral checks the two values that carry no input, byte for byte rather
// than against the package's own constants. A constant compared with itself passes whatever it holds.
func TestVLLMConfig_ConstantsAreLiteral(t *testing.T) {
	config := renderedConfig(t, Input{Engine: EngineVLLM, Connection: testConnection()})

	assert.Equal(t, "P2PHANDSHAKE", config["metadata_server"],
		"the metadata plane is peer-to-peer; this is a constant, never an address")
	assert.Equal(t, "", config["device_name"],
		"empty means discover per host, which is the only value correct for every host in one pool")
}

// TestVLLMConfig_CarriesTheResolvedConnection checks the two values that do come from the input.
func TestVLLMConfig_CarriesTheResolvedConnection(t *testing.T) {
	conn := testConnection()
	config := renderedConfig(t, Input{Engine: EngineVLLM, Connection: conn})

	assert.Equal(t, conn.MasterAddress, config["master_server_address"])
	assert.Equal(t, conn.Protocol, config["protocol"])
}

// TestVLLMConfig_ProtocolIsWrittenOnEveryTransport pins that the key is present whatever the transport
// is. The engines disagree on the default for an absent protocol - vLLM assumes rdma, SGLang tcp - so
// omitting it would pick one of them by accident.
func TestVLLMConfig_ProtocolIsWrittenOnEveryTransport(t *testing.T) {
	for _, protocol := range []string{"tcp", "rdma", "ascend"} {
		t.Run(protocol, func(t *testing.T) {
			config := renderedConfig(t, Input{
				Engine:     EngineVLLM,
				Connection: Connection{MasterAddress: "master:50051", Protocol: protocol},
			})
			assert.Equal(t, protocol, config["protocol"])
		})
	}
}

// TestVLLMConfig_DeviceNameEmptyOnEveryPath includes the RDMA path, where a reader might expect a
// device list. Empty is correct there too: a device is named per host.
func TestVLLMConfig_DeviceNameEmptyOnEveryPath(t *testing.T) {
	for _, protocol := range []string{"tcp", "rdma"} {
		t.Run(protocol, func(t *testing.T) {
			config := renderedConfig(t, Input{
				Engine:     EngineVLLM,
				Connection: Connection{MasterAddress: "master:50051", Protocol: protocol},
			})

			value, present := config["device_name"]
			require.True(t, present, "written rather than omitted, so the behaviour is predictable")
			assert.Equal(t, "", value)
		})
	}
}
