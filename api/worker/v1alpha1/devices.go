package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Devices is the schema for worker.gpustack.ai.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",subResources=["status"]
type Devices struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   DevicesSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status DevicesStatus `json:"status,omitempty" protobuf:"bytes,3,name=status"`
}

var _ runtime.Object = (*Devices)(nil)

// DevicesSpec defines the desired state of Devices.
type DevicesSpec struct {
	// Groups is the list of device groups on the worker.
	//
	// +listType=map
	// +listMapKey=id
	// +listMapKey=manufacturer
	Groups []DevicesGroup `json:"groups" yaml:"groups" protobuf:"bytes,1,name=groups"`

	// Interfaces is the list of network interfaces on the worker.
	//
	// It hangs on the worker rather than on a device group because a network interface belongs to
	// the machine, not to a manufacturer's accelerators: correlating the two is the reader's job
	// and is done by comparing the bus coordinates both sides carry, never by storing a
	// cross-reference here.
	//
	// Absence covers two cases and does not separate them: a worker with no interfaces, which is
	// not a state real hardware reaches, and a worker whose enumeration has never succeeded. A pass
	// that fails leaves whatever was recorded before it in place rather than replacing it with an
	// empty list, so on a worker profiled even once the previous inventory is what remains here.
	// Only a first pass that fails leaves the field absent, and that failure is reported in the
	// device manager's log at Error rather than modeled here — a state that resolves on the next
	// pass does not earn API surface. A reader must not treat absence as "this worker has no
	// interfaces".
	//
	// Every kernel interface is recorded, EPHEMERAL VIRTUAL ONES INCLUDED — and for those the list
	// is not guaranteed current. A change confined to virtual interfaces carrying no RDMA device and
	// no link verdict does not itself trigger a re-read: every Pod that starts or stops adds or
	// removes a veth, and treating that as a hardware change rewrote this cluster-scoped object on
	// every Pod event, once per manufacturer. So such an interface's arrival or departure is
	// published when some other change reports, not when it happens. Anything carrying an RDMA
	// record is exempt and is always current. A consumer that needs an up-to-the-second veth list
	// must read the node, not this field.
	//
	// +listType=map
	// +listMapKey=name
	Interfaces []DeviceInterface `json:"interfaces,omitempty" yaml:"interfaces,omitempty" protobuf:"bytes,2,opt,name=interfaces"`
}

// DevicesStatus defines the observed state of Devices.
type DevicesStatus struct {
	// Groups is the list of device groups on the worker.
	//
	// +listType=map
	// +listMapKey=id
	// +listMapKey=manufacturer
	Groups []DevicesAllocationGroup `json:"groups,omitempty" yaml:"groups,omitempty" protobuf:"bytes,1,opt,name=groups"`
}

