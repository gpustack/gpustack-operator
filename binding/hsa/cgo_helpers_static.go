// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package hsa

/*
#cgo linux LDFLAGS: -Wl,--export-dynamic -Wl,--unresolved-symbols=ignore-in-object-files
#cgo darwin LDFLAGS: -Wl,-undefined,dynamic_lookup
#include "hsa_wrapper.h"
#include <stdlib.h>
#include "cgo_helpers.h"
*/
import "C"

import (
	"fmt"
	"sync"
	"sync/atomic"
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

type IterateAgentsCallback = func(agent Agent, data unsafe.Pointer) Return

var (
	cbRegistry sync.Map // map[uintptr]IterateAgentsCallback
	cbID       atomic.Uintptr
)

func registerCallback(cb IterateAgentsCallback) uintptr {
	id := cbID.Add(1)
	cbRegistry.Store(id, cb)
	return id
}

func unregisterCallback(id uintptr) {
	cbRegistry.Delete(id)
}

//export go_hsa_iterate_agents_cb
func go_hsa_iterate_agents_cb(agent C.hsa_agent_t, data unsafe.Pointer) C.hsa_status_t {
	id := uintptr(data)

	v, ok := cbRegistry.Load(id)
	if !ok {
		return C.HSA_STATUS_ERROR
	}

	cb := v.(func(Agent, unsafe.Pointer) Return)
	return C.hsa_status_t(cb(*(*Agent)(unsafe.Pointer(&agent)), data))
}

func hsaIterateAgents(cb IterateAgentsCallback) Return {
	if cb == nil {
		return Return(C.HSA_STATUS_ERROR_INVALID_ARGUMENT)
	}

	id := registerCallback(cb)
	defer unregisterCallback(id)

	ret := C.call_hsa_iterate_agents(unsafe.Pointer(id))
	return Return(ret)
}

func hsaAgentGetInfoDevice(agent Agent) (uint32, Return) {
	var device uint32
	// cDevice := (*C.uint32_t)(unsafe.Pointer(&device))
	return device, hsaAgentGetInfo(agent, AGENT_INFO_DEVICE, unsafe.Pointer(&device))
}

func hsaAgentGetInfoUUID(agent Agent) (string, Return) {
	uuid := make([]byte, 21)
	// cUuid := (*C.char)(unsafe.Pointer(&uuid[0]))
	ret := hsaAgentGetInfo(agent, AgentInfo(AMD_AGENT_INFO_UUID), unsafe.Pointer(&uuid[0]))
	if !ret.IsSuccess() {
		return "", ret
	}
	return string(uuid[:clen(uuid)]), STATUS_SUCCESS
}

func hsaAgentGetInfoProduceName(agent Agent) (string, Return) {
	name := make([]byte, 64)
	// cName := (*C.char)(unsafe.Pointer(&name[0]))
	ret := hsaAgentGetInfo(agent, AgentInfo(AMD_AGENT_INFO_PRODUCT_NAME), unsafe.Pointer(&name[0]))
	if !ret.IsSuccess() {
		return "", ret
	}
	return string(name[:clen(name)]), STATUS_SUCCESS
}

func hsaAgentGetInfoName(agent Agent) (string, Return) {
	name := make([]byte, 64)
	// cName := (*C.char)(unsafe.Pointer(&name[0]))
	ret := hsaAgentGetInfo(agent, AGENT_INFO_NAME, unsafe.Pointer(&name[0]))
	if !ret.IsSuccess() {
		return "", ret
	}
	return string(name[:clen(name)]), STATUS_SUCCESS
}

func hsaAgentGetInfoComputeUnitCount(agent Agent) (uint32, Return) {
	var cuCount uint32
	// cCuCount := (*C.uint32_t)(unsafe.Pointer(&cuCount))
	return cuCount, hsaAgentGetInfo(agent, AgentInfo(AMD_AGENT_INFO_COMPUTE_UNIT_COUNT), unsafe.Pointer(&cuCount))
}

func hsaAgentGetInfoAsicFamilyId(agent Agent) (uint32, Return) {
	var familyId uint32
	// cFamilyId := (*C.uint32_t)(unsafe.Pointer(&familyId))
	return familyId, hsaAgentGetInfo(agent, AgentInfo(AMD_AGENT_INFO_ASIC_FAMILY_ID), unsafe.Pointer(&familyId))
}

func hsaAgentGetBDF(agent Agent) (string, Return) {
	var bdf uint64
	ret := hsaAgentGetInfo(agent, AgentInfo(AMD_AGENT_INFO_BDFID), unsafe.Pointer(&bdf))
	if !ret.IsSuccess() {
		return "", ret
	}
	domain := (bdf >> 32) & 0xFFFFFFFF
	bus := (bdf >> 8) & 0xFF
	deviceId := (bdf >> 3) & 0x1F
	function := bdf & 0x7
	return fmt.Sprintf("%04x:%02x:%02x.%x", domain, bus, deviceId, function), STATUS_SUCCESS
}
