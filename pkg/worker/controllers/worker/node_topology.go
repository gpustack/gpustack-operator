package worker

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/ctrlhandlerx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const (
	// TopologyProfileLabel partitions Nodes by their ordered topology label-key set.
	TopologyProfileLabel = "topology.gpustack.ai/profile"

	topologyNamePrefix    = "gpustack-"
	topologyProfilePrefix = "fnv64-"
	topologyResourceType  = "topology"
	topographFabricPrefix = "fabric.topograph.run/tier-"
)

// NodeTopologyReconciler assigns topology profiles and materializes their Kueue Topologies.
type NodeTopologyReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*NodeTopologyReconciler)(nil)

func (r *NodeTopologyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := new(core.Node)
	if err := r.Client.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	if node.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	levels, err := r.resolveNodeTopologyLevels(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	profile := topologyProfile(levels)
	if err := r.ensureTopology(ctx, profile, levels); err != nil {
		return ctrl.Result{}, err
	}
	if node.Labels[TopologyProfileLabel] == profile {
		return ctrl.Result{}, nil
	}
	updated := node.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = make(map[string]string)
	}
	updated.Labels[TopologyProfileLabel] = profile
	return ctrl.Result{}, r.Client.Update(ctx, updated)
}

func (r *NodeTopologyReconciler) resolveNodeTopologyLevels(ctx context.Context, node *core.Node) ([]string, error) {
	sources := new(workercore.TopologySourceList)
	if err := r.Client.List(ctx, sources); err != nil {
		return nil, err
	}
	var matched []string
	matches := 0
	for i := range sources.Items {
		source := &sources.Items[i]
		if !TopologySourceConditionReady.IsTrue(source) {
			continue
		}
		selector, err := meta.LabelSelectorAsSelector(&source.Spec.NodeSelector)
		if err != nil {
			return nil, err
		}
		if !selector.Matches(labels.Set(node.Labels)) {
			continue
		}
		matches++
		matched = topologyPrefix(node.Labels, normalizeTopologyLevels(source.Spec.Levels))
	}
	if matches != 1 {
		matched = nil
	}
	return append(matched, core.LabelHostname), nil
}

func (r *NodeTopologyReconciler) ensureTopology(ctx context.Context, profile string, levels []string) error {
	wanted := &kueue.Topology{
		ObjectMeta: meta.ObjectMeta{Name: topologyName(profile)},
		Spec:       kueue.TopologySpec{Levels: topologyLevelObjects(levels)},
	}
	systemmeta.NoteResource(wanted, topologyResourceType, nil)
	actual := new(kueue.Topology)
	err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(wanted), actual)
	if ctrlcli.IgnoreNotFound(err) != nil {
		return err
	}
	if err != nil {
		return r.Client.Create(ctx, wanted)
	}
	if !kubemeta.DeepEqual(actual.Spec, wanted.Spec) {
		return fmt.Errorf("managed Topology %q has immutable level drift", actual.Name)
	}
	return nil
}

func topologyProfile(levels []string) string {
	return topologyProfilePrefix + stringx.SumByFNV64a(strings.Join(levels, "\x00"))
}

func topologyName(profile string) string {
	return topologyNamePrefix + profile
}

func topologyLevelObjects(levels []string) []kueue.TopologyLevel {
	result := make([]kueue.TopologyLevel, len(levels))
	for i, level := range levels {
		result[i] = kueue.TopologyLevel{NodeLabel: level}
	}
	return result
}

func topologyPrefix(nodeLabels map[string]string, levels []string) []string {
	result := make([]string, 0, len(levels))
	for _, level := range levels {
		if nodeLabels[level] == "" {
			break
		}
		result = append(result, level)
	}
	return result
}

func normalizeTopologyLevels(levels []string) []string {
	result := slices.Clone(levels)
	for start := 0; start < len(result); {
		if _, found := topographFabricTier(result[start]); !found {
			start++
			continue
		}
		end := start + 1
		for end < len(result) {
			if _, found := topographFabricTier(result[end]); !found {
				break
			}
			end++
		}
		first, _ := topographFabricTier(result[start])
		last, _ := topographFabricTier(result[end-1])
		if first < last {
			slices.Reverse(result[start:end])
		}
		start = end
	}
	return result
}

func topographFabricTier(level string) (int, bool) {
	if !strings.HasPrefix(level, topographFabricPrefix) {
		return 0, false
	}
	tier, err := strconv.Atoi(strings.TrimPrefix(level, topographFabricPrefix))
	return tier, err == nil
}

func validateTopologyNodeHierarchy(levels []string, nodes []core.Node) error {
	parents := make(map[string]map[string]string)
	for i := range nodes {
		node := &nodes[i]
		missingParent := false
		for levelIndex, level := range levels {
			value := node.Labels[level]
			if value == "" {
				missingParent = true
				continue
			}
			if missingParent {
				return fmt.Errorf("node %q has level %q without its parent", node.Name, level)
			}
			if levelIndex == 0 {
				continue
			}
			parentValues := make([]string, levelIndex)
			for parentIndex := range levelIndex {
				parentValues[parentIndex] = node.Labels[levels[parentIndex]]
			}
			byValue := parents[level]
			if byValue == nil {
				byValue = make(map[string]string)
				parents[level] = byValue
			}
			parentTuple := strings.Join(parentValues, "\x00")
			if existing, found := byValue[value]; found && existing != parentTuple {
				return fmt.Errorf("level %q value %q has conflicting parent tuples", level, value)
			}
			byValue[value] = parentTuple
		}
	}
	return nil
}

func (r *NodeTopologyReconciler) enqueueAllNodes(ctx context.Context, _ ctrlcli.Object) []ctrlreconcile.Request {
	nodes := new(core.NodeList)
	if err := r.Client.List(ctx, nodes); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list Nodes for topology change")
		return nil
	}
	requests := make([]ctrlreconcile.Request, len(nodes.Items))
	for i := range nodes.Items {
		requests[i] = ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&nodes.Items[i])}
	}
	return requests
}

func (r *NodeTopologyReconciler) enqueueNodesForTopology(ctx context.Context, topology ctrlcli.Object) []ctrlreconcile.Request {
	profile := strings.TrimPrefix(topology.GetName(), topologyNamePrefix)
	if profile == topology.GetName() {
		return nil
	}
	nodes := new(core.NodeList)
	if err := r.Client.List(ctx, nodes, ctrlcli.MatchingLabels{TopologyProfileLabel: profile}); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list Nodes for managed Topology change")
		return nil
	}
	requests := make([]ctrlreconcile.Request, len(nodes.Items))
	for i := range nodes.Items {
		requests[i] = ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&nodes.Items[i])}
	}
	return requests
}

func (r *NodeTopologyReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("nodetopology").
		For(
			&core.Node{},
			ctrlbuilder.WithPredicates(ctrlpredicate.Funcs{
				DeleteFunc: func(ctrlevent.DeleteEvent) bool { return false },
				UpdateFunc: func(event ctrlevent.UpdateEvent) bool {
					oldNode, newNode := event.ObjectOld.(*core.Node), event.ObjectNew.(*core.Node)
					return !kubemeta.DeepEqual(oldNode.Labels, newNode.Labels)
				},
			}),
		).
		Watches(
			&workercore.TopologySource{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueAllNodes),
		).
		Watches(
			&kueue.Topology{},
			ctrlhandlerx.DedupEnqueueRequestsFromMapFunc(time.Second, r.enqueueNodesForTopology),
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
				return systemmeta.MatchResource(obj, topologyResourceType)
			})),
		).
		Complete(r)
}
