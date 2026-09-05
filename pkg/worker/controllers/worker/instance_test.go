package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemetrics"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// qty parses a quantity for terse fixture construction.
func qty(s string) resource.Quantity { return resource.MustParse(s) }

// slicedDetail builds the observed accelerator Status.Detail a fixture InstanceType carries: the
// manufacturer (which getResourceRequirements reads for the accelerator resource names) and, when
// sliceable, a non-zero logical slice count so Status.Detail.IsLogicallySliceable() is true. The Pod-sizing
// path reads sliceability and manufacturer from Status.Detail, not the spec.
func slicedDetail(manufacturer string, sliceable bool) workercore.InstanceTypeDetail {
	d := workercore.InstanceTypeDetail{Manufacturer: manufacturer}
	if sliceable {
		d.SlicedDetail.Logical.Count = 128
	}
	return d
}

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
	partCardNVIDIA := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModePartitioned)
	partProfileNVIDIA := nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, "3g.40gb")
	visNVIDIA := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)

	cases := []struct {
		name string

		// Instance fixture.
		cpu, ram, storage string
		acc               *string // nil → pod did not request accelerator
		memPct, coresPct  int32   // AcceleratorSliced{Memory,Cores}Percentage; 0 → unset
		profile           string  // AcceleratorPartitionedProfile; "" → not a partition request

		// InstanceType fixture.
		acceleratable bool
		manufacturer  string
		sliceable     bool // → Status.Detail slicing (true → the accelerator can be sliced)

		// getResourceRequirements flags.
		withGeneral, withGeneralOvercommit, withAccelerator, withVisibility bool

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
			// A partition request emits the two partition keys, both exactly 1. The
			// .partitioned.units credit key is folded by the Pod webhook from the profile's
			// VRAM, so the controller must not write it here.
			name: "partitioned accelerator — the two partition keys, both 1",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("1"), profile: "3g.40gb",
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withAccelerator: true,
			wantLimits: core.ResourceList{
				partCardNVIDIA:    qty("1"),
				partProfileNVIDIA: qty("1"),
			},
			wantRequests: core.ResourceList{
				partCardNVIDIA:    qty("1"),
				partProfileNVIDIA: qty("1"),
			},
		},
		{
			// A partition request wins over a logically sliceable pool's slice keys: the two
			// requests are mutually exclusive, and the webhook rejects the combination.
			name: "partitioned accelerator on a logically sliceable type — still partition keys",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("1"), profile: "1g.10gb",
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA, sliceable: true,
			withAccelerator: true,
			wantLimits: core.ResourceList{
				partCardNVIDIA: qty("1"),
				nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb"): qty("1"),
			},
			wantRequests: core.ResourceList{
				partCardNVIDIA: qty("1"),
				nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, "1g.10gb"): qty("1"),
			},
		},
		{
			// A manufacturer with no partition kind yields no partition key at all, so the
			// request falls back to the whole-card shape instead of emitting an empty key.
			name: "partition profile on a manufacturer with no partition kind — whole card",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("1"), profile: "3g.40gb",
			acceleratable: true, manufacturer: nodefeature.ManufacturerAscend,
			withAccelerator: true,
			wantLimits: core.ResourceList{
				nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerAscend, workercore.DeviceAllocationModeExclusive): qty("1"),
			},
			wantRequests: core.ResourceList{
				nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerAscend, workercore.DeviceAllocationModeExclusive): qty("1"),
			},
		},
		{
			// The SSH sidecar of a partition request still asks only for visibility.
			name: "visibility on a partition request — just the card count",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("1"), profile: "3g.40gb",
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withVisibility: true,
			wantLimits:     core.ResourceList{visNVIDIA: qty("1")},
			wantRequests:   core.ResourceList{visNVIDIA: qty("1")},
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
		{
			// The SSH sidecar requests the internal visibility resource with main's
			// card count, so the device-plugin co-allocates the same physical card(s).
			name: "visibility only — sidecar carries main's card count",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("2"),
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withVisibility: true,
			wantLimits:     core.ResourceList{visNVIDIA: qty("2")},
			wantRequests:   core.ResourceList{visNVIDIA: qty("2")},
		},
		{
			// On a sliced type the sidecar visibility is still just the card count:
			// no slice percentage keys, since it grants device access, not a slice.
			name: "visibility on a sliced type — just the card count, no slice keys",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: ptr.To("1"), memPct: 60, coresPct: 100,
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA, sliceable: true,
			withVisibility: true,
			wantLimits:     core.ResourceList{visNVIDIA: qty("1")},
			wantRequests:   core.ResourceList{visNVIDIA: qty("1")},
		},
		{
			name: "visibility requested but pod did not request accelerator — empty",
			cpu:  "4", ram: "16Gi", storage: "32Gi", acc: nil,
			acceleratable: true, manufacturer: nodefeature.ManufacturerNVIDIA,
			withVisibility: true,
			wantLimits:     core.ResourceList{},
			wantRequests:   core.ResourceList{},
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
			inst.Spec.Resources.AcceleratorPartitionedProfile = c.profile

			instType := &worker.InstanceType{
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: c.acceleratable,
				},
				Status: workercore.InstanceTypeStatus{
					Detail: slicedDetail(c.manufacturer, c.sliceable),
				},
			}

			rr := getResourceRequirements(inst.Spec.Resources, instType,
				c.withGeneral, c.withGeneralOvercommit, c.withAccelerator, c.withVisibility)

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
		stopPolicy       kueue.StopPolicy // backing ClusterQueue StopPolicy; "" leaves it unset (None)
		withCQ           bool
		itDeleting       bool
		withPod          bool

		wantStop bool
	}{
		{
			// A Ready instance whose Pod is gone and whose InstanceType no longer
			// exists (ClusterQueue drained and deleted) must be stopped, not recreated.
			name:     "removed type stops pod-less instance",
			instType: "missing-type",
			wantStop: true,
		},
		{
			// A Hold-Inactive type (admin marked it Inactive without draining; its ClusterQueue
			// is Hold) blocks new admission but keeps running workloads, so it leaves a running
			// instance's Pod untouched.
			name:             "hold-inactive type keeps running instance",
			instType:         "hold-type",
			withInstanceType: true,
			withCQ:           true,
			stopPolicy:       kueue.Hold,
			withPod:          true,
			wantStop:         false,
		},
		{
			// A draining type (its ClusterQueue is HoldAndDrain, evicting admitted workloads)
			// stops a running instance instead of leaving the Pod behind.
			name:             "draining type stops running instance",
			instType:         "draining-type",
			withInstanceType: true,
			withCQ:           true,
			stopPolicy:       kueue.HoldAndDrain,
			withPod:          true,
			wantStop:         true,
		},
		{
			// A fully-drained HoldAndDrain queue stops even a pod-less instance: the stop keys on the
			// queue's StopPolicy, not a transient Draining phase (which a fast drain skips), so a
			// drained pool never leaves an instance able to recreate a Pod it can never schedule.
			name:             "drained holdanddrain stops pod-less instance",
			instType:         "drained-type",
			withInstanceType: true,
			withCQ:           true,
			stopPolicy:       kueue.HoldAndDrain,
			wantStop:         true,
		},
		{
			// A running instance whose type was removed is stopped, not recreated.
			name:     "removed type stops running instance",
			instType: "missing-type",
			withPod:  true,
			wantStop: true,
		},
		{
			// A running instance whose type is being deleted (has a deletion
			// timestamp, still Active) is stopped the moment teardown begins.
			name:             "deleting type stops running instance",
			instType:         "deleting-type",
			withInstanceType: true,
			withCQ:           true,
			itDeleting:       true,
			withPod:          true,
			wantStop:         true,
		},
		{
			// A healthy Active type (queue None) must not stop a running instance.
			name:             "active type keeps running instance",
			instType:         "active-type",
			withInstanceType: true,
			withCQ:           true,
			withPod:          true,
			wantStop:         false,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			inst := newReadyInstance("default", "inst", c.instType)
			objs := []ctrlcli.Object{inst}
			if c.withPod {
				objs = append(objs, &core.Pod{
					ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "inst"},
				})
			}
			if c.withInstanceType {
				it := &worker.InstanceType{
					ObjectMeta: meta.ObjectMeta{Name: c.instType},
				}
				if c.itDeleting {
					now := meta.Now()
					it.DeletionTimestamp = &now
					it.Finalizers = []string{systemmeta.LockedResourceFinalizer}
				}
				objs = append(objs, it)
			}
			if c.withCQ {
				cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: c.instType}}
				if c.stopPolicy != "" {
					cq.Spec.StopPolicy = ptr.To(c.stopPolicy)
				}
				objs = append(objs, cq)
			}
			cli := buildInstanceClient(objs...)

			_, err := reconcileInstance(t, cli, "default", "inst")
			require.NoError(t, err)

			got := &workercore.Instance{}
			require.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Namespace: "default", Name: "inst"}, got))
			assert.Equal(t, c.wantStop, got.Spec.Stop, "Spec.Stop")
		})
	}
}

