package hygon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/binding/rsmi"
	"gpustack.ai/gpustack/pkg/device"
)

// kfdStatsInvalid mirrors the header's KFD_STATS_INVALID the way a driver leaves it in the field —
// the literal it is defined as — so the test proves the sentinel is recognized rather than the
// predicate being trusted to.
const kfdStatsInvalidForTest = 0xFFFFFFFF

// TestDetectorServesProcesses pins that the detector this package constructs is the one the device
// manager's per-process pass will actually use. The pass reaches the query by a type assertion, so
// without this a drifted signature would leave every slice figure absent on hardware that answers.
func TestDetectorServesProcesses(t *testing.T) {
	assert.Implements(t, (*device.AcceleratorProcessDetector)(nil), New(device.DetectorOptions{}))
}

// TestMonitorAcceleratorProcessesUnread pins the interface's promise — one entry per requested
// device and no others — on the paths where nothing can be read at all: no Hygon PCI device answers
// while an allocation still names one, and the devices cannot be counted. Returning an empty group
// instead would produce no snapshot section, and an absence with no reason to explain it is exactly
// what this feature exists to prevent. It runs on any host, with or without a DCU.
func TestMonitorAcceleratorProcessesUnread(t *testing.T) {
	detector, ok := New(device.DetectorOptions{}).(device.AcceleratorProcessDetector)
	require.True(t, ok)

	requested := sets.New("DCU-0", "DCU-1")
	unread := []device.AcceleratorProcesses{
		{
			ID:           "DCU-0",
			MemoryReason: device.AcceleratorProcessReasonDriverError,
			CoresReason:  device.AcceleratorProcessReasonDriverError,
		},
		{
			ID:           "DCU-1",
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
	unread := unreadAcceleratorProcesses(sets.New("DCU-0", "DCU-1"), sets.New("DCU-0"))

	assert.Equal(t, []device.AcceleratorProcesses{{
		ID:           "DCU-1",
		MemoryReason: device.AcceleratorProcessReasonDriverError,
		CoresReason:  device.AcceleratorProcessReasonDriverError,
	}}, unread)
	assert.Empty(t, unreadAcceleratorProcesses(sets.New("DCU-0"), sets.New("DCU-0")))
}

// TestProcessInfo_CuOccupancyAvailable pins the boundary of the sentinel: the value one below the
// invalidation constant is a measurement, and the constant itself is never mistaken for one, at the
// exact value a driver would leave in the field.
func TestProcessInfo_CuOccupancyAvailable(t *testing.T) {
	cases := []struct {
		name  string
		value uint32
		want  bool
	}{
		{"the value one below the sentinel is a measurement", kfdStatsInvalidForTest - 1, true},
		{"a measured zero is a measurement", 0, true},
		{"the sentinel itself is not a measurement", kfdStatsInvalidForTest, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := rsmi.ProcessInfo{Cu_occupancy: c.value}
			assert.Equal(t, c.want, p.CuOccupancyAvailable())
		})
	}
}

// TestAcceleratorProcessesOf pins how RSMI's compute-process answer becomes rows: memory and
// compute share one Return, so a device the query could not answer for is unsupported (or whatever
// other reason applies) on both alike; a row whose cu_occupancy reads the invalidation sentinel
// still reports memory and leaves CoresPercent absent rather than a fabricated figure, and one such
// row does not turn the rest of the device's rows unmeasurable.
func TestAcceleratorProcessesOf(t *testing.T) {
	const deviceID = "DCU-0"

	cases := []struct {
		name    string
		infos   []rsmi.ProcessInfo
		infoRet rsmi.Return
		want    device.AcceleratorProcesses
	}{
		{
			name: "a successful query reports memory and compute per process",
			infos: []rsmi.ProcessInfo{
				{Process_id: 100, Vram_usage: 1 << 30, Cu_occupancy: 35},
				{Process_id: 200, Vram_usage: 2 << 30, Cu_occupancy: 70},
			},
			infoRet: rsmi.STATUS_SUCCESS,
			want: device.AcceleratorProcesses{
				ID: deviceID,
				Processes: []device.AcceleratorProcess{
					{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](35)},
					{PID: 200, MemoryBytes: ptr.To[uint64](2 << 30), CoresPercent: ptr.To[uint32](70)},
				},
			},
		},
		{
			name: "a row carrying the invalid sentinel reports memory and no compute figure",
			infos: []rsmi.ProcessInfo{
				{Process_id: 100, Vram_usage: 1 << 30, Cu_occupancy: kfdStatsInvalidForTest},
			},
			infoRet: rsmi.STATUS_SUCCESS,
			want: device.AcceleratorProcesses{
				ID: deviceID,
				Processes: []device.AcceleratorProcess{
					{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30)},
				},
			},
		},
		{
			name: "a mixed device: one row measures occupancy, one carries the sentinel",
			infos: []rsmi.ProcessInfo{
				{Process_id: 100, Vram_usage: 1 << 30, Cu_occupancy: 42},
				{Process_id: 200, Vram_usage: 2 << 30, Cu_occupancy: kfdStatsInvalidForTest},
			},
			infoRet: rsmi.STATUS_SUCCESS,
			want: device.AcceleratorProcesses{
				ID: deviceID,
				Processes: []device.AcceleratorProcess{
					{PID: 100, MemoryBytes: ptr.To[uint64](1 << 30), CoresPercent: ptr.To[uint32](42)},
					{PID: 200, MemoryBytes: ptr.To[uint64](2 << 30)},
				},
			},
		},
		{
			name:    "an idle device answers with no rows and no reason",
			infoRet: rsmi.STATUS_SUCCESS,
			want:    device.AcceleratorProcesses{ID: deviceID, Processes: []device.AcceleratorProcess{}},
		},
		{
			name:    "a driver that does not serve the query reports unsupported for both figures",
			infoRet: rsmi.STATUS_NOT_SUPPORTED,
			want: device.AcceleratorProcesses{
				ID:           deviceID,
				MemoryReason: device.AcceleratorProcessReasonUnsupported,
				CoresReason:  device.AcceleratorProcessReasonUnsupported,
				Processes:    []device.AcceleratorProcess{},
			},
		},
		{
			name:    "a list that could not be completed is truncated, never partial, on both figures",
			infos:   []rsmi.ProcessInfo{{Process_id: 100, Vram_usage: 1 << 30, Cu_occupancy: 10}},
			infoRet: rsmi.STATUS_INSUFFICIENT_SIZE,
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
			got := acceleratorProcessesOf(deviceID, c.infos, c.infoRet)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestProcessQueryReason pins the reason taxonomy: a refusal that will not clear on its own is told
// apart from one that may, and success — with rows or without — is never a refusal.
func TestProcessQueryReason(t *testing.T) {
	cases := []struct {
		ret  rsmi.Return
		want device.AcceleratorProcessReason
	}{
		{rsmi.STATUS_SUCCESS, device.AcceleratorProcessReasonNone},
		{rsmi.STATUS_NOT_SUPPORTED, device.AcceleratorProcessReasonUnsupported},
		{rsmi.STATUS_FUNCTION_NOT_FOUND, device.AcceleratorProcessReasonUnsupported},
		{rsmi.STATUS_PERMISSION, device.AcceleratorProcessReasonPermission},
		{rsmi.STATUS_INSUFFICIENT_SIZE, device.AcceleratorProcessReasonTruncated},
		{rsmi.STATUS_INTERNAL_EXCEPTION, device.AcceleratorProcessReasonDriverError},
		{rsmi.STATUS_INIT_ERROR, device.AcceleratorProcessReasonDriverError},
	}

	for _, c := range cases {
		t.Run(c.ret.String(), func(t *testing.T) {
			assert.Equal(t, c.want, processQueryReason(c.ret))
		})
	}
}
