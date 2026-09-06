package binding

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGetNumaNodeByBDFKeepsUnknownDistinctFromNodeZero pins the three readings
// /sys/bus/pci/devices/<bdf>/numa_node can carry apart.
//
// This is the check whose absence let the defect ship. The proximity comparison in pkg/device is
// already tested for an empty accelerator affinity, and that test passed throughout: it exercises
// the branch that was always correct, while the collapse happened one layer down, here, where "-1"
// and an unparseable reading both became "0". A test asserting the empty branch of the consumer can
// never fail for this, whatever the producer does.
//
// It lives in a _linux_test.go file because the reading is sysfs, and it constructs the tree rather
// than reading the host's: a virtualized host reports -1 for every device and a bare-metal one may
// report none at all, so no single machine can present all three readings at once.
func TestGetNumaNodeByBDFKeepsUnknownDistinctFromNodeZero(t *testing.T) {
	root := t.TempDir()
	original := sysfsPCIDevicesPath
	sysfsPCIDevicesPath = root
	t.Cleanup(func() { sysfsPCIDevicesPath = original })

	// write lays down one device's numa_node exactly as the kernel would, contents and all.
	write := func(t *testing.T, bdf, contents string) {
		t.Helper()
		dir := filepath.Join(root, bdf)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "numa_node"), []byte(contents), 0o644); err != nil {
			t.Fatalf("write numa_node for %s: %v", bdf, err)
		}
	}

	testCases := []struct {
		name     string
		bdf      string
		contents string
		present  bool
		want     string
	}{
		{
			// The positive baseline. Without it every other row would also pass on a helper that
			// answered empty unconditionally, which is a different defect in the same place.
			name: "node 0 is a node",
			bdf:  "0000:01:00.0", contents: "0\n", present: true, want: "0",
		},
		{
			name: "a numbered node is reported as itself",
			bdf:  "0000:02:00.0", contents: "3\n", present: true, want: "3",
		},
		{
			// The kernel's sentinel for "this device has no NUMA affinity". Reported as "0" before
			// the fix, which is what let the proximity comparison answer NODE against an interface
			// genuinely on node 0.
			name: "the kernel's -1 sentinel is UNKNOWN, not node 0",
			bdf:  "0000:03:00.0", contents: "-1\n", present: true, want: "",
		},
		{
			name: "an unparseable reading is UNKNOWN, not node 0",
			bdf:  "0000:04:00.0", contents: "unexpected\n", present: true, want: "",
		},
		{
			name: "an empty file is UNKNOWN, not node 0",
			bdf:  "0000:05:00.0", contents: "", present: true, want: "",
		},
		{
			name: "a device with no numa_node at all is UNKNOWN",
			bdf:  "0000:06:00.0", present: false, want: "",
		},
		{
			name: "an empty BDF is UNKNOWN",
			bdf:  "", present: false, want: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.present {
				write(t, tc.bdf, tc.contents)
			}
			if got := GetNumaNodeByBDF(tc.bdf); got != tc.want {
				t.Errorf("GetNumaNodeByBDF(%q) with numa_node %q = %q, want %q",
					tc.bdf, tc.contents, got, tc.want)
			}
		})
	}

	// The invariant the rows above are instances of, asserted as a relation so it survives a change
	// to how either state is spelled: whatever "no affinity" becomes, it must not become whatever
	// "node 0" is. Two rows agreeing on a literal would not catch a future edit that made both "0"
	// again.
	write(t, "0000:07:00.0", "0\n")
	write(t, "0000:08:00.0", "-1\n")
	if zero, unknown := GetNumaNodeByBDF("0000:07:00.0"), GetNumaNodeByBDF("0000:08:00.0"); zero == unknown {
		t.Errorf("node 0 and no-affinity both read as %q; a proximity comparison cannot tell them apart", zero)
	}
}
