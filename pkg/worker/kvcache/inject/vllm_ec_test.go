package inject

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
)

// The encoder-cache leg is exercised through renderVLLMWithEncoderCache rather than Render, because
// Render's gate admits an input that asks for the store, direct transfer or event publishing and
// none of those is this leg. Reaching it needs a field on Input the package does not carry yet; the
// two call sites that add it are named in the task report, and nothing else about this file changes
// when they land.

// nixlLeg and sharedLeg are the two complete requests, so a case names the leg it exercises by
// naming one of these rather than restating three fields.
func nixlLeg() EncoderCache {
	return EncoderCache{Connector: vllmECCPUConnector, CPUBytes: 1_073_741_824}
}

func sharedLeg() EncoderCache {
	return EncoderCache{Connector: vllmECExampleConnector, SharedStoragePath: "/mnt/ec"}
}

// renderedFlags returns the flag names of an argument list. Every list this package renders
// alternates a flag with its value, which is what lets the owned-key proof below compare the leg's
// whole output against the keys it claims to own instead of searching for one substring.
func renderedFlags(t *testing.T, args []string) []string {
	t.Helper()

	require.Zero(t, len(args)%2, "every rendered argument is a flag followed by its value: %v", args)
	flags := make([]string, 0, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		flags = append(flags, args[i])
	}
	return flags
}

// ecDocument returns the rendered --ec-transfer-config document as the text the engine parses.
func ecDocument(t *testing.T, result *Result) string {
	t.Helper()

	at := slices.Index(result.Args, vllmECTransferConfigArg)
	require.NotEqual(t, -1, at, "the leg must name its argument: %v", result.Args)
	require.Less(t, at+1, len(result.Args), "a flag with no value renders nothing: %v", result.Args)
	return result.Args[at+1]
}

// ecConfig returns the rendered --ec-transfer-config document, decoded.
func ecConfig(t *testing.T, result *Result) map[string]any {
	t.Helper()

	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(ecDocument(t, result)), &doc))
	return doc
}

// ecExtra returns the document's connector-specific block, decoded.
func ecExtra(t *testing.T, result *Result) map[string]any {
	t.Helper()

	extra, ok := ecConfig(t, result)["ec_connector_extra_config"].(map[string]any)
	require.True(t, ok, "the document must carry a connector block: %v", ecConfig(t, result))
	return extra
}

