package worker

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelstore"
)

// modelArtifactNodesWindow is the least time between two writes of one artifact's status.nodes. A
// fleet of nodes finishing one download changes the counts once per node, and the digest may be
// named by artifacts in many namespaces; the window bounds each artifact's writes whatever the fleet.
const modelArtifactNodesWindow = 30 * time.Second

// modelArtifactNodes is where the artifact's content is across the nodes, or nil when it has none to
// count: a claim, or a hub source not resolved yet.
func modelArtifactNodes(ma *workercore.ModelArtifact, stores []workercore.NodeModelStore) *workercore.ModelArtifactNodes {
	r := ma.Status.Resolved
	if ma.Spec.Source.HuggingFace == nil || r == nil || r.ManifestDigest == "" {
		return nil
	}
	a := modelstore.AggregateEntries(modelstore.NodeEntries(stores, r.ManifestDigest))

	return &workercore.ModelArtifactNodes{
		Ready: a.Ready, Downloading: a.Downloading, Failed: a.Failed, DownloadingPercent: a.SteppedPercent(),
	}
}

// reconcileNodes sets status.nodes from the NodeModelStores, unless the artifact's nodes were written
// less than the window ago: then the stored value stays and the result asks for the pass that
// writes it when the window ends.
func (r *ModelArtifactReconciler) reconcileNodes(
	ctx context.Context, ma *workercore.ModelArtifact, stored *workercore.ModelArtifactNodes,
) ctrl.Result {
	// The stores are read only when the artifact has nodes to count at all: modelArtifactNodes
	// returns nil for a claim or an unresolved hub source, whatever the fleet holds.
	var stores []workercore.NodeModelStore
	if res := ma.Status.Resolved; ma.Spec.Source.HuggingFace != nil && res != nil && res.ManifestDigest != "" {
		list := new(workercore.NodeModelStoreList)
		if err := r.Client.List(ctx, list,
			ctrlcli.MatchingFields{IndexingNodeModelStoreByModelDigest: res.ManifestDigest}); err != nil {
			ctrllog.FromContext(ctx).Error(err, "list node model stores; the artifact's nodes stay as they are")
			ma.Status.Nodes = stored
			return ctrl.Result{RequeueAfter: modelArtifactNodesWindow}
		}
		stores = list.Items
	}
	next := modelArtifactNodes(ma, stores)
	if kubemeta.DeepEqual(next, stored) {
		ma.Status.Nodes = stored
		return ctrl.Result{}
	}
	if last, ok := r.nodesWritten.Load(ma.UID); ok {
		if wait := modelArtifactNodesWindow - r.now().Sub(last.(time.Time)); wait > 0 {
			ma.Status.Nodes = stored
			return ctrl.Result{RequeueAfter: wait}
		}
	}
	ma.Status.Nodes = next

	return ctrl.Result{}
}

// IndexingModelArtifactByManifestDigest indexes ModelArtifacts by the digest their resolution
// bound, so a NodeModelStore event reaches exactly the artifacts naming a digest it holds,
// instead of listing them all per event.
const IndexingModelArtifactByManifestDigest = "modelartifacts.worker.gpustack.ai/manifest-digest"

// indexModelArtifactByManifestDigest extracts the IndexingModelArtifactByManifestDigest key.
func indexModelArtifactByManifestDigest(obj ctrlcli.Object) []string {
	ma, ok := obj.(*workercore.ModelArtifact)
	if !ok {
		return nil
	}
	if res := ma.Status.Resolved; res != nil && res.ManifestDigest != "" {
		return []string{res.ManifestDigest}
	}

	return nil
}

// IndexingNodeModelStoreByModelDigest indexes NodeModelStores by the digests their status holds, so
// an artifact's reconcile reads exactly the stores holding its digest instead of the whole fleet.
const IndexingNodeModelStoreByModelDigest = "nodemodelstores.worker.gpustack.ai/model-digest"

// indexNodeModelStoreByModelDigest extracts the IndexingNodeModelStoreByModelDigest keys.
func indexNodeModelStoreByModelDigest(obj ctrlcli.Object) []string {
	nms, ok := obj.(*workercore.NodeModelStore)
	if !ok {
		return nil
	}
	digests := make([]string, 0, len(nms.Status.Models))
	for _, m := range nms.Status.Models {
		digests = append(digests, m.Digest)
	}

	return digests
}

// mapModelArtifactNodeModelStore maps a NodeModelStore to the artifacts naming a digest it holds,
// in every namespace. Each List is a lookup on the artifact's digest index; the watch wraps this in
// the same second-scale dedup window the controller's other watches use.
func (r *ModelArtifactReconciler) mapModelArtifactNodeModelStore(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	nms, ok := obj.(*workercore.NodeModelStore)
	if !ok {
		return nil
	}
	var requests []ctrlreconcile.Request
	for _, m := range nms.Status.Models {
		mas := new(workercore.ModelArtifactList)
		if err := r.Client.List(ctx, mas, ctrlcli.MatchingFields{IndexingModelArtifactByManifestDigest: m.Digest}); err != nil {
			ctrllog.FromContext(ctx).Error(err, "list model artifacts for a node model store")
			continue
		}
		for i := range mas.Items {
			requests = append(requests, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&mas.Items[i])})
		}
	}

	return requests
}
