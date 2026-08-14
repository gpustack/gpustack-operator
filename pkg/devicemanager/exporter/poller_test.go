package exporter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	kubeletstats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// apiHandler dispatches the fake API server the kubelet readout goes through.
var apiHandler atomic.Value

func TestMain(m *testing.M) {
	apiHandler.Store(http.NotFoundHandler())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHandler.Load().(http.Handler).ServeHTTP(w, r)
	}))

	cfg := &rest.Config{Host: srv.URL}
	system.LoopbackKubeRestConfig.Configure(*cfg)
	system.LoopbackKubeClient.Configure(kubernetes.NewForConfigOrDie(cfg))
	// pkg/manager does this from the controller manager before Config.Apply runs; here it is
	// what lets Apply hand the poller a reader.
	ctrlCli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	system.ConfigureLoopbackCtrlRuntime(ctrlCli, ctrlCli)

	// The DaemonSet sets these from the downward API; the exporter role is resolved by finding
	// this pod among its node's device manager pods.
	_ = os.Setenv("KUBERNETES_POD_NAME", testSelfPodName)
	_ = os.Setenv("KUBERNETES_POD_NAMESPACE", testSystemNamespace)

	code := m.Run()
	srv.Close()
	os.Exit(code)
}

// serveNodeSummary answers one node's kubelet readout, or fails it when summary is nil.
// Each case uses a node of its own, so the readout cache never carries one case's answer into
// the next.
func serveNodeSummary(t *testing.T, nodeName string, summary *kubeletstats.Summary) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes/"+nodeName+"/proxy/stats/summary",
		func(w http.ResponseWriter, _ *http.Request) {
			if summary == nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			bs, _ := json.Marshal(summary)
			_, _ = w.Write(bs)
		})
	apiHandler.Store(http.HandlerFunc(mux.ServeHTTP))
	t.Cleanup(func() { apiHandler.Store(http.NotFoundHandler()) })
}

const (
	testNamespace       = "tenant"
	testInstanceUID     = "instance-uid-1"
	testSystemNamespace = "gpustack-system"
	testSelfPodName     = "dm-self"
)

// deviceManagerPodFixture builds a device manager pod as the chart rolls it: one DaemonSet per
// manufacturer, each stamping the component and manufacturer labels the exporter role reads.
func deviceManagerPodFixture(nodeName, name, manufacturer string, opts ...podOption) *core.Pod {
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: testSystemNamespace,
			Name:      name,
			UID:       types.UID("dm-" + name),
			Labels: map[string]string{
				deviceplugin.ComponentLabelKey:    deviceplugin.DeviceManagerComponent,
				deviceplugin.ManufacturerLabelKey: manufacturer,
			},
		},
		Spec: core.PodSpec{NodeName: nodeName},
		Status: core.PodStatus{
			Conditions: []core.PodCondition{
				{Type: core.PodReady, Status: core.ConditionTrue},
			},
		},
	}
	for _, opt := range opts {
		opt(pod)
	}
	return pod
}

func notReady() podOption {
	return func(pod *core.Pod) { pod.Status.Conditions[0].Status = core.ConditionFalse }
}

// podOption tweaks a fixture pod away from the shape the Instance controller produces.
type podOption func(*core.Pod)

// instancePodFixture builds a pod as the Instance controller writes it: a controller
// ownerReference to the Instance, the part-of label repeating its UID, and the Instance's
// declared resources as the workload container's limits.
func instancePodFixture(nodeName, name, instUID, podUID string, opts ...podOption) *core.Pod {
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace:         testNamespace,
			Name:              name,
			UID:               types.UID(podUID),
			CreationTimestamp: meta.NewTime(time.Now()),
			Labels:            map[string]string{deviceplugin.InstancePartOfLabelKey: instUID},
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: workercore.GroupVersion.String(),
				Kind:       _InstanceKind,
				Name:       name,
				UID:        types.UID(instUID),
				Controller: ptr.To(true),
			}},
		},
		Spec: core.PodSpec{
			NodeName: nodeName,
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
		// Running, which is the state of a pod whose Instance is there to be measured. The gate
		// above every measurement is whether any container has started, so a fixture that has
		// not is a case of its own — see unstarted().
		Status: core.PodStatus{
			Phase: core.PodRunning,
			ContainerStatuses: []core.ContainerStatus{{
				Name:    "main",
				Started: ptr.To(true),
				State: core.ContainerState{
					Running: &core.ContainerStateRunning{StartedAt: meta.Now()},
				},
			}},
		},
	}
	for _, opt := range opts {
		opt(pod)
	}
	return pod
}

