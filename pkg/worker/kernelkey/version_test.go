package kernelkey

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
		{name: "standard semver", input: "0.34.0", want: "0.34.0"},
		{name: "leading v prefix", input: "v0.34.0", want: "0.34.0"},
		{name: "major.minor padded to patch", input: "0.34", want: "0.34.0"},
		{name: "strip cuda build metadata", input: "0.11.0+cu128", want: "0.11.0"},
		{name: "strip rocm build metadata", input: "0.11.0+rocm6.3", want: "0.11.0"},
		{name: "strip build metadata with git sha", input: "0.11.0+cu128.g1a2b3c4", want: "0.11.0"},
		{name: "strip hyphen git commit suffix", input: "0.11.0-g1a2b3c4", want: "0.11.0"},
		{name: "strip git describe distance and dirty", input: "0.11.0-12-g1a2b3c4-dirty", want: "0.11.0"},
		{name: "strip dotted git commit suffix", input: "0.11.0.g1a2b3c4", want: "0.11.0"},
		{name: "preserve pep440 rc tag", input: "0.30.0rc1+cu128", want: "0.30.0rc1"},
		{name: "preserve dotted rc tag", input: "0.30.0.rc1+rocm6.3", want: "0.30.0.rc1"},
		{name: "preserve semver prerelease tag", input: "0.30.0-rc.1+cu128", want: "0.30.0-rc.1"},
		{name: "preserve post tag", input: "0.5.15.post1", want: "0.5.15.post1"},
		{name: "preserve dev tag without git metadata", input: "0.30.0.dev0+g1a2b3c4", want: "0.30.0.dev0"},
		{name: "four-segment version", input: "0.10.1.1+cu126", want: "0.10.1.1"},
		{name: "empty", input: "", wantErr: true},
		{name: "whitespace only", input: "   ", wantErr: true},
		{name: "non-numeric tag", input: "latest", wantErr: true},
		{name: "single number", input: "1", wantErr: true},
		{name: "slash in version", input: "0.34.0/evil", wantErr: true},
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
