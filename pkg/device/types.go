package device

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// Structures for device metadata.
type (
	// Topology represents the topology information of a device,
	// including its NUMA node and PCI information.
	Topology = workercore.DeviceTopology
	// Ethernet represents the Ethernet information of a device,
	// including its name and PCI information.
	Ethernet = workercore.DeviceEthernet
	// AcceleratorPhysicalSlicedProfile represents one hardware partition profile of a
	// device model, such as an NVIDIA MIG profile.
	AcceleratorPhysicalSlicedProfile = workercore.AcceleratorPhysicalSlicedProfile
	// AcceleratorLogicalSliced represents an accelerator's logical (software) slicing capability.
	AcceleratorLogicalSliced = workercore.AcceleratorLogicalSliced
	// AcceleratorPhysicalSliced represents an accelerator's physical (hardware) slicing capability.
	AcceleratorPhysicalSliced = workercore.AcceleratorPhysicalSliced
	// AcceleratorSlicedDetail represents the group-level aggregated slicing capability.
	AcceleratorSlicedDetail = workercore.AcceleratorSlicedDetail
	// AcceleratorSlicedLogicalDetail represents the group's aggregated logical slicing capability.
	AcceleratorSlicedLogicalDetail = workercore.AcceleratorSlicedLogicalDetail
	// AcceleratorSlicedPhysicalDetail represents the group's aggregated physical slicing capability.
	AcceleratorSlicedPhysicalDetail = workercore.AcceleratorSlicedPhysicalDetail
	// AcceleratorSlicedPhysicalDetailProfile represents one group-aggregated physical profile.
	AcceleratorSlicedPhysicalDetailProfile = workercore.AcceleratorSlicedPhysicalDetailProfile
	// AcceleratorProfileCount pairs a physical-slice profile name with an instance count
	// (allocated or free), as carried in an accelerator's per-accelerator MIG ledger.
	AcceleratorProfileCount = workercore.AcceleratorProfileCount
	// AcceleratorPlacement is one contiguous run an allocation occupies on an accelerator, in
	// the unit the field carrying it counts.
	AcceleratorPlacement = workercore.AcceleratorPlacement
	// AcceleratorAllocation is an accelerator's runtime allocation row — its mode, credit budget
	// and, for a hardware-partitioned accelerator, its per-profile partition ledger.
	AcceleratorAllocation = workercore.AcceleratorAllocation
	// AcceleratorStatus represents the status of the accelerator device,
	// including its health status and other status information.
	AcceleratorStatus = workercore.AcceleratorStatus
	// Accelerator represents the metadata of the accelerator device,
	// including its name, topology, features, and status.
	Accelerator = workercore.Accelerator
	// DevicesGroup represents a group of devices that have the same name and memory size.
	DevicesGroup = workercore.DevicesGroup
	// DevicesGroupList represents a list of device groups.
	DevicesGroupList = []DevicesGroup
)

// Structures for device metrics.
type (
	// AcceleratorMetrics represents the metrics of the accelerator device,
	// including its memory usage, core usage, temperature, and power consumption.
	AcceleratorMetrics struct {
		// ID is the universally unique identifier for this device.
		ID string `json:"id" yaml:"id"`
		// Memory is the memory size of the device in MiB.
		Memory uint64 `json:"memory" yaml:"memory"`
		// MemoryUsage is the memory used by the device in MiB.
		MemoryUsage uint64 `json:"memoryUsage" yaml:"memoryUsage"`
		// MemoryUtilization is the percentage of memory being used by the device, in the range of [0, 100].
		MemoryUtilization uint32 `json:"memoryUtilization" yaml:"memoryUtilization"`
		// CoresUtilization is the percentage of cores being used by the device, in the range of [0, 100].
		CoresUtilization uint32 `json:"coresUtilization" yaml:"coresUtilization"`
		// Temperature is the temperature of the device in Celsius.
		Temperature uint32 `json:"temperature" yaml:"temperature"`
		// PowerUsage is the power used by the device in Watts.
		PowerUsage uint32 `json:"powerUsage" yaml:"powerUsage"`
		// Unhealthy indicates whether the device is healthy or not.
		Unhealthy bool `json:"unhealthy" yaml:"unhealthy"`
	}
	// MetricsGroup represents a group of device metrics.
	MetricsGroup struct {
		// Manufacturer is the name of the device manufacturer.
		Manufacturer string `json:"manufacturer" yaml:"manufacturer"`
		// Timestamp is the time when the metrics were collected.
		Timestamp time.Time `json:"timestamp" yaml:"timestamp"`
		// Accelerators is the list of the accelerator device metrics in this group.
		Accelerators []AcceleratorMetrics `json:"accelerators" yaml:"accelerators"`
	}
	// MetricsGroupList represents a list of device metrics groups.
	MetricsGroupList = []MetricsGroup
)

