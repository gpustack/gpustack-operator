package binding

import (
	"runtime"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// systemLibraryName names a library the running platform can open. One name, resolved here rather
// than left to GetLibFromPaths, whose paths[0] fallback would otherwise hand dlopen a name from the
// wrong platform.
func systemLibraryName() string {
	if runtime.GOOS == "darwin" {
		return "libSystem.B.dylib"
	}
	return "libc.so.6"
}

// TestLibraryLookupMissIsNotAnError pins how a symbol probe that misses is reported. Every caller
// asks whether the loaded library offers a symbol before using it, so a miss is an answer about the
// driver — take the older symbol — and reporting it as a failure made a supported driver print an
// error line for a working configuration.
func TestLibraryLookupMissIsNotAnError(t *testing.T) {
	var errorRecords int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, `"error"=`) {
			errorRecords++
		}
	}, funcr.Options{Verbosity: 10})

	// One name per platform, not a list: GetLibFromPaths falls back to paths[0] when nothing
	// resolves, and Linux resolution runs ldconfig — so a two-name list would try to dlopen the
	// darwin name on a Linux box without ldconfig and fail for a reason this case is not about.
	lib := NewLibrary([]string{systemLibraryName()}, WithLogger(logger))
	require.NoError(t, lib.Load())
	t.Cleanup(func() { _ = lib.Unload() })

	require.Error(t, lib.Lookup("gpustackAbsentSymbol"), "an absent symbol is an error to its caller")
	assert.Zero(t, errorRecords, "a probe that misses is not a failure to record")
}
