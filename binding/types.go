package binding

import (
	"errors"
	"fmt"
	"sync"

	"github.com/go-logr/logr"

	"gpustack.ai/gpustack/binding/dl"
)

import "C"

// ErrLibraryNotLoaded is returned when an operation is attempted on a library that has not been loaded.
var ErrLibraryNotLoaded = errors.New("library not loaded")

type (
	// DynamicLibrary is an interface for abstracting the underlying library.
	DynamicLibrary interface {
		// Lookup checks whether the specified library symbol exists in the library.
		Lookup(string) error
	}

	// LibraryCallbackFunc is a function type that can be used as a callback after loading the library.
	LibraryCallbackFunc = func(DynamicLibrary) error

	// libraryOptions hold the parameters than can be set by a LibraryOption.
	libraryOptions struct {
		flags           int
		logger          logr.Logger
		loadCallbacks   []LibraryCallbackFunc
		unloadCallbacks []LibraryCallbackFunc
	}

	// LibraryOption represents a functional option to configure the underlying library.
	LibraryOption func(*libraryOptions)
)

// WithLibraryLoadFlags provides an option to set the flags to be used when loading the library.
func WithLibraryLoadFlags(flags int) LibraryOption {
	return func(o *libraryOptions) {
		o.flags = flags
	}
}

// WithLogger provides an option to set a logger for the library.
func WithLogger(logger logr.Logger) LibraryOption {
	return func(o *libraryOptions) {
		o.logger = logger
	}
}

// WithLibraryLoadCallback provides an option to set a callback function that will be called after the library is loaded.
func WithLibraryLoadCallback(loadCallback LibraryCallbackFunc) LibraryOption {
	return func(o *libraryOptions) {
		if loadCallback != nil {
			o.loadCallbacks = append(o.loadCallbacks, loadCallback)
		}
	}
}

// WithLibraryUnloadCallback provides an option to set a callback function that will be called after the library is unloaded.
func WithLibraryUnloadCallback(unloadCallback LibraryCallbackFunc) LibraryOption {
	return func(o *libraryOptions) {
		if unloadCallback != nil {
			o.unloadCallbacks = append(o.unloadCallbacks, unloadCallback)
		}
	}
}

type (
	Library interface {
		// Load initializes the library,
		// multiple calls to an already loaded library will return without error.
		Load() error
		// Unload unloads the library,
		// multiple calls to an already unloaded library will return without error.
		Unload() error
		// Lookup checks whether the specified library symbol exists in the library.
		// Note that this requires that call Load.
		Lookup(string) error
		// Path returns the path of the library.
		Path() string
	}
	library struct {
		sync.Mutex
		lg                logr.Logger
		rc                int
		dl                *dl.DynamicLibrary
		dlPath            string
		dlLoadCallbacks   []LibraryCallbackFunc
		dlUnloadCallbacks []LibraryCallbackFunc
	}
)

// NewLibrary creates a new library instance with the specified options.
func NewLibrary(paths []string, opts ...LibraryOption) Library {
	p := GetLibFromPaths(paths)

	o := &libraryOptions{
		flags:  dl.RTLD_LAZY | dl.RTLD_GLOBAL,
		logger: logr.Discard(),
	}
	for i := range opts {
		opts[i](o)
	}

	return &library{
		lg:                o.logger,
		dl:                dl.New(p, o.flags),
		dlPath:            p,
		dlLoadCallbacks:   o.loadCallbacks,
		dlUnloadCallbacks: o.unloadCallbacks,
	}
}

func (l *library) Load() (err error) {
	l.Lock()
	defer l.Unlock()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic loading library %s: %v", l.dlPath, r)
		}
		if err == nil {
			l.rc++
		}
	}()
	if l.rc > 0 {
		l.lg.Info("library already loaded, skipping", "path", l.dlPath)
		return nil
	}

	if err = l.dl.Open(); err != nil {
		l.lg.Error(err, "error loading library", "path", l.dlPath)
		return fmt.Errorf("error loading %s: %w", l.dlPath, err)
	}

	if len(l.dlLoadCallbacks) != 0 {
		l.lg.Info("library loaded, running load callback", "path", l.dlPath)
		for i := range l.dlLoadCallbacks {
			if err = l.dlLoadCallbacks[i](l.dl); err != nil {
				l.lg.Error(err, "error in load callback, unloading library", "path", l.dlPath)
				_ = l.dl.Close()
				return fmt.Errorf("error in load callback for %s: %w", l.dlPath, err)
			}
		}
	}

	l.lg.Info("library loaded", "path", l.dlPath)

	return nil
}

func (l *library) Unload() (err error) {
	l.Lock()
	defer l.Unlock()

	if l.rc == 0 {
		return ErrLibraryNotLoaded
	}
	defer func() {
		if err == nil {
			l.rc--
		}
	}()

	if l.rc != 1 {
		l.lg.Info("library still in use, skipping unload", "path", l.dlPath, "refCount", l.rc-1)
		return nil
	}

	if err = l.dl.Close(); err != nil {
		l.lg.Error(err, "error unloading library", "path", l.dlPath)
		return fmt.Errorf("error unloading %s: %w", l.dlPath, err)
	}

	if len(l.dlUnloadCallbacks) != 0 {
		l.lg.Info("library unloaded, running unload callback", "path", l.dlPath)
		for i := range l.dlUnloadCallbacks {
			if err = l.dlUnloadCallbacks[i](l.dl); err != nil {
				l.lg.Error(err, "error in unload callback", "path", l.dlPath)
				return fmt.Errorf("error in unload callback for %s: %w", l.dlPath, err)
			}
		}
	}

	l.lg.Info("library unloaded", "path", l.dlPath)

	return nil
}

func (l *library) Lookup(symbol string) error {
	if l.rc == 0 {
		return fmt.Errorf("error looking symbol %s: %w", symbol, ErrLibraryNotLoaded)
	}

	if err := l.dl.Lookup(symbol); err != nil {
		// A miss is how a caller asks whether the loaded library offers a symbol at all, and every
		// one of them treats the answer as a fact about the driver rather than as a failure — a
		// symbol a newer driver added is absent from an older one, and the caller falls back. The
		// returned error carries the detail for whichever caller decides a miss is fatal.
		l.lg.Info("symbol not found", "symbol", symbol, "path", l.dlPath)
		return fmt.Errorf("error looking up symbol %s in %s: %w", symbol, l.dlPath, err)
	}

	return nil
}

func (l *library) Path() string {
	return l.dlPath
}
