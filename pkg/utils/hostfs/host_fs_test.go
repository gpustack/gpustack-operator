package hostfs

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestHostRoot creates a valid simulated host root directory containing
// the required etc, proc, and usr directory markers.
func newTestHostRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"etc", "proc", "usr"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
	}
	return root
}

func TestDefaultHostRoot(t *testing.T) {
	assert.Equal(t, "/host", DefaultHostRoot)
}

func TestExportedReadOnlyMethodSurface(t *testing.T) {
	typ := reflect.TypeFor[*ReadOnlyHostFS]()
	got := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	slices.Sort(got)

	want := []string{
		"Close",
		"Exists",
		"Lstat",
		"Open",
		"ReadDir",
		"ReadFile",
		"ReadRegularFile",
		"Readlink",
		"Stat",
	}
	assert.Equal(t, want, got)
}

func TestOpenHostRootValidation(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr bool
	}{
		{
			name: "empty root path is rejected",
			setup: func(t *testing.T) string {
				return ""
			},
			wantErr: true,
		},
		{
			name: "non-existent root path is rejected",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "missing")
			},
			wantErr: true,
		},
		{
			name: "regular file as root path is rejected",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "file-root")
				require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
				return p
			},
			wantErr: true,
		},
		{
			name: "empty directory without markers is rejected",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			wantErr: true,
		},
		{
			name: "missing etc marker is rejected",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(root, "proc"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "usr"), 0o755))
				return root
			},
			wantErr: true,
		},
		{
			name: "missing proc marker is rejected",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(root, "etc"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "usr"), 0o755))
				return root
			},
			wantErr: true,
		},
		{
			name: "missing usr marker is rejected",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(root, "etc"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "proc"), 0o755))
				return root
			},
			wantErr: true,
		},
		{
			name: "regular file in place of etc marker is rejected",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(root, "etc"), []byte("file"), 0o644))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "proc"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "usr"), 0o755))
				return root
			},
			wantErr: true,
		},
		{
			name: "regular file in place of proc marker is rejected",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(root, "etc"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "proc"), []byte("file"), 0o644))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "usr"), 0o755))
				return root
			},
			wantErr: true,
		},
		{
			name: "regular file in place of usr marker is rejected",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(root, "etc"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "proc"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "usr"), []byte("file"), 0o644))
				return root
			},
			wantErr: true,
		},
		{
			name: "marker absolute symlink outside root is rejected",
			setup: func(t *testing.T) string {
				outside := t.TempDir()
				root := t.TempDir()
				require.NoError(t, os.Symlink(outside, filepath.Join(root, "etc")))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "proc"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "usr"), 0o755))
				return root
			},
			wantErr: true,
		},
		{
			name: "marker relative symlink escaping root is rejected",
			setup: func(t *testing.T) string {
				parent := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(parent, "outside_etc"), 0o755))
				root := filepath.Join(parent, "root")
				require.NoError(t, os.MkdirAll(root, 0o755))
				require.NoError(t, os.Symlink("../outside_etc", filepath.Join(root, "etc")))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "proc"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(root, "usr"), 0o755))
				return root
			},
			wantErr: true,
		},
		{
			name:    "valid host root with etc proc and usr directories succeeds",
			setup:   newTestHostRoot,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := OpenHostRoot(tc.setup(t))
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, h)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, h)
			require.NoError(t, h.Close())
		})
	}
}

