package ixml

import (
	"fmt"
	"os"
	"path/filepath"

	"gpustack.ai/gpustack/binding"
)

type IXML struct {
	so binding.Library
}

// New creates a new IXML library instance.
// It attempts to load the IXML library from the system and sets up the function pointers for the IXML API functions.
func New(opts ...binding.LibraryOption) *IXML {
	soPaths := []string{
		"libixml.so",
	}
	{
		home := os.Getenv("COREX_HOME")
		if home == "" {
			home = "/usr/local/corex"
		}
		if s, err := os.Stat(home); err == nil && s.IsDir() {
			soPaths = append(soPaths,
				filepath.Join(home, "lib64", "libixml.so"),
				filepath.Join(home, "lib", "libixml.so"),
			)
		}
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &IXML{so: so}
}

// Init initializes the IXML library.
func (l *IXML) Init() Return {
	if err := l.so.Load(); err != nil {
		return ERROR_LIBRARY_NOT_FOUND
	}
	if l.so.Lookup("nvmlInit_v2") == nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	return nvmlInit_v2()
}

// Shutdown shuts down the IXML library and releases any resources it holds.
func (l *IXML) Shutdown() Return {
	if l.so.Lookup("nvmlShutdown") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	ret := nvmlShutdown()
	if !ret.IsSuccess() {
		return ret
	}
	if err := l.so.Unload(); err != nil {
		return ERROR_UNKNOWN
	}
	return ret
}

// SystemGetDriverVersion retrieves the version of the Driver.
func (l *IXML) SystemGetDriverVersion() (string, Return) {
	if l.so.Lookup("nvmlSystemGetDriverVersion") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	version := make([]byte, SYSTEM_DRIVER_VERSION_BUFFER_SIZE)
	ret := nvmlSystemGetDriverVersion(&version[0], SYSTEM_DRIVER_VERSION_BUFFER_SIZE)
	return string(version[:clen(version)]), ret
}

// SystemGetCudaDriverVersion retrieves the version of the CoreX driver.
func (l *IXML) SystemGetCudaDriverVersion() (int32, Return) {
	var version int32
	if l.so.Lookup("nvmlSystemGetCudaDriverVersion") != nil {
		ret := nvmlSystemGetCudaDriverVersion(&version)
		return version, ret
	}
	if l.so.Lookup("nvmlSystemGetCudaDriverVersion_v2") != nil {
		ret := nvmlSystemGetCudaDriverVersion_v2(&version)
		return version, ret
	}
	return 0, ERROR_FUNCTION_NOT_FOUND
}

// IsSuccess returns true if the Return value indicates success.
func (r Return) IsSuccess() bool {
	return r == SUCCESS
}

// IsAPIUnavailable reports whether the Return says the call could not be made at all, because the
// loaded library or the installed driver does not offer it: the shared object is absent, the symbol
// is missing from it, or the library and the driver are version-incompatible as a pair.
//
// It is false for every code a caller could act on differently — another struct version, another
// device, different permissions — and for every code that is the driver's own answer about a
// device, ERROR_NOT_SUPPORTED above all.
func (r Return) IsAPIUnavailable() bool {
	switch r {
	case ERROR_LIBRARY_NOT_FOUND, ERROR_FUNCTION_NOT_FOUND,
		ERROR_DRIVER_NOT_LOADED, ERROR_LIB_RM_VERSION_MISMATCH:
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
	return defaultErrorStringFunc(r)
}

func defaultErrorStringFunc(r Return) string {
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
	case ERROR_UNKNOWN:
		return "UNKNOWN"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}
