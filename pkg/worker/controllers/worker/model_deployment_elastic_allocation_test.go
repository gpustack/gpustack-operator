package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/elasticprofile"
)

// The cases here are behavioral: a fake server carrying the production annotation shape,
// real Workloads with their own conditions and admissions, and the Devices ledger the device
// manager publishes. The anchor positive is a member admitted through its own group-of-one
// Workload that holds one whole card its node's ledger agrees to; every negative holds the
// exact layer the defect belongs to and no other, because the two layers answering
// independently is the contract.

const (
	allocationDeployUID  = types.UID("elastic-deploy-uid")
	allocationGeneration = int64(3)
)

func allocationDeployment() *workercore.ModelDeployment {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Generation = allocationGeneration
		md.Spec.KVCache = nil
		md.Spec.Roles[0].ElasticEP = &workercore.ModelDeploymentRoleElasticEP{Width: 4}
	})
	md.UID = allocationDeployUID

	return md
}

// allocationDevices is the node ledger two whole-card members are carved from: the retirement
// release fixture's ledger with a second card, so two members never share one.
func allocationDevices() *workercore.Devices {
	return releaseDevices(func(devs *workercore.Devices) {
		devs.Spec.Groups[0].Accelerators = append(devs.Spec.Groups[0].Accelerators,
			workercore.Accelerator{ID: "GPU-def", Index: 1})
	})
}

// allocationCardAnnotation is the allocation record one container carries for one card, in
// the shape the allocator writes.
func allocationCardAnnotation(
	t *testing.T, container, cardID string, index uint32,
	mode workercore.DeviceAllocationMode, allocated int32,
) string {
	t.Helper()

	return mustJSON(t, deviceplugin.PodAllocations{container: {Devices: workercore.DevicesStatus{
		Groups: []workercore.DevicesAllocationGroup{{
			ID: "gpu-0", Manufacturer: "NVIDIA",
			Accelerators: []workercore.AcceleratorAllocation{{
				ID: cardID, Index: index, Mode: mode, Allocated: allocated,
			}},
		}},
	}}})
}

// allocationMember builds one seated GPU member the way the elastic render and the allocator
// leave them: owned by the deployment in full, ordinal-labeled, requesting and -- unless a
// case says otherwise -- holding one whole card through its main container.
func allocationMember(
	t *testing.T, md *workercore.ModelDeployment, name, uid string, ordinal int,
	mutate ...func(*core.Pod),
) *core.Pod {
	t.Helper()
	owned := true
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: name, Namespace: md.Namespace, UID: types.UID(uid),
			Labels: map[string]string{
				modelDeploymentLabelKeyComponent:   "server",
				modelDeploymentReplicaOrdinalLabel: strconv.Itoa(ordinal),
			},
			OwnerReferences: []meta.OwnerReference{{
				APIVersion: workercore.GroupVersion.String(), Kind: "ModelDeployment",
				Name: md.Name, UID: md.UID, Controller: &owned,
			}},
		},
		Spec: core.PodSpec{
			NodeName: releaseNode,
			Containers: []core.Container{{
				Name: modelDeploymentMainContainerName,
				Resources: core.ResourceRequirements{
					Requests: core.ResourceList{
						nodefeature.GetAcceleratableResourceName(
							nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive,
						): resource.MustParse("1"),
					},
				},
			}},
		},
		Status: core.PodStatus{Phase: core.PodRunning},
	}
	pod.Annotations = map[string]string{
		deviceplugin.AllocatedAcceleratorAnnoKey: allocationCardAnnotation(
			t, modelDeploymentMainContainerName, "GPU-abc", 0,
			workercore.DeviceAllocationModeExclusive, nodefeature.ResourceMaxUnits),
	}
	for _, m := range mutate {
		m(pod)
	}

	return pod
}

// allocationHead is the CPU head: owned and ordinal-seated like a member, but carrying the
// head's own component, which is what excludes it from the role's count.
func allocationHead(t *testing.T, md *workercore.ModelDeployment) *core.Pod {
	t.Helper()
	head := allocationMember(t, md, modelDeploymentElasticHeadName(md), "head-uid", 0)
	head.Annotations = nil
	head.Spec.Containers[0].Resources = core.ResourceRequirements{}
	head.Labels[modelDeploymentLabelKeyComponent] = modelDeploymentElasticHeadName(md)

	return head
}

// allocationWorkload is the group-of-one Workload one member composes: one server podset of
// one member, a queue behind it, and the reservation and admission the queue writes, with
// one matching assignment whose Count is left nil -- the documented producer shape where the
// nil defaults to the spec's own count. Cases poke at exactly these fields.
func allocationWorkload(name, uid, ownerPodName, ownerPodUID string, mutate ...func(*kueue.Workload)) *kueue.Workload {
	workload := &kueue.Workload{}
	workload.Name, workload.Namespace = name, "team-a"
	workload.UID = types.UID(uid)
	workload.OwnerReferences = []meta.OwnerReference{
		{APIVersion: "v1", Kind: "Pod", Name: ownerPodName, UID: types.UID(ownerPodUID)},
	}
	workload.Spec.PodSets = []kueue.PodSet{{Name: kueue.PodSetReference("server"), Count: 1}}
	workload.Status.Conditions = []meta.Condition{
		{
			Type: kueue.WorkloadQuotaReserved, Status: meta.ConditionTrue, Reason: "Reserved",
			LastTransitionTime: meta.Now(),
		},
		{
			Type: kueue.WorkloadAdmitted, Status: meta.ConditionTrue, Reason: "Admitted",
			LastTransitionTime: meta.Now(),
		},
	}
	workload.Status.Admission = &kueue.Admission{
		ClusterQueue:      "gpu-queue",
		PodSetAssignments: []kueue.PodSetAssignment{{Name: kueue.PodSetReference("server")}},
	}
	for _, m := range mutate {
		m(workload)
	}

	return workload
}

