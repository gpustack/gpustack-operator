package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	// MaxManifestBytes bounds the exact encoded manifest before parsing.
	MaxManifestBytes = 64 << 20
	// MaxManifestFiles bounds file descriptors in one manifest.
	MaxManifestFiles = 16384
	// MaxManifestTensors bounds tensor descriptors in one manifest.
	MaxManifestTensors = 1 << 20
	// MaxManifestResponseBytes includes worst-case JSON string escaping and its envelope.
	MaxManifestResponseBytes = 6*MaxManifestBytes + (1 << 20)
)

// Manifest binds one artifact UID and source generation to the complete
// file and tensor layout. ManifestSHA256 is the digest of the exact manifest
// bytes; LoadManifest fills it and the wire format omits it.
type Manifest struct {
	ContractVersion  int                `json:"contract_version"`
	ArtifactUID      string             `json:"artifact_uid"`
	SourceGeneration int64              `json:"source_generation"`
	Files            []FileDescriptor   `json:"files"`
	Tensors          []TensorDescriptor `json:"tensors"`
	TensorPages      []TensorPage       `json:"tensor_pages,omitempty"`
	ManifestSHA256   string             `json:"-"`
	pageIndex        *manifestPageIndex
}

// Identity returns the artifact identity this manifest vouches for. The
// digest is present only when the manifest was loaded from bytes.
func (m *Manifest) Identity() ArtifactIdentity {
	return ArtifactIdentity{
		ArtifactUID:      m.ArtifactUID,
		ManifestSHA256:   m.ManifestSHA256,
		SourceGeneration: m.SourceGeneration,
	}
}

// File returns the descriptor of one file by path.
func (m *Manifest) File(path string) (FileDescriptor, error) {
	for _, file := range m.Files {
		if file.Path == path {
			return file, nil
		}
	}
	return FileDescriptor{}, New(KindInvalidDescriptor, "manifest.file",
		"unknown file %q", path)
}

// Tensor returns one tensor without cancellation. Request paths use TensorContext.
func (m *Manifest) Tensor(name string) (TensorDescriptor, error) {
	if len(m.TensorPages) > 0 {
		return m.TensorContext(context.Background(), name)
	}
	for _, tensor := range m.Tensors {
		if tensor.Name == name {
			return tensor, nil
		}
	}
	return TensorDescriptor{}, New(KindInvalidDescriptor, "manifest.tensor",
		"unknown tensor %q", name)
}

// Check validates the manifest structure: contract version, identity fields,
// file and tensor descriptors, and per-file overlap. It does not verify the
// manifest digest, which only the loader knows.
func (m *Manifest) Check() error {
	if err := CheckContractVersion(m.ContractVersion); err != nil {
		return err
	}
	if !uidPattern.MatchString(m.ArtifactUID) {
		return New(KindIdentity, "manifest.artifact_uid",
			"invalid artifact uid %q", m.ArtifactUID)
	}
	if m.SourceGeneration < 1 {
		return New(KindIdentity, "manifest.source_generation",
			"must be positive, got %d", m.SourceGeneration)
	}
	if m.Files == nil {
		return New(KindInvalidDescriptor, "manifest.files", "files list is required")
	}
	if m.Tensors == nil {
		return New(KindInvalidDescriptor, "manifest.tensors", "tensors list is required")
	}
	if len(m.Files) > MaxManifestFiles || len(m.Tensors) > MaxManifestTensors {
		return New(KindInvalidDescriptor, "manifest", "descriptor count exceeds bound")
	}
	seenPaths := make(map[string]bool)
	for _, file := range m.Files {
		if err := file.Validate(); err != nil {
			return err
		}
		if seenPaths[file.Path] {
			return New(KindInvalidDescriptor, "manifest.files",
				"duplicate file entry %q", file.Path)
		}
		seenPaths[file.Path] = true
	}
	fileSizes := make(map[string]int64)
	for _, file := range m.Files {
		fileSizes[file.Path] = file.Size
	}
	if len(m.TensorPages) > 0 {
		return m.checkPageReferences(fileSizes)
	}
	byFile := make(map[string][]TensorDescriptor)
	names := make(map[string]bool)
	for _, tensor := range m.Tensors {
		if _, ok := fileSizes[tensor.File]; !ok {
			return New(KindInvalidDescriptor, "manifest.tensors",
				"tensor %q references unknown file %q", tensor.Name, tensor.File)
		}
		if names[tensor.Name] {
			return New(KindInvalidDescriptor, "manifest.tensors",
				"duplicate tensor name %q", tensor.Name)
		}
		names[tensor.Name] = true
		byFile[tensor.File] = append(byFile[tensor.File], tensor)
	}
	paths := make([]string, 0, len(byFile))
	for path := range byFile {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := ValidateFileTensors(byFile[path], fileSizes[path]); err != nil {
			return err
		}
	}
	return nil
}

// LoadManifest reads and fully verifies a manifest file. expectedSHA256,
// when non-empty, must equal the sha256 of the manifest bytes; a mismatch
// is an authentication failure, not a parse error. Unknown fields are
// rejected so a newer manifest cannot silently drop fields.
func LoadManifest(path, expectedSHA256 string) (*Manifest, error) {
	return LoadManifestContext(context.Background(), path, expectedSHA256)
}

// LoadManifestContext verifies a local manifest and its pages within the caller context.
func LoadManifestContext(ctx context.Context, path, expectedSHA256 string) (*Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, New(KindSourceUnavailable, "manifest",
			"cannot read %s: %v", path, err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxManifestBytes+1))
	if err != nil {
		return nil, New(KindSourceUnavailable, "manifest", "cannot read %s: %v", path, err)
	}
	if len(raw) > MaxManifestBytes {
		return nil, New(KindInvalidDescriptor, "manifest", "manifest exceeds the byte bound")
	}
	digest := digestHex(raw)
	if expectedSHA256 != "" && expectedSHA256 != digest {
		return nil, New(KindAuthentication, "manifest",
			"digest mismatch: expected %s, got %s", expectedSHA256, digest)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, New(KindInvalidDescriptor, "manifest",
			"invalid manifest JSON: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, New(KindInvalidDescriptor, "manifest",
			"trailing JSON tokens after manifest object")
	}
	if len(manifest.TensorPages) > 0 && len(raw) > MaxPagedManifestBytes {
		return nil, New(KindInvalidDescriptor, "manifest", "paged root exceeds byte bound")
	}
	if err := manifest.Check(); err != nil {
		return nil, err
	}
	manifest.ManifestSHA256 = digest
	if len(manifest.TensorPages) > 0 {
		if err := manifest.BindPages(ctx, LocalPageReader(filepath.Join(filepath.Dir(path), "pages"))); err != nil {
			return nil, err
		}
	}
	return &manifest, nil
}

// digestHex returns the lowercase hex sha256 of raw.
func digestHex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
