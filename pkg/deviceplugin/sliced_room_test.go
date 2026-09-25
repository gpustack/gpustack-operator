package deviceplugin

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const slicedTestNode = "node-sliced"

// pct is p percent of one card's units.
func pct(p int64) int64 { return p * nodefeature.ResourceMaxUnits / 100 }

// slicedHolding is one container's logical slice of one card.
type slicedHolding struct {
	pod, container, card string
	units                int64
}

// slicedPodFixture describes one Pod of a sliced Allocate test.
type slicedPodFixture struct {
	name string
	// units is the per-card ".sliced.units" every container of the Pod requests.
	units int64
	// age orders the Pods by creation; larger is older, and whole seconds apart, which is the only
	// resolution a creation timestamp keeps.
	age        int
	phase      core.PodPhase
	containers []string
}

// slicedAllocateCase is one Allocate against a node of sliced cards, judged by what the Pods'
// annotations record afterwards.
type slicedAllocateCase struct {
	name string
	// cards are the node's card IDs; every one hosts at most count slices (default 128).
	cards []string
	count int32
	// statusRemaining overrides the Devices ledger's Remaining per card, for a ledger that lags;
	// the rest read a whole card free.
	statusRemaining map[string]int32
	pods            []slicedPodFixture
	annotated       []slicedHolding
	reserved        []slicedHolding
	gateOff         bool
	// kubelet is what kubelet reports; nil leaves the lookup off.
	kubelet kubeletPodLister
	// calls are the Allocates kubelet issues in order, one card token each.
	calls []slicedCall
}

// slicedCall is one Allocate and what it must leave behind.
type slicedCall struct {
	card string
	// kubelet, when set, replaces the case's kubelet report from this call on.
	kubelet kubeletPodLister
	// wantCode is the gRPC code the call returns.
	wantCode grpccodes.Code
	// wantHolder is the Pod whose "main" container must record the card afterwards, with wantUnits;
	// empty on a refusal, which must leave every annotation as it was.
	wantHolder string
	wantUnits  int64
}

func (c *slicedAllocateCase) devices() *workercore.Devices {
	count := c.count
	if count == 0 {
		count = 128
	}
	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: slicedTestNode},
		Spec: workercore.DevicesSpec{Groups: []workercore.DevicesGroup{{
			ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA,
		}}},
		Status: workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
			ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA,
		}}},
	}
	for i, id := range c.cards {
		devs.Spec.Groups[0].Accelerators = append(devs.Spec.Groups[0].Accelerators, workercore.Accelerator{
			ID: id, Index: uint32(i),
			Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: count}},
		})
		remaining, mode := int32(nodefeature.ResourceMaxUnits), workercore.DeviceAllocationModeNone
		if r, ok := c.statusRemaining[id]; ok {
			remaining, mode = r, workercore.DeviceAllocationModeSliced
		}
		devs.Status.Groups[0].Accelerators = append(devs.Status.Groups[0].Accelerators,
			workercore.AcceleratorAllocation{ID: id, Index: uint32(i), Mode: mode, Remaining: remaining})
	}
	return devs
}

func slicedHoldingStatus(h slicedHolding) workercore.DevicesStatus {
	return workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
		ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA,
		Accelerators: []workercore.AcceleratorAllocation{{
			ID: h.card, Mode: workercore.DeviceAllocationModeSliced, Allocated: int32(h.units),
		}},
	}}}
}

func (c *slicedAllocateCase) buildPods(t *testing.T) []*core.Pod {
	t.Helper()
	slicedRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)
	unitsRes := nodefeature.GetAcceleratableSlicedUnitsResourceName(nodefeature.ManufacturerNVIDIA)
	base := time.Now().Truncate(time.Second)

	pods := make([]*core.Pod, 0, len(c.pods))
	for _, f := range c.pods {
		ctrs := f.containers
		if len(ctrs) == 0 {
			ctrs = []string{workloadContainer}
		}
		phase := f.phase
		if phase == "" {
			phase = core.PodPending
		}
		pod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Name: f.name, Namespace: "default", UID: types.UID("uid-" + f.name),
				CreationTimestamp: meta.NewTime(base.Add(-time.Duration(f.age) * time.Second)),
			},
			Spec:   core.PodSpec{NodeName: slicedTestNode},
			Status: core.PodStatus{Phase: phase},
		}
		for _, name := range ctrs {
			pod.Spec.Containers = append(pod.Spec.Containers, core.Container{
				Name: name,
				Resources: core.ResourceRequirements{Limits: core.ResourceList{
					slicedRes: resource.MustParse("1"),
					unitsRes:  *resource.NewQuantity(f.units, resource.DecimalSI),
				}},
			})
		}
		allocations := PodAllocations{}
		for _, h := range c.annotated {
			if h.pod == f.name {
				allocations[h.container] = ContainerAllocation{
					Devices:   slicedHoldingStatus(h),
					DeviceIDs: []string{"grp-0:" + h.card + ":0000"},
				}
			}
		}
		if len(allocations) > 0 {
			pod.Annotations = allocationAnnotation(t, allocations)
		}
		pods = append(pods, pod)
	}
	return pods
}

