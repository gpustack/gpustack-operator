package iluvatar

import (
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/ixml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

// The per-process query is reached by a type assertion on the detector, so a signature that drifts
// away from the interface would disable the whole feature silently rather than failing to build.
var _ device.AcceleratorProcessDetector = (*iluvatar)(nil)

// MonitorAcceleratorProcesses implements device.AcceleratorProcessDetector: it reports which host
// processes hold each GPU and how much memory of it, so the device manager can attribute a share of
// a shared card to the Instance holding it.
//
// UNVERIFIED ON HARDWARE: no Iluvatar GPU was available while writing this adapter; the conversion
// below is exercised only against recorded IXML payloads in process_test.go.
func (in *iluvatar) MonitorAcceleratorProcesses(
	noPciCheck bool, deviceIDs sets.Set[string],
) (_ device.AcceleratorProcessesGroup, err error) {
	grp := device.AcceleratorProcessesGroup{Manufacturer: Manufacturer}

	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor iluvatar device processes")
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
		// Something holds a carved allocation on a card no Iluvatar PCI device answers for. That
		// is worth telling an operator — a stale allocation, or a card that has left the bus — so
		// every requested device is reported unread rather than dropped, which would leave the
		// consumer an absence with nothing to explain it.
		in.logger.Info("no iluvatar pci devices found for allocated accelerators",
			"devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	in.init()

	cnt, ret := in.ixml.DeviceGetCount()
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

		dev, ret := in.ixml.DeviceGetHandleByIndex(i)
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device handle")
			continue
		}

		// The identity is resolved before the query, so a device carrying no carved allocation
		// costs one cheap call and no process read at all.
		uuid, ret := dev.GetUUID()
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device UUID")
			continue
		}
		if !deviceIDs.Has(uuid) {
			continue
		}

		infos, infoRet := dev.GetComputeRunningProcesses()

		procs := acceleratorProcessesOf(uuid, infos, infoRet)
		if procs.MemoryReason != device.AcceleratorProcessReasonNone {
			logger.V(3).Info("no per-process memory", "reason", procs.MemoryReason, "return", infoRet)
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
// carrying no rows, a transient reason on memory, and unsupported on compute.
//
// The memory reason is transient rather than unsupported on purpose: nothing on these paths
// disproves that IXML serves the query, so a capability probe must not conclude from a bad pass
// that it does not. Compute stays unsupported here as everywhere else on Iluvatar, because IXML
// carrying no per-process utilization query at all is a property of the library, not of this pass.
func unreadAcceleratorProcesses(
	deviceIDs, answered sets.Set[string],
) []device.AcceleratorProcesses {
	unread := make([]device.AcceleratorProcesses, 0, deviceIDs.Len()-answered.Len())
	for _, id := range sets.List(deviceIDs.Difference(answered)) {
		unread = append(unread, device.AcceleratorProcesses{
			ID:           id,
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonUnsupported,
		})
	}
	return unread
}

// acceleratorProcessesOf turns one device's IXML compute-process answer into the rows the
// aggregator consumes, in IXML's own units and without flattening its semantics.
//
// CoresReason is always unsupported: IXML carries no per-process utilization query at all, so
// per-process compute is not merely zero on this hardware, it is unavailable — a figure this
// adapter must never claim.
func acceleratorProcessesOf(id string, infos []ixml.ProcessInfo_v1, infoRet ixml.Return) device.AcceleratorProcesses {
	procs := device.AcceleratorProcesses{
		ID:           id,
		MemoryReason: processQueryReason(infoRet),
		CoresReason:  device.AcceleratorProcessReasonUnsupported,
	}

	procs.Processes = make([]device.AcceleratorProcess, 0, len(infos))
	if procs.MemoryReason != device.AcceleratorProcessReasonNone {
		return procs
	}
	for i := range infos {
		procs.Processes = append(procs.Processes, device.AcceleratorProcess{
			PID:         infos[i].Pid,
			MemoryBytes: &infos[i].UsedGpuMemory,
		})
	}
	return procs
}

// processQueryReason classifies what IXML's compute-running-processes query answered.
//
// A successful query holding no rows is not a refusal: IXML reports only processes that hold the
// device, so an empty answer is a measurement of an idle device.
func processQueryReason(ret ixml.Return) device.AcceleratorProcessReason {
	switch {
	case ret.IsSuccess(), ret == ixml.ERROR_NOT_FOUND:
		return device.AcceleratorProcessReasonNone
	case ret.IsAPIUnavailable(), ret == ixml.ERROR_NOT_SUPPORTED:
		return device.AcceleratorProcessReasonUnsupported
	case ret == ixml.ERROR_NO_PERMISSION:
		return device.AcceleratorProcessReasonPermission
	case ret == ixml.ERROR_INSUFFICIENT_SIZE:
		return device.AcceleratorProcessReasonTruncated
	}
	return device.AcceleratorProcessReasonDriverError
}
