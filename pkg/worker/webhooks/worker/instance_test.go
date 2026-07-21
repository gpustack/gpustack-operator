package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/system"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

func newInstanceWebhook(objs ...ctrlcli.Object) *InstanceWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
	return &InstanceWebhook{Client: cli, APIReader: cli}
}

// sliceableDetail is the observed accelerator Detail that marks a fixture InstanceType as
// slice-ready: a manufacturer (so Status.Detail.AcceleratorReady is true) plus a non-zero logical
// slice count (so Status.Detail.IsSliceable is true). The webhook now reads sliceability and
// readiness from Status.Detail, so a slice-path fixture must set it.
var sliceableDetail = workercore.InstanceTypeDetail{
	Manufacturer: "nvidia",
	InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
		SlicedDetail: workercore.AcceleratorSlicedDetail{
			Logical: workercore.AcceleratorSlicedLogicalDetail{Count: 128},
		},
	},
}

// webhookInstance builds a valid Instance (with a volume so non-type validation
// passes) referencing the given InstanceType.
func webhookInstance(name, instType string) *workercore.Instance {
	return &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: name},
		Spec: workercore.InstanceSpec{
			Type:             instType,
			InstanceTemplate: workercore.InstanceTemplate{Image: "img"},
			Volume: workercore.InstanceVolume{
				Ephemeral: &workercore.InstanceEphemeralVolume{Capacity: resource.MustParse("10Gi")},
			},
		},
	}
}

