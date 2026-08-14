package thead

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/device"
)

func partitionRequest(id, profile string, start, length int32) device.AcceleratorPartitionRequest {
	return device.AcceleratorPartitionRequest{
		DeviceID:   "PPU-0",
		ID:         id,
		Profile:    profile,
		Placements: []device.AcceleratorPlacement{{Start: start, Length: length}},
	}
}

// addressableSource is a partitioned card that answers for the partitions it carries and for no
// others, so a case states only what it changes.
func addressableSource(carried map[string]migPartition) migPartitionSource {
	return migPartitionSource{
		migEnabled: func() (bool, device.AcceleratorProcessReason) {
			return true, device.AcceleratorProcessReasonNone
		},
		partitionByID: func(id string) (migPartition, device.AcceleratorProcessReason, bool) {
			read, ok := carried[id]
			if !ok {
				return migPartition{}, device.AcceleratorProcessReasonNone, false
			}
			return read, device.AcceleratorProcessReasonNone, true
		},
	}
}

// TestResolvePartitions is this detector's whole contract: a partition is addressed by the
// identifier the allocation recorded, every request is answered whether or not anything could be
// read from it, and compute is never reported.
func TestResolvePartitions(t *testing.T) {
	carried := map[string]migPartition{
		"MIG-aaa": {id: "MIG-aaa", totalBytes: 10 << 30, usedBytes: 1 << 30},
		"MIG-bbb": {id: "MIG-bbb", totalBytes: 10 << 30, usedBytes: 0},
	}

	cases := []struct {
		name     string
		requests []device.AcceleratorPartitionRequest
		source   func() migPartitionSource
		want     []device.AcceleratorPartition
	}{
		{
			name:     "a recorded identifier reads its own partition",
			requests: []device.AcceleratorPartitionRequest{partitionRequest("MIG-aaa", "1g.10gb", 0, 1)},
			source:   func() migPartitionSource { return addressableSource(carried) },
			want: []device.AcceleratorPartition{{
				DeviceID:         "PPU-0",
				ID:               "MIG-aaa",
				Profile:          "1g.10gb",
				Placements:       []device.AcceleratorPlacement{{Start: 0, Length: 1}},
				MemoryTotalBytes: ptrTo[uint64](10 << 30),
				MemoryUsedBytes:  ptrTo[uint64](1 << 30),
				CoresReason:      device.AcceleratorProcessReasonUnsupported,
			}},
		},
		{
			name:     "an idle partition reads zero, not an absence",
			requests: []device.AcceleratorPartitionRequest{partitionRequest("MIG-bbb", "1g.10gb", 1, 1)},
			source:   func() migPartitionSource { return addressableSource(carried) },
			want: []device.AcceleratorPartition{{
				DeviceID:         "PPU-0",
				ID:               "MIG-bbb",
				Profile:          "1g.10gb",
				Placements:       []device.AcceleratorPlacement{{Start: 1, Length: 1}},
				MemoryTotalBytes: ptrTo[uint64](10 << 30),
				MemoryUsedBytes:  ptrTo[uint64](0),
				CoresReason:      device.AcceleratorProcessReasonUnsupported,
			}},
		},
		{
			name:     "an allocation that recorded no identifier is answered as an absence",
			requests: []device.AcceleratorPartitionRequest{partitionRequest("", "1g.10gb", 0, 1)},
			source:   func() migPartitionSource { return addressableSource(carried) },
			want: []device.AcceleratorPartition{{
				DeviceID:     "PPU-0",
				Profile:      "1g.10gb",
				Placements:   []device.AcceleratorPlacement{{Start: 0, Length: 1}},
				MemoryReason: device.AcceleratorProcessReasonDriverError,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
			}},
		},
		{
			name:     "an identifier the card no longer carries is an absence, never a sibling's figure",
			requests: []device.AcceleratorPartitionRequest{partitionRequest("MIG-zzz", "1g.10gb", 0, 1)},
			source:   func() migPartitionSource { return addressableSource(carried) },
			want: []device.AcceleratorPartition{{
				DeviceID:     "PPU-0",
				Profile:      "1g.10gb",
				Placements:   []device.AcceleratorPlacement{{Start: 0, Length: 1}},
				MemoryReason: device.AcceleratorProcessReasonDriverError,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
			}},
		},
		{
			name:     "a card whose MIG mode is off holds no partition to read",
			requests: []device.AcceleratorPartitionRequest{partitionRequest("MIG-aaa", "1g.10gb", 0, 1)},
			source: func() migPartitionSource {
				src := addressableSource(carried)
				src.migEnabled = func() (bool, device.AcceleratorProcessReason) {
					return false, device.AcceleratorProcessReasonUnsupported
				}
				return src
			},
			want: []device.AcceleratorPartition{{
				DeviceID:     "PPU-0",
				Profile:      "1g.10gb",
				Placements:   []device.AcceleratorPlacement{{Start: 0, Length: 1}},
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
			}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, resolvePartitions(c.requests, c.source()))
		})
	}
}

// TestResolvePartitionsWalksOnceForManyPartitions pins the cost this design exists for: however many
// partitions of one card the node holds, each is one addressed read and nothing enumerates the
// library's profile catalog, which this manufacturer publishes in 85 ids.
func TestResolvePartitionsWalksOnceForManyPartitions(t *testing.T) {
	carried := map[string]migPartition{
		"MIG-aaa": {id: "MIG-aaa", totalBytes: 10 << 30, usedBytes: 1 << 30},
		"MIG-bbb": {id: "MIG-bbb", totalBytes: 10 << 30, usedBytes: 2 << 30},
		"MIG-ccc": {id: "MIG-ccc", totalBytes: 10 << 30, usedBytes: 3 << 30},
	}
	src := addressableSource(carried)
	reads := 0
	inner := src.partitionByID
	src.partitionByID = func(id string) (migPartition, device.AcceleratorProcessReason, bool) {
		reads++
		return inner(id)
	}

	got := resolvePartitions([]device.AcceleratorPartitionRequest{
		partitionRequest("MIG-aaa", "1g.10gb", 0, 1),
		partitionRequest("MIG-bbb", "1g.10gb", 1, 1),
		partitionRequest("MIG-ccc", "1g.10gb", 2, 1),
	}, src)

	require.Len(t, got, 3)
	assert.Equal(t, 3, reads, "one addressed read per partition, and nothing else")
	for i, want := range []uint64{1 << 30, 2 << 30, 3 << 30} {
		require.NotNil(t, got[i].MemoryUsedBytes)
		assert.Equal(t, want, *got[i].MemoryUsedBytes, "each partition reads its own handle")
	}
	assert.Equal(t, []string{"MIG-aaa", "MIG-bbb", "MIG-ccc"},
		[]string{got[0].ID, got[1].ID, got[2].ID})
}

// TestDetectorServesPartitions pins that the detector this package constructs is the one the device
// manager's partition pass will use — the pass reaches it by a type assertion, so a drifted
// signature would silently disable partition reporting rather than fail to build.
func TestDetectorServesPartitions(t *testing.T) {
	assert.Implements(t, (*device.AcceleratorPartitionDetector)(nil), New(device.DetectorOptions{}))
}

func ptrTo[T any](v T) *T { return &v }
