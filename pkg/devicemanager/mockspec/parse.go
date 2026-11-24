package mockspec

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"

	"sigs.k8s.io/yaml"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// Parse decodes and strictly validates a YAML or JSON mock hardware specification document.
func Parse(raw []byte) (*Document, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("mock devices spec is empty")
	}

	var doc Document
	if err := yaml.Unmarshal(raw, &doc, yaml.DisallowUnknownFields); err != nil {
		return nil, fmt.Errorf("parse mock devices spec: %w", err)
	}

	hasMultiNode := doc.Default != nil || doc.Nodes != nil
	if hasMultiNode && hasFlatNodeFields(&doc.NodeSpec) {
		return nil, errors.New("cannot mix top-level node fields with default or nodes map")
	}

	doc.isMultiNode = hasMultiNode
	if !hasMultiNode {
		resolved, err := resolveNodeSpec("root", &doc.NodeSpec)
		if err != nil {
			return nil, err
		}
		doc.resolvedFlat = resolved
		return &doc, nil
	}

	if doc.Default != nil {
		resolved, err := resolveNodeSpec("default", doc.Default)
		if err != nil {
			return nil, err
		}
		doc.resolvedDefault = resolved
	}

	doc.resolvedNodes = make(map[string]*ResolvedNodeSpec, len(doc.Nodes))
	nodeNames := make([]string, 0, len(doc.Nodes))
	for name := range doc.Nodes {
		nodeNames = append(nodeNames, name)
	}
	slices.Sort(nodeNames)

	for _, name := range nodeNames {
		if name == "" {
			return nil, errors.New("nodes map contains an empty node name")
		}
		nodeSpec := doc.Nodes[name]
		if nodeSpec == nil {
			doc.resolvedNodes[name] = emptyResolvedNodeSpec()
			continue
		}
		resolved, err := resolveNodeSpec("nodes."+name, nodeSpec)
		if err != nil {
			return nil, err
		}
		doc.resolvedNodes[name] = resolved
	}

	return &doc, nil
}

