// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0
//
/**
 * Copyright 2018 Advanced Micro Devices, Inc.  All rights reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
**/

// Package amdgpu is a collection of utility functions to access various properties
// of AMD GPU via Linux kernel interfaces like sysfs and ioctl (using libdrm.)
package amdgpu

import (
	"fmt"

	"gpustack.ai/gpustack/binding"
)

type AMDGPU struct {
	so binding.Library
}

// New creates a new AMDGPU library instance.
// It attempts to load the AMDGPU library from the system and sets up the function pointers for the AMDGPU API functions.
func New(opts ...binding.LibraryOption) *AMDGPU {
	soPaths := []string{
		"libdrm_amdgpu.so.1.0.0",
		"libdrm_amdgpu.so.1",
		"libdrm_amdgpu.so",
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &AMDGPU{so: so}
}

// Init initializes the AMDGPU library.
func (l *AMDGPU) Init() Return {
	if err := l.so.Load(); err != nil {
		return ERROR_LIBRARY_NOT_FOUND
	}
	return SUCCESS
}

// Shutdown shuts down the AMDGPU library and releases any resources it holds.
func (l *AMDGPU) Shutdown() Return {
	if err := l.so.Unload(); err != nil {
		return ERROR_LIBRARY_NOT_FOUND
	}
	return SUCCESS
}

// QueryGPUInfo retrieves information about the GPU with the specified card ID.
func (l *AMDGPU) QueryGPUInfo(cardID uint32) (GpuInfo, Return) {
	dev, ret := l.Open(cardID)
	if !ret.IsSuccess() {
		return GpuInfo{}, ret
	}
	defer func() {
		_ = dev.Free()
	}()

	return dev.QueryGPUInfo()
}

func (l *AMDGPU) GetMarketingName(cardID uint32) string {
	dev, ret := l.Open(cardID)
	if !ret.IsSuccess() {
		return ""
	}
	defer func() {
		_ = dev.Free()
	}()

	return dev.GetMarketingName()
}

type Return int32

const (
	SUCCESS                  Return = 0
	ERROR_CARD_NOTFOUND      Return = -99997
	ERROR_FUNCTION_NOT_FOUND Return = -99998
	ERROR_LIBRARY_NOT_FOUND  Return = -99999
)

// IsSuccess returns true if the return code indicates success.
func (r Return) IsSuccess() bool {
	return r == SUCCESS
}

// String returns the string representation of a Return.
func (r Return) String() string {
	return r.Error()
}

// Error returns the string representation of a Return.
func (r Return) Error() string {
	return defaultErrorStringFunc(r)
}

func defaultErrorStringFunc(r Return) string {
	switch r {
	case SUCCESS:
		return "success"
	case ERROR_CARD_NOTFOUND:
		return "card not found"
	case ERROR_FUNCTION_NOT_FOUND:
		return "function not found in library"
	case ERROR_LIBRARY_NOT_FOUND:
		return "library not found"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}

// AMDGPU_FAMILY_SI = 110  # Hainan, Oland, Verde, Pitcairn, Tahiti
// AMDGPU_FAMILY_CI = 120  # Bonaire, Hawaii
// AMDGPU_FAMILY_KV = 125  # Kaveri, Kabini, Mullins
// AMDGPU_FAMILY_VI = 130  # Iceland, Tonga
// AMDGPU_FAMILY_CZ = 135  # Carrizo, Stoney
// AMDGPU_FAMILY_AI = 141  # Vega10
// AMDGPU_FAMILY_RV = 142  # Raven
// AMDGPU_FAMILY_NV = 143  # Navi10
// AMDGPU_FAMILY_VGH = 144  # Van Gogh
// AMDGPU_FAMILY_GC_11_0_0 = 145  # GC 11.0.0
// AMDGPU_FAMILY_YC = 146  # Yellow Carp
// AMDGPU_FAMILY_GC_11_0_1 = 148  # GC 11.0.1
// AMDGPU_FAMILY_GC_10_3_6 = 149  # GC 10.3.6
// AMDGPU_FAMILY_GC_10_3_7 = 151  # GC 10.3.7
// AMDGPU_FAMILY_GC_11_5_0 = 150  # GC 11.5.0
// AMDGPU_FAMILY_GC_12_0_0 = 152  # GC 12.0.0
var familyIdMap = map[uint32]string{
	110: "Southern Islands",
	120: "Sea Islands",
	125: "Kaveri",
	130: "Volcanic Islands",
	135: "Carrizo",
	141: "Arctic Islands",
	142: "Raven",
	143: "Navi",
	144: "Van Gogh",
	145: "GC 11.0.0",
	146: "Yellow Carp",
	148: "GC 11.0.1",
	149: "GC 10.3.6",
	151: "GC 10.3.7",
	150: "GC 11.5.0",
	152: "GC 12.0.0",
}

func (in GpuInfo) Family() string {
	if v, ok := familyIdMap[in.Family_id]; ok {
		return v
	}
	return ""
}
