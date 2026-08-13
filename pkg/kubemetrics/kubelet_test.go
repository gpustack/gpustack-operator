package kubemetrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	kubeletstats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/json"
)

const testNode = "node-1"

// apiHandler dispatches the fake API server: the reads below go through the loopback
// Kubernetes client, so the tests stand one up for it. Every store goes through
// http.HandlerFunc, the one concrete type an atomic.Value accepts consistently.
var apiHandler atomic.Value

func TestMain(m *testing.M) {
	apiHandler.Store(http.NotFoundHandler())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHandler.Load().(http.Handler).ServeHTTP(w, r)
	}))

	cfg := &rest.Config{Host: srv.URL}
	system.LoopbackKubeRestConfig.Configure(*cfg)
	system.LoopbackKubeClient.Configure(kubernetes.NewForConfigOrDie(cfg))

	code := m.Run()
	srv.Close()
	os.Exit(code)
}

// serveAPI installs the fake API server's routes for the duration of a test, and empties the
// node readout cache on both ends so tests never inherit each other's readouts.
func serveAPI(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	resetPodStatsCache()
	apiHandler.Store(http.HandlerFunc(mux.ServeHTTP))
	t.Cleanup(func() {
		apiHandler.Store(http.NotFoundHandler())
		resetPodStatsCache()
	})
}

func resetPodStatsCache() {
	podStatsCache.mu.Lock()
	defer podStatsCache.mu.Unlock()
	podStatsCache.entries = map[string]podStatsEntry{}
}

// nodeProxyMux answers the node-proxy kubelet read of the test node.
func nodeProxyMux(h http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes/"+testNode+"/proxy/stats/summary", h)
	return mux
}

// servePods answers the node-proxy read with a summary carrying the given pod entries,
// counting how many times the remote was actually hit.
func servePods(t *testing.T, pods []kubeletstats.PodStats) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	serveAPI(t, nodeProxyMux(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		bs, _ := json.Marshal(&kubeletstats.Summary{Pods: pods})
		_, _ = w.Write(bs)
	}))
	return &calls
}

// testPod is the pod every case asks about.
func testPod() *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default", Name: "inst", UID: types.UID("pod-uid-1"),
		},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name: "main",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						core.ResourceCPU:              resource.MustParse("2"),
						core.ResourceMemory:           resource.MustParse("4Gi"),
						core.ResourceEphemeralStorage: resource.MustParse("10Gi"),
					},
				},
			}},
		},
	}
}

// testPodStats is the kubelet entry of testPod.
func testPodStats() kubeletstats.PodStats {
	return kubeletstats.PodStats{
		PodRef: kubeletstats.PodReference{Namespace: "default", Name: "inst", UID: "pod-uid-1"},
		CPU:    &kubeletstats.CPUStats{Time: meta.Now(), UsageNanoCores: ptr.To[uint64](500_000_000)},
		Memory: &kubeletstats.MemoryStats{WorkingSetBytes: ptr.To[uint64](1 << 30)},
	}
}

// serveKubeletAndMetricsAPI answers both sources: the kubelet with the given pod entries (nil
// meaning it answers but carries nothing), and metrics.k8s.io with the given body and status.
func serveKubeletAndMetricsAPI(
	t *testing.T,
	pods []kubeletstats.PodStats,
	kubeletDown bool,
	podMetricsBody string,
	podMetricsStatus int,
) {
	t.Helper()
	mux := nodeProxyMux(func(w http.ResponseWriter, _ *http.Request) {
		if kubeletDown {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		bs, _ := json.Marshal(&kubeletstats.Summary{Pods: pods})
		_, _ = w.Write(bs)
	})
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/namespaces/default/pods/inst",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(podMetricsStatus)
			_, _ = w.Write([]byte(podMetricsBody))
		})
	serveAPI(t, mux)
}

