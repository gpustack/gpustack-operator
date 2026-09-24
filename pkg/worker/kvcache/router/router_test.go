package router

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestRenderLLMD_IsDeterministicAcrossRoleOrder(t *testing.T) {
	left, err := Render(LLMD, Input{Roles: []Role{
		{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
	}})
	require.NoError(t, err)
	right, err := Render(LLMD, Input{Roles: []Role{
		{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
	}})
	require.NoError(t, err)

	assert.Equal(t, left, right)
	assert.Contains(t, left.Document, "modeldeployment.gpustack.ai/role-kind")
	assert.NotContains(t, left.Document, "replicas")
}

func TestRender_RefusesAnUnknownRouter(t *testing.T) {
	_, err := Render("unknown", Input{})
	require.EqualError(t, err, `unsupported router "unknown"`)
}

// TestRender_RefusesARendererThatFillsNeitherHalf pins the refusal on the shape a newly added
// renderer arrives in. A function that returns its zero value compiles and renders nothing, and
// without this rule the caller would mount an empty document and start the router anyway.
//
// The renderer is registered here rather than declared beside the real ones because a permanently
// registered empty router would be reachable from the API, and the point is to catch one that is
// being written rather than to ship one.
func TestRender_RefusesARendererThatFillsNeitherHalf(t *testing.T) {
	const name = "fills-neither"
	// It fills the command, so the refusal this pins is the one under test rather than the missing
	// command that every renderer needs whichever half it fills.
	renderers[name] = func(Input) (Output, error) { return Output{Command: []string{"x"}}, nil }
	t.Cleanup(func() { delete(renderers, name) })

	_, err := Render(name, Input{})
	require.EqualError(t, err,
		`router "fills-neither" rendered neither a configuration document nor arguments`)
}

// TestRender_RefusesARendererThatFillsBothHalves pins the other half of the same rule. A router
// reads one of the two and ignores the other, so a renderer that fills both hands a user
// configuration that nothing applies and nothing reports.
func TestRender_RefusesARendererThatFillsBothHalves(t *testing.T) {
	const name = "fills-both"
	renderers[name] = func(Input) (Output, error) {
		return Output{Command: []string{"x"}, Document: "a: b", Arguments: []string{"--port=8000"}}, nil
	}
	t.Cleanup(func() { delete(renderers, name) })

	_, err := Render(name, Input{})
	require.EqualError(t, err,
		`router "fills-both" rendered both a configuration document and arguments`)
}

// TestEveryRegisteredRendererFillsExactlyOneHalf holds every renderer actually registered to the
// rule, so a router added later is covered without anyone remembering to add a case.
//
// It calls the renderer DIRECTLY rather than through Render, and that is the whole point of the
// case: Render refuses a wrong shape, so a test that went through it would fail on the error and
// never reach this assertion, leaving an assertion that cannot fail standing beside a rule nothing
// checks independently of its own guard.
func TestEveryRegisteredRendererFillsExactlyOneHalf(t *testing.T) {
	// The input is complete rather than minimal, because a renderer that refuses an incomplete one
	// would report a missing half it was never given the chance to fill.
	input := Input{
		Roles: []Role{{
			Kind:         "server",
			RoleLabelKey: "modeldeployment.gpustack.ai/role-kind",
			Selector:     map[string]string{"app.kubernetes.io/instance": "qwen"},
		}},
		Namespace:      "team-a",
		EndpointLabels: map[string]string{"app.kubernetes.io/instance": "qwen"},
		ListenPort:     8081,
		MetricsPort:    9090,
		DiscoveryPort:  8000,
	}
	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			output, err := render(input)
			require.NoError(t, err)

			hasDocument := output.Document != ""
			hasArguments := len(output.Arguments) != 0
			assert.NotEqual(t, hasDocument, hasArguments,
				"a router is configured by a document or by arguments, never by both and never by neither")
		})
	}
}

// TestEveryRegisteredRendererNamesItsOwnBinary holds the half that the exactly-one rule above does
// not reach. One image carries all three programs and declares no entrypoint, so a renderer that
// fills its configuration half correctly and leaves the command empty produces a container whose
// first flag is read as the program name -- which fails as "executable file not found", naming the
// flag rather than the omission. Asserting the arguments alone passes over exactly that.
//
// The expected names are written out per router rather than read back from the table, so that a
// table edit which points two routers at one program is a failure here instead of agreeing with
// itself.
func TestEveryRegisteredRendererNamesItsOwnBinary(t *testing.T) {
	want := map[string]string{
		LLMD:   "epp",
		VLLM:   "vllm-router",
		SGLang: "sgl-model-gateway",
	}
	require.Len(t, want, len(renderers), "every registered router needs a binary named here")

	input := Input{
		Roles: []Role{{
			Kind:         "server",
			RoleLabelKey: "modeldeployment.gpustack.ai/role-kind",
			Selector:     map[string]string{"app.kubernetes.io/instance": "qwen"},
		}},
		Namespace:      "team-a",
		EndpointLabels: map[string]string{"app.kubernetes.io/instance": "qwen"},
		ListenPort:     8081,
		MetricsPort:    9090,
		DiscoveryPort:  8000,
	}
	for name, binary := range want {
		t.Run(name, func(t *testing.T) {
			output, err := Render(name, input)
			require.NoError(t, err)

			assert.Equal(t, []string{binary}, output.Command)
			// A path would tie the image to one layout; the program only has to be on PATH.
			assert.NotContains(t, output.Command[0], "/", "the command is a bare program name")
		})
	}
}

