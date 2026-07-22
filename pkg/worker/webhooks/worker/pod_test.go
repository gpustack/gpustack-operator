package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const testLocalQueueName = "test-lq"

func newPodWebhook(objs ...ctrlcli.Object) *PodWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
	return &PodWebhook{Client: cli, APIReader: cli}
}

// slicedPod builds a queue-routed Pod whose single container requests the given
// (parsed) resources in both Requests and Limits.
func slicedPod(requests map[core.ResourceName]string) *core.Pod {
	rl := core.ResourceList{}
	for n, v := range requests {
		rl[n] = resource.MustParse(v)
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default",
			Name:      "p",
			Labels:    map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: rl, Limits: rl.DeepCopy()}},
			},
		},
	}
}

// instanceTypeWithEntrance builds the operator-owned InstanceType that fronts
// testLocalQueueName, carrying the observed per-card VRAM (Status.Detail.Memory) the webhook
// reverse-looks-up by the queue-entrance label the Default webhook stamps. An empty memory models
// the not-yet-ready state (Detail not computed).
func instanceTypeWithEntrance(memory string) *workercore.InstanceType {
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:   "it-" + testLocalQueueName,
			Labels: map[string]string{QueueEntranceLabelKey: testLocalQueueName},
		},
	}
	it.Status.Detail.Memory = memory
	return it
}

// instanceTypeWithPhysicalProfiles builds the fronting InstanceType a MIG request reads: a computed
// Detail (a non-empty Manufacturer marks it ready) carrying the per-card VRAM and the pool's
// aggregated physical-slice profile inventory (name → per-instance MemoryMib + pool ceiling Count).
func instanceTypeWithPhysicalProfiles(
	memory string, profiles ...workercore.AcceleratorSlicedPhysicalDetailProfile,
) *workercore.InstanceType {
	it := instanceTypeWithEntrance(memory)
	it.Status.Detail.Manufacturer = nodefeature.ManufacturerNVIDIA
	it.Status.Detail.SlicedDetail.Physical.Profiles = profiles
	return it
}

// physicalProfiles is the canonical H100-80GB inventory the MIG tests fold against.
func physicalProfiles() []workercore.AcceleratorSlicedPhysicalDetailProfile {
	return []workercore.AcceleratorSlicedPhysicalDetailProfile{
		{Name: "1g.10gb", Count: 7, MemoryMib: 10240},
		{Name: "2g.20gb", Count: 3, MemoryMib: 20480},
	}
}

