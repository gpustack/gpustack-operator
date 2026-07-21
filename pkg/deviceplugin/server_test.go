package deviceplugin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeletdeviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func TestResourceServer_GetResourceName(t *testing.T) {
	cases := []struct {
		name string
		mode workercore.DeviceAllocationMode
		want core.ResourceName
	}{
		{
			name: "exclusive uses the bare resource key",
			mode: workercore.DeviceAllocationModeExclusive,
			want: "nvidia.com/gpu",
		},
		{
			name: "shared uses the .shared key",
			mode: workercore.DeviceAllocationModeShared,
			want: "nvidia.com/gpu.shared",
		},
		{
			// Sliced advertises the bare ".sliced" injection-token key, never the
			// ".sliced.units" counting key (which is reported via Patch Node).
			name: "sliced uses the bare .sliced injection-token key",
			mode: workercore.DeviceAllocationModeSliced,
			want: "nvidia.com/gpu.sliced",
		},
		{
			// Visibility advertises the device-only visibility resource, outside the
			// accelerator families.
			name: "visibility uses the device-only visibility key",
			mode: workercore.DeviceAllocationModeVisibility,
			want: "device.gpustack.ai/nvidia.visibility",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			s := &ResourceServer{Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: c.mode}
			assert.Equal(t, c.want, s.GetResourceName())
		})
	}
}

type stubResponder struct{}

func (stubResponder) GetContainerAllocateResponse(
	context.Context, *core.Pod, *core.Container, *workercore.Devices, map[Resource]int32,
) (*ContainerAllocateResponse, error) {
	return &ContainerAllocateResponse{}, nil
}

// TestResourceServer_Allocate_Sliced verifies the fallback path: when a sliced container carries
// no ".sliced.units" request (a Pod the webhook did not shape), Allocated falls back to the plain
// injection-token count (one token per card). The shaped path — recording the real per-card units
// so the per-card ledger reflects capacity — is covered by TestResourceServer_Allocate_Sliced_RecordsUnits.
func TestResourceServer_Allocate_Sliced(t *testing.T) {
	const nodeName = "node-5"
	resName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)

	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{
					ID:    "dev-0",
					Index: 0,
				}},
			}},
		},
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "c",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{resName: resource.MustParse("1")},
				},
			}},
		},
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, pod).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()

	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeSliced,
		Reconciler:     &DevicesReconciler{NodeName: nodeName, Client: cli},
		Responder:      stubResponder{},
	}

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{
			DevicesIds: []string{"grp-0:dev-0:0000"},
		}},
	})
	require.NoError(t, err)

	got := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
	allocated, err := extractAllocatedStatusFromPod(got)
	require.NoError(t, err)
	require.Len(t, allocated.Groups, 1)
	require.Len(t, allocated.Groups[0].Accelerators, 1)
	acc := allocated.Groups[0].Accelerators[0]
	assert.Equal(t, "dev-0", acc.ID)
	assert.Equal(t, workercore.DeviceAllocationModeSliced, acc.Mode)
	assert.Equal(t, int32(1), acc.Allocated, "plain injection-token count, not padded units")
}

// TestResourceServer_Allocate_Sliced_RecordsUnits verifies the sliced Allocate records the
// container's per-card ".sliced.units" (the real committed units the Pod webhook folded the memory
// budget into) as the ledger Allocated — so the per-card ledger reflects capacity and the
// node-devices admission check can refuse a card whose committed units would exceed it, not the
// loose injection-token count.
func TestResourceServer_Allocate_Sliced_RecordsUnits(t *testing.T) {
	const nodeName = "node-6"
	resName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)
	unitsName := nodefeature.GetAcceleratableSlicedUnitsResourceName(nodefeature.ManufacturerNVIDIA)
	wantUnits := int32(60 * nodefeature.ResourceMaxUnits / 100) // a 60% slice = 60% of a card's units

	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{
					ID:    "dev-0",
					Index: 0,
				}},
			}},
		},
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "c",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						resName:   resource.MustParse("1"),
						unitsName: *resource.NewQuantity(int64(wantUnits), resource.DecimalSI),
					},
				},
			}},
		},
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, pod).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()

	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeSliced,
		Reconciler:     &DevicesReconciler{NodeName: nodeName, Client: cli},
		Responder:      stubResponder{},
	}

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{
			DevicesIds: []string{"grp-0:dev-0:0000"},
		}},
	})
	require.NoError(t, err)

	got := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
	allocated, err := extractAllocatedStatusFromPod(got)
	require.NoError(t, err)
	require.Len(t, allocated.Groups, 1)
	require.Len(t, allocated.Groups[0].Accelerators, 1)
	acc := allocated.Groups[0].Accelerators[0]
	assert.Equal(t, workercore.DeviceAllocationModeSliced, acc.Mode)
	assert.Equal(t, wantUnits, acc.Allocated, "real per-card committed units, not the token count")
}

