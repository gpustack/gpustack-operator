package detector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/procattr"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// MonitorSliceSchemaVersion is the version of the slice section's schema, and
// MonitorSliceSchemaVersionMin is the oldest one this consumer still reads.
//
// The version exists so that a consumer can tell version skew from unsupported hardware. Without it,
// a newer consumer reading an older device manager sees a valid, fresh snapshot carrying no slice
// data and has no way to know whether this node's accelerators cannot be measured or the producer
// simply predates the feature — and reporting the first as the second is exactly the "absent read as
// idle" error the whole section exists to prevent.
//
// The consumer accepts a RANGE rather than its own version alone, because the two run in separate
// Pods upgraded at separate times: during a rollout a new worker reads an old device manager, and
// refusing that whole section would throw away the figures the old producer does serve in order to
// report a newer kind it never wrote. So each version is added here when a consumer can read it as
// what it is, and a version outside the range answers nothing — which is the honest claim about a
// section whose meaning this build cannot know.
//
// Version 1 carries the per-process usages, the device diagnostics and the hardware partitions.
const (
	MonitorSliceSchemaVersion    = 1
	MonitorSliceSchemaVersionMin = 1
)

// KnownMonitorSliceSchema reports whether a section's schema version is one this build reads.
func KnownMonitorSliceSchema(version int) bool {
	return version >= MonitorSliceSchemaVersionMin && version <= MonitorSliceSchemaVersion
}

const (
	// maxSliceUsages bounds how many records the section carries, so a node with an implausible
	// number of Pods, containers or devices cannot grow a snapshot without limit. It is orders of
	// magnitude above a real node: every container of every Pod on a maximally packed node,
	// holding every one of its accelerators.
	maxSliceUsages = 4096
	// maxSliceDevices bounds the diagnostics, one entry per accelerator the node holds.
	maxSliceDevices = 256
	// monitorSnapshotReadLimitBytes is the size the worker will read a snapshot up to
	// (_MonitorSnapshotMaxBytes, pkg/worker/extensionapis/worker/instance.metrics.go). It is
	// restated here because the bound above is only meaningful against it, and a test asserts a
	// bound-full section stays well inside it rather than assuming it from typical node sizes.
	monitorSnapshotReadLimitBytes = 4 << 20
)

const (
	// sliceReasonInvalidData means the vendor's own rows contradict the device they came from —
	// a sum above the card's physical capacity — so the figure is refused rather than published
	// as a number nothing can be computed from, and rather than silently zeroed.
	sliceReasonInvalidData device.AcceleratorProcessReason = "invalid_data"
	// sliceReasonBounded means the device's records were dropped to keep the section inside its
	// bound. Dropping the whole device rather than some of its records is what keeps a missing
	// record from being read as a measured zero.
	sliceReasonBounded device.AcceleratorProcessReason = "bounded"
	// SliceReasonVersionSkew means the producer wrote a section this consumer cannot read as what it
	// is. It is the reason a consumer publishes for every figure of every device in that case, and it
	// exists so that the one state the schema version was added to make visible is visible: without
	// it, a consumer newer than the device manager it reads is indistinguishable — on both surfaces —
	// from a node whose hardware cannot be measured.
	//
	// Exported, unlike its siblings, because the consumer rather than the producer is what discovers
	// it: nothing in a section can carry the verdict that the section is unreadable.
	SliceReasonVersionSkew device.AcceleratorProcessReason = "version_skew"
)

// MonitorSliceSection is the per-Instance share of every accelerator the node measured this pass,
// keyed by (Pod UID, container name, device ID).
//
// It is a section of its own rather than a field inside device.AcceleratorMetrics, which stays what
// it is: one card's readings. The raw per-process rows this is aggregated from never appear here —
// they name host processes, and both the privacy boundary and the section's size bound depend on
// them staying producer-internal.
//
// A consumer must not read the absence of a record as an absence of usage. A record is emitted only
// for a container that held processes; a container with none, on a device Devices reports as
// measured, used nothing — which is zero. Figures makes that decision once, so both surfaces make
// it identically.
type MonitorSliceSection struct {
	// SchemaVersion is MonitorSliceSchemaVersion as of the producer that wrote the section. A
	// consumer that does not know the version must report every slice figure absent with a
	// version-skew reason rather than treating the section as empty; one it knows but that predates
	// a kind of record reads the records that version carries and reports the rest absent.
	SchemaVersion int `json:"schemaVersion"`
	// Truncated reports that the bound was reached and some devices were dropped whole. Those
	// devices carry their reason in Devices, so a dropped device reads as absent rather than idle.
	Truncated bool `json:"truncated,omitempty"`
	// Usages holds one record per (Pod UID, container name, device ID) that was measured.
	Usages []SliceUsage `json:"usages"`
	// Devices holds one entry per accelerator the per-process pass covered, whether or not any
	// record came out of it. An accelerator absent from here was not covered at all.
	Devices []SliceDeviceDiagnostics `json:"devices"`
	// Partitions holds one record per hardware partition an Instance holds, read on the partition's
	// own device handle. It is a list of its own rather than a field on Usages because it comes from
	// a different source and survives that source's failures independently — see Figures.
	Partitions []SlicePartition `json:"partitions,omitempty"`
}

