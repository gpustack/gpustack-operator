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

// devicesWithRemaining builds one Devices ledger whose single group lists a card
// per value in remaining (the units still free on that card), mirroring the status
// the DevicesReconciler seeds from Spec at ResourceMaxUnits and decrements per
// allocation.
func devicesWithRemaining(remaining ...int32) workercore.Devices {
	accels := make([]workercore.AcceleratorAllocation, len(remaining))
	for i, free := range remaining {
		accels[i] = workercore.AcceleratorAllocation{
			ID:        fmt.Sprintf("gpu-%d", i),
			Index:     uint32(i),
			Remaining: free,
		}
	}
	return workercore.Devices{
		Status: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{ID: "g0", Accelerators: accels}},
		},
	}
}

func TestNodeDevicesFeasibility(t *testing.T) {
	const (
		whole = int32(nodefeature.ResourceMaxUnits)                                     // a clean whole card
		half  = int32(nodefeature.ResourceMaxUnits / 2)                                 // a 50%-sliced card's free units
		slot  = int32(nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize) // one shared owner's units
	)
	exclusive := workercore.DeviceAllocationModeExclusive
	sliced := workercore.DeviceAllocationModeSliced
	shared := workercore.DeviceAllocationModeShared

	cases := []struct {
		name        string
		devices     []workercore.Devices
		mode        workercore.DeviceAllocationMode
		count       int32
		slicedUnits int32
		want        kueue.CheckState
	}{
		{
			name:    "exclusive over a fully-sliced node is held",
			devices: []workercore.Devices{devicesWithRemaining(half, half, half, half, half, half, half, half)},
			mode:    exclusive,
			count:   5,
			want:    kueue.CheckStateRetry,
		},
		{
			name:    "exclusive with generic headroom but no clean card is held",
			devices: []workercore.Devices{devicesWithRemaining(half, half, half, half, half, half, half, half, half, half, half, half)},
			mode:    exclusive,
			count:   5, // 6M total free spread across slices, but zero whole cards
			want:    kueue.CheckStateRetry,
		},
		{
			name:    "exclusive with enough clean cards is ready",
			devices: []workercore.Devices{devicesWithRemaining(whole, whole, whole, whole, whole, half, half, half)},
			mode:    exclusive,
			count:   5,
			want:    kueue.CheckStateReady,
		},
		{
			name:    "exclusive short by one card is held",
			devices: []workercore.Devices{devicesWithRemaining(whole, whole, whole, whole)},
			mode:    exclusive,
			count:   5,
			want:    kueue.CheckStateRetry,
		},
		{
			name:        "sliced fits on partially-used cards",
			devices:     []workercore.Devices{devicesWithRemaining(half, half, half, half)},
			mode:        sliced,
			count:       3,
			slicedUnits: half,
			want:        kueue.CheckStateReady,
		},
		{
			name:        "sliced is held when the demand exceeds every card's remainder",
			devices:     []workercore.Devices{devicesWithRemaining(half, half)},
			mode:        sliced,
			count:       1,
			slicedUnits: whole, // wants a whole card's units; no sliced card has it
			want:        kueue.CheckStateRetry,
		},
		{
			name:    "shared fits when a free owner slot remains",
			devices: []workercore.Devices{devicesWithRemaining(slot, slot, 0, 0)},
			mode:    shared,
			count:   2,
			want:    kueue.CheckStateReady,
		},
		{
			name:    "feasibility aggregates whole cards across devices",
			devices: []workercore.Devices{devicesWithRemaining(whole, half), devicesWithRemaining(whole, whole, half)},
			mode:    exclusive,
			count:   3, // three clean cards across the two ledgers
			want:    kueue.CheckStateReady,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, _ := nodeDevicesFeasibility(c.devices, cardRequest{mode: c.mode, count: c.count, slicedUnits: c.slicedUnits})
			assert.Equal(t, c.want, got)
		})
	}
}

// physicalCard describes one MIG-enabled card for a feasibility fixture: its allocation Mode,
// its scalar remaining units, its RemainingProfiles ledger (profile → free instance count), and
// whether its capability carries cached Placements (the ledger-ready signal).
type physicalCard struct {
	id                string
	mode              workercore.DeviceAllocationMode
	remaining         int32
	remainingProfiles map[string]int32
	placementsCached  bool
}

