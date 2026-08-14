package deviceplugin

import (
	"testing"

	"github.com/stretchr/testify/assert"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// TestAllocatedAccelerators pins the order the collector hands its pairs back in: the enumeration
// index the detector recorded, whatever order the ledger stores its groups and accelerators in. An
// injection addressed by position is read against the numbering the container derives for itself, so
// this order is what keeps a per-accelerator entry pointing at the accelerator it was computed for.
func TestAllocatedAccelerators(t *testing.T) {
	// group builds one ledger group holding an accelerator per (id, index) pair given.
	type accel struct {
		id    string
		index uint32
	}
	group := func(id string, accels ...accel) workercore.DevicesGroup {
		out := workercore.DevicesGroup{ID: id, Manufacturer: "nvidia"}
		for _, a := range accels {
			out.Accelerators = append(out.Accelerators,
				workercore.Accelerator{ID: a.id, Index: a.index})
		}
		return out
	}

	cases := []struct {
		name      string
		groups    []workercore.DevicesGroup
		allocated []Resource
		want      []string
	}{
		{
			name:      "one group, already in enumeration order",
			groups:    []workercore.DevicesGroup{group("l40s", accel{"gpu-a", 0}, accel{"gpu-b", 1})},
			allocated: []Resource{{Group: "l40s", Device: "gpu-a"}, {Group: "l40s", Device: "gpu-b"}},
			want:      []string{"gpu-a", "gpu-b"},
		},
		{
			name:      "only the allocated accelerators are collected",
			groups:    []workercore.DevicesGroup{group("l40s", accel{"gpu-a", 0}, accel{"gpu-b", 1}, accel{"gpu-c", 2})},
			allocated: []Resource{{Group: "l40s", Device: "gpu-c"}, {Group: "l40s", Device: "gpu-a"}},
			want:      []string{"gpu-a", "gpu-c"},
		},
		{
			name: "accelerators of two models are ordered across their groups",
			// The ledger walk would yield gpu-b then gpu-a; the container numbers gpu-a first.
			groups: []workercore.DevicesGroup{
				group("a10", accel{"gpu-b", 1}),
				group("l40s", accel{"gpu-a", 0}),
			},
			allocated: []Resource{{Group: "a10", Device: "gpu-b"}, {Group: "l40s", Device: "gpu-a"}},
			want:      []string{"gpu-a", "gpu-b"},
		},
		{
			name: "a ledger recording no index keeps its own walk order",
			groups: []workercore.DevicesGroup{
				group("a10", accel{"gpu-b", 0}),
				group("l40s", accel{"gpu-a", 0}),
			},
			allocated: []Resource{{Group: "a10", Device: "gpu-b"}, {Group: "l40s", Device: "gpu-a"}},
			want:      []string{"gpu-b", "gpu-a"},
		},
		{
			name:      "nothing allocated yields nothing",
			groups:    []workercore.DevicesGroup{group("l40s", accel{"gpu-a", 0})},
			allocated: nil,
			want:      []string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			devs := &workercore.Devices{Spec: workercore.DevicesSpec{Groups: c.groups}}
			allocated := make(map[Resource]int32, len(c.allocated))
			for _, res := range c.allocated {
				allocated[res] = 1
			}

			got := AllocatedAccelerators(devs, allocated)

			assert.Equal(t, c.want, AllocatedAcceleratorIDs(got))
			for i := range got {
				assert.NotNil(t, got[i].Group, "every pair carries its group")
			}
		})
	}
}
