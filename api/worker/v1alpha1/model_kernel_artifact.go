package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

const (
	// ModelKernelArtifactProtectionFinalizer keeps a ModelKernelArtifact in Terminating while any
	// ModelDeployment role references it or any ModelDeploymentForger holds it for rollback.
	ModelKernelArtifactProtectionFinalizer = "worker.gpustack.ai/model-kernel-artifact-protection"
)

// Condition types reported by ModelKernelArtifact.
const (
	// ModelKernelArtifactConditionTypeResolved reports whether the kernel source is bound to its
	// immutable content identity and remains available.
	ModelKernelArtifactConditionTypeResolved = "Resolved"

	// ModelKernelArtifactConditionTypeDegraded reports whether resolution or revalidation is
	// experiencing errors.
	ModelKernelArtifactConditionTypeDegraded = "Degraded"

	// ModelKernelArtifactConditionTypePinned reports whether the artifact digest is pinned in the
	// target NodeModelStore cache configuration.
	ModelKernelArtifactConditionTypePinned = "Pinned"
)

// Condition and admission reasons for ModelKernelArtifact.
const (
	// ModelKernelArtifactReasonResolved indicates the kernel bundle resolved to its immutable digest.
	ModelKernelArtifactReasonResolved = "Resolved"

	// ModelKernelArtifactReasonResolving indicates resolution is in progress.
	ModelKernelArtifactReasonResolving = "Resolving"

	// ModelKernelArtifactReasonSourceLost indicates no node holds a ready copy of a node-stored
	// kernel tree.
	ModelKernelArtifactReasonSourceLost = "SourceLost"

	// ModelKernelArtifactReasonDigestMismatch indicates the observed content digest disagrees with
	// the declared digest.
	ModelKernelArtifactReasonDigestMismatch = "DigestMismatch"

	// ModelKernelArtifactReasonSecretNotFound indicates the referenced hub credential Secret is
	// missing.
	ModelKernelArtifactReasonSecretNotFound = "SecretNotFound"

	// ModelKernelArtifactReasonUnsupportedSource indicates the cluster cannot serve the selected
	// source kind.
	ModelKernelArtifactReasonUnsupportedSource = "UnsupportedSource"

	// ModelKernelArtifactReasonHealthy indicates no resolution or revalidation degradation exists.
	ModelKernelArtifactReasonHealthy = "Healthy"

	// ModelKernelArtifactReasonPinned indicates the kernel digest is retained in NodeModelStore
	// pin lists.
	ModelKernelArtifactReasonPinned = "Pinned"

	// ModelKernelArtifactReasonUnpinned indicates the kernel digest does not currently require a
	// NodeModelStore pin.
	ModelKernelArtifactReasonUnpinned = "Unpinned"

	// ModelKernelArtifactReasonPinnedCapacityExceeded indicates a target node exceeded the per-node
	// pin ceiling before admitting this artifact's digest.
	ModelKernelArtifactReasonPinnedCapacityExceeded = "PinnedCapacityExceeded"

	// ModelKernelArtifactReasonNodeStoreSourceForbidden indicates a caller other than the operator
	// worker attempted to create a nodeStore kernel artifact.
	ModelKernelArtifactReasonNodeStoreSourceForbidden = "NodeStoreSourceForbidden"
)

// ModelKernelArtifact declares an immutable, content-addressed bundle of specialized single-GPU
// operator kernels and their hardware and engine compatibility envelope.
//
// Its whole spec is immutable after creation, enforced at admission. Binary kernel bundles live in
// NodeModelStore, a commit-pinned hub repository, or a digest-pinned OCI image; ConfigMap sources
// are omitted from the schema by design.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["mkart"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Engine",type="string",jsonPath=".spec.compatibility.engine"
// +k8s:crd-gen:printcolumn:name="Arch",type="string",jsonPath=".spec.compatibility.architecture"
// +k8s:crd-gen:printcolumn:name="Size",type="integer",jsonPath=".status.resolved.sizeBytes"
// +k8s:crd-gen:printcolumn:name="Resolved",type="string",jsonPath=".status.conditions[?(@.type=='Resolved')].status"
// +k8s:crd-gen:printcolumn:name="Ready",type="integer",jsonPath=".status.nodes.ready"
// +k8s:crd-gen:printcolumn:name="Quarantined",type="boolean",jsonPath=".status.quarantined"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type ModelKernelArtifact struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ModelKernelArtifactSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status ModelKernelArtifactStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*ModelKernelArtifact)(nil)

// ModelKernelArtifactSpec defines the immutable source and compatibility contract of a
// ModelKernelArtifact.
type ModelKernelArtifactSpec struct {
	// Source declares where the verified kernel bundle comes from.
	//
	// +required
	Source ModelKernelArtifactSource `json:"source" protobuf:"bytes,1,name=source"`

	// Compatibility declares the engine, hardware architecture, and canonical single-GPU kernel
	// keys this bundle was built and verified for.
	//
	// +required
	Compatibility ModelKernelArtifactCompatibility `json:"compatibility" protobuf:"bytes,2,name=compatibility"`
}

