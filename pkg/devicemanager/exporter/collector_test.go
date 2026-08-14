package exporter

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	"gpustack.ai/gpustack/pkg/kubemetrics"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// _AcceleratorFamilies are the per-device families, named so a case can assert their absence.
var _AcceleratorFamilies = []string{
	"gpustack_instance_accelerator_memory_total_mib",
	"gpustack_instance_accelerator_memory_used_mib",
	"gpustack_instance_accelerator_memory_utilization_percent",
	"gpustack_instance_accelerator_cores_utilization_percent",
	"gpustack_instance_accelerator_temperature_celsius",
	"gpustack_instance_accelerator_power_usage_watts",
	"gpustack_instance_accelerator_unhealthy",
}

// _CapabilityFamily is the device-scoped probe family, which carries no Instance dimension.
const _CapabilityFamily = "gpustack_accelerator_process_capability"

// collectorFixture returns a collector over a poller holding the given round and a device
// manager holding the given monitor snapshot.
func collectorFixture(round *Round, monitor *detector.MonitorSnapshot) prometheus.Collector {
	p := &Poller{nodeName: "node-1"}
	if round != nil {
		p.round.Store(round)
	}
	return NewCollector(p, func() *detector.MonitorSnapshot { return monitor })
}

// monitorSnapshotFixture returns a snapshot stored the given time ago, carrying the Instance's
// card and one the Instance was not allocated.
func monitorSnapshotFixture(age time.Duration) *detector.MonitorSnapshot {
	return &detector.MonitorSnapshot{
		Timestamp: time.Now().Add(-age),
		Groups: device.MetricsGroupList{{
			Manufacturer: "nvidia",
			Timestamp:    time.Now().Add(-age),
			Accelerators: []device.AcceleratorMetrics{
				{
					ID: "gpu-uuid-1", Memory: 81920, MemoryUsage: 1024,
					MemoryUtilization: 12, CoresUtilization: 34,
					Temperature: 42, PowerUsage: 120,
				},
				{ID: "gpu-uuid-someone-else", Memory: 81920, MemoryUsage: 4096},
			},
		}},
	}
}

// measuredRound is a successful round carrying one Instance holding the WHOLE of its card, exported
// by this device manager.
func measuredRound() *Round {
	return carvedRound(workercore.AcceleratorAllocation{
		ID: "gpu-uuid-1", Index: 3, Mode: workercore.DeviceAllocationModeExclusive,
	})
}

// baseRound is the round before an allocation is wired onto it.
func baseRound() *Round {
	return &Round{
		Duration: 250 * time.Millisecond,
		Snapshot: &Snapshot{
			Timestamp:     time.Now(),
			UsageMeasured: true,
			Exporting:     true,
			Instances: []InstanceSample{{
				Namespace: "tenant",
				Name:      "inst",
				UID:       "instance-uid-1",
				Accelerators: []workercore.DevicesAllocationGroup{{
					ID:           "grp",
					Manufacturer: "nvidia",
					Accelerators: []workercore.AcceleratorAllocation{{ID: "gpu-uuid-1", Index: 3}},
				}},
				Sample: worker.InstanceMetricsSample{
					CPUTotalMilliCores: 2000,
					CPUUsedMilliCores:  ptr.To[uint64](500),
					MemoryTotalMiB:     4096,
					MemoryUsedMiB:      ptr.To[uint64](1024),
					StorageTotalMiB:    10240,
					StorageUsedMiB:     ptr.To[uint64](3072),
				},
			}},
		},
	}
}

// slicedRound is a successful round whose Instance holds a quarter of its card rather than the
// whole of it, with the compute cap where the allocator enforced it: the container's own request.
func slicedRound() *Round {
	return carvedRound(workercore.AcceleratorAllocation{
		ID:        "gpu-uuid-1",
		Index:     3,
		Mode:      workercore.DeviceAllocationModeSliced,
		Allocated: nodefeature.ResourceMaxUnits / 4,
	})
}

// partitionedRound is the same Instance holding a hardware partition — a 1g.10gb of the card — where
// the compute cap is the driver's to state and reaches the block from the producer's own read.
func partitionedRound() *Round {
	return carvedRound(workercore.AcceleratorAllocation{
		ID:                          "gpu-uuid-1",
		Index:                       3,
		Mode:                        workercore.DeviceAllocationModePartitioned,
		Allocated:                   nodefeature.ResourceMaxUnits / 8,
		AllocatedPhysicalProfile:    "1g.10gb",
		AllocatedPhysicalPlacements: []workercore.AcceleratorPlacement{{Start: 0, Length: 1}},
	})
}

// carvedRound is the round both carved modes share: one Instance, one container, holding the given
// share of one card. The container's compute request stays on it whichever mode it is, so a partition
// case proves its cap is not read from there.
func carvedRound(accelerator workercore.AcceleratorAllocation) *Round {
	round := baseRound()
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Namespace: "tenant", Name: "inst", UID: "pod-uid-1"},
		Spec: core.PodSpec{
			Containers: []core.Container{{
				Name: "main",
				Resources: core.ResourceRequirements{
					Limits: core.ResourceList{
						nodefeature.GetAcceleratableSlicedCoresPercentageResourceName("nvidia"): *resource.NewQuantity(25, resource.DecimalSI),
					},
				},
			}},
		},
	}
	allocations := deviceplugin.PodAllocations{
		"main": {
			Devices: workercore.DevicesStatus{
				Groups: []workercore.DevicesAllocationGroup{{
					ID: "grp", Manufacturer: "nvidia",
					Accelerators: []workercore.AcceleratorAllocation{accelerator},
				}},
			},
		},
	}

	inst := &round.Snapshot.Instances[0]
	inst.Accelerators = allocations.Aggregate().Groups
	inst.Grants = kubemetrics.NewAcceleratorGrants(pod, allocations)
	return round
}

