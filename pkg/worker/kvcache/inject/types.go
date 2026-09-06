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

// The engines this package renders for. Each has its own entry in the facts table and its own
// renderer, so adding one is a table row plus a file rather than a branch in shared code.
const (
	EngineVLLM       Engine = "vllm"
	EngineVLLMAscend Engine = "vllm-ascend"
	EngineSGLang     Engine = "sglang"
)

// Engines returns every engine this package RENDERS FOR, in a stable order.
//
// It exists so callers and tests enumerate one list instead of restating it: a value renderable but
// missing from here would be undescribed by the facts table.
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
				"engine anybody picks. Set %q -- the operator renders the Ascend connector on its "+
				"own for a pool whose accelerator is Ascend", value, EngineVLLM)
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
// documentation -- the same reason engineTenantSupport is read instead of each renderer knowing its
// own engine. Substituting an entry changes what renders.
var engineRoleSupport = map[Engine][]Role{
	// Both vLLM-family engines share renderVLLM, whose vllmKVRole maps all three onto kv_both,
	// kv_producer and kv_consumer.
	EngineVLLM:       {RoleNone, RolePrefill, RoleDecode},
	EngineVLLMAscend: {RoleNone, RolePrefill, RoleDecode},
	// SGLang's store configuration has no prefill/decode equivalent, so the only role it renders is
	// the absence of one.
	EngineSGLang: {RoleNone},
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
	Protocol string
}

// Input is everything the synthesis needs, already resolved and already validated.
type Input struct {
	// Engine selects the renderer.
	Engine Engine

	// Role is the prefill/decode role, RoleNone when the caller declared none.
	Role Role

	// Domain is the reuse domain the Binding declared, carried here so an engine that FORWARDS a
	// tenant can be given one. Note the direction: an earlier revision carried a domain here so the
	// synthesis could REFUSE on it, which is the opposite use and is gone.
	//
	// Whether it is emitted is per engine and nothing else - the renderer that reads a tenant emits
	// it, the one that does not, does not. There is no version check, and there cannot be one here:
	// nothing on this path inspects the container image, so a build older than the one measured would
	// be handed a variable it never reads. That is why the stamp records what was INJECTED rather than
	// whether isolation resulted.
	Domain string

	// Connection is what the pool and its backend published.
	Connection Connection
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

	// ReasonRoleUnsupported is a role declared for an engine whose equivalent knob is not yet
	// known. Accepting and ignoring it would be the silent wrong result this package exists to
	// avoid.
	ReasonRoleUnsupported Reason = "RoleUnsupported"

	// ReasonConnectionIncomplete is a Connection missing a value the engine cannot start without.
	ReasonConnectionIncomplete Reason = "ConnectionIncomplete"
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