// allocationPublishLedger publishes the accounting the node's live records imply, so a case
// is about the disagreement it introduces rather than about an unrelated stale row. The
// object is read back first, so a second publish updates the ledger the server holds now.
func allocationPublishLedger(t *testing.T, cli ctrlcli.Client, devs *workercore.Devices) {
	t.Helper()
	live := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), live, ctrlcli.MatchingFields{"spec.nodeName": releaseNode}))
	rebuilt, _, err := deviceplugin.BuildDesiredStatusStrict(ctrllogDiscard(), devs, live)
	require.NoError(t, err, "the node's records are readable, or the case is not about the ledger")
	published := new(workercore.Devices)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: devs.Name}, published))
	published.Status = rebuilt
	require.NoError(t, cli.Status().Update(context.Background(), published))
}

func allocationReconciler(cli ctrlcli.Client) *ModelDeploymentReconciler {
	return &ModelDeploymentReconciler{Client: cli, APIReader: cli}
}

// mustJSON marshals a value or fails the test; a fixture that cannot encode the production
// annotation shape is not a case about the production annotation shape.
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return string(raw)
}

// TestObserveModelDeploymentElasticAllocation_WholeGPUMembers is the anchor positive and the
// independence positive in one: four seated members whose admission set and allocation set
// overlap but are not each other, plus the Pods that belong to no layer at all.
func TestObserveModelDeploymentElasticAllocation_WholeGPUMembers(t *testing.T) {
	md := allocationDeployment()
	devs := allocationDevices()
	m1 := allocationMember(t, md, "m1", "uid-1", 1)
	m2 := allocationMember(t, md, "m2", "uid-2", 2, func(pod *core.Pod) { pod.Annotations = nil })
	m3 := allocationMember(t, md, "m3", "uid-3", 3, func(pod *core.Pod) {
		pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = allocationCardAnnotation(
			t, modelDeploymentMainContainerName, "GPU-def", 1,
			workercore.DeviceAllocationModeExclusive, nodefeature.ResourceMaxUnits)
	})
	m4 := allocationMember(t, md, "m4", "uid-4", 4, func(pod *core.Pod) { pod.Annotations = nil })
	foreign := allocationMember(t, md, "foreign", "uid-f", 5, func(pod *core.Pod) {
		pod.OwnerReferences[0].UID = "another-deploy-uid"
		pod.Annotations = nil
	})
	unseated := allocationMember(t, md, "unseated", "uid-u", 6, func(pod *core.Pod) {
		delete(pod.Labels, modelDeploymentReplicaOrdinalLabel)
		pod.Annotations = nil
	})

	cli := newModelDeploymentClient(md, devs,
		allocationWorkload("wl-1", "wl-uid-1", "m1", "uid-1"),
		allocationWorkload("wl-2", "wl-uid-2", "m2", "uid-2"),
		m1, m2, m3, m4, foreign, unseated, allocationHead(t, md))
	allocationPublishLedger(t, cli, devs)

	supplied := []core.Pod{*m1, *m2, *m3, *m4, *foreign, *unseated, *allocationHead(t, md)}
	admitted, allocated := allocationReconciler(cli).observeModelDeploymentElasticAllocation(
		context.Background(), md, supplied)

	// The quota layer holds exactly the two members an admitted group-of-one Workload names;
	// the allocation layer holds exactly the two members a whole-card record and an agreeing
	// ledger name. m2 and m3 prove neither layer borrowed the other's answer.
	assert.True(t, admitted.Known, "reason: %s", admitted.Reason)
	assert.Equal(t, 2, admitted.Value)
	assert.Empty(t, admitted.Reason)
	assert.True(t, allocated.Known, "reason: %s", allocated.Reason)
	assert.Equal(t, 2, allocated.Value)
	assert.Empty(t, allocated.Reason)
}

