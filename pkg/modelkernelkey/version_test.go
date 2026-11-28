package modelkernelkey

import "testing"

func TestNormalizeEngine(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "vLLM API spelling", input: "vLLM", want: EngineVLLM},
		{name: "vllm lowercase", input: "vllm", want: EngineVLLM},
		{name: "SGLang API spelling", input: "SGLang", want: EngineSGLang},
		{name: "sglang lowercase with spaces", input: " sglang ", want: EngineSGLang},
		{name: "empty", input: "", wantErr: true},
		{name: "unsupported engine", input: "mindie", wantErr: true},
		{name: "vllm-ascend package name", input: "vllm-ascend", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeEngine(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NormalizeEngine(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("NormalizeEngine(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizeEngineVersion(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "patch 0.8.5 collapses to 0.8", input: "0.8.5", want: "0.8"},
		{name: "patch 0.8.6 collapses to 0.8", input: "0.8.6", want: "0.8"},
		{name: "bare major.minor 0.8", input: "0.8", want: "0.8"},
		{name: "v prefix and cuda build metadata", input: "v0.8.5+cu128", want: "0.8"},
		{name: "uppercase V prefix", input: "V0.34.0", want: "0.34"},
		{name: "git describe distance and sha", input: "0.8.5-12-g1a2b3c4", want: "0.8"},
		{name: "post release tag stripped", input: "0.4.6.post2", want: "0.4"},
		{name: "post release with rocm metadata stripped", input: "0.5.15.post1+rocm6.3", want: "0.5"},
		{name: "minor bump 0.9.0", input: "0.9.0", want: "0.9"},
		{name: "pep440 rc1 preserved", input: "0.9.0rc1", want: "0.9rc1"},
		{name: "semver rc.1 preserved", input: "0.9.0-rc.1", want: "0.9-rc.1"},
		{name: "dotted rc1 with metadata preserved", input: "0.30.0.rc1+rocm6.3", want: "0.30.rc1"},
		{name: "dev0 preserved", input: "0.9.0.dev0", want: "0.9.dev0"},
		{name: "dev0 with git metadata preserved", input: "0.30.0.dev0+g1a2b3c4", want: "0.30.dev0"},
		{name: "alpha a1 preserved", input: "0.9.0a1", want: "0.9a1"},
		{name: "beta b2 preserved", input: "0.9.0b2", want: "0.9b2"},
		{name: "already canonical prerelease idempotent", input: "0.9rc1", want: "0.9rc1"},
		{name: "strip hyphen git commit suffix", input: "0.11.0-g1a2b3c4", want: "0.11"},
		{name: "strip git describe and dirty", input: "0.11.0-12-g1a2b3c4-dirty", want: "0.11"},
		{name: "strip standalone dirty", input: "0.8.5-dirty", want: "0.8"},
		{name: "strip dotted git commit suffix", input: "0.11.0.g1a2b3c4", want: "0.11"},
		{name: "four-segment micro version", input: "0.10.1.1+cu126", want: "0.10"},
		{name: "empty", input: "", wantErr: true},
		{name: "whitespace only", input: "   ", wantErr: true},
		{name: "internal whitespace", input: "0.8 .5", wantErr: true},
		{name: "non-numeric tag", input: "latest", wantErr: true},
		{name: "single number", input: "1", wantErr: true},
		{name: "slash in version", input: "0.8.5/evil", wantErr: true},
		{name: "trailing dot", input: "0.34.", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeEngineVersion(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NormalizeEngineVersion(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("NormalizeEngineVersion(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
