package worker

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// storeOperation is one (operation, status) child of vLLM's Mooncake store families.
type storeOperation struct {
	operation string
	status    string
	calls     int
}

// storeExposition writes the five Mooncake store families the way vLLM's client does: each family
// once with its HELP and TYPE lines, then one labeled child per (operation, status) row. A child
// carries the operation counter, the key, byte and failed-key counters, and a histogram. A row of
// zero calls writes no series at all, because the client reaches `.labels()` only from an
// observation path.
func storeExposition(rows ...storeOperation) string {
	var counters, keys, bytesTotal, failedKeys, histogram []string
	for _, row := range rows {
		if row.calls == 0 {
			continue
		}
		labels := fmt.Sprintf(`engine_idx="0",operation=%q,status=%q`, row.operation, row.status)
		counters = append(counters, fmt.Sprintf("vllm:mooncake_store_operation_total{%s} %d", labels, row.calls))
		keys = append(keys, fmt.Sprintf("vllm:mooncake_store_operation_keys_total{%s} %d", labels, row.calls))
		bytesTotal = append(bytesTotal,
			fmt.Sprintf("vllm:mooncake_store_operation_bytes_total{%s} %d", labels, row.calls*3670016))
		failedKeys = append(failedKeys,
			fmt.Sprintf("vllm:mooncake_store_operation_failed_keys_total{%s} %d", labels, row.calls))
		for _, le := range []string{"0.001", "0.01", "0.1", "1.0", "+Inf"} {
			histogram = append(histogram, fmt.Sprintf(
				"vllm:mooncake_store_operation_time_seconds_bucket{%s,le=%q} %d", labels, le, row.calls,
			))
		}
		histogram = append(histogram,
			fmt.Sprintf("vllm:mooncake_store_operation_time_seconds_sum{%s} 0.012", labels),
			fmt.Sprintf("vllm:mooncake_store_operation_time_seconds_count{%s} %d", labels, row.calls))
	}

	var b strings.Builder
	b.WriteString("# HELP vllm:num_requests_running Number of requests currently running on GPU.\n")
	b.WriteString("# TYPE vllm:num_requests_running gauge\n")
	b.WriteString("vllm:num_requests_running{model_name=\"qwen\"} 0\n")
	family := func(name, help, typ string, samples []string) {
		if len(samples) == 0 {
			return
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s\n", name, help, name, typ, strings.Join(samples, "\n"))
	}
	family("vllm:mooncake_store_operation_total",
		"Number of Mooncake store communication operations.", "counter", counters)
	family("vllm:mooncake_store_operation_keys_total",
		"Number of Mooncake store keys touched by operations.", "counter", keys)
	family("vllm:mooncake_store_operation_bytes_total",
		"Number of bytes transferred by Mooncake store operations.", "counter", bytesTotal)
	family("vllm:mooncake_store_operation_failed_keys_total",
		"Number of Mooncake store keys that failed in operations.", "counter", failedKeys)
	family("vllm:mooncake_store_operation_time_seconds",
		"Histogram of Mooncake store communication time.", "histogram", histogram)

	return b.String()
}

// TestModelDeploymentCacheScraper_Table reads the verdict off real exposition text. The verdict
// counts TRANSFER operations alone (save_put and load_get): in the failure this condition exists
// for, every lookup and save_exists succeeds over the master's TCP RPC while every transfer is
// refused, so a verdict over all operations would read healthy.
func TestModelDeploymentCacheScraper_Table(t *testing.T) {
	testCases := []struct {
		name          string
		exposition    string
		wantReading   ModelDeploymentCacheReading
		wantAttempted float64
		wantFailed    float64
	}{
		{
			name: "every transfer operation failed",
			exposition: storeExposition(
				storeOperation{"save_exists", "ok", 2}, storeOperation{"save_put", "error", 3},
				storeOperation{"lookup_lease", "ok", 2}, storeOperation{"load_get", "error", 2},
			),
			wantReading: ModelDeploymentCacheFailing, wantAttempted: 5, wantFailed: 5,
		},
		{
			name:          "one transfer succeeded",
			exposition:    storeExposition(storeOperation{"save_put", "error", 2}, storeOperation{"save_put", "ok", 1}),
			wantReading:   ModelDeploymentCacheActive,
			wantAttempted: 3,
			wantFailed:    2,
		},
		{
			name:          "only partial failures",
			exposition:    storeExposition(storeOperation{"save_put", "partial_failure", 2}),
			wantReading:   ModelDeploymentCacheActive,
			wantAttempted: 2,
			wantFailed:    0,
		},
		{
			// The issue's shape: lookups and save_exists answer over TCP, so status is ok there,
			// while every byte-moving operation is refused.
			name: "lookups succeed while every transfer fails",
			exposition: storeExposition(
				storeOperation{"lookup_exists", "ok", 4}, storeOperation{"lookup_lease", "ok", 4},
				storeOperation{"save_exists", "ok", 2}, storeOperation{"save_put", "error", 2},
			),
			wantReading: ModelDeploymentCacheFailing, wantAttempted: 2, wantFailed: 2,
		},
		{
			name:          "lookups alone never make a replica active",
			exposition:    storeExposition(storeOperation{"lookup_exists", "ok", 1}, storeOperation{"save_exists", "ok", 1}),
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 0,
			wantFailed:    0,
		},
		{
			name:          "an idle scrape carries no store series",
			exposition:    storeExposition(),
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 0,
			wantFailed:    0,
		},
		{
			name:          "an empty exposition",
			exposition:    "",
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 0,
			wantFailed:    0,
		},
		{
			// Counters are cumulative since process start, so one error followed by thousands of
			// successes reads active and never latches failing.
			name:          "an early error followed by successes",
			exposition:    storeExposition(storeOperation{"save_put", "error", 1}, storeOperation{"save_put", "ok", 50}),
			wantReading:   ModelDeploymentCacheActive,
			wantAttempted: 51,
			wantFailed:    1,
		},
		{
			// A status outside the measured vocabulary is an account this reader cannot classify.
			name:          "an unmeasured status stays unreadable",
			exposition:    storeExposition(storeOperation{"save_put", "timeout", 2}),
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 2,
			wantFailed:    0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := modelDeploymentCacheAccount([]byte(tc.exposition))
			require.NoError(t, err)
			assert.Equal(t, tc.wantReading, got.Reading)
			assert.Equal(t, tc.wantAttempted, got.Attempted)
			assert.Equal(t, tc.wantFailed, got.Failed)
		})
	}
}