// TestConvertPodFromInstance_NodePin asserts how spec.nodeName reaches the backing Pod: as a
// single kubernetes.io/hostname nodeSelector entry valued from the Node's OWN hostname label —
// which some providers set to something other than the Node object's name — and never as
// pod.spec.nodeName, which would bypass the scheduler and Kueue's admission gating. An unset pin
// renders no selector at all, so an unpinned Instance's Pod is unchanged.
func TestConvertPodFromInstance_NodePin(t *testing.T) {
	cases := []struct {
		name string

		nodeName   string
		nodeLabels map[string]string
		nodeAbsent bool

		wantSelector map[string]string
	}{
		{
			name: "unset pin renders no selector",
		},
		{
			name:         "hostname label equal to the object name",
			nodeName:     "node-1",
			nodeLabels:   map[string]string{core.LabelHostname: "node-1"},
			wantSelector: map[string]string{core.LabelHostname: "node-1"},
		},
		{
			name:         "hostname label differing from the object name",
			nodeName:     "node-1",
			nodeLabels:   map[string]string{core.LabelHostname: "node-1.internal"},
			wantSelector: map[string]string{core.LabelHostname: "node-1.internal"},
		},
		{
			name:         "node without a hostname label falls back to the object name",
			nodeName:     "node-1",
			nodeLabels:   map[string]string{},
			wantSelector: map[string]string{core.LabelHostname: "node-1"},
		},
		{
			name:         "unreadable node falls back to the object name",
			nodeName:     "node-1",
			nodeAbsent:   true,
			wantSelector: map[string]string{core.LabelHostname: "node-1"},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.nodeName != "" && !c.nodeAbsent {
				objs = append(objs, &core.Node{
					ObjectMeta: meta.ObjectMeta{Name: c.nodeName, Labels: c.nodeLabels},
				})
			}
			cli := buildInstanceClient(objs...)
			r := &InstanceReconciler{Client: cli, APIReader: cli}

			inst := &workercore.Instance{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "inst"},
				Spec: workercore.InstanceSpec{
					Type:     "generic-type",
					NodeName: c.nodeName,
					InstanceTemplate: workercore.InstanceTemplate{
						Image: "img",
						Resources: &workercore.InstanceResources{
							CPU:          qty("1"),
							RAM:          qty("2Gi"),
							LocalStorage: qty("10Gi"),
						},
					},
					Volume: workercore.InstanceVolume{
						Ephemeral: &workercore.InstanceEphemeralVolume{Capacity: qty("10Gi")},
					},
				},
			}
			instType := &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "generic-type"}}

			pod := r.convertPodFromInstance(context.Background(), inst, instType)

			assert.Equal(t, c.wantSelector, pod.Spec.NodeSelector, "pod node selector")
			assert.Empty(t, pod.Spec.NodeName, "pod.spec.nodeName must never be written")
		})
	}
}

