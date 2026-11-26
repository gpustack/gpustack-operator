package worker

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// storeOperation is one (operation, status) child of vLLM's Mooncake store families: the operation
// records it holds and, cumulative over them, the keys they touched and the keys that failed.
type storeOperation struct {
	operation  string
	status     string
	calls      int
	keys       int
	failedKeys int
}

// saved is the return path with no failed key.
func saved(operation string, calls, keys int) storeOperation {
	return storeOperation{operation: operation, status: "ok", calls: calls, keys: keys}
}

// refusedKeys is the return path with EVERY key failed. vLLM v0.25.1 and v0.30.0 record it as
// partial_failure, not error (worker.py: `status="partial_failure" if failed else "ok"`), and so does
// main for save_put and load_get.
func refusedKeys(operation string, calls, keys int) storeOperation {
	return storeOperation{operation: operation, status: "partial_failure", calls: calls, keys: keys, failedKeys: keys}
}

// partlySaved is the return path with some keys failed and some not.
func partlySaved(operation string, calls, keys, failed int) storeOperation {
	return storeOperation{operation: operation, status: "partial_failure", calls: calls, keys: keys, failedKeys: failed}
}

// raised is the exception path: every version records error and counts every key as failed.
func raised(operation string, calls, keys int) storeOperation {
	return storeOperation{operation: operation, status: "error", calls: calls, keys: keys, failedKeys: keys}
}

