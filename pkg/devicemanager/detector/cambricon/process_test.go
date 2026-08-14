package cambricon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/binding/cndev"
	"gpustack.ai/gpustack/pkg/device"
)

// TestDetectorServesProcesses pins that the detector this package constructs is the one the device
// manager's per-process pass will actually use. The pass reaches the query by a type assertion, so
// without this a drifted signature would leave every slice figure absent on hardware that answers.
func TestDetectorServesProcesses(t *testing.T) {
	assert.Implements(t, (*device.AcceleratorProcessDetector)(nil), New(device.DetectorOptions{}))
}

// TestMonitorAcceleratorProcessesUnread pins the interface's promise — one entry per requested
// device and no others — on the paths where nothing can be read at all: no Cambricon PCI device
// answers while an allocation still names one, and the devices cannot be counted. Returning an
// empty group instead would produce no snapshot section, and an absence with no reason to explain
// it is exactly what this feature exists to prevent. It runs on any host, with or without an MLU.
func TestMonitorAcceleratorProcessesUnread(t *testing.T) {
	detector, ok := New(device.DetectorOptions{}).(device.AcceleratorProcessDetector)
	require.True(t, ok)

	requested := sets.New("MLU-0", "MLU-1")
	unread := []device.AcceleratorProcesses{
		{
			ID:           "MLU-0",
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonDriverError,
		},
		{
			ID:           "MLU-1",
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
			// Skipping the PCI check reaches the driver, whose devices cannot be counted on a host
			// without one — the same shape as a transient failure on a host with one.
			name:       "the devices cannot be counted",
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
	unread := unreadAcceleratorProcesses(sets.New("MLU-0", "MLU-1"), sets.New("MLU-0"))

	assert.Equal(t, []device.AcceleratorProcesses{{
		ID:           "MLU-1",
		MemoryReason: device.AcceleratorProcessReasonDriverError,
		CoresReason:  device.AcceleratorProcessReasonDriverError,
	}}, unread)
	assert.Empty(t, unreadAcceleratorProcesses(sets.New("MLU-0"), sets.New("MLU-0")))
}

// TestAcceleratorProcessesOf pins how one device's two CNDev answers become rows: Cambricon is the
// one of the four hardware-less manufacturers in this task whose binding exposes both memory and
// per-process compute, a query that refused is named while the other one still reports, and a
// successful query holding no rows is an idle device rather than an unmeasurable one.
func TestAcceleratorProcessesOf(t *testing.T) {
	const deviceID = "MLU-0"

	cases := []struct {
		name    string
		infos   []cndev.ProcessInfo
		infoRet cndev.Return
		utils   []cndev.ProcessUtilization
		utilRet cndev.Return
		want    device.AcceleratorProcesses
	}{
		{
			name:    "both queries answer, and a process with no utilization row is idle",
			infos:   []cndev.ProcessInfo{{Pid: 100, PhysicalMemoryUsed: 1 << 20}, {Pid: 200, PhysicalMemoryUsed: 2 << 20}},
			infoRet: cndev.SUCCESS,
			utils:   []cndev.ProcessUtilization{{Pid: 100, IpuUtil: 35}},
			utilRet: cndev.SUCCESS,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](35)},
				// Measured and idle: the library answered for this pid and named no utilization, and
				// on this library that means the process was not busy. Stated as a zero so the
				// aggregator does not read it as a figure nobody could take.
				{PID: 200, MemoryBytes: ptr.To[uint64](2 << 30), CoresPercent: ptr.To[uint32](0)},
			}},
		},
		{
			name:    "an idle device answers with no rows and no reason",
			infoRet: cndev.SUCCESS,
			utilRet: cndev.SUCCESS,
			want:    device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{}},
		},
		{
			name:    "a driver serving memory but not utilization reports the half it serves",
			infos:   []cndev.ProcessInfo{{Pid: 100, PhysicalMemoryUsed: 1 << 20}},
			infoRet: cndev.SUCCESS,
			utilRet: cndev.ERROR_NOT_SUPPORTED,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30)},
				},
			},
		},
		{
			name:    "a driver serving utilization but not the process list still reports compute",
			infoRet: cndev.ERROR_NOT_SUPPORTED,
			utils:   []cndev.ProcessUtilization{{Pid: 100, IpuUtil: 42}},
			utilRet: cndev.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, CoresPercent: ptr.To[uint32](42)},
				},
			},
		},
		{
			name:    "a whole generation answering neither query is unsupported on both",
			infoRet: cndev.ERROR_NOT_SUPPORTED,
			utilRet: cndev.ERROR_NOT_SUPPORTED,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
				Processes:    []device.AcceleratorProcess{},
			},
		},
		{
			name:    "a list that could not be completed is truncated, never partial",
			infos:   []cndev.ProcessInfo{{Pid: 100, PhysicalMemoryUsed: 1 << 20}},
			infoRet: cndev.ERROR_INSUFFICIENT_SPACE,
			utilRet: cndev.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonTruncated,
				Processes:    []device.AcceleratorProcess{},
			},
		},
		{
			name:    "a utilization row naming a process the list does not is dropped",
			infos:   []cndev.ProcessInfo{{Pid: 100, PhysicalMemoryUsed: 1 << 20}},
			infoRet: cndev.SUCCESS,
			utils:   []cndev.ProcessUtilization{{Pid: 999, IpuUtil: 90}},
			utilRet: cndev.SUCCESS,
			want: device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{
				{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](0)},
			}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := acceleratorProcessesOf(deviceID, c.infos, c.infoRet, c.utils, c.utilRet)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestCoresByPID pins that per-process compute is read straight off CNDev's IpuUtil field, one row
