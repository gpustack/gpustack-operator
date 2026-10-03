package v1alpha1

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
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

// TestModelDeploymentStatusEligibilityWireSurvivesJSONRoundTrip pins the distinctions the
// eligibility status contract keeps on the JSON wire: a declared degree from an undeclared one,
// an observed mode-false from an absent key, an ordinal zero from an omitted field, and the
// unobserved defaults a reader must see rather than have to guess. Every assertion reads a
// distinction a caller acts on, not a field inventory.
func TestModelDeploymentStatusEligibilityWireSurvivesJSONRoundTrip(t *testing.T) {
	now := meta.Now().Rfc3339Copy()
	status := &ModelDeploymentStatus{
		Roles: []ModelDeploymentRoleStatus{{
			Name: "prefill",
			Parallelism: ModelDeploymentRoleParallelismStatus{
				Declared: ModelDeploymentParallelismDeclaredStatus{
					TensorParallel:    ptr.To(int32(1)),
					DataParallelLocal: ptr.To(int32(0)),
				},
				Modes:       map[string]bool{"dp": false},
				LoadBalance: ModelDeploymentLoadBalanceUnknown,
				Source: ModelDeploymentParallelismSourceStatus{
					Kind:             ModelDeploymentParallelismSourceKindUnknown,
					UnreadableReason: "NotObserved",
				},
			},
			Endpoints: ModelDeploymentRoleEndpointsStatus{
				Serving: ModelDeploymentServingStatus{
					State: ModelDeploymentServingStateNotConfigured,
				},
			},
		}},
		Retirement: &ModelDeploymentRetirementStatus{
			RoleName:           "prefill",
			ReplicaOrdinal:     0,
			ObservedGeneration: 7,
			TargetMemberUIDs:   []string{"member-uid"},
			TargetWorkloadUID:  "workload-uid",
			State:              ModelDeploymentRetirementStateAborted,
			Reason:             "budget",
			StartedAt:          now,
			Deadline:           now,
			PhaseStartedAt:     now,
		},
	}

	raw, err := json.Marshal(status)
	require.NoError(t, err)

	assert.Contains(t, string(raw), `"tensorParallel":1`)
	assert.Contains(t, string(raw), `"dataParallelLocal":0`,
		"an explicit local zero is a declared degree and must be encoded, not omitted")
	assert.Contains(t, string(raw), `"dp":false`,
		"an observed mode carries its explicit boolean, not an omission")
	assert.Contains(t, string(raw), `"replicaOrdinal":0`,
		"ordinal zero is a real position and must be encoded, not omitted")

	got := &ModelDeploymentStatus{}
	require.NoError(t, json.Unmarshal(raw, got))

	role := got.Roles[0]
	require.NotNil(t, role.Parallelism.Declared.TensorParallel)
	assert.Equal(t, int32(1), *role.Parallelism.Declared.TensorParallel)
	require.NotNil(t, role.Parallelism.Declared.DataParallelLocal)
	assert.Equal(t, int32(0), *role.Parallelism.Declared.DataParallelLocal)
	assert.Nil(t, role.Parallelism.Declared.PipelineParallel,
		"undeclared stays nil, never zero")
	mode, present := role.Parallelism.Modes["dp"]
	require.True(t, present)
	assert.False(t, mode)
	assert.Len(t, role.Parallelism.Modes, 1, "absence of a key must not materialize as a false entry")
	assert.Equal(t, ModelDeploymentLoadBalanceUnknown, role.Parallelism.LoadBalance)
	assert.Equal(t, ModelDeploymentParallelismSourceKindUnknown, role.Parallelism.Source.Kind)
	assert.False(t, role.Parallelism.Source.Complete)
	assert.Equal(t, "NotObserved", role.Parallelism.Source.UnreadableReason)
	assert.Equal(t, ModelDeploymentServingStateNotConfigured, role.Endpoints.Serving.State)
	assert.Nil(t, role.Endpoints.Serving.Value,
		"an unobserved serving state carries no value to misread")

	require.NotNil(t, got.Retirement)
	assert.Equal(t, int32(0), got.Retirement.ReplicaOrdinal)
	assert.Equal(t, int64(7), got.Retirement.ObservedGeneration)
	assert.Equal(t, ModelDeploymentRetirementStateAborted, got.Retirement.State)
	assert.Equal(t, []string{"member-uid"}, got.Retirement.TargetMemberUIDs)
	assert.Equal(t, now, got.Retirement.StartedAt)
}

