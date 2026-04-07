package apistatus

import (
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
)

const (
	// ClusterConditionImported indicates the cluster is imported to the control plane,
	// which may be in the process of importing or is not imported.
	ClusterConditionImported kubeapistatus.ConditionType = "Importing"
	// ClusterConditionConnected indicates the cluster is connected to the control plane,
	// may be in the process of connecting, or is disconnected.
	ClusterConditionConnected kubeapistatus.ConditionType = "Connected"
	// ClusterConditionDeleting indicates the cluster is being deleted.
	ClusterConditionDeleting kubeapistatus.ConditionType = "Deleting"
)

// Reasons for ClusterConditionConnected.
const (
	// ClusterConditionImportedReasonWaitingForImport indicates the cluster is waiting to be imported to the control plane.
	ClusterConditionImportedReasonWaitingForImport = "WaitingForImport"

	// ClusterConditionImportedReasonApplyingConfig indicates the cluster is being imported to the control plane,
	// and the control plane is installing the worker components in the cluster.
	ClusterConditionImportedReasonApplyingConfig = "ApplyingConfig"

	// ClusterConditionConnectedReasonDisconnected indicates the cluster is disconnected from the control plane,
	// which may be caused by network issues or cluster failure.
	ClusterConditionConnectedReasonDisconnected = "Disconnected"
)

// clusterStatusPaths makes the following decision.
//
//	|  Condition Type  |     Condition Status    | Human Readable Status | Human Sensible Status |
//	| ---------------- | ----------------------- | --------------------- | --------------------- |
//	| Imported         | Unknown                 | Importing             | Transitioning         |
//	| Imported         | False                   | ImportedFailed        | Interrupted           |
//	| Imported         | True                    | Imported              | /                     |
//	| Connected        | Unknown                 | Connecting            | Transitioning         |
//	| Connected        | False                   | Disconnected          | Interrupted           |
//	| Connected        | True                    | Connected             | /                     |
//	| Deleting         | True                    | Deleting              | Transitioning         |
var clusterStatusPaths = kubeapistatus.NewSummarizer(
	[][]kubeapistatus.ConditionType{
		{
			ClusterConditionImported,
			ClusterConditionConnected,
		},
		{
			ClusterConditionDeleting,
		},
	},
)

// SummarizeCluster summarizes the given status by cluster flow,
// and applies the phase and phase message into the status.
func SummarizeCluster(clsStatus *servercore.ClusterStatus) {
	clusterStatusPaths.Summarize(clsStatus)
}
