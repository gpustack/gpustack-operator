package kubemetrics

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// fetchPodUsageFromMetricsAPI reads a pod's CPU and memory from the metrics.k8s.io API through
// the generic REST client (no k8s.io/metrics dependency), in nanocores and bytes.
//
// This is the degraded source behind the kubelet: it carries no storage figures and is not
// served at all in a cluster without a metrics adapter. A nil cpu with a nil error means
// exactly that — not served — which the caller has to tell apart from a genuine failure.
//
// The measurement timestamp comes back too, where the adapter supplied one, so the caller can
// reject an entry that predates the pod and therefore belongs to a previous incarnation of the
// same name. It is nil for an untimed measurement, and that check does not apply to those: an
// entry the adapter would not date cannot be dated by us either.
func fetchPodUsageFromMetricsAPI(ctx context.Context, pod *core.Pod) (*uint64, *uint64, *meta.Time, error) {
	raw, err := system.LoopbackKubeClient.Get().CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces/" + pod.Namespace + "/pods/" + pod.Name).
		DoRaw(ctx)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("failed to get pod metrics of %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return parsePodMetricsUsage(raw)
}

// parsePodMetricsUsage decodes the metrics.k8s.io PodMetrics shape and sums the containers'
// usage: CPU in nanocores, memory in bytes.
func parsePodMetricsUsage(raw []byte) (*uint64, *uint64, *meta.Time, error) {
	var podMetrics struct {
		Timestamp  meta.Time `json:"timestamp"`
		Containers []struct {
			Usage map[core.ResourceName]resource.Quantity `json:"usage"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(raw, &podMetrics); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to decode pod metrics: %w", err)
	}

	// A response without containers carries no measurement: report it as unserved rather than
	// as a genuine zero usage.
	if len(podMetrics.Containers) == 0 {
		return nil, nil, nil, nil
	}

	// Any adapter may serve metrics.k8s.io, so a negative or absurd quantity is possible; an
	// unchecked unsigned conversion would turn it into exabytes of "current usage".
	var cpu, memory uint64
	for i := range podMetrics.Containers {
		if q, ok := podMetrics.Containers[i].Usage[core.ResourceCPU]; ok {
			cpu += nonNegative(q.ScaledValue(resource.Nano))
		}
		if q, ok := podMetrics.Containers[i].Usage[core.ResourceMemory]; ok {
			memory += nonNegative(q.Value())
		}
	}
	ts := &podMetrics.Timestamp
	if ts.IsZero() {
		ts = nil
	}
	return &cpu, &memory, ts, nil
}

// nonNegative clamps a signed measurement to an unsigned one.
func nonNegative(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
