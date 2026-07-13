package worker

import (
	"context"
	"encoding/json"
	"strings"
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
		ObjectMeta: meta.ObjectMeta{Name: "gpustack--generic-linux-amd64"},
		Spec: workercore.InstanceTypeSpec{
			GeneralGroup:  "generic",
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

// TestInstanceTypeWebhook_ValidateCreateRequired pins the required inputs: os/arch always, and
// acceleratorGroup only when acceleratable. A non-accelerated type needs no group (the mutating
// webhook defaults GeneralGroup to "generic"), so an empty group is not rejected here.
func TestInstanceTypeWebhook_ValidateCreateRequired(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(it *workercore.InstanceType)
	}{
		{"missing acceleratorGroup when acceleratable", func(it *workercore.InstanceType) {
			it.Spec.Acceleratable = true
			it.Spec.AcceleratorGroup = ""
		}},
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
			Name: "gpustack--generic--nvidia-a10g-linux-amd64-2d",
			Labels: map[string]string{
				nodefeature.NodeAcceleratableLabelKey:                       "true",
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
				AcceleratorGroup: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64",
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

	t.Run("accelerated type without accelerator group is a no-op", func(t *testing.T) {
		it := &workercore.InstanceType{Spec: workercore.InstanceTypeSpec{Acceleratable: true}}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Empty(t, it.Spec.Manufacturer, "no flavor is queried without an accelerator group")
	})

	t.Run("no matching flavor leaves the spec intact", func(t *testing.T) {
		it := &workercore.InstanceType{
			Spec: workercore.InstanceTypeSpec{AcceleratorGroup: "nvidia-h100", Acceleratable: true, OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Empty(t, it.Spec.Manufacturer, "an unmatched group enriches nothing")
	})

	t.Run("already-populated descriptors are left untouched (enrich-once)", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "preset"},
			Spec: workercore.InstanceTypeSpec{
				AcceleratorGroup: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64",
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
				AcceleratorGroup: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64",
			},
		}
		it.Spec.Memory = "12Gi" // an accel type with VRAM present is treated as populated
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Empty(t, it.Spec.Manufacturer, "an accel type that already has memory is not enriched")
		assert.Equal(t, "12Gi", it.Spec.Memory, "existing memory is preserved")
	})
}

// TestInstanceTypeWebhook_DefaultStampsScheduleLabels pins that Default stamps the pool's
// schedule labels (the acceleratable boolean, the feature keys, kubernetes.io/os|arch) on the
// InstanceType from its spec identity, and prunes a stale feature key when the
// group/acceleratable-ness changes. The unit binary resolves instance-type-aware-cpu-manufacturer
// to false, so a non-accelerated pool collapses (no general.* key, just acceleratable=false).
func TestInstanceTypeWebhook_DefaultStampsScheduleLabels(t *testing.T) {
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build() // no flavor: labels still stamped
	wh := &InstanceTypeWebhook{Client: cli}

	t.Run("accelerated type gets the acceleratable feature key + boolean + os/arch", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "my-a10g"},
			Spec:       workercore.InstanceTypeSpec{AcceleratorGroup: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, "true", it.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-a10g"], "accelerated feature key")
		assert.Equal(t, "true", it.Labels[nodefeature.NodeAcceleratableLabelKey], "acceleratable=true discriminator")
		assert.Equal(t, "linux", it.Labels[core.LabelOSStable])
		assert.Equal(t, "amd64", it.Labels[core.LabelArchStable])
		assert.Equal(t, nodefeature.FormatLocalQueueName("my-a10g"), it.Labels[QueueEntranceLabelKey],
			"entrance label advertises the fronting LocalQueue name")
	})

	t.Run("cpu-only type collapses to the acceleratable=false discriminator (unaware)", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "my-generic"},
			Spec:       workercore.InstanceTypeSpec{GeneralGroup: "generic", OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, "false", it.Labels[nodefeature.NodeAcceleratableLabelKey], "acceleratable=false discriminator")
		assert.NotContains(t, it.Labels, nodefeature.GeneralFeatureLabelPrefix+"generic", "no general key when unaware/collapsed")
		assert.NotContains(t, it.Labels, nodefeature.AcceleratableFeatureLabelPrefix+"generic")
	})

	t.Run("prunes a stale feature key on a group/acceleratable change", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{
				Name:   "repoint",
				Labels: map[string]string{nodefeature.GeneralFeatureLabelPrefix + "amd-epyc-7763": "true"}, // prior identity
			},
			Spec: workercore.InstanceTypeSpec{AcceleratorGroup: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, "true", it.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-a10g"], "new feature key stamped")
		assert.NotContains(t, it.Labels, nodefeature.GeneralFeatureLabelPrefix+"amd-epyc-7763", "stale feature key pruned")
	})
}

