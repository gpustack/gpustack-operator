package exporter

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
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
		Timestamp:     time.Now().Add(-age),
		PeriodSeconds: 15,
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

// measuredRound is a successful round carrying one Instance, exported by this device manager.
func measuredRound() *Round {
	return &Round{
		Duration: 250 * time.Millisecond,
		Snapshot: &Snapshot{
			Timestamp:     time.Now(),
			PeriodSeconds: 15,
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
# HELP gpustack_instance_accelerator_cores_utilization_percent The cores utilization of the accelerator allocated to the Instance in [0, 100].
# TYPE gpustack_instance_accelerator_cores_utilization_percent gauge
gpustack_instance_accelerator_cores_utilization_percent{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",namespace="tenant",node="node-1"} 34
# HELP gpustack_instance_accelerator_memory_total_mib The total memory of the accelerator allocated to the Instance in MiB.
# TYPE gpustack_instance_accelerator_memory_total_mib gauge
gpustack_instance_accelerator_memory_total_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",namespace="tenant",node="node-1"} 81920
# HELP gpustack_instance_accelerator_memory_used_mib The used memory of the accelerator allocated to the Instance in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",namespace="tenant",node="node-1"} 1024
# HELP gpustack_instance_accelerator_memory_utilization_percent The memory utilization of the accelerator allocated to the Instance in [0, 100].
# TYPE gpustack_instance_accelerator_memory_utilization_percent gauge
gpustack_instance_accelerator_memory_utilization_percent{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",namespace="tenant",node="node-1"} 12
# HELP gpustack_instance_accelerator_power_usage_watts The power usage of the accelerator allocated to the Instance in Watts.
# TYPE gpustack_instance_accelerator_power_usage_watts gauge
gpustack_instance_accelerator_power_usage_watts{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",namespace="tenant",node="node-1"} 120
# HELP gpustack_instance_accelerator_temperature_celsius The temperature of the accelerator allocated to the Instance in Celsius.
# TYPE gpustack_instance_accelerator_temperature_celsius gauge
gpustack_instance_accelerator_temperature_celsius{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",namespace="tenant",node="node-1"} 42
# HELP gpustack_instance_accelerator_unhealthy Whether the accelerator allocated to the Instance is unhealthy.
# TYPE gpustack_instance_accelerator_unhealthy gauge
gpustack_instance_accelerator_unhealthy{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",namespace="tenant",node="node-1"} 0
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
# HELP gpustack_instance_accelerator_memory_used_mib The used memory of the accelerator allocated to the Instance in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",namespace="tenant",node="node-1"} 1024
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
# HELP gpustack_instance_accelerator_memory_used_mib The used memory of the accelerator allocated to the Instance in MiB.
# TYPE gpustack_instance_accelerator_memory_used_mib gauge
gpustack_instance_accelerator_memory_used_mib{id="gpu-uuid-1",index="3",instance_name="inst",instance_uid="instance-uid-1",manufacturer="nvidia",namespace="tenant",node="node-1"} 1024
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
