package nvidia

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/pkg/device"
)

func placement(start, length int32) []device.AcceleratorPlacement {
	return []device.AcceleratorPlacement{{Start: start, Length: length}}
}

// request is an allocation that recorded no partition identifier — everything one made before the
// field existed, and the shape the paths that never reach a driver are exercised with.
func request(profile string, start, length int32) device.AcceleratorPartitionRequest {
	return device.AcceleratorPartitionRequest{
		DeviceID:   "GPU-0",
		Profile:    profile,
		Placements: placement(start, length),
	}
}

// addressed is the same allocation with the identifier the device plugin recorded when the partition
// was created.
func addressed(id, profile string, start, length int32) device.AcceleratorPartitionRequest {
	req := request(profile, start, length)
	req.ID = id
	return req
}

// read is one partition's answer about itself, as its own MIG device handle reports it.
func read(id string, totalBytes, usedBytes uint64) migPartition {
	return migPartition{id: id, totalBytes: totalBytes, usedBytes: usedBytes}
}

// addressableSource is a partitioned card that answers for the partitions it carries and for no
// others, so a case states only what it changes.
func addressableSource(carried map[string]migPartition) migPartitionSource {
	return migPartitionSource{
		migEnabled: func() (bool, device.AcceleratorProcessReason) {
			return true, device.AcceleratorProcessReasonNone
		},
		partitionByID: func(id string) (migPartition, device.AcceleratorProcessReason, bool) {
			partition, ok := carried[id]
			if !ok {
				return migPartition{}, device.AcceleratorProcessReasonNone, false
			}
			return partition, device.AcceleratorProcessReasonNone, true
		},
	}
}

