package deviceplugin

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// terminationCase is one Pod state the ledger is rebuilt against.
type terminationCase struct {
	name string
	// manufacturer and mode are those of the one accelerator the Pod's allocation records.
	manufacturer string
	mode         workercore.DeviceAllocationMode
	// physical records a partition on the accelerator, as a partition allocation does.
	physical bool
	phase    core.PodPhase
	deleting bool
	// switchOff sets the release switch to "false".
	switchOff bool
	wantHeld  bool
}

// terminationCases covers every phase a Pod can be read in, for each family, the partitions and the
// stateful slices the release must leave alone, and the switch that turns it off.
var terminationCases = []terminationCase{
	{name: "succeeded sliced", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, phase: core.PodSucceeded},
	{name: "failed sliced", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, phase: core.PodFailed},
	{name: "succeeded exclusive", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeExclusive, phase: core.PodSucceeded},
	{name: "failed shared", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeShared, phase: core.PodFailed},
	{name: "running sliced", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, phase: core.PodRunning, wantHeld: true},
	{name: "pending sliced", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, phase: core.PodPending, wantHeld: true},
	{name: "unknown phase sliced", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, phase: core.PodUnknown, wantHeld: true},
	{name: "terminating running sliced", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, phase: core.PodRunning, deleting: true, wantHeld: true},
	{name: "terminating succeeded sliced", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, phase: core.PodSucceeded, deleting: true},
	{name: "succeeded partitioned", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModePartitioned, physical: true, phase: core.PodSucceeded, wantHeld: true},
	{name: "failed partitioned", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModePartitioned, physical: true, phase: core.PodFailed, wantHeld: true},
	{name: "succeeded partition recorded under sliced", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, physical: true, phase: core.PodSucceeded, wantHeld: true},
	{name: "succeeded metax sliced", manufacturer: nodefeature.ManufacturerMetaX, mode: workercore.DeviceAllocationModeSliced, phase: core.PodSucceeded, wantHeld: true},
	{name: "failed cambricon sliced", manufacturer: nodefeature.ManufacturerCambricon, mode: workercore.DeviceAllocationModeSliced, phase: core.PodFailed, wantHeld: true},
	{name: "succeeded metax exclusive", manufacturer: nodefeature.ManufacturerMetaX, mode: workercore.DeviceAllocationModeExclusive, phase: core.PodSucceeded},
	{name: "succeeded sliced with the switch off", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, phase: core.PodSucceeded, switchOff: true, wantHeld: true},
	{name: "running sliced with the switch off", manufacturer: nodefeature.ManufacturerNVIDIA, mode: workercore.DeviceAllocationModeSliced, phase: core.PodRunning, switchOff: true, wantHeld: true},
}

const terminationDevice = "dev-0"

// terminationAllocation is the allocation the case's Pod records on its one accelerator: a quarter of
// it, with a logical window, or with a partition when the case records one.
func terminationAllocation(c terminationCase) workercore.DevicesStatus {
	acc := workercore.AcceleratorAllocation{
		ID:                         terminationDevice,
		Mode:                       c.mode,
		Allocated:                  nodefeature.ResourceMaxUnits / 4,
		AllocatedLogicalPlacements: []workercore.AcceleratorPlacement{pl(0, 4)},
	}
	if c.physical {
		acc.AllocatedLogicalPlacements = nil
		acc.AllocatedPhysicalProfile = "3g.20gb"
		acc.AllocatedPhysicalPlacements = []workercore.AcceleratorPlacement{pl(0, 4)}
	}
	return workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
		ID:           "grp-0",
		Manufacturer: c.manufacturer,
		Accelerators: []workercore.AcceleratorAllocation{acc},
	}}}
}

func terminationPod(t *testing.T, c terminationCase, node string) *core.Pod {
	t.Helper()
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "p", Namespace: "default", UID: types.UID("uid-p"),
			Annotations: allocationAnnotation(t, PodAllocations{"main": {Devices: terminationAllocation(c)}}),
		},
		Spec:   core.PodSpec{NodeName: node},
		Status: core.PodStatus{Phase: c.phase},
	}
	if c.deleting {
		deleting := meta.NewTime(time.Unix(1, 0))
		pod.DeletionTimestamp = &deleting
		pod.Finalizers = []string{"gpustack.ai/test-hold"} // the fake client rejects a deleted object without one
	}
	return pod
}

func terminationDevices(c terminationCase, node string) *workercore.Devices {
	acc := workercore.Accelerator{
		ID:     terminationDevice,
		Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 16}},
	}
	if c.physical {
		acc.Status = workercore.AcceleratorStatus{PhysicalSliced: workercore.AcceleratorPhysicalSliced{
			Profiles: a100Placements(),
			Count:    7,
		}}
	}
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: node},
		Spec: workercore.DevicesSpec{Groups: []workercore.DevicesGroup{{
			ID:           "grp-0",
			Manufacturer: c.manufacturer,
			Accelerators: []workercore.Accelerator{acc},
		}}},
	}
}

