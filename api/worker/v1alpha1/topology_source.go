package v1alpha1

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	gpustack "gpustack.ai/gpustack/api/v1"
)

// TopologySource is the cluster-scoped topology inventory used to form Kueue hierarchies.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:crd-gen:resource:scope="Cluster",categories=["gpustack"],shortName=["toposrc"],subResources=["status"]
// +k8s:crd-gen:printcolumn:name="Kind",type="string",jsonPath=".status.sourceKind"
// +k8s:crd-gen:printcolumn:name="Revision",type="string",jsonPath=".status.lastSuccessfulRevision"
// +k8s:crd-gen:printcolumn:name="Ready",type="string",jsonPath=".status.conditions[?(@.type=='Ready')].status"
type TopologySource struct {
	meta.TypeMeta   `json:",inline"`
	meta.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   TopologySourceSpec   `json:"spec" protobuf:"bytes,2,name=spec"`
	Status TopologySourceStatus `json:"status,omitempty" protobuf:"bytes,3,name=status"`
}

var _ runtime.Object = (*TopologySource)(nil)

// TopologySourceSpec selects Nodes and one inventory source. Levels are ordered coarsest first.
type TopologySourceSpec struct {
	// NodeSelector selects the Nodes this source may describe.
	// +required
	NodeSelector meta.LabelSelector `json:"nodeSelector" protobuf:"bytes,1,name=nodeSelector"`

	// Levels is the ordered topology hierarchy. kubernetes.io/hostname is implicit and forbidden here.
	// +required
	// +k8s:validation:minItems=1
	// +k8s:validation:maxItems=16
	// +listType=atomic
	Levels []string `json:"levels" protobuf:"bytes,2,rep,name=levels"`

	// AdditionalWritePrefix permits ConfigMap and webhook snapshots to write labels below one
	// administrator-owned DNS prefix. The value includes its trailing slash.
	// +optional
	AdditionalWritePrefix string `json:"additionalWritePrefix,omitempty" protobuf:"bytes,6,opt,name=additionalWritePrefix"`

	// NodeLabels consumes the selected Nodes' existing labels without changing them.
	// +optional
	NodeLabels *TopologySourceNodeLabels `json:"nodeLabels,omitempty" protobuf:"bytes,3,name=nodeLabels"`

	// ConfigMap reads a versioned inventory snapshot from the worker namespace.
	// +optional
	ConfigMap *TopologySourceConfigMap `json:"configMap,omitempty" protobuf:"bytes,4,name=configMap"`

	// Webhook reads a versioned inventory snapshot over authenticated HTTPS.
	// +optional
	Webhook *TopologySourceWebhook `json:"webhook,omitempty" protobuf:"bytes,5,name=webhook"`
}

// TopologySourceNodeLabels is the read-only source arm. Its presence selects labels already on Nodes.
type TopologySourceNodeLabels struct{}

// TopologySourceObjectReference identifies a namespaced credential or configuration object.
type TopologySourceObjectReference struct {
	// +required
	Namespace string `json:"namespace" protobuf:"bytes,1,name=namespace"`
	// +required
	Name string `json:"name" protobuf:"bytes,2,name=name"`
}

// TopologySourceConfigMap reads the snapshot key from one ConfigMap.
type TopologySourceConfigMap struct {
	// +required
	ConfigMapRef TopologySourceObjectReference `json:"configMapRef" protobuf:"bytes,1,name=configMapRef"`
	// +required
	Key string `json:"key" protobuf:"bytes,2,name=key"`
	// MaxStaleness is how long the last valid snapshot remains usable after reads fail.
	// +required
	MaxStaleness meta.Duration `json:"maxStaleness" protobuf:"bytes,3,name=maxStaleness"`
}

// TopologySourceWebhook reads snapshots from an HTTPS endpoint. Exactly one credential arm is required.
type TopologySourceWebhook struct {
	// +required
	URL string `json:"url" protobuf:"bytes,1,name=url"`
	// +required
	PollInterval meta.Duration `json:"pollInterval" protobuf:"bytes,2,name=pollInterval"`
	// +required
	Timeout meta.Duration `json:"timeout" protobuf:"bytes,3,name=timeout"`
	// +required
	MaxStaleness meta.Duration `json:"maxStaleness" protobuf:"bytes,4,name=maxStaleness"`
	// CABundleConfigMapRef optionally supplies the endpoint's CA bundle from the worker namespace.
	// The ConfigMap must store the PEM bundle under ca.crt.
	// +optional
	CABundleConfigMapRef *TopologySourceObjectReference `json:"caBundleConfigMapRef,omitempty" protobuf:"bytes,5,name=caBundleConfigMapRef"`
	// BearerTokenSecretRef selects bearer-token authentication. The Secret key is token.
	// +optional
	BearerTokenSecretRef *TopologySourceObjectReference `json:"bearerTokenSecretRef,omitempty" protobuf:"bytes,6,name=bearerTokenSecretRef"`
	// TLSClientCertificateSecretRef selects mTLS authentication. The Secret keys are tls.crt and tls.key.
	// +optional
	//nolint:lll
	TLSClientCertificateSecretRef *TopologySourceObjectReference `json:"tlsClientCertificateSecretRef,omitempty" protobuf:"bytes,7,name=tlsClientCertificateSecretRef"`
}

// TopologySourceStatus is the last observed topology inventory state.
type TopologySourceStatus struct {
	ObservedGeneration        int64      `json:"observedGeneration,omitempty" protobuf:"varint,1,name=observedGeneration"`
	SourceKind                string     `json:"sourceKind,omitempty" protobuf:"bytes,2,name=sourceKind"`
	LastSuccessfulRevision    string     `json:"lastSuccessfulRevision,omitempty" protobuf:"bytes,3,name=lastSuccessfulRevision"`
	LastSuccessfulRefreshTime *meta.Time `json:"lastSuccessfulRefreshTime,omitempty" protobuf:"bytes,4,name=lastSuccessfulRefreshTime"`
	SelectedNodes             int32      `json:"selectedNodes,omitempty" protobuf:"varint,5,name=selectedNodes"`
	MutatedNodes              int32      `json:"mutatedNodes,omitempty" protobuf:"varint,6,name=mutatedNodes"`
	ConflictedNodes           int32      `json:"conflictedNodes,omitempty" protobuf:"varint,7,name=conflictedNodes"`
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []gpustack.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,8,rep,name=conditions"`
}

// TopologySourceList holds a list of TopologySource objects.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type TopologySourceList struct {
	meta.TypeMeta `json:",inline"`
	meta.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
	Items         []TopologySource `json:"items" protobuf:"bytes,2,rep,name=items"`
}

var _ runtime.Object = (*TopologySourceList)(nil)
