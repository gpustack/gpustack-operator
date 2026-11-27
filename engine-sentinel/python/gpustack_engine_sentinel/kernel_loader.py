"""Lazy startup kernel dispatch loader reading /var/lib/gpustack/kernels/dispatch-manifest.json."""

from __future__ import annotations

import hashlib
import importlib.util
import json
import logging
import os
import stat
import threading
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from gpustack_engine_sentinel.probe import (
    ALLOWED_OP_FAMILIES,
    canonical_engine_name,
    is_excluded_operator,
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
    if (
        want_version
        and have_version
        and normalize_engine_version(want_version)
        != normalize_engine_version(have_version)
    ):
        return False
    if (
        want_manufacturer
        and have_manufacturer
        and want_manufacturer.strip().lower() != have_manufacturer.strip().lower()
    ):
        return False
    return not (
        want_arch
        and have_arch
        and want_arch.strip().lower() != have_arch.strip().lower()
    )


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
        override = LoadedKernelOverride(
            local_kernel_key=raw_key.strip(),
            op_family=parsed["op_family"],
            definition=str(entry.get("definition") or parsed["op_family"]),
            library_path=lib_file,
            entrypoint=entrypoint,
            solution_path=sol_file,
            trace_path=trace_file,
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
            return self._status

    def get_override_for_family(self, op_family: str) -> LoadedKernelOverride | None:
        with self._lock:
            for override in self._status.overrides.values():
                if override.op_family == op_family:
                    return override
            return None

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
    for override in status.overrides.values():
        if op_families is None or override.op_family in op_families:
            selected[override.op_family] = override

    # Expose registry metadata cleanly on the module for custom-op / layer dispatch.
    mod._gpustack_kernel_overrides = selected
    mod._gpustack_dispatch_manifest_digest = status.dispatch_manifest_digest
    mod._gpustack_loaded_kernel_keys = list(status.loaded_kernel_keys)

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
    "ensure_process_kernels_loaded",
    "get_process_kernel_registry",
    "get_process_kernel_report",
    "load_kernel_dispatch_manifest",
    "patch_sglang_layernorm_module",
    "patch_sglang_model_runner_module",
    "patch_sglang_rotary_embedding_module",
    "patch_vllm_custom_op_module",
    "patch_vllm_gpu_model_runner_module",
    "reset_process_kernel_registry",
]
