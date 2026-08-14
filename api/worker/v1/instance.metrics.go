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

	// Accelerators holds the metrics of the accelerators allocated to the Instance, keyed by the
	// identifier of what it was granted — a device, or a hardware partition of one; absent when the
	// Instance has no allocated accelerator or the device manager is unreachable.
	//
	// +listType=map
	// +listMapKey=id
	Accelerators []InstanceAcceleratorMetrics `json:"accelerators,omitempty" protobuf:"bytes,8,rep,name=accelerators"`
}

// InstanceAcceleratorMetrics is one accelerator an Instance holds, reported in the Instance's OWN
// terms: what it was granted on that accelerator, and what it was measured using of the grant.
//
// EVERY MODE REPORTS THE SAME FIELDS WITH THE SAME MEANING. An Instance holding a whole device reads
// the device's figures because the device is the grant; one holding a carved share — a logical slice
// or a hardware partition — reads that share's quota and that share's usage. So a consumer asking
// "how much memory is this Instance using, and how close is it to its ceiling" reads the same two
// fields whatever the allocation did, and never has to know. Mode says how the grant was made, for a
// consumer that wants to group by it; nothing else changes shape with it.
//
// A Total comes from the allocation and is present whenever the allocation can state it. A Used
// figure is a measurement, and is absent — never zero — when nothing on this node could measure it:
// a manufacturer serving no per-process device query, a driver answering NOT_SUPPORTED, a process on
// the device that could not be attributed to its container. Absence means "not measurable here", and
// the Device Manager's capability gauge names which of those it was; zero means measured and idle.
// A carved share whose usage could not be measured therefore reports NO usage rather than the
// device's, whose figure counts every other tenant on the card.
type InstanceAcceleratorMetrics struct {
	// ID is the universally unique identifier of the accelerator the Instance holds.
	//
	// Under Partitioned this is the PARTITION's own identifier — a MIG UUID rather than the parent
	// card's — because the partition is what the Instance was granted and what every figure below
	// describes. It is also what keeps two partitions of one card, held by one Instance, from
	// collapsing into a single entry under the card's shared identifier.
	ID string `json:"id" protobuf:"bytes,1,opt,name=id"`

	// Mode is how this accelerator was granted to the Instance: the whole device, a logical slice of
	// it, or a hardware partition of it. It does not change what the figures below mean — it names
	// the mechanism behind them, so that a consumer can group or filter by it.
	//
	// It carries the mode's NAME — "Exclusive", "Sliced", "Partitioned" — rather than the enum's wire
	// value, because a name is the only form that says anything to the reader of a metric, and because
	// it is what this same field already reads as on the Device Manager's Prometheus surface. A
	// consumer must never have to translate between the two.
	Mode string `json:"mode" protobuf:"bytes,9,opt,name=mode"`

	// MemoryTotalMiB is the memory the Instance was granted on this accelerator, in MiB: the device's
	// own memory when it holds the device, the quota the allocation charged when it holds a logical
	// slice, and the partition's own memory when it holds a partition.
	//
	// A slice's quota is folded back from the units the allocation charged — the same memory-anchored
	// basis admission counted credits on — so this denominator can never disagree with what was
	// granted. Absent when nothing could state the grant: a partition whose geometry the driver would
	// not disclose, or a device whose capacity did not reach this sample.
	MemoryTotalMiB *uint64 `json:"memoryTotalMiB,omitempty" protobuf:"varint,2,opt,name=memoryTotalMiB"`

	// MemoryUsedMiB is the memory the Instance was measured holding of that grant, in MiB.
	//
	// It measures the hardware rather than any bookkeeping, so it MAY EXCEED MemoryTotalMiB and is not
	// clamped: clamping would present a leaking quota as a perfectly enforced one. Read an overshoot
	// as an anomaly to investigate rather than as proof — the quota's unit conversion floors, and
	// driver accounting overhead can produce one too.
	MemoryUsedMiB *uint64 `json:"memoryUsedMiB,omitempty" protobuf:"varint,3,opt,name=memoryUsedMiB"`

	// MemoryUtilizationPercent is MemoryUsedMiB over MemoryTotalMiB, in percent. It is stated rather
	// than left to the consumer so that the percentage and the pair it comes from can never disagree,
	// and it is absent whenever either of them is.
	MemoryUtilizationPercent *uint32 `json:"memoryUtilizationPercent,omitempty" protobuf:"varint,4,opt,name=memoryUtilizationPercent"`

	// CoresUtilizationPercent is how much of the Instance's OWN compute allowance it was measured
	// using, in percent — the device's utilization when it holds the device, and the measured share of
	// the card over the enforced cap when it holds a logical slice. A slice capped at a fifth of a
	// card and saturating that fifth reads 100, not 20.
	//
	// TWO CONSEQUENCES OF THAT DENOMINATOR, both deliberate. It MAY EXCEED 100, for the same reason
	// the memory pair may: a shim that lets a slice burst past its cap while the card is idle is
	// reporting a cap that is not being enforced, and clamping would hide exactly that. And it is
	// COARSE under a small cap — the manufacturers measure the card in whole percent, so a cap of 5%
	// can only ever yield multiples of 20 here.
	//
	// Absent for a hardware partition: no manufacturer serves a per-partition compute figure today.
	CoresUtilizationPercent *uint32 `json:"coresUtilizationPercent,omitempty" protobuf:"varint,5,opt,name=coresUtilizationPercent"` // nolint: lll

	// TemperatureCelsius is the temperature of the accelerator in Celsius.
	//
	// WHOLE-DEVICE, in every mode: a carved share has no temperature of its own, and the device's is
	// the only honest answer to what the Instance's hardware is doing thermally.
	TemperatureCelsius *uint32 `json:"temperatureCelsius,omitempty" protobuf:"varint,6,opt,name=temperatureCelsius"`

	// PowerUsageWatts is the power usage of the accelerator in Watts.
	// Whole-device in every mode, for the reason TemperatureCelsius is.
	PowerUsageWatts *uint32 `json:"powerUsageWatts,omitempty" protobuf:"varint,7,opt,name=powerUsageWatts"`

	// Unhealthy indicates whether the accelerator is unhealthy.
	// Whole-device in every mode: an unhealthy card carries every share of it down.
	Unhealthy *bool `json:"unhealthy,omitempty" protobuf:"varint,8,opt,name=unhealthy"`
}
