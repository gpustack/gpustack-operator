package mock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeletdeviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	detectormock "gpustack.ai/gpustack/pkg/devicemanager/detector/mock"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

var _ detectormock.PartitionProvider = (*PartitionStore)(nil)

func mustBuildPresetDevices(t *testing.T, nodeName, presetName string, count int) *workercore.Devices {
	t.Helper()
	raw := fmt.Sprintf("preset: %s\ncount: %d\n", presetName, count)
	doc, err := mockspec.Parse([]byte(raw))
	require.NoError(t, err)
	resolved := doc.ResolveNode(nodeName)
	names := resolved.ManufacturerNames()
	require.Len(t, names, 1)
	groups := resolved.Manufacturers[names[0]].BuildGroups()
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec:       workercore.DevicesSpec{Groups: groups},
	}
}

func newPartitionPod(
	nodeName, podName, mfr, profile string, instances int64, withVisibility bool,
) *core.Pod {
	partRes := nodefeature.GetAcceleratableResourceName(mfr, workercore.DeviceAllocationModePartitioned)
	profRes := nodefeature.GetAcceleratablePartitionedProfileResourceName(mfr, profile)
	qty := *resource.NewQuantity(instances, resource.DecimalSI)

	ctrs := []core.Container{{
		Name: "main",
		Resources: core.ResourceRequirements{
			Limits: core.ResourceList{
				partRes: qty,
				profRes: qty,
			},
		},
	}}
	if withVisibility {
		visRes := nodefeature.GetAcceleratableResourceName(mfr, workercore.DeviceAllocationModeVisibility)
		ctrs = append(ctrs, core.Container{
			Name: "ssh",
			Resources: core.ResourceRequirements{
				Limits: core.ResourceList{
					visRes: qty,
				},
			},
		})
	}

	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      podName,
			Namespace: "default",
			UID:       types.UID("uid-" + podName),
		},
		Spec: core.PodSpec{
			NodeName:   nodeName,
			Containers: ctrs,
		},
	}
}

