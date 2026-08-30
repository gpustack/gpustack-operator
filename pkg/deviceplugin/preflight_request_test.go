package deviceplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestNewPreflightAllocationRequest(t *testing.T) {
	nvidiaGroup := workercore.DevicesGroup{
		ID:           "grp-nvidia",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Accelerators: []workercore.Accelerator{
			{ID: "dev-0", Index: 0},
			{ID: "dev-1", Index: 1},
		},
	}
	ascendGroup := workercore.DevicesGroup{
		ID:           "grp-ascend",
		Manufacturer: nodefeature.ManufacturerAscend,
		Accelerators: []workercore.Accelerator{
			{ID: "dev-0", Index: 0},
		},
	}

	cases := []struct {
		name         string
		groups       []workercore.DevicesGroup
		manufacturer string
		mode         workercore.DeviceAllocationMode
		quota        int32
		wantErr      string
		wantAlloc    map[Resource]int32
		wantResName  string
		wantUnitsRes string
		wantUnits    int64
	}{
		{
			name:         "sliced commits quota as the per-accelerator units",
			groups:       []workercore.DevicesGroup{nvidiaGroup, ascendGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        nodefeature.ResourceMaxUnits / 4,
			wantAlloc: map[Resource]int32{
				{Group: "grp-nvidia", Device: "dev-0"}: nodefeature.ResourceMaxUnits / 4,
				{Group: "grp-nvidia", Device: "dev-1"}: nodefeature.ResourceMaxUnits / 4,
			},
			wantResName:  "nvidia.com/gpu.sliced",
			wantUnitsRes: "nvidia.com/gpu.sliced.units",
			wantUnits:    nodefeature.ResourceMaxUnits / 4,
		},
		{
			// The mode this builder cannot shape a valid request for, on the manufacturer that most
			// plausibly asks: a partition is named by a <kind>-<profile> limit, and there is no
			// profile here to write one from. Built anyway, the request carries the bare
			// ".partitioned" key, passes every check, and is refused at allocation for naming no
			// partition profile -- so it is refused here, where the caller can still do something
			// about it.
			name:         "partitioned is refused rather than built without a profile",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModePartitioned,
			quota:        500,
			wantErr:      "cannot be asked about through a preflight request",
		},
		{
			name:         "exclusive charges a whole accelerator regardless of quota",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeExclusive,
			quota:        1,
			wantAlloc: map[Resource]int32{
				{Group: "grp-nvidia", Device: "dev-0"}: nodefeature.ResourceMaxUnits,
				{Group: "grp-nvidia", Device: "dev-1"}: nodefeature.ResourceMaxUnits,
			},
			wantResName: "nvidia.com/gpu",
		},
		{
			name:         "shared charges one owner's share regardless of quota",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeShared,
			quota:        1,
			wantAlloc: map[Resource]int32{
				{Group: "grp-nvidia", Device: "dev-0"}: nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize,
				{Group: "grp-nvidia", Device: "dev-1"}: nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize,
			},
			wantResName: "nvidia.com/gpu.shared",
		},
		{
			// Refused rather than capped. Capping reaches only the allocation map: the units limit
			// and the two percentages would keep the oversized figure, leaving a request that
			// describes a slice larger than the accelerator its own map says it was charged for.
			name:         "a quota over the global denominator is refused",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        nodefeature.ResourceMaxUnits + 1,
			wantErr:      "quota must not exceed one whole accelerator",
		},
		{
			// The boundary itself is a whole accelerator, which is an ordinary ask.
			name:         "a quota of exactly the global denominator is a whole accelerator",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        nodefeature.ResourceMaxUnits,
			wantAlloc: map[Resource]int32{
				{Group: "grp-nvidia", Device: "dev-0"}: nodefeature.ResourceMaxUnits,
				{Group: "grp-nvidia", Device: "dev-1"}: nodefeature.ResourceMaxUnits,
			},
			wantResName:  "nvidia.com/gpu.sliced",
			wantUnitsRes: "nvidia.com/gpu.sliced.units",
			wantUnits:    nodefeature.ResourceMaxUnits,
		},
		{
			// The mode suffix is appended to the manufacturer's base name, and an unknown one has
			// none -- so this used to come back as the bare ".sliced", which is not empty and got
			// through a check on the derived name. Every key in the request was then a suffix with
			// no vendor in front of it.
			name:         "an unknown manufacturer is refused before any resource name is derived",
			groups:       []workercore.DevicesGroup{{Manufacturer: "acme", ID: "grp", Accelerators: []workercore.Accelerator{{ID: "dev-0"}}}},
			manufacturer: "acme",
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        nodefeature.ResourceMaxUnits / 2,
			wantErr:      "not an acceleratable manufacturer",
		},
		{
			// Both derivations answer an unrecognized mode from a default arm -- the resource name
			// comes back as the bare exclusive key and the unit count as a whole accelerator -- so
			// this used to be served as a silent request for the entire card. None is the zero
			// value, so it is what an uninitialised caller passes.
			name:         "the none mode is refused rather than served as a whole accelerator",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeNone,
			quota:        nodefeature.ResourceMaxUnits / 2,
			wantErr:      "is not a mode a preflight can ask about",
		},
		{
			name:         "a mode past the enum is refused",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeVisibility + 1,
			quota:        nodefeature.ResourceMaxUnits / 2,
			wantErr:      "is not a mode a preflight can ask about",
		},
		{
			// The two percentages carry whole numbers only, so a quota that is not one would commit
			// 1234 in units while describing the 1% that is 16000 of them. Refused rather than
			// rounded, because the units are the half a scheduler charges against.
			name:         "a sliced quota that is not a whole percent is refused",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        1234,
			wantErr:      "must be a whole percent of an accelerator",
		},
		{
			// Rounding down would have served this as the same 1% as the case above.
			name:         "a sliced quota rounding down to another's percent is refused",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        20000,
			wantErr:      "must be a whole percent of an accelerator",
		},
		{
			// One whole percent is the smallest quota the percentages can carry.
			name:         "the smallest whole percent is accepted",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        nodefeature.ResourceMaxUnits / 100,
			wantAlloc: map[Resource]int32{
				{Group: "grp-nvidia", Device: "dev-0"}: nodefeature.ResourceMaxUnits / 100,
				{Group: "grp-nvidia", Device: "dev-1"}: nodefeature.ResourceMaxUnits / 100,
			},
			wantResName:  "nvidia.com/gpu.sliced",
			wantUnitsRes: "nvidia.com/gpu.sliced.units",
			wantUnits:    nodefeature.ResourceMaxUnits / 100,
		},
		{
			name:    "an empty group list is refused",
			groups:  nil,
			mode:    workercore.DeviceAllocationModeSliced,
			quota:   1,
			wantErr: "no device groups given",
		},
		{
			name:         "a non-positive quota is refused",
			groups:       []workercore.DevicesGroup{nvidiaGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        0,
			wantErr:      "quota must be positive",
		},
		{
			name:         "a manufacturer absent from the groups is refused",
			groups:       []workercore.DevicesGroup{ascendGroup},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        1,
			wantErr:      `no device group for manufacturer "nvidia"`,
		},
		{
			name:         "a manufacturer with no accelerator in its own group is refused",
			groups:       []workercore.DevicesGroup{{ID: "grp-empty", Manufacturer: nodefeature.ManufacturerNVIDIA}},
			manufacturer: nodefeature.ManufacturerNVIDIA,
			mode:         workercore.DeviceAllocationModeSliced,
			quota:        1,
			wantErr:      `manufacturer "nvidia" has no accelerator`,
		},
		{
			// Refused on the mode before the manufacturer's kinds are ever consulted, so a
			// manufacturer with no partition kind and one with a working one are refused for the
			// same reason: no request built here can name a profile, whoever is asking.
			name:         "partitioned is refused on a manufacturer with no partition kind either",
			groups:       []workercore.DevicesGroup{ascendGroup},
			manufacturer: nodefeature.ManufacturerAscend,
			mode:         workercore.DeviceAllocationModePartitioned,
			quota:        1,
			wantErr:      "cannot be asked about through a preflight request",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			pod, ctr, devs, alloc, err := NewPreflightAllocationRequest(c.groups, c.manufacturer, c.mode, c.quota)

			if c.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, c.wantErr)
				assert.Nil(t, pod)
				assert.Nil(t, ctr)
				assert.Nil(t, devs)
				assert.Nil(t, alloc)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, c.wantAlloc, alloc)

			require.NotNil(t, pod)
			require.Len(t, pod.Spec.Containers, 1)
			assert.Same(t, &pod.Spec.Containers[0], ctr)

			resQty, ok := ctr.Resources.Limits[core.ResourceName(c.wantResName)]
			require.True(t, ok, "container carries the %s resource", c.wantResName)
			assert.Equal(t, int64(len(c.wantAlloc)), resQty.Value())

			if c.wantUnitsRes != "" {
				unitsQty, ok := ctr.Resources.Limits[core.ResourceName(c.wantUnitsRes)]
				require.True(t, ok, "container carries the %s units resource", c.wantUnitsRes)
				assert.Equal(t, c.wantUnits, unitsQty.Value())
			}

			require.NotNil(t, devs)
			assert.Equal(t, c.groups, devs.Spec.Groups)
		})
	}
}

