package rsmi

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

type RSMI struct {
	so binding.Library
}

// New creates a new RSMI library instance.
// It attempts to load the RSMI library from the system and sets up the function pointers for the RSMI API functions.
func New(opts ...binding.LibraryOption) *RSMI {
	soPaths := []string{
		"librocm_smi64.so",
	}
	{
		path := os.Getenv("ROCM_SMI_LIB_PATH")
		if path != "" {
			soPaths = append(soPaths,
				filepath.Join(path, "librocm_smi64.so"),
			)
		}

		for _, soPathDir := range []string{
			"/opt/hyhal/lib",
			"/opt/dtk/rocm_smi/lib",
			"/opt/dtk/.hyhal/rocm_smi/lib",
		} {
			if s, err := os.Stat(soPathDir); err == nil && s.IsDir() {
				soPaths = append(soPaths,
					filepath.Join(soPathDir, "librocm_smi64.so"),
				)
			}
		}

		if s, err := os.Stat(home); err == nil && s.IsDir() {
			soPaths = append(soPaths,
				filepath.Join(home, "lib", "librocm_smi64.so"),
				filepath.Join(home, "rocm_smi", "lib", "librocm_smi64.so"),
			)
		}
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &RSMI{so: so}
}

// Init initializes the RSMI library.
func (l *RSMI) Init() Return {
	if err := l.so.Load(); err != nil {
		return STATUS_LIBRARY_NOT_FOUND
	}
	if l.so.Lookup("rsmi_init") != nil {
		return STATUS_FUNCTION_NOT_FOUND
	}
	return rsmiInit(uint64(INIT_FLAG_ALL_GPUS))
}

// InitWithFlags initializes the RSMI library with the specified flags.
func (l *RSMI) InitWithFlags(flags uint64) Return {
	if err := l.so.Load(); err != nil {
		return STATUS_LIBRARY_NOT_FOUND
	}
	if l.so.Lookup("rsmi_init") != nil {
		return STATUS_FUNCTION_NOT_FOUND
	}
	return rsmiInit(flags)
}

// Shutdown shuts down the RSMI library and unloads the library from memory.
func (l *RSMI) Shutdown() Return {
	if l.so.Lookup("rsmi_shut_down") != nil {
		return STATUS_FUNCTION_NOT_FOUND
	}

	ret := rsmiShutDown()
	if !ret.IsSuccess() {
		return ret
	}
	if err := l.so.Unload(); err != nil {
		return STATUS_LIBRARY_NOT_FOUND
	}
	return ret
}

// GetROCMVersion attempts to read the ROCM version from known file paths and returns it as a string.
func (l *RSMI) GetROCMVersion() (string, Return) {
	for _, path := range []string{
		filepath.Join(home, ".info", "version"),
		filepath.Join(home, ".info", "version-rocm"),
		filepath.Join(home, ".info", "version-dev"),
		filepath.Join(home, ".info", "version-libs"),
		filepath.Join(filepath.Dir(home), ".info", "version"),
		filepath.Join(filepath.Dir(home), ".info", "version-rocm"),
		filepath.Join(filepath.Dir(home), ".info", "version-dev"),
		filepath.Join(filepath.Dir(home), ".info", "version-libs"),
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
	case STATUS_INVALID_ARGS:
		return "INVALID_ARGS"
	case STATUS_NOT_SUPPORTED:
		return "NOT_SUPPORTED"
	case STATUS_FILE_ERROR:
		return "FILE_ERROR"
	case STATUS_PERMISSION:
		return "PERMISSION"
	case STATUS_OUT_OF_RESOURCES:
		return "OUT_OF_RESOURCES"
	case STATUS_INTERNAL_EXCEPTION:
		return "INTERNAL_EXCEPTION"
	case STATUS_INPUT_OUT_OF_BOUNDS:
		return "INPUT_OUT_OF_BOUNDS"
	case STATUS_INIT_ERROR:
		return "INIT_ERROR"
	case STATUS_NOT_YET_IMPLEMENTED:
		return "NOT_YET_IMPLEMENTED"
	case STATUS_NOT_FOUND:
		return "NOT_FOUND"
	case STATUS_INSUFFICIENT_SIZE:
		return "INSUFFICIENT_SIZE"
	case STATUS_INTERRUPT:
		return "INTERRUPT"
	case STATUS_UNEXPECTED_SIZE:
		return "UNEXPECTED_SIZE"
	case STATUS_NO_DATA:
		return "NO_DATA"
	case STATUS_UNEXPECTED_DATA:
		return "UNEXPECTED_DATA"
	case STATUS_BUSY:
		return "BUSY"
	case STATUS_REFCOUNT_OVERFLOW:
		return "REFCOUNT_OVERFLOW"
	case STATUS_DIRECTORY_NOT_FOUND:
		return "DIRECTORY_NOT_FOUND"
	case STATUS_SETTING_UNAVAILABLE:
		return "SETTING_UNAVAILABLE"
	case STATUS_AMDGPU_RESTART_ERR:
		return "AMDGPU_RESTART_ERR"
	case STATUS_DRIVER_NOT_LOADED:
		return "DRIVER_NOT_LOADED"
	case STATUS_IPC_ERROR:
		return "IPC_ERROR"
	case STATUS_FUNCTION_NOT_FOUND:
		return "FUNCTION_NOT_FOUND"
	case STATUS_LIBRARY_NOT_FOUND:
		return "LIBRARY_NOT_FOUND"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}
