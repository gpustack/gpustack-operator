package worker

import (
	"context"
	"crypto/tls"
	stdjson "encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

const (
	modelDeploymentKernelVolumeName = "gpustack-kernels"

	// ModelDeploymentKernelMountPath is the read-only container directory where a role's
	// ModelKernelArtifact is mounted.
	ModelDeploymentKernelMountPath = "/var/lib/gpustack/kernels"

	modelDeploymentKernelArtifactDirEnv = "GPUSTACK_KERNEL_ARTIFACT_DIR"

	modelDeploymentSentinelRuntimeConfigPath = "/gpustack/v1/runtime_config"
	modelDeploymentKernelRuntimeHTTPTimeout  = 5 * time.Second
	modelDeploymentKernelRuntimeMaxBodyBytes = 1 << 20

	// ModelDeploymentConditionKernelsReady reports whether every role's referenced
	// ModelKernelArtifact is resolved, unquarantined, and deliverable.
	ModelDeploymentConditionKernelsReady kubeapistatus.ConditionType = workercore.ModelDeploymentConditionTypeKernelsReady
)

var (
	defaultKernelHTTPClient = &http.Client{
		Timeout: modelDeploymentKernelRuntimeHTTPTimeout,
	}
	defaultKernelInsecureHTTPSClient = &http.Client{
		Timeout: modelDeploymentKernelRuntimeHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true, //nolint:gosec // Pod-local HTTPS listeners use self-signed certificates when --ssl-certfile is set.
			},
		},
	}
)

// ModelDeploymentKernelRender holds the resolved inputs for rendering a role's kernel artifact
// volume and environment.
type ModelDeploymentKernelRender struct {
	Delivery               workercore.ModelDeploymentKernelDelivery `json:"delivery,omitempty"`
	ArtifactName           string                                   `json:"artifactName,omitempty"`
	ArtifactUID            string                                   `json:"artifactUID,omitempty"`
	ManifestDigest         string                                   `json:"manifestDigest,omitempty"`
	DispatchManifestDigest string                                   `json:"dispatchManifestDigest,omitempty"`
	ImageReference         string                                   `json:"imageReference,omitempty"`
	SecretName             string                                   `json:"secretName,omitempty"`
}

func (k *ModelDeploymentKernelRender) compileCacheDigest() string {
	if k == nil {
		return ""
	}
	if d := strings.TrimSpace(k.ManifestDigest); d != "" {
		return d
	}
	if k.Delivery == workercore.ModelDeploymentKernelDeliveryImage && k.ImageReference != "" {
		if hexDigits, ok := modelDeploymentCompileCacheOCIDigest(k.ImageReference); ok {
			return modelDeploymentCompileCacheDigestPrefix + hexDigits
		}
		return modelDeploymentCompileCacheDigestPrefix + stringx.SumBySHA256(k.ImageReference)
	}
	return ""
}

func (k *ModelDeploymentKernelRender) volumes(takeOver bool) ([]core.Volume, []core.VolumeMount) {
	if k == nil || takeOver {
		return nil, nil
	}

	switch k.Delivery {
	case workercore.ModelDeploymentKernelDeliveryNode:
		csi := &core.CSIVolumeSource{
			Driver:   modelstore.DriverName,
			ReadOnly: ptr.To(true),
			VolumeAttributes: map[string]string{
				modelstore.VolumeAttrArtifactKind:   modelstore.ArtifactKindKernel,
				modelstore.VolumeAttrArtifact:       k.ArtifactName,
				modelstore.VolumeAttrArtifactUID:    k.ArtifactUID,
				modelstore.VolumeAttrManifestDigest: k.ManifestDigest,
			},
		}
		if k.SecretName != "" {
			csi.NodePublishSecretRef = &core.LocalObjectReference{Name: k.SecretName}
		}
		return []core.Volume{{
			Name:         modelDeploymentKernelVolumeName,
			VolumeSource: core.VolumeSource{CSI: csi},
		}}, []core.VolumeMount{{
			Name:      modelDeploymentKernelVolumeName,
			MountPath: ModelDeploymentKernelMountPath,
			ReadOnly:  true,
		}}

	case workercore.ModelDeploymentKernelDeliveryImage:
		return []core.Volume{{
			Name: modelDeploymentKernelVolumeName,
			VolumeSource: core.VolumeSource{
				Image: &core.ImageVolumeSource{
					Reference:  k.ImageReference,
					PullPolicy: core.PullIfNotPresent,
				},
			},
		}}, []core.VolumeMount{{
			Name:      modelDeploymentKernelVolumeName,
			MountPath: ModelDeploymentKernelMountPath,
			ReadOnly:  true,
		}}
	}

	return nil, nil
}

