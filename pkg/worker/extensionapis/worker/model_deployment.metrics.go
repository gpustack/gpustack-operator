package worker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	app "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

const (
	modelDeploymentMetricsTimeout  = 8 * time.Second
	modelDeploymentScrapeTimeout   = 2 * time.Second
	modelDeploymentMetricsMaxBytes = 1 << 20
	modelDeploymentMetricsMaxPods  = 64
	modelDeploymentCounterMaxAge   = 5 * time.Minute

	// modelDeploymentMetricsConcurrency is sized so that every Pod up to the limit is fetched
	// within half the request budget even when each fetch runs to its own limit, leaving the
	// other half for discovery. With fewer concurrent fetches, Pods that hang would spend the
	// budget before the Pods queued behind them are asked at all.
	modelDeploymentMetricsConcurrency = 32
)

type modelDeploymentPreviousCounter struct {
	value modelDeploymentCounterPair
	at    time.Time
}

type modelDeploymentPreviousWindow struct {
	value modelDeploymentWindowPair
	at    time.Time
}

// ModelDeploymentMetricsHandler handles the authorized metrics subresource.
type ModelDeploymentMetricsHandler struct {
	extensionapi.GetOperation
	APIReader  ctrlcli.Reader
	HTTPClient *http.Client

	mu       sync.Mutex
	previous map[string]modelDeploymentPreviousCounter
	windows  map[string]modelDeploymentPreviousWindow
}

func newModelDeploymentMetricsHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *ModelDeploymentMetricsHandler {
	h := &ModelDeploymentMetricsHandler{
		APIReader: opts.Manager.GetAPIReader(),
		HTTPClient: &http.Client{
			Transport: httpx.Transport(httpx.TransportOptions().WithoutProxy()),
			Timeout:   modelDeploymentScrapeTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		previous: make(map[string]modelDeploymentPreviousCounter),
		windows:  make(map[string]modelDeploymentPreviousWindow),
	}
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)
	return h
}

var (
	_ rest.Storage = (*ModelDeploymentMetricsHandler)(nil)
	_ rest.Getter  = (*ModelDeploymentMetricsHandler)(nil)
)

func (h *ModelDeploymentMetricsHandler) New() runtime.Object { return &worker.ModelDeploymentMetrics{} }
func (h *ModelDeploymentMetricsHandler) Destroy()            {}

type modelDeploymentPodScrape struct {
	pod    core.Pod
	router bool
	value  modelDeploymentScrape
	at     time.Time
	err    error
}

