// Package router renders configuration for the upstream routers managed by a ModelDeployment.
package router

import (
	"fmt"
	"slices"

	"sigs.k8s.io/yaml"
)

// The router names this package renders for, which are the API enum's values. They are declared
// here rather than imported because the refusal messages interpolate them, and a package that took
// them from the API would make this renderer depend on the type it exists to stay independent of.
const (
	LLMD   = "llm-d-router"
	VLLM   = "vllm-router"
	SGLang = "sglang-gateway"
)

// Role is one stable class of model-server endpoints visible to a router.
type Role struct {
	Kind         string
	RoleLabelKey string

	// Selector names this role's Pods that answer the API, as label equalities.
	//
	// IT IS A MAP RATHER THAN A SELECTOR STRING because the routers configured by argv match on
	// equality alone and parse their own input: the vLLM router splits each entry on the first "="
	// and DROPS an entry carrying none, with nothing logged
	// (`vllm-project/router@v0.1.15:src/main.rs:345-355`, `CliArgs::parse_selector`, matched at
	// `src/service_discovery.rs:74-82`). Handing it an expression would silently discard whichever
	// terms were not equalities, so a renderer is given what it can express in the first place.
	Selector map[string]string
}

// Metrics names the model-server metrics consumed by a router.
type Metrics struct {
	Engine             string
	Port               int32
	QueuedRequests     string
	RunningRequests    string
	KVCacheUtilization string
}

// KVEvents configures discovery of per-Pod event publishers.
type KVEvents struct {
	Engine     string
	Port       int32
	ReplayPort int32
	Topic      string
}

// Input contains only stable configuration. Live Pod addresses and replica counts do not belong
// here because changing either would restart the router during an ordinary scale operation.
type Input struct {
	Roles             []Role
	Metrics           Metrics
	ModelName         string
	TokenizerEndpoint string
	Namespace         string
	EndpointSelector  string
	KVEvents          KVEvents

	// EndpointLabels is EndpointSelector's equality-only form: the Pods of this deployment that
	// answer the API, whatever their role. The two say the same thing for the routers here, and
	// carrying both is deliberate -- see Role.Selector for why a map rather than a string.
	EndpointLabels map[string]string

	// ListenPort is the port the router serves its own API on, which is what the Service in front
	// of it targets.
	ListenPort int32

	// MetricsPort is the port the router serves Prometheus metrics on.
	MetricsPort int32

	// DiscoveryPort is the port to reach a discovered Pod on, which is the port the model servers
	// serve. A router that inherits its default dials 80 on Pods that do not listen there.
	DiscoveryPort int32

	// RequestTimeoutSeconds is how long to wait for a reply. Absent leaves each router on its own
	// default, which is NOT one value across them -- see the API field, which says so.
	RequestTimeoutSeconds *int32

	// DisaggregationThreshold is how many non-cached prompt tokens make a request worth splitting.
	// Absent leaves the renderer's own value; an explicit zero disables splitting, which is what
	// the upstream decider does with it.
	DisaggregationThreshold *int32
}

// roleKindDecode is the decoder's kind as this package receives it.
//
// IT IS A STRING BECAUSE THIS PACKAGE TAKES STRINGS: its input carries kinds already resolved by the
// caller, which keeps the router renderer independent of the API types. The cost is that the value
// has to agree with the API's enum by convention rather than by the compiler, so the two places that
// compare against it do so through this one name - a second spelling would be the thing that drifts.
const roleKindDecode = "decode"

// Output is a rendered router configuration. A router is configured EITHER by a document it reads at
// startup OR entirely by its own arguments, so exactly one half is filled and Render refuses every
// other shape.
//
// Both halves live in one type, rather than each router having its own return type, because the
// caller picks the router at run time from a field and would otherwise need a type switch to learn
// what it just rendered.
type Output struct {
	// Command is the program to run, and every router has one because one image holds all three.
	//
	// IT IS RENDERED RATHER THAN LEFT TO THE IMAGE. The image carries no entrypoint on purpose:
	// naming one of the three there would make the other two reachable only by overriding it. So
	// the binary is named here, beside the arguments it takes, and a caller that writes the
	// arguments without this one hands the container a flag where it expects a program -- which
	// fails as "executable file not found", far from the field that caused it.
	Command []string

	// Document is the configuration file the router reads at startup. How it reaches the process is
	// the caller's decision, not this package's.
	Document string

	// Arguments are appended to the router's own command line, for a router that reads no
	// configuration file at all.
	Arguments []string
}

