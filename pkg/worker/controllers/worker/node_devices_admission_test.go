package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// devicesWithRemaining builds one Devices ledger whose single group lists a card per value
// in remaining (the units still free on that card), mirroring the status the
// DevicesReconciler seeds from Spec at ResourceMaxUnits and decrements per allocation. Every
// card reports a logical slicing capability and no partition profile, so it belongs to the
// exclusive, shared and logical-slice populations alike — the unpartitioned card of F1's table.
func devicesWithRemaining(remaining ...int32) workercore.Devices {
	specAccels := make([]workercore.Accelerator, len(remaining))
	statusAccels := make([]workercore.AcceleratorAllocation, len(remaining))
	for i, free := range remaining {
		id := fmt.Sprintf("gpu-%d", i)
		specAccels[i] = workercore.Accelerator{
			ID: id, Index: uint32(i),
			Status: workercore.AcceleratorStatus{
				LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 10},
			},
		}
		statusAccels[i] = workercore.AcceleratorAllocation{
			ID:        id,
			Index:     uint32(i),
			Remaining: free,
		}
	}
	return workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{ID: "g0", Manufacturer: "nvidia", Accelerators: specAccels}},
		},
		Status: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{ID: "g0", Manufacturer: "nvidia", Accelerators: statusAccels}},
		},
	}
}