// Structures for per-process device usage.
//
// These rows are producer-internal: they name host processes, so they are aggregated per Pod and
// container before anything is published and never travel on a wire themselves.
type (
	// AcceleratorProcessReason names why a per-process figure could not be produced. The values
	// are stable strings because they are published as a label beside the absence they explain,
	// and an absence without a named reason is indistinguishable from an idle device.
	AcceleratorProcessReason string

	// AcceleratorProcess is one process the manufacturer reports holding an accelerator.
	//
	// The figures are pointers because a manufacturer that cannot measure one of them must not be
	// read as measuring zero: the vendor libraries carry sentinels for exactly this, and a
	// sentinel taken as a number is the largest figure the field can hold.
	AcceleratorProcess struct {
		// PID is the process's HOST pid, as the vendor library reports it. A containerized
		// process is named by the pid the host sees, never by the pid it sees itself.
		PID uint32
		// MemoryBytes is the device memory the process holds, in bytes — the vendor's native
		// unit rather than MiB, so that a sum of sub-MiB rows is taken before any conversion
		// rounds it. Nil when the vendor reported a sentinel in place of a number.
		MemoryBytes *uint64
		// CoresPercent is the share of the whole accelerator's compute the process was last
		// measured using, in [0,100].
		//
		// NIL MEANS NOT MEASURABLE, exactly as it does for the memory above, and an adapter that
		// means "measured and idle" must say so with a pointer to zero. The distinction cannot be
		// left to the consumer: on one library a process missing from the sample list is idle,
		// because the library reports only non-zero samples; on another a row carries a sentinel
		// because that hardware revision cannot measure occupancy at all. Both arrive here as one
		// field, so the adapter is the only place that knows which of the two it is holding.
		CoresPercent *uint32
	}

	// AcceleratorProcesses is one accelerator's per-process rows, together with why an entry
	// point produced no rows at all.
	//
	// The two reasons are independent: a driver commonly serves process memory while refusing
	// process utilization, and collapsing them into one would hide half of what it does answer.
	// An empty Processes list with no reason set is a complete read of an idle device.
	AcceleratorProcesses struct {
		// ID is the universally unique identifier of the accelerator these rows belong to, the
		// same identifier AcceleratorMetrics carries.
		ID string
		// MemoryReason is why no row carries usable memory, empty when memory was read.
		MemoryReason AcceleratorProcessReason
		// CoresReason is why no row carries usable utilization, empty when it was read.
		CoresReason AcceleratorProcessReason
		// Processes is one row per process the device reports.
		Processes []AcceleratorProcess
	}

	// AcceleratorProcessesGroup is one manufacturer's per-process rows for every accelerator it
	// enumerated in one pass.
	AcceleratorProcessesGroup struct {
		// Manufacturer is the name of the device manufacturer.
		Manufacturer string
		// Timestamp is the time when the rows were collected.
		Timestamp time.Time
		// Accelerators is the list of per-accelerator rows in this group.
		Accelerators []AcceleratorProcesses
	}
)

