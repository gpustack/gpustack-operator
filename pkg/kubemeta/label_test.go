package kubemeta

import (
	"testing"
)

func TestSanitizeLabelValue(t *testing.T) {
	cases := []struct {
		name string
		str  string
		want string
	}{
		{
			name: "empty string",
			str:  "",
			want: "",
		},
		{
			name: "string with only alphanumeric characters",
			str:  "abc123",
			want: "abc123",
		},
		{
			name: "string with non-alphanumeric characters",
			str:  "a!b@c#1$2%3^()[]{}",
			want: "abc123",
		},
		{
			name: "string with spaces",
			str:  "a.b_c-1 2  -  3  .  ",
			want: "a.b_c-1-2-3",
		},
		{
			name: "string with leading and trailing non-alphanumeric characters",
			str:  "-_ .abc123.-_ ",
			want: "abc123",
		},
		{
			name: "string that exceeds maximum length",
			str:  "a.b_c-1 2  -  3  .  xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			want: "a.b_c-1-2-3-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeLabelValue(tc.str); got != tc.want {
				t.Errorf("SanitizeLabelValue(%q) = %q, want %q", tc.str, got, tc.want)
			}
		})
	}
}
