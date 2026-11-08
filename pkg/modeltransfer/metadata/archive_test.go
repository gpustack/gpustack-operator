package metadata

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

func TestMetadataArchiveRoundTripWithoutCheckpoint(t *testing.T) {
	view, dir := buildView(t)
	var output bytes.Buffer
	require.NoError(t, ExportView(context.Background(), dir, &output))
	root := t.TempDir()
	for range 2 {
		imported, err := ImportView(context.Background(), bytes.NewReader(output.Bytes()), root,
			view.ArtifactDigest, DefaultRequiredFiles)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, view.ID()), imported)
		actual, err := Verify(imported)
		require.NoError(t, err)
		assert.True(t, actual.Incomplete)
		assert.Equal(t, view.Files, actual.Files)
	}
}

func TestMetadataArchiveRejectsInvalidEntries(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		kind      byte
		size      int64
		duplicate bool
	}{
		{name: "escape", path: "../config.json", kind: tar.TypeReg},
		{name: "absolute", path: "/config.json", kind: tar.TypeReg},
		{name: "symlink", path: "config.json", kind: tar.TypeSymlink},
		{name: "oversized metadata", path: "config.json", kind: tar.TypeReg, size: MaxMetadataBytes + 1},
		{name: "unknown file", path: "weights.bin", kind: tar.TypeReg},
		{name: "duplicate file", path: "config.json", kind: tar.TypeReg, duplicate: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			header := &tar.Header{Name: test.path, Typeflag: test.kind, Size: test.size, Mode: 0o444}
			require.NoError(t, writer.WriteHeader(header))
			if test.duplicate {
				require.NoError(t, writer.WriteHeader(header))
			}
			// An oversized declaration is rejected before its missing body is read.
			if test.size == 0 {
				require.NoError(t, writer.Close())
			}
			_, err := ImportView(context.Background(), &archive, t.TempDir(), "sha256:"+repeatHex('a'), nil)
			require.Error(t, err)
		})
	}
}

func TestMetadataArchiveRefusesStaleIdentity(t *testing.T) {
	_, dir := buildView(t)
	var output bytes.Buffer
	require.NoError(t, ExportView(context.Background(), dir, &output))
	_, err := ImportView(context.Background(), &output, t.TempDir(), "sha256:"+repeatHex('b'), DefaultRequiredFiles)
	require.ErrorIs(t, err, ErrStaleDigest)
}

func TestMetadataArchiveCancellation(t *testing.T) {
	_, dir := buildView(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, ExportView(ctx, dir, io.Discard), context.Canceled)
	_, err := ImportView(ctx, bytes.NewReader(nil), t.TempDir(), "sha256:"+repeatHex('a'), nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestMetadataArchiveRejectsPayloadInSelfConsistentRecord(t *testing.T) {
	view, dir := buildView(t)
	for i := range view.Files {
		file := &view.Files[i]
		if !file.Truncated {
			continue
		}
		name := filepath.Join(dir, filepath.FromSlash(file.Name))
		raw, err := os.ReadFile(name)
		require.NoError(t, err)
		raw = append(raw, 0xff)
		require.NoError(t, os.Chmod(name, 0o600))
		require.NoError(t, os.WriteFile(name, raw, 0o444))
		file.Size = int64(len(raw))
		sum := sha256.Sum256(raw)
		file.SHA256 = hex.EncodeToString(sum[:])
		break
	}
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	record := filepath.Join(dir, viewFile)
	require.NoError(t, os.Chmod(record, 0o600))
	require.NoError(t, os.WriteFile(record, raw, 0o444))
	_, err = Verify(dir)
	require.NoError(t, err, "a self-consistent file hash does not prove a metadata-only stub")
	require.ErrorContains(t, ExportView(context.Background(), dir, io.Discard), "checkpoint payload")
}

type importStageProbe struct {
	*bytes.Reader
	t       *testing.T
	root    string
	checked bool
}

func (r *importStageProbe) Read(p []byte) (int, error) {
	if !r.checked {
		r.checked = true
		entries, err := os.ReadDir(r.root)
		require.NoError(r.t, err)
		var active string
		for _, entry := range entries {
			if entry.IsDir() {
				active = entry.Name()
				break
			}
		}
		require.True(r.t, strings.HasPrefix(active, stagingPrefix), "import stage must participate in owned crash recovery")
		require.NoError(r.t, ReclaimAbandonedStaging(r.root))
		require.DirExists(r.t, filepath.Join(r.root, active), "recovery must preserve an active import")
	}
	return r.Reader.Read(p)
}

func TestImportViewUsesOwnedRecoverableStaging(t *testing.T) {
	view, dir := buildView(t)
	var archive bytes.Buffer
	require.NoError(t, ExportView(t.Context(), dir, &archive))
	root := t.TempDir()
	input := &importStageProbe{Reader: bytes.NewReader(archive.Bytes()), t: t, root: root}
	imported, err := ImportView(t.Context(), input, root, view.ArtifactDigest, DefaultRequiredFiles)
	require.NoError(t, err)
	require.True(t, input.checked)
	_, err = Verify(imported)
	require.NoError(t, err)
}

func TestImportStoreViewPreservesReferencedFinalAndRepairsUnreferenced(t *testing.T) {
	view, dir := buildView(t)
	var archive bytes.Buffer
	require.NoError(t, ExportView(t.Context(), dir, &archive))

	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	hex := store.HexOf(view.ArtifactDigest)

	imported, err := ImportStoreView(t.Context(), st, bytes.NewReader(archive.Bytes()), view.ArtifactDigest, DefaultRequiredFiles)
	require.NoError(t, err)

	// Corrupt the published view while an active reference protects hex.
	require.NoError(t, st.Acquire(t.Context(), store.Ref{VolumeID: "csi-mounted", Hex: hex}))
	require.NoError(t, os.Chmod(filepath.Join(imported, "config.json"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(imported, "config.json"), []byte("corrupt"), 0o644))
	beforeInfo, err := os.Stat(imported)
	require.NoError(t, err)
	beforeIno := beforeInfo.Sys().(*syscall.Stat_t).Ino

	// Both standalone ImportView and ImportStoreView with another live ref must
	// refuse to replace the referenced directory.
	_, err = ImportView(t.Context(), bytes.NewReader(archive.Bytes()), st.ViewsRoot(), view.ArtifactDigest, DefaultRequiredFiles)
	require.Error(t, err)
	require.NoError(t, st.Acquire(t.Context(), store.Ref{VolumeID: "csi-fetching", Hex: hex}))
	_, err = ImportStoreView(t.Context(), st, bytes.NewReader(archive.Bytes()), view.ArtifactDigest, DefaultRequiredFiles, "csi-fetching")
	require.Error(t, err)
	afterInfo, err := os.Stat(imported)
	require.NoError(t, err)
	assert.Equal(t, beforeIno, afterInfo.Sys().(*syscall.Stat_t).Ino,
		"ImportView must never replace a referenced final view directory")

	// Once csi-mounted unmounts, csi-fetching's own pre-mount reference repairs the view.
	require.NoError(t, st.RemoveRef("csi-mounted"))
	repaired, err := ImportStoreView(t.Context(), st, bytes.NewReader(archive.Bytes()), view.ArtifactDigest, DefaultRequiredFiles, "csi-fetching")
	require.NoError(t, err)
	assert.Equal(t, imported, repaired)
	_, err = Verify(repaired)
	require.NoError(t, err)
}
