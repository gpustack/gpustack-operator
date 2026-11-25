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

// VLLMKVEvents returns the dialable event contract paired with vLLM's fixed bind configuration
// and the legacy bare topic prefix.
func VLLMKVEvents(host string) *KVEvents {
	return KVEventsForTopic(host, VLLMKVEventsTopic)
}

// KVEventsMaxLocalDP is the maximum per-Pod DP publisher span whose ZMQ PUB ports
// [5557 .. 5557+localDP-1] and ROUTER replay ports [5557+localDP .. 5557+2*localDP-1] fit inside
// the TCP port range [1, 65535] without overlap or overflow.
const KVEventsMaxLocalDP int32 = (65535 - VLLMKVEventsPort + 1) / 2

// ValidateKVEventsLocalDP refuses a KV event DP publisher span whose ZMQ PUB and ROUTER replay
// port range overflows TCP port 65535.
func ValidateKVEventsLocalDP(localDP int32) error {
	if localDP > KVEventsMaxLocalDP {
		return newRefusal(ReasonKVEventsPortOverflow,
			"KV events data-parallel span %d overflows TCP port 65535 (max supported span is %d from base port %d)",
			localDP, KVEventsMaxLocalDP, VLLMKVEventsPort)
	}
	return nil
}

// KVEventsReplayPortForLocalDP returns the ZMQ ROUTER replay base port for localDP per-Pod
// publishers. Single-DP workloads (localDP <= 1) keep VLLMKVEventsReplayPort (5558); multi-DP
// workloads place replay at VLLMKVEventsPort + localDP so rank r's PUB (5557 + r) and replay
// (5557 + localDP + r) sockets never overlap. Returns 0 when localDP exceeds KVEventsMaxLocalDP;
// callers should validate with ValidateKVEventsLocalDP first.
func KVEventsReplayPortForLocalDP(localDP int32) int32 {
	if localDP <= 1 {
		return VLLMKVEventsReplayPort
	}
	if localDP > KVEventsMaxLocalDP {
		return 0
	}
	return VLLMKVEventsPort + localDP
}

// KVEventsForTopic returns the dialable event contract paired with the ZMQ publisher's default
// single-DP ports and the given topic.
func KVEventsForTopic(host, topic string) *KVEvents {
	return KVEventsForTopicAndLocalDP(host, topic, 1)
}

// KVEventsForTopicAndLocalDP returns the dialable event contract paired with VLLMKVEventsPort,
// KVEventsReplayPortForLocalDP(localDP), and the given topic.
func KVEventsForTopicAndLocalDP(host, topic string, localDP int32) *KVEvents {
	return &KVEvents{
		Endpoint:       fmt.Sprintf("tcp://%s:%d", host, VLLMKVEventsPort),
		ReplayEndpoint: fmt.Sprintf("tcp://%s:%d", host, KVEventsReplayPortForLocalDP(localDP)),
		Topic:          topic,
	}
}

// VLLMKVEventsTopicForModel returns the 3-segment vLLM / vLLM-Ascend KV event publisher topic
// ("kv@pod@<model>") expected by llm-d-router parseTopic and vllm-router /kv_event_sources.
// An empty model falls back to VLLMKVEventsTopic ("kv@").
func VLLMKVEventsTopicForModel(model string) string {
	if model == "" {
		return VLLMKVEventsTopic
	}
	return VLLMKVEventsTopic + "pod@" + model
}

// engineVersionAtLeast reports whether version satisfies minimum under CompareEngineVersion.
// An empty version represents the legacy baseline (< minimum). Non-empty invalid versions return
// a typed refusal.
func engineVersionAtLeast(version, minimum string) (bool, error) {
	if version == "" {
		return false, nil
	}
	ok, err := EngineVersionAtLeast(version, minimum)
	if err != nil {
		return false, newRefusal(ReasonEngineVersionInvalid, "%v", err)
	}
	return ok, nil
}

