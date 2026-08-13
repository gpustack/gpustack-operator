package detector

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

// fakeDetector is a device.Detector stub whose monitor result carries a monotonically
// increasing marker, so tests can tell one tick's sample from the next.
type fakeDetector struct {
	name        string
	monitorTick atomic.Uint64
}

func (f *fakeDetector) Name() string {
	return f.name
}

func (f *fakeDetector) DetectAccelerator(bool) (device.DevicesGroupList, error) {
	return device.DevicesGroupList{
		{
			Manufacturer: f.name,
			ID:           "group-0",
			Accelerators: []device.Accelerator{
				{ID: "dev-0"},
			},
		},
	}, nil
}

func (f *fakeDetector) MonitorAccelerator(bool) (device.MetricsGroupList, error) {
	tick := f.monitorTick.Add(1)
	return device.MetricsGroupList{
		{
			Manufacturer: f.name,
			Timestamp:    time.Unix(int64(tick), 0),
			Accelerators: []device.AcceleratorMetrics{
				{ID: "dev-0", MemoryUsage: tick},
			},
		},
	}, nil
}

// TestMonitorSnapshotStoredPerTick pins that each monitor tick replaces the snapshot with the
// latest sample: the stored value must advance past the first tick (old value replaced) and carry
// a store timestamp for staleness checks.
func TestMonitorSnapshotStoredPerTick(t *testing.T) {
	det := &Detector{
		detectors:     []device.Detector{&fakeDetector{name: "fake"}},
		monitorPeriod: 10 * time.Millisecond,
	}
	assert.Nil(t, det.MonitorSnapshot(), "no snapshot before the first tick")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// NB: reporting devices fails without KUBERNETES_NODE_NAME and a loopback client,
		// which the detector logs and tolerates — the monitor loop still runs.
		_ = det.Start(ctx)
	}()

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		s := det.MonitorSnapshot()
		if !assert.NotNil(c, s, "snapshot stored after monitor ticks") {
			return
		}
		assert.False(c, s.Timestamp.IsZero(), "snapshot carries its store time")
		require.Len(c, s.Groups, 1)
		require.Len(c, s.Groups[0].Accelerators, 1)
		assert.GreaterOrEqual(c, s.Groups[0].Accelerators[0].MemoryUsage, uint64(2),
			"the first tick's sample must be replaced by a later one")
	}, 5*time.Second, 5*time.Millisecond)
}

// allocationFixture is an Instance's allocation of one nvidia card. Its index is deliberately
// not its position in the snapshot below, so a result reading the position instead of the
// allocation is a failure rather than a coincidence.
func allocationFixture() []workercore.DevicesAllocationGroup {
	return []workercore.DevicesAllocationGroup{{
		ID:           "grp",
		Manufacturer: "nvidia",
		Accelerators: []workercore.AcceleratorAllocation{{ID: "gpu-uuid-1", Index: 3}},
	}}
}

// snapshotFixture is a snapshot stored the given time ago, carrying the allocated card, a card
// of the same manufacturer allocated to someone else, and a whole group of another vendor.
func snapshotFixture(age time.Duration, periodSeconds int64) *MonitorSnapshot {
	return &MonitorSnapshot{
		Timestamp:     time.Now().Add(-age),
		PeriodSeconds: periodSeconds,
		Groups: device.MetricsGroupList{
			{
				Manufacturer: "nvidia",
				Accelerators: []device.AcceleratorMetrics{
					{ID: "gpu-uuid-1", Memory: 81920, MemoryUsage: 1024},
					{ID: "gpu-uuid-someone-else", Memory: 81920},
				},
			},
			{
				Manufacturer: "amd",
				// Another vendor happening to name a card the same must not be mistaken for
				// this allocation's.
				Accelerators: []device.AcceleratorMetrics{{ID: "gpu-uuid-1", Memory: 32768}},
			},
		},
	}
}

func TestMonitorSnapshotFresh(t *testing.T) {
	testCases := []struct {
		name string

		snapshot *MonitorSnapshot

		want bool
	}{
		{
			name:     "a snapshot just stored",
			snapshot: snapshotFixture(0, 15),
			want:     true,
		},
		{
			// The loop only replaces the snapshot after a successful non-empty sample, so an
			// old one is the last thing that worked rather than the current state.
			name:     "a snapshot older than three periods",
			snapshot: snapshotFixture(46*time.Second, 15),
		},
		{
			name:     "a snapshot within three periods",
			snapshot: snapshotFixture(44*time.Second, 15),
			want:     true,
		},
		{
			// A slower cadence must not be mistaken for a failure.
			name:     "the bound scales with the reported period",
			snapshot: snapshotFixture(50*time.Second, 60),
			want:     true,
		},
		{
			// An older device manager reports no period; 45s is three default ones.
			name:     "a snapshot reporting no period falls back to the default bound",
			snapshot: snapshotFixture(44*time.Second, 0),
			want:     true,
		},
		{
			name:     "a snapshot reporting no period, past the fallback bound",
			snapshot: snapshotFixture(46*time.Second, 0),
		},
		{
			name: "nothing stored yet",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, MonitorSnapshotFresh(tc.snapshot))
		})
	}
}

func TestAllocatedAcceleratorMetricsOf(t *testing.T) {
	testCases := []struct {
		name string

		snapshot    *MonitorSnapshot
		allocGroups []workercore.DevicesAllocationGroup

		wantIDs          []string
		wantManufacturer string
		wantIndex        uint32
	}{
		{
			name:             "keeps the allocated card and nothing else",
			snapshot:         snapshotFixture(0, 15),
			allocGroups:      allocationFixture(),
			wantIDs:          []string{"gpu-uuid-1"},
			wantManufacturer: "nvidia",
			wantIndex:        3,
		},
		{
			name:        "yields nothing from a stale snapshot",
			snapshot:    snapshotFixture(time.Minute, 15),
			allocGroups: allocationFixture(),
		},
		{
			name:        "yields nothing before the first sample",
			allocGroups: allocationFixture(),
		},
		{
			name:     "yields nothing for an instance holding no card",
			snapshot: snapshotFixture(0, 15),
		},
		{
			name:     "yields nothing for a manufacturer this snapshot does not carry",
			snapshot: snapshotFixture(0, 15),
			allocGroups: []workercore.DevicesAllocationGroup{{
				Manufacturer: "ascend",
				Accelerators: []workercore.AcceleratorAllocation{{ID: "gpu-uuid-1"}},
			}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := AllocatedAcceleratorMetricsOf(tc.snapshot, tc.allocGroups)

			require.Len(t, got, len(tc.wantIDs))
			for i := range tc.wantIDs {
				assert.Equal(t, tc.wantIDs[i], got[i].Metrics.ID)
				assert.Equal(t, tc.wantManufacturer, got[i].Manufacturer,
					"the manufacturer travels with the metrics, to label them by")
				assert.Equal(t, tc.wantIndex, got[i].Index,
					"the index comes from the allocation, which is the only side that carries one")
			}
		})
	}
}
