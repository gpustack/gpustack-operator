"""Lazy startup kernel dispatch loader reading /var/lib/gpustack/kernels/dispatch-manifest.json."""

from __future__ import annotations

import hashlib
import importlib.util
import inspect
import json
import logging
import os
import re
import stat
import threading
from collections.abc import Callable
from dataclasses import dataclass, field
from functools import wraps
from pathlib import Path
from typing import Any

from gpustack_engine_sentinel.probe import (
    ALLOWED_OP_FAMILIES,
    canonical_engine_name,
    classify_op_family,
    is_excluded_operator,
    normalize_arch,
    normalize_engine_version,
    parse_local_kernel_key,
)
from gpustack_engine_sentinel.protocol import (
    DEFAULT_KERNEL_ARTIFACT_DIR,
    DISPATCH_MANIFEST_SCHEMA_VERSION,
    ENV_KERNEL_ARTIFACT_DIR,
)

LOGGER = logging.getLogger(__name__)
UNSUPPORTED_KERNEL_ARTIFACT_ERROR = "unsupported_kernel_artifact"

_NVIDIA_DOT_COMCAP_RE = re.compile(r"^(\d+)\.(\d+)([a-z]*)$")
_NVIDIA_SM_OR_COMPUTE_RE = re.compile(r"^(?:sm|compute)_(\d+)([a-z]*)$")
_DEF_NUMERIC_SUFFIX_RE = re.compile(r"_(\d{1,6})$")
_CAMEL_TO_SNAKE_1 = re.compile(r"(.)([A-Z][a-z]+)")
_CAMEL_TO_SNAKE_2 = re.compile(r"([a-z0-9])([A-Z])")
_DISPATCH_METHOD_NAMES: tuple[str, ...] = (
    "forward_cuda",
    "forward_hip",
    "forward_oot",
    "forward_native",
)
_DISPATCH_GUARD = threading.local()


@dataclass(frozen=True)
class LoadedKernelOverride:
    """Verified lazy kernel override entry from dispatch-manifest.json."""

    local_kernel_key: str
    op_family: str
    definition: str
    library_path: Path
    entrypoint: str
    solution_path: Path | None = None
    trace_path: Path | None = None
    const_axes: dict[str, int] = field(default_factory=dict)
    dispatchable: bool = True

    def load_callable(self) -> Callable[..., Any]:
        """Lazily import and return the override callable inside worker execution only."""
        mod_name = f"_gpustack_forged_kernel_{hashlib.sha256(str(self.library_path).encode('utf-8')).hexdigest()[:16]}"
        spec = importlib.util.spec_from_file_location(mod_name, str(self.library_path))
        if spec is None or spec.loader is None:
            raise ImportError(
                f"Cannot load forged kernel module from {self.library_path}"
            )
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        fn = getattr(mod, self.entrypoint, None)
        if not callable(fn):
            raise AttributeError(
                f"Entrypoint {self.entrypoint!r} not found or not callable in {self.library_path}"
            )
        return fn


@dataclass
class KernelLoadStatus:
    """Per-process kernel dispatch manifest load outcome."""

    configured: bool = False
    artifact_dir: str = ""
    dispatch_manifest_digest: str | None = None
    loaded_kernel_keys: list[str] = field(default_factory=list)
    skipped_kernel_keys: list[str] = field(default_factory=list)
    kernel_load_error: str | None = None
    overrides: dict[str, LoadedKernelOverride] = field(default_factory=dict)

    def to_report(self) -> dict[str, Any]:
        """Return strictly additive wire fields when kernel loading is configured, else empty dict."""
        if not self.configured:
            return {}
        out: dict[str, Any] = {
            "loaded_kernel_keys": list(self.loaded_kernel_keys),
        }
        if self.dispatch_manifest_digest:
            out["dispatch_manifest_digest"] = self.dispatch_manifest_digest
        if self.kernel_load_error:
            out["kernel_load_error"] = self.kernel_load_error
        return out


def _resolve_regular_file_under(root: Path, rel_path: str) -> Path | None:
    """Resolve a relative path strictly inside root, rejecting escapes, symlinks, and non-regular files."""
    if not isinstance(rel_path, str) or not rel_path.strip():
        return None
    raw = Path(rel_path.strip())
    if raw.is_absolute() or ".." in raw.parts:
        return None
    root_resolved = root.resolve()
    candidate = root_resolved / raw
    # Reject symlinks anywhere along the relative path.
    cur = root_resolved
    for part in raw.parts:
        cur = cur / part
        try:
            st = os.lstat(cur)
        except OSError:
            return None
        if stat.S_ISLNK(st.st_mode):
            return None
    try:
        resolved = candidate.resolve(strict=True)
    except OSError:
        return None
    if root_resolved != resolved and root_resolved not in resolved.parents:
        return None
    try:
        st = os.lstat(resolved)
    except OSError:
        return None
    if not stat.S_ISREG(st.st_mode):
        return None
    return resolved


def _split_nvidia_sm(arch: str) -> tuple[str, str]:
    m = _NVIDIA_SM_OR_COMPUTE_RE.match(arch)
    if m is None:
        return "", ""
    return f"sm_{m.group(1)}", m.group(2)


