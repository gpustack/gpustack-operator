package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// modelPrefetchPodsPerPass bounds how many warm-up Pods one reconcile creates, so a prefetch over
// a large pool lands as a sequence of bounded writes rather than one burst on the API server.
const modelPrefetchPodsPerPass = 8

// Conditions of a ModelPrefetch's status.
const (
	// ModelPrefetchConditionProgressing says delivery is under way: the artifact is resolved and
	// some target nodes do not hold the content yet.
	ModelPrefetchConditionProgressing = "Progressing"
	// ModelPrefetchConditionAvailable says enough target nodes hold the content: ReadyNodes
	// reached the MinReady bar, where MinReady 0 means all of them.
	ModelPrefetchConditionAvailable = "Available"
	// ModelPrefetchConditionDegraded says a target node gave up; the message names the reasons.
	ModelPrefetchConditionDegraded = "Degraded"
	// ModelPrefetchConditionLapsed says the MinReady shortfall is the retention TTL's doing: the
	// copies that would close the gap lapsed past TTLAfterLastUse, stopped counting as ready and
	// were unpinned, and nothing re-warms them. It stays False while the shortfall has any other
	// cause, so a lapse never masks a warm-up still running or a delivery that failed.
	ModelPrefetchConditionLapsed = "Lapsed"
	// ModelPrefetchConditionSourceReady says the current artifact has a serving source.
	ModelPrefetchConditionSourceReady = "SourceReady"
)

// modelTransferSourcePodLabel identifies legacy inert source-holder Pods for migration.
const modelTransferSourcePodLabel = "worker.gpustack.ai/model-transfer-source"

// ModelPrefetchReconciler turns a prefetch's residency intent into one warm-up Pod per target
// node, reads the delivery's progress from the target nodes' own reports, and pins what its
// retention asks to keep.
//
// The warm-up Pod IS the delivery operation: it mounts the artifact's volume the consumer path
// mounts, so the node's cache does the download, the verification and the publishing exactly once
// per node; the Pod reads the tree back and exits, dropping its reference. It never renders a
// queue-name label, so neither Kueue nor the operator's Pod webhook has anything to say about it.
type ModelPrefetchReconciler struct {
	Client ctrlcli.Client
	Reader ctrlcli.Reader
	Now    func() time.Time
}

var _ ctrlreconcile.Reconciler = (*ModelPrefetchReconciler)(nil)

func (r *ModelPrefetchReconciler) reader() ctrlcli.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