func (h *ModelDeploymentMetricsHandler) OnGet(
	ctx context.Context, key types.NamespacedName, _ ctrlcli.GetOptions,
) (runtime.Object, error) {
	ctx, cancel := context.WithTimeout(ctx, modelDeploymentMetricsTimeout)
	defer cancel()
	md := &workercore.ModelDeployment{}
	if err := h.APIReader.Get(ctx, key, md); err != nil {
		return nil, err
	}
	list := &core.PodList{}
	if err := h.APIReader.List(ctx, list, ctrlcli.InNamespace(key.Namespace), ctrlcli.MatchingLabels{
		"app.kubernetes.io/name": "model-deployment", "app.kubernetes.io/instance": key.Name,
	}); err != nil {
		return nil, fmt.Errorf("list ModelDeployment Pods: %w", err)
	}
	slices.SortFunc(list.Items, func(a, b core.Pod) int { return compareNames(a.Name, b.Name) })
	selected := make([]core.Pod, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		owned, err := h.ownedMetricsPod(ctx, md, pod)
		if err != nil {
			return nil, err
		}
		if owned {
			selected = append(selected, *pod)
		}
	}
	if len(selected) == 0 {
		return nil, kerrors.NewServiceUnavailable("ModelDeployment has no owned metrics Pods")
	}
	result := &worker.ModelDeploymentMetrics{ObjectMeta: meta.ObjectMeta{Name: md.Name, Namespace: md.Namespace}}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if len(role.Command) != 0 {
			continue
		}
		found := 0
		for j := range selected {
			if selected[j].Labels["app.kubernetes.io/component"] == role.Name {
				found++
			}
		}
		expected := max(int(role.Replicas), 1) * max(int(role.ReplicaSize), 1)
		if found == 0 {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Source: "engine/" + role.Name, Reason: "no owned metrics Pod",
			})
		} else if found < expected {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Source: "engine/" + role.Name, Reason: fmt.Sprintf("%d of %d owned metrics Pods present", found, expected),
			})
		}
	}
	if md.Spec.Router != nil {
		found := 0
		for j := range selected {
			if selected[j].Labels["modeldeployment.gpustack.ai/router"] == md.Spec.Router.Name {
				found++
			}
		}
		expected := 1
		if md.Spec.Router.Replicas != nil {
			expected = max(int(*md.Spec.Router.Replicas), 1)
		}
		if found == 0 {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Source: "router", Reason: "no owned metrics Pod",
			})
		} else if found < expected {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Source: "router", Reason: fmt.Sprintf("%d of %d owned metrics Pods present", found, expected),
			})
		}
	}
	if len(selected) > modelDeploymentMetricsMaxPods {
		result.Partial = true
		result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
			Source: "pod-discovery", Reason: "Pod count exceeds the bounded scrape limit",
		})
		selected = selected[:modelDeploymentMetricsMaxPods]
	}
	reads := make([]modelDeploymentPodScrape, len(selected))
	var wg sync.WaitGroup
	sem := make(chan struct{}, modelDeploymentMetricsConcurrency)
	for i := range selected {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			reads[i] = h.scrapeMetricsPod(ctx, md, selected[i])
		}(i)
	}
	wg.Wait()
	readable := 0
	for i := range reads {
		read := &reads[i]
		if read.err != nil {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: "scrape", Reason: read.err.Error(),
			})
			continue
		}
		readable++
		h.mergeMetrics(result, md, read)
	}
	if readable == 0 {
		return nil, kerrors.NewServiceUnavailable("all ModelDeployment metrics Pods are unreadable: " + reads[0].err.Error())
	}
	slices.SortFunc(result.Processing, func(a, b worker.ModelDeploymentMetricGauge) int { return compareNames(a.Name+a.Scope, b.Name+b.Scope) })
	slices.SortFunc(result.Queueing, func(a, b worker.ModelDeploymentMetricGauge) int { return compareNames(a.Name+a.Scope, b.Name+b.Scope) })
	slices.SortFunc(result.CacheHits, func(a, b worker.ModelDeploymentCacheHit) int { return compareNames(a.Pod+a.Scope, b.Pod+b.Scope) })
	slices.SortFunc(result.Latency, func(a, b worker.ModelDeploymentMetricWindow) int { return compareNames(a.Pod+a.Name, b.Pod+b.Name) })
	slices.SortFunc(result.Traffic, func(a, b worker.ModelDeploymentMetricWindow) int { return compareNames(a.Pod+a.Name, b.Pod+b.Name) })
	slices.SortFunc(result.Transfer, func(a, b worker.ModelDeploymentMetricWindow) int { return compareNames(a.Pod+a.Name, b.Pod+b.Name) })
	slices.SortFunc(result.Missing, func(a, b worker.ModelDeploymentMetricMissing) int {
		return compareNames(a.Pod+a.Source, b.Pod+b.Source)
	})
	result.Partial = result.Partial || modelDeploymentMissingIsPartial(result.Missing)
	result.Timestamp = meta.NewTime(time.Now())
	return result, nil
}

func compareNames(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func (h *ModelDeploymentMetricsHandler) ownedMetricsPod(
	ctx context.Context, md *workercore.ModelDeployment, pod *core.Pod,
) (bool, error) {
	if pod.Labels["modeldeployment.gpustack.ai/router"] != "" {
		if md.Spec.Router == nil || pod.Labels["modeldeployment.gpustack.ai/router"] != md.Spec.Router.Name {
			return false, nil
		}
		podOwner := meta.GetControllerOf(pod)
		if podOwner == nil || podOwner.Kind != "ReplicaSet" {
			return false, nil
		}
		rs := &app.ReplicaSet{}
		if err := h.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: podOwner.Name}, rs); err != nil {
			if kerrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if rs.UID != podOwner.UID {
			return false, nil
		}
		rsOwner := meta.GetControllerOf(rs)
		if rsOwner == nil || rsOwner.Kind != "Deployment" || rsOwner.Name != md.Name+"-router" {
			return false, nil
		}
		deployment := &app.Deployment{}
		if err := h.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: rsOwner.Name}, deployment); err != nil {
			if kerrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if deployment.UID != rsOwner.UID {
			return false, nil
		}
		owner := meta.GetControllerOf(deployment)
		return owner != nil && owner.Kind == "ModelDeployment" && owner.UID == md.UID, nil
	}
	role := pod.Labels["app.kubernetes.io/component"]
	validRole := false
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].Name == role && len(md.Spec.Roles[i].Command) == 0 {
			validRole = true
			break
		}
	}
	owner := meta.GetControllerOf(pod)
	return validRole && owner != nil && owner.Kind == "ModelDeployment" && owner.UID == md.UID, nil
}

