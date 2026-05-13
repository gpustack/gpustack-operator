package dcmi

import (
	"fmt"
	"os"
	"path/filepath"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding"
)

type DCMI struct {
	so binding.Library
}

// New creates a new DCMI library instance.
// It attempts to load the DCMI library from the system and sets up the function pointers for the DCMI API functions.
func New(opts ...binding.LibraryOption) *DCMI {
	soPaths := []string{
		"libdcmi.so",
	}
	{
		home := os.Getenv("CANN_HOME")
		if home == "" {
			home = "/usr/local/Ascend"
		}
		if s, err := os.Stat(home); err == nil && s.IsDir() {
			soPaths = append(soPaths,
				filepath.Join(home, "driver", "lib64", "driver", "libdcmi.so"),
			)
		}
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &DCMI{so: so}
}

// Init initializes the DCMI library.
func (l *DCMI) Init(logger klog.Logger) Return {
	ret := Return(dcmiInit(l.so.Path()))
	if !ret.IsSuccess() {
		errStr := dcmiLastError()
		logger.Errorf(nil, "dcmiInit(%s): %s", l.so.Path(), errStr)
	}
	return ret
}

// GetDriverVersion retrieves the version of the DCMI driver.
func (l *DCMI) GetDriverVersion() (string, Return) {
	version := make([]byte, MAX_VER_LEN)
	ret := Return(dcmiGetDriverVersion(&version[0], uint32(len(version))))
	return string(version[:clen(version)]), ret
}

type Return int

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
	return defaultErrorStringFunc(int(r))
}

func defaultErrorStringFunc(r int) string {
	switch r {
	case SUCCESS:
		return "SUCCESS"
	case ERROR_INVALID_PARAMETER:
		return "INVALID_PARAMETER"
	case ERROR_MEM_OPERATE_FAIL:
		return "MEM_OPERATE_FAIL"
	case ERROR_INVALID_DEVICE_ID:
		return "INVALID_DEVICE_ID"
	case ERROR_DEVICE_NOT_EXIST:
		return "DEVICE_NOT_EXIST"
	case ERROR_CONFIG_INFO_NOT_EXIST:
		return "CONFIG_INFO_NOT_EXIST"
	case ERROR_OPER_NOT_PERMITTED:
		return "OPER_NOT_PERMITTED"
	case ERROR_NOT_SUPPORT_IN_CONTAINER:
		return "NOT_SUPPORT_IN_CONTAINER"
	case ERROR_NOT_SUPPORT:
		return "NOT_SUPPORT"
	case ERROR_TIME_OUT:
		return "TIME_OUT"
	case ERROR_NOT_REDAY:
		return "NOT_REDAY"
	case ERROR_IS_UPGRADING:
		return "IS_UPGRADING"
	case ERROR_RESOURCE_OCCUPIED:
		return "RESOURCE_OCCUPIED"
	case ERROR_SECURE_FUN_FAIL:
		return "SECURE_FUN_FAIL"
	case ERROR_INNER_ERR:
		return "INNER_ERR"
	case ERROR_IOCTL_FAIL:
		return "IOCTL_FAIL"
	case ERROR_SEND_MSG_FAIL:
		return "SEND_MSG_FAIL"
	case ERROR_RECV_MSG_FAIL:
		return "RECV_MSG_FAIL"
	case ERROR_RESET_FAIL:
		return "RESET_FAIL"
	case ERROR_ABORT_OPERATE:
		return "ABORT_OPERATE"
	case ERROR_FUNCTION_NOT_FOUND:
		return "FUNCTION_NOT_FOUND"
	case ERROR_LIBRARY_NOT_FOUND:
		return "LIBRARY_NOT_FOUND"
	case ERROR_UNKNOWN:
		return "UNKNOWN"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}