func TestNodeDevicesFeasibility(t *testing.T) {
	const (
		whole = int32(nodefeature.ResourceMaxUnits)                                     // a clean whole card
		half  = int32(nodefeature.ResourceMaxUnits / 2)                                 // a 50%-sliced card's free units
		slot  = int32(nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize) // one shared owner's units
	)
	exclusive := nodefeature.ResourceFamilyExclusive
	sliced := nodefeature.ResourceFamilySliced
	shared := nodefeature.ResourceFamilyShared

	cases := []struct {
		name    string
		devices []workercore.Devices
		demands []familyDemand
		want    kueue.CheckState
	}{
		{
			name:    "exclusive over a fully-sliced node is held",
			devices: []workercore.Devices{devicesWithRemaining(half, half, half, half, half, half, half, half)},
			demands: []familyDemand{{family: exclusive, cards: 5}},
			want:    kueue.CheckStateRetry,
		},
		{
			name:    "exclusive with generic headroom but no clean card is held",
			devices: []workercore.Devices{devicesWithRemaining(half, half, half, half, half, half, half, half, half, half, half, half)},
			// 6M total free spread across slices, but zero whole cards.
			demands: []familyDemand{{family: exclusive, cards: 5}},
			want:    kueue.CheckStateRetry,
		},
		{
			name:    "exclusive with enough clean cards is ready",
			devices: []workercore.Devices{devicesWithRemaining(whole, whole, whole, whole, whole, half, half, half)},
			demands: []familyDemand{{family: exclusive, cards: 5}},
			want:    kueue.CheckStateReady,
		},
		{
			name:    "exclusive short by one card is held",
			devices: []workercore.Devices{devicesWithRemaining(whole, whole, whole, whole)},
			demands: []familyDemand{{family: exclusive, cards: 5}},
			want:    kueue.CheckStateRetry,
		},
		{
			name:    "sliced fits on partially-used cards",
			devices: []workercore.Devices{devicesWithRemaining(half, half, half, half)},
			demands: []familyDemand{{family: sliced, cards: 3, unitsPerCard: half}},
			want:    kueue.CheckStateReady,
		},
		{
			name:    "sliced is held when the demand exceeds every card's remainder",
			devices: []workercore.Devices{devicesWithRemaining(half, half)},
			// Wants a whole card's units; no sliced card has it.
			demands: []familyDemand{{family: sliced, cards: 1, unitsPerCard: whole}},
			want:    kueue.CheckStateRetry,
		},
		{
			name:    "shared fits when a free owner slot remains",
			devices: []workercore.Devices{devicesWithRemaining(slot, slot, 0, 0)},
			demands: []familyDemand{{family: shared, cards: 2}},
			want:    kueue.CheckStateReady,
		},
		{
			name:    "feasibility aggregates whole cards across devices",
			devices: []workercore.Devices{devicesWithRemaining(whole, half), devicesWithRemaining(whole, whole, half)},
			// Three clean cards across the two ledgers.
			demands: []familyDemand{{family: exclusive, cards: 3}},
			want:    kueue.CheckStateReady,
		},
		{
			name:    "no accelerator demand is ready",
			devices: []workercore.Devices{devicesWithRemaining(half)},
			demands: nil,
			want:    kueue.CheckStateReady,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, _ := nodeDevicesFeasibility(c.devices, c.demands)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestNodeDevicesFeasibilityScopesEveryFamilyToItsPopulation pins F1's population rule on the
// admission side: a card serves only the families its capability reports. Without it an
// exclusive or shared tenant is judged feasible against a partitioned card, admitted, and left
// Pending forever, because the device plugin advertises that card no whole-card token.
func TestNodeDevicesFeasibilityScopesEveryFamilyToItsPopulation(t *testing.T) {
	whole := int32(nodefeature.ResourceMaxUnits)
	partitioned := []workercore.Devices{physicalDevices(physicalCard{
		id: "g0", mode: workercore.DeviceAllocationModeNone, remaining: whole,
		remainingProfiles: map[string]int32{"1g.10gb": 7}, placementsCached: true,
	})}
	unpartitioned := []workercore.Devices{devicesWithRemaining(whole)}

	cases := []struct {
		name    string
		devices []workercore.Devices
		demand  familyDemand
		want    kueue.CheckState
	}{
		{
			name: "exclusive is not feasible against a partitioned card", devices: partitioned,
			demand: familyDemand{family: nodefeature.ResourceFamilyExclusive, cards: 1},
			want:   kueue.CheckStateRetry,
		},
		{
			name: "shared is not feasible against a partitioned card", devices: partitioned,
			demand: familyDemand{family: nodefeature.ResourceFamilyShared, cards: 1},
			want:   kueue.CheckStateRetry,
		},
		{
			name: "a logical slice is not feasible against a partitioned card", devices: partitioned,
			demand: familyDemand{family: nodefeature.ResourceFamilySliced, cards: 1, unitsPerCard: whole / 2},
			want:   kueue.CheckStateRetry,
		},
		{
			name: "a partition is not feasible against an unpartitioned card", devices: unpartitioned,
			demand: familyDemand{family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "1g.10gb"},
			want:   kueue.CheckStateRetry,
		},
		{
			name: "exclusive is feasible against an unpartitioned card", devices: unpartitioned,
			demand: familyDemand{family: nodefeature.ResourceFamilyExclusive, cards: 1},
			want:   kueue.CheckStateReady,
		},
		{
			name: "a partition is feasible against a partitioned card", devices: partitioned,
			demand: familyDemand{family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "1g.10gb"},
			want:   kueue.CheckStateReady,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, _ := nodeDevicesFeasibility(c.devices, []familyDemand{c.demand})
			assert.Equal(t, c.want, got)
		})
	}
}

// TestNodeDevicesFeasibilityConsumesCardsPerDemand pins that two demands of one Workload never
// both claim the same card: with a single free card, one exclusive card is placeable and two
// are not, even when the two demands arrive as separate correlated tuples.
func TestNodeDevicesFeasibilityConsumesCardsPerDemand(t *testing.T) {
	whole := int32(nodefeature.ResourceMaxUnits)
	one := []workercore.Devices{devicesWithRemaining(whole)}

	got, _ := nodeDevicesFeasibility(one, []familyDemand{
		{family: nodefeature.ResourceFamilyExclusive, cards: 1},
	})
	assert.Equal(t, kueue.CheckStateReady, got)

	got, _ = nodeDevicesFeasibility(one, []familyDemand{
		{family: nodefeature.ResourceFamilyExclusive, cards: 1},
		{family: nodefeature.ResourceFamilySliced, cards: 1, unitsPerCard: whole / 2},
	})
	assert.Equal(t, kueue.CheckStateRetry, got,
		"the logical slice must not reuse the card the exclusive demand already took")
}

// physicalCard describes one partitioned card for a feasibility fixture: its allocation Mode,
// its scalar remaining units, its RemainingProfiles ledger (profile → free instance count), and
// whether its capability carries cached Placements (the ledger-ready signal).
type physicalCard struct {
	id                string
	mode              workercore.DeviceAllocationMode
	remaining         int32
	remainingProfiles map[string]int32
	placementsCached  bool
}

// physicalDevices builds one Devices ledger whose group lists each partitioned card twice — the
// Spec-side capability (a partition profile with its ceiling, and Placements when cached) and the
// Status-side allocation (mode + scalar remaining + RemainingProfiles) — matched by accelerator
// ID, the shape collectCards joins.
func physicalDevices(cards ...physicalCard) workercore.Devices {
	return physicalDevicesOf("nvidia", cards...)
}

// physicalDevicesOf is physicalDevices for a named manufacturer, which the profile-name
// boundary depends on: a card's per-profile ledger is keyed as its own manufacturer's library
// spells a profile, so the manufacturer is what decides whether a published demand has to be
// converted before the lookup.
func physicalDevicesOf(manufacturer string, cards ...physicalCard) workercore.Devices {
	specAccels := make([]workercore.Accelerator, len(cards))
	statusAccels := make([]workercore.AcceleratorAllocation, len(cards))
	for i, c := range cards {
		prof := workercore.AcceleratorPhysicalSlicedProfile{Name: "cap", Count: 7}
		if c.placementsCached {
			prof.Placements = []workercore.AcceleratorPlacement{{Start: 0, Length: 1}}
		}
		specAccels[i] = workercore.Accelerator{
			ID: c.id, Index: uint32(i),
			Status: workercore.AcceleratorStatus{
				PhysicalSliced: workercore.AcceleratorPhysicalSliced{
					Profiles: []workercore.AcceleratorPhysicalSlicedProfile{prof},
					Count:    prof.Count,
				},
			},
		}
		rem := make([]workercore.AcceleratorProfileCount, 0, len(c.remainingProfiles))
		for name, cnt := range c.remainingProfiles {
			rem = append(rem, workercore.AcceleratorProfileCount{Name: name, Count: cnt})
		}
		statusAccels[i] = workercore.AcceleratorAllocation{
			ID: c.id, Index: uint32(i), Mode: c.mode, Remaining: c.remaining, RemainingProfiles: rem,
		}
	}
	return workercore.Devices{
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{ID: "g0", Manufacturer: manufacturer, Accelerators: specAccels}},
		},
		Status: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{ID: "g0", Manufacturer: manufacturer, Accelerators: statusAccels}},
		},
	}
}

