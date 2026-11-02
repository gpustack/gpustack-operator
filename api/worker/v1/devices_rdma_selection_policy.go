// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// DevicesRDMASelectionPolicy restricts which RDMA HCAs are allocation candidates on the
// Nodes it selects.
//
// Configuration changes proxy to the v1alpha1 resource. The public API serves no status
// subresource. Main-resource writes preserve the backing resource's status.
//
// +genclient
// +genclient:noStatus
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:apireg-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["rdmasel"]
type DevicesRDMASelectionPolicy workercore.DevicesRDMASelectionPolicy

var _ runtime.Object = (*DevicesRDMASelectionPolicy)(nil)

// DevicesRDMASelectionPolicyList holds the list of DevicesRDMASelectionPolicies.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type DevicesRDMASelectionPolicyList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []DevicesRDMASelectionPolicy `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*DevicesRDMASelectionPolicyList)(nil)
