package deviceplugin

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestCanonicalSlotContainerName(t *testing.T) {
	gpuRes := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive,
	)
	cpuOnly := func(name string) core.Container {
		return core.Container{
			Name: name,
			Resources: core.ResourceRequirements{
				Limits: core.ResourceList{core.ResourceCPU: resource.MustParse("1")},
			},
		}
	}
	gpuInit := func(name string) core.Container {
		return core.Container{
			Name: name,
			Resources: core.ResourceRequirements{
				Limits: core.ResourceList{gpuRes: resource.MustParse("1")},
			},
		}
	}
	gpuRequestsOnlyInit := func(name string) core.Container {
		return core.Container{
			Name: name,
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{gpuRes: resource.MustParse("1")},
			},
		}
	}
	sidecarGPUInit := func(name string) core.Container {
		c := gpuInit(name)
		c.RestartPolicy = ptr.To(core.ContainerRestartPolicyAlways)
		return c
	}

	cases := []struct {
		name string
		pod  *core.Pod
		ctr  *core.Container
		want string
	}{
		{
			name: "nil container returns empty string",
			pod:  &core.Pod{},
			ctr:  nil,
			want: "",
		},
		{
			name: "nil pod returns container name",
			pod:  nil,
			ctr:  &core.Container{Name: "forge-verifier"},
			want: "forge-verifier",
		},
		{
			name: "sequential non-sidecar GPU init containers resolve to first GPU init",
			pod: &core.Pod{Spec: core.PodSpec{
				InitContainers: []core.Container{gpuInit("forge-runner"), gpuInit("forge-verifier")},
				Containers:     []core.Container{cpuOnly("pause")},
			}},
			ctr: func() *core.Container {
				c := gpuInit("forge-verifier")
				return &c
			}(),
			want: "forge-runner",
		},
		{
			name: "mixed CPU init before sequential GPU init containers skips CPU init",
			pod: &core.Pod{Spec: core.PodSpec{
				InitContainers: []core.Container{
					cpuOnly("stage-sentinel"),
					gpuInit("forge-runner"),
					gpuInit("forge-verifier"),
				},
			}},
			ctr: func() *core.Container {
				c := gpuInit("forge-verifier")
				return &c
			}(),
			want: "forge-runner",
		},
		{
			name: "CPU-only init container keeps its own name even after a GPU init",
			pod: &core.Pod{Spec: core.PodSpec{
				InitContainers: []core.Container{
					gpuInit("forge-runner"),
					cpuOnly("post-check"),
				},
			}},
			ctr: func() *core.Container {
				c := cpuOnly("post-check")
				return &c
			}(),
			want: "post-check",
		},
		{
			name: "sidecar GPU init container keeps its own name and is skipped by non-sidecar inits",
			pod: &core.Pod{Spec: core.PodSpec{
				InitContainers: []core.Container{
					sidecarGPUInit("gpu-sidecar"),
					gpuInit("forge-runner"),
					gpuInit("forge-verifier"),
				},
			}},
			ctr: func() *core.Container {
				c := gpuInit("forge-verifier")
				return &c
			}(),
			want: "forge-runner",
		},
		{
			name: "sidecar GPU init container itself keeps its own name",
			pod: &core.Pod{Spec: core.PodSpec{
				InitContainers: []core.Container{
					gpuInit("forge-runner"),
					sidecarGPUInit("gpu-sidecar"),
				},
			}},
			ctr: func() *core.Container {
				c := sidecarGPUInit("gpu-sidecar")
				return &c
			}(),
			want: "gpu-sidecar",
		},
		{
			name: "regular workload container keeps its own name",
			pod: &core.Pod{Spec: core.PodSpec{
				InitContainers: []core.Container{gpuInit("forge-runner")},
				Containers:     []core.Container{gpuInit("main")},
			}},
			ctr: func() *core.Container {
				c := gpuInit("main")
				return &c
			}(),
			want: "main",
		},
		{
			name: "requests-only GPU init container is recognized as accelerator-requesting",
			pod: &core.Pod{Spec: core.PodSpec{
				InitContainers: []core.Container{
					gpuRequestsOnlyInit("forge-runner"),
					gpuRequestsOnlyInit("forge-verifier"),
				},
			}},
			ctr: func() *core.Container {
				c := gpuRequestsOnlyInit("forge-verifier")
				return &c
			}(),
			want: "forge-runner",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, CanonicalSlotContainerName(c.pod, c.ctr))
		})
	}
}

