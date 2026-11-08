package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestModelStoreBindingSourceMemoryDefaultsToNone pins the default that matters: a Binding an
// admin wrote before source preparation existed carries no memory grant at all, and no schema
// default or empty object invents one for it. quota.bytes and allowPinned sit beside it as the
// separate grants they are.
func TestModelStoreBindingSourceMemoryDefaultsToNone(t *testing.T) {
	binding := newModelStoreBindingFixture()

	raw, err := json.Marshal(binding)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"sourceMemory"`,
		"no grant is the field's absence, not an empty grant or a defaulted one")
	assert.NotContains(t, string(raw), `"reservedMemory"`)

	got := &ModelStoreBinding{}
	require.NoError(t, json.Unmarshal(raw, got))
	assert.Nil(t, got.Spec.SourceMemory,
		"absent follows node capacity without materializing an explicit grant")
	assert.Nil(t, got.Status.ReservedMemory)
}

// TestModelStoreBindingSourceMemoryWire pins the grant and its reservation on the wire, both
// JSON and protobuf, with the growth the webhook admits and the figures the accounting reports.
func TestModelStoreBindingSourceMemoryWire(t *testing.T) {
	binding := newModelStoreBindingFixture()
	q := resource.MustParse("16Gi")
	registrations, sessions := int32(4), int32(8)
	binding.Spec.SourceMemory = &ModelStoreBindingSourceMemory{
		Bytes:         q,
		Registrations: registrations,
		Sessions:      sessions,
	}
	binding.Status.ReservedMemory = &q

	t.Run("json", func(t *testing.T) {
		raw, err := json.Marshal(binding)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"bytes":"16Gi"`)
		assert.Contains(t, string(raw), `"registrations":4`)
		assert.Contains(t, string(raw), `"sessions":8`)
		assert.Contains(t, string(raw), `"reservedMemory":"16Gi"`)

		got := &ModelStoreBinding{}
		require.NoError(t, json.Unmarshal(raw, got))
		require.NotNil(t, got.Spec.SourceMemory)
		assert.Equal(t, "16Gi", got.Spec.SourceMemory.Bytes.String())
		assert.Equal(t, registrations, got.Spec.SourceMemory.Registrations)
		assert.Equal(t, sessions, got.Spec.SourceMemory.Sessions)
		require.NotNil(t, got.Status.ReservedMemory)
		assert.Equal(t, "16Gi", got.Status.ReservedMemory.String())
	})

	t.Run("protobuf", func(t *testing.T) {
		raw, err := binding.Marshal()
		require.NoError(t, err)

		got := &ModelStoreBinding{}
		require.NoError(t, got.Unmarshal(raw))
		require.NotNil(t, got.Spec.SourceMemory)
		assert.Equal(t, "16Gi", got.Spec.SourceMemory.Bytes.String())
		assert.Equal(t, registrations, got.Spec.SourceMemory.Registrations)
		assert.Equal(t, sessions, got.Spec.SourceMemory.Sessions)
		require.NotNil(t, got.Status.ReservedMemory)
		assert.Equal(t, "16Gi", got.Status.ReservedMemory.String())
	})
}

// TestModelStoreBindingSourceMemorySchema pins the generated schema to the contract: the grant is
// optional and carries the same three administrator-set limits the source enforces, each bounded
// below one, and the reservation is an optional status figure.
func TestModelStoreBindingSourceMemorySchema(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelStoreBinding"]
	require.NotNil(t, crd, "ModelStoreBinding is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema

	spec, ok := schema.Properties["spec"]
	require.True(t, ok, "the binding renders no spec schema")
	assert.NotContains(t, spec.Required, "sourceMemory",
		"the grant is opt-in; requiring it would turn every existing binding into a memory grant")

	grant, ok := spec.Properties["sourceMemory"]
	require.True(t, ok, "the schema has no spec.sourceMemory")
	assert.Contains(t, grant.Required, "bytes")
	assert.Contains(t, grant.Required, "registrations")
	assert.Contains(t, grant.Required, "sessions")
	registrations, ok := grant.Properties["registrations"]
	require.True(t, ok, "the grant renders no registrations schema")
	require.NotNil(t, registrations.Minimum, "the registration count is bounded below one")
	assert.InDelta(t, 1, *registrations.Minimum, 0)
	sessions, ok := grant.Properties["sessions"]
	require.True(t, ok, "the grant renders no sessions schema")
	require.NotNil(t, sessions.Minimum, "the session count is bounded below one")
	assert.InDelta(t, 1, *sessions.Minimum, 0)

	status, ok := schema.Properties["status"]
	require.True(t, ok, "the binding renders no status schema")
	assert.NotContains(t, status.Required, "reservedMemory")
	reserved, ok := status.Properties["reservedMemory"]
	require.True(t, ok, "the schema has no status.reservedMemory")
	assert.True(t, reserved.XIntOrString, "a reservation is a quantity, like UsedBytes")
}

// newModelStoreBindingFixture builds a Binding that passes every shape rule, with no grant yet.
func newModelStoreBindingFixture() *ModelStoreBinding {
	return &ModelStoreBinding{
		Spec: ModelStoreBindingSpec{
			StoreRefs: []ModelStoreBindingStoreReference{{Name: "h100"}},
			Quota:     ModelStoreBindingQuota{Bytes: resource.MustParse("10Gi")},
		},
	}
}
