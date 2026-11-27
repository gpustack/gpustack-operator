package modelkernelkey

import (
	"math"
	"testing"
)

func TestBuildParseFormatRoundTripAndHashing(t *testing.T) {
	baseSpec := BuildSpec{
		Engine:        "vLLM",
		EngineVersion: "0.34.0+cu128",
		Manufacturer:  "NVIDIA",
		Arch:          "sm_90a",
		OpFamily:      OpFamilyRMSNorm,
		ConstAxes: map[string]int64{
			"hidden_size": 4096,
			"head_dim":    128,
		},
		VarAxes: map[string]int64{
			"batch_size": 16,
			"seq_len":    512,
			"num_tokens": 8192,
		},
		InputDtypes:  []string{"bfloat16", "bfloat16"},
		OutputDtypes: []string{"bfloat16"},
		Constants: map[string]any{
			"eps":       1e-6,
			"is_causal": false,
			"nested": map[string]any{
				"tags": []any{"a", map[string]string{"k": "v"}},
				"list": []string{"x", "y"},
			},
		},
	}

	k1, err := Build(baseSpec)
	if err != nil {
		t.Fatalf("Build(baseSpec) unexpected error: %v", err)
	}
	if len(k1.SignatureHash16) != 16 {
		t.Fatalf("SignatureHash16 len = %d, want 16", len(k1.SignatureHash16))
	}

	formatted, err := k1.Format()
	if err != nil {
		t.Fatalf("Format() unexpected error: %v", err)
	}
	wantPrefix := "vllm/0.34/nvidia/sm_90a/rmsnorm/"
	if formatted != wantPrefix+k1.SignatureHash16 {
		t.Fatalf("Format() = %q, want prefix %q + hash", formatted, wantPrefix)
	}
	if k1.String() != formatted {
		t.Fatalf("String() = %q, want %q", k1.String(), formatted)
	}

	parsed, err := Parse(formatted)
	if err != nil {
		t.Fatalf("Parse(%q) unexpected error: %v", formatted, err)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("parsed.Validate() unexpected error: %v", err)
	}
	reformatted, err := Format(parsed)
	if err != nil {
		t.Fatalf("Format(parsed) unexpected error: %v", err)
	}
	if reformatted != formatted {
		t.Fatalf("round-trip Format(Parse(%q)) = %q", formatted, reformatted)
	}

	t.Run("dynamic var-axis invariance", func(t *testing.T) {
		varied := baseSpec
		varied.VarAxes = map[string]int64{
			"batch_size": 128,
			"seq_len":    4096,
			"num_tokens": 32768,
		}
		// Even if dynamic var axes are accidentally placed in ConstAxes, hashing ignores them.
		varied.ConstAxes = map[string]int64{
			"hidden_size": 4096,
			"head_dim":    128,
			"batch_size":  128,
			"seq_len":     4096,
			"num_tokens":  32768,
		}
		k2, err := Build(varied)
		if err != nil {
			t.Fatalf("Build(varied) unexpected error: %v", err)
		}
		if k2.SignatureHash16 != k1.SignatureHash16 || k2.String() != k1.String() {
			t.Fatalf("var-axis change mutated key: got %q, want %q", k2.String(), k1.String())
		}
	})

	t.Run("TP/EP local shard dimension changes produce distinct keys", func(t *testing.T) {
		tp1Spec := BuildSpec{
			Engine:        "vLLM",
			EngineVersion: "0.34.0",
			Manufacturer:  "nvidia",
			Arch:          "sm_90a",
			OpFamily:      OpFamilyPagedAttention,
			ConstAxes: map[string]int64{
				"num_heads_local":    32,
				"num_kv_heads_local": 8,
				"head_dim":           128,
				"page_size":          16,
			},
			InputDtypes:  []string{"bfloat16", "bfloat16", "bfloat16"},
			OutputDtypes: []string{"bfloat16"},
		}
		tp2Spec := tp1Spec
		tp2Spec.ConstAxes = map[string]int64{
			"num_heads_local":    16,
			"num_kv_heads_local": 4,
			"head_dim":           128,
			"page_size":          16,
		}

		kTP1, err := Build(tp1Spec)
		if err != nil {
			t.Fatalf("Build(tp1Spec) error: %v", err)
		}
		kTP2, err := Build(tp2Spec)
		if err != nil {
			t.Fatalf("Build(tp2Spec) error: %v", err)
		}
		if kTP1.SignatureHash16 == kTP2.SignatureHash16 {
			t.Fatalf("TP1 and TP2 local shard keys collided: %q", kTP1.String())
		}

		ep1Spec := BuildSpec{
			Engine:        "SGLang",
			EngineVersion: "0.5.15.post1+rocm6.3",
			Manufacturer:  "amd",
			Arch:          "gfx942",
			OpFamily:      OpFamilyMoEPermuteUnpermute,
			ConstAxes: map[string]int64{
				"hidden_size":       4096,
				"num_experts_local": 64,
				"top_k":             8,
			},
			InputDtypes:  []string{"bfloat16"},
			OutputDtypes: []string{"bfloat16"},
		}
		ep4Spec := ep1Spec
		ep4Spec.ConstAxes = map[string]int64{
			"hidden_size":       4096,
			"num_experts_local": 16,
			"top_k":             8,
		}

		kEP1, err := Build(ep1Spec)
		if err != nil {
			t.Fatalf("Build(ep1Spec) error: %v", err)
		}
		kEP4, err := Build(ep4Spec)
		if err != nil {
			t.Fatalf("Build(ep4Spec) error: %v", err)
		}
		if kEP1.SignatureHash16 == kEP4.SignatureHash16 {
			t.Fatalf("EP1 and EP4 local shard keys collided: %q", kEP1.String())
		}
	})

	t.Run("nil vs empty collections produce identical signatureHash16", func(t *testing.T) {
		hNil, err := ComputeSignatureHash(nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("ComputeSignatureHash(nil...) error: %v", err)
		}
		hEmpty, err := ComputeSignatureHash(map[string]int64{}, []string{}, []string{}, map[string]any{})
		if err != nil {
			t.Fatalf("ComputeSignatureHash(empty...) error: %v", err)
		}
		if hNil != hEmpty {
			t.Fatalf("nil hash %q != empty hash %q", hNil, hEmpty)
		}
	})

	t.Run("input dtype order matters", func(t *testing.T) {
		hA, err := ComputeSignatureHash(nil, []string{"bfloat16", "float32"}, []string{"bfloat16"}, nil)
		if err != nil {
			t.Fatalf("ComputeSignatureHash error: %v", err)
		}
		hB, err := ComputeSignatureHash(nil, []string{"float32", "bfloat16"}, []string{"bfloat16"}, nil)
		if err != nil {
			t.Fatalf("ComputeSignatureHash error: %v", err)
		}
		if hA == hB {
			t.Fatalf("swapped input dtypes produced identical hash %q", hA)
		}
	})
}

