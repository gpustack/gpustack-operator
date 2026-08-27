package dcmi

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
//
// The library serves one of two API generations. Init tries the V1 entry point first and falls back
// to the V2 one only when V1's answer means this driver does not serve V1 at all; APIVersion then
// reports which answered. When neither does, the V1 code is returned — an operator on a V1 fleet has
// to act on that one — and the V2 code travels out in the logged error string.
//
// # Why the initialization is pinned to one OS thread
//
// The reason for a failed initialization lives in a thread-local buffer inside the C wrapper, and
// getting it out takes a second cgo call. The runtime is free to resume a goroutine on a different
// thread after a cgo call — and this one is the longest in the package, a dlopen plus a vendor init,
// so it is exactly where a hand-off happens. The read would then find another thread's empty buffer.
// initLocked below therefore holds both calls on one thread.
//
// This binding is the only one that has to arrange that for itself, which is why the pinning looks
// out of place beside the other ten. Every other binding here opens its library through
// binding.Library.Load, which goes to dl.DynamicLibrary.Open, which already pins dlopen and the
// dlerror that reads its reason. DCMI does not: it uses binding.Library only to resolve a path
// (l.so.Path), and hands that path to the C wrapper, which does its own dlopen, keeps its own
// thread-local error buffer, and is reached through cgo rather than through dl. None of dl's
// protection applies on this path, so the same hazard reappears here and is answered the same way.
//
// It also matters more here than the general case: when both API generations refuse, this returns the
// V1 code, and the V2 code exists nowhere but that string. Losing it leaves an operator with a bare
// library path and no reason at all.
func (l *DCMI) Init(logger klog.Logger) Return {
	ret, errStr := l.initLocked()
	if !ret.IsSuccess() {
		logger.Errorf(nil, "dcmiInit(%s): %s", l.so.Path(), errStr)
		return ret
	}

	// Only V2 is worth a line: V1 is what every existing node answers, and a log entry per
	// initialization saying so would be noise. A V2 host is the new case, and knowing the binding
	// took that path is the first thing to establish when its readings look wrong.
	if version := l.APIVersion(); version == APIVersionV2 {
		logger.Infof("dcmiInit(%s): the driver serves the %s API", l.so.Path(), version)
	}

	return ret
}

// initLocked runs the vendor initialization and reads its reason on one OS thread, returning both.
// See Init for why that has to be one thread.
//
// The pinned region is those two calls and nothing else. Logging is deliberately left outside it: the
// container-share driver retries this on every Allocate while the library is unready, and holding a
// thread across a log write on that path takes an M out of the scheduler for no benefit.
func (l *DCMI) initLocked() (Return, string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ret := Return(dcmiInit(l.so.Path()))
	if ret.IsSuccess() {
		return ret, ""
	}

	return ret, dcmiLastError()
}

// APIVersion identifies which generation of the DCMI API a library's initialization answered.
type APIVersion int32

const (
	// APIVersionUnknown means no initialization has succeeded against the library currently held.
	//
	// It is the zero value deliberately: a DCMI whose Init never ran, or ran and failed, must not
	// report V1. Reporting a generation that nothing established would send every adapted call down
	// the V1 path on a host that may well be V2.
	APIVersionUnknown APIVersion = API_VERSION_UNKNOWN
	// APIVersionV1 is the generation every Ascend driver up to and including 910B/310P serves.
	APIVersionV1 APIVersion = API_VERSION_V1
	// APIVersionV2 is the generation the A5/950 driver serves. It enumerates devices flat, with no
	// card level, and declares no counterpart for a number of V1 queries.
	APIVersionV2 APIVersion = API_VERSION_V2
)

// String returns the string representation of an APIVersion.
func (v APIVersion) String() string {
	switch v {
	case APIVersionV1:
		return "V1"
	case APIVersionV2:
		return "V2"
	case APIVersionUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("unknown API version: %d", int32(v))
	}
}

// APIVersion reports which generation of the DCMI API answered this library's initialization.
//
// It reads the loaded library on every call and caches nothing, which is a correctness requirement
// rather than a style choice. The library is process-global while its holders are not: in one
// device-manager the accelerator detector initializes it once through sync.Once and never retries,
// while the allocator's container-share driver retries on every call. A generation cached on either
// holder could be recorded as Unknown or V1 from an initialization that failed, then never revised
// after the other holder succeeded through V2 — leaving every adapted call on the V1 path against a
// V2 driver, and discovery permanently empty.
func (l *DCMI) APIVersion() APIVersion {
	return APIVersion(dcmiApiVersion())
}

// Unlike every other binding here, DCMI exposes no Shutdown. Unloading the library blanks the
// function pointers of whatever else in the process still holds it, and in the device-manager the
// detector and the allocator's container-share driver initialize this same process-wide library
// independently, neither knowing of the other.
//
// TODO: expose Shutdown once the wrapper counts the library's holders (see the TODO on
// w_dcmi_shutdown in dcmi_wrapper.c). The method then mirrors the other bindings:
// return Return(dcmiShutdown()).

// GetDriverVersion retrieves the version of the DCMI driver.
func (l *DCMI) GetDriverVersion() (string, Return) {
	version := make([]byte, MAX_VER_LEN)
	ret := Return(dcmiGetDriverVersion(&version[0], uint32(len(version))))
	return string(version[:clen(version)]), ret
}

// GetMultiDiePolicy retrieves the policy deciding whether the dies of a multi-die device
// are injected into a container together or one at a time. The policy belongs to the
// driver as a whole rather than to any one card, which is why it hangs off the library.
// A driver that does not implement the query returns ERROR_FUNCTION_NOT_FOUND.
func (l *DCMI) GetMultiDiePolicy() (MultiDiePolicy, Return) {
	var policy MultiDiePolicy
	ret := Return(dcmiGetMultiDiePolicy(&policy))
	return policy, ret
}

// SetMultiDiePolicy sets the multi-die container-injection policy. It applies to every
// multi-die device the driver manages, so it is a node-wide change and not a per-workload
// one.
func (l *DCMI) SetMultiDiePolicy(policy MultiDiePolicy) Return {
	return Return(dcmiSetMultiDiePolicy(policy))
}

type Return int

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
// device, ERROR_NOT_SUPPORT above all.
func (r Return) IsAPIUnavailable() bool {
	switch r {
	case ERROR_LIBRARY_NOT_FOUND, ERROR_FUNCTION_NOT_FOUND:
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
