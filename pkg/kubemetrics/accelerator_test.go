package kubemetrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const (
	// grantTestPodUID is the Pod UID the producer's records are keyed by — the Pod's own, never the
	// Instance's.
	grantTestPodUID  = "pod-uid-1"
	grantTestCardMiB = uint64(81920)
	grantTestDevice  = "gpu-uuid-1"
	grantTestVendor  = "nvidia"
)

// grantTestPod returns a Pod whose one container holds a compute cap where the allocator enforced
// it: the container's own request. A cap of zero requests none, which is the whole card.
func grantTestPod(coresPercent int64) *core.Pod {
	limits := core.ResourceList{}
	if coresPercent > 0 {
		limits[nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(grantTestVendor)] = *resource.NewQuantity(coresPercent, resource.DecimalSI)
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{Namespace: "tenant", Name: "inst", UID: grantTestPodUID},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name:      "main",
				Resources: core.ResourceRequirements{Limits: limits},
			}},
		},
	}
}

// grantTestAllocations returns what the device plugin recorded for one container.
func grantTestAllocations(
	container string, accelerators ...workercore.AcceleratorAllocation,
) deviceplugin.PodAllocations {
	return deviceplugin.PodAllocations{
		container: {
			Devices: workercore.DevicesStatus{
				Groups: []workercore.DevicesAllocationGroup{
					{ID: "gpu-group", Manufacturer: grantTestVendor, Accelerators: accelerators},
				},
			},
		},
	}
}

// grantTestAllocation is one accelerator held under the given mode, charged the given units.
func grantTestAllocation(
	mode workercore.DeviceAllocationMode, units int32,
) workercore.AcceleratorAllocation {
	return workercore.AcceleratorAllocation{ID: grantTestDevice, Mode: mode, Allocated: units}
}

// grantTestPartition is one hardware partition of the card, named as the allocation recorded it.
func grantTestPartition(id string) workercore.AcceleratorAllocation {
	return workercore.AcceleratorAllocation{
		ID:                       grantTestDevice,
		Mode:                     workercore.DeviceAllocationModePartitioned,
		AllocatedPhysicalProfile: "1g.10gb",
		AllocatedPhysicalID:      id,
	}
}

// grantTestCard is what the manufacturer's monitor read off the whole card: a busy card, most of it
// somebody else's.
func grantTestCard() *device.AcceleratorMetrics {
	return &device.AcceleratorMetrics{
		ID:                grantTestDevice,
		Memory:            grantTestCardMiB,
		MemoryUsage:       61440,
		MemoryUtilization: 75,
		CoresUtilization:  80,
		Temperature:       67,
		PowerUsage:        310,
	}
}

// grantTestSection returns a section that covered the card, carrying the given per-process records.
func grantTestSection(usages ...detector.SliceUsage) *detector.MonitorSliceSection {
	return &detector.MonitorSliceSection{
		SchemaVersion: detector.MonitorSliceSchemaVersion,
		Usages:        usages,
		Devices: []detector.SliceDeviceDiagnostics{
			{Manufacturer: grantTestVendor, DeviceID: grantTestDevice},
		},
	}
}

// grantTestUsage is what the per-process pass measured for the fixture's container on the card.
func grantTestUsage(memoryMiB *uint64, coresPercent *uint32) detector.SliceUsage {
	return detector.SliceUsage{
		Manufacturer: grantTestVendor, PodUID: grantTestPodUID,
		Container: "main", DeviceID: grantTestDevice,
		MemoryUsedMiB:           memoryMiB,
		CoresUtilizationPercent: coresPercent,
	}
}

