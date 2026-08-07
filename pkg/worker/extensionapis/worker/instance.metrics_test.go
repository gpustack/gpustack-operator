package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	kubeletstats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/json"
)

const (
	_metricsTestNS          = "default"
	_metricsTestName        = "test-instance"
	_metricsTestUID         = "instance-uid-1"
	_metricsTestNode        = "node-1"
	_metricsTestDMNamespace = "gpustack-system"
)

var _metricsTestPodUID = types.UID("pod-uid-1")

// metricsAPIHandler dispatches the fake API server for the metrics tests:
// the handler reads the backing pod's stats through the loopback Kubernetes
// client, so the tests stand up a fake API server for it.
var metricsAPIHandler atomic.Value

func TestMain(m *testing.M) {
	metricsAPIHandler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metricsAPIHandler.Load().(http.Handler).ServeHTTP(w, r)
	}))

	cfg := &rest.Config{Host: srv.URL}
	system.LoopbackKubeRestConfig.Configure(*cfg)
	system.LoopbackKubeClient.Configure(kubernetes.NewForConfigOrDie(cfg))

	code := m.Run()
	srv.Close()
	os.Exit(code)
}

// useFakeAPI swaps the fake API server's handler for the duration of a test.
func useFakeAPI(t *testing.T, h http.Handler) {
	t.Helper()
	metricsAPIHandler.Store(http.HandlerFunc(h.ServeHTTP))
	t.Cleanup(func() {
		metricsAPIHandler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
	})
}

// metricsTestInstance returns a scheduled Instance backed by the test pod.
func metricsTestInstance() *workercore.Instance {
	return &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{
			Namespace: _metricsTestNS,
			Name:      _metricsTestName,
			UID:       types.UID(_metricsTestUID),
		},
		Status: workercore.InstanceStatus{
			NodeName: _metricsTestNode,
		},
	}
}

// metricsTestPod returns the backing pod of the test Instance, allocated one nvidia card.
func metricsTestPod() *core.Pod {
	allocations := deviceplugin.PodAllocations{
		"main": {
			Devices: workercore.DevicesStatus{
				Groups: []workercore.DevicesAllocationGroup{
					{
						ID:           "gpu-group",
						Manufacturer: "nvidia",
						Accelerators: []workercore.AcceleratorAllocation{
							{ID: "gpu-uuid-1"},
						},
					},
				},
			},
		},
	}
	anno, _ := json.Marshal(allocations)
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: _metricsTestNS,
			Name:      _metricsTestName,
			UID:       _metricsTestPodUID,
			Labels:    map[string]string{_InstancePartOfLabelKey: _metricsTestUID},
			Annotations: map[string]string{
				deviceplugin.AllocatedAcceleratorAnnoKey: string(anno),
			},
		},
	}
}

// metricsTestDMPod returns a Ready device manager pod on the test node.
func metricsTestDMPod() *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: _metricsTestDMNamespace,
			Name:      "dm-nvidia-abc",
			Labels: map[string]string{
				"app.kubernetes.io/component":      _DeviceManagerComponentLabelValue,
				_DeviceManagerManufacturerLabelKey: "nvidia",
			},
		},
		Spec: core.PodSpec{
			NodeName: _metricsTestNode,
			Containers: []core.Container{
				{
					Name:  "device-manager",
					Ports: []core.ContainerPort{{Name: "https", ContainerPort: 32443}},
				},
			},
		},
		Status: core.PodStatus{
			PodIP: "10.0.0.9",
			Conditions: []core.PodCondition{
				{Type: core.PodReady, Status: core.ConditionTrue},
			},
		},
	}
}