func TestPatchVersionSharingAndMinorOrPrereleaseIsolation(t *testing.T) {
	specFor := func(ver string) BuildSpec {
		return BuildSpec{
			Engine:        "vLLM",
			EngineVersion: ver,
			Manufacturer:  "nvidia",
			Arch:          "sm_90a",
			OpFamily:      OpFamilyRMSNorm,
			ConstAxes:     map[string]int64{"hidden_size": 4096},
			InputDtypes:   []string{"bfloat16"},
			OutputDtypes:  []string{"bfloat16"},
		}
	}

	k085, err := Build(specFor("0.8.5"))
	if err != nil {
		t.Fatalf("Build(0.8.5) error: %v", err)
	}
	k086, err := Build(specFor("0.8.6"))
	if err != nil {
		t.Fatalf("Build(0.8.6) error: %v", err)
	}
	if k085.String() != k086.String() {
		t.Fatalf("0.8.5 key %q != 0.8.6 key %q", k085.String(), k086.String())
	}
	if k085.EngineVersion != "0.8" {
		t.Fatalf("0.8.5 EngineVersion = %q, want 0.8", k085.EngineVersion)
	}

	parsed08, err := Parse(k085.String())
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", k085.String(), err)
	}
	for _, targetVer := range []string{"0.8", "0.8.5", "0.8.6", "v0.8.6+cu128"} {
		target := CompatibilityTarget{
			Engine:        "vLLM",
			EngineVersion: targetVer,
			Manufacturer:  "nvidia",
			Arch:          "sm_90a",
		}
		if err := MatchCompatibility(parsed08, target); err != nil {
			t.Fatalf("MatchCompatibility(%q, %q) unexpected error: %v", parsed08.String(), targetVer, err)
		}
	}

	k090, err := Build(specFor("0.9.0"))
	if err != nil {
		t.Fatalf("Build(0.9.0) error: %v", err)
	}
	if k090.String() == k085.String() {
		t.Fatalf("minor bump 0.9.0 collided with 0.8.5: %q", k090.String())
	}
	if err := MatchCompatibility(parsed08, CompatibilityTarget{
		Engine: "vLLM", EngineVersion: "0.9.0", Manufacturer: "nvidia", Arch: "sm_90a",
	}); err == nil {
		t.Fatal("expected 0.8.5 key not to match 0.9.0 target")
	}

	k090rc1, err := Build(specFor("0.9.0rc1"))
	if err != nil {
		t.Fatalf("Build(0.9.0rc1) error: %v", err)
	}
	if k090rc1.String() == k090.String() {
		t.Fatalf("pre-release 0.9.0rc1 collided with 0.9.0: %q", k090rc1.String())
	}
	if err := MatchCompatibility(k090, CompatibilityTarget{
		Engine: "vLLM", EngineVersion: "0.9.0rc1", Manufacturer: "nvidia", Arch: "sm_90a",
	}); err == nil {
		t.Fatal("expected 0.9.0 key not to match 0.9.0rc1 target")
	}
	if err := MatchCompatibility(k090rc1, CompatibilityTarget{
		Engine: "vLLM", EngineVersion: "0.9.0", Manufacturer: "nvidia", Arch: "sm_90a",
	}); err == nil {
		t.Fatal("expected 0.9.0rc1 key not to match 0.9.0 target")
	}
}

func TestParseAndValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "valid canonical nvidia", raw: "vllm/0.34/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
		{name: "valid canonical amd", raw: "sglang/0.5/amd/gfx942/paged_attention/fedcba9876543210"},
		{name: "valid canonical ascend", raw: "vllm/0.32/ascend/910b/mla_local/0011223344556677"},
		{name: "valid canonical prerelease", raw: "vllm/0.9rc1/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
		{name: "empty key", raw: "", wantErr: true},
		{name: "too few segments", raw: "vllm/0.34/nvidia/sm_90a/rmsnorm", wantErr: true},
		{name: "too many segments", raw: "vllm/0.34/nvidia/sm_90a/rmsnorm/0123456789abcdef/extra", wantErr: true},
		{name: "non-canonical engine casing", raw: "vLLM/0.34/nvidia/sm_90a/rmsnorm/0123456789abcdef", wantErr: true},
		{name: "unnormalized patch engineVersion", raw: "vllm/0.8.5/nvidia/sm_90a/rmsnorm/0123456789abcdef", wantErr: true},
		{name: "unnormalized engineVersion build suffix", raw: "vllm/0.34+cu128/nvidia/sm_90a/rmsnorm/0123456789abcdef", wantErr: true},
		{name: "unknown manufacturer", raw: "vllm/0.34/unknown/sm_90a/rmsnorm/0123456789abcdef", wantErr: true},
		{name: "unnormalized nvidia arch", raw: "vllm/0.34/nvidia/9.0/rmsnorm/0123456789abcdef", wantErr: true},
		{name: "rejected dense gemm", raw: "vllm/0.34/nvidia/sm_90a/gemm/0123456789abcdef", wantErr: true},
		{name: "rejected block-scaled gemm", raw: "vllm/0.34/nvidia/sm_90a/nvfp4_gemm/0123456789abcdef", wantErr: true},
		{name: "rejected collective", raw: "vllm/0.34/nvidia/sm_90a/all_reduce/0123456789abcdef", wantErr: true},
		{name: "uppercase hex in signatureHash16", raw: "vllm/0.34/nvidia/sm_90a/rmsnorm/0123456789ABCDEF", wantErr: true},
		{name: "short signatureHash16", raw: "vllm/0.34/nvidia/sm_90a/rmsnorm/01234567", wantErr: true},
		{name: "non-hex signatureHash16", raw: "vllm/0.34/nvidia/sm_90a/rmsnorm/0123456789abcdeg", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse(%q) err = %v, wantErr = %v", tc.raw, err, tc.wantErr)
			}
			vErr := ValidateKey(tc.raw)
			if (vErr != nil) != tc.wantErr {
				t.Fatalf("ValidateKey(%q) err = %v, wantErr = %v", tc.raw, vErr, tc.wantErr)
			}
		})
	}
}

