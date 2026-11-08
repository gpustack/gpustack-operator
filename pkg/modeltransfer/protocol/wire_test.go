package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManifestWireRoundTrip checks that the wire types survive a
// marshal/unmarshal cycle without losing any field the broker will send.
func TestManifestWireRoundTrip(t *testing.T) {
	manifest := loadFixtureManifest(t)
	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)

	var decoded Manifest
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, manifest.ArtifactUID, decoded.ArtifactUID)
	assert.Equal(t, manifest.ContractVersion, decoded.ContractVersion)
	assert.EqualValues(t, manifest.SourceGeneration, decoded.SourceGeneration)
	require.Len(t, decoded.Tensors, len(manifest.Tensors))
	for index, tensor := range manifest.Tensors {
		other := decoded.Tensors[index]
		assert.Equal(t, tensor.Name, other.Name)
		assert.Equal(t, tensor.Dtype, other.Dtype)
		assert.Equal(t, tensor.Shape, other.Shape)
		assert.EqualValues(t, tensor.DataBegin, other.DataBegin)
		assert.EqualValues(t, tensor.DataEnd, other.DataEnd)
	}
	require.Len(t, decoded.Files, len(manifest.Files))
	assert.EqualValues(t, manifest.Files[0].Size, decoded.Files[0].Size)

	require.NoError(t, decoded.Check())
}

// TestManifestDecodesPythonCanonicalBytes checks that Go reads the exact
// bytes the Python generator wrote, not merely its own serialization.
func TestManifestDecodesPythonCanonicalBytes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(t, err)
	var manifest Manifest
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.NoError(t, manifest.Check())
	assert.Equal(t, "model-fixture-tiny-dense", manifest.ArtifactUID)
}

func TestCapabilitiesWireRoundTrip(t *testing.T) {
	caps := validCapabilities()
	encoded, err := json.Marshal(caps)
	require.NoError(t, err)

	var decoded BackendCapabilities
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.NoError(t, decoded.Validate())
	assert.Equal(t, caps.Requested, decoded.Requested)
	assert.Equal(t, caps.Applied, decoded.Applied)
	assert.Equal(t, caps.Effective, decoded.Effective)
	assert.True(t, decoded.EffectiveObserved)
	require.Len(t, decoded.Rejected, 1)
	assert.Equal(t, "ucx", decoded.Rejected[0].Option)
	assert.Equal(t, "no verbs devices", decoded.Rejected[0].Reason)
	assert.Equal(t, caps.Transport.Control, decoded.Transport.Control)
	assert.Equal(t, caps.Transport.Payload, decoded.Transport.Payload)
}

func TestIdentityWireRoundTrip(t *testing.T) {
	identity := ArtifactIdentity{
		ArtifactUID:      "model-fixture-tiny-dense",
		ManifestSHA256:   repeatHex("a"),
		SourceGeneration: 1,
	}
	encoded, err := json.Marshal(identity)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"artifact_uid"`)
	assert.Contains(t, string(encoded), `"manifest_sha256"`)
	assert.Contains(t, string(encoded), `"source_generation"`)

	var decoded ArtifactIdentity
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.NoError(t, decoded.Validate())
	require.NoError(t, identity.Matches(decoded))
}

func TestOperationWireRoundTrip(t *testing.T) {
	op := pendingOperation()
	require.NoError(t, op.MarkUnknown("deadline"))
	encoded, err := json.Marshal(op)
	require.NoError(t, err)

	var decoded Operation
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, OpUnknown, decoded.State)
	assert.True(t, decoded.Retained)
	assert.Equal(t, "deadline", decoded.ErrorMessage)
}

func TestErrorKindsRemainDistinguishable(t *testing.T) {
	// The five failure classes callers must tell apart never share a kind.
	kinds := []Kind{
		KindOf(New(KindAuthentication, "manifest", "digest mismatch")),
		KindOf(New(KindDeniedContent, "session", "outside namespace")),
		KindOf(New(KindStaleGeneration, "session", "generation 2")),
		KindOf(New(KindCapacityExhausted, "read", "budget")),
		KindOf(New(KindSourceUnavailable, "file", "gone")),
	}
	unique := make(map[Kind]bool)
	for _, kind := range kinds {
		require.False(t, unique[kind], "duplicate kind %s", kind)
		unique[kind] = true
	}
}

func TestNegotiateContractVersionRefusesOldContract(t *testing.T) {
	for _, versions := range [][]int{nil, {1}, {99}} {
		_, err := NegotiateContractVersion(versions)
		require.Error(t, err)
		assert.Equal(t, KindContractVersion, KindOf(err))
	}
	ver, err := NegotiateContractVersion([]int{1, 2})
	require.NoError(t, err)
	assert.Equal(t, 2, ver)
}
