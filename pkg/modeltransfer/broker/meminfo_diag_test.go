package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemInfoFieldBytesDistinguishesTheCause pins the shared parser's
// diagnostics: an unreadable file, an absent field, a malformed line, and a
// out-of-range figure are each named by an explicit error, while a good
// figure answers in bytes with no error. Every failure keeps the caller's
// fail-closed zero; the point is that the cause is no longer silently
// conflated with genuine zero headroom.
func TestMemInfoFieldBytesDistinguishesTheCause(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	good := write("good", "MemTotal:       16088568 kB\nMemAvailable:    8000000 kB\n")
	cases := []struct {
		name        string
		path        string
		field       string
		wantBytes   int64
		wantErrPart string
	}{
		{name: "a good figure answers in bytes", path: good, field: "MemAvailable:", wantBytes: 8000000 * 1024},
		{name: "an unreadable file names the path", path: filepath.Join(dir, "absent"), field: "MemAvailable:", wantErrPart: dir},
		{name: "an absent field names the field", path: good, field: "MemFree:", wantErrPart: "MemFree:"},
		{name: "a malformed line names the field", path: write("malformed", "MemAvailable:    not-a-number\n"), field: "MemAvailable:", wantErrPart: "malformed"},
		{name: "a wrong unit is malformed", path: write("wrong-unit", "MemAvailable:    8000000 MB\n"), field: "MemAvailable:", wantErrPart: "malformed"},
		{name: "zero headroom is a valid figure", path: write("valid-zero", "MemAvailable:    0 kB\n"), field: "MemAvailable:"},
		{name: "byte overflow is refused", path: write("overflow", "MemAvailable:    9223372036854775807 kB\n"), field: "MemAvailable:", wantErrPart: "byte range"},
		{name: "a negative figure is refused", path: write("negative", "MemAvailable:    -1 kB\n"), field: "MemAvailable:", wantErrPart: "byte range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bytes, err := memInfoFieldBytes(tc.path, tc.field)
			if tc.wantErrPart == "" {
				require.Nil(t, err)
				assert.Equal(t, tc.wantBytes, bytes)
				return
			}
			assert.Zero(t, bytes, "every failure stays fail-closed at zero")
			require.NotNil(t, err)
			assert.True(t, strings.Contains(err.Error(), tc.wantErrPart),
				"the error names the cause: %q", err.Error())
		})
	}
}

// TestMemInfoTotalBytesKeepsItsFailClosedAnswer pins the exported total
// figure's unchanged contract for its existing callers: zero on any
// failure, no error surface, bytes on success.
func TestMemInfoTotalBytesKeepsItsFailClosedAnswer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	require.NoError(t, os.WriteFile(path, []byte("MemTotal:       16088568 kB\n"), 0o600))
	assert.Equal(t, int64(16088568*1024), MemInfoTotalBytes(path))
	assert.Zero(t, MemInfoTotalBytes(filepath.Join(dir, "absent")))
	assert.Zero(t, MemInfoTotalBytes(writeBrokenMemInfo(t, dir, "MemTotal:    broken\n")))
}

func writeBrokenMemInfo(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "broken")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
