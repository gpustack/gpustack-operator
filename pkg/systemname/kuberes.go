package systemname

import "gpustack.ai/gpustack/pkg/utils/osx"

var (
	// NamespaceName is the name indicates which Kubernetes Namespace storing system resources.
	NamespaceName string

	// ToolkitNamespaceName is the name indicates which Kubernetes Namespace storing system toolkit resources.
	ToolkitNamespaceName string
)

func init() {
	NamespaceName = osx.Getenv("KUBERNETES_POD_NAMESPACE", "gpustack-system")
	ToolkitNamespaceName = NamespaceName + "-toolkit"
}
