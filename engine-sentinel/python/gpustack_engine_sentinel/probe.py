"""Short-window /gpustack/v1/probe controller, /dev/shm staging, and FlashInfer Trace distillation."""

from __future__ import annotations

import asyncio
import contextlib
import gzip
import hashlib
import hmac
import inspect
import json
import logging
import math
import os
import re
import shutil
import stat
import time
import uuid
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from gpustack_engine_sentinel.protocol import (
    DEFAULT_PROBE_MIN_COOLDOWN_SECONDS,
    DEFAULT_PROBE_SHM_DIR,
    DEFAULT_SHM_SIZE_BYTES,
    ENV_PROBE_AUTH_TOKEN,
    ENV_PROBE_MIN_COOLDOWN_SECONDS,
    ENV_PROBE_SHM_DIR,
    ENV_PROBE_SHM_SIZE_BYTES,
    PROBE_MAX_PAYLOAD_BYTES,
    PROBE_MAX_SHM_BYTES,
    PROBE_SHM_FRACTION,
    PROBE_STALE_AGE_SECONDS,
    PROBE_WALL_CLOCK_CAP_SECONDS,
    ProbeCooldownError,
    ProbeInProgressError,
    ProbeRequest,
    RolePausedError,
    SentinelError,
    UnauthorizedError,
    UnsupportedEngineVersionError,
)

LOGGER = logging.getLogger(__name__)

ALLOWED_OP_FAMILIES: frozenset[str] = frozenset(
    {
        "rmsnorm",
        "layernorm",
        "rope_kv_append",
        "activation_quant",
        "moe_router_topk",
        "moe_permute_unpermute",
        "paged_attention",
        "ragged_prefill_attention",
        "mla_local",
        "speculative_verify",
        "sampling_logits",
        "custom_elementwise_fusion",
    }
)

_EXACT_EXCLUDED_OPS: frozenset[str] = frozenset(
    {
        "gemm",
        "matmul",
        "bmm",
        "addmm",
        "mm",
        "sgemm",
        "hgemm",
        "bgemm",
        "igemm",
        "dgemm",
        "aten::mm",
        "aten::addmm",
        "aten::bmm",
        "aten::matmul",
        "aten::linear",
        "aten::baddbmm",
        "fp8_blockwise_gemm",
        "mxfp8_gemm",
        "nvfp4_gemm",
        "cutlass_block_scaled_gemm",
        "all_reduce",
        "all_gather",
        "reduce_scatter",
        "all_to_all",
        "allreduce",
        "allgather",
        "reducescatter",
        "alltoall",
    }
)

_EXCLUDED_SUBSTRINGS: tuple[str, ...] = (
    "cublas",
    "hipblas",
    "rocblas",
    "deep_gemm",
    "deepgemm",
    "block_scaled_gemm",
    "blockwise_gemm",
    "fp8_blockwise_gemm",
    "mxfp8_gemm",
    "nvfp4_gemm",
    "cutlass_block_scaled_gemm",
    "cutlass_gemm",
    "cutlass::gemm",
    "nccl",
    "rccl",
    "hccl",
    "nvshmem",
    "rocshmem",
    "msccl",
    "fused_all_reduce",
    "all_reduce",
    "all_gather",
    "reduce_scatter",
    "all_to_all",
    "allreduce",
    "allgather",
    "reducescatter",
    "alltoall",
    "c10d::",
    "dist::",
    "aten::mm",
    "aten::addmm",
    "aten::bmm",
    "aten::matmul",
    "aten::linear",
)

_EXCLUDED_TOKEN_RE = re.compile(
    r"(^|[^a-z0-9])(gemm|sgemm|hgemm|bgemm|igemm|dgemm|matmul|bmm|addmm|baddbmm|mm)([^a-z0-9]|$)"
)

_OP_FAMILY_PATTERNS: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("rmsnorm", ("rmsnorm", "rms_norm", "fused_add_rms_norm")),
    ("layernorm", ("layernorm", "layer_norm")),
    (
        "rope_kv_append",
        (
            "rope_kv_append",
            "rotary_embedding",
            "apply_rotary",
            "rope",
            "reshape_and_cache",
            "append_paged_kv",
            "kv_cache_append",
        ),
    ),
    (
        "activation_quant",
        (
            "activation_quant",
            "silu_and_mul",
            "mul_silu",
            "silu_mul",
            "gelu_and_mul",
            "swiglu",
            "scaled_fp8_quant",
            "dynamic_scaled_fp8_quant",
            "static_scaled_fp8_quant",
            "per_token_group_quant",
            "per_token_quant",
            "act_quant",
        ),
    ),
    (
        "moe_router_topk",
        ("moe_router_topk", "topk_softmax", "moe_router", "grouped_topk", "fused_topk"),
    ),
    (
        "moe_permute_unpermute",
        (
            "moe_permute_unpermute",
            "moe_permute",
            "moe_unpermute",
            "moe_align_block_size",
            "moe_sum",
            "moe_shuffle",
        ),
    ),
    (
        "paged_attention",
        (
            "paged_attention",
            "flash_attn_with_kvcache",
            "batch_decode_with_paged_kv",
            "paged_kv",
        ),
    ),
    (
        "ragged_prefill_attention",
        (
            "ragged_prefill_attention",
            "batch_prefill_with_ragged_kv",
            "flash_attn_varlen",
            "ragged_prefill",
        ),
    ),
    (
        "mla_local",
        ("mla_local", "flash_mla", "concat_mla", "mla_decode", "mla_prefill"),
    ),
    (
        "speculative_verify",
        (
            "speculative_verify",
            "verify_tree",
            "rejection_sample",
            "spec_verify",
            "eagle_verify",
        ),
    ),
    (
        "sampling_logits",
        (
            "sampling_logits",
            "top_k_top_p",
            "sampling_from_probs",
            "min_p_sampling",
            "renorm_probs",
            "logits_processor",
        ),
    ),
    (
        "custom_elementwise_fusion",
        (
            "custom_elementwise_fusion",
            "fused_add",
            "elementwise_fusion",
            "fused_bias",
            "fused_mul",
        ),
    ),
)

