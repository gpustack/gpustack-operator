// Package inject renders the KV cache client configuration a Pod's engine container needs, as a
// pure function over a value: no Kubernetes client, no context, no cluster types. Resolving what to
// render is the admission webhook's job; turning that into env, args, volumes and mounts is this
// package's, so every case in its test plan is a table row and needs neither a cluster nor an engine.
//
// The contract this package renders against is the ENGINE's own config file schema, never the
// Mooncake client's setup() signature. The two disagree on names -- master_server_address against
// master_server_addr, device_name against rdma_devices -- and the file reader ignores a key it does
// not recognize, so a value written under the signature's spelling is dropped in silence.
package inject

import (
	"fmt"
	"slices"
)

// Engine names the inference engine whose configuration is being rendered.
//
// It is always declared by the caller and never inferred from a container image: engines take
// completely different flags, and a renamed or vendored image would mis-inject without any symptom
// beyond a cache that is never used.
type Engine string

// The engines this package renders for.
const (
	EngineVLLM       Engine = "vllm"
	EngineVLLMAscend Engine = "vllm-ascend"
	EngineSGLang     Engine = "sglang"
)

// Engines returns every engine this package RENDERS FOR, in a stable order.
//
// It is WIDER than the set a user may name -- see SelectableEngines.
func Engines() []Engine {
	return []Engine{EngineVLLM, EngineVLLMAscend, EngineSGLang}
}

// SelectableEngines returns the engines a user may NAME, in a stable order.
//
// EngineVLLMAscend is absent, and that is what keeps this annotation agreeing with
// ModelDeployment.spec.engine, which closed the same question first: vllm_ascend is the package the
// runner installs when the accelerator backend is CANN, not an engine anybody picks. It is DERIVED
// here too -- the operator selects it from the pool's accelerator -- so it stays renderable while
// ceasing to be nameable. Two API surfaces publishing different value sets for one concept is what
// this split removes; it is not a second surface describing the difference.
func SelectableEngines() []Engine {
	return []Engine{EngineVLLM, EngineSGLang}
}

// ParseEngine converts the engine annotation's value, refusing anything a user may not name.
//
// An unknown value is a refusal rather than a default because there is no safe engine to guess:
// each takes different flags, and injecting the wrong set produces a container that starts normally
// and caches nothing.
func ParseEngine(value string) (Engine, error) {
	for _, engine := range SelectableEngines() {
		if string(engine) == value {
			return engine, nil
		}
	}

	// Refused HERE rather than by dropping the constant, because this is where the value arrives.
	// It names the replacement, since the reason it is refused is also the reason the replacement
	// is right: the accelerator decides the package, so the engine to name is the plain one.
	if value == string(EngineVLLMAscend) {
		return "", newRefusal(ReasonEngineUnknown,
			"engine %q is not one this annotation takes: it names the Python package vllm_ascend, "+
				"which the runner installs when the accelerator backend is CANN, rather than an "+
				"engine anybody picks. Set %q and declare the Ascend runtime with "+
				"kvcache.gpustack.ai/manufacturer=%q", value, EngineVLLM, "ascend")
	}

	return "", newRefusal(ReasonEngineUnknown,
		"engine %q is not one this operator can configure; set one of %v", value, SelectableEngines())
}

// Role is the prefill/decode role a caller may declare for its Pod.
type Role string

// The roles, plus the absence of one. RoleNone is a value rather than an empty string used bare, so
// a renderer switching on the role has no unlabelled case.
const (
	RoleNone    Role = ""
	RolePrefill Role = "prefill"
	RoleDecode  Role = "decode"
)

// ParseRole converts the role annotation's value. An unset annotation is legal and means the caller
// has no prefill/decode split, which is the ordinary case for a shared cache.
func ParseRole(value string) (Role, error) {
	switch Role(value) {
	case RoleNone, RolePrefill, RoleDecode:
		return Role(value), nil
	default:
		return "", newRefusal(ReasonRoleUnknown,
			"role %q is not recognised; set %q, %q, or leave the annotation off",
			value, RolePrefill, RoleDecode)
	}
}

