package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueadmissioncheck "sigs.k8s.io/kueue/pkg/util/admissioncheck"

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

// oneFlavorPool presents a fixture's ledgers the way a single-podset Workload's pool reaches the
// check: every node claimed by ONE assigned flavor, whose accelerator key is derived per device
// group so the flavor covers every card the fixture carries. It is the shape every case predating
// multi-role P/D exercises, so those cases keep their data unchanged.
//
// The flavor reference is empty here and so are the demands the cases build, and they match by
// plain equality — the same rule production uses with real names on both sides. Nothing treats
// empty as a wildcard.
func oneFlavorPool(devices []workercore.Devices) []scopedDevices {
	pool := make([]scopedDevices, 0, len(devices))
	for i := range devices {
		sd := scopedDevices{devices: devices[i]}
		seen := sets.New[string]()
		for gi := range devices[i].Status.Groups {
			g := &devices[i].Status.Groups[gi]
			if key := acceleratorKeyOf(g.Manufacturer, g.ID); !seen.Has(key) {
				seen.Insert(key)
				sd.scopes = append(sd.scopes, flavorScope{acceleratorKey: key})
			}
		}
		pool = append(pool, sd)
	}
	return pool
}

// feasibilityOfOnePool runs the check over a single-flavor pool: every card under one flavor, and
// every demand assigned to it. It keeps the pre-P/D cases exercising exactly what they always did
// while the check itself became per-role.
func feasibilityOfOnePool(devices []workercore.Devices, demands []familyDemand) (kueue.CheckState, string) {
	return nodeDevicesFeasibility(oneFlavorPool(devices), demands)
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
			got, _ := feasibilityOfOnePool(c.devices, c.demands)
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
		// wantMessage, when set, pins WHY the verdict came out that way. A state alone cannot:
		// several independent exclusions produce Retry, so a case claiming a card was excluded
		// FROM A POPULATION passes just as well when it was excluded for having no room.
		wantMessage string
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
			// The card must be excluded from the partitioned POPULATION, not merely found full.
			// Were it wrongly counted as partitioned, its empty profile ledger would yield Retry
			// too — so without this the case holds for a reason it does not claim.
			wantMessage: "no partitioned card",
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
			got, msg := feasibilityOfOnePool(c.devices, []familyDemand{c.demand})
			assert.Equal(t, c.want, got)
			if c.wantMessage != "" {
				assert.Contains(t, msg, c.wantMessage, "the verdict must state the exclusion it claims")
			}
		})
	}
}

// TestNodeDevicesFeasibilityConsumesCardsPerDemand pins that two demands of one Workload never
// both claim the same card: with a single free card, one exclusive card is placeable and two
// are not, even when the two demands arrive as separate correlated tuples.
func TestNodeDevicesFeasibilityConsumesCardsPerDemand(t *testing.T) {
	whole := int32(nodefeature.ResourceMaxUnits)
	one := []workercore.Devices{devicesWithRemaining(whole)}

	got, _ := feasibilityOfOnePool(one, []familyDemand{
		{family: nodefeature.ResourceFamilyExclusive, cards: 1},
	})
	assert.Equal(t, kueue.CheckStateReady, got)

	got, _ = feasibilityOfOnePool(one, []familyDemand{
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
			// One free placement each, so the two instances MUST come from two different cards.
			// With room to spare on the first, the fit satisfied both there and returned before
			// ever examining the second — the traversal this case is named for went unexercised.
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: none, remainingProfiles: map[string]int32{"1g.10gb": 1}, placementsCached: true},
				physicalCard{id: "g1", mode: partitioned, remainingProfiles: map[string]int32{"1g.10gb": 1}, placementsCached: true},
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
			got, _ := feasibilityOfOnePool(c.devices, []familyDemand{
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

	got, msg := feasibilityOfOnePool(roomyCard, []familyDemand{
		{family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "4g.48gb"},
	})
	assert.Equal(t, kueue.CheckStateReady, got, "a published profile name against a vendor-spelled ledger")
	assert.Contains(t, msg, `"4g.48gb"`, "the verdict quotes the name the user wrote")

	// The conversion resolves one spelling to the other, it does not make every name match.
	got, _ = feasibilityOfOnePool(roomyCard, []familyDemand{
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
		got, _ := feasibilityOfOnePool(oneRoomyCard, []familyDemand{
			{family: nodefeature.ResourceFamilyPartitioned, cards: want.instances, profile: "1g.10gb"},
		})
		assert.Equal(t, want.state, got, "%d instances against one card with 3 free placements", want.instances)
	}

	// Two partition demands of one Workload must not spend the same placements twice.
	got, _ := feasibilityOfOnePool(oneRoomyCard, []familyDemand{
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
	_, notReadyMsg := feasibilityOfOnePool(notReady, demand)
	_, fullMsg := feasibilityOfOnePool(full, demand)
	_, noPartitionMsg := feasibilityOfOnePool(noPartition, demand)

	assert.Equal(t, partitionLedgerNotReadyMessage(nil), notReadyMsg)
	assert.NotEqual(t, notReadyMsg, fullMsg, "ledger-not-ready and profile-full messages must differ")
	assert.NotEqual(t, partitionLedgerNotReadyMessage(nil), noPartitionMsg,
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

// creditsResource is the resource name a podset's flavor assignment is keyed by: Kueue accounts
// the manufacturer's credits, which the chart's resource transformation puts in place of every raw
// accelerator key. A shorthand like "credits" would leave every fixture exercising the fallback in
// assignedFlavor rather than the lookup real Workloads take.
var creditsResource = nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA)

// fromPodSets is the provenance a demand carries when read from the named podsets. A demand merged
// from several carries all of their names, which is what lets a Retry verdict say which role fell
// short instead of only that the Workload did.
func fromPodSets(names ...string) []kueue.PodSetReference {
	refs := make([]kueue.PodSetReference, len(names))
	for i, n := range names {
		refs[i] = kueue.PodSetReference(n)
	}
	return refs
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
			// workloadRequesting builds one podset named "main", so every demand a case yields is
			// read from it. Stamping the provenance here rather than in each case keeps the table
			// declarative — the cases are about what is read off the containers, not about which
			// podset the shared fixture happens to name.
			want := make([]familyDemand, len(c.want))
			for i, d := range c.want {
				d.podSets = []kueue.PodSetReference{"main"}
				want[i] = d
			}
			if c.want == nil {
				want = nil
			}
			assert.Equal(t, want, parseFamilyDemands(workloadRequesting(c.podCount, c.reqs)))
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
		{family: nodefeature.ResourceFamilySliced, cards: 2, unitsPerCard: 480000, podSets: fromPodSets("main")},
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
		{family: nodefeature.ResourceFamilySliced, cards: 1, unitsPerCard: 1280000, podSets: fromPodSets("large")},
		{family: nodefeature.ResourceFamilySliced, cards: 2, unitsPerCard: 160000, podSets: fromPodSets("small")},
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
		{family: nodefeature.ResourceFamilyExclusive, cards: 5, podSets: fromPodSets("a", "b")},
	}, parseFamilyDemands(wl), "merged podsets accumulate BOTH names, so a verdict can say which roles it is about")
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
		{family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "1g.10gb", podSets: fromPodSets("main")},
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
		unitsPerCard: 200000, profile: "1g.10gb", podSets: fromPodSets("main"),
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
					Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "gpu-pool"},
				}},
			},
		},
	}

	pool, err := r.candidateDevices(context.Background(), wl)
	assert.NoError(t, err)
	names := make([]string, 0, len(pool))
	for i := range pool {
		names = append(names, pool[i].devices.Name)
	}
	require.Equal(t, []string{"node-a"}, names, "only the same-pool node's Devices are candidates")

	// Each candidate node carries the flavor that claimed it, so the feasibility fit can tell a
	// role's own cards from another role's. This flavor pins no bare accelerator key, so it covers
	// no card group — which is the point of reading the key rather than the node match alone.
	assert.Equal(t,
		[]flavorScope{{flavor: "gpu-pool", acceleratorKey: ""}},
		pool[0].scopes,
		"the node is scoped to the flavor that matched it")
}

