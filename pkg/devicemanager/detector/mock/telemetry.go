package mock

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// MonitorAcceleratorProcesses returns mock per-process rows for the requested accelerator IDs.
func (d *Detector) MonitorAcceleratorProcesses(
	_ bool, deviceIDs sets.Set[string],
) (device.AcceleratorProcessesGroup, error) {
	grp := device.AcceleratorProcessesGroup{Manufacturer: d.manufacturer}
	if deviceIDs.Len() == 0 {
		return grp, nil
	}

	d.mu.Lock()
	mfrSpec, err := d.resolveManufacturerLocked()
	d.mu.Unlock()
	if err != nil {
		return grp, err
	}
	if mfrSpec != nil {
		if err := checkSlotFaults(d.manufacturer, mfrSpec.Slots); err != nil {
			return grp, err
		}
	}

	grp.Timestamp = time.Now()
	ids := sets.List(deviceIDs)
	grp.Accelerators = make([]device.AcceleratorProcesses, 0, len(ids))

	if mfrSpec == nil || len(mfrSpec.Slots) == 0 {
		for _, id := range ids {
			grp.Accelerators = append(grp.Accelerators, unreadProcessEntry(id))
		}
		return grp, nil
	}

	queryableByID := make(map[string]bool, len(mfrSpec.Slots))
	for i := range mfrSpec.Slots {
		if mfrSpec.Slots[i].QueryState == mockspec.QueryStateOK {
			queryableByID[mfrSpec.Slots[i].Accelerator.ID] = true
		}
	}

	for _, id := range ids {
		if !queryableByID[id] {
			grp.Accelerators = append(grp.Accelerators, unreadProcessEntry(id))
			continue
		}
		if procs, ok := mfrSpec.Processes[id]; ok {
			grp.Accelerators = append(grp.Accelerators, procs)
			continue
		}
		grp.Accelerators = append(grp.Accelerators, device.AcceleratorProcesses{
			ID:        id,
			Processes: []device.AcceleratorProcess{},
		})
	}

	return grp, nil
}

// MonitorAcceleratorPartitions returns mock telemetry answers for each requested hardware partition.
func (d *Detector) MonitorAcceleratorPartitions(
	_ bool, requests []device.AcceleratorPartitionRequest,
) (device.AcceleratorPartitionsGroup, error) {
	grp := device.AcceleratorPartitionsGroup{Manufacturer: d.manufacturer}
	if len(requests) == 0 {
		return grp, nil
	}

	d.mu.Lock()
	mfrSpec, err := d.resolveManufacturerLocked()
	provider := d.partitionProvider
	d.mu.Unlock()
	if err != nil {
		return grp, err
	}
	if mfrSpec != nil {
		if err := checkSlotFaults(d.manufacturer, mfrSpec.Slots); err != nil {
			return grp, err
		}
	}

	grp.Timestamp = time.Now()
	grp.Partitions = make([]device.AcceleratorPartition, 0, len(requests))

	slotsByID := make(map[string]*mockspec.ResolvedSlot)
	if mfrSpec != nil {
		slotsByID = make(map[string]*mockspec.ResolvedSlot, len(mfrSpec.Slots))
		for i := range mfrSpec.Slots {
			slotsByID[mfrSpec.Slots[i].Accelerator.ID] = &mfrSpec.Slots[i]
		}
	}

	for _, req := range requests {
		slot := slotsByID[req.DeviceID]
		if slot == nil || slot.QueryState != mockspec.QueryStateOK {
			grp.Partitions = append(grp.Partitions, unreadPartitionEntry(
				d.manufacturer, req, device.AcceleratorProcessReasonDriverError,
			))
			continue
		}

		if mfrSpec != nil {
			if matched, ok := matchStaticPartition(d.manufacturer, req, mfrSpec.Partitions); ok {
				grp.Partitions = append(grp.Partitions, matched)
				continue
			}
		}

		if provider != nil {
			if live, ok := provider.LookupPartition(d.manufacturer, req); ok {
				grp.Partitions = append(grp.Partitions, normalizePartitionAnswer(d.manufacturer, req, live))
				continue
			}
		}

		ps := slot.Accelerator.Status.PhysicalSliced
		if ps.Count == 0 && len(ps.Profiles) == 0 {
			grp.Partitions = append(grp.Partitions, unreadPartitionEntry(
				d.manufacturer, req, device.AcceleratorProcessReasonUnsupported,
			))
			continue
		}

		grp.Partitions = append(grp.Partitions, unreadPartitionEntry(
			d.manufacturer, req, device.AcceleratorProcessReasonDriverError,
		))
	}

	return grp, nil
}

