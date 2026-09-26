package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// The reasons WeightsReady carries while a deployment's weights are not available. Every one of
// them holds the deployment's Pods back, and none of them touches a Pod that already runs.
const (
	modelWeightsReasonNotApplicable    = "NotApplicable"
	modelWeightsReasonArtifactNotFound = "ArtifactNotFound"
	modelWeightsReasonArtifactUnready  = "ArtifactNotResolved"
	modelWeightsReasonClaimNotBound    = "ClaimNotBound"
	modelWeightsReasonAccessModeClash  = "AccessModeConflict"
	modelWeightsReasonNotMounted       = "WeightsNotMounted"
	modelWeightsReasonDownloading      = "Downloading"
	modelWeightsReasonMounted          = "Mounted"
	modelWeightsReasonDownloaded       = "Downloaded"
	modelWeightsReasonNodeUnavailable  = "NodeDeliveryUnavailable"
	modelWeightsReasonFilterNeedsNode  = "FilterNeedsNodeDelivery"
	modelWeightsReasonMaterializing    = "Materializing"
	modelWeightsReasonMaterializeFail  = "MaterializationFailed"

	// modelArtifactNoProvisioner is the provisioner a static local-volume class names.
	modelArtifactNoProvisioner = "kubernetes.io/no-provisioner"
)

// modelArtifactWeights is what a consumer's weights resolved to on one pass.
type modelArtifactWeights struct {
	// Render is how the weights reach the engine. It is nil while the artifact has never resolved,
	// and then nothing about the consumer's Pods can be rendered at all.
	Render *ModelDeploymentArtifactRender
	// Affinity is the bound PV's required node affinity, added to every Pod at creation.
	Affinity *core.NodeSelector
	// Status is the echo written to the consumer's status, nil until the artifact resolved.
	Status *workercore.ModelDeploymentModelStatus
	// KVIdentity is the weight identity a KV store's keys are prefixed with; see
	// modelArtifactKVIdentity.
	KVIdentity string
	// Blocked says new Pods must not be created, for Reason and Message.
	Blocked bool
	Reason  string
	Message string
}

func blockedModelArtifactWeights(reason, message string) *modelArtifactWeights {
	return &modelArtifactWeights{Blocked: true, Reason: reason, Message: message}
}

// modelArtifactDeliveryMode is the delivery Setting as a ModelDeployment's reconcile reads it. It is
// a variable so a test can choose a delivery without a Settings store.
var modelArtifactDeliveryMode = func(ctx context.Context) string {
	return settings.ModelArtifactDeliveryMode.ShouldValue(ctx)
}

