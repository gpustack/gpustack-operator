package service

import (
	"k8s.io/apimachinery/pkg/api/resource"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// AggregatedInstanceTypeFlavor.
type (
	// AggregatedInstanceTypeFlavor represents an aggregated view of instance type flavors across multiple clusters.
	AggregatedInstanceTypeFlavor struct {
		Name string `json:"name"`

		Spec AggregatedInstanceTypeFlavorSpec `json:"spec"`
	}

	// AggregatedInstanceTypeFlavorSpec represents the specification of an AggregatedInstanceTypeFlavor.
	AggregatedInstanceTypeFlavorSpec struct {
		worker.InstanceTypeFlavorSpec `json:",inline"`

		// Clusters are the clusters that have this instance type flavor, e.g. "cluster-a", "cluster-b".
		Clusters []string `json:"clusters"`
	}

	// AggregatedInstanceTypeFlavorList represents a list of AggregatedInstanceTypeFlavor.
	AggregatedInstanceTypeFlavorList struct {
		Items []AggregatedInstanceTypeFlavor `json:"items"`
	}
)

// AggregatedInstanceType.
type (
	// AggregatedInstanceType represents an aggregated view of instance types across multiple clusters.
	AggregatedInstanceType struct {
		Name string `json:"name"`

		Spec   AggregatedInstanceTypeSpec   `json:"spec"`
		Status AggregatedInstanceTypeStatus `json:"status"`
	}

	// AggregatedInstanceTypeSpec represents the specification of an AggregatedInstanceType.
	AggregatedInstanceTypeSpec = workercore.InstanceTypeSpec

	// AggregatedInstanceTypeResource represents the resource of an AggregatedInstanceType.
	AggregatedInstanceTypeResource = workercore.InstanceTypeResource

	// AggregatedInstanceTypeStatus represents the status of an AggregatedInstanceType,
	// including resource availability and tier information.
	AggregatedInstanceTypeStatus struct {
		// OnceMaxRequest is the once max request overview resource of the AggregatedInstanceType.
		//
		// It is the resource bundle of the tier that wins on the primary dimension:
		// Accelerator when Spec.Acceleratable is true, otherwise CPU.
		// All four fields (Accelerator/AcceleratorShared/AcceleratorSliced/CPU) are taken from the same winning tier,
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

	// AggregatedInstanceTypeOverviewResource represents the overview resource of an AggregatedInstanceType,
	// including allocatable-as-exclusive, shareable, sliceable accelerator resources and CPU resources.
	AggregatedInstanceTypeOverviewResource struct {
		// Accelerator is the allocatable-as-exclusive accelerator resource of the AggregatedInstanceType, e.g. "1", "4".
		Accelerator resource.Quantity `json:"accelerator"`

		// AcceleratorShared is the shareable accelerator resource of the AggregatedInstanceType, e.g. "10", "40".
		AcceleratorShared resource.Quantity `json:"acceleratorShared"`

		// AcceleratorSliced is the sliceable accelerator resource of the AggregatedInstanceType, e.g. "100", "400".
		AcceleratorSliced resource.Quantity `json:"acceleratorSliced"`

		// CPU is the CPU remaining resource of the AggregatedInstanceType, e.g. "4", "8".
		CPU resource.Quantity `json:"cpu"`
	}

	// AggregatedInstanceTypeOnceMaxRequestTier represents a tier of once max request candidates of the AggregatedInstanceType.
	AggregatedInstanceTypeOnceMaxRequestTier struct {
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
		// but may differ on AcceleratorShared/AcceleratorSliced/CPU.
		Candidates []AggregatedInstanceTypeOnceMaxRequestCandidate `json:"candidates"`
	}

	// AggregatedInstanceTypeOnceMaxRequestCandidate represents a candidate of the max request tier of the AggregatedInstanceType.
	AggregatedInstanceTypeOnceMaxRequestCandidate struct {
		// Cluster is the candidate belongs to, e.g. "cluster-a", "cluster-b".
		Cluster string `json:"cluster"`

		// Name is the instance type name of the candidate, e.g. "nvidia-a100-40g", "nvidia-v100-32g".
		Name string `json:"name"`

		// Accelerator is the allocatable-as-exclusive accelerator resource of the candidate, e.g. "1", "4".
		Accelerator AggregatedInstanceTypeResource `json:"accelerator"`

		// AcceleratorShared is the shareable accelerator resource of the candidate, e.g. "10", "40".
		AcceleratorShared AggregatedInstanceTypeResource `json:"acceleratorShared"`

		// AcceleratorSliced is the sliceable accelerator resource of the candidate, e.g. "100", "400".
		AcceleratorSliced AggregatedInstanceTypeResource `json:"acceleratorSliced"`

		// CPU is the CPU once max request resource of the candidate, e.g. "4", "8".
		CPU AggregatedInstanceTypeResource `json:"cpu"`
	}

	// AggregatedInstanceTypeList represents a list of AggregatedInstanceType.
	AggregatedInstanceTypeList struct {
		Items []AggregatedInstanceType `json:"items,omitempty"`
	}
)

// ClusterInstanceTypeFlavor.
type (
	// ClusterInstanceTypeFlavor represents an instance type flavor in a specific cluster.
	ClusterInstanceTypeFlavor struct {
		worker.InstanceTypeFlavor

		Cluster string `json:"cluster"`
	}

	// ClusterInstanceTypeFlavorList represents a list of ClusterInstanceTypeFlavor.
	ClusterInstanceTypeFlavorList struct {
		Items []ClusterInstanceTypeFlavor `json:"items"`
	}
)

// ClusterInstanceType.
type (
	// ClusterInstanceType represents an instance type in a specific cluster.
	ClusterInstanceType struct {
		worker.InstanceType

		Cluster string `json:"cluster"`
	}

	// ClusterInstanceTypeList represents a list of ClusterInstanceType.
	ClusterInstanceTypeList struct {
		Items []ClusterInstanceType `json:"items"`
	}
)

// ClusterInstance.
type (
	// ClusterInstance represents an instance in a specific cluster.
	ClusterInstance struct {
		worker.Instance

		Cluster string `json:"cluster"`
	}

	// ClusterInstanceList represents a list of ClusterInstance.
	ClusterInstanceList struct {
		Items []ClusterInstance `json:"items"`
	}
)

// ClusterInstancePersistentVolumeType.
type (
	// ClusterInstancePersistentVolumeType represents a persistent volume type in a specific cluster.
	ClusterInstancePersistentVolumeType struct {
		worker.InstancePersistentVolumeType

		Cluster string `json:"cluster"`
	}

	// ClusterInstancePersistentVolumeTypeList represents a list of ClusterInstancePersistentVolumeType.
	ClusterInstancePersistentVolumeTypeList struct {
		Items []ClusterInstancePersistentVolumeType `json:"items"`
	}
)

// ClusterInstancePersistentVolume.
type (
	// ClusterInstancePersistentVolume represents a persistent volume in a specific cluster.
	ClusterInstancePersistentVolume struct {
		worker.InstancePersistentVolume

		Cluster string `json:"cluster"`
	}

	// ClusterInstancePersistentVolumeList represents a list of ClusterInstancePersistentVolume.
	ClusterInstancePersistentVolumeList struct {
		Items []ClusterInstancePersistentVolume `json:"items"`
	}
)

// ClusterInstanceImagePullSecret.
type (
	// ClusterInstanceImagePullSecret represents an image pull secret in a specific cluster.
	ClusterInstanceImagePullSecret struct {
		worker.InstanceImagePullSecret

		Cluster string `json:"cluster"`
	}
	// ClusterInstanceImagePullSecretList represents a list of ClusterInstanceImagePullSecret.
	ClusterInstanceImagePullSecretList struct {
		Items []ClusterInstanceImagePullSecret `json:"items"`
	}
)

// ClusterInstanceSSHPublicKey.
type (
	// ClusterInstanceSSHPublicKey represents an SSH public key in a specific cluster.
	ClusterInstanceSSHPublicKey struct {
		worker.InstanceSSHPublicKey

		Cluster string `json:"cluster"`
	}

	// ClusterInstanceSSHPublicKeyList represents a list of ClusterInstanceSSHPublicKey.
	ClusterInstanceSSHPublicKeyList struct {
		Items []ClusterInstanceSSHPublicKey `json:"items"`
	}
)
