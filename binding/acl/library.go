package acl

import "C"
import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"gpustack.ai/gpustack/binding"
)

const (
	SUCCESS                               = 0
	ERROR_INVALID_PARAM                   = 100000
	ERROR_UNINITIALIZE                    = 100001
	ERROR_REPEAT_INITIALIZE               = 100002
	ERROR_INVALID_FILE                    = 100003
	ERROR_WRITE_FILE                      = 100004
	ERROR_INVALID_FILE_SIZE               = 100005
	ERROR_PARSE_FILE                      = 100006
	ERROR_FILE_MISSING_ATTR               = 100007
	ERROR_FILE_ATTR_INVALID               = 100008
	ERROR_INVALID_DUMP_CONFIG             = 100009
	ERROR_INVALID_PROFILING_CONFIG        = 100010
	ERROR_INVALID_MODEL_ID                = 100011
	ERROR_DESERIALIZE_MODEL               = 100012
	ERROR_PARSE_MODEL                     = 100013
	ERROR_READ_MODEL_FAILURE              = 100014
	ERROR_MODEL_SIZE_INVALID              = 100015
	ERROR_MODEL_MISSING_ATTR              = 100016
	ERROR_MODEL_INPUT_NOT_MATCH           = 100017
	ERROR_MODEL_OUTPUT_NOT_MATCH          = 100018
	ERROR_MODEL_NOT_DYNAMIC               = 100019
	ERROR_OP_TYPE_NOT_MATCH               = 100020
	ERROR_OP_INPUT_NOT_MATCH              = 100021
	ERROR_OP_OUTPUT_NOT_MATCH             = 100022
	ERROR_OP_ATTR_NOT_MATCH               = 100023
	ERROR_OP_NOT_FOUND                    = 100024
	ERROR_OP_LOAD_FAILED                  = 100025
	ERROR_UNSUPPORTED_DATA_TYPE           = 100026
	ERROR_FORMAT_NOT_MATCH                = 100027
	ERROR_BIN_SELECTOR_NOT_REGISTERED     = 100028
	ERROR_KERNEL_NOT_FOUND                = 100029
	ERROR_BIN_SELECTOR_ALREADY_REGISTERED = 100030
	ERROR_KERNEL_ALREADY_REGISTERED       = 100031
	ERROR_INVALID_QUEUE_ID                = 100032
	ERROR_REPEAT_SUBSCRIBE                = 100033
	ERROR_STREAM_NOT_SUBSCRIBE            = 100034
	ERROR_THREAD_NOT_SUBSCRIBE            = 100035
	ERROR_WAIT_CALLBACK_TIMEOUT           = 100036
	ERROR_REPEAT_FINALIZE                 = 100037
	ERROR_NOT_STATIC_AIPP                 = 100038
	ERROR_COMPILING_STUB_MODE             = 100039
	ERROR_GROUP_NOT_SET                   = 100040
	ERROR_GROUP_NOT_CREATE                = 100041
	ERROR_PROF_ALREADY_RUN                = 100042
	ERROR_PROF_NOT_RUN                    = 100043
	ERROR_DUMP_ALREADY_RUN                = 100044
	ERROR_DUMP_NOT_RUN                    = 100045
	ERROR_PROF_REPEAT_SUBSCRIBE           = 148046
	ERROR_PROF_API_CONFLICT               = 148047
	ERROR_INVALID_MAX_OPQUEUE_NUM_CONFIG  = 148048
	ERROR_INVALID_OPP_PATH                = 148049
	ERROR_OP_UNSUPPORTED_DYNAMIC          = 148050
	ERROR_RELATIVE_RESOURCE_NOT_CLEARED   = 148051
	ERROR_UNSUPPORTED_JPEG                = 148052
	ERROR_INVALID_BUNDLE_MODEL_ID         = 148053
	ERROR_BAD_ALLOC                       = 200000
	ERROR_API_NOT_SUPPORT                 = 200001
	ERROR_INVALID_DEVICE                  = 200002
	ERROR_MEMORY_ADDRESS_UNALIGNED        = 200003
	ERROR_RESOURCE_NOT_MATCH              = 200004
	ERROR_INVALID_RESOURCE_HANDLE         = 200005
	ERROR_FEATURE_UNSUPPORTED             = 200006
	ERROR_PROF_MODULES_UNSUPPORTED        = 200007
	ERROR_STORAGE_OVER_LIMIT              = 300000
	ERROR_INTERNAL_ERROR                  = 500000
	ERROR_FAILURE                         = 500001
	ERROR_GE_FAILURE                      = 500002
	ERROR_RT_FAILURE                      = 500003
	ERROR_DRV_FAILURE                     = 500004
	ERROR_PROFILING_FAILURE               = 500005
	ERROR_FUNCTION_NOT_FOUND              = -999998
	ERROR_LIBRARY_NOT_FOUND               = -999999
)

type ACL struct {
	so binding.Library
}

