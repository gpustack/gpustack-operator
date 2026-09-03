package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlrecord "k8s.io/client-go/tools/record"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// TestModelDeploymentEvents_Departures walks every way a replica stops serving from the cache it
// shared. Each case asserts the reason AND that the message carries the two things an operator
// correlating failed requests needs: which replica, and how long its lease outlives it.
func TestModelDeploymentEvents_Departures(t *testing.T) {
	testCases := []struct {
		name       string
		mutate     func(*core.Pod)
		wantReason string
	}{
		{
			name:       "a healthy replica has not left",
			wantReason: "",
		},
		{
			name: "evicted under node pressure",
			mutate: func(pod *core.Pod) {
				pod.Status.Reason = _PodReasonEvicted
			},
			wantReason: modelDeploymentEventReplicaEvicted,
		},
		{
			// Kueue preemption arrives as an ordinary deletion rather than as an eviction, so the
			// two share this path: what is observable is that the Pod is going away.
			name: "deleted, which is also how preemption arrives",
			mutate: func(pod *core.Pod) {
				now := meta.Now()
				pod.DeletionTimestamp = &now
			},
			wantReason: modelDeploymentEventReplicaLeaving,
		},
		{
			// The Pod stayed and so did its name, but the engine process died: its heartbeat
			// stopped and the blocks it held went with it.
			name: "restarted in place",
			mutate: func(pod *core.Pod) {
				pod.Status.ContainerStatuses = []core.ContainerStatus{{Name: "main", RestartCount: 2}}
			},
			wantReason: modelDeploymentEventReplicaRestarted,
		},
		{
			// Both are true at once, and the more specific cause is the one worth reporting: an
			// operator reading "leaving" would go looking for who deleted it.
			name: "evicted and therefore also terminating",
			mutate: func(pod *core.Pod) {
				now := meta.Now()
				pod.DeletionTimestamp = &now
				pod.Status.Reason = _PodReasonEvicted
			},
			wantReason: modelDeploymentEventReplicaEvicted,
		},
		{
			name: "a restart count of zero is not a restart",
			mutate: func(pod *core.Pod) {
				pod.Status.ContainerStatuses = []core.ContainerStatus{{Name: "main", RestartCount: 0}}
			},
			wantReason: "",
		},
	}

	md := newRenderDeployment()

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := readyReplica(md, 0, true)
			if tc.mutate != nil {
				tc.mutate(pod)
			}

			departures := modelDeploymentReplicaDepartures([]core.Pod{*pod})

			if tc.wantReason == "" {
				assert.Empty(t, departures)

				return
			}

			require.Len(t, departures, 1)
			assert.Equal(t, tc.wantReason, departures[0].reason)
			assert.Equal(t, "qwen-server-0", departures[0].pod)
			assert.Contains(t, departures[0].message, "qwen-server-0",
				"the event must name the replica, or it correlates with nothing")
			assert.Contains(t, departures[0].message, modelDeploymentKVLeaseWindow,
				"and the lease window, which is how long the failures outlive the replica")
			assert.Contains(t, departures[0].message, "kv_lease_duration",
				"named so a reader can find the knob the number comes from")
		})
	}
}

// TestModelDeploymentEvents_ReadFromObservedStateNotADeleteHook is the design constraint, asserted
// rather than described.
//
// A hook fires once, on an event that can be missed — a restarted manager, a dropped watch — and a
// correlation that exists only in an event nobody received is not written down at all. Reading the
// Pods answers the same question on every pass, which is what a second pass reporting the same
// departure demonstrates: the record does not depend on having caught the moment.
func TestModelDeploymentEvents_ReadFromObservedStateNotADeleteHook(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 1 })

	leaving := readyReplica(md, 0, true)
	now := meta.Now()
	leaving.DeletionTimestamp = &now
	leaving.Finalizers = []string{"test.gpustack.ai/hold"}

	cli := newModelDeploymentClient(md, newRenderInstanceType(), leaving)
	recorder := ctrlrecord.NewFakeRecorder(64)
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: recorder}

	for pass := 1; pass <= 2; pass++ {
		_, err := reconcileModelDeploymentWith(t, r)
		require.NoError(t, err, "pass %d", pass)
	}

	events := drainEvents(recorder)
	require.Len(t, events, 2,
		"every pass re-reads the departure; collapsing repeats is the recorder's job, not this one's")
	for _, e := range events {
		assert.Contains(t, e, modelDeploymentEventReplicaLeaving)
		assert.Contains(t, e, "qwen-server-0")
	}
}

// TestModelDeploymentEvents_NothingForASteadyDeployment keeps the events for what they are for. A
// deployment whose replicas are all running has nothing to correlate, and an event stream that said
// otherwise would train an operator to ignore it.
func TestModelDeploymentEvents_NothingForASteadyDeployment(t *testing.T) {
	md := newRenderDeployment()
	cli := newModelDeploymentClient(md, newRenderInstanceType())
	recorder := ctrlrecord.NewFakeRecorder(64)
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: recorder}

	_, err := reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)

	assert.Empty(t, drainEvents(recorder))
}