// TestAssignedFlavorPicksTheAcceleratorsOwnResource pins WHICH of a podset's assigned flavors
// scopes its accelerator demand. The assignment maps one flavor per covered resource, so a queue
// covering both cpu and a manufacturer's credits gives the podset two — and only the credits one
// reaches any card, since a CPU flavor pins no accelerator key.
//
// The fixture names the CPU flavor so it sorts FIRST, which is what a name-ordered choice would
// take: it covers no card, so every accelerator workload of that pool would sit in Retry with its
// cards free. This operator's own queues cover one resource and are unaffected either way, so the
// case is written for the admin-authored queue that is the only way to reach two.
func TestAssignedFlavorPicksTheAcceleratorsOwnResource(t *testing.T) {
	credits := nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA)
	assignment := func(flavors map[core.ResourceName]kueue.ResourceFlavorReference) *kueue.Workload {
		return &kueue.Workload{Status: kueue.WorkloadStatus{Admission: &kueue.Admission{
			PodSetAssignments: []kueue.PodSetAssignment{{Name: "prefill", Flavors: flavors}},
		}}}
	}

	both := assignment(map[core.ResourceName]kueue.ResourceFlavorReference{
		core.ResourceCPU: "aaa-cpu-flavor",
		credits:          "zzz-gpu-flavor",
	})
	assert.Equal(t, kueue.ResourceFlavorReference("zzz-gpu-flavor"),
		assignedFlavor(both, "prefill"),
		"the flavor assigned for the accelerator's own resource scopes the demand, not the first name")

	// The ordinary shape: one covered resource, so there is nothing to choose between.
	only := assignment(map[core.ResourceName]kueue.ResourceFlavorReference{credits: "zzz-gpu-flavor"})
	assert.Equal(t, kueue.ResourceFlavorReference("zzz-gpu-flavor"), assignedFlavor(only, "prefill"))

	// An assignment carrying only a CPU flavor resolves to NOTHING, not to that flavor. A CPU
	// flavor covers no card, so falling back to it would hold the Workload while reporting a
	// capacity shortage against a pool short of nothing; empty routes it to the hold that states
	// the assignment as the cause. An ordinary CPU-only Workload is unaffected either way — it
	// parses no accelerator demand, so no demand carries this.
	cpu := assignment(map[core.ResourceName]kueue.ResourceFlavorReference{core.ResourceCPU: "aaa-cpu-flavor"})
	assert.Empty(t, assignedFlavor(cpu, "prefill"),
		"a flavor assigned for another resource is not this demand's flavor")

	// Two manufacturers' credits, two flavors: not decidable, so nothing is chosen. A podset may
	// name two bases of one family — the webhook forbids two families, not two bases — and a demand
	// merges them into one card count, so scoping that count to either flavor would count cards of
	// a model the request was not placed on. Ambiguity is answered the way flavorAcceleratorKey
	// answers it, with a hold.
	twoManufacturers := assignment(map[core.ResourceName]kueue.ResourceFlavorReference{
		credits: "zzz-gpu-flavor",
		nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerAMD): "aaa-gpu-flavor",
	})
	assert.Empty(t, assignedFlavor(twoManufacturers, "prefill"),
		"two accelerator flavors are no more decidable than none")

	// Two credits resources naming the SAME flavor are ambiguous too, and this is the dangerous
	// shape: one reference reads as unambiguous while a flavor pins ONE accelerator key, so its
	// cards are one model's — and the demand's count spans two. A Kueue resource group may cover
	// both manufacturers' credits and quote a single flavor for each, so this is what an admin's
	// queue produces most easily. Counting the accelerator resources is what refuses it; comparing
	// the references does not.
	sameFlavor := assignment(map[core.ResourceName]kueue.ResourceFlavorReference{
		credits: "zzz-gpu-flavor",
		nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerAMD): "zzz-gpu-flavor",
	})
	assert.Empty(t, assignedFlavor(sameFlavor, "prefill"),
		"one flavor cannot answer for two manufacturers' cards, however it is referenced")
}

// TestDesiredCheckStatesCarriesEveryOwnedCheck pins what the apply payload must contain when this
// controller owns more than one AdmissionCheck.
//
// The write is a server-side apply and admissionChecks is a list keyed by name, so an entry this
// field owner applied before and then omits is an entry it has stopped claiming — the API server
// prunes the fields it owned there. Sending only the checks that differ would therefore reset every
// settled sibling each time one of them changed. The fake client does not implement apply-time
// pruning, so the payload is what has to be asserted; a test through Reconcile cannot tell a
// complete payload from a partial one.
func TestDesiredCheckStatesCarriesEveryOwnedCheck(t *testing.T) {
	const settled, pending = kueue.AdmissionCheckReference("settled"), kueue.AdmissionCheckReference("pending")
	message := verdictMessage(kueue.CheckStateRetry, nil)
	wl := &kueue.Workload{Status: kueue.WorkloadStatus{AdmissionChecks: []kueue.AdmissionCheckState{
		{
			Name: settled, State: kueue.CheckStateRetry, Message: message,
			RequeueAfterSeconds: ptr.To(_NodeDevicesRetryAfterSeconds),
		},
		{Name: pending, State: kueue.CheckStatePending},
	}}}

	desired, changed := desiredCheckStates(wl, []kueue.AdmissionCheckReference{settled, pending},
		kueue.CheckStateRetry, message)
	assert.True(t, changed, "one of the two checks differs, so a write is needed")
	require.Len(t, desired, 2, "the payload claims every owned check, not only the one that differs")
	for _, acs := range desired {
		assert.Equal(t, kueue.CheckStateRetry, acs.State, string(acs.Name))
		assert.Equal(t, message, acs.Message, string(acs.Name))
		require.NotNil(t, acs.RequeueAfterSeconds, string(acs.Name))
		assert.Equal(t, _NodeDevicesRetryAfterSeconds, *acs.RequeueAfterSeconds, string(acs.Name))
	}

	// Nothing differs: no write at all, so no apply can prune anything.
	_, changed = desiredCheckStates(wl, []kueue.AdmissionCheckReference{settled},
		kueue.CheckStateRetry, message)
	assert.False(t, changed, "a settled check alone needs no write")
}

