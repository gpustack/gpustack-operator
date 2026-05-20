// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package amdgpu

/*
#include <stdlib.h>
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