// TestAcceleratorGrantsResolve is the expression's core: one accelerator entry, stated in the
// Instance's own terms whatever the allocation did to the card.
func TestAcceleratorGrantsResolve(t *testing.T) {
	cases := []struct {
		name        string
		pod         *core.Pod
		allocations deviceplugin.PodAllocations
		section     *detector.MonitorSliceSection
		want        worker.InstanceAcceleratorMetrics
	}{
		{
			// The device IS the grant, so the card's own figures are already this Instance's alone.
			name:        "a whole device reports the device",
			pod:         grantTestPod(0),
			allocations: grantTestAllocations("main", grantTestAllocation(workercore.DeviceAllocationModeExclusive, 0)),
			section:     nil,
			want: worker.InstanceAcceleratorMetrics{
				ID:                       grantTestDevice,
				Mode:                     workercore.DeviceAllocationModeExclusive.String(),
				MemoryTotalMiB:           ptr.To(grantTestCardMiB),
				MemoryUsedMiB:            ptr.To[uint64](61440),
				MemoryUtilizationPercent: ptr.To[uint32](75),
				CoresUtilizationPercent:  ptr.To[uint32](80),
				TemperatureCelsius:       ptr.To[uint32](67),
				PowerUsageWatts:          ptr.To[uint32](310),
				Unhealthy:                ptr.To(false),
			},
		},
		{
			// Shared grants the whole device to several holders with no quota between them, so the
			// device's figures are the only ones its holder has.
			name:        "a shared device reports the device",
			pod:         grantTestPod(0),
			allocations: grantTestAllocations("main", grantTestAllocation(workercore.DeviceAllocationModeShared, 0)),
			section:     nil,
			want: worker.InstanceAcceleratorMetrics{
				ID:                       grantTestDevice,
				Mode:                     workercore.DeviceAllocationModeShared.String(),
				MemoryTotalMiB:           ptr.To(grantTestCardMiB),
				MemoryUsedMiB:            ptr.To[uint64](61440),
				MemoryUtilizationPercent: ptr.To[uint32](75),
				CoresUtilizationPercent:  ptr.To[uint32](80),
				TemperatureCelsius:       ptr.To[uint32](67),
				PowerUsageWatts:          ptr.To[uint32](310),
				Unhealthy:                ptr.To(false),
			},
		},
		{
			// A quarter of an 80 GiB card capped at a fifth of its compute, measured holding 8 GiB
			// and using a tenth of the card. The totals are the grant, and the compute is restated
			// against the grant: 10 of the card's 100 is half of this slice's 20.
			name:        "a logical slice reports its quota and its own usage",
			pod:         grantTestPod(20),
			allocations: grantTestAllocations("main", grantTestAllocation(workercore.DeviceAllocationModeSliced, nodefeature.ResourceMaxUnits/4)),
			section:     grantTestSection(grantTestUsage(ptr.To[uint64](8192), ptr.To[uint32](10))),
			want: worker.InstanceAcceleratorMetrics{
				ID:                       grantTestDevice,
				Mode:                     workercore.DeviceAllocationModeSliced.String(),
				MemoryTotalMiB:           ptr.To(grantTestCardMiB / 4),
				MemoryUsedMiB:            ptr.To[uint64](8192),
				MemoryUtilizationPercent: ptr.To[uint32](40),
				CoresUtilizationPercent:  ptr.To[uint32](50),
				TemperatureCelsius:       ptr.To[uint32](67),
				PowerUsageWatts:          ptr.To[uint32](310),
				Unhealthy:                ptr.To(false),
			},
		},
		{
			// A slice requesting no compute budget is capped at the whole card, so the measured
			// share passes through untouched — the restatement is an identity, not a special case.
			name:        "a slice with no compute budget reports the measured share",
			pod:         grantTestPod(0),
			allocations: grantTestAllocations("main", grantTestAllocation(workercore.DeviceAllocationModeSliced, nodefeature.ResourceMaxUnits/4)),
			section:     grantTestSection(grantTestUsage(ptr.To[uint64](8192), ptr.To[uint32](10))),
			want: worker.InstanceAcceleratorMetrics{
				ID:                       grantTestDevice,
				Mode:                     workercore.DeviceAllocationModeSliced.String(),
				MemoryTotalMiB:           ptr.To(grantTestCardMiB / 4),
				MemoryUsedMiB:            ptr.To[uint64](8192),
				MemoryUtilizationPercent: ptr.To[uint32](40),
				CoresUtilizationPercent:  ptr.To[uint32](10),
				TemperatureCelsius:       ptr.To[uint32](67),
				PowerUsageWatts:          ptr.To[uint32](310),
				Unhealthy:                ptr.To(false),
			},
		},
		{
			// A slice measured busy while its cap is not being enforced. Published as it stands: a
			// figure held at 100 would present exactly this as a perfectly enforced cap.
			name:        "a slice over its compute cap is published, not clamped",
			pod:         grantTestPod(20),
			allocations: grantTestAllocations("main", grantTestAllocation(workercore.DeviceAllocationModeSliced, nodefeature.ResourceMaxUnits/4)),
			section:     grantTestSection(grantTestUsage(ptr.To[uint64](8192), ptr.To[uint32](50))),
			want: worker.InstanceAcceleratorMetrics{
				ID:                       grantTestDevice,
				Mode:                     workercore.DeviceAllocationModeSliced.String(),
				MemoryTotalMiB:           ptr.To(grantTestCardMiB / 4),
				MemoryUsedMiB:            ptr.To[uint64](8192),
				MemoryUtilizationPercent: ptr.To[uint32](40),
				CoresUtilizationPercent:  ptr.To[uint32](250),
				TemperatureCelsius:       ptr.To[uint32](67),
				PowerUsageWatts:          ptr.To[uint32](310),
				Unhealthy:                ptr.To(false),
			},
		},
		{
			// THE SUBSTITUTION THIS WHOLE FEATURE REFUSES. Nothing could measure the slice, so it
			// reports no usage — the card's 61440 MiB is every tenant on it, and publishing that
			// would charge them all to this one.
			name:        "a slice nothing could measure reports no usage, never the card's",
			pod:         grantTestPod(20),
			allocations: grantTestAllocations("main", grantTestAllocation(workercore.DeviceAllocationModeSliced, nodefeature.ResourceMaxUnits/4)),
			section:     nil,
			want: worker.InstanceAcceleratorMetrics{
				ID:                 grantTestDevice,
				Mode:               workercore.DeviceAllocationModeSliced.String(),
				MemoryTotalMiB:     ptr.To(grantTestCardMiB / 4),
				TemperatureCelsius: ptr.To[uint32](67),
				PowerUsageWatts:    ptr.To[uint32](310),
				Unhealthy:          ptr.To(false),
			},
		},
		{
			// Measured and idle. Zero is a figure, and the pair beside it says so.
			name:        "an idle slice reports zero",
			pod:         grantTestPod(20),
			allocations: grantTestAllocations("main", grantTestAllocation(workercore.DeviceAllocationModeSliced, nodefeature.ResourceMaxUnits/4)),
			section:     grantTestSection(),
			want: worker.InstanceAcceleratorMetrics{
				ID:                       grantTestDevice,
				Mode:                     workercore.DeviceAllocationModeSliced.String(),
				MemoryTotalMiB:           ptr.To(grantTestCardMiB / 4),
				MemoryUsedMiB:            ptr.To[uint64](0),
				MemoryUtilizationPercent: ptr.To[uint32](0),
				CoresUtilizationPercent:  ptr.To[uint32](0),
				TemperatureCelsius:       ptr.To[uint32](67),
				PowerUsageWatts:          ptr.To[uint32](310),
				Unhealthy:                ptr.To(false),
			},
		},
		{
			// The partition names and sizes ITSELF, off its own handle: the entry is reported under
			// the MIG UUID and against the partition's own 9856 MiB, not the card's 81920.
			name: "a hardware partition reports its own identity and capacity",
			pod:  grantTestPod(0),
			allocations: grantTestAllocations("main",
				grantTestAllocation(workercore.DeviceAllocationModePartitioned, nodefeature.ResourceMaxUnits/8)),
			section: &detector.MonitorSliceSection{
				SchemaVersion: detector.MonitorSliceSchemaVersion,
				Partitions: []detector.SlicePartition{{
					Manufacturer: grantTestVendor, PodUID: grantTestPodUID,
					Container: "main", DeviceID: grantTestDevice,
					ID:             "MIG-aaa",
					MemoryTotalMiB: ptr.To[uint64](9856),
					MemoryUsedMiB:  ptr.To[uint64](2464),
					CoresReason:    device.AcceleratorProcessReasonUnsupported,
				}},
			},
			want: worker.InstanceAcceleratorMetrics{
				ID:                       "MIG-aaa",
				Mode:                     workercore.DeviceAllocationModePartitioned.String(),
				MemoryTotalMiB:           ptr.To[uint64](9856),
				MemoryUsedMiB:            ptr.To[uint64](2464),
				MemoryUtilizationPercent: ptr.To[uint32](25),
				TemperatureCelsius:       ptr.To[uint32](67),
				PowerUsageWatts:          ptr.To[uint32](310),
				Unhealthy:                ptr.To(false),
			},
		},
		{
			// A partition the driver would not name is not reported under the card's identifier: the
			// card is not what the Instance holds, and its figures are not the partition's.
			name: "a partition nothing could read keeps the device identifier and no figures",
			pod:  grantTestPod(0),
			allocations: grantTestAllocations("main",
				grantTestAllocation(workercore.DeviceAllocationModePartitioned, nodefeature.ResourceMaxUnits/8)),
			section: &detector.MonitorSliceSection{
				SchemaVersion: detector.MonitorSliceSchemaVersion,
				Partitions: []detector.SlicePartition{{
					Manufacturer: grantTestVendor, PodUID: grantTestPodUID,
					Container: "main", DeviceID: grantTestDevice,
					MemoryReason: device.AcceleratorProcessReasonDriverError,
					CoresReason:  device.AcceleratorProcessReasonUnsupported,
				}},
			},
			want: worker.InstanceAcceleratorMetrics{
				ID:                 grantTestDevice,
				Mode:               workercore.DeviceAllocationModePartitioned.String(),
				TemperatureCelsius: ptr.To[uint32](67),
				PowerUsageWatts:    ptr.To[uint32](310),
				Unhealthy:          ptr.To(false),
			},
		},
		{
			// One entry cannot carry two grants, and picking or summing them would state a quota
			// nobody was granted. The card's own readings survive, because they are the card's.
			name: "an accelerator two containers were granted reports no figures of its own",
			pod:  grantTestPod(20),
			allocations: deviceplugin.PodAllocations{
				"main": {Devices: workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{
					{ID: "gpu-group", Manufacturer: grantTestVendor, Accelerators: []workercore.AcceleratorAllocation{
						grantTestAllocation(workercore.DeviceAllocationModeSliced, nodefeature.ResourceMaxUnits/4),
					}},
				}}},
				"sidecar": {Devices: workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{
					{ID: "gpu-group", Manufacturer: grantTestVendor, Accelerators: []workercore.AcceleratorAllocation{
						grantTestAllocation(workercore.DeviceAllocationModeSliced, nodefeature.ResourceMaxUnits/8),
					}},
				}}},
			},
			section: grantTestSection(grantTestUsage(ptr.To[uint64](8192), ptr.To[uint32](10))),
			want: worker.InstanceAcceleratorMetrics{
				ID:                 grantTestDevice,
				Mode:               workercore.DeviceAllocationModeSliced.String(),
				TemperatureCelsius: ptr.To[uint32](67),
				PowerUsageWatts:    ptr.To[uint32](310),
				Unhealthy:          ptr.To(false),
			},
		},
		{
			// Visibility grants no resources: it lets a sidecar SEE what the main container holds.
			// Counting it as a second claimant would leave every such Pod reporting nothing.
			name: "a visibility sidecar is not a second claimant",
			pod:  grantTestPod(0),
			allocations: deviceplugin.PodAllocations{
				"main": {Devices: workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{
					{ID: "gpu-group", Manufacturer: grantTestVendor, Accelerators: []workercore.AcceleratorAllocation{
						grantTestAllocation(workercore.DeviceAllocationModeExclusive, 0),
					}},
				}}},
				"sidecar": {Devices: workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{
					{ID: "gpu-group", Manufacturer: grantTestVendor, Accelerators: []workercore.AcceleratorAllocation{
						grantTestAllocation(workercore.DeviceAllocationModeVisibility, 0),
					}},
				}}},
			},
			section: nil,
			want: worker.InstanceAcceleratorMetrics{
				ID:                       grantTestDevice,
				Mode:                     workercore.DeviceAllocationModeExclusive.String(),
				MemoryTotalMiB:           ptr.To(grantTestCardMiB),
				MemoryUsedMiB:            ptr.To[uint64](61440),
				MemoryUtilizationPercent: ptr.To[uint32](75),
				CoresUtilizationPercent:  ptr.To[uint32](80),
				TemperatureCelsius:       ptr.To[uint32](67),
				PowerUsageWatts:          ptr.To[uint32](310),
				Unhealthy:                ptr.To(false),
			},
		},
		{
			// An accelerator this Pod's allocation does not name. Nothing here can say whose the
			// card's figures are, so it says nothing about them.
			name:        "an accelerator no grant names reports no figures",
			pod:         grantTestPod(0),
			allocations: deviceplugin.PodAllocations{},
			section:     nil,
			want: worker.InstanceAcceleratorMetrics{
				ID:                 grantTestDevice,
				TemperatureCelsius: ptr.To[uint32](67),
				PowerUsageWatts:    ptr.To[uint32](310),
				Unhealthy:          ptr.To(false),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			grants := NewAcceleratorGrants(c.pod, c.allocations)
			got := grants.Resolve(c.section, grantTestVendor, grantTestCard())
			require.Len(t, got, 1, "each case describes one grant on the card")
			assert.Equal(t, c.want, got[0])
		})
	}
}