// TestFlavorAcceleratorKey pins which label the card population is keyed on. The bare
// "acceleratable.feature.gpustack.ai/<key>=true" entry is the model identity; its ".count" sibling
// pins a node batch and its ".product"/".memory" notes are descriptors, so reading any of those as
// the key would scope a role to a population that is not a model.
func TestFlavorAcceleratorKey(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name: "accelerated flavor uses the bare feature key",
			labels: map[string]string{
				"gpustack.ai/managed":                                  "true",
				"kubernetes.io/os":                                     "linux",
				"general.feature.gpustack.ai/amd-epyc":                 "true",
				"acceleratable.feature.gpustack.ai/nvidia-h20":         "true",
				"acceleratable.feature.gpustack.ai/nvidia-h20.count":   "8",
				"acceleratable.feature.gpustack.ai/nvidia-h20.product": "NVIDIA-H20",
			},
			want: "nvidia-h20",
		},
		{
			name: "a CPU flavor pins no accelerator key",
			labels: map[string]string{
				"gpustack.ai/managed":                  "true",
				"kubernetes.io/os":                     "linux",
				"general.feature.gpustack.ai/amd-epyc": "true",
			},
			want: "",
		},
		{
			name: "the .count sibling alone is not a key",
			labels: map[string]string{
				"acceleratable.feature.gpustack.ai/nvidia-h20.count": "8",
			},
			want: "",
		},
		{
			// A device group id may legally contain a period — device.NormalizeName preserves
			// "-", "_" and ".". Treating a period as a metadata suffix would discard this real
			// identity, leave the scope keyless, and hold every workload on that flavor in Retry
			// with its cards free. What excludes the metadata siblings is their VALUE, not a dot.
			name: "a dotted model id is a real key, not a metadata suffix",
			labels: map[string]string{
				"acceleratable.feature.gpustack.ai/nvidia-foo.v2":       "true",
				"acceleratable.feature.gpustack.ai/nvidia-foo.v2.count": "8",
			},
			want: "nvidia-foo.v2",
		},
		{
			// The first segment must be a known acceleratable manufacturer, which is what makes
			// the value-plus-shape predicate the same one ExtractAcceleratableNodeKeys applies.
			name:   "an unknown manufacturer is not a key",
			labels: map[string]string{"acceleratable.feature.gpustack.ai/acme-h20": "true"},
			want:   "",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rf := &kueue.ResourceFlavor{Spec: kueue.ResourceFlavorSpec{NodeLabels: c.labels}}
			assert.Equal(t, c.want, flavorAcceleratorKey(rf))
		})
	}

	// A flavor pinning several acceleratable keys violates the writer's invariant, and the cards it
	// covers are then genuinely ambiguous — so it resolves to NO key and its demands are held.
	//
	// Choosing one of them would have been arbitrary, and a test of an arbitrary choice over a map is
	// probabilistic in the same way the choice is: an implementation returning whichever key a range
	// yields first sometimes yields the intended one, and no fixture removes that, since some draw
	// always does. Answering "none" is what makes the property testable at all — every draw over
	// these keys returns the same empty string, so one pass is a verdict rather than a coin flip.
	t.Run("several keys resolve to no key at all, so the demand is held", func(t *testing.T) {
		labels := map[string]string{}
		for _, key := range []string{"nvidia-a", "nvidia-b", "nvidia-c"} {
			labels["acceleratable.feature.gpustack.ai/"+key] = "true"
		}
		rf := &kueue.ResourceFlavor{Spec: kueue.ResourceFlavorSpec{NodeLabels: labels}}
		assert.Empty(t, flavorAcceleratorKey(rf),
			"an ambiguous flavor must scope a role to nothing, not to a model picked at random")
	})
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
			// Kueue's scheduler writes PodSetAssignment.Name from the podset's own name, and the
			// CRD defaults it to "main"; the fake client does neither, so it is set here. Without
			// it the assignment matches no podset and the gate holds the Workload for a MISSING
			// ASSIGNMENT rather than for the shortage these cases are about — passing, and
			// exercising nothing.
			Admission: &kueue.Admission{PodSetAssignments: []kueue.PodSetAssignment{{
				Name:    "main",
				Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "gpu-pool"},
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
func reconcileSlicedGate(t *testing.T, wl *kueue.Workload) (kueue.CheckState, string) {
	t.Helper()

	// The bare accelerator feature key is what an accelerated flavor pins and what decides which
	// of a node's cards it covers, so a fixture without it would make this gate answer Retry for
	// an empty card population rather than for the shortage these cases are about — a test that
	// passes for the wrong reason. TestNodeDevicesAdmission_ReconcilePerRole asserts the same pool
	// reaches Ready when it has room, which is what keeps that from going unnoticed.
	poolLabels := map[string]string{
		"feature.gpustack.ai/nvidia":                  "true",
		"acceleratable.feature.gpustack.ai/nvidia-g0": "true",
	}
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
			return cs.State, cs.Message
		}
	}
	return "", ""
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
			state, msg := reconcileSlicedGate(t, tc.wl)
			assert.Equal(t, tc.want, state, tc.why)
			if tc.want == kueue.CheckStateRetry {
				// Pin WHY it is held. A state-only assertion let this fixture drift once already:
				// it answered Retry for a missing flavor assignment while its doc comment claimed
				// a units shortage, and passed.
				assert.Contains(t, msg, "enough free cards",
					"the hold must be the units shortage this fixture builds, not a missing assignment")
			}
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
			state, msg := reconcileSlicedGate(t, tc.wl)
			assert.Equal(t, tc.want, state, tc.why)
			if tc.want == kueue.CheckStateRetry {
				// Pin WHY it is held. A state-only assertion let this fixture drift once already:
				// it answered Retry for a missing flavor assignment while its doc comment claimed
				// a units shortage, and passed.
				assert.Contains(t, msg, "enough free cards",
					"the hold must be the units shortage this fixture builds, not a missing assignment")
			}
		})
	}
}

// modelGroup is one accelerator model on a node: the group id that forms the model half of the
// accelerator key, and the free units of each of its cards.
type modelGroup struct {
	model     string
	remaining []int32
}

// devicesOfModels builds one node's Devices ledger carrying a group per model, the shape a node
// with more than one accelerator model publishes: ONE Devices object holding both models' cards.
// Every card reports a logical slicing capability and no partition profile, as devicesWithRemaining
// does, so the model is the only axis these fixtures vary.
func devicesOfModels(node string, groups ...modelGroup) workercore.Devices {
	d := workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: node}}
	for _, g := range groups {
		specAccels := make([]workercore.Accelerator, len(g.remaining))
		statusAccels := make([]workercore.AcceleratorAllocation, len(g.remaining))
		for i, free := range g.remaining {
			id := fmt.Sprintf("%s-%s-gpu-%d", node, g.model, i)
			specAccels[i] = workercore.Accelerator{
				ID: id, Index: uint32(i),
				Status: workercore.AcceleratorStatus{
					LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 10},
				},
			}
			statusAccels[i] = workercore.AcceleratorAllocation{ID: id, Index: uint32(i), Remaining: free}
		}
		d.Spec.Groups = append(d.Spec.Groups, workercore.DevicesGroup{
			ID: g.model, Manufacturer: "nvidia", Accelerators: specAccels,
		})
		d.Status.Groups = append(d.Status.Groups, workercore.DevicesAllocationGroup{
			ID: g.model, Manufacturer: "nvidia", Accelerators: statusAccels,
		})
	}
	return d
}

// scopeNodes claims every node with each of the given flavors, as candidateDevices does for the
// flavors Kueue assigned: they all belong to one ClusterQueue, so each flavor's selector reaches
// every node of the pool. Which of a node's cards a flavor actually covers is then settled per
// card by the accelerator key — which is the behavior these cases exist to pin.
func scopeNodes(nodes []workercore.Devices, scopes ...flavorScope) []scopedDevices {
	pool := make([]scopedDevices, 0, len(nodes))
	for i := range nodes {
		pool = append(pool, scopedDevices{devices: nodes[i], scopes: scopes})
	}
	return pool
}

// TestNodeDevicesFeasibilityScopesEveryDemandToItsOwnFlavor pins the rule a multi-role Workload
// rests on: a role's demand is placeable only on the cards of the flavor THAT role was assigned.
//
// Kueue assigns a flavor per PodSet, so a prefill/decode deployment whose roles want different
// accelerator models carries two. Judged against the union of both, a demand for cards that are
// exhausted is satisfied by the other role's free cards — the check reports Ready, Kueue admits,
// and the starved role's Pods sit Pending forever. That is the worst shape an admission gate can
// take: a legal answer, no error anywhere, and a workload that never runs.
func TestNodeDevicesFeasibilityScopesEveryDemandToItsOwnFlavor(t *testing.T) {
	const (
		h20Flavor  = kueue.ResourceFlavorReference("gpustack--nvidia-h20-linux-amd64-8d")
		l40sFlavor = kueue.ResourceFlavorReference("gpustack--nvidia-l40s-linux-amd64-8d")
	)
	whole := int32(nodefeature.ResourceMaxUnits)
	exclusive := nodefeature.ResourceFamilyExclusive
	scopes := []flavorScope{
		{flavor: h20Flavor, acceleratorKey: "nvidia-h20"},
		{flavor: l40sFlavor, acceleratorKey: "nvidia-l40s"},
	}

	prefill := func(cards int32) familyDemand {
		return familyDemand{family: exclusive, cards: cards, flavor: h20Flavor, podSets: fromPodSets("prefill")}
	}
	decode := func(cards int32) familyDemand {
		return familyDemand{family: exclusive, cards: cards, flavor: l40sFlavor, podSets: fromPodSets("decode")}
	}

	cases := []struct {
		name    string
		nodes   []workercore.Devices
		demands []familyDemand
		want    kueue.CheckState
		// wantRole, when set, must appear in the verdict message so an operator reading it knows
		// which half of the deployment fell short.
		wantRole string
	}{
		{
			// The headline defect. Four free L40S and zero free H20 used to read as Ready.
			name: "a role's demand is not satisfied by the other role's free cards",
			nodes: []workercore.Devices{
				devicesOfModels("node-a", modelGroup{model: "h20", remaining: []int32{0, 0}}),
				devicesOfModels("node-b", modelGroup{model: "l40s", remaining: []int32{whole, whole, whole, whole}}),
			},
			demands:  []familyDemand{prefill(2), decode(2)},
			want:     kueue.CheckStateRetry,
			wantRole: "prefill",
		},
		{
			// Reachable today with no multi-role workload at all: one node publishes one Devices
			// object holding both models' cards, so a single-flavor demand used to be judged
			// against models its own flavor does not cover.
			name: "a mixed-model node's other model does not satisfy the demand",
			nodes: []workercore.Devices{devicesOfModels("node-a",
				modelGroup{model: "h20", remaining: []int32{0, 0}},
				modelGroup{model: "l40s", remaining: []int32{whole, whole, whole, whole}},
			)},
			// A single-podset Workload's demands reach the check carrying no provenance — Reconcile
			// strips it, there being nothing to disambiguate — so the case states that shape rather
			// than a named demand this Workload never produces. What the wording then reads as is
			// pinned where the shape is known: TestVerdictOmitsTheRoleClauseForASingleRole.
			demands: []familyDemand{{family: exclusive, cards: 2, flavor: h20Flavor}},
			want:    kueue.CheckStateRetry,
		},
		{
			name: "each role placed on its own model is ready",
			nodes: []workercore.Devices{
				devicesOfModels("node-a", modelGroup{model: "h20", remaining: []int32{whole, whole}}),
				devicesOfModels("node-b", modelGroup{model: "l40s", remaining: []int32{whole, whole}}),
			},
			demands: []familyDemand{prefill(2), decode(2)},
			want:    kueue.CheckStateReady,
		},
		{
			// The budget stays global: two roles on ONE flavor compete for the same cards, so
			// three cards cannot serve 2 + 2.
			name: "two roles on one flavor share the budget and are held when it is short",
			nodes: []workercore.Devices{
				devicesOfModels("node-a", modelGroup{model: "h20", remaining: []int32{whole, whole, whole}}),
			},
			demands: []familyDemand{
				{family: exclusive, cards: 2, flavor: h20Flavor, podSets: fromPodSets("prefill")},
				{family: exclusive, cards: 2, flavor: h20Flavor, podSets: fromPodSets("decode")},
			},
			want: kueue.CheckStateRetry,
		},
		{
			name: "two roles on one flavor are ready when it has room for both",
			nodes: []workercore.Devices{
				devicesOfModels("node-a", modelGroup{model: "h20", remaining: []int32{whole, whole, whole, whole}}),
			},
			demands: []familyDemand{
				{family: exclusive, cards: 2, flavor: h20Flavor, podSets: fromPodSets("prefill")},
				{family: exclusive, cards: 2, flavor: h20Flavor, podSets: fromPodSets("decode")},
			},
			want: kueue.CheckStateReady,
		},
		{
			// A demand whose podset assignment could not be read carries no flavor. It must be
			// held, never widened to the whole pool: guessing here is what admits a workload that
			// cannot be placed.
			name: "a demand with no assigned flavor is held, not widened to every card",
			nodes: []workercore.Devices{
				devicesOfModels("node-a", modelGroup{model: "h20", remaining: []int32{whole, whole, whole, whole}}),
			},
			demands: []familyDemand{{family: exclusive, cards: 1, podSets: fromPodSets("prefill")}},
			want:    kueue.CheckStateRetry,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, msg := nodeDevicesFeasibility(scopeNodes(c.nodes, scopes...), c.demands)
			assert.Equal(t, c.want, got, "verdict")
			if c.wantRole != "" {
				assert.Contains(t, msg, c.wantRole, "the verdict must name the role that fell short")
			}
		})
	}
}

