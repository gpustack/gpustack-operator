package modelkernelkey

import (
	"fmt"
	"slices"
	"strings"
)

// Allowed single-GPU operator family names accepted by LocalKernelKey.
const (
	OpFamilyRMSNorm                 = "rmsnorm"
	OpFamilyLayerNorm               = "layernorm"
	OpFamilyRopeKVAppend            = "rope_kv_append"
	OpFamilyActivationQuant         = "activation_quant"
	OpFamilyMoERouterTopK           = "moe_router_topk"
	OpFamilyMoEPermuteUnpermute     = "moe_permute_unpermute"
	OpFamilyPagedAttention          = "paged_attention"
	OpFamilyRaggedPrefillAttention  = "ragged_prefill_attention"
	OpFamilyMLALocal                = "mla_local"
	OpFamilySpeculativeVerify       = "speculative_verify"
	OpFamilySamplingLogits          = "sampling_logits"
	OpFamilyCustomElementwiseFusion = "custom_elementwise_fusion"
)

var allowedOpFamilies = []string{
	OpFamilyRMSNorm,
	OpFamilyLayerNorm,
	OpFamilyRopeKVAppend,
	OpFamilyActivationQuant,
	OpFamilyMoERouterTopK,
	OpFamilyMoEPermuteUnpermute,
	OpFamilyPagedAttention,
	OpFamilyRaggedPrefillAttention,
	OpFamilyMLALocal,
	OpFamilySpeculativeVerify,
	OpFamilySamplingLogits,
	OpFamilyCustomElementwiseFusion,
}

var allowedOpFamilySet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(allowedOpFamilies))
	for _, op := range allowedOpFamilies {
		m[op] = struct{}{}
	}
	return m
}()

// OpCategory classifies an operator or kernel name against the single-GPU allowlist
// and the hard-excluded GEMM and collective categories.
type OpCategory string

const (
	OpCategoryAllowed                 OpCategory = "allowed"
	OpCategoryExcludedDenseGEMM       OpCategory = "excluded_dense_gemm"
	OpCategoryExcludedBlockScaledGEMM OpCategory = "excluded_block_scaled_gemm"
	OpCategoryExcludedCollective      OpCategory = "excluded_collective"
	OpCategoryUnknown                 OpCategory = "unknown"
)

// OpClassification describes the result of classifying an operator or kernel symbol name.
type OpClassification struct {
	Name     string
	Category OpCategory
	OpFamily string
	Reason   string
}

// Allowed reports whether the classified operator is an allowed single-GPU OpFamily.
func (c OpClassification) Allowed() bool {
	return c.Category == OpCategoryAllowed
}

// Excluded reports whether the classified operator belongs to a hard-excluded category.
func (c OpClassification) Excluded() bool {
	return c.Category == OpCategoryExcludedDenseGEMM ||
		c.Category == OpCategoryExcludedBlockScaledGEMM ||
		c.Category == OpCategoryExcludedCollective
}

// AllowedOpFamilies returns a copy of all supported single-GPU OpFamily names.
func AllowedOpFamilies() []string {
	return slices.Clone(allowedOpFamilies)
}

// IsAllowedOpFamily reports whether opFamily is in the canonical single-GPU allowlist.
func IsAllowedOpFamily(opFamily string) bool {
	_, ok := allowedOpFamilySet[opFamily]
	return ok
}

