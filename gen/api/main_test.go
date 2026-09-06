package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveProjectDir pins the two ways the suffix check can be satisfied by a path the
// generators must not write against. Both were live defects in the first version of this
// guard: it compared the unresolved path, and it compared a bare string tail.
//
// The refusal is what protects the tree -- go-to-protobuf rewrites the generated files
// before a wrong output base is noticed -- so each case asserts the verdict, not a message.
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
			// The resolved path is what the generators are configured with, so an
			// accepted directory has to come back resolved rather than as given.
			//
			// NB: the slash-normalisation half of this assertion carries no information
			// off Windows: filepath.ToSlash is the identity on any platform whose
			// separator is already '/', so injecting a defect that returns the
			// un-normalised path leaves this test green here. It is asserted anyway
			// because the value is what the downstream trim runs against, but do not
			// read a passing run on linux or darwin as having exercised it.
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