// TestVerdictMessageNamesTheRoles pins that the role clause appears only when a demand names
// podsets, so a Workload whose assignment has not been read yet reads exactly as it did before
// roles existed — and that merged podsets are listed in a stable order rather than a map's.
func TestVerdictMessageNamesTheRoles(t *testing.T) {
	assert.Equal(t,
		"no node in the assigned flavor pool currently has enough free cards; will retry as capacity frees",
		verdictMessage(kueue.CheckStateRetry, nil))
	assert.Contains(t, verdictMessage(kueue.CheckStateRetry, fromPodSets("prefill")), `for role "prefill"`)
	assert.Contains(t,
		verdictMessage(kueue.CheckStateRetry, fromPodSets("decode", "prefill")),
		`for roles "decode", "prefill"`)
	assert.Equal(t,
		verdictMessage(kueue.CheckStateRetry, fromPodSets("decode", "prefill")),
		verdictMessage(kueue.CheckStateRetry, fromPodSets("prefill", "decode")),
		"the roles are ordered, so the verdict does not flip between reconciles")
	assert.Contains(t,
		partitionVerdictMessage(kueue.CheckStateRetry, "1g.10gb", fromPodSets("decode")),
		`for role "decode"`)
}

// TestNodeDevicesAdmission_ReconcilePerRole exercises the whole gate through Reconcile, which the
// unit cases above cannot: each of the three pieces — reading a podset's assigned flavor, scoping a
// node's cards to the flavors that cover them, and fitting a demand against only those — can be
// right on its own while the wiring between them is wrong. It is also what proves the pool fixtures
// are not answering Retry merely because nothing covers them.
func TestNodeDevicesAdmission_ReconcilePerRole(t *testing.T) {
	base := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	whole := int32(nodefeature.ResourceMaxUnits)

	// Two model-level flavors of one manufacturer in one pool, exactly as a cross-model pool
	// presents them: each pins its own accelerator key, both select the same pool nodes.
	poolLabel := map[string]string{"feature.gpustack.ai/nvidia": "true"}
	flavorOf := func(name, aKey string) *kueue.ResourceFlavor {
		lbs := map[string]string{"feature.gpustack.ai/nvidia": "true"}
		lbs["acceleratable.feature.gpustack.ai/"+aKey] = "true"
		return &kueue.ResourceFlavor{
			ObjectMeta: meta.ObjectMeta{Name: name},
			Spec:       kueue.ResourceFlavorSpec{NodeLabels: lbs},
		}
	}

	// A two-role Workload: prefill assigned the H20 flavor, decode the L40S flavor.
	workload := func() *kueue.Workload {
		podSet := func(name string, count int32) kueue.PodSet {
			return kueue.PodSet{
				Name: kueue.PodSetReference(name), Count: count,
				Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
					Name:      "c",
					Resources: core.ResourceRequirements{Requests: core.ResourceList{base: resource.MustParse("1")}},
				}}}},
			}
		}
		return &kueue.Workload{
			ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "w"},
			Spec:       kueue.WorkloadSpec{PodSets: []kueue.PodSet{podSet("prefill", 2), podSet("decode", 2)}},
			Status: kueue.WorkloadStatus{
				Conditions: []meta.Condition{{
					Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue,
					Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: meta.Now(),
				}},
				Admission: &kueue.Admission{PodSetAssignments: []kueue.PodSetAssignment{
					{Name: "prefill", Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "h20-flavor"}},
					{Name: "decode", Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "l40s-flavor"}},
				}},
				AdmissionChecks: []kueue.AdmissionCheckState{{
					Name: _NodeDevicesAdmissionCheckName, State: kueue.CheckStatePending,
				}},
			},
		}
	}

	run := func(t *testing.T, h20Free, l40sFree []int32) (kueue.CheckState, string) {
		t.Helper()
		node := devicesOfModels("node-a",
			modelGroup{model: "h20", remaining: h20Free},
			modelGroup{model: "l40s", remaining: l40sFree},
		)
		node.Labels = poolLabel
		node.Labels["acceleratable.feature.gpustack.ai/nvidia-h20"] = "true"
		node.Labels["acceleratable.feature.gpustack.ai/nvidia-l40s"] = "true"

		check := &kueue.AdmissionCheck{
			ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName},
			Spec:       kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName},
		}
		wl := workload()
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(flavorOf("h20-flavor", "nvidia-h20"), flavorOf("l40s-flavor", "nvidia-l40s"),
				check, &node, wl).
			WithStatusSubresource(&kueue.Workload{}).
			Build()
		r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

		_, err := r.Reconcile(context.Background(),
			ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "default", Name: "w"}})
		assert.NoError(t, err)

		got := new(kueue.Workload)
		assert.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "default", Name: "w"}, got))
		for _, cs := range got.Status.AdmissionChecks {
			if cs.Name == _NodeDevicesAdmissionCheckName {
				return cs.State, cs.Message
			}
		}
		return "", ""
	}

	t.Run("both roles fit on their own model", func(t *testing.T) {
		state, _ := run(t, []int32{whole, whole}, []int32{whole, whole})
		assert.Equal(t, kueue.CheckStateReady, state,
			"a Ready here is what proves the Retry below is a shortage and not an empty card population")
	})

	t.Run("one role's model is exhausted and the group is held", func(t *testing.T) {
		// Four free L40S would cover 2 + 2 if the roles' models were pooled; they must not be.
		state, msg := run(t, []int32{0, 0}, []int32{whole, whole, whole, whole})
		assert.Equal(t, kueue.CheckStateRetry, state)
		assert.Contains(t, msg, "prefill", "the verdict names the role whose model ran out")
	})
}