func (r *ModelPrefetchReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

func (r *ModelPrefetchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	pf := new(workercore.ModelPrefetch)
	switch err := r.reader().Get(ctx, req.NamespacedName, pf); {
	case kerrors.IsNotFound(err):
		// A deleted prefetch is gone from the list, so the union recompute is what releases its
		// pins; running here covers the case where the deleted one was the only prefetch.
		return ctrl.Result{}, r.recomputePinned(ctx)
	case err != nil:
		return ctrl.Result{}, err
	}

	if pf.DeletionTimestamp != nil {
		// The warm-up Pods go with the object through their owner references; the pins leave with
		// the union recompute, which no longer sees this prefetch.
		return ctrl.Result{}, r.recomputePinned(ctx)
	}

	if err := r.removeLegacySourceHolders(ctx, pf); err != nil {
		return ctrl.Result{}, err
	}

	// The target set comes first: it is computable before the artifact resolves, and the status's
	// desired count is its size.
	nodes := new(core.NodeList)
	if err := r.Client.List(ctx, nodes); err != nil {
		return ctrl.Result{}, err
	}
	deps := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, deps, ctrlcli.InNamespace(pf.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	pools, err := ResolveInstanceTypeFlavorNames(ctx, r.Client, pf, deps.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	targets, err := prefetchTargetNodes(pf, nodes.Items, deps.Items, pools)
	if err != nil {
		// An unparseable selector is a spec the API server's schema should have refused; report it
		// as degraded and wait for the next generation. No source observation exists without a
		// target set, so the transfer view stays off.
		logger.Error(err, "the prefetch's placement does not parse")
		before := pf.Status.DeepCopy()
		r.writeStatus(pf, prefetchProgress{}, err, false, nil, modelPrefetchFromRemote(ctx))
		return ctrl.Result{}, r.commitStatus(ctx, pf, before)
	}

	artifact := new(workercore.ModelArtifact)
	artifactErr := r.reader().Get(ctx,
		ctrlcli.ObjectKey{Namespace: pf.Namespace, Name: pf.Spec.ArtifactRef.Name}, artifact)
	artifactMissing := kerrors.IsNotFound(artifactErr)
	if artifactErr != nil && !artifactMissing {
		return ctrl.Result{}, artifactErr
	}
	digest := ""
	if artifactErr == nil && artifact.Status.Resolved != nil {
		digest = artifact.Status.Resolved.ManifestDigest
	}
	artifactReady := digest != ""

	// The delivery's facts are the target nodes' own entries for the digest, read through the
	// store aggregates this controller shares with the artifact's nodes view.
	stores := new(workercore.NodeModelStoreList)
	if err := r.Client.List(ctx, stores); err != nil {
		return ctrl.Result{}, err
	}
	entries := modelstore.NodeEntries(stores.Items, digest)

	// A source intent adds the registered-memory half: the grant's reach, the admitted
	// generations' health and configured images. Its observations run over the target
	// nodes only — a generation on a node this prefetch no longer names is another prefetch's
	// business, not this status's.
	var source *sourceObservation
	if pf.Spec.Source != nil || pf.Status.Transfer != nil {
		workers, sets, planErr := r.readSourceWorkerPlan(ctx, nodes.Items, targets)
		if planErr != nil {
			return ctrl.Result{}, planErr
		}
		source, err = r.observeSource(ctx, pf, artifact, artifactMissing, digest, targets, nodes.Items, stores.Items, workers, sets)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	progress := prefetchProgress{desired: int32(len(targets))}
	expired := map[string]bool{}
	for _, node := range targets {
		switch entry, ok := entries[node]; {
		case !ok || entry.State == workercore.NodeModelStoreModelStateDownloading:
			progress.downloading++
		case entry.State == workercore.NodeModelStoreModelStateFailed:
			progress.failed++
		default: // Ready, unless its retention TTL ran out on this node.
			if r.ttlExpired(pf, entry) {
				expired[node] = true
				progress.lapsed++
				continue
			}
			progress.ready = append(progress.ready, node)
		}
	}

	// The warm-up Pods exist only for a resolved artifact: until then every target node is waiting
	// on the resolution, not on a Pod.
	var requeue ctrl.Result
	if artifactReady {
		image, err := r.warmupImage(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue, err = r.deliver(ctx, pf, artifact, digest, image, targets, progress.ready, entries, expired)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	before := pf.Status.DeepCopy()
	r.writeStatus(pf, progress, err, artifactMissing, source, modelPrefetchFromRemote(ctx))
	if err := r.commitStatus(ctx, pf, before); err != nil {
		return ctrl.Result{}, err
	}

	// The pins are recomputed as a whole after every pass, so a prefetch that lapsed, was retargeted
	// or was deleted leaves no pin behind and never drops a pin another prefetch still wants.
	if err := r.recomputePinned(ctx); err != nil {
		return ctrl.Result{}, err
	}

	if pf.Spec.Source != nil || pf.Status.Transfer != nil {
		// Refresh at the first reported lease expiry, bounded by the retry interval.
		delay := 10 * time.Second
		if source != nil && source.nextLease > 0 && source.nextLease < delay {
			delay = source.nextLease
		}
		if requeue.Requeue || (requeue.RequeueAfter > 0 && requeue.RequeueAfter < delay) {
			return requeue, nil
		}
		return ctrl.Result{RequeueAfter: delay}, nil
	}
	if pf.Spec.Retention.TTLAfterLastUse != nil {
		// TTL expiry is hourly by the node's own report; an hourly look keeps it enforced without
		// pretending to be finer than the facts.
		return requeueOr(requeue, ctrl.Result{RequeueAfter: time.Hour}), nil
	}

	return requeueOr(requeue, ctrl.Result{}), nil
}

// prefetchProgress is one pass's reading of the delivery, the raw material of the status.
type prefetchProgress struct {
	desired     int32
	ready       []string
	downloading int32
	failed      int32
	// lapsed counts the target nodes whose copy is Ready but past the retention TTL: they stopped
	// counting as ready and their pins are released, and no warm-up Pod re-warms them.
	lapsed int32
}

// modelPrefetchFromRemote is the cluster's remote delivery policy as a
// ModelPrefetch's reconcile reads it. It is a variable so a test can choose a
// policy without a Settings store.
var modelPrefetchFromRemote = func(ctx context.Context) bool {
	return settings.ModelArtifactRemoteDeliveryPolicy.ShouldValueFromRemote(ctx) ==
		settings.ModelArtifactRemoteDeliveryRequire
}

// sourceObservation is one pass's reading of a prefetch's source half: the grant's reach, what
// the admission has to say, and the target nodes' generation reports. A refusal — no grant, no
// placement, an unsized artifact, or a residency window under the floor — keeps the disk delivery
// running and reports no transfer view.
type sourceObservation struct {
	grant *workercore.ModelStoreBindingSourceMemory

	missingGrant     bool
	placementMissing bool
	unsized          bool
	belowFloor       bool
	authorityUnknown bool

	// imageMissing says a target node has no configured source image.
	imageMissing bool
	nextLease    time.Duration

	// admitted says the pinned authority holds a reservation for this source intent.
	// charge is the namespace's own reservation; live generation reports separately
	// establish whether any source is ready to serve.
	admitted bool
	charge   int64
	// generations are the target nodes' reports, aggregated per generation in name order.
	generations []workercore.ModelPrefetchTransferGeneration
	serving     *workercore.ModelPrefetchTransferServing
}

// observeSource reads the source half through the identities it already has: the Binding grants,
// the artifact sizes, and the target nodes' own model-transfer reports. Nothing here writes.
func (r *ModelPrefetchReconciler) observeSource(ctx context.Context, pf *workercore.ModelPrefetch,
	artifact *workercore.ModelArtifact, artifactMissing bool, digest string, targets []string,
	nodes []core.Node, stores []workercore.NodeModelStore, workers sourceWorkerPlan, sets []apps.DaemonSet,
) (*sourceObservation, error) {
	obs := &sourceObservation{}

	// A source names its target set; the derived-from-deployments default an ordinary prefetch
	// keeps is not available to it, so a missing placement is a refusal, not an expansion.
	placement := pf.Spec.Placement
	if placement == nil || (len(placement.InstanceTypes) == 0 && placement.NodeSelector == nil) {
		obs.placementMissing = true
	}

	binding := new(workercore.ModelStoreBinding)
	switch err := r.reader().Get(ctx,
		ctrlcli.ObjectKey{Namespace: pf.Namespace, Name: pf.Spec.BindingRef.Name}, binding); {
	case kerrors.IsNotFound(err):
		obs.missingGrant = true
	case err != nil:
		return nil, err
	default:
		// A Binding without a sourceMemory section is a valid shape: the
		// node's own modelTransfer.source.* capacity governs what its
		// sources draw, so a missing section is not a missing grant.
		obs.grant = binding.Spec.SourceMemory
	}
	if err := r.observeSourceCharges(ctx, pf, obs); err != nil {
		return nil, err
	}
	if pf.Spec.Source == nil {
		// A prefetch that no longer requests a source has no source intent:
		// its placement, grant or authority refusals are not its refusals.
		// The retained charges stay observed; an authority that cannot be
		// read only governs a source that exists.
		obs.placementMissing, obs.missingGrant, obs.authorityUnknown = false, false, false
	}
	if pf.Spec.Source == nil || obs.placementMissing || obs.missingGrant || obs.authorityUnknown {
		return obs, nil
	}

	// An unresolved or unsized artifact authorizes no allocation: a stale zero-size figure must
	// never become a zero-cost Pod.
	if artifactMissing || artifact == nil || artifact.Status.Resolved == nil ||
		artifact.Status.Resolved.ManifestDigest == "" || artifact.Status.Resolved.SizeBytes <= 0 ||
		artifact.Status.ObservedGeneration != artifact.Generation {
		obs.unsized = true

		return obs, nil
	}
	size := artifact.Status.Resolved.SizeBytes
	// The floor check is a Binding-grant check: without a sourceMemory
	// section there is no grant figure to check the plan against, and the
	// node's own broker-side ceiling and floor decide at fill time.
	if obs.grant != nil && obs.grant.Bytes.Sign() > 0 {
		_, err := modelstore.PlanModelTransferSourceMemory(size, obs.grant.Bytes.Value())
		switch {
		case errors.Is(err, modelstore.ErrSourceWindowBelowFloor):
			// The grant's headroom cannot back even the minimum source window. The
			// policy decides: a Prefer deployment falls back to Node-to-Node Peer
			// Sync or the local CSI disk load, and a Require deployment keeps
			// waiting for the source node to free capacity.
			obs.belowFloor = true

			return obs, nil
		case err != nil:
			return nil, err
		}
	}

	for _, node := range targets {
		if _, _, configured := workers.Resolve(node); !configured {
			obs.imageMissing = true
		}
	}
	entries, err := broker.ReadPinnedAuthority(ctx, r.reader(), setting.DelegatedSecretNamespace, binding)
	if err != nil || pf.UID == "" || pf.Generation < 1 || artifact.UID == "" || artifact.Generation < 1 {
		obs.authorityUnknown = true
		return obs, nil //nolint:nilerr // an unreadable authority is recorded on obs.authorityUnknown rather than returned as an error
	}
	targetSet := make(map[string]bool, len(targets))
	for _, node := range targets {
		targetSet[node] = true
	}
	own := map[string]broker.ReservationEntry{}
	for _, entry := range entries {
		if entry.IntentUID != string(pf.UID) || entry.IntentGeneration != pf.Generation || entry.Namespace != pf.Namespace {
			continue
		}
		if entry.NodeName == "" || entry.ArtifactUID == "" || entry.SourceGeneration < 1 || entry.SourcePodUID == "" {
			obs.authorityUnknown = true
			return obs, nil
		}
		if !targetSet[entry.NodeName] || entry.ArtifactUID != string(artifact.UID) ||
			entry.SourceGeneration != artifact.Generation || entry.Digest != digest {
			continue
		}
		if _, duplicate := own[entry.Generation]; duplicate {
			obs.authorityUnknown = true
			return obs, nil
		}
		own[entry.Generation] = entry
	}
	if len(own) == 0 {
		return obs, nil
	}
	pods, err := r.sourceRuntimePods(ctx, nodes, targets, workers, sets)
	if err != nil {
		obs.authorityUnknown = true
		return obs, nil //nolint:nilerr // an unreadable runtime fleet is recorded on obs.authorityUnknown rather than returned as an error
	}
	qualified := make([]workercore.NodeModelStore, 0, len(stores))
	coverageNodes := make([]core.Node, 0, len(stores))
	nodeByName := make(map[string]*core.Node, len(nodes))
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}
	for i := range stores {
		if !targetSet[stores[i].Name] || stores[i].Status.ModelTransfer == nil {
			continue
		}
		if node := nodeByName[stores[i].Name]; node != nil {
			coverageNodes = append(coverageNodes, *node)
		}
	}
	var coverageReader ctrlcli.Reader = r.Client
	if coverageReader == nil {
		coverageReader = r.reader()
	}
	coverage, err := modelstore.BindingCoversNodes(ctx, coverageReader, binding, coverageNodes)
	if err != nil {
		return nil, err
	}
	for i := range stores {
		store := stores[i]
		if !coverage[store.Name] {
			continue
		}
		filtered := store.DeepCopy()
		filtered.Status.ModelTransfer.Generations = nil
		for _, generation := range store.Status.ModelTransfer.Generations {
			entry, ok := own[generation.Generation]
			if !ok || entry.NodeName != store.Name || entry.Digest != generation.Digest || entry.State != broker.ReservationStateAdmitted {
				continue
			}
			if generation.SourcePodUID != entry.SourcePodUID || generation.SourceGeneration != artifact.Generation ||
				generation.SourceInstanceID == "" || generation.LeaseExpiresAt == nil ||
				!generation.LeaseExpiresAt.After(r.now()) {
				continue
			}
			pod := pods[generation.SourcePodUID]
			if pod == nil || pod.Spec.NodeName != store.Name ||
				pod.Labels["gpustack.ai/manufacturer"] != entry.NodeScope ||
				pod.Annotations[transferWorkerInstance] != generation.SourceInstanceID {
				continue
			}
			if generation.State == workercore.NodeModelStoreModelTransferGenerationStateServing &&
				artifact.Status.ObservedGeneration == artifact.Generation {
				if obs.serving == nil || generation.LeaseExpiresAt.Before(&obs.serving.ReadyLeaseExpiresAt) {
					obs.serving = &workercore.ModelPrefetchTransferServing{
						ArtifactUID: string(artifact.UID), ArtifactGeneration: artifact.Generation,
						Digest: digest, ReadyLeaseExpiresAt: *generation.LeaseExpiresAt,
					}
				}
			}
			if remaining := generation.LeaseExpiresAt.Sub(r.now()); obs.nextLease == 0 || remaining < obs.nextLease {
				obs.nextLease = remaining
			}
			filtered.Status.ModelTransfer.Generations = append(filtered.Status.ModelTransfer.Generations, generation)
		}
		qualified = append(qualified, *filtered)
	}
	for _, agg := range modelstore.AggregateModelTransferGenerations(qualified, digest) {
		obs.generations = append(obs.generations, workercore.ModelPrefetchTransferGeneration{
			Generation: agg.Generation, ReadyNodes: agg.ReadyNodes, DrainingNodes: agg.DrainingNodes,
			ResidentBytes: agg.ResidentBytes, Registrations: agg.Registrations,
			ActiveSessions: agg.ActiveSessions, RejectedAdmissions: agg.RejectedAdmissions,
		})
	}

	return obs, nil
}

// sourceRuntimePods returns only Pods owned by an unambiguous chart fleet with the configured image.
// Pod state is not a serving signal; it merely binds a separately reported native lease to its worker.
func (r *ModelPrefetchReconciler) sourceRuntimePods(ctx context.Context, nodes []core.Node, targets []string,
	workers sourceWorkerPlan, sets []apps.DaemonSet,
) (map[string]*core.Pod, error) {
	byVendor := map[string][]*apps.DaemonSet{}
	for i := range sets {
		ds := &sets[i]
		if ds.UID == "" || ds.DeletionTimestamp != nil || ds.Labels["app.kubernetes.io/managed-by"] != "Helm" ||
			ds.Labels["app.kubernetes.io/instance"] == "" || ds.Spec.UpdateStrategy.Type != apps.OnDeleteDaemonSetStrategyType {
			continue
		}
		byVendor[ds.Labels["gpustack.ai/manufacturer"]] = append(byVendor[ds.Labels["gpustack.ai/manufacturer"]], ds)
	}
	vendors := map[string]string{}
	for i := range nodes {
		vendors[nodes[i].Name] = modelstore.NodeSourceManufacturer(&nodes[i])
	}
	target := map[string]bool{}
	for _, name := range targets {
		target[name] = true
	}
	list := new(core.PodList)
	if err := r.reader().List(ctx, list, ctrlcli.InNamespace(setting.DelegatedSecretNamespace),
		ctrlcli.MatchingLabels{"app.kubernetes.io/component": "model-transfer"}); err != nil {
		return nil, err
	}
	result := map[string]*core.Pod{}
	for i := range list.Items {
		p := &list.Items[i]
		vendor := vendors[p.Spec.NodeName]
		image, _, configured := workers.Resolve(p.Spec.NodeName)
		sets := byVendor[vendor]
		if !target[p.Spec.NodeName] || !configured || len(sets) != 1 || p.UID == "" ||
			p.DeletionTimestamp != nil || p.Status.Phase == core.PodFailed || p.Status.Phase == core.PodSucceeded ||
			p.Labels["gpustack.ai/manufacturer"] != vendor || p.Annotations[transferDrainFor] != "" {
			continue
		}
		owner := meta.GetControllerOf(p)
		if owner == nil || owner.Kind != "DaemonSet" || owner.UID != sets[0].UID {
			continue
		}
		index := slices.IndexFunc(p.Spec.Containers, func(c core.Container) bool { return c.Name == "worker" })
		if index < 0 || p.Spec.Containers[index].Image != image {
			continue
		}
		result[string(p.UID)] = p
	}
	return result, nil
}

// prefetchTargetNodes resolves the placement into sorted node names. The rule lives in
// modelstore beside the pool resolution it consumes; this wrapper keeps the package's internal
// call sites and tests unchanged.
func prefetchTargetNodes(
	pf *workercore.ModelPrefetch, nodes []core.Node, deps []workercore.ModelDeployment,
	pools InstanceTypeFlavorNames,
) ([]string, error) {
	return modelstore.PrefetchTargetNodes(pf, nodes, deps, pools)
}

// deploymentInstanceTypes collects the InstanceType names the namespace's deployments serve the
// artifact's weights on; owned by modelstore, wrapped for this package's tests.
func deploymentInstanceTypes(deps []workercore.ModelDeployment, artifactName string) []string {
	return modelstore.DeploymentInstanceTypes(deps, artifactName)
}

// deliver keeps one live warm-up Pod per target node, in bounded batches, and removes the ones
// whose node left the target set or whose work is done. A replacement is rendered under a fresh
// generated name while its deleted predecessor is still terminating, so a node's live attempt and
// a terminating dead one can coexist for the grace period.
//
// The node's own entry state decides what a finished pod's removal means. A node still reading
// Downloading, or with no entry at all, is where a pod does work: its pod is (re)created. A node
// reading Ready or Failed has settled — a done pod there is removed and NOT replaced, so a
// deterministically failing materialization is surfaced as Degraded rather than retried at event
// rate, and a report lag after a pod succeeded never churns a fresh read-back.
func (r *ModelPrefetchReconciler) deliver(ctx context.Context, pf *workercore.ModelPrefetch,
	artifact *workercore.ModelArtifact, digest, image string, targets, ready []string,
	entries map[string]workercore.NodeModelStoreModel, expired map[string]bool,
) (ctrl.Result, error) {
	want := map[string]bool{}
	for _, node := range targets {
		want[node] = true
	}

	readySet := map[string]bool{}
	for _, node := range ready {
		readySet[node] = true
	}
	failed := map[string]bool{}
	waiting := map[string]bool{}
	for _, node := range targets {
		if expired[node] {
			continue
		}
		entry, ok := entries[node]
		switch {
		case !ok || entry.State == workercore.NodeModelStoreModelStateDownloading:
			waiting[node] = true
		case entry.State == workercore.NodeModelStoreModelStateFailed:
			failed[node] = true
		}
	}

	pods := new(core.PodList)
	if err := r.Client.List(ctx, pods, ctrlcli.InNamespace(pf.Namespace),
		ctrlcli.MatchingLabels{"worker.gpustack.ai/model-prefetch": pf.Name}); err != nil {
		return ctrl.Result{}, err
	}

	created := 0
	requeue := ctrl.Result{}
	removed := map[string]bool{}
	for _, pod := range pods.Items {
		if pod.Labels[modelTransferSourcePodLabel] != "" {
			owned := legacySourceHolder(&pod, pf)
			if !owned {
				ctrllog.FromContext(ctx).Info("preserve source-labeled Pod without proven legacy ownership",
					"pod", ctrlcli.ObjectKeyFromObject(&pod), "prefetch", ctrlcli.ObjectKeyFromObject(pf))
			}
			if owned && pod.DeletionTimestamp == nil {
				if err := r.Client.Delete(ctx, &pod, ctrlcli.Preconditions{UID: &pod.UID}); ctrlcli.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, err
				}
			}
			removed[pod.Name] = true
			continue
		}
		_, onTarget := want[pod.Spec.NodeName]
		// A pod mounting a digest the artifact no longer resolves to is stale the moment it is
		// seen: keeping it would hold the node on the old weights for as long as it lives.
		stale := podWarmDigest(&pod) != digest
		done := pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed
		settled := readySet[pod.Spec.NodeName] || failed[pod.Spec.NodeName] || expired[pod.Spec.NodeName]
		// A done pod on a settled node is evidence whose work ended; a done pod on a node still
		// reading waiting is a dead attempt, and its replacement goes up beside it.
		if !onTarget || stale || (done && (settled || waiting[pod.Spec.NodeName])) {
			if err := r.Client.Delete(ctx, &pod); ctrlcli.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			removed[pod.Name] = true
			continue
		}
	}

	// A deleted pod can linger while terminating, so skip attempts removed in this pass.
	live := map[string]bool{}
	for i := range pods.Items {
		if removed[pods.Items[i].Name] {
			continue
		}
		node := pods.Items[i].Spec.NodeName
		live[node] = true
	}

	for _, node := range targets {
		if !waiting[node] || live[node] {
			continue
		}
		if created >= modelPrefetchPodsPerPass {
			// The rest wait for the next pass, which the batch already ran asks for.
			requeue = ctrl.Result{RequeueAfter: 5 * time.Second}

			return requeue, nil
		}
		pod := warmupPod(pf, artifact, image, node, digest)
		kubemeta.ControlOn(pod, pf, workercore.SchemeGroupVersion.WithKind("ModelPrefetch"))
		if err := r.Client.Create(ctx, pod); err != nil {
			return ctrl.Result{}, err
		}
		created++
	}

	return requeue, nil
}

// bindingAllowsPinned is the binding's effective allowPinned, nil reading as the false default.
func bindingAllowsPinned(binding *workercore.ModelStoreBinding) bool {
	return binding.Spec.AllowPinned != nil && *binding.Spec.AllowPinned
}

// recomputePinned rewrites every node's spec.pinned to the union of two voices: the surviving
// prefetches' disk retention — a digest lands on the nodes whose delivery is ready and whose TTL
// has not expired, and a digest two namespaces pin stays for both — and the nodes' own reports,
// which pin every admitted generation's tree on its own node whatever the prefetches say. This
// controller is the field's only writer, so the whole list is replaced rather than merged — there
// is no second opinion to lose, and a pin that vanished from the desired set is gone by the end
// of the pass.
func (r *ModelPrefetchReconciler) recomputePinned(ctx context.Context) error {
	pfs := new(workercore.ModelPrefetchList)
	if err := r.Client.List(ctx, pfs); err != nil {
		return err
	}
	nodes := new(core.NodeList)
	if err := r.Client.List(ctx, nodes); err != nil {
		return err
	}
	stores := new(workercore.NodeModelStoreList)
	if err := r.Client.List(ctx, stores); err != nil {
		return err
	}
	// depsByNS keeps the derived-placement expansion inside the prefetch's own namespace: a
	// same-named artifact in another namespace must never widen this one's target set.
	deps := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, deps); err != nil {
		return err
	}
	depsByNS := map[string][]workercore.ModelDeployment{}
	for i := range deps.Items {
		ns := deps.Items[i].Namespace
		depsByNS[ns] = append(depsByNS[ns], deps.Items[i])
	}

	// Bindings and artifacts are read once each: a Get per prefetch turns the pass into 2P
	// round-trips for a map the whole loop can share.
	bindings := new(workercore.ModelStoreBindingList)
	if err := r.Client.List(ctx, bindings); err != nil {
		return err
	}
	bindingByKey := map[string]*workercore.ModelStoreBinding{}
	for i := range bindings.Items {
		b := &bindings.Items[i]
		bindingByKey[b.Namespace+"/"+b.Name] = b
	}
	artifacts := new(workercore.ModelArtifactList)
	if err := r.Client.List(ctx, artifacts); err != nil {
		return err
	}
	artifactByKey := map[string]*workercore.ModelArtifact{}
	for i := range artifacts.Items {
		a := &artifacts.Items[i]
		artifactByKey[a.Namespace+"/"+a.Name] = a
	}

	// desired[node] = the digests to pin there. Two independent voices pin:
	// the DISK RETENTION (spec.retention.pinned under the binding's
	// allowPinned, while the TTL holds, from live prefetches only), and the
	// SOURCE-FILE RETENTION — a published tree an admitted generation reads
	// is pinned for as long as that generation stands, whatever the intent,
	// the TTL, or the prefetch's deletion say, because the fill and every
	// refill re-verify against that tree and unpinning it under a live
	// generation strands the next drain-and-refill.
	storeByName := make(map[string]*workercore.NodeModelStore, len(stores.Items))
	admittedSourceDigests := map[string]bool{}
	for i := range stores.Items {
		store := &stores.Items[i]
		storeByName[store.Name] = store
		if store.Status.ModelTransfer != nil {
			for _, generation := range store.Status.ModelTransfer.Generations {
				admittedSourceDigests[generation.Digest] = true
			}
		}
	}
	desired := map[string]map[string]bool{}
	// The node's own report is the source-file pin's source: a generation
	// keeps reading its tree after the prefetch that named it was
	// retargeted, re-resolved to another revision, or deleted, so prefetch
	// membership cannot decide whether the tree stays pinned. Every
	// reported generation pins its digest on its own node, before any
	// prefetch loop runs; the reports' own departure removes the pin.
	for i := range stores.Items {
		store := &stores.Items[i]
		if store.Status.ModelTransfer == nil {
			continue
		}
		for _, generation := range store.Status.ModelTransfer.Generations {
			if generation.Digest == "" {
				continue
			}
			if desired[store.Name] == nil {
				desired[store.Name] = map[string]bool{}
			}
			desired[store.Name][generation.Digest] = true
		}
	}
	for i := range pfs.Items {
		pf := &pfs.Items[i]
		deleting := pf.DeletionTimestamp != nil

		artifact := artifactByKey[pf.Namespace+"/"+pf.Spec.ArtifactRef.Name]
		if artifact == nil || artifact.Status.Resolved == nil {
			continue
		}
		digest := artifact.Status.Resolved.ManifestDigest
		if digest == "" {
			continue
		}
		// A source intent pins no set it did not name: the derived-from-deployments default is
		// the disk half's, and a source without a placement is refused before it renders.
		hasSourcePlacement := pf.Spec.Source != nil && pf.Spec.Placement != nil &&
			(len(pf.Spec.Placement.InstanceTypes) > 0 || pf.Spec.Placement.NodeSelector != nil)
		binding := bindingByKey[pf.Namespace+"/"+pf.Spec.BindingRef.Name]
		retentionPin := !deleting && pf.Spec.Retention.Pinned &&
			binding != nil && bindingAllowsPinned(binding)
		if !retentionPin && (pf.Spec.Source != nil && !hasSourcePlacement) {
			continue
		}
		if !retentionPin && pf.Spec.Source == nil && !admittedSourceDigests[digest] {
			continue
		}
		pools, err := ResolveInstanceTypeFlavorNames(ctx, r.Client, pf, depsByNS[pf.Namespace])
		if err != nil {
			if !deleting {
				return err
			}

			continue
		}
		targets, err := prefetchTargetNodes(pf, nodes.Items, depsByNS[pf.Namespace], pools)
		if err != nil {
			continue // the placement is broken; the prefetch's own status carries it
		}
		entries := modelstore.NodeEntries(stores.Items, digest)

		for _, node := range targets {
			entry, ok := entries[node]
			treeReady := ok && entry.State == workercore.NodeModelStoreModelStateReady
			if !treeReady {
				continue
			}
			ttlHolds := retentionPin && !r.ttlExpired(pf, entry)
			// The source-file retention needs no grant, no pinned flag, and
			// no living prefetch: the generation holding the memory is the
			// reader, and the tree is what its next refill verifies against.
			sourceHolds := (pf.Spec.Source == nil || hasSourcePlacement) &&
				modelstore.ModelTransferAdmittedOnNode(storeByName[node], digest)
			if !ttlHolds && !sourceHolds {
				continue
			}
			if desired[node] == nil {
				desired[node] = map[string]bool{}
			}
			desired[node][digest] = true
		}
	}

	for i := range stores.Items {
		nms := &stores.Items[i]
		want := desired[nms.Name]
		if want == nil && len(nms.Spec.Pinned) == 0 {
			continue
		}
		fresh := make([]string, 0, len(want))
		for digest := range want {
			fresh = append(fresh, digest)
		}
		sort.Strings(fresh)
		if slices.Equal(nms.Spec.Pinned, fresh) {
			continue
		}
		updated := nms.DeepCopy()
		updated.Spec.Pinned = fresh
		if err := r.Client.Update(ctx, updated); err != nil {
			return err
		}
	}

	return nil
}

// ttlExpired says whether the node's last use of the content is older than the retention TTL. A
// node that never reported a use has none to expire.
func (r *ModelPrefetchReconciler) ttlExpired(pf *workercore.ModelPrefetch, entry workercore.NodeModelStoreModel) bool {
	ttl := pf.Spec.Retention.TTLAfterLastUse
	if ttl == nil || entry.LastUsedTime == nil {
		return false
	}

	return r.now().Sub(entry.LastUsedTime.Time) > ttl.Duration
}

// writeStatus fills the status from the progress. Available wants readyNodes to reach MinReady,
// where 0 means all of them; Degraded waits for a target node to give up — or for the artifact the
// prefetch names to disappear, which stops the delivery rather than leaving it fetching forever.
// Lapsed says when a shortfall below MinReady is the retention TTL's doing: it turns True only
// when the lapsed copies alone account for the gap, so a warm-up or a failure keeps its own voice.
//
// A source intent adds current eligible generations beside disk progress. Retained reservations
// remain visible while old generations drain, even when the desired intent changes or is removed.
func (r *ModelPrefetchReconciler) writeStatus(
	pf *workercore.ModelPrefetch, progress prefetchProgress, broken error, artifactMissing bool,
	source *sourceObservation, fromRemote bool,
) {
	pf.Status.DesiredNodes = progress.desired
	pf.Status.ReadyNodes = int32(len(progress.ready))
	pf.Status.DownloadingNodes = progress.downloading

	var summaryTruncated bool
	if source != nil && source.admitted {
		// The view exists when something is observed: an admitted generation, its figures, the
		// namespace's charge, and — when the Binding names one — the grant echoed as applied.
		// Deliverable-but-unadmitted is absent, never present and empty.
		summary, truncated := summarizeModelPrefetchTransferGenerations(source.generations)
		summaryTruncated = truncated
		transfer := &workercore.ModelPrefetchTransferStatus{
			ReservedBytes: source.charge,
			Generations:   summary,
		}
		if source.grant != nil {
			transfer.Limits = modelstore.TransferLimits(source.grant)
		}
		pf.Status.Transfer = transfer
	} else {
		// Nothing retained: omit the view once verified authority reports no reservations.
		pf.Status.Transfer = nil
	}

	required := requiredNodes(pf, progress.desired)
	delivered := int32(len(progress.ready)) >= required
	progressing := gpustack.Condition{
		Type:               ModelPrefetchConditionProgressing,
		ObservedGeneration: pf.Generation,
	}
	available := gpustack.Condition{
		Type:               ModelPrefetchConditionAvailable,
		Status:             meta.ConditionFalse,
		Reason:             "NotEnoughNodes",
		Message:            "fewer target nodes hold the content than the prefetch requires",
		ObservedGeneration: pf.Generation,
	}
	degraded := gpustack.Condition{
		Type:               ModelPrefetchConditionDegraded,
		Status:             meta.ConditionFalse,
		Reason:             "NoFailures",
		Message:            "no target node has given up",
		ObservedGeneration: pf.Generation,
	}
	lapsed := gpustack.Condition{
		Type:               ModelPrefetchConditionLapsed,
		Status:             meta.ConditionFalse,
		Reason:             "NoLapsedCopies",
		Message:            "no target node's copy has lapsed past retention.ttlAfterLastUse",
		ObservedGeneration: pf.Generation,
	}

	// The lapse explains the shortfall only when the lapsed copies alone account for the gap below
	// the requirement: a gap they cannot close has other causes — a warm-up still running or a
	// failure — that Lapsed must never paper over.
	lead := fmt.Sprintf("%d of %d target node copies lapsed past retention.ttlAfterLastUse",
		progress.lapsed, progress.desired)
	switch gap := required - int32(len(progress.ready)); {
	case progress.lapsed == 0:
		// Nothing lapsed; the default stands.
	case gap <= 0:
		lapsed.Reason = "NoShortfall"
		lapsed.Message = lead + ", but readyNodes still meets the requirement"
	case progress.lapsed >= gap:
		lapsed.Status, lapsed.Reason = meta.ConditionTrue, "RetentionTTLExpired"
		lapsed.Message = lead + " and no longer count toward readyNodes"
		if pf.Spec.Retention.Pinned {
			lapsed.Message = lead + " and their pins were released; they no longer count toward readyNodes"
		}
	default:
		lapsed.Reason = "ShortfallBeyondLapse"
		lapsed.Message = lead + ", but the shortfall below minReady is wider than the lapse; the rest is still warming or has failed"
	}

	switch {
	case broken != nil:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "PlacementInvalid"
		progressing.Message = "the placement does not parse, so no target set exists"
	case artifactMissing:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "ArtifactMissing"
		progressing.Message = "the artifact this prefetch names no longer exists"
		available.Status, available.Reason = meta.ConditionFalse, "ArtifactMissing"
		available.Message = "the artifact is gone, so the delivery cannot proceed"
		degraded.Status, degraded.Reason = meta.ConditionTrue, "ArtifactMissing"
		degraded.Message = "the named artifact was deleted after admission; create it again or delete the prefetch"
	case progress.desired == 0:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "NoTargets"
		progressing.Message = "the placement matches no nodes"
	case delivered:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "Delivered"
		progressing.Message = "every node the prefetch requires holds the content"
		available.Status, available.Reason = meta.ConditionTrue, "MinReadyReached"
		available.Message = "enough target nodes hold the content"
	default:
		progressing.Status, progressing.Reason = meta.ConditionTrue, "Delivering"
		progressing.Message = "some target nodes are still fetching or waiting on the artifact"
	}

	// A source refusal speaks on Progressing and only there: it says why the source half is not
	// being prepared. The disk half's own answer — Available above, delivered or not — keeps its
	// voice, because a refusal that never touches disk delivery must not drag a fully delivered
	// prefetch back to NotEnoughNodes.
	switch {
	case source != nil && source.placementMissing:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "SourcePlacementMissing"
		progressing.Message = "the source intent names no target set: set placement.nodeSelector or " +
			"placement.instanceTypes, or drop spec.source to warm wherever this namespace's deployments run"
	case source != nil && source.missingGrant:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "SourceGrantMissing"
		progressing.Message = "the referenced Binding does not exist, so no source is prepared; " +
			"the disk delivery is unaffected"
	case source != nil && source.unsized:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "SourceSizeUnresolved"
		progressing.Message = "the artifact's resolved size is not measured, so no allocation is authorized"
	case source != nil && source.belowFloor && fromRemote:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "SourceWindowBelowFloor"
		progressing.Message = "the grant's headroom cannot back even the minimum source window under the " +
			"Require policy; the source keeps waiting for the source node to free capacity"
	case source != nil && source.belowFloor:
		progressing.Status, progressing.Reason = meta.ConditionTrue, "SourceWindowFallback"
		progressing.Message = "the grant's headroom cannot back even the minimum source window under the " +
			"Prefer policy; the deployment falls back to Node-to-Node Peer Sync or the local CSI disk load"
	case source != nil && source.imageMissing:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "SourceWorkerImageMissing"
		progressing.Message = "no source-worker DaemonSet is deployed for every pool the target set names: configure " +
			"modelTransfer.images in the Helm chart for the target manufacturers; the disk delivery is unaffected"
	case source != nil && source.authorityUnknown:
		progressing.Status, progressing.Reason = meta.ConditionFalse, "SourceAuthorityUnknown"
		progressing.Message = "the source reservation cannot be verified; admission and readiness are unknown"
	}

	if progress.failed > 0 {
		degraded.Status, degraded.Reason = meta.ConditionTrue, "NodeFailed"
		degraded.Message = "one or more target nodes reported the delivery failed; their reasons are on the nodes' reports"
	}

	conditions := []gpustack.Condition{progressing, available, degraded, lapsed}
	if source != nil || slices.ContainsFunc(pf.Status.Conditions, func(c gpustack.Condition) bool {
		return c.Type == "SourceSummaryTruncated"
	}) {
		truncated := gpustack.Condition{
			Type: "SourceSummaryTruncated", Status: meta.ConditionFalse,
			ObservedGeneration: pf.Generation, Reason: "CompleteSummary",
			Message: "all observed source generations fit in the status summary",
		}
		if summaryTruncated {
			truncated.Status, truncated.Reason = meta.ConditionTrue, "GenerationLimitReached"
			truncated.Message = "the status summary keeps at most 16 generations, serving generations first; accounting and readiness use the full observation"
		}
		conditions = append(conditions, truncated)
	}
	hadSource := slices.ContainsFunc(pf.Status.Conditions, func(c gpustack.Condition) bool {
		return c.Type == ModelPrefetchConditionSourceReady
	})
	if pf.Spec.Source != nil || source != nil || hadSource {
		ready := gpustack.Condition{
			Type: ModelPrefetchConditionSourceReady, Status: meta.ConditionFalse,
			Reason: "NoServingGeneration", Message: "no current source generation has an unexpired serving lease",
			ObservedGeneration: pf.Generation,
		}
		switch {
		case pf.Spec.Source == nil:
			ready.Reason, ready.Message = "SourceDisabled", "this prefetch does not request a serving source"
		case source != nil && source.authorityUnknown:
			ready.Reason, ready.Message = "SourceAuthorityUnknown", "the source authority could not be verified"
		case source != nil && source.admitted && pf.Status.Transfer != nil && source.serving != nil &&
			source.serving.ReadyLeaseExpiresAt.After(r.now()):
			proof := *source.serving
			pf.Status.Transfer.Serving = &proof
			ready.Status, ready.Reason = meta.ConditionTrue, "SourceServing"
			ready.Message = "a current artifact source has an unexpired serving lease"
		}
		conditions = append(conditions, ready)
	}
	pf.Status.Conditions = carryConditionTransitions(pf.Status.Conditions, conditions, r.now())
}

func (r *ModelPrefetchReconciler) commitStatus(
	ctx context.Context, pf *workercore.ModelPrefetch, before *workercore.ModelPrefetchStatus,
) error {
	if kubemeta.DeepEqual(before, &pf.Status) {
		return nil
	}

	return r.Client.Status().Update(ctx, pf)
}

// requiredNodes is how many ready nodes satisfy the prefetch: MinReady, or every target when it
// is left at its default of zero.
func requiredNodes(pf *workercore.ModelPrefetch, desired int32) int32 {
	if pf.Spec.MinReady == 0 || pf.Spec.MinReady > desired {
		return desired
	}

	return pf.Spec.MinReady
}

// warmupImage reads the warm-up image Setting the way the configuration reconciler reads its
// layer: from the delegated secret, falling back to the built-in default.
func (r *ModelPrefetchReconciler) warmupImage(ctx context.Context) (string, error) {
	sec := new(core.Secret)
	key := ctrlcli.ObjectKey{Namespace: setting.DelegatedSecretNamespace, Name: setting.DelegatedSecretName}
	if err := r.Client.Get(ctx, key, sec); ctrlcli.IgnoreNotFound(err) != nil {
		return "", err
	}
	if v, ok := sec.Data[settings.ModelPrefetchWarmupImage.Name()]; ok {
		return string(v), nil
	}

	return settings.ModelPrefetchWarmupImage.DefaultValue(), nil
}

// sourceWorkerPlan selects source images independently from disk warm-up images.
// Manufacturers come from node features, never from host runtime versions. The mapping lives in
// modelstore; the worker contributes the Setting's parsed image map and keeps the secret
// acquisition.
type sourceWorkerPlan = modelstore.SourceWorkerPlan

// readSourceWorkerPlan reads the chart-rendered source DaemonSets and the target nodes' manufacturers.
// Nodes without accelerators retain the NVIDIA host-memory source path.
// Mixed-manufacturer nodes have no implicit selection and are refused.
func (r *ModelPrefetchReconciler) readSourceWorkerPlan(ctx context.Context, nodes []core.Node,
	targets []string,
) (sourceWorkerPlan, []apps.DaemonSet, error) {
	sets := new(apps.DaemonSetList)
	if err := r.reader().List(ctx, sets, ctrlcli.InNamespace(setting.DelegatedSecretNamespace), ctrlcli.MatchingLabels{
		"app.kubernetes.io/component": "model-transfer", "app.kubernetes.io/part-of": "gpustack-operator",
	}); err != nil {
		return sourceWorkerPlan{}, nil, err
	}
	byVendor := map[string][]*apps.DaemonSet{}
	for i := range sets.Items {
		ds := &sets.Items[i]
		if ds.UID == "" || ds.DeletionTimestamp != nil || ds.Labels["app.kubernetes.io/managed-by"] != "Helm" ||
			ds.Labels["app.kubernetes.io/instance"] == "" || ds.Spec.UpdateStrategy.Type != apps.OnDeleteDaemonSetStrategyType {
			continue
		}
		v := ds.Labels["gpustack.ai/manufacturer"]
		if v == "amd" || v == "ascend" || v == "nvidia" {
			byVendor[v] = append(byVendor[v], ds)
		}
	}
	images := map[string]string{}
	for vendor, matches := range byVendor {
		if len(matches) != 1 {
			continue
		}
		idx := slices.IndexFunc(matches[0].Spec.Template.Spec.Containers, func(c core.Container) bool { return c.Name == "worker" })
		if idx >= 0 && matches[0].Spec.Template.Spec.Containers[idx].Image != "" {
			images[vendor] = matches[0].Spec.Template.Spec.Containers[idx].Image
		}
	}

	return modelstore.PlanSourceWorkers(images, nodes, targets), sets.Items, nil
}

// warmupPod is the delivery operation rendered as a Pod: the artifact's own volume, the node
// pinned, nothing privileged, and a read-back that exits. It carries no queue-name label, so
// neither Kueue nor this operator's Pod webhook has anything to say about it. Each attempt is
// rendered under a fresh generated name, so a replacement never collides with its terminating
// predecessor.
func warmupPod(pf *workercore.ModelPrefetch, artifact *workercore.ModelArtifact, image, nodeName, digest string) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			GenerateName: prefetchPodName(pf.Name, nodeName) + "-",
			Namespace:    pf.Namespace,
			Labels: map[string]string{
				"worker.gpustack.ai/model-prefetch":        pf.Name,
				"worker.gpustack.ai/model-prefetch-digest": warmDigestLabel(digest),
			},
		},
		Spec: core.PodSpec{
			NodeName:                      nodeName,
			RestartPolicy:                 core.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To[int64](10),
			// A wedged mount or a hung read-back must end somewhere: the deadline fails the pod, the
			// entry's state (or its absence, on the next pass) decides whether the attempt repeats.
			ActiveDeadlineSeconds: ptr.To[int64](2 * 60 * 60),
			SecurityContext: &core.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](65534),
				SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []core.Container{{
				Name:    "warmup",
				Image:   image,
				Command: []string{"sh", "-c", "cd /model && find . -type f | sort | xargs -r sha256sum"},
				SecurityContext: &core.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &core.Capabilities{Drop: []core.Capability{"ALL"}},
				},
				VolumeMounts: []core.VolumeMount{{Name: "model", MountPath: "/model", ReadOnly: true}},
			}},
			Volumes: []core.Volume{{
				Name: "model",
				VolumeSource: core.VolumeSource{
					CSI: &core.CSIVolumeSource{
						Driver:   modelstore.DriverName,
						ReadOnly: ptr.To(true),
						VolumeAttributes: map[string]string{
							modelstore.VolumeAttrArtifact:       artifact.Name,
							modelstore.VolumeAttrArtifactUID:    string(artifact.UID),
							modelstore.VolumeAttrManifestDigest: digest,
						},
					},
				},
			}},
		},
	}
}

