package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const (
	// _DeviceManagerSecurePort is the secure port of the device manager's webserver,
	// which serves the monitor snapshot readout (devicemanager.MonitorSnapshotPath).
	// It mirrors the chart's deviceManager.securePort value.
	_DeviceManagerSecurePort = 32443

	// _DeviceManagerComponentLabelValue selects the device manager pods.
	_DeviceManagerComponentLabelValue = "device-manager"

	// _DeviceManagerManufacturerLabelKey carries the manufacturer of a device manager pod.
	_DeviceManagerManufacturerLabelKey = "gpustack.ai/manufacturer"

	// _InstancePartOfLabelKey is the pod label carrying the backing Instance's UID,
	// stamped by the Instance controller (pkg/worker/controllers/worker/instance.go).
	_InstancePartOfLabelKey = "app.kubernetes.io/part-of"

	// _InstanceMetricsTimeout bounds the whole metrics operation of one request,
	// including the kubelet read, the metrics API fallback, and the device manager fetch
	// with one retry.
	_InstanceMetricsTimeout = 10 * time.Second
)

// InstanceMetricsHandler handles the "metrics" subresource of v1.Instance objects.
//
// InstanceMetricsHandler returns the current utilization of the v1.Instance's backing Pod —
// read in real time from the node kubelet through the API-server node proxy, with a
// metrics.k8s.io fallback for CPU/memory — merged with the allocated accelerators' metrics
// from the device manager's latest snapshot.
type InstanceMetricsHandler struct {
	extensionapi.GetOperation

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader

	// Injectable seams for testing.

	// fetchKubeletSummary returns the node kubelet's stats summary.
	fetchKubeletSummary func(ctx context.Context, nodeName string) (*statsv1alpha1.Summary, error)
	// fetchPodUsageFallback returns the pod's CPU (nanocores), memory (bytes) and the
	// measurement timestamp from the metrics.k8s.io API; a nil-cpu return means the API
	// is not served or has no usable entry.
	fetchPodUsageFallback func(ctx context.Context, key types.NamespacedName) (*uint64, *uint64, *meta.Time, error)
	// listDeviceManagerPods returns the candidate device manager pods of the given node.
	listDeviceManagerPods func(ctx context.Context, nodeName string) ([]core.Pod, error)
	// fetchMonitorSnapshot fetches and decodes the monitor snapshot readout from the given URL.
	fetchMonitorSnapshot func(ctx context.Context, url string) (*devicemanager.MonitorSnapshot, error)
}

func newInstanceMetricsHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *InstanceMetricsHandler {
	h := &InstanceMetricsHandler{
		Client:    opts.Manager.GetClient(),
		APIReader: opts.Manager.GetAPIReader(),
	}

	// As storage.
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)

	// Set production seams.
	h.fetchKubeletSummary = fetchKubeletSummaryViaNodeProxy
	h.fetchPodUsageFallback = fetchPodUsageFromMetricsAPI
	h.listDeviceManagerPods = h.listNodeDeviceManagerPods
	h.fetchMonitorSnapshot = fetchMonitorSnapshot

	return h
}

var (
	_ rest.Storage = (*InstanceMetricsHandler)(nil)
	_ rest.Getter  = (*InstanceMetricsHandler)(nil)
)

func (h *InstanceMetricsHandler) New() runtime.Object {
	return &worker.InstanceMetrics{}
}

func (h *InstanceMetricsHandler) Destroy() {
}

