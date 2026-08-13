// Package exporter samples the Instances running on this node and publishes them as gauges on
// the device manager's existing /metrics endpoint.
//
// The sampling is a background loop rather than scrape-time work, for two reasons. A scrape
// must never perform I/O — it must not block, and it must not fail the whole endpoint because
// one source is unreachable. And it would buy no freshness: the kubelet's own housekeeping runs
// on roughly ten seconds and the summary it serves is cached, so reading it at scrape time is
// no more current than polling it on a comparable period.
package exporter

import (
	"context"
	"errors"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubemetrics"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/datax"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

// InstanceSample is one Instance's utilization together with the identity to label it by,
// which worker.InstanceMetricsSample does not carry.
type InstanceSample struct {
	// Namespace, Name and UID identify the Instance, not its backing Pod. UID is what keeps a
	// deleted-and-recreated Instance of the same name from continuing the previous
	// incarnation's series.
	Namespace string
	Name      string
	UID       types.UID

	Sample worker.InstanceMetricsSample

	// Accelerators are the devices allocated to the Instance, as its backing Pod records them.
	// Only the allocation is kept here; the figures come from the device manager's own monitor
	// snapshot when a scrape joins the two, so they are as fresh as that snapshot rather than
	// as this round.
	Accelerators []workercore.DevicesAllocationGroup
}

// Snapshot is what a successful round measured.
type Snapshot struct {
	// Timestamp is when the round was stored.
	Timestamp time.Time
	// UsageMeasured reports whether the node kubelet answered this round. When it did not, the
	// Instances below carry their declared totals and no measured figures — the round is still
	// worth keeping, because the accelerator families do not come from the kubelet.
	UsageMeasured bool
	// Exporting reports whether this device manager is the one publishing the node's
	// per-Instance figures; see (*Poller).exporting for the rule.
	Exporting bool
	// Instances holds one entry per Instance the node kubelet measured, and is empty on a node
	// running none.
	Instances []InstanceSample
}

// Round is the poller's latest completed round. Its outcome is held together with what it
// measured, so a reader cannot see one round's figures beside another round's verdict.
type Round struct {
	// Duration is how long the round took, whether it succeeded or not: a source that answers
	// slowly is worth seeing before it starts failing outright.
	Duration time.Duration
	// Snapshot is what the round measured, or nil when it failed. A failed round keeps nothing:
	// reporting the figures of several periods ago as current is worse than reporting none.
	Snapshot *Snapshot
}

// Poller samples this node's Instances on a period and keeps only the latest round, so that a
// scrape reads memory and nothing else.
type Poller struct {
	reader   ctrlcli.Reader
	nodeName string
	period   time.Duration

	round datax.Snapshot[Round]

	// lastFailure and lastRoleFailure carry the previous round's failure and the previous failed
	// election, so a repeat of either is logged quietly. A poller whose source is down would
	// otherwise repeat one line every period for the life of the process. Only the poll loop
	// touches them, so they need no synchronization.
	lastFailure     string
	lastRoleFailure string
}

// New returns a poller for this node's Instances.
func New(c *Config) (*Poller, error) {
	if c.MonitorPeriod <= 0 {
		// waitx polls on the period, so a zero one would spin instead of sampling.
		return nil, errors.New("monitor period must be positive")
	}

	reader := c.ClientReader
	if reader == nil {
		// pkg/manager configures the controller runtime onto the system package while the
		// device manager applies its own config, before this runs. Reading Pods through it
		// costs a lookup in an informer this process already runs for the device plugin —
		// which watches every Pod of this node and indexes them by node name — rather than a
		// list against the API server once per period per node.
		reader = system.LoopbackCtrlClient.Get()
	}

	return &Poller{
		reader:   reader,
		nodeName: osx.Getenv("KUBERNETES_NODE_NAME"),
		period:   c.MonitorPeriod,
	}, nil
}

// LastRound returns the latest completed round, or nil before the first one.
// The caller must not modify it.
func (p *Poller) LastRound() *Round {
	return p.round.Load()
}

// snapshot returns what the latest round measured, or nil when it failed or none has run yet.
// The collector reads the round itself; this is the nil-safe shorthand behind it.
func (p *Poller) snapshot() *Snapshot {
	if r := p.round.Load(); r != nil {
		return r.Snapshot
	}
	return nil
}

// Start polls until the context is canceled. A failing round never stops the loop and never
// fails the process: these metrics are not a critical path, and a device manager that exits
// because a kubelet was briefly unreachable would take the node's device plugin with it.
func (p *Poller) Start(ctx context.Context) error {
	if p.nodeName == "" {
		// The DaemonSet sets this from spec.nodeName, so an empty value is a broken
		// deployment. Report it and serve no Instance gauges rather than take the process down.
		logger.Error(nil, "not polling instance metrics, KUBERNETES_NODE_NAME is not set")
		return nil
	}

	return waitx.UntilContextCancel(ctx, p.period, true, func(ctx context.Context) error {
		p.poll(ctx)
		return nil
	})
}

var logger = klog.Background().WithName("instance-metrics-exporter")

// poll runs one round: read this node's Instance pods from the informer, ask the kubelet what
// it measured for them, and store the result.
func (p *Poller) poll(ctx context.Context) {
	started := time.Now()

	snapshot, err := p.measure(ctx)
	if err != nil {
		// Keep nothing: a scrape reporting the figures of several periods ago as current is
		// worse than reporting none beside a failure gauge.
		p.round.Store(&Round{Duration: time.Since(started)})
		p.logFailure(err)
		return
	}

	// Only a round that measured everything clears the repeat-log guard: a round kept despite a
	// failed kubelet read has already reported that failure, and forgetting it here would log
	// the same line every period.
	if snapshot.UsageMeasured {
		p.lastFailure = ""
	}
	p.round.Store(&Round{Duration: time.Since(started), Snapshot: snapshot})
}

// measure runs the reads of one round.
func (p *Poller) measure(ctx context.Context) (*Snapshot, error) {
	pods, err := p.instancePods(ctx)
	if err != nil {
		return nil, err
	}

	// A failed election costs the pod-level families and nothing else: accelerator figures are
	// not subject to the rule, since device IDs are disjoint across manufacturers. Failing the
	// round over it would drop figures the election has no say in.
	exporting, err := p.exporting(ctx)
	if err != nil {
		p.logRoleFailure(err)
		exporting = false
	}

	// Read afresh, never from the cache: rounds start exactly one period apart while an entry is
	// stamped mid-round, so passing the period here would serve the previous round's readout
	// every other round — half the cadence, with nothing on the wire to show it.
	samples, err := kubemetrics.FetchPodSamples(ctx, p.nodeName, podsOf(pods), 0)
	if err != nil {
		// The kubelet is one of two sources and the other one is untouched by this. Which
		// Instances the node runs and which devices each holds come from the informer, so the
		// accelerator families — which the kubelet has no part in — still have everything they
		// need. Keep the round, carry the declared totals, and let the measured figures be
		// absent beside a zeroed success gauge.
		p.logFailure(err)
		return &Snapshot{
			Timestamp: time.Now(),
			Exporting: exporting,
			Instances: declaredSamples(pods),
		}, nil
	}

	return &Snapshot{
		Timestamp:     time.Now(),
		UsageMeasured: true,
		Exporting:     exporting,
		Instances:     instanceSamples(pods, samples),
	}, nil
}

// logFailure reports a round's failure once, and repeats of it quietly.
func (p *Poller) logFailure(err error) {
	if reason := err.Error(); reason != p.lastFailure {
		p.lastFailure = reason
		logger.Error(err, "polling instance metrics", "node", p.nodeName)
		return
	}
	logger.V(2).Info("polling instance metrics still failing",
		"node", p.nodeName, "reason", p.lastFailure)
}

// logRoleFailure reports a failed election once, and repeats of it quietly. It is tracked apart
// from a round's own failure so that neither silences the first report of the other.
func (p *Poller) logRoleFailure(err error) {
	if reason := err.Error(); reason != p.lastRoleFailure {
		p.lastRoleFailure = reason
		logger.Error(err, "deciding whether to export instance metrics, not exporting them",
			"node", p.nodeName)
		return
	}
	logger.V(2).Info("still cannot decide whether to export instance metrics",
		"node", p.nodeName, "reason", p.lastRoleFailure)
}

// instancePod pairs an Instance pod with the identity of the Instance behind it.
type instancePod struct {
	pod      *core.Pod
	instance InstanceSample
}

// instancePods returns this node's Instance pods, at most one per Instance.
func (p *Poller) instancePods(ctx context.Context) ([]instancePod, error) {
	podList := &core.PodList{}
	err := p.reader.List(ctx, podList,
		ctrlcli.MatchingFieldsSelector{
			Selector: fields.OneTermEqualSelector(deviceplugin.IndexingPodsByNodeName, p.nodeName),
		},
	)
	if err != nil {
		return nil, err
	}

	// An Instance can briefly have two pods in the cache while one is replaced, and both would
	// carry the same label set — keep the newer, so one gather can never emit a duplicate.
	byInstance := make(map[types.UID]instancePod, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		inst, ok := instanceOf(pod)
		if !ok {
			continue
		}
		if kept, dup := byInstance[inst.UID]; dup && !isNewer(pod, kept.pod) {
			continue
		}
		byInstance[inst.UID] = instancePod{pod: pod, instance: inst}
	}

	pods := make([]instancePod, 0, len(byInstance))
	for _, ip := range byInstance {
		pods = append(pods, ip)
	}
	return pods, nil
}

// instanceOf reports the Instance a pod backs, or false when the pod is not one.
//
// The controller ownerReference names the Instance; the app.kubernetes.io/part-of label repeats
// its UID and is checked against it, because that label is what the Instance metrics subresource
// resolves a pod by, and the two surfaces must agree on which pod belongs to which Instance.
// A terminating pod is not one: its figures are on their way out.
func instanceOf(pod *core.Pod) (InstanceSample, bool) {
	if pod.DeletionTimestamp != nil {
		return InstanceSample{}, false
	}

	ref := meta.GetControllerOf(pod)
	if ref == nil || ref.Kind != _InstanceKind ||
		schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind).Group != workercore.GroupName {
		return InstanceSample{}, false
	}
	if pod.Labels[deviceplugin.InstancePartOfLabelKey] != string(ref.UID) {
		return InstanceSample{}, false
	}

	return InstanceSample{
		Namespace: pod.Namespace,
		Name:      ref.Name,
		UID:       ref.UID,
	}, true
}

