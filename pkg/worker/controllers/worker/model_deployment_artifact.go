package worker

import (
	"net/url"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelstore"
)

const (
	// modelDeploymentModelVolumeName and modelDeploymentModelCacheVolumeName name the weights'
	// volumes. They share no prefix with the role's own "additional-<n>" volumes or the connector's.
	modelDeploymentModelVolumeName      = "gpustack-model"
	modelDeploymentModelCacheVolumeName = "gpustack-model-cache"

	// modelDeploymentRevisionArg pins a hub download, weights and tokenizer alike: both supported
	// engines load the tokenizer at the same revision unless told otherwise, which is owned.
	modelDeploymentRevisionArg = "--revision"

	modelDeploymentHFHomeEnv     = "HF_HOME"
	modelDeploymentHFEndpointEnv = "HF_ENDPOINT"
	modelDeploymentHFTokenEnv    = "HF_TOKEN"
	modelDeploymentHTTPSProxyEnv = "HTTPS_PROXY"
	modelDeploymentNoProxyEnv    = "NO_PROXY"

	modelDeploymentModelScopeCacheEnv  = "MODELSCOPE_CACHE"
	modelDeploymentModelScopeDomainEnv = "MODELSCOPE_DOMAIN"
	modelDeploymentModelScopeTokenEnv  = "MODELSCOPE_API_TOKEN"
	// The engines' switches that route their own download through their bundled ModelScope SDK
	// (measured, PoC-C): each engine reads only its own.
	modelDeploymentVLLMModelScopeSwitch   = "VLLM_USE_MODELSCOPE"
	modelDeploymentSGLangModelScopeSwitch = "SGLANG_USE_MODELSCOPE"
)

// The hub kinds a hub artifact's download environment is rendered for. An engine downloads from
// one hub, and the names it reads differ by hub.
const (
	modelDeploymentHubHuggingFace = "huggingFace"
	modelDeploymentHubModelScope  = "modelscope"
)

// modelDeploymentCacheHeadroomMin is the least headroom an engine's download cache gets above the
// manifest's size, and modelDeploymentCacheHeadroomDivisor the fraction it otherwise gets. The
// headroom covers what a download writes beside the files: partial files and the hub client's own
// bookkeeping.
const (
	modelDeploymentCacheHeadroomMin     = 1 << 30
	modelDeploymentCacheHeadroomDivisor = 10
)

// ModelDeploymentArtifactRender is how a deployment's weights reach its engine, resolved from the
// ModelArtifact by the reconciler and handed to the render as values, so the render stays pure.
//
// A nil one is a deployment that names no artifact, which renders exactly what it rendered before
// the field existed.
type ModelDeploymentArtifactRender struct {
	// Delivery is PVC, Engine, Node, Image or Remote.
	Delivery workercore.ModelDeploymentModelDelivery

	// ModelTransferFallback is the declared identity-preserving fallback of a Remote
	// delivery: Node or Engine, per the cluster's delivery mode, with the
	// verified-node-only restriction applied. Empty for every other
	// delivery.
	ModelTransferFallback workercore.ModelDeploymentModelDelivery

	// ModelTransferPolicy is the cluster policy the Remote delivery was resolved
	// under, echoed into the deployment status's remote view.
	ModelTransferPolicy string

	// ModelTransferBackend is the transfer backend the role's pool qualifies for,
	// stamped per role at render (nixl on NVIDIA, hixl on Ascend) and read
	// by the adapter through the environment. It is a hardware fact, not a
	// spec fact, so it never takes part in a Pod's identity hash.
	ModelTransferBackend string

	// ModelTransferEFA reports that AWS EFA (vpc.amazonaws.com/efa) is the fabric the role's
	// transfer legs resolved to, stamped per role beside ModelTransferBackend. On the nixl
	// backend it selects the LIBFABRIC transport plugin over UCX; every other backend reads
	// nothing from it.
	ModelTransferEFA bool
	// ModelTransferConfigMap holds public source endpoints and trust anchors in
	// the workload namespace. Its name stays stable as source Pods change.
	ModelTransferConfigMap string

	// SourceReady says a prepared source — a node's source runtime serving
	// a generation for the manifest digest — was seen on the pass that
	// resolved this render. It is the observation the status publishes and
	// the gate a Prefer deployment reads; under Require it is information,
	// never permission.
	SourceReady bool

	// VerifiedNodeOnly says the artifact can only be delivered honestly by
	// the node: it carries an expected digest or file patterns, which an
	// engine download can neither anchor-verify nor filter. A Remote
	// delivery over such an artifact falls back to Node and never weakens
	// to an engine download.
	VerifiedNodeOnly bool

	// ArtifactName, ArtifactUID and ManifestDigest are what a node-delivered volume names, as hints
	// the node's plugin checks against the API before it mounts anything.
	ArtifactName   string
	ArtifactUID    string
	ManifestDigest string

	// ClaimName and Path are a claim artifact's claim and the directory inside it.
	ClaimName string
	Path      string

	// ImageReference is an image artifact's digest-pinned reference, rendered as a Kubernetes image
	// volume that kubelet pulls and mounts read-only.
	ImageReference string

	// Repository and Revision are a hub artifact's repository and resolved commit, SecretName the
	// Secret holding its token or "", and SizeBytes its manifest's total size.
	Repository string
	Revision   string
	SecretName string
	SizeBytes  int64

	// Hub is which hub a hub artifact downloads from, one of the modelDeploymentHub kinds. The
	// download environment's names differ by hub; a non-hub delivery renders none.
	Hub string

	// Endpoint, HTTPSProxy and NoProxy are the administrator's Settings, read by the reconciler.
	Endpoint   string
	HTTPSProxy string
	NoProxy    string
}