func TestNodeDevicesFeasibilityPartitioned(t *testing.T) {
	none := workercore.DeviceAllocationModeNone
	partitioned := workercore.DeviceAllocationModePartitioned
	exclusive := workercore.DeviceAllocationModeExclusive

	cases := []struct {
		name    string
		devices []workercore.Devices
		profile string
		cards   int32
		want    kueue.CheckState
	}{
		{
			name: "enough cards with a free placement is ready",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: none, remainingProfiles: map[string]int32{"1g.10gb": 7}, placementsCached: true},
				physicalCard{id: "g1", mode: partitioned, remainingProfiles: map[string]int32{"1g.10gb": 4}, placementsCached: true},
			)},
			profile: "1g.10gb", cards: 2, want: kueue.CheckStateReady,
		},
		{
			name: "profile full everywhere retries (not reject)",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: partitioned, remainingProfiles: map[string]int32{"1g.10gb": 0}, placementsCached: true},
			)},
			profile: "1g.10gb", cards: 1, want: kueue.CheckStateRetry,
		},
		{
			name: "no placements cached is ledger-not-ready retry",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: none, remainingProfiles: map[string]int32{}, placementsCached: false},
			)},
			profile: "1g.10gb", cards: 1, want: kueue.CheckStateRetry,
		},
		{
			name: "exclusive-held partitioned card is not a candidate",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: exclusive, remainingProfiles: map[string]int32{"1g.10gb": 7}, placementsCached: true},
			)},
			profile: "1g.10gb", cards: 1, want: kueue.CheckStateRetry,
		},
		{
			name: "short by one card retries",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: none, remainingProfiles: map[string]int32{"1g.10gb": 1}, placementsCached: true},
			)},
			profile: "1g.10gb", cards: 2, want: kueue.CheckStateRetry,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, _ := nodeDevicesFeasibility(c.devices, []familyDemand{
				{family: nodefeature.ResourceFamilyPartitioned, cards: c.cards, profile: c.profile},
			})
			assert.Equal(t, c.want, got)
		})
	}
}

// TestNodeDevicesFeasibilityPartitionedVendorSpelledLedger pins the profile-name boundary at
// the feasibility check. The demand names the profile as its published resource key spells it,
// while the card's per-profile ledger is keyed as the manufacturer's own library spells it —
// comparing the two verbatim matches no row, reads as a full card, and would leave every
// request for a published profile in Retry forever on a pool with room for it. The verdict
// still quotes the published name, since that is the name the user wrote.
func TestNodeDevicesFeasibilityPartitionedVendorSpelledLedger(t *testing.T) {
	roomyCard := []workercore.Devices{physicalDevicesOf(nodefeature.ManufacturerTHead, physicalCard{
		id: "p0", mode: workercore.DeviceAllocationModeNone,
		remainingProfiles: map[string]int32{"4g48gb": 2}, placementsCached: true,
	})}

	got, msg := nodeDevicesFeasibility(roomyCard, []familyDemand{
		{family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "4g.48gb"},
	})
	assert.Equal(t, kueue.CheckStateReady, got, "a published profile name against a vendor-spelled ledger")
	assert.Contains(t, msg, `"4g.48gb"`, "the verdict quotes the name the user wrote")

	// The conversion resolves one spelling to the other, it does not make every name match.
	got, _ = nodeDevicesFeasibility(roomyCard, []familyDemand{
		{family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "8g.96gb"},
	})
	assert.Equal(t, kueue.CheckStateRetry, got, "a profile the card does not offer")
}

