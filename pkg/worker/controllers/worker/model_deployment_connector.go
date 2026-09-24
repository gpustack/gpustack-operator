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

	// Kind is what the engine is told this role is, and it is the ONE term that makes a prefill role
	// and a decode role different configurations rather than two copies of one.
	//
	// Its zero value is the deployment shape that predates disaggregation, which renders exactly what
	// a single-role deployment always rendered.
	Kind workercore.ModelDeploymentRoleKind

	// Disaggregated reports whether the deployment declaring this role declares BOTH halves of a
	// prefill/decode pair. It is answered from the spec alone, so admission and rendering cannot
	// disagree about it, and it is what keeps the engine's own split mode in step with the router's:
	// a lone half is routed undivided and now runs undivided. The role discriminator on the store
	// client follows Kind regardless, so a half declared to feed a shared pool keeps contributing
	// to it.
	Disaggregated bool

	// Manufacturer is the accelerator vendor of the role's pool, as this project spells it, e.g.
	// "nvidia" or "ascend". It selects the CONNECTOR, which the engine does not.
	//
	// Empty means the pool's hardware is not yet observed, which renders the vendor-neutral
	// connector rather than failing: the alternative is refusing to attach a cache because a status
	// has not converged.
	Manufacturer string

	// Domain is the reuse identity the Binding declares — or empty, when the pool's master holds no
	// tenant ledger to keep it apart in: a ledger-less master is handed no tenant at all rather than
	// one it would collapse into its default. Whether the engine build reads a rendered value is the
	// image owner's compatibility responsibility.
	Domain string

	// MasterServerAddress is the address of the store master, observed from the pool.
	MasterServerAddress string

	// Protocols is what the pool's backend offers, in the artifact's own spelling and in group
	// declaration order: each member group's effective protocol, ALREADY MAPPED by
	// `mooncake.MemberProtocols`. It is a list because groups may disagree — a VRAM group on a
	// fabric beside a DRAM group on TCP. Admission refuses a new binding of an unconstrained
	// engine to such a pool; synthesis retains the first-offer rule for an existing binding.
	//
	// It feeds the store client alone. The point-to-point leg does not read it -- the two data
	// planes declare separately, and defaultKVTransferProtocol says why. Empty is the no-store
	// shape, which the renderer refuses for its own reason.
	Protocols []string

	// PublishKVEvents is true for a routed role that produces cache blocks.
	PublishKVEvents bool

	// KVEventsHost is the role Service hostname a consumer dials instead of the publisher's wildcard
	// bind address.
	KVEventsHost string

	// KVTransfer composes point-to-point P/D transfer with the shared store. The two are
	// orthogonal inputs and both may be on at once; neither is a branch that excludes the other.
	KVTransfer bool

	// RoutingSidecar asks for the decode proxy. It travels beside KVTransfer rather than being
	// derived from it because the two now answer differently: every admitted pair carries the
	// transfer leg, and one router carries the proxy.
	RoutingSidecar bool

	// KVTransferProtocol is the transport the point-to-point leg is told to use, declared on
	// the ModelDeployment. Empty renders the renderer's default. It is passed through verbatim:
	// the accepted set belongs to the engine image's mooncake build, not to this operator.
	KVTransferProtocol string

	// Parallelism is the declared parallel shape of BOTH halves of the prefill/decode pair,
	// resolved off each role's own books by the caller. It travels as one value because the
	// transfer document asserts on the pair: two roles synthesizing from two resolutions could
	// carry different blocks, and a block disagreeing with the engine it describes answers with
	// a wrong block layout instead of a refusal. Only the document that asserts on it reads it
	// -- the Ascend transfer leg's -- and the zero value renders the engine's own default of
	// one on both sides there.
	Parallelism inject.ParallelismPair
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

	// DefaultedArgs are argument groups the operator supplies only where the role's own ExtraArgs
	// name none of the same flag. Each group starts with its flag, and a named flag drops the
	// whole group: last-wins would settle a valued flag, but not a switch that only turns
	// something on, so the group is dropped rather than overridden.
	DefaultedArgs [][]string

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

	// Ports are additional ports opened by the synthesized connector configuration.
	Ports []core.ContainerPort

	// KVEvents is the rendered, dialable event contract, absent when this role does not publish.
	KVEvents *inject.KVEvents

	// KVTransfer is true when Args include the point-to-point P/D connector.
	KVTransfer bool

	// RoutingSidecar is true when a decode replica is fronted by the proxy that performs the
	// handshake on its behalf. It is decided HERE rather than read off KVTransfer by the renderer,
	// because the two stopped agreeing the moment a second router could carry the transfer leg.
	RoutingSidecar bool
}