type (
	// DevicesGroup is a group of devices with the same metadata.
	DevicesGroup struct {
		// ID is the universally unique identifier for this device group.
		ID string `json:"id" yaml:"id" protobuf:"bytes,1,opt,name=id"`

		// Manufacturer is the name of the device manufacturer.
		Manufacturer string `json:"manufacturer" yaml:"manufacturer" protobuf:"bytes,2,name=manufacturer"`

		// Name is the name of the device product.
		Name string `json:"name" yaml:"name" protobuf:"bytes,3,name=name"`

		// Memory is the memory size of the device in MiB.
		Memory uint64 `json:"memory" yaml:"memory" protobuf:"bytes,4,name=memory"`

		// Cores is the number of cores of the device.
		Cores uint32 `json:"cores,omitempty" yaml:"cores,omitempty" protobuf:"bytes,5,opt,name=cores"`

		// DriverVersion is the version of the driver used by the device.
		DriverVersion string `json:"driverVersion,omitempty" yaml:"driverVersion,omitempty" protobuf:"bytes,6,opt,name=driverVersion"`

		// RuntimeVersion is the version of the runtime used by the device.
		RuntimeVersion string `json:"runtimeVersion,omitempty" yaml:"runtimeVersion,omitempty" protobuf:"bytes,7,opt,name=runtimeVersion"`

		// ComputeCapability is the compute capability of the device.
		ComputeCapability string `json:"computeCapability,omitempty" yaml:"computeCapability,omitempty" protobuf:"bytes,8,opt,name=computeCapability"`

		// Family is the family of the device.
		Family string `json:"family,omitempty" yaml:"family,omitempty" protobuf:"bytes,9,opt,name=family"`

		// Accelerators is the list of the accelerator devices in this group.
		//
		// +listType=map
		// +listMapKey=id
		Accelerators []Accelerator `json:"accelerators,omitempty" yaml:"accelerators,omitempty" protobuf:"bytes,10,opt,name=accelerators"`

		// AcceleratorSlicedDetail is the group's slicing capability, aggregated from its
		// accelerators' per-accelerator slicing status.
		AcceleratorSlicedDetail AcceleratorSlicedDetail `json:"acceleratorSlicedDetail,omitempty" yaml:"acceleratorSlicedDetail,omitempty" protobuf:"bytes,11,opt,name=acceleratorSlicedDetail"` // nolint: lll
	}

	// DeviceTopology describes the topology information of the device.
	DeviceTopology struct {
		// PciBusID is the PCI bus ID of the device.
		PciBusID string `json:"pciBusId" yaml:"pciBusId" protobuf:"bytes,1,name=pciBusId"`

		// PciRootID is the address of the OUTERMOST PCI BRIDGE above the device, or the device's own
		// address when no bridge sits above it.
		//
		// It is NOT the root complex's identifier, despite the name. Two devices sharing this value
		// reached it through one bridge subtree; they are not thereby behind the same switch, which
		// is the tighter fact PciSwitches below carries. For a device attached directly to the root
		// complex the value is the device itself, so equality there is an identity check rather
		// than a same-root-complex claim.
		PciRootID string `json:"pciRootId" yaml:"pciRootId" protobuf:"bytes,2,name=pciRootId"`

		// PciClass is the PCI class of the device.
		PciClass string `json:"pciClass" yaml:"pciClass" protobuf:"bytes,3,name=pciClass"`

		// NumaAffinity is the NUMA node that the device is attached to.
		NumaAffinity string `json:"numaAffinity" yaml:"numaAffinity" protobuf:"bytes,4,name=numaAffinity"`

		// CpuAffinity is the CPU cores that are close to the device.
		CpuAffinity string `json:"cpuAffinity" yaml:"cpuAffinity" protobuf:"bytes,5,name=cpuAffinity"`

		// RoCE is the RoCE (RDMA over Converged Ethernet) network information of the device.
		RoCE *DeviceEthernet `json:"roce,omitempty" yaml:"roce,omitempty" protobuf:"bytes,6,opt,name=roce"`

		// PciSwitches is the upstream PCI bridge/switch path of the device, innermost first.
		//
		// Two devices sharing the whole path sit behind the same switch, which is strictly tighter
		// proximity than sharing the outermost bridge. PciRootID above is the OUTERMOST BRIDGE and
		// not a switch: reporting equality there as switch-level proximity advertises closeness
		// nobody measured, so the two fields are never read as the same claim.
		//
		// Absent for a device with no PCI path at all, never an empty-but-present marker. Ordered
		// by construction, so two consecutive reads of unchanged hardware are byte-identical.
		//
		// +listType=atomic
		PciSwitches []string `json:"pciSwitches,omitempty" yaml:"pciSwitches,omitempty" protobuf:"bytes,7,rep,name=pciSwitches"`
	}

	// DeviceEthernet describes the Ethernet information of the device.
	DeviceEthernet struct {
		// IP is the IP address of the Ethernet interface of the device.
		IP string `json:"ip" yaml:"ip" protobuf:"bytes,1,name=ip"`

		// SubnetMask is the subnet mask of the Ethernet interface of the device.
		SubnetMask string `json:"subnetMask" yaml:"subnetMask" protobuf:"bytes,2,name=subnetMask"`

		// Gateway is the gateway of the Ethernet interface of the device.
		Gateway string `json:"gateway" yaml:"gateway" protobuf:"bytes,3,name=gateway"`
	}

	// DeviceInterface describes one network interface of the worker.
	//
	// Enumeration is interface-first: the interface is the identity and its PCI device is resolved
	// as an attribute, which is the inverse of walking the PCI bus and correlating back. An
	// interface reached over a non-PCI interconnect is invisible to a PCI-rooted walk, and that is
	// exactly the case this record must not lose — so every PCI field here is optional and their
	// joint absence is a KIND of interface, not a hole in the record.
	DeviceInterface struct {
		// Name is the kernel interface name, and is this interface's identity.
		Name string `json:"name" yaml:"name" protobuf:"bytes,1,name=name"`

		// Bus names the interconnect the interface was found on, so an interface with no PCI
		// coordinates reports what it is instead of reading as a failed PCI lookup.
		Bus string `json:"bus,omitempty" yaml:"bus,omitempty" protobuf:"bytes,2,opt,name=bus"`

		// PciBusID is the PCI bus ID of the interface. Absent for a non-PCI interface.
		PciBusID string `json:"pciBusId,omitempty" yaml:"pciBusId,omitempty" protobuf:"bytes,3,opt,name=pciBusId"`

		// PciRootID is the address of the OUTERMOST PCI BRIDGE above the interface, or the
		// interface's own address when no bridge sits above it. Despite the name it is not a root
		// complex, and reading it as one advertises closeness nobody measured. Absent for a
		// non-PCI interface. It comes from the same walk the accelerator side uses, so the two
		// values are comparable.
		PciRootID string `json:"pciRootId,omitempty" yaml:"pciRootId,omitempty" protobuf:"bytes,4,opt,name=pciRootId"`

		// PciSwitches is the upstream PCI bridge/switch path, innermost first — the same
		// coordinate DeviceTopology carries, so an accelerator and an interface can be compared
		// without a translation layer. Absent for a non-PCI interface.
		//
		// +listType=atomic
		PciSwitches []string `json:"pciSwitches,omitempty" yaml:"pciSwitches,omitempty" protobuf:"bytes,5,rep,name=pciSwitches"`

		// PciVendor and PciDevice are the raw hex PCI ids, deliberately not resolved to a model
		// name: resolving one reads a host data file a minimal image may not carry, and a name
		// that resolves on one worker but not another is worse than a hex id that always does.
		PciVendor string `json:"pciVendor,omitempty" yaml:"pciVendor,omitempty" protobuf:"bytes,6,opt,name=pciVendor"`

		// PciDevice is the raw hex PCI device id. See PciVendor.
		PciDevice string `json:"pciDevice,omitempty" yaml:"pciDevice,omitempty" protobuf:"bytes,7,opt,name=pciDevice"`

		// NumaAffinity is the NUMA node the interface is attached to. Empty means UNKNOWN and is
		// never normalised to node 0 — that would report an affinity nobody read.
		NumaAffinity string `json:"numaAffinity,omitempty" yaml:"numaAffinity,omitempty" protobuf:"bytes,8,opt,name=numaAffinity"`

		// CpuAffinity is the CPU cores close to the interface. Empty means unknown.
		CpuAffinity string `json:"cpuAffinity,omitempty" yaml:"cpuAffinity,omitempty" protobuf:"bytes,9,opt,name=cpuAffinity"`

		// MTU is the link MTU. Zero means it was not read, not an MTU of zero — no operational
		// interface reports zero, so the absent value carries no ambiguity.
		MTU int32 `json:"mtu,omitempty" yaml:"mtu,omitempty" protobuf:"varint,10,opt,name=mtu"`

		// Up reports the interface's operational state.
		Up bool `json:"up,omitempty" yaml:"up,omitempty" protobuf:"varint,11,opt,name=up"`

		// Virtual marks an interface with no device behind it (loopback, bridge, veth). Such an
		// interface is RECORDED AND MARKED, never dropped: a worker whose only interface is a
		// bridge must read as "one virtual interface", not as "no interfaces".
		Virtual bool `json:"virtual,omitempty" yaml:"virtual,omitempty" protobuf:"varint,12,opt,name=virtual"`

		// RDMA reports that an RDMA device is bound to this interface. It says nothing about
		// whether the link works — that is Link below, and the two differ on real hardware.
		RDMA bool `json:"rdma,omitempty" yaml:"rdma,omitempty" protobuf:"varint,13,opt,name=rdma"`

		// RDMADevice is the bound RDMA device's name. Empty when RDMA is false.
		RDMADevice string `json:"rdmaDevice,omitempty" yaml:"rdmaDevice,omitempty" protobuf:"bytes,14,opt,name=rdmaDevice"`

		// SRIOV reports that this interface is an SR-IOV physical function. It is a SEPARATE fact
		// from the length of VirtualFunctions: "a PF with zero VFs configured" and "not a PF at
		// all" are different states, and deriving the second from an empty VF list collapses them.
		SRIOV bool `json:"sriov,omitempty" yaml:"sriov,omitempty" protobuf:"varint,15,opt,name=sriov"`

		// VirtualFunctions are this physical function's virtual functions, NESTED here rather
		// than listed as siblings: a PF with eight VFs is one entry with eight nested, never nine
		// top-level entries. Ordered by bus id, so two consecutive reads are byte-identical.
		//
		// +listType=atomic
		VirtualFunctions []DeviceInterfaceVirtualFunction `json:"virtualFunctions,omitempty" yaml:"virtualFunctions,omitempty" protobuf:"bytes,16,rep,name=virtualFunctions"` // nolint: lll

		// Link is the result of verifying that this interface's RDMA link is usable. Nil when
		// there is no RDMA link to verify.
		Link *DeviceInterfaceLink `json:"link,omitempty" yaml:"link,omitempty" protobuf:"bytes,17,opt,name=link"`
	}

	// DeviceInterfaceVirtualFunction describes one SR-IOV virtual function of a physical function.
	//
	// It is a type of its own rather than a nested DeviceInterface because SR-IOV nests exactly
	// one level deep — a virtual function cannot itself be partitioned into virtual functions — so
	// a self-referential shape would carry a level nothing can ever fill.
	//
	// What a VF shares with its parent is the UPSTREAM BRIDGE PATH — `pciRootId` (the outermost
	// bridge, not a root complex) and `pciSwitches` — so those are read from the parent and not
	// repeated here. A VF is its own PCI function with its own address, so that sharing is a claim
	// about the path above both of them and nothing more. What can differ per VF is recorded:
	// its own address, its own NUMA node and CPU list, and its own RDMA device and link verdict.
	DeviceInterfaceVirtualFunction struct {
		// Name is the kernel interface name of the virtual function.
		Name string `json:"name" yaml:"name" protobuf:"bytes,1,name=name"`

		// PciBusID is the PCI bus ID of the virtual function, which is what distinguishes it from
		// its siblings and from its parent.
		PciBusID string `json:"pciBusId,omitempty" yaml:"pciBusId,omitempty" protobuf:"bytes,2,opt,name=pciBusId"`

		// NumaAffinity is the NUMA node the virtual function is attached to. Empty means unknown.
		NumaAffinity string `json:"numaAffinity,omitempty" yaml:"numaAffinity,omitempty" protobuf:"bytes,3,opt,name=numaAffinity"`

		// CpuAffinity is the CPU cores close to the virtual function. Empty means unknown.
		CpuAffinity string `json:"cpuAffinity,omitempty" yaml:"cpuAffinity,omitempty" protobuf:"bytes,4,opt,name=cpuAffinity"`

		// MTU is the link MTU. Zero means it was not read. See DeviceInterface.MTU.
		MTU int32 `json:"mtu,omitempty" yaml:"mtu,omitempty" protobuf:"varint,5,opt,name=mtu"`

		// Up reports the virtual function's operational state.
		Up bool `json:"up,omitempty" yaml:"up,omitempty" protobuf:"varint,6,opt,name=up"`

		// RDMA reports that an RDMA device is bound to this virtual function.
		RDMA bool `json:"rdma,omitempty" yaml:"rdma,omitempty" protobuf:"varint,7,opt,name=rdma"`

		// RDMADevice is the bound RDMA device's name. Empty when RDMA is false.
		RDMADevice string `json:"rdmaDevice,omitempty" yaml:"rdmaDevice,omitempty" protobuf:"bytes,8,opt,name=rdmaDevice"`

		// Link is the result of verifying this virtual function's RDMA link. Nil when there is no
		// RDMA link to verify.
		Link *DeviceInterfaceLink `json:"link,omitempty" yaml:"link,omitempty" protobuf:"bytes,9,opt,name=link"`
	}

	// DeviceInterfaceLink is the outcome of verifying that an RDMA-capable interface's link is
	// actually usable.
	//
	// Reporting and enforcement are separated by making the outcome an explicit state rather than
	// a boolean: only Failed withholds the node's RDMA label, because "we have no check for this
	// interface" must not be published as "this worker has no RDMA".
	DeviceInterfaceLink struct {
		// State is the verification outcome.
		State DeviceInterfaceLinkState `json:"state" yaml:"state" protobuf:"bytes,1,name=state,casttype=DeviceInterfaceLinkState"`

		// Reason carries the checker's own words, verbatim — including the attribute values it
		// read. A non-ok state without a reason leaves the operator's actual question ("why?")
		// unanswerable from the record alone.
		Reason string `json:"reason,omitempty" yaml:"reason,omitempty" protobuf:"bytes,2,opt,name=reason"`

		// FirstSeenTime is when an ONGOING FAILED state was first observed. It is stable across
		// passes for as long as the failure persists, and is cleared the moment the state is
		// anything else. Refreshing it every pass would make "how long has this been down?"
		// unanswerable, which is the question the field exists to answer.
		//
		// Nil for both other states, `unverified` included: a state that reached no verdict has no
		// outage for a clock to be the start of, so this is not "the current non-ok state" but
		// specifically the failed one.
		FirstSeenTime *meta.Time `json:"firstSeenTime,omitempty" yaml:"firstSeenTime,omitempty" protobuf:"bytes,3,opt,name=firstSeenTime"`
	}

	// DevicesAllocationGroup describes the allocated device group.
	DevicesAllocationGroup struct {
		// ID is the universally unique identifier for this device group.
		ID string `json:"id" yaml:"id" protobuf:"bytes,1,opt,name=id"`

		// Manufacturer is the name of the device manufacturer.
		Manufacturer string `json:"manufacturer" yaml:"manufacturer" protobuf:"bytes,2,name=manufacturer"`

		// Accelerators is the list of the allocated accelerator devices in this group.
		//
		// +listType=map
		// +listMapKey=id
		Accelerators []AcceleratorAllocation `json:"accelerators,omitempty" yaml:"accelerators,omitempty" protobuf:"bytes,3,opt,name=accelerators"`
	}
)

