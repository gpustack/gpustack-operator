// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstanceMetrics is the subresource of Instance for reading the current utilization,
// which provides one up-to-date sample of the underlying Kubernetes Pod's
// CPU/memory/storage usage and the allocated accelerators' metrics.
//
// The CPU/memory/storage figures are read in real time from the node kubelet at request
// time; the accelerator figures are at most one monitor period stale.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type InstanceMetrics struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Sample is the current utilization sample of the Instance.
	// Pointer fields inside are absent when the corresponding source is unavailable.
	Sample InstanceMetricsSample `json:"sample" protobuf:"bytes,2,opt,name=sample"`
}

var _ runtime.Object = (*InstanceMetrics)(nil)

// InstanceMetricsSample is a single utilization sampling point of an Instance.
type InstanceMetricsSample struct {
	// Timestamp indicates when the CPU/memory/storage figures were measured by the kubelet.
	Timestamp meta.Time `json:"timestamp" protobuf:"bytes,1,opt,name=timestamp"`

	// CPUUsageNanoCores is the CPU usage of the Instance's Pod in nanocores
	// (core-nanoseconds per second), averaged over the kubelet's sample window.
	CPUUsageNanoCores *uint64 `json:"cpuUsageNanoCores,omitempty" protobuf:"varint,2,opt,name=cpuUsageNanoCores"`

	// MemoryWorkingSetBytes is the working set memory usage of the Instance's Pod in bytes.
	MemoryWorkingSetBytes *uint64 `json:"memoryWorkingSetBytes,omitempty" protobuf:"varint,3,opt,name=memoryWorkingSetBytes"`

	// RootfsUsedBytes is the used bytes of the Instance containers' writable layers,
	// which accounts against the Instance's spec.resources.localStorage.
	RootfsUsedBytes *uint64 `json:"rootfsUsedBytes,omitempty" protobuf:"varint,4,opt,name=rootfsUsedBytes"`

	// EphemeralStorageUsedBytes is the total ephemeral storage usage of the Instance's Pod
	// in bytes, including containers' writable layers, logs and emptyDir-backed volumes.
	// Absent when the figures came from the metrics.k8s.io fallback, which carries no
	// storage metrics.
	EphemeralStorageUsedBytes *uint64 `json:"ephemeralStorageUsedBytes,omitempty" protobuf:"varint,5,opt,name=ephemeralStorageUsedBytes"`

	// Accelerators holds the metrics of the accelerator devices allocated to the Instance,
	// keyed by device ID; absent when the Instance has no allocated accelerator or the
	// device manager is unreachable.
	//
	// +listType=map
	// +listMapKey=id
	Accelerators []InstanceAcceleratorMetrics `json:"accelerators,omitempty" protobuf:"bytes,6,rep,name=accelerators"`
}

// InstanceAcceleratorMetrics is the metrics of one accelerator device
// allocated to an Instance.
type InstanceAcceleratorMetrics struct {
	// ID is the universally unique identifier of the accelerator device.
	ID string `json:"id" protobuf:"bytes,1,opt,name=id"`

	// MemoryBytes is the total memory of the accelerator in bytes.
	MemoryBytes *uint64 `json:"memoryBytes,omitempty" protobuf:"varint,2,opt,name=memoryBytes"`

	// MemoryUsageBytes is the used memory of the accelerator in bytes.
	MemoryUsageBytes *uint64 `json:"memoryUsageBytes,omitempty" protobuf:"varint,3,opt,name=memoryUsageBytes"`

	// MemoryUtilizationPercent is the memory utilization of the accelerator in [0, 100].
	MemoryUtilizationPercent *uint32 `json:"memoryUtilizationPercent,omitempty" protobuf:"varint,4,opt,name=memoryUtilizationPercent"`

	// CoresUtilizationPercent is the cores utilization of the accelerator in [0, 100].
	CoresUtilizationPercent *uint32 `json:"coresUtilizationPercent,omitempty" protobuf:"varint,5,opt,name=coresUtilizationPercent"`

	// TemperatureCelsius is the temperature of the accelerator in Celsius.
	TemperatureCelsius *uint32 `json:"temperatureCelsius,omitempty" protobuf:"varint,6,opt,name=temperatureCelsius"`

	// PowerUsageWatts is the power usage of the accelerator in Watts.
	PowerUsageWatts *uint32 `json:"powerUsageWatts,omitempty" protobuf:"varint,7,opt,name=powerUsageWatts"`

	// Unhealthy indicates whether the accelerator is unhealthy.
	Unhealthy *bool `json:"unhealthy,omitempty" protobuf:"varint,8,opt,name=unhealthy"`
}
