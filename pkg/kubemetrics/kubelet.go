package kubemetrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeletstats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// DefaultMaxAge is the accepted age of a cached node readout for a caller with no sampling
// cadence of its own — a request-driven one, whose bursts this bounds to one readout per node.
//
// A caller that samples on a fixed period must pass 0 rather than its period. Its rounds start
// exactly one period apart while an entry is stamped mid-round, so an entry is always younger
// than the period when the next round asks: it would read the source every other round and
// republish the round between, at half the cadence it advertises.
const DefaultMaxAge = 15 * time.Second

// FetchPodSample returns one Instance pod's utilization sample: the totals it declared, and
// the usage the node kubelet measured for it.
//
// When the kubelet cannot answer — it is unreachable, or it answered without carrying the pod,
// which happens while its stats provider catches up after a restart — the usage degrades to the
// metrics.k8s.io API, which serves CPU and memory but no storage. The totals survive either
// way, because they come from the Instance's own declaration rather than from a measurement.
//
// The kubelet readout is node-wide, so maxAge states how old a readout of this node the caller
// still accepts; see fetchPodStatsFromKubelet for what that buys.
//
// A returned error names every source that failed and why, so an API-serving caller can hand
// the message to its client as it stands.
func FetchPodSample(
	ctx context.Context,
	nodeName string,
	pod *core.Pod,
	maxAge time.Duration,
) (*worker.InstanceMetricsSample, error) {
	pods, err := fetchPodStatsFromKubelet(ctx, nodeName, maxAge)
	if err == nil {
		if ps := podStatsOf(pods, pod); ps != nil {
			return newSampleFromPodStats(pod, ps), nil
		}
		// A cached readout can simply predate the pod. Re-read once before concluding the
		// kubelet does not know it: degrading on a stale readout costs the storage figures, and
		// can even report the pod as gone when the metrics API still holds its predecessor.
		if maxAge > 0 {
			if fresh, ferr := refetchPodStatsFromKubelet(ctx, nodeName, maxAge); ferr == nil {
				if ps := podStatsOf(fresh, pod); ps != nil {
					return newSampleFromPodStats(pod, ps), nil
				}
			}
		}
		// The kubelet answered but does not carry the pod. Degrade as well: a sample stamped
		// with a measurement that never happened is worse than degraded CPU/memory figures.
		err = fmt.Errorf("node %s reports no stats for the pod", nodeName)
	}

	cpu, memory, ts, ferr := fetchPodUsageFromMetricsAPI(ctx, pod)
	// The metrics entry was measured before this pod existed: it belongs to a previous
	// incarnation of the same name — never serve it.
	previousIncarnation := ferr == nil && cpu != nil && ts != nil && ts.Time.Before(pod.CreationTimestamp.Time)

	// Name the actual reason: "the metrics API failed", "it is not served here" and "it only
	// knows a previous pod of this name" are three different operator actions.
	switch {
	case ferr != nil:
		return nil, fmt.Errorf(
			"failed to read the usage of instance pod %s/%s from the kubelet (%w) and from the metrics API (%w)",
			pod.Namespace, pod.Name, err, ferr)
	case previousIncarnation:
		return nil, fmt.Errorf(
			"failed to read the usage of instance pod %s/%s from the kubelet (%w); "+
				"the metrics API only knows a previous incarnation of this pod name",
			pod.Namespace, pod.Name, err)
	case cpu == nil:
		return nil, fmt.Errorf(
			"failed to read the usage of instance pod %s/%s from the kubelet (%w); "+
				"the metrics.k8s.io API is not served in this cluster",
			pod.Namespace, pod.Name, err)
	}

	// The fallback carries no storage figures, so StorageUsedMiB stays absent.
	sample := NewSample(pod)
	sample.CPUUsedMilliCores = nanoCoresToMilliCores(cpu)
	sample.MemoryUsedMiB = bytesToMiB(memory)
	if ts != nil {
		sample.Timestamp = *ts
	}
	return sample, nil
}