// ModelKernelArtifactSource is a tagged union naming where a kernel bundle is stored. Exactly one
// member must be set, enforced at admission; ConfigMap sources do not exist on this union.
type ModelKernelArtifactSource struct {
	// NodeStore references a forged kernel bundle ingested into NodeModelStore on a seed node and
	// replicated across peers. Only the operator worker identity may create artifacts with this
	// source.
	//
	// +optional
	NodeStore *ModelKernelArtifactNodeStoreSource `json:"nodeStore,omitempty" protobuf:"bytes,1,opt,name=nodeStore"`

	// HuggingFace imports an exported kernel bundle from a Hugging Face repository pinned to a
	// full 40-character hexadecimal commit SHA.
	//
	// +optional
	HuggingFace *ModelArtifactHubSource `json:"huggingFace,omitempty" protobuf:"bytes,2,opt,name=huggingFace"`

	// Image imports an exported kernel bundle from a digest-pinned OCI image mounted read-only
	// through a Kubernetes image volume.
	//
	// +optional
	Image *ModelArtifactImageSource `json:"image,omitempty" protobuf:"bytes,3,opt,name=image"`
}

// ModelKernelArtifactNodeStoreSource identifies a verified forged kernel tree published in
// NodeModelStore.
type ModelKernelArtifactNodeStoreSource struct {
	// ManifestDigest is the content address of the published kernel tree ("sha256:" followed by
	// 64 lowercase hexadecimal characters).
	//
	// +required
	// +k8s:validation:pattern="^sha256:[a-f0-9]{64}$"
	// +k8s:validation:maxLength=71
	ManifestDigest string `json:"manifestDigest" protobuf:"bytes,1,name=manifestDigest"`

	// DispatchManifestDigest is the SHA-256 digest of dispatch-manifest.json at the root of the
	// kernel tree ("sha256:" followed by 64 lowercase hexadecimal characters).
	//
	// +required
	// +k8s:validation:pattern="^sha256:[a-f0-9]{64}$"
	// +k8s:validation:maxLength=71
	DispatchManifestDigest string `json:"dispatchManifestDigest" protobuf:"bytes,2,name=dispatchManifestDigest"`

	// SizeBytes is the total byte size of all files in the published kernel tree.
	//
	// +required
	// +k8s:validation:minimum=1
	SizeBytes int64 `json:"sizeBytes" protobuf:"varint,3,name=sizeBytes"`

	// FileCount is the number of regular files in the published kernel tree, including
	// dispatch-manifest.json and at least one solution or kernel file.
	//
	// +required
	// +k8s:validation:minimum=2
	FileCount int64 `json:"fileCount" protobuf:"varint,4,name=fileCount"`

	// SeedNode names the Kubernetes Node whose model-manager daemon first ingested and published
	// the verified kernel tree.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	SeedNode string `json:"seedNode" protobuf:"bytes,5,name=seedNode"`

	// ForgerRef identifies the owning ModelDeploymentForger in this namespace that produced the
	// kernel bundle.
	//
	// +required
	ForgerRef ModelKernelArtifactForgerReference `json:"forgerRef" protobuf:"bytes,6,name=forgerRef"`
}

// ModelKernelArtifactForgerReference identifies the ModelDeploymentForger that produced a
// node-stored kernel artifact.
type ModelKernelArtifactForgerReference struct {
	// Name is the name of the owning ModelDeploymentForger in this namespace.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// UID is the Kubernetes UID of the owning ModelDeploymentForger at creation time.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=128
	UID string `json:"uid" protobuf:"bytes,2,name=uid"`
}

// ModelKernelArtifactCompatibility declares the runtime engine, hardware target, and canonical
// single-GPU kernel identities a bundle serves.
type ModelKernelArtifactCompatibility struct {
	// Engine selects the inference engine family this bundle targets.
	//
	// +required
	// +k8s:validation:enum=["vLLM","SGLang"]
	Engine string `json:"engine" protobuf:"bytes,1,name=engine"`

	// EngineVersion is the normalized inference engine release version this bundle was verified
	// against.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=64
	EngineVersion string `json:"engineVersion" protobuf:"bytes,2,name=engineVersion"`

	// Manufacturer is the canonical lowercase accelerator vendor key, such as "nvidia", "amd", or
	// "ascend".
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=63
	Manufacturer string `json:"manufacturer" protobuf:"bytes,3,name=manufacturer"`

	// Architecture is the canonical accelerator architecture identifier, such as "sm_90a",
	// "gfx942", or "910b".
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=63
	Architecture string `json:"architecture" protobuf:"bytes,4,name=architecture"`

	// KernelKeys lists the canonical single-GPU LocalKernelKey entries carried in this bundle's
	// dispatch manifest, over static per-rank dimensions only.
	//
	// +required
	// +listType=atomic
	// +k8s:validation:minItems=1
	// +k8s:validation:maxItems=128
	KernelKeys []string `json:"kernelKeys" protobuf:"bytes,5,rep,name=kernelKeys"`
}

