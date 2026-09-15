// Package router renders configuration for the upstream routers managed by a ModelDeployment.
package router

import (
	"fmt"
	"slices"

	"sigs.k8s.io/yaml"
)

const LLMD = "llm-d"

// Role is one stable class of model-server endpoints visible to a router.
type Role struct {
	Kind         string
	RoleLabelKey string
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
}

// Render returns configuration understood by the selected upstream router.
// roleKindDecode is the decoder's kind as this package receives it.
//
// IT IS A STRING BECAUSE THIS PACKAGE TAKES STRINGS: its input carries kinds already resolved by the
// caller, which keeps the router renderer independent of the API types. The cost is that the value
// has to agree with the API's enum by convention rather than by the compiler, so the two places that
// compare against it do so through this one name - a second spelling would be the thing that drifts.
const roleKindDecode = "decode"

func Render(name string, input Input) (string, error) {
	render, ok := renderers[name]
	if !ok {
		return "", fmt.Errorf("unsupported router %q", name)
	}

	return render(input)
}

var renderers = map[string]func(Input) (string, error){
	LLMD: renderLLMD,
}

func renderLLMD(input Input) (string, error) {
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
		// Excludes decoders from the set of Pods the producer subscribes to. Appended rather than
		// assigned, and guarded, because an empty selector would otherwise render a leading comma -
		// which Kubernetes rejects as a label selector, and which no current caller produces. The
		// guard is here because this function is a library and its callers are not its contract.
		publisherSelector := input.EndpointSelector
		if len(orderedKinds) > 0 {
			exclusion := kinds[orderedKinds[0]] + "!=" + roleKindDecode
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
						"promptTokens":    0,
						"nonCachedTokens": 8,
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
		return "", fmt.Errorf("render llm-d configuration: %w", err)
	}

	return string(encoded), nil
}
