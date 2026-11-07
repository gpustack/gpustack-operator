package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// modelDeploymentRemoteStatusFixture is one remote-delivering deployment as the producer writes
// it: the configured outcome, and the per-replica entries that are the observed truth. The
// prefill role is mid-fallback — its first replica loads from the source while its second has
// visibly taken Node — and the decode role is uniformly remote, so the mixed and the uniform
// shapes are both present in one object.
func modelDeploymentRemoteStatusFixture() *ModelDeploymentStatus {
	return &ModelDeploymentStatus{
		Model: &ModelDeploymentModelStatus{
			Artifact:       "weights",
			ManifestDigest: "sha256:" + strings.Repeat("7", 64),
			Delivery:       ModelDeploymentModelDeliveryRemote,
			Remote: &ModelDeploymentModelRemoteStatus{
				Policy: ModelDeploymentRemotePolicyPrefer,
				Roles: []ModelDeploymentRemoteRoleStatus{
					{
						Role: "prefill",
						Replicas: []ModelDeploymentRemoteReplicaStatus{
							{
								Ordinal:  0,
								Delivery: ModelDeploymentModelDeliveryRemote,
								Load:     ModelDeploymentRemoteLoadStateLoaded,
							},
							{
								Ordinal:          1,
								Delivery:         ModelDeploymentModelDeliveryNode,
								Load:             ModelDeploymentRemoteLoadStateUnknown,
								Reason:           "no authorized source held the content within the declared startup limit",
								FallbackAttempts: 1,
							},
						},
					},
					{
						Role: "decode",
						Replicas: []ModelDeploymentRemoteReplicaStatus{
							{
								Ordinal:  0,
								Delivery: ModelDeploymentModelDeliveryRemote,
								Load:     ModelDeploymentRemoteLoadStateLoading,
							},
						},
					},
				},
			},
		},
	}
}

// TestModelDeploymentRemoteStatusWireSurvivesJSONRoundTrip pins the distinctions the remote
// view keeps on the JSON wire: a configured outcome from a replica's observed one, an ordinal
// zero from an omitted field, a fallback replica that is never relabeled as the configured
// outcome, and the per-replica entries that keep a mixed deployment from reading as uniform.
func TestModelDeploymentRemoteStatusWireSurvivesJSONRoundTrip(t *testing.T) {
	raw, err := json.Marshal(modelDeploymentRemoteStatusFixture())
	require.NoError(t, err)

	assert.Contains(t, string(raw), `"delivery":"Remote"`,
		"the configured outcome is the aggregate delivery")
	assert.Contains(t, string(raw), `"ordinal":0`,
		"ordinal zero is a real position and must be encoded, not omitted")
	assert.Contains(t, string(raw), `"fallbackAttempts":1`,
		"a consumed fallback attempt is the counted evidence of a bounded attempt")

	got := &ModelDeploymentStatus{}
	require.NoError(t, json.Unmarshal(raw, got))

	model := got.Model
	require.NotNil(t, model)
	require.NotNil(t, model.Remote)
	assert.Equal(t, ModelDeploymentModelDeliveryRemote, model.Delivery)
	assert.Equal(t, ModelDeploymentRemotePolicyPrefer, model.Remote.Policy)

	require.Len(t, model.Remote.Roles, 2)
	roleBy := func(name string) ModelDeploymentRemoteRoleStatus {
		for _, role := range model.Remote.Roles {
			if role.Role == name {
				return role
			}
		}
		return ModelDeploymentRemoteRoleStatus{}
	}

	prefill := roleBy("prefill")
	require.Len(t, prefill.Replicas, 2)
	assert.Equal(t, ModelDeploymentModelDeliveryRemote, prefill.Replicas[0].Delivery)
	assert.Equal(t, ModelDeploymentRemoteLoadStateLoaded, prefill.Replicas[0].Load)
	assert.Empty(t, prefill.Replicas[0].Reason)
	assert.Equal(t, int32(0), prefill.Replicas[0].FallbackAttempts)
	assert.Equal(t, ModelDeploymentModelDeliveryNode, prefill.Replicas[1].Delivery,
		"a fallback replica reads its own outcome, never the configured one")
	assert.Equal(t, ModelDeploymentRemoteLoadStateUnknown, prefill.Replicas[1].Load,
		"a replica that fell back before loading carries no load observation, not a borrowed one")
	assert.NotEmpty(t, prefill.Replicas[1].Reason)
	assert.Equal(t, int32(1), prefill.Replicas[1].FallbackAttempts)

	decode := roleBy("decode")
	require.Len(t, decode.Replicas, 1)
	assert.Equal(t, ModelDeploymentModelDeliveryRemote, decode.Replicas[0].Delivery)
	assert.Equal(t, ModelDeploymentRemoteLoadStateLoading, decode.Replicas[0].Load)

	assert.NotEqual(t, prefill.Replicas[0].Delivery, prefill.Replicas[1].Delivery,
		"the two replicas of one role disagree, and the entries are what says so")
}