const (
	// AcceleratorProcessReasonNone is the absence of a refusal: the entry point answered.
	AcceleratorProcessReasonNone AcceleratorProcessReason = ""
	// AcceleratorProcessReasonUnsupported means the driver does not serve this query for this
	// device. It is a property of the node, not of this sample.
	AcceleratorProcessReasonUnsupported AcceleratorProcessReason = "unsupported"
	// AcceleratorProcessReasonPermission means the query needs privileges the process does not
	// have. Like the reason above it will not clear on its own.
	AcceleratorProcessReasonPermission AcceleratorProcessReason = "permission"
	// AcceleratorProcessReasonDriverError means the query failed for a reason that may not
	// repeat, so support is not disproven by it and the next sample is worth taking.
	AcceleratorProcessReasonDriverError AcceleratorProcessReason = "transient_driver_error"
	// AcceleratorProcessReasonTruncated means the driver kept asking for a larger buffer than
	// the read would accept, so the row list could not be completed. A truncated list read as a
	// complete one turns processes that exist into processes that do not.
	AcceleratorProcessReasonTruncated AcceleratorProcessReason = "truncated"
)

// Structures for hardware-partition usage.
//
// A hardware partition is measured on the partition's OWN device handle rather than on the parent
// card's, and it is addressed by the identifier the allocation recorded rather than by which
// partition happens to hold a process of ours. That is what lets an idle partition report zero: a
// partition with no process is still a partition that can be named, while a process-first lookup
// would find nothing and have to publish an absence.
//
// So these carry no process ids at all, and the request carries no tenant identity either: which Pod
// and container holds a partition is the caller's join, made where the Pod list already is.
type (
	// AcceleratorPartitionRequest names one hardware partition to read, as the allocation recorded
	// it: the parent accelerator and the partition's own identifier, plus the profile and placement
	// an answer is echoed back with.
	AcceleratorPartitionRequest struct {
		// DeviceID is the universally unique identifier of the PARENT accelerator, which is what
		// groups the requests by the card each one is read from.
		DeviceID string
		// ID is the partition's own identifier as the allocation recorded it, and the ONLY thing a
		// partition is addressed by. Empty for an allocation made before the device plugin recorded
		// it, which is answered as an absence with a reason rather than derived: deriving means
		// translating the profile name below into a driver profile id, which costs a walk of the
		// manufacturer's whole profile catalog — 17 ids on one partitioning manufacturer and 85 on
		// the other — every card, every period, to recover what the allocator held for free.
		//
		// It is a way to FIND a partition, never proof of which one was granted. These identifiers
		// are name-based, derived from the parent and the instance's own identity, so destroying a
		// partition and creating another at the same placement returns the same one.
		ID string
		// Profile is the partition profile's name, e.g. "1g.10gb". Carried for the answer to echo
		// and for a log to name the grant, not to resolve anything.
		Profile string
		// Placements is the run(s) the partition occupies on the parent, in the manufacturer's own
		// slice units — one run, for every partitioning manufacturer today. Carried for the same
		// reason as Profile.
		Placements []AcceleratorPlacement
	}

	// AcceleratorPartition is one requested partition's answer. It echoes the request's three
	// identifying fields so a caller can match it back without depending on the order of the reply.
	//
	// Every request is answered, whether or not anything could be read from it: an answer carrying a
	// nil figure and a reason is what keeps a partition that could not be read from being read as
	// idle, which an omitted answer would become the moment the caller reports a measured device.
	AcceleratorPartition struct {
		// DeviceID, Profile and Placements are the request's own, echoed unchanged.
		DeviceID   string
		Profile    string
		Placements []AcceleratorPlacement

		// ID is the PARTITION's own universally unique identifier — a MIG UUID, not the parent card's.
		// It is what the partition is reported under, because the partition rather than the card is
		// what its holder was granted. Empty when the partition's handle could not be reached, which
		// is the same failure that leaves the two figures below nil.
		ID string

		// MemoryTotalBytes is the partition's OWN memory capacity, as its handle reports it, in the
		// vendor's native unit. It comes from the driver rather than from the profile name because the
		// name rounds: a "1g.10gb" partition carries 9856 MiB, not 10240.
		MemoryTotalBytes *uint64
		// MemoryUsedBytes is the device memory held on the partition's own handle, in the vendor's
		// native unit. Nil when the figure was not readable, for the reason below.
		MemoryUsedBytes *uint64

		// MemoryReason is why the two memory figures are nil, empty when they were read.
		MemoryReason AcceleratorProcessReason
		// CoresReason is why no compute utilization accompanies the partition. No manufacturer serves
		// a per-partition one today, so it is always populated.
		CoresReason AcceleratorProcessReason
	}

	// AcceleratorPartitionsGroup is one manufacturer's answers for every partition it was asked
	// about in one pass.
	AcceleratorPartitionsGroup struct {
		// Manufacturer is the name of the device manufacturer.
		Manufacturer string
		// Timestamp is the time when the partitions were read.
		Timestamp time.Time
		// Partitions holds one answer per request, in any order.
		Partitions []AcceleratorPartition
	}

	// AcceleratorPartitionDetector is an optional companion to Detector, implemented only by the
	// manufacturers whose library answers for a hardware partition's own handle. It is separate from
	// AcceleratorProcessDetector because the two ask different questions: that one asks which
	// processes hold a card, this one asks what one partition of it holds, and a manufacturer can
	// serve either without the other.
	AcceleratorPartitionDetector interface {
		// MonitorAcceleratorPartitions reads the partitions named by requests, addressing each one's
		// handle by the identifier its allocation recorded.
		//
		// The result carries an answer for each request and for no others. If noPciCheck is true,
		// the detector will skip PCI checks, exactly as the Detector methods do.
		MonitorAcceleratorPartitions(
			noPciCheck bool, requests []AcceleratorPartitionRequest,
		) (AcceleratorPartitionsGroup, error)
	}
)

