// This file renders the encoder-cache leg of an encoder/prefill/decode deployment for the vLLM
// family: the arguments, the variables and the port that move a multimodal encoding from the
// encoder stage to the prefiller.
//
// It is an arm of its own beside the KV rendering, because the two caches are unrelated. One moves
// attention blocks from a prefill half to a decode half; the other moves encodings from an encoder
// stage to the prefill half. A Pod therefore renders one, the other, or - the prefiller - both, and
// the encoder renders only this one.
package inject

import (
	"encoding/json"
	"fmt"
	"strconv"

	core "k8s.io/api/core/v1"
)

const (
	// vllmECTransferConfigArg selects the encoder-cache connector and the role. Like its KV
	// sibling it is an EngineArgs field parsed off the command line (`ec_transfer_config`, read
	// at vLLM v0.30.0 `vllm/engine/arg_utils.py:1717`) and has no environment equivalent, so
	// this leg cannot be configured without appending an argument.
	vllmECTransferConfigArg = "--ec-transfer-config"

	// vllmECCPUConnector is the name vLLM PROPER registers for the encoder-cache connector that
	// offloads encodings into a CPU mmap region and hands them to a peer with NIXL (read at v0.30.0
	// `docs/features/ec_cpu_connector.md`).
	//
	// Reach it through the caller-supplied connector name, which the selector below checks, rather
	// than by rendering it unconditionally: which spellings any one engine's registry resolves is
	// upstream state this repository neither controls nor observes.
	vllmECCPUConnector = "ECCPUConnector"

	// vllmECExampleConnector is the name vLLM PROPER registers for the encoder-cache connector
	// that writes encodings to a shared directory as safetensors (`example_connector.py:53-56`,
	// read at v0.30.0), which is what the upstream encoder example ships
	// (`examples/disaggregated/disaggregated_encoder/README.md:69-89`).
	vllmECExampleConnector = "ECExampleConnector"

	// vllmECProducerRole and vllmECConsumerRole are the connector's own spellings of the two ends
	// of the leg (`ECProducer`/`ECConsumer`, read at v0.30.0 `vllm/config/ec_transfer.py:12-14`).
	// Upstream also accepts "ec_both", which this renderer never names: one of these roles is one
	// engine each here, and a Pod that both produced and consumed its own encodings would be a
	// deployment shape nothing in this repository builds.
	vllmECProducerRole = "ec_producer"
	vllmECConsumerRole = "ec_consumer"

	// VLLMECSideChannelPort is where an NIXL producer binds the ZMQ ROUTER socket its consumers
	// pull from. It is the upstream default (v0.30.0 `vllm/envs.py:231`), which is where a
	// measured value beats one chosen here. It is separate from every other port this package
	// renders because every other one carries a different protocol: HTTP, metrics and the KV
	// event publisher each sit on their own, and the KV transfer handshake sits on a fourth.
	VLLMECSideChannelPort int32 = 5601

	// vllmECSideChannelHostEnv is the bind address of that socket. Upstream reads the pair only on
	// a producer with NIXL transfer enabled (v0.30.0 `docs/features/ec_cpu_connector.md:108`), and
	// its default of "localhost" works only when a producer and its consumer share a host
	// (`:105`) - which two Pods never do, so the renderer names the Pod's own address.
	vllmECSideChannelHostEnv = "VLLM_EC_SIDE_CHANNEL_HOST"

	// vllmECSideChannelPortEnv is the port half of the same pair.
	vllmECSideChannelPortEnv = "VLLM_EC_SIDE_CHANNEL_PORT"

	// vllmModelRunnerEnv selects the engine's model runner. ECCPUConnector requires the second one
	// and raises ValueError at construction without it (v0.30.0
	// `docs/features/ec_cpu_connector.md:10`), so on this leg the variable is the connector's
	// precondition rather than a preference, and it is owned rather than defaulted.
	vllmModelRunnerEnv = "VLLM_USE_V2_MODEL_RUNNER"

	// vllmECPodIPFieldPath is the field the kubelet resolves at container start, which is what
	// makes the side channel bind address routable without the renderer knowing the Pod's address.
	vllmECPodIPFieldPath = "status.podIP"

	// The encoder-instance flags below are the four the upstream example lists for an encoder
	// (v0.30.0 `examples/disaggregated/disaggregated_encoder/README.md:38-46`). All four are
	// DEFAULTED rather than owned: upstream marks the first two required, but a workload that
	// declares the same flag has asked for the same thing, so the group is dropped and nothing is
	// lost. They render on the producer alone, because every one of them configures an engine that
	// runs no language model - on a prefiller, --mm-encoder-only would skip the very model that
	// has to read the encodings.
	vllmEnforceEagerArg          = "--enforce-eager"
	vllmNoPrefixCachingArg       = "--no-enable-prefix-caching"
	vllmECMaxNumBatchedTokensArg = "--max-num-batched-tokens"
	vllmMMEncoderOnlyArg         = "--mm-encoder-only"

	// vllmECMaxNumBatchedTokens is the token scheduling budget the encoder example sets. Upstream
	// calls the flag irrelevant to an encoder-only instance and says to set it very high to
	// bypass scheduler limits, leaving the real budget to the encoder cache manager (README:44);
	// the example ships 114688 (README:96). It is a default rather than an answer, so a workload
	// with its own budget names the flag and this one is dropped.
	vllmECMaxNumBatchedTokens = "114688"
)