def arch_compatible(manufacturer: str | None, a: str | None, b: str | None) -> bool:
    """Check architecture compatibility matching pkg/worker/kernelkey/hardware.go:ArchCompatible."""
    manu = (manufacturer or "").strip().lower()
    na = normalize_arch(manu, a)
    nb = normalize_arch(manu, b)
    if not na or not nb:
        return na == nb
    if na == nb:
        return True
    if manu == "nvidia" or not manu:
        base_a, var_a = _split_nvidia_sm(na)
        base_b, var_b = _split_nvidia_sm(nb)
        if base_a and base_a == base_b and (var_a == "" or var_b == ""):
            return True
    return False


def _is_compatible(
    *,
    want_engine: str | None,
    want_version: str | None,
    want_manufacturer: str | None,
    want_arch: str | None,
    have_engine: str | None,
    have_version: str | None,
    have_manufacturer: str | None,
    have_arch: str | None,
) -> bool:
    if (
        want_engine
        and have_engine
        and canonical_engine_name(want_engine).lower()
        != canonical_engine_name(have_engine).lower()
    ):
        return False
    norm_want_ver: str | None = None
    if want_version is not None and (
        not isinstance(want_version, str) or want_version.strip()
    ):
        try:
            norm_want_ver = normalize_engine_version(want_version)
        except ValueError:
            return False
    norm_have_ver: str | None = None
    if have_version is not None and (
        not isinstance(have_version, str) or have_version.strip()
    ):
        try:
            norm_have_ver = normalize_engine_version(have_version)
        except ValueError:
            return False
    if norm_want_ver and norm_have_ver and norm_want_ver != norm_have_ver:
        return False
    if (
        want_manufacturer
        and have_manufacturer
        and want_manufacturer.strip().lower() != have_manufacturer.strip().lower()
    ):
        return False
    if want_arch and have_arch:
        eff_manu = (want_manufacturer or have_manufacturer or "").strip().lower()
        return arch_compatible(eff_manu, want_arch, have_arch)
    return True


def _parse_const_axes_mapping(raw: Any) -> dict[str, int]:
    if not isinstance(raw, dict):
        return {}
    out: dict[str, int] = {}
    for k, v in raw.items():
        ks = str(k).strip()
        if not ks:
            continue
        if isinstance(v, bool):
            continue
        if isinstance(v, int) and v > 0:
            out[ks] = v
        elif (
            isinstance(v, dict)
            and v.get("type") == "const"
            and isinstance(v.get("value"), int)
            and not isinstance(v.get("value"), bool)
            and int(v["value"]) > 0
        ):
            out[ks] = int(v["value"])
    return out


def _load_solution_json(sol_file: Path | None) -> dict[str, Any] | None:
    if sol_file is None:
        return None
    try:
        sol_obj = json.loads(sol_file.read_text(encoding="utf-8"))
    except Exception:
        return None
    return sol_obj if isinstance(sol_obj, dict) else None


def _is_mock_entry(entry: dict[str, Any], sol_obj: dict[str, Any] | None) -> bool:
    if entry.get("mock") is True:
        return True
    if sol_obj is not None:
        if sol_obj.get("mock") is True:
            return True
        spec_obj = sol_obj.get("spec")
        if isinstance(spec_obj, dict) and spec_obj.get("mock") is True:
            return True
    return False


def _extract_const_axes_for_entry(
    entry: dict[str, Any],
    sol_obj: dict[str, Any] | None,
    op_family: str,
    definition: str,
) -> dict[str, int]:
    for key in ("constAxes", "const_axes", "axes"):
        parsed = _parse_const_axes_mapping(entry.get(key))
        if parsed:
            return parsed

    if sol_obj is not None:
        for key in ("constAxes", "const_axes", "axes"):
            parsed = _parse_const_axes_mapping(sol_obj.get(key))
            if parsed:
                return parsed
        spec_obj = sol_obj.get("spec")
        if isinstance(spec_obj, dict):
            for key in ("constAxes", "const_axes", "axes"):
                parsed = _parse_const_axes_mapping(spec_obj.get(key))
                if parsed:
                    return parsed

    m = _DEF_NUMERIC_SUFFIX_RE.search(definition)
    if m is not None:
        dim_val = int(m.group(1))
        if 0 < dim_val < 1_000_000:
            if op_family in (
                "rmsnorm",
                "layernorm",
                "activation_quant",
                "moe_permute_unpermute",
                "custom_elementwise_fusion",
            ):
                return {"hidden_size": dim_val}
            if op_family in (
                "rope_kv_append",
                "paged_attention",
                "ragged_prefill_attention",
                "mla_local",
            ):
                return {"head_dim": dim_val}
            if op_family == "moe_router_topk":
                return {"num_experts_local": dim_val}
            if op_family in ("sampling_logits", "speculative_verify"):
                return {"vocab_size_local": dim_val}
    return {}