// New creates a new ACL library instance.
// It attempts to load the ACL library from the system and sets up the function pointers for the ACL API functions.
func New(opts ...binding.LibraryOption) *ACL {
	soPaths := []string{
		"libascendcl.so",
	}
	{
		var home string
		for _, env := range []string{"ASCEND_HOME_PATH", "ASCEND_TOOLKIT_HOME", "ASCEND_TOOLKIT_LATEST_HOME"} {
			home = os.Getenv(env)
			if home != "" {
				break
			}
		}
		if home == "" {
			home = "/usr/local/Ascend/ascend-toolkit/latest"
		}
		if s, err := os.Stat(home); err == nil && s.IsDir() {
			soPaths = append(soPaths,
				filepath.Join(home, "runtime", "lib64", "libascendcl.so"),
			)
			if runtime.GOARCH == "arm64" {
				soPaths = append(soPaths,
					filepath.Join(home, "aarch64-linux", "lib64", "libascendcl.so"),
				)
			} else {
				soPaths = append(soPaths,
					filepath.Join(home, "x86_64-linux", "lib64", "libascendcl.so"),
				)
			}
		}
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &ACL{so: so}
}

func (l *ACL) Init() Return {
	if l.so.Load() != nil {
		return ERROR_LIBRARY_NOT_FOUND
	}
	return SUCCESS
}

func (l *ACL) Shutdown() Return {
	if l.so.Unload() != nil {
		return ERROR_LIBRARY_NOT_FOUND
	}
	return SUCCESS
}

func (l *ACL) GetSocName() (string, Return) {
	if l.so.Lookup("aclsysGetSocName") != nil {
		return "", ERROR_FUNCTION_NOT_FOUND
	}
	return aclrtGetSocName(), SUCCESS
}

func (l *ACL) GetCANNVersion() (string, Return) {
	if l.so.Lookup("aclsysGetVersionStr") == nil {
		pkgName := []byte("runtime")
		versionStr := make([]byte, PKG_VERSION_MAX_SIZE)
		ret := aclsysGetVersionStr(&pkgName[0], &versionStr[0])
		if ret.IsSuccess() {
			return string(versionStr[:clen(versionStr)]), SUCCESS
		}
	}
	if l.so.Lookup("aclsysGetCANNVersion") == nil {
		var version CANNPackageVersion
		ret := aclsysGetCANNVersion(PKG_NAME_CANN, &version)
		if ret.IsSuccess() {
			return C.GoString((*C.char)(unsafe.Pointer(&version.Version[0]))), SUCCESS
		}
	}
	return "", ERROR_FUNCTION_NOT_FOUND
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
	return defaultErrorStringFunc(int(r))
}

func defaultErrorStringFunc(r int) string {
	switch r {
	case SUCCESS:
		return "SUCCESS"
	case ERROR_INVALID_PARAM:
		return "INVALID_PARAM"
	case ERROR_UNINITIALIZE:
		return "UNINITIALIZE"
	case ERROR_REPEAT_INITIALIZE:
		return "REPEAT_INITIALIZE"
	case ERROR_INVALID_FILE:
		return "INVALID_FILE"
	case ERROR_WRITE_FILE:
		return "WRITE_FILE"
	case ERROR_INVALID_FILE_SIZE:
		return "INVALID_FILE_SIZE"
	case ERROR_PARSE_FILE:
		return "PARSE_FILE"
	case ERROR_FILE_MISSING_ATTR:
		return "FILE_MISSING_ATTR"
	case ERROR_FILE_ATTR_INVALID:
		return "FILE_ATTR_INVALID"
	case ERROR_INVALID_DUMP_CONFIG:
		return "INVALID_DUMP_CONFIG"
	case ERROR_INVALID_PROFILING_CONFIG:
		return "INVALID_PROFILING_CONFIG"
	case ERROR_INVALID_MODEL_ID:
		return "INVALID_MODEL_ID"
	case ERROR_DESERIALIZE_MODEL:
		return "DESERIALIZE_MODEL"
	case ERROR_PARSE_MODEL:
		return "PARSE_MODEL"
	case ERROR_READ_MODEL_FAILURE:
		return "READ_MODEL_FAILURE"
	case ERROR_MODEL_SIZE_INVALID:
		return "MODEL_SIZE_INVALID"
	case ERROR_MODEL_MISSING_ATTR:
		return "MODEL_MISSING_ATTR"
	case ERROR_MODEL_INPUT_NOT_MATCH:
		return "MODEL_INPUT_NOT_MATCH"
	case ERROR_MODEL_OUTPUT_NOT_MATCH:
		return "MODEL_OUTPUT_NOT_MATCH"
	case ERROR_MODEL_NOT_DYNAMIC:
		return "MODEL_NOT_DYNAMIC"
	case ERROR_OP_TYPE_NOT_MATCH:
		return "OP_TYPE_NOT_MATCH"
	case ERROR_OP_INPUT_NOT_MATCH:
		return "OP_INPUT_NOT_MATCH"
	case ERROR_OP_OUTPUT_NOT_MATCH:
		return "OP_OUTPUT_NOT_MATCH"
	case ERROR_OP_ATTR_NOT_MATCH:
		return "OP_ATTR_NOT_MATCH"
	case ERROR_OP_NOT_FOUND:
		return "OP_NOT_FOUND"
	case ERROR_OP_LOAD_FAILED:
		return "OP_LOAD_FAILED"
	case ERROR_UNSUPPORTED_DATA_TYPE:
		return "UNSUPPORTED_DATA_TYPE"
	case ERROR_FORMAT_NOT_MATCH:
		return "FORMAT_NOT_MATCH"
	case ERROR_BIN_SELECTOR_NOT_REGISTERED:
		return "BIN_SELECTOR_NOT_REGISTERED"
	case ERROR_KERNEL_NOT_FOUND:
		return "KERNEL_NOT_FOUND"
	case ERROR_BIN_SELECTOR_ALREADY_REGISTERED:
		return "BIN_SELECTOR_ALREADY_REGISTERED"
	case ERROR_KERNEL_ALREADY_REGISTERED:
		return "KERNEL_ALREADY_REGISTERED"
	case ERROR_INVALID_QUEUE_ID:
		return "INVALID_QUEUE_ID"
	case ERROR_REPEAT_SUBSCRIBE:
		return "REPEAT_SUBSCRIBE"
	case ERROR_STREAM_NOT_SUBSCRIBE:
		return "STREAM_NOT_SUBSCRIBE"
	case ERROR_THREAD_NOT_SUBSCRIBE:
		return "THREAD_NOT_SUBSCRIBE"
	case ERROR_WAIT_CALLBACK_TIMEOUT:
		return "WAIT_CALLBACK_TIMEOUT"
	case ERROR_REPEAT_FINALIZE:
		return "REPEAT_FINALIZE"
	case ERROR_NOT_STATIC_AIPP:
		return "NOT_STATIC_AIPP"
	case ERROR_COMPILING_STUB_MODE:
		return "COMPILING_STUB_MODE"
	case ERROR_GROUP_NOT_SET:
		return "GROUP_NOT_SET"
	case ERROR_GROUP_NOT_CREATE:
		return "GROUP_NOT_CREATE"
	case ERROR_PROF_ALREADY_RUN:
		return "PROF_ALREADY_RUN"
	case ERROR_PROF_NOT_RUN:
		return "PROF_NOT_RUN"
	case ERROR_DUMP_ALREADY_RUN:
		return "DUMP_ALREADY_RUN"
	case ERROR_DUMP_NOT_RUN:
		return "DUMP_NOT_RUN"
	case ERROR_PROF_REPEAT_SUBSCRIBE:
		return "PROF_REPEAT_SUBSCRIBE"
	case ERROR_PROF_API_CONFLICT:
		return "PROF_API_CONFLICT"
	case ERROR_INVALID_MAX_OPQUEUE_NUM_CONFIG:
		return "INVALID_MAX_OPQUEUE_NUM_CONFIG"
	case ERROR_INVALID_OPP_PATH:
		return "INVALID_OPP_PATH"
	case ERROR_OP_UNSUPPORTED_DYNAMIC:
		return "OP_UNSUPPORTED_DYNAMIC"
	case ERROR_RELATIVE_RESOURCE_NOT_CLEARED:
		return "RELATIVE_RESOURCE_NOT_CLEARED"
	case ERROR_UNSUPPORTED_JPEG:
		return "UNSUPPORTED_JPEG"
	case ERROR_INVALID_BUNDLE_MODEL_ID:
		return "INVALID_BUNDLE_MODEL_ID"
	case ERROR_BAD_ALLOC:
		return "BAD_ALLOC"
	case ERROR_API_NOT_SUPPORT:
		return "API_NOT_SUPPORT"
	case ERROR_INVALID_DEVICE:
		return "INVALID_DEVICE"
	case ERROR_MEMORY_ADDRESS_UNALIGNED:
		return "MEMORY_ADDRESS_UNALIGNED"
	case ERROR_RESOURCE_NOT_MATCH:
		return "RESOURCE_NOT_MATCH"
	case ERROR_INVALID_RESOURCE_HANDLE:
		return "INVALID_RESOURCE_HANDLE"
	case ERROR_FEATURE_UNSUPPORTED:
		return "FEATURE_UNSUPPORTED"
	case ERROR_PROF_MODULES_UNSUPPORTED:
		return "PROF_MODULES_UNSUPPORTED"
	case ERROR_STORAGE_OVER_LIMIT:
		return "STORAGE_OVER_LIMIT"
	case ERROR_INTERNAL_ERROR:
		return "INTERNAL_ERROR"
	case ERROR_FAILURE:
		return "FAILURE"
	case ERROR_GE_FAILURE:
		return "GE_FAILURE"
	case ERROR_RT_FAILURE:
		return "RT_FAILURE"
	case ERROR_DRV_FAILURE:
		return "DRV_FAILURE"
	case ERROR_PROFILING_FAILURE:
		return "PROFILING_FAILURE"
	case ERROR_FUNCTION_NOT_FOUND:
		return "FUNCTION_NOT_FOUND"
	case ERROR_LIBRARY_NOT_FOUND:
		return "LIBRARY_NOT_FOUND"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}
