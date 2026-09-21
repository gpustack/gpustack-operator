// This file renders for the routers that are one process configured entirely by argv: the vLLM
// project's own router and the SGLang model gateway. Neither reads a configuration file, so
// everything either is told arrives as a flag.
//
// THE TWO SHARE THIS RENDERER RATHER THAN HAVING ONE EACH, because their flag surfaces are the same
// surface: host, port, Kubernetes service discovery with a selector, a namespace and a target port,
// per-role selectors, and a Prometheus host and port are spelled identically in both. What differs
// is small enough to be data, and it is the argvRouter table below.
//
// Upstream coordinates name their tag and their symbol. The flags are clap fields on one struct per
// project, so a rename shows up as a field rename rather than as a moved line.
package router

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
)

const (
	// The flags both projects spell alike. Each is a CliArgs field at
	// `vllm-project/router@v0.1.15:src/main.rs:99-341` and at
	// `sgl-project/sglang@gateway-v0.3.1:sgl-model-gateway/src/main.rs:134-263`; clap derives the
	// spelling below from the field name in both.
	argvRouterHostArg          = "--host"
	argvRouterPortArg          = "--port"
	argvRouterMetricsHostArg   = "--prometheus-host"
	argvRouterMetricsPortArg   = "--prometheus-port"
	argvRouterDiscoveryArg     = "--service-discovery"
	argvRouterDiscoveryNSArg   = "--service-discovery-namespace"
	argvRouterDiscoveryPortArg = "--service-discovery-port"
	argvRouterSelectorArg      = "--selector"
	argvRouterPrefillSelectArg = "--prefill-selector"
	argvRouterDecodeSelectArg  = "--decode-selector"
	argvRouterRequestTimeout   = "--request-timeout-secs"

	// argvRouterBindAddress is what replaces a loopback default. Only the vLLM router has one -- it
	// binds its API (`vllm-project/router@v0.1.15:src/main.rs:98-99`, `CliArgs::host`) and its
	// metrics server (`:239-240`, `CliArgs::prometheus_host`) to 127.0.0.1, where the gateway binds
	// both to every interface already. It is stated for both anyway, because a value that is
	// correct by accident on one side is a value nobody notices changing.
	argvRouterBindAddress = "0.0.0.0"
)

// argvRouter is what one of these routers differs by. Everything not named here is shared.
type argvRouter struct {
	// Binary is the program this router is, inside the image that carries all three.
	//
	// A BARE NAME RATHER THAN A PATH, so that a deployment naming its own image only has to put the
	// program on PATH instead of at the path this project's image happens to use.
	Binary string

	// DisaggregationArg turns on prefill/decode mode. The two projects spell the same switch
	// differently, which is the whole of the difference in that mode.
	DisaggregationArg string

	// Extra are flags only this router has, and they are rendered whether or not the deployment is
	// split, because each states a default that would otherwise be inherited.
	Extra []string
}

var argvRouters = map[string]argvRouter{
	VLLM: {
		Binary: "vllm-router",
		// `vllm-project/router@v0.1.15:src/main.rs:114-115`, `CliArgs::vllm_pd_disaggregation`.
		DisaggregationArg: "--vllm-pd-disaggregation",
		// The transfer this project runs. Upstream defaults the flag to NIXL (`:339-340`,
		// `CliArgs::kv_connector`, `KvConnector::Nixl`), and a router left on that default ROUTES
		// CORRECTLY AND NEVER TRANSFERS -- the failure arrives through a default rather than
		// through a defect. The gateway has no such flag at all: its transfer backend is the
		// engine's, named on the engine's own command line.
		Extra: []string{"--kv-connector=mooncake"},
	},
	SGLang: {
		Binary: "sgl-model-gateway",
		// `sgl-project/sglang@gateway-v0.3.1:sgl-model-gateway/src/main.rs:191-192`,
		// `CliArgs::pd_disaggregation`.
		DisaggregationArg: "--pd-disaggregation",
		// NOTHING EXTRA, and no model name either. Turning on service discovery turns on this
		// gateway's inference-gateway mode by itself (`:1056-1060`), and that mode routes by the id
		// each worker reports for itself rather than by one the gateway was told. Its --model-path
		// loads a TOKENIZER (`:937`, `maybe_model_path`), which would make the router Pod depend on
		// reaching a model repository at startup for a name it does not route by.
	},
}

func argvRenderer(name string) func(Input) (Output, error) {
	return func(input Input) (Output, error) {
		return renderArgvRouter(name, input)
	}
}