// unstarted returns the pod before anything of it has run: it is scheduled and it already holds
// its card, and no container has started.
func unstarted() podOption {
	return func(pod *core.Pod) {
		pod.Status = core.PodStatus{
			Phase: core.PodPending,
			ContainerStatuses: []core.ContainerStatus{{
				Name:    "main",
				Started: ptr.To(false),
				State: core.ContainerState{
					Waiting: &core.ContainerStateWaiting{Reason: "ContainerCreating"},
				},
			}},
		}
	}
}

// refuseUpstream fails the test if anything reaches the API server, which is where both the
// kubelet readout and the metrics API are proxied from.
func refuseUpstream(t *testing.T) {
	t.Helper()
	apiHandler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no upstream read was allowed, got %s", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(func() { apiHandler.Store(http.NotFoundHandler()) })
}

func terminating() podOption {
	return func(pod *core.Pod) {
		now := meta.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"gpustack.ai/test"}
	}
}

func withoutOwner() podOption {
	return func(pod *core.Pod) { pod.OwnerReferences = nil }
}

func ownedByAnotherKind() podOption {
	return func(pod *core.Pod) { pod.OwnerReferences[0].Kind = "StatefulSet" }
}

func ownedByAnotherGroup() podOption {
	return func(pod *core.Pod) { pod.OwnerReferences[0].APIVersion = "apps/v1" }
}

func partOfAnotherInstance() podOption {
	return func(pod *core.Pod) { pod.Labels[deviceplugin.InstancePartOfLabelKey] = "some-other-uid" }
}

func createdAt(ts time.Time) podOption {
	return func(pod *core.Pod) { pod.CreationTimestamp = meta.NewTime(ts) }
}

// podStats is the kubelet entry of a pod. The name matters as much as the UID: the readout is
// node-wide and matched on both.
func podStats(name, podUID string, cpuNanoCores uint64) kubeletstats.PodStats {
	return kubeletstats.PodStats{
		PodRef: kubeletstats.PodReference{
			Namespace: testNamespace, Name: name, UID: podUID,
		},
		CPU: &kubeletstats.CPUStats{Time: meta.Now(), UsageNanoCores: ptr.To(cpuNanoCores)},
	}
}

// newTestPoller builds a poller over a fake informer carrying the pods, plus this process's own
// device manager pod so the exporter role resolves to it.
func newTestPoller(nodeName string, pods ...*core.Pod) *Poller {
	return newTestPollerWith(nodeName,
		append([]*core.Pod{deviceManagerPodFixture(nodeName, testSelfPodName, "nvidia")}, pods...)...)
}

// newTestPollerWith builds a poller over exactly the given pods.
func newTestPollerWith(nodeName string, pods ...*core.Pod) *Poller {
	builder := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithIndex(&core.Pod{}, deviceplugin.IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		})
	for i := range pods {
		builder = builder.WithObjects(pods[i])
	}
	p, err := New(&Config{ClientReader: builder.Build(), MonitorPeriod: 15 * time.Second})
	if err != nil {
		panic(err)
	}
	p.nodeName = nodeName
	return p
}