// TestResolvePartitions is the task's core: a partition is addressed by the identifier its
// allocation recorded, and every request is answered whether or not anything could be read from it.
func TestResolvePartitions(t *testing.T) {
	// Two 1g partitions of one card and one 3g, as the driver names them.
	carried := map[string]migPartition{
		"MIG-aaa": read("MIG-aaa", 10<<30, 0),
		"MIG-bbb": read("MIG-bbb", 10<<30, 6<<30),
		"MIG-ccc": read("MIG-ccc", 40<<30, 2<<30),
	}

	cases := []struct {
		name     string
		requests []device.AcceleratorPartitionRequest
		source   func() migPartitionSource
		want     []device.AcceleratorPartition
	}{
		{
			name:     "a busy partition names and sizes itself off its own handle",
			requests: []device.AcceleratorPartitionRequest{addressed("MIG-bbb", "1g.10gb", 1, 1)},
			source:   func() migPartitionSource { return addressableSource(carried) },
			want: []device.AcceleratorPartition{{
				DeviceID: "GPU-0", Profile: "1g.10gb", Placements: placement(1, 1),
				// Its own, not a sibling partition's and not the card's sum.
				ID:               "MIG-bbb",
				MemoryTotalBytes: ptr.To[uint64](10 << 30),
				MemoryUsedBytes:  ptr.To[uint64](6 << 30),
				CoresReason:      device.AcceleratorProcessReasonUnsupported,
			}},
		},
		{
			// The whole point of addressing rather than looking up by process: nothing runs here, and
			// the partition is still nameable, so its figure is a measured zero rather than an absence.
			name:     "an idle partition reports zero, not absent",
			requests: []device.AcceleratorPartitionRequest{addressed("MIG-aaa", "1g.10gb", 0, 1)},
			source:   func() migPartitionSource { return addressableSource(carried) },
			want: []device.AcceleratorPartition{{
				DeviceID: "GPU-0", Profile: "1g.10gb", Placements: placement(0, 1),
				ID:               "MIG-aaa",
				MemoryTotalBytes: ptr.To[uint64](10 << 30),
				MemoryUsedBytes:  ptr.To[uint64](0),
				CoresReason:      device.AcceleratorProcessReasonUnsupported,
			}},
		},
		{
			name: "two partitions of one card are each read on their own handle",
			requests: []device.AcceleratorPartitionRequest{
				addressed("MIG-bbb", "1g.10gb", 1, 1),
				addressed("MIG-ccc", "3g.40gb", 4, 4),
			},
			source: func() migPartitionSource { return addressableSource(carried) },
			want: []device.AcceleratorPartition{
				{
					DeviceID: "GPU-0", Profile: "1g.10gb", Placements: placement(1, 1),
					ID:               "MIG-bbb",
					MemoryTotalBytes: ptr.To[uint64](10 << 30),
					MemoryUsedBytes:  ptr.To[uint64](6 << 30),
					CoresReason:      device.AcceleratorProcessReasonUnsupported,
				},
				{
					DeviceID: "GPU-0", Profile: "3g.40gb", Placements: placement(4, 4),
					ID:               "MIG-ccc",
					MemoryTotalBytes: ptr.To[uint64](40 << 30),
					MemoryUsedBytes:  ptr.To[uint64](2 << 30),
					CoresReason:      device.AcceleratorProcessReasonUnsupported,
				},
			},
		},
		{
			name:     "an allocation that recorded no identifier is answered as an absence",
			requests: []device.AcceleratorPartitionRequest{request("1g.10gb", 0, 1)},
			source:   func() migPartitionSource { return addressableSource(carried) },
			want: []device.AcceleratorPartition{{
				DeviceID: "GPU-0", Profile: "1g.10gb", Placements: placement(0, 1),
				MemoryReason: device.AcceleratorProcessReasonDriverError,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
			}},
		},
		{
			name:     "an identifier the card no longer carries is an absence, never a sibling's figure",
			requests: []device.AcceleratorPartitionRequest{addressed("MIG-zzz", "1g.10gb", 0, 1)},
			source:   func() migPartitionSource { return addressableSource(carried) },
			want: []device.AcceleratorPartition{{
				DeviceID: "GPU-0", Profile: "1g.10gb", Placements: placement(0, 1),
				MemoryReason: device.AcceleratorProcessReasonDriverError,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
			}},
		},
		{
			name:     "a card whose MIG mode is off holds no partition to read",
			requests: []device.AcceleratorPartitionRequest{addressed("MIG-aaa", "1g.10gb", 0, 1)},
			source: func() migPartitionSource {
				src := addressableSource(carried)
				src.migEnabled = func() (bool, device.AcceleratorProcessReason) {
					return false, device.AcceleratorProcessReasonUnsupported
				}
				return src
			},
			want: []device.AcceleratorPartition{{
				DeviceID: "GPU-0", Profile: "1g.10gb", Placements: placement(0, 1),
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
			}},
		},
		{
			name:     "a card whose partitions could not be listed reports why",
			requests: []device.AcceleratorPartitionRequest{addressed("MIG-aaa", "1g.10gb", 0, 1)},
			source: func() migPartitionSource {
				src := addressableSource(carried)
				src.partitionByID = func(string) (migPartition, device.AcceleratorProcessReason, bool) {
					return migPartition{}, device.AcceleratorProcessReasonPermission, false
				}
				return src
			},
			want: []device.AcceleratorPartition{{
				DeviceID: "GPU-0", Profile: "1g.10gb", Placements: placement(0, 1),
				MemoryReason: device.AcceleratorProcessReasonPermission,
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

// TestResolvePartitionsReadsEachPartitionOnce pins the cost recording the identifier buys: each
// partition is one addressed read, and nothing walks the driver's profile catalog to translate a
// recorded profile name back into a profile id.
func TestResolvePartitionsReadsEachPartitionOnce(t *testing.T) {
	carried := map[string]migPartition{
		"MIG-aaa": read("MIG-aaa", 10<<30, 1<<30),
		"MIG-bbb": read("MIG-bbb", 10<<30, 2<<30),
		"MIG-ccc": read("MIG-ccc", 10<<30, 3<<30),
	}
	src := addressableSource(carried)
	reads := 0
	inner := src.partitionByID
	src.partitionByID = func(id string) (migPartition, device.AcceleratorProcessReason, bool) {
		reads++
		return inner(id)
	}

	got := resolvePartitions([]device.AcceleratorPartitionRequest{
		addressed("MIG-aaa", "1g.10gb", 0, 1),
		addressed("MIG-bbb", "1g.10gb", 1, 1),
		addressed("MIG-ccc", "1g.10gb", 2, 1),
	}, src)

	require.Len(t, got, 3)
	assert.Equal(t, 3, reads, "one addressed read per partition, and nothing else")
	for i, want := range []uint64{1 << 30, 2 << 30, 3 << 30} {
		require.NotNil(t, got[i].MemoryUsedBytes)
		assert.Equal(t, want, *got[i].MemoryUsedBytes, "each partition reads its own handle")
	}
	// Sibling partitions of one card differ only by their own identifiers, which is what keeps two
	// of them from collapsing into one entry on the surfaces that report them.
	assert.Equal(t, []string{"MIG-aaa", "MIG-bbb", "MIG-ccc"},
		[]string{got[0].ID, got[1].ID, got[2].ID})
}

// TestDetectorServesPartitions pins that the detector this package constructs is the one the device
// manager's partition pass will use — the pass reaches it by a type assertion, so a drifted
// signature would silently disable partition reporting rather than fail to build.
func TestDetectorServesPartitions(t *testing.T) {
	assert.Implements(t, (*device.AcceleratorPartitionDetector)(nil), New(device.DetectorOptions{}))
}

// TestMonitorAcceleratorPartitionsUnread pins the interface's promise — one answer per request — on
// the path where nothing can be read at all: no NVIDIA PCI device answers while an allocation still
// names a partition on one. It runs on any host, with or without a GPU.
func TestMonitorAcceleratorPartitionsUnread(t *testing.T) {
	detector, ok := New(device.DetectorOptions{}).(device.AcceleratorPartitionDetector)
	require.True(t, ok)

	requests := []device.AcceleratorPartitionRequest{
		request("1g.10gb", 0, 1),
		{DeviceID: "GPU-1", Profile: "3g.40gb", Placements: placement(4, 4)},
	}

	t.Run("no request costs no call", func(t *testing.T) {
		grp, err := detector.MonitorAcceleratorPartitions(false, nil)
		require.NoError(t, err)
		assert.Equal(t, Manufacturer, grp.Manufacturer)
		assert.Empty(t, grp.Partitions)
	})

	t.Run("no pci device answers for an allocated partition", func(t *testing.T) {
		grp, err := detector.MonitorAcceleratorPartitions(false, requests)
		require.NoError(t, err)
		assert.Equal(t, Manufacturer, grp.Manufacturer)
		require.Len(t, grp.Partitions, len(requests))
		for i := range grp.Partitions {
			assert.Equal(t, requests[i].DeviceID, grp.Partitions[i].DeviceID)
			assert.Equal(t, requests[i].Profile, grp.Partitions[i].Profile)
			assert.Empty(t, grp.Partitions[i].ID)
			assert.Nil(t, grp.Partitions[i].MemoryTotalBytes)
			assert.Nil(t, grp.Partitions[i].MemoryUsedBytes)
			// Transient, not unsupported: nothing here disproves that the driver serves the query.
			assert.Equal(t, device.AcceleratorProcessReasonDriverError, grp.Partitions[i].MemoryReason)
			assert.Equal(t, device.AcceleratorProcessReasonUnsupported, grp.Partitions[i].CoresReason)
		}
	})
}

// TestUnreadAcceleratorPartitions pins the shape of an answer nothing could be read into, since it is
// what a consumer's absent-versus-zero decision rests on for the paths that never reach a driver.
func TestUnreadAcceleratorPartitions(t *testing.T) {
	got := unreadAcceleratorPartitions(
		[]device.AcceleratorPartitionRequest{request("1g.10gb", 0, 1)},
		device.AcceleratorProcessReasonPermission)

	assert.Equal(t, []device.AcceleratorPartition{{
		DeviceID: "GPU-0", Profile: "1g.10gb", Placements: placement(0, 1),
		MemoryReason: device.AcceleratorProcessReasonPermission,
		CoresReason:  device.AcceleratorProcessReasonUnsupported,
	}}, got)
}