// TestNodeDevicesAdmission_UnassignedFlavorIsHeldExplicitly pins that a demand with no assigned
// flavor is held with a verdict that says SO, not with a capacity verdict.
//
// The fit would hold it either way — an unnamed flavor is covered by no card — but arriving there
// indirectly means an operator facing a pool-wide stall reads "no node has enough free cards" about
// a pool that is short of nothing. This is the mirror of a false positive reached by an indirect
// test: a false negative reached by an indirect test, and it is stated for the same reason.
func TestNodeDevicesAdmission_UnassignedFlavorIsHeldExplicitly(t *testing.T) {
	base := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	whole := int32(nodefeature.ResourceMaxUnits)

	podSet := func(name string, accelerated bool) kueue.PodSet {
		reqs := core.ResourceList{core.ResourceCPU: resource.MustParse("1")}
		if accelerated {
			reqs[base] = resource.MustParse("1")
		}
		return kueue.PodSet{
			Name: kueue.PodSetReference(name), Count: 1,
			Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
				Name: "c", Resources: core.ResourceRequirements{Requests: reqs},
			}}}},
		}
	}

	cases := []struct {
		name        string
		podSets     []kueue.PodSet
		admission   *kueue.Admission
		wantState   kueue.CheckState
		wantMessage string
		// wantNoRole asserts the hold carries no role clause. It follows the same rule every other
		// verdict does: a single-podset Workload has nothing to disambiguate, so naming Kueue's
		// default podset would be noise. The hold must still FIRE for it — that is what makes this
		// worth asserting rather than assuming.
		wantNoRole bool
	}{
		{
			// Kueue's SetQuotaReservation dereferences the admission it stores, so it cannot
			// produce this pairing itself; reaching it means the status was written from outside
			// Kueue, or Kueue's contract moved. The hold is fail-safe either way — what this case
			// pins is that it is also EXPLAINED.
			name:        "reserved with no admission at all",
			podSets:     []kueue.PodSet{podSet("prefill", true)},
			admission:   nil,
			wantState:   kueue.CheckStateRetry,
			wantMessage: "does not resolve to a single accelerator flavor",
			wantNoRole:  true,
		},
		{
			name:    "one role's assignment is missing from the admission",
			podSets: []kueue.PodSet{podSet("prefill", true), podSet("decode", true)},
			admission: &kueue.Admission{PodSetAssignments: []kueue.PodSetAssignment{
				{Name: "prefill", Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "gpu-pool"}},
			}},
			wantState:   kueue.CheckStateRetry,
			wantMessage: `"decode"`,
		},
		{
			name:    "an assignment carrying no flavor is the same hold",
			podSets: []kueue.PodSet{podSet("prefill", true)},
			admission: &kueue.Admission{PodSetAssignments: []kueue.PodSetAssignment{
				{Name: "prefill", Flavors: nil},
			}},
			wantState:   kueue.CheckStateRetry,
			wantMessage: "does not resolve to a single accelerator flavor",
			wantNoRole:  true,
		},
		{
			// A Workload demanding no accelerator has nothing to resolve a card population for, so
			// the hold must not fire on it — this is the case that would turn every CPU-only
			// workload into a permanent Retry if the predicate were written on the admission
			// instead of on the demands.
			name:        "no accelerator demand is unaffected",
			podSets:     []kueue.PodSet{podSet("server", false)},
			admission:   nil,
			wantState:   kueue.CheckStateReady,
			wantMessage: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			poolLabels := map[string]string{
				"feature.gpustack.ai/nvidia":                  "true",
				"acceleratable.feature.gpustack.ai/nvidia-g0": "true",
			}
			rf := &kueue.ResourceFlavor{
				ObjectMeta: meta.ObjectMeta{Name: "gpu-pool"},
				Spec:       kueue.ResourceFlavorSpec{NodeLabels: poolLabels},
			}
			check := &kueue.AdmissionCheck{
				ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName},
				Spec:       kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName},
			}
			// A roomy pool, so a Retry here can only be the hold under test and never a shortage.
			devs := devicesWithRemaining(whole, whole, whole, whole)
			devs.ObjectMeta = meta.ObjectMeta{Name: "node-a", Labels: poolLabels}
			wl := &kueue.Workload{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "w"},
				Spec:       kueue.WorkloadSpec{PodSets: c.podSets},
				Status: kueue.WorkloadStatus{
					Conditions: []meta.Condition{{
						Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue,
						Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: meta.Now(),
					}},
					Admission: c.admission,
					AdmissionChecks: []kueue.AdmissionCheckState{{
						Name: _NodeDevicesAdmissionCheckName, State: kueue.CheckStatePending,
					}},
				},
			}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(rf, check, &devs, wl).
				WithStatusSubresource(&kueue.Workload{}).
				Build()
			r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

			_, err := r.Reconcile(context.Background(),
				ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "default", Name: "w"}})
			assert.NoError(t, err)

			got := new(kueue.Workload)
			assert.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Namespace: "default", Name: "w"}, got))
			var state kueue.CheckState
			var message string
			for _, cs := range got.Status.AdmissionChecks {
				if cs.Name == _NodeDevicesAdmissionCheckName {
					state, message = cs.State, cs.Message
				}
			}
			assert.Equal(t, c.wantState, state, "verdict")
			if c.wantMessage != "" {
				assert.Contains(t, message, c.wantMessage, "the verdict states WHY it is held")
				assert.NotContains(t, message, "enough free cards",
					"a missing assignment must not be reported as a capacity shortage")
			}
			if c.wantNoRole {
				assert.NotContains(t, message, "for role",
					"a single-podset Workload's hold must not name Kueue's default podset")
			}
		})
	}
}

// TestNodeDevicesAdmission_RefreshesTheMessageWhenTheCauseChanges pins that a verdict's stated
// cause keeps up with reality while the state does not move.
//
// applyVerdict skips the patch on a settled Workload so the controller does not re-trigger itself,
// and it decided that on the state alone. Every Retry used to read the same, so that was harmless;
// now a Retry names its cause and its role, and two Retry verdicts of one Workload can differ in
// everything but the state. This one is held first for a missing flavor assignment and then, once
// Kueue assigns one, for a card shortage: the same state, a different explanation, and the operator
// reading it would otherwise be told to wait for an assignment that has already arrived.
func TestNodeDevicesAdmission_RefreshesTheMessageWhenTheCauseChanges(t *testing.T) {
	base := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	poolLabels := map[string]string{
		"feature.gpustack.ai/nvidia":                  "true",
		"acceleratable.feature.gpustack.ai/nvidia-g0": "true",
	}
	// One card, two demanded: the second cause is a real shortage, not an empty population.
	devs := devicesWithRemaining(int32(nodefeature.ResourceMaxUnits))
	devs.ObjectMeta = meta.ObjectMeta{Name: "node-a", Labels: poolLabels}
	wl := &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "w"},
		Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
			Name: "main", Count: 1,
			Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
				Name:      "c",
				Resources: core.ResourceRequirements{Requests: core.ResourceList{base: resource.MustParse("2")}},
			}}}},
		}}},
		Status: kueue.WorkloadStatus{
			Conditions: []meta.Condition{{
				Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue,
				Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: meta.Now(),
			}},
			AdmissionChecks: []kueue.AdmissionCheckState{{
				Name: _NodeDevicesAdmissionCheckName, State: kueue.CheckStatePending,
			}},
		},
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(
			&kueue.ResourceFlavor{
				ObjectMeta: meta.ObjectMeta{Name: "gpu-pool"},
				Spec:       kueue.ResourceFlavorSpec{NodeLabels: poolLabels},
			},
			&kueue.AdmissionCheck{
				ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName},
				Spec:       kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName},
			},
			&devs, wl,
		).
		WithStatusSubresource(&kueue.Workload{}).
		Build()
	r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}
	key := ctrlcli.ObjectKey{Namespace: "default", Name: "w"}

	verdict := func() (kueue.CheckState, string) {
		t.Helper()
		_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: key})
		require.NoError(t, err)
		got := new(kueue.Workload)
		require.NoError(t, cli.Get(context.Background(), key, got))
		for _, cs := range got.Status.AdmissionChecks {
			if cs.Name == _NodeDevicesAdmissionCheckName {
				return cs.State, cs.Message
			}
		}
		return "", ""
	}

	state, message := verdict()
	require.Equal(t, kueue.CheckStateRetry, state)
	require.Contains(t, message, "does not resolve to a single accelerator flavor", "the first cause")

	// Kueue assigns the flavor. The capacity is still short, so the state does not move.
	live := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(), key, live))
	live.Status.Admission = &kueue.Admission{PodSetAssignments: []kueue.PodSetAssignment{{
		Name:    "main",
		Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{"credits.gpustack.ai/nvidia": "gpu-pool"},
	}}}
	require.NoError(t, cli.Status().Update(context.Background(), live))

	state, message = verdict()
	assert.Equal(t, kueue.CheckStateRetry, state, "still short of a card, so the state holds")
	assert.Contains(t, message, "enough free cards", "the message follows the cause that is now true")
	assert.NotContains(t, message, "does not resolve to a single accelerator flavor",
		"an explanation that stopped being true must not survive because the state did")
}

