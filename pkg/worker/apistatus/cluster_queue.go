package apistatus

import (
	"k8s.io/utils/ptr"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/utils/slicex"
)

const (
	// ClusterQueueConditionActive indicates the cluster queue is active,
	// which means the cluster queue is ready to admit workloads.
	ClusterQueueConditionActive kubeapistatus.ConditionType = "Active"
)

// clusterQueueStatusPaths makes the following decision.
//
//	|  Condition Type  |     Condition Status    | Human Readable Status | Human Sensible Status |
//	| ---------------- | ----------------------- | --------------------- | --------------------- |
//	| Active           | Unknown                 | Activating            | Transitioning         |
//	| Active           | False                   | Inactive              | Interrupted           |
//	| Active           | True                    | Active                | /                     |
var clusterQueueStatusPaths = kubeapistatus.NewSummarizer(
	[][]kubeapistatus.ConditionType{
		{
			ClusterQueueConditionActive,
		},
	},
)

// GetSummaryOfClusterQueue derives the human status from the queue's StopPolicy first,
// falling back to the condition-based summary when the queue is not held.
//
// A HoldAndDrain queue reads as Draining while it still holds reserved/admitted workloads and
// Inactive once fully drained; a Hold queue reads as Inactive directly.
func GetSummaryOfClusterQueue(cq *kueue.ClusterQueue) (phase, phaseMessage string) {
	switch ptr.Deref(cq.Spec.StopPolicy, kueue.None) {
	case kueue.HoldAndDrain:
		if clusterQueueHasReserved(cq) {
			return "Draining", "cluster queue is draining admitted workloads"
		}
		return "Inactive", "cluster queue is held and drained"
	case kueue.Hold:
		return "Inactive", "cluster queue is held"
	}
	return clusterQueueStatusPaths.GetSummary(&cq.Status)
}

// clusterQueueHasReserved reports whether the queue still holds any reserved or admitted
// capacity. It mirrors hasReserved in the worker controllers package, reimplemented here to
// avoid an import cycle.
func clusterQueueHasReserved(cq *kueue.ClusterQueue) bool {
	if cq.Status.ReservingWorkloads != 0 || cq.Status.AdmittedWorkloads != 0 {
		return true
	}
	return slicex.Any(cq.Status.FlavorsReservation, func(i int) bool {
		return slicex.Any(cq.Status.FlavorsReservation[i].Resources, func(j int) bool {
			return !cq.Status.FlavorsReservation[i].Resources[j].Total.IsZero() ||
				!cq.Status.FlavorsReservation[i].Resources[j].Borrowed.IsZero()
		})
	})
}