// TestConvertPodFromInstance_AdditionalVolumes asserts how spec.additionalVolumes reaches the
// backing Pod: one Pod volume and one mount per entry, mounted into the workload container only
// (the sshd sidecar sees them through the shared mount namespace), named from the entry's index so
// the name can never collide with the workspace or the sshd key volume, and rendered identically on
// a re-render so the reconcile stays idempotent. An empty list leaves the Pod as it is today.
func TestConvertPodFromInstance_AdditionalVolumes(t *testing.T) {
	cli := buildInstanceClient()
	r := &InstanceReconciler{Client: cli, APIReader: cli}

	newInstance := func(avs ...workercore.InstanceAdditionalVolume) *workercore.Instance {
		return &workercore.Instance{
			ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "inst"},
			Spec: workercore.InstanceSpec{
				Type: "generic-type",
				InstanceTemplate: workercore.InstanceTemplate{
					Image:       "img",
					VolumeMount: "/workspace",
					Resources: &workercore.InstanceResources{
						CPU:          qty("1"),
						RAM:          qty("2Gi"),
						LocalStorage: qty("10Gi"),
					},
					AdditionalVolumes: avs,
				},
				SSHPublicKey: &core.LocalObjectReference{Name: "inst-ssh-key"},
				Volume: workercore.InstanceVolume{
					Ephemeral: &workercore.InstanceEphemeralVolume{Capacity: qty("10Gi")},
				},
			},
		}
	}
	instType := &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "generic-type"}}

	t.Run("empty list leaves the pod unchanged", func(t *testing.T) {
		pod := r.convertPodFromInstance(context.Background(), newInstance(), instType)

		require.Len(t, pod.Spec.Volumes, 2, "only the sshd key and the workspace")
		assert.Equal(t, []core.VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
			pod.Spec.Containers[0].VolumeMounts, "main mounts only the workspace")
		// Both new fields unset must leave the Pod exactly as it renders today, and the selector is
		// the other half of that: this instance pins no node either.
		assert.Nil(t, pod.Spec.NodeSelector, "an unpinned instance adds no selector")
	})

	t.Run("every source renders one volume and one mount on main", func(t *testing.T) {
		inst := newInstance(
			workercore.InstanceAdditionalVolume{
				MountPath:  "/data",
				Persistent: &core.LocalObjectReference{Name: "dataset"},
			},
			workercore.InstanceAdditionalVolume{
				MountPath: "/etc/app",
				ReadOnly:  true,
				SubPath:   "conf",
				ConfigMap: &core.LocalObjectReference{Name: "app-config"},
			},
			workercore.InstanceAdditionalVolume{
				MountPath: "/var/run/creds",
				ReadOnly:  true,
				Secret:    &core.LocalObjectReference{Name: "app-creds"},
			},
			workercore.InstanceAdditionalVolume{
				MountPath: "/host/models",
				HostPath:  &core.HostPathVolumeSource{Path: "/mnt/models"},
			},
		)

		pod := r.convertPodFromInstance(context.Background(), inst, instType)

		require.Len(t, pod.Spec.Volumes, 6, "the sshd key, the workspace and the four additions")
		added := pod.Spec.Volumes[2:]
		assert.Equal(t, "dataset", added[0].PersistentVolumeClaim.ClaimName)
		assert.Equal(t, "app-config", added[1].ConfigMap.Name)
		assert.Equal(t, "app-creds", added[2].Secret.SecretName)
		assert.Equal(t, "/mnt/models", added[3].HostPath.Path)

		for i, vol := range added {
			assert.Equal(t, additionalVolumeName(i), vol.Name, "volume name is derived from the index")
			assert.NotEqual(t, "workspace", vol.Name)
			assert.NotEqual(t, "sshd-authorized-keys", vol.Name)
		}

		main, sshd := &pod.Spec.Containers[0], &pod.Spec.Containers[1]
		assert.Equal(t, []core.VolumeMount{
			{Name: "workspace", MountPath: "/workspace"},
			{Name: "additional-0", MountPath: "/data"},
			{Name: "additional-1", MountPath: "/etc/app", ReadOnly: true, SubPath: "conf"},
			{Name: "additional-2", MountPath: "/var/run/creds", ReadOnly: true},
			{Name: "additional-3", MountPath: "/host/models"},
		}, main.VolumeMounts, "main carries the workspace plus every addition")
		assert.Equal(t, []core.VolumeMount{{
			Name:      "sshd-authorized-keys",
			MountPath: "/var/run/sshd-authorized-keys",
			ReadOnly:  true,
		}}, sshd.VolumeMounts, "the sidecar gains no additional mount")

		again := r.convertPodFromInstance(context.Background(), inst, instType)
		assert.Equal(t, pod.Spec, again.Spec, "re-rendering an unchanged instance is identical")
	})

	t.Run("an entry with no source is skipped", func(t *testing.T) {
		pod := r.convertPodFromInstance(context.Background(),
			newInstance(workercore.InstanceAdditionalVolume{MountPath: "/data"}), instType)

		assert.Len(t, pod.Spec.Volumes, 2, "a sourceless entry renders no volume")
		assert.Len(t, pod.Spec.Containers[0].VolumeMounts, 1, "and no mount")
	})

	// The workspace is rendered by one of two branches — an emptyDir or a claim — and each appends
	// the additions itself, so a persistent workspace has to be asserted on its own.
	t.Run("a persistent workspace carries the additions too", func(t *testing.T) {
		inst := newInstance(workercore.InstanceAdditionalVolume{
			MountPath: "/etc/app",
			ConfigMap: &core.LocalObjectReference{Name: "app-config"},
		})
		inst.Spec.Volume = workercore.InstanceVolume{
			Persistent: &core.LocalObjectReference{Name: "inst-disk"},
		}

		pod := r.convertPodFromInstance(context.Background(), inst, instType)

		require.Len(t, pod.Spec.Volumes, 3, "the sshd key, the claimed workspace and the addition")
		assert.Equal(t, "inst-disk", pod.Spec.Volumes[1].PersistentVolumeClaim.ClaimName,
			"the workspace is still the claim")
		assert.Equal(t, "app-config", pod.Spec.Volumes[2].ConfigMap.Name)
		assert.Equal(t, []core.VolumeMount{
			{Name: "workspace", MountPath: "/workspace"},
			{Name: "additional-0", MountPath: "/etc/app"},
		}, pod.Spec.Containers[0].VolumeMounts)
	})
}