// TestSlicedMemoryTotalMiB pins the quota's arithmetic, which inverts the fold that produced the
// units so the reported total cannot disagree with what was granted.
func TestSlicedMemoryTotalMiB(t *testing.T) {
	cases := []struct {
		name  string
		units int32
		card  uint64
		want  *uint64
	}{
		{name: "a quarter of a card", units: nodefeature.ResourceMaxUnits / 4, card: 81920, want: ptr.To[uint64](20480)},
		{name: "the whole card", units: nodefeature.ResourceMaxUnits, card: 81920, want: ptr.To[uint64](81920)},
		{
			name:  "a record above the whole card is held at the card",
			units: nodefeature.ResourceMaxUnits * 2, card: 81920, want: ptr.To[uint64](81920),
		},
		{
			// A granted quota never folds back to nothing: zero is not a small quota but the absence
			// of one, and it would take every figure beside it with it.
			name:  "a slice too small to fold reports one MiB",
			units: 19, card: 81920, want: ptr.To[uint64](1),
		},
		{name: "an allocation that charged nothing states no quota", units: 0, card: 81920, want: nil},
		{name: "a card of unknown capacity states no quota", units: nodefeature.ResourceMaxUnits / 4, card: 0, want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, slicedMemoryTotalMiB(c.units, c.card))
		})
	}
}

