package worker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

// The expositions below are written out rather than read from a fixture directory so that the
// label shape under test is visible next to the assertion about it. The two SGLang shapes are
// both real: one engine reports its rank on every series, and another reports no rank at all
// and splits its first-token latency by whether the request was streamed.

// scalerSamplerVLLMExposition is a vLLM engine reporting request gauges, token counters and
// latency histograms under an engine label.
const scalerSamplerVLLMExposition = `# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 4.0
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 2.0
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 0.35
# TYPE vllm:prompt_tokens_total counter
vllm:prompt_tokens_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 120000.0
# TYPE vllm:generation_tokens_total counter
vllm:generation_tokens_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 48000.0
# TYPE vllm:prefix_cache_hits_total counter
vllm:prefix_cache_hits_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 30000.0
# TYPE vllm:prefix_cache_queries_total counter
vllm:prefix_cache_queries_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 60000.0
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="0.5"} 40.0
vllm:time_to_first_token_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="1.0"} 50.0
vllm:time_to_first_token_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="+Inf"} 56.0
vllm:time_to_first_token_seconds_sum{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 20.0
vllm:time_to_first_token_seconds_count{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 56.0
# TYPE vllm:inter_token_latency_seconds histogram
vllm:inter_token_latency_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="0.01"} 1200.0
vllm:inter_token_latency_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="0.05"} 4800.0
vllm:inter_token_latency_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="+Inf"} 5428.0
vllm:inter_token_latency_seconds_sum{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 31.09642936099135
vllm:inter_token_latency_seconds_count{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 5428.0
`

// scalerSamplerVLLMSecondExposition is the same engine fourteen seconds later, with more work
// done and more of it served from the prefix cache.
const scalerSamplerVLLMSecondExposition = `# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 7.0
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 3.0
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 0.42
# TYPE vllm:prompt_tokens_total counter
vllm:prompt_tokens_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 132000.0
# TYPE vllm:generation_tokens_total counter
vllm:generation_tokens_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 51600.0
# TYPE vllm:prefix_cache_hits_total counter
vllm:prefix_cache_hits_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 33000.0
# TYPE vllm:prefix_cache_queries_total counter
vllm:prefix_cache_queries_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 64000.0
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="0.5"} 110.0
vllm:time_to_first_token_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="1.0"} 130.0
vllm:time_to_first_token_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="+Inf"} 140.0
vllm:time_to_first_token_seconds_sum{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 61.0
vllm:time_to_first_token_seconds_count{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 140.0
# TYPE vllm:inter_token_latency_seconds histogram
vllm:inter_token_latency_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="0.01"} 1500.0
vllm:inter_token_latency_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="0.05"} 5400.0
vllm:inter_token_latency_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="+Inf"} 6000.0
vllm:inter_token_latency_seconds_sum{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 33.09642936099135
vllm:inter_token_latency_seconds_count{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 6000.0
`

// scalerSamplerSGLangExposition is an SGLang engine reporting every series under a tensor, a
// pipeline and an expert-parallel rank, with two ranks reporting the same counters.
//
// THE SECOND RANK REPORTS A DIFFERENT HISTOGRAM on purpose: a sampler that reads both ranks
// would double the token counts and widen the latency window, and a test that cannot tell the
// two apart does not hold the sampler to reading one rank.
const scalerSamplerSGLangExposition = `# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="0"} 4.0
sglang:num_running_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="1"} 4.0
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="0"} 2.0
sglang:num_queue_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="1"} 2.0
# TYPE sglang:token_usage gauge
sglang:token_usage{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="0"} 0.38
# TYPE sglang:prompt_tokens_total counter
sglang:prompt_tokens_total{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 100000.0
sglang:prompt_tokens_total{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 100000.0
# TYPE sglang:generation_tokens_total counter
sglang:generation_tokens_total{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 40000.0
sglang:generation_tokens_total{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 40000.0
# TYPE sglang:prefill_effective_tokens_total counter
sglang:prefill_effective_tokens_total{mode="input",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 60000.0
sglang:prefill_effective_tokens_total{mode="input",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 60000.0
sglang:prefill_effective_tokens_total{mode="device_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 20000.0
sglang:prefill_effective_tokens_total{mode="device_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 20000.0
sglang:prefill_effective_tokens_total{mode="host_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 5000.0
sglang:prefill_effective_tokens_total{mode="host_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 5000.0
sglang:prefill_effective_tokens_total{mode="storage_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 1000.0
sglang:prefill_effective_tokens_total{mode="storage_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 1000.0
# TYPE sglang:time_to_first_token_seconds histogram
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="0.5"} 40.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="1.0"} 50.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="+Inf"} 56.0
sglang:time_to_first_token_seconds_sum{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 20.0
sglang:time_to_first_token_seconds_count{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 56.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="0.5"} 0.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="1.0"} 0.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="+Inf"} 400.0
sglang:time_to_first_token_seconds_sum{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 300.0
sglang:time_to_first_token_seconds_count{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 400.0
# TYPE sglang:inter_token_latency_seconds histogram
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="0.01"} 1200.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="0.05"} 4800.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="+Inf"} 5428.0
sglang:inter_token_latency_seconds_sum{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 15.15720793402943
sglang:inter_token_latency_seconds_count{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 5428.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="0.01"} 1200.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="0.05"} 4800.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="+Inf"} 5428.0
sglang:inter_token_latency_seconds_sum{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 15.15720793402943
sglang:inter_token_latency_seconds_count{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 5428.0
`

