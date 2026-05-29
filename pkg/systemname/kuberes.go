package systemname

import "gpustack.ai/gpustack/pkg/utils/osx"

// NamespaceName is the name indicates which Kubernetes Namespace storing system resources.
var NamespaceName string

func init() {
	NamespaceName = osx.Getenv("KUBERNETES_POD_NAMESPACE", "gpustack-system")
}

const (
	// LabelPrefix is the prefix of all labels used by gpustack.
	LabelPrefix = "gpustack.ai/"

	// ManagedLabelKey is the label key to indicate whether a resource is managed by gpustack.
	ManagedLabelKey = LabelPrefix + "managed"
)
