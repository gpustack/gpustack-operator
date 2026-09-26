package worker

import (
	"context"
	"time"

	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
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

// nodeModelStoreView is what an artifact's status.nodes reads from a NodeModelStore: each digest's
// state and its progress step. Progress inside a step changes nothing the artifact shows.
func nodeModelStoreView(nms *workercore.NodeModelStore) map[string][2]int64 {
	view := make(map[string][2]int64, len(nms.Status.Models))
	for _, m := range nms.Status.Models {
		var step int64
		if m.State == workercore.NodeModelStoreModelStateDownloading && m.SizeBytes > 0 {
			step = m.DownloadedBytes * 100 / m.SizeBytes / 5
		}
		state := map[workercore.NodeModelStoreModelState]int64{
			workercore.NodeModelStoreModelStateReady: 1, workercore.NodeModelStoreModelStateDownloading: 2,
			workercore.NodeModelStoreModelStateFailed: 3,
		}[m.State]
		view[m.Digest] = [2]int64{state, step}
	}

	return view
}

// changedDigests are the digests whose state or progress step differ between two views, entries that
// appeared or went included.
func changedDigests(oldView, newView map[string][2]int64) map[string]bool {
	changed := map[string]bool{}
	for d, v := range newView {
		if o, ok := oldView[d]; !ok || o != v {
			changed[d] = true
		}
	}
	for d := range oldView {
		if _, ok := newView[d]; !ok {
			changed[d] = true
		}
	}

	return changed
}

// IndexingModelArtifactByManifestDigest indexes ModelArtifacts by the digest their resolution
// bound, so a NodeModelStore event reaches exactly the artifacts naming a digest it changed,
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

// nodeModelStoreHandler enqueues the artifacts naming a digest whose view a NodeModelStore event
// changed, in every namespace.
func (r *ModelArtifactReconciler) nodeModelStoreHandler() ctrlhandler.EventHandler {
	add := func(ctx context.Context, digests map[string]bool, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
		for digest := range digests {
			mas := new(workercore.ModelArtifactList)
			if err := r.Client.List(ctx, mas, ctrlcli.MatchingFields{IndexingModelArtifactByManifestDigest: digest}); err != nil {
				ctrllog.FromContext(ctx).Error(err, "list model artifacts for a node model store")
				continue
			}
			for i := range mas.Items {
				q.Add(ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&mas.Items[i])})
			}
		}
	}
	view := func(obj ctrlcli.Object) map[string][2]int64 {
		if nms, ok := obj.(*workercore.NodeModelStore); ok {
			return nodeModelStoreView(nms)
		}
		return nil
	}

	return ctrlhandler.Funcs{
		CreateFunc: func(ctx context.Context, e ctrlevent.CreateEvent, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			add(ctx, changedDigests(nil, view(e.Object)), q)
		},
		UpdateFunc: func(ctx context.Context, e ctrlevent.UpdateEvent, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			add(ctx, changedDigests(view(e.ObjectOld), view(e.ObjectNew)), q)
		},
		DeleteFunc: func(ctx context.Context, e ctrlevent.DeleteEvent, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			add(ctx, changedDigests(view(e.Object), nil), q)
		},
	}
}