// scalerSamplerSGLangSecondExposition is the same SGLang engine nineteen seconds later.
const scalerSamplerSGLangSecondExposition = `# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="0"} 7.0
sglang:num_running_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="1"} 7.0
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="0"} 3.0
sglang:num_queue_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="1"} 3.0
# TYPE sglang:token_usage gauge
sglang:token_usage{engine_type="unified",model_name="Qwen/Qwen2.5-7B-Instruct",moe_ep_rank="0",pp_rank="0",tp_rank="0"} 0.44
# TYPE sglang:prompt_tokens_total counter
sglang:prompt_tokens_total{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 112000.0
sglang:prompt_tokens_total{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 112000.0
# TYPE sglang:generation_tokens_total counter
sglang:generation_tokens_total{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 43600.0
sglang:generation_tokens_total{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 43600.0
# TYPE sglang:prefill_effective_tokens_total counter
sglang:prefill_effective_tokens_total{mode="input",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 78000.0
sglang:prefill_effective_tokens_total{mode="input",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 78000.0
sglang:prefill_effective_tokens_total{mode="device_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 26000.0
sglang:prefill_effective_tokens_total{mode="device_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 26000.0
sglang:prefill_effective_tokens_total{mode="host_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 7000.0
sglang:prefill_effective_tokens_total{mode="host_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 7000.0
sglang:prefill_effective_tokens_total{mode="storage_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 2000.0
sglang:prefill_effective_tokens_total{mode="storage_hit",model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 2000.0
# TYPE sglang:time_to_first_token_seconds histogram
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="0.5"} 110.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="1.0"} 130.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="+Inf"} 140.0
sglang:time_to_first_token_seconds_sum{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 61.0
sglang:time_to_first_token_seconds_count{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 140.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="0.5"} 0.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="1.0"} 0.0
sglang:time_to_first_token_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="+Inf"} 900.0
sglang:time_to_first_token_seconds_sum{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 700.0
sglang:time_to_first_token_seconds_count{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 900.0
# TYPE sglang:inter_token_latency_seconds histogram
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="0.01"} 1500.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="0.05"} 5400.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0",le="+Inf"} 6000.0
sglang:inter_token_latency_seconds_sum{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 17.15720793402943
sglang:inter_token_latency_seconds_count{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="0"} 6000.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="0.01"} 1500.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="0.05"} 5400.0
sglang:inter_token_latency_seconds_bucket{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1",le="+Inf"} 6000.0
sglang:inter_token_latency_seconds_sum{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 17.15720793402943
sglang:inter_token_latency_seconds_count{model_name="Qwen/Qwen2.5-7B-Instruct",pp_rank="0",tp_rank="1"} 6000.0
`

// scalerSamplerSGLangRankFreeExposition is an SGLang engine that reports no rank at all and
// splits its first-token latency by whether the request was streamed.
const scalerSamplerSGLangRankFreeExposition = `# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 1.0
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 0.0
# TYPE sglang:prefill_effective_tokens_total counter
sglang:prefill_effective_tokens_total{engine_type="unified",mode="input",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 4180.0
sglang:prefill_effective_tokens_total{engine_type="unified",mode="device_hit",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 14153.0
sglang:prefill_effective_tokens_total{engine_type="unified",mode="host_hit",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 0.0
sglang:prefill_effective_tokens_total{engine_type="unified",mode="storage_hit",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 0.0
# TYPE sglang:time_to_first_token_seconds histogram
sglang:time_to_first_token_seconds_sum{engine_type="unified",is_streaming="false",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 27.006403835998753
sglang:time_to_first_token_seconds_count{engine_type="unified",is_streaming="false",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 59.0
sglang:time_to_first_token_seconds_sum{engine_type="unified",is_streaming="true",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 1.3266321940018315
sglang:time_to_first_token_seconds_count{engine_type="unified",is_streaming="true",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 58.0
# TYPE sglang:inter_token_latency_seconds histogram
sglang:inter_token_latency_seconds_sum{engine_type="unified",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 15.15720793402943
sglang:inter_token_latency_seconds_count{engine_type="unified",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 2699.0
`

// scalerSamplerSGLangRankFreeSecondExposition is the same rank-free engine ten seconds later,
// with a longer first-token latency and a longer inter-token latency.
const scalerSamplerSGLangRankFreeSecondExposition = `# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 1.0
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs{engine_type="unified",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 0.0
# TYPE sglang:prefill_effective_tokens_total counter
sglang:prefill_effective_tokens_total{engine_type="unified",mode="input",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 4180.0
sglang:prefill_effective_tokens_total{engine_type="unified",mode="device_hit",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 14153.0
sglang:prefill_effective_tokens_total{engine_type="unified",mode="host_hit",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 0.0
sglang:prefill_effective_tokens_total{engine_type="unified",mode="storage_hit",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 0.0
# TYPE sglang:time_to_first_token_seconds histogram
sglang:time_to_first_token_seconds_sum{engine_type="unified",is_streaming="false",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 54.012807671997506
sglang:time_to_first_token_seconds_count{engine_type="unified",is_streaming="false",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 118.0
sglang:time_to_first_token_seconds_sum{engine_type="unified",is_streaming="true",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 2.653264388003663
sglang:time_to_first_token_seconds_count{engine_type="unified",is_streaming="true",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 116.0
# TYPE sglang:inter_token_latency_seconds histogram
sglang:inter_token_latency_seconds_sum{engine_type="unified",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 30.31441586805886
sglang:inter_token_latency_seconds_count{engine_type="unified",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 5398.0
`