func TestPodWebhook_Default(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := slicedResourceNamesForBase(nvidiaBase)

	cases := []struct {
		name      string
		requests  map[core.ResourceName]string
		itMemory  string // fronting InstanceType Status.Detail.Memory; "" → no InstanceType object unless itPresent
		itPresent bool   // add a fronting InstanceType even when itMemory is empty (models Detail not ready)
		wantUnits int64  // expected .sliced.units after Default; 0 → unset
		wantCores int64  // expected .sliced.cores-percentage after Default; 0 → unchecked
		wantErr   bool
	}{
		{
			name:      "memory-percentage folds to units, cores defaults to 100",
			requests:  map[core.ResourceName]string{names.card: "1", names.memPct: "20"},
			wantUnits: 320000, // 20 × M/100
			wantCores: 100,
		},
		{
			name:      "memory-mib folds to units using the InstanceType VRAM",
			requests:  map[core.ResourceName]string{names.card: "1", names.memMib: "4096"},
			itMemory:  "40960Mi", // 40Gi card; 4Gi req = 10% = 160000 units
			wantUnits: 160000,
			wantCores: 100,
		},
		{
			name:      "explicit cores-percentage is preserved",
			requests:  map[core.ResourceName]string{names.card: "1", names.memPct: "20", names.coresPct: "50"},
			wantUnits: 320000,
			wantCores: 50,
		},
		{
			name:      "client-supplied units is recomputed from memory (not trusted)",
			requests:  map[core.ResourceName]string{names.card: "1", names.memPct: "20", names.units: "999"},
			wantUnits: 320000, // 20 × M/100 overwrites the forged 999
			wantCores: 100,
		},
		{
			name:      "no memory leaves units unset",
			requests:  map[core.ResourceName]string{names.card: "1"},
			wantUnits: 0,
			wantCores: 100,
		},
		{
			name:     "memory-mib without a resolvable VRAM is rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memMib: "4096"},
			wantErr:  true,
		},
		{
			name:      "memory-mib rejected as retryable when Detail not ready",
			requests:  map[core.ResourceName]string{names.card: "1", names.memMib: "4096"},
			itPresent: true, // InstanceType exists but Status.Detail.Memory is empty
			wantErr:   true,
		},
		{
			name:     "memory-percentage over 100 is rejected (no overflow)",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "200"},
			wantErr:  true,
		},
		{
			name:     "memory-mib over the per-card VRAM is rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memMib: "81920"},
			itMemory: "40960Mi", // 40Gi card; a 80Gi request cannot fit one card
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.itMemory != "" || c.itPresent {
				objs = append(objs, instanceTypeWithEntrance(c.itMemory))
			}
			w := newPodWebhook(objs...)
			pod := slicedPod(c.requests)

			err := w.Default(context.Background(), pod)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)

			ctr := &pod.Spec.Containers[0]
			if c.wantUnits == 0 {
				_, ok := ctr.Resources.Requests[names.units]
				assert.False(t, ok, "units should be unset")
			} else {
				reqUnits := ctr.Resources.Requests[names.units]
				limUnits := ctr.Resources.Limits[names.units]
				assert.Equal(t, c.wantUnits, reqUnits.Value(), "units request")
				assert.Equal(t, c.wantUnits, limUnits.Value(), "units limit")
			}
			if c.wantCores != 0 {
				reqCores := ctr.Resources.Requests[names.coresPct]
				assert.Equal(t, c.wantCores, reqCores.Value(), "cores-percentage")
			}
		})
	}
}