func TestPoller_poll(t *testing.T) {
	// Every figure the exporter publishes is scoped to one Instance, so what this pins is which
	// pods count as one, which Instance each belongs to, and what happens when the source fails.
	testCases := []struct {
		name string

		pods    []*core.Pod
		summary *kubeletstats.Summary

		wantSnapshot  bool
		wantInstances []InstanceSample
	}{
		{
			name: "one instance the kubelet measured",
			pods: []*core.Pod{instancePodFixture("n", "inst", testInstanceUID, "pod-uid-1")},
			summary: &kubeletstats.Summary{
				Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
			},
			wantSnapshot: true,
			wantInstances: []InstanceSample{
				{Namespace: testNamespace, Name: "inst", UID: testInstanceUID},
			},
		},
		{
			name: "a node running no instance still stores a round",
			pods: nil,
			summary: &kubeletstats.Summary{
				Pods: []kubeletstats.PodStats{podStats("someone-else", "someone-else-uid", 1)},
			},
			wantSnapshot: true,
		},
		{
			name: "a terminating pod is not an instance",
			pods: []*core.Pod{
				instancePodFixture("n", "inst", testInstanceUID, "pod-uid-1", terminating()),
			},
			summary: &kubeletstats.Summary{
				Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
			},
			wantSnapshot: true,
		},
		{
			name: "a pod with no controller is not an instance",
			pods: []*core.Pod{
				instancePodFixture("n", "inst", testInstanceUID, "pod-uid-1", withoutOwner()),
			},
			summary: &kubeletstats.Summary{
				Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
			},
			wantSnapshot: true,
		},
		{
			name: "a pod controlled by another kind is not an instance",
			pods: []*core.Pod{
				instancePodFixture("n", "inst", testInstanceUID, "pod-uid-1", ownedByAnotherKind()),
			},
			summary: &kubeletstats.Summary{
				Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
			},
			wantSnapshot: true,
		},
		{
			name: "an Instance kind of another API group is not this Instance",
			pods: []*core.Pod{
				instancePodFixture("n", "inst", testInstanceUID, "pod-uid-1", ownedByAnotherGroup()),
			},
			summary: &kubeletstats.Summary{
				Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
			},
			wantSnapshot: true,
		},
		{
			name: "a pod whose label disagrees with its owner is skipped",
			pods: []*core.Pod{
				instancePodFixture("n", "inst", testInstanceUID, "pod-uid-1", partOfAnotherInstance()),
			},
			summary: &kubeletstats.Summary{
				Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
			},
			wantSnapshot: true,
		},
		{
			name: "two pods of one instance collapse to the newer",
			pods: []*core.Pod{
				instancePodFixture("n", "inst", testInstanceUID, "old-pod-uid",
					createdAt(time.Now().Add(-time.Hour))),
				instancePodFixture("n", "inst-new", testInstanceUID, "new-pod-uid"),
			},
			summary: &kubeletstats.Summary{Pods: []kubeletstats.PodStats{
				podStats("inst", "old-pod-uid", 111),
				podStats("inst-new", "new-pod-uid", 500_000_000),
			}},
			wantSnapshot: true,
			// One entry, and the figures of the pod that is actually running.
			wantInstances: []InstanceSample{
				{Namespace: testNamespace, Name: "inst-new", UID: testInstanceUID},
			},
		},
		{
			name: "an instance the kubelet does not carry is left out",
			pods: []*core.Pod{instancePodFixture("n", "inst", testInstanceUID, "pod-uid-1")},
			// The kubelet answers, but its stats provider has not picked the pod up.
			summary:      &kubeletstats.Summary{},
			wantSnapshot: true,
		},
		{
			name: "a previous incarnation's entry is never served",
			pods: []*core.Pod{instancePodFixture("n", "inst", testInstanceUID, "pod-uid-1")},
			summary: &kubeletstats.Summary{
				// Same namespace and name, a different pod UID.
				Pods: []kubeletstats.PodStats{podStats("inst", "stale-pod-uid", 999)},
			},
			wantSnapshot: true,
		},
		// A failed kubelet readout keeps the round and drops only the measured figures — see
		// TestPoller_keepsTheRoundWhenOnlyTheKubeletFails, which asserts what survives it.
	}

	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// A node per case: the kubelet readout is cached per node, and a shared name would
			// let one case answer another's poll.
			nodeName := "node-" + strconv.Itoa(i)
			for _, pod := range tc.pods {
				pod.Spec.NodeName = nodeName
			}
			serveNodeSummary(t, nodeName, tc.summary)

			p := newTestPoller(nodeName, tc.pods...)
			p.poll(context.Background())

			snapshot := p.snapshot()
			if !tc.wantSnapshot {
				assert.Nil(t, snapshot, "a failed round must leave no figures behind to report as current")
				return
			}
			require.NotNil(t, snapshot)

			require.Len(t, snapshot.Instances, len(tc.wantInstances))
			for j := range tc.wantInstances {
				got := snapshot.Instances[j]
				assert.Equal(t, tc.wantInstances[j].Namespace, got.Namespace)
				assert.Equal(t, tc.wantInstances[j].Name, got.Name)
				assert.Equal(t, tc.wantInstances[j].UID, got.UID)
				// The identity is the Instance's, the figures are the backing pod's.
				assert.Equal(t, uint64(2000), got.Sample.CPUTotalMilliCores)
				require.NotNil(t, got.Sample.CPUUsedMilliCores)
				assert.Equal(t, uint64(500), *got.Sample.CPUUsedMilliCores)
			}
		})
	}
}

