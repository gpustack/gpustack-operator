package deviceplugin

import (
	"context"
	"encoding/json"
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
	"gpustack.ai/gpustack/pkg/device"
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

// workloadContainer is the container name every fixture here gives the workload container that
// claims accelerators. Reservations are keyed by (pod, container), so a test that seeds one
// directly has to name a container the same way a real Allocate would.
const workloadContainer = "main"

// reserveWorkload seeds the reservation a workload container's Allocate would have written.
func reserveWorkload(r *DevicesReconciler, podUID types.UID, status workercore.DevicesStatus) {
	r.reserveDevices(podUID, workloadContainer, status, nil)
}

// reservedWorkload reads back the workload container's reservation.
func reservedWorkload(r *DevicesReconciler, podUID types.UID) (workercore.DevicesStatus, bool) {
	return r.reservedDevices(podUID, workloadContainer)
}

// exclusiveMatch is the (resource, quantity) pair an exclusive Allocate for one card carries.
func exclusiveMatch(skipReserved bool) _AllocationMatch {
	return _AllocationMatch{
		ResourceName: nodefeature.GetAcceleratableResourceName(
			nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive),
		Quantity:     resource.MustParse("1"),
		SkipReserved: skipReserved,
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
				reserveWorkload(rec, "sibling", workercore.DevicesStatus{
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

// TestResourceServer_GetListAndWatch_CrossModeWithhold verifies ListAndWatch keeps advertising a
// card held in a conflicting mode — via the ledger Status OR an in-process reservation — but
// reports its tokens Unhealthy, so kubelet can never assign one to an opposite-mode pod
// (removing the tokens would strand kubelet's checkpointed allocations on re-registration).
// A same-mode hold and the Visibility server keep the tokens Healthy: the former still accepts
// same-mode co-tenants, the latter must co-allocate the held card for the SSH sidecar.
func TestResourceServer_GetListAndWatch_CrossModeWithhold(t *testing.T) {
	const nodeName = "node-wh"
	dev0 := Resource{Group: "grp-0", Device: "dev-0"}
	dev1 := Resource{Group: "grp-0", Device: "dev-1"}

	// dev-0 held Exclusive in the ledger Status; dev-1 held Exclusive only via a reservation
	// (ledger lagging).
	devs := twoCardDevices(nodeName, workercore.DeviceAllocationModeExclusive)
	cli := nodeFixture(devs)

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	reserveWorkload(rec, "sibling", workercore.DevicesStatus{
		Groups: []workercore.DevicesAllocationGroup{{
			ID:           "grp-0",
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{{ID: "dev-1", Mode: workercore.DeviceAllocationModeExclusive}},
		}},
	})
	server := func(mode workercore.DeviceAllocationMode) *ResourceServer {
		return &ResourceServer{
			Manufacturer:   nodefeature.ManufacturerNVIDIA,
			AllocationMode: mode,
			Reconciler:     rec,
		}
	}

	// Shared server: both held cards stay advertised, but none of their tokens is healthy.
	resp, err := server(workercore.DeviceAllocationModeShared).getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	assert.Greater(t, cardTokenCount(resp, dev0), 0, "a card held in another mode via Status stays advertised")
	assert.Greater(t, cardTokenCount(resp, dev1), 0, "a card held in another mode via reservation stays advertised")
	assert.Zero(t, cardHealthyTokenCount(resp, dev0), "a card held in another mode via Status reports Unhealthy")
	assert.Zero(t, cardHealthyTokenCount(resp, dev1), "a card held in another mode via reservation reports Unhealthy")

	// Exclusive server: the hold is same-mode, so the tokens stay Healthy (kubelet's own
	// accounting has already consumed the card's single exclusive token anyway).
	resp, err = server(workercore.DeviceAllocationModeExclusive).getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	assert.Greater(t, cardHealthyTokenCount(resp, dev0), 0, "a same-mode hold keeps tokens healthy")
	assert.Greater(t, cardHealthyTokenCount(resp, dev1), 0, "a same-mode hold keeps tokens healthy")

	// Visibility server: exempt — the SSH sidecar must co-allocate the held card.
	resp, err = server(workercore.DeviceAllocationModeVisibility).getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	assert.Greater(t, cardHealthyTokenCount(resp, dev0), 0, "visibility keeps a held card's tokens healthy")
	assert.Greater(t, cardHealthyTokenCount(resp, dev1), 0, "visibility keeps a held card's tokens healthy")
}

// TestResourceServer_GetListAndWatch_PerCardSlicedTokens verifies each family's token pool is
// sized per card from that card's own capability, on a node mixing both card populations: the
// logical pool covers the logically sliceable cards only, sized by each one's LogicalSliced.Count, and the
// partition pool covers the MIG card only, sized by its PhysicalSliced.Count. The whole-card
// families cover the logical cards and skip the MIG card.
func TestResourceServer_GetListAndWatch_PerCardSlicedTokens(t *testing.T) {
	const nodeName = "node-pc"
	logical0 := Resource{Group: "grp-0", Device: "logical-0"}
	logical1 := Resource{Group: "grp-0", Device: "logical-1"}
	mig := Resource{Group: "grp-0", Device: "mig-0"}

	// Two logical cards (128 slices each) + one MIG card (7 physical instances): each card's own
	// per-card capability sizes its token pool independently.
	newFmt := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{
					{ID: "logical-0", Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 128}}},
					{ID: "logical-1", Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 128}}},
					{ID: "mig-0", Status: workercore.AcceleratorStatus{PhysicalSliced: workercore.AcceleratorPhysicalSliced{Count: 7}}},
				},
			}},
		},
	}

	server := func(devs *workercore.Devices, mode workercore.DeviceAllocationMode) *ResourceServer {
		cli := nodeFixture(devs)
		return &ResourceServer{
			Manufacturer:   nodefeature.ManufacturerNVIDIA,
			AllocationMode: mode,
			Reconciler:     &DevicesReconciler{NodeName: nodeName, Client: cli},
		}
	}

	t.Run("logical pool covers the logically sliceable cards only", func(t *testing.T) {
		resp, err := server(newFmt, workercore.DeviceAllocationModeSliced).getListAndWatchResponse(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 128, cardTokenCount(resp, logical0), "logical card advertises LogicalSliced.Count tokens")
		assert.Equal(t, 128, cardTokenCount(resp, logical1), "logical card advertises LogicalSliced.Count tokens")
		assert.Zero(t, cardTokenCount(resp, mig), "a MIG card serves no logical slice")
	})

	t.Run("partition pool covers the mig card only", func(t *testing.T) {
		resp, err := server(newFmt, workercore.DeviceAllocationModePartitioned).getListAndWatchResponse(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 7, cardTokenCount(resp, mig), "MIG card advertises PhysicalSliced.Count tokens")
		assert.Zero(t, cardTokenCount(resp, logical0), "an unpartitioned card serves no partition")
		assert.Zero(t, cardTokenCount(resp, logical1), "an unpartitioned card serves no partition")
	})

	t.Run("exclusive mode ignores the sliced count and skips the mig card", func(t *testing.T) {
		resp, err := server(newFmt, workercore.DeviceAllocationModeExclusive).getListAndWatchResponse(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 1, cardTokenCount(resp, logical0), "exclusive advertises one token per card")
		assert.Zero(t, cardTokenCount(resp, mig), "a MIG card serves no whole-card claim")
	})
}

