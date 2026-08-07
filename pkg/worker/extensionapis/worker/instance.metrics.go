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
	klog "k8s.io/klog/v2"
	kubeletstats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
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
	"gpustack.ai/gpustack/pkg/utils/mathx"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const (
	// _DeviceManagerSecurePort is the default secure port of the device manager's
	// webserver, used when its container port is unnamed; it mirrors the chart's
	// deviceManager.securePort default.
	_DeviceManagerSecurePort = 32443

	// _DeviceManagerSecurePortName is the name of the device manager's secure container
	// port; the actual port is resolved from the pod spec so a chart-level
	// deviceManager.securePort override keeps working.
	_DeviceManagerSecurePortName = "https"

	// _DeviceManagerComponentLabelValue selects the device manager pods.
	_DeviceManagerComponentLabelValue = "device-manager"

	// _DeviceManagerManufacturerLabelKey carries the manufacturer of a device manager pod.
	_DeviceManagerManufacturerLabelKey = "gpustack.ai/manufacturer"

	// _InstancePartOfLabelKey is the pod label carrying the backing Instance's UID,
	// stamped by the Instance controller (pkg/worker/controllers/worker/instance.go).
	_InstancePartOfLabelKey = "app.kubernetes.io/part-of"

	// _InstanceMetricsTimeout bounds the whole metrics operation of one request,
	// including the kubelet read, the metrics API fallback, and one device manager fetch
	// per allocated manufacturer.
	_InstanceMetricsTimeout = 10 * time.Second

	// _MonitorSnapshotMaxAge is the fallback accepted age of a device manager snapshot
	// (three default monitor periods) when the snapshot does not report its period.
	_MonitorSnapshotMaxAge = 45 * time.Second

	// _MonitorSnapshotMaxBytes bounds a device manager snapshot readout, orders of
	// magnitude above one node's accelerator metrics.
	_MonitorSnapshotMaxBytes = 4 << 20
)

// metricsLogger records why the best-effort accelerator section came back empty:
// without it, a missing accelerators field is indistinguishable from "no cards allocated".
var metricsLogger = klog.Background().WithName("instance-metrics")

// deviceManagerSecurePortOf resolves the device manager's secure port from its pod spec,
// falling back to _DeviceManagerSecurePort when the port is unnamed.
func deviceManagerSecurePortOf(pod *core.Pod) int {
	for i := range pod.Spec.Containers {
		for j := range pod.Spec.Containers[i].Ports {
			if pod.Spec.Containers[i].Ports[j].Name == _DeviceManagerSecurePortName {
				return int(pod.Spec.Containers[i].Ports[j].ContainerPort)
			}
		}
	}
	return _DeviceManagerSecurePort
}

// InstanceMetricsHandler handles the "metrics" subresource of v1.Instance objects.
//
// InstanceMetricsHandler returns the current utilization of the v1.Instance's backing Pod —
// read in real time from the node kubelet through the API-server node proxy, with a
// metrics.k8s.io fallback for CPU/memory — merged with the allocated accelerators' metrics
// from the device manager's latest snapshot.
type InstanceMetricsHandler struct {
	extensionapi.GetOperation

	APIReader ctrlcli.Reader
}

func newInstanceMetricsHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *InstanceMetricsHandler {
	h := &InstanceMetricsHandler{
		APIReader: opts.Manager.GetAPIReader(),
	}

	// As storage.
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)

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
		// The controller has not replaced the previous incarnation's pod yet: transient
		// backing state, same as an unscheduled Instance or a missing pod.
		return nil, kerrors.NewServiceUnavailable(
			fmt.Sprintf("backing pod of instance %s belongs to a previous incarnation", key))
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
// falling back to the metrics.k8s.io API (CPU/memory only) when the kubelet read fails
// or when the kubelet does not know the pod yet.
func (h *InstanceMetricsHandler) currentPodUsage(
	ctx context.Context,
	nodeName string,
	pod *core.Pod,
) (*worker.InstanceMetricsSample, error) {
	summary, err := fetchKubeletSummaryViaNodeProxy(ctx, nodeName)
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
		// The kubelet answered but does not carry the pod, e.g. its CRI stats provider has
		// not picked it up after a restart. Fall back as well: an empty sample stamped with
		// a measurement that never happened is worse than the degraded CPU/memory figures.
		err = fmt.Errorf("node %s reports no stats for the pod", nodeName)
	}

	cpu, memory, ts, ferr := fetchPodUsageFromMetricsAPI(ctx, types.NamespacedName{
		Namespace: pod.Namespace, Name: pod.Name,
	})
	// The metrics entry was measured before this pod existed: it belongs to a
	// previous incarnation of the same name — never serve it.
	previousIncarnation := ferr == nil && cpu != nil && ts != nil && ts.Time.Before(pod.CreationTimestamp.Time)

	// Name the actual reason: "the metrics API failed", "it is not served here" and "it only
	// knows a previous pod of this name" are three different operator actions.
	switch {
	case ferr != nil:
		return nil, kerrors.NewServiceUnavailable(fmt.Sprintf(
			"failed to read the usage of instance pod %s/%s from the kubelet (%v) and from the metrics API (%v)",
			pod.Namespace, pod.Name, err, ferr))
	case previousIncarnation:
		return nil, kerrors.NewServiceUnavailable(fmt.Sprintf(
			"failed to read the usage of instance pod %s/%s from the kubelet (%v); "+
				"the metrics API only knows a previous incarnation of this pod name",
			pod.Namespace, pod.Name, err))
	case cpu == nil:
		return nil, kerrors.NewServiceUnavailable(fmt.Sprintf(
			"failed to read the usage of instance pod %s/%s from the kubelet (%v); "+
				"the metrics.k8s.io API is not served in this cluster",
			pod.Namespace, pod.Name, err))
	}

	sample := &worker.InstanceMetricsSample{
		CPUUsageNanoCores:   cpu,
		MemoryWorkingSetMiB: bytesToMiB(memory),
	}
	if ts != nil {
		sample.Timestamp = *ts
	} else {
		sample.Timestamp = meta.Now()
	}
	return sample, nil
}