// TestPoller_gateOnStartedContainer pins the gate both surfaces apply: an Instance none of whose
// containers has started consumes nothing, so its usage is zero by reasoning — and reading a
// source for it could only report a neighbour's figures as its own.
func TestPoller_gateOnStartedContainer(t *testing.T) {
	const nodeName = "node-unstarted"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1", unstarted())
	p := newTestPoller(nodeName, pod)

	// Not a stubbed source: a read of any kind fails the test, which is what makes "no upstream
	// I/O" an assertion rather than a claim.
	refuseUpstream(t)

	p.poll(context.Background())

	snapshot := p.snapshot()
	require.NotNil(t, snapshot)
	assert.True(t, snapshot.UsageMeasured, "nothing was left unmeasured: the usage is known")
	require.Len(t, snapshot.Instances, 1)

	inst := snapshot.Instances[0]
	assert.Equal(t, uint64(2000), inst.Sample.CPUTotalMilliCores, "the declared totals still stand")
	// Zero here is a measurement: nothing has started, so nothing exists that could consume.
	require.NotNil(t, inst.Sample.CPUUsedMilliCores)
	assert.Zero(t, *inst.Sample.CPUUsedMilliCores)
	require.NotNil(t, inst.Sample.MemoryUsedMiB)
	assert.Zero(t, *inst.Sample.MemoryUsedMiB)
	require.NotNil(t, inst.Sample.StorageUsedMiB)
	assert.Zero(t, *inst.Sample.StorageUsedMiB)
	// Nothing of it holds a card yet, so the card's figures are somebody else's.
	assert.Empty(t, inst.Accelerators)
	assert.Nil(t, inst.Grants)
}

// TestPoller_gateSpares only the instances that have started nothing: a node with one of each
// still reads the kubelet once, and the two Instances answer differently.
func TestPoller_gateSparesOnlyTheUnstarted(t *testing.T) {
	const nodeName = "node-mixed"
	running := instancePodFixture(nodeName, "running", testInstanceUID, "pod-uid-1")
	pending := instancePodFixture(nodeName, "pending", "instance-uid-2", "pod-uid-2", unstarted())
	p := newTestPoller(nodeName, running, pending)

	serveNodeSummary(t, nodeName, &kubeletstats.Summary{Pods: []kubeletstats.PodStats{
		podStats("running", "pod-uid-1", 500_000_000),
		// The kubelet carries the unstarted pod too; it is the gate, not the source, that
		// decides — the pod's own figures are never read.
		podStats("pending", "pod-uid-2", 900_000_000),
	}})

	p.poll(context.Background())

	snapshot := p.snapshot()
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Instances, 2)

	byName := make(map[string]InstanceSample, 2)
	for _, inst := range snapshot.Instances {
		byName[inst.Name] = inst
	}

	require.NotNil(t, byName["running"].Sample.CPUUsedMilliCores)
	assert.Equal(t, uint64(500), *byName["running"].Sample.CPUUsedMilliCores)
	require.NotNil(t, byName["pending"].Sample.CPUUsedMilliCores)
	assert.Zero(t, *byName["pending"].Sample.CPUUsedMilliCores,
		"a measurement the kubelet happens to carry is not this Instance's to report")
}

// TestPoller_gateSurvivesAFailedKubelet pins that a source failure cannot flip the gate's answer:
// the Instances that started nothing still report zeros, which needed no source at all.
func TestPoller_gateSurvivesAFailedKubelet(t *testing.T) {
	const nodeName = "node-unstarted-down"
	running := instancePodFixture(nodeName, "running", testInstanceUID, "pod-uid-1")
	pending := instancePodFixture(nodeName, "pending", "instance-uid-2", "pod-uid-2", unstarted())
	p := newTestPoller(nodeName, running, pending)
	serveNodeSummary(t, nodeName, nil) // the node proxy answers 502

	p.poll(context.Background())

	snapshot := p.snapshot()
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Instances, 2)
	for _, inst := range snapshot.Instances {
		if inst.Name == "pending" {
			require.NotNil(t, inst.Sample.CPUUsedMilliCores)
			assert.Zero(t, *inst.Sample.CPUUsedMilliCores)
			continue
		}
		assert.Nil(t, inst.Sample.CPUUsedMilliCores, "a measurement that failed stays absent")
	}
}