// ClassifyOp classifies an operator name, OpFamily, or kernel symbol into allowed,
// excluded (collective, block-scaled GEMM, dense GEMM), or unknown.
//
// Collectives and block-scaled GEMMs are checked before dense GEMMs and allowed names
// so hybrid names such as "fused_all_reduce_rmsnorm" or "nvfp4_gemm" never bypass exclusion.
func ClassifyOp(name string) OpClassification {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return OpClassification{Name: name, Category: OpCategoryUnknown, Reason: "empty operator name"}
	}

	lower := strings.ToLower(trimmed)
	tokens := splitIdentifierTokens(lower)

	if reason, ok := matchCollectiveExclusion(lower, tokens); ok {
		return OpClassification{
			Name:     name,
			Category: OpCategoryExcludedCollective,
			Reason:   reason,
		}
	}
	if reason, ok := matchBlockScaledGEMMExclusion(lower, tokens); ok {
		return OpClassification{
			Name:     name,
			Category: OpCategoryExcludedBlockScaledGEMM,
			Reason:   reason,
		}
	}
	if reason, ok := matchDenseGEMMExclusion(lower, tokens); ok {
		return OpClassification{
			Name:     name,
			Category: OpCategoryExcludedDenseGEMM,
			Reason:   reason,
		}
	}

	if IsAllowedOpFamily(lower) {
		return OpClassification{
			Name:     name,
			Category: OpCategoryAllowed,
			OpFamily: lower,
		}
	}

	return OpClassification{
		Name:     name,
		Category: OpCategoryUnknown,
		Reason:   fmt.Sprintf("operator %q is not a recognized single-GPU opFamily", name),
	}
}

// IsExcluded reports whether name matches any hard-excluded GEMM or collective pattern,
// returning a human-readable exclusion category reason when true.
func IsExcluded(name string) (bool, string) {
	cls := ClassifyOp(name)
	if cls.Excluded() {
		return true, cls.Reason
	}
	return false, ""
}

// ValidateOpFamily ensures opFamily is strictly in the canonical single-GPU allowlist
// and returns a descriptive error for excluded GEMM/collective ops or unknown families.
func ValidateOpFamily(opFamily string) error {
	trimmed := strings.TrimSpace(opFamily)
	if trimmed == "" {
		return fmt.Errorf("opFamily must not be empty")
	}
	cls := ClassifyOp(trimmed)
	if cls.Excluded() {
		return fmt.Errorf("opFamily %q is excluded (%s)", opFamily, cls.Reason)
	}
	if opFamily != trimmed || opFamily != strings.ToLower(trimmed) {
		return fmt.Errorf("opFamily %q must be canonical lowercase without surrounding whitespace", opFamily)
	}
	if !cls.Allowed() {
		return fmt.Errorf("unsupported opFamily %q", opFamily)
	}
	return nil
}

func matchCollectiveExclusion(lower string, tokens []string) (string, bool) {
	collectivePrefixes := []string{"nccl", "rccl", "hccl", "fused_all_reduce", "nvshmem", "rocshmem", "msccl"}
	for _, prefix := range collectivePrefixes {
		if strings.HasPrefix(lower, prefix) || hasTokenPrefix(tokens, prefix) {
			return "multi-gpu collective kernel (" + prefix + ")", true
		}
	}

	collectiveSubstrings := []string{
		"all_reduce", "allreduce",
		"all_gather", "allgather",
		"reduce_scatter", "reducescatter",
		"all_to_all", "alltoall",
		"fused_all_reduce",
	}
	for _, sub := range collectiveSubstrings {
		if strings.Contains(lower, sub) {
			return "multi-gpu collective kernel (" + sub + ")", true
		}
	}
	return "", false
}

func matchBlockScaledGEMMExclusion(lower string, tokens []string) (string, bool) {
	if strings.HasPrefix(lower, "deep_gemm") || strings.HasPrefix(lower, "deepgemm") ||
		hasTokenPrefix(tokens, "deep_gemm") || hasTokenPrefix(tokens, "deepgemm") {
		return "block-scaled GEMM kernel (deep_gemm)", true
	}

	blockScaledSubstrings := []string{
		"fp8_blockwise_gemm",
		"blockwise_fp8_gemm",
		"blockwise_fp8_matmul",
		"mxfp8_gemm",
		"mxfp8_scaled_mm",
		"mxfp8_matmul",
		"nvfp4_gemm",
		"nvfp4_matmul",
		"fp4_gemm",
		"cutlass_block_scaled_gemm",
		"block_scaled_gemm",
	}
	for _, sub := range blockScaledSubstrings {
		if strings.Contains(lower, sub) {
			return "block-scaled GEMM kernel (" + sub + ")", true
		}
	}

	if (strings.Contains(lower, "block_scaled") || strings.Contains(lower, "blockwise")) &&
		(hasToken(tokens, "gemm") || hasToken(tokens, "matmul") || strings.Contains(lower, "gemm")) {
		return "block-scaled GEMM kernel", true
	}
	if (hasToken(tokens, "mxfp8") || hasToken(tokens, "nvfp4")) &&
		(hasToken(tokens, "gemm") || hasToken(tokens, "matmul") || hasToken(tokens, "mm")) {
		return "block-scaled GEMM kernel", true
	}
	return "", false
}