// TestModelDeploymentRemoteStatusWireSurvivesProtoRoundTrip pins the same distinctions on the
// protobuf rendering, which clients reading generated types go through.
func TestModelDeploymentRemoteStatusWireSurvivesProtoRoundTrip(t *testing.T) {
	deployment := &ModelDeployment{Status: *modelDeploymentRemoteStatusFixture()}

	raw, err := deployment.Marshal()
	require.NoError(t, err)

	got := &ModelDeployment{}
	require.NoError(t, got.Unmarshal(raw))

	require.NotNil(t, got.Status.Model)
	require.NotNil(t, got.Status.Model.Remote)
	assert.Equal(t, ModelDeploymentModelDeliveryRemote, got.Status.Model.Delivery)
	assert.Equal(t, ModelDeploymentRemotePolicyPrefer, got.Status.Model.Remote.Policy)

	require.Len(t, got.Status.Model.Remote.Roles, 2)
	prefill := got.Status.Model.Remote.Roles[0]
	assert.Equal(t, "prefill", prefill.Role)
	require.Len(t, prefill.Replicas, 2)
	assert.Equal(t, int32(0), prefill.Replicas[0].Ordinal)
	assert.Equal(t, ModelDeploymentModelDeliveryRemote, prefill.Replicas[0].Delivery)
	assert.Equal(t, ModelDeploymentRemoteLoadStateLoaded, prefill.Replicas[0].Load)
	assert.Equal(t, int32(1), prefill.Replicas[1].Ordinal)
	assert.Equal(t, ModelDeploymentModelDeliveryNode, prefill.Replicas[1].Delivery)
	assert.Equal(t, "no authorized source held the content within the declared startup limit",
		prefill.Replicas[1].Reason)
	assert.Equal(t, int32(1), prefill.Replicas[1].FallbackAttempts)

	decode := got.Status.Model.Remote.Roles[1]
	require.Len(t, decode.Replicas, 1)
	assert.Equal(t, ModelDeploymentRemoteLoadStateLoading, decode.Replicas[0].Load)
}

// TestModelDeploymentRemoteStatusLegacyStaysAbsent pins the absence-of-opt-in contract: a
// deployment delivered the legacy way carries no remote view at all, so a consumer that has
// never heard of remote delivery sees no new key and an absent view never reads as a changed
// answer.
func TestModelDeploymentRemoteStatusLegacyStaysAbsent(t *testing.T) {
	legacy := &ModelDeploymentStatus{
		Model: &ModelDeploymentModelStatus{
			Artifact: "weights",
			Delivery: ModelDeploymentModelDeliveryNode,
		},
	}

	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"remote"`,
		"absence of remote delivery is the field's absence, not an empty object")

	got := &ModelDeploymentStatus{}
	require.NoError(t, json.Unmarshal(raw, got))
	require.NotNil(t, got.Model)
	assert.Nil(t, got.Model.Remote)
	assert.Equal(t, ModelDeploymentModelDeliveryNode, got.Model.Delivery)
}

// TestModelDeploymentRemoteStatusDeepCopyCutsReferenceFields pins that a copied status shares no
// backing arrays: the role and replica entries a later writer rebuilds must not be views that
// move underneath the stored object.
func TestModelDeploymentRemoteStatusDeepCopyCutsReferenceFields(t *testing.T) {
	status := modelDeploymentRemoteStatusFixture()

	got := status.DeepCopy()
	got.Model.Remote.Roles[0].Role = "mutated"
	got.Model.Remote.Roles[0].Replicas[0].Delivery = ModelDeploymentModelDeliveryEngine
	got.Model.Remote.Roles = append(got.Model.Remote.Roles, ModelDeploymentRemoteRoleStatus{Role: "added"})

	assert.Equal(t, "prefill", status.Model.Remote.Roles[0].Role)
	assert.Equal(t, ModelDeploymentModelDeliveryRemote, status.Model.Remote.Roles[0].Replicas[0].Delivery)
	assert.Len(t, status.Model.Remote.Roles, 2)
}

// TestModelDeploymentRemoteStatusSchema pins the generated schema to the contract: the remote
// view is optional, the policy echoes the two delivery policies, a replica's delivery is
// narrowed to the outcomes a remote deployment can actually be on, and the load stages stay
// distinct from serving readiness.
func TestModelDeploymentRemoteStatusSchema(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd, "ModelDeployment is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	model, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["model"]
	require.True(t, ok, "the schema has no status.model")

	remote, ok := model.Properties["remote"]
	require.True(t, ok, "the schema has no status.model.remote")
	assert.Equal(t, "object", remote.Type)

	assert.ElementsMatch(t,
		[]string{`"Require"`, `"Prefer"`},
		enumValues(t, remote, "policy"))

	remoteRoles, ok := remote.Properties["roles"]
	require.True(t, ok, "the schema has no status.model.remote.roles")
	require.NotNil(t, remoteRoles.XListType)
	assert.Equal(t, "map", *remoteRoles.XListType, "a role entry is matched on its role name")
	assert.Equal(t, []string{"role"}, remoteRoles.XListMapKeys)

	roleSchema := remoteRoles.Items.Schema
	require.NotNil(t, roleSchema)
	replicas, ok := roleSchema.Properties["replicas"]
	require.True(t, ok, "the schema has no status.model.remote.roles[].replicas")
	require.NotNil(t, replicas.XListType)
	assert.Equal(t, "atomic", *replicas.XListType,
		"a replica entry is rebuilt every pass, so the list is an ordinary one")

	require.NotNil(t, replicas.Items)
	require.NotNil(t, replicas.Items.Schema)
	replica := *replicas.Items.Schema
	assert.ElementsMatch(t,
		[]string{`"Remote"`, `"Node"`, `"Engine"`},
		enumValues(t, replica, "delivery"),
		"a replica on a remote deployment is on the source or on its identity-preserving fallback, "+
			"never on a claim or an image delivery that is a different artifact")
	assert.ElementsMatch(t,
		[]string{`"Unknown"`, `"AwaitingMetadata"`, `"AwaitingSource"`, `"Loading"`, `"Loaded"`},
		enumValues(t, replica, "load"),
		"the load stages are the distinct observations the startup path makes, and serving ready "+
			"stays out of them")
}

// modelDeploymentRemoteWriteFixture returns a full stored object whose model status carries the
// remote view, every required field spelled out.
func modelDeploymentRemoteWriteFixture(t *testing.T) map[string]any {
	t.Helper()

	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{
		"apiVersion": "worker.gpustack.ai/v1alpha1",
		"kind": "ModelDeployment",
		"metadata": {"name": "demo", "namespace": "default"},
		"spec": {
			"model": {"name": "qwen-demo", "artifactRef": {"name": "weights"}},
			"engine": {"name": "vLLM", "image": "registry/vllm:latest"},
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
				"kind": "Server"
			}],
			"model": {
				"artifact": "weights",
				"delivery": "Remote",
				"remote": {
					"policy": "Prefer",
					"roles": [{
						"role": "s0",
						"replicas": [
							{"ordinal": 0, "delivery": "Remote", "load": "Loaded"},
							{"ordinal": 1, "delivery": "Node", "load": "Unknown",
							 "reason": "no authorized source held the content", "fallbackAttempts": 1}
						]
					}]
				}
			}
		}
	}`), &obj), "the fixture must be valid JSON")

	return obj
}