func TestPodWebhook_ValidateCreate(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := slicedResourceNamesForBase(nvidiaBase)
	nvidiaShared := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeShared)

	cases := []struct {
		name     string
		requests map[core.ResourceName]string
		wantErr  bool
	}{
		{
			name:     "memory-percentage with compute headroom allowed",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "20", names.coresPct: "100"},
		},
		{
			name:     "memory-percentage without cores allowed",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "20"},
		},
		{
			name:     "memory-mib allowed",
			requests: map[core.ResourceName]string{names.card: "1", names.memMib: "4096", names.coresPct: "100"},
		},
		{
			name:     "only .sliced rejected",
			requests: map[core.ResourceName]string{names.card: "1"},
			wantErr:  true,
		},
		{
			name:     ".sliced plus units but no memory rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.units: "320000", names.coresPct: "100"},
			wantErr:  true,
		},
		{
			name:     "units-only request without the .sliced card rejected",
			requests: map[core.ResourceName]string{names.units: "320000"},
			wantErr:  true,
		},
		{
			name:     "memory-percentage over 100 rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "200", names.coresPct: "100"},
			wantErr:  true,
		},
		{
			name:     "both memory keys rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "20", names.memMib: "4096", names.coresPct: "100"},
			wantErr:  true,
		},
		{
			name:     "cores smaller than memory allowed",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "50", names.coresPct: "20"},
		},
		{
			name:     "non-positive memory-percentage rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "0", names.coresPct: "100"},
			wantErr:  true,
		},
		{
			name:     "sliced sub-resource without the .sliced card rejected",
			requests: map[core.ResourceName]string{names.memPct: "20"},
			wantErr:  true,
		},
		{
			name:     "exclusive only allowed",
			requests: map[core.ResourceName]string{nvidiaBase: "1"},
		},
		{
			name:     "exclusive and sliced together rejected",
			requests: map[core.ResourceName]string{nvidiaBase: "1", names.card: "1", names.memPct: "20"},
			wantErr:  true,
		},
		{
			name:     "shared and sliced together rejected",
			requests: map[core.ResourceName]string{nvidiaShared: "1", names.card: "1", names.memPct: "20"},
			wantErr:  true,
		},
		{
			name:     "exclusive and shared together rejected",
			requests: map[core.ResourceName]string{nvidiaBase: "1", nvidiaShared: "1"},
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newPodWebhook()
			pod := slicedPod(c.requests)

			_, err := w.ValidateCreate(context.Background(), pod)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPodWebhook_DefaultPhysicalSliced(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := slicedResourceNamesForBase(nvidiaBase)
	physicalKey := nodefeature.GetAcceleratableSlicedMigResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb")
	unknownKey := nodefeature.GetAcceleratableSlicedMigResourceName(nodefeature.ManufacturerNVIDIA, "9g.99gb")

	cases := []struct {
		name      string
		requests  map[core.ResourceName]string
		it        *workercore.InstanceType
		wantUnits int64
		wantErr   bool
	}{
		{
			// 10Gi / 80Gi × D = D/8 = 200000, identical to a soft .sliced.memory-mib: 10Gi request.
			name:      "profile folds units from its VRAM",
			requests:  map[core.ResourceName]string{names.card: "2", physicalKey: "1"},
			it:        instanceTypeWithPhysicalProfiles("81920Mi", physicalProfiles()...),
			wantUnits: 200000,
		},
		{
			name:     "unknown profile rejected",
			requests: map[core.ResourceName]string{names.card: "1", unknownKey: "1"},
			it:       instanceTypeWithPhysicalProfiles("81920Mi", physicalProfiles()...),
			wantErr:  true,
		},
		{
			name:     "detail not ready rejected (retryable)",
			requests: map[core.ResourceName]string{names.card: "1", physicalKey: "1"},
			it:       instanceTypeWithEntrance("81920Mi"), // Manufacturer empty → not ready
			wantErr:  true,
		},
		{
			name:     "card count over the pool ceiling rejected",
			requests: map[core.ResourceName]string{names.card: "8", physicalKey: "1"}, // ceiling 7
			it:       instanceTypeWithPhysicalProfiles("81920Mi", physicalProfiles()...),
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newPodWebhook(c.it)
			pod := slicedPod(c.requests)

			err := w.Default(context.Background(), pod)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)

			ctr := &pod.Spec.Containers[0]
			reqUnits := ctr.Resources.Requests[names.units]
			assert.Equal(t, c.wantUnits, reqUnits.Value(), "units request")
			// A MIG request takes none of the logical budget keys.
			_, hasCores := ctr.Resources.Requests[names.coresPct]
			assert.False(t, hasCores, "mig request must not default cores-percentage")
		})
	}
}

// TestPodWebhook_PhysicalSlicedUnitsParity guards the non-conflict property: a mig-<profile> request and
// a soft .sliced.memory-mib request of the profile's VRAM fold to the identical .sliced.units.
func TestPodWebhook_PhysicalSlicedUnitsParity(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := slicedResourceNamesForBase(nvidiaBase)
	physicalKey := nodefeature.GetAcceleratableSlicedMigResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb")

	physicalPod := slicedPod(map[core.ResourceName]string{names.card: "1", physicalKey: "1"})
	softPod := slicedPod(map[core.ResourceName]string{names.card: "1", names.memMib: "10240"}) // 10Gi
	w := newPodWebhook(instanceTypeWithPhysicalProfiles("81920Mi", physicalProfiles()...))

	assert.NoError(t, w.Default(context.Background(), physicalPod))
	assert.NoError(t, w.Default(context.Background(), softPod))

	physicalUnits := physicalPod.Spec.Containers[0].Resources.Requests[names.units]
	softUnits := softPod.Spec.Containers[0].Resources.Requests[names.units]
	assert.Equal(t, softUnits.Value(), physicalUnits.Value(), "mig and same-VRAM soft slice fold identically")
}