func TestInstanceWebhook_ValidateCreate(t *testing.T) {
	cases := []struct {
		name string

		instType string
		stop     bool

		wantErr bool
	}{
		{
			name:     "stopped allows missing type",
			instType: "missing",
			stop:     true,
		},
		{
			name:     "running requires type",
			instType: "missing",
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook()
			inst := webhookInstance("a", c.instType)
			if c.stop {
				inst.Spec.Stop = true
			}

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_SlicedPercentages pins the sliced request
// contract: the memory/compute percentages must be in [0,100] and are independent
// of each other.
func TestInstanceWebhook_ValidateCreate_SlicedPercentages(t *testing.T) {
	const typeName = "sliced-8s"

	cases := []struct {
		name             string
		memPct, coresPct int32
		wantErr          bool
	}{
		{name: "equal budgets allowed", memPct: 20, coresPct: 20},
		{name: "compute larger than memory allowed", memPct: 20, coresPct: 50},
		{name: "whole-card slice allowed", memPct: 100, coresPct: 100},
		{name: "no slice allowed", memPct: 0, coresPct: 0},
		{name: "compute smaller than memory allowed", memPct: 50, coresPct: 20},
		{name: "memory above 100 rejected", memPct: 101, coresPct: 101, wantErr: true},
		{name: "compute above 100 rejected", memPct: 20, coresPct: 101, wantErr: true},
		{name: "negative memory rejected", memPct: -1, wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
				},
				Status: workercore.InstanceTypeStatus{
					Detail:      sliceableDetail,
					Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			acc := resource.MustParse("1")
			inst.Spec.Resources = &workercore.InstanceResources{
				Accelerator:                       &acc,
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_ResourceCaps pins that a Create is rejected when
// the request exceeds the InstanceType's per-unit RAM (unitRAM × count) or its
// LocalStorage, and accepted within those caps. The count is the accelerator request
// for an acceleratable type, else the CPU request.
func TestInstanceWebhook_ValidateCreate_ResourceCaps(t *testing.T) {
	const typeName = "gpustack-generic-linux-amd64"

	cases := []struct {
		name          string
		acceleratable bool
		unitRAM       string
		localCap      string
		count         int64
		ram           string
		localStorage  string
		wantErr       bool
	}{
		{name: "non-accel RAM within cap", unitRAM: "2Gi", localCap: "64Gi", count: 2, ram: "4Gi", localStorage: "32Gi"},
		{name: "non-accel RAM over cap", unitRAM: "2Gi", localCap: "64Gi", count: 2, ram: "5Gi", localStorage: "32Gi", wantErr: true},
		{name: "non-accel local storage over cap", unitRAM: "2Gi", localCap: "64Gi", count: 2, ram: "4Gi", localStorage: "100Gi", wantErr: true},
		{name: "accel within caps", acceleratable: true, unitRAM: "4Gi", localCap: "128Gi", count: 2, ram: "8Gi", localStorage: "64Gi"},
		{name: "accel RAM over cap", acceleratable: true, unitRAM: "4Gi", localCap: "128Gi", count: 2, ram: "16Gi", localStorage: "64Gi", wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: c.acceleratable,
					UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: c.unitRAM},
					LocalStorage:  c.localCap,
				},
				Status: workercore.InstanceTypeStatus{
					Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("100")},
					CPU:         workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("100")},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			ram := resource.MustParse(c.ram)
			local := resource.MustParse(c.localStorage)
			cnt := resource.NewQuantity(c.count, resource.DecimalSI)
			res := &workercore.InstanceResources{RAM: ram, LocalStorage: local}
			if c.acceleratable {
				res.Accelerator = cnt
			} else {
				res.CPU = *cnt
			}
			inst.Spec.Resources = res

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_AcceleratedCPU pins the accelerated-CPU contract. A real
// accelerated InstanceType reports the three-view, not the CPU view, so its Status.CPU is zero —
// capping the CPU request against Status.CPU.OnceMaxRequest would reject every accelerated Instance
// (the real-cluster regression). Instead the CPU is bounded by unitResources.cpu × accelerator
// count: accepted at the cap (what defaulting sets), rejected above it.
func TestInstanceWebhook_ValidateCreate_AcceleratedCPU(t *testing.T) {
	const typeName = "gpustack-nvidia-a10g-linux-amd64"

	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "2", RAM: "8Gi"},
			LocalStorage:  "64Gi",
		},
		Status: workercore.InstanceTypeStatus{
			// A real accelerated type reports the three-view; Status.CPU stays zero.
			Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
		},
	}
	w := newInstanceWebhook(instType)

	cases := []struct {
		name    string
		cpu     string // request on a single-accelerator Instance; cap is unitCPU(2) × 1
		wantErr bool
	}{
		{name: "cpu at unitCPU x count accepted", cpu: "2"},
		{name: "cpu above unitCPU x count rejected", cpu: "3", wantErr: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			inst := webhookInstance("a", typeName)
			inst.Spec.Resources = &workercore.InstanceResources{
				CPU:          resource.MustParse(c.cpu),
				RAM:          resource.MustParse("8Gi"),
				LocalStorage: resource.MustParse("20Gi"),
				Accelerator:  resource.NewQuantity(1, resource.DecimalSI),
			}
			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestInstanceWebhook_ValidateUpdate(t *testing.T) {
	cases := []struct {
		name string

		oldType, newType string
		oldStop, newStop bool
		phase            string // applied to both old and new status
		registerType     string // "" → no InstanceType registered

		wantErr bool
	}{
		{
			name:    "stopped allows type change",
			oldType: "old", newType: "new",
			oldStop: true, newStop: true,
			phase: workerctrl.InstancePhaseStopped,
		},
		{
			name:    "running forbids type change",
			oldType: "old", newType: "new",
			phase:   workerctrl.InstancePhaseReady,
			wantErr: true,
		},
		{
			name:    "start stopped requires existing type",
			oldType: "gone", newType: "gone",
			oldStop: true, newStop: false,
			phase:   workerctrl.InstancePhaseStopped,
			wantErr: true,
		},
		{
			name:    "start stopped with existing type allowed",
			oldType: "live", newType: "live",
			oldStop: true, newStop: false,
			phase:        workerctrl.InstancePhaseStopped,
			registerType: "live",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.registerType != "" {
				objs = append(objs, &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: c.registerType}})
			}
			w := newInstanceWebhook(objs...)

			old := webhookInstance("a", c.oldType)
			old.Spec.Stop = c.oldStop
			old.Status.Phase = c.phase
			neu := webhookInstance("a", c.newType)
			neu.Spec.Stop = c.newStop
			neu.Status.Phase = c.phase

			_, err := w.ValidateUpdate(context.Background(), old, neu)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateUpdate_StartRevalidatesResources pins that starting a stopped
// Instance re-validates the resources that will take effect with the SAME checks as create — not
// just the upper caps. A stopped Instance's resources are mutable (the immutability guard is
// skipped while stopped), so without this a request create would reject (CPU over the non-accel
// cap, a negative quantity, an out-of-range slice percentage) could be slipped in while stopped
// and then started.
func TestInstanceWebhook_ValidateUpdate_StartRevalidatesResources(t *testing.T) {
	const genType = "gpustack-generic-linux-amd64"
	const sliceType = "gpustack-nvidia-a10g-linux-amd64"

	generic := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: genType},
		Spec: workercore.InstanceTypeSpec{
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
			LocalStorage:  "64Gi",
		},
		Status: workercore.InstanceTypeStatus{
			CPU: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("48")},
		},
	}
	sliceable := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: sliceType},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "2", RAM: "8Gi"},
			LocalStorage:  "64Gi",
		},
		Status: workercore.InstanceTypeStatus{
			Detail:      sliceableDetail,
			Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("1")},
		},
	}

	cases := []struct {
		name     string
		instType string
		res      *workercore.InstanceResources
		wantErr  bool
	}{
		{
			name:     "start within caps allowed",
			instType: genType,
			res: &workercore.InstanceResources{
				CPU: resource.MustParse("1"), RAM: resource.MustParse("2Gi"), LocalStorage: resource.MustParse("10Gi"),
			},
		},
		{
			name:     "start with non-accel cpu over cap rejected",
			instType: genType,
			res: &workercore.InstanceResources{
				CPU: resource.MustParse("999"), RAM: resource.MustParse("2Gi"), LocalStorage: resource.MustParse("10Gi"),
			},
			wantErr: true,
		},
		{
			name:     "start with negative ram rejected",
			instType: genType,
			res: &workercore.InstanceResources{
				CPU: resource.MustParse("1"), RAM: resource.MustParse("-2Gi"), LocalStorage: resource.MustParse("10Gi"),
			},
			wantErr: true,
		},
		{
			name:     "start with out-of-range slice percentage rejected",
			instType: sliceType,
			res: &workercore.InstanceResources{
				Accelerator:                       resource.NewQuantity(1, resource.DecimalSI),
				AcceleratorSlicedMemoryPercentage: 200,
				AcceleratorSlicedCoresPercentage:  200,
			},
			wantErr: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook(generic, sliceable)

			old := webhookInstance("a", c.instType)
			old.Spec.Stop = true
			old.Status.Phase = workerctrl.InstancePhaseStopped
			neu := webhookInstance("a", c.instType)
			neu.Spec.Stop = false
			neu.Status.Phase = workerctrl.InstancePhaseStopped
			neu.Spec.Resources = c.res

			_, err := w.ValidateUpdate(context.Background(), old, neu)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestInstanceWebhook_Default(t *testing.T) {
	cases := []struct {
		name string

		stop  bool
		phase string

		wantErr bool
	}{
		{
			// Fresh (Phase "", not stopped) → the type must exist.
			name:    "fresh requires type",
			wantErr: true,
		},
		{
			name: "stopped skips type",
			stop: true,
		},
		{
			name:    "running update skips type",
			phase:   workerctrl.InstancePhaseReady,
			wantErr: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook()
			inst := webhookInstance("a", "missing")
			if c.stop {
				inst.Spec.Stop = true
			}
			inst.Status.Phase = c.phase

			err := w.Default(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_Default_SlicedPercentages verifies the webhook copies a
// lone memory/compute percentage to the other so they default to an equal share.
func TestInstanceWebhook_Default_SlicedPercentages(t *testing.T) {
	// Default reads a setting through the loopback client; point it at an empty
	// fake cluster (configured once) so the setting falls back to its default.
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-8s"

	cases := []struct {
		name                     string
		memPct, coresPct         int32
		wantMemPct, wantCoresPct int32
	}{
		{name: "memory copied to cores", memPct: 20, coresPct: 0, wantMemPct: 20, wantCoresPct: 20},
		{name: "cores copied to memory", memPct: 0, coresPct: 30, wantMemPct: 30, wantCoresPct: 30},
		{name: "both set left unchanged", memPct: 20, coresPct: 40, wantMemPct: 20, wantCoresPct: 40},
		{name: "both zero left unchanged", memPct: 0, coresPct: 0, wantMemPct: 0, wantCoresPct: 0},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
					UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
				},
				Status: workercore.InstanceTypeStatus{Detail: sliceableDetail},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			acc := resource.MustParse("1")
			inst.Spec.Resources = &workercore.InstanceResources{
				Accelerator:                       &acc,
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}

			err := w.Default(context.Background(), inst)
			assert.NoError(t, err)
			assert.Equal(t, c.wantMemPct, inst.Spec.Resources.AcceleratorSlicedMemoryPercentage, "memory percentage")
			assert.Equal(t, c.wantCoresPct, inst.Spec.Resources.AcceleratorSlicedCoresPercentage, "cores percentage")
		})
	}
}

// TestInstanceWebhook_Default_SlicedUnitScaling pins that on a sliceable InstanceType the
// defaulted CPU/RAM are both sized by the memory slice percentage of ONE card's unit resources
// (the fraction of the card actually reserved; the compute percentage throttles GPU cores only,
// not host resources), flooring fractions and never dropping below one, while a zero (no-slice)
// percentage takes the whole card's unit.
func TestInstanceWebhook_Default_SlicedUnitScaling(t *testing.T) {
	// Default reads the overcommit setting through the loopback client; point it at an empty
	// fake cluster so it falls back to its default (which recomputes CPU/RAM regardless).
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-a10g"
	const unitCPU = "16"
	const unitRAM = "40Gi"

	cases := []struct {
		name             string
		memPct, coresPct int32
		wantCPU          int64 // cores
		wantRAMGi        int64
	}{
		{name: "half slice", memPct: 50, coresPct: 50, wantCPU: 8, wantRAMGi: 20},
		{name: "quarter slice", memPct: 25, coresPct: 25, wantCPU: 4, wantRAMGi: 10},
		{name: "cores rounds down", memPct: 20, coresPct: 20, wantCPU: 3, wantRAMGi: 8},
		{name: "compute share ignored for host cpu", memPct: 20, coresPct: 50, wantCPU: 3, wantRAMGi: 8},
		{name: "tiny slice floors cpu to one", memPct: 5, coresPct: 5, wantCPU: 1, wantRAMGi: 2},
		{name: "no slice takes the whole card", memPct: 0, coresPct: 0, wantCPU: 16, wantRAMGi: 40},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
					UnitResources: workercore.InstanceTypeUnitResources{CPU: unitCPU, RAM: unitRAM},
				},
				Status: workercore.InstanceTypeStatus{Detail: sliceableDetail},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			inst.Spec.Resources = &workercore.InstanceResources{
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}

			err := w.Default(context.Background(), inst)
			assert.NoError(t, err)
			assert.Equal(t, int64(1), inst.Spec.Resources.Accelerator.Value(), "accelerator defaults to 1")
			assert.Equal(t, c.wantCPU, inst.Spec.Resources.CPU.Value(), "cpu cores")
			assert.Equal(t, c.wantRAMGi<<30, inst.Spec.Resources.RAM.Value(), "ram bytes")
		})
	}
}

// TestInstanceWebhook_Default_SlicedZeroAccelerator pins that a sliced request (a non-zero slice
// percentage) whose accelerator count is explicitly zero is defaulted to one card, so it is not
// rejected by validation for not being exactly 1 — a slice is a fraction of ONE card.
func TestInstanceWebhook_Default_SlicedZeroAccelerator(t *testing.T) {
	// Default reads the overcommit setting through the loopback client; point it at an empty
	// fake cluster so it falls back to its default.
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-a10g"
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "40Gi"},
		},
		Status: workercore.InstanceTypeStatus{Detail: sliceableDetail},
	}
	w := newInstanceWebhook(instType)

	inst := webhookInstance("a", typeName)
	zero := resource.MustParse("0")
	inst.Spec.Resources = &workercore.InstanceResources{
		Accelerator:                       &zero,
		AcceleratorSlicedMemoryPercentage: 50,
	}

	err := w.Default(context.Background(), inst)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), inst.Spec.Resources.Accelerator.Value(), "explicit zero accelerator defaults to 1")
}

