package stringx

import "testing"

func TestCompareNumeric(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{name: "equal", a: "5", b: "5", want: 0},
		{name: "less by value", a: "4", b: "5", want: -1},
		{name: "greater by value", a: "9", b: "5", want: 1},
		{name: "less by length", a: "9", b: "10", want: -1},
		{name: "greater by length", a: "100", b: "99", want: 1},
		{name: "leading zeros equal", a: "007", b: "7", want: 0},
		{name: "leading zeros compare", a: "010", b: "9", want: 1},
		{name: "empty is zero", a: "", b: "0", want: 0},
		{name: "empty less than positive", a: "", b: "1", want: -1},
		{name: "zero variants equal", a: "00", b: "0", want: 0},
		{name: "large values", a: "1600000", b: "160000", want: 1},
		{name: "non-digit is zero", a: "12Gi", b: "0", want: 0},
		{name: "non-digit less than positive", a: "x", b: "3", want: -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CompareNumeric(c.a, c.b); got != c.want {
				t.Fatalf("CompareNumeric(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
			}
		})
	}
}