func applyModelDeploymentKernelArtifactToContainer(
	c *core.Container, kernel *ModelDeploymentKernelRender, takeOver bool,
) {
	if c == nil || kernel == nil || takeOver {
		return
	}
	c.Env = setModelDeploymentEnv(c.Env, modelDeploymentKernelArtifactDirEnv, ModelDeploymentKernelMountPath)
}

// ModelDeploymentKernelRuntimeRankState captures per-rank kernel loader fields from sentinel
// /gpustack/v1/runtime_config.
type ModelDeploymentKernelRuntimeRankState struct {
	RankID                 int      `json:"rank_id"`
	DispatchManifestDigest string   `json:"dispatch_manifest_digest,omitempty"`
	LoadedKernelKeys       []string `json:"loaded_kernel_keys,omitempty"`
	KernelLoadError        string   `json:"kernel_load_error,omitempty"`
}

// ModelDeploymentKernelRuntimeState captures sentinel /gpustack/v1/runtime_config kernel loader
// state on a serving Pod.
type ModelDeploymentKernelRuntimeState struct {
	BootID                 string                                  `json:"boot_id,omitempty"`
	Ready                  bool                                    `json:"ready,omitempty"`
	Paused                 bool                                    `json:"paused,omitempty"`
	DispatchManifestDigest string                                  `json:"dispatch_manifest_digest,omitempty"`
	LoadedKernelKeys       []string                                `json:"loaded_kernel_keys,omitempty"`
	KernelLoadError        string                                  `json:"kernel_load_error,omitempty"`
	Ranks                  []ModelDeploymentKernelRuntimeRankState `json:"ranks,omitempty"`
}

func (s *ModelDeploymentKernelRuntimeState) normalize() {
	if s == nil {
		return
	}
	if s.DispatchManifestDigest != "" || len(s.LoadedKernelKeys) > 0 || s.KernelLoadError != "" {
		return
	}
	if len(s.Ranks) == 0 {
		return
	}
	var commonKeys map[string]struct{}
	for i := range s.Ranks {
		rk := &s.Ranks[i]
		if rk.KernelLoadError != "" && s.KernelLoadError == "" {
			s.KernelLoadError = rk.KernelLoadError
		}
		if rk.DispatchManifestDigest != "" {
			if s.DispatchManifestDigest == "" {
				s.DispatchManifestDigest = rk.DispatchManifestDigest
			} else if s.DispatchManifestDigest != rk.DispatchManifestDigest && s.KernelLoadError == "" {
				s.KernelLoadError = "rank_dispatch_manifest_mismatch"
			}
		}
		rankSet := make(map[string]struct{}, len(rk.LoadedKernelKeys))
		for _, k := range rk.LoadedKernelKeys {
			if trimmed := strings.TrimSpace(k); trimmed != "" {
				rankSet[trimmed] = struct{}{}
			}
		}
		if i == 0 {
			commonKeys = rankSet
		} else {
			for k := range commonKeys {
				if _, ok := rankSet[k]; !ok {
					delete(commonKeys, k)
				}
			}
		}
	}
	if len(commonKeys) > 0 {
		s.LoadedKernelKeys = make([]string, 0, len(commonKeys))
		for k := range commonKeys {
			s.LoadedKernelKeys = append(s.LoadedKernelKeys, k)
		}
		sort.Strings(s.LoadedKernelKeys)
	}
}

// ModelDeploymentKernelRuntimeReader reads sentinel kernel runtime state from a running Pod.
type ModelDeploymentKernelRuntimeReader func(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pod *core.Pod,
	target runtimecontrol.Target,
) (*ModelDeploymentKernelRuntimeState, error)

type modelDeploymentRoleKernel struct {
	Render  *ModelDeploymentKernelRender
	Status  *workercore.ModelDeploymentRoleKernelArtifactStatus
	Blocked bool
	Reason  string
	Message string
}

type modelDeploymentKernels struct {
	Configured bool
	AnyBlocked bool
	Reason     string
	Message    string
	Roles      map[string]*modelDeploymentRoleKernel
}

func (k *modelDeploymentKernels) forRole(roleName string) *modelDeploymentRoleKernel {
	if k == nil {
		return nil
	}
	return k.Roles[roleName]
}

func (k *modelDeploymentKernels) roleBlocked(roleName string) bool {
	rk := k.forRole(roleName)
	return rk != nil && rk.Blocked
}

func (k *modelDeploymentKernels) dispatchDigestForRole(roleName string) string {
	rk := k.forRole(roleName)
	if rk == nil || rk.Render == nil {
		return ""
	}
	return rk.Render.DispatchManifestDigest
}

func (k *modelDeploymentKernels) recheckNeeded() bool {
	if k == nil || !k.AnyBlocked {
		return false
	}
	return k.Reason == workercore.ModelDeploymentReasonKernelNodeDeliveryUnavailable ||
		k.Reason == workercore.ModelDeploymentReasonKernelPeerSyncDisabled
}

