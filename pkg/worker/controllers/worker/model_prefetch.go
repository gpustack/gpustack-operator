package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	"gpustack.ai/gpustack/pkg/setting"
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
)

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
	Now    func() time.Time
}

var _ ctrlreconcile.Reconciler = (*ModelPrefetchReconciler)(nil)

func (r *ModelPrefetchReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

func (r *ModelPrefetchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	pf := new(workercore.ModelPrefetch)
	switch err := r.Client.Get(ctx, req.NamespacedName, pf); {
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
	targets, err := prefetchTargetNodes(pf, nodes.Items, deps.Items)
	if err != nil {
		// An unparseable selector is a spec the API server's schema should have refused; report it
		// as degraded and wait for the next generation.
		logger.Error(err, "the prefetch's placement does not parse")
		before := pf.Status.DeepCopy()
		r.writeStatus(pf, prefetchProgress{}, err, false)
		return ctrl.Result{}, r.commitStatus(ctx, pf, before)
	}

	artifact := new(workercore.ModelArtifact)
	artifactErr := r.Client.Get(ctx,
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
	r.writeStatus(pf, progress, err, artifactMissing)
	if err := r.commitStatus(ctx, pf, before); err != nil {
		return ctrl.Result{}, err
	}

	// The pins are recomputed as a whole after every pass, so a prefetch that lapsed, was retargeted
	// or was deleted leaves no pin behind and never drops a pin another prefetch still wants.
	if err := r.recomputePinned(ctx); err != nil {
		return ctrl.Result{}, err
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
}

// prefetchTargetNodes resolves the placement into sorted node names: the named InstanceTypes'
// nodes, or the selector's matches, or — with no placement at all — the InstanceTypes of the
// namespace's deployments that reference the artifact.
func prefetchTargetNodes(
	pf *workercore.ModelPrefetch, nodes []core.Node, deps []workercore.ModelDeployment,
) ([]string, error) {
	placement := pf.Spec.Placement
	var typeNames []string
	switch {
	case placement != nil && len(placement.InstanceTypes) > 0 && placement.NodeSelector != nil:
		return nil, ErrPrefetchPlacementAmbiguous
	case placement != nil && len(placement.InstanceTypes) > 0:
		typeNames = placement.InstanceTypes
	case placement != nil && placement.NodeSelector != nil:
		sel, err := meta.LabelSelectorAsSelector(placement.NodeSelector)
		if err != nil {
			return nil, err
		}
		var out []string
		for i := range nodes {
			if sel.Matches(labels.Set(nodes[i].Labels)) {
				out = append(out, nodes[i].Name)
			}
		}
		sort.Strings(out)

		return out, nil
	default:
		// The derived set: every InstanceType the namespace's own deployments serve these weights
		// on, so warming follows serving without a second place to say where.
		typeNames = deploymentInstanceTypes(deps, pf.Spec.ArtifactRef.Name)
	}

	wanted := map[string]bool{}
	for _, name := range typeNames {
		wanted[name] = true
	}
	var out []string
	for i := range nodes {
		for name := range wanted {
			if matchNodeFlavor(&nodes[i], name) != nil {
				out = append(out, nodes[i].Name)
				break
			}
		}
	}
	sort.Strings(out)

	return out, nil
}

// deploymentInstanceTypes collects the InstanceType names the namespace's deployments serve the
// artifact's weights on.
func deploymentInstanceTypes(deps []workercore.ModelDeployment, artifactName string) []string {
	var out []string
	for i := range deps {
		ref := deps[i].Spec.Model.ArtifactRef
		if ref == nil || ref.Name != artifactName {
			continue
		}
		for _, role := range deps[i].Spec.Roles {
			if role.InstanceType != "" {
				out = append(out, role.InstanceType)
			}
		}
	}

	return out
}

// deliver ensures at most one warm-up Pod per target node, in bounded batches, and removes the
// ones whose node left the target set or whose work is done.
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
	for _, pod := range pods.Items {
		_, onTarget := want[pod.Spec.NodeName]
		// A pod mounting a digest the artifact no longer resolves to is stale the moment it is
		// seen: keeping it would hold the node on the old weights for as long as it lives.
		stale := podWarmDigest(&pod) != digest
		done := pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed
		settled := readySet[pod.Spec.NodeName] || failed[pod.Spec.NodeName] || expired[pod.Spec.NodeName]
		// A done pod on a settled node is evidence whose work ended; a done pod on a node still
		// reading waiting is a dead attempt blocking the retry by its name, and goes.
		if !onTarget || stale || (done && (settled || waiting[pod.Spec.NodeName])) {
			if err := r.Client.Delete(ctx, &pod); ctrlcli.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
	}

	for _, node := range targets {
		if !waiting[node] {
			continue
		}
		name := prefetchPodName(pf.Name, node)
		existing := new(core.Pod)
		switch err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: pf.Namespace, Name: name}, existing); {
		case kerrors.IsNotFound(err):
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
		case err != nil:
			return ctrl.Result{}, err
		}
	}

	return requeue, nil
}

// bindingAllowsPinned is the binding's effective allowPinned, nil reading as the false default.
func bindingAllowsPinned(binding *workercore.ModelStoreBinding) bool {
	return binding.Spec.AllowPinned != nil && *binding.Spec.AllowPinned
}

// recomputePinned rewrites every node's spec.pinned to the union of what the surviving prefetches
// pin: a digest lands on the nodes whose delivery is ready and whose TTL has not expired, and a
// digest two namespaces pin stays for both. This controller is the field's only writer, so the
// whole list is replaced rather than merged — there is no second opinion to lose, and a pin that
// vanished from the desired set is gone by the end of the pass.
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

	// desired[node] = the digests to pin there, from every prefetch whose retention still holds.
	desired := map[string]map[string]bool{}
	for i := range pfs.Items {
		pf := &pfs.Items[i]
		if pf.DeletionTimestamp != nil || !pf.Spec.Retention.Pinned {
			continue
		}
		binding := bindingByKey[pf.Namespace+"/"+pf.Spec.BindingRef.Name]
		if binding == nil {
			continue // the grant is gone; nothing is pinned on its say-so
		}
		if !bindingAllowsPinned(binding) {
			continue
		}

		artifact := artifactByKey[pf.Namespace+"/"+pf.Spec.ArtifactRef.Name]
		if artifact == nil || artifact.Status.Resolved == nil {
			continue
		}
		digest := artifact.Status.Resolved.ManifestDigest
		if digest == "" {
			continue
		}
		targets, err := prefetchTargetNodes(pf, nodes.Items, depsByNS[pf.Namespace])
		if err != nil {
			continue // the placement is broken; the prefetch's own status carries it
		}
		entries := modelstore.NodeEntries(stores.Items, digest)
		for _, node := range targets {
			entry, ok := entries[node]
			entryReady := ok && entry.State == workercore.NodeModelStoreModelStateReady && !r.ttlExpired(pf, entry)
			if !entryReady {
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
func (r *ModelPrefetchReconciler) writeStatus(
	pf *workercore.ModelPrefetch, progress prefetchProgress, broken error, artifactMissing bool,
) {
	pf.Status.DesiredNodes = progress.desired
	pf.Status.ReadyNodes = int32(len(progress.ready))
	pf.Status.DownloadingNodes = progress.downloading

	delivered := int32(len(progress.ready)) >= requiredNodes(pf, progress.desired)
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

	if progress.failed > 0 {
		degraded.Status, degraded.Reason = meta.ConditionTrue, "NodeFailed"
		degraded.Message = "one or more target nodes reported the delivery failed; their reasons are on the nodes' reports"
	}

	pf.Status.Conditions = carryConditionTransitions(pf.Status.Conditions,
		[]gpustack.Condition{progressing, available, degraded}, r.now())
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

// warmupPod is the delivery operation rendered as a Pod: the artifact's own volume, the node
// pinned, nothing privileged, and a read-back that exits. It carries no queue-name label, so
// neither Kueue nor this operator's Pod webhook has anything to say about it.
func warmupPod(pf *workercore.ModelPrefetch, artifact *workercore.ModelArtifact, image, nodeName, digest string) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      prefetchPodName(pf.Name, nodeName),
			Namespace: pf.Namespace,
			Labels: map[string]string{
				"worker.gpustack.ai/model-prefetch":        pf.Name,
				"worker.gpustack.ai/model-prefetch-digest": strings.ReplaceAll(digest, "sha256:", ""),
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

// prefetchPodName names the warm-up Pod after its prefetch and its node. A combined name that
// fits stays readable; one that does not is settled by a digest of the pair, which neither panics
// on a long prefix nor collides the way two truncated node names sharing one would.
func prefetchPodName(pf, node string) string {
	name := pf + "-warmup-" + node
	if len(name) <= 253 {
		return name
	}
	sum := sha256.Sum256([]byte(pf + "/" + node))
	tag := hex.EncodeToString(sum[:])[:8]
	room := 253 - len(tag) - 1
	base := (pf + "-warmup-" + node)[:room]

	return base + "-" + tag
}

func requeueOr(chosen, fallback ctrl.Result) ctrl.Result {
	if chosen.RequeueAfter > 0 || chosen.Requeue {
		return chosen
	}

	return fallback
}

// MatchesInstanceType reports whether the node carries the named InstanceType's flavor: the same
// matching the placement expansion runs, exported for the admission webhook that projects a
// prefetch onto the nodes it would warm.
func MatchesInstanceType(nd *core.Node, instanceTypeName string) bool {
	return matchNodeFlavor(nd, instanceTypeName) != nil
}

// PrefetchTargetNodes resolves a prefetch's placement into node names. It is the controller's own
// expansion, shared with the admission webhook so admission and delivery can never disagree about
// what a prefetch would warm.
func PrefetchTargetNodes(pf *workercore.ModelPrefetch, nodes []core.Node, deps []workercore.ModelDeployment) ([]string, error) {
	return prefetchTargetNodes(pf, nodes, deps)
}

// The placement refusal the webhook shares: both kinds set is ambiguous, and the schema cannot
// see it because neither field is required.
var ErrPrefetchPlacementAmbiguous = fmt.Errorf("placement sets both instanceTypes and nodeSelector")

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

// nodeReportChanged enqueues only when a node's report changed in a way a prefetch reads: the
// per-digest entries and the pin list. Watermark bookkeeping the plugin writes alongside is
// another object's feed.
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
			!kubemeta.DeepEqual(o.Spec.Pinned, n.Spec.Pinned)
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
func (r *ModelPrefetchReconciler) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("modelprefetch").
		For(&workercore.ModelPrefetch{}, ctrlbuilder.WithPredicates(ctrlpredicate.GenerationChangedPredicate{})).
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
			&core.Secret{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueAllPrefetches),
			ctrlbuilder.WithPredicates(ctrlpredicate.NewPredicateFuncs(func(o ctrlcli.Object) bool {
				return o.GetNamespace() == setting.DelegatedSecretNamespace && o.GetName() == setting.DelegatedSecretName
			})),
		).
		Complete(r)
}

// enqueueAllPrefetches enqueues every prefetch: nodes and their reports are cluster-wide facts any
// of them may read.
func (r *ModelPrefetchReconciler) enqueueAllPrefetches(ctx context.Context, _ ctrlcli.Object) []ctrlreconcile.Request {
	pfs := new(workercore.ModelPrefetchList)
	if err := r.Client.List(ctx, pfs); err != nil {
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