// SliceUsage is one Instance container's measured share of one accelerator.
//
// The two figures are pointers because absent and zero are different claims: absent means the
// figure was not measurable here, zero means it was measured and the container was idle.
type SliceUsage struct {
	// Manufacturer, PodUID, Container and DeviceID are the key. Container is the container's NAME
	// rather than its runtime ID: the allocation a consumer joins this against is recorded per
	// container name, and the runtime ID — like the process ids behind it — stays on the node.
	Manufacturer string `json:"manufacturer"`
	PodUID       string `json:"podUID"`
	Container    string `json:"container"`
	DeviceID     string `json:"deviceID"`

	// MemoryUsedMiB is the device memory this container's processes hold on this device, summed
	// in the vendor's native unit and converted once. Absent when the manufacturer served no
	// usable figure for it.
	MemoryUsedMiB *uint64 `json:"memoryUsedMiB,omitempty"`
	// CoresUtilizationPercent is the share of the WHOLE card's compute this container's processes
	// were last measured using. Absent when the manufacturer serves no per-process utilization.
	CoresUtilizationPercent *uint32 `json:"coresUtilizationPercent,omitempty"`
}

// SlicePartition is one Instance container's hardware partition of one accelerator, measured on the
// partition's OWN device handle.
//
// A partition is not aggregated from the parent card's processes, and that is what makes this a
// record rather than a SliceUsage: the partition is named by what the allocation recorded — a profile
// and a placement — so an idle partition is still a partition that can be found and reported as zero,
// and a process another tenant holds on a sibling partition cannot poison it.
type SlicePartition struct {
	// Manufacturer, PodUID, Container and DeviceID are the key, the same one SliceUsage carries.
	// DeviceID is the PARENT accelerator's, which is what the allocation records and what a consumer
	// joins by.
	Manufacturer string `json:"manufacturer"`
	PodUID       string `json:"podUID"`
	Container    string `json:"container"`
	DeviceID     string `json:"deviceID"`

	// ID is the partition's OWN identifier, as its handle reports it. A consumer reports the partition
	// under this rather than under DeviceID above, because the partition is what its holder was
	// granted — and because two partitions of one card held by one Instance are one entry under the
	// card's identifier and two under theirs. Empty when the handle could not be reached.
	ID string `json:"id,omitempty"`

	// MemoryTotalMiB and MemoryUsedMiB are the partition's own capacity and what is held of it, both
	// read on its handle and converted once. The capacity comes from the driver rather than from the
	// profile name because the name rounds: a "1g.10gb" partition carries 9856 MiB, not 10240. Absent
	// when the handle could not be read; used is zero when it was read and the partition is idle.
	MemoryTotalMiB *uint64 `json:"memoryTotalMiB,omitempty"`
	MemoryUsedMiB  *uint64 `json:"memoryUsedMiB,omitempty"`

	// CoresPercent is the partition's own compute utilization, read on its handle. Absent on every
	// manufacturer whose library serves no per-partition compute figure, which is most of them; see
	// device.AcceleratorPartition.CoresPercent.
	CoresPercent *uint32 `json:"coresPercent,omitempty"`

	// MemoryReason names why the memory figures above are absent, and is empty when they were
	// produced. CoresReason names why CoresPercent is absent, and is likewise empty when it was read.
	MemoryReason device.AcceleratorProcessReason `json:"memoryReason,omitempty"`
	CoresReason  device.AcceleratorProcessReason `json:"coresReason,omitempty"`
}

// SliceDeviceDiagnostics explains one accelerator's sample: why a figure is absent, and what the
// pass saw. Without the counts an aggregate-only section makes an undercount indistinguishable from
// a genuinely idle device, which is the one confusion this feature cannot afford.
type SliceDeviceDiagnostics struct {
	Manufacturer string `json:"manufacturer"`
	DeviceID     string `json:"deviceID"`

	// MemoryReason and CoresReason name why that figure is absent on EVERY record of this device,
	// and are empty when the figure was produced. They are separate because a driver commonly
	// serves process memory while refusing process utilization.
	MemoryReason device.AcceleratorProcessReason `json:"memoryReason,omitempty"`
	CoresReason  device.AcceleratorProcessReason `json:"coresReason,omitempty"`

	// RowsReturned is how many per-process rows the manufacturer returned for this device.
	RowsReturned uint32 `json:"rowsReturned"`
	// RowsAttributed is how many of them were resolved to an Instance's Pod and container.
	RowsAttributed uint32 `json:"rowsAttributed"`
	// RowsNonInstance is how many belonged to a Pod the node runs that backs no Instance. Their
	// usage is real and nobody's slice, so dropping them leaves the device measurable.
	RowsNonInstance uint32 `json:"rowsNonInstance"`
	// RowsUnreadable is how many could not be read from /proc at all, or whose identity moved
	// while it was read: exited, permission, unreadable, zombie, unstable, invisible.
	RowsUnreadable uint32 `json:"rowsUnreadable"`
	// RowsAmbiguous is how many were read but could not be turned into one known Pod and
	// container: a host process, two Pod ancestors, a Pod or container the node's index does not
	// carry, a process holding the device on others' behalf.
	RowsAmbiguous uint32 `json:"rowsAmbiguous"`
	// ReadsTruncated is how many of this device's entry points could not deliver a complete row
	// list. A truncated list read as a complete one turns processes that exist into processes
	// that do not, so it makes its figure absent rather than partial.
	ReadsTruncated uint32 `json:"readsTruncated"`
}

// SliceFigures is what a consumer publishes for one (Pod, container, device): each figure either
// measured — possibly zero — or absent.
type SliceFigures struct {
	MemoryUsedMiB *uint64

	// CoresUtilizationPercent is a share of the WHOLE card in every mode but Partitioned, where it is
	// the partition's own — read on the partition's handle, which knows nothing of the parent. The
	// consumer restates the former against the holder's cap and leaves the latter alone, since a
	// partition makes no compute request and so carries no cap to restate against.
	CoresUtilizationPercent *uint32

	// ID and MemoryTotalMiB describe what was measured rather than what it was measured using, and
	// they are here for the one case where the producer is what knows them: a hardware partition
	// carries its own identifier and its own capacity, both read off its handle. Empty and absent for
	// every other mode, whose identity is the accelerator asked about and whose quota the consumer
	// reads off the allocation.
	ID             string
	MemoryTotalMiB *uint64
}