func modelDeploymentHasKernelArtifactRefs(md *workercore.ModelDeployment) bool {
	if md == nil {
		return false
	}
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].KernelArtifactRef != nil && md.Spec.Roles[i].KernelArtifactRef.Name != "" {
			return true
		}
	}
	return false
}

func (r *ModelDeploymentReconciler) resolveModelDeploymentKernels(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) (*modelDeploymentKernels, error) {
	out := &modelDeploymentKernels{}
	if !modelDeploymentHasKernelArtifactRefs(md) {
		return out, nil
	}
	out.Configured = true
	out.Roles = make(map[string]*modelDeploymentRoleKernel, len(md.Spec.Roles))

	reader := r.apiReaderOrClient()
	if reader == nil {
		return out, nil
	}

	var (
		csiChecked   bool
		csiInstalled bool
		storesLoaded bool
		stores       []workercore.NodeModelStore
		storeByName  map[string]*workercore.NodeModelStore
		nodesLoaded  bool
		nodes        []core.Node
		flavorsByAT  map[string][]string
	)

	ensureCSI := func() (bool, error) {
		if csiChecked {
			return csiInstalled, nil
		}
		err := reader.Get(ctx, ctrlcli.ObjectKey{Name: modelstore.DriverName}, new(storage.CSIDriver))
		switch {
		case err == nil:
			csiInstalled = true
		case kerrors.IsNotFound(err):
			csiInstalled = false
		default:
			return false, fmt.Errorf("read CSIDriver %s: %w", modelstore.DriverName, err)
		}
		csiChecked = true
		return csiInstalled, nil
	}

	ensureStores := func() error {
		if storesLoaded {
			return nil
		}
		list := new(workercore.NodeModelStoreList)
		if err := reader.List(ctx, list); err != nil {
			return fmt.Errorf("list node model stores: %w", err)
		}
		stores = list.Items
		storeByName = make(map[string]*workercore.NodeModelStore, len(stores))
		for i := range stores {
			storeByName[stores[i].Name] = &stores[i]
		}
		storesLoaded = true
		return nil
	}

	poolNodesForRole := func(role *workercore.ModelDeploymentRole) ([]string, error) {
		if role.AcceleratorType == "" {
			return nil, nil
		}
		if !nodesLoaded {
			nl := new(core.NodeList)
			if err := reader.List(ctx, nl); err != nil {
				return nil, fmt.Errorf("list nodes: %w", err)
			}
			nodes = nl.Items
			nodesLoaded = true
		}
		if len(nodes) == 0 {
			return nil, nil
		}
		if flavorsByAT == nil {
			flavorsByAT = map[string][]string{}
		}
		if _, seen := flavorsByAT[role.AcceleratorType]; !seen {
			pf := &workercore.ModelPrefetch{
				Spec: workercore.ModelPrefetchSpec{
					Placement: &workercore.ModelPrefetchPlacement{
						AcceleratorTypes: []string{role.AcceleratorType},
					},
				},
			}
			resolved, err := ResolveAcceleratorTypeFlavorNames(ctx, reader, pf, nil)
			if err != nil {
				return nil, err
			}
			flavorsByAT[role.AcceleratorType] = resolved[role.AcceleratorType]
		}
		flavors := flavorsByAT[role.AcceleratorType]
		if len(flavors) == 0 {
			return nil, nil
		}
		pf := &workercore.ModelPrefetch{
			Spec: workercore.ModelPrefetchSpec{
				Placement: &workercore.ModelPrefetchPlacement{
					AcceleratorTypes: []string{role.AcceleratorType},
				},
			},
		}
		matched, err := modelstore.PrefetchTargetNodes(pf, nodes, nil, modelstore.AcceleratorTypeFlavorNames{
			role.AcceleratorType: flavors,
		})
		if err != nil {
			return nil, err
		}
		return matched, nil
	}

	recordBlocked := func(rk *modelDeploymentRoleKernel, reason, msg string) {
		rk.Blocked = true
		rk.Reason = reason
		rk.Message = msg
		if !out.AnyBlocked {
			out.AnyBlocked = true
			out.Reason = reason
			out.Message = msg
		}
	}

	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.KernelArtifactRef == nil || role.KernelArtifactRef.Name == "" {
			continue
		}
		refName := role.KernelArtifactRef.Name
		rk := &modelDeploymentRoleKernel{}
		out.Roles[role.Name] = rk

		mka := new(workercore.ModelKernelArtifact)
		if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: refName}, mka); err != nil {
			if kerrors.IsNotFound(err) {
				recordBlocked(rk, workercore.ModelDeploymentReasonKernelArtifactNotFound, fmt.Sprintf(
					"role %q references ModelKernelArtifact %q, which does not exist in this namespace; new replicas are held while running Pods stay untouched",
					role.Name, refName,
				))
				continue
			}
			return nil, fmt.Errorf("read model kernel artifact %q: %w", refName, err)
		}

		delivery := kernelArtifactDeliveryKind(mka)
		rk.Render = buildModelDeploymentKernelRender(mka, delivery)
		if delivery != "" {
			st := &workercore.ModelDeploymentRoleKernelArtifactStatus{
				Artifact:    mka.Name,
				Delivery:    delivery,
				Quarantined: mka.Status.Quarantined,
			}
			if delivery == workercore.ModelDeploymentKernelDeliveryNode && mka.Status.Resolved != nil {
				st.ManifestDigest = mka.Status.Resolved.ManifestDigest
				st.DispatchManifestDigest = mka.Status.Resolved.DispatchManifestDigest
			} else if mka.Status.Resolved != nil && mka.Status.Resolved.DispatchManifestDigest != "" {
				st.DispatchManifestDigest = mka.Status.Resolved.DispatchManifestDigest
			}
			rk.Status = st
		}

		if mka.Status.Quarantined {
			detail := strings.TrimSpace(mka.Status.QuarantineMessage)
			if detail == "" {
				detail = strings.TrimSpace(mka.Status.QuarantineReason)
			}
			if detail == "" {
				detail = "artifact is quarantined"
			}
			recordBlocked(rk, workercore.ModelDeploymentReasonKernelArtifactQuarantined, fmt.Sprintf(
				"role %q references quarantined ModelKernelArtifact %q (%s); new replicas are held while running Pods stay untouched",
				role.Name, refName, detail,
			))
			continue
		}

		if delivery == "" || !ModelKernelArtifactConditionResolved.IsTrue(mka) || rk.Render == nil {
			detail := ModelKernelArtifactConditionResolved.GetMessage(mka)
			if detail == "" {
				detail = "resolution has not completed"
			}
			recordBlocked(rk, workercore.ModelDeploymentReasonKernelArtifactNotResolved, fmt.Sprintf(
				"role %q references ModelKernelArtifact %q, which is not resolved: %s; new replicas are held while running Pods stay untouched",
				role.Name, refName, detail,
			))
			continue
		}

		if delivery == workercore.ModelDeploymentKernelDeliveryNode {
			installed, err := ensureCSI()
			if err != nil {
				return nil, err
			}
			if !installed {
				recordBlocked(rk, workercore.ModelDeploymentReasonKernelNodeDeliveryUnavailable, fmt.Sprintf(
					"role %q references node-delivered ModelKernelArtifact %q, and CSIDriver %s does not exist; install the model-manager plugin",
					role.Name, refName, modelstore.DriverName,
				))
				continue
			}

			if mka.Spec.Source.NodeStore != nil {
				if err := ensureStores(); err != nil {
					return nil, err
				}
				poolNodes, err := poolNodesForRole(role)
				if err != nil {
					return nil, err
				}
				if disabledNode := findPeerSyncDisabledConsumerNode(
					ctx, role, pods, mka, stores, storeByName, poolNodes,
				); disabledNode != "" {
					recordBlocked(rk, workercore.ModelDeploymentReasonKernelPeerSyncDisabled, fmt.Sprintf(
						"role %q references nodeStore ModelKernelArtifact %q, and Peer Sync is disabled on consumer node %q",
						role.Name, refName, disabledNode,
					))
					continue
				}
			}
		}
	}

	return out, nil
}