// EncoderCache is the encoder-cache leg one Pod of a deployment runs.
//
// The zero value asks for no leg, which is every input this renderer handled before encoder-cache
// transfer existed. A caller names the connector rather than receiving one because the two paths
// need different facts: the NIXL path needs a CPU region size and the shared-directory path needs a
// directory, and neither is derivable from the other or from the role.
type EncoderCache struct {
	// Connector names the connector the engine must resolve: ECCPUConnector moves encodings
	// between Pods with NIXL, ECExampleConnector through a directory both ends mount. An
	// unrecognized name is refused, because a name the engine's registry does not resolve stops
	// it at startup with no way to tell that apart from a misconfigured cache.
	Connector string

	// CPUBytes is the total size of the CPU mmap region ECCPUConnector offloads encodings into.
	// It has no default here: upstream raises ValueError when the key is absent
	// (v0.30.0 `docs/features/ec_cpu_connector.md:99`), and the region competes for host memory
	// with everything else on the node, so its size is the caller's capacity decision to make.
	CPUBytes int64

	// SharedStoragePath is the directory ECExampleConnector reads and writes the encodings in.
	// It is the mount path the caller attached, and both ends must name the same one: the
	// connector falls back to "/tmp" when the key is absent
	// (`example_connector.py:53-56`, read at v0.30.0), which in a Pod is a directory neither end
	// shares, so an unset value would look configured and move nothing.
	SharedStoragePath string
}

// vllmECTransferConfig is the --ec-transfer-config document. A struct rather than a map so an
// unreadable key is a compile error, matching the two documents already in this package. Key
// order is not part of the contract - JSON defines none - so anything comparing this must decode
// it rather than match the string.
type vllmECTransferConfig struct {
	ECConnector string `json:"ec_connector"`
	ECRole      string `json:"ec_role"`
	// EngineID names the NIXL agent. Left unset because upstream defaults it to a fresh UUID4
	// per instance (v0.30.0 `docs/features/ec_cpu_connector.md:90`), which is already unique per
	// Pod; naming one here would have to be unique per Pod too, and nothing here could check it.
	EngineID               string                      `json:"engine_id,omitempty"`
	ECConnectorExtraConfig *vllmECConnectorExtraConfig `json:"ec_connector_extra_config,omitempty"`
}

type vllmECConnectorExtraConfig struct {
	// ECEnableNIXL is a JSON boolean and not the string "true": extra config is not type coerced,
	// so a value that arrives as anything but a bool is stringified and read against "true", "1"
	// and "yes" (read at vLLM v0.30.0
	// `vllm/distributed/ec_transfer/ec_connector/cpu/scheduler/__init__.py:88-92`). The string
	// "true" would therefore work, and so would the number 1, while 0 and 2 would read as disabled.
	// That is an accident of the parse rather than a contract, and a switch is what the key is
	// documented as, so this is what it renders.
	ECEnableNIXL bool `json:"ec_enable_nixl,omitempty"`

	// ECCPUBytes sizes the CPU region. Required by ECCPUConnector and read by nothing in
	// ECExampleConnector.
	ECCPUBytes int64 `json:"ec_cpu_bytes,omitempty"`

	// SharedStoragePath is the directory ECExampleConnector works in. Required by that connector
	// and read by nothing in ECCPUConnector.
	SharedStoragePath string `json:"shared_storage_path,omitempty"`
}

