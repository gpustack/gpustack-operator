package authorization

import (
	"sort"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// BindingCandidate is one cached Binding that could grant source access on
// a node, named by its immutable identity. It is an observation to verify,
// not a verdict: the caller must still prove the Binding and its stores
// live before admitting anything on it.
type BindingCandidate struct {
	Namespace string
	Name      string
	UID       types.UID
}

// Coverage selects the Bindings whose grant could cover a node out of the
// watch caches, so a metadata admission reads only the Bindings it must
// prove live instead of listing every Binding and reading every StoreRef in
// the caller's namespace. The selection is only a nomination: a stale cache
// may still nominate a Binding whose grant is already gone, and may miss a
// Binding created since its last sync, so an empty selection denies and no
// positive verdict ever rests on cached state — the caller re-reads every
// selected Binding and its stores live before admitting. Until both
// informers complete their initial sync the selection is empty and callers
// read the namespace live.
type Coverage struct {
	bindings cache.SharedIndexInformer
	stores   cache.SharedIndexInformer
}

// NewCoverage selects candidates from the Binding and ModelStore caches.
func NewCoverage(bindings, stores cache.SharedIndexInformer) *Coverage {
	return &Coverage{bindings: bindings, stores: stores}
}

// Ready reports whether both caches finished their initial sync. A nil
// Coverage is never ready, which keeps every read live.
func (c *Coverage) Ready() bool {
	return c != nil && c.bindings.HasSynced() && c.stores.HasSynced()
}

// Candidates returns the caller namespace's cached Bindings whose stores
// would select the node's labels, in name order. A Binding being deleted, a
// store missing from the cache, or an unparsable selector is not nominated;
// the result is still only a nomination, and the caller proves each
// candidate live before admitting.
func (c *Coverage) Candidates(namespace string, nodeLabels map[string]string) []BindingCandidate {
	if !c.Ready() {
		return nil
	}
	items, err := c.bindings.GetIndexer().ByIndex(cache.NamespaceIndex, namespace)
	if err != nil {
		return nil
	}
	matching := labels.Set(nodeLabels)
	var out []BindingCandidate
	for _, item := range items {
		binding, ok := item.(*workercore.ModelStoreBinding)
		if !ok || binding.UID == "" || binding.DeletionTimestamp != nil {
			continue
		}
		if c.coversNode(binding, matching) {
			out = append(out, BindingCandidate{
				Namespace: binding.Namespace, Name: binding.Name, UID: binding.UID,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// coversNode reports whether any cached ModelStore the Binding names would
// select the node. A ref whose store is absent from the cache selects
// nothing rather than trusting a half-observed grant.
func (c *Coverage) coversNode(binding *workercore.ModelStoreBinding, nodeLabels labels.Set) bool {
	for _, ref := range binding.Spec.StoreRefs {
		raw, exists, err := c.stores.GetStore().GetByKey(ref.Name)
		if err != nil || !exists {
			continue
		}
		store, ok := raw.(*workercore.ModelStore)
		if !ok || store.DeletionTimestamp != nil {
			continue
		}
		selector, err := meta.LabelSelectorAsSelector(&store.Spec.NodeSelector)
		if err != nil {
			continue
		}
		if selector.Matches(nodeLabels) {
			return true
		}
	}
	return false
}
