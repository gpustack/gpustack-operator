package mockspec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

// QueryState controls how a mock accelerator slot responds during detection and monitoring.
type QueryState string

const (
	// QueryStateOK indicates that the slot responds normally.
	QueryStateOK QueryState = "ok"
	// QueryStateUnqueryable simulates a card-specific hardware or bus fault that skips the slot
	// while retaining any previously cached reading as unhealthy.
	QueryStateUnqueryable QueryState = "unqueryable"
	// QueryStateFatalError simulates a non-card-specific error that fails the whole manufacturer pass.
	QueryStateFatalError QueryState = "fatalError"
)

// InitErrorSpec configures library initialization failure injection for one manufacturer.
// In YAML or JSON it accepts a boolean, an error message string, or an object with
// enabled, failCount, and message fields. In object form, omitting enabled auto-enables
// injection whenever failCount or message is non-empty; setting enabled explicitly to false
// disables injection while preserving failCount and message for toggling.
type InitErrorSpec struct {
	// Enabled activates initialization failure injection.
	Enabled bool `json:"enabled,omitempty"`
	// FailCount sets how many initial attempts fail before succeeding; zero means persistent failure.
	FailCount int `json:"failCount,omitempty"`
	// Message is the diagnostic message returned on failure.
	Message string `json:"message,omitempty"`
}

// UnmarshalJSON decodes InitErrorSpec from a bool, string, or structured object.
func (s *InitErrorSpec) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = InitErrorSpec{}
		return nil
	}

	var b bool
	if err := json.Unmarshal(trimmed, &b); err == nil {
		if b {
			*s = InitErrorSpec{Enabled: true, Message: "mock library initialization failed"}
		} else {
			*s = InitErrorSpec{}
		}
		return nil
	}

	var msg string
	if err := json.Unmarshal(trimmed, &msg); err == nil {
		if msg != "" {
			*s = InitErrorSpec{Enabled: true, Message: msg}
		} else {
			*s = InitErrorSpec{}
		}
		return nil
	}

	type rawInitError struct {
		Enabled   *bool  `json:"enabled,omitempty"`
		FailCount int    `json:"failCount,omitempty"`
		Message   string `json:"message,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var raw rawInitError
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("decode initError: %w", err)
	}
	enabled := raw.FailCount != 0 || raw.Message != ""
	if raw.Enabled != nil {
		enabled = *raw.Enabled
	}
	out := InitErrorSpec{
		Enabled:   enabled,
		FailCount: raw.FailCount,
		Message:   raw.Message,
	}
	if out.Enabled && out.Message == "" {
		out.Message = "mock library initialization failed"
	}
	*s = out
	return nil
}

// FaultErrorSpec configures a manufacturer-wide count or total-query failure.
// In YAML or JSON it accepts a boolean, an error message string, or an object with enabled and message.
// In object form, omitting enabled auto-enables injection when message is non-empty; setting enabled
// explicitly to false disables injection while preserving message for toggling.
type FaultErrorSpec struct {
	// Enabled activates the fault.
	Enabled bool `json:"enabled,omitempty"`
	// Message is the diagnostic message returned on failure.
	Message string `json:"message,omitempty"`
}

// UnmarshalJSON decodes FaultErrorSpec from a bool, string, or structured object.
func (s *FaultErrorSpec) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = FaultErrorSpec{}
		return nil
	}

	var b bool
	if err := json.Unmarshal(trimmed, &b); err == nil {
		*s = FaultErrorSpec{Enabled: b}
		return nil
	}

	var msg string
	if err := json.Unmarshal(trimmed, &msg); err == nil {
		*s = FaultErrorSpec{Enabled: msg != "", Message: msg}
		return nil
	}

	type rawFaultError struct {
		Enabled *bool  `json:"enabled,omitempty"`
		Message string `json:"message,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var raw rawFaultError
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("decode fault error: %w", err)
	}
	enabled := raw.Message != ""
	if raw.Enabled != nil {
		enabled = *raw.Enabled
	}
	*s = FaultErrorSpec{
		Enabled: enabled,
		Message: raw.Message,
	}
	return nil
}

