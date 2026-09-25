package modelartifact

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFilterMatchesPythonFnmatch checks MatchPattern against CPython itself:
// testdata/filter/fnmatch_cases.py recorded what fnmatch.fnmatchcase answers for every pair.
func TestFilterMatchesPythonFnmatch(t *testing.T) {
	raw, err := os.ReadFile("testdata/filter/fnmatch_cases.json")
	require.NoError(t, err)
	var fixture struct {
		Python string `json:"python"`
		Cases  []struct {
			Pattern string `json:"pattern"`
			Name    string `json:"name"`
			Match   bool   `json:"match"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Cases)

	matched := 0
	for _, c := range fixture.Cases {
		if c.Match {
			matched++
		}
		assert.Equalf(t, c.Match, MatchPattern(c.Pattern, c.Name),
			"fnmatchcase(%q, %q) is %v on Python %s", c.Name, c.Pattern, c.Match, fixture.Python)
	}
	// A fixture of only misses or only hits would pass a matcher that always answers one way.
	assert.Positive(t, matched)
	assert.Less(t, matched, len(fixture.Cases))
}

func TestFilterEntries(t *testing.T) {
	entries := []ManifestEntry{
		manifestEntry(".gitattributes", 1, testGitSHA1),
		manifestEntry("config.json", 1, testGitSHA1),
		manifestEntry("model.safetensors", 3, testSHA256),
		manifestEntry("Model.SAFETENSORS", 3, testSHA256),
		manifestEntry("original/consolidated.00.pth", 5, testSHA256),
		manifestEntry("original/params.json", 1, testGitSHA1),
		manifestEntry("sub/original/x.bin", 2, testSHA256),
	}
	cases := []struct {
		name   string
		allow  []string
		ignore []string
		want   []string
	}{
		{
			name: "no pattern keeps every entry",
			want: []string{
				".gitattributes", "config.json", "model.safetensors", "Model.SAFETENSORS",
				"original/consolidated.00.pth", "original/params.json", "sub/original/x.bin",
			},
		},
		{
			name:  "a star crosses slashes, so *.json selects nested files too",
			allow: []string{"*.json"},
			want:  []string{"config.json", "original/params.json"},
		},
		{
			name:  "** is a star like any other",
			allow: []string{"**"},
			want: []string{
				".gitattributes", "config.json", "model.safetensors", "Model.SAFETENSORS",
				"original/consolidated.00.pth", "original/params.json", "sub/original/x.bin",
			},
		},
		{
			name:   "original/* ignores the directory at the root only",
			ignore: []string{"original/*"},
			want: []string{
				".gitattributes", "config.json", "model.safetensors", "Model.SAFETENSORS",
				"sub/original/x.bin",
			},
		},
		{
			name:  "a trailing slash on an allow pattern is the directory's contents",
			allow: []string{"original/"},
			want:  []string{"original/consolidated.00.pth", "original/params.json"},
		},
		{
			name:   "a trailing slash on an ignore pattern is the directory's contents",
			ignore: []string{"original/"},
			want: []string{
				".gitattributes", "config.json", "model.safetensors", "Model.SAFETENSORS",
				"sub/original/x.bin",
			},
		},
		{
			name:  "matching is case-sensitive on the allow side",
			allow: []string{"*.safetensors"},
			want:  []string{"model.safetensors"},
		},
		{
			name:   "matching is case-sensitive on the ignore side",
			ignore: []string{"*.SAFETENSORS"},
			want: []string{
				".gitattributes", "config.json", "model.safetensors",
				"original/consolidated.00.pth", "original/params.json", "sub/original/x.bin",
			},
		},
		{
			name:   "ignore wins over allow",
			allow:  []string{"*.json", "*.safetensors"},
			ignore: []string{"original/"},
			want:   []string{"config.json", "model.safetensors"},
		},
		{
			name:  "a pattern that matches nothing keeps nothing",
			allow: []string{"*.gguf"},
			want:  []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := []string{}
			for _, e := range FilterEntries(entries, c.allow, c.ignore) {
				got = append(got, e.Path)
			}
			assert.Equal(t, c.want, got)
		})
	}
}

// TestFilterDigestCrossImplementation checks a filtered manifest against the reference script run
// with the same patterns, and that no pattern leaves the ModelArtifact spec's recorded digest and
// encoding exactly as they were.
func TestFilterDigestCrossImplementation(t *testing.T) {
	entries := loadTreeEntries(t, "testdata/manifest/qwen2.5-0.5b-instruct-7ae55760.tree.json")
	unfiltered, err := NewManifest(entries)
	require.NoError(t, err)

	cases := []struct {
		name      string
		allow     []string
		ignore    []string
		digest    string
		fileCount int64
		same      bool
	}{
		{
			name:      "no pattern is the unfiltered manifest, byte for byte",
			digest:    "testdata/manifest/qwen2.5-0.5b-instruct-7ae55760.digest",
			fileCount: 10,
			same:      true,
		},
		{
			name:      "allow json and safetensors, ignore the tokenizer files",
			allow:     []string{"*.json", "*.safetensors"},
			ignore:    []string{"tokenizer*"},
			digest:    "testdata/manifest/qwen2.5-0.5b-instruct-7ae55760.filtered.digest",
			fileCount: 4,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantDigest, err := os.ReadFile(c.digest)
			require.NoError(t, err)

			got, err := NewManifest(FilterEntries(entries, c.allow, c.ignore))
			require.NoError(t, err)
			assert.Equal(t, strings.TrimSpace(string(wantDigest)), got.Digest)
			assert.Equal(t, c.fileCount, got.FileCount)
			if c.same {
				assert.Equal(t, unfiltered.Encoded, got.Encoded)
			} else {
				assert.NotEqual(t, unfiltered.Digest, got.Digest)
			}
		})
	}
}

// loadTreeEntries reads a recorded Hugging Face tree listing into manifest entries.
func loadTreeEntries(t *testing.T, path string) []ManifestEntry {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var tree []struct {
		Type string `json:"type"`
		OID  string `json:"oid"`
		Size int64  `json:"size"`
		Path string `json:"path"`
		LFS  *struct {
			OID string `json:"oid"`
		} `json:"lfs"`
	}
	require.NoError(t, json.Unmarshal(raw, &tree))

	var entries []ManifestEntry
	for _, e := range tree {
		if e.Type != "file" {
			continue
		}
		digest := DigestGitSHA1 + ":" + e.OID
		if e.LFS != nil {
			digest = DigestSHA256 + ":" + e.LFS.OID
		}
		entries = append(entries, manifestEntry(e.Path, e.Size, digest))
	}

	return entries
}