func (h *ModelDeploymentMetricsHandler) scrapeMetricsPod(
	ctx context.Context, md *workercore.ModelDeployment, pod core.Pod,
) modelDeploymentPodScrape {
	read := modelDeploymentPodScrape{pod: pod, router: pod.Labels["modeldeployment.gpustack.ai/router"] != ""}
	if pod.Status.PodIP == "" || pod.Annotations["prometheus.io/scrape"] != "true" ||
		pod.Annotations["prometheus.io/path"] != "/metrics" {
		read.err = fmt.Errorf("pod has no ready advertised metrics endpoint")
		return read
	}
	port, err := strconv.ParseInt(pod.Annotations["prometheus.io/port"], 10, 32)
	if err != nil || port < 1 || port > 65535 {
		read.err = fmt.Errorf("pod advertises an invalid metrics port")
		return read
	}
	if !modelDeploymentMetricsPortDeclared(&pod, int32(port)) {
		read.err = fmt.Errorf("pod advertises a metrics port absent from its containers")
		return read
	}
	scheme := pod.Annotations["prometheus.io/scheme"]
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		read.err = fmt.Errorf("pod advertises an invalid metrics scheme")
		return read
	}
	url := scheme + "://" + net.JoinHostPort(pod.Status.PodIP, strconv.FormatInt(port, 10)) + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		read.err = err
		return read
	}
	response, err := h.HTTPClient.Do(req)
	if err != nil {
		read.err = err
		return read
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		read.err = fmt.Errorf("metrics endpoint returned HTTP %d", response.StatusCode)
		return read
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, modelDeploymentMetricsMaxBytes+1))
	if err != nil {
		read.err = err
		return read
	}
	if len(body) > modelDeploymentMetricsMaxBytes {
		read.err = fmt.Errorf("metrics response exceeds %d bytes", modelDeploymentMetricsMaxBytes)
		return read
	}
	engine, router := md.Spec.Engine.Name, ""
	if read.router {
		engine, router = "", md.Spec.Router.Name
	}
	read.value, read.err = parseModelDeploymentMetrics(body, engine, router)
	read.at = time.Now()
	return read
}

func modelDeploymentMetricsPortDeclared(pod *core.Pod, port int32) bool {
	for _, container := range append(slices.Clone(pod.Spec.Containers), pod.Spec.InitContainers...) {
		for _, declared := range container.Ports {
			if declared.ContainerPort == port {
				return true
			}
		}
	}
	return false
}

