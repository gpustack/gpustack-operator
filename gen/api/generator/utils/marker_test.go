package utils

import "testing"

func Test_indexJSON(t *testing.T) {
	testcases := []struct {
		given    string
		expected string
	}{
		{
			given:    `{"a": "b"}`,
			expected: `{"a": "b"}`,
		},
		{
			given:    `[{"x": "y"}, ["a", 1, true]]`,
			expected: `[{"x": "y"}, ["a", 1, true]]`,
		},
		{
			given:    `"a"`,
			expected: `"a"`,
		},
		{
			given:    `["\"a\"", "b", "\\]"]`,
			expected: `["\"a\"", "b", "\\]"]`,
		},
		{
			given:    `["\"a\"", "b", "\\"]"]]]]]`,
			expected: `["\"a\"", "b", "\\"]`,
		},
		{
			given:    `["\"a\"", "b", "\"]"]]]]]`,
			expected: `["\"a\"", "b", "\"]"]`,
		},
		{
			given:    `{"a":{"b":["x",2,true]},"c":["d",1,false]}`,
			expected: `{"a":{"b":["x",2,true]},"c":["d",1,false]}`,
		},
		{
			given:    `{"a":{"b":["x",2,true]}},"c":["d",1,false]}`,
			expected: `{"a":{"b":["x",2,true]}}`,
		},
		{
			given:    `"{{}}}}"`,
			expected: `"{{}}}}"`,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.given, func(t *testing.T) {
			got := indexJSON(tc.given)
			if got == -1 {
				if tc.expected != "" {
					t.Fatalf("unexpected -1")
				}
				return
			}
			if tc.expected != tc.given[:got+1] {
				t.Fatalf("expected %q, got %q", tc.expected, tc.given[:got+1])
			}
		})
	}
}
