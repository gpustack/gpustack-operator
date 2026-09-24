package router

import "fmt"

var metricsByEngine = map[string]Metrics{
	"vllm": {
		Port:               8000,
		QueuedRequests:     "vllm:num_requests_waiting",
		RunningRequests:    "vllm:num_requests_running",
		KVCacheUtilization: "vllm:kv_cache_usage_perc",
		CacheInfo:          "vllm:cache_config_info",
	},
	// SGLang exports its KV cache capacity as two gauges from v0.5.11 on; earlier versions carried
	// it as labels on sglang:cache_config_info, which is the metric the picker's built-in SGLang
	// entry still reads (`llm-d/llm-d-router@v0.10.0:pkg/epp/framework/plugins/datalayer/extractor/metrics/factories.go:98-107`,
	// replaced by these gauges upstream after that release). A version before v0.5.11 exports
	// neither gauge, so its capacity reads as zero and every scrape records an extraction error.
	"sglang": {
		Port:               8000,
		QueuedRequests:     "sglang:num_queue_reqs",
		RunningRequests:    "sglang:num_running_reqs",
		KVCacheUtilization: "sglang:token_usage",
		CacheBlockSize:     "sglang:page_size",
		CacheNumBlocks:     "sglang:num_pages",
	},
}

// MetricsForEngine returns the serving metrics required by the managed router.
func MetricsForEngine(engine string) (Metrics, error) {
	metrics := metricsByEngine[engine]
	metrics.Engine = engine
	for _, required := range []struct {
		name  string
		value string
	}{
		{name: "queued requests", value: metrics.QueuedRequests},
		{name: "running requests", value: metrics.RunningRequests},
		{name: "KV cache utilization", value: metrics.KVCacheUtilization},
	} {
		if required.value == "" {
			return Metrics{}, fmt.Errorf("metric %q required by router %q is unavailable on engine %q",
				required.name, LLMD, engine)
		}
	}
	if metrics.Port < 1 {
		return Metrics{}, fmt.Errorf("metrics port required by router %q is unavailable on engine %q",
			LLMD, engine)
	}

	return metrics, nil
}