func (h *ModelDeploymentMetricsHandler) mergeMetrics(
	result *worker.ModelDeploymentMetrics, md *workercore.ModelDeployment, read *modelDeploymentPodScrape,
) {
	metricNames := map[string]string{}
	switch {
	case read.router:
		switch md.Spec.Router.Name {
		case "llm-d-router":
			metricNames["router-running"] = "llm_d_epp_request_running"
			metricNames["router-backends"] = "llm_d_epp_ready_endpoints"
		case "vllm-router":
			if isModelDeploymentPD(md) {
				for _, source := range []string{"vllm_router_running_requests", "vllm_router_active_workers"} {
					result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
						Pod: read.pod.Name, Source: source, Reason: modelDeploymentVLLMRouterPDNoProcessing,
					})
				}
				break
			}
			metricNames["router-running"] = "vllm_router_running_requests"
			metricNames["router-reported-workers"] = "vllm_router_active_workers"
		case "sglang-gateway":
			metricNames["router-running"] = "smg_worker_requests_active"
			metricNames["router-backends"] = "smg_worker_pool_size"
		}
	case md.Spec.Engine.Name == "vllm":
		metricNames["running"] = "vllm:num_requests_running"
		metricNames["waiting"] = "vllm:num_requests_waiting"
	default:
		metricNames["running"] = "sglang:num_running_reqs"
		metricNames["waiting"] = "sglang:num_queue_reqs"
		if isModelDeploymentPD(md) && modelDeploymentPodRoleKind(md, &read.pod) == workercore.ModelDeploymentRoleKindDecode {
			metricNames["decode-transfer-waiting"] = "sglang:num_decode_transfer_queue_reqs"
		}
	}
	for key, source := range metricNames {
		value, ok := read.value.gauges[key]
		if !ok {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: source, Reason: "metric is absent",
			})
			continue
		}
		scope := "engine"
		if read.router {
			scope = "router"
		} else {
			scope += "/" + read.pod.Labels["app.kubernetes.io/component"]
		}
		gauge := worker.ModelDeploymentMetricGauge{
			Name: key, Source: source, Scope: scope, Unit: "requests",
			Value: value, PodCount: 1, ObservedAt: meta.NewTime(read.at),
		}
		switch key {
		case "router-backends":
			gauge.Unit = "backends"
		case "router-reported-workers":
			gauge.Unit = "workers"
		}
		if key == "waiting" || key == "decode-transfer-waiting" {
			mergeModelDeploymentGauge(&result.Queueing, gauge)
		} else {
			mergeModelDeploymentGauge(&result.Processing, gauge)
		}
	}
	idle := h.mergeWindowMetrics(result, md, read)
	if read.router {
		return
	}
	// The decode half of an SGLang pair receives its prompt's cache from the prefill half and
	// never prefills, so its prefill token counters stay at zero and hold no hit ratio.
	if md.Spec.Engine.Name == "sglang" && isModelDeploymentPD(md) &&
		modelDeploymentPodRoleKind(md, &read.pod) == workercore.ModelDeploymentRoleKindDecode {
		return
	}
	if len(read.value.counters) == 0 {
		result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
			Pod: read.pod.Name, Source: "cache-hit counters", Reason: "metric is absent",
		})
		return
	}
	var sglangHost, sglangStorage bool
	if md.Spec.Engine.Name == "sglang" {
		sglangHost, sglangStorage = modelDeploymentSGLangPodTiers(&read.pod)
	}
	for scope, current := range read.value.counters {
		previous, ok := h.replaceCounter(md, &read.pod, scope, current, read.at)
		// SGLang exports every tier's hit counter from start, and a tier the Pod does not build
		// keeps its counter at zero, which would read as a measured miss rather than an absent tier.
		if md.Spec.Engine.Name == "sglang" && (scope == "host-prefix" && !sglangHost ||
			scope == "storage-prefix" && !sglangStorage) {
			reason := modelDeploymentSGLangNoHostTier
			if scope == "storage-prefix" {
				reason = modelDeploymentSGLangNoStorageTier
			}
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: scope, Reason: reason,
			})
			continue
		}
		// vLLM exports its external prefix cache counters with or without a KV connector, and
		// without one they never move. A query counter that has moved proves a connector, so
		// only a still-zero one on a Pod rendering none is declared unsupported.
		if scope == "external-store" && current.queries == 0 && !modelDeploymentPodRendersKVConnector(&read.pod) {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: scope, Reason: modelDeploymentVLLMNoKVConnector,
			})
			continue
		}
		// vLLM counts every token any KV connector loads as an external hit, the point-to-point
		// leg's included. The counters measure the shared store only on a Pod that attaches a pool
		// and is not the decode half of a pair: that half loads what the prefill half sends, and
		// the prefill half's own leg never loads anything. A Pod with no connector keeps the one
		// reason above.
		if scope == "external-store" && (md.Spec.KVCache == nil || (isModelDeploymentPD(md) &&
			modelDeploymentPodRoleKind(md, &read.pod) == workercore.ModelDeploymentRoleKindDecode)) {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: scope, Reason: modelDeploymentVLLMExternalNotStore,
			})
			continue
		}
		if !ok {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: scope, Reason: "awaiting a second counter sample",
			})
			continue
		}
		hits := current.hits - previous.value.hits
		queries := current.queries - previous.value.queries
		if hits < 0 || queries < 0 || hits > queries {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: scope, Reason: "counter reset or incompatible denominator",
			})
			continue
		}
		if queries == 0 {
			reason := "no queries in the sampling window"
			if idle {
				reason = modelDeploymentIdleWindow
			}
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: scope, Reason: reason,
			})
			continue
		}
		source := "vllm:prefix_cache"
		if scope == "external-store" {
			source = "vllm:external_prefix_cache"
		}
		if md.Spec.Engine.Name == "sglang" {
			source = "sglang:prefill_effective_tokens_total"
		}
		result.CacheHits = append(result.CacheHits, worker.ModelDeploymentCacheHit{
			Pod: read.pod.Name, Source: source, Scope: scope, Unit: "tokens",
			Hits: hits, Queries: queries, Rate: hits / queries,
			WindowSeconds: read.at.Sub(previous.at).Seconds(),
			PodCount:      1, ObservedAt: meta.NewTime(read.at),
		})
	}
}

// modelDeploymentPodRendersKVConnector reports whether a vLLM engine Pod was given a KV connector.
// The operator renders every connector a managed role runs, the shared store and the
// point-to-point prefill/decode leg alike, through this one argument, and admission refuses it
// among a role's extra arguments.
func modelDeploymentPodRendersKVConnector(pod *core.Pod) bool {
	for _, container := range pod.Spec.Containers {
		for _, arg := range append(slices.Clone(container.Command), container.Args...) {
			if name, _, _ := strings.Cut(arg, "="); name == "--kv-transfer-config" {
				return true
			}
		}
	}
	return false
}