// Render returns configuration understood by the selected upstream router.
//
// THE EXACTLY-ONE RULE IS ENFORCED HERE RATHER THAN IN EACH RENDERER, because the shape it catches
// is the shape a newly added renderer arrives in: a function that returns its zero value compiles,
// renders nothing, and would otherwise start a router with no configuration and no error reported
// anywhere. Filling both halves is refused for the same reason it is never correct - a router reads
// one of them and silently ignores the other, so the ignored half is configuration a user wrote and
// nothing applies.
func Render(name string, input Input) (Output, error) {
	render, ok := renderers[name]
	if !ok {
		return Output{}, fmt.Errorf("unsupported router %q", name)
	}

	output, err := render(input)
	if err != nil {
		return Output{}, err
	}
	switch {
	case len(output.Command) == 0:
		// Checked separately from the pair below because it is not part of that alternative: every
		// router needs a command, whichever half it fills. A renderer that omits it produces a
		// container which starts, treats its first flag as the program name, and fails with a
		// message naming that flag rather than the omission.
		return Output{}, fmt.Errorf("router %q rendered no command", name)
	case output.Document == "" && len(output.Arguments) == 0:
		return Output{}, fmt.Errorf(
			"router %q rendered neither a configuration document nor arguments", name)
	case output.Document != "" && len(output.Arguments) != 0:
		return Output{}, fmt.Errorf(
			"router %q rendered both a configuration document and arguments", name)
	}

	return output, nil
}

var renderers = map[string]func(Input) (Output, error){
	LLMD:   renderLLMD,
	VLLM:   argvRenderer(VLLM),
	SGLang: argvRenderer(SGLang),
}

