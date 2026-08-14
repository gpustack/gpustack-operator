package detector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/procattr"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

const (
	testManufacturer = "nvidia"
	testDeviceID     = "GPU-0"
	testPodAUID      = "01234567-89ab-cdef-0123-456789abcdef"
	testPodBUID      = "11111111-2222-3333-4444-555555555555"
	testPodCUID      = "99999999-8888-7777-6666-555555555555"
	testCtrAID       = "deadbeef1234"
	testCtrBID       = "cafebabe5678"
	testCtrName      = "main"
	testCardMemory   = 16 << 10 // 16 GiB in MiB.
)

// testContainerNames is the container-name join every aggregation case runs against: two Instance
// Pods, each with one container. Attribution itself is stubbed per case, so the index it consults is
// not needed here — only the join from a resolved container to the name a record is keyed by.
func testContainerNames() containerNames {
	return containerNames{
		testPodAUID: {testCtrAID: testCtrName},
		testPodBUID: {testCtrBID: testCtrName},
	}
}

func attributed(podUID, containerID string) procattr.Result {
	return procattr.Result{
		Outcome:  procattr.OutcomeAttributed,
		Identity: procattr.Identity{PodUID: podUID, ContainerID: containerID},
	}
}

// memoryRow carries memory and NO compute figure, which the interface defines as compute this
// hardware could not measure — the AMD and Hygon sentinel case.
func memoryRow(pid uint32, bytes uint64) device.AcceleratorProcess {
	return device.AcceleratorProcess{PID: pid, MemoryBytes: ptr.To(bytes)}
}

// idleRow carries memory and a compute figure of zero, which is what an adapter emits for a process
// its library measured and found idle. It is the ordinary row of a working NVML-style backend.
func idleRow(pid uint32, bytes uint64) device.AcceleratorProcess {
	return device.AcceleratorProcess{
		PID: pid, MemoryBytes: ptr.To(bytes), CoresPercent: ptr.To[uint32](0),
	}
}

// TestAggregateAcceleratorProcesses pins the absent-versus-zero rules of one device's sample: a
// figure the manufacturer served is published even when it is zero, a figure it could not serve is
// absent, and a single row that could not be attributed makes EVERY slice figure on the device
// absent rather than publishing a partial sum as a complete one.
func TestAggregateAcceleratorProcesses(t *testing.T) {
	usage := func(podUID string, memoryMiB *uint64, cores *uint32) SliceUsage {
		return SliceUsage{
			Manufacturer:            testManufacturer,
			PodUID:                  podUID,
			Container:               testCtrName,
			DeviceID:                testDeviceID,
			MemoryUsedMiB:           memoryMiB,
			CoresUtilizationPercent: cores,
		}
	}

	cases := []struct {
		name       string
		procs      device.AcceleratorProcesses
		results    map[uint32]procattr.Result
		cardMemory uint64
		wantUsages []SliceUsage
		wantDiag   SliceDeviceDiagnostics
	}{
		{
			name:       "a complete read of an idle device yields no record and no reason",
			procs:      device.AcceleratorProcesses{ID: testDeviceID},
			cardMemory: testCardMemory,
			wantUsages: nil,
			wantDiag:   SliceDeviceDiagnostics{Manufacturer: testManufacturer, DeviceID: testDeviceID},
		},
		{
			name: "another Instance's row is that Instance's alone",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 200, MemoryBytes: ptr.To[uint64](2 << 30), CoresPercent: ptr.To[uint32](40)},
			}},
			results:    map[uint32]procattr.Result{200: attributed(testPodBUID, testCtrBID)},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodBUID, ptr.To[uint64](2<<10), ptr.To[uint32](40))},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 1, RowsAttributed: 1,
			},
		},
		{
			name: "a row reporting zero is a measurement, not an absence",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](0), CoresPercent: ptr.To[uint32](0)},
			}},
			results:    map[uint32]procattr.Result{100: attributed(testPodAUID, testCtrAID)},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodAUID, ptr.To[uint64](0), ptr.To[uint32](0))},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 1, RowsAttributed: 1,
			},
		},
		{
			name: "a row stating a compute figure of zero is idle, and publishes that zero",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				idleRow(100, 1<<30),
			}},
			results:    map[uint32]procattr.Result{100: attributed(testPodAUID, testCtrAID)},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodAUID, ptr.To[uint64](1<<10), ptr.To[uint32](0))},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 1, RowsAttributed: 1,
			},
		},
		{
			// The gfx1101 case, and the one this rule exists for: the query answered, so the device
			// is measurable, but the row carries no compute figure because that hardware revision
			// cannot measure occupancy. Publishing a zero would report a busy container as idle.
			name: "a row carrying no compute figure is unmeasured, not idle",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				memoryRow(100, 1<<30),
			}},
			results:    map[uint32]procattr.Result{100: attributed(testPodAUID, testCtrAID)},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodAUID, ptr.To[uint64](1<<10), nil)},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 1, RowsAttributed: 1,
			},
		},
		{
			// One unmeasurable row is enough: a sum missing one of its terms is not a smaller share,
			// it is a wrong one — the same rule the memory sum already followed.
			name: "one row with no compute figure makes the container's whole compute share absent",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				idleRow(100, 1<<30),
				{PID: 101, MemoryBytes: ptr.To[uint64](1 << 30)},
			}},
			results: map[uint32]procattr.Result{
				100: attributed(testPodAUID, testCtrAID),
				101: attributed(testPodAUID, testCtrAID),
			},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodAUID, ptr.To[uint64](2<<10), nil)},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 2, RowsAttributed: 2,
			},
		},
		{
			name: "an unsupported memory entry point leaves memory absent and compute untouched",
			procs: device.AcceleratorProcesses{
				ID:           testDeviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, CoresPercent: ptr.To[uint32](25)},
				},
			},
			results:    map[uint32]procattr.Result{100: attributed(testPodAUID, testCtrAID)},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodAUID, nil, ptr.To[uint32](25))},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				RowsReturned: 1, RowsAttributed: 1,
			},
		},
		{
			name: "a permission failure is absent, never idle",
			procs: device.AcceleratorProcesses{
				ID:           testDeviceID,
				MemoryReason: device.AcceleratorProcessReasonPermission,
				CoresReason:  device.AcceleratorProcessReasonPermission,
			},
			cardMemory: testCardMemory,
			wantUsages: nil,
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				MemoryReason: device.AcceleratorProcessReasonPermission,
				CoresReason:  device.AcceleratorProcessReasonPermission,
			},
		},
		{
			name: "a truncated read is absent and counted, never partial",
			procs: device.AcceleratorProcesses{
				ID:           testDeviceID,
				MemoryReason: device.AcceleratorProcessReasonTruncated,
				CoresReason:  device.AcceleratorProcessReasonTruncated,
			},
			cardMemory: testCardMemory,
			wantUsages: nil,
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				MemoryReason:   device.AcceleratorProcessReasonTruncated,
				CoresReason:    device.AcceleratorProcessReasonTruncated,
				ReadsTruncated: 2,
			},
		},
		{
			name: "one unattributable row makes every figure on the device absent",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](10)},
				memoryRow(300, 4<<30),
			}},
			results: map[uint32]procattr.Result{
				100: attributed(testPodAUID, testCtrAID),
				300: {Reason: procattr.ReasonNoPodComponent},
			},
			cardMemory: testCardMemory,
			wantUsages: nil,
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				MemoryReason: device.AcceleratorProcessReason(procattr.ReasonNoPodComponent),
				CoresReason:  device.AcceleratorProcessReason(procattr.ReasonNoPodComponent),
				// The counts stay a record of what the pass saw: one row was attributed, and the
				// figures are absent anyway because the other one could not be.
				RowsReturned: 2, RowsAttributed: 1, RowsAmbiguous: 1,
			},
		},
		{
			name: "a row in a Pod backing no Instance is dropped and the device stays measurable",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](10)},
				memoryRow(400, 4<<30),
			}},
			results: map[uint32]procattr.Result{
				100: attributed(testPodAUID, testCtrAID),
				400: {Outcome: procattr.OutcomeExcluded},
			},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodAUID, ptr.To[uint64](1<<10), ptr.To[uint32](10))},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 2, RowsAttributed: 1, RowsNonInstance: 1,
			},
		},
		{
			name: "a /proc read that failed is counted apart from an unusable cgroup",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				memoryRow(300, 1<<30),
			}},
			results:    map[uint32]procattr.Result{300: {Reason: procattr.ReasonPermission}},
			cardMemory: testCardMemory,
			wantUsages: nil,
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				MemoryReason:   device.AcceleratorProcessReason(procattr.ReasonPermission),
				CoresReason:    device.AcceleratorProcessReason(procattr.ReasonPermission),
				RowsReturned:   1,
				RowsUnreadable: 1,
			},
		},
		{
			name: "one sentinel row makes its own container's memory absent, not its neighbour's",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				idleRow(100, 1<<30),
				{PID: 101, CoresPercent: ptr.To[uint32](5)},
				idleRow(200, 2<<30),
			}},
			results: map[uint32]procattr.Result{
				100: attributed(testPodAUID, testCtrAID),
				101: attributed(testPodAUID, testCtrAID),
				200: attributed(testPodBUID, testCtrBID),
			},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{
				usage(testPodAUID, nil, ptr.To[uint32](5)),
				usage(testPodBUID, ptr.To[uint64](2<<10), ptr.To[uint32](0)),
			},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 3, RowsAttributed: 3,
			},
		},
		{
			name: "a sub-MiB sum is summed native and stays non-zero",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				idleRow(100, 300<<10),
				idleRow(101, 300<<10),
			}},
			results: map[uint32]procattr.Result{
				100: attributed(testPodAUID, testCtrAID),
				101: attributed(testPodAUID, testCtrAID),
			},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodAUID, ptr.To[uint64](1), ptr.To[uint32](0))},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 2, RowsAttributed: 2,
			},
		},
		{
			name: "a sum above the card's own capacity is invalid data, never a figure",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](64 << 30), CoresPercent: ptr.To[uint32](20)},
			}},
			results:    map[uint32]procattr.Result{100: attributed(testPodAUID, testCtrAID)},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodAUID, nil, ptr.To[uint32](20))},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				MemoryReason: sliceReasonInvalidData,
				RowsReturned: 1, RowsAttributed: 1,
			},
		},
		{
			name: "a container's processes sum, and a compute sum past the card is held at 100",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](70)},
				{PID: 101, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](60)},
			}},
			results: map[uint32]procattr.Result{
				100: attributed(testPodAUID, testCtrAID),
				101: attributed(testPodAUID, testCtrAID),
			},
			cardMemory: testCardMemory,
			wantUsages: []SliceUsage{usage(testPodAUID, ptr.To[uint64](2<<10), ptr.To[uint32](100))},
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 2, RowsAttributed: 2,
			},
		},
		{
			name: "a container the Pod's status does not name is refused, and refuses the device",
			procs: device.AcceleratorProcesses{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				memoryRow(100, 1<<30),
			}},
			results:    map[uint32]procattr.Result{100: {Reason: procattr.ReasonUnknownContainer}},
			cardMemory: testCardMemory,
			wantUsages: nil,
			wantDiag: SliceDeviceDiagnostics{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				MemoryReason:  device.AcceleratorProcessReason(procattr.ReasonUnknownContainer),
				CoresReason:   device.AcceleratorProcessReason(procattr.ReasonUnknownContainer),
				RowsReturned:  1,
				RowsAmbiguous: 1,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			usages, diag := aggregateAcceleratorProcesses(
				testManufacturer, c.procs, c.cardMemory, testContainerNames(),
				func(_ []uint32) map[uint32]procattr.Result { return c.results },
			)
			assert.Equal(t, c.wantUsages, usages)
			assert.Equal(t, c.wantDiag, diag)
		})
	}
}

