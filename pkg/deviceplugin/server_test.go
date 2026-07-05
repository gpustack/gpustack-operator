package deviceplugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeletdeviceplugin "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

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
					ID:       "dev-0",
					Index:    0,
					Features: workercore.AcceleratorFeatures{MaxPartitions: 8},
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
					ID:       "dev-0",
					Index:    0,
					Features: workercore.AcceleratorFeatures{MaxPartitions: 8},
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
					ID:       "dev-0",
					Index:    0,
					Features: workercore.AcceleratorFeatures{MaxPartitions: 8},
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