// resolveModelArtifactWeights reads the artifact a consumer references and, for a claim, where
// the claim lets its Pods run. pods is how many Pods would mount the weights at once, which is
// what a claim that is not shared cannot serve beyond one. nodeOnly says the consumer has no engine
// to download a hub artifact, so node delivery is its only one whatever the delivery Setting says.
//
// NOTHING HERE IS A REFUSAL. Every state that stops new Pods is one that changes by itself or by
// a user creating something, so the consumer waits and says why, and its Pods are created the pass
// after the state clears.
func resolveModelArtifactWeights(
	ctx context.Context, cli ctrlcli.Reader, namespace, name string, pods int, nodeOnly bool,
) (*modelArtifactWeights, error) {
	ma := new(workercore.ModelArtifact)
	if err := cli.Get(ctx, ctrlcli.ObjectKey{Namespace: namespace, Name: name}, ma); err != nil {
		if kerrors.IsNotFound(err) {
			return blockedModelArtifactWeights(modelWeightsReasonArtifactNotFound,
				fmt.Sprintf("ModelArtifact %q does not exist in this namespace; nothing is created until it does", name)), nil
		}
		return nil, fmt.Errorf("read model artifact %q: %w", name, err)
	}

	resolved := ma.Status.Resolved
	if resolved == nil {
		return blockedModelArtifactWeights(modelWeightsReasonArtifactUnready, fmt.Sprintf(
			"ModelArtifact %q has not resolved: %s", name, ModelArtifactConditionResolved.GetMessage(ma))), nil
	}

	w := &modelArtifactWeights{}
	switch source := ma.Spec.Source; {
	case source.PersistentVolumeClaim != nil:
		w.Render = &ModelDeploymentArtifactRender{
			Delivery:  workercore.ModelDeploymentModelDeliveryPvc,
			ClaimName: source.PersistentVolumeClaim.ClaimName,
			Path:      source.PersistentVolumeClaim.Path,
		}
	case source.HuggingFace != nil && (nodeOnly || modelArtifactDeliveryMode(ctx) == settings.ModelArtifactDeliveryNode):
		w.Render = &ModelDeploymentArtifactRender{
			Delivery:       workercore.ModelDeploymentModelDeliveryNode,
			ArtifactName:   name,
			ArtifactUID:    string(ma.UID),
			ManifestDigest: resolved.ManifestDigest,
			Repository:     source.HuggingFace.Repository,
			Revision:       resolved.Revision,
			SizeBytes:      resolved.SizeBytes,
		}
		if source.HuggingFace.SecretRef != nil {
			w.Render.SecretName = source.HuggingFace.SecretRef.Name
		}
	case source.HuggingFace != nil:
		w.Render = &ModelDeploymentArtifactRender{
			Delivery:   workercore.ModelDeploymentModelDeliveryEngine,
			Repository: source.HuggingFace.Repository,
			Revision:   resolved.Revision,
			SizeBytes:  resolved.SizeBytes,
			Endpoint:   settings.ModelArtifactHuggingFaceEndpoint.ShouldValue(ctx),
			HTTPSProxy: settings.ModelArtifactHTTPSProxy.ShouldValue(ctx),
			NoProxy:    settings.ModelArtifactNoProxy.ShouldValue(ctx),
		}
		if source.HuggingFace.SecretRef != nil {
			w.Render.SecretName = source.HuggingFace.SecretRef.Name
		}
	default:
		return blockedModelArtifactWeights(modelWeightsReasonArtifactUnready,
			fmt.Sprintf("ModelArtifact %q has a source this version does not deliver", name)), nil
	}
	w.KVIdentity = modelArtifactKVIdentity(ma)
	w.Status = &workercore.ModelDeploymentModelStatus{
		Artifact:       name,
		Revision:       resolved.Revision,
		ManifestDigest: resolved.ManifestDigest,
		Delivery:       w.Render.Delivery,
	}

	if !ModelArtifactConditionResolved.IsTrue(ma) {
		w.Blocked, w.Reason = true, modelWeightsReasonArtifactUnready
		w.Message = fmt.Sprintf("ModelArtifact %q is not resolved (%s): %s; running replicas are left alone",
			name, ModelArtifactConditionResolved.GetReason(ma), ModelArtifactConditionResolved.GetMessage(ma))
		return w, nil
	}

	switch w.Render.Delivery {
	case workercore.ModelDeploymentModelDeliveryPvc:
		if err := placeModelArtifactClaim(ctx, cli, namespace, w.Render.ClaimName, pods, w); err != nil {
			return nil, err
		}
	case workercore.ModelDeploymentModelDeliveryNode:
		// Node delivery needs the plugin. Whether it is installed is a runtime fact admission cannot
		// see for a value seeded from the environment or a plugin removed later, so the consumer waits.
		switch err := cli.Get(ctx, ctrlcli.ObjectKey{Name: modelstore.DriverName}, new(storage.CSIDriver)); {
		case kerrors.IsNotFound(err):
			w.Blocked, w.Reason = true, modelWeightsReasonNodeUnavailable
			w.Message = fmt.Sprintf("ModelArtifact %q is delivered by the node, and the CSIDriver %s does not exist: "+
				"install the model-manager plugin (chart value modelManager.enabled), or set "+
				"model-artifact-delivery-mode to Engine for a ModelDeployment", name, modelstore.DriverName)
		case err != nil:
			return nil, fmt.Errorf("read the CSIDriver %s: %w", modelstore.DriverName, err)
		}
	case workercore.ModelDeploymentModelDeliveryEngine:
		// An engine chooses its own files and its cache is sized to the filtered total, so a filter
		// could only be honored by the node.
		if len(ma.Spec.AllowPatterns) > 0 || len(ma.Spec.IgnorePatterns) > 0 {
			w.Blocked, w.Reason = true, modelWeightsReasonFilterNeedsNode
			w.Message = fmt.Sprintf("ModelArtifact %q selects files with allow or ignore patterns, which only node "+
				"delivery honors: set model-artifact-delivery-mode to Node", name)
		}
	}

	return w, nil
}