func TestValidReadOperations(t *testing.T) {
	root := newTestHostRoot(t)
	wantJSON := []byte(`{"version":"2.0"}`)
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), wantJSON, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "empty.conf"), []byte{}, 0o644))

	h, err := OpenHostRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	t.Run("Stat and Lstat on root and regular file", func(t *testing.T) {
		for _, rootPath := range []string{"/", "///"} {
			info, err := h.Stat(rootPath)
			require.NoError(t, err)
			assert.True(t, info.IsDir())

			linfo, err := h.Lstat(rootPath)
			require.NoError(t, err)
			assert.True(t, linfo.IsDir())
		}

		info, err := h.Stat("/etc/hccl_rootinfo.json")
		require.NoError(t, err)
		assert.True(t, info.Mode().IsRegular())
		assert.Equal(t, int64(len(wantJSON)), info.Size())

		linfo, err := h.Lstat("/etc/hccl_rootinfo.json")
		require.NoError(t, err)
		assert.True(t, linfo.Mode().IsRegular())

		_, err = h.Lstat("/etc/missing.json")
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("Open reads file content and enforces O_RDONLY", func(t *testing.T) {
		f, err := h.Open("/etc/hccl_rootinfo.json")
		require.NoError(t, err)
		defer func() { _ = f.Close() }()

		got, err := io.ReadAll(f)
		require.NoError(t, err)
		assert.Equal(t, wantJSON, got)

		_, writeErr := f.Write([]byte("mutate"))
		require.Error(t, writeErr)

		rootFile, err := h.Open("/")
		require.NoError(t, err)
		defer func() { _ = rootFile.Close() }()
		info, err := rootFile.Stat()
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("ReadFile and ReadRegularFile", func(t *testing.T) {
		got, err := h.ReadFile("/etc/hccl_rootinfo.json")
		require.NoError(t, err)
		assert.Equal(t, wantJSON, got)

		gotReg, err := h.ReadRegularFile("/etc/hccl_rootinfo.json", int64(len(wantJSON)))
		require.NoError(t, err)
		assert.Equal(t, wantJSON, gotReg)

		gotMax, err := h.ReadRegularFile("/etc/hccl_rootinfo.json", math.MaxInt64)
		require.NoError(t, err)
		assert.Equal(t, wantJSON, gotMax)

		gotEmpty, err := h.ReadRegularFile("/etc/empty.conf", 16)
		require.NoError(t, err)
		assert.Empty(t, gotEmpty)

		gotNoLeadingSlash, err := h.ReadRegularFile("etc/hccl_rootinfo.json", 64)
		require.NoError(t, err)
		assert.Equal(t, wantJSON, gotNoLeadingSlash)

		_, err = h.ReadFile("/etc")
		require.Error(t, err)
	})

	t.Run("unreadable file surfaces permission error on Open and ReadRegularFile", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file read permission bits")
		}
		permRoot := newTestHostRoot(t)
		unreadable := filepath.Join(permRoot, "etc", "unreadable.conf")
		require.NoError(t, os.WriteFile(unreadable, []byte("secret"), 0o000))
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })

		permFS, err := OpenHostRoot(permRoot)
		require.NoError(t, err)
		t.Cleanup(func() { _ = permFS.Close() })

		_, err = permFS.Open("/etc/unreadable.conf")
		require.Error(t, err)

		_, err = permFS.ReadRegularFile("/etc/unreadable.conf", 64)
		require.Error(t, err)
	})

	t.Run("ReadDir returns sorted entries on root and subdirectory", func(t *testing.T) {
		rootEntries, err := h.ReadDir("/")
		require.NoError(t, err)
		names := make([]string, 0, len(rootEntries))
		for _, e := range rootEntries {
			names = append(names, e.Name())
		}
		assert.Equal(t, []string{"etc", "proc", "usr"}, names)

		etcEntries, err := h.ReadDir("/etc")
		require.NoError(t, err)
		etcNames := make([]string, 0, len(etcEntries))
		for _, e := range etcEntries {
			etcNames = append(etcNames, e.Name())
		}
		assert.Equal(t, []string{"empty.conf", "hccl_rootinfo.json"}, etcNames)

		nonDirEntries, err := h.ReadDir("/etc/hccl_rootinfo.json")
		require.Error(t, err)
		assert.Nil(t, nonDirEntries)

		_, err = h.ReadDir("/missing-dir")
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("Exists distinguishes present absent and invalid paths", func(t *testing.T) {
		ok, err := h.Exists("/")
		require.NoError(t, err)
		assert.True(t, ok)

		ok, err = h.Exists("/etc/hccl_rootinfo.json")
		require.NoError(t, err)
		assert.True(t, ok)

		ok, err = h.Exists("/etc/absent.json")
		require.NoError(t, err)
		assert.False(t, ok)

		ok, err = h.Exists("/absent-dir/file.json")
		require.NoError(t, err)
		assert.False(t, ok)

		ok, err = h.Exists("../etc/hccl_rootinfo.json")
		require.Error(t, err)
		assert.False(t, ok)
	})
}

func TestPathTraversalAndInvalidPathsRejected(t *testing.T) {
	root := newTestHostRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), []byte("ok"), 0o644))

	h, err := OpenHostRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	invalidPaths := []struct {
		name string
		path string
	}{
		{name: "empty path", path: ""},
		{name: "bare dotdot", path: ".."},
		{name: "root dotdot", path: "/.."},
		{name: "leading relative dotdot", path: "../etc/hccl_rootinfo.json"},
		{name: "leading slash dotdot", path: "/../etc/hccl_rootinfo.json"},
		{name: "internal dotdot never folded", path: "/etc/../etc/hccl_rootinfo.json"},
		{name: "trailing dotdot", path: "/etc/hccl_rootinfo.json/.."},
		{name: "nul byte in path", path: "/etc/\x00hccl_rootinfo.json"},
	}

	for _, tc := range invalidPaths {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Stat(tc.path)
			require.ErrorIs(t, err, fs.ErrInvalid)

			_, err = h.Lstat(tc.path)
			require.ErrorIs(t, err, fs.ErrInvalid)

			_, err = h.Open(tc.path)
			require.ErrorIs(t, err, fs.ErrInvalid)

			_, err = h.ReadFile(tc.path)
			require.ErrorIs(t, err, fs.ErrInvalid)

			_, err = h.ReadRegularFile(tc.path, 64)
			require.ErrorIs(t, err, fs.ErrInvalid)

			_, err = h.ReadDir(tc.path)
			require.ErrorIs(t, err, fs.ErrInvalid)

			_, err = h.Readlink(tc.path)
			require.ErrorIs(t, err, fs.ErrInvalid)

			ok, err := h.Exists(tc.path)
			require.ErrorIs(t, err, fs.ErrInvalid)
			assert.False(t, ok)
		})
	}

	t.Run("non-directory intermediate component is rejected", func(t *testing.T) {
		_, err := h.Stat("/etc/hccl_rootinfo.json/child")
		require.Error(t, err)
		assert.False(t, errors.Is(err, fs.ErrNotExist))

		ok, err := h.Exists("/etc/hccl_rootinfo.json/child")
		require.Error(t, err)
		assert.False(t, ok)
	})
}

