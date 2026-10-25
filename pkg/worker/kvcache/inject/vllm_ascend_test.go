package inject

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
)

// The vLLM-Ascend encoder-cache leg. It is a separate file because the finding that shapes it is a
// negative one, and a test that reads as "Ascend renders something special" would misstate it.
//
// What upstream records: vllm-ascend's own EPD example launches the encoder with the same reference
// pathway its guide describes -- ECExampleConnector as ec_producer against a shared storage path
// (`examples/disaggregated_encoder/disagg_1e1pd_example.sh`, read at v0.23.0), and the guide
// (`docs/source/user_guide/feature_guide/epd_disaggregation.md`, read at v0.23.0) documents the same
// three keys under the same flag. So the connector name, the document keys and the encoder flags are
// the ones the shared path already renders, and the leg needs no Ascend-specific rendering.
//
// What the example sets beyond them: `ASCEND_RT_VISIBLE_DEVICES="$CARD_E"` on the encoder line. That
// is accelerator selection, the direct parallel of the `CUDA_VISIBLE_DEVICES` the NVIDIA example sets
// on the same line, and it belongs to the device manager that hands a Pod its device -- not to this
// renderer. The example also passes `--enable-request-id-headers` and `--max-num-seqs 128`; the first
// correlates responses for the example's own proxy and the second is tuning, and the NVIDIA example
// passes the same two. Neither belongs to the leg, so neither is rendered on either vendor.
//
// What makes Ascend differ is not additive: the NIXL connector is refused, because that transfer is a
// device-to-device copy with no NPU equivalent. The tests below pin both halves of that.

// TestRenderVLLMAscend_EncodeRendersTheSharedDirectoryProducerAndNoKVLeg pins the positive half of
// the Ascend encoder: the shared-directory producer, and nothing else.
//
// The store row carries the weight. A legal, complete connection with a dtype and a cache prefix
// renders KV keys on every other role, so this row fails the moment the renderer stops consulting
// the role before it renders the store.
func TestRenderVLLMAscend_EncodeRendersTheSharedDirectoryProducerAndNoKVLeg(t *testing.T) {
	result, err := renderVLLMWithEncoderCache(Input{
		Engine: EngineVLLMAscend, Role: RoleEncode,
		Dtype: "fp8", CachePrefix: "team-a",
	}, sharedLeg())
	require.NoError(t, err)

	// The shared-directory producer, carrying the directory the caller passed in. The whole
	// argument list, so a KV flag appearing beside it is named in the failure rather than
	// swallowed by an assertion about the one flag that should be there.
	assert.Equal(t, []string{vllmECTransferConfigArg}, renderedFlags(t, result.Args),
		"the encoder-cache document is the only argument an encoder stage takes")
	assert.JSONEq(t, `{"ec_connector":"ECExampleConnector","ec_role":"ec_producer",
		"ec_connector_extra_config":{"shared_storage_path":"/mnt/ec"}}`, ecDocument(t, result))

	// An encoder allocates no KV cache, so the whole transfer half is dropped rather than filled
	// with a role the engine cannot act on: the flag, the dtype the arm renders beside it, and the
	// event flag it renders last.
	for _, arg := range []string{vllmTransferConfigArg, KVCacheDtypeArg, vllmKVEventsConfigArg} {
		assert.NotContains(t, result.Args, arg, "an encoder stage runs no KV leg: %v", result.Args)
	}
	assert.False(t, result.KVTransfer, "an encoder stage transfers no attention blocks")
	assert.Nil(t, result.KVEvents)
	// A Mooncake client configuration file on a Pod with no cache to address would be a file the
	// connector never reads.
	assert.NotContains(t, envNames(result.Env), vllmConfigPathEnv,
		"an encoder stage has no Mooncake client configuration: %v", result.Env)

	// No driver-tree projection. The tree exists so the transfer leg can read each NPU's NIC
	// address through hccn_tool; an encoder never runs that leg, and the mount is otherwise a
	// host path projected into every Pod that renders one.
	assert.Empty(t, result.Volumes, "an encoder stage projects no volume: %v", result.Volumes)
	assert.Empty(t, result.VolumeMounts)
	assert.Empty(t, result.PodAnnotations)

	// The shared-directory connector reads its whole configuration from the document, so it writes
	// no variable and opens no port.
	assert.Empty(t, result.Env)
	assert.Empty(t, result.Ports)

	// The encoder-instance flags are DEFAULTED as a group, so a workload declaring any one of them
	// drops the whole set. Asserted as groups because that is how a caller experiences them.
	assert.Equal(t, [][]string{
		{vllmEnforceEagerArg},
		{vllmNoPrefixCachingArg},
		{vllmECMaxNumBatchedTokensArg, vllmECMaxNumBatchedTokens},
		{vllmMMEncoderOnlyArg},
	}, result.DefaultedArgs)
}