// placeModelArtifactClaim decides where a claim lets its Pods run, and whether it lets them run.
//
// KUEUE'S TOPOLOGY-AWARE SCHEDULING DOES NOT READ A POD'S VOLUMES. A claim bound to a PV with node
// affinity was measured to fail silently without this: TAS assigned another node, the Pod stayed
// Pending forever, and its Workload stayed Admitted holding quota with nothing on it saying why.
// With the PV's required node affinity on the Pod, TAS honors it, and when that node is full the
// Workload waits before quota and admits itself once room appears.
func placeModelArtifactClaim(
	ctx context.Context, cli ctrlcli.Reader, namespace, claimName string, pods int, w *modelArtifactWeights,
) error {
	pvc := new(core.PersistentVolumeClaim)
	if err := cli.Get(ctx, ctrlcli.ObjectKey{Namespace: namespace, Name: claimName}, pvc); err != nil {
		if kerrors.IsNotFound(err) {
			w.Blocked, w.Reason = true, modelWeightsReasonArtifactUnready
			w.Message = fmt.Sprintf("PersistentVolumeClaim %q does not exist in this namespace", claimName)
			return nil
		}
		return fmt.Errorf("read persistent volume claim %q: %w", claimName, err)
	}

	modes := pvc.Status.AccessModes
	if len(modes) == 0 {
		modes = pvc.Spec.AccessModes
	}
	if pods > 1 && !slices.Contains(modes, core.ReadOnlyMany) &&
		!slices.Contains(modes, core.ReadWriteMany) {
		w.Blocked, w.Reason = true, modelWeightsReasonAccessModeClash
		w.Message = fmt.Sprintf(
			"%d Pods would mount PersistentVolumeClaim %q, whose access modes %v allow neither ReadOnlyMany "+
				"nor ReadWriteMany: ReadWriteOnce would crowd every replica onto one node and "+
				"ReadWriteOncePod admits one; use a claim that can be shared", pods, claimName, modes)
		return nil
	}

	switch pvc.Status.Phase {
	case core.ClaimBound:
		pv := new(core.PersistentVolume)
		if err := cli.Get(ctx, ctrlcli.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
			return fmt.Errorf("read persistent volume %q: %w", pvc.Spec.VolumeName, err)
		}
		if pv.Spec.NodeAffinity != nil && pv.Spec.NodeAffinity.Required != nil {
			w.Affinity = pv.Spec.NodeAffinity.Required.DeepCopy()
		}
		return nil
	case core.ClaimPending:
		dynamic, err := modelArtifactClaimBindsOnFirstPod(ctx, cli, pvc)
		if err != nil {
			return err
		}
		if dynamic {
			// The provisioner creates the volume on the node the first Pod is assigned, so the
			// assignment decides the binding rather than the other way round.
			return nil
		}
	}

	w.Blocked, w.Reason = true, modelWeightsReasonClaimNotBound
	w.Message = fmt.Sprintf(
		"PersistentVolumeClaim %q is %s and nothing binds it to the node a Pod is assigned: bind it to "+
			"its PersistentVolume first (spec.volumeName), so the Pods can be placed where that volume is",
		claimName, pvc.Status.Phase)

	return nil
}