// TestNodeDevicesAdmission_UnresolvedFlavorIsHeldExplicitly pins the verdict for a flavor whose
// nodes are in the pool while none of its cards can be identified as its own.
//
// A flavor pinning several acceleratable keys resolves to no key, so its scope covers no card and
// the fit holds the Workload either way. What this pins is the REASON: without it an operator reads
// "no node in the assigned flavor pool currently has enough free cards" about a pool whose cards are
// free, and no amount of waiting clears it, because the flavor's labels are what have to change.
//
// The second case is the boundary that keeps the first from swallowing the ordinary one. A flavor
// whose selector matches no Devices contributes no scope at all, and there a capacity verdict is
// correct — a pool with no nodes yet is exactly the transient state Retry is for. Told apart by
// whether the flavor reached any node, not by whether a key was resolved.
func TestNodeDevicesAdmission_UnresolvedFlavorIsHeldExplicitly(t *testing.T) {
	base := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	whole := int32(nodefeature.ResourceMaxUnits)

	cases := []struct {
		name string
		// flavorLabels is the assigned flavor's selector AND its accelerator-key claim; nodeLabels
		// is what the pool's one Devices object carries, so the two together decide whether the
		// flavor reaches it at all.
		flavorLabels  map[string]string
		nodeLabels    map[string]string
		wantMessage   string
		wantNoMessage string
	}{
		{
			// Two acceleratable keys on one flavor: the writer never produces this, an admin can.
			name: "a flavor pinning two accelerator keys names itself as the cause",
			flavorLabels: map[string]string{
				"feature.gpustack.ai/nvidia":                    "true",
				"acceleratable.feature.gpustack.ai/nvidia-g0":   "true",
				"acceleratable.feature.gpustack.ai/nvidia-l40s": "true",
			},
			nodeLabels: map[string]string{
				"feature.gpustack.ai/nvidia":                    "true",
				"acceleratable.feature.gpustack.ai/nvidia-g0":   "true",
				"acceleratable.feature.gpustack.ai/nvidia-l40s": "true",
			},
			wantMessage:   "pins no single accelerator key",
			wantNoMessage: "enough free cards",
		},
		{
			name: "a flavor whose selector matches no node is a shortage, not an unresolvable flavor",
			flavorLabels: map[string]string{
				"feature.gpustack.ai/nvidia":                    "true",
				"acceleratable.feature.gpustack.ai/nvidia-h100": "true",
			},
			nodeLabels: map[string]string{
				"feature.gpustack.ai/nvidia":                  "true",
				"acceleratable.feature.gpustack.ai/nvidia-g0": "true",
			},
			wantMessage:   "enough free cards",
			wantNoMessage: "pins no single accelerator key",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A roomy pool of cards the flavor cannot claim, so neither verdict can be a real
			// shortage of the node's own capacity.
			devs := devicesWithRemaining(whole, whole, whole, whole)
			devs.ObjectMeta = meta.ObjectMeta{Name: "node-a", Labels: c.nodeLabels}
			wl := &kueue.Workload{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "w"},
				Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
					Name: "main", Count: 1,
					Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
						Name:      "c",
						Resources: core.ResourceRequirements{Requests: core.ResourceList{base: resource.MustParse("1")}},
					}}}},
				}}},
				Status: kueue.WorkloadStatus{
					Conditions: []meta.Condition{{
						Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue,
						Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: meta.Now(),
					}},
					Admission: &kueue.Admission{PodSetAssignments: []kueue.PodSetAssignment{{
						Name:    "main",
						Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "gpu-pool"},
					}}},
					AdmissionChecks: []kueue.AdmissionCheckState{{
						Name: _NodeDevicesAdmissionCheckName, State: kueue.CheckStatePending,
					}},
				},
			}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(
					&kueue.ResourceFlavor{
						ObjectMeta: meta.ObjectMeta{Name: "gpu-pool"},
						Spec:       kueue.ResourceFlavorSpec{NodeLabels: c.flavorLabels},
					},
					&kueue.AdmissionCheck{
						ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName},
						Spec:       kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName},
					},
					&devs, wl,
				).
				WithStatusSubresource(&kueue.Workload{}).
				Build()
			r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}
			key := ctrlcli.ObjectKey{Namespace: "default", Name: "w"}

			_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: key})
			require.NoError(t, err)

			got := new(kueue.Workload)
			require.NoError(t, cli.Get(context.Background(), key, got))
			cur := kueueadmissioncheck.FindAdmissionCheck(got.Status.AdmissionChecks, _NodeDevicesAdmissionCheckName)
			require.NotNil(t, cur)
			assert.Equal(t, kueue.CheckStateRetry, cur.State)
			assert.Contains(t, cur.Message, c.wantMessage)
			assert.NotContains(t, cur.Message, c.wantNoMessage)
		})
	}
}

// TestNodeDevicesAdmission_RestoresAMissingRetryDelay pins the other half of what the no-op guard
// compares. The verdict this controller writes is a triple — state, message and requeue delay — and
// a field left out of the comparison is one the guard would pin to whatever reached the Workload
// first. A Retry carrying no delay is the one that costs: Kueue reads a missing delay as "retry
// now", which is the hot loop the fixed backoff exists to prevent.
//
// The fixture is a Workload already holding this controller's own verdict, byte for byte, with the
// delay dropped — the only state from which the guard can be observed to skip at all.
func TestNodeDevicesAdmission_RestoresAMissingRetryDelay(t *testing.T) {
	base := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	poolLabels := map[string]string{
		"feature.gpustack.ai/nvidia":                  "true",
		"acceleratable.feature.gpustack.ai/nvidia-g0": "true",
	}
	devs := devicesWithRemaining(int32(nodefeature.ResourceMaxUnits))
	devs.ObjectMeta = meta.ObjectMeta{Name: "node-a", Labels: poolLabels}
	wl := &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "w"},
		Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
			Name: "main", Count: 1,
			Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
				Name:      "c",
				Resources: core.ResourceRequirements{Requests: core.ResourceList{base: resource.MustParse("2")}},
			}}}},
		}}},
		Status: kueue.WorkloadStatus{
			Conditions: []meta.Condition{{
				Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue,
				Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: meta.Now(),
			}},
			Admission: &kueue.Admission{PodSetAssignments: []kueue.PodSetAssignment{{
				Name:    "main",
				Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "gpu-pool"},
			}}},
			AdmissionChecks: []kueue.AdmissionCheckState{{
				Name:  _NodeDevicesAdmissionCheckName,
				State: kueue.CheckStateRetry,
				// The verdict this reconcile will compute, so state and message both match and only
				// the delay differs. Rendered rather than restated, so a reworded verdict cannot
				// leave this case matching nothing.
				Message:             verdictMessage(kueue.CheckStateRetry, nil),
				RequeueAfterSeconds: nil,
			}},
		},
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(
			&kueue.ResourceFlavor{
				ObjectMeta: meta.ObjectMeta{Name: "gpu-pool"},
				Spec:       kueue.ResourceFlavorSpec{NodeLabels: poolLabels},
			},
			&kueue.AdmissionCheck{
				ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName},
				Spec:       kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName},
			},
			&devs, wl,
		).
		WithStatusSubresource(&kueue.Workload{}).
		Build()
	r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}
	key := ctrlcli.ObjectKey{Namespace: "default", Name: "w"}

	_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: key})
	require.NoError(t, err)

	got := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(), key, got))
	cur := kueueadmissioncheck.FindAdmissionCheck(got.Status.AdmissionChecks, _NodeDevicesAdmissionCheckName)
	require.NotNil(t, cur)
	assert.Equal(t, kueue.CheckStateRetry, cur.State, "the verdict is unchanged")
	require.NotNil(t, cur.RequeueAfterSeconds,
		"a Retry with no delay has Kueue retry immediately, so the write must not be skipped")
	assert.Equal(t, _NodeDevicesRetryAfterSeconds, *cur.RequeueAfterSeconds)
}

// TestPartitionNoCardsMessageNamesTheRole pins that the "this pool has no partitioned card"
// verdict names the role it is about. The population it reports empty is the demand's own — the
// cards of the flavor that role was assigned — so on a two-role deployment the unqualified wording
// would claim the whole pool has no partitioned card while the other role runs on partitioned
// cards of that same pool.
func TestPartitionNoCardsMessageNamesTheRole(t *testing.T) {
	assert.Contains(t, partitionNoCardsMessage("1g.10gb", fromPodSets("decode")), `for role "decode"`)
	assert.Contains(t, partitionNoCardsMessage("1g.10gb", nil), "no partitioned card",
		"a demand naming no podset reads as it did before roles existed")
	assert.NotContains(t, partitionNoCardsMessage("1g.10gb", nil), "for role")
}

