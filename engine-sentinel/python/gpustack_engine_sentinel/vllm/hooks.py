"""Zero-intrusion vLLM and vllm-omni post-import hooks (no VLLM_PLUGINS mutation)."""

from __future__ import annotations

import inspect
from functools import wraps
from typing import Any

from gpustack_engine_sentinel.latch import BootLatch
from gpustack_engine_sentinel.protocol import (
    ENGINE_VLLM,
    VLLM_ALL_FIELDS,
    InvalidRequestError,
    RankReceipt,
    SentinelError,
    UnsupportedFieldsError,
)
from gpustack_engine_sentinel.server import (
    SentinelControlPlane,
    ensure_outermost_sentinel_middleware,
)
from gpustack_engine_sentinel.vllm.utility import UTILITY_METHOD_NAME


def _supports_async_mp_utility(engine_core: Any) -> bool:
    if engine_core is None:
        return False
    call_all = getattr(engine_core, "call_utility_all_async", None)
    if not callable(call_all):
        return False
    return hasattr(engine_core, "core_engines")


class VLLMSentinelBackend:
    """Drives vLLM EngineClient health checks and multi-rank EngineCore RPCs."""

    engine = ENGINE_VLLM

    def __init__(self, *, app_state: Any = None) -> None:
        self.engine_client: Any = None
        self.app_state: Any = app_state
        self.args: Any = None
        self._membership_epoch = 1
        self._last_core_identities: tuple[Any, ...] | None = None

    def _resolve_engine_client(self) -> Any:
        if self.engine_client is not None:
            return self.engine_client
        if self.app_state is not None:
            ec = getattr(self.app_state, "engine_client", None)
            if ec is not None:
                self.engine_client = ec
                return ec
        return None

    def bind(self, engine_client: Any, state: Any, args: Any) -> None:
        self.engine_client = engine_client
        self.app_state = state
        self.args = args
        engine_core = getattr(engine_client, "engine_core", None)
        identities = tuple(getattr(engine_core, "core_engines", ()) or ())
        self._last_core_identities = identities

    def _current_membership_epoch(self, rank_epochs: list[int]) -> int:
        client = self._resolve_engine_client()
        engine_core = getattr(client, "engine_core", None)
        identities = tuple(getattr(engine_core, "core_engines", ()) or ())
        if self._last_core_identities is None:
            self._last_core_identities = identities
        elif identities != self._last_core_identities:
            self._membership_epoch += 1
            self._last_core_identities = identities
        return max([self._membership_epoch, *rank_epochs])

    async def check_live(self) -> tuple[bool, bool, str | None]:
        client = self._resolve_engine_client()
        if client is None:
            return True, False, None
        if bool(getattr(client, "errored", False)):
            return False, False, "engine_errored"
        engine_core = getattr(client, "engine_core", None)
        resources = getattr(engine_core, "resources", None)
        if bool(getattr(resources, "engine_dead", False)):
            return False, False, "engine_core_dead"

        check_health = getattr(client, "check_health", None)
        if callable(check_health):
            try:
                await check_health()
            except Exception as exc:
                return False, False, str(exc) or type(exc).__name__

        paused = False
        is_paused_fn = getattr(client, "is_paused", None)
        if callable(is_paused_fn):
            try:
                paused = bool(await is_paused_fn())
            except Exception:
                paused = False

        return True, paused, None

    async def check_ready(self) -> tuple[bool, str | None]:
        client = self._resolve_engine_client()
        if client is None:
            return False, "engine_starting"
        is_ready_fn = getattr(client, "is_ready", None)
        if callable(is_ready_fn):
            res = is_ready_fn()
            if inspect.isawaitable(res):
                res = await res
            if not bool(res):
                return False, "engine_starting"
        return True, None

    async def get_runtime_config(self) -> dict[str, Any]:
        client = self._resolve_engine_client()
        engine_core = getattr(client, "engine_core", None)
        if not _supports_async_mp_utility(engine_core):
            return {
                "membership_epoch": self._membership_epoch,
                "supported_fields": [],
                "unsupported_fields": list(VLLM_ALL_FIELDS),
                "boot_defaults": {},
                "ranks": [],
                "values": {},
            }

        raw_results = await engine_core.call_utility_all_async(
            UTILITY_METHOD_NAME, {"op": "inspect"}
        )
        expected_ranks = len(getattr(engine_core, "core_engines", ()) or ())
        if not raw_results:
            return {
                "membership_epoch": self._membership_epoch,
                "supported_fields": [],
                "unsupported_fields": list(VLLM_ALL_FIELDS),
                "boot_defaults": {},
                "ranks": [],
                "values": {},
            }
        if expected_ranks and len(raw_results) != expected_ranks:
            raise SentinelError(
                503,
                "incomplete_rank_membership",
                f"Expected {expected_ranks} EngineCore ranks, got {len(raw_results)}",
            )

        supported_sets = [set(res.get("supported_fields", ())) for res in raw_results]
        common_supported = sorted(set.intersection(*supported_sets))
        all_unsupported = sorted(set(VLLM_ALL_FIELDS) - set(common_supported))

        ranks: list[dict[str, Any]] = []
        rank_epochs: list[int] = []
        seen_rank_ids: set[int] = set()
        boot_defaults: dict[str, Any] = {}
        for idx, res in enumerate(raw_results):
            epoch = int(res.get("membership_epoch", 1))
            rank_epochs.append(epoch)
            rid = int(res.get("rank_id", idx))
            if rid in seen_rank_ids:
                raise SentinelError(
                    503,
                    "incomplete_rank_membership",
                    f"Duplicate EngineCore rank_id {rid} in inspect response",
                )
            seen_rank_ids.add(rid)
            bid = str(res.get("boot_id", ""))
            if not bid:
                raise SentinelError(
                    503,
                    "incomplete_rank_membership",
                    f"EngineCore rank {rid} reported empty boot_id",
                )
            if not boot_defaults and isinstance(res.get("boot_defaults"), dict):
                boot_defaults = dict(res["boot_defaults"])
            receipt = RankReceipt(
                rank_id=rid,
                boot_id=bid,
                membership_epoch=epoch,
                applied_digest=str(res.get("applied_digest", "")),
                values=dict(res.get("values", {})),
            )
            ranks.append(receipt.to_dict())
        ranks.sort(key=lambda r: r["rank_id"])
        values = dict(ranks[0]["values"]) if ranks else {}
        return {
            "membership_epoch": self._current_membership_epoch(rank_epochs),
            "supported_fields": common_supported,
            "unsupported_fields": all_unsupported,
            "boot_defaults": boot_defaults,
            "ranks": ranks,
            "values": values,
        }

    async def apply_runtime_config(
        self, request_data: dict[str, Any]
    ) -> dict[str, Any]:
        client = self._resolve_engine_client()
        engine_core = getattr(client, "engine_core", None)
        parameters = dict(request_data.get("parameters") or {})
        if not _supports_async_mp_utility(engine_core):
            requested = list(parameters.keys())
            raise UnsupportedFieldsError(
                requested or list(VLLM_ALL_FIELDS),
                supported_fields=[],
            )

        target_digest = str(request_data.get("applied_config_digest", ""))
        raw_results = await engine_core.call_utility_all_async(
            UTILITY_METHOD_NAME,
            {
                "op": "apply",
                "applied_config_digest": target_digest,
                "parameters": parameters,
                "reset_fields": request_data.get("reset_fields", []),
            },
        )
        expected_ranks = len(getattr(engine_core, "core_engines", ()) or ())
        if not raw_results or (expected_ranks and len(raw_results) != expected_ranks):
            if raw_results:
                await self._rollback_applied_ranks(engine_core, raw_results)
            raise SentinelError(
                500,
                "rollback_incomplete",
                f"Expected {expected_ranks} rank receipts, got {len(raw_results or ())}; missing ranks cannot be verified restored",
            )

        failed = [res for res in raw_results if not res.get("ok")]
        digest_mismatch = [
            res
            for res in raw_results
            if res.get("ok") and res.get("applied_digest") != target_digest
        ]
        if failed or digest_mismatch:
            rollback_ok = await self._rollback_applied_ranks(engine_core, raw_results)
            if not rollback_ok:
                raise SentinelError(
                    500,
                    "rollback_incomplete",
                    "Failed to roll back all applied vLLM ranks after partial apply failure",
                )

            unsupported_union: set[str] = set()
            for res in failed:
                if res.get("error_code") == "unsupported_fields":
                    unsupported_union.update(res.get("unsupported_fields", ()))
            if unsupported_union:
                supported_sets = [
                    set(res.get("supported_fields", ()))
                    for res in raw_results
                    if "supported_fields" in res
                ]
                common_sup = (
                    sorted(set.intersection(*supported_sets)) if supported_sets else []
                )
                raise UnsupportedFieldsError(
                    sorted(unsupported_union),
                    supported_fields=common_sup,
                )

            for res in failed:
                if res.get("error_code") == "invalid_argument":
                    raise InvalidRequestError(
                        str(res.get("message", "Invalid runtime control update"))
                    )

            first_msg = (
                failed[0].get("message", "Rank apply failed")
                if failed
                else "Rank applied_digest mismatch"
            )
            raise SentinelError(500, "rank_apply_failed", str(first_msg))

        supported_sets = [set(res.get("supported_fields", ())) for res in raw_results]
        common_supported = sorted(set.intersection(*supported_sets))
        all_unsupported = sorted(set(VLLM_ALL_FIELDS) - set(common_supported))

        ranks: list[dict[str, Any]] = []
        rank_epochs: list[int] = []
        boot_defaults: dict[str, Any] = {}
        for idx, res in enumerate(raw_results):
            epoch = int(res.get("membership_epoch", 1))
            rank_epochs.append(epoch)
            if not res.get("boot_id"):
                raise SentinelError(
                    500, "invalid_rank_receipt", f"Rank {idx} returned empty boot_id"
                )
            if not boot_defaults and isinstance(res.get("boot_defaults"), dict):
                boot_defaults = dict(res["boot_defaults"])
            receipt = RankReceipt(
                rank_id=int(res.get("rank_id", idx)),
                boot_id=str(res["boot_id"]),
                membership_epoch=epoch,
                applied_digest=str(res["applied_digest"]),
                values=dict(res.get("values", {})),
            )
            ranks.append(receipt.to_dict())
        ranks.sort(key=lambda r: r["rank_id"])
        values = dict(ranks[0]["values"]) if ranks else {}
        return {
            "membership_epoch": self._current_membership_epoch(rank_epochs),
            "supported_fields": common_supported,
            "unsupported_fields": all_unsupported,
            "boot_defaults": boot_defaults,
            "ranks": ranks,
            "values": values,
        }

    @staticmethod
    async def _rollback_applied_ranks(
        engine_core: Any, raw_results: list[dict[str, Any]]
    ) -> bool:
        applied_entries = [
            res for res in raw_results if res.get("ok") and "previous_snapshot" in res
        ]
        if not applied_entries:
            return True

        by_boot_id = {
            str(res["boot_id"]): {
                "snapshot": res["previous_snapshot"],
                "previous_digest": str(res.get("previous_digest", "")),
            }
            for res in applied_entries
            if res.get("boot_id")
        }
        if len(by_boot_id) != len(applied_entries):
            return False

        # Prefer order-independent boot_id-keyed broadcast over positional index zipping.
        call_all = getattr(engine_core, "call_utility_all_async", None)
        if callable(call_all):
            try:
                rb_results = await call_all(
                    UTILITY_METHOD_NAME,
                    {"op": "rollback", "by_boot_id": by_boot_id},
                )
            except Exception:
                return False
            restored_boots: set[str] = set()
            for r in rb_results or ():
                if not isinstance(r, dict) or not r.get("ok") or r.get("skipped"):
                    continue
                bid = str(r.get("boot_id", ""))
                if (
                    bid in by_boot_id
                    and str(r.get("applied_digest", ""))
                    == by_boot_id[bid]["previous_digest"]
                ):
                    restored_boots.add(bid)
            return set(by_boot_id.keys()).issubset(restored_boots)

        engines = list(getattr(engine_core, "core_engines", ()) or ())
        call_one = getattr(engine_core, "_call_utility_async", None)
        if callable(call_one) and len(engines) == len(raw_results):
            all_ok = True
            for engine_id, res in zip(engines, raw_results, strict=True):
                if res.get("ok") and "previous_snapshot" in res:
                    want_boot = str(res.get("boot_id", ""))
                    want_prev = str(res.get("previous_digest", ""))
                    try:
                        rb = await call_one(
                            UTILITY_METHOD_NAME,
                            {
                                "op": "rollback",
                                "by_boot_id": {
                                    want_boot: {
                                        "snapshot": res["previous_snapshot"],
                                        "previous_digest": want_prev,
                                    }
                                },
                            },
                            engine=engine_id,
                        )
                        if (
                            not isinstance(rb, dict)
                            or not rb.get("ok")
                            or rb.get("skipped")
                            or str(rb.get("boot_id", "")) != want_boot
                            or str(rb.get("applied_digest", "")) != want_prev
                        ):
                            all_ok = False
                    except Exception:
                        all_ok = False
            return all_ok

        cores = getattr(engine_core, "cores", None)
        if isinstance(cores, list) and len(cores) == len(raw_results):
            all_ok = True
            for core_obj, res in zip(cores, raw_results, strict=True):
                if res.get("ok") and "previous_snapshot" in res:
                    want_boot = str(res.get("boot_id", ""))
                    want_prev = str(res.get("previous_digest", ""))
                    rpc_fn = getattr(core_obj, UTILITY_METHOD_NAME, None)
                    if callable(rpc_fn):
                        try:
                            rb = rpc_fn(
                                {
                                    "op": "rollback",
                                    "by_boot_id": {
                                        want_boot: {
                                            "snapshot": res["previous_snapshot"],
                                            "previous_digest": want_prev,
                                        }
                                    },
                                }
                            )
                            if (
                                not isinstance(rb, dict)
                                or not rb.get("ok")
                                or rb.get("skipped")
                                or str(rb.get("boot_id", "")) != want_boot
                                or str(rb.get("applied_digest", "")) != want_prev
                            ):
                                all_ok = False
                        except Exception:
                            all_ok = False
                    else:
                        all_ok = False
            return all_ok
        return False