// metricsTestSummary returns a kubelet summary holding the test pod's stats,
// another tenant's pod, and a stale same-name entry of a previous incarnation.
func metricsTestSummary() *kubeletstats.Summary {
	cpu := uint64(500_000_000)
	mem := uint64(1 << 30)
	eph := uint64(3 << 30)
	rootfs := uint64(2 << 30)
	foreignCPU := uint64(999)
	staleCPU := uint64(777)
	return &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{
			{
				PodRef: kubeletstats.PodReference{
					Namespace: _metricsTestNS, Name: _metricsTestName, UID: string(_metricsTestPodUID),
				},
				CPU:              &kubeletstats.CPUStats{Time: meta.Now(), UsageNanoCores: &cpu},
				Memory:           &kubeletstats.MemoryStats{WorkingSetBytes: &mem},
				EphemeralStorage: &kubeletstats.FsStats{UsedBytes: &eph},
				Containers: []kubeletstats.ContainerStats{
					{Name: "main", Rootfs: &kubeletstats.FsStats{UsedBytes: &rootfs}},
				},
			},
			{
				PodRef: kubeletstats.PodReference{Namespace: "other", Name: "other", UID: "uid-2"},
				CPU:    &kubeletstats.CPUStats{Time: meta.Now(), UsageNanoCores: &foreignCPU},
			},
			{
				// Previous incarnation: same name, different UID.
				PodRef: kubeletstats.PodReference{
					Namespace: _metricsTestNS, Name: _metricsTestName, UID: "stale-pod-uid",
				},
				CPU: &kubeletstats.CPUStats{Time: meta.Now(), UsageNanoCores: &staleCPU},
			},
		},
	}
}

// metricsTestSnapshot returns a device manager snapshot with the allocated card
// and a foreign card.
func metricsTestSnapshot() *devicemanager.MonitorSnapshot {
	return &devicemanager.MonitorSnapshot{
		Timestamp: time.Now(),
		Groups: []device.MetricsGroup{
			{
				Manufacturer: "nvidia",
				Timestamp:    time.Now(),
				Accelerators: []device.AcceleratorMetrics{
					{ID: "gpu-uuid-1", Memory: 81920, MemoryUsage: 1024, MemoryUtilization: 12, CoresUtilization: 34, Temperature: 42, PowerUsage: 120},
					{ID: "gpu-uuid-foreign", Memory: 81920},
				},
			},
		},
	}
}

// serveSummary lets the fake API server answer the node-proxy kubelet read.
func serveSummary(t *testing.T, summary *kubeletstats.Summary, err error) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes/"+_metricsTestNode+"/proxy/stats/summary",
		func(w http.ResponseWriter, _ *http.Request) {
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			bs, _ := json.Marshal(summary)
			_, _ = w.Write(bs)
		})
	useFakeAPI(t, mux)
}

// serveSummaryAndFallback lets the fake API server answer the kubelet read
// and the metrics.k8s.io fallback.
func serveSummaryAndFallback(t *testing.T, summaryErr error, podMetricsPayload string, podMetricsStatus int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes/"+_metricsTestNode+"/proxy/stats/summary",
		func(w http.ResponseWriter, _ *http.Request) {
			if summaryErr != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/namespaces/"+_metricsTestNS+"/pods/"+_metricsTestName,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(podMetricsStatus)
			_, _ = w.Write([]byte(podMetricsPayload))
		})
	useFakeAPI(t, mux)
}

// newMetricsTestHandler builds a handler backed by a fake controller client.
func newMetricsTestHandler(t *testing.T, dmPods []core.Pod) *InstanceMetricsHandler {
	t.Helper()
	return newMetricsTestHandlerWith(t, metricsTestInstance(), metricsTestPod(), dmPods)
}

func newMetricsTestHandlerWith(
	t *testing.T,
	inst *workercore.Instance,
	pod *core.Pod,
	dmPods []core.Pod,
) *InstanceMetricsHandler {
	t.Helper()

	builder := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(inst, pod).
		WithIndex(&core.Pod{}, "spec.nodeName", func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		})
	for i := range dmPods {
		builder = builder.WithObjects(&dmPods[i])
	}
	cli := builder.Build()
	return &InstanceMetricsHandler{APIReader: cli}
}