// TestInstanceTypeWebhook_FoldCPUDetail pins the cpuDetail JSON ↔ spec mapping the defaulting
// webhook applies when awareness is on: a non-accelerated type carries the raw CPU detail as the
// embedded spec.CPU (promoted PhysicalCores etc.), while an accelerated type carries it as
// spec.Accelerator.CPU — which also records the CPU's own manufacturer/product/family, distinct
// from the device's. The note is the JSON the NodeFlavorReconciler marshals (an
// InstanceTypeAcceleratorCPU), so this round-trips the single typed source.
func TestInstanceTypeWebhook_FoldCPUDetail(t *testing.T) {
	detail := workercore.InstanceTypeAcceleratorCPU{
		Manufacturer: "amd",
		Product:      "AMD EPYC 7763",
		Family:       "25",
		InstanceTypeCPU: workercore.InstanceTypeCPU{
			PhysicalCores: "64",
			LogicalCores:  "128",
			Cache:         workercore.InstanceTypeCPUCache{L3: "256MiB"},
		},
	}
	accelRaw, err := json.Marshal(detail)
	require.NoError(t, err)
	// A non-accelerated flavor's cpuDetail note is a plain InstanceTypeCPU (its
	// manufacturer/product/family are the InstanceType's top-level descriptors).
	cpuRaw, err := json.Marshal(detail.InstanceTypeCPU)
	require.NoError(t, err)

	t.Run("non-accelerated folds an InstanceTypeCPU into the embedded spec.CPU", func(t *testing.T) {
		it := &workercore.InstanceType{Spec: workercore.InstanceTypeSpec{Acceleratable: false}}
		foldCPUDetail(it, string(cpuRaw))
		assert.Equal(t, "64", it.Spec.PhysicalCores, "promoted from the embedded InstanceTypeCPU")
		assert.Equal(t, "128", it.Spec.LogicalCores)
		assert.Equal(t, "256MiB", it.Spec.Cache.L3)
		assert.Empty(t, it.Spec.CPU.Manufacturer, "the accelerator CPU is untouched for a non-accelerated type")
	})

	t.Run("accelerated folds an InstanceTypeAcceleratorCPU into spec.Accelerator.CPU", func(t *testing.T) {
		it := &workercore.InstanceType{Spec: workercore.InstanceTypeSpec{Acceleratable: true}}
		foldCPUDetail(it, string(accelRaw))
		assert.Equal(t, "amd", it.Spec.CPU.Manufacturer, "the accelerator CPU carries the CPU's own manufacturer")
		assert.Equal(t, "AMD EPYC 7763", it.Spec.CPU.Product)
		assert.Equal(t, "64", it.Spec.CPU.PhysicalCores)
		assert.Empty(t, it.Spec.PhysicalCores, "the embedded top-level CPU detail is untouched for an accelerated type")
	})

	t.Run("empty note is a no-op", func(t *testing.T) {
		it := &workercore.InstanceType{Spec: workercore.InstanceTypeSpec{Acceleratable: false}}
		foldCPUDetail(it, "")
		assert.Empty(t, it.Spec.PhysicalCores)
	})

	t.Run("malformed note leaves the existing CPU detail unchanged", func(t *testing.T) {
		it := &workercore.InstanceType{Spec: workercore.InstanceTypeSpec{Acceleratable: true}}
		it.Spec.CPU = workercore.InstanceTypeAcceleratorCPU{Manufacturer: "amd", Product: "AMD EPYC 7763"}
		foldCPUDetail(it, "{not valid json")
		assert.Equal(t, "amd", it.Spec.CPU.Manufacturer, "a malformed note must not clobber the spec to zero")
		assert.Equal(t, "AMD EPYC 7763", it.Spec.CPU.Product)
	})
}