// Figures resolves one container's figures on one device, and reports whether the section can
// answer for that device at all.
//
// It holds the absent-versus-zero decision so that both consuming surfaces make it identically: a
// device the section does not carry answers nothing; a device it carries with no record for this
// container answers zero for each figure the device measured, because a container that held no
// process on a measured device used none of it.
//
// A PARTITION'S FIGURES ARE RESOLVED INDEPENDENTLY OF THE PER-PROCESS PASS, and that is deliberate
// rather than an oversight in the rule above. They come from a source of their own — the partition's
// own device handle — so a failure in the manufacturer's per-process query says nothing about them: a
// row nobody could attribute poisons memory and cores for the whole device and must not take a
// partition with it, and on a MIG-enabled card the per-process query is precisely the one that cannot
// answer. So a partition record alone is enough to make this an answer.
//
// A partition record supersedes the per-process pass for its own key rather than merging with it. The
// two measure the same memory on two different handles, so a per-field mixture would publish a figure
// nobody can recompute; the partition's own handle is the narrower and more reliable of the two, and
// it is the only one that survives a sibling tenant's unattributable process.
func (s *MonitorSliceSection) Figures(
	manufacturer, podUID, container, deviceID string,
) (SliceFigures, bool) {
	if s == nil {
		return SliceFigures{}, false
	}
	if !KnownMonitorSliceSchema(s.SchemaVersion) {
		// The API carries no reason field, by design, so this surface's share of making the skew
		// discoverable is saying it once per pass rather than going quiet: an operator comparing a
		// console's empty figures against the exporter's version_skew gauge has both halves of the
		// same story, and neither is a guess about the hardware.
		logger.V(2).Info("a device manager wrote a slice section this build cannot read,"+
			" reporting no slice figures", "reason", SliceReasonVersionSkew,
			"sectionVersion", s.SchemaVersion, "supported", MonitorSliceSchemaVersion,
			"manufacturer", manufacturer, "device", deviceID)
		return SliceFigures{}, false
	}

	figures := SliceFigures{}
	if p := slices.IndexFunc(s.Partitions, func(p SlicePartition) bool {
		return p.Manufacturer == manufacturer && p.PodUID == podUID &&
			p.Container == container && p.DeviceID == deviceID
	}); p >= 0 {
		figures.ID = s.Partitions[p].ID
		figures.MemoryTotalMiB = s.Partitions[p].MemoryTotalMiB
		figures.MemoryUsedMiB = s.Partitions[p].MemoryUsedMiB
		// A partition's compute is already stated against the partition, so it travels as-is: the
		// consumer's cap-relative restatement is a no-op for a mode that makes no compute request.
		// Absent on every manufacturer whose library serves no per-partition figure.
		figures.CoresUtilizationPercent = s.Partitions[p].CoresPercent
		return figures, true
	}

	i := slices.IndexFunc(s.Devices, func(d SliceDeviceDiagnostics) bool {
		return d.Manufacturer == manufacturer && d.DeviceID == deviceID
	})
	if i < 0 {
		return figures, false
	}
	diag := s.Devices[i]

	j := slices.IndexFunc(s.Usages, func(u SliceUsage) bool {
		return u.Manufacturer == manufacturer && u.PodUID == podUID &&
			u.Container == container && u.DeviceID == deviceID
	})
	if j >= 0 {
		figures.MemoryUsedMiB = s.Usages[j].MemoryUsedMiB
		figures.CoresUtilizationPercent = s.Usages[j].CoresUtilizationPercent
		return figures, true
	}

	if diag.MemoryReason == device.AcceleratorProcessReasonNone {
		figures.MemoryUsedMiB = ptr.To[uint64](0)
	}
	if diag.CoresReason == device.AcceleratorProcessReasonNone {
		figures.CoresUtilizationPercent = ptr.To[uint32](0)
	}
	return figures, true
}

// containerNames maps a Pod UID and one of its container runtime IDs to that container's name. The
// vendor rows resolve to a runtime ID and the allocations are recorded per name, so the join happens
// here — on the node, where both are known — and only the name travels.
type containerNames map[string]map[string]string

