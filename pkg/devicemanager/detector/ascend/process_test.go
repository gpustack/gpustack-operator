package ascend

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/binding/dcmi"
	"gpustack.ai/gpustack/pkg/device"
)

// TestDetectorServesProcesses pins that the detector this package constructs is the one the device
// manager's per-process pass will actually use. The pass reaches the query by a type assertion, so
// without this a drifted signature would leave every slice figure absent on hardware that answers.
func TestDetectorServesProcesses(t *testing.T) {
	assert.Implements(t, (*device.AcceleratorProcessDetector)(nil), New(device.DetectorOptions{}))
}

// TestMonitorAcceleratorProcessesUnread pins the interface's promise — one entry per requested
// device and no others — on the paths where nothing can be read at all: no Ascend PCI device answers
// while an allocation still names one, and the cards cannot be listed. Returning an empty group
// instead would produce no snapshot section, and an absence with no reason to explain it is exactly
// what this feature exists to prevent. It runs on any host, with or without an NPU.
func TestMonitorAcceleratorProcessesUnread(t *testing.T) {
	detector, ok := New(device.DetectorOptions{}).(device.AcceleratorProcessDetector)
	require.True(t, ok)

	requested := sets.New("NPU-0", "NPU-1")
	unread := []device.AcceleratorProcesses{
		{
			ID:           "NPU-0",
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonUnsupported,
		},
		{
			ID:           "NPU-1",
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonUnsupported,
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
			// Skipping the PCI check reaches the driver, whose cards cannot be listed on a host
			// without one — the same shape as a transient failure on a host with one.
			name:       "the cards cannot be listed",
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
	unread := unreadAcceleratorProcesses(sets.New("NPU-0", "NPU-1"), sets.New("NPU-0"))

	assert.Equal(t, []device.AcceleratorProcesses{{
		ID:           "NPU-1",
		MemoryReason: device.AcceleratorProcessReasonDriverError,
		CoresReason:  device.AcceleratorProcessReasonUnsupported,
	}}, unread)
	assert.Empty(t, unreadAcceleratorProcesses(sets.New("NPU-0"), sets.New("NPU-0")))
}

// TestAcceleratorProcessesOf pins how one device's DCMI answer becomes rows: memory is carried in
// the library's own bytes, compute is always unsupported because no query exists to serve it, a
// successful query holding no rows is an idle device rather than an unmeasurable one, and a row the
// read cannot interpret makes the device's memory absent instead of partial.
func TestAcceleratorProcessesOf(t *testing.T) {
	const deviceID = "NPU-0"

	cases := []struct {
		name    string
		rows    []dcmi.ProcMemInfo
		rowsRet dcmi.Return
		want    device.AcceleratorProcesses
	}{
		{
			name: "a successful query reports each process's memory in bytes",
			rows: []dcmi.ProcMemInfo{
				{Id: 899293, Mem_usage: 57280 << 20},
				{Id: 899294, Mem_usage: 1 << 30},
			},
			rowsRet: dcmi.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 899293, MemoryBytes: ptr.To[uint64](57280 << 20)},
					{PID: 899294, MemoryBytes: ptr.To[uint64](1 << 30)},
				},
			},
		},
		{
			name:    "a sub-MiB figure is carried as the byte count it is",
			rows:    []dcmi.ProcMemInfo{{Id: 100, Mem_usage: 4096}},
			rowsRet: dcmi.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, MemoryBytes: ptr.To[uint64](4096)},
				},
			},
		},
		{
			name:    "a process measured holding nothing is a zero, not an absence",
			rows:    []dcmi.ProcMemInfo{{Id: 100, Mem_usage: 0}},
			rowsRet: dcmi.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, MemoryBytes: ptr.To[uint64](0)},
				},
			},
		},
		{
			name:    "an idle device answers with no rows and no memory reason",
			rowsRet: dcmi.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes:   []device.AcceleratorProcess{},
			},
		},
		{
			name: "a row the read cannot interpret makes the whole device's memory absent",
			rows: []dcmi.ProcMemInfo{
				{Id: 100, Mem_usage: 1 << 30},
				{Id: -1, Mem_usage: 1 << 30},
			},
			rowsRet: dcmi.SUCCESS,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonDriverError,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
				Processes:    []device.AcceleratorProcess{},
			},
		},
		{
			name:    "a driver that does not serve the query reports unsupported memory",
			rowsRet: dcmi.ERROR_NOT_SUPPORT,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
				Processes:    []device.AcceleratorProcess{},
			},
		},
		{
			name:    "a list that could not be completed is truncated, never partial",
			rows:    []dcmi.ProcMemInfo{{Id: 100, Mem_usage: 1 << 30}},
			rowsRet: dcmi.ERROR_LIST_TRUNCATED,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonTruncated,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
				Processes:    []device.AcceleratorProcess{},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := acceleratorProcessesOf(deviceID, c.rows, c.rowsRet)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestProcessQueryReason pins the reason taxonomy: a refusal that will not clear on its own is told
// apart from a failure that may, so a capability probe never concludes "unsupported" from one bad
// pass.
func TestProcessQueryReason(t *testing.T) {
	cases := []struct {
		name string
		ret  dcmi.Return
		want device.AcceleratorProcessReason
	}{
		{"a successful query is no refusal", dcmi.SUCCESS, device.AcceleratorProcessReasonNone},
		{"a driver that does not serve the query is unsupported", dcmi.ERROR_NOT_SUPPORT, device.AcceleratorProcessReasonUnsupported},
		{"a query the container cannot make is unsupported", dcmi.ERROR_NOT_SUPPORT_IN_CONTAINER, device.AcceleratorProcessReasonUnsupported},
		{"a symbol the library does not carry is unsupported", dcmi.ERROR_FUNCTION_NOT_FOUND, device.AcceleratorProcessReasonUnsupported},
		{"an operation not permitted is permission", dcmi.ERROR_OPER_NOT_PERMITTED, device.AcceleratorProcessReasonPermission},
		{"a list that would not fit is truncated", dcmi.ERROR_LIST_TRUNCATED, device.AcceleratorProcessReasonTruncated},
		{"anything else may not repeat", dcmi.ERROR_INVALID_PARAMETER, device.AcceleratorProcessReasonDriverError},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, processQueryReason(c.ret))
		})
	}
}
