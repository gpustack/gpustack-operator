package cambricon

import (
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/cndev"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

// kibShift turns the KiB this library reports its per-process memory in into the bytes the interface
// carries. It is a shift rather than a division because the conversion goes the other way: the field
// is the larger unit's, so a KiB figure has to grow to reach it.
const kibShift = 10

// The per-process query is reached by a type assertion on the detector, so a signature that drifts
// away from the interface would disable the whole feature silently rather than failing to build.
var _ device.AcceleratorProcessDetector = (*cambricon)(nil)

// MonitorAcceleratorProcesses implements device.AcceleratorProcessDetector: it reports which host
// processes hold each MLU and how much of it, so the device manager can attribute a share of a
// shared card to the Instance holding it.
//
// It enumerates the devices a second time rather than riding along with MonitorAccelerator, which
// keeps the raw rows — host process ids — out of every type that reaches a wire. The two passes are
// therefore milliseconds apart; nothing here is derived from the card's own figures, so no consumer
// can observe the gap.
//
// UNVERIFIED ON HARDWARE: no Cambricon MLU was available while writing this adapter; the
// conversion below is exercised only against recorded CNDev payloads in process_test.go.
func (in *cambricon) MonitorAcceleratorProcesses(
	noPciCheck bool, deviceIDs sets.Set[string],
) (_ device.AcceleratorProcessesGroup, err error) {
	grp := device.AcceleratorProcessesGroup{Manufacturer: Manufacturer}

	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor cambricon device processes")
		for s.Scan() {
			in.logger.Error(nil, s.Text())
		}
		err = e
	})

	if deviceIDs.Len() == 0 {
		return grp, nil
	}

	grp.Timestamp = time.Now()

	pciDevs := binding.GetPCIDevices([]string{_PciVendor}, nil)
	if !noPciCheck && len(pciDevs) == 0 {
		// Something holds a carved allocation on a card no Cambricon PCI device answers for. That
		// is worth telling an operator — a stale allocation, or a card that has left the bus — so
		// every requested device is reported unread rather than dropped, which would leave the
		// consumer an absence with nothing to explain it.
		in.logger.Info("no cambricon pci devices found for allocated accelerators",
			"devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	in.init()

	cnt, ret := in.cndev.GetDeviceCount()
	if !ret.IsSuccess() || cnt == 0 {
		in.logger.V(3).Error(ret, "failed to get device count for allocated accelerators",
			"count", cnt, "devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	grp.Accelerators = make([]device.AcceleratorProcesses, 0, deviceIDs.Len())
	answered := sets.New[string]()
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.cndev.GetDeviceHandleByIndex(i)
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device handle by index")
			continue
		}

		// The identity is resolved before the two queries, so a device carrying no carved
		// allocation costs one cheap call and no process read at all.
		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device UUID")
			continue
		}
		if !deviceIDs.Has(uuid) {
			continue
		}

		// Both queries are made on the same handle before anything is interpreted, so the two
		// sources describe the same instant as closely as two library calls can.
		infos, infoRet := dev.GetProcessInfo()
		utils, utilRet := dev.GetProcessUtilization()

		procs := acceleratorProcessesOf(uuid, infos, infoRet, utils, utilRet)
		if procs.MemoryReason != device.AcceleratorProcessReasonNone {
			logger.V(3).Info("no per-process memory", "reason", procs.MemoryReason, "return", infoRet)
		}
		if procs.CoresReason != device.AcceleratorProcessReasonNone {
			logger.V(3).Info("no per-process utilization", "reason", procs.CoresReason, "return", utilRet)
		}
		grp.Accelerators = append(grp.Accelerators, procs)
		answered.Insert(uuid)
	}

	// A requested device no enumerated handle answered for — a handle or UUID call that failed, or
	// a card that has left the machine while an allocation still names it — is reported unread for
	// the same reason: the promise is one entry per requested device, and an entry carrying a
	// reason is what keeps its absence explicable.
	grp.Accelerators = append(grp.Accelerators,
		unreadAcceleratorProcesses(deviceIDs, answered)...)

	return grp, nil
}

// unreadAcceleratorProcesses returns one entry per requested device that was not answered, each
// carrying no rows and a transient reason on both figures.
//
// The reason is transient rather than unsupported on purpose: nothing on these paths disproves that
// CNDev serves either query, so a capability probe must not conclude from a bad pass that it does
// not. Compute shares the reason with memory here, unlike on manufacturers whose library never
// serves a compute figure at all: CNDev's per-process utilization is a real figure this adapter
// reads elsewhere, so its absence on an unread device is the same transient gap as memory's, not a
// property of the library.
func unreadAcceleratorProcesses(
	deviceIDs, answered sets.Set[string],
) []device.AcceleratorProcesses {
	unread := make([]device.AcceleratorProcesses, 0, deviceIDs.Len()-answered.Len())
	for _, id := range sets.List(deviceIDs.Difference(answered)) {
		unread = append(unread, device.AcceleratorProcesses{
			ID:           id,
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonDriverError,
		})
	}
	return unread
}