func TestFetchPodSample(t *testing.T) {
	t.Run("serves the kubelet's measurement", func(t *testing.T) {
		servePods(t, []kubeletstats.PodStats{testPodStats()})

		sample, err := FetchPodSample(context.Background(), testNode, testPod(), 0)
		require.NoError(t, err)
		assert.Equal(t, uint64(500), *sample.CPUUsedMilliCores)
		assert.Equal(t, uint64(1024), *sample.MemoryUsedMiB)
		assert.Equal(t, uint64(2000), sample.CPUTotalMilliCores)
	})

	t.Run("degrades to the metrics API when the kubelet is unreachable", func(t *testing.T) {
		serveKubeletAndMetricsAPI(t, nil, true,
			podMetricsPayload(time.Now(), "250m", "512Mi"), http.StatusOK)

		sample, err := FetchPodSample(context.Background(), testNode, testPod(), 0)
		require.NoError(t, err)
		assert.Equal(t, uint64(250), *sample.CPUUsedMilliCores)
		assert.Equal(t, uint64(512), *sample.MemoryUsedMiB)
		assert.Nil(t, sample.StorageUsedMiB, "the degraded source carries no storage figures")
		// The totals come from the Instance's declaration, so they survive the degradation.
		assert.Equal(t, uint64(10240), sample.StorageTotalMiB)
	})

	t.Run("degrades when the kubelet answers without carrying the pod", func(t *testing.T) {
		// Its stats provider has not caught up after a restart. A sample stamped with a
		// measurement that never happened would be worse than degraded CPU/memory figures.
		serveKubeletAndMetricsAPI(t, nil, false,
			podMetricsPayload(time.Now(), "250m", "512Mi"), http.StatusOK)

		sample, err := FetchPodSample(context.Background(), testNode, testPod(), 0)
		require.NoError(t, err)
		assert.Equal(t, uint64(250), *sample.CPUUsedMilliCores)
	})

	t.Run("reports both sources when the metrics API fails too", func(t *testing.T) {
		serveKubeletAndMetricsAPI(t, nil, true, "{not-json", http.StatusOK)

		_, err := FetchPodSample(context.Background(), testNode, testPod(), 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "from the metrics API")
	})

	t.Run("names an unserved metrics API rather than a nil error", func(t *testing.T) {
		serveKubeletAndMetricsAPI(t, nil, true, "", http.StatusNotFound)

		_, err := FetchPodSample(context.Background(), testNode, testPod(), 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "metrics.k8s.io API is not served")
	})

	t.Run("re-reads once before concluding the kubelet does not know the pod", func(t *testing.T) {
		// A cached readout can simply predate the pod. Degrading on it costs the storage
		// figures, and where the metrics API still holds the pod name's previous incarnation it
		// answers a live pod with "only a previous incarnation" — a diagnosis pointing at the
		// wrong thing entirely.
		var calls atomic.Int64
		serveAPI(t, nodeProxyMux(func(w http.ResponseWriter, _ *http.Request) {
			// The first readout predates the pod; every later one carries it.
			var pods []kubeletstats.PodStats
			if calls.Add(1) > 1 {
				pods = []kubeletstats.PodStats{testPodStats()}
			}
			bs, _ := json.Marshal(&kubeletstats.Summary{Pods: pods})
			_, _ = w.Write(bs)
		}))

		// Prime the cache with the readout that predates the pod, then ask within its window.
		_, err := fetchPodStatsFromKubelet(context.Background(), testNode, time.Minute)
		require.NoError(t, err)

		sample, err := FetchPodSample(context.Background(), testNode, testPod(), time.Minute)
		require.NoError(t, err, "the pod is live and the kubelet knows it")
		require.NotNil(t, sample.CPUUsedMilliCores)
		assert.Equal(t, uint64(500), *sample.CPUUsedMilliCores)
		assert.Equal(t, int64(2), calls.Load(), "exactly one extra read, only on the miss")
	})

	t.Run("rejects a degraded entry measured before this pod existed", func(t *testing.T) {
		// A recreated pod keeps its name; the metrics API may still serve the previous
		// incarnation's entry, which must never be presented as current.
		serveKubeletAndMetricsAPI(t, nil, true,
			podMetricsPayload(time.Now().Add(-time.Hour), "250m", "512Mi"), http.StatusOK)
		pod := testPod()
		pod.CreationTimestamp = meta.Now()

		_, err := FetchPodSample(context.Background(), testNode, pod, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "previous incarnation")
	})
}

