package v1alpha1

import (
	"regexp"
	"strings"
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

// TestKVCacheBackendDeviceResourceNamePattern runs the schema's own pattern against the names real
// device plugins advertise, and against the shapes that would be accepted by a looser one.
//
// The pattern is hand-written and the API server is the only thing that would otherwise enforce it,
// which puts its first real exercise on a cluster. Compiling it here is faithful rather than
// approximate: CRD pattern validation is Go's own regexp engine, so this is the same matcher.
//
// The rejections carry the weight. An extended resource must be fully qualified -- a bare name is
// not one, and a plugin never advertises one -- so accepting "efa" would let an operator write
// something no node can satisfy and learn about it only when the member stays Pending.
func TestKVCacheBackendDeviceResourceNamePattern(t *testing.T) {
	crd := GetCustomResourceDefinitions()["KVCacheBackend"]
	require.NotNil(t, crd, "KVCacheBackend is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, level := range []string{"spec", "transport"} {
		next, ok := schema.Properties[level]
		require.True(t, ok, "the schema has no %q on the path to the transport", level)
		schema = &next
	}

	field, ok := schema.Properties["deviceResourceName"]
	require.True(t, ok, "the transport declares no device resource name")
	require.NotEmpty(t, field.Pattern, "without a pattern the only check is the API server's own")

	matcher, err := regexp.Compile(field.Pattern)
	require.NoError(t, err, "the pattern has to compile in the engine that enforces it")

	for _, accepted := range []string{
		// AWS's EFA plugin, which is also this field's fallback for the EFA protocol.
		"vpc.amazonaws.com/efa",
		// The RDMA shared-device plugin, whose suffix is whatever its own configuration names.
		"rdma/hca_shared_devices_a",
		// An SR-IOV deployment, where both halves are chosen by the administrator.
		"openshift.io/mlnxnics",
	} {
		assert.True(t, matcher.MatchString(accepted),
			"%q is a name a device plugin really advertises, and refusing it would leave that "+
				"cluster no way to declare its fabric", accepted)
	}

	// The longest name the API server accepts, and one character more. This axis was missing from
	// the first version of these cases: the pattern bounded the whole value and not the part after
	// the slash, so a name the CRD admitted became a resource list key the API server refused, and
	// the failure landed on the rendered DaemonSet rather than on the object that declared it.
	assert.True(t, matcher.MatchString("example.com/"+strings.Repeat("a", 63)),
		"63 characters is the limit a resource name's own part is held to, so it has to pass")

	for _, refused := range []string{
		"",                      // absent is expressed by omitting the field, not by emptying it
		"efa",                   // not qualified, and no plugin advertises a bare name
		"/efa",                  // no domain
		"vpc.amazonaws.com/",    // no name
		"VPC.amazonaws.com/efa", // a domain is lower case
		"vpc.amazonaws.com/e fa",
		"example.com/" + strings.Repeat("a", 64), // one past the name part's limit
		strings.Repeat("a", 64) + ".com/efa",     // one past a domain label's limit
	} {
		assert.False(t, matcher.MatchString(refused),
			"%q is not a resource any node advertises, so a member asking for it would stay "+
				"Pending with nothing saying why", refused)
	}
}
