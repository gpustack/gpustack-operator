package amdsmi

import (
	"fmt"
	"os"
	"path/filepath"

	"gpustack.ai/gpustack/binding"
)

var home string

func init() {
	for _, env := range []string{"ROCM_HOME", "ROCM_PATH"} {
		home = os.Getenv(env)
		if home != "" {
			break
		}
	}
	if home == "" {
		home = "/opt/rocm"
	}
}

const (
	STATUS_FUNCTION_NOT_FOUND = -99998
	STATUS_LIBRARY_NOT_FOUND  = -99999
)

type AMDSMI struct {
	so binding.Library
}

// New creates a new AMDSMI library instance.
// It attempts to load the AMDSMI library from the system and sets up the function pointers for the AMDSMI API functions.
func New(opts ...binding.LibraryOption) *AMDSMI {
	soPaths := []string{
		"libamd_smi.so",
	}
	{
		path := os.Getenv("AMD_SMI_LIB_PATH")
		if path != "" {
			soPaths = append(soPaths,
				filepath.Join(path, "libamd_smi.so"),
			)
		}

		if s, err := os.Stat(home); err == nil && s.IsDir() {
			soPaths = append(soPaths,
				filepath.Join(home, "lib", "libamd_smi.so"),
			)
		}
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &AMDSMI{so: so}
}

// Init initializes the AMDSMI library.
func (l *AMDSMI) Init() Return {
	if err := l.so.Load(); err != nil {
		return STATUS_LIBRARY_NOT_FOUND
	}
	if l.so.Lookup("amdsmi_init") != nil {
		return STATUS_FUNCTION_NOT_FOUND
	}
	return amdsmiInit(uint64(INIT_AMD_GPUS))
}

// InitWithFlags initializes the AMDSMI library with the specified flags.
func (l *AMDSMI) InitWithFlags(flags uint64) Return {
	if err := l.so.Load(); err != nil {
		return STATUS_LIBRARY_NOT_FOUND
	}
	if l.so.Lookup("amdsmi_init") != nil {
		return STATUS_FUNCTION_NOT_FOUND
	}
	return amdsmiInit(flags)
}

// Shutdown shuts down the AMDSMI library and releases any resources it holds.
func (l *AMDSMI) Shutdown() Return {
	if l.so.Lookup("amdsmi_shut_down") != nil {
		return STATUS_FUNCTION_NOT_FOUND
	}
	ret := amdsmiShutDown()
	if !ret.IsSuccess() {
		return ret
	}
	if err := l.so.Unload(); err != nil {
		return STATUS_LIBRARY_NOT_FOUND
	}
	return ret
}

// GetROCMVersion attempts to read the ROCM version from known file paths and returns it as a string.
func (l *AMDSMI) GetROCMVersion() (string, Return) {
	for _, path := range []string{
		filepath.Join(home, ".info", "version"),
		filepath.Join(home, ".info", "version-rocm"),
		filepath.Join(home, ".info", "version-dev"),
		filepath.Join(home, ".info", "version-libs"),
	} {
		if s, err := os.Stat(path); err == nil && s.Mode().IsRegular() {
			c, err := os.ReadFile(path)
			if err == nil {
				return string(c), STATUS_SUCCESS
			}
		}
	}
	return "", STATUS_NOT_FOUND
}

// IsSuccess returns true if the Return value indicates success.
func (r Return) IsSuccess() bool {
	return r == STATUS_SUCCESS
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
	case STATUS_SUCCESS:
		return "SUCCESS"
	case STATUS_INVAL:
		return "INVAL"
	case STATUS_NOT_SUPPORTED:
		return "NOT_SUPPORTED"
	case STATUS_NOT_YET_IMPLEMENTED:
		return "NOT_YET_IMPLEMENTED"
	case STATUS_FAIL_LOAD_MODULE:
		return "FAIL_LOAD_MODULE"
	case STATUS_FAIL_LOAD_SYMBOL:
		return "FAIL_LOAD_SYMBOL"
	case STATUS_DRM_ERROR:
		return "DRM_ERROR"
	case STATUS_API_FAILED:
		return "API_FAILED"
	case STATUS_TIMEOUT:
		return "TIMEOUT"
	case STATUS_RETRY:
		return "RETRY"
	case STATUS_NO_PERM:
		return "NO_PERM"
	case STATUS_INTERRUPT:
		return "INTERRUPT"
	case STATUS_IO:
		return "IO"
	case STATUS_ADDRESS_FAULT:
		return "ADDRESS_FAULT"
	case STATUS_FILE_ERROR:
		return "FILE_ERROR"
	case STATUS_OUT_OF_RESOURCES:
		return "OUT_OF_RESOURCES"
	case STATUS_INTERNAL_EXCEPTION:
		return "INTERNAL_EXCEPTION"
	case STATUS_INPUT_OUT_OF_BOUNDS:
		return "INPUT_OUT_OF_BOUNDS"
	case STATUS_INIT_ERROR:
		return "INIT_ERROR"
	case STATUS_REFCOUNT_OVERFLOW:
		return "REFCOUNT_OVERFLOW"
	case STATUS_DIRECTORY_NOT_FOUND:
		return "DIRECTORY_NOT_FOUND"
	case STATUS_IPC_ERROR:
		return "IPC_ERROR"
	case STATUS_BUSY:
		return "BUSY"
	case STATUS_NOT_FOUND:
		return "NOT_FOUND"
	case STATUS_NOT_INIT:
		return "NOT_INIT"
	case STATUS_NO_SLOT:
		return "NO_SLOT"
	case STATUS_DRIVER_NOT_LOADED:
		return "DRIVER_NOT_LOADED"
	case STATUS_MORE_DATA:
		return "MORE_DATA"
	case STATUS_NO_DATA:
		return "NO_DATA"
	case STATUS_INSUFFICIENT_SIZE:
		return "INSUFFICIENT_SIZE"
	case STATUS_UNEXPECTED_SIZE:
		return "UNEXPECTED_SIZE"
	case STATUS_UNEXPECTED_DATA:
		return "UNEXPECTED_DATA"
	case STATUS_NON_AMD_CPU:
		return "NON_AMD_CPU"
	case STATUS_NO_ENERGY_DRV:
		return "NO_ENERGY_DRV"
	case STATUS_NO_MSR_DRV:
		return "NO_MSR_DRV"
	case STATUS_NO_HSMP_DRV:
		return "NO_HSMP_DRV"
	case STATUS_NO_HSMP_SUP:
		return "NO_HSMP_SUP"
	case STATUS_NO_HSMP_MSG_SUP:
		return "NO_HSMP_MSG_SUP"
	case STATUS_HSMP_TIMEOUT:
		return "HSMP_TIMEOUT"
	case STATUS_NO_DRV:
		return "NO_DRV"
	case STATUS_FILE_NOT_FOUND:
		return "FILE_NOT_FOUND"
	case STATUS_ARG_PTR_NULL:
		return "ARG_PTR_NULL"
	case STATUS_AMDGPU_RESTART_ERR:
		return "AMDGPU_RESTART_ERR"
	case STATUS_SETTING_UNAVAILABLE:
		return "SETTING_UNAVAILABLE"
	case STATUS_CORRUPTED_EEPROM:
		return "CORRUPTED_EEPROM"
	case STATUS_FUNCTION_NOT_FOUND:
		return "FUNCTION_NOT_FOUND"
	case STATUS_LIBRARY_NOT_FOUND:
		return "LIBRARY_NOT_FOUND"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}
