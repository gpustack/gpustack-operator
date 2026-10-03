package deviceplugin

import (
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// The strict rebuild exists so a retirement predicate can be told "I could not read this" instead of
// being handed a ledger with a holder missing from it. Each case below is one input the ordinary
// rebuild absorbs silently and the strict one must refuse.

// releaseTestDevices is a one-group, one-card inventory: the smallest thing the strict path can say
// something definite about.
func releaseTestDevices(mutate ...func(*workercore.Devices)) *workercore.Devices {
	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: "node-a"},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID: "gpu-0", Manufacturer: "NVIDIA", Name: "H20", Cores: 78, Memory: 96,
				Accelerators: []workercore.Accelerator{{ID: "GPU-abc", Index: 0}},
			}},
		},
	}
	for _, m := range mutate {
		m(devs)
	}

	return devs
}

// releaseTestPod is a pod on a node holding one card, expressed through the real annotation shape
// the allocator writes, so a case that invalidates this format invalidates a real one.
func releaseTestPod(mutate ...func(*core.Pod)) *core.Pod {
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "qwen-server-one", Namespace: "team-a", UID: "pod-1",
			Labels: map[string]string{"app": "qwen"},
		},
		Spec: core.PodSpec{
			NodeName: "node-a",
			Containers: []core.Container{{
				Name:  "vllm",
				Image: "vllm/vllm-openai:v0.25.1",
			}},
		},
	}
	// The annotation map exists before the mutators run, so a case that wants to write malformed
	// JSON can write malformed JSON rather than assigning into a nil map.
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	for _, m := range mutate {
		m(pod)
	}
	if _, already := pod.Annotations[AllocatedAcceleratorAnnoKey]; !already {
		record := PodAllocations{"vllm": releaseTestAllocation()}
		raw, err := json.Marshal(record)
		if err != nil {
			panic(err)
		}
		pod.Annotations[AllocatedAcceleratorAnnoKey] = string(raw)
	}

	return pod
}

func releaseTestAllocation() ContainerAllocation {
	return ContainerAllocation{
		Devices: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID: "gpu-0", Manufacturer: "NVIDIA",
				Accelerators: []workercore.AcceleratorAllocation{{
					ID: "GPU-abc", Index: 0,
					Mode:      workercore.DeviceAllocationModeExclusive,
					Allocated: 1,
				}},
			}},
		},
		DeviceIDs: []string{"GPU-abc"},
	}
}