// modelArtifactClaimBindsOnFirstPod reports whether a pending claim is provisioned for the node its
// first consumer is assigned: WaitForFirstConsumer with a provisioner that creates volumes.
//
// A class without a provisioner is the usual static local-volume class, and it is treated as
// unbound: TAS picks a node first, and the binder then looks for a matching PV on that node alone,
// so a node without one would bring back the silent Pending that holds quota. That reading is
// inferred from the binding order, not measured.
func modelArtifactClaimBindsOnFirstPod(ctx context.Context, cli ctrlcli.Reader, pvc *core.PersistentVolumeClaim) (bool, error) {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return false, nil
	}
	sc := new(storage.StorageClass)
	if err := cli.Get(ctx, ctrlcli.ObjectKey{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		if kerrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read storage class %q: %w", *pvc.Spec.StorageClassName, err)
	}

	return sc.VolumeBindingMode != nil && *sc.VolumeBindingMode == storage.VolumeBindingWaitForFirstConsumer &&
		sc.Provisioner != modelArtifactNoProvisioner, nil
}

// injectModelArtifactAffinity adds a PV's required node affinity to a Pod's own, as an AND: the
// Pod's terms and the PV's are crossed, so a node satisfies the result only when it satisfies one
// term of each.
func injectModelArtifactAffinity(pod *core.Pod, required *core.NodeSelector) {
	if required == nil || len(required.NodeSelectorTerms) == 0 {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &core.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &core.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	existing := na.RequiredDuringSchedulingIgnoredDuringExecution
	if existing == nil || len(existing.NodeSelectorTerms) == 0 {
		na.RequiredDuringSchedulingIgnoredDuringExecution = required.DeepCopy()
		return
	}

	crossed := make([]core.NodeSelectorTerm, 0, len(existing.NodeSelectorTerms)*len(required.NodeSelectorTerms))
	for _, a := range existing.NodeSelectorTerms {
		for _, b := range required.NodeSelectorTerms {
			crossed = append(crossed, core.NodeSelectorTerm{
				MatchExpressions: slices.Concat(a.MatchExpressions, b.MatchExpressions),
				MatchFields:      slices.Concat(a.MatchFields, b.MatchFields),
			})
		}
	}
	na.RequiredDuringSchedulingIgnoredDuringExecution = &core.NodeSelector{NodeSelectorTerms: crossed}
}

// resolveModelDeploymentWeights resolves the weights spec.model.artifactRef names, or nil for a
// deployment naming none.
func (r *ModelDeploymentReconciler) resolveModelDeploymentWeights(
	ctx context.Context, md *workercore.ModelDeployment,
) (*modelArtifactWeights, error) {
	name := ModelDeploymentArtifactName(md)
	if name == "" {
		return nil, nil
	}

	// Every Pod of every role mounts a claim, the take-over tier's included.
	pods := 0
	for i := range md.Spec.Roles {
		pods += int(md.Spec.Roles[i].Replicas) * modelDeploymentRoleSize(&md.Spec.Roles[i])
	}

	return resolveModelArtifactWeights(ctx, r.Client, md.Namespace, name, pods, false)
}

// observeModelDeploymentWeights writes status.model and the WeightsReady condition. nodeModels is
// what each replica Pod's node reports about the digest, by node name, for node delivery.
//
// THE CONDITION IS DERIVED FROM POD CONDITIONS AND NODE STATUS, NEVER FROM EVENTS. A claim's or a
// node-delivered artifact's weights are there once kubelet has mounted the Pod's volumes, which
// PodReadyToStartContainers reports, and until then the node's NodeModelStore says whether it is
// materializing them or failed to; an engine download is complete only once the engine serves,
// because the engine reports no progress of its own.
func observeModelDeploymentWeights(
	holder *workercore.ModelDeployment, pods []core.Pod, weights *modelArtifactWeights,
	nodeModels map[string]*workercore.NodeModelStoreModel,
) {
	if weights == nil {
		holder.Status.Model = nil
		ModelDeploymentConditionWeightsReady.True(holder, modelWeightsReasonNotApplicable,
			"the deployment names no model artifact")
		return
	}

	holder.Status.Model = weights.Status
	if weights.Blocked {
		ModelDeploymentConditionWeightsReady.False(holder, weights.Reason, weights.Message)
		return
	}

	engine := weights.Render.Delivery == workercore.ModelDeploymentModelDeliveryEngine
	node := weights.Render.Delivery == workercore.ModelDeploymentModelDeliveryNode
	waiting := 0
	var materializing int
	var failed *workercore.NodeModelStoreModel
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed {
			continue
		}
		if engine && !modelDeploymentMainContainerReady(pod) || !engine && !modelDeploymentPodVolumesMounted(pod) {
			waiting++
			if m := nodeModels[pod.Spec.NodeName]; node && m != nil {
				switch m.State {
				case workercore.NodeModelStoreModelStateDownloading:
					materializing++
				case workercore.NodeModelStoreModelStateFailed:
					failed = m
				}
			}
		}
	}

	switch {
	case waiting > 0 && failed != nil:
		retry := ""
		if failed.RetryTime != nil {
			retry = "; the node tries again at " + failed.RetryTime.UTC().Format(time.RFC3339)
		}
		ModelDeploymentConditionWeightsReady.False(holder, modelWeightsReasonMaterializeFail, fmt.Sprintf(
			"a replica Pod's node failed to materialize %s, %s: %s%s", failed.Digest, failed.Reason, failed.Message, retry))
	case waiting > 0 && materializing > 0:
		ModelDeploymentConditionWeightsReady.False(holder, modelWeightsReasonMaterializing, fmt.Sprintf(
			"%d replica Pods wait for their nodes to materialize %s; a Pod starts at kubelet's next mount "+
				"retry after that, up to about two minutes later", waiting, weights.Render.ManifestDigest))
	case waiting > 0 && engine:
		ModelDeploymentConditionWeightsReady.False(holder, modelWeightsReasonDownloading, fmt.Sprintf(
			"%d replica Pods' engines are not ready yet; each downloads commit %s itself and reports no progress",
			waiting, weights.Render.Revision))
	case waiting > 0:
		ModelDeploymentConditionWeightsReady.False(holder, modelWeightsReasonNotMounted, fmt.Sprintf(
			"%d replica Pods do not have their volumes mounted yet (PodReadyToStartContainers is not True)", waiting))
	case engine:
		ModelDeploymentConditionWeightsReady.True(holder, modelWeightsReasonDownloaded,
			fmt.Sprintf("every replica Pod serves commit %s", weights.Render.Revision))
	case node:
		ModelDeploymentConditionWeightsReady.True(holder, modelWeightsReasonMounted,
			fmt.Sprintf("every replica Pod has %s mounted from its node", weights.Render.ManifestDigest))
	default:
		ModelDeploymentConditionWeightsReady.True(holder, modelWeightsReasonMounted,
			fmt.Sprintf("every replica Pod has PersistentVolumeClaim %q mounted", weights.Render.ClaimName))
	}
}

