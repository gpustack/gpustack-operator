package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
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
	// Engine selects which engine's argument and config-path variable to render, and it is the
	// left half of every owned-key decision.
	Engine string

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
	// One ConfigMap for all replicas rather than one each, because every key here is
	// deployment-wide. That holds because of what is ABSENT as much as what is present: no engine
	// reads local_hostname from the file — each derives it from its own process — so the one value
	// that would have differed per replica never enters the file.
	ClientConfig map[string]string
}

// modelDeploymentOwnedKeys is the (engine, key) catalog of what the operator owns.
//
// It is DATA READ BY BOTH the renderer and the validating webhook, so the refusal and the render can
// never disagree about what is owned. Adding an engine adds an entry; nothing else has to change.
var modelDeploymentOwnedKeys = map[string]struct {
	Args []string
	Env  []string
}{
	workercore.ModelDeploymentEngineVLLM: {
		Args: []string{"--kv-transfer-config"},
		Env:  []string{"MOONCAKE_CONFIG_PATH"},
	},
	workercore.ModelDeploymentEngineVLLMAscend: {
		Args: []string{"--kv-transfer-config"},
		Env:  []string{"MOONCAKE_CONFIG_PATH"},
	},
	workercore.ModelDeploymentEngineSGLang: {
		Args: []string{"--hicache-storage-backend", "--hicache-storage-backend-extra-config"},
		Env:  []string{"SGLANG_HICACHE_MOONCAKE_CONFIG_PATH"},
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
	// The client JSON is the same four keys on all three engines, and that is a conclusion rather
	// than a coincidence: the three readers' key sets differ only in keys the operator deliberately
	// does not choose — the segment sizes, vLLM's mode, SGLang's master metrics port. What is left
	// is the intersection, and all three read all of it.
	//
	// The segment sizes stay absent because they size a replica's own contribution to the pool, and
	// an operator-invented value there is a silent capacity error rather than a visible refusal.
	// vLLM's mode stays absent for a reason one step further on: it carries a cross-field rule
	// against global_segment_size — embedded demands a positive size, standalone-store demands
	// exactly zero — so choosing one half of a pair whose other half is not ours to choose is how
	// an engine ends up refusing its own configuration.
	config := map[string]string{
		"master_server_address": in.MasterServerAddress,
		"metadata_server":       in.MetadataServer,
		"protocol":              modelDeploymentClientProtocol(in.Protocol),
		"device_name":           in.DeviceName,
	}

	render := ModelDeploymentConnectorRender{
		ClientConfig: config,
		DefaultedEnv: []core.EnvVar{
			{Name: "MC_TE_METRIC", Value: "1"},
		},
	}

	switch in.Engine {
	case workercore.ModelDeploymentEngineVLLM:
		args, err := modelDeploymentKVTransferConfigArg("MooncakeStoreConnector")
		if err != nil {
			return ModelDeploymentConnectorRender{}, err
		}
		render.Args = args
		render.Env = []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: ModelDeploymentClientConfigPath}}

	case workercore.ModelDeploymentEngineVLLMAscend:
		// AscendStoreConnector and not MultiConnector: vllm-ascend re-registers MultiConnector to
		// its own composite, which exists to run several connectors at once. The connector that
		// reaches the store is this one, and the store backend it resolves already defaults to
		// mooncake, so that key is not rendered either.
		args, err := modelDeploymentKVTransferConfigArg("AscendStoreConnector")
		if err != nil {
			return ModelDeploymentConnectorRender{}, err
		}
		render.Args = args
		render.Env = []core.EnvVar{{Name: "MOONCAKE_CONFIG_PATH", Value: ModelDeploymentClientConfigPath}}

	case workercore.ModelDeploymentEngineSGLang:
		extra, err := json.Marshal(map[string]string{"master_server_address": in.MasterServerAddress})
		if err != nil {
			return ModelDeploymentConnectorRender{}, fmt.Errorf("marshaling hicache extra config: %w", err)
		}
		// The extra config carries the master address as well as the mounted file, because this
		// reader takes the extra config in preference to the file: supplying only the file would
		// work, and would stop working the day anything else set an extra config.
		render.Args = []string{
			"--hicache-storage-backend", "mooncake",
			"--hicache-storage-backend-extra-config", string(extra),
		}
		render.Env = []core.EnvVar{
			{Name: "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH", Value: ModelDeploymentClientConfigPath},
		}

	default:
		return ModelDeploymentConnectorRender{}, fmt.Errorf("unsupported engine %q", in.Engine)
	}

	return render, nil
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
// a module with the model named by --model-path. vllm-ascend is a vLLM plugin and shares its
// entry point.
func ModelDeploymentEngineCommand(engine, model string) ([]string, error) {
	if model == "" {
		return nil, errors.New("model name is empty")
	}

	switch engine {
	case workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineVLLMAscend:
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
