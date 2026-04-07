package authz

import (
	"context"
	"fmt"

	rbac "k8s.io/api/rbac/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeclientset/review"
	"gpustack.ai/gpustack/pkg/server/kuberess"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

const (
	// AnonymousClusterRoleName is the name of the Kubernetes ClusterRole for system anonymous.
	AnonymousClusterRoleName = "gpustack-anonymous"
	// ViewerClusterRoleName is the name of the Kubernetes ClusterRole for system viewer.
	ViewerClusterRoleName = "gpustack-viewer"
	// DeployerClusterRoleName is the name of the Kubernetes ClusterRole for system deployer.
	DeployerClusterRoleName = "gpustack-deployer"
	// EditorClusterRoleName is the name of the Kubernetes ClusterRole for system editor.
	EditorClusterRoleName = "gpustack-editor"
	// AdminClusterRoleName is the name of the Kubernetes ClusterRole for system administrator.
	AdminClusterRoleName = "gpustack-admin"
)

// Initialize initializes Kubernetes resources for authorization.
//
// Initialize creates Kubernetes ClusterRole/ClusterRoleBinding/RoleBinding for system.
func Initialize(ctx context.Context, cli kubernetes.Interface) error {
	err := review.CanDoCreate(ctx,
		cli.AuthorizationV1().SelfSubjectAccessReviews(),
		review.Simples{
			{
				Group:    rbac.SchemeGroupVersion.Group,
				Version:  rbac.SchemeGroupVersion.Version,
				Resource: "clusterroles",
			},
			{
				Group:    rbac.SchemeGroupVersion.Group,
				Version:  rbac.SchemeGroupVersion.Version,
				Resource: "rolebindings",
			},
		},
		review.WithUpdateIfExisted(),
	)
	if err != nil {
		return err
	}

	crCli := cli.RbacV1().ClusterRoles()
	eCrs := []*rbac.ClusterRole{
		// Anonymous.
		{
			ObjectMeta: meta.ObjectMeta{
				Name: AnonymousClusterRoleName,
			},
			Rules: []rbac.PolicyRule{
				// Read limited resources include:
				// - Specific settings.
				{
					APIGroups: []string{
						server.GroupName,
					},
					Resources: []string{
						"settings",
					},
					ResourceNames: []string{
						"bootstrap-password-provision-state",
						"serve-url",
					},
					Verbs: []string{
						"get",
					},
				},
			},
		},
		// Viewer.
		{
			ObjectMeta: meta.ObjectMeta{
				Name: ViewerClusterRoleName,
			},
			Rules: []rbac.PolicyRule{
				// View all resources exclude:
				// - Subject Login
				// - Subject Token
				// - Subject Providers
				{
					APIGroups: []string{
						server.GroupName,
					},
					Resources: []string{
						"clusters",
						"clusters/status",
						"projects",
						"projects/clusters",
						"teams",
						"teams/subjects",
						"subjects",
					},
					Verbs: []string{
						"get",
						"list",
						"watch",
					},
				},
				// Manage self Team.
				{
					APIGroups: []string{
						server.GroupName,
					},
					Resources: []string{
						"projects",
					},
					Verbs: []string{
						rbac.VerbAll,
					},
				},
			},
		},
		// Deployer.
		{
			ObjectMeta: meta.ObjectMeta{
				Name: DeployerClusterRoleName,
			},
			Rules: []rbac.PolicyRule{
				// Manage partial resources.
				{
					APIGroups: []string{
						server.GroupName,
					},
					Resources: []string{
						"clusters",
						"clusters/status",
						"clusters/config",
					},
					Verbs: []string{
						"get",
						"list",
						"watch",
					},
				},
			},
		},
		// Editor.
		{
			ObjectMeta: meta.ObjectMeta{
				Name: EditorClusterRoleName,
			},
			Rules: []rbac.PolicyRule{
				// Manage all resources exclude:
				// - Subject
				// - Subject Login
				// - Subject Token
				// - Subject Providers
				{
					APIGroups: []string{
						server.GroupName,
					},
					Resources: []string{
						"clusters",
						"clusters/status",
						"clusters/config",
						"projects",
						"projects/status",
						"projects/clusters",
						"teams",
						"teams/status",
						"teams/subjects",
					},
					Verbs: []string{
						rbac.VerbAll,
					},
				},
				{
					APIGroups: []string{
						server.GroupName,
					},
					Resources: []string{
						"subjects",
					},
					Verbs: []string{
						"get",
						"list",
						"watch",
					},
				},
			},
		},
		// Admin.
		{
			ObjectMeta: meta.ObjectMeta{
				Name: AdminClusterRoleName,
			},
			Rules: []rbac.PolicyRule{
				// Manage all resources exclude:
				// - Subject Login
				// - Subject Token
				{
					APIGroups: []string{
						server.GroupName,
					},
					Resources: []string{
						"clusters",
						"clusters/status",
						"clusters/config",
						"projects",
						"projects/status",
						"projects/clusters",
						"teams",
						"teams/status",
						"teams/subjects",
						"subjects",
						"subjectproviders",
					},
					Verbs: []string{
						rbac.VerbAll,
					},
				},
			},
		},
	}
	for i := range eCrs {
		systemmeta.NoteResource(eCrs[i], "roles", nil)

		// Create.
		_, err = kubeclientset.Create(ctx, crCli, eCrs[i],
			kubeclientset.WithUpdateIfExisted(kubeclientset.NewRbacClusterRoleAlignFunc(eCrs[i])))
		if err != nil {
			return fmt.Errorf("install cluster role %q: %w", eCrs[i].Name, err)
		}
	}

	rbCli := cli.RbacV1().RoleBindings(kuberess.SystemNamespaceName)
	eRbs := []*rbac.RoleBinding{
		// Fro system anonymous.
		{
			ObjectMeta: meta.ObjectMeta{
				Namespace: kuberess.SystemNamespaceName,
				Name:      AnonymousClusterRoleName,
			},
			RoleRef: rbac.RoleRef{
				APIGroup: rbac.GroupName,
				Kind:     "ClusterRole",
				Name:     AnonymousClusterRoleName,
			},
			Subjects: []rbac.Subject{
				{
					APIGroup: rbac.GroupName,
					Kind:     rbac.GroupKind,
					Name:     "system:unauthenticated",
				},
			},
		},
		// For system user.
		{
			ObjectMeta: meta.ObjectMeta{
				Namespace: kuberess.SystemNamespaceName,
				Name:      ViewerClusterRoleName,
			},
			RoleRef: rbac.RoleRef{
				APIGroup: rbac.GroupName,
				Kind:     "ClusterRole",
				Name:     ViewerClusterRoleName,
			},
			Subjects: []rbac.Subject{
				{
					APIGroup: rbac.GroupName,
					Kind:     rbac.GroupKind,
					Name:     "system:authenticated",
				},
			},
		},
	}
	for i := range eRbs {
		// Create.
		_, err = kubeclientset.Create(ctx, rbCli, eRbs[i],
			kubeclientset.WithRecreateIfDuplicated(kubeclientset.NewRbacRoleBindingCompareFunc(eRbs[i])))
		if err != nil {
			return fmt.Errorf("install role binding %q: %w", eRbs[i].Name, err)
		}
	}

	return nil
}
