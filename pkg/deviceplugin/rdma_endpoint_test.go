package deviceplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	deviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// The mode judgment's whole-function branch serves three modes, and a case that holds for one of
// them must hold for all three: an interface in the partitioned state advertises nothing under any
// of them, and a whole-function interface advertises itself under all of them.
var rdmaWholeFunctionModes = []workercore.DeviceAllocationMode{
	workercore.DeviceAllocationModeExclusive,
	workercore.DeviceAllocationModeShared,
	workercore.DeviceAllocationModeSliced,
}

func TestRDMAEndpointsModeJudgment(t *testing.T) {
	cases := []struct {
		name  string
		iface workercore.DeviceInterface
		// wantWhole names the endpoints each whole-function mode advertises, nil for none.
		wantWhole []string
		// wantPartitioned names the endpoints Partitioned advertises, nil for none.
		wantPartitioned []string
	}{
		{
			name: "a physical function with two virtual functions serves partitioned only",
			iface: workercore.DeviceInterface{
				Name:       "pf0",
				SRIOV:      true,
				RDMADevice: "mlx5_0",
				VirtualFunctions: []workercore.DeviceInterfaceVirtualFunction{
					{Name: "vf0", RDMADevice: "mlx5_1"},
					{Name: "vf1", RDMADevice: "mlx5_2"},
				},
			},
			wantPartitioned: []string{"vf0", "vf1"},
		},
		{
			name: "a physical function with zero virtual functions serves the whole-function modes",
			iface: workercore.DeviceInterface{
				Name:       "pf0",
				SRIOV:      true,
				RDMADevice: "mlx5_0",
			},
			wantWhole: []string{"pf0"},
		},
		{
			name: "an interface that is not a physical function serves the whole-function modes",
			iface: workercore.DeviceInterface{
				Name:       "eth0",
				RDMADevice: "rxe0_eth0",
			},
			wantWhole: []string{"eth0"},
		},
		{
			// Being a physical function and having virtual functions are separate facts in the
			// record. A record carrying functions without the physical-function fact is not in
			// the partitioned state, and an implementation deciding the branch from the count
			// alone sends it there.
			name: "virtual functions without the physical-function fact do not partition",
			iface: workercore.DeviceInterface{
				Name:       "eth0",
				RDMADevice: "rxe0_eth0",
				VirtualFunctions: []workercore.DeviceInterfaceVirtualFunction{
					{Name: "vf0", RDMADevice: "mlx5_1"},
					{Name: "vf1", RDMADevice: "mlx5_2"},
				},
			},
			wantWhole: []string{"eth0"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, mode := range rdmaWholeFunctionModes {
				assertRDMAEndpoints(t, mode.String(), c.iface.Name,
					rdmaEndpoints(&c.iface, mode), c.wantWhole)
			}
			assertRDMAEndpoints(t, workercore.DeviceAllocationModePartitioned.String(), c.iface.Name,
				rdmaEndpoints(&c.iface, workercore.DeviceAllocationModePartitioned), c.wantPartitioned)
		})
	}
}

func TestRDMAEndpointsRequireDeviceName(t *testing.T) {
	cases := []struct {
		name  string
		iface workercore.DeviceInterface
		// wantPartitioned names the virtual functions Partitioned advertises; every
		// whole-function mode is asserted to advertise nothing for every case here.
		wantPartitioned []string
	}{
		{
			name:  "a whole-function interface with no RDMA device name is not an endpoint",
			iface: workercore.DeviceInterface{Name: "eth0"},
		},
		{
			name: "a virtual function with no RDMA device name is not an endpoint",
			iface: workercore.DeviceInterface{
				Name:       "pf0",
				SRIOV:      true,
				RDMADevice: "mlx5_0",
				VirtualFunctions: []workercore.DeviceInterfaceVirtualFunction{
					{Name: "vf0"},
					{Name: "vf1", RDMADevice: "mlx5_2"},
				},
			},
			wantPartitioned: []string{"vf1"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, mode := range rdmaWholeFunctionModes {
				assertRDMAEndpoints(t, mode.String(), c.iface.Name,
					rdmaEndpoints(&c.iface, mode), nil)
			}
			assertRDMAEndpoints(t, workercore.DeviceAllocationModePartitioned.String(), c.iface.Name,
				rdmaEndpoints(&c.iface, workercore.DeviceAllocationModePartitioned), c.wantPartitioned)
		})
	}
}

func TestRDMAEndpointsUnservedModes(t *testing.T) {
	// Visibility names an endpoint another container of the same Pod holds, and no RDMA
	// allocation record is written to answer that from, so the mode is unserved. None and any
	// unrecognized mode likewise advertise nothing rather than silently taking the
	// whole-function branch.
	iface := workercore.DeviceInterface{Name: "eth0", RDMADevice: "rxe0_eth0"}
	for _, mode := range []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeVisibility,
		workercore.DeviceAllocationModeNone,
	} {
		assert.Empty(t, rdmaEndpoints(&iface, mode), "%s advertises nothing", mode)
	}
}

