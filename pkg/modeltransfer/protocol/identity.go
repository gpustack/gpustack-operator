package protocol

import "regexp"

var uidPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ArtifactIdentity binds one load attempt to an artifact UID, the sha256 of
// the exact manifest bytes, and an immutable source generation. A reader
// session opened against any other combination of the three is refused.
type ArtifactIdentity struct {
	ArtifactUID      string `json:"artifact_uid"`
	ManifestSHA256   string `json:"manifest_sha256"`
	SourceGeneration int64  `json:"source_generation"`
}

// Validate checks the identity fields: UID shape, digest shape, and a
// positive generation.
func (i ArtifactIdentity) Validate() error {
	if !uidPattern.MatchString(i.ArtifactUID) {
		return New(KindIdentity, "artifact_uid",
			"invalid artifact uid %q", i.ArtifactUID)
	}
	if !isHex64(i.ManifestSHA256) {
		return New(KindIdentity, "manifest_sha256",
			"must be 64 lowercase hex characters")
	}
	if i.SourceGeneration < 1 {
		return New(KindIdentity, "source_generation",
			"must be positive, got %d", i.SourceGeneration)
	}
	return nil
}

// Matches reports whether other is the same identity. A UID or digest
// difference is an authentication failure; a generation difference is a
// stale generation, so a caller can tell a wrong artifact from a source
// that has moved on.
func (i ArtifactIdentity) Matches(other ArtifactIdentity) error {
	if i.ArtifactUID != other.ArtifactUID || i.ManifestSHA256 != other.ManifestSHA256 {
		return New(KindAuthentication, "identity",
			"artifact uid or manifest digest does not match")
	}
	if i.SourceGeneration != other.SourceGeneration {
		return New(KindStaleGeneration, "identity",
			"identity generation %d, other generation %d",
			i.SourceGeneration, other.SourceGeneration)
	}
	return nil
}

// isHex64 reports whether s is 64 lowercase hexadecimal characters.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