// TestModelDeploymentEvents_TheReconcilerLeavesReplicasObservableWhileTheyGo pins the dependency
// between the two halves of this task, which is the kind that breaks silently.
//
// Reading departures off the Pods only works while a departing Pod is still there to be read. A
// replica deleted with no grace at all can be gone before any pass sees it go, so the reconciler
// must issue its deletes with the object's own grace period — never with ctrlclix.Terminated, which
// the Instance path uses and which would be the obvious thing to copy.
func TestModelDeploymentEvents_TheReconcilerLeavesReplicasObservableWhileTheyGo(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 2 })
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, md, newRenderInstanceType())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2)

	scaled := getModelDeployment(t, cli)
	scaled.Spec.Roles[0].Replicas = 1
	require.NoError(t, cli.Update(context.Background(), scaled))

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	require.Equal(t, 1, writes.deletes, "the scale down must have removed one replica")
	require.Len(t, writes.deleteGrace, 1)
	assert.Nil(t, writes.deleteGrace[0],
		"a replica deleted with no grace at all can be gone before any pass observes it leave")
}

// skewDetail is an observed detail reporting the given versions, with the published single value
// taken from the list exactly as the reconciler takes it.
//
// It derives the single value rather than taking it as a second argument on purpose: a fixture that
// let a case state them independently could set up a state the aggregation cannot produce, and a
// test passing against an impossible input proves nothing about the real one.
func skewDetail(versions ...string) workercore.InstanceTypeDetail {
	d := workercore.InstanceTypeDetail{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
			RuntimeVersions: versions,
		},
	}
	if len(versions) > 0 {
		d.RuntimeVersion = versions[0]
	}

	return d
}

func TestModelDeploymentRuntimeVersionSkew(t *testing.T) {
	testCases := []struct {
		name     string
		detail   workercore.InstanceTypeDetail
		wantSkew bool
		contains []string
		why      string
	}{
		{
			name:     "a pool that agrees says nothing",
			detail:   skewDetail("12.9"),
			wantSkew: false,
			why:      "one version is the normal state; warning about it would train readers to ignore this",
		},
		{
			name:     "nothing observed says nothing",
			detail:   skewDetail(),
			wantSkew: false,
			why:      "an unconverged detail is a wait, and there is no value to report yet",
		},
		{
			name:     "a disagreeing pool names the value taken and the ones skipped",
			detail:   skewDetail("12.4", "12.9"),
			wantSkew: true,
			// EACH SUBSTRING IS ANCHORED SO THAT ONLY ONE SOURCE CAN PRODUCE IT. A bare "12.4"
			// would not do: it appears both as the version chosen and inside the full list, so an
			// implementation that dropped the chosen value would still satisfy it. "runtime
			// version 12.4," can only come from the first position, and the parenthesised list only
			// from the join.
			contains: []string{`role "server"`, "runtime version 12.4,", "the lowest of 2", "(12.4, 12.9)"},
			why: "either half alone leaves the operator guessing which end of the range to act on, " +
				"and the taken value is what the image was actually built from",
		},
		{
			name:     "three versions",
			detail:   skewDetail("12.4", "12.8", "12.9"),
			wantSkew: true,
			contains: []string{"runtime version 12.4,", "the lowest of 3", "(12.4, 12.8, 12.9)"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			message, ok := modelDeploymentRuntimeVersionSkew("server", tc.detail)
			assert.Equal(t, tc.wantSkew, ok, tc.why)
			if !tc.wantSkew {
				assert.Empty(t, message, "no skew renders no message")

				return
			}
			for _, want := range tc.contains {
				assert.Contains(t, message, want, tc.why)
			}
		})
	}
}

// TestRecordModelDeploymentRuntimeVersionSkew covers the gate the pure function above cannot: the
// event is for a role whose image the OPERATOR chose, and a role that stated its own image is
// unaffected by the pool's version spread.
func TestRecordModelDeploymentRuntimeVersionSkew(t *testing.T) {
	testCases := []struct {
		name      string
		image     string
		versions  []string
		wantEvent bool
		why       string
	}{
		{
			name:      "synthesized image on a disagreeing pool warns",
			image:     "",
			versions:  []string{"12.4", "12.9"},
			wantEvent: true,
			why:       "this is the case where the operator picked the version, so it owes the reason",
		},
		{
			name:      "a stated image on the same pool is silent",
			image:     "my-registry/vllm:custom",
			versions:  []string{"12.4", "12.9"},
			wantEvent: false,
			why: "the operator did not choose that tag, so the spread tells its owner nothing " +
				"actionable -- and a warning that is usually noise gets ignored when it is not",
		},
		{
			name:      "synthesized image on an agreeing pool is silent",
			image:     "",
			versions:  []string{"12.9"},
			wantEvent: false,
			why:       "the gate is the disagreement, not the synthesis",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := ctrlrecord.NewFakeRecorder(8)
			r := &ModelDeploymentReconciler{Recorder: recorder}

			md := &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Name: "qwen-72b", Namespace: "team-a"},
			}
			role := &workercore.ModelDeploymentRole{Name: "server"}
			if tc.image != "" {
				role.Template = &workercore.InstanceTemplate{Image: tc.image}
			}
			it := &worker.InstanceType{
				Status: workercore.InstanceTypeStatus{Detail: skewDetail(tc.versions...)},
			}

			r.recordModelDeploymentRuntimeVersionSkew(md, role, it)

			events := drainEvents(recorder)
			if !tc.wantEvent {
				assert.Empty(t, events, tc.why)

				return
			}
			require.Len(t, events, 1, tc.why)
			assert.Contains(t, events[0], "Warning")
			assert.Contains(t, events[0], modelDeploymentEventRuntimeVersionSkew)
		})
	}
}