// cardWithCapability builds one accelerator carrying only the slicing capability under test:
// logicalCount > 0 is an unpartitioned card, physicalCount > 0 a card in a partitioning mode,
// both zero a card that reports neither.
func cardWithCapability(id string, index uint32, logicalCount, physicalCount int32) workercore.Accelerator {
	return workercore.Accelerator{
		ID:    id,
		Index: index,
		Status: workercore.AcceleratorStatus{
			LogicalSliced:  workercore.AcceleratorLogicalSliced{Count: logicalCount},
			PhysicalSliced: workercore.AcceleratorPhysicalSliced{Count: physicalCount},
		},
	}
}

// TestResourceServer_GetListAndWatch_TokenSetPerCardState pins the token pool every mode
// advertises for every card state. Each family draws only from the card population that can
// physically serve it: an unpartitioned card advertises the whole-card families and a logical
// slice pool sized by its own logical slice count, while a card in a partitioning mode advertises
// nothing but its partition pool. Visibility is advertised on every card whatever its state, so
// a sidecar can always co-allocate the card its workload holds.
func TestResourceServer_GetListAndWatch_TokenSetPerCardState(t *testing.T) {
	const nodeName = "node-pin"
	card := Resource{Group: "grp-0", Device: "dev-0"}

	cases := []struct {
		name          string
		logicalCount  int32
		physicalCount int32
		want          map[workercore.DeviceAllocationMode]int
	}{
		{
			name:         "unpartitioned card",
			logicalCount: 128,
			want: map[workercore.DeviceAllocationMode]int{
				workercore.DeviceAllocationModeExclusive: 1,
				workercore.DeviceAllocationModeShared:    nodefeature.SharedResourceMaxSize,
				// The logical pool is sized by the card's own logical slice count.
				workercore.DeviceAllocationModeSliced: 128,
				// A card that is not partitioned serves no partition claim.
				workercore.DeviceAllocationModePartitioned: 0,
				workercore.DeviceAllocationModeVisibility:  nodefeature.SlicedResourceMaxSize,
			},
		},
		{
			name:          "card in a partitioning mode",
			physicalCount: 7,
			want: map[workercore.DeviceAllocationMode]int{
				// A partitioned card cannot serve a whole-card claim, nor a logical slice:
				// none of the three advertise it at all.
				workercore.DeviceAllocationModeExclusive: 0,
				workercore.DeviceAllocationModeShared:    0,
				workercore.DeviceAllocationModeSliced:    0,
				// The partition pool is sized by the card's partition ceiling.
				workercore.DeviceAllocationModePartitioned: 7,
				workercore.DeviceAllocationModeVisibility:  nodefeature.SlicedResourceMaxSize,
			},
		},
		{
			name: "card reporting neither capability",
			want: map[workercore.DeviceAllocationMode]int{
				workercore.DeviceAllocationModeExclusive: 1,
				workercore.DeviceAllocationModeShared:    nodefeature.SharedResourceMaxSize,
				// A zero-sized pool advertises no IDs at all.
				workercore.DeviceAllocationModeSliced:      0,
				workercore.DeviceAllocationModePartitioned: 0,
				workercore.DeviceAllocationModeVisibility:  nodefeature.SlicedResourceMaxSize,
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			devs := &workercore.Devices{
				ObjectMeta: meta.ObjectMeta{Name: nodeName},
				Spec: workercore.DevicesSpec{
					Groups: []workercore.DevicesGroup{{
						ID:           "grp-0",
						Manufacturer: nodefeature.ManufacturerNVIDIA,
						Accelerators: []workercore.Accelerator{
							cardWithCapability("dev-0", 0, c.logicalCount, c.physicalCount),
						},
					}},
				},
			}
			cli := nodeFixture(devs)

			for mode, want := range c.want {
				s := &ResourceServer{
					Manufacturer:   nodefeature.ManufacturerNVIDIA,
					AllocationMode: mode,
					Reconciler:     &DevicesReconciler{NodeName: nodeName, Client: cli},
				}
				resp, err := s.getListAndWatchResponse(context.Background())
				require.NoError(t, err)
				assert.Equal(t, want, cardTokenCount(resp, card), "mode %s", mode)
				assert.Equal(t, want, cardHealthyTokenCount(resp, card),
					"an unheld card's tokens are all healthy, mode %s", mode)
			}
		})
	}
}

// TestResourceServer_Allocate_LedgerCostPerMode pins what one token costs a card in the ledger,
// per mode, as it stands today. A token-set assertion cannot see this: two modes can advertise
// the same tokens and charge wildly different amounts for one. It matters most for what the
// switch does with a mode it does not name — it charges a whole card — so any mode added later
// silently consumes the card unless it is given a branch of its own.
func TestResourceServer_Allocate_LedgerCostPerMode(t *testing.T) {
	const nodeName = "node-cost"
	slicedUnits := int64(nodefeature.ResourceMaxUnits / 4) // a quarter-card slice

	cases := []struct {
		name string
		mode workercore.DeviceAllocationMode
		want int32
	}{
		{
			name: "exclusive costs the whole card",
			mode: workercore.DeviceAllocationModeExclusive,
			want: nodefeature.ResourceMaxUnits,
		},
		{
			name: "shared costs one ownership share",
			mode: workercore.DeviceAllocationModeShared,
			want: nodefeature.ResourceMaxUnits / nodefeature.SharedResourceMaxSize,
		},
		{
			name: "sliced costs the container's own per-card units",
			mode: workercore.DeviceAllocationModeSliced,
			want: int32(slicedUnits),
		},
		{
			// A partition must never fall through to the whole-card default: one small
			// instance would then look like it owned the card and hide the rest of its
			// geometry from every consumer of the scalar remaining.
			name: "partitioned costs the instance's own per-card units",
			mode: workercore.DeviceAllocationModePartitioned,
			want: int32(slicedUnits),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var (
				pod       *core.Pod
				devs      *workercore.Devices
				responder ContainerAllocateResponder = stubResponder{}
			)
			if c.mode == workercore.DeviceAllocationModePartitioned {
				// A partition needs a card that can actually host its geometry, and an
				// actuator to materialize it.
				devs = partitionedDevices(nodeName,
					partitionedCard("dev-0", 0, "1g.10gb",
						workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 2}))
				pod = partitionPod(nodeName, "p", "uid-cost", "1g.10gb", slicedUnits)
				responder = physicalActuatorResponder{
					placements: map[Resource][]workercore.AcceleratorPhysicalPlacement{
						{Group: "grp-0", Device: "dev-0"}: {{Start: 0, Length: 2}},
					},
				}
			} else {
				resName := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, c.mode)
				limits := core.ResourceList{resName: resource.MustParse("1")}
				if c.mode == workercore.DeviceAllocationModeSliced {
					limits[nodefeature.GetAcceleratableSlicedUnitsResourceName(nodefeature.ManufacturerNVIDIA)] = *resource.NewQuantity(slicedUnits, resource.DecimalSI)
				}
				devs = crossModeDevices(nodeName, workercore.DeviceAllocationModeNone)
				pod = &core.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: "uid-cost"},
					Spec: core.PodSpec{
						NodeName:   nodeName,
						Containers: []core.Container{{Name: workloadContainer, Resources: core.ResourceRequirements{Limits: limits}}},
					},
				}
			}
			cli := nodeFixture(devs, pod)

			rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
			s := &ResourceServer{
				Manufacturer:   nodefeature.ManufacturerNVIDIA,
				AllocationMode: c.mode,
				Reconciler:     rec,
				Responder:      responder,
			}
			_, err := s.Allocate(context.Background(), &AllocateRequest{
				ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}},
			})
			require.NoError(t, err)

			reserved, ok := reservedWorkload(rec, "uid-cost")
			require.True(t, ok)
			require.Len(t, reserved.Groups, 1)
			require.Len(t, reserved.Groups[0].Accelerators, 1)
			assert.Equal(t, c.want, reserved.Groups[0].Accelerators[0].Allocated)
		})
	}
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

