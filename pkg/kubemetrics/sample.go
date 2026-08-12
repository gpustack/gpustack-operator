// Package kubemetrics reads the Kubernetes-side utilization of an Instance: the declared
// totals off its backing Pod, the measured usage off the node kubelet's stats, and the
// kubelet's stats summary itself through the API-server node proxy.
//
// It is the single implementation behind the two surfaces reporting these figures — the
// Instance "metrics" subresource and the device manager's Prometheus exporter — so the two
// can never drift apart by a unit or a rounding step. The accelerator half of a sample comes
// from the device manager's own monitor snapshot and is not this package's concern.
package kubemetrics

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeletstats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/utils/mathx"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
)

// NewSample returns a sample carrying only what the Instance declared — its CPU, memory and
// storage ceilings — stamped with the current time and with every measured figure absent.
//
// It is exported for the one thing no caller inside this package can do: assert, from beside
// the controller that builds the Pod, that what the controller writes is what these figures
// read back.
func NewSample(pod *core.Pod) *worker.InstanceMetricsSample {
	sample := &worker.InstanceMetricsSample{Timestamp: meta.Now()}
	setTotals(sample, pod)
	return sample
}

// newSampleFromPodStats returns a complete sample: the Instance's declared totals, and the usage
// measured by the kubelet for the backing Pod. Any nil stat field is tolerated — the kubelet
// omits them, e.g. for a pod its stats provider has only just picked up — and leaves the
// corresponding Used figure absent.
func newSampleFromPodStats(pod *core.Pod, ps *kubeletstats.PodStats) *worker.InstanceMetricsSample {
	sample := NewSample(pod)

	if ps.CPU != nil {
		if !ps.CPU.Time.IsZero() {
			sample.Timestamp = ps.CPU.Time
		}
		sample.CPUUsedMilliCores = nanoCoresToMilliCores(ps.CPU.UsageNanoCores)
	}
	if ps.Memory != nil {
		sample.MemoryUsedMiB = bytesToMiB(ps.Memory.WorkingSetBytes)
	}
	// The pod-level ephemeral storage aggregate, not the containers' writable layers: the
	// kubelet evicts against the aggregate — writable layers plus logs plus local emptyDir
	// volumes — so it is the only numerator comparable to StorageTotalMiB. An Instance
	// filling its workspace volume must show as nearly full, not as nearly empty.
	if ps.EphemeralStorage != nil {
		sample.StorageUsedMiB = bytesToMiB(ps.EphemeralStorage.UsedBytes)
	}

	return sample
}

// setTotals fills the sample's declared ceilings from the backing Pod's container limits,
// which the Instance controller writes unchanged from the Instance's spec.resources — only
// the requests are overcommit-scaled. Init containers are excluded: they never run alongside
// the application containers, so counting them would inflate a ceiling nothing can reach.
//
// A resource the Pod declares no limit for totals 0, which reads as "no declared ceiling";
// a consumer computing a percentage has to guard that division either way.
func setTotals(sample *worker.InstanceMetricsSample, pod *core.Pod) {
	var cpuMilliCores, memoryBytes, storageBytes int64
	for i := range pod.Spec.Containers {
		limits := pod.Spec.Containers[i].Resources.Limits
		if q, ok := limits[core.ResourceCPU]; ok {
			cpuMilliCores += q.ScaledValue(resource.Milli)
		}
		if q, ok := limits[core.ResourceMemory]; ok {
			memoryBytes += q.Value()
		}
		if q, ok := limits[core.ResourceEphemeralStorage]; ok {
			storageBytes += q.Value()
		}
	}

	sample.CPUTotalMilliCores = uint64(cpuMilliCores)
	sample.MemoryTotalMiB = mathx.CeilDiv(uint64(memoryBytes), uint64(quantityx.Mi))
	sample.StorageTotalMiB = mathx.CeilDiv(uint64(storageBytes), uint64(quantityx.Mi))
}

// bytesToMiB converts an optional byte figure to MiB, keeping absence absent.
// The quotient rounds up so that any usage the source measured stays visible: an idle
// instance's working set is routinely under 1 MiB, and truncating it would present a
// measured figure as no usage at all.
func bytesToMiB(bytes *uint64) *uint64 {
	if bytes == nil {
		return nil
	}
	mib := mathx.CeilDiv(*bytes, uint64(quantityx.Mi))
	return &mib
}

// nanoCoresToMilliCores converts an optional nanocore figure to milli-cores, keeping absence
// absent and rounding up for the same reason bytesToMiB does: an idle instance burns far
// less than one milli-core, and a percentage does not need the finer unit.
func nanoCoresToMilliCores(nanoCores *uint64) *uint64 {
	if nanoCores == nil {
		return nil
	}
	milliCores := mathx.CeilDiv(*nanoCores, uint64(1_000_000))
	return &milliCores
}