// TestNodeDevicesFeasibilityPartitionedCountsPlacements pins that a partition demand is counted
// in instances, not cards: one card with several free placements of the profile hosts several
// replicas, which is what RemainingProfiles reports and what the placement-authoritative Allocate
// does. Counting one instance per card would strand every replica after the first in Retry
// forever on a node with room for them all.
func TestNodeDevicesFeasibilityPartitionedCountsPlacements(t *testing.T) {
	oneRoomyCard := []workercore.Devices{physicalDevices(physicalCard{
		id: "g0", mode: workercore.DeviceAllocationModeNone,
		remainingProfiles: map[string]int32{"1g.10gb": 3}, placementsCached: true,
	})}

	for _, want := range []struct {
		instances int32
		state     kueue.CheckState
	}{
		{instances: 1, state: kueue.CheckStateReady},
		{instances: 3, state: kueue.CheckStateReady},
		{instances: 4, state: kueue.CheckStateRetry},
	} {
		got, _ := nodeDevicesFeasibility(oneRoomyCard, []familyDemand{
			{family: nodefeature.ResourceFamilyPartitioned, cards: want.instances, profile: "1g.10gb"},
		})
		assert.Equal(t, want.state, got, "%d instances against one card with 3 free placements", want.instances)
	}

	// Two partition demands of one Workload must not spend the same placements twice.
	got, _ := nodeDevicesFeasibility(oneRoomyCard, []familyDemand{
		{family: nodefeature.ResourceFamilyPartitioned, cards: 2, profile: "1g.10gb"},
		{family: nodefeature.ResourceFamilyPartitioned, cards: 2, profile: "1g.10gb", unitsPerCard: 1},
	})
	assert.Equal(t, kueue.CheckStateRetry, got, "4 instances across two demands exceed the 3 free placements")
}

// TestNodeDevicesFeasibilityPartitionedMessages pins that "ledger not ready" carries a message
// distinct from a genuine "profile full", so an operator can tell an upgrade window from real
// contention, and that a pool with no partitioned card at all reuses neither.
func TestNodeDevicesFeasibilityPartitionedMessages(t *testing.T) {
	notReady := []workercore.Devices{physicalDevices(
		physicalCard{id: "g0", mode: workercore.DeviceAllocationModeNone, placementsCached: false},
	)}
	full := []workercore.Devices{physicalDevices(
		physicalCard{id: "g0", mode: workercore.DeviceAllocationModePartitioned, remainingProfiles: map[string]int32{"1g.10gb": 0}, placementsCached: true},
	)}
	// A pool with NO partitioned card at all is a different condition from a rollout window,
	// and must carry its own message.
	noPartition := []workercore.Devices{devicesWithRemaining(int32(nodefeature.ResourceMaxUnits))}

	demand := []familyDemand{{family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "1g.10gb"}}
	_, notReadyMsg := nodeDevicesFeasibility(notReady, demand)
	_, fullMsg := nodeDevicesFeasibility(full, demand)
	_, noPartitionMsg := nodeDevicesFeasibility(noPartition, demand)

	assert.Equal(t, partitionLedgerNotReadyMessage, notReadyMsg)
	assert.NotEqual(t, notReadyMsg, fullMsg, "ledger-not-ready and profile-full messages must differ")
	assert.NotEqual(t, partitionLedgerNotReadyMessage, noPartitionMsg,
		"a pool with no partitioned card must not reuse the device-manager-rollout message")
	assert.Contains(t, noPartitionMsg, "no partitioned card")
}

// workloadRequesting builds a single-podset Workload whose one container requests
// the given resources, repeated across podCount pods.
func workloadRequesting(podCount int32, reqs map[core.ResourceName]string) *kueue.Workload {
	rl := core.ResourceList{}
	for n, v := range reqs {
		rl[n] = resource.MustParse(v)
	}
	return &kueue.Workload{
		Spec: kueue.WorkloadSpec{
			PodSets: []kueue.PodSet{{
				Name:  "main",
				Count: podCount,
				Template: core.PodTemplateSpec{
					Spec: core.PodSpec{
						Containers: []core.Container{{Name: "c", Resources: core.ResourceRequirements{Requests: rl}}},
					},
				},
			}},
		},
	}
}