func withAllocation(mutate ...func(map[string]ContainerAllocation)) func(*core.Pod) {
	return func(pod *core.Pod) {
		record := releaseTestAllocation()
		entries := map[string]ContainerAllocation{"vllm": record}
		for _, m := range mutate {
			m(entries)
		}
		raw, err := json.Marshal(entries)
		if err != nil {
			panic(err)
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[AllocatedAcceleratorAnnoKey] = string(raw)
	}
}

func TestBuildDesiredStatusStrict_Rejects(t *testing.T) {
	testCases := []struct {
		name     string
		devs     *workercore.Devices
		pods     []*core.Pod
		wantErrs []string
	}{
		{
			name: "a valid single holder is accepted",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod()},
		},
		{
			name: "a pod with no record at all is not an error",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(func(pod *core.Pod) {
				delete(pod.Annotations, AllocatedAcceleratorAnnoKey)
			})},
		},
		{
			name: "malformed JSON is refused rather than read as no allocation",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(func(pod *core.Pod) {
				pod.Annotations[AllocatedAcceleratorAnnoKey] = "{not json"
			})},
			wantErrs: []string{"read its allocation record"},
		},
		{
			name: "a null container record is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(func(pod *core.Pod) {
				pod.Annotations[AllocatedAcceleratorAnnoKey] = `{"vllm":null}`
			})},
			wantErrs: []string{"is null"},
		},
		{
			name: "a record naming a container the pod does not declare is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				entries["ghost"] = releaseTestAllocation()
			}))},
			wantErrs: []string{"declares no such container"},
		},
		{
			name: "an unknown group is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].ID = "gpu-9"
				entries["vllm"] = record
			}))},
			wantErrs: []string{"which the inventory does not have"},
		},
		{
			name: "a wrong manufacturer is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Manufacturer = "AMD"
				entries["vllm"] = record
			}))},
			wantErrs: []string{"claims manufacturer"},
		},
		{
			name: "an unknown card is refused rather than skipped by the merge",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Accelerators[0].ID = "GPU-zzz"
				entries["vllm"] = record
			}))},
			wantErrs: []string{"which the inventory does not have"},
		},
		{
			name: "an index that disagrees with the inventory is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Accelerators[0].Index = 7
				entries["vllm"] = record
			}))},
			wantErrs: []string{"the inventory says"},
		},
		{
			name: "a duplicate card inside one record is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Accelerators = append(
					record.Devices.Groups[0].Accelerators,
					workercore.AcceleratorAllocation{
						ID: "GPU-abc", Index: 0,
						Mode:      workercore.DeviceAllocationModeExclusive,
						Allocated: 1,
					})
				entries["vllm"] = record
			}))},
			wantErrs: []string{"twice"},
		},
		{
			name: "a mode the operator never writes is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Accelerators[0].Mode = workercore.DeviceAllocationModeNone
				entries["vllm"] = record
			}))},
			wantErrs: []string{"not one this operator writes"},
		},
		{
			name: "the internal visibility mode is refused as a recorded claim",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Accelerators[0].Mode = workercore.DeviceAllocationModeVisibility
				entries["vllm"] = record
			}))},
			wantErrs: []string{"not one this operator writes"},
		},
		{
			name: "negative units are refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Accelerators[0].Allocated = -1
				entries["vllm"] = record
			}))},
			wantErrs: []string{"negative units"},
		},
		{
			name: "more units than a card holds are refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Accelerators[0].Allocated = 99_000_000
				entries["vllm"] = record
			}))},
			wantErrs: []string{"more than the"},
		},
		{
			name: "a placement without a profile is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Accelerators[0].AllocatedPhysicalPlacements = []workercore.AcceleratorPlacement{{
					Start: 0, Length: 1,
				}}
				entries["vllm"] = record
			}))},
			wantErrs: []string{"placement intervals but no profile"},
		},
		{
			name: "a profile without a placement is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(withAllocation(func(entries map[string]ContainerAllocation) {
				record := releaseTestAllocation()
				record.Devices.Groups[0].Accelerators[0].AllocatedPhysicalProfile = "1g.10gb"
				entries["vllm"] = record
			}))},
			wantErrs: []string{"but no placement intervals"},
		},
		{
			name: "a claim on a pod that is on no node is refused",
			devs: releaseTestDevices(),
			pods: []*core.Pod{releaseTestPod(func(pod *core.Pod) {
				pod.Spec.NodeName = ""
			})},
			wantErrs: []string{"not on a node"},
		},
		{
			name: "a duplicated card in the inventory is refused before any pod is read",
			devs: releaseTestDevices(func(devs *workercore.Devices) {
				devs.Spec.Groups[0].Accelerators = append(
					devs.Spec.Groups[0].Accelerators, workercore.Accelerator{ID: "GPU-abc", Index: 0})
			}),
			pods:     []*core.Pod{releaseTestPod()},
			wantErrs: []string{"declared twice in group"},
		},
		{
			name: "a duplicated group in the inventory is refused",
			devs: releaseTestDevices(func(devs *workercore.Devices) {
				devs.Spec.Groups = append(devs.Spec.Groups, devs.Spec.Groups[0])
			}),
			pods:     []*core.Pod{releaseTestPod()},
			wantErrs: []string{"declared twice"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podList := new(core.PodList)
			for _, pod := range tc.pods {
				podList.Items = append(podList.Items, *pod)
			}

			status, _, err := BuildDesiredStatusStrict(logr.Discard(), tc.devs, podList)

			if len(tc.wantErrs) == 0 {
				require.NoError(t, err, "a well-formed record must not be refused")
				assert.NotEmpty(t, status.Groups, "and the rebuild produced the ledger the merge would have")

				return
			}
			require.Error(t, err, "the strict path must refuse what the ordinary one absorbs")
			for _, want := range tc.wantErrs {
				assert.Contains(t, err.Error(), want)
			}
			assert.Nil(t, status.Groups, "no ledger is returned with a refusal, so a caller cannot "+
				"mistake a dropped holder for a free card")
		})
	}
}

