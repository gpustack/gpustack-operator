// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dmi

/*
#cgo LDFLAGS: -ldl
#cgo CFLAGS: -w
#include "dmi_mig_wrapper.h"
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

// The helpers below hand a caller's own Go object to the library rather than a copy of it, which is
// what lets the two array-filling queries work: GetGpuInstancePossiblePlacements and its compute
// counterpart are given the first element of a slice plus a separate count, and fill the elements
// after it. `hack/generate.sh` deletes the generated cgo_helpers.go, so this hand-written file is
// the only place these can come from -- a missing one is a compile error, not a silent fallback.
//
// Passing a Go pointer this way is only safe for a whole heap object: cgocheck scans the entire
// object a pointer lands in, so &someStruct.field must never be handed across, even though the
// compiler accepts it.

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *PciInfo) PassRef() (*C.struct_nvmlPciInfo_st, *cgoAllocMap) {
	return (*C.struct_nvmlPciInfo_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *DeviceAttributes) PassRef() (*C.struct_nvmlDeviceAttributes_st, *cgoAllocMap) {
	return (*C.struct_nvmlDeviceAttributes_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *Utilization) PassRef() (*C.struct_nvmlUtilization_st, *cgoAllocMap) {
	return (*C.struct_nvmlUtilization_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *Memory) PassRef() (*C.struct_nvmlMemory_st, *cgoAllocMap) {
	return (*C.struct_nvmlMemory_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *GpuInstanceProfileInfo) PassRef() (*C.struct_nvmlGpuInstanceProfileInfo_st, *cgoAllocMap) {
	return (*C.struct_nvmlGpuInstanceProfileInfo_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *ComputeInstanceProfileInfo) PassRef() (*C.struct_nvmlComputeInstanceProfileInfo_st, *cgoAllocMap) {
	return (*C.struct_nvmlComputeInstanceProfileInfo_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
//
// The placement queries fill an array through this pointer, so handing over the Go object's own
// address rather than a copy is load-bearing here rather than merely cheaper.
func (x *GpuInstancePlacement) PassRef() (*C.struct_nvmlGpuInstancePlacement_st, *cgoAllocMap) {
	return (*C.struct_nvmlGpuInstancePlacement_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
//
// Filled as an array, for the same reason as its GPU-instance counterpart above.
func (x *ComputeInstancePlacement) PassRef() (*C.struct_nvmlComputeInstancePlacement_st, *cgoAllocMap) {
	return (*C.struct_nvmlComputeInstancePlacement_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *GpuInstanceInfo) PassRef() (*C.struct_nvmlGpuInstanceInfo_st, *cgoAllocMap) {
	return (*C.struct_nvmlGpuInstanceInfo_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}

// PassRef returns a reference to C object as it is or allocates a new C object of this type.
func (x *ComputeInstanceInfo) PassRef() (*C.struct_nvmlComputeInstanceInfo_st, *cgoAllocMap) {
	return (*C.struct_nvmlComputeInstanceInfo_st)(unsafe.Pointer(x)), cgoAllocsUnknown
}
