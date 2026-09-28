package v1alpha1

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestModelDeploymentKVCacheIsOptional(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd, "ModelDeployment is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	assert.NotContains(t, spec.Required, "kvCache")
	assert.Equal(t, "object", spec.Properties["kvCache"].Type)
}

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

func TestModelDeploymentEnumSpellings(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd)
	require.Len(t, crd.Spec.Versions, 1)

	cases := []struct {
		name         string
		path         []string
		want         []string
		old          []string
		defaultValue string
	}{
		{"spec role kind", []string{"spec", "roles", "[]", "kind"}, []string{`"Server"`, `"Prefill"`, `"Decode"`}, []string{`"server"`, `"prefill"`, `"decode"`}, `"Server"`},
		{"status role kind", []string{"status", "roles", "[]", "kind"}, []string{`"Server"`, `"Prefill"`, `"Decode"`}, []string{`"server"`, `"prefill"`, `"decode"`}, ""},
		{"engine name", []string{"spec", "engine", "name"}, []string{`"vLLM"`, `"SGLang"`}, []string{`"vllm"`, `"sglang"`}, ""},
		{"cache connector", []string{"spec", "kvCache", "connector"}, []string{`"Mooncake"`}, []string{`"mooncake"`}, `"Mooncake"`},
		{"direct protocol", []string{"spec", "kvTransfer", "protocol"}, []string{`"Auto"`, `"TCP"`, `"RDMA"`, `"EFA"`, `"CANN"`, `"ROCM"`}, []string{`"auto"`, `"tcp"`, `"rdma"`, `"efa"`, `"cann"`, `"rocm"`, `"MUSA"`, `"MACA"`}, ""},
		{"model delivery", []string{"status", "model", "delivery"}, []string{`"PVC"`, `"Engine"`, `"Node"`, `"Image"`}, []string{`"Pvc"`}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			for _, part := range tc.path {
				if part == "[]" {
					require.NotNil(t, schema.Items)
					require.NotNil(t, schema.Items.Schema)
					schema = schema.Items.Schema
					continue
				}
				child, ok := schema.Properties[part]
				require.True(t, ok, "missing %s", part)
				schema = &child
			}
			values := make([]string, 0, len(schema.Enum))
			for _, value := range schema.Enum {
				values = append(values, string(value.Raw))
			}
			assert.ElementsMatch(t, tc.want, values)
			for _, old := range tc.old {
				assert.NotContains(t, values, old)
			}
			if tc.defaultValue != "" {
				require.NotNil(t, schema.Default)
				assert.Equal(t, tc.defaultValue, string(schema.Default.Raw))
			}
		})
	}
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

// TestModelDeploymentStatusDeliveryAdmitsEveryDelivery pins the status model delivery enum to the
// delivery constants.
//
// The role kind guard above can diff two annotations, because kind is declared on both spec and
// status. Delivery is declared on the status side only — the controller computes it from the
// artifact's source — so there is no second enum to diff against, and the constants stand in for
// it. The comparison still forces the checkpoint that matters: a fifth delivery constant turns this
// test red until the list here is updated, and updating the list is where an author notices whether
// the enum annotation kept up. Without that checkpoint the failure is a status write the API server
// rejects for an object it accepted, which is how the Image value was lost.
func TestModelDeploymentStatusDeliveryAdmitsEveryDelivery(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd, "ModelDeployment is not registered")

	status, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	require.True(t, ok, "the schema has no status")
	model, ok := status.Properties["model"]
	require.True(t, ok, "the schema has no status.model")

	deliveries := make([]string, 0, 4)
	for _, d := range []ModelDeploymentModelDelivery{
		ModelDeploymentModelDeliveryPVC,
		ModelDeploymentModelDeliveryEngine,
		ModelDeploymentModelDeliveryNode,
		ModelDeploymentModelDeliveryImage,
	} {
		deliveries = append(deliveries, fmt.Sprintf("%q", d))
	}

	assert.ElementsMatch(t, deliveries, enumValues(t, model, "delivery"),
		"every delivery the controller can write must be a value the status enum admits")
}