func TestPodAllocations_Aggregate_OverlappingDeviceIDs(t *testing.T) {
	const nodeName = "node-agg"
	alloc := func(dev string, mode workercore.DeviceAllocationMode, units int32) workercore.DevicesStatus {
		return workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
			ID:           "grp-0",
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{{
				ID:        dev,
				Mode:      mode,
				Allocated: units,
			}},
		}}}
	}
	partAlloc := func(dev, profile string, placements ...workercore.AcceleratorPlacement) workercore.DevicesStatus {
		return workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
			ID:           "grp-0",
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{{
				ID:                          dev,
				Mode:                        workercore.DeviceAllocationModePartitioned,
				Allocated:                   nodefeature.ResourceMaxUnits / 8,
				AllocatedPhysicalProfile:    profile,
				AllocatedPhysicalPlacements: placements,
			}},
		}}}
	}

	cases := []struct {
		name            string
		allocations     PodAllocations
		devID           string
		wantEntries     int
		wantRemaining   int32
		wantSlices      int32
		wantProfileCnts []workercore.AcceleratorProfileCount
	}{
		{
			name: "exclusive overlapping DeviceIDs deduplicates to single whole-card charge",
			allocations: PodAllocations{
				"forge-runner":   {Devices: alloc("logical-0", workercore.DeviceAllocationModeExclusive, nodefeature.ResourceMaxUnits), DeviceIDs: []string{"grp-0:logical-0:0000"}},
				"forge-verifier": {Devices: alloc("logical-0", workercore.DeviceAllocationModeExclusive, nodefeature.ResourceMaxUnits), DeviceIDs: []string{"grp-0:logical-0:0000"}},
			},
			devID:         "logical-0",
			wantEntries:   1,
			wantRemaining: 0,
		},
		{
			name: "shared overlapping DeviceIDs deduplicates to one share",
			allocations: PodAllocations{
				"forge-runner":   {Devices: alloc("logical-0", workercore.DeviceAllocationModeShared, 200000), DeviceIDs: []string{"grp-0:logical-0:0000"}},
				"forge-verifier": {Devices: alloc("logical-0", workercore.DeviceAllocationModeShared, 200000), DeviceIDs: []string{"grp-0:logical-0:0000"}},
			},
			devID:         "logical-0",
			wantEntries:   1,
			wantRemaining: nodefeature.ResourceMaxUnits - 200000,
		},
		{
			name: "shared disjoint DeviceIDs accumulates both concurrent containers",
			allocations: PodAllocations{
				"c0": {Devices: alloc("logical-0", workercore.DeviceAllocationModeShared, 200000), DeviceIDs: []string{"grp-0:logical-0:0000"}},
				"c1": {Devices: alloc("logical-0", workercore.DeviceAllocationModeShared, 200000), DeviceIDs: []string{"grp-0:logical-0:160000"}},
			},
			devID:         "logical-0",
			wantEntries:   2,
			wantRemaining: nodefeature.ResourceMaxUnits - 400000,
		},
		{
			name: "sliced overlapping DeviceIDs counts one slice and one unit charge",
			allocations: PodAllocations{
				"forge-runner":   {Devices: alloc("logical-0", workercore.DeviceAllocationModeSliced, 600000), DeviceIDs: []string{"grp-0:logical-0:0000"}},
				"forge-verifier": {Devices: alloc("logical-0", workercore.DeviceAllocationModeSliced, 600000), DeviceIDs: []string{"grp-0:logical-0:0000"}},
			},
			devID:         "logical-0",
			wantEntries:   1,
			wantRemaining: nodefeature.ResourceMaxUnits - 600000,
			wantSlices:    1,
		},
		{
			name: "sliced asymmetric overlapping DeviceIDs keeps peak unit charge independent of container name order",
			allocations: PodAllocations{
				"alpha-small": {Devices: alloc("logical-0", workercore.DeviceAllocationModeSliced, 400000), DeviceIDs: []string{"grp-0:logical-0:0000"}},
				"zeta-large":  {Devices: alloc("logical-0", workercore.DeviceAllocationModeSliced, 800000), DeviceIDs: []string{"grp-0:logical-0:0000"}},
			},
			devID:         "logical-0",
			wantEntries:   1,
			wantRemaining: nodefeature.ResourceMaxUnits - 800000,
			wantSlices:    1,
		},
		{
			name: "sliced disjoint DeviceIDs counts two slices and sums units",
			allocations: PodAllocations{
				"aux":  {Devices: alloc("logical-0", workercore.DeviceAllocationModeSliced, 400000), DeviceIDs: []string{"grp-0:logical-0:0000"}},
				"main": {Devices: alloc("logical-0", workercore.DeviceAllocationModeSliced, 400000), DeviceIDs: []string{"grp-0:logical-0:0001"}},
			},
			devID:         "logical-0",
			wantEntries:   2,
			wantRemaining: nodefeature.ResourceMaxUnits - 800000,
			wantSlices:    2,
		},
		{
			name: "partitioned overlapping DeviceIDs counts one MIG profile instance",
			allocations: PodAllocations{
				"forge-runner": {
					Devices:   partAlloc("mig-0", "3g.20gb", pl(0, 4)),
					DeviceIDs: []string{"grp-0:mig:0000"},
				},
				"forge-verifier": {
					Devices:   partAlloc("mig-0", "3g.20gb", pl(0, 4)),
					DeviceIDs: []string{"grp-0:mig:0000"},
				},
			},
			devID:           "mig-0",
			wantEntries:     1,
			wantRemaining:   nodefeature.ResourceMaxUnits - nodefeature.ResourceMaxUnits/8,
			wantProfileCnts: []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}},
		},
		{
			name: "legacy empty DeviceIDs retains both entries",
			allocations: PodAllocations{
				"c0": {Devices: alloc("logical-0", workercore.DeviceAllocationModeSliced, 300000)},
				"c1": {Devices: alloc("logical-0", workercore.DeviceAllocationModeSliced, 300000)},
			},
			devID:         "logical-0",
			wantEntries:   2,
			wantRemaining: nodefeature.ResourceMaxUnits - 600000,
			wantSlices:    2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agg := c.allocations.Aggregate()
			require.Len(t, agg.Groups, 1)
			assert.Len(t, agg.Groups[0].Accelerators, c.wantEntries)

			devs := physicalAndLogicalDevices(nodeName)
			pod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Name:        "pod-agg",
					Namespace:   "default",
					UID:         types.UID("uid-agg"),
					Annotations: allocationAnnotation(t, c.allocations),
				},
				Spec:   core.PodSpec{NodeName: nodeName},
				Status: core.PodStatus{Phase: core.PodRunning},
			}
			desired, _ := BuildDesiredStatus(logr.Discard(), devs, &core.PodList{Items: []core.Pod{*pod}})
			require.Len(t, desired.Groups, 1)
			var gotAcc *workercore.AcceleratorAllocation
			for i := range desired.Groups[0].Accelerators {
				if desired.Groups[0].Accelerators[i].ID == c.devID {
					gotAcc = &desired.Groups[0].Accelerators[i]
					break
				}
			}
			require.NotNil(t, gotAcc)
			assert.Equal(t, c.wantRemaining, gotAcc.Remaining)
			assert.Equal(t, c.wantSlices, gotAcc.AllocatedSlices)
			if c.wantProfileCnts != nil {
				assert.Equal(t, c.wantProfileCnts, gotAcc.AllocatedProfiles)
			}
		})
	}
}