// KVEventsPorts returns the container ports used by a single-DP ZMQ publisher and replay endpoint.
func KVEventsPorts() []core.ContainerPort {
	return KVEventsPortsForLocalDP(1)
}

// KVEventsPortsForLocalDP returns the base ZMQ publisher and replay container ports for localDP
// per-Pod publishers.
func KVEventsPortsForLocalDP(localDP int32) []core.ContainerPort {
	return []core.ContainerPort{
		{Name: "kv-events", Protocol: core.ProtocolTCP, ContainerPort: VLLMKVEventsPort},
		{Name: "kv-replay", Protocol: core.ProtocolTCP, ContainerPort: KVEventsReplayPortForLocalDP(localDP)},
	}
}

// KVEventsReservedPortsForLocalDP returns every TCP port bound inside the Pod by localDP ZMQ PUB
// and ROUTER replay publishers: [5557 .. 5557 + 2*max(localDP, 1) - 1]. Returns nil when localDP
// exceeds KVEventsMaxLocalDP; callers must validate with ValidateKVEventsLocalDP so overflowing
// spans are refused rather than understated.
func KVEventsReservedPortsForLocalDP(localDP int32) []int32 {
	if localDP < 1 {
		localDP = 1
	}
	if localDP > KVEventsMaxLocalDP {
		return nil
	}
	end := VLLMKVEventsPort + 2*localDP - 1
	ports := make([]int32, 0, int(end-VLLMKVEventsPort+1))
	for p := VLLMKVEventsPort; p <= end; p++ {
		ports = append(ports, p)
	}
	return ports
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
	if in.EngineVersion != "" {
		if err := ValidateEngineVersion(in.EngineVersion); err != nil {
			return nil, newRefusal(ReasonEngineVersionInvalid, "%v", err)
		}
	}
	if in.PublishKVEvents {
		if err := ValidateKVEventsLocalDP(in.KVEventsLocalDP); err != nil {
			return nil, err
		}
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
	// An encoder role is exempt from both refusals below. The exemption is on the role rather
	// than on the engine, because the leg an encoder renders is not a cache this package connects:
	// there is no store, no point-to-point KV transfer and no event stream on that Pod. Whether a
	// given engine can render the role stays that engine renderer's answer, which it re-checks
	// against SupportsRole before writing anything.
	if !hasStore && !in.KVTransfer && !in.PublishKVEvents && in.Role != RoleEncode {
		return nil, newRefusal(ReasonConnectionIncomplete,
			"no shared store, point-to-point transfer, or KV event publisher was requested")
	}
	if !hasStore && in.Role != RoleEncode &&
		in.Engine != EngineVLLM && in.Engine != EngineVLLMAscend &&
		(in.Engine != EngineSGLang || (!in.KVTransfer && !in.PublishKVEvents)) {
		return nil, newRefusal(ReasonConnectionIncomplete,
			"engine %q requires a shared store connection", in.Engine)
	}
	// Point-to-point transfer and KV event publishing are rendered by the vLLM family and SGLang,
	// each in its own vocabulary. Any future engine outside that set is refused here rather than
	// silently dropping the capability.
	if in.KVTransfer && in.Engine != EngineVLLM && in.Engine != EngineVLLMAscend && in.Engine != EngineSGLang {
		return nil, newRefusal(ReasonRoleUnsupported,
			"engine %q renders no point-to-point transfer; asking for one would leave a container "+
				"that starts and moves nothing", in.Engine)
	}
	if in.PublishKVEvents && in.Engine != EngineVLLM && in.Engine != EngineVLLMAscend && in.Engine != EngineSGLang {
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
		return renderVLLMWithEncoderCache(in, in.EncoderCache)
	case EngineSGLang:
		return renderSGLang(in)
	default:
		// Unreachable: the Engines check above accepts exactly the values this switch covers.
		return nil, newRefusal(ReasonEngineUnknown, "engine %q has no renderer", in.Engine)
	}
}