func metricsTestKey() types.NamespacedName {
	return types.NamespacedName{Namespace: _metricsTestNS, Name: _metricsTestName}
}

func TestInstanceMetricsHandler_OnGet(t *testing.T) {
	t.Run("unavailable when unscheduled", func(t *testing.T) {
		inst := metricsTestInstance()
		inst.Status.NodeName = ""
		h := newMetricsTestHandlerWith(t, inst, metricsTestPod(), nil)

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
	})

	t.Run("unavailable without a backing pod", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(metricsTestInstance()).Build()
		h := &InstanceMetricsHandler{APIReader: cli}

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
	})

	t.Run("rejects a pod owned by a previous incarnation", func(t *testing.T) {
		pod := metricsTestPod()
		pod.Labels[_InstancePartOfLabelKey] = "stale-uid"
		h := newMetricsTestHandlerWith(t, metricsTestInstance(), pod, nil)

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		// Transient backing state, like an unscheduled Instance: the caller should retry.
		assert.True(t, kerrors.IsServiceUnavailable(err))
	})

	t.Run("returns the current instance-scoped sample", func(t *testing.T) {
		serveSummary(t, metricsTestSummary(), nil)
		h := newMetricsTestHandler(t, []core.Pod{*metricsTestDMPod()})

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		metrics, ok := obj.(*worker.InstanceMetrics)
		require.True(t, ok)
		sample := metrics.Sample

		// The UID-matched entry only: neither the other tenant's nor the stale incarnation's.
		require.NotNil(t, sample.CPUUsageNanoCores)
		assert.Equal(t, uint64(500_000_000), *sample.CPUUsageNanoCores)
		// The kubelet measures in bytes, the sample reports MiB.
		assert.Equal(t, uint64(1024), *sample.MemoryWorkingSetMiB)
		assert.Equal(t, uint64(2048), *sample.RootfsUsedMiB)
		assert.Equal(t, uint64(3072), *sample.EphemeralStorageUsedMiB)

		// The device manager pod IP is unreachable in the unit environment,
		// so the accelerator section degrades away; merge logic is covered
		// by the pure filter tests below.
		assert.Empty(t, sample.Accelerators)
	})

	t.Run("falls back when the kubelet does not carry the pod yet", func(t *testing.T) {
		// The kubelet answers but its stats provider has no entry for the pod: degraded
		// CPU/memory beats a sample stamped with a measurement that never happened.
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/nodes/"+_metricsTestNode+"/proxy/stats/summary",
			func(w http.ResponseWriter, _ *http.Request) {
				bs, _ := json.Marshal(&kubeletstats.Summary{})
				_, _ = w.Write(bs)
			})
		mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/namespaces/"+_metricsTestNS+"/pods/"+_metricsTestName,
			func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"kind":"PodMetrics","timestamp":"` +
					time.Now().UTC().Format(time.RFC3339) +
					`","containers":[{"name":"main","usage":{"cpu":"250m","memory":"512Mi"}}]}`))
			})
		useFakeAPI(t, mux)
		h := newMetricsTestHandler(t, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		require.NotNil(t, sample.CPUUsageNanoCores)
		assert.Equal(t, uint64(250_000_000), *sample.CPUUsageNanoCores)
	})

	t.Run("unavailable when the kubelet does not carry the pod and no metrics API exists", func(t *testing.T) {
		serveSummary(t, &kubeletstats.Summary{}, nil)
		h := newMetricsTestHandler(t, nil)

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
		assert.Contains(t, err.Error(), "metrics.k8s.io API is not served",
			"the message must name the actual reason, not a nil error")
	})

	t.Run("falls back to the metrics API when the kubelet read fails", func(t *testing.T) {
		serveSummaryAndFallback(t, fmt.Errorf("kubelet down"),
			`{"kind":"PodMetrics","timestamp":"`+time.Now().UTC().Format(time.RFC3339)+`",`+
				`"containers":[{"name":"main","usage":{"cpu":"250m","memory":"512Mi"}}]}`,
			http.StatusOK)
		h := newMetricsTestHandler(t, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		require.NotNil(t, sample.CPUUsageNanoCores)
		assert.Equal(t, uint64(250_000_000), *sample.CPUUsageNanoCores)
		assert.Equal(t, uint64(512), *sample.MemoryWorkingSetMiB)
		assert.Nil(t, sample.RootfsUsedMiB, "the fallback carries no disk figures")
		assert.Nil(t, sample.EphemeralStorageUsedMiB)
	})

	t.Run("unavailable when both sources fail", func(t *testing.T) {
		serveSummaryAndFallback(t, fmt.Errorf("kubelet down"), "", http.StatusNotFound)
		h := newMetricsTestHandler(t, nil)

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
	})

	t.Run("rejects a fallback entry measured before this pod existed", func(t *testing.T) {
		// A recreated pod keeps the name; the metrics API may still serve the
		// previous incarnation's entry — it must not be presented as current.
		pod := metricsTestPod()
		pod.CreationTimestamp = meta.Now()
		serveSummaryAndFallback(t, fmt.Errorf("kubelet down"),
			`{"kind":"PodMetrics","timestamp":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`",`+
				`"containers":[{"name":"main","usage":{"cpu":"250m","memory":"512Mi"}}]}`,
			http.StatusOK)
		h := newMetricsTestHandlerWith(t, metricsTestInstance(), pod, nil)

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
	})

	t.Run("keeps the pod stats when the device manager is unreachable", func(t *testing.T) {
		// AC3.2: the accelerator section is best-effort, the pod usage is authoritative.
		serveSummary(t, metricsTestSummary(), nil)
		dmPod := metricsTestDMPod()
		dmPod.Status.PodIP = "127.0.0.1"
		// A port nothing listens on: the fetch fails, the request must not.
		dmPod.Spec.Containers[0].Ports[0].ContainerPort = 1
		h := newMetricsTestHandler(t, []core.Pod{*dmPod})

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.Empty(t, sample.Accelerators)
		require.NotNil(t, sample.CPUUsageNanoCores)
		assert.Equal(t, uint64(500_000_000), *sample.CPUUsageNanoCores)
		assert.Equal(t, uint64(1024), *sample.MemoryWorkingSetMiB)
	})

	t.Run("keeps the pod stats when no device manager pod is ready", func(t *testing.T) {
		serveSummary(t, metricsTestSummary(), nil)
		h := newMetricsTestHandler(t, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.Empty(t, sample.Accelerators)
		assert.NotNil(t, sample.CPUUsageNanoCores)
	})

	t.Run("keeps the pod stats when the allocation annotation is malformed", func(t *testing.T) {
		serveSummary(t, metricsTestSummary(), nil)
		pod := metricsTestPod()
		pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = "{not-json"
		h := newMetricsTestHandlerWith(t, metricsTestInstance(), pod, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.Empty(t, sample.Accelerators)
		assert.NotNil(t, sample.CPUUsageNanoCores)
	})

	t.Run("keeps a sub-MiB measurement visible", func(t *testing.T) {
		// An idle instance's working set and writable layer are routinely under 1 MiB;
		// truncating them to 0 would present a measured figure as no usage at all.
		summary := metricsTestSummary()
		mem, eph, rootfs := uint64(585_728), uint64(20_480), uint64(12_288)
		summary.Pods[0].Memory.WorkingSetBytes = &mem
		summary.Pods[0].EphemeralStorage.UsedBytes = &eph
		summary.Pods[0].Containers[0].Rootfs.UsedBytes = &rootfs
		serveSummary(t, summary, nil)
		h := newMetricsTestHandler(t, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.Equal(t, uint64(1), *sample.MemoryWorkingSetMiB)
		assert.Equal(t, uint64(1), *sample.EphemeralStorageUsedMiB)
		assert.Equal(t, uint64(1), *sample.RootfsUsedMiB)
	})

	t.Run("reports zero only when the source measured no usage", func(t *testing.T) {
		summary := metricsTestSummary()
		zero := uint64(0)
		summary.Pods[0].Memory.WorkingSetBytes = &zero
		serveSummary(t, summary, nil)
		h := newMetricsTestHandler(t, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		assert.Equal(t, uint64(0), *obj.(*worker.InstanceMetrics).Sample.MemoryWorkingSetMiB)
	})

	t.Run("reports rootfs absent when any container's figure is missing", func(t *testing.T) {
		summary := metricsTestSummary()
		summary.Pods[0].Containers[0].Rootfs = nil
		serveSummary(t, summary, nil)
		h := newMetricsTestHandler(t, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.Nil(t, sample.RootfsUsedMiB, "a partial rootfs total would mislead")
		assert.NotNil(t, sample.CPUUsageNanoCores)
	})
}

func TestFilterAllocatedAcceleratorMetrics(t *testing.T) {
	allocGroups := deviceplugin.AllocatedAcceleratorGroupsOf(metricsTestPod())

	t.Run("keeps only the allocated devices, in vendor-native MiB", func(t *testing.T) {
		got := filterAllocatedAcceleratorMetrics(metricsTestSnapshot(), allocGroups)
		require.Len(t, got, 1)

		accel := got[0]
		assert.Equal(t, "gpu-uuid-1", accel.ID)
		assert.Equal(t, uint64(81920), *accel.MemoryMiB)
		assert.Equal(t, uint64(1024), *accel.MemoryUsageMiB)
		assert.Equal(t, uint32(34), *accel.CoresUtilizationPercent)
		assert.Equal(t, uint32(42), *accel.TemperatureCelsius)
		assert.Equal(t, uint32(120), *accel.PowerUsageWatts)
	})

	t.Run("drops a stale snapshot", func(t *testing.T) {
		snapshot := metricsTestSnapshot()
		snapshot.Timestamp = time.Now().Add(-2 * time.Minute)
		assert.Empty(t, filterAllocatedAcceleratorMetrics(snapshot, allocGroups))
	})

	t.Run("drops everything for a CPU instance", func(t *testing.T) {
		assert.Empty(t, filterAllocatedAcceleratorMetrics(metricsTestSnapshot(), nil))
	})
}

func TestSelectDeviceManagerPod(t *testing.T) {
	amdPod := func() *core.Pod {
		pod := metricsTestDMPod()
		pod.Name = "dm-amd-abc"
		pod.Labels[_DeviceManagerManufacturerLabelKey] = "amd"
		pod.Status.PodIP = "10.0.0.10"
		return pod
	}

	t.Run("picks the pod of the asked manufacturer", func(t *testing.T) {
		pod := selectDeviceManagerPod([]core.Pod{*amdPod(), *metricsTestDMPod()}, "nvidia")
		require.NotNil(t, pod)
		assert.Equal(t, "dm-nvidia-abc", pod.Name)
	})

	t.Run("never substitutes another manufacturer's pod", func(t *testing.T) {
		// That pod's device manager runs with --manufacturer=amd, so its snapshot
		// cannot carry the nvidia cards we were asked about.
		assert.Nil(t, selectDeviceManagerPod([]core.Pod{*amdPod()}, "nvidia"))
	})

	t.Run("skips terminating and unready pods", func(t *testing.T) {
		terminating := metricsTestDMPod()
		now := meta.Now()
		terminating.DeletionTimestamp = &now
		unready := metricsTestDMPod()
		unready.Status.Conditions[0].Status = core.ConditionFalse
		noIP := metricsTestDMPod()
		noIP.Status.PodIP = ""

		assert.Nil(t, selectDeviceManagerPod([]core.Pod{*terminating, *unready, *noIP}, "nvidia"))
	})

	t.Run("accepts any pod for an unlabeled manufacturer", func(t *testing.T) {
		pod := selectDeviceManagerPod([]core.Pod{*amdPod()}, "")
		require.NotNil(t, pod)
		assert.Equal(t, "dm-amd-abc", pod.Name)
	})

	t.Run("returns nil when nothing is ready", func(t *testing.T) {
		assert.Nil(t, selectDeviceManagerPod(nil, "nvidia"))
	})
}

func TestAllocatedManufacturers(t *testing.T) {
	got := allocatedManufacturers([]workercore.DevicesAllocationGroup{
		{Manufacturer: "nvidia"},
		{Manufacturer: "ascend"},
		{Manufacturer: "nvidia"},
	})
	assert.Equal(t, []string{"nvidia", "ascend"}, got,
		"each manufacturer is read once, in allocation order")
}

func TestDeviceManagerSecurePortOf(t *testing.T) {
	t.Run("resolves the named container port", func(t *testing.T) {
		pod := metricsTestDMPod()
		pod.Spec.Containers[0].Ports[0].ContainerPort = 42443
		assert.Equal(t, 42443, deviceManagerSecurePortOf(pod))
	})

	t.Run("falls back to the default for an unnamed port", func(t *testing.T) {
		pod := metricsTestDMPod()
		pod.Spec.Containers[0].Ports[0].Name = "web"
		assert.Equal(t, _DeviceManagerSecurePort, deviceManagerSecurePortOf(pod))
	})
}

func TestFetchMonitorSnapshot(t *testing.T) {
	t.Run("decodes a valid readout", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, devicemanager.MonitorSnapshotPath, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			bs, _ := json.Marshal(metricsTestSnapshot())
			_, _ = w.Write(bs)
		}))
		defer srv.Close()

		snapshot, err := fetchMonitorSnapshot(context.Background(), srv.URL+devicemanager.MonitorSnapshotPath)
		require.NoError(t, err)
		require.Len(t, snapshot.Groups, 1)
		assert.Equal(t, "nvidia", snapshot.Groups[0].Manufacturer)
	})

	t.Run("errors on non-200", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		_, err := fetchMonitorSnapshot(context.Background(), srv.URL+devicemanager.MonitorSnapshotPath)
		require.Error(t, err)
	})
}

