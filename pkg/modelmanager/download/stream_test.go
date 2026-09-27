package download

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const streamContent = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func streamDigest(t *testing.T, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))

	return "sha256:" + hex.EncodeToString(sum[:])
}

// appendTo lands bytes on disk at their offset, the invariant a checkpoint promises: the hashed
// prefix is really there.
func appendTo(dest, chunk string) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(chunk)

	return err
}

func TestStreamHasherHashesInOrderAndCheckpoints(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "weights.bin")
	checkpointPath := filepath.Join(dir, "weights.checkpoint")
	digest := streamDigest(t, streamContent)
	s, err := NewStreamHasher(digest, int64(len(streamContent)), checkpointPath, 16)
	require.NoError(t, err)

	// Three arrivals in byte order, whoever delivered them; each lands on disk at its offset
	// before it is hashed, which is the invariant a checkpoint promises.
	for _, chunk := range []string{streamContent[:20], streamContent[20:45], streamContent[45:]} {
		require.NoError(t, appendTo(dest, chunk))
		require.NoError(t, s.Hash([]byte(chunk)))
	}
	assert.Equal(t, int64(len(streamContent)), s.Written())
	require.NoError(t, s.Verify(dest))

	offset := ResumableOffset(dest, checkpointPath, int64(len(streamContent)))
	assert.Equal(t, int64(len(streamContent)), offset, "the final checkpoint holds the whole file")
}

func TestStreamHasherResumeContinuesFromCheckpoint(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "weights.bin")
	checkpointPath := filepath.Join(dir, "weights.checkpoint")
	digest := streamDigest(t, streamContent)

	first, err := NewStreamHasher(digest, int64(len(streamContent)), checkpointPath, 16)
	require.NoError(t, err)
	require.NoError(t, appendTo(dest, streamContent[:20]))
	require.NoError(t, first.Hash([]byte(streamContent[:20])))
	require.NoError(t, first.Checkpoint())

	second, offset, err := Resume(digest, int64(len(streamContent)), dest, checkpointPath, 16)
	require.NoError(t, err)
	assert.Equal(t, int64(20), offset, "the checkpoint's bytes are carried over")
	require.NoError(t, appendTo(dest, streamContent[20:]))
	require.NoError(t, second.Hash([]byte(streamContent[20:])))
	require.NoError(t, second.Verify(dest))

	body, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, streamContent, string(body))
}

func TestStreamHasherResumeReHashesWhenTheStateIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "weights.bin")
	checkpointPath := filepath.Join(dir, "weights.checkpoint")
	digest := streamDigest(t, streamContent)
	require.NoError(t, os.WriteFile(dest, []byte(streamContent[:20]), 0o644))

	// A checkpoint this build cannot restore: the offset is valid, the state is not.
	state, err := json.Marshal(checkpoint{Offset: 20, State: []byte("not a hash state")})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(checkpointPath, state, 0o600))

	second, offset, err := Resume(digest, int64(len(streamContent)), dest, checkpointPath, 16)
	require.NoError(t, err)
	assert.Equal(t, int64(20), offset, "the checkpointed bytes are re-hashed from disk")
	require.NoError(t, second.Hash([]byte(streamContent[20:])))
	require.NoError(t, second.Verify(dest))
}

func TestStreamHasherStartsOverWithoutACheckpoint(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "weights.bin")
	digest := streamDigest(t, streamContent)
	require.NoError(t, os.WriteFile(dest, []byte(streamContent[:20]), 0o644))

	second, offset, err := Resume(digest, int64(len(streamContent)), dest, filepath.Join(dir, "absent.checkpoint"), 16)
	require.NoError(t, err)
	assert.Equal(t, int64(0), offset)
	require.NoError(t, second.Hash([]byte(streamContent)))
	require.NoError(t, second.Verify(dest))
}

func TestStreamHasherVerifyRejectsWrongContent(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "weights.bin")
	checkpointPath := filepath.Join(dir, "weights.checkpoint")
	s, err := NewStreamHasher(streamDigest(t, streamContent), int64(len(streamContent)), checkpointPath, 16)
	require.NoError(t, err)
	require.NoError(t, s.Hash([]byte(strings.Repeat("X", len(streamContent)))))
	require.NoError(t, os.WriteFile(dest, []byte(strings.Repeat("X", len(streamContent))), 0o644))

	err = s.Verify(dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the manifest says")
}

func TestStreamHasherCheckpointsAtTheInterval(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "weights.bin")
	checkpointPath := filepath.Join(dir, "weights.checkpoint")
	s, err := NewStreamHasher(streamDigest(t, streamContent), int64(len(streamContent)), checkpointPath, 16)
	require.NoError(t, err)
	require.NoError(t, appendTo(dest, streamContent[:20]))
	require.NoError(t, s.Hash([]byte(streamContent[:20])))

	cp := readCheckpointOffset(t, checkpointPath)
	assert.Equal(t, int64(20), cp, "crossing an interval boundary checkpoints everything hashed so far")

	require.NoError(t, appendTo(dest, streamContent[20:]))
	require.NoError(t, s.Hash([]byte(streamContent[20:])))
	cp = readCheckpointOffset(t, checkpointPath)
	assert.Equal(t, int64(len(streamContent)), cp, "the tail checkpoints past the last boundary")
	require.NoError(t, s.Verify(dest))
}

func readCheckpointOffset(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var cp checkpoint
	require.NoError(t, json.Unmarshal(b, &cp))

	return cp.Offset
}