// TestDevicesReconciler_GetAllocatingPod_SkipReserved verifies the container-identification skip
// that underlies distinct-pod attribution: with two identical pending pods, skipReserved=false
// returns the oldest (the legacy guess), while skipReserved=true skips a container already holding
// a reservation and returns the next one. Once every candidate is reserved the search replays the
// oldest instead of erroring, so a kubelet that lost its checkpoint and is asking again gets an
// answer rather than a permanently failed admission.
func TestDevicesReconciler_GetAllocatingPod_SkipReserved(t *testing.T) {
	const nodeName = "node-skip"
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
	exclusiveOn := func(dev string) workercore.DevicesStatus {
		return workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.AcceleratorAllocation{{ID: dev, Mode: workercore.DeviceAllocationModeExclusive}},
			}},
		}
	}

	// No reservation yet: the oldest pending pod is returned.
	got, _, err := rec.getAllocatingPod(context.Background(), exclusiveMatch(true))
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-x"), got.UID, "oldest pending pod when none is reserved")

	// Reserve podX: skipReserved=true now skips it and returns podY; skipReserved=false still returns podX.
	reserveWorkload(rec, "uid-x", exclusiveOn("dev-0"))
	got, _, err = rec.getAllocatingPod(context.Background(), exclusiveMatch(true))
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-y"), got.UID, "skipReserved must skip the already-reserved container")

	got, _, err = rec.getAllocatingPod(context.Background(), exclusiveMatch(false))
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-x"), got.UID, "without skipReserved the oldest pod is still returned")

	// Every candidate reserved: replay the oldest rather than fail the call.
	reserveWorkload(rec, "uid-y", exclusiveOn("dev-1"))
	got, _, err = rec.getAllocatingPod(context.Background(), exclusiveMatch(true))
	require.NoError(t, err, "an all-reserved candidate set must not error")
	assert.Equal(t, types.UID("uid-x"), got.UID, "the oldest reserved candidate is replayed")
}

// TestDevicesReconciler_GetAllocatingPod_Feasibility verifies the only disambiguator available for
// the request dimensions the Allocate RPC does not carry: two pods asking for the same coarse
// resource but very different per-card budgets are told apart by which of them the offered card
// can still hold. When no candidate fits, the search still answers — the test disambiguates, it
// does not gate admission.
func TestDevicesReconciler_GetAllocatingPod_Feasibility(t *testing.T) {
	const nodeName = "node-feas"
	slicedRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced)
	unitsRes := nodefeature.GetAcceleratableSlicedUnitsResourceName(nodefeature.ManufacturerNVIDIA)

	slicePod := func(name, uid string, units int64, createdAfter time.Duration) *core.Pod {
		return &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Name: name, Namespace: "default", UID: types.UID(uid),
				CreationTimestamp: meta.NewTime(time.Now().Add(createdAfter)),
			},
			Spec: core.PodSpec{
				NodeName: nodeName,
				Containers: []core.Container{{
					Name: workloadContainer,
					Resources: core.ResourceRequirements{
						Limits: core.ResourceList{
							slicedRes: resource.MustParse("1"),
							unitsRes:  *resource.NewQuantity(units, resource.DecimalSI),
						},
					},
				}},
			},
		}
	}
	// The big slice is the older pod, so the plain oldest-pending guess would always pick it.
	big := slicePod("big", "uid-big", int64(nodefeature.ResourceMaxUnits*3/4), 0)
	small := slicePod("small", "uid-small", int64(nodefeature.ResourceMaxUnits/8), time.Second)

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(big, small).
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
	offered := map[Resource][]ResourceUnit{{Group: "grp-0", Device: "dev-0"}: {{Index: 0}}}
	matchWithRemaining := func(remaining int32) _AllocationMatch {
		devs := crossModeDevices(nodeName, workercore.DeviceAllocationModeSliced)
		devs.Status.Groups[0].Accelerators[0].Remaining = remaining
		return _AllocationMatch{
			ResourceName: slicedRes,
			Quantity:     resource.MustParse("1"),
			SkipReserved: true,
			Feasible:     s.candidateFeasible(devs, offered, nil),
		}
	}

	// A quarter of the card's units are left: only the small slice still fits.
	got, _, err := rec.getAllocatingPod(context.Background(), matchWithRemaining(nodefeature.ResourceMaxUnits/4))
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-small"), got.UID,
		"the candidate the offered card can still hold wins over the older one it cannot")

	// With the card empty both fit again, so the oldest-pending tie-break decides.
	got, _, err = rec.getAllocatingPod(context.Background(), matchWithRemaining(nodefeature.ResourceMaxUnits))
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-big"), got.UID, "interchangeable candidates fall back to oldest-pending")

	// With no room for either, the search still resolves: feasibility disambiguates, it does not
	// reject — admission is the webhook's and the admission check's job.
	got, _, err = rec.getAllocatingPod(context.Background(), matchWithRemaining(0))
	require.NoError(t, err, "an all-infeasible candidate set must not turn into a hard failure")
	assert.Equal(t, types.UID("uid-big"), got.UID)
}

