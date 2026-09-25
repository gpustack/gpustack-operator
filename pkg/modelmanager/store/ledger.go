package store

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	klog "k8s.io/klog/v2"
)

// The ledger lives on the node's disk only. It is the node's own record, never an API object, so it
// may name the Pod a reference belongs to; it never holds a credential.

// Ref records that a target path is bind-mounted from a digest's published tree.
type Ref struct {
	// VolumeID is the ID kubelet gave the volume.
	VolumeID string `json:"volumeID"`
	// TargetPath is where kubelet asked the volume to be mounted.
	TargetPath string `json:"targetPath"`
	// Hex is the digest's hexadecimal part.
	Hex string `json:"hex"`
	// PodNamespace, PodName and PodUID are the Pod kubelet mounted it for, when known.
	PodNamespace string `json:"podNamespace,omitempty"`
	PodName      string `json:"podName,omitempty"`
	PodUID       string `json:"podUID,omitempty"`
}

var volumeIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (s *Store) refPath(volumeID string) (string, error) {
	if !volumeIDPattern.MatchString(volumeID) {
		return "", fmt.Errorf("volume ID %q is not a file name", volumeID)
	}

	return filepath.Join(s.root, ledgerDir, refsDir, volumeID+".json"), nil
}

// WriteRef records r, replacing an earlier record of the same volume.
func (s *Store) WriteRef(r Ref) error {
	path, err := s.refPath(r.VolumeID)
	if err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}

	return writeFileSync(path, b)
}

// Claim records r, a reference to r.Hex's published tree, if that tree is published, and reports
// whether it is. It holds the lock RemoveUnreferenced does, so the tree a claim finds stays until the
// reference is removed. A mount claims before it binds: a reference that cannot be written fails the
// mount instead of leaving a mounted tree the collector cannot see.
func (s *Store) Claim(r Ref) (bool, error) {
	s.claimMu.Lock()
	defer s.claimMu.Unlock()

	if !s.IsPublished(r.Hex) {
		return false, nil
	}
	if err := s.WriteRef(r); err != nil {
		return false, err
	}

	return true, nil
}

// ReadRef reads the record of volumeID, reporting false when there is none.
func (s *Store) ReadRef(volumeID string) (Ref, bool, error) {
	var r Ref
	path, err := s.refPath(volumeID)
	if err != nil {
		return r, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, false, err
	}

	return r, true, nil
}

// RemoveRef deletes the record of volumeID; a missing record is not an error.
func (s *Store) RemoveRef(volumeID string) error {
	path, err := s.refPath(volumeID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// Refs returns every reference record, by target path.
func (s *Store) Refs() ([]Ref, error) {
	paths, err := filepath.Glob(filepath.Join(s.root, ledgerDir, refsDir, "*.json"))
	if err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			// Unmounted since the listing: the reference is gone, which is what it now says.
			continue
		}
		if err != nil {
			return nil, err
		}
		var r Ref
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		refs = append(refs, r)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].TargetPath < refs[j].TargetPath })

	return refs, nil
}

// DigestRecord is the ledger's per-digest record.
type DigestRecord struct {
	// LastAttempt is the highest attempt number handed out, so a number is never reused.
	LastAttempt int64 `json:"lastAttempt,omitempty"`
	// LastUsedTime is when a mount or an unmount of the digest last happened.
	LastUsedTime time.Time `json:"lastUsedTime,omitzero"`
	// Failures counts consecutive failed attempts; a success resets it.
	Failures int `json:"failures,omitempty"`
	// Reason and Message describe the last failure; RetryTime is the earliest next attempt.
	Reason    string    `json:"reason,omitempty"`
	Message   string    `json:"message,omitempty"`
	RetryTime time.Time `json:"retryTime,omitzero"`
}

func (s *Store) digestPath(hex string) (string, error) {
	if !hexDigestPattern.MatchString(hex) {
		return "", fmt.Errorf("digest %q is not 64 hexadecimal digits", hex)
	}

	return filepath.Join(s.root, ledgerDir, digestsDir, hex+".json"), nil
}

// ReadDigest reads hex's record; a digest without one has the zero record.
func (s *Store) ReadDigest(hex string) (DigestRecord, error) {
	var rec DigestRecord
	path, err := s.digestPath(hex)
	if err != nil {
		return rec, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	err = json.Unmarshal(b, &rec)

	return rec, err
}

// WriteDigest writes hex's record.
func (s *Store) WriteDigest(hex string, rec DigestRecord) error {
	path, err := s.digestPath(hex)
	if err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	return writeFileSync(path, b)
}

// UpdateDigest applies fn to hex's record and writes it back. Every update holds one lock, so a
// mount's use and an attempt's outcome, which read and write the same record, merge rather than one
// overwriting the other with what it read before.
func (s *Store) UpdateDigest(hex string, fn func(*DigestRecord)) error {
	s.digestMu.Lock()
	defer s.digestMu.Unlock()

	rec, err := s.ReadDigest(hex)
	if err != nil {
		return err
	}
	fn(&rec)

	return s.WriteDigest(hex, rec)
}

// Digests returns every digest that has a record.
func (s *Store) Digests() (map[string]DigestRecord, error) {
	paths, err := filepath.Glob(filepath.Join(s.root, ledgerDir, digestsDir, "*.json"))
	if err != nil {
		return nil, err
	}
	recs := make(map[string]DigestRecord, len(paths))
	for _, p := range paths {
		hex := filepath.Base(p[:len(p)-len(".json")])
		if !hexDigestPattern.MatchString(hex) {
			// Not a record this store wrote: one stray file must not fail every report of the node.
			klog.V(1).InfoS("skip a file that is not a digest record", "path", p)
			continue
		}
		rec, err := s.ReadDigest(hex)
		if err != nil {
			return nil, err
		}
		recs[hex] = rec
	}

	return recs, nil
}

// RemoveDigest removes hex's record, once its content is gone.
func (s *Store) RemoveDigest(hex string) error {
	path, err := s.digestPath(hex)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// EphemeralVolumeID is the volume ID kubelet gives a CSI inline volume: "csi-" and the hexadecimal
// SHA-256 of the Pod's UID followed by the volume's name. It is kubelet's formula, not the CSI
// contract, so it is used only to name a reference adopted from a mount the ledger lost; nothing
// depends on recomputing it.
func EphemeralVolumeID(podUID, volumeName string) string {
	return fmt.Sprintf("csi-%x", sha256.Sum256([]byte(podUID+volumeName)))
}
