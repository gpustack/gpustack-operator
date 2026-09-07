package device

import (
	"context"
	"strings"
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
	// Interface represents one of the worker's network interfaces, including its bus
	// coordinates and its RDMA link state.
	Interface = workercore.DeviceInterface
	// Ethernet represents the Ethernet information of a device,
	// including its name and PCI information.
	Ethernet = workercore.DeviceEthernet
	// Fabric represents the scale-up interconnect domain a device belongs to, including the
	// domain's identity and this device's own addresses on it.
	Fabric = workercore.DeviceFabric
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

		// CoresPercent is the partition's own compute utilization, as its handle reports it.
		//
		// Nil on every manufacturer whose library serves no per-partition compute figure, which is
		// most of them: NVML answers neither a per-process nor a per-instance one on a partitioned
		// card. Hygon's does, on the partition's own handle -- measured at 95% on an instance running
		// a matmul loop while its three idle siblings on the same card read 0 -- so the field exists
		// rather than the absence being assumed universal.
		CoresPercent *uint32

		// MemoryReason is why the two memory figures are nil, empty when they were read.
		MemoryReason AcceleratorProcessReason
		// CoresReason is why CoresPercent is nil, empty when it was read.
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
	//
	// Both of its passes report an absent driver, a library that will not initialize and a bus
	// holding no card as an EMPTY list and no error: that is a measurement, and it says this
	// manufacturer has no accelerators here. An error says the opposite — the pass could not measure,
	// so it carries no claim about the hardware at all, and a caller that reads it as an empty result
	// declares accelerators gone on no evidence. Today the only thing that produces one is the panic
	// guard each implementation defers.
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

// Structures for the allocation-time preconditions an allocator reads before it hands a device out.
type (
	// PreflightState is what one answer concluded about one subject — an accelerator's
	// precondition, or a manufacturer's detect pass. The three values are exhaustive and mutually
	// exclusive, and each carries a different consequence for the allocation the answer guards.
	PreflightState string

	// PreflightDepth is how far an answer was actually taken, and is what keeps an assumption from
	// being read as evidence. The three values are ordered: nothing may carry a deeper one than it
	// earned.
	PreflightDepth string

	// PreflightDetection is what a manufacturer's detect pass concluded, reported as an answer in
	// its own right rather than only as the input to the reads that follow.
	//
	// Detecting nothing and failing to look are different facts with different remedies, and
	// collapsing them sends an operator to debug hardware that is present or absent for reasons the
	// pass never established.
	PreflightDetection struct {
		// State is what the detect pass concluded: ok when accelerators were detected,
		// not-declared when none of this manufacturer's are on the host, and unavailable when the
		// pass itself could not measure — which says nothing about the hardware either way.
		State PreflightState `json:"state" yaml:"state"`
		// Depth is how far this answer was taken.
		Depth PreflightDepth `json:"depth" yaml:"depth"`
		// Accelerators is how many the detect pass found, across every group of this manufacturer.
		Accelerators int `json:"accelerators" yaml:"accelerators"`
		// Detail is what was detected, in the detector's own words.
		Detail string `json:"detail,omitempty" yaml:"detail,omitempty"`
		// Reason is why nothing was detected, or why the pass could not measure. It is empty
		// exactly when State is PreflightStateOK.
		Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
		// Host is what the host's own vendor CLI saw, set only where one could be asked. It is
		// what separates a machine with no accelerators from a machine with eight that this
		// container cannot see, which is the single most common bring-up mistake and the one the
		// detect pass cannot diagnose from inside.
		Host *PreflightHostView `json:"host,omitempty" yaml:"host,omitempty"`
	}

	// PreflightHostView is what the host's own vendor CLI reported, read by entering the mounted
	// host root — so it answers even when this container has no device mounts at all.
	PreflightHostView struct {
		// Command is what was run as the host, so a reader can run it themselves.
		Command string `json:"command" yaml:"command"`
		// Accelerators is how many the host's own CLI reported.
		Accelerators int `json:"accelerators" yaml:"accelerators"`
		// Detail is what that CLI answered, in its own words.
		Detail string `json:"detail,omitempty" yaml:"detail,omitempty"`
		// Reason is why the host could not be asked — no host root, no such CLI on the host, or a
		// CLI that failed. It is empty exactly when the host answered.
		Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
		// MissingMounts names what this container would need in order to see what the host sees,
		// and is set only when the host reported accelerators and the detect pass did not. That is
		// the whole point of asking: the remedy, not just the discrepancy.
		MissingMounts []string `json:"missingMounts,omitempty" yaml:"missingMounts,omitempty"`
	}

	// PreflightCheck is one allocation-time precondition, read on one accelerator.
	//
	// The failure is carried as a reason rather than flattened into the state, because the three
	// states say what kind of answer this was and only the reason says what the driver actually
	// replied — and an operator acting on the result needs both.
	PreflightCheck struct {
		// Accelerator is the ID of the accelerator the precondition was read on.
		Accelerator string `json:"accelerator" yaml:"accelerator"`
		// Capability names the precondition, in the manufacturer's own vocabulary.
		//
		// It is deliberately the vendor's word rather than a normalized one: an operator debugging
		// an Ascend node searches for "container-share", not for a name this package invented. What
		// makes two manufacturers' rows comparable is Mode, not this.
		Capability string `json:"capability" yaml:"capability"`
		// Mode is the allocation mode this precondition is a precondition *for*.
		//
		// It is what makes a report readable across manufacturers. Capability alone cannot be
		// compared — "cu-mask-topology" and "container-share" are different words for the same
		// question, "can this accelerator be logically sliced?" — and without Mode a reader cannot
		// tell which of them answer the same thing, nor which mode went unanswered on a node.
		//
		// A string rather than workercore.DeviceAllocationMode, which is a uint32 with no marshaller
		// of its own: putting the enum here would print "mode: 3" in a report meant to be read.
		// Setting it through PreflightModeOf is what keeps the two from drifting.
		Mode string `json:"mode" yaml:"mode"`
		// State is what the read concluded.
		State PreflightState `json:"state" yaml:"state"`
		// Depth is how far this answer was taken. A preflighter that leaves it unset means the
		// shallowest, which is the only depth a driver read on its own can reach.
		Depth PreflightDepth `json:"depth" yaml:"depth"`
		// Detail is what the driver answered, present when it answered at all. It is what
		// distinguishes a capability that is on from one that is merely readable.
		Detail string `json:"detail,omitempty" yaml:"detail,omitempty"`
		// Reason is why the capability could not be read, or why there is none to read. It is
		// empty exactly when State is PreflightStateOK.
		Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
		// Command is the container step this answer was reached by, printed exactly as it was run
		// or exactly as it would have been. It is set on an answer a container was involved in,
		// and is what lets a reader take the step themselves — which is the whole of the answer
		// when the step was emitted rather than run.
		Command string `json:"command,omitempty" yaml:"command,omitempty"`
		// Evidence is what the container printed, carried verbatim. A measured answer is only
		// worth what was observed, so the observation travels with the verdict rather than being
		// summarized into it.
		Evidence string `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	}

	// PreflightGroup is one manufacturer's answers for every accelerator it was asked about in one
	// pass.
	PreflightGroup struct {
		// Manufacturer is the name of the device manufacturer.
		Manufacturer string `json:"manufacturer" yaml:"manufacturer"`
		// Timestamp is the time when the preconditions were read. A preflight reports mutable host
		// state as it stands, so the reading is only worth what its time claims.
		Timestamp time.Time `json:"timestamp" yaml:"timestamp"`
		// Detection is what this manufacturer's detect pass concluded. It is the floor: every
		// answer below it is meaningless without it, so it is reported even for a manufacturer
		// nothing else is read for.
		Detection PreflightDetection `json:"detection" yaml:"detection"`
		// Checks holds one row per accelerator per precondition, in any order.
		Checks []PreflightCheck `json:"checks,omitempty" yaml:"checks,omitempty"`
		// Note says what the checks below cannot: why no driver precondition was read for this
		// manufacturer, or why the group carries no check at all. A manufacturer with nothing to
		// check must say so in words, because an empty list alone reads as a pass -- and one that
		// reads no driver but still produces simulated rows must say so too, because rows alone
		// read as a driver that answered.
		Note string `json:"note,omitempty" yaml:"note,omitempty"`
	}

	// PreflightGroupList represents a list of preflight groups.
	PreflightGroupList = []PreflightGroup

	// PreflighterOptions represents the options for configuring a preflighter.
	PreflighterOptions struct {
		Logger klog.Logger
		// DryRun withholds every action a preflighter would otherwise take, leaving it a pure read.
		//
		// It reaches the manufacturer rather than being handled by the caller because the caller
		// cannot know which reads are also actions: a capability that is off is asked on, to tell
		// "off" from "cannot be turned on", and only the manufacturer knows it does that. A
		// preflighter that skips the ask says so in the row's detail, so a dry run does not read as
		// a capability that was checked and found working.
		DryRun bool
		// HostRoot is where the host's own root filesystem is bind-mounted, and every host path a
		// preflighter reads must be joined onto it.
		//
		// The command runs in a container while the facts it reports are the host's, and only paths
		// the deployment happens to bind-mount at their own name -- /usr/local/Ascend, /usr/local/dcmi
		// -- are readable without it. Reading an unmounted path such as /etc without joining does not
		// fail: it silently reads the CONTAINER's copy, which for most of them means "absent", and a
		// check whose absent branch is its passing branch then reports ok on every node.
		HostRoot string
	}

	// AcceleratorPreflighter is an optional companion to Allocator, implemented by every
	// manufacturer that can answer either half of a preflight: a precondition read through a driver
	// seam, an injection produced without one being served, or both.
	//
	// A manufacturer with no driver seam still implements it, because the injection half needs no
	// seam -- its PreflightAccelerator returns no checks and a note saying why, and the runner adds
	// the simulated rows from its responder. What the interface being optional buys is the case
	// where neither half exists: that manufacturer does not implement it at all, which keeps
	// "nothing is checked here" a compile-time fact the caller reports in words, rather than a
	// method that has to answer a hopeful ok at runtime.
	AcceleratorPreflighter interface {
		// PreflightAccelerator reads each precondition its manufacturer's allocator would read at
		// allocation time, over the groups given — which are this manufacturer's and no others.
		//
		// It returns no error: every failure belongs to the accelerator it happened on and is
		// carried as that accelerator's state and reason, so one unreadable device never hides the
		// rest of the node.
		PreflightAccelerator(groups DevicesGroupList) PreflightGroup
	}
)

const (
	// PreflightStateOK means the capability was read and the accelerator can serve the mode it
	// guards. What the capability currently says is in the check's detail: a flag the allocator
	// turns on itself at allocation time is ok while it is still off.
	PreflightStateOK PreflightState = "ok"
	// PreflightStateUnavailable means the accelerator offers the capability and this pass did not
	// establish it: a driver that could not be asked, a container that ran and showed the capability
	// failing, or a container that could not be got far enough to show anything. The last is why
	// this is not "it does not work" -- a probe image whose client cannot start lands here too,
	// because a pass that waived what it could not observe would let a node through on an
	// assumption. This is the state an allocation is refused on, so the reason is the operator's
	// whole lead, and it is the reason rather than the state that says which of the three it was.
	PreflightStateUnavailable PreflightState = "unavailable"
	// PreflightStateNotDeclared means there is no such capability here to read or to set: an API
	// generation that declares none, or an accelerator whose driver disclaims it. Nothing can fix
	// it and nothing needs to -- the allocator proceeds without it.
	PreflightStateNotDeclared PreflightState = "not-declared"
)

const (
	// PreflightDepthDeclared means the driver was asked and answered. It is the shallowest depth
	// and the only one a read on its own can reach: it says what the host claims, not what
	// happened when something relied on the claim.
	PreflightDepthDeclared PreflightDepth = "declared"
	// PreflightDepthSimulated means the allocator's own code produced the artifact and the
	// artifact was asserted on, while nothing on the hardware changed. What it requires is that
	// second clause and nothing more: a manufacturer whose allocator reaches a driver to produce an
	// injection reaches it here too, so that seam is substituted for the pass, while one that
	// serves an allocation out of paths and a request touches nothing to begin with and reaches
	// this depth with no seam to substitute.
	PreflightDepthSimulated PreflightDepth = "simulated"
	// PreflightDepthMeasured means something ran and was observed. It is the only depth that
	// establishes a behavior rather than predicting it.
	PreflightDepthMeasured PreflightDepth = "measured"
)

// PreflightModeUnnamed is what a check that named no allocation mode carries. It is a value rather
// than an empty string so a reader sees the gap instead of a blank column, and so it can never be
// mistaken for a mode a manufacturer actually established.
const PreflightModeUnnamed = "unnamed"

// PreflightModeOf renders an allocation mode the way a preflight report names it: lower case, in the
// same register as the states and depths beside it.
//
// It exists so the report and the allocator cannot drift. workercore.DeviceAllocationMode is a
// uint32 whose String() is the single source of these names, and going through it means a mode
// renamed there is renamed here — where writing the strings by hand would leave a report naming a
// mode the allocator no longer has.
func PreflightModeOf(mode workercore.DeviceAllocationMode) string {
	if mode == workercore.DeviceAllocationModeNone {
		return PreflightModeUnnamed
	}
	return strings.ToLower(mode.String())
}
