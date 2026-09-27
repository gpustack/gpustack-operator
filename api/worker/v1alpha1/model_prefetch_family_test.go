package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// TestModelPrefetchFamilyCRDScopes pins the scope split of the prefetch family: the store is a
// cluster-scoped pool object, the binding is the namespaced provisioning point, and the prefetch
// is a tenant object. A flipped scope marker would make the store unmanageable, or a binding
// grantable by nobody.
func TestModelPrefetchFamilyCRDScopes(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		wantScope extension.ResourceScope
	}{
		{name: "the store is cluster-scoped", kind: "ModelStore", wantScope: extension.ClusterScoped},
		{
			name: "the provisioning point is namespaced",
			kind: "ModelStoreBinding", wantScope: extension.NamespaceScoped,
		},
		{name: "the prefetch is namespaced", kind: "ModelPrefetch", wantScope: extension.NamespaceScoped},
	}
	crds := GetCustomResourceDefinitions()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			crd := crds[c.kind]
			require.NotNil(t, crd, "%s is not registered", c.kind)
			assert.Equal(t, c.wantScope, crd.Spec.Scope)
			require.Len(t, crd.Spec.Versions, 1)
			assert.NotNil(t, crd.Spec.Versions[0].Subresources.Status,
				"status must be a subresource, or the controller's status writes take the spec with them")
		})
	}
}

// TestModelStoreOverridesAreOptional asserts the rendered schema requires only the selector: a
// required policy field would force every store to restate the cluster defaults it means to leave
// alone, and a later cluster-default change would silently stop reaching the matched nodes.
func TestModelStoreOverridesAreOptional(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelStore"]
	require.NotNil(t, crd)

	spec, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	require.True(t, ok, "the store renders no spec schema")
	assert.Contains(t, spec.Required, "nodeSelector")
	assert.NotContains(t, spec.Required, "watermarks")
	assert.NotContains(t, spec.Required, "download")
}

// TestModelStoreBindingStoreRefsAreBounded asserts the grant renders as a set with at least one
// member: an empty storeRefs would be a binding that grants nothing while reporting Ready.
func TestModelStoreBindingStoreRefsAreBounded(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelStoreBinding"]
	require.NotNil(t, crd)

	spec, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	require.True(t, ok, "the binding renders no spec schema")
	refs, ok := spec.Properties["storeRefs"]
	require.True(t, ok, "the binding renders no storeRefs schema")
	assert.Contains(t, spec.Required, "storeRefs")
	assert.NotNil(t, refs.XListType, "storeRefs must carry list semantics")
	assert.Equal(t, "atomic", *refs.XListType)
	require.NotNil(t, refs.MinItems, "storeRefs must refuse the empty grant")
	assert.Equal(t, int64(1), *refs.MinItems)
}

// TestNodeModelStoreWorkerFieldsAreOptional asserts the prefetch-era fields the worker writes stay
// optional: requiring them would strand every store that predates them, and the plugin reads a
// node's object the moment it registers, long before any prefetch or store exists.
func TestNodeModelStoreWorkerFieldsAreOptional(t *testing.T) {
	crd := GetCustomResourceDefinitions()["NodeModelStore"]
	require.NotNil(t, crd)

	spec, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	require.True(t, ok, "the node model store renders no spec schema")
	assert.NotContains(t, spec.Required, "store")
	assert.NotContains(t, spec.Required, "pinned")

	pinned, ok := spec.Properties["pinned"]
	require.True(t, ok, "the spec renders no pinned schema")
	require.NotNil(t, pinned.XListType, "pinned must carry list semantics")
	assert.Equal(t, "set", *pinned.XListType, "pinned is a set of digests, or a duplicated digest is a schema error")
}
