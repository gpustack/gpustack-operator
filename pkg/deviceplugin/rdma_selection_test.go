// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// rdmaSnapshotFirstSeen is the first-seen time the fixture's failed verdict carries, and the exact
// value every assertion below holds the capture to. A fresh value per call, so an in-place rewrite
// of one fixture's verdict can never leak into the next build's expectation.
func rdmaSnapshotFirstSeen() meta.Time {
	return meta.Time{Time: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)}
}

// rdmaSnapshotFixture is one inventory and the handles an in-place detector correction writes
// through: the verdict records are held by pointer exactly as the live object holds them.
type rdmaSnapshotFixture struct {
	devs    *workercore.Devices
	ib0Link *workercore.DeviceInterfaceLink
	vf0Link *workercore.DeviceInterfaceLink
}

// newRDMASnapshotFixture builds one whole-function interface over ib0/mlx5_0 on node 0, and one
// SR-IOV physical function whose vf0 serves mlx5_1 on the parent's node 1 carrying a failed
// verdict.
func newRDMASnapshotFixture() *rdmaSnapshotFixture {
	seen := rdmaSnapshotFirstSeen()
	f := &rdmaSnapshotFixture{
		ib0Link: &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK},
		vf0Link: &workercore.DeviceInterfaceLink{
			State:         workercore.DeviceInterfaceLinkStateFailed,
			Reason:        "state down",
			FirstSeenTime: &seen,
		},
	}
	f.devs = rdmaDevices("node-rdma-snap",
		wholeFunctionIface("ib0", "0", "mlx5_0", f.ib0Link),
		sriovPF("pf0", "1", rdmaVF("vf0", "", "mlx5_1", f.vf0Link)),
	)
	return f
}

// rdmaSnapshotMutation names one post-capture write the isolation cases study.
type rdmaSnapshotMutation int

const (
	// rdmaSnapshotMutatesIb0Verdict corrects the whole-function verdict in place, through the
	// pointer the live record retains.
	rdmaSnapshotMutatesIb0Verdict rdmaSnapshotMutation = iota

	// rdmaSnapshotMutatesVfFirstSeen rewrites the virtual function verdict's nested first-seen
	// time in place, through the pointers the live record retains.
	rdmaSnapshotMutatesVfFirstSeen

	// rdmaSnapshotMutatesIb0Device rebinds the whole-function interface's device on the record.
	rdmaSnapshotMutatesIb0Device

	// rdmaSnapshotMutatesIb0Affinity rewrites the interface's affinity on the record.
	rdmaSnapshotMutatesIb0Affinity

	// rdmaSnapshotReplacesIb0 replaces the record's own interface slice element wholesale.
	rdmaSnapshotReplacesIb0

	// rdmaSnapshotMutatesVfDevice rebinds the virtual function's device on the record.
	rdmaSnapshotMutatesVfDevice

	// rdmaSnapshotMutatesVfAffinity rewrites the virtual function's affinity on the record.
	rdmaSnapshotMutatesVfAffinity

	// rdmaSnapshotAppendsVf grows the record's virtual functions with a later sibling.
	rdmaSnapshotAppendsVf

	// rdmaSnapshotMutatesDerivedPartitionedEndpoint rewrites an endpoint derived from the
	// capture itself — the very object a later derivation must not inherit from.
	rdmaSnapshotMutatesDerivedPartitionedEndpoint
)