// ManufacturerFaultSpec groups manufacturer-level fault controls.
type ManufacturerFaultSpec struct {
	// InitError injects persistent or transient library initialization errors.
	InitError InitErrorSpec `json:"initError,omitempty"`
	// CountError injects a failure at the device-count enumeration step.
	CountError FaultErrorSpec `json:"countError,omitempty"`
	// TotalFailure fails the entire detect and monitor pass so lastDetected is retained.
	TotalFailure FaultErrorSpec `json:"totalFailure,omitempty"`
}

// AcceleratorMetricsSpec declares monitor metrics for one mock accelerator.
type AcceleratorMetricsSpec struct {
	MemoryUsage       uint64  `json:"memoryUsage,omitempty"`
	MemoryUtilization *uint32 `json:"memoryUtilization,omitempty"`
	CoresUtilization  uint32  `json:"coresUtilization,omitempty"`
	Temperature       uint32  `json:"temperature,omitempty"`
	PowerUsage        uint32  `json:"powerUsage,omitempty"`
}

// AcceleratorMetricsOverrideSpec patches monitor metrics on an existing accelerator slot.
type AcceleratorMetricsOverrideSpec struct {
	MemoryUsage       *uint64 `json:"memoryUsage,omitempty"`
	MemoryUtilization *uint32 `json:"memoryUtilization,omitempty"`
	CoresUtilization  *uint32 `json:"coresUtilization,omitempty"`
	Temperature       *uint32 `json:"temperature,omitempty"`
	PowerUsage        *uint32 `json:"powerUsage,omitempty"`
}

// AcceleratorSpec declares one mock accelerator slot inside a device group.
type AcceleratorSpec struct {
	ID              string                                `json:"id"`
	Index           *int                                  `json:"index,omitempty"`
	PhysicalIndexes []uint32                              `json:"physicalIndexes,omitempty"`
	Topology        workercore.DeviceTopology             `json:"topology,omitempty"`
	LogicalSliced   *workercore.AcceleratorLogicalSliced  `json:"logicalSliced,omitempty"`
	PhysicalSliced  *workercore.AcceleratorPhysicalSliced `json:"physicalSliced,omitempty"`
	Unhealthy       bool                                  `json:"unhealthy,omitempty"`
	QueryState      QueryState                            `json:"queryState,omitempty"`
	Metrics         *AcceleratorMetricsSpec               `json:"metrics,omitempty"`
}

// GroupSpec declares a homogeneous group of mock accelerators.
type GroupSpec struct {
	ID                string            `json:"id,omitempty"`
	Manufacturer      string            `json:"manufacturer,omitempty"`
	Name              string            `json:"name"`
	Memory            uint64            `json:"memory"`
	Cores             uint32            `json:"cores,omitempty"`
	DriverVersion     string            `json:"driverVersion,omitempty"`
	RuntimeVersion    string            `json:"runtimeVersion,omitempty"`
	ComputeCapability string            `json:"computeCapability,omitempty"`
	Family            string            `json:"family,omitempty"`
	Accelerators      []AcceleratorSpec `json:"accelerators,omitempty"`
}

// GroupOverrideSpec patches group metadata on a preset or group.
type GroupOverrideSpec struct {
	ID                *string `json:"id,omitempty"`
	Name              *string `json:"name,omitempty"`
	Memory            *uint64 `json:"memory,omitempty"`
	Cores             *uint32 `json:"cores,omitempty"`
	DriverVersion     *string `json:"driverVersion,omitempty"`
	RuntimeVersion    *string `json:"runtimeVersion,omitempty"`
	ComputeCapability *string `json:"computeCapability,omitempty"`
	Family            *string `json:"family,omitempty"`
}