// TestPoller_leavesGrantsNilWithoutAnAccelerator pins the field's stated invariant — Grants is nil
// exactly when Accelerators is empty — because the collector reads that nil as "nothing here can say
// whose a card's figures are". An index built for a Pod holding no card would satisfy the guard while
// answering for nothing, and the guard could never fire again.
func TestPoller_leavesGrantsNilWithoutAnAccelerator(t *testing.T) {
	const nodeName = "node-cpu-only"
	// A started Instance, so it goes through the full sample path rather than the gate's shortcut.
	p := newTestPoller(nodeName, instancePodFixture(nodeName, "cpu", testInstanceUID, "pod-uid-1"))
	serveNodeSummary(t, nodeName, &kubeletstats.Summary{Pods: []kubeletstats.PodStats{
		podStats("cpu", "pod-uid-1", 500_000_000),
	}})

	p.poll(context.Background())

	snapshot := p.snapshot()
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Instances, 1)

	inst := snapshot.Instances[0]
	require.NotNil(t, inst.Sample.CPUUsedMilliCores, "the pod-level figures are unaffected")
	assert.Empty(t, inst.Accelerators)
	assert.Nil(t, inst.Grants)
}

// TestPoller_carriesTheCarvedShare pins that the round carries the quota half of the join, read
// off the container the allocation was enforced on — the scrape supplies the measured half.
func TestPoller_carriesTheCarvedShare(t *testing.T) {
	const nodeName = "node-sliced"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1")
	coresResName := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName("nvidia")
	pod.Spec.Containers[0].Resources.Limits[coresResName] = *resource.NewQuantity(25, resource.DecimalSI)
	setPodAllocation(pod, workercore.AcceleratorAllocation{
		ID:        "gpu-uuid-1",
		Index:     3,
		Mode:      workercore.DeviceAllocationModeSliced,
		Allocated: nodefeature.ResourceMaxUnits / 4,
	})

	p := newTestPoller(nodeName, pod)
	serveNodeSummary(t, nodeName, &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
	})
	p.poll(context.Background())

	snapshot := p.snapshot()
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Instances, 1)
	inst := snapshot.Instances[0]
	require.Len(t, inst.Accelerators, 1)
	require.NotNil(t, inst.Grants)

	// The card's own readings come from the monitor snapshot at scrape time, so the resolution is
	// asked with them here.
	resolved := inst.Grants.Resolve(nil, "nvidia", &device.AcceleratorMetrics{ID: "gpu-uuid-1", Memory: 81920})
	require.Len(t, resolved, 1, "one logical slice of one card is one grant")
	got := resolved[0]
	assert.Equal(t, workercore.DeviceAllocationModeSliced.String(), got.Mode)
	assert.Equal(t, ptr.To[uint64](20480), got.MemoryTotalMiB, "a quarter of the card is the grant")
	assert.Nil(t, got.MemoryUsedMiB, "no producer answered, so nothing measured the usage")
	assert.Nil(t, got.CoresUtilizationPercent)
}

// TestPoller_unreadableAllocationKeepsThePodFigures pins that a pod whose allocation annotation
// cannot be read names no card rather than a guessed one, and keeps its pod-level figures.
func TestPoller_unreadableAllocationKeepsThePodFigures(t *testing.T) {
	const nodeName = "node-badanno"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1")
	pod.Annotations = map[string]string{deviceplugin.AllocatedAcceleratorAnnoKey: "{not-json"}

	p := newTestPoller(nodeName, pod)
	serveNodeSummary(t, nodeName, &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
	})
	p.poll(context.Background())

	snapshot := p.snapshot()
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Instances, 1)
	assert.Empty(t, snapshot.Instances[0].Accelerators)
	require.NotNil(t, snapshot.Instances[0].Sample.CPUUsedMilliCores)
}

// setPodAllocation records one container's allocation on the pod, as the device plugin writes it.
func setPodAllocation(pod *core.Pod, accelerators ...workercore.AcceleratorAllocation) {
	allocations := deviceplugin.PodAllocations{
		"main": {
			Devices: workercore.DevicesStatus{
				Groups: []workercore.DevicesAllocationGroup{
					{ID: "grp", Manufacturer: "nvidia", Accelerators: accelerators},
				},
			},
		},
	}
	anno, _ := json.Marshal(allocations)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = string(anno)
}