// vllmECRole maps a declared role onto the encoder-cache connector's own vocabulary.
//
// Only these two roles appear, and the asymmetry is the design: the encoder produces, and only the
// prefiller consumes, because the decoder proxy sends multimodal parts to an encoder and the
// prefill half is the one that reads their embeddings back
// (v0.30.0 `examples/disaggregated/disaggregated_encoder/disagg_1e1p1d_example.sh:52-57`). A
// decode half consumes attention blocks and nothing else, so it is refused here rather than
// rendered an "ec_consumer" it would never be asked to serve.
func vllmECRole(role Role) (string, error) {
	switch role {
	case RoleEncode:
		return vllmECProducerRole, nil
	case RolePrefill:
		return vllmECConsumerRole, nil
	case RoleDecode:
		return "", newRefusal(ReasonRoleUnsupported,
			"the decode role consumes attention blocks, not encodings: only the encoder produces "+
				"them and only the prefill role reads them")
	default:
		return "", newRefusal(ReasonRoleUnsupported,
			"the encoder-cache leg has no end for role %q: a deployment runs it as a separate "+
				"encoder stage or as the prefill half that consumes it", role)
	}
}

// renderVLLMEncoderCache produces the encoder-cache leg on its own, so the caller composes it onto
// a role's ordinary rendering rather than this function deciding what surrounds it.
//
// The returned Result carries the leg and nothing else, which is what lets a caller append it
// wherever the order suits and lets a test hold the leg alone and compare it against the keys this
// package claims to own.
//
// An absent request renders nothing here on every role, encoder included. Whether an ENCODER may
// arrive without one is not this function's question: it answers what a named leg produces, and the
// encoder-stage rule belongs to the composing entry beside the other role rules.
func renderVLLMEncoderCache(in Input, ec EncoderCache) (*Result, error) {
	// An absent request renders nothing and is NOT a refusal. Every input that predates this leg
	// arrives here naming no connector, and a role the leg does not reach has no business being
	// declined for a request it never made. The emptiness is decided here rather than at the call
	// site so a second composition point cannot get it wrong.
	if ec.Connector == "" {
		return &Result{}, nil
	}

	role, err := vllmECRole(in.Role)
	if err != nil {
		return nil, err
	}

	doc := &vllmECTransferConfig{ECRole: role}
	switch ec.Connector {
	case vllmECCPUConnector:
		// Refused on the engine that has no NPU copy path rather than rendered and left to fail
		// in the container: the whole transfer is a device-to-device copy, and an Ascend build has
		// none of the NIXL backends it would need.
		if in.Engine == EngineVLLMAscend {
			return nil, newRefusal(ReasonTransportUnsupported,
				"the NIXL encoder-cache connector has no NPU copy path on engine %q, so every "+
					"value the leg declares is legal and the pair still has no container that can "+
					"run it; name %s instead", in.Engine, vllmECExampleConnector)
		}
		if ec.CPUBytes <= 0 {
			return nil, newRefusal(ReasonConnectionIncomplete,
				"the NIXL encoder-cache connector needs the size of the CPU region to offload "+
					"encodings into, and it raises ValueError without the key; declare one on the "+
					"encoder-cache leg")
		}
		doc.ECConnector = vllmECCPUConnector
		doc.ECConnectorExtraConfig = &vllmECConnectorExtraConfig{
			ECEnableNIXL: true,
			ECCPUBytes:   ec.CPUBytes,
		}
	case vllmECExampleConnector:
		if ec.SharedStoragePath == "" {
			return nil, newRefusal(ReasonConnectionIncomplete,
				"the shared-directory encoder-cache connector needs the mount path both ends "+
					"share, and falls back to /tmp without it, which in a Pod is a directory "+
					"neither end can see; declare one on the encoder-cache leg")
		}
		doc.ECConnector = vllmECExampleConnector
		doc.ECConnectorExtraConfig = &vllmECConnectorExtraConfig{SharedStoragePath: ec.SharedStoragePath}
	default:
		return nil, newRefusal(ReasonEngineUnknown,
			"encoder-cache connector %q is not one the operator's decode proxy speaks: its sidecar "+
				"carries the shared-directory and NIXL encodings, so this leg would move nothing "+
				"whatever the engine's own registry resolves", ec.Connector)
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal the vLLM encoder-cache configuration: %w", err)
	}
	result := &Result{Args: []string{vllmECTransferConfigArg, string(encoded)}}

	// The model runner is the connector's precondition on EVERY role, not the producer's.
	// ECCPUConnector.__init__ raises ValueError when use_v2_model_runner is false BEFORE it looks
	// at the role it was handed (read at vLLM v0.30.0
	// `vllm/distributed/ec_transfer/ec_connector/cpu/connector.py:40-44`), and the factory
	// constructs that class for a consumer instance exactly as it does for a producer's
	// (`ec_connector/factory.py:36-56`: the role it passes is worker-or-scheduler, not
	// produce-or-consume). So a prefiller on this leg crash-loops at construction without the
	// variable. It is left unset on the shared-directory leg because ECExampleConnector.__init__
	// reads no such flag (`example_connector.py:54-67`).
	if doc.ECConnector == vllmECCPUConnector {
		result.Env = []core.EnvVar{{Name: vllmModelRunnerEnv, Value: "1"}}
	}

	if role == vllmECConsumerRole {
		return result, nil
	}

	// Producer-only from here. The side channel is read by a producer with NIXL transfer enabled
	// and by nothing else, so a consumer that rendered the pair would advertise an address it
	// never binds.
	if doc.ECConnector == vllmECCPUConnector {
		result.Env = append(result.Env,
			core.EnvVar{
				Name: vllmECSideChannelHostEnv,
				ValueFrom: &core.EnvVarSource{
					FieldRef: &core.ObjectFieldSelector{FieldPath: vllmECPodIPFieldPath},
				},
			},
			core.EnvVar{Name: vllmECSideChannelPortEnv, Value: strconv.Itoa(int(VLLMECSideChannelPort))},
		)
		result.Ports = []core.ContainerPort{{
			Name: "ec-side-channel", Protocol: core.ProtocolTCP, ContainerPort: VLLMECSideChannelPort,
		}}
	}

	result.DefaultedArgs = [][]string{
		{vllmEnforceEagerArg},
		{vllmNoPrefixCachingArg},
		{vllmECMaxNumBatchedTokensArg, vllmECMaxNumBatchedTokens},
		{vllmMMEncoderOnlyArg},
	}
	return result, nil
}

