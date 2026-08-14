package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	kubeletstats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemetrics"
	"gpustack.ai/gpustack/pkg/nodefeature"
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

// _metricsTestCoresResName is the compute-budget request key a sliced nvidia container carries,
// and the value the slice block reports as its compute quota.
var _metricsTestCoresResName = nodefeature.GetAcceleratableSlicedCoresPercentageResourceName("nvidia")

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

// metricsTestInstance returns a scheduled Instance backed by the test pod, declaring the same
// resources the backing pod's containers carry as limits — so the totals hold whether they are
// read off the pod or, with no pod at all, off the declaration.
func metricsTestInstance() *workercore.Instance {
	return &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{
			Namespace: _metricsTestNS,
			Name:      _metricsTestName,
			UID:       types.UID(_metricsTestUID),
		},
		Spec: workercore.InstanceSpec{
			InstanceTemplate: workercore.InstanceTemplate{
				Resources: &workercore.InstanceResources{
					CPU:          resource.MustParse("2"),
					RAM:          resource.MustParse("4Gi"),
					LocalStorage: resource.MustParse("10Gi"),
				},
			},
		},
		Status: workercore.InstanceStatus{
			NodeName: _metricsTestNode,
		},
	}
}

// metricsTestPod returns the backing pod of the test Instance, allocated one nvidia card.
//
// The containers mirror what the Instance controller writes: "main" carries the Instance's
// spec.resources as its limits, and the sshd sidecar declares no general resources — so the
// sum of the pod's limits, which is where the sample's totals come from, is exactly
// 2 cores / 4Gi / 10Gi.
func metricsTestPod() *core.Pod {
	allocations := deviceplugin.PodAllocations{
		"main": {
			Devices: workercore.DevicesStatus{
				Groups: []workercore.DevicesAllocationGroup{
					{
						ID:           "gpu-group",
						Manufacturer: "nvidia",
						Accelerators: []workercore.AcceleratorAllocation{
							{ID: "gpu-uuid-1", Mode: workercore.DeviceAllocationModeExclusive},
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
			Labels:    map[string]string{deviceplugin.InstancePartOfLabelKey: _metricsTestUID},
			Annotations: map[string]string{
				deviceplugin.AllocatedAcceleratorAnnoKey: string(anno),
			},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{
					Name: "main",
					Resources: core.ResourceRequirements{
						Limits: core.ResourceList{
							core.ResourceCPU:              resource.MustParse("2"),
							core.ResourceMemory:           resource.MustParse("4Gi"),
							core.ResourceEphemeralStorage: resource.MustParse("10Gi"),
						},
					},
				},
				{Name: "sshd"},
			},
		},
		Status: core.PodStatus{
			Phase: core.PodRunning,
			ContainerStatuses: []core.ContainerStatus{
				{
					Name:  "main",
					Ready: true,
					State: core.ContainerState{Running: &core.ContainerStateRunning{}},
				},
				{Name: "sshd", State: core.ContainerState{Running: &core.ContainerStateRunning{}}},
			},
		},
	}
}

// metricsTestSlicedPod returns the backing pod holding a QUARTER of the same card, with the
// compute cap the allocator enforced on it — the shape the slice block is read off.
func metricsTestSlicedPod() *core.Pod {
	pod := metricsTestPod()
	pod.Spec.Containers[0].Resources.Limits[_metricsTestCoresResName] = *resource.NewQuantity(25, resource.DecimalSI)
	setPodAllocations(pod, deviceplugin.PodAllocations{
		"main": {
			Devices: workercore.DevicesStatus{
				Groups: []workercore.DevicesAllocationGroup{
					{
						ID:           "gpu-group",
						Manufacturer: "nvidia",
						Accelerators: []workercore.AcceleratorAllocation{
							{
								ID:        "gpu-uuid-1",
								Mode:      workercore.DeviceAllocationModeSliced,
								Allocated: nodefeature.ResourceMaxUnits / 4,
							},
						},
					},
				},
			},
		},
	})
	return pod
}

// setPodAllocations rewrites the pod's per-container allocation annotation.
func setPodAllocations(pod *core.Pod, allocations deviceplugin.PodAllocations) {
	anno, _ := json.Marshal(allocations)
	pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = string(anno)
}

// metricsTestUnstartedPod returns the backing pod before anything of it has run: it is scheduled
// and it already holds the card, and no container has started.
func metricsTestUnstartedPod() *core.Pod {
	pod := metricsTestSlicedPod()
	pod.Status = core.PodStatus{
		Phase: core.PodPending,
		ContainerStatuses: []core.ContainerStatus{
			{
				Name:  "main",
				State: core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "PullingImage"}},
			},
			{Name: "sshd", State: core.ContainerState{Waiting: &core.ContainerStateWaiting{}}},
		},
	}
	return pod
}

// assertTestPodTotals pins the sample's denominators to the backing pod's declared limits.
// They come from the Instance's own declaration, so they hold whichever measurement source
// answered — including the degraded metrics.k8s.io fallback.
func assertTestPodTotals(t *testing.T, sample worker.InstanceMetricsSample) {
	t.Helper()
	assert.Equal(t, uint64(2000), sample.CPUTotalMilliCores)
	assert.Equal(t, uint64(4096), sample.MemoryTotalMiB)
	assert.Equal(t, uint64(10240), sample.StorageTotalMiB)
}

// metricsTestDMPod returns a Ready device manager pod on the test node.
func metricsTestDMPod() *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: _metricsTestDMNamespace,
			Name:      "dm-nvidia-abc",
			Labels: map[string]string{
				deviceplugin.ComponentLabelKey:    deviceplugin.DeviceManagerComponent,
				deviceplugin.ManufacturerLabelKey: "nvidia",
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

// dmPodAt returns a Ready device manager pod advertising the given loopback server as the
// endpoint its snapshot is read from.
func dmPodAt(t *testing.T, srv *httptest.Server) *core.Pod {
	t.Helper()
	port, err := strconv.Atoi(strings.TrimPrefix(srv.URL, "https://127.0.0.1:"))
	require.NoError(t, err)

	pod := metricsTestDMPod()
	pod.Status.PodIP = "127.0.0.1"
	pod.Spec.Containers[0].Ports[0].ContainerPort = int32(port)
	return pod
}

// denyUpstream stands up all three upstream sources this handler can read — the kubelet stats
// summary and metrics.k8s.io, both through the API server, and one device manager snapshot — as
// fakes that FAIL THE TEST when they are called, and returns the device manager pod pointing at
// the third. It is what turns "the closed gate performs no upstream read" from a claim into an
// assertion: the handler swallows a device manager failure, so only the served side can catch a
// call.
//
// The handlers under test are built with a zero MaxAge, so the process-wide kubelet readout cache
// cannot answer a read in place of these fakes and hide it.
func denyUpstream(t *testing.T) []core.Pod {
	t.Helper()

	useFakeAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no upstream read is allowed, the API server was asked for %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no upstream read is allowed, the device manager was asked for %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	return []core.Pod{*dmPodAt(t, srv)}
}

// assertUnstartedSample pins the answer the closed gate serves: the declared totals, and the
// three used figures present and zero — a measurement, since nothing has started that could
// consume anything — with no accelerator section at all.
func assertUnstartedSample(t *testing.T, sample worker.InstanceMetricsSample) {
	t.Helper()

	assertTestPodTotals(t, sample)
	for name, v := range map[string]*uint64{
		"cpu":     sample.CPUUsedMilliCores,
		"memory":  sample.MemoryUsedMiB,
		"storage": sample.StorageUsedMiB,
	} {
		require.NotNilf(t, v, "%s usage is measured as zero, not absent", name)
		assert.Zerof(t, *v, "%s usage", name)
	}
	assert.Empty(t, sample.Accelerators)
	assert.False(t, sample.Timestamp.IsZero())
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
		Timestamp:     time.Now(),
		PeriodSeconds: 15,
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

// metricsTestSliceSection returns a slice section that measured the allocated card, carrying the
// given record for the sliced test pod's own container.
func metricsTestSliceSection(usages ...detector.SliceUsage) *detector.MonitorSliceSection {
	return &detector.MonitorSliceSection{
		SchemaVersion: detector.MonitorSliceSchemaVersion,
		Usages:        usages,
		Devices: []detector.SliceDeviceDiagnostics{
			{Manufacturer: "nvidia", DeviceID: "gpu-uuid-1", RowsReturned: uint32(len(usages)), RowsAttributed: uint32(len(usages))},
		},
	}
}

// metricsTestSliceUsage returns the section's record for the sliced test pod's own container.
func metricsTestSliceUsage(memoryMiB *uint64, coresPercent *uint32) detector.SliceUsage {
	return detector.SliceUsage{
		Manufacturer:            "nvidia",
		PodUID:                  string(_metricsTestPodUID),
		Container:               "main",
		DeviceID:                "gpu-uuid-1",
		MemoryUsedMiB:           memoryMiB,
		CoresUtilizationPercent: coresPercent,
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
	t.Run("answers with zeros, and reads nothing, when no container has started", func(t *testing.T) {
		dmPods := denyUpstream(t)
		h := newMetricsTestHandlerWith(t, metricsTestInstance(), metricsTestUnstartedPod(), dmPods)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)
		// The pod already holds a card, so a measuring path would have had something to read
		// and a neighbour's figures to report as this Instance's.
		assertUnstartedSample(t, obj.(*worker.InstanceMetrics).Sample)
	})

	t.Run("measures a running instance whose readiness probe fails", func(t *testing.T) {
		serveSummary(t, metricsTestSummary(), nil)
		pod := metricsTestPod()
		pod.Status.ContainerStatuses[0].Ready = false
		h := newMetricsTestHandlerWith(t, metricsTestInstance(), pod, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		// The case the gate's predicate exists for: this pod can be holding accelerator memory,
		// so it is measured rather than reported idle.
		sample := obj.(*worker.InstanceMetrics).Sample
		require.NotNil(t, sample.CPUUsedMilliCores)
		assert.Equal(t, uint64(500), *sample.CPUUsedMilliCores)
	})

	t.Run("answers from the declaration when the instance has no pod", func(t *testing.T) {
		dmPods := denyUpstream(t)
		builder := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(metricsTestInstance()).
			WithIndex(&core.Pod{}, "spec.nodeName", func(obj ctrlcli.Object) []string {
				return []string{obj.(*core.Pod).Spec.NodeName}
			})
		for i := range dmPods {
			builder = builder.WithObjects(&dmPods[i])
		}
		h := &InstanceMetricsHandler{APIReader: builder.Build()}

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)
		// The totals come from spec.resources, and match what the rendered pod would have said.
		assertUnstartedSample(t, obj.(*worker.InstanceMetrics).Sample)
	})

	t.Run("answers from the declaration when unscheduled", func(t *testing.T) {
		denyUpstream(t)
		inst := metricsTestInstance()
		inst.Status.NodeName = ""
		h := newMetricsTestHandlerWith(t, inst, metricsTestPod(), nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)
		assertUnstartedSample(t, obj.(*worker.InstanceMetrics).Sample)
	})

	t.Run("answers from the declaration when the pod belongs to a previous incarnation", func(t *testing.T) {
		dmPods := denyUpstream(t)
		pod := metricsTestPod()
		pod.Labels[deviceplugin.InstancePartOfLabelKey] = "stale-uid"
		h := newMetricsTestHandlerWith(t, metricsTestInstance(), pod, dmPods)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)
		// The previous incarnation's figures are never served, and its pod is not this
		// Instance's to measure — so this Instance has no pod, and answers as one.
		assertUnstartedSample(t, obj.(*worker.InstanceMetrics).Sample)
	})

	t.Run("totals zero when the instance declares no resources", func(t *testing.T) {
		denyUpstream(t)
		inst := metricsTestInstance()
		inst.Spec.Resources = nil
		inst.Status.NodeName = ""
		h := newMetricsTestHandlerWith(t, inst, metricsTestPod(), nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.Zero(t, sample.CPUTotalMilliCores)
		assert.Zero(t, sample.MemoryTotalMiB)
		assert.Zero(t, sample.StorageTotalMiB)
		require.NotNil(t, sample.CPUUsedMilliCores)
		assert.Zero(t, *sample.CPUUsedMilliCores)
	})

	t.Run("propagates a missing instance as not found", func(t *testing.T) {
		denyUpstream(t)
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		h := &InstanceMetricsHandler{APIReader: cli}

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsNotFound(err), "a gate that is not open must not turn 404 into 200")
	})

	t.Run("returns the current instance-scoped sample", func(t *testing.T) {
		serveSummary(t, metricsTestSummary(), nil)

		// Stand up a loopback device manager: a TLS server whose address the
		// fake dm pod advertises as its pod IP + named https port.
		dmSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, devicemanager.MonitorSnapshotPath, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			bs, _ := json.Marshal(metricsTestSnapshot())
			_, _ = w.Write(bs)
		}))
		t.Cleanup(dmSrv.Close)
		h := newMetricsTestHandler(t, []core.Pod{*dmPodAt(t, dmSrv)})

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		metrics, ok := obj.(*worker.InstanceMetrics)
		require.True(t, ok)
		sample := metrics.Sample

		// Every figure is one half of a pair the consumer can divide.
		assertTestPodTotals(t, sample)

		// The UID-matched entry only: neither the other tenant's nor the stale incarnation's.
		require.NotNil(t, sample.CPUUsedMilliCores)
		assert.Equal(t, uint64(500), *sample.CPUUsedMilliCores)
		// The kubelet measures in nanocores and bytes, the sample reports the totals' units.
		assert.Equal(t, uint64(1024), *sample.MemoryUsedMiB)
		// The pod-level ephemeral aggregate, not the containers' 2Gi of writable layers.
		assert.Equal(t, uint64(3072), *sample.StorageUsedMiB)

		// The merged accelerator section: only the allocated card, vendor-native MiB.
		require.Len(t, sample.Accelerators, 1)
		assert.Equal(t, "gpu-uuid-1", sample.Accelerators[0].ID)
		assert.Equal(t, workercore.DeviceAllocationModeExclusive.String(), sample.Accelerators[0].Mode)
		assert.Equal(t, uint64(81920), *sample.Accelerators[0].MemoryTotalMiB,
			"the device is the grant, so the device's own capacity is the total")
	})

	t.Run("carries a sliced instance's own share of the card", func(t *testing.T) {
		serveSummary(t, metricsTestSummary(), nil)

		snapshot := metricsTestSnapshot()
		snapshot.Slices = metricsTestSliceSection(
			metricsTestSliceUsage(ptr.To[uint64](6144), ptr.To[uint32](18)))
		dmSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			bs, _ := json.Marshal(snapshot)
			_, _ = w.Write(bs)
		}))
		t.Cleanup(dmSrv.Close)

		h := newMetricsTestHandlerWith(t, metricsTestInstance(), metricsTestSlicedPod(),
			[]core.Pod{*dmPodAt(t, dmSrv)})

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		accelerators := obj.(*worker.InstanceMetrics).Sample.Accelerators
		require.Len(t, accelerators, 1)

		// Every figure is the Instance's own: the quarter it was granted, what it holds of that
		// quarter, and the 18% of the card it was measured using restated against its 25% cap.
		// The card's own 1024 MiB — every tenant on it — appears nowhere.
		got := accelerators[0]
		assert.Equal(t, "gpu-uuid-1", got.ID)
		assert.Equal(t, workercore.DeviceAllocationModeSliced.String(), got.Mode)
		assert.Equal(t, ptr.To[uint64](20480), got.MemoryTotalMiB, "a quarter of the card")
		assert.Equal(t, ptr.To[uint64](6144), got.MemoryUsedMiB)
		assert.Equal(t, ptr.To[uint32](30), got.MemoryUtilizationPercent)
		assert.Equal(t, ptr.To[uint32](72), got.CoresUtilizationPercent)
		// The whole card's own readings, which a share has none of.
		assert.Equal(t, ptr.To[uint32](42), got.TemperatureCelsius)
	})

	// The partition path end to end, including the wire: the record travels in the snapshot's own
	// list, so this also pins that the new list survives the encoding the device manager publishes
	// through. The Pod still carries a compute request, and the cap must not come from it.
	t.Run("carries a partitioned instance's own share of the card", func(t *testing.T) {
		serveSummary(t, metricsTestSummary(), nil)

		snapshot := metricsTestSnapshot()
		snapshot.Slices = &detector.MonitorSliceSection{
			SchemaVersion: detector.MonitorSliceSchemaVersion,
			Partitions: []detector.SlicePartition{{
				Manufacturer: "nvidia", PodUID: string(_metricsTestPodUID), Container: "main",
				DeviceID: "gpu-uuid-1",
				// The partition names and sizes itself, off its own handle.
				ID:             "MIG-aaa",
				MemoryTotalMiB: ptr.To[uint64](9856),
				MemoryUsedMiB:  ptr.To[uint64](0),
				CoresReason:    device.AcceleratorProcessReasonUnsupported,
			}},
		}
		dmSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			bs, _ := json.Marshal(snapshot)
			_, _ = w.Write(bs)
		}))
		t.Cleanup(dmSrv.Close)

		pod := slicedAllocationPod(t, workercore.AcceleratorAllocation{
			ID: "gpu-uuid-1", Mode: workercore.DeviceAllocationModePartitioned,
			Allocated:                   nodefeature.ResourceMaxUnits / 8,
			AllocatedPhysicalProfile:    "1g.10gb",
			AllocatedPhysicalPlacements: []workercore.AcceleratorPlacement{{Start: 0, Length: 1}},
		})
		h := newMetricsTestHandlerWith(t, metricsTestInstance(), pod,
			[]core.Pod{*dmPodAt(t, dmSrv)})

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		accelerators := obj.(*worker.InstanceMetrics).Sample.Accelerators
		require.Len(t, accelerators, 1)

		got := accelerators[0]
		assert.Equal(t, "MIG-aaa", got.ID, "the partition is what the Instance was granted")
		assert.Equal(t, workercore.DeviceAllocationModePartitioned.String(), got.Mode)
		assert.Equal(t, ptr.To[uint64](9856), got.MemoryTotalMiB,
			"the partition's own capacity, not an eighth of the card folded out of the units")
		// Measured and idle, which is the figure a process-first lookup could not have taken.
		assert.Equal(t, ptr.To[uint64](0), got.MemoryUsedMiB)
		assert.Equal(t, ptr.To[uint32](0), got.MemoryUtilizationPercent)
		assert.Nil(t, got.CoresUtilizationPercent, "no manufacturer serves one per partition")
	})

	// The degraded source's own branches are pinned in pkg/kubemetrics; what the two cases
	// below add is the round trip through the served object and the API error mapping.
	t.Run("serves a degraded sample when the kubelet read fails", func(t *testing.T) {
		serveSummaryAndFallback(t, fmt.Errorf("kubelet down"),
			`{"kind":"PodMetrics","timestamp":"`+time.Now().UTC().Format(time.RFC3339)+`",`+
				`"containers":[{"name":"main","usage":{"cpu":"250m","memory":"512Mi"}}]}`,
			http.StatusOK)
		h := newMetricsTestHandler(t, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		require.NotNil(t, sample.CPUUsedMilliCores)
		assert.Equal(t, uint64(250), *sample.CPUUsedMilliCores)
		assert.Equal(t, uint64(512), *sample.MemoryUsedMiB)
		assert.Nil(t, sample.StorageUsedMiB, "the fallback carries no disk figures")
		// The totals come from the Instance's declaration, so they survive the fallback —
		// a consumer still has a denominator for the two figures it did get.
		assertTestPodTotals(t, sample)
	})

	t.Run("reports an unreadable usage as unavailable, naming why", func(t *testing.T) {
		serveSummaryAndFallback(t, fmt.Errorf("kubelet down"), "", http.StatusNotFound)
		h := newMetricsTestHandler(t, nil)

		_, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.Error(t, err)
		assert.True(t, kerrors.IsServiceUnavailable(err))
		assert.Contains(t, err.Error(), "metrics.k8s.io API is not served",
			"the client-facing message must name the actual reason, not a nil error")
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
		require.NotNil(t, sample.CPUUsedMilliCores)
		assert.Equal(t, uint64(500), *sample.CPUUsedMilliCores)
		assert.Equal(t, uint64(1024), *sample.MemoryUsedMiB)
	})

	t.Run("keeps the pod stats when no device manager pod is ready", func(t *testing.T) {
		serveSummary(t, metricsTestSummary(), nil)
		h := newMetricsTestHandler(t, nil)

		obj, err := h.OnGet(context.Background(), metricsTestKey(), ctrlcli.GetOptions{})
		require.NoError(t, err)

		sample := obj.(*worker.InstanceMetrics).Sample
		assert.Empty(t, sample.Accelerators)
		assert.NotNil(t, sample.CPUUsedMilliCores)
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
		assert.NotNil(t, sample.CPUUsedMilliCores)
	})
}

