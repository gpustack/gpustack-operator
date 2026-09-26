package v1alpha1

import (
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// ModelArtifact is the schema for worker.gpustack.ai.
//
// It is WHERE A MODEL'S WEIGHTS COME FROM AND WHICH CREDENTIAL READS THEM, and it is the only
// object that says so: a ModelDeployment or an Instance references it by name and carries no URI,
// revision or token of its own. Every consumer in the namespace shares one declaration.
//
// IT IS AN IDENTITY, SO ITS WHOLE SPEC IS IMMUTABLE AFTER CREATION, webhook-enforced. A different
// source or revision is a different artifact, created rather than edited. That is also what makes a
// frozen reference to it pin anything: a reference that cannot change to an object that can would
// pin nothing.
//
// A Hugging Face source is resolved ONCE: the branch or tag becomes a 40-character commit, the files
// at that commit become the canonical manifest, and the manifest's digest becomes the content
// address. Nothing follows the branch afterwards. Access is revalidated periodically; losing it stops
// new consumption and never touches running Pods.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Namespaced",categories=["gpustack"],shortName=["mart"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Revision",type="string",jsonPath=".status.resolved.revision"
// +k8s:crd-gen:printcolumn:name="Size",type="integer",jsonPath=".status.resolved.sizeBytes"
// +k8s:crd-gen:printcolumn:name="Resolved",type="string",jsonPath=".status.conditions[?(@.type=='Resolved')].status"
// +k8s:crd-gen:printcolumn:name="Ready",type="integer",jsonPath=".status.nodes.ready"
// +k8s:crd-gen:printcolumn:name="Age",type="date",jsonPath=".metadata.creationTimestamp"
type ModelArtifact struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   ModelArtifactSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status ModelArtifactStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

var _ runtime.Object = (*ModelArtifact)(nil)

// ModelArtifactSpec defines the desired state of ModelArtifact.
type ModelArtifactSpec struct {
	// Source is where the weights come from.
	//
	// +required
	Source ModelArtifactSource `json:"source" protobuf:"bytes,1,name=source"`

	// AllowPatterns and IgnorePatterns select the files of a hub source that make up the artifact,
	// with the semantics of huggingface_hub's allow_patterns and ignore_patterns: Python's
	// fnmatch.fnmatchcase, where "*" and "?" cross "/"; a pattern ending in "/" is the directory's
	// contents; no allow pattern keeps every file; an ignore pattern wins over an allow pattern.
	//
	// The selection happens before the manifest is built, so the digest, file count and size
	// describe the selected files, while the patterns themselves never enter the digest: two
	// artifacts selecting the same files share one digest, and an artifact with no pattern has the
	// digest of the whole commit. Only node delivery can honor a selection; an engine that downloads
	// the weights itself chooses its own files. A claim source takes none, webhook-enforced, and
	// each pattern is 1 to 256 characters without a control character.
	//
	// +optional
	// +listType=atomic
	// +k8s:validation:maxItems=32
	AllowPatterns []string `json:"allowPatterns,omitempty" protobuf:"bytes,2,rep,name=allowPatterns"`

	// +optional
	// +listType=atomic
	// +k8s:validation:maxItems=32
	IgnorePatterns []string `json:"ignorePatterns,omitempty" protobuf:"bytes,3,rep,name=ignorePatterns"`
}

// ModelArtifactSource is a tagged union: exactly one member is set, webhook-enforced.
type ModelArtifactSource struct {
	// HuggingFace is a Hugging Face model repository at one revision.
	//
	// +optional
	HuggingFace *ModelArtifactHubSource `json:"huggingFace,omitempty" protobuf:"bytes,1,opt,name=huggingFace"`

	// ModelScope is RESERVED AND REFUSED by admission in this version. Its shape is fixed so that
	// opening it is a webhook change rather than a schema change, and the refusal names what opening
	// it needs: branch resolution cross-checked against git, a listing that re-lists per directory at
	// the API's silent truncation point, errors classified by the envelope code, and an engine runner
	// whose ModelScope SDK accepts a commit as the revision.
	//
	// +optional
	ModelScope *ModelArtifactHubSource `json:"modelScope,omitempty" protobuf:"bytes,2,opt,name=modelScope"`

	// PersistentVolumeClaim is a directory inside a claim in this namespace. The operator never
	// reads the claim's content, so the artifact has no revision and no digest, and what the
	// directory holds, and any change to it, is the user's.
	//
	// +optional
	PersistentVolumeClaim *ModelArtifactPersistentVolumeClaimSource `json:"persistentVolumeClaim,omitempty" protobuf:"bytes,3,opt,name=persistentVolumeClaim"` // nolint: lll
}

// ModelArtifactHubSource is one repository on a model hub at one revision.
type ModelArtifactHubSource struct {
	// Repository is the repository id, "owner/name", or a bare canonical name.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=256
	Repository string `json:"repository" protobuf:"bytes,1,name=repository"`

	// Revision is a branch, a tag or a commit. Admission defaults it to "main". It is resolved to a
	// full commit once, at creation, into status.resolved.revision.
	//
	// +optional
	// +k8s:validation:maxLength=255
	Revision string `json:"revision,omitempty" protobuf:"bytes,2,opt,name=revision"`

	// SecretRef names a Secret in this namespace whose "token" key is the hub token. It is read by
	// the controller for resolution and revalidation, and handed to an engine that downloads the
	// weights itself through an environment variable referencing the Secret, so the value never
	// enters a Pod spec, a status or an event.
	//
	// The Secret need not exist at admission; a missing one is reported in status.
	//
	// +optional
	SecretRef *core.LocalObjectReference `json:"secretRef,omitempty" protobuf:"bytes,3,opt,name=secretRef"`
}

// ModelArtifactPersistentVolumeClaimSource is a directory inside a claim.
type ModelArtifactPersistentVolumeClaimSource struct {
	// ClaimName names a PersistentVolumeClaim in this namespace. It need not exist at admission; a
	// missing one is reported in status.
	//
	// +required
	// +k8s:validation:minLength=1
	// +k8s:validation:maxLength=253
	ClaimName string `json:"claimName" protobuf:"bytes,1,name=claimName"`

	// Path is the directory inside the volume, relative to its root. Empty is the root. It must not
	// be absolute and must not contain a ".." element; the second half is admission's, because this
	// schema's patterns cannot express it.
	//
	// +optional
	// +k8s:validation:pattern="^[^/].*$"
	// +k8s:validation:maxLength=1024
	Path string `json:"path,omitempty" protobuf:"bytes,2,opt,name=path"`
}

// ModelArtifactStatus defines the observed state of ModelArtifact.
type ModelArtifactStatus struct {
	// ObservedGeneration is the generation the status was written for.
	ObservedGeneration int64 `json:"observedGeneration,omitempty" protobuf:"varint,1,opt,name=observedGeneration"`

	// Resolved is what the source was resolved to. It is written once and never changes afterwards:
	// a later revalidation moves only LastValidatedTime and the conditions. It is absent until the
	// first resolution succeeds.
	//
	// +optional
	Resolved *ModelArtifactResolved `json:"resolved,omitempty" protobuf:"bytes,2,opt,name=resolved"`

	// Nodes is where the content is: how many nodes hold it ready, are downloading it or failed to,
	// and how far the downloading ones are. It counts the content, the manifest digest, so artifacts
	// with the same digest see the same nodes; it holds numbers only. Absent for a claim source and
	// before the first resolution.
	//
	// +optional
	Nodes *ModelArtifactNodes `json:"nodes,omitempty" protobuf:"bytes,4,opt,name=nodes"`

	// Conditions: Resolved says whether the source is bound to its immutable identity and the most
	// recent access check passed; Degraded says a resolution or revalidation is failing, including
	// one that has not yet revoked access.
	//
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,3,rep,name=conditions"` // nolint: lll
}

// ModelArtifactNodes counts the nodes whose NodeModelStore lists an artifact's digest, by state. It
// is written when a count changes or the percentage moves to another step, at most once every 30
// seconds, so a fleet downloading the content does not rewrite the artifact at every node's write.
type ModelArtifactNodes struct {
	// Ready counts the nodes holding the content published.
	//
	// +required
	// +k8s:validation:minimum=0
	Ready int32 `json:"ready" protobuf:"varint,1,name=ready"`

	// Downloading counts the nodes downloading it.
	//
	// +required
	// +k8s:validation:minimum=0
	Downloading int32 `json:"downloading" protobuf:"varint,2,name=downloading"`

	// Failed counts the nodes whose last attempt failed and that wait to retry.
	//
	// +required
	// +k8s:validation:minimum=0
	Failed int32 `json:"failed" protobuf:"varint,3,name=failed"`

	// DownloadingPercent is the mean progress of the downloading nodes, each a whole copy, rounded
	// down to a multiple of 5. Ready nodes are counted, not averaged in. Absent while no node
	// downloads.
	//
	// +optional
	// +k8s:validation:minimum=0
	// +k8s:validation:maximum=100
	DownloadingPercent *int32 `json:"downloadingPercent,omitempty" protobuf:"varint,4,opt,name=downloadingPercent"`
}

// ModelArtifactResolved is the identity a source resolved to.
type ModelArtifactResolved struct {
	// Revision is the full 40-character commit a hub source resolved to. Absent for a claim.
	//
	// +optional
	Revision string `json:"revision,omitempty" protobuf:"bytes,1,opt,name=revision"`

	// ManifestDigest is the content address of a hub source: "sha256:" and the SHA-256 of the
	// canonical manifest of every file at Revision, in the format pkg/modelartifact defines.
	//
	// IT IS NEVER EVIDENCE OF AUTHORIZATION. It is a pure content address: a public and a private
	// repository holding the same files have the same digest, so knowing it proves nothing about
	// access. Absent for a claim.
	//
	// +optional
	ManifestDigest string `json:"manifestDigest,omitempty" protobuf:"bytes,2,opt,name=manifestDigest"`

	// FileCount and SizeBytes are the manifest's file count and total size. Absent for a claim.
	//
	// +optional
	FileCount int64 `json:"fileCount,omitempty" protobuf:"varint,3,opt,name=fileCount"`

	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty" protobuf:"varint,4,opt,name=sizeBytes"`

	// ResolvedTime is when the resolution succeeded.
	ResolvedTime meta.Time `json:"resolvedTime" protobuf:"bytes,5,name=resolvedTime"`

	// LastValidatedTime is when access was last confirmed. Absent for a claim, which has no access
	// check of its own.
	//
	// +optional
	LastValidatedTime *meta.Time `json:"lastValidatedTime,omitempty" protobuf:"bytes,6,opt,name=lastValidatedTime"`
}

// ModelArtifactList holds the list of ModelArtifact.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ModelArtifactList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []ModelArtifact `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*ModelArtifactList)(nil)