// ModelKernelArtifactStatus defines the observed state of ModelKernelArtifact.
type ModelKernelArtifactStatus struct {
	// ObservedGeneration is the metadata generation this status reflects.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty" protobuf:"varint,1,opt,name=observedGeneration"`

	// Resolved records the immutable content identity once resolution succeeds. Absent for OCI
	// image sources and before initial resolution completes.
	//
	// +optional
	Resolved *ModelKernelArtifactResolved `json:"resolved,omitempty" protobuf:"bytes,2,opt,name=resolved"`

	// Nodes summarizes how many NodeModelStore caches hold, download, or failed this artifact's
	// manifest digest without exposing node names.
	//
	// +optional
	Nodes *ModelArtifactNodes `json:"nodes,omitempty" protobuf:"bytes,3,opt,name=nodes"`

	// Quarantined is true when a canary rollout or runtime load check failed on this bundle.
	// Quarantined artifacts are never promoted or newly mounted, and quarantine persists across
	// forger recreation.
	//
	// +optional
	Quarantined bool `json:"quarantined,omitempty" protobuf:"varint,4,opt,name=quarantined"`

	// QuarantineReason classifies why Quarantined was set.
	//
	// +optional
	QuarantineReason string `json:"quarantineReason,omitempty" protobuf:"bytes,5,opt,name=quarantineReason"`

	// QuarantineMessage provides human-readable details for why Quarantined was set.
	//
	// +optional
	QuarantineMessage string `json:"quarantineMessage,omitempty" protobuf:"bytes,6,opt,name=quarantineMessage"`

	// Conditions reports Resolved, Degraded, and Pinned state for this kernel artifact.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,7,rep,name=conditions"` // nolint: lll
}

// ModelKernelArtifactResolved records the verified content identity of a resolved kernel source.
type ModelKernelArtifactResolved struct {
	// ManifestDigest is the canonical gpustack-manifest v1 SHA-256 content address of the tree.
	//
	// +optional
	ManifestDigest string `json:"manifestDigest,omitempty" protobuf:"bytes,1,opt,name=manifestDigest"`

	// DispatchManifestDigest is the SHA-256 digest of dispatch-manifest.json at the root of the
	// resolved kernel tree.
	//
	// +optional
	DispatchManifestDigest string `json:"dispatchManifestDigest,omitempty" protobuf:"bytes,2,opt,name=dispatchManifestDigest"`

	// Revision is the 40-character commit SHA when imported from Hugging Face; empty for forged
	// nodeStore bundles.
	//
	// +optional
	Revision string `json:"revision,omitempty" protobuf:"bytes,3,opt,name=revision"`

	// FileCount is the number of files in the resolved manifest.
	//
	// +optional
	FileCount int64 `json:"fileCount,omitempty" protobuf:"varint,4,opt,name=fileCount"`

	// SizeBytes is the total byte size of all files in the resolved manifest.
	//
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty" protobuf:"varint,5,opt,name=sizeBytes"`

	// ResolvedTime is when the source first resolved to its content identity.
	ResolvedTime meta.Time `json:"resolvedTime" protobuf:"bytes,6,name=resolvedTime"`

	// LastValidatedTime is when source availability or hub access was most recently confirmed.
	//
	// +optional
	LastValidatedTime *meta.Time `json:"lastValidatedTime,omitempty" protobuf:"bytes,7,opt,name=lastValidatedTime"`

	// DigestSource records whether ManifestDigest originated from an in-cluster Forge publish or a
	// Hugging Face Hub listing.
	//
	// +optional
	// +k8s:validation:enum=["Forge","Hub"]
	DigestSource ModelKernelArtifactDigestSource `json:"digestSource,omitempty" protobuf:"bytes,8,opt,name=digestSource,casttype=ModelKernelArtifactDigestSource"` // nolint: lll
}

// ModelKernelArtifactDigestSource names where a resolved kernel manifest digest originated.
// +enum
type ModelKernelArtifactDigestSource string

const (
	// ModelKernelArtifactDigestSourceForge indicates the digest was produced by in-cluster sandbox
	// forging and NodeModelStore ingestion.
	ModelKernelArtifactDigestSourceForge ModelKernelArtifactDigestSource = "Forge"

	// ModelKernelArtifactDigestSourceHub indicates the digest was resolved from a commit-pinned
	// Hugging Face repository listing.
	ModelKernelArtifactDigestSourceHub ModelKernelArtifactDigestSource = "Hub"
)

// ModelKernelArtifactList holds a list of ModelKernelArtifact resources.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelKernelArtifactList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelKernelArtifact `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelKernelArtifactList)(nil)
