package driver

import (
	"fmt"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
)

// The volume attribute keys. kubelet adds the csi.storage.k8s.io ones because the CSIDriver sets
// podInfoOnMount, over whatever the Pod wrote under the same keys; the others come from the Pod's
// volumeAttributes and are only hints.
const (
	attrPodNamespace = "csi.storage.k8s.io/pod.namespace"
	attrPodName      = "csi.storage.k8s.io/pod.name"
	attrPodUID       = "csi.storage.k8s.io/pod.uid"
	attrEphemeral    = "csi.storage.k8s.io/ephemeral"

	// AttrArtifactKind, AttrArtifact, AttrArtifactUID and AttrManifestDigest are what a consumer's
	// volume names.
	AttrArtifactKind   = modelstore.VolumeAttrArtifactKind
	AttrArtifact       = modelstore.VolumeAttrArtifact
	AttrArtifactUID    = modelstore.VolumeAttrArtifactUID
	AttrManifestDigest = modelstore.VolumeAttrManifestDigest
)

// The authorization rules, named in a refusal.
const (
	ruleArtifactMissing       = "the Pod's namespace has no such ModelArtifact"
	ruleUIDMismatch           = "the ModelArtifact is not the one the volume names"
	ruleNotHub                = "the ModelArtifact is not on a model hub"
	ruleNotResolved           = "the ModelArtifact is not resolved"
	ruleDigestMismatch        = "the ModelArtifact resolves to another digest"
	ruleModelClassMismatch    = "the published tree is not a model artifact"
	unauthorizedModelTemplate = "%s: ModelArtifact %s/%s: %s"

	ruleKernelArtifactMissing   = "the Pod's namespace has no such ModelKernelArtifact"
	ruleKernelUIDMismatch       = "the ModelKernelArtifact is not the one the volume names"
	ruleKernelUnsupportedSource = "the ModelKernelArtifact is not node-delivered"
	ruleKernelQuarantined       = "the ModelKernelArtifact is quarantined"
	ruleKernelNotResolved       = "the ModelKernelArtifact is not resolved"
	ruleKernelDigestMismatch    = "the ModelKernelArtifact resolves to another digest"
	ruleKernelClassMismatch     = "the published tree is not a kernel artifact"
	unauthorizedKernelTemplate  = "%s: ModelKernelArtifact %s/%s: %s"

	resolvedCondition   = "Resolved"
	conditionStatusTrue = "True"
)

// mountAttributes are the hints a volume carries.
type mountAttributes struct {
	ArtifactKind string
	Artifact     string
	ArtifactUID  string
	Digest       string
}

func attributesOf(vc map[string]string) mountAttributes {
	return mountAttributes{
		ArtifactKind: vc[AttrArtifactKind],
		Artifact:     vc[AttrArtifact],
		ArtifactUID:  vc[AttrArtifactUID],
		Digest:       vc[AttrManifestDigest],
	}
}

// authorizeMount decides whether a Pod in podNamespace may mount the volume, from the artifact as
// the API reports it; ma is nil when the namespace has no artifact of that name.
//
// THE VOLUME'S ATTRIBUTES ARE HINTS. A tenant can hand-write a Pod naming any artifact and any
// digest, so every attribute must agree with a resolved artifact in the Pod's own namespace, and the
// namespace is the one kubelet writes over the Pod's attributes. The digest is a content address and
// proves nothing about access on its own: a namespace mounts content only through its own artifact,
// resolved with its own credential.
func authorizeMount(podNamespace string, attrs mountAttributes, ma *workercore.ModelArtifact) (rule, message string) {
	deny := func(rule, detail string) (string, string) {
		return rule, fmt.Sprintf(unauthorizedModelTemplate, rule, podNamespace, attrs.Artifact, detail)
	}
	switch {
	case ma == nil:
		return deny(ruleArtifactMissing, "does not exist")
	case string(ma.UID) != attrs.ArtifactUID:
		return deny(ruleUIDMismatch, fmt.Sprintf("has UID %q, the volume names %q", ma.UID, attrs.ArtifactUID))
	case ma.Spec.Source.HuggingFace == nil && ma.Spec.Source.ModelScope == nil:
		return deny(ruleNotHub, "only a hub artifact is delivered by the node")
	case !conditionTrue(ma.Status.Conditions, resolvedCondition) || ma.Status.Resolved == nil:
		return deny(ruleNotResolved, "is not Resolved; new mounts wait until it is")
	case store.HexOf(attrs.Digest) == "" || ma.Status.Resolved.ManifestDigest != attrs.Digest:
		return deny(ruleDigestMismatch, fmt.Sprintf("resolves to %s, the volume asks for %s",
			ma.Status.Resolved.ManifestDigest, attrs.Digest))
	}

	return "", ""
}