// crossModeDevices builds a single-card node inventory. When statusMode is not None the ledger
// Status records dev-0 held in statusMode (Remaining 0), so a cross-mode Allocate observes it.
func crossModeDevices(nodeName string, statusMode workercore.DeviceAllocationMode) *workercore.Devices {
	d := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{
					ID:    "dev-0",
					Index: 0,
				}},
			}},
		},
	}
	if statusMode != workercore.DeviceAllocationModeNone {
		d.Status = workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.AcceleratorAllocation{{
					ID:        "dev-0",
					Index:     0,
					Mode:      statusMode,
					Remaining: 0,
				}},
			}},
		}
	}
	return d
}

// TestResourceServer_Allocate_CrossMode verifies the authoritative on-node gate: a card kubelet
// assigned that another, non-None mode already holds — via the ledger Status OR the in-process
// reservation (the ledger not yet reconciled) — is refused with FailedPrecondition and never
// patches an allocation annotation, while a free card and same-mode (sliced-on-sliced) succeed.
func TestResourceServer_Allocate_CrossMode(t *testing.T) {
	cases := []struct {
		name        string
		serverMode  workercore.DeviceAllocationMode
		statusMode  workercore.DeviceAllocationMode // dev-0 mode in the ledger Status (None ⇒ free/absent)
		reserveMode workercore.DeviceAllocationMode // a sibling pod's reservation on dev-0 (None ⇒ none)
		wantErr     bool
	}{
		{
			name:       "shared onto an exclusive-held card (via Status) is rejected",
			serverMode: workercore.DeviceAllocationModeShared,
			statusMode: workercore.DeviceAllocationModeExclusive,
			wantErr:    true,
		},
		{
			name:        "shared onto an exclusive-held card (via reservation only, ledger lagging) is rejected",
			serverMode:  workercore.DeviceAllocationModeShared,
			reserveMode: workercore.DeviceAllocationModeExclusive,
			wantErr:     true,
		},
		{
			name:       "exclusive onto a shared-held card is rejected",
			serverMode: workercore.DeviceAllocationModeExclusive,
			statusMode: workercore.DeviceAllocationModeShared,
			wantErr:    true,
		},
		{
			name:       "shared onto a free card succeeds",
			serverMode: workercore.DeviceAllocationModeShared,
			wantErr:    false,
		},
		{
			name:       "sliced onto a sliced-held card (same mode) succeeds",
			serverMode: workercore.DeviceAllocationModeSliced,
			statusMode: workercore.DeviceAllocationModeSliced,
			wantErr:    false,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			const nodeName = "node-cm"
			devs := crossModeDevices(nodeName, c.statusMode)
			resName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, c.serverMode)
			pod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: "pod-cm"},
				Spec: core.PodSpec{
					NodeName: nodeName,
					Containers: []core.Container{{
						Name: "main",
						Resources: core.ResourceRequirements{
							Limits: core.ResourceList{resName: resource.MustParse("1")},
						},
					}},
				},
			}

			cli := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(devs, pod).
				WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
					return []string{obj.(*core.Pod).Spec.NodeName}
				}).
				Build()

			rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
			if c.reserveMode != workercore.DeviceAllocationModeNone {
				rec.reserveDevices("sibling", workercore.DevicesStatus{
					Groups: []workercore.DevicesAllocationGroup{{
						ID:           "grp-0",
						Manufacturer: nodefeature.ManufacturerNVIDIA,
						Accelerators: []workercore.AcceleratorAllocation{{ID: "dev-0", Mode: c.reserveMode}},
					}},
				})
			}
			s := &ResourceServer{
				Manufacturer:   nodefeature.ManufacturerNVIDIA,
				AllocationMode: c.serverMode,
				Reconciler:     rec,
				Responder:      stubResponder{},
			}

			_, err := s.Allocate(context.Background(), &AllocateRequest{
				ContainerRequests: []*ContainerAllocateRequest{{
					DevicesIds: []string{"grp-0:dev-0:0000"},
				}},
			})

			got := new(core.Pod)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
			_, annotated := got.Annotations[AllocatedAcceleratorAnnoKey]

			if c.wantErr {
				require.Error(t, err)
				assert.Equal(t, grpccodes.FailedPrecondition, grpcstatus.Code(err),
					"a cross-mode conflict must be a FailedPrecondition")
				assert.False(t, annotated, "a rejected Allocate must not patch the allocation annotation")
				return
			}
			require.NoError(t, err)
			assert.True(t, annotated, "a permitted Allocate must patch the allocation annotation")
		})
	}
}

// twoCardDevices builds a two-card node inventory (dev-0, dev-1). When dev0Status is not None the
// ledger Status records dev-0 held in that mode (Remaining 0) and dev-1 free.
func twoCardDevices(nodeName string, dev0Status workercore.DeviceAllocationMode) *workercore.Devices {
	d := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{
					{ID: "dev-0", Index: 0},
					{ID: "dev-1", Index: 1},
				},
			}},
		},
	}
	if dev0Status != workercore.DeviceAllocationModeNone {
		d.Status = workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.AcceleratorAllocation{
					{ID: "dev-0", Index: 0, Mode: dev0Status, Remaining: 0},
					{ID: "dev-1", Index: 1, Mode: workercore.DeviceAllocationModeNone, Remaining: nodefeature.ResourceMaxUnits},
				},
			}},
		}
	}
	return d
}

