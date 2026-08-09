package detector

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
