package amd

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
var _ device.AcceleratorProcessDetector = (*amd)(nil)

// MonitorAcceleratorProcesses implements device.AcceleratorProcessDetector: it reports which host
// processes hold each GPU, how much memory of it, and — where the GFX revision measures it — its
// compute-unit occupancy, so the device manager can attribute a share of a shared card to the
// Instance holding it.
//
// This is the one query on this backend that does not go through AMD SMI, and the reason is a defect
// measured on ROCm 7.2.0 rather than a preference. AMD SMI's own per-device process list ignores the
// processor handle it is given and answers with every compute process the driver knows, so a row
// read through it would be charged to every card in the machine; and the entry point that would
// resolve which cards a process actually holds — amdsmi_get_gpu_compute_process_gpus — answers
// INVAL for a live process id, though the symbol resolves and its signature matches both the
// installed header and AMD's published documentation. ROCm SMI answers the same question PER DEVICE,
// which is both the figure this feature needs and the route rocm-smi itself takes on this stack.
//
// The figure that comes back is therefore better than AMD SMI could give even when it worked: a
// process's memory ON THIS DEVICE, rather than the process-wide total AMD SMI's own header warns is
// not device memory.
func (in *amd) MonitorAcceleratorProcesses(
	noPciCheck bool, deviceIDs sets.Set[string],
) (_ device.AcceleratorProcessesGroup, err error) {
	grp := device.AcceleratorProcessesGroup{Manufacturer: Manufacturer}

	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor amd device processes")
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
		// Something holds a carved allocation on a card no AMD PCI device answers for. That is
		// worth telling an operator — a stale allocation, or a card that has left the bus — so
		// every requested device is reported unread rather than dropped, which would leave the
		// consumer an absence with nothing to explain it.
		in.logger.Info("no amd pci devices found for allocated accelerators",
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
		// costs one cheap call and no process read at all. The two libraries agree on it: measured
		// on a two-card host, ROCm SMI's unique id is byte for byte the identifier AMD SMI reports
		// as the accelerator's, which is what lets this query be answered per device ID at all.
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
// the library serves the query, so a capability probe must not conclude from a bad pass that it does
// not. Compute shares the reason with memory here because both come from the same query: ROCm SMI's
// cu_occupancy is a real per-process figure this adapter reads where the hardware measures it, so its
// absence on an unread device is the same transient gap as memory's.
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

// acceleratorProcessesOf turns one device's compute-process answer into the rows the aggregator
// consumes, in the library's own units and without flattening its semantics.
//
// Memory and compute come from the same query, so MemoryReason and CoresReason are the same
// classification of the same Return. The compute sentinel is a PER-ROW fact rather than a per-device
// one: a GFX revision that cannot measure occupancy makes that row's compute absent while leaving its
// memory — and the rest of the device's rows — exactly as measured. Observed on gfx1101, where every
// row carries the sentinel and every row still carries a memory figure.
func acceleratorProcessesOf(
	id string, infos []rsmi.ProcessInfo, infoRet rsmi.Return,
) device.AcceleratorProcesses {
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

// processQueryReason classifies what the compute-process query answered.
//
// A successful query holding no rows is not a refusal: the library reports only processes that hold
// the device, so an empty answer is a measurement of an idle device.
func processQueryReason(ret rsmi.Return) device.AcceleratorProcessReason {
	switch {
	case ret.IsSuccess():
		return device.AcceleratorProcessReasonNone
	case ret.IsAPIUnavailable(), ret == rsmi.STATUS_NOT_SUPPORTED:
		return device.AcceleratorProcessReasonUnsupported
	case ret == rsmi.STATUS_PERMISSION:
		return device.AcceleratorProcessReasonPermission
	case ret == rsmi.STATUS_INSUFFICIENT_SIZE:
		// The binding reports a row list it could not complete as an insufficient size rather than
		// returning the part of it that fit.
		return device.AcceleratorProcessReasonTruncated
	}
	return device.AcceleratorProcessReasonDriverError
}