// TestResourceServer_GetListAndWatch_AdvertisesByHealth verifies ListAndWatch reflects hardware
// health only, never allocation state: a card held in a conflicting mode — via the ledger Status OR
// an in-process reservation — is still advertised for this mode. Withholding its tokens would make
// the advertised allocatable count track allocation and desync kubelet's device accounting; the
// cross-mode invariant is enforced at Allocate, not here.
func TestResourceServer_GetListAndWatch_AdvertisesByHealth(t *testing.T) {
	const nodeName = "node-wh"
	dev0 := Resource{Group: "grp-0", Device: "dev-0"}
	dev1 := Resource{Group: "grp-0", Device: "dev-1"}

	// dev-0 held Exclusive in the ledger Status; dev-1 held Exclusive only via a reservation (ledger
	// lagging). A shared server must still advertise both.
	devs := twoCardDevices(nodeName, workercore.DeviceAllocationModeExclusive)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(devs).Build()

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	rec.reserveDevices("sibling", workercore.DevicesStatus{
		Groups: []workercore.DevicesAllocationGroup{{
			ID:           "grp-0",
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{{ID: "dev-1", Mode: workercore.DeviceAllocationModeExclusive}},
		}},
	})
	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeShared,
		Reconciler:     rec,
	}

	resp, err := s.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	assert.Greater(t, cardTokenCount(resp, dev0), 0,
		"a card held in another mode via Status must still be advertised (health-only)")
	assert.Greater(t, cardTokenCount(resp, dev1), 0,
		"a card held in another mode via reservation must still be advertised (health-only)")
}

// TestResourceServer_GetListAndWatch_PerCardSlicedTokens verifies the sliced token pool is
// sized per card from its own slicing capability: a soft card advertises LogicalSliced.Count
// tokens and a MIG card advertises its PhysicalSliced.Count (non-zero, so it stays served
// rather than dropping out). Non-sliced modes ignore the count.
func TestResourceServer_GetListAndWatch_PerCardSlicedTokens(t *testing.T) {
	const nodeName = "node-pc"
	soft0 := Resource{Group: "grp-0", Device: "soft-0"}
	soft1 := Resource{Group: "grp-0", Device: "soft-1"}
	mig := Resource{Group: "grp-0", Device: "mig-0"}

	// Two soft cards (128 slices each) + one MIG card (7 physical instances): each card's own
	// per-card capability sizes its token pool independently.
	newFmt := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{
					{ID: "soft-0", Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 128}}},
					{ID: "soft-1", Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 128}}},
					{ID: "mig-0", Status: workercore.AcceleratorStatus{PhysicalSliced: workercore.AcceleratorPhysicalSliced{Count: 7}}},
				},
			}},
		},
	}

	server := func(devs *workercore.Devices, mode workercore.DeviceAllocationMode) *ResourceServer {
		cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(devs).Build()
		return &ResourceServer{
			Manufacturer:   nodefeature.ManufacturerNVIDIA,
			AllocationMode: mode,
			Reconciler:     &DevicesReconciler{NodeName: nodeName, Client: cli},
		}
	}

	t.Run("soft cards get logical count, mig card gets physical ceiling", func(t *testing.T) {
		resp, err := server(newFmt, workercore.DeviceAllocationModeSliced).getListAndWatchResponse(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 128, cardTokenCount(resp, soft0), "soft card advertises LogicalSliced.Count tokens")
		assert.Equal(t, 128, cardTokenCount(resp, soft1), "soft card advertises LogicalSliced.Count tokens")
		assert.Equal(t, 7, cardTokenCount(resp, mig), "MIG card advertises PhysicalSliced.Count tokens (stays served)")
	})

	t.Run("exclusive mode ignores the sliced count", func(t *testing.T) {
		resp, err := server(newFmt, workercore.DeviceAllocationModeExclusive).getListAndWatchResponse(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 1, cardTokenCount(resp, soft0), "exclusive advertises one token per card")
		assert.Equal(t, 1, cardTokenCount(resp, mig), "exclusive advertises one token per card regardless of MIG")
	})
}

// concurrentAllocatePod builds a pending pod requesting count units of mode's resource on node.
func concurrentAllocatePod(nodeName, name, uid string, mode workercore.DeviceAllocationMode, count int) *core.Pod {
	resName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, mode)
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "main",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{resName: *resource.NewQuantity(int64(count), resource.DecimalSI)},
				},
			}},
		},
	}
}

// waitOrDeadlock fails the test if wg does not complete within the timeout.
func waitOrDeadlock(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("Allocate deadlocked")
	}
}