// annotations reads every Pod's allocation record back.
func slicedAnnotations(t *testing.T, cli ctrlcli.Client, pods []*core.Pod) map[string]PodAllocations {
	t.Helper()
	out := make(map[string]PodAllocations, len(pods))
	for _, pod := range pods {
		got := new(core.Pod)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
		allocations, err := AllocatedAcceleratorsOf(got)
		require.NoError(t, err)
		out[pod.Name] = allocations
	}
	return out
}

func runSlicedAllocateCase(t *testing.T, c slicedAllocateCase) {
	t.Helper()
	pods := c.buildPods(t)
	objs := make([]ctrlcli.Object, 0, 1+len(pods))
	objs = append(objs, c.devices())
	for _, pod := range pods {
		objs = append(objs, pod)
	}
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()
	rec := &DevicesReconciler{
		NodeName: slicedTestNode, Client: cli,
		kubeletPods: c.kubelet, slicedAllocateGateOff: c.gateOff,
	}
	for _, h := range c.reserved {
		rec.reserveDevices(types.UID("uid-"+h.pod), h.container, slicedHoldingStatus(h), nil)
	}
	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeSliced,
		Reconciler:     rec,
		Responder:      stubResponder{},
	}

	for i, call := range c.calls {
		if call.kubelet != nil {
			rec.kubeletPods = call.kubelet
		}
		before := slicedAnnotations(t, cli, pods)
		_, err := s.Allocate(context.Background(), &AllocateRequest{
			ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:" + call.card + ":0000"}}},
		})
		require.Equal(t, call.wantCode, grpcstatus.Code(err), "call %d: %v", i, err)
		after := slicedAnnotations(t, cli, pods)
		if call.wantHolder == "" {
			assert.Equal(t, before, after, "call %d: a refusal records nothing", i)
			continue
		}
		held, ok := after[call.wantHolder][workloadContainer]
		require.True(t, ok, "call %d: %s records no allocation; annotations: %v", i, call.wantHolder, after)
		require.Len(t, held.Devices.Groups, 1)
		require.Len(t, held.Devices.Groups[0].Accelerators, 1)
		acc := held.Devices.Groups[0].Accelerators[0]
		assert.Equal(t, call.card, acc.ID, "call %d: %s records the card kubelet handed it", i, call.wantHolder)
		assert.Equal(t, int32(call.wantUnits), acc.Allocated, "call %d: %s records its own units", i, call.wantHolder)
	}
}

// kubeletReports returns a lister reporting the given pods, each container mapped to whether it
// already holds a device of the sliced resource.
func kubeletReports(pods map[string]map[string]bool) kubeletPodLister {
	slicedRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)
	return func(context.Context) ([]*podresources.PodResources, error) {
		out := make([]*podresources.PodResources, 0, len(pods))
		for name, ctrs := range pods {
			p := &podresources.PodResources{Name: name, Namespace: "default"}
			for ctr, held := range ctrs {
				cr := &podresources.ContainerResources{Name: ctr}
				if held {
					cr.Devices = []*podresources.ContainerDevices{{
						ResourceName: string(slicedRes), DeviceIds: []string{"grp-0:any:0000"},
					}}
				}
				p.Containers = append(p.Containers, cr)
			}
			out = append(out, p)
		}
		return out, nil
	}
}

func kubeletUnavailable(context.Context) ([]*podresources.PodResources, error) {
	return nil, errors.New("dial unix: no such file or directory")
}

