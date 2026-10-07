package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestElasticTPAllocation(t *testing.T) {
	cases := []struct {
		name    string
		ids     []string
		units   []int32
		sidecar string
		valid   bool
	}{
		{"complete group", []string{"GPU-abc", "GPU-def"}, []int32{nodefeature.ResourceMaxUnits, nodefeature.ResourceMaxUnits}, "", true},
		{"empty sibling record", []string{"GPU-abc", "GPU-def"}, []int32{nodefeature.ResourceMaxUnits, nodefeature.ResourceMaxUnits}, "cpu", true},
		{"missing card", []string{"GPU-abc"}, []int32{nodefeature.ResourceMaxUnits}, "", false},
		{"duplicate card", []string{"GPU-abc", "GPU-abc"}, []int32{nodefeature.ResourceMaxUnits, nodefeature.ResourceMaxUnits}, "", false},
		{"partial second card", []string{"GPU-abc", "GPU-def"}, []int32{nodefeature.ResourceMaxUnits, 1}, "", false},
		{"extra container", []string{"GPU-abc", "GPU-def"}, []int32{nodefeature.ResourceMaxUnits, nodefeature.ResourceMaxUnits}, "gpu", false},
		{"extra card", []string{"GPU-abc", "GPU-def", "GPU-ghi"}, []int32{nodefeature.ResourceMaxUnits, nodefeature.ResourceMaxUnits, nodefeature.ResourceMaxUnits}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := allocationDeployment()
			md.Spec.Roles[0].ElasticEP.TensorParallelSize = ptr.To[int32](2)
			pod := allocationMember(t, md, "m1", "uid-1", 1)
			allocations := deviceplugin.PodAllocations{}
			main := workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{ID: "gpu-0", Manufacturer: "NVIDIA"}}}
			for i, id := range tc.ids {
				main.Groups[0].Accelerators = append(main.Groups[0].Accelerators, workercore.AcceleratorAllocation{ID: id, Index: uint32(i), Mode: workercore.DeviceAllocationModeExclusive, Allocated: tc.units[i]})
			}
			existing, err := deviceplugin.AllocatedAcceleratorsOf(pod)
			require.NoError(t, err)
			allocation := existing[modelDeploymentMainContainerName]
			allocation.Devices = main
			allocations[modelDeploymentMainContainerName] = allocation
			switch tc.sidecar {
			case "gpu":
				allocations["sidecar"] = allocation
			case "cpu":
				empty := allocation
				empty.Devices = workercore.DevicesStatus{}
				allocations["sidecar"] = empty
			}
			pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = mustJSON(t, allocations)
			cards, reason, held := elasticWholeCardsOf(pod, 2)
			assert.Equal(t, tc.valid, held, reason)
			if tc.valid {
				require.Len(t, cards, 2)
			} else {
				assert.NotEmpty(t, reason)
			}
		})
	}
}

func TestElasticTPAllocationCountsMembers(t *testing.T) {
	md := allocationDeployment()
	md.Spec.Roles[0].ElasticEP.TensorParallelSize = ptr.To[int32](2)
	pod := allocationMember(t, md, "m1", "uid-1", 1)
	allocations, err := deviceplugin.AllocatedAcceleratorsOf(pod)
	require.NoError(t, err)
	allocation := allocations[modelDeploymentMainContainerName]
	allocation.Devices.Groups[0].Accelerators = append(allocation.Devices.Groups[0].Accelerators,
		workercore.AcceleratorAllocation{ID: "GPU-def", Index: 1, Mode: workercore.DeviceAllocationModeExclusive, Allocated: nodefeature.ResourceMaxUnits})
	allocations[modelDeploymentMainContainerName] = allocation
	pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = mustJSON(t, allocations)
	devs := allocationDevices()
	cli := newModelDeploymentClient(md, pod, devs)
	allocationPublishLedger(t, cli, devs)
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli}
	_, allocated := r.observeModelDeploymentElasticAllocation(context.Background(), md, []core.Pod{*pod})
	require.True(t, allocated.Known, allocated.Reason)
	assert.Equal(t, 1, allocated.Value)

	published := new(workercore.Devices)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: devs.Name}, published))
	published.Status.Groups[0].Accelerators[1].Allocated--
	require.NoError(t, cli.Status().Update(context.Background(), published))
	_, allocated = r.observeModelDeploymentElasticAllocation(context.Background(), md, []core.Pod{*pod})
	assert.False(t, allocated.Known, "a disagreement on the second card must hold the entire member")
}