// TestInstanceWebhook_ValidateCreate_SlicedAccelerator pins that a sliced request (a non-zero
// slice percentage) on a sliceable InstanceType must be a single card: the accelerator count
// must be exactly 1 (the slice is expressed through the memory/compute percentages, not the
// card count). Whole-card (zero-percentage) requests are covered by
// TestInstanceWebhook_ValidateCreate_WholeCardOnSliceable.
func TestInstanceWebhook_ValidateCreate_SlicedAccelerator(t *testing.T) {
	const typeName = "sliced-8s"

	cases := []struct {
		name    string
		acc     string // "" → accelerator left unset
		wantErr bool
	}{
		{name: "one accepted", acc: "1"},
		{name: "two rejected", acc: "2", wantErr: true},
		{name: "zero rejected", acc: "0", wantErr: true},
		{name: "fractional rejected", acc: "1m", wantErr: true}, // Value() rounds "1m" up to 1
		{name: "unset rejected", acc: "", wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
				},
				Status: workercore.InstanceTypeStatus{
					Detail:      sliceableDetail,
					Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			res := &workercore.InstanceResources{
				AcceleratorSlicedMemoryPercentage: 50,
				AcceleratorSlicedCoresPercentage:  50,
			}
			if c.acc != "" {
				q := resource.MustParse(c.acc)
				res.Accelerator = &q
			}
			inst.Spec.Resources = res

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_WholeCardOnSliceable pins that a whole-card request (both
// slice percentages zero) on a sliceable InstanceType is treated as an exclusive request: it
// may span multiple cards up to the InstanceType's whole-card OnceMaxRequest, unlike a sliced
// request which is pinned to one card.
func TestInstanceWebhook_ValidateCreate_WholeCardOnSliceable(t *testing.T) {
	const typeName = "sliced-8s"

	cases := []struct {
		name    string
		acc     string
		wantErr bool
	}{
		{name: "single card accepted", acc: "1"},
		{name: "multi card accepted", acc: "3"},
		{name: "at max accepted", acc: "4"},
		{name: "above max rejected", acc: "5", wantErr: true},
		{name: "negative rejected", acc: "-1", wantErr: true},
		{name: "fractional rejected", acc: "1m", wantErr: true}, // extended resources must be whole cards
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
				},
				Status: workercore.InstanceTypeStatus{
					Detail:      sliceableDetail,
					Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			q := resource.MustParse(c.acc)
			// No slice percentages → a whole-card (exclusive) request.
			inst.Spec.Resources = &workercore.InstanceResources{Accelerator: &q}

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_Default_WholeCardScaling pins that a whole-card request (zero slice
// percentages) on a sliceable InstanceType scales the unit CPU/RAM by the accelerator count,
// like a non-sliceable type — not by a single card's slice fraction.
func TestInstanceWebhook_Default_WholeCardScaling(t *testing.T) {
	// Default reads the overcommit setting through the loopback client; point it at an empty
	// fake cluster so it falls back to its default (which recomputes CPU/RAM regardless).
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-a10g"
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "40Gi"},
		},
		Status: workercore.InstanceTypeStatus{Detail: sliceableDetail},
	}
	w := newInstanceWebhook(instType)

	inst := webhookInstance("a", typeName)
	acc := resource.MustParse("3")
	inst.Spec.Resources = &workercore.InstanceResources{Accelerator: &acc}

	err := w.Default(context.Background(), inst)
	assert.NoError(t, err)
	assert.Equal(t, int64(48), inst.Spec.Resources.CPU.Value(), "cpu scales by card count")
	assert.Equal(t, int64(120)<<30, inst.Spec.Resources.RAM.Value(), "ram scales by card count")
}

// TestInstanceWebhook_SlicedRequestNotReadyRejected pins the R3-High fail-safe: a slice request (a
// non-zero slice percentage) on an accelerated InstanceType whose Status.Detail is not yet computed
// is rejected as retryable, never silently sized or validated as a whole-card request. Both the
// mutating Default (which would otherwise fall through to whole-card CPU/RAM scaling) and the
// validating path must reject it.
func TestInstanceWebhook_SlicedRequestNotReadyRejected(t *testing.T) {
	// Default reads the overcommit setting through the loopback client; point it at an empty fake
	// cluster so it falls back to its default.
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-not-ready"
	// Accelerated, but Status.Detail is empty — the reconciler has not computed it yet.
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "40Gi"},
		},
	}
	w := newInstanceWebhook(instType)

	newSliceInstance := func() *workercore.Instance {
		inst := webhookInstance("a", typeName)
		acc := resource.MustParse("1")
		inst.Spec.Resources = &workercore.InstanceResources{
			Accelerator:                       &acc,
			AcceleratorSlicedMemoryPercentage: 50,
		}
		return inst
	}

	// Default must reject with a transient (retryable) error, not fall through to whole-card sizing.
	derr := w.Default(context.Background(), newSliceInstance())
	require.Error(t, derr, "Default must reject a slice request while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(derr),
		"Default rejection is a transient (retryable) error, not a permanent Invalid")

	// ValidateCreate must reject with a transient error, not treat it as a whole-card request.
	_, cerr := w.ValidateCreate(context.Background(), newSliceInstance())
	require.Error(t, cerr, "ValidateCreate must reject a slice request while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(cerr),
		"ValidateCreate rejection is a transient (retryable) error, not a permanent Invalid")

	// ValidateUpdate on the start (resume) path likewise rejects with a transient error.
	old := newSliceInstance()
	old.Spec.Stop = true
	old.Status.Phase = workerctrl.InstancePhaseStopped
	neu := newSliceInstance()
	neu.Spec.Stop = false
	neu.Status.Phase = workerctrl.InstancePhaseStopped
	_, uerr := w.ValidateUpdate(context.Background(), old, neu)
	require.Error(t, uerr, "ValidateUpdate start must reject a slice request while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(uerr),
		"ValidateUpdate start rejection is a transient (retryable) error, not a permanent Invalid")

	// A negative slice percentage is still a slice request: the readiness gate keys on a
	// non-zero (not merely positive) percentage, so it is rejected as retryable rather than
	// falling through to whole-card sizing while Detail is not ready.
	negInst := webhookInstance("a", typeName)
	negAcc := resource.MustParse("1")
	negInst.Spec.Resources = &workercore.InstanceResources{
		Accelerator:                       &negAcc,
		AcceleratorSlicedMemoryPercentage: -1,
	}
	nerr := w.Default(context.Background(), negInst)
	require.Error(t, nerr, "Default must reject a negative slice percentage while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(nerr),
		"negative-percentage rejection is a transient (retryable) error, not a whole-card fallthrough")
}
