package hygon

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/dmi"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

// The partition query is reached by a type assertion on the detector, so a signature that drifts away
// from the interface would disable partition reporting silently rather than failing to build.
var _ device.AcceleratorPartitionDetector = (*hygon)(nil)

// migConfigDir is the vendor's registry of live instances. The driver writes one file per compute
// instance here the moment it is created, and the same file is what a workload container binds to
// use that instance -- so it is both the mapping this detector needs and the thing the allocator
// hands out. It is a variable so tests can point at a fixture.
var migConfigDir = "/etc/dmi_mig_config"

// _MigInstanceIDPrefix is what the vendor's own tooling prefixes an instance UUID with, in its
// listing and in the DMI_MIG_VISIBLE_DEVICE value a container selects with. Partition identifiers
// carry it for the same reason: the prefixed form is the one an operator sees everywhere else.
const _MigInstanceIDPrefix = "MIG-"

// _MigConfNameRegex matches the vendor's compute-instance file name, which encodes the three numbers
// that address the instance: the device's own index, its GPU instance and its compute instance.
var _MigConfNameRegex = regexp.MustCompile(`^dev([0-9]+)gi([0-9]+)ci([0-9]+)\.conf$`)

// migInstanceRef addresses one compute instance the way the driver enumerates it.
//
// The device index is kept although the lookup below matches on the instance pair alone: a GPU
// instance id is unique only within its card, so two cards can both hold gi 0, and dropping the card
// would make them the same key.
type migInstanceRef struct {
	deviceIndex uint32
	gpuInstance uint32
	computeInst uint32
}