// TestResourceServer_Allocate_SlicedRefusal pins the sliced Allocate refusal: a logical slice is
// allocated only where its card still has a free slot and the units it needs, judged against every
// other container's slice counted once, and is refused with FailedPrecondition otherwise. Every case
// expected to pass is one a naive refusal would wrongly fail.
func TestResourceServer_Allocate_SlicedRefusal(t *testing.T) {
	cases := []slicedAllocateCase{
		{
			// kubelet hands a bypassing Pod a card that cannot hold it; allocating anyway would
			// commit 110 % of the card's memory and clamp the ledger at zero.
			name:      "a slice its card cannot hold is refused",
			cards:     []string{"card-1"},
			pods:      []slicedPodFixture{{name: "fill", units: pct(60), age: 60, phase: core.PodRunning}, {name: "bypass", units: pct(50)}},
			annotated: []slicedHolding{{pod: "fill", container: workloadContainer, card: "card-1", units: pct(60)}},
			calls:     []slicedCall{{card: "card-1", wantCode: grpccodes.FailedPrecondition}},
		},
		{
			name:      "a slice that exactly fills its card is allocated",
			cards:     []string{"card-1"},
			pods:      []slicedPodFixture{{name: "fill", units: pct(50), age: 60, phase: core.PodRunning}, {name: "p", units: pct(50)}},
			annotated: []slicedHolding{{pod: "fill", container: workloadContainer, card: "card-1", units: pct(50)}},
			calls:     []slicedCall{{card: "card-1", wantCode: grpccodes.OK, wantHolder: "p", wantUnits: pct(50)}},
		},
		{
			// kubelet re-runs Allocate for a container whose checkpoint it lost; the container's
			// own slice, in both records, must not be counted against it.
			name:      "a retry for a container already holding the slice is allocated",
			cards:     []string{"card-1"},
			pods:      []slicedPodFixture{{name: "p", units: pct(60)}, {name: "q", units: pct(40), age: 60, phase: core.PodRunning}},
			annotated: []slicedHolding{{pod: "p", container: workloadContainer, card: "card-1", units: pct(60)}, {pod: "q", container: workloadContainer, card: "card-1", units: pct(40)}},
			reserved:  []slicedHolding{{pod: "p", container: workloadContainer, card: "card-1", units: pct(60)}},
			calls:     []slicedCall{{card: "card-1", wantCode: grpccodes.OK, wantHolder: "p", wantUnits: pct(60)}},
		},
		{
			// The reservation is gone with the process that held it; only the annotation remains.
			name:      "a retry after a device-manager restart is allocated",
			cards:     []string{"card-1"},
			pods:      []slicedPodFixture{{name: "p", units: pct(60)}, {name: "q", units: pct(40), age: 60, phase: core.PodRunning}},
			annotated: []slicedHolding{{pod: "p", container: workloadContainer, card: "card-1", units: pct(60)}, {pod: "q", container: workloadContainer, card: "card-1", units: pct(40)}},
			calls:     []slicedCall{{card: "card-1", wantCode: grpccodes.OK, wantHolder: "p", wantUnits: pct(60)}},
		},
		{
			// A neighbor whose annotation has landed while its reservation is still held is one
			// slice, not two.
			name:      "a neighbour in both records counts once",
			cards:     []string{"card-1"},
			pods:      []slicedPodFixture{{name: "q", units: pct(40), age: 60}, {name: "p", units: pct(60)}},
			annotated: []slicedHolding{{pod: "q", container: workloadContainer, card: "card-1", units: pct(40)}},
			reserved:  []slicedHolding{{pod: "q", container: workloadContainer, card: "card-1", units: pct(40)}},
			calls:     []slicedCall{{card: "card-1", wantCode: grpccodes.OK, wantHolder: "p", wantUnits: pct(60)}},
		},
		{
			// A neighbor reserved a moment ago has no annotation yet and the ledger still reads
			// the card empty; the reservation alone must be enough to refuse.
			name:     "a neighbour reserved but not yet annotated is counted",
			cards:    []string{"card-1"},
			pods:     []slicedPodFixture{{name: "q", units: pct(60), age: 60}, {name: "p", units: pct(50)}},
			reserved: []slicedHolding{{pod: "q", container: workloadContainer, card: "card-1", units: pct(60)}},
			calls:    []slicedCall{{card: "card-1", wantCode: grpccodes.FailedPrecondition}},
		},
		{
			// The ledger still charges a Pod that is already gone; the Pods are the record.
			name:            "a ledger still charging a deleted Pod does not refuse",
			cards:           []string{"card-1"},
			statusRemaining: map[string]int32{"card-1": 0},
			pods:            []slicedPodFixture{{name: "p", units: pct(60)}},
			reserved:        []slicedHolding{{pod: "gone", container: workloadContainer, card: "card-1", units: pct(100)}},
			calls:           []slicedCall{{card: "card-1", wantCode: grpccodes.OK, wantHolder: "p", wantUnits: pct(60)}},
		},
		{
			// kubelet returned a finished Pod's token, and its memory limit ended with its process.
			name:      "a finished Pod's slice does not refuse",
			cards:     []string{"card-1"},
			pods:      []slicedPodFixture{{name: "done", units: pct(60), age: 60, phase: core.PodSucceeded}, {name: "p", units: pct(60)}},
			annotated: []slicedHolding{{pod: "done", container: workloadContainer, card: "card-1", units: pct(60)}},
			calls:     []slicedCall{{card: "card-1", wantCode: grpccodes.OK, wantHolder: "p", wantUnits: pct(60)}},
		},
		{
			// Two containers of one Pod each take a slice of the same card; the second is judged
			// against the first, which it must be to fit exactly.
			name:  "a second container of the same Pod is judged against the first",
			cards: []string{"card-1"},
			pods: []slicedPodFixture{
				{name: "fill", units: pct(20), age: 60, phase: core.PodRunning},
				{name: "p", units: pct(40), containers: []string{"aux", workloadContainer}},
			},
			annotated: []slicedHolding{
				{pod: "fill", container: workloadContainer, card: "card-1", units: pct(20)},
				{pod: "p", container: "aux", card: "card-1", units: pct(40)},
			},
			reserved: []slicedHolding{{pod: "p", container: "aux", card: "card-1", units: pct(40)}},
			calls:    []slicedCall{{card: "card-1", wantCode: grpccodes.OK, wantHolder: "p", wantUnits: pct(40)}},
		},
		{
			// Hygon hosts four slices per card: four small slices fill it though most of its
			// memory is free.
			name:  "a card holding its full count of slices is refused",
			cards: []string{"card-0"},
			count: 4,
			pods: []slicedPodFixture{
				{name: "s1", units: pct(10), age: 60, phase: core.PodRunning},
				{name: "s2", units: pct(10), age: 59, phase: core.PodRunning},
				{name: "s3", units: pct(10), age: 58, phase: core.PodRunning},
				{name: "s4", units: pct(10), age: 57, phase: core.PodRunning},
				{name: "p", units: pct(30)},
			},
			annotated: []slicedHolding{
				{pod: "s1", container: workloadContainer, card: "card-0", units: pct(10)},
				{pod: "s2", container: workloadContainer, card: "card-0", units: pct(10)},
				{pod: "s3", container: workloadContainer, card: "card-0", units: pct(10)},
				{pod: "s4", container: workloadContainer, card: "card-0", units: pct(10)},
			},
			calls: []slicedCall{{card: "card-0", wantCode: grpccodes.FailedPrecondition}},
		},
		{
			name:  "a card with one slot left takes the slice",
			cards: []string{"card-0"},
			count: 4,
			pods: []slicedPodFixture{
				{name: "s1", units: pct(10), age: 60, phase: core.PodRunning},
				{name: "s2", units: pct(10), age: 59, phase: core.PodRunning},
				{name: "s3", units: pct(10), age: 58, phase: core.PodRunning},
				{name: "p", units: pct(30)},
			},
			annotated: []slicedHolding{
				{pod: "s1", container: workloadContainer, card: "card-0", units: pct(10)},
				{pod: "s2", container: workloadContainer, card: "card-0", units: pct(10)},
				{pod: "s3", container: workloadContainer, card: "card-0", units: pct(10)},
			},
			calls: []slicedCall{{card: "card-0", wantCode: grpccodes.OK, wantHolder: "p", wantUnits: pct(30)}},
		},
		{
			// The off switch restores the clamp: the slice is allocated and recorded as asked.
			name:      "with the refusal switched off an overcommit is allocated",
			cards:     []string{"card-1"},
			gateOff:   true,
			pods:      []slicedPodFixture{{name: "fill", units: pct(60), age: 60, phase: core.PodRunning}, {name: "bypass", units: pct(50)}},
			annotated: []slicedHolding{{pod: "fill", container: workloadContainer, card: "card-1", units: pct(60)}},
			calls:     []slicedCall{{card: "card-1", wantCode: grpccodes.OK, wantHolder: "bypass", wantUnits: pct(50)}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runSlicedAllocateCase(t, c)
		})
	}
}

