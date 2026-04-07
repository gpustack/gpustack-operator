package kuberess

import (
	"context"
	"fmt"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
)

// DefaultSubjectProviderName is the name of the default subject provider.
const DefaultSubjectProviderName = "default"

// InstallDefaultSubjectProvider creates the default subject provider,
// alias to Kubernetes Secret gpustack-subject-provider-default under the system namespace.
func InstallDefaultSubjectProvider(ctx context.Context, cli kubernetes.Interface) error {
	subjProvCli := cli.ServerV1().SubjectProviders(SystemNamespaceName)
	subjProv := &server.SubjectProvider{
		ObjectMeta: meta.ObjectMeta{
			Namespace: SystemNamespaceName,
			Name:      DefaultSubjectProviderName,
		},
		Spec: server.SubjectProviderSpec{
			Type:        server.SubjectProviderTypeInternal,
			DisplayName: "Default Subject Provider",
			Description: "The default subject provider created by GPUStack.",
		},
	}

	_, err := kubeclientset.Create(ctx, subjProvCli, subjProv)
	if err != nil {
		return fmt.Errorf("install %s subject provider: %w", DefaultSubjectProviderName, err)
	}

	return nil
}
