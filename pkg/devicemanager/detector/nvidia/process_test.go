package nvidia

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/device"
)

// notAvailable is the memory sentinel NVML writes when it cannot measure a process's memory. It is
// spelled here the way a driver leaves it in the field — an unsigned wrap of -1 — so the test proves
// the sentinel is recognized rather than the helper being trusted to.
const notAvailable = ^uint64(0)

// TestDetectorServesProcesses pins that the detector this package constructs is the one the device
// manager's per-process pass will actually use. The pass reaches the query by a type assertion, so
// without this a drifted signature would leave every slice figure absent on hardware that answers.
func TestDetectorServesProcesses(t *testing.T) {
	assert.Implements(t, (*device.AcceleratorProcessDetector)(nil), New(device.DetectorOptions{}))
}

// TestMonitorAcceleratorProcessesUnread pins the interface's promise — one entry per requested
// device and no others — on the paths where nothing can be read at all: no NVIDIA PCI device answers
// while an allocation still names one, and the driver cannot be counted. Returning an empty group
// instead would produce no snapshot section, and an absence with no reason to explain it is exactly
// what this feature exists to prevent. It runs on any host, with or without a GPU.
func TestMonitorAcceleratorProcessesUnread(t *testing.T) {
	detector, ok := New(device.DetectorOptions{}).(device.AcceleratorProcessDetector)
	require.True(t, ok)

	requested := sets.New("GPU-0", "GPU-1")
	unread := []device.AcceleratorProcesses{
		{
			ID:           "GPU-0",
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonDriverError,
		},
		{
			ID:           "GPU-1",
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
	unread := unreadAcceleratorProcesses(sets.New("GPU-0", "GPU-1"), sets.New("GPU-0"))

	assert.Equal(t, []device.AcceleratorProcesses{{
		ID:           "GPU-1",
		MemoryReason: device.AcceleratorProcessReasonDriverError,
		CoresReason:  device.AcceleratorProcessReasonDriverError,
	}}, unread)
	assert.Empty(t, unreadAcceleratorProcesses(sets.New("GPU-0"), sets.New("GPU-0")))
}

// TestAcceleratorProcessesOf pins how one device's two NVML answers become rows: the memory sentinel
// is carried as "no number" and never as a saturated figure, an entry point that refused is named
// while the other one still reports, and a successful query holding no rows is an idle device rather
// than an unmeasurable one.
func TestAcceleratorProcessesOf(t *testing.T) {
	const deviceID = "GPU-0"

	cases := []struct {
		name      string
		infos     []nvml.ProcessInfo
		infoRet   nvml.Return
		samples   []nvml.ProcessUtilizationSample
		sampleRet nvml.Return
		want      device.AcceleratorProcesses
	}{
		{
			name:      "both queries answer, and a process with no sample is idle",
			infos:     []nvml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}, {Pid: 200, UsedGpuMemory: 2 << 30}},
			infoRet:   nvml.SUCCESS,
			samples:   []nvml.ProcessUtilizationSample{{Pid: 100, TimeStamp: 10, SmUtil: 35}},
			sampleRet: nvml.SUCCESS,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](35)},
				{PID: 200, MemoryBytes: ptr.To[uint64](2 << 30), CoresPercent: ptr.To[uint32](0)},
			}},
		},
		{
			name:      "the memory sentinel is no number, not the largest one",
			infos:     []nvml.ProcessInfo{{Pid: 100, UsedGpuMemory: notAvailable}},
			infoRet:   nvml.SUCCESS,
			sampleRet: nvml.SUCCESS,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				// The memory sentinel is no number; the compute figure beside it is a measured zero,
				// because the utilization query answered and named no sample for this pid.
				{PID: 100, CoresPercent: ptr.To[uint32](0)},
			}},
		},
		{
			name:      "an idle device answers with no rows and no reason",
			infoRet:   nvml.SUCCESS,
			sampleRet: nvml.SUCCESS,
			want:      device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{}},
		},
		{
			name:      "no utilization sample in the window is idle, not unsupported",
			infos:     []nvml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}},
			infoRet:   nvml.SUCCESS,
			sampleRet: nvml.ERROR_NOT_FOUND,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](0)},
			}},
		},
		{
			name:      "a driver serving memory but not utilization reports the half it serves",
			infos:     []nvml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}},
			infoRet:   nvml.SUCCESS,
			sampleRet: nvml.ERROR_NOT_SUPPORTED,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30)},
				},
			},
		},
		{
			name:      "a driver serving utilization but not the process list still reports compute",
			infoRet:   nvml.ERROR_NOT_SUPPORTED,
			samples:   []nvml.ProcessUtilizationSample{{Pid: 100, TimeStamp: 10, SmUtil: 42}},
			sampleRet: nvml.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, CoresPercent: ptr.To[uint32](42)},
				},
			},
		},
		{
			name:      "a whole generation answering neither query is unsupported on both",
			infoRet:   nvml.ERROR_NOT_SUPPORTED,
			sampleRet: nvml.ERROR_NOT_SUPPORTED,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
				Processes:    []device.AcceleratorProcess{},
			},
		},
		{
			name:      "a list that could not be completed is truncated, never partial",
			infos:     []nvml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}},
			infoRet:   nvml.ERROR_INSUFFICIENT_SIZE,
			sampleRet: nvml.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonTruncated,
				Processes:    []device.AcceleratorProcess{},
			},
		},
		{
			name:      "a sample naming a process the list does not is dropped",
			infos:     []nvml.ProcessInfo{{Pid: 100, UsedGpuMemory: 1 << 30}},
			infoRet:   nvml.SUCCESS,
			samples:   []nvml.ProcessUtilizationSample{{Pid: 999, TimeStamp: 10, SmUtil: 90}},
			sampleRet: nvml.SUCCESS,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](0)},
			}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := acceleratorProcessesOf(deviceID, c.infos, c.infoRet, c.samples, c.sampleRet)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestNewestUtilizationByPID pins that only the newest sample of a process contributes: NVML keeps