// TestObserveModelDeploymentElasticAllocation_Holds is every way one member's answer is not
// a count. Each case mutates the anchor world in one place and names the layer it holds and
// the layer it must leave standing.
func TestObserveModelDeploymentElasticAllocation_Holds(t *testing.T) {
	cases := []struct {
		name string
		// member mutates the one member the layers are asked about.
		member func(*testing.T, *core.Pod)
		// workload mutates that member's Workload.
		workload func(*kueue.Workload)
		// claimTwice adds a second Workload claiming the same member, so the admission
		// claim cannot be decided at all.
		claimTwice bool
		// arrange is the server state the case is about, applied after the ledger publishes.
		arrange func(*testing.T, ctrlcli.Client, *workercore.Devices)
		// absent holds no member on the server: the pass sees a Pod the server does not.
		absent bool
		// staleSupplied hands the pass a member carrying an identity the server replaced.
		staleSupplied bool
		// noPublish leaves the published ledger empty: the case's own fixture is a record
		// the strict publisher would refuse, and the layer under test fails before the
		// published ledger is ever read.
		noPublish bool
		// wantAdmitted/wantAllocated are the expected layers; an Unknown want asserts the
		// reason fragment.
		wantAdmitted  elasticLayer
		wantAllocated elasticLayer
	}{
		{
			name: "an unreadable allocation record holds allocation only",
			member: func(t *testing.T, pod *core.Pod) {
				pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = "{not json"
			},
			noPublish:     true,
			wantAdmitted:  knownLayer(1),
			wantAllocated: unknownLayer("not readable"),
		},
		{
			name: "a record on another container holds allocation",
			member: func(t *testing.T, pod *core.Pod) {
				pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = allocationCardAnnotation(
					t, "sidecar", "GPU-abc", 0,
					workercore.DeviceAllocationModeExclusive, nodefeature.ResourceMaxUnits)
			},
			noPublish:     true,
			wantAdmitted:  knownLayer(1),
			wantAllocated: unknownLayer(`other than "main"`),
		},
		{
			name: "a sliced-mode record holds allocation",
			member: func(t *testing.T, pod *core.Pod) {
				pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = allocationCardAnnotation(
					t, modelDeploymentMainContainerName, "GPU-abc", 0,
					workercore.DeviceAllocationModeSliced, nodefeature.ResourceMaxUnits/4)
			},
			wantAdmitted:  knownLayer(1),
			wantAllocated: unknownLayer("non-exclusive"),
		},
		{
			name: "an exclusive record carrying slices holds allocation",
			member: func(t *testing.T, pod *core.Pod) {
				pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = mustJSON(t, deviceplugin.PodAllocations{
					modelDeploymentMainContainerName: {Devices: workercore.DevicesStatus{
						Groups: []workercore.DevicesAllocationGroup{{
							ID: "gpu-0", Manufacturer: "NVIDIA",
							Accelerators: []workercore.AcceleratorAllocation{{
								ID: "GPU-abc", Index: 0,
								Mode:            workercore.DeviceAllocationModeExclusive,
								Allocated:       nodefeature.ResourceMaxUnits,
								AllocatedSlices: 1,
							}},
						}},
					}},
				})
			},
			wantAdmitted:  knownLayer(1),
			wantAllocated: unknownLayer("sliced"),
		},
		{
			name: "a partial-card record holds allocation",
			member: func(t *testing.T, pod *core.Pod) {
				pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = allocationCardAnnotation(
					t, modelDeploymentMainContainerName, "GPU-abc", 0,
					workercore.DeviceAllocationModeExclusive, 160_000)
			},
			wantAdmitted:  knownLayer(1),
			wantAllocated: unknownLayer("not a whole card"),
		},
		{
			name: "two cards on one member hold allocation",
			member: func(t *testing.T, pod *core.Pod) {
				record := mustJSON(t, deviceplugin.PodAllocations{
					modelDeploymentMainContainerName: {Devices: workercore.DevicesStatus{
						Groups: []workercore.DevicesAllocationGroup{{
							ID: "gpu-0", Manufacturer: "NVIDIA",
							Accelerators: []workercore.AcceleratorAllocation{
								{
									ID: "GPU-abc", Index: 0, Mode: workercore.DeviceAllocationModeExclusive,
									Allocated: nodefeature.ResourceMaxUnits,
								},
								{
									ID: "GPU-def", Index: 1, Mode: workercore.DeviceAllocationModeExclusive,
									Allocated: nodefeature.ResourceMaxUnits,
								},
							},
						}},
					}},
				})
				pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = record
			},
			wantAdmitted:  knownLayer(1),
			wantAllocated: unknownLayer("requires exactly 1"),
		},
		{
			name: "a card outside the node's inventory holds allocation",
			member: func(t *testing.T, pod *core.Pod) {
				pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = allocationCardAnnotation(
					t, modelDeploymentMainContainerName, "GPU-xyz", 7,
					workercore.DeviceAllocationModeExclusive, nodefeature.ResourceMaxUnits)
			},
			noPublish:     true,
			wantAdmitted:  knownLayer(1),
			wantAllocated: unknownLayer("not in the node's current inventory"),
		},
		{
			name: "a node with no ledger holds allocation",
			arrange: func(t *testing.T, cli ctrlcli.Client, devs *workercore.Devices) {
				require.NoError(t, cli.Delete(context.Background(), devs))
			},
			wantAdmitted:  knownLayer(1),
			wantAllocated: unknownLayer("read the node's ledger"),
		},
		{
			name: "a stale published ledger holds allocation",
			arrange: func(t *testing.T, cli ctrlcli.Client, devs *workercore.Devices) {
				current := new(workercore.Devices)
				require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: devs.Name}, current))
				published := current.DeepCopy()
				published.Status.Groups[0].Accelerators[0] = workercore.AcceleratorAllocation{
					ID: "GPU-abc", Index: 0,
					Mode: workercore.DeviceAllocationModeNone, Remaining: nodefeature.ResourceMaxUnits,
				}
				require.NoError(t, cli.Status().Update(context.Background(), published))
			},
			wantAdmitted:  knownLayer(1),
			wantAllocated: unknownLayer("has not settled"),
		},
		{
			name: "a workload without admission holds quota only",
			workload: func(wl *kueue.Workload) {
				for i := range wl.Status.Conditions {
					if wl.Status.Conditions[i].Type == kueue.WorkloadAdmitted {
						wl.Status.Conditions[i].Status = meta.ConditionFalse
					}
				}
			},
			wantAdmitted:  knownLayer(0),
			wantAllocated: knownLayer(1),
		},
		{
			name: "a workload without an assignment holds quota only",
			workload: func(wl *kueue.Workload) {
				wl.Status.Admission = nil
			},
			wantAdmitted:  knownLayer(0),
			wantAllocated: knownLayer(1),
		},
		{
			name: "no workload holds quota only",
			workload: func(wl *kueue.Workload) {
				wl.OwnerReferences = nil
			},
			wantAdmitted:  knownLayer(0),
			wantAllocated: knownLayer(1),
		},
		{
			name: "a foreign workload claim holds quota only",
			workload: func(wl *kueue.Workload) {
				wl.OwnerReferences[0].UID = "another-member-uid"
			},
			wantAdmitted:  knownLayer(0),
			wantAllocated: knownLayer(1),
		},
		{
			name: "a group of several holds quota only",
			workload: func(wl *kueue.Workload) {
				wl.OwnerReferences = append(wl.OwnerReferences, meta.OwnerReference{
					APIVersion: "v1", Kind: "Pod", Name: "sibling", UID: "uid-sibling",
				})
			},
			wantAdmitted:  knownLayer(0),
			wantAllocated: knownLayer(1),
		},
		{
			// TWO Workloads claiming one member is a claim no caller may pick from: the
			// admission layer holds, and the allocation layer -- which never read either
			// Workload -- stays standing.
			name:          "two workloads claiming one member hold quota only",
			claimTwice:    true,
			wantAdmitted:  unknownLayer("both claim"),
			wantAllocated: knownLayer(1),
		},
		{
			name: "a member without a record is a known pending zero",
			member: func(t *testing.T, pod *core.Pod) {
				pod.Annotations = nil
			},
			wantAdmitted:  knownLayer(1),
			wantAllocated: knownLayer(0),
		},
		{
			name:          "a same-name replacement is not the member this pass counted",
			staleSupplied: true,
			wantAdmitted:  knownLayer(0),
			wantAllocated: knownLayer(0),
		},
		{
			name:          "a member already gone is a known zero on both layers",
			absent:        true,
			wantAdmitted:  knownLayer(0),
			wantAllocated: knownLayer(0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := allocationDeployment()
			devs := allocationDevices()
			member := allocationMember(t, md, "m1", "uid-1", 1)
			if tc.member != nil {
				tc.member(t, member)
			}
			supplied := *member
			if tc.staleSupplied {
				supplied.UID = "uid-stale"
			}
			workload := allocationWorkload("wl-1", "wl-uid-1", "m1", "uid-1")
			if tc.workload != nil {
				tc.workload(workload)
			}
			objs := []ctrlcli.Object{md, devs, workload}
			if tc.claimTwice {
				objs = append(objs, allocationWorkload("wl-1-rival", "wl-uid-1-rival", "m1", "uid-1"))
			}
			if !tc.absent {
				objs = append(objs, member)
			}
			cli := newModelDeploymentClient(objs...)
			if !tc.noPublish {
				allocationPublishLedger(t, cli, devs)
			}
			if tc.arrange != nil {
				tc.arrange(t, cli, devs)
			}

			admitted, allocated := allocationReconciler(cli).observeModelDeploymentElasticAllocation(
				context.Background(), md, []core.Pod{supplied})

			assertLayer(t, "admitted", admitted, tc.wantAdmitted)
			assertLayer(t, "allocated", allocated, tc.wantAllocated)
		})
	}

	t.Run("a deployment without an elastic role holds both layers", func(t *testing.T) {
		admitted, allocated := allocationReconciler(
			newModelDeploymentClient()).observeModelDeploymentElasticAllocation(
			context.Background(), newRenderDeployment(), nil)
		assert.False(t, admitted.Known)
		assert.Contains(t, admitted.Reason, "no elastic role")
		assert.False(t, allocated.Known)
		assert.Contains(t, allocated.Reason, "no elastic role")
	})
}

