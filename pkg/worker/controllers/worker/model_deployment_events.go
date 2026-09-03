package worker

import (
	"fmt"

	core "k8s.io/api/core/v1"
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
