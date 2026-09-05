package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// memberSchema returns the schema of one member group as the generated CRD carries it.
func memberSchema(t *testing.T) extension.JSONSchemaProps {
	t.Helper()

	crd := GetCustomResourceDefinitions()["KVCacheBackend"]
	require.NotNil(t, crd, "KVCacheBackend is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	// Walked rather than reached for by a single path expression, so a rename anywhere along the
	// way fails here with the level that moved instead of a nil dereference.
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, level := range []string{"spec", "connection", "managed", "members"} {
		next, ok := schema.Properties[level]
		require.True(t, ok, "the schema has no %q under the path to a member group", level)
		schema = &next
	}
	require.NotNil(t, schema.Items, "members is a list and its item schema is what carries the fields")
	require.NotNil(t, schema.Items.Schema)

	return *schema.Items.Schema
}

// TestKVCacheBackendMediumEnumCarriesOnlyWhatRuns is the only automated guard on the narrowed enum,
// and it guards a claim nothing else can reach: the four removed values are refused by the SCHEMA,
// in rest.BeforeCreate, before any webhook runs — so no admission test can cover them, and the
// webhook rule that used to refuse them is gone precisely because no request reaches it any more.
//
// What it is really protecting against is a value being put back. Each of the four named something
// that is not a member group at all: a local disk belongs to the group holding the memory replica
// (it is members[].localDisk), an NVMe-oF namespace is a target coordinate with no node affinity,
// and CXL and DFS are configured on the leader's own process. Widening this enum without moving the
// renderer would bring back a group that reports capacity it never fills.
func TestKVCacheBackendMediumEnumCarriesOnlyWhatRuns(t *testing.T) {
	medium, ok := memberSchema(t).Properties["medium"]
	require.True(t, ok, "a member group must still name what its segment is made of")

	values := make([]string, 0, len(medium.Enum))
	for _, entry := range medium.Enum {
		values = append(values, string(entry.Raw))
	}

	assert.Equal(t, []string{`"DRAM"`}, values,
		"the enum carries exactly the medium that runs; adding one without a renderer for it is how "+
			"a member group comes to report a tier it never fills")

	// The guidance has to live somewhere a reader looks, and with the values gone the field's own
	// description is the only place left: a value outside an enum is refused before any webhook
	// runs, so no message of ours can reach the operator who tried LocalDisk.
	assert.Contains(t, medium.Description, "localDisk",
		"kubectl explain is where someone who tried LocalDisk finds out where a disk tier is declared")
}

// TestKVCacheBackendDiskTierRequiresItsPath pins that the tier cannot be declared without saying
// where on the node it lives.
//
// The path has no default on purpose — the store defaults it to a directory of its own, and picking
// a host directory on somebody else's nodes is not a default this operator may take. Required is
// what turns that decision into an apply-time error rather than a mount nobody asked for.
func TestKVCacheBackendDiskTierRequiresItsPath(t *testing.T) {
	localDisk, ok := memberSchema(t).Properties["localDisk"]
	require.True(t, ok, "a member group must be able to declare a disk tier")

	assert.Equal(t, []string{"path"}, localDisk.Required,
		"the path is required and the capacity is not: an unset capacity means the store's own "+
			"ceiling, while an unset path would mean a host directory this operator chose")
}