// TestObserveModelDeploymentElasticAllocation_PodSetShape is the Workload assignment half of
// an admission: the workload must resolve exactly ONE spec podset of one member, exactly ONE
// admission assignment matching that podset's name at an effective count of one -- the nil
// assignment count defaulting to the spec's own -- behind a named ClusterQueue. Every case
// drives both consumers of the shared predicate: the admission layer counts the member or
// not, and a capture refuses with the named reason.
func TestObserveModelDeploymentElasticAllocation_PodSetShape(t *testing.T) {
	zero := int32(0)
	two := int32(2)
	cases := []struct {
		name string
		// workload mutates the member's Workload away from the anchor shape.
		workload func(*kueue.Workload)
		// wantAdmitted is the admission layer answer; the allocation layer stays known one
		// in every case, because it never reads the Workload.
		wantAdmitted elasticLayer
		// wantCapture is the fragment a capture's refusal must carry; empty means the
		// capture succeeds.
		wantCapture string
	}{
		{
			name:         "a nil assignment count defaults to the spec's one member",
			wantAdmitted: knownLayer(1),
		},
		{
			name: "an assignment for a podset the spec does not declare is refused",
			workload: func(wl *kueue.Workload) {
				wl.Status.Admission.PodSetAssignments[0].Name = kueue.PodSetReference("foreign")
			},
			wantAdmitted: knownLayer(0),
			wantCapture:  "which the spec does not declare",
		},
		{
			name: "an assignment admitting zero members is refused",
			workload: func(wl *kueue.Workload) {
				wl.Status.Admission.PodSetAssignments[0].Count = &zero
			},
			wantAdmitted: knownLayer(0),
			wantCapture:  "admits 0 members",
		},
		{
			name: "an assignment admitting two members is refused",
			workload: func(wl *kueue.Workload) {
				wl.Status.Admission.PodSetAssignments[0].Count = &two
			},
			wantAdmitted: knownLayer(0),
			wantCapture:  "admits 2 members",
		},
		{
			name: "a spec declaring two podsets is refused",
			workload: func(wl *kueue.Workload) {
				wl.Spec.PodSets = []kueue.PodSet{
					{Name: kueue.PodSetReference("server"), Count: 1},
					{Name: kueue.PodSetReference("head"), Count: 1},
				}
			},
			wantAdmitted: knownLayer(0),
			wantCapture:  "does not declare exactly one podset",
		},
		{
			name: "a spec declaring no podsets is refused",
			workload: func(wl *kueue.Workload) {
				wl.Spec.PodSets = nil
			},
			wantAdmitted: knownLayer(0),
			wantCapture:  "does not declare exactly one podset",
		},
		{
			name: "a spec podset of two members is refused",
			workload: func(wl *kueue.Workload) {
				wl.Spec.PodSets = []kueue.PodSet{{Name: kueue.PodSetReference("server"), Count: 2}}
			},
			wantAdmitted: knownLayer(0),
			wantCapture:  "podset of 2 members",
		},
		{
			name: "an admission naming no ClusterQueue is refused",
			workload: func(wl *kueue.Workload) {
				wl.Status.Admission.ClusterQueue = ""
			},
			wantAdmitted: knownLayer(0),
			wantCapture:  "names no ClusterQueue",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := allocationDeployment()
			devs := allocationDevices()
			member := allocationMember(t, md, "m1", "uid-1", 1)
			workload := allocationWorkload("wl-1", "wl-uid-1", "m1", "uid-1")
			if tc.workload != nil {
				tc.workload(workload)
			}
			cli := newModelDeploymentClient(md, devs, workload, member)
			allocationPublishLedger(t, cli, devs)

			admitted, allocated := allocationReconciler(cli).observeModelDeploymentElasticAllocation(
				context.Background(), md, []core.Pod{*member})

			assertLayer(t, "admitted", admitted, tc.wantAdmitted)
			assertLayer(t, "allocated", allocated, knownLayer(1))

			_, err := allocationReconciler(cli).captureModelDeploymentElasticRelease(
				context.Background(), md, []core.Pod{*member})
			if tc.wantCapture == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantCapture)
			}
		})
	}
}