type (
	// DetectorOptions represents the options for configuring the detector.
	DetectorOptions struct {
		Logger klog.Logger
	}

	// Detector is an interface for detecting devices on the system.
	Detector interface {
		// Name returns the name of the device type that this interface detects.
		Name() string

		// DetectAccelerator detects the accelerators on the system and returns a list of device groups.
		//
		// If noPciCheck is true, the detector will skip PCI checks when detecting accelerators.
		// This is useful for platforms where PCI information is not available or not reliable.
		DetectAccelerator(noPciCheck bool) (DevicesGroupList, error)

		// MonitorAccelerator monitors the accelerators on the system and returns a list of device metrics groups.
		//
		// If noPciCheck is true, the detector will skip PCI checks when monitoring accelerators.
		// This is useful for platforms where PCI information is not available or not reliable.
		MonitorAccelerator(noPciCheck bool) (MetricsGroupList, error)
	}

	// AcceleratorProcessDetector is an optional companion to Detector, implemented only by the
	// manufacturers whose library answers which processes hold a device and how much of it.
	//
	// It is deliberately not part of Detector. A detector that cannot answer simply does not
	// implement it, which keeps the nine implementations and every test fake unchanged, and keeps
	// "this manufacturer serves no per-process query" a compile-time fact rather than a method
	// that has to return an error at runtime.
	AcceleratorProcessDetector interface {
		// MonitorAcceleratorProcesses returns the per-process rows of the accelerators named by
		// deviceIDs, in the vendor's own units and with the vendor's own semantics preserved.
		//
		// Only those accelerators are queried, and the result carries an entry for each of them
		// and for no others. The caller names the devices because a per-process query only means
		// something on a device carrying a carved allocation: querying the rest would spend
		// vendor and /proc work on figures nobody can ask for, and would let a process on a card
		// nobody sliced make that card's figures unmeasurable for no one's benefit.
		//
		// If noPciCheck is true, the detector will skip PCI checks, exactly as the two Detector
		// methods do.
		MonitorAcceleratorProcesses(
			noPciCheck bool, deviceIDs sets.Set[string],
		) (AcceleratorProcessesGroup, error)
	}
)

type (
	// AllocatorOptions represents the options for configuring the allocator.
	AllocatorOptions struct {
		Logger        klog.Logger
		KubeSocket    string
		NoShared      bool
		NoSliced      bool
		NoPartitioned bool
	}

	// Allocator is an interface for allocating devices to containers.
	Allocator interface {
		// Name returns the name of the device type that this interface allocates.
		Name() string

		// Start starts the allocator and prepares it for allocation.
		//
		// The allocator should be ready to allocate devices after this method is called.
		Start(ctx context.Context) error

		// Stop stops the allocator and releases any resources it holds.
		// After this method is called, the allocator should not be used for allocation anymore.
		Stop()
	}
)
