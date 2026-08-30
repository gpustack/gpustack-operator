package amd

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// classifyTopologyCall is what turns one topology read into the state an operator acts on, and it
// must agree with PlaceLogicalSliced's own two-way refusal-or-proceed split: every failure this call
// can carry refuses the allocation identically, so none of them may read as anything other than
// unavailable.
func TestClassifyTopologyCall(t *testing.T) {
	testCases := []struct {
		name       string
		topo       Topology
		err        error
		wantState  device.PreflightState
		wantDetail string
	}{
		{
			name:       "a topology that reads and validates is ok",
			topo:       testTopology,
			wantState:  device.PreflightStateOK,
			wantDetail: fmt.Sprintf("%s reports %d compute units in %d-cu allocation atoms", testTopology.Name, testTopology.CU, testTopology.Quantum()),
		},
		{
			name:      "hsa failing to initialize is unavailable",
			err:       errors.New("hsa init failed: HSA_STATUS_ERROR"),
			wantState: device.PreflightStateUnavailable,
		},
		{
			name:      "no hsa agent naming the card is unavailable",
			err:       errors.New("no hsa agent reports card GPU-x (pci bus id \"0000:04:00.0\")"),
			wantState: device.PreflightStateUnavailable,
		},
		{
			name:      "a topology that reads but that Validate refuses is unavailable",
			topo:      Topology{Name: "gfx1101", CU: 0},
			wantState: device.PreflightStateUnavailable,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			state, detail, reason := classifyTopologyCall(tc.topo, tc.err)
			assert.Equal(t, tc.wantState, state, "state")
			assert.Equal(t, tc.wantDetail, detail, "detail")
			if tc.err == nil {
				if tc.wantState == device.PreflightStateOK {
					assert.Empty(t, reason, "a call that validated carries no reason")
					return
				}
				// A Validate refusal on a read that succeeded still carries a reason -- Validate's own.
				assert.NotEmpty(t, reason, "a validation refusal must say why")
				return
			}
			assert.Equal(t, tc.err.Error(), reason, "the driver's own words are the reason")
		})
	}
}

// preflightOneAccelerator runs a preflight over a single accelerator carrying id, and returns its
// one check together with the reader that served it.
func preflightOneAccelerator(
	t *testing.T,
	id string,
	reader func(string, string) (Topology, error),
) device.PreflightCheck {
	t.Helper()

	p := &preflighter{logger: klog.Background(), readTopology: reader}
	grp := p.PreflightAccelerator(device.DevicesGroupList{{
		ID:           "grp-0",
		Manufacturer: Manufacturer,
		Accelerators: []workercore.Accelerator{
			{ID: id, Index: 0, Topology: workercore.DeviceTopology{PciBusID: "0000:04:00.0"}},
		},
	}})

	require.Equal(t, Manufacturer, grp.Manufacturer)
	require.Len(t, grp.Checks, 1, "one accelerator yields one check")
	require.Equal(t, id, grp.Checks[0].Accelerator)
	require.Equal(t, cuMaskCapability, grp.Checks[0].Capability)
	return grp.Checks[0]
}

// TestPreflightAccelerator covers what check adds on top of the classifier: the identity guard
// PlaceLogicalSliced itself applies before ever calling readTopologyFn, and that the classifier's
// verdict reaches the row unmodified.
func TestPreflightAccelerator(t *testing.T) {
	testCases := []struct {
		name      string
		id        string
		reader    func(string, string) (Topology, error)
		wantState device.PreflightState
		wantCalls int
	}{
		{
			name:      "a readable, valid topology is ok",
			id:        testCardUUID,
			reader:    func(string, string) (Topology, error) { return testTopology, nil },
			wantState: device.PreflightStateOK,
			wantCalls: 1,
		},
		{
			name: "a topology the driver could not answer for is unavailable",
			id:   testCardUUID,
			reader: func(string, string) (Topology, error) {
				return Topology{}, errors.New("no hsa agent reports card")
			},
			wantState: device.PreflightStateUnavailable,
			wantCalls: 1,
		},
		{
			// The accelerator carries no identity, so PlaceLogicalSliced would refuse it before
			// ever reaching readTopologyFn -- and so must this.
			name:      "an accelerator with no id is unavailable and never asked about",
			id:        "",
			reader:    func(string, string) (Topology, error) { t.Fatal("must not be called"); return Topology{}, nil },
			wantState: device.PreflightStateUnavailable,
			wantCalls: 0,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			reader := func(pci, id string) (Topology, error) {
				calls++
				return tc.reader(pci, id)
			}

			check := preflightOneAccelerator(t, tc.id, reader)

			assert.Equal(t, tc.wantState, check.State, "state")
			assert.Equal(t, tc.wantCalls, calls, "reads")
			if tc.wantState == device.PreflightStateOK {
				assert.Empty(t, check.Reason, "a check that passed carries no reason")
			} else {
				assert.NotEmpty(t, check.Reason, "a check that did not pass must say why")
			}
		})
	}
}