// TestConvertPodFromInstance_SlicedSSHColocatesAcceleratorOnMain asserts that an
// SSH-enabled sliced Instance renders the accelerator resource on the workload
// container (main), not the sshd sidecar, so the device-plugin injects the slicing
// artifacts (preload file, interception library, limit env) where the workload
// actually runs. The sidecar carries no accelerator resource; instead it requests
// the internal visibility resource with main's card count, so the device-plugin
// co-allocates the same physical device(s) as a narrow device-cgroup grant.
func TestConvertPodFromInstance_SlicedSSHColocatesAcceleratorOnMain(t *testing.T) {
	cli := buildInstanceClient()
	r := &InstanceReconciler{Client: cli, APIReader: cli}

	slicedCard := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)
	visRes := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)

	acc := qty("1")
	inst := &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "inst"},
		Spec: workercore.InstanceSpec{
			Type: "gpu-type",
			InstanceTemplate: workercore.InstanceTemplate{
				Image: "vllm/vllm-openai:latest",
				Resources: &workercore.InstanceResources{
					CPU:                               qty("4"),
					RAM:                               qty("16Gi"),
					LocalStorage:                      qty("32Gi"),
					Accelerator:                       &acc,
					AcceleratorSlicedMemoryPercentage: 60,
					AcceleratorSlicedCoresPercentage:  100,
				},
			},
			SSHPublicKey: &core.LocalObjectReference{Name: "inst-ssh-key"},
			Volume: workercore.InstanceVolume{
				Ephemeral: &workercore.InstanceEphemeralVolume{Capacity: qty("10Gi")},
			},
		},
	}
	instType := &worker.InstanceType{
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
		},
		Status: workercore.InstanceTypeStatus{
			Detail: slicedDetail(nodefeature.ManufacturerNVIDIA, true),
		},
	}

	pod := r.convertPodFromInstance(context.Background(), inst, instType)

	require.Len(t, pod.Spec.Containers, 2, "SSH-enabled Instance renders main + sshd")
	main, sshd := &pod.Spec.Containers[0], &pod.Spec.Containers[1]
	require.Equal(t, "main", main.Name, "main precedes sshd")
	require.Equal(t, "sshd", sshd.Name)

	_, mainHas := main.Resources.Limits[slicedCard]
	assert.True(t, mainHas, "the sliced accelerator must land on main (the workload container)")
	_, sshdHas := sshd.Resources.Limits[slicedCard]
	assert.False(t, sshdHas, "sshd must not carry the sliced accelerator resource")

	// The sidecar carries the device-only visibility resource with main's card count
	// (1 here), which the device-plugin resolves to main's device(s) on the node.
	_, mainHasVis := main.Resources.Limits[visRes]
	assert.False(t, mainHasVis, "main must not carry the sidecar visibility resource")
	visQ, sshdHasVis := sshd.Resources.Limits[visRes]
	assert.True(t, sshdHasVis, "sshd must carry the device-only visibility resource")
	assert.Equal(t, "1", visQ.String(), "sidecar visibility quantity equals main's card count")
}