func TestParseFamilyDemands(t *testing.T) {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	slicedCard := base + nodefeature.SlicedResourceNameSuffix
	slicedUnits := base + nodefeature.SlicedUnitsResourceNameSuffix
	slicedCores := base + nodefeature.SlicedCoresPercentageResourceNameSuffix
	sharedCard := base + nodefeature.SharedResourceNameSuffix
	partitionCard := base + nodefeature.PartitionedResourceNameSuffix
	partitionUnits := base + nodefeature.PartitionedUnitsResourceNameSuffix
	partitionProfileKey := string(nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb"))

	cases := []struct {
		name     string
		podCount int32
		reqs     map[core.ResourceName]string
		want     []familyDemand
	}{
		{
			name:     "exclusive single pod, many cards",
			podCount: 1,
			reqs:     map[core.ResourceName]string{core.ResourceName(base): "5"},
			want:     []familyDemand{{family: nodefeature.ResourceFamilyExclusive, cards: 5}},
		},
		{
			name:     "exclusive many pods, one card each",
			podCount: 5,
			reqs:     map[core.ResourceName]string{core.ResourceName(base): "1"},
			want:     []familyDemand{{family: nodefeature.ResourceFamilyExclusive, cards: 5}},
		},
		{
			name:     "sliced reads card count and per-card units, ignores cores-percentage",
			podCount: 1,
			reqs: map[core.ResourceName]string{
				core.ResourceName(slicedCard):  "2",
				core.ResourceName(slicedUnits): "320000",
				core.ResourceName(slicedCores): "100",
			},
			want: []familyDemand{{family: nodefeature.ResourceFamilySliced, cards: 2, unitsPerCard: 320000}},
		},
		{
			name:     "shared",
			podCount: 1,
			reqs:     map[core.ResourceName]string{core.ResourceName(sharedCard): "3"},
			want:     []familyDemand{{family: nodefeature.ResourceFamilyShared, cards: 3}},
		},
		{
			name:     "partitioned reads profile, card count and per-card units",
			podCount: 1,
			reqs: map[core.ResourceName]string{
				core.ResourceName(partitionCard):       "1",
				core.ResourceName(partitionProfileKey): "1",
				core.ResourceName(partitionUnits):      "200000",
			},
			want: []familyDemand{{
				family: nodefeature.ResourceFamilyPartitioned, cards: 1,
				unitsPerCard: 200000, profile: "1g.10gb",
			}},
		},
		{
			name:     "no accelerator request",
			podCount: 2,
			reqs:     map[core.ResourceName]string{core.ResourceCPU: "4"},
			want:     nil,
		},
		{
			name:     "a counting key with no card request behind it demands nothing",
			podCount: 1,
			reqs:     map[core.ResourceName]string{core.ResourceName(slicedUnits): "320000"},
			want:     nil,
		},
		{
			name:     "a podset of zero pods demands nothing",
			podCount: 0,
			reqs:     map[core.ResourceName]string{core.ResourceName(base): "5"},
			want:     nil,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, parseFamilyDemands(workloadRequesting(c.podCount, c.reqs)))
		})
	}
}

// TestParseFamilyDemands_TakesStrictestUnitsWithinAPodSet pins that when a podset's containers
// carry different per-card units, feasibility is checked against the largest (strictest) demand
// rather than whichever the map happened to iterate last.
func TestParseFamilyDemands_TakesStrictestUnitsWithinAPodSet(t *testing.T) {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	slicedCard := base + nodefeature.SlicedResourceNameSuffix
	slicedUnits := base + nodefeature.SlicedUnitsResourceNameSuffix

	mkCtr := func(card, units string) core.Container {
		return core.Container{Name: "c", Resources: core.ResourceRequirements{Requests: core.ResourceList{
			core.ResourceName(slicedCard):  resource.MustParse(card),
			core.ResourceName(slicedUnits): resource.MustParse(units),
		}}}
	}
	wl := &kueue.Workload{Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
		Name:     "main",
		Count:    1,
		Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{mkCtr("1", "160000"), mkCtr("1", "480000")}}},
	}}}}

	assert.Equal(t, []familyDemand{
		{family: nodefeature.ResourceFamilySliced, cards: 2, unitsPerCard: 480000},
	}, parseFamilyDemands(wl), "cards summed across containers, strictest (max) per-card units")
}