// TestPoller_dropsStaleFiguresOnFailure pins that a round which loses the kubelet after a good
// one leaves none of that round's measurements behind: reporting the figures of several periods
// ago as current is worse than reporting none. What survives is only what needs no measuring.
func TestPoller_dropsStaleFiguresOnFailure(t *testing.T) {
	const nodeName = "node-drop"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1")
	p := newTestPoller(nodeName, pod)

	serveNodeSummary(t, nodeName, &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
	})
	p.poll(context.Background())
	first := p.snapshot()
	require.NotNil(t, first)
	require.NotNil(t, first.Instances[0].Sample.CPUUsedMilliCores)

	serveNodeSummary(t, nodeName, nil)
	p.poll(context.Background())

	second := p.snapshot()
	require.NotNil(t, second)
	assert.False(t, second.UsageMeasured)
	require.Len(t, second.Instances, 1)
	assert.Nil(t, second.Instances[0].Sample.CPUUsedMilliCores,
		"the previous round's measurement must not be carried forward as current")
}

// TestPoller_readsAfreshEveryRound pins the cadence the exporter advertises. Rounds start one
// period apart while a cached readout is stamped mid-round, so a poller that handed its period
// to the readout cache would find every second round's entry still young enough, republish the
// round before it, and reach the kubelet only every other period — at half the cadence, with
// nothing on the wire to show it. Two back-to-back rounds over a changed source must see the
// change.
func TestPoller_readsAfreshEveryRound(t *testing.T) {
	const nodeName = "node-afresh"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1")
	p := newTestPoller(nodeName, pod)

	serveNodeSummary(t, nodeName, &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
	})
	p.poll(context.Background())
	first := p.snapshot()
	require.NotNil(t, first)
	require.Len(t, first.Instances, 1)
	require.NotNil(t, first.Instances[0].Sample.CPUUsedMilliCores)
	assert.Equal(t, uint64(500), *first.Instances[0].Sample.CPUUsedMilliCores)

	// The very next round, well inside the poller's own period.
	serveNodeSummary(t, nodeName, &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 900_000_000)},
	})
	p.poll(context.Background())
	second := p.snapshot()
	require.NotNil(t, second)
	require.Len(t, second.Instances, 1)
	require.NotNil(t, second.Instances[0].Sample.CPUUsedMilliCores)
	assert.Equal(t, uint64(900), *second.Instances[0].Sample.CPUUsedMilliCores,
		"the round must reach the kubelet, not republish the previous round's readout")
}

// TestPoller_electionFailureKeepsTheRound pins that a failed election costs the pod-level
// families and nothing else. Accelerator figures are not subject to the rule — device IDs are
// disjoint across manufacturers — so failing the whole round over it would drop figures the
// election has no say in, and a device manager whose pod name is missing from its environment
// would publish nothing at all rather than everything but the pod-level families.
func TestPoller_electionFailureKeepsTheRound(t *testing.T) {
	const nodeName = "node-noelection"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1")
	p := newTestPoller(nodeName, pod)
	serveNodeSummary(t, nodeName, &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
	})

	// The one input the election cannot do without.
	t.Setenv("KUBERNETES_POD_NAME", "")

	p.poll(context.Background())

	snapshot := p.snapshot()
	require.NotNil(t, snapshot, "the round still measured what it was asked to measure")
	assert.False(t, snapshot.Exporting, "an undecidable election never claims the role")
	require.Len(t, snapshot.Instances, 1,
		"the Instance and its allocation are still carried, so the accelerator families survive")
}