func checkSlotFaults(manufacturer string, slots []mockspec.ResolvedSlot) error {
	if len(slots) == 0 {
		return nil
	}
	queriedCount := 0
	var lastUnqueryableErr error
	for i := range slots {
		switch slots[i].QueryState {
		case mockspec.QueryStateFatalError:
			return fmt.Errorf(
				"fatal mock query error on %s index %d (%s)",
				manufacturer, slots[i].Index, slots[i].Accelerator.ID,
			)
		case mockspec.QueryStateUnqueryable:
			lastUnqueryableErr = fmt.Errorf(
				"unqueryable mock device on %s index %d (%s)",
				manufacturer, slots[i].Index, slots[i].Accelerator.ID,
			)
		default:
			queriedCount++
		}
	}
	if queriedCount == 0 {
		return lastUnqueryableErr
	}
	return nil
}

func unreadProcessEntry(id string) device.AcceleratorProcesses {
	return device.AcceleratorProcesses{
		ID:           id,
		MemoryReason: device.AcceleratorProcessReasonDriverError,
		CoresReason:  device.AcceleratorProcessReasonDriverError,
	}
}

func unreadPartitionEntry(
	manufacturer string,
	req device.AcceleratorPartitionRequest,
	memReason device.AcceleratorProcessReason,
) device.AcceleratorPartition {
	coresReason := device.AcceleratorProcessReasonUnsupported
	if manufacturer == nodefeature.ManufacturerHygon {
		coresReason = device.AcceleratorProcessReasonDriverError
	}
	return device.AcceleratorPartition{
		DeviceID:     req.DeviceID,
		Profile:      req.Profile,
		Placements:   slices.Clone(req.Placements),
		MemoryReason: memReason,
		CoresReason:  coresReason,
	}
}

func matchStaticPartition(
	manufacturer string,
	req device.AcceleratorPartitionRequest,
	candidates []device.AcceleratorPartition,
) (device.AcceleratorPartition, bool) {
	for i := range candidates {
		c := &candidates[i]
		if c.DeviceID != req.DeviceID {
			continue
		}
		switch {
		case c.ID != "" && req.ID != "":
			if c.ID != req.ID {
				continue
			}
			if c.Profile != "" && req.Profile != "" && c.Profile != req.Profile {
				continue
			}
			if len(c.Placements) > 0 && len(req.Placements) > 0 && !slices.Equal(c.Placements, req.Placements) {
				continue
			}
			return normalizePartitionAnswer(manufacturer, req, *c), true
		case c.Profile != "" && req.Profile != "":
			if c.Profile != req.Profile {
				continue
			}
			if len(c.Placements) > 0 && len(req.Placements) > 0 && !slices.Equal(c.Placements, req.Placements) {
				continue
			}
			return normalizePartitionAnswer(manufacturer, req, *c), true
		}
	}
	return device.AcceleratorPartition{}, false
}

func normalizePartitionAnswer(
	_ string,
	req device.AcceleratorPartitionRequest,
	raw device.AcceleratorPartition,
) device.AcceleratorPartition {
	placements := slices.Clone(req.Placements)
	if len(placements) == 0 && len(raw.Placements) > 0 {
		placements = slices.Clone(raw.Placements)
	}

	var memTotalPtr *uint64
	if raw.MemoryTotalBytes != nil {
		v := *raw.MemoryTotalBytes
		memTotalPtr = &v
	}
	var memUsedPtr *uint64
	if raw.MemoryUsedBytes != nil {
		v := *raw.MemoryUsedBytes
		memUsedPtr = &v
	}
	var coresPtr *uint32
	if raw.CoresPercent != nil {
		v := *raw.CoresPercent
		coresPtr = &v
	}

	id := raw.ID
	if id == "" && (memTotalPtr != nil || memUsedPtr != nil || coresPtr != nil) {
		id = req.ID
	}

	memReason := raw.MemoryReason
	if memTotalPtr == nil && memUsedPtr == nil && memReason == device.AcceleratorProcessReasonNone {
		memReason = device.AcceleratorProcessReasonDriverError
	}
	coresReason := raw.CoresReason
	if coresPtr == nil && coresReason == device.AcceleratorProcessReasonNone {
		coresReason = device.AcceleratorProcessReasonUnsupported
	}

	return device.AcceleratorPartition{
		DeviceID:         req.DeviceID,
		Profile:          cmp.Or(req.Profile, raw.Profile),
		Placements:       placements,
		ID:               id,
		MemoryTotalBytes: memTotalPtr,
		MemoryUsedBytes:  memUsedPtr,
		CoresPercent:     coresPtr,
		MemoryReason:     memReason,
		CoresReason:      coresReason,
	}
}
