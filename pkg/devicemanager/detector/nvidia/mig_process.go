package nvidia

import (
	"time"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

// The partition query is reached by a type assertion on the detector, so a signature that drifts away
// from the interface would disable partition reporting silently rather than failing to build.
var _ device.AcceleratorPartitionDetector = (*nvidia)(nil)

// MonitorAcceleratorPartitions implements device.AcceleratorPartitionDetector: it reports what each
// requested MIG partition holds, read on the partition's OWN device handle.
//
// A partition is found by matching the recorded profile and placement against the instances the
// driver enumerates — never by which instance holds a process of ours. The difference is the whole
// reason this exists: an idle partition holds no process, and a process-first lookup would find no
// handle and report an absence where the truth is zero.
//
// Compute utilization is never reported. NVML serves no per-process utilization on a MIG-enabled card
// and no per-instance utilization either, so every answer carries the unsupported reason for it — an
// absence with a named cause rather than a zero nobody measured.
func (in *nvidia) MonitorAcceleratorPartitions(
	noPciCheck bool, requests []device.AcceleratorPartitionRequest,
) (_ device.AcceleratorPartitionsGroup, err error) {
	grp := device.AcceleratorPartitionsGroup{Manufacturer: Manufacturer}

	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor nvidia device partitions")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	if len(requests) == 0 {
		return grp, nil
	}

	grp.Timestamp = time.Now()

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		// An allocation names a partition on a card no NVIDIA PCI device answers for. Every request
		// is still answered — an answer carrying a reason is what keeps its absence explicable.
		in.logger.Info("no nvidia pci devices found for allocated partitions",
			"partitions", len(requests))
		grp.Partitions = unreadAcceleratorPartitions(
			requests, device.AcceleratorProcessReasonDriverError)
		return grp, nil
	}

	in.init()

	cnt, ret := in.nvml.DeviceGetCount()
	if !ret.IsSuccess() || cnt == 0 {
		in.logger.V(3).Error(ret, "failed to count devices for allocated partitions",
			"count", cnt, "partitions", len(requests))
		grp.Partitions = unreadAcceleratorPartitions(
			requests, device.AcceleratorProcessReasonDriverError)
		return grp, nil
	}

	byCard := partitionRequestsByCard(requests)
	grp.Partitions = make([]device.AcceleratorPartition, 0, len(requests))
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.nvml.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device handle")
			continue
		}

		// The identity is resolved first, so a card holding no partition of ours costs one cheap
		// call and no MIG enumeration at all.
		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device UUID")
			continue
		}
		cardRequests, ok := byCard[uuid]
		if !ok {
			continue
		}
		delete(byCard, uuid)

		partitions := resolvePartitions(cardRequests, in.migPartitionSource(dev))
		for j := range partitions {
			if partitions[j].MemoryReason != device.AcceleratorProcessReasonNone {
				logger.V(3).Info("no partition memory", "device", uuid,
					"profile", partitions[j].Profile, "reason", partitions[j].MemoryReason)
			}
		}
		grp.Partitions = append(grp.Partitions, partitions...)
	}

	// A request whose card no enumerated handle answered for — a handle or UUID call that failed, or
	// a card that has left the machine while an allocation still names a partition on it.
	for _, cardRequests := range byCard {
		grp.Partitions = append(grp.Partitions, unreadAcceleratorPartitions(
			cardRequests, device.AcceleratorProcessReasonDriverError)...)
	}

	return grp, nil
}

// partitionRequestsByCard groups the requests by the parent accelerator each one names, so a card is
// enumerated once however many partitions of it the node holds.
func partitionRequestsByCard(
	requests []device.AcceleratorPartitionRequest,
) map[string][]device.AcceleratorPartitionRequest {
	byCard := make(map[string][]device.AcceleratorPartitionRequest)
	for _, req := range requests {
		byCard[req.DeviceID] = append(byCard[req.DeviceID], req)
	}
	return byCard
}

