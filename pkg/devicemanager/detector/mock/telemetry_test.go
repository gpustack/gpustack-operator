package mock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
)

func TestMonitorAcceleratorProcesses(t *testing.T) {
	specYAML := `
manufacturers:
  nvidia:
    groups:
      - name: NVIDIA H100 80GB HBM3
        memory: 81559
        accelerators:
          - id: GPU-0
            index: 0
          - id: GPU-1
            index: 1
          - id: GPU-2
            index: 2
            queryState: unqueryable
    processes:
      - id: GPU-0
        processes:
          - pid: 4100
            memoryBytes: 2147483648
            coresPercent: 40
          - pid: 4101
  amd:
    preset: amd-mi300x
    count: 1
    fault:
      totalFailure: "amd driver hung"
  ascend:
    preset: ascend-910b2
    count: 2
    cardOverrides:
      0:
        queryState: unqueryable
      1:
        queryState: unqueryable
  hygon:
    preset: hygon-k100-ai
    count: 2
    cardOverrides:
      1:
        queryState: fatalError
`
	p := writeSpecFile(t, t.TempDir(), specYAML)
	store, err := mockspec.NewStore(p)
	require.NoError(t, err)

	testCases := []struct {
		name         string
		manufacturer string
		deviceIDs    sets.Set[string]
		wantErr      string
		check        func(t *testing.T, grp device.AcceleratorProcessesGroup)
	}{
		{
			name:         "empty deviceIDs returns immediately without error",
			manufacturer: "amd",
			deviceIDs:    sets.New[string](),
			check: func(t *testing.T, grp device.AcceleratorProcessesGroup) {
				assert.Equal(t, "amd", grp.Manufacturer)
				assert.True(t, grp.Timestamp.IsZero())
				assert.Empty(t, grp.Accelerators)
			},
		},
		{
			name:         "returns configured rows idle empty rows and unread driver error rows",
			manufacturer: "nvidia",
			deviceIDs:    sets.New("GPU-0", "GPU-1", "GPU-2", "GPU-gone"),
			check: func(t *testing.T, grp device.AcceleratorProcessesGroup) {
				assert.Equal(t, "nvidia", grp.Manufacturer)
				assert.False(t, grp.Timestamp.IsZero())
				require.Len(t, grp.Accelerators, 4)

				byID := make(map[string]device.AcceleratorProcesses, len(grp.Accelerators))
				for _, a := range grp.Accelerators {
					byID[a.ID] = a
				}

				// GPU-0: configured rows including nil sentinel pointers.
				gpu0 := byID["GPU-0"]
				assert.Equal(t, device.AcceleratorProcessReasonNone, gpu0.MemoryReason)
				assert.Equal(t, device.AcceleratorProcessReasonNone, gpu0.CoresReason)
				require.Len(t, gpu0.Processes, 2)
				assert.Equal(t, uint32(4100), gpu0.Processes[0].PID)
				require.NotNil(t, gpu0.Processes[0].MemoryBytes)
				assert.Equal(t, uint64(2147483648), *gpu0.Processes[0].MemoryBytes)
				require.NotNil(t, gpu0.Processes[0].CoresPercent)
				assert.Equal(t, uint32(40), *gpu0.Processes[0].CoresPercent)
				assert.Nil(t, gpu0.Processes[1].MemoryBytes)
				assert.Nil(t, gpu0.Processes[1].CoresPercent)

				// GPU-1: healthy queryable card with no process entry reports measured idle.
				gpu1 := byID["GPU-1"]
				assert.Equal(t, device.AcceleratorProcessReasonNone, gpu1.MemoryReason)
				assert.Equal(t, device.AcceleratorProcessReasonNone, gpu1.CoresReason)
				assert.Empty(t, gpu1.Processes)

				// GPU-2: unqueryable sibling reports transient driver error.
				gpu2 := byID["GPU-2"]
				assert.Equal(t, device.AcceleratorProcessReasonDriverError, gpu2.MemoryReason)
				assert.Equal(t, device.AcceleratorProcessReasonDriverError, gpu2.CoresReason)

				// GPU-gone: absent device reports transient driver error.
				gone := byID["GPU-gone"]
				assert.Equal(t, device.AcceleratorProcessReasonDriverError, gone.MemoryReason)
				assert.Equal(t, device.AcceleratorProcessReasonDriverError, gone.CoresReason)
			},
		},
		{
			name:         "unconfigured manufacturer reports unread entries for requested devices",
			manufacturer: "thead",
			deviceIDs:    sets.New("PPU-0"),
			check: func(t *testing.T, grp device.AcceleratorProcessesGroup) {
				require.Len(t, grp.Accelerators, 1)
				assert.Equal(t, "PPU-0", grp.Accelerators[0].ID)
				assert.Equal(t, device.AcceleratorProcessReasonDriverError, grp.Accelerators[0].MemoryReason)
				assert.Equal(t, device.AcceleratorProcessReasonDriverError, grp.Accelerators[0].CoresReason)
			},
		},
		{
			name:         "manufacturer totalFailure propagates error",
			manufacturer: "amd",
			deviceIDs:    sets.New("GPU-0"),
			wantErr:      "amd driver hung",
		},
		{
			name:         "all slots unqueryable propagates error",
			manufacturer: "ascend",
			deviceIDs:    sets.New("NPU-0"),
			wantErr:      "unqueryable",
		},
		{
			name:         "slot fatalError propagates error",
			manufacturer: "hygon",
			deviceIDs:    sets.New("DCU-0"),
			wantErr:      "fatal",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDetector(tc.manufacturer, "", store)
			grp, err := d.MonitorAcceleratorProcesses(true, tc.deviceIDs)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			tc.check(t, grp)
		})
	}
}