// scalerSamplerIdleExposition is an engine that is serving nothing: no request is running, none
// is waiting, and its counters did not move.
const scalerSamplerIdleExposition = `# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 0.0
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 0.0
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 0.10
# TYPE vllm:prompt_tokens_total counter
vllm:prompt_tokens_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 1000.0
# TYPE vllm:generation_tokens_total counter
vllm:generation_tokens_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 500.0
# TYPE vllm:prefix_cache_hits_total counter
vllm:prefix_cache_hits_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 250.0
# TYPE vllm:prefix_cache_queries_total counter
vllm:prefix_cache_queries_total{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 1000.0
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="0.5"} 40.0
vllm:time_to_first_token_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="+Inf"} 60.0
vllm:time_to_first_token_seconds_sum{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 20.0
vllm:time_to_first_token_seconds_count{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 60.0
# TYPE vllm:inter_token_latency_seconds histogram
vllm:inter_token_latency_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="0.01"} 1200.0
vllm:inter_token_latency_seconds_bucket{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct",le="+Inf"} 1500.0
vllm:inter_token_latency_seconds_sum{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 40.0
vllm:inter_token_latency_seconds_count{engine="0",model_name="Qwen/Qwen2.5-7B-Instruct"} 1500.0
`

// scalerSamplerPod builds a ready engine Pod of a role, as the render leaves one: the role on
// its component label, a metrics port the engine bound, and the main container the operator
// rendered.
func scalerSamplerPod(name, role string, uid types.UID, ordinal int) core.Pod {
	return core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       uid,
			Labels: map[string]string{
				modelDeploymentLabelKeyComponent:   role,
				modelDeploymentReplicaOrdinalLabel: strconv.Itoa(ordinal),
			},
			Annotations: map[string]string{
				"prometheus.io/port":                    "8000",
				"prometheus.io/scheme":                  "http",
				"prometheus.io/path":                    "/metrics",
				"prometheus.io/scrape":                  modelDeploymentEndpointEligibleValue,
				kueuepodconst.GroupTotalCountAnnotation: "1",
			},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name:  modelDeploymentMainContainerName,
				Ports: []core.ContainerPort{{Name: "http", ContainerPort: 8000}},
			}},
		},
		Status: core.PodStatus{
			PodIP: "10.0.0.1",
			Conditions: []core.PodCondition{{
				Type:   core.PodReady,
				Status: core.ConditionTrue,
			}},
			ContainerStatuses: []core.ContainerStatus{{
				Name:         modelDeploymentMainContainerName,
				Ready:        true,
				RestartCount: 0,
			}},
		},
	}
}

// scalerSamplerTarget builds the target a scaler plans for.
func scalerSamplerTarget(uid types.UID, engine string, roles ...workercore.ModelDeploymentRole) *workercore.ModelDeployment {
	return &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "target", Namespace: "default", UID: uid},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: engine},
			Roles:  roles,
		},
	}
}

// scalerSamplerRole builds a role of the target.
func scalerSamplerRole(name string, kind workercore.ModelDeploymentRoleKind, replicas int32) workercore.ModelDeploymentRole {
	return workercore.ModelDeploymentRole{Name: name, Kind: kind, Replicas: replicas}
}

// scalerSamplerScaler builds the scaler that plans for the target.
func scalerSamplerScaler(uid types.UID, intervalSeconds int32) *workercore.ModelDeploymentScaler {
	return &workercore.ModelDeploymentScaler{
		ObjectMeta: meta.ObjectMeta{Name: "scaler", Namespace: "default", UID: uid},
		Spec: workercore.ModelDeploymentScalerSpec{
			TargetRef: workercore.ModelDeploymentScalerTargetReference{Name: "target"},
			Sampling:  workercore.ModelDeploymentScalerSamplingSpec{IntervalSeconds: intervalSeconds},
		},
	}
}

// scalerSamplerScripted answers a pass from a fixed script of expositions, one per Pod, and
// records how many times each Pod was asked.
type scalerSamplerScripted struct {
	mu         sync.Mutex
	bodies     map[string][]byte
	failures   map[string]bool
	asked      map[string]int
	blockUntil chan struct{}
}

func (s *scalerSamplerScripted) scrape(_ context.Context, pod *core.Pod) ([]byte, error) {
	s.mu.Lock()
	s.asked[pod.Name]++
	body, known := s.bodies[pod.Name]
	failing := s.failures[pod.Name]
	block := s.blockUntil
	s.mu.Unlock()

	if block != nil {
		<-block
	}
	if failing {
		return nil, fmt.Errorf("pod %s does not answer", pod.Name)
	}
	if !known {
		return nil, fmt.Errorf("pod %s has no scripted exposition", pod.Name)
	}
	return body, nil
}