// unreadAcceleratorPartitions answers every request with no figure and the given reason, for the
// paths that never reach a partition's handle at all.
//
// The reason a caller passes here is transient rather than unsupported wherever nothing disproves
// that the driver serves the query: a capability probe must not conclude "this node cannot do it"
// from a card it merely could not reach.
func unreadAcceleratorPartitions(
	requests []device.AcceleratorPartitionRequest, reason device.AcceleratorProcessReason,
) []device.AcceleratorPartition {
	unread := make([]device.AcceleratorPartition, 0, len(requests))
	for _, req := range requests {
		unread = append(unread, device.AcceleratorPartition{
			DeviceID:     req.DeviceID,
			Profile:      req.Profile,
			Placements:   req.Placements,
			MemoryReason: reason,
			CoresReason:  device.AcceleratorProcessReasonUnsupported,
		})
	}
	return unread
}

// migPartition is what one GPU instance's own MIG device handle reports about itself: the identity a
// consumer reports it under, and the memory pair measured on that handle.
type migPartition struct {
	id         string
	totalBytes uint64
	usedBytes  uint64
}

// migPartitionSource is the driver-facing half of the resolution. Each read is a closure so the
// matching itself stays hardware-free and unit-testable, as this package's MIG profile derivation
// already is.
//
// Every closure reports a reason beside its answer rather than an error, because that reason is
// published as the label explaining the absence it causes.
type migPartitionSource struct {
	// migEnabled reports whether the card is partitioned at all.
	migEnabled func() (bool, device.AcceleratorProcessReason)
	// partitionByID reads the partition the allocation named. The bool reports whether the card
	// carries a partition under that identifier at all.
	partitionByID func(id string) (migPartition, device.AcceleratorProcessReason, bool)
}

// resolvePartitions answers every request from one card, in the order they were asked.
//
// The identity and both memory figures come from ONE read on the partition's own handle, so a
// partition is either reported whole — named, sized and measured — or reported as an absence with a
// reason. There is nothing here to resolve independently: everything a partition says about itself
// comes off the same handle, and half an answer would name a grant nobody can size.
func resolvePartitions(
	requests []device.AcceleratorPartitionRequest, src migPartitionSource,
) []device.AcceleratorPartition {
	if enabled, reason := src.migEnabled(); !enabled {
		return unreadAcceleratorPartitions(requests, reason)
	}

	partitions := make([]device.AcceleratorPartition, 0, len(requests))
	for _, req := range requests {
		partition := device.AcceleratorPartition{
			DeviceID:   req.DeviceID,
			Profile:    req.Profile,
			Placements: req.Placements,
			// NVML serves no per-process utilization on a MIG-enabled card, and no per-instance
			// utilization either, so the figure is absent with a named reason on every answer.
			CoresReason: device.AcceleratorProcessReasonUnsupported,
		}

		if req.ID == "" {
			// Nothing to address. Transient rather than unsupported: this node's driver serves the
			// query perfectly well, it is the record that predates the field, and the next
			// allocation of this Pod carries one.
			partition.MemoryReason = device.AcceleratorProcessReasonDriverError
			partitions = append(partitions, partition)
			continue
		}

		read, reason, found := src.partitionByID(req.ID)
		if !found && reason == device.AcceleratorProcessReasonNone {
			// The card carries no partition under that identifier: one destroyed under a live
			// allocation, or an allocation recorded against a partition the card no longer has.
			// Absent — a sibling partition's figure would charge one tenant's memory to another.
			reason = device.AcceleratorProcessReasonDriverError
		}
		if found && reason == device.AcceleratorProcessReasonNone {
			partition.ID = read.id
			partition.MemoryTotalBytes = &read.totalBytes
			partition.MemoryUsedBytes = &read.usedBytes
		}
		partition.MemoryReason = reason
		partitions = append(partitions, partition)
	}
	return partitions
}