// model is what the engine command names as the model: the fixed mount path for a claim, a
// node-delivered artifact, or a remote metadata view; the repository for an engine download.
func (a *ModelDeploymentArtifactRender) model() string {
	if a.Delivery == workercore.ModelDeploymentModelDeliveryEngine {
		return a.Repository
	}

	return ModelDeploymentModelMountPath
}

// args are the owned arguments that follow the engine's base command. A remote delivery pins no
// revision — its view is addressed by the manifest digest — and its load format is applied with
// the container decorations, not here.
func (a *ModelDeploymentArtifactRender) args() []string {
	if a.Delivery != workercore.ModelDeploymentModelDeliveryEngine {
		return nil
	}

	return []string{modelDeploymentRevisionArg, a.Revision}
}

// volumes are the weights' volumes and the main container's mounts of them. A role that replaced
// its command line gets the claim, and nothing for an engine download: its command's author
// downloads the weights. A remote delivery mounts the metadata-only view, the broker token, and
// the staged wheel directory.
func (a *ModelDeploymentArtifactRender) volumes(takeOver bool) ([]core.Volume, []core.VolumeMount) {
	switch {
	case a.Delivery == workercore.ModelDeploymentModelDeliveryRemote:
		return a.modelTransferVolumes()
	case a.Delivery == workercore.ModelDeploymentModelDeliveryNode:
		return []core.Volume{{
			Name: modelDeploymentModelVolumeName, VolumeSource: a.nodeVolumeSource(),
		}}, []core.VolumeMount{{
			Name: modelDeploymentModelVolumeName, MountPath: ModelDeploymentModelMountPath, ReadOnly: true,
		}}
	case a.Delivery == workercore.ModelDeploymentModelDeliveryPVC:
		return []core.Volume{{
			Name: modelDeploymentModelVolumeName,
			VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
				ClaimName: a.ClaimName, ReadOnly: true,
			}},
		}}, []core.VolumeMount{{
			Name: modelDeploymentModelVolumeName, MountPath: ModelDeploymentModelMountPath,
			SubPath: a.Path, ReadOnly: true,
		}}
	case a.Delivery == workercore.ModelDeploymentModelDeliveryImage:
		// The pull policy is left at its default: a digest-pinned reference defaults to
		// IfNotPresent, so a node that already holds the image does not fetch it again. No cache
		// volume, no download environment: kubelet's own image store is the whole delivery.
		return []core.Volume{{
			Name: modelDeploymentModelVolumeName,
			VolumeSource: core.VolumeSource{Image: &core.ImageVolumeSource{
				Reference: a.ImageReference,
			}},
		}}, []core.VolumeMount{{
			Name: modelDeploymentModelVolumeName, MountPath: ModelDeploymentModelMountPath, ReadOnly: true,
		}}
	case takeOver:
		return nil, nil
	default:
		limit := a.cacheSize()
		return []core.Volume{{
			Name: modelDeploymentModelCacheVolumeName,
			VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{
				SizeLimit: &limit,
			}},
		}}, []core.VolumeMount{{
			Name: modelDeploymentModelCacheVolumeName, MountPath: ModelDeploymentModelCachePath,
		}}
	}
}

// nodeVolumeSource is the node plugin's inline volume. Its attributes are hints the plugin checks
// against a resolved artifact in the Pod's own namespace; the Secret reference hands the artifact's
// token to the plugin through kubelet, never through the Pod's environment.
func (a *ModelDeploymentArtifactRender) nodeVolumeSource() core.VolumeSource {
	csi := &core.CSIVolumeSource{
		Driver:   modelstore.DriverName,
		ReadOnly: ptr.To(true),
		VolumeAttributes: map[string]string{
			modelArtifactVolumeAttrArtifact:    a.ArtifactName,
			modelArtifactVolumeAttrArtifactUID: a.ArtifactUID,
			modelArtifactVolumeAttrDigest:      a.ManifestDigest,
		},
	}
	if a.SecretName != "" {
		csi.NodePublishSecretRef = &core.LocalObjectReference{Name: a.SecretName}
	}

	return core.VolumeSource{CSI: csi}
}