func TestMonitorAcceleratorPartitions(t *testing.T) {
	specYAML := `
manufacturers:
  nvidia:
    preset: nvidia-h100-mig
    count: 3
    cardOverrides:
      0:
        id: GPU-NV-0
      1:
        id: GPU-NV-1
        queryState: unqueryable
      2:
        id: GPU-NV-2
        logicalSliced:
          count: 128
          coresPercentageOvercommit: true
    partitions:
      - deviceId: GPU-NV-0
        id: MIG-static-0
        profile: 1g.10gb
        placements:
          - start: 0
            length: 1
        memoryTotalBytes: 10334765056
        memoryUsedBytes: 1073741824
      - deviceId: GPU-NV-0
        profile: 1g.10gb
        placements:
          - start: 1
            length: 1
        memoryReason: permission
  hygon:
    preset: hygon-k100-ai-mig
    count: 2
    cardOverrides:
      0:
        id: GPU-HY-0
      1:
        id: GPU-HY-1
        queryState: unqueryable
    partitions:
      - deviceId: GPU-HY-0
        id: MIG-hygon-0
        profile: 2g.15gb
        placements:
          - start: 0
            length: 1
        memoryTotalBytes: 16106127360
        memoryUsedBytes: 4294967296
        coresPercent: 85
  amd:
    preset: amd-mi300x
    count: 1
    fault:
      countError: "rocm count failed"
`
	p := writeSpecFile(t, t.TempDir(), specYAML)
	store, err := mockspec.NewStore(p)
	require.NoError(t, err)

	liveProvider := PartitionProviderFunc(func(
		manufacturer string, req device.AcceleratorPartitionRequest,
	) (device.AcceleratorPartition, bool) {
		if manufacturer == "nvidia" && req.ID == "MIG-live-1" {
			return device.AcceleratorPartition{
				ID:               "MIG-live-1",
				MemoryTotalBytes: ptr.To[uint64](20669530112),
				MemoryUsedBytes:  ptr.To[uint64](0),
			}, true
		}
		return device.AcceleratorPartition{}, false
	})

	t.Run("empty requests returns immediately without error even on faulted manufacturer", func(t *testing.T) {
		d := NewDetector("amd", "", store)
		grp, err := d.MonitorAcceleratorPartitions(true, nil)
		require.NoError(t, err)
		assert.Equal(t, "amd", grp.Manufacturer)
		assert.True(t, grp.Timestamp.IsZero())
		assert.Empty(t, grp.Partitions)
	})

	t.Run("nvidia static spec live provider unqueryable non-mig and missing partition", func(t *testing.T) {
		var nilFunc PartitionProviderFunc
		_, ok := nilFunc.LookupPartition("nvidia", device.AcceleratorPartitionRequest{})
		assert.False(t, ok)

		d := NewDetector("nvidia", "", store, WithPartitionProvider(liveProvider))
		reqs := []device.AcceleratorPartitionRequest{
			{
				DeviceID:   "GPU-NV-0",
				ID:         "MIG-static-0",
				Profile:    "1g.10gb",
				Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
			},
			{
				DeviceID:   "GPU-NV-0",
				ID:         "MIG-live-1",
				Profile:    "2g.20gb",
				Placements: []device.AcceleratorPlacement{{Start: 2, Length: 2}},
			},
			{
				DeviceID:   "GPU-NV-0",
				ID:         "MIG-missing",
				Profile:    "1g.10gb",
				Placements: []device.AcceleratorPlacement{{Start: 4, Length: 1}},
			},
			{
				DeviceID:   "GPU-NV-1",
				ID:         "MIG-on-lost-card",
				Profile:    "1g.10gb",
				Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
			},
			{
				DeviceID:   "GPU-NV-2",
				ID:         "MIG-on-non-mig",
				Profile:    "1g.10gb",
				Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
			},
			{
				DeviceID:   "GPU-NV-0",
				ID:         "MIG-by-geometry",
				Profile:    "1g.10gb",
				Placements: []device.AcceleratorPlacement{{Start: 1, Length: 1}},
			},
		}

		grp, err := d.MonitorAcceleratorPartitions(true, reqs)
		require.NoError(t, err)
		assert.Equal(t, "nvidia", grp.Manufacturer)
		assert.False(t, grp.Timestamp.IsZero())
		require.Len(t, grp.Partitions, len(reqs))

		// Static entry.
		assert.Equal(t, "MIG-static-0", grp.Partitions[0].ID)
		assert.Equal(t, "1g.10gb", grp.Partitions[0].Profile)
		require.NotNil(t, grp.Partitions[0].MemoryTotalBytes)
		assert.Equal(t, uint64(10334765056), *grp.Partitions[0].MemoryTotalBytes)
		assert.Equal(t, device.AcceleratorProcessReasonNone, grp.Partitions[0].MemoryReason)
		assert.Equal(t, device.AcceleratorProcessReasonUnsupported, grp.Partitions[0].CoresReason)

		// Live provider entry echoes request geometry.
		assert.Equal(t, "MIG-live-1", grp.Partitions[1].ID)
		assert.Equal(t, "2g.20gb", grp.Partitions[1].Profile)
		assert.Equal(t, []device.AcceleratorPlacement{{Start: 2, Length: 2}}, grp.Partitions[1].Placements)
		require.NotNil(t, grp.Partitions[1].MemoryTotalBytes)
		assert.Equal(t, uint64(20669530112), *grp.Partitions[1].MemoryTotalBytes)
		assert.Equal(t, device.AcceleratorProcessReasonUnsupported, grp.Partitions[1].CoresReason)

		// Missing partition on MIG card reports transient driver error.
		assert.Empty(t, grp.Partitions[2].ID)
		assert.Equal(t, device.AcceleratorProcessReasonDriverError, grp.Partitions[2].MemoryReason)

		// Unqueryable parent card reports transient driver error.
		assert.Empty(t, grp.Partitions[3].ID)
		assert.Equal(t, device.AcceleratorProcessReasonDriverError, grp.Partitions[3].MemoryReason)

		// Non-MIG card reports unsupported.
		assert.Empty(t, grp.Partitions[4].ID)
		assert.Equal(t, device.AcceleratorProcessReasonUnsupported, grp.Partitions[4].MemoryReason)

		// Geometry-matched entry without static ID preserves explicit permission reason.
		assert.Empty(t, grp.Partitions[5].ID)
		assert.Equal(t, device.AcceleratorProcessReasonPermission, grp.Partitions[5].MemoryReason)
	})

	t.Run("hygon preserves per-partition coresPercent and uses transient driver error on unqueryable card", func(t *testing.T) {
		d := NewDetector("hygon", "", store)
		reqs := []device.AcceleratorPartitionRequest{
			{
				DeviceID:   "GPU-HY-0",
				ID:         "MIG-hygon-0",
				Profile:    "2g.15gb",
				Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
			},
			{
				DeviceID:   "GPU-HY-1",
				ID:         "MIG-hygon-lost",
				Profile:    "2g.15gb",
				Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
			},
		}

		grp, err := d.MonitorAcceleratorPartitions(true, reqs)
		require.NoError(t, err)
		require.Len(t, grp.Partitions, 2)

		require.NotNil(t, grp.Partitions[0].CoresPercent)
		assert.Equal(t, uint32(85), *grp.Partitions[0].CoresPercent)
		assert.Equal(t, device.AcceleratorProcessReasonNone, grp.Partitions[0].CoresReason)

		assert.Equal(t, device.AcceleratorProcessReasonDriverError, grp.Partitions[1].MemoryReason)
		assert.Equal(t, device.AcceleratorProcessReasonDriverError, grp.Partitions[1].CoresReason)
	})

	t.Run("manufacturer fault propagates error", func(t *testing.T) {
		d := NewDetector("amd", "", store)
		_, err := d.MonitorAcceleratorPartitions(true, []device.AcceleratorPartitionRequest{
			{DeviceID: "GPU-0", ID: "P-0", Profile: "1g", Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}}},
		})
		require.ErrorContains(t, err, "rocm count failed")
	})
}