// physicalDevices builds one Devices ledger whose group lists each MIG-enabled card twice — the
// Spec-side capability (a physical-slice profile, with Placements when cached) and the Status-side
// allocation (mode + scalar remaining + RemainingProfiles) — matched by accelerator ID, the shape
// physicalSlicedFeasibility joins.
func physicalDevices(cards ...physicalCard) workercore.Devices {
	specAccels := make([]workercore.Accelerator, len(cards))
	statusAccels := make([]workercore.AcceleratorAllocation, len(cards))
	for i, c := range cards {
		prof := workercore.AcceleratorPhysicalSlicedProfile{Name: "cap", Count: 7}
		if c.placementsCached {
			prof.Placements = []workercore.AcceleratorPhysicalPlacement{{Start: 0, Length: 1}}
		}
		specAccels[i] = workercore.Accelerator{
			ID: c.id, Index: uint32(i),
			Status: workercore.AcceleratorStatus{
				PhysicalSliced: workercore.AcceleratorPhysicalSliced{
					Profiles: []workercore.AcceleratorPhysicalSlicedProfile{prof},
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
			Groups: []workercore.DevicesGroup{{ID: "g0", Manufacturer: "nvidia", Accelerators: specAccels}},
		},
		Status: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{ID: "g0", Manufacturer: "nvidia", Accelerators: statusAccels}},
		},
	}
}

func TestNodeDevicesFeasibilityPhysicalSliced(t *testing.T) {
	none := workercore.DeviceAllocationModeNone
	sliced := workercore.DeviceAllocationModeSliced
	exclusive := workercore.DeviceAllocationModeExclusive

	cases := []struct {
		name    string
		devices []workercore.Devices
		profile string
		count   int32
		want    kueue.CheckState
	}{
		{
			name: "enough cards with a free placement is ready",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: none, remainingProfiles: map[string]int32{"1g.10gb": 7}, placementsCached: true},
				physicalCard{id: "g1", mode: sliced, remainingProfiles: map[string]int32{"1g.10gb": 4}, placementsCached: true},
			)},
			profile: "1g.10gb", count: 2, want: kueue.CheckStateReady,
		},
		{
			name: "profile full everywhere retries (not reject)",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: sliced, remainingProfiles: map[string]int32{"1g.10gb": 0}, placementsCached: true},
			)},
			profile: "1g.10gb", count: 1, want: kueue.CheckStateRetry,
		},
		{
			name: "no placements cached is ledger-not-ready retry",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: none, remainingProfiles: map[string]int32{}, placementsCached: false},
			)},
			profile: "1g.10gb", count: 1, want: kueue.CheckStateRetry,
		},
		{
			name: "exclusive-held mig card is not a candidate",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: exclusive, remainingProfiles: map[string]int32{"1g.10gb": 7}, placementsCached: true},
			)},
			profile: "1g.10gb", count: 1, want: kueue.CheckStateRetry,
		},
		{
			name: "short by one card retries",
			devices: []workercore.Devices{physicalDevices(
				physicalCard{id: "g0", mode: none, remainingProfiles: map[string]int32{"1g.10gb": 1}, placementsCached: true},
			)},
			profile: "1g.10gb", count: 2, want: kueue.CheckStateRetry,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, _ := nodeDevicesFeasibility(c.devices, cardRequest{mode: sliced, count: c.count, profile: c.profile})
			assert.Equal(t, c.want, got)
		})
	}
}