// The volume attributes a node-delivered volume names, the keys the node plugin reads.
const (
	modelArtifactVolumeAttrArtifact    = modelstore.VolumeAttrArtifact
	modelArtifactVolumeAttrArtifactUID = modelstore.VolumeAttrArtifactUID
	modelArtifactVolumeAttrDigest      = modelstore.VolumeAttrManifestDigest
)

// cacheSize is an engine download's cache limit: the manifest's size and a headroom of a tenth,
// at least modelDeploymentCacheHeadroomMin. The manifest is the whole commit, and an engine
// downloads a subset of it, so this bounds any download from above.
func (a *ModelDeploymentArtifactRender) cacheSize() resource.Quantity {
	headroom := max(a.SizeBytes/modelDeploymentCacheHeadroomDivisor, modelDeploymentCacheHeadroomMin)

	return *resource.NewQuantity(a.SizeBytes+headroom, resource.BinarySI)
}

// env returns the environment an engine download needs: the owned entries, then the defaulted
// ones a role's own value replaces. A claim needs none, and a take-over role gets none.
func (a *ModelDeploymentArtifactRender) env(takeOver bool, engine string) (owned, defaulted []core.EnvVar) {
	if takeOver || a.Delivery != workercore.ModelDeploymentModelDeliveryEngine {
		return nil, nil
	}

	switch a.Hub {
	case modelDeploymentHubModelScope:
		owned = []core.EnvVar{
			{Name: modelDeploymentModelScopeCacheEnv, Value: ModelDeploymentModelCachePath},
			// The SDK takes a bare host and prefixes the scheme itself (measured in the runner's
			// own SDK); the Setting keeps the full-URL shape the Hugging Face endpoint uses.
			{Name: modelDeploymentModelScopeDomainEnv, Value: endpointHost(a.Endpoint)},
		}
		if sw := modelScopeSwitch(engine); sw != "" {
			owned = append(owned, core.EnvVar{Name: sw, Value: "true"})
		}
		if a.SecretName != "" {
			owned = append(owned, core.EnvVar{Name: modelDeploymentModelScopeTokenEnv, ValueFrom: &core.EnvVarSource{
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{Name: a.SecretName},
					Key:                  modelArtifactTokenKey,
				},
			}})
		}
	default:
		owned = []core.EnvVar{
			{Name: modelDeploymentHFHomeEnv, Value: ModelDeploymentModelCachePath},
			{Name: modelDeploymentHFEndpointEnv, Value: a.Endpoint},
		}
		if a.SecretName != "" {
			owned = append(owned, core.EnvVar{Name: modelDeploymentHFTokenEnv, ValueFrom: &core.EnvVarSource{
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{Name: a.SecretName},
					Key:                  modelArtifactTokenKey,
				},
			}})
		}
	}
	if a.HTTPSProxy != "" {
		defaulted = append(defaulted, core.EnvVar{Name: modelDeploymentHTTPSProxyEnv, Value: a.HTTPSProxy})
	}
	if a.NoProxy != "" {
		defaulted = append(defaulted, core.EnvVar{Name: modelDeploymentNoProxyEnv, Value: a.NoProxy})
	}

	return owned, defaulted
}

// modelScopeSwitch is the environment name that routes an engine's own download through its
// bundled ModelScope SDK. An engine the table does not know reads neither switch, so none is
// rendered for it.
func modelScopeSwitch(engine string) string {
	switch engine {
	case workercore.ModelDeploymentEngineVLLM:
		return modelDeploymentVLLMModelScopeSwitch
	case workercore.ModelDeploymentEngineSGLang:
		return modelDeploymentSGLangModelScopeSwitch
	default:
		return ""
	}
}

// endpointHost is the URL's bare host, for the one consumer that takes a host rather than a URL.
// A value that does not parse rides as it is: the admission that passed it has already demanded a
// schema and a host.
func endpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}

	return u.Host
}

// raiseEphemeralStorageLimit adds an engine download's cache limit to the main container's
// ephemeral-storage limit, leaving the request alone.
//
// kubelet counts an emptyDir's usage toward the Pod's ephemeral-storage limit, the sum of its
// containers' limits, as well as toward the volume's own sizeLimit. The container's limit is the
// role's local storage, so a model larger than that would be evicted mid-download however
// large the volume's limit. The request is what Kueue and the scheduler account, and it stays what
// the role's resources say.
func (a *ModelDeploymentArtifactRender) raiseEphemeralStorageLimit(c *core.Container, takeOver bool) {
	if takeOver || a.Delivery != workercore.ModelDeploymentModelDeliveryEngine {
		return
	}
	// A container with no limit has no ceiling to raise, and adding one would create a cap.
	limit, ok := c.Resources.Limits[core.ResourceEphemeralStorage]
	if !ok {
		return
	}
	limit.Add(a.cacheSize())
	c.Resources.Limits[core.ResourceEphemeralStorage] = limit
}
