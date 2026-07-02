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
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
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

func localQueueWithMemory(memory string) *kueue.LocalQueue {
	lq := &kueue.LocalQueue{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: testLocalQueueName},
	}
	systemmeta.NoteResource(lq, "instancetypeselectors", map[string]string{"memory": memory})
	return lq
}

func TestPodWebhook_Default(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := slicedResourceNamesForBase(nvidiaBase)

	cases := []struct {
		name      string
		requests  map[core.ResourceName]string
		lqMemory  string // LocalQueue "memory" note; "" → no LocalQueue object
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
			name:      "memory-mib folds to units using the LocalQueue VRAM note",
			requests:  map[core.ResourceName]string{names.card: "1", names.memMib: "4096"},
			lqMemory:  "40960Mi", // 40Gi card; 4Gi req = 10% = 160000 units
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
			name:     "memory-mib without a resolvable VRAM note is rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memMib: "4096"},
			wantErr:  true,
		},
		{
			name:     "memory-percentage over 100 is rejected (no overflow)",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "200"},
			wantErr:  true,
		},
		{
			name:     "memory-mib over the per-card VRAM is rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memMib: "81920"},
			lqMemory: "40960Mi", // 40Gi card; a 80Gi request cannot fit one card
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.lqMemory != "" {
				objs = append(objs, localQueueWithMemory(c.lqMemory))
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
			name:     "cores smaller than memory rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "50", names.coresPct: "20"},
			wantErr:  true,
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