// per process, with no folding: unlike some other manufacturers' libraries, CNDev's per-process
// utilization query carries no timestamped sample buffer to reduce.
func TestCoresByPID(t *testing.T) {
	cases := []struct {
		name  string
		utils []cndev.ProcessUtilization
		want  map[uint32]uint32
	}{
		{
			name:  "each process is kept apart",
			utils: []cndev.ProcessUtilization{{Pid: 100, IpuUtil: 20}, {Pid: 200, IpuUtil: 30}},
			want:  map[uint32]uint32{100: 20, 200: 30},
		},
		{
			name:  "a reported zero is a measurement",
			utils: []cndev.ProcessUtilization{{Pid: 100, IpuUtil: 0}},
			want:  map[uint32]uint32{100: 0},
		},
		{
			name:  "no row at all is no entry, which the caller reads as idle",
			utils: nil,
			want:  map[uint32]uint32{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, coresByPID(c.utils))
		})
	}
}

// TestProcessQueryReason pins the reason taxonomy: a refusal that will not clear on its own is told
// apart from one that may, and success — with rows or without — is never a refusal.
func TestProcessQueryReason(t *testing.T) {
	cases := []struct {
		ret  cndev.Return
		want device.AcceleratorProcessReason
	}{
		{cndev.SUCCESS, device.AcceleratorProcessReasonNone},
		{cndev.ERROR_NOT_SUPPORTED, device.AcceleratorProcessReasonUnsupported},
		{cndev.ERROR_LIBRARY_NOT_FOUND, device.AcceleratorProcessReasonUnsupported},
		{cndev.ERROR_NO_PERMISSION, device.AcceleratorProcessReasonPermission},
		{cndev.ERROR_INSUFFICIENT_SPACE, device.AcceleratorProcessReasonTruncated},
		{cndev.ERROR_UNKNOWN, device.AcceleratorProcessReasonDriverError},
		{cndev.ERROR_UNINITIALIZED, device.AcceleratorProcessReasonDriverError},
	}

	for _, c := range cases {
		t.Run(c.ret.String(), func(t *testing.T) {
			assert.Equal(t, c.want, processQueryReason(c.ret))
		})
	}
}