func TestClassifyAllocatingCandidates_RestartRecovery(t *testing.T) {
	const nodeName = "node-restart"
	resName := nodefeature.GetAcceleratableResourceName(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive,
	)
	runnerStatus := workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
		ID:           "grp-0",
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Accelerators: []workercore.AcceleratorAllocation{{
			ID:        "dev-0",
			Mode:      workercore.DeviceAllocationModeExclusive,
			Allocated: nodefeature.ResourceMaxUnits,
		}},
	}}}

	cases := []struct {
		name          string
		initStatuses  []core.ContainerStatus
		wantContainer string
	}{
		{
			name: "completed init container in annotation is treated as claimed so successor is selected after restart",
			initStatuses: []core.ContainerStatus{{
				Name: "forge-runner",
				State: core.ContainerState{
					Terminated: &core.ContainerStateTerminated{ExitCode: 0},
				},
			}},
			wantContainer: "forge-verifier",
		},
		{
			name:          "unstarted init container in annotation without reservation is selected for kubelet replay",
			initStatuses:  nil,
			wantContainer: "forge-runner",
		},
		{
			name: "failed non-zero exit init container is not treated as completed",
			initStatuses: []core.ContainerStatus{{
				Name: "forge-runner",
				State: core.ContainerState{
					Terminated: &core.ContainerStateTerminated{ExitCode: 1},
				},
			}},
			wantContainer: "forge-runner",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Name:      "forge-pod",
					Namespace: "default",
					UID:       types.UID("uid-forge"),
					Annotations: allocationAnnotation(t, PodAllocations{
						"forge-runner": {
							Devices:   runnerStatus,
							DeviceIDs: []string{"grp-0:dev-0:0000"},
						},
					}),
				},
				Spec: core.PodSpec{
					NodeName: nodeName,
					InitContainers: []core.Container{
						{
							Name:      "forge-runner",
							Resources: core.ResourceRequirements{Limits: core.ResourceList{resName: resource.MustParse("1")}},
						},
						{
							Name:      "forge-verifier",
							Resources: core.ResourceRequirements{Limits: core.ResourceList{resName: resource.MustParse("1")}},
						},
					},
					Containers: []core.Container{{Name: "pause"}},
				},
				Status: core.PodStatus{
					Phase:                 core.PodPending,
					InitContainerStatuses: c.initStatuses,
				},
			}

			cli := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(pod).
				WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
					return []string{obj.(*core.Pod).Spec.NodeName}
				}).
				Build()
			r := &DevicesReconciler{NodeName: nodeName, Client: cli}

			_, ctr, err := r.getAllocatingPod(context.Background(), exclusiveMatch(true))
			require.NoError(t, err)
			assert.Equal(t, c.wantContainer, ctr.Name)
		})
	}
}

