package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// roleSchema returns the schema of one entry under the given top-level section, as the generated
// CRD carries it.
//
// Walked rather than reached for by a single path expression, so a rename anywhere along the way
// fails here naming the level that moved instead of a nil dereference.
func roleSchema(t *testing.T, section string) extension.JSONSchemaProps {
	t.Helper()

	crd := GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd, "ModelDeployment is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, level := range []string{section, "roles"} {
		next, ok := schema.Properties[level]
		require.True(t, ok, "the schema has no %q under the path to a %s role", level, section)
		schema = &next
	}

	require.NotNil(t, schema.Items, "roles is a list and its item schema is what carries the fields")
	require.NotNil(t, schema.Items.Schema)

	return *schema.Items.Schema
}

// enumValues returns a field's enum as plain strings, so two of them can be compared.
func enumValues(t *testing.T, schema extension.JSONSchemaProps, field string) []string {
	t.Helper()

	prop, ok := schema.Properties[field]
	require.True(t, ok, "the schema has no %q", field)

	values := make([]string, 0, len(prop.Enum))
	for _, entry := range prop.Enum {
		values = append(values, string(entry.Raw))
	}

	return values
}

// TestModelDeploymentStatusRoleKindIsConstrained pins that the status kind carries an enum at all.
//
// Without one the field is a plain string, and the controller's own resolution of the unset spec
// kind is the only thing keeping an empty value out of stored status. That is a convention rather
// than a constraint: it holds for as long as every writer remembers it.
func TestModelDeploymentStatusRoleKindIsConstrained(t *testing.T) {
	values := enumValues(t, roleSchema(t, "status"), "kind")

	assert.NotEmpty(t, values,
		"status.roles[].kind with no enum stores whatever a writer produces, including the empty "+
			"string the spec field's default exists to prevent")
}

// TestModelDeploymentRoleKindEnumsCannotDiverge is the guard that matters, and it is deliberately
// not a list of the three values.
//
// The status kind is written FROM the spec kind, so the failure this enum introduces is a value the
// writer can still produce that the enum refuses. The API server rejects that status write, and
// because the write carries every other figure on the object, one refused kind freezes readiness,
// replica counts and assigned flavors along with it.
//
// Comparing the two enums to each other rather than to a literal is what makes the guard survive a
// fourth kind: adding one to the spec field and forgetting the status field reddens here, while a
// hand-written list would have to be remembered in a third place to do the same work.
func TestModelDeploymentRoleKindEnumsCannotDiverge(t *testing.T) {
	spec := enumValues(t, roleSchema(t, "spec"), "kind")
	status := enumValues(t, roleSchema(t, "status"), "kind")

	require.NotEmpty(t, spec, "the spec field is the source of the values and must carry them")
	assert.ElementsMatch(t, spec, status,
		"the status kind echoes the spec kind, so a value the spec admits and the status enum "+
			"refuses is a status write the API server rejects for an object that was accepted")
}