// collectSlices runs the per-process pass of one monitor tick and returns the snapshot's slice
// section, or nil when the pass produced nothing to say.
//
// nil is not an empty section, and the difference matters: an empty section claims the node was
// measured and nothing was found, while nil says this pass could not measure. So a failed Pod list —
// which the first ticks can hit, before the informer this reads through has started — yields nil and
// the next tick recovers.
//
// The raw rows exist only as local values here. They cannot reach the snapshot because they are
// never in it: the section this returns carries per-(Pod, container, device) aggregates and nothing
// else, so stripping them is not a step that can be forgotten.
func (d *Detector) collectSlices(
	ctx context.Context, metricsGrpList device.MetricsGroupList,
) *MonitorSliceSection {
	// A manufacturer whose library answers neither question does not implement either interface. When
	// none of them implements either there is nothing to read, and the Pods are not listed at all.
	providers := make(map[string]device.AcceleratorProcessDetector, len(d.detectors))
	partitioners := make(map[string]device.AcceleratorPartitionDetector, len(d.detectors))
	for i := range d.detectors {
		if pd, ok := d.detectors[i].(device.AcceleratorProcessDetector); ok {
			providers[d.detectors[i].Name()] = pd
		}
		if pd, ok := d.detectors[i].(device.AcceleratorPartitionDetector); ok {
			partitioners[d.detectors[i].Name()] = pd
		}
	}
	if (len(providers) == 0 && len(partitioners) == 0) || d.podLister == nil {
		return nil
	}

	pods, err := d.podLister(ctx)
	if err != nil {
		// The read failed, so this tick knows nothing — which is not the same as knowing that
		// nothing is carved. Both end in no section, and they must stay distinct paths: a failed
		// read that produced an EMPTY section would claim the node was measured and found bare.
		logger.V(2).Error(err, "listing this node's pods, reporting no slice figures")
		return nil
	}

	// Only a device some container holds under a carved mode is worth querying: a whole-device
	// allocation has no slice to report, and an idle sliceable card should cost nothing.
	targets := carvedAcceleratorsOf(pods)
	if len(targets) == 0 {
		logger.V(4).Info("no carved accelerator allocation on this node, no slice figures to take")
		return nil
	}

	var procGroups []device.AcceleratorProcessesGroup
	for manufacturer, deviceIDs := range targets {
		pd, ok := providers[manufacturer]
		if !ok || d.procResolver == nil {
			continue
		}

		logger := logger.V(3).WithValues("manufacturer", manufacturer)
		grp, err := pd.MonitorAcceleratorProcesses(d.noPCICheck, deviceIDs)
		if err != nil {
			logger.Error(err, "monitor accelerator processes")
			continue
		}
		if len(grp.Accelerators) == 0 {
			continue
		}
		procGroups = append(procGroups, grp)
	}

	// The partitions are read on their own handles, and their tenant is known before the read rather
	// than resolved from a process afterwards. So this pass needs no /proc at all, and a partition an
	// Instance holds is reported whether or not anything is running inside it.
	partitions := d.readPartitions(pods, partitioners)

	var section *MonitorSliceSection
	if len(procGroups) > 0 {
		index, names := podIndexOf(pods)
		section = buildSliceSection(procGroups, metricsGrpList, names,
			func(pids []uint32) map[uint32]procattr.Result {
				return d.procResolver.Resolve(index, pids)
			})
	}
	return withPartitions(section, partitions, cardMemoryOf(metricsGrpList))
}

// partitionTarget is one hardware partition an Instance container holds, as the allocation recorded
// it: the request to make of the manufacturer, and the tenant its answer belongs to.
type partitionTarget struct {
	podUID    string
	container string
	request   device.AcceleratorPartitionRequest
}

// readPartitions asks each manufacturer that answers for a partition's own handle about every
// partition this node's Pods hold, and returns the records keyed by tenant.
//
// The tenant comes from the allocation rather than from the answer, which is the whole difference
// between this pass and the per-process one: the driver is asked about a geometry, so an answer is
// charged to the container the annotation says holds that geometry and to nobody else.
func (d *Detector) readPartitions(
	pods []core.Pod, partitioners map[string]device.AcceleratorPartitionDetector,
) []partitionRow {
	if len(partitioners) == 0 {
		return nil
	}

	var rows []partitionRow
	for manufacturer, targets := range partitionTargetsOf(pods) {
		pd, ok := partitioners[manufacturer]
		if !ok {
			continue
		}

		logger := logger.V(3).WithValues("manufacturer", manufacturer)
		requests := make([]device.AcceleratorPartitionRequest, 0, len(targets))
		for i := range targets {
			requests = append(requests, targets[i].request)
		}
		grp, err := pd.MonitorAcceleratorPartitions(d.noPCICheck, requests)
		if err != nil {
			logger.Error(err, "monitor accelerator partitions")
			continue
		}

		for i := range targets {
			j := slices.IndexFunc(grp.Partitions, func(p device.AcceleratorPartition) bool {
				return p.DeviceID == targets[i].request.DeviceID &&
					p.Profile == targets[i].request.Profile &&
					slices.Equal(p.Placements, targets[i].request.Placements)
			})
			if j < 0 {
				// The interface promises an answer per request, so a missing one is a manufacturer
				// adapter defect rather than a state of the hardware. It is dropped rather than
				// reported as absent-with-a-reason, because the reason would be ours and not the
				// node's — and a consumer that sees no record leaves the figures absent anyway.
				logger.Info("a manufacturer answered no partition for a request, reporting nothing for it",
					"device", targets[i].request.DeviceID, "profile", targets[i].request.Profile)
				continue
			}
			rows = append(rows, partitionRow{
				manufacturer: manufacturer,
				podUID:       targets[i].podUID,
				container:    targets[i].container,
				partition:    grp.Partitions[j],
			})
		}
	}
	return rows
}

// partitionTargetsOf returns, per manufacturer, every hardware partition this node's Pods hold.
//
// A partition is recorded on the Pod's own allocation annotation as a profile name and the placement
// it occupies on the parent accelerator (AllocatedPhysicalProfile / AllocatedPhysicalPlacements). A
// record missing either is not a partition this can name: the two together are what distinguishes one
// tenant's partition from another's on the same card, so an incomplete record is skipped rather than
// matched loosely.
//
// A partition two containers claim is dropped for BOTH of them. One partition has one tenant, so
// records naming two are records at least one of which is wrong, and nothing here can tell which:
// answering both would charge one tenant's memory to another, which is the misattribution this whole
// pass exists to refuse. It is the same refusal the consumer's own join makes for a device two
// containers of one Pod carved, made here because only this pass sees across Pods.
func partitionTargetsOf(pods []core.Pod) map[string][]partitionTarget {
	targets := make(map[string][]partitionTarget)
	for i := range pods {
		podUID := string(pods[i].UID)
		if podUID == "" {
			continue
		}
		allocations, err := deviceplugin.AllocatedAcceleratorsOf(&pods[i])
		if err != nil {
			// Already logged where the same annotation is read for the per-process pass.
			continue
		}

		for container, allocation := range allocations {
			for _, grp := range allocation.Devices.Groups {
				for _, accelerator := range grp.Accelerators {
					if accelerator.Mode != workercore.DeviceAllocationModePartitioned ||
						accelerator.AllocatedPhysicalProfile == "" ||
						len(accelerator.AllocatedPhysicalPlacements) == 0 {
						continue
					}
					targets[grp.Manufacturer] = append(targets[grp.Manufacturer], partitionTarget{
						podUID:    podUID,
						container: container,
						request: device.AcceleratorPartitionRequest{
							DeviceID: accelerator.ID,
							ID:       accelerator.AllocatedPhysicalID,
							Profile:  accelerator.AllocatedPhysicalProfile,
							// device.AcceleratorPlacement is an alias of the recorded type, so the
							// placements travel as they were recorded and nothing can rewrite them
							// on the way.
							Placements: accelerator.AllocatedPhysicalPlacements,
						},
					})
				}
			}
		}
	}
	for manufacturer, claimed := range targets {
		if kept := singlyClaimedPartitions(manufacturer, claimed); len(kept) > 0 {
			targets[manufacturer] = kept
		} else {
			delete(targets, manufacturer)
		}
	}
	return targets
}

