package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/devicefeature"
)

func qty(s string) resource.Quantity {
	return resource.MustParse(s)
}

func qtyPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func assertResourceListEqual(t *testing.T, want, got core.ResourceList, label string) {
	t.Helper()
	if !assert.Equal(t, len(want), len(got),
		"%s: length mismatch — want %v, got %v", label, want, got) {
		return
	}
	for name, wantQ := range want {
		gotQ, ok := got[name]
		if !assert.Truef(t, ok, "%s: missing %q — want %s", label, name, wantQ.String()) {
			continue
		}
		assert.Truef(t, wantQ.Equal(gotQ),
			"%s[%s]: want %s, got %s", label, name, wantQ.String(), gotQ.String())
	}
}

func TestGetResourceRequirements(t *testing.T) {
	nvidiaExclusive := devicefeature.GetResourceName(
		devicefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	nvidiaSliced := devicefeature.GetResourceName(
		devicefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)

	mkInst := func(cpu, ram, storage string, accel *resource.Quantity) *worker.Instance {
		return &worker.Instance{
			Spec: worker.InstanceSpec{
				InstanceTemplate: worker.InstanceTemplate{
					Resources: &worker.InstanceResources{
						CPU:          qty(cpu),
						RAM:          qty(ram),
						LocalStorage: qty(storage),
						Accelerator:  accel,
					},
				},
			},
		}
	}
	defaultInst := func(accel *resource.Quantity) *worker.Instance {
		return mkInst("4", "8Gi", "15Gi", accel)
	}

	nonAccType := &worker.InstanceType{
		Spec: worker.InstanceTypeSpec{Acceleratable: false},
	}
	accType := &worker.InstanceType{
		Spec: worker.InstanceTypeSpec{
			Acceleratable: true,
			Manufacturer:  devicefeature.ManufacturerNVIDIA,
		},
	}
	accSlicedType := &worker.InstanceType{
		Spec: worker.InstanceTypeSpec{
			Acceleratable: true,
			Manufacturer:  devicefeature.ManufacturerNVIDIA,
			Sliced:        4,
		},
	}

	type tc struct {
		name                  string
		inst                  *worker.Instance
		instType              *worker.InstanceType
		withGeneral           bool
		withGeneralOvercommit bool
		withAccelerator       bool
		wantLimits            core.ResourceList
		wantRequests          core.ResourceList
	}

	cases := []tc{
		{
			name:         "all flags false yields empty maps",
			inst:         defaultInst(nil),
			instType:     nonAccType,
			wantLimits:   core.ResourceList{},
			wantRequests: core.ResourceList{},
		},
		{
			name:        "withGeneral only — Limits == Requests, no accelerator",
			inst:        defaultInst(nil),
			instType:    nonAccType,
			withGeneral: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
			},
		},
		{
			name:                  "overcommit non-acceleratable: CPU=800m×CPU.Value(), RAM/Stg=128Mi×(value/Gi)",
			inst:                  defaultInst(nil),
			instType:              nonAccType,
			withGeneral:           true,
			withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("3200m"),  // 800m * 4
				core.ResourceMemory:           qty("1Gi"),    // 128Mi * 8 = 1024Mi
				core.ResourceEphemeralStorage: qty("1920Mi"), // 128Mi * 15
			},
		},
		{
			name:                  "overcommit acceleratable accel>0: CPU=100m×Accelerator.Value()",
			inst:                  defaultInst(qtyPtr("2")),
			instType:              accType,
			withGeneral:           true,
			withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("200m"),   // 100m * 2
				core.ResourceMemory:           qty("1Gi"),    // 128Mi * 8 (RAM-in-Gi)
				core.ResourceEphemeralStorage: qty("1920Mi"), // 128Mi * 15
			},
		},
		{
			name:                  "overcommit acceleratable accel=0: CPU multiplier falls back to CPU.Value()",
			inst:                  defaultInst(qtyPtr("0")),
			instType:              accType,
			withGeneral:           true,
			withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("400m"),   // 100m * 4 (CPU.Value())
				core.ResourceMemory:           qty("1Gi"),    // 128Mi * 8
				core.ResourceEphemeralStorage: qty("1920Mi"), // 128Mi * 15
			},
		},
		{
			name:                  "overcommit acceleratable nil accel: CPU multiplier falls back to CPU.Value()",
			inst:                  defaultInst(nil),
			instType:              accType,
			withGeneral:           true,
			withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("400m"),   // 100m * 4
				core.ResourceMemory:           qty("1Gi"),    // 128Mi * 8
				core.ResourceEphemeralStorage: qty("1920Mi"), // 128Mi * 15
			},
		},
		{
			name:                  "overcommit with sub-Gi LocalStorage drops storage request to 0",
			inst:                  mkInst("2", "4Gi", "512Mi", nil),
			instType:              nonAccType,
			withGeneral:           true,
			withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("2"),
				core.ResourceMemory:           qty("4Gi"),
				core.ResourceEphemeralStorage: qty("512Mi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("1600m"), // 800m * 2
				core.ResourceMemory:           qty("512Mi"), // 128Mi * 4 (RAM-in-Gi)
				core.ResourceEphemeralStorage: qty("0"),     // 128Mi * 0
			},
		},
		{
			name:                  "overcommit with sub-Gi RAM drops RAM request to 0",
			inst:                  mkInst("2", "512Mi", "10Gi", nil),
			instType:              nonAccType,
			withGeneral:           true,
			withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("2"),
				core.ResourceMemory:           qty("512Mi"),
				core.ResourceEphemeralStorage: qty("10Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("1600m"),  // 800m * 2
				core.ResourceMemory:           qty("0"),      // 128Mi * 0 (RAM<1Gi)
				core.ResourceEphemeralStorage: qty("1280Mi"), // 128Mi * 10
			},
		},
		{
			name:                  "overcommit: RAM uses RAM-in-Gi (not CPU.Value) — disproportionate RAM/CPU ratio",
			inst:                  mkInst("2", "16Gi", "10Gi", nil),
			instType:              nonAccType,
			withGeneral:           true,
			withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("2"),
				core.ResourceMemory:           qty("16Gi"),
				core.ResourceEphemeralStorage: qty("10Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("1600m"),  // 800m * 2
				core.ResourceMemory:           qty("2Gi"),    // 128Mi * 16 = 2048Mi — independent of CPU
				core.ResourceEphemeralStorage: qty("1280Mi"), // 128Mi * 10
			},
		},
		{
			name:                  "withGeneralOvercommit without withGeneral has no effect",
			inst:                  defaultInst(nil),
			instType:              nonAccType,
			withGeneralOvercommit: true,
			wantLimits:            core.ResourceList{},
			wantRequests:          core.ResourceList{},
		},
		{
			name:            "withAccelerator only — exclusive resource name and original value",
			inst:            defaultInst(qtyPtr("2")),
			instType:        accType,
			withAccelerator: true,
			wantLimits: core.ResourceList{
				nvidiaExclusive: qty("2"),
			},
			wantRequests: core.ResourceList{
				nvidiaExclusive: qty("2"),
			},
		},
		{
			name:            "withAccelerator only — sliced resource name and aligned value",
			inst:            defaultInst(qtyPtr("1")),
			instType:        accSlicedType,
			withAccelerator: true,
			wantLimits: core.ResourceList{
				nvidiaSliced: devicefeature.QuantityToAlignedValue(qty("1"), 4),
			},
			wantRequests: core.ResourceList{
				nvidiaSliced: devicefeature.QuantityToAlignedValue(qty("1"), 4),
			},
		},
		{
			name:                  "full payload — general + overcommit + accelerator (accel>0 branch)",
			inst:                  defaultInst(qtyPtr("2")),
			instType:              accType,
			withGeneral:           true,
			withGeneralOvercommit: true,
			withAccelerator:       true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("8Gi"),
				core.ResourceEphemeralStorage: qty("15Gi"),
				nvidiaExclusive:               qty("2"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("200m"),   // 100m * 2 (accel)
				core.ResourceMemory:           qty("1Gi"),    // 128Mi * 8 (RAM-in-Gi)
				core.ResourceEphemeralStorage: qty("1920Mi"), // 128Mi * 15
				nvidiaExclusive:               qty("2"),
			},
		},
		{
			name:            "withAccelerator but non-acceleratable type — no accel resource",
			inst:            defaultInst(qtyPtr("2")),
			instType:        nonAccType,
			withAccelerator: true,
			wantLimits:      core.ResourceList{},
			wantRequests:    core.ResourceList{},
		},
		{
			name:            "withAccelerator but nil Accelerator — no accel resource",
			inst:            defaultInst(nil),
			instType:        accType,
			withAccelerator: true,
			wantLimits:      core.ResourceList{},
			wantRequests:    core.ResourceList{},
		},
		{
			name:            "withAccelerator with zero-value Accelerator on sliced type — Sign()>0 gate skips emission",
			inst:            defaultInst(qtyPtr("0")),
			instType:        accSlicedType,
			withAccelerator: true,
			wantLimits:      core.ResourceList{},
			wantRequests:    core.ResourceList{},
		},
		{
			name:            "withAccelerator with zero-value Accelerator on exclusive type — Sign()>0 gate skips emission",
			inst:            defaultInst(qtyPtr("0")),
			instType:        accType,
			withAccelerator: true,
			wantLimits:      core.ResourceList{},
			wantRequests:    core.ResourceList{},
		},
		{
			name:            "withAccelerator without withGeneral — only accel emitted, no CPU/RAM/storage",
			inst:            defaultInst(qtyPtr("1")),
			instType:        accSlicedType,
			withAccelerator: true,
			wantLimits: core.ResourceList{
				nvidiaSliced: devicefeature.QuantityToAlignedValue(qty("1"), 4),
			},
			wantRequests: core.ResourceList{
				nvidiaSliced: devicefeature.QuantityToAlignedValue(qty("1"), 4),
			},
		},
	}

	for _, cs := range cases {
		cs := cs
		t.Run(cs.name, func(t *testing.T) {
			got := getResourceRequirements(cs.inst, cs.instType,
				cs.withGeneral, cs.withGeneralOvercommit, cs.withAccelerator)
			assertResourceListEqual(t, cs.wantLimits, got.Limits, "limits")
			assertResourceListEqual(t, cs.wantRequests, got.Requests, "requests")
		})
	}
}