// TestResourceServer_Allocate_Concurrent verifies the node allocate mutex closes the check→reserve
// TOCTOU: simultaneous exclusive and shared Allocate for the same free card(s) yield exactly one
// success and one FailedPrecondition (never two co-located allocations), including a multi-card
// request whose cards are requested in the opposite order.
func TestResourceServer_Allocate_Concurrent(t *testing.T) {
	// classifyOutcomes counts nil (success) vs FailedPrecondition (rejected); any other error fails.
	classifyOutcomes := func(t *testing.T, errs []error) (success, rejected int) {
		t.Helper()
		for _, err := range errs {
			switch {
			case err == nil:
				success++
			case grpcstatus.Code(err) == grpccodes.FailedPrecondition:
				rejected++
			default:
				t.Fatalf("unexpected Allocate error: %v", err)
			}
		}
		return success, rejected
	}

	cases := []struct {
		name       string
		cardsInReq []string // device IDs each concurrent Allocate requests (both request the same cards)
	}{
		{
			name:       "one card: exactly one of exclusive/shared wins",
			cardsInReq: []string{"grp-0:dev-0:0000"},
		},
		{
			name:       "two cards (opposite request order): exactly one of exclusive/shared wins, no deadlock",
			cardsInReq: []string{"grp-0:dev-0:0000", "grp-0:dev-1:0000"},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			const nodeName = "node-cc"
			count := len(c.cardsInReq)

			devs := twoCardDevices(nodeName, workercore.DeviceAllocationModeNone) // dev-0, dev-1 both free
			exclPod := concurrentAllocatePod(nodeName, "excl", "uid-excl", workercore.DeviceAllocationModeExclusive, count)
			sharedPod := concurrentAllocatePod(nodeName, "shared", "uid-shared", workercore.DeviceAllocationModeShared, count)

			cli := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(devs, exclPod, sharedPod).
				WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
					return []string{obj.(*core.Pod).Spec.NodeName}
				}).
				Build()

			rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
			exclServer := &ResourceServer{
				Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: workercore.DeviceAllocationModeExclusive,
				Reconciler: rec, Responder: stubResponder{},
			}
			sharedServer := &ResourceServer{
				Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: workercore.DeviceAllocationModeShared,
				Reconciler: rec, Responder: stubResponder{},
			}

			// Both request the same cards; the shared server requests them in reverse so the outcome
			// (exactly one winner) is shown to be independent of the device-ID order in each request,
			// which the single node allocateMutex serializes.
			exclIDs := append([]string(nil), c.cardsInReq...)
			sharedIDs := append([]string(nil), c.cardsInReq...)
			for i, j := 0, len(sharedIDs)-1; i < j; i, j = i+1, j-1 {
				sharedIDs[i], sharedIDs[j] = sharedIDs[j], sharedIDs[i]
			}

			var wg sync.WaitGroup
			errs := make([]error, 2)
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, errs[0] = exclServer.Allocate(context.Background(),
					&AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: exclIDs}}})
			}()
			go func() {
				defer wg.Done()
				_, errs[1] = sharedServer.Allocate(context.Background(),
					&AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: sharedIDs}}})
			}()
			waitOrDeadlock(t, &wg, 15*time.Second)

			success, rejected := classifyOutcomes(t, errs)
			assert.Equal(t, 1, success, "exactly one Allocate must succeed")
			assert.Equal(t, 1, rejected, "exactly one Allocate must be rejected with FailedPrecondition")
		})
	}
}

// TestDevicesReconciler_GetAllocatingPod_SkipReserved verifies the pod-identification skip that
// underlies distinct-pod attribution: with two identical pending pods, skipReserved=false returns
// the oldest (the legacy guess), while skipReserved=true skips a pod already holding a reservation
// and returns the next one.
func TestDevicesReconciler_GetAllocatingPod_SkipReserved(t *testing.T) {
	const nodeName = "node-skip"
	resName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	podX := concurrentAllocatePod(nodeName, "x", "uid-x", workercore.DeviceAllocationModeExclusive, 1)
	podY := concurrentAllocatePod(nodeName, "y", "uid-y", workercore.DeviceAllocationModeExclusive, 1)
	// podY is created after podX, so the "oldest pending" guess prefers podX.
	podY.CreationTimestamp = meta.NewTime(podX.CreationTimestamp.Add(time.Second))

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(podX, podY).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()
	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	q := resource.MustParse("1")

	// No reservation yet: the oldest pending pod is returned.
	got, _, err := rec.getAllocatingPod(context.Background(), resName, q, true)
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-x"), got.UID, "oldest pending pod when none is reserved")

	// Reserve podX: skipReserved=true now skips it and returns podY; skipReserved=false still returns podX.
	rec.reserveDevices("uid-x", workercore.DevicesStatus{
		Groups: []workercore.DevicesAllocationGroup{{
			ID:           "grp-0",
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{{ID: "dev-0", Mode: workercore.DeviceAllocationModeExclusive}},
		}},
	})
	got, _, err = rec.getAllocatingPod(context.Background(), resName, q, true)
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-y"), got.UID, "skipReserved must skip the already-reserved pod")

	got, _, err = rec.getAllocatingPod(context.Background(), resName, q, false)
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-x"), got.UID, "without skipReserved the oldest pod is still returned")
}