// TestResourceServer_SlicedOccupancyOnlyForSliced pins that no other family reads the logical-slice
// occupancy, so the refusal and the room it measures leave them exactly as they were.
func TestResourceServer_SlicedOccupancyOnlyForSliced(t *testing.T) {
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).Build()
	for _, mode := range []workercore.DeviceAllocationMode{
		workercore.DeviceAllocationModeExclusive,
		workercore.DeviceAllocationModeShared,
		workercore.DeviceAllocationModePartitioned,
		workercore.DeviceAllocationModeVisibility,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			s := &ResourceServer{
				Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: mode,
				Reconciler: &DevicesReconciler{NodeName: slicedTestNode, Client: cli},
			}
			occupied, err := s.slicedOccupancyForAllocate(context.Background())
			require.NoError(t, err)
			assert.Nil(t, occupied)
		})
	}
}

// TestResourceServer_Allocate_IdentifiesByKubelet pins how an Allocate finds the container it serves
// when kubelet can be asked: the one container kubelet reports as waiting for a device wins, whatever
// the pending-Pod heuristic would have picked; and every way the lookup can come up empty falls back
// to that heuristic rather than failing.
func TestResourceServer_Allocate_IdentifiesByKubelet(t *testing.T) {
	older60, newer50 := slicedPodFixture{name: "a", units: pct(60), age: 2}, slicedPodFixture{name: "b", units: pct(50)}
	cases := []slicedAllocateCase{
		{
			// Both slices fit either card, so nothing but identity tells them apart: kubelet
			// admits b first, and the older a is not the call's.
			name:    "the container kubelet is admitting wins over an older Pod",
			cards:   []string{"card-x", "card-y"},
			pods:    []slicedPodFixture{older60, newer50},
			kubelet: kubeletReports(map[string]map[string]bool{"b": {workloadContainer: false}}),
			calls:   []slicedCall{{card: "card-x", wantCode: grpccodes.OK, wantHolder: "b", wantUnits: pct(50)}},
		},
		{
			// The shape observed on a four-card node: b is bound first and handed the free card,
			// then a is handed a card with 40 % left. Each record names its own card, and the
			// slice that does not fit is refused rather than recorded on the other Pod.
			name:  "two slices bound out of creation order land on their own cards",
			cards: []string{"card-355b", "card-ed55"},
			pods: []slicedPodFixture{
				{name: "base", units: pct(60), age: 60, phase: core.PodRunning}, older60, newer50,
			},
			annotated: []slicedHolding{{pod: "base", container: workloadContainer, card: "card-355b", units: pct(60)}},
			calls: []slicedCall{
				{
					card:     "card-ed55",
					kubelet:  kubeletReports(map[string]map[string]bool{"b": {workloadContainer: false}}),
					wantCode: grpccodes.OK, wantHolder: "b", wantUnits: pct(50),
				},
				{
					card:     "card-355b",
					kubelet:  kubeletReports(map[string]map[string]bool{"b": {workloadContainer: true}, "a": {workloadContainer: false}}),
					wantCode: grpccodes.FailedPrecondition,
				},
			},
		},
		{
			name:    "kubelet unreachable falls back to the oldest Pending Pod",
			cards:   []string{"card-x"},
			pods:    []slicedPodFixture{older60, newer50},
			kubelet: kubeletUnavailable,
			calls:   []slicedCall{{card: "card-x", wantCode: grpccodes.OK, wantHolder: "a", wantUnits: pct(60)}},
		},
		{
			// kubelet and the informer disagree; the heuristic's guess beats no answer.
			name:    "kubelet naming no candidate falls back to the oldest Pending Pod",
			cards:   []string{"card-x"},
			pods:    []slicedPodFixture{older60, newer50},
			kubelet: kubeletReports(map[string]map[string]bool{"unknown": {workloadContainer: false}}),
			calls:   []slicedCall{{card: "card-x", wantCode: grpccodes.OK, wantHolder: "a", wantUnits: pct(60)}},
		},
		{
			// The heuristic chooses among the containers kubelet names, never outside them.
			name:  "kubelet naming several candidates leaves the heuristic to choose among them",
			cards: []string{"card-x"},
			pods:  []slicedPodFixture{{name: "a", units: pct(10), age: 3}, {name: "b", units: pct(20), age: 2}, {name: "c", units: pct(30)}},
			kubelet: kubeletReports(map[string]map[string]bool{
				"b": {workloadContainer: false}, "c": {workloadContainer: false},
			}),
			calls: []slicedCall{{card: "card-x", wantCode: grpccodes.OK, wantHolder: "b", wantUnits: pct(20)}},
		},
		{
			// kubelet lost the checkpoint of a container this process already served and is
			// asking again. The heuristic would take the unserved b; kubelet says it is a.
			name:      "kubelet re-asking for a served container replays it",
			cards:     []string{"card-x"},
			pods:      []slicedPodFixture{older60, {name: "b", units: pct(30)}},
			annotated: []slicedHolding{{pod: "a", container: workloadContainer, card: "card-x", units: pct(60)}},
			reserved:  []slicedHolding{{pod: "a", container: workloadContainer, card: "card-x", units: pct(60)}},
			kubelet:   kubeletReports(map[string]map[string]bool{"a": {workloadContainer: false}}),
			calls:     []slicedCall{{card: "card-x", wantCode: grpccodes.OK, wantHolder: "a", wantUnits: pct(60)}},
		},
		{
			// The API leaves non-restartable init containers out of its list, so a container of a
			// Pod kubelet holds but does not report cannot be ruled out.
			name:    "a container kubelet does not report counts as waiting",
			cards:   []string{"card-x"},
			pods:    []slicedPodFixture{older60, newer50},
			kubelet: kubeletReports(map[string]map[string]bool{"b": {}}),
			calls:   []slicedCall{{card: "card-x", wantCode: grpccodes.OK, wantHolder: "b", wantUnits: pct(50)}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runSlicedAllocateCase(t, c)
		})
	}
}