// bytesToMiB converts an optional byte figure to MiB, keeping absence absent.
// The quotient rounds up so that any usage the kubelet measured stays visible: an idle
// instance's working set and writable layer are routinely under 1 MiB, and truncating
// them would present a measured figure as no usage at all.
func bytesToMiB(bytes *uint64) *uint64 {
	if bytes == nil {
		return nil
	}
	mib := mathx.CeilDiv(*bytes, uint64(quantityx.Mi))
	return &mib
}

// podUsageFromStats converts one kubelet pod stats entry into a metrics sample,
// tolerating any nil stat field (the kubelet omits them e.g. for fresh pods).
func podUsageFromStats(ps *kubeletstats.PodStats) *worker.InstanceMetricsSample {
	sample := &worker.InstanceMetricsSample{Timestamp: meta.Now()}
	if ps.CPU != nil {
		if !ps.CPU.Time.IsZero() {
			sample.Timestamp = ps.CPU.Time
		}
		if ps.CPU.UsageNanoCores != nil {
			sample.CPUUsageNanoCores = ps.CPU.UsageNanoCores
		}
	}
	if ps.Memory != nil {
		sample.MemoryWorkingSetMiB = bytesToMiB(ps.Memory.WorkingSetBytes)
	}
	if ps.EphemeralStorage != nil {
		sample.EphemeralStorageUsedMiB = bytesToMiB(ps.EphemeralStorage.UsedBytes)
	}
	// The summary carries no pod-level rootfs, aggregate the containers' write layers.
	// If any container's figure is missing, the total would mislead — report it absent.
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
			sample.RootfsUsedMiB = bytesToMiB(&sum)
		}
	}
	return sample
}

// currentAcceleratorMetrics reads the device manager's latest snapshot and filters it to the
// allocated device IDs. Any failure degrades to no accelerator section.
func (h *InstanceMetricsHandler) currentAcceleratorMetrics(
	ctx context.Context,
	nodeName string,
	allocGroups []workercore.DevicesAllocationGroup,
) []worker.InstanceAcceleratorMetrics {
	pods, err := h.listNodeDeviceManagerPods(ctx, nodeName)
	if err != nil {
		metricsLogger.Error(err, "listing device manager pods", "node", nodeName)
		return nil
	}

	// The chart rolls one device manager DaemonSet per manufacturer and passes
	// --manufacturer to it, so each snapshot only carries its own manufacturer's cards:
	// read one snapshot per allocated manufacturer instead of picking a single pod.
	var accelerators []worker.InstanceAcceleratorMetrics
	for _, manufacturer := range allocatedManufacturers(allocGroups) {
		pod := selectDeviceManagerPod(pods, manufacturer)
		if pod == nil {
			metricsLogger.V(2).Info("no ready device manager pod",
				"node", nodeName, "manufacturer", manufacturer)
			continue
		}

		url := "https://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(deviceManagerSecurePortOf(pod))) +
			devicemanager.MonitorSnapshotPath
		snapshot, ferr := fetchMonitorSnapshot(ctx, url)
		if ferr != nil {
			metricsLogger.Error(ferr, "reading device manager snapshot",
				"node", nodeName, "manufacturer", manufacturer, "pod", ctrlcli.ObjectKeyFromObject(pod))
			continue
		}
		accelerators = append(accelerators, filterAllocatedAcceleratorMetrics(snapshot, allocGroups)...)
	}
	return accelerators
}

// allocatedManufacturers returns the distinct manufacturers of an allocation,
// in the allocation's own order.
func allocatedManufacturers(allocGroups []workercore.DevicesAllocationGroup) []string {
	seen := make(map[string]struct{}, len(allocGroups))
	manufacturers := make([]string, 0, len(allocGroups))
	for i := range allocGroups {
		manufacturer := allocGroups[i].Manufacturer
		if _, ok := seen[manufacturer]; ok {
			continue
		}
		seen[manufacturer] = struct{}{}
		manufacturers = append(manufacturers, manufacturer)
	}
	return manufacturers
}