// TestModelDeploymentRoleEndpointsNilAndZeroStayDistinct pins the pair a consumer of a
// scale-down reads: nil eligible means the qualified set was never observed, an eligible zero
// means it was observed and is empty — and only a Confirmed serving state may carry a value.
func TestModelDeploymentRoleEndpointsNilAndZeroStayDistinct(t *testing.T) {
	unobserved := &ModelDeploymentRoleEndpointsStatus{
		Serving: ModelDeploymentServingStatus{State: ModelDeploymentServingStateUnknown},
	}
	drained := &ModelDeploymentRoleEndpointsStatus{
		Eligible: ptr.To(int32(0)),
		Serving: ModelDeploymentServingStatus{
			State: ModelDeploymentServingStateConfirmed,
			Value: ptr.To(int32(0)),
		},
	}

	rawUnknown, err := json.Marshal(unobserved)
	require.NoError(t, err)
	assert.NotContains(t, string(rawUnknown), `"eligible"`,
		"an unobserved count is nil on the wire, not a zero")
	assert.NotContains(t, string(rawUnknown), `"value"`,
		"an unconfirmed serving state never encodes a value")

	rawDrained, err := json.Marshal(drained)
	require.NoError(t, err)
	assert.Contains(t, string(rawDrained), `"eligible":0`,
		"a confirmed empty set is an encoded zero, never an omission")
	assert.Contains(t, string(rawDrained), `"value":0`)

	gotUnknown, gotDrained := &ModelDeploymentRoleEndpointsStatus{}, &ModelDeploymentRoleEndpointsStatus{}
	require.NoError(t, json.Unmarshal(rawUnknown, gotUnknown))
	require.NoError(t, json.Unmarshal(rawDrained, gotDrained))
	assert.Nil(t, gotUnknown.Eligible)
	assert.Equal(t, ModelDeploymentServingStateUnknown, gotUnknown.Serving.State)
	require.NotNil(t, gotDrained.Eligible)
	assert.Equal(t, int32(0), *gotDrained.Eligible)
	require.NotNil(t, gotDrained.Serving.Value)
	assert.Equal(t, int32(0), *gotDrained.Serving.Value)
	assert.Equal(t, ModelDeploymentServingStateConfirmed, gotDrained.Serving.State)
}