// TestBuildSliceSection pins the whole producer pass over a two-Instance node sharing one card:
// each Instance's aggregate is its own, a container holding two devices is summed per device rather
// than collapsed, and one host process on a device takes every figure on THAT device away while
// leaving the other device measurable.
func TestBuildSliceSection(t *testing.T) {
	const secondDeviceID = "GPU-1"

	metrics := device.MetricsGroupList{{
		Manufacturer: testManufacturer,
		Timestamp:    time.Now(),
		Accelerators: []device.AcceleratorMetrics{
			{ID: testDeviceID, Memory: testCardMemory},
			{ID: secondDeviceID, Memory: testCardMemory},
		},
	}}

	procGroups := []device.AcceleratorProcessesGroup{{
		Manufacturer: testManufacturer,
		Timestamp:    time.Now(),
		Accelerators: []device.AcceleratorProcesses{
			{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](10)},
				{PID: 200, MemoryBytes: ptr.To[uint64](2 << 30), CoresPercent: ptr.To[uint32](20)},
			}},
			{ID: secondDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 101, MemoryBytes: ptr.To[uint64](4 << 30), CoresPercent: ptr.To[uint32](30)},
			}},
		},
	}}

	results := map[uint32]procattr.Result{
		100: attributed(testPodAUID, testCtrAID),
		101: attributed(testPodAUID, testCtrAID),
		200: attributed(testPodBUID, testCtrBID),
	}
	resolve := func(pids []uint32) map[uint32]procattr.Result {
		out := make(map[uint32]procattr.Result, len(pids))
		for _, pid := range pids {
			if r, ok := results[pid]; ok {
				out[pid] = r
			} else {
				out[pid] = procattr.Result{Reason: procattr.ReasonNoPodComponent}
			}
		}
		return out
	}

	t.Run("each Instance reads its own figure, per device", func(t *testing.T) {
		section := buildSliceSection(procGroups, metrics, testContainerNames(), resolve)
		require.NotNil(t, section)
		assert.Equal(t, MonitorSliceSchemaVersion, section.SchemaVersion)
		assert.False(t, section.Truncated)

		figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
		require.True(t, ok)
		assert.Equal(t, ptr.To[uint64](1<<10), figures.MemoryUsedMiB, "pod A holds its own GiB, not the card's three")
		assert.Equal(t, ptr.To[uint32](10), figures.CoresUtilizationPercent)

		figures, ok = section.Figures(testManufacturer, testPodBUID, testCtrName, testDeviceID)
		require.True(t, ok)
		assert.Equal(t, ptr.To[uint64](2<<10), figures.MemoryUsedMiB)

		// The same container on the second device is a second record, not a bigger first one.
		figures, ok = section.Figures(testManufacturer, testPodAUID, testCtrName, secondDeviceID)
		require.True(t, ok)
		assert.Equal(t, ptr.To[uint64](4<<10), figures.MemoryUsedMiB)
	})

	t.Run("a container with no process on a measured device reads zero, not absent", func(t *testing.T) {
		section := buildSliceSection(procGroups, metrics, testContainerNames(), resolve)
		require.NotNil(t, section)

		figures, ok := section.Figures(testManufacturer, testPodBUID, testCtrName, secondDeviceID)
		require.True(t, ok)
		assert.Equal(t, ptr.To[uint64](0), figures.MemoryUsedMiB)
		assert.Equal(t, ptr.To[uint32](0), figures.CoresUtilizationPercent)
	})

	t.Run("a device the section does not carry answers nothing", func(t *testing.T) {
		section := buildSliceSection(procGroups, metrics, testContainerNames(), resolve)
		require.NotNil(t, section)

		_, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, "GPU-absent")
		assert.False(t, ok)
		_, ok = section.Figures("amd", testPodAUID, testCtrName, testDeviceID)
		assert.False(t, ok)
	})

	t.Run("one host process takes its own device's figures and no others", func(t *testing.T) {
		poisoned := []device.AcceleratorProcessesGroup{{
			Manufacturer: testManufacturer,
			Accelerators: []device.AcceleratorProcesses{
				{ID: testDeviceID, Processes: append(
					procGroups[0].Accelerators[0].Processes,
					memoryRow(999, 8<<30), // a plain host process, in no Pod at all
				)},
				procGroups[0].Accelerators[1],
			},
		}}

		section := buildSliceSection(poisoned, metrics, testContainerNames(), resolve)
		require.NotNil(t, section)

		for _, podUID := range []string{testPodAUID, testPodBUID} {
			figures, ok := section.Figures(testManufacturer, podUID, testCtrName, testDeviceID)
			require.True(t, ok)
			assert.Nil(t, figures.MemoryUsedMiB, "an unattributable row makes every figure on the device absent")
			assert.Nil(t, figures.CoresUtilizationPercent)
		}

		figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, secondDeviceID)
		require.True(t, ok)
		assert.Equal(t, ptr.To[uint64](4<<10), figures.MemoryUsedMiB, "the other device is untouched")
	})

	t.Run("no host process and no rows at all yields no section", func(t *testing.T) {
		assert.Nil(t, buildSliceSection(nil, metrics, testContainerNames(), resolve))
	})

	t.Run("neither a raw pid nor a container runtime id reaches the wire", func(t *testing.T) {
		section := buildSliceSection(procGroups, metrics, testContainerNames(), resolve)
		require.NotNil(t, section)

		data, err := json.Marshal(MonitorSnapshot{Groups: metrics, Slices: section})
		require.NoError(t, err)
		encoded := string(data)
		for _, pid := range []string{"100", "101", "200", "999"} {
			assert.NotContains(t, encoded, `"pid":`+pid)
		}
		assert.NotContains(t, encoded, testCtrAID)
		assert.NotContains(t, encoded, testCtrBID)
		assert.Contains(t, encoded, testCtrName)
	})
}