// TestParseFamilyDemands_KeepsPodSetCorrelation pins the reason the demand is a list rather than
// a single tuple: two podsets asking for different per-card budgets must stay separate, so the
// small one is not gated on the large one's budget — the "max units against summed cards"
// conflation this model replaces.
func TestParseFamilyDemands_KeepsPodSetCorrelation(t *testing.T) {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	slicedCard := core.ResourceName(base + nodefeature.SlicedResourceNameSuffix)
	slicedUnits := core.ResourceName(base + nodefeature.SlicedUnitsResourceNameSuffix)

	mkPodSet := func(name string, count int32, units string) kueue.PodSet {
		return kueue.PodSet{
			Name: kueue.PodSetReference(name), Count: count,
			Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
				Name: "c", Resources: core.ResourceRequirements{Requests: core.ResourceList{
					slicedCard:  resource.MustParse("1"),
					slicedUnits: resource.MustParse(units),
				}},
			}}}},
		}
	}
	wl := &kueue.Workload{Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{
		mkPodSet("small", 2, "160000"),
		mkPodSet("large", 1, "1280000"),
	}}}

	assert.Equal(t, []familyDemand{
		{family: nodefeature.ResourceFamilySliced, cards: 1, unitsPerCard: 1280000},
		{family: nodefeature.ResourceFamilySliced, cards: 2, unitsPerCard: 160000},
	}, parseFamilyDemands(wl), "the two budgets stay separate, ordered strictest first")
}

// TestParseFamilyDemands_MergesIdenticalPodSets pins the other half: podsets that agree on their
// per-card budget and profile collapse to the single tuple per family the model promises.
func TestParseFamilyDemands_MergesIdenticalPodSets(t *testing.T) {
	base := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)

	mkPodSet := func(name string, count int32) kueue.PodSet {
		return kueue.PodSet{
			Name: kueue.PodSetReference(name), Count: count,
			Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
				Name:      "c",
				Resources: core.ResourceRequirements{Requests: core.ResourceList{base: resource.MustParse("1")}},
			}}}},
		}
	}
	wl := &kueue.Workload{Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{mkPodSet("a", 2), mkPodSet("b", 3)}}}

	assert.Equal(t, []familyDemand{
		{family: nodefeature.ResourceFamilyExclusive, cards: 5},
	}, parseFamilyDemands(wl))
}

// TestParseFamilyDemands_InitContainerPartition pins that a partition request carried only on an
// init container is still parsed (profile + card count), so feasibility gates it — getAllocatingPod
// attributes init containers too, and the Pod webhook folds them.
func TestParseFamilyDemands_InitContainerPartition(t *testing.T) {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	partitionCard := core.ResourceName(base + nodefeature.PartitionedResourceNameSuffix)
	partitionProfileKey := nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb")

	wl := &kueue.Workload{Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
		Name:  "main",
		Count: 1,
		Template: core.PodTemplateSpec{Spec: core.PodSpec{
			InitContainers: []core.Container{{Name: "init", Resources: core.ResourceRequirements{Requests: core.ResourceList{
				partitionCard:       resource.MustParse("1"),
				partitionProfileKey: resource.MustParse("1"),
			}}}},
			Containers: []core.Container{{Name: "main"}},
		}},
	}}}}

	assert.Equal(t, []familyDemand{
		{family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "1g.10gb"},
	}, parseFamilyDemands(wl), "card count and profile read from the init container")
}

// TestParseFamilyDemands_LimitsOnly pins that accelerator keys specified only under
// resources.limits (the conventional place for extended resources) are still parsed — a
// requests-only scan would miss them and wrongly mark feasibility Ready.
func TestParseFamilyDemands_LimitsOnly(t *testing.T) {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	partitionCard := core.ResourceName(base + nodefeature.PartitionedResourceNameSuffix)
	partitionUnits := core.ResourceName(base + nodefeature.PartitionedUnitsResourceNameSuffix)
	partitionProfileKey := nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb")

	wl := &kueue.Workload{Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
		Name:  "main",
		Count: 1,
		Template: core.PodTemplateSpec{Spec: core.PodSpec{
			Containers: []core.Container{{Name: "main", Resources: core.ResourceRequirements{Limits: core.ResourceList{
				partitionCard:       resource.MustParse("1"),
				partitionProfileKey: resource.MustParse("1"),
				partitionUnits:      resource.MustParse("200000"),
			}}}},
		}},
	}}}}

	assert.Equal(t, []familyDemand{{
		family: nodefeature.ResourceFamilyPartitioned, cards: 1,
		unitsPerCard: 200000, profile: "1g.10gb",
	}}, parseFamilyDemands(wl))
}

