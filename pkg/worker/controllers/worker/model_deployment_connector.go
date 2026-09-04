package worker

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// The mount path, the file name and their join are `inject`'s, not redeclared here. That package
// renders the file and this one only reports where it landed, so a second pair of constants with the
// same values would be two definitions of one fact -- and the one that drifted would be this one,
// since the renderer is what a reader checks.

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

	// Domain is the reuse identity the Binding declares. It is passed through to the shared renderer,
	// which decides per engine whether to emit it -- `inject.SupportsTenant` reads a table carrying
	// the version and source line each answer was measured at.
	//
	// NOTHING HERE RESTATES THAT ANSWER, and the reason is the shape of how the previous comment went
	// wrong rather than a preference for brevity. It said the field was DELIBERATELY NOT RENDERED
	// because no supported engine could receive a tenant: tenant_id is the 11th parameter of the
	// client's setup() and every engine calls setup() positionally with seven or eight arguments.
	// That is a counterfactual now. It measured the C++ client and the positional overload, while
	// SGLang reaches the same parameter from another direction -- its Python layer reads
	// MOONCAKE_TENANT_ID and forwards the value as a keyword argument. "The client reads no
	// environment variable" stayed true while "no tenant reaches the client" went false, because the
	// measurement point sat downstream of the path that carries it.
	//
	// A copy of that answer is a second implementation of it, agreeing with the table today and
	// diverging on whichever engine release lands next, with nothing failing in between. The test
	// for this field asserts what the renderer was HANDED, not what any engine does with it.
	Domain string

	// MasterServerAddress is the address of the store master, observed from the pool.
	MasterServerAddress string

	// Protocol is the transport in the artifact's own spelling, ALREADY MAPPED from the backend's
	// enum by `mooncake.MemberProtocol`. `inject.Connection.Protocol` documents itself as arriving
	// mapped, and that function is what maps it: it belongs to the package that owns the backend,
	// it resolves Auto, and it falls back to Auto for an empty value.
	//
	// A mapping used to live in this file and was deleted rather than kept. It agreed with
	// mooncake's table on all five enum values -- so feeding one into the other was harmless and
	// also pointless, and a third implementation of one table is a place for the next transport to
	// be added in two of three.
	Protocol string
}

// TWO FIELDS THIS STRUCT USED TO CARRY ARE GONE, and neither is a capability that was lost.
//
// `MetadataServer` was an input whose only value was ever the literal P2PHANDSHAKE, which the
// metadata plane in this scope takes unconditionally. It is `inject.MetadataServer` now, defined
// once.
//
// `DeviceName` was the RDMA device filter, and REMOVING IT IS A FIX. It accepted a specific device
// -- a test passed `mlx5_0` -- and a specific device is wrong for a pool by construction: devices
// are named per host, `mlx5_0` on one and `erdma_0` on the next, so no single name is right for
// every host one pool spans. `inject.DeviceName` is empty on every path including RDMA, meaning
// "use every device found", which is the only value correct everywhere. The field did not offer
// tuning; it offered a way to configure a filter that matches nothing on some hosts.

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

	// Volumes and VolumeMounts carry the client configuration into the container. Both are empty for
	// an engine whose vehicle is the environment, because mounting a file that engine never reads
	// would claim a wiring that is not happening.
	//
	// THEY ARE HALF OF A PAIR WITH PodAnnotations BELOW. The volume is a downwardAPI projection of
	// that annotation, so applying the volume without the annotation mounts an EMPTY FILE -- a
	// container that starts, looks configured and uses no cache. The renderer returns the three
	// together and this struct keeps them together for that reason; nothing may apply a subset.
	Volumes      []core.Volume
	VolumeMounts []core.VolumeMount

	// PodAnnotations must land on the same Pod whose spec receives the fields above.
	//
	// It holds the client configuration ITSELF rather than a digest of it, and that is what makes a
	// content change move the Pod spec hash without any extra field: the hash's subject is
	// {Labels, Annotations, PodSpec}. A ConfigMap would have reached the Pod as a NAME, leaving
	// PodSpec byte-identical while the contents changed -- the replicas would keep a stale
	// configuration and a check on the hash would go green over it.
	PodAnnotations map[string]string
}

// THE SIZE AND TOPOLOGY CONSTANTS ARE `inject`'S, not redeclared here: `GlobalSegmentSize`,
// `LocalBufferSize` and `ModeStandaloneStore`. Their values were compared one by one against the
// ones this file used to declare and all three match, so the removal changes no rendered document.
//
// Each carries the reasoning that made it that value, on the side that renders it: the segment size
// declares a ROLE rather than a size (an absent key makes every replica an in-process store member
// on a 4 GiB default), the buffer is the store's own documented staging size and must be positive,
// and the topology is half of a cross-field rule vLLM validates in both directions -- so it and the
// segment size are always written as a pair.

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
	//
	// MOONCAKE_TENANT_ID IS OWNED AND MUST NEVER BE DEFAULTED, and this is the one entry here that
	// is a security property rather than a correctness one. The tenant IS the reuse domain, and
	// every distinct domain is a tenant with its own quota ledger -- so a workload that could set
	// this variable could mint tenants in its namespace and escape the namespace ceiling. The API
	// already refuses a self-declared domain, which is the durable half of that guarantee; this is
	// the other half, because the variable is a second path to the same value and an unowned key
	// would leave it open. Defaulted is exactly the wrong class: that class lets a user's value
	// win.
	//
	// It appears in this table because the renderer emits it for THIS engine, at the version this
	// project ships. Nothing here decides that -- the shared renderer reads a measured table -- so
	// if an engine starts or stops forwarding a tenant, the invariant test that pairs this table
	// with the renderer is what says so.
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
			"MOONCAKE_TENANT_ID",
		},
	},
}