func TestNewPreflightAllocationRequest_Deterministic(t *testing.T) {
	const sameQuota = nodefeature.ResourceMaxUnits / 4

	groups := []workercore.DevicesGroup{{
		ID:           "grp-nvidia",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Accelerators: []workercore.Accelerator{{ID: "dev-0", Index: 0}},
	}}

	pod1, ctr1, devs1, alloc1, err1 := NewPreflightAllocationRequest(
		groups, nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced, sameQuota)
	require.NoError(t, err1)

	pod2, ctr2, devs2, alloc2, err2 := NewPreflightAllocationRequest(
		groups, nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced, sameQuota)
	require.NoError(t, err2)

	assert.Equal(t, pod1, pod2)
	assert.Equal(t, ctr1, ctr2)
	assert.Equal(t, devs1, devs2)
	assert.Equal(t, alloc1, alloc2)
}

// A sliced request has to carry the dimensions the slice is cut along, not only the units that
// count it. A responder handed a request without them cannot render a slice at all — SlicedMemoryMib
// refuses a container that asks for neither memory figure — and what reaches the caller is a driver
// error that reads as a broken node rather than as an under-specified ask.
//
// They are derived from quota rather than fixed, so the units the request commits and the slice it
// describes cannot disagree.
func TestNewPreflightAllocationRequest_SlicedCarriesItsDimensions(t *testing.T) {
	group := workercore.DevicesGroup{
		ID:           "grp-amd",
		Manufacturer: nodefeature.ManufacturerAMD,
		Accelerators: []workercore.Accelerator{{ID: "dev-0", Index: 0}},
	}

	testCases := []struct {
		name    string
		mode    workercore.DeviceAllocationMode
		quota   int32
		wantPct string
		absent  bool
	}{
		{
			name:  "half an accelerator asks for half of each dimension",
			mode:  workercore.DeviceAllocationModeSliced,
			quota: nodefeature.ResourceMaxUnits / 2, wantPct: "50",
		},
		{
			name:  "a quarter asks for a quarter of each",
			mode:  workercore.DeviceAllocationModeSliced,
			quota: nodefeature.ResourceMaxUnits / 4, wantPct: "25",
		},
		{
			// One percent is the smallest quota these dimensions can carry: below it the ratio
			// rounds to nothing, and a zero figure is what SlicedMemoryMib treats as unset. A
			// quota that is not a whole percent is refused by the constructor itself rather than
			// rounded into one -- see the table in TestNewPreflightAllocationRequest.
			name:  "the smallest quota the dimensions can carry asks for one percent",
			mode:  workercore.DeviceAllocationModeSliced,
			quota: nodefeature.ResourceMaxUnits / 100, wantPct: "1",
		},
		{
			// Only the sliced mode is cut along these dimensions; a whole-accelerator grant is not
			// cut at all, and asking for a fraction of one would describe an allocation nobody makes.
			name:  "an exclusive request carries neither",
			mode:  workercore.DeviceAllocationModeExclusive,
			quota: nodefeature.ResourceMaxUnits, absent: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, ctr, _, _, err := NewPreflightAllocationRequest(
				[]workercore.DevicesGroup{group}, nodefeature.ManufacturerAMD, tc.mode, tc.quota)
			require.NoError(t, err)

			memPct := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(nodefeature.ManufacturerAMD)
			corePct := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(nodefeature.ManufacturerAMD)

			if tc.absent {
				assert.NotContains(t, ctr.Resources.Limits, memPct)
				assert.NotContains(t, ctr.Resources.Limits, corePct)
				return
			}

			mem, ok := ctr.Resources.Limits[memPct]
			require.True(t, ok, "a sliced request names no memory dimension, so no slice can be rendered")
			assert.Equal(t, tc.wantPct, mem.String())

			core, ok := ctr.Resources.Limits[corePct]
			require.True(t, ok, "a sliced request names no compute dimension")
			assert.Equal(t, tc.wantPct, core.String(), "both dimensions follow the same quota")

			// And the figure the responder derives from it is a real slice of the accelerator,
			// rather than the whole of it.
			mib, err := SlicedMemoryMib(ctr, memPct,
				nodefeature.GetAcceleratableSlicedMemoryMibResourceName(nodefeature.ManufacturerAMD), 16368)
			require.NoError(t, err, "the responder's own reader must accept this request")
			assert.Less(t, mib, int64(16368), "a slice is less than the accelerator it is cut from")
		})
	}
}