// TestRenderVLLM_EncodeCarriesTheEncoderCacheArmAndNoKVKey is the positive half of the encoder
// contract and the reason its negative half is worth an assertion of its own.
//
// An encoder stage allocates no KV cache - upstream states that GPUModelRunner.get_kv_cache_spec
// returns {} for it (read at vLLM v0.30.0 `vllm/config/ec_transfer.py:112-119`) - so every KV key
// is dropped rather than filled with a role the engine cannot act on. The store row is what carries
// that weight: it hands the renderer a complete, legal connection, and a renderer that read the
// store block before the role would project a client configuration file onto a Pod with no cache to
// address.
func TestRenderVLLM_EncodeCarriesTheEncoderCacheArmAndNoKVKey(t *testing.T) {
	testCases := []struct {
		name     string
		in       Input
		ec       EncoderCache
		document string
	}{
		{
			name: "the NIXL path moves encodings between Pods",
			in:   Input{Engine: EngineVLLM, Role: RoleEncode},
			ec:   nixlLeg(),
			document: `{"ec_connector":"ECCPUConnector","ec_role":"ec_producer",
				"ec_connector_extra_config":{"ec_enable_nixl":true,"ec_cpu_bytes":1073741824}}`,
		},
		{
			name: "the shared path moves encodings through the mounted directory",
			in:   Input{Engine: EngineVLLM, Role: RoleEncode},
			ec:   sharedLeg(),
			document: `{"ec_connector":"ECExampleConnector","ec_role":"ec_producer",
				"ec_connector_extra_config":{"shared_storage_path":"/mnt/ec"}}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := renderVLLMWithEncoderCache(tc.in, tc.ec)
			require.NoError(t, err)

			// The whole argument list, so a KV flag appearing on an encoder is named in the failure
			// rather than swallowed by an assertion about the one flag that should be there.
			assert.Equal(t, []string{vllmECTransferConfigArg}, renderedFlags(t, result.Args),
				"the encoder-cache document is the only argument an encoder stage takes")
			assert.JSONEq(t, tc.document, ecDocument(t, result))

			assert.False(t, result.KVTransfer, "an encoder stage transfers no attention blocks")
			assert.Nil(t, result.KVEvents, "an encoder stage writes no KV cache to publish events about")
			assert.Empty(t, result.Volumes, "no client configuration is projected: there is no cache to address")
			assert.Empty(t, result.VolumeMounts)
			assert.Empty(t, result.PodAnnotations)
			for _, arg := range []string{vllmTransferConfigArg, KVCacheDtypeArg, vllmKVEventsConfigArg} {
				assert.NotContains(t, result.Args, arg, "%s is a KV key and this Pod has no KV cache", arg)
			}
		})
	}
}

// TestRenderVLLM_EncoderConfigCarriesNoKeyFromTheOtherConnector pins which keys belong to which
// path. It is the half of the two-path contract the JSON comparison above cannot see: a document
// carrying both paths' keys would still satisfy a comparison written on the one key under test.
func TestRenderVLLM_EncoderConfigCarriesNoKeyFromTheOtherConnector(t *testing.T) {
	nixl, err := renderVLLMEncoderCache(
		Input{Engine: EngineVLLM, Role: RoleEncode}, nixlLeg())
	require.NoError(t, err)
	shared, err := renderVLLMEncoderCache(
		Input{Engine: EngineVLLM, Role: RoleEncode}, sharedLeg())
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"ec_enable_nixl": true,
		"ec_cpu_bytes":   float64(1_073_741_824),
	}, ecExtra(t, nixl))
	assert.Equal(t, map[string]any{"shared_storage_path": "/mnt/ec"}, ecExtra(t, shared))

	// The enabled flag is a JSON boolean rather than the string "true". Extra config is not type
	// coerced, so the engine parses the strings "true", "1" and "yes" as enabled and anything
	// else as not (read at vLLM v0.30.0 `docs/features/ec_cpu_connector.md:97`): a string would
	// work by accident, and a number would silently disable the transfer.
	assert.IsType(t, true, ecExtra(t, nixl)["ec_enable_nixl"],
		"a non-boolean would be parsed by string coercion rather than read as a switch")
}

// TestRenderVLLM_NIXLProducerPublishesTheSideChannelTheConsumerDials is the pair the two halves of
// a NIXL leg must agree on, and it is a pair rather than two independent settings: the consumer
// reads neither variable, so a producer that bound its socket to the upstream default of
// "localhost" would be dialed by nothing on any other host.
func TestRenderVLLM_NIXLProducerPublishesTheSideChannelTheConsumerDials(t *testing.T) {
	producer, err := renderVLLMEncoderCache(Input{Engine: EngineVLLM, Role: RoleEncode}, nixlLeg())
	require.NoError(t, err)

	assert.Equal(t, []core.EnvVar{
		// The connector raises ValueError at construction without the model runner, BEFORE it
		// looks at the role it was handed (read at vLLM v0.30.0
		// `vllm/distributed/ec_transfer/ec_connector/cpu/connector.py:40-44`), so it is owned
		// rather than defaulted: a workload answering "0" would leave a leg that cannot start.
		{Name: "VLLM_USE_V2_MODEL_RUNNER", Value: "1"},
		{
			// The field reference, not a value: the address is not knowable at admission, and the
			// kubelet resolving it at container start is what makes it the producer's own.
			Name: "VLLM_EC_SIDE_CHANNEL_HOST",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{FieldPath: "status.podIP"},
			},
		},
		{Name: "VLLM_EC_SIDE_CHANNEL_PORT", Value: "5601"},
	}, producer.Env, "the whole environment: these three are the producer's preconditions")

	assert.Equal(t, []core.ContainerPort{{
		Name: "ec-side-channel", Protocol: core.ProtocolTCP, ContainerPort: 5601,
	}}, producer.Ports, "the port is opened on the Pod so the prefiller's Pod can reach it")

	consumer, err := renderVLLMEncoderCache(Input{Engine: EngineVLLM, Role: RolePrefill}, nixlLeg())
	require.NoError(t, err)

	// The consumer constructs the same connector class: the role the factory passes to __init__ is
	// worker-or-scheduler, not produce-or-consume (read at vLLM v0.30.0
	// `vllm/distributed/ec_transfer/ec_connector/factory.py:36-56`), so the model runner is a
	// precondition here too and a prefiller without it crash-loops at construction.
	assert.Equal(t, []core.EnvVar{{Name: "VLLM_USE_V2_MODEL_RUNNER", Value: "1"}}, consumer.Env,
		"the model runner is the connector's precondition on both roles")
	assert.Empty(t, consumer.Ports, "the consumer dials the producer and listens on nothing")

	// The side channel is the producer's alone: it is read only when NIXL transfer is enabled on a
	// producer (read at vLLM v0.30.0 `docs/features/ec_cpu_connector.md:108`), so a consumer
	// rendering the pair would advertise an address it never binds.
	assert.NotContains(t, envNames(consumer.Env), "VLLM_EC_SIDE_CHANNEL_HOST")
	assert.NotContains(t, envNames(consumer.Env), "VLLM_EC_SIDE_CHANNEL_PORT")
}

// TestRenderVLLM_TheSharedLegNeedsNoModelRunner is the other connector's answer to the same
// question. ECExampleConnector.__init__ reads no model-runner flag (read at vLLM v0.30.0
// `example_connector.py:54-67`), so rendering the variable there would set a switch the leg does not
// depend on, and would claim an owned key the leg never writes.
func TestRenderVLLM_TheSharedLegNeedsNoModelRunner(t *testing.T) {
	for _, role := range []Role{RoleEncode, RolePrefill} {
		t.Run(string(role), func(t *testing.T) {
			leg, err := renderVLLMEncoderCache(Input{Engine: EngineVLLM, Role: role}, sharedLeg())
			require.NoError(t, err)

			assert.Empty(t, leg.Env, "the shared-directory connector reads no model-runner flag")
			assert.Empty(t, leg.Ports, "and it opens no side channel")
		})
	}
}

// TestRenderVLLM_EncoderFlagsRenderOnTheProducerAlone covers four defaults and the one role they
// must never reach.
//
// Every one of them configures an engine that runs no language model. On a prefiller --mm-encoder-only
// would skip the very model that has to read the encodings back, so a leg rendered for the whole
// pair rather than for its producer end is a prefill half that answers nothing.
func TestRenderVLLM_EncoderFlagsRenderOnTheProducerAlone(t *testing.T) {
	producer, err := renderVLLMEncoderCache(Input{Engine: EngineVLLM, Role: RoleEncode}, nixlLeg())
	require.NoError(t, err)

	// One group each, so a caller drops any one of them without losing the rest, and each group
	// starts with the flag that decides whether it applies.
	assert.Equal(t, [][]string{
		{"--enforce-eager"},
		{"--no-enable-prefix-caching"},
		{"--max-num-batched-tokens", "114688"},
		{"--mm-encoder-only"},
	}, producer.DefaultedArgs)

	consumer, err := renderVLLMEncoderCache(Input{Engine: EngineVLLM, Role: RolePrefill}, sharedLeg())
	require.NoError(t, err)
	assert.Empty(t, consumer.DefaultedArgs, "a prefiller runs a language model and keeps its own KV cache")
}

// TestRenderVLLM_PrefillComposesTheConsumerWithItsKVProducer covers the only Pod that carries both
// legs, which is the whole reason the arms compose rather than replace.
//
// The KV rows are the point. A prefiller's KV rendering is what a deployment without any encoder
// stage already gets, so the encoder-cache arm has to leave it alone rather than re-deriving it.
func TestRenderVLLM_PrefillComposesTheConsumerWithItsKVProducer(t *testing.T) {
	testCases := []struct {
		name     string
		in       Input
		ec       EncoderCache
		flags    []string
		kv       string
		document string
	}{
		{
			name: "a pool-backed prefiller keeps its store rendering",
			in:   Input{Engine: EngineVLLM, Role: RolePrefill, Connection: testConnection(), Dtype: "fp8"},
			ec:   nixlLeg(),
			// The dtype is the store's own key and it survives the composition, which is the whole
			// claim: the arm adds a document and takes nothing away.
			flags: []string{vllmTransferConfigArg, vllmECTransferConfigArg, KVCacheDtypeArg},
			kv:    `{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_producer"}`,
			document: `{"ec_connector":"ECCPUConnector","ec_role":"ec_consumer",
				"ec_connector_extra_config":{"ec_enable_nixl":true,"ec_cpu_bytes":1073741824}}`,
		},
		{
			name:  "a directly paired prefiller keeps its transfer rendering",
			in:    Input{Engine: EngineVLLM, Role: RolePrefill, KVTransfer: true},
			ec:    sharedLeg(),
			flags: []string{vllmTransferConfigArg, vllmECTransferConfigArg},
			kv: `{"kv_connector":"MooncakeConnector","kv_role":"kv_producer",
				"kv_connector_extra_config":{"mooncake_protocol":"tcp"}}`,
			document: `{"ec_connector":"ECExampleConnector","ec_role":"ec_consumer",
				"ec_connector_extra_config":{"shared_storage_path":"/mnt/ec"}}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := renderVLLMWithEncoderCache(tc.in, tc.ec)
			require.NoError(t, err)

			assert.Equal(t, tc.flags, renderedFlags(t, result.Args),
				"the two transfer documents sit together, with the store's own keys around them")
			kvAt := slices.Index(result.Args, vllmTransferConfigArg)
			require.Less(t, kvAt+1, len(result.Args))
			assert.JSONEq(t, tc.kv, result.Args[kvAt+1], "the KV rendering is unchanged")
			assert.JSONEq(t, tc.document, ecDocument(t, result))
		})
	}
}

// TestRenderVLLM_DecodeAndUnsplitRolesAreUnchanged freezes what every deployment without an encoder
// stage already renders. The literals are the whole contract: this renderer gained an arm, and any
// change to a role that never asks for it is a regression whether or not a reader can tell why.
func TestRenderVLLM_DecodeAndUnsplitRolesAreUnchanged(t *testing.T) {
	tcpEnv := mooncakeTCPDefaultedEnv()
	forceTCPEnv := append([]core.EnvVar{{Name: "MC_FORCE_TCP", Value: "1"}}, tcpEnv...)
	testCases := []struct {
		name         string
		in           Input
		args         []string
		env          []core.EnvVar
		defaultedEnv []core.EnvVar
		ports        []core.ContainerPort
	}{
		{
			name:         "an undivided server reads and writes",
			in:           Input{Engine: EngineVLLM, Connection: testConnection()},
			args:         []string{vllmTransferConfigArg, `{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_both"}`},
			env:          []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"}},
			defaultedEnv: tcpEnv,
		},
		{
			name:         "a decode half consumes",
			in:           Input{Engine: EngineVLLM, Role: RoleDecode, Connection: testConnection()},
			args:         []string{vllmTransferConfigArg, `{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_consumer"}`},
			env:          []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"}},
			defaultedEnv: tcpEnv,
		},
		{
			name: "a prefill half produces and publishes its handshake",
			in:   Input{Engine: EngineVLLM, Role: RolePrefill, KVTransfer: true},
			args: []string{
				vllmTransferConfigArg,
				`{"kv_connector":"MooncakeConnector","kv_role":"kv_producer","kv_connector_extra_config":{"mooncake_protocol":"tcp"}}`,
			},
			env:          []core.EnvVar{{Name: "VLLM_MOONCAKE_BOOTSTRAP_PORT", Value: "8998"}},
			defaultedEnv: forceTCPEnv,
			ports: []core.ContainerPort{{
				Name: "mc-bootstrap", Protocol: core.ProtocolTCP, ContainerPort: 8998,
			}},
		},
		{
			name: "a directly paired decode half consumes what the prefiller sent",
			in:   Input{Engine: EngineVLLM, Role: RoleDecode, KVTransfer: true},
			args: []string{
				vllmTransferConfigArg,
				`{"kv_connector":"MooncakeConnector","kv_role":"kv_consumer","kv_connector_extra_config":{"mooncake_protocol":"tcp"}}`,
			},
			// No store means no config path variable and no bootstrap port: the decode half learns
			// the prefiller's address per request and never listens itself.
			defaultedEnv: forceTCPEnv,
		},
		{
			name: "a store-backed decode half keeps its dtype",
			in:   Input{Engine: EngineVLLM, Role: RoleDecode, Connection: testConnection(), Dtype: "fp8"},
			args: []string{
				vllmTransferConfigArg, `{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_consumer"}`,
				KVCacheDtypeArg, "fp8",
			},
			env:          []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"}},
			defaultedEnv: tcpEnv,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := renderVLLM(tc.in)
			require.NoError(t, err)

			assert.Equal(t, tc.args, result.Args)
			assert.Equal(t, tc.env, result.Env)
			assert.Equal(t, tc.defaultedEnv, result.DefaultedEnv)
			assert.Equal(t, tc.ports, result.Ports)

			// The same input through the composing entry point, because a caller that never asks
			// for the leg must reach it by whichever route it uses.
			composed, err := renderVLLMWithEncoderCache(tc.in, EncoderCache{})
			require.NoError(t, err)
			assert.Equal(t, result, composed, "an empty leg renders the same Result")
		})
	}
}

// TestEncoderCacheOwnedKeys_CoverEverythingTheLegWrites is the owned-key proof, and it is exact
// rather than a search.
//
// The comparison runs both ways on the leg's own Result, so adding a key the renderer writes is a
// failure and dropping one from the list is a failure. A workload declaring either would leave the
// engine reading whichever it parsed last, and no engine log distinguishes that from the leg simply
// not working - which is the only kind of failure this proof is here to prevent.
func TestEncoderCacheOwnedKeys_CoverEverythingTheLegWrites(t *testing.T) {
	testCases := []struct {
		name string
		in   Input
		ec   EncoderCache
		// owned counts what the row must own, so a leg that starts writing a new key fails here
		// rather than passing because both sides grew together.
		owned bool
	}{
		{name: "the NIXL producer owns the model runner and the side channel", in: Input{Role: RoleEncode}, ec: nixlLeg(), owned: true},
		{name: "the NIXL consumer owns the model runner and the document", in: Input{Role: RolePrefill}, ec: nixlLeg(), owned: true},
		{name: "the shared producer owns the transfer document alone", in: Input{Role: RoleEncode}, ec: sharedLeg(), owned: true},
		{name: "the shared consumer owns the transfer document alone", in: Input{Role: RolePrefill}, ec: sharedLeg(), owned: true},
		// The Ascend legs carry the engine explicitly because EncoderCacheOwnedKeys takes no engine:
		// these two rows are the only thing tying the vendor that has its own refusal to the same
		// list the NVIDIA rows above pin. The NIXL connector has no Ascend row because it is refused
		// there rather than rendered, and a refusal renders nothing to own.
		{name: "the Ascend shared producer owns the transfer document alone", in: Input{Engine: EngineVLLMAscend, Role: RoleEncode}, ec: sharedLeg(), owned: true},
		{name: "the Ascend shared consumer owns the transfer document alone", in: Input{Engine: EngineVLLMAscend, Role: RolePrefill}, ec: sharedLeg(), owned: true},
		// The zero request renders nothing, so it owns nothing. A caller that asked for these keys
		// on every deployment would otherwise refuse a user's --ec-transfer-config on a deployment
		// that never had an encoder stage at all.
		{name: "a request naming no connector owns nothing", in: Input{Role: RolePrefill}, ec: EncoderCache{}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			leg, err := renderVLLMEncoderCache(tc.in, tc.ec)
			require.NoError(t, err)
			ownedArgs, ownedEnv := EncoderCacheOwnedKeys(tc.in.Role, tc.ec)

			assert.Equal(t, ownedArgs, renderedFlags(t, leg.Args),
				"the flags this package claims to own are exactly the flags the leg writes")
			assert.Equal(t, ownedEnv, envNames(leg.Env),
				"the variables this package claims to own are exactly the variables the leg writes")
			if tc.owned {
				assert.NotEmpty(t, ownedArgs, "a named leg always writes the document that names it")
			} else {
				assert.Empty(t, ownedArgs, "a request that renders nothing owns nothing")
				assert.Empty(t, ownedEnv)
			}
		})
	}
}