func kernelArtifactDeliveryKind(mka *workercore.ModelKernelArtifact) workercore.ModelDeploymentKernelDelivery {
	if mka == nil || countKernelSources(&mka.Spec.Source) != 1 {
		return ""
	}
	switch {
	case mka.Spec.Source.NodeStore != nil, mka.Spec.Source.HuggingFace != nil:
		return workercore.ModelDeploymentKernelDeliveryNode
	case mka.Spec.Source.Image != nil:
		return workercore.ModelDeploymentKernelDeliveryImage
	default:
		return ""
	}
}

func buildModelDeploymentKernelRender(
	mka *workercore.ModelKernelArtifact, delivery workercore.ModelDeploymentKernelDelivery,
) *ModelDeploymentKernelRender {
	if mka == nil || delivery == "" {
		return nil
	}
	switch delivery {
	case workercore.ModelDeploymentKernelDeliveryImage:
		if mka.Spec.Source.Image == nil || strings.TrimSpace(mka.Spec.Source.Image.Reference) == "" {
			return nil
		}
		return &ModelDeploymentKernelRender{
			Delivery:       workercore.ModelDeploymentKernelDeliveryImage,
			ArtifactName:   mka.Name,
			ArtifactUID:    string(mka.UID),
			ImageReference: mka.Spec.Source.Image.Reference,
		}
	case workercore.ModelDeploymentKernelDeliveryNode:
		if mka.Status.Resolved == nil || strings.TrimSpace(mka.Status.Resolved.ManifestDigest) == "" {
			return nil
		}
		var secretName string
		if mka.Spec.Source.HuggingFace != nil && mka.Spec.Source.HuggingFace.SecretRef != nil {
			secretName = mka.Spec.Source.HuggingFace.SecretRef.Name
		}
		return &ModelDeploymentKernelRender{
			Delivery:               workercore.ModelDeploymentKernelDeliveryNode,
			ArtifactName:           mka.Name,
			ArtifactUID:            string(mka.UID),
			ManifestDigest:         mka.Status.Resolved.ManifestDigest,
			DispatchManifestDigest: mka.Status.Resolved.DispatchManifestDigest,
			SecretName:             secretName,
		}
	default:
		return nil
	}
}