type logicalSliceRecordingResponder struct {
	placeCalls int
	placed     Placements
	got        []Placements
}

func (r *logicalSliceRecordingResponder) GetContainerAllocateResponse(
	context.Context, *core.Pod, *core.Container, *workercore.Devices, map[Resource]int32,
) (*ContainerAllocateResponse, error) {
	return &ContainerAllocateResponse{}, nil
}

func (r *logicalSliceRecordingResponder) PlaceLogicalSliced(
	_ context.Context, _ *core.Pod, _ *core.Container, _ *workercore.Devices,
	_ map[Resource]int32, occupied Placements,
) (Placements, error) {
	r.placeCalls++
	res := Resource{Group: "grp-0", Device: "dev-0"}
	if len(occupied[res]) > 0 {
		return nil, assert.AnError
	}
	return r.placed, nil
}

func (r *logicalSliceRecordingResponder) GetLogicalSlicedResponse(
	_ context.Context, _ *core.Pod, _ *core.Container, _ *workercore.Devices,
	_ map[Resource]int32, placements Placements,
) (*ContainerAllocateResponse, error) {
	r.got = append(r.got, placements)
	return &ContainerAllocateResponse{}, nil
}

type slotTrackingPhysicalResponder struct {
	stubResponder
	actuatedSlots []string
	visOwners     []string
}

func (r *slotTrackingPhysicalResponder) ActuatePhysicalSliced(
	_ context.Context, pod *core.Pod, ctr *core.Container, _ *workercore.Devices,
	_ map[Resource]int32, profile string,
) (*PhysicalSlicedAllocation, error) {
	slot := CanonicalSlotContainerName(pod, ctr)
	r.actuatedSlots = append(r.actuatedSlots, slot)
	res := Resource{Group: "grp-0", Device: "mig-0"}
	return &PhysicalSlicedAllocation{
		Profile:    profile,
		Placements: Placements{res: {pl(0, 8)}},
		IDs:        map[Resource]string{res: "MIG-7g"},
		Response:   &ContainerAllocateResponse{Envs: map[string]string{"NVIDIA_VISIBLE_DEVICES": "MIG-7g"}},
		Rollback:   func() {},
	}, nil
}

