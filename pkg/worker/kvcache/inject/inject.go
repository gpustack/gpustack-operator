// This file is the package's only entry point and the gates it runs before dispatching to an engine.
package inject

import (
	"fmt"
	"slices"
	"strings"

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
	// A caller leaves a container-declared value in place, except TenantEnvName. That value comes from
	// the resolved Binding and must overwrite every duplicate declaration so the workload cannot choose
	// another reuse domain.
	Env []core.EnvVar

	// DefaultedEnv is the variables the target container gets only where it declares none of the
	// same name. Unlike Env, a container-declared value is never overwritten here, because each of
	// these is a default the workload may legitimately answer for itself.
	DefaultedEnv []core.EnvVar

	// Args is appended to the target container's args, in order.
	//
	// Both engines need an argument that has no environment equivalent, so this is never empty. The
	// caller is responsible for one precondition this package cannot check: a container declaring
	// NEITHER command nor args must not receive these, because Kubernetes then reads args as the whole
	// command line and discards the image's own.
	Args []string

	// DefaultedArgs is argument groups appended after Args, each starting with its flag. A caller
	// drops a whole group when the container's own arguments already name that flag, in either
	// "--flag value" or "--flag=value" form: each is a default the workload may answer for itself,
	// and a switch that only turns something on could not be turned off by a later entry.
	DefaultedArgs [][]string

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
	// For TenantEnvName, callers overwrite every workload declaration, so a true value means the
	// resolved tenant was written to the container. TenantEnvName identifies that exception.
	TenantInjected bool

	// TenantEnvName is the variable the tenant travels in, empty when it travels in the file instead
	// (or when none was produced). Callers use it to apply the Binding's tenant value over any
	// workload declaration of the same environment variable.
	TenantEnvName string

	// Ports are the additional container ports opened by synthesized configuration.
	Ports []core.ContainerPort

	// KVEvents is the event stream as a consumer reaches it, absent when publishing is disabled.
	KVEvents *KVEvents

	// KVTransfer reports that the rendered transfer document includes the point-to-point arm.
	KVTransfer bool
}

// KVEvents is the dialable cache-event contract produced alongside the engine's bind configuration.
//
// The Endpoint is the per-role ClusterIP Service, and a connection to it is pinned to one backing
// Pod: with more than one replica a consumer sees only one producer's stream. The managed router
// therefore consumes the stream through per-Pod discovery instead of this endpoint. The ZMQ
// publisher binds a wildcard address and authenticates nothing, so the events plane expects
// network isolation - it sits in the same trust domain as the data plane.
type KVEvents struct {
	Endpoint       string
	ReplayEndpoint string
	Topic          string
}

// VLLMKVEvents returns the dialable event contract paired with vLLM's fixed bind configuration.
func VLLMKVEvents(host string) *KVEvents {
	return &KVEvents{
		Endpoint:       fmt.Sprintf("tcp://%s:%d", host, VLLMKVEventsPort),
		ReplayEndpoint: fmt.Sprintf("tcp://%s:%d", host, VLLMKVEventsReplayPort),
		Topic:          VLLMKVEventsTopic,
	}
}

// KVEventsPorts returns the ports used by the vLLM ZMQ publisher and its replay endpoint.
func KVEventsPorts() []core.ContainerPort {
	return []core.ContainerPort{
		{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: VLLMKVEventsPort},
		{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: VLLMKVEventsReplayPort},
	}
}

// cachePrefixSeparators are the characters vLLM and SGLang join a store key's parts with.
const cachePrefixSeparators = "@_:"