func TestPartitionStore_PlacementsOverlapConcurrencyAndInspection(t *testing.T) {
	devs := mustBuildPresetDevices(t, "node-mig", "nvidia-h100-mig", 1)
	grp := &devs.Spec.Groups[0]
	acc := &grp.Accelerators[0]

	store := NewPartitionStore()
	assert.Equal(t, 0, store.Count())

	// Concurrent multi-profile creation on the same H100 card:
	// - 3g.40gb -> [0, 4) or [4, 8)
	// - 2g.20gb -> [0, 2), [2, 4), or [4, 6)
	// - 1g.10gb -> [0, 1)..[6, 7)
	// Reserve 3g.40gb and 2g.20gb concurrently (both scheduling orders leave room for 1g.10gb,
	// avoiding the [0,4)+[4,5) fragmentation order that legitimately blocks 2g.20gb), then add 1g.10gb.
	concurrentProfiles := []string{"3g.40gb", "2g.20gb"}
	results := make([]VirtualPartition, 3)
	errs := make([]error, 3)

	var wg sync.WaitGroup
	for i, prof := range concurrentProfiles {
		wg.Add(1)
		go func(idx int, p string) {
			defer wg.Done()
			vp, rebound, err := store.Reserve(
				nodefeature.ManufacturerNVIDIA,
				fmt.Sprintf("pod-%d", idx),
				"main",
				grp,
				acc,
				p,
			)
			assert.False(t, rebound)
			results[idx] = vp
			errs[idx] = err
		}(i, prof)
	}
	wg.Wait()

	vp1g, rebound1g, err1g := store.Reserve(
		nodefeature.ManufacturerNVIDIA,
		"pod-2",
		"main",
		grp,
		acc,
		"1g.10gb",
	)
	assert.False(t, rebound1g)
	results[2] = vp1g
	errs[2] = err1g

	profiles := []string{"3g.40gb", "2g.20gb", "1g.10gb"}
	for i, err := range errs {
		require.NoError(t, err, "profile %s should fit concurrently", profiles[i])
		assert.NotEmpty(t, results[i].ID)
	}
	assert.Equal(t, 3, store.Count())

	// Verify zero overlap among all live placements on the card.
	byCard := store.ListByCard(nodefeature.ManufacturerNVIDIA, acc.ID)
	require.Len(t, byCard, 3)
	for i := 0; i < len(byCard); i++ {
		for j := i + 1; j < len(byCard); j++ {
			a := byCard[i].Placement
			b := byCard[j].Placement
			overlaps := a.Start < b.Start+b.Length && b.Start < a.Start+a.Length
			assert.False(t, overlaps, "placements %+v and %+v must not overlap", a, b)
		}
	}

	// Verify overlapping whole-card 7g.80gb [0, 8) and 4g.40gb [0, 4) are refused due to mutual exclusion.
	_, _, err := store.Reserve(nodefeature.ManufacturerNVIDIA, "pod-full", "main", grp, acc, "7g.80gb")
	require.Error(t, err)
	assert.ErrorContains(t, err, "no free placement")

	// Idempotent retry for pod-0 with matching profile returns rebound=true and exact same ID.
	reboundPart, rebound, err := store.Reserve(nodefeature.ManufacturerNVIDIA, "pod-0", "main", grp, acc, "3g.40gb")
	require.NoError(t, err)
	assert.True(t, rebound)
	assert.Equal(t, results[0].ID, reboundPart.ID)
	assert.Equal(t, 3, store.Count())

	// Conflicting profile retry for pod-0 fails closed with rebound=false.
	_, conflictRebound, err := store.Reserve(nodefeature.ManufacturerNVIDIA, "pod-0", "main", grp, acc, "1g.10gb")
	require.Error(t, err)
	assert.False(t, conflictRebound)
	assert.ErrorContains(t, err, "profile")

	// Inspection helpers return accurate snapshots and defensive copies.
	gotByID, ok := store.Get(results[0].ID)
	require.True(t, ok)
	assert.Equal(t, results[0], gotByID)

	gotByOwner, ok := store.GetByOwner(nodefeature.ManufacturerNVIDIA, "pod-0", "main", acc.ID)
	require.True(t, ok)
	assert.Equal(t, results[0], gotByOwner)

	all := store.List()
	require.Len(t, all, 3)
	all[0].ID = "mutated"
	stillIntact, ok := store.Get(results[0].ID)
	require.True(t, ok)
	assert.Equal(t, results[0].ID, stillIntact.ID)

	byMfr := store.ListByManufacturer(nodefeature.ManufacturerNVIDIA)
	assert.Len(t, byMfr, 3)
	assert.Empty(t, store.ListByManufacturer(nodefeature.ManufacturerHygon))

	// Reclaim dead pods, keeping only pod-1 alive.
	reclaimed := store.Reclaim([]string{"pod-1"})
	assert.Equal(t, 2, reclaimed)
	assert.Equal(t, 1, store.Count())

	// Reclaim all remaining pods, then verify 7g.80gb [0, 8) succeeds and produces deterministic ID.
	assert.Equal(t, 1, store.Reclaim(nil))
	assert.Equal(t, 0, store.Count())

	whole, rebound, err := store.Reserve(nodefeature.ManufacturerNVIDIA, "pod-whole", "main", grp, acc, "7g.80gb")
	require.NoError(t, err)
	assert.False(t, rebound)
	assert.Equal(t, SyntheticPartitionID(nodefeature.ManufacturerNVIDIA, acc.Index, "7g.80gb", 0), whole.ID)
}