// TestResourceServer_CandidateFeasible_Partitioned pins the partition side of the same seam:
// the Allocate RPC carries no profile, so two pending containers asking for the same family are
// indistinguishable to it. A partition's demand is its geometry, and the offered tokens name no
// card the allocation will use, so the test is against the whole node — the candidate whose
// profile the node can still host wins over an older one it cannot.
func TestResourceServer_CandidateFeasible_Partitioned(t *testing.T) {
	const nodeName = "node-feasible-partition"
	partitionRes := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModePartitioned)

	agedPartitionPod := func(name, uid, profile string, age time.Duration) *core.Pod {
		pod := partitionPod(nodeName, name, uid, profile, 0)
		pod.CreationTimestamp = meta.NewTime(pod.CreationTimestamp.Add(age))
		return pod
	}
	// The whole-card request is the older pod, so the plain oldest-pending guess always picks it.
	whole := agedPartitionPod("whole", "uid-whole", "7g.80gb", 0)
	small := agedPartitionPod("small", "uid-small", "1g.10gb", time.Second)

	cli := nodeFixture(whole, small)
	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	s := partitionServer(rec, stubResponder{})

	matchAgainst := func(devs *workercore.Devices) _AllocationMatch {
		return _AllocationMatch{
			ResourceName: partitionRes,
			Quantity:     resource.MustParse("1"),
			SkipReserved: true,
			Feasible:     s.candidateFeasible(devs, nil, nil),
		}
	}

	// The card offers only the small geometry, so only the small candidate can be this call's.
	smallOnly := partitionedDevices(nodeName,
		partitionedCard("dev-0", 0, "1g.10gb", workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 2}))
	got, _, err := rec.getAllocatingPod(context.Background(), matchAgainst(smallOnly))
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-small"), got.UID,
		"the candidate whose profile the node can host wins over the older one it cannot")

	// A card offering both geometries makes them interchangeable again, so oldest-pending decides.
	bothCard := partitionedCard("dev-0", 0, "1g.10gb", workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 2})
	bothCard.Status.PhysicalSliced.Profiles = append(bothCard.Status.PhysicalSliced.Profiles,
		workercore.AcceleratorPhysicalSlicedProfile{
			Name: "7g.80gb", Count: 1,
			Placements: []workercore.AcceleratorPhysicalPlacement{{Start: 0, Length: 8}},
		})
	got, _, err = rec.getAllocatingPod(context.Background(), matchAgainst(partitionedDevices(nodeName, bothCard)))
	require.NoError(t, err)
	assert.Equal(t, types.UID("uid-whole"), got.UID, "interchangeable candidates fall back to oldest-pending")

	// With no partitioned card at all the search still resolves: feasibility disambiguates, it
	// does not reject — Allocate itself is what refuses when the node has no room.
	got, _, err = rec.getAllocatingPod(context.Background(), matchAgainst(partitionedDevices(nodeName)))
	require.NoError(t, err, "an all-infeasible candidate set must not turn into a hard failure")
	assert.Equal(t, types.UID("uid-whole"), got.UID)
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
	resX, okX := reservedWorkload(rec, "uid-x")
	resY, okY := reservedWorkload(rec, "uid-y")
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