// TestConvertPodFromInstance_ContainerLimitsCarryTheDeclaredResources pins the denominator the
// Instance metrics surfaces divide by. Those surfaces read the totals off the backing Pod's
// container limits, which is only the Instance's declared size for as long as the Pod declares
// nothing beyond it — the sshd sidecar is built with withGeneral=false today. A sidecar that
// starts declaring general resources fails here, instead of silently inflating every utilization
// percentage the API and /metrics report.
func TestConvertPodFromInstance_ContainerLimitsCarryTheDeclaredResources(t *testing.T) {
	cases := []struct {
		name string

		withSSH bool

		wantContainers int
	}{
		{name: "workload container alone", wantContainers: 1},
		{name: "workload container and the sshd sidecar", withSSH: true, wantContainers: 2},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := buildInstanceClient()
			r := &InstanceReconciler{Client: cli, APIReader: cli}

			inst := &workercore.Instance{
				ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "inst"},
				Spec: workercore.InstanceSpec{
					Type: "generic-type",
					InstanceTemplate: workercore.InstanceTemplate{
						Image: "img",
						Resources: &workercore.InstanceResources{
							CPU:          qty("4"),
							RAM:          qty("16Gi"),
							LocalStorage: qty("32Gi"),
						},
					},
					Volume: workercore.InstanceVolume{
						Ephemeral: &workercore.InstanceEphemeralVolume{Capacity: qty("10Gi")},
					},
				},
			}
			if c.withSSH {
				inst.Spec.SSHPublicKey = &core.LocalObjectReference{Name: "inst-ssh-key"}
			}
			instType := &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "generic-type"}}

			pod := r.convertPodFromInstance(context.Background(), inst, instType)
			require.Len(t, pod.Spec.Containers, c.wantContainers)

			sample := kubemetrics.NewSample(pod)
			assert.Equal(t, uint64(4000), sample.CPUTotalMilliCores, "spec.resources.cpu 4")
			assert.Equal(t, uint64(16384), sample.MemoryTotalMiB, "spec.resources.ram 16Gi")
			assert.Equal(t, uint64(32768), sample.StorageTotalMiB, "spec.resources.localStorage 32Gi")

			// The Pod-less path must total the same declared ceilings as the Pod-derived path
			// for the same spec.resources, with no Pod involved at all, and measure nothing.
			fromResources := kubemetrics.NewSampleFromResources(*inst.Spec.Resources)
			assert.Equal(t, sample.CPUTotalMilliCores, fromResources.CPUTotalMilliCores, "CPU total matches the Pod-derived total")
			assert.Equal(t, sample.MemoryTotalMiB, fromResources.MemoryTotalMiB, "memory total matches the Pod-derived total")
			assert.Equal(t, sample.StorageTotalMiB, fromResources.StorageTotalMiB, "storage total matches the Pod-derived total")
			assert.Nil(t, fromResources.CPUUsedMilliCores, "the Pod-less path measures nothing")
			assert.Nil(t, fromResources.MemoryUsedMiB, "the Pod-less path measures nothing")
			assert.Nil(t, fromResources.StorageUsedMiB, "the Pod-less path measures nothing")
		})
	}
}

