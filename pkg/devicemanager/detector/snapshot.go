package detector

import (
	"time"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

// MonitorSnapshotMaxAge is the accepted age of a snapshot that does not report the period it
// was taken on, which only an older device manager produces.
const MonitorSnapshotMaxAge = 45 * time.Second

// AllocatedAcceleratorMetric is one allocated device's metrics together with what a caller needs
// in order to label or key them: the manufacturer whose group carried the metrics, and the
// device's index as the allocation records it. The index comes from the allocation rather than
// from the snapshot, which carries only the ID — it is the ordinal the manufacturer's own tools
// name the device by, so a reader can line a figure up with what they see on the host.
type AllocatedAcceleratorMetric struct {
	Manufacturer string
	Index        uint32
	Metrics      device.AcceleratorMetrics
}

// MonitorSnapshotFresh reports whether a snapshot is recent enough to report figures from.
//
// A snapshot older than three monitor periods means the monitor is failing: the loop only
// replaces the snapshot after a successful non-empty sample, so an old one is the last thing
// that worked rather than the current state. The bound scales with the period the snapshot
// reports so a slower cadence is not mistaken for a failure, falling back to
// MonitorSnapshotMaxAge when the field is absent.
func MonitorSnapshotFresh(snapshot *MonitorSnapshot) bool {
	if snapshot == nil {
		return false
	}

	maxAge := MonitorSnapshotMaxAge
	if snapshot.PeriodSeconds > 0 {
		maxAge = time.Duration(snapshot.PeriodSeconds) * time.Second * 3
	}
	return time.Since(snapshot.Timestamp) <= maxAge
}

// AllocatedAcceleratorMetricsOf filters a monitor snapshot to the devices an allocation records,
// matched by manufacturer and device ID, and yields nothing from a snapshot that is not fresh.
//
// A manufacturer the snapshot does not carry yields nothing for its devices. That is what makes
// this safe to call on any single device manager's snapshot: the chart rolls one per
// manufacturer and each samples only its own cards, so a snapshot answers for the devices it
// knows and stays silent about the rest rather than reporting them as missing.
func AllocatedAcceleratorMetricsOf(
	snapshot *MonitorSnapshot,
	allocGroups []workercore.DevicesAllocationGroup,
) []AllocatedAcceleratorMetric {
	if !MonitorSnapshotFresh(snapshot) {
		return nil
	}

	allocatedByManufacturer := make(map[string]map[string]uint32, len(allocGroups))
	for i := range allocGroups {
		indexes := allocatedByManufacturer[allocGroups[i].Manufacturer]
		if indexes == nil {
			indexes = make(map[string]uint32)
			allocatedByManufacturer[allocGroups[i].Manufacturer] = indexes
		}
		for j := range allocGroups[i].Accelerators {
			indexes[allocGroups[i].Accelerators[j].ID] = allocGroups[i].Accelerators[j].Index
		}
	}

	var metrics []AllocatedAcceleratorMetric
	for i := range snapshot.Groups {
		grp := &snapshot.Groups[i]
		allocated, ok := allocatedByManufacturer[grp.Manufacturer]
		if !ok {
			continue
		}
		for j := range grp.Accelerators {
			am := &grp.Accelerators[j]
			index, ok := allocated[am.ID]
			if !ok {
				continue
			}
			metrics = append(metrics, AllocatedAcceleratorMetric{
				Manufacturer: grp.Manufacturer,
				Index:        index,
				Metrics:      *am,
			})
		}
	}
	return metrics
}