// TestFlavorAcceleratorCount pins the node batch a flavor claims. The batch is part of a flavor's
// identity — its name encodes the per-node card count and Kueue admits Pods onto that batch only —
// but the Devices selector cannot carry it, so it has to be re-applied per card group.
//
// The two failure directions are opposite and both are real: a flavor that states NO batch must
// read as "any", never as "none", or every workload on it becomes unplaceable; a flavor that states
// a batch this operator cannot read must cover NONE, never "any", or free cards from another batch
// report Ready for Pods Kueue will never place there.
func TestFlavorAcceleratorCount(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		key    string
		want   int
	}{
		{
			name: "the key's own .count label is the batch",
			labels: map[string]string{
				"acceleratable.feature.gpustack.ai/nvidia-h20":       "true",
				"acceleratable.feature.gpustack.ai/nvidia-h20.count": "8",
			},
			key: "nvidia-h20", want: 8,
		},
		{
			name: "another model's .count is not this key's batch",
			labels: map[string]string{
				"acceleratable.feature.gpustack.ai/nvidia-h20":        "true",
				"acceleratable.feature.gpustack.ai/nvidia-l40s.count": "4",
			},
			key: "nvidia-h20", want: 0,
		},
		{
			name:   "an absent label pins no batch",
			labels: map[string]string{"acceleratable.feature.gpustack.ai/nvidia-h20": "true"},
			key:    "nvidia-h20", want: 0,
		},
		{
			name:   "a flavor with no accelerator key pins no batch",
			labels: map[string]string{"general.feature.gpustack.ai/amd-epyc": "true"},
			key:    "", want: 0,
		},
		{
			// The other direction, and it must NOT read as "any": the flavor does state a batch,
			// this operator just cannot tell which. Kueue admits its Pods only onto nodes carrying
			// that exact value, and no node carries an unreadable one.
			name: "a .count that is not a number states a batch that cannot be read",
			labels: map[string]string{
				"acceleratable.feature.gpustack.ai/nvidia-h20":       "true",
				"acceleratable.feature.gpustack.ai/nvidia-h20.count": "eight",
			},
			key: "nvidia-h20", want: _unreadableAcceleratorBatch,
		},
		{
			name: "a non-positive .count is not a card count either",
			labels: map[string]string{
				"acceleratable.feature.gpustack.ai/nvidia-h20":       "true",
				"acceleratable.feature.gpustack.ai/nvidia-h20.count": "0",
			},
			key: "nvidia-h20", want: _unreadableAcceleratorBatch,
		},
		{
			// Present with an empty value is a legal label and an unreadable batch, so it is told
			// apart from an absent one by the lookup, not by the parse.
			name: "a .count present but empty is not a card count either",
			labels: map[string]string{
				"acceleratable.feature.gpustack.ai/nvidia-h20":       "true",
				"acceleratable.feature.gpustack.ai/nvidia-h20.count": "",
			},
			key: "nvidia-h20", want: _unreadableAcceleratorBatch,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rf := &kueue.ResourceFlavor{Spec: kueue.ResourceFlavorSpec{NodeLabels: c.labels}}
			assert.Equal(t, c.want, flavorAcceleratorCount(rf, c.key))
		})
	}
}

// TestNodeDevicesFeasibilityScopesEveryDemandToItsNodeBatch pins the second half of what a flavor's
// identity means. A flavor is per (CPU, model, os, arch, PER-NODE CARD COUNT), and Kueue admits a
// Pod onto that count batch only — but candidateDevices must strip ".count" from the Devices
// selector, because the DeviceManager deliberately omits it from a Devices object's labels.
//
// So without re-applying the batch per card group, the 4-device and the 8-device flavor of one
// model claim each other's nodes: free cards on a 4-device node make an 8-device-flavor demand
// Ready, Kueue admits it, and its Pods stay Pending on a node batch they were never assigned.
func TestNodeDevicesFeasibilityScopesEveryDemandToItsNodeBatch(t *testing.T) {
	const (
		fourDevice  = kueue.ResourceFlavorReference("gpustack--nvidia-h20-linux-amd64-4d")
		eightDevice = kueue.ResourceFlavorReference("gpustack--nvidia-h20-linux-amd64-8d")
	)
	whole := int32(nodefeature.ResourceMaxUnits)
	free4 := []int32{whole, whole, whole, whole}

	// One node in the 4-device batch, all four cards free. Both flavors' selectors reach it —
	// the batch pin is what must keep the 8-device flavor from covering its cards.
	node := []workercore.Devices{devicesOfModels("node-4d", modelGroup{model: "h20", remaining: free4})}
	scopes := []flavorScope{
		{flavor: fourDevice, acceleratorKey: "nvidia-h20", acceleratorCount: 4},
		{flavor: eightDevice, acceleratorKey: "nvidia-h20", acceleratorCount: 8},
	}
	demand := func(f kueue.ResourceFlavorReference) []familyDemand {
		return []familyDemand{{family: nodefeature.ResourceFamilyExclusive, cards: 2, flavor: f}}
	}

	got, _ := nodeDevicesFeasibility(scopeNodes(node, scopes...), demand(fourDevice))
	assert.Equal(t, kueue.CheckStateReady, got,
		"the flavor whose batch matches this node covers its cards")

	got, _ = nodeDevicesFeasibility(scopeNodes(node, scopes...), demand(eightDevice))
	assert.Equal(t, kueue.CheckStateRetry, got,
		"a flavor pinning an 8-device batch must not be satisfied by a 4-device node's free cards")

	// A flavor that pins no batch covers any: a missing label must read as "any", not "none".
	unpinned := []flavorScope{{flavor: eightDevice, acceleratorKey: "nvidia-h20"}}
	got, _ = nodeDevicesFeasibility(scopeNodes(node, unpinned...), demand(eightDevice))
	assert.Equal(t, kueue.CheckStateReady, got,
		"a flavor stating no batch must cover every batch, never none")

	// A batch that is PRESENT but unreadable is the opposite case and must cover nothing. Kueue
	// admits this flavor's Pods only onto nodes carrying that exact label value and no node can
	// carry an unreadable one, so widening it to "any batch" reports Ready off free cards its Pods
	// will never be placed on. The scope is built through flavorAcceleratorCount rather than by
	// hand, so this pins the label reading and the coverage rule together.
	malformed := &kueue.ResourceFlavor{Spec: kueue.ResourceFlavorSpec{NodeLabels: map[string]string{
		"acceleratable.feature.gpustack.ai/nvidia-h20":       "true",
		"acceleratable.feature.gpustack.ai/nvidia-h20.count": "eight",
	}}}
	unreadable := []flavorScope{{
		flavor:           eightDevice,
		acceleratorKey:   "nvidia-h20",
		acceleratorCount: flavorAcceleratorCount(malformed, "nvidia-h20"),
	}}
	got, _ = nodeDevicesFeasibility(scopeNodes(node, unreadable...), demand(eightDevice))
	assert.Equal(t, kueue.CheckStateRetry, got,
		"a flavor stating a batch that cannot be read must cover no card, never every card")
}