func TestAllocator_PartitionedVisibilityAndReclaimLoop_EndToEnd(t *testing.T) {
	const nodeName = "node-e2e-mig"
	devs := mustBuildPresetDevices(t, nodeName, "nvidia-h100-mig", 1)
	grpID := devs.Spec.Groups[0].ID
	cardID := devs.Spec.Groups[0].Accelerators[0].ID

	pod1 := newPartitionPod(nodeName, "p1", nodefeature.ManufacturerNVIDIA, "3g.40gb", 1, true)
	pod2 := newPartitionPod(nodeName, "p2", nodefeature.ManufacturerNVIDIA, "3g.40gb", 1, false)
	pod3 := newPartitionPod(nodeName, "p3", nodefeature.ManufacturerNVIDIA, "3g.40gb", 1, false)

	rec, cli := newFakeReconciler(nodeName, devs, pod1, pod2)

	// Run initial Reconcile so lastLivePodUIDs is seeded before Allocate runs (exercising the
	// reserveDevices notifyListeners broadcast path while RunReclaimLoop is active).
	_, err := rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)

	store := NewPartitionStore()
	sockDir := shortSocketDir(t)
	kubeSock, _ := startFakeKubelet(t, sockDir)

	alloc := NewAllocator(
		nodefeature.ManufacturerNVIDIA,
		device.AllocatorOptions{Logger: logr.Discard(), KubeSocket: kubeSock},
		WithReconciler(rec),
		WithPartitionStore(store),
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- alloc.Start(ctx) }()
	t.Cleanup(func() {
		alloc.Stop()
		<-errCh
	})

	var partSrv, visSrv kubeletdeviceplugin.DevicePluginServer
	for i, m := range alloc.Modes() {
		if m == workercore.DeviceAllocationModePartitioned {
			partSrv = alloc.Servers()[i].(kubeletdeviceplugin.DevicePluginServer)
		}
		if m == workercore.DeviceAllocationModeVisibility {
			visSrv = alloc.Servers()[i].(kubeletdeviceplugin.DevicePluginServer)
		}
	}
	require.NotNil(t, partSrv)
	require.NotNil(t, visSrv)

	tokens := deviceplugin.Resource{Group: grpID, Device: cardID}.DeviceIDs(
		workercore.DeviceAllocationModePartitioned, 7,
	)

	// 1. Allocate pod1 (3g.40gb -> placement [0, 4)).
	resp1, err := partSrv.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{DevicesIds: []string{tokens[0]}}},
	})
	require.NoError(t, err)
	wantID1 := SyntheticPartitionID(nodefeature.ManufacturerNVIDIA, 0, "3g.40gb", 0)
	ctr1 := resp1.ContainerResponses[0]
	assert.Empty(t, ctr1.Devices)
	assert.Empty(t, ctr1.Mounts)
	assert.Equal(t, wantID1, ctr1.Envs[EnvMockAllocatedDevices])
	assert.Equal(t, "partitioned", ctr1.Envs[EnvMockAllocationMode])
	assert.Equal(t, nodefeature.ManufacturerNVIDIA, ctr1.Envs[EnvMockManufacturer])
	assert.Equal(t, wantID1, ctr1.Envs["NVIDIA_VISIBLE_DEVICES"])

	// 2. Co-allocate pod1's SSH visibility sidecar -> must see pod1's exact partition UUID!
	visToken := deviceplugin.Resource{Group: grpID, Device: cardID}.DeviceIDs(
		workercore.DeviceAllocationModeVisibility, 0,
	)[0]
	visResp, err := visSrv.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{DevicesIds: []string{visToken}}},
	})
	require.NoError(t, err)
	visCtr := visResp.ContainerResponses[0]
	assert.Empty(t, visCtr.Devices)
	assert.Empty(t, visCtr.Mounts)
	assert.Equal(t, wantID1, visCtr.Envs[EnvMockAllocatedDevices])
	assert.Equal(t, "visibility", visCtr.Envs[EnvMockAllocationMode])
	assert.Equal(t, wantID1, visCtr.Envs["NVIDIA_VISIBLE_DEVICES"])

	// 3. Allocate pod2 (3g.40gb -> placement [4, 8)), saturating the 3g.40gb profile on card 0.
	resp2, err := partSrv.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{DevicesIds: []string{tokens[1]}}},
	})
	require.NoError(t, err)
	wantID2 := SyntheticPartitionID(nodefeature.ManufacturerNVIDIA, 0, "3g.40gb", 4)
	assert.Equal(t, wantID2, resp2.ContainerResponses[0].Envs[EnvMockAllocatedDevices])
	assert.Equal(t, 2, store.Count())

	// Reconcile Devices.status and verify 2 allocated 3g.40gb profiles and 0 remaining 3g.40gb profiles.
	_, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)

	gotDevs := new(workercore.Devices)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, gotDevs))
	require.Len(t, gotDevs.Status.Groups, 1)
	accStatus := gotDevs.Status.Groups[0].Accelerators[0]
	assert.Equal(t, workercore.DeviceAllocationModePartitioned, accStatus.Mode)
	assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.40gb", Count: 2}}, accStatus.AllocatedProfiles)
	for _, rem := range accStatus.RemainingProfiles {
		assert.NotEqual(t, "3g.40gb", rem.Name, "3g.40gb must be saturated")
	}

	// 4. Adding pod3 while saturated must be refused by Allocate!
	require.NoError(t, cli.Create(ctx, pod3))
	_, err = partSrv.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{DevicesIds: []string{tokens[2]}}},
	})
	require.Error(t, err)

	// 5. Delete pod1 and run Reconcile -> Devices.status frees 1 slot AND RunReclaimLoop reclaims pod1's partition!
	require.NoError(t, cli.Delete(ctx, pod1))
	_, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: nodeName}})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, exists := store.Get(wantID1)
		return !exists && store.Count() == 1
	}, 5*time.Second, 20*time.Millisecond, "RunReclaimLoop must reclaim pod1's virtual partition")

	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, gotDevs))
	accStatus = gotDevs.Status.Groups[0].Accelerators[0]
	assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.40gb", Count: 1}}, accStatus.AllocatedProfiles)
	assert.Contains(t, accStatus.RemainingProfiles, workercore.AcceleratorProfileCount{Name: "3g.40gb", Count: 1})

	// 6. Now pod3 succeeds and re-takes placement [0, 4) with deterministic UUID wantID1!
	resp3, err := partSrv.Allocate(ctx, &kubeletdeviceplugin.AllocateRequest{
		ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{DevicesIds: []string{tokens[2]}}},
	})
	require.NoError(t, err)
	assert.Equal(t, wantID1, resp3.ContainerResponses[0].Envs[EnvMockAllocatedDevices])
	assert.Equal(t, 2, store.Count())
}

