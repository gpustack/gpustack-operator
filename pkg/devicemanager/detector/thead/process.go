package thead

import (
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/hgml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

// The per-process query is reached by a type assertion on the detector, so a signature that drifts
// away from the interface would disable the whole feature silently rather than failing to build.
var _ device.AcceleratorProcessDetector = (*thead)(nil)

// MonitorAcceleratorProcesses implements device.AcceleratorProcessDetector: it reports which host
// processes hold each PPU, how much memory of it and how much of its compute, so the device manager
// can attribute a share of a shared card to the Instance holding it.
//
// It enumerates the devices a second time rather than riding along with MonitorAccelerator, which
// keeps the raw rows — host process ids — out of every type that reaches a wire. The two passes are
// therefore milliseconds apart; nothing here is derived from the card's own figures, so no consumer
// can observe the gap.
func (in *thead) MonitorAcceleratorProcesses(
	noPciCheck bool, deviceIDs sets.Set[string],
) (_ device.AcceleratorProcessesGroup, err error) {
	grp := device.AcceleratorProcessesGroup{Manufacturer: Manufacturer}

	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor thead device processes")
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
		// Something holds a carved allocation on a card no THead PCI device answers for. That is
		// worth telling an operator — a stale allocation, or a card that has left the bus — so
		// every requested device is reported unread rather than dropped, which would leave the
		// consumer an absence with nothing to explain it.
		in.logger.Info("no thead pci devices found for allocated accelerators",
			"devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	in.init()

	cnt, ret := in.hgml.DeviceGetCount()
	if !ret.IsSuccess() || cnt == 0 {
		in.logger.V(3).Error(ret, "failed to count devices for allocated accelerators",
			"count", cnt, "devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	grp.Accelerators = make([]device.AcceleratorProcesses, 0, deviceIDs.Len())
	answered := sets.New[string]()
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev, ret := in.hgml.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device handle")
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
		infos, infoRet := dev.GetComputeRunningProcesses()
		// Ask for every sample the driver still holds rather than only those since a remembered
		// timestamp: the library keeps a few seconds of them, and the newest one per process is the
		// figure worth reporting, so carrying a cursor between ticks would buy nothing and would
		// turn a missed tick into a gap.
		samples, sampleRet := dev.GetProcessUtilization(0)

		procs := acceleratorProcessesOf(uuid, infos, infoRet, samples, sampleRet)
		if procs.MemoryReason != device.AcceleratorProcessReasonNone {
			logger.V(3).Info("no per-process memory", "reason", procs.MemoryReason, "return", infoRet)
		}
		if procs.CoresReason != device.AcceleratorProcessReasonNone {
			logger.V(3).Info("no per-process utilization", "reason", procs.CoresReason, "return", sampleRet)
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
// The reason is transient rather than unsupported on purpose: nothing here disproves that the
// driver serves the query, so a capability probe must not conclude from this that it does not. What
// this says is only that this pass could not read these devices.
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

// acceleratorProcessesOf turns one device's two raw HGML answers into the rows the aggregator
// consumes, in HGML's own units and without flattening its semantics.
//
// The rows are the process list's when that query answered. When it did not, they are the pids the
// utilization samples name, so that a driver serving only one of the two queries still reports what
// it does serve. In the other direction a pid appearing only among the samples is dropped: the
// sample buffer spans several seconds, so such a pid is a process that has since exited and holding
// its utilization against a device it no longer holds would report a figure with no holder.
func acceleratorProcessesOf(
	id string,
	infos []hgml.ProcessInfo,
	infoRet hgml.Return,
	samples []hgml.ProcessUtilizationSample,
	sampleRet hgml.Return,
) device.AcceleratorProcesses {
	procs := device.AcceleratorProcesses{
		ID:           id,
		MemoryReason: processQueryReason(infoRet),
		CoresReason:  processQueryReason(sampleRet),
	}

	utilization := newestUtilizationByPID(samples)
	if procs.CoresReason != device.AcceleratorProcessReasonNone {
		utilization = nil
	}

	pids := make([]uint32, 0, max(len(infos), len(samples)))
	memory := make(map[uint32]*uint64, len(infos))
	if procs.MemoryReason == device.AcceleratorProcessReasonNone {
		for i := range infos {
			pid := infos[i].Pid
			if _, seen := memory[pid]; seen {
				continue
			}
			// The memory field carries a sentinel when the driver does not manage the memory it
			// would have to measure. Unsigned, that sentinel reads as the largest memory figure
			// representable, so it is carried as "no number" instead of as a number.
			var used *uint64
			if infos[i].UsedGpuMemoryAvailable() {
				used = &infos[i].UsedGpuMemory
			}
			memory[pid] = used
			pids = append(pids, pid)
		}
	} else {
		for pid := range utilization {
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
			if sampled, ok := utilization[pid]; ok {
				percent = sampled
			}
			row.CoresPercent = &percent
		}
		procs.Processes = append(procs.Processes, row)
	}
	return procs
}

// newestUtilizationByPID keeps the newest sample of each process.
//
// The library emits one sample per process per sampling period and only for a process that was not
// idle in that period, so one process can appear several times with different timestamps and an idle
// one does not appear at all. Summing a process's samples would count one process's activity as
// several processes'; the newest is the one that describes now — which is also what the PPU slicing
// shim's own compute controller acts on.
func newestUtilizationByPID(samples []hgml.ProcessUtilizationSample) map[uint32]uint32 {
	type sample struct {
		timestamp uint64
		smUtil    uint32
	}

	newest := make(map[uint32]sample, len(samples))
	for i := range samples {
		if kept, ok := newest[samples[i].Pid]; ok && kept.timestamp > samples[i].TimeStamp {
			continue
		}
		newest[samples[i].Pid] = sample{timestamp: samples[i].TimeStamp, smUtil: samples[i].SmUtil}
	}

	utilization := make(map[uint32]uint32, len(newest))
	for pid, s := range newest {
		utilization[pid] = s.smUtil
	}
	return utilization
}

// processQueryReason classifies what a per-process query answered.
//
// A successful query holding no rows is not a refusal: the library reports only processes that hold
// the device and only samples that were not idle, so an empty answer is a measurement of an idle
// device. ERROR_NOT_FOUND — no sample at all in the window — is the same statement, and reporting it
// as an absence would hide a real zero.
func processQueryReason(ret hgml.Return) device.AcceleratorProcessReason {
	switch {
	case ret.IsSuccess(), ret == hgml.ERROR_NOT_FOUND:
		return device.AcceleratorProcessReasonNone
	case ret == hgml.ERROR_NOT_SUPPORTED, ret.IsAPIUnavailable():
		return device.AcceleratorProcessReasonUnsupported
	case ret == hgml.ERROR_NO_PERMISSION:
		return device.AcceleratorProcessReasonPermission
	case ret == hgml.ERROR_INSUFFICIENT_SIZE:
		// The binding reports a row list it could not complete as an insufficient size rather
		// than returning the part of it that fit.
		return device.AcceleratorProcessReasonTruncated
	}
	return device.AcceleratorProcessReasonDriverError
}