// acceleratorProcessesOf turns one device's two raw CNDev answers into the rows the aggregator
// consumes, in CNDev's own units and without flattening its semantics.
//
// The rows are the process list's when that query answered. When it did not, they are the pids the
// utilization rows name, so that a driver serving only one of the two queries still reports what it
// does serve. In the other direction a pid appearing only among the utilization rows is dropped:
// GetProcessInfo and GetProcessUtilization are two independent queries of the device's current
// process table, so a pid the process list does not also name is one that has since exited, and
// holding its utilization against a device it no longer holds would report a figure with no
// holder.
func acceleratorProcessesOf(
	id string,
	infos []cndev.ProcessInfo,
	infoRet cndev.Return,
	utils []cndev.ProcessUtilization,
	utilRet cndev.Return,
) device.AcceleratorProcesses {
	procs := device.AcceleratorProcesses{
		ID:           id,
		MemoryReason: processQueryReason(infoRet),
		CoresReason:  processQueryReason(utilRet),
	}

	cores := coresByPID(utils)
	if procs.CoresReason != device.AcceleratorProcessReasonNone {
		cores = nil
	}

	pids := make([]uint32, 0, max(len(infos), len(utils)))
	memory := make(map[uint32]*uint64, len(infos))
	if procs.MemoryReason == device.AcceleratorProcessReasonNone {
		for i := range infos {
			pid := infos[i].Pid
			if _, seen := memory[pid]; seen {
				continue
			}
			// THE LIBRARY REPORTS THIS FIGURE IN KiB, and the field it goes into is bytes. The
			// binding hands it over in the library's own unit on purpose, so this is where it is
			// converted — and it is a multiplication rather than a conversion to MiB, because the
			// aggregator sums the rows natively and converts once, which is what keeps a container
			// holding a few hundred KiB from summing to nothing.
			memory[pid] = ptr.To(infos[i].PhysicalMemoryUsed << kibShift)
			pids = append(pids, pid)
		}
	} else {
		for pid := range cores {
			pids = append(pids, pid)
		}
	}

	procs.Processes = make([]device.AcceleratorProcess, 0, len(pids))
	for _, pid := range pids {
		row := device.AcceleratorProcess{PID: pid, MemoryBytes: memory[pid]}
		if procs.CoresReason == device.AcceleratorProcessReasonNone {
			// A pid the query answered for but held no sample of is an IDLE process, not an
			// unmeasurable one: this library emits a sample only for a process that was busy in
			// the sampling period. So the zero is stated rather than left as a nil, which the
			// aggregator reads as "this row could not be measured" and would take the whole
			// container's figure down with it.
			percent := uint32(0)
			if sampled, ok := cores[pid]; ok {
				percent = sampled
			}
			row.CoresPercent = &percent
		}
		procs.Processes = append(procs.Processes, row)
	}
	return procs
}

// coresByPID reads the MLU (IPU) share each process was using, one row per process as CNDev
// reports it — there is no timestamped sample buffer to fold here, unlike the utilization queries
// some other manufacturers' libraries carry.
func coresByPID(utils []cndev.ProcessUtilization) map[uint32]uint32 {
	cores := make(map[uint32]uint32, len(utils))
	for i := range utils {
		cores[utils[i].Pid] = utils[i].IpuUtil
	}
	return cores
}

// processQueryReason classifies what one of CNDev's per-process queries answered.
//
// A successful query holding no rows is not a refusal: CNDev reports only processes that hold the
// device, so an empty answer is a measurement of an idle device.
func processQueryReason(ret cndev.Return) device.AcceleratorProcessReason {
	switch {
	case ret.IsSuccess():
		return device.AcceleratorProcessReasonNone
	case ret.IsAPIUnavailable(), ret == cndev.ERROR_NOT_SUPPORTED:
		return device.AcceleratorProcessReasonUnsupported
	case ret == cndev.ERROR_NO_PERMISSION:
		return device.AcceleratorProcessReasonPermission
	case ret == cndev.ERROR_INSUFFICIENT_SPACE:
		return device.AcceleratorProcessReasonTruncated
	}
	return device.AcceleratorProcessReasonDriverError
}