// TestBuildSliceSectionBound pins that the section is bounded rather than unbounded, and that a
// bounded section cannot push the snapshot past the worker's read limit. The bound is asserted here
// rather than assumed from typical node sizes.
func TestBuildSliceSectionBound(t *testing.T) {
	index := procattr.PodIndex{}
	names := containerNames{}
	procs := make([]device.AcceleratorProcesses, 0, 8)
	results := map[uint32]procattr.Result{}
	metrics := device.MetricsGroupList{{Manufacturer: testManufacturer}}

	// A node far past anything real: 8 devices, and enough Pods on each to overrun the record bound.
	pid := uint32(1)
	perDevice := maxSliceUsages/8 + 16
	for d := range 8 {
		deviceID := fmt.Sprintf("GPU-%02d-%s", d, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
		metrics[0].Accelerators = append(metrics[0].Accelerators,
			device.AcceleratorMetrics{ID: deviceID, Memory: 1 << 20})
		rows := make([]device.AcceleratorProcess, 0, perDevice)
		for i := range perDevice {
			podUID := fmt.Sprintf("%08d-89ab-cdef-0123-456789abcdef", i)
			ctrID := fmt.Sprintf("deadbeef%08d", i)
			index[podUID] = procattr.Pod{Instance: true, Containers: sets.New(ctrID)}
			if names[podUID] == nil {
				names[podUID] = map[string]string{}
			}
			names[podUID][ctrID] = "instance-container-name"
			rows = append(rows, memoryRow(pid, 1<<20))
			results[pid] = attributed(podUID, ctrID)
			pid++
		}
		procs = append(procs, device.AcceleratorProcesses{ID: deviceID, Processes: rows})
	}

	section := buildSliceSection(
		[]device.AcceleratorProcessesGroup{{Manufacturer: testManufacturer, Accelerators: procs}},
		metrics, names,
		func(_ []uint32) map[uint32]procattr.Result { return results },
	)
	require.NotNil(t, section)

	assert.True(t, section.Truncated, "overrunning the bound is reported, not hidden")
	assert.LessOrEqual(t, len(section.Usages), maxSliceUsages, "the record count is bounded")
	assert.LessOrEqual(t, len(section.Devices), maxSliceDevices)

	// A device dropped by the bound is dropped WHOLE and says so, so its containers read absent
	// rather than the measured zero a missing record on a measured device would mean.
	dropped := 0
	for _, diag := range section.Devices {
		if diag.MemoryReason != sliceReasonBounded {
			continue
		}
		dropped++
		assert.Equal(t, sliceReasonBounded, diag.CoresReason)
		// Every figure on such a device is absent, whichever container is asked for.
		figures, ok := section.Figures(
			testManufacturer, "00000000-89ab-cdef-0123-456789abcdef",
			"instance-container-name", diag.DeviceID)
		require.True(t, ok)
		assert.Nil(t, figures.MemoryUsedMiB)
		assert.Nil(t, figures.CoresUtilizationPercent)
		for _, usage := range section.Usages {
			assert.NotEqual(t, diag.DeviceID, usage.DeviceID,
				"a bounded-out device keeps none of its records")
		}
	}
	assert.Positive(t, dropped, "the fixture must actually overrun the bound")

	data, err := json.Marshal(MonitorSnapshot{
		Timestamp: time.Now(), PeriodSeconds: 15, Groups: metrics, Slices: section,
	})
	require.NoError(t, err)
	assert.Less(t, len(data), monitorSnapshotReadLimitBytes,
		"a bound-full section must not push the snapshot past the worker's read limit")
}

// TestCollectSlices pins the monitor pass's own decisions: a manufacturer whose detector cannot
// answer the per-process query yields no section at all, and neither does a tick whose Pod list
// failed — an EMPTY section would say "measured, nothing there", which is the one thing a failed
// read must never say.
func TestCollectSlices(t *testing.T) {
	metrics := device.MetricsGroupList{{
		Manufacturer: testManufacturer,
		Accelerators: []device.AcceleratorMetrics{{ID: testDeviceID, Memory: testCardMemory}},
	}}

	// detectorWith builds a detector whose one manufacturer answers per-process rows for both
	// devices, over the given Pod list and /proc tree.
	detectorWith := func(
		pods func(context.Context) ([]core.Pod, error), tree fstest.MapFS,
	) (*Detector, *fakeProcessDetector) {
		provider := &fakeProcessDetector{
			fakeDetector: fakeDetector{name: testManufacturer},
			group: device.AcceleratorProcessesGroup{
				Manufacturer: testManufacturer,
				Accelerators: []device.AcceleratorProcesses{
					{ID: testDeviceID, Processes: []device.AcceleratorProcess{
						{PID: 100, MemoryBytes: ptr.To[uint64](6 << 30), CoresPercent: ptr.To[uint32](18)},
					}},
					{ID: "GPU-1", Processes: []device.AcceleratorProcess{memoryRow(999, 8<<30)}},
				},
			},
		}
		return &Detector{
			detectors:    []device.Detector{provider},
			procResolver: procattr.New(tree),
			podLister:    pods,
		}, provider
	}

	slicedPods := func(_ context.Context) ([]core.Pod, error) {
		return []core.Pod{*instancePodFixture(
			testPodAUID, testCtrAID, workercore.DeviceAllocationModeSliced, testDeviceID)}, nil
	}

	t.Run("a detector serving no per-process query yields no section", func(t *testing.T) {
		d := &Detector{
			detectors:    []device.Detector{&fakeDetector{name: testManufacturer}},
			procResolver: procattr.New(fstest.MapFS{}),
			podLister:    slicedPods,
		}
		assert.Nil(t, d.collectSlices(t.Context(), metrics))
	})

	// The next two cases are the same outcome for opposite reasons, and the difference is the
	// point: "the node carves nothing" is knowledge, "the Pod list failed" is the absence of it.
	// Neither may produce an empty section, which would claim the node was measured and found bare.
	t.Run("a failed Pod list yields no section, and queries nothing", func(t *testing.T) {
		d, provider := detectorWith(func(_ context.Context) ([]core.Pod, error) {
			return nil, errors.New("informer cache is not started yet")
		}, fstest.MapFS{})

		assert.Nil(t, d.collectSlices(t.Context(), metrics))
		assert.Nil(t, provider.queried, "a failed Pod list must not reach the vendor library")
	})

	t.Run("a node whose Pods carve nothing yields no section, and queries nothing", func(t *testing.T) {
		for _, mode := range []workercore.DeviceAllocationMode{
			workercore.DeviceAllocationModeExclusive,
			workercore.DeviceAllocationModeShared,
			workercore.DeviceAllocationModeVisibility,
		} {
			t.Run(mode.String(), func(t *testing.T) {
				d, provider := detectorWith(func(_ context.Context) ([]core.Pod, error) {
					return []core.Pod{*instancePodFixture(
						testPodAUID, testCtrAID, mode, testDeviceID)}, nil
				}, fakeProcTree(100, testPodAUID, testCtrAID))

				assert.Nil(t, d.collectSlices(t.Context(), metrics))
				assert.Nil(t, provider.queried,
					"a whole-device allocation has no slice to report and costs no vendor call")
			})
		}
	})

	t.Run("only the carved devices are queried", func(t *testing.T) {
		for _, mode := range []workercore.DeviceAllocationMode{
			workercore.DeviceAllocationModeSliced,
			workercore.DeviceAllocationModePartitioned,
		} {
			t.Run(mode.String(), func(t *testing.T) {
				d, provider := detectorWith(func(_ context.Context) ([]core.Pod, error) {
					return []core.Pod{*instancePodFixture(
						testPodAUID, testCtrAID, mode, testDeviceID)}, nil
				}, fakeProcTree(100, testPodAUID, testCtrAID))

				section := d.collectSlices(t.Context(), metrics)
				require.NotNil(t, section)
				assert.Equal(t, sets.New(testDeviceID), provider.queried)

				// The card nobody carved is not in the section at all, so the host process on it
				// poisons nothing.
				assert.Len(t, section.Devices, 1)
				_, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, "GPU-1")
				assert.False(t, ok)
			})
		}
	})

	t.Run("a device nothing could be read from still reaches the snapshot with its reason", func(t *testing.T) {
		// This is what the adapter answers when the driver could not be reached at all. The
		// section must still be produced: no section means no schema version and no reason, and
		// then the absence the consumer publishes has nothing to explain it.
		d := &Detector{
			detectors: []device.Detector{&fakeProcessDetector{
				fakeDetector: fakeDetector{name: testManufacturer},
				group: device.AcceleratorProcessesGroup{
					Manufacturer: testManufacturer,
					Accelerators: []device.AcceleratorProcesses{{
						ID:           testDeviceID,
						MemoryReason: device.AcceleratorProcessReasonDriverError,
						CoresReason:  device.AcceleratorProcessReasonDriverError,
					}},
				},
			}},
			procResolver: procattr.New(fstest.MapFS{}),
			podLister:    slicedPods,
		}

		section := d.collectSlices(t.Context(), metrics)
		require.NotNil(t, section, "a device that could not be read is still reported")
		assert.Equal(t, MonitorSliceSchemaVersion, section.SchemaVersion)
		require.Len(t, section.Devices, 1)
		assert.Equal(t, device.AcceleratorProcessReasonDriverError, section.Devices[0].MemoryReason)
		assert.Equal(t, device.AcceleratorProcessReasonDriverError, section.Devices[0].CoresReason)

		figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
		require.True(t, ok)
		assert.Nil(t, figures.MemoryUsedMiB, "absent, and the reason above says why")
		assert.Nil(t, figures.CoresUtilizationPercent)
	})

	t.Run("a fake /proc tree carries a real attribution end to end", func(t *testing.T) {
		d, _ := detectorWith(slicedPods, fakeProcTree(100, testPodAUID, testCtrAID))

		section := d.collectSlices(t.Context(), metrics)
		require.NotNil(t, section)
		figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
		require.True(t, ok)
		assert.Equal(t, ptr.To[uint64](6<<10), figures.MemoryUsedMiB)
		assert.Equal(t, ptr.To[uint32](18), figures.CoresUtilizationPercent)
	})
}