// TestNodeDevicesFeasibilityPhysicalMessages pins that "ledger not ready" carries a message
// distinct from a genuine "profile full", so an operator can tell an upgrade window from real
// contention.
func TestNodeDevicesFeasibilityPhysicalMessages(t *testing.T) {
	notReady := []workercore.Devices{physicalDevices(
		physicalCard{id: "g0", mode: workercore.DeviceAllocationModeNone, placementsCached: false},
	)}
	full := []workercore.Devices{physicalDevices(
		physicalCard{id: "g0", mode: workercore.DeviceAllocationModeSliced, remainingProfiles: map[string]int32{"1g.10gb": 0}, placementsCached: true},
	)}

	// A pool with NO MIG-enabled card at all (no physical-slice profiles on any card) is a
	// different condition from a rollout window, and must carry its own message.
	noMig := []workercore.Devices{{
		Spec: workercore.DevicesSpec{Groups: []workercore.DevicesGroup{{
			ID: "g0", Manufacturer: "nvidia",
			Accelerators: []workercore.Accelerator{{ID: "g0", Index: 0}}, // no PhysicalSliced.Profiles
		}}},
		Status: workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
			ID: "g0", Manufacturer: "nvidia",
			Accelerators: []workercore.AcceleratorAllocation{{ID: "g0", Index: 0, Mode: workercore.DeviceAllocationModeNone}},
		}}},
	}}

	_, notReadyMsg := nodeDevicesFeasibility(notReady, cardRequest{count: 1, profile: "1g.10gb"})
	_, fullMsg := nodeDevicesFeasibility(full, cardRequest{count: 1, profile: "1g.10gb"})
	_, noMigMsg := nodeDevicesFeasibility(noMig, cardRequest{count: 1, profile: "1g.10gb"})

	assert.Equal(t, physicalLedgerNotReadyMessage, notReadyMsg)
	assert.NotEqual(t, notReadyMsg, fullMsg, "ledger-not-ready and profile-full messages must differ")
	assert.NotEqual(t, physicalLedgerNotReadyMessage, noMigMsg,
		"a pool with no MIG-enabled card must not reuse the device-manager-rollout message")
	assert.Contains(t, noMigMsg, "no MIG-enabled card")
}

// TestNodeDevicesFeasibilityExcludesMigFromLogicalSliced pins that a soft-slice request never
// lands on a MIG-enabled card, even when that card's scalar remaining would otherwise fit.
func TestNodeDevicesFeasibilityExcludesMigFromLogicalSliced(t *testing.T) {
	half := int32(nodefeature.ResourceMaxUnits / 2)
	whole := int32(nodefeature.ResourceMaxUnits)
	sliced := workercore.DeviceAllocationModeSliced

	migCard := []workercore.Devices{physicalDevices(
		physicalCard{id: "g0", mode: workercore.DeviceAllocationModeNone, remaining: whole, placementsCached: true},
	)}
	gotMig, _ := nodeDevicesFeasibility(migCard, cardRequest{mode: sliced, count: 1, slicedUnits: half})
	assert.Equal(t, kueue.CheckStateRetry, gotMig, "a soft slice must not count a MIG-enabled card")

	softCard := []workercore.Devices{devicesWithRemaining(whole)}
	gotSoft, _ := nodeDevicesFeasibility(softCard, cardRequest{mode: sliced, count: 1, slicedUnits: half})
	assert.Equal(t, kueue.CheckStateReady, gotSoft, "the same remaining on a non-MIG card fits")
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

func TestParseCardRequest(t *testing.T) {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	slicedCard := base + nodefeature.SlicedResourceNameSuffix
	slicedUnits := base + nodefeature.SlicedUnitsResourceNameSuffix
	slicedCores := base + nodefeature.SlicedCoresPercentageResourceNameSuffix
	sharedCard := base + nodefeature.SharedResourceNameSuffix
	slicedMig := string(nodefeature.GetAcceleratableSlicedMigResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb"))

	cases := []struct {
		name     string
		podCount int32
		reqs     map[core.ResourceName]string
		want     cardRequest
	}{
		{
			name:     "exclusive single pod, many cards",
			podCount: 1,
			reqs:     map[core.ResourceName]string{core.ResourceName(base): "5"},
			want:     cardRequest{mode: workercore.DeviceAllocationModeExclusive, count: 5},
		},
		{
			name:     "exclusive many pods, one card each",
			podCount: 5,
			reqs:     map[core.ResourceName]string{core.ResourceName(base): "1"},
			want:     cardRequest{mode: workercore.DeviceAllocationModeExclusive, count: 5},
		},
		{
			name:     "sliced reads card count and per-card units, ignores cores-percentage",
			podCount: 1,
			reqs: map[core.ResourceName]string{
				core.ResourceName(slicedCard):  "2",
				core.ResourceName(slicedUnits): "320000",
				core.ResourceName(slicedCores): "100",
			},
			want: cardRequest{mode: workercore.DeviceAllocationModeSliced, count: 2, slicedUnits: 320000},
		},
		{
			name:     "shared",
			podCount: 1,
			reqs:     map[core.ResourceName]string{core.ResourceName(sharedCard): "3"},
			want:     cardRequest{mode: workercore.DeviceAllocationModeShared, count: 3},
		},
		{
			name:     "physical-sliced (mig) reads profile and card count, mode sliced",
			podCount: 1,
			reqs: map[core.ResourceName]string{
				core.ResourceName(slicedCard):  "2",
				core.ResourceName(slicedMig):   "1",
				core.ResourceName(slicedUnits): "200000",
			},
			want: cardRequest{mode: workercore.DeviceAllocationModeSliced, count: 2, slicedUnits: 200000, profile: "1g.10gb"},
		},
		{
			name:     "no accelerator request",
			podCount: 2,
			reqs:     map[core.ResourceName]string{core.ResourceCPU: "4"},
			want:     cardRequest{mode: workercore.DeviceAllocationModeNone},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, parseCardRequest(workloadRequesting(c.podCount, c.reqs)))
		})
	}
}

