package download

import (
	"crypto/sha1" // nolint: gosec // the git blob id of a non-LFS file is a SHA-1 by git's definition.
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"gpustack.ai/gpustack/pkg/modelartifact"
)

// verifier hashes one file in byte order and says whether it matches its manifest line.
type verifier struct {
	algorithm string
	want      string
	size      int64
	h         hash.Hash
	// offset is how many content bytes have been hashed.
	offset int64
}

func newVerifier(digest string, size int64) (*verifier, error) {
	algorithm, want, ok := strings.Cut(digest, ":")
	if !ok {
		return nil, errorf(ReasonIntegrityMismatch, "digest %q has no algorithm", digest)
	}
	v := &verifier{algorithm: algorithm, want: want, size: size}
	switch algorithm {
	case modelartifact.DigestSHA256:
		v.h = sha256.New()
	case modelartifact.DigestGitSHA1:
		// A git blob id hashes a header naming the size before the content.
		v.h = sha1.New() // nolint: gosec
		_, _ = v.h.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	default:
		return nil, errorf(ReasonIntegrityMismatch, "digest %q has an unknown algorithm", digest)
	}

	return v, nil
}

func (v *verifier) Write(p []byte) (int, error) {
	n, err := v.h.Write(p)
	v.offset += int64(n)

	return n, err
}

// check reports a mismatch of the size or the hash.
func (v *verifier) check(path string) error {
	if v.offset != v.size {
		return errorf(ReasonIntegrityMismatch, "%s: received %d bytes, the manifest says %d", path, v.offset, v.size)
	}
	if got := hex.EncodeToString(v.h.Sum(nil)); got != v.want {
		return errorf(ReasonIntegrityMismatch, "%s: the content hashes to %s:%s, the manifest says %s:%s",
			path, v.algorithm, got, v.algorithm, v.want)
	}

	return nil
}

// ResumableOffset is what a download of a file at dest with its checkpoint at path carries over
// from a previous attempt: the checkpoint's offset while it is valid for the file on disk, else 0.
// It decides as resume does, without touching the file, so a caller counts the carried-over bytes
// before the download starts.
func ResumableOffset(dest, path string, size int64) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var cp checkpoint
	if json.Unmarshal(b, &cp) != nil || cp.Offset < 0 || cp.Offset > size {
		return 0
	}
	if info, err := os.Stat(dest); err != nil || info.Size() < cp.Offset {
		return 0
	}

	return cp.Offset
}

// checkpoint is what a resume starts from: the bytes before Offset were hashed, and synced to disk
// before this was written, so the hash state covers bytes that are really there.
type checkpoint struct {
	Offset int64  `json:"offset"`
	State  []byte `json:"state"`
}

// saveCheckpoint records the hashed position at path, which lies outside the tree being published.
// The checkpoint itself is not synced: a machine crash may leave the previous one or none, and the
// file then resumes from further back or from the start, losing progress but never trusting a byte
// the disk may not hold.
func (v *verifier) saveCheckpoint(path string) error {
	state, err := v.h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	b, err := json.Marshal(checkpoint{Offset: v.offset, State: state})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

// resume returns where the download into f continues from, given its checkpoint at path. It trusts only a checkpoint: bytes a
// parallel download wrote beyond the hashed position may have holes before them, so they are
// truncated away. A checkpoint whose hash state this build cannot restore re-hashes the checkpointed
// bytes from disk instead; a file without a checkpoint starts over.
func (v *verifier) resume(f *os.File, path string) (int64, error) {
	var cp checkpoint
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, f.Truncate(0)
	case err != nil:
		return 0, err
	}
	if json.Unmarshal(b, &cp) != nil || cp.Offset < 0 || cp.Offset > v.size {
		return 0, f.Truncate(0)
	}
	if info, err := f.Stat(); err != nil || info.Size() < cp.Offset {
		return 0, f.Truncate(0)
	}
	if err := f.Truncate(cp.Offset); err != nil {
		return 0, err
	}

	if err := v.h.(encoding.BinaryUnmarshaler).UnmarshalBinary(cp.State); err == nil {
		v.offset = cp.Offset
		return cp.Offset, nil
	}
	fresh, err := newVerifier(v.algorithm+":"+v.want, v.size)
	if err != nil {
		return 0, err
	}
	*v = *fresh
	if _, err := io.Copy(v, io.NewSectionReader(f, 0, cp.Offset)); err != nil {
		return 0, fmt.Errorf("re-hash the resumed part of %s: %w", f.Name(), err)
	}

	return cp.Offset, nil
}

func removeCheckpoint(path string) {
	_ = os.Remove(path)
}