// CardOverrideSpec patches one accelerator slot addressed by its logical index.
type CardOverrideSpec struct {
	ID              *string                               `json:"id,omitempty"`
	PhysicalIndexes *[]uint32                             `json:"physicalIndexes,omitempty"`
	Topology        *workercore.DeviceTopology            `json:"topology,omitempty"`
	LogicalSliced   *workercore.AcceleratorLogicalSliced  `json:"logicalSliced,omitempty"`
	PhysicalSliced  *workercore.AcceleratorPhysicalSliced `json:"physicalSliced,omitempty"`
	Unhealthy       *bool                                 `json:"unhealthy,omitempty"`
	QueryState      *QueryState                           `json:"queryState,omitempty"`
	Metrics         *AcceleratorMetricsOverrideSpec       `json:"metrics,omitempty"`
}

// ProcessSpec declares one host process holding a mock accelerator.
type ProcessSpec struct {
	PID          uint32  `json:"pid"`
	MemoryBytes  *uint64 `json:"memoryBytes,omitempty"`
	CoresPercent *uint32 `json:"coresPercent,omitempty"`
}

// AcceleratorProcessesSpec declares per-process usage rows for one mock accelerator.
type AcceleratorProcessesSpec struct {
	ID           string                          `json:"id"`
	MemoryReason device.AcceleratorProcessReason `json:"memoryReason,omitempty"`
	CoresReason  device.AcceleratorProcessReason `json:"coresReason,omitempty"`
	Processes    []ProcessSpec                   `json:"processes,omitempty"`
}

// PartitionUsageSpec declares hardware-partition telemetry for one mock partition handle.
type PartitionUsageSpec struct {
	DeviceID         string                            `json:"deviceId"`
	ID               string                            `json:"id,omitempty"`
	Profile          string                            `json:"profile,omitempty"`
	Placements       []workercore.AcceleratorPlacement `json:"placements,omitempty"`
	MemoryTotalBytes *uint64                           `json:"memoryTotalBytes,omitempty"`
	MemoryUsedBytes  *uint64                           `json:"memoryUsedBytes,omitempty"`
	CoresPercent     *uint32                           `json:"coresPercent,omitempty"`
	MemoryReason     device.AcceleratorProcessReason   `json:"memoryReason,omitempty"`
	CoresReason      device.AcceleratorProcessReason   `json:"coresReason,omitempty"`
}

// RDMAPortSpec declares one port of a mock host RDMA verbs device.
type RDMAPortSpec struct {
	Number        uint32   `json:"number"`
	NetDevs       []string `json:"netDevs,omitempty"`
	LinkLayer     string   `json:"linkLayer,omitempty"`
	State         string   `json:"state,omitempty"`
	PhysicalState string   `json:"physicalState,omitempty"`
	Rate          string   `json:"rate,omitempty"`
	LID           string   `json:"lid,omitempty"`
	SMLID         string   `json:"smLid,omitempty"`
	GIDs          []string `json:"gids,omitempty"`
	PKeys         []string `json:"pkeys,omitempty"`
}

// RDMADeviceSpec declares one mock host RDMA verbs device.
type RDMADeviceSpec struct {
	Name            string                 `json:"name"`
	AllocationUnit  string                 `json:"allocationUnit,omitempty"`
	Owner           device.RDMADeviceOwner `json:"owner,omitempty"`
	Driver          string                 `json:"driver,omitempty"`
	PciBusID        string                 `json:"pciBusId,omitempty"`
	ParentPciBusID  string                 `json:"parentPciBusId,omitempty"`
	VirtualFunction bool                   `json:"virtualFunction,omitempty"`
	NodeGuid        string                 `json:"nodeGuid,omitempty"`
	SysImageGuid    string                 `json:"sysImageGuid,omitempty"`
	NumaAffinity    string                 `json:"numaAffinity,omitempty"`
	CpuAffinity     string                 `json:"cpuAffinity,omitempty"`
	Ports           []RDMAPortSpec         `json:"ports,omitempty"`
	Message         string                 `json:"message,omitempty"`
}