func renderLLMD(input Input) (Output, error) {
	kinds := make(map[string]string, len(input.Roles))
	for _, role := range input.Roles {
		kinds[role.Kind] = role.RoleLabelKey
	}

	orderedKinds := make([]string, 0, len(kinds))
	for kind := range kinds {
		orderedKinds = append(orderedKinds, kind)
	}
	slices.Sort(orderedKinds)

	plugins := make([]any, 0, len(orderedKinds)+7)
	profiles := make([]any, 0, len(orderedKinds))
	for _, kind := range orderedKinds {
		filter := kind + "-pods"
		plugins = append(plugins, map[string]any{
			"name":       filter,
			"type":       "label-selector-filter",
			"parameters": map[string]any{"matchLabels": map[string]string{kinds[kind]: kind}},
		})
		profilePlugins := []any{
			map[string]any{"pluginRef": filter},
			map[string]any{"pluginRef": "queue-scorer", "weight": 2},
			map[string]any{"pluginRef": "kv-cache-utilization-scorer", "weight": 2},
			map[string]any{"pluginRef": "max-score-picker"},
		}
		if kind != roleKindDecode {
			profilePlugins = append(profilePlugins,
				map[string]any{"pluginRef": "prefix-cache-scorer", "weight": 3})
		}
		profiles = append(profiles, map[string]any{
			"name":    kind,
			"plugins": profilePlugins,
		})
	}

	metricsSource := map[string]any{"type": "metrics-data-source"}
	if input.Metrics.Port > 0 {
		metricsSource["parameters"] = map[string]any{"port": input.Metrics.Port}
	}
	prefixProducer := "approx-prefix-cache-producer"
	if input.KVEvents.Port > 0 {
		prefixProducer = "precise-prefix-cache-producer"
		// Excludes decoders from the set of Pods the producer subscribes to. The exclusion is keyed
		// on the decode kind's own label, so it exists only when a decode role is among the inputs -
		// keying it on whichever kind sorts first would pin it to an arbitrary role. Appended rather
		// than assigned, and guarded, because an empty selector would otherwise render a leading
		// comma - which Kubernetes rejects as a label selector, and which no current caller
		// produces. The guard is here because this function is a library and its callers are not
		// its contract.
		publisherSelector := input.EndpointSelector
		if key, ok := kinds[roleKindDecode]; ok {
			exclusion := key + "!=" + roleKindDecode
			if publisherSelector == "" {
				publisherSelector = exclusion
			} else {
				publisherSelector += "," + exclusion
			}
		}
		plugins = append(plugins,
			map[string]any{
				"type": "token-producer",
				"parameters": map[string]any{
					"modelName": input.ModelName,
					"vllm":      map[string]any{"url": input.TokenizerEndpoint},
				},
			},
			map[string]any{"type": "endpoint-notification-source"},
			map[string]any{
				"type": prefixProducer,
				"parameters": map[string]any{
					"kvEventsConfig": map[string]any{
						"topicFilter":  input.KVEvents.Topic,
						"concurrency":  4,
						"engineType":   input.KVEvents.Engine,
						"discoverPods": true,
						"podDiscoveryConfig": map[string]any{
							"podLabelSelector": publisherSelector,
							"podNamespace":     input.Namespace,
							"socketPort":       input.KVEvents.Port,
							"replaySocketPort": input.KVEvents.ReplayPort,
						},
					},
				},
			})
	} else {
		plugins = append(plugins, map[string]any{"type": prefixProducer})
	}
	prefixScorer := map[string]any{"type": "prefix-cache-scorer"}
	if prefixProducer == "precise-prefix-cache-producer" {
		prefixScorer["parameters"] = map[string]any{"prefixMatchInfoProducerName": prefixProducer}
	}
	plugins = append(plugins,
		map[string]any{"type": "queue-scorer"},
		map[string]any{"type": "kv-cache-utilization-scorer"},
		map[string]any{"type": "max-score-picker"},
		prefixScorer,
		metricsSource,
	)
	metricsExtractor := map[string]any{"type": "core-metrics-extractor"}
	if input.Metrics.Engine != "" {
		metricsExtractor["parameters"] = map[string]any{
			"engineConfigs": []any{map[string]any{
				"name":                input.Metrics.Engine,
				"queuedRequestsSpec":  input.Metrics.QueuedRequests,
				"runningRequestsSpec": input.Metrics.RunningRequests,
				"kvUsageSpec":         input.Metrics.KVCacheUtilization,
			}},
		}
	}
	plugins = append(plugins, metricsExtractor)

	if _, prefill := kinds["prefill"]; prefill {
		if _, decode := kinds["decode"]; decode {
			plugins = append(plugins,
				map[string]any{
					"type": "prefix-based-pd-decider",
					"parameters": map[string]any{
						"promptTokens": 0,
						// Zero is a value here rather than an absence: the decider returns "do not
						// disaggregate" on a zero threshold before reading anything else, so the
						// pointer is what tells an explicit zero from an unset field.
						"nonCachedTokens": llmdNonCachedTokens(input.DisaggregationThreshold),
					},
				},
				map[string]any{
					"type": "disagg-profile-handler",
					"parameters": map[string]any{
						"profiles": map[string]string{"prefill": "prefill", "decode": "decode"},
						"deciders": map[string]string{"prefill": "prefix-based-pd-decider"},
					},
				},
			)
		}
	}

	config := map[string]any{
		"apiVersion":         "llm-d.ai/v1alpha1",
		"kind":               "EndpointPickerConfig",
		"plugins":            plugins,
		"schedulingProfiles": profiles,
	}
	if prefixProducer == "precise-prefix-cache-producer" {
		config["dataLayer"] = map[string]any{"sources": []any{
			map[string]any{
				"pluginRef":  "metrics-data-source",
				"extractors": []any{map[string]any{"pluginRef": "core-metrics-extractor"}},
			},
			map[string]any{
				"pluginRef":  "endpoint-notification-source",
				"extractors": []any{map[string]any{"pluginRef": prefixProducer}},
			},
		}}
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		return Output{}, fmt.Errorf("render llm-d configuration: %w", err)
	}

	// The document half alone: this router takes its plugins, profiles and data layer from a file,
	// and the arguments its process needs are the ones the object renderer writes rather than
	// anything derived from this input.
	return Output{Command: []string{"epp"}, Document: string(encoded)}, nil
}

// llmdDefaultNonCachedTokens is what the picker's decider keeps when the field is unset. It is the
// value this renderer has always written, so an absent field renders what it rendered before.
const llmdDefaultNonCachedTokens int32 = 8

func llmdNonCachedTokens(threshold *int32) int32 {
	if threshold == nil {
		return llmdDefaultNonCachedTokens
	}

	return *threshold
}
