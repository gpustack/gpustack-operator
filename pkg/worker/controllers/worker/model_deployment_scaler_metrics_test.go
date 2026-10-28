package worker

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/pkg/worker/scaling/algorithm"
)

// TestModelDeploymentScalerHistogramDelta asserts a change over a window is read only when the
// two readings establish it.
//
// IT MATTERS BECAUSE A BUCKET THAT APPEARED HAS NO BASELINE, and the count it holds now is the
// total since the engine started rather than the count inside the window. A reader that treats
// the missing baseline as zero reads the lifetime total as the window.
func TestModelDeploymentScalerHistogramDelta(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		previous        modelDeploymentScalerHistogram
		current         modelDeploymentScalerHistogram
		wantEstablished bool
		wantCount       float64
		wantBucket      float64
	}{
		{
			name: "two readings of the same buckets establish the change",
			previous: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 40, 1: 50, math.Inf(1): 56},
				count:   56,
				sum:     20,
			},
			current: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 110, 1: 130, math.Inf(1): 140},
				count:   140,
				sum:     61,
			},
			wantEstablished: true,
			wantCount:       84,
			wantBucket:      80,
		},
		{
			name: "a bucket the previous reading did not carry has no baseline",
			previous: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{math.Inf(1): 1000},
				count:   1000,
				sum:     800,
			},
			current: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{1: 900, math.Inf(1): 1100},
				count:   1100,
				sum:     1800,
			},
			wantEstablished: false,
		},
		{
			name: "a bucket that appeared inside the window is refused",
			previous: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{math.Inf(1): 1000},
				count:   1000,
				sum:     800,
			},
			current: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{1: 50, math.Inf(1): 1100},
				count:   1100,
				sum:     1800,
			},
			wantEstablished: false,
		},
		{
			// The engine replaced one boundary with another, so the two readings
			// carry the same number of buckets and the shape of the window is not
			// what refuses it. Only the missing baseline refuses it.
			name: "a bucket the previous reading did not carry has no baseline, and the shape of the window is the same",
			previous: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 90, 1: 100, math.Inf(1): 100},
				count:   100,
				sum:     10,
			},
			current: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 90, 2: 50, math.Inf(1): 200},
				count:   200,
				sum:     110,
			},
			wantEstablished: false,
		},
		{
			name: "a bucket that holds more than the window is refused",
			previous: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 40, math.Inf(1): 100},
				count:   100,
			},
			current: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 940, math.Inf(1): 1000},
				count:   900,
			},
			wantEstablished: false,
		},
		{
			name: "a bucket that fell below the bucket below it is refused",
			previous: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.01: 1200, 0.05: 4800, math.Inf(1): 5428},
				count:   5428,
			},
			current: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.01: 1500, 0.05: 4800, math.Inf(1): 6000},
				count:   6000,
			},
			wantEstablished: false,
		},
		{
			name: "a bucket the current reading does not carry is refused",
			previous: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 40, math.Inf(1): 56},
				count:   56,
			},
			current: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{math.Inf(1): 140},
				count:   140,
			},
			wantEstablished: false,
		},
		{
			name: "a bucket that went down is refused",
			previous: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 40, math.Inf(1): 56},
				count:   56,
			},
			current: modelDeploymentScalerHistogram{
				buckets: map[float64]float64{0.5: 30, math.Inf(1): 140},
				count:   140,
			},
			wantEstablished: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			delta, established := modelDeploymentScalerHistogramDelta(testCase.previous, testCase.current)
			assert.Equal(t, testCase.wantEstablished, established)
			if !testCase.wantEstablished {
				return
			}
			assert.InDelta(t, testCase.wantCount, delta.count, 1e-9)
			assert.InDelta(t, testCase.wantBucket, delta.buckets[1], 1e-9)
		})
	}
}

// TestModelDeploymentScalerWindowRefusesAnUnprovenBucket asserts a window whose histogram
// changed shape is not a reading and that the window after it is.
//
// IT MATTERS BECAUSE A REFUSED WINDOW MUST NOT POISON THE NEXT ONE, because a reader that
// refused the window and then kept refusing would hold a role above its floor for ever.
func TestModelDeploymentScalerWindowRefusesAnUnprovenBucket(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	previous := modelDeploymentScalerPodSample{
		observedAt: start,
		ttft:       modelDeploymentScalerHistogram{buckets: map[float64]float64{math.Inf(1): 1000}, count: 1000, sum: 800},
		itl:        modelDeploymentScalerHistogram{buckets: map[float64]float64{math.Inf(1): 1000}, count: 1000, sum: 40},
	}
	current := modelDeploymentScalerPodSample{
		observedAt: start.Add(10 * time.Second),
		ttft:       modelDeploymentScalerHistogram{buckets: map[float64]float64{1: 900, math.Inf(1): 1100}, count: 1100, sum: 1800},
		itl:        modelDeploymentScalerHistogram{buckets: map[float64]float64{1: 900, math.Inf(1): 1100}, count: 1100, sum: 1040},
	}

	window := modelDeploymentScalerWindowOf(&previous, current, start.Add(10*time.Second), time.Minute)
	assert.Equal(t, algorithm.SignalMissing, window.state,
		"a window whose finite bucket has no baseline is not a reading")
	assert.Contains(t, window.reason, "latency histograms",
		"the reason says which reading failed")

	next := modelDeploymentScalerPodSample{
		observedAt: start.Add(20 * time.Second),
		ttft:       modelDeploymentScalerHistogram{buckets: map[float64]float64{1: 950, math.Inf(1): 1150}, count: 1150, sum: 1850},
		itl:        modelDeploymentScalerHistogram{buckets: map[float64]float64{1: 950, math.Inf(1): 1150}, count: 1150, sum: 1090},
	}
	recovered := modelDeploymentScalerWindowOf(&current, next, start.Add(20*time.Second), time.Minute)
	assert.Equal(t, algorithm.SignalFresh, recovered.state,
		"the window after a refused one reads from the sample taken then")
	assert.InDelta(t, 50.0, recovered.ttft.count, 1e-9,
		"the recovered window holds the observations of its own window")
}

// TestModelDeploymentScalerWindowRefusesARestart asserts a window that spans a restart is not a
// reading.
//
// IT MATTERS BECAUSE THE NUMBERS AFTER A RESTART BELONG TO ANOTHER LIFE of the engine, and
// reading the change as a rate reads a restart as a burst of work that never happened.
func TestModelDeploymentScalerWindowRefusesARestart(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	previous := modelDeploymentScalerPodSample{
		observedAt:   start,
		restartCount: 1,
		ttft:         modelDeploymentScalerHistogram{buckets: map[float64]float64{1: 50, math.Inf(1): 56}, count: 56},
		itl:          modelDeploymentScalerHistogram{buckets: map[float64]float64{0.05: 40, math.Inf(1): 56}, count: 56},
	}
	current := modelDeploymentScalerPodSample{
		observedAt:   start.Add(10 * time.Second),
		restartCount: 2,
		ttft:         modelDeploymentScalerHistogram{buckets: map[float64]float64{1: 130, math.Inf(1): 140}, count: 140},
		itl:          modelDeploymentScalerHistogram{buckets: map[float64]float64{0.05: 100, math.Inf(1): 140}, count: 140},
	}

	window := modelDeploymentScalerWindowOf(&previous, current, start.Add(10*time.Second), time.Minute)
	assert.Equal(t, algorithm.SignalMissing, window.state,
		"a window that spans a restart is not a reading")
	assert.Contains(t, window.reason, "restarted")
}