// TestCarvedAcceleratorsOf pins the query gate: a device is a target exactly when some container
// holds it under a carved mode, whichever of the two carved modes that is.
func TestCarvedAcceleratorsOf(t *testing.T) {
	cases := []struct {
		name string
		pods []core.Pod
		want map[string]sets.Set[string]
	}{
		{
			name: "a logical slice is a target",
			pods: []core.Pod{*instancePodFixture(
				testPodAUID, testCtrAID, workercore.DeviceAllocationModeSliced, testDeviceID)},
			want: map[string]sets.Set[string]{testManufacturer: sets.New(testDeviceID)},
		},
		{
			name: "a hardware partition is a target too",
			pods: []core.Pod{*instancePodFixture(
				testPodAUID, testCtrAID, workercore.DeviceAllocationModePartitioned, testDeviceID)},
			want: map[string]sets.Set[string]{testManufacturer: sets.New(testDeviceID)},
		},
		{
			name: "a whole-device allocation is not",
			pods: []core.Pod{*instancePodFixture(
				testPodAUID, testCtrAID, workercore.DeviceAllocationModeExclusive, testDeviceID)},
			want: map[string]sets.Set[string]{},
		},
		{
			name: "a shared allocation carries no quota to read a slice against",
			pods: []core.Pod{*instancePodFixture(
				testPodAUID, testCtrAID, workercore.DeviceAllocationModeShared, testDeviceID)},
			want: map[string]sets.Set[string]{},
		},
		{
			name: "two Pods carving two devices yield both",
			pods: []core.Pod{
				*instancePodFixture(
					testPodAUID, testCtrAID, workercore.DeviceAllocationModeSliced, testDeviceID),
				*instancePodFixture(
					testPodBUID, testCtrBID, workercore.DeviceAllocationModeSliced, "GPU-1"),
			},
			want: map[string]sets.Set[string]{testManufacturer: sets.New(testDeviceID, "GPU-1")},
		},
		{
			name: "an unannotated Pod names nothing",
			pods: []core.Pod{{ObjectMeta: meta.ObjectMeta{Name: "plain", UID: types.UID(testPodCUID)}}},
			want: map[string]sets.Set[string]{},
		},
		{
			name: "a Pod whose annotation cannot be read never hides its siblings' devices",
			pods: []core.Pod{
				{ObjectMeta: meta.ObjectMeta{
					Name: "broken", UID: types.UID(testPodCUID),
					Annotations: map[string]string{
						deviceplugin.AllocatedAcceleratorAnnoKey: "{not json",
					},
				}},
				*instancePodFixture(
					testPodAUID, testCtrAID, workercore.DeviceAllocationModeSliced, testDeviceID),
			},
			want: map[string]sets.Set[string]{testManufacturer: sets.New(testDeviceID)},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, carvedAcceleratorsOf(c.pods))
		})
	}
}

