package metax

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/binding/mxsml"
	"gpustack.ai/gpustack/pkg/device"
)

// TestDetectorServesProcesses pins that the detector this package constructs is the one the device
// manager's per-process pass will actually use. The pass reaches the query by a type assertion, so
// without this a drifted signature would leave every slice figure absent on hardware that answers.
func TestDetectorServesProcesses(t *testing.T) {
	assert.Implements(t, (*device.AcceleratorProcessDetector)(nil), New(device.DetectorOptions{}))
}

// TestMonitorAcceleratorProcessesUnread pins the interface's promise — one entry per requested
// device and no others — on the paths where nothing can be read at all: no MetaX PCI device answers
// while an allocation still names one, and the devices cannot be counted. Returning an empty group
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
			CoresReason:  device.AcceleratorProcessReasonUnsupported,
		},
		{
			ID:           "GPU-1",
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
	unread := unreadAcceleratorProcesses(sets.New("GPU-0", "GPU-1"), sets.New("GPU-0"))

	assert.Equal(t, []device.AcceleratorProcesses{{
		ID:           "GPU-1",
		MemoryReason: device.AcceleratorProcessReasonDriverError,
		CoresReason:  device.AcceleratorProcessReasonUnsupported,
	}}, unread)
	assert.Empty(t, unreadAcceleratorProcesses(sets.New("GPU-0"), sets.New("GPU-0")))
}

// TestAcceleratorProcessesOf pins how MXSML's per-device process answer becomes rows: a process
// naming several GPUs is attributed to this device only through the entry whose GpuId matches it,
// a successful query holding no rows is an idle device rather than an unmeasurable one, and compute
// is always reported unsupported because MXSML carries no per-process utilization query at all.
func TestAcceleratorProcessesOf(t *testing.T) {
	const deviceID = "GPU-0"
	const gpuID = uint32(0)

	rowFor := func(pid uint32, entries ...mxsml.ProcessGpuInfo_v3) mxsml.ProcessInfo_v3 {
		row := mxsml.ProcessInfo_v3{ProcessId: pid, GpuNumber: uint32(len(entries))}
		copy(row.ProcessGpuInfo[:], entries)
		return row
	}

	cases := []struct {
		name    string
		rows    []mxsml.ProcessInfo_v3
		rowsRet mxsml.Return
		want    device.AcceleratorProcesses
	}{
		{
			name: "a process is attributed through the entry naming this device's GpuId",
			rows: []mxsml.ProcessInfo_v3{
				rowFor(100, mxsml.ProcessGpuInfo_v3{GpuId: gpuID, GpuMemoryUsage: 1 << 20}),
				rowFor(200, mxsml.ProcessGpuInfo_v3{GpuId: 1, GpuMemoryUsage: 5 << 20}, mxsml.ProcessGpuInfo_v3{GpuId: gpuID, GpuMemoryUsage: 2 << 20}),
			},
			rowsRet: mxsml.Success,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes: []device.AcceleratorProcess{
					{PID: 100, MemoryBytes: ptr.To[uint64](1 << 20)},
					{PID: 200, MemoryBytes: ptr.To[uint64](2 << 20)},
				},
			},
		},
		{
			name: "a process naming no entry for this device is not reported for it",
			rows: []mxsml.ProcessInfo_v3{
				rowFor(100, mxsml.ProcessGpuInfo_v3{GpuId: 1, GpuMemoryUsage: 5 << 20}),
			},
			rowsRet: mxsml.Success,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes:   []device.AcceleratorProcess{},
			},
		},
		{
			name:    "an idle device answers with no rows and no memory reason",
			rowsRet: mxsml.Success,
			want: device.AcceleratorProcesses{
				ID:          deviceID,
				CoresReason: device.AcceleratorProcessReasonUnsupported,
				Processes:   []device.AcceleratorProcess{},
			},
		},
		{
			name:    "a driver that does not serve the query reports unsupported for memory too",
			rowsRet: mxsml.OperationNotSupport,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
				Processes:    []device.AcceleratorProcess{},
			},
		},
		{
			name:    "a list that could not be completed is truncated, never partial",
			rows:    []mxsml.ProcessInfo_v3{rowFor(100, mxsml.ProcessGpuInfo_v3{GpuId: gpuID, GpuMemoryUsage: 1 << 20})},
			rowsRet: mxsml.InsufficientSize,
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
			got := acceleratorProcessesOf(deviceID, gpuID, c.rows, c.rowsRet)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestProcessQueryReason pins the reason taxonomy: a refusal that will not clear on its own is told
// apart from one that may, and success — with rows or without — is never a refusal.
func TestProcessQueryReason(t *testing.T) {
	cases := []struct {
		ret  mxsml.Return
		want device.AcceleratorProcessReason
	}{
		{mxsml.Success, device.AcceleratorProcessReasonNone},
		{mxsml.OperationNotSupport, device.AcceleratorProcessReasonUnsupported},
		{mxsml.FunctionNotFound, device.AcceleratorProcessReasonUnsupported},
		{mxsml.PermissionDenied, device.AcceleratorProcessReasonPermission},
		{mxsml.InsufficientSize, device.AcceleratorProcessReasonTruncated},
		{mxsml.Failure, device.AcceleratorProcessReasonDriverError},
		{mxsml.IOControlFailure, device.AcceleratorProcessReasonDriverError},
	}

	for _, c := range cases {
		t.Run(c.ret.String(), func(t *testing.T) {
			assert.Equal(t, c.want, processQueryReason(c.ret))
		})
	}
}
