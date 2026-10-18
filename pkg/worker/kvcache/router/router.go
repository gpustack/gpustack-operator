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

	// CacheInfo names an info-style gauge whose block_size and num_gpu_blocks labels carry the KV
	// cache's capacity. CacheBlockSize and CacheNumBlocks name gauges carrying the same two values
	// directly, for an engine that exports them that way. An engine fills one form or neither.
	CacheInfo      string
	CacheBlockSize string
	CacheNumBlocks string
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

	// EligibleLabelKey names the label a stage filter must match when that role's eligibility
	// requirement is carried per role rather than by the deployment-wide selector. A non-empty key
	// adds the term to the stage filter of every role whose Selector already carries it, which is
	// the set of roles the gate can decide; empty leaves every stage filter as it renders today.
	//
	// IT IS NAMED BY THE CALLER rather than declared here because the label belongs to the
	// operator's own vocabulary, and a second spelling of it in this package is the thing that
	// would drift.
	EligibleLabelKey string

	// EncoderURLs are the HTTP(S) endpoints of the deployment's encoders, as the router that takes
	// a static list of them requires. Each entry is the encoder role's per-role Service URL rather
	// than a replica address: the Service fronts every replica, so a scale changes nothing here
	// and does not restart the router. Nil on a deployment with no Encode role.
	EncoderURLs []string

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

// roleKindEncode is the encoder's kind as this package receives it. It joins roleKindDecode for the
// reason stated on that one: this package compares strings against a convention the API's enum
// defines, so both kinds are named here rather than imported.
const roleKindEncode = "encode"

// llmdMultimodalDecider is the plugin that makes the encoder stage conditional. It activates on a
// request carrying image, audio or video content and declines a text-only one, which is the whole of
// the no-media path: the decode stage has already picked its Pod by then, so declining sends the
// request straight to the prefiller and the decoder.
// (llm-d/llm-d-router@v0.11.0 `pkg/epp/framework/plugins/scheduling/profilehandler/disagg/
// always_disagg_mm_decider.go:29`, and the E/P/D configuration in `docs/disaggregation.md`).
const llmdMultimodalDecider = "always-disagg-multimodal-decider"

// llmdAlwaysDisaggPDDecider splits every request between the prefiller and the decoder. It takes no
// parameters
// (llm-d/llm-d-router@v0.11.0 `pkg/epp/framework/plugins/scheduling/profilehandler/disagg/
// always_disagg_pd_decider.go:28`, registered at `cmd/epp/runner/runner.go:612`).
//
// SGLang NEEDS IT BECAUSE ITS DECODER CANNOT PREFILL: a decode-mode server rejects any request that
// carries no bootstrap room id with HTTP 400, so a decider that skips the prefiller on a short or
// cached prompt, or on a request it could not tokenize, sends the request to a server that cannot
// answer it. vLLM keeps the prefix-based decider because its decoder loads the full model and can
// prefill locally.
const llmdAlwaysDisaggPDDecider = "always-disagg-pd-decider"

// llmdEngineSGLang is the engine name this package receives for SGLang.
const llmdEngineSGLang = "sglang"

// llmdStageFilterType is the picker plugin that filters endpoints by label. Named here so the
// renderer and the reader of its own output agree on the spelling.
const llmdStageFilterType = "label-selector-filter"