// DeviceInterfaceLinkState is the outcome of an interface's RDMA link verification.
//
// The three states exist because "the link is fine" and "nobody could ask" are different facts
// with different consequences, and collapsing them into a boolean forces one of them to be a lie.
// +enum
type DeviceInterfaceLinkState string

const (
	// DeviceInterfaceLinkStateOK indicates the link was checked and verified.
	DeviceInterfaceLinkStateOK DeviceInterfaceLinkState = "ok"
	// DeviceInterfaceLinkStateUnverified indicates the check reached no verdict: an attribute it
	// needs was unreadable, the port directory could not be listed, or the RDMA tree is present
	// and could not be read at all. There is no per-manufacturer checker to be missing — the
	// check reads the RDMA subsystem's own port attributes and dispatches on nothing.
	//
	// It is REPORTED, and it does not withhold the node's RDMA label: a link we cannot interrogate
	// must not silently exclude its worker from scheduling.
	DeviceInterfaceLinkStateUnverified DeviceInterfaceLinkState = "unverified"
	// DeviceInterfaceLinkStateFailed indicates a check ran and the link is not usable. This is
	// the only state that withholds the node's RDMA label, and it is never reached from a missing
	// or unreadable file — an unreadable attribute is Unverified.
	DeviceInterfaceLinkStateFailed DeviceInterfaceLinkState = "failed"
)