def install_vllm_sentinel_on_app(
    app: Any, *, latch: BootLatch | None = None
) -> SentinelControlPlane:
    """Attach SentinelControlPlane and outermost pause middleware onto a vLLM FastAPI app idempotently."""
    existing: SentinelControlPlane | None = getattr(
        app, "_gpustack_sentinel_plane", None
    )
    if existing is not None:
        ensure_outermost_sentinel_middleware(app, control_plane=existing)
        return existing

    app_state = getattr(app, "state", None)
    backend = VLLMSentinelBackend(app_state=app_state)
    plane = SentinelControlPlane(backend=backend, latch=latch or BootLatch())
    plane.install_routes(app)
    ensure_outermost_sentinel_middleware(app, control_plane=plane)

    app._gpustack_sentinel_installed = True
    app._gpustack_sentinel_plane = plane
    if app_state is not None:
        app_state._gpustack_sentinel_app = app
        app_state._gpustack_sentinel_plane = plane
    return plane


def _wrap_build_app_fn(orig_fn: Any) -> Any:
    if getattr(orig_fn, "_gpustack_sentinel_wrapped", False):
        return orig_fn

    @wraps(orig_fn)
    def _wrapped_build_app(*args: Any, **kwargs: Any) -> Any:
        app = orig_fn(*args, **kwargs)
        if app is not None:
            install_vllm_sentinel_on_app(app)
        return app

    _wrapped_build_app._gpustack_sentinel_wrapped = True  # type: ignore[attr-defined]
    return _wrapped_build_app


