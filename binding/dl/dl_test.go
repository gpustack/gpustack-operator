package dl

import (
	"runtime"
	"testing"
)

func skipOnMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("libdl.so is not available on macOS")
	}
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
