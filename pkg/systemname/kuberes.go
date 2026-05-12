package systemname

import "gpustack.ai/gpustack/pkg/utils/osx"

// NamespaceName is the name indicates which Kubernetes Namespace storing system resources.
var NamespaceName string

func init() {
	NamespaceName = osx.Getenv("KUBERNETES_POD_NAMESPACE", "gpustack-system")
}