// modelDeploymentSGLangHostTierArgs are the SGLang arguments that select a tree cache reporting host
// hits, the host_hit mode of sglang:prefill_effective_tokens_total, at v0.5.18: the tree cache's
// host_hit_length is what split_cached_prefix_by_tier (managers/schedule_batch.py:203-215) counts as
// host, and the plain RadixCache the selection chain falls back to reports none. The operator
// renders only the first, and a user may add any of them.
var modelDeploymentSGLangHostTierArgs = []string{
	// server_args.py:2668 enable_hierarchical_cache selects HiRadixCache (mem_cache/registry.py:124-132),
	// whose match sets host_hit_length (mem_cache/hiradix_cache.py:1754-1768).
	"--enable-hierarchical-cache",
	// server_args.py:3010 enable_lmcache selects LMCRadixCache (mem_cache/registry.py:138-148),
	// whose match sets host_hit_length (mem_cache/storage/lmcache/lmc_radix_cache.py:245).
	"--enable-lmcache",
	// server_args.py:3022 enable_flexkv selects the FlexKV cache (mem_cache/registry.py:151-165),
	// whose match sets host_hit_length (mem_cache/storage/flexkv/flexkv_radix_cache.py:232).
	"--enable-flexkv",
}

const (
	// modelDeploymentSGLangRadixBackendArg names a registered tree cache in place of the selection
	// chain (server_args.py:1742-1746, default None; mem_cache/registry.py:223-236). It selects a
	// host tier only with the value modelDeploymentSGLangRadixBackendFlexKV, the one backend v0.5.18
	// registers (mem_cache/storage/flexkv/__init__.py:82); any other name is a backend nothing here
	// knows the tiers of.
	modelDeploymentSGLangRadixBackendArg    = "--radix-cache-backend"
	modelDeploymentSGLangRadixBackendFlexKV = "flexkv"
	// modelDeploymentSGLangStorageBackendArg enables the storage tier at start
	// (managers/scheduler.py:444), and at v0.5.18 only that tier sets the storage_hit mode
	// (managers/scheduler.py:3317-3325). The operator owns it and renders it exactly where the
	// deployment attaches a KV cache pool.
	modelDeploymentSGLangStorageBackendArg = "--hicache-storage-backend"
)

// modelDeploymentSGLangPodTiers reports whether an SGLang engine Pod's arguments build a host tier
// and a storage tier, matching each flag by SGLang's own spelling rules.
func modelDeploymentSGLangPodTiers(pod *core.Pod) (host, storage bool) {
	engine := workercore.ModelDeploymentEngineSGLang
	for _, container := range pod.Spec.Containers {
		args := slices.Concat(container.Command, container.Args)
		for i, arg := range args {
			if _, ok := workerctrl.ModelDeploymentResolveArg(engine, arg, modelDeploymentSGLangHostTierArgs); ok {
				host = true
			}
			if _, ok := workerctrl.ModelDeploymentResolveArg(engine, arg, []string{modelDeploymentSGLangStorageBackendArg}); ok {
				storage = true
			}
			if _, ok := workerctrl.ModelDeploymentResolveArg(engine, arg, []string{modelDeploymentSGLangRadixBackendArg}); ok {
				_, value, inline := strings.Cut(arg, "=")
				if !inline && i+1 < len(args) {
					value = args[i+1]
				}
				host = host || value == modelDeploymentSGLangRadixBackendFlexKV
			}
		}
	}
	return host, storage
}

func isModelDeploymentPD(md *workercore.ModelDeployment) bool {
	hasPrefill, hasDecode := false, false
	for _, role := range md.Spec.Roles {
		hasPrefill = hasPrefill || role.Kind == workercore.ModelDeploymentRoleKindPrefill
		hasDecode = hasDecode || role.Kind == workercore.ModelDeploymentRoleKindDecode
	}
	return hasPrefill && hasDecode
}

func modelDeploymentPodRoleKind(md *workercore.ModelDeployment, pod *core.Pod) workercore.ModelDeploymentRoleKind {
	name := pod.Labels["app.kubernetes.io/component"]
	for _, role := range md.Spec.Roles {
		if role.Name == name {
			return role.Kind
		}
	}
	return ""
}