// ManufacturerSpec declares inventory, overlays, faults, and telemetry for one manufacturer.
type ManufacturerSpec struct {
	Preset        string                     `json:"preset,omitempty"`
	Count         *int                       `json:"count,omitempty"`
	Fault         ManufacturerFaultSpec      `json:"fault,omitempty"`
	GroupOverride *GroupOverrideSpec         `json:"groupOverride,omitempty"`
	CardOverrides map[int]CardOverrideSpec   `json:"cardOverrides,omitempty"`
	Groups        []GroupSpec                `json:"groups,omitempty"`
	Processes     []AcceleratorProcessesSpec `json:"processes,omitempty"`
	Partitions    []PartitionUsageSpec       `json:"partitions,omitempty"`
}

// PresetRef references a named preset with optional count, fault, and card overrides.
type PresetRef struct {
	Preset        string                   `json:"preset"`
	Count         *int                     `json:"count,omitempty"`
	Fault         ManufacturerFaultSpec    `json:"fault,omitempty"`
	GroupOverride *GroupOverrideSpec       `json:"groupOverride,omitempty"`
	CardOverrides map[int]CardOverrideSpec `json:"cardOverrides,omitempty"`
}

// NodeSpec declares the mock hardware and network inventory for one node.
type NodeSpec struct {
	Preset          string                        `json:"preset,omitempty"`
	Count           *int                          `json:"count,omitempty"`
	Fault           ManufacturerFaultSpec         `json:"fault,omitempty"`
	GroupOverride   *GroupOverrideSpec            `json:"groupOverride,omitempty"`
	CardOverrides   map[int]CardOverrideSpec      `json:"cardOverrides,omitempty"`
	Groups          []GroupSpec                   `json:"groups,omitempty"`
	Processes       []AcceleratorProcessesSpec    `json:"processes,omitempty"`
	Partitions      []PartitionUsageSpec          `json:"partitions,omitempty"`
	Presets         []PresetRef                   `json:"presets,omitempty"`
	Manufacturers   map[string]ManufacturerSpec   `json:"manufacturers,omitempty"`
	Interfaces      *[]workercore.DeviceInterface `json:"interfaces,omitempty"`
	InterfacesError string                        `json:"interfacesError,omitempty"`
	RDMADevices     *[]RDMADeviceSpec             `json:"rdmaDevices,omitempty"`
	RDMAError       string                        `json:"rdmaError,omitempty"`
}

// Document is a parsed and validated mock hardware specification file.
// It represents either a flat NodeSpec or a multi-node map with Default and Nodes.
type Document struct {
	NodeSpec
	Default *NodeSpec            `json:"default,omitempty"`
	Nodes   map[string]*NodeSpec `json:"nodes,omitempty"`

	isMultiNode     bool
	resolvedFlat    *ResolvedNodeSpec
	resolvedDefault *ResolvedNodeSpec
	resolvedNodes   map[string]*ResolvedNodeSpec
}

// ResolveNode returns an isolated ResolvedNodeSpec for nodeName.
// In multi-node documents, nodes[nodeName] wins when present, falling back to default,
// or returning an empty ResolvedNodeSpec when neither is defined.
func (d *Document) ResolveNode(nodeName string) *ResolvedNodeSpec {
	if d == nil {
		return emptyResolvedNodeSpec()
	}
	if !d.isMultiNode {
		return d.resolvedFlat.DeepCopy()
	}
	if nodeName != "" {
		if spec, ok := d.resolvedNodes[nodeName]; ok {
			return spec.DeepCopy()
		}
	}
	if d.resolvedDefault != nil {
		return d.resolvedDefault.DeepCopy()
	}
	return emptyResolvedNodeSpec()
}

// GroupMeta holds the group-level attributes shared by accelerators in one DevicesGroup.
type GroupMeta struct {
	ID                string
	Manufacturer      string
	Name              string
	Memory            uint64
	Cores             uint32
	DriverVersion     string
	RuntimeVersion    string
	ComputeCapability string
	Family            string
}

