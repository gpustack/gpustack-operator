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

// newInstanceType builds a non-acceleratable InstanceType carrying the required admin inputs (a
// generic linux/amd64 pool) plus the given unit spec, so the unit-spec cases exercise unit
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

// newAcceleratableInstanceType builds an acceleratable InstanceType (an nvidia-a10g linux/amd64
// pool) plus the given unit spec, so the unit-CPU well-formedness cases (any positive integer)
// are exercised without tripping the CPU-only unit-CPU-must-be-1 rule.
func newAcceleratableInstanceType(cpu, ram, localStorage string) *workercore.InstanceType {
	return &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "gpustack--nvidia-a10g-linux-amd64"},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable:    true,
			AcceleratorGroup: "nvidia-a10g",
			OS:               "linux",
			Arch:             "amd64",
			UnitResources:    workercore.InstanceTypeUnitResources{CPU: cpu, RAM: ram},
			LocalStorage:     localStorage,
		},
	}
}

// TestInstanceTypeWebhook_ValidateUnitSpec pins the unit-spec rule: all three fields must be set
// and well-formed — unitRAM and localStorage a positive integer with a "Gi" suffix. The unit-CPU
// rule branches on acceleratable: an acceleratable type accepts any unitless positive integer,
// while a non-acceleratable (CPU-only) type accepts only exactly 1 core.
func TestInstanceTypeWebhook_ValidateUnitSpec(t *testing.T) {
	cases := []struct {
		name                   string
		acceleratable          bool
		cpu, ram, localStorage string
		wantErr                bool
	}{
		// The full triple must be supplied and RAM/localStorage well-formed (branch-independent).
		{"empty is rejected (must supply the full triple)", true, "", "", "", true},
		{"partial: only cpu", true, "4", "", "", true},
		{"partial: missing localStorage", true, "4", "48Gi", "", true},
		{"ram without Gi suffix", true, "4", "48", "100Gi", true},
		{"ram lowercase gi", true, "4", "48gi", "100Gi", true},
		{"localStorage zero", true, "4", "48Gi", "0Gi", true},
		// Acceleratable unit CPU: any unitless positive integer.
		{"accel all three valid", true, "4", "48Gi", "100Gi", false},
		{"accel cpu with unit suffix", true, "4Gi", "48Gi", "100Gi", true},
		{"accel cpu fractional", true, "0.5", "48Gi", "100Gi", true},
		{"accel cpu zero", true, "0", "48Gi", "100Gi", true},
		// Non-acceleratable (CPU-only) unit CPU: exactly 1 core.
		{"non-accel cpu 1 is valid", false, "1", "48Gi", "100Gi", false},
		{"non-accel cpu 2 is rejected", false, "2", "48Gi", "100Gi", true},
		{"non-accel cpu 4 is rejected", false, "4", "48Gi", "100Gi", true},
		{"non-accel cpu 0 is rejected", false, "0", "48Gi", "100Gi", true},
		{"non-accel cpu empty is rejected", false, "", "48Gi", "100Gi", true},
		{"non-accel cpu 1000 is rejected", false, "1000", "48Gi", "100Gi", true},
		{"non-accel cpu 1Gi is rejected", false, "1Gi", "48Gi", "100Gi", true},
	}

	wh := &InstanceTypeWebhook{}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			it := newInstanceType(c.cpu, c.ram, c.localStorage)
			if c.acceleratable {
				it = newAcceleratableInstanceType(c.cpu, c.ram, c.localStorage)
			}
			_, err := wh.ValidateCreate(context.Background(), it)
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
			it := newInstanceType("1", "48Gi", "100Gi")
			c.mutate(it)
			_, err := wh.ValidateCreate(context.Background(), it)
			require.Error(t, err, "a missing required input must be rejected")
		})
	}
}

