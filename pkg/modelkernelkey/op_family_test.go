package modelkernelkey

import "testing"

func TestAllowedOpFamilies(t *testing.T) {
	allowed := AllowedOpFamilies()
	if len(allowed) != 12 {
		t.Fatalf("AllowedOpFamilies() len = %d, want 12", len(allowed))
	}

	for _, op := range allowed {
		if !IsAllowedOpFamily(op) {
			t.Fatalf("IsAllowedOpFamily(%q) = false, want true", op)
		}
		if err := ValidateOpFamily(op); err != nil {
			t.Fatalf("ValidateOpFamily(%q) unexpected error: %v", op, err)
		}
		cls := ClassifyOp(op)
		if !cls.Allowed() || cls.Excluded() || cls.Category != OpCategoryAllowed || cls.OpFamily != op {
			t.Fatalf("ClassifyOp(%q) = %+v, want allowed", op, cls)
		}
	}
}

func TestClassifyOpAndExclusions(t *testing.T) {
	cases := []struct {
		name         string
		op           string
		wantCategory OpCategory
		wantExcluded bool
		wantAllowed  bool
	}{
		// Allowed single-GPU op families
		{name: "rmsnorm", op: "rmsnorm", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "layernorm", op: "layernorm", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "rope_kv_append", op: "rope_kv_append", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "activation_quant", op: "activation_quant", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "moe_router_topk", op: "moe_router_topk", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "moe_permute_unpermute", op: "moe_permute_unpermute", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "paged_attention", op: "paged_attention", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "ragged_prefill_attention", op: "ragged_prefill_attention", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "mla_local", op: "mla_local", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "speculative_verify", op: "speculative_verify", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "sampling_logits", op: "sampling_logits", wantCategory: OpCategoryAllowed, wantAllowed: true},
		{name: "custom_elementwise_fusion", op: "custom_elementwise_fusion", wantCategory: OpCategoryAllowed, wantAllowed: true},

		// Regular dense GEMM exclusions
		{name: "dense gemm", op: "gemm", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "matmul", op: "matmul", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "bmm", op: "bmm", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "addmm", op: "addmm", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "cublasLtMatmul", op: "cublasLtMatmul", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "cublasGemmEx", op: "cublasGemmEx", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "hipblaslt", op: "hipblaslt_matmul", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "rocblas", op: "rocblas_gemm_ex", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "aten mm", op: "aten::mm", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "aten addmm", op: "aten::addmm", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "aten bmm", op: "aten::bmm", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "cutlass dense gemm", op: "cutlass::gemm::device::Gemm", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},

		// Block-scaled GEMM exclusions
		{name: "fp8_blockwise_gemm", op: "fp8_blockwise_gemm", wantCategory: OpCategoryExcludedBlockScaledGEMM, wantExcluded: true},
		{name: "blockwise generic gemm", op: "w8a8_blockwise_scaled_gemm", wantCategory: OpCategoryExcludedBlockScaledGEMM, wantExcluded: true},
		{name: "mxfp8_gemm", op: "mxfp8_gemm", wantCategory: OpCategoryExcludedBlockScaledGEMM, wantExcluded: true},
		{name: "mxfp8_mm", op: "mxfp8_linear_mm", wantCategory: OpCategoryExcludedBlockScaledGEMM, wantExcluded: true},
		{name: "nvfp4_gemm", op: "nvfp4_gemm", wantCategory: OpCategoryExcludedBlockScaledGEMM, wantExcluded: true},
		{name: "cutlass_block_scaled_gemm", op: "cutlass_block_scaled_gemm", wantCategory: OpCategoryExcludedBlockScaledGEMM, wantExcluded: true},
		{name: "deep_gemm prefix", op: "deep_gemm_fp8_nt", wantCategory: OpCategoryExcludedBlockScaledGEMM, wantExcluded: true},
		{name: "deep_gemm namespace", op: "vllm::deep_gemm_fp8_nt", wantCategory: OpCategoryExcludedBlockScaledGEMM, wantExcluded: true},

		// Multi-GPU collective exclusions
		{name: "nccl kernel", op: "ncclDevKernel_AllReduce_Sum_bf16", wantCategory: OpCategoryExcludedCollective, wantExcluded: true},
		{name: "rccl kernel", op: "rcclKernel_AllGather", wantCategory: OpCategoryExcludedCollective, wantExcluded: true},
		{name: "hccl kernel", op: "hccl_all_reduce", wantCategory: OpCategoryExcludedCollective, wantExcluded: true},
		{name: "all_reduce", op: "all_reduce", wantCategory: OpCategoryExcludedCollective, wantExcluded: true},
		{name: "all_gather", op: "all_gather", wantCategory: OpCategoryExcludedCollective, wantExcluded: true},
		{name: "reduce_scatter", op: "reduce_scatter", wantCategory: OpCategoryExcludedCollective, wantExcluded: true},
		{name: "all_to_all", op: "all_to_all", wantCategory: OpCategoryExcludedCollective, wantExcluded: true},
		{name: "fused_all_reduce", op: "fused_all_reduce", wantCategory: OpCategoryExcludedCollective, wantExcluded: true},
		{name: "fused_all_reduce_rmsnorm must be excluded as collective", op: "flashinfer::fused_all_reduce_rmsnorm", wantCategory: OpCategoryExcludedCollective, wantExcluded: true},

		// Additional dense GEMM token forms
		{name: "standalone mm token", op: "custom_mm", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},
		{name: "sgemm substring", op: "cutlass_sgemm_128x128", wantCategory: OpCategoryExcludedDenseGEMM, wantExcluded: true},

		// Unknown / non-canonical names (neither allowed OpFamily nor excluded)
		{name: "unknown op", op: "gamma_scale_kernel", wantCategory: OpCategoryUnknown},
		{name: "unregistered alias", op: "rms_norm", wantCategory: OpCategoryUnknown},
		{name: "empty", op: "", wantCategory: OpCategoryUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := ClassifyOp(tc.op)
			if cls.Category != tc.wantCategory {
				t.Fatalf("ClassifyOp(%q).Category = %q, want %q", tc.op, cls.Category, tc.wantCategory)
			}
			if cls.Excluded() != tc.wantExcluded {
				t.Fatalf("ClassifyOp(%q).Excluded() = %v, want %v", tc.op, cls.Excluded(), tc.wantExcluded)
			}
			if cls.Allowed() != tc.wantAllowed {
				t.Fatalf("ClassifyOp(%q).Allowed() = %v, want %v", tc.op, cls.Allowed(), tc.wantAllowed)
			}

			excluded, reason := IsExcluded(tc.op)
			if excluded != tc.wantExcluded {
				t.Fatalf("IsExcluded(%q) = %v, want %v", tc.op, excluded, tc.wantExcluded)
			}
			if excluded && reason == "" {
				t.Fatalf("IsExcluded(%q) returned empty reason", tc.op)
			}

			err := ValidateOpFamily(tc.op)
			if tc.wantAllowed && err != nil {
				t.Fatalf("ValidateOpFamily(%q) unexpected error: %v", tc.op, err)
			}
			if !tc.wantAllowed && err == nil {
				t.Fatalf("ValidateOpFamily(%q) expected error, got nil", tc.op)
			}
		})
	}
}