// engineRoleSupport is the set of roles each engine's rendering has a term for.
//
// It is a table rather than a branch inside each renderer because an ADMISSION handler has to ask
// the question before anything is rendered: a role an engine has no term for must be refused where
// the user can still fix it, not at container start. A caller that could only learn the answer by
// calling Render would have to build a whole Input to ask.
//
// The renderers read this table rather than restating it, so it is a live constraint rather than
// documentation. Substituting an entry changes what renders.
var engineRoleSupport = map[Engine][]Role{
	// Both vLLM-family engines share renderVLLM, whose vllmKVRole maps all three onto kv_both,
	// kv_producer and kv_consumer.
	EngineVLLM:       {RoleNone, RolePrefill, RoleDecode},
	EngineVLLMAscend: {RoleNone, RolePrefill, RoleDecode},
	// SGLang's store client is role-blind, but its disaggregation arguments carry both roles, so
	// all three render. The two kinds' ONLY rendering is the disaggregation one -- a kind asked
	// for without the point-to-point leg is refused by the renderer rather than rendered as a
	// store member wearing a label, which would be a container that looks configured and pairs
	// with nothing.
	EngineSGLang: {RoleNone, RolePrefill, RoleDecode},
}

// SupportsRole reports whether the engine's rendering has a term for the role.
//
// An unknown engine, and a role outside the accepted set, both report false: the side that claims
// less, and the side that refuses rather than renders something nothing reads.
func SupportsRole(engine Engine, role Role) bool {
	return slices.Contains(engineRoleSupport[engine], role)
}

// Connection is what resolution already established about the pool the Pod will talk to.
//
// It carries no metadata-plane address and no RDMA device list, and neither is an omission. The
// metadata plane is peer-to-peer, so there is no address to carry -- every participant writes one
// constant. The device filter is left empty so the client discovers per host, which is the only
// value correct for every consumer of one pool.
type Connection struct {
	// MasterAddress is the pool's published client endpoint, host:port. It is the address an
	// inference engine connects to, never the backend's admin address, which serves the quota
	// ledger and is republished nowhere.
	MasterAddress string

	// Protocol is the transport in the artifact's own spelling, already mapped from the backend's
	// API spelling by the caller. It is backend-wide rather than per-node: one member group renders
	// one DaemonSet, so a single Pod template cannot carry a different transport per node.
	//
	// It feeds the store client alone. The point-to-point leg does not take its transport from it
	// -- the two data planes declare separately, and defaultKVTransferProtocol says why; the leg
	// reads it only to learn whether pinning itself to tcp would strip the store of its fabric,
	// and directLegForcesTCP says why.
	Protocol string
}

// Parallelism is one role's declared parallel shape, in the two degrees the point-to-point
// transfer document carries. The zero value is the shape nothing declared; orOne renders it as
// the engine's own default of one, which is what the document wrote when it held literals.
type Parallelism struct {
	// TensorParallel is the tensor-parallel width the role declared.
	TensorParallel int

	// DataParallel is the data-parallel width the role declared.
	DataParallel int
}

// orOne maps an undeclared degree to the engine's own default of one, so the document never
// claims a zero-width role.
func (p Parallelism) orOne() Parallelism {
	if p.TensorParallel == 0 {
		p.TensorParallel = 1
	}
	if p.DataParallel == 0 {
		p.DataParallel = 1
	}

	return p
}

// ParallelismPair is the declared parallel shape of BOTH roles of a prefill/decode pair. The
// two halves travel as one value because the document asserts on them together: a renderer
// that could fill one side from the caller and default the other would answer with a wrong
// block layout instead of a refusal.
type ParallelismPair struct {
	// Prefill is the prefill role's declared shape.
	Prefill Parallelism

	// Decode is the decode role's declared shape.
	Decode Parallelism
}

