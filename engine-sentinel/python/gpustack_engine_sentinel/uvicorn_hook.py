"""Universal uvicorn.Config fallback hook ensuring /gpustack/v1/* never 404s on version drift or omni servers."""

from __future__ import annotations

import inspect
import os
import sys
from functools import wraps
from typing import Any

from gpustack_engine_sentinel.kernel_loader import get_process_kernel_report
from gpustack_engine_sentinel.latch import BootLatch
from gpustack_engine_sentinel.protocol import (
    ALL_CANONICAL_FIELDS,
    ENGINE_SGLANG,
    ENGINE_VLLM,
    UnsupportedEngineVersionError,
    UnsupportedFieldsError,
)
from gpustack_engine_sentinel.server import (
    SENTINEL_INSTALLED_ATTR,
    SentinelASGIMiddleware,
    SentinelControlPlane,
    ensure_outermost_sentinel_middleware,
)


def _detect_fallback_engine() -> str:
    env_engine = os.environ.get("GPUSTACK_ENGINE_SENTINEL_ENGINE", "").strip().lower()
    if env_engine in (ENGINE_VLLM, ENGINE_SGLANG):
        return env_engine
    for mod_name in tuple(sys.modules):
        if mod_name == "sglang" or mod_name.startswith(("sglang.", "sglang_omni")):
            return ENGINE_SGLANG
        if mod_name == "vllm" or mod_name.startswith(("vllm.", "vllm_omni")):
            return ENGINE_VLLM
    return "generic"


class FallbackSentinelBackend:
    """Safe zero-assumption backend for standalone, omni, or version-drifted ASGI servers."""

    def __init__(self, *, app: Any = None) -> None:
        self._app = app

    @property
    def engine(self) -> str:
        return _detect_fallback_engine()

    async def check_live(self) -> tuple[bool, bool, str | None]:
        state = getattr(self._app, "state", None)
        engine_client = getattr(state, "engine_client", None)
        if bool(getattr(engine_client, "errored", False)):
            return False, False, "engine_errored"
        return True, False, None

    async def check_ready(self) -> tuple[bool, str | None]:
        return True, None

    async def get_runtime_config(self) -> dict[str, Any]:
        return {
            "membership_epoch": 1,
            "supported_fields": [],
            "unsupported_fields": sorted(ALL_CANONICAL_FIELDS),
            "boot_defaults": {},
            "ranks": [],
            "values": {},
        }

    async def apply_runtime_config(
        self, request_data: dict[str, Any]
    ) -> dict[str, Any]:
        parameters = dict(request_data.get("parameters") or {})
        requested = sorted(parameters.keys()) or sorted(ALL_CANONICAL_FIELDS)
        raise UnsupportedFieldsError(requested, supported_fields=[])

    def get_kernel_summary(self) -> dict[str, Any]:
        return get_process_kernel_report(engine=self.engine)

    async def start_probe(self, spec: dict[str, Any]) -> dict[str, Any]:
        del spec
        raise UnsupportedEngineVersionError(
            f"Fallback sentinel backend for {self.engine!r} lacks worker probe hooks"
        )


def is_sentinel_installed(app: Any) -> bool:
    """Return True if app or any inner wrapped ASGI app already carries gpustack-engine-sentinel."""
    seen: set[int] = set()
    cur = app
    for _ in range(16):
        if cur is None or id(cur) in seen:
            break
        seen.add(id(cur))
        if bool(getattr(cur, SENTINEL_INSTALLED_ATTR, False)) or isinstance(
            cur, SentinelASGIMiddleware
        ):
            return True
        user_mw = getattr(cur, "user_middleware", None)
        if isinstance(user_mw, list) and any(
            getattr(item, "cls", None) is SentinelASGIMiddleware for item in user_mw
        ):
            return True
        cur = getattr(cur, "app", None)
    return False


def _find_middleware_capable_app(app: Any) -> Any | None:
    seen: set[int] = set()
    cur = app
    for _ in range(16):
        if cur is None or id(cur) in seen:
            break
        seen.add(id(cur))
        if callable(getattr(cur, "add_middleware", None)):
            return cur
        cur = getattr(cur, "app", None)
    return None


def ensure_fallback_sentinel_on_app(app: Any, *, latch: BootLatch | None = None) -> Any:
    """Wrap an ASGI app with FallbackSentinelBackend if no engine-specific sentinel hook installed yet."""
    if app is None or is_sentinel_installed(app):
        return app

    target_app = _find_middleware_capable_app(app)
    if target_app is not None:
        backend = FallbackSentinelBackend(app=target_app)
        plane = SentinelControlPlane(backend=backend, latch=latch or BootLatch())
        plane.install_routes(target_app)
        ensure_outermost_sentinel_middleware(target_app, control_plane=plane)
        setattr(target_app, SENTINEL_INSTALLED_ATTR, True)
        target_app._gpustack_sentinel_plane = plane
        return app

    backend = FallbackSentinelBackend(app=app)
    plane = SentinelControlPlane(backend=backend, latch=latch or BootLatch())
    wrapped = SentinelASGIMiddleware(app, control_plane=plane)
    setattr(wrapped, SENTINEL_INSTALLED_ATTR, True)
    wrapped._gpustack_sentinel_plane = plane  # type: ignore[attr-defined]
    return wrapped


def _can_wrap_in_config_init(config_obj: Any, app: Any) -> bool:
    if app is None or isinstance(app, str):
        return False
    if bool(getattr(config_obj, "factory", False)):
        return False
    if getattr(config_obj, "interface", "auto") == "wsgi":
        return False
    if callable(getattr(app, "add_middleware", None)):
        return True
    if inspect.iscoroutinefunction(app):
        return True
    return (
        callable(app)
        and not inspect.isclass(app)
        and inspect.iscoroutinefunction(type(app).__call__)
    )


def patch_uvicorn_config_module(mod: Any) -> None:
    """Hook uvicorn.config.Config.__init__ and Config.load idempotently."""
    config_cls = getattr(mod, "Config", None)
    if config_cls is None or getattr(config_cls, "_gpustack_sentinel_patched", False):
        return

    orig_init = config_cls.__init__
    orig_load = getattr(config_cls, "load", None)

    @wraps(orig_init)
    def _wrapped_init(self: Any, *args: Any, **kwargs: Any) -> None:
        orig_init(self, *args, **kwargs)
        current_app = getattr(self, "app", None)
        if _can_wrap_in_config_init(self, current_app):
            self.app = ensure_fallback_sentinel_on_app(current_app)

    config_cls.__init__ = _wrapped_init

    if callable(orig_load):

        @wraps(orig_load)
        def _wrapped_load(self: Any, *args: Any, **kwargs: Any) -> Any:
            result = orig_load(self, *args, **kwargs)
            loaded_app = getattr(self, "loaded_app", None)
            if (
                loaded_app is not None
                and getattr(self, "interface", "auto") != "wsgi"
                and not is_sentinel_installed(loaded_app)
            ):
                self.loaded_app = ensure_fallback_sentinel_on_app(loaded_app)
            return result

        config_cls.load = _wrapped_load

    config_cls._gpustack_sentinel_patched = True