// TestModelDeploymentStatusEligibilityWireSurvivesProtoRoundTrip pins the same distinctions on
// the protobuf rendering, which clients reading generated types go through.
func TestModelDeploymentStatusEligibilityWireSurvivesProtoRoundTrip(t *testing.T) {
	now := meta.Now().Rfc3339Copy()
	deployment := &ModelDeployment{
		Status: ModelDeploymentStatus{
			Roles: []ModelDeploymentRoleStatus{{
				Name: "prefill",
				Parallelism: ModelDeploymentRoleParallelismStatus{
					Declared: ModelDeploymentParallelismDeclaredStatus{
						TensorParallel:    ptr.To(int32(1)),
						DataParallelLocal: ptr.To(int32(0)),
					},
					Modes:       map[string]bool{"dp": false},
					LoadBalance: ModelDeploymentLoadBalanceUnknown,
					Source: ModelDeploymentParallelismSourceStatus{
						Kind:             ModelDeploymentParallelismSourceKindUnknown,
						UnreadableReason: "NotObserved",
					},
				},
				Endpoints: ModelDeploymentRoleEndpointsStatus{
					Serving: ModelDeploymentServingStatus{
						State: ModelDeploymentServingStateNotConfigured,
					},
				},
			}},
			Retirement: &ModelDeploymentRetirementStatus{
				RoleName:           "prefill",
				ReplicaOrdinal:     0,
				ObservedGeneration: 7,
				TargetMemberUIDs:   []string{"member-uid"},
				TargetWorkloadUID:  "workload-uid",
				State:              ModelDeploymentRetirementStateAborted,
				StartedAt:          now,
				Deadline:           now,
				PhaseStartedAt:     now,
			},
		},
	}

	raw, err := deployment.Marshal()
	require.NoError(t, err)

	got := &ModelDeployment{}
	require.NoError(t, got.Unmarshal(raw))

	role := got.Status.Roles[0]
	require.NotNil(t, role.Parallelism.Declared.TensorParallel)
	assert.Equal(t, int32(1), *role.Parallelism.Declared.TensorParallel)
	require.NotNil(t, role.Parallelism.Declared.DataParallelLocal)
	assert.Equal(t, int32(0), *role.Parallelism.Declared.DataParallelLocal)
	assert.Nil(t, role.Parallelism.Declared.PipelineParallel)
	mode, present := role.Parallelism.Modes["dp"]
	require.True(t, present)
	assert.False(t, mode)
	assert.Equal(t, ModelDeploymentServingStateNotConfigured, role.Endpoints.Serving.State)
	assert.Nil(t, role.Endpoints.Serving.Value)

	require.NotNil(t, got.Status.Retirement)
	assert.Equal(t, int32(0), got.Status.Retirement.ReplicaOrdinal)
	assert.Equal(t, int64(7), got.Status.Retirement.ObservedGeneration)
	assert.Equal(t, ModelDeploymentRetirementStateAborted, got.Status.Retirement.State)
	assert.Equal(t, []string{"member-uid"}, got.Status.Retirement.TargetMemberUIDs)
	// The proto rendering preserves the retirement start instant, not the wall-clock
	// location the unmarshalled time carries, so equality is judged on instants.
	assert.Equal(t, now.UTC(), got.Status.Retirement.StartedAt.UTC())
}

// TestModelDeploymentStatusDeepCopyCutsReferenceFields pins that a copied status shares no
// backing arrays: the reservation a rebuild carries forward must not be a view a later writer
// can move underneath the stored object.
func TestModelDeploymentStatusDeepCopyCutsReferenceFields(t *testing.T) {
	status := &ModelDeploymentStatus{
		Roles: []ModelDeploymentRoleStatus{{
			Name: "prefill",
			Parallelism: ModelDeploymentRoleParallelismStatus{
				Declared: ModelDeploymentParallelismDeclaredStatus{
					TensorParallel: ptr.To(int32(1)),
				},
				Modes: map[string]bool{"dp": false},
			},
		}},
		Retirement: &ModelDeploymentRetirementStatus{
			RoleName:         "prefill",
			TargetMemberUIDs: []string{"member-uid"},
		},
	}

	got := status.DeepCopy()
	got.Roles[0].Parallelism.Modes["dp"] = true
	*got.Roles[0].Parallelism.Declared.TensorParallel = 9
	got.Retirement.TargetMemberUIDs[0] = "mutated"
	got.Retirement.ReplicaOrdinal = 3

	assert.False(t, status.Roles[0].Parallelism.Modes["dp"])
	assert.Equal(t, int32(1), *status.Roles[0].Parallelism.Declared.TensorParallel)
	assert.Equal(t, []string{"member-uid"}, status.Retirement.TargetMemberUIDs)
	assert.Equal(t, int32(0), status.Retirement.ReplicaOrdinal)
}