// TestDevicesReconciler_KubeletPending pins what counts as holding a device: only a device of the
// resource being allocated, with at least one ID. Anything else leaves the container waiting.
func TestDevicesReconciler_KubeletPending(t *testing.T) {
	const resName = "nvidia.com/gpu.sliced"
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default"}}
	cases := []struct {
		name        string
		devices     []*podresources.ContainerDevices
		wantWaiting bool
	}{
		{name: "no device", wantWaiting: true},
		{name: "a device of another resource", devices: []*podresources.ContainerDevices{{ResourceName: "rdma/hca", DeviceIds: []string{"0"}}}, wantWaiting: true},
		{name: "an entry with no device ID", devices: []*podresources.ContainerDevices{{ResourceName: resName}}, wantWaiting: true},
		{name: "a device of the resource", devices: []*podresources.ContainerDevices{{ResourceName: resName, DeviceIds: []string{"grp-0:c:0000"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &DevicesReconciler{kubeletPods: func(context.Context) ([]*podresources.PodResources, error) {
				return []*podresources.PodResources{{
					Name: "p", Namespace: "default",
					Containers: []*podresources.ContainerResources{{Name: workloadContainer, Devices: c.devices}},
				}}, nil
			}}
			pending := rec.kubeletPending(context.Background(), resName)
			require.NotNil(t, pending)
			assert.Equal(t, c.wantWaiting, pending.waitsForDevice(pod, &core.Container{Name: workloadContainer}))
		})
	}

	t.Run("a Pod kubelet does not hold is not waiting", func(t *testing.T) {
		rec := &DevicesReconciler{kubeletPods: kubeletReports(map[string]map[string]bool{"other": {}})}
		assert.False(t, rec.kubeletPending(context.Background(), resName).waitsForDevice(pod, &core.Container{Name: workloadContainer}))
	})
	t.Run("the lookup switched off answers nothing", func(t *testing.T) {
		assert.Nil(t, (&DevicesReconciler{}).kubeletPending(context.Background(), resName))
	})
	t.Run("kubelet unreachable answers nothing", func(t *testing.T) {
		assert.Nil(t, (&DevicesReconciler{kubeletPods: kubeletUnavailable}).kubeletPending(context.Background(), resName))
	})
}

// TestResourceServer_PreferredAllocation_SlicedReadsTheRoom pins that the hint reads the same room
// the Allocate refusal does, for the container kubelet is admitting: a hint drawn from the ledger
// alone, or for another Pod, steers kubelet onto a card the Allocate that follows refuses while a
// card with room was free.
func TestResourceServer_PreferredAllocation_SlicedReadsTheRoom(t *testing.T) {
	cases := []struct {
		name      string
		c         slicedAllocateCase
		wantCards []string
	}{
		{
			// b fits the fuller card-1 and packs there; the older a would not and would be
			// hinted the empty card-2.
			name: "the hint is for the container kubelet is admitting",
			c: slicedAllocateCase{
				cards:     []string{"card-1", "card-2"},
				pods:      []slicedPodFixture{{name: "fill", units: pct(45), age: 60, phase: core.PodRunning}, {name: "a", units: pct(60), age: 2}, {name: "b", units: pct(50)}},
				annotated: []slicedHolding{{pod: "fill", container: workloadContainer, card: "card-1", units: pct(45)}},
				kubelet:   kubeletReports(map[string]map[string]bool{"b": {workloadContainer: false}}),
			},
			wantCards: []string{"card-1"},
		},
		{
			// q was reserved on card-1 a moment ago and the ledger still reads it empty.
			name: "the hint counts a neighbour reserved but not yet in the ledger",
			c: slicedAllocateCase{
				cards:    []string{"card-1", "card-2"},
				pods:     []slicedPodFixture{{name: "q", units: pct(60), age: 60, phase: core.PodRunning}, {name: "p", units: pct(50)}},
				reserved: []slicedHolding{{pod: "q", container: workloadContainer, card: "card-1", units: pct(60)}},
			},
			wantCards: []string{"card-2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pods := tc.c.buildPods(t)
			objs := []ctrlcli.Object{tc.c.devices()}
			for _, pod := range pods {
				objs = append(objs, pod)
			}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
				WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
					return []string{obj.(*core.Pod).Spec.NodeName}
				}).Build()
			rec := &DevicesReconciler{NodeName: slicedTestNode, Client: cli, kubeletPods: tc.c.kubelet}
			for _, h := range tc.c.reserved {
				rec.reserveDevices(types.UID("uid-"+h.pod), h.container, slicedHoldingStatus(h), nil)
			}
			s := &ResourceServer{
				Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: workercore.DeviceAllocationModeSliced,
				Reconciler: rec, Responder: stubResponder{},
			}
			var available []string
			for _, card := range tc.c.cards {
				available = append(available, "grp-0:"+card+":0000")
			}
			resp, err := s.GetPreferredAllocation(context.Background(), &PreferredAllocationRequest{
				ContainerRequests: []*ContainerPreferredAllocationRequest{{AvailableDeviceIDs: available, AllocationSize: 1}},
			})
			require.NoError(t, err)
			var gotCards []string
			for _, id := range resp.GetContainerResponses()[0].GetDeviceIDs() {
				token, err := ParseResourceToken(id)
				require.NoError(t, err)
				gotCards = append(gotCards, token.Device)
			}
			assert.Equal(t, tc.wantCards, gotCards)
		})
	}
}