// TestInstanceTypeWebhook_DefaultClearsCPUDescriptorsWhenAgnostic pins that with CPU-manufacturer
// awareness off (the unit binary default) a non-acceleratable type is manufacturer-agnostic: the
// Default webhook clears its CPU descriptors and skips enrichment entirely (no flavor is stamped),
// because the collapsed pool spans many CPU kinds and no single flavor represents it. The schedule
// and entrance labels are still stamped — the guard runs after label stamping.
func TestInstanceTypeWebhook_DefaultClearsCPUDescriptorsWhenAgnostic(t *testing.T) {
	cpuRF := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name: "gpustack--amd-epyc-7763-linux-amd64-8c",
			Labels: map[string]string{
				nodefeature.NodeAcceleratableLabelKey: "false",
				core.LabelOSStable:                    "linux",
				core.LabelArchStable:                  "amd64",
			},
		},
	}
	// A matching CPU flavor exists; under awareness-off the webhook must NOT stamp its manufacturer
	// onto the agnostic pool.
	systemmeta.NoteResource(cpuRF, "nodes", map[string]string{
		"acceleratable": "false",
		"manufacturer":  "amd",
		"product":       "AMD EPYC 7763",
		"family":        "25",
		"cpuDetail":     `{"physicalCores":"64"}`,
	})
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cpuRF).Build()
	wh := &InstanceTypeWebhook{Client: cli}

	// A pre-set manufacturer/product/family is cleared too — an agnostic pool carries no
	// representative CPU identity.
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "my-generic"},
		Spec: workercore.InstanceTypeSpec{
			GeneralGroup: "generic", OS: "linux", Arch: "amd64",
			Manufacturer: "stale", Product: "Stale", Family: "stale",
		},
	}
	require.NoError(t, wh.Default(context.Background(), it))
	assert.Empty(t, it.Spec.Manufacturer, "manufacturer cleared (no representative for an agnostic pool)")
	assert.Empty(t, it.Spec.Product, "product cleared")
	assert.Empty(t, it.Spec.Family, "family cleared")
	assert.Empty(t, it.Spec.PhysicalCores, "cpuDetail is not folded")
	// Labels are still stamped (the guard runs after label stamping).
	assert.Equal(t, "false", it.Labels[nodefeature.NodeAcceleratableLabelKey], "acceleratable=false discriminator stamped")
	assert.Equal(t, nodefeature.FormatLocalQueueName("my-generic"), it.Labels[QueueEntranceLabelKey], "entrance label stamped")
}

// TestInstanceTypeWebhook_DefaultDisplayName pins the DisplayName default: it copies the
// (possibly just-enriched) Product when absent and preserves an admin-provided value. On the
// CPU-agnostic guard path Product is empty, so a defaulted DisplayName falls back to "CPU-only".
func TestInstanceTypeWebhook_DefaultDisplayName(t *testing.T) {
	accelRF := &kueue.ResourceFlavor{
		ObjectMeta: meta.ObjectMeta{
			Name: "gpustack--generic--nvidia-a10g-linux-amd64-2d",
			Labels: map[string]string{
				nodefeature.NodeAcceleratableLabelKey:                       "true",
				nodefeature.AcceleratableFeatureLabelPrefix + "nvidia-a10g": "true",
				core.LabelOSStable:   "linux",
				core.LabelArchStable: "amd64",
			},
		},
	}
	systemmeta.NoteResource(accelRF, "nodes", map[string]string{
		"acceleratable": "true", "manufacturer": "nvidia", "product": "NVIDIA A10G", "memory": "24Gi",
	})
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(accelRF).Build()
	wh := &InstanceTypeWebhook{Client: cli}

	newAccel := func() *workercore.InstanceType {
		return &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "my-a10g"},
			Spec:       workercore.InstanceTypeSpec{AcceleratorGroup: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64"},
		}
	}

	t.Run("defaults to the enriched product on the enriched path", func(t *testing.T) {
		it := newAccel()
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, "NVIDIA A10G", it.Spec.DisplayName, "defaults to the enriched Product")
	})

	t.Run("preserves an admin-provided display name", func(t *testing.T) {
		it := newAccel()
		it.Spec.DisplayName = "Custom Name"
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, "Custom Name", it.Spec.DisplayName, "an admin value is preserved")
	})

	t.Run("defaults to CPU-only on the CPU-agnostic guard path", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "my-generic"},
			Spec:       workercore.InstanceTypeSpec{GeneralGroup: "generic", OS: "linux", Arch: "amd64"},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, "CPU-only", it.Spec.DisplayName, "the agnostic path defaults DisplayName to CPU-only")
	})

	t.Run("preserves an admin-provided display name on the guard path", func(t *testing.T) {
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "my-generic"},
			Spec:       workercore.InstanceTypeSpec{GeneralGroup: "generic", OS: "linux", Arch: "amd64", DisplayName: "Agnostic Pool"},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, "Agnostic Pool", it.Spec.DisplayName, "an admin value survives the guard path")
	})

	t.Run("caps a defaulted display name at 64 characters", func(t *testing.T) {
		long := strings.Repeat("x", 100)
		it := &workercore.InstanceType{
			ObjectMeta: meta.ObjectMeta{Name: "my-long"},
			Spec: workercore.InstanceTypeSpec{
				AcceleratorGroup: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64",
				Manufacturer: "nvidia", Product: long,
			},
		}
		require.NoError(t, wh.Default(context.Background(), it))
		assert.Equal(t, strings.Repeat("x", 64), it.Spec.DisplayName,
			"a defaulted DisplayName longer than the maxLength is capped at 64 runes")
	})
}