// TestModelDeploymentStatusEligibilitySchema pins the generated schema to the contract: the
// retirement object is optional because absent means no operation, the per-role objects exist,
// and every new enum is spelled exactly as the Go constants — so a widened writer set fails
// here instead of at a refused status write.
func TestModelDeploymentStatusEligibilitySchema(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd, "ModelDeployment is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	retirement, ok := schema.Properties["retirement"]
	require.True(t, ok, "the schema has no status.retirement")
	assert.NotContains(t, schema.Required, "retirement",
		"absent means no operation, so the object can never be required")

	roles := schema.Properties["roles"].Items.Schema
	parallelism, ok := roles.Properties["parallelism"]
	require.True(t, ok, "the schema has no status.roles[].parallelism")
	endpoints, ok := roles.Properties["endpoints"]
	require.True(t, ok, "the schema has no status.roles[].endpoints")

	serving, ok := endpoints.Properties["serving"]
	require.True(t, ok, "the schema has no status.roles[].endpoints.serving")
	assert.ElementsMatch(t,
		[]string{`"Confirmed"`, `"NotConverged"`, `"Unknown"`, `"NotConfigured"`},
		enumValues(t, serving, "state"))
	assert.ElementsMatch(t,
		[]string{`"Internal"`, `"External"`, `"Hybrid"`, `"MultiPort"`, `"Unknown"`},
		enumValues(t, parallelism, "loadBalance"))
	source, ok := parallelism.Properties["source"]
	require.True(t, ok, "the schema has no status.roles[].parallelism.source")
	assert.ElementsMatch(t,
		[]string{`"ExtraArgs"`, `"Command"`, `"UnmanagedCommand"`, `"Unknown"`},
		enumValues(t, source, "kind"))

	state, ok := retirement.Properties["state"]
	require.True(t, ok, "the schema has no status.retirement.state")
	states := make([]string, 0, len(state.Enum))
	for _, entry := range state.Enum {
		states = append(states, string(entry.Raw))
	}
	assert.ElementsMatch(t,
		[]string{
			`"Admitted"`, `"Disqualified"`, `"Withdrawing"`, `"Draining"`,
			`"Deleting"`, `"Settling"`, `"Aborted"`, `"Completed"`,
		},
		states)
}

// modelDeploymentSchemaValidator builds the real schema validator over the generated CRD: the
// generated schema converted the way the API server converts it, then the validator chain the
// API server runs against stored writes.
func modelDeploymentSchemaValidator(t *testing.T) apiservervalidation.SchemaValidator {
	t.Helper()

	crd := GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd, "ModelDeployment is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	internal := &apiextensions.JSONSchemaProps{}
	require.NoError(t, extension.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, internal, nil),
		"the generated schema must convert for validation")
	validator, _, err := apiservervalidation.NewSchemaValidator(internal)
	require.NoError(t, err, "the generated schema must build a validator")
	return validator
}

