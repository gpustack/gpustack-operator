package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// newInstanceType builds an InstanceType carrying the required admin inputs (a generic
// linux/amd64 pool) plus the given unit spec, so the unit-spec cases exercise unit
// validity with the other required fields satisfied.
func newInstanceType(cpu, ram, localStorage string) *workercore.InstanceType {
	return &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "gpustack-generic-linux-amd64"},
		Spec: workercore.InstanceTypeSpec{
			Group:         "generic",
			OS:            "linux",
			Arch:          "amd64",
			UnitResources: workercore.InstanceTypeUnitResources{CPU: cpu, RAM: ram},
			LocalStorage:  localStorage,
		},
	}
}

// TestInstanceTypeWebhook_ValidateUnitSpec pins the unit-spec rule: all three fields must
// be set and well-formed. An empty or partial spec is rejected — a derived type is stamped
// with the fixed default at creation and an admin must supply the full triple.
func TestInstanceTypeWebhook_ValidateUnitSpec(t *testing.T) {
	cases := []struct {
		name                   string
		cpu, ram, localStorage string
		wantErr                bool
	}{
		{"empty is rejected (must supply the full triple)", "", "", "", true},
		{"all three valid", "12", "48Gi", "100Gi", false},
		{"partial: only cpu", "12", "", "", true},
		{"partial: missing localStorage", "12", "48Gi", "", true},
		{"cpu with unit suffix", "12Gi", "48Gi", "100Gi", true},
		{"cpu fractional", "0.5", "48Gi", "100Gi", true},
		{"cpu zero", "0", "48Gi", "100Gi", true},
		{"ram without Gi suffix", "12", "48", "100Gi", true},
		{"ram lowercase gi", "12", "48gi", "100Gi", true},
		{"localStorage zero", "12", "48Gi", "0Gi", true},
	}

	wh := &InstanceTypeWebhook{}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := wh.ValidateCreate(context.Background(), newInstanceType(c.cpu, c.ram, c.localStorage))
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestInstanceTypeWebhook_ValidateCreateRequired pins that group/os/arch are required inputs.
func TestInstanceTypeWebhook_ValidateCreateRequired(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(it *workercore.InstanceType)
	}{
		{"missing group", func(it *workercore.InstanceType) { it.Spec.Group = "" }},
		{"missing os", func(it *workercore.InstanceType) { it.Spec.OS = "" }},
		{"missing arch", func(it *workercore.InstanceType) { it.Spec.Arch = "" }},
	}

	wh := &InstanceTypeWebhook{}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			it := newInstanceType("12", "48Gi", "100Gi")
			c.mutate(it)
			_, err := wh.ValidateCreate(context.Background(), it)
			require.Error(t, err, "a missing required input must be rejected")
		})
	}
}

// TestInstanceTypeWebhook_ValidateUpdateImmutable pins that the unit spec is frozen after
// creation while other fields stay mutable.
func TestInstanceTypeWebhook_ValidateUpdateImmutable(t *testing.T) {
	base := newInstanceType("12", "48Gi", "100Gi")

	cases := []struct {
		name    string
		mutate  func(it *workercore.InstanceType)
		wantErr bool
	}{
		{"unitResources cpu change is rejected", func(it *workercore.InstanceType) { it.Spec.UnitResources.CPU = "24" }, true},
		{"unitResources ram change is rejected", func(it *workercore.InstanceType) { it.Spec.UnitResources.RAM = "96Gi" }, true},
		{"localStorage change is rejected", func(it *workercore.InstanceType) { it.Spec.LocalStorage = "200Gi" }, true},
		{"descriptor change is allowed", func(it *workercore.InstanceType) { it.Spec.Manufacturer = "nvidia" }, false},
	}

	wh := &InstanceTypeWebhook{}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			updated := base.DeepCopy()
			c.mutate(updated)
			_, err := wh.ValidateUpdate(context.Background(), base, updated)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestInstanceTypeWebhook_DefaultEnriches pins the defaulting webhook: when spec.group is set,
// the descriptors are empty, and a matching ResourceFlavor exists, the descriptor fields are
// filled from its notes; it is a no-op when group is empty, no flavor matches, or the
// descriptors are already populated (enrich-once).
func TestInstanceTypeWebhook_DefaultEnriches(t *testing.T) {
	accelRF := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name: "gpustack-nvidia-a10g-linux-amd64-2d",
			Labels: map[string]string{
				nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g": "true",
				core.LabelOSStable:   "linux",
				core.LabelArchStable: "amd64",
			},
		},
	}
	systemmeta.NoteResource(accelRF, "nodes", map[string]string{
		"acceleratable": "true", "manufacturer": "nvidia", "product": "NVIDIA A10G",
		"family": "ampere", "memory": "24Gi", "cores": "9216", "sliceable": "true",
	})
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(accelRF).Build()
	wh := &InstanceTypeWebhook{Client: cli}

	t.Run("accelerated type is enriched from the matching flavor", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "my-a10g"},
			Spec: workercore.InstanceTypeSpec{
				Group: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64",
			},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, "nvidia", it.Spec.Manufacturer)
		assert.Equal(t, "NVIDIA A10G", it.Spec.Product)
		assert.Equal(t, "ampere", it.Spec.Family)
		assert.Equal(t, "24Gi", it.Spec.Memory)
		assert.Equal(t, "9216", it.Spec.Cores)
		assert.True(t, it.Spec.Sliceable)
	})

	t.Run("empty group is a no-op", func(t *testing.T) {
		it := &workercore.InstanceType{Spec: workercore.InstanceTypeSpec{Acceleratable: true}}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Empty(t, it.Spec.Manufacturer, "no flavor is queried without a group")
	})

	t.Run("no matching flavor leaves the spec intact", func(t *testing.T) {
		it := &workercore.InstanceType{
			Spec: workercore.InstanceTypeSpec{Group: "nvidia-h100", Acceleratable: true, OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Empty(t, it.Spec.Manufacturer, "an unmatched group enriches nothing")
	})

	t.Run("already-populated descriptors are left untouched (enrich-once)", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "preset"},
			Spec: workercore.InstanceTypeSpec{
				Group: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64",
				Manufacturer: "preset-manu", Product: "Preset Card",
			},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, "preset-manu", it.Spec.Manufacturer, "existing descriptors are not re-enriched")
		assert.Equal(t, "Preset Card", it.Spec.Product)
		assert.Empty(t, it.Spec.Memory, "no flavor is queried once descriptors are set")
	})

	t.Run("accelerated type already carrying memory skips enrichment", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "preset-mem"},
			Spec: workercore.InstanceTypeSpec{
				Group: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64",
			},
		}
		it.Spec.Memory = "12Gi" // an accel type with VRAM present is treated as populated
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Empty(t, it.Spec.Manufacturer, "an accel type that already has memory is not enriched")
		assert.Equal(t, "12Gi", it.Spec.Memory, "existing memory is preserved")
	})
}