// TestPodIndexOf pins what the Pod index carries: every Pod the node runs, marked by whether it
// backs an Instance, with the container runtime IDs stripped of their scheme and joined to the
// container NAMES the section is keyed by.
func TestPodIndexOf(t *testing.T) {
	instance := instancePodFixture(
		testPodAUID, testCtrAID, workercore.DeviceAllocationModeSliced, testDeviceID)
	plain := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "plain", Namespace: "default", UID: types.UID(testPodCUID)},
		Status: core.PodStatus{ContainerStatuses: []core.ContainerStatus{
			{Name: "app", ContainerID: "containerd://beefbeef9999"},
		}},
	}

	index, names := podIndexOf([]core.Pod{*instance, *plain})

	assert.Equal(t, procattr.PodIndex{
		testPodAUID: {Instance: true, Containers: sets.New(testCtrAID)},
		testPodCUID: {Instance: false, Containers: sets.New("beefbeef9999")},
	}, index)
	assert.Equal(t, containerNames{
		testPodAUID: {testCtrAID: testCtrName},
		testPodCUID: {"beefbeef9999": "app"},
	}, names)
}

// fakeProcessDetector is a device.Detector that also serves recorded per-process rows, standing in
// for a manufacturer whose library answers the per-process query. A plain fakeDetector stands for
// the eight that do not, and must leave the section unaffected.
type fakeProcessDetector struct {
	fakeDetector
	group device.AcceleratorProcessesGroup
	err   error

	// queried records the devices the pass asked about, so a test can pin that a device carrying
	// no carved allocation is never queried at all.
	queried sets.Set[string]
}

func (f *fakeProcessDetector) MonitorAcceleratorProcesses(
	_ bool, deviceIDs sets.Set[string],
) (device.AcceleratorProcessesGroup, error) {
	f.queried = deviceIDs
	if f.err != nil {
		return device.AcceleratorProcessesGroup{}, f.err
	}

	// Answer only for the devices asked about, as a real adapter does.
	grp := device.AcceleratorProcessesGroup{Manufacturer: f.group.Manufacturer}
	for _, procs := range f.group.Accelerators {
		if deviceIDs.Has(procs.ID) {
			grp.Accelerators = append(grp.Accelerators, procs)
		}
	}
	return grp, nil
}

// fakeProcTree is a /proc tree holding one containerized process, in the cgroup v2 systemd shape a
// containerd node writes.
func fakeProcTree(pid uint32, podUID, containerID string) fstest.MapFS {
	escaped := strings.ReplaceAll(podUID, "-", "_")
	dir := fmt.Sprintf("%d", pid)
	return fstest.MapFS{
		dir + "/cgroup": {Data: []byte(fmt.Sprintf(
			"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod%s.slice/cri-containerd-%s.scope\n",
			escaped, containerID))},
		dir + "/stat": {Data: []byte(fmt.Sprintf("%d (python3) S 1 1 0 0 -1 4194304 0", pid))},
		dir + "/comm": {Data: []byte("python3\n")},
	}
}

// instancePodFixture is a Pod backing an Instance, in the shape the exporter recognizes one by,
// holding the given devices under the given allocation mode as the device plugin records it.
func instancePodFixture(
	podUID, containerID string, mode workercore.DeviceAllocationMode, deviceIDs ...string,
) *core.Pod {
	return manufacturerPodFixture(podUID, containerID, testManufacturer, mode, deviceIDs...)
}

// manufacturerPodFixture is the same Pod, holding one named manufacturer's devices — which the
// ledger cases need, because only two manufacturers have a slicing shim of ours to read a region
// from.
func manufacturerPodFixture(
	podUID, containerID, manufacturer string,
	mode workercore.DeviceAllocationMode, deviceIDs ...string,
) *core.Pod {
	accelerators := make([]workercore.AcceleratorAllocation, 0, len(deviceIDs))
	for i, id := range deviceIDs {
		accelerators = append(accelerators, workercore.AcceleratorAllocation{
			ID: id, Index: uint32(i), Mode: mode, Allocated: 400_000,
		})
	}
	return podFixture(podUID, containerID, manufacturer, accelerators)
}

// partitionPodFixture is a Pod holding one hardware partition of one accelerator, recorded the way
// the device plugin records one: the parent accelerator's ID, the profile name, and the placement the
// partition occupies on it.
func partitionPodFixture(
	podUID, containerID, deviceID, profile string, placements ...workercore.AcceleratorPlacement,
) *core.Pod {
	return podFixture(podUID, containerID, testManufacturer, []workercore.AcceleratorAllocation{{
		ID:                          deviceID,
		Mode:                        workercore.DeviceAllocationModePartitioned,
		Allocated:                   50_000,
		AllocatedPhysicalProfile:    profile,
		AllocatedPhysicalPlacements: placements,
	}})
}

// podFixture is the Pod skeleton both fixtures share: an Instance's Pod, one container, holding the
// given accelerator allocations under the device plugin's own annotation.
func podFixture(
	podUID, containerID, manufacturer string, accelerators []workercore.AcceleratorAllocation,
) *core.Pod {
	allocations := deviceplugin.PodAllocations{
		testCtrName: {Devices: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID:           "group-0",
				Manufacturer: manufacturer,
				Accelerators: accelerators,
			}},
		}},
	}
	annotation, err := json.Marshal(allocations)
	if err != nil {
		panic(err)
	}

	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      "instance-0",
			Namespace: "default",
			UID:       types.UID(podUID),
			Labels: map[string]string{
				deviceplugin.InstancePartOfLabelKey: "instance-uid",
			},
			Annotations: map[string]string{
				deviceplugin.AllocatedAcceleratorAnnoKey: string(annotation),
			},
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: workercore.GroupVersion.String(),
				Kind:       "Instance",
				Name:       "instance-0",
				UID:        "instance-uid",
				Controller: ptr.To(true),
			}},
		},
		Status: core.PodStatus{ContainerStatuses: []core.ContainerStatus{
			{Name: testCtrName, ContainerID: "containerd://" + containerID},
		}},
	}
}

// fakePartitionDetector is a device.Detector that also answers for a hardware partition's own
// handle, standing in for a manufacturer whose library resolves one. A plain fakeDetector stands for
// the ones that do not, and must leave the section's partitions empty.
type fakePartitionDetector struct {
	fakeDetector
	partitions []device.AcceleratorPartition
	err        error

	// requested records what the pass asked about, so a test can pin that an uncarved card and a
	// logical slice cost no partition call.
	requested []device.AcceleratorPartitionRequest
}

func (f *fakePartitionDetector) MonitorAcceleratorPartitions(
	_ bool, requests []device.AcceleratorPartitionRequest,
) (device.AcceleratorPartitionsGroup, error) {
	f.requested = requests
	if f.err != nil {
		return device.AcceleratorPartitionsGroup{}, f.err
	}
	return device.AcceleratorPartitionsGroup{
		Manufacturer: f.name,
		Partitions:   f.partitions,
	}, nil
}

// testPlacement is the placement one 1g partition occupies on an H100, in the memory-slice units both
// the allocation record and the driver count in.
func testPlacement(start, length int32) []workercore.AcceleratorPlacement {
	return []workercore.AcceleratorPlacement{{Start: start, Length: length}}
}

