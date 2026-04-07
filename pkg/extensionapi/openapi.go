package extensionapi

import "k8s.io/kube-openapi/pkg/common"

// OpenAPIDefinitionGetter is a function type that returns a map of OpenAPI definitions.
type OpenAPIDefinitionGetter = func(ref common.ReferenceCallback) map[string]common.OpenAPIDefinition

// MergeOpenAPIDefinitionsGetter merges multiple OpenAPIDefinitionGetter into one.
func MergeOpenAPIDefinitionsGetter(getters ...OpenAPIDefinitionGetter) OpenAPIDefinitionGetter {
	return func(ref common.ReferenceCallback) map[string]common.OpenAPIDefinition {
		ret := getters[0](ref)
		for i := 1; i < len(getters); i++ {
			for k, v := range getters[i](ref) {
				if _, ok := ret[k]; ok {
					continue
				}
				ret[k] = v
			}
		}
		return ret
	}
}
