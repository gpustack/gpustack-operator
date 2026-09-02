package v1alpha1

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestKVCachePoolCRDScopes pins the split the whole design rests on: the pool is the cluster-scoped
// quota domain and the Binding is the namespaced provisioning point. A scope marker that silently
// flipped would make a Binding grantable by nobody, or a pool writable by every namespace.
func TestKVCachePoolCRDScopes(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		wantScope extension.ResourceScope
	}{
		{name: "the quota domain is cluster-scoped", kind: "KVCachePool", wantScope: extension.ClusterScoped},
		{
			name: "the provisioning point is namespaced",
			kind: "KVCachePoolBinding", wantScope: extension.NamespaceScoped,
		},
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

// TestKVCachePoolBindingStatusFiguresAreNotRequired asserts the rendered schema requires none of the
// figures the operator may be unable to observe.
//
// A required entry here would be unfixable once published: a status write that omits an unobserved
// figure — which is the contract for every one of them — would be refused by the API server, taking
// the whole status update with it, and the object would freeze at its last value with nothing saying
// why.
func TestKVCachePoolBindingStatusFiguresAreNotRequired(t *testing.T) {
	crd := GetCustomResourceDefinitions()["KVCachePoolBinding"]
	require.NotNil(t, crd)

	status, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	require.True(t, ok, "the Binding renders no status schema")

	for _, figure := range []string{
		"requestedQuota", "effectiveQuota", "usage", "overQuota", "blocks", "hitRate",
	} {
		assert.NotContains(t, status.Required, figure)
	}
}

// TestKVCachePoolBindingStatusKeepsObservedZeroes is the other half of the absence contract, and the
// half a schema cannot express: a figure that WAS observed must survive serialization even when what
// was observed is zero or false.
//
// Without it, "not over quota" and "never scraped" are the same bytes on the wire, and the healthy
// case is the one that disappears.
func TestKVCachePoolBindingStatusKeepsObservedZeroes(t *testing.T) {
	zero := resource.MustParse("0")
	noBlocks := int64(0)
	notOver := false

	encoded, err := json.Marshal(KVCachePoolBindingStatus{
		RequestedQuota: &zero,
		EffectiveQuota: &zero,
		Usage:          &zero,
		OverQuota:      &notOver,
		Blocks:         &noBlocks,
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	for _, figure := range []string{"requestedQuota", "effectiveQuota", "usage", "overQuota", "blocks"} {
		assert.Contains(t, decoded, figure, "an observed zero must stay on the wire, and %s did not", figure)
	}
	assert.Equal(t, false, decoded["overQuota"])
}

// TestKVCachePoolHitRatePattern pins the ratio format BOTH kinds render, because the pattern is a
// constraint on the writer and not only on a client: a hit rate that fails it fails the whole status
// write, freezing every other figure at its last value.
//
// The accepted set is what a ratio in [0,1] looks like when this operator formats it. The refused set
// is every spelling that would arrive from formatting a float without deciding its precision.
func TestKVCachePoolHitRatePattern(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "a measured rate", value: "0.87", want: true},
		{name: "four decimals", value: "0.8712", want: true},
		{name: "a cold cache", value: "0", want: true},
		{name: "everything hit", value: "1", want: true},
		{name: "everything hit, padded", value: "1.0000", want: true},
		{name: "above one is not a ratio", value: "1.5", want: false},
		{name: "negative is not a ratio", value: "-0.1", want: false},
		{name: "float formatting overruns the precision", value: "0.87123", want: false},
		{name: "scientific notation", value: "8.7e-1", want: false},
		{name: "a percentage is a different unit", value: "87%", want: false},
	}

	crd := GetCustomResourceDefinitions()["KVCachePoolBinding"]
	require.NotNil(t, crd)
	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	hitRate, ok := status.Properties["hitRate"]
	require.True(t, ok)
	require.NotEmpty(t, hitRate.Pattern, "hitRate renders no pattern, so nothing constrains the writer")

	pattern := regexp.MustCompile(hitRate.Pattern)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, pattern.MatchString(c.value))
		})
	}
}

// TestKVCachePoolBindingStatusOmitsUnobservedFigures is the test that guards the pointer decision,
// and it is separate from the schema one because the schema cannot catch it: a value-held
// resource.Quantity is just as un-required as a pointer, and serializes as "0" anyway.
//
// A quota the operator refused to write and a quota that really is zero must not look the same to a
// client. Held by value they do, because omitempty does not omit a zero-valued struct.
func TestKVCachePoolBindingStatusOmitsUnobservedFigures(t *testing.T) {
	encoded, err := json.Marshal(KVCachePoolBindingStatus{})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	assert.Empty(t, decoded, "an unobserved status must serialize to nothing at all, and it carried %s", encoded)
}

// TestKVCachePoolStatusOmitsUnobservedUsage is the pool-side half of the same guarantee: usage is
// absent until a scrape succeeds, and absent is not a cache that is empty.
func TestKVCachePoolStatusOmitsUnobservedUsage(t *testing.T) {
	encoded, err := json.Marshal(KVCachePoolStatus{})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	assert.Empty(t, decoded, "an unobserved pool status must serialize to nothing at all, and it carried %s", encoded)
}
