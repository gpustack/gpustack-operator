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
	h := (*stringHeader)(unsafe.Pointer(&str))
	return (*C.char)(h.Data), cgoAllocsUnknown
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
