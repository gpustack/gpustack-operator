package authz

import (
	"slices"

	authnuser "k8s.io/apiserver/pkg/authentication/user"

	"gpustack.ai/gpustack/pkg/server/kuberess"
)

// IsWellKnownAdminUser checks if the user is a well-known admin user.
func IsWellKnownAdminUser(user authnuser.Info) bool {
	if user.GetName() == "system:admin" {
		return true
	}
	if slices.Contains(user.GetGroups(), "system:masters") {
		return true
	}
	subjNamespace, subjName, ok := ConvertSubjectNamesFromAuthnUser(user)
	if !ok {
		return false
	}
	return subjNamespace == kuberess.SystemNamespaceName && subjName == kuberess.AdminSubjectName
}
