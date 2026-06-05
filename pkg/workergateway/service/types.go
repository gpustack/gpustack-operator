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
	// OnceMaxRequest is the once max request overview resource of the AggregatedInstanceType.
	//
	// It is the resource bundle of the tier that wins on the primary dimension:
	// Accelerator when Spec.Acceleratable is true, otherwise CPU.
	// All four fields (Accelerator/CPU/RAM/LocalStorage) are taken from the same winning tier,
	// so the overview always represents a bundle achievable by some real candidate,
	// not a per-dimension maximum across tiers.
	OnceMaxRequest AggregatedInstanceTypeOverviewResource `json:"onceMaxRequest"`

	// Remaining is the total remaining requestable resources of the AggregatedInstanceType.
	//
	// Each dimension is the sum of the corresponding dimension across all tiers (i.e. all
	// candidates), giving a fleet-wide view of how much capacity is still requestable.
	// Unlike OnceMaxRequest, which is a single-allocation bundle, Remaining is an aggregate
	// total and may not be achievable in one allocation.
	Remaining AggregatedInstanceTypeOverviewResource `json:"remaining"`

	// Tiers is the list of once max request tiers of the AggregatedInstanceType, grouped by accelerator OnceMaxRequest.
	//
	// When Spec.Acceleratable is true, each tier holds candidates sharing the same accelerator OnceMaxRequest value.
	// When Spec.Acceleratable is false, all candidates collapse into a single tier (accelerator is always zero);
	// in that case, the per-candidate CPU OnceMaxRequest is the primary dimension within the tier.
	//
	// Each tier represents a combination of AggregatedInstanceTypeOnceMaxRequestCandidate that satisfy the once max request of the tier.
	Tiers []AggregatedInstanceTypeOnceMaxRequestTier `json:"tiers"`
}

type AggregatedInstanceTypeOverviewResource struct {
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
	// OnceMaxRequest is the resource bundle of the candidate that wins on the primary dimension within this tier.
	//
	// The primary dimension is Accelerator when the owning AggregatedInstanceType is acceleratable, otherwise CPU.
	// All four fields are taken from the same winning candidate so the bundle is achievable, not synthesized
	// from per-dimension maxes across candidates.
	OnceMaxRequest AggregatedInstanceTypeOverviewResource `json:"onceMaxRequest"`

	// Remaining is the total remaining requestable resources of the tier.
	//
	// Each dimension is the sum of the corresponding dimension across all candidates in the tier.
	// Unlike OnceMaxRequest, which is a single-allocation bundle, Remaining is an aggregate total
	// and may not be achievable in one allocation.
	Remaining AggregatedInstanceTypeOverviewResource `json:"remaining"`

	// Candidates is the list of candidates of the tier.
	//
	// All candidates in the same tier share the same accelerator OnceMaxRequest,
	// but may differ on CPU/RAM/LocalStorage.
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
