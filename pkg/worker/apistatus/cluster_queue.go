package apistatus

import (
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/kubeapistatus"
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
//	| Active           | Unknown                 | Preparing             | Transitioning         |
//	| Active           | False                   | Inactive              | Interrupted           |
//	| Active           | True                    | Active                | /                     |
var clusterQueueStatusPaths = kubeapistatus.NewSummarizer(
	[][]kubeapistatus.ConditionType{
		{
			ClusterQueueConditionActive,
		},
	},
)

// GetSummaryOfClusterQueue summarizes the given status by cluster queue flow,
// and returns the phase and phase message.
func GetSummaryOfClusterQueue(clsQueueStatus *kueue.ClusterQueueStatus) (phase, phaseMessage string) {
	return clusterQueueStatusPaths.GetSummary(clsQueueStatus)
}