func TestCandidateDevices(t *testing.T) {
	poolLabels := map[string]string{"feature.gpustack.ai/nvidia": "true", "kubernetes.io/os": "linux"}
	// The flavor's nodeLabels also carry a ".count" node-batch pin (for Kueue scheduling) that the
	// DeviceManager deliberately omits from a Devices object's selector labels; candidateDevices must
	// drop it, or MatchingLabels would find no Devices and the AdmissionCheck would wrongly Retry.
	rfNodeLabels := map[string]string{
		"feature.gpustack.ai/nvidia":                     "true",
		"kubernetes.io/os":                               "linux",
		"acceleratable.feature.gpustack.ai/nvidia.count": "1",
	}
	rf := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{Name: "gpu-pool"},
		Spec:       kueue.ResourceFlavorSpec{NodeLabels: rfNodeLabels},
	}
	inPool := &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: poolLabels}}
	otherOS := &workercore.Devices{ObjectMeta: meta.ObjectMeta{
		Name:   "node-b",
		Labels: map[string]string{"feature.gpustack.ai/nvidia": "true", "kubernetes.io/os": "windows"},
	}}

	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(rf, inPool, otherOS).Build()
	r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

	wl := &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "w"},
		Status: kueue.WorkloadStatus{
			Admission: &kueue.Admission{
				PodSetAssignments: []kueue.PodSetAssignment{{
					Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{"credits": "gpu-pool"},
				}},
			},
		},
	}

	devices, err := r.candidateDevices(context.Background(), wl)
	assert.NoError(t, err)
	names := make([]string, 0, len(devices))
	for i := range devices {
		names = append(names, devices[i].Name)
	}
	assert.Equal(t, []string{"node-a"}, names, "only the same-pool node's Devices are candidates")
}

func TestNodeDevicesAdmissionCheckReconciler(t *testing.T) {
	ours := &kueue.AdmissionCheck{
		ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName},
		Spec:       kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName},
	}
	foreign := &kueue.AdmissionCheck{
		ObjectMeta: meta.ObjectMeta{Name: "foreign"},
		Spec:       kueue.AdmissionCheckSpec{ControllerName: "someone-else"},
	}
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(ours, foreign).
		WithStatusSubresource(&kueue.AdmissionCheck{}).
		Build()
	r := &NodeDevicesAdmissionCheckReconciler{Client: cli}

	// Our check is marked Active.
	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: _NodeDevicesAdmissionCheckName}})
	assert.NoError(t, err)
	got := new(kueue.AdmissionCheck)
	assert.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: _NodeDevicesAdmissionCheckName}, got))
	assert.True(t, kubemeta.IsConditionTrue(got.Status.Conditions, kueue.AdmissionCheckActive))

	// A check owned by another controller is left untouched.
	_, err = r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: "foreign"}})
	assert.NoError(t, err)
	gotForeign := new(kueue.AdmissionCheck)
	assert.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "foreign"}, gotForeign))
	assert.Empty(t, gotForeign.Status.Conditions)
}

// slicedGateWorkload builds a Workload requesting a 960k-unit exclusive slice of one NVIDIA card,
// already assigned to the gpu-pool flavor. It always carries QuotaReserved=True — the state in
// which this controller evaluates a Workload — plus extraConds, and holds check in this
// controller's AdmissionCheck slot.
func slicedGateWorkload(check kueue.CheckState, extraConds ...meta.Condition) *kueue.Workload {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	slicedCard := core.ResourceName(base + nodefeature.SlicedResourceNameSuffix)
	slicedUnits := core.ResourceName(base + nodefeature.SlicedUnitsResourceNameSuffix)

	conds := append([]meta.Condition{{
		Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue,
		Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: meta.Now(),
	}}, extraConds...)

	return &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "w"},
		Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
			Name: "main", Count: 1,
			Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
				Name: "c",
				Resources: core.ResourceRequirements{Requests: core.ResourceList{
					slicedCard:  resource.MustParse("1"),
					slicedUnits: resource.MustParse("960000"),
				}},
			}}}},
		}}},
		Status: kueue.WorkloadStatus{
			Conditions: conds,
			Admission: &kueue.Admission{PodSetAssignments: []kueue.PodSetAssignment{{
				Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{"credits": "gpu-pool"},
			}}},
			AdmissionChecks: []kueue.AdmissionCheckState{{
				Name: _NodeDevicesAdmissionCheckName, State: check,
			}},
		},
	}
}

