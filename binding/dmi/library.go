// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dmi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
)

// DMI is the Hygon Multi-Instance management interface: the API that carves a DCU into GPU
// instances and compute instances, and reports what has been carved.
//
// # Why this binding does not look like the others
//
// The vendor exports this API under NVML's symbol names while implementing something else. Every
// other binding here calls its C functions by name and lets the dynamic loader resolve them from a
// library opened RTLD_GLOBAL; doing that with these names would let this package's calls bind to
// libnvidia-ml.so in a process that has loaded both, with structs that do not agree on their
// layout. So the C side of this package reaches the library through a hand-written wrapper that
// dlopens RTLD_LOCAL and resolves each function against that one handle. binding.Library is used
// here only to pick a path -- Load is never called on it, because the wrapper does its own opening.
type DMI struct {
	so binding.Library
}

// New creates a new DMI library instance.
//
// The library ships in the hyhal tree, which is not on the dynamic linker's search path, so the
// absolute locations are named here: that is what makes it loadable on a Hygon host without a
// caller exporting anything. The versioned soname is tried before the bare one because a host can
// carry the former without the latter.
func New(opts ...binding.LibraryOption) *DMI {
	soPaths := []string{
		"libhydmi_mig.so.1",
		"libhydmi_mig.so",
	}
	for _, dir := range []string{
		"/opt/hyhal/lib",
		"/opt/dtk/lib",
	} {
		if s, err := os.Stat(dir); err == nil && s.IsDir() {
			soPaths = append(soPaths,
				filepath.Join(dir, "libhydmi_mig.so.1"),
				filepath.Join(dir, "libhydmi_mig.so"),
			)
		}
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &DMI{so: so}
}

// Init loads the vendor library and resolves every function in it.
//
// Calling it again for the library already held succeeds without reopening it, so the detector and
// the allocator can each initialize independently and neither has to know of the other.
func (l *DMI) Init(logger klog.Logger) Return {
	ret, errStr := l.initLocked()
	if !ret.IsSuccess() {
		logger.Error(ret, "failed to initialize the hygon mig library",
			"path", l.so.Path(), "reason", errStr)
	}
	return ret
}

// initLocked runs the load and reads its reason on one OS thread, returning both.
//
// The reason lives in a thread-local buffer inside the C wrapper, and the runtime is free to resume
// a goroutine on a different thread after a cgo call -- a dlopen plus forty dlsyms being exactly the
// kind of long call where that happens. A reason read after such a hand-off would come from another
// thread's empty buffer, leaving an operator with a bare library path and nothing else.
func (l *DMI) initLocked() (Return, string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ret := dmi_mig_init(l.so.Path())
	if ret.IsSuccess() {
		return ret, ""
	}
	// Cloned, not returned as read: the generated accessor builds a Go string that BORROWS the C
	// buffer rather than copying it, and the buffer is freed to be rewritten the moment the deferred
	// unlock above releases this thread. A borrowed reason read by the caller is whatever the next
	// failure on that thread wrote, or nothing.
	return ret, strings.Clone(dmi_mig_last_error())
}

// Path reports which shared object this instance resolved to, for diagnostics.
func (l *DMI) Path() string {
	return l.so.Path()
}

// GetSystemMigMode reports the node's Multi-Instance mode, current and pending.
//
// The mode is a property of the NODE, not of a card: the vendor tool refuses a device selector on
// its mode switch, and this call takes no device. That is the deepest difference from NVIDIA's MIG,
// where each card carries its own mode, and it is why a caller must never ask "is this card
// partitioned" and expect the answer to differ between two cards of one host.
func (l *DMI) GetSystemMigMode() (current, pending uint32, ret Return) {
	ret = nvmlGetSystemMigMode(&current, &pending)
	return current, pending, ret
}

// GetDeviceCount reports how many physical DCUs the library sees.
func (l *DMI) GetDeviceCount() (uint32, Return) {
	var count uint32
	ret := nvmlDeviceGetCount(&count)
	return count, ret
}

// GetDeviceHandleByIndex returns a handle for the physical DCU at index.
func (l *DMI) GetDeviceHandleByIndex(index uint32) (Device, Return) {
	var handle dmiDevice
	ret := nvmlDeviceGetHandleByIndex(index, &handle)
	return Device{handle: handle, lib: l}, ret
}

// GetDeviceHandleByPciBusId returns a handle for the physical DCU at a PCI address.
//
// This is how a caller holding an identity from another library -- RSMI's, which is what the rest of
// the Hygon detector enumerates by -- reaches the same card here. It is the only such bridge that
// works: this library's own PCI query returns success and writes an empty string, its UUID lookup
// answers NOT_SUPPORTED, and it has no GetUUID at all.
//
// The address must be domain-qualified, "0000:09:00.0" rather than "09:00.0"; the short form is
// refused with INVALID_ARGUMENT. RSMI's GetPciId already renders the long form. An address no card
// answers for is NOT_FOUND, which is a clean absence rather than a fault.
func (l *DMI) GetDeviceHandleByPciBusId(pciBusID string) (Device, Return) {
	var handle dmiDevice
	ret := nvmlDeviceGetHandleByPciBusId(pciBusID, &handle)
	return Device{handle: handle, lib: l}, ret
}

// IsSuccess reports whether the call succeeded.
func (r Return) IsSuccess() bool {
	return r == SUCCESS
}

// IsAPIUnavailable reports whether the Return says the call could not be made at all, because the
// loaded library does not offer it. It is false for every code that is the library's own answer
// about a device -- ERROR_NOT_SUPPORTED and ERROR_NOT_FOUND above all, which this API returns
// routinely for a profile a card does not offer and an instance index nothing occupies.
func (r Return) IsAPIUnavailable() bool {
	switch r {
	case ERROR_LIBRARY_NOT_FOUND, ERROR_FUNCTION_NOT_FOUND,
		ERROR_DRIVER_NOT_LOADED, ERROR_LIB_RM_VERSION_MISMATCH:
		return true
	}
	return false
}

// ReportsAbsent reports whether a non-success return is the library ANSWERING that it has nothing at
// the id or index asked about, rather than failing to answer at all.
//
// The distinction carries real weight on this API, because both of its enumerations walk a fixed
// space and expect gaps: the GPU-instance profile space has an unsupported slot in the middle on
// every card measured, and the MIG-device index space is shared by the whole node, so most indices
// belong to another card. Treating those as failures would report a healthy card as unreadable;
// treating a genuine failure as a gap would publish a card as offering less than it does.
func (r Return) ReportsAbsent() bool {
	switch r {
	case ERROR_NOT_SUPPORTED, ERROR_NOT_FOUND, ERROR_INVALID_ARGUMENT:
		return true
	}
	return false
}

// String returns the string representation of a Return.
func (r Return) String() string {
	return r.Error()
}

// Error returns the string representation of a Return.
func (r Return) Error() string {
	switch r {
	case SUCCESS:
		return "SUCCESS"
	case ERROR_UNINITIALIZED:
		return "UNINITIALIZED"
	case ERROR_INVALID_ARGUMENT:
		return "INVALID_ARGUMENT"
	case ERROR_NOT_SUPPORTED:
		return "NOT_SUPPORTED"
	case ERROR_NO_PERMISSION:
		return "NO_PERMISSION"
	case ERROR_ALREADY_INITIALIZED:
		return "ALREADY_INITIALIZED"
	case ERROR_NOT_FOUND:
		return "NOT_FOUND"
	case ERROR_INSUFFICIENT_SIZE:
		return "INSUFFICIENT_SIZE"
	case ERROR_INSUFFICIENT_POWER:
		return "INSUFFICIENT_POWER"
	case ERROR_DRIVER_NOT_LOADED:
		return "DRIVER_NOT_LOADED"
	case ERROR_TIMEOUT:
		return "TIMEOUT"
	case ERROR_IRQ_ISSUE:
		return "IRQ_ISSUE"
	case ERROR_LIBRARY_NOT_FOUND:
		return "LIBRARY_NOT_FOUND"
	case ERROR_FUNCTION_NOT_FOUND:
		return "FUNCTION_NOT_FOUND"
	case ERROR_CORRUPTED_INFOROM:
		return "CORRUPTED_INFOROM"
	case ERROR_GPU_IS_LOST:
		return "GPU_IS_LOST"
	case ERROR_RESET_REQUIRED:
		return "RESET_REQUIRED"
	case ERROR_OPERATING_SYSTEM:
		return "OPERATING_SYSTEM"
	case ERROR_LIB_RM_VERSION_MISMATCH:
		return "LIB_RM_VERSION_MISMATCH"
	case ERROR_IN_USE:
		return "IN_USE"
	case ERROR_MEMORY:
		return "MEMORY"
	case ERROR_NO_DATA:
		return "NO_DATA"
	case ERROR_VGPU_ECC_NOT_SUPPORTED:
		return "VGPU_ECC_NOT_SUPPORTED"
	case ERROR_INSUFFICIENT_RESOURCES:
		return "INSUFFICIENT_RESOURCES"
	case ERROR_FREQ_NOT_SUPPORTED:
		return "FREQ_NOT_SUPPORTED"
	case ERROR_ARGUMENT_VERSION_MISMATCH:
		return "ARGUMENT_VERSION_MISMATCH"
	case ERROR_DEPRECATED:
		return "DEPRECATED"
	case ERROR_NOT_READY:
		return "NOT_READY"
	case ERROR_UNKNOWN:
		return "UNKNOWN"
	}
	return "UNKNOWN_RETURN_CODE"
}