func (r *slotTrackingPhysicalResponder) GetPhysicalSlicedVisibilityResponse(
	_ context.Context, _ *core.Pod, _ *core.Container, _ *workercore.Devices,
	_ map[Resource]int32, owner string,
) (*ContainerAllocateResponse, error) {
	r.visOwners = append(r.visOwners, owner)
	return &ContainerAllocateResponse{Envs: map[string]string{"NVIDIA_VISIBLE_DEVICES": "MIG-7g"}}, nil
}

func TestResourceServer_Allocate_SequentialInitContainersReuse(t *testing.T) {
	const nodeName = "node-init-reuse"
	ctx := context.Background()

	newFakeClient := func(objs ...ctrlcli.Object) ctrlcli.Client {
		return ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(objs...).
			WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
				return []string{obj.(*core.Pod).Spec.NodeName}
			}).
			Build()
	}

	t.Run("exclusive and shared modes reuse same token without double ledger charge", func(t *testing.T) {
		for _, mode := range []workercore.DeviceAllocationMode{
			workercore.DeviceAllocationModeExclusive,
			workercore.DeviceAllocationModeShared,
		} {
			resName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, mode)
			devs := &workercore.Devices{
				ObjectMeta: meta.ObjectMeta{Name: nodeName},
				Spec: workercore.DevicesSpec{Groups: []workercore.DevicesGroup{{
					ID:           "grp-0",
					Manufacturer: nodefeature.ManufacturerNVIDIA,
					Accelerators: []workercore.Accelerator{{ID: "dev-0", Index: 0}},
				}}},
			}
			pod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{Name: "forge", Namespace: "default", UID: types.UID("uid-forge")},
				Spec: core.PodSpec{
					NodeName: nodeName,
					InitContainers: []core.Container{
						{Name: "forge-runner", Resources: core.ResourceRequirements{Limits: core.ResourceList{resName: resource.MustParse("1")}}},
						{Name: "forge-verifier", Resources: core.ResourceRequirements{Limits: core.ResourceList{resName: resource.MustParse("1")}}},
					},
					Containers: []core.Container{{Name: "pause"}},
				},
				Status: core.PodStatus{Phase: core.PodPending},
			}
			cli := newFakeClient(devs, pod)
			rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
			srv := &ResourceServer{
				Manufacturer:   nodefeature.ManufacturerNVIDIA,
				AllocationMode: mode,
				Reconciler:     rec,
				Responder:      stubResponder{},
			}

			req := &AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}}}
			_, err := srv.Allocate(ctx, req)
			require.NoError(t, err)
			_, err = srv.Allocate(ctx, req)
			require.NoError(t, err)

			got := new(core.Pod)
			require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), got))
			allocs, err := AllocatedAcceleratorsOf(got)
			require.NoError(t, err)
			assert.Contains(t, allocs, "forge-runner")
			assert.Contains(t, allocs, "forge-verifier")

			desired, _ := BuildDesiredStatus(logr.Discard(), devs, &core.PodList{Items: []core.Pod{*got}})
			require.Len(t, desired.Groups[0].Accelerators, 1)
			expectedUnits := int32(nodefeature.ResourceMaxUnits)
			if mode == workercore.DeviceAllocationModeShared {
				expectedUnits = nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize
			}
			assert.Equal(t, nodefeature.ResourceMaxUnits-expectedUnits, desired.Groups[0].Accelerators[0].Remaining)
		}
	})

	t.Run("sliced mode allows >50% and 100% sequential init reuse and preserves neighbor room", func(t *testing.T) {
		slicedRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)
		unitsRes := nodefeature.GetAcceleratableSlicedUnitsResourceName(nodefeature.ManufacturerNVIDIA)

		for _, tc := range []struct {
			name      string
			units     int64
			slotCount int32
			neighbor  int64
		}{
			{name: "60 percent memory on 2-slot card leaves 40 percent for neighbor", units: 600000, slotCount: 2, neighbor: 400000},
			{name: "100 percent memory on 1-slot card succeeds for sequential init", units: nodefeature.ResourceMaxUnits, slotCount: 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				devs := &workercore.Devices{
					ObjectMeta: meta.ObjectMeta{Name: nodeName},
					Spec: workercore.DevicesSpec{Groups: []workercore.DevicesGroup{{
						ID:           "grp-0",
						Manufacturer: nodefeature.ManufacturerNVIDIA,
						Accelerators: []workercore.Accelerator{{
							ID:     "dev-0",
							Index:  0,
							Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: tc.slotCount}},
						}},
					}}},
				}
				lim := core.ResourceList{
					slicedRes: resource.MustParse("1"),
					unitsRes:  *resource.NewQuantity(tc.units, resource.DecimalSI),
				}
				pod := &core.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "forge", Namespace: "default", UID: types.UID("uid-sliced")},
					Spec: core.PodSpec{
						NodeName: nodeName,
						InitContainers: []core.Container{
							{Name: "forge-runner", Resources: core.ResourceRequirements{Limits: lim}},
							{Name: "forge-verifier", Resources: core.ResourceRequirements{Limits: lim}},
						},
						Containers: []core.Container{{Name: "pause"}},
					},
					Status: core.PodStatus{Phase: core.PodPending},
				}
				objs := []ctrlcli.Object{devs, pod}
				var neighborPod *core.Pod
				if tc.neighbor > 0 {
					neighborPod = &core.Pod{
						ObjectMeta: meta.ObjectMeta{
							Name:              "neighbor",
							Namespace:         "default",
							UID:               types.UID("uid-neighbor"),
							CreationTimestamp: meta.Now(),
						},
						Spec: core.PodSpec{
							NodeName: nodeName,
							Containers: []core.Container{{
								Name: "main",
								Resources: core.ResourceRequirements{Limits: core.ResourceList{
									slicedRes: resource.MustParse("1"),
									unitsRes:  *resource.NewQuantity(tc.neighbor, resource.DecimalSI),
								}},
							}},
						},
						Status: core.PodStatus{Phase: core.PodPending},
					}
					objs = append(objs, neighborPod)
				}

				cli := newFakeClient(objs...)
				rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
				srv := &ResourceServer{
					Manufacturer:   nodefeature.ManufacturerNVIDIA,
					AllocationMode: workercore.DeviceAllocationModeSliced,
					Reconciler:     rec,
					Responder:      stubResponder{},
				}

				reuseReq := &AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}}}
				_, err := srv.Allocate(ctx, reuseReq)
				require.NoError(t, err, "first init container allocation must succeed")
				_, err = srv.Allocate(ctx, reuseReq)
				require.NoError(t, err, "second sequential init container reusing token must not fail sliced overcommit")

				if neighborPod != nil {
					pref, err := srv.GetPreferredAllocation(ctx, &PreferredAllocationRequest{
						ContainerRequests: []*ContainerPreferredAllocationRequest{{
							AvailableDeviceIDs: []string{"grp-0:dev-0:0001"},
							AllocationSize:     1,
						}},
					})
					require.NoError(t, err)
					assert.Equal(t, []string{"grp-0:dev-0:0001"}, pref.ContainerResponses[0].DeviceIDs)

					_, err = srv.Allocate(ctx, &AllocateRequest{
						ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0001"}}},
					})
					require.NoError(t, err, "neighbor fitting remaining room must succeed")
				}
			})
		}
	})

	t.Run("partitioned mode reuses single MIG slot across sequential inits and resolves visibility owner", func(t *testing.T) {
		partRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModePartitioned)
		profRes := nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, "7g.80gb")
		visRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)

		devs := &workercore.Devices{
			ObjectMeta: meta.ObjectMeta{Name: nodeName},
			Spec: workercore.DevicesSpec{Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Memory:       81920,
				Accelerators: []workercore.Accelerator{{
					ID:    "mig-0",
					Index: 0,
					Status: workercore.AcceleratorStatus{PhysicalSliced: workercore.AcceleratorPhysicalSliced{
						Profiles: []workercore.AcceleratorPhysicalSlicedProfile{{
							Name:          "7g.80gb",
							MemoryMib:     81920,
							ComputeSlices: 7,
							MemorySlices:  8,
							Count:         1,
							Placements:    []workercore.AcceleratorPlacement{pl(0, 8)},
						}},
						Count: 1,
					}},
				}},
			}}},
		}
		lim := core.ResourceList{
			partRes: resource.MustParse("1"),
			profRes: resource.MustParse("1"),
		}
		pod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Name:      "forge-mig",
				Namespace: "default",
				UID:       types.UID("uid-mig"),
			},
			Spec: core.PodSpec{
				NodeName: nodeName,
				InitContainers: []core.Container{
					{Name: "step-runner", Resources: core.ResourceRequirements{Limits: lim}},
					{Name: "alpha-verifier", Resources: core.ResourceRequirements{Limits: lim}},
					{
						Name:          "sshd",
						RestartPolicy: ptr.To(core.ContainerRestartPolicyAlways),
						Resources:     core.ResourceRequirements{Limits: core.ResourceList{visRes: resource.MustParse("1")}},
					},
				},
				Containers: []core.Container{{Name: "pause"}},
			},
			Status: core.PodStatus{Phase: core.PodPending},
		}

		cli := newFakeClient(devs, pod)
		rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
		resp := &slotTrackingPhysicalResponder{}
		srv := &ResourceServer{
			Manufacturer:   nodefeature.ManufacturerNVIDIA,
			AllocationMode: workercore.DeviceAllocationModePartitioned,
			Reconciler:     rec,
			Responder:      resp,
		}

		req := &AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:mig:0000"}}}}
		_, err := srv.Allocate(ctx, req)
		require.NoError(t, err, "first MIG init container must succeed")
		_, err = srv.Allocate(ctx, req)
		require.NoError(t, err, "second sequential MIG init container must reuse the 1-slot card")
		assert.Equal(t, []string{"step-runner", "step-runner"}, resp.actuatedSlots)

		visSrv := &ResourceServer{
			Manufacturer:   nodefeature.ManufacturerNVIDIA,
			AllocationMode: workercore.DeviceAllocationModeVisibility,
			Reconciler:     rec,
			Responder:      resp,
		}
		_, err = visSrv.Allocate(ctx, &AllocateRequest{
			ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:mig-0:0000"}}},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"step-runner"}, resp.visOwners, "visibility resolves canonical owner slot")

		got := new(core.Pod)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), got))
		desired, _ := BuildDesiredStatus(logr.Discard(), devs, &core.PodList{Items: []core.Pod{*got}})
		acc := desired.Groups[0].Accelerators[0]
		assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "7g.80gb", Count: 1}}, acc.AllocatedProfiles)
		assert.Empty(t, acc.RemainingProfiles)
	})

	t.Run("logical sliced mode inherits predecessor CU placement without re-placing", func(t *testing.T) {
		slicedRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerAMD, workercore.DeviceAllocationModeSliced)
		unitsRes := nodefeature.GetAcceleratableSlicedUnitsResourceName(nodefeature.ManufacturerAMD)

		devs := &workercore.Devices{
			ObjectMeta: meta.ObjectMeta{Name: nodeName},
			Spec: workercore.DevicesSpec{Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerAMD,
				Accelerators: []workercore.Accelerator{{
					ID:     "dev-0",
					Index:  0,
					Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 4}},
				}},
			}}},
		}
		lim := core.ResourceList{
			slicedRes: resource.MustParse("1"),
			unitsRes:  *resource.NewQuantity(750000, resource.DecimalSI),
		}
		pod := &core.Pod{
			ObjectMeta: meta.ObjectMeta{Name: "forge-amd", Namespace: "default", UID: types.UID("uid-amd")},
			Spec: core.PodSpec{
				NodeName: nodeName,
				InitContainers: []core.Container{
					{Name: "forge-runner", Resources: core.ResourceRequirements{Limits: lim}},
					{Name: "forge-verifier", Resources: core.ResourceRequirements{Limits: lim}},
				},
				Containers: []core.Container{{Name: "pause"}},
			},
			Status: core.PodStatus{Phase: core.PodPending},
		}

		cli := newFakeClient(devs, pod)
		rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
		wantWindow := Placements{{Group: "grp-0", Device: "dev-0"}: {pl(0, 48)}}
		placer := &logicalSliceRecordingResponder{placed: wantWindow}
		srv := &ResourceServer{
			Manufacturer:   nodefeature.ManufacturerAMD,
			AllocationMode: workercore.DeviceAllocationModeSliced,
			Reconciler:     rec,
			Responder:      placer,
		}

		req := &AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}}}
		_, err := srv.Allocate(ctx, req)
		require.NoError(t, err)
		_, err = srv.Allocate(ctx, req)
		require.NoError(t, err)

		assert.Equal(t, 1, placer.placeCalls, "second init container must inherit CU placement rather than re-placing")
		require.Len(t, placer.got, 2)
		assert.Equal(t, wantWindow, placer.got[0])
		assert.Equal(t, wantWindow, placer.got[1])
	})
}