func TestFilterConstAxes(t *testing.T) {
	in := map[string]int64{
		"hidden_size":       4096,
		"head_dim":          128,
		"num_heads_local":   8,
		"batch_size":        16,
		"seq_len":           1024,
		"num_tokens":        2048,
		"num_seqs":          4,
		"max_seq_len":       4096,
		"page_size":         16,
		"num_experts_local": 32,
	}

	got, err := FilterConstAxes(in)
	if err != nil {
		t.Fatalf("FilterConstAxes unexpected error: %v", err)
	}
	want := map[string]int64{
		"hidden_size":       4096,
		"head_dim":          128,
		"num_heads_local":   8,
		"page_size":         16,
		"num_experts_local": 32,
	}
	if len(got) != len(want) {
		t.Fatalf("FilterConstAxes len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("FilterConstAxes[%q] = %d, want %d", k, got[k], v)
		}
	}

	if _, err := FilterConstAxes(map[string]int64{" ": 1}); err == nil {
		t.Fatal("FilterConstAxes with empty key expected error, got nil")
	}
	if err := ValidateOpFamily("RMSNorm"); err == nil {
		t.Fatal("ValidateOpFamily uppercase expected error, got nil")
	}
	if err := ValidateOpFamily(" rmsnorm "); err == nil {
		t.Fatal("ValidateOpFamily whitespace expected error, got nil")
	}
}