// singlyClaimedPartitions keeps the partitions exactly one container claims.
func singlyClaimedPartitions(manufacturer string, claimed []partitionTarget) []partitionTarget {
	claimants := make(map[string]int, len(claimed))
	key := func(t partitionTarget) string {
		return t.request.DeviceID + "\x00" + t.request.Profile + "\x00" +
			fmt.Sprint(t.request.Placements)
	}
	for _, target := range claimed {
		claimants[key(target)]++
	}

	kept := make([]partitionTarget, 0, len(claimed))
	for _, target := range claimed {
		if claimants[key(target)] > 1 {
			logger.Info("one hardware partition is claimed by more than one container, reporting nothing for it",
				"manufacturer", manufacturer, "device", target.request.DeviceID,
				"profile", target.request.Profile, "pod", target.podUID,
				"container", target.container)
			continue
		}
		kept = append(kept, target)
	}
	return kept
}

// partitionRow is one manufacturer's answer about one partition, with the tenant it belongs to. It is
// the raw read, before the conversion and the plausibility check withPartitions applies.
type partitionRow struct {
	manufacturer string
	podUID       string
	container    string
	partition    device.AcceleratorPartition
}

// withPartitions folds the partition reads into the section, converting each memory figure to MiB
// once and dropping one the parent card contradicts.
//
// The section is created when the per-process pass produced none, for the same reason a charge creates
// one: a partition's figures are an answer on their own, and on a MIG-enabled card the per-process
// query is the one that cannot answer.
func withPartitions(
	section *MonitorSliceSection, rows []partitionRow, cardMemory map[string]uint64,
) *MonitorSliceSection {
	if len(rows) == 0 {
		return section
	}
	if section == nil {
		section = &MonitorSliceSection{SchemaVersion: MonitorSliceSchemaVersion}
	}

	for _, row := range rows {
		if len(section.Partitions) >= maxSliceUsages {
			section.Truncated = true
			break
		}
		partition := SlicePartition{
			Manufacturer: row.manufacturer,
			PodUID:       row.podUID,
			Container:    row.container,
			DeviceID:     row.partition.DeviceID,
			ID:           row.partition.ID,
			MemoryReason: row.partition.MemoryReason,
			CoresReason:  row.partition.CoresReason,
		}
		// Carried through unchanged. It has no whole-card figure to be sanity-checked against the way
		// the memory pair below does -- a percentage cannot exceed the card by being large -- and it
		// comes off the partition's own handle, so a manufacturer that serves it has already scoped it.
		if row.partition.CoresPercent != nil {
			partition.CoresPercent = ptr.To(*row.partition.CoresPercent)
		}
		if row.partition.MemoryUsedBytes != nil && row.partition.MemoryTotalBytes != nil {
			var (
				totalMiB      = convertUsageToMiB(*row.partition.MemoryTotalBytes)
				usedMiB       = convertUsageToMiB(*row.partition.MemoryUsedBytes)
				cardMemoryMiB = cardMemory[row.manufacturer+"/"+row.partition.DeviceID]
			)
			switch {
			case cardMemoryMiB > 0 && (totalMiB > cardMemoryMiB || usedMiB > cardMemoryMiB):
				// A partition larger than, or holding more than, the card it was carved from is a read
				// that cannot be believed, exactly as an over-capacity per-process sum is. Both figures
				// go: they came off one handle, so one of them being impossible discredits the read
				// rather than just the field.
				logger.V(2).Info("a partition exceeds its whole accelerator, dropping the figures",
					"manufacturer", row.manufacturer, "device", row.partition.DeviceID,
					"pod", row.podUID, "container", row.container,
					"totalMiB", totalMiB, "usedMiB", usedMiB, "cardMiB", cardMemoryMiB)
				partition.MemoryReason = sliceReasonInvalidData
			default:
				partition.MemoryTotalMiB = ptr.To(totalMiB)
				partition.MemoryUsedMiB = ptr.To(usedMiB)
			}
		}
		section.Partitions = append(section.Partitions, partition)
	}

	slices.SortFunc(section.Partitions, func(a, b SlicePartition) int {
		return strings.Compare(
			a.Manufacturer+"\x00"+a.DeviceID+"\x00"+a.PodUID+"\x00"+a.Container,
			b.Manufacturer+"\x00"+b.DeviceID+"\x00"+b.PodUID+"\x00"+b.Container)
	})
	return section
}