// filterAllocatedAcceleratorMetrics filters a device manager snapshot to the metrics of the
// devices recorded in the pod's allocation, keyed by manufacturer and device ID.
// A snapshot older than three monitor periods means the monitor is failing — the detector
// only replaces the snapshot after a successful non-empty sample — and yields nothing.
// The bound scales with the period the snapshot reports, falling back to
// _MonitorSnapshotMaxAge when the field is absent (older device managers).
func filterAllocatedAcceleratorMetrics(
	snapshot *devicemanager.MonitorSnapshot,
	allocGroups []workercore.DevicesAllocationGroup,
) []worker.InstanceAcceleratorMetrics {
	if snapshot == nil {
		return nil
	}
	maxAge := _MonitorSnapshotMaxAge
	if snapshot.PeriodSeconds > 0 {
		maxAge = time.Duration(snapshot.PeriodSeconds) * time.Second * 3
	}
	if time.Since(snapshot.Timestamp) > maxAge {
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

// selectDeviceManagerPod returns the Ready, non-terminating device manager pod serving the
// given manufacturer, or nil. Another manufacturer's pod is never an acceptable substitute:
// its snapshot cannot carry the cards we are asked about. An unlabeled manufacturer
// (an allocation predating the label) accepts any pod.
func selectDeviceManagerPod(pods []core.Pod, manufacturer string) *core.Pod {
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" || !isPodReady(pod) {
			continue
		}
		if manufacturer != "" && pod.Labels[_DeviceManagerManufacturerLabelKey] != manufacturer {
			continue
		}
		return pod
	}
	return nil
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
func fetchKubeletSummaryViaNodeProxy(ctx context.Context, nodeName string) (*kubeletstats.Summary, error) {
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

	// A response without containers carries no measurement: report it as unserved rather
	// than as a genuine zero usage.
	if len(podMetrics.Containers) == 0 {
		return nil, nil, nil, nil
	}

	// Any adapter may serve metrics.k8s.io, so a negative or absurd quantity is possible;
	// an unchecked unsigned conversion would turn it into exabytes of "current usage".
	var cpu, memory uint64
	for i := range podMetrics.Containers {
		if q, ok := podMetrics.Containers[i].Usage[core.ResourceCPU]; ok {
			cpu += nonNegative(q.ScaledValue(resource.Nano))
		}
		if q, ok := podMetrics.Containers[i].Usage[core.ResourceMemory]; ok {
			memory += nonNegative(q.Value())
		}
	}
	ts := &podMetrics.Timestamp
	if ts.IsZero() {
		ts = nil
	}
	return &cpu, &memory, ts, nil
}

// nonNegative clamps a signed measurement to an unsigned one.
func nonNegative(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// _monitorSnapshotClient is the shared client for device manager readouts: one keep-alive
// transport per process instead of one leaked per request.
var _monitorSnapshotClient = &http.Client{
	Transport: httpx.Transport(httpx.TransportOptions().WithoutProxy().WithTLSClientConfig(
		&tls.Config{InsecureSkipVerify: true})), // nolint: gosec
}

// fetchMonitorSnapshot GETs and decodes the monitor snapshot readout of a device manager pod.
// The caller owns the operation timeout.
//
// The device manager serves a self-signed certificate by default, so verification is skipped
// inside the same trust domain (see the spec's Risks for the mTLS follow-up). Proxying is
// disabled explicitly: the project's transport helpers honor proxy env vars, which must never
// reroute pod-to-pod traffic.
func fetchMonitorSnapshot(ctx context.Context, url string) (*devicemanager.MonitorSnapshot, error) {
	client := _monitorSnapshotClient

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create monitor snapshot request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	// Verification is skipped, so the peer's identity is not established: bound both the
	// decode and the keep-alive drain instead of trusting it to stop sending.
	body := io.LimitReader(resp.Body, _MonitorSnapshotMaxBytes)
	defer func() {
		_, _ = io.Copy(io.Discard, body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	snapshot := &devicemanager.MonitorSnapshot{}
	if err = json.NewDecoder(body).Decode(snapshot); err != nil {
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
// keeping the vendor-native MiB memory figures.
func toInstanceAcceleratorMetrics(am *device.AcceleratorMetrics) worker.InstanceAcceleratorMetrics {
	result := worker.InstanceAcceleratorMetrics{
		ID: am.ID,
	}
	result.MemoryMiB = &am.Memory
	result.MemoryUsageMiB = &am.MemoryUsage
	result.MemoryUtilizationPercent = &am.MemoryUtilization
	result.CoresUtilizationPercent = &am.CoresUtilization
	result.TemperatureCelsius = &am.Temperature
	result.PowerUsageWatts = &am.PowerUsage
	result.Unhealthy = &am.Unhealthy
	return result
}