// TestBuildDesiredStatus_CountsSlicesPerCard pins the per-card slice count the ledger publishes:
// one per container holding a logical slice of the card, from the same annotations the units come
// from, and none for any other mode.
func TestBuildDesiredStatus_CountsSlicesPerCard(t *testing.T) {
	c := slicedAllocateCase{
		cards: []string{"card-0", "card-1", "card-2"},
		pods: []slicedPodFixture{
			{name: "p", units: pct(10), containers: []string{"aux", workloadContainer}},
			{name: "q", units: pct(10)},
		},
		annotated: []slicedHolding{
			{pod: "p", container: "aux", card: "card-0", units: pct(10)},
			{pod: "p", container: workloadContainer, card: "card-0", units: pct(10)},
			{pod: "q", container: workloadContainer, card: "card-1", units: pct(10)},
		},
	}
	devs := c.devices()
	podList := &core.PodList{}
	for _, pod := range c.buildPods(t) {
		podList.Items = append(podList.Items, *pod)
	}
	exclusive := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "x", Namespace: "default", UID: "uid-x",
			Annotations: allocationAnnotation(t, PodAllocations{workloadContainer: {Devices: workercore.DevicesStatus{
				Groups: []workercore.DevicesAllocationGroup{{
					ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA,
					Accelerators: []workercore.AcceleratorAllocation{{ID: "card-2", Mode: workercore.DeviceAllocationModeExclusive, Allocated: nodefeature.ResourceMaxUnits}},
				}},
			}}}),
		},
		Spec: core.PodSpec{NodeName: slicedTestNode},
	}
	podList.Items = append(podList.Items, *exclusive)

	status, _ := BuildDesiredStatus(logr.Discard(), devs, podList)

	got := map[string]int32{}
	for _, acc := range status.Groups[0].Accelerators {
		got[acc.ID] = acc.AllocatedSlices
	}
	assert.Equal(t, map[string]int32{"card-0": 2, "card-1": 1, "card-2": 0}, got)
}

