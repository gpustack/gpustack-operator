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