// TestInstanceReconciler_AcceleratedDetailNotReadyRequeues pins the R3-High controller fail-safe:
// a running Instance whose accelerated InstanceType has no computed Status.Detail yet creates NO
// Pod and requeues, so a Pod never lands with a missing RuntimeClass or empty-manufacturer resource
// names. A later reconcile proceeds once Detail is populated.
func TestInstanceReconciler_AcceleratedDetailNotReadyRequeues(t *testing.T) {
	const typeName = "accel-type"
	inst := newReadyInstance("default", "inst", typeName)
	// Accelerated, but Status.Detail is empty — the reconciler has not computed it yet.
	it := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec:       workercore.InstanceTypeSpec{Acceleratable: true},
	}
	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: typeName}}
	cli := buildInstanceClient(inst, it, cq)

	res, err := reconcileInstance(t, cli, "default", "inst")
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "reconcile requeues while Detail is not ready")

	// No Pod was created while Detail is not ready.
	pod := &core.Pod{}
	err = cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "default", Name: "inst"}, pod)
	assert.True(t, kerrors.IsNotFound(err), "no Pod is created while Detail is not ready")

	// A not-ready (but healthy) type does not stop the instance.
	got := &workercore.Instance{}
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: "default", Name: "inst"}, got))
	assert.False(t, got.Spec.Stop, "a not-ready type does not stop the instance")
}