func TestAllocator_HygonAndTHeadPartitionedEnvAndRollback(t *testing.T) {
	t.Run("hygon sets HIP_VISIBLE_DEVICES and DMI_MIG_VISIBLE_DEVICE", func(t *testing.T) {
		const nodeName = "node-hygon"
		devs := mustBuildPresetDevices(t, nodeName, "hygon-k100-ai-mig", 1)
		grpID := devs.Spec.Groups[0].ID
		cardID := devs.Spec.Groups[0].Accelerators[0].ID
		pod := newPartitionPod(nodeName, "p-hygon", nodefeature.ManufacturerHygon, "2g.15gb", 1, true)

		rec, _ := newFakeReconciler(nodeName, devs, pod)
		alloc := NewAllocator(nodefeature.ManufacturerHygon, device.AllocatorOptions{Logger: logr.Discard()}, WithReconciler(rec))

		var partSrv, visSrv kubeletdeviceplugin.DevicePluginServer
		for i, m := range alloc.Modes() {
			if m == workercore.DeviceAllocationModePartitioned {
				partSrv = alloc.Servers()[i].(kubeletdeviceplugin.DevicePluginServer)
			}
			if m == workercore.DeviceAllocationModeVisibility {
				visSrv = alloc.Servers()[i].(kubeletdeviceplugin.DevicePluginServer)
			}
		}

		tok := deviceplugin.Resource{Group: grpID, Device: cardID}.DeviceIDs(workercore.DeviceAllocationModePartitioned, 4)[0]
		resp, err := partSrv.Allocate(context.Background(), &kubeletdeviceplugin.AllocateRequest{
			ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{DevicesIds: []string{tok}}},
		})
		require.NoError(t, err)
		wantID := SyntheticPartitionID(nodefeature.ManufacturerHygon, 0, "2g.15gb", 0)
		envs := resp.ContainerResponses[0].Envs
		assert.Equal(t, wantID, envs["HIP_VISIBLE_DEVICES"])
		assert.Equal(t, wantID, envs[EnvHygonMIGVisibleDevice])
		assert.Empty(t, resp.ContainerResponses[0].Devices)
		assert.Empty(t, resp.ContainerResponses[0].Mounts)

		visTok := deviceplugin.Resource{Group: grpID, Device: cardID}.DeviceIDs(workercore.DeviceAllocationModeVisibility, 0)[0]
		visResp, err := visSrv.Allocate(context.Background(), &kubeletdeviceplugin.AllocateRequest{
			ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{DevicesIds: []string{visTok}}},
		})
		require.NoError(t, err)
		assert.Equal(t, wantID, visResp.ContainerResponses[0].Envs["HIP_VISIBLE_DEVICES"])
		assert.Equal(t, wantID, visResp.ContainerResponses[0].Envs[EnvHygonMIGVisibleDevice])
	})

	t.Run("thead separator-less profile round-trip and patch failure rollback", func(t *testing.T) {
		const nodeName = "node-thead"
		devs := mustBuildPresetDevices(t, nodeName, "thead-ppu-mig", 1)
		grpID := devs.Spec.Groups[0].ID
		cardID := devs.Spec.Groups[0].Accelerators[0].ID
		// Published profile "4g.48gb" maps back to vendor spelling "4g48gb".
		pod := newPartitionPod(nodeName, "p-thead", nodefeature.ManufacturerTHead, "4g.48gb", 1, false)

		failPatch := true
		cli := ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(devs, pod).
			WithIndex(&core.Pod{}, deviceplugin.IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
				return []string{obj.(*core.Pod).Spec.NodeName}
			}).
			WithInterceptorFuncs(ctrlintercept.Funcs{
				Patch: func(ctx context.Context, client ctrlcli.WithWatch, obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.PatchOption) error {
					if failPatch {
						return errors.New("injected apiserver patch failure")
					}
					return client.Patch(ctx, obj, patch, opts...)
				},
			}).
			Build()

		rec := &deviceplugin.DevicesReconciler{NodeName: nodeName, Client: cli}
		store := NewPartitionStore()
		alloc := NewAllocator(
			nodefeature.ManufacturerTHead,
			device.AllocatorOptions{Logger: logr.Discard()},
			WithReconciler(rec),
			WithPartitionStore(store),
		)

		var partSrv kubeletdeviceplugin.DevicePluginServer
		for i, m := range alloc.Modes() {
			if m == workercore.DeviceAllocationModePartitioned {
				partSrv = alloc.Servers()[i].(kubeletdeviceplugin.DevicePluginServer)
			}
		}

		tok := deviceplugin.Resource{Group: grpID, Device: cardID}.DeviceIDs(workercore.DeviceAllocationModePartitioned, 2)[0]

		// Patch fails -> Rollback must clean up the newly created partition from PartitionStore!
		_, err := partSrv.Allocate(context.Background(), &kubeletdeviceplugin.AllocateRequest{
			ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{DevicesIds: []string{tok}}},
		})
		require.Error(t, err)
		assert.Equal(t, 0, store.Count(), "failed annotation patch must roll back created virtual partition")

		// Once patch succeeds, allocation succeeds with vendor profile "4g48gb".
		failPatch = false
		resp, err := partSrv.Allocate(context.Background(), &kubeletdeviceplugin.AllocateRequest{
			ContainerRequests: []*kubeletdeviceplugin.ContainerAllocateRequest{{DevicesIds: []string{tok}}},
		})
		require.NoError(t, err)
		wantID := SyntheticPartitionID(nodefeature.ManufacturerTHead, 0, "4g48gb", 0)
		assert.Equal(t, wantID, resp.ContainerResponses[0].Envs["PPU_VISIBLE_DEVICES"])
		assert.Equal(t, 1, store.Count())
	})
}