// FetchPodSamples returns one utilization sample per given pod, keyed by pod UID.
//
// This is the node-wide counterpart of FetchPodSample, for a caller sampling every Instance of
// a node at once. It takes the pods because a sample's totals come from their container limits,
// which the kubelet readout does not carry, and it keys the result by UID because a sample
// carries no identity of its own.
//
// The two differ in what they do about a pod the kubelet cannot account for. FetchPodSample
// degrades that pod to metrics.k8s.io, because its caller was asked about that one pod and owes
// an answer. Here the pod is simply left out: an exporter reporting a whole node has no one to
// answer to for a single pod, and degrading each of them would cost one extra request per pod
// per period. Only a failed node-wide readout is an error, since that one costs every pod its
// figures at once.
func FetchPodSamples(
	ctx context.Context,
	nodeName string,
	pods []*core.Pod,
	maxAge time.Duration,
) (map[types.UID]*worker.InstanceMetricsSample, error) {
	stats, err := fetchPodStatsFromKubelet(ctx, nodeName, maxAge)
	if err != nil {
		return nil, err
	}

	samples := make(map[types.UID]*worker.InstanceMetricsSample, len(pods))
	for _, pod := range pods {
		ps := podStatsOf(stats, pod)
		if ps == nil {
			continue
		}
		samples[pod.UID] = newSampleFromPodStats(pod, ps)
	}
	return samples, nil
}

// readTimeout bounds one node readout. A readout is deliberately detached from whichever
// caller happened to start it — the others are waiting on that same read, and one of them
// giving up must not cancel it out from under them — so it carries a deadline of its own.
const readTimeout = 10 * time.Second

// fetchPodStatsFromKubelet returns the per-pod stats the kubelet of the given node measured,
// read through the API-server node proxy.
//
// Going through the proxy means no kubelet address resolution and no kubelet TLS handling, and
// it keeps the caller's ServiceAccount token where it belongs: reading the kubelet directly
// would hand that token to a peer whose serving certificate cannot be verified on most
// distributions.
//
// The readout is node-wide, so asking about several pods of one node would otherwise re-read
// the same payload once per pod. Two things stop that: a successful readout younger than maxAge
// is served from a process-wide cache, and concurrent misses for one node collapse onto a
// single in-flight read. A maxAge of 0 or less bypasses the cache — not the collapsing, which
// costs nothing and is always right. Freshness stays visible either way, because a sample is
// stamped with the kubelet's own measurement time rather than with the time it was read.
// Failures are never cached.
//
// A caller waiting on another's read still leaves when its own context says so.
//
// The returned slice is shared with every other caller inside the maxAge window: read it, never
// write to it.
func fetchPodStatsFromKubelet(
	ctx context.Context,
	nodeName string,
	maxAge time.Duration,
) ([]kubeletstats.PodStats, error) {
	return podStatsFromKubelet(ctx, nodeName, maxAge, true)
}

// refetchPodStatsFromKubelet reads past a cached readout the caller found unusable — one taken
// before the pod it is asking about existed — and caches what it reads under maxAge like any
// other readout. Reading past the cache without replacing it would leave the stale entry in place
// for the rest of its window, so every caller behind this one would repeat the same extra read.
func refetchPodStatsFromKubelet(
	ctx context.Context,
	nodeName string,
	maxAge time.Duration,
) ([]kubeletstats.PodStats, error) {
	return podStatsFromKubelet(ctx, nodeName, maxAge, false)
}

// podStatsFromKubelet performs the readout, optionally serving it from the cache first. Whether
// the cache is consulted and whether the result is cached are two different questions: a forced
// re-read skips the lookup and still stores what it read.
func podStatsFromKubelet(
	ctx context.Context,
	nodeName string,
	maxAge time.Duration,
	useCache bool,
) ([]kubeletstats.PodStats, error) {
	if useCache {
		if pods, ok := podStatsCache.load(nodeName, maxAge); ok {
			return pods, nil
		}
	}

	ch := podStatsFlight.DoChan(nodeName, func() (any, error) {
		// A caller can arrive in the gap between the leader storing its readout and its
		// flight being forgotten, and would otherwise re-read what is already cached.
		if useCache {
			if pods, ok := podStatsCache.load(nodeName, maxAge); ok {
				return pods, nil
			}
		}
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readTimeout)
		defer cancel()
		return getPodStats(readCtx, nodeName, maxAge)
	})

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("failed to get kubelet stats summary of node %s: %w", nodeName, ctx.Err())
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.([]kubeletstats.PodStats), nil
	}
}

