package peer

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
)

// ClusterMetadata contains the metadata of the cluster, including API endpoint, CA and Kubernetes version.
type ClusterMetadata struct {
	// Endpoint is the endpoint of the cluster, including host and API path.
	Endpoint string `json:"endpoint"`

	// Version is the Kubernetes version of the cluster.
	Version string `json:"version"`

	// CA is the certificate authority data of the cluster, in PEM format.
	CA string `json:"ca,omitempty"`
}

// Peer is the interface for peer communication and cluster access.
type Peer interface {
	// GetPeerID returns the unique identifier of the peer, which is used for peer discovery and communication.
	GetPeerID() string

	// GetClusterKubeRestConfig returns the kube rest config of the cluster,
	// which can be used to create kube client to access the cluster.
	GetClusterKubeRestConfig(ctx context.Context, cls types.NamespacedName, opts ...func(config *rest.Config)) (*rest.Config, error)

	// GetClusterKubeClient returns the kube client of the cluster, which can be used to access the cluster.
	GetClusterKubeClient(ctx context.Context, cls types.NamespacedName, opts ...func(config *rest.Config)) (kubernetes.Interface, error)

	// GetClusterCtrlClient returns the controller-runtime client of the cluster, which can be used to access the cluster.
	GetClusterCtrlClient(ctx context.Context, cls types.NamespacedName, opts ...func(config *rest.Config)) (ctrlcli.Client, error)

	// GetClusterMetadata returns the metadata of the cluster, including API endpoint, CA and Kubernetes version.
	GetClusterMetadata(ctx context.Context, cls types.NamespacedName) (*ClusterMetadata, error)
}