// modelDeploymentWriteFixture returns a full object as the new producer writes it: both
// per-role views and a retirement reservation, every required field spelled out.
func modelDeploymentWriteFixture(t *testing.T) map[string]any {
	t.Helper()

	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{
		"apiVersion": "worker.gpustack.ai/v1alpha1",
		"kind": "ModelDeployment",
		"metadata": {"name": "demo", "namespace": "default"},
		"spec": {
			"model": {"name": "qwen-demo", "artifactRef": {"name": "weights"}},
			"engine": {"name": "vLLM"},
			"roles": [{"name": "s0", "instanceType": "gpu-small", "kind": "Server", "size": 1}]
		},
		"status": {
			"roleSummary": "S1",
			"roles": [{
				"name": "s0",
				"desired": 1,
				"ready": 1,
				"quotaReserved": 1,
				"unmanaged": false,
				"kind": "Server",
				"assignedFlavors": ["gpu-a"],
				"parallelism": {
					"declared": {"tensorParallel": 2, "pipelineParallel": 1},
					"modes": {"tensor": true},
					"loadBalance": "Internal",
					"source": {"kind": "ExtraArgs", "complete": true}
				},
				"endpoints": {
					"eligible": 1,
					"serving": {"state": "Confirmed", "value": 1}
				}
			}],
			"retirement": {
				"roleName": "s0",
				"replicaOrdinal": 0,
				"observedGeneration": 1,
				"targetWorkloadUID": "workload-uid",
				"targetMemberUIDs": ["member-uid"],
				"state": "Admitted",
				"startedAt": "2026-10-01T00:00:00Z",
				"deadline": "2026-10-01T01:00:00Z",
				"phaseStartedAt": "2026-10-01T00:00:00Z"
			}
		}
	}`), &obj), "the fixture must be valid JSON")
	return obj
}

// jsonRoundTrip deep-copies a fixture through JSON, so table cases mutate their own copy.
func jsonRoundTrip(t *testing.T, in map[string]any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(in)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// firstStatusRole returns the single status role entry of a fixture copy.
func firstStatusRole(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()

	status, ok := obj["status"].(map[string]any)
	require.True(t, ok, "the fixture has no status object")
	roles, ok := status["roles"].([]any)
	require.True(t, ok, "the fixture has no status.roles list")
	require.NotEmpty(t, roles, "the fixture has no status role entry")
	role, ok := roles[0].(map[string]any)
	require.True(t, ok, "the status role entry is not an object")
	return role
}

// roleParallelism returns the role's parallelism view, for cases that mutate it.
func roleParallelism(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()

	view, ok := firstStatusRole(t, obj)["parallelism"].(map[string]any)
	require.True(t, ok, "the role has no parallelism object")
	return view
}

// roleServing returns the role's endpoints.serving view, for cases that mutate it.
func roleServing(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()

	endpoints, ok := firstStatusRole(t, obj)["endpoints"].(map[string]any)
	require.True(t, ok, "the role has no endpoints object")
	serving, ok := endpoints["serving"].(map[string]any)
	require.True(t, ok, "the role has no endpoints.serving object")
	return serving
}

// storedRoleWithoutNewObjects rewrites the fixture to its legacy shape, deleting the
// parallelism and endpoints views and the retirement reservation, so the object is what an
// operator wrote before this change existed.
func storedRoleWithoutNewObjects(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()

	stored := jsonRoundTrip(t, obj)
	delete(firstStatusRole(t, stored), "parallelism")
	delete(firstStatusRole(t, stored), "endpoints")
	status, ok := stored["status"].(map[string]any)
	require.True(t, ok, "the fixture has no status object")
	delete(status, "retirement")
	return stored
}

// TestModelDeploymentRoleStatusSchemaValidation runs objects through the real Kubernetes
// validator over the generated schema, because the worker/v1 aggregated handler proxies onto
// this v1alpha1 storage: a role stored before the parallelism and endpoints views existed has
// to survive every write that proxy lets through, while a view that IS present still
// validates for real.
func TestModelDeploymentRoleStatusSchemaValidation(t *testing.T) {
	validator := modelDeploymentSchemaValidator(t)
	complete := modelDeploymentWriteFixture(t)

	cases := []struct {
		name      string
		obj       func(t *testing.T) map[string]any
		wantValid bool
		wantField string
	}{
		{
			name:      "the complete object the producer writes validates",
			obj:       func(t *testing.T) map[string]any { return jsonRoundTrip(t, complete) },
			wantValid: true,
		},
		{
			name:      "a role stored before the views existed stays writable",
			obj:       func(t *testing.T) map[string]any { return storedRoleWithoutNewObjects(t, complete) },
			wantValid: true,
		},
		{
			name: "a present serving view still validates its state",
			obj: func(t *testing.T) map[string]any {
				obj := jsonRoundTrip(t, complete)
				roleServing(t, obj)["state"] = "AlmostConfirmed"
				return obj
			},
			wantValid: false,
			wantField: "endpoints.serving.state",
		},
		{
			name: "a present parallelism view still validates its balance",
			obj: func(t *testing.T) map[string]any {
				obj := jsonRoundTrip(t, complete)
				roleParallelism(t, obj)["loadBalance"] = "RoundRobin"
				return obj
			},
			wantValid: false,
			wantField: "parallelism.loadBalance",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := apiservervalidation.ValidateCustomResource(field.NewPath("object"), c.obj(t), validator)
			if c.wantValid {
				assert.Empty(t, errs, "the object must validate, and did not: %v", errs.ToAggregate())
				return
			}
			assert.NotEmpty(t, errs, "the object must be rejected, and was not")
			assert.Contains(t, errs.ToAggregate().Error(), c.wantField,
				"the rejection must name the field that broke the schema")
		})
	}
}
