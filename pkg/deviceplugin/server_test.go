package deviceplugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	context.Context, *core.Pod, *workercore.Devices, map[Resource]int32,
) (*ContainerAllocateResponse, error) {
	return &ContainerAllocateResponse{}, nil
}

// TestResourceServer_Allocate_Sliced verifies the sliced Allocate does placement
// bookkeeping only: it writes an AcceleratorAllocation whose Allocated is the plain
// injection-token count (one token per card), not a padded units value.
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
