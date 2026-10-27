package priors

import (
	"strings"

	"gpustack.ai/gpustack/pkg/nodefeature"
)

// UnspecifiedRevision is the revision an identity that pinned no weight commit carries.
//
// It is a named value and not an empty field, because an empty field says a record carries
// nothing while a named value says the source made a statement about what it did not pin.
const UnspecifiedRevision = "unspecified"

// quantizationAliases maps a quantization spelling to its canonical token.
//
// The table is closed, because a spelling the table does not carry is one this operator cannot
// vouch for, and an unvouched recipe is not the recipe a measurement ran under.
//
// The keys are normalized spellings, so "fp8_e4m3" and "fp8-e4m3" reach the same entry.
// "w8a8-fp8" is kept apart from "w8a8", because a weight-only eight-bit recipe and an eight-bit
// floating point recipe are different recipes.
var quantizationAliases = map[string]string{
	"fp8":         "fp8",
	"fp8-e4m3":    "fp8",
	"fp8-e4m3fn":  "fp8",
	"w8a8-fp8":    "fp8",
	"bf16":        "bf16",
	"bfloat16":    "bf16",
	"fp16":        "fp16",
	"float16":     "fp16",
	"half":        "fp16",
	"fp4":         "fp4",
	"nvfp4":       "fp4",
	"mxfp4":       "fp4",
	"int8":        "int8",
	"w8a8":        "int8",
	"smoothquant": "int8",
	"int4":        "int4",
	"w4a16":       "int4",
	"awq":         "awq",
	"gptq":        "gptq",
}

// acceleratorSpecs maps an accelerator family to what is known about it without being told.
//
// The keys are the family names of pkg/nodefeature, so a name the accelerator table resolves
// names one accelerator and no two.
//
// The memory and the form factor are defaults and not facts, because a deployment states its own
// memory and a source states its own form factor. The default only fills a field the record left
// empty.
var acceleratorSpecs = map[string]struct {
	memoryGB   int
	formFactor string
}{
	"amd-mi325x":       {memoryGB: 256, formFactor: "OAM Module"},
	"amd-mi300x":       {memoryGB: 192, formFactor: "OAM Module"},
	"amd-mi350x":       {memoryGB: 288, formFactor: "OAM Module"},
	"amd-mi355x":       {memoryGB: 288, formFactor: "OAM Module"},
	"nvidia-h100":      {memoryGB: 80, formFactor: "SXM"},
	"nvidia-h200":      {memoryGB: 141, formFactor: "SXM"},
	"nvidia-b200":      {memoryGB: 192, formFactor: "SXM"},
	"nvidia-b300":      {memoryGB: 288, formFactor: "SXM"},
	"nvidia-gb200":     {memoryGB: 186, formFactor: "SXM"},
	"nvidia-gb300":     {memoryGB: 288, formFactor: "SXM"},
	"nvidia-a100-80gb": {memoryGB: 80, formFactor: "SXM"},
	"nvidia-a100-40gb": {memoryGB: 40, formFactor: "SXM"},
}

// canonicalModelNames maps a public benchmark's internal model id to the
// verified Hugging Face repository that holds the model.
//
// IT EXISTS BECAUSE A BENCHMARK NAMES A MODEL BY A SHORT ID and a deployment
// names it by its repository, and the catalog has to name the one model both
// spell.
//
// THE KEYS ARE LOWERCASED, because the ids are written in one case and looked
// up in another.
var canonicalModelNames = map[string]string{
	"dsr1":        "deepseek-ai/DeepSeek-R1-0528",
	"dsv4":        "deepseek-ai/DeepSeek-V4-Pro",
	"glm5":        "zai-org/GLM-5",
	"glm5.1":      "zai-org/GLM-5",
	"glm5.2":      "zai-org/GLM-5.2",
	"glm5.3":      "zai-org/GLM-5.2",
	"kimik2.5":    "moonshotai/Kimi-K2.5",
	"kimik2.6":    "moonshotai/Kimi-K2.5",
	"kimik3":      "moonshotai/Kimi-K3",
	"llama70b":    "meta-llama/Llama-3.3-70B-Instruct",
	"minimaxm3":   "MiniMaxAI/MiniMax-M3",
	"qwen3.5":     "Qwen/Qwen3.5-397B-A17B",
	"qwen3.8next": "Qwen/Qwen3.8-Flash-Next",
	"gptoss120b":  "openai/gpt-oss-120b",
}

