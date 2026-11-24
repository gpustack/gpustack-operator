package mockspec

import (
	"fmt"
	"slices"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func validateFaultSpec(scope string, f ManufacturerFaultSpec) error {
	if f.InitError.FailCount < 0 {
		return fmt.Errorf("%s.initError: failCount (%d) must be non-negative", scope, f.InitError.FailCount)
	}
	return nil
}

func isValidQueryState(q QueryState) bool {
	switch q {
	case QueryStateOK, QueryStateUnqueryable, QueryStateFatalError:
		return true
	default:
		return false
	}
}

func isValidProcessReason(r device.AcceleratorProcessReason) bool {
	switch r {
	case device.AcceleratorProcessReasonNone,
		device.AcceleratorProcessReasonUnsupported,
		device.AcceleratorProcessReasonPermission,
		device.AcceleratorProcessReasonDriverError,
		device.AcceleratorProcessReasonTruncated:
		return true
	default:
		return false
	}
}

func validateResolvedSlots(scope, manufacturer string, slots []ResolvedSlot) error {
	seenIndexes := make(map[uint32]bool, len(slots))
	seenIDs := make(map[string]bool, len(slots))
	groupFirstProfiles := make(map[string]map[string]device.AcceleratorPhysicalSlicedProfile)

	for i, slot := range slots {
		sScope := fmt.Sprintf("%s.slots[%d]", scope, i)
		if slot.Accelerator.ID == "" {
			return fmt.Errorf("%s: accelerator id is required", sScope)
		}
		if seenIndexes[slot.Index] {
			return fmt.Errorf("%s: duplicate accelerator index %d", sScope, slot.Index)
		}
		seenIndexes[slot.Index] = true

		if seenIDs[slot.Accelerator.ID] {
			return fmt.Errorf("%s: duplicate accelerator id %q", sScope, slot.Accelerator.ID)
		}
		seenIDs[slot.Accelerator.ID] = true

		if !isValidQueryState(slot.QueryState) {
			return fmt.Errorf("%s: invalid queryState %q (want ok, unqueryable, or fatalError)", sScope, slot.QueryState)
		}

		ls := slot.Accelerator.Status.LogicalSliced
		ps := slot.Accelerator.Status.PhysicalSliced
		if ls.Count < 0 {
			return fmt.Errorf("%s: logicalSliced.count (%d) must be non-negative", sScope, ls.Count)
		}
		if ps.Count < 0 {
			return fmt.Errorf("%s: physicalSliced.count (%d) must be non-negative", sScope, ps.Count)
		}
		if ls.Count > 0 && (ps.Count > 0 || len(ps.Profiles) > 0) {
			return fmt.Errorf("%s: logicalSliced and physicalSliced are mutually exclusive on card %s", sScope, slot.Accelerator.ID)
		}

		if len(ps.Profiles) > 0 || ps.Count > 0 {
			if err := validatePhysicalSliced(sScope, manufacturer, slot.Group.ID, ps, groupFirstProfiles); err != nil {
				return err
			}
		}
	}

	return nil
}

func validatePhysicalSliced(
	scope, manufacturer, groupID string,
	ps device.AcceleratorPhysicalSliced,
	groupFirst map[string]map[string]device.AcceleratorPhysicalSlicedProfile,
) error {
	if nodefeature.GetPartitionKind(manufacturer) == "" {
		return fmt.Errorf("%s: manufacturer %q does not support hardware partition profiles", scope, manufacturer)
	}
	if len(ps.Profiles) == 0 {
		return fmt.Errorf("%s: physicalSliced.profiles must not be empty when physicalSliced.count is positive", scope)
	}

	seenNames := make(map[string]bool, len(ps.Profiles))
	for pi, p := range ps.Profiles {
		pScope := fmt.Sprintf("%s.physicalSliced.profiles[%d]", scope, pi)
		if p.Name == "" {
			return fmt.Errorf("%s: profile name is required", pScope)
		}
		if seenNames[p.Name] {
			return fmt.Errorf("%s: duplicate partition profile name %q", pScope, p.Name)
		}
		seenNames[p.Name] = true

		if nodefeature.GetAcceleratablePartitionedProfileResourceName(manufacturer, p.Name) == "" {
			return fmt.Errorf("%s: partition profile %q does not yield a valid resource name", pScope, p.Name)
		}
		if p.MemoryMib <= 0 {
			return fmt.Errorf("%s: profile %q memoryMib (%d) must be positive", pScope, p.Name, p.MemoryMib)
		}
		if p.ComputeSlices <= 0 {
			return fmt.Errorf("%s: profile %q computeSlices (%d) must be positive", pScope, p.Name, p.ComputeSlices)
		}
		if p.MemorySlices <= 0 {
			return fmt.Errorf("%s: profile %q memorySlices (%d) must be positive", pScope, p.Name, p.MemorySlices)
		}
		if p.Count <= 0 {
			return fmt.Errorf("%s: profile %q count (%d) must be positive", pScope, p.Name, p.Count)
		}
		if len(p.Placements) == 0 {
			return fmt.Errorf("%s: profile %q must declare at least one legal placement", pScope, p.Name)
		}
		for si, pl := range p.Placements {
			if pl.Start < 0 || pl.Length <= 0 {
				return fmt.Errorf(
					"%s: profile %q placement[%d] ({start:%d, length:%d}) must have non-negative start and positive length",
					pScope, p.Name, si, pl.Start, pl.Length,
				)
			}
		}

		byGroup := groupFirst[groupID]
		if byGroup == nil {
			byGroup = make(map[string]device.AcceleratorPhysicalSlicedProfile)
			groupFirst[groupID] = byGroup
		}
		if prev, exists := byGroup[p.Name]; exists {
			if !physicalProfileEqual(prev, p) {
				return fmt.Errorf("%s: profile %q diverges in geometry across cards in group %q", pScope, p.Name, groupID)
			}
		} else {
			byGroup[p.Name] = p
		}
	}

	return nil
}

func physicalProfileEqual(a, b device.AcceleratorPhysicalSlicedProfile) bool {
	return a.Name == b.Name &&
		a.MemoryMib == b.MemoryMib &&
		a.ComputeSlices == b.ComputeSlices &&
		a.MemorySlices == b.MemorySlices &&
		a.Count == b.Count &&
		slices.Equal(a.Placements, b.Placements)
}

func resolveProcesses(
	scope string, specs []AcceleratorProcessesSpec,
) (map[string]device.AcceleratorProcesses, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make(map[string]device.AcceleratorProcesses, len(specs))
	for i, sp := range specs {
		pScope := fmt.Sprintf("%s[%d]", scope, i)
		if sp.ID == "" {
			return nil, fmt.Errorf("%s: process entry accelerator id is required", pScope)
		}
		if _, dup := out[sp.ID]; dup {
			return nil, fmt.Errorf("%s: duplicate process entry for accelerator %q", pScope, sp.ID)
		}
		if !isValidProcessReason(sp.MemoryReason) {
			return nil, fmt.Errorf("%s: invalid memoryReason %q", pScope, sp.MemoryReason)
		}
		if !isValidProcessReason(sp.CoresReason) {
			return nil, fmt.Errorf("%s: invalid coresReason %q", pScope, sp.CoresReason)
		}

		rows := make([]device.AcceleratorProcess, len(sp.Processes))
		for j, r := range sp.Processes {
			if r.PID == 0 {
				return nil, fmt.Errorf("%s.processes[%d]: pid must be positive", pScope, j)
			}
			if r.CoresPercent != nil && *r.CoresPercent > 100 {
				return nil, fmt.Errorf("%s.processes[%d]: coresPercent (%d) must not exceed 100", pScope, j, *r.CoresPercent)
			}
			var memPtr *uint64
			if r.MemoryBytes != nil {
				v := *r.MemoryBytes
				memPtr = &v
			}
			var coresPtr *uint32
			if r.CoresPercent != nil {
				v := *r.CoresPercent
				coresPtr = &v
			}
			rows[j] = device.AcceleratorProcess{
				PID:          r.PID,
				MemoryBytes:  memPtr,
				CoresPercent: coresPtr,
			}
		}

		out[sp.ID] = device.AcceleratorProcesses{
			ID:           sp.ID,
			MemoryReason: sp.MemoryReason,
			CoresReason:  sp.CoresReason,
			Processes:    rows,
		}
	}
	return out, nil
}

func resolvePartitions(scope string, specs []PartitionUsageSpec) ([]device.AcceleratorPartition, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]device.AcceleratorPartition, len(specs))
	for i, sp := range specs {
		pScope := fmt.Sprintf("%s[%d]", scope, i)
		if sp.DeviceID == "" {
			return nil, fmt.Errorf("%s: partition deviceId is required", pScope)
		}
		if !isValidProcessReason(sp.MemoryReason) {
			return nil, fmt.Errorf("%s: invalid memoryReason %q", pScope, sp.MemoryReason)
		}
		if !isValidProcessReason(sp.CoresReason) {
			return nil, fmt.Errorf("%s: invalid coresReason %q", pScope, sp.CoresReason)
		}
		if sp.CoresPercent != nil && *sp.CoresPercent > 100 {
			return nil, fmt.Errorf("%s: coresPercent (%d) must not exceed 100", pScope, *sp.CoresPercent)
		}

		out[i] = cloneAcceleratorPartition(device.AcceleratorPartition{
			DeviceID:         sp.DeviceID,
			ID:               sp.ID,
			Profile:          sp.Profile,
			Placements:       sp.Placements,
			MemoryTotalBytes: sp.MemoryTotalBytes,
			MemoryUsedBytes:  sp.MemoryUsedBytes,
			CoresPercent:     sp.CoresPercent,
			MemoryReason:     sp.MemoryReason,
			CoresReason:      sp.CoresReason,
		})
	}
	return out, nil
}