func TestFetchPodSamples(t *testing.T) {
	otherPod := func() *core.Pod {
		p := testPod()
		p.Name, p.UID = "other", "pod-uid-2"
		return p
	}

	t.Run("returns a sample per measured pod, keyed by pod UID", func(t *testing.T) {
		other := otherPod()
		servePods(t, []kubeletstats.PodStats{
			testPodStats(),
			{
				PodRef: kubeletstats.PodReference{Namespace: "default", Name: "other", UID: "pod-uid-2"},
				CPU:    &kubeletstats.CPUStats{UsageNanoCores: ptr.To[uint64](1_000_000_000)},
			},
		})

		samples, err := FetchPodSamples(context.Background(), testNode,
			[]*core.Pod{testPod(), other}, 0)
		require.NoError(t, err)
		require.Len(t, samples, 2)
		assert.Equal(t, uint64(500), *samples["pod-uid-1"].CPUUsedMilliCores)
		assert.Equal(t, uint64(1000), *samples["pod-uid-2"].CPUUsedMilliCores)
		assert.Equal(t, uint64(2000), samples["pod-uid-1"].CPUTotalMilliCores,
			"the totals come from each pod's own limits")
	})

	t.Run("leaves out a pod the kubelet does not carry", func(t *testing.T) {
		// The node-wide caller has no one to answer to for a single pod, so an absent entry
		// beats one extra metrics.k8s.io request per pod per period.
		servePods(t, []kubeletstats.PodStats{testPodStats()})

		samples, err := FetchPodSamples(context.Background(), testNode,
			[]*core.Pod{testPod(), otherPod()}, 0)
		require.NoError(t, err)
		require.Len(t, samples, 1)
		assert.Contains(t, samples, types.UID("pod-uid-1"))
	})

	t.Run("errors when the node-wide readout failed", func(t *testing.T) {
		// That one failure costs every pod its figures at once, so it is the caller's problem.
		serveAPI(t, nodeProxyMux(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))

		_, err := FetchPodSamples(context.Background(), testNode, []*core.Pod{testPod()}, 0)
		require.Error(t, err)
	})

	t.Run("asks the node once however many pods it is given", func(t *testing.T) {
		calls := servePods(t, []kubeletstats.PodStats{testPodStats()})

		_, err := FetchPodSamples(context.Background(), testNode,
			[]*core.Pod{testPod(), otherPod(), testPod()}, 0)
		require.NoError(t, err)
		assert.Equal(t, int64(1), calls.Load())
	})
}

func TestFetchPodStatsFromKubelet(t *testing.T) {
	t.Run("decodes the node's pod entries", func(t *testing.T) {
		servePods(t, []kubeletstats.PodStats{testPodStats()})

		pods, err := fetchPodStatsFromKubelet(context.Background(), testNode, 0)
		require.NoError(t, err)
		require.Len(t, pods, 1)
		assert.Equal(t, "inst", pods[0].PodRef.Name)
	})

	t.Run("errors when the proxy fails", func(t *testing.T) {
		serveAPI(t, nodeProxyMux(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))

		_, err := fetchPodStatsFromKubelet(context.Background(), testNode, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), testNode, "the error must name the node it failed on")
	})

	t.Run("errors on an undecodable body", func(t *testing.T) {
		serveAPI(t, nodeProxyMux(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{not-json"))
		}))

		_, err := fetchPodStatsFromKubelet(context.Background(), testNode, 0)
		require.Error(t, err)
	})
}