func TestPartitionStore_EdgeCasesMultiCardRollbackAndVisibilityRefusals(t *testing.T) {
	t.Run("nil store and invalid Reserve inputs fail closed", func(t *testing.T) {
		var nilStore *PartitionStore
		assert.Equal(t, 0, nilStore.Count())
		assert.Nil(t, nilStore.List())
		assert.Nil(t, nilStore.ListByCard("nvidia", "GPU-0"))
		assert.Nil(t, nilStore.ListByManufacturer("nvidia"))
		_, ok := nilStore.Get("x")
		assert.False(t, ok)
		_, ok = nilStore.GetByOwner("nvidia", "p", "c", "GPU-0")
		assert.False(t, ok)
		assert.False(t, nilStore.Release("nvidia", "GPU-0", "x", "p", "c"))
		assert.Equal(t, 0, nilStore.Reclaim([]string{"p"}))
		_, _, err := nilStore.Reserve("nvidia", "p", "c", nil, nil, "1g.10gb")
		require.Error(t, err)

		store := NewPartitionStore()
		devs := mustBuildPresetDevices(t, "node-val", "nvidia-h100-mig", 1)
		grp := &devs.Spec.Groups[0]
		acc := &grp.Accelerators[0]

		_, _, err = store.Reserve("nvidia", "p", "c", grp, &workercore.Accelerator{}, "1g.10gb")
		require.Error(t, err)
		_, _, err = store.Reserve("nvidia", "", "c", grp, acc, "1g.10gb")
		require.Error(t, err)
		_, _, err = store.Reserve("nvidia", "p", "c", grp, acc, "")
		require.Error(t, err)
		_, _, err = store.Reserve("nvidia", "p", "c", grp, acc, "nonexistent-profile")
		require.Error(t, err)

		assert.Equal(t, "MOCK-MIG-0-1g.10gb-0", SyntheticPartitionID("", 0, "1g.10gb", 0))
		assert.Equal(t, "GPU_VISIBLE_DEVICES", VisibleDevicesEnv("   "))
	})

	t.Run("multi-card partial failure rolls back earlier card and attribution self-check preserves live placement", func(t *testing.T) {
		const nodeName = "node-multicard"
		devs := mustBuildPresetDevices(t, nodeName, "nvidia-h100-mig", 2)
		grp := &devs.Spec.Groups[0]
		card0 := &grp.Accelerators[0]
		card1 := &grp.Accelerators[1]
		// Remove 7g.80gb from card1 so a 2-card 7g.80gb actuation succeeds on card0 and fails on card1.
		card1.Status.PhysicalSliced.Profiles = card1.Status.PhysicalSliced.Profiles[:1]

		hygonDevs := mustBuildPresetDevices(t, nodeName, "hygon-k100-ai-mig", 1)
		store := NewPartitionStore()
		_, _, err := store.Reserve(nodefeature.ManufacturerHygon, "pod-h", "main", &hygonDevs.Spec.Groups[0], &hygonDevs.Spec.Groups[0].Accelerators[0], "2g.15gb")
		require.NoError(t, err)

		rec, _ := newFakeReconciler(nodeName, devs)
		alloc := NewAllocator(
			nodefeature.ManufacturerNVIDIA,
			device.AllocatorOptions{Logger: logr.Discard()},
			WithReconciler(rec),
			WithPartitionStore(store),
		)

		var physResponder deviceplugin.PhysicalSlicedResponder
		for i, m := range alloc.Modes() {
			if m == workercore.DeviceAllocationModePartitioned {
				physResponder = alloc.Servers()[i].(deviceplugin.PhysicalSlicedResponder)
			}
		}
		require.NotNil(t, physResponder)

		pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "p-multi", Namespace: "default", UID: "uid-multi"}}
		ctr := &core.Container{Name: "main"}
		allocated := map[deviceplugin.Resource]int32{
			{Group: grp.ID, Device: card0.ID}: 1,
			{Group: grp.ID, Device: card1.ID}: 1,
		}

		_, err = physResponder.ActuatePhysicalSliced(context.Background(), pod, ctr, devs, allocated, "7g.80gb")
		require.Error(t, err)
		assert.Empty(t, store.ListByCard(nodefeature.ManufacturerNVIDIA, card0.ID), "card0 partition must be rolled back")
		assert.Equal(t, 1, store.Count(), "unrelated hygon partition must remain intact")

		// Reserve two partitions across cards and manufacturers to exercise deterministic sorting and attribution guard.
		vp0, _, err := store.Reserve(nodefeature.ManufacturerNVIDIA, "pod-dead", "main", grp, card0, "1g.10gb")
		require.NoError(t, err)
		_, _, err = store.Reserve(nodefeature.ManufacturerNVIDIA, "pod-dead", "main", grp, card1, "1g.10gb")
		require.NoError(t, err)
		sorted := store.List()
		require.Len(t, sorted, 3)
		assert.Equal(t, nodefeature.ManufacturerHygon, sorted[0].Manufacturer)
		assert.Equal(t, uint32(0), sorted[1].CardIndex)
		assert.Equal(t, uint32(1), sorted[2].CardIndex)

		// Attribution guard: card0's placement [0,1) is claimed in occupied, so ReclaimManufacturer skips card0 and reclaims card1.
		occupied := deviceplugin.Placements{
			{Group: grp.ID, Device: card0.ID}: {vp0.Placement},
		}
		reclaimed := store.ReclaimManufacturer(nodefeature.ManufacturerNVIDIA, nil, occupied)
		assert.Equal(t, 1, reclaimed)
		_, stillOnCard0 := store.Get(vp0.ID)
		assert.True(t, stillOnCard0, "occupied placement must not be reclaimed")

		// Visibility responder refusals: missing owner partition and removed profile.
		_, err = physResponder.GetPhysicalSlicedVisibilityResponse(
			context.Background(),
			&core.Pod{ObjectMeta: meta.ObjectMeta{UID: "uid-missing"}},
			&core.Container{Name: "ssh"},
			devs,
			map[deviceplugin.Resource]int32{{Group: grp.ID, Device: card0.ID}: 1},
			"main",
		)
		require.Error(t, err)
		assert.ErrorContains(t, err, "no live virtual partition")

		strippedDevs := devs.DeepCopy()
		strippedDevs.Spec.Groups[0].Accelerators[0].Status.PhysicalSliced.Profiles = nil
		_, err = physResponder.GetPhysicalSlicedVisibilityResponse(
			context.Background(),
			&core.Pod{ObjectMeta: meta.ObjectMeta{UID: "pod-dead"}},
			&core.Container{Name: "ssh"},
			strippedDevs,
			map[deviceplugin.Resource]int32{{Group: grp.ID, Device: card0.ID}: 1},
			"main",
		)
		require.Error(t, err)
		assert.ErrorContains(t, err, "no longer offers profile")

		// Verify LookupPartition satisfies detectormock.PartitionProvider for both NVIDIA and Hygon.
		nvAns, found := store.LookupPartition(nodefeature.ManufacturerNVIDIA, device.AcceleratorPartitionRequest{
			DeviceID:   card0.ID,
			ID:         vp0.ID,
			Profile:    vp0.Profile,
			Placements: []workercore.AcceleratorPlacement{vp0.Placement},
		})
		require.True(t, found)
		assert.Equal(t, vp0.ID, nvAns.ID)
		require.NotNil(t, nvAns.MemoryTotalBytes)
		assert.Equal(t, uint64(vp0.MemoryMib)<<20, *nvAns.MemoryTotalBytes)
		require.NotNil(t, nvAns.MemoryUsedBytes)
		assert.Equal(t, uint64(0), *nvAns.MemoryUsedBytes)
		assert.Nil(t, nvAns.CoresPercent)
		assert.Equal(t, device.AcceleratorProcessReasonUnsupported, nvAns.CoresReason)

		hygonParts := store.ListByManufacturer(nodefeature.ManufacturerHygon)
		require.Len(t, hygonParts, 1)
		hyAns, found := store.LookupPartition(nodefeature.ManufacturerHygon, device.AcceleratorPartitionRequest{
			DeviceID: hygonParts[0].DeviceID,
			ID:       hygonParts[0].ID,
		})
		require.True(t, found)
		require.NotNil(t, hyAns.CoresPercent)
		assert.Equal(t, uint32(0), *hyAns.CoresPercent)

		_, found = store.LookupPartition(nodefeature.ManufacturerAMD, device.AcceleratorPartitionRequest{ID: vp0.ID})
		assert.False(t, found)
	})
}
