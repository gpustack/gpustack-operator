package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveProjectDir pins the paths that must be refused. Each case asserts the
// verdict rather than the message, since the refusal is what protects the tree.
//
// Both refusals below were live defects in the first version of the guard:
//   - it compared the unresolved path;
//   - it compared a bare string tail.
func TestResolveProjectDir(t *testing.T) {
	root := t.TempDir()

	// A real checkout, and a lookalike whose last two segments spell the module path but
	// whose parent absorbs part of it.
	good := filepath.Join(root, "checkout", "gpustack.ai", "gpustack")
	lookalike := filepath.Join(root, "mygpustack.ai", "gpustack")
	unrelated := filepath.Join(root, "somewhere", "else")
	for _, d := range []string{good, lookalike, unrelated} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// A symlink named after the module path, pointing at a directory that is not one.
	linkDir := filepath.Join(root, "link", "gpustack.ai")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", linkDir, err)
	}
	link := filepath.Join(linkDir, "gpustack")
	if err := os.Symlink(unrelated, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cases := []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{name: "a real module-suffixed checkout is accepted", dir: good},
		{name: "a parent absorbing part of the module path is refused", dir: lookalike, wantErr: true},
		{name: "an unrelated directory is refused", dir: unrelated, wantErr: true},
		{name: "a symlink named like the module path is refused", dir: link, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveProjectDir(tc.dir)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveProjectDir(%q) = %q, want refusal", tc.dir, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveProjectDir(%q): %v", tc.dir, err)
			}
			// An accepted directory comes back resolved and slash-normalized, since
			// that value is what the generators are configured with.
			//
			// LIMITED: the normalization half is untestable off Windows. ToSlash is
			// the identity where the separator is already '/', so a defect returning
			// the un-normalized path leaves this green on linux and darwin.
			w, err := filepath.EvalSymlinks(tc.dir)
			if err != nil {
				t.Fatalf("EvalSymlinks(%q): %v", tc.dir, err)
			}
			want := filepath.ToSlash(w)
			if got != want {
				t.Fatalf("resolveProjectDir(%q) = %q, want the resolved slash-normalised path %q", tc.dir, got, want)
			}
		})
	}
}
