package kuberess

import (
	"context"
	"fmt"
	"time"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/server/settings"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

const (
	// DefaultClusterName is the name of the default cluster.
	DefaultClusterName = "local"
)

// ImportLocalCluster imports the local cluster if the system setting ImportLocalCluster is true.
func ImportLocalCluster(ctx context.Context, cli kubernetes.Interface) error {
	importLocalCluster, err := settings.ImportLocalCluster.ValueBool(ctx)
	if err != nil {
		return err
	}

	if !importLocalCluster {
		return nil
	}

	clsCli := cli.ServerV1().Clusters(DefaultTeamName)
	cls := &server.Cluster{
		ObjectMeta: meta.ObjectMeta{
			Namespace: DefaultTeamName,
			Name:      DefaultClusterName,
		},
		Spec: servercore.ClusterSpec{
			Type:        servercore.ClusterTypeLoopback,
			DisplayName: "Local Cluster",
			Description: "The default cluster imported by GPUStack.",
		},
	}

	_, err = kubeclientset.Create(ctx, clsCli, cls)
	if err != nil {
		return fmt.Errorf("install %s cluster: %w", DefaultClusterName, err)
	}

	// Bind the local cluster to the default project.
	err = waitx.PollUntilContextTimeout(
		ctx,
		time.Second,
		30*time.Second,
		true,
		func(ctx context.Context) (err error) {
			projCli := cli.ServerV1().Projects(DefaultTeamName)
			projClss, err := projCli.GetClusters(ctx, DefaultProjectName, meta.GetOptions{ResourceVersion: "0"})
			if err != nil {
				return fmt.Errorf("get clusters of %s projects: %w", DefaultProjectName, err)
			}
			if !projClss.HasCluster(cls) {
				projClss.Items = append(projClss.Items, server.ProjectCluster{
					ClusterReference: servercore.ClusterReference{
						Namespace: DefaultTeamName,
						Name:      DefaultClusterName,
					},
				})
				_, err = projCli.UpdateClusters(ctx, DefaultProjectName, projClss, meta.UpdateOptions{})
				if err != nil {
					return fmt.Errorf("update clusters of %s projects: %w", DefaultProjectName, err)
				}
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("bind %s cluster to %s project: %w", DefaultClusterName, DefaultProjectName, err)
	}

	return nil
}

func IsLocalCluster(obj any) bool {
	if obj == nil {
		return false
	}

	switch o := obj.(type) {
	case types.NamespacedName:
		return o.Namespace == DefaultTeamName && o.Name == DefaultClusterName
	case *types.NamespacedName:
		return o.Namespace == DefaultTeamName && o.Name == DefaultClusterName
	case meta.Object:
		return o.GetNamespace() == DefaultTeamName && o.GetName() == DefaultClusterName
	}
	return false
}