// cardHealthyTokenCount counts the healthy device tokens a ListAndWatch response advertises for
// one physical card.
func cardHealthyTokenCount(resp *kubeletdeviceplugin.ListAndWatchResponse, res Resource) int {
	n := 0
	for i := range resp.Devices {
		if resp.Devices[i].Health != kubeletdeviceplugin.Healthy {
			continue
		}
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
	reserveWorkload(rec, "uid-a", workercore.DevicesStatus{
		Groups: []workercore.DevicesAllocationGroup{{
			ID:           "grp-0",
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{{ID: "dev-0", Mode: workercore.DeviceAllocationModeExclusive}},
		}},
	})

	// While Pod A is still live (in the sweep's live-pod set), pruning keeps its reservation, so
	// dev-0 stays held against a shared claim at the Allocate gate. ListAndWatch keeps advertising
	// its tokens but reports them Unhealthy, so kubelet can never hand the held card to a shared pod.
	rec.pruneReservations([]string{"uid-a", "uid-b"})
	held, _ := sharedServer.cardHeldInOtherMode(mustGetDevices(t, rec), dev0)
	assert.True(t, held, "dev-0 must stay held while its exclusive Pod is live")
	lw, err := sharedServer.getListAndWatchResponse(ctx)
	require.NoError(t, err)
	assert.Greater(t, cardTokenCount(lw, dev0), 0, "a held card stays advertised")
	assert.Zero(t, cardHealthyTokenCount(lw, dev0), "a held card's tokens report Unhealthy")

	// Pod A terminates: the same live-pod-set sweep that rebuilds the ledger Status prunes its
	// reservation, so the card frees for the opposite mode exactly when its Pod disappears.
	rec.pruneReservations([]string{"uid-b"})
	_, stillReserved := reservedWorkload(rec, "uid-a")
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

	_, reserved := reservedWorkload(rec, "uid-a")
	assert.False(t, reserved, "a failed patch must roll back the reservation so the card is not stranded")
}

// reservationStatusFor builds a one-card sliced allocation for a reservation fixture.
func reservationStatusFor(dev string) workercore.DevicesStatus {
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

// TestDevicesReconciler_Reservation verifies the in-process reservation the workload Allocate
// records for the sidecar's visibility Allocate to reuse: reserve→read, empty inputs are no-ops,
// and pruning drops reservations whose pod is no longer live.
func TestDevicesReconciler_Reservation(t *testing.T) {
	r := &DevicesReconciler{}

	// An empty UID, an empty container name or an empty allocation is a no-op.
	reserveWorkload(r, "", reservationStatusFor("dev-0"))
	r.reserveDevices("p1", "", reservationStatusFor("dev-0"), nil)
	reserveWorkload(r, "p1", workercore.DevicesStatus{})
	_, ok := reservedWorkload(r, "p1")
	assert.False(t, ok, "empty allocation must not be reserved")

	// Reserve then read.
	reserveWorkload(r, "p1", reservationStatusFor("dev-0"))
	got, ok := reservedWorkload(r, "p1")
	require.True(t, ok)
	require.Len(t, got.Groups, 1)
	require.Len(t, got.Groups[0].Accelerators, 1)
	assert.Equal(t, "dev-0", got.Groups[0].Accelerators[0].ID)

	// A second pod coexists; pruning to a live set keeps it and drops the gone pod.
	reserveWorkload(r, "p2", reservationStatusFor("dev-1"))
	r.pruneReservations([]string{"p2"})
	_, ok = reservedWorkload(r, "p1")
	assert.False(t, ok, "p1 must be pruned when no longer live")
	got2, ok := reservedWorkload(r, "p2")
	require.True(t, ok, "p2 must survive the prune")
	assert.Equal(t, "dev-1", got2.Groups[0].Accelerators[0].ID)
}

// TestDevicesReconciler_Reservation_PerContainer verifies that a pod's containers hold separate
// reservations. Keyed by pod alone, the first container served would mask every later one — the
// already-reserved skip would refuse to resolve them — so two containers each taking a card could
// never both be served, and the sidecar's lookup could pick up its own claim instead of the
// workload's.
func TestDevicesReconciler_Reservation_PerContainer(t *testing.T) {
	r := &DevicesReconciler{}

	r.reserveDevices("p1", "init", reservationStatusFor("dev-0"), []string{"grp-0:dev-0:0000"})
	r.reserveDevices("p1", "main", reservationStatusFor("dev-1"), []string{"grp-0:dev-1:0000"})

	initReserved, ok := r.reservedDevices("p1", "init")
	require.True(t, ok, "the first container's reservation must survive the second")
	assert.Equal(t, "dev-0", initReserved.Groups[0].Accelerators[0].ID)
	mainReserved, ok := r.reservedDevices("p1", "main")
	require.True(t, ok)
	assert.Equal(t, "dev-1", mainReserved.Groups[0].Accelerators[0].ID)

	// A container with no reservation of its own is unclaimed, so its Allocate still resolves.
	_, ok = r.reservedDevices("p1", "sshd")
	assert.False(t, ok, "an unserved container must not inherit a sibling's reservation")

	// The sidecar co-allocates a sibling's accelerator claim, never its own, and the owner name
	// reported alongside it is the very container those devices were read from — a caller that
	// looks up a per-container record by owner must not read it against another one's cards.
	sidecarView, owner, ok := r.reservedAcceleratorDevices("p1", "sshd")
	require.True(t, ok, "the sidecar must see the pod's accelerator reservation")
	assert.Equal(t, "init", owner, "the owner must be the container the devices were read from")
	assert.Equal(t, "dev-0", sidecarView.Groups[0].Accelerators[0].ID)
	_, _, ok = r.reservedAcceleratorDevices("p2", "sshd")
	assert.False(t, ok, "another pod's reservation must not leak")

	// Releasing one container leaves the other alone; pruning the pod drops both.
	r.releaseReservation("p1", "init")
	_, ok = r.reservedDevices("p1", "init")
	assert.False(t, ok)
	_, ok = r.reservedDevices("p1", "main")
	assert.True(t, ok, "releasing one container must not release its siblings")
	r.pruneReservations(nil)
	_, ok = r.reservedDevices("p1", "main")
	assert.False(t, ok, "no reservation may outlive its pod")
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

	got, ok := reservedWorkload(rec, "pod-uid-7")
	require.True(t, ok, "workload Allocate must record an in-process reservation")
	require.Len(t, got.Groups, 1)
	require.Len(t, got.Groups[0].Accelerators, 1)
	assert.Equal(t, "dev-0", got.Groups[0].Accelerators[0].ID)
}

// physicalActuatorResponder is a stubResponder that also implements PhysicalSlicedResponder,
// returning a canned partition allocation, so the server's physical-slice branch (detect →
// actuate → fold placement → patch → return the actuator response) is tested without NVML.
type physicalActuatorResponder struct {
	stubResponder
	placements map[Resource][]workercore.AcceleratorPhysicalPlacement
	rolledBack *bool
	actErr     error
}

func (r physicalActuatorResponder) ActuatePhysicalSliced(
	_ context.Context, _ *core.Pod, _ *core.Container, _ *workercore.Devices,
	_ map[Resource]int32, profile string,
) (*PhysicalSlicedAllocation, error) {
	if r.actErr != nil {
		return nil, r.actErr
	}
	return &PhysicalSlicedAllocation{
		Profile:    profile,
		Placements: r.placements,
		Response:   &ContainerAllocateResponse{Envs: map[string]string{"NVIDIA_VISIBLE_DEVICES": "MIG-actuated"}},
		Rollback:   func() { *r.rolledBack = true },
	}, nil
}

// echoActuatorResponder reports back exactly the placement the plugin selected and published
// into the reservation, so a test observes the plugin's own decision rather than a canned one.
type echoActuatorResponder struct {
	stubResponder
	rec *DevicesReconciler
}

func (r echoActuatorResponder) ActuatePhysicalSliced(
	_ context.Context, pod *core.Pod, ctr *core.Container, _ *workercore.Devices,
	allocated map[Resource]int32, profile string,
) (*PhysicalSlicedAllocation, error) {
	reserved, _ := r.rec.reservedDevices(pod.UID, ctr.Name)
	placements := make(map[Resource][]workercore.AcceleratorPhysicalPlacement, len(allocated))
	for i := range reserved.Groups {
		grp := &reserved.Groups[i]
		for j := range grp.Accelerators {
			acc := &grp.Accelerators[j]
			placements[Resource{Group: grp.ID, Device: acc.ID}] = acc.AllocatedPhysicalPlacements
		}
	}
	return &PhysicalSlicedAllocation{
		Profile:    profile,
		Placements: placements,
		Response:   &ContainerAllocateResponse{},
		Rollback:   func() {},
	}, nil
}

// partitionedCard builds one partitioned accelerator offering profile at the given legal
// placements — the detect-time capability a placement decision reads.
func partitionedCard(
	id string, index uint32, profile string, placements ...workercore.AcceleratorPhysicalPlacement,
) workercore.Accelerator {
	return workercore.Accelerator{
		ID: id, Index: index,
		Status: workercore.AcceleratorStatus{
			PhysicalSliced: workercore.AcceleratorPhysicalSliced{
				Count: int32(len(placements)),
				Profiles: []workercore.AcceleratorPhysicalSlicedProfile{
					{Name: profile, Count: int32(len(placements)), Placements: placements},
				},
			},
		},
	}
}

// partitionedDevices wraps cards into the single NVIDIA group of a node's Devices.
func partitionedDevices(nodeName string, cards ...workercore.Accelerator) *workercore.Devices {
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: cards,
			}},
		},
	}
}

// partitionPod builds a Pod requesting one partition of profile — the card key plus the
// per-profile key — with the ".partitioned.units" the Pod webhook folds when units > 0.
func partitionPod(nodeName, name, uid, profile string, units int64) *core.Pod {
	limits := core.ResourceList{
		nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModePartitioned): resource.MustParse("1"),
		nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, profile):                  resource.MustParse("1"),
	}
	if units > 0 {
		limits[nodefeature.GetAcceleratablePartitionedUnitsResourceName(nodefeature.ManufacturerNVIDIA)] = *resource.NewQuantity(units, resource.DecimalSI)
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
		Spec: core.PodSpec{
			NodeName:   nodeName,
			Containers: []core.Container{{Name: workloadContainer, Resources: core.ResourceRequirements{Limits: limits}}},
		},
	}
}

// partitionServer wires a Partitioned server over cli with a canned actuator.
func partitionServer(rec *DevicesReconciler, responder ContainerAllocateResponder) *ResourceServer {
	return &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModePartitioned,
		Reconciler:     rec,
		Responder:      responder,
	}
}

// nodeFixture builds a fake client over the given objects, indexed by node name — the index
// every path that reads the node's Pods needs, including ListAndWatch's held-ID scan.
func nodeFixture(objs ...ctrlcli.Object) ctrlcli.WithWatch {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()
}

