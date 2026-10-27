package priors

import "testing"

// TestCanonicalQuantizationFoldsTheSpellingsThatNameOneRecipe asserts the
// spellings that name one recipe fold to one token.
//
// IT MATTERS BECAUSE A RECIPE IS SPELLED SEVERAL WAYS, and a comparison of
// two spellings says nothing about the weights behind them.
func TestCanonicalQuantizationFoldsTheSpellingsThatNameOneRecipe(t *testing.T) {
	cases := []struct {
		name     string
		spelling string
		want     string
	}{
		{name: "the plain spelling", spelling: "fp8", want: "fp8"},
		{name: "the e4m3 spelling", spelling: "fp8_e4m3", want: "fp8"},
		{name: "the dashed e4m3 spelling", spelling: "fp8-e4m3", want: "fp8"},
		{name: "the e4m3fn spelling", spelling: "fp8_e4m3fn", want: "fp8"},
		{name: "the weighted eight bit floating point spelling", spelling: "w8a8-fp8", want: "fp8"},
		{name: "the bfloat16 spelling", spelling: "bfloat16", want: "bf16"},
		{name: "the float16 spelling", spelling: "float16", want: "fp16"},
		{name: "the half spelling", spelling: "half", want: "fp16"},
		{name: "the nvfp4 spelling", spelling: "nvfp4", want: "fp4"},
		{name: "the mxfp4 spelling", spelling: "mxfp4", want: "fp4"},
		{name: "the weighted eight bit spelling", spelling: "w8a8", want: "int8"},
		{name: "the smoothquant spelling", spelling: "smoothquant", want: "int8"},
		{name: "the weighted four bit spelling", spelling: "w4a16", want: "int4"},
		{name: "the uppercase spelling", spelling: "FP8_E4M3", want: "fp8"},
		{name: "the surrounding space", spelling: "  fp8  ", want: "fp8"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := CanonicalQuantization(testCase.spelling); got != testCase.want {
				t.Fatalf("quantization %q is %q, want %q", testCase.spelling, got, testCase.want)
			}
		})
	}
}

// TestCanonicalQuantizationLeavesAnUnknownRecipeAlone asserts a recipe this
// operator cannot name is not folded into another one.
//
// IT MATTERS BECAUSE A RECIPE NOBODY VOUCHED FOR IS NOT THE RECIPE A
// MEASUREMENT RAN UNDER.
func TestCanonicalQuantizationLeavesAnUnknownRecipeAlone(t *testing.T) {
	cases := []struct {
		name     string
		spelling string
		want     string
	}{
		{name: "the awq recipe", spelling: "awq", want: "awq"},
		{name: "the gptq recipe", spelling: "gptq", want: "gptq"},
		{name: "an unknown recipe", spelling: "not-a-recipe", want: "not-a-recipe"},
		{name: "an empty recipe", spelling: "", want: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := CanonicalQuantization(testCase.spelling); got != testCase.want {
				t.Fatalf("quantization %q is %q, want %q", testCase.spelling, got, testCase.want)
			}
		})
	}
}

// TestNormalizeModelIdentityNamesTheUnpinnedRevision asserts the fields a
// record leaves empty are named.
//
// IT MATTERS BECAUSE A FIELD NOBODY FILLED IN AND A FIELD THAT SAYS THE
// SOURCE PINNED NOTHING ARE DIFFERENT STATEMENTS.
func TestNormalizeModelIdentityNamesTheUnpinnedRevision(t *testing.T) {
	identity := NormalizeModelIdentity(ModelIdentity{Name: "  example/model  ", Quantization: "bfloat16"})

	if identity.Name != "example/model" {
		t.Fatalf("the model name is %q", identity.Name)
	}
	if identity.Quantization != "bf16" {
		t.Fatalf("the quantization is %q", identity.Quantization)
	}
	if identity.Revision != UnspecifiedRevision {
		t.Fatalf("the revision is %q, want %q", identity.Revision, UnspecifiedRevision)
	}
	if identity.Tokenizer != "example/model" {
		t.Fatalf("the tokenizer is %q", identity.Tokenizer)
	}
}

// TestNormalizeModelIdentityKeepsWhatTheRecordStates asserts a record keeps
// the revision and the tokenizer it states.
//
// IT MATTERS BECAUSE A NORMALIZATION THAT REPLACED WHAT THE SOURCE SAID WOULD
// DESCRIBE A DIFFERENT MODEL.
func TestNormalizeModelIdentityKeepsWhatTheRecordStates(t *testing.T) {
	identity := NormalizeModelIdentity(ModelIdentity{
		Name:         "example/model",
		Revision:     "rev-1",
		Tokenizer:    "example/tokenizer",
		Quantization: "fp8",
	})

	if identity.Revision != "rev-1" {
		t.Fatalf("the revision is %q", identity.Revision)
	}
	if identity.Tokenizer != "example/tokenizer" {
		t.Fatalf("the tokenizer is %q", identity.Tokenizer)
	}
	if identity.Quantization != "fp8" {
		t.Fatalf("the quantization is %q", identity.Quantization)
	}
}