// TestPartitionTargetsOf pins which partitions are asked about. A partition is named by its profile
// AND its placement together, so a record carrying only one of the two is not a partition this can
// name — and naming it loosely would read one tenant's figure off another's partition.
func TestPartitionTargetsOf(t *testing.T) {
	cases := []struct {
		name string
		pods []core.Pod
		want map[string][]partitionTarget
	}{
		{
			name: "a partition with a profile and a placement is a target",
			pods: []core.Pod{*partitionPodFixture(
				testPodAUID, testCtrAID, testDeviceID, "1g.10gb", testPlacement(0, 1)...)},
			want: map[string][]partitionTarget{testManufacturer: {{
				podUID: testPodAUID, container: testCtrName,
				request: device.AcceleratorPartitionRequest{
					DeviceID: testDeviceID, Profile: "1g.10gb",
					Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
				},
			}}},
		},
		{
			name: "a partition naming no profile is not a target",
			pods: []core.Pod{*partitionPodFixture(
				testPodAUID, testCtrAID, testDeviceID, "", testPlacement(0, 1)...)},
			want: map[string][]partitionTarget{},
		},
		{
			name: "a partition naming no placement is not a target",
			pods: []core.Pod{*partitionPodFixture(
				testPodAUID, testCtrAID, testDeviceID, "1g.10gb")},
			want: map[string][]partitionTarget{},
		},
		{
			name: "a logical slice is not a target: it has no handle of its own to read",
			pods: []core.Pod{*instancePodFixture(
				testPodAUID, testCtrAID, workercore.DeviceAllocationModeSliced, testDeviceID)},
			want: map[string][]partitionTarget{},
		},
		{
			// One partition has one tenant. Two records naming it are two records at least one of
			// which is wrong, and nothing here can tell which, so neither is answered.
			name: "a partition two Pods claim is dropped for both",
			pods: []core.Pod{
				*partitionPodFixture(
					testPodAUID, testCtrAID, testDeviceID, "1g.10gb", testPlacement(0, 1)...),
				*partitionPodFixture(
					testPodBUID, testCtrBID, testDeviceID, "1g.10gb", testPlacement(0, 1)...),
			},
			want: map[string][]partitionTarget{},
		},
		{
			name: "a partition two Pods claim does not take a sibling partition with it",
			pods: []core.Pod{
				*partitionPodFixture(
					testPodAUID, testCtrAID, testDeviceID, "1g.10gb", testPlacement(0, 1)...),
				*partitionPodFixture(
					testPodBUID, testCtrBID, testDeviceID, "1g.10gb", testPlacement(0, 1)...),
				*partitionPodFixture(
					testPodCUID, testCtrAID, testDeviceID, "1g.10gb", testPlacement(2, 1)...),
			},
			want: map[string][]partitionTarget{testManufacturer: {{
				podUID: testPodCUID, container: testCtrName,
				request: device.AcceleratorPartitionRequest{
					DeviceID: testDeviceID, Profile: "1g.10gb",
					Placements: []device.AcceleratorPlacement{{Start: 2, Length: 1}},
				},
			}}},
		},
		{
			name: "two Pods partitioning one card are two targets",
			pods: []core.Pod{
				*partitionPodFixture(
					testPodAUID, testCtrAID, testDeviceID, "1g.10gb", testPlacement(0, 1)...),
				*partitionPodFixture(
					testPodBUID, testCtrBID, testDeviceID, "1g.10gb", testPlacement(1, 1)...),
			},
			want: map[string][]partitionTarget{testManufacturer: {
				{
					podUID: testPodAUID, container: testCtrName,
					request: device.AcceleratorPartitionRequest{
						DeviceID: testDeviceID, Profile: "1g.10gb",
						Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
					},
				},
				{
					podUID: testPodBUID, container: testCtrName,
					request: device.AcceleratorPartitionRequest{
						DeviceID: testDeviceID, Profile: "1g.10gb",
						Placements: []device.AcceleratorPlacement{{Start: 1, Length: 1}},
					},
				},
			}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, partitionTargetsOf(c.pods))
		})
	}
}

// TestWithPartitions pins the fold: a figure converted once, a zero kept as the measurement it is, a
// reason carried where there is no figure, and a figure the parent card contradicts refused.
func TestWithPartitions(t *testing.T) {
	cardMemory := map[string]uint64{testManufacturer + "/" + testDeviceID: testCardMemory}
	row := func(partition device.AcceleratorPartition) partitionRow {
		partition.DeviceID = testDeviceID
		return partitionRow{
			manufacturer: testManufacturer, podUID: testPodAUID,
			container: testCtrName, partition: partition,
		}
	}

	t.Run("a partition's identity and figures are converted once and carried", func(t *testing.T) {
		section := withPartitions(nil, []partitionRow{row(device.AcceleratorPartition{
			ID:               "MIG-aaa",
			MemoryTotalBytes: ptr.To[uint64](10 << 30),
			MemoryUsedBytes:  ptr.To[uint64](3 << 30),
			CoresReason:      device.AcceleratorProcessReasonUnsupported,
		})}, cardMemory)
		require.NotNil(t, section)
		assert.Equal(t, MonitorSliceSchemaVersion, section.SchemaVersion)
		assert.Equal(t, []SlicePartition{{
			Manufacturer: testManufacturer, PodUID: testPodAUID,
			Container: testCtrName, DeviceID: testDeviceID,
			ID:             "MIG-aaa",
			MemoryTotalMiB: ptr.To[uint64](10 << 10),
			MemoryUsedMiB:  ptr.To[uint64](3 << 10),
			CoresReason:    device.AcceleratorProcessReasonUnsupported,
		}}, section.Partitions)
	})

	// The case the whole partition path exists for: a partition with nothing running in it was
	// measured and found idle, which is zero. A process-first lookup would have found no handle and
	// published an absence instead.
	t.Run("an idle partition reports zero, not absent", func(t *testing.T) {
		section := withPartitions(nil, []partitionRow{row(device.AcceleratorPartition{
			ID:               "MIG-aaa",
			MemoryTotalBytes: ptr.To[uint64](10 << 30),
			MemoryUsedBytes:  ptr.To[uint64](0),
		})}, cardMemory)
		require.NotNil(t, section)
		require.Len(t, section.Partitions, 1)
		require.NotNil(t, section.Partitions[0].MemoryUsedMiB)
		assert.Zero(t, *section.Partitions[0].MemoryUsedMiB)
	})

	t.Run("a partition nothing could be read from carries its reason and no identity", func(t *testing.T) {
		section := withPartitions(nil, []partitionRow{row(device.AcceleratorPartition{
			MemoryReason: device.AcceleratorProcessReasonDriverError,
		})}, cardMemory)
		require.NotNil(t, section)
		require.Len(t, section.Partitions, 1)
		assert.Empty(t, section.Partitions[0].ID)
		assert.Nil(t, section.Partitions[0].MemoryTotalMiB)
		assert.Nil(t, section.Partitions[0].MemoryUsedMiB)
		assert.Equal(t, device.AcceleratorProcessReasonDriverError, section.Partitions[0].MemoryReason)
	})

	t.Run("a figure above the whole card is refused, not clamped", func(t *testing.T) {
		section := withPartitions(nil, []partitionRow{row(device.AcceleratorPartition{
			ID:               "MIG-aaa",
			MemoryTotalBytes: ptr.To[uint64](10 << 30),
			MemoryUsedBytes:  ptr.To(uint64(testCardMemory+1) << 20),
		})}, cardMemory)
		require.NotNil(t, section)
		require.Len(t, section.Partitions, 1)
		assert.Nil(t, section.Partitions[0].MemoryTotalMiB)
		assert.Nil(t, section.Partitions[0].MemoryUsedMiB)
		assert.Equal(t, sliceReasonInvalidData, section.Partitions[0].MemoryReason)
	})

	// One read, one verdict: the capacity and the usage came off the same handle, so a capacity the
	// card contradicts discredits the usage beside it rather than only itself.
	t.Run("a capacity above the whole card takes the usage with it", func(t *testing.T) {
		section := withPartitions(nil, []partitionRow{row(device.AcceleratorPartition{
			ID:               "MIG-aaa",
			MemoryTotalBytes: ptr.To(uint64(testCardMemory+1) << 20),
			MemoryUsedBytes:  ptr.To[uint64](1 << 30),
		})}, cardMemory)
		require.NotNil(t, section)
		require.Len(t, section.Partitions, 1)
		assert.Nil(t, section.Partitions[0].MemoryTotalMiB)
		assert.Nil(t, section.Partitions[0].MemoryUsedMiB)
		assert.Equal(t, sliceReasonInvalidData, section.Partitions[0].MemoryReason)
	})

	t.Run("no partition leaves the section exactly as it was", func(t *testing.T) {
		assert.Nil(t, withPartitions(nil, nil, cardMemory))

		section := &MonitorSliceSection{SchemaVersion: MonitorSliceSchemaVersion}
		assert.Same(t, section, withPartitions(section, nil, cardMemory))
	})

	t.Run("the partitions are bounded and say so", func(t *testing.T) {
		rows := make([]partitionRow, 0, maxSliceUsages+8)
		for i := range maxSliceUsages + 8 {
			r := row(device.AcceleratorPartition{
				ID:               "MIG-aaa",
				MemoryTotalBytes: ptr.To[uint64](10 << 30),
				MemoryUsedBytes:  ptr.To[uint64](1 << 30),
			})
			r.podUID = fmt.Sprintf("%08d-89ab-cdef-0123-456789abcdef", i)
			rows = append(rows, r)
		}

		section := withPartitions(nil, rows, nil)
		require.NotNil(t, section)
		assert.True(t, section.Truncated)
		assert.Len(t, section.Partitions, maxSliceUsages)
	})
}

