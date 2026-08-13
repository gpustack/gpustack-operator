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
// time; the accelerator figures come from the device manager's latest snapshot and are
// dropped when older than a few monitor periods.
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
//
// Every figure is one half of a Total/Used pair reported in one unit, so that a consumer
// computes a utilization percentage from one sample without reading the Instance's spec:
// CPU in milli-cores, memory and storage in MiB. The sources measure in finer units — the
// kubelet in nanocores and bytes, the manufacturer's device libraries in MiB — and the
// coarser unit wins, because a percentage needs no more precision than that.
//
// A Total comes from the Instance's own declaration and is therefore always populated; a Used
// figure is a measurement, and is absent when its source is unavailable. Every conversion
// rounds up, so a measured usage below one unit reads as 1 and 0 means no usage at all.
type InstanceMetricsSample struct {
	// Timestamp indicates when the CPU/memory/storage figures were measured by the kubelet.
	Timestamp meta.Time `json:"timestamp" protobuf:"bytes,1,opt,name=timestamp"`

	// CPUTotalMilliCores is the CPU limit of the Instance's Pod in milli-cores.
	CPUTotalMilliCores uint64 `json:"cpuTotalMilliCores" protobuf:"varint,2,opt,name=cpuTotalMilliCores"`

	// CPUUsedMilliCores is the CPU usage of the Instance's Pod in milli-cores,
	// averaged over the kubelet's sample window.
	CPUUsedMilliCores *uint64 `json:"cpuUsedMilliCores,omitempty" protobuf:"varint,3,opt,name=cpuUsedMilliCores"`

	// MemoryTotalMiB is the memory limit of the Instance's Pod in MiB.
	MemoryTotalMiB uint64 `json:"memoryTotalMiB" protobuf:"varint,4,opt,name=memoryTotalMiB"`

	// MemoryUsedMiB is the working set memory usage of the Instance's Pod in MiB.
	MemoryUsedMiB *uint64 `json:"memoryUsedMiB,omitempty" protobuf:"varint,5,opt,name=memoryUsedMiB"`

	// StorageTotalMiB is the ephemeral storage limit of the Instance's Pod in MiB.
	StorageTotalMiB uint64 `json:"storageTotalMiB" protobuf:"varint,6,opt,name=storageTotalMiB"`

	// StorageUsedMiB is the ephemeral storage usage of the Instance's Pod in MiB.
	//
	// This is the pod-level aggregate the kubelet evicts on — the containers' writable
	// layers, their logs, and the local emptyDir volumes — so it is measured against the
	// same ceiling StorageTotalMiB reports. Absent when the figures came from the
	// metrics.k8s.io fallback, which carries no storage metrics.
	StorageUsedMiB *uint64 `json:"storageUsedMiB,omitempty" protobuf:"varint,7,opt,name=storageUsedMiB"`

	// Accelerators holds the metrics of the accelerator devices allocated to the Instance,
	// keyed by device ID; absent when the Instance has no allocated accelerator or the
	// device manager is unreachable.
	//
	// +listType=map
	// +listMapKey=id
	Accelerators []InstanceAcceleratorMetrics `json:"accelerators,omitempty" protobuf:"bytes,8,rep,name=accelerators"`
}

// InstanceAcceleratorMetrics is the metrics of one accelerator device
// allocated to an Instance.
//
// All figures come from the manufacturer's device libraries; a zero value may also
// mean the library could not read that metric at sampling time.
type InstanceAcceleratorMetrics struct {
	// ID is the universally unique identifier of the accelerator device.
	ID string `json:"id" protobuf:"bytes,1,opt,name=id"`

	// MemoryTotalMiB is the total memory of the accelerator in MiB.
	MemoryTotalMiB *uint64 `json:"memoryTotalMiB,omitempty" protobuf:"varint,2,opt,name=memoryTotalMiB"`

	// MemoryUsedMiB is the used memory of the accelerator in MiB.
	MemoryUsedMiB *uint64 `json:"memoryUsedMiB,omitempty" protobuf:"varint,3,opt,name=memoryUsedMiB"`

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