// TestRenderVLLM_RefusesAnEncoderCacheLegItCannotRender covers the shapes that would otherwise
// render a container that looks configured and is not. Each row is refused at a different point,
// which is why the table asserts a reason and not just an error: a caller branches on the reason to
// tell a missing value from a role the leg does not reach.
func TestRenderVLLM_RefusesAnEncoderCacheLegItCannotRender(t *testing.T) {
	testCases := []struct {
		name       string
		in         Input
		ec         EncoderCache
		wantReason Reason
		wantErr    string
	}{
		{
			name:       "the NIXL connector with no CPU region",
			in:         Input{Engine: EngineVLLM, Role: RoleEncode},
			ec:         EncoderCache{Connector: vllmECCPUConnector},
			wantReason: ReasonConnectionIncomplete,
			wantErr:    "needs the size of the CPU region",
		},
		{
			name:       "the NIXL connector with a negative CPU region",
			in:         Input{Engine: EngineVLLM, Role: RoleEncode},
			ec:         EncoderCache{Connector: vllmECCPUConnector, CPUBytes: -1},
			wantReason: ReasonConnectionIncomplete,
			wantErr:    "needs the size of the CPU region",
		},
		{
			name:       "the shared connector with no directory",
			in:         Input{Engine: EngineVLLM, Role: RolePrefill},
			ec:         EncoderCache{Connector: vllmECExampleConnector},
			wantReason: ReasonConnectionIncomplete,
			wantErr:    "needs the mount path both ends share",
		},
		{
			name:       "a connector the operator's sidecar does not speak",
			in:         Input{Engine: EngineVLLM, Role: RoleEncode},
			ec:         EncoderCache{Connector: "ECMooncakeConnector", CPUBytes: 1},
			wantReason: ReasonEngineUnknown,
			// The registry is not the reason: v0.30.0 registers ECMooncakeConnector
			// (`ec_connector/factory.py:93-97`). The sidecar is, because it carries only the
			// shared-directory and NIXL encodings.
			wantErr: "is not one the operator's decode proxy speaks",
		},
		{
			name: "the NIXL connector on the engine with no NPU copy path",
			in:   Input{Engine: EngineVLLMAscend, Role: RoleEncode},
			ec:   EncoderCache{Connector: vllmECCPUConnector, CPUBytes: 1_073_741_824},
			// Every value is present and legal, so this is the pairing rather than the input that
			// is incomplete.
			wantReason: ReasonTransportUnsupported,
			wantErr:    "no NPU copy path",
		},
		{
			name:       "a decode half asked to consume encodings",
			in:         Input{Engine: EngineVLLM, Role: RoleDecode, Connection: testConnection()},
			ec:         nixlLeg(),
			wantReason: ReasonRoleUnsupported,
			wantErr:    "consumes attention blocks, not encodings",
		},
		{
			name:       "an undivided server asked to produce encodings",
			in:         Input{Engine: EngineVLLM, Connection: testConnection()},
			ec:         nixlLeg(),
			wantReason: ReasonRoleUnsupported,
			wantErr:    "has no end for role",
		},
		{
			name:       "an encoder stage asked to transfer attention blocks",
			in:         Input{Engine: EngineVLLM, Role: RoleEncode, KVTransfer: true},
			ec:         nixlLeg(),
			wantReason: ReasonRoleUnsupported,
			wantErr:    "no KV transfer partner",
		},
		{
			name:       "an encoder stage asked to publish KV events",
			in:         Input{Engine: EngineVLLM, Role: RoleEncode, PublishKVEvents: true, KVEventsHost: "h.svc"},
			ec:         nixlLeg(),
			wantReason: ReasonRoleUnsupported,
			wantErr:    "not among the KV event publishers",
		},
		{
			// The refusal the SGLang renderer already gives the same shape: a store connection
			// handed to an encoder is a caller error, not something to drop silently.
			name:       "an encoder stage handed a store connection beside its leg",
			in:         Input{Engine: EngineVLLM, Role: RoleEncode, Connection: testConnection()},
			ec:         nixlLeg(),
			wantReason: ReasonRoleUnsupported,
			wantErr:    "holds no KV cache",
		},
		{
			// The one row that arrives with no leg at all. Before the encoder-cache arm existed the
			// role mapping had no encoder case and this refused; a renderer that answered it with an
			// empty Result instead would hand back a vLLM container with no transfer configuration
			// of any kind and start it to serve embeddings nobody can read. The store connection
			// makes the row sharper: it is exactly the input that used to draw the refusal.
			name:       "an encoder stage declaring no leg",
			in:         Input{Engine: EngineVLLM, Role: RoleEncode, Connection: testConnection()},
			ec:         EncoderCache{},
			wantReason: ReasonRoleUnknown,
			wantErr:    "has no vllm encoder-cache rendering",
		},
		{
			name:       "an encoder stage declaring no leg and nothing else either",
			in:         Input{Engine: EngineVLLM, Role: RoleEncode},
			ec:         EncoderCache{},
			wantReason: ReasonRoleUnknown,
			wantErr:    "has no vllm encoder-cache rendering",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := renderVLLMWithEncoderCache(tc.in, tc.ec)
			require.Error(t, err, "a leg that cannot be rendered must be refused, not ignored")
			assert.Equal(t, tc.wantReason, reasonOf(t, err))
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Nil(t, result, "a refusal returns nothing a caller could apply anyway")
		})
	}
}

