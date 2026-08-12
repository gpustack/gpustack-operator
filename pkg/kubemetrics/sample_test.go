package kubemetrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeletstats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/utils/ptr"
)

// podWithLimits builds a pod whose containers carry the given limits, in the shape the
// Instance controller produces: a "main" container declaring the general resources, and any
// further container standing in for the sshd sidecar.
func podWithLimits(limits ...core.ResourceList) *core.Pod {
	pod := &core.Pod{}
	for i, l := range limits {
		name := "sidecar"
		if i == 0 {
			name = "main"
		}
		pod.Spec.Containers = append(pod.Spec.Containers, core.Container{
			Name:      name,
			Resources: core.ResourceRequirements{Limits: l},
		})
	}
	return pod
}

func TestNewSample(t *testing.T) {
	cases := []struct {
		name string

		pod *core.Pod

		wantCPUMilliCores uint64
		wantMemoryMiB     uint64
		wantStorageMiB    uint64
	}{
		{
			name: "the declared limits of a single container",
			pod: podWithLimits(core.ResourceList{
				core.ResourceCPU:              resource.MustParse("2"),
				core.ResourceMemory:           resource.MustParse("4Gi"),
				core.ResourceEphemeralStorage: resource.MustParse("10Gi"),
			}),
			wantCPUMilliCores: 2000,
			wantMemoryMiB:     4096,
			wantStorageMiB:    10240,
		},
		{
			name: "a sidecar declaring nothing leaves the totals alone",
			pod: podWithLimits(
				core.ResourceList{
					core.ResourceCPU:              resource.MustParse("2"),
					core.ResourceMemory:           resource.MustParse("4Gi"),
					core.ResourceEphemeralStorage: resource.MustParse("10Gi"),
				},
				core.ResourceList{},
			),
			wantCPUMilliCores: 2000,
			wantMemoryMiB:     4096,
			wantStorageMiB:    10240,
		},
		{
			name: "container limits are summed",
			pod: podWithLimits(
				core.ResourceList{core.ResourceCPU: resource.MustParse("1500m")},
				core.ResourceList{core.ResourceCPU: resource.MustParse("500m")},
			),
			wantCPUMilliCores: 2000,
		},
		{
			name: "a resource without a limit totals zero",
			pod:  podWithLimits(core.ResourceList{core.ResourceCPU: resource.MustParse("1")}),
			// No declared ceiling: the consumer has to guard the division either way.
			wantCPUMilliCores: 1000,
		},
		{
			name: "a sub-MiB limit rounds up",
			pod: podWithLimits(core.ResourceList{
				core.ResourceMemory:           resource.MustParse("512Ki"),
				core.ResourceEphemeralStorage: resource.MustParse("1500Ki"),
			}),
			wantMemoryMiB:  1,
			wantStorageMiB: 2,
		},
		{
			name: "a pod without containers totals zero",
			pod:  &core.Pod{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sample := NewSample(c.pod)

			assert.Equal(t, c.wantCPUMilliCores, sample.CPUTotalMilliCores)
			assert.Equal(t, c.wantMemoryMiB, sample.MemoryTotalMiB)
			assert.Equal(t, c.wantStorageMiB, sample.StorageTotalMiB)

			// A totals-only sample leaves every measurement absent for the caller to fill.
			assert.Nil(t, sample.CPUUsedMilliCores)
			assert.Nil(t, sample.MemoryUsedMiB)
			assert.Nil(t, sample.StorageUsedMiB)
			assert.False(t, sample.Timestamp.IsZero(), "a sample is always stamped")
		})
	}
}

