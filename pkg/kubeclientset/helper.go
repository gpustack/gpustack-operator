package kubeclientset

import (
	"slices"

	rbac "k8s.io/api/rbac/v1"

	"gpustack.ai/gpustack/pkg/kubemeta"
)

// NewRbacRoleBindingCompareFunc returns a CompareWithFn that compares two rbac.RoleBindings.
func NewRbacRoleBindingCompareFunc(eRb *rbac.RoleBinding) CompareWithFn[*rbac.RoleBinding] {
	return func(aRb *rbac.RoleBinding) bool {
		if !kubemeta.DeepEqual(eRb.RoleRef, aRb.RoleRef) {
			return false
		}
		for i := range eRb.Subjects {
			if !slices.ContainsFunc(aRb.Subjects, func(s rbac.Subject) bool {
				return kubemeta.DeepEqual(eRb.Subjects[i], s)
			}) {
				return false
			}
		}
		return true
	}
}

// NewRbacClusterRoleBindingCompareFunc returns a CompareWithFn that compares two rbac.ClusterRoleBindings.
func NewRbacClusterRoleBindingCompareFunc(eCrb *rbac.ClusterRoleBinding) CompareWithFn[*rbac.ClusterRoleBinding] {
	return func(aCrb *rbac.ClusterRoleBinding) bool {
		if !kubemeta.DeepEqual(eCrb.RoleRef, aCrb.RoleRef) {
			return false
		}
		for i := range eCrb.Subjects {
			if !slices.ContainsFunc(aCrb.Subjects, func(s rbac.Subject) bool {
				return kubemeta.DeepEqual(eCrb.Subjects[i], s)
			}) {
				return false
			}
		}
		return true
	}
}

// NewRbacRoleCompareFunc returns a CompareWithFn that compares two rbac.Roles.
func NewRbacRoleCompareFunc(eR *rbac.Role) CompareWithFn[*rbac.Role] {
	return func(aR *rbac.Role) bool {
		for i := range eR.Rules {
			if !slices.ContainsFunc(aR.Rules, func(r rbac.PolicyRule) bool {
				return kubemeta.DeepEqual(eR.Rules[i], r)
			}) {
				return false
			}
		}
		return true
	}
}

// NewRbacClusterRoleCompareFunc returns a CompareWithFn that compares two rbac.ClusterRoles.
func NewRbacClusterRoleCompareFunc(eCr *rbac.ClusterRole) CompareWithFn[*rbac.ClusterRole] {
	return func(aCr *rbac.ClusterRole) bool {
		for i := range eCr.Rules {
			if !slices.ContainsFunc(aCr.Rules, func(r rbac.PolicyRule) bool {
				return kubemeta.DeepEqual(eCr.Rules[i], r)
			}) {
				return false
			}
		}
		return true
	}
}

// NewRbacRoleAlignFunc returns an AlignWithFn that aligns an existing rbac.Role with the given rbac.Role.
func NewRbacRoleAlignFunc(eR *rbac.Role) AlignWithFn[*rbac.Role] {
	return func(aR *rbac.Role) (_ *rbac.Role, skip bool, _ error) {
		skip = true
		for i := range eR.Rules {
			if slices.ContainsFunc(aR.Rules, func(r rbac.PolicyRule) bool {
				return kubemeta.DeepEqual(eR.Rules[i], r)
			}) {
				continue
			}

			aR.Rules = append(aR.Rules, eR.Rules[i])
			skip = false
		}
		return aR, skip, nil
	}
}

// NewRbacClusterRoleAlignFunc returns an AlignWithFn that aligns an existing rbac.ClusterRole with the given rbac.ClusterRole.
func NewRbacClusterRoleAlignFunc(eCr *rbac.ClusterRole) AlignWithFn[*rbac.ClusterRole] {
	return func(aCr *rbac.ClusterRole) (_ *rbac.ClusterRole, skip bool, _ error) {
		skip = true
		for i := range eCr.Rules {
			if slices.ContainsFunc(aCr.Rules, func(r rbac.PolicyRule) bool {
				return kubemeta.DeepEqual(eCr.Rules[i], r)
			}) {
				continue
			}

			aCr.Rules = append(aCr.Rules, eCr.Rules[i])
			skip = false
		}
		return aCr, skip, nil
	}
}
