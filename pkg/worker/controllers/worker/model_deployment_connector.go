package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const (
	// ModelDeploymentClientConfigMountPath is where the rendered client JSON is mounted. It sits
	// under /etc rather than under the template's workspace, which a user's own volume may occupy.
	ModelDeploymentClientConfigMountPath = "/etc/gpustack/kvcache"

	// ModelDeploymentClientConfigFileName is the file every engine's config path points at.
	ModelDeploymentClientConfigFileName = "mooncake.json"

	// ModelDeploymentClientConfigPath is the full in-container path of the rendered client JSON.
	ModelDeploymentClientConfigPath = ModelDeploymentClientConfigMountPath + "/" + ModelDeploymentClientConfigFileName
)

// ModelDeploymentConnectorInput is everything connector synthesis needs, as plain values.
//
// It takes VALUES RATHER THAN OBJECTS so that synthesis is a pure function testable without a
// KVCachePoolBinding, a KVCachePool or a live master. Resolving the Binding into this shape is a
// separate responsibility.
type ModelDeploymentConnectorInput struct {
	// Engine selects which engine's argument keys and carrier to render, and it is the left half of
	// every owned-key decision.
	Engine string

	// Manufacturer is the accelerator vendor of the role's pool, as this project spells it, e.g.
	// "nvidia" or "ascend". It selects the CONNECTOR, which the engine does not.
	//
	// Empty means the pool's hardware is not yet observed, which renders the vendor-neutral
	// connector rather than failing: the alternative is refusing to attach a cache because a status
	// has not converged.
	Manufacturer string

	// Domain is the reuse identity the Binding declares.
	//
	// IT IS DELIBERATELY NOT RENDERED, and it is carried here anyway so that the test proving it is
	// not rendered has a domain to not render. No supported engine passes a tenant to the cache
	// client: tenant_id is the 11th parameter of the client's setup(), every engine calls setup()
	// positionally with seven or eight arguments, the client reads no environment variable for it,
	// and no engine's own config class carries the key. Emitting it would document a wiring that is
	// not happening. When an engine starts passing it, this is the field to start rendering.
	Domain string

	// MasterServerAddress is the address of the store master, observed from the pool.
	MasterServerAddress string

	// MetadataServer is the transfer engine's metadata source, observed from the pool.
	MetadataServer string

	// Protocol is the backend's transport as the backend spells it — Auto, TCP, RDMA, HIP or
	// Ascend. Synthesis lowercases it and resolves Auto, because the client matches protocol names
	// case-sensitively in lowercase while this project's enum is capitalized.
	Protocol string

	// DeviceName is the RDMA device list. Empty means the client auto-discovers, which is the right
	// default on every transport that does not use one.
	//
	// The same fact has three spellings across three surfaces: setup()'s positional parameter is
	// rdma_devices, the JSON key every engine reads is device_name, and Mooncake's own environment
	// variable is MOONCAKE_DEVICE. This renders the engines' JSON, so device_name is the spelling
	// that applies.
	DeviceName string
}

// ModelDeploymentConnectorRender is what synthesis produces for one role.
type ModelDeploymentConnectorRender struct {
	// Args are appended to the engine's command line before the role's own ExtraArgs.
	Args []string

	// Env is the environment the operator OWNS: a user entry naming one of these names is refused
	// at admission rather than merged.
	Env []core.EnvVar

	// DefaultedEnv is the environment the operator supplies only where the user supplied none.
	// Duplication here is harmless because last-wins is well defined, so a user's value stands and
	// no rejection follows.
	DefaultedEnv []core.EnvVar

	// ClientConfig is the client JSON, rendered into one ConfigMap per deployment and mounted
	// read-only into every replica of the role.
	//
	// The values are typed rather than all strings, because two of the keys are integers in every
	// engine's own config class. All three engines happen to run their size keys through a parser
	// that would accept "0", but a JSON number matches what the dataclass declares and does not
	// depend on that parser continuing to exist.
	//
	// IT IS NIL FOR AN ENGINE CONFIGURED THROUGH THE ENVIRONMENT, and nil means no ConfigMap:
	// mounting one an engine never reads would claim a wiring that is not happening.
	//
	// Where it is used, one ConfigMap serves all replicas rather than one each, because every key
	// in it is deployment-wide. That holds for vLLM and vLLM-Ascend because of what is ABSENT as
	// much as what is present: neither reads local_hostname from the file, each deriving it from
	// its own process, so the one value that would have differed per replica never enters the file.
	// It does NOT hold for SGLang, which is why SGLang does not get a file.
	ClientConfig map[string]any
}