// storeExposition writes the five Mooncake store families the way vLLM's client does: each family
// once with its HELP and TYPE lines, then one labeled child per (operation, status) row. A child
// carries the operation counter, the key, byte and failed-key counters, and a histogram whose count
// is the operation counter. A row of zero calls writes no series at all, because the client reaches
// `.labels()` only from an observation path.
func storeExposition(rows ...storeOperation) string {
	var counters, keys, bytesTotal, failedKeys, histogram []string
	for _, row := range rows {
		if row.calls == 0 {
			continue
		}
		labels := fmt.Sprintf(`engine_idx="0",operation=%q,status=%q`, row.operation, row.status)
		counters = append(counters, fmt.Sprintf("vllm:mooncake_store_operation_total{%s} %d", labels, row.calls))
		keys = append(keys, fmt.Sprintf("vllm:mooncake_store_operation_keys_total{%s} %d", labels, row.keys))
		bytesTotal = append(bytesTotal,
			fmt.Sprintf("vllm:mooncake_store_operation_bytes_total{%s} %d", labels, row.keys*3670016))
		failedKeys = append(failedKeys,
			fmt.Sprintf("vllm:mooncake_store_operation_failed_keys_total{%s} %d", labels, row.failedKeys))
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

// TestModelDeploymentCacheScraper_Table reads the verdict off real exposition text.
//
// TRANSFER OPERATIONS ALONE ARE COUNTED (save_put and load_get), because in the failure this
// condition exists for every lookup and save_exists succeeds over the master's TCP RPC while every
// transfer is refused.
//
// THE VERDICT IS KEY-LEVEL. On the pinned images a transfer whose every key failed is recorded as
// partial_failure, so a reader that counts that status as success reports the failure as active. A
// replica is active when at least one key succeeded or an operation completed with no failed key,
// and failing when no key ever did.
func TestModelDeploymentCacheScraper_Table(t *testing.T) {
	testCases := []struct {
		name          string
		exposition    string
		wantReading   ModelDeploymentCacheReading
		wantAttempted float64
		wantKeys      float64
		wantFailed    float64
	}{
		{
			// THE PINNED-IMAGE SHAPE from the issue: lookups and save_exists answer ok over TCP,
			// and batch_put returns one refused result per key, which worker.py records as
			// partial_failure with num_failed_keys == num_keys.
			name: "the pinned image: every save_put key refused under partial_failure",
			exposition: storeExposition(
				saved("lookup_exists", 4, 8), saved("lookup_lease", 4, 8), saved("save_exists", 2, 2),
				refusedKeys("save_put", 2, 2), refusedKeys("load_get", 1, 4),
			),
			wantReading: ModelDeploymentCacheFailing, wantAttempted: 3, wantKeys: 6, wantFailed: 6,
		},
		{
			name:          "partial failure with some keys saved",
			exposition:    storeExposition(partlySaved("save_put", 2, 10, 4)),
			wantReading:   ModelDeploymentCacheActive,
			wantAttempted: 2,
			wantKeys:      10,
			wantFailed:    4,
		},
		{
			// The exception path, which every version records as error with every key failed.
			name:          "error operations with every key failed",
			exposition:    storeExposition(raised("save_put", 3, 3)),
			wantReading:   ModelDeploymentCacheFailing,
			wantAttempted: 3,
			wantKeys:      3,
			wantFailed:    3,
		},
		{
			name:          "every key failed across partial and error operations",
			exposition:    storeExposition(refusedKeys("save_put", 2, 2), raised("load_get", 1, 4)),
			wantReading:   ModelDeploymentCacheFailing,
			wantAttempted: 3,
			wantKeys:      6,
			wantFailed:    6,
		},
		{
			// No key succeeded and no operation completed clean, so nothing positive was seen.
			name:          "an operation with no key and no clean operation",
			exposition:    storeExposition(partlySaved("save_put", 1, 0, 0)),
			wantReading:   ModelDeploymentCacheFailing,
			wantAttempted: 1,
			wantKeys:      0,
			wantFailed:    0,
		},
		{
			name:          "an ok operation with no key",
			exposition:    storeExposition(saved("save_put", 1, 0)),
			wantReading:   ModelDeploymentCacheActive,
			wantAttempted: 1,
			wantKeys:      0,
			wantFailed:    0,
		},
		{
			name: "lookups alone never make a replica active",
			exposition: storeExposition(
				saved("lookup_exists", 4, 8), saved("lookup_lease", 4, 8), saved("save_exists", 2, 2),
			),
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 0,
			wantKeys:      0,
			wantFailed:    0,
		},
		{
			name:          "an idle scrape carries no store series",
			exposition:    storeExposition(),
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 0,
			wantKeys:      0,
			wantFailed:    0,
		},
		{
			name:          "an empty exposition",
			exposition:    "",
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 0,
			wantKeys:      0,
			wantFailed:    0,
		},
		{
			// Counters are cumulative since process start, so one failure followed by thousands of
			// saved keys reads active and never latches failing.
			name:          "an early failure followed by saved keys",
			exposition:    storeExposition(raised("save_put", 1, 1), saved("save_put", 50, 50)),
			wantReading:   ModelDeploymentCacheActive,
			wantAttempted: 51,
			wantKeys:      51,
			wantFailed:    1,
		},
		{
			name:          "an unmeasured status stays unreadable",
			exposition:    storeExposition(storeOperation{"save_put", "timeout", 2, 2, 2}),
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 2,
			wantKeys:      0,
			wantFailed:    0,
		},
		{
			// Without the failed-key counter the rule cannot tell a refused key from a saved one, and
			// reading that as active is the one answer that would hide the failure.
			name: "transfer operations beside no failed-key counter",
			exposition: "# TYPE vllm:mooncake_store_operation_total counter\n" +
				"vllm:mooncake_store_operation_total{operation=\"save_put\",status=\"partial_failure\"} 2\n" +
				"# TYPE vllm:mooncake_store_operation_keys_total counter\n" +
				"vllm:mooncake_store_operation_keys_total{operation=\"save_put\",status=\"partial_failure\"} 2\n",
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 2,
			wantKeys:      2,
			wantFailed:    0,
		},
		{
			name: "transfer operations beside no key counter",
			exposition: "# TYPE vllm:mooncake_store_operation_total counter\n" +
				"vllm:mooncake_store_operation_total{operation=\"save_put\",status=\"partial_failure\"} 2\n" +
				"# TYPE vllm:mooncake_store_operation_failed_keys_total counter\n" +
				"vllm:mooncake_store_operation_failed_keys_total{operation=\"save_put\",status=\"partial_failure\"} 2\n",
			wantReading:   ModelDeploymentCacheUnreadable,
			wantAttempted: 2,
			wantKeys:      0,
			wantFailed:    2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := modelDeploymentCacheAccount([]byte(tc.exposition))
			require.NoError(t, err)
			assert.Equal(t, tc.wantReading, got.Reading)
			assert.Equal(t, tc.wantAttempted, got.Attempted)
			assert.Equal(t, tc.wantKeys, got.Keys)
			assert.Equal(t, tc.wantFailed, got.Failed)
		})
	}
}

// scrapeReplica is a Ready replica of the fixture deployment whose metrics endpoint is the test
// server's, reached over the given scheme.
func scrapeReplica(t *testing.T, md *workercore.ModelDeployment, url, scheme string) core.Pod {
	t.Helper()

	pod := readyReplica(md, 0, true)
	pointReplicaAt(t, pod, url, scheme)

	return *pod
}

// pointReplicaAt aims one Ready replica's metrics endpoint at a test server.
func pointReplicaAt(t *testing.T, pod *core.Pod, url, scheme string) {
	t.Helper()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"))
	require.NoError(t, err)
	pod.Status.PodIP = host
	pod.Annotations["prometheus.io/port"] = port
	pod.Annotations["prometheus.io/scheme"] = scheme
}

// TestModelDeploymentCacheScraper_ScrapeCache covers the transport around the parse: an endpoint
// that refuses is an error, and an engine behind a self-signed certificate is still read.
func TestModelDeploymentCacheScraper_ScrapeCache(t *testing.T) {
	failing := storeExposition(refusedKeys("save_put", 3, 3))
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
		_, _ = w.Write([]byte(storeExposition(refusedKeys("save_put", 3, 3))))
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
		_, _ = w.Write([]byte(storeExposition(saved("load_get", 1, 1))))
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

// TestModelDeploymentCacheScraper_RendersStoreIsInvariantAcrossTheVLLMEngines pins the equality the
// scrape set relies on.
//
// ModelDeploymentRoleRendersStore asks inject.RendersStore for the plain vLLM engine because the
// pool's manufacturer is not resolved on that path, and vLLM-Ascend is selected by the manufacturer
// alone. The answer is therefore right for an Ascend replica only while the predicate does not
// distinguish the two engines. Every cell of role, point-to-point leg and pool address is compared,
// so a divergence fails here instead of silently skewing which replicas are asked.
func TestModelDeploymentCacheScraper_RendersStoreIsInvariantAcrossTheVLLMEngines(t *testing.T) {
	roles := []inject.Role{inject.RoleNone, inject.RolePrefill, inject.RoleDecode, inject.RoleEncode}
	legs := []bool{false, true}
	addresses := []string{"", "master:50051"}

	cells := 0
	for _, role := range roles {
		for _, kvTransfer := range legs {
			for _, address := range addresses {
				input := inject.Input{
					Role:       role,
					KVTransfer: kvTransfer,
					Connection: inject.Connection{MasterAddress: address},
				}
				input.Engine = inject.EngineVLLM
				plain := inject.RendersStore(input)
				input.Engine = inject.EngineVLLMAscend
				ascend := inject.RendersStore(input)

				assert.Equal(t, plain, ascend,
					"role %q, kvTransfer %t, master address %q: the scrape set asks the plain engine for both",
					role, kvTransfer, address)
				cells++
			}
		}
	}
	assert.Equal(t, len(roles)*len(legs)*len(addresses), cells)

	// The baseline every cell is compared against, so the matrix is known to contain both answers.
	assert.True(t, inject.RendersStore(inject.Input{
		Engine:     inject.EngineVLLM,
		Connection: inject.Connection{MasterAddress: "master:50051"},
	}))
	assert.False(t, inject.RendersStore(inject.Input{
		Engine: inject.EngineVLLM, Role: inject.RoleEncode,
		Connection: inject.Connection{MasterAddress: "master:50051"},
	}))
}

// TestModelDeploymentCacheScraper_ReplicasAreScrapedTogether keeps one hung endpoint from holding
// the pass for every replica's timeout.
//
// The pass requeues every modelDeploymentCacheRequeue, and its reconcile is one of a bounded number
// the controller runs at a time, so a serial scrape of N replicas is bounded by N timeouts in the
// worst case. Four replicas here never answer until the client gives up, one answers slowly and one
// at once. Scraped together the pass costs about one timeout.
//
// THE ORDER OF THE MESSAGE IS THE ORDER OF THE REPLICAS, not the order in which they answered: the
// slow one is listed first and the fast one last, so an implementation that appended as scrapes
// completed would list them the other way round. A scrape that fails or times out is Unreadable for
// that replica alone.
func TestModelDeploymentCacheScraper_ReplicasAreScrapedTogether(t *testing.T) {
	const hung, slowAnswer, replicas = 4, 300 * time.Millisecond, 6

	hungServers := make([]*httptest.Server, hung)
	for i := range hungServers {
		// Hangs until the client gives up and the connection goes away.
		hungServers[i] = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
	}
	defer func() {
		for _, server := range hungServers {
			server.Close()
		}
	}()

	var slowHits, fastHits atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(slowAnswer)
		slowHits.Add(1)
		_, _ = w.Write([]byte(storeExposition(refusedKeys("save_put", 3, 3))))
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fastHits.Add(1)
		_, _ = w.Write([]byte(storeExposition(refusedKeys("save_put", 7, 7))))
	}))
	defer fast.Close()

	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = replicas })
	pods := readyPods(md, replicas)
	pointReplicaAt(t, &pods[0], slow.URL, "http")
	for i := 1; i <= hung; i++ {
		pointReplicaAt(t, &pods[i], hungServers[i-1].URL, "http")
	}
	pointReplicaAt(t, &pods[replicas-1], fast.URL, "http")

	started := time.Now()
	got := cacheAttachedOf(t, md, pods, readyDomain(), nil)
	elapsed := time.Since(started)

	// GENEROUS ON PURPOSE: the pass costs one timeout when the scrapes run together, and four when
	// they run one after another, so the bound sits at three and cannot flake on a slow machine.
	assert.Less(t, elapsed, 3*samplerPodTimeout,
		"one hung endpoint must not hold the pass for every replica's timeout")
	assert.EqualValues(t, 1, slowHits.Load())
	assert.EqualValues(t, 1, fastHits.Load())

	message := ModelDeploymentConditionCacheAttached.GetMessage(got)
	assert.Equal(t, modelDeploymentReasonCacheOperationsFailing,
		ModelDeploymentConditionCacheAttached.GetReason(got))
	assert.Contains(t, message, "2 of 6 ready replicas report store transfers of which every key failed")
	assert.Contains(t, message, "; 4 gave no account")

	slowSegment := strings.Index(message, `qwen-server-0 (role "server", resources.interface: unset): 3 transfer operations attempted`)
	fastSegment := strings.Index(message, `qwen-server-5 (role "server", resources.interface: unset): 7 transfer operations attempted`)
	assert.GreaterOrEqual(t, slowSegment, 0, "the slow replica is rendered")
	assert.GreaterOrEqual(t, fastSegment, 0, "the fast replica is rendered")
	assert.Less(t, slowSegment, fastSegment, "replicas are listed in replica order, not in the order they answered")
}
