package kubemetrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestHasStartedContainer(t *testing.T) {
	statusPod := func(field func(*core.PodStatus, core.ContainerStatus), cs core.ContainerStatus) *core.Pod {
		pod := &core.Pod{}
		field(&pod.Status, cs)
		return pod
	}
	asRegular := func(s *core.PodStatus, cs core.ContainerStatus) {
		s.ContainerStatuses = append(s.ContainerStatuses, cs)
	}
	asInit := func(s *core.PodStatus, cs core.ContainerStatus) {
		s.InitContainerStatuses = append(s.InitContainerStatuses, cs)
	}
	asEphemeral := func(s *core.PodStatus, cs core.ContainerStatus) {
		s.EphemeralContainerStatuses = append(s.EphemeralContainerStatuses, cs)
	}

	cases := []struct {
		name string

		pod *core.Pod

		want bool
	}{
		{
			name: "a nil pod has started nothing",
			pod:  nil,
			want: false,
		},
		{
			name: "a pod with no container status at all",
			pod:  &core.Pod{},
			want: false,
		},
		{
			name: "a container still pulling its image",
			pod: statusPod(asRegular, core.ContainerStatus{
				Name:  "main",
				State: core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "PullingImage"}},
			}),
			want: false,
		},
		{
			name: "a running container",
			pod: statusPod(asRegular, core.ContainerStatus{
				Name:  "main",
				State: core.ContainerState{Running: &core.ContainerStateRunning{}},
			}),
			want: true,
		},
		{
			name: "a running container whose readiness probe fails",
			pod: statusPod(asRegular, core.ContainerStatus{
				Name:  "main",
				Ready: false,
				State: core.ContainerState{Running: &core.ContainerStateRunning{}},
			}),
			// The case the predicate exists for: this container can be holding accelerator
			// memory, so reporting zero for it would fabricate an idle measurement.
			want: true,
		},
		{
			name: "a container that has already exited",
			pod: statusPod(asRegular, core.ContainerStatus{
				Name:  "main",
				State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1}},
			}),
			want: true,
		},
		{
			name: "a container waiting to be restarted",
			pod: statusPod(asRegular, core.ContainerStatus{
				Name:         "main",
				State:        core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				RestartCount: 3,
			}),
			want: true,
		},
		{
			name: "a container waiting whose previous run terminated",
			pod: statusPod(asRegular, core.ContainerStatus{
				Name:  "main",
				State: core.ContainerState{Waiting: &core.ContainerStateWaiting{}},
				LastTerminationState: core.ContainerState{
					Terminated: &core.ContainerStateTerminated{ExitCode: 0},
				},
			}),
			want: true,
		},
		{
			name: "a container the kubelet only flags as started",
			pod: statusPod(asRegular, core.ContainerStatus{
				Name:    "main",
				Started: ptr.To(true),
			}),
			want: true,
		},
		{
			name: "an init container that is running",
			pod: statusPod(asInit, core.ContainerStatus{
				Name:  "init",
				State: core.ContainerState{Running: &core.ContainerStateRunning{}},
			}),
			want: true,
		},
		{
			name: "an ephemeral container that is running",
			pod: statusPod(asEphemeral, core.ContainerStatus{
				Name:  "debug",
				State: core.ContainerState{Running: &core.ContainerStateRunning{}},
			}),
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, HasStartedContainer(c.pod))
		})
	}
}

func TestNewUnstartedSample(t *testing.T) {
	resources := &workercore.InstanceResources{
		CPU:          resource.MustParse("2"),
		RAM:          resource.MustParse("4Gi"),
		LocalStorage: resource.MustParse("10Gi"),
	}

	cases := []struct {
		name string

		pod       *core.Pod
		resources *workercore.InstanceResources

		wantCPUMilliCores uint64
		wantMemoryMiB     uint64
		wantStorageMiB    uint64
	}{
		{
			name: "the totals come from the pod when there is one",
			pod: podWithLimits(core.ResourceList{
				core.ResourceCPU:              resource.MustParse("2"),
				core.ResourceMemory:           resource.MustParse("4Gi"),
				core.ResourceEphemeralStorage: resource.MustParse("10Gi"),
			}),
			// Deliberately different from the pod's: whichever wins is asserted, not guessed.
			resources:         &workercore.InstanceResources{CPU: resource.MustParse("64")},
			wantCPUMilliCores: 2000,
			wantMemoryMiB:     4096,
			wantStorageMiB:    10240,
		},
		{
			name:              "the totals come from spec.resources when there is no pod",
			pod:               nil,
			resources:         resources,
			wantCPUMilliCores: 2000,
			wantMemoryMiB:     4096,
			wantStorageMiB:    10240,
		},
		{
			name:              "an instance declaring no resources totals zero",
			pod:               nil,
			resources:         nil,
			wantCPUMilliCores: 0,
			wantMemoryMiB:     0,
			wantStorageMiB:    0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sample := NewUnstartedSample(c.pod, c.resources)

			assert.Equal(t, c.wantCPUMilliCores, sample.CPUTotalMilliCores)
			assert.Equal(t, c.wantMemoryMiB, sample.MemoryTotalMiB)
			assert.Equal(t, c.wantStorageMiB, sample.StorageTotalMiB)

			// Zero, and present: nothing has started, so nothing can have consumed anything —
			// which is a measurement rather than an absence.
			require.NotNil(t, sample.CPUUsedMilliCores)
			require.NotNil(t, sample.MemoryUsedMiB)
			require.NotNil(t, sample.StorageUsedMiB)
			assert.Zero(t, *sample.CPUUsedMilliCores)
			assert.Zero(t, *sample.MemoryUsedMiB)
			assert.Zero(t, *sample.StorageUsedMiB)

			assert.Empty(t, sample.Accelerators)
			assert.False(t, sample.Timestamp.IsZero(), "the sample is stamped at read time")
		})
	}

	t.Run("the totals match the pod-derived ones for the same declaration", func(t *testing.T) {
		fromPod := NewUnstartedSample(podWithLimits(core.ResourceList{
			core.ResourceCPU:              resource.MustParse("2"),
			core.ResourceMemory:           resource.MustParse("4Gi"),
			core.ResourceEphemeralStorage: resource.MustParse("10Gi"),
		}), nil)
		fromSpec := NewUnstartedSample(nil, resources)

		assert.Equal(t, fromPod.CPUTotalMilliCores, fromSpec.CPUTotalMilliCores)
		assert.Equal(t, fromPod.MemoryTotalMiB, fromSpec.MemoryTotalMiB)
		assert.Equal(t, fromPod.StorageTotalMiB, fromSpec.StorageTotalMiB)
	})
}
