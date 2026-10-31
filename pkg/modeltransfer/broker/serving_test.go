package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

func TestValidateDescriptorsMatchesReferencedFiles(t *testing.T) {
	const worker = "worker-pod"
	manifest := protocol.Manifest{
		ContractVersion:  protocol.ContractVersion,
		ArtifactUID:      "artifact",
		SourceGeneration: 1,
		Files: []protocol.FileDescriptor{
			{Path: "config.json", Size: 20},
			{Path: "first.safetensors", Size: 100},
			{Path: "second.safetensors", Size: 80},
			{Path: "unused.safetensors", Size: 60},
		},
		Tensors: []protocol.TensorDescriptor{
			{Name: "one", File: "first.safetensors", Dtype: "U8", Shape: []int64{10}, DataBegin: 10, DataEnd: 20},
			{Name: "two", File: "first.safetensors", Dtype: "U8", Shape: []int64{10}, DataBegin: 20, DataEnd: 30},
			{Name: "three", File: "second.safetensors", Dtype: "U8", Shape: []int64{10}, DataBegin: 10, DataEnd: 20},
		},
	}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	record := GenerationRecord{
		ArtifactUID: "artifact", SourceGeneration: 1, ManifestJSON: string(raw),
		ManifestSHA256: hex.EncodeToString(sum[:]), Registrations: 2, Backend: "nixl",
	}
	first := RegistrationDescriptor{Name: "first.safetensors", Addr: 1, Length: 100, MemoryType: "HOST", Backend: "nixl", WorkerUID: worker}
	second := RegistrationDescriptor{Name: "second.safetensors", Addr: 2, Length: 80, MemoryType: "HOST", Backend: "nixl", WorkerUID: worker}
	ancillary := RegistrationDescriptor{Name: "config.json", Addr: 3, Length: 20, MemoryType: "HOST", Backend: "nixl", WorkerUID: worker}
	unused := RegistrationDescriptor{Name: "unused.safetensors", Addr: 4, Length: 60, MemoryType: "HOST", Backend: "nixl", WorkerUID: worker}
	partial := RegistrationDescriptor{Name: "first.safetensors", Addr: 1, Length: 10, MemoryType: "HOST", Backend: "nixl", WorkerUID: worker}
	foreign := RegistrationDescriptor{Name: "first.safetensors", Addr: 1, Length: 100, MemoryType: "HOST", Backend: "nixl", WorkerUID: "other"}
	wrongBackend := RegistrationDescriptor{Name: "first.safetensors", Addr: 1, Length: 100, MemoryType: "HOST", Backend: "hixl", WorkerUID: worker}
	device := RegistrationDescriptor{Name: "first.safetensors", Addr: 1, Length: 100, MemoryType: "DEVICE", Backend: "nixl", WorkerUID: worker}
	noAddress := RegistrationDescriptor{Name: "first.safetensors", Length: 100, MemoryType: "HOST", Backend: "nixl", WorkerUID: worker}

	for _, tc := range []struct {
		name          string
		descriptors   []RegistrationDescriptor
		registrations int32
		wantValid     bool
	}{
		{name: "referenced files only", descriptors: []RegistrationDescriptor{first, second}, registrations: 2, wantValid: true},
		{name: "missing file", descriptors: []RegistrationDescriptor{first}, registrations: 2},
		{name: "duplicate file", descriptors: []RegistrationDescriptor{first, first}, registrations: 2},
		{name: "ancillary file", descriptors: []RegistrationDescriptor{first, ancillary}, registrations: 2},
		{name: "unreferenced safetensors", descriptors: []RegistrationDescriptor{first, unused}, registrations: 2},
		{name: "partial file", descriptors: []RegistrationDescriptor{partial, second}, registrations: 2},
		{name: "foreign worker", descriptors: []RegistrationDescriptor{foreign, second}, registrations: 2},
		{name: "wrong backend", descriptors: []RegistrationDescriptor{wrongBackend, second}, registrations: 2},
		{name: "device memory", descriptors: []RegistrationDescriptor{device, second}, registrations: 2},
		{name: "zero address", descriptors: []RegistrationDescriptor{noAddress, second}, registrations: 2},
		{name: "admitted count differs", descriptors: []RegistrationDescriptor{first, second}, registrations: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := record
			rec.Registrations = tc.registrations
			perr := (&Broker{}).validateDescriptors(&rec, worker, tc.descriptors)
			if tc.wantValid {
				assert.Nil(t, perr)
			} else {
				require.NotNil(t, perr)
				assert.Equal(t, protocol.KindInvalidDescriptor, perr.Kind)
			}
		})
	}

	t.Run("tensor cannot reference ancillary file", func(t *testing.T) {
		badManifest := manifest
		badManifest.Files = []protocol.FileDescriptor{{Path: "config.json", Size: 20}}
		badManifest.Tensors = []protocol.TensorDescriptor{{Name: "one", File: "config.json", Dtype: "U8", Shape: []int64{10}, DataBegin: 0, DataEnd: 10}}
		raw, err := json.Marshal(badManifest)
		require.NoError(t, err)
		sum := sha256.Sum256(raw)
		rec := record
		rec.ManifestJSON = string(raw)
		rec.ManifestSHA256 = hex.EncodeToString(sum[:])
		rec.Registrations = 1
		perr := (&Broker{}).validateDescriptors(&rec, worker, []RegistrationDescriptor{ancillary})
		require.NotNil(t, perr)
		assert.Equal(t, protocol.KindInvalidDescriptor, perr.Kind)
	})
}
