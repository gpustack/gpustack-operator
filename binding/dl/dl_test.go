package dl

import (
	"runtime"
	"sync"
	"testing"
)

func skipOnMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("libdl.so is not available on macOS")
	}
}

// systemLibrary names a library the running platform can open, and several symbols it exports. The
// cases above pin a Linux-only library and skip everywhere else; a case about concurrency has to run
// on the machine a developer actually runs the suite on, or the race it guards against is never
// exercised at all.
func systemLibrary() (string, []string) {
	symbols := []string{"malloc", "free", "getpid", "strlen", "memcpy", "abort"}
	if runtime.GOOS == "darwin" {
		return "libSystem.B.dylib", symbols
	}
	return "libc.so.6", symbols
}

func TestNew(t *testing.T) {
	t.Parallel()
	dl := New("libc.so", RTLD_LAZY|RTLD_GLOBAL)

	if dl == nil {
		t.Errorf("Error in New: should not return '%v'", dl)
	}
}

func TestOpenSuccess(t *testing.T) {
	skipOnMacOS(t)

	t.Parallel()
	dl := New("libdl.so.2", RTLD_LAZY|RTLD_GLOBAL)

	err := dl.Open()
	defer dl.Close()

	if err != nil {
		t.Errorf("Error opening shared lib: %v", err)
	}
}

func TestOpenFailed(t *testing.T) {
	t.Parallel()
	dl := New("libbogusbadname.so", RTLD_LAZY|RTLD_GLOBAL)

	err := dl.Open()
	if err == nil {
		t.Errorf("Should have errored opening shared lib but did not")
	}
}

func TestOpenTwice(t *testing.T) {
	skipOnMacOS(t)

	t.Parallel()
	dl1 := New("libdl.so.2", RTLD_LAZY|RTLD_GLOBAL)
	dl2 := New("libdl.so.2", RTLD_LAZY|RTLD_GLOBAL)

	err := dl1.Open()
	if err != nil {
		t.Fatalf("First dlopen finished with error: %v", err)
	}

	err = dl2.Open()
	if err != nil {
		t.Fatalf("Second dlopen finished with error: %v", err)
	}

	if dl1.handle != dl2.handle {
		t.Fatal("Two handles must be same")
	}

	err = dl1.Close()
	if err != nil {
		t.Fatalf("First dlclose finished with error: %v", err)
	}

	err = dl2.Close()
	if err != nil {
		t.Fatalf("Second dlclose finished with error: %v", err)
	}
}

func TestClose(t *testing.T) {
	skipOnMacOS(t)

	t.Parallel()
	dl := New("libdl.so.2", RTLD_LAZY|RTLD_GLOBAL)

	_ = dl.Open()
	err := dl.Close()
	if err != nil {
		t.Errorf("Error closing shared lib: %v", err)
	}
}

func TestLookupSuccess(t *testing.T) {
	skipOnMacOS(t)

	t.Parallel()
	dl := New("libdl.so.2", RTLD_LAZY|RTLD_GLOBAL)

	_ = dl.Open()
	defer dl.Close()

	err := dl.Lookup("dlsym")
	if err != nil {
		t.Errorf("Error looking up symbol: %v", err)
	}
}

func TestLookupFailed(t *testing.T) {
	skipOnMacOS(t)

	t.Parallel()
	dl := New("libdl.so.2", RTLD_LAZY|RTLD_GLOBAL)

	_ = dl.Open()
	defer dl.Close()

	err := dl.Lookup("bogus")
	if err == nil {
		t.Errorf("Should have errored loking up symbol but did not")
	}
}

// An absent symbol is remembered exactly like a present one. The callers that matter probe for a
// symbol their driver may be too old to export, and they do it on every call: without this, each of
// those calls paid a dlsym and produced a fresh error. A library's exports cannot change while it is
// open, so the remembered answer keeps holding.
func TestLookupFailedCached(t *testing.T) {
	t.Parallel()

	const absent = "gpustackAbsentSymbol"

	name, _ := systemLibrary()
	dl := New(name, RTLD_LAZY|RTLD_GLOBAL)
	if err := dl.Open(); err != nil {
		t.Fatalf("Error opening %s: %v", name, err)
	}
	t.Cleanup(func() { _ = dl.Close() })

	first := dl.Lookup(absent)
	if first == nil {
		t.Fatal("Should have errored looking up an absent symbol but did not")
	}

	dl.mu.RLock()
	_, cached := dl.caches[absent]
	dl.mu.RUnlock()
	if !cached {
		t.Fatal("An absent symbol must be remembered, so it is resolved only once")
	}

	second := dl.Lookup(absent)
	if second == nil || second.Error() != first.Error() {
		t.Errorf("A remembered absent symbol must answer as before, got '%v'", second)
	}
}

// One library handle is looked up from several goroutines at once, because that is how it is used:
// the wrapper one package up serializes loading and unloading but deliberately not looking up, and a
// device manager hands a single handle to more than one server. Every goroutine walks every symbol,
// so each symbol's first resolution — the only time the cache is written — happens while the others
// are reading that same map.
//
// This case is meaningful under -race, where an unguarded cache reports a data race. Without the
// race detector the same interleaving is a runtime throw ("concurrent map read and map write") that
// no recover can contain, which is why it is worth a case of its own: in production it does not fail
// the one call, it takes the process down.
func TestLookupConcurrent(t *testing.T) {
	t.Parallel()

	name, symbols := systemLibrary()
	dl := New(name, RTLD_LAZY|RTLD_GLOBAL)
	if err := dl.Open(); err != nil {
		t.Fatalf("Error opening %s: %v", name, err)
	}
	t.Cleanup(func() { _ = dl.Close() })

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Repeat the walk so the cached read path is driven too, not only the first write.
			for range 4 {
				for _, symbol := range symbols {
					if err := dl.Lookup(symbol); err != nil {
						t.Errorf("Error looking up %s in %s: %v", symbol, name, err)
					}
				}
			}
		}()
	}
	wg.Wait()
}
