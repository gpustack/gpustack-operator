package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildSourceTree writes a small verified-tree-like source directory: the
// golden fixture's manifest and checkpoint, a config, a tokenizer config,
// and one unrelated file.
func buildSourceTree(t *testing.T) string {
	t.Helper()
	fixture := filepath.Join("..", "..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
	dir := t.TempDir()
	for _, name := range []string{"manifest.json", "checkpoint.safetensors"} {
		b, err := os.ReadFile(filepath.Join(fixture, name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"architectures":["TinyForCausalLM"],"tie_word_embeddings":true}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tokenizer_config.json"),
		[]byte(`{"model_max_length":8}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"),
		[]byte("not metadata"), 0o644))
	return dir
}

func buildView(t *testing.T) (*View, string) {
	t.Helper()
	src := buildSourceTree(t)
	viewDir := filepath.Join(t.TempDir(), "view")
	view, err := Build(src, viewDir, "sha256:"+repeatHex('a'), nil)
	require.NoError(t, err)
	return view, viewDir
}

func repeatHex(char byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = char
	}
	return string(out)
}

func fileSha(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestBuildProducesTheMetadataView(t *testing.T) {
	view, viewDir := buildView(t)

	assert.Equal(t, ViewKind, view.Kind)
	assert.Equal(t, "sha256:"+repeatHex('a'), view.ArtifactDigest)
	assert.True(t, view.Incomplete, "a metadata view is never a complete checkpoint")

	byName := map[string]ViewFile{}
	for _, f := range view.Files {
		byName[f.Name] = f
	}
	// Required files copied verbatim.
	for _, name := range []string{"manifest.json", "config.json"} {
		f, ok := byName[name]
		require.True(t, ok, name)
		assert.False(t, f.Truncated, name)
		assert.Equal(t, fileSha(t, filepath.Join(viewDir, name)), f.SHA256)
	}
	// Known optional files copied when present.
	_, ok := byName["tokenizer_config.json"]
	assert.True(t, ok)
	// Unrelated files are not part of the view.
	_, ok = byName["README.md"]
	assert.False(t, ok)

	// The checkpoint is present as its header only: 8-byte length plus the
	// JSON header, without the weight payload.
	stub := byName["checkpoint.safetensors"]
	require.True(t, stub.Truncated)
	fixture := filepath.Join("..", "..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
	full, err := os.ReadFile(filepath.Join(fixture, "checkpoint.safetensors"))
	require.NoError(t, err)
	copied, err := os.ReadFile(filepath.Join(viewDir, "checkpoint.safetensors"))
	require.NoError(t, err)
	headerLength := int64(le64(copied[:8]))
	require.Less(t, 8+headerLength, int64(len(full)),
		"the stub must be shorter than the full checkpoint")
	require.Equal(t, full[:8+headerLength], copied,
		"the stub is byte-identical to the source prefix")
}

func TestARewrittenRecordChangesTheCallerVisibleDigest(t *testing.T) {
	// Verify returns the digest of record so the caller compares it against
	// the artifact it asked for; a rewritten record cannot pose as another
	// artifact's view.
	_, viewDir := buildView(t)
	rewriteView(t, viewDir, func(v map[string]any) {
		v["artifactDigest"] = "sha256:" + repeatHex('z')
	})
	view, err := Verify(viewDir)
	require.NoError(t, err)
	assert.Equal(t, "sha256:"+repeatHex('z'), view.ArtifactDigest,
		"the caller sees exactly what the record claims")
}

func TestViewIDIsContentAddressedAndStable(t *testing.T) {
	src := buildSourceTree(t)
	first, err := Build(src, filepath.Join(t.TempDir(), "v1"), "sha256:"+repeatHex('a'), nil)
	require.NoError(t, err)
	second, err := Build(src, filepath.Join(t.TempDir(), "v2"), "sha256:"+repeatHex('a'), nil)
	require.NoError(t, err)
	assert.Equal(t, first.ID(), second.ID())
	assert.Len(t, first.ID(), 64)

	// A different artifact digest is a different view.
	third, err := Build(src, filepath.Join(t.TempDir(), "v3"), "sha256:"+repeatHex('b'), nil)
	require.NoError(t, err)
	assert.NotEqual(t, first.ID(), third.ID())
}

func TestBuildIsIdempotentIntoTheSameDirectory(t *testing.T) {
	src := buildSourceTree(t)
	dir := filepath.Join(t.TempDir(), "view")
	first, err := Build(src, dir, "sha256:"+repeatHex('a'), nil)
	require.NoError(t, err)
	again, err := Build(src, dir, "sha256:"+repeatHex('a'), nil)
	require.NoError(t, err)
	assert.Equal(t, first.ID(), again.ID())
}

func TestBuildRefusals(t *testing.T) {
	digest := "sha256:" + repeatHex('a')

	cases := []struct {
		name     string
		required []string
		mutate   func(t *testing.T, dir string)
		wantErr  error
	}{
		{
			name:     "unknown required file",
			required: []string{"config.json", "weights.bin"},
			wantErr:  ErrUnknownRequiredFile,
		},
		{
			name:     "required file escaping the tree",
			required: []string{"../secret"},
			wantErr:  ErrPathEscape,
		},
		{
			name:     "absolute required file",
			required: []string{"/etc/passwd"},
			wantErr:  ErrPathEscape,
		},
		{
			name:     "missing required file in the tree",
			required: []string{"generation_config.json"},
			wantErr:  ErrMissingRequiredFile,
		},
		{
			name: "symlinked required file",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
				require.NoError(t, os.Symlink(filepath.Join(dir, "manifest.json"),
					filepath.Join(dir, "config.json")))
			},
			wantErr: ErrNotRegularFile,
		},
		{
			name: "symlinked checkpoint",
			mutate: func(t *testing.T, dir string) {
				fixture := filepath.Join("..", "..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
				require.NoError(t, os.Remove(filepath.Join(dir, "checkpoint.safetensors")))
				require.NoError(t, os.Symlink(filepath.Join(fixture, "checkpoint.safetensors"),
					filepath.Join(dir, "checkpoint.safetensors")))
			},
			wantErr: ErrNotRegularFile,
		},
		{
			name: "directory where a file is required",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
				require.NoError(t, os.Mkdir(filepath.Join(dir, "config.json"), 0o755))
			},
			wantErr: ErrNotRegularFile,
		},
		{
			name: "oversized metadata file",
			mutate: func(t *testing.T, dir string) {
				big := make([]byte, MaxMetadataBytes+1)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), big, 0o644))
			},
			wantErr: ErrTooLarge,
		},
		{
			name: "implausible safetensors header length",
			mutate: func(t *testing.T, dir string) {
				b := make([]byte, 8)
				putLE64(b, uint64(MaxHeaderBytes+1))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "checkpoint.safetensors"), b, 0o644))
			},
			wantErr: ErrHeaderTooLarge,
		},
		{
			name: "header past the file end",
			mutate: func(t *testing.T, dir string) {
				b := make([]byte, 8)
				putLE64(b, 64)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "checkpoint.safetensors"), b, 0o644))
			},
			wantErr: ErrInvalidHeader,
		},
		{
			name: "header is not JSON",
			mutate: func(t *testing.T, dir string) {
				b := append(make([]byte, 8), []byte("not json at all...")...)
				putLE64(b[:8], uint64(len(b)-8))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "checkpoint.safetensors"), b, 0o644))
			},
			wantErr: ErrInvalidHeader,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := buildSourceTree(t)
			if tc.mutate != nil {
				tc.mutate(t, dir)
			}
			required := tc.required
			if required == nil {
				required = DefaultRequiredFiles
			}
			_, err := Build(dir, filepath.Join(t.TempDir(), "view"), digest, required)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestVerifyAcceptsAnUntouchedView(t *testing.T) {
	_, viewDir := buildView(t)
	view, err := Verify(viewDir)
	require.NoError(t, err)
	assert.True(t, view.Incomplete)
}

func TestVerifyRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
		wantEr error
	}{
		{
			name: "tampered metadata file",
			mutate: func(t *testing.T, dir string) {
				appendFile(t, filepath.Join(dir, "config.json"), []byte("x"))
			},
			wantEr: ErrTampered,
		},
		{
			name: "tampered header stub",
			mutate: func(t *testing.T, dir string) {
				appendFile(t, filepath.Join(dir, "checkpoint.safetensors"), []byte("x"))
			},
			wantEr: ErrTampered,
		},
		{
			name: "unknown extra file",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "extra.bin"), []byte("x"), 0o644))
			},
			wantEr: ErrTampered,
		},
		{
			name: "a listed file replaced by a symlink",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
				require.NoError(t, os.Symlink(filepath.Join(dir, "manifest.json"),
					filepath.Join(dir, "config.json")))
			},
			wantEr: ErrNotRegularFile,
		},
		{
			name: "view record claiming completeness",
			mutate: func(t *testing.T, dir string) {
				rewriteView(t, dir, func(v map[string]any) { v["incomplete"] = false })
			},
			wantEr: ErrNotAMetadataView,
		},
		{
			name: "view record of another kind",
			mutate: func(t *testing.T, dir string) {
				rewriteView(t, dir, func(v map[string]any) { v["kind"] = "something-else" })
			},
			wantEr: ErrNotAMetadataView,
		},
		{
			name: "missing view record",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, viewFile)))
			},
			wantEr: ErrNotAMetadataView,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, viewDir := buildView(t)
			tc.mutate(t, viewDir)
			_, err := Verify(viewDir)
			require.ErrorIs(t, err, tc.wantEr)
		})
	}
}

// TestHeaderParsingMatchesTheAdapterProbe checks the parity that matters:
// the header stub a destination receives answers the same configuration
// questions the adapter's probe asks — dtype, shape, and byte offsets — and
// they agree with the reader manifest the fixture carries.
func TestHeaderParsingMatchesTheAdapterProbe(t *testing.T) {
	fixture := filepath.Join("..", "..", "..", "model-transfer", "contracts", "fixtures", "tiny_dense")
	raw, err := os.ReadFile(filepath.Join(fixture, "checkpoint.safetensors"))
	require.NoError(t, err)
	stub := raw[:8+int64(le64(raw[:8]))]

	header, err := ParseHeader(stub)
	require.NoError(t, err)
	require.NotEmpty(t, header.Entries)

	manifestRaw, err := os.ReadFile(filepath.Join(fixture, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		Tensors []struct {
			Name      string  `json:"name"`
			Dtype     string  `json:"dtype"`
			Shape     []int64 `json:"shape"`
			DataBegin int64   `json:"data_begin"`
			DataEnd   int64   `json:"data_end"`
		} `json:"tensors"`
	}
	require.NoError(t, json.Unmarshal(manifestRaw, &manifest))
	require.NotEmpty(t, manifest.Tensors)
	for _, tensor := range manifest.Tensors {
		entry, ok := header.Entries[tensor.Name]
		require.True(t, ok, tensor.Name)
		assert.Equal(t, tensor.Dtype, entry.Dtype, tensor.Name)
		assert.Equal(t, tensor.Shape, entry.Shape, tensor.Name)
		assert.Equal(t, tensor.DataBegin, header.DataOffset+entry.Begin, tensor.Name)
		assert.Equal(t, tensor.DataEnd, header.DataOffset+entry.End, tensor.Name)
	}

	// Duplicate header keys are refused the way the probe refuses them, and
	// so is an entry without a dtype and shape.
	_, err = ParseHeader(stubWithHeader(t, `{"w":{"dtype":"F32","shape":[1],"data_offsets":[0,4]},"w":{"dtype":"F32","shape":[1],"data_offsets":[0,4]}}`))
	require.ErrorIs(t, err, ErrInvalidHeader)
	_, err = ParseHeader(stubWithHeader(t, `{"w":{"shape":[1],"data_offsets":[0,4]}}`))
	require.ErrorIs(t, err, ErrInvalidHeader)
}

// stubWithHeader wraps a JSON header the way a safetensors file does.
func stubWithHeader(t *testing.T, header string) []byte {
	t.Helper()
	stub := make([]byte, 8, 8+len(header))
	putLE64(stub[:8], uint64(len(header)))
	return append(stub, header...)
}

func appendFile(t *testing.T, path string, extra []byte) {
	t.Helper()
	require.NoError(t, os.Chmod(path, 0o644))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	_, err = f.Write(extra)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func rewriteView(t *testing.T, dir string, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, viewFile)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var view map[string]any
	require.NoError(t, json.Unmarshal(raw, &view))
	mutate(view)
	b, err := json.Marshal(view)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o644))
	require.NoError(t, os.WriteFile(path, b, 0o644))
}

func putLE64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * uint(i)))
	}
}

// --- the crosscheck fixes, each pinned by its own refusal or outcome ---

func TestVerifyRejectsDuplicateListedNames(t *testing.T) {
	_, viewDir := buildView(t)
	rewriteView(t, viewDir, func(v map[string]any) {
		files := v["files"].([]any)
		first := files[0].(map[string]any)
		clone := map[string]any{}
		for key, value := range first {
			clone[key] = value
		}
		v["files"] = append(files, clone)
	})
	_, err := Verify(viewDir)
	require.ErrorIs(t, err, ErrTampered)
}

func TestVerifyRejectsTheRecordListingItself(t *testing.T) {
	_, viewDir := buildView(t)
	rewriteView(t, viewDir, func(v map[string]any) {
		files := v["files"].([]any)
		v["files"] = append(files, map[string]any{
			"name": "view.json", "size": 10, "sha256": repeatHex('0'),
		})
	})
	_, err := Verify(viewDir)
	require.ErrorIs(t, err, ErrTampered)
}

func TestVerifyRejectsUnrecordedSubdirectories(t *testing.T) {
	_, viewDir := buildView(t)
	require.NoError(t, os.Chmod(viewDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(viewDir, "extra"), 0o755))
	_, err := Verify(viewDir)
	require.ErrorIs(t, err, ErrTampered)
}

func TestBuildIsConcurrencySafeAndIdempotent(t *testing.T) {
	src := buildSourceTree(t)
	dir := filepath.Join(t.TempDir(), "view")
	results := make(chan *View, 8)
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view, err := Build(src, dir, "sha256:"+repeatHex('a'), nil)
			if err != nil {
				failures <- err
				return
			}
			results <- view
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatalf("a concurrent build failed: %v", err)
	}
	var id string
	for view := range results {
		if id == "" {
			id = view.ID()
			continue
		}
		require.Equal(t, id, view.ID(), "concurrent builds diverged")
	}
	require.NotEmpty(t, id)
	// No staging directory survived the race.
	entries, err := os.ReadDir(filepath.Dir(dir))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".staging-", "a staging directory leaked")
	}
}

func TestKnownFilesCarryTheWiderConfigurationSet(t *testing.T) {
	src := buildSourceTree(t)
	for _, name := range []string{
		"model.safetensors.index.json", "tokenizer.model", "vocab.json",
		"merges.txt", "added_tokens.json", "chat_template.jinja",
		"preprocessor_config.json", "processor_config.json", "quant_model_description.json",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(src, name),
			[]byte("{}"), 0o644))
	}
	view, err := Build(src, filepath.Join(t.TempDir(), "view"),
		"sha256:"+repeatHex('a'), nil)
	require.NoError(t, err)
	held := map[string]bool{}
	for _, f := range view.Files {
		held[f.Name] = true
	}
	for _, name := range []string{
		"model.safetensors.index.json", "tokenizer.model", "vocab.json",
		"merges.txt", "added_tokens.json", "chat_template.jinja",
		"preprocessor_config.json", "processor_config.json", "quant_model_description.json",
	} {
		assert.True(t, held[name], name)
	}
}

func TestHeaderDataEndPastTheFileIsRefused(t *testing.T) {
	// A header whose declared data offsets run past the file's bytes is a
	// corrupt or hostile checkpoint, not a big model.
	dir := buildSourceTree(t)
	header := `{"w":{"dtype":"F32","shape":[1024],"data_offsets":[0,4096]}}`
	stub := make([]byte, 8, 8+len(header))
	putLE64(stub[:8], uint64(len(header)))
	stub = append(stub, header...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "checkpoint.safetensors"),
		stub, 0o644))
	_, err := Build(dir, filepath.Join(t.TempDir(), "view"),
		"sha256:"+repeatHex('a'), nil)
	require.ErrorIs(t, err, ErrInvalidHeader)
}

func TestBuildCreatesMissingViewParents(t *testing.T) {
	source := buildSourceTree(t)
	viewDir := filepath.Join(t.TempDir(), "missing", "parent", "view")
	view, err := Build(source, viewDir, "sha256:"+repeatHex('a'), nil)
	require.NoError(t, err)
	verified, err := Verify(viewDir)
	require.NoError(t, err)
	assert.Equal(t, view.ID(), verified.ID())
}
