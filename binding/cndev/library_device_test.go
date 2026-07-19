// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package cndev

import "testing"

// Without a loaded libcndev.so (the case on any build/test host that has no Cambricon driver, such
// as darwin), every symbol lookup fails, so each sMLU wrapper must fail closed with
// ERROR_FUNCTION_NOT_FOUND rather than dereferencing an absent function pointer.
func TestSMluWrappers_FunctionNotFoundWithoutLibrary(t *testing.T) {
	dev := Device{so: New().so}

	cases := []struct {
		name string
		call func() Return
	}{
		{"GetSMLUMode", func() Return { _, r := dev.GetSMLUMode(); return r }},
		{"SetSMLUMode", func() Return { return dev.SetSMLUMode(true) }},
		{"CreateSMluProfile", func() Return { _, r := dev.CreateSMluProfile(SMluSet{}); return r }},
		{"DestroySMluProfile", func() Return { return dev.DestroySMluProfile(0) }},
		{"CreateSMluInstance", func() Return { return dev.CreateSMluInstance(0, "gpustack") }},
		{"DestroySMluInstanceByName", func() Return { return dev.DestroySMluInstanceByName("gpustack") }},
		{"GetAllSMluInstanceInfo", func() Return { _, r := dev.GetAllSMluInstanceInfo(); return r }},
		{"GetSMluProfileIds", func() Return { _, r := dev.GetSMluProfileIds(); return r }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.call(); got != ERROR_FUNCTION_NOT_FOUND {
				t.Fatalf("got %v, want ERROR_FUNCTION_NOT_FOUND", got)
			}
		})
	}
}