func assertLayer(t *testing.T, name string, got, want elasticLayer) {
	t.Helper()
	assert.Equal(t, want.Known, got.Known, "%s layer known-ness (got reason: %s)", name, got.Reason)
	if !want.Known {
		assert.Contains(t, got.Reason, want.Reason, "%s layer reason", name)

		return
	}
	assert.Equal(t, want.Value, got.Value, "%s layer value", name)
}

// TestCaptureModelDeploymentElasticRelease is the capture contract: exactly the supplied
// members, refused in every shape it cannot stand behind, and a record the lower retirement
// release predicates consume unchanged.
func TestCaptureModelDeploymentElasticRelease(t *testing.T) {
	newWorld := func(t *testing.T, extra ...ctrlcli.Object) (*workercore.ModelDeployment, *core.Pod, ctrlcli.Client) {
		md := allocationDeployment()
		devs := allocationDevices()
		member := allocationMember(t, md, "m1", "uid-1", 1)
		cli := newModelDeploymentClient(append([]ctrlcli.Object{md, devs, member},
			append([]ctrlcli.Object{allocationWorkload("wl-1", "wl-uid-1", "m1", "uid-1")}, extra...)...)...)
		allocationPublishLedger(t, cli, devs)

		return md, member, cli
	}

	t.Run("a whole-GPU member is captured in the retirement record shape", func(t *testing.T) {
		md, member, cli := newWorld(t)

		record, err := allocationReconciler(cli).captureModelDeploymentElasticRelease(
			context.Background(), md, []core.Pod{*member})
		require.NoError(t, err)
		assert.Equal(t, allocationDeployUID, record.ModelDeploymentUID)
		assert.Equal(t, allocationGeneration, record.ObservedGeneration)
		assert.Equal(t, []string{"uid-1"}, record.TargetMemberUIDs)
		require.Len(t, record.Members, 1)
		assert.Equal(t, types.UID("uid-1"), record.Members[0].PodUID)
		assert.Equal(t, "m1", record.Members[0].Name)
		assert.Equal(t, releaseNode, record.Members[0].NodeName)
		assert.Equal(t, modelDeploymentRetirementClaimCard, record.Members[0].AcceleratorClaim)
		require.Len(t, record.Members[0].Cards, 1)
		assert.Equal(t, "GPU-abc", record.Members[0].Cards[0].DeviceID)
		assert.Equal(t, "gpu-0", record.Members[0].Cards[0].GroupID)
		assert.NotEmpty(t, record.Members[0].Claim)
		assert.Equal(t, []string{"wl-uid-1"}, record.WorkloadUIDs)
		require.Len(t, record.Nodes, 1)
		assert.Equal(t, releaseNode, record.Nodes[0].NodeName)
		assert.Equal(t, types.UID("devices-uid"), record.Nodes[0].DevicesUID)
		assert.NotEmpty(t, record.Nodes[0].Inventory)
		// The reservation fields the retirement operation owns are never invented here.
		assert.Empty(t, record.StartedAt)
		assert.Empty(t, record.LastConsumedRetryToken)
		assert.Empty(t, record.TargetWorkloadUID)
		assert.Empty(t, record.RoleName)
		assert.Zero(t, record.ReplicaOrdinal)
	})

	refusals := []struct {
		name string
		// pods is the set supplied to the capture; it may name Pods the server does not hold.
		pods func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod
		// arrange changes the server before the capture runs.
		arrange func(*testing.T, ctrlcli.Client, *workercore.ModelDeployment)
		want    string
	}{
		{
			name: "an empty set is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				return nil
			},
			want: "nothing to capture",
		},
		{
			name: "a member without an identity is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				stripped := *member
				stripped.UID = ""

				return []core.Pod{stripped}
			},
			want: "carries no identity",
		},
		{
			name: "the master is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				return []core.Pod{*allocationMember(t, md, "m0", "uid-0", 0)}
			},
			want: "the master is never retirable",
		},
		{
			name: "a member of another deployment is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				foreign := allocationMember(t, md, "foreign", "uid-f", 5, func(pod *core.Pod) {
					pod.OwnerReferences[0].UID = "another-deploy-uid"
				})

				return []core.Pod{*foreign}
			},
			want: "cannot be captured",
		},
		{
			name: "the head is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				return []core.Pod{*allocationHead(t, md)}
			},
			want: "cannot be captured",
		},
		{
			name: "a member supplied under a stale identity is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				stale := *member
				stale.UID = "uid-stale"

				return []core.Pod{stale}
			},
			want: "same-name replacement",
		},
		{
			name: "a member whose live claim moved is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				stale := *member
				stale.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = "{}"

				return []core.Pod{stale}
			},
			want: "different allocation",
		},
		{
			name: "a member without a record is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				return []core.Pod{*allocationMember(t, md, "m2", "uid-2", 2, func(pod *core.Pod) {
					pod.Annotations = nil
				})}
			},
			arrange: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				require.NoError(t, cli.Create(context.Background(),
					allocationMember(t, md, "m2", "uid-2", 2, func(pod *core.Pod) { pod.Annotations = nil })))
			},
			want: "holds no whole accelerator",
		},
		{
			name: "a member whose workload never composed is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				return []core.Pod{*member}
			},
			arrange: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				workload := new(kueue.Workload)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKey{Name: "wl-1", Namespace: md.Namespace}, workload))
				workload.OwnerReferences = nil
				require.NoError(t, cli.Update(context.Background(), workload))
			},
			want: "composes no workload",
		},
		{
			name: "a member whose workload is not admitted is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				return []core.Pod{*member}
			},
			arrange: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				workload := new(kueue.Workload)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKey{Name: "wl-1", Namespace: md.Namespace}, workload))
				for i := range workload.Status.Conditions {
					if workload.Status.Conditions[i].Type == kueue.WorkloadAdmitted {
						workload.Status.Conditions[i].Status = meta.ConditionFalse
					}
				}
				require.NoError(t, cli.Update(context.Background(), workload))
			},
			want: "not capturable",
		},
		{
			name: "a member whose workload is a group of several is refused",
			pods: func(t *testing.T, md *workercore.ModelDeployment, member *core.Pod) []core.Pod {
				return []core.Pod{*member}
			},
			arrange: func(t *testing.T, cli ctrlcli.Client, md *workercore.ModelDeployment) {
				workload := new(kueue.Workload)
				require.NoError(t, cli.Get(context.Background(),
					ctrlcli.ObjectKey{Name: "wl-1", Namespace: md.Namespace}, workload))
				workload.OwnerReferences = append(workload.OwnerReferences, meta.OwnerReference{
					APIVersion: "v1", Kind: "Pod", Name: "sibling", UID: "uid-sibling",
				})
				require.NoError(t, cli.Update(context.Background(), workload))
			},
			want: "not capturable",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			md, member, cli := newWorld(t)
			if tc.arrange != nil {
				tc.arrange(t, cli, md)
			}

			_, err := allocationReconciler(cli).captureModelDeploymentElasticRelease(
				context.Background(), md, tc.pods(t, md, member))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("capture then the lower release observers", func(t *testing.T) {
		md, member, cli := newWorld(t)
		r := allocationReconciler(cli)
		record, err := r.captureModelDeploymentElasticRelease(context.Background(), md, []core.Pod{*member})
		require.NoError(t, err)
		devs := allocationDevices()

		// Everything still present: every predicate says the release has not happened.
		_, ok := r.observeReleaseTargetsAbsent(context.Background(), record)
		assert.False(t, ok, "the member is still there")
		_, ok = r.observeReleaseWorkloadsGone(context.Background(), md, record)
		assert.False(t, ok, "the workload is still there")

		// The member and its Workload leave, but the published ledger still shows the card
		// held: the accelerator half holds until the ledger catches up.
		require.NoError(t, cli.Delete(context.Background(), member))
		require.NoError(t, cli.Delete(context.Background(),
			allocationWorkload("wl-1", "wl-uid-1", "m1", "uid-1")))
		_, ok = r.observeReleaseTargetsAbsent(context.Background(), record)
		assert.True(t, ok, "the member is gone")
		_, ok = r.observeReleaseWorkloadsGone(context.Background(), md, record)
		assert.True(t, ok, "the workload is gone")
		_, ok = r.observeReleaseAcceleratorsReturned(context.Background(), record)
		assert.False(t, ok, "the published ledger still shows the card held")

		// The ledger catches up, and the whole release reads released.
		allocationPublishLedger(t, cli, devs)
		_, ok = r.observeReleaseAcceleratorsReturned(context.Background(), record)
		assert.True(t, ok, "the ledger now agrees with the node's records")
	})
}