_OP_REFERENCES: dict[str, str] = {
    "rmsnorm": (
        "def run(x, weight, eps=1e-6):\n"
        "    return x * torch.rsqrt(x.pow(2).mean(-1, keepdim=True) + eps) * weight"
    ),
    "layernorm": (
        "def run(x, weight, bias=None, eps=1e-5):\n"
        "    return torch.nn.functional.layer_norm(x, (x.shape[-1],), weight, bias, eps)"
    ),
    "rope_kv_append": (
        "def run(q, k, cos, sin):\n"
        "    c = cos.unsqueeze(-2) if cos.ndim < q.ndim else cos\n"
        "    s = sin.unsqueeze(-2) if sin.ndim < q.ndim else sin\n"
        "    half = q.shape[-1] // 2\n"
        "    if half == 0:\n"
        "        return (q * c, k * c)\n"
        "    q_rot = torch.cat((-q[..., half : 2 * half], q[..., :half]), dim=-1)\n"
        "    k_rot = torch.cat((-k[..., half : 2 * half], k[..., :half]), dim=-1)\n"
        "    c_use = c[..., : 2 * half]\n"
        "    s_use = s[..., : 2 * half]\n"
        "    return (\n"
        "        q[..., : 2 * half] * c_use + q_rot * s_use,\n"
        "        k[..., : 2 * half] * c_use + k_rot * s_use,\n"
        "    )"
    ),
    "activation_quant": (
        "def run(x):\n"
        "    half = x.shape[-1] // 2\n"
        "    if half == 0:\n"
        "        return torch.nn.functional.silu(x)\n"
        "    return torch.nn.functional.silu(x[..., :half]) * x[..., half : 2 * half]"
    ),
    "moe_router_topk": (
        "def run(logits, top_k=8):\n"
        "    k = max(1, min(int(top_k), logits.shape[-1]))\n"
        "    probs = torch.softmax(logits, dim=-1)\n"
        "    return torch.topk(probs, k=k, dim=-1)"
    ),
    "moe_permute_unpermute": (
        "def run(x, indices=None):\n"
        "    if indices is None:\n"
        "        indices = torch.arange(x.shape[0] - 1, -1, -1, device=x.device, dtype=torch.long)\n"
        "    return x.index_select(0, indices)"
    ),
    "paged_attention": (
        "def run(q, k_cache, v_cache):\n"
        "    scale = q.shape[-1] ** -0.5\n"
        "    if q.ndim == 3:\n"
        "        qh = q.transpose(0, 1)\n"
        "        kh = k_cache.transpose(0, 1)\n"
        "        vh = v_cache.transpose(0, 1)\n"
        "        scores = torch.matmul(qh, kh.transpose(-2, -1)) * scale\n"
        "        probs = torch.softmax(scores, dim=-1)\n"
        "        return torch.matmul(probs, vh).transpose(0, 1)\n"
        "    scores = torch.matmul(q, k_cache.transpose(-2, -1)) * scale\n"
        "    probs = torch.softmax(scores, dim=-1)\n"
        "    return torch.matmul(probs, v_cache)"
    ),
    "ragged_prefill_attention": (
        "def run(q, k, v):\n"
        "    scale = q.shape[-1] ** -0.5\n"
        "    if q.ndim == 3:\n"
        "        qh = q.transpose(0, 1)\n"
        "        kh = k.transpose(0, 1)\n"
        "        vh = v.transpose(0, 1)\n"
        "        scores = torch.matmul(qh, kh.transpose(-2, -1)) * scale\n"
        "        t = scores.shape[-1]\n"
        "        mask = torch.triu(torch.ones((t, t), device=q.device, dtype=torch.bool), diagonal=1)\n"
        "        scores = scores.masked_fill(mask, float('-inf'))\n"
        "        probs = torch.softmax(scores, dim=-1)\n"
        "        return torch.matmul(probs, vh).transpose(0, 1)\n"
        "    scores = torch.matmul(q, k.transpose(-2, -1)) * scale\n"
        "    t = scores.shape[-1]\n"
        "    mask = torch.triu(torch.ones((t, t), device=q.device, dtype=torch.bool), diagonal=1)\n"
        "    scores = scores.masked_fill(mask, float('-inf'))\n"
        "    probs = torch.softmax(scores, dim=-1)\n"
        "    return torch.matmul(probs, v)"
    ),
    "mla_local": (
        "def run(q_nope, q_pe, kv_c, k_pe):\n"
        "    q = torch.cat((q_nope, q_pe), dim=-1)\n"
        "    k = torch.cat((kv_c, k_pe), dim=-1)\n"
        "    scale = q.shape[-1] ** -0.5\n"
        "    if q.ndim == 3:\n"
        "        qh = q.transpose(0, 1)\n"
        "        kh = k.transpose(0, 1)\n"
        "        vh = kv_c.transpose(0, 1)\n"
        "        scores = torch.matmul(qh, kh.transpose(-2, -1)) * scale\n"
        "        probs = torch.softmax(scores, dim=-1)\n"
        "        return torch.matmul(probs, vh).transpose(0, 1)\n"
        "    scores = torch.matmul(q, k.transpose(-2, -1)) * scale\n"
        "    probs = torch.softmax(scores, dim=-1)\n"
        "    return torch.matmul(probs, kv_c)"
    ),
    "speculative_verify": (
        "def run(draft_logits, target_logits):\n"
        "    draft_probs = torch.softmax(draft_logits, dim=-1)\n"
        "    target_probs = torch.softmax(target_logits, dim=-1)\n"
        "    ratio = target_probs / draft_probs.clamp_min(1e-8)\n"
        "    return torch.clamp(ratio, max=1.0)"
    ),
    "sampling_logits": (
        "def run(logits, top_k=50, top_p=0.9):\n"
        "    k = max(1, min(int(top_k), logits.shape[-1]))\n"
        "    top_vals, top_idx = torch.topk(logits, k=k, dim=-1)\n"
        "    probs = torch.softmax(top_vals, dim=-1)\n"
        "    cumprobs = torch.cumsum(probs, dim=-1)\n"
        "    keep = (cumprobs - probs) <= float(top_p)\n"
        "    masked = probs * keep.to(probs.dtype)\n"
        "    norm = masked.sum(dim=-1, keepdim=True).clamp_min(1e-8)\n"
        "    return (masked / norm, top_idx)"
    ),
    "custom_elementwise_fusion": "def run(x, y):\n    return x + y",
}

_VERSION_GIT_DESCRIBE_RE = re.compile(
    r"(?:-\d+)?[-.](?:g|git)[0-9a-fA-F]{6,}(?:[-.].*)?$", re.ASCII
)
_VERSION_DIRTY_RE = re.compile(r"[-.]dirty$", re.ASCII)
_CANONICAL_ENGINE_VERSION_RE = re.compile(
    r"^(\d+)\.(\d+)(?:\.\d+(?:\.\d+)?)?(?:(?:\.?|-)post\d+(?:\.\d+)*)?((?:(?:\.?[ab]|(?:\.?|-)(?:rc|alpha|beta|dev))\d*(?:\.\d+)*)?)$",
    re.ASCII,
)
_HEX16_RE = re.compile(r"^[0-9a-f]{16}$")
_NVIDIA_DOT_COMCAP_RE = re.compile(r"^(\d+)\.(\d+)([a-z]*)$")
_NVIDIA_SM_OR_COMPUTE_RE = re.compile(r"^(?:sm|compute)_(\d+)([a-z]*)$")
_SAFE_ARCH_RE = re.compile(r"^[a-z0-9][a-z0-9._-]*$")
_MAX_HOTSPOT_KERNEL_NAME_LEN = 256
_MAX_TOP_RANKED_DEFINITIONS = 16

_DYNAMIC_VAR_AXES: frozenset[str] = frozenset(
    {
        "batch",
        "batch_size",
        "bs",
        "context_len",
        "kv_len",
        "max_num_tokens",
        "max_seq_len",
        "num_decode_tokens",
        "num_prefill_tokens",
        "num_requests",
        "num_seqs",
        "num_tokens",
        "q_len",
        "seq_len",
        "seq_length",
        "sequence_length",
        "token_count",
        "total_tokens",
    }
)

_SENSITIVE_ARG_KEYS: frozenset[str] = frozenset(
    {
        "prompt",
        "prompts",
        "prompt_text",
        "text",
        "input_text",
        "input_ids",
        "token_ids",
        "tokens",
        "raw_tensor",
        "tensor_data",
        "tensor_values",
        "values",
        "data",
        "activations",
    }
)


def is_excluded_operator(name: str) -> bool:
    """Return True if operator name matches regular GEMM, block-scaled GEMM, or multi-GPU collectives."""
    if not isinstance(name, str):
        return True
    lower = name.strip().lower()
    if not lower:
        return True
    if lower in _EXACT_EXCLUDED_OPS:
        return True
    for needle in _EXCLUDED_SUBSTRINGS:
        if needle in lower:
            return True
    return bool(_EXCLUDED_TOKEN_RE.search(lower))


def classify_op_family(name: str) -> str | None:
    """Map a profiler operator name to an allowed single-GPU OpFamily, or None if excluded/unknown."""
    if not isinstance(name, str) or not name.strip():
        return None
    if is_excluded_operator(name):
        return None
    lower = name.strip().lower()
    if lower in ALLOWED_OP_FAMILIES:
        return lower
    for family, patterns in _OP_FAMILY_PATTERNS:
        for pat in patterns:
            if pat in lower:
                return family
    return None


def canonical_engine_name(raw: str | None) -> str:
    """Return canonical lowercase engine identifier (vllm or sglang) matching pkg/worker/kernelkey."""
    text = (raw or "").strip()
    lower = text.lower()
    if lower in ("vllm", "sglang"):
        return lower
    return lower or "unknown"


def normalize_engine_version(raw: str | None) -> str:
    """Normalize engine version to canonical major.minor(+prerelease), stripping build and git suffixes."""
    if not isinstance(raw, str):
        raise ValueError(f"Invalid engineVersion: {raw!r}")
    text = raw.strip()
    if not text:
        raise ValueError("engineVersion must not be empty")
    if any(c.isspace() or c == "/" for c in text):
        raise ValueError(f"engineVersion {raw!r} contains invalid characters")
    if len(text) > 1 and text[0] in ("v", "V") and "0" <= text[1] <= "9":
        text = text[1:]
    plus = text.find("+")
    if plus >= 0:
        text = text[:plus]
    text = _VERSION_GIT_DESCRIBE_RE.sub("", text)
    text = _VERSION_DIRTY_RE.sub("", text)
    m = _CANONICAL_ENGINE_VERSION_RE.fullmatch(text)
    if m is None:
        raise ValueError(f"Invalid engineVersion: {raw!r}")
    return f"{m.group(1)}.{m.group(2)}{m.group(3)}"