// ResolvedSlot is one fully resolved accelerator slot and its telemetry.
type ResolvedSlot struct {
	Index       uint32
	Group       GroupMeta
	Accelerator device.Accelerator
	Metrics     device.AcceleratorMetrics
	QueryState  QueryState
}

// DeepCopy returns an isolated copy of the ResolvedSlot.
func (s ResolvedSlot) DeepCopy() ResolvedSlot {
	out := s
	out.Accelerator = *s.Accelerator.DeepCopy()
	return out
}

// ResolvedManufacturerSpec holds the validated slots, faults, and companion telemetry for one manufacturer.
type ResolvedManufacturerSpec struct {
	Manufacturer string
	Fault        ManufacturerFaultSpec
	Slots        []ResolvedSlot
	Processes    map[string]device.AcceleratorProcesses
	Partitions   []device.AcceleratorPartition
}

// BuildGroups constructs a DevicesGroupList from all slots with aggregated slicing details.
func (m *ResolvedManufacturerSpec) BuildGroups() device.DevicesGroupList {
	if m == nil || len(m.Slots) == 0 {
		return nil
	}
	var groups device.DevicesGroupList
	for i := range m.Slots {
		slot := m.Slots[i]
		idx := slices.IndexFunc(groups, func(g device.DevicesGroup) bool {
			return g.ID == slot.Group.ID && g.Name == slot.Group.Name && g.Memory == slot.Group.Memory
		})
		if idx < 0 {
			groups = append(groups, device.DevicesGroup{
				ID:                slot.Group.ID,
				Manufacturer:      slot.Group.Manufacturer,
				Name:              slot.Group.Name,
				Memory:            slot.Group.Memory,
				Cores:             slot.Group.Cores,
				DriverVersion:     slot.Group.DriverVersion,
				RuntimeVersion:    slot.Group.RuntimeVersion,
				ComputeCapability: slot.Group.ComputeCapability,
				Family:            slot.Group.Family,
			})
			idx = len(groups) - 1
		}
		groups[idx].Accelerators = append(groups[idx].Accelerators, *slot.Accelerator.DeepCopy())
	}
	device.SetGroupSlicedDetails(groups)
	return groups
}

// BuildMetricsGroup constructs a MetricsGroup stamped with ts across all slots.
func (m *ResolvedManufacturerSpec) BuildMetricsGroup(ts time.Time) device.MetricsGroup {
	if m == nil {
		return device.MetricsGroup{Timestamp: ts}
	}
	accels := make([]device.AcceleratorMetrics, len(m.Slots))
	for i := range m.Slots {
		accels[i] = m.Slots[i].Metrics
	}
	return device.MetricsGroup{
		Manufacturer: m.Manufacturer,
		Timestamp:    ts,
		Accelerators: accels,
	}
}

// DeepCopy returns an isolated copy of ResolvedManufacturerSpec.
func (m *ResolvedManufacturerSpec) DeepCopy() *ResolvedManufacturerSpec {
	if m == nil {
		return nil
	}
	out := &ResolvedManufacturerSpec{
		Manufacturer: m.Manufacturer,
		Fault:        m.Fault,
		Slots:        make([]ResolvedSlot, len(m.Slots)),
	}
	for i := range m.Slots {
		out.Slots[i] = m.Slots[i].DeepCopy()
	}
	if m.Processes != nil {
		out.Processes = make(map[string]device.AcceleratorProcesses, len(m.Processes))
		for k, v := range m.Processes {
			out.Processes[k] = cloneAcceleratorProcesses(v)
		}
	}
	if m.Partitions != nil {
		out.Partitions = make([]device.AcceleratorPartition, len(m.Partitions))
		for i := range m.Partitions {
			out.Partitions[i] = cloneAcceleratorPartition(m.Partitions[i])
		}
	}
	return out
}

// ResolvedNodeSpec holds the fully resolved mock specification for a single node.
type ResolvedNodeSpec struct {
	Manufacturers        map[string]*ResolvedManufacturerSpec
	Interfaces           []device.Interface
	InterfacesError      string
	RDMADevices          []device.RDMADevice
	RDMAError            string
	interfacesConfigured bool
	rdmaConfigured       bool
}