func (h *InstanceMetricsHandler) OnGet(ctx context.Context, key types.NamespacedName, _ ctrlcli.GetOptions) (runtime.Object, error) {
	// Bound the whole operation, including the object reads below.
	ctx, cancel := context.WithTimeout(ctx, _InstanceMetricsTimeout)
	defer cancel()

	// Get the backing Instance.
	inst := &workercore.Instance{}
	err := h.APIReader.Get(ctx, key, inst, ctrlclix.WithoutQuorum)
	if err != nil {
		return nil, err
	}
	nodeName := inst.Status.NodeName
	if nodeName == "" {
		return nil, kerrors.NewServiceUnavailable(
			fmt.Sprintf("instance %s is not scheduled to any node yet", key))
	}

	// Get the backing Pod, which must still belong to this very Instance
	// (a deleted-and-recreated Instance must not read the previous one's metrics).
	pod := &core.Pod{}
	err = h.APIReader.Get(ctx, key, pod, ctrlclix.WithoutQuorum)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, kerrors.NewServiceUnavailable(
				fmt.Sprintf("instance %s has no backing pod yet", key))
		}
		return nil, err
	}
	if uid := pod.Labels[_InstancePartOfLabelKey]; uid != string(inst.UID) {
		return nil, kerrors.NewConflict(
			worker.SchemeGroupVersionResource(_InstanceResource).GroupResource(), key.String(),
			fmt.Errorf("backing pod of instance %s is not owned by this instance", key))
	}

	// Current CPU/memory/storage usage of the backing Pod.
	sample, err := h.currentPodUsage(ctx, nodeName, pod)
	if err != nil {
		return nil, err
	}

	// Best-effort merge of the allocated accelerators' latest metrics:
	// the pod usage above is authoritative, a device manager failure only
	// drops the accelerator section.
	if allocGroups := deviceplugin.AllocatedAcceleratorGroupsOf(pod); len(allocGroups) != 0 {
		sample.Accelerators = h.currentAcceleratorMetrics(ctx, nodeName, allocGroups)
	}

	return &worker.InstanceMetrics{
		ObjectMeta: meta.ObjectMeta{
			Namespace: inst.Namespace,
			Name:      inst.Name,
			UID:       inst.UID,
		},
		Sample: *sample,
	}, nil
}

// currentPodUsage returns the backing pod's current usage from the node kubelet,
// falling back to the metrics.k8s.io API (CPU/memory only) when the kubelet read fails.
func (h *InstanceMetricsHandler) currentPodUsage(
	ctx context.Context,
	nodeName string,
	pod *core.Pod,
) (*worker.InstanceMetricsSample, error) {
	summary, err := h.fetchKubeletSummary(ctx, nodeName)
	if err == nil {
		for i := range summary.Pods {
			ps := &summary.Pods[i]
			// Match on the pod UID as well: the kubelet summary is node-wide, and a
			// recreated pod must never leak the previous incarnation's figures.
			if ps.PodRef.Namespace != pod.Namespace || ps.PodRef.Name != pod.Name ||
				ps.PodRef.UID != string(pod.UID) {
				continue
			}
			return podUsageFromStats(ps), nil
		}
		// The pod is not (yet) in the summary, e.g. it just started.
		return &worker.InstanceMetricsSample{Timestamp: meta.Now()}, nil
	}

	cpu, memory, ts, ferr := h.fetchPodUsageFallback(ctx, types.NamespacedName{
		Namespace: pod.Namespace, Name: pod.Name,
	})
	if ferr == nil && cpu != nil && ts != nil && ts.Time.Before(pod.CreationTimestamp.Time) {
		// The metrics entry was measured before this pod existed: it belongs to a
		// previous incarnation of the same name — never serve it.
		cpu, memory, ts = nil, nil, nil
	}
	if ferr != nil || cpu == nil {
		return nil, kerrors.NewServiceUnavailable(fmt.Sprintf(
			"failed to read the usage of instance pod %s/%s from the kubelet (%v) and the metrics API (%v)",
			pod.Namespace, pod.Name, err, ferr))
	}
	sample := &worker.InstanceMetricsSample{
		CPUUsageNanoCores:     cpu,
		MemoryWorkingSetBytes: memory,
	}
	if ts != nil {
		sample.Timestamp = *ts
	} else {
		sample.Timestamp = meta.Now()
	}
	return sample, nil
}

// podUsageFromStats converts one kubelet pod stats entry into a metrics sample,
// tolerating any nil stat field (the kubelet omits them e.g. for fresh pods).
func podUsageFromStats(ps *statsv1alpha1.PodStats) *worker.InstanceMetricsSample {
	sample := &worker.InstanceMetricsSample{Timestamp: meta.Now()}
	if ps.CPU != nil {
		if !ps.CPU.Time.IsZero() {
			sample.Timestamp = ps.CPU.Time
		}
		if ps.CPU.UsageNanoCores != nil {
			sample.CPUUsageNanoCores = ps.CPU.UsageNanoCores
		}
	}
	if ps.Memory != nil && ps.Memory.WorkingSetBytes != nil {
		sample.MemoryWorkingSetBytes = ps.Memory.WorkingSetBytes
	}
	if ps.EphemeralStorage != nil && ps.EphemeralStorage.UsedBytes != nil {
		sample.EphemeralStorageUsedBytes = ps.EphemeralStorage.UsedBytes
	}
	// The summary carries no pod-level rootfs, aggregate the containers' write layers.
	// If any container's figure is missing, the total would mislead — report it absent.
	var rootfs *uint64
	if len(ps.Containers) != 0 {
		sum := uint64(0)
		complete := true
		for i := range ps.Containers {
			r := ps.Containers[i].Rootfs
			if r == nil || r.UsedBytes == nil {
				complete = false
				break
			}
			sum += *r.UsedBytes
		}
		if complete {
			rootfs = &sum
		}
	}
	sample.RootfsUsedBytes = rootfs
	return sample
}

