package ascend

import (
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/binding"
	"gpustack.ai/gpustack/binding/dcmi"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

// The per-process query is reached by a type assertion on the detector, so a signature that drifts
// away from the interface would disable the whole feature silently rather than failing to build.
var _ device.AcceleratorProcessDetector = (*ascend)(nil)

// MonitorAcceleratorProcesses implements device.AcceleratorProcessDetector: it reports which host
// processes hold each NPU and how much memory of it, so the device manager can attribute a share of
// a shared chip to the Instance holding it.
//
// Memory only. DCMI carries a per-vNPU compute figure inside dcmi_computing_resource, but no
// function in either the vendored or the driver's own header takes the struct that wraps it and
// libdcmi.so exports no such query, so per-process compute is not merely zero on this hardware, it
// is unavailable — a figure this adapter never claims.
//
// The library addresses a device by its (card id, device id) pair rather than by the identifier the
// rest of the system keys on, so the enumeration below is the same card-then-index walk the two
// Detector methods use, with the vdie identity resolved on each handle before it is queried.
func (in *ascend) MonitorAcceleratorProcesses(
	noPciCheck bool, deviceIDs sets.Set[string],
) (_ device.AcceleratorProcessesGroup, err error) {
	grp := device.AcceleratorProcessesGroup{Manufacturer: Manufacturer}

	defer loggerx.RecoverWithStackScanner(func(s loggerx.Scanner, e error) {
		in.logger.Error(e, "failed to monitor ascend device processes")
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
		// Something holds a carved allocation on a chip no Ascend PCI device answers for. That is
		// worth telling an operator — a stale allocation, or a card that has left the bus — so
		// every requested device is reported unread rather than dropped, which would leave the
		// consumer an absence with nothing to explain it.
		in.logger.Info("no ascend pci devices found for allocated accelerators",
			"devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	in.init()

	_, cardList, ret := in.dcmi.GetCardList()
	if !ret.IsSuccess() || len(cardList) == 0 {
		in.logger.V(3).Error(ret, "failed to list cards for allocated accelerators",
			"cards", len(cardList), "devices", deviceIDs.Len())
		grp.Accelerators = unreadAcceleratorProcesses(deviceIDs, nil)
		return grp, nil
	}

	grp.Accelerators = make([]device.AcceleratorProcesses, 0, deviceIDs.Len())
	answered := sets.New[string]()
	for _, card := range cardList {
		logger := in.logger.WithValues("card", card)

		cnt, ret := in.dcmi.GetDeviceNumInCard(card)
		if !ret.IsSuccess() {
			logger.V(3).Error(ret, "failed to get device count in card")
			continue
		}

		for i := int32(0); i < cnt; i++ {
			logger := logger.WithValues("index", i)

			dev := in.dcmi.GetDeviceHandleByCardAndIndex(card, i)

			if typ, ret := dev.GetType(); ret.IsSuccess() && typ != dcmi.NPU_TYPE {
				continue
			}

			// The identity is resolved before the query, so a device carrying no carved allocation
			// costs one cheap call and no process read at all.
			var uuid string
			{
				dieHandler := dev.GetVDieV()
				die, ret := dieHandler.V2()
				if !ret.IsSuccess() {
					die, ret = dieHandler.V1()
					if !ret.IsSuccess() {
						logger.V(3).Error(ret, "failed to get device vdie info")
						continue
					}
				}
				uuid = die.String()
			}
			if !deviceIDs.Has(uuid) {
				continue
			}

			rows, rowsRet := dev.GetProcessMemoryUsage()

			procs := acceleratorProcessesOf(uuid, rows, rowsRet)
			if procs.MemoryReason != device.AcceleratorProcessReasonNone {
				logger.V(3).Info("no per-process memory", "reason", procs.MemoryReason, "return", rowsRet)
			}
			grp.Accelerators = append(grp.Accelerators, procs)
			answered.Insert(uuid)
		}
	}

	// A requested device no enumerated handle answered for — a card, type or vdie call that failed,
	// or a chip that has left the machine while an allocation still names it — is reported unread
	// for the same reason: the promise is one entry per requested device, and an entry carrying a
	// reason is what keeps its absence explicable.
	grp.Accelerators = append(grp.Accelerators,
		unreadAcceleratorProcesses(deviceIDs, answered)...)

	return grp, nil
}

// unreadAcceleratorProcesses returns one entry per requested device that was not answered, each
// carrying no rows and a transient reason on memory.
//
// The memory reason is transient rather than unsupported on purpose: nothing here disproves that the
// driver serves the query, so a capability probe must not conclude from this that it does not. What
// this says is only that this pass could not read these devices. Compute is unsupported here as
// everywhere else on Ascend, because that is a property of the library rather than of this pass.
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

// acceleratorProcessesOf turns one device's DCMI per-process memory answer into the rows the
// aggregator consumes, in DCMI's own unit — bytes — and without flattening its semantics.
//
// A row whose process id is not positive is not a process: the field is signed, so a negative id is
// the library having written something this read cannot interpret, and dropping just that row would
// present part of a device's processes as all of them. The whole device's memory figure therefore
// goes absent for this sample, for the same reason a truncated list does.
func acceleratorProcessesOf(
	id string, rows []dcmi.ProcMemInfo, rowsRet dcmi.Return,
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
		if rows[i].Id <= 0 {
			procs.MemoryReason = device.AcceleratorProcessReasonDriverError
			procs.Processes = procs.Processes[:0]
			return procs
		}
		procs.Processes = append(procs.Processes, device.AcceleratorProcess{
			PID:         uint32(rows[i].Id),
			MemoryBytes: &rows[i].Mem_usage,
		})
	}
	return procs
}

// processQueryReason classifies what DCMI's per-process memory query answered.
//
// A successful query holding no rows is not a refusal: the library reports only processes holding
// memory on the device, so an empty answer is a measurement of an idle device.
func processQueryReason(ret dcmi.Return) device.AcceleratorProcessReason {
	switch {
	case ret.IsSuccess():
		return device.AcceleratorProcessReasonNone
	case ret.IsAPIUnavailable(),
		ret == dcmi.ERROR_NOT_SUPPORT, ret == dcmi.ERROR_NOT_SUPPORT_IN_CONTAINER:
		return device.AcceleratorProcessReasonUnsupported
	case ret == dcmi.ERROR_OPER_NOT_PERMITTED:
		return device.AcceleratorProcessReasonPermission
	case ret == dcmi.ERROR_LIST_TRUNCATED:
		// The binding reports a device holding more processes than one read accepts as a truncated
		// list rather than returning the rows that did fit, because the library's process count is
		// an output-only parameter and cannot be told where the buffer ends.
		return device.AcceleratorProcessReasonTruncated
	}
	return device.AcceleratorProcessReasonDriverError
}