// uncachedReadCounter is the uncached reader the read-amplification contracts run against. It
// counts every read it serves, so a case can assert how many round trips a pass costs, and it
// can move the objects behind those reads part way through, so a case can say "the claim
// changes on the fourth read" and observe what the reconciler does about it.
//
// The count is the point of the reader: a reconciler that reaches the same answer through a
// cached object is cheaper but is answering about a different state, and these cases exist to
// keep the answer honest while making it cheaper.
type uncachedReadCounter struct {
	ctrlcli.Reader
	gets, lists int
	// beforeRead runs before every read, with the number of reads already served and the object
	// about to be filled. A case that needs the world to move mid-collection mutates the client
	// from here, on the one read it chose.
	beforeRead func(served int, obj any)
}

func (c *uncachedReadCounter) Get(
	ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption,
) error {
	if c.beforeRead != nil {
		c.beforeRead(c.gets+c.lists, obj)
	}
	c.gets++

	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c *uncachedReadCounter) List(
	ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption,
) error {
	if c.beforeRead != nil {
		c.beforeRead(c.gets+c.lists, list)
	}
	c.lists++

	return c.Reader.List(ctx, list, opts...)
}

// reads is the whole uncached cost of one pass: every Get and every List, however many times
// either object was asked for.
func (c *uncachedReadCounter) reads() int { return c.gets + c.lists }

// groupedAllocationDevices is the node ledger the grouped accounting cases run against: five
// cards, four of which the members below allocate and one of which stays free so a member can
// claim a card the published accounting never allocated.
func groupedAllocationDevices() *workercore.Devices {
	return releaseDevices(func(devs *workercore.Devices) {
		devs.Spec.Groups[0].Accelerators = append(devs.Spec.Groups[0].Accelerators,
			workercore.Accelerator{ID: "GPU-def", Index: 1},
			workercore.Accelerator{ID: "GPU-ghi", Index: 5},
			workercore.Accelerator{ID: "GPU-jkl", Index: 6},
			workercore.Accelerator{ID: "GPU-sss", Index: 9},
		)
	})
}

