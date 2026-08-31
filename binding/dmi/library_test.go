// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dmi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The table pins both halves of the predicate: every code that means the call could not be made at
// all, and the near-misses that must not be mistaken for one.
//
// ERROR_NOT_SUPPORTED and ERROR_NOT_FOUND are the near-misses that matter here, and they are not
// rare: this API returns the first for a profile width a card does not offer and the second for a
// MIG-device index belonging to another card, both on every healthy sweep. Reading either as "the
// library is unusable" would report a working driver as absent.
func TestReturn_IsAPIUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the library was not found", ERROR_LIBRARY_NOT_FOUND, true},
		{"the symbol is absent from the library", ERROR_FUNCTION_NOT_FOUND, true},
		{"the driver is not loaded", ERROR_DRIVER_NOT_LOADED, true},
		{"the library and the driver disagree on version", ERROR_LIB_RM_VERSION_MISMATCH, true},

		{"success", SUCCESS, false},
		{"the card offers no profile of that width", ERROR_NOT_SUPPORTED, false},
		{"nothing occupies that index", ERROR_NOT_FOUND, false},
		{"the argument is invalid", ERROR_INVALID_ARGUMENT, false},
		{"the caller lacks permission", ERROR_NO_PERMISSION, false},
		{"the library was never initialized", ERROR_UNINITIALIZED, false},
		{"the instance is in use", ERROR_IN_USE, false},
		{"the failure is unknown", ERROR_UNKNOWN, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.IsAPIUnavailable(),
				"Return(%d).IsAPIUnavailable()", tc.ret)
		})
	}
}

// ReportsAbsent separates "the library answered that there is nothing here" from "the library could
// not answer", and both enumerations on this API depend on the line falling exactly here.
//
// The GPU-instance profile sweep walks a fixed slice-count space in which every measured card leaves
// the three-slice slot unsupported, and the MIG-device sweep walks an index space shared by the
// whole node in which most indices belong to another card. Treating those as failures reports a
// healthy card as unreadable; treating a permission refusal or a lost GPU as a gap publishes a card
// as offering less than it does, with nothing said about why.
func TestReturn_ReportsAbsent(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the card offers no profile of that width", ERROR_NOT_SUPPORTED, true},
		{"nothing occupies that index", ERROR_NOT_FOUND, true},
		{"the profile index is outside the enumeration", ERROR_INVALID_ARGUMENT, true},

		{"success", SUCCESS, false},
		{"the caller lacks permission", ERROR_NO_PERMISSION, false},
		{"the library was never initialized", ERROR_UNINITIALIZED, false},
		{"the card has fallen off the bus", ERROR_GPU_IS_LOST, false},
		{"the buffer was too small", ERROR_INSUFFICIENT_SIZE, false},
		{"the instance is in use", ERROR_IN_USE, false},
		{"the symbol is absent from the library", ERROR_FUNCTION_NOT_FOUND, false},
		{"the failure is unknown", ERROR_UNKNOWN, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.ReportsAbsent(),
				"Return(%d).ReportsAbsent()", tc.ret)
		})
	}
}

// A slice count outside the enumeration is refused before the library is reached, which is what lets
// a caller sweep a width range without first knowing how wide the enumeration is.
//
// Zero is the case worth pinning: the argument is converted to the vendor's index by subtracting
// one, so an unguarded zero would underflow to the largest unsigned int rather than being rejected.
// The receiver carries no library handle on purpose -- reaching one would mean the guard did not
// run.
func TestDevice_GetGpuInstanceProfileInfoBySliceCount_RejectsOutOfRange(t *testing.T) {
	testCases := []struct {
		name       string
		sliceCount uint32
	}{
		{"zero would underflow the vendor's index", 0},
		{"one past the enumeration", GPU_INSTANCE_PROFILE_COUNT + 1},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, ret := Device{}.GetGpuInstanceProfileInfoBySliceCount(tc.sliceCount)
			assert.Equal(t, ERROR_INVALID_ARGUMENT, ret)
		})
	}
}

// The same guard on the compute-instance side, for the same reason.
func TestGpuInstance_GetComputeInstanceProfileInfoBySliceCount_RejectsOutOfRange(t *testing.T) {
	testCases := []struct {
		name       string
		sliceCount uint32
	}{
		{"zero would underflow the vendor's index", 0},
		{"one past the enumeration", COMPUTE_INSTANCE_PROFILE_COUNT + 1},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, ret := GpuInstance{}.GetComputeInstanceProfileInfoBySliceCount(tc.sliceCount, 0)
			assert.Equal(t, ERROR_INVALID_ARGUMENT, ret)
		})
	}
}