def load_kernel_dispatch_manifest(
    artifact_dir: Path | str | None,
    *,
    engine: str | None = None,
    engine_version: str | None = None,
    manufacturer: str | None = None,
    architecture: str | None = None,
) -> KernelLoadStatus:
    """Validate and lazily index dispatch-manifest.json without importing torch/triton/flashinfer."""
    if artifact_dir is None or not str(artifact_dir).strip():
        return KernelLoadStatus(configured=False)

    root = Path(str(artifact_dir).strip())
    status = KernelLoadStatus(configured=True, artifact_dir=str(root))

    manifest_path = _resolve_regular_file_under(root, "dispatch-manifest.json")
    if manifest_path is None:
        status.kernel_load_error = UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        return status

    try:
        raw_bytes = manifest_path.read_bytes()
        status.dispatch_manifest_digest = (
            f"sha256:{hashlib.sha256(raw_bytes).hexdigest()}"
        )
        manifest = json.loads(raw_bytes.decode("utf-8"))
    except Exception:
        status.kernel_load_error = UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        return status

    if not isinstance(manifest, dict):
        status.kernel_load_error = UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        return status

    if manifest.get("schemaVersion") != DISPATCH_MANIFEST_SCHEMA_VERSION:
        status.kernel_load_error = UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        return status

    compat = manifest.get("compatibility")
    if not isinstance(compat, dict):
        status.kernel_load_error = UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        return status

    if not _is_compatible(
        want_engine=compat.get("engine"),
        want_version=compat.get("engineVersion"),
        want_manufacturer=compat.get("manufacturer"),
        want_arch=compat.get("architecture"),
        have_engine=engine,
        have_version=engine_version,
        have_manufacturer=manufacturer,
        have_arch=architecture,
    ):
        status.kernel_load_error = UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        return status

    entries = manifest.get("entries")
    if not isinstance(entries, list):
        status.kernel_load_error = UNSUPPORTED_KERNEL_ARTIFACT_ERROR
        return status

    for entry in entries:
        if not isinstance(entry, dict):
            continue
        raw_key = entry.get("localKernelKey")
        if not isinstance(raw_key, str) or not raw_key.strip():
            continue
        try:
            parsed = parse_local_kernel_key(raw_key)
        except ValueError:
            status.skipped_kernel_keys.append(str(raw_key))
            continue

        if parsed["op_family"] not in ALLOWED_OP_FAMILIES or is_excluded_operator(
            parsed["op_family"]
        ):
            status.skipped_kernel_keys.append(raw_key)
            continue

        if not _is_compatible(
            want_engine=parsed["engine"],
            want_version=parsed["engine_version"],
            want_manufacturer=parsed["manufacturer"],
            want_arch=parsed["architecture"],
            have_engine=engine,
            have_version=engine_version,
            have_manufacturer=manufacturer,
            have_arch=architecture,
        ):
            status.skipped_kernel_keys.append(raw_key)
            continue

        lib_rel = entry.get("libraryPath")
        lib_file = _resolve_regular_file_under(root, str(lib_rel or ""))
        if lib_file is None:
            status.skipped_kernel_keys.append(raw_key)
            continue

        sol_rel = entry.get("solutionPath")
        sol_file = (
            _resolve_regular_file_under(root, str(sol_rel))
            if isinstance(sol_rel, str) and sol_rel.strip()
            else None
        )
        if sol_rel and sol_file is None:
            status.skipped_kernel_keys.append(raw_key)
            continue

        trace_rel = entry.get("tracePath")
        trace_file = (
            _resolve_regular_file_under(root, str(trace_rel))
            if isinstance(trace_rel, str) and trace_rel.strip()
            else None
        )
        if trace_rel and trace_file is None:
            status.skipped_kernel_keys.append(raw_key)
            continue

        entrypoint = str(entry.get("entrypoint") or "run").strip() or "run"
        definition_name = str(entry.get("definition") or parsed["op_family"])
        sol_obj = _load_solution_json(sol_file)
        const_axes = _extract_const_axes_for_entry(
            entry, sol_obj, parsed["op_family"], definition_name
        )
        dispatchable = not _is_mock_entry(entry, sol_obj)
        override = LoadedKernelOverride(
            local_kernel_key=raw_key.strip(),
            op_family=parsed["op_family"],
            definition=definition_name,
            library_path=lib_file,
            entrypoint=entrypoint,
            solution_path=sol_file,
            trace_path=trace_file,
            const_axes=const_axes,
            dispatchable=dispatchable,
        )
        status.overrides[raw_key.strip()] = override
        status.loaded_kernel_keys.append(raw_key.strip())

    status.loaded_kernel_keys = sorted(set(status.loaded_kernel_keys))
    status.skipped_kernel_keys = sorted(set(status.skipped_kernel_keys))
    return status


