package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
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
		Spec: core.PodSpec{NodeName: _metricsTestNode},
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
func metricsTestSummary() *statsv1alpha1.Summary {
	cpu := uint64(500_000_000)
	mem := uint64(1 << 30)
	eph := uint64(3 << 30)
	rootfs := uint64(2 << 30)
	foreignCPU := uint64(999)
	staleCPU := uint64(777)
	return &statsv1alpha1.Summary{
		Pods: []statsv1alpha1.PodStats{
			{
				PodRef: statsv1alpha1.PodReference{
					Namespace: _metricsTestNS, Name: _metricsTestName, UID: string(_metricsTestPodUID),
				},
				CPU:              &statsv1alpha1.CPUStats{Time: meta.Now(), UsageNanoCores: &cpu},
				Memory:           &statsv1alpha1.MemoryStats{WorkingSetBytes: &mem},
				EphemeralStorage: &statsv1alpha1.FsStats{UsedBytes: &eph},
				Containers: []statsv1alpha1.ContainerStats{
					{Name: "main", Rootfs: &statsv1alpha1.FsStats{UsedBytes: &rootfs}},
				},
			},
			{
				PodRef: statsv1alpha1.PodReference{Namespace: "other", Name: "other", UID: "uid-2"},
				CPU:    &statsv1alpha1.CPUStats{Time: meta.Now(), UsageNanoCores: &foreignCPU},
			},
			{
				// Previous incarnation: same name, different UID.
				PodRef: statsv1alpha1.PodReference{
					Namespace: _metricsTestNS, Name: _metricsTestName, UID: "stale-pod-uid",
				},
				CPU: &statsv1alpha1.CPUStats{Time: meta.Now(), UsageNanoCores: &staleCPU},
			},
		},
	}
}