// Render turns a resolved input into the artifacts one container needs to use a KV cache pool.
//
// It is a pure function over values: no Kubernetes client, no context, no cluster reads. Deciding WHAT
// to render belongs to the caller; this is only the turning of that decision into artifacts.
//
// It refuses rather than approximating. Every refusal here names a case where rendering something
// would leave a container that starts normally, looks configured, and does not use the cache - the
// failure mode that is invisible from outside the Pod and therefore the one worth failing loudly for.
//
// The reuse domain IS an input, and it is never a reason to refuse. A non-empty domain is always
// rendered; whether the engine build reads it is the image owner's compatibility responsibility.
func Render(in Input) (*Result, error) {
	if !slices.Contains(Engines(), in.Engine) {
		return nil, newRefusal(ReasonEngineUnknown,
			"engine %q is not one this operator can configure; set one of %v", in.Engine, Engines())
	}

	hasStore := in.Connection.MasterAddress != "" || in.Connection.Protocol != ""
	if hasStore && in.Connection.MasterAddress == "" {
		return nil, newRefusal(ReasonConnectionIncomplete,
			"the pool published no client endpoint; there is no address for engine %q to connect to",
			in.Engine)
	}
	if hasStore && in.Connection.Protocol == "" {
		return nil, newRefusal(ReasonConnectionIncomplete,
			"the backend published no transport; it is written explicitly because the engines "+
				"disagree on the default, so omitting it would pick one of them at random")
	}
	// Refused HERE rather than at either caller, because this is the one point both of them pass
	// through: the Pod admission webhook renders from an annotation, the ModelDeployment reconciler
	// from an accelerator it derived, and only one of the two can name vLLM-Ascend today. A check on
	// the caller that can would leave the other admitting the pair, and a check on both would be two
	// implementations of one table.
	if hasStore {
		if err := checkTransport(in.Engine, in.Connection.Protocol); err != nil {
			return nil, err
		}
	}
	if !hasStore && !in.KVTransfer && !in.PublishKVEvents {
		return nil, newRefusal(ReasonConnectionIncomplete,
			"no shared store, point-to-point transfer, or KV event publisher was requested")
	}
	if !hasStore && in.Engine != EngineVLLM && in.Engine != EngineVLLMAscend &&
		(in.Engine != EngineSGLang || !in.KVTransfer) {
		return nil, newRefusal(ReasonConnectionIncomplete,
			"engine %q requires a shared store connection", in.Engine)
	}
	// Point-to-point transfer is rendered by the whole vLLM family and by SGLang, each in its
	// own vocabulary; KV event publishing is vLLM proper's alone. A capability an engine does not
	// render is refused HERE rather than ignored by that engine's renderer.
	//
	// It is refused rather than dropped because dropping it is the failure this package exists to
	// prevent: an engine asked for a capability it renders no term for would start normally, serve
	// normally, and move no blocks - with nothing in the Pod to read that says so. The combination
	// is reachable, not theoretical: the router's metrics contract covers SGLang, so a managed
	// router over an SGLang pool is a configuration a user can write today.
	//
	// The vLLM renderer refuses point-to-point transfer again, on a condition that also covers the
	// role. That is not a duplicate of this one: this check is about the ENGINE and runs for every
	// caller, while that one is about a role that is neither prefill nor decode and can only be
	// reached once the engine is already vLLM.
	if in.KVTransfer && in.Engine != EngineVLLM && in.Engine != EngineVLLMAscend && in.Engine != EngineSGLang {
		return nil, newRefusal(ReasonRoleUnsupported,
			"engine %q renders no point-to-point transfer; asking for one would leave a container "+
				"that starts and moves nothing", in.Engine)
	}
	if in.PublishKVEvents && in.Engine != EngineVLLM {
		return nil, newRefusal(ReasonRoleUnsupported,
			"engine %q renders no KV event publishing; asking for it would leave a container that "+
				"starts and moves nothing", in.Engine)
	}

	if strings.ContainsAny(in.CachePrefix, cachePrefixSeparators) {
		return nil, newRefusal(ReasonCachePrefixInvalid,
			"cache prefix %q holds one of %q, the separators the engines join store keys with",
			in.CachePrefix, cachePrefixSeparators)
	}

	switch in.Engine {
	case EngineVLLM, EngineVLLMAscend:
		return renderVLLM(in)
	case EngineSGLang:
		return renderSGLang(in)
	default:
		// Unreachable: the Engines check above accepts exactly the values this switch covers.
		return nil, newRefusal(ReasonEngineUnknown, "engine %q has no renderer", in.Engine)
	}
}