// DeviceAllocationMode describes the allocation mode of the accelerator device.
// +enum
type DeviceAllocationMode uint32

const (
	// DeviceAllocationModeNone indicates that the allocation mode of the device is unknown.
	DeviceAllocationModeNone DeviceAllocationMode = iota
	// DeviceAllocationModeExclusive indicates that the device is allocated exclusively to a single consumer.
	DeviceAllocationModeExclusive
	// DeviceAllocationModeShared indicates that the device is allocated to multiple consumers,
	// and the resources are shared among them.
	DeviceAllocationModeShared
	// DeviceAllocationModeSliced indicates that the device is allocated to multiple consumers,
	// and the resources are partitioned among them.
	DeviceAllocationModeSliced
	// DeviceAllocationModePartitioned indicates that the device is carved into hardware
	// partitions (e.g. NVIDIA MIG GPU instances), each allocated to one consumer. Unlike
	// Sliced, the isolation is enforced by the hardware, the accelerator must be put into a
	// partitioning mode first, and it can then serve no other mode.
	DeviceAllocationModePartitioned
	// DeviceAllocationModeVisibility is an internal-only mode: it grants a container
	// device-cgroup visibility to the physical device(s) another container in the same Pod
	// was allocated, without a real device selection or any resource accounting. It is never
	// advertised on an InstanceType and never written to Devices status.
	DeviceAllocationModeVisibility
)