// TestPoller_keepsTheRoundWhenOnlyTheKubeletFails pins that one failed source does not blank the
// other. The accelerator families are labeled by Instance but measured by the monitor loop, and
// which Instances the node runs comes from the informer — so a kubelet that will not answer costs
// the measured pod-level figures and nothing else. Dropping the round instead would take healthy
// accelerator data with it while reporting the snapshot source as fine.
func TestPoller_keepsTheRoundWhenOnlyTheKubeletFails(t *testing.T) {
	const nodeName = "node-kubeletdown"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1")
	p := newTestPoller(nodeName, pod)
	serveNodeSummary(t, nodeName, nil) // the node proxy answers 502

	p.poll(context.Background())

	snapshot := p.snapshot()
	require.NotNil(t, snapshot, "the round carries what the kubelet has no part in")
	assert.False(t, snapshot.UsageMeasured, "and says plainly that nothing was measured")
	require.Len(t, snapshot.Instances, 1,
		"the Instance and its allocation survive, so the accelerator families can publish")
	inst := snapshot.Instances[0]
	assert.Equal(t, uint64(2000), inst.Sample.CPUTotalMilliCores,
		"the declared totals need no measurement")
	assert.Nil(t, inst.Sample.CPUUsedMilliCores, "an unmeasured figure stays absent, never zero")
	assert.Nil(t, inst.Sample.MemoryUsedMiB)
	assert.Nil(t, inst.Sample.StorageUsedMiB)
}

// TestPoller_pollWithoutTheFieldIndex pins that the poller reports a broken informer instead of
// publishing an empty node: it lists Pods through a field index another component registers, and
// "no pods on this node" and "the index is missing" must not look the same on /metrics.
func TestPoller_pollWithoutTheFieldIndex(t *testing.T) {
	const nodeName = "node-noindex"
	serveNodeSummary(t, nodeName, &kubeletstats.Summary{})

	p := newTestPoller(nodeName)
	p.reader = ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	p.poll(context.Background())

	assert.Nil(t, p.snapshot())
	assert.Contains(t, p.lastFailure, deviceplugin.IndexingPodsByNodeName)
}

// TestPoller_logFailure pins the de-duplication behind AC3.5. lastFailure is the decision
// itself: while it holds, the round is logged quietly, so a poller whose source is down cannot
// repeat one line every period for the life of the process.
func TestPoller_logFailure(t *testing.T) {
	p := &Poller{nodeName: "n"}

	p.logFailure(errors.New("kubelet down"))
	assert.Equal(t, "kubelet down", p.lastFailure)

	p.logFailure(errors.New("kubelet down"))
	assert.Equal(t, "kubelet down", p.lastFailure, "a repeat is remembered, not reported again")

	p.logFailure(errors.New("node proxy denied"))
	assert.Equal(t, "node proxy denied", p.lastFailure, "a different reason is reported afresh")
}

func TestIsNewer(t *testing.T) {
	older := instancePodFixture("n", "a", testInstanceUID, "uid-a",
		createdAt(time.Now().Add(-time.Hour)))
	newer := instancePodFixture("n", "b", testInstanceUID, "uid-b")

	assert.True(t, isNewer(newer, older))
	assert.False(t, isNewer(older, newer))

	// Two pods stamped in the same second must still resolve the same way on every poll,
	// otherwise the exporter would flip between them and so would the series.
	same := time.Now()
	first := instancePodFixture("n", "a", testInstanceUID, "uid-a", createdAt(same))
	second := instancePodFixture("n", "b", testInstanceUID, "uid-b", createdAt(same))
	assert.True(t, isNewer(second, first))
	assert.False(t, isNewer(first, second))
}

// TestPoller_GatherDuringPolling is the -race guard behind the whole design: a scrape gathers
// while the poller replaces the round underneath it, and touches no source of its own.
func TestPoller_GatherDuringPolling(t *testing.T) {
	const nodeName = "node-race"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1")
	serveNodeSummary(t, nodeName, &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
	})

	p := newTestPoller(nodeName, pod)
	p.period = time.Millisecond
	collector := NewCollector(p, func() *detector.MonitorSnapshot { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.Start(ctx)
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		// Non-zero throughout: the collector always reports on itself, even before the first
		// round and after a failed one.
		assert.NotZero(t, testutil.CollectAndCount(collector))
	}

	cancel()
	<-done
}

// TestPoller_StartWithoutNodeName pins that a broken deployment degrades the metrics instead of
// taking the device manager down with it.
func TestPoller_StartWithoutNodeName(t *testing.T) {
	t.Setenv("KUBERNETES_NODE_NAME", "")

	p, err := New(&Config{
		ClientReader:  newTestPoller("unused").reader,
		MonitorPeriod: time.Millisecond,
	})
	require.NoError(t, err)
	require.Empty(t, p.nodeName)

	require.NoError(t, p.Start(context.Background()), "an unset node name must not fail the process")
	assert.Nil(t, p.snapshot())
}