def _normalize_dtypes(dtypes: list[str] | tuple[str, ...] | None) -> list[str]:
    if not dtypes:
        return []
    out: list[str] = []
    for item in dtypes:
        text = str(item).strip().lower()
        text = text.removeprefix("torch.")
        text = text.removeprefix("c10::")
        if text:
            out.append(text)
    return out


def _canonicalize_constant_value(value: Any) -> Any:
    if isinstance(value, bool):
        return value
    if isinstance(value, float) and math.isfinite(value) and value.is_integer():
        return int(value)
    if isinstance(value, dict):
        return {
            str(k): _canonicalize_constant_value(v) for k, v in sorted(value.items())
        }
    if isinstance(value, (list, tuple)):
        return [_canonicalize_constant_value(v) for v in value]
    return value


def compute_signature_hash16(
    const_axes: dict[str, int],
    input_dtypes: list[str] | tuple[str, ...],
    output_dtypes: list[str] | tuple[str, ...],
    constants: dict[str, Any] | None = None,
) -> str:
    """Compute first 16 lowercase hex chars of SHA-256 over canonical sorted-key JSON signature."""
    filtered_axes = {
        str(k).strip(): int(v)
        for k, v in sorted(const_axes.items())
        if str(k).strip() and str(k).strip().lower() not in _DYNAMIC_VAR_AXES
    }
    norm_constants = {
        str(k).strip(): _canonicalize_constant_value(v)
        for k, v in sorted((constants or {}).items())
        if str(k).strip()
    }
    payload = {
        "constAxes": filtered_axes,
        "constants": norm_constants,
        "inputDtypes": _normalize_dtypes(input_dtypes),
        "outputDtypes": _normalize_dtypes(output_dtypes),
    }
    encoded_str = json.dumps(
        payload,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )
    # Match Go encoding/json escaping of U+2028/U+2029 for cross-language hash parity.
    encoded_str = encoded_str.replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
    return hashlib.sha256(encoded_str.encode("utf-8")).hexdigest()[:16]


def normalize_arch(manufacturer: str | None, arch: str | None) -> str:
    """Normalize accelerator architecture matching pkg/worker/kernelkey/hardware.go:NormalizeArch."""
    manu = (manufacturer or "").strip().lower()
    raw = (arch or "").strip().lower()
    if not raw:
        return ""

    if manu == "nvidia" or (
        not manu
        and (
            _NVIDIA_SM_OR_COMPUTE_RE.match(raw) is not None
            or _NVIDIA_DOT_COMCAP_RE.match(raw) is not None
        )
    ):
        m = _NVIDIA_SM_OR_COMPUTE_RE.match(raw)
        if m is not None:
            return f"sm_{m.group(1)}{m.group(2)}"
        m = _NVIDIA_DOT_COMCAP_RE.match(raw)
        if m is not None:
            return f"sm_{m.group(1)}{m.group(2)}{m.group(3)}"
        return raw

    if manu in ("amd", "hygon") or (not manu and ":" in raw):
        colon = raw.find(":")
        if colon > 0:
            raw = raw[:colon]
        return raw

    if manu == "ascend":
        raw = raw.removeprefix("ascend-")
        raw = raw.removeprefix("ascend")
        return raw

    if manu:
        raw = raw.removeprefix(f"{manu}-")
    return raw


def format_local_kernel_key(
    *,
    engine: str,
    engine_version: str,
    manufacturer: str,
    arch: str,
    op_family: str,
    const_axes: dict[str, int],
    input_dtypes: list[str] | tuple[str, ...],
    output_dtypes: list[str] | tuple[str, ...],
    constants: dict[str, Any] | None = None,
) -> str:
    """Format canonical LocalKernelKey: <engine>/<engineVersion>/<manufacturer>/<arch>/<opFamily>/<signatureHash16>."""
    if op_family not in ALLOWED_OP_FAMILIES or is_excluded_operator(op_family):
        raise ValueError(f"Unsupported or excluded opFamily: {op_family!r}")
    eng = canonical_engine_name(engine)
    ver = normalize_engine_version(engine_version)
    mfr = (manufacturer or "unknown").strip().lower() or "unknown"
    architecture = normalize_arch(mfr, arch) or "unknown"
    if any(c.isspace() or c == "/" for c in architecture) or not _SAFE_ARCH_RE.match(
        architecture
    ):
        raise ValueError(f"Invalid arch {arch!r} for manufacturer {mfr!r}")
    sig = compute_signature_hash16(const_axes, input_dtypes, output_dtypes, constants)
    return f"{eng}/{ver}/{mfr}/{architecture}/{op_family}/{sig}"


def parse_local_kernel_key(key: str) -> dict[str, str]:
    """Parse and validate a canonical LocalKernelKey string."""
    if not isinstance(key, str) or not key.strip():
        raise ValueError("LocalKernelKey must be a non-empty string")
    parts = key.strip().split("/")
    if len(parts) != 6 or any(not p for p in parts):
        raise ValueError(f"Invalid LocalKernelKey format: {key!r}")
    engine, engine_version, manufacturer, arch, op_family, sig = parts
    normalize_engine_version(engine_version)
    if op_family not in ALLOWED_OP_FAMILIES or is_excluded_operator(op_family):
        raise ValueError(
            f"Excluded or invalid opFamily in LocalKernelKey: {op_family!r}"
        )
    if not _HEX16_RE.match(sig):
        raise ValueError(f"Invalid signatureHash16 in LocalKernelKey: {sig!r}")
    return {
        "engine": engine,
        "engine_version": engine_version,
        "manufacturer": manufacturer,
        "architecture": arch,
        "op_family": op_family,
        "signature_hash16": sig,
    }


def compute_shm_budget_bytes(
    staging_root: Path,
    *,
    shm_size_override: int | None = None,
) -> int:
    """Compute staging ceiling as min(64 MiB, 5% of shmSize)."""
    shm_size = shm_size_override
    if shm_size is None:
        raw_env = os.environ.get(ENV_PROBE_SHM_SIZE_BYTES, "").strip()
        if raw_env:
            try:
                parsed = int(raw_env)
                if parsed > 0:
                    shm_size = parsed
            except ValueError:
                shm_size = None
    if shm_size is None:
        resolved_str = str(staging_root)
        if resolved_str == "/dev/shm" or resolved_str.startswith("/dev/shm/"):
            shm_mount = Path("/dev/shm")
            if shm_mount.is_dir():
                try:
                    st = os.statvfs(shm_mount)
                    detected = int(st.f_frsize) * int(st.f_blocks)
                    if detected > 0:
                        shm_size = detected
                except OSError:
                    shm_size = None
    if shm_size is None or shm_size <= 0:
        shm_size = DEFAULT_SHM_SIZE_BYTES
    return max(1, min(PROBE_MAX_SHM_BYTES, int(shm_size * PROBE_SHM_FRACTION)))


def directory_size_bytes(path: Path) -> int:
    """Sum regular file bytes under path without following symlinks."""
    if not path.exists():
        return 0
    total = 0
    for root, _dirs, files in os.walk(path, followlinks=False):
        for fname in files:
            fpath = Path(root) / fname
            try:
                st = os.lstat(fpath)
                if stat.S_ISREG(st.st_mode):
                    total += int(st.st_size)
            except OSError:
                continue
    return total


def _sweep_entry_if_stale(
    entry: Path, *, current_ts: float, max_age_seconds: float
) -> str | None:
    try:
        st = os.lstat(entry)
        age = current_ts - float(st.st_mtime)
        if age > max_age_seconds:
            if stat.S_ISDIR(st.st_mode):
                purge_probe_dir(entry)
            else:
                entry.unlink(missing_ok=True)
            return entry.name
    except OSError:
        return None
    return None


def purge_probe_dir(probe_dir: Path) -> int:
    """Delete all files and subdirectories under probe_dir, returning count of deleted files."""
    if not probe_dir.exists():
        return 0
    deleted = 0
    for root, dirs, files in os.walk(probe_dir, topdown=False, followlinks=False):
        for fname in files:
            fpath = Path(root) / fname
            with contextlib.suppress(OSError):
                fpath.unlink()
                deleted += 1
        for dname in dirs:
            dpath = Path(root) / dname
            with contextlib.suppress(OSError):
                if dpath.is_symlink():
                    dpath.unlink()
                    deleted += 1
                else:
                    dpath.rmdir()
    with contextlib.suppress(OSError):
        shutil.rmtree(probe_dir, ignore_errors=True)
    return deleted