func resolveInterfaces(scope string, specs []workercore.DeviceInterface) ([]device.Interface, error) {
	out := make([]device.Interface, len(specs))
	seen := make(map[string]bool, len(specs))
	for i := range specs {
		iScope := fmt.Sprintf("%s[%d]", scope, i)
		if specs[i].Name == "" {
			return nil, fmt.Errorf("%s: interface name is required", iScope)
		}
		if seen[specs[i].Name] {
			return nil, fmt.Errorf("%s: duplicate interface name %q", iScope, specs[i].Name)
		}
		seen[specs[i].Name] = true
		if err := validateInterfaceLink(iScope+".link", specs[i].Link); err != nil {
			return nil, err
		}
		for vi, vf := range specs[i].VirtualFunctions {
			if err := validateInterfaceLink(fmt.Sprintf("%s.virtualFunctions[%d].link", iScope, vi), vf.Link); err != nil {
				return nil, err
			}
		}
		specs[i].DeepCopyInto(&out[i])
	}
	return out, nil
}

func validateInterfaceLink(scope string, link *workercore.DeviceInterfaceLink) error {
	if link == nil {
		return nil
	}
	switch link.State {
	case workercore.DeviceInterfaceLinkStateOK,
		workercore.DeviceInterfaceLinkStateUnverified,
		workercore.DeviceInterfaceLinkStateFailed:
		return nil
	default:
		return fmt.Errorf("%s: invalid link state %q (want OK, Unverified, or Failed)", scope, link.State)
	}
}