// podWarmDigest reads the digest the pod's warm-up volume was rendered for, empty when the pod
// predates the attribute or carries none.
func podWarmDigest(pod *core.Pod) string {
	for _, v := range pod.Spec.Volumes {
		if v.CSI != nil && v.CSI.Driver == modelstore.DriverName {
			return v.CSI.VolumeAttributes[modelstore.VolumeAttrManifestDigest]
		}
	}

	return ""
}

func (r *ModelPrefetchReconciler) removeLegacySourceHolders(ctx context.Context, pf *workercore.ModelPrefetch) error {
	pods := new(core.PodList)
	if err := r.Client.List(ctx, pods, ctrlcli.InNamespace(pf.Namespace), ctrlcli.MatchingLabels{modelTransferSourcePodLabel: pf.Name}); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if legacySourceHolder(pod, pf) && pod.DeletionTimestamp == nil {
			if err := r.Client.Delete(ctx, pod, ctrlcli.Preconditions{UID: &pod.UID}); ctrlcli.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

// legacySourceHolder recognizes only the inert Pod rendered by the former controller.
func legacySourceHolder(pod *core.Pod, pf *workercore.ModelPrefetch) bool {
	owner := meta.GetControllerOf(pod)
	if owner == nil || owner.UID != pf.UID || owner.Kind != "ModelPrefetch" ||
		pod.Labels[modelTransferSourcePodLabel] != pf.Name || len(pod.Spec.Containers) != 1 ||
		len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 {
		return false
	}
	c := pod.Spec.Containers[0]
	return c.Name == "source" && slices.Equal(c.Command, []string{"sh", "-c", "exec sleep infinity"}) &&
		len(c.Args) == 0 && len(pod.Spec.Volumes) == 1 && pod.Spec.Volumes[0].CSI != nil &&
		pod.Spec.Volumes[0].CSI.Driver == modelstore.DriverName
}

// warmDigestLabel folds a digest into a label value the API server accepts: label values stop at
// 63 characters and a full digest is 64 hex, so the first half carries the identity here. The
// authoritative digest match is the volume attribute, which keeps the whole value.
func warmDigestLabel(digest string) string {
	hexPart := strings.TrimPrefix(digest, "sha256:")
	if len(hexPart) > 32 {
		hexPart = hexPart[:32]
	}

	return hexPart
}

// prefetchPodName is the warm-up Pod's generateName prefix, named after its prefetch and its
// node. A combined name that fits stays readable; one that does not is settled by a digest of the
// pair, which neither panics on a long prefix nor collides the way two truncated node names
// sharing one would. The cap leaves room for the separator and the random suffix the API server
// appends to a generated name: 253 - 1 - 5.
func prefetchPodName(pf, node string) string {
	name := pf + "-warmup-" + node
	if len(name) <= 247 {
		return name
	}
	tag := stringx.SumBySHA256(pf + "/" + node)[:8]
	room := 247 - len(tag) - 1
	base := (pf + "-warmup-" + node)[:room]

	return base + "-" + tag
}

func requeueOr(chosen, fallback ctrl.Result) ctrl.Result {
	if chosen.RequeueAfter > 0 || chosen.Requeue {
		return chosen
	}

	return fallback
}

// InstanceTypeFlavorNames carries, per InstanceType name, the ResourceFlavor names the type's
// pool selects; owned by modelstore beside the resolution that fills it.
type InstanceTypeFlavorNames = modelstore.InstanceTypeFlavorNames

// ResolveInstanceTypeFlavorNames resolves the InstanceType names a prefetch's placement — or, in
// its default form, the type names its namespace's deployments serve the artifact on — into the
// ResourceFlavor names each type's pool selects: the flavors carrying the schedule labels the
// type's spec derives, the same reverse-lookup the type's backing ClusterQueue and its status
// Detail run. Flavor names are topology-qualified and count-suffixed, so an InstanceType's
// object name is never itself a flavor name and cannot be compared against one. The rule lives
// in modelstore; this wrapper is where the worker reads its CPU-manufacturer awareness setting,
// which the shared resolver takes as an explicit input.
func ResolveInstanceTypeFlavorNames(
	ctx context.Context, r ctrlcli.Reader, pf *workercore.ModelPrefetch, deps []workercore.ModelDeployment,
) (InstanceTypeFlavorNames, error) {
	return modelstore.ResolveInstanceTypeFlavorNames(ctx, r, pf, deps,
		settings.InstanceTypeAwareCPUManufacturer.ShouldValueBool(ctx))
}

// PrefetchTargetNodes resolves a prefetch's placement into node names. It is the controller's own
// expansion, shared with the admission webhook so admission and delivery can never disagree about
// what a prefetch would warm. The pools carry the named InstanceTypes' flavor resolution from
// ResolveInstanceTypeFlavorNames; the rule lives in modelstore.
func PrefetchTargetNodes(
	pf *workercore.ModelPrefetch, nodes []core.Node, deps []workercore.ModelDeployment,
	pools InstanceTypeFlavorNames,
) ([]string, error) {
	return modelstore.PrefetchTargetNodes(pf, nodes, deps, pools)
}

// The placement refusal the webhook shares: both kinds set is ambiguous, and the schema cannot
// see it because neither field is required.
var ErrPrefetchPlacementAmbiguous = modelstore.ErrPrefetchPlacementAmbiguous

// artifactResolutionChanged enqueues only when an artifact's resolution moved — the fact both the
// accounting and the prefetch delivery read. Description and label churn is nobody's feed here.
var artifactResolutionChanged = ctrlpredicate.Funcs{
	CreateFunc: func(ctrlevent.CreateEvent) bool { return true },
	DeleteFunc: func(ctrlevent.DeleteEvent) bool { return true },
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*workercore.ModelArtifact)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*workercore.ModelArtifact)
		if !ok {
			return false
		}

		return !kubemeta.DeepEqual(o.Status.Resolved, n.Status.Resolved)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

// nodeReportChanged enqueues only when a node's report changed in a way a prefetch or a grant's
// accounting reads: the per-digest entries and the pin list, and the node's model-transfer report — the
// source generations' health, whose change is exactly what invalidates transfer readiness. The
// plugin's other bookkeeping is another object's feed.
var nodeReportChanged = ctrlpredicate.Funcs{
	CreateFunc: func(ctrlevent.CreateEvent) bool { return true },
	DeleteFunc: func(ctrlevent.DeleteEvent) bool { return true },
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*workercore.NodeModelStore)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*workercore.NodeModelStore)
		if !ok {
			return false
		}

		return !kubemeta.DeepEqual(o.Status.Models, n.Status.Models) ||
			!kubemeta.DeepEqual(o.Spec.Pinned, n.Spec.Pinned) ||
			!kubemeta.DeepEqual(o.Status.ModelTransfer, n.Status.ModelTransfer)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