// TestFilterAllocatedAcceleratorMetrics covers what this package still owns: mapping the
// device manager's own metrics onto the API type. The filtering behind it — the staleness bound
// and the manufacturer-and-ID match — is pinned in pkg/devicemanager/detector, beside the
// snapshot it reads.
func TestFilterAllocatedAcceleratorMetrics(t *testing.T) {
	pod := metricsTestPod()
	allocGroups := deviceplugin.AllocatedAcceleratorGroupsOf(pod)
	allocations, err := deviceplugin.AllocatedAcceleratorsOf(pod)
	require.NoError(t, err)
	grants := kubemetrics.NewAcceleratorGrants(pod, allocations)

	t.Run("maps the allocated device onto the API type, in vendor-native MiB", func(t *testing.T) {
		got := filterAllocatedAcceleratorMetrics(metricsTestSnapshot(), allocGroups, grants)
		require.Len(t, got, 1)

		accel := got[0]
		assert.Equal(t, "gpu-uuid-1", accel.ID)
		assert.Equal(t, uint64(81920), *accel.MemoryTotalMiB)
		assert.Equal(t, uint64(1024), *accel.MemoryUsedMiB)
		assert.Equal(t, uint32(12), *accel.MemoryUtilizationPercent)
		assert.Equal(t, uint32(34), *accel.CoresUtilizationPercent)
		assert.Equal(t, uint32(42), *accel.TemperatureCelsius)
		assert.Equal(t, uint32(120), *accel.PowerUsageWatts)
		require.NotNil(t, accel.Unhealthy)
		assert.False(t, *accel.Unhealthy)
	})

	t.Run("maps nothing when the filter yields nothing", func(t *testing.T) {
		assert.Empty(t, filterAllocatedAcceleratorMetrics(metricsTestSnapshot(), nil, grants))
	})
}