// authorizeKernelMount decides whether a Pod in podNamespace may mount a forged or imported
// kernel tree, from the Pod's namespace as kubelet reports it and the ModelKernelArtifact as
// the API reports it.
//
// THE VOLUME'S ATTRIBUTES ARE HINTS. Every attribute must agree with a Resolved, unquarantined
// ModelKernelArtifact in the Pod's own namespace, and the published marker must carry Kind Kernel,
// so knowing a manifest digest never grants cross-namespace or cross-class access.
func authorizeKernelMount(
	podNamespace string, attrs mountAttributes, mka *workercore.ModelKernelArtifact,
) (rule, message string) {
	deny := func(rule, detail string) (string, string) {
		return rule, fmt.Sprintf(unauthorizedKernelTemplate, rule, podNamespace, attrs.Artifact, detail)
	}
	switch {
	case mka == nil:
		return deny(ruleKernelArtifactMissing, "does not exist")
	case string(mka.UID) != attrs.ArtifactUID:
		return deny(ruleKernelUIDMismatch, fmt.Sprintf("has UID %q, the volume names %q", mka.UID, attrs.ArtifactUID))
	case mka.Spec.Source.NodeStore == nil && mka.Spec.Source.HuggingFace == nil:
		return deny(ruleKernelUnsupportedSource, "only a nodeStore or huggingFace kernel artifact is delivered by the node")
	case mka.Status.Quarantined:
		return deny(ruleKernelQuarantined, "is quarantined and cannot be mounted")
	case !conditionTrue(mka.Status.Conditions, resolvedCondition) || mka.Status.Resolved == nil:
		return deny(ruleKernelNotResolved, "is not Resolved; new mounts wait until it is")
	case store.HexOf(attrs.Digest) == "" || mka.Status.Resolved.ManifestDigest != attrs.Digest:
		return deny(ruleKernelDigestMismatch, fmt.Sprintf("resolves to %s, the volume asks for %s",
			mka.Status.Resolved.ManifestDigest, attrs.Digest))
	}

	return "", ""
}

// authorizeMarkerKind enforces content-class isolation between published Kernel and Model trees.
func authorizeMarkerKind(artifactKind, podNamespace string, attrs mountAttributes, marker store.Marker) (rule, message string) {
	if artifactKind == modelstore.ArtifactKindKernel {
		if marker.Kind != store.MarkerKindKernel {
			return ruleKernelClassMismatch, fmt.Sprintf(unauthorizedKernelTemplate,
				ruleKernelClassMismatch, podNamespace, attrs.Artifact,
				fmt.Sprintf("published marker kind is %q, want %q", marker.Kind, store.MarkerKindKernel))
		}
		return "", ""
	}
	if marker.Kind != "" && marker.Kind != store.MarkerKindModel {
		return ruleModelClassMismatch, fmt.Sprintf(unauthorizedModelTemplate,
			ruleModelClassMismatch, podNamespace, attrs.Artifact,
			fmt.Sprintf("published marker kind is %q", marker.Kind))
	}
	return "", ""
}

func conditionTrue(conds []gpustack.Condition, typ string) bool {
	for _, c := range conds {
		if c.Type == typ {
			return string(c.Status) == conditionStatusTrue
		}
	}

	return false
}
