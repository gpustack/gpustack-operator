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
	// It feeds the store client alone. The point-to-point leg does not read it -- the two data
	// planes declare separately, and vllmKVTransferProtocol says why.
	Protocol string
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

	// KVTransfer enables the point-to-point connector used by a managed prefill/decode router.
	// With a complete Connection it is composed with the shared store; without one it is rendered on
	// its own. The two are orthogonal inputs, not alternatives: both may be on at once.
	KVTransfer bool

	// KVTransferProtocol is the transport the point-to-point leg is told to use, declared by
	// the caller. Empty selects the renderer's default. It is passed through verbatim: the
	// accepted set is a property of the mooncake build inside the engine's own image, which this
	// operator neither ships nor can inspect, so gating it here would hard-code one image's
	// compile set onto another image's connector. It is read only when KVTransfer is set.
	KVTransferProtocol string

	// PublishKVEvents asks the engine to publish cache-placement events for a router. It is resolved
	// per role by the caller; a false value preserves the ordinary connector render byte for byte.
	PublishKVEvents bool

	// KVEventsHost is the dialable host paired with the publisher's fixed ports. The engine binds a
	// wildcard address, which cannot be published to a consumer as an endpoint.
	KVEventsHost string
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
	// has no known knob for: a role the engine cannot express, or point-to-point transfer or KV
	// event publishing asked of an engine that renders neither. Accepting and ignoring it would
	// be the silent wrong result this package exists to avoid.
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