// sliceSectionFixture returns a slice section that measured the Instance's card, carrying the
// given records and device diagnostics.
func sliceSectionFixture(
	usages []detector.SliceUsage,
	devices []detector.SliceDeviceDiagnostics,
) *detector.MonitorSliceSection {
	return &detector.MonitorSliceSection{
		SchemaVersion: detector.MonitorSliceSchemaVersion,
		Usages:        usages,
		Devices:       devices,
	}
}

// measuredDevice is a device whose per-process pass answered both entry points.
func measuredDevice(deviceID string) detector.SliceDeviceDiagnostics {
	return detector.SliceDeviceDiagnostics{Manufacturer: "nvidia", DeviceID: deviceID}
}

// sliceUsageFixture is one measured record for the fixture Pod's container and card.
func sliceUsageFixture(memoryMiB *uint64, coresPercent *uint32) detector.SliceUsage {
	return detector.SliceUsage{
		Manufacturer: "nvidia", PodUID: "pod-uid-1", Container: "main", DeviceID: "gpu-uuid-1",
		MemoryUsedMiB: memoryMiB, CoresUtilizationPercent: coresPercent,
	}
}

// withSlices returns the monitor snapshot fixture carrying the given slice section.
func withSlices(section *detector.MonitorSliceSection) *detector.MonitorSnapshot {
	monitor := monitorSnapshotFixture(0)
	monitor.Slices = section
	return monitor
}

// _InstanceFamilies are the per-Instance families, named so a case can assert their absence.
var _InstanceFamilies = []string{
	"gpustack_instance_cpu_total_millicores",
	"gpustack_instance_cpu_used_millicores",
	"gpustack_instance_memory_total_mib",
	"gpustack_instance_memory_used_mib",
	"gpustack_instance_storage_total_mib",
	"gpustack_instance_storage_used_mib",
}