// several timestamped samples per process, and summing them would report one process's activity as
// several processes'.
func TestNewestUtilizationByPID(t *testing.T) {
	cases := []struct {
		name    string
		samples []nvml.ProcessUtilizationSample
		want    map[uint32]uint32
	}{
		{
			name: "the newest sample wins, whichever order they arrive in",
			samples: []nvml.ProcessUtilizationSample{
				{Pid: 100, TimeStamp: 30, SmUtil: 15},
				{Pid: 100, TimeStamp: 10, SmUtil: 90},
				{Pid: 100, TimeStamp: 20, SmUtil: 70},
			},
			want: map[uint32]uint32{100: 15},
		},
		{
			name: "each process is kept apart",
			samples: []nvml.ProcessUtilizationSample{
				{Pid: 100, TimeStamp: 10, SmUtil: 20},
				{Pid: 200, TimeStamp: 11, SmUtil: 30},
			},
			want: map[uint32]uint32{100: 20, 200: 30},
		},
		{
			name: "a newest sample of zero is a measurement",
			samples: []nvml.ProcessUtilizationSample{
				{Pid: 100, TimeStamp: 10, SmUtil: 80},
				{Pid: 100, TimeStamp: 20, SmUtil: 0},
			},
			want: map[uint32]uint32{100: 0},
		},
		{
			name:    "no sample at all is no entry, which the caller reads as idle",
			samples: nil,
			want:    map[uint32]uint32{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, newestUtilizationByPID(c.samples))
		})
	}
}

// TestProcessQueryReason pins the reason taxonomy: a refusal that will not clear on its own is told
// apart from one that may, and success — with rows or without — is never a refusal.
func TestProcessQueryReason(t *testing.T) {
	cases := []struct {
		ret  nvml.Return
		want device.AcceleratorProcessReason
	}{
		{nvml.SUCCESS, device.AcceleratorProcessReasonNone},
		{nvml.ERROR_NOT_FOUND, device.AcceleratorProcessReasonNone},
		{nvml.ERROR_NOT_SUPPORTED, device.AcceleratorProcessReasonUnsupported},
		{nvml.ERROR_FUNCTION_NOT_FOUND, device.AcceleratorProcessReasonUnsupported},
		{nvml.ERROR_NO_PERMISSION, device.AcceleratorProcessReasonPermission},
		{nvml.ERROR_INSUFFICIENT_SIZE, device.AcceleratorProcessReasonTruncated},
		{nvml.ERROR_GPU_IS_LOST, device.AcceleratorProcessReasonDriverError},
		{nvml.ERROR_UNINITIALIZED, device.AcceleratorProcessReasonDriverError},
	}

	for _, c := range cases {
		t.Run(c.ret.String(), func(t *testing.T) {
			assert.Equal(t, c.want, processQueryReason(c.ret))
		})
	}
}