func TestPodWebhook_ValidateCreatePhysicalSliced(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := slicedResourceNamesForBase(nvidiaBase)
	physicalKey := nodefeature.GetAcceleratableSlicedMigResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb")
	physicalKey2 := nodefeature.GetAcceleratableSlicedMigResourceName(nodefeature.ManufacturerNVIDIA, "2g.20gb")

	cases := []struct {
		name    string
		pod     *core.Pod
		wantErr bool
	}{
		{
			name: "valid mig on 2 cards",
			pod:  slicedPod(map[core.ResourceName]string{names.card: "2", physicalKey: "1"}),
		},
		{
			name:    "mig value 2 rejected",
			pod:     slicedPod(map[core.ResourceName]string{names.card: "1", physicalKey: "2"}),
			wantErr: true,
		},
		{
			name:    "mig without card rejected",
			pod:     slicedPod(map[core.ResourceName]string{physicalKey: "1"}),
			wantErr: true,
		},
		{
			name:    "mig plus memory-mib rejected",
			pod:     slicedPod(map[core.ResourceName]string{names.card: "1", physicalKey: "1", names.memMib: "4096"}),
			wantErr: true,
		},
		{
			name:    "mig plus memory-percentage rejected",
			pod:     slicedPod(map[core.ResourceName]string{names.card: "1", physicalKey: "1", names.memPct: "20"}),
			wantErr: true,
		},
		{
			name:    "two containers naming different profiles rejected",
			pod:     physicalTwoProfilePod(names.card, physicalKey, physicalKey2),
			wantErr: true,
		},
		{
			name: "init container mig validated (value 1 passes)",
			pod:  physicalInitPod(map[core.ResourceName]string{names.card: "1", physicalKey: "1"}),
		},
		{
			name:    "init container mig validated (value 2 rejected, not skipped)",
			pod:     physicalInitPod(map[core.ResourceName]string{names.card: "1", physicalKey: "2"}),
			wantErr: true,
		},
		{
			name:    "init-container mig plus app-container exclusive rejected (cross-mode)",
			pod:     physicalInitMigPlusAppExclusivePod(names.card, physicalKey, nvidiaBase),
			wantErr: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newPodWebhook()
			_, err := w.ValidateCreate(context.Background(), c.pod)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestPodWebhook_MigProfileMissingMemoryIsRetryable pins that a MIG profile present in the fronting
// InstanceType's inventory but with MemoryMib not yet populated (partial detail during detection /
// rollout skew) yields a retryable not-ready error from Default, not a permanent zero-units rejection.
func TestPodWebhook_MigProfileMissingMemoryIsRetryable(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := slicedResourceNamesForBase(nvidiaBase)
	physicalKey := nodefeature.GetAcceleratableSlicedMigResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb")

	// The profile is offered but its per-instance MemoryMib is still 0 (detail not fully computed).
	it := instanceTypeWithPhysicalProfiles("81920Mi",
		workercore.AcceleratorSlicedPhysicalDetailProfile{Name: "1g.10gb", Count: 7, MemoryMib: 0})
	w := newPodWebhook(it)

	pod := slicedPod(map[core.ResourceName]string{names.card: "1", physicalKey: "1"})
	err := w.Default(context.Background(), pod)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not ready",
		"a MIG profile with MemoryMib==0 must be a retryable not-ready rejection, not a permanent zero-units error")
}

// physicalTwoProfilePod builds a Pod whose two app containers each name a distinct MIG profile — the
// unattributable multi-profile shape ValidateCreate rejects.
func physicalTwoProfilePod(card, profileA, profileB core.ResourceName) *core.Pod {
	rlA := core.ResourceList{card: resource.MustParse("1"), profileA: resource.MustParse("1")}
	rlB := core.ResourceList{card: resource.MustParse("1"), profileB: resource.MustParse("1")}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default", Name: "p",
			Labels: map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: rlA, Limits: rlA.DeepCopy()}},
				{Name: "aux", Resources: core.ResourceRequirements{Requests: rlB, Limits: rlB.DeepCopy()}},
			},
		},
	}
}

// physicalInitPod builds a queue-routed Pod whose sliced request lives on an init container, so the
// webhook must validate init containers (getAllocatingPod attributes across both).
func physicalInitPod(requests map[core.ResourceName]string) *core.Pod {
	rl := core.ResourceList{}
	for n, v := range requests {
		rl[n] = resource.MustParse(v)
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default", Name: "p",
			Labels: map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			InitContainers: []core.Container{
				{Name: "init", Resources: core.ResourceRequirements{Requests: rl, Limits: rl.DeepCopy()}},
			},
			Containers: []core.Container{{Name: "main"}},
		},
	}
}