func (s *scalerSamplerScripted) askedCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked[name]
}

// TestModelDeploymentScalerSampler_TwoPassRates reads a role twice and holds the rates it
// reports against the counters each engine actually exported.
func TestModelDeploymentScalerSampler_TwoPassRates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// engine is the engine the target runs.
		engine string
		// first and second are the expositions of the two passes.
		first  string
		second string
		// window is how long the two passes are apart.
		window time.Duration
		// wantPromptTokensPerSecond is the rate of cache-missed prompt tokens.
		wantPromptTokensPerSecond float64
		// wantGenerationTokensPerSecond is the rate of generated tokens.
		wantGenerationTokensPerSecond float64
		// wantTTFTMilliseconds and wantITLMilliseconds are the reported latencies.
		wantTTFTMilliseconds float64
		wantITLMilliseconds  float64
		// wantRunning and wantWaiting are the reported request gauges.
		wantRunning float64
		wantWaiting float64
		// wantKVCacheUsage is the reported cache usage.
		wantKVCacheUsage float64
	}{
		{
			name:                          "vLLM counts the prompt tokens the prefix cache did not serve",
			engine:                        workercore.ModelDeploymentEngineVLLM,
			first:                         scalerSamplerVLLMExposition,
			second:                        scalerSamplerVLLMSecondExposition,
			window:                        14 * time.Second,
			wantPromptTokensPerSecond:     9000.0 / 14.0,
			wantGenerationTokensPerSecond: 3600.0 / 14.0,
			wantTTFTMilliseconds:          1000.0,
			wantITLMilliseconds:           50.0,
			wantRunning:                   7,
			wantWaiting:                   3,
			wantKVCacheUsage:              0.42,
		},
		{
			name:                          "SGLang counts one rank and reads the effective input tokens",
			engine:                        workercore.ModelDeploymentEngineSGLang,
			first:                         scalerSamplerSGLangExposition,
			second:                        scalerSamplerSGLangSecondExposition,
			window:                        19 * time.Second,
			wantPromptTokensPerSecond:     18000.0 / 19.0,
			wantGenerationTokensPerSecond: 3600.0 / 19.0,
			wantTTFTMilliseconds:          1000.0,
			wantITLMilliseconds:           50.0,
			wantRunning:                   7,
			wantWaiting:                   3,
			wantKVCacheUsage:              0.44,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
			target := scalerSamplerTarget(types.UID("target-uid"), tc.engine, scalerSamplerRole("server", workercore.ModelDeploymentRoleKindServer, 1))
			// The scaler samples the target no faster than its own interval, so the interval is
			// below the window the two passes are apart.
			scaler := scalerSamplerScaler(types.UID("scaler-uid"), 10)
			script := &scalerSamplerScripted{
				bodies:   map[string][]byte{"engine-0": []byte(tc.first)},
				failures: map[string]bool{},
				asked:    map[string]int{},
			}
			sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

			start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			first := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)
			require.Contains(t, first, "server")
			assert.Equal(t, algorithm.SignalFresh, first["server"].GaugeState,
				"the first pass reads the request gauges")
			assert.Equal(t, algorithm.SignalMissing, first["server"].ThroughputState,
				"the first pass has no window to read a rate from")
			assert.False(t, first["server"].CompleteCoverage,
				"the first pass cannot cover a rate it has no window for")

			script.mu.Lock()
			script.bodies["engine-0"] = []byte(tc.second)
			script.mu.Unlock()

			second := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(tc.window))
			require.Contains(t, second, "server")
			observation := second["server"]

			assert.Equal(t, algorithm.SignalFresh, observation.GaugeState)
			assert.Equal(t, algorithm.SignalFresh, observation.ThroughputState)
			assert.Equal(t, algorithm.SignalFresh, observation.LatencyState)
			assert.True(t, observation.CompleteCoverage,
				"one ready Pod answering twice covers the role")
			assert.InDelta(t, tc.wantRunning, observation.RunningRequests, 1e-9)
			assert.InDelta(t, tc.wantWaiting, observation.WaitingRequests, 1e-9)
			assert.InDelta(t, tc.wantKVCacheUsage, observation.KVCacheUsage, 1e-9)
			assert.InDelta(t, tc.wantPromptTokensPerSecond, observation.PromptTokensPerSecond, 1e-6)
			assert.InDelta(t, tc.wantGenerationTokensPerSecond, observation.GenerationTokensPerSecond, 1e-6)
			assert.InDelta(t, tc.wantTTFTMilliseconds, observation.TTFTMilliseconds, 1e-6)
			assert.InDelta(t, tc.wantITLMilliseconds, observation.ITLMilliseconds, 1e-6)
			assert.Equal(t, int32(1), observation.ReadyReplicas)
			assert.Equal(t, int32(1), observation.CurrentReplicas)
			assert.Equal(t, algorithm.RoleKindServer, observation.Kind)
		})
	}
}

