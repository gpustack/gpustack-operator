package kuberess

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
)

// DefaultTeamName is the Kubernetes Namespace name for the default team.
const DefaultTeamName = core.NamespaceDefault

// InstallDefaultTeam creates the default team, alias of the Kubernetes Namespace default.
func InstallDefaultTeam(ctx context.Context, cli kubernetes.Interface) error {
	teamCli := cli.ServerV1().Teams(SystemNamespaceName)
	team := &server.Team{
		ObjectMeta: meta.ObjectMeta{
			Namespace: SystemNamespaceName,
			Name:      DefaultTeamName,
		},
		Spec: server.TeamSpec{
			DisplayName: "Default Team",
			Description: "The default team created by GPUStack.",
		},
	}

	_, err := kubeclientset.Create(ctx, teamCli, team)
	if err != nil {
		return fmt.Errorf("install %s team: %w", DefaultTeamName, err)
	}

	return nil
}