// carvedAcceleratorsOf returns the accelerators this node's Pods hold under a carved allocation
// mode, keyed by manufacturer.
//
// Both carved modes count. A partition whose manufacturer answers for its own handle is measured by
// the partition pass and its record supersedes this one's, but a manufacturer that partitions in
// hardware without serving a per-partition query has the per-process path and nothing else — so
// gating on the soft mode alone would leave those cards unqueried. Every other mode is a whole-device
// allocation, and Shared, which is neither carved nor whole, carries no quota a slice figure could be
// read against.
//
// The allocation is read from the Pod's own annotation, the durable per-container record the device
// plugin writes, so this costs no read beyond the Pod list the attribution index already needs.
func carvedAcceleratorsOf(pods []core.Pod) map[string]sets.Set[string] {
	targets := make(map[string]sets.Set[string])
	for i := range pods {
		allocations, err := deviceplugin.AllocatedAcceleratorsOf(&pods[i])
		if err != nil {
			// A Pod whose annotation cannot be read names no device here. It is not treated as
			// holding nothing either: what it holds is simply unknown, and the devices its
			// siblings name are still queried.
			logger.V(2).Error(err, "reading a pod's allocated accelerators",
				"pod", pods[i].Namespace+"/"+pods[i].Name)
			continue
		}

		for _, allocation := range allocations {
			for _, grp := range allocation.Devices.Groups {
				for _, accelerator := range grp.Accelerators {
					if !carvedAllocationMode(accelerator.Mode) {
						continue
					}
					if targets[grp.Manufacturer] == nil {
						targets[grp.Manufacturer] = sets.New[string]()
					}
					targets[grp.Manufacturer].Insert(accelerator.ID)
				}
			}
		}
	}
	return targets
}

// carvedAllocationMode reports whether an allocation holds a share of an accelerator rather than
// the whole of it.
func carvedAllocationMode(mode workercore.DeviceAllocationMode) bool {
	switch mode {
	case workercore.DeviceAllocationModeSliced, workercore.DeviceAllocationModePartitioned:
		return true
	}
	return false
}

// listNodePods lists the Pods of this node through the informer the device plugin already runs for
// it, so the pass costs a cache lookup rather than a List against the API server every period. It
// is the same read the metrics exporter performs (pkg/devicemanager/exporter/poller.go).
func (d *Detector) listNodePods(ctx context.Context) ([]core.Pod, error) {
	reader := system.LoopbackCtrlClient.Get()
	if reader == nil {
		return nil, errors.New("loopback controller client is not configured yet")
	}
	ndName := osx.Getenv("KUBERNETES_NODE_NAME")
	if ndName == "" {
		return nil, errors.New("environment variable KUBERNETES_NODE_NAME is not set")
	}

	podList := &core.PodList{}
	err := reader.List(ctx, podList,
		ctrlcli.MatchingFieldsSelector{
			Selector: fields.OneTermEqualSelector(deviceplugin.IndexingPodsByNodeName, ndName),
		},
	)
	if err != nil {
		return nil, err
	}
	return podList.Items, nil
}

// buildSliceSection aggregates every manufacturer's per-process rows into the section.
//
// resolve is the attribution of one device's process ids, closing over the node's Pod index. It is
// called per device rather than once for the node: a process id appearing on two devices is
// attributed independently for each, and the resolver's own "the vendor reported ids from a
// namespace this reader cannot see" verdict is only meaningful over one device's row set.
func buildSliceSection(
	procGroups []device.AcceleratorProcessesGroup,
	metricsGrpList device.MetricsGroupList,
	names containerNames,
	resolve func(pids []uint32) map[uint32]procattr.Result,
) *MonitorSliceSection {
	cardMemory := cardMemoryOf(metricsGrpList)

	section := &MonitorSliceSection{SchemaVersion: MonitorSliceSchemaVersion}
	for _, procGrp := range procGroups {
		manufacturer := procGrp.Manufacturer
		for _, procs := range procGrp.Accelerators {
			usages, diag := aggregateAcceleratorProcesses(
				manufacturer, procs, cardMemory[manufacturer+"/"+procs.ID],
				names, resolve)

			// The bound drops a device whole, never some of its records: a record missing from a
			// device the section still reports as measured would read as a measured zero.
			switch {
			case len(section.Devices) >= maxSliceDevices:
				section.Truncated = true
				continue
			case len(section.Usages)+len(usages) > maxSliceUsages:
				section.Truncated = true
				diag.MemoryReason = sliceReasonBounded
				diag.CoresReason = sliceReasonBounded
				usages = nil
			}
			section.Usages = append(section.Usages, usages...)
			section.Devices = append(section.Devices, diag)
		}
	}
	if len(section.Devices) == 0 {
		return nil
	}

	slices.SortFunc(section.Usages, func(a, b SliceUsage) int {
		return strings.Compare(
			a.Manufacturer+"\x00"+a.DeviceID+"\x00"+a.PodUID+"\x00"+a.Container,
			b.Manufacturer+"\x00"+b.DeviceID+"\x00"+b.PodUID+"\x00"+b.Container)
	})
	slices.SortFunc(section.Devices, func(a, b SliceDeviceDiagnostics) int {
		return strings.Compare(a.Manufacturer+"\x00"+a.DeviceID, b.Manufacturer+"\x00"+b.DeviceID)
	})
	return section
}

// cardMemoryOf indexes the accelerators' own capacities from the same monitor pass, keyed by
// manufacturer and device ID. It is what makes a figure above physical capacity recognizable as
// invalid rather than publishable, for the vendor's rows and for a shim ledger alike.
func cardMemoryOf(metricsGrpList device.MetricsGroupList) map[string]uint64 {
	cardMemory := make(map[string]uint64)
	for _, grp := range metricsGrpList {
		for _, am := range grp.Accelerators {
			cardMemory[grp.Manufacturer+"/"+am.ID] = am.Memory
		}
	}
	return cardMemory
}