// CanonicalModelName returns the canonical name of a model.
//
// IT MAPS THE SHORT IDS A PUBLIC BENCHMARK PUBLISHES, because a measurement of
// "dsr1" is a measurement of one model and the catalog has to name that model
// the way a deployment names it.
//
// IT KEEPS A REPOSITORY NAME AS IT IS, because a repository already names the
// model it holds.
func CanonicalModelName(name string) string {
	trimmed := strings.TrimSpace(name)
	if canonical, known := canonicalModelNames[strings.ToLower(trimmed)]; known {
		return canonical
	}
	return trimmed
}

// CanonicalQuantization returns the canonical token of a quantization.
//
// It folds the spellings that name the same recipe, because "bfloat16" and "bf16" name one recipe
// and a comparison of two spellings says nothing about the weights behind them.
//
// It leaves an unknown spelling alone, because a recipe this operator cannot name is not a recipe
// it may fold into another one.
func CanonicalQuantization(quantization string) string {
	key := normalizeSpelling(quantization)
	if canonical, found := quantizationAliases[key]; found {
		return canonical
	}
	return key
}

// NormalizeModelIdentity returns the canonical form of a model identity.
//
// It keeps the name as it is written, because the name is what a deployment declares and what a
// status field shows. The comparison of two names is a separate question that ModelIdentityMatches
// answers.
//
// It canonicalizes the quantization, because the recipe is the same recipe under several
// spellings.
//
// It names the unpinned revision, because a source that pinned no weight commit made a statement
// about that and a record that says nothing does not.
//
// It defaults the tokenizer to the model name, because a model served with its own tokenizer is
// the ordinary case and a source that watched no separate tokenizer was watching the model's.
func NormalizeModelIdentity(identity ModelIdentity) ModelIdentity {
	identity.Name = CanonicalModelName(identity.Name)
	identity.Quantization = CanonicalQuantization(identity.Quantization)
	identity.Revision = canonicalRevision(identity.Revision)
	identity.Tokenizer = strings.TrimSpace(identity.Tokenizer)
	if identity.Tokenizer == "" || identity.Tokenizer == UnspecifiedRevision {
		identity.Tokenizer = identity.Name
	}
	return identity
}

// NormalizeHardwareIdentity returns the canonical form of a hardware identity.
//
// It resolves the accelerator through the accelerator table of pkg/nodefeature, because the same
// accelerator reaches a catalog under a product line name and under a part number.
//
// It fills an empty memory and an empty form factor from the canonical accelerator, because an
// accelerator this operator knows states its own memory and form factor. A field the record states
// is kept, because the record states the one the deployment has.
//
// It leaves an unknown accelerator alone, because an accelerator this operator cannot name is not
// one it may fold into another one.
func NormalizeHardwareIdentity(identity HardwareIdentity) HardwareIdentity {
	identity.Accelerator = strings.TrimSpace(identity.Accelerator)
	family := nodefeature.ResolveAcceleratorFamily(identity.Accelerator)
	if family == "" {
		return identity
	}
	identity.Accelerator = family
	spec, known := acceleratorSpecs[family]
	if !known {
		return identity
	}
	if identity.MemoryGB == 0 {
		identity.MemoryGB = spec.memoryGB
	}
	if identity.FormFactor == "" {
		identity.FormFactor = spec.formFactor
	}
	return identity
}