// currentAcceleratorMetrics reads the device manager's latest snapshot and filters it to the
// allocated device IDs. Any failure degrades to no accelerator section.
func (h *InstanceMetricsHandler) currentAcceleratorMetrics(
	ctx context.Context,
	nodeName string,
	allocGroups []workercore.DevicesAllocationGroup,
) []worker.InstanceAcceleratorMetrics {
	fetch := func() (*devicemanager.MonitorSnapshot, error) {
		pod, err := h.selectDeviceManagerPod(ctx, nodeName, allocGroups)
		if err != nil {
			return nil, err
		}
		url := "https://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(_DeviceManagerSecurePort)) +
			devicemanager.MonitorSnapshotPath
		return h.fetchMonitorSnapshot(ctx, url)
	}

	snapshot, err := fetch()
	if err != nil {
		// Retry once with a fresh resolution, the previously resolved pod may be gone.
		snapshot, err = fetch()
	}
	if err != nil || snapshot == nil {
		return nil
	}

	allocatedByManufacturer := make(map[string]map[string]struct{}, len(allocGroups))
	for i := range allocGroups {
		ids := allocatedByManufacturer[allocGroups[i].Manufacturer]
		if ids == nil {
			ids = make(map[string]struct{})
			allocatedByManufacturer[allocGroups[i].Manufacturer] = ids
		}
		for j := range allocGroups[i].Accelerators {
			ids[allocGroups[i].Accelerators[j].ID] = struct{}{}
		}
	}

	var accelerators []worker.InstanceAcceleratorMetrics
	for i := range snapshot.Groups {
		grp := &snapshot.Groups[i]
		allocated, ok := allocatedByManufacturer[grp.Manufacturer]
		if !ok {
			continue
		}
		for j := range grp.Accelerators {
			am := &grp.Accelerators[j]
			if _, ok = allocated[am.ID]; !ok {
				continue
			}
			accelerators = append(accelerators, toInstanceAcceleratorMetrics(am))
		}
	}
	return accelerators
}

// selectDeviceManagerPod returns the device manager pod to read from: Ready, non-terminating,
// on the given node, preferring the manufacturer of the allocated accelerators.
func (h *InstanceMetricsHandler) selectDeviceManagerPod(
	ctx context.Context,
	nodeName string,
	allocGroups []workercore.DevicesAllocationGroup,
) (*core.Pod, error) {
	pods, err := h.listDeviceManagerPods(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	preferredManufacturer := ""
	if len(allocGroups) != 0 {
		preferredManufacturer = allocGroups[0].Manufacturer
	}

	var fallback *core.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" || !isPodReady(pod) {
			continue
		}
		if preferredManufacturer != "" &&
			pod.Labels[_DeviceManagerManufacturerLabelKey] == preferredManufacturer {
			return pod, nil
		}
		if fallback == nil {
			fallback = pod
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("no ready device manager pod found on node %s", nodeName)
}

// listNodeDeviceManagerPods lists the device manager pods of the given node
// in the system namespace.
func (h *InstanceMetricsHandler) listNodeDeviceManagerPods(ctx context.Context, nodeName string) ([]core.Pod, error) {
	podList := &core.PodList{}
	err := h.APIReader.List(ctx, podList,
		ctrlcli.InNamespace(kuberess.SystemNamespaceName),
		ctrlcli.MatchingLabels{"app.kubernetes.io/component": _DeviceManagerComponentLabelValue},
		ctrlcli.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.nodeName", nodeName)},
		ctrlclix.WithoutQuorum,
		ctrlcli.UnsafeDisableDeepCopy,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list device manager pods on node %s: %w", nodeName, err)
	}
	return podList.Items, nil
}

// fetchKubeletSummaryViaNodeProxy reads the node kubelet's stats summary through the
// API-server node proxy: no address resolution or kubelet TLS handling is needed, and the
// worker's RBAC already covers it.
func fetchKubeletSummaryViaNodeProxy(ctx context.Context, nodeName string) (*statsv1alpha1.Summary, error) {
	raw, err := system.LoopbackKubeClient.Get().CoreV1().RESTClient().Get().
		Resource("nodes").Name(nodeName).SubResource("proxy").
		Suffix("stats", "summary").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubelet stats summary of node %s: %w", nodeName, err)
	}

	summary := &statsv1alpha1.Summary{}
	if err = json.Unmarshal(raw, summary); err != nil {
		return nil, fmt.Errorf("failed to decode kubelet stats summary of node %s: %w", nodeName, err)
	}
	return summary, nil
}

