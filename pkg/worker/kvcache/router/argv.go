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
	"strings"

	"gpustack.ai/gpustack/pkg/utils/json"
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

	// argvRouterEPDConfigArg carries the vLLM router's encoder stage, which the other router has no
	// flag for. Upstream takes it as ONE optional string holding a JSON document (pull request 266,
	// cherry-picked at pack/llm-router/patches/vllm-router/0001-epd-routing.patch,
	// `CliArgs::epd_config`), so the whole stage is one argument rather than a repeatable flag.
	argvRouterEPDConfigArg = "--epd-config"
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
		// AN ENCODER IS CARRIED AS ONE STATIC LIST, and the vLLM router is the one that takes it.
		// Its --epd-config is the whole of the encoder stage upstream knows (pull request 266): a
		// JSON document read once at startup, whose encoder_urls are never refreshed and whose
		// endpoints are never health-checked, and upstream documents dynamic prefill/decode
		// discovery as unsupported beside EPD. That is why the two halves above are selectors and
		// the encoder alone is a plain list of Service URLs. The gateway renders nothing here --
		// its prefiller dials the encoder itself, by --encoder-urls on the engine's own command
		// line.
		//
		// IT IS RENDERED BEFORE THE TWO SELECTORS: both are lists that consume every following
		// token until the next one starting with a dash (see selectorArgs), so the document rides
		// ahead of them, where no flag can be mistaken for a list entry.
		//
		// THE ROUTER STAYS OUT OF GATEWAY MODE. Upstream refuses EPD beside inference-gateway mode
		// outright, and this renderer renders --enable-igw for no input; the statement is kept
		// because it is the invariant the refusal upstream is checked against, not an accident of
		// this flag list.
		if name == VLLM && (declaresRole(input, roleKindEncode) || len(input.EncoderURLs) > 0) {
			config, err := epdConfigArg(name, input)
			if err != nil {
				return Output{}, err
			}
			args = append(args, config)
		}
		args = append(args, selectorArgs(argvRouterPrefillSelectArg, prefill)...)
		args = append(args, selectorArgs(argvRouterDecodeSelectArg, decode)...)

		return Output{Command: []string{delta.Binary}, Arguments: args}, nil
	}

	// The undivided shape: one selector naming every Pod that answers.
	//
	// AN ENCODER IS REFUSED HERE RATHER THAN LEFT OUT OF THE SELECTOR, because this one selector is
	// deployment-wide: it IS the answering set, and the encoder is in it. An encoder answers nothing
	// -- it consumes media and returns embeddings only a prefiller reads -- so a router offered one as
	// a backend routes requests to a process that cannot generate from them, and the failure would
	// arrive as a request that never completes. The split shape above is what pairs an encoder, and it
	// names each half by its own role, which is where an encoder is absent by construction.
	if declaresRole(input, roleKindEncode) {
		return Output{}, fmt.Errorf("render %s: a deployment-wide selector would offer the %s role as "+
			"a backend, so a deployment holding one needs a %s and a %s role beside it",
			name, roleKindEncode, rolePrefill, roleKindDecode)
	}
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

// declaresRole reports whether the deployment declares a role of this kind, whatever that role's
// selector holds.
//
// IT IS SEPARATE FROM roleSelector because the two answer different questions. A selector answers
// "which Pods does this role name", which is what a flag is built from; this answers "is this role
// present at all", which is what a refusal is built from. A role declared with an empty selector is
// present either way, and reading its absence as a reason to render would be exactly the silent
// omission this package is written against.
func declaresRole(input Input, kind string) bool {
	for i := range input.Roles {
		if input.Roles[i].Kind == kind {
			return true
		}
	}

	return false
}

func roleSelector(input Input, kind string) map[string]string {
	for i := range input.Roles {
		if input.Roles[i].Kind == kind && len(input.Roles[i].Selector) > 0 {
			return input.Roles[i].Selector
		}
	}

	return nil
}

// epdConfig is the JSON document --epd-config carries: the encoder endpoints and nothing else.
// Upstream reads the key back with unknown fields refused, so the document is one struct rather
// than a map -- a second spelling of the key would be a startup failure on the router, not a
// render error here.
type epdConfig struct {
	EncoderURLs []string `json:"encoder_urls"`
}

// epdConfigArg renders the --epd-config argument for the vLLM router's encoder stage.
//
// THE CONTRACT IS ALL-OR-NOTHING. A declared Encode role with no endpoints is refused because
// upstream refuses to start on an empty encoder_urls; endpoints without a declared role are
// refused because dropping them would be exactly the silent omission this package is written
// against. Every endpoint must carry its scheme, because upstream validates http:// or https://
// and refuses anything else at startup.
//
// consumer_zmq_addrs is OMITTED rather than written empty. Upstream treats the key as optional,
// and it exists to hand each prefiller's address to an encoder-cache connector that registers
// them one by one -- an address list that does not exist when the two halves are discovered by
// selector -- so writing it would state a claim about a transfer leg nothing configures.
func epdConfigArg(name string, input Input) (string, error) {
	if !declaresRole(input, roleKindEncode) || len(input.EncoderURLs) == 0 {
		return "", fmt.Errorf("render %s: the %s role and its encoder endpoints must be declared together",
			name, roleKindEncode)
	}
	for _, url := range input.EncoderURLs {
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return "", fmt.Errorf("render %s: encoder endpoint %q is not an http(s) URL", name, url)
		}
	}
	encoded, err := json.Marshal(epdConfig{EncoderURLs: input.EncoderURLs})
	if err != nil {
		return "", fmt.Errorf("render %s: encoder stage configuration: %w", name, err)
	}

	return argvRouterEPDConfigArg + "=" + string(encoded), nil
}