def _wrap_init_app_state_fn(orig_fn: Any) -> Any:
    if getattr(orig_fn, "_gpustack_sentinel_wrapped", False):
        return orig_fn

    @wraps(orig_fn)
    async def _wrapped_init_app_state(
        engine_client: Any, state: Any, args: Any, *rest: Any, **kwargs: Any
    ) -> Any:
        result = await orig_fn(engine_client, state, args, *rest, **kwargs)
        plane: SentinelControlPlane | None = getattr(
            state, "_gpustack_sentinel_plane", None
        )
        if plane is not None and isinstance(plane.backend, VLLMSentinelBackend):
            plane.backend.bind(engine_client, state, args)
            app = getattr(state, "_gpustack_sentinel_app", None)
            if app is not None:
                ensure_outermost_sentinel_middleware(app, control_plane=plane)
        return result

    _wrapped_init_app_state._gpustack_sentinel_wrapped = True  # type: ignore[attr-defined]
    return _wrapped_init_app_state


def patch_vllm_api_server_module(mod: Any) -> None:
    """Wrap build_app and init_app_state on vLLM API server modules idempotently."""
    build_app = getattr(mod, "build_app", None)
    if callable(build_app):
        mod.build_app = _wrap_build_app_fn(build_app)

    init_app_state = getattr(mod, "init_app_state", None)
    if callable(init_app_state):
        mod.init_app_state = _wrap_init_app_state_fn(init_app_state)


def patch_vllm_omni_api_server_module(mod: Any) -> None:
    """Wrap build_openai_app and omni_init_app_state on vllm-omni API server module."""
    for attr in ("build_openai_app", "build_app"):
        fn = getattr(mod, attr, None)
        if callable(fn):
            setattr(mod, attr, _wrap_build_app_fn(fn))

    omni_init = getattr(mod, "omni_init_app_state", None)
    if callable(omni_init):
        mod.omni_init_app_state = _wrap_init_app_state_fn(omni_init)