// TestParseCardRequest_TakesStrictestSlicedUnits pins that when containers carry
// different per-card sliced units, feasibility is checked against the largest
// (strictest) demand rather than whichever the map happened to iterate last.
func TestParseCardRequest_TakesStrictestSlicedUnits(t *testing.T) {
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

	got := parseCardRequest(wl)
	assert.Equal(t, workercore.DeviceAllocationModeSliced, got.mode)
	assert.Equal(t, int32(2), got.count, "cards summed across containers")
	assert.Equal(t, int32(480000), got.slicedUnits, "strictest (max) per-card units, not last-wins")
}

// TestParseCardRequest_InitContainerMig pins that a MIG request carried only on an init container
// is still parsed (profile + card count), so feasibility gates it — getAllocatingPod attributes
// init containers too, and the Pod webhook folds them.
func TestParseCardRequest_InitContainerMig(t *testing.T) {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	slicedCard := core.ResourceName(base + nodefeature.SlicedResourceNameSuffix)
	slicedMig := nodefeature.GetAcceleratableSlicedMigResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb")

	wl := &kueue.Workload{Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
		Name:  "main",
		Count: 1,
		Template: core.PodTemplateSpec{Spec: core.PodSpec{
			InitContainers: []core.Container{{Name: "init", Resources: core.ResourceRequirements{Requests: core.ResourceList{
				slicedCard: resource.MustParse("1"),
				slicedMig:  resource.MustParse("1"),
			}}}},
			Containers: []core.Container{{Name: "main"}},
		}},
	}}}}

	got := parseCardRequest(wl)
	assert.Equal(t, workercore.DeviceAllocationModeSliced, got.mode)
	assert.Equal(t, int32(1), got.count, "card count read from the init container")
	assert.Equal(t, "1g.10gb", got.profile)
}

