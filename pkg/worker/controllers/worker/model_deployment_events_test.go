package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlrecord "k8s.io/client-go/tools/record"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TestModelDeploymentEvents_Departures walks every way a replica stops serving from the cache it
// shared. Each case asserts the reason AND that the message carries the two things an operator
// correlating failed requests needs: which replica, and how long its lease outlives it.
func TestModelDeploymentEvents_Departures(t *testing.T) {
	testCases := []struct {
		name       string
		mutate     func(*core.Pod)
		wantReason string
		// wantContains pins message content beyond the shared assertions: the kubelet's own words,
		// when the departure carries them.
		wantContains []string
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
			// The kubelet's message is the only record of why the eviction happened: the evicted
			// container's logs are gone with the eviction.
			name: "evicted, carrying the kubelet's why",
			mutate: func(pod *core.Pod) {
				pod.Status.Reason = _PodReasonEvicted
				pod.Status.Message = `Usage of EmptyDir volume "weights" exceeds the limit "500Mi".`
			},
			wantReason:   modelDeploymentEventReplicaEvicted,
			wantContains: []string{`Usage of EmptyDir volume "weights" exceeds the limit "500Mi".`},
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
			for _, want := range tc.wantContains {
				assert.Contains(t, departures[0].message, want)
			}
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
	// Router-backed so the 2-to-1 trim completes. The Router is seeded before the measured pass, so
	// the delete counter still observes exactly the one departing replica.
	md, router := retirementRouterFixture(md)
	writes := new(modelDeploymentWrites)
	cli := newCountingModelDeploymentClient(writes, md, newRenderInstanceType(), router)

	_, err := reconcileModelDeploymentRetiring(t, cli)
	require.NoError(t, err)
	require.Len(t, replicaNames(t, cli), 2)

	scaled := getModelDeployment(t, cli)
	scaled.Spec.Roles[0].Replicas = 1
	require.NoError(t, cli.Update(context.Background(), scaled))

	*writes = modelDeploymentWrites{}
	_, err = reconcileModelDeploymentRetiring(t, cli)
	require.NoError(t, err)

	// ONE delete for a 2-to-1 scale, because a replicas change trims the highest ordinal rather
	// than rebuilding anything: the departing replica's group is its own, and the survivor's total
	// never moved. What this case is about is unaffected -- how the delete is issued, not how many
	// there are.
	require.Equal(t, 1, writes.deletes, "the scale trims the ordinal the count no longer names")
	require.Len(t, writes.deleteGrace, 1)
	for i, grace := range writes.deleteGrace {
		assert.Nil(t, grace,
			"a replica deleted with no grace at all can be gone before any pass observes it leave "+
				"(delete %d)", i)
	}
}
