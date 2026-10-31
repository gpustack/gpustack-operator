package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// modelPrefetchModelTransferFixture is a source-preparing prefetch: the intent, and the transfer status
// one drain-and-refill window produces — two generations with their own figures, one serving,
// one draining — plus the reservation and the policy echo the namespace was admitted under.
func modelPrefetchModelTransferFixture() *ModelPrefetch {
	q := resource.MustParse("8Gi")

	return &ModelPrefetch{
		Spec: ModelPrefetchSpec{
			ArtifactRef: ModelPrefetchArtifactReference{Name: "model"},
			BindingRef:  ModelPrefetchBindingReference{Name: "cache"},
			Placement: &ModelPrefetchPlacement{
				NodeSelector: &meta.LabelSelector{MatchLabels: map[string]string{"pool": "h100"}},
			},
			Source: &ModelPrefetchSource{},
		},
		Status: ModelPrefetchStatus{
			DesiredNodes:     3,
			ReadyNodes:       3,
			DownloadingNodes: 0,
			Transfer: &ModelPrefetchTransferStatus{
				ReservedBytes: 5 << 30,
				Limits: &ModelPrefetchTransferLimits{
					Bytes:         q,
					Registrations: 4,
					Sessions:      8,
				},
				Generations: []ModelPrefetchTransferGeneration{
					{
						Generation:         "gen-1",
						ReadyNodes:         2,
						DrainingNodes:      1,
						ResidentBytes:      5 << 30,
						Registrations:      3,
						ActiveSessions:     2,
						RejectedAdmissions: 1,
					},
					{
						Generation:    "gen-2",
						ReadyNodes:    1,
						ResidentBytes: 5 << 30,
					},
				},
			},
		},
	}
}

// TestModelPrefetchSourceIntentOptIn pins the opt-in contract on the wire: an ordinary prefetch
// carries no source key at all and keeps meaning it always had, while a source intent survives
// the wire as its own object.
func TestModelPrefetchSourceIntentOptIn(t *testing.T) {
	plain := &ModelPrefetch{
		Spec: ModelPrefetchSpec{
			ArtifactRef: ModelPrefetchArtifactReference{Name: "model"},
			BindingRef:  ModelPrefetchBindingReference{Name: "cache"},
		},
	}

	raw, err := json.Marshal(plain)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"source"`,
		"absence of the intent is the field's absence, not an empty object a client must learn to read")
	assert.NotContains(t, string(raw), `"transfer"`,
		"an ordinary prefetch reports no transfer status")

	got := &ModelPrefetch{}
	require.NoError(t, json.Unmarshal(raw, got))
	assert.Nil(t, got.Spec.Source, "an object an older client wrote has no source intent")
	assert.Nil(t, got.Status.Transfer)

	preparing := modelPrefetchModelTransferFixture()
	raw, err = json.Marshal(preparing)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"source":{}`,
		"the intent is its own object, so a client can tell prepared from unprepared")

	got = &ModelPrefetch{}
	require.NoError(t, json.Unmarshal(raw, got))
	require.NotNil(t, got.Spec.Source)
	require.NotNil(t, got.Status.Transfer)
}

// TestModelPrefetchTransferStatusWireSurvivesJSONRoundTrip pins the distinctions the transfer
// status keeps on the JSON wire: per-generation figures that do not borrow each other's, a
// reservation charged whether or not the bytes are physically distinct, and a policy echo that
// is the grant as admitted.
func TestModelPrefetchTransferStatusWireSurvivesJSONRoundTrip(t *testing.T) {
	raw, err := json.Marshal(modelPrefetchModelTransferFixture())
	require.NoError(t, err)

	assert.Contains(t, string(raw), `"reservedBytes":5368709120`)
	assert.Contains(t, string(raw), `"bytes":"8Gi"`,
		"the policy echo is written in the quantity the grant is written in")

	got := &ModelPrefetch{}
	require.NoError(t, json.Unmarshal(raw, got))

	transfer := got.Status.Transfer
	require.NotNil(t, transfer)
	assert.Equal(t, int64(5<<30), transfer.ReservedBytes)
	require.NotNil(t, transfer.Limits)
	assert.Equal(t, "8Gi", transfer.Limits.Bytes.String())
	assert.Equal(t, int32(4), transfer.Limits.Registrations)
	assert.Equal(t, int32(8), transfer.Limits.Sessions)

	require.Len(t, transfer.Generations, 2)
	draining := transfer.Generations[0]
	assert.Equal(t, "gen-1", draining.Generation)
	assert.Equal(t, int32(2), draining.ReadyNodes)
	assert.Equal(t, int32(1), draining.DrainingNodes,
		"a window that mixes serving and draining reads as two figures, not one average")
	assert.Equal(t, int64(5<<30), draining.ResidentBytes)
	assert.Equal(t, int32(3), draining.Registrations)
	assert.Equal(t, int32(2), draining.ActiveSessions)
	assert.Equal(t, int64(1), draining.RejectedAdmissions)

	fresh := transfer.Generations[1]
	assert.Equal(t, "gen-2", fresh.Generation)
	assert.Equal(t, int32(0), fresh.ActiveSessions,
		"a generation's zero is its own, never the sibling's")
	assert.Equal(t, int64(0), fresh.RejectedAdmissions)
}