// reconcileSlicedGate seeds the one-node pool the Workload is assigned to, reconciles it once, and
// reports the state this controller's check holds afterwards. The pool's single card has only 640k
// units free: the 960k slicedGateWorkload asks for no longer fits, so the gate answers Retry for
// every Workload it is allowed to evaluate.
func reconcileSlicedGate(t *testing.T, wl *kueue.Workload) kueue.CheckState {
	t.Helper()

	poolLabels := map[string]string{"feature.gpustack.ai/nvidia": "true"}
	rf := &kueue.ResourceFlavor{ObjectMeta: meta.ObjectMeta{Name: "gpu-pool"}, Spec: kueue.ResourceFlavorSpec{NodeLabels: poolLabels}}
	check := &kueue.AdmissionCheck{ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName}, Spec: kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName}}
	devs := devicesWithRemaining(640000)
	devs.ObjectMeta = meta.ObjectMeta{Name: "node-a", Labels: poolLabels}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(rf, check, &devs, wl).
		WithStatusSubresource(&kueue.Workload{}).
		Build()
	r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "default", Name: "w"}})
	assert.NoError(t, err)

	got := new(kueue.Workload)
	assert.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "default", Name: "w"}, got))
	for _, cs := range got.Status.AdmissionChecks {
		if cs.Name == _NodeDevicesAdmissionCheckName {
			return cs.State
		}
	}
	return ""
}

// TestNodeDevicesAdmission_AdmittedWorkloadNotSelfEvicted guards the > 50% single-card
// self-eviction: once a Workload is admitted, its own slice is already subtracted from the
// per-card ledger, so re-checking must not count that allocation against itself and flip the
// check to Retry (which would evict the running Workload in a recreate loop). A not-yet-admitted
// Workload with the same ledger must still be held (Retry) — the gate only fires before admission.
func TestNodeDevicesAdmission_AdmittedWorkloadNotSelfEvicted(t *testing.T) {
	admitted := meta.Condition{
		Type: kueue.WorkloadAdmitted, Status: meta.ConditionTrue,
		Reason: "Admitted", Message: "admitted", LastTransitionTime: meta.Now(),
	}
	testCases := []struct {
		name string
		wl   *kueue.Workload
		want kueue.CheckState
		why  string
	}{
		{
			name: "admitted workload is not self-evicted",
			wl:   slicedGateWorkload(kueue.CheckStateReady, admitted),
			want: kueue.CheckStateReady,
			why:  "admitted workload's check must stay Ready, not flip to Retry",
		},
		{
			name: "not-yet-admitted workload is still gated",
			wl:   slicedGateWorkload(kueue.CheckStateReady),
			want: kueue.CheckStateRetry,
			why:  "before admission the gate must hold Retry when the slice cannot fit",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reconcileSlicedGate(t, tc.wl), tc.why)
		})
	}
}

// TestNodeDevicesAdmission_EvictedWorkloadKeepsKueuesResetCheck guards the eviction window: Kueue
// sets Evicted=True and resets every check to Pending in one patch, but drops the quota reservation
// only later, from the job reconciler. A Workload caught in between still reports a reservation, so
// evaluating it would overwrite Kueue's fresh Pending with Retry. Once such a write wins the race
// against Kueue's own the Workload is wedged for good: Kueue's eviction path short-circuits while
// Evicted is set, its scheduler refuses to reserve quota while a check is Retry, and this
// controller then skips the Workload for want of a reservation. A Workload that is not evicted must
// still be held (Retry) — the gate itself is unchanged.
func TestNodeDevicesAdmission_EvictedWorkloadKeepsKueuesResetCheck(t *testing.T) {
	evicted := meta.Condition{
		Type: kueue.WorkloadEvicted, Status: meta.ConditionTrue,
		Reason: kueue.WorkloadEvictedByAdmissionCheck, Message: "evicted", LastTransitionTime: meta.Now(),
	}
	testCases := []struct {
		name string
		wl   *kueue.Workload
		want kueue.CheckState
		why  string
	}{
		{
			name: "evicted workload keeps kueue's reset",
			wl:   slicedGateWorkload(kueue.CheckStatePending, evicted),
			want: kueue.CheckStatePending,
			why:  "an evicted workload must keep the Pending Kueue reset it to, or the retry loop deadlocks",
		},
		{
			name: "live workload is still gated",
			wl:   slicedGateWorkload(kueue.CheckStatePending),
			want: kueue.CheckStateRetry,
			why:  "a live workload whose slice cannot fit must still be held",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reconcileSlicedGate(t, tc.wl), tc.why)
		})
	}
}