// TestFiguresPartition pins what a partition record answers, and that it answers INSTEAD of the
// per-process pass for its own key: the two measure the same memory on two different handles, and the
// partition's own is the one that survives a sibling tenant's unattributable process.
func TestFiguresPartition(t *testing.T) {
	partition := SlicePartition{
		Manufacturer: testManufacturer, PodUID: testPodAUID,
		Container: testCtrName, DeviceID: testDeviceID,
		ID:             "MIG-aaa",
		MemoryTotalMiB: ptr.To[uint64](10 << 10),
		MemoryUsedMiB:  ptr.To[uint64](2 << 10),
		CoresReason:    device.AcceleratorProcessReasonUnsupported,
	}

	t.Run("a partition answers for a device the per-process pass never covered", func(t *testing.T) {
		section := &MonitorSliceSection{
			SchemaVersion: MonitorSliceSchemaVersion,
			Partitions:    []SlicePartition{partition},
		}

		figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
		require.True(t, ok, "a partition alone is an answer")
		assert.Equal(t, "MIG-aaa", figures.ID)
		assert.Equal(t, ptr.To[uint64](10<<10), figures.MemoryTotalMiB)
		assert.Equal(t, ptr.To[uint64](2<<10), figures.MemoryUsedMiB)
		assert.Nil(t, figures.CoresUtilizationPercent,
			"no manufacturer serves a per-partition utilization, and none is invented")
	})

	t.Run("a partition supersedes a per-process record for the same key", func(t *testing.T) {
		section := &MonitorSliceSection{
			SchemaVersion: MonitorSliceSchemaVersion,
			Devices: []SliceDeviceDiagnostics{{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				RowsReturned: 1, RowsAttributed: 1,
			}},
			Usages: []SliceUsage{{
				Manufacturer: testManufacturer, PodUID: testPodAUID,
				Container: testCtrName, DeviceID: testDeviceID,
				MemoryUsedMiB:           ptr.To[uint64](9 << 10),
				CoresUtilizationPercent: ptr.To[uint32](40),
			}},
			Partitions: []SlicePartition{partition},
		}

		figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
		require.True(t, ok)
		assert.Equal(t, ptr.To[uint64](2<<10), figures.MemoryUsedMiB,
			"the partition's own handle, not the parent card's processes")
		assert.Nil(t, figures.CoresUtilizationPercent)
	})

	t.Run("a partition survives a device every vendor row poisoned", func(t *testing.T) {
		section := &MonitorSliceSection{
			SchemaVersion: MonitorSliceSchemaVersion,
			Devices: []SliceDeviceDiagnostics{{
				Manufacturer: testManufacturer, DeviceID: testDeviceID,
				MemoryReason: device.AcceleratorProcessReason(procattr.ReasonNoPodComponent),
				CoresReason:  device.AcceleratorProcessReason(procattr.ReasonNoPodComponent),
			}},
			Partitions: []SlicePartition{partition},
		}

		figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
		require.True(t, ok)
		assert.Equal(t, ptr.To[uint64](2<<10), figures.MemoryUsedMiB,
			"a process on a sibling partition is not this partition's to be poisoned by")
	})

	t.Run("another container's partition is not this one's", func(t *testing.T) {
		section := &MonitorSliceSection{
			SchemaVersion: MonitorSliceSchemaVersion,
			Partitions:    []SlicePartition{partition},
		}

		_, ok := section.Figures(testManufacturer, testPodBUID, testCtrName, testDeviceID)
		assert.False(t, ok)
		_, ok = section.Figures(testManufacturer, testPodAUID, "sidecar", testDeviceID)
		assert.False(t, ok)
	})

	t.Run("a section of an unknown schema answers nothing, partition or not", func(t *testing.T) {
		section := &MonitorSliceSection{
			SchemaVersion: MonitorSliceSchemaVersion + 1,
			Partitions:    []SlicePartition{partition},
		}

		figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
		assert.False(t, ok)
		assert.Empty(t, figures.ID)
		assert.Nil(t, figures.MemoryTotalMiB)
	})
}

