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
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
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
	testNamespace   = "tenant"
	testInstanceUID = "instance-uid-1"
)

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
			Labels:            map[string]string{_InstancePartOfLabelKey: instUID},
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
	}
	for _, opt := range opts {
		opt(pod)
	}
	return pod
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
	return func(pod *core.Pod) { pod.Labels[_InstancePartOfLabelKey] = "some-other-uid" }
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

// newTestPoller builds a poller over a fake informer carrying the pods.
func newTestPoller(nodeName string, pods ...*core.Pod) *Poller {
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
		{
			name:    "a failed readout drops the round",
			pods:    []*core.Pod{instancePodFixture("n", "inst", testInstanceUID, "pod-uid-1")},
			summary: nil,
		},
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

			snapshot := p.Snapshot()
			if !tc.wantSnapshot {
				assert.Nil(t, snapshot, "a failed round must leave no figures behind to report as current")
				return
			}
			require.NotNil(t, snapshot)
			assert.Equal(t, int64(15), snapshot.PeriodSeconds,
				"the round reports the cadence a consumer scales its staleness bound to")

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

// TestPoller_dropsStaleRoundOnFailure pins that a round which fails after a good one leaves
// nothing behind: reporting the figures of several periods ago as current is worse than
// reporting none.
func TestPoller_dropsStaleRoundOnFailure(t *testing.T) {
	const nodeName = "node-drop"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1")
	p := newTestPoller(nodeName, pod)
	// Read afresh every round, so the second one actually reaches the failing source.
	p.period = 0

	serveNodeSummary(t, nodeName, &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
	})
	p.poll(context.Background())
	require.NotNil(t, p.Snapshot())

	serveNodeSummary(t, nodeName, nil)
	p.poll(context.Background())
	assert.Nil(t, p.Snapshot())
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

	assert.Nil(t, p.Snapshot())
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

// TestPoller_SnapshotDuringPolling is the -race guard behind the whole design: a scrape reads
// the snapshot while the poller replaces it, and must never touch the sources itself.
func TestPoller_SnapshotDuringPolling(t *testing.T) {
	const nodeName = "node-race"
	pod := instancePodFixture(nodeName, "inst", testInstanceUID, "pod-uid-1")
	serveNodeSummary(t, nodeName, &kubeletstats.Summary{
		Pods: []kubeletstats.PodStats{podStats("inst", "pod-uid-1", 500_000_000)},
	})

	p := newTestPoller(nodeName, pod)
	p.period = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.Start(ctx)
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s := p.Snapshot(); s != nil {
			assert.LessOrEqual(t, len(s.Instances), 1)
		}
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
	assert.Nil(t, p.Snapshot())
}