const (
	modelDeploymentLabeledCounterUnexported = "labeled failure or error counter is not exported before its first increment; " +
		"its paired counter was read in the same scrape"
	modelDeploymentLabeledHistogramUnexported = "labeled histogram is not exported before its first observation; " +
		"its paired histogram recorded no new observation in the same window"
	modelDeploymentVLLMRouterPDNoProcessing = "unsupported source: the vLLM router exports no router processing gauge " +
		"for a prefill/decode deployment"
	modelDeploymentVLLMNoKVConnector = "unsupported source: the Pod renders no KV connector, so vLLM queries no " +
		"external prefix cache"
	modelDeploymentVLLMExternalNotStore = "unsupported source: vLLM counts every token any KV connector loads as an " +
		"external prefix cache hit, and this Pod's are not the shared store's alone: it attaches no KV cache pool, " +
		"or it is the decode half of a pair, whose store hits cannot be told from the blocks the prefill half sends"
	modelDeploymentSGLangNoHostTier = "unsupported source: the Pod renders no SGLang argument that selects a tree " +
		"cache with a host tier, so SGLang counts no host hit"
	modelDeploymentSGLangNoStorageTier = "unsupported source: the Pod renders no SGLang storage backend, so SGLang " +
		"counts no storage hit"
	modelDeploymentIdleWindow = "idle sampling window: the Pod's TTFT histogram recorded no new request, so this " +
		"source has no new sample"
)

// modelDeploymentNotPartialReasons are the listed entries that do not make a snapshot partial: a
// labeled series that a healthy Pod has had no reason to export yet, a source the router or the
// Pod's rendered shape is known not to provide, and a source of a Pod that served no request in
// the sampling window, which has no new sample to give. They stay in missing[] so no value is
// fabricated, but none says a readable source failed to contribute.
var modelDeploymentNotPartialReasons = []string{
	modelDeploymentLabeledCounterUnexported,
	modelDeploymentLabeledHistogramUnexported,
	modelDeploymentVLLMRouterPDNoProcessing,
	modelDeploymentVLLMNoKVConnector,
	modelDeploymentVLLMExternalNotStore,
	modelDeploymentSGLangNoHostTier,
	modelDeploymentSGLangNoStorageTier,
	modelDeploymentIdleWindow,
}

// modelDeploymentMissingIsPartial reports whether a required source is missing.
func modelDeploymentMissingIsPartial(missing []worker.ModelDeploymentMetricMissing) bool {
	return slices.ContainsFunc(missing, func(m worker.ModelDeploymentMetricMissing) bool {
		return !slices.Contains(modelDeploymentNotPartialReasons, m.Reason)
	})
}

type modelDeploymentWindowDefinition struct {
	name, source, area, unit string
	histogram                bool
	// pairedWith names the series that vouches for a labeled one. A labeled series exports nothing
	// until its first increment or observation. A failure or error counter absent while its pair
	// was read in the same scrape is a Pod that has not failed yet, not an unreadable source. A
	// histogram absent while its pair recorded no new observation in the same window had nothing to
	// observe; beside a pair that moved, its absence would drop real samples, so it stays missing.
	// Only the entries listed with a pair get this reading; any other absent series is still a
	// missing source.
	pairedWith string
}