// setReleaseSwitch sets the release switch for one test, as the environment would at startup.
func setReleaseSwitch(t *testing.T, off bool) {
	t.Helper()
	previous := terminatedPodsReleased
	terminatedPodsReleased = func() bool { return !off }
	t.Cleanup(func() { terminatedPodsReleased = previous })
}

// TestBuildDesiredStatus_TerminatedPods verifies the rebuilt ledger stops charging what a Pod in a
// terminal phase no longer holds, and only that. kubelet returns a finished Pod's devices, so a
// logical allocation is free once the phase is Succeeded or Failed; a partition and a stateful slice
// are destroyed only once the Pod object is gone, so they stay charged until then. The Pod stays in
// the live set either way, because that set drives the reclaimers that do the destroying.
func TestBuildDesiredStatus_TerminatedPods(t *testing.T) {
	const node = "node-term"
	for _, c := range terminationCases {
		t.Run(c.name, func(t *testing.T) {
			setReleaseSwitch(t, c.switchOff)
			pod := terminationPod(t, c, node)

			status, live := BuildDesiredStatus(logr.Discard(), terminationDevices(c, node), &core.PodList{Items: []core.Pod{*pod}})

			acc := acceleratorByID(t, &status, terminationDevice)
			if c.physical {
				wantProfiles := []workercore.AcceleratorProfileCount(nil)
				if c.wantHeld {
					wantProfiles = []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}}
				}
				assert.Equal(t, wantProfiles, acc.AllocatedProfiles)
			}
			if c.wantHeld {
				assert.Equal(t, c.mode, acc.Mode)
				assert.Equal(t, int32(nodefeature.ResourceMaxUnits-nodefeature.ResourceMaxUnits/4), acc.Remaining)
			} else {
				assert.Equal(t, workercore.DeviceAllocationModeNone, acc.Mode)
				assert.Equal(t, int32(nodefeature.ResourceMaxUnits), acc.Remaining)
			}
			assert.Equal(t, []string{string(pod.UID)}, live, "the live set keeps every Pod object")
		})
	}
}

// TestLiveLogicalOccupied_FollowsTheLedger verifies the window a logical placement is chosen against
// frees exactly when the ledger does. A window held after the ledger freed its units would let a
// Workload be admitted onto a card whose Allocate then finds no window for it.
func TestLiveLogicalOccupied_FollowsTheLedger(t *testing.T) {
	const node = "node-window"
	for _, c := range terminationCases {
		if c.physical {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			setReleaseSwitch(t, c.switchOff)
			pod := terminationPod(t, c, node)
			devs := terminationDevices(c, node)
			rec := &DevicesReconciler{NodeName: node, Client: nodeFixture(pod)}

			occupied, err := rec.LiveLogicalOccupied(context.Background())
			require.NoError(t, err)
			status, _ := BuildDesiredStatus(logr.Discard(), devs, &core.PodList{Items: []core.Pod{*pod}})

			windowHeld := len(occupied[Resource{Group: "grp-0", Device: terminationDevice}]) > 0
			ledgerHeld := acceleratorByID(t, &status, terminationDevice).Remaining < nodefeature.ResourceMaxUnits
			assert.Equal(t, c.wantHeld, windowHeld, "window")
			assert.Equal(t, ledgerHeld, windowHeld, "the window and the ledger must agree")
		})
	}
}

// TestSlicedOccupancy_FollowsTheLedger verifies the sliced Allocate gate reads a finished Pod's slice
// the way the ledger does. A gate that freed a MetaX or Cambricon slice the ledger still charges
// would let an Allocate through onto a slice the driver still holds; one that kept a slice the ledger
// freed would refuse a Pod the node-devices check admitted.
func TestSlicedOccupancy_FollowsTheLedger(t *testing.T) {
	const node = "node-gate"
	for _, c := range terminationCases {
		if c.physical || c.mode != workercore.DeviceAllocationModeSliced {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			setReleaseSwitch(t, c.switchOff)
			pod := terminationPod(t, c, node)
			s := &ResourceServer{
				Manufacturer:   c.manufacturer,
				AllocationMode: workercore.DeviceAllocationModeSliced,
				Reconciler:     &DevicesReconciler{NodeName: node, Client: nodeFixture(pod)},
			}

			occupied, err := s.slicedOccupancy(context.Background())
			require.NoError(t, err)
			status, _ := BuildDesiredStatus(logr.Discard(), terminationDevices(c, node), &core.PodList{Items: []core.Pod{*pod}})

			units, _ := occupied.heldExcept(Resource{Group: "grp-0", Device: terminationDevice}, _ReservationKey{PodUID: "someone-else"})
			ledgerHeld := acceleratorByID(t, &status, terminationDevice).Remaining < nodefeature.ResourceMaxUnits
			assert.Equal(t, c.wantHeld, units > 0, "gate")
			assert.Equal(t, ledgerHeld, units > 0, "the gate and the ledger must agree")
		})
	}
}

