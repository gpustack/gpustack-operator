package dl

// #cgo LDFLAGS: -ldl
// #define _GNU_SOURCE
// #include <dlfcn.h>
// #include <stdlib.h>
// #include <linux/limits.h>
import "C"

import (
	"fmt"
	"path"
	"unsafe"
)

const (
	RTLD_DEEPBIND = C.RTLD_DEEPBIND
)

// Path returns the path to the loaded library.
// See https://man7.org/linux/man-pages/man3/dlinfo.3.html
func (dl *DynamicLibrary) Path() (string, error) {
	if dl.handle == nil {
		return "", fmt.Errorf("%v not opened", dl.name)
	}

	libParentPathBuffer := C.CBytes(make([]byte, 0, C.PATH_MAX))
	defer C.free(unsafe.Pointer(libParentPathBuffer)) // nolint:unconvert

	var libPath string
	if err := withOSLock(func() error {
		if dl.path != "" {
			libPath = dl.path
			return nil
		}
		// Call dlError() to clear out any previous errors.
		_ = dlError()
		ret := C.dlinfo(dl.handle, C.RTLD_DI_ORIGIN, libParentPathBuffer)
		if ret == -1 {
			return fmt.Errorf("dlinfo call failed: %w", dlError())
		}

		libPath = path.Join(C.GoString((*C.char)(libParentPathBuffer)), dl.name)
		dl.path = libPath

		return nil
	}); err != nil {
		return "", err
	}
	return libPath, nil
}
