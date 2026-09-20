package deviceplugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verbsFixture builds a throwaway sysfs tree so the verbs resolution can be exercised without
// a Linux host.
//
// The layout mirrors the real one rather than a convenient flattening of it, because the
// resolution depends on that layout: class entries are symlinks into the devices tree, the
// uverbs entries carry an ibdev attribute naming their RDMA device, and where a symlink lands
// is part of the answer. A symlink target outside the root is how an escape is planted.
type verbsFixture struct {
	t    *testing.T
	root string
}

func newVerbsFixture(t *testing.T) *verbsFixture {
	t.Helper()
	return &verbsFixture{t: t, root: t.TempDir()}
}

func (f *verbsFixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", p, err)
	}
}

func (f *verbsFixture) mkdir(rel string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.root, rel), 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", rel, err)
	}
}

// symlink points rel at target. target is interpreted relative to the fixture root unless
// absolute, which is how a link escaping the root is planted.
func (f *verbsFixture) symlink(target, rel string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(f.root, target)
	}
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatalf("symlink %s -> %s: %v", p, target, err)
	}
}

// addVerbsClassEntry plants one entry of the verbs class layout the way the kernel exposes
// it: a real directory under the devices tree, with the class entry a symlink to it. The
// attribute carries the trailing newline the kernel writes.
func (f *verbsFixture) addVerbsClassEntry(entry, ibdev string) {
	f.t.Helper()
	realDir := filepath.Join("devices/virtual/infiniband_verbs", entry)
	f.write(filepath.Join(realDir, ibdevAttr), ibdev+"\n")
	f.symlink(realDir, filepath.Join(sysfsVerbsClassDir, entry))
}

// addVerbsDeviceTreeEntry plants one uverbs entry the way the kernel hangs it off a device's
// hardware parent: under a directory named for its class, never directly under the parent. Every
// device-tree case goes through this rather than joining the path itself, so the layout is written
// down once -- a fixture that spells it out per case is free to spell it the way the code happens
// to read, and then the layout tests compare the code with itself.
func (f *verbsFixture) addVerbsDeviceTreeEntry(parentRel, entry, ibdev string) {
	f.t.Helper()
	f.write(filepath.Join(parentRel, sysfsVerbsClassName, entry, ibdevAttr), ibdev+"\n")
}

// verbsDeviceTreeEntryPath is where addVerbsDeviceTreeEntry puts an entry, for the cases that
// plant something other than a readable attribute there.
func verbsDeviceTreeEntryPath(parentRel, entry string) string {
	return filepath.Join(parentRel, sysfsVerbsClassName, entry)
}

// addRDMADevice plants an RDMA device bound to a hardware parent, both directions as the
// kernel does: the class entry points at the device directory, whose own device attribute
// points back up at the hardware parent.
func (f *verbsFixture) addRDMADevice(name, parentRel string) {
	f.t.Helper()
	rdmaRel := filepath.Join(parentRel, "infiniband", name)
	f.mkdir(rdmaRel)
	f.symlink(rdmaRel, filepath.Join(sysfsInfinibandDir, name))
	f.symlink(parentRel, filepath.Join(rdmaRel, "device"))
}

const fixturePCIRel = "devices/pci0000:00/0000:00:07.0"