// TestNormalizeHardwareIdentityFoldsTheVendorSpellings asserts the published
// spellings of one accelerator fold to one family.
//
// IT MATTERS BECAUSE THE SAME ACCELERATOR REACHES A CATALOG UNDER A PRODUCT
// LINE NAME AND UNDER A PART NUMBER.
func TestNormalizeHardwareIdentityFoldsTheVendorSpellings(t *testing.T) {
	cases := []struct {
		name        string
		accelerator string
		family      string
		memoryGB    int
		formFactor  string
	}{
		{name: "the plain name", accelerator: "mi325x", family: "amd-mi325x", memoryGB: 256, formFactor: "OAM Module"},
		{name: "the product line name", accelerator: "AMD Instinct MI325X", family: "amd-mi325x", memoryGB: 256, formFactor: "OAM Module"},
		{name: "the family itself", accelerator: "amd-mi325x", family: "amd-mi325x", memoryGB: 256, formFactor: "OAM Module"},
		{name: "the part number in a product string", accelerator: "NVIDIA H100-SXM5-80GB", family: "nvidia-h100", memoryGB: 80, formFactor: "SXM"},
		{name: "the memory in the name", accelerator: "NVIDIA H200 141GB", family: "nvidia-h200", memoryGB: 141, formFactor: "SXM"},
		{name: "the blackwell name", accelerator: "B200-SXM", family: "nvidia-b200", memoryGB: 192, formFactor: "SXM"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			identity := NormalizeHardwareIdentity(HardwareIdentity{Accelerator: testCase.accelerator})

			if identity.Accelerator != testCase.family {
				t.Fatalf("the accelerator is %q, want %q", identity.Accelerator, testCase.family)
			}
			if identity.MemoryGB != testCase.memoryGB {
				t.Fatalf("the memory is %d, want %d", identity.MemoryGB, testCase.memoryGB)
			}
			if identity.FormFactor != testCase.formFactor {
				t.Fatalf("the form factor is %q, want %q", identity.FormFactor, testCase.formFactor)
			}
		})
	}
}

// TestNormalizeHardwareIdentityKeepsWhatTheRecordStates asserts the memory
// and the form factor the record states are kept.
//
// IT MATTERS BECAUSE THE RECORD STATES THE MEMORY THE DEPLOYMENT HAS, and the
// default states what this operator knows about the accelerator.
func TestNormalizeHardwareIdentityKeepsWhatTheRecordStates(t *testing.T) {
	identity := NormalizeHardwareIdentity(HardwareIdentity{
		Accelerator: "mi325x",
		MemoryGB:    192,
		FormFactor:  "OAM Module",
	})

	if identity.Accelerator != "amd-mi325x" {
		t.Fatalf("the accelerator is %q", identity.Accelerator)
	}
	if identity.MemoryGB != 192 {
		t.Fatalf("the memory is %d, want 192", identity.MemoryGB)
	}
}

// TestNormalizeHardwareIdentityLeavesAnUnknownAcceleratorAlone asserts an
// accelerator this operator cannot name is not folded into another one.
//
// IT MATTERS BECAUSE AN ACCELERATOR NOBODY CAN NAME IS NOT ONE THAT MAY BE
// FOLDED.
func TestNormalizeHardwareIdentityLeavesAnUnknownAcceleratorAlone(t *testing.T) {
	identity := NormalizeHardwareIdentity(HardwareIdentity{
		Accelerator: "Example Accelerator",
		MemoryGB:    270,
		FormFactor:  "SXM6",
	})

	if identity.Accelerator != "Example Accelerator" {
		t.Fatalf("the accelerator is %q", identity.Accelerator)
	}
	if identity.MemoryGB != 270 {
		t.Fatalf("the memory is %d, want 270", identity.MemoryGB)
	}
	if identity.FormFactor != "SXM6" {
		t.Fatalf("the form factor is %q", identity.FormFactor)
	}
}

// TestHardwareIdentityMatchesAcceptsTheVendorSpellings asserts one
// accelerator matches under every spelling a catalog and a deployment use.
//
// IT MATTERS BECAUSE THE CATALOG RECORDS THE FAMILY AND THE DEPLOYMENT
// DECLARES THE PRODUCT, AND THE TWO SPELL ONE ACCELERATOR DIFFERENTLY.
func TestHardwareIdentityMatchesAcceptsTheVendorSpellings(t *testing.T) {
	want := HardwareIdentity{Accelerator: "amd-mi325x"}
	cases := []struct {
		name        string
		accelerator string
	}{
		{name: "the bare part number", accelerator: "mi325x"},
		{name: "the product line name", accelerator: "AMD Instinct MI325X"},
		{name: "the family itself", accelerator: "amd-mi325x"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := HardwareIdentity{Accelerator: testCase.accelerator}
			if !HardwareIdentityMatches(want, got) {
				t.Fatalf("the accelerator %q does not match %q", testCase.accelerator, want.Accelerator)
			}
		})
	}
}