// modelArtifactNodeModels reads what each Pod's node reports about digest, by node name. A node
// without a NodeModelStore, or one not listing the digest, has no entry.
func modelArtifactNodeModels(
	ctx context.Context, cli ctrlcli.Reader, pods []core.Pod, digest string,
) (map[string]*workercore.NodeModelStoreModel, error) {
	models := map[string]*workercore.NodeModelStoreModel{}
	for i := range pods {
		name := pods[i].Spec.NodeName
		if name == "" {
			continue
		}
		if _, seen := models[name]; seen {
			continue
		}
		nms := new(workercore.NodeModelStore)
		if err := cli.Get(ctx, ctrlcli.ObjectKey{Name: name}, nms); err != nil {
			if kerrors.IsNotFound(err) {
				models[name] = nil
				continue
			}
			return nil, fmt.Errorf("read node model store %q: %w", name, err)
		}
		models[name] = nil
		for j := range nms.Status.Models {
			if nms.Status.Models[j].Digest == digest {
				models[name] = &nms.Status.Models[j]
			}
		}
	}

	return models, nil
}

func modelDeploymentPodVolumesMounted(pod *core.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == core.PodReadyToStartContainers {
			return c.Status == core.ConditionTrue
		}
	}

	return false
}

// modelDeploymentMainContainerReady reads the engine's own container rather than the Pod: the Pod's
// readiness also waits on a routing sidecar, which says nothing about the download.
func modelDeploymentMainContainerReady(pod *core.Pod) bool {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == modelDeploymentMainContainerName {
			return c.Ready
		}
	}

	return false
}