// scrapeReplica is a Ready replica of the fixture deployment whose metrics endpoint is the test
// server's, reached over the given scheme.
func scrapeReplica(t *testing.T, md *workercore.ModelDeployment, url, scheme string) core.Pod {
	t.Helper()

	pod := readyReplica(md, 0, true)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"))
	require.NoError(t, err)
	pod.Status.PodIP = host
	pod.Annotations["prometheus.io/port"] = port
	pod.Annotations["prometheus.io/scheme"] = scheme

	return *pod
}

// TestModelDeploymentCacheScraper_ScrapeCache covers the transport around the parse: an endpoint
// that refuses is an error, and an engine behind a self-signed certificate is still read.
func TestModelDeploymentCacheScraper_ScrapeCache(t *testing.T) {
	failing := storeExposition(storeOperation{"save_put", "error", 3})
	scraper := newModelDeploymentMetricsCacheScraper()
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 1 })

	t.Run("http_500_is_unreadable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		pod := scrapeReplica(t, md, server.URL, "http")
		got, err := scraper.ScrapeCache(t.Context(), &pod)
		require.Error(t, err)
		assert.Equal(t, ModelDeploymentCacheUnreadable, got.Reading)
	})

	t.Run("a_self_signed_certificate_is_read", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(failing))
		}))
		defer server.Close()

		pod := scrapeReplica(t, md, server.URL, "https")
		got, err := scraper.ScrapeCache(t.Context(), &pod)
		require.NoError(t, err)
		assert.Equal(t, ModelDeploymentCacheFailing, got.Reading)
		assert.Equal(t, float64(3), got.Attempted)
		assert.Equal(t, float64(3), got.Failed)
	})

	t.Run("a_replica_with_no_address_is_an_error", func(t *testing.T) {
		got, err := scraper.ScrapeCache(t.Context(), &core.Pod{})
		require.Error(t, err)
		assert.Equal(t, ModelDeploymentCacheUnreadable, got.Reading)
	})
}

// TestModelDeploymentCacheScraper_ANilScraperIsTheProductionOne pins the wiring by behavior: a
// reconciler built without a scraper reads through the production one, so a replica that answers
// with failing transfers reports CacheOperationsFailing with nobody assigning a scraper.
func TestModelDeploymentCacheScraper_ANilScraperIsTheProductionOne(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(storeExposition(storeOperation{"save_put", "error", 3})))
	}))
	defer server.Close()

	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 1 })
	pod := scrapeReplica(t, md, server.URL, "http")

	got := cacheAttachedOf(t, md, []core.Pod{pod}, readyDomain(), nil)

	assert.EqualValues(t, 1, hits.Load(), "the production scraper asked the replica")
	assert.Equal(t, modelDeploymentReasonCacheOperationsFailing,
		ModelDeploymentConditionCacheAttached.GetReason(got))
}

// TestModelDeploymentCacheScraper_RequestShape pins what the scraper asks for, since a wrong path or
// method would read an engine's 404 page as an empty exposition.
func TestModelDeploymentCacheScraper_RequestShape(t *testing.T) {
	var path, method string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		_, _ = w.Write([]byte(storeExposition(storeOperation{"load_get", "ok", 1})))
	}))
	defer server.Close()

	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 1 })
	pod := scrapeReplica(t, md, server.URL, "http")

	got, err := newModelDeploymentMetricsCacheScraper().ScrapeCache(t.Context(), &pod)
	require.NoError(t, err)

	assert.Equal(t, "/metrics", path)
	assert.Equal(t, http.MethodGet, method)
	assert.Equal(t, ModelDeploymentCacheActive, got.Reading)
}