func TestScaleBackOvercommitRequest(t *testing.T) {
	nvidiaCredits := devicefeature.GetCreditsResourceName(devicefeature.ManufacturerNVIDIA)

	cases := []struct {
		name          string
		resName       core.ResourceName
		val           resource.Quantity
		acceleratable bool
		want          resource.Quantity
	}{
		{
			name:    "non-acc CPU: 3200m → 4 (multiplier=4, ×1 core)",
			resName: core.ResourceCPU,
			val:     qty("3200m"),
			want:    qty("4"),
		},
		{
			name:    "non-acc CPU: 800m → 1",
			resName: core.ResourceCPU,
			val:     qty("800m"),
			want:    qty("1"),
		},
		{
			name:    "non-acc CPU: 0 → 0",
			resName: core.ResourceCPU,
			val:     qty("0"),
			want:    qty("0"),
		},
		{
			name:          "acc CPU: 200m → 2 (multiplier=2, ×1 core)",
			resName:       core.ResourceCPU,
			val:           qty("200m"),
			acceleratable: true,
			want:          qty("2"),
		},
		{
			name:          "acc CPU: 100m → 1",
			resName:       core.ResourceCPU,
			val:           qty("100m"),
			acceleratable: true,
			want:          qty("1"),
		},
		{
			name:    "RAM: 1Gi → 8Gi (1024Mi/128Mi = 8 × Gi)",
			resName: core.ResourceMemory,
			val:     qty("1Gi"),
			want:    qty("8Gi"),
		},
		{
			name:    "RAM: 128Mi → 1Gi",
			resName: core.ResourceMemory,
			val:     qty("128Mi"),
			want:    qty("1Gi"),
		},
		{
			name:    "RAM: 0 → 0",
			resName: core.ResourceMemory,
			val:     qty("0"),
			want:    qty("0"),
		},
		{
			name:    "RAM: sub-base 64Mi → 0 (integer division loses fraction)",
			resName: core.ResourceMemory,
			val:     qty("64Mi"),
			want:    qty("0"),
		},
		{
			name:    "Storage: 1920Mi → 15Gi (1920Mi/128Mi = 15 × Gi)",
			resName: core.ResourceEphemeralStorage,
			val:     qty("1920Mi"),
			want:    qty("15Gi"),
		},
		{
			name:    "Storage: 0 → 0",
			resName: core.ResourceEphemeralStorage,
			val:     qty("0"),
			want:    qty("0"),
		},
		{
			name:    "Accelerator credits: pass-through (not overcommitted)",
			resName: nvidiaCredits,
			val:     qty("2"),
			want:    qty("2"),
		},
		{
			name:          "Accelerator credits acc-type: pass-through",
			resName:       nvidiaCredits,
			val:           qty("4"),
			acceleratable: true,
			want:          qty("4"),
		},
	}

	for _, cs := range cases {
		cs := cs
		t.Run(cs.name, func(t *testing.T) {
			got := scaleBackOvercommitRequest(cs.resName, cs.val, cs.acceleratable)
			assert.Truef(t, cs.want.Equal(got),
				"want %s, got %s", cs.want.String(), got.String())
		})
	}
}