// TestVerbsDevicePath covers the resolution itself: each layout answering alone, the class
// layout answering first when both hold the mapping, and a name under neither layout failing
// with an error that names both.
func TestVerbsDevicePath(t *testing.T) {
	testCases := []struct {
		name string
		root func(f *verbsFixture) string
		// build plants the tree the case needs.
		build func(f *verbsFixture)
		// device is the RDMA device name being resolved.
		device string
		// wantPath is the expected character device path, when no error is expected.
		wantPath string
		// wantErr holds substrings the error must carry, every one of them.
		wantErr []string
	}{
		{
			name: "the verbs class layout answers",
			build: func(f *verbsFixture) {
				// The class directory also carries a class attribute that is no uverbs
				// entry, the way the real one carries abi_version.
				f.write(filepath.Join(sysfsVerbsClassDir, "abi_version"), "6")
				f.addVerbsClassEntry("uverbs0", "mlx5_1")
				f.addVerbsClassEntry("uverbs1", "mlx5_0")
			},
			device:   "mlx5_0",
			wantPath: devInfinibandDir + "/uverbs1",
		},
		{
			name: "the device tree layout answers when the verbs class is absent",
			build: func(f *verbsFixture) {
				f.addRDMADevice("mlx5_0", fixturePCIRel)
				f.addVerbsDeviceTreeEntry(fixturePCIRel, "uverbs0", "mlx5_0")
				f.addVerbsDeviceTreeEntry(fixturePCIRel, "uverbs1", "mlx5_1")
			},
			device:   "mlx5_0",
			wantPath: devInfinibandDir + "/uverbs0",
		},
		{
			name: "the class layout answers first when both hold the mapping",
			build: func(f *verbsFixture) {
				f.addRDMADevice("mlx5_0", fixturePCIRel)
				f.addVerbsDeviceTreeEntry(fixturePCIRel, "uverbs0", "mlx5_0")
				f.addVerbsClassEntry("uverbs3", "mlx5_0")
			},
			device:   "mlx5_0",
			wantPath: devInfinibandDir + "/uverbs3",
		},
		{
			name: "a clean match later in the directory answers despite an unreadable sibling",
			build: func(f *verbsFixture) {
				// uverbs0 sorts first and cannot be consulted; it might have been the
				// match, so it may not be skipped silently -- but uverbs1 answers.
				f.symlink("/nonexistent-escape-target",
					filepath.Join(sysfsVerbsClassDir, "uverbs0"))
				f.addVerbsClassEntry("uverbs1", "mlx5_0")
			},
			device:   "mlx5_0",
			wantPath: devInfinibandDir + "/uverbs1",
		},
		{
			name: "a name under neither layout fails naming both",
			build: func(f *verbsFixture) {
				f.addVerbsClassEntry("uverbs0", "mlx5_1")
				f.addRDMADevice("mlx5_1", fixturePCIRel)
				f.addVerbsDeviceTreeEntry(fixturePCIRel, "uverbs0", "mlx5_1")
			},
			device: "mlx5_9",
			wantErr: []string{
				sysfsVerbsClassDir,
				sysfsVerbsClassName + `" directory under`,
				filepath.Join(sysfsInfinibandDir, "mlx5_9"),
			},
		},
		{
			name:   "an empty tree fails naming both",
			build:  func(f *verbsFixture) {},
			device: "mlx5_0",
			wantErr: []string{
				sysfsVerbsClassDir,
				sysfsVerbsClassName + `" directory under`,
			},
		},
		{
			name:    "an empty RDMA device name is refused",
			build:   func(f *verbsFixture) {},
			device:  "",
			wantErr: []string{"name is empty"},
		},
		{
			name: "an unresolvable sysfs root is refused",
			root: func(f *verbsFixture) string {
				return filepath.Join(f.root, "absent")
			},
			build:   func(f *verbsFixture) {},
			device:  "mlx5_0",
			wantErr: []string{"resolve the sysfs root"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerbsFixture(t)
			tc.build(f)
			root := f.root
			if tc.root != nil {
				root = tc.root(f)
			}

			got, err := verbsDevicePath(root, tc.device)

			switch {
			case tc.wantPath != "":
				if err != nil {
					t.Fatalf("verbsDevicePath(%q): unexpected error: %v", tc.device, err)
				}
				if got != tc.wantPath {
					t.Errorf("verbsDevicePath(%q) = %q, want %q", tc.device, got, tc.wantPath)
				}
			case len(tc.wantErr) > 0:
				if err == nil {
					t.Fatalf("verbsDevicePath(%q) = %q, want an error", tc.device, got)
				}
				for _, sub := range tc.wantErr {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q does not name %q", err.Error(), sub)
					}
				}
			default:
				t.Fatal("the case states no expected outcome")
			}
		})
	}
}