// The simulated depth exists to answer what an allocation would inject without becoming one, and for
// AMD's driven entry point that claim is structural rather than something a driver stand-in has to
// prove: GetContainerAllocateResponse reads no driver seam and writes no host path. This pins that a
// preflight pass over it changes nothing on a real host layout, in the same shape every other
// vendor's write-guard test takes.
func TestPreflightResponder_WritesNothing(t *testing.T) {
	defer deviceplugin.RedirectHostWrites(t.TempDir())()
	redirectDevicePaths(t)

	p := &preflighter{logger: klog.Background(), readTopology: readTopologyFn}

	responder, restore1, err := p.PreflightResponder(workercore.DeviceAllocationModeShared)
	require.NoError(t, err)
	defer restore1()
	require.NoError(t, err)

	resp, err := responder.GetContainerAllocateResponse(context.Background(), testPod(), testContainer(0, 0),
		testDevices(testCardUUID, testCardUUID2), testAllocated(testCardUUID))
	require.NoError(t, err)
	assert.NotEmpty(t, resp.Envs, "the response must carry something to have exercised the responder at all")
}

// The anti-drift test. A preflight answer is only worth anything while the injection it reports is
// the one an allocation emits: the same request through a production server and through a preflight
// responder must produce the same injection, differing in nothing.
//
// For AMD this is not a claim about a driver stand-in producing the production branch -- there is no
// stand-in on this path, as PreflightResponder's own doc explains. What this test pins is the seam
// itself: PreflightResponder hands back a server built by the allocator's own newServer, with no
// second construction to drift, and the one line calling newServer is what a reviewer deleting the
// substitution would not notice breaking.
func TestPreflightResponder_MatchesTheProductionResponder(t *testing.T) {
	modes := []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModeVisibility,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			defer deviceplugin.RedirectHostWrites(t.TempDir())()
			redirectDevicePaths(t)

			devs := testDevices(testCardUUID, testCardUUID2)
			allocated := testAllocated(testCardUUID, testCardUUID2)

			// Production: the server an allocation is served by.
			prodSrv, ok := newServer(klog.Background(), mode).(deviceplugin.ContainerAllocateResponder)
			require.True(t, ok)
			want, err := prodSrv.GetContainerAllocateResponse(context.Background(), testPod(), testContainer(0, 0),
				devs, allocated)
			require.NoError(t, err)

			// Preflight: the same server, reached through the seam.
			p := &preflighter{logger: klog.Background(), readTopology: readTopologyFn}
			preSrv, restore2, err := p.PreflightResponder(mode)
			require.NoError(t, err)
			defer restore2()
			require.NoError(t, err)
			got, err := preSrv.GetContainerAllocateResponse(context.Background(), testPod(), testContainer(0, 0),
				devs, allocated)
			require.NoError(t, err)

			// Guarded against passing by comparing two empty responses: every AMD mode emits at
			// least the AMD_VISIBLE_DEVICES env and the granted device nodes, so an injection
			// carrying nothing is a broken fixture rather than an agreement.
			require.NotEmpty(t, want.Envs, "the production injection must carry something to compare")
			require.NotEmpty(t, want.Devices, "and some device nodes too")
			assert.Equal(t, want, got,
				"a preflight answer and the allocation it predicts must not be able to disagree")
		})
	}
}

// The redirect the seam sets up is the whole of its read-only promise, and it is the one thing the
// tests around it cannot see: each of them opens a redirect of its own before calling, so a seam
// that had stopped redirecting would still write nowhere and still pass. This asserts the seam's
// own redirect directly, with no outer one to mask it.
//
// It goes through the exported constructor rather than the struct literal, so it also pins that
// what the registry hands out serves the injection seam at all.
func TestPreflightResponder_RedirectsTheSharedHostPathsAndPutsThemBack(t *testing.T) {
	origLib, origPods := deviceplugin.OperatorLibDir, deviceplugin.OperatorPodsDir

	p, ok := NewPreflighter(device.PreflighterOptions{Logger: klog.Background()}).(deviceplugin.AcceleratorInjectionPreflighter)
	require.True(t, ok, "the registered preflighter must serve the injection seam")

	_, restore, err := p.PreflightResponder(workercore.DeviceAllocationModeSliced)
	require.NoError(t, err)

	assert.NotEqual(t, origLib, deviceplugin.OperatorLibDir,
		"a responder driven here must render under a scratch root, never the host's")
	assert.NotEqual(t, origPods, deviceplugin.OperatorPodsDir)

	restore()

	assert.Equal(t, origLib, deviceplugin.OperatorLibDir,
		"and the restore puts them back, or the rest of this process points at a directory that is gone")
	assert.Equal(t, origPods, deviceplugin.OperatorPodsDir)
}