// TestModelPrefetchTransferStatusWireSurvivesProtoRoundTrip pins the same distinctions on the
// protobuf rendering, which clients reading generated types go through.
func TestModelPrefetchTransferStatusWireSurvivesProtoRoundTrip(t *testing.T) {
	raw, err := modelPrefetchModelTransferFixture().Marshal()
	require.NoError(t, err)

	got := &ModelPrefetch{}
	require.NoError(t, got.Unmarshal(raw))

	require.NotNil(t, got.Spec.Source, "the intent must survive the protobuf rendering")
	require.NotNil(t, got.Status.Transfer)
	assert.Equal(t, int64(5<<30), got.Status.Transfer.ReservedBytes)
	require.NotNil(t, got.Status.Transfer.Limits)
	assert.Equal(t, "8Gi", got.Status.Transfer.Limits.Bytes.String())
	require.Len(t, got.Status.Transfer.Generations, 2)
	assert.Equal(t, "gen-1", got.Status.Transfer.Generations[0].Generation)
	assert.Equal(t, int32(2), got.Status.Transfer.Generations[0].ReadyNodes)
	assert.Equal(t, int32(1), got.Status.Transfer.Generations[0].DrainingNodes)
	assert.Equal(t, int64(1), got.Status.Transfer.Generations[0].RejectedAdmissions)
	assert.Equal(t, "gen-2", got.Status.Transfer.Generations[1].Generation)
	assert.Equal(t, int32(0), got.Status.Transfer.Generations[1].ActiveSessions)
}

// TestModelPrefetchTransferStatusDeepCopyCutsReferenceFields pins that a copied prefetch shares
// no backing arrays with the original.
func TestModelPrefetchTransferStatusDeepCopyCutsReferenceFields(t *testing.T) {
	pf := modelPrefetchModelTransferFixture()

	got := pf.DeepCopy()
	got.Status.Transfer.Limits.Bytes = resource.MustParse("1Gi")
	got.Status.Transfer.Generations[0].Generation = "mutated"
	got.Status.Transfer.Generations = append(got.Status.Transfer.Generations,
		ModelPrefetchTransferGeneration{Generation: "added"})

	assert.Equal(t, "8Gi", pf.Status.Transfer.Limits.Bytes.String())
	assert.Equal(t, "gen-1", pf.Status.Transfer.Generations[0].Generation)
	assert.Len(t, pf.Status.Transfer.Generations, 2)
}