// Input is everything the synthesis needs, already resolved and already validated.
type Input struct {
	// Engine selects the renderer.
	Engine Engine

	// Role is the prefill/decode role, RoleNone when the caller declared none.
	Role Role

	// Disaggregated reports whether the deployment declaring this role declares BOTH halves of a
	// prefill/decode pair.
	//
	// IT GATES THE ENGINE'S OWN SPLIT MODE, because the role alone cannot: a deployment holding one
	// half has no pair to hand blocks to, so an engine started in half mode waits on a counterpart
	// nothing mints while whatever routes it sends whole requests -- two layers of one object
	// answering "is this a split" differently. The routers made the same move first: their renderer
	// enters disaggregation only with both selectors present and routes a lone half as the undivided
	// shape, and this field is that rule reaching the engine side. The cost is stated rather than
	// hidden: a deployment deliberately declaring one half to feed a shared store runs that engine
	// undivided now, where it used to start as one half of a pair that was never declared.
	Disaggregated bool

	// Domain is the reuse domain the Binding declared. A non-empty value is emitted by every engine
	// that carries a tenant identity; an empty one — a master that holds no tenant ledger — renders
	// no tenant at all. There is no engine-version check because this path does not inspect the
	// container image.
	Domain string

	// Connection is what the pool and its backend published.
	Connection Connection

	// CachePrefix is the weight identity the store's keys are prefixed with, or empty for none.
	//
	// NEITHER ENGINE'S STORE KEY NAMES THE WEIGHTS. vLLM's Mooncake store keys carry the last path
	// segment of --model, SGLang's the served model name, and neither carries a revision or a
	// digest, so two deployments serving different weights under one tenant read each other's
	// blocks -- measured on vLLM between two commits of one repository. The prefix is the key
	// namespace each engine lets the server side set: vLLM's kv_connector_extra_config.cache_prefix
	// and SGLang's extra_backend_tag. It renders only with a store: a point-to-point leg keys
	// nothing.
	//
	// It must not contain "@", "_" or ":", the separators the two engines join their keys with;
	// Render refuses one that does.
	CachePrefix string

	// Dtype is the element type the Binding declared, rendered as KVCacheDtypeArg beside the store
	// and passed through verbatim: the accepted spellings are each engine's, and this package does
	// not judge them. Empty renders none.
	//
	// NEITHER ENGINE'S STORE KEY CARRIES THE DTYPE, so two engines caching one prompt under
	// different dtypes write the same key. A reader with the wider dtype then loads the narrower
	// block as a success over a partly stale buffer, because neither engine compares the bytes read
	// with the bytes expected. Rendering the Binding's value makes every engine on that Binding
	// write one element type, including an engine left on "auto", which follows the model instead.
	Dtype string

	// KVTransfer enables the point-to-point connector used by a managed prefill/decode router.
	// With a complete Connection it is composed with the shared store; without one it is rendered on
	// its own. The two are orthogonal inputs, not alternatives: both may be on at once.
	KVTransfer bool

	// KVTransferProtocol is the transport the point-to-point leg is told to use, declared by
	// the caller. Empty selects the renderer's default. It is not gated: the accepted set is a
	// property of the mooncake build inside the engine's own image, which this operator neither
	// ships nor can inspect, so gating it here would hard-code one image's compile set onto
	// another image's connector. The vLLM family reads it only when KVTransfer is set; SGLang
	// reads it on a disaggregated half, whose split follows the pair rather than the flag, and
	// maps it onto its transfer backend rather than passing it through.
	KVTransferProtocol string

	// Parallelism is the pair's declared parallel shape, resolved by the caller off each
	// role's own books. Only the document that asserts on it reads it -- the Ascend transfer
	// leg's -- and the zero value renders 1/1 there, exactly what the leg's inline literals
	// rendered, so a caller with no roles to parse changes nothing.
	Parallelism ParallelismPair

	// PublishKVEvents asks the engine to publish cache-placement events for a router. It is resolved
	// per role by the caller; a false value preserves the ordinary connector render byte for byte.
	PublishKVEvents bool

	// KVEventsHost is the dialable host paired with the publisher's fixed ports. The engine binds a
	// wildcard address, which cannot be published to a consumer as an endpoint.
	KVEventsHost string
}

// defaultKVTransferProtocol is the transport the prefill-to-decode leg is told to use when the
// caller declares none, and it is deliberately NOT resolved from the backend. Both engines'
// renderers read it through directLegProtocol, so the default is defined once.
//
// KVCacheBackend.spec.transport defines the data plane the store MEMBERS run. This leg is engine
// to engine and never traverses the store, so the two planes have no business sharing one value
// -- yet they did: a pair with no store always rendered tcp even on fabric hardware, and a pair
// with one inherited the members' transport, an RDMA pool telling engine Pods to run a fabric
// this operator gives them no access to. The backend-level field is also set to become an
// inherited default once member groups can override it, which would leave this leg reading a
// value no group necessarily uses.
//
// No source can DISCOVER the right value: the accepted set is a property of the mooncake build
// inside the engine's own image, which this operator neither ships nor can inspect. The value is
// therefore DECLARED, and the declarer is the ModelDeployment's spec.kvTransfer.protocol. This
// constant is the default when that field is unset: "tcp" is the one answer honest from here --
// the transport every mooncake build carries, and what a store-less pair has always rendered. A
// pair whose engines can speak a fabric protocol says so through the API; the vLLM renderer's
// gating rule binds the declared value exactly as it binds this default.
const defaultKVTransferProtocol = "tcp"

