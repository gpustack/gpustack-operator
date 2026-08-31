package hygon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/device"
)

// writeCIConf lays down one compute-instance file the way the driver does: the GPU instance's fields
// first, then the compute instance's, with the UUID last.
func writeCIConf(t *testing.T, ciDir, name, uuid string) {
	t.Helper()
	body := "pci:                0000:09:00.0\n" +
		"id:                 5\n" +
		"profile_id:         3\n" +
		"cu_count:           20\n" +
		"memory_size_MB:     16380\n" +
		"pci:                0000:09:00.0\n" +
		"gi_id:              5\n" +
		"id:                 0\n" +
		"profile_id:         0\n" +
		"cu_count:           20\n"
	if uuid != "" {
		body += "mig_uuid:           " + uuid + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(ciDir, name), []byte(body), 0o600))
}

// The registry is the ONLY thing that maps a partition's identifier to an instance the driver
// enumerates -- the library serves no UUID at all -- so what it does with a malformed or missing
// entry decides whether a healthy partition is reportable.
func TestReadMigInstanceRefs(t *testing.T) {
	t.Run("a directory that does not exist is an empty registry, not a failure", func(t *testing.T) {
		refs, err := readMigInstanceRefs(filepath.Join(t.TempDir(), "absent"))

		require.NoError(t, err)
		assert.Empty(t, refs)
	})

	t.Run("every well-formed entry is indexed by its uuid", func(t *testing.T) {
		dir := t.TempDir()
		ciDir := filepath.Join(dir, "ci")
		require.NoError(t, os.MkdirAll(ciDir, 0o700))
		writeCIConf(t, ciDir, "dev0gi3ci0.conf", "3e37fec8-3de4-491f-849c-d249ae437067")
		writeCIConf(t, ciDir, "dev1gi1ci2.conf", "51858397-9b2f-4466-b644-ae3e743963fd")
		writeCIConf(t, ciDir, "dev12gi7ci6.conf", "5cc4c492-0b4f-4b50-b728-ef1688ac3d3a")

		refs, err := readMigInstanceRefs(dir)

		require.NoError(t, err)
		assert.Equal(t, map[string]migInstanceRef{
			"3e37fec8-3de4-491f-849c-d249ae437067": {deviceIndex: 0, gpuInstance: 3, computeInst: 0},
			"51858397-9b2f-4466-b644-ae3e743963fd": {deviceIndex: 1, gpuInstance: 1, computeInst: 2},
			"5cc4c492-0b4f-4b50-b728-ef1688ac3d3a": {deviceIndex: 12, gpuInstance: 7, computeInst: 6},
		}, refs)
	})

	t.Run("one unusable entry does not blind the reader to the rest", func(t *testing.T) {
		dir := t.TempDir()
		ciDir := filepath.Join(dir, "ci")
		require.NoError(t, os.MkdirAll(ciDir, 0o700))
		writeCIConf(t, ciDir, "dev0gi3ci0.conf", "3e37fec8-3de4-491f-849c-d249ae437067")
		writeCIConf(t, ciDir, "dev0gi4ci0.conf", "")                                    // no uuid line
		writeCIConf(t, ciDir, "notaconf.txt", "aaaa-bbbb")                              // name the driver never writes
		require.NoError(t, os.MkdirAll(filepath.Join(ciDir, "dev9gi9ci9.conf"), 0o700)) // a directory

		refs, err := readMigInstanceRefs(dir)

		require.NoError(t, err)
		assert.Equal(t, map[string]migInstanceRef{
			"3e37fec8-3de4-491f-849c-d249ae437067": {deviceIndex: 0, gpuInstance: 3, computeInst: 0},
		}, refs)
	})
}