def sweep_stale_probe_dirs(
    staging_root: Path,
    *,
    max_age_seconds: float = PROBE_STALE_AGE_SECONDS,
    now: float | None = None,
) -> list[str]:
    """Delete orphaned probe subdirectories older than max_age_seconds. Run only by API-server latch on startup."""
    if not staging_root.is_dir():
        return []
    current_ts = time.time() if now is None else now
    removed: list[str] = []
    try:
        entries = list(staging_root.iterdir())
    except OSError:
        return []
    for entry in entries:
        name = _sweep_entry_if_stale(
            entry, current_ts=current_ts, max_age_seconds=max_age_seconds
        )
        if name is not None:
            removed.append(name)
    return sorted(removed)


def _extract_shapes_and_axes(
    op_family: str,
    input_dims: list[list[int]],
    constants: dict[str, Any] | None = None,
) -> tuple[dict[str, int], dict[str, int], dict[str, list[str]], dict[str, list[str]]]:
    """Separate static const axes (per-GPU shard dims) from dynamic var axes (batch/tokens/seq)."""
    primary_shape: list[int] = []
    for shp in input_dims:
        if (
            isinstance(shp, list)
            and shp
            and all(isinstance(d, int) and d > 0 for d in shp)
        ):
            primary_shape = shp
            break

    if not primary_shape:
        primary_shape = [1, 1]

    if op_family in (
        "rmsnorm",
        "layernorm",
        "activation_quant",
        "custom_elementwise_fusion",
    ):
        hidden_size = primary_shape[-1]
        num_tokens = 1
        for dim in primary_shape[:-1]:
            num_tokens *= dim
        const_axes = {"hidden_size": hidden_size}
        var_axes = {"num_tokens": num_tokens}
        in_specs: dict[str, list[str]] = {"x": ["num_tokens", "hidden_size"]}
        if op_family == "activation_quant":
            half_hidden_size = max(1, hidden_size // 2)
            const_axes["half_hidden_size"] = half_hidden_size
            return (
                const_axes,
                var_axes,
                in_specs,
                {"out": ["num_tokens", "half_hidden_size"]},
            )
        if op_family in ("rmsnorm", "layernorm"):
            in_specs["weight"] = ["hidden_size"]
        if op_family == "layernorm":
            in_specs["bias"] = ["hidden_size"]
        if op_family == "custom_elementwise_fusion":
            in_specs["y"] = ["num_tokens", "hidden_size"]
        return const_axes, var_axes, in_specs, {"out": ["num_tokens", "hidden_size"]}

    if op_family in (
        "rope_kv_append",
        "paged_attention",
        "ragged_prefill_attention",
        "mla_local",
    ):
        if len(primary_shape) >= 3:
            head_dim = primary_shape[-1]
            num_heads_local = primary_shape[-2]
            num_tokens = 1
            for dim in primary_shape[:-2]:
                num_tokens *= dim
            const_axes = {
                "num_heads_local": num_heads_local,
                "head_dim": head_dim,
            }
            var_axes = {"num_tokens": num_tokens}
            shape_ref = ["num_tokens", "num_heads_local", "head_dim"]
        else:
            head_dim = primary_shape[-1]
            num_tokens = primary_shape[0] if len(primary_shape) > 1 else 1
            const_axes = {"head_dim": head_dim}
            var_axes = {"num_tokens": num_tokens}
            shape_ref = ["num_tokens", "head_dim"]

        if op_family == "rope_kv_append":
            cos_sin_shape = ["num_tokens", "head_dim"]
            return (
                const_axes,
                var_axes,
                {
                    "q": shape_ref,
                    "k": shape_ref,
                    "cos": cos_sin_shape,
                    "sin": cos_sin_shape,
                },
                {"q_out": shape_ref, "k_out": shape_ref},
            )
        if op_family == "paged_attention":
            return (
                const_axes,
                var_axes,
                {"q": shape_ref, "k_cache": shape_ref, "v_cache": shape_ref},
                {"out": shape_ref},
            )
        if op_family == "ragged_prefill_attention":
            return (
                const_axes,
                var_axes,
                {"q": shape_ref, "k": shape_ref, "v": shape_ref},
                {"out": shape_ref},
            )
        return (
            const_axes,
            var_axes,
            {
                "q_nope": shape_ref,
                "q_pe": shape_ref,
                "kv_c": shape_ref,
                "k_pe": shape_ref,
            },
            {"out": shape_ref},
        )

    if op_family in ("moe_router_topk", "moe_permute_unpermute"):
        last_dim = primary_shape[-1]
        num_tokens = 1
        for dim in primary_shape[:-1]:
            num_tokens *= dim
        if op_family == "moe_router_topk":
            raw_k = constants.get("top_k") if isinstance(constants, dict) else None
            eff_k = (
                int(raw_k)
                if isinstance(raw_k, int) and not isinstance(raw_k, bool) and raw_k > 0
                else 8
            )
            top_k = max(1, min(eff_k, last_dim))
            return (
                {"num_experts_local": last_dim, "top_k": top_k},
                {"num_tokens": num_tokens},
                {"logits": ["num_tokens", "num_experts_local"]},
                {
                    "values": ["num_tokens", "top_k"],
                    "indices": ["num_tokens", "top_k"],
                },
            )
        return (
            {"hidden_size": last_dim},
            {"num_tokens": num_tokens},
            {"x": ["num_tokens", "hidden_size"]},
            {"out": ["num_tokens", "hidden_size"]},
        )

    if op_family in ("sampling_logits", "speculative_verify"):
        vocab_or_dim = primary_shape[-1]
        batch_size = 1
        for dim in primary_shape[:-1]:
            batch_size *= dim
        if op_family == "speculative_verify":
            return (
                {"vocab_size_local": vocab_or_dim},
                {"batch_size": batch_size},
                {
                    "draft_logits": ["batch_size", "vocab_size_local"],
                    "target_logits": ["batch_size", "vocab_size_local"],
                },
                {"out": ["batch_size", "vocab_size_local"]},
            )
        raw_k = constants.get("top_k") if isinstance(constants, dict) else None
        eff_k = (
            int(raw_k)
            if isinstance(raw_k, int) and not isinstance(raw_k, bool) and raw_k > 0
            else 50
        )
        top_k = max(1, min(eff_k, vocab_or_dim))
        return (
            {"vocab_size_local": vocab_or_dim, "top_k": top_k},
            {"batch_size": batch_size},
            {"logits": ["batch_size", "vocab_size_local"]},
            {
                "probs": ["batch_size", "top_k"],
                "indices": ["batch_size", "top_k"],
            },
        )

    hidden_size = primary_shape[-1]
    num_tokens = 1
    for dim in primary_shape[:-1]:
        num_tokens *= dim
    return (
        {"hidden_size": hidden_size},
        {"num_tokens": num_tokens},
        {"x": ["num_tokens", "hidden_size"]},
        {"out": ["num_tokens", "hidden_size"]},
    )


def _sanitize_input_dims(raw_dims: Any) -> list[list[int]]:
    if not isinstance(raw_dims, list):
        return []
    out: list[list[int]] = []
    for item in raw_dims:
        if isinstance(item, list):
            clean = [
                int(d)
                for d in item
                if isinstance(d, int) and not isinstance(d, bool) and d > 0
            ]
            if clean:
                out.append(clean)
        elif isinstance(item, int) and not isinstance(item, bool) and item > 0:
            out.append([int(item)])
    return out


def _sanitize_constants(raw_constants: Any) -> dict[str, Any]:
    if not isinstance(raw_constants, dict):
        return {}
    allowed_keys = {"eps", "top_k", "page_size", "block_size", "is_causal", "scale"}
    out: dict[str, Any] = {}
    for k, v in raw_constants.items():
        ks = str(k)
        if ks.lower() in _SENSITIVE_ARG_KEYS or ks not in allowed_keys:
            continue
        if isinstance(v, (int, float, bool)):
            out[ks] = v
    return out


def _normalize_trace_event(raw: dict[str, Any]) -> dict[str, Any] | None:
    """Extract only privacy-safe operator metadata (never prompt text, token IDs, or tensor values)."""
    if not isinstance(raw, dict):
        return None
    name = raw.get("name")
    if not isinstance(name, str) or not name.strip():
        return None
    args = raw.get("args") if isinstance(raw.get("args"), dict) else {}

    # Duration in microseconds.
    raw_dur = (
        raw.get("dur")
        if raw.get("dur") is not None
        else raw.get(
            "duration_us",
            raw.get("self_device_time_total", raw.get("cuda_time_us", 0.0)),
        )
    )
    try:
        dur_us = float(raw_dur or 0.0)
    except (TypeError, ValueError):
        dur_us = 0.0
    if dur_us < 0:
        dur_us = 0.0

    raw_dims = (
        raw.get("input_dims")
        or raw.get("shapes")
        or args.get("Input Dims")
        or args.get("input_dims")
        or []
    )
    input_dims = _sanitize_input_dims(raw_dims)

    raw_dtypes = (
        raw.get("input_dtypes")
        or args.get("Input type")
        or args.get("input_dtypes")
        or []
    )
    if isinstance(raw_dtypes, str):
        raw_dtypes = [raw_dtypes]
    input_dtypes = _normalize_dtypes(raw_dtypes if isinstance(raw_dtypes, list) else [])

    raw_out_dtypes = raw.get("output_dtypes") or args.get("output_dtypes") or []
    if isinstance(raw_out_dtypes, str):
        raw_out_dtypes = [raw_out_dtypes]
    output_dtypes = _normalize_dtypes(
        raw_out_dtypes if isinstance(raw_out_dtypes, list) else []
    )
    has_explicit_dtypes = bool(input_dtypes)
    if not output_dtypes and input_dtypes:
        output_dtypes = [input_dtypes[0]]
    if not input_dtypes:
        input_dtypes = ["bfloat16"]
    if not output_dtypes:
        output_dtypes = list(input_dtypes[:1])

    count = raw.get("count", 1)
    try:
        call_count = max(1, int(count))
    except (TypeError, ValueError):
        call_count = 1

    cat = str(raw.get("cat") or "").lower()
    is_cuda_graph = bool(
        raw.get("is_cuda_graph", False)
        or "cuda_graph" in cat
        or "cudagraph" in name.lower()
        or "cuda_graph" in name.lower()
        or "graphlaunch" in name.lower()
    )
    constants = _sanitize_constants(raw.get("constants") or args.get("constants"))

    return {
        "name": name.strip(),
        "dur_us": dur_us,
        "input_dims": input_dims,
        "input_dtypes": input_dtypes,
        "output_dtypes": output_dtypes,
        "has_explicit_dtypes": has_explicit_dtypes,
        "call_count": call_count,
        "is_cuda_graph": is_cuda_graph,
        "constants": constants,
    }


def _enforce_payload_budget(
    payload: dict[str, Any], max_bytes: int = PROBE_MAX_PAYLOAD_BYTES
) -> dict[str, Any]:
    """Ensure serialized distilled payload never exceeds max_bytes."""
    raw_diag = payload.get("diagnostics")
    if isinstance(raw_diag, dict) and isinstance(
        raw_diag.get("cuda_graph_hotspots"), list
    ):
        capped_hotspots: list[dict[str, Any]] = []
        changed_hs = False
        for item in raw_diag["cuda_graph_hotspots"]:
            if not isinstance(item, dict):
                continue
            kname = str(item.get("kernel_name") or "")
            if len(kname) > _MAX_HOTSPOT_KERNEL_NAME_LEN:
                item = dict(item)
                item["kernel_name"] = kname[:_MAX_HOTSPOT_KERNEL_NAME_LEN]
                changed_hs = True
            capped_hotspots.append(item)
        if changed_hs:
            diag_copy = dict(raw_diag)
            diag_copy["cuda_graph_hotspots"] = capped_hotspots
            payload["diagnostics"] = diag_copy

    def _size() -> int:
        return len(json.dumps(payload, separators=(",", ":")).encode("utf-8"))

    if _size() <= max_bytes:
        return payload

    diagnostics = dict(payload.get("diagnostics") or {})
    diagnostics["truncated"] = True
    payload["diagnostics"] = diagnostics

    workloads = list(payload.get("workloads") or [])
    definitions = list(payload.get("definitions") or [])
    bottlenecks = list(payload.get("bottlenecks") or [])

    workloads.sort(
        key=lambda w: (
            -float(w.get("observed_duration_us", 0.0)),
            str(w.get("uuid", "")),
        )
    )
    payload["workloads"] = workloads

    def _drop_tail_definition() -> None:
        nonlocal workloads, bottlenecks
        removed_def = definitions.pop()
        removed_name = removed_def.get("name")
        removed_key = removed_def.get("local_kernel_key")
        payload["definitions"] = definitions
        workloads = [
            w
            for w in workloads
            if w.get("definition") != removed_name
            and (not removed_key or w.get("local_kernel_key") != removed_key)
        ]
        payload["workloads"] = workloads
        if removed_key:
            bottlenecks = [
                b for b in bottlenecks if b.get("local_kernel_key") != removed_key
            ]
        elif len(bottlenecks) > len(definitions):
            bottlenecks = bottlenecks[: len(definitions)]
        payload["bottlenecks"] = bottlenecks

    # 1. Drop tail definitions and bottlenecks beyond rank 16 with their workloads.
    while len(definitions) > _MAX_TOP_RANKED_DEFINITIONS and _size() > max_bytes:
        _drop_tail_definition()

    # 2. Trim excess workloads (lowest duration first) while keeping >= 1 workload per retained definition.
    retained_names = {
        str(d.get("name")) for d in definitions if isinstance(d, dict) and d.get("name")
    }
    wl_counts: dict[str, int] = {}
    for w in workloads:
        dname = str(w.get("definition") or "")
        if dname:
            wl_counts[dname] = wl_counts.get(dname, 0) + 1

    while _size() > max_bytes:
        evict_idx = -1
        for idx in range(len(workloads) - 1, -1, -1):
            dname = str(workloads[idx].get("definition") or "")
            if dname not in retained_names or wl_counts.get(dname, 0) > 1:
                evict_idx = idx
                break
        if evict_idx < 0:
            break
        removed_wl = workloads.pop(evict_idx)
        dname = str(removed_wl.get("definition") or "")
        if dname in wl_counts:
            wl_counts[dname] -= 1
        payload["workloads"] = workloads

    # 3. Drop lowest-ranked definitions and bottlenecks down to 1 with their workloads.
    while len(definitions) > 1 and _size() > max_bytes:
        _drop_tail_definition()

    # 4. Trim cuda_graph_hotspots down to [].
    hotspots = list(diagnostics.get("cuda_graph_hotspots") or [])
    while hotspots and _size() > max_bytes:
        hotspots.pop()
        diagnostics["cuda_graph_hotspots"] = hotspots

    # 5. Last resort: trim remaining workloads.
    while workloads and _size() > max_bytes:
        workloads.pop()
        payload["workloads"] = workloads

    return payload


def _positive_int(val: Any) -> int:
    if isinstance(val, bool):
        return 0
    try:
        n = int(val)
    except (TypeError, ValueError):
        return 0
    return max(0, n)


def _synthesize_input_dims_from_model_dims(
    op_family: str,
    model_dims: dict[str, Any] | None,
) -> tuple[list[list[int]], list[str]]:
    """Derive static per-GPU tensor dims for CUDA-graph GPU kernels using engine model_config."""
    if not isinstance(model_dims, dict) or not model_dims:
        return [], []
    num_tokens = max(1, _positive_int(model_dims.get("num_tokens")))
    raw_dtype = model_dims.get("dtype")
    dtypes = _normalize_dtypes([raw_dtype]) if raw_dtype else []

    hidden_size = _positive_int(model_dims.get("hidden_size"))
    intermediate_local = _positive_int(model_dims.get("intermediate_size_local"))
    head_dim = _positive_int(model_dims.get("head_dim"))
    num_heads_local = _positive_int(model_dims.get("num_heads_local"))
    num_experts_local = _positive_int(model_dims.get("num_experts_local"))
    vocab_size_local = _positive_int(model_dims.get("vocab_size_local"))

    if op_family in (
        "rmsnorm",
        "layernorm",
        "moe_permute_unpermute",
        "custom_elementwise_fusion",
    ):
        if hidden_size > 0:
            return [[num_tokens, hidden_size]], dtypes
        return [], []
    if op_family == "activation_quant":
        act_width = intermediate_local * 2 if intermediate_local > 0 else hidden_size
        if act_width > 0:
            return [[num_tokens, act_width]], dtypes
        return [], []
    if op_family in (
        "rope_kv_append",
        "paged_attention",
        "ragged_prefill_attention",
        "mla_local",
    ):
        if head_dim > 0:
            if num_heads_local > 0:
                return [[num_tokens, num_heads_local, head_dim]], dtypes
            return [[num_tokens, head_dim]], dtypes
        return [], []
    if op_family == "moe_router_topk":
        if num_experts_local > 0:
            return [[num_tokens, num_experts_local]], dtypes
        return [], []
    if op_family in ("sampling_logits", "speculative_verify"):
        if vocab_size_local > 0:
            return [[num_tokens, vocab_size_local]], dtypes
        return [], []
    return [], []


def distill_trace_events(
    events: list[dict[str, Any]],
    *,
    engine: str,
    engine_version: str = "0.0.0",
    manufacturer: str = "nvidia",
    architecture: str = "unknown",
    steps_captured: int = 0,
    raw_bytes_staged: int = 0,
    wall_clock_seconds: float = 0.0,
    max_payload_bytes: int = PROBE_MAX_PAYLOAD_BYTES,
    model_dims: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Distill profiler events into privacy-safe FlashInfer Trace Definition + Workload JSON."""
    filtered_excluded_count = 0
    unclassified_count = 0
    cuda_graph_hotspots: dict[str, dict[str, Any]] = {}
    grouped_defs: dict[str, dict[str, Any]] = {}
    grouped_workloads: dict[
        tuple[str, tuple[tuple[str, int], ...]], dict[str, Any]
    ] = {}
    total_gpu_us = 0.0

    norm_mfr = (manufacturer or "unknown").strip().lower() or "unknown"
    norm_arch = normalize_arch(norm_mfr, architecture) or "unknown"

    for raw in events:
        ev = _normalize_trace_event(raw)
        if ev is None:
            continue
        total_gpu_us += ev["dur_us"]
        name = ev["name"]

        if is_excluded_operator(name):
            filtered_excluded_count += 1
            continue

        capped_name = name[:_MAX_HOTSPOT_KERNEL_NAME_LEN]
        if ev["is_cuda_graph"]:
            hs = cuda_graph_hotspots.setdefault(
                capped_name,
                {
                    "kernel_name": capped_name,
                    "total_duration_us": 0.0,
                    "call_count": 0,
                },
            )
            hs["total_duration_us"] += ev["dur_us"]
            hs["call_count"] += ev["call_count"]

        op_family = classify_op_family(name)
        if op_family is None:
            unclassified_count += 1
            continue

        eff_input_dims = ev["input_dims"]
        eff_input_dtypes = ev["input_dtypes"]
        eff_raw_out_dtypes = ev["output_dtypes"]
        if not eff_input_dims:
            # CUDA-graph kernel launch without aten input shapes: record hotspot, and
            # synthesize static per-GPU dims from engine model_config when available.
            hs = cuda_graph_hotspots.setdefault(
                capped_name,
                {
                    "kernel_name": capped_name,
                    "total_duration_us": 0.0,
                    "call_count": 0,
                },
            )
            hs["total_duration_us"] += ev["dur_us"]
            hs["call_count"] += ev["call_count"]
            synth_dims, synth_dtypes = _synthesize_input_dims_from_model_dims(
                op_family, model_dims
            )
            if not synth_dims:
                continue
            eff_input_dims = synth_dims
            if synth_dtypes and not ev.get("has_explicit_dtypes"):
                eff_input_dtypes = synth_dtypes
                eff_raw_out_dtypes = list(synth_dtypes[:1])

        const_axes, var_axes, in_shape_map, out_shape_map = _extract_shapes_and_axes(
            op_family, eff_input_dims, ev["constants"]
        )
        eff_output_dtypes: list[str] = []
        for idx, out_name in enumerate(out_shape_map):
            if out_name == "indices":
                eff_output_dtypes.append("int64")
            elif idx < len(eff_raw_out_dtypes):
                eff_output_dtypes.append(eff_raw_out_dtypes[idx])
            else:
                eff_output_dtypes.append(eff_raw_out_dtypes[0])

        local_key = format_local_kernel_key(
            engine=engine,
            engine_version=engine_version,
            manufacturer=norm_mfr,
            arch=norm_arch,
            op_family=op_family,
            const_axes=const_axes,
            input_dtypes=eff_input_dtypes,
            output_dtypes=eff_output_dtypes,
            constants=ev["constants"],
        )
        sig16 = local_key.rsplit("/", 1)[-1]
        def_name = f"{op_family}_{sig16}"

        if local_key not in grouped_defs:
            axes_spec: dict[str, dict[str, Any]] = {
                k: {"type": "const", "value": int(v)}
                for k, v in sorted(const_axes.items())
            }
            for vk in sorted(var_axes.keys()):
                axes_spec[vk] = {"type": "var"}

            inputs_spec: dict[str, dict[str, Any]] = {}
            for idx, (in_name, dims_ref) in enumerate(in_shape_map.items()):
                dtype = (
                    eff_input_dtypes[idx]
                    if idx < len(eff_input_dtypes)
                    else eff_input_dtypes[0]
                )
                inputs_spec[in_name] = {"shape": dims_ref, "dtype": dtype}

            outputs_spec: dict[str, dict[str, Any]] = {}
            for idx, (out_name, dims_ref) in enumerate(out_shape_map.items()):
                outputs_spec[out_name] = {
                    "shape": dims_ref,
                    "dtype": eff_output_dtypes[idx],
                }

            grouped_defs[local_key] = {
                "definition": {
                    "name": def_name,
                    "op_type": op_family,
                    "local_kernel_key": local_key,
                    "axes": axes_spec,
                    "inputs": inputs_spec,
                    "outputs": outputs_spec,
                    "reference": _OP_REFERENCES.get(op_family, "def run(x): return x"),
                    "tags": [canonical_engine_name(engine), op_family],
                },
                "local_kernel_key": local_key,
                "op_family": op_family,
                "gpu_time_us": 0.0,
                "call_count": 0,
                "input_names": list(inputs_spec.keys()),
            }

        entry = grouped_defs[local_key]
        entry["gpu_time_us"] += ev["dur_us"]
        entry["call_count"] += ev["call_count"]

        var_key = tuple(sorted((k, int(v)) for k, v in var_axes.items()))
        wl_key = (local_key, var_key)
        if wl_key not in grouped_workloads:
            wl_hash = hashlib.sha256(
                json.dumps(
                    {"definition": def_name, "axes": dict(var_key)},
                    sort_keys=True,
                    separators=(",", ":"),
                ).encode("utf-8")
            ).hexdigest()[:16]
            grouped_workloads[wl_key] = {
                "uuid": f"wl-{wl_hash}",
                "definition": def_name,
                "local_kernel_key": local_key,
                "axes": dict(var_key),
                "inputs": {
                    in_name: {"type": "random"} for in_name in entry["input_names"]
                },
                "observed_duration_us": 0.0,
                "call_count": 0,
            }
        wl = grouped_workloads[wl_key]
        wl["observed_duration_us"] += ev["dur_us"]
        wl["call_count"] += ev["call_count"]

    ranked_groups = sorted(
        grouped_defs.values(),
        key=lambda item: (-float(item["gpu_time_us"]), item["local_kernel_key"]),
    )
    definitions = [item["definition"] for item in ranked_groups]
    bottlenecks: list[dict[str, Any]] = []
    for item in ranked_groups:
        share = (
            round((float(item["gpu_time_us"]) / total_gpu_us) * 100.0, 2)
            if total_gpu_us > 0.0
            else 0.0
        )
        bottlenecks.append(
            {
                "local_kernel_key": item["local_kernel_key"],
                "op_family": item["op_family"],
                "gpu_time_us": round(float(item["gpu_time_us"]), 3),
                "gpu_time_share_percent": share,
                "call_count": int(item["call_count"]),
            }
        )

    workloads = sorted(
        grouped_workloads.values(),
        key=lambda w: (-float(w["observed_duration_us"]), str(w["uuid"])),
    )
    for w in workloads:
        w["observed_duration_us"] = round(float(w["observed_duration_us"]), 3)

    hotspots = sorted(
        cuda_graph_hotspots.values(),
        key=lambda h: (-float(h["total_duration_us"]), str(h["kernel_name"])),
    )[:32]
    for h in hotspots:
        h["total_duration_us"] = round(float(h["total_duration_us"]), 3)

    payload: dict[str, Any] = {
        "schema_version": "flashinfer-trace/v1",
        "engine": canonical_engine_name(engine),
        "engine_version": normalize_engine_version(engine_version),
        "manufacturer": norm_mfr,
        "architecture": norm_arch,
        "definitions": definitions,
        "workloads": workloads,
        "bottlenecks": bottlenecks,
        "diagnostics": {
            "steps_captured": int(steps_captured),
            "filtered_excluded_count": int(filtered_excluded_count),
            "unclassified_count": int(unclassified_count),
            "raw_bytes_staged": int(raw_bytes_staged),
            "wall_clock_seconds": round(float(wall_clock_seconds), 4),
            "cuda_graph_hotspots": hotspots,
            "truncated": False,
        },
    }
    return _enforce_payload_budget(payload, max_bytes=max_payload_bytes)


def load_chrome_trace_events(trace_path: Path) -> list[dict[str, Any]]:
    """Load Chrome trace events from a .json or .json.gz file safely."""
    if trace_path.suffix == ".gz":
        with gzip.open(trace_path, "rt", encoding="utf-8") as fh:
            data = json.load(fh)
    else:
        with trace_path.open("r", encoding="utf-8") as fh:
            data = json.load(fh)
    if isinstance(data, dict):
        raw_events = data.get("traceEvents") or data.get("events") or []
        return [ev for ev in raw_events if isinstance(ev, dict)]
    if isinstance(data, list):
        return [ev for ev in data if isinstance(ev, dict)]
    return []


def select_rank0_trace_file(probe_dir: Path) -> Path | None:
    """Select TP rank 0 trace file (*-TP-0*) if present, else first trace JSON file."""
    if not probe_dir.is_dir():
        return None
    candidates: list[Path] = []
    for entry in sorted(probe_dir.rglob("*")):
        if not entry.is_file() or entry.is_symlink():
            continue
        lname = entry.name.lower()
        if lname.endswith((".trace.json.gz", ".trace.json", ".json.gz", ".json")):
            candidates.append(entry)
    if not candidates:
        return None
    for path in candidates:
        upper = path.name.upper()
        if (
            "-TP-0" in upper
            or "TP_0" in upper
            or "RANK-0" in upper
            or "RANK_0" in upper
        ):
            return path
    return candidates[0]


def distill_probe_directory(
    probe_dir: Path,
    *,
    engine: str,
    engine_version: str = "0.0.0",
    manufacturer: str = "nvidia",
    architecture: str = "unknown",
    steps_captured: int = 0,
    wall_clock_seconds: float = 0.0,
    max_payload_bytes: int = PROBE_MAX_PAYLOAD_BYTES,
    model_dims: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Distill rank-0 trace file from probe_dir into FlashInfer Trace JSON."""
    raw_bytes = directory_size_bytes(probe_dir)
    trace_file = select_rank0_trace_file(probe_dir)
    events = load_chrome_trace_events(trace_file) if trace_file is not None else []
    return distill_trace_events(
        events,
        engine=engine,
        engine_version=engine_version,
        manufacturer=manufacturer,
        architecture=architecture,
        steps_captured=steps_captured,
        raw_bytes_staged=raw_bytes,
        wall_clock_seconds=wall_clock_seconds,
        max_payload_bytes=max_payload_bytes,
        model_dims=model_dims,
    )


def validate_probe_auth(
    headers: dict[str, str] | list[tuple[bytes, bytes]] | None,
    *,
    expected_token: str | None = None,
) -> None:
    """Enforce optional shared-secret / bearer-token gate when GPUSTACK_PROBE_AUTH_TOKEN is configured."""
    token = (
        expected_token
        if expected_token is not None
        else os.environ.get(ENV_PROBE_AUTH_TOKEN, "")
    ).strip()
    if not token:
        return

    norm_headers: dict[str, str] = {}
    if isinstance(headers, dict):
        for k, v in headers.items():
            norm_headers[str(k).lower()] = str(v)
    elif isinstance(headers, list):
        for k_b, v_b in headers:
            norm_headers[k_b.decode("latin1").lower()] = v_b.decode("latin1")

    provided = ""
    auth_val = norm_headers.get("authorization", "").strip()
    if auth_val.lower().startswith("bearer "):
        provided = auth_val[7:].strip()
    if not provided:
        provided = norm_headers.get("x-gpustack-probe-token", "").strip()

    if not provided or not hmac.compare_digest(provided, token):
        raise UnauthorizedError


@dataclass
class _ProbeRecord:
    probe_id: str | None = None
    state: str = "idle"
    steps: int = 0
    started_at: float | None = None
    finished_at: float | None = None
    error: str | None = None
    error_code: str | None = None
    payload: dict[str, Any] | None = None
    diagnostics: dict[str, Any] = field(default_factory=dict)
    staging_dir: Path | None = None


class ProbeManager:
    """Manages /gpustack/v1/probe short-window profiling, abuse controls, and /dev/shm cleanup."""

    def __init__(
        self,
        *,
        backend: Any,
        latch: Any,
        staging_root: Path | str | None = None,
        cooldown_seconds: float | None = None,
        wall_clock_cap_seconds: float = PROBE_WALL_CLOCK_CAP_SECONDS,
        shm_size_override: int | None = None,
        auth_token: str | None = None,
        clock: Callable[[], float] | None = None,
        sweep_on_init: bool = True,
    ) -> None:
        self._backend = backend
        self._latch = latch
        raw_root = (
            str(staging_root)
            if staging_root is not None
            else os.environ.get(ENV_PROBE_SHM_DIR, DEFAULT_PROBE_SHM_DIR)
        )
        self._staging_root = Path(raw_root)
        self._cooldown_override = cooldown_seconds
        self._wall_clock_cap_seconds = float(wall_clock_cap_seconds)
        self._shm_size_override = shm_size_override
        self._auth_token_override = auth_token
        self._clock = clock or time.monotonic
        self._record = _ProbeRecord()
        self._last_started_monotonic: float | None = None
        self._active_task: asyncio.Task[None] | None = None
        self._state_lock: asyncio.Lock | None = None
        self._state_lock_loop: asyncio.AbstractEventLoop | None = None
        self._swept_stale_dirs: list[str] = []
        if sweep_on_init:
            self._swept_stale_dirs = sweep_stale_probe_dirs(self._staging_root)

    def _get_lock(self) -> asyncio.Lock:
        loop = asyncio.get_running_loop()
        if self._state_lock is None or self._state_lock_loop is not loop:
            self._state_lock = asyncio.Lock()
            self._state_lock_loop = loop
        return self._state_lock

    @property
    def staging_root(self) -> Path:
        return self._staging_root

    @property
    def swept_stale_dirs(self) -> list[str]:
        return list(self._swept_stale_dirs)

    def _effective_cooldown(self) -> float:
        if self._cooldown_override is not None:
            return max(0.0, float(self._cooldown_override))
        raw = os.environ.get(ENV_PROBE_MIN_COOLDOWN_SECONDS, "").strip()
        if raw:
            try:
                val = float(raw)
                if val >= 0.0:
                    return val
            except ValueError:
                pass
        return DEFAULT_PROBE_MIN_COOLDOWN_SECONDS

    def check_auth(
        self, headers: dict[str, str] | list[tuple[bytes, bytes]] | None
    ) -> None:
        validate_probe_auth(headers, expected_token=self._auth_token_override)

    def _build_response(self) -> dict[str, Any]:
        snap = self._latch.snapshot()
        rec = self._record
        out: dict[str, Any] = {
            "boot_id": snap.boot_id,
            "state": rec.state,
            "probe_id": rec.probe_id,
            "steps": rec.steps,
        }
        if rec.error is not None:
            out["error"] = rec.error
        if rec.error_code is not None:
            out["error_code"] = rec.error_code
        if rec.payload is not None:
            placeholder = dict(out)
            placeholder["payload"] = None
            overhead_bytes = (
                len(json.dumps(placeholder, separators=(",", ":")).encode("utf-8")) - 4
            )
            payload_budget = max(256, PROBE_MAX_PAYLOAD_BYTES - overhead_bytes)
            rec.payload = _enforce_payload_budget(
                dict(rec.payload), max_bytes=payload_budget
            )
            rec.diagnostics = dict(rec.payload.get("diagnostics") or {})
            out["payload"] = rec.payload
        else:
            out["diagnostics"] = dict(rec.diagnostics)
        return out

    async def get_probe(self) -> tuple[int, dict[str, Any]]:
        if self._record.state == "running" and self._record.probe_id is not None:
            await self._poll_backend_once()
        return 200, self._build_response()

    async def _poll_backend_once(self) -> None:
        rec = self._record
        if rec.state != "running" or rec.probe_id is None:
            return
        collect_fn = getattr(self._backend, "collect_probe", None)
        if not callable(collect_fn):
            return
        try:
            res = collect_fn(
                {
                    "boot_id": self._latch.boot_id,
                    "probe_id": rec.probe_id,
                    "output_dir": str(rec.staging_dir) if rec.staging_dir else "",
                    "max_shm_bytes": compute_shm_budget_bytes(
                        self._staging_root, shm_size_override=self._shm_size_override
                    ),
                }
            )
            if inspect.isawaitable(res):
                res = await res
            if isinstance(res, dict):
                self._apply_backend_result(res)
        except Exception as exc:
            self._finish_failed(rec.probe_id, "probe_collect_failed", str(exc))

    def _apply_backend_result(self, res: dict[str, Any]) -> None:
        rec = self._record
        status = str(res.get("status") or "").lower()
        if status == "completed":
            rec.state = "completed"
            rec.finished_at = self._clock()
            if isinstance(res.get("payload"), dict):
                rec.payload = _enforce_payload_budget(dict(res["payload"]))
                rec.diagnostics = dict(rec.payload.get("diagnostics") or {})
            elif isinstance(res.get("diagnostics"), dict):
                rec.diagnostics = dict(res["diagnostics"])
            if rec.staging_dir is not None:
                purge_probe_dir(rec.staging_dir)
        elif status == "aborted":
            rec.state = "aborted"
            rec.finished_at = self._clock()
            rec.error_code = str(res.get("error_code") or "probe_aborted")
            rec.error = str(res.get("message") or "Probe aborted")
            if rec.staging_dir is not None:
                purge_probe_dir(rec.staging_dir)
        elif status == "failed":
            rec.state = "failed"
            rec.finished_at = self._clock()
            rec.error_code = str(res.get("error_code") or "probe_failed")
            rec.error = str(res.get("message") or "Probe failed")
            if rec.staging_dir is not None:
                purge_probe_dir(rec.staging_dir)

    def _finish_failed(self, probe_id: str, error_code: str, message: str) -> None:
        rec = self._record
        if rec.probe_id != probe_id:
            return
        rec.state = "failed"
        rec.finished_at = self._clock()
        rec.error_code = error_code
        rec.error = message
        if rec.staging_dir is not None:
            purge_probe_dir(rec.staging_dir)

    async def _abort_backend(self, probe_id: str, staging_dir: Path | None) -> None:
        abort_fn = getattr(self._backend, "abort_probe", None)
        if callable(abort_fn):
            try:
                res = abort_fn(
                    {
                        "boot_id": self._latch.boot_id,
                        "probe_id": probe_id,
                        "output_dir": str(staging_dir) if staging_dir else "",
                    }
                )
                if inspect.isawaitable(res):
                    await res
            except Exception:
                LOGGER.debug(
                    "Backend abort_probe failed for %s", probe_id, exc_info=True
                )

    async def _run_probe_lifecycle(
        self,
        probe_id: str,
        staging_dir: Path,
        max_shm_bytes: int,
        started_monotonic: float,
    ) -> None:
        try:
            while self._record.state == "running" and self._record.probe_id == probe_id:
                elapsed = self._clock() - started_monotonic
                if elapsed > self._wall_clock_cap_seconds:
                    await self._abort_backend(probe_id, staging_dir)
                    if (
                        self._record.probe_id == probe_id
                        and self._record.state == "running"
                    ):
                        self._record.state = "failed"
                        self._record.finished_at = self._clock()
                        self._record.error_code = "probe_timeout"
                        self._record.error = f"Probe exceeded {self._wall_clock_cap_seconds}s wall-clock cap"
                        self._record.diagnostics["wall_clock_seconds"] = round(
                            elapsed, 4
                        )
                    return

                staged_bytes = await asyncio.to_thread(
                    directory_size_bytes, staging_dir
                )
                if staged_bytes > max_shm_bytes:
                    await self._abort_backend(probe_id, staging_dir)
                    if (
                        self._record.probe_id == probe_id
                        and self._record.state == "running"
                    ):
                        self._record.state = "failed"
                        self._record.finished_at = self._clock()
                        self._record.error_code = "probe_shm_budget_exceeded"
                        self._record.error = f"Probe staging exceeded {max_shm_bytes} byte ceiling ({staged_bytes} bytes)"
                        self._record.diagnostics["raw_bytes_staged"] = staged_bytes
                    return

                await self._poll_backend_once()
                if self._record.state != "running":
                    return
                await asyncio.sleep(0.02)
        except asyncio.CancelledError:
            await self._abort_backend(probe_id, staging_dir)
            if self._record.probe_id == probe_id and self._record.state == "running":
                self._record.state = "aborted"
                self._record.finished_at = self._clock()
            raise
        finally:
            await asyncio.to_thread(purge_probe_dir, staging_dir)

    async def start_probe(self, body: Any) -> tuple[int, dict[str, Any]]:
        req = ProbeRequest.from_dict(body)
        async with self._get_lock():
            self._latch.check_expected_boot_id(req.expected_boot_id)
            snap = self._latch.snapshot()

            _, engine_paused, _ = await self._backend.check_live()
            if snap.paused or engine_paused:
                raise RolePausedError(boot_id=snap.boot_id)

            if self._record.state == "running" and self._record.probe_id is not None:
                raise ProbeInProgressError(
                    boot_id=snap.boot_id, probe_id=self._record.probe_id
                )

            now = self._clock()
            cooldown = self._effective_cooldown()
            if (
                self._last_started_monotonic is not None
                and cooldown > 0.0
                and (now - self._last_started_monotonic) < cooldown
            ):
                retry_after = cooldown - (now - self._last_started_monotonic)
                raise ProbeCooldownError(
                    boot_id=snap.boot_id,
                    retry_after_seconds=retry_after,
                    cooldown_seconds=cooldown,
                )

            start_fn = getattr(self._backend, "start_probe", None)
            if not callable(start_fn):
                raise UnsupportedEngineVersionError(
                    f"Engine backend {getattr(self._backend, 'engine', 'unknown')!r} does not support probing"
                )

            probe_id = str(uuid.uuid4())
            staging_dir = self._staging_root / probe_id
            max_shm_bytes = compute_shm_budget_bytes(
                self._staging_root, shm_size_override=self._shm_size_override
            )
            staging_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
            with contextlib.suppress(OSError):
                os.chmod(staging_dir, 0o700)

            spec = {
                "boot_id": snap.boot_id,
                "probe_id": probe_id,
                "steps": req.steps,
                "output_dir": str(staging_dir),
                "max_shm_bytes": max_shm_bytes,
                "wall_clock_cap_seconds": self._wall_clock_cap_seconds,
                "engine_version": req.engine_version or "0.0.0",
                "manufacturer": req.manufacturer or "nvidia",
                "architecture": req.architecture or "unknown",
                "metadata": dict(req.metadata),
            }

            try:
                start_res = start_fn(spec)
                if inspect.isawaitable(start_res):
                    start_res = await start_res
            except SentinelError:
                purge_probe_dir(staging_dir)
                raise
            except Exception as exc:
                purge_probe_dir(staging_dir)
                raise SentinelError(
                    500, "probe_start_failed", f"Failed to start probe: {exc}"
                ) from exc

            self._last_started_monotonic = now
            self._record = _ProbeRecord(
                probe_id=probe_id,
                state="running",
                steps=req.steps,
                started_at=now,
                staging_dir=staging_dir,
                diagnostics={
                    "max_shm_bytes": max_shm_bytes,
                    "wall_clock_cap_seconds": self._wall_clock_cap_seconds,
                },
            )

            if isinstance(start_res, dict) and str(
                start_res.get("status") or ""
            ).lower() in (
                "completed",
                "failed",
                "aborted",
            ):
                try:
                    self._apply_backend_result(start_res)
                finally:
                    purge_probe_dir(staging_dir)
                return 200, self._build_response()

            # Run bounded background watcher so GET /gpustack/v1/probe and DELETE /gpustack/v1/probe work asynchronously.
            self._active_task = asyncio.create_task(
                self._run_probe_lifecycle(probe_id, staging_dir, max_shm_bytes, now)
            )
            return 200, self._build_response()

    async def delete_probe(self) -> tuple[int, dict[str, Any]]:
        async with self._get_lock():
            rec = self._record
            active_task = self._active_task
            self._active_task = None
            if active_task is not None and not active_task.done():
                active_task.cancel()
                try:
                    await active_task
                except asyncio.CancelledError:
                    pass
                except Exception:
                    pass

            if rec.probe_id is not None and rec.state == "running":
                await self._abort_backend(rec.probe_id, rec.staging_dir)
                rec.state = "aborted"
                rec.finished_at = self._clock()
            if rec.staging_dir is not None:
                purge_probe_dir(rec.staging_dir)
            return 200, self._build_response()