// modelDeploymentRoutesManaged is now the KV EVENTS gate alone: a managed router is in front of
// this deployment, off Ascend.
//
// THE THREE DECISIONS BELOW ANSWER SEPARATELY, and stating what they do not share is the point of
// keeping them apart. The KV event publisher exists to feed one router's data layer, so it follows
// that router and that engine -- and it stays off Ascend, because the vLLM-Ascend render knows no
// publisher, and a refused render is an error loop, not a deployment without events. The
// engine-side transfer leg follows every admitted pair, because a prefiller that cannot hand a
// decoder its blocks is not disaggregated under any router. The decode proxy follows one router
// alone, because it reads an endpoint out of a header only that router writes.
func modelDeploymentRoutesManaged(md *workercore.ModelDeployment, manufacturer string) bool {
	return md.Spec.Router != nil && manufacturer != nodefeature.ManufacturerAscend
}

// ModelDeploymentDeclaresBothHalves reports whether this deployment's roles contain a prefill role
// and a decode role.
//
// IT IS THE PAIR RULE THE ROUTER RENDERER ALREADY FOLLOWS, stated for the engine side: a deployment
// holding one half has no pair to hand blocks to, and every layer answering "is this a split" --
// the router's mode, the engine's transfer leg, the engine's own split arguments -- must answer
// alike or one object serves requests one way and moves blocks another. The router renderer routes
// a lone half as the undivided shape; this predicate is what lets the engine side agree with it.
// The cost is carried knowingly: a deployment deliberately declaring one half to feed a shared
// store now runs its engines undivided, and the role discriminator on the store client is what
// keeps that half's contribution to the pool.
//
// IT IS EXPORTED because the webhook's port reservation reads the same rule: the pair is knowable
// at admission -- the roles are in the spec, unlike the pool's vendor -- so the refusal and the
// render can agree on this axis exactly, refusing no port a render actually binds and releasing no
// port one does.
func ModelDeploymentDeclaresBothHalves(md *workercore.ModelDeployment) bool {
	var prefill, decode bool
	for i := range md.Spec.Roles {
		switch ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) {
		case workercore.ModelDeploymentRoleKindPrefill:
			prefill = true
		case workercore.ModelDeploymentRoleKindDecode:
			decode = true
		}
	}

	return prefill && decode
}

// modelDeploymentUsesKVTransfer reports whether this role runs the engine side of a routed
// prefill/decode pair.
//
// ON ASCEND IT NAMES A ROUTER, and the exception is the handshake rather than the leg: the
// connector vLLM-Ascend registers wants the prefiller's host, port, block ids and engine id
// relayed per request, and the one driver wired for that here is the llm-d-router's decode
// proxy. The vLLM router drives its pairs itself and speaks a vocabulary the Ascend connector
// rejects, so a pair under it renders no leg rather than a dead one -- the shape Ascend always
// had, not a regression.
//
// THE LEG FOLLOWS THE PAIR, NOT THE HALF: a role that is one half of nothing has nobody to hand
// blocks to, and the router in front of it is already routing it as the undivided shape. Rendering
// the leg anyway would start an engine that waits for a counterpart no router ever assigns.
func modelDeploymentUsesKVTransfer(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, manufacturer string,
) bool {
	if md.Spec.Router == nil {
		return false
	}
	if manufacturer == nodefeature.ManufacturerAscend &&
		md.Spec.Router.Name != workercore.ModelDeploymentRouterLLMD {
		return false
	}
	if !ModelDeploymentDeclaresBothHalves(md) {
		return false
	}

	kind := ModelDeploymentEffectiveRoleKind(role)
	return kind == workercore.ModelDeploymentRoleKindPrefill ||
		kind == workercore.ModelDeploymentRoleKindDecode
}