// migPartitionSource binds the resolution's two reads to one card's NVML handle.
//
// The MIG device handles are walked at most once per card per pass, on the first read that needs
// them: the walk costs a call per partition slot, and a card asked about no partition never pays
// for it.
func (in *nvidia) migPartitionSource(dev nvml.Device) migPartitionSource {
	var (
		handles      map[string]nvml.MigDevice
		handleReason device.AcceleratorProcessReason
		walked       bool
	)
	walk := func() {
		if !walked {
			handles, handleReason = migDeviceHandles(dev)
			walked = true
		}
	}
	// readPartition is the one read a partition's whole answer comes from.
	readPartition := func(mig nvml.MigDevice) (migPartition, device.AcceleratorProcessReason) {
		// The identity is read first: a partition nobody can name is one no consumer can report,
		// whatever its memory says, so failing here withholds the whole answer rather than
		// publishing figures under the parent card's identifier.
		uuid, ret := mig.GetUUID()
		if !ret.IsSuccess() {
			return migPartition{}, partitionQueryReason(ret)
		}

		memHandler := mig.GetMemoryInfoV()
		memInfo, ret := memHandler.V2()
		if !ret.IsSuccess() {
			memInfo, ret = memHandler.V1()
			if !ret.IsSuccess() {
				return migPartition{}, partitionQueryReason(ret)
			}
		}
		return migPartition{
			id:         uuid,
			totalBytes: memInfo.Total,
			usedBytes:  memInfo.Used,
		}, device.AcceleratorProcessReasonNone
	}

	return migPartitionSource{
		migEnabled: func() (bool, device.AcceleratorProcessReason) {
			current, _, ret := dev.GetMigMode()
			if !ret.IsSuccess() {
				return false, partitionQueryReason(ret)
			}
			// A card whose MIG mode is off holds no partition to read, and it will not start holding
			// one without a mode change: unsupported is what this node can answer, not a failure.
			if current != nvml.DEVICE_MIG_ENABLE {
				return false, device.AcceleratorProcessReasonUnsupported
			}
			return true, device.AcceleratorProcessReasonNone
		},

		partitionByID: func(id string) (migPartition, device.AcceleratorProcessReason, bool) {
			walk()
			if handleReason != device.AcceleratorProcessReasonNone {
				return migPartition{}, handleReason, false
			}
			mig, ok := handles[id]
			if !ok {
				return migPartition{}, device.AcceleratorProcessReasonNone, false
			}
			read, reason := readPartition(mig)
			return read, reason, true
		},
	}
}

// migDeviceHandles maps each of a card's live partition identifiers to the MIG device handle that
// addresses it, which is the handle a partition's own memory is read on.
//
// A handle whose identifier cannot be read is skipped rather than failing the card: another
// partition's figure is still worth reading, and the partition this one belonged to reports an
// absence with a reason of its own.
func migDeviceHandles(dev nvml.Device) (map[string]nvml.MigDevice, device.AcceleratorProcessReason) {
	count, ret := dev.GetMaxMigDeviceCount()
	if !ret.IsSuccess() {
		return nil, partitionQueryReason(ret)
	}

	handles := make(map[string]nvml.MigDevice, count)
	// One partition cannot answer to two handles, so an identifier claimed twice is addressable by
	// neither — and it stays unaddressable however many more handles claim it. Dropping the map entry
	// alone would let a third claimant re-register the very identifier the second one disqualified.
	ambiguous := make(map[string]struct{})
	for i := 0; i < count; i++ {
		mig, ret := dev.GetMigDeviceHandleByIndex(i)
		if !ret.IsSuccess() || mig == nil {
			continue
		}
		uuid, ret := mig.GetUUID()
		if !ret.IsSuccess() || uuid == "" {
			continue
		}
		if _, dup := ambiguous[uuid]; dup {
			continue
		}
		if _, dup := handles[uuid]; dup {
			// Refusing to address the partition by either handle is what keeps a figure from being
			// read off the wrong one.
			delete(handles, uuid)
			ambiguous[uuid] = struct{}{}
			continue
		}
		handles[uuid] = mig
	}
	return handles, device.AcceleratorProcessReasonNone
}

// partitionQueryReason classifies what a partition read answered. It draws the same line the
// per-process classification does — a refusal that will not clear apart from one that may — because
// both are published as the label explaining an absent figure.
func partitionQueryReason(ret nvml.Return) device.AcceleratorProcessReason {
	switch {
	case ret.IsSuccess():
		return device.AcceleratorProcessReasonNone
	case ret == nvml.ERROR_NOT_SUPPORTED, ret.IsAPIUnavailable(), ret == nvml.ERROR_FUNCTION_NOT_FOUND:
		return device.AcceleratorProcessReasonUnsupported
	case ret == nvml.ERROR_NO_PERMISSION:
		return device.AcceleratorProcessReasonPermission
	}
	return device.AcceleratorProcessReasonDriverError
}