func TestMatchCompatibility(t *testing.T) {
	key, err := Build(BuildSpec{
		Engine:        "vLLM",
		EngineVersion: "0.34.0+cu128",
		Manufacturer:  "nvidia",
		Arch:          "sm_90a",
		OpFamily:      OpFamilyRMSNorm,
		ConstAxes:     map[string]int64{"hidden_size": 4096},
		InputDtypes:   []string{"bfloat16"},
		OutputDtypes:  []string{"bfloat16"},
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	cases := []struct {
		name    string
		target  CompatibilityTarget
		wantErr bool
	}{
		{
			name: "exact match with unnormalized target fields",
			target: CompatibilityTarget{
				Engine:        "vLLM",
				EngineVersion: "0.34.0+cu128",
				Manufacturer:  "NVIDIA",
				Arch:          "sm_90a",
			},
		},
		{
			name: "patch release in same major.minor matches",
			target: CompatibilityTarget{
				Engine:        "vllm",
				EngineVersion: "0.34.2",
				Manufacturer:  "nvidia",
				Arch:          "sm_90a",
			},
		},
		{
			name: "nvidia comcap 9.0 matches sm_90a",
			target: CompatibilityTarget{
				Engine:        "vllm",
				EngineVersion: "0.34.0",
				Manufacturer:  "nvidia",
				Arch:          "9.0",
			},
		},
		{
			name: "engine mismatch",
			target: CompatibilityTarget{
				Engine:        "SGLang",
				EngineVersion: "0.34.0",
				Manufacturer:  "nvidia",
				Arch:          "sm_90a",
			},
			wantErr: true,
		},
		{
			name: "engineVersion minor mismatch",
			target: CompatibilityTarget{
				Engine:        "vLLM",
				EngineVersion: "0.35.0",
				Manufacturer:  "nvidia",
				Arch:          "sm_90a",
			},
			wantErr: true,
		},
		{
			name: "manufacturer mismatch",
			target: CompatibilityTarget{
				Engine:        "vLLM",
				EngineVersion: "0.34.0",
				Manufacturer:  "amd",
				Arch:          "gfx942",
			},
			wantErr: true,
		},
		{
			name: "arch mismatch",
			target: CompatibilityTarget{
				Engine:        "vLLM",
				EngineVersion: "0.34.0",
				Manufacturer:  "nvidia",
				Arch:          "8.0",
			},
			wantErr: true,
		},
		{
			name: "invalid target engine",
			target: CompatibilityTarget{
				Engine:        "",
				EngineVersion: "0.34.0",
				Manufacturer:  "nvidia",
				Arch:          "sm_90a",
			},
			wantErr: true,
		},
		{
			name: "invalid target engineVersion",
			target: CompatibilityTarget{
				Engine:        "vLLM",
				EngineVersion: "latest",
				Manufacturer:  "nvidia",
				Arch:          "sm_90a",
			},
			wantErr: true,
		},
		{
			name: "invalid target manufacturer",
			target: CompatibilityTarget{
				Engine:        "vLLM",
				EngineVersion: "0.34.0",
				Manufacturer:  "unknown",
				Arch:          "sm_90a",
			},
			wantErr: true,
		},
		{
			name: "invalid target arch",
			target: CompatibilityTarget{
				Engine:        "vLLM",
				EngineVersion: "0.34.0",
				Manufacturer:  "nvidia",
				Arch:          "",
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := MatchCompatibility(key, tc.target)
			if (err != nil) != tc.wantErr {
				t.Fatalf("MatchCompatibility err = %v, wantErr = %v", err, tc.wantErr)
			}
			mErr := key.MatchesTarget(tc.target)
			if (mErr != nil) != tc.wantErr {
				t.Fatalf("key.MatchesTarget err = %v, wantErr = %v", mErr, tc.wantErr)
			}
		})
	}

	if err := MatchCompatibility(LocalKernelKey{}, cases[0].target); err == nil {
		t.Fatal("MatchCompatibility with invalid key expected error")
	}
}

func TestBuildAndValidateEdgeErrors(t *testing.T) {
	valid := BuildSpec{
		Engine:        "vLLM",
		EngineVersion: "0.34.0",
		Manufacturer:  "nvidia",
		Arch:          "sm_90a",
		OpFamily:      OpFamilyRMSNorm,
		ConstAxes:     map[string]int64{"hidden_size": 4096},
		InputDtypes:   []string{"torch.bfloat16"},
		OutputDtypes:  []string{"bfloat16"},
	}

	badBuildCases := []struct {
		name   string
		mutate func(*BuildSpec)
	}{
		{name: "bad engine", mutate: func(s *BuildSpec) { s.Engine = "tgi" }},
		{name: "bad engineVersion", mutate: func(s *BuildSpec) { s.EngineVersion = "bad" }},
		{name: "bad manufacturer", mutate: func(s *BuildSpec) { s.Manufacturer = "unknown" }},
		{name: "bad arch", mutate: func(s *BuildSpec) { s.Arch = "" }},
		{name: "excluded op", mutate: func(s *BuildSpec) { s.OpFamily = "cublasLtMatmul" }},
		{name: "empty constAxis key", mutate: func(s *BuildSpec) { s.ConstAxes = map[string]int64{"": 1} }},
		{name: "empty inputDtype", mutate: func(s *BuildSpec) { s.InputDtypes = []string{""} }},
		{name: "empty outputDtype", mutate: func(s *BuildSpec) { s.OutputDtypes = []string{"  "} }},
		{name: "empty constant key", mutate: func(s *BuildSpec) { s.Constants = map[string]any{"": 1} }},
		{name: "unmarshalable constant", mutate: func(s *BuildSpec) { s.Constants = map[string]any{"x": math.NaN()} }},
	}

	for _, tc := range badBuildCases {
		t.Run(tc.name, func(t *testing.T) {
			spec := valid
			tc.mutate(&spec)
			if _, err := Build(spec); err == nil {
				t.Fatalf("Build(%s) expected error, got nil", tc.name)
			}
		})
	}

	baseKey, err := Build(valid)
	if err != nil {
		t.Fatalf("Build(valid) error: %v", err)
	}

	if (LocalKernelKey{}).String() != "" {
		t.Fatal("empty LocalKernelKey.String() expected empty string")
	}
	if _, err := (LocalKernelKey{}).Format(); err == nil {
		t.Fatal("empty LocalKernelKey.Format() expected error")
	}

	badKeyCases := []struct {
		name   string
		mutate func(*LocalKernelKey)
	}{
		{name: "non-canonical engine", mutate: func(k *LocalKernelKey) { k.Engine = "vLLM" }},
		{name: "non-canonical patch version", mutate: func(k *LocalKernelKey) { k.EngineVersion = "0.34.0" }},
		{name: "non-canonical version metadata", mutate: func(k *LocalKernelKey) { k.EngineVersion = "0.34+cu128" }},
		{name: "non-canonical manufacturer", mutate: func(k *LocalKernelKey) { k.Manufacturer = "NVIDIA" }},
		{name: "non-canonical arch", mutate: func(k *LocalKernelKey) { k.Arch = "9.0" }},
		{name: "empty constAxis name", mutate: func(k *LocalKernelKey) { k.ConstAxes = map[string]int64{" ": 1} }},
		{name: "dynamic var axis in ConstAxes", mutate: func(k *LocalKernelKey) { k.ConstAxes = map[string]int64{"batch_size": 8} }},
		{name: "mismatched signatureHash16", mutate: func(k *LocalKernelKey) { k.SignatureHash16 = "0000000000000000" }},
		{name: "unmarshalable constant in Validate", mutate: func(k *LocalKernelKey) { k.Constants = map[string]any{"x": math.NaN()} }},
	}

	for _, tc := range badKeyCases {
		t.Run(tc.name, func(t *testing.T) {
			k := baseKey
			tc.mutate(&k)
			if err := k.Validate(); err == nil {
				t.Fatalf("Validate(%s) expected error, got nil", tc.name)
			}
		})
	}

	if _, err := ComputeSignatureHash(map[string]int64{"": 1}, nil, nil, nil); err == nil {
		t.Fatal("ComputeSignatureHash empty axis expected error")
	}
	if _, err := ComputeSignatureHash(nil, []string{""}, nil, nil); err == nil {
		t.Fatal("ComputeSignatureHash empty inputDtype expected error")
	}
	if _, err := ComputeSignatureHash(nil, nil, []string{""}, nil); err == nil {
		t.Fatal("ComputeSignatureHash empty outputDtype expected error")
	}
	if _, err := ComputeSignatureHash(nil, nil, nil, map[string]any{"": 1}); err == nil {
		t.Fatal("ComputeSignatureHash empty constant expected error")
	}
}
