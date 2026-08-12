package exporter

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

const (
	// _ComponentLabelKey and _DeviceManagerComponent select this node's device manager pods.
	_ComponentLabelKey      = "app.kubernetes.io/component"
	_DeviceManagerComponent = "device-manager"

	// _ManufacturerLabelKey carries the manufacturer a device manager pod was rolled for. The
	// chart rolls one DaemonSet per manufacturer and stamps this on each.
	_ManufacturerLabelKey = "gpustack.ai/manufacturer"
)

// exporting reports whether this device manager is the one publishing the node's per-Instance
// figures.
//
// A node carrying two vendors runs two device managers, and both see all of its Instances:
// without a rule they would publish the same series from two scrape targets. Deduplicating at
// query time with max by(...) does not work either — the two sample independently, so max is
// biased upward rather than choosing between identical copies.
//
// The rule is that the manufacturer sorting first among the Ready device manager pods of this
// node exports. It is a total order that needs no shared state and no lease: each pod already
// watches its node's pods, and the role hands over by itself as soon as the current holder
// stops being Ready. Two pods can briefly both consider themselves the exporter while readiness
// changes, which duplicates a series across two targets for one period rather than inside one
// gather, and resolves itself.
//
// Accelerator figures are not subject to this: device IDs are disjoint across manufacturers, so
// every device manager publishes its own without ever colliding.
func (p *Poller) exporting(ctx context.Context) (bool, error) {
	podName := osx.Getenv("KUBERNETES_POD_NAME")
	if podName == "" {
		return false, fmt.Errorf("environment variable KUBERNETES_POD_NAME is not set")
	}

	podList := &core.PodList{}
	err := p.reader.List(ctx, podList,
		ctrlcli.InNamespace(osx.Getenv("KUBERNETES_POD_NAMESPACE")),
		ctrlcli.MatchingLabels{_ComponentLabelKey: _DeviceManagerComponent},
		ctrlcli.MatchingFieldsSelector{
			Selector: fields.OneTermEqualSelector(deviceplugin.IndexingPodsByNodeName, p.nodeName),
		},
	)
	if err != nil {
		return false, fmt.Errorf("failed to list device manager pods on node %s: %w", p.nodeName, err)
	}

	// Read this pod's own manufacturer off its label rather than off its --manufacturer flag:
	// the label is what every other pod is compared by, and a process cannot be the exporter
	// under a name its peers do not see it by.
	var mine string
	first := ""
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Name == podName {
			mine = pod.Labels[_ManufacturerLabelKey]
		}
		if pod.DeletionTimestamp != nil || !isPodReady(pod) {
			continue
		}
		// A pod that does not say which manufacturer it serves is not in the running. An empty
		// label sorts before every real one, so counting it as the winner would leave the node
		// with no exporter at all: the labeled pods would defer to it, and it could not
		// recognize itself as the winner either.
		manufacturer := pod.Labels[_ManufacturerLabelKey]
		if manufacturer == "" {
			continue
		}
		if first == "" || manufacturer < first {
			first = manufacturer
		}
	}

	// Not Ready yet, or not in the cache yet: whichever peer is Ready exports until this one is,
	// and a pod that is not Ready is not being scraped anyway.
	return mine != "" && mine == first, nil
}

// isPodReady reports whether the pod carries a true Ready condition.
func isPodReady(pod *core.Pod) bool {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == core.PodReady {
			return pod.Status.Conditions[i].Status == core.ConditionTrue
		}
	}
	return false
}
