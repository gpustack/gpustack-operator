package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/utils/ptr"
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

// memberStatusListSchema returns status.members as the generated CRD carries it.
func memberStatusListSchema(t *testing.T) extension.JSONSchemaProps {
	t.Helper()

	crd := GetCustomResourceDefinitions()["KVCacheBackend"]
	require.NotNil(t, crd, "KVCacheBackend is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, level := range []string{"status", "members"} {
		next, ok := schema.Properties[level]
		require.True(t, ok, "the schema has no %q under the path to status members", level)
		schema = &next
	}
	require.NotNil(t, schema.Items)
	require.NotNil(t, schema.Items.Schema)

	return *schema
}

// TestKVCacheBackendMembersAreKeyedBySegmentID guards the wire shape that host-network members
// expose. Several members on one node legitimately share segmentName, while segmentID remains unique;
// keying this list by the name makes the API server reject the status update that reports them.
func TestKVCacheBackendMembersAreKeyedBySegmentID(t *testing.T) {
	members := memberStatusListSchema(t)

	require.NotNil(t, members.XListType)
	assert.Equal(t, "map", *members.XListType)
	assert.Equal(t, []string{"segmentID"}, members.XListMapKeys)
	assert.ElementsMatch(t, []string{"segmentID", "clientID", "segmentName"},
		members.Items.Schema.Required)
}

// TestKVCacheBackendMediumEnumCarriesOnlyWhatRuns is the only automated guard on the enum, and
// it guards a claim nothing else can reach: a value outside the enum is refused by the SCHEMA,
// in rest.BeforeCreate, before any webhook runs — so no admission test can cover it, and a value
// added here without a renderer behind it would be admitted with nothing to fail.
//
// What it is really protecting against is a value being added without its renderer. DRAM and
// VRAM each have one. A local disk belongs to the group holding the memory replica (it is
// members[].localDisks), an NVMe-oF namespace is a target coordinate with no node affinity, and
// CXL and DFS are configured on the leader's own process: none of them is a member group, so
// none of them belongs here.
func TestKVCacheBackendMediumEnumCarriesOnlyWhatRuns(t *testing.T) {
	medium, ok := memberSchema(t).Properties["medium"]
	require.True(t, ok, "a member group must still name what its segment is made of")

	values := make([]string, 0, len(medium.Enum))
	for _, entry := range medium.Enum {
		values = append(values, string(entry.Raw))
	}

	assert.Equal(t, []string{`"DRAM"`, `"VRAM"`}, values,
		"the enum carries exactly the media that run; adding one without a renderer for it is how "+
			"a member group comes to report a tier it never fills")

	// The guidance has to live somewhere a reader looks, and with the values gone the field's own
	// description is the only place left: a value outside an enum is refused before any webhook
	// runs, so no message of ours can reach the operator who tried LocalDisk.
	assert.Contains(t, medium.Description, "localDisks",
		"kubectl explain is where someone who tried LocalDisk finds out where a disk tier is declared")
}

// TestKVCacheBackendDiskTierRequiresItsPath pins that the tier cannot be declared without saying
// where on the node it lives.
//
// The path has no default on purpose — the store defaults it to a directory of its own, and picking
// a host directory on somebody else's nodes is not a default this operator may take. Required is
// what turns that decision into an apply-time error rather than a mount nobody asked for.
//
// The list is KEYED BY PATH rather than merely carrying it, which is what lets the schema refuse a
// second entry naming the same directory with no webhook involved: one key, one tier, and the
// refusal arrives on the same write the entry did.
func TestKVCacheBackendDiskTierRequiresItsPath(t *testing.T) {
	localDisks, ok := memberSchema(t).Properties["localDisks"]
	require.True(t, ok, "a member group must be able to declare a disk tier")
	require.NotNil(t, localDisks.Items, "the tier is a list of entries and not a single block")
	require.NotNil(t, localDisks.Items.Schema, "each entry carries its own schema")

	assert.Equal(t, "map", ptr.Deref(localDisks.XListType, ""),
		"a keyed list is what turns a second entry naming one directory into a schema refusal")
	assert.Equal(t, []string{"path"}, localDisks.XListMapKeys)
	assert.Equal(t, []string{"path"}, localDisks.Items.Schema.Required,
		"the path is required and the capacity is not: an unset capacity means the store's own "+
			"ceiling, while an unset path would mean a host directory this operator chose")
}
