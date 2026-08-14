package metax

import (
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/mxsml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

// The per-process query is reached by a type assertion on the detector, so a signature that drifts
// away from the interface would disable the whole feature silently rather than failing to build.
var _ device.AcceleratorProcessDetector = (*metax)(nil)

// MonitorAcceleratorProcesses implements device.AcceleratorProcessDetector: it reports which host
// processes hold each GPU and how much memory of it, so the device manager can attribute a share of
// a shared card to the Instance holding it.
//
// UNVERIFIED ON HARDWARE: no MetaX GPU was available while writing this adapter; the conversion
// below is exercised only against recorded MXSML payloads in process_test.go.
func (in *metax) MonitorAcceleratorProcesses(
	noPciCheck bool, deviceIDs sets.Set[string],
) (_ device.AcceleratorProcessesGroup, err error) {
	grp := device.AcceleratorProcessesGroup{Manufacturer: Manufacturer}

	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor metax device processes")
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
		// Something holds a carved allocation on a card no MetaX PCI device answers for. That is
		// worth telling an operator — a stale allocation, or a card that has left the bus — so
		// every requested device is reported unread rather than dropped, which would leave the
		// consumer an absence with nothing to explain it.
		in.logger.Info("no metax pci devices found for allocated accelerators",
			"devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	in.init()

	cnt := in.mxsml.GetDeviceCount()
	if cnt == 0 {
		in.logger.V(3).Info("no metax devices found for allocated accelerators",
			"devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	grp.Accelerators = make([]device.AcceleratorProcesses, 0, deviceIDs.Len())
	answered := sets.New[string]()
	for i := 0; i < cnt; i++ {
		logger := in.logger.WithValues("index", i)

		dev := in.mxsml.GetDeviceHandleByIndex(i)

		info, ret := dev.GetInfo()
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device info")
			continue
		}
		if mxsml.DeviceVirtualizationMode(info.Mode) == mxsml.Virtualization_Mode_Vf {
			continue
		}

		// The identity is resolved before the query, so a device carrying no carved allocation
		// costs one cheap call and no process read at all.
		uuid := info.GetUUID()
		if !deviceIDs.Has(uuid) {
			continue
		}

		rows, rowsRet := dev.GetProcessInfo()

		procs := acceleratorProcessesOf(uuid, info.GpuId, rows, rowsRet)
		if procs.MemoryReason != device.AcceleratorProcessReasonNone {
			logger.V(3).Info("no per-process memory", "reason", procs.MemoryReason, "return", rowsRet)
		}
		grp.Accelerators = append(grp.Accelerators, procs)
		answered.Insert(uuid)
	}

	// A requested device no enumerated handle answered for — a device-info call that failed, or a
	// card that has left the machine while an allocation still names it — is reported unread for
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
// disproves that MXSML serves the query, so a capability probe must not conclude from a bad pass
// that it does not. Compute stays unsupported here as everywhere else on MetaX, because MXSML
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

// acceleratorProcessesOf turns one device's MXSML per-device process answer into the rows the
// aggregator consumes, in MXSML's own units and without flattening its semantics.
//
// Each row names every GPU the process is using, not only this one, so the row matching this
// device's own GpuId is what is read; a process the query names that carries no entry for this
// device is not reported for it. CoresReason is always unsupported: MXSML carries no per-process
// utilization query at all, so per-process compute is not merely zero on this hardware, it is
// unavailable — a figure this adapter must never claim.
func acceleratorProcessesOf(
	id string, gpuID uint32, rows []mxsml.ProcessInfo_v3, rowsRet mxsml.Return,
) device.AcceleratorProcesses {
	procs := device.AcceleratorProcesses{
		ID:           id,
		MemoryReason: processQueryReason(rowsRet),
		CoresReason:  device.AcceleratorProcessReasonUnsupported,
	}

	procs.Processes = make([]device.AcceleratorProcess, 0, len(rows))
	if procs.MemoryReason != device.AcceleratorProcessReasonNone {
		return procs
	}
	for i := range rows {
		n := min(int(rows[i].GpuNumber), len(rows[i].ProcessGpuInfo))
		for j := 0; j < n; j++ {
			if rows[i].ProcessGpuInfo[j].GpuId != gpuID {
				continue
			}
			procs.Processes = append(procs.Processes, device.AcceleratorProcess{
				PID:         rows[i].ProcessId,
				MemoryBytes: &rows[i].ProcessGpuInfo[j].GpuMemoryUsage,
			})
			break
		}
	}
	return procs
}

// processQueryReason classifies what MXSML's per-device process query answered.
//
// A successful query holding no rows is not a refusal: MXSML reports only processes that hold the
// device, so an empty answer is a measurement of an idle device.
func processQueryReason(ret mxsml.Return) device.AcceleratorProcessReason {
	switch {
	case ret.IsSuccess():
		return device.AcceleratorProcessReasonNone
	case ret.IsAPIUnavailable(), ret == mxsml.OperationNotSupport:
		return device.AcceleratorProcessReasonUnsupported
	case ret == mxsml.PermissionDenied:
		return device.AcceleratorProcessReasonPermission
	case ret == mxsml.InsufficientSize:
		return device.AcceleratorProcessReasonTruncated
	}
	return device.AcceleratorProcessReasonDriverError
}