// TestInstanceReconciler_RebuildsAdmissionRejectedPod covers the admission-rejection rebuild
// loop: a backing Pod kubelet rejected with UnexpectedAdmissionError (the device-plugin's
// Allocate failing, e.g. the cross-mode FailedPrecondition) is deleted and recreated while
// the gap between the Instance's and the Pod's creation timestamps stays within the retry
// window — the growing gap doubling as backoff and deadline, with no state persisted on the
// Instance. A Pod failed for any other reason is left alone, and a gap past the window
// leaves the failed Pod in place as the visible error.
func TestInstanceReconciler_RebuildsAdmissionRejectedPod(t *testing.T) {
	const (
		ns       = "default"
		name     = "inst"
		typeName = "active-type"
	)

	now := time.Now()
	newClient := func(instAge, podAge time.Duration, podPhase core.PodPhase, podReason string) ctrlcli.Client {
		inst := newReadyInstance(ns, name, typeName)
		inst.CreationTimestamp = meta.NewTime(now.Add(-instAge))
		pod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace:         ns,
				Name:              name,
				CreationTimestamp: meta.NewTime(now.Add(-podAge)),
			},
			Status: core.PodStatus{Phase: podPhase, Reason: podReason},
		}
		it := &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: typeName}}
		cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: typeName}}
		return buildInstanceClient(inst, pod, it, cq)
	}

	t.Run("rejected pod inside the window is deleted for a backed-off rebuild", func(t *testing.T) {
		// Instance created 1m ago, this (rebuilt) Pod 30s ago: gap 30s is within 5m.
		cli := newClient(time.Minute, 30*time.Second, core.PodFailed, _PodReasonUnexpectedAdmissionError)
		res, err := reconcileInstance(t, cli, ns, name)
		require.NoError(t, err)
		assert.Equal(t, 30*time.Second, res.RequeueAfter, "the backoff is the instance-to-pod creation gap")

		err = cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: ns, Name: name}, &core.Pod{})
		assert.True(t, kerrors.IsNotFound(err), "the rejected pod is deleted so the reconcile recreates it")

		got := &workercore.Instance{}
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: ns, Name: name}, got))
		assert.Empty(t, got.Annotations, "the retry budget is stateless — nothing is written to the Instance")
	})

	t.Run("a fresh failure's backoff clamps to the floor", func(t *testing.T) {
		// Gap ~0 (first Pod created right after the Instance): backoff floors at 2s.
		cli := newClient(time.Second, 0, core.PodFailed, _PodReasonUnexpectedAdmissionError)
		res, err := reconcileInstance(t, cli, ns, name)
		require.NoError(t, err)
		assert.Equal(t, 2*time.Second, res.RequeueAfter)
	})

	t.Run("rejected pod past the window is left in place", func(t *testing.T) {
		// Instance created 10m ago, this Pod 1m ago: gap 9m exceeds the 5m window.
		cli := newClient(10*time.Minute, time.Minute, core.PodFailed, _PodReasonUnexpectedAdmissionError)
		_, err := reconcileInstance(t, cli, ns, name)
		require.NoError(t, err)

		pod := &core.Pod{}
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: ns, Name: name}, pod))
		assert.Nil(t, pod.DeletionTimestamp, "past the window the failed pod stays as the visible error")
	})

	t.Run("pod failed for another reason is left alone", func(t *testing.T) {
		cli := newClient(time.Minute, 30*time.Second, core.PodFailed, "Evicted")
		_, err := reconcileInstance(t, cli, ns, name)
		require.NoError(t, err)

		pod := &core.Pod{}
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: ns, Name: name}, pod))
		assert.Nil(t, pod.DeletionTimestamp, "a non-admission failure must not trigger a rebuild")
	})
}