// directLegProtocol is the transport the point-to-point leg is told to use: the declared value,
// or the default when none was declared.
func directLegProtocol(in Input) string {
	if in.KVTransferProtocol != "" {
		return in.KVTransferProtocol
	}

	return defaultKVTransferProtocol
}

// MooncakeForceTCPEnv is the variable that makes the Mooncake transfer engine install TCP alone. It
// is read by the transfer engine rather than by any engine's config class, and only for presence.
const MooncakeForceTCPEnv = "MC_FORCE_TCP"

// KVCacheDtypeArg is the flag both engine families read the KV cache element type from: vLLM
// v0.29.0 `vllm/config/cache.py` CacheDType and SGLang v0.5.18 `server_args.py` kv_cache_dtype.
// Each validates it against its own choices at startup, so a spelling the engine does not know stops
// the container instead of being ignored.
const KVCacheDtypeArg = "--kv-cache-dtype"

// directLegForcesTCP reports whether a leg that resolved to tcp is also PINNED to it, beyond
// being told so.
//
// Telling is not enough: the Mooncake transfer engine selects its transport from the host's
// hardware and does not read the connector's protocol key, so on a host with no RDMA device a
// build with multi-node NVLink compiled in installs NVLink even between hosts that have no NVLink
// path, and the leg moves nothing. The one switch it does read is MooncakeForceTCPEnv, whose
// mere presence makes the engine install TCP alone.
//
// That switch is PROCESS-WIDE: it returns early from the transfer engine's init, and the store
// client in the same process initializes through that same function. So the leg is pinned only
// when every transfer engine in the process wants tcp -- a store whose transport is not tcp would
// otherwise be left without the fabric it was given. With such a store the leg keeps the engine's
// own selection.
func directLegForcesTCP(in Input) bool {
	if directLegProtocol(in) != "tcp" {
		return false
	}

	return in.Connection.MasterAddress == "" || in.Connection.Protocol == "tcp"
}

// Reason classifies a refusal. Callers branch on it; the message that accompanies it is for a human
// and may be reworded without breaking them.
type Reason string

// The reasons this package refuses. Each names a condition under which rendering anything at all
// would produce a container that looks configured and is not.
const (
	// ReasonEngineUnknown is an engine value outside the accepted set.
	ReasonEngineUnknown Reason = "EngineUnknown"

	// ReasonRoleUnknown is a role value outside the accepted set.
	ReasonRoleUnknown Reason = "RoleUnknown"

	// ReasonRoleUnsupported is a role - or a capability requested alongside one - that the engine
	// has no known knob for: a role the engine cannot express, a role PAIRING whose declared
	// values the engine cannot run (each role legal alone, the combination refused at engine
	// start), or point-to-point transfer or KV event publishing asked of an engine that renders
	// neither. Accepting and ignoring it would be the silent wrong result this package exists to
	// avoid.
	ReasonRoleUnsupported Reason = "RoleUnsupported"

	// ReasonConnectionIncomplete is an input missing something rendering cannot proceed without:
	// a Connection missing a value the engine cannot start without, an input that requests none
	// of the store, direct transfer, or event publishing, an engine that cannot run without a
	// shared store, or an event publisher with no dialable host.
	ReasonConnectionIncomplete Reason = "ConnectionIncomplete"

	// ReasonTransportUnsupported is a pool transport the engine's store backend refuses. It is
	// separate from ReasonConnectionIncomplete because the Connection is complete: every value is
	// present and legal, and it is the PAIR that no container can run.
	ReasonTransportUnsupported Reason = "TransportUnsupported"

	// ReasonCachePrefixInvalid is a cache prefix holding a separator the engines join their store
	// keys with. Such a prefix would be joined into keys another prefix can also produce, so two
	// weight identities could read each other's blocks while both look configured.
	ReasonCachePrefixInvalid Reason = "CachePrefixInvalid"
)

// RefusalError is a rendering that was declined, carrying the reason a caller branches on and a
// message naming the subject it declined.
type RefusalError struct {
	Reason  Reason
	Message string
}

// Error implements error.
func (r *RefusalError) Error() string {
	return r.Message
}

// newRefusal builds a typed refusal. The format arguments name the subject -- the engine, the role,
// the domain -- because a refusal a reader cannot act on is barely better than a silent one.
func newRefusal(reason Reason, format string, args ...any) error {
	return &RefusalError{
		Reason:  reason,
		Message: fmt.Sprintf(format, args...),
	}
}
