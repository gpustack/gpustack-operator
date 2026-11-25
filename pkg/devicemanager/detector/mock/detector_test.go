package mock

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func writeSpecFile(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "mock-devices.yaml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func updateSpecFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(content), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

func TestDetectAndMonitorMultiVendor(t *testing.T) {
	specYAML := `
default:
  manufacturers:
    nvidia:
      preset: nvidia-h100
      count: 2
      cardOverrides:
        1:
          unhealthy: true
          metrics:
            memoryUsage: 40779
            coresUtilization: 75
            temperature: 68
            powerUsage: 520
    amd:
      preset: amd-mi300x
      count: 2
nodes:
  worker-mig:
    manufacturers:
      nvidia:
        preset: nvidia-h100-mig
        count: 1
      hygon:
        preset: hygon-k100-ai-mig
        count: 1
  worker-mixed:
    manufacturers:
      nvidia:
        groups:
          - name: NVIDIA H100 80GB HBM3
            memory: 81559
            cores: 132
            accelerators:
              - id: GPU-H100-0
                index: 0
                logicalSliced:
                  count: 128
                  coresPercentageOvercommit: true
          - name: NVIDIA GeForce RTX 4090
            memory: 49140
            cores: 128
            accelerators:
              - id: GPU-4090-1
                index: 1
                logicalSliced:
                  count: 128
                  coresPercentageOvercommit: true
`
	specPath := writeSpecFile(t, t.TempDir(), specYAML)
	store, err := mockspec.NewStore(specPath)
	require.NoError(t, err)

	testCases := []struct {
		name               string
		manufacturer       string
		nodeName           string
		noPciCheck         bool
		wantGroups         int
		wantTotalAccels    int
		wantSecondUnhealth bool
		wantPhysicalSliced bool
	}{
		{
			name:               "default node nvidia preset with card override and noPciCheck false",
			manufacturer:       nodefeature.ManufacturerNVIDIA,
			nodeName:           "worker-unknown",
			noPciCheck:         false,
			wantGroups:         1,
			wantTotalAccels:    2,
			wantSecondUnhealth: true,
		},
		{
			name:            "default node amd preset with noPciCheck true",
			manufacturer:    nodefeature.ManufacturerAMD,
			nodeName:        "",
			noPciCheck:      true,
			wantGroups:      1,
			wantTotalAccels: 2,
		},
		{
			name:            "unconfigured manufacturer on default node returns empty without error",
			manufacturer:    nodefeature.ManufacturerAscend,
			nodeName:        "worker-unknown",
			wantGroups:      0,
			wantTotalAccels: 0,
		},
		{
			name:               "node-specific override nvidia mig preset",
			manufacturer:       nodefeature.ManufacturerNVIDIA,
			nodeName:           "worker-mig",
			wantGroups:         1,
			wantTotalAccels:    1,
			wantPhysicalSliced: true,
		},
		{
			name:               "node-specific override hygon mig preset",
			manufacturer:       nodefeature.ManufacturerHygon,
			nodeName:           "worker-mig",
			wantGroups:         1,
			wantTotalAccels:    1,
			wantPhysicalSliced: true,
		},
		{
			name:            "multi-group single manufacturer",
			manufacturer:    nodefeature.ManufacturerNVIDIA,
			nodeName:        "worker-mixed",
			wantGroups:      2,
			wantTotalAccels: 2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDetector(tc.manufacturer, tc.nodeName, store, WithLogger(klog.Background()))
			assert.Equal(t, tc.manufacturer, d.Name())

			groups, err := d.DetectAccelerator(tc.noPciCheck)
			require.NoError(t, err)
			assert.Len(t, groups, tc.wantGroups)

			metrics, err := d.MonitorAccelerator(tc.noPciCheck)
			require.NoError(t, err)

			if tc.wantGroups == 0 {
				assert.Empty(t, groups)
				assert.Empty(t, metrics)
				return
			}

			totalAccels := 0
			for _, g := range groups {
				assert.Equal(t, tc.manufacturer, g.Manufacturer)
				totalAccels += len(g.Accelerators)
				if tc.wantPhysicalSliced {
					assert.Greater(t, g.AcceleratorSlicedDetail.Physical.Count, int32(0))
					assert.NotEmpty(t, g.AcceleratorSlicedDetail.Physical.Profiles)
				} else {
					assert.Greater(t, g.AcceleratorSlicedDetail.Logical.Count, int32(0))
				}
			}
			assert.Equal(t, tc.wantTotalAccels, totalAccels)

			require.Len(t, metrics, 1)
			assert.Equal(t, tc.manufacturer, metrics[0].Manufacturer)
			assert.False(t, metrics[0].Timestamp.IsZero())
			require.Len(t, metrics[0].Accelerators, tc.wantTotalAccels)

			if tc.wantSecondUnhealth {
				assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
				assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)
				assert.False(t, metrics[0].Accelerators[0].Unhealthy)
				assert.True(t, metrics[0].Accelerators[1].Unhealthy)
				assert.Equal(t, uint64(40779), metrics[0].Accelerators[1].MemoryUsage)
				assert.Equal(t, uint32(75), metrics[0].Accelerators[1].CoresUtilization)
				assert.Equal(t, uint32(68), metrics[0].Accelerators[1].Temperature)
				assert.Equal(t, uint32(520), metrics[0].Accelerators[1].PowerUsage)
			}

			// Mutating caller-owned slice must not corrupt subsequent reads.
			groups[0].Accelerators[0].ID = "mutated"
			again, err := d.DetectAccelerator(tc.noPciCheck)
			require.NoError(t, err)
			assert.NotEqual(t, "mutated", again[0].Accelerators[0].ID)
		})
	}

	t.Run("NewDetectors builds ordered detectors and nil store returns error", func(t *testing.T) {
		list := NewDetectors([]string{"nvidia", "amd"}, "worker-unknown", store, nil)
		require.Len(t, list, 2)
		assert.Equal(t, "nvidia", list[0].Name())
		assert.Equal(t, "amd", list[1].Name())

		nilDet := NewDetector("nvidia", "", nil)
		_, err := nilDet.DetectAccelerator(true)
		require.Error(t, err)
		_, err = nilDet.MonitorAccelerator(true)
		require.Error(t, err)

		var emptyStore mockspec.Store
		brokenDet := NewDetector("nvidia", "", &emptyStore)
		_, err = brokenDet.DetectAccelerator(true)
		require.Error(t, err)
		_, err = brokenDet.MonitorAccelerator(true)
		require.Error(t, err)
	})
}