// nodeLabelsChanged enqueues only when a node's labels changed: the heartbeat's status writes
// decide nothing about which stores match a node or what a prefetch targets. The store
// reconciler keeps its own inline copy of this shape; both read only labels.
var nodeLabelsChanged = ctrlpredicate.Funcs{
	CreateFunc: func(ctrlevent.CreateEvent) bool { return true },
	DeleteFunc: func(ctrlevent.DeleteEvent) bool { return true },
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*core.Node)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*core.Node)
		if !ok {
			return false
		}

		return !maps.Equal(o.Labels, n.Labels)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

// SetupController watches everything a prefetch's target set or delivery depends on: the
// artifact's resolution is a status change, the deployments name the derived set, the nodes are
// the targets, and the nodes' reports are the readiness.
func (r *ModelPrefetchReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()
	r.Reader = opts.Manager.GetAPIReader()
	if err := opts.Manager.GetFieldIndexer().IndexField(ctx, &workercore.ModelPrefetch{},
		modelPrefetchArtifactIndex, modelPrefetchArtifactIndexValues); err != nil {
		return fmt.Errorf("index prefetches by artifact for source observation: %w", err)
	}

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modelprefetch").
		For(&workercore.ModelPrefetch{}, ctrlbuilder.WithPredicates(ctrlpredicate.GenerationChangedPredicate{})).
		// The warm-up Pods are this controller's own
		// writes: a Pod reaching a terminal phase (or vanishing) is the
		// event that replaces a dead attempt.
		Owns(&core.Pod{}).
		Watches(
			&workercore.ModelStoreBinding{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueuePrefetchesForBinding),
			ctrlbuilder.WithPredicates(bindingGrantChanged),
		).
		Watches(
			&workercore.ModelArtifact{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueuePrefetchesInNamespace),
			ctrlbuilder.WithPredicates(artifactResolutionChanged),
		).
		Watches(
			&workercore.ModelDeployment{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueuePrefetchesInNamespace),
			ctrlbuilder.WithPredicates(ctrlpredicate.GenerationChangedPredicate{}),
		).
		Watches(
			&core.Node{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllPrefetches),
			ctrlbuilder.WithPredicates(nodeLabelsChanged),
		).
		Watches(
			&workercore.NodeModelStore{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllPrefetches),
			ctrlbuilder.WithPredicates(nodeReportChanged),
		).
		Watches(
			&core.Pod{}, ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllPrefetches),
			ctrlbuilder.WithPredicates(modelTransferSourcePodChanged()),
		).
		Watches(
			&apps.DaemonSet{}, ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllPrefetches),
			ctrlbuilder.WithPredicates(modelTransferSourceDaemonSetChanged()),
		).
		Watches(
			&core.ConfigMap{}, ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllPrefetches),
			ctrlbuilder.WithPredicates(modelPrefetchSourceAuthorityChanged()),
		).
		Watches(
			&core.Secret{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllPrefetches),
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(o ctrlcli.Object) bool {
				return o.GetNamespace() == setting.DelegatedSecretNamespace && o.GetName() == setting.DelegatedSecretName
			})),
		).
		Complete(r)
}