// TestModelDeploymentScalerSampler_SGLangRankFreeHistogram reads an engine that reports no rank
// and splits its first-token latency by whether the request was streamed, and holds the
// sampler against the shape that would drop it.
func TestModelDeploymentScalerSampler_SGLangRankFreeHistogram(t *testing.T) {
	t.Parallel()

	pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
	target := scalerSamplerTarget(types.UID("target-uid"), workercore.ModelDeploymentEngineSGLang,
		scalerSamplerRole("server", workercore.ModelDeploymentRoleKindServer, 1))
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 10)
	script := &scalerSamplerScripted{
		bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerSGLangRankFreeExposition)},
		failures: map[string]bool{},
		asked:    map[string]int{},
	}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)
	script.mu.Lock()
	script.bodies["engine-0"] = []byte(scalerSamplerSGLangRankFreeSecondExposition)
	script.mu.Unlock()
	second := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(10*time.Second))

	observation := second["server"]
	assert.Equal(t, algorithm.SignalFresh, observation.LatencyState,
		"a rank-free histogram is read rather than dropped")
	// The first-token latency has no bucket boundaries, so the mean of the 117 observations is
	// the only figure that is true of them.
	wantTTFT := (27.006403835998753 + 1.3266321940018315) / 117.0 * 1000
	assert.InDelta(t, wantTTFT, observation.TTFTMilliseconds, 1e-6)
	wantITL := 15.15720793402943 / 2699.0 * 1000
	assert.InDelta(t, wantITL, observation.ITLMilliseconds, 1e-6)
}

// TestModelDeploymentScalerSampler_CounterResetAndRestart holds the sampler against the three
// ways a Pod's own history stops being continuous: a counter that went down, a container that
// restarted, and a Pod that was replaced by another one.
func TestModelDeploymentScalerSampler_CounterResetAndRestart(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
	target := scalerScalerTargetForTest()
	// The scaler samples the target no faster than its own interval, so the interval is no
	// longer than the window the two passes are apart.
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 10)

	t.Run("a counter that went down invalidates the window", func(t *testing.T) {
		t.Parallel()

		script := &scalerSamplerScripted{
			bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
			failures: map[string]bool{},
			asked:    map[string]int{},
		}
		sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}
		sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)

		// The prompt counter is lower than it was, which no counter does while its process lives.
		reset := scalerSamplerVLLMExposition
		reset = replaceExposition(reset, "vllm:prompt_tokens_total{engine=\"0\",model_name=\"Qwen/Qwen2.5-7B-Instruct\"} 120000.0",
			"vllm:prompt_tokens_total{engine=\"0\",model_name=\"Qwen/Qwen2.5-7B-Instruct\"} 1000.0")
		script.mu.Lock()
		script.bodies["engine-0"] = []byte(reset)
		script.mu.Unlock()

		observation := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(10*time.Second))["server"]
		assert.Equal(t, algorithm.SignalMissing, observation.ThroughputState,
			"a counter that went down is not a rate")
		assert.Equal(t, algorithm.SignalMissing, observation.LatencyState)
		assert.False(t, observation.CompleteCoverage)
		assert.Equal(t, algorithm.SignalFresh, observation.GaugeState,
			"the request gauges are still readable")
	})

	t.Run("a container that restarted invalidates the window", func(t *testing.T) {
		t.Parallel()

		script := &scalerSamplerScripted{
			bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
			failures: map[string]bool{},
			asked:    map[string]int{},
		}
		sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}
		sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)

		restarted := pod.DeepCopy()
		restarted.Status.ContainerStatuses[0].RestartCount = 1
		script.mu.Lock()
		script.bodies["engine-0"] = []byte(scalerSamplerVLLMSecondExposition)
		script.mu.Unlock()

		observation := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{*restarted}, start.Add(10*time.Second))["server"]
		assert.Equal(t, algorithm.SignalMissing, observation.ThroughputState,
			"a restarted container has no continuous window")
		assert.False(t, observation.CompleteCoverage)
		assert.Equal(t, algorithm.SignalFresh, observation.GaugeState)
	})

	t.Run("a replaced Pod invalidates the window and re-establishes the baseline", func(t *testing.T) {
		t.Parallel()

		script := &scalerSamplerScripted{
			bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
			failures: map[string]bool{},
			asked:    map[string]int{},
		}
		sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}
		sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)

		replacement := scalerSamplerPod("engine-0", "server", types.UID("other-pod-uid"), 0)
		observation := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{replacement}, start.Add(10*time.Second))["server"]
		assert.Equal(t, algorithm.SignalMissing, observation.ThroughputState,
			"a Pod that is not the one sampled before has no window")
		assert.False(t, observation.CompleteCoverage)
		assert.Equal(t, algorithm.SignalFresh, observation.GaugeState)

		observation = sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{replacement}, start.Add(20*time.Second))["server"]
		assert.Equal(t, algorithm.SignalFresh, observation.ThroughputState,
			"the next pass reads the window the replacement established")
		assert.True(t, observation.CompleteCoverage)
	})
}