// TestRenderVLLMAscend_PrefillComposesTheConsumerWithItsUnchangedKVProducer pins the composition
// half: an Ascend prefill is two legs at once, and the encoder-cache one must not disturb the
// attention-blocks one it sits beside.
//
// The proof is byte-identity against the render that has no encoder stage rather than a quoted
// document, because "unchanged" is the whole claim. A pasted MooncakeConnectorV1 document would
// still pass if the renderer changed something around it.
func TestRenderVLLMAscend_PrefillComposesTheConsumerWithItsUnchangedKVProducer(t *testing.T) {
	in := Input{
		Engine: EngineVLLMAscend, Role: RolePrefill, KVTransfer: true,
		Connection: testConnection(),
		Parallelism: ParallelismPair{
			Prefill: Parallelism{TensorParallel: 2, DataParallel: 1},
			Decode:  Parallelism{TensorParallel: 2, DataParallel: 1},
		},
	}
	plain, err := renderVLLM(in)
	require.NoError(t, err)
	composed, err := renderVLLMWithEncoderCache(in, sharedLeg())
	require.NoError(t, err)

	// The EC pair is appended last, so everything before it is the KV leg verbatim.
	at := slices.Index(composed.Args, vllmECTransferConfigArg)
	require.NotEqual(t, -1, at, "the leg must render its document: %v", composed.Args)
	assert.Equal(t, plain.Args, composed.Args[:at],
		"the attention-blocks leg must come through byte-identical, driver projection included")
	assert.Equal(t, plain.Env, composed.Env)
	assert.Equal(t, plain.Volumes, composed.Volumes,
		"the driver tree stays, because the transfer leg still runs on this Pod")
	assert.Equal(t, plain.VolumeMounts, composed.VolumeMounts)
	assert.Equal(t, plain.Ports, composed.Ports,
		"the bootstrap port is declared on either vendor, so the encoder stage does not change it")

	// The leg under test: the shared-directory consumer, carrying the same directory.
	assert.JSONEq(t, `{"ec_connector":"ECExampleConnector","ec_role":"ec_consumer",
		"ec_connector_extra_config":{"shared_storage_path":"/mnt/ec"}}`, ecDocument(t, composed))

	// The connector the KV half kept is the one vLLM-Ascend registers for its point-to-point leg.
	// Named rather than compared, because a renderer that swapped in the NVIDIA store connector
	// would otherwise still satisfy the byte-identity check above.
	kvAt := slices.Index(composed.Args, vllmTransferConfigArg)
	require.NotEqual(t, -1, kvAt)
	assert.Contains(t, composed.Args[kvAt+1], vllmAscendTransferConnector)
}

// TestRenderVLLMAscend_TheNIXLPathIsRefusedOnEveryRoleThatCouldNameIt pins the negative half.
//
// The refusal is the one thing that IS Ascend-specific, so it is pinned per spelling rather than
// once: the leg answers to two roles, and a check written only against the encoder leaves the
// consumer -- the role that would actually start a Pod -- unpinned.
func TestRenderVLLMAscend_TheNIXLPathIsRefusedOnEveryRoleThatCouldNameIt(t *testing.T) {
	for _, role := range []Role{RoleEncode, RolePrefill} {
		t.Run(string(role), func(t *testing.T) {
			// Every value is present and legal, so what is refused is the pairing.
			leg := EncoderCache{Connector: vllmECCPUConnector, CPUBytes: 1_073_741_824}
			in := Input{Engine: EngineVLLMAscend, Role: role}
			if role != RoleEncode {
				// An encoder is refused a store connection before the pairing is looked at.
				in.Connection = testConnection()
			}
			result, err := renderVLLMWithEncoderCache(in, leg)
			require.Error(t, err, "a leg with no NPU copy path must be refused, not rendered")
			assert.Equal(t, ReasonTransportUnsupported, reasonOf(t, err))
			assert.Contains(t, err.Error(), "no NPU copy path")
			assert.Contains(t, err.Error(), vllmECExampleConnector, "the message must name the way out")
			assert.Nil(t, result, "a refusal returns nothing a caller could apply anyway")
		})
	}
}

// TestRenderVLLMAscend_RolesWithoutAnEncoderStageRenderUnchanged holds the line that the Ascend work
// is additive. The composition entry point must hand back the same Result for every role that never
// asks for the leg, so nothing downstream can tell which route a caller used.
//
// The KV-transfer rows matter most here: they are the ones carrying the driver projection and the
// bootstrap port, which is exactly the state an encoder-cache change could have disturbed.
func TestRenderVLLMAscend_RolesWithoutAnEncoderStageRenderUnchanged(t *testing.T) {
	testCases := []struct {
		name string
		in   Input
	}{
		{name: "an undivided server", in: Input{Role: RoleNone}},
		{name: "a prefill with no transfer leg", in: Input{Role: RolePrefill}},
		{name: "a prefill producing blocks", in: Input{Role: RolePrefill, KVTransfer: true}},
		{name: "a decode with no transfer leg", in: Input{Role: RoleDecode}},
		{name: "a decode consuming blocks", in: Input{Role: RoleDecode, KVTransfer: true}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.Engine, in.Connection = EngineVLLMAscend, testConnection()
			in.Parallelism = ParallelismPair{
				Prefill: Parallelism{TensorParallel: 2},
				Decode:  Parallelism{TensorParallel: 2},
			}

			plain, err := renderVLLM(in)
			require.NoError(t, err)
			composed, err := renderVLLMWithEncoderCache(in, EncoderCache{})
			require.NoError(t, err, "an absent leg is not a request the renderer can decline")

			assert.Equal(t, plain, composed, "the same Result, so nothing downstream can tell")
		})
	}
}