// modelDeploymentFrontsDecodeWithSidecar reports whether a decode replica gets the routing proxy.
//
// IT STAYS WITH ONE ROUTER because the proxy is that router's own protocol, not a property of
// disaggregation: it reads the prefiller this request was assigned out of a header the picker
// writes, and neither of the other two routers writes it. Ascend is not excluded: under this
// router the proxy is also what DRIVES the transfer leg -- nothing else relays the prefiller's
// handshake into the decoder's request -- so an Ascend leg without it would be rendered and
// never pulled on.
func modelDeploymentFrontsDecodeWithSidecar(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	return md.Spec.Router != nil &&
		md.Spec.Router.Name == workercore.ModelDeploymentRouterLLMD &&
		ModelDeploymentEffectiveRoleKind(role) == workercore.ModelDeploymentRoleKindDecode
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
		Args: []string{"--kv-transfer-config", "--kv-events-config"},
		Env:  []string{"MOONCAKE_CONFIG_PATH", "VLLM_MOONCAKE_BOOTSTRAP_PORT"},
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
	// is a security property rather than a correctness one. The tenant IS the reuse domain, so a
	// workload that could set this variable could name a domain the API refuses to let it name. The
	// API refusing a self-declared domain is the durable half of that guarantee; this is the other
	// half, because the variable is a second path to the same value and an unowned key would leave
	// it open. Defaulted is exactly the wrong class: that class lets a user's value win.
	//
	// THE ESCAPE THIS PREVENTS IS BOUNDED BY SOMETHING NOT MEASURED HERE. The full attack -- mint
	// domains, get one quota account each, exceed the namespace ceiling -- needs each domain to
	// carry an INDEPENDENT quota account, and this store's quota behaves as an eviction trigger
	// rather than a hard ledger. That step is an assumption rather than a measurement, tracked on
	// the kv-cache side. Data isolation between domains IS measured (case-47), and it is a different
	// claim. Owning the key is right either way, which is why it does not wait on the answer.
	//
	// It appears in this table because the renderer always emits it for this engine. Whether the
	// selected image reads it is not decided here.
	//
	// THE THREE DISAGGREGATION KEYS ARE OWNED FOR A DIFFERENT REASON than the two above: they are
	// not a loader switch, they are the halves of a PAIR. The prefiller's bootstrap port is written
	// in three places that must agree -- its own argument, the annotation a router discovers it by,
	// and the environment the decode Pod's proxy reads -- and the mode is what makes a Pod one half
	// rather than the other. A user entry lands after the operator's on the same command line, so
	// an unowned key here lets a role declared as one half start as the other, with both ends of
	// the pair still advertising the first.
	workercore.ModelDeploymentEngineSGLang: {
		Args: []string{
			"--hicache-storage-backend", "--hicache-storage-backend-extra-config",
			"--disaggregation-mode", "--disaggregation-transfer-backend",
			"--disaggregation-bootstrap-port",
		},
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
//
// MC_FORCE_TCP pins a tcp point-to-point leg to TCP; the renderer emits it only where the pin
// cannot strip a store in the same process of its fabric. The transfer engine reads it for presence alone, so a user's own
// entry, whatever its value, asks for the same thing and is left in place.
var modelDeploymentDefaultedEnvNames = []string{"MC_TE_METRIC", inject.MooncakeForceTCPEnv}

// ModelDeploymentOwnsArg reports whether the named argument belongs to the operator on this engine.
//
// Ownership is PER (ENGINE, KEY): a key one engine owns is an ordinary user argument on another, so
// the engine is not optional and a caller that does not have one has no question to ask.
func ModelDeploymentOwnsArg(engine, arg string) bool {
	_, owned := ModelDeploymentOwnedArg(engine, arg)
	return owned
}

// ModelDeploymentOwnedArg reports which owned argument the engine's own parser reads a command-line
// entry as, so a refusal can name the key a differently spelled entry reaches.
func ModelDeploymentOwnedArg(engine, arg string) (string, bool) {
	return ModelDeploymentResolveArg(engine, arg, modelDeploymentOwnedKeys[engine].Args)
}

// ModelDeploymentResolveArg reports which of keys the engine's own parser reads a command-line entry
// as, and false when it reads it as none of them.
//
// The rules are the engine's, the same ones the parallelism parse follows. vLLM rewrites the
// underscores of a long flag to dashes up to its first dot, and merges a dotted entry
// ("--kv-transfer-config.kv_role=x") into one whole document for the flag before the dot, appended
// after every other argument so it replaces the operator's. Both engines resolve a long flag by
// argparse's unique-prefix abbreviation. SGLang rewrites and merges nothing.
//
// A PREFIX OF ANY KEY RESOLVES TO IT, without counting how many flags the engine registers under that
// prefix, because the full table is the engine's and not known here. That errs toward refusing: a
// prefix the engine calls ambiguous is refused at engine start anyway, and argparse prefers an exact
// registered spelling over an abbreviation, so the only entry wrongly resolved is one exactly equal
// to another registered flag that is also a prefix of a key. The owned flags are the registered
// flags known here, so an exact owned spelling is never read as a prefix of a longer owned key.
func ModelDeploymentResolveArg(engine, arg string, keys []string) (string, bool) {
	name := ModelDeploymentArgName(arg)
	if engine == workercore.ModelDeploymentEngineVLLM {
		name, _, _ = strings.Cut(modelDeploymentVLLMFlagKey(name), ".")
	}
	// "--" alone ends the options rather than abbreviating one.
	if !strings.HasPrefix(name, "--") || len(name) == len("--") {
		return "", false
	}

	if slices.Contains(keys, name) {
		return name, true
	}
	if slices.Contains(modelDeploymentOwnedKeys[engine].Args, name) {
		return "", false
	}
	for _, key := range keys {
		if strings.HasPrefix(key, name) {
			return key, true
		}
	}

	return "", false
}

// modelDeploymentVLLMFlagKey applies vLLM's own rewrite of a flag key: every underscore between the
// leading "--" and the first dot becomes a dash. Anything that is not a long flag is returned as is.
func modelDeploymentVLLMFlagKey(key string) string {
	if !strings.HasPrefix(key, "--") {
		return key
	}

	head, tail, dotted := strings.Cut(key, ".")
	head = strings.ReplaceAll(head, "_", "-")
	if !dotted {
		return head
	}

	return head + "." + tail
}

// ModelDeploymentOwnsEnv reports whether the named environment variable belongs to the operator on
// this engine.
//
// The config-path variable is the load-bearing entry, and it is owned for what it destroys rather
// than for what it duplicates: re-pointing it silently swaps the whole client configuration — pool
// address, transport, metadata source — for whatever the other file says, and every symptom then
// appears one layer away from its cause.
//
// The rank variables are owned ON EVERY ENGINE rather than per engine, because they describe the
// Kubernetes shape a Pod was rendered in and no engine reads them by name. They are owned even for a
// role at size one, where nothing renders them: the alternative is a rule that turns on with a field
// value, and a refusal a user meets only after widening an instance is worse than one they meet on
// the edit that names the key.
func ModelDeploymentOwnsEnv(engine, name string) bool {
	return slices.Contains(modelDeploymentRankEnvNames, name) ||
		slices.Contains(modelDeploymentOwnedKeys[engine].Env, name)
}

// modelDeploymentRankEnvNames is what a multi-Member instance publishes to its main container. It is
// declared beside the ownership test rather than beside the render, because ownership is the reason
// it is a list at all -- the renderer writes the three entries directly.
var modelDeploymentRankEnvNames = []string{
	modelDeploymentLeaderAddressEnv,
	modelDeploymentReplicaSizeEnv,
	modelDeploymentMemberIndexEnv,
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
	engine, err := ModelDeploymentInjectEngine(in.Engine, in.Manufacturer)
	if err != nil {
		return ModelDeploymentConnectorRender{}, err
	}

	// The role is the kind, mapped onto the renderer's vocabulary. An unset kind maps to RoleNone,
	// which renders exactly what a single-role deployment always rendered -- the whole reason that
	// mapping is not a special case here.
	//
	// A kind with no mapping is REFUSED rather than rendered as RoleNone. Admission already turns
	// such a kind away, so reaching this line means the two disagree, and the failure a fallback
	// would produce is the silent one: a decoder configured as a plain server, serving whole
	// requests and looking healthy.
	role, ok := modelDeploymentInjectRole(in.Kind)
	if !ok {
		return ModelDeploymentConnectorRender{}, fmt.Errorf("unsupported role kind %q", in.Kind)
	}

	// Which of the pool's offers the engine is handed is decided HERE and not at resolution,
	// because the answer needs the engine and the connection is resolved per deployment while the
	// engine varies per role. A constrained engine takes the first accepted offer; an
	// unconstrained engine takes the first for an existing binding admitted before the mix check.
	protocol, err := inject.MatchTransport(engine, in.Protocols)
	if err != nil {
		return ModelDeploymentConnectorRender{}, err
	}

	res, err := inject.Render(inject.Input{
		Engine: engine,
		Role:   role,
		// The pair term travels beside the role rather than being derived from it: the renderer's
		// split mode is a property of the deployment's whole role set, and a role alone cannot
		// know whether its other half is declared.
		Disaggregated: in.Disaggregated,
		Domain:        in.Domain,
		Connection: inject.Connection{
			MasterAddress: in.MasterServerAddress,
			Protocol:      protocol,
		},
		PublishKVEvents: in.PublishKVEvents,
		KVEventsHost:    in.KVEventsHost,
		KVTransfer:      in.KVTransfer,
		// The protocol is threaded rather than resolved here: the renderer owns the default, and
		// a second default in this file would be two definitions of one fact.
		KVTransferProtocol: in.KVTransferProtocol,
		// The pair is threaded rather than resolved here for the same reason the role is: the
		// resolution reads every role of the deployment, and synthesis sees one role's input.
		Parallelism: in.Parallelism,
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
		// wins with no refusal. The renderer's own defaulted entries follow it on the same terms.
		DefaultedEnv:   append([]core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}}, res.DefaultedEnv...),
		DefaultedArgs:  res.DefaultedArgs,
		Volumes:        res.Volumes,
		VolumeMounts:   res.VolumeMounts,
		PodAnnotations: res.PodAnnotations,
		Ports:          res.Ports,
		KVEvents:       res.KVEvents,
		KVTransfer:     res.KVTransfer,
		// Passed through rather than derived: the renderer has no way to tell which router is in
		// front, and deriving it from the transfer leg is exactly the inheritance this split ends.
		RoutingSidecar: in.RoutingSidecar,
	}, nil
}

// ModelDeploymentInjectEngine maps this API's engine value and role manufacturer onto the shared renderer's.
//
// THE TWO ENUMS ARE DELIBERATELY DIFFERENT SHAPES. This API has a single `vllm` value because on
// CANN the runner installs the vllm_ascend package for that same declared engine, so the
// accelerator decides which package runs and the user does not name it. The renderer has two,
// because the two packages register different connector names.
//
// A WRONG MAPPING HERE IS INVISIBLE IN THE TENANT OUTPUT: both vLLM entries render tenant_id, so
// swapping them changes nothing a tenant assertion could observe. What it does change is the
// connector name, which is why that is what the test for this pins.
func ModelDeploymentInjectEngine(engine, manufacturer string) (inject.Engine, error) {
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

// ModelDeploymentEffectiveRoleKind is the kind a role has, resolving the unset field.
//
// The CRD schema defaults it, so nothing arriving from the API server carries an empty one -- but
// every in-process caller that builds the value in Go does, and there are several: the webhook's
// rules, the connector's mapping and the status echo. Reading the zero value as a kind of its own
// would make them disagree about the same object, and the status echo is where that becomes costly:
// status.roles[].kind is REQUIRED and enumerated, so resolving here is what keeps a status write
// inside the enum. An empty kind written through would be refused, and a refused status write takes
// every other figure on the object down with the kind.
func ModelDeploymentEffectiveRoleKind(
	role *workercore.ModelDeploymentRole,
) workercore.ModelDeploymentRoleKind {
	if role.Kind == "" {
		return workercore.ModelDeploymentRoleKindServer
	}

	return role.Kind
}

// modelDeploymentPublishesKVEvents reports whether this role runs the event publisher.
//
// IT STAYS WITH ONE ROUTER AND ONE ENGINE. The publisher exists to feed that router's prefix-cache
// data layer, which subscribes per Pod; the other two score on their own observations and subscribe
// to nothing, so publishing under them would open two sockets on every replica that nothing reads.
func modelDeploymentPublishesKVEvents(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, manufacturer string,
) bool {
	return modelDeploymentRoutesManaged(md, manufacturer) &&
		md.Spec.Router.Name == workercore.ModelDeploymentRouterLLMD &&
		md.Spec.Engine.Name == workercore.ModelDeploymentEngineVLLM &&
		len(role.Command) == 0 &&
		ModelDeploymentEffectiveRoleKind(role) != workercore.ModelDeploymentRoleKindDecode
}

// modelDeploymentInjectRole maps this API's role kind onto the renderer's role.
//
// THE TWO ENUMS ARE SEPARATE FOR THE SAME REASON THE ENGINE ONES ARE. This API's "server" names a
// deployment shape — one process doing both halves — while the renderer's RoleNone names the absence
// of a prefill/decode split. They coincide today, and they are still not the same statement.
//
// An unset kind maps to the same place as "server": the CRD schema defaults the field, so nothing
// arriving from the API server carries an empty one, but a Go caller constructing the value directly
// does. Reading the zero value as an unknown kind would refuse every such caller.
func modelDeploymentInjectRole(kind workercore.ModelDeploymentRoleKind) (inject.Role, bool) {
	switch kind {
	case "", workercore.ModelDeploymentRoleKindServer:
		return inject.RoleNone, true
	case workercore.ModelDeploymentRoleKindPrefill:
		return inject.RolePrefill, true
	case workercore.ModelDeploymentRoleKindDecode:
		return inject.RoleDecode, true
	default:
		return "", false
	}
}

// ModelDeploymentSupportsRoleKind reports whether the engine's rendering has a term for this kind.
//
// It exists so admission can refuse a kind the engine cannot be told about, where the user can still
// fix it, rather than rendering a configuration the engine rejects at container start. The answer is
// the renderer's own table and not a copy of it: a second table would agree today and diverge on
// whichever engine release lands next, with nothing failing in between.
//
// THE POOL'S ACCELERATOR VENDOR IS NOT KNOWN AT ADMISSION — it is observed from the nodes long
// after — and the vendor is what splits the vLLM value into two renderer engines. So the kind is
// supported only when EVERY engine this API value can map to renders it. That is the one answer that
// cannot become wrong once the vendor is observed.
func ModelDeploymentSupportsRoleKind(engine string, kind workercore.ModelDeploymentRoleKind) bool {
	role, ok := modelDeploymentInjectRole(kind)
	if !ok {
		return false
	}

	for _, manufacturer := range []string{"", nodefeature.ManufacturerAscend} {
		injectEngine, err := ModelDeploymentInjectEngine(engine, manufacturer)
		if err != nil {
			return false
		}
		if !inject.SupportsRole(injectEngine, role) {
			return false
		}
	}

	return true
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
		return []string{
			"python3", "-m", "sglang.launch_server", "--model-path", model, "--enable-metrics",
		}, nil
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
// package that owns the backend. The two agreed on every enum value, so the deletion changes no
// rendered document -- with one difference worth stating: this one lowercased an UNRECOGNIZED value
// and passed it through, reasoning that the client warns and carries on. The surviving one returns
// empty for a value outside the enum, and `inject.Render` refuses an empty protocol. An object that
// reached this code with a protocol the schema does not allow never went through admission, and a
// refusal naming that is better than a lowercased guess.
