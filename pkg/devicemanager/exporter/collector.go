package exporter

import (
	"github.com/prometheus/client_golang/prometheus"

	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
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

// The collector's own gauges name the source they report on: the node kubelet answers for the
// pod-level figures, the device manager's monitor loop for the accelerator ones.
const (
	_SourceKubelet  = "kubelet"
	_SourceSnapshot = "snapshot"
)

// NewCollector returns the collector publishing this node's Instance figures, joining what the
// poller sampled from the kubelet to what the monitor loop sampled from the devices.
//
// Collect reads those two in memory and nothing else: no request, no lock held across a source,
// nothing that can block a scrape or fail it.
func NewCollector(poller *Poller, monitorSnapshot func() *detector.MonitorSnapshot) prometheus.Collector {
	// Not "instance": Prometheus attaches its own instance target label, and under the default
	// honor_labels: false it renames a colliding exported label to exported_instance — so a
	// query grouping by instance would silently group by scrape target.
	instanceLabels := []string{"namespace", "instance_name", "instance_uid", "node"}

	// Accelerator figures are per device, so they carry the card they came from — by ID and by
	// the index the manufacturer's own tools name it by — and the manufacturer whose monitor
	// sampled it.
	acceleratorLabels := append(append([]string{}, instanceLabels...), "id", "index", "manufacturer")

	return &instanceCollector{
		poller:          poller,
		monitorSnapshot: monitorSnapshot,

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

		acceleratorMemoryTotal: prometheus.NewDesc(
			_Namespace+"accelerator_memory_total_mib",
			"The total memory of the accelerator allocated to the Instance in MiB.",
			acceleratorLabels, nil,
		),
		acceleratorMemoryUsed: prometheus.NewDesc(
			_Namespace+"accelerator_memory_used_mib",
			"The used memory of the accelerator allocated to the Instance in MiB.",
			acceleratorLabels, nil,
		),
		acceleratorMemoryUtilization: prometheus.NewDesc(
			_Namespace+"accelerator_memory_utilization_percent",
			"The memory utilization of the accelerator allocated to the Instance in [0, 100].",
			acceleratorLabels, nil,
		),
		acceleratorCoresUtilization: prometheus.NewDesc(
			_Namespace+"accelerator_cores_utilization_percent",
			"The cores utilization of the accelerator allocated to the Instance in [0, 100].",
			acceleratorLabels, nil,
		),
		acceleratorTemperature: prometheus.NewDesc(
			_Namespace+"accelerator_temperature_celsius",
			"The temperature of the accelerator allocated to the Instance in Celsius.",
			acceleratorLabels, nil,
		),
		acceleratorPowerUsage: prometheus.NewDesc(
			_Namespace+"accelerator_power_usage_watts",
			"The power usage of the accelerator allocated to the Instance in Watts.",
			acceleratorLabels, nil,
		),
		acceleratorUnhealthy: prometheus.NewDesc(
			_Namespace+"accelerator_unhealthy",
			"Whether the accelerator allocated to the Instance is unhealthy.",
			acceleratorLabels, nil,
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
	poller          *Poller
	monitorSnapshot func() *detector.MonitorSnapshot

	cpuTotal     *prometheus.Desc
	cpuUsed      *prometheus.Desc
	memoryTotal  *prometheus.Desc
	memoryUsed   *prometheus.Desc
	storageTotal *prometheus.Desc
	storageUsed  *prometheus.Desc

	acceleratorMemoryTotal       *prometheus.Desc
	acceleratorMemoryUsed        *prometheus.Desc
	acceleratorMemoryUtilization *prometheus.Desc
	acceleratorCoresUtilization  *prometheus.Desc
	acceleratorTemperature       *prometheus.Desc
	acceleratorPowerUsage        *prometheus.Desc
	acceleratorUnhealthy         *prometheus.Desc

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
	ch <- c.acceleratorMemoryTotal
	ch <- c.acceleratorMemoryUsed
	ch <- c.acceleratorMemoryUtilization
	ch <- c.acceleratorCoresUtilization
	ch <- c.acceleratorTemperature
	ch <- c.acceleratorPowerUsage
	ch <- c.acceleratorUnhealthy
	ch <- c.success
	ch <- c.duration
}

func (c *instanceCollector) Collect(ch chan<- prometheus.Metric) {
	// The two sources are independent and each reports on itself whatever the other did: the
	// kubelet answers for the pod-level figures on the poller's round, the monitor loop for the
	// accelerator ones on its own. The monitor snapshot is read here rather than folded into a
	// round so the accelerator figures are as fresh as the monitor made them, not as the last
	// poll; reading it is a load of a pointer this process already holds.
	monitor := c.monitorSnapshot()
	monitorFresh := detector.MonitorSnapshotFresh(monitor)
	ch <- gauge(c.success, boolValue(monitorFresh), _SourceSnapshot)

	round := c.poller.LastRound()
	if round == nil {
		// Nothing has been sampled yet. Report that rather than silence, so a scrape can tell
		// "no data yet" from "this node runs no Instance".
		ch <- gauge(c.success, 0, _SourceKubelet)
		return
	}

	ch <- gauge(c.success, boolValue(round.Snapshot != nil), _SourceKubelet)
	ch <- gauge(c.duration, round.Duration.Seconds(), _SourceKubelet)

	// A failed round publishes its verdict and no figures at all — including no accelerator
	// ones, since it is the round that knows which Instances this node runs and which devices
	// each was allocated.
	if round.Snapshot == nil {
		return
	}

	for i := range round.Snapshot.Instances {
		inst := &round.Snapshot.Instances[i]
		// Pod-level figures come from exactly one device manager of the node — see
		// (*Poller).exporting — while accelerator figures come from every one of them: device
		// IDs are disjoint across manufacturers, so each answers for its own cards and they
		// never collide.
		if round.Snapshot.Exporting {
			c.collectInstance(ch, inst)
		}
		if monitorFresh {
			c.collectAccelerators(ch, inst, monitor)
		}
	}
}

// collectAccelerators publishes the figures of the devices allocated to one Instance, joining
// its allocation to this device manager's own monitor snapshot.
func (c *instanceCollector) collectAccelerators(
	ch chan<- prometheus.Metric,
	inst *InstanceSample,
	monitor *detector.MonitorSnapshot,
) {
	allocated := detector.AllocatedAcceleratorMetricsOf(monitor, inst.Accelerators)
	for i := range allocated {
		am := &allocated[i].Metrics
		labels := []string{
			inst.Namespace, inst.Name, string(inst.UID), c.poller.nodeName,
			am.ID, strconvx.Itoa(allocated[i].Index), allocated[i].Manufacturer,
		}

		// Every figure here is a reading the vendor library either produced or did not; unlike
		// the pod-level pairs there is no absent-versus-zero to preserve, because the snapshot
		// carries plain values throughout.
		ch <- gauge(c.acceleratorMemoryTotal, float64(am.Memory), labels...)
		ch <- gauge(c.acceleratorMemoryUsed, float64(am.MemoryUsage), labels...)
		ch <- gauge(c.acceleratorMemoryUtilization, float64(am.MemoryUtilization), labels...)
		ch <- gauge(c.acceleratorCoresUtilization, float64(am.CoresUtilization), labels...)
		ch <- gauge(c.acceleratorTemperature, float64(am.Temperature), labels...)
		ch <- gauge(c.acceleratorPowerUsage, float64(am.PowerUsage), labels...)
		ch <- gauge(c.acceleratorUnhealthy, boolValue(am.Unhealthy), labels...)
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