func TestManufacturerFaultsAndInitError(t *testing.T) {
	testCases := []struct {
		name     string
		runSteps func(t *testing.T, dir string)
	}{
		{
			name: "persistent initError fails every detect and monitor pass",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
preset: nvidia-h100
count: 2
fault:
  initError:
    enabled: true
    failCount: 0
    message: "nvml library load error"
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("nvidia", "", store)

				for range 3 {
					groups, err := d.DetectAccelerator(true)
					require.ErrorContains(t, err, "nvml library load error")
					assert.Nil(t, groups)

					metrics, err := d.MonitorAccelerator(true)
					require.ErrorContains(t, err, "nvml library load error")
					assert.Nil(t, metrics)
				}
			},
		},
		{
			name: "transient initError failCount succeeds after N calls and resets on revision change",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
preset: nvidia-h100
count: 1
fault:
  initError:
    enabled: true
    failCount: 2
    message: "driver warming up"
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("nvidia", "", store)

				_, err = d.DetectAccelerator(true)
				require.ErrorContains(t, err, "driver warming up")

				_, err = d.MonitorAccelerator(true)
				require.ErrorContains(t, err, "driver warming up")

				// Third call on revision 1 succeeds.
				groups, err := d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups, 1)
				require.Len(t, groups[0].Accelerators, 1)

				metrics, err := d.MonitorAccelerator(true)
				require.NoError(t, err)
				require.Len(t, metrics, 1)

				// Malformed hot-reload keeps revision 1 and must not reset failCount.
				updateSpecFile(t, p, `invalid: [yaml`)
				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups, 1)

				// Valid hot-reload bumps revision to 2 and resets transient failCount.
				updateSpecFile(t, p, `
preset: nvidia-h100
count: 2
fault:
  initError:
    enabled: true
    failCount: 1
    message: "driver reinit"
`)
				_, err = d.DetectAccelerator(true)
				require.ErrorContains(t, err, "driver reinit")

				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups, 1)
				require.Len(t, groups[0].Accelerators, 2)
			},
		},
		{
			name: "countError and totalFailure return errors without clearing cached cards",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
preset: amd-mi300x
count: 2
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("amd", "", store)

				groups, err := d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 2)

				// Inject countError (boolean form with default message).
				updateSpecFile(t, p, `
preset: amd-mi300x
count: 2
fault:
  countError: true
`)
				_, err = d.DetectAccelerator(true)
				require.Error(t, err)
				_, err = d.MonitorAccelerator(true)
				require.Error(t, err)

				// Inject totalFailure with custom message.
				updateSpecFile(t, p, `
preset: amd-mi300x
count: 2
fault:
  totalFailure: "bus reset in progress"
`)
				_, err = d.DetectAccelerator(true)
				require.ErrorContains(t, err, "bus reset in progress")
				_, err = d.MonitorAccelerator(true)
				require.ErrorContains(t, err, "bus reset in progress")

				// Recover with card 1 unqueryable: cache from pass 1 is still intact.
				updateSpecFile(t, p, `
preset: amd-mi300x
count: 2
cardOverrides:
  1:
    queryState: unqueryable
`)
				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 2)
				assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
				assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)
			},
		},
		{
			name: "per-slot fatalError fails entire pass without mutating cache",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
preset: nvidia-h100
count: 2
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("nvidia", "", store)

				groups, err := d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 2)

				updateSpecFile(t, p, `
preset: nvidia-h100
count: 2
cardOverrides:
  1:
    queryState: fatalError
`)
				_, err = d.DetectAccelerator(true)
				require.ErrorContains(t, err, "fatal")
				_, err = d.MonitorAccelerator(true)
				require.ErrorContains(t, err, "fatal")

				// Cache must still hold slot 1 if slot 1 transitions to unqueryable.
				updateSpecFile(t, p, `
preset: nvidia-h100
count: 2
cardOverrides:
  1:
    queryState: unqueryable
`)
				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 2)
				assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.runSteps(t, t.TempDir())
		})
	}
}