// TestPodUpdateChangesLedger verifies a Pod reaching a terminal phase brings the ledger rebuild back.
// The phase is the only thing that changes when a Pod finishes, so a watch that fires only on the
// allocation record or the deletion would leave the published ledger charging it until its object
// is gone.
func TestPodUpdateChangesLedger(t *testing.T) {
	allocated := map[string]string{AllocatedAcceleratorAnnoKey: "{}"}
	pod := func(annotations map[string]string, phase core.PodPhase, deleting bool) *core.Pod {
		p := &core.Pod{ObjectMeta: meta.ObjectMeta{Annotations: annotations}, Status: core.PodStatus{Phase: phase}}
		if deleting {
			ts := meta.NewTime(time.Unix(1, 0))
			p.DeletionTimestamp = &ts
		}
		return p
	}
	cases := []struct {
		name     string
		old, new *core.Pod
		want     bool
	}{
		{name: "running to succeeded", old: pod(allocated, core.PodRunning, false), new: pod(allocated, core.PodSucceeded, false), want: true},
		{name: "running to failed", old: pod(allocated, core.PodRunning, false), new: pod(allocated, core.PodFailed, false), want: true},
		{name: "pending to running", old: pod(allocated, core.PodPending, false), new: pod(allocated, core.PodRunning, false)},
		{name: "terminal phase without a record", old: pod(nil, core.PodRunning, false), new: pod(nil, core.PodSucceeded, false)},
		{name: "record lands", old: pod(nil, core.PodPending, false), new: pod(allocated, core.PodPending, false), want: true},
		{name: "deletion starts", old: pod(allocated, core.PodRunning, false), new: pod(allocated, core.PodRunning, true), want: true},
		{name: "nothing the ledger reads", old: pod(allocated, core.PodRunning, false), new: pod(allocated, core.PodRunning, false)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, podUpdateChangesLedger(ctrlevent.UpdateEvent{ObjectOld: c.old, ObjectNew: c.new}))
		})
	}
}

// TestStatefulSlicedManufacturers_MatchTheReclaimLoops verifies statefulSlicedManufacturers names
// exactly the manufacturers whose allocator starts a reclaim loop for logical slices. Such a slice is
// destroyed only once its Pod object is gone, so a manufacturer missing from the set would have its
// finished Pods' slices released from the ledger while the driver still holds them, and every Pod
// placed onto them would fail its Allocate.
func TestStatefulSlicedManufacturers_MatchTheReclaimLoops(t *testing.T) {
	consts := packageStringConsts(t, filepath.Join("..", "nodefeature"))
	matches, err := filepath.Glob(filepath.Join("..", "devicemanager", "allocator", "*"))
	require.NoError(t, err)

	got := sets.New[string]()
	loops := 0
	for _, dir := range matches {
		manufacturer := ""
		var modes []string
		for _, file := range parseGoFiles(t, dir) {
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.ValueSpec:
					for i, id := range n.Names {
						if id.Name != "Manufacturer" || i >= len(n.Values) {
							continue
						}
						sel, ok := n.Values[i].(*ast.SelectorExpr)
						require.True(t, ok, "%s: Manufacturer is not a nodefeature constant", dir)
						manufacturer, ok = consts[sel.Sel.Name]
						require.True(t, ok, "%s: nodefeature.%s is not a string constant", dir, sel.Sel.Name)
					}
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "RunReclaimLoop" {
						return true
					}
					require.Len(t, n.Args, 5, "%s: RunReclaimLoop changed its signature", dir)
					manu, ok := n.Args[2].(*ast.Ident)
					require.True(t, ok && manu.Name == "Manufacturer",
						"%s: RunReclaimLoop is not called with the package's Manufacturer", dir)
					mode, ok := n.Args[3].(*ast.SelectorExpr)
					require.True(t, ok, "%s: RunReclaimLoop is not called with a named allocation mode", dir)
					modes = append(modes, mode.Sel.Name)
				}
				return true
			})
		}
		for _, mode := range modes {
			loops++
			if mode == "DeviceAllocationModeSliced" {
				require.NotEmpty(t, manufacturer, "%s: no Manufacturer constant", dir)
				got.Insert(manufacturer)
			}
		}
	}
	require.NotZero(t, loops, "no reclaim loop was found, so the scan read nothing")
	assert.Equal(t, sets.List(got), sets.List(statefulSlicedManufacturers))
}

// packageStringConsts returns every string constant a package declares, by name.
func packageStringConsts(t *testing.T, dir string) map[string]string {
	t.Helper()
	consts := make(map[string]string)
	for _, file := range parseGoFiles(t, dir) {
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, id := range spec.Names {
				if i >= len(spec.Values) {
					continue
				}
				if lit, ok := spec.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						consts[id.Name] = v
					}
				}
			}
			return true
		})
	}
	require.NotEmpty(t, consts, "%s declares no string constant, so the scan read nothing", dir)
	return consts
}

// parseGoFiles parses every non-test Go file of a directory, whatever its build constraints.
func parseGoFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		files = append(files, file)
	}
	return files
}