// TestResourceServer_PreferredAllocation_SlicedOccupancyUnreadable pins that a hint whose occupancy
// cannot be read falls back to the ledger instead of failing: kubelet fails a Pod's admission on a
// hint error, and the Allocate that follows judges the outcome either way.
func TestResourceServer_PreferredAllocation_SlicedOccupancyUnreadable(t *testing.T) {
	c := slicedAllocateCase{cards: []string{"card-1"}, pods: []slicedPodFixture{{name: "p", units: pct(50)}}}
	pods := c.buildPods(t)
	var podLists atomic.Int32
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(c.devices(), pods[0]).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			List: func(ctx context.Context, cli ctrlcli.WithWatch, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
				// The first Pod list identifies the container; the second is the occupancy read.
				if _, ok := list.(*core.PodList); ok && podLists.Add(1) == 2 {
					return errors.New("informer unavailable")
				}
				return cli.List(ctx, list, opts...)
			},
		}).Build()
	s := &ResourceServer{
		Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: workercore.DeviceAllocationModeSliced,
		Reconciler: &DevicesReconciler{NodeName: slicedTestNode, Client: cli}, Responder: stubResponder{},
	}

	resp, err := s.GetPreferredAllocation(context.Background(), &PreferredAllocationRequest{
		ContainerRequests: []*ContainerPreferredAllocationRequest{{AvailableDeviceIDs: []string{"grp-0:card-1:0000"}, AllocationSize: 1}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), podLists.Load(), "the occupancy read was attempted and failed")
	assert.Equal(t, []string{"grp-0:card-1:0000"}, resp.GetContainerResponses()[0].GetDeviceIDs())
}

