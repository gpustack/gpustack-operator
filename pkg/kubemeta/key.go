package kubemeta

import (
	"strings"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ContainsNamespaceInNamespacedNameKey returns true if the given namespace is in the namespaced name key.
func ContainsNamespaceInNamespacedNameKey(namespace, key string) bool {
	if namespace == "" || key == "" {
		return false
	}

	ss := strings.Split(key, "/")
	if len(ss) > 2 {
		return false
	}

	if len(ss) == 1 {
		return false
	}
	return ss[0] == namespace
}

// ContainsNameInNamespacedNameKey returns true if the given name is in the namespaced name key.
func ContainsNameInNamespacedNameKey(name, key string) bool {
	if name == "" || key == "" {
		return false
	}

	ss := strings.Split(key, "/")
	if len(ss) > 2 {
		return false
	}

	if len(ss) == 1 {
		return ss[0] == name
	}
	return ss[1] == name
}

// GetNamespacedNameKey returns the string representation of the namespaced name.
//
// The given object must be either types.NamespacedName or meta v1.Object,
// otherwise, it returns an empty string.
func GetNamespacedNameKey(obj any) (key string) {
	switch t := obj.(type) {
	case types.NamespacedName:
		return t.String()
	case *types.NamespacedName:
		return t.String()
	case meta.Object:
		return (types.NamespacedName{
			Namespace: t.GetNamespace(),
			Name:      t.GetName(),
		}).String()
	}
	return ""
}

// ParseNamespacedNameKey parses the string representation of the namespaced name.
//
// It returns nil if the given string is empty or invalid.
func ParseNamespacedNameKey(key string) (objKey *types.NamespacedName) {
	if key == "" {
		return nil
	}

	ss := strings.Split(key, "/")
	if len(ss) > 2 {
		return nil
	}

	if len(ss) == 1 {
		return &types.NamespacedName{
			Name: ss[0],
		}
	}
	return &types.NamespacedName{
		Namespace: ss[0],
		Name:      ss[1],
	}
}
