package router

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// epdInput builds the input one encoder/prefill/decode deployment renders the picker document from.
// The KV events contract is filled because the encoder's exclusion from the publisher set is only
// rendered beside the precise producer, which is what this input selects.
//
// The endpoint selector is the deployment's own, exactly as the controller passes it: the
// exclusions from the publisher set are this renderer's work, not the caller's, so supplying one
// that already carried them would assert the renderer's arithmetic against itself.
func epdInput() Input {
	return Input{
		Roles: []Role{
			{Kind: "encode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		},
		Namespace: "team-a",
		EndpointSelector: "app.kubernetes.io/name=modeldeployment," +
			"app.kubernetes.io/instance=qwen," +
			"modeldeployment.gpustack.ai/member-index=0",
		ModelName:         "Qwen/Qwen3-VL-8B-Instruct",
		TokenizerEndpoint: "http://qwen-prefill.team-a.svc:8000",
		KVEvents:          KVEvents{Engine: "vllm", Port: 5557, ReplayPort: 5558, Topic: "kv@"},
	}
}

// llmdDocument is the picker document as a tree, so a case reads one field rather than matching
// against the rendered text.
type llmdDocument struct {
	Plugins []struct {
		Type       string         `json:"type"`
		Name       string         `json:"name"`
		Parameters map[string]any `json:"parameters"`
	} `json:"plugins"`
	SchedulingProfiles []struct {
		Name    string `json:"name"`
		Plugins []struct {
			PluginRef string `json:"pluginRef"`
			Weight    *int   `json:"weight"`
		} `json:"plugins"`
	} `json:"schedulingProfiles"`
}

// profileRefs returns the plugin references of one named scheduling profile, and fails when no
// profile carries that name.
func (d llmdDocument) profileRefs(t *testing.T, name string) []string {
	t.Helper()

	refs, present := d.lookupProfile(name)
	require.True(t, present, "no scheduling profile named %q in the rendered document", name)

	return refs
}

// lookupProfile is the non-failing half of profileRefs, for the cases that assert a profile is
// absent.
func (d llmdDocument) lookupProfile(name string) ([]string, bool) {
	for _, profile := range d.SchedulingProfiles {
		if profile.Name != name {
			continue
		}
		refs := make([]string, 0, len(profile.Plugins))
		for _, plugin := range profile.Plugins {
			refs = append(refs, plugin.PluginRef)
		}

		return refs, true
	}

	return nil, false
}

func parseLLMDDocument(t *testing.T, document string) llmdDocument {
	t.Helper()

	var parsed llmdDocument
	require.NoError(t, yaml.Unmarshal([]byte(document), &parsed))

	return parsed
}

// llmdStringParameters reads one plugin parameter that renders as a map of strings, which is the
// shape the profile handler's profiles and deciders take.
func llmdStringParameters(t *testing.T, parameters map[string]any, key string) map[string]string {
	t.Helper()

	raw, ok := parameters[key].(map[string]any)
	require.True(t, ok, "the handler carries a %q map", key)
	out := make(map[string]string, len(raw))
	for name, value := range raw {
		out[name], _ = value.(string)
	}

	return out
}

// TestRenderLLMD_SelectsEncodePodsByTheRoleLabel pins how the picker finds an encoder. The picker
// is given the deployment's own role label rather than upstream's llm-d.ai/role, because that is
// the label the operator puts on its Pods and the routers configured by argv match on equality
// alone. The generic label-selector-filter is what carries it; the hardcoded encode-filter upstream
// ships reads llm-d.ai/role and would select nothing here.
func TestRenderLLMD_SelectsEncodePodsByTheRoleLabel(t *testing.T) {
	output, err := Render(LLMD, epdInput())
	require.NoError(t, err)
	document := parseLLMDDocument(t, output.Document)

	filters := 0
	for _, plugin := range document.Plugins {
		if plugin.Type != "label-selector-filter" || plugin.Name != "encode-pods" {
			continue
		}
		filters++
		matchLabels, ok := plugin.Parameters["matchLabels"].(map[string]any)
		require.True(t, ok, "the encoder filter selects on equalities alone")
		assert.Equal(t, "encode", matchLabels["modeldeployment.gpustack.ai/role-kind"])
	}
	assert.Equal(t, 1, filters, "the encoder filter is rendered exactly once")
}

// TestRenderLLMD_ScoresTheEncoderOnNothing holds the encoder profile to the shape upstream ships:
// the role filter and nothing else. An encoder keeps no KV cache, so the queue and KV-cache scorers
// read gauges it never exports and rank every candidate equally; the prefix scorer asks a question
// about token overlap that does not exist before a request has tokens. Upstream completes a profile
// that names no picker at load time, so the picker is supplied there rather than here.
func TestRenderLLMD_ScoresTheEncoderOnNothing(t *testing.T) {
	output, err := Render(LLMD, epdInput())
	require.NoError(t, err)
	document := parseLLMDDocument(t, output.Document)

	assert.Equal(t, []string{"encode-pods"}, document.profileRefs(t, "encode"))
}

// TestRenderLLMD_ScoresThePrefillerAndTheDecoderNormally is the other half of the case above: the
// encoder profile loses its scorers, and no other profile may follow it there. A change that made
// the exclusion cover every kind would satisfy the encoder case and silently stop ranking the
// prefiller and the decoder.
func TestRenderLLMD_ScoresThePrefillerAndTheDecoderNormally(t *testing.T) {
	output, err := Render(LLMD, epdInput())
	require.NoError(t, err)
	document := parseLLMDDocument(t, output.Document)

	for _, name := range []string{"prefill", "decode"} {
		refs := document.profileRefs(t, name)
		assert.Contains(t, refs, "queue-scorer", name)
		assert.Contains(t, refs, "kv-cache-utilization-scorer", name)
		assert.Contains(t, refs, "max-score-picker", name)
	}
	assert.Contains(t, document.profileRefs(t, "prefill"), "prefix-cache-scorer")
}

// TestRenderLLMD_WiresTheEncoderStageThroughTheMultimodalDecider pins the stage wiring. The
// multimodal decider is what makes the stage conditional: it activates on a request carrying media
// and leaves a text-only request to the prefiller and the decoder alone, which is the whole of the
// no-media path.
func TestRenderLLMD_WiresTheEncoderStageThroughTheMultimodalDecider(t *testing.T) {
	output, err := Render(LLMD, epdInput())
	require.NoError(t, err)
	document := parseLLMDDocument(t, output.Document)

	deciders := 0
	profiles := map[string]string{}
	wired := map[string]string{}
	handlers := 0
	for _, plugin := range document.Plugins {
		switch plugin.Type {
		case "always-disagg-multimodal-decider":
			deciders++
			assert.Empty(t, plugin.Parameters, "the decider takes no parameters upstream")
		case "disagg-profile-handler":
			handlers++
			profiles = llmdStringParameters(t, plugin.Parameters, "profiles")
			wired = llmdStringParameters(t, plugin.Parameters, "deciders")
		}
	}
	assert.Equal(t, 1, deciders, "the multimodal decider is rendered exactly once")
	assert.Equal(t, 1, handlers)

	assert.Equal(t, "encode", profiles["encode"], "the handler names the encoder profile")
	assert.Equal(t, "always-disagg-multimodal-decider", wired["encode"],
		"the encoder stage runs on the decider, not unconditionally")
	assert.Equal(t, "prefix-based-pd-decider", wired["prefill"],
		"the prefiller stage keeps its own decider beside the encoder's")
}

// TestRenderLLMD_LeavesTheEncoderStageOutOfAPair pins the shape a deployment without an encoder
// role renders. Every term this adds is keyed on the encode role, so a pair carries neither the
// filter, nor the profile, nor the decider, nor the wiring.
func TestRenderLLMD_LeavesTheEncoderStageOutOfAPair(t *testing.T) {
	output, err := Render(LLMD, Input{
		Roles: []Role{
			{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		},
		Namespace:        "team-a",
		EndpointSelector: "app.kubernetes.io/instance=qwen",
		KVEvents:         KVEvents{Engine: "vllm", Port: 5557, ReplayPort: 5558, Topic: "kv@"},
	})
	require.NoError(t, err)
	document := parseLLMDDocument(t, output.Document)

	_, present := document.lookupProfile("encode")
	assert.False(t, present, "a pair has no encoder stage to run")
	for _, plugin := range document.Plugins {
		assert.NotEqual(t, "always-disagg-multimodal-decider", plugin.Type)
		assert.NotEqual(t, "encode-pods", plugin.Name)
		if plugin.Type != "disagg-profile-handler" {
			continue
		}
		raw, ok := plugin.Parameters["profiles"].(map[string]any)
		require.True(t, ok)
		assert.NotContains(t, raw, "encode")
		raw, ok = plugin.Parameters["deciders"].(map[string]any)
		require.True(t, ok)
		assert.NotContains(t, raw, "encode")
	}
}

// TestRenderLLMD_KeepsTheEncoderOutOfTheKVEventPublishers holds the encoder out of the set of Pods
// the prefix producer subscribes to. The set is the deployment's own selector, which every role
// carries, so an encoder left in it is a subscriber dialing a socket it never opens -- the encoder
// keeps no KV cache and publishes no events.
func TestRenderLLMD_KeepsTheEncoderOutOfTheKVEventPublishers(t *testing.T) {
	output, err := Render(LLMD, epdInput())
	require.NoError(t, err)

	var selector string
	document := parseLLMDDocument(t, output.Document)
	for _, plugin := range document.Plugins {
		if plugin.Type != "precise-prefix-cache-producer" {
			continue
		}
		events, ok := plugin.Parameters["kvEventsConfig"].(map[string]any)
		require.True(t, ok, "the precise producer carries its events contract")
		discovery, ok := events["podDiscoveryConfig"].(map[string]any)
		require.True(t, ok)
		selector, _ = discovery["podLabelSelector"].(string)
	}
	require.NotEmpty(t, selector, "the precise producer carries the publisher selector")
	assert.Contains(t, selector, "modeldeployment.gpustack.ai/role-kind!=decode")
	assert.Contains(t, selector, "modeldeployment.gpustack.ai/role-kind!=encode")
}

// TestRenderLLMD_IsDeterministicAcrossEncoderRoleOrder holds the encoder role's position in the
// list to be the author's accident rather than the picker's: the document is hashed into the router
// Pod, so a shape that followed the declared order would roll the router when a user reordered
// roles they did not intend to change anything about.
func TestRenderLLMD_IsDeterministicAcrossEncoderRoleOrder(t *testing.T) {
	orders := map[string][]Role{
		"encode first": {
			{Kind: "encode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		},
		"encode in the middle": {
			{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "encode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		},
		"encode last": {
			{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "encode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		},
	}

	rendered := make(map[string]string, len(orders))
	for name, roles := range orders {
		input := epdInput()
		input.Roles = roles
		output, err := Render(LLMD, input)
		require.NoError(t, err, name)
		rendered[name] = output.Document
	}

	assert.Equal(t, rendered["encode first"], rendered["encode in the middle"])
	assert.Equal(t, rendered["encode first"], rendered["encode last"])
}

// TestRenderLLMD_LeavesAPairDocumentByteIdentical is the compatibility rule in its strictest form:
// a deployment with no encoder role renders exactly the bytes it rendered before, because the
// document is hashed into the router Pod and any difference rolls every prefill/decode router in the
// cluster. The expected document is written out in full rather than compared against a stored
// golden, so a change to the renderer has to be made here deliberately.
func TestRenderLLMD_LeavesAPairDocumentByteIdentical(t *testing.T) {
	const want = `apiVersion: llm-d.ai/v1alpha1
dataLayer:
  sources:
  - extractors:
    - pluginRef: core-metrics-extractor
    pluginRef: metrics-data-source
  - extractors:
    - pluginRef: precise-prefix-cache-producer
    pluginRef: endpoint-notification-source
kind: EndpointPickerConfig
plugins:
- name: decode-pods
  parameters:
    matchLabels:
      modeldeployment.gpustack.ai/role-kind: decode
  type: label-selector-filter
- name: prefill-pods
  parameters:
    matchLabels:
      modeldeployment.gpustack.ai/role-kind: prefill
  type: label-selector-filter
- parameters:
    modelName: Qwen/Qwen3-8B
    vllm:
      url: http://qwen-prefill.team-a.svc:8000
  type: token-producer
- type: endpoint-notification-source
- parameters:
    kvEventsConfig:
      concurrency: 4
      discoverPods: true
      engineType: vllm
      podDiscoveryConfig:
        podLabelSelector: app.kubernetes.io/name=modeldeployment,app.kubernetes.io/instance=qwen,modeldeployment.gpustack.ai/role-kind!=decode
        podNamespace: team-a
        replaySocketPort: 5558
        socketPort: 5557
      topicFilter: kv@
  type: precise-prefix-cache-producer
- type: queue-scorer
- type: kv-cache-utilization-scorer
- type: max-score-picker
- parameters:
    prefixMatchInfoProducerName: precise-prefix-cache-producer
  type: prefix-cache-scorer
- type: metrics-data-source
- type: core-metrics-extractor
- parameters:
    nonCachedTokens: 8
    promptTokens: 0
  type: prefix-based-pd-decider
- parameters:
    deciders:
      prefill: prefix-based-pd-decider
    profiles:
      decode: decode
      prefill: prefill
  type: disagg-profile-handler
schedulingProfiles:
- name: decode
  plugins:
  - pluginRef: decode-pods
  - pluginRef: queue-scorer
    weight: 2
  - pluginRef: kv-cache-utilization-scorer
    weight: 2
  - pluginRef: max-score-picker
- name: prefill
  plugins:
  - pluginRef: prefill-pods
  - pluginRef: queue-scorer
    weight: 2
  - pluginRef: kv-cache-utilization-scorer
    weight: 2
  - pluginRef: max-score-picker
  - pluginRef: prefix-cache-scorer
    weight: 3
`

	output, err := Render(LLMD, Input{
		Roles: []Role{
			{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
			{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		},
		Namespace:         "team-a",
		EndpointSelector:  "app.kubernetes.io/name=modeldeployment,app.kubernetes.io/instance=qwen",
		ModelName:         "Qwen/Qwen3-8B",
		TokenizerEndpoint: "http://qwen-prefill.team-a.svc:8000",
		KVEvents:          KVEvents{Engine: "vllm", Port: 5557, ReplayPort: 5558, Topic: "kv@"},
	})
	require.NoError(t, err)

	assert.Equal(t, want, output.Document)
}