func nodeModelStorePeerSyncEnabled(ctx context.Context, nms *workercore.NodeModelStore) bool {
	if nms != nil && nms.Spec.PeerSyncEnabled != nil {
		return *nms.Spec.PeerSyncEnabled
	}
	return settings.ModelStorePeerSync.ShouldValueBool(ctx)
}

func findPeerSyncDisabledConsumerNode(
	ctx context.Context,
	role *workercore.ModelDeploymentRole,
	pods []core.Pod,
	mka *workercore.ModelKernelArtifact,
	stores []workercore.NodeModelStore,
	storeByName map[string]*workercore.NodeModelStore,
	poolNodes []string,
) string {
	if mka == nil || mka.Spec.Source.NodeStore == nil || mka.Status.Resolved == nil {
		return ""
	}
	digest := mka.Status.Resolved.ManifestDigest
	seedNode := mka.Spec.Source.NodeStore.SeedNode

	readyNodes := map[string]bool{}
	if len(stores) == 0 && seedNode != "" {
		readyNodes[seedNode] = true
	}
	for i := range stores {
		nms := &stores[i]
		for j := range nms.Status.Models {
			if nms.Status.Models[j].Digest == digest &&
				nms.Status.Models[j].State == workercore.NodeModelStoreModelStateReady {
				readyNodes[nms.Name] = true
				break
			}
		}
	}

	candidateSet := map[string]struct{}{}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Status.Phase == core.PodFailed || p.Status.Phase == core.PodSucceeded {
			continue
		}
		if modelDeploymentPodRole(p) == role.Name && p.Spec.NodeName != "" {
			candidateSet[p.Spec.NodeName] = struct{}{}
		}
	}
	if len(poolNodes) > 0 {
		for _, n := range poolNodes {
			candidateSet[n] = struct{}{}
		}
	} else {
		for i := range stores {
			candidateSet[stores[i].Name] = struct{}{}
		}
	}

	candidates := sortedSetKeys(candidateSet)
	for _, nodeName := range candidates {
		if readyNodes[nodeName] {
			continue
		}
		if !nodeModelStorePeerSyncEnabled(ctx, storeByName[nodeName]) {
			return nodeName
		}
	}
	return ""
}

func (r *ModelDeploymentReconciler) observeModelDeploymentKernels(
	ctx context.Context,
	md *workercore.ModelDeployment,
	pods []core.Pod,
	holder *workercore.ModelDeployment,
) error {
	if !modelDeploymentHasKernelArtifactRefs(md) {
		for i := range holder.Status.Roles {
			holder.Status.Roles[i].KernelArtifact = nil
		}
		ModelDeploymentConditionKernelsReady.True(
			holder,
			workercore.ModelDeploymentReasonKernelsNotApplicable,
			"the deployment names no kernel artifact",
		)
		return nil
	}

	kernels, err := r.resolveModelDeploymentKernels(ctx, md, pods)
	if err != nil {
		return err
	}

	roleByName := make(map[string]*workercore.ModelDeploymentRole, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		roleByName[md.Spec.Roles[i].Name] = &md.Spec.Roles[i]
	}

	for i := range holder.Status.Roles {
		roleName := holder.Status.Roles[i].Name
		rk := kernels.forRole(roleName)
		if rk == nil || rk.Status == nil {
			holder.Status.Roles[i].KernelArtifact = nil
			continue
		}
		st := rk.Status.DeepCopy()
		if role := roleByName[roleName]; role != nil && rk.Render != nil && !rk.Blocked {
			loaded, observedDigest := r.observeRoleLoadedKernels(ctx, md, role, rk.Render, pods)
			st.LoadedKernels = loaded
			if st.DispatchManifestDigest == "" && observedDigest != "" {
				st.DispatchManifestDigest = observedDigest
			}
		}
		holder.Status.Roles[i].KernelArtifact = st
	}

	if kernels.AnyBlocked {
		ModelDeploymentConditionKernelsReady.False(holder, kernels.Reason, kernels.Message)
		return nil
	}

	ModelDeploymentConditionKernelsReady.True(
		holder,
		workercore.ModelDeploymentReasonKernelsReady,
		"every referenced kernel artifact is resolved and deliverable",
	)
	return nil
}