// aggregateAcceleratorProcesses turns one accelerator's per-process rows into per-(Pod, container)
// records and the diagnostics that explain them. The Pod index is not taken here: it is what resolve
// closes over, so which Pods the node runs is decided once per pass rather than once per device.
func aggregateAcceleratorProcesses(
	manufacturer string,
	procs device.AcceleratorProcesses,
	cardMemoryMiB uint64,
	names containerNames,
	resolve func(pids []uint32) map[uint32]procattr.Result,
) ([]SliceUsage, SliceDeviceDiagnostics) {
	acc := newSliceAccumulator(manufacturer, procs, cardMemoryMiB)

	pids := make([]uint32, 0, len(procs.Processes))
	for i := range procs.Processes {
		pids = append(pids, procs.Processes[i].PID)
	}
	results := resolve(pids)

	for i := range procs.Processes {
		row := &procs.Processes[i]
		result := results[row.PID]
		switch result.Outcome {
		case procattr.OutcomeExcluded:
			acc.exclude()
		case procattr.OutcomeAttributed:
			// A container whose name the Pod's status does not carry cannot be keyed, and
			// guessing one would charge a measurement to the wrong allocation.
			name := names[result.Identity.PodUID][result.Identity.ContainerID]
			if name == "" {
				acc.poison(device.AcceleratorProcessReason(procattr.ReasonUnknownContainer))
				continue
			}
			acc.add(sliceKey{podUID: result.Identity.PodUID, container: name}, row)
		default:
			// The row belongs to nobody this pass could name. Every figure on this device goes
			// with it: a sum missing one of its terms is not a smaller sum, it is a wrong one.
			acc.poison(device.AcceleratorProcessReason(result.Reason))
		}
	}

	return acc.emit()
}

// sliceKey is one record's identity within a device: which container of which Pod.
type sliceKey struct {
	podUID    string
	container string
}

// sliceEntry is one container's running sums for one device, in the vendor's native units.
type sliceEntry struct {
	// memoryBytes is the sum of the container's rows, and memoryUnusable records that one of
	// those rows carried no number — which makes the sum a partial one, and a partial sum is
	// published as absent rather than as a smaller figure.
	memoryBytes    uint64
	memoryUnusable bool
	// coresPercent is the sum of the container's per-process utilization, in whole percent of the
	// card, and coresUnusable records that one of those rows carried no figure — which makes this
	// sum partial for exactly the reason the memory one can be, and is published as absent.
	//
	// A row carrying no figure is NOT read as an idle process here. An adapter whose library reports
	// only non-zero samples says "idle" with a pointer to zero; a nil reaching this point is a row
	// the hardware could not measure, and adding nothing for it would publish a share of the card
	// that the container may well be exceeding.
	coresPercent  uint64
	coresUnusable bool
}

// sliceAccumulator builds one device's records. It is the only way a SliceUsage is produced, and
// emit is its only exit — so "an unattributable row makes every figure on this device absent" holds
// by construction rather than by every caller remembering to check a flag.
type sliceAccumulator struct {
	manufacturer  string
	deviceID      string
	cardMemoryMiB uint64

	memoryReason device.AcceleratorProcessReason
	coresReason  device.AcceleratorProcessReason
	poisoned     device.AcceleratorProcessReason

	diag    SliceDeviceDiagnostics
	order   []sliceKey
	entries map[sliceKey]*sliceEntry
}

func newSliceAccumulator(
	manufacturer string, procs device.AcceleratorProcesses, cardMemoryMiB uint64,
) *sliceAccumulator {
	acc := &sliceAccumulator{
		manufacturer:  manufacturer,
		deviceID:      procs.ID,
		cardMemoryMiB: cardMemoryMiB,
		memoryReason:  procs.MemoryReason,
		coresReason:   procs.CoresReason,
		entries:       make(map[sliceKey]*sliceEntry),
		diag: SliceDeviceDiagnostics{
			Manufacturer: manufacturer,
			DeviceID:     procs.ID,
			RowsReturned: uint32(len(procs.Processes)),
		},
	}
	for _, reason := range []device.AcceleratorProcessReason{procs.MemoryReason, procs.CoresReason} {
		if reason == device.AcceleratorProcessReasonTruncated {
			acc.diag.ReadsTruncated++
		}
	}
	return acc
}

// add folds one attributed row into its container's sums.
func (a *sliceAccumulator) add(key sliceKey, row *device.AcceleratorProcess) {
	a.diag.RowsAttributed++

	entry, ok := a.entries[key]
	if !ok {
		entry = &sliceEntry{}
		a.entries[key] = entry
		a.order = append(a.order, key)
	}

	if row.MemoryBytes == nil {
		entry.memoryUnusable = true
	} else {
		entry.memoryBytes += *row.MemoryBytes
	}
	if row.CoresPercent == nil {
		entry.coresUnusable = true
	} else {
		entry.coresPercent += uint64(*row.CoresPercent)
	}
}

// exclude records a row belonging to a Pod the node runs that backs no Instance.
func (a *sliceAccumulator) exclude() {
	a.diag.RowsNonInstance++
}

// poison records a row that could not be attributed, and takes every figure on the device with it.
// The first reason is kept: it is the one a reader should act on, and later rows are commonly the
// same refusal repeated.
func (a *sliceAccumulator) poison(reason device.AcceleratorProcessReason) {
	if unreadableAttributionReason(procattr.Reason(reason)) {
		a.diag.RowsUnreadable++
	} else {
		a.diag.RowsAmbiguous++
	}
	if a.poisoned == device.AcceleratorProcessReasonNone {
		a.poisoned = reason
	}
}

