package authz

import (
	"context"
	"fmt"
	"strings"

	rbac "k8s.io/api/rbac/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	authnuser "k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const (
	_ServiceAccountNamePrefix     = "gpustack-subject-"
	_ServiceAccountUsernamePrefix = "system:serviceaccount:"
	_ImpersonateUsernamePrefix    = "gpustack:"
	_UsernameSeparator            = ":"
)

// ConvertImpersonateUserFromSubjectName converts the subject {namespace,name} pair to an impersonate user.
func ConvertImpersonateUserFromSubjectName(subjNamespace, subjName string) (impersonateUser string) {
	return _ImpersonateUsernamePrefix + subjNamespace + _UsernameSeparator + subjName
}

// ConvertSubjectNamesFromImpersonateUser converts the impersonate user to the subject {namespace,name} pair.
//
// If the impersonate user is not a subject-delegated impersonate user,
// it returns empty strings.
func ConvertSubjectNamesFromImpersonateUser(impersonateUser string) (subjNamespace, subjName string, ok bool) {
	subjNamespace, subjName, ok = strings.Cut(strings.TrimPrefix(impersonateUser, _ImpersonateUsernamePrefix), _UsernameSeparator)
	if ok && subjNamespace != "" && subjName != "" {
		return subjNamespace, subjName, true
	}
	return "", "", false
}

// ConvertServiceAccountNameFromSubjectName converts the subject name to a service account name.
func ConvertServiceAccountNameFromSubjectName(subjName string) (saName string) {
	return _ServiceAccountNamePrefix + subjName
}

// ConvertSubjectNameFromServiceAccountName converts the service account name to a subject name.
//
// If the service account name is not a subject-delegated service account name,
// it returns an empty string.
func ConvertSubjectNameFromServiceAccountName(saName string) (subjName string) {
	subjName = strings.TrimPrefix(saName, _ServiceAccountNamePrefix)
	if subjName != saName {
		return subjName
	}
	return ""
}

// ConvertSubjectNamesFromJwtSubject converts the JWT subject to the subject {namespace,name} pair.
//
// If the JWT subject is not a subject-delegated JWT subject,
// it returns empty strings.
func ConvertSubjectNamesFromJwtSubject(jwtSubject string) (subjNamespace, subjName string, ok bool) {
	subjNamespace, subjName, ok = strings.Cut(strings.TrimPrefix(jwtSubject, _ServiceAccountUsernamePrefix), _UsernameSeparator)
	if ok {
		subjName = ConvertSubjectNameFromServiceAccountName(subjName)
	}
	if subjNamespace != "" && subjName != "" {
		return subjNamespace, subjName, true
	}
	return "", "", false
}

// ConvertSubjectNamesFromAuthnUser converts the authentication user to the subject {namespace,name} pair.
//
// If the authentication user is not a subject-delegated authentication user(impersonate user or service account),
// it returns empty strings.
func ConvertSubjectNamesFromAuthnUser(user authnuser.Info) (subjNamespace, subjName string, ok bool) {
	un := user.GetName()
	switch {
	case strings.HasPrefix(un, _ImpersonateUsernamePrefix):
		return ConvertSubjectNamesFromImpersonateUser(un)
	case strings.HasPrefix(un, _ServiceAccountUsernamePrefix):
		return ConvertSubjectNamesFromJwtSubject(un)
	}
	return "", "", false
}

// GrantSubject (re)grants the role of the subject for the subject.
func GrantSubject(ctx context.Context, cli ctrlcli.Client, subj *server.Subject) error {
	n := GetSubjectRoleBindingName(subj)
	imName := ConvertImpersonateUserFromSubjectName(subj.Namespace, subj.Name)
	saName := ConvertServiceAccountNameFromSubjectName(subj.Name)

	// Private role and role binding.
	eP := []ctrlcli.Object{
		&rbac.Role{
			ObjectMeta: meta.ObjectMeta{
				Namespace: subj.Namespace,
				Name:      fmt.Sprintf("%s-selfreview", n),
			},
			Rules: []rbac.PolicyRule{
				{
					APIGroups: []string{
						server.GroupName,
					},
					Resources: []string{
						"subjects",
					},
					ResourceNames: []string{
						subj.Name,
					},
					Verbs: []string{
						"get",
						"update",
						"patch",
					},
				},
				{
					APIGroups: []string{
						server.GroupName,
					},
					Resources: []string{
						"subjects/login",
						"subjects/token",
					},
					ResourceNames: []string{
						subj.Name,
					},
					Verbs: []string{
						"create",
					},
				},
			},
		},
		&rbac.RoleBinding{
			ObjectMeta: meta.ObjectMeta{
				Namespace: subj.Namespace,
				Name:      fmt.Sprintf("%s-selfreview", n),
			},
			RoleRef: rbac.RoleRef{
				APIGroup: rbac.GroupName,
				Kind:     "Role",
				Name:     fmt.Sprintf("%s-selfreview", n),
			},
			Subjects: []rbac.Subject{
				{
					Kind:      rbac.ServiceAccountKind,
					Namespace: subj.Namespace,
					Name:      saName,
				},
				{
					APIGroup: rbac.GroupName,
					Kind:     rbac.UserKind,
					Name:     imName,
				},
			},
		},
	}
	for i := range eP {
		switch eP[i].(type) {
		case *rbac.Role:
			systemmeta.NoteResource(eP[i], "roles", nil)
		case *rbac.RoleBinding:
			systemmeta.NoteResource(eP[i], "rolebindings", map[string]string{
				"scope":   "organization",
				"subject": kubemeta.GetNamespacedNameKey(subj),
			})
		}

		// Create.
		kubemeta.ControlOnWithoutBlock(eP[i], subj, server.SchemeGroupVersionKind("Subject"))
		_, err := kubeclientset.CreateWithCtrlClient(ctx, cli, eP[i])
		if err != nil {
			return fmt.Errorf("create role or role binding: %w", err)
		}
	}

	// Public role binding.
	switch subj.Spec.Role {
	case server.SubjectRoleManager:
		eRb := &rbac.RoleBinding{
			ObjectMeta: meta.ObjectMeta{
				Namespace: subj.Namespace,
				Name:      n,
			},
			RoleRef: rbac.RoleRef{
				APIGroup: rbac.GroupName,
				Kind:     "ClusterRole",
				Name:     EditorClusterRoleName,
			},
			Subjects: []rbac.Subject{
				{
					Kind:      rbac.ServiceAccountKind,
					Namespace: subj.Namespace,
					Name:      saName,
				},
				{
					APIGroup: rbac.GroupName,
					Kind:     rbac.UserKind,
					Name:     imName,
				},
			},
		}
		systemmeta.NoteResource(eRb, "rolebindings", map[string]string{
			"scope":   "organization",
			"subject": kubemeta.GetNamespacedNameKey(subj),
		})

		// Create.
		kubemeta.ControlOnWithoutBlock(eRb, subj, server.SchemeGroupVersionKind("Subject"))
		_, err := kubeclientset.CreateWithCtrlClient(ctx, cli, eRb,
			kubeclientset.WithRecreateIfDuplicated(kubeclientset.NewRbacRoleBindingCompareFunc(eRb)))
		if err != nil {
			return fmt.Errorf("create role binding: %w", err)
		}
		return nil
	case server.SubjectRoleAdmin:
		eCrb := &rbac.ClusterRoleBinding{
			ObjectMeta: meta.ObjectMeta{
				Name: n,
			},
			RoleRef: rbac.RoleRef{
				APIGroup: rbac.GroupName,
				Kind:     "ClusterRole",
				Name:     AdminClusterRoleName,
			},
			Subjects: []rbac.Subject{
				{
					Kind:      rbac.ServiceAccountKind,
					Namespace: subj.Namespace,
					Name:      saName,
				},
				{
					APIGroup: rbac.GroupName,
					Kind:     rbac.UserKind,
					Name:     imName,
				},
			},
		}
		systemmeta.NoteResource(eCrb, "rolebindings", map[string]string{
			"scope":   "organization",
			"subject": kubemeta.GetNamespacedNameKey(subj),
		})

		// Create.
		kubemeta.ControlOnWithoutBlock(eCrb, subj, server.SchemeGroupVersionKind("Subject"))
		_, err := kubeclientset.CreateWithCtrlClient(ctx, cli, eCrb,
			kubeclientset.WithRecreateIfDuplicated(kubeclientset.NewRbacClusterRoleBindingCompareFunc(eCrb)))
		if err != nil {
			return fmt.Errorf("create cluster role binding: %w", err)
		}
		return nil
	}

	return nil
}

// RevokeSubject revokes the role of the subject from the subject.
func RevokeSubject(ctx context.Context, cli ctrlcli.Client, subj *server.Subject) error {
	n := GetSubjectRoleBindingName(subj)

	switch subj.Spec.Role {
	case server.SubjectRoleManager:
		eRb := &rbac.RoleBinding{
			ObjectMeta: meta.ObjectMeta{
				Namespace: subj.Namespace,
				Name:      n,
			},
		}

		// Delete.
		return kubeclientset.DeleteWithCtrlClient(ctx, cli, eRb,
			kubeclientset.WithDeleteMetaOptions(meta.DeleteOptions{
				PropagationPolicy: ptr.To(meta.DeletePropagationForeground),
			}))
	case server.SubjectRoleAdmin:
		eCrb := &rbac.ClusterRoleBinding{
			ObjectMeta: meta.ObjectMeta{
				Name: n,
			},
		}

		// Delete.
		return kubeclientset.DeleteWithCtrlClient(ctx, cli, eCrb,
			kubeclientset.WithDeleteMetaOptions(meta.DeleteOptions{
				PropagationPolicy: ptr.To(meta.DeletePropagationForeground),
			}))
	}

	return nil
}

// GetSubjectRoleBindingName gets the role binding name for the subject.
func GetSubjectRoleBindingName(subj *server.Subject) string {
	return fmt.Sprintf("gpustack-subject-%s",
		stringx.SumByFNV64a(subj.Namespace, subj.Name))
}
