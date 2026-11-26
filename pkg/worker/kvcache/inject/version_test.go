package inject

import "testing"

func TestParseRunnerEngineVersion(t *testing.T) {
	cases := []struct {
		name        string
		image       string
		engineName  string
		wantVersion string
		wantOK      bool
	}{
		{
			name:        "cuda vllm release",
			image:       "gpustack/runner:cuda13.0-vllm0.34.0",
			engineName:  "vLLM",
			wantVersion: "0.34.0",
			wantOK:      true,
		},
		{
			name:        "cann variant vllm release",
			image:       "gpustack/runner:cann9.2-a3-vllm0.32.0",
			engineName:  "vLLM",
			wantVersion: "0.32.0",
			wantOK:      true,
		},
		{
			name:        "cann variant vllm-ascend engine spelling",
			image:       "gpustack/runner:cann9.2-910b-vllm0.30.0rc1",
			engineName:  string(EngineVLLMAscend),
			wantVersion: "0.30.0rc1",
			wantOK:      true,
		},
		{
			name:        "dotted rc pre-release",
			image:       "gpustack/runner:cann9.2-a3-vllm0.30.0.rc1",
			engineName:  "vllm",
			wantVersion: "0.30.0.rc1",
			wantOK:      true,
		},
		{
			name:        "sglang post release",
			image:       "gpustack/runner:cuda13.0-sglang0.5.15.post1",
			engineName:  "SGLang",
			wantVersion: "0.5.15.post1",
			wantOK:      true,
		},
		{
			name:        "four-segment vllm release",
			image:       "gpustack/runner:cuda12.8-vllm0.10.1.1",
			engineName:  "vLLM",
			wantVersion: "0.10.1.1",
			wantOK:      true,
		},
		{
			name:        "registry with port",
			image:       "registry:5000/gpustack/runner:rocm7.0-vllm0.31.0",
			engineName:  "vLLM",
			wantVersion: "0.31.0",
			wantOK:      true,
		},
		{
			name:        "multi-segment registry path",
			image:       "quay.io/org/team/runner:musa4.0-sglang0.5.23",
			engineName:  "SGLang",
			wantVersion: "0.5.23",
			wantOK:      true,
		},
		{
			name:        "bare runner repository",
			image:       "runner:cuda12.8-vllm0.30.0",
			engineName:  "vLLM",
			wantVersion: "0.30.0",
			wantOK:      true,
		},
		{
			name:        "host backend uppercase",
			image:       "gpustack/runner:Host1.0-vllm0.34.0",
			engineName:  "vLLM",
			wantVersion: "0.34.0",
			wantOK:      true,
		},
		{
			name:        "host backend lowercase",
			image:       "gpustack/runner:host1.0-vllm0.34.0",
			engineName:  "vLLM",
			wantVersion: "0.34.0",
			wantOK:      true,
		},
		{
			name:        "tag with digest suffix",
			image:       "gpustack/runner:cuda13.0-vllm0.34.0@sha256:0123456789abcdef",
			engineName:  "vLLM",
			wantVersion: "0.34.0",
			wantOK:      true,
		},
		{
			name:        "tag with trailing build suffix",
			image:       "gpustack/runner:cuda13.0-vllm0.34.0-dev",
			engineName:  "vLLM",
			wantVersion: "0.34.0",
			wantOK:      true,
		},
		{
			name:        "tag with hyphenated trailing build suffix",
			image:       "gpustack/runner:cann9.2-a3-vllm0.32.0-ubuntu22.04-dev",
			engineName:  "vLLM",
			wantVersion: "0.32.0",
			wantOK:      true,
		},
		{
			name:        "other supported backends",
			image:       "gpustack/runner:maca2.30-sglang0.5.9",
			engineName:  "SGLang",
			wantVersion: "0.5.9",
			wantOK:      true,
		},
		{
			name:        "dtk backend",
			image:       "gpustack/runner:dtk25.04-vllm0.29.0",
			engineName:  "vLLM",
			wantVersion: "0.29.0",
			wantOK:      true,
		},
		{
			name:        "corex backend",
			image:       "gpustack/runner:corex4.2-vllm0.31.0",
			engineName:  "vLLM",
			wantVersion: "0.31.0",
			wantOK:      true,
		},
		{
			name:        "hggc backend",
			image:       "gpustack/runner:hggc1.0-vllm0.31.0",
			engineName:  "vLLM",
			wantVersion: "0.31.0",
			wantOK:      true,
		},
		{
			name:       "service disagrees with declared engine vLLM vs sglang",
			image:      "gpustack/runner:cuda13.0-sglang0.5.23",
			engineName: "vLLM",
			wantOK:     false,
		},
		{
			name:       "service disagrees with declared engine SGLang vs vllm",
			image:      "gpustack/runner:cuda13.0-vllm0.34.0",
			engineName: "SGLang",
			wantOK:     false,
		},
		{
			name:       "digest-only image without tag",
			image:      "gpustack/runner@sha256:0123456789abcdef",
			engineName: "vLLM",
			wantOK:     false,
		},
		{
			name:       "non-runner repository",
			image:      "vllm/vllm-openai:v0.34.0",
			engineName: "vLLM",
			wantOK:     false,
		},
		{
			name:       "custom repository containing runner substring",
			image:      "my-reg/my-runner:cuda13.0-vllm0.34.0",
			engineName: "vLLM",
			wantOK:     false,
		},
		{
			name:       "runner image with non-runner tag format",
			image:      "gpustack/runner:v0.1.31",
			engineName: "vLLM",
			wantOK:     false,
		},
		{
			name:       "runner image with non-numeric service version",
			image:      "gpustack/runner:cuda13.0-vllmlatest",
			engineName: "vLLM",
			wantOK:     false,
		},
		{
			name:       "unknown backend",
			image:      "gpustack/runner:tpu1.0-vllm0.34.0",
			engineName: "vLLM",
			wantOK:     false,
		},
		{
			name:       "empty engine name",
			image:      "gpustack/runner:cuda13.0-vllm0.34.0",
			engineName: "",
			wantOK:     false,
		},
		{
			name:       "unknown engine name",
			image:      "gpustack/runner:cuda13.0-mindie2.0.0",
			engineName: "mindie",
			wantOK:     false,
		},
		{
			name:       "empty image",
			image:      "",
			engineName: "vLLM",
			wantOK:     false,
		},
		{
			name:       "leading slash empty prefix",
			image:      "/runner:cuda13.0-vllm0.34.0",
			engineName: "vLLM",
			wantOK:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRunnerEngineVersion(tc.image, tc.engineName)
			if ok != tc.wantOK {
				t.Fatalf("ParseRunnerEngineVersion(%q, %q) ok = %v, want %v (version=%q)",
					tc.image, tc.engineName, ok, tc.wantOK, got)
			}
			if got != tc.wantVersion {
				t.Fatalf("ParseRunnerEngineVersion(%q, %q) = %q, want %q",
					tc.image, tc.engineName, got, tc.wantVersion)
			}
		})
	}
}

func TestValidateEngineVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		wantErr bool
	}{
		{name: "major.minor", version: "0.34", wantErr: false},
		{name: "major.minor.patch", version: "0.34.0", wantErr: false},
		{name: "four-segment release", version: "0.10.1.1", wantErr: false},
		{name: "rc suffix", version: "0.30.0rc1", wantErr: false},
		{name: "dotted rc suffix", version: "0.30.0.rc1", wantErr: false},
		{name: "rc0 suffix", version: "0.34.0rc0", wantErr: false},
		{name: "dotted post suffix", version: "0.5.15.post1", wantErr: false},
		{name: "post suffix", version: "0.5.15post1", wantErr: false},
		{name: "alpha suffix", version: "0.30.0a1", wantErr: false},
		{name: "dotted beta suffix", version: "0.30.0.b2", wantErr: false},
		{name: "dev suffix", version: "0.30.0.dev0", wantErr: false},
		{name: "empty", version: "", wantErr: true},
		{name: "leading v", version: "v0.34.0", wantErr: true},
		{name: "single number", version: "1", wantErr: true},
		{name: "latest", version: "latest", wantErr: true},
		{name: "trailing dot", version: "0.34.", wantErr: true},
		{name: "leading dot", version: ".0.34", wantErr: true},
		{name: "double dot", version: "0..34", wantErr: true},
		{name: "rc without number", version: "0.30.0rc", wantErr: true},
		{name: "hyphenated suffix", version: "0.34.0-rc1", wantErr: true},
		{name: "whitespace", version: " 0.34.0 ", wantErr: true},
		{name: "overflow", version: "99999999999999999999999.0.0", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEngineVersion(tc.version)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateEngineVersion(%q) err = %v, wantErr = %v", tc.version, err, tc.wantErr)
			}
		})
	}
}

