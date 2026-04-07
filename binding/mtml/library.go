package mtml

import (
	"fmt"

	"gpustack.ai/gpustack/binding"
)

type MTML struct {
	so  binding.Library
	lib *mtmlLibrary
}

// New creates a new MTML library instance.
// It attempts to load the MTML library from the system and sets up the function pointers for the MTML API functions.
func New(opts ...binding.LibraryOption) *MTML {
	soPaths := []string{
		"libmtml.so",
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &MTML{so: so}
}

const (
	ERROR_FUNCTION_NOT_FOUND Return = -999
	ERROR_LIBRARY_NOT_FOUND  Return = -1000
)

// Init initializes the MTML library.
func (l *MTML) Init() Return {
	if err := l.so.Load(); err != nil {
		return ERROR_LIBRARY_NOT_FOUND
	}
	if l.so.Lookup("mtmlLibraryInit") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}
	return mtmlLibraryInit(&l.lib)
}

// Shutdown shuts down the MTML library and releases any resources it holds.
func (l *MTML) Shutdown() Return {
	if l.so.Lookup("mtmlLibraryShutDown") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}

	ret := mtmlLibraryShutDown(l.lib)
	if !ret.IsSuccess() {
		return ret
	}
	if err := l.so.Unload(); err != nil {
		return ERROR_UNKNOWN
	}
	return ret
}

const (
	MTML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE = 80
)

// SystemGetDriverVersion retrieves the version of the Driver.
func (l *MTML) SystemGetDriverVersion() (string, Return) {
	if l.so.Lookup("mtmlSystemGetDriverVersion") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}

	var sys *mtmlSystem
	if ret := mtmlLibraryInitSystem(l.lib, &sys); !ret.IsSuccess() {
		return "", ret
	}
	defer func() {
		_ = mtmlLibraryFreeSystem(sys)
	}()

	version := make([]byte, MTML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE)
	ret := mtmlSystemGetDriverVersion(sys, &version[0], MTML_SYSTEM_DRIVER_VERSION_BUFFER_SIZE)
	return string(version[:clen(version)]), ret
}

// IsSuccess returns true if the Return value indicates success.
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
		return "SUCCESS"
	case ERROR_DRIVER_NOT_LOADED:
		return "DRIVER_NOT_LOADED"
	case ERROR_DRIVER_FAILURE:
		return "DRIVER_FAILURE"
	case ERROR_INVALID_ARGUMENT:
		return "INVALID_ARGUMENT"
	case ERROR_NOT_SUPPORTED:
		return "NOT_SUPPORTED"
	case ERROR_NO_PERMISSION:
		return "NO_PERMISSION"
	case ERROR_INSUFFICIENT_SIZE:
		return "INSUFFICIENT_SIZE"
	case ERROR_NOT_FOUND:
		return "NOT_FOUND"
	case ERROR_INSUFFICIENT_MEMORY:
		return "INSUFFICIENT_MEMORY"
	case ERROR_DRIVER_TOO_OLD:
		return "DRIVER_TOO_OLD"
	case ERROR_DRIVER_TOO_NEW:
		return "DRIVER_TOO_NEW"
	case ERROR_TIMEOUT:
		return "TIMEOUT"
	case ERROR_RESOURCE_IS_BUSY:
		return "RESOURCE_IS_BUSY"
	case ERROR_UNKNOWN:
		return "UNKNOWN"
	case ERROR_FUNCTION_NOT_FOUND:
		return "FUNCTION_NOT_FOUND"
	case ERROR_LIBRARY_NOT_FOUND:
		return "LIBRARY_NOT_FOUND"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}