func (r *ModelDeploymentReconciler) observeRoleLoadedKernels(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	render *ModelDeploymentKernelRender,
	pods []core.Pod,
) (loadedKernels int32, observedDispatchDigest string) {
	if !ModelDeploymentRoleSupportsEngineSentinel(md, role) && !roleHasRuntimeControlPod(role.Name, pods) {
		return 0, ""
	}
	groups := resolveRoleAnsweringGroups(md, role, pods, nil)
	if len(groups) == 0 {
		return 0, ""
	}

	var commonKeys map[string]struct{}
	evaluatedInstances := 0
	for _, g := range groups {
		if len(g.allMembers) == 0 || len(g.runningPods) < g.requiredCount {
			continue
		}
		allReadyAndMounted := true
		for _, m := range g.allMembers {
			if !podIsReady(m) || !podMountsKernelRender(m, render) {
				allReadyAndMounted = false
				break
			}
		}
		if !allReadyAndMounted {
			continue
		}

		instanceValid := len(g.runningPods) > 0
		var instanceKeys map[string]struct{}
		for idx, pod := range g.runningPods {
			if !r.canReadPodKernelRuntime(pod) {
				instanceValid = false
				break
			}
			state, err := r.readPodKernelRuntimeState(ctx, md, role, pod)
			if err != nil || state == nil || state.KernelLoadError != "" {
				instanceValid = false
				break
			}
			if render.DispatchManifestDigest != "" && state.DispatchManifestDigest != render.DispatchManifestDigest {
				instanceValid = false
				break
			}
			if observedDispatchDigest == "" && state.DispatchManifestDigest != "" {
				observedDispatchDigest = state.DispatchManifestDigest
			}
			podKeys := make(map[string]struct{}, len(state.LoadedKernelKeys))
			for _, k := range state.LoadedKernelKeys {
				if trimmed := strings.TrimSpace(k); trimmed != "" {
					podKeys[trimmed] = struct{}{}
				}
			}
			if idx == 0 {
				instanceKeys = podKeys
			} else {
				for k := range instanceKeys {
					if _, ok := podKeys[k]; !ok {
						delete(instanceKeys, k)
					}
				}
			}
		}
		if !instanceValid {
			return 0, ""
		}
		if evaluatedInstances == 0 {
			commonKeys = instanceKeys
		} else {
			for k := range commonKeys {
				if _, ok := instanceKeys[k]; !ok {
					delete(commonKeys, k)
				}
			}
		}
		evaluatedInstances++
	}
	if evaluatedInstances == 0 {
		return 0, ""
	}
	return int32(len(commonKeys)), observedDispatchDigest
}

func podMountsKernelRender(pod *core.Pod, render *ModelDeploymentKernelRender) bool {
	src := modelDeploymentPodKernelVolumeSource(pod)
	if src == nil || render == nil {
		return false
	}
	switch render.Delivery {
	case workercore.ModelDeploymentKernelDeliveryNode:
		return src.CSI != nil &&
			src.CSI.VolumeAttributes[modelstore.VolumeAttrArtifact] == render.ArtifactName &&
			src.CSI.VolumeAttributes[modelstore.VolumeAttrManifestDigest] == render.ManifestDigest
	case workercore.ModelDeploymentKernelDeliveryImage:
		return src.Image != nil && src.Image.Reference == render.ImageReference
	default:
		return false
	}
}

func (r *ModelDeploymentReconciler) canReadPodKernelRuntime(pod *core.Pod) bool {
	if r.KernelRuntimeReader != nil || r.runtimeControlTarget != nil {
		return true
	}
	return pod != nil && strings.TrimSpace(pod.Status.PodIP) != ""
}