// TestResourceServer_Allocate_ConcurrentDistinctPods verifies the node allocate mutex + skip-reserved
// pod identification: when two identical exclusive Pods are pending at once (a Kueue-admitted batch)
// and kubelet issues one Allocate per distinct card, each Allocate must map to a DISTINCT pod so both
// cards are accounted. Before the fix both Allocates resolved to the oldest pending pod, so one card
// was double-attributed to it and the other was lost from the ledger — which could make a genuinely
// held card look free and defeat the cross-mode exclusion.
func TestResourceServer_Allocate_ConcurrentDistinctPods(t *testing.T) {
	const nodeName = "node-dp"
	devs := twoCardDevices(nodeName, workercore.DeviceAllocationModeNone) // dev-0, dev-1 both free
	podX := concurrentAllocatePod(nodeName, "x", "uid-x", workercore.DeviceAllocationModeExclusive, 1)
	podY := concurrentAllocatePod(nodeName, "y", "uid-y", workercore.DeviceAllocationModeExclusive, 1)
	// podY is created after podX, so the naive "oldest pending" guess would pick podX for both.
	podY.CreationTimestamp = meta.NewTime(podX.CreationTimestamp.Add(time.Second))

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, podX, podY).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	// One exclusive server instance handles both calls (as in production); the reconciler singleton
	// carries the shared allocate mutex and reservation map.
	exclServer := &ResourceServer{
		Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: workercore.DeviceAllocationModeExclusive,
		Reconciler: rec, Responder: stubResponder{},
	}

	// kubelet assigns a distinct card to each pod; the two Allocate calls race.
	reqIDs := []string{"grp-0:dev-0:0000", "grp-0:dev-1:0000"}
	var wg sync.WaitGroup
	errs := make([]error, len(reqIDs))
	wg.Add(len(reqIDs))
	for i := range reqIDs {
		i := i
		go func() {
			defer wg.Done()
			_, errs[i] = exclServer.Allocate(context.Background(),
				&AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{reqIDs[i]}}}})
		}()
	}
	waitOrDeadlock(t, &wg, 15*time.Second)
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	// Both distinct pods are reserved, and the two reserved cards are exactly {dev-0, dev-1}: no card
	// was double-attributed to one pod nor lost (the pod↔card pairing may vary with the mutex order).
	resX, okX := rec.reservedDevices("uid-x")
	resY, okY := rec.reservedDevices("uid-y")
	require.True(t, okX, "podX must hold a reservation")
	require.True(t, okY, "podY must hold a reservation")

	reservedCards := map[string]struct{}{}
	for _, res := range []workercore.DevicesStatus{resX, resY} {
		require.Len(t, res.Groups, 1)
		require.Len(t, res.Groups[0].Accelerators, 1)
		reservedCards[res.Groups[0].Accelerators[0].ID] = struct{}{}
	}
	assert.Equal(t, map[string]struct{}{"dev-0": {}, "dev-1": {}}, reservedCards,
		"each pod must be attributed a distinct card (both cards accounted)")
}

// mustGetDevices reads the current Devices (with Status) through the reconciler, failing the test
// on error.
func mustGetDevices(t *testing.T, rec *DevicesReconciler) *workercore.Devices {
	t.Helper()
	d, err := rec.getDevices(context.Background())
	require.NoError(t, err)
	return d
}

// cardTokenCount counts the device tokens a ListAndWatch response advertises for one physical card.
func cardTokenCount(resp *kubeletdeviceplugin.ListAndWatchResponse, res Resource) int {
	n := 0
	for i := range resp.Devices {
		if ru, err := ConvertResourceUnitFromDeviceIds(resp.Devices[i].ID); err == nil && ru.Resource == res {
			n++
		}
	}
	return n
}