// A GPU instance id is unique only within its card, so the device index is part of the key: two
// cards both holding gi 0 would otherwise be one entry, and a partition of one would be reported
// with the other's figures.
func TestMigInstanceRefOf(t *testing.T) {
	testCases := []struct {
		name   string
		file   string
		want   migInstanceRef
		wantOK bool
	}{
		{"the driver's own spelling", "dev0gi3ci0.conf", migInstanceRef{0, 3, 0}, true},
		{"multi-digit indices", "dev12gi7ci6.conf", migInstanceRef{12, 7, 6}, true},
		{"a gpu instance file is not a compute instance file", "dev0gi5.conf", migInstanceRef{}, false},
		{"a name with no extension", "dev0gi3ci0", migInstanceRef{}, false},
		{"an unrelated file", "README", migInstanceRef{}, false},
		{"a name that only looks like one", "xdev0gi3ci0.conf", migInstanceRef{}, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := migInstanceRefOf(tc.file)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Every way a partition can fail to be measured has to stay distinguishable from a partition that is
// simply idle, because the two look identical in the figures and only the reason tells them apart.
func TestResolvePartitions(t *testing.T) {
	const (
		liveUUID  = "3e37fec8-3de4-491f-849c-d249ae437067"
		staleUUID = "00000000-0000-0000-0000-000000000000"
	)
	// The measured capacity of one 2g.15gb instance, in bytes.
	const _TwoGInstanceBytes uint64 = 17_180_720_640

	liveRef := migInstanceRef{deviceIndex: 0, gpuInstance: 3, computeInst: 0}
	refs := map[string]migInstanceRef{liveUUID: liveRef}

	request := func(id string) device.AcceleratorPartitionRequest {
		return device.AcceleratorPartitionRequest{
			DeviceID:   "GPU-abc",
			ID:         id,
			Profile:    "2g.15gb",
			Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
		}
	}

	testCases := []struct {
		name       string
		id         string
		src        migPartitionSource
		wantID     string
		wantTotal  *uint64
		wantUsed   *uint64
		wantCores  *uint32
		wantReason device.AcceleratorProcessReason
	}{
		{
			name: "a live partition is reported whole, compute included",
			id:   _MigInstanceIDPrefix + liveUUID,
			src: func(ref migInstanceRef) (migPartitionRead, device.AcceleratorProcessReason, bool) {
				assert.Equal(t, liveRef, ref)
				return migPartitionRead{
					memoryTotalBytes: _TwoGInstanceBytes,
					memoryUsedBytes:  1_738_539_008,
					coresPercent:     95,
				}, device.AcceleratorProcessReasonNone, true
			},
			wantID:     _MigInstanceIDPrefix + liveUUID,
			wantTotal:  ptrU64(_TwoGInstanceBytes),
			wantUsed:   ptrU64(1_738_539_008),
			wantCores:  ptrU32(95),
			wantReason: device.AcceleratorProcessReasonNone,
		},
		{
			name: "an idle partition reads zero, which is a measurement rather than an absence",
			id:   liveUUID, // the bare spelling resolves the same instance
			src: func(migInstanceRef) (migPartitionRead, device.AcceleratorProcessReason, bool) {
				return migPartitionRead{
					memoryTotalBytes: _TwoGInstanceBytes,
					memoryUsedBytes:  0,
					coresPercent:     0,
				}, device.AcceleratorProcessReasonNone, true
			},
			wantID:     _MigInstanceIDPrefix + liveUUID,
			wantTotal:  ptrU64(_TwoGInstanceBytes),
			wantUsed:   ptrU64(0),
			wantCores:  ptrU32(0),
			wantReason: device.AcceleratorProcessReasonNone,
		},
		{
			name: "an allocation carrying no identifier cannot be addressed",
			id:   "",
			src: func(migInstanceRef) (migPartitionRead, device.AcceleratorProcessReason, bool) {
				t.Fatal("the driver must not be consulted for an unaddressable request")
				return migPartitionRead{}, device.AcceleratorProcessReasonNone, false
			},
			wantReason: device.AcceleratorProcessReasonDriverError,
		},
		{
			name: "an identifier the registry no longer carries is an absence, never a sibling's figures",
			id:   _MigInstanceIDPrefix + staleUUID,
			src: func(migInstanceRef) (migPartitionRead, device.AcceleratorProcessReason, bool) {
				t.Fatal("the driver must not be consulted for an unknown identifier")
				return migPartitionRead{}, device.AcceleratorProcessReasonNone, false
			},
			wantReason: device.AcceleratorProcessReasonDriverError,
		},
		{
			name: "the registry names it but the driver does not enumerate it",
			id:   _MigInstanceIDPrefix + liveUUID,
			src: func(migInstanceRef) (migPartitionRead, device.AcceleratorProcessReason, bool) {
				return migPartitionRead{}, device.AcceleratorProcessReasonNone, false
			},
			wantReason: device.AcceleratorProcessReasonDriverError,
		},
		{
			name: "a read the driver refused carries the driver's own reason",
			id:   _MigInstanceIDPrefix + liveUUID,
			src: func(migInstanceRef) (migPartitionRead, device.AcceleratorProcessReason, bool) {
				return migPartitionRead{}, device.AcceleratorProcessReasonPermission, true
			},
			wantReason: device.AcceleratorProcessReasonPermission,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePartitions([]device.AcceleratorPartitionRequest{request(tc.id)}, refs, tc.src)

			require.Len(t, got, 1)
			assert.Equal(t, "GPU-abc", got[0].DeviceID)
			assert.Equal(t, "2g.15gb", got[0].Profile)
			assert.Equal(t, tc.wantID, got[0].ID)
			assert.Equal(t, tc.wantTotal, got[0].MemoryTotalBytes)
			assert.Equal(t, tc.wantUsed, got[0].MemoryUsedBytes)
			assert.Equal(t, tc.wantCores, got[0].CoresPercent)
			assert.Equal(t, tc.wantReason, got[0].MemoryReason)
			// Compute and memory come off one handle, so they can never disagree about why they are
			// absent -- unlike on a manufacturer whose library serves no compute figure at all.
			assert.Equal(t, tc.wantReason, got[0].CoresReason)
		})
	}
}

func ptrU64(v uint64) *uint64 { return &v }
func ptrU32(v uint32) *uint32 { return &v }
