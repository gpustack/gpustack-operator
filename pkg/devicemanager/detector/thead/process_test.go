package thead

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/binding/hgml"
	"gpustack.ai/gpustack/pkg/device"
)

// notAvailable is the memory sentinel the library writes when it cannot measure a process's memory.
// It is spelled here the way a driver leaves it in the field — an unsigned wrap of -1 — so the test
// proves the sentinel is recognized rather than the helper being trusted to.
const notAvailable = ^uint64(0)

// TestDetectorServesProcesses pins that the detector this package constructs is the one the device
// manager's per-process pass will actually use. The pass reaches the query by a type assertion, so
// without this a drifted signature would leave every slice figure absent on hardware that answers.
func TestDetectorServesProcesses(t *testing.T) {
	assert.Implements(t, (*device.AcceleratorProcessDetector)(nil), New(device.DetectorOptions{}))
}

// TestMonitorAcceleratorProcessesUnread pins the interface's promise — one entry per requested
// device and no others — on the paths where nothing can be read at all: no THead PCI device answers
// while an allocation still names one, and the driver cannot be counted. Returning an empty group
// instead would produce no snapshot section, and an absence with no reason to explain it is exactly
// what this feature exists to prevent. It runs on any host, with or without a PPU.
func TestMonitorAcceleratorProcessesUnread(t *testing.T) {
	detector, ok := New(device.DetectorOptions{}).(device.AcceleratorProcessDetector)
	require.True(t, ok)

	requested := sets.New("PPU-0", "PPU-1")
	unread := []device.AcceleratorProcesses{
		{
			ID:           "PPU-0",
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonDriverError,
		},
		{
			ID:           "PPU-1",
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonDriverError,
		},
	}

	cases := []struct {
		name       string
		noPciCheck bool
		deviceIDs  sets.Set[string]
		want       []device.AcceleratorProcesses
	}{
		{
			name:       "no pci device answers for an allocated accelerator",
			noPciCheck: false,
			deviceIDs:  requested,
			want:       unread,
		},
		{
			// Skipping the PCI check reaches the driver, which on a host without one cannot be
			// counted — the same shape as a transient failure on a host with one.
			name:       "the driver cannot be counted",
			noPciCheck: true,
			deviceIDs:  requested,
			want:       unread,
		},
		{
			name:       "nothing requested is answered with nothing",
			noPciCheck: false,
			deviceIDs:  sets.New[string](),
			want:       nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			grp, err := detector.MonitorAcceleratorProcesses(c.noPciCheck, c.deviceIDs)
			require.NoError(t, err)
			assert.Equal(t, Manufacturer, grp.Manufacturer)
			assert.Equal(t, c.want, grp.Accelerators,
				"every requested device is reported, carrying a reason and no rows")
			for _, procs := range grp.Accelerators {
				assert.Empty(t, procs.Processes)
			}
		})
	}
}

// TestUnreadAcceleratorProcesses pins that only the devices left unanswered are synthesized, so a
// pass that read some of its devices does not report those twice.
func TestUnreadAcceleratorProcesses(t *testing.T) {
	unread := unreadAcceleratorProcesses(sets.New("PPU-0", "PPU-1"), sets.New("PPU-0"))

	assert.Equal(t, []device.AcceleratorProcesses{{
		ID:           "PPU-1",
		MemoryReason: device.AcceleratorProcessReasonDriverError,
		CoresReason:  device.AcceleratorProcessReasonDriverError,
	}}, unread)
	assert.Empty(t, unreadAcceleratorProcesses(sets.New("PPU-0"), sets.New("PPU-0")))
}