// physicalInitMigPlusAppExclusivePod builds a Pod with a MIG (sliced) request on an init container
// and a whole-card exclusive request on an app container — two allocation modes across the Pod.
// ValidateCreate must reject it, which requires the one-mode-per-Pod check to scan init containers
// too (not only app containers).
func physicalInitMigPlusAppExclusivePod(card, migKey, exclusiveKey core.ResourceName) *core.Pod {
	initRL := core.ResourceList{card: resource.MustParse("1"), migKey: resource.MustParse("1")}
	appRL := core.ResourceList{exclusiveKey: resource.MustParse("1")}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default", Name: "p",
			Labels: map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			InitContainers: []core.Container{
				{Name: "init", Resources: core.ResourceRequirements{Requests: initRL, Limits: initRL.DeepCopy()}},
			},
			Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: appRL, Limits: appRL.DeepCopy()}},
			},
		},
	}
}

// slicedPodWithSidecar builds the two-container shape the InstanceReconciler emits
// for an SSH-enabled sliced Instance once the accelerator is colocated on `main`:
// the sliced request lives on `main`, and the `sshd` sidecar carries no accelerator
// resource. The webhook iterates all containers, so it must still default and admit
// the sliced request wherever it lands.
func slicedPodWithSidecar(requests map[core.ResourceName]string) *core.Pod {
	rl := core.ResourceList{}
	for n, v := range requests {
		rl[n] = resource.MustParse(v)
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default",
			Name:      "p",
			Labels:    map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: rl, Limits: rl.DeepCopy()}},
				{Name: "sshd"},
			},
		},
	}
}

// TestPodWebhook_SlicedRequestOnMainWithSidecar guards that after the accelerator is
// colocated on `main`, the Pod webhook still folds .sliced.units on `main` and admits
// the main(sliced) + sshd(no accelerator) Pod — the sidecar contributes no accelerator
// mode, so podAcceleratorModes stays at one mode.
func TestPodWebhook_SlicedRequestOnMainWithSidecar(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := slicedResourceNamesForBase(nvidiaBase)

	pod := slicedPodWithSidecar(map[core.ResourceName]string{names.card: "1", names.memPct: "60"})
	w := newPodWebhook()

	// Default folds units onto main (the request holder) and leaves sshd untouched.
	assert.NoError(t, w.Default(context.Background(), pod))
	main, sshd := &pod.Spec.Containers[0], &pod.Spec.Containers[1]
	units := main.Resources.Requests[names.units]
	assert.Equal(t, int64(960000), units.Value(), "units folded on main (60 × M/100)")
	assert.Empty(t, sshd.Resources.Requests, "sshd sidecar carries no resources")

	// ValidateCreate admits: only one accelerator mode (sliced) across the Pod.
	_, err := w.ValidateCreate(context.Background(), pod)
	assert.NoError(t, err)
}

// TestPodWebhook_VisibilityResourceIsNotAMode guards that the SSH sidecar's device-only
// visibility resource is outside the known-acceleratable families, so a Pod that requests a
// real sliced accelerator on `main` and the visibility resource on `sshd` still reads as a
// single accelerator mode and admits (a known-family name here would make
// podAcceleratorModes see two modes and reject the Pod).
func TestPodWebhook_VisibilityResourceIsNotAMode(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := slicedResourceNamesForBase(nvidiaBase)
	visibility := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)

	pod := slicedPodWithSidecar(map[core.ResourceName]string{names.card: "1", names.memPct: "60"})
	// sshd requests the visibility resource with the same quantity as main's card count.
	cards := resource.MustParse("1")
	pod.Spec.Containers[1].Resources = core.ResourceRequirements{
		Requests: core.ResourceList{visibility: cards},
		Limits:   core.ResourceList{visibility: cards.DeepCopy()},
	}
	w := newPodWebhook()

	_, err := w.ValidateCreate(context.Background(), pod)
	assert.NoError(t, err, "main(sliced) + sshd(visibility) is a single accelerator mode")
}