func TestCompareEngineVersion(t *testing.T) {
	cases := []struct {
		name    string
		a       string
		b       string
		want    int
		wantErr bool
	}{
		{name: "equal exact", a: "0.30.0", b: "0.30.0", want: 0},
		{name: "missing patch equals zero", a: "0.30", b: "0.30.0", want: 0},
		{name: "rc compares equal to release line", a: "0.30.0rc1", b: "0.30.0", want: 0},
		{name: "dotted rc compares equal to release line", a: "0.30.0.rc1", b: "0.30.0", want: 0},
		{name: "rc0 compares equal to 0.34.0", a: "0.34.0rc0", b: "0.34.0", want: 0},
		{name: "post compares equal to release line", a: "0.5.15.post1", b: "0.5.15", want: 0},
		{name: "minor less", a: "0.29.0", b: "0.30.0", want: -1},
		{name: "minor greater numeric not lexical", a: "0.10.0", b: "0.9.2", want: 1},
		{name: "patch less", a: "0.30.0", b: "0.30.1", want: -1},
		{name: "patch greater", a: "0.5.23", b: "0.5.9", want: 1},
		{name: "major greater", a: "1.0.0", b: "0.99.99", want: 1},
		{name: "invalid a", a: "latest", b: "0.30.0", wantErr: true},
		{name: "invalid b", a: "0.30.0", b: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompareEngineVersion(tc.a, tc.b)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CompareEngineVersion(%q, %q) err = %v, wantErr = %v", tc.a, tc.b, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("CompareEngineVersion(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestEngineVersionAtLeast(t *testing.T) {
	cases := []struct {
		name    string
		version string
		min     string
		want    bool
		wantErr bool
	}{
		{name: "rc satisfies milestone", version: "0.30.0rc1", min: "0.30.0", want: true},
		{name: "dotted rc satisfies milestone", version: "0.30.0.rc1", min: "0.30.0", want: true},
		{name: "rc0 satisfies 0.34.0", version: "0.34.0rc0", min: "0.34.0", want: true},
		{name: "newer satisfies", version: "0.32.0", min: "0.30.0", want: true},
		{name: "older fails", version: "0.29.0", min: "0.30.0", want: false},
		{name: "below vllm-router floor", version: "0.30.0", min: "0.31.0", want: false},
		{name: "at vllm-router floor", version: "0.31.0", min: "0.31.0", want: true},
		{name: "invalid version", version: "custom", min: "0.30.0", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EngineVersionAtLeast(tc.version, tc.min)
			if (err != nil) != tc.wantErr {
				t.Fatalf("EngineVersionAtLeast(%q, %q) err = %v, wantErr = %v", tc.version, tc.min, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("EngineVersionAtLeast(%q, %q) = %v, want %v", tc.version, tc.min, got, tc.want)
			}
		})
	}
}