// TestVerdictOmitsTheRoleClauseForASingleRole pins that the role clause is decided by the
// WORKLOAD's shape, through Reconcile — the only place that knows the shape. Two contracts, and the
// second is why the first cannot be pinned from nodeDevicesFeasibility at all:
//
//   - a Workload of one podset — every Workload rendered from a plain Pod, whose podset carries
//     Kueue's default name — reads exactly as it did before roles existed, down to the byte;
//   - a Workload of two podsets where only ONE asks for accelerators still names that role. Its
//     demands look identical to the single-podset one's — one demand, one name — so a rule read off
//     the demands cannot tell the two apart, and the role it would drop is exactly the one a
//     prefill/decode deployment needs named.
func TestVerdictOmitsTheRoleClauseForASingleRole(t *testing.T) {
	base := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	whole := int32(nodefeature.ResourceMaxUnits)
	poolLabels := map[string]string{
		"feature.gpustack.ai/nvidia":                  "true",
		"acceleratable.feature.gpustack.ai/nvidia-g0": "true",
	}

	// cards == 0 is a CPU-only role: it is a podset of the Workload and yields no demand.
	podSet := func(name, cards string) kueue.PodSet {
		reqs := core.ResourceList{core.ResourceCPU: resource.MustParse("1")}
		if cards != "" {
			reqs[base] = resource.MustParse(cards)
		}
		return kueue.PodSet{
			Name: kueue.PodSetReference(name), Count: 1,
			Template: core.PodTemplateSpec{Spec: core.PodSpec{Containers: []core.Container{{
				Name: "c", Resources: core.ResourceRequirements{Requests: reqs},
			}}}},
		}
	}
	// Two free cards under one flavor, and every podset assigned that flavor — so a Retry below is
	// a card shortage and never an unresolved population or an empty pool.
	run := func(t *testing.T, podSets ...kueue.PodSet) (kueue.CheckState, string) {
		t.Helper()
		assignments := make([]kueue.PodSetAssignment, len(podSets))
		for i := range podSets {
			assignments[i] = kueue.PodSetAssignment{
				Name:    podSets[i].Name,
				Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "gpu-pool"},
			}
		}
		devs := devicesWithRemaining(whole, whole)
		devs.ObjectMeta = meta.ObjectMeta{Name: "node-a", Labels: poolLabels}
		wl := &kueue.Workload{
			ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "w"},
			Spec:       kueue.WorkloadSpec{PodSets: podSets},
			Status: kueue.WorkloadStatus{
				Conditions: []meta.Condition{{
					Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue,
					Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: meta.Now(),
				}},
				Admission: &kueue.Admission{PodSetAssignments: assignments},
				AdmissionChecks: []kueue.AdmissionCheckState{{
					Name: _NodeDevicesAdmissionCheckName, State: kueue.CheckStatePending,
				}},
			},
		}
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(
				&kueue.ResourceFlavor{
					ObjectMeta: meta.ObjectMeta{Name: "gpu-pool"},
					Spec:       kueue.ResourceFlavorSpec{NodeLabels: poolLabels},
				},
				&kueue.AdmissionCheck{
					ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName},
					Spec:       kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName},
				},
				&devs, wl,
			).
			WithStatusSubresource(&kueue.Workload{}).
			Build()
		r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

		_, err := r.Reconcile(context.Background(),
			ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "default", Name: "w"}})
		assert.NoError(t, err)

		got := new(kueue.Workload)
		assert.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "default", Name: "w"}, got))
		for _, cs := range got.Status.AdmissionChecks {
			if cs.Name == _NodeDevicesAdmissionCheckName {
				return cs.State, cs.Message
			}
		}
		return "", ""
	}

	t.Run("one podset that fits", func(t *testing.T) {
		state, msg := run(t, podSet("main", "1"))
		assert.Equal(t, kueue.CheckStateReady, state,
			"a Ready here is what proves the Retry below is a shortage and not an empty population")
		assert.Equal(t,
			"the assigned flavor pool has enough free cards to place the request",
			msg, "byte-identical to the pre-roles Ready verdict")
	})

	t.Run("one podset that does not fit", func(t *testing.T) {
		state, msg := run(t, podSet("main", "4"))
		assert.Equal(t, kueue.CheckStateRetry, state)
		assert.Equal(t,
			"no node in the assigned flavor pool currently has enough free cards; will retry as capacity frees",
			msg, "byte-identical to the pre-roles Retry verdict")
	})

	t.Run("two podsets where only one asks for accelerators", func(t *testing.T) {
		state, msg := run(t, podSet("prefill", "4"), podSet("sidecar", ""))
		assert.Equal(t, kueue.CheckStateRetry, state)
		assert.Contains(t, msg, `for role "prefill"`,
			"the Workload has two roles, so the one that fell short is named — even though only it demanded cards")
	})
}

// TestPartitionLedgerNotReadyMessageNamesTheRole pins that the rollout-window verdict names the
// role too. Its ledger-ready count is taken over the cards that role's own flavor covers, so an
// unqualified wording would claim a rollout window over a pool another role is running on.
//
// It asserts through nodeDevicesFeasibility, not against the message function alone: a direct call
// pins the wording but not the CALL SITE, so a branch passing no provenance would still satisfy it.
// That is the same "green while exercising nothing" shape this file has been bitten by twice.
func TestPartitionLedgerNotReadyMessageNamesTheRole(t *testing.T) {
	const (
		decodeFlavor  = kueue.ResourceFlavorReference("decode-flavor")
		prefillFlavor = kueue.ResourceFlavorReference("prefill-flavor")
	)
	// One partition-capable card with NO free placement of the profile and NO cached placement
	// ledger. Both are needed: free placements would let the fit succeed inside the loop and the
	// ledger-not-ready branch — which is only consulted after it — would never be reached.
	node := []workercore.Devices{physicalDevices(physicalCard{
		id: "g0", mode: workercore.DeviceAllocationModeNone, placementsCached: false,
	})}
	// Both roles' flavors reach the same card group, so the demand below is genuinely covered and
	// the branch is reached for a Workload that names two roles.
	scopes := []flavorScope{
		{flavor: decodeFlavor, acceleratorKey: "nvidia-g0"},
		{flavor: prefillFlavor, acceleratorKey: "nvidia-g0"},
	}
	demands := []familyDemand{
		{
			family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "1g.10gb",
			flavor: decodeFlavor, podSets: fromPodSets("decode"),
		},
		{
			family: nodefeature.ResourceFamilyPartitioned, cards: 1, profile: "1g.10gb",
			flavor: prefillFlavor, podSets: fromPodSets("prefill"),
		},
	}

	state, msg := nodeDevicesFeasibility(scopeNodes(node, scopes...), demands)
	assert.Equal(t, kueue.CheckStateRetry, state)
	assert.Contains(t, msg, "device manager rolling out", "the rollout-window branch is the one reached")
	assert.Contains(t, msg, `for role "decode"`, "the branch must pass its demand's provenance through")

	// And a Workload naming one role reads as it did before roles existed.
	assert.NotContains(t, partitionLedgerNotReadyMessage(nil), "for role")
	assert.Contains(t, partitionLedgerNotReadyMessage(nil), "device manager rolling out")
}

// TestCandidateDevicesOrderIsStable pins that the candidate nodes come back in the same order every
// time. The order matters because the feasibility fit spends a per-card budget in list order, so an
// unstable one can change which eligible card a demand consumes.
//
// It was not stable: the list was built while ranging a SET of flavor references, whose iteration
// order Go randomizes, and the List behind each reference carries no ordering guarantee either.
//
// The two flavors match disjoint node sets, and the fixture INTERLEAVES them — one flavor holds
// node-a and node-c, the other holds node-b. Each flavor contributes its nodes as one contiguous
// block, so an unsorted result is either [a c b] or [b a c]: node-b can never land in the middle,
// and neither order can equal the sorted one. That is what makes a single pass a verdict rather
// than a coin flip — the earlier fixture had each order right half the time, so it only failed a
// missing sort probabilistically, and so did the mutation that was supposed to prove it.
func TestCandidateDevicesOrderIsStable(t *testing.T) {
	flavorOf := func(name, batch string) *kueue.ResourceFlavor {
		return &kueue.ResourceFlavor{
			ObjectMeta: meta.ObjectMeta{Name: name},
			Spec: kueue.ResourceFlavorSpec{NodeLabels: map[string]string{
				"acceleratable.feature.gpustack.ai/nvidia-h20": "true",
				"batch.example.com/id":                         batch,
			}},
		}
	}
	nodeOf := func(name, batch string) *workercore.Devices {
		return &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{
			"acceleratable.feature.gpustack.ai/nvidia-h20": "true",
			"batch.example.com/id":                         batch,
		}}}
	}
	wl := &kueue.Workload{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "w"},
		Status: kueue.WorkloadStatus{Admission: &kueue.Admission{
			PodSetAssignments: []kueue.PodSetAssignment{
				{Name: "prefill", Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "flavor-b"}},
				{Name: "decode", Flavors: map[core.ResourceName]kueue.ResourceFlavorReference{creditsResource: "flavor-a"}},
			},
		}},
	}

	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		flavorOf("flavor-a", "1"), flavorOf("flavor-b", "2"),
		nodeOf("node-a", "1"), nodeOf("node-c", "1"), nodeOf("node-b", "2"),
	).Build()
	r := &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}

	pool, err := r.candidateDevices(context.Background(), wl)
	assert.NoError(t, err)
	got := make([]string, 0, len(pool))
	for i := range pool {
		got = append(got, pool[i].devices.Name)
	}
	assert.Equal(t, []string{"node-a", "node-b", "node-c"}, got,
		"candidate order must not depend on map iteration order")
}