// TestDevicesReconciler_ReleaseOnPodTermination verifies the release counting once a Pod is done:
// the in-process reservation gates cross-mode allocation for the holding Pod's whole lifetime, so
// it must be pruned in the same live-pod-set sweep Reconcile uses — freeing the card for the
// opposite mode exactly when the Pod disappears — and it must NOT be dropped while the Pod is live.
// (Release via the ledger Status rebuild on Pod deletion is covered end-to-end by e2e CASE 22.)
func TestDevicesReconciler_ReleaseOnPodTermination(t *testing.T) {
	const nodeName = "node-rel"
	dev0 := Resource{Group: "grp-0", Device: "dev-0"}

	devs := crossModeDevices(nodeName, workercore.DeviceAllocationModeNone) // dev-0 free in the ledger
	podB := concurrentAllocatePod(nodeName, "b", "uid-b", workercore.DeviceAllocationModeShared, 1)

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, podB).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	sharedServer := &ResourceServer{
		Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: workercore.DeviceAllocationModeShared,
		Reconciler: rec, Responder: stubResponder{},
	}

	ctx := context.Background()
	oneCard := &AllocateRequest{ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}}}

	// Pod A (uid-a) holds dev-0 exclusively via the reservation the workload Allocate records; the
	// ledger Status stays free here, isolating the reservation as the authoritative release signal.
	rec.reserveDevices("uid-a", workercore.DevicesStatus{
		Groups: []workercore.DevicesAllocationGroup{{
			ID:           "grp-0",
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{{ID: "dev-0", Mode: workercore.DeviceAllocationModeExclusive}},
		}},
	})

	// While Pod A is still live (in the sweep's live-pod set), pruning keeps its reservation, so
	// dev-0 stays held against a shared claim at the Allocate gate. ListAndWatch still advertises it
	// (health-only): the cross-mode invariant lives in Allocate, not the advertisement.
	rec.pruneReservations([]string{"uid-a", "uid-b"})
	held, _ := sharedServer.cardHeldInOtherMode(mustGetDevices(t, rec), dev0)
	assert.True(t, held, "dev-0 must stay held while its exclusive Pod is live")
	lw, err := sharedServer.getListAndWatchResponse(ctx)
	require.NoError(t, err)
	assert.Greater(t, cardTokenCount(lw, dev0), 0, "a held card is still advertised (health-only)")

	// Pod A terminates: the same live-pod-set sweep that rebuilds the ledger Status prunes its
	// reservation, so the card frees for the opposite mode exactly when its Pod disappears.
	rec.pruneReservations([]string{"uid-b"})
	_, stillReserved := rec.reservedDevices("uid-a")
	assert.False(t, stillReserved, "the reservation must be pruned once Pod A is gone")
	held, _ = sharedServer.cardHeldInOtherMode(mustGetDevices(t, rec), dev0)
	assert.False(t, held, "dev-0 must be free for the opposite mode once its Pod is gone")

	// The freed card is reusable by the opposite mode: a shared claim now succeeds on dev-0.
	_, err = sharedServer.Allocate(ctx, oneCard)
	require.NoError(t, err, "a shared claim must succeed on the freed card")
	gotB := new(core.Pod)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(podB), gotB))
	allocated, err := extractAllocatedStatusFromPod(gotB)
	require.NoError(t, err)
	require.Len(t, allocated.Groups, 1)
	require.Len(t, allocated.Groups[0].Accelerators, 1)
	assert.Equal(t, workercore.DeviceAllocationModeShared, allocated.Groups[0].Accelerators[0].Mode)
}

// TestResourceServer_Allocate_RollsBackReservationOnPatchFailure verifies the release counting on
// the failure path: when the durable annotation patch fails after the in-process reservation was
// written, Allocate rolls the reservation back, so the card is not stranded for the opposite mode
// (the Pod-delete prune would otherwise never fire — it is gated on the annotation that never landed).
func TestResourceServer_Allocate_RollsBackReservationOnPatchFailure(t *testing.T) {
	const nodeName = "node-rb"
	devs := crossModeDevices(nodeName, workercore.DeviceAllocationModeNone)
	podA := concurrentAllocatePod(nodeName, "a", "uid-a", workercore.DeviceAllocationModeExclusive, 1)

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, podA).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			Patch: func(_ context.Context, _ ctrlcli.WithWatch, _ ctrlcli.Object, _ ctrlcli.Patch, _ ...ctrlcli.PatchOption) error {
				return errors.New("simulated annotation patch failure")
			},
		}).
		Build()

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	s := &ResourceServer{
		Manufacturer: nodefeature.ManufacturerNVIDIA, AllocationMode: workercore.DeviceAllocationModeExclusive,
		Reconciler: rec, Responder: stubResponder{},
	}

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}},
	})
	require.Error(t, err, "Allocate must fail when the annotation patch fails")

	_, reserved := rec.reservedDevices("uid-a")
	assert.False(t, reserved, "a failed patch must roll back the reservation so the card is not stranded")
}

// TestDevicesReconciler_Reservation verifies the in-process, pod-keyed reservation the
// workload Allocate records for the sidecar's visibility Allocate to reuse: reserve→read,
// empty inputs are no-ops, and pruning drops reservations whose pod is no longer live.
func TestDevicesReconciler_Reservation(t *testing.T) {
	statusFor := func(dev string) workercore.DevicesStatus {
		return workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.AcceleratorAllocation{
					{ID: dev, Mode: workercore.DeviceAllocationModeSliced},
				},
			}},
		}
	}

	r := &DevicesReconciler{}

	// An empty UID or an empty allocation is a no-op.
	r.reserveDevices("", statusFor("dev-0"))
	r.reserveDevices("p1", workercore.DevicesStatus{})
	_, ok := r.reservedDevices("p1")
	assert.False(t, ok, "empty allocation must not be reserved")

	// Reserve then read.
	r.reserveDevices("p1", statusFor("dev-0"))
	got, ok := r.reservedDevices("p1")
	require.True(t, ok)
	require.Len(t, got.Groups, 1)
	require.Len(t, got.Groups[0].Accelerators, 1)
	assert.Equal(t, "dev-0", got.Groups[0].Accelerators[0].ID)

	// A second pod coexists; pruning to a live set keeps it and drops the gone pod.
	r.reserveDevices("p2", statusFor("dev-1"))
	r.pruneReservations([]string{"p2"})
	_, ok = r.reservedDevices("p1")
	assert.False(t, ok, "p1 must be pruned when no longer live")
	got2, ok := r.reservedDevices("p2")
	require.True(t, ok, "p2 must survive the prune")
	assert.Equal(t, "dev-1", got2.Groups[0].Accelerators[0].ID)
}