// getPodStats performs one node-proxy readout and caches it when it succeeds.
func getPodStats(
	ctx context.Context,
	nodeName string,
	maxAge time.Duration,
) ([]kubeletstats.PodStats, error) {
	raw, err := system.LoopbackKubeClient.Get().CoreV1().RESTClient().Get().
		Resource("nodes").Name(nodeName).SubResource("proxy").
		Suffix("stats", "summary").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubelet stats summary of node %s: %w", nodeName, err)
	}

	summary := &kubeletstats.Summary{}
	if err = json.Unmarshal(raw, summary); err != nil {
		return nil, fmt.Errorf("failed to decode kubelet stats summary of node %s: %w", nodeName, err)
	}

	podStatsCache.store(nodeName, summary.Pods, maxAge)
	return summary.Pods, nil
}

// podStatsOf returns the readout's entry for the given pod, or nil when the node-wide readout
// does not carry it — e.g. the kubelet's stats provider has not picked the pod up yet.
//
// The match includes the pod UID: a deleted-and-recreated pod keeps its namespace and name,
// and must never be served the previous incarnation's figures.
func podStatsOf(pods []kubeletstats.PodStats, pod *core.Pod) *kubeletstats.PodStats {
	for i := range pods {
		ref := &pods[i].PodRef
		if ref.Namespace == pod.Namespace && ref.Name == pod.Name && ref.UID == string(pod.UID) {
			return &pods[i]
		}
	}
	return nil
}

// maxCachedNodes bounds how many nodes' readouts are held at once. The age sweep alone does not:
// a client walking every Instance of a large cluster inside one window reads every node, and each
// entry is a full decoded summary of every pod on that node. Past this many, the oldest go first —
// they are the ones closest to expiring anyway.
const maxCachedNodes = 64

// cacheRetention is how long an entry is kept regardless of who asks for it. The sweep is
// deliberately not the asking caller's maxAge: a caller that accepts only very fresh readouts
// would otherwise evict entries still perfectly good for every other caller. Nothing stale can be
// served either way — load compares each entry against the age its own caller stated — so this
// only decides what is worth keeping, and maxCachedNodes is what bounds the memory.
const cacheRetention = 5 * time.Minute

// podStatsCache holds the latest successful readout per node, so several pods of one node
// cost one node-proxy request between them instead of one each.
var podStatsCache = podStatsStore{entries: map[string]podStatsEntry{}}

// podStatsFlight keeps one readout per node in flight at a time. The cache alone only
// halves a steady polling load: a client asking about every Instance of a node at once misses
// together and reads together, once per window.
var podStatsFlight singleflight.Group

type podStatsEntry struct {
	pods      []kubeletstats.PodStats
	fetchedAt time.Time
}

type podStatsStore struct {
	mu      sync.RWMutex
	entries map[string]podStatsEntry
}

// load returns the node's cached readout when it is younger than maxAge.
func (c *podStatsStore) load(nodeName string, maxAge time.Duration) ([]kubeletstats.PodStats, bool) {
	if maxAge <= 0 {
		return nil, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.entries[nodeName]
	if !ok || time.Since(e.fetchedAt) >= maxAge {
		return nil, false
	}
	return e.pods, true
}

// store caches the node's readout, dropping every entry past cacheRetention on the way, then the
// oldest of whatever is left until at most maxCachedNodes remain. Without the sweep the map would
// hold one readout per node ever asked about for the life of the process; without the bound, one
// burst across a large cluster would hold them all at once.
func (c *podStatsStore) store(nodeName string, pods []kubeletstats.PodStats, maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for name, e := range c.entries {
		if now.Sub(e.fetchedAt) >= cacheRetention {
			delete(c.entries, name)
		}
	}
	c.entries[nodeName] = podStatsEntry{pods: pods, fetchedAt: now}
	c.evictOldest()
}

// evictOldest drops entries, oldest first, until the store is back within maxCachedNodes.
// The caller holds the write lock.
func (c *podStatsStore) evictOldest() {
	for len(c.entries) > maxCachedNodes {
		var oldestName string
		var oldestAt time.Time
		for name, e := range c.entries {
			if oldestName == "" || e.fetchedAt.Before(oldestAt) {
				oldestName, oldestAt = name, e.fetchedAt
			}
		}
		delete(c.entries, oldestName)
	}
}
