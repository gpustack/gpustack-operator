package dl

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
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
	name  string
	flags int

	// mu guards the three fields below, which a symbol lookup reads while another goroutine may
	// be opening or closing the same library.
	//
	// The lock has to live here rather than one level up: the wrapper in the parent package
	// serializes loading and unloading, but deliberately not looking up, because every vendor
	// wrapper begins with a lookup on its own hot path. That left the symbol cache a plain map
	// read on every call and written the first time each symbol is resolved — and a map race is
	// a runtime throw rather than a panic, so no recover in a gRPC handler can contain it. One
	// concurrent lookup would take down the whole device manager, and with it every
	// manufacturer's allocations on that node.
	mu     sync.RWMutex
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

// init sets the guarded fields to their unopened state. It does NOT take the lock itself, because
// its two callers already stand on either side of one: New builds a value nothing else can see yet,
// and reset runs inside Close, which holds the write lock across the whole close.
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

	dl.mu.Lock()
	defer dl.mu.Unlock()

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
	dl.mu.Lock()
	defer dl.mu.Unlock()

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

// Lookup reports whether the library exports the symbol, remembering the ones it has already
// resolved so a repeated lookup costs no dlsym call. It is safe to call concurrently, which the
// wrapper above relies on: it is the one entry point that is not serialized there, and several
// callers reach one library handle at once.
//
// The read of the handle and of the cache is taken in one critical section, so a caller cannot
// observe a handle from before a close together with a cache from after it. The dlsym call itself
// is left outside the lock — it is the expensive part, it is what the whole cache exists to avoid
// repeating, and holding a lock across it would serialize every vendor call in the process.
func (dl *DynamicLibrary) Lookup(symbol string) error {
	dl.mu.RLock()
	handle, cached := dl.handle, false
	if handle != nil {
		_, cached = dl.caches[symbol]
	}
	dl.mu.RUnlock()

	if handle == nil {
		return fmt.Errorf("error looking up %q: %w", symbol, errors.New("library not loaded"))
	}
	if cached {
		return nil
	}

	sym := C.CString(symbol)
	defer C.free(unsafe.Pointer(sym))

	var pointer unsafe.Pointer
	if err := withOSLock(func() error {
		// Call dlError() to clear out any previous errors.
		_ = dlError()
		pointer = C.dlsym(handle, sym)
		if pointer == nil {
			return fmt.Errorf("symbol %q not found: %w", symbol, dlError())
		}
		return nil
	}); err != nil {
		return err
	}

	// Record the symbol only against the handle it was actually resolved on. A close that landed
	// while dlsym ran has already replaced the cache with an empty one, and remembering a symbol
	// in it would tell the next lookup — on a handle from a later open — that a symbol is present
	// without ever asking the library.
	dl.mu.Lock()
	if dl.handle == handle {
		dl.caches[symbol] = struct{}{}
	}
	dl.mu.Unlock()

	return nil
}
