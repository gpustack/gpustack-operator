package apis

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
	// Accelerator is the accelerator resource of the AggregatedInstanceType, e.g. "1", "4".
	Accelerator AggregatedInstanceTypeResource `json:"accelerator"`

	// CPU is the CPU resource of the AggregatedInstanceType, e.g. "4", "8".
	CPU AggregatedInstanceTypeResource `json:"cpu"`

	// RAM is the RAM resource of the AggregatedInstanceType, e.g. "40G", "16G".
	RAM AggregatedInstanceTypeResource `json:"ram"`

	// LocalStorage is the local storage resource of the AggregatedInstanceType, e.g. "100G", "500G".
	LocalStorage AggregatedInstanceTypeResource `json:"localStorage"`

	// Tiers is the list of once max request tiers of the AggregatedInstanceType.
	//
	// Each tier represents a combination of the AggregatedInstanceTypeOnceMaxRequestCandidate those satisfy the once max request of the tier.
	Tiers []AggregatedInstanceTypeOnceMaxRequestTier `json:"tiers"`
}

type AggregatedInstanceTypeOnceMaxRequestTier struct {
	// OnceMaxRequest is the accelerator once max request resource of the tier, e.g. "4", "8".
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

	// InstanceType is the instance type name of the candidate, e.g. "nvidia-a100-40g", "nvidia-v100-32g".
	InstanceType string `json:"instanceType"`

	// CPU is the CPU once max request resource of the candidate, e.g. "4", "8".
	CPU resource.Quantity `json:"cpu"`

	// RAM is the RAM once max request resource of the candidate, e.g. "40G", "16G".
	RAM resource.Quantity `json:"ram"`

	// LocalStorage is the local storage once max request resource of the candidate, e.g. "100G", "500G".
	LocalStorage resource.Quantity `json:"localStorage"`
}

type AggregatedInstanceTypeList struct {
	Items []AggregatedInstanceType `json:"items,omitempty"`
}