func (in DeviceAllocationMode) String() string {
	switch in {
	case DeviceAllocationModeExclusive:
		return "Exclusive"
	case DeviceAllocationModeShared:
		return "Shared"
	case DeviceAllocationModeSliced:
		return "Sliced"
	case DeviceAllocationModePartitioned:
		return "Partitioned"
	case DeviceAllocationModeVisibility:
		return "Visibility"
	default:
		return "None"
	}
}

type (
	// Accelerator describes the information of an accelerator device.
	Accelerator struct {
		// ID is the universally unique identifier for this device.
		ID string `json:"id" yaml:"id" protobuf:"bytes,1,opt,name=id"`

		// Index is the logic number of the device, starting from 0.
		Index uint32 `json:"index" yaml:"index" protobuf:"varint,2,name=index"`

		// PhysicalIndexes is the device char list for the device, constructed by the manufacturer.
		PhysicalIndexes []uint32 `json:"physicalIndexes" yaml:"physicalIndexes" protobuf:"varint,3,rep,name=physicalIndexes"`

		// Topology is the topology information of the device.
		Topology DeviceTopology `json:"topology" yaml:"topology" protobuf:"bytes,4,name=topology"`

		// Status is the current status of the device.
		Status AcceleratorStatus `json:"status" yaml:"status" protobuf:"bytes,5,name=status"`
	}

	// AcceleratorPlacement is one contiguous run [Start, Start+Length) an allocation occupies on
	// an accelerator. It is one record carrier for both placement ledgers; the unit it counts
	// depends on the field carrying it:
	//
	//   - memory slices, for the physical-partition ledger — the interval a hardware GPU
	//     partition (e.g. an NVIDIA MIG GPU instance) occupies, which is what both the
	//     capability's empty-accelerator legal-slot cache and the annotation-transported
	//     occupied slot are expressed in.
	//   - the manufacturer's own compute units, for the logical-slice ledger — on AMD, CU-mask
	//     bit indexes exactly as they appear in HSA_CU_MASK. A logical slice occupies a POSITION
	//     on the accelerator, not only a share of it: two slices handed the same run share those
	//     compute units instead of the accelerator.
	AcceleratorPlacement struct {
		// Start is the first unit the run covers (0-based).
		Start int32 `json:"start" yaml:"start" protobuf:"varint,1,opt,name=start"`

		// Length is the number of units the run spans; the run is [Start, Start+Length).
		// Named Length, not Size, to avoid colliding with the protobuf-generated Size()
		// method on this message.
		Length int32 `json:"length" yaml:"length" protobuf:"varint,2,opt,name=length"`
	}

	// AcceleratorPhysicalSlicedProfile describes one hardware partition profile of a device
	// model, such as an NVIDIA MIG profile (e.g. "1g.5gb"). The compute/memory slice counts
	// express the request granularity on each axis, one dimension richer than a scalar step.
	AcceleratorPhysicalSlicedProfile struct {
		// Name is the profile identifier, e.g. "1g.5gb". It is the display name and the
		// future resource-key suffix for a physical-slice request.
		Name string `json:"name" yaml:"name" protobuf:"bytes,1,opt,name=name"`

		// MemoryMib is the memory of one instance of this profile, in MiB.
		MemoryMib int64 `json:"memoryMib" yaml:"memoryMib" protobuf:"varint,2,opt,name=memoryMib"`

		// ComputeSlices is the number of compute slices one instance occupies — the request
		// granularity on the compute axis (1..7 on current hardware).
		ComputeSlices int32 `json:"computeSlices" yaml:"computeSlices" protobuf:"varint,3,opt,name=computeSlices"` // nolint: lll

		// MemorySlices is the number of memory slices one instance occupies — the request
		// granularity on the memory axis (1..8 on current hardware).
		MemorySlices int32 `json:"memorySlices" yaml:"memorySlices" protobuf:"varint,4,opt,name=memorySlices"` // nolint: lll

		// Count is the maximum number of instances of this profile on a single accelerator.
		Count int32 `json:"count" yaml:"count" protobuf:"varint,5,opt,name=count"`

		// Placements is the profile's full empty-accelerator legal placement set (start:size in
		// memory-slice units), enumerated once at detect time. The reconciler subtracts the
		// occupied intervals it reconstructs from Pod annotations from this cached set to derive
		// the accelerator's RemainingProfiles, so no device query runs per reconcile. Caching the
		// full empty-accelerator set makes the subtraction correct regardless of whether the
		// manufacturer's possible-placements query is itself occupancy-aware. Empty for an
		// accelerator with no physical-slice profiles.
		//
		// +listType=atomic
		Placements []AcceleratorPlacement `json:"placements,omitempty" yaml:"placements,omitempty" protobuf:"bytes,6,rep,name=placements"` // nolint: lll
	}

	// AcceleratorLogicalSliced describes an accelerator's logical (software) slicing capability.
	AcceleratorLogicalSliced struct {
		// CoresPercentageOvercommit reports whether each slice may claim up to 100% of the
		// device compute (time-sharing / weighted sharing); false means compute is partitioned.
		CoresPercentageOvercommit bool `json:"coresPercentageOvercommit,omitempty" yaml:"coresPercentageOvercommit,omitempty" protobuf:"varint,1,opt,name=coresPercentageOvercommit"` // nolint: lll

		// Count is the maximum number of logical slices this accelerator can host. An accelerator
		// whose MIG mode is currently enabled is always 0, which excludes it from the logical
		// capacity keys; a pending-enable accelerator is not partitioned yet and still reports
		// its logical count.
		Count int32 `json:"count,omitempty" yaml:"count,omitempty" protobuf:"varint,2,opt,name=count"`
	}

	// AcceleratorPhysicalSliced describes an accelerator's physical (hardware) slicing capability.
	AcceleratorPhysicalSliced struct {
		// Profiles is empty when the accelerator does not support, or has not enabled, hard
		// slicing.
		//
		// +listType=map
		// +listMapKey=name
		Profiles []AcceleratorPhysicalSlicedProfile `json:"profiles,omitempty" yaml:"profiles,omitempty" protobuf:"bytes,1,rep,name=profiles"` // nolint: lll

		// Count is the accelerator's physical-slice ceiling — the largest Count across Profiles
		// (e.g. 7 on A100, from 7x 1g.5gb). It sizes the device-plugin's bare ".partitioned"
		// token pool for a partitioned accelerator, which is the family that serves it; a
		// partitioned accelerator offers no logical slicing and so leaves the ".sliced" pool
		// entirely. Zero when Profiles is empty.
		Count int32 `json:"count,omitempty" yaml:"count,omitempty" protobuf:"varint,2,opt,name=count"`
	}

	// AcceleratorStatus describes the observed state of the accelerator device.
	AcceleratorStatus struct {
		// Unhealthy indicates whether the device is healthy or not.
		Unhealthy bool `json:"unhealthy" yaml:"unhealthy" protobuf:"varint,1,name=unhealthy"`

		// LogicalSliced is the accelerator's logical (software) slicing capability.
		LogicalSliced AcceleratorLogicalSliced `json:"logicalSliced,omitempty" yaml:"logicalSliced,omitempty" protobuf:"bytes,2,opt,name=logicalSliced"` // nolint: lll

		// PhysicalSliced is the accelerator's physical (hardware) slicing capability.
		PhysicalSliced AcceleratorPhysicalSliced `json:"physicalSliced,omitempty" yaml:"physicalSliced,omitempty" protobuf:"bytes,3,opt,name=physicalSliced"` // nolint: lll
	}

	// AcceleratorSlicedLogicalDetail aggregates the group's logical slicing capability. The
	// per-accelerator LogicalSliced is what an accelerator-level decision reads; this group view
	// is what external queries read to learn whether the node accepts logical-slice requests at
	// all (Count > 0) and whether it permits compute overcommit.
	AcceleratorSlicedLogicalDetail struct {
		// CoresPercentageOvercommit is a per-model property (uniform within a group), taken from
		// any logically sliceable accelerator; false and meaningless when no accelerator is
		// logically sliceable.
		CoresPercentageOvercommit bool `json:"coresPercentageOvercommit,omitempty" yaml:"coresPercentageOvercommit,omitempty" protobuf:"varint,1,opt,name=coresPercentageOvercommit"` // nolint: lll

		// Count is the sum of per-accelerator LogicalSliced.Count across the group.
		Count int32 `json:"count,omitempty" yaml:"count,omitempty" protobuf:"varint,2,opt,name=count"`
	}

	// AcceleratorSlicedPhysicalDetailProfile aggregates one profile across the group's
	// accelerators.
	AcceleratorSlicedPhysicalDetailProfile struct {
		// Name is the profile identifier, e.g. "1g.5gb".
		Name string `json:"name" yaml:"name" protobuf:"bytes,1,opt,name=name"`

		// Count is the sum of per-accelerator Count for this profile name across the group.
		Count int32 `json:"count,omitempty" yaml:"count,omitempty" protobuf:"varint,2,opt,name=count"`

		// MemoryMib is the memory of one instance of this profile, in MiB. It is uniform
		// per profile name within a group, so it is carried through (not summed). It is the
		// VRAM-anchored input the Pod webhook folds into ".sliced.units" (MemoryMibToUnits)
		// for a MIG request, which is why the aggregate — reachable from the InstanceType
		// Detail, unlike per-accelerator Devices — must carry it. Optional in the schema (a real
		// profile always carries a non-zero value); the Pod webhook treats a not-yet-populated
		// detail as a retryable not-ready state rather than relying on schema-required presence.
		MemoryMib int64 `json:"memoryMib,omitempty" yaml:"memoryMib,omitempty" protobuf:"varint,3,opt,name=memoryMib"`
	}

	// AcceleratorSlicedPhysicalDetail aggregates the group's physical slicing capability.
	AcceleratorSlicedPhysicalDetail struct {
		// Profiles is the group's physical profiles, summed by name.
		//
		// +listType=map
		// +listMapKey=name
		Profiles []AcceleratorSlicedPhysicalDetailProfile `json:"profiles,omitempty" yaml:"profiles,omitempty" protobuf:"bytes,1,rep,name=profiles"` // nolint: lll

		// Count is the sum of per-accelerator PhysicalSliced.Count across the group.
		Count int32 `json:"count,omitempty" yaml:"count,omitempty" protobuf:"varint,2,opt,name=count"`
	}

	// AcceleratorSlicedDetail is the group-level slicing capability view, aggregated from
	// the group's per-accelerator slicing status.
	AcceleratorSlicedDetail struct {
		// Logical is the aggregated logical (software) slicing capability.
		Logical AcceleratorSlicedLogicalDetail `json:"logical,omitempty" yaml:"logical,omitempty" protobuf:"bytes,1,opt,name=logical"`

		// Physical is the aggregated physical (hardware) slicing capability.
		Physical AcceleratorSlicedPhysicalDetail `json:"physical,omitempty" yaml:"physical,omitempty" protobuf:"bytes,2,opt,name=physical"`
	}

	// AcceleratorProfileCount pairs a physical-slice profile name with a count of
	// instances — allocated (bound) or remaining (still buildable) per the field carrying
	// it. It is a status-only type (never a map key), so the profile ledger and the
	// capability inventory (AcceleratorSlicedPhysicalDetailProfile) stay independently
	// evolvable.
	AcceleratorProfileCount struct {
		// Name is the profile identifier, e.g. "1g.10gb".
		Name string `json:"name" yaml:"name" protobuf:"bytes,1,opt,name=name"`

		// Count is the number of instances of this profile.
		Count int32 `json:"count,omitempty" yaml:"count,omitempty" protobuf:"varint,2,opt,name=count"`
	}

	// AcceleratorAllocation describes the allocated accelerator device.
	AcceleratorAllocation struct {
		// ID is the universally unique identifier for this device.
		ID string `json:"id" yaml:"id" protobuf:"bytes,1,opt,name=id"`

		// Index is the logic number of the device, starting from 0.
		Index uint32 `json:"index" yaml:"index" protobuf:"varint,2,name=index"`

		// Mode is the allocation mode of the device.
		Mode DeviceAllocationMode `json:"mode" yaml:"mode" protobuf:"varint,3,name=mode,casttype=DeviceAllocationMode"`

		// Allocated is the allocated units of the device.
		Allocated int32 `json:"allocated,omitempty" yaml:"allocated,omitempty" protobuf:"varint,4,opt,name=allocated"`

		// Remaining is the remaining allocatable units of the device.
		Remaining int32 `json:"remaining,omitempty" yaml:"remaining,omitempty" protobuf:"varint,5,opt,name=remaining"`

		// AllocatedProfiles and RemainingProfiles are the per-accelerator physical-slice ledger
		// the AdmissionCheck reads — the aggregated OUTPUT the reconciler computes from the
		// per-Pod AllocatedPhysicalProfile/AllocatedPhysicalPlacements transport fields below
		// (unioning every Pod's occupied slots on this accelerator). Both are empty (omitted) for
		// an accelerator with no physical-slice profiles, so it serializes byte-identically to
		// before they existed.
		//
		// AllocatedProfiles lists, by profile name, how many instances are currently created
		// and bound on this accelerator (the count of the Pods' recorded placements).
		//
		// +listType=map
		// +listMapKey=name
		AllocatedProfiles []AcceleratorProfileCount `json:"allocatedProfiles,omitempty" yaml:"allocatedProfiles,omitempty" protobuf:"bytes,6,rep,name=allocatedProfiles"` // nolint: lll

		// RemainingProfiles lists, by profile name, how many more instances of each profile can
		// still be created given the accelerator's occupied placement slots — the placement-aware
		// remaining capacity (the per-profile analog of the scalar Remaining) the
		// AdmissionCheck gates on.
		//
		// +listType=map
		// +listMapKey=name
		RemainingProfiles []AcceleratorProfileCount `json:"remainingProfiles,omitempty" yaml:"remainingProfiles,omitempty" protobuf:"bytes,7,rep,name=remainingProfiles"` // nolint: lll

		// AllocatedPhysicalProfile and AllocatedPhysicalPlacements are the per-Pod annotation
		// TRANSPORT the reconciler consumes to build the ledger above — not status output. The
		// device-plugin Allocate records, in the Pod's own allocation annotation, the single
		// physical partition that Pod holds on this accelerator (e.g. an NVIDIA MIG instance):
		// its profile name and the memory-slice interval(s) it occupies. A Pod holds one instance of
		// one profile per accelerator.
		//
		// Every field in this transport group is omitted from the aggregated Devices.Status — but
		// none of them is hidden: Instance.status.allocations reports each Pod's own record
		// verbatim, and that is where an operator reads which partition their Instance holds.
		AllocatedPhysicalProfile string `json:"allocatedPhysicalProfile,omitempty" yaml:"allocatedPhysicalProfile,omitempty" protobuf:"bytes,8,opt,name=allocatedPhysicalProfile"` // nolint: lll

		// AllocatedPhysicalPlacements is the memory-slice interval(s) the Pod's partition occupies
		// on this accelerator. The reconciler unions these across the node's Pods to derive
		// RemainingProfiles.
		//
		// +listType=atomic
		AllocatedPhysicalPlacements []AcceleratorPlacement `json:"allocatedPhysicalPlacements,omitempty" yaml:"allocatedPhysicalPlacements,omitempty" protobuf:"bytes,9,rep,name=allocatedPhysicalPlacements"` // nolint: lll

		// AllocatedPhysicalID is the partition's own identifier, as the driver named it at the moment
		// it was created (an NVIDIA MIG UUID, a T-Head PPU MIG UUID). Part of the same annotation
		// TRANSPORT, empty (omitted) in the aggregated Devices.Status.
		//
		// It is recorded because the allocator holds it already and the reader would otherwise have to
		// EARN it back: without it, naming a partition means translating the recorded profile name
		// into a driver profile id, enumerating every instance of that profile, and matching on the
		// placement — dozens of driver calls per card per monitor period, to recover something that
		// was in hand at Allocate time. So it is also the ONLY way a partition is addressed: an
		// allocation carrying none is reported as an absence rather than derived.
		//
		// It is NOT a generation marker. These UUIDs are name-based, derived from the parent device
		// and the instance's own identity, so destroying a partition and creating another at the same
		// placement yields the SAME identifier. Treat a recorded id as a fast way to FIND the
		// partition, never as proof that the one found is the one that was granted.
		AllocatedPhysicalID string `json:"allocatedPhysicalID,omitempty" yaml:"allocatedPhysicalID,omitempty" protobuf:"bytes,11,opt,name=allocatedPhysicalID"` // nolint: lll

		// AllocatedLogicalPlacements is the per-Pod annotation TRANSPORT of the logical-slice
		// ledger: the compute geometry the Pod's logical slice holds on this accelerator, in the
		// manufacturer's own compute units (on AMD, CU-mask bit indexes exactly as they appear in
		// HSA_CU_MASK). The device-plugin Allocate records it so a later placement decision reads
		// what the node's live slices already occupy. Empty (omitted) in the aggregated
		// Devices.Status, and for a manufacturer whose logical slice has no position.
		//
		// +listType=atomic
		AllocatedLogicalPlacements []AcceleratorPlacement `json:"allocatedLogicalPlacements,omitempty" yaml:"allocatedLogicalPlacements,omitempty" protobuf:"bytes,10,rep,name=allocatedLogicalPlacements"` // nolint: lll
	}
)

// DevicesList holds the list of Devices.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type DevicesList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,name=metadata"`

	Items []Devices `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*DevicesList)(nil)
