package deviceplugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCDIKind  = "nvidia.com/gpu"
	testCDIUUID0 = "GPU-1068d7f6-26eb-5225-371e-58ca20150673"
	testCDIUUID1 = "GPU-bbc521eb-4a08-800b-eb2a-770a4a5b8a8c"
)

// A specification as a vendor generator publishes it by default: every accelerator named twice, once
// by index and once by its own identifier, plus the "all" entry. Trimmed to the parts this reads.
const testCDISpecYAML = `---
cdiVersion: 0.7.0
kind: ` + testCDIKind + `
devices:
    - name: "0"
      containerEdits:
        deviceNodes:
            - path: /dev/nvidia0
    - name: ` + testCDIUUID0 + `
      containerEdits:
        deviceNodes:
            - path: /dev/nvidia0
    - name: all
      containerEdits:
        deviceNodes:
            - path: /dev/nvidia0
`

// testCDISpec renders the smallest specification the reader accepts: both declarations the container
// engine requires of a document before it loads it, and the named devices.
func testCDISpec(kind string, devices ...string) string {
	spec := "cdiVersion: 0.7.0\nkind: " + kind + "\ndevices:\n"
	for _, device := range devices {
		spec += "  - name: " + device + "\n"
	}

	return spec
}

// writeTestFile writes one fixture file, creating its directory.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// redirectCDISpecDirs points the specification directories at a temporary tree, keeping the static and
// dynamic pair the real ones have, and returns its root. Nothing is created under it: an absent
// directory is the case most of these tests are about.
func redirectCDISpecDirs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	orig := CDISpecDirs
	CDISpecDirs = []string{filepath.Join(root, "etc/cdi"), filepath.Join(root, "run/cdi")}
	t.Cleanup(func() { CDISpecDirs = orig })

	return root
}