// _InstanceKind is the owner kind an Instance pod carries.
const _InstanceKind = "Instance"

// isNewer reports whether a was created after b, falling back to the pod name so that two pods
// stamped in the same second still resolve the same way on every poll.
func isNewer(a, b *core.Pod) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.After(b.CreationTimestamp.Time)
	}
	return a.Name > b.Name
}

// podsOf projects the pods to hand to the kubelet read.
func podsOf(pods []instancePod) []*core.Pod {
	out := make([]*core.Pod, 0, len(pods))
	for i := range pods {
		out = append(out, pods[i].pod)
	}
	return out
}

// instanceSamples joins the identities to the measurements, dropping an Instance the kubelet
// did not account for rather than publishing it with no usage at all.
func instanceSamples(
	pods []instancePod,
	samples map[types.UID]*worker.InstanceMetricsSample,
) []InstanceSample {
	out := make([]InstanceSample, 0, len(samples))
	for i := range pods {
		sample, ok := samples[pods[i].pod.UID]
		if !ok {
			continue
		}
		out = append(out, instanceSampleOf(&pods[i], sample))
	}
	return out
}

// declaredSamples is instanceSamples for a round whose kubelet read failed: every Instance is
// kept, carrying only what its Pod declares. Dropping them here would be the rule for a pod the
// kubelet answered about and did not carry, which is a different thing — nothing was measured at
// all this round, and the accelerator families still need the allocations these entries hold.
func declaredSamples(pods []instancePod) []InstanceSample {
	out := make([]InstanceSample, 0, len(pods))
	for i := range pods {
		out = append(out, instanceSampleOf(&pods[i], kubemetrics.NewSample(pods[i].pod)))
	}
	return out
}

// instanceSampleOf joins one Instance's identity to a sample and to its Pod's allocation.
func instanceSampleOf(pod *instancePod, sample *worker.InstanceMetricsSample) InstanceSample {
	inst := pod.instance
	inst.Sample = *sample
	inst.Accelerators = deviceplugin.AllocatedAcceleratorGroupsOf(pod.pod)
	return inst
}
