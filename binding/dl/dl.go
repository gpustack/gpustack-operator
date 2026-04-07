package dl

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"
)

// #cgo LDFLAGS: -ldl
// #include <dlfcn.h>
// #include <stdlib.h>
import "C"

const (
	RTLD_LAZY     = C.RTLD_LAZY
	RTLD_NOW      = C.RTLD_NOW
	RTLD_GLOBAL   = C.RTLD_GLOBAL
	RTLD_LOCAL    = C.RTLD_LOCAL
	RTLD_NODELETE = C.RTLD_NODELETE
	RTLD_NOLOAD   = C.RTLD_NOLOAD
)

type DynamicLibrary struct {
	name   string
	flags  int
	handle unsafe.Pointer
	path   string
	caches map[string]struct{}
}

func New(name string, flags int) *DynamicLibrary {
	return (&DynamicLibrary{
		name:  name,
		flags: flags,
	}).init()
}

func (dl *DynamicLibrary) reset() {
	_ = dl.init()
}

func (dl *DynamicLibrary) init() *DynamicLibrary {
	dl.handle = nil
	dl.path = func() string {
		if strings.Contains(dl.name, "/") {
			return dl.name
		}
		return ""
	}()
	dl.caches = make(map[string]struct{})
	return dl
}

func withOSLock(action func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	return action()
}

func dlError() error {
	lastErr := C.dlerror()
	if lastErr == nil {
		return nil
	}
	return errors.New(C.GoString(lastErr))
}

func (dl *DynamicLibrary) Open() error {
	name := C.CString(dl.name)
	defer C.free(unsafe.Pointer(name))

	if err := withOSLock(func() error {
		handle := C.dlopen(name, C.int(dl.flags))
		if handle == nil {
			return dlError()
		}
		dl.handle = handle
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (dl *DynamicLibrary) Close() error {
	if dl.handle == nil {
		return nil
	}
	if err := withOSLock(func() error {
		if C.dlclose(dl.handle) != 0 {
			return dlError()
		}
		dl.reset()
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (dl *DynamicLibrary) Lookup(symbol string) error {
	if dl.handle == nil {
		return fmt.Errorf("error looking up %q: %w", symbol, errors.New("library not loaded"))
	}

	if _, ok := dl.caches[symbol]; ok {
		return nil
	}

	sym := C.CString(symbol)
	defer C.free(unsafe.Pointer(sym))

	var pointer unsafe.Pointer
	if err := withOSLock(func() error {
		// Call dlError() to clear out any previous errors.
		_ = dlError()
		pointer = C.dlsym(dl.handle, sym)
		if pointer == nil {
			return fmt.Errorf("symbol %q not found: %w", symbol, dlError())
		}
		return nil
	}); err != nil {
		return err
	}

	dl.caches[symbol] = struct{}{}

	return nil
}