// modelDeploymentWindowDefinitions lists the windowed sources of one Pod. A list that reads TTFT
// names it first: every request the Pod answers passes through it, so the entries after it read
// its window to tell a Pod that served no request from one that stopped recording.
func modelDeploymentWindowDefinitions(md *workercore.ModelDeployment, router bool) []modelDeploymentWindowDefinition {
	if router {
		switch md.Spec.Router.Name {
		case "llm-d-router":
			// The router's error counter also counts requests its request counter never records,
			// such as a bad request that names no model, so the error counter is named apart
			// to keep the two out of an error fraction.
			return []modelDeploymentWindowDefinition{
				{"ttft", "llm_d_epp_request_ttft_seconds", "latency", "seconds", true, ""},
				{"tpot", "llm_d_epp_request_streaming_tpot_seconds", "latency", "seconds", true, ""},
				{"requests", "llm_d_epp_request_total", "traffic", "requests/second", false, ""},
				{"request-errors", "llm_d_epp_request_error_total", "traffic", "errors/second", false, "requests"},
			}
		case "vllm-router":
			// The router's prefill/decode mode records only its pd_* series. Its error counter
			// is also incremented on refusals that never reach the request counter, so the two
			// share no denominator and are named apart to keep them out of an error fraction.
			if isModelDeploymentPD(md) {
				return []modelDeploymentWindowDefinition{
					{"pd-requests", "vllm_router_pd_requests_total", "traffic", "requests/second", false, ""},
					{"pd-errors", "vllm_router_pd_errors_total", "traffic", "errors/second", false, "pd-requests"},
				}
			}
			// The retries-exhausted counter is labeled by route like the error counter, so it too
			// is absent until a request first exhausts its attempts.
			return []modelDeploymentWindowDefinition{
				{"successful-requests", "vllm_router_requests_total", "traffic", "requests/second", false, ""},
				{"errors", "vllm_router_request_errors_total", "traffic", "errors/second", false, "successful-requests"},
				{"retries-exhausted", "vllm_router_retries_exhausted_total", "traffic", "events/second", false, "successful-requests"},
			}
		case "sglang-gateway":
			return []modelDeploymentWindowDefinition{
				{"requests", "smg_router_requests_total", "traffic", "requests/second", false, ""},
				{"errors", "smg_router_request_errors_total", "traffic", "errors/second", false, "requests"},
				{"http-5xx-responses", "smg_http_responses_total", "traffic", "responses/second", false, ""},
			}
		}
	}
	if md.Spec.Engine.Name == "vllm" {
		return []modelDeploymentWindowDefinition{
			{"ttft", "vllm:time_to_first_token_seconds", "latency", "seconds", true, ""},
			{"tpot", "vllm:request_time_per_output_token_seconds", "latency", "seconds", true, ""},
			{"itl", "vllm:inter_token_latency_seconds", "latency", "seconds", true, ""},
		}
	}
	// SGLang's inter-token histogram is labeled and observed only when a request streams a second
	// chunk, so a server that has answered nothing but its unstreamed warmup request exports none.
	definitions := []modelDeploymentWindowDefinition{
		{"ttft", "sglang:time_to_first_token_seconds", "latency", "seconds", true, ""},
		{"itl", "sglang:inter_token_latency_seconds", "latency", "seconds", true, "ttft"},
	}
	if isModelDeploymentPD(md) {
		definitions = append(definitions,
			modelDeploymentWindowDefinition{"transfer-latency", "sglang:kv_transfer_latency_ms", "transfer", "milliseconds", true, ""},
			modelDeploymentWindowDefinition{"transfer-speed", "sglang:kv_transfer_speed_gb_s", "transfer", "GB/second", true, ""},
			modelDeploymentWindowDefinition{"transfer-size", "sglang:kv_transfer_total_mb", "transfer", "MB", true, ""},
			modelDeploymentWindowDefinition{"transfer-failures", "sglang:num_transfer_failed_reqs_total", "transfer", "errors/second", false, "transfer-size"},
		)
	}
	return definitions
}

// mergeWindowMetrics reports whether the Pod is idle: its TTFT histogram recorded no new request in
// the sampling window, so none of its windowed sources had anything new to observe.
func (h *ModelDeploymentMetricsHandler) mergeWindowMetrics(
	result *worker.ModelDeploymentMetrics, md *workercore.ModelDeployment, read *modelDeploymentPodScrape,
) bool {
	scope := "router"
	if !read.router {
		scope = "engine/" + read.pod.Labels["app.kubernetes.io/component"]
	}
	deltas := map[string]float64{}
	durations := map[string]float64{}
	starts := map[string]time.Time{}
	sources := map[string]string{}
	unobserved := func(name string) bool {
		count, ok := deltas[name]
		return ok && count == 0
	}
	for _, definition := range modelDeploymentWindowDefinitions(md, read.router) {
		sources[definition.name] = definition.source
		// SGLang observes a KV transfer on the half that sends it, the prefill half.
		if definition.area == "transfer" && modelDeploymentPodRoleKind(md, &read.pod) != workercore.ModelDeploymentRoleKindPrefill {
			continue
		}
		// The same half hands the first token to decode, which is where SGLang records TTFT.
		if definition.name == "ttft" && md.Spec.Engine.Name == "sglang" && isModelDeploymentPD(md) &&
			modelDeploymentPodRoleKind(md, &read.pod) == workercore.ModelDeploymentRoleKindPrefill {
			continue
		}
		// The prefill half of a pair answers with the first token alone, so it never observes
		// a gap between two tokens and its time per output token stays at zero; expecting either
		// would mark every paired snapshot partial or publish a mean that measures nothing.
		if (definition.name == "itl" || definition.name == "tpot") && isModelDeploymentPD(md) &&
			modelDeploymentPodRoleKind(md, &read.pod) == workercore.ModelDeploymentRoleKindPrefill {
			continue
		}
		current, ok := read.value.windows[definition.name]
		if !ok {
			reason := "metric is absent"
			_, seen := read.value.windows[definition.pairedWith]
			switch {
			case definition.pairedWith == "":
			case definition.histogram && unobserved(definition.pairedWith):
				reason = modelDeploymentLabeledHistogramUnexported
			case !definition.histogram && seen:
				reason = modelDeploymentLabeledCounterUnexported
			}
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: definition.source, Reason: reason,
			})
			continue
		}
		previous, ok := h.replaceWindow(md, &read.pod, definition.source, current, read.at)
		if !ok {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: definition.source, Reason: "awaiting a second counter sample",
			})
			continue
		}
		window := read.at.Sub(previous.at).Seconds()
		count := current.count - previous.value.count
		sum := current.sum - previous.value.sum
		if count < 0 || sum < 0 {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: definition.source, Reason: "counter reset",
			})
			continue
		}
		deltas[definition.name] = count
		durations[definition.name] = window
		starts[definition.name] = previous.at
		if count == 0 && definition.histogram {
			// Only an idle Pod's latency histogram has no new sample to give. One that stands
			// still beside a TTFT that moved missed requests the Pod served.
			reason := "no observations in the sampling window"
			if definition.area == "latency" && unobserved("ttft") {
				reason = modelDeploymentIdleWindow
			}
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: definition.source, Reason: reason,
			})
			continue
		}
		value := count / window
		if definition.histogram {
			value = sum / count
		}
		metric := worker.ModelDeploymentMetricWindow{
			Name: definition.name, Pod: read.pod.Name, Source: definition.source,
			Scope: scope, Unit: definition.unit, Value: value, Samples: count,
			WindowSeconds: window, ObservedAt: meta.NewTime(read.at),
		}
		switch definition.area {
		case "latency":
			result.Latency = append(result.Latency, metric)
		case "traffic":
			result.Traffic = append(result.Traffic, metric)
		case "transfer":
			result.Transfer = append(result.Transfer, metric)
		}
	}
	if requests, ok := deltas["requests"]; ok {
		if errors, hasErrors := deltas["errors"]; hasErrors && !starts["requests"].Equal(starts["errors"]) {
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: sources["requests"] + "+" + sources["errors"],
				Reason: "request and error counters have different sampling windows",
			})
		} else if hasErrors && requests > 0 && errors <= requests {
			result.Traffic = append(result.Traffic, worker.ModelDeploymentMetricWindow{
				Name: "error-ratio", Pod: read.pod.Name, Source: sources["requests"] + "+" + sources["errors"],
				Scope: scope, Unit: "fraction", Value: errors / requests, Samples: requests,
				WindowSeconds: durations["requests"], ObservedAt: meta.NewTime(read.at),
			})
		} else if hasErrors {
			reason := "no requests in the sampling window"
			if errors > requests {
				reason = "error counter exceeds request counter"
			}
			result.Missing = append(result.Missing, worker.ModelDeploymentMetricMissing{
				Pod: read.pod.Name, Source: sources["requests"] + "+" + sources["errors"], Reason: reason,
			})
		}
	}
	return unobserved("ttft")
}