// TestCollectSlicesPartitions runs the partition pass end to end. It needs no /proc tree at all,
// which is the pass's own point: the tenant comes from the allocation, so nothing has to be resolved
// from a process afterwards.
func TestCollectSlicesPartitions(t *testing.T) {
	metrics := device.MetricsGroupList{{
		Manufacturer: testManufacturer,
		Accelerators: []device.AcceleratorMetrics{{ID: testDeviceID, Memory: testCardMemory}},
	}}
	pods := func(_ context.Context) ([]core.Pod, error) {
		return []core.Pod{*partitionPodFixture(
			testPodAUID, testCtrAID, testDeviceID, "1g.10gb", testPlacement(0, 1)...)}, nil
	}
	answered := device.AcceleratorPartition{
		DeviceID:         testDeviceID,
		Profile:          "1g.10gb",
		Placements:       []device.AcceleratorPlacement{{Start: 0, Length: 1}},
		ID:               "MIG-aaa",
		MemoryTotalBytes: ptr.To[uint64](10 << 30),
		MemoryUsedBytes:  ptr.To[uint64](0),
		CoresReason:      device.AcceleratorProcessReasonUnsupported,
	}
	detectorWith := func(provider *fakePartitionDetector) *Detector {
		return &Detector{
			detectors: []device.Detector{provider},
			podLister: pods,
		}
	}

	t.Run("an idle partition reaches the section named, sized and measured at zero", func(t *testing.T) {
		provider := &fakePartitionDetector{
			fakeDetector: fakeDetector{name: testManufacturer},
			partitions:   []device.AcceleratorPartition{answered},
		}

		section := detectorWith(provider).collectSlices(t.Context(), metrics)
		require.NotNil(t, section)
		assert.Equal(t, []device.AcceleratorPartitionRequest{{
			DeviceID: testDeviceID, Profile: "1g.10gb",
			Placements: []device.AcceleratorPlacement{{Start: 0, Length: 1}},
		}}, provider.requested)

		figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
		require.True(t, ok)
		assert.Equal(t, "MIG-aaa", figures.ID)
		assert.Equal(t, ptr.To[uint64](10<<10), figures.MemoryTotalMiB)
		assert.Equal(t, ptr.To[uint64](0), figures.MemoryUsedMiB)
	})

	// The interface promises one answer per request. A manufacturer that breaks it must not have its
	// silence published as a partition of unknown size, so the record is simply not made.
	t.Run("a request nothing answered yields no record", func(t *testing.T) {
		section := detectorWith(&fakePartitionDetector{
			fakeDetector: fakeDetector{name: testManufacturer},
		}).collectSlices(t.Context(), metrics)
		assert.Nil(t, section)
	})

	t.Run("a manufacturer that failed yields no record", func(t *testing.T) {
		section := detectorWith(&fakePartitionDetector{
			fakeDetector: fakeDetector{name: testManufacturer},
			err:          errors.New("the management library is not loaded"),
		}).collectSlices(t.Context(), metrics)
		assert.Nil(t, section)
	})

	t.Run("a detector serving no partition query reports no partition", func(t *testing.T) {
		d := &Detector{
			detectors: []device.Detector{&fakeDetector{name: testManufacturer}},
			podLister: pods,
		}
		assert.Nil(t, d.collectSlices(t.Context(), metrics))
	})

	t.Run("a logical slice costs no partition call", func(t *testing.T) {
		provider := &fakePartitionDetector{
			fakeDetector: fakeDetector{name: testManufacturer},
			partitions:   []device.AcceleratorPartition{answered},
		}
		d := detectorWith(provider)
		d.podLister = func(_ context.Context) ([]core.Pod, error) {
			return []core.Pod{*instancePodFixture(
				testPodAUID, testCtrAID, workercore.DeviceAllocationModeSliced, testDeviceID)}, nil
		}

		d.collectSlices(t.Context(), metrics)
		assert.Nil(t, provider.requested)
	})
}

// TestKnownMonitorSliceSchema pins which producers a consumer of this build reads. The two run in
// separate Pods upgraded at separate times, so the range is what keeps a rollout from throwing away
// the figures an older device manager does serve.
func TestKnownMonitorSliceSchema(t *testing.T) {
	assert.True(t, KnownMonitorSliceSchema(MonitorSliceSchemaVersion))
	assert.True(t, KnownMonitorSliceSchema(MonitorSliceSchemaVersionMin))
	assert.False(t, KnownMonitorSliceSchema(MonitorSliceSchemaVersionMin-1),
		"a section with no version at all is not a section this build can read")
	assert.False(t, KnownMonitorSliceSchema(MonitorSliceSchemaVersion+1),
		"a newer producer's records cannot be read as what they are")
}

// TestFiguresSchemaSkew pins that a section at the OLDEST version this build reads is still read as
// what it is. The two sides run in separate Pods upgraded at separate times, so this is the case a
// range of accepted versions exists for: refusing it would throw away the figures the producer does
// serve, and report a measurable node as an unmeasurable one.
func TestFiguresSchemaSkew(t *testing.T) {
	section := &MonitorSliceSection{
		SchemaVersion: MonitorSliceSchemaVersionMin,
		Devices: []SliceDeviceDiagnostics{{
			Manufacturer: testManufacturer, DeviceID: testDeviceID,
			RowsReturned: 1, RowsAttributed: 1,
		}},
		Usages: []SliceUsage{{
			Manufacturer: testManufacturer, PodUID: testPodAUID,
			Container: testCtrName, DeviceID: testDeviceID,
			MemoryUsedMiB:           ptr.To[uint64](4 << 10),
			CoresUtilizationPercent: ptr.To[uint32](30),
		}},
	}

	figures, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
	require.True(t, ok, "an older producer this build knows how to read still answers")
	assert.Equal(t, ptr.To[uint64](4<<10), figures.MemoryUsedMiB)
	assert.Equal(t, ptr.To[uint32](30), figures.CoresUtilizationPercent)
}

// TestBuildSliceSectionProcessOnTwoDevices pins that one process holding two accelerators is
// attributed to each of them independently: two records, each carrying that device's own figure, and
// no figure counted twice.
//
// It is the shape a tensor-parallel worker takes — one pid per chip was measured on an eight-NPU host
// — and also the shape a single process spanning two cards takes. The resolver is asked per device
// rather than once for the node precisely so that this stays two answers rather than one, and a
// consumer joining by (Pod, container, device) would silently sum them if the producer folded them.
func TestBuildSliceSectionProcessOnTwoDevices(t *testing.T) {
	const secondDeviceID = "GPU-1"

	metrics := device.MetricsGroupList{{
		Manufacturer: testManufacturer,
		Accelerators: []device.AcceleratorMetrics{
			{ID: testDeviceID, Memory: testCardMemory},
			{ID: secondDeviceID, Memory: testCardMemory},
		},
	}}

	// ONE pid, reported by both device queries with a different figure on each — which is what a
	// process holding two cards looks like to two independent queries.
	procGroups := []device.AcceleratorProcessesGroup{{
		Manufacturer: testManufacturer,
		Accelerators: []device.AcceleratorProcesses{
			{ID: testDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](10)},
			}},
			{ID: secondDeviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](3 << 30), CoresPercent: ptr.To[uint32](30)},
			}},
		},
	}}

	var resolved int
	section := buildSliceSection(procGroups, metrics, testContainerNames(),
		func(pids []uint32) map[uint32]procattr.Result {
			resolved++
			return map[uint32]procattr.Result{100: attributed(testPodAUID, testCtrAID)}
		})
	require.NotNil(t, section)

	assert.Equal(t, 2, resolved,
		"the resolver is asked once per device, so one device's verdict cannot decide another's")
	assert.Equal(t, []SliceUsage{
		{
			Manufacturer: testManufacturer, PodUID: testPodAUID, Container: testCtrName,
			DeviceID:                testDeviceID,
			MemoryUsedMiB:           ptr.To[uint64](1 << 10),
			CoresUtilizationPercent: ptr.To[uint32](10),
		},
		{
			Manufacturer: testManufacturer, PodUID: testPodAUID, Container: testCtrName,
			DeviceID:                secondDeviceID,
			MemoryUsedMiB:           ptr.To[uint64](3 << 10),
			CoresUtilizationPercent: ptr.To[uint32](30),
		},
	}, section.Usages, "one record per device, each carrying that device's own figure")

	// And the consumer reads them apart, which is where a fold would have shown up as a doubled figure.
	first, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, testDeviceID)
	require.True(t, ok)
	second, ok := section.Figures(testManufacturer, testPodAUID, testCtrName, secondDeviceID)
	require.True(t, ok)
	assert.Equal(t, ptr.To[uint64](1<<10), first.MemoryUsedMiB)
	assert.Equal(t, ptr.To[uint64](3<<10), second.MemoryUsedMiB)
}
