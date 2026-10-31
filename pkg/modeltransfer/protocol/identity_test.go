package protocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckContractVersion(t *testing.T) {
	cases := []struct {
		name    string
		version int
		ok      bool
	}{
		{"supported two", 2, true},
		{"old one", 1, false},
		{"zero", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckContractVersion(tc.version)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, KindContractVersion, KindOf(err))
		})
	}
}

func TestContractIdentity(t *testing.T) {
	assert.Equal(t, "gpustack-model-transfer/reader/2", ContractIdentity())
}

func TestArtifactIdentityValidate(t *testing.T) {
	valid := ArtifactIdentity{
		ArtifactUID:      "model-fixture-tiny-dense",
		ManifestSHA256:   repeatHex("a"),
		SourceGeneration: 1,
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name     string
		mutate   func(*ArtifactIdentity)
		wantKind Kind
	}{
		{"empty uid", func(i *ArtifactIdentity) { i.ArtifactUID = "" }, KindIdentity},
		{"uid with space", func(i *ArtifactIdentity) { i.ArtifactUID = "has space" }, KindIdentity},
		{"uid with slash", func(i *ArtifactIdentity) { i.ArtifactUID = "a/b" }, KindIdentity},
		{"short digest", func(i *ArtifactIdentity) { i.ManifestSHA256 = "abcd" }, KindIdentity},
		{"non-hex digest", func(i *ArtifactIdentity) { i.ManifestSHA256 = repeatHex("z") }, KindIdentity},
		{"zero generation", func(i *ArtifactIdentity) { i.SourceGeneration = 0 }, KindIdentity},
		{"negative generation", func(i *ArtifactIdentity) { i.SourceGeneration = -1 }, KindIdentity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := valid
			tc.mutate(&candidate)
			err := candidate.Validate()
			require.Error(t, err)
			assert.Equal(t, tc.wantKind, KindOf(err))
		})
	}
}

func TestArtifactIdentityMatches(t *testing.T) {
	base := ArtifactIdentity{
		ArtifactUID:      "mf",
		ManifestSHA256:   repeatHex("a"),
		SourceGeneration: 3,
	}
	require.NoError(t, base.Matches(base))

	uidDiff := base
	uidDiff.ArtifactUID = "other"
	assert.Equal(t, KindAuthentication, KindOf(base.Matches(uidDiff)))

	digestDiff := base
	digestDiff.ManifestSHA256 = repeatHex("b")
	assert.Equal(t, KindAuthentication, KindOf(base.Matches(digestDiff)))

	stale := base
	stale.SourceGeneration = 4
	assert.Equal(t, KindStaleGeneration, KindOf(base.Matches(stale)))
}

func TestKindOfUnwrapsWrappedErrors(t *testing.T) {
	inner := New(KindSourceUnavailable, "read", "short read")
	assert.Equal(t, KindSourceUnavailable, KindOf(inner))
	assert.Equal(t, Kind("internal"), KindOf(assert.AnError))
}

func repeatHex(char string) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = char[0]
	}
	return string(out)
}