// TestResourceServer_Allocate_RecordsReservation verifies the workload Allocate records an
// in-process reservation keyed by the pod UID, carrying the allocated device (ID + Index),
// so the sidecar's visibility Allocate can co-allocate the same physical device.
func TestResourceServer_Allocate_RecordsReservation(t *testing.T) {
	const nodeName = "node-7"
	resName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)

	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{
					ID:    "dev-0",
					Index: 0,
				}},
			}},
		},
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: "pod-uid-7"},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "main",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{resName: resource.MustParse("1")},
				},
			}},
		},
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, pod).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeSliced,
		Reconciler:     rec,
		Responder:      stubResponder{},
	}

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{
			DevicesIds: []string{"grp-0:dev-0:0000"},
		}},
	})
	require.NoError(t, err)

	got, ok := rec.reservedDevices("pod-uid-7")
	require.True(t, ok, "workload Allocate must record an in-process reservation")
	require.Len(t, got.Groups, 1)
	require.Len(t, got.Groups[0].Accelerators, 1)
	assert.Equal(t, "dev-0", got.Groups[0].Accelerators[0].ID)
}

// recordingResponder captures the allocated set the visibility Allocate hands the Responder.
type recordingResponder struct {
	gotAllocated map[Resource]int32
}

func (r *recordingResponder) GetContainerAllocateResponse(
	_ context.Context, _ *core.Pod, _ *core.Container, _ *workercore.Devices, allocated map[Resource]int32,
) (*ContainerAllocateResponse, error) {
	r.gotAllocated = allocated
	return &ContainerAllocateResponse{}, nil
}

// visibilityReservation builds a one-device reserved status for a pod.
func visibilityReservation(dev string) workercore.DevicesStatus {
	return workercore.DevicesStatus{
		Groups: []workercore.DevicesAllocationGroup{{
			ID:           "grp-0",
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{
				{ID: dev, Mode: workercore.DeviceAllocationModeSliced},
			},
		}},
	}
}

// TestResourceServer_GetListAndWatch_Visibility verifies the visibility mode advertises, per
// card, a flat pool of SlicedResourceMaxSize healthy tokens (via Resource.GetDeviceIds).
func TestResourceServer_GetListAndWatch_Visibility(t *testing.T) {
	const nodeName = "node-v"
	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{ID: "dev-0", Index: 0}},
			}},
		},
	}
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(devs).Build()

	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeVisibility,
		Reconciler:     &DevicesReconciler{NodeName: nodeName, Client: cli},
	}

	resp, err := s.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	require.Len(t, resp.Devices, nodefeature.SlicedResourceMaxSize, "one card advertises SlicedResourceMaxSize visibility tokens")
	for i := range resp.Devices {
		assert.Equal(t, kubeletdeviceplugin.Healthy, resp.Devices[i].Health)
	}
}

// TestResourceServer_GetListAndWatch_Sliced verifies the sliced mode advertises, per card, a token
// pool sized by the card's own logical slice count, and advertises nothing for a group whose cards
// carry no slicing capability.
func TestResourceServer_GetListAndWatch_Sliced(t *testing.T) {
	const nodeName = "node-s"
	cases := []struct {
		name         string
		logicalCount int32
		wantLen      int
	}{
		{
			name:         "nvidia soft cards advertise cards x logical count",
			logicalCount: 128,
			wantLen:      2 * 128,
		},
		{
			name:    "slice-less group advertises nothing",
			wantLen: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			twoCards := []workercore.Accelerator{
				{ID: "dev-0", Index: 0, Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: c.logicalCount}}},
				{ID: "dev-1", Index: 1, Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: c.logicalCount}}},
			}
			devs := &workercore.Devices{
				ObjectMeta: meta.ObjectMeta{Name: nodeName},
				Spec: workercore.DevicesSpec{
					Groups: []workercore.DevicesGroup{{
						ID:           "grp-0",
						Manufacturer: nodefeature.ManufacturerNVIDIA,
						Accelerators: twoCards,
					}},
				},
			}
			cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(devs).Build()
			s := &ResourceServer{
				Manufacturer:   nodefeature.ManufacturerNVIDIA,
				AllocationMode: workercore.DeviceAllocationModeSliced,
				Reconciler:     &DevicesReconciler{NodeName: nodeName, Client: cli},
			}
			resp, err := s.getListAndWatchResponse(context.Background())
			require.NoError(t, err)
			assert.Len(t, resp.Devices, c.wantLen)
		})
	}
}

