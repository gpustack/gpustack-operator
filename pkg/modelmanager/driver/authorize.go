package driver

import (
	"fmt"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
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

	// AttrArtifact, AttrArtifactUID and AttrManifestDigest are what a consumer's volume names.
	AttrArtifact       = modelstore.VolumeAttrArtifact
	AttrArtifactUID    = modelstore.VolumeAttrArtifactUID
	AttrManifestDigest = modelstore.VolumeAttrManifestDigest
)

// The authorization rules, named in a refusal.
const (
	ruleArtifactMissing  = "the Pod's namespace has no such ModelArtifact"
	ruleUIDMismatch      = "the ModelArtifact is not the one the volume names"
	ruleNotHub           = "the ModelArtifact is not on a model hub"
	ruleNotResolved      = "the ModelArtifact is not resolved"
	ruleDigestMismatch   = "the ModelArtifact resolves to another digest"
	resolvedCondition    = "Resolved"
	conditionStatusTrue  = "True"
	unauthorizedTemplate = "%s: ModelArtifact %s/%s: %s"
)

// mountAttributes are the hints a volume carries.
type mountAttributes struct {
	Artifact    string
	ArtifactUID string
	Digest      string
}

func attributesOf(vc map[string]string) mountAttributes {
	return mountAttributes{Artifact: vc[AttrArtifact], ArtifactUID: vc[AttrArtifactUID], Digest: vc[AttrManifestDigest]}
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
		return rule, fmt.Sprintf(unauthorizedTemplate, rule, podNamespace, attrs.Artifact, detail)
	}
	switch {
	case ma == nil:
		return deny(ruleArtifactMissing, "does not exist")
	case string(ma.UID) != attrs.ArtifactUID:
		return deny(ruleUIDMismatch, fmt.Sprintf("has UID %q, the volume names %q", ma.UID, attrs.ArtifactUID))
	case ma.Spec.Source.HuggingFace == nil:
		return deny(ruleNotHub, "only a Hugging Face artifact is delivered by the node")
	case !conditionTrue(ma.Status.Conditions, resolvedCondition) || ma.Status.Resolved == nil:
		return deny(ruleNotResolved, "is not Resolved; new mounts wait until it is")
	case ma.Status.Resolved.ManifestDigest != attrs.Digest:
		return deny(ruleDigestMismatch, fmt.Sprintf("resolves to %s, the volume asks for %s",
			ma.Status.Resolved.ManifestDigest, attrs.Digest))
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