func (r *ModelDeploymentReconciler) readPodKernelRuntimeState(
	ctx context.Context,
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	pod *core.Pod,
) (*ModelDeploymentKernelRuntimeState, error) {
	target, err := r.resolveRuntimeControlTarget(ctx, md, role, pod)
	if err != nil {
		return nil, err
	}
	if r.KernelRuntimeReader != nil {
		state, readErr := r.KernelRuntimeReader(ctx, md, role, pod, target)
		if readErr != nil {
			return nil, readErr
		}
		if state != nil {
			state.normalize()
		}
		return state, nil
	}

	host := strings.TrimSpace(target.Host)
	if host == "" {
		return nil, fmt.Errorf("pod %s has no IP address for sentinel runtime_config check", pod.Name)
	}
	if target.Port <= 0 {
		return nil, fmt.Errorf("pod %s has invalid sentinel port %d", pod.Name, target.Port)
	}
	scheme := strings.ToLower(strings.TrimSpace(target.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	u := &url.URL{
		Scheme: scheme,
		Host:   fmt.Sprintf("%s:%d", host, target.Port),
		Path:   modelDeploymentSentinelRuntimeConfigPath,
	}

	reqCtx, cancel := context.WithTimeout(ctx, modelDeploymentKernelRuntimeHTTPTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")

	httpClient := defaultKernelHTTPClient
	if scheme == "https" && target.InsecureSkipVerify {
		httpClient = defaultKernelInsecureHTTPSClient
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, modelDeploymentKernelRuntimeMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s returned HTTP %d", modelDeploymentSentinelRuntimeConfigPath, resp.StatusCode)
	}

	var state ModelDeploymentKernelRuntimeState
	if err := stdjson.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", modelDeploymentSentinelRuntimeConfigPath, err)
	}
	state.normalize()
	return &state, nil
}

func modelDeploymentPodKernelVolumeSource(pod *core.Pod) *core.VolumeSource {
	if pod == nil {
		return nil
	}
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == modelDeploymentKernelVolumeName {
			cp := pod.Spec.Volumes[i].VolumeSource.DeepCopy()
			return cp
		}
	}
	return nil
}

func replacementHasKernelDelta(departing, want []*core.Pod) bool {
	var oldSrc, newSrc *core.VolumeSource
	if len(departing) > 0 {
		oldSrc = modelDeploymentPodKernelVolumeSource(departing[0])
	}
	if len(want) > 0 {
		newSrc = modelDeploymentPodKernelVolumeSource(want[0])
	}
	return !kubemeta.DeepEqual(oldSrc, newSrc)
}

func (r *ModelDeploymentReconciler) populateReplacementSlotKernelGate(
	slot *modelDeploymentReplacementSlot,
	departing, want []*core.Pod,
	kernels *modelDeploymentKernels,
) {
	if slot == nil {
		return
	}
	if !replacementHasKernelDelta(departing, want) {
		return
	}
	slot.RequireReadiness = true
	if len(want) > 0 && modelDeploymentPodKernelVolumeSource(want[0]) != nil {
		slot.DispatchManifestDigest = kernels.dispatchDigestForRole(slot.Role)
	} else {
		slot.DispatchManifestDigest = ""
	}
}

func (r *ModelDeploymentReconciler) replacementKernelGateOnSupersede(
	ctx context.Context,
	md *workercore.ModelDeployment,
	slot modelDeploymentReplacementSlot,
	standing, want []*core.Pod,
) (requireReadiness bool, dispatchDigest string) {
	wantHasKernel := len(want) > 0 && modelDeploymentPodKernelVolumeSource(want[0]) != nil
	delta := replacementHasKernelDelta(standing, want)
	requireReadiness = slot.RequireReadiness || delta || wantHasKernel
	if !requireReadiness || !wantHasKernel {
		return requireReadiness, ""
	}
	kernels, err := r.resolveModelDeploymentKernels(ctx, md, nil)
	if err != nil {
		return requireReadiness, slot.DispatchManifestDigest
	}
	return requireReadiness, kernels.dispatchDigestForRole(slot.Role)
}

func answeringMembersOfStanding(
	md *workercore.ModelDeployment,
	role *workercore.ModelDeploymentRole,
	standing []*core.Pod,
) []*core.Pod {
	if len(standing) <= 1 {
		return standing
	}
	view := modelDeploymentReplicaView{
		Role:    role.Name,
		Seated:  true,
		Members: standing,
	}
	switch modelDeploymentDeployedAnsweringShape(view, md.Spec.Engine.Name) {
	case modelDeploymentAnsweringAll:
		return standing
	case modelDeploymentAnsweringSole:
		return standing[:1]
	case modelDeploymentAnsweringLeader:
		var leaders []*core.Pod
		for _, m := range standing {
			if modelDeploymentAnsweringMemberLeader(m) {
				leaders = append(leaders, m)
			}
		}
		if len(leaders) > 0 {
			return leaders
		}
	default:
		if modelDeploymentRoleExternalDP(md, role) {
			return standing
		}
		for _, m := range standing {
			if modelDeploymentAnsweringMemberLeader(m) {
				return []*core.Pod{m}
			}
		}
	}
	return standing[:1]
}

func (r *ModelDeploymentReconciler) verifyCanaryReplacementKernels(
	ctx context.Context,
	md *workercore.ModelDeployment,
	slot modelDeploymentReplacementSlot,
	standing []*core.Pod,
) string {
	for _, member := range standing {
		if !podIsReady(member) {
			return fmt.Sprintf("canary member %s is not Ready yet", member.Name)
		}
	}
	if slot.DispatchManifestDigest == "" {
		return ""
	}
	role := replacementRoleByName(md, slot.Role)
	if role == nil {
		return ""
	}
	sentinelEnabled := ModelDeploymentRoleSupportsEngineSentinel(md, role) ||
		slices.ContainsFunc(standing, modelDeploymentPodHasRuntimeControlEnable)
	if !sentinelEnabled {
		return ""
	}

	for _, member := range answeringMembersOfStanding(md, role, standing) {
		state, err := r.readPodKernelRuntimeState(ctx, md, role, member)
		if err != nil {
			return fmt.Sprintf("canary member %s sentinel runtime_config is not readable yet: %v", member.Name, err)
		}
		if state == nil {
			return fmt.Sprintf("canary member %s returned empty sentinel runtime_config", member.Name)
		}
		if state.KernelLoadError != "" {
			return fmt.Sprintf("canary member %s reported kernel_load_error %q", member.Name, state.KernelLoadError)
		}
		if state.DispatchManifestDigest != slot.DispatchManifestDigest {
			return fmt.Sprintf(
				"canary member %s dispatch_manifest_digest is %q, want %q",
				member.Name, state.DispatchManifestDigest, slot.DispatchManifestDigest,
			)
		}
		if len(state.LoadedKernelKeys) == 0 {
			return fmt.Sprintf("canary member %s has not confirmed loaded_kernel_keys yet", member.Name)
		}
	}

	return ""
}

func (r *ModelDeploymentReconciler) mapModelDeploymentKernelArtifact(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	if obj == nil {
		return nil
	}
	mdList := new(workercore.ModelDeploymentList)
	if err := r.Client.List(
		ctx, mdList, ctrlcli.InNamespace(obj.GetNamespace()), ctrlclix.WithoutQuorum,
	); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model deployments for model kernel artifact",
			"model kernel artifact", ctrlcli.ObjectKeyFromObject(obj))
		return nil
	}

	var reqs []ctrlreconcile.Request
	for i := range mdList.Items {
		md := &mdList.Items[i]
		for j := range md.Spec.Roles {
			ref := md.Spec.Roles[j].KernelArtifactRef
			if ref == nil || ref.Name != obj.GetName() {
				continue
			}
			reqs = append(reqs, ctrlreconcile.Request{
				NamespacedName: ctrlcli.ObjectKeyFromObject(md),
			})
			break
		}
	}
	return reqs
}