func TestLoadCDISpecs(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		// unlistablePath is one of CDISpecDirs made impossible to list, which is the difference
		// between a directory that is not there and one whose contents cannot be established.
		//
		// It is realized as a REGULAR FILE at that path, not as a directory with its read bit
		// stripped. chmod 0o000 is what this used to do and root walks straight through it: the
		// listing succeeds, Unreadable stays false, and the row fails -- so the verdict was decided
		// by who ran the test rather than by the code, and any CI running tests as root reported a
		// red about its own environment. ENOTDIR grants nobody an exemption, and it reaches the same
		// branch, because that branch turns on ReadDir failing for any reason other than "not
		// there" and this file is very much there.
		unlistablePath string
		want           []string
		wantAbsent     []string
		wantUnreadable bool
	}{
		{
			name:  "no directory at all",
			files: nil,
		},
		{
			// A device manager mounts the specification directory with DirectoryOrCreate, so the mount
			// itself creates an empty directory on a host with no CDI. Taking the directory as evidence
			// would conclude CDI was available on every node this ran on.
			name:  "a directory that exists but holds no specification",
			files: map[string]string{"run/cdi/.keep": ""},
		},
		{
			name:  "a specification names its devices by index, by identifier and as all",
			files: map[string]string{"run/cdi/nvidia.yaml": testCDISpecYAML},
			want: []string{
				testCDIKind + "=0",
				testCDIKind + "=" + testCDIUUID0,
				testCDIKind + "=all",
			},
		},
		{
			// The engine reads the static directory too, and a node whose generator was run by hand has
			// its only specification there.
			name:  "the static specification directory is read as well",
			files: map[string]string{"etc/cdi/nvidia.yaml": testCDISpecYAML},
			want:  []string{testCDIKind + "=" + testCDIUUID0},
		},
		{
			// YAML is a superset of JSON, so one decoder reads both spellings a specification may be
			// published in.
			name: "a json specification is read too",
			files: map[string]string{
				"run/cdi/nvidia.json": `{"cdiVersion":"0.7.0","kind":"` + testCDIKind +
					`","devices":[{"name":"` + testCDIUUID1 + `"}]}`,
			},
			want: []string{testCDIKind + "=" + testCDIUUID1},
		},
		{
			name: "every specification in a directory is read, whatever kind it declares",
			files: map[string]string{
				"run/cdi/nvidia.yaml": testCDISpecYAML,
				"run/cdi/other.yaml":  testCDISpec("example.com/dev", "d0"),
			},
			want: []string{testCDIKind + "=" + testCDIUUID0, "example.com/dev=d0"},
		},
		{
			// The engine's own rule, and the case it exists for: a generator rewrote a stale hand-placed
			// file with a new set of accelerators. A union would claim the old one, which the engine
			// will not resolve.
			name: "the dynamic directory's specification replaces a static one of the same name",
			files: map[string]string{
				"etc/cdi/nvidia.yaml": testCDISpecYAML,
				"run/cdi/nvidia.yaml": testCDISpec(testCDIKind, testCDIUUID1),
			},
			want:       []string{testCDIKind + "=" + testCDIUUID1},
			wantAbsent: []string{testCDIKind + "=" + testCDIUUID0},
		},
		{
			// The same shadowing, with the replacement unreadable. What the static file named is no
			// longer what the engine loaded, so keeping it would answer from a specification the node
			// has replaced — the very staleness the keying exists to prevent.
			name: "an unreadable specification shadows the static one of the same name",
			files: map[string]string{
				"etc/cdi/nvidia.yaml": testCDISpecYAML,
				"run/cdi/nvidia.yaml": "\tthis: is: not: yaml\n",
			},
			wantAbsent:     []string{testCDIKind + "=" + testCDIUUID0},
			wantUnreadable: true,
		},
		{
			// Different file names are both loaded, however many directories they are spread over.
			name: "a static specification the dynamic directory does not shadow is kept",
			files: map[string]string{
				"etc/cdi/vendor-a.yaml": testCDISpec("a.com/dev", "d0"),
				"run/cdi/vendor-b.yaml": testCDISpec("b.com/dev", "d1"),
			},
			want: []string{"a.com/dev=d0", "b.com/dev=d1"},
		},
		{
			name:  "a file that is not a specification is skipped",
			files: map[string]string{"run/cdi/notes.txt": "kind: " + testCDIKind + "\n"},
		},
		{
			// An absent directory names nothing, which is an answer. Reporting it as unreadable would
			// make every node without CDI look like a node this could not read.
			name:           "a directory that is not there is not an unreadable one",
			files:          map[string]string{"run/cdi/nvidia.yaml": testCDISpecYAML},
			want:           []string{testCDIKind + "=" + testCDIUUID0},
			wantUnreadable: false,
		},
		{
			// And the converse: a directory that exists and cannot be listed holds an unknown, which
			// includes whatever it may shadow from the directory before it.
			name:           "a directory that cannot be listed is an unreadable view",
			files:          map[string]string{"etc/cdi/nvidia.yaml": testCDISpecYAML},
			unlistablePath: "run/cdi",
			want:           []string{testCDIKind + "=" + testCDIUUID0},
			wantUnreadable: true,
		},
		{
			name:           "a specification declaring no kind is unreadable, not empty",
			files:          map[string]string{"run/cdi/broken.yaml": "cdiVersion: 0.7.0\ndevices:\n  - name: \"0\"\n"},
			wantUnreadable: true,
		},
		{
			// The engine requires a version too, and does not load a document without one. Reading its
			// names anyway would offer auto a device the engine never heard of.
			name:           "a specification declaring no cdiVersion is unreadable, not empty",
			files:          map[string]string{"run/cdi/broken.yaml": "kind: " + testCDIKind + "\ndevices:\n  - name: \"0\"\n"},
			wantUnreadable: true,
		},
		{
			// A file that could not be parsed is not a file that names nothing. Reporting the two alike
			// would let one malformed specification turn a good accelerator into a definite "nothing
			// names it".
			name: "a malformed specification is reported as unreadable, not as naming nothing",
			files: map[string]string{
				"run/cdi/nvidia.yaml": testCDISpecYAML,
				"run/cdi/broken.yaml": "\tthis: is: not: yaml\n",
			},
			want:           []string{testCDIKind + "=" + testCDIUUID0},
			wantUnreadable: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			root := redirectCDISpecDirs(t)
			for rel, content := range c.files {
				writeTestFile(t, filepath.Join(root, rel), content)
			}
			if c.unlistablePath != "" {
				path := filepath.Join(root, c.unlistablePath)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte("not a directory\n"), 0o644))
			}

			got := LoadCDISpecs()
			for _, want := range c.want {
				assert.Contains(t, got.Names, want)
			}
			for _, absent := range c.wantAbsent {
				assert.NotContains(t, got.Names, absent)
			}
			if len(c.want) == 0 {
				assert.Empty(t, got.Names)
			}
			assert.Equal(t, c.wantUnreadable, got.Unreadable)
		})
	}
}

func TestCDISpecsMissing(t *testing.T) {
	root := redirectCDISpecDirs(t)
	writeTestFile(t, filepath.Join(root, "run/cdi/nvidia.yaml"), testCDISpecYAML)
	specs := LoadCDISpecs()

	assert.Empty(t, specs.Missing(CDIDeviceNames(testCDIKind, []string{testCDIUUID0})))
	// Sorted, so the message a user reads does not change between identical runs.
	assert.Equal(t,
		[]string{testCDIKind + "=" + testCDIUUID1, "other.com/dev=x"},
		specs.Missing([]string{"other.com/dev=x", CDIDeviceName(testCDIKind, testCDIUUID1)}))
}

func TestSetCDIRequest(t *testing.T) {
	resp := &ContainerAllocateResponse{Envs: map[string]string{"KEEP": "1"}}
	SetCDIRequest(resp, "gpustack-nvidia", CDIDeviceNames(testCDIKind, []string{testCDIUUID0, testCDIUUID1}))

	assert.Equal(t, map[string]string{
		"cdi.k8s.io/gpustack-nvidia": testCDIKind + "=" + testCDIUUID0 + "," + testCDIKind + "=" + testCDIUUID1,
	}, resp.Annotations)
	assert.Equal(t, "1", resp.Envs["KEEP"], "an unrelated env is left alone")
	assert.Empty(t, resp.CdiDevices, "the typed field is below this chart's kubernetes floor")
}