// TestResourceServer_Allocate_Partitioned verifies the partition branch end to end: the plugin
// chooses the card, the actuator's placement is folded into the allocation annotation (the
// ledger's occupied source), and the actuator's response — the partition UUID env, no
// logical-slice artifacts — is returned in place of the logical-slice responder's.
func TestResourceServer_Allocate_Partitioned(t *testing.T) {
	const nodeName = "node-partition"
	const profile = "1g.10gb"

	devs := partitionedDevices(nodeName,
		partitionedCard("dev-0", 0, profile,
			workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 2},
			workercore.AcceleratorPhysicalPlacement{Start: 2, Length: 2}))
	pod := partitionPod(nodeName, "p", "pod-partition", profile, 0)
	cli := nodeFixture(devs, pod)

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	s := partitionServer(rec, physicalActuatorResponder{
		placements: map[Resource][]workercore.AcceleratorPhysicalPlacement{
			{Group: "grp-0", Device: "dev-0"}: {{Start: 0, Length: 2}},
		},
	})

	resp, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}},
	})
	require.NoError(t, err)
	require.Len(t, resp.ContainerResponses, 1)
	assert.Equal(t, "MIG-actuated", resp.ContainerResponses[0].Envs["NVIDIA_VISIBLE_DEVICES"])
	assert.NotContains(t, resp.ContainerResponses[0].Envs, "CUDA_DEVICE_MEMORY_SHARED_CACHE")

	got := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), got))
	allocated, err := extractAllocatedStatusFromPod(got)
	require.NoError(t, err)
	require.Len(t, allocated.Groups, 1)
	require.Len(t, allocated.Groups[0].Accelerators, 1)
	acc := allocated.Groups[0].Accelerators[0]
	assert.Equal(t, workercore.DeviceAllocationModePartitioned, acc.Mode)
	assert.Equal(t, "dev-0", acc.ID, "the annotation records the card the plugin actually used")
	assert.Equal(t, profile, acc.AllocatedPhysicalProfile)
	require.Len(t, acc.AllocatedPhysicalPlacements, 1)
	assert.Equal(t, int32(0), acc.AllocatedPhysicalPlacements[0].Start)
	assert.Equal(t, int32(2), acc.AllocatedPhysicalPlacements[0].Length)
}

// TestResourceServer_Allocate_PartitionedIgnoresOfferedCard pins the placement authority: the
// kubelet cannot know which card can host a geometry, so a request whose offered token names a
// card that cannot host the profile still runs — on a card that can. Without this the
// allocation would fail terminally on a node with room.
func TestResourceServer_Allocate_PartitionedIgnoresOfferedCard(t *testing.T) {
	const nodeName = "node-partition-offer"
	const profile = "7g.80gb"

	// dev-0 offers no placement for this profile at all; dev-1 offers one.
	devs := partitionedDevices(nodeName,
		partitionedCard("dev-0", 0, "1g.10gb", workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 2}),
		partitionedCard("dev-1", 1, profile, workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 8}))
	pod := partitionPod(nodeName, "p", "pod-offer", profile, 0)
	cli := nodeFixture(devs, pod)

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	s := partitionServer(rec, physicalActuatorResponder{
		placements: map[Resource][]workercore.AcceleratorPhysicalPlacement{
			{Group: "grp-0", Device: "dev-1"}: {{Start: 0, Length: 8}},
		},
	})

	// kubelet offers a token naming dev-0, the card that cannot host the profile.
	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}},
	})
	require.NoError(t, err)

	reserved, ok := reservedWorkload(rec, "pod-offer")
	require.True(t, ok)
	require.Len(t, reserved.Groups, 1)
	require.Len(t, reserved.Groups[0].Accelerators, 1)
	assert.Equal(t, "dev-1", reserved.Groups[0].Accelerators[0].ID,
		"the plugin must place on the card that can host the profile, not the one kubelet named")
}

// TestResourceServer_Allocate_PartitionedPublishesSelection pins the mutex-window guarantee: the
// first allocation's chosen intervals are visible in the reservation before it releases the node
// mutex, so a second request for a profile that cannot share the card lands on the other card
// rather than on a card that merely has not been carved yet.
func TestResourceServer_Allocate_PartitionedPublishesSelection(t *testing.T) {
	const nodeName = "node-partition-race"
	const profile = "7g.80gb"

	whole := workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 8}
	devs := partitionedDevices(nodeName,
		partitionedCard("dev-0", 0, profile, whole),
		partitionedCard("dev-1", 1, profile, whole))
	first := partitionPod(nodeName, "first", "pod-first", profile, 0)
	second := partitionPod(nodeName, "second", "pod-second", profile, 0)
	// The second pod is younger, so getAllocatingPod resolves the first call to "first".
	second.CreationTimestamp = meta.NewTime(first.CreationTimestamp.Add(time.Second))
	cli := nodeFixture(devs, first, second)

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	// The actuator echoes back whatever the plugin selected, so the test observes the decision
	// rather than a canned answer.
	s := partitionServer(rec, echoActuatorResponder{rec: rec})

	for range 2 {
		_, err := s.Allocate(context.Background(), &AllocateRequest{
			ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}},
		})
		require.NoError(t, err)
	}

	cardOf := func(uid string) string {
		reserved, ok := reservedWorkload(rec, types.UID(uid))
		require.True(t, ok, "pod %s must hold a reservation", uid)
		require.Len(t, reserved.Groups, 1)
		require.Len(t, reserved.Groups[0].Accelerators, 1)
		return reserved.Groups[0].Accelerators[0].ID
	}
	assert.NotEqual(t, cardOf("pod-first"), cardOf("pod-second"),
		"two whole-card partitions must land on different cards; the first publishes its selection "+
			"into the reservation before releasing the mutex")
}

// TestResourceServer_Allocate_PartitionedRetryReusesTheCard pins that a retried Allocate is
// idempotent. The kubelet re-runs Allocate for a container whose checkpoint it lost — a restart
// while the container was stopped — and by then this container's own placement is part of the
// node's occupancy. Deciding afresh would read it as somebody else's: a whole-card profile
// would report the node exhausted, and a node with a free sibling would place on THAT card,
// bypassing the vendor's reuse marker and carving a second instance.
func TestResourceServer_Allocate_PartitionedRetryReusesTheCard(t *testing.T) {
	const profile = "7g.80gb"
	whole := workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 8}

	cases := []struct {
		name      string
		cards     []workercore.Accelerator
		keepAlive bool // keep the in-process reservation, i.e. no device-manager restart
	}{
		{
			// One card, fully consumed by this container's own instance: a fresh decision
			// would report the node exhausted.
			name:      "the only card is reused rather than reported exhausted",
			cards:     []workercore.Accelerator{partitionedCard("dev-0", 0, profile, whole)},
			keepAlive: true,
		},
		{
			// A free sibling exists: a fresh decision would place there and carve a second
			// instance, orphaning the first until reclaim.
			name: "a free sibling does not attract a second instance",
			cards: []workercore.Accelerator{
				partitionedCard("dev-0", 0, profile, whole),
				partitionedCard("dev-1", 1, profile, whole),
			},
			keepAlive: true,
		},
		{
			// A device-manager restart clears the reservations, so the durable annotation is
			// the only record left — and it must be enough.
			name: "the annotation alone survives a device-manager restart",
			cards: []workercore.Accelerator{
				partitionedCard("dev-0", 0, profile, whole),
				partitionedCard("dev-1", 1, profile, whole),
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			const nodeName = "node-partition-retry"
			pod := partitionPod(nodeName, "p", "pod-retry", profile, 0)
			cli := nodeFixture(partitionedDevices(nodeName, c.cards...), pod)
			rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
			s := partitionServer(rec, echoActuatorResponder{rec: rec})

			allocate := func() error {
				_, err := s.Allocate(context.Background(), &AllocateRequest{
					ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}},
				})
				return err
			}
			require.NoError(t, allocate())
			first, ok := reservedWorkload(rec, "pod-retry")
			require.True(t, ok)
			require.Len(t, first.Groups[0].Accelerators, 1)
			firstCard := first.Groups[0].Accelerators[0].ID

			if !c.keepAlive {
				rec.releaseReservation("pod-retry", workloadContainer)
			}

			require.NoError(t, allocate(), "a retry must not report the node exhausted")
			second, ok := reservedWorkload(rec, "pod-retry")
			require.True(t, ok)
			require.Len(t, second.Groups, 1, "the retry must not add a second card")
			require.Len(t, second.Groups[0].Accelerators, 1)
			assert.Equal(t, firstCard, second.Groups[0].Accelerators[0].ID,
				"the retry must land back on the card the first allocation used")
		})
	}
}