// TestOwnCoresUtilizationPercent pins the restatement that makes one field mean one thing in every
// mode: a measured share of the whole card, over the holder's own allowance of it.
func TestOwnCoresUtilizationPercent(t *testing.T) {
	cases := []struct {
		name     string
		measured *uint32
		cap      uint32
		want     *uint32
	}{
		{name: "a whole card passes through", measured: ptr.To[uint32](34), cap: 100, want: ptr.To[uint32](34)},
		{name: "no cap at all passes through", measured: ptr.To[uint32](34), cap: 0, want: ptr.To[uint32](34)},
		{name: "a fifth of a card, saturated", measured: ptr.To[uint32](20), cap: 20, want: ptr.To[uint32](100)},
		{name: "a fifth of a card, half used", measured: ptr.To[uint32](10), cap: 20, want: ptr.To[uint32](50)},
		{name: "an idle slice stays zero", measured: ptr.To[uint32](0), cap: 20, want: ptr.To[uint32](0)},
		{
			// Rounded up, because the card is measured in whole percent: flooring 1 of a 3% cap to
			// 33 is fine, but a measurably busy slice must never read as doing nothing.
			name: "a busy slice under a small cap rounds up", measured: ptr.To[uint32](1), cap: 3,
			want: ptr.To[uint32](34),
		},
		{
			name: "a cap that is not being enforced exceeds a hundred", measured: ptr.To[uint32](50),
			cap: 20, want: ptr.To[uint32](250),
		},
		{name: "nothing measured states nothing", measured: nil, cap: 20, want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, ownCoresUtilizationPercent(c.measured, c.cap))
		})
	}
}