func TestLeafSymlinkHandling(t *testing.T) {
	root := newTestHostRoot(t)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside-secret"), 0o644))

	wantPayload := []byte("target-payload")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "usr", "share"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "usr", "share", "target.conf"), wantPayload, 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc", "sub"), 0o755))

	// Create relative, parent-relative, absolute, root-pointing, dangling, and escaping leaf symlinks.
	require.NoError(t, os.Symlink("../usr/share/target.conf", filepath.Join(root, "etc", "rel_leaf.conf")))
	require.NoError(t, os.Symlink("../../usr/share/target.conf", filepath.Join(root, "etc", "sub", "up_leaf.conf")))
	require.NoError(t, os.Symlink("/usr/share/target.conf", filepath.Join(root, "etc", "abs_leaf.conf")))
	require.NoError(t, os.Symlink("/", filepath.Join(root, "etc", "root_alias")))
	require.NoError(t, os.Symlink("/usr/share/missing.conf", filepath.Join(root, "etc", "dangling.conf")))
	require.NoError(t, os.Symlink("../../../secret.txt", filepath.Join(root, "etc", "escape_rel.conf")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "etc", "escape_abs.conf")))

	h, err := OpenHostRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	inRootLinks := []struct {
		name       string
		path       string
		wantTarget string
	}{
		{
			name:       "relative leaf symlink inside root",
			path:       "/etc/rel_leaf.conf",
			wantTarget: "../usr/share/target.conf",
		},
		{
			name:       "nested relative leaf symlink inside root",
			path:       "/etc/sub/up_leaf.conf",
			wantTarget: "../../usr/share/target.conf",
		},
		{
			name:       "absolute leaf symlink inside root",
			path:       "/etc/abs_leaf.conf",
			wantTarget: "/usr/share/target.conf",
		},
	}

	for _, tc := range inRootLinks {
		t.Run(tc.name, func(t *testing.T) {
			linfo, err := h.Lstat(tc.path)
			require.NoError(t, err)
			assert.NotZero(t, linfo.Mode()&fs.ModeSymlink)
			assert.False(t, linfo.Mode().IsRegular())

			gotTarget, err := h.Readlink(tc.path)
			require.NoError(t, err)
			assert.Equal(t, tc.wantTarget, gotTarget)

			_, err = h.ReadRegularFile(tc.path, 1024)
			require.Error(t, err)

			sinfo, err := h.Stat(tc.path)
			require.NoError(t, err)
			assert.True(t, sinfo.Mode().IsRegular())

			gotData, err := h.ReadFile(tc.path)
			require.NoError(t, err)
			assert.Equal(t, wantPayload, gotData)

			f, err := h.Open(tc.path)
			require.NoError(t, err)
			defer func() { _ = f.Close() }()
			openData, err := io.ReadAll(f)
			require.NoError(t, err)
			assert.Equal(t, wantPayload, openData)

			ok, err := h.Exists(tc.path)
			require.NoError(t, err)
			assert.True(t, ok)
		})
	}

	t.Run("leaf symlink targeting slash resolves to host root", func(t *testing.T) {
		info, err := h.Stat("/etc/root_alias")
		require.NoError(t, err)
		assert.True(t, info.IsDir())

		entries, err := h.ReadDir("/etc/root_alias")
		require.NoError(t, err)
		assert.NotEmpty(t, entries)
	})

	t.Run("dangling leaf symlink reports absent on Exists and Stat", func(t *testing.T) {
		linfo, err := h.Lstat("/etc/dangling.conf")
		require.NoError(t, err)
		assert.NotZero(t, linfo.Mode()&fs.ModeSymlink)

		_, err = h.Stat("/etc/dangling.conf")
		require.ErrorIs(t, err, fs.ErrNotExist)

		ok, err := h.Exists("/etc/dangling.conf")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("escaping leaf symlinks cannot read outside host root", func(t *testing.T) {
		_, err := h.Stat("/etc/escape_rel.conf")
		require.Error(t, err)
		assert.False(t, errors.Is(err, fs.ErrNotExist))

		ok, err := h.Exists("/etc/escape_rel.conf")
		require.Error(t, err)
		assert.False(t, ok)

		_, err = h.ReadFile("/etc/escape_rel.conf")
		require.Error(t, err)

		_, err = h.ReadFile("/etc/escape_abs.conf")
		require.Error(t, err)

		_, err = h.Stat("/etc/escape_abs.conf")
		require.Error(t, err)
	})

	t.Run("Readlink on non-symlink returns error", func(t *testing.T) {
		_, err := h.Readlink("/usr/share/target.conf")
		require.Error(t, err)

		_, err = h.Readlink("/")
		require.Error(t, err)
	})
}

func TestMergedUsrIntermediateSymlinksAndLoopCap(t *testing.T) {
	t.Run("relative merged-usr lib symlink", func(t *testing.T) {
		root := newTestHostRoot(t)
		wantRoute := []byte("route-relative")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "usr", "lib"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "usr", "lib", "route.conf"), wantRoute, 0o644))
		require.NoError(t, os.Symlink("usr/lib", filepath.Join(root, "lib")))

		h, err := OpenHostRoot(root)
		require.NoError(t, err)
		t.Cleanup(func() { _ = h.Close() })

		linfo, err := h.Lstat("/lib/route.conf")
		require.NoError(t, err)
		assert.True(t, linfo.Mode().IsRegular())

		got, err := h.ReadRegularFile("/lib/route.conf", 1024)
		require.NoError(t, err)
		assert.Equal(t, wantRoute, got)

		entries, err := h.ReadDir("/lib")
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, "route.conf", entries[0].Name())
	})

	t.Run("absolute merged-usr lib symlink and chained symlinks", func(t *testing.T) {
		root := newTestHostRoot(t)
		wantRoute := []byte("route-absolute")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "usr", "lib"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "usr", "lib", "route.conf"), wantRoute, 0o644))
		require.NoError(t, os.Symlink("/usr/lib", filepath.Join(root, "lib")))
		require.NoError(t, os.Symlink("/lib", filepath.Join(root, "lib64")))

		h, err := OpenHostRoot(root)
		require.NoError(t, err)
		t.Cleanup(func() { _ = h.Close() })

		for _, p := range []string{"/lib/route.conf", "/lib64/route.conf"} {
			linfo, err := h.Lstat(p)
			require.NoError(t, err)
			assert.True(t, linfo.Mode().IsRegular())

			sinfo, err := h.Stat(p)
			require.NoError(t, err)
			assert.True(t, sinfo.Mode().IsRegular())

			got, err := h.ReadRegularFile(p, 1024)
			require.NoError(t, err)
			assert.Equal(t, wantRoute, got)

			gotRead, err := h.ReadFile(p)
			require.NoError(t, err)
			assert.Equal(t, wantRoute, gotRead)

			ok, err := h.Exists(p)
			require.NoError(t, err)
			assert.True(t, ok)
		}
	})

	t.Run("intermediate symlinks escaping root are rejected", func(t *testing.T) {
		root := newTestHostRoot(t)
		outside := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "real.conf"), []byte("real"), 0o644))

		require.NoError(t, os.Symlink("../../../outside", filepath.Join(root, "escape_rel")))
		require.NoError(t, os.Symlink("/../../outside", filepath.Join(root, "escape_abs_dotdot")))
		require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape_abs_outside")))
		require.NoError(t, os.Symlink("/etc/real.conf/..", filepath.Join(root, "traverse_through_file")))

		h, err := OpenHostRoot(root)
		require.NoError(t, err)
		t.Cleanup(func() { _ = h.Close() })

		for _, badPath := range []string{
			"/escape_rel/secret.txt",
			"/escape_abs_dotdot/secret.txt",
			"/escape_abs_outside/secret.txt",
			"/traverse_through_file/real.conf",
		} {
			_, err := h.Lstat(badPath)
			require.Error(t, err, "Lstat(%q) must fail", badPath)

			_, err = h.Stat(badPath)
			require.Error(t, err, "Stat(%q) must fail", badPath)

			_, err = h.ReadFile(badPath)
			require.Error(t, err, "ReadFile(%q) must fail", badPath)

			_, err = h.ReadRegularFile(badPath, 1024)
			require.Error(t, err, "ReadRegularFile(%q) must fail", badPath)

			_, err = h.Open(badPath)
			require.Error(t, err, "Open(%q) must fail", badPath)
		}

		// Relative and dotdot escapes must surface an error on Exists rather than false, nil.
		for _, escapeDotDot := range []string{"/escape_rel/secret.txt", "/escape_abs_dotdot/secret.txt"} {
			ok, err := h.Exists(escapeDotDot)
			require.Error(t, err)
			assert.False(t, ok)
		}
	})

	t.Run("40-hop symlink boundary and loop detection", func(t *testing.T) {
		root := newTestHostRoot(t)
		want := []byte("hop-ok")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "usr", "chain", "real"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, "usr", "chain", "real", "data.txt"), want, 0o644))

		// Build a 41-step directory symlink chain: dir1 -> /usr/chain/real, dir2 -> /usr/chain/dir1, ..., dir41 -> /usr/chain/dir40.
		prev := "/usr/chain/real"
		for i := 1; i <= 41; i++ {
			linkName := filepath.Join(root, "usr", "chain", directoryHopName(i))
			require.NoError(t, os.Symlink(prev, linkName))
			prev = "/usr/chain/" + directoryHopName(i)
		}

		// Also create a 2-node symlink cycle.
		require.NoError(t, os.Symlink("/usr/chain/cycle_b", filepath.Join(root, "usr", "chain", "cycle_a")))
		require.NoError(t, os.Symlink("/usr/chain/cycle_a", filepath.Join(root, "usr", "chain", "cycle_b")))

		h, err := OpenHostRoot(root)
		require.NoError(t, err)
		t.Cleanup(func() { _ = h.Close() })

		// 40 intermediate directory hops must succeed.
		got, err := h.ReadRegularFile("/usr/chain/"+directoryHopName(40)+"/data.txt", 1024)
		require.NoError(t, err)
		assert.Equal(t, want, got)

		// 41 intermediate directory hops must fail with ELOOP.
		_, err = h.ReadRegularFile("/usr/chain/"+directoryHopName(41)+"/data.txt", 1024)
		require.ErrorIs(t, err, syscall.ELOOP)

		// 40 leaf symlink hops must also succeed on Stat and ReadFile, while 41 fail.
		leafPrev := "real/data.txt"
		for i := 1; i <= 41; i++ {
			leafName := "leaf_" + directoryHopName(i)
			require.NoError(t, os.Symlink(leafPrev, filepath.Join(root, "usr", "chain", leafName)))
			leafPrev = leafName
		}

		gotLeaf, err := h.ReadFile("/usr/chain/leaf_" + directoryHopName(40))
		require.NoError(t, err)
		assert.Equal(t, want, gotLeaf)

		_, err = h.ReadFile("/usr/chain/leaf_" + directoryHopName(41))
		require.ErrorIs(t, err, syscall.ELOOP)

		// Symlink cycle must fail with ELOOP and report error on Exists.
		_, err = h.Stat("/usr/chain/cycle_a/data.txt")
		require.ErrorIs(t, err, syscall.ELOOP)

		ok, err := h.Exists("/usr/chain/cycle_a/data.txt")
		require.ErrorIs(t, err, syscall.ELOOP)
		assert.False(t, ok)
	})
}

