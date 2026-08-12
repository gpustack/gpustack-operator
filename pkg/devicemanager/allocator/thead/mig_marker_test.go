package thead

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyMarkerKeys is the complete on-disk key set of the ownership marker, as records written by
// earlier releases carry it. It is data, not a restatement of the struct: the point of the test
// below is to fail when a tag moves, which a set derived from the struct itself could never do.
var legacyMarkerKeys = []string{
	"podUID", "container", "card", "profile", "profileID",
	"giID", "ciID", "migUUID", "computeSlices", "start", "length",
}

// TestLegacyMarkerRoundTrip pins the ownership marker's on-disk format against a literal document,
// because that format is not this repository's to rename: markers written before this vocabulary
// change are still on real nodes, and an unreadable one breaks retry, visibility, adoption and
// reclamation.
//
// It is deliberately not written through migMarker. Every other marker test marshals and unmarshals
// the same struct, so a tag renamed on both ends of that round trip stays green while every
// pre-upgrade record on a node becomes unreadable. Feeding a literal document in and inspecting the
// literal keys on the way out is what closes that gap.
func TestLegacyMarkerRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want migMarker
	}{
		{
			name: "a full pre-refactor record",
			doc: `{
  "podUID": "11111111-2222-3333-4444-555555555555",
  "container": "worker",
  "card": "` + testPPUUUID0 + `",
  "profile": "1c.10g",
  "profileID": 3,
  "giID": 7,
  "ciID": 1,
  "migUUID": "MIG-99999999-8888-7777-6666-555555555555",
  "computeSlices": 1,
  "start": 2,
  "length": 2
}`,
			want: migMarker{
				PodUID:        "11111111-2222-3333-4444-555555555555",
				Container:     "worker",
				Card:          testPPUUUID0,
				Profile:       "1c.10g",
				ProfileID:     3,
				GiID:          7,
				CiID:          1,
				MigUUID:       "MIG-99999999-8888-7777-6666-555555555555",
				ComputeSlices: 1,
				Start:         2,
				Length:        2,
			},
		},
		{
			// The vendor numbering makes 0 a legal profile id and 0 a legal instance id, so a record
			// carrying them must survive as itself rather than as an absent field.
			name: "a record whose profile and instance ids are the legal zero",
			doc: `{
  "podUID": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
  "container": "sidecar",
  "card": "` + testPPUUUID1 + `",
  "profile": "2c.20g",
  "profileID": 0,
  "giID": 0,
  "ciID": 0,
  "migUUID": "MIG-00000000-0000-0000-0000-000000000000",
  "computeSlices": 2,
  "start": 0,
  "length": 4
}`,
			want: migMarker{
				PodUID:        "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
				Container:     "sidecar",
				Card:          testPPUUUID1,
				Profile:       "2c.20g",
				ProfileID:     0,
				GiID:          0,
				CiID:          0,
				MigUUID:       "MIG-00000000-0000-0000-0000-000000000000",
				ComputeSlices: 2,
				Start:         0,
				Length:        4,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := markerPath(t.TempDir(), tc.want.PodUID, tc.want.Container, tc.want.Card)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(tc.doc), 0o600))

			got, err := parseMarker(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			data, err := json.Marshal(got)
			require.NoError(t, err)
			var written map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &written))

			keys := make([]string, 0, len(written))
			for k := range written {
				keys = append(keys, k)
			}
			assert.ElementsMatch(t, legacyMarkerKeys, keys)
			assert.Contains(t, written, "card")
			assert.NotContains(t, written, "accelerator")
		})
	}
}
