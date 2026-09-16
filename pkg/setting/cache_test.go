package setting

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInvalidateCacheIsCalledOnlyFromTests holds InvalidateCache to what its comment says it is for.
//
// A COMMENT DOES NOT STOP A CALL. The function exists so a test can cover both sides of a setting,
// and a production path calling it would silently turn the cache off for the whole process -- every
// setting read then goes to the API server, on every reconcile, with nothing reporting the change.
// That is the kind of regression a reviewer reads straight past, because the call site looks like
// ordinary cache hygiene.
func TestInvalidateCacheIsCalledOnlyFromTests(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// ".claude" holds agent worktrees that embed full copies of this repository
			// (gitignored, so never part of the build); scanning them flags the copies.
			switch d.Name() {
			case ".git", ".claude", "staging", "vendor", ".sbin":
				return fs.SkipDir
			}

			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The declaration itself lives in this package and is not a call.
		text := strings.ReplaceAll(string(body), "func InvalidateCache()", "")
		if strings.Contains(text, "InvalidateCache(") {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
		}

		return nil
	})
	require.NoError(t, err)

	// The one sanctioned non-test caller: pkg/setting/settingtest is a test-support package,
	// imported only from _test.go files, so its calls run in test binaries only. Naming the file
	// here keeps the exemption reviewable; anything else stays an offender.
	offenders = slices.DeleteFunc(offenders, func(path string) bool {
		return path == filepath.Join("pkg", "setting", "settingtest", "settingtest.go")
	})

	assert.Empty(t, offenders,
		"InvalidateCache exists for tests that cover both sides of a setting; a production caller "+
			"turns the settings cache off for the whole process")
}