// allocationCardMember is one seated member claiming exactly the card named, so a case states
// which member holds which accelerator without restating the allocator's record shape.
func allocationCardMember(
	t *testing.T, md *workercore.ModelDeployment, name, uid string, ordinal int,
	cardID string, index uint32, mutate ...func(*core.Pod),
) *core.Pod {
	t.Helper()

	return allocationMember(t, md, name, uid, ordinal, func(pod *core.Pod) {
		pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = allocationCardAnnotation(
			t, modelDeploymentMainContainerName, cardID, index,
			workercore.DeviceAllocationModeExclusive, nodefeature.ResourceMaxUnits)
		for _, m := range mutate {
			m(pod)
		}
	})
}

// TestObserveModelDeploymentElasticAllocation_GroupedLedgerReads proves the grouping changes how
// many times the ledger is read and nothing else: four members on one node reach the same answer
// as four independent verifications would, out of one node read instead of four.
//
// THE COUNT IS THE CONTRACT. One workload listing, one fresh read per member and ONE
// bookended pass over the node -- the ledger, the node's pods, and the same two again -- is
// nine reads. A pass that verified each member against its own node read would spend twenty-one,
// which is the cost this issue set out to remove, and a pass that spent more than nine has added
// a read nobody asked for.
func TestObserveModelDeploymentElasticAllocation_GroupedLedgerReads(t *testing.T) {
	md := allocationDeployment()
	devs := groupedAllocationDevices()
	members := []*core.Pod{
		allocationCardMember(t, md, "m1", "uid-1", 1, "GPU-abc", 0),
		allocationCardMember(t, md, "m2", "uid-2", 2, "GPU-def", 1),
		allocationCardMember(t, md, "m3", "uid-3", 3, "GPU-ghi", 5),
		allocationCardMember(t, md, "m4", "uid-4", 4, "GPU-jkl", 6),
	}
	supplied := make([]core.Pod, 0, len(members))
	for _, member := range members {
		supplied = append(supplied, *member)
	}

	cli := newModelDeploymentClient(allocationObjects(md, devs, members)...)
	allocationPublishLedger(t, cli, devs)
	reader := &uncachedReadCounter{Reader: cli}
	r := &ModelDeploymentReconciler{Client: cli, APIReader: reader}

	_, allocated := r.observeModelDeploymentElasticAllocation(context.Background(), md, supplied)

	require.True(t, allocated.Known, "reason: %s", allocated.Reason)
	assert.Equal(t, len(members), allocated.Value)
	assert.Empty(t, allocated.Reason)
	assert.Equal(t, 9, reader.reads(),
		"one workload listing, one read per member, one bookended pass over the node")
}

// TestObserveModelDeploymentElasticAllocation_GroupedAccountingKeepsIndividualUnknowns proves
// the grouping did not turn into a vote. Four members share one node read, one of their cards
// disagrees with the node's published accounting, and the layer holds on THAT member's reason
// alone: the other three are still counted, and none of them is dragged down with it.
func TestObserveModelDeploymentElasticAllocation_GroupedAccountingKeepsIndividualUnknowns(t *testing.T) {
	md := allocationDeployment()
	devs := groupedAllocationDevices()
	agreeing := []*core.Pod{
		allocationCardMember(t, md, "m1", "uid-1", 1, "GPU-abc", 0),
		allocationCardMember(t, md, "m2", "uid-2", 2, "GPU-def", 1),
		allocationCardMember(t, md, "m3", "uid-3", 3, "GPU-ghi", 5),
	}
	// THE FOURTH MEMBER HOLDS NOTHING WHEN THE LEDGER IS PUBLISHED, and claims a free card
	// afterwards. The card is in the node's inventory, so this is not an unknown device: it is a
	// card the published accounting says nobody holds, which is exactly the disagreement one
	// member can carry while its neighbors are clean.
	latecomer := allocationCardMember(t, md, "m4", "uid-4", 4, "GPU-jkl", 6,
		func(pod *core.Pod) { pod.Annotations = nil })
	seated := make([]*core.Pod, 0, len(agreeing)+1)
	seated = append(seated, agreeing...)
	seated = append(seated, latecomer)
	cli := newModelDeploymentClient(allocationObjects(md, devs, seated)...)
	allocationPublishLedger(t, cli, devs)

	claimKey := deviceplugin.AllocatedAcceleratorAnnoKey
	held := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "m4"}, held))
	held.Annotations = map[string]string{claimKey: allocationCardAnnotation(
		t, modelDeploymentMainContainerName, "GPU-sss", 9,
		workercore.DeviceAllocationModeExclusive, nodefeature.ResourceMaxUnits)}
	require.NoError(t, cli.Update(context.Background(), held))
	// THE SUPPLIED MEMBERS ARE THE THREE THAT AGREED PLUS THE CLAIM THE SERVER NOW HOLDS. The
	// latecomer fixture is the same Pod before its claim moved, so it is not supplied twice.
	supplied := make([]core.Pod, 0, len(agreeing)+1)
	for _, member := range append(agreeing[:len(agreeing):len(agreeing)], held) {
		supplied = append(supplied, *member)
	}

	reader := &uncachedReadCounter{Reader: cli}
	r := &ModelDeploymentReconciler{Client: cli, APIReader: reader}

	_, allocated := r.observeModelDeploymentElasticAllocation(context.Background(), md, supplied)

	require.False(t, allocated.Known, "one disagreeing card holds the allocation layer")
	// THE REFUSAL NAMES THE DISAGREEING MEMBER'S CARD, and no other. A grouped read that handed
	// every member on the node one verdict would fail here by naming a neighbour's card instead.
	assert.Contains(t, allocated.Reason, "GPU-sss")
	for _, member := range agreeing {
		assert.NotContains(t, allocated.Reason, allocationCardOf(t, member).DeviceID,
			"a member whose own card agrees was refused by its neighbour's disagreement")
	}
	assert.Equal(t, 9, reader.reads(),
		"the node is read once per pass whether its members agree or not")
}