// TestNarrowToKubeletPending_Logs pins when narrowing says it fell back or chose among several: only
// when kubelet's answer disagrees with a candidate set the informer has. An empty set is the informer
// not having delivered the Pod yet, which the caller's retry covers, so it is not reported.
func TestNarrowToKubeletPending_Logs(t *testing.T) {
	candidate := func(name string) _AllocatingCandidate {
		return _AllocatingCandidate{
			pod: &core.Pod{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: "default"}},
			ctr: &core.Container{Name: workloadContainer},
		}
	}
	waiting := func(names ...string) *_KubeletPending {
		pods := make(map[types.NamespacedName]map[string]bool, len(names))
		for _, n := range names {
			pods[types.NamespacedName{Namespace: "default", Name: n}] = map[string]bool{workloadContainer: false}
		}
		return &_KubeletPending{pods: pods}
	}
	cases := []struct {
		name       string
		feasible   []_AllocatingCandidate
		kubelet    *_KubeletPending
		wantLeft   int
		wantLogged string
	}{
		{name: "no candidate yet", kubelet: waiting("a"), wantLeft: 0},
		{name: "kubelet names one candidate", feasible: []_AllocatingCandidate{candidate("a"), candidate("b")}, kubelet: waiting("b"), wantLeft: 1},
		{name: "kubelet names none of the candidates", feasible: []_AllocatingCandidate{candidate("a")}, kubelet: waiting("x"), wantLeft: 1, wantLogged: "kubelet reports no pending candidate"},
		{name: "kubelet names several candidates", feasible: []_AllocatingCandidate{candidate("a"), candidate("b")}, kubelet: waiting("a", "b"), wantLeft: 2, wantLogged: "kubelet reports several candidates"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logged []string
			logger := funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{})
			match := _AllocationMatch{ResourceName: "nvidia.com/gpu.sliced", Kubelet: c.kubelet}

			feasible, infeasible, claimed := narrowToKubeletPending(logger, match, c.feasible, nil, nil)

			assert.Len(t, feasible, c.wantLeft)
			assert.Empty(t, infeasible)
			assert.Empty(t, claimed)
			if c.wantLogged == "" {
				assert.Empty(t, logged)
				return
			}
			require.Len(t, logged, 1)
			assert.Contains(t, logged[0], c.wantLogged)
		})
	}
}