// TestInstanceTypeWebhook_ValidateUpdateImmutable pins that the whole spec is frozen after
// creation except the admin-editable fields: displayName (rename), description (annotation) and
// inactive (take in/out of service). Every other spec field — os/arch, groups, acceleratable,
// unit resources, local storage — is rejected on update.
func TestInstanceTypeWebhook_ValidateUpdateImmutable(t *testing.T) {
	nonAccel := func() *workercore.InstanceType { return newInstanceType("1", "48Gi", "100Gi") }
	// An acceleratable base isolates the acceleratable toggle through the immutability check:
	// flipping it to false keeps the object otherwise valid (unit cpu 1 passes the CPU-only rule),
	// so the rejection is attributable to the spec-immutability guard, not the required-input check.
	accel := func() *workercore.InstanceType { return newAcceleratableInstanceType("1", "48Gi", "100Gi") }
	// A CPU-only object stored with unit cpu other than 1 predates the F1 rule; update must not
	// re-run the create-time check against it, so a displayName/inactive edit (and the operator's
	// own inactive backfill) still goes through while the frozen unit cpu is left as stored.
	legacyNonAccel := func() *workercore.InstanceType { return newInstanceType("8", "48Gi", "100Gi") }

	cases := []struct {
		name    string
		base    func() *workercore.InstanceType
		mutate  func(it *workercore.InstanceType)
		wantErr bool
	}{
		{"displayName change is allowed", nonAccel, func(it *workercore.InstanceType) { it.Spec.DisplayName = "Renamed" }, false},
		{"inactive toggle is allowed", nonAccel, func(it *workercore.InstanceType) { it.Spec.Inactive = true }, false},
		{"legacy non-1 cpu displayName change is allowed", legacyNonAccel, func(it *workercore.InstanceType) { it.Spec.DisplayName = "Renamed" }, false},
		{"legacy non-1 cpu inactive toggle is allowed", legacyNonAccel, func(it *workercore.InstanceType) { it.Spec.Inactive = true }, false},
		{"description change is allowed", nonAccel, func(it *workercore.InstanceType) { it.Spec.Description = "note" }, false},
		{"unitResources change is rejected", nonAccel, func(it *workercore.InstanceType) { it.Spec.UnitResources.RAM = "96Gi" }, true},
		{"localStorage change is rejected", nonAccel, func(it *workercore.InstanceType) { it.Spec.LocalStorage = "200Gi" }, true},
		{"os change is rejected", nonAccel, func(it *workercore.InstanceType) { it.Spec.OS = "windows" }, true},
		{"arch change is rejected", nonAccel, func(it *workercore.InstanceType) { it.Spec.Arch = "arm64" }, true},
		{"generalGroup change is rejected", nonAccel, func(it *workercore.InstanceType) { it.Spec.GeneralGroup = "amd-epyc-7763" }, true},
		{"acceleratorGroup change is rejected", nonAccel, func(it *workercore.InstanceType) { it.Spec.AcceleratorGroup = "nvidia-a10g" }, true},
		{"acceleratable toggle is rejected", accel, func(it *workercore.InstanceType) { it.Spec.Acceleratable = false }, true},
	}

	wh := &InstanceTypeWebhook{}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			base := c.base()
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

// TestInstanceTypeWebhook_DefaultWritesNoDescriptor pins that Default reads nothing from a matching
// ResourceFlavor onto the spec: the observed hardware descriptor lives in Status.Detail (computed by
// the reconciler) and DisplayName is stamped at derivation, so even with a matching flavor present
// Default leaves DisplayName empty and only stamps the schedule + entrance labels.
func TestInstanceTypeWebhook_DefaultWritesNoDescriptor(t *testing.T) {
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
		"family": "ampere", "memory": "24Gi", "cores": "9216",
	})
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(accelRF).Build()
	wh := &InstanceTypeWebhook{Client: cli}

	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "my-a10g"},
		Spec: workercore.InstanceTypeSpec{
			AcceleratorGroup: "nvidia-a10g", Acceleratable: true, OS: "linux", Arch: "amd64",
		},
	}
	require.NoError(t, wh.Default(context.Background(), it))
	assert.Empty(t, it.Spec.DisplayName, "Default no longer defaults DisplayName (derivation stamps it)")
	// The schedule + entrance labels are still stamped.
	assert.Equal(t, "true", it.Labels[nodefeature.AcceleratableFeatureLabelPrefix+"nvidia-a10g"])
	assert.Equal(t, nodefeature.FormatLocalQueueName("my-a10g"), it.Labels[QueueEntranceLabelKey])
}