func TestNewSampleFromPodStats(t *testing.T) {
	measuredAt := meta.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))

	fullPod := podWithLimits(core.ResourceList{
		core.ResourceCPU:              resource.MustParse("2"),
		core.ResourceMemory:           resource.MustParse("4Gi"),
		core.ResourceEphemeralStorage: resource.MustParse("10Gi"),
	})

	cases := []struct {
		name string

		pod   *core.Pod
		stats *kubeletstats.PodStats

		wantCPUUsed     *uint64
		wantMemoryUsed  *uint64
		wantStorageUsed *uint64
		wantTimestamp   *meta.Time
	}{
		{
			name: "the measured usage, in the totals' units",
			pod:  fullPod,
			stats: &kubeletstats.PodStats{
				CPU: &kubeletstats.CPUStats{
					Time:           measuredAt,
					UsageNanoCores: ptr.To[uint64](500_000_000),
				},
				Memory:           &kubeletstats.MemoryStats{WorkingSetBytes: ptr.To[uint64](1 << 30)},
				EphemeralStorage: &kubeletstats.FsStats{UsedBytes: ptr.To[uint64](3 << 30)},
			},
			wantCPUUsed:     ptr.To[uint64](500),
			wantMemoryUsed:  ptr.To[uint64](1024),
			wantStorageUsed: ptr.To[uint64](3072),
			wantTimestamp:   &measuredAt,
		},
		{
			name: "the storage numerator is the pod-level aggregate, not the writable layers",
			pod:  fullPod,
			stats: &kubeletstats.PodStats{
				// An Instance filling its workspace emptyDir grows the aggregate while its
				// containers' writable layers stay near zero: reading the layers would show a
				// pod about to be evicted as nearly empty.
				EphemeralStorage: &kubeletstats.FsStats{UsedBytes: ptr.To[uint64](9 << 30)},
				Containers: []kubeletstats.ContainerStats{
					{Name: "main", Rootfs: &kubeletstats.FsStats{UsedBytes: ptr.To[uint64](1 << 20)}},
				},
			},
			wantStorageUsed: ptr.To[uint64](9216),
		},
		{
			name: "a sub-unit measurement stays visible",
			pod:  fullPod,
			stats: &kubeletstats.PodStats{
				CPU:              &kubeletstats.CPUStats{UsageNanoCores: ptr.To[uint64](1)},
				Memory:           &kubeletstats.MemoryStats{WorkingSetBytes: ptr.To[uint64](585_728)},
				EphemeralStorage: &kubeletstats.FsStats{UsedBytes: ptr.To[uint64](20_480)},
			},
			wantCPUUsed:     ptr.To[uint64](1),
			wantMemoryUsed:  ptr.To[uint64](1),
			wantStorageUsed: ptr.To[uint64](1),
		},
		{
			name: "zero only when the source measured no usage",
			pod:  fullPod,
			stats: &kubeletstats.PodStats{
				Memory: &kubeletstats.MemoryStats{WorkingSetBytes: ptr.To[uint64](0)},
			},
			wantMemoryUsed: ptr.To[uint64](0),
		},
		{
			name:  "an empty stats entry leaves every measurement absent",
			pod:   fullPod,
			stats: &kubeletstats.PodStats{},
		},
		{
			name: "a stats section without its figure leaves that measurement absent",
			pod:  fullPod,
			stats: &kubeletstats.PodStats{
				CPU:              &kubeletstats.CPUStats{},
				Memory:           &kubeletstats.MemoryStats{},
				EphemeralStorage: &kubeletstats.FsStats{},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sample := newSampleFromPodStats(c.pod, c.stats)

			// The totals survive whatever the measurement carries.
			assert.Equal(t, uint64(2000), sample.CPUTotalMilliCores)
			assert.Equal(t, uint64(4096), sample.MemoryTotalMiB)
			assert.Equal(t, uint64(10240), sample.StorageTotalMiB)

			assert.Equal(t, c.wantCPUUsed, sample.CPUUsedMilliCores)
			assert.Equal(t, c.wantMemoryUsed, sample.MemoryUsedMiB)
			assert.Equal(t, c.wantStorageUsed, sample.StorageUsedMiB)

			if c.wantTimestamp != nil {
				assert.Equal(t, *c.wantTimestamp, sample.Timestamp,
					"the sample reports when the kubelet measured, not when it was read")
				return
			}
			assert.False(t, sample.Timestamp.IsZero(),
				"a stats entry without a measurement time is still stamped")
		})
	}
}

func TestBytesToMiB(t *testing.T) {
	cases := []struct {
		name string
		in   *uint64
		want *uint64
	}{
		{name: "absence stays absent"},
		{name: "zero stays zero", in: ptr.To[uint64](0), want: ptr.To[uint64](0)},
		{name: "a sub-MiB figure rounds up", in: ptr.To[uint64](1), want: ptr.To[uint64](1)},
		{name: "an exact MiB", in: ptr.To[uint64](2 << 20), want: ptr.To[uint64](2)},
		{name: "a remainder rounds up", in: ptr.To[uint64](2<<20 + 1), want: ptr.To[uint64](3)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, bytesToMiB(c.in))
		})
	}
}

func TestNanoCoresToMilliCores(t *testing.T) {
	cases := []struct {
		name string
		in   *uint64
		want *uint64
	}{
		{name: "absence stays absent"},
		{name: "zero stays zero", in: ptr.To[uint64](0), want: ptr.To[uint64](0)},
		{name: "a sub-milli-core figure rounds up", in: ptr.To[uint64](1), want: ptr.To[uint64](1)},
		{name: "half a core", in: ptr.To[uint64](500_000_000), want: ptr.To[uint64](500)},
		{name: "a remainder rounds up", in: ptr.To[uint64](1_000_001), want: ptr.To[uint64](2)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, nanoCoresToMilliCores(c.in))
		})
	}
}
