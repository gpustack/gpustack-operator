package priors

import (
	"strings"
	"testing"

	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// TestCanonicalJSONIsPinned asserts the canonical encoding of representative values.
//
// THE GOLDEN BYTES ARE PINNED BECAUSE THE DIGEST IS A STORED VALUE, so the
// encoding must not change with the encoder, the build tag, or the Go version.
// The cases cover the places two JSON encoders are known to disagree: numeric
// precision, HTML and Unicode escaping, and map key order.
func TestCanonicalJSONIsPinned(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "map keys are sorted",
			value: map[string]any{"z": 1, "a": "two"},
			want:  `{"a":"two","z":1}`,
		},
		{
			name:  "html characters are escaped",
			value: map[string]any{"v": "<a>&"},
			want:  `{"v":"\u003ca\u003e\u0026"}`,
		},
		{
			name:  "unicode is kept as it is",
			value: map[string]any{"v": "héllo wörld 日本語"},
			want:  `{"v":"héllo wörld 日本語"}`,
		},
		{
			name:  "a small exponent keeps its digits",
			value: map[string]any{"v": 1e-7},
			want:  `{"v":1e-7}`,
		},
		{
			name:  "a large exponent keeps its digits",
			value: map[string]any{"v": 1e21},
			want:  `{"v":1e+21}`,
		},
		{
			name:  "a whole float loses its fraction",
			value: map[string]any{"v": 2.0},
			want:  `{"v":2}`,
		},
		{
			name:  "a fractional float keeps its digits",
			value: map[string]any{"v": 0.1},
			want:  `{"v":0.1}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := canonicalJSON(testCase.value)
			if err != nil {
				t.Fatalf("encode the content: %v", err)
			}
			if string(encoded) != testCase.want {
				t.Fatalf("the encoding is %q, want %q", encoded, testCase.want)
			}
		})
	}
}

// TestDigestIsPinned asserts the digest of a fixed value.
//
// THE PIN EXISTS BECAUSE EVERY SHIPPED CATALOG STORES THE DIGEST OF ITS
// CONTENT, and a digest that changed would make every shipped catalog
// unreadable.
func TestDigestIsPinned(t *testing.T) {
	digest, err := Digest(map[string]any{"b": "two", "a": 1})
	if err != nil {
		t.Fatalf("compute digest: %v", err)
	}

	const want = "sha256:f15bfc93d70801047473922f67fed863ecc7f82f0677ebb7122923aee81e0f97"
	if digest != want {
		t.Fatalf("digest is %q, want %q", digest, want)
	}
}

// TestDigestUsesTheSharedHashHelper asserts the digest is the shared SHA-256
// helper of the encoded content.
//
// IT IS CHECKED AGAINST THE HELPER, because the digest is the only place the
// scaling change computes a hash, and a helper that formats its result
// differently would change every stored digest.
func TestDigestUsesTheSharedHashHelper(t *testing.T) {
	encoded, err := canonicalJSON(map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("encode the content: %v", err)
	}

	digest, err := Digest(map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("compute digest: %v", err)
	}

	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		t.Fatalf("digest %q does not start with %q", digest, prefix)
	}
	if want := prefix + stringx.SumBytesBySHA256(encoded); digest != want {
		t.Fatalf("digest is %q, want %q", digest, want)
	}
}
