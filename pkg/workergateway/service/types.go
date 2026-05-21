package service

import (
	"k8s.io/apimachinery/pkg/api/resource"

	worker "gpustack.ai/gpustack/api/worker/v1"
)

type (
	AggregatedInstanceTypeSpec     = worker.InstanceTypeSpec
	AggregatedInstanceTypeResource = worker.InstanceTypeResource
)

type AggregatedInstanceType struct {
	Name string `json:"name"`

	Spec   AggregatedInstanceTypeSpec   `json:"spec"`
	Status AggregatedInstanceTypeStatus `json:"status"`
}

type AggregatedInstanceTypeStatus struct {
	// Remaining is the remaining resource of the AggregatedInstanceType.
	Remaining AggregatedInstanceTypeRemainingResource `json:"remaining"`

	// AcceleratorTiers is the list of once max request tiers of the AggregatedInstanceType in accelerator dimension.
	//
	// Each tier represents a combination of the AggregatedInstanceTypeOnceMaxRequestCandidate those satisfy the once max request of the tier.
	AcceleratorTiers []AggregatedInstanceTypeOnceMaxRequestTier `json:"acceleratorTiers"`
}

type AggregatedInstanceTypeRemainingResource struct {
	// Accelerator is the accelerator remaining resource of the AggregatedInstanceType, e.g. "1", "4".
	Accelerator resource.Quantity `json:"accelerator"`

	// CPU is the CPU remaining resource of the AggregatedInstanceType, e.g. "4", "8".
	CPU resource.Quantity `json:"cpu"`

	// RAM is the RAM remaining resource of the AggregatedInstanceType, e.g. "40G", "16G".
	RAM resource.Quantity `json:"ram"`

	// LocalStorage is the local storage remaining resource of the AggregatedInstanceType, e.g. "100G", "500G".
	LocalStorage resource.Quantity `json:"localStorage"`
}

type AggregatedInstanceTypeOnceMaxRequestTier struct {
	// OnceMaxRequest is the once max request accelerator of the tier, e.g. "4", "8".
	OnceMaxRequest resource.Quantity `json:"onceMaxRequest"`

	// Candidates is the list of candidates of the tier.
	//
	// All candidates in the same tier must satisfy the max request of the tier,
	// but may have different resource values.
	Candidates []AggregatedInstanceTypeOnceMaxRequestCandidate `json:"candidates"`
}

// AggregatedInstanceTypeOnceMaxRequestCandidate represents a candidate of the max request tier of the AggregatedInstanceType.
type AggregatedInstanceTypeOnceMaxRequestCandidate struct {
	// Cluster is the candidate belongs to, e.g. "cluster-a", "cluster-b".
	Cluster string `json:"cluster"`

	// Name is the instance type name of the candidate, e.g. "nvidia-a100-40g", "nvidia-v100-32g".
	Name string `json:"name"`

	// Accelerator is the accelerator resource of the AggregatedInstanceType, e.g. "1", "4".
	Accelerator AggregatedInstanceTypeResource `json:"accelerator"`

	// CPU is the CPU once max request resource of the candidate, e.g. "4", "8".
	CPU AggregatedInstanceTypeResource `json:"cpu"`

	// RAM is the RAM once max request resource of the candidate, e.g. "40G", "16G".
	RAM AggregatedInstanceTypeResource `json:"ram"`

	// LocalStorage is the local storage once max request resource of the candidate, e.g. "100G", "500G".
	LocalStorage AggregatedInstanceTypeResource `json:"localStorage"`
}

type AggregatedInstanceTypeList struct {
	Items []AggregatedInstanceType `json:"items,omitempty"`
}

type ClusterInstanceType struct {
	worker.InstanceType

	Cluster string `json:"cluster"`
}

type ClusterInstanceTypeList struct {
	Items []ClusterInstanceType `json:"items"`
}

type ClusterInstance struct {
	worker.Instance

	Cluster string `json:"cluster"`
}

type ClusterInstanceList struct {
	Items []ClusterInstance `json:"items"`
}

type ClusterInstancePersistentVolumeType struct {
	worker.InstancePersistentVolumeType

	Cluster string `json:"cluster"`
}

type ClusterInstancePersistentVolumeTypeList struct {
	Items []ClusterInstancePersistentVolumeType `json:"items"`
}

type ClusterInstancePersistentVolume struct {
	worker.InstancePersistentVolume

	Cluster string `json:"cluster"`
}

type ClusterInstancePersistentVolumeList struct {
	Items []ClusterInstancePersistentVolume `json:"items"`
}

type ClusterInstanceImagePullSecret struct {
	worker.InstanceImagePullSecret

	Cluster string `json:"cluster"`
}

type ClusterInstanceImagePullSecretList struct {
	Items []ClusterInstanceImagePullSecret `json:"items"`
}

type ClusterInstanceSSHPublicKey struct {
	worker.InstanceSSHPublicKey

	Cluster string `json:"cluster"`
}

type ClusterInstanceSSHPublicKeyList struct {
	Items []ClusterInstanceSSHPublicKey `json:"items"`
}