// MonitorAcceleratorPartitions implements device.AcceleratorPartitionDetector: it reports what each
// requested partition holds, read on the partition's OWN device handle.
//
// A partition is found by the identifier its allocation recorded, resolved through the vendor's
// instance registry rather than by which instance holds a process of ours. The difference is the
// whole reason this exists: an idle partition holds no process, and a process-first lookup would
// find no handle and report an absence where the truth is zero.
//
// Unlike every other manufacturer here, compute utilization IS reported. The library answers a
// per-instance utilization on the partition's own handle -- measured at 95% on an instance running a
// matmul loop while its three idle siblings on the same card read 0 -- so the figure is a real
// measurement rather than an absence with a reason.
func (in *hygon) MonitorAcceleratorPartitions(
	noPciCheck bool, requests []device.AcceleratorPartitionRequest,
) (_ device.AcceleratorPartitionsGroup, err error) {
	grp := device.AcceleratorPartitionsGroup{Manufacturer: Manufacturer}

	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor hygon device partitions")
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
		// An allocation names a partition on a card no Hygon PCI device answers for. Every request is
		// still answered -- an answer carrying a reason is what keeps its absence explicable.
		in.logger.Info("no hygon pci devices found for allocated partitions", "partitions", len(requests))
		grp.Partitions = unreadAcceleratorPartitions(requests)
		return grp, nil
	}

	in.init()

	cnt, ret := in.rsmi.GetDeviceCount()
	if !ret.IsSuccess() || cnt == 0 {
		in.logger.V(3).Error(ret, "failed to count devices for allocated partitions",
			"count", cnt, "partitions", len(requests))
		grp.Partitions = unreadAcceleratorPartitions(requests)
		return grp, nil
	}

	// Read once for the whole pass. A partition's identifier is its UUID, and the UUID exists nowhere
	// but here: the library has no GetUUID at all, so without this registry no request can be
	// resolved to a handle.
	refs, err := readMigInstanceRefs(migConfigDir)
	if err != nil {
		in.logger.V(3).Error(err, "failed to read the hygon multi-instance registry",
			"dir", migConfigDir, "partitions", len(requests))
		grp.Partitions = unreadAcceleratorPartitions(requests)
		return grp, nil
	}

	byCard := partitionRequestsByCard(requests)
	grp.Partitions = make([]device.AcceleratorPartition, 0, len(requests))
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev := in.rsmi.GetDeviceHandleByIndex(i)

		// The identity is resolved first, so a card holding no partition of ours costs one cheap call
		// and no instance enumeration at all.
		uuid, ret := dev.GetUniqueId()
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device UUID")
			continue
		}
		cardRequests, ok := byCard[uuid]
		if !ok {
			continue
		}
		delete(byCard, uuid)

		pciBusID, ret := dev.GetPciId()
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device PCI ID for allocated partitions")
			grp.Partitions = append(grp.Partitions, unreadAcceleratorPartitions(cardRequests)...)
			continue
		}

		partitions := resolvePartitions(cardRequests, refs, in.migPartitionSource(pciBusID))
		for j := range partitions {
			if partitions[j].MemoryReason != device.AcceleratorProcessReasonNone {
				logger.V(3).Info("no partition memory", "device", uuid,
					"profile", partitions[j].Profile, "reason", partitions[j].MemoryReason)
			}
		}
		grp.Partitions = append(grp.Partitions, partitions...)
	}

	// A request whose card no enumerated handle answered for -- a handle or UUID call that failed, or
	// a card that has left the machine while an allocation still names a partition on it.
	for _, cardRequests := range byCard {
		grp.Partitions = append(grp.Partitions, unreadAcceleratorPartitions(cardRequests)...)
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

// unreadAcceleratorPartitions answers every request with no figure, for the paths that never reach a
// partition's handle at all.
//
// The reason is always transient rather than unsupported, and it is fixed rather than passed in
// because nothing on these paths disproves that the library serves the query: every one of them --
// no PCI device, no device count, an unreadable registry, a card no handle answered for -- is a card
// this pass could not reach, and a capability probe must not conclude "this node cannot do it" from
// that. Compute carries the same reason as memory, unlike on the manufacturers whose library serves
// no compute figure at all: this one does, so its absence here is the same transient gap as
// memory's rather than a property of the library.
func unreadAcceleratorPartitions(
	requests []device.AcceleratorPartitionRequest,
) []device.AcceleratorPartition {
	unread := make([]device.AcceleratorPartition, 0, len(requests))
	for _, req := range requests {
		unread = append(unread, device.AcceleratorPartition{
			DeviceID:     req.DeviceID,
			Profile:      req.Profile,
			Placements:   req.Placements,
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonDriverError,
		})
	}
	return unread
}

// migPartitionRead is what one compute instance's own handle reported about itself.
type migPartitionRead struct {
	memoryTotalBytes uint64
	memoryUsedBytes  uint64
	coresPercent     uint32
}

// migPartitionSource is the driver-facing half of the resolution: it answers for one card, keyed by
// the instance pair the registry resolved a request's identifier to.
//
// It is a closure so the matching itself stays hardware-free and unit-testable, as this package's
// profile derivation already is. The bool reports whether the card carries that instance at all,
// which is a different answer from a read that failed.
type migPartitionSource func(ref migInstanceRef) (migPartitionRead, device.AcceleratorProcessReason, bool)

// resolvePartitions answers every request from one card, in the order they were asked.
//
// Identity, memory and compute all come from ONE read on the partition's own handle, so a partition
// is either reported whole -- named, sized and measured -- or reported as an absence with a reason.
// Half an answer would name a grant nobody can size.
func resolvePartitions(
	requests []device.AcceleratorPartitionRequest,
	refs map[string]migInstanceRef,
	src migPartitionSource,
) []device.AcceleratorPartition {
	partitions := make([]device.AcceleratorPartition, 0, len(requests))
	for _, req := range requests {
		partition := device.AcceleratorPartition{
			DeviceID:   req.DeviceID,
			Profile:    req.Profile,
			Placements: req.Placements,
		}

		ref, known := refs[normalizeMigInstanceID(req.ID)]
		if req.ID == "" || !known {
			// Nothing to address, or an identifier the vendor's registry does not carry: an instance
			// destroyed under a live allocation, or an allocation recorded against one the node no
			// longer has. Transient rather than unsupported -- the driver serves the query perfectly
			// well, it is this partition that cannot be found -- and absent rather than approximated,
			// because a sibling instance's figure would charge one tenant's memory to another.
			partition.MemoryReason = device.AcceleratorProcessReasonDriverError
			partition.CoresReason = device.AcceleratorProcessReasonDriverError
			partitions = append(partitions, partition)
			continue
		}

		read, reason, found := src(ref)
		if !found && reason == device.AcceleratorProcessReasonNone {
			// The registry names the instance but the driver does not enumerate it. Same treatment,
			// same reason: the two disagree, so neither can be believed about this partition.
			reason = device.AcceleratorProcessReasonDriverError
		}
		if found && reason == device.AcceleratorProcessReasonNone {
			partition.ID = _MigInstanceIDPrefix + trimMigInstanceIDPrefix(req.ID)
			partition.MemoryTotalBytes = &read.memoryTotalBytes
			partition.MemoryUsedBytes = &read.memoryUsedBytes
			partition.CoresPercent = &read.coresPercent
		}
		partition.MemoryReason = reason
		partition.CoresReason = reason
		partitions = append(partitions, partition)
	}
	return partitions
}

// normalizeMigInstanceID reduces an identifier to the bare UUID the registry is keyed by, so a
// caller holding either spelling resolves the same instance.
func normalizeMigInstanceID(id string) string {
	return strings.ToLower(trimMigInstanceIDPrefix(id))
}

// trimMigInstanceIDPrefix removes the vendor's display prefix if it is there.
func trimMigInstanceIDPrefix(id string) string {
	return strings.TrimPrefix(id, _MigInstanceIDPrefix)
}

// migPartitionSource binds the resolution to one card's Multi-Instance handle.
//
// The card's instance handles are walked at most once per card per pass, on the first read that
// needs them: the walk costs a call per index of a node-wide index space, and a card asked about no
// partition never pays for it.
func (in *hygon) migPartitionSource(pciBusID string) migPartitionSource {
	var (
		handles map[migInstanceRef]dmi.Device
		reason  device.AcceleratorProcessReason
		walked  bool
	)

	walk := func() {
		walked = true
		handles = make(map[migInstanceRef]dmi.Device)

		dev, ret := in.dmi.GetDeviceHandleByPciBusId(pciBusID)
		if !ret.IsSuccess() {
			reason = migPartitionReason(ret)
			return
		}

		migs, ret := dev.MigDevices()
		if !ret.IsSuccess() {
			reason = migPartitionReason(ret)
			return
		}

		index, ret := dev.GetIndex()
		if !ret.IsSuccess() {
			reason = migPartitionReason(ret)
			return
		}

		for _, md := range migs {
			gi, ret := md.GetGpuInstanceID()
			if !ret.IsSuccess() {
				continue
			}
			ci, ret := md.GetComputeInstanceID()
			if !ret.IsSuccess() {
				continue
			}
			handles[migInstanceRef{deviceIndex: index, gpuInstance: gi, computeInst: ci}] = md
		}
	}

	return func(ref migInstanceRef) (migPartitionRead, device.AcceleratorProcessReason, bool) {
		if !walked {
			walk()
		}
		if reason != device.AcceleratorProcessReasonNone {
			return migPartitionRead{}, reason, false
		}

		md, ok := handles[ref]
		if !ok {
			return migPartitionRead{}, device.AcceleratorProcessReasonNone, false
		}

		mem, ret := md.GetMemoryInfo()
		if !ret.IsSuccess() {
			return migPartitionRead{}, migPartitionReason(ret), true
		}
		util, ret := md.GetUtilizationRates()
		if !ret.IsSuccess() {
			return migPartitionRead{}, migPartitionReason(ret), true
		}

		return migPartitionRead{
			memoryTotalBytes: mem.Total,
			memoryUsedBytes:  mem.Used,
			coresPercent:     util.Gpu,
		}, device.AcceleratorProcessReasonNone, true
	}
}

// migPartitionReason classifies what the library answered about a partition.
func migPartitionReason(ret dmi.Return) device.AcceleratorProcessReason {
	switch {
	case ret.IsSuccess():
		return device.AcceleratorProcessReasonNone
	case ret.IsAPIUnavailable(), ret == dmi.ERROR_NOT_SUPPORTED:
		return device.AcceleratorProcessReasonUnsupported
	case ret == dmi.ERROR_NO_PERMISSION:
		return device.AcceleratorProcessReasonPermission
	case ret == dmi.ERROR_INSUFFICIENT_SIZE:
		return device.AcceleratorProcessReasonTruncated
	}
	return device.AcceleratorProcessReasonDriverError
}

// readMigInstanceRefs reads the vendor's instance registry into a lookup from a partition's UUID to
// the three numbers that address it.
//
// A missing directory is an empty registry rather than an error: it exists only while the node is in
// Multi-Instance mode, and a node that is not partitioned holds no partition to be asked about. A
// file that cannot be parsed is skipped rather than failing the pass -- one unreadable entry must
// not blind the reader to every other instance on the node -- while a directory that cannot be read
// at all IS an error, because then nothing is known and reporting an empty registry would answer
// every request with "no such partition".
func readMigInstanceRefs(dir string) (map[string]migInstanceRef, error) {
	ciDir := filepath.Join(dir, "ci")

	entries, err := os.ReadDir(ciDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]migInstanceRef{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", ciDir, err)
	}

	refs := make(map[string]migInstanceRef, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ref, ok := migInstanceRefOf(entry.Name())
		if !ok {
			continue
		}
		uuid, err := readMigConfUUID(filepath.Join(ciDir, entry.Name()))
		if err != nil || uuid == "" {
			continue
		}
		refs[strings.ToLower(uuid)] = ref
	}
	return refs, nil
}

// migInstanceRefOf parses the three numbers out of a compute-instance file name.
func migInstanceRefOf(name string) (migInstanceRef, bool) {
	m := _MigConfNameRegex.FindStringSubmatch(name)
	if m == nil {
		return migInstanceRef{}, false
	}
	nums := make([]uint32, 3)
	for i := range nums {
		n, err := strconv.ParseUint(m[i+1], 10, 32)
		if err != nil {
			return migInstanceRef{}, false
		}
		nums[i] = uint32(n)
	}
	return migInstanceRef{deviceIndex: nums[0], gpuInstance: nums[1], computeInst: nums[2]}, true
}

// readMigConfUUID returns the instance UUID a compute-instance file carries.
//
// The file is the concatenation of its GPU instance's fields and its own, so it holds several
// repeated keys; the UUID appears once and only in the compute-instance half, which is why it can be
// read by key rather than by position.
func readMigConfUUID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), ":")
		if !ok || strings.TrimSpace(key) != "mig_uuid" {
			continue
		}
		return strings.TrimSpace(value), nil
	}
	return "", s.Err()
}