// TestResourceServer_Allocate_PartitionedRejectsWhenTheNodeIsFull pins that a rejection means
// what it says: the node, not the card, has no room. The message names the profile so an
// operator does not have to infer it.
func TestResourceServer_Allocate_PartitionedRejectsWhenTheNodeIsFull(t *testing.T) {
	const nodeName = "node-partition-full"
	const profile = "7g.80gb"

	devs := partitionedDevices(nodeName,
		partitionedCard("dev-0", 0, "1g.10gb", workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 2}))
	pod := partitionPod(nodeName, "p", "pod-full", profile, 0)
	cli := nodeFixture(devs, pod)

	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	s := partitionServer(rec, physicalActuatorResponder{})

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), profile)
	_, reserved := reservedWorkload(rec, "pod-full")
	assert.False(t, reserved, "a rejected allocation holds no reservation")
}

// TestResourceServer_Allocate_PartitionedRollbackOnPatchFailure verifies a failed annotation
// patch tears down the materialized partition and releases the reservation, so no half-owned
// instance or stranded card persists.
func TestResourceServer_Allocate_PartitionedRollbackOnPatchFailure(t *testing.T) {
	const nodeName = "node-partition-rb"
	const profile = "1g.10gb"

	devs := partitionedDevices(nodeName,
		partitionedCard("dev-0", 0, profile, workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 2}))
	pod := partitionPod(nodeName, "p", "pod-partition-rb", profile, 0)
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(devs, pod).
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
	rolledBack := false
	s := partitionServer(rec, physicalActuatorResponder{
		placements: map[Resource][]workercore.AcceleratorPhysicalPlacement{
			{Group: "grp-0", Device: "dev-0"}: {{Start: 0, Length: 2}},
		},
		rolledBack: &rolledBack,
	})

	_, err := s.Allocate(context.Background(), &AllocateRequest{
		ContainerRequests: []*ContainerAllocateRequest{{DevicesIds: []string{"grp-0:dev-0:0000"}}},
	})
	require.Error(t, err)
	assert.True(t, rolledBack, "a failed patch must roll back the materialized partition")
	_, reserved := reservedWorkload(rec, "pod-partition-rb")
	assert.False(t, reserved, "a failed patch must release the reservation")
}

// partitionLedgerCard builds a partitioned accelerator whose Status-side ledger reports the
// instances it carries and how many more of each profile it can still host — the placement-aware
// view the partition pool's health is computed from.
func partitionLedgerCard(
	id string, index uint32, ceiling int32,
	allocated, remaining map[string]int32,
) (workercore.Accelerator, workercore.AcceleratorAllocation) {
	spec := workercore.Accelerator{
		ID: id, Index: index,
		Status: workercore.AcceleratorStatus{
			PhysicalSliced: workercore.AcceleratorPhysicalSliced{
				Count:    ceiling,
				Profiles: []workercore.AcceleratorPhysicalSlicedProfile{{Name: "1g.10gb", Count: ceiling}},
			},
		},
	}
	status := workercore.AcceleratorAllocation{
		ID: id, Index: index,
		AllocatedProfiles: device.ProfileCountSlice(allocated),
		RemainingProfiles: device.ProfileCountSlice(remaining),
	}
	return spec, status
}

// partitionLedgerDevices assembles one node's Devices from paired Spec/Status card views.
func partitionLedgerDevices(
	nodeName string, spec []workercore.Accelerator, status []workercore.AcceleratorAllocation,
) *workercore.Devices {
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA, Accelerators: spec}},
		},
		Status: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA, Accelerators: status}},
		},
	}
}

// healthyIDs returns the device IDs a ListAndWatch response reports Healthy, in response order.
func healthyIDs(resp *ListAndWatchResponse) []string {
	out := make([]string, 0, len(resp.Devices))
	for i := range resp.Devices {
		if resp.Devices[i].Health == kubeletdeviceplugin.Healthy {
			out = append(out, resp.Devices[i].ID)
		}
	}
	return out
}

// TestResourceServer_GetListAndWatch_PartitionHealthIsANodeCount pins the partition pool's
// health rule: every card advertises its full ceiling of IDs and never fewer, while the healthy
// count is allocated + remaining summed over the node's partitioned cards. The allocated term is
// what keeps the scheduler's free view — allocatable minus the requests of the Pods already on
// the node — equal to the room that is actually left.
func TestResourceServer_GetListAndWatch_PartitionHealthIsANodeCount(t *testing.T) {
	const nodeName = "node-partition-health"

	cases := []struct {
		name        string
		allocated   map[string]int32
		remaining   map[string]int32
		unhealthy   bool
		wantHealthy int
	}{
		{
			// An empty card can host its whole ceiling.
			name:        "an empty card advertises its full ceiling healthy",
			remaining:   map[string]int32{"1g.10gb": 7},
			wantHealthy: 7,
		},
		{
			// One instance carved: 1 + 4 healthy, so the scheduler's free view is 4 once it
			// subtracts the one Pod already on the node.
			name:        "a carved card advertises allocated plus remaining",
			allocated:   map[string]int32{"3g.40gb": 1},
			remaining:   map[string]int32{"1g.10gb": 4},
			wantHealthy: 5,
		},
		{
			// No room for anything: the healthy count is exactly the live instance count, which
			// the scheduler reduces to a free view of zero.
			name:        "a saturated card advertises exactly its live instances",
			allocated:   map[string]int32{"7g.80gb": 1},
			remaining:   nil,
			wantHealthy: 1,
		},
		{
			// A ledger the device manager has not published yet must not read as "no room".
			name:        "a card with no ledger yet falls back to its ceiling",
			wantHealthy: 7,
		},
		{
			// A broken card keeps its IDs but offers no room.
			name:        "an unhealthy card offers no room",
			remaining:   map[string]int32{"1g.10gb": 7},
			unhealthy:   true,
			wantHealthy: 0,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			spec, status := partitionLedgerCard("dev-0", 0, 7, c.allocated, c.remaining)
			spec.Status.Unhealthy = c.unhealthy
			devs := partitionLedgerDevices(nodeName, []workercore.Accelerator{spec},
				[]workercore.AcceleratorAllocation{status})
			cli := nodeFixture(devs)
			s := partitionServer(&DevicesReconciler{NodeName: nodeName, Client: cli}, stubResponder{})

			resp, err := s.getListAndWatchResponse(context.Background())
			require.NoError(t, err)
			assert.Len(t, resp.Devices, 7, "a card's IDs are never removed, whatever the ledger says")
			assert.Len(t, healthyIDs(resp), c.wantHealthy)
		})
	}
}