// slicedAllocationPod returns the sliced test pod with its recorded allocation replaced, so a case
// states only what it varies.
func slicedAllocationPod(t *testing.T, accelerators ...workercore.AcceleratorAllocation) *core.Pod {
	t.Helper()
	pod := metricsTestSlicedPod()
	setPodAllocations(pod, deviceplugin.PodAllocations{
		"main": {
			Devices: workercore.DevicesStatus{
				Groups: []workercore.DevicesAllocationGroup{
					{ID: "gpu-group", Manufacturer: "nvidia", Accelerators: accelerators},
				},
			},
		},
	})
	return pod
}

// TestSliceSectionDistinguishesAbsences pins the one decision the served response cannot carry: two
// absences reach a client identically — the JSON key is simply omitted — but they are different
// claims, and the section is where they stay apart. "This device was measured and the figure was not
// obtainable" answers, while "this section cannot speak for this device" does not answer at all.
// Whoever adds a reason to the API reads them from here.
//
// The resolution those absences flow through is pinned in pkg/kubemetrics, beside the index that
// makes it; what this package owns is the round trip below.
func TestSliceSectionDistinguishesAbsences(t *testing.T) {
	unmeasurable := metricsTestSliceSection(metricsTestSliceUsage(nil, nil))
	figures, ok := unmeasurable.Figures("nvidia", string(_metricsTestPodUID), "main", "gpu-uuid-1")
	assert.True(t, ok, "the device was measured")
	assert.Nil(t, figures.MemoryUsedMiB)

	unanswerable := &detector.MonitorSliceSection{SchemaVersion: detector.MonitorSliceSchemaVersion}
	_, ok = unanswerable.Figures("nvidia", string(_metricsTestPodUID), "main", "gpu-uuid-1")
	assert.False(t, ok, "the section carries no such device")
}

func TestSelectDeviceManagerPod(t *testing.T) {
	amdPod := func() *core.Pod {
		pod := metricsTestDMPod()
		pod.Name = "dm-amd-abc"
		pod.Labels[deviceplugin.ManufacturerLabelKey] = "amd"
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
