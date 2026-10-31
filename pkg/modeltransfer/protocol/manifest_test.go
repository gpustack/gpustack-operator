package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManifestRawByteBound(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(t, err)
	for _, tt := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "exactly 64 MiB", size: MaxManifestBytes},
		{name: "one byte over", size: MaxManifestBytes + 1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			raw := bytes.Repeat([]byte(" "), tt.size)
			copy(raw, fixture)
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			got, err := LoadManifest(path, "")
			if tt.wantErr {
				require.ErrorContains(t, err, "byte bound")
				require.Equal(t, KindInvalidDescriptor, KindOf(err))
				return
			}
			require.NoError(t, err)
			require.Len(t, got.Tensors, 7)
		})
	}
}

func TestManifestCountBounds(t *testing.T) {
	for _, tt := range []struct {
		name           string
		files, tensors int
	}{
		{name: "files", files: MaxManifestFiles + 1},
		{name: "tensors", tensors: MaxManifestTensors + 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manifest := &Manifest{
				ContractVersion: ContractVersion, ArtifactUID: "artifact", SourceGeneration: 1,
				Files: make([]FileDescriptor, tt.files), Tensors: make([]TensorDescriptor, tt.tensors),
			}
			require.ErrorContains(t, manifest.Check(), "descriptor count exceeds bound")
		})
	}
}

const fixtureDir = "../../../model-transfer/contracts/fixtures/tiny_dense"

func loadFixtureManifest(t *testing.T) *Manifest {
	t.Helper()
	path := filepath.Join(fixtureDir, "manifest.json")
	expected := strings.TrimSpace(tReadFile(t, filepath.Join(fixtureDir, "manifest.sha256")))
	manifest, err := LoadManifest(path, expected)
	require.NoError(t, err)
	return manifest
}

func tReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(raw)
}

func TestGoldenManifestSidecarDigest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(t, err)
	expected := strings.TrimSpace(tReadFile(t, filepath.Join(fixtureDir, "manifest.sha256")))
	assert.Equal(t, expected, digestHex(raw))
}

func TestGoldenManifestLoads(t *testing.T) {
	manifest := loadFixtureManifest(t)
	assert.Equal(t, ContractVersion, manifest.ContractVersion)
	assert.Equal(t, "model-fixture-tiny-dense", manifest.ArtifactUID)
	assert.EqualValues(t, 1, manifest.SourceGeneration)
	assert.Len(t, manifest.Files, 1)
	assert.Len(t, manifest.Tensors, 7)
}

func TestGoldenManifestLayoutMatchesFixture(t *testing.T) {
	manifest := loadFixtureManifest(t)
	blob, err := os.ReadFile(filepath.Join(fixtureDir, "checkpoint.safetensors"))
	require.NoError(t, err)
	for _, tensor := range manifest.Tensors {
		require.NoError(t, tensor.Validate(int64(len(blob))), tensor.Name)
	}
}

func TestLoadManifestDigestMismatch(t *testing.T) {
	path := filepath.Join(fixtureDir, "manifest.json")
	_, err := LoadManifest(path, strings.Repeat("0", 64))
	require.Error(t, err)
	assert.Equal(t, KindAuthentication, KindOf(err))
}

func TestLoadManifestUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(t, err)
	loose := strings.Replace(string(raw), "{", `{"extra":1,`, 1)
	require.NoError(t, os.WriteFile(path, []byte(loose), 0o600))
	_, err = LoadManifest(path, "")
	require.Error(t, err)
	assert.Equal(t, KindInvalidDescriptor, KindOf(err))
}

func TestLoadManifestWrongContractVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	var fields map[string]any
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &fields))
	fields["contract_version"] = 99
	mutated, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, mutated, 0o600))
	_, err = LoadManifest(path, "")
	require.Error(t, err)
	assert.Equal(t, KindContractVersion, KindOf(err))
}

func TestLoadManifestTensorUnknownFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	var fields map[string]any
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &fields))
	tensors := fields["tensors"].([]any)
	first := tensors[0].(map[string]any)
	first["file"] = "ghost.bin"
	mutated, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, mutated, 0o600))
	_, err = LoadManifest(path, "")
	require.Error(t, err)
	assert.Equal(t, KindInvalidDescriptor, KindOf(err))
}

func TestLoadManifestOverlappingTensors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	var fields map[string]any
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &fields))
	tensors := fields["tensors"].([]any)
	first := tensors[0].(map[string]any)
	second := tensors[1].(map[string]any)
	// Move the second tensor to overlap the first one.
	second["data_begin"] = first["data_begin"].(float64) + 4
	second["data_end"] = second["data_begin"].(float64) + (second["data_end"].(float64) - second["data_begin"].(float64))
	mutated, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, mutated, 0o600))
	_, err = LoadManifest(path, "")
	require.Error(t, err)
	assert.Equal(t, KindInvalidDescriptor, KindOf(err))
}

func TestManifestLookups(t *testing.T) {
	manifest := loadFixtureManifest(t)
	tensor, err := manifest.Tensor("model.norm.weight")
	require.NoError(t, err)
	assert.Equal(t, "F32", tensor.Dtype)

	_, err = manifest.Tensor("nope")
	assert.Equal(t, KindInvalidDescriptor, KindOf(err))

	file, err := manifest.File("checkpoint.safetensors")
	require.NoError(t, err)
	assert.EqualValues(t, 734, file.Size)

	_, err = manifest.File("nope.bin")
	assert.Equal(t, KindInvalidDescriptor, KindOf(err))
}

func TestLoadManifestRejectsTrailingTokensAndMissingSections(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	require.NoError(t, err)

	t.Run("trailing JSON object rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "manifest.json")
		require.NoError(t, os.WriteFile(path, append(append([]byte{}, raw...), []byte(`{"extra":1}`)...), 0o600))
		_, err := LoadManifest(path, "")
		require.Error(t, err)
		assert.Equal(t, KindInvalidDescriptor, KindOf(err))
	})

	t.Run("missing files key rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "manifest.json")
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		delete(fields, "files")
		mutated, err := json.Marshal(fields)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, mutated, 0o600))
		_, err = LoadManifest(path, "")
		require.Error(t, err)
		assert.Equal(t, KindInvalidDescriptor, KindOf(err))
	})

	t.Run("missing tensors key rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "manifest.json")
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		delete(fields, "tensors")
		mutated, err := json.Marshal(fields)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, mutated, 0o600))
		_, err = LoadManifest(path, "")
		require.Error(t, err)
		assert.Equal(t, KindInvalidDescriptor, KindOf(err))
	})
}