func TestRDMAEndpointNumaTopology(t *testing.T) {
	// fixture is one interface and, aligned with its advertised endpoints in order, the NUMA
	// node ids each endpoint's hint must name. A nil entry asserts that endpoint carries no
	// hint at all, which is a different fact from a hint naming node 0.
	type fixture struct {
		iface workercore.DeviceInterface
		want  [][]int64
	}

	cases := []struct {
		name string
		mode workercore.DeviceAllocationMode
		// fixtures carries one entry per interface. More than one is what asserts each
		// advertised device's hint names its own interface's node: a single-interface fixture
		// cannot tell a hint read off this endpoint's record from one read off some other
		// interface's.
		fixtures []fixture
	}{
		{
			name: "the hint names the node the interface's own record carries",
			mode: workercore.DeviceAllocationModeExclusive,
			fixtures: []fixture{
				{
					iface: workercore.DeviceInterface{
						Name: "ibp0", RDMADevice: "mlx5_0", NumaAffinity: "1",
					},
					want: [][]int64{{1}},
				},
			},
		},
		{
			name: "an interface whose affinity is unknown carries no hint",
			mode: workercore.DeviceAllocationModeExclusive,
			fixtures: []fixture{
				{
					iface: workercore.DeviceInterface{
						Name: "ibp0", RDMADevice: "mlx5_0",
					},
					want: [][]int64{nil},
				},
			},
		},
		{
			name: "a range affinity names every node it covers",
			mode: workercore.DeviceAllocationModeExclusive,
			fixtures: []fixture{
				{
					iface: workercore.DeviceInterface{
						Name: "ibp0", RDMADevice: "mlx5_0", NumaAffinity: "0-1",
					},
					want: [][]int64{{0, 1}},
				},
			},
		},
		{
			name: "a virtual function falls back to its parent's affinity",
			mode: workercore.DeviceAllocationModePartitioned,
			fixtures: []fixture{
				{
					iface: workercore.DeviceInterface{
						Name: "pf0", SRIOV: true, NumaAffinity: "0",
						VirtualFunctions: []workercore.DeviceInterfaceVirtualFunction{
							{Name: "vf0", RDMADevice: "mlx5_1"},
						},
					},
					want: [][]int64{{0}},
				},
			},
		},
		{
			name: "a virtual function's own affinity outranks its parent's",
			mode: workercore.DeviceAllocationModePartitioned,
			fixtures: []fixture{
				{
					iface: workercore.DeviceInterface{
						Name: "pf0", SRIOV: true, NumaAffinity: "0",
						VirtualFunctions: []workercore.DeviceInterfaceVirtualFunction{
							{Name: "vf0", RDMADevice: "mlx5_1", NumaAffinity: "1"},
						},
					},
					want: [][]int64{{1}},
				},
			},
		},
		{
			name: "each interface's hint names its own interface's node",
			mode: workercore.DeviceAllocationModeExclusive,
			fixtures: []fixture{
				{
					iface: workercore.DeviceInterface{
						Name: "ibp0", RDMADevice: "mlx5_0", NumaAffinity: "0",
					},
					want: [][]int64{{0}},
				},
				{
					iface: workercore.DeviceInterface{
						Name: "ibp1", RDMADevice: "mlx5_1", NumaAffinity: "1",
					},
					want: [][]int64{{1}},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, f := range c.fixtures {
				endpoints := rdmaEndpoints(&f.iface, c.mode)
				require.Len(t, endpoints, len(f.want),
					"%s: advertised endpoints under %s", f.iface.Name, c.mode)
				for i, want := range f.want {
					assert.Equal(t, want, rdmaNumaNodeIDs(endpoints[i].Topology),
						"%s: endpoint %s: NUMA hint", f.iface.Name, endpoints[i].Resource.Device)
				}
			}
		})
	}
}

// assertRDMAEndpoints asserts the endpoint locators one mode advertises for one interface, and
// that every advertised endpoint carries the RDMA device name an allocation resolves to a
// character device.
func assertRDMAEndpoints(
	t *testing.T, mode, group string, got []rdmaEndpoint, want []string,
) {
	t.Helper()
	require.Len(t, got, len(want), "%s: advertised endpoints", mode)
	for i, name := range want {
		assert.Equal(t, Resource{Group: group, Device: name}, got[i].Resource,
			"%s: endpoint locator", mode)
		assert.NotEmpty(t, got[i].RDMADevice,
			"%s: an advertised endpoint carries an RDMA device name", mode)
	}
}

// rdmaNumaNodeIDs reads the NUMA node ids a hint names, nil when there is no hint.
func rdmaNumaNodeIDs(topology *deviceplugin.TopologyInfo) []int64 {
	if topology == nil {
		return nil
	}
	ids := make([]int64, 0, len(topology.Nodes))
	for _, node := range topology.Nodes {
		ids = append(ids, node.ID)
	}
	return ids
}