// metricsTestSnapshot returns a device manager snapshot with the allocated card
// and a foreign card.
func metricsTestSnapshot() *devicemanager.MonitorSnapshot {
	return &devicemanager.MonitorSnapshot{
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

// metricsTestSeams carries the injectable handler seams for a test.
type metricsTestSeams struct {
	summary    *statsv1alpha1.Summary
	summaryErr error
	fallback   func(ctx context.Context, key types.NamespacedName) (*uint64, *uint64, *meta.Time, error)
	snapshot   *devicemanager.MonitorSnapshot
	snapErr    error
}

// newMetricsTestHandler builds a handler with fake clients and the given seams.
func newMetricsTestHandler(t *testing.T, dmPods []core.Pod, seams metricsTestSeams) *InstanceMetricsHandler {
	t.Helper()
	return newMetricsTestHandlerWith(t, metricsTestInstance(), metricsTestPod(), dmPods, seams)
}

func newMetricsTestHandlerWith(
	t *testing.T,
	inst *workercore.Instance,
	pod *core.Pod,
	dmPods []core.Pod,
	seams metricsTestSeams,
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

	h := &InstanceMetricsHandler{Client: cli, APIReader: cli}
	h.listDeviceManagerPods = h.listNodeDeviceManagerPods
	h.fetchKubeletSummary = func(context.Context, string) (*statsv1alpha1.Summary, error) {
		return seams.summary, seams.summaryErr
	}
	h.fetchPodUsageFallback = seams.fallback
	h.fetchMonitorSnapshot = func(_ context.Context, url string) (*devicemanager.MonitorSnapshot, error) {
		assert.Equal(t, "https://10.0.0.9:32443"+devicemanager.MonitorSnapshotPath, url)
		return seams.snapshot, seams.snapErr
	}
	return h
}

func metricsTestKey() types.NamespacedName {
	return types.NamespacedName{Namespace: _metricsTestNS, Name: _metricsTestName}
}

func TestInstanceMetricsHandler_OnGet(t *testing.T) {
	t.Run("unavailable when unscheduled", func(t *testing.T) {
		inst := metricsTestInstance()
		inst.Status.NodeName = ""
		h := newMetricsTestHandlerWith(t, inst, metricsTestPod(), nil, metricsTestSeams{})

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
	})

	t.Run("unavailable without a backing pod", func(t *testing.T) {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(metricsTestInstance()).Build()
		h := &InstanceMetricsHandler{Client: cli, APIReader: cli}

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
	})

	t.Run("rejects a pod owned by a previous incarnation", func(t *testing.T) {
		pod := metricsTestPod()
		pod.Labels[_InstancePartOfLabelKey] = "stale-uid"
		h := newMetricsTestHandlerWith(t, metricsTestInstance(), pod, nil, metricsTestSeams{})

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsConflict(err))
	})

	t.Run("returns the current instance-scoped sample", func(t *testing.T) {
		h := newMetricsTestHandler(t, []core.Pod{*metricsTestDMPod()}, metricsTestSeams{
			summary:  metricsTestSummary(),
			snapshot: metricsTestSnapshot(),
		})

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		metrics, ok := obj.(*worker.InstanceMetrics)
		require.True(t, ok)
		sample := metrics.Sample

		// The UID-matched entry only: neither the other tenant's nor the stale incarnation's.
		require.NotNil(t, sample.CPUUsageNanoCores)
		assert.Equal(t, uint64(500_000_000), *sample.CPUUsageNanoCores)
		assert.Equal(t, uint64(1<<30), *sample.MemoryWorkingSetBytes)
		assert.Equal(t, uint64(2<<30), *sample.RootfsUsedBytes)
		assert.Equal(t, uint64(3<<30), *sample.EphemeralStorageUsedBytes)

		// Only the allocated card, converted from MiB to bytes.
		require.Len(t, sample.Accelerators, 1)
		accel := sample.Accelerators[0]
		assert.Equal(t, "gpu-uuid-1", accel.ID)
		assert.Equal(t, uint64(81920<<20), *accel.MemoryBytes)
		assert.Equal(t, uint64(1024<<20), *accel.MemoryUsageBytes)
		assert.Equal(t, uint32(34), *accel.CoresUtilizationPercent)
		assert.Equal(t, uint32(42), *accel.TemperatureCelsius)
		assert.Equal(t, uint32(120), *accel.PowerUsageWatts)
	})

	t.Run("degrades gracefully when the pod is not in the summary yet", func(t *testing.T) {
		h := newMetricsTestHandler(t, []core.Pod{*metricsTestDMPod()}, metricsTestSeams{
			summary:  &statsv1alpha1.Summary{},
			snapshot: metricsTestSnapshot(),
		})

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.Nil(t, sample.CPUUsageNanoCores)
		require.Len(t, sample.Accelerators, 1, "GPU section is independent of kubelet stats")
	})

	t.Run("falls back to the metrics API when the kubelet read fails", func(t *testing.T) {
		cpu, mem := uint64(250_000_000), uint64(1<<29)
		h := newMetricsTestHandler(t, []core.Pod{*metricsTestDMPod()}, metricsTestSeams{
			summaryErr: fmt.Errorf("kubelet down"),
			fallback: func(context.Context, types.NamespacedName) (*uint64, *uint64, *meta.Time, error) {
				ts := meta.Now()
				return &cpu, &mem, &ts, nil
			},
			snapshot: metricsTestSnapshot(),
		})

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		require.NotNil(t, sample.CPUUsageNanoCores)
		assert.Equal(t, cpu, *sample.CPUUsageNanoCores)
		assert.Equal(t, mem, *sample.MemoryWorkingSetBytes)
		assert.Nil(t, sample.RootfsUsedBytes, "the fallback carries no disk figures")
		assert.Nil(t, sample.EphemeralStorageUsedBytes)
	})

	t.Run("unavailable when both sources fail", func(t *testing.T) {
		h := newMetricsTestHandler(t, nil, metricsTestSeams{
			summaryErr: fmt.Errorf("kubelet down"),
			fallback: func(context.Context, types.NamespacedName) (*uint64, *uint64, *meta.Time, error) {
				return nil, nil, nil, nil // metrics.k8s.io not served
			},
		})

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
	})

	t.Run("rejects a fallback entry measured before this pod existed", func(t *testing.T) {
		// A recreated pod keeps the name; the metrics API may still serve the
		// previous incarnation's entry — it must not be presented as current.
		pod := metricsTestPod()
		pod.CreationTimestamp = meta.Now()
		stale := meta.NewTime(time.Now().Add(-time.Hour))
		cpu, mem := uint64(1), uint64(1)
		h := newMetricsTestHandlerWith(t, metricsTestInstance(), pod, nil, metricsTestSeams{
			summaryErr: fmt.Errorf("kubelet down"),
			fallback: func(context.Context, types.NamespacedName) (*uint64, *uint64, *meta.Time, error) {
				return &cpu, &mem, &stale, nil
			},
		})

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
	})

	t.Run("reports rootfs absent when any container's figure is missing", func(t *testing.T) {
		summary := metricsTestSummary()
		summary.Pods[0].Containers[0].Rootfs = nil

		h := newMetricsTestHandler(t, []core.Pod{*metricsTestDMPod()}, metricsTestSeams{
			summary:  summary,
			snapshot: metricsTestSnapshot(),
		})
		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.Nil(t, sample.RootfsUsedBytes, "a partial rootfs total would mislead")
		assert.NotNil(t, sample.CPUUsageNanoCores)
	})

	t.Run("drops the accelerator section when the device manager fails", func(t *testing.T) {
		h := newMetricsTestHandler(t, []core.Pod{*metricsTestDMPod()}, metricsTestSeams{
			summary: metricsTestSummary(),
			snapErr: fmt.Errorf("dm unreachable"),
		})

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.NotNil(t, sample.CPUUsageNanoCores)
		assert.Empty(t, sample.Accelerators)
	})
}

func TestInstanceMetricsHandler_SelectDeviceManagerPod(t *testing.T) {
	allocGroups := deviceplugin.AllocatedAcceleratorGroupsOf(metricsTestPod())

	t.Run("prefers the allocated manufacturer", func(t *testing.T) {
		amd := metricsTestDMPod()
		amd.Name = "dm-amd-abc"
		amd.Labels[_DeviceManagerManufacturerLabelKey] = "amd"
		amd.Status.PodIP = "10.0.0.10"

		h := newMetricsTestHandler(t, []core.Pod{*amd, *metricsTestDMPod()}, metricsTestSeams{})
		pod, err := h.selectDeviceManagerPod(context.Background(), _metricsTestNode, allocGroups)
		require.NoError(t, err)
		assert.Equal(t, "dm-nvidia-abc", pod.Name)
	})

	t.Run("skips terminating pods and falls back", func(t *testing.T) {
		nvidia := metricsTestDMPod()
		now := meta.Now()
		nvidia.DeletionTimestamp = &now
		// The fake client requires a finalizer on objects with a deletion timestamp.
		nvidia.Finalizers = []string{"test.finalizer/delete-in-progress"}
		amd := metricsTestDMPod()
		amd.Name = "dm-amd-abc"
		amd.Labels[_DeviceManagerManufacturerLabelKey] = "amd"

		h := newMetricsTestHandler(t, []core.Pod{*nvidia, *amd}, metricsTestSeams{})
		pod, err := h.selectDeviceManagerPod(context.Background(), _metricsTestNode, allocGroups)
		require.NoError(t, err)
		assert.Equal(t, "dm-amd-abc", pod.Name)
	})

	t.Run("errors when nothing is ready", func(t *testing.T) {
		h := newMetricsTestHandler(t, nil, metricsTestSeams{})
		_, err := h.selectDeviceManagerPod(context.Background(), _metricsTestNode, nil)
		require.Error(t, err)
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
	raw := []byte(`{"kind":"PodMetrics","timestamp":"2026-08-07T01:02:03Z","containers":[
		{"name":"main","usage":{"cpu":"250m","memory":"512Mi"}},
		{"name":"sshd","usage":{"cpu":"2500000n","memory":"65536Ki"}}]}`)

	cpu, memory, ts, err := parsePodMetricsUsage(raw)
	require.NoError(t, err)
	// 250m + 2500000n = 252500000 nanocores.
	assert.Equal(t, uint64(252_500_000), *cpu)
	// 512Mi + 64Mi.
	assert.Equal(t, uint64(576<<20), *memory)
	assert.NotNil(t, ts)
}