// EncoderCacheOwnedKeys returns the flags and the variables the encoder-cache leg writes for the
// given request, and therefore resolves itself.
//
// A caller uses this to refuse a workload that also declares one of them. Both would otherwise be
// honored and the engine would read whichever it parsed last, so a workload could turn NIXL
// transfer off, point the side channel at a host no consumer dials, or swap the connector under a
// deployment that is already running - none of which any engine log distinguishes from the leg
// simply not working.
//
// The two lists are exact rather than a fixed set, because what the leg writes depends on both the
// connector and the role: the model runner belongs to the NIXL leg on either role, while the side
// channel belongs to its producer alone. They are returned separately because a flag and a variable
// are refused through different mechanisms, and each is always a list - empty where the leg owns
// nothing of that kind - so a caller can compare them against what a render produced without
// normalizing either side.
//
// A request naming no connector owns nothing because it renders nothing, and the caller in
// model_deployment_connector.go that builds the operator's owned-key set always names a connector,
// so the keys are owned on every deployment.
//
// The role is taken alongside the request because a request alone cannot answer the question: the
// same NIXL leg owns one variable as a consumer and three as a producer.
func EncoderCacheOwnedKeys(role Role, ec EncoderCache) (args, env []string) {
	if ec.Connector == "" {
		return []string{}, []string{}
	}
	args = []string{vllmECTransferConfigArg}
	if ec.Connector != vllmECCPUConnector {
		return args, []string{}
	}
	env = []string{vllmModelRunnerEnv}
	if role == RoleEncode {
		env = append(env, vllmECSideChannelHostEnv, vllmECSideChannelPortEnv)
	}
	return args, env
}
