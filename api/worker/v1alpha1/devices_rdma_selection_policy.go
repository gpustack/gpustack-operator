// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DevicesRDMASelectionPolicy is the cluster-scoped administrator filter restricting
// which RDMA HCAs are allocation candidates on the Nodes it selects.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["rdmasel"]
type DevicesRDMASelectionPolicy struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// +k8s:validation:cel[0]:rule="!(has(self.include) && has(self.exclude))"
	// +k8s:validation:cel[0]:message="include and exclude are mutually exclusive: specify at most one of them"
	Spec DevicesRDMASelectionPolicySpec `json:"spec" protobuf:"bytes,2,name=spec"`
}

var _ runtime.Object = (*DevicesRDMASelectionPolicy)(nil)

// DevicesRDMASelectionPolicySpec selects Nodes and the HCA include/exclude filters.
type DevicesRDMASelectionPolicySpec struct {
	// NodeSelector selects the Nodes this policy targets. A nil selector is invalid; an
	// empty selector explicitly matches every Node.
	// +required
	NodeSelector *meta.LabelSelector `json:"nodeSelector" protobuf:"bytes,1,name=nodeSelector"`

	// Include restricts candidates to these exact HCA names. Absence or null imposes no restriction.
	//
	// Include and Exclude are mutually exclusive: at most one of the two may be specified. The
	// name length is bounded here because the CRD registration cost budget prices an unbounded
	// string at its worst case; uniqueness stays with the webhook, which needs no budget.
	// +optional
	// +listType=atomic
	// +k8s:validation:minItems=1
	// +k8s:validation:maxItems=64
	// +k8s:validation:cel[0]:rule="self.all(x, size(x) > 0 && size(x) <= 64)"
	// +k8s:validation:cel[0]:message="each HCA name must be non-empty and at most 64 characters"
	Include []string `json:"include,omitempty" protobuf:"bytes,2,rep,name=include"`

	// Exclude removes these exact HCA names from the candidates. Absence or null excludes none.
	//
	// Include and Exclude are mutually exclusive: at most one of the two may be specified. The
	// name length is bounded here because the CRD registration cost budget prices an unbounded
	// string at its worst case; uniqueness stays with the webhook, which needs no budget.
	// +optional
	// +listType=atomic
	// +k8s:validation:minItems=1
	// +k8s:validation:maxItems=64
	// +k8s:validation:cel[0]:rule="self.all(x, size(x) > 0 && size(x) <= 64)"
	// +k8s:validation:cel[0]:message="each HCA name must be non-empty and at most 64 characters"
	Exclude []string `json:"exclude,omitempty" protobuf:"bytes,3,rep,name=exclude"`

	// Port restricts selected HCAs to this 1-based physical RDMA port.
	// When omitted, all active ports on each selected HCA are eligible.
	// +optional
	// +k8s:validation:minimum=1
	Port *int32 `json:"port,omitempty" protobuf:"varint,4,opt,name=port"`
}

// DevicesRDMASelectionPolicyList holds a list of DevicesRDMASelectionPolicy objects.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type DevicesRDMASelectionPolicyList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Items         []DevicesRDMASelectionPolicy `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*DevicesRDMASelectionPolicyList)(nil)