// emit produces the device's records and diagnostics. It is the accumulator's only exit: a poisoned
// device yields no record at all, and the reason it was poisoned lands on both figures.
func (a *sliceAccumulator) emit() ([]SliceUsage, SliceDeviceDiagnostics) {
	if a.poisoned != device.AcceleratorProcessReasonNone {
		a.diag.MemoryReason = a.poisoned
		a.diag.CoresReason = a.poisoned
		return nil, a.diag
	}
	a.diag.MemoryReason = a.memoryReason
	a.diag.CoresReason = a.coresReason

	// The vendor's own rows can contradict the card they came from. Summing past physical capacity
	// is not an overshoot worth publishing — an over-quota slice is, and is published unchanged —
	// it is a read that cannot be believed for any container on the device.
	var deviceMemoryMiB uint64
	for _, key := range a.order {
		deviceMemoryMiB += convertUsageToMiB(a.entries[key].memoryBytes)
	}
	if a.cardMemoryMiB > 0 && deviceMemoryMiB > a.cardMemoryMiB {
		a.diag.MemoryReason = sliceReasonInvalidData
	}

	usages := make([]SliceUsage, 0, len(a.order))
	for _, key := range a.order {
		entry := a.entries[key]
		usage := SliceUsage{
			Manufacturer: a.manufacturer,
			PodUID:       key.podUID,
			Container:    key.container,
			DeviceID:     a.deviceID,
		}
		if a.diag.MemoryReason == device.AcceleratorProcessReasonNone && !entry.memoryUnusable {
			usage.MemoryUsedMiB = ptr.To(convertUsageToMiB(entry.memoryBytes))
		}
		if a.diag.CoresReason == device.AcceleratorProcessReasonNone && !entry.coresUnusable {
			// The figure shares the whole card as its denominator, so a sum of overlapping
			// per-process samples past 100 is held at the denominator rather than published as a
			// percentage of nothing.
			usage.CoresUtilizationPercent = ptr.To(uint32(min(entry.coresPercent, 100)))
		}
		usages = append(usages, usage)
	}
	if len(usages) == 0 {
		return nil, a.diag
	}
	slices.SortFunc(usages, func(x, y SliceUsage) int {
		return strings.Compare(x.PodUID+"\x00"+x.Container, y.PodUID+"\x00"+y.Container)
	})
	return usages, a.diag
}

// convertUsageToMiB converts a native byte sum to MiB once, at the end, and keeps a non-zero sum
// non-zero: the conversion floors, so a container genuinely holding a few hundred KiB would
// otherwise be published as holding nothing.
func convertUsageToMiB(bytes uint64) uint64 {
	mib := device.ConvertBytesToMiB(bytes)
	if mib == 0 && bytes > 0 {
		return 1
	}
	return mib
}

// unreadableAttributionReason reports whether a refusal came from /proc failing to answer rather
// than from what it answered. The two are counted apart because they mean different things: the
// first is a race or a broken deployment, the second is a process that is genuinely not an
// Instance's to charge.
func unreadableAttributionReason(reason procattr.Reason) bool {
	switch reason {
	case procattr.ReasonExited, procattr.ReasonPermission, procattr.ReasonUnreadable,
		procattr.ReasonZombie, procattr.ReasonUnstable, procattr.ReasonInvisible:
		return true
	}
	return false
}

// podIndexOf builds the attribution index and the container-name join from the Pods of this node.
//
// Every Pod is carried, not only the Instances': a process belonging to a Pod that backs no
// Instance is safe to drop, while one belonging to a Pod nothing knows about makes its device
// unmeasurable — so recognizing the ordinary Pods of a node is what keeps a device measurable.
func podIndexOf(pods []core.Pod) (procattr.PodIndex, containerNames) {
	index := make(procattr.PodIndex, len(pods))
	names := make(containerNames, len(pods))
	for i := range pods {
		pod := &pods[i]
		uid := string(pod.UID)
		if uid == "" {
			continue
		}

		containers := sets.New[string]()
		for _, statuses := range [][]core.ContainerStatus{
			pod.Status.InitContainerStatuses,
			pod.Status.ContainerStatuses,
			pod.Status.EphemeralContainerStatuses,
		} {
			for j := range statuses {
				id := containerIDOf(statuses[j].ContainerID)
				if id == "" {
					continue
				}
				containers.Insert(id)
				if names[uid] == nil {
					names[uid] = make(map[string]string)
				}
				names[uid][id] = statuses[j].Name
			}
		}

		index[uid] = procattr.Pod{Instance: isInstancePod(pod), Containers: containers}
	}
	return index, names
}

// containerIDOf strips the "<runtime>://" scheme a Pod status reports a container ID with, because
// a cgroup path spells the bare id and the two have to compare directly.
func containerIDOf(id string) string {
	if _, rest, ok := strings.Cut(id, "://"); ok {
		return rest
	}
	return id
}

// isInstancePod reports whether a Pod backs an Instance.
//
// This mirrors instanceOf in pkg/devicemanager/exporter/poller.go, which resolves the same question
// for the metrics exporter and is unexported there: the controller ownerReference names the
// Instance and the part-of label repeats its UID, and both are checked because the metrics
// subresource resolves a Pod by that label. Whoever changes one of the two should change the other.
func isInstancePod(pod *core.Pod) bool {
	ref := meta.GetControllerOf(pod)
	if ref == nil || ref.Kind != _InstanceKind ||
		schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind).Group != workercore.GroupName {
		return false
	}
	return pod.Labels[deviceplugin.InstancePartOfLabelKey] == string(ref.UID)
}

// _InstanceKind is the owner kind an Instance Pod carries.
const _InstanceKind = "Instance"