// TestBuildDesiredStatusStrict_KeepsTerminatingAndOtherNamespaces is the positive that the strict
// path did not become narrower than the rebuild it wraps. A terminating pod and a pod in another
// namespace are both still charged, and neither is an error.
func TestBuildDesiredStatusStrict_KeepsTerminatingAndOtherNamespaces(t *testing.T) {
	deleting := releaseTestPod()
	now := meta.Now()
	deleting.DeletionTimestamp = &now
	other := releaseTestPod(func(pod *core.Pod) {
		pod.Name = "qwen-server-two"
		pod.UID = "pod-2"
		pod.Namespace = "team-b"
		pod.Annotations[AllocatedAcceleratorAnnoKey] = releaseTestPod().Annotations[AllocatedAcceleratorAnnoKey]
	})
	terminal := releaseTestPod(func(pod *core.Pod) {
		pod.Name = "qwen-server-three"
		pod.UID = "pod-3"
		pod.Status.Phase = core.PodSucceeded
	})

	podList := new(core.PodList)
	for _, pod := range []*core.Pod{deleting, other, terminal} {
		podList.Items = append(podList.Items, *pod)
	}

	status, liveUIDs, err := BuildDesiredStatusStrict(logr.Discard(), releaseTestDevices(), podList)

	require.NoError(t, err, "a terminating, terminal and cross-namespace holder are all legitimate")
	assert.Len(t, liveUIDs, 3, "all three are in the live set, so none reads free while it still holds the card")
	require.Len(t, status.Groups, 1)
	require.Len(t, status.Groups[0].Accelerators, 1)
	// The card is charged: its remaining capacity is below a whole card. That is the fact the
	// strict path has to preserve, and it is asserted on Remaining rather than on Allocated because
	// Allocated is a derived field of the fold while Remaining is what a later release moves.
	assert.Less(t, status.Groups[0].Accelerators[0].Remaining, int32(nodefeature.ResourceMaxUnits),
		"a card three holders are carved from is not whole, whatever the fold derived")
}

// TestBuildDesiredStatusStrict_PreservesTheOrdinaryRebuild is the check that the strict path is the
// same arithmetic and not a second implementation of it.
func TestBuildDesiredStatusStrict_PreservesTheOrdinaryRebuild(t *testing.T) {
	devs := releaseTestDevices()
	podList := new(core.PodList)
	podList.Items = append(podList.Items, *releaseTestPod())

	want, wantLive := BuildDesiredStatus(logr.Discard(), devs, podList)
	got, gotLive, err := BuildDesiredStatusStrict(logr.Discard(), devs, podList)

	require.NoError(t, err)
	assert.Equal(t, want, got, "the strict rebuild agrees with the ordinary one on valid input")
	assert.Equal(t, wantLive, gotLive)
}

// TestATopLevelNullAllocationRecordIsRejected pins that a whole-record JSON null is an invalid
// record rather than "no record at all": json.Unmarshal decodes null into a nil map without
// error, which would otherwise bypass the explicit-null hold.
func TestATopLevelNullAllocationRecordIsRejected(t *testing.T) {
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:        "holder",
			Namespace:   "default",
			Annotations: map[string]string{AllocatedAcceleratorAnnoKey: "null"},
		},
	}
	err := validatePodAllocationRecord(pod, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allocation record is null")
}
