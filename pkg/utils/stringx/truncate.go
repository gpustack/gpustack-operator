package stringx

import "unicode/utf8"

// TruncateRunes bounds s to n runes, appending tail when it had to cut.
//
// It counts runes rather than display cells, which is what Truncate does. The two are not
// interchangeable: a rune of zero width — a combining mark, a zero-width space, a control
// character — costs nothing in cells, so a string built entirely from those has width 0 whatever
// its length, and Truncate returns it whole. Anything bounding a string against a limit expressed
// in characters, such as a Kubernetes schema's maxLength, needs this one.
//
// Cutting on a rune boundary is the other half: a byte slice would split a multi-byte character.
func TruncateRunes(s string, n int, tail string) string {
	if n < 0 || utf8.RuneCountInString(s) <= n {
		return s
	}

	var w int
	for i := range s {
		if w == n {
			return s[:i] + tail
		}
		w++
	}
	return s + tail
}