// TestFetchPodStatsFromKubelet_Cache pins the reason the cache exists: the readout is node-wide, so several
// pods of one node must cost one node-proxy request between them, not one each.
func TestFetchPodStatsFromKubelet_Cache(t *testing.T) {
	t.Run("serves a readout younger than maxAge without hitting the node", func(t *testing.T) {
		calls := servePods(t, []kubeletstats.PodStats{testPodStats()})

		for range 5 {
			_, err := fetchPodStatsFromKubelet(context.Background(), testNode, time.Minute)
			require.NoError(t, err)
		}
		assert.Equal(t, int64(1), calls.Load())
	})

	t.Run("re-reads once the readout has aged past maxAge", func(t *testing.T) {
		calls := servePods(t, []kubeletstats.PodStats{testPodStats()})

		_, err := fetchPodStatsFromKubelet(context.Background(), testNode, time.Millisecond)
		require.NoError(t, err)
		time.Sleep(5 * time.Millisecond)
		_, err = fetchPodStatsFromKubelet(context.Background(), testNode, time.Millisecond)
		require.NoError(t, err)

		assert.Equal(t, int64(2), calls.Load())
	})

	t.Run("a zero maxAge reads afresh every time", func(t *testing.T) {
		calls := servePods(t, []kubeletstats.PodStats{testPodStats()})

		for range 3 {
			_, err := fetchPodStatsFromKubelet(context.Background(), testNode, 0)
			require.NoError(t, err)
		}
		assert.Equal(t, int64(3), calls.Load())
	})

	t.Run("never caches a failure", func(t *testing.T) {
		var calls atomic.Int64
		serveAPI(t, nodeProxyMux(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		}))

		for range 3 {
			_, err := fetchPodStatsFromKubelet(context.Background(), testNode, time.Minute)
			require.Error(t, err)
		}
		// A transient failure must not blind the node for a whole window.
		assert.Equal(t, int64(3), calls.Load())
	})

	t.Run("collapses concurrent misses onto one read", func(t *testing.T) {
		// The cache alone would only halve a steady polling load: a client asking about every
		// Instance of a node at once misses together and reads together.
		var calls atomic.Int64
		release := make(chan struct{})
		serveAPI(t, nodeProxyMux(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			<-release
			bs, _ := json.Marshal(&kubeletstats.Summary{Pods: []kubeletstats.PodStats{testPodStats()}})
			_, _ = w.Write(bs)
		}))

		const callers = 8
		var wg sync.WaitGroup
		errs := make([]error, callers)
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = fetchPodStatsFromKubelet(context.Background(), testNode, time.Minute)
			}()
		}
		// Let the leader reach the node, then let the rest queue behind it.
		require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
		time.Sleep(50 * time.Millisecond)
		close(release)
		wg.Wait()

		for i := range errs {
			require.NoError(t, errs[i], "every caller is served by the one read")
		}
		assert.Equal(t, int64(1), calls.Load())
	})

	t.Run("a caller waiting on another's read still honors its own deadline", func(t *testing.T) {
		// Otherwise one stuck kubelet would hold every request for that node until the read's
		// own deadline, long past the point each caller was told to give up.
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		serveAPI(t, nodeProxyMux(func(_ http.ResponseWriter, _ *http.Request) {
			<-release
		}))

		// A zero age keeps this stuck read out of the cache once it finally returns.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := fetchPodStatsFromKubelet(ctx, testNode, 0)

		require.Error(t, err)
		assert.Less(t, time.Since(start), readTimeout,
			"the caller must leave on its own deadline, not the read's")
	})

	t.Run("drops entries past the retention horizon, not ones the caller happens not to want",
		func(t *testing.T) {
			// Two rules in one: without a sweep the map would keep one readout per node the
			// process was ever asked about, for its whole life — and sweeping by the *caller's*
			// maxAge instead would let a caller that accepts only very fresh data evict entries
			// still perfectly good for everyone else.
			c := podStatsStore{entries: map[string]podStatsEntry{
				"node-past":  {pods: []kubeletstats.PodStats{testPodStats()}, fetchedAt: time.Now().Add(-2 * cacheRetention)},
				"node-fresh": {pods: []kubeletstats.PodStats{testPodStats()}, fetchedAt: time.Now().Add(-time.Minute)},
			}}

			// A caller wanting nothing older than a millisecond.
			c.store("node-new", []kubeletstats.PodStats{testPodStats()}, time.Millisecond)

			c.mu.RLock()
			defer c.mu.RUnlock()
			assert.NotContains(t, c.entries, "node-past", "past the retention horizon")
			assert.Contains(t, c.entries, "node-fresh",
				"a minute-old entry survives a caller that would not have accepted it")
			assert.Contains(t, c.entries, "node-new")
		})

	t.Run("bounds how many nodes it holds at once, oldest first", func(t *testing.T) {
		// The age sweep alone does not bound it: a client walking every Instance of a large
		// cluster inside one window reads every node, and each entry is a whole decoded summary.
		c := podStatsStore{entries: map[string]podStatsEntry{}}
		for i := range maxCachedNodes + 10 {
			c.store("node-"+strconv.Itoa(i), []kubeletstats.PodStats{testPodStats()}, time.Minute)
		}

		c.mu.RLock()
		defer c.mu.RUnlock()
		assert.Len(t, c.entries, maxCachedNodes)
		assert.NotContains(t, c.entries, "node-0", "the oldest entry goes first")
		assert.Contains(t, c.entries, "node-"+strconv.Itoa(maxCachedNodes+9),
			"the newest entry stays")
	})
}

func TestPodStatsOf(t *testing.T) {
	pod := testPod()
	pods := []kubeletstats.PodStats{
		{PodRef: kubeletstats.PodReference{Namespace: "other", Name: "inst", UID: "pod-uid-1"}},
		// A previous incarnation: the same namespace and name, a different UID.
		{PodRef: kubeletstats.PodReference{Namespace: "default", Name: "inst", UID: "stale-uid"}},
		{PodRef: kubeletstats.PodReference{Namespace: "default", Name: "inst", UID: "pod-uid-1"}},
	}

	cases := []struct {
		name string

		pods []kubeletstats.PodStats

		wantFound bool
	}{
		{name: "matches on namespace, name and UID", pods: pods, wantFound: true},
		{name: "never serves a previous incarnation's figures", pods: pods[1:2]},
		{name: "reports a readout that does not carry the pod", pods: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := podStatsOf(c.pods, pod)
			if !c.wantFound {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, "pod-uid-1", got.PodRef.UID)
			assert.Equal(t, "default", got.PodRef.Namespace)
		})
	}
}