func TestUnqueryableRetentionRecoveryAndShrink(t *testing.T) {
	testCases := []struct {
		name     string
		runSteps func(t *testing.T, dir string)
	}{
		{
			name: "initial unqueryable card skipped while preserving sibling index",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
preset: nvidia-h100
count: 3
cardOverrides:
  1:
    queryState: unqueryable
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("nvidia", "", store)

				groups, err := d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups, 1)
				require.Len(t, groups[0].Accelerators, 2)
				assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
				assert.Equal(t, uint32(2), groups[0].Accelerators[1].Index)

				metrics, err := d.MonitorAccelerator(true)
				require.NoError(t, err)
				require.Len(t, metrics, 1)
				require.Len(t, metrics[0].Accelerators, 2)
				assert.Equal(t, groups[0].Accelerators[0].ID, metrics[0].Accelerators[0].ID)
				assert.Equal(t, groups[0].Accelerators[1].ID, metrics[0].Accelerators[1].ID)
			},
		},
		{
			name: "all cards unqueryable on first pass returns error",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
preset: nvidia-h100
count: 2
cardOverrides:
  0:
    queryState: unqueryable
  1:
    queryState: unqueryable
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("nvidia", "", store)

				groups, err := d.DetectAccelerator(true)
				require.Error(t, err)
				assert.Nil(t, groups)

				metrics, err := d.MonitorAccelerator(true)
				require.Error(t, err)
				assert.Nil(t, metrics)
			},
		},
		{
			name: "known card retained as unhealthy across monitor and detect passes then recovers",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
preset: nvidia-h100
count: 3
cardOverrides:
  1:
    metrics:
      memoryUsage: 20000
      coresUtilization: 50
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("nvidia", "", store)

				// Pass 1: Detect seeds both detect and monitor caches.
				groups, err := d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 3)
				card1ID := groups[0].Accelerators[1].ID
				card1Mem := groups[0].Memory
				for i := range 3 {
					assert.Equal(t, uint32(i), groups[0].Accelerators[i].Index)
					assert.False(t, groups[0].Accelerators[i].Status.Unhealthy)
				}

				// Pass 2 & 3: card 1 becomes unqueryable; Monitor runs first before ever succeeding live.
				updateSpecFile(t, p, `
preset: nvidia-h100
count: 3
cardOverrides:
  1:
    queryState: unqueryable
    metrics:
      memoryUsage: 20000
      coresUtilization: 50
`)
				for pass := 2; pass <= 3; pass++ {
					metrics, err := d.MonitorAccelerator(true)
					require.NoError(t, err, "pass %d monitor", pass)
					require.Len(t, metrics, 1)
					require.Len(t, metrics[0].Accelerators, 3)
					assert.False(t, metrics[0].Accelerators[0].Unhealthy)
					assert.Equal(t, card1ID, metrics[0].Accelerators[1].ID)
					assert.Equal(t, card1Mem, metrics[0].Accelerators[1].Memory)
					assert.Equal(t, uint64(0), metrics[0].Accelerators[1].MemoryUsage)
					assert.True(t, metrics[0].Accelerators[1].Unhealthy)
					assert.False(t, metrics[0].Accelerators[2].Unhealthy)

					groups, err = d.DetectAccelerator(true)
					require.NoError(t, err, "pass %d detect", pass)
					require.Len(t, groups, 1)
					require.Len(t, groups[0].Accelerators, 3)
					assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
					assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
					assert.Equal(t, card1ID, groups[0].Accelerators[1].ID)
					assert.Equal(t, uint32(1), groups[0].Accelerators[1].Index)
					assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)
					assert.Equal(t, uint32(2), groups[0].Accelerators[2].Index)
					assert.False(t, groups[0].Accelerators[2].Status.Unhealthy)
				}

				// Pass 4: card 1 recovers to queryState ok; unhealthy clears automatically.
				updateSpecFile(t, p, `
preset: nvidia-h100
count: 3
cardOverrides:
  1:
    queryState: ok
    metrics:
      memoryUsage: 10240
      coresUtilization: 30
`)
				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 3)
				for i := range 3 {
					assert.False(t, groups[0].Accelerators[i].Status.Unhealthy)
				}

				metrics, err := d.MonitorAccelerator(true)
				require.NoError(t, err)
				require.Len(t, metrics[0].Accelerators, 3)
				assert.False(t, metrics[0].Accelerators[1].Unhealthy)
				assert.Equal(t, uint64(10240), metrics[0].Accelerators[1].MemoryUsage)
				assert.Equal(t, uint32(30), metrics[0].Accelerators[1].CoresUtilization)
			},
		},
		{
			name: "multi-group retention reconstructs cached group when sole card in second group is unqueryable",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100 80GB HBM3
    memory: 81559
    accelerators:
      - id: GPU-0
        index: 0
  - manufacturer: nvidia
    name: NVIDIA GeForce RTX 4090
    memory: 49140
    accelerators:
      - id: GPU-1
        index: 1
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("nvidia", "", store)

				groups, err := d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups, 2)

				updateSpecFile(t, p, `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100 80GB HBM3
    memory: 81559
    accelerators:
      - id: GPU-0
        index: 0
  - manufacturer: nvidia
    name: NVIDIA GeForce RTX 4090
    memory: 49140
    accelerators:
      - id: GPU-1
        index: 1
        queryState: unqueryable
`)
				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups, 2)
				assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
				assert.Equal(t, "GPU-1", groups[1].Accelerators[0].ID)
				assert.True(t, groups[1].Accelerators[0].Status.Unhealthy)
			},
		},
		{
			name: "all cards unqueryable after cache returns error without wiping cache",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
preset: nvidia-h100
count: 2
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("nvidia", "", store)

				_, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				_, err = d.MonitorAccelerator(true)
				require.NoError(t, err)

				updateSpecFile(t, p, `
preset: nvidia-h100
count: 2
cardOverrides:
  0:
    queryState: unqueryable
  1:
    queryState: unqueryable
`)
				groups, err := d.DetectAccelerator(true)
				require.Error(t, err)
				assert.Nil(t, groups)

				metrics, err := d.MonitorAccelerator(true)
				require.Error(t, err)
				assert.Nil(t, metrics)

				// Partial recovery: card 0 recovers while card 1 stays unqueryable and retained.
				updateSpecFile(t, p, `
preset: nvidia-h100
count: 2
cardOverrides:
  1:
    queryState: unqueryable
`)
				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 2)
				assert.False(t, groups[0].Accelerators[0].Status.Unhealthy)
				assert.True(t, groups[0].Accelerators[1].Status.Unhealthy)
			},
		},
		{
			name: "clean shrink prunes tail removed middle index and zero count",
			runSteps: func(t *testing.T, dir string) {
				p := writeSpecFile(t, dir, `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100 80GB HBM3
    memory: 81559
    accelerators:
      - id: GPU-0
        index: 0
      - id: GPU-1
        index: 1
      - id: GPU-2
        index: 2
`)
				store, err := mockspec.NewStore(p)
				require.NoError(t, err)
				d := NewDetector("nvidia", "", store)

				groups, err := d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 3)

				// Middle index 1 removed cleanly; Monitor runs first to verify monitor-side pruning too.
				updateSpecFile(t, p, `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100 80GB HBM3
    memory: 81559
    accelerators:
      - id: GPU-0
        index: 0
      - id: GPU-2
        index: 2
`)
				metrics, err := d.MonitorAccelerator(true)
				require.NoError(t, err)
				require.Len(t, metrics[0].Accelerators, 2)

				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 2)

				// Re-introducing index 1 as unqueryable must NOT resurrect pruned GPU-1.
				updateSpecFile(t, p, `
groups:
  - manufacturer: nvidia
    name: NVIDIA H100 80GB HBM3
    memory: 81559
    accelerators:
      - id: GPU-0
        index: 0
      - id: GPU-1-NEW
        index: 1
        queryState: unqueryable
      - id: GPU-2
        index: 2
`)
				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 2)
				assert.Equal(t, "GPU-0", groups[0].Accelerators[0].ID)
				assert.Equal(t, "GPU-2", groups[0].Accelerators[1].ID)

				// Shrink to zero count clears remaining cache.
				updateSpecFile(t, p, `
preset: nvidia-h100
count: 0
`)
				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				assert.Nil(t, groups)

				metrics, err = d.MonitorAccelerator(true)
				require.NoError(t, err)
				assert.Nil(t, metrics)

				// Re-adding 2 cards with slot 1 unqueryable must not resurrect anything.
				updateSpecFile(t, p, `
preset: nvidia-h100
count: 2
cardOverrides:
  1:
    queryState: unqueryable
`)
				groups, err = d.DetectAccelerator(true)
				require.NoError(t, err)
				require.Len(t, groups[0].Accelerators, 1)
				assert.Equal(t, uint32(0), groups[0].Accelerators[0].Index)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.runSteps(t, t.TempDir())
		})
	}
}