const (
	// modelDeploymentGlobalSegmentSize is the segment this client contributes to the pool: none.
	//
	// It has to be written, and written as exactly zero. The key DECLARES A ROLE rather than sizing
	// a contribution: every engine's config class defaults it to 4 GiB, so an absent key makes each
	// replica an in-process store member. The pool's own members provide the storage. The store
	// accepts zero for exactly this purpose -- its setup_internal skips mounting a segment and its
	// validator requires the value to be zero or at least MIN_SEGMENT_SIZE, so a small non-zero
	// value is what would be rejected.
	modelDeploymentGlobalSegmentSize = 0

	// modelDeploymentLocalBufferSize is the client-side staging buffer, 128 MiB.
	//
	// It is the size the store's own setup example uses, spelled `128*1024*1024` and described as
	// short-lived client-side staging. It must be positive: vLLM's config class rejects a
	// non-positive value outright. This is a vLLM-family key -- SGLang has none, passing a
	// hardcoded 16 MiB to setup() instead.
	modelDeploymentLocalBufferSize = 128 * 1024 * 1024

	// modelDeploymentModeStandaloneStore is the topology a pure client declares, on vLLM only.
	//
	// It is half of a cross-field rule and cannot be split from the other half: vLLM's
	// __post_init__ rejects embedded mode with a zero segment AND standalone-store with a non-zero
	// one. vLLM-Ascend and SGLang have no such field, so for them the segment size stands alone.
	modelDeploymentModeStandaloneStore = "standalone-store"
)

// modelDeploymentOwnedKeys is the (engine, key) catalog of what the operator owns.
//
// It is DATA READ BY BOTH the renderer and the validating webhook, so the refusal and the render can
// never disagree about what is owned. Adding an engine adds an entry; nothing else has to change.
var modelDeploymentOwnedKeys = map[string]struct {
	Args []string
	Env  []string
}{
	// One entry covers the whole vLLM family, on every backend. That is the evidence the
	// connector was hung on the wrong dimension: when this map had a separate vllm-ascend key,
	// its value was IDENTICAL to this one, because the owned keys follow the engine while only the
	// connector name follows the backend.
	workercore.ModelDeploymentEngineVLLM: {
		Args: []string{"--kv-transfer-config"},
		Env:  []string{"MOONCAKE_CONFIG_PATH"},
	},
	// SGLang is configured through the environment, so its owned set covers both the variables it
	// actually reads AND the two keys that would divert it onto a different loader. Ownership here
	// is for what a user entry would DESTROY, not for what it would duplicate: this engine picks
	// its config source in the order extra-config, then file, then environment, and each of the
	// first two is loaded by a function whose per-key fallbacks are compile-time literals. So a
	// user setting either one does not merely override a value, it silently replaces the whole
	// configuration with defaults — a 4 GiB segment and a "localhost" identity.
	workercore.ModelDeploymentEngineSGLang: {
		Args: []string{"--hicache-storage-backend", "--hicache-storage-backend-extra-config"},
		Env: []string{
			"SGLANG_HICACHE_MOONCAKE_CONFIG_PATH",
			"MOONCAKE_MASTER",
			"MOONCAKE_TE_META_DATA_SERVER",
			"MOONCAKE_PROTOCOL",
			"MOONCAKE_DEVICE",
			"MOONCAKE_GLOBAL_SEGMENT_SIZE",
			"MOONCAKE_LOCAL_HOSTNAME",
		},
	},
}

// modelDeploymentDefaultedEnvNames are the environment names the operator supplies but does not own.
//
// MC_TE_METRIC turns on the transfer engine's own metrics, without which the hit rate this whole
// design rests on cannot be measured at all. It is read by the transfer engine rather than by any
// engine's config class, which is why it is reachable where a tenant is not. A user may turn it off.
var modelDeploymentDefaultedEnvNames = []string{"MC_TE_METRIC"}

// ModelDeploymentOwnsArg reports whether the named argument belongs to the operator on this engine.
//
// Ownership is PER (ENGINE, KEY): a key one engine owns is an ordinary user argument on another, so
// the engine is not optional and a caller that does not have one has no question to ask.
func ModelDeploymentOwnsArg(engine, arg string) bool {
	return slices.Contains(modelDeploymentOwnedKeys[engine].Args, ModelDeploymentArgName(arg))
}