// TestResourceServer_Allocate_Visibility verifies the visibility Allocate reuses the pod's
// reserved device(s) — recorded by the workload Allocate — as the allocated set handed to the
// Responder, without writing any allocation status (no ledger consumption).
func TestResourceServer_Allocate_Visibility(t *testing.T) {
	const nodeName = "node-v1"
	visName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)

	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{ID: "dev-0", Index: 0}},
			}},
		},
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: "pod-uid-v"},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "sshd",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{visName: resource.MustParse("1")},
				},
			}},
		},
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, pod).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	// Simulate the workload container's earlier Allocate having recorded its device.
	rec.reserveDevices("pod-uid-v", visibilityReservation("dev-0"))

	responder := &recordingResponder{}
	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeVisibility,
		Reconciler:     rec,
		Responder:      responder,
	}

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{
			DevicesIds: []string{"visibility:0000"},
		}},
	})
	require.NoError(t, err)

	// The responder saw exactly main's reserved device.
	require.Len(t, responder.gotAllocated, 1)
	_, ok := responder.gotAllocated[Resource{Group: "grp-0", Device: "dev-0"}]
	assert.True(t, ok, "visibility Allocate must reuse the reserved device")

	// Visibility writes no allocation status: the pod annotation stays unset.
	got := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
	_, annotated := got.Annotations[AllocatedAcceleratorAnnoKey]
	assert.False(t, annotated, "visibility must not patch the allocation annotation")
}

// TestResourceServer_Allocate_Visibility_FailsClosed verifies the visibility Allocate errors
// when the pod has no reservation, rather than emitting an empty visible-devices env.
func TestResourceServer_Allocate_Visibility_FailsClosed(t *testing.T) {
	const nodeName = "node-v2"
	visName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)

	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{ID: "dev-0", Index: 0}},
			}},
		},
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: "pod-uid-v2"},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "sshd",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{visName: resource.MustParse("1")},
				},
			}},
		},
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, pod).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()

	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeVisibility,
		Reconciler:     &DevicesReconciler{NodeName: nodeName, Client: cli}, // no reservation
		Responder:      &recordingResponder{},
	}

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{
			DevicesIds: []string{"visibility:0000"},
		}},
	})
	require.Error(t, err, "visibility Allocate must fail closed without a reservation")
}

// TestResourceServer_Allocate_Visibility_StaleReservation verifies the visibility Allocate
// fails closed when the reserved device is no longer in the node inventory, rather than
// delegating to the Responder (which would emit an empty visible-devices env).
func TestResourceServer_Allocate_Visibility_StaleReservation(t *testing.T) {
	const nodeName = "node-v3"
	visName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)

	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{ID: "dev-0", Index: 0}},
			}},
		},
	}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: "pod-uid-v3"},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "sshd",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{visName: resource.MustParse("1")},
				},
			}},
		},
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, pod).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	// The reservation points at a device that is no longer present in devs (only dev-0 is).
	rec.reserveDevices("pod-uid-v3", visibilityReservation("dev-9"))

	responder := &recordingResponder{}
	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeVisibility,
		Reconciler:     rec,
		Responder:      responder,
	}

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{
			DevicesIds: []string{"visibility:0000"},
		}},
	})
	require.Error(t, err, "visibility Allocate must fail closed when the reserved device is gone")
	assert.Nil(t, responder.gotAllocated, "the Responder must not be invoked with a stale reservation")
}

// TestResourceServer_Allocate_Visibility_CountMismatch verifies the visibility Allocate fails closed
// when the request asks for more devices than the pod reserved, rather than over-granting the sidecar
// visibility to the full reserved set (least-privilege: the grant must match the request exactly).
func TestResourceServer_Allocate_Visibility_CountMismatch(t *testing.T) {
	const nodeName = "node-v4"
	visName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)

	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{{ID: "dev-0", Index: 0}},
			}},
		},
	}
	// The pod requests visibility for 2 devices, but only one device is reserved.
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: "pod-uid-v4"},
		Spec: core.PodSpec{
			NodeName: nodeName,
			Containers: []core.Container{{
				Name: "sshd",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{visName: resource.MustParse("2")},
				},
			}},
		},
	}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, pod).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	rec.reserveDevices("pod-uid-v4", visibilityReservation("dev-0")) // only 1 device reserved

	responder := &recordingResponder{}
	s := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeVisibility,
		Reconciler:     rec,
		Responder:      responder,
	}

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{
			DevicesIds: []string{"visibility:0000", "visibility:0001"}, // requests 2
		}},
	})
	require.Error(t, err, "visibility Allocate must fail closed when the request count exceeds the reserved device count")
	assert.Nil(t, responder.gotAllocated, "the Responder must not be invoked on a count mismatch")
}
