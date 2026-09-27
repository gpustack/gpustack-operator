package download

import "os"

// The streaming surface a non-hub fetcher shares with the hub downloader's own in-stream
// hashing: bytes are hashed in byte order as they arrive, the hash state is checkpointed at byte
// intervals, and an interrupted download resumes from the last checkpoint instead of starting
// over. Who delivered the bytes is the caller's business.

// StreamHasher hashes one file's bytes in arrival order and checkpoints the hash state, so a
// later attempt of the same file resumes from the checkpointed position.
type StreamHasher struct {
	v              *verifier
	checkpointPath string
	interval       int64
	next           int64
}

// NewStreamHasher starts hashing a file whose manifest line is digest and whose size is size,
// checkpointing to checkpointPath every checkpointBytes of hashed content; checkpointBytes of
// zero or less checkpoints only when Verify says the file is complete.
func NewStreamHasher(digest string, size int64, checkpointPath string, checkpointBytes int64) (*StreamHasher, error) {
	v, err := newVerifier(digest, size)
	if err != nil {
		return nil, err
	}

	return &StreamHasher{v: v, checkpointPath: checkpointPath, interval: checkpointBytes, next: checkpointBytes}, nil
}

// Resume restores what a previous hasher of the same file at dest checkpointed to checkpointPath,
// truncating dest to the hashed position, and returns the offset to fetch from. A checkpoint whose
// hash state this build cannot restore re-hashes the checkpointed bytes from disk; a file without
// a checkpoint starts from zero.
func Resume(digest string, size int64, dest, checkpointPath string, checkpointBytes int64) (*StreamHasher, int64, error) {
	s, err := NewStreamHasher(digest, size, checkpointPath, checkpointBytes)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	offset, err := s.v.resume(f, checkpointPath)
	if err != nil {
		return nil, 0, err
	}
	if s.interval > 0 && offset > 0 {
		s.next = (offset/s.interval + 1) * s.interval
	}

	return s, offset, nil
}

// Hash hashes the next bytes in byte order, checkpointing on the interval's boundaries. The
// caller feeds the bytes in byte order, whoever delivered them; the checkpoint written on a
// boundary covers bytes that are already on disk at their offsets.
func (s *StreamHasher) Hash(p []byte) error {
	if _, err := s.v.Write(p); err != nil {
		return err
	}
	if s.interval <= 0 {
		return nil
	}
	checkpointed := false
	for s.v.offset >= s.next {
		checkpointed = true
		s.next += s.interval
	}
	if !checkpointed {
		return nil
	}

	return s.v.saveCheckpoint(s.checkpointPath)
}

// Written is how many bytes of the file have been hashed.
func (s *StreamHasher) Written() int64 {
	return s.v.offset
}

// Checkpoint records the hashed position now.
func (s *StreamHasher) Checkpoint() error {
	return s.v.saveCheckpoint(s.checkpointPath)
}

// Verify finishes the file at dest: size and digest must match what was hashed, and the final
// checkpoint records the whole file, so a later attempt of the same file skips it entirely.
func (s *StreamHasher) Verify(dest string) error {
	if err := s.v.check(dest); err != nil {
		return err
	}

	return s.v.saveCheckpoint(s.checkpointPath)
}