// modelPrefetchSourceAuthorityChanged ignores metadata churn while retaining changes
// to reservation data, authority identity and deletion state.
func modelPrefetchSourceAuthorityChanged() ctrlpredicate.Predicate {
	selected := func(obj ctrlcli.Object) bool {
		cm, ok := obj.(*core.ConfigMap)
		return ok && cm.Namespace == setting.DelegatedSecretNamespace && strings.HasPrefix(cm.Name, "model-transfer-resv-")
	}
	return ctrlpredicate.Funcs{
		CreateFunc:  func(e ctrlevent.CreateEvent) bool { return selected(e.Object) },
		DeleteFunc:  func(e ctrlevent.DeleteEvent) bool { return selected(e.Object) },
		GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
		UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
			old, oldOK := e.ObjectOld.(*core.ConfigMap)
			next, nextOK := e.ObjectNew.(*core.ConfigMap)
			if !oldOK || !nextOK || (!selected(old) && !selected(next)) {
				return false
			}
			return !maps.Equal(old.Data, next.Data) || old.UID != next.UID ||
				!old.DeletionTimestamp.Equal(next.DeletionTimestamp)
		},
	}
}

// enqueueAllPrefetches enqueues every prefetch: nodes and their reports are cluster-wide facts any
// of them may read. When no prefetch exists, a sentinel request still runs the NotFound branch so
// admitted-generation pins on NodeModelStores are recomputed as generations appear and exit.
func (r *ModelPrefetchReconciler) enqueueAllPrefetches(ctx context.Context, _ ctrlcli.Object) []ctrlreconcile.Request {
	pfs := new(workercore.ModelPrefetchList)
	if err := r.Client.List(ctx, pfs); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model prefetches")

		return nil
	}
	if len(pfs.Items) == 0 {
		return []ctrlreconcile.Request{{NamespacedName: ctrlcli.ObjectKey{Name: "__pinned__"}}}
	}

	reqs := make([]ctrlreconcile.Request, 0, len(pfs.Items))
	for i := range pfs.Items {
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{
			Namespace: pfs.Items[i].Namespace, Name: pfs.Items[i].Name,
		}})
	}

	return reqs
}