func (r *ModelDeploymentReconciler) mapModelDeploymentDeliveryWithKernels(
	ctx context.Context, obj ctrlcli.Object,
) ([]ctrlreconcile.Request, error) {
	reqs, err := r.mapModelDeploymentDelivery(ctx, obj)
	if err != nil {
		return nil, err
	}
	mds := new(workercore.ModelDeploymentList)
	if err := r.APIReader.List(ctx, mds); err != nil {
		return nil, fmt.Errorf("list model deployments for kernel delivery change: %w", err)
	}
	for i := range mds.Items {
		if !modelDeploymentHasKernelArtifactRefs(&mds.Items[i]) {
			continue
		}
		req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&mds.Items[i])}
		if !slices.Contains(reqs, req) {
			reqs = append(reqs, req)
		}
	}
	return reqs, nil
}

func (r *ModelDeploymentReconciler) mapModelDeploymentNodeModelStoreWithKernels(
	ctx context.Context, obj ctrlcli.Object,
) []ctrlreconcile.Request {
	reqs := r.mapModelDeploymentNodeModelStore(ctx, obj)
	if _, ok := obj.(*workercore.NodeModelStore); !ok {
		return reqs
	}
	mds := new(workercore.ModelDeploymentList)
	if err := r.Client.List(ctx, mds); err != nil {
		ctrllog.FromContext(ctx).Error(err, "list model deployments for kernel node model store change")
		return reqs
	}
	for i := range mds.Items {
		if !modelDeploymentHasKernelArtifactRefs(&mds.Items[i]) {
			continue
		}
		req := ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKeyFromObject(&mds.Items[i])}
		if !slices.Contains(reqs, req) {
			reqs = append(reqs, req)
		}
	}
	return reqs
}

func nodeModelStoreModelsOrPeerSyncChanged() ctrlpredicate.Predicate {
	base := nodeModelStoreModelsChanged()
	return ctrlpredicate.Funcs{
		UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
			if base.Update(e) {
				return true
			}
			o, okOld := e.ObjectOld.(*workercore.NodeModelStore)
			n, okNew := e.ObjectNew.(*workercore.NodeModelStore)
			if !okOld || !okNew {
				return false
			}
			return !kubemeta.DeepEqual(o.Spec.PeerSyncEnabled, n.Spec.PeerSyncEnabled)
		},
	}
}
