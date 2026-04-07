package authz

import (
	"context"
	"errors"
	"fmt"

	rbac "k8s.io/api/rbac/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	authnuser "k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/kubereviewsubject"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// ConvertClusterRoleNameFromTeamRole converts the cluster role name from the team subject role.
func ConvertClusterRoleNameFromTeamRole(role server.TeamRole) (clusterRoleName string) {
	switch role {
	case server.TeamRoleOwner:
		return AdminClusterRoleName
	case server.TeamRoleMember:
		return EditorClusterRoleName
	default:
		return ViewerClusterRoleName
	}
}

// ConvertTeamRoleFromClusterRoleName converts the team role from the cluster role name.
//
// If the cluster role name is not recognized, it returns an empty string.
func ConvertTeamRoleFromClusterRoleName(clusterRoleName string) (role server.TeamRole) {
	switch clusterRoleName {
	case AdminClusterRoleName:
		return server.TeamRoleOwner
	case EditorClusterRoleName:
		return server.TeamRoleMember
	case ViewerClusterRoleName:
		return server.TeamRoleViewer
	}
	return ""
}

// GrantTeamSubjects (re)grants the given team role to the corresponding subjects.
func GrantTeamSubjects(ctx context.Context, cli ctrlcli.Client, teamSubjs *server.TeamSubjects) error {
	if teamSubjs == nil || len(teamSubjs.Items) == 0 {
		return nil
	}

	for i := range teamSubjs.Items {
		tsbj := &teamSubjs.Items[i]

		eRb := &rbac.RoleBinding{
			ObjectMeta: meta.ObjectMeta{
				Namespace: teamSubjs.Name,
				Name:      GetTeamSubjectRoleBindingName(tsbj),
			},
			RoleRef: rbac.RoleRef{
				APIGroup: rbac.GroupName,
				Kind:     "ClusterRole",
				Name:     ConvertClusterRoleNameFromTeamRole(tsbj.Role),
			},
			Subjects: []rbac.Subject{
				{
					Kind:      rbac.ServiceAccountKind,
					Namespace: tsbj.Namespace,
					Name:      ConvertServiceAccountNameFromSubjectName(tsbj.Name),
				},
				{
					APIGroup: rbac.GroupName,
					Kind:     rbac.UserKind,
					Name:     ConvertImpersonateUserFromSubjectName(tsbj.Namespace, tsbj.Name),
				},
			},
		}
		systemmeta.NoteResource(eRb, "rolebindings", map[string]string{
			"scope":   "team",
			"team":    kubemeta.GetNamespacedNameKey(teamSubjs),
			"subject": kubemeta.GetNamespacedNameKey(tsbj.ToNamespacedName()),
		})

		// Create.
		_, err := kubeclientset.CreateWithCtrlClient(ctx, cli, eRb,
			kubeclientset.WithRecreateIfDuplicated(kubeclientset.NewRbacRoleBindingCompareFunc(eRb)))
		if err != nil {
			return fmt.Errorf("create role binding: %w", err)
		}
	}

	return nil
}

// RevokeTeamSubjects revokes the team role from the corresponding subjects.
func RevokeTeamSubjects(ctx context.Context, cli ctrlcli.Client, teamSubjs *server.TeamSubjects) error {
	if teamSubjs == nil || len(teamSubjs.Items) == 0 {
		return nil
	}

	for i := range teamSubjs.Items {
		tsbj := &teamSubjs.Items[i]

		eRb := &rbac.RoleBinding{
			ObjectMeta: meta.ObjectMeta{
				Namespace: teamSubjs.Name,
				Name:      GetTeamSubjectRoleBindingName(tsbj),
			},
		}

		// Delete.
		err := kubeclientset.DeleteWithCtrlClient(ctx, cli, eRb)
		if err != nil {
			return fmt.Errorf("delete role binding: %w", err)
		}
	}

	return nil
}

// GrantTeamSubjectRole (re)grants the given team role for the request user.
func GrantTeamSubjectRole(ctx context.Context, cli ctrlcli.Client, team *server.Team, role server.TeamRole) error {
	ui, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		return errors.New("request user not found")
	}

	return GrantTeamSubjectRoleFor(ctx, cli, team, role, ui)
}

// GrantTeamSubjectRoleFor (re)grants the given for the specified user.
func GrantTeamSubjectRoleFor(ctx context.Context, cli ctrlcli.Client, team *server.Team, role server.TeamRole, user authnuser.Info) error { // nolint:lll
	// Validate.
	if team == nil || team.Name == "" {
		return errors.New("empty team")
	}
	if err := role.Validate(); err != nil {
		return err
	}

	// Return directly if the user has the global management permissions.
	revs := kubereviewsubject.Reviews{
		{
			ResourceAttributes: &kubereviewsubject.ResourceAttributes{
				Group:    server.GroupName,
				Resource: "teams",
				Verb:     rbac.VerbAll,
			},
		},
	}
	err := kubereviewsubject.CanSpecificUserDoWithCtrlClient(ctx, cli, revs, user)
	switch {
	case err == nil:
		return nil
	case !kubereviewsubject.IsDeniedError(err):
		return fmt.Errorf("check global management permissions: %w", err)
	}

	// Convert.
	var subjRef server.SubjectReference
	{
		subjNamespace, subjName, ok := ConvertSubjectNamesFromAuthnUser(user)
		if !ok {
			return errors.New("incomplete user")
		}
		subjRef = server.SubjectReference{
			Namespace: subjNamespace,
			Name:      subjName,
		}
	}

	eRb := &rbac.RoleBinding{
		ObjectMeta: meta.ObjectMeta{
			Namespace: team.Name,
			Name:      GetTeamSubjectRoleBindingNameOfSubjectReference(&subjRef),
		},
		RoleRef: rbac.RoleRef{
			APIGroup: rbac.GroupName,
			Kind:     "ClusterRole",
			Name:     ConvertClusterRoleNameFromTeamRole(role),
		},
		Subjects: []rbac.Subject{
			{
				Kind:      rbac.ServiceAccountKind,
				Namespace: subjRef.Namespace,
				Name:      ConvertServiceAccountNameFromSubjectName(subjRef.Name),
			},
			{
				APIGroup: rbac.GroupName,
				Kind:     rbac.UserKind,
				Name:     ConvertImpersonateUserFromSubjectName(subjRef.Namespace, subjRef.Name),
			},
		},
	}
	systemmeta.NoteResource(eRb, "rolebindings", map[string]string{
		"scope":   "team",
		"team":    kubemeta.GetNamespacedNameKey(team),
		"subject": kubemeta.GetNamespacedNameKey(subjRef.ToNamespacedName()),
	})

	// Create.
	_, err = kubeclientset.CreateWithCtrlClient(ctx, cli, eRb,
		kubeclientset.WithRecreateIfDuplicated(kubeclientset.NewRbacRoleBindingCompareFunc(eRb)))
	if err != nil {
		return fmt.Errorf("create role binding: %w", err)
	}
	return nil
}

// GetTeamSubjectRoleBindingName returns the role binding name for the team subject.
func GetTeamSubjectRoleBindingName(tsbj *server.TeamSubject) string {
	return GetTeamSubjectRoleBindingNameOfSubjectReference(&tsbj.SubjectReference)
}

// GetTeamSubjectRoleBindingNameOfSubjectReference returns the role binding name for the subject reference.
func GetTeamSubjectRoleBindingNameOfSubjectReference(sbjRef *server.SubjectReference) string {
	return fmt.Sprintf("gpustack-team-subject-%s",
		stringx.SumByFNV64a(sbjRef.Namespace, sbjRef.Name))
}