// renderArgvRouter produces the command line for one of the single-process routers.
//
// THE FLAGS IT STATES ARE THE ONES WHOSE DEFAULTS FAIL SILENTLY, each in a different place: a bind
// address left on loopback makes the Service in front of the router resolve to a process nothing
// outside its own network namespace reaches; an absent discovery namespace makes the router watch
// the whole cluster, which the namespaced Role this operator renders does not permit, so it lists
// nothing at all; and an absent target port dials whatever the router assumes rather than the port
// the model servers serve.
func renderArgvRouter(name string, input Input) (Output, error) {
	delta, known := argvRouters[name]
	if !known {
		return Output{}, fmt.Errorf("render %s: no argument surface for this router", name)
	}
	if input.Namespace == "" {
		return Output{}, fmt.Errorf("render %s: no discovery namespace", name)
	}
	if input.DiscoveryPort <= 0 {
		return Output{}, fmt.Errorf("render %s: no discovery port", name)
	}

	args := []string{
		argvRouterHostArg + "=" + argvRouterBindAddress,
		argvRouterPortArg + "=" + strconv.Itoa(int(input.ListenPort)),
		argvRouterMetricsHostArg + "=" + argvRouterBindAddress,
		argvRouterMetricsPortArg + "=" + strconv.Itoa(int(input.MetricsPort)),
	}
	// Absent leaves this router on its own default, which is HALF AN HOUR on both of them against a
	// day under the proxy. Rendering nothing is what that absence means; rendering a value here is
	// what makes a declaration survive a change of router.
	if input.RequestTimeoutSeconds != nil {
		args = append(args,
			argvRouterRequestTimeout+"="+strconv.Itoa(int(*input.RequestTimeoutSeconds)))
	}
	args = append(args, delta.Extra...)
	args = append(args,
		argvRouterDiscoveryArg,
		argvRouterDiscoveryNSArg+"="+input.Namespace,
		argvRouterDiscoveryPortArg+"="+strconv.Itoa(int(input.DiscoveryPort)),
	)

	prefill, decode := roleSelector(input, rolePrefill), roleSelector(input, roleKindDecode)
	if prefill != nil && decode != nil {
		// The disaggregated shape. The vLLM router refuses this mode unless it is given one of
		// three ways to find the two halves, and per-role selectors under service discovery are the
		// one that survives a scale (`vllm-project/router@v0.1.15:src/main.rs:403-415`). The two
		// selectors are a UNION rather than a conjunction: a Pod is taken when it matches either
		// (`src/service_discovery.rs:88-95`).
		args = append(args, delta.DisaggregationArg)
		args = append(args, selectorArgs(argvRouterPrefillSelectArg, prefill)...)
		args = append(args, selectorArgs(argvRouterDecodeSelectArg, decode)...)

		return Output{Command: []string{delta.Binary}, Arguments: args}, nil
	}

	// The undivided shape: one selector naming every Pod that answers.
	deployment := selectorArgs(argvRouterSelectorArg, input.EndpointLabels)
	if deployment == nil {
		return Output{}, fmt.Errorf("render %s: no endpoint labels to discover by", name)
	}

	return Output{Command: []string{delta.Binary}, Arguments: append(args, deployment...)}, nil
}

// selectorArgs renders one label per argument, sorted.
//
// A SELECTOR IS NOT ONE COMMA-JOINED STRING. The flags take a list (`num_args = 0..`) and each
// entry is split on its first "="; an entry carrying none is DROPPED without a word
// (`vllm-project/router@v0.1.15:src/main.rs:345-355`, `CliArgs::parse_selector`). Sorting is what
// makes two renders of one input produce one command line rather than a rolling Deployment.
//
// They are rendered LAST for a reason that is not cosmetic: a list flag consumes every following
// value until the next one starting with a dash, so a flag placed after a selector would be read as
// one of its entries.
func selectorArgs(flag string, labels map[string]string) []string {
	if len(labels) == 0 {
		return nil
	}

	out := make([]string, 0, len(labels)+1)
	out = append(out, flag)
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		out = append(out, key+"="+labels[key])
	}

	return out
}

// rolePrefill is the prefiller's kind as this package receives it, for the same reason
// roleKindDecode is a string: the caller resolves kinds and this package compares what it is given.
const rolePrefill = "prefill"

func roleSelector(input Input, kind string) map[string]string {
	for i := range input.Roles {
		if input.Roles[i].Kind == kind && len(input.Roles[i].Selector) > 0 {
			return input.Roles[i].Selector
		}
	}

	return nil
}
