package worker

import (
	"cmp"
	"context"
	"slices"

	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

const (
	// modelPlacementPreferenceWeight is the weight of the preferred term naming a digest's nodes. It
	// is the weight the preference was measured with; TAS only compares scores with each other, so a
	// Pod carrying one term is placed the same under any weight, and several terms weigh equally.
	modelPlacementPreferenceWeight = 100

	// modelPlacementMaxNodes bounds the hostnames one term names, so the size a preference adds to a
	// Pod and to its Workload does not grow with the cluster.
	modelPlacementMaxNodes = 16

	// nodeModelStoreConditionReady is the condition the plugin keeps True while it serves its node.
	nodeModelStoreConditionReady = "Ready"
)

// modelPlacementCandidate is a node that holds a digest ready to mount.
type modelPlacementCandidate struct {
	node     string
	hostname string
	model    workercore.NodeModelStoreModel
}

// placementPreference returns the preference for the Pods these weights are mounted by: the nodes
// holding a node-delivered digest ready to mount, or nil for any other delivery, a blocked resolution,
// or a digest no node holds.
//
// It walks every NodeModelStore, so it is computed only by a pass that creates Pods, once for all of
// them: that keeps a reconcile that creates nothing from paying for it, and gives every member of a
// replica the same term, which Kueue needs because it builds the replica's PodSet from one member.
func (w *modelArtifactWeights) placementPreference(ctx context.Context, cli ctrlcli.Reader) *core.PreferredSchedulingTerm {
	if w == nil || w.Blocked || w.Render == nil || w.Render.Delivery != workercore.ModelDeploymentModelDeliveryNode {
		return nil
	}

	return modelPlacementPreference(ctx, cli, w.Render.ManifestDigest)
}

// modelPlacementPreference returns the preferred node-affinity term naming the nodes that hold digest
// ready to mount, or nil when none does.
//
// A PREFERENCE, NEVER A FILTER. Kueue's topology-aware scheduling reads the term as a node score among
// the nodes that already fit the Pod, so a wrong or stale entry costs at most one download on another
// node. For the same reason a read that fails yields no term rather than an error: holding a Pod back
// because its preference could not be computed would put the file location ahead of compute.
//
// A node counts only while three things hold: its NodeModelStore lists the digest Ready, that object's
// Ready condition is True, and its CSINode lists the plugin's driver now. The third is there because
// the object does not say when it went stale: a node the plugin left keeps its object and a Ready
// condition that stops changing, while kubelet drops the driver from CSINode.
//
// The values are the nodes' kubernetes.io/hostname labels, not their names: TAS keys its nodes by that
// label, and some providers set it to something other than the Node's name.
func modelPlacementPreference(ctx context.Context, cli ctrlcli.Reader, digest string) *core.PreferredSchedulingTerm {
	logger := ctrllog.FromContext(ctx)

	stores := new(workercore.NodeModelStoreList)
	if err := cli.List(ctx, stores); err != nil {
		logger.Error(err, "list node model stores for a placement preference; none is added")
		return nil
	}

	var candidates []modelPlacementCandidate
	for i := range stores.Items {
		nms := &stores.Items[i]
		if !nodeModelStoreReady(nms) {
			continue
		}
		j := slices.IndexFunc(nms.Status.Models, func(m workercore.NodeModelStoreModel) bool {
			return m.Digest == digest && m.State == workercore.NodeModelStoreModelStateReady
		})
		if j < 0 {
			continue
		}

		csiNode := new(storage.CSINode)
		if err := cli.Get(ctx, ctrlcli.ObjectKey{Name: nms.Name}, csiNode); err != nil {
			if ctrlcli.IgnoreNotFound(err) != nil {
				logger.Error(err, "read a CSINode for a placement preference; the node is left out", "node", nms.Name)
			}
			continue
		}
		if !csiNodeListsModelDriver(csiNode) {
			continue
		}

		node := new(core.Node)
		if err := cli.Get(ctx, ctrlcli.ObjectKey{Name: nms.Name}, node); err != nil {
			if ctrlcli.IgnoreNotFound(err) != nil {
				logger.Error(err, "read a Node for a placement preference; the node is left out", "node", nms.Name)
			}
			continue
		}
		hostname := node.Labels[core.LabelHostname]
		if hostname == "" {
			continue
		}

		candidates = append(candidates, modelPlacementCandidate{node: nms.Name, hostname: hostname, model: nms.Status.Models[j]})
	}

	hostnames := modelPlacementHostnames(candidates)
	if len(hostnames) == 0 {
		return nil
	}

	return &core.PreferredSchedulingTerm{
		Weight: modelPlacementPreferenceWeight,
		Preference: core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{
			{Key: core.LabelHostname, Operator: core.NodeSelectorOpIn, Values: hostnames},
		}},
	}
}

// modelPlacementHostnames orders candidates and returns the hostnames of the first
// modelPlacementMaxNodes: nodes where a Pod mounts the digest now, then the most recently used, then
// by node name. Nodes serving the digest now are the likeliest to be in the pool of the consumer's
// other replicas, which is what a truncated list should keep. The order depends on nothing but the
// candidates, so the members of one replica built from one snapshot carry the same list.
func modelPlacementHostnames(candidates []modelPlacementCandidate) []string {
	sorted := slices.Clone(candidates)
	slices.SortFunc(sorted, func(a, b modelPlacementCandidate) int {
		if a.model.Referenced != b.model.Referenced {
			if a.model.Referenced {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(lastUsedUnix(b.model), lastUsedUnix(a.model)); c != 0 {
			return c
		}
		return cmp.Compare(a.node, b.node)
	})
	if len(sorted) > modelPlacementMaxNodes {
		sorted = sorted[:modelPlacementMaxNodes]
	}

	hostnames := make([]string, 0, len(sorted))
	for i := range sorted {
		hostnames = append(hostnames, sorted[i].hostname)
	}

	return hostnames
}

func lastUsedUnix(m workercore.NodeModelStoreModel) int64 {
	if m.LastUsedTime == nil {
		return 0
	}

	return m.LastUsedTime.Unix()
}

func nodeModelStoreReady(nms *workercore.NodeModelStore) bool {
	return slices.ContainsFunc(nms.Status.Conditions, func(c gpustack.Condition) bool {
		return c.Type == nodeModelStoreConditionReady && c.Status == meta.ConditionTrue
	})
}

// injectModelPlacementPreference appends term to a Pod's preferred node affinity, keeping every term
// already there.
//
// A Pod asking Kueue for a preferred topology level gets nothing: with TASBalancedPlacement on, as the
// chart has it, such a PodSet was measured to lose its node-affinity score entirely, so the term would
// claim a preference that nothing honors.
func injectModelPlacementPreference(pod *core.Pod, term *core.PreferredSchedulingTerm) {
	if term == nil {
		return
	}
	if _, ok := pod.Annotations[kueue.PodSetPreferredTopologyAnnotation]; ok {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &core.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &core.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	na.PreferredDuringSchedulingIgnoredDuringExecution = append(na.PreferredDuringSchedulingIgnoredDuringExecution, *term.DeepCopy())
}