// plantOutside writes a file under a tree the fixture does not own, for planting symlinks
// that resolve outside the root.
func plantOutside(t *testing.T, outside, rel, content string) {
	t.Helper()
	p := filepath.Join(outside, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// TestVerbsDevicePathRefusesEscapes pins the refusal a symlink chain landing outside the
// root must produce, on each path the resolution follows: a class entry, the device link that
// anchors the second layout, and a device-tree candidate.
func TestVerbsDevicePathRefusesEscapes(t *testing.T) {
	testCases := []struct {
		name  string
		build func(f *verbsFixture, outside string)
	}{
		{
			name: "a verbs class entry escaping the root",
			build: func(f *verbsFixture, outside string) {
				plantOutside(t, outside, filepath.Join("uverbs0", ibdevAttr), "mlx5_0\n")
				f.symlink(filepath.Join(outside, "uverbs0"),
					filepath.Join(sysfsVerbsClassDir, "uverbs0"))
			},
		},
		{
			name: "the device link anchoring the second layout escaping the root",
			build: func(f *verbsFixture, outside string) {
				f.mkdir(filepath.Join(sysfsInfinibandDir, "mlx5_0"))
				plantOutside(t, outside, "parked", "")
				f.symlink(outside, filepath.Join(sysfsInfinibandDir, "mlx5_0", "device"))
			},
		},
		{
			name: "a device-tree candidate escaping the root",
			build: func(f *verbsFixture, outside string) {
				f.addRDMADevice("mlx5_0", fixturePCIRel)
				plantOutside(t, outside, filepath.Join("uverbs0", ibdevAttr), "mlx5_0\n")
				f.symlink(filepath.Join(outside, "uverbs0"),
					verbsDeviceTreeEntryPath(fixturePCIRel, "uverbs0"))
			},
		},
		{
			name: "the verbs class directory itself escaping the root",
			build: func(f *verbsFixture, outside string) {
				plantOutside(t, outside, "parked", "")
				f.symlink(outside, sysfsVerbsClassDir)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerbsFixture(t)
			// A second throwaway tree plays the part of filesystem this process does not
			// own: real, resolvable, and outside the sysfs root.
			tc.build(f, t.TempDir())

			_, err := verbsDevicePath(f.root, "mlx5_0")
			if err == nil {
				t.Fatal("verbsDevicePath resolved through a symlink escaping the root")
			}
			if !strings.Contains(err.Error(), "outside") {
				t.Errorf("error %q does not say the resolution landed outside the root", err.Error())
			}
		})
	}
}

// TestVerbsDevicePathUnreadableTreeIsNotAbsence pins the difference between a layout that is
// not there and one that is there and could not be read: the second might have carried the
// match, so a miss over it is reported as unestablished rather than as the device's absence.
func TestVerbsDevicePathUnreadableTreeIsNotAbsence(t *testing.T) {
	testCases := []struct {
		name string
		// build plants a tree where at least one candidate cannot be consulted.
		build func(f *verbsFixture)
		// wantCause is a substring of the underlying failure the error must carry.
		wantCause string
	}{
		{
			name: "the verbs class directory is a dangling link",
			build: func(f *verbsFixture) {
				f.symlink("/nonexistent-escape-target", sysfsVerbsClassDir)
			},
			wantCause: sysfsVerbsClassDir,
		},
		{
			name: "an ibdev attribute exceeds the read cap",
			build: func(f *verbsFixture) {
				f.write(filepath.Join(sysfsVerbsClassDir, "uverbs0", ibdevAttr),
					strings.Repeat("x", sysfsAttrMaxBytes+1))
			},
			wantCause: "exceeds",
		},
		{
			name: "a class entry has no ibdev attribute to read",
			build: func(f *verbsFixture) {
				f.mkdir(filepath.Join("devices/virtual/infiniband_verbs", "uverbs0"))
				f.symlink(filepath.Join("devices/virtual/infiniband_verbs", "uverbs0"),
					filepath.Join(sysfsVerbsClassDir, "uverbs0"))
			},
			wantCause: ibdevAttr,
		},
		{
			name: "the RDMA class entry anchoring the second layout is a dangling link",
			build: func(f *verbsFixture) {
				f.symlink("/nonexistent-escape-target",
					filepath.Join(sysfsInfinibandDir, "mlx5_0"))
			},
			wantCause: filepath.Join(sysfsInfinibandDir, "mlx5_0"),
		},
		{
			name: "a device-tree candidate cannot be consulted",
			build: func(f *verbsFixture) {
				f.addRDMADevice("mlx5_0", fixturePCIRel)
				f.symlink("/nonexistent-escape-target", verbsDeviceTreeEntryPath(fixturePCIRel, "uverbs0"))
			},
			wantCause: "/nonexistent-escape-target",
		},
		{
			name: "the verbs class directory is a regular file",
			build: func(f *verbsFixture) {
				f.write(sysfsVerbsClassDir, "not a directory")
			},
			wantCause: "not a directory",
		},
		{
			name: "the ibdev attribute is a directory",
			build: func(f *verbsFixture) {
				f.mkdir(filepath.Join("devices/virtual/infiniband_verbs", "uverbs0", ibdevAttr))
				f.symlink(filepath.Join("devices/virtual/infiniband_verbs", "uverbs0"),
					filepath.Join(sysfsVerbsClassDir, "uverbs0"))
			},
			wantCause: "is a directory",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerbsFixture(t)
			tc.build(f)

			_, err := verbsDevicePath(f.root, "mlx5_0")
			if err == nil {
				t.Fatal("verbsDevicePath reported an outcome over a tree it could not read")
			}
			if !strings.Contains(err.Error(), "could not be established") {
				t.Errorf("error %q claims an absence rather than an unestablished answer", err.Error())
			}
			if !strings.Contains(err.Error(), tc.wantCause) {
				t.Errorf("error %q does not carry the cause %q", err.Error(), tc.wantCause)
			}
		})
	}
}