// modelDeploymentDefaultedEnvNames are the environment names the operator supplies but does not own.
//
// MC_TE_METRIC turns on the transfer engine's own metrics, without which the hit rate this whole
// design rests on cannot be measured at all. It is read by the transfer engine rather than by any
// engine's config class, so it does not depend on which keys that class accepts. A user may turn
// it off.
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
	engine, err := modelDeploymentInjectEngine(in.Engine, in.Manufacturer)
	if err != nil {
		return ModelDeploymentConnectorRender{}, err
	}

	// Role is always RoleNone, because this version admits exactly one role and so has no
	// prefill/decode split to describe. That is what makes the rendered kv_role `kv_both` -- the
	// value this function used to hardcode. Same output, now read from the table the disaggregated
	// case will read too, rather than from a constant that would have to be found and changed.
	res, err := inject.Render(inject.Input{
		Engine: engine,
		Role:   inject.RoleNone,
		Domain: in.Domain,
		Connection: inject.Connection{
			MasterAddress: in.MasterServerAddress,
			Protocol:      in.Protocol,
		},
	})
	if err != nil {
		return ModelDeploymentConnectorRender{}, err
	}

	return ModelDeploymentConnectorRender{
		Args: res.Args,
		Env:  res.Env,
		// MC_TE_METRIC has no counterpart in the shared renderer and should not have one: that
		// package renders what an engine needs to REACH the pool, while this variable is what this
		// design needs to MEASURE it. It stays defaulted rather than owned, so a user's own value
		// wins with no refusal.
		DefaultedEnv:   []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
		Volumes:        res.Volumes,
		VolumeMounts:   res.VolumeMounts,
		PodAnnotations: res.PodAnnotations,
	}, nil
}

// modelDeploymentInjectEngine maps this API's engine value onto the shared renderer's.
//
// THE TWO ENUMS ARE DELIBERATELY DIFFERENT SHAPES. This API has a single `vllm` value because on
// CANN the runner installs the vllm_ascend package for that same declared engine, so the
// accelerator decides which package runs and the user does not name it. The renderer has two,
// because the two packages register different connector names.
//
// A WRONG MAPPING HERE IS INVISIBLE IN THE TENANT OUTPUT: both vLLM entries in the renderer's facts
// table forward no tenant, so swapping them changes nothing a tenant assertion could observe. What
// it does change is the connector name, which is why that is what the test for this pins.
func modelDeploymentInjectEngine(engine, manufacturer string) (inject.Engine, error) {
	switch engine {
	case workercore.ModelDeploymentEngineVLLM:
		if manufacturer == nodefeature.ManufacturerAscend {
			return inject.EngineVLLMAscend, nil
		}

		return inject.EngineVLLM, nil
	case workercore.ModelDeploymentEngineSGLang:
		return inject.EngineSGLang, nil
	default:
		return "", fmt.Errorf("unsupported engine %q", engine)
	}
}

// THE PER-ENGINE RENDERERS THAT USED TO LIVE HERE ARE GONE, one for the vLLM-family file and one
// for SGLang's environment, and `pkg/worker/kvcache/inject` renders both now. The reasoning they
// carried was not dropped with them -- it is in that package, on the renderer it belongs to,
// including why SGLang's vehicle is the environment (its config path is fixed at admission, before
// a Pod has an IP, so only a fieldRef evaluated by kubelet can carry local_hostname) and why
// leaving SGLANG_HICACHE_MOONCAKE_CONFIG_PATH unset is what SELECTS that path.
//
// What stays here is the half that is this API's rather than the renderer's: the owned-key table
// below still refuses a user's attempt to set that config path or the extra-config argument, since
// either one would divert the engine to a loader that resolves local_hostname to a literal.

// ModelDeploymentEngineCommand renders the argv that starts one engine on one model.
//
// The OPERATOR OWNS THE WHOLE ARGV, and that follows from the template type rather than from
// preference: the template carries Command and deliberately no Args, so there is nowhere to put
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

// The --kv-transfer-config document is `inject`'s too. It renders the same two keys from a struct
// rather than a map, which turns an unreadable key into a compile error, and it derives kv_role from
// the role instead of hardcoding kv_both -- RoleNone, which is all this version admits, yields
// exactly kv_both. vLLM refuses a kv_connector with no kv_role, and kv_both is the one value that is
// both a valid producer and a valid consumer, which is what replicas sharing one store need.

// The protocol mapping this file used to hold is `mooncake.MemberProtocol`'s, which belongs to the
// package that owns the backend. The two agreed on all five enum values, so the deletion changes no
// rendered document -- with one difference worth stating: this one lowercased an UNRECOGNIZED value
// and passed it through, reasoning that the client warns and carries on. The surviving one returns
// empty for a value outside the enum, and `inject.Render` refuses an empty protocol. An object that
// reached this code with a protocol the schema does not allow never went through admission, and a
// refusal naming that is better than a lowercased guess.