// LoadFile reads, parses, and validates a mock hardware specification file from disk.
func LoadFile(filePath string) (*Document, error) {
	raw, _, _, err := readBoundedSpecFile(filePath)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func hasFlatNodeFields(n *NodeSpec) bool {
	if n == nil {
		return false
	}
	return n.Preset != "" ||
		n.Count != nil ||
		n.Fault.InitError.Enabled || n.Fault.InitError.FailCount != 0 || n.Fault.InitError.Message != "" ||
		n.Fault.CountError.Enabled || n.Fault.CountError.Message != "" ||
		n.Fault.TotalFailure.Enabled || n.Fault.TotalFailure.Message != "" ||
		n.GroupOverride != nil ||
		len(n.CardOverrides) > 0 ||
		len(n.Groups) > 0 ||
		len(n.Processes) > 0 ||
		len(n.Partitions) > 0 ||
		len(n.Presets) > 0 ||
		len(n.Manufacturers) > 0 ||
		n.Interfaces != nil ||
		n.InterfacesError != "" ||
		n.RDMADevices != nil ||
		n.RDMAError != ""
}

func resolveNodeSpec(scope string, spec *NodeSpec) (*ResolvedNodeSpec, error) {
	if spec == nil {
		return emptyResolvedNodeSpec(), nil
	}

	byMfr, err := collectManufacturerSpecs(scope, spec)
	if err != nil {
		return nil, err
	}

	out := emptyResolvedNodeSpec()
	seenNodeAccelIDs := make(map[string]string)

	mfrNames := make([]string, 0, len(byMfr))
	for mfr := range byMfr {
		mfrNames = append(mfrNames, mfr)
	}
	slices.Sort(mfrNames)

	for _, mfr := range mfrNames {
		resMfr, err := resolveManufacturerSpec(scope+"."+mfr, mfr, byMfr[mfr], LookupPreset)
		if err != nil {
			return nil, err
		}
		for _, slot := range resMfr.Slots {
			if prevMfr, dup := seenNodeAccelIDs[slot.Accelerator.ID]; dup {
				return nil, fmt.Errorf(
					"%s: duplicate accelerator id %q across %s and %s",
					scope, slot.Accelerator.ID, prevMfr, mfr,
				)
			}
			seenNodeAccelIDs[slot.Accelerator.ID] = mfr
		}
		out.Manufacturers[mfr] = resMfr
	}

	if spec.Interfaces != nil {
		ifaces, err := resolveInterfaces(scope+".interfaces", *spec.Interfaces)
		if err != nil {
			return nil, err
		}
		out.Interfaces = ifaces
		out.interfacesConfigured = true
	}
	out.InterfacesError = spec.InterfacesError

	if spec.RDMADevices != nil {
		rdmaDevs, err := resolveRDMADevices(scope+".rdmaDevices", *spec.RDMADevices)
		if err != nil {
			return nil, err
		}
		out.RDMADevices = rdmaDevs
		out.rdmaConfigured = true
	}
	out.RDMAError = spec.RDMAError

	return out, nil
}

func collectManufacturerSpecs(scope string, spec *NodeSpec) (map[string]ManufacturerSpec, error) {
	byMfr := make(map[string]ManufacturerSpec)

	addOrReject := func(mfr string, ms ManufacturerSpec) error {
		if !nodefeature.IsKnownAcceleratableManufacturer(mfr) {
			return fmt.Errorf("%s: unknown manufacturer %q", scope, mfr)
		}
		if _, exists := byMfr[mfr]; exists {
			return fmt.Errorf("%s: manufacturer %q is declared more than once", scope, mfr)
		}
		byMfr[mfr] = ms
		return nil
	}

	switch {
	case spec.Preset != "":
		if len(spec.Groups) > 0 {
			return nil, fmt.Errorf("%s: cannot set both top-level preset and top-level groups", scope)
		}
		mfr, _, ok := LookupPreset(spec.Preset)
		if !ok {
			return nil, fmt.Errorf("%s: unknown preset %q", scope, spec.Preset)
		}
		err := addOrReject(mfr, ManufacturerSpec{
			Preset:        spec.Preset,
			Count:         spec.Count,
			Fault:         spec.Fault,
			GroupOverride: spec.GroupOverride,
			CardOverrides: spec.CardOverrides,
			Processes:     spec.Processes,
			Partitions:    spec.Partitions,
		})
		if err != nil {
			return nil, err
		}
	case len(spec.Groups) > 0:
		groupedByMfr := make(map[string][]GroupSpec)
		for i, g := range spec.Groups {
			if !nodefeature.IsKnownAcceleratableManufacturer(g.Manufacturer) {
				return nil, fmt.Errorf("%s.groups[%d]: unknown manufacturer %q", scope, i, g.Manufacturer)
			}
			groupedByMfr[g.Manufacturer] = append(groupedByMfr[g.Manufacturer], g)
		}
		if len(groupedByMfr) > 1 && (spec.Count != nil || spec.GroupOverride != nil ||
			len(spec.CardOverrides) > 0 || len(spec.Processes) > 0 || len(spec.Partitions) > 0) {
			return nil, fmt.Errorf("%s: top-level overrides cannot be combined with multi-manufacturer groups", scope)
		}
		for mfr, grps := range groupedByMfr {
			err := addOrReject(mfr, ManufacturerSpec{
				Count:         spec.Count,
				Fault:         spec.Fault,
				GroupOverride: spec.GroupOverride,
				CardOverrides: spec.CardOverrides,
				Groups:        grps,
				Processes:     spec.Processes,
				Partitions:    spec.Partitions,
			})
			if err != nil {
				return nil, err
			}
		}
	case spec.Count != nil || spec.GroupOverride != nil || len(spec.CardOverrides) > 0 ||
		len(spec.Processes) > 0 || len(spec.Partitions) > 0 ||
		spec.Fault.InitError.Enabled || spec.Fault.InitError.FailCount != 0 ||
		spec.Fault.CountError.Enabled || spec.Fault.TotalFailure.Enabled:
		return nil, fmt.Errorf("%s: top-level overrides or faults require preset or groups", scope)
	}

	for i, ref := range spec.Presets {
		mfr, _, ok := LookupPreset(ref.Preset)
		if !ok {
			return nil, fmt.Errorf("%s.presets[%d]: unknown preset %q", scope, i, ref.Preset)
		}
		err := addOrReject(mfr, ManufacturerSpec{
			Preset:        ref.Preset,
			Count:         ref.Count,
			Fault:         ref.Fault,
			GroupOverride: ref.GroupOverride,
			CardOverrides: ref.CardOverrides,
		})
		if err != nil {
			return nil, err
		}
	}

	for mfr, ms := range spec.Manufacturers {
		if err := addOrReject(mfr, ms); err != nil {
			return nil, err
		}
	}

	return byMfr, nil
}

type presetLookupFunc func(name string) (string, ManufacturerSpec, bool)

func resolveManufacturerSpec(
	scope, manufacturer string, spec ManufacturerSpec, lookup presetLookupFunc,
) (*ResolvedManufacturerSpec, error) {
	if err := validateFaultSpec(scope+".fault", spec.Fault); err != nil {
		return nil, err
	}
	if spec.Count != nil && *spec.Count < 0 {
		return nil, fmt.Errorf("%s: count (%d) must be non-negative", scope, *spec.Count)
	}

	var groups []GroupSpec
	if spec.Preset != "" {
		if lookup == nil {
			return nil, fmt.Errorf("%s: preset reference %q is not allowed here", scope, spec.Preset)
		}
		if len(spec.Groups) > 0 {
			return nil, fmt.Errorf("%s: cannot specify both preset %q and explicit groups", scope, spec.Preset)
		}
		presetMfr, presetSpec, ok := lookup(spec.Preset)
		if !ok {
			return nil, fmt.Errorf("%s: unknown preset %q", scope, spec.Preset)
		}
		if presetMfr != manufacturer {
			return nil, fmt.Errorf(
				"%s: preset %q belongs to manufacturer %q, expected %q",
				scope, spec.Preset, presetMfr, manufacturer,
			)
		}
		groups = presetSpec.Groups
	} else {
		groups = make([]GroupSpec, len(spec.Groups))
		for i := range spec.Groups {
			groups[i] = cloneGroupSpec(spec.Groups[i])
		}
	}

	slots, err := buildAndScaleSlots(scope, manufacturer, groups, spec.GroupOverride, spec.Count)
	if err != nil {
		return nil, err
	}

	if err := applyCardOverrides(scope, manufacturer, slots, spec.CardOverrides); err != nil {
		return nil, err
	}

	if err := validateResolvedSlots(scope, manufacturer, slots); err != nil {
		return nil, err
	}

	procs, err := resolveProcesses(scope+".processes", spec.Processes)
	if err != nil {
		return nil, err
	}
	parts, err := resolvePartitions(scope+".partitions", spec.Partitions)
	if err != nil {
		return nil, err
	}

	return &ResolvedManufacturerSpec{
		Manufacturer: manufacturer,
		Fault:        spec.Fault,
		Slots:        slots,
		Processes:    procs,
		Partitions:   parts,
	}, nil
}

func buildAndScaleSlots(
	scope, manufacturer string,
	groups []GroupSpec,
	grpOverride *GroupOverrideSpec,
	count *int,
) ([]ResolvedSlot, error) {
	var slots []ResolvedSlot
	for gi, g := range groups {
		gScope := fmt.Sprintf("%s.groups[%d]", scope, gi)
		if g.Manufacturer != "" && g.Manufacturer != manufacturer {
			return nil, fmt.Errorf("%s: manufacturer %q does not match %q", gScope, g.Manufacturer, manufacturer)
		}

		name := g.Name
		memory := g.Memory
		cores := g.Cores
		driverVer := g.DriverVersion
		runtimeVer := g.RuntimeVersion
		cc := g.ComputeCapability
		family := g.Family
		id := g.ID
		idPinned := id != ""

		if grpOverride != nil {
			if grpOverride.Name != nil {
				name = *grpOverride.Name
			}
			if grpOverride.Memory != nil {
				memory = *grpOverride.Memory
			}
			if grpOverride.Cores != nil {
				cores = *grpOverride.Cores
			}
			if grpOverride.DriverVersion != nil {
				driverVer = *grpOverride.DriverVersion
			}
			if grpOverride.RuntimeVersion != nil {
				runtimeVer = *grpOverride.RuntimeVersion
			}
			if grpOverride.ComputeCapability != nil {
				cc = *grpOverride.ComputeCapability
			}
			if grpOverride.Family != nil {
				family = *grpOverride.Family
			}
			if grpOverride.ID != nil {
				id = *grpOverride.ID
				idPinned = true
			}
		}

		if name == "" {
			return nil, fmt.Errorf("%s: group name is required", gScope)
		}
		if memory == 0 {
			return nil, fmt.Errorf("%s: group memory must be greater than zero", gScope)
		}
		if !idPinned || id == "" {
			id = device.ConstructGroupID(manufacturer, name, memory)
		}
		if id == "" {
			return nil, fmt.Errorf("%s: group name %q produced an empty group id", gScope, name)
		}

		meta := GroupMeta{
			ID:                id,
			Manufacturer:      manufacturer,
			Name:              name,
			Memory:            memory,
			Cores:             cores,
			DriverVersion:     driverVer,
			RuntimeVersion:    runtimeVer,
			ComputeCapability: cc,
			Family:            family,
		}

		for ai, a := range g.Accelerators {
			aScope := fmt.Sprintf("%s.accelerators[%d]", gScope, ai)
			slot, err := buildSlot(aScope, meta, ai, a)
			if err != nil {
				return nil, err
			}
			slots = append(slots, slot)
		}
	}

	slices.SortFunc(slots, func(a, b ResolvedSlot) int {
		return cmp.Compare(a.Index, b.Index)
	})

	if count == nil {
		return slots, nil
	}
	target := *count
	if target == 0 {
		return nil, nil
	}
	if len(slots) == 0 {
		return nil, fmt.Errorf("%s: cannot scale count to %d with zero template accelerators", scope, target)
	}
	if target <= len(slots) {
		return slots[:target], nil
	}

	usedBDFs := make(map[string]bool, target)
	for _, s := range slots {
		if s.Accelerator.Topology.PciBusID != "" {
			usedBDFs[s.Accelerator.Topology.PciBusID] = true
		}
	}

	baseLen := len(slots)
	nextBus := 1
	for i := baseLen; i < target; i++ {
		tmpl := slots[i%baseLen].DeepCopy()
		idx := uint32(i)
		tmpl.Index = idx
		tmpl.Accelerator.Index = idx
		tmpl.Accelerator.ID = fmt.Sprintf("%s-mock-%d", tmpl.Accelerator.ID, i)
		tmpl.Accelerator.PhysicalIndexes = synthesizePhysicalIndexes(manufacturer, tmpl.Accelerator.PhysicalIndexes, idx)

		oldBDF := tmpl.Accelerator.Topology.PciBusID
		if oldBDF != "" {
			for {
				candidate := fmt.Sprintf("0000:%02x:%02x.0", (nextBus>>4)&0xff, nextBus&0x0f)
				nextBus++
				if !usedBDFs[candidate] {
					usedBDFs[candidate] = true
					tmpl.Accelerator.Topology.PciBusID = candidate
					if tmpl.Accelerator.Topology.PciRootID == oldBDF {
						tmpl.Accelerator.Topology.PciRootID = candidate
					}
					break
				}
			}
		}
		tmpl.Metrics.ID = tmpl.Accelerator.ID
		slots = append(slots, tmpl)
	}

	return slots, nil
}

func synthesizePhysicalIndexes(manufacturer string, tmpl []uint32, idx uint32) []uint32 {
	switch {
	case len(tmpl) == 3:
		return []uint32{idx, idx, 0}
	case len(tmpl) == 2:
		return []uint32{idx, 128 + idx}
	case manufacturer == nodefeature.ManufacturerTHead:
		return []uint32{idx + 1}
	default:
		return []uint32{idx}
	}
}

func buildSlot(scope string, meta GroupMeta, defaultIndex int, a AcceleratorSpec) (ResolvedSlot, error) {
	if a.ID == "" {
		return ResolvedSlot{}, fmt.Errorf("%s: accelerator id is required", scope)
	}
	rawIdx := defaultIndex
	if a.Index != nil {
		rawIdx = *a.Index
	}
	if rawIdx < 0 {
		return ResolvedSlot{}, fmt.Errorf("%s: accelerator index (%d) must be non-negative", scope, rawIdx)
	}
	idx := uint32(rawIdx)

	qState := a.QueryState
	if qState == "" {
		qState = QueryStateOK
	}

	physIdx := slices.Clone(a.PhysicalIndexes)
	if len(physIdx) == 0 {
		physIdx = []uint32{idx}
	}

	var status device.AcceleratorStatus
	status.Unhealthy = a.Unhealthy
	if a.LogicalSliced != nil {
		status.LogicalSliced = *a.LogicalSliced
	}
	if a.PhysicalSliced != nil {
		status.PhysicalSliced = *a.PhysicalSliced.DeepCopy()
		if status.PhysicalSliced.Count == 0 && len(status.PhysicalSliced.Profiles) > 0 {
			status.PhysicalSliced.Count = maxPhysicalProfileCount(status.PhysicalSliced.Profiles)
		}
	}

	var topo device.Topology
	a.Topology.DeepCopyInto(&topo)

	accel := device.Accelerator{
		ID:              a.ID,
		Index:           idx,
		PhysicalIndexes: physIdx,
		Topology:        topo,
		Status:          status,
	}

	metrics := device.AcceleratorMetrics{
		ID:        a.ID,
		Memory:    meta.Memory,
		Unhealthy: a.Unhealthy,
	}
	if a.Metrics != nil {
		if a.Metrics.MemoryUtilization != nil && *a.Metrics.MemoryUtilization > 100 {
			return ResolvedSlot{}, fmt.Errorf(
				"%s: memoryUtilization (%d) must not exceed 100",
				scope, *a.Metrics.MemoryUtilization,
			)
		}
		if a.Metrics.CoresUtilization > 100 {
			return ResolvedSlot{}, fmt.Errorf(
				"%s: coresUtilization (%d) must not exceed 100",
				scope, a.Metrics.CoresUtilization,
			)
		}
		metrics.MemoryUsage = a.Metrics.MemoryUsage
		if a.Metrics.MemoryUtilization != nil {
			metrics.MemoryUtilization = *a.Metrics.MemoryUtilization
		} else {
			metrics.MemoryUtilization = device.CalculateUtilization(a.Metrics.MemoryUsage, meta.Memory)
		}
		metrics.CoresUtilization = a.Metrics.CoresUtilization
		metrics.Temperature = a.Metrics.Temperature
		metrics.PowerUsage = a.Metrics.PowerUsage
	}

	return ResolvedSlot{
		Index:       idx,
		Group:       meta,
		Accelerator: accel,
		Metrics:     metrics,
		QueryState:  qState,
	}, nil
}

func applyCardOverrides(
	scope, _ string, slots []ResolvedSlot, overrides map[int]CardOverrideSpec,
) error {
	if len(overrides) == 0 {
		return nil
	}

	keys := make([]int, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	for _, key := range keys {
		ovScope := fmt.Sprintf("%s.cardOverrides[%d]", scope, key)
		if key < 0 {
			return fmt.Errorf("%s: cardOverrides index (%d) must be non-negative", ovScope, key)
		}
		pos := slices.IndexFunc(slots, func(s ResolvedSlot) bool {
			return s.Index == uint32(key)
		})
		if pos < 0 {
			return fmt.Errorf("%s: cardOverrides targets nonexistent card index %d", ovScope, key)
		}

		ov := overrides[key]
		slot := &slots[pos]
		if ov.ID != nil {
			slot.Accelerator.ID = *ov.ID
			slot.Metrics.ID = *ov.ID
		}
		if ov.PhysicalIndexes != nil {
			slot.Accelerator.PhysicalIndexes = slices.Clone(*ov.PhysicalIndexes)
		}
		if ov.Topology != nil {
			ov.Topology.DeepCopyInto(&slot.Accelerator.Topology)
		}
		if ov.LogicalSliced != nil && ov.PhysicalSliced != nil &&
			ov.LogicalSliced.Count > 0 && (ov.PhysicalSliced.Count > 0 || len(ov.PhysicalSliced.Profiles) > 0) {
			return fmt.Errorf("%s: logicalSliced and physicalSliced are mutually exclusive", ovScope)
		}
		if ov.LogicalSliced != nil {
			slot.Accelerator.Status.LogicalSliced = *ov.LogicalSliced
			if ov.LogicalSliced.Count > 0 && ov.PhysicalSliced == nil {
				slot.Accelerator.Status.PhysicalSliced = device.AcceleratorPhysicalSliced{}
			}
		}
		if ov.PhysicalSliced != nil {
			ps := *ov.PhysicalSliced.DeepCopy()
			if ps.Count == 0 && len(ps.Profiles) > 0 {
				ps.Count = maxPhysicalProfileCount(ps.Profiles)
			}
			slot.Accelerator.Status.PhysicalSliced = ps
			if (ps.Count > 0 || len(ps.Profiles) > 0) && ov.LogicalSliced == nil {
				slot.Accelerator.Status.LogicalSliced = device.AcceleratorLogicalSliced{}
			}
		}
		if ov.Unhealthy != nil {
			slot.Accelerator.Status.Unhealthy = *ov.Unhealthy
			slot.Metrics.Unhealthy = *ov.Unhealthy
		}
		if ov.QueryState != nil {
			slot.QueryState = *ov.QueryState
		}
		if ov.Metrics != nil {
			if err := applyMetricsOverride(ovScope+".metrics", &slot.Metrics, slot.Group.Memory, ov.Metrics); err != nil {
				return err
			}
		}
	}

	return nil
}

func applyMetricsOverride(
	scope string, dst *device.AcceleratorMetrics, totalMem uint64, ov *AcceleratorMetricsOverrideSpec,
) error {
	if ov.MemoryUtilization != nil && *ov.MemoryUtilization > 100 {
		return fmt.Errorf("%s: memoryUtilization (%d) must not exceed 100", scope, *ov.MemoryUtilization)
	}
	if ov.CoresUtilization != nil && *ov.CoresUtilization > 100 {
		return fmt.Errorf("%s: coresUtilization (%d) must not exceed 100", scope, *ov.CoresUtilization)
	}
	if ov.MemoryUsage != nil {
		dst.MemoryUsage = *ov.MemoryUsage
		if ov.MemoryUtilization == nil {
			dst.MemoryUtilization = device.CalculateUtilization(dst.MemoryUsage, totalMem)
		}
	}
	if ov.MemoryUtilization != nil {
		dst.MemoryUtilization = *ov.MemoryUtilization
	}
	if ov.CoresUtilization != nil {
		dst.CoresUtilization = *ov.CoresUtilization
	}
	if ov.Temperature != nil {
		dst.Temperature = *ov.Temperature
	}
	if ov.PowerUsage != nil {
		dst.PowerUsage = *ov.PowerUsage
	}
	return nil
}

func maxPhysicalProfileCount(profiles []device.AcceleratorPhysicalSlicedProfile) int32 {
	var ceiling int32
	for _, p := range profiles {
		if p.Count > ceiling {
			ceiling = p.Count
		}
	}
	return ceiling
}

func readBoundedSpecFile(filePath string) ([]byte, string, os.FileInfo, error) {
	resolved, err := resolveRegularFile(filePath)
	if err != nil {
		return nil, "", nil, err
	}
	raw, info, err := readFileCapped(resolved, maxSpecFileBytes)
	if err != nil {
		return nil, "", nil, err
	}
	return raw, resolved, info, nil
}