func resolveRDMADevices(scope string, specs []RDMADeviceSpec) ([]device.RDMADevice, error) {
	out := make([]device.RDMADevice, len(specs))
	seen := make(map[string]bool, len(specs))
	for i, sp := range specs {
		rScope := fmt.Sprintf("%s[%d]", scope, i)
		if sp.Name == "" {
			return nil, fmt.Errorf("%s: rdma device name is required", rScope)
		}
		if seen[sp.Name] {
			return nil, fmt.Errorf("%s: duplicate rdma device name %q", rScope, sp.Name)
		}
		seen[sp.Name] = true

		owner := sp.Owner
		switch owner {
		case "":
			if sp.Driver == "efa" {
				owner = device.RDMADeviceOwnerExternal
			} else {
				owner = device.RDMADeviceOwnerGPUStack
			}
		case device.RDMADeviceOwnerGPUStack, device.RDMADeviceOwnerExternal:
		default:
			return nil, fmt.Errorf("%s: invalid rdma owner %q (want GPUStack or External)", rScope, owner)
		}

		allocUnit := sp.AllocationUnit
		if allocUnit == "" && sp.PciBusID != "" {
			allocUnit = sp.PciBusID
		}

		var ports []device.RDMAPort
		if len(sp.Ports) > 0 {
			ports = make([]device.RDMAPort, len(sp.Ports))
			for j, p := range sp.Ports {
				ports[j] = device.RDMAPort{
					Number:        p.Number,
					NetDevs:       slices.Clone(p.NetDevs),
					LinkLayer:     p.LinkLayer,
					State:         p.State,
					PhysicalState: p.PhysicalState,
					Rate:          p.Rate,
					LID:           p.LID,
					SMLID:         p.SMLID,
					GIDs:          slices.Clone(p.GIDs),
					PKeys:         slices.Clone(p.PKeys),
				}
			}
		}

		out[i] = device.RDMADevice{
			Name:            sp.Name,
			AllocationUnit:  allocUnit,
			Owner:           owner,
			Driver:          sp.Driver,
			PciBusID:        sp.PciBusID,
			ParentPciBusID:  sp.ParentPciBusID,
			VirtualFunction: sp.VirtualFunction,
			NodeGuid:        sp.NodeGuid,
			SysImageGuid:    sp.SysImageGuid,
			NumaAffinity:    sp.NumaAffinity,
			CpuAffinity:     sp.CpuAffinity,
			Ports:           ports,
			Message:         sp.Message,
		}
	}
	return out, nil
}