// ModelIdentityMatches says whether two model identities name the same model.
//
// It compares the name without its organization, because a deployment declares the model without
// one and the catalog records it with one. The bare name matches only when one side carries no
// organization, so a name that is the suffix of two different models matches neither.
//
// It accepts an unpinned revision on either side, because a source that pinned no weight commit
// and a deployment that pinned none are the same statement, while two different pinned revisions
// are a conflict.
func ModelIdentityMatches(want, got ModelIdentity) bool {
	if !ModelNameMatches(want.Name, got.Name) {
		return false
	}
	if CanonicalQuantization(want.Quantization) != CanonicalQuantization(got.Quantization) {
		return false
	}
	if !RevisionMatches(want.Revision, got.Revision) {
		return false
	}
	return TokenizerMatches(want.Tokenizer, got.Tokenizer)
}

// HardwareIdentityMatches says whether two hardware identities name the same accelerator.
//
// It compares the canonical identities, because the same accelerator reaches a catalog under
// several spellings.
func HardwareIdentityMatches(want, got HardwareIdentity) bool {
	return NormalizeHardwareIdentity(want) == NormalizeHardwareIdentity(got)
}

// EngineFamily returns the serving engine family an image belongs to.
//
// It is empty when the image names no known family, because an image this operator cannot place
// is not one it may match against a deployment.
//
// It reads the image and not the tag, because the family is what the image carries and a tag or a
// digest says nothing about it.
func EngineFamily(image string) string {
	name := strings.ToLower(image)
	switch {
	case strings.Contains(name, "sglang"):
		return "sglang"
	case strings.Contains(name, "vllm"):
		return "vllm"
	default:
		return ""
	}
}

// canonicalRevision returns the canonical form of a revision.
//
// It names the unpinned revision, because a record that pins no commit says so when it says
// "unspecified" and says nothing when it says nothing.
func canonicalRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return UnspecifiedRevision
	}
	return revision
}

// RevisionMatches says whether two revisions agree.
//
// It accepts an unpinned revision on either side, because a source that pinned no weight commit
// and a deployment that pinned none are the same statement.
func RevisionMatches(want, got string) bool {
	want, got = canonicalRevision(want), canonicalRevision(got)
	return want == got || want == UnspecifiedRevision || got == UnspecifiedRevision
}

// TokenizerMatches says whether two tokenizer identities agree.
//
// It accepts an unpinned tokenizer on either side, because a source that named no tokenizer
// served the model with the model's own.
func TokenizerMatches(want, got string) bool {
	want, got = strings.TrimSpace(want), strings.TrimSpace(got)
	if want == "" || got == "" {
		return true
	}
	return want == got || want == UnspecifiedRevision || got == UnspecifiedRevision
}

// ModelNameMatches says whether two model names name the same model.
//
// The organization is optional on one side only, because a name that is the suffix of two
// different models names neither of them.
func ModelNameMatches(want, got string) bool {
	want, got = CanonicalModelName(want), CanonicalModelName(got)
	if want == "" || got == "" {
		return false
	}
	if strings.EqualFold(want, got) {
		return true
	}
	if !strings.Contains(got, "/") && strings.EqualFold(modelBareName(want), got) {
		return true
	}
	if !strings.Contains(want, "/") && strings.EqualFold(modelBareName(got), want) {
		return true
	}
	return false
}

// modelBareName returns the part of a model name after its organization.
func modelBareName(name string) string {
	if _, bare, found := strings.Cut(name, "/"); found {
		return bare
	}
	return name
}

// normalizeSpelling returns the comparable form of a spelling.
//
// It lowercases the spelling and unifies the separators, because "FP8_E4M3" and "fp8-e4m3" name
// one recipe.
func normalizeSpelling(spelling string) string {
	spelling = strings.ToLower(strings.TrimSpace(spelling))
	spelling = strings.ReplaceAll(spelling, "_", "-")
	return strings.Join(strings.Fields(spelling), " ")
}