// TestModelDeploymentScalerSampler_IdleWindow holds a deployment that serves nothing as idle
// rather than missing, so that it can scale down to its floor.
func TestModelDeploymentScalerSampler_IdleWindow(t *testing.T) {
	t.Parallel()

	pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
	target := scalerScalerTargetForTest()
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 15)
	script := &scalerSamplerScripted{
		bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerIdleExposition)},
		failures: map[string]bool{},
		asked:    map[string]int{},
	}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)
	observation := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(15*time.Second))["server"]

	assert.Equal(t, algorithm.SignalIdle, observation.GaugeState)
	assert.Equal(t, algorithm.SignalIdle, observation.ThroughputState)
	assert.Equal(t, algorithm.SignalIdle, observation.LatencyState)
	assert.True(t, observation.CompleteCoverage,
		"an idle role is fully covered: it was read, it was quiet")
	assert.Equal(t, 0.0, observation.RunningRequests)
	assert.Equal(t, 0.0, observation.WaitingRequests)
	assert.Equal(t, 0.0, observation.PromptTokensPerSecond)
	assert.Equal(t, 0.0, observation.GenerationTokensPerSecond)
	assert.Equal(t, 0.0, observation.TTFTMilliseconds)
	assert.Equal(t, 0.0, observation.ITLMilliseconds)
}

// TestModelDeploymentScalerSampler_PartialCoverage holds a role whose second Pod did not answer
// as incompletely covered, so that it is not scaled down on the strength of the first one.
func TestModelDeploymentScalerSampler_PartialCoverage(t *testing.T) {
	t.Parallel()

	first := scalerSamplerPod("engine-0", "server", types.UID("pod-uid-0"), 0)
	second := scalerSamplerPod("engine-1", "server", types.UID("pod-uid-1"), 1)
	target := scalerScalerTargetForTest()
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 15)
	script := &scalerSamplerScripted{
		bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
		failures: map[string]bool{"engine-1": true},
		asked:    map[string]int{},
	}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{first, second}, start)
	observation := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{first, second}, start.Add(15*time.Second))["server"]

	assert.False(t, observation.CompleteCoverage,
		"a Pod that did not answer leaves the role partly uncovered")
	assert.Equal(t, algorithm.SignalMissing, observation.ThroughputState)
	assert.Equal(t, algorithm.SignalMissing, observation.LatencyState)
	assert.Equal(t, int32(2), observation.ReadyReplicas)
}

// TestModelDeploymentScalerSampler_PodCap holds a role with more ready Pods than the sampler is
// willing to ask as incompletely covered.
func TestModelDeploymentScalerSampler_PodCap(t *testing.T) {
	t.Parallel()

	pods := make([]core.Pod, 0, maxSamplerPodsPerRole+1)
	bodies := map[string][]byte{}
	failures := map[string]bool{}
	asked := map[string]int{}
	for i := 0; i <= maxSamplerPodsPerRole; i++ {
		name := fmt.Sprintf("engine-%d", i)
		pods = append(pods, scalerSamplerPod(name, "server", types.UID(fmt.Sprintf("pod-uid-%d", i)), i))
		bodies[name] = []byte(scalerSamplerVLLMExposition)
	}
	target := scalerScalerTargetForTest()
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 15)
	script := &scalerSamplerScripted{bodies: bodies, failures: failures, asked: asked}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sampler.SampleRoleMetrics(context.Background(), scaler, target, pods, start)
	observation := sampler.SampleRoleMetrics(context.Background(), scaler, target, pods, start.Add(15*time.Second))["server"]

	assert.False(t, observation.CompleteCoverage,
		"a role with more ready Pods than the cap is not fully covered")
	assert.Equal(t, int32(maxSamplerPodsPerRole+1), observation.ReadyReplicas)
	assert.Equal(t, 0, script.askedCount("engine-64"),
		"the Pod beyond the cap is not asked about")
	assert.Equal(t, 2, script.askedCount("engine-0"),
		"the Pods within the cap are asked about on every pass")
}

// TestModelDeploymentScalerSampler_NonOverlappingPass holds that two passes over one target do
// not run at the same time.
func TestModelDeploymentScalerSampler_NonOverlappingPass(t *testing.T) {
	t.Parallel()

	pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
	target := scalerScalerTargetForTest()
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 15)

	release := make(chan struct{})
	script := &scalerSamplerScripted{
		bodies:     map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
		failures:   map[string]bool{},
		asked:      map[string]int{},
		blockUntil: release,
	}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	first := make(chan map[string]algorithm.RoleObservation, 1)
	go func() {
		first <- sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)
	}()

	// The first pass is inside its scrape when the second one asks.
	require.Eventually(t, func() bool {
		return script.askedCount("engine-0") == 1
	}, time.Second, time.Millisecond)

	second := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(15*time.Second))
	assert.Empty(t, second,
		"a pass that would overlap another one reports nothing rather than a reading of its own")
	assert.Equal(t, 1, script.askedCount("engine-0"),
		"the overlapping pass did not scrape the Pod a second time")

	close(release)
	<-first
}

