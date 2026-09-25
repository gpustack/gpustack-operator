package worker

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// TestPerCardSliceLimit_AgreesAcrossLayers pins the per-card slice limit on the three layers above
// the device plugin, on a Hygon-shaped node whose cards host at most four slices each: the
// admission check, the fit label and the InstanceType sliced view all read a card whose slots are
// taken as having no room, whatever memory it has left.
func TestPerCardSliceLimit_AgreesAcrossLayers(t *testing.T) {
	const unitsPerPercent = ledgerD / 100
	hygonCard := func(freePct, slices int32) nodeCard {
		return nodeCard{
			capability: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 4}},
			alloc: workercore.AcceleratorAllocation{
				Mode: workercore.DeviceAllocationModeSliced, Remaining: freePct * unitsPerPercent, AllocatedSlices: slices,
			},
		}
	}
	cases := []struct {
		name string
		// card0 holds small slices, card1 two large ones.
		cards     []nodeCard
		wantState kueue.CheckState
		// wantLabel is the sliced fit label's value, wantOnceMax and wantRemaining the sliced
		// view's, both in percent of a card.
		wantLabel     int32
		wantOnceMax   int64
		wantRemaining int64
	}{
		{
			// card0: four 10 % slices, every slot taken, 60 % free; card1: two 45 % slices, 10 % free.
			name:          "a 30 % slice finds no card with both a slot and the units",
			cards:         []nodeCard{hygonCard(60, 4), hygonCard(10, 2)},
			wantState:     kueue.CheckStateRetry,
			wantLabel:     10 * unitsPerPercent,
			wantOnceMax:   10,
			wantRemaining: 10,
		},
		{
			name:          "a 30 % slice fits the card with a slot left",
			cards:         []nodeCard{hygonCard(70, 3), hygonCard(10, 2)},
			wantState:     kueue.CheckStateReady,
			wantLabel:     70 * unitsPerPercent,
			wantOnceMax:   70,
			wantRemaining: 80,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			devs := nodeDevicesWithCapability("hygon", c.cards...)

			state, _ := feasibilityOfOnePool([]workercore.Devices{devs}, []familyDemand{{
				family: nodefeature.ResourceFamilySliced, cards: 1, unitsPerCard: 30 * unitsPerPercent,
			}})
			assert.Equal(t, c.wantState, state, "admission check")

			label := fitLabelsOf(&devs)[nodefeature.FitSlicedMaxFreeUnitsLabelKey(testAcceleratorKey)]
			assert.Equal(t, strconv.Itoa(int(c.wantLabel)), label, "fit label")

			_, _, sliced, _ := getAcceleratorResources([]workercore.Devices{devs}, testAcceleratorKey)
			assert.Equal(t, c.wantOnceMax, sliced.OnceMaxRequest.Value(), "InstanceType once-max request")
			assert.Equal(t, c.wantRemaining, sliced.Remaining.Value(), "InstanceType remaining")
			assert.Equal(t, int64(200), sliced.Capacity.Value(), "InstanceType capacity is the whole pool")
		})
	}
}

// TestPerCardSliceLimit_CountsTheWorkloadsOwnSlices pins that the admission check adds the slices a
// Workload is being given to the ones the ledger already records: a card with one slot left takes
// one of a Workload's two slices, not both, even with memory for both.
func TestPerCardSliceLimit_CountsTheWorkloadsOwnSlices(t *testing.T) {
	const unitsPerPercent = ledgerD / 100
	devs := nodeDevicesWithCapability("hygon", nodeCard{
		capability: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 4}},
		alloc: workercore.AcceleratorAllocation{
			Mode: workercore.DeviceAllocationModeSliced, Remaining: 70 * unitsPerPercent, AllocatedSlices: 3,
		},
	})
	demand := func(slices int32) []familyDemand {
		return []familyDemand{{family: nodefeature.ResourceFamilySliced, cards: slices, unitsPerCard: 10 * unitsPerPercent}}
	}

	state, _ := feasibilityOfOnePool([]workercore.Devices{devs}, demand(1))
	assert.Equal(t, kueue.CheckStateReady, state, "one slice takes the last slot")
	state, _ = feasibilityOfOnePool([]workercore.Devices{devs}, demand(2))
	assert.Equal(t, kueue.CheckStateRetry, state, "a second slice finds no slot")
}
