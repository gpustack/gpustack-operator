package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// qty parses a quantity for terse fixture construction.
func qty(s string) resource.Quantity { return resource.MustParse(s) }

// qtyEqual compares quantities by value (Cmp) so the assertion does not depend
// on the SI format (BinarySI vs DecimalSI) of the operands.
func qtyEqual(t *testing.T, want, got resource.Quantity, name string) {
	t.Helper()
	assert.Zerof(t, want.Cmp(got), "%s: want %s, got %s", name, want.String(), got.String())
}

// assertResourceList checks that got has exactly the keys in want and that
// each value is equal by Quantity.Cmp.
func assertResourceList(t *testing.T, want, got core.ResourceList, label string) {
	t.Helper()
	keys := func(rl core.ResourceList) []core.ResourceName {
		out := make([]core.ResourceName, 0, len(rl))
		for k := range rl {
			out = append(out, k)
		}
		return out
	}

	assert.Equalf(t, len(want), len(got),
		"%s: expected %d entries (%v), got %d (%v)", label, len(want), keys(want), len(got), keys(got))
	for k, v := range want {
		gv, ok := got[k]
		if !assert.Truef(t, ok, "%s: missing key %s", label, k) {
			continue
		}
		qtyEqual(t, v, gv, label+"["+string(k)+"]")
	}
}

func TestGetResourceRequirements(t *testing.T) {
	accNVIDIA := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	slicedCardNVIDIA := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)
	slicedMemPctNVIDIA := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(nodefeature.ManufacturerNVIDIA)
	slicedCoresPctNVIDIA := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(nodefeature.ManufacturerNVIDIA)

	cases := []struct {
		name string

		// Instance fixture.
		cpu, ram, storage string
		acc               *string // nil → pod did not request accelerator
		memPct, coresPct  int32   // AcceleratorSliced{Memory,Cores}Percentage; 0 → unset

		// InstanceType fixture.
		acceleratable bool
		manufacturer  string
		sliceable     bool // → Spec.Sliceable (true → the accelerator can be sliced)

		// getResourceRequirements flags.
		withGeneral, withGeneralOvercommit, withAccelerator bool

		wantLimits   core.ResourceList
		wantRequests core.ResourceList
	}{
		{
			name: "general only, overcommit off — requests mirror limits",
			cpu:  "4", ram: "16Gi", storage: "32Gi",
			withGeneral: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("16Gi"),
				core.ResourceEphemeralStorage: qty("32Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("16Gi"),
				core.ResourceEphemeralStorage: qty("32Gi"),
			},
		},
		{
			name: "general only, overcommit on, non-acceleratable — 800m / 128Mi bases",
			cpu:  "4", ram: "16Gi", storage: "32Gi",
			withGeneral: true, withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("16Gi"),
				core.ResourceEphemeralStorage: qty("32Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("3200m"), // 800m × 4
				core.ResourceMemory:           qty("2Gi"),   // 128Mi × 16
				core.ResourceEphemeralStorage: qty("4Gi"),   // 128Mi × 32
			},
		},
		{
			name: "general only, overcommit on, acceleratable — CPU uses 100m base",
			cpu:  "16", ram: "64Gi", storage: "128Gi", acc: ptr.To("2"),
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withGeneral: true, withGeneralOvercommit: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("16"),
				core.ResourceMemory:           qty("64Gi"),
				core.ResourceEphemeralStorage: qty("128Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("1600m"), // 100m × 16
				core.ResourceMemory:           qty("8Gi"),   // 128Mi × 64
				core.ResourceEphemeralStorage: qty("16Gi"),  // 128Mi × 128
			},
		},
		{
			name: "accelerator only — exclusive, Limits == Requests",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("2"),
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withAccelerator: true,
			wantLimits:      core.ResourceList{accNVIDIA: qty("2")},
			wantRequests:    core.ResourceList{accNVIDIA: qty("2")},
		},
		{
			// memory 20% / cores 20% on a sliced type → emit the card count plus the
			// per-card percentages; the Pod webhook later folds memory-% into .sliced.units.
			name: "sliced accelerator — one card, 20% memory / 20% cores",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("1"), memPct: 20, coresPct: 20,
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA, sliceable: true,
			withAccelerator: true,
			wantLimits: core.ResourceList{
				slicedCardNVIDIA:     qty("1"),
				slicedMemPctNVIDIA:   qty("20"),
				slicedCoresPctNVIDIA: qty("20"),
			},
			wantRequests: core.ResourceList{
				slicedCardNVIDIA:     qty("1"),
				slicedMemPctNVIDIA:   qty("20"),
				slicedCoresPctNVIDIA: qty("20"),
			},
		},
		{
			// A larger compute share than memory share is allowed.
			name: "sliced accelerator — two cards, 20% memory / 30% cores",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("2"), memPct: 20, coresPct: 30,
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA, sliceable: true,
			withAccelerator: true,
			wantLimits: core.ResourceList{
				slicedCardNVIDIA:     qty("2"),
				slicedMemPctNVIDIA:   qty("20"),
				slicedCoresPctNVIDIA: qty("30"),
			},
			wantRequests: core.ResourceList{
				slicedCardNVIDIA:     qty("2"),
				slicedMemPctNVIDIA:   qty("20"),
				slicedCoresPctNVIDIA: qty("30"),
			},
		},
		{
			// A 0% memory request on a sliced type falls through to exclusive whole cards.
			name: "sliced InstanceType but 0% request — exclusive whole card",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("2"), memPct: 0, coresPct: 0,
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA, sliceable: true,
			withAccelerator: true,
			wantLimits:      core.ResourceList{accNVIDIA: qty("2")},
			wantRequests:    core.ResourceList{accNVIDIA: qty("2")},
		},
		{
			name: "combined — general + accelerator",
			cpu:  "8", ram: "32Gi", storage: "64Gi", acc: ptr.To("4"),
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withGeneral: true, withGeneralOvercommit: true, withAccelerator: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("8"),
				core.ResourceMemory:           qty("32Gi"),
				core.ResourceEphemeralStorage: qty("64Gi"),
				accNVIDIA:                     qty("4"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("800m"), // 100m × 8 (acceleratable base)
				core.ResourceMemory:           qty("4Gi"),  // 128Mi × 32
				core.ResourceEphemeralStorage: qty("8Gi"),  // 128Mi × 64
				accNVIDIA:                     qty("4"),
			},
		},
		{
			name: "accelerator requested but withAccelerator=false — no accelerator entry",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("2"),
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withGeneral: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("16Gi"),
				core.ResourceEphemeralStorage: qty("32Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("16Gi"),
				core.ResourceEphemeralStorage: qty("32Gi"),
			},
		},
		{
			name: "acceleratable InstanceType but pod did not request accelerator (nil)",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: nil,
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withAccelerator: true,
			wantLimits:      core.ResourceList{},
			wantRequests:    core.ResourceList{},
		},
		{
			name: "acceleratable InstanceType but pod requested zero accelerator",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("0"),
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withAccelerator: true,
			wantLimits:      core.ResourceList{},
			wantRequests:    core.ResourceList{},
		},
		{
			name: "non-acceleratable InstanceType ignores accelerator request",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("2"),
			withGeneral: true, withAccelerator: true,
			wantLimits: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("16Gi"),
				core.ResourceEphemeralStorage: qty("32Gi"),
			},
			wantRequests: core.ResourceList{
				core.ResourceCPU:              qty("4"),
				core.ResourceMemory:           qty("16Gi"),
				core.ResourceEphemeralStorage: qty("32Gi"),
			},
		},
		{
			name: "all flags off — empty but initialized maps",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("2"),
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			wantLimits:   core.ResourceList{},
			wantRequests: core.ResourceList{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst := &workercore.Instance{}
			inst.Spec.Resources = &workercore.InstanceResources{
				CPU:          qty(c.cpu),
				RAM:          qty(c.ram),
				LocalStorage: qty(c.storage),
			}
			if c.acc != nil {
				q := qty(*c.acc)
				inst.Spec.Resources.Accelerator = &q
			}
			inst.Spec.Resources.AcceleratorSlicedMemoryPercentage = c.memPct
			inst.Spec.Resources.AcceleratorSlicedCoresPercentage = c.coresPct

			instType := &worker.InstanceType{
				Spec: workercore.InstanceTypeSpec{
					Acceleratable:           c.acceleratable,
					Manufacturer:            c.manufacturer,
					InstanceTypeAccelerator: workercore.InstanceTypeAccelerator{Sliceable: c.sliceable},
				},
			}

			rr := getResourceRequirements(inst, instType,
				c.withGeneral, c.withGeneralOvercommit, c.withAccelerator)

			assert.NotNil(t, rr.Limits, "limits map should be initialized")
			assert.NotNil(t, rr.Requests, "requests map should be initialized")
			assertResourceList(t, c.wantLimits, rr.Limits, "limits")
			assertResourceList(t, c.wantRequests, rr.Requests, "requests")
		})
	}
}

func buildInstanceClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&workercore.Instance{}).
		WithObjects(objs...).
		Build()
}

func reconcileInstance(t *testing.T, cli ctrlcli.Client, namespace, name string) (ctrlreconcile.Result, error) {
	t.Helper()
	r := &InstanceReconciler{Client: cli, APIReader: cli}
	return r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Namespace: namespace, Name: name}})
}

// newReadyInstance builds a running Instance (Ready, no backing Pod) referencing
// the given InstanceType.
func newReadyInstance(namespace, name, instType string) *workercore.Instance {
	return &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       workercore.InstanceSpec{Type: instType},
		Status:     workercore.InstanceStatus{Phase: InstancePhaseReady},
	}
}

func TestInstanceReconciler_Reconcile(t *testing.T) {
	cases := []struct {
		name string

		instType         string
		withInstanceType bool
		itPhase          string

		wantStop bool
	}{
		{
			// A Ready instance whose Pod is gone and whose InstanceType no longer
			// exists (ClusterQueue drained and deleted) must be stopped, not recreated.
			name:     "InstanceType removed stops instance",
			instType: "missing-type",
			wantStop: true,
		},
		{
			// A Ready instance whose Pod is gone while its InstanceType is Inactive
			// (ClusterQueue in HoldAndDrain) must be stopped, not recreated.
			name:             "InstanceType inactive stops instance",
			instType:         "draining-type",
			withInstanceType: true,
			itPhase:          InstanceTypePhaseInactive,
			wantStop:         true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			inst := newReadyInstance("default", "inst", c.instType)
			objs := []ctrlcli.Object{inst}
			if c.withInstanceType {
				objs = append(objs, &worker.InstanceType{
					ObjectMeta: meta.ObjectMeta{Name: c.instType},
					Status:     workercore.InstanceTypeStatus{Phase: c.itPhase},
				})
			}
			cli := buildInstanceClient(objs...)

			_, err := reconcileInstance(t, cli, "default", "inst")
			require.NoError(t, err)

			got := &workercore.Instance{}
			require.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Namespace: "default", Name: "inst"}, got))
			assert.Equal(t, c.wantStop, ptr.Deref(got.Spec.Stop, false), "Spec.Stop")
		})
	}
}