func TestRenderLLMD_CarriesTheMetricsContract(t *testing.T) {
	rendered, err := Render(LLMD, Input{
		Roles: []Role{{Kind: "server", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"}},
		Metrics: Metrics{
			Engine:             "vllm",
			Port:               8000,
			QueuedRequests:     "vllm:num_requests_waiting",
			RunningRequests:    "vllm:num_requests_running",
			KVCacheUtilization: "vllm:kv_cache_usage_perc",
		},
	})
	require.NoError(t, err)
	config := rendered.Document

	assert.Contains(t, config, "vllm:num_requests_waiting")
	assert.Contains(t, config, "vllm:num_requests_running")
	assert.Contains(t, config, "vllm:kv_cache_usage_perc")
	assert.Contains(t, config, "port: 8000")
}

// TestRenderLLMD_NamesTheEngineAPodWithoutALabelIsReadAs pins which metric names the picker reads a
// model server's metrics by. Its extractor picks them by the Pod's engine-type label and reads a Pod
// without one as its default engine, which upstream defaults to vllm, and no model-server Pod
// carries that label. An SGLang deployment left on that default is read by vLLM's names, which its
// servers do not export, so no endpoint ever has fresh metrics.
//
// vLLM is held to rendering WITHOUT the key rather than with it spelled out: the document is hashed
// into the router Pod, so writing the value upstream already holds would roll every existing vLLM
// router for nothing. The SGLang case is what makes that absence mean something - the same parse
// finds the key there.
func TestRenderLLMD_NamesTheEngineAPodWithoutALabelIsReadAs(t *testing.T) {
	for _, tc := range []struct {
		engine string
		want   string
	}{
		{engine: "vllm", want: ""},
		{engine: "sglang", want: "sglang"},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			metrics, err := MetricsForEngine(tc.engine)
			require.NoError(t, err)
			output, err := Render(LLMD, Input{
				Roles:   []Role{{Kind: "server", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"}},
				Metrics: metrics,
			})
			require.NoError(t, err)

			var rendered struct {
				Plugins []struct {
					Type       string         `json:"type"`
					Parameters map[string]any `json:"parameters"`
				} `json:"plugins"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(output.Document), &rendered))

			extractors := 0
			for _, plugin := range rendered.Plugins {
				if plugin.Type != "core-metrics-extractor" {
					continue
				}
				extractors++
				defaultEngine, present := plugin.Parameters["defaultEngine"]
				if tc.want == "" {
					assert.False(t, present, "the upstream default is left to upstream")
				} else {
					assert.Equal(t, tc.want, defaultEngine)
				}
			}
			require.Equal(t, 1, extractors)
		})
	}
}

// TestRenderLLMD_CarriesTheCacheCapacityBesideTheApproximateProducer pins where the picker learns a
// server's KV cache capacity from. Naming an engine in engineConfigs replaces upstream's built-in
// entry for it, so a capacity field this renderer leaves out is one upstream never reads, and the
// approximate producer, which sizes each server's prefix index by that capacity, falls back to its
// own default.
//
// The expected names are written out rather than read back from the engine table, so a table edit
// that drops or renames one fails here instead of agreeing with itself. SGLang is held to its two
// gauges and to NO info-style spec: the info gauge upstream's built-in entry reads is one SGLang no
// longer exports, and a spec naming an absent metric records an extraction error on every scrape.
//
// The precise case is what makes the absence mean something: the same parse finds the fields
// beside the approximate producer, and the precise producer reads none of them, so rendering them
// there would roll every such router for a value nothing reads.
func TestRenderLLMD_CarriesTheCacheCapacityBesideTheApproximateProducer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		engine   string
		kvEvents KVEvents
		want     map[string]any
	}{
		{
			name:   "sglang",
			engine: "sglang",
			want: map[string]any{
				"cacheBlockSizeSpec": "sglang:page_size",
				"cacheNumBlocksSpec": "sglang:num_pages",
			},
		},
		{
			name:   "vllm",
			engine: "vllm",
			want:   map[string]any{"cacheInfoSpec": "vllm:cache_config_info"},
		},
		{
			name:     "vllm with the precise producer",
			engine:   "vllm",
			kvEvents: KVEvents{Engine: "vllm", Port: 5557, ReplayPort: 5558, Topic: "kv@"},
			want:     map[string]any{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics, err := MetricsForEngine(tc.engine)
			require.NoError(t, err)
			output, err := Render(LLMD, Input{
				Roles:    []Role{{Kind: "server", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"}},
				Metrics:  metrics,
				KVEvents: tc.kvEvents,
			})
			require.NoError(t, err)

			var rendered struct {
				Plugins []struct {
					Type       string `json:"type"`
					Parameters struct {
						EngineConfigs []map[string]any `json:"engineConfigs"`
					} `json:"parameters"`
				} `json:"plugins"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(output.Document), &rendered))

			var configs []map[string]any
			for _, plugin := range rendered.Plugins {
				if plugin.Type == "core-metrics-extractor" {
					configs = append(configs, plugin.Parameters.EngineConfigs...)
				}
			}
			require.Len(t, configs, 1)
			assert.Equal(t, tc.engine, configs[0]["name"])

			capacity := map[string]any{}
			for key, value := range configs[0] {
				switch key {
				case "cacheInfoSpec", "cacheBlockSizeLabelName", "cacheNumBlocksLabelName",
					"cacheBlockSizeSpec", "cacheNumBlocksSpec":
					capacity[key] = value
				}
			}
			assert.Equal(t, tc.want, capacity)
		})
	}
}

func TestRenderLLMD_CarriesTheKVEventsContract(t *testing.T) {
	rendered, err := Render(LLMD, Input{
		Roles: []Role{
			{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		},
		Namespace:         "team-a",
		EndpointSelector:  "app.kubernetes.io/name=modeldeployment,app.kubernetes.io/instance=qwen",
		ModelName:         "Qwen/Qwen3-8B",
		TokenizerEndpoint: "http://qwen-prefill.team-a.svc:8000",
		KVEvents: KVEvents{
			Engine: "vllm", Port: 5557, ReplayPort: 5558, Topic: "kv@",
		},
	})
	require.NoError(t, err)
	config := rendered.Document

	assert.Contains(t, config, "precise-prefix-cache-producer")
	assert.Contains(t, config, "topicFilter: kv@")
	assert.Contains(t, config, "socketPort: 5557")
	assert.Contains(t, config, "replaySocketPort: 5558")
	assert.Contains(t, config, "podNamespace: team-a")
	assert.Contains(t, config, "modeldeployment.gpustack.ai/role-kind!=decode")
	assert.Contains(t, config, "modelName: Qwen/Qwen3-8B")
	assert.Contains(t, config, "url: http://qwen-prefill.team-a.svc:8000")
	assert.Contains(t, config, "endpoint-notification-source")
	assert.Contains(t, config, "dataLayer:")
	assert.NotContains(t, config, "approx-prefix-cache-producer")
	assert.Contains(t, config, "nonCachedTokens: 8")
	assert.Contains(t, config, "promptTokens: 0")
}

func TestRenderLLMD_NoDecodeRoleMeansNoPublisherExclusion(t *testing.T) {
	rendered, err := Render(LLMD, Input{
		Roles: []Role{
			{Kind: "server", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		},
		Namespace: "team-a",
		KVEvents:  KVEvents{Engine: "vllm", Port: 5557, ReplayPort: 5558, Topic: "kv@"},
	})
	require.NoError(t, err)
	config := rendered.Document

	assert.Contains(t, config, "precise-prefix-cache-producer")
	assert.NotContains(t, config, "!=decode",
		"the exclusion is keyed on the decode kind's own label, so it exists only with a decode role")
}

func TestRenderLLMD_DecodeDoesNotScorePrefixOverlap(t *testing.T) {
	output, err := Render(LLMD, Input{Roles: []Role{
		{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
	}})
	require.NoError(t, err)
	config := output.Document

	var rendered struct {
		SchedulingProfiles []struct {
			Name    string `json:"name"`
			Plugins []struct {
				PluginRef string `json:"pluginRef"`
			} `json:"plugins"`
		} `json:"schedulingProfiles"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(config), &rendered))

	for _, profile := range rendered.SchedulingProfiles {
		refs := make([]string, 0, len(profile.Plugins))
		for _, plugin := range profile.Plugins {
			refs = append(refs, plugin.PluginRef)
		}
		if profile.Name == "decode" {
			assert.NotContains(t, refs, "prefix-cache-scorer")
		} else {
			assert.Contains(t, refs, "prefix-cache-scorer")
		}
	}
}

func TestRenderLLMD_EveryProfileCanPickAnEndpoint(t *testing.T) {
	output, err := Render(LLMD, Input{Roles: []Role{
		{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
	}})
	require.NoError(t, err)
	config := output.Document

	var rendered struct {
		SchedulingProfiles []struct {
			Name    string `json:"name"`
			Plugins []struct {
				PluginRef string `json:"pluginRef"`
			} `json:"plugins"`
		} `json:"schedulingProfiles"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(config), &rendered))
	require.NotEmpty(t, rendered.SchedulingProfiles)

	for _, profile := range rendered.SchedulingProfiles {
		refs := make([]string, 0, len(profile.Plugins))
		for _, plugin := range profile.Plugins {
			refs = append(refs, plugin.PluginRef)
		}
		assert.Contains(t, refs, "max-score-picker", profile.Name)
	}
}