func TestCollector(t *testing.T) {
	// The exposition is compared in full rather than by substring: the name, the unit suffix,
	// the HELP, the GAUGE type and the exact label set are all part of the contract a
	// dashboard is written against.
	testCases := []struct {
		name string

		round *Round
		// monitor is the device manager's monitor snapshot, nil meaning none was ever stored.
		monitor *detector.MonitorSnapshot

		// families are the ones this case asserts, and wantExposition what they must render to.
		families       []string
		wantExposition string
		// wantAbsent are families the round must publish nothing for.
		wantAbsent []string
	}{
		{
			name:     "publishes every pair for the instances of this node",
			round:    measuredRound(),
			families: _InstanceFamilies,
			wantExposition: `
# HELP gpustack_instance_cpu_total_millicores The CPU limit of the Instance in milli-cores.
# TYPE gpustack_instance_cpu_total_millicores gauge
gpustack_instance_cpu_total_millicores{instance_name="inst",instance_uid="instance-uid-1",namespace="tenant",node="node-1"} 2000
# HELP gpustack_instance_cpu_used_millicores The CPU usage of the Instance in milli-cores.
# TYPE gpustack_instance_cpu_used_millicores gauge
gpustack_instance_cpu_used_millicores{instance_name="inst",instance_uid="instance-uid-1",namespace="tenant",node="node-1"} 500
# HELP gpustack_instance_memory_total_mib The memory limit of the Instance in MiB.
# TYPE gpustack_instance_memory_total_mib gauge
gpustack_instance_memory_total_mib{instance_name="inst",instance_uid="instance-uid-1",namespace="tenant",node="node-1"} 4096
# HELP gpustack_instance_memory_used_mib The working set memory usage of the Instance in MiB.
# TYPE gpustack_instance_memory_used_mib gauge
gpustack_instance_memory_used_mib{instance_name="inst",instance_uid="instance-uid-1",namespace="tenant",node="node-1"} 1024
# HELP gpustack_instance_storage_total_mib The ephemeral storage limit of the Instance in MiB.
# TYPE gpustack_instance_storage_total_mib gauge
gpustack_instance_storage_total_mib{instance_name="inst",instance_uid="instance-uid-1",namespace="tenant",node="node-1"} 10240
# HELP gpustack_instance_storage_used_mib The ephemeral storage usage of the Instance in MiB, the pod-level aggregate the kubelet evicts on.
# TYPE gpustack_instance_storage_used_mib gauge
gpustack_instance_storage_used_mib{instance_name="inst",instance_uid="instance-uid-1",namespace="tenant",node="node-1"} 3072
`,
		},
		{
			name:     "reports a healthy source alongside the figures",
			round:    measuredRound(),
			families: []string{"gpustack_instance_metrics_collector_success", "gpustack_instance_metrics_collector_duration_seconds"},
			wantExposition: `
# HELP gpustack_instance_metrics_collector_duration_seconds How long the last sampling round of this source took.
# TYPE gpustack_instance_metrics_collector_duration_seconds gauge
gpustack_instance_metrics_collector_duration_seconds{source="kubelet"} 0.25
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 1
gpustack_instance_metrics_collector_success{source="snapshot"} 0
`,
		},
		{
			name:     "reports a failed round without publishing stale figures",
			round:    &Round{Duration: 10 * time.Second},
			families: []string{"gpustack_instance_metrics_collector_success", "gpustack_instance_metrics_collector_duration_seconds"},
			wantExposition: `
# HELP gpustack_instance_metrics_collector_duration_seconds How long the last sampling round of this source took.
# TYPE gpustack_instance_metrics_collector_duration_seconds gauge
gpustack_instance_metrics_collector_duration_seconds{source="kubelet"} 10
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 0
gpustack_instance_metrics_collector_success{source="snapshot"} 0
`,
			wantAbsent: _InstanceFamilies,
		},
		{
			name: "publishes no figures when another device manager of the node exports",
			round: func() *Round {
				r := measuredRound()
				r.Snapshot.Exporting = false
				return r
			}(),
			// The round succeeded, so the source reads healthy — this device manager simply is
			// not the one publishing the node's Instances.
			families: []string{"gpustack_instance_metrics_collector_success"},
			wantExposition: `
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 1
gpustack_instance_metrics_collector_success{source="snapshot"} 0
`,
			wantAbsent: _InstanceFamilies,
		},
		{
			name: "leaves an unmeasured figure unpublished rather than reporting it as zero",
			round: func() *Round {
				r := measuredRound()
				r.Snapshot.Instances[0].Sample.StorageUsedMiB = nil
				return r
			}(),
			families: []string{"gpustack_instance_storage_total_mib"},
			wantExposition: `
# HELP gpustack_instance_storage_total_mib The ephemeral storage limit of the Instance in MiB.
# TYPE gpustack_instance_storage_total_mib gauge
gpustack_instance_storage_total_mib{instance_name="inst",instance_uid="instance-uid-1",namespace="tenant",node="node-1"} 10240
`,
			// Publishing 0 would read as an idle disk rather than as an unknown one.
			wantAbsent: []string{"gpustack_instance_storage_used_mib"},
		},
		{
			name:     "reports itself before the first round rather than staying silent",
			round:    nil,
			families: []string{"gpustack_instance_metrics_collector_success"},
			// "No data yet" and "this node runs no Instance" must not look the same.
			wantExposition: `
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 0
gpustack_instance_metrics_collector_success{source="snapshot"} 0
`,
			wantAbsent: _InstanceFamilies,
		},
		{
			name:     "publishes the allocated accelerators against a fresh snapshot",
			round:    measuredRound(),
			monitor:  monitorSnapshotFixture(0),
			families: _AcceleratorFamilies,
			wantExposition: `
# HELP gpustack_instance_accelerator_cores_utilization_percent How much of the Instance's own compute allowance on the accelerator it was measured using, in percent; it may exceed 100 where a cap is not being enforced.
# TYPE gpustack_instance_accelerator_cores_utilization_percent gauge
gpustack_instance_accelerator_cores_utilization_percent{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 34
# HELP gpustack_instance_accelerator_memory_total_mib The memory the Instance was granted on the accelerator in MiB: the whole device, a logical slice's quota, or a hardware partition's own capacity.
# TYPE gpustack_instance_accelerator_memory_total_mib gauge
gpustack_instance_accelerator_memory_total_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 81920
# HELP gpustack_instance_accelerator_memory_used_mib The memory the Instance was measured holding of that grant in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 1024
# HELP gpustack_instance_accelerator_memory_utilization_percent The used memory over the granted memory, in percent.
# TYPE gpustack_instance_accelerator_memory_utilization_percent gauge
gpustack_instance_accelerator_memory_utilization_percent{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 12
# HELP gpustack_instance_accelerator_power_usage_watts The power usage of the whole accelerator allocated to the Instance in Watts.
# TYPE gpustack_instance_accelerator_power_usage_watts gauge
gpustack_instance_accelerator_power_usage_watts{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 120
# HELP gpustack_instance_accelerator_temperature_celsius The temperature of the whole accelerator allocated to the Instance in Celsius.
# TYPE gpustack_instance_accelerator_temperature_celsius gauge
gpustack_instance_accelerator_temperature_celsius{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 42
# HELP gpustack_instance_accelerator_unhealthy Whether the whole accelerator allocated to the Instance is unhealthy.
# TYPE gpustack_instance_accelerator_unhealthy gauge
gpustack_instance_accelerator_unhealthy{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 0
`,
		},
		{
			// The snapshot carries a second card of the same manufacturer that this Instance
			// was not allocated; publishing it would report someone else's card as this
			// Instance's.
			name:     "publishes only the cards the instance was allocated",
			round:    measuredRound(),
			monitor:  monitorSnapshotFixture(0),
			families: []string{"gpustack_instance_accelerator_memory_used_mib"},
			wantExposition: `
# HELP gpustack_instance_accelerator_memory_used_mib The memory the Instance was measured holding of that grant in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 1024
`,
		},
		{
			// The monitor only replaces its snapshot after a successful sample, so an old one
			// is the last thing that worked rather than the current state.
			name:     "a stale snapshot publishes no accelerator figures",
			round:    measuredRound(),
			monitor:  monitorSnapshotFixture(time.Minute),
			families: []string{"gpustack_instance_metrics_collector_success"},
			wantExposition: `
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 1
gpustack_instance_metrics_collector_success{source="snapshot"} 0
`,
			wantAbsent: _AcceleratorFamilies,
		},
		{
			// Each device manager samples only its own manufacturer's cards, so an amd one
			// answers for none of this Instance's nvidia cards rather than reporting them
			// missing.
			name:  "another manufacturer's snapshot answers for none of these cards",
			round: measuredRound(),
			monitor: func() *detector.MonitorSnapshot {
				m := monitorSnapshotFixture(0)
				m.Groups[0].Manufacturer = "amd"
				return m
			}(),
			families: []string{"gpustack_instance_metrics_collector_success"},
			wantExposition: `
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 1
gpustack_instance_metrics_collector_success{source="snapshot"} 1
`,
			wantAbsent: _AcceleratorFamilies,
		},
		{
			// Device IDs are disjoint across manufacturers, so every device manager of the node
			// publishes its own cards even though only one publishes the pod-level figures.
			name: "accelerator figures are published even by the device manager that is not exporting",
			round: func() *Round {
				r := measuredRound()
				r.Snapshot.Exporting = false
				return r
			}(),
			monitor:  monitorSnapshotFixture(0),
			families: []string{"gpustack_instance_accelerator_memory_used_mib"},
			wantExposition: `
# HELP gpustack_instance_accelerator_memory_used_mib The memory the Instance was measured holding of that grant in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 1024
`,
			wantAbsent: _InstanceFamilies,
		},
		{
			// The round is what knows which Instances run here and what each was allocated.
			name:       "a failed round publishes no accelerator figures either",
			round:      &Round{Duration: time.Second},
			monitor:    monitorSnapshotFixture(0),
			families:   []string{"gpustack_instance_metrics_collector_success"},
			wantAbsent: _AcceleratorFamilies,
			wantExposition: `
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 0
gpustack_instance_metrics_collector_success{source="snapshot"} 1
`,
		},
		{
			// One failed source must not blank the other. The kubelet has no part in the
			// accelerator figures: the allocation comes from the Pod and the readings from the
			// monitor loop, so both are still there to publish.
			name: "a failed kubelet read still publishes the accelerator figures",
			round: func() *Round {
				r := measuredRound()
				r.Snapshot.UsageMeasured = false
				inst := &r.Snapshot.Instances[0]
				inst.Sample.CPUUsedMilliCores = nil
				inst.Sample.MemoryUsedMiB = nil
				inst.Sample.StorageUsedMiB = nil
				return r
			}(),
			monitor: monitorSnapshotFixture(0),
			families: []string{
				"gpustack_instance_accelerator_memory_used_mib",
				"gpustack_instance_cpu_total_millicores",
				"gpustack_instance_metrics_collector_success",
			},
			wantAbsent: []string{
				"gpustack_instance_cpu_used_millicores",
				"gpustack_instance_memory_used_mib",
				"gpustack_instance_storage_used_mib",
			},
			wantExposition: `
# HELP gpustack_instance_accelerator_memory_used_mib The memory the Instance was measured holding of that grant in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Exclusive",namespace="tenant",node="node-1"} 1024
# HELP gpustack_instance_cpu_total_millicores The CPU limit of the Instance in milli-cores.
# TYPE gpustack_instance_cpu_total_millicores gauge
gpustack_instance_cpu_total_millicores{instance_name="inst",instance_uid="instance-uid-1",namespace="tenant",node="node-1"} 2000
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 0
gpustack_instance_metrics_collector_success{source="snapshot"} 1
`,
		},
		{
			// The grant is the total and the measurement is the used figure, both in the Instance's
			// own terms: a quarter of the card's 81920 MiB, and 18% of the card restated against
			// the 25% cap the allocator enforced.
			name:  "a carved share reports its grant and its own usage",
			round: slicedRound(),
			monitor: withSlices(sliceSectionFixture(
				[]detector.SliceUsage{sliceUsageFixture(ptr.To[uint64](6144), ptr.To[uint32](18))},
				[]detector.SliceDeviceDiagnostics{measuredDevice("gpu-uuid-1")},
			)),
			families: _AcceleratorFamilies,
			wantExposition: `
# HELP gpustack_instance_accelerator_cores_utilization_percent How much of the Instance's own compute allowance on the accelerator it was measured using, in percent; it may exceed 100 where a cap is not being enforced.
# TYPE gpustack_instance_accelerator_cores_utilization_percent gauge
gpustack_instance_accelerator_cores_utilization_percent{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 72
# HELP gpustack_instance_accelerator_memory_total_mib The memory the Instance was granted on the accelerator in MiB: the whole device, a logical slice's quota, or a hardware partition's own capacity.
# TYPE gpustack_instance_accelerator_memory_total_mib gauge
gpustack_instance_accelerator_memory_total_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 20480
# HELP gpustack_instance_accelerator_memory_used_mib The memory the Instance was measured holding of that grant in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 6144
# HELP gpustack_instance_accelerator_memory_utilization_percent The used memory over the granted memory, in percent.
# TYPE gpustack_instance_accelerator_memory_utilization_percent gauge
gpustack_instance_accelerator_memory_utilization_percent{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 30
# HELP gpustack_instance_accelerator_power_usage_watts The power usage of the whole accelerator allocated to the Instance in Watts.
# TYPE gpustack_instance_accelerator_power_usage_watts gauge
gpustack_instance_accelerator_power_usage_watts{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 120
# HELP gpustack_instance_accelerator_temperature_celsius The temperature of the whole accelerator allocated to the Instance in Celsius.
# TYPE gpustack_instance_accelerator_temperature_celsius gauge
gpustack_instance_accelerator_temperature_celsius{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 42
# HELP gpustack_instance_accelerator_unhealthy Whether the whole accelerator allocated to the Instance is unhealthy.
# TYPE gpustack_instance_accelerator_unhealthy gauge
gpustack_instance_accelerator_unhealthy{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 0
`,
		},
		{
			// The partition names and sizes ITSELF: the series is keyed by the MIG UUID rather than
			// the parent card's, and its total is the partition's own capacity. No manufacturer
			// serves a per-partition compute figure, so that family publishes no sample at all.
			name:  "a hardware partition reports under its own identity",
			round: partitionedRound(),
			monitor: withSlices(&detector.MonitorSliceSection{
				SchemaVersion: detector.MonitorSliceSchemaVersion,
				Partitions: []detector.SlicePartition{{
					Manufacturer: "nvidia", PodUID: "pod-uid-1", Container: "main",
					DeviceID:       "gpu-uuid-1",
					ID:             "MIG-aaa",
					MemoryTotalMiB: ptr.To[uint64](9856),
					MemoryUsedMiB:  ptr.To[uint64](2464),
					CoresReason:    device.AcceleratorProcessReasonUnsupported,
				}},
			}),
			families: []string{
				"gpustack_instance_accelerator_memory_total_mib",
				"gpustack_instance_accelerator_memory_used_mib",
				"gpustack_instance_accelerator_memory_utilization_percent",
			},
			wantAbsent: []string{"gpustack_instance_accelerator_cores_utilization_percent"},
			wantExposition: `
# HELP gpustack_instance_accelerator_memory_total_mib The memory the Instance was granted on the accelerator in MiB: the whole device, a logical slice's quota, or a hardware partition's own capacity.
# TYPE gpustack_instance_accelerator_memory_total_mib gauge
gpustack_instance_accelerator_memory_total_mib{id="MIG-aaa",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Partitioned",namespace="tenant",node="node-1"} 9856
# HELP gpustack_instance_accelerator_memory_used_mib The memory the Instance was measured holding of that grant in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="MIG-aaa",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Partitioned",namespace="tenant",node="node-1"} 2464
# HELP gpustack_instance_accelerator_memory_utilization_percent The used memory over the granted memory, in percent.
# TYPE gpustack_instance_accelerator_memory_utilization_percent gauge
gpustack_instance_accelerator_memory_utilization_percent{id="MIG-aaa",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Partitioned",namespace="tenant",node="node-1"} 25
`,
		},
		{
			// A Prometheus gauge cannot say "unknown", so the only honest report of an
			// unmeasurable figure is no sample at all: publishing 0 would read as idle.
			name:  "an unmeasurable figure publishes no sample while the grant still does",
			round: slicedRound(),
			monitor: withSlices(sliceSectionFixture(
				[]detector.SliceUsage{sliceUsageFixture(nil, nil)},
				[]detector.SliceDeviceDiagnostics{measuredDevice("gpu-uuid-1")},
			)),
			families: []string{"gpustack_instance_accelerator_memory_total_mib"},
			wantExposition: `
# HELP gpustack_instance_accelerator_memory_total_mib The memory the Instance was granted on the accelerator in MiB: the whole device, a logical slice's quota, or a hardware partition's own capacity.
# TYPE gpustack_instance_accelerator_memory_total_mib gauge
gpustack_instance_accelerator_memory_total_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 20480
`,
			wantAbsent: []string{
				"gpustack_instance_accelerator_memory_used_mib",
				"gpustack_instance_accelerator_memory_utilization_percent",
				"gpustack_instance_accelerator_cores_utilization_percent",
			},
		},
		{
			// THE SUBSTITUTION THIS SURFACE REFUSES. The card is measured at 1024 MiB across every
			// tenant on it; a carved share nothing could measure publishes no usage rather than
			// that.
			name:     "a share the section cannot answer for publishes no usage, never the card's",
			round:    slicedRound(),
			monitor:  withSlices(sliceSectionFixture(nil, nil)),
			families: []string{"gpustack_instance_accelerator_memory_total_mib"},
			wantExposition: `
# HELP gpustack_instance_accelerator_memory_total_mib The memory the Instance was granted on the accelerator in MiB: the whole device, a logical slice's quota, or a hardware partition's own capacity.
# TYPE gpustack_instance_accelerator_memory_total_mib gauge
gpustack_instance_accelerator_memory_total_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 20480
`,
			wantAbsent: []string{
				"gpustack_instance_accelerator_memory_used_mib",
				"gpustack_instance_accelerator_cores_utilization_percent",
			},
		},
		{
			// A device the section measured and holds no record for is a container that held no
			// process on it, which is a measured zero rather than an absence.
			name:  "a measured device with no record for the share publishes zeros",
			round: slicedRound(),
			monitor: withSlices(sliceSectionFixture(nil,
				[]detector.SliceDeviceDiagnostics{measuredDevice("gpu-uuid-1")},
			)),
			families: []string{
				"gpustack_instance_accelerator_memory_used_mib",
				"gpustack_instance_accelerator_cores_utilization_percent",
			},
			wantExposition: `
# HELP gpustack_instance_accelerator_cores_utilization_percent How much of the Instance's own compute allowance on the accelerator it was measured using, in percent; it may exceed 100 where a cap is not being enforced.
# TYPE gpustack_instance_accelerator_cores_utilization_percent gauge
gpustack_instance_accelerator_cores_utilization_percent{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 0
# HELP gpustack_instance_accelerator_memory_used_mib The memory the Instance was measured holding of that grant in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 0
`,
		},
		{
			// A producer whose schema this consumer does not know carries no figure it can read,
			// and no device list it can trust to key a capability series by.
			// The quota survives a section this build cannot read, and every measured figure goes —
			// but the SKEW ITSELF is published, because a consumer newer than the device manager it
			// reads is otherwise indistinguishable from hardware that cannot be measured, which is the
			// one confusion the schema version exists to remove. The devices come from the Instance's
			// own allocation, since the unreadable section's list cannot be believed.
			name:  "a section of an unknown schema publishes the totals and the skew",
			round: slicedRound(),
			monitor: withSlices(&detector.MonitorSliceSection{
				SchemaVersion: detector.MonitorSliceSchemaVersion + 1,
				Usages:        []detector.SliceUsage{sliceUsageFixture(ptr.To[uint64](6144), ptr.To[uint32](18))},
				Devices:       []detector.SliceDeviceDiagnostics{measuredDevice("gpu-uuid-1")},
			}),
			families: []string{"gpustack_instance_accelerator_memory_total_mib", _CapabilityFamily},
			wantExposition: `
# HELP gpustack_accelerator_process_capability Whether the accelerator's per-process query answered on this node's driver, per entry point, carrying the reason it did not.
# TYPE gpustack_accelerator_process_capability gauge
gpustack_accelerator_process_capability{entry_point="cores",id="gpu-uuid-1",manufacturer="nvidia",node="node-1",reason="version_skew"} 0
gpustack_accelerator_process_capability{entry_point="memory",id="gpu-uuid-1",manufacturer="nvidia",node="node-1",reason="version_skew"} 0
# HELP gpustack_instance_accelerator_memory_total_mib The memory the Instance was granted on the accelerator in MiB: the whole device, a logical slice's quota, or a hardware partition's own capacity.
# TYPE gpustack_instance_accelerator_memory_total_mib gauge
gpustack_instance_accelerator_memory_total_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",mode="Sliced",namespace="tenant",node="node-1"} 20480
`,
			wantAbsent: []string{
				"gpustack_instance_accelerator_memory_used_mib",
				"gpustack_instance_accelerator_cores_utilization_percent",
			},
		},
		{
			// Per device, not per manufacturer: a node where one card answers and another does
			// not must yield two distinguishable results rather than two samples nothing can
			// tell apart.
			name:  "reports each device's own capability, per entry point",
			round: slicedRound(),
			monitor: withSlices(sliceSectionFixture(nil, []detector.SliceDeviceDiagnostics{
				{
					Manufacturer: "nvidia", DeviceID: "gpu-uuid-1",
					CoresReason: device.AcceleratorProcessReasonUnsupported,
				},
				{
					Manufacturer: "nvidia", DeviceID: "gpu-uuid-2",
					MemoryReason: device.AcceleratorProcessReasonDriverError,
					CoresReason:  device.AcceleratorProcessReasonDriverError,
				},
			})),
			families: []string{_CapabilityFamily},
			// The reasons are not interchangeable: a driver briefly unreachable must never read
			// as a verdict about the hardware.
			wantExposition: `
# HELP gpustack_accelerator_process_capability Whether the accelerator's per-process query answered on this node's driver, per entry point, carrying the reason it did not.
# TYPE gpustack_accelerator_process_capability gauge
gpustack_accelerator_process_capability{entry_point="cores",id="gpu-uuid-1",manufacturer="nvidia",node="node-1",reason="unsupported"} 0
gpustack_accelerator_process_capability{entry_point="cores",id="gpu-uuid-2",manufacturer="nvidia",node="node-1",reason="transient_driver_error"} 0
gpustack_accelerator_process_capability{entry_point="memory",id="gpu-uuid-1",manufacturer="nvidia",node="node-1",reason=""} 1
gpustack_accelerator_process_capability{entry_point="memory",id="gpu-uuid-2",manufacturer="nvidia",node="node-1",reason="transient_driver_error"} 0
`,
		},
		{
			// A duplicate label set fails a whole scrape, so a section repeating a device is
			// reported once rather than twice.
			name:  "a device the section repeats is reported once",
			round: slicedRound(),
			monitor: withSlices(sliceSectionFixture(nil, []detector.SliceDeviceDiagnostics{
				measuredDevice("gpu-uuid-1"),
				{
					Manufacturer: "nvidia", DeviceID: "gpu-uuid-1",
					MemoryReason: device.AcceleratorProcessReasonUnsupported,
				},
			})),
			families: []string{_CapabilityFamily},
			wantExposition: `
# HELP gpustack_accelerator_process_capability Whether the accelerator's per-process query answered on this node's driver, per entry point, carrying the reason it did not.
# TYPE gpustack_accelerator_process_capability gauge
gpustack_accelerator_process_capability{entry_point="cores",id="gpu-uuid-1",manufacturer="nvidia",node="node-1",reason=""} 1
gpustack_accelerator_process_capability{entry_point="memory",id="gpu-uuid-1",manufacturer="nvidia",node="node-1",reason=""} 1
`,
		},
		{
			// Support is not a freshness claim: a probe that answered three periods ago says
			// nothing about this pass.
			name:  "a stale snapshot publishes no capability series",
			round: slicedRound(),
			monitor: func() *detector.MonitorSnapshot {
				monitor := monitorSnapshotFixture(time.Minute)
				monitor.Slices = sliceSectionFixture(nil,
					[]detector.SliceDeviceDiagnostics{measuredDevice("gpu-uuid-1")})
				return monitor
			}(),
			families:   []string{"gpustack_instance_metrics_collector_success"},
			wantAbsent: append([]string{_CapabilityFamily}, _AcceleratorFamilies...),
			wantExposition: `
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 1
gpustack_instance_metrics_collector_success{source="snapshot"} 0
`,
		},
		{
			// The capability is the node's driver's, not an Instance's, so it is published
			// whether or not a round has told this collector which Instances run here.
			name:     "capability is published before the first round",
			round:    nil,
			monitor:  withSlices(sliceSectionFixture(nil, []detector.SliceDeviceDiagnostics{measuredDevice("gpu-uuid-1")})),
			families: []string{_CapabilityFamily},
			wantExposition: `
# HELP gpustack_accelerator_process_capability Whether the accelerator's per-process query answered on this node's driver, per entry point, carrying the reason it did not.
# TYPE gpustack_accelerator_process_capability gauge
gpustack_accelerator_process_capability{entry_point="cores",id="gpu-uuid-1",manufacturer="nvidia",node="node-1",reason=""} 1
gpustack_accelerator_process_capability{entry_point="memory",id="gpu-uuid-1",manufacturer="nvidia",node="node-1",reason=""} 1
`,
		},
		{
			// The gate closed on it, so the round carries no allocation for it: the card it will
			// hold is holding somebody else's work, and publishing that card's figures under this
			// Instance's labels is the defect the gate exists to remove.
			name: "an instance that has started nothing publishes its totals and no card",
			round: func() *Round {
				r := slicedRound()
				inst := &r.Snapshot.Instances[0]
				inst.Accelerators = nil
				inst.Grants = nil
				inst.Sample.CPUUsedMilliCores = ptr.To[uint64](0)
				inst.Sample.MemoryUsedMiB = ptr.To[uint64](0)
				inst.Sample.StorageUsedMiB = ptr.To[uint64](0)
				return r
			}(),
			monitor: withSlices(sliceSectionFixture(
				[]detector.SliceUsage{sliceUsageFixture(ptr.To[uint64](6144), ptr.To[uint32](18))},
				[]detector.SliceDeviceDiagnostics{measuredDevice("gpu-uuid-1")},
			)),
			families: []string{"gpustack_instance_cpu_used_millicores"},
			// Zero is a measurement here: nothing has started, so nothing could consume.
			wantExposition: `
# HELP gpustack_instance_cpu_used_millicores The CPU usage of the Instance in milli-cores.
# TYPE gpustack_instance_cpu_used_millicores gauge
gpustack_instance_cpu_used_millicores{instance_name="inst",instance_uid="instance-uid-1",namespace="tenant",node="node-1"} 0
`,
			wantAbsent: _AcceleratorFamilies,
		},
		{
			name: "a node running no instance publishes its verdict and no series",
			round: func() *Round {
				r := measuredRound()
				r.Snapshot.Instances = nil
				return r
			}(),
			families: []string{"gpustack_instance_metrics_collector_success"},
			wantExposition: `
# HELP gpustack_instance_metrics_collector_success Whether the last sampling round of this source succeeded.
# TYPE gpustack_instance_metrics_collector_success gauge
gpustack_instance_metrics_collector_success{source="kubelet"} 1
gpustack_instance_metrics_collector_success{source="snapshot"} 0
`,
			wantAbsent: _InstanceFamilies,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c := collectorFixture(tc.round, tc.monitor)

			// The client library's own lint: malformed names, missing help, inconsistent
			// types and duplicate label sets all fail here.
			problems, err := testutil.CollectAndLint(c)
			require.NoError(t, err)
			assert.Empty(t, problems, "the exposition must lint clean")

			require.NoError(t,
				testutil.CollectAndCompare(c, strings.NewReader(tc.wantExposition), tc.families...))

			for _, family := range tc.wantAbsent {
				assert.Zero(t, testutil.CollectAndCount(c, family), "%s must not be published", family)
			}
		})
	}
}

