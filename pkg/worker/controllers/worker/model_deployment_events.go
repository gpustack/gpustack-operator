package worker

import (
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

const (
	// modelDeploymentKVLeaseWindow is kv_lease_duration's default.
	//
	// It does not expire because a request queued for a long time, but it DOES expire when the
	// engine Pod's heartbeat is interrupted — preemption, eviction, restart — and under the default
	// failure policy the request then simply fails. So the window is what an operator needs in order
	// to correlate a burst of failed requests with a replica that went away.
	//
	// It is stated as the default rather than read from the pool: the value lives on the backend,
	// which the rule that resolves the Binding reads. Until then, naming the default and saying so
	// is honest, where naming a number this operator did not read would not be.
	modelDeploymentKVLeaseWindow = "30s"

	// _PodReasonEvicted is the Pod status reason the kubelet sets when it evicts a Pod under node
	// pressure. Kueue preemption arrives as an ordinary deletion instead, so both paths are covered
	// by watching what the Pod says about itself.
	_PodReasonEvicted = "Evicted"
)

// The event reasons a ModelDeployment records about its replicas. They are separate reasons rather
// than one, because an operator filtering events is asking which of them happened.
const (
	modelDeploymentEventReplicaEvicted   = "ReplicaEvicted"
	modelDeploymentEventReplicaLeaving   = "ReplicaLeaving"
	modelDeploymentEventReplicaRestarted = "ReplicaRestarted"

	// modelDeploymentEventRuntimeVersionSkew reports a pool whose nodes do not agree on an
	// accelerator runtime version, which is what a driver rollout looks like while it runs.
	modelDeploymentEventRuntimeVersionSkew = "RuntimeVersionSkew"

	// modelDeploymentEventRenderFailed reports a pass that could not build a replica at all.
	//
	// It exists because that failure has no other reader-visible home. Rendering aborts the pass
	// before any status is written, so the object keeps whatever it said last -- Phase=Starting with
	// "no replica has been created yet" -- and none of the conditions names the cause. Some of these
	// are permanent: a manufacturer with no runner backend, or an engine family with no published
	// variant, never resolves however long the controller retries, and the deployment would sit in a
	// state that reads like a slow start forever.
	//
	// Admission cannot take this one instead. The check needs the InstanceType's OBSERVED detail,
	// and the ModelDeployment webhook holds no client by design.
	//
	// Repeats aggregate into one Event with a count rather than a stream, which is what keeps a
	// per-pass emission readable.
	modelDeploymentEventRenderFailed = "RenderFailed"
)

// modelDeploymentReplicaDeparture is one replica that has stopped serving from the cache it shared,
// and why.
type modelDeploymentReplicaDeparture struct {
	pod     string
	reason  string
	message string
}

// modelDeploymentReplicaDepartures reads, from the observed Pods alone, which replicas have left.
//
// It is derived from OBSERVED STATE rather than from a deletion hook, and that is the whole
// design. A hook fires once, on an event that can be missed — a restarted manager, a dropped watch —
// and a correlation that exists only in an event nobody received is not written down at all. Reading
// the Pods answers the same question on every pass, so a missed event costs a delay and not the
// record.
//
// It follows that a replica must be observable while it leaves. That is why the reconciler deletes
// replicas with the default grace period rather than with ctrlclix.Terminated: a Pod removed with no
// grace at all can be gone before any pass sees it go.
//
// Repeats are the API server's problem, not this function's: an event recorder aggregates identical
// events into one with a count, so re-reporting a replica that is still terminating adds to that
// count instead of adding a line.
func modelDeploymentReplicaDepartures(pods []core.Pod) []modelDeploymentReplicaDeparture {
	var departures []modelDeploymentReplicaDeparture

	for i := range pods {
		pod := &pods[i]

		switch {
		case pod.Status.Reason == _PodReasonEvicted:
			departures = append(departures, modelDeploymentReplicaDeparture{
				pod:    pod.Name,
				reason: modelDeploymentEventReplicaEvicted,
				message: fmt.Sprintf(
					"replica %s was evicted; its cached blocks are lost to its siblings, and any KV "+
						"lease it holds lapses after the %s kv_lease_duration window, failing the "+
						"requests still waiting on them",
					pod.Name, modelDeploymentKVLeaseWindow),
			})
		case pod.DeletionTimestamp != nil:
			departures = append(departures, modelDeploymentReplicaDeparture{
				pod:    pod.Name,
				reason: modelDeploymentEventReplicaLeaving,
				message: fmt.Sprintf(
					"replica %s is going away; its cached blocks are lost to its siblings, and any "+
						"KV lease it holds lapses after the %s kv_lease_duration window, failing "+
						"the requests still waiting on them",
					pod.Name, modelDeploymentKVLeaseWindow),
			})
		default:
			restarts := modelDeploymentReplicaRestarts(pod)
			if restarts == 0 {
				continue
			}
			departures = append(departures, modelDeploymentReplicaDeparture{
				pod:    pod.Name,
				reason: modelDeploymentEventReplicaRestarted,
				message: fmt.Sprintf(
					"replica %s has restarted %d time(s); each restart interrupts the engine's "+
						"heartbeat, so the blocks it held were lost and any KV lease lapsed after "+
						"the %s kv_lease_duration window",
					pod.Name, restarts, modelDeploymentKVLeaseWindow),
			})
		}
	}

	return departures
}

// modelDeploymentReplicaRestarts is how many times a replica's containers have restarted.
//
// A restart is a departure in every way that matters here: the engine process died, so its
// heartbeat stopped and the blocks it held went with it — even though the Pod, and its name, stayed.
func modelDeploymentReplicaRestarts(pod *core.Pod) int32 {
	var restarts int32
	for i := range pod.Status.ContainerStatuses {
		restarts += pod.Status.ContainerStatuses[i].RestartCount
	}

	return restarts
}

// modelDeploymentRuntimeVersionSkew describes a pool whose nodes disagree on a runtime version, or
// reports false when they agree or when nothing was observed.
//
// IT EXISTS BECAUSE THE MINIMUM'S FAILURE MODE IS UNTRACEABLE WITHOUT IT. A synthesized image takes
// the lowest version in the pool, since that is the only one every node can run and admission picks
// the node after the image is fixed. When one un-upgraded node holds the pool back onto a version
// the runner never published for the requested engine version, the user sees an ImagePullBackOff
// with nothing pointing at the node responsible. This event is that pointer.
//
// It reads len() > 1 on the published list rather than re-deriving anything, which is why the list
// is published at all: a consumer needs a length comparison, not a second copy of the aggregation.
//
// The message names the value taken AND the ones skipped, because either alone leaves the operator
// guessing which end of the range to act on.
func modelDeploymentRuntimeVersionSkew(
	roleName string, detail workercore.InstanceTypeDetail,
) (string, bool) {
	if len(detail.RuntimeVersions) < 2 {
		return "", false
	}

	return fmt.Sprintf(
		"role %q builds its image from accelerator runtime version %s, the lowest of %d the pool "+
			"reports (%s); the lowest is used because a workload's image is fixed before admission "+
			"chooses its node, and only that version runs everywhere",
		roleName, detail.RuntimeVersion, len(detail.RuntimeVersions),
		strings.Join(detail.RuntimeVersions, ", "),
	), true
}