// fetchPodUsageFromMetricsAPI reads the pod's CPU/memory from the metrics.k8s.io API via the
// generic REST client (no k8s.io/metrics dependency). A nil-cpu return means the API is not
// served in this cluster.
func fetchPodUsageFromMetricsAPI(ctx context.Context, key types.NamespacedName) (*uint64, *uint64, *meta.Time, error) {
	raw, err := system.LoopbackKubeClient.Get().CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces/" + key.Namespace + "/pods/" + key.Name).
		DoRaw(ctx)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("failed to get pod metrics of %s: %w", key, err)
	}
	return parsePodMetricsUsage(raw)
}

// parsePodMetricsUsage decodes the metrics.k8s.io PodMetrics shape and sums the
// containers' usage: CPU in nanocores, memory in bytes. The measurement timestamp
// lets the caller reject entries that predate the pod (a previous incarnation).
func parsePodMetricsUsage(raw []byte) (*uint64, *uint64, *meta.Time, error) {
	var podMetrics struct {
		Timestamp  meta.Time `json:"timestamp"`
		Containers []struct {
			Usage map[core.ResourceName]resource.Quantity `json:"usage"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(raw, &podMetrics); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to decode pod metrics: %w", err)
	}

	var cpu, memory uint64
	for i := range podMetrics.Containers {
		if q, ok := podMetrics.Containers[i].Usage[core.ResourceCPU]; ok {
			cpu += uint64(q.ScaledValue(resource.Nano))
		}
		if q, ok := podMetrics.Containers[i].Usage[core.ResourceMemory]; ok {
			memory += uint64(q.Value())
		}
	}
	ts := &podMetrics.Timestamp
	if ts.IsZero() {
		ts = nil
	}
	return &cpu, &memory, ts, nil
}

// fetchMonitorSnapshot GETs and decodes the monitor snapshot readout of a device manager pod.
// The caller owns the operation timeout.
//
// The device manager serves a self-signed certificate by default, so verification is skipped
// inside the same trust domain (see the spec's Risks for the mTLS follow-up). Proxying is
// disabled explicitly: the project's transport helpers honor proxy env vars, which must never
// reroute pod-to-pod traffic.
func fetchMonitorSnapshot(ctx context.Context, url string) (*devicemanager.MonitorSnapshot, error) {
	rt := httpx.Transport(httpx.TransportOptions().WithoutProxy().WithTLSClientConfig(
		&tls.Config{InsecureSkipVerify: true})) // nolint: gosec
	client := &http.Client{Transport: rt}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create monitor snapshot request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	snapshot := &devicemanager.MonitorSnapshot{}
	if err = json.NewDecoder(resp.Body).Decode(snapshot); err != nil {
		return nil, fmt.Errorf("failed to decode monitor snapshot: %w", err)
	}
	return snapshot, nil
}

// isPodReady reports whether the pod carries a true Ready condition.
func isPodReady(pod *core.Pod) bool {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == core.PodReady {
			return pod.Status.Conditions[i].Status == core.ConditionTrue
		}
	}
	return false
}

// toInstanceAcceleratorMetrics converts the internal accelerator metrics to the API type,
// scaling the memory figures from MiB to bytes.
func toInstanceAcceleratorMetrics(am *device.AcceleratorMetrics) worker.InstanceAcceleratorMetrics {
	result := worker.InstanceAcceleratorMetrics{
		ID: am.ID,
	}
	memoryBytes := am.Memory << 20
	result.MemoryBytes = &memoryBytes
	memoryUsageBytes := am.MemoryUsage << 20
	result.MemoryUsageBytes = &memoryUsageBytes
	result.MemoryUtilizationPercent = &am.MemoryUtilization
	result.CoresUtilizationPercent = &am.CoresUtilization
	result.TemperatureCelsius = &am.Temperature
	result.PowerUsageWatts = &am.PowerUsage
	result.Unhealthy = &am.Unhealthy
	return result
}