// TestModelDeploymentScalerSampler_SamplingFloor holds that a target is not scraped again before
// the interval of its scaler has passed, because a pass costs one scrape per engine Pod.
func TestModelDeploymentScalerSampler_SamplingFloor(t *testing.T) {
	t.Parallel()

	pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
	target := scalerScalerTargetForTest()
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 15)
	script := &scalerSamplerScripted{
		bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
		failures: map[string]bool{},
		asked:    map[string]int{},
	}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)
	require.Equal(t, 1, script.askedCount("engine-0"))

	tooEarly := sampler.SampleRoleMetrics(
		context.Background(), scaler, target, []core.Pod{pod}, start.Add(14*time.Second))
	assert.Equal(t, 1, script.askedCount("engine-0"),
		"a pass before the interval of the scaler did not scrape the Pod again")
	require.Contains(t, tooEarly, "server")
	assert.Equal(t, algorithm.SignalMissing, tooEarly["server"].ThroughputState,
		"the refused pass reports the reading of the last pass")

	sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(15*time.Second))
	assert.Equal(t, 2, script.askedCount("engine-0"),
		"a pass after the interval of the scaler scraped the Pod again")
}

// TestModelDeploymentScalerSampler_Forget holds that a deleted scaler leaves nothing behind for
// the scaler that takes its place.
func TestModelDeploymentScalerSampler_Forget(t *testing.T) {
	t.Parallel()

	pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
	target := scalerScalerTargetForTest()
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 15)
	script := &scalerSamplerScripted{
		bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
		failures: map[string]bool{},
		asked:    map[string]int{},
	}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)
	sampler.Forget(scaler)

	observation := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(15*time.Second))["server"]
	assert.Equal(t, algorithm.SignalMissing, observation.ThroughputState,
		"the forgotten scaler has no window to read a rate from")
}

// TestModelDeploymentScalerSampler_ForgetKeepsTheTargetFloor holds that the floor on how often a
// target is sampled survives the scaler that is deleted, because the floor is the target's and
// another scaler of the same target still samples it under that floor.
func TestModelDeploymentScalerSampler_ForgetKeepsTheTargetFloor(t *testing.T) {
	t.Parallel()

	pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
	target := scalerScalerTargetForTest()
	deleted := scalerSamplerScaler(types.UID("scaler-uid-deleted"), 15)
	kept := scalerSamplerScaler(types.UID("scaler-uid-kept"), 15)
	script := &scalerSamplerScripted{
		bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
		failures: map[string]bool{},
		asked:    map[string]int{},
	}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sampler.SampleRoleMetrics(context.Background(), deleted, target, []core.Pod{pod}, start)
	sampler.SampleRoleMetrics(context.Background(), kept, target, []core.Pod{pod}, start.Add(15*time.Second))
	asked := script.askedCount("engine-0")

	sampler.Forget(deleted)
	sampler.SampleRoleMetrics(context.Background(), kept, target, []core.Pod{pod}, start.Add(16*time.Second))

	assert.Equal(t, asked, script.askedCount("engine-0"),
		"the target is still sampled under the floor of the scaler that was deleted")
}

// scalerScalerTargetForTest builds the target the tests that do not vary its shape share.
func scalerScalerTargetForTest() *workercore.ModelDeployment {
	return scalerSamplerTarget(types.UID("target-uid"), workercore.ModelDeploymentEngineVLLM,
		scalerSamplerRole("server", workercore.ModelDeploymentRoleKindServer, 1))
}

// replaceExposition swaps one line of an exposition for another.
func replaceExposition(exposition, old, replacement string) string {
	return strings.Replace(exposition, old, replacement, 1)
}

// TestModelDeploymentScalerSampler_RoleSamples holds that the sampler says why a signal is not
// the reading it would be, because an observation cannot carry the reason itself.
func TestModelDeploymentScalerSampler_RoleSamples(t *testing.T) {
	t.Parallel()

	first := scalerSamplerPod("engine-0", "server", types.UID("pod-uid-0"), 0)
	second := scalerSamplerPod("engine-1", "server", types.UID("pod-uid-1"), 1)
	target := scalerScalerTargetForTest()
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 15)
	script := &scalerSamplerScripted{
		bodies:   map[string][]byte{"engine-0": []byte(scalerSamplerVLLMExposition)},
		failures: map[string]bool{"engine-1": true},
		asked:    map[string]int{},
	}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{first, second}, start)
	sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{first, second}, start.Add(15*time.Second))

	samples := sampler.RoleSamples(scaler)
	require.Contains(t, samples, "server")
	sample := samples["server"]
	assert.NotEmpty(t, sample.Reasons, "the sampler says why the role is not fully covered")
	joined := strings.Join(sample.Reasons, "; ")
	assert.Contains(t, joined, "engine-1",
		"the reason names the pod that did not answer")
	assert.Contains(t, joined, "did not answer",
		"the reason says what the pod did not do")
	assert.Equal(t, 2, sample.ReadyPods)
	assert.InDelta(t, 15.0, sample.WindowSeconds, 1e-9,
		"the window the rates were read over is the time between the two passes")
	assert.Equal(t, int32(0), sample.ContainerRestarts)
}

// TestModelDeploymentScalerSampler_HTTPClientScrape reads a Pod through the sampler's own
// transport, so that the default scrape path is covered rather than only the injected one.
func TestModelDeploymentScalerSampler_HTTPClientScrape(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(scalerSamplerIdleExposition))
	}))
	defer server.Close()

	pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
	pod.Status.PodIP = "127.0.0.1"
	port := server.Listener.Addr().(*net.TCPAddr).Port
	pod.Annotations["prometheus.io/port"] = strconv.Itoa(port)
	pod.Spec.Containers[0].Ports[0].ContainerPort = int32(port)
	target := scalerScalerTargetForTest()
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 15)
	sampler := &modelDeploymentScalerSampler{HTTPClient: server.Client()}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	observation := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)["server"]
	assert.Equal(t, algorithm.SignalFresh, observation.GaugeState)
	assert.Equal(t, 0.0, observation.RunningRequests)
	assert.Equal(t, 0.0, observation.WaitingRequests)
}