// TestEncoderCacheOwnedKeys_AscendOwnsExactlyWhatNvidiaOwns answers the owned-key question for the
// vendor that has its own refusal. The function takes no engine, so the only thing keeping the two
// vendors honest is that the Ascend legs, rendered and compared against it, produce the same list.
//
// A row whose Result is rendered rather than quoted is what makes this exact: adding a key to either
// vendor changes both sides of the comparison only if the function changes too, and dropping one
// fails the equality outright.
func TestEncoderCacheOwnedKeys_AscendOwnsExactlyWhatNvidiaOwns(t *testing.T) {
	for _, leg := range []struct {
		name   string
		engine Engine
		leg    EncoderCache
		role   Role
	}{
		{name: "the shared producer", engine: EngineVLLMAscend, leg: sharedLeg(), role: RoleEncode},
		{name: "the shared consumer", engine: EngineVLLMAscend, leg: sharedLeg(), role: RolePrefill},
	} {
		t.Run(leg.name, func(t *testing.T) {
			rendered, err := renderVLLMEncoderCache(Input{Engine: leg.engine, Role: leg.role}, leg.leg)
			require.NoError(t, err)
			native, err := renderVLLMEncoderCache(Input{Engine: EngineVLLM, Role: leg.role}, leg.leg)
			require.NoError(t, err)

			ownedArgs, ownedEnv := EncoderCacheOwnedKeys(leg.role, leg.leg)
			assert.Equal(t, ownedArgs, renderedFlags(t, rendered.Args),
				"the flags this package claims to own are exactly the flags the Ascend leg writes")
			assert.Equal(t, ownedEnv, envNames(rendered.Env))
			assert.Equal(t, rendered.Args, native.Args,
				"the shared-directory connector is vendor-blind, so the document must not differ")
			assert.Equal(t, envNames(native.Env), envNames(rendered.Env))
		})
	}
}

// TestRenderVLLMAscend_NoOwnedKeyOnTheAscendLegIsNew pins the consequence of the finding above: the
// encoder-cache arm introduced no Ascend-only owned key or variable, so a workload declaring the
// keys it DOES own is refused identically on both vendors.
//
// Every spelling matters. A workload declares an owned key in its command line or its environment,
// and a refusal that caught only one of those would leave the engine reading whichever it parsed
// last -- a failure no engine log distinguishes from the leg simply not working.
func TestRenderVLLMAscend_NoOwnedKeyOnTheAscendLegIsNew(t *testing.T) {
	ownedArgs, ownedEnv := EncoderCacheOwnedKeys(RoleEncode, sharedLeg())
	require.Equal(t, []string{vllmECTransferConfigArg}, ownedArgs,
		"the shared leg owns the document alone, so this list is the whole claim")
	require.Empty(t, ownedEnv, "the shared leg writes no variable, so there is no second spelling")

	// Rendering the Ascend leg must not have added anything to either list.
	for _, role := range []Role{RoleEncode, RolePrefill} {
		rendered, err := renderVLLMEncoderCache(Input{Engine: EngineVLLMAscend, Role: role}, sharedLeg())
		require.NoError(t, err)
		assert.Equal(t, ownedArgs, renderedFlags(t, rendered.Args))
		assert.Equal(t, ownedEnv, envNames(rendered.Env))
	}
}

// TestRenderVLLMAscend_TheEncoderStageDeclaresNoPortOrVolume asserts the observable Pod surface of
// the Ascend encoder directly, so a future leg that opens a side channel or projects a path cannot
// pass by leaving these fields empty on the composed Result only.
func TestRenderVLLMAscend_TheEncoderStageDeclaresNoPortOrVolume(t *testing.T) {
	result, err := renderVLLMWithEncoderCache(
		Input{Engine: EngineVLLMAscend, Role: RoleEncode}, sharedLeg(),
	)
	require.NoError(t, err)

	assert.Empty(t, result.Ports, "the shared-directory leg moves nothing over the network")
	assert.NotContains(t, volumeNames(result.Volumes), vllmAscendDriverVolumeName,
		"the driver tree belongs to the transfer leg, which the encoder does not run")
	assert.NotContains(t, volumeNames(result.Volumes), ConfigVolumeName,
		"an encoder stage has no Mooncake client configuration to write")
}

// volumeNames returns the names of a volume list, in order.
func volumeNames(volumes []core.Volume) []string {
	out := make([]string, 0, len(volumes))
	for i := range volumes {
		out = append(out, volumes[i].Name)
	}
	return out
}