func (h *ModelDeploymentMetricsHandler) replaceWindow(
	md *workercore.ModelDeployment, pod *core.Pod, source string,
	value modelDeploymentWindowPair, at time.Time,
) (modelDeploymentPreviousWindow, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.windows == nil {
		h.windows = make(map[string]modelDeploymentPreviousWindow)
	}
	for key, sample := range h.windows {
		if at.Sub(sample.at) > 2*modelDeploymentCounterMaxAge {
			delete(h.windows, key)
		}
	}
	key := string(md.UID) + "/" + string(pod.UID) + "/" + source
	previous, ok := h.windows[key]
	h.windows[key] = modelDeploymentPreviousWindow{value: value, at: at}
	return previous, ok && at.Sub(previous.at) > 0 && at.Sub(previous.at) <= modelDeploymentCounterMaxAge
}

func mergeModelDeploymentGauge(gauges *[]worker.ModelDeploymentMetricGauge, next worker.ModelDeploymentMetricGauge) {
	for i := range *gauges {
		current := &(*gauges)[i]
		if current.Name == next.Name && current.Source == next.Source && current.Scope == next.Scope {
			current.Value += next.Value
			current.PodCount++
			if next.ObservedAt.After(current.ObservedAt.Time) {
				current.ObservedAt = next.ObservedAt
			}
			return
		}
	}
	*gauges = append(*gauges, next)
}

func (h *ModelDeploymentMetricsHandler) replaceCounter(
	md *workercore.ModelDeployment, pod *core.Pod, scope string,
	value modelDeploymentCounterPair, at time.Time,
) (modelDeploymentPreviousCounter, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.previous == nil {
		h.previous = make(map[string]modelDeploymentPreviousCounter)
	}
	for key, sample := range h.previous {
		if at.Sub(sample.at) > 2*modelDeploymentCounterMaxAge {
			delete(h.previous, key)
		}
	}
	key := string(md.UID) + "/" + string(pod.UID) + "/" + scope
	previous, ok := h.previous[key]
	h.previous[key] = modelDeploymentPreviousCounter{value: value, at: at}
	return previous, ok && at.Sub(previous.at) > 0 && at.Sub(previous.at) <= modelDeploymentCounterMaxAge
}
