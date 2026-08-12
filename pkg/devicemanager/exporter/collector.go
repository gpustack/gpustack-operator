package exporter

import (
	"github.com/prometheus/client_golang/prometheus"
)

// The exposed units deliberately deviate from Prometheus convention, which is base units —
// bytes, cores, ratios. They mirror the Instance metrics API field for field instead, so the two
// surfaces reporting the same figure can never disagree by a rounding step. The unit is in every
// name, so nothing here is ambiguous, only unidiomatic.
const (
	// _Namespace prefixes every family this collector publishes.
	_Namespace = "gpustack_instance_"

	// _CollectorNamespace prefixes the collector's report on itself, so a silently degraded
	// exporter is visible in Prometheus rather than only in a log.
	_CollectorNamespace = _Namespace + "metrics_collector_"
)

// _SourceKubelet names the node kubelet in the collector's own gauges.
const _SourceKubelet = "kubelet"

// NewCollector returns the collector publishing this node's Instance figures.
//
// Collect reads the poller's latest round and nothing else: no request, no lock held across a
// source, nothing that can block a scrape or fail it.
func NewCollector(poller *Poller) prometheus.Collector {
	// Not "instance": Prometheus attaches its own instance target label, and under the default
	// honor_labels: false it renames a colliding exported label to exported_instance — so a
	// query grouping by instance would silently group by scrape target.
	instanceLabels := []string{"namespace", "instance_name", "instance_uid", "node"}

	return &instanceCollector{
		poller: poller,

		cpuTotal: prometheus.NewDesc(
			_Namespace+"cpu_total_millicores",
			"The CPU limit of the Instance in milli-cores.",
			instanceLabels, nil,
		),
		cpuUsed: prometheus.NewDesc(
			_Namespace+"cpu_used_millicores",
			"The CPU usage of the Instance in milli-cores.",
			instanceLabels, nil,
		),
		memoryTotal: prometheus.NewDesc(
			_Namespace+"memory_total_mib",
			"The memory limit of the Instance in MiB.",
			instanceLabels, nil,
		),
		memoryUsed: prometheus.NewDesc(
			_Namespace+"memory_used_mib",
			"The working set memory usage of the Instance in MiB.",
			instanceLabels, nil,
		),
		storageTotal: prometheus.NewDesc(
			_Namespace+"storage_total_mib",
			"The ephemeral storage limit of the Instance in MiB.",
			instanceLabels, nil,
		),
		storageUsed: prometheus.NewDesc(
			_Namespace+"storage_used_mib",
			"The ephemeral storage usage of the Instance in MiB, "+
				"the pod-level aggregate the kubelet evicts on.",
			instanceLabels, nil,
		),

		success: prometheus.NewDesc(
			_CollectorNamespace+"success",
			"Whether the last sampling round of this source succeeded.",
			[]string{"source"}, nil,
		),
		duration: prometheus.NewDesc(
			_CollectorNamespace+"duration_seconds",
			"How long the last sampling round of this source took.",
			[]string{"source"}, nil,
		),
	}
}

type instanceCollector struct {
	poller *Poller

	cpuTotal     *prometheus.Desc
	cpuUsed      *prometheus.Desc
	memoryTotal  *prometheus.Desc
	memoryUsed   *prometheus.Desc
	storageTotal *prometheus.Desc
	storageUsed  *prometheus.Desc

	success  *prometheus.Desc
	duration *prometheus.Desc
}

func (c *instanceCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.cpuTotal
	ch <- c.cpuUsed
	ch <- c.memoryTotal
	ch <- c.memoryUsed
	ch <- c.storageTotal
	ch <- c.storageUsed
	ch <- c.success
	ch <- c.duration
}

func (c *instanceCollector) Collect(ch chan<- prometheus.Metric) {
	round := c.poller.LastRound()
	if round == nil {
		// Nothing has been sampled yet. Report that rather than silence, so a scrape can tell
		// "no data yet" from "this node runs no Instance".
		ch <- gauge(c.success, 0, _SourceKubelet)
		return
	}

	ch <- gauge(c.success, boolValue(round.Snapshot != nil), _SourceKubelet)
	ch <- gauge(c.duration, round.Duration.Seconds(), _SourceKubelet)

	// A failed round publishes its verdict and no figures. A round taken by another device
	// manager of this node publishes no figures either — see (*Poller).exporting.
	if round.Snapshot == nil || !round.Snapshot.Exporting {
		return
	}

	for i := range round.Snapshot.Instances {
		c.collectInstance(ch, &round.Snapshot.Instances[i])
	}
}

func (c *instanceCollector) collectInstance(ch chan<- prometheus.Metric, inst *InstanceSample) {
	labels := []string{inst.Namespace, inst.Name, string(inst.UID), c.poller.nodeName}

	// A total is always populated; a used figure is a measurement, and an absent one is left
	// unpublished rather than reported as zero, which would read as idle.
	ch <- gauge(c.cpuTotal, float64(inst.Sample.CPUTotalMilliCores), labels...)
	ch <- gauge(c.memoryTotal, float64(inst.Sample.MemoryTotalMiB), labels...)
	ch <- gauge(c.storageTotal, float64(inst.Sample.StorageTotalMiB), labels...)

	if v := inst.Sample.CPUUsedMilliCores; v != nil {
		ch <- gauge(c.cpuUsed, float64(*v), labels...)
	}
	if v := inst.Sample.MemoryUsedMiB; v != nil {
		ch <- gauge(c.memoryUsed, float64(*v), labels...)
	}
	if v := inst.Sample.StorageUsedMiB; v != nil {
		ch <- gauge(c.storageUsed, float64(*v), labels...)
	}
}

func gauge(desc *prometheus.Desc, value float64, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