func matchDenseGEMMExclusion(lower string, tokens []string) (string, bool) {
	blasPrefixes := []string{"cublas", "hipblas", "rocblas"}
	for _, prefix := range blasPrefixes {
		if strings.HasPrefix(lower, prefix) || hasTokenPrefix(tokens, prefix) {
			return "regular dense GEMM kernel (" + prefix + ")", true
		}
	}

	exactDenseOps := []string{"aten::mm", "aten::addmm", "aten::bmm", "aten::matmul", "torch.mm", "torch.addmm", "torch.bmm", "torch.matmul"}
	for _, op := range exactDenseOps {
		if lower == op || strings.Contains(lower, op) {
			return "regular dense GEMM operator (" + op + ")", true
		}
	}

	if hasToken(tokens, "gemm") || hasToken(tokens, "matmul") || hasToken(tokens, "bmm") || hasToken(tokens, "addmm") {
		return "regular dense GEMM kernel", true
	}
	// Bare "mm" is matched only as a standalone token so names like "rmsnorm" are never matched.
	if hasToken(tokens, "mm") {
		return "regular dense GEMM kernel (mm)", true
	}
	if strings.Contains(lower, "gemm") || strings.Contains(lower, "matmul") || strings.Contains(lower, "addmm") {
		return "regular dense GEMM kernel", true
	}
	return "", false
}

func splitIdentifierTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
}

func hasToken(tokens []string, want string) bool {
	for _, tok := range tokens {
		if tok == want {
			return true
		}
	}
	return false
}

func hasTokenPrefix(tokens []string, prefix string) bool {
	pTokens := splitIdentifierTokens(prefix)
	if len(pTokens) == 0 {
		return false
	}
	if len(pTokens) == 1 {
		for _, tok := range tokens {
			if strings.HasPrefix(tok, pTokens[0]) {
				return true
			}
		}
		return false
	}
	for i := 0; i+len(pTokens) <= len(tokens); i++ {
		match := true
		for j := range pTokens {
			if j == len(pTokens)-1 {
				if !strings.HasPrefix(tokens[i+j], pTokens[j]) {
					match = false
					break
				}
			} else if tokens[i+j] != pTokens[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// dynamicVarAxes contains batch, sequence, and token-count axes that must never
// enter ConstAxes or signatureHash16 so compiled kernels stay batch-size invariant.
var dynamicVarAxes = map[string]struct{}{
	"batch":              {},
	"batch_size":         {},
	"bs":                 {},
	"context_len":        {},
	"kv_len":             {},
	"max_num_tokens":     {},
	"max_seq_len":        {},
	"num_decode_tokens":  {},
	"num_prefill_tokens": {},
	"num_requests":       {},
	"num_seqs":           {},
	"num_tokens":         {},
	"q_len":              {},
	"seq_len":            {},
	"seq_length":         {},
	"sequence_length":    {},
	"token_count":        {},
	"total_tokens":       {},
}

// IsDynamicVarAxis reports whether an axis name represents a runtime-variable
// batch or sequence dimension excluded from ConstAxes hashing.
func IsDynamicVarAxis(name string) bool {
	_, ok := dynamicVarAxes[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// FilterConstAxes clones axes into a non-nil map after trimming keys, rejecting
// empty axis names, and dropping dynamic runtime var axes.
func FilterConstAxes(axes map[string]int64) (map[string]int64, error) {
	out := make(map[string]int64, len(axes))
	for k, v := range axes {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			return nil, fmt.Errorf("constAxes contains an empty axis name")
		}
		if IsDynamicVarAxis(trimmed) {
			continue
		}
		out[trimmed] = v
	}
	return out, nil
}
