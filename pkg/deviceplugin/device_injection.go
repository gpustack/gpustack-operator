package deviceplugin

import (
	"fmt"
	"strings"
	"sync"

	core "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

// An injection strategy decides which channel carries the granted accelerators to the container. Two
// exist, and which of them works is a property of the node rather than of this operator:
//
//   - A manufacturer's *_VISIBLE_DEVICES variable is read by that manufacturer's container runtime, so
//     it reaches the container only when that runtime is in the Pod's path. Under a generic OCI
//     handler it is inert: the container starts with no accelerator and no error, measured.
//   - A CDI request on this response is read by the container engine, which resolves it against the
//     specifications on the node and injects the device nodes AND the driver libraries. It works under
//     a generic handler, and a name the engine cannot resolve fails the container creation with that
//     name in the message rather than widening the grant, measured.
type InjectionStrategy string

const (
	// InjectionEnvvar is the default: the manufacturer's visible-devices variable, which is what this
	// operator emitted before a second channel existed.
	InjectionEnvvar InjectionStrategy = "envvar"
	// InjectionCDI requests the granted accelerators as CDI devices.
	InjectionCDI InjectionStrategy = "cdi-annotations"
	// InjectionAuto reads the node and picks one, refusing to guess. Opt-in: one node can host a
	// generic handler, a vendor handler and a CDI-configured handler at once, so no node-global answer
	// is right for every Pod.
	InjectionAuto InjectionStrategy = "auto"
)

// InjectionConfig is what a manufacturer's allocator supplies to get the whole resolution above. Every
// field is that manufacturer's own vocabulary; nothing else about it is needed.
type InjectionConfig struct {
	// Manufacturer names the strategy's environment variable and the CDI request's annotation suffix.
	Manufacturer string
	// CDIKind is the vendor and class the manufacturer's own generator publishes accelerators under.
	// A subdevice materialized at allocation time carries a kind of its own and no pre-generated
	// specification can name it, so a responder serving one never consults this resolver.
	CDIKind string
	// VisibleDevicesEnv is the variable the manufacturer's container runtime reads a device list from.
	VisibleDevicesEnv string
}

// InjectionStrategyEnv names the strategy for one manufacturer, set on its device-manager DaemonSet
// through the chart's deviceManager.env like every other per-manufacturer override.
func InjectionStrategyEnv(manufacturer string) string {
	return "GPUSTACK_" + strings.ToUpper(manufacturer) + "_DEVICE_INJECTION_STRATEGY"
}

// ParseInjectionStrategy reads a configured strategy. An unrecognized value is an error rather than a
// silent fall back to the default: it is a deployment mistake, and the spellings differ by which
// channel carries the grant.
func ParseInjectionStrategy(value string) (InjectionStrategy, error) {
	switch s := InjectionStrategy(strings.TrimSpace(value)); s {
	case "":
		return InjectionEnvvar, nil
	case InjectionEnvvar, InjectionCDI, InjectionAuto:
		return s, nil
	default:
		return "", fmt.Errorf("unrecognized %q: want one of %q, %q, %q",
			value, InjectionEnvvar, InjectionCDI, InjectionAuto)
	}
}

// InjectionResolver answers, per container, which channel carries the grant.
//
// The node facts are read once and cached, because reading them per Allocate would put file I/O on the
// allocation path. The engine's configuration is a node's static state; the loaded CDI specifications
// are regenerated as the node's hardware changes, so a node whose specifications appear after this
// DaemonSet started keeps the environment-variable channel until it is restarted. That direction is
// safe — it declines to switch rather than switching wrongly — and restarting the DaemonSet is the
// same remedy the detector's own startup reads already need.
type InjectionResolver struct {
	cfg      InjectionConfig
	strategy InjectionStrategy

	once   sync.Once
	specs  CDISpecs
	engine ContainerEngineFacts

	// logged records which resolutions have been reported, keyed by the strategy settled on. Under
	// auto the answer is per container — it reads the Pod's own runtimeClassName and the granted
	// accelerators — so reporting only the first would describe one Pod and hide every later one that
	// took the other channel, on the one decision whose whole subject is which channel carried the
	// grant. Keyed rather than unbounded: the answer ranges over two values.
	logged sync.Map
}

// NewInjectionResolver reads the manufacturer's configured strategy. A misconfigured value fails here,
// at startup, rather than on the first allocation. One resolver serves every responder of a
// manufacturer, so the node is read once and each resolved channel is logged once.
func NewInjectionResolver(cfg InjectionConfig) (*InjectionResolver, error) {
	env := InjectionStrategyEnv(cfg.Manufacturer)
	strategy, err := ParseInjectionStrategy(osx.Getenv(env))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", env, err)
	}

	return &InjectionResolver{cfg: cfg, strategy: strategy}, nil
}

// DefaultInjectionResolver is what an allocator falls back to when the configured strategy could not
// be read: the default channel, and no node reads at all.
func DefaultInjectionResolver(cfg InjectionConfig) *InjectionResolver {
	return &InjectionResolver{cfg: cfg, strategy: InjectionEnvvar}
}

