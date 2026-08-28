// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dcmi

/*
#cgo LDFLAGS: -ldl
#cgo CFLAGS: -w
#include "dcmi_wrapper.h"
#include <stdlib.h>
#include "cgo_helpers.h"
*/
import "C"

import (
	"runtime"
	"unsafe"
)

// stringHeader is a struct that represents the internal structure of a Go string.
type stringHeader struct {
	Data unsafe.Pointer
	Len  int
}

// cgoAllocMap stores pointers to C allocated memory for future reference.
type cgoAllocMap struct{}

var cgoAllocsUnknown = new(cgoAllocMap)

// clen returns the length of a null-terminated byte array.
func clen(n []byte) int {
	for i := 0; i < len(n); i++ {
		if n[i] == 0 {
			return i
		}
	}
	return len(n)
}

// unpackPCharString represents the data from Go string as *C.char and avoids copying.
func unpackPCharString(str string) (*C.char, *cgoAllocMap) {
	allocs := new(cgoAllocMap)

	mem0 := unsafe.Pointer(C.CString(str))
	runtime.AddCleanup(allocs, func(mem0 unsafe.Pointer) {
		C.free(mem0)
	}, mem0)
	return (*C.char)(mem0), allocs
}

// packPCharString creates a Go string backed by *C.char and avoids copying.
func packPCharString(p *C.char) (raw string) {
	if p != nil && *p != 0 {
		h := (*stringHeader)(unsafe.Pointer(&raw))
		h.Data = unsafe.Pointer(p)
		for *p != 0 {
			p = (*C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + 1)) // p++.
		}
		h.Len = int(uintptr(unsafe.Pointer(p)) - uintptr(h.Data))
	}
	return raw
}

/*
 */

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *ChipInfo) PassRef() (*C.struct_dcmi_chip_info, *cgoAllocMap) {
	return (*C.struct_dcmi_chip_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (info *ChipInfoV2) PassRef() (*C.struct_dcmi_chip_info_v2, *cgoAllocMap) {
	return (*C.struct_dcmi_chip_info_v2)(unsafe.Pointer(info)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *SocDie) PassRef() (*C.struct_dcmi_soc_die_stru, *cgoAllocMap) {
	return (*C.struct_dcmi_soc_die_stru)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (dieId *DieId) PassRef() (*C.struct_dcmi_die_id, *cgoAllocMap) {
	return (*C.struct_dcmi_die_id)(unsafe.Pointer(dieId)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *EccInfo) PassRef() (*C.struct_dcmi_ecc_info, *cgoAllocMap) {
	return (*C.struct_dcmi_ecc_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (ipAddr *IpAddr) PassRef() (*C.struct_dcmi_ip_addr, *cgoAllocMap) {
	return (*C.struct_dcmi_ip_addr)(unsafe.Pointer(ipAddr)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *HbmInfo) PassRef() (*C.struct_dcmi_hbm_info, *cgoAllocMap) {
	return (*C.struct_dcmi_hbm_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *MemoryInfo) PassRef() (*C.struct_dcmi_memory_info, *cgoAllocMap) {
	return (*C.struct_dcmi_memory_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *GetMemoryInfo) PassRef() (*C.struct_dcmi_get_memory_info_stru, *cgoAllocMap) {
	return (*C.struct_dcmi_get_memory_info_stru)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *PcieInfo) PassRef() (*C.struct_dcmi_pcie_info, *cgoAllocMap) {
	return (*C.struct_dcmi_pcie_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (info *PcieInfoAll) PassRef() (*C.struct_dcmi_pcie_info_all, *cgoAllocMap) {
	return (*C.struct_dcmi_pcie_info_all)(unsafe.Pointer(info)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *MultiUtilizationInfo) PassRef() (*C.struct_dcmi_multi_utilization_info, *cgoAllocMap) {
	return (*C.struct_dcmi_multi_utilization_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
//
// It hands over the Go object's own address rather than a copy, which is what lets a caller pass
// the first element of a slice and have the library fill the elements after it. Every helper in
// this file does that, and two entry points depend on it: the process-memory read below and the
// UB ping mesh reply further down are both filled as arrays, sized by a separate count parameter.
func (x *ProcMemInfo) PassRef() (*C.struct_dcmi_proc_mem_info, *cgoAllocMap) {
	return (*C.struct_dcmi_proc_mem_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// The helpers below exist for the V2 surface. A generated call site binds one for every struct a
// bound wrapper takes by pointer, and `hack/generate.sh` deletes the generated cgo_helpers.go, so
// this hand-written file is the only place they can come from -- a missing one is a compile error,
// not a silent fallback. A parameter declared through a typedef rather than as `struct X *` needs
// none: c-for-go casts it with unsafe.Pointer directly, which is why dcmi_urma_eid_info_t has no
// entry here.

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *BoardInfo) PassRef() (*C.struct_dcmi_board_info, *cgoAllocMap) {
	return (*C.struct_dcmi_board_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *PcieLinkBandwidthInfo) PassRef() (*C.struct_dcmi_pcie_link_bandwidth_info, *cgoAllocMap) {
	return (*C.struct_dcmi_pcie_link_bandwidth_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *ElabelInfo) PassRef() (*C.struct_dcmi_elabel_info, *cgoAllocMap) {
	return (*C.struct_dcmi_elabel_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *UbPingMeshOperate) PassRef() (*C.struct_dcmi_ub_ping_mesh_operate, *cgoAllocMap) {
	return (*C.struct_dcmi_ub_ping_mesh_operate)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
//
// The ping mesh reply is filled as an array: the caller passes the first element of a slice and a
// separate size, so handing over the Go object's own address rather than a copy is load-bearing.
func (x *UbPingMeshInfo) PassRef() (*C.struct_dcmi_ub_ping_mesh_info, *cgoAllocMap) {
	return (*C.struct_dcmi_ub_ping_mesh_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *UbPortInfo) PassRef() (*C.struct_dcmi_ub_port_info, *cgoAllocMap) {
	return (*C.struct_dcmi_ub_port_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *PortPktStatsInfo) PassRef() (*C.struct_dcmi_port_pkt_stats_info, *cgoAllocMap) {
	return (*C.struct_dcmi_port_pkt_stats_info)(unsafe.Pointer(x)), cgoAllocsUnknown
}