func TestConcurrentDetectMonitorAndHotReloadRace(t *testing.T) {
	dir := t.TempDir()
	specVariants := []string{
		`
preset: nvidia-h100-mig
count: 3
cardOverrides:
  0:
    id: GPU-RACE-0
processes:
  - id: GPU-RACE-0
    processes:
      - pid: 1001
        memoryBytes: 1073741824
        coresPercent: 25
partitions:
  - deviceId: GPU-RACE-0
    id: MIG-1111
    profile: 1g.10gb
    placements:
      - start: 0
        length: 1
    memoryTotalBytes: 10334765056
    memoryUsedBytes: 2147483648
`,
		`
preset: nvidia-h100-mig
count: 3
cardOverrides:
  1:
    queryState: unqueryable
`,
		`
preset: nvidia-h100-mig
count: 2
`,
		`
preset: nvidia-h100-mig
count: 2
fault:
  initError:
    enabled: true
    failCount: 1
`,
	}

	p := writeSpecFile(t, dir, specVariants[0])
	store, err := mockspec.NewStore(p)
	require.NoError(t, err)

	d := NewDetector("nvidia", "", store)
	reqSet := sets.New("GPU-RACE-0", "GPU-missing")
	partReqs := []device.AcceleratorPartitionRequest{
		{
			DeviceID:   "GPU-RACE-0",
			ID:         "MIG-1111",
			Profile:    "1g.10gb",
			Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
		},
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = d.DetectAccelerator(true)
					_, _ = d.MonitorAccelerator(true)
					_, _ = d.MonitorAcceleratorProcesses(true, reqSet)
					_, _ = d.MonitorAcceleratorPartitions(true, partReqs)
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 24 {
			updateSpecFile(t, p, specVariants[i%len(specVariants)])
			time.Sleep(2 * time.Millisecond)
		}
		close(stop)
	}()

	wg.Wait()
}