// ModelDeploymentOwnsEnv reports whether the named environment variable belongs to the operator on
// this engine.
//
// The config-path variable is the load-bearing entry, and it is owned for what it destroys rather
// than for what it duplicates: re-pointing it silently swaps the whole client configuration — pool
// address, transport, metadata source — for whatever the other file says, and every symptom then
// appears one layer away from its cause.
func ModelDeploymentOwnsEnv(engine, name string) bool {
	return slices.Contains(modelDeploymentOwnedKeys[engine].Env, name)
}

// ModelDeploymentDefaultsEnv reports whether the operator merely defaults this environment variable,
// in which case a user's own value wins and no refusal follows.
func ModelDeploymentDefaultsEnv(name string) bool {
	return slices.Contains(modelDeploymentDefaultedEnvNames, name)
}

// ModelDeploymentArgName reduces a command-line entry to the flag name ownership is decided on, so
// that "--kv-transfer-config=x" and "--kv-transfer-config x" answer alike. An entry that is not a
// flag reduces to itself and matches nothing owned.
func ModelDeploymentArgName(arg string) string {
	name, _, _ := strings.Cut(arg, "=")

	return name
}

// SynthesizeModelDeploymentConnector renders the engine argument, the environment and the client
// JSON that attach one role's replicas to the cache.
//
// It is PURE: same input, same output, no client and no clock. Everything it needs about the pool
// and the domain arrives as values.
func SynthesizeModelDeploymentConnector(in ModelDeploymentConnectorInput) (ModelDeploymentConnectorRender, error) {
	protocol := modelDeploymentClientProtocol(in.Protocol)

	render := ModelDeploymentConnectorRender{
		DefaultedEnv: []core.EnvVar{
			{Name: "MC_TE_METRIC", Value: "1"},
		},
	}

	switch in.Engine {
	case workercore.ModelDeploymentEngineVLLM:
		// ONE BOOLEAN DECIDES BOTH DIFFERENCES IN THIS BRANCH, and both are properties of the
		// backend rather than of the engine. That is why "vllm-ascend" is not an engine value: on
		// CANN the runner installs the vllm_ascend package, which ships a different store connector
		// and no `mode` field, while owning exactly the same argument and environment keys.
		ascend := in.Manufacturer == nodefeature.ManufacturerAscend

		// AscendStoreConnector, also registered as MooncakeConnectorStoreV1, and NOT
		// MultiConnector: that project re-registers MultiConnector to its own composite, which
		// exists to run several connectors at once, and a single-role deployment has nothing to
		// compose. Its store backend already defaults to mooncake, so that key is not rendered.
		connector := "MooncakeStoreConnector"
		if ascend {
			connector = "AscendStoreConnector"
		}

		args, err := modelDeploymentKVTransferConfigArg(connector)
		if err != nil {
			return ModelDeploymentConnectorRender{}, err
		}
		render.Args = args
		render.Env = []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: ModelDeploymentClientConfigPath}}
		render.ClientConfig = modelDeploymentFileClientConfig(in, protocol)

		// `mode` is the partner of the zero segment size on vLLM proper, which rejects a zero
		// segment in embedded mode and a non-zero one here. The Ascend package has no such field,
		// so rendering it there would be a key its reader ignores.
		if !ascend {
			render.ClientConfig["mode"] = modelDeploymentModeStandaloneStore
		}

	case workercore.ModelDeploymentEngineSGLang:
		render.Args = []string{"--hicache-storage-backend", "mooncake"}
		render.Env = modelDeploymentSGLangClientEnv(in, protocol)

	default:
		return ModelDeploymentConnectorRender{}, fmt.Errorf("unsupported engine %q", in.Engine)
	}

	return render, nil
}

// modelDeploymentFileClientConfig renders the client JSON the vLLM-family readers load from a file.
//
// Neither of them has an environment fallback for the values themselves: their loader uses the
// environment only to locate the path, so a file is the only carrier that reaches them.
//
// It renders the size pair but NOT vLLM's mode, which only that engine has: the caller adds it.
// Keeping the cross-field partner at the one call site that needs it is what stops the pair from
// being split, and splitting it is the failure both halves guard against.
func modelDeploymentFileClientConfig(in ModelDeploymentConnectorInput, protocol string) map[string]any {
	return map[string]any{
		"master_server_address": in.MasterServerAddress,
		"metadata_server":       in.MetadataServer,
		"protocol":              protocol,
		"device_name":           in.DeviceName,
		"global_segment_size":   modelDeploymentGlobalSegmentSize,
		"local_buffer_size":     modelDeploymentLocalBufferSize,
	}
}