// DocumentStageFilterDemands reports whether the rendered picker document holds a label-selector
// filter whose matchLabels require one label to equal one value.
//
// IT IS ASKED OF THE DOCUMENT RATHER THAN OF ITS TEXT because the same string is also where a
// user-supplied model name is rendered. A document that merely CONTAINS the label's key is not a
// requirement, and reading it as one would activate a gate nobody decided. A stage filter's own
// matchLabels is the only place the document states a requirement about a Pod, so it is the only
// place this looks.
//
// A document that does not parse, and one that declares no filter, demand nothing.
func DocumentStageFilterDemands(document, labelKey, value string) bool {
	var parsed struct {
		Plugins []struct {
			Type       string `json:"type"`
			Parameters struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"parameters"`
		} `json:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(document), &parsed); err != nil {
		return false
	}
	for _, plugin := range parsed.Plugins {
		if plugin.Type != llmdStageFilterType {
			continue
		}
		if plugin.Parameters.MatchLabels[labelKey] == value {
			return true
		}
	}

	return false
}

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
	selectors := make(map[string]map[string]string, len(input.Roles))
	for _, role := range input.Roles {
		kinds[role.Kind] = role.RoleLabelKey
		selectors[role.Kind] = role.Selector
	}

	// SGLang's prefiller dials the encoder itself through its encoder URLs, so the router sees the
	// encoder and the prefiller as one stage: encode plus prefill, then decode. The encoder is
	// dropped here rather than rendered as a stage the decoder's sidecar has no connector for.
	sglang := input.Metrics.Engine == llmdEngineSGLang
	if sglang {
		delete(kinds, roleKindEncode)
	}

	orderedKinds := make([]string, 0, len(kinds))
	for kind := range kinds {
		orderedKinds = append(orderedKinds, kind)
	}
	slices.Sort(orderedKinds)

	// Whether this deployment has an encoder stage at all. Hoisted because two decisions below are
	// keyed on it, and a shadowing redeclaration inside either of them would leave the two reading
	// the same name while only one of them could see the value they share.
	_, encode := kinds[roleKindEncode]

	plugins := make([]any, 0, len(orderedKinds)+7)
	profiles := make([]any, 0, len(orderedKinds))
	for _, kind := range orderedKinds {
		filter := kind + "-pods"
		matchLabels := map[string]string{kinds[kind]: kind}
		// A role the gate can decide carries its eligibility requirement HERE rather than in the
		// deployment-wide selector, which is one AND of every term and so cannot say "required of
		// the encoder and not of the prefiller". A role that can never activate never carries the
		// label at all, so a term demanded of it would select nothing; its Selector has none, its
		// stage filter is left alone, and its Pods stay selectable.
		if value, ok := selectors[kind][input.EligibleLabelKey]; ok {
			matchLabels[input.EligibleLabelKey] = value
		}
		plugins = append(plugins, map[string]any{
			"name":       filter,
			"type":       llmdStageFilterType,
			"parameters": map[string]any{"matchLabels": matchLabels},
		})
		profilePlugins := []any{map[string]any{"pluginRef": filter}}
		if kind == roleKindEncode {
			// AN ENCODER IS SCORED ON NOTHING, and the shape is upstream's own: the E/P/D
			// configuration in llm-d/llm-d-router@v0.11.0 `docs/disaggregation.md` gives the
			// encode profile the role filter and nothing else.
			//
			// It has to. An encoder keeps no KV cache, so the queue and KV-cache scorers read
			// gauges it never exports and rank every candidate identically; the prefix scorer
			// asks about token overlap, which does not exist before a request has tokens. A
			// scorer that cannot separate candidates is not a weak preference, it is a cost
			// paid on every media request.
			//
			// THE PICKER IS STILL THERE. Upstream completes any profile naming none at load time
			// by appending its max-score-picker (`pkg/epp/config/loader/defaults.go:197-216`),
			// and the profile runner dereferences that picker unconditionally
			// (`pkg/epp/scheduling/scheduler_profile.go:351`), so a profile that named none
			// would fail rather than run unscored. Naming it here would say nothing the loader
			// does not already say, and would put a second answer to the same question in the
			// document this operator hashes into its router Pod.
			profiles = append(profiles, map[string]any{
				"name":    kind,
				"plugins": profilePlugins,
			})
			continue
		}
		profilePlugins = append(profilePlugins,
			map[string]any{"pluginRef": "queue-scorer", "weight": 2},
			map[string]any{"pluginRef": "kv-cache-utilization-scorer", "weight": 2},
			map[string]any{"pluginRef": "max-score-picker"})
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
		// Excludes every role that keeps no KV cache from the set of Pods the producer
		// subscribes to. The exclusion is keyed on each kind's own label, so a term exists only
		// for a kind among the inputs -- keying it on whichever kind sorts first would pin it to
		// an arbitrary role. Appended rather than assigned, and guarded, because an empty
		// selector would otherwise render a leading comma - which Kubernetes rejects as a label
		// selector, and which no current caller produces. The guard is here because this
		// function is a library and its callers are not its contract.
		//
		// DECODE FIRST, THEN ENCODE, and the order is rendered. The selector is a ConfigMap value
		// whose hash is stamped on the router Pod, so reordering the terms would roll every
		// prefill/decode router in the cluster for a change nothing reads.
		//
		// AN ENCODER IS EXCLUDED FOR THE SAME REASON A DECODER IS, and it is worth stating
		// because the two look different from outside. A decoder consumes KV and produces none;
		// an encoder does not hold KV at all. Either way it publishes no events, and leaving one
		// in the set is a subscriber dialing a socket the engine never opens.
		publisherSelector := input.EndpointSelector
		for _, kind := range []string{roleKindDecode, roleKindEncode} {
			key, ok := kinds[kind]
			if !ok {
				continue
			}
			exclusion := key + "!=" + kind
			if publisherSelector == "" {
				publisherSelector = exclusion
			} else {
				publisherSelector += "," + exclusion
			}
		}
		// AN ENCODER STAGE GETS THE BUILT-IN ESTIMATOR, AND EVERY OTHER DEPLOYMENT GETS THE HTTP
		// RENDERER. Naming the vLLM URL here selects llm-d-router's vllmHTTPRenderer, which POSTs to
		// <url>/v1/chat/completions/render -- an endpoint vLLM 0.30.0 serves only under
		// --enable-scale-out, so on a default server the call answers 404 and no request is routed
		// at all. A bare token-producer instead selects the built-in estimator, which reads PNG and
		// JPEG dimensions out of a data: URL with no HTTP call.
		//
		// The two are also not interchangeable on an encoder: the request is forwarded to the
		// encoder with its media still attached, and the multimodal decider reads the estimator's
		// MultiModalFeatures to decide the stage exists at all. The HTTP renderer would have to ask
		// the prefiller to render, which is the wrong server for the question.
		tokenProducer := map[string]any{"type": "token-producer"}
		if !encode {
			tokenProducer["parameters"] = map[string]any{
				"modelName": input.ModelName,
				"vllm":      map[string]any{"url": input.TokenizerEndpoint},
			}
		}
		plugins = append(plugins,
			tokenProducer,
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
		engineConfig := map[string]any{
			"name":                input.Metrics.Engine,
			"queuedRequestsSpec":  input.Metrics.QueuedRequests,
			"runningRequestsSpec": input.Metrics.RunningRequests,
			"kvUsageSpec":         input.Metrics.KVCacheUtilization,
		}
		// Naming an engine here replaces the extractor's built-in entry for it rather than
		// overlaying it
		// (`llm-d/llm-d-router@v0.10.0:pkg/epp/framework/plugins/datalayer/extractor/metrics/factories.go:185-194`),
		// so a field left out is one the extractor never reads. The cache capacity is rendered
		// beside the approximate producer alone because it is the one plugin that reads it, sizing
		// each server's prefix index by it; beside the precise producer it would roll the router
		// for a value nothing reads.
		if prefixProducer == "approx-prefix-cache-producer" {
			for key, spec := range map[string]string{
				"cacheInfoSpec":      input.Metrics.CacheInfo,
				"cacheBlockSizeSpec": input.Metrics.CacheBlockSize,
				"cacheNumBlocksSpec": input.Metrics.CacheNumBlocks,
			} {
				if spec != "" {
					engineConfig[key] = spec
				}
			}
		}
		parameters := map[string]any{"engineConfigs": []any{engineConfig}}
		// The extractor reads a Pod by the engine its engine-type label names, and a Pod without
		// that label as the default engine; no model-server Pod carries the label, so every Pod is
		// read as the default. Left to upstream's default, an SGLang server is read by vLLM's
		// metric names, which it does not export, so no endpoint ever has fresh metrics and the
		// queue and KV-cache scorers score every endpoint alike.
		if input.Metrics.Engine != llmdDefaultMetricsEngine {
			parameters["defaultEngine"] = input.Metrics.Engine
		}
		metricsExtractor["parameters"] = parameters
	}
	plugins = append(plugins, metricsExtractor)

	if _, prefill := kinds["prefill"]; prefill {
		if _, decode := kinds["decode"]; decode {
			// The encoder stage is wired beside the prefiller's because admission requires an
			// encoder to declare both halves: a deployment reaching this renderer with an
			// encoder and no prefiller is refused upstream and never arrives here.
			stageProfiles := map[string]string{"prefill": "prefill", "decode": "decode"}
			stageDeciders := map[string]string{"prefill": "prefix-based-pd-decider"}
			// An explicit zero threshold still disables splitting, which only the prefix-based decider
			// honors, so the always-split decider is for a split that is not disabled.
			alwaysSplit := sglang &&
				(input.DisaggregationThreshold == nil || *input.DisaggregationThreshold > 0)
			if alwaysSplit {
				stageDeciders["prefill"] = llmdAlwaysDisaggPDDecider
			}
			if encode {
				// Declared before the prefiller's decider, which is the order upstream's E/P/D
				// configuration lists them in (llm-d/llm-d-router@v0.11.0
				// `docs/disaggregation.md`, E/P/D Configuration).
				plugins = append(plugins, map[string]any{"type": llmdMultimodalDecider})
				stageProfiles["encode"] = "encode"
				stageDeciders["encode"] = llmdMultimodalDecider
			}
			pdDeciderParams := map[string]any{
				"promptTokens": 0,
				// Zero is a value here rather than an absence: the decider returns "do not
				// disaggregate" on a zero threshold before reading anything else, so the
				// pointer is what tells an explicit zero from an unset field.
				"nonCachedTokens": llmdNonCachedTokens(input.DisaggregationThreshold),
			}
			// THE PREFILL DECIDER MUST BE TOLD WHICH PRODUCER FEEDS IT, and only on an encoder
			// deployment. With the precise producer active, the decider reads its token counts from
			// the producer named in prefixMatchInfoProducerName; omitted, it falls back to the
			// approximate producer, which this document does not define, so the router creates an
			// unconfigured one at load and then logs
			// "failed to read prefix plugin state: not found" for every request.
			//
			// The name is written rather than relied upon, because the fallback is a silent second
			// answer to a question this document has already answered.
			if encode && prefixProducer == "precise-prefix-cache-producer" {
				pdDeciderParams["prefixMatchInfoProducerName"] = prefixProducer
			}
			pdDecider := map[string]any{
				"type":       "prefix-based-pd-decider",
				"parameters": pdDeciderParams,
			}
			if alwaysSplit {
				pdDecider = map[string]any{"type": llmdAlwaysDisaggPDDecider}
			}
			plugins = append(plugins,
				pdDecider,
				map[string]any{
					"type": "disagg-profile-handler",
					"parameters": map[string]any{
						"profiles": stageProfiles,
						"deciders": stageDeciders,
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

// llmdDefaultMetricsEngine is the engine the picker's metrics extractor reads a Pod without an
// engine-type label as when its configuration names none
// (`llm-d/llm-d-router@v0.10.0:pkg/epp/framework/plugins/datalayer/extractor/metrics/factories.go:148-149`,
// applied at `:178-180`, the label read at `extractor.go:201-217`).
//
// IT IS NOT WRITTEN FOR THE ENGINE IT ALREADY IS: the rendered document is hashed into the router
// Pod, so spelling out a value upstream already holds would roll every existing vLLM router for a
// change nothing reads. The cost is that this value has to agree with upstream's by convention, so a
// version bump of the picker re-reads the reference above.
const llmdDefaultMetricsEngine = "vllm"

// llmdDefaultNonCachedTokens is what the picker's decider keeps when the field is unset. It is the
// value this renderer has always written, so an absent field renders what it rendered before.
const llmdDefaultNonCachedTokens int32 = 8

func llmdNonCachedTokens(threshold *int32) int32 {
	if threshold == nil {
		return llmdDefaultNonCachedTokens
	}

	return *threshold
}