// TestCollector_SameFixtureAsTheSubresource is api_exporter_same_fixture: one Pod, one allocation
// and one snapshot section, and the exporter's samples must carry exactly what the metrics
// subresource serves for them — the same identity, the same mode, the same values, and the same
// omission decisions.
//
// The comparison is against the grants index's own resolution because that resolution IS what the
// subresource serves: it puts the entry on accelerators[] untouched. Both surfaces reach it through
// this one index, so what this pins is the only thing that could still diverge — what the exporter
// does with the entry once it holds it, publication and omission.
func TestCollector_SameFixtureAsTheSubresource(t *testing.T) {
	cases := []struct {
		name    string
		section *detector.MonitorSliceSection

		// round is the Instance's own allocation, the logically sliced one unless a case says
		// otherwise.
		round *Round
	}{
		{
			name: "both figures measured",
			section: sliceSectionFixture(
				[]detector.SliceUsage{sliceUsageFixture(ptr.To[uint64](6144), ptr.To[uint32](18))},
				[]detector.SliceDeviceDiagnostics{measuredDevice("gpu-uuid-1")}),
		},
		{
			name: "measured and idle",
			section: sliceSectionFixture(
				[]detector.SliceUsage{sliceUsageFixture(ptr.To[uint64](0), ptr.To[uint32](0))},
				[]detector.SliceDeviceDiagnostics{measuredDevice("gpu-uuid-1")}),
		},
		{
			name: "memory answered, compute did not",
			section: sliceSectionFixture(
				[]detector.SliceUsage{sliceUsageFixture(ptr.To[uint64](6144), nil)},
				[]detector.SliceDeviceDiagnostics{{
					Manufacturer: "nvidia", DeviceID: "gpu-uuid-1",
					CoresReason: device.AcceleratorProcessReasonUnsupported,
				}}),
		},
		{
			name:    "a device the section cannot answer for",
			section: sliceSectionFixture(nil, nil),
		},
		{
			name:    "a producer predating slice reporting",
			section: nil,
		},
		{
			// The partition's own record, whose identity and capacity both come from the producer
			// rather than from the allocation — so a surface deriving either of its own would
			// diverge exactly here.
			name:  "a hardware partition measured on its own handle",
			round: partitionedRound(),
			section: &detector.MonitorSliceSection{
				SchemaVersion: detector.MonitorSliceSchemaVersion,
				Partitions: []detector.SlicePartition{{
					Manufacturer: "nvidia", PodUID: "pod-uid-1", Container: "main",
					DeviceID:       "gpu-uuid-1",
					ID:             "MIG-aaa",
					MemoryTotalMiB: ptr.To[uint64](9856),
					MemoryUsedMiB:  ptr.To[uint64](0),
					CoresReason:    device.AcceleratorProcessReasonUnsupported,
				}},
			},
		},
		{
			name:  "a whole device, where the device is the grant",
			round: measuredRound(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			round := c.round
			if round == nil {
				round = slicedRound()
			}
			monitor := withSlices(c.section)

			// What the subresource serves for this very fixture. One grant per card in every mode
			// these fixtures cover, so one entry.
			resolved := round.Snapshot.Instances[0].Grants.Resolve(
				c.section, "nvidia", &monitor.Groups[0].Accelerators[0])
			require.Len(t, resolved, 1)
			want := resolved[0]

			got := gatherAcceleratorSamples(t, collectorFixture(round, monitor))

			assert.Equal(t, want.ID, got.id, "the series is keyed by what the Instance holds")
			assert.Equal(t, want.Mode, got.mode, "the mode label is the entry's own")
			assertSameOptionality(t, want.MemoryTotalMiB, got, "memory_total_mib")
			assertSameOptionality(t, want.MemoryUsedMiB, got, "memory_used_mib")
			assertSameOptionality(t, want.MemoryUtilizationPercent, got, "memory_utilization_percent")
			assertSameOptionality(t, want.CoresUtilizationPercent, got, "cores_utilization_percent")
		})
	}
}