// TestModelDeploymentScalerSampler_MetricsURL holds the address the sampler reads a Pod at to
// the annotations the render wrote, and refuses a Pod that advertises none.
func TestModelDeploymentScalerSampler_MetricsURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mutate   func(pod *core.Pod)
		wantURL  string
		wantRead bool
	}{
		{
			name:     "the advertised port and path are read",
			mutate:   func(*core.Pod) {},
			wantURL:  "http://10.0.0.1:8000/metrics",
			wantRead: true,
		},
		{
			name: "a Pod without an address is not read",
			mutate: func(pod *core.Pod) {
				pod.Status.PodIP = ""
			},
			wantRead: false,
		},
		{
			name: "a Pod advertising an unusable port is not read",
			mutate: func(pod *core.Pod) {
				pod.Annotations["prometheus.io/port"] = "0"
			},
			wantRead: false,
		},
		{
			name: "a Pod advertising a path that is not a path is not read",
			mutate: func(pod *core.Pod) {
				pod.Annotations["prometheus.io/path"] = "metrics"
			},
			wantRead: false,
		},
		{
			name: "a Pod that does not opt into scraping is not read",
			mutate: func(pod *core.Pod) {
				pod.Annotations["prometheus.io/scrape"] = "false"
			},
			wantRead: false,
		},
		{
			name: "a Pod advertising a port its containers do not declare is not read",
			mutate: func(pod *core.Pod) {
				pod.Annotations["prometheus.io/port"] = "9090"
			},
			wantRead: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pod := scalerSamplerPod("engine-0", "server", types.UID("pod-uid"), 0)
			tc.mutate(&pod)
			url, ok := modelDeploymentScalerMetricsURL(&pod)
			assert.Equal(t, tc.wantRead, ok)
			if tc.wantRead {
				assert.Equal(t, tc.wantURL, url)
			}
		})
	}
}

// TestModelDeploymentScalerSampler_StaleWindowAndDecodeTransfer covers stale windows past twice the
// sampling interval and SGLang decode transfer queue waiting requests on Decode roles.
func TestModelDeploymentScalerSampler_StaleWindowAndDecodeTransfer(t *testing.T) {
	t.Parallel()

	pod := scalerSamplerPod("decode-0", "decode", types.UID("pod-uid"), 0)
	target := scalerSamplerTarget(types.UID("target-uid"), workercore.ModelDeploymentEngineSGLang,
		scalerSamplerRole("decode", workercore.ModelDeploymentRoleKindDecode, 1))
	scaler := scalerSamplerScaler(types.UID("scaler-uid"), 15)
	firstBody := scalerSamplerSGLangExposition + "\n# TYPE sglang:num_decode_transfer_queue_reqs gauge\nsglang:num_decode_transfer_queue_reqs{moe_ep_rank=\"0\",pp_rank=\"0\",tp_rank=\"0\"} 5.0\nsglang:num_decode_transfer_queue_reqs{moe_ep_rank=\"0\",pp_rank=\"1\",tp_rank=\"0\"} 99.0\n"
	secondBody := scalerSamplerSGLangSecondExposition + "\n# TYPE sglang:num_decode_transfer_queue_reqs gauge\nsglang:num_decode_transfer_queue_reqs{moe_ep_rank=\"0\",pp_rank=\"0\",tp_rank=\"0\"} 6.0\nsglang:num_decode_transfer_queue_reqs{moe_ep_rank=\"0\",pp_rank=\"1\",tp_rank=\"0\"} 99.0\n"
	script := &scalerSamplerScripted{
		bodies:   map[string][]byte{"decode-0": []byte(firstBody)},
		failures: map[string]bool{},
		asked:    map[string]int{},
	}
	sampler := &modelDeploymentScalerSampler{Scrape: script.scrape}

	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	first := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start)["decode"]
	assert.Equal(t, 7.0, first.WaitingRequests,
		"a decode role adds the rank-zero decode transfer queue to the waiting queue and ignores non-zero pp_rank")

	script.mu.Lock()
	script.bodies["decode-0"] = []byte(secondBody)
	script.mu.Unlock()

	stale := sampler.SampleRoleMetrics(context.Background(), scaler, target, []core.Pod{pod}, start.Add(31*time.Second))["decode"]
	assert.Equal(t, algorithm.SignalFresh, stale.GaugeState)
	assert.Equal(t, 9.0, stale.WaitingRequests)
	assert.Equal(t, algorithm.SignalStale, stale.ThroughputState,
		"a window older than twice the sampling interval is marked stale")
	assert.Equal(t, algorithm.SignalStale, stale.LatencyState,
		"a latency window older than twice the sampling interval is marked stale")
	assert.False(t, stale.CompleteCoverage,
		"a stale window does not count as complete coverage for scale-down")
}
