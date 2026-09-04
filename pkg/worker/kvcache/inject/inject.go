// This file is the package's only entry point and the gates it runs before dispatching to an engine.
package inject

import (
	core "k8s.io/api/core/v1"
)

// Result is a rendering, as values a caller applies to an object this package never sees.
//
// Nothing here is applied. The caller owns the object - a Pod under admission, a workload's Pod
// template elsewhere - and decides how each piece lands on it, which is what lets one renderer serve
// callers that mutate different kinds.
type Result struct {
	// Env is the variables the target container needs, as DESIRED values.
	//
	// A caller that finds the container already declaring a variable of one of these names leaves the
	// container's own value in place: an injection does not overrule what a workload declared for
	// itself. This repository already has that rule and a helper for it,
	// `deviceplugin.ContainerEnvDeclared`, and this package expects its callers to use it rather than
	// to invent a second precedence.
	Env []core.EnvVar

	// Args is appended to the target container's args, in order.
	//
	// Both engines need an argument that has no environment equivalent, so this is never empty. The
	// caller is responsible for one precondition this package cannot check: a container declaring
	// NEITHER command nor args must not receive these, because Kubernetes then reads args as the whole
	// command line and discards the image's own.
	Args []string

	// Volumes is added to the Pod's spec, and VolumeMounts to the target container. Both are empty for
	// an engine whose vehicle is the environment.
	Volumes      []core.Volume
	VolumeMounts []core.VolumeMount

	// PodAnnotations must be set on the same Pod - or the same Pod template - whose spec receives the
	// fields above. The file vehicle is a downwardAPI projection of an annotation, so the projection
	// and the annotation it reads are two halves of one thing: applying the volume without the
	// annotation mounts an empty file.
	PodAnnotations map[string]string

	// TenantInjected is whether this render PRODUCED a tenant identity. It is an ACTION and not an
	// outcome: whether the engine build honors the value is not knowable here, so nothing downstream
	// may turn it into a claim about isolation.
	//
	// It is not, on its own, whether a tenant reached the container. A caller applies Env under this
	// repository's precedence rule, so a variable the workload already declared is left alone - and
	// for the environment vehicle that is exactly the tenant. The renderer cannot see that happen,
	// which is why TenantEnvName exists: the caller has to complete this answer, and a stamp built
	// from this field alone would claim a tenant the container does not carry.
	TenantInjected bool

	// TenantEnvName is the variable the tenant travels in, empty when it travels in the file instead
	// (or when none was produced). It exists so a caller can tell whether its own precedence rule
	// silently dropped the tenant: the file vehicle is written wholesale and cannot be overridden
	// that way, the environment one can.
	TenantEnvName string
}

// Render turns a resolved input into the artifacts one container needs to use a KV cache pool.
//
// It is a pure function over values: no Kubernetes client, no context, no cluster reads. Deciding WHAT
// to render belongs to the caller; this is only the turning of that decision into artifacts.
//
// It refuses rather than approximating. Every refusal here names a case where rendering something
// would leave a container that starts normally, looks configured, and does not use the cache - the
// failure mode that is invisible from outside the Pod and therefore the one worth failing loudly for.
//
// The reuse domain IS an input, and it is never a reason to refuse. Whether it gets rendered is the
// facts table's answer for that engine: one that reads a tenant is given the domain, one that does not
// is given nothing, because a key nothing reads would be decoration that reads as a guarantee. A
// refusal keyed on the domain would reject every caller, since every Binding declares one.
func Render(in Input) (*Result, error) {
	if _, ok := engineFactsFor(in.Engine); !ok {
		return nil, newRefusal(ReasonEngineUnknown,
			"engine %q is not one this operator can configure; set one of %v", in.Engine, Engines())
	}

	if in.Connection.MasterAddress == "" {
		return nil, newRefusal(ReasonConnectionIncomplete,
			"the pool published no client endpoint; there is no address for engine %q to connect to",
			in.Engine)
	}
	if in.Connection.Protocol == "" {
		return nil, newRefusal(ReasonConnectionIncomplete,
			"the backend published no transport; it is written explicitly because the engines "+
				"disagree on the default, so omitting it would pick one of them at random")
	}

	switch in.Engine {
	case EngineVLLM, EngineVLLMAscend:
		return renderVLLM(in)
	case EngineSGLang:
		return renderSGLang(in)
	default:
		// Unreachable: the facts table above accepts exactly the engines this switch covers, and
		// TestEngines_AreAllKnown pins the two lists to each other.
		return nil, newRefusal(ReasonEngineUnknown, "engine %q has no renderer", in.Engine)
	}
}
