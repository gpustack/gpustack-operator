package exporter

import (
	"github.com/prometheus/client_golang/prometheus"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
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

// _CapabilityNamespace prefixes the per-process probe family. It is deliberately not
// _Namespace: that family carries no Instance dimension at all, and naming it after one would
// send a reader aggregating by instance_name to an empty result and to the conclusion that the
// capability is unreported rather than that it was never per-Instance.
const _CapabilityNamespace = "gpustack_"

// The collector's own gauges name the source they report on: the node kubelet answers for the
// pod-level figures, the device manager's monitor loop for the accelerator ones.
const (
	_SourceKubelet  = "kubelet"
	_SourceSnapshot = "snapshot"
)

// The entry points a per-process pass reads separately, because a driver commonly serves process
// memory while refusing process utilization. They name the figure each one produces.
const (
	_EntryPointMemory = "memory"
	_EntryPointCores  = "cores"
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

	// Accelerator figures are per accelerator, so they carry what they came from — by ID and by the
	// index the manufacturer's own tools name it by — and the manufacturer whose monitor sampled it.
	//
	// The mode is a label rather than a family of its own, because every family below means the same
	// thing in every mode: the Instance's own grant and its own usage of it. So a query that does not
	// care how the grant was made ignores the label, and one that does groups by it.
	acceleratorLabels := append(append([]string{}, instanceLabels...),
		"id", "index", "manufacturer", "mode")

	// The probe is a property of the node's driver and one of its cards, so it carries neither an
	// Instance nor an allocation: two Instances sharing a card have one answer between them, and
	// giving it Instance labels would publish that one answer twice.
	capabilityLabels := []string{"node", "manufacturer", "id", "entry_point", "reason"}

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
			"The memory the Instance was granted on the accelerator in MiB: the whole device, "+
				"a logical slice's quota, or a hardware partition's own capacity.",
			acceleratorLabels, nil,
		),
		acceleratorMemoryUsed: prometheus.NewDesc(
			_Namespace+"accelerator_memory_used_mib",
			"The memory the Instance was measured holding of that grant in MiB.",
			acceleratorLabels, nil,
		),
		acceleratorMemoryUtilization: prometheus.NewDesc(
			_Namespace+"accelerator_memory_utilization_percent",
			"The used memory over the granted memory, in percent.",
			acceleratorLabels, nil,
		),
		acceleratorCoresUtilization: prometheus.NewDesc(
			_Namespace+"accelerator_cores_utilization_percent",
			"How much of the Instance's own compute allowance on the accelerator it was measured "+
				"using, in percent; it may exceed 100 where a cap is not being enforced.",
			acceleratorLabels, nil,
		),
		acceleratorTemperature: prometheus.NewDesc(
			_Namespace+"accelerator_temperature_celsius",
			"The temperature of the whole accelerator allocated to the Instance in Celsius.",
			acceleratorLabels, nil,
		),
		acceleratorPowerUsage: prometheus.NewDesc(
			_Namespace+"accelerator_power_usage_watts",
			"The power usage of the whole accelerator allocated to the Instance in Watts.",
			acceleratorLabels, nil,
		),
		acceleratorUnhealthy: prometheus.NewDesc(
			_Namespace+"accelerator_unhealthy",
			"Whether the whole accelerator allocated to the Instance is unhealthy.",
			acceleratorLabels, nil,
		),

		processCapability: prometheus.NewDesc(
			_CapabilityNamespace+"accelerator_process_capability",
			"Whether the accelerator's per-process query answered on this node's driver, "+
				"per entry point, carrying the reason it did not.",
			capabilityLabels, nil,
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

	processCapability *prometheus.Desc

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
	ch <- c.processCapability
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

	// The probe answers for this node's driver and cards rather than for an Instance, so it is
	// published once per device here rather than once per Instance below — where a card two
	// Instances share would yield the same label set twice, which fails a whole scrape.
	if monitorFresh {
		c.collectProcessCapability(ch, monitor.Slices, carvedAcceleratorsOfRound(round))
	}

	if round == nil {
		// Nothing has been sampled yet. Report that rather than silence, so a scrape can tell
		// "no data yet" from "this node runs no Instance".
		ch <- gauge(c.success, 0, _SourceKubelet)
		return
	}

	ch <- gauge(c.success, boolValue(round.Snapshot != nil && round.Snapshot.UsageMeasured), _SourceKubelet)
	ch <- gauge(c.duration, round.Duration.Seconds(), _SourceKubelet)

	// A round that failed outright publishes its verdict and no figures: without it there is no
	// list of this node's Instances, and every family here is labeled by one. A round whose
	// kubelet read alone failed is not that round — it carries the Instances and their
	// allocations, so the accelerator families below still publish, and only the measured
	// pod-level figures are missing.
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

// collectAccelerators publishes what one Instance holds on each of its accelerators, resolved in the
// Instance's own terms by the same index the metrics subresource resolves with.
//
// Every decision here was made before it arrived: the grant by the allocation, absence versus zero by
// the producer's own section, the identity and the mode by the resolution. What is left is the one
// rule this surface owns — a figure nothing could state publishes no sample rather than a zero that
// would read as idle or as entitled to nothing.
func (c *instanceCollector) collectAccelerators(
	ch chan<- prometheus.Metric,
	inst *InstanceSample,
	monitor *detector.MonitorSnapshot,
) {
	if inst.Grants == nil {
		// The Pod's allocation could not be read, so nothing here can say whose figures a card's
		// are. Publishing the card's would charge every tenant on it to this Instance.
		return
	}

	allocated := detector.AllocatedAcceleratorMetricsOf(monitor, inst.Accelerators)
	for i := range allocated {
		// One card can yield several entries: an Instance holding two hardware partitions of it holds
		// two grants, each with its own identity and its own figures.
		for _, am := range inst.Grants.Resolve(
			monitor.Slices, allocated[i].Manufacturer, &allocated[i].Metrics,
		) {
			c.publishAccelerator(ch, inst, allocated[i].Index, allocated[i].Manufacturer, am)
		}
	}
}

// publishAccelerator publishes one entry's families, each only where the entry carries a figure: an
// absent figure means nothing could state it, and a gauge of zero would say it measured idle.
func (c *instanceCollector) publishAccelerator(
	ch chan<- prometheus.Metric,
	inst *InstanceSample,
	index uint32,
	manufacturer string,
	am worker.InstanceAcceleratorMetrics,
) {
	labels := []string{
		inst.Namespace, inst.Name, string(inst.UID), c.poller.nodeName,
		am.ID, strconvx.Itoa(index), manufacturer, am.Mode,
	}

	if v := am.MemoryTotalMiB; v != nil {
		ch <- gauge(c.acceleratorMemoryTotal, float64(*v), labels...)
	}
	if v := am.MemoryUsedMiB; v != nil {
		ch <- gauge(c.acceleratorMemoryUsed, float64(*v), labels...)
	}
	if v := am.MemoryUtilizationPercent; v != nil {
		ch <- gauge(c.acceleratorMemoryUtilization, float64(*v), labels...)
	}
	if v := am.CoresUtilizationPercent; v != nil {
		ch <- gauge(c.acceleratorCoresUtilization, float64(*v), labels...)
	}
	// The whole card's own readings, published in every mode: a share of a card has no
	// temperature, no power draw and no health of its own.
	if v := am.TemperatureCelsius; v != nil {
		ch <- gauge(c.acceleratorTemperature, float64(*v), labels...)
	}
	if v := am.PowerUsageWatts; v != nil {
		ch <- gauge(c.acceleratorPowerUsage, float64(*v), labels...)
	}
	if v := am.Unhealthy; v != nil {
		ch <- gauge(c.acceleratorUnhealthy, boolValue(*v), labels...)
	}
}

// collectSliceCapability publishes, per accelerator and per entry point, whether the per-process
// query answered on this node's driver — and the reason it did not, which is what keeps "could
// not measure" from being read as "idle".
//
// A section of an unknown schema publishes nothing: its device list cannot be trusted to mean
// what this consumer reads it as, and deriving one from the allocations instead would publish the
// same device twice on a card two Instances share.
// capabilityDevice is one accelerator the capability probe can answer for, identified the way
// that gauge's labels identify one.
type capabilityDevice struct {
	manufacturer string
	id           string
}

// carvedAcceleratorsOfRound lists the accelerators this node's Instances hold under a carved mode,
// once each. It is the fallback device list for the capability probe, used only where the snapshot's
// own list cannot be believed — so it is derived from what the Pods recorded rather than from any
// reading of the hardware.
func carvedAcceleratorsOfRound(round *Round) []capabilityDevice {
	if round == nil || round.Snapshot == nil {
		return nil
	}

	seen := make(map[capabilityDevice]struct{})
	devices := make([]capabilityDevice, 0, len(round.Snapshot.Instances))
	for i := range round.Snapshot.Instances {
		for _, grp := range round.Snapshot.Instances[i].Accelerators {
			for _, accelerator := range grp.Accelerators {
				switch accelerator.Mode {
				case workercore.DeviceAllocationModeSliced,
					workercore.DeviceAllocationModePartitioned:
				default:
					continue
				}
				dev := capabilityDevice{manufacturer: grp.Manufacturer, id: accelerator.ID}
				if _, dup := seen[dev]; dup {
					continue
				}
				seen[dev] = struct{}{}
				devices = append(devices, dev)
			}
		}
	}
	return devices
}

func (c *instanceCollector) collectProcessCapability(
	ch chan<- prometheus.Metric,
	section *detector.MonitorSliceSection,
	carved []capabilityDevice,
) {
	if section == nil {
		return
	}
	if !detector.KnownMonitorSliceSchema(section.SchemaVersion) {
		// A section this build cannot read tells nothing about the node's drivers, so its own device
		// list is not usable here — but the state itself has to be visible, because a consumer newer
		// than the device manager it reads is otherwise indistinguishable from hardware that cannot be
		// measured, which is the one confusion the schema version exists to remove. The devices come
		// from the Instances' own allocations instead, deduplicated for the same reason the loop below
		// deduplicates: one card two Instances share must not yield the label set twice.
		for _, dev := range carved {
			for _, entryPoint := range []string{_EntryPointMemory, _EntryPointCores} {
				ch <- gauge(c.processCapability, 0,
					c.poller.nodeName, dev.manufacturer, dev.id, entryPoint,
					string(detector.SliceReasonVersionSkew))
			}
		}
		return
	}

	// The producer emits one entry per accelerator, so this only guards against a snapshot that
	// repeats one: a duplicate label set fails the whole scrape, which is worse than reporting
	// the first answer alone.
	seen := make(map[string]struct{}, len(section.Devices))
	for _, diag := range section.Devices {
		key := diag.Manufacturer + "\x00" + diag.DeviceID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		for entryPoint, reason := range map[string]device.AcceleratorProcessReason{
			_EntryPointMemory: diag.MemoryReason,
			_EntryPointCores:  diag.CoresReason,
		} {
			// The reasons are never folded together: "the library does not serve this query" and
			// "this pass could not read it" are different claims, and a driver briefly
			// unreachable must not read as a verdict about the hardware.
			ch <- gauge(c.processCapability,
				boolValue(reason == device.AcceleratorProcessReasonNone),
				c.poller.nodeName, diag.Manufacturer, diag.DeviceID, entryPoint, string(reason))
		}
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
