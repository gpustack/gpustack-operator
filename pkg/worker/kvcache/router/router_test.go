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
	assert.Contains(t, left, "modeldeployment.gpustack.ai/role-kind")
	assert.NotContains(t, left, "replicas")
}

func TestRender_RefusesAnUnknownRouter(t *testing.T) {
	_, err := Render("unknown", Input{})
	require.EqualError(t, err, `unsupported router "unknown"`)
}

func TestRenderLLMD_CarriesTheMetricsContract(t *testing.T) {
	config, err := Render(LLMD, Input{
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

	assert.Contains(t, config, "vllm:num_requests_waiting")
	assert.Contains(t, config, "vllm:num_requests_running")
	assert.Contains(t, config, "vllm:kv_cache_usage_perc")
	assert.Contains(t, config, "port: 8000")
}

func TestRenderLLMD_CarriesTheKVEventsContract(t *testing.T) {
	config, err := Render(LLMD, Input{
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
	config, err := Render(LLMD, Input{
		Roles: []Role{
			{Kind: "server", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		},
		Namespace: "team-a",
		KVEvents:  KVEvents{Engine: "vllm", Port: 5557, ReplayPort: 5558, Topic: "kv@"},
	})
	require.NoError(t, err)

	assert.Contains(t, config, "precise-prefix-cache-producer")
	assert.NotContains(t, config, "!=decode",
		"the exclusion is keyed on the decode kind's own label, so it exists only with a decode role")
}

func TestRenderLLMD_DecodeDoesNotScorePrefixOverlap(t *testing.T) {
	config, err := Render(LLMD, Input{Roles: []Role{
		{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
	}})
	require.NoError(t, err)

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
	config, err := Render(LLMD, Input{Roles: []Role{
		{Kind: "prefill", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
		{Kind: "decode", RoleLabelKey: "modeldeployment.gpustack.ai/role-kind"},
	}})
	require.NoError(t, err)

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