// TestAcceleratorGrantsResolveUsesEnforcedCap pins where the compute cap comes from: the container
// the allocation was enforced on, and not the Pod. A search that missed the container would default
// to the whole card and quietly understate every slice's own utilization.
func TestAcceleratorGrantsResolveUsesEnforcedCap(t *testing.T) {
	pod := grantTestPod(25)
	pod.Spec.Containers[0].Name = "worker"
	allocations := grantTestAllocations("worker",
		grantTestAllocation(workercore.DeviceAllocationModeSliced, nodefeature.ResourceMaxUnits/4))

	section := &detector.MonitorSliceSection{
		SchemaVersion: detector.MonitorSliceSchemaVersion,
		Usages: []detector.SliceUsage{{
			Manufacturer: grantTestVendor, PodUID: grantTestPodUID,
			Container: "worker", DeviceID: grantTestDevice,
			CoresUtilizationPercent: ptr.To[uint32](5),
		}},
		Devices: []detector.SliceDeviceDiagnostics{
			{Manufacturer: grantTestVendor, DeviceID: grantTestDevice},
		},
	}

	got := NewAcceleratorGrants(pod, allocations).Resolve(section, grantTestVendor, grantTestCard())
	require.Len(t, got, 1)
	require.NotNil(t, got[0].CoresUtilizationPercent)
	assert.Equal(t, uint32(20), *got[0].CoresUtilizationPercent, "5 of the card is a fifth of a 25% cap")
}

