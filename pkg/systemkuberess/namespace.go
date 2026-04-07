package systemkuberess

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeclientset/review"
)

// InstallSystemNamespace creates the system namespace.
func InstallSystemNamespace(ctx context.Context, cli kubernetes.Interface, nsName string) error {
	err := review.CanDoCreate(ctx,
		cli.AuthorizationV1().SelfSubjectAccessReviews(),
		review.Simples{
			{
				Group:    core.SchemeGroupVersion.Group,
				Version:  core.SchemeGroupVersion.Version,
				Resource: "namespaces",
			},
		},
	)
	if err != nil {
		return err
	}

	nsCli := cli.CoreV1().Namespaces()
	ns := &core.Namespace{
		ObjectMeta: meta.ObjectMeta{
			Name: nsName,
		},
	}

	_, err = kubeclientset.Create(ctx, nsCli, ns)
	if err != nil {
		return fmt.Errorf("install namespace %q: %w", ns.GetName(), err)
	}

	return nil
}
