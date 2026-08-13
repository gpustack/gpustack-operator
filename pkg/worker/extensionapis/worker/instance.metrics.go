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
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubemetrics"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/utils/json"
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

	// _InstanceMetricsTimeout bounds the whole metrics operation of one request,
	// including the kubelet read, the metrics API fallback, and one device manager fetch
	// per allocated manufacturer.
	_InstanceMetricsTimeout = 10 * time.Second

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

	// MaxAge is the age of a node's kubelet readout this handler still serves. The readout is
	// node-wide, so without it a client walking the Instances of one node re-reads the same
	// payload once per Instance. The zero value reads afresh every request.
	MaxAge time.Duration
}

func newInstanceMetricsHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *InstanceMetricsHandler {
	h := &InstanceMetricsHandler{
		APIReader: opts.Manager.GetAPIReader(),
		MaxAge:    kubemetrics.DefaultMaxAge,
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
	if uid := pod.Labels[deviceplugin.InstancePartOfLabelKey]; uid != string(inst.UID) {
		// The controller has not replaced the previous incarnation's pod yet: transient
		// backing state, same as an unscheduled Instance or a missing pod.
		return nil, kerrors.NewServiceUnavailable(
			fmt.Sprintf("backing pod of instance %s belongs to a previous incarnation", key))
	}

	// Current CPU/memory/storage usage of the backing Pod. The error already names every
	// source that failed and why, so it becomes the client-facing message unchanged.
	sample, err := kubemetrics.FetchPodSample(ctx, nodeName, pod, h.MaxAge)
	if err != nil {
		return nil, kerrors.NewServiceUnavailable(err.Error())
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

// filterAllocatedAcceleratorMetrics maps the snapshot's metrics for the allocated devices onto
// the API type. The filtering itself — the staleness bound and the manufacturer-and-ID match —
// belongs to the device manager that produced the snapshot, and is shared with its own exporter
// so the two surfaces cannot report different cards for one Instance.
func filterAllocatedAcceleratorMetrics(
	snapshot *devicemanager.MonitorSnapshot,
	allocGroups []workercore.DevicesAllocationGroup,
) []worker.InstanceAcceleratorMetrics {
	allocated := detector.AllocatedAcceleratorMetricsOf(snapshot, allocGroups)
	if len(allocated) == 0 {
		// Nil rather than an empty list, though the response cannot tell the two apart: the
		// field is omitempty, so both are omitted. What it therefore never distinguishes is
		// "this Instance holds no card" from "the device manager could not be read" — both
		// simply arrive carrying no accelerators.
		return nil
	}

	accelerators := make([]worker.InstanceAcceleratorMetrics, 0, len(allocated))
	for i := range allocated {
		accelerators = append(accelerators, toInstanceAcceleratorMetrics(&allocated[i].Metrics))
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
		if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" || !deviceplugin.IsPodReady(pod) {
			continue
		}
		if manufacturer != "" && pod.Labels[deviceplugin.ManufacturerLabelKey] != manufacturer {
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
		ctrlcli.MatchingLabels{deviceplugin.ComponentLabelKey: deviceplugin.DeviceManagerComponent},
		ctrlcli.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.nodeName", nodeName)},
		ctrlclix.WithoutQuorum,
		ctrlcli.UnsafeDisableDeepCopy,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list device manager pods on node %s: %w", nodeName, err)
	}
	return podList.Items, nil
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

// toInstanceAcceleratorMetrics converts the internal accelerator metrics to the API type,
// keeping the vendor-native MiB memory figures.
func toInstanceAcceleratorMetrics(am *device.AcceleratorMetrics) worker.InstanceAcceleratorMetrics {
	result := worker.InstanceAcceleratorMetrics{
		ID: am.ID,
	}
	result.MemoryTotalMiB = &am.Memory
	result.MemoryUsedMiB = &am.MemoryUsage
	result.MemoryUtilizationPercent = &am.MemoryUtilization
	result.CoresUtilizationPercent = &am.CoresUtilization
	result.TemperatureCelsius = &am.Temperature
	result.PowerUsageWatts = &am.PowerUsage
	result.Unhealthy = &am.Unhealthy
	return result
}
