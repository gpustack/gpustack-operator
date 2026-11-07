package modelstore

import (
	"context"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// BindingCoversNode reports whether the binding's grant reaches the node: the binding is live,
// and at least one of the live ModelStores its storeRefs name selects the node by its
// spec.NodeSelector. It is the coverage half of admission's granted check, shared so the
// upcoming source IntentSource and any other caller ask the grant the same question the
// admission webhook asks, instead of re-deriving it.
//
// The refusals are deliberate: a missing store covers nothing rather than fabricating a grant
// (the Binding's own admission refuses the ghost); a store being deleted covers nothing; an
// unparsable store selector covers nothing rather than matching everything; and an API error is
// returned, never read as an uncovered answer — a transient read failure on an enforcement
// question must not look like "no".
func BindingCoversNode(
	ctx context.Context, reader ctrlcli.Reader, binding *workercore.ModelStoreBinding, node *core.Node,
) (bool, error) {
	if node == nil {
		return false, nil
	}
	covered, err := BindingCoversNodes(ctx, reader, binding, []core.Node{*node})
	return covered[node.Name], err
}

// BindingCoversNodes evaluates one binding against a node snapshot, reading each
// referenced ModelStore at most once. No observations survive this call; errors
// are returned without publishing a partial coverage result.
func BindingCoversNodes(
	ctx context.Context, reader ctrlcli.Reader, binding *workercore.ModelStoreBinding, nodes []core.Node,
) (map[string]bool, error) {
	covered := make(map[string]bool, len(nodes))
	if binding == nil || binding.DeletionTimestamp != nil || len(nodes) == 0 {
		return covered, nil
	}
	remaining := make(map[string]labels.Set, len(nodes))
	for i := range nodes {
		remaining[nodes[i].Name] = labels.Set(nodes[i].Labels)
	}
	seen := make(map[string]bool, len(binding.Spec.StoreRefs))
	for _, ref := range binding.Spec.StoreRefs {
		if seen[ref.Name] {
			continue
		}
		seen[ref.Name] = true
		store := new(workercore.ModelStore)
		err := reader.Get(ctx, ctrlcli.ObjectKey{Name: ref.Name}, store)
		if kerrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if store.DeletionTimestamp != nil {
			continue
		}
		sel, err := meta.LabelSelectorAsSelector(&store.Spec.NodeSelector)
		if err != nil {
			continue
		}
		for name, nodeLabels := range remaining {
			if sel.Matches(nodeLabels) {
				covered[name] = true
				delete(remaining, name)
			}
		}
		if len(remaining) == 0 {
			break
		}
	}

	return covered, nil
}