// applyRDMAInventoryMutation applies one post-capture write to the world a case studies. It is
// the one place the suite executes logic, so the cases above stay data: the way an in-place
// detector correction, a re-enumeration or an endpoint holder arrives.
func applyRDMAInventoryMutation(
	t *testing.T, kind rdmaSnapshotMutation, f *rdmaSnapshotFixture, snap rdmaInterfaceSnapshot,
) {
	t.Helper()

	switch kind {
	case rdmaSnapshotMutatesIb0Verdict:
		f.ib0Link.State = workercore.DeviceInterfaceLinkStateFailed
	case rdmaSnapshotMutatesVfFirstSeen:
		*f.vf0Link.FirstSeenTime = meta.Time{Time: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	case rdmaSnapshotMutatesIb0Device:
		f.devs.Spec.Interfaces[0].RDMADevice = "mlx5_9"
	case rdmaSnapshotMutatesIb0Affinity:
		f.devs.Spec.Interfaces[0].NumaAffinity = "7"
	case rdmaSnapshotReplacesIb0:
		// The replacement rewrites the record's own slice element, which a capture sharing the
		// slice's backing array would inherit wholesale.
		f.devs.Spec.Interfaces[0] = wholeFunctionIface("ib0", "0", "mlx5_replaced", nil)
	case rdmaSnapshotMutatesVfDevice:
		f.devs.Spec.Interfaces[1].VirtualFunctions[0].RDMADevice = "mlx5_8"
	case rdmaSnapshotMutatesVfAffinity:
		f.devs.Spec.Interfaces[1].VirtualFunctions[0].NumaAffinity = "7"
	case rdmaSnapshotAppendsVf:
		// The append grows the record's own slice; the capture advertises exactly the virtual
		// functions it carried, never the record's later ones.
		f.devs.Spec.Interfaces[1].VirtualFunctions = append(
			f.devs.Spec.Interfaces[1].VirtualFunctions,
			rdmaVF("vf9", "9", "mlx5_7", nil),
		)
	case rdmaSnapshotMutatesDerivedPartitionedEndpoint:
		// The write lands on a derived endpoint — its holder's copy — and must reach neither
		// the record nor the capture: the re-derivation after this reads the same capture and
		// must find the pristine facts whatever was done to the earlier derivation.
		eps := rdmaEndpoints(&snap.interfaces[1], workercore.DeviceAllocationModePartitioned)
		require.Len(t, eps, 1)
		eps[0].RDMADevice = "mlx5_8"
		eps[0].Link.State = workercore.DeviceInterfaceLinkStateOK
		*eps[0].Link.FirstSeenTime = meta.Time{Time: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
		eps[0].Topology.Nodes[0].ID = 9
	default:
		t.Fatalf("unknown snapshot mutation kind %d", kind)
	}
}

// TestRDMASnapshot_IsolatesEndpointsFromLaterInventoryChanges pins the capture's own contract,
// one post-capture write per case: a verdict corrected in place, a nested first-seen time
// rewritten, a device rebound, an affinity rewritten, an interface replaced wholesale, a virtual
// function rebound, a virtual function appended, and a derived endpoint rewritten before a
// re-derivation. After each write, endpoints derived from the retained capture still read the
// capture-time facts, asserted as the whole binding — locator, HCA name, NUMA hint, verdict and
// the verdict's first-seen time — not as counts.
func TestRDMASnapshot_IsolatesEndpointsFromLaterInventoryChanges(t *testing.T) {
	cases := []struct {
		name string
		kind rdmaSnapshotMutation
	}{
		{name: "a verdict corrected in place through its retained pointer", kind: rdmaSnapshotMutatesIb0Verdict},
		{name: "a nested first-seen time rewritten in place", kind: rdmaSnapshotMutatesVfFirstSeen},
		{name: "a whole-function device rebound on the record", kind: rdmaSnapshotMutatesIb0Device},
		{name: "an interface affinity rewritten on the record", kind: rdmaSnapshotMutatesIb0Affinity},
		{name: "an interface replaced wholesale", kind: rdmaSnapshotReplacesIb0},
		{name: "a virtual function rebound on the record", kind: rdmaSnapshotMutatesVfDevice},
		{name: "a virtual function affinity rewritten on the record", kind: rdmaSnapshotMutatesVfAffinity},
		{name: "a virtual function appended", kind: rdmaSnapshotAppendsVf},
		{name: "a derived partitioned endpoint mutated, then re-derived", kind: rdmaSnapshotMutatesDerivedPartitionedEndpoint},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRDMASnapshotFixture()
			snap := captureRDMAInterfaceSnapshot(f.devs)
			applyRDMAInventoryMutation(t, c.kind, f, snap)

			for _, mode := range rdmaWholeFunctionModes {
				eps := rdmaEndpoints(&snap.interfaces[0], mode)
				require.Len(t, eps, 1, "%s: whole-function endpoints under %s", c.name, mode)
				assert.Equal(t, Resource{Group: "ib0", Device: "ib0"}, eps[0].Resource,
					"%s: whole-function locator under %s", c.name, mode)
				assert.Equal(t, "mlx5_0", eps[0].RDMADevice,
					"%s: whole-function HCA under %s", c.name, mode)
				assert.Equal(t, []int64{0}, rdmaNumaNodeIDs(eps[0].Topology),
					"%s: whole-function NUMA hint under %s", c.name, mode)
				require.NotNil(t, eps[0].Link, "%s: whole-function verdict under %s", c.name, mode)
				assert.Equal(t, workercore.DeviceInterfaceLinkStateOK, eps[0].Link.State,
					"%s: whole-function verdict state under %s", c.name, mode)
			}

			eps := rdmaEndpoints(&snap.interfaces[1], workercore.DeviceAllocationModePartitioned)
			require.Len(t, eps, 1, "%s: partitioned endpoints", c.name)
			assert.Equal(t, Resource{Group: "pf0", Device: "vf0"}, eps[0].Resource,
				"%s: partitioned locator", c.name)
			assert.Equal(t, "mlx5_1", eps[0].RDMADevice, "%s: partitioned HCA", c.name)
			assert.Equal(t, []int64{1}, rdmaNumaNodeIDs(eps[0].Topology),
				"%s: partitioned NUMA hint", c.name)
			require.NotNil(t, eps[0].Link, "%s: partitioned verdict", c.name)
			assert.Equal(t, workercore.DeviceInterfaceLinkStateFailed, eps[0].Link.State,
				"%s: partitioned verdict state", c.name)
			require.NotNil(t, eps[0].Link.FirstSeenTime, "%s: partitioned first-seen time", c.name)
			assert.Equal(t, rdmaSnapshotFirstSeen(), *eps[0].Link.FirstSeenTime,
				"%s: partitioned first-seen time value", c.name)
		})
	}
}