// HasInterfaces reports whether mock network interfaces (or an interfaces error) were configured.
func (n *ResolvedNodeSpec) HasInterfaces() bool {
	return n != nil && (n.interfacesConfigured || n.InterfacesError != "")
}

// HasRDMADevices reports whether mock RDMA devices (or an RDMA error) were configured.
func (n *ResolvedNodeSpec) HasRDMADevices() bool {
	return n != nil && (n.rdmaConfigured || n.RDMAError != "")
}

// ManufacturerNames returns the sorted list of manufacturers configured on this node.
func (n *ResolvedNodeSpec) ManufacturerNames() []string {
	if n == nil || len(n.Manufacturers) == 0 {
		return nil
	}
	names := slices.Collect(maps.Keys(n.Manufacturers))
	sort.Strings(names)
	return names
}

// DeepCopy returns an isolated copy of ResolvedNodeSpec.
func (n *ResolvedNodeSpec) DeepCopy() *ResolvedNodeSpec {
	if n == nil {
		return emptyResolvedNodeSpec()
	}
	out := &ResolvedNodeSpec{
		Manufacturers:        make(map[string]*ResolvedManufacturerSpec, len(n.Manufacturers)),
		InterfacesError:      n.InterfacesError,
		RDMAError:            n.RDMAError,
		interfacesConfigured: n.interfacesConfigured,
		rdmaConfigured:       n.rdmaConfigured,
	}
	for k, v := range n.Manufacturers {
		out.Manufacturers[k] = v.DeepCopy()
	}
	if n.Interfaces != nil {
		out.Interfaces = make([]device.Interface, len(n.Interfaces))
		for i := range n.Interfaces {
			n.Interfaces[i].DeepCopyInto(&out.Interfaces[i])
		}
	}
	if n.RDMADevices != nil {
		out.RDMADevices = make([]device.RDMADevice, len(n.RDMADevices))
		for i := range n.RDMADevices {
			out.RDMADevices[i] = *n.RDMADevices[i].DeepCopy()
		}
	}
	return out
}

func emptyResolvedNodeSpec() *ResolvedNodeSpec {
	return &ResolvedNodeSpec{
		Manufacturers: make(map[string]*ResolvedManufacturerSpec),
	}
}

func cloneAcceleratorProcesses(in device.AcceleratorProcesses) device.AcceleratorProcesses {
	out := device.AcceleratorProcesses{
		ID:           in.ID,
		MemoryReason: in.MemoryReason,
		CoresReason:  in.CoresReason,
	}
	if in.Processes != nil {
		out.Processes = make([]device.AcceleratorProcess, len(in.Processes))
		for i, p := range in.Processes {
			cp := ProcessSpec{PID: p.PID}
			if p.MemoryBytes != nil {
				v := *p.MemoryBytes
				cp.MemoryBytes = &v
			}
			if p.CoresPercent != nil {
				v := *p.CoresPercent
				cp.CoresPercent = &v
			}
			out.Processes[i] = device.AcceleratorProcess{
				PID:          cp.PID,
				MemoryBytes:  cp.MemoryBytes,
				CoresPercent: cp.CoresPercent,
			}
		}
	}
	return out
}

func cloneAcceleratorPartition(in device.AcceleratorPartition) device.AcceleratorPartition {
	out := in
	if in.Placements != nil {
		out.Placements = slices.Clone(in.Placements)
	}
	if in.MemoryTotalBytes != nil {
		v := *in.MemoryTotalBytes
		out.MemoryTotalBytes = &v
	}
	if in.MemoryUsedBytes != nil {
		v := *in.MemoryUsedBytes
		out.MemoryUsedBytes = &v
	}
	if in.CoresPercent != nil {
		v := *in.CoresPercent
		out.CoresPercent = &v
	}
	return out
}