// TestOvercommitRoundTrip verifies that scaleBackOvercommitRequest is the
// inverse of the requests-side transformation in getResourceRequirements:
// for an arbitrary instance, the overcommit requests scaled back equals the
// original limits (modulo information loss documented in the function).
func TestOvercommitRoundTrip(t *testing.T) {
	nonAccType := &worker.InstanceType{
		Spec: worker.InstanceTypeSpec{Acceleratable: false},
	}
	accType := &worker.InstanceType{
		Spec: worker.InstanceTypeSpec{
			Acceleratable: true,
			Manufacturer:  devicefeature.ManufacturerNVIDIA,
		},
	}

	mkInst := func(cpu, ram, storage string, accel *resource.Quantity) *worker.Instance {
		return &worker.Instance{
			Spec: worker.InstanceSpec{
				InstanceTemplate: worker.InstanceTemplate{
					Resources: &worker.InstanceResources{
						CPU:          qty(cpu),
						RAM:          qty(ram),
						LocalStorage: qty(storage),
						Accelerator:  accel,
					},
				},
			},
		}
	}

	cases := []struct {
		name     string
		inst     *worker.Instance
		instType *worker.InstanceType
		wantCPU  resource.Quantity
		wantRAM  resource.Quantity
		wantStg  resource.Quantity
	}{
		{
			name:     "non-acc 4C/8Gi/15Gi round-trips exactly",
			inst:     mkInst("4", "8Gi", "15Gi", nil),
			instType: nonAccType,
			wantCPU:  qty("4"),
			wantRAM:  qty("8Gi"),
			wantStg:  qty("15Gi"),
		},
		{
			name:     "non-acc 2C/4Gi/10Gi round-trips exactly",
			inst:     mkInst("2", "4Gi", "10Gi", nil),
			instType: nonAccType,
			wantCPU:  qty("2"),
			wantRAM:  qty("4Gi"),
			wantStg:  qty("10Gi"),
		},
		{
			name:     "acc with nil accelerator (multiplier=CPU.Value) round-trips for CPU",
			inst:     mkInst("4", "8Gi", "15Gi", nil),
			instType: accType,
			wantCPU:  qty("4"),
			wantRAM:  qty("8Gi"),
			wantStg:  qty("15Gi"),
		},
		{
			name:     "acc with accel>0: CPU reverses to accel.Value() (info loss is expected)",
			inst:     mkInst("4", "8Gi", "15Gi", qtyPtr("2")),
			instType: accType,
			wantCPU:  qty("2"), // multiplier=2 (from accel), not original 4
			wantRAM:  qty("8Gi"),
			wantStg:  qty("15Gi"),
		},
	}

	for _, cs := range cases {
		cs := cs
		t.Run(cs.name, func(t *testing.T) {
			rr := getResourceRequirements(cs.inst, cs.instType, true, true, false)
			acc := cs.instType.Spec.Acceleratable

			gotCPU := scaleBackOvercommitRequest(core.ResourceCPU, rr.Requests[core.ResourceCPU], acc)
			gotRAM := scaleBackOvercommitRequest(core.ResourceMemory, rr.Requests[core.ResourceMemory], acc)
			gotStg := scaleBackOvercommitRequest(core.ResourceEphemeralStorage, rr.Requests[core.ResourceEphemeralStorage], acc)

			assert.Truef(t, cs.wantCPU.Equal(gotCPU), "CPU: want %s, got %s", cs.wantCPU.String(), gotCPU.String())
			assert.Truef(t, cs.wantRAM.Equal(gotRAM), "RAM: want %s, got %s", cs.wantRAM.String(), gotRAM.String())
			assert.Truef(t, cs.wantStg.Equal(gotStg), "Storage: want %s, got %s", cs.wantStg.String(), gotStg.String())
		})
	}
}