// allocationObjects collects a case's deployment, its node ledger and its members into the one
// object list the fake server is built from, so a case names its members once and hands them
// over in the order it declared them.
func allocationObjects(md *workercore.ModelDeployment, devs *workercore.Devices, members []*core.Pod) []ctrlcli.Object {
	objs := make([]ctrlcli.Object, 0, len(members)+2)
	objs = append(objs, md, devs)
	for _, member := range members {
		objs = append(objs, member)
	}

	return objs
}

// allocationCardOf reads the single accelerator a member's record names, for the assertions that
// name which member a refusal belongs to.
func allocationCardOf(t *testing.T, pod *core.Pod) modelDeploymentRetirementReleaseCard {
	t.Helper()

	cards, reason, held := elasticWholeCardsOf(pod, 1)
	require.True(t, held, "member %q holds no whole card: %s", pod.Name, reason)

	return cards[0]
}

// TestObserveModelDeploymentElasticAllocation_GroupedReadsHoldWhenTheNodeMoves proves the node
// read is still bookended now that it is shared. A member's claim changes after the ledger has
// been read and rebuilt, so the closing pod list disagrees with the opening one, and the pass
// holds rather than answering from the state it started with.
func TestObserveModelDeploymentElasticAllocation_GroupedReadsHoldWhenTheNodeMoves(t *testing.T) {
	md := allocationDeployment()
	devs := groupedAllocationDevices()
	members := []*core.Pod{
		allocationCardMember(t, md, "m1", "uid-1", 1, "GPU-abc", 0),
		allocationCardMember(t, md, "m2", "uid-2", 2, "GPU-def", 1),
	}
	supplied := make([]core.Pod, 0, len(members))
	for _, member := range members {
		supplied = append(supplied, *member)
	}

	cli := newModelDeploymentClient(allocationObjects(md, devs, members)...)
	allocationPublishLedger(t, cli, devs)

	// THE SECOND MEMBER'S CLAIM MOVES ONCE, BETWEEN THE NODE'S TWO POD LISTINGS. The mutation is
	// applied to the server's copy of the Pod, which is what the node's own listing reads, so the
	// closing bookend sees a different node than the opening one did. Two listings have been
	// served by then -- the namespace's Workloads and the node's opening pod list -- so the next
	// read is that closing bookend and nothing else.
	moved, podLists := false, 0
	reader := &uncachedReadCounter{
		Reader: cli,
		beforeRead: func(_ int, obj any) {
			if _, isPodList := obj.(*core.PodList); !isPodList {
				return
			}
			podLists++
			if moved || podLists != 2 {
				return
			}
			moved = true
			live := new(core.Pod)
			require.NoError(t, cli.Get(context.Background(),
				ctrlcli.ObjectKey{Namespace: md.Namespace, Name: "m2"}, live))
			live.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = allocationCardAnnotation(
				t, modelDeploymentMainContainerName, "GPU-jkl", 6,
				workercore.DeviceAllocationModeExclusive, nodefeature.ResourceMaxUnits)
			require.NoError(t, cli.Update(context.Background(), live))
		},
	}
	r := &ModelDeploymentReconciler{Client: cli, APIReader: reader}

	_, allocated := r.observeModelDeploymentElasticAllocation(context.Background(), md, supplied)

	require.True(t, moved, "the case did not move the claim, so it proves nothing")
	require.False(t, allocated.Known, "a node that moved under its own read must hold")
	assert.Contains(t, allocated.Reason, "changed while their ledger was being read")
}

// TestObserveModelDeploymentElasticAllocation_WideGroupingHoldsAtTheProfileMaximum measures the
// allocation layer at the widest width the elastic profile supports, which is the width the
// reconcile hot path requeues every fifteen seconds.
//
// THE LEDGER COST IS THE SAME AT WIDTH 2 AND AT WIDTH 64: four reads for the node, whatever
// seats on it. A pass that verified each member against its own node read spent four per member,
// so this width cost 256 reads of the ledger alone against the 4 it costs now -- 69 uncached
// reads in total against 321. The elapsed time is reported rather than asserted, because a
// duration is a property of the host and not a contract; the read count is the contract.
func TestObserveModelDeploymentElasticAllocation_WideGroupingHoldsAtTheProfileMaximum(t *testing.T) {
	const width = elasticprofile.WidthMax

	md := allocationDeployment()
	devs := releaseDevices()
	members := make([]*core.Pod, 0, width)
	supplied := make([]core.Pod, 0, width)
	for i := range width {
		// THE INDEXES START ABOVE THE ONE CARD THE SHARED LEDGER FIXTURE ALREADY CARRIES, because
		// a group may not declare one index twice.
		cardID, index := fmt.Sprintf("GPU-wide-%02d", i), uint32(i+1)
		devs.Spec.Groups[0].Accelerators = append(devs.Spec.Groups[0].Accelerators,
			workercore.Accelerator{ID: cardID, Index: index})
		name := fmt.Sprintf("m%02d", i)
		member := allocationCardMember(t, md, name, "uid-"+name, i+1, cardID, index)
		members = append(members, member)
		supplied = append(supplied, *member)
	}

	cli := newModelDeploymentClient(allocationObjects(md, devs, members)...)
	allocationPublishLedger(t, cli, devs)
	reader := &uncachedReadCounter{Reader: cli}
	r := &ModelDeploymentReconciler{Client: cli, APIReader: reader}

	started := time.Now()
	_, allocated := r.observeModelDeploymentElasticAllocation(context.Background(), md, supplied)
	elapsed := time.Since(started)

	require.True(t, allocated.Known, "reason: %s", allocated.Reason)
	assert.Equal(t, width, allocated.Value)
	// ONE WORKLOAD LISTING, ONE FRESH READ PER MEMBER, ONE BOOKENDED PASS OVER THE ONE NODE.
	assert.Equal(t, 1+width+4, reader.reads())
	t.Logf("width %d: %d uncached reads in %s", width, reader.reads(), elapsed)
}