// TestAcceleratorGrantsResolveSplitsPartitionsOfOneCard is the shape a card cannot flatten: an
// Instance whose two containers each hold a partition of ONE card holds two grants, and each is its
// own entry with its own identity, capacity and usage. Reporting them as one would publish one
// tenant of a card as the card, and the entry list — keyed by id — could not carry them anyway.
func TestAcceleratorGrantsResolveSplitsPartitionsOfOneCard(t *testing.T) {
	pod := grantTestPod(0)
	pod.Spec.Containers = []core.Container{{Name: "main"}, {Name: "sidecar"}}
	allocations := deviceplugin.PodAllocations{
		"main":    grantTestAllocations("main", grantTestPartition("MIG-aaa"))["main"],
		"sidecar": grantTestAllocations("sidecar", grantTestPartition("MIG-bbb"))["sidecar"],
	}

	section := &detector.MonitorSliceSection{
		SchemaVersion: detector.MonitorSliceSchemaVersion,
		Partitions: []detector.SlicePartition{
			{
				Manufacturer: grantTestVendor, PodUID: grantTestPodUID, Container: "main",
				DeviceID: grantTestDevice, ID: "MIG-aaa",
				MemoryTotalMiB: ptr.To[uint64](9984), MemoryUsedMiB: ptr.To[uint64](4222),
			},
			{
				Manufacturer: grantTestVendor, PodUID: grantTestPodUID, Container: "sidecar",
				DeviceID: grantTestDevice, ID: "MIG-bbb",
				MemoryTotalMiB: ptr.To[uint64](9984), MemoryUsedMiB: ptr.To[uint64](0),
			},
		},
		Devices: []detector.SliceDeviceDiagnostics{
			{Manufacturer: grantTestVendor, DeviceID: grantTestDevice},
		},
	}

	got := NewAcceleratorGrants(pod, allocations).Resolve(section, grantTestVendor, grantTestCard())
	require.Len(t, got, 2, "two partitions of one card are two grants")

	// Ordered by the partition each holds, so a scrape and a subresource read agree twice running.
	assert.Equal(t, []string{"MIG-aaa", "MIG-bbb"}, []string{got[0].ID, got[1].ID},
		"each entry names the partition, never the parent card")
	for i := range got {
		assert.Equal(t, workercore.DeviceAllocationModePartitioned.String(), got[i].Mode)
		assert.Equal(t, ptr.To[uint64](9984), got[i].MemoryTotalMiB,
			"the partition's own capacity, not the card's")
	}
	// The loaded partition and the idle one read differently, which is the whole point of splitting
	// them: one entry would have had to pick or sum.
	assert.Equal(t, ptr.To[uint64](4222), got[0].MemoryUsedMiB)
	assert.Equal(t, ptr.To[uint64](0), got[1].MemoryUsedMiB, "measured and idle, not absent")
}

// TestAcceleratorGrantsResolveNamesAnUnmeasurablePartition keeps two partitions distinguishable even
// when nothing could read them: the allocation recorded which one each grant holds, so the entries
// carry those identifiers rather than collapsing onto the parent card's.
func TestAcceleratorGrantsResolveNamesAnUnmeasurablePartition(t *testing.T) {
	pod := grantTestPod(0)
	pod.Spec.Containers = []core.Container{{Name: "main"}, {Name: "sidecar"}}
	allocations := deviceplugin.PodAllocations{
		"main":    grantTestAllocations("main", grantTestPartition("MIG-aaa"))["main"],
		"sidecar": grantTestAllocations("sidecar", grantTestPartition("MIG-bbb"))["sidecar"],
	}

	// A section that answers for nothing at all — a driver that refused every partition read.
	got := NewAcceleratorGrants(pod, allocations).Resolve(nil, grantTestVendor, grantTestCard())

	require.Len(t, got, 2)
	assert.Equal(t, []string{"MIG-aaa", "MIG-bbb"}, []string{got[0].ID, got[1].ID})
	for i := range got {
		assert.Nil(t, got[i].MemoryTotalMiB, "nothing could size it")
		assert.Nil(t, got[i].MemoryUsedMiB, "nothing measured it, which is not zero")
		assert.Nil(t, got[i].CoresUtilizationPercent)
		// The card's own readings still belong to every tenant of it.
		assert.NotNil(t, got[i].TemperatureCelsius)
	}
}