// bindingGrantChanged wakes the prefetches when a grant's say-so moved: the grant appearing or
// growing resolves a SourceGrantMissing or SourceOverGrant refusal, the binding going away ends
// the source half, and allowPinned moves the disk retention. A binding's status churn decides
// none of those and wakes nobody.
var bindingGrantChanged = ctrlpredicate.Funcs{
	CreateFunc: func(ctrlevent.CreateEvent) bool { return true },
	DeleteFunc: func(ctrlevent.DeleteEvent) bool { return true },
	UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*workercore.ModelStoreBinding)
		if !ok {
			return false
		}
		n, ok := e.ObjectNew.(*workercore.ModelStoreBinding)
		if !ok {
			return false
		}

		return o.Generation != n.Generation ||
			o.Annotations[broker.AuthorityAnnotation] != n.Annotations[broker.AuthorityAnnotation] ||
			!o.DeletionTimestamp.Equal(n.DeletionTimestamp)
	},
	GenericFunc: func(ctrlevent.GenericEvent) bool { return false },
}

// enqueuePrefetchesForBinding enqueues the prefetches bound to the changed grant: its memory
// grant and its existence are exactly their source half's inputs, so a grant that appears, grows,
// or dies must reach them without waiting on an unrelated node event.
func (r *ModelPrefetchReconciler) enqueuePrefetchesForBinding(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	pfs := new(workercore.ModelPrefetchList)
	if err := r.Client.List(ctx, pfs, ctrlcli.InNamespace(obj.GetNamespace())); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model prefetches")

		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(pfs.Items))
	for i := range pfs.Items {
		if pfs.Items[i].Spec.BindingRef.Name != obj.GetName() {
			continue
		}
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{
			Namespace: pfs.Items[i].Namespace, Name: pfs.Items[i].Name,
		}})
	}

	return reqs
}

// enqueuePrefetchesInNamespace enqueues the prefetches of the object's namespace: an artifact's
// resolution and a deployment's placement both decide what their namespace's prefetches do.
func (r *ModelPrefetchReconciler) enqueuePrefetchesInNamespace(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	pfs := new(workercore.ModelPrefetchList)
	if err := r.Client.List(ctx, pfs, ctrlcli.InNamespace(obj.GetNamespace())); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model prefetches")

		return nil
	}

	reqs := make([]ctrlreconcile.Request, 0, len(pfs.Items))
	for i := range pfs.Items {
		reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{
			Namespace: pfs.Items[i].Namespace, Name: pfs.Items[i].Name,
		}})
	}

	return reqs
}