func directoryHopName(i int) string {
	return "hop_" + string(rune('a'+(i/26))) + string(rune('a'+(i%26)))
}

func TestReadRegularFileRejectsNonRegularAndOversized(t *testing.T) {
	root := newTestHostRoot(t)
	payload := bytes.Repeat([]byte("a"), 100)
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "sized.bin"), payload, 0o644))

	fifoPath := filepath.Join(root, "etc", "named.fifo")
	require.NoError(t, syscall.Mkfifo(fifoPath, 0o644))

	h, err := OpenHostRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	cases := []struct {
		name     string
		path     string
		maxBytes int64
	}{
		{
			name:     "zero maxBytes is rejected",
			path:     "/etc/sized.bin",
			maxBytes: 0,
		},
		{
			name:     "negative maxBytes is rejected",
			path:     "/etc/sized.bin",
			maxBytes: -1,
		},
		{
			name:     "file exceeding maxBytes is rejected",
			path:     "/etc/sized.bin",
			maxBytes: 99,
		},
		{
			name:     "directory is rejected",
			path:     "/etc",
			maxBytes: 1024,
		},
		{
			name:     "root directory is rejected",
			path:     "/",
			maxBytes: 1024,
		},
		{
			name:     "named pipe FIFO is rejected without blocking",
			path:     "/etc/named.fifo",
			maxBytes: 1024,
		},
		{
			name:     "missing file is rejected",
			path:     "/etc/missing.bin",
			maxBytes: 1024,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.ReadRegularFile(tc.path, tc.maxBytes)
			require.Error(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestReadVerifiedRegularFileTOCTOUGuards(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")
	require.NoError(t, os.WriteFile(fileA, []byte("alpha"), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte("bravo-longer"), 0o644))

	infoA, err := os.Lstat(fileA)
	require.NoError(t, err)
	infoB, err := os.Lstat(fileB)
	require.NoError(t, err)
	dirInfo, err := os.Lstat(dir)
	require.NoError(t, err)

	t.Run("fstat error is returned", func(t *testing.T) {
		_, err := readVerifiedRegularFile("/etc/a.txt", 32, infoA, bytes.NewReader([]byte("alpha")), func() (fs.FileInfo, error) {
			return nil, errors.New("fstat failed")
		})
		require.Error(t, err)
	})

	t.Run("non-regular fstat mode is rejected", func(t *testing.T) {
		_, err := readVerifiedRegularFile("/etc/a.txt", 32, infoA, bytes.NewReader([]byte("alpha")), func() (fs.FileInfo, error) {
			return dirInfo, nil
		})
		require.Error(t, err)
	})

	t.Run("swapped inode failing SameFile is rejected", func(t *testing.T) {
		_, err := readVerifiedRegularFile("/etc/a.txt", 32, infoA, bytes.NewReader([]byte("alpha")), func() (fs.FileInfo, error) {
			return infoB, nil
		})
		require.Error(t, err)
	})

	t.Run("fstat size exceeding maxBytes is rejected", func(t *testing.T) {
		_, err := readVerifiedRegularFile("/etc/b.txt", 5, infoB, bytes.NewReader([]byte("bravo")), func() (fs.FileInfo, error) {
			return infoB, nil
		})
		require.Error(t, err)
	})

	t.Run("concurrent append growing past maxBytes during read is rejected", func(t *testing.T) {
		_, err := readVerifiedRegularFile("/etc/a.txt", 5, infoA, bytes.NewReader([]byte("alpha-extra")), func() (fs.FileInfo, error) {
			return infoA, nil
		})
		require.Error(t, err)
	})

	t.Run("reader I/O error is returned", func(t *testing.T) {
		_, err := readVerifiedRegularFile("/etc/a.txt", 32, infoA, iotest.ErrReader(errors.New("disk read failed")), func() (fs.FileInfo, error) {
			return infoA, nil
		})
		require.Error(t, err)
	})
}

func TestConcurrentReadsAreRaceFree(t *testing.T) {
	root := newTestHostRoot(t)
	wantInfo := []byte(`{"version":"2.0"}`)
	wantRoute := []byte("route-table")
	require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "hccl_rootinfo.json"), wantInfo, 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "usr", "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "usr", "lib", "route.conf"), wantRoute, 0o644))
	require.NoError(t, os.Symlink("/usr/lib", filepath.Join(root, "lib")))

	h, err := OpenHostRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				got, err := h.ReadRegularFile("/etc/hccl_rootinfo.json", 1<<20)
				assert.NoError(t, err)
				assert.Equal(t, wantInfo, got)

				linfo, err := h.Lstat("/lib/route.conf")
				assert.NoError(t, err)
				if linfo != nil {
					assert.True(t, linfo.Mode().IsRegular())
				}

				ok, err := h.Exists("/lib/route.conf")
				assert.NoError(t, err)
				assert.True(t, ok)

				entries, err := h.ReadDir("/")
				assert.NoError(t, err)
				assert.NotEmpty(t, entries)
			}
		}()
	}
	wg.Wait()
}