// TestRenderVLLM_AnEmptyLegRendersNothing covers the shape a caller reaches before any deployment
// declares an encoder stage. It holds for every role EXCEPT the encoder stage itself, which has
// nothing to fall back on: without this, a caller passing a zero EncoderCache to a role the leg does
// not reach would be refused, which is the opposite of what an absent request means.
//
// The comparison is against the plain render, so it asserts the property where it actually lives -
// on the composed result a caller applies - rather than on the leg in isolation.
func TestRenderVLLM_AnEmptyLegRendersNothing(t *testing.T) {
	for _, role := range []Role{RoleNone, RolePrefill, RoleDecode} {
		t.Run(string(role), func(t *testing.T) {
			in := Input{Engine: EngineVLLM, Role: role, Connection: testConnection()}
			plain, err := renderVLLM(in)
			require.NoError(t, err)
			composed, err := renderVLLMWithEncoderCache(in, EncoderCache{})
			require.NoError(t, err, "an absent leg is not a request the renderer can decline")

			assert.Equal(t, plain, composed, "the same Result, so nothing downstream can tell")
		})
	}
}

// TestRenderVLLM_AnEncoderStageAlwaysDeclaresItsLeg pins the other half of the rule above from the
// side that changed: before the encoder-cache arm, an Encode role had no rendering and the role
// mapping refused it. It must refuse again rather than render an empty result, or the renderer's
// answer would silently disagree with SupportsRole, which still excludes this role.
func TestRenderVLLM_AnEncoderStageAlwaysDeclaresItsLeg(t *testing.T) {
	for _, leg := range []EncoderCache{nixlLeg(), sharedLeg()} {
		t.Run(leg.Connector, func(t *testing.T) {
			result, err := renderVLLMWithEncoderCache(
				Input{Engine: EngineVLLM, Role: RoleEncode}, leg)
			require.NoError(t, err, "a declared leg is what makes the role renderable")
			assert.NotEmpty(t, result.Args)
		})
	}
}