// TestParseCardRequest_LimitsOnly pins that accelerator keys specified only under
// resources.limits (the conventional place for extended resources) are still parsed — a
// requests-only scan would miss them and wrongly mark feasibility Ready.
func TestParseCardRequest_LimitsOnly(t *testing.T) {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	slicedCard := core.ResourceName(base + nodefeature.SlicedResourceNameSuffix)
	slicedUnits := core.ResourceName(base + nodefeature.SlicedUnitsResourceNameSuffix)
	slicedMig := nodefeature.GetAcceleratableSlicedMigResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb")

	wl := &kueue.Workload{Spec: kueue.WorkloadSpec{PodSets: []kueue.PodSet{{
		Name:  "main",
		Count: 1,
		Template: core.PodTemplateSpec{Spec: core.PodSpec{
			Containers: []core.Container{{Name: "main", Resources: core.ResourceRequirements{Limits: core.ResourceList{
				slicedCard:  resource.MustParse("2"),
				slicedMig:   resource.MustParse("1"),
				slicedUnits: resource.MustParse("200000"),
			}}}},
		}},
	}}}}

	got := parseCardRequest(wl)
	assert.Equal(t, workercore.DeviceAllocationModeSliced, got.mode)
	assert.Equal(t, int32(2), got.count, "card count read from limits-only request")
	assert.Equal(t, int32(200000), got.slicedUnits)
	assert.Equal(t, "1g.10gb", got.profile)
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

// TestNodeDevicesAdmission_AdmittedWorkloadNotSelfEvicted guards the > 50% single-card
// self-eviction: once a Workload is admitted, its own slice is already subtracted from the
// per-card ledger, so re-checking must not count that allocation against itself and flip the
// check to Retry (which would evict the running Workload in a recreate loop). A not-yet-admitted
// Workload with the same ledger must still be held (Retry) — the gate only fires before admission.
func TestNodeDevicesAdmission_AdmittedWorkloadNotSelfEvicted(t *testing.T) {
	base := string(nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive))
	slicedCard := core.ResourceName(base + nodefeature.SlicedResourceNameSuffix)
	slicedUnits := core.ResourceName(base + nodefeature.SlicedUnitsResourceNameSuffix)
	poolLabels := map[string]string{"feature.gpustack.ai/nvidia": "true"}

	newWorkload := func(admitted bool) *kueue.Workload {
		conds := []meta.Condition{{
			Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue,
			Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: meta.Now(),
		}}
		if admitted {
			conds = append(conds, meta.Condition{
				Type: kueue.WorkloadAdmitted, Status: meta.ConditionTrue,
				Reason: "Admitted", Message: "admitted", LastTransitionTime: meta.Now(),
			})
		}
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
					Name: _NodeDevicesAdmissionCheckName, State: kueue.CheckStateReady,
				}},
			},
		}
	}

	// The card has only 640k free: the 960k this slice needs no longer fits because the
	// workload's own allocation is already subtracted — a naive re-check flips to Retry.
	setup := func(wl *kueue.Workload) (*NodeDevicesAdmissionReconciler, ctrlcli.Client) {
		rf := &kueue.ResourceFlavor{ObjectMeta: meta.ObjectMeta{Name: "gpu-pool"}, Spec: kueue.ResourceFlavorSpec{NodeLabels: poolLabels}}
		check := &kueue.AdmissionCheck{ObjectMeta: meta.ObjectMeta{Name: _NodeDevicesAdmissionCheckName}, Spec: kueue.AdmissionCheckSpec{ControllerName: _NodeDevicesControllerName}}
		devs := devicesWithRemaining(640000)
		devs.ObjectMeta = meta.ObjectMeta{Name: "node-a", Labels: poolLabels}
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(rf, check, &devs, wl).
			WithStatusSubresource(&kueue.Workload{}).
			Build()
		return &NodeDevicesAdmissionReconciler{Client: cli, APIReader: cli}, cli
	}

	stateOf := func(cli ctrlcli.Client) kueue.CheckState {
		got := new(kueue.Workload)
		assert.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "default", Name: "w"}, got))
		for _, cs := range got.Status.AdmissionChecks {
			if cs.Name == _NodeDevicesAdmissionCheckName {
				return cs.State
			}
		}
		return ""
	}

	t.Run("admitted workload is not self-evicted", func(t *testing.T) {
		wl := newWorkload(true)
		r, cli := setup(wl)
		_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "default", Name: "w"}})
		assert.NoError(t, err)
		assert.Equal(t, kueue.CheckStateReady, stateOf(cli), "admitted workload's check must stay Ready, not flip to Retry")
	})

	t.Run("not-yet-admitted workload is still gated", func(t *testing.T) {
		wl := newWorkload(false)
		r, cli := setup(wl)
		_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: "default", Name: "w"}})
		assert.NoError(t, err)
		assert.Equal(t, kueue.CheckStateRetry, stateOf(cli), "before admission the gate must hold Retry when the slice cannot fit")
	})
}