// Apply writes the granted accelerators into the response through the resolved channel.
//
// Exactly one channel, never both: two live injection paths for one container is how a container ends
// up holding hardware nobody granted it.
func (r *InjectionResolver) Apply(
	logger klog.Logger,
	resp *ContainerAllocateResponse,
	pod *core.Pod,
	ids []string,
) error {
	strategy, evidence := r.resolve(pod, ids)

	// Once per answer rather than once per allocation: a node that always resolves the same way logs
	// one line for its lifetime, and a node where auto sends different Pods down different channels
	// logs each of them.
	if _, seen := r.logged.LoadOrStore(strategy, struct{}{}); !seen {
		logger.Info("resolved the device-injection strategy", "strategy", strategy, "evidence", evidence)
	}

	if strategy == InjectionEnvvar {
		if resp.Envs == nil {
			resp.Envs = make(map[string]string, 1)
		}
		resp.Envs[r.cfg.VisibleDevicesEnv] = strings.Join(ids, ",")

		return nil
	}

	names := CDIDeviceNames(r.cfg.CDIKind, ids)

	// An explicitly configured CDI strategy still validates, because a specification generated with a
	// non-default naming strategy carries no accelerator-id-named device, and failing here names the
	// accelerator where the container engine would name only the CDI string.
	//
	// A name that was found ends it: that is a positive fact, and no file this could not read is able
	// to unfind it.
	r.readNode()
	switch missing := r.specs.Missing(names); {
	case len(missing) == 0:
	// Nothing can be seen at all, or what is missing might be in a file that could not be read: an
	// empty or partial view is absence of evidence, not evidence of absence, and refusing on it would
	// break a working node. The engine is the backstop — an unresolvable name fails the container
	// creation with that name in the message, measured — so the request is handed over and the gap is
	// logged.
	case len(r.specs.Names) == 0 || r.specs.Unreadable:
		logger.Info("the requested devices could not be validated against a CDI specification before "+
			"the container engine sees them",
			"directories", r.specs.Dirs, "devices", missing, "someSpecificationUnreadable", r.specs.Unreadable)
	default:
		return fmt.Errorf("no CDI specification under %v names %s; the accelerator cannot be "+
			"requested as a CDI device", r.specs.Dirs, strings.Join(missing, ", "))
	}

	SetCDIRequest(resp, "gpustack-"+r.cfg.Manufacturer, names)

	return nil
}

// resolve returns the channel this container's accelerators are injected through, and the evidence the
// choice rests on. It never returns an error: an unreadable node is a reason to keep today's behavior,
// not a reason to fail an allocation.
func (r *InjectionResolver) resolve(pod *core.Pod, ids []string) (InjectionStrategy, string) {
	switch r.strategy {
	case "", InjectionEnvvar:
		return InjectionEnvvar, "configured"
	case InjectionCDI:
		return r.strategy, "configured"
	}

	r.readNode()

	// A Pod that named a runtime handler asked for that handler's injection, and this resolver cannot
	// read the handler's configuration to know what it will do. Leave it to the handler.
	if pod != nil && pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName != "" {
		return InjectionEnvvar, fmt.Sprintf("the pod names runtimeClassName %q, whose handler owns injection",
			*pod.Spec.RuntimeClassName)
	}
	if !r.engine.Known {
		return InjectionEnvvar, fmt.Sprintf("the container engine configuration could not be read at %q, "+
			"which %s names", r.engine.Path, ContainerdConfigDirEnv)
	}
	// The Pod named no handler, so it runs under the engine's default. If that default is a vendor
	// runtime, the visible-devices variable reaches it and a CDI request would be a second injection
	// path for one container.
	if r.engine.DefaultHandlerIsVendor {
		return InjectionEnvvar, fmt.Sprintf("the engine's default runtime handler %q is a vendor runtime",
			r.engine.DefaultHandler)
	}
	if !r.engine.ResolvesCDI {
		return InjectionEnvvar, "the container engine does not resolve CDI requests"
	}
	// Every granted accelerator, not merely one: a request naming a device the specifications do not
	// carry fails the whole container, so a partial match is no better than none.
	//
	// Asked before the unreadable check below, not after it. A name that was found is the positive fact
	// auto needs, and no file this could not read is able to unfind it — while these directories are
	// shared with every other manufacturer's generator, so a malformed specification describing hardware
	// this operator has never heard of would otherwise be enough to switch a working node back.
	missing := r.specs.Missing(CDIDeviceNames(r.cfg.CDIKind, ids))
	if len(missing) != 0 {
		// A specification that could not be parsed is not a specification that names nothing: it is the
		// one place the missing name could still have been, so its absence is unestablished rather than
		// established, and auto only moves a node off today's behavior on a positive fact.
		if r.specs.Unreadable {
			return InjectionEnvvar, fmt.Sprintf("a CDI specification under %v could not be read, so whether "+
				"the node names %s cannot be established", r.specs.Dirs, strings.Join(missing, ", "))
		}

		return InjectionEnvvar, fmt.Sprintf("no loaded CDI specification names %s", strings.Join(missing, ", "))
	}

	return InjectionCDI, fmt.Sprintf("the pod names no runtimeClassName, the engine's default handler %q "+
		"resolves CDI per %q, and the specifications under %v name every granted accelerator",
		r.engine.DefaultHandler, r.engine.Path, r.specs.Dirs)
}

// readNode reads everything auto decides on, once for the resolver's lifetime.
func (r *InjectionResolver) readNode() {
	r.once.Do(func() {
		r.specs = LoadCDISpecs()
		r.engine = ReadContainerEngineFacts()
	})
}
