package stringx

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name  string
		given string
		limit int
		tail  string
		want  string
	}{
		{
			name:  "shorter than the limit is returned whole",
			given: "abc",
			limit: 8,
			want:  "abc",
		},
		{
			name:  "exactly the limit is not a cut",
			given: "abcd",
			limit: 4,
			tail:  "…",
			want:  "abcd",
		},
		{
			name:  "longer than the limit is cut and told",
			given: "abcdef",
			limit: 3,
			tail:  "…",
			want:  "abc…",
		},
		{
			// A byte-wise cut here would split the third character and produce invalid UTF-8.
			name:  "a multi-byte character is never split",
			given: "日本語です",
			limit: 3,
			tail:  "…",
			want:  "日本語…",
		},
		{
			name:  "an empty limit keeps nothing",
			given: "abc",
			limit: 0,
			tail:  "…",
			want:  "…",
		},
		{
			name:  "a negative limit is a request to leave it alone",
			given: "abc",
			limit: -1,
			want:  "abc",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, TruncateRunes(c.given, c.limit, c.tail))
		})
	}
}

// TestTruncateRunes_BoundsWhatTruncateDoesNot is the reason this function exists next to Truncate.
//
// Truncate measures display cells, and a combining mark, a zero-width space and a control character
// all measure zero — so a string built from them has width 0 however long it is, and Truncate hands
// it back whole. A limit expressed in characters, such as a schema's maxLength, is not a limit on
// cells, and using the wrong one leaves the bound entirely absent for exactly the inputs an
// adversary picks.
func TestTruncateRunes_BoundsWhatTruncateDoesNot(t *testing.T) {
	// Escaped rather than written literally: each of these is invisible in a source file, which is
	// the same property that makes them worth a test.
	for _, c := range []struct{ name, glyph string }{
		{"zero-width space", "\u200b"},
		{"combining acute", "\u0301"},
		{"control character", "\x01"},
	} {
		t.Run(c.name, func(t *testing.T) {
			given := strings.Repeat(c.glyph, 10000)

			assert.Equal(t, given, Truncate(given, 64, "…"),
				"the width-based one has nothing to measure and cuts nothing")
			assert.Equal(t, 64+1, utf8.RuneCountInString(TruncateRunes(given, 64, "…")),
				"and this one bounds it in the unit the caller asked for")
		})
	}
}