// modelDeploymentSGLangClientEnv renders the environment SGLang's own loader reads.
//
// SGLANG IS CONFIGURED THROUGH THE ENVIRONMENT AND NOT A FILE, and the reason is evaluation time
// rather than expressiveness. This engine needs local_hostname, which is the replica's Pod IP; a
// file and an extra-config argument are both fixed when the object is admitted, when no Pod IP
// exists yet. Only an environment variable can carry a fieldRef that kubelet evaluates as the
// container starts. Its other two loaders would each fall back to a compile-time literal for that
// key — "localhost" for every replica — and to a 4 GiB segment, which is the wrong role.
//
// Leaving SGLANG_HICACHE_MOONCAKE_CONFIG_PATH unset is what SELECTS this loader: the engine tries
// the extra-config argument, then that variable, then the environment. So the config path is unset
// deliberately and the extra-config argument is deliberately not passed, both of them owned keys so
// that a user cannot reintroduce the diversion.
//
// global_segment_size is written as an explicit zero because this client contributes no storage
// segment; the pool's own members do. The store accepts zero for exactly this purpose.
func modelDeploymentSGLangClientEnv(in ModelDeploymentConnectorInput, protocol string) []core.EnvVar {
	return []core.EnvVar{
		{Name: "MOONCAKE_MASTER", Value: in.MasterServerAddress},
		{Name: "MOONCAKE_TE_META_DATA_SERVER", Value: in.MetadataServer},
		{Name: "MOONCAKE_PROTOCOL", Value: protocol},
		{Name: "MOONCAKE_DEVICE", Value: in.DeviceName},
		{Name: "MOONCAKE_GLOBAL_SEGMENT_SIZE", Value: "0"},
		{
			Name: "MOONCAKE_LOCAL_HOSTNAME",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{FieldPath: "status.podIP"},
			},
		},
	}
}

// ModelDeploymentEngineCommand renders the argv that starts one engine on one model.
//
// The OPERATOR OWNS THE WHOLE ARGV, and that follows from the template type rather than from
// preference: InstanceTemplate carries Command and deliberately no Args, so there is nowhere to put
// arguments beside an image's own entrypoint. Either the operator builds the command line — base
// command, then the synthesized connector arguments, then the role's ExtraArgs — or the user
// replaces all of it through the take-over tier. There is no middle where the operator contributes
// arguments to a command line it did not build.
//
// The base commands are the engines' own documented entry points: vLLM installs a "vllm" console
// script whose serve subcommand takes the model as a POSITIONAL argument, and SGLang is launched as
// a module with the model named by --model-path.
//
// The command does not vary with the backend, which is why this function takes no manufacturer:
// vllm_ascend is a vLLM plugin and shares the same entry point. It is the one place where the two
// vLLM-family variants genuinely coincide, as opposed to the owned keys, where they coincide
// because the keys were never a backend property to begin with.
func ModelDeploymentEngineCommand(engine, model string) ([]string, error) {
	if model == "" {
		return nil, errors.New("model name is empty")
	}

	switch engine {
	case workercore.ModelDeploymentEngineVLLM:
		return []string{"vllm", "serve", model}, nil
	case workercore.ModelDeploymentEngineSGLang:
		return []string{"python3", "-m", "sglang.launch_server", "--model-path", model}, nil
	default:
		return nil, fmt.Errorf("unsupported engine %q", engine)
	}
}

// modelDeploymentKVTransferConfigArg renders vLLM's --kv-transfer-config for one connector.
//
// kv_role is not optional: vLLM refuses a kv_connector with no kv_role. kv_both is the one value
// that is simultaneously a valid producer and a valid consumer, which is what replicas sharing a
// store-backed cache need — each fills the cache and reads from it.
func modelDeploymentKVTransferConfigArg(connector string) ([]string, error) {
	value, err := json.Marshal(map[string]string{
		"kv_connector": connector,
		"kv_role":      "kv_both",
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling kv transfer config: %w", err)
	}

	return []string{"--kv-transfer-config", string(value)}, nil
}

// modelDeploymentClientProtocol maps the backend's transport onto the spelling the client matches.
//
// The client compares protocol names case-sensitively in lowercase while this project's enum is
// capitalized, so the mapping is not cosmetic. Auto resolves to TCP, which is what the backend's own
// contract says Auto means. An unrecognized value is lowercased and passed through: the client warns
// and carries on rather than refusing, so turning it into an error here would refuse a transport the
// client would have accepted.
func modelDeploymentClientProtocol(protocol string) string {
	if protocol == "" || strings.EqualFold(protocol, "Auto") {
		return "tcp"
	}

	return strings.ToLower(protocol)
}