// TestPartitionProfileMemoryPercent pins the boundaries of the VRAM-anchored percentage, which two
// callers now share: the Instance webhook, which defaults the values onto the object, and the
// ModelDeployment renderer, which derives them at render time.
//
// THE TRUNCATION IS PINNED RATHER THAN FIXED, and the distinction is the reason this test exists.
// The division is integer, so a profile holding 49.9% of a card sizes its host resources as 49% --
// a bounded under-size of up to one percentage point. That behavior predates this package: the
// same expression shipped in the Instance webhook, and this PR moved it into one helper so a second
// caller could not drift from it. Changing the arithmetic here would silently resize every existing
// Instance, so it is recorded as behavior and left to a change that owns that consequence.
//
// The floor and the ceiling are not decoration either: a profile smaller than 1% of a card must
// still get a positive share, and a profile the detail reports as larger than the card must not
// produce a request above the whole of it.
func TestPartitionProfileMemoryPercent(t *testing.T) {
	testCases := []struct {
		name         string
		cardMemory   string
		profileMib   int64
		profile      string
		wantPct      int64
		wantSizeable bool
		why          string
	}{
		{
			name: "not a partition request at all", cardMemory: "80Gi", profile: "",
			wantPct: 0, wantSizeable: true,
			why: "an empty profile is a whole-card request, which this helper does not size",
		},
		{
			name: "an exact half", cardMemory: "80Gi", profile: "3g.40gb", profileMib: 40 << 10,
			wantPct: 50, wantSizeable: true,
		},
		{
			name:       "a share that does not divide evenly truncates DOWN",
			cardMemory: "80Gi", profile: "odd", profileMib: 40911,
			wantPct: 49, wantSizeable: true,
			why: "40911/81920 is 49.94%, and integer division floors it to 49 rather than rounding",
		},
		{
			name:       "a share below one percent is floored to one, not to zero",
			cardMemory: "80Gi", profile: "tiny", profileMib: 100,
			wantPct: 1, wantSizeable: true,
			why: "0.12% would truncate to 0, and a replica asking for 0% of the host is unschedulable",
		},
		{
			name:       "a profile larger than its card is capped at the whole card",
			cardMemory: "40Gi", profile: "impossible", profileMib: 80 << 10,
			wantPct: 100, wantSizeable: true,
			why: "a detail that reports more per instance than per card must not size above the card",
		},
		{
			name:       "a profile the detail cannot size yet is not sizeable",
			cardMemory: "80Gi", profile: "3g.40gb", profileMib: 0,
			wantPct: 0, wantSizeable: false,
			why: "transient during detection; the caller must retry rather than fall back to a card",
		},
		{
			name:       "a card with no observed memory is not sizeable",
			cardMemory: "", profile: "3g.40gb", profileMib: 40 << 10,
			wantPct: 0, wantSizeable: false,
		},
		{
			name:       "a profile the type does not offer sizes as no partition",
			cardMemory: "80Gi", profile: "not-offered", profileMib: 40 << 10,
			wantPct: 0, wantSizeable: true,
			why: "permanent rather than transient, and each caller rejects it with its own message",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			it := &workercore.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: "h20-8x"},
				Status: workercore.InstanceTypeStatus{
					Detail: workercore.InstanceTypeDetail{
						Manufacturer: nodefeature.ManufacturerNVIDIA,
						InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
							Memory: tc.cardMemory,
						},
					},
				},
			}
			it.Status.Detail.SlicedDetail.Physical.Profiles = []workercore.AcceleratorSlicedPhysicalDetailProfile{
				{Name: "3g.40gb", MemoryMib: tc.profileMib, Count: 2},
				{Name: "odd", MemoryMib: tc.profileMib, Count: 2},
				{Name: "tiny", MemoryMib: tc.profileMib, Count: 2},
				{Name: "impossible", MemoryMib: tc.profileMib, Count: 2},
			}

			pct, sizeable := PartitionProfileMemoryPercent(it, tc.profile)
			assert.Equal(t, tc.wantSizeable, sizeable, tc.why)
			assert.Equal(t, tc.wantPct, pct, tc.why)
		})
	}
}