// TestModelPrefetchModelTransferSchema pins the generated schema to the contract: the intent and the
// transfer view are optional, a transfer entry is matched on its generation, and the policy echo
// carries the same three limits the grant does.
func TestModelPrefetchModelTransferSchema(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelPrefetch"]
	require.NotNil(t, crd, "ModelPrefetch is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	spec, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	require.True(t, ok, "the prefetch renders no spec schema")
	assert.NotContains(t, spec.Required, "source",
		"the intent is opt-in; requiring it would turn every existing prefetch into a source request")

	source, ok := spec.Properties["source"]
	require.True(t, ok, "the schema has no spec.source")
	assert.Equal(t, "object", source.Type)

	status, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	require.True(t, ok, "the prefetch renders no status schema")
	assert.NotContains(t, status.Required, "transfer")

	transfer, ok := status.Properties["transfer"]
	require.True(t, ok, "the schema has no status.transfer")

	generations, ok := transfer.Properties["generations"]
	require.True(t, ok, "the schema has no status.transfer.generations")
	require.NotNil(t, generations.XListType)
	assert.Equal(t, "map", *generations.XListType, "a generation entry is matched on its generation")
	assert.Equal(t, []string{"generation"}, generations.XListMapKeys)
	require.NotNil(t, generations.MaxItems, "the generation list is bounded")

	limits, ok := transfer.Properties["limits"]
	require.True(t, ok, "the schema has no status.transfer.limits")
	assert.Contains(t, limits.Required, "bytes")
	assert.Contains(t, limits.Required, "registrations")
	assert.Contains(t, limits.Required, "sessions")

	generation := generations.Items.Schema
	assert.NotContains(t, generation.Required, "readyNodes",
		"an observed zero and an unobserved figure must stay distinguishable")
}

func crdSchemaValidator(t *testing.T, kind string) apiservervalidation.SchemaValidator {
	t.Helper()

	crd := GetCustomResourceDefinitions()[kind]
	require.NotNil(t, crd, "%s is not registered", kind)
	require.Len(t, crd.Spec.Versions, 1)

	internal := &apiextensions.CustomResourceValidation{}
	require.NoError(t, apiextensionsv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(
		crd.Spec.Versions[0].Schema, internal, nil,
	))
	validator, _, err := apiservervalidation.NewSchemaValidator(internal.OpenAPIV3Schema)
	require.NoError(t, err)

	return validator
}

// TestModelPrefetchAndNodeModelStoreModelTransferSchemaValidation runs ModelPrefetch and NodeModelStore
// objects through the real Kubernetes OpenAPI validator over the generated schemas.
func TestModelPrefetchAndNodeModelStoreModelTransferSchemaValidation(t *testing.T) {
	pfValidator := crdSchemaValidator(t, "ModelPrefetch")
	pfRaw, err := json.Marshal(modelPrefetchModelTransferFixture())
	require.NoError(t, err)
	var pfObj map[string]any
	require.NoError(t, json.Unmarshal(pfRaw, &pfObj))
	assert.Empty(t, apiservervalidation.ValidateCustomResource(field.NewPath("object"), pfObj, pfValidator))

	badPf := jsonRoundTrip(t, pfObj)
	badPf["status"].(map[string]any)["transfer"].(map[string]any)["limits"].(map[string]any)["registrations"] = 0
	errs := apiservervalidation.ValidateCustomResource(field.NewPath("object"), badPf, pfValidator)
	assert.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "registrations")

	nmsValidator := crdSchemaValidator(t, "NodeModelStore")
	nmsObj := map[string]any{
		"apiVersion": "worker.gpustack.ai/v1alpha1",
		"kind":       "NodeModelStore",
		"metadata":   map[string]any{"name": "node-1"},
		"spec": map[string]any{
			"watermarks": map[string]any{"highPercent": int64(80), "lowPercent": int64(60)},
			"download":   map[string]any{"concurrency": int64(4)},
			"hub":        map[string]any{"huggingFaceEndpoint": "https://huggingface.co"},
		},
		"status": map[string]any{
			"modelTransfer": map[string]any{
				"generations": []any{
					map[string]any{
						"generation":         "gen-1",
						"digest":             "sha256:" + strings.Repeat("a", 64),
						"state":              "Serving",
						"residentBytes":      int64(1024),
						"registrations":      int64(2),
						"activeSessions":     int64(1),
						"rejectedAdmissions": int64(0),
					},
				},
			},
		},
	}
	assert.Empty(t, apiservervalidation.ValidateCustomResource(field.NewPath("object"), nmsObj, nmsValidator))

	badNms := jsonRoundTrip(t, nmsObj)
	badNms["status"].(map[string]any)["modelTransfer"].(map[string]any)["generations"].([]any)[0].(map[string]any)["state"] = "InvalidState"
	errs = apiservervalidation.ValidateCustomResource(field.NewPath("object"), badNms, nmsValidator)
	assert.NotEmpty(t, errs)
	assert.Contains(t, errs.ToAggregate().Error(), "state")
}
