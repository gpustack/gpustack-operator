package cndev

import (
	"fmt"
	"os"
	"path/filepath"

	"gpustack.ai/gpustack/binding"
)

const (
	ERROR_FUNCTION_NOT_FOUND = -99998
	ERROR_LIBRARY_NOT_FOUND  = -99999
)

type CNDev struct {
	so binding.Library
}

// New creates a new CNDev library instance.
// It attempts to load the CNDev library from the system and sets up the function pointers for the CNDev API functions.
func New(opts ...binding.LibraryOption) *CNDev {
	soPaths := []string{
		"libcndev.so",
	}
	{
		home := os.Getenv("NEUWARE_HOME")
		if home == "" {
			home = "/usr/local/neuware"
		}
		if s, err := os.Stat(home); err == nil && s.IsDir() {
			soPaths = append(soPaths,
				filepath.Join(home, "lib64", "libcndev.so"),
				filepath.Join(home, "lib", "libcndev.so"),
			)
		}
	}

	so := binding.NewLibrary(soPaths, opts...)
	return &CNDev{so: so}
}

func (l *CNDev) Init() Return {
	if err := l.so.Load(); err != nil {
		return ERROR_LIBRARY_NOT_FOUND
	}
	if l.so.Lookup("cndevInit") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}
	return cndevInit(0)
}

func (l *CNDev) Release() Return {
	if l.so.Lookup("cndevRelease") != nil {
		return ERROR_FUNCTION_NOT_FOUND
	}
	ret := cndevRelease()
	if !ret.IsSuccess() {
		return ret
	}
	if err := l.so.Unload(); err != nil {
		return ERROR_LIBRARY_NOT_FOUND
	}
	return ret
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
	case ERROR_NO_DRIVER:
		return "NO_DRIVER"
	case ERROR_LOW_DRIVER_VERSION:
		return "LOW_DRIVER_VERSION"
	case ERROR_UNSUPPORTED_API_VERSION:
		return "UNSUPPORTED_API_VERSION"
	case ERROR_UNINITIALIZED:
		return "UNINITIALIZED"
	case ERROR_INVALID_ARGUMENT:
		return "INVALID_ARGUMENT"
	case ERROR_INVALID_DEVICE_ID:
		return "INVALID_DEVICE_ID"
	case ERROR_UNKNOWN:
		return "UNKNOWN"
	case ERROR_MALLOC:
		return "MALLOC"
	case ERROR_INSUFFICIENT_SPACE:
		return "INSUFFICIENT_SPACE"
	case ERROR_NOT_SUPPORTED:
		return "NOT_SUPPORTED"
	case ERROR_INVALID_LINK:
		return "INVALID_LINK"
	case ERROR_NO_DEVICES:
		return "NO_DEVICES"
	case ERROR_NO_PERMISSION:
		return "NO_PERMISSION"
	case ERROR_NOT_FOUND:
		return "NOT_FOUND"
	case ERROR_IN_USE:
		return "IN_USE"
	case ERROR_DUPLICATE:
		return "DUPLICATE"
	case ERROR_TIMEOUT:
		return "TIMEOUT"
	case ERROR_IN_PROBLEM:
		return "IN_PROBLEM"
	case ERROR_FUNCTION_NOT_FOUND:
		return "FUNCTION_NOT_FOUND"
	case ERROR_LIBRARY_NOT_FOUND:
		return "LIBRARY_NOT_FOUND"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}
