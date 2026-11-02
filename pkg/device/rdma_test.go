package device

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRDMASnapshotDeepCopyIsolates pins the contract the detector's read access depends on: a
// copy shares no slice, backing array, or nested record with the original, so a reader can hold
// and mutate its copy while the discovery pass stores fresh observations.
func TestRDMASnapshotDeepCopyIsolates(t *testing.T) {
	src := &RDMASnapshot{
		Devices: []RDMADevice{{
			Name:           "mlx5_0",
			AllocationUnit: "0000:01:00.0",
			Owner:          RDMADeviceOwnerGPUStack,
			Ports: []RDMAPort{{
				Number:  1,
				NetDevs: []string{"eth0"},
				GIDs:    []string{"fe80::1"},
				PKeys:   []string{"0xffff"},
			}},
		}},
		Complete:           true,
		Revision:           "digest",
		LastSuccessfulTime: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
	}

	got := src.DeepCopy()
	requireDeepCopyIsolated(t, src, got)

	got.Devices[0].Ports[0].NetDevs[0] = "mutated"
	got.Devices[0].Ports[0].GIDs[0] = "mutated"
	got.Devices[0].Ports[0].PKeys[0] = "mutated"
	got.Devices[0].Name = "mutated"
	got.Revision = "mutated"

	assert.Equal(t, "mlx5_0", src.Devices[0].Name)
	assert.Equal(t, "digest", src.Revision)
	assert.Equal(t, []string{"eth0"}, src.Devices[0].Ports[0].NetDevs)
	assert.Equal(t, []string{"fe80::1"}, src.Devices[0].Ports[0].GIDs)
	assert.Equal(t, []string{"0xffff"}, src.Devices[0].Ports[0].PKeys)

	// Appending to the copy's top-level slice must not move the source's elements either.
	got.Devices = append(got.Devices, RDMADevice{Name: "appended"})
	assert.Len(t, src.Devices, 1)
}

// TestRDMASnapshotDeepCopyNilSafety pins that the accessor's nil path stays nil: the initial and
// post-restart state is "no observation", and a reader asking for it gets exactly that back.
func TestRDMASnapshotDeepCopyNilSafety(t *testing.T) {
	var nilSnapshot *RDMASnapshot
	assert.Nil(t, nilSnapshot.DeepCopy())

	empty := &RDMASnapshot{}
	got := empty.DeepCopy()
	assert.NotNil(t, got)
	assert.Nil(t, got.Devices, "an empty observation copies as empty, not as a device list")
}

func requireDeepCopyIsolated(t *testing.T, src, got *RDMASnapshot) {
	t.Helper()

	assert.NotSame(t, src, got)
	assert.Equal(t, src.Devices, got.Devices, "the copy starts equal")
	assert.NotSame(t, &src.Devices[0], &got.Devices[0], "device records must not alias")
	assert.NotSame(t, &src.Devices[0].Ports[0], &got.Devices[0].Ports[0], "port records must not alias")
	assert.NotSame(t, &src.Devices[0].Ports[0].NetDevs[0], &got.Devices[0].Ports[0].NetDevs[0],
		"slice backing arrays must not alias")
}

// TestRDMASnapshotRevisionDeterminism pins what the revision digest answers for: the same facts
// give the same value regardless of observation order, diagnostics, or the success time, and the
// fact changes the selection path relies on change it. An empty inventory still digests
// nonempty, which is what lets a complete empty inventory publish a revision at all. A digest is
// a compression, so the test pins the specific moves below and claims no uniqueness beyond them.
func TestRDMASnapshotRevisionDeterminism(t *testing.T) {
	port1 := RDMAPort{
		Number:    1,
		NetDevs:   []string{"eth0", "eth1"},
		LinkLayer: "Ethernet",
		State:     "ACTIVE",
		Rate:      "200 Gb/sec",
		GIDs:      []string{"fe80::1", "fe80::2"},
	}
	port2 := RDMAPort{
		Number:    2,
		NetDevs:   []string{"bond0"},
		LinkLayer: "Ethernet",
		State:     "DOWN",
		Rate:      "100 Gb/sec",
		GIDs:      []string{"fe80::3"},
	}
	deviceA := RDMADevice{
		Name:           "mlx5_0",
		AllocationUnit: "0000:01:00.0",
		Owner:          RDMADeviceOwnerGPUStack,
		Driver:         "mlx5_core",
		PciBusID:       "0000:01:00.0",
		NodeGuid:       "0c:c9:a0:00:00:00:00:00",
		NumaAffinity:   "0",
		Ports:          []RDMAPort{port1, port2},
	}
	deviceB := RDMADevice{
		Name:   "efa_0",
		Owner:  RDMADeviceOwnerExternal,
		Driver: "efa",
		Ports: []RDMAPort{{
			Number:    1,
			LinkLayer: "Ethernet",
			State:     "ACTIVE",
		}},
	}

	// Each case builds on its own deep copy: the fixtures' nested slices must not leak a
	// mutation from one case into another case's baseline.
	base := func() []RDMADevice {
		return []RDMADevice{*deviceA.DeepCopy(), *deviceB.DeepCopy()}
	}
	want := RDMASnapshotRevision(base())

	t.Run("stable across order, messages, and time", func(t *testing.T) {
		reordered := func() []RDMADevice {
			return []RDMADevice{*deviceB.DeepCopy(), *deviceA.DeepCopy()}
		}
		assert.Equal(t, want, RDMASnapshotRevision(reordered()), "device order must not matter")

		portsPermuted := deviceA.DeepCopy()
		portsPermuted.Ports = []RDMAPort{*port2.DeepCopy(), *port1.DeepCopy()}
		assert.Equal(t,
			RDMASnapshotRevision([]RDMADevice{*deviceA.DeepCopy()}),
			RDMASnapshotRevision([]RDMADevice{*portsPermuted}),
			"permuting distinct actual ports must not move a device's revision")

		withNoise := base()
		withNoise[0].Message = "some diagnostic"
		withNoise[0].Ports[0].NetDevs = []string{"eth1", "eth0"}
		assert.Equal(t, want, RDMASnapshotRevision(withNoise),
			"diagnostics and alias order must not matter")
	})

	t.Run("changes with allocation-relevant facts", func(t *testing.T) {
		changed := base()
		changed[0].Ports[0].State = "DOWN"
		assert.NotEqual(t, want, RDMASnapshotRevision(changed), "port state is allocation-relevant")

		portFacts := base()
		portFacts[0].Ports[1].NetDevs = []string{"eth9"}
		assert.NotEqual(t, want, RDMASnapshotRevision(portFacts),
			"a second port's associations are allocation-relevant")

		ownerChanged := base()
		ownerChanged[1].Owner = RDMADeviceOwnerGPUStack
		assert.NotEqual(t, want, RDMASnapshotRevision(ownerChanged), "owner is allocation-relevant")

		added := base()
		added = append(added, RDMADevice{Name: "mlx5_1"})
		assert.NotEqual(t, want, RDMASnapshotRevision(added), "an added device moves the revision")

		removed := []RDMADevice{*deviceA.DeepCopy()}
		assert.NotEqual(t, want, RDMASnapshotRevision(removed), "a removed device moves the revision")
	})

	t.Run("empty inventory digests nonempty", func(t *testing.T) {
		assert.NotEmpty(t, RDMASnapshotRevision(nil))
		assert.NotEmpty(t, RDMASnapshotRevision([]RDMADevice{}))
		assert.Equal(t,
			RDMASnapshotRevision(nil),
			RDMASnapshotRevision([]RDMADevice{}),
			"nil and empty lists are the same observation")
	})
}