func TestCloseAndNilReceiverLifecycle(t *testing.T) {
	t.Run("nil and zero-value receivers are safe", func(t *testing.T) {
		var nilFS *ReadOnlyHostFS
		require.NoError(t, nilFS.Close())

		zeroFS := &ReadOnlyHostFS{}
		require.NoError(t, zeroFS.Close())

		for _, h := range []*ReadOnlyHostFS{nilFS, zeroFS} {
			_, err := h.Open("/etc")
			require.ErrorIs(t, err, fs.ErrClosed)

			_, err = h.ReadFile("/etc/foo")
			require.ErrorIs(t, err, fs.ErrClosed)

			_, err = h.ReadRegularFile("/etc/foo", 64)
			require.ErrorIs(t, err, fs.ErrClosed)

			_, err = h.Stat("/etc")
			require.ErrorIs(t, err, fs.ErrClosed)

			_, err = h.Lstat("/etc")
			require.ErrorIs(t, err, fs.ErrClosed)

			_, err = h.ReadDir("/etc")
			require.ErrorIs(t, err, fs.ErrClosed)

			_, err = h.Readlink("/etc/foo")
			require.ErrorIs(t, err, fs.ErrClosed)

			ok, err := h.Exists("/etc")
			require.ErrorIs(t, err, fs.ErrClosed)
			assert.False(t, ok)
		}
	})

	t.Run("double Close succeeds and reads after Close fail", func(t *testing.T) {
		root := newTestHostRoot(t)
		require.NoError(t, os.WriteFile(filepath.Join(root, "etc", "test.conf"), []byte("data"), 0o644))

		h, err := OpenHostRoot(root)
		require.NoError(t, err)

		require.NoError(t, h.Close())
		require.NoError(t, h.Close())

		_, err = h.Open("/etc/test.conf")
		require.ErrorIs(t, err, fs.ErrClosed)

		_, err = h.ReadFile("/etc/test.conf")
		require.ErrorIs(t, err, fs.ErrClosed)

		_, err = h.ReadRegularFile("/etc/test.conf", 64)
		require.ErrorIs(t, err, fs.ErrClosed)

		_, err = h.Stat("/etc/test.conf")
		require.ErrorIs(t, err, fs.ErrClosed)

		_, err = h.Lstat("/etc/test.conf")
		require.ErrorIs(t, err, fs.ErrClosed)

		_, err = h.ReadDir("/etc")
		require.ErrorIs(t, err, fs.ErrClosed)

		_, err = h.Readlink("/etc/test.conf")
		require.ErrorIs(t, err, fs.ErrClosed)

		ok, err := h.Exists("/etc/test.conf")
		require.ErrorIs(t, err, fs.ErrClosed)
		assert.False(t, ok)
	})
}