class KernelDispatchRegistry:
    """Process-local registry for verified forged kernel overrides."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._status = KernelLoadStatus(configured=False)
        self._registered_targets: set[str] = set()
        self._disabled_keys: set[str] = set()

    @property
    def status(self) -> KernelLoadStatus:
        with self._lock:
            return self._status

    def load_from_env(
        self,
        *,
        engine: str | None = None,
        engine_version: str | None = None,
        manufacturer: str | None = None,
        architecture: str | None = None,
        force: bool = False,
    ) -> KernelLoadStatus:
        artifact_dir = os.environ.get(ENV_KERNEL_ARTIFACT_DIR, "").strip()
        if not artifact_dir:
            with self._lock:
                if force:
                    self._status = KernelLoadStatus(configured=False)
                    self._disabled_keys.clear()
                return self._status

        with self._lock:
            if (
                not force
                and self._status.configured
                and self._status.artifact_dir == artifact_dir
            ):
                return self._status

        resolved_engine = (
            engine or os.environ.get("GPUSTACK_ENGINE_SENTINEL_ENGINE") or None
        )
        resolved_version = (
            engine_version or os.environ.get("GPUSTACK_ENGINE_VERSION") or None
        )
        resolved_mfr = (
            manufacturer or os.environ.get("GPUSTACK_HARDWARE_MANUFACTURER") or None
        )
        resolved_arch = (
            architecture or os.environ.get("GPUSTACK_HARDWARE_ARCHITECTURE") or None
        )

        loaded = load_kernel_dispatch_manifest(
            artifact_dir,
            engine=resolved_engine,
            engine_version=resolved_version,
            manufacturer=resolved_mfr,
            architecture=resolved_arch,
        )
        with self._lock:
            self._status = loaded
            if force:
                self._disabled_keys.clear()
            return self._status

    def get_override_for_family(self, op_family: str) -> LoadedKernelOverride | None:
        with self._lock:
            for override in self._status.overrides.values():
                if (
                    override.op_family == op_family
                    and override.dispatchable
                    and override.local_kernel_key not in self._disabled_keys
                ):
                    return override
            return None

    def is_override_disabled(self, local_kernel_key: str) -> bool:
        with self._lock:
            return local_kernel_key in self._disabled_keys

    def record_runtime_error(
        self,
        local_kernel_key: str,
        error_code: str = UNSUPPORTED_KERNEL_ARTIFACT_ERROR,
    ) -> bool:
        """Disable a failing override and record kernel_load_error; returns True on first failure."""
        with self._lock:
            first_failure = local_kernel_key not in self._disabled_keys
            self._disabled_keys.add(local_kernel_key)
            if self._status.configured and not self._status.kernel_load_error:
                self._status.kernel_load_error = error_code
            return first_failure

    def mark_target_hooked(self, target_name: str) -> None:
        with self._lock:
            self._registered_targets.add(target_name)

    @property
    def registered_targets(self) -> frozenset[str]:
        with self._lock:
            return frozenset(self._registered_targets)

    def reset(self) -> None:
        with self._lock:
            self._status = KernelLoadStatus(configured=False)
            self._registered_targets.clear()
            self._disabled_keys.clear()


_PROCESS_REGISTRY = KernelDispatchRegistry()


def get_process_kernel_registry() -> KernelDispatchRegistry:
    return _PROCESS_REGISTRY


def reset_process_kernel_registry() -> None:
    _PROCESS_REGISTRY.reset()


def ensure_process_kernels_loaded(
    *,
    engine: str | None = None,
    engine_version: str | None = None,
    manufacturer: str | None = None,
    architecture: str | None = None,
    force: bool = False,
) -> KernelLoadStatus:
    """Load kernel manifest lazily when GPUSTACK_KERNEL_ARTIFACT_DIR is set."""
    return _PROCESS_REGISTRY.load_from_env(
        engine=engine,
        engine_version=engine_version,
        manufacturer=manufacturer,
        architecture=architecture,
        force=force,
    )


def get_process_kernel_report(
    *,
    engine: str | None = None,
    engine_version: str | None = None,
    manufacturer: str | None = None,
    architecture: str | None = None,
) -> dict[str, Any]:
    """Return additive kernel report dict for the current process, or {} if unconfigured."""
    if not os.environ.get(ENV_KERNEL_ARTIFACT_DIR, "").strip():
        status = _PROCESS_REGISTRY.status
        return status.to_report() if status.configured else {}
    status = ensure_process_kernels_loaded(
        engine=engine,
        engine_version=engine_version,
        manufacturer=manufacturer,
        architecture=architecture,
    )
    return status.to_report()


def aggregate_kernel_reports(reports: list[dict[str, Any]]) -> dict[str, Any]:
    """Aggregate per-rank kernel loader reports into a top-level summary."""
    present = [
        r
        for r in reports
        if isinstance(r, dict)
        and (
            "loaded_kernel_keys" in r
            or "dispatch_manifest_digest" in r
            or "kernel_load_error" in r
        )
    ]
    if not present:
        return {}

    key_sets = [
        set(r.get("loaded_kernel_keys") or ())
        for r in present
        if isinstance(r.get("loaded_kernel_keys"), list)
    ]
    common_keys = sorted(set.intersection(*key_sets)) if key_sets else []

    digests = [
        str(r["dispatch_manifest_digest"])
        for r in present
        if r.get("dispatch_manifest_digest")
    ]
    errors = [
        str(r["kernel_load_error"]) for r in present if r.get("kernel_load_error")
    ]

    out: dict[str, Any] = {"loaded_kernel_keys": common_keys}
    if digests:
        unique_digests = sorted(set(digests))
        if len(unique_digests) == 1:
            out["dispatch_manifest_digest"] = unique_digests[0]
        else:
            out["dispatch_manifest_digest"] = unique_digests[0]
            if not errors:
                errors.append(UNSUPPORTED_KERNEL_ARTIFACT_ERROR)
    if errors:
        out["kernel_load_error"] = errors[0]
    return out


def _safe_register_override(
    register_fn: Callable[[str, LoadedKernelOverride], Any],
    family: str,
    override: LoadedKernelOverride,
    target_name: str,
) -> None:
    try:
        register_fn(family, override)
    except Exception:
        LOGGER.warning(
            "Failed registering forged override %s on %s",
            override.local_kernel_key,
            target_name,
            exc_info=True,
        )


def _tensor_shape_tuple(obj: Any) -> tuple[int, ...] | None:
    shape = getattr(obj, "shape", None)
    if shape is None:
        return None
    try:
        dims = tuple(int(d) for d in shape)
    except (TypeError, ValueError):
        return None
    return dims


def _find_primary_tensor(
    op_family: str, args: tuple[Any, ...], kwargs: dict[str, Any]
) -> Any:
    if op_family == "rope_kv_append":
        if len(args) >= 2:
            s0 = _tensor_shape_tuple(args[0])
            s1 = _tensor_shape_tuple(args[1])
            if s1 is not None and (s0 is None or len(s0) <= 1):
                return args[1]
        if "query" in kwargs and _tensor_shape_tuple(kwargs["query"]) is not None:
            return kwargs["query"]
        if "q" in kwargs and _tensor_shape_tuple(kwargs["q"]) is not None:
            return kwargs["q"]
    for item in args:
        if _tensor_shape_tuple(item) is not None:
            return item
    for item in kwargs.values():
        if _tensor_shape_tuple(item) is not None:
            return item
    return None


def _matches_const_axes(
    override: LoadedKernelOverride,
    op_family: str,
    instance: Any,
    args: tuple[Any, ...],
    kwargs: dict[str, Any],
) -> bool:
    if not override.const_axes:
        return True
    primary = _find_primary_tensor(op_family, args, kwargs)
    shape = _tensor_shape_tuple(primary)

    for axis_name, expected in override.const_axes.items():
        ax = axis_name.lower()
        if ax in ("hidden_size", "num_experts_local", "vocab_size_local"):
            if shape:
                if shape[-1] != expected:
                    return False
            elif instance is not None:
                attr_val = getattr(instance, ax, None)
                if attr_val != expected:
                    return False
            else:
                return False
        elif ax == "head_dim":
            if shape and len(shape) >= 3:
                if shape[-1] != expected:
                    return False
            elif shape and len(shape) >= 1:
                inst_hd = (
                    getattr(
                        instance,
                        "head_size",
                        getattr(instance, "head_dim", None),
                    )
                    if instance is not None
                    else None
                )
                if shape[-1] == expected:
                    continue
                if inst_hd == expected and shape[-1] % expected == 0:
                    continue
                return False
            elif instance is not None:
                inst_hd = getattr(
                    instance, "head_size", getattr(instance, "head_dim", None)
                )
                if inst_hd != expected:
                    return False
            else:
                return False
        elif ax == "num_heads_local":
            if shape and len(shape) >= 3:
                if shape[-2] != expected:
                    return False
            elif shape and len(shape) >= 1:
                hd = override.const_axes.get("head_dim")
                if hd is None and instance is not None:
                    hd = getattr(
                        instance, "head_size", getattr(instance, "head_dim", None)
                    )
                if not (isinstance(hd, int) and hd > 0 and shape[-1] == expected * hd):
                    return False
            else:
                return False
        elif ax == "half_hidden_size":
            if shape and len(shape) >= 1:
                if max(1, shape[-1] // 2) != expected:
                    return False
            elif instance is not None and hasattr(instance, "hidden_size"):
                if max(1, int(instance.hidden_size) // 2) != expected:
                    return False
            else:
                return False
        elif ax == "top_k":
            if instance is not None and hasattr(instance, "top_k"):
                if getattr(instance, "top_k", None) != expected:
                    return False
            elif shape and len(shape) >= 1:
                if expected > shape[-1]:
                    return False
            else:
                return False
        else:
            if instance is None or getattr(instance, axis_name, None) != expected:
                return False
    return True


def _adapt_and_call_override(
    fn: Callable[..., Any],
    sig: inspect.Signature,
    op_family: str,
    instance: Any,
    args: tuple[Any, ...],
    kwargs: dict[str, Any],
) -> tuple[bool, Any]:
    pos_params = [
        p
        for p in sig.parameters.values()
        if p.kind
        in (
            inspect.Parameter.POSITIONAL_ONLY,
            inspect.Parameter.POSITIONAL_OR_KEYWORD,
        )
    ]

    if op_family == "rmsnorm":
        x = args[0] if args else kwargs.get("x")
        residual = args[1] if len(args) > 1 else kwargs.get("residual")
        if x is None:
            return False, None
        if residual is not None:
            if "residual" in sig.parameters:
                return True, fn(*args, **kwargs)
            return False, None
        weight = getattr(instance, "weight", None) if instance is not None else None
        eps = (
            getattr(instance, "variance_epsilon", getattr(instance, "eps", 1e-6))
            if instance is not None
            else 1e-6
        )
        if len(pos_params) == 1:
            return True, fn(x)
        if len(pos_params) == 2:
            if weight is None:
                return False, None
            return True, fn(x, weight)
        if len(pos_params) >= 3:
            if weight is None:
                return False, None
            return True, fn(x, weight, eps)

    if op_family == "layernorm":
        x = args[0] if args else kwargs.get("x")
        if x is None:
            return False, None
        weight = getattr(instance, "weight", None) if instance is not None else None
        bias = getattr(instance, "bias", None) if instance is not None else None
        eps = (
            getattr(instance, "eps", getattr(instance, "variance_epsilon", 1e-5))
            if instance is not None
            else 1e-5
        )
        if len(pos_params) == 1:
            return True, fn(x)
        if len(pos_params) == 2:
            if weight is None:
                return False, None
            return True, fn(x, weight)
        if len(pos_params) == 3:
            if weight is None:
                return False, None
            return True, fn(x, weight, bias)
        if len(pos_params) >= 4:
            if weight is None:
                return False, None
            return True, fn(x, weight, bias, eps)

    try:
        sig.bind(*args, **kwargs)
    except TypeError:
        return False, None
    return True, fn(*args, **kwargs)


def _make_forged_method_wrapper(
    orig_fn: Callable[..., Any],
    *,
    op_family: str,
    overrides: list[LoadedKernelOverride],
    is_static: bool,
    target_name: str,
) -> Callable[..., Any]:
    loaded_cache: dict[str, tuple[Callable[..., Any], inspect.Signature] | None] = {}

    def _try_dispatch(
        instance: Any, args: tuple[Any, ...], kwargs: dict[str, Any]
    ) -> tuple[bool, Any]:
        if getattr(_DISPATCH_GUARD, "active", False):
            return False, None
        registry = get_process_kernel_registry()
        for override in overrides:
            if not override.dispatchable or registry.is_override_disabled(
                override.local_kernel_key
            ):
                continue
            if not _matches_const_axes(override, op_family, instance, args, kwargs):
                continue
            cached = loaded_cache.get(override.local_kernel_key)
            if override.local_kernel_key not in loaded_cache:
                try:
                    fn = override.load_callable()
                    cached = (fn, inspect.signature(fn))
                except Exception:
                    first_err = registry.record_runtime_error(
                        override.local_kernel_key,
                        UNSUPPORTED_KERNEL_ARTIFACT_ERROR,
                    )
                    if first_err:
                        LOGGER.warning(
                            "Failed lazily loading forged kernel %s for %s; falling back to stock op",
                            override.local_kernel_key,
                            target_name,
                            exc_info=True,
                        )
                    cached = None
                loaded_cache[override.local_kernel_key] = cached
            if cached is None:
                continue
            fn, sig = cached
            _DISPATCH_GUARD.active = True
            try:
                ok, result = _adapt_and_call_override(
                    fn, sig, op_family, instance, args, kwargs
                )
            except Exception:
                loaded_cache[override.local_kernel_key] = None
                first_err = registry.record_runtime_error(
                    override.local_kernel_key,
                    UNSUPPORTED_KERNEL_ARTIFACT_ERROR,
                )
                if first_err:
                    LOGGER.warning(
                        "Forged kernel %s raised during %s forward call; disabling override and falling back to stock op",
                        override.local_kernel_key,
                        target_name,
                        exc_info=True,
                    )
                continue
            finally:
                _DISPATCH_GUARD.active = False
            if ok:
                return True, result
        return False, None

    if is_static:

        @wraps(orig_fn)
        def _static_wrapper(*args: Any, **kwargs: Any) -> Any:
            dispatched, result = _try_dispatch(None, args, kwargs)
            if dispatched:
                return result
            return orig_fn(*args, **kwargs)

        _static_wrapper._gpustack_forged_wrapped = True  # type: ignore[attr-defined]
        return _static_wrapper

    @wraps(orig_fn)
    def _method_wrapper(self: Any, *args: Any, **kwargs: Any) -> Any:
        dispatched, result = _try_dispatch(self, args, kwargs)
        if dispatched:
            return result
        return orig_fn(self, *args, **kwargs)

    _method_wrapper._gpustack_forged_wrapped = True  # type: ignore[attr-defined]
    return _method_wrapper


def _camel_to_snake(name: str) -> str:
    step1 = _CAMEL_TO_SNAKE_1.sub(r"\1_\2", name)
    return _CAMEL_TO_SNAKE_2.sub(r"\1_\2", step1).lower()


def _resolve_op_family_for_class(
    op_cls: type[Any], explicit_name: str | None = None
) -> str | None:
    for candidate in (
        explicit_name,
        getattr(op_cls, "name", None),
        getattr(op_cls, "__name__", None),
    ):
        if isinstance(candidate, str) and candidate.strip():
            family = classify_op_family(candidate)
            if family is not None:
                return family
            snake = _camel_to_snake(candidate.strip())
            family = classify_op_family(snake)
            if family is not None:
                return family
    return None


def _wrap_class_dispatch_methods(
    op_cls: type[Any],
    *,
    op_family: str,
    overrides: list[LoadedKernelOverride],
    target_name: str,
) -> bool:
    if not isinstance(op_cls, type) or not overrides:
        return False

    wrapped_any = False
    has_platform_method = False
    for method_name in _DISPATCH_METHOD_NAMES:
        raw_attr = op_cls.__dict__.get(method_name)
        if raw_attr is None:
            continue
        has_platform_method = True
        is_static = isinstance(raw_attr, staticmethod)
        orig_fn = raw_attr.__func__ if is_static else raw_attr
        if not callable(orig_fn) or getattr(orig_fn, "_gpustack_forged_wrapped", False):
            continue
        wrapped = _make_forged_method_wrapper(
            orig_fn,
            op_family=op_family,
            overrides=overrides,
            is_static=is_static,
            target_name=target_name,
        )
        setattr(op_cls, method_name, staticmethod(wrapped) if is_static else wrapped)
        wrapped_any = True

    if not has_platform_method:
        raw_fwd = op_cls.__dict__.get("forward")
        if raw_fwd is not None:
            is_static = isinstance(raw_fwd, staticmethod)
            orig_fn = raw_fwd.__func__ if is_static else raw_fwd
            if callable(orig_fn) and not getattr(
                orig_fn, "_gpustack_forged_wrapped", False
            ):
                wrapped = _make_forged_method_wrapper(
                    orig_fn,
                    op_family=op_family,
                    overrides=overrides,
                    is_static=is_static,
                    target_name=target_name,
                )
                op_cls.forward = (  # type: ignore[attr-defined]
                    staticmethod(wrapped) if is_static else wrapped
                )
                wrapped_any = True
    return wrapped_any


def _bind_overrides_to_module_classes(
    mod: Any,
    *,
    by_family: dict[str, list[LoadedKernelOverride]],
    target_name: str,
) -> None:
    if not by_family:
        return

    # 1. Bind any classes already registered in vLLM's op_registry.
    op_registry = getattr(mod, "op_registry", None)
    if isinstance(op_registry, dict):
        for reg_name, op_cls in list(op_registry.items()):
            if not isinstance(op_cls, type):
                continue
            family = _resolve_op_family_for_class(op_cls, explicit_name=str(reg_name))
            if family and family in by_family:
                _wrap_class_dispatch_methods(
                    op_cls,
                    op_family=family,
                    overrides=by_family[family],
                    target_name=target_name,
                )

    # 2. Wrap vLLM CustomOp.register and dispatch_forward so classes registered later are bound automatically.
    custom_op_cls = getattr(mod, "CustomOp", None)
    if isinstance(custom_op_cls, type):
        raw_register = custom_op_cls.__dict__.get("register")
        if isinstance(raw_register, classmethod) and not getattr(
            raw_register.__func__, "_gpustack_forged_wrapped", False
        ):
            orig_register_fn = raw_register.__func__

            @wraps(orig_register_fn)
            def _wrapped_register(
                cls: type[Any], name: str, *args: Any, **kwargs: Any
            ) -> Any:
                orig_decorator = orig_register_fn(cls, name, *args, **kwargs)

                def _decorator(op_cls: type[Any]) -> type[Any]:
                    registered_cls = orig_decorator(op_cls)
                    family = _resolve_op_family_for_class(
                        registered_cls, explicit_name=name
                    )
                    if family and family in by_family:
                        _wrap_class_dispatch_methods(
                            registered_cls,
                            op_family=family,
                            overrides=by_family[family],
                            target_name=target_name,
                        )
                    return registered_cls

                return _decorator

            _wrapped_register._gpustack_forged_wrapped = True  # type: ignore[attr-defined]
            custom_op_cls.register = classmethod(_wrapped_register)

        raw_dispatch = custom_op_cls.__dict__.get("dispatch_forward")
        if callable(raw_dispatch) and not getattr(
            raw_dispatch, "_gpustack_forged_wrapped", False
        ):

            @wraps(raw_dispatch)
            def _wrapped_dispatch_forward(self: Any, *args: Any, **kwargs: Any) -> Any:
                family = _resolve_op_family_for_class(type(self))
                if family and family in by_family:
                    _wrap_class_dispatch_methods(
                        type(self),
                        op_family=family,
                        overrides=by_family[family],
                        target_name=target_name,
                    )
                return raw_dispatch(self, *args, **kwargs)

            _wrapped_dispatch_forward._gpustack_forged_wrapped = True  # type: ignore[attr-defined]
            custom_op_cls.dispatch_forward = _wrapped_dispatch_forward

    # 3. Bind matching operator classes exposed directly on the module (SGLang/vLLM layer modules).
    for attr_name in dir(mod):
        if attr_name.startswith("_"):
            continue
        try:
            attr_val = getattr(mod, attr_name)
        except Exception:
            continue
        if not isinstance(attr_val, type) or attr_val is custom_op_cls:
            continue
        family = _resolve_op_family_for_class(attr_val, explicit_name=attr_name)
        if family and family in by_family:
            _wrap_class_dispatch_methods(
                attr_val,
                op_family=family,
                overrides=by_family[family],
                target_name=target_name,
            )


def _attach_overrides_to_module(
    mod: Any,
    *,
    target_name: str,
    engine: str,
    op_families: tuple[str, ...] | None = None,
) -> KernelLoadStatus:
    """Attach verified lazy override hooks onto a target module without eager heavy imports."""
    registry = get_process_kernel_registry()
    if not os.environ.get(ENV_KERNEL_ARTIFACT_DIR, "").strip():
        return registry.status

    status = ensure_process_kernels_loaded(engine=engine)
    registry.mark_target_hooked(target_name)
    if getattr(mod, "_gpustack_kernel_loader_patched", False):
        return status

    selected: dict[str, LoadedKernelOverride] = {}
    by_family: dict[str, list[LoadedKernelOverride]] = {}
    for override in status.overrides.values():
        if not override.dispatchable:
            continue
        if op_families is None or override.op_family in op_families:
            selected[override.op_family] = override
            by_family.setdefault(override.op_family, []).append(override)

    # Expose registry metadata cleanly on the module for custom-op / layer dispatch.
    mod._gpustack_kernel_overrides = selected
    mod._gpustack_dispatch_manifest_digest = status.dispatch_manifest_digest
    mod._gpustack_loaded_kernel_keys = list(status.loaded_kernel_keys)

    # Bind forged callables into real vLLM CustomOp / SGLang layer dispatch methods.
    _bind_overrides_to_module_classes(mod, by_family=by_family, target_name=target_name)

    # If the target module exposes a custom-op registration hook, register lazy shims.
    register_fn = getattr(mod, "register_gpustack_override", None)
    if callable(register_fn):
        for family, override in selected.items():
            _safe_register_override(register_fn, family, override, target_name)

    mod._gpustack_kernel_loader_patched = True
    return status


def patch_vllm_custom_op_module(mod: Any) -> KernelLoadStatus:
    """Lazy post-import hook for vllm.model_executor.custom_op."""
    return _attach_overrides_to_module(
        mod,
        target_name="vllm.model_executor.custom_op",
        engine="vLLM",
    )


def patch_vllm_layernorm_module(mod: Any) -> KernelLoadStatus:
    """Lazy post-import hook for vllm.model_executor.layers.layernorm."""
    return _attach_overrides_to_module(
        mod,
        target_name="vllm.model_executor.layers.layernorm",
        engine="vLLM",
        op_families=("rmsnorm", "layernorm"),
    )


def patch_vllm_rotary_embedding_module(mod: Any) -> KernelLoadStatus:
    """Lazy post-import hook for vllm.model_executor.layers.rotary_embedding."""
    return _attach_overrides_to_module(
        mod,
        target_name="vllm.model_executor.layers.rotary_embedding",
        engine="vLLM",
        op_families=("rope_kv_append",),
    )


def patch_vllm_activation_module(mod: Any) -> KernelLoadStatus:
    """Lazy post-import hook for vllm.model_executor.layers.activation."""
    return _attach_overrides_to_module(
        mod,
        target_name="vllm.model_executor.layers.activation",
        engine="vLLM",
        op_families=("activation_quant",),
    )


def patch_vllm_gpu_model_runner_module(mod: Any) -> KernelLoadStatus:
    """Lazy post-import hook for vllm.v1.worker.gpu_model_runner."""
    return _attach_overrides_to_module(
        mod,
        target_name="vllm.v1.worker.gpu_model_runner",
        engine="vLLM",
    )


def patch_sglang_model_runner_module(mod: Any) -> KernelLoadStatus:
    """Lazy post-import hook for sglang.srt.model_executor.model_runner."""
    return _attach_overrides_to_module(
        mod,
        target_name="sglang.srt.model_executor.model_runner",
        engine="SGLang",
    )


def patch_sglang_layernorm_module(mod: Any) -> KernelLoadStatus:
    """Lazy post-import hook for sglang.srt.layers.layernorm."""
    return _attach_overrides_to_module(
        mod,
        target_name="sglang.srt.layers.layernorm",
        engine="SGLang",
        op_families=("rmsnorm", "layernorm"),
    )


def patch_sglang_rotary_embedding_module(mod: Any) -> KernelLoadStatus:
    """Lazy post-import hook for sglang.srt.layers.rotary_embedding."""
    return _attach_overrides_to_module(
        mod,
        target_name="sglang.srt.layers.rotary_embedding",
        engine="SGLang",
        op_families=("rope_kv_append",),
    )


__all__ = [
    "DEFAULT_KERNEL_ARTIFACT_DIR",
    "UNSUPPORTED_KERNEL_ARTIFACT_ERROR",
    "KernelDispatchRegistry",
    "KernelLoadStatus",
    "LoadedKernelOverride",
    "aggregate_kernel_reports",
    "arch_compatible",
    "ensure_process_kernels_loaded",
    "get_process_kernel_registry",
    "get_process_kernel_report",
    "load_kernel_dispatch_manifest",
    "normalize_arch",
    "patch_sglang_layernorm_module",
    "patch_sglang_model_runner_module",
    "patch_sglang_rotary_embedding_module",
    "patch_vllm_activation_module",
    "patch_vllm_custom_op_module",
    "patch_vllm_gpu_model_runner_module",
    "patch_vllm_layernorm_module",
    "patch_vllm_rotary_embedding_module",
    "reset_process_kernel_registry",
]