func TestElasticTPRayPlacement(t *testing.T) {
	cases := []struct {
		name     string
		quantity string
		bundles  []elasticRayBundleWire
		valid    bool
	}{
		{"complete TP group", "2", []elasticRayBundleWire{{BundleIndex: ptr.To(0), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "1"}}, {BundleIndex: ptr.To(1), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "1"}}}, true},
		{"CPU control bundle", "2", []elasticRayBundleWire{{BundleIndex: ptr.To(0), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "1"}}, {BundleIndex: ptr.To(1), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "1"}}, {BundleIndex: ptr.To(2), NodeIDHex: "33", UnitResources: map[string]string{"CPU": "1"}}}, true},
		{"cross node", "2", []elasticRayBundleWire{{BundleIndex: ptr.To(0), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "1"}}, {BundleIndex: ptr.To(1), NodeIDHex: "44", UnitResources: map[string]string{"GPU": "1"}}}, false},
		{"missing resources", "2", []elasticRayBundleWire{{BundleIndex: ptr.To(0), NodeIDHex: "33"}}, false},
		{"missing bundle", "2", []elasticRayBundleWire{{BundleIndex: ptr.To(0), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "1"}}}, false},
		{"duplicate index", "2", []elasticRayBundleWire{{BundleIndex: ptr.To(0), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "1"}}, {BundleIndex: ptr.To(0), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "1"}}}, false},
		{"fractional bundle", "2", []elasticRayBundleWire{{BundleIndex: ptr.To(0), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "0.5"}}, {BundleIndex: ptr.To(1), NodeIDHex: "33", UnitResources: map[string]string{"GPU": "1"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := observerHappyDocument(t)
			for i := range *doc.Nodes {
				if i > 0 {
					(*doc.Nodes)[i].Resources = map[string]string{"GPU": tc.quantity}
				}
			}
			for i := range *doc.PlacementGroups {
				bundles := make([]elasticRayBundleWire, len(tc.bundles))
				copy(bundles, tc.bundles)
				if i == 1 {
					for j := range bundles {
						bundles[j].NodeIDHex = "44"
					}
				}
				(*doc.PlacementGroups)[i].Bundles = bundles
			}
			obs := joinModelDeploymentElasticRayIdentity(doc, observerMembers(), 2, 2)
			assert.Equal(t, tc.valid, obs.WidthKnown, obs.WidthUnknownReason)
			require.True(t, obs.RegisteredGPUKnown, obs.RegisteredGPUUnknownReason)
			assert.Equal(t, 3, obs.RegisteredGPU)
		})
	}
}

func TestElasticTPRegisteredCapacity(t *testing.T) {
	cases := []struct {
		name, quantity string
		present, valid bool
	}{
		{"two GPUs", "2", true, true},
		{"one GPU", "1", true, false},
		{"three GPUs", "3", true, false},
		{"fractional", "1.5", true, false},
		{"nonfinite", "NaN", true, false},
		{"missing", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := observerHappyDocument(t)
			for i := range *doc.Nodes {
				if i > 0 {
					(*doc.Nodes)[i].Resources = map[string]string{}
					if tc.present {
						(*doc.Nodes)[i].Resources["GPU"] = tc.quantity
					}
				}
			}
			obs := joinModelDeploymentElasticRayIdentity(doc, observerMembers(), 2, 2)
			assert.Equal(t, tc.valid, obs.RegisteredGPUKnown, obs.RegisteredGPUUnknownReason)
			if tc.valid {
				assert.Equal(t, 3, obs.RegisteredGPU)
			}
		})
	}
}