// modelDeploymentEvictionNote returns the kubelet's own message of the first evicted replica Pod,
// or "". An eviction's reason survives only on the Pod: the container's logs go with it.
func modelDeploymentEvictionNote(pods []core.Pod) string {
	for i := range pods {
		if pods[i].Status.Reason == "Evicted" && pods[i].Status.Message != "" {
			return fmt.Sprintf("replica Pod %s was evicted: %s", pods[i].Name, pods[i].Status.Message)
		}
	}

	return ""
}

// mapModelDeploymentArtifact enqueues the deployments in the artifact's namespace referencing it.
func (r *ModelDeploymentReconciler) mapModelDeploymentArtifact(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	return r.modelDeploymentsReferencing(ctx, obj.GetNamespace(), obj.GetName())
}

// mapModelDeploymentClaim enqueues the deployments whose artifact is the claim.
func (r *ModelDeploymentReconciler) mapModelDeploymentClaim(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	artifacts := new(workercore.ModelArtifactList)
	if err := r.Client.List(ctx, artifacts, ctrlcli.InNamespace(obj.GetNamespace())); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model artifacts for a claim", "claim", obj.GetName())
		return nil
	}
	var names []string
	for i := range artifacts.Items {
		if claim := artifacts.Items[i].Spec.Source.PersistentVolumeClaim; claim != nil && claim.ClaimName == obj.GetName() {
			names = append(names, artifacts.Items[i].Name)
		}
	}

	return r.modelDeploymentsReferencing(ctx, obj.GetNamespace(), names...)
}

// mapModelDeploymentNodeModelStore enqueues the node-delivered deployments whose digest the node
// lists.
func (r *ModelDeploymentReconciler) mapModelDeploymentNodeModelStore(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	nms, ok := obj.(*workercore.NodeModelStore)
	if !ok || len(nms.Status.Models) == 0 {
		return nil
	}
	digests := make(map[string]bool, len(nms.Status.Models))
	for i := range nms.Status.Models {
		digests[nms.Status.Models[i].Digest] = true
	}
	mds := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mds); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model deployments for a node model store")
		return nil
	}
	var reqs []ctrlreconcile.Request
	for i := range mds.Items {
		m := mds.Items[i].Status.Model
		if m != nil && m.Delivery == workercore.ModelDeploymentModelDeliveryNode && digests[m.ManifestDigest] {
			reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&mds.Items[i])})
		}
	}

	return reqs
}

// nodeModelStoreModelsChanged passes a NodeModelStore update only when its models changed. A
// deployment reads nothing else from the node, and the capacity and conditions move during every
// download without changing what any deployment sees.
func nodeModelStoreModelsChanged() ctrlpredicate.Predicate {
	// A download's progress moves on every threshold the node writes, and no deployment reads it.
	return ctrlpredicate.Funcs{
		UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
			o, okOld := e.ObjectOld.(*workercore.NodeModelStore)
			n, okNew := e.ObjectNew.(*workercore.NodeModelStore)
			return !okOld || !okNew || !kubemeta.DeepEqual(modelsWithoutProgress(o), modelsWithoutProgress(n))
		},
	}
}

// modelsWithoutProgress is the node's entries with the download progress left out.
func modelsWithoutProgress(nms *workercore.NodeModelStore) []workercore.NodeModelStoreModel {
	models := make([]workercore.NodeModelStoreModel, len(nms.Status.Models))
	for i, m := range nms.Status.Models {
		m.DownloadedBytes = 0
		models[i] = m
	}

	return models
}

// mapModelDeploymentDelivery enqueues every deployment on an artifact. Which delivery a hub artifact
// takes, and whether it can be delivered at all, follow the delivery Setting and the plugin's
// CSIDriver, and neither changes any deployment's own object: without this a deployment waiting on
// NodeDeliveryUnavailable or FilterNeedsNodeDelivery stays waiting after the fix, and a switch of
// delivery rolls nothing until something unrelated wakes each deployment.
func (r *ModelDeploymentReconciler) mapModelDeploymentDelivery(ctx context.Context, _ ctrlcli.Object) []ctrlreconcile.Request {
	mds := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mds); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model deployments for a delivery change")
		return nil
	}
	var reqs []ctrlreconcile.Request
	for i := range mds.Items {
		if ModelDeploymentArtifactName(&mds.Items[i]) != "" {
			reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&mds.Items[i])})
		}
	}

	return reqs
}