func TestParsePodMetricsUsage(t *testing.T) {
	t.Run("sums the containers' usage", func(t *testing.T) {
		raw := []byte(`{"kind":"PodMetrics","timestamp":"2026-08-07T01:02:03Z","containers":[
			{"name":"main","usage":{"cpu":"250m","memory":"512Mi"}},
			{"name":"sshd","usage":{"cpu":"2500000n","memory":"65536Ki"}}]}`)

		cpu, memory, ts, err := parsePodMetricsUsage(raw)
		require.NoError(t, err)
		// 250m + 2500000n = 252500000 nanocores.
		assert.Equal(t, uint64(252_500_000), *cpu)
		// 512Mi + 64Mi, in bytes — the caller scales to MiB.
		assert.Equal(t, uint64(576<<20), *memory)
		assert.NotNil(t, ts)
	})

	t.Run("clamps a negative quantity instead of wrapping it", func(t *testing.T) {
		// Any adapter may serve metrics.k8s.io; an unchecked conversion would turn
		// -250m into 1.8e19 nanocores of "current usage".
		raw := []byte(`{"kind":"PodMetrics","timestamp":"2026-08-07T01:02:03Z","containers":[
			{"name":"main","usage":{"cpu":"-250m","memory":"-1Gi"}}]}`)

		cpu, memory, _, err := parsePodMetricsUsage(raw)
		require.NoError(t, err)
		assert.Equal(t, uint64(0), *cpu)
		assert.Equal(t, uint64(0), *memory)
	})

	t.Run("reports an empty container list as unserved", func(t *testing.T) {
		cpu, memory, ts, err := parsePodMetricsUsage(
			[]byte(`{"kind":"PodMetrics","timestamp":"2026-08-07T01:02:03Z","containers":[]}`))
		require.NoError(t, err)
		assert.Nil(t, cpu, "no containers means no measurement, not a genuine zero")
		assert.Nil(t, memory)
		assert.Nil(t, ts)
	})
}