// TestHardwareIdentityMatchesRefusesAnotherAccelerator asserts two
// accelerators never match.
//
// IT MATTERS BECAUSE A MEASUREMENT TAKEN ON ONE ACCELERATOR DESCRIBES THAT
// ACCELERATOR AND NOT THE OTHER ONE.
func TestHardwareIdentityMatchesRefusesAnotherAccelerator(t *testing.T) {
	want := HardwareIdentity{Accelerator: "amd-mi325x"}
	got := HardwareIdentity{Accelerator: "h100"}

	if HardwareIdentityMatches(want, got) {
		t.Fatal("two accelerators match")
	}
}

// TestModelIdentityMatchesAcceptsTheBareName asserts a deployment that
// declares the model without its organization matches the catalog.
//
// IT MATTERS BECAUSE A DEPLOYMENT DECLARES THE MODEL IT SERVES AND A SOURCE
// DECLARES THE MODEL IT MEASURED, AND THE TWO SPELL ONE MODEL DIFFERENTLY.
func TestModelIdentityMatchesAcceptsTheBareName(t *testing.T) {
	want := ModelIdentity{
		Name:         "deepseek-ai/DeepSeek-R1-0528",
		Revision:     UnspecifiedRevision,
		Tokenizer:    "deepseek-ai/DeepSeek-R1-0528",
		Quantization: "fp8",
	}
	got := ModelIdentity{
		Name:         "DeepSeek-R1-0528",
		Revision:     "rev-1",
		Tokenizer:    "deepseek-ai/DeepSeek-R1-0528",
		Quantization: "fp8",
	}

	if !ModelIdentityMatches(want, got) {
		t.Fatal("the bare model name does not match the qualified one")
	}
}

// TestModelIdentityMatchesRefusesAnotherOrganization asserts a name that is
// only the suffix of two models matches neither of them.
//
// IT MATTERS BECAUSE THE BARE NAME MATCHES ONLY WHEN ONE SIDE CARRIES NO
// ORGANIZATION.
func TestModelIdentityMatchesRefusesAnotherOrganization(t *testing.T) {
	want := ModelIdentity{Name: "first/model", Tokenizer: "first/model"}
	got := ModelIdentity{Name: "second/model", Tokenizer: "second/model"}

	if ModelIdentityMatches(want, got) {
		t.Fatal("the names of two organizations match")
	}
}

// TestModelIdentityMatchesRefusesTwoPinnedRevisions asserts two different
// pinned revisions do not match.
//
// IT MATTERS BECAUSE TWO PINNED REVISIONS ARE TWO DIFFERENT SETS OF WEIGHTS.
func TestModelIdentityMatchesRefusesTwoPinnedRevisions(t *testing.T) {
	want := ModelIdentity{Name: "example/model", Revision: "rev-1", Tokenizer: "example/model"}
	got := ModelIdentity{Name: "example/model", Revision: "rev-2", Tokenizer: "example/model"}

	if ModelIdentityMatches(want, got) {
		t.Fatal("two pinned revisions match")
	}
}

// TestModelIdentityMatchesAcceptsOneUnpinnedRevision asserts an unpinned
// revision matches a pinned one.
//
// IT MATTERS BECAUSE A SOURCE THAT PINNED NO WEIGHT COMMIT AND A DEPLOYMENT
// THAT PINNED NONE ARE THE SAME STATEMENT.
func TestModelIdentityMatchesAcceptsOneUnpinnedRevision(t *testing.T) {
	want := ModelIdentity{Name: "example/model", Revision: UnspecifiedRevision, Tokenizer: "example/model"}
	got := ModelIdentity{Name: "example/model", Revision: "rev-1", Tokenizer: "example/model"}

	if !ModelIdentityMatches(want, got) {
		t.Fatal("an unpinned revision does not match a pinned one")
	}
}

// TestModelIdentityMatchesRefusesAnotherRecipe asserts two different
// recipes do not match.
//
// IT MATTERS BECAUSE A RECIPE DECIDES THE WEIGHTS AND THE THROUGHPUT.
func TestModelIdentityMatchesRefusesAnotherRecipe(t *testing.T) {
	want := ModelIdentity{Name: "example/model", Tokenizer: "example/model", Quantization: "fp8"}
	got := ModelIdentity{Name: "example/model", Tokenizer: "example/model", Quantization: "bf16"}

	if ModelIdentityMatches(want, got) {
		t.Fatal("two recipes match")
	}
}

// TestEngineFamilyReadsTheImage asserts the family is read from the image.
//
// IT MATTERS BECAUSE THE FAMILY IS WHAT THE IMAGE CARRIES AND A TAG OR A
// DIGEST SAYS NOTHING ABOUT IT.
func TestEngineFamilyReadsTheImage(t *testing.T) {
	cases := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "the sglang image",
			image: "lmsysorg/sglang:v0.5.22-rocm720-mi30x@sha256:58b6bc",
			want:  "sglang",
		},
		{
			name:  "the vllm image",
			image: "vllm/vllm-openai:v0.8.5",
			want:  "vllm",
		},
		{
			name:  "an image that names no family",
			image: "registry.example/engine@sha256:58b6bc",
			want:  "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := EngineFamily(testCase.image); got != testCase.want {
				t.Fatalf("the family of %q is %q, want %q", testCase.image, got, testCase.want)
			}
		})
	}
}