// modelDeploymentDeliveryRecheck is how often a deployment held by the delivery is looked at again
// without an event.
const modelDeploymentDeliveryRecheck = time.Minute

// settingsReadDelay is how long after a Settings change a deployment is looked at again: past the
// Settings read cache, so the pass reads the new value rather than the cached one.
const settingsReadDelay = setting.ReadCacheTTL + 5*time.Second

// enqueueAfterSettingsRead enqueues what mapFn returns for a Settings change once settingsReadDelay
// has passed.
func enqueueAfterSettingsRead(mapFn ctrlhandler.MapFunc) ctrlhandler.EventHandler {
	add := func(ctx context.Context, obj ctrlcli.Object, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
		for _, req := range mapFn(ctx, obj) {
			q.AddAfter(req, settingsReadDelay)
		}
	}

	return ctrlhandler.Funcs{
		CreateFunc: func(ctx context.Context, e ctrlevent.CreateEvent, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			add(ctx, e.Object, q)
		},
		UpdateFunc: func(ctx context.Context, e ctrlevent.UpdateEvent, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			add(ctx, e.ObjectNew, q)
		},
		DeleteFunc: func(ctx context.Context, e ctrlevent.DeleteEvent, q workqueue.TypedRateLimitingInterface[ctrlreconcile.Request]) {
			add(ctx, e.Object, q)
		},
	}
}

func (r *ModelDeploymentReconciler) modelDeploymentsReferencing(
	ctx context.Context, namespace string, artifacts ...string,
) []ctrlreconcile.Request {
	if len(artifacts) == 0 {
		return nil
	}
	mds := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mds, ctrlcli.InNamespace(namespace)); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model deployments for a model artifact")
		return nil
	}
	var reqs []ctrlreconcile.Request
	for i := range mds.Items {
		if slices.Contains(artifacts, ModelDeploymentArtifactName(&mds.Items[i])) {
			reqs = append(reqs, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&mds.Items[i])})
		}
	}

	return reqs
}

// modelArtifactKVIdentityDigits is how many hexadecimal digits of the digest the identity keeps:
// 128 bits, far beyond any collision among the artifacts sharing one store tenant, at 34 characters
// on every key.
const modelArtifactKVIdentityDigits = 32

// modelArtifactKVIdentity is the weight identity a deployment's KV store keys carry: "m-" and the
// leading digits of the manifest digest for a hub artifact, whose content has an address, or of the
// SHA-256 of the artifact's UID for a claim, whose content has none. Deployments with one identity
// share blocks; deployments with two never do.
//
// A CLAIM'S IDENTITY IS THE ARTIFACT, NOT WHAT THE CLAIM HOLDS, so replacing the files under one
// artifact keeps the identity and new deployments would hit blocks computed from the old files. New
// weights on a claim are a new ModelArtifact, and the documentation says so.
//
// The result holds no "@", "_" or ":", the separators the engines join their keys with.
//
// The artifact must be resolved: a Hugging Face artifact has no identity before its digest, and
// falling back to the UID would render a wrong one rather than none.
func modelArtifactKVIdentity(ma *workercore.ModelArtifact) string {
	hexDigits := strings.TrimPrefix(ma.Status.Resolved.ManifestDigest, modelartifact.DigestSHA256+":")
	if ma.Status.Resolved.ManifestDigest == "" {
		sum := sha256.Sum256([]byte(ma.UID))
		hexDigits = hex.EncodeToString(sum[:])
	}
	if len(hexDigits) > modelArtifactKVIdentityDigits {
		hexDigits = hexDigits[:modelArtifactKVIdentityDigits]
	}

	return "m-" + hexDigits
}
