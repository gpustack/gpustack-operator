package mxsml

import (
	"fmt"
	"os"
	"path/filepath"

	"gpustack.ai/gpustack/binding"
)

type MXSML struct {
	so binding.Library
}

// New creates a new MXSML library instance.
// It attempts to load the MXSML library from the system and sets up the function pointers for the MXSML API functions.
func New(opts ...binding.LibraryOption) *MXSML {
	soPaths := []string{
		"libmxsml.so",
		"/opt/mxdriver/lib/libmxsml.so",
		"/opt/mxn100/lib/libmxsml.so",
	}
	{
		home := os.Getenv("MACA_HOME")
		if home == "" {
			home = "/opt/maca"
		}
		if s, err := os.Stat(home); err == nil && s.IsDir() {
			soPaths = append(soPaths,
				filepath.Join(home, "lib64", "libmxsml.so"),
				filepath.Join(home, "lib", "libmxsml.so"),
			)
		}
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &MXSML{so: so}
}

const (
	FunctionNotFound Return = -999
	LibraryNotFound  Return = -1000
)

// Init initializes the MXSML library.
func (l *MXSML) Init() Return {
	if err := l.so.Load(); err != nil {
		return LibraryNotFound
	}
	if l.so.Lookup("mxSmlInit") != nil {
		return FunctionNotFound
	}
	return mxSmlInit()
}

// InitWithFlags initializes the MXSML library with the specified flags.
func (l *MXSML) InitWithFlags(flags uint32) Return {
	if err := l.so.Load(); err != nil {
		return LibraryNotFound
	}
	if l.so.Lookup("mxSmlInitWithFlags") != nil {
		return FunctionNotFound
	}
	return mxSmlInitWithFlags(flags)
}

// GetDriverVersion retrieves the version of the MXSML driver.
func (l *MXSML) GetDriverVersion() (string, Return) {
	if l.so.Lookup("mxSmlGetDeviceVersion") != nil {
		return "", FunctionNotFound
	}

	version := make([]byte, VERSION_INFO_SIZE)
	var versionLen uint32
	ret := mxSmlGetDeviceVersion(0, Version_Driver, &version[0], &versionLen)
	return string(version[:versionLen]), ret
}

// GetMacaVersion retrieves the version of the MACA version.
func (l *MXSML) GetMacaVersion() (string, Return) {
	if l.so.Lookup("mxSmlGetMacaVersion") != nil {
		return "", FunctionNotFound
	}

	version := make([]byte, VERSION_INFO_SIZE)
	var versionLen uint32
	ret := mxSmlGetMacaVersion(&version[0], &versionLen)
	return string(version[:versionLen]), ret
}

// IsSuccess returns true if the Return value indicates success.
func (r Return) IsSuccess() bool {
	return r == Success
}

// IsAPIUnavailable reports whether the Return says the call could not be made at all, because the
// loaded library or the installed driver does not offer it: the shared object is absent, the symbol
// is missing from it, or the library and the driver are version-incompatible as a pair.
//
// It is false for every code a caller could act on differently — another struct version, another
// device, different permissions — and for every code that is the driver's own answer about a
// device, OperationNotSupport above all.
func (r Return) IsAPIUnavailable() bool {
	switch r {
	case LibraryNotFound, FunctionNotFound, LoadDllFailure:
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
	case Success:
		return "Success"
	case Failure:
		return "Failure"
	case NoDevice:
		return "NoDevice"
	case OperationNotSupport:
		return "OperationNotSupport"
	case SysfsError:
		return "SysfsError"
	case SysfsWriteError:
		return "SysfsWriteError"
	case InvalidDeviceId:
		return "InvalidDeviceId"
	case InvalidDieId:
		return "InvalidDieId"
	case PermissionDenied:
		return "PermissionDenied"
	case InvalidInput:
		return "InvalidInput"
	case InsufficientSize:
		return "InsufficientSize"
	case Reserved3:
		return "Reserved3"
	case IOControlFailure:
		return "IOControlFailure"
	case MmapFailure:
		return "MmapFailure"
	case UnMmapFailure:
		return "UnMmapFailure"
	case InvalidInputForMmap:
		return "InvalidInputForMmap"
	case Reserved1:
		return "Reserved1"
	case Reserved2:
		return "Reserved2"
	case TargetVfNotFound:
		return "TargetVfNotFound"
	case InvalidFrequency:
		return "InvalidFrequency"
	case FlrNotReady:
		return "FlrNotReady"
	case OpenDeviceFileFailure:
		return "OpenDeviceFileFailure"
	case CloseDeviceFileFailure:
		return "CloseDeviceFileFailure"
	case BusyDevice:
		return "BusyDevice"
	case MmioNotEnough:
		return "MmioNotEnough"
	case GetPciBridgeFailure:
		return "GetPciBridgeFailure"
	case LoadDllFailure:
		return "LoadDllFailure"
	case FunctionNotFound:
		return "FunctionNotFound"
	case LibraryNotFound:
		return "LibraryNotFound"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}
