package hygon

import (
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/rsmi"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

// The per-process query is reached by a type assertion on the detector, so a signature that drifts
// away from the interface would disable the whole feature silently rather than failing to build.
var _ device.AcceleratorProcessDetector = (*hygon)(nil)

// MonitorAcceleratorProcesses implements device.AcceleratorProcessDetector: it reports which host
// processes hold each DCU, how much memory of it, and — where the GFX revision measures it — its
// compute-unit occupancy, so the device manager can attribute a share of a shared card to the
// Instance holding it.
//
// THIS ADAPTER and the observable path above it have both been run against a driver. On a BW card
// (gfx9, DTK 25.04) carrying a sliced container running a matmul, the library answered with the
// holding process's host pid, its VRAM usage and a live cu_occupancy, all three matching the
// kernel's own figures under /sys/class/kfd/kfd/proc/<pid>/ at the time they were read. What that
// settles is the question this adapter could not answer from recorded payloads: RSMI's compute
// figure on Hygon is a real percentage and not the KFD_STATS_INVALID sentinel an AMD GFX revision
// returns — on this revision.
//
// A later run carried it up through the subresource, the exporter, /monitor/snapshot and the
// capability gauge on a held slice allocation, so `docs/reference/instance-metrics.md` now marks
// Hygon run in its "On hardware" column. That run also showed the two figures are not corroborated
// to the same depth, and the page says so: memory agreed exactly with both the vendor's hy-smi and
// the kernel's vram_<gpuid>, while nothing BUT the kernel publishes a per-process compute figure to
// compare against — and there it read a steady 10 where the library's figure implied 11. So read the
// compute figure as measuring the right quantity rather than as agreeing with the kernel's number.
//
// What that run also established is that the THREE-STEP shape below is load-bearing rather than
// defensive. The host-wide enumeration reports the pid and leaves both figures at zero — it is a
// list of processes, not a measurement — so an adapter that trusted its rows would report every
// holder of every card as using nothing. Only the per-device query answers, and only it: the
// per-pid variant beside it returned the VRAM figure with cu_occupancy still zero.
func (in *hygon) MonitorAcceleratorProcesses(
	noPciCheck bool, deviceIDs sets.Set[string],
) (_ device.AcceleratorProcessesGroup, err error) {
	grp := device.AcceleratorProcessesGroup{Manufacturer: Manufacturer}

	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor hygon device processes")
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
		// Something holds a carved allocation on a card no Hygon PCI device answers for. That is
		// worth telling an operator — a stale allocation, or a card that has left the bus — so
		// every requested device is reported unread rather than dropped, which would leave the
		// consumer an absence with nothing to explain it.
		in.logger.Info("no hygon pci devices found for allocated accelerators",
			"devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	in.init()

	cnt, ret := in.rsmi.GetDeviceCount()
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

		dev := in.rsmi.GetDeviceHandleByIndex(i)

		// The identity is resolved before the query, so a device carrying no carved allocation
		// costs one cheap call and no process read at all.
		uuid, ret := dev.GetUniqueId()
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device UUID")
			continue
		}
		if !deviceIDs.Has(uuid) {
			continue
		}

		infos, infoRet := dev.GetComputeProcessInfo()

		procs := acceleratorProcessesOf(uuid, infos, infoRet)
		if procs.MemoryReason != device.AcceleratorProcessReasonNone {
			logger.V(3).Info("no per-process memory", "reason", procs.MemoryReason, "return", infoRet)
		}
		if procs.CoresReason != device.AcceleratorProcessReasonNone {
			logger.V(3).Info("no per-process compute", "reason", procs.CoresReason, "return", infoRet)
		}
		grp.Accelerators = append(grp.Accelerators, procs)
		answered.Insert(uuid)
	}

	// A requested device no enumerated handle answered for — a UUID call that failed, or a card
	// that has left the machine while an allocation still names it — is reported unread for the
	// same reason: the promise is one entry per requested device, and an entry carrying a reason is
	// what keeps its absence explicable.
	grp.Accelerators = append(grp.Accelerators,
		unreadAcceleratorProcesses(deviceIDs, answered)...)

	return grp, nil
}

// unreadAcceleratorProcesses returns one entry per requested device that was not answered, each
// carrying no rows and a transient reason on both figures.
//
// The reason is transient rather than unsupported on purpose: nothing on these paths disproves that
// RSMI serves the query, so a capability probe must not conclude from a bad pass that it does not.
// Compute shares the reason with memory here, unlike on manufacturers whose library never serves a
// compute figure at all: RSMI's cu_occupancy is a real per-process figure this adapter reads
// elsewhere, so its absence on an unread device is the same transient gap as memory's, not a
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

// acceleratorProcessesOf turns one device's RSMI compute-process answer into the rows the
// aggregator consumes, in RSMI's own units and without flattening its semantics.
//
// Memory and compute come from the same query, so MemoryReason and CoresReason are the same
// classification of the same Return: a device this call could not answer for is unsupported (or
// whichever other reason applies) on both figures alike, and a call that succeeded is none on both
// — even when every row it returned carries the compute sentinel. CuOccupancyAvailable is a
// per-row fact, not a per-device one: one process on a GFX revision that cannot measure occupancy
// does not make the query's answer for the rest of the device a refusal, so a row failing that
// check reports memory and leaves CoresPercent absent, the same treatment the NVIDIA adapter gives
// a row whose memory reads the NVML sentinel.
func acceleratorProcessesOf(id string, infos []rsmi.ProcessInfo, infoRet rsmi.Return) device.AcceleratorProcesses {
	reason := processQueryReason(infoRet)
	procs := device.AcceleratorProcesses{
		ID:           id,
		MemoryReason: reason,
		CoresReason:  reason,
	}

	procs.Processes = make([]device.AcceleratorProcess, 0, len(infos))
	if procs.MemoryReason != device.AcceleratorProcessReasonNone {
		return procs
	}
	for i := range infos {
		row := device.AcceleratorProcess{
			PID:         infos[i].Process_id,
			MemoryBytes: &infos[i].Vram_usage,
		}
		if infos[i].CuOccupancyAvailable() {
			row.CoresPercent = &infos[i].Cu_occupancy
		}
		procs.Processes = append(procs.Processes, row)
	}
	return procs
}

// processQueryReason classifies what RSMI's compute-process query answered.
//
// A successful query holding no rows is not a refusal: RSMI reports only processes that hold the
// device, so an empty answer is a measurement of an idle device.
func processQueryReason(ret rsmi.Return) device.AcceleratorProcessReason {
	switch {
	case ret.IsSuccess():
		return device.AcceleratorProcessReasonNone
	case ret.IsAPIUnavailable(), ret == rsmi.STATUS_NOT_SUPPORTED:
		return device.AcceleratorProcessReasonUnsupported
	case ret == rsmi.STATUS_PERMISSION:
		return device.AcceleratorProcessReasonPermission
	case ret == rsmi.STATUS_INSUFFICIENT_SIZE:
		return device.AcceleratorProcessReasonTruncated
	}
	return device.AcceleratorProcessReasonDriverError
}