// TestResourceServer_GetListAndWatch_PartitionHealthySetIsStable pins the half a count cannot
// express. The kubelet checkpoints the exact IDs it offered a container and refuses any later
// allocation for it unless every one is still healthy, so an ID a live allocation holds must
// stay Healthy even as the count falls — and the free room must be granted as a stable prefix,
// so an unchanged ledger publishes an unchanged set.
func TestResourceServer_GetListAndWatch_PartitionHealthySetIsStable(t *testing.T) {
	const nodeName = "node-partition-set"

	// A saturated card: one live instance, no room for another. Its holder's ID is the LAST of
	// the card's seven, so a naive prefix-only grant would drop exactly it.
	heldID := "grp-0:dev-0:0006"
	spec, status := partitionLedgerCard("dev-0", 0, 7,
		map[string]int32{"7g.80gb": 1}, nil)
	devs := partitionLedgerDevices(nodeName, []workercore.Accelerator{spec},
		[]workercore.AcceleratorAllocation{status})

	pod := partitionPod(nodeName, "holder", "pod-holder", "7g.80gb", 0)
	cli := nodeFixture(devs, pod)
	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
	s := partitionServer(rec, stubResponder{})

	// The allocation is recorded in-process only, as it is between Allocate and the reconcile.
	rec.reserveDevices(pod.UID, workloadContainer, workercore.DevicesStatus{
		Groups: []workercore.DevicesAllocationGroup{{
			ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA,
			Accelerators: []workercore.AcceleratorAllocation{{ID: "dev-0", Mode: workercore.DeviceAllocationModePartitioned}},
		}},
	}, []string{heldID})

	first, err := s.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{heldID}, healthyIDs(first),
		"a saturated card reports exactly the ID its live allocation holds")

	// A second cycle over the same ledger must publish the identical set, byte for byte.
	second, err := s.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	assert.Equal(t, healthyIDs(first), healthyIDs(second),
		"two cycles over an unchanged ledger publish the identical healthy set")

	// Freeing the instance raises the count with no restart: the same server, over a ledger
	// that reports the card empty again, advertises the whole ceiling healthy.
	rec.releaseReservation(pod.UID, workloadContainer)
	freedSpec, freedStatus := partitionLedgerCard("dev-0", 0, 7, nil, map[string]int32{"1g.10gb": 7})
	freed := partitionLedgerDevices(nodeName, []workercore.Accelerator{freedSpec},
		[]workercore.AcceleratorAllocation{freedStatus})
	rec.Client = nodeFixture(freed)
	after, err := s.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	assert.Len(t, healthyIDs(after), 7, "freeing an instance raises the count")
}

// TestResourceServer_GetListAndWatch_PartitionHealthySetSurvivesARestart pins that the durable
// annotation alone keeps a checkpointed ID healthy. A device-manager restart clears the
// in-process reservations, but the kubelet's checkpoint survives it — and a container that was
// stopped at kubelet restart takes the checked path.
func TestResourceServer_GetListAndWatch_PartitionHealthySetSurvivesARestart(t *testing.T) {
	const nodeName = "node-partition-restart"
	heldID := "grp-0:dev-0:0006"

	spec, status := partitionLedgerCard("dev-0", 0, 7, map[string]int32{"7g.80gb": 1}, nil)
	devs := partitionLedgerDevices(nodeName, []workercore.Accelerator{spec},
		[]workercore.AcceleratorAllocation{status})

	pod := partitionPod(nodeName, "holder", "pod-holder", "7g.80gb", 0)
	allocations := PodAllocations{workloadContainer: ContainerAllocation{
		Devices: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID: "grp-0", Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.AcceleratorAllocation{{ID: "dev-0", Mode: workercore.DeviceAllocationModePartitioned}},
			}},
		},
		DeviceIDs: []string{heldID},
	}}
	raw, err := json.Marshal(allocations)
	require.NoError(t, err)
	pod.Annotations = map[string]string{AllocatedAcceleratorAnnoKey: string(raw)}

	cli := nodeFixture(devs, pod)
	// A fresh reconciler: no reservations, exactly as after a device-manager restart.
	s := partitionServer(&DevicesReconciler{NodeName: nodeName, Client: cli}, stubResponder{})

	resp, err := s.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{heldID}, healthyIDs(resp),
		"the annotation alone must keep a checkpointed ID healthy")
}

// TestResourceServer_GetListAndWatch_PartitionedHasNoTopology pins that a partition token
// carries no NUMA hint: Allocate chooses the card, so the token names none and a hint would tell
// the TopologyManager something the token cannot honor. The card-bound families keep theirs.
func TestResourceServer_GetListAndWatch_PartitionedHasNoTopology(t *testing.T) {
	const nodeName = "node-partition-numa"

	// A mixed node: one card in a partitioning mode, one logically sliceable. A card serves
	// exactly one family, so the two servers must be given two cards rather than one card
	// reporting both capabilities — a state no detector produces.
	partCard := partitionedCard("dev-0", 0, "1g.10gb",
		workercore.AcceleratorPhysicalPlacement{Start: 0, Length: 2})
	partCard.Topology.NumaAffinity = "0"
	logicalCard := workercore.Accelerator{
		ID: "dev-1", Index: 1,
		Status: workercore.AcceleratorStatus{
			LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 4},
		},
	}
	logicalCard.Topology.NumaAffinity = "0"
	devs := partitionedDevices(nodeName, partCard, logicalCard)
	cli := nodeFixture(devs)
	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}

	partitionResp, err := partitionServer(rec, stubResponder{}).getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, partitionResp.Devices)
	for i := range partitionResp.Devices {
		assert.Nil(t, partitionResp.Devices[i].Topology, "a partition token names no card, so it carries no NUMA hint")
	}

	sliced := &ResourceServer{
		Manufacturer:   nodefeature.ManufacturerNVIDIA,
		AllocationMode: workercore.DeviceAllocationModeSliced,
		Reconciler:     rec,
	}
	slicedResp, err := sliced.getListAndWatchResponse(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, slicedResp.Devices)
	assert.NotNil(t, slicedResp.Devices[0].Topology, "a card-bound token keeps its NUMA hint")
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
	cli := nodeFixture(devs)

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
			name:         "nvidia logical cards advertise cards x logical count",
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
			cli := nodeFixture(devs)
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
	reserveWorkload(rec, "pod-uid-v", visibilityReservation("dev-0"))

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
	reserveWorkload(rec, "pod-uid-v3", visibilityReservation("dev-9"))

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
	reserveWorkload(rec, "pod-uid-v4", visibilityReservation("dev-0")) // only 1 device reserved

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
	// The rejection reaches the user as a container-creation failure, so it must be diagnosable
	// on its own: which pod, which container holds the accelerator, and which cards it holds.
	assert.Contains(t, err.Error(), "default/p")
	assert.Contains(t, err.Error(), workloadContainer)
	assert.Contains(t, err.Error(), "grp-0:dev-0")
}