// TestAcceleratorProcessesOf pins how one device's two HGML answers become rows: the memory sentinel
// is carried as "no number" and never as a saturated figure, an entry point that refused is named
// while the other one still reports, and a successful query holding no rows is an idle device rather
// than an unmeasurable one.
func TestAcceleratorProcessesOf(t *testing.T) {
	const deviceID = "PPU-0"

	cases := []struct {
		name      string
		infos     []hgml.ProcessInfo
		infoRet   hgml.Return
		samples   []hgml.ProcessUtilizationSample
		sampleRet hgml.Return
		want      device.AcceleratorProcesses
	}{
		{
			name:      "both queries answer, and a process with no sample is idle",
			infos:     []hgml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}, {Pid: 200, UsedGpuMemory: 2 << 30}},
			infoRet:   hgml.SUCCESS,
			samples:   []hgml.ProcessUtilizationSample{{Pid: 100, TimeStamp: 10, SmUtil: 35}},
			sampleRet: hgml.SUCCESS,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](35)},
				{PID: 200, MemoryBytes: ptr.To[uint64](2 << 30), CoresPercent: ptr.To[uint32](0)},
			}},
		},
		{
			name:      "the memory sentinel is no number, not the largest one",
			infos:     []hgml.ProcessInfo{{Pid: 100, UsedGpuMemory: notAvailable}},
			infoRet:   hgml.SUCCESS,
			sampleRet: hgml.SUCCESS,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				// The memory sentinel is no number; the compute figure beside it is a measured zero,
				// because the utilization query answered and named no sample for this pid.
				{PID: 100, CoresPercent: ptr.To[uint32](0)},
			}},
		},
		{
			name:      "an idle device answers with no rows and no reason",
			infoRet:   hgml.SUCCESS,
			sampleRet: hgml.SUCCESS,
			want:      device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{}},
		},
		{
			name:      "no utilization sample in the window is idle, not unsupported",
			infos:     []hgml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}},
			infoRet:   hgml.SUCCESS,
			sampleRet: hgml.ERROR_NOT_FOUND,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](0)},
			}},
		},
		{
			name:      "only the newest sample of a process contributes",
			infos:     []hgml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}},
			infoRet:   hgml.SUCCESS,
			samples:   []hgml.ProcessUtilizationSample{{Pid: 100, TimeStamp: 10, SmUtil: 90}, {Pid: 100, TimeStamp: 20, SmUtil: 12}},
			sampleRet: hgml.SUCCESS,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](12)},
			}},
		},
		{
			name:      "a driver refusing process memory still reports the utilization it serves",
			infoRet:   hgml.ERROR_NOT_SUPPORTED,
			samples:   []hgml.ProcessUtilizationSample{{Pid: 100, TimeStamp: 10, SmUtil: 35}},
			sampleRet: hgml.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, CoresPercent: ptr.To[uint32](35)},
				},
			},
		},
		{
			name:      "a driver refusing process utilization still reports the memory it serves",
			infos:     []hgml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}},
			infoRet:   hgml.SUCCESS,
			samples:   []hgml.ProcessUtilizationSample{{Pid: 100, TimeStamp: 10, SmUtil: 35}},
			sampleRet: hgml.ERROR_NOT_SUPPORTED,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30)},
				},
			},
		},
		{
			name:      "a pid only the sample buffer names is not held against the device",
			infos:     []hgml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}},
			infoRet:   hgml.SUCCESS,
			samples:   []hgml.ProcessUtilizationSample{{Pid: 999, TimeStamp: 10, SmUtil: 35}},
			sampleRet: hgml.SUCCESS,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](0)},
			}},
		},
		{
			name:      "a list that could not be completed is truncated, never partial",
			infoRet:   hgml.ERROR_INSUFFICIENT_SIZE,
			sampleRet: hgml.ERROR_INSUFFICIENT_SIZE,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonTruncated,
				CoresReason:  device.AcceleratorProcessReasonTruncated,
				Processes:    []device.AcceleratorProcess{},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := acceleratorProcessesOf(deviceID, c.infos, c.infoRet, c.samples, c.sampleRet)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestNewestUtilizationByPID pins that only the newest sample of a process contributes: the library
// keeps several timestamped samples per process, and summing them would report one process's
// activity as several processes'.
func TestNewestUtilizationByPID(t *testing.T) {
	cases := []struct {
		name    string
		samples []hgml.ProcessUtilizationSample
		want    map[uint32]uint32
	}{
		{
			name: "the newest sample wins, whichever order they arrive in",
			samples: []hgml.ProcessUtilizationSample{
				{Pid: 100, TimeStamp: 30, SmUtil: 11},
				{Pid: 100, TimeStamp: 10, SmUtil: 99},
				{Pid: 100, TimeStamp: 20, SmUtil: 55},
			},
			want: map[uint32]uint32{100: 11},
		},
		{
			name: "each process keeps its own newest sample",
			samples: []hgml.ProcessUtilizationSample{
				{Pid: 100, TimeStamp: 10, SmUtil: 20},
				{Pid: 200, TimeStamp: 20, SmUtil: 40},
				{Pid: 100, TimeStamp: 20, SmUtil: 30},
			},
			want: map[uint32]uint32{100: 30, 200: 40},
		},
		{
			name: "a newest sample of zero is a measurement, not an absence",
			samples: []hgml.ProcessUtilizationSample{
				{Pid: 100, TimeStamp: 10, SmUtil: 80},
				{Pid: 100, TimeStamp: 20, SmUtil: 0},
			},
			want: map[uint32]uint32{100: 0},
		},
		{
			name: "no sample at all yields no entry",
			want: map[uint32]uint32{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, newestUtilizationByPID(c.samples))
		})
	}
}

// TestProcessQueryReason pins the reason taxonomy: a refusal that will not clear on its own is told
// apart from a failure that may, and neither an empty answer nor an empty sample window is reported
// as an absence — those are measured idleness.
func TestProcessQueryReason(t *testing.T) {
	cases := []struct {
		name string
		ret  hgml.Return
		want device.AcceleratorProcessReason
	}{
		{"a successful query is no refusal", hgml.SUCCESS, device.AcceleratorProcessReasonNone},
		{"no sample in the window is measured idleness", hgml.ERROR_NOT_FOUND, device.AcceleratorProcessReasonNone},
		{"a driver that does not serve the query is unsupported", hgml.ERROR_NOT_SUPPORTED, device.AcceleratorProcessReasonUnsupported},
		{"a symbol the library does not carry is unsupported", hgml.ERROR_FUNCTION_NOT_FOUND, device.AcceleratorProcessReasonUnsupported},
		{"a query needing privileges is permission", hgml.ERROR_NO_PERMISSION, device.AcceleratorProcessReasonPermission},
		{"a list that would not fit is truncated", hgml.ERROR_INSUFFICIENT_SIZE, device.AcceleratorProcessReasonTruncated},
		{"anything else may not repeat", hgml.ERROR_UNKNOWN, device.AcceleratorProcessReasonDriverError},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, processQueryReason(c.ret))
		})
	}
}
