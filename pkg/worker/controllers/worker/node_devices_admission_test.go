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
			got := nodeDevicesFeasibility(c.devices, c.mode, c.count, c.slicedUnits)
			assert.Equal(t, c.want, got)
		})
	}
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
