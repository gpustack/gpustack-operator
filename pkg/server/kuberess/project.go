package kuberess

import (
	"context"
	"fmt"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
)

// DefaultProjectName is the Kubernetes Namespace name for the default project.
const DefaultProjectName = DefaultTeamName + "-local"

// InstallDefaultProject creates the default project, alias to Kubernetes Namespace default-local.
func InstallDefaultProject(ctx context.Context, cli kubernetes.Interface) error {
	projCli := cli.ServerV1().Projects(DefaultTeamName)
	proj := &server.Project{
		ObjectMeta: meta.ObjectMeta{
			Namespace: DefaultTeamName,
			Name:      DefaultProjectName,
		},
		Spec: server.ProjectSpec{
			DisplayName: "Default Project",
			Description: "The default project created by GPUStack.",
		},
	}

	_, err := kubeclientset.Create(ctx, projCli, proj)
	if err != nil {
		return fmt.Errorf("install %s project: %w", DefaultProjectName, err)
	}

	return nil
}