// TestModelDeploymentRemoteStatusSchemaValidation runs objects through the real Kubernetes
// validator over the generated schema: a deployment delivered the legacy way stays writable with
// no remote view at all, the view the producer writes validates, and a writer that names an
// outcome or a stage the schema does not carry is rejected instead of stored.
func TestModelDeploymentRemoteStatusSchemaValidation(t *testing.T) {
	validator := modelDeploymentSchemaValidator(t)

	complete := modelDeploymentRemoteWriteFixture(t)
	legacy := modelDeploymentRemoteWriteFixture(t)
	delete(legacy["status"].(map[string]any), "model")

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
			name:      "a deployment delivered the legacy way stays writable",
			obj:       func(t *testing.T) map[string]any { return jsonRoundTrip(t, legacy) },
			wantValid: true,
		},
		{
			name: "a policy the schema does not carry is rejected",
			obj: func(t *testing.T) map[string]any {
				obj := jsonRoundTrip(t, complete)
				model(obj)["remote"].(map[string]any)["policy"] = "Fallback"
				return obj
			},
			wantValid: false,
			wantField: "remote.policy",
		},
		{
			name: "a replica delivery outside the remote outcomes is rejected",
			obj: func(t *testing.T) map[string]any {
				obj := jsonRoundTrip(t, complete)
				replica(obj, 1)["delivery"] = "PVC"
				return obj
			},
			wantValid: false,
			wantField: "delivery",
		},
		{
			name: "a load stage outside the observations is rejected",
			obj: func(t *testing.T) map[string]any {
				obj := jsonRoundTrip(t, complete)
				replica(obj, 0)["load"] = "ServingReady"
				return obj
			},
			wantValid: false,
			wantField: "load",
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

// model returns the fixture's status.model object.
func model(obj map[string]any) map[string]any {
	return obj["status"].(map[string]any)["model"].(map[string]any)
}

// replicas returns the fixture's first role entry's replicas list.
func replicas(obj map[string]any) []any {
	return model(obj)["remote"].(map[string]any)["roles"].([]any)[0].(map[string]any)["replicas"].([]any)
}

// replica returns the fixture's replica entry at the given position.
func replica(obj map[string]any, i int) map[string]any {
	return replicas(obj)[i].(map[string]any)
}