// assertSameOptionality pins the one rule a gauge surface has to add to the API's: an absent
// figure is no sample at all, because a gauge cannot say "unknown" and a zero would read as idle.
func assertSameOptionality[T uint64 | uint32](
	t *testing.T, field *T, got acceleratorSamples, suffix string,
) {
	t.Helper()
	value, published := got.values[suffix]
	if field == nil {
		assert.False(t, published, "%s is absent in the response, so it publishes no sample", suffix)
		return
	}
	require.True(t, published, "%s is present in the response, so it publishes a sample", suffix)
	assert.Equal(t, float64(*field), value)
}

// acceleratorSamples is what one scrape published for one accelerator: the identity and mode it was
// labeled with, and each figure that produced a sample at all.
type acceleratorSamples struct {
	id     string
	mode   string
	values map[string]float64
}

// gatherAcceleratorSamples scrapes the collector and keeps the per-accelerator families, keyed by
// the part of the name after the family prefix.
func gatherAcceleratorSamples(t *testing.T, collector prometheus.Collector) acceleratorSamples {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	require.NoError(t, registry.Register(collector))

	families, err := registry.Gather()
	require.NoError(t, err)

	const prefix = "gpustack_instance_accelerator_"
	got := acceleratorSamples{values: make(map[string]float64)}
	for _, family := range families {
		suffix, ok := strings.CutPrefix(family.GetName(), prefix)
		if !ok {
			continue
		}
		require.Len(t, family.GetMetric(), 1, "one series per accelerator and figure")
		metric := family.GetMetric()[0]
		got.values[suffix] = metric.GetGauge().GetValue()

		for _, label := range metric.GetLabel() {
			switch label.GetName() {
			case "id":
				got.id = label.GetValue()
			case "mode":
				got.mode = label.GetValue()
			}
		}
	}
	return got
}
