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
)

// collectorFixture returns a collector over a poller holding the given round.
func collectorFixture(round *Round) prometheus.Collector {
	p := &Poller{nodeName: "node-1"}
	if round != nil {
		p.round.Store(round)
	}
	return NewCollector(p)
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
`,
			wantAbsent: _InstanceFamilies,
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
`,
			wantAbsent: _InstanceFamilies,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c := collectorFixture(tc.round)

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
