"""Pure-ASGI control-plane router and pause gate for /gpustack/v1/*."""

from __future__ import annotations

import inspect
import json
import logging
import re
from typing import Any, Protocol

from gpustack_engine_sentinel.latch import BootLatch
from gpustack_engine_sentinel.protocol import (
    ACK_PATH,
    LIVE_PATH,
    PAUSE_PATH,
    READY_PATH,
    RUNTIME_CONFIG_PATH,
    AckRequest,
    InvalidRequestError,
    LiveResponse,
    PauseRequest,
    PauseResponse,
    ReadyResponse,
    RuntimeConfigRequest,
    RuntimeConfigResponse,
    SentinelError,
    UnsupportedFieldsError,
)

MAX_REQUEST_BODY_BYTES = 1 << 20
LOGGER = logging.getLogger(__name__)
PAUSE_EXEMPT_PATHS: frozenset[str] = frozenset({"/health", "/metrics"})
SENTINEL_INSTALLED_ATTR = "_gpustack_sentinel_installed"
_MULTI_SLASH_RE = re.compile(r"/+")


def normalize_request_path(scope: dict[str, Any]) -> str:
    """Normalize an ASGI request path against root_path, repeated slashes, and trailing slash."""
    raw_path = str(scope.get("path") or "/")
    root_path = str(scope.get("root_path") or "")
    if root_path and raw_path.startswith(root_path):
        raw_path = raw_path[len(root_path) :] or "/"
    collapsed = _MULTI_SLASH_RE.sub("/", raw_path)
    if not collapsed.startswith("/"):
        collapsed = "/" + collapsed
    if len(collapsed) > 1:
        collapsed = collapsed.rstrip("/")
    return collapsed or "/"


async def send_json_response(
    send: Any,
    status_code: int,
    payload: dict[str, Any],
    *,
    extra_headers: list[tuple[bytes, bytes]] | None = None,
    head_only: bool = False,
) -> None:
    """Write a JSON HTTP response directly over raw ASGI send()."""
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    headers: list[tuple[bytes, bytes]] = [
        (b"content-type", b"application/json"),
        (b"content-length", str(len(body)).encode("ascii")),
    ]
    if extra_headers:
        headers.extend(extra_headers)
    await send(
        {
            "type": "http.response.start",
            "status": status_code,
            "headers": headers,
        }
    )
    await send({"type": "http.response.body", "body": b"" if head_only else body})


class EngineSentinelBackend(Protocol):
    """Engine-specific adapter backing /gpustack/v1/* endpoints."""

    engine: str

    async def check_live(self) -> tuple[bool, bool, str | None]:
        """Return (alive, engine_paused, error_message)."""
        ...

    async def get_runtime_config(self) -> dict[str, Any]:
        """Return live engine runtime configuration, boot defaults, and per-rank state."""
        ...

    async def apply_runtime_config(
        self, request_data: dict[str, Any]
    ) -> dict[str, Any]:
        """Atomically apply runtime configuration across all ranks and return updated state."""
        ...


class _ClientDisconnectedError(Exception):
    """Raised internally when the peer disconnects before completing the request body."""


def _parse_json_bytes(raw: bytes) -> Any:
    if len(raw) > MAX_REQUEST_BODY_BYTES:
        raise InvalidRequestError("Request body exceeds 1 MiB limit")
    if not raw.strip():
        raise InvalidRequestError("Request body must not be empty")
    try:
        return json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise InvalidRequestError(f"Malformed JSON body: {exc}") from exc


async def _read_json_body(receive: Any) -> Any:
    chunks: list[bytes] = []
    total = 0
    while True:
        message = await receive()
        msg_type = message.get("type")
        if msg_type == "http.request":
            piece = message.get("body", b"")
            if piece:
                total += len(piece)
                if total > MAX_REQUEST_BODY_BYTES:
                    raise InvalidRequestError("Request body exceeds 1 MiB limit")
                chunks.append(piece)
            if not message.get("more_body", False):
                break
        elif msg_type == "http.disconnect":
            raise _ClientDisconnectedError
        else:
            break

    return _parse_json_bytes(b"".join(chunks))


class SentinelControlPlane:
    """Serves /gpustack/v1/{ready,live,ack,pause,runtime_config} in pure ASGI."""

    def __init__(
        self,
        *,
        backend: EngineSentinelBackend,
        latch: BootLatch | None = None,
    ) -> None:
        self.backend = backend
        self.latch = latch or BootLatch()

    @property
    def paused(self) -> bool:
        return self.latch.paused

    async def _check_backend_ready(self) -> tuple[bool, str | None]:
        check_ready_fn = getattr(self.backend, "check_ready", None)
        if not callable(check_ready_fn):
            return True, None
        result = check_ready_fn()
        if inspect.isawaitable(result):
            result = await result
        return bool(result[0]), result[1]

    async def handle_ready(self) -> tuple[int, dict[str, Any]]:
        snap = self.latch.snapshot()
        alive, engine_paused, err = await self.backend.check_live()
        if not alive:
            return (
                503,
                ReadyResponse(
                    ready=False,
                    boot_id=snap.boot_id,
                    observed_generation=snap.observed_generation,
                    applied_config_digest=snap.applied_config_digest,
                    reason=err or "engine_not_alive",
                ).to_dict(),
            )
        if snap.paused or engine_paused:
            return (
                503,
                ReadyResponse(
                    ready=False,
                    boot_id=snap.boot_id,
                    observed_generation=snap.observed_generation,
                    applied_config_digest=snap.applied_config_digest,
                    reason="role_paused",
                ).to_dict(),
            )
        engine_ready, ready_reason = await self._check_backend_ready()
        if not engine_ready:
            return (
                503,
                ReadyResponse(
                    ready=False,
                    boot_id=snap.boot_id,
                    observed_generation=snap.observed_generation,
                    applied_config_digest=snap.applied_config_digest,
                    reason=ready_reason or "engine_starting",
                ).to_dict(),
            )
        if not snap.acked:
            return (
                503,
                ReadyResponse(
                    ready=False,
                    boot_id=snap.boot_id,
                    observed_generation=snap.observed_generation,
                    applied_config_digest=snap.applied_config_digest,
                    reason="awaiting_ack",
                ).to_dict(),
            )
        return (
            200,
            ReadyResponse(
                ready=True,
                boot_id=snap.boot_id,
                observed_generation=snap.observed_generation,
                applied_config_digest=snap.applied_config_digest,
            ).to_dict(),
        )

    async def handle_live(self) -> tuple[int, dict[str, Any]]:
        snap = self.latch.snapshot()
        alive, engine_paused, err = await self.backend.check_live()
        status = 200 if alive else 503
        return (
            status,
            LiveResponse(
                alive=alive,
                boot_id=snap.boot_id,
                paused=bool(snap.paused or engine_paused),
                error=None if alive else (err or "engine_dead"),
            ).to_dict(),
        )

    async def handle_ack(self, body: Any) -> tuple[int, dict[str, Any]]:
        req = AckRequest.from_dict(body)
        resp = self.latch.ack(
            boot_id=req.boot_id,
            observed_generation=req.observed_generation,
            applied_config_digest=req.applied_config_digest,
        )
        return 200, resp.to_dict()

    async def handle_get_pause(self) -> tuple[int, dict[str, Any]]:
        snap = self.latch.snapshot()
        return 200, PauseResponse(paused=snap.paused, boot_id=snap.boot_id).to_dict()

    async def handle_post_pause(self, body: Any) -> tuple[int, dict[str, Any]]:
        req = PauseRequest.from_dict(body)
        resp = self.latch.set_paused(
            req.paused,
            expected_boot_id=req.expected_boot_id,
        )
        return 200, resp.to_dict()

    def _compose_runtime_config_response(
        self,
        state_dict: dict[str, Any],
        *,
        alive: bool,
        engine_paused: bool,
        engine_ready: bool,
    ) -> dict[str, Any]:
        snap = self.latch.snapshot()
        paused = bool(snap.paused or engine_paused)
        resp = RuntimeConfigResponse(
            engine=self.backend.engine,
            boot_id=snap.boot_id,
            membership_epoch=int(state_dict.get("membership_epoch", 1)),
            observed_generation=snap.observed_generation,
            applied_config_digest=snap.applied_config_digest,
            last_operation_id=snap.last_operation_id,
            ready=bool(snap.acked and alive and engine_ready and not paused),
            paused=paused,
            supported_fields=list(state_dict.get("supported_fields", ())),
            unsupported_fields=list(state_dict.get("unsupported_fields", ())),
            boot_defaults=dict(state_dict.get("boot_defaults", {})),
            ranks=list(state_dict.get("ranks", ())),
            values=dict(state_dict.get("values", {})),
        )
        return resp.to_dict()

    async def handle_get_runtime_config(self) -> tuple[int, dict[str, Any]]:
        alive, engine_paused, _ = await self.backend.check_live()
        engine_ready, _ = await self._check_backend_ready()
        state_dict = await self.backend.get_runtime_config()
        return (
            200,
            self._compose_runtime_config_response(
                state_dict,
                alive=alive,
                engine_paused=engine_paused,
                engine_ready=engine_ready,
            ),
        )

    async def handle_post_runtime_config(self, body: Any) -> tuple[int, dict[str, Any]]:
        req = RuntimeConfigRequest.from_dict(body)
        self.latch.check_expected_boot_id(req.expected_boot_id)
        try:
            state_dict = await self.backend.apply_runtime_config(
                {
                    "operation_id": req.operation_id,
                    "expected_boot_id": req.expected_boot_id,
                    "generation": req.generation,
                    "applied_config_digest": req.applied_config_digest,
                    "parameters": req.parameters,
                    "reset_fields": req.reset_fields,
                }
            )
        except UnsupportedFieldsError as exc:
            snap = self.latch.snapshot()
            exc.extra.setdefault("boot_id", snap.boot_id)
            exc.extra.setdefault("applied_config_digest", snap.applied_config_digest)
            raise
        self.latch.record_apply(
            operation_id=req.operation_id,
            generation=req.generation,
            applied_config_digest=req.applied_config_digest,
        )
        alive, engine_paused, _ = await self.backend.check_live()
        engine_ready, _ = await self._check_backend_ready()
        return (
            200,
            self._compose_runtime_config_response(
                state_dict,
                alive=alive,
                engine_paused=engine_paused,
                engine_ready=engine_ready,
            ),
        )

    async def handle_asgi(
        self,
        scope: dict[str, Any],
        receive: Any,
        send: Any,
        norm_path: str | None = None,
    ) -> None:
        method = str(scope.get("method") or "GET").upper()
        path = norm_path or normalize_request_path(scope)
        head_only = method == "HEAD"

        if method == "OPTIONS":
            await send_json_response(send, 200, {"ok": True})
            return

        async def _method_not_allowed(allow: bytes) -> None:
            await send_json_response(
                send,
                405,
                {
                    "error": "method_not_allowed",
                    "message": f"Method {method} not allowed on {path}",
                },
                extra_headers=[(b"allow", allow)],
            )

        try:
            if path == READY_PATH:
                if method not in ("GET", "HEAD"):
                    await _method_not_allowed(b"GET, HEAD")
                    return
                status, payload = await self.handle_ready()
                await send_json_response(send, status, payload, head_only=head_only)
                return

            if path == LIVE_PATH:
                if method not in ("GET", "HEAD"):
                    await _method_not_allowed(b"GET, HEAD")
                    return
                status, payload = await self.handle_live()
                await send_json_response(send, status, payload, head_only=head_only)
                return

            if path == ACK_PATH:
                if method != "POST":
                    await _method_not_allowed(b"POST")
                    return
                body = await _read_json_body(receive)
                status, payload = await self.handle_ack(body)
                await send_json_response(send, status, payload)
                return

            if path == PAUSE_PATH:
                if method in ("GET", "HEAD"):
                    status, payload = await self.handle_get_pause()
                    await send_json_response(send, status, payload, head_only=head_only)
                    return
                if method == "POST":
                    body = await _read_json_body(receive)
                    status, payload = await self.handle_post_pause(body)
                    await send_json_response(send, status, payload)
                    return
                await _method_not_allowed(b"GET, HEAD, POST")
                return

            if path == RUNTIME_CONFIG_PATH:
                if method in ("GET", "HEAD"):
                    status, payload = await self.handle_get_runtime_config()
                    await send_json_response(send, status, payload, head_only=head_only)
                    return
                if method == "POST":
                    body = await _read_json_body(receive)
                    status, payload = await self.handle_post_runtime_config(body)
                    await send_json_response(send, status, payload)
                    return
                await _method_not_allowed(b"GET, HEAD, POST")
                return

            await send_json_response(
                send,
                404,
                {
                    "error": "not_found",
                    "message": f"Unknown engine sentinel endpoint: {path}",
                },
                head_only=head_only,
            )
        except _ClientDisconnectedError:
            return
        except SentinelError as exc:
            await send_json_response(
                send, exc.status_code, exc.to_dict(), head_only=head_only
            )
        except Exception:
            LOGGER.exception("Unhandled error serving %s %s", method, path)
            await send_json_response(
                send,
                500,
                {
                    "error": "internal_error",
                    "message": "Internal server error",
                },
                head_only=head_only,
            )

    def install_routes(self, app: Any) -> None:
        """Register /gpustack/v1/* route entries on a FastAPI/Starlette app for route introspection."""
        add_api_route = getattr(app, "add_api_route", None)
        if not callable(add_api_route):
            return

        app_module = type(app).__module__
        if app_module.startswith(("fastapi", "starlette")):
            from starlette.requests import Request
            from starlette.responses import JSONResponse

            async def _ready_ep(request: Request) -> JSONResponse:
                del request
                code, data = await self.handle_ready()
                return JSONResponse(content=data, status_code=code)

            async def _live_ep(request: Request) -> JSONResponse:
                del request
                code, data = await self.handle_live()
                return JSONResponse(content=data, status_code=code)

            async def _read_bounded_json(request: Request) -> Any:
                chunks: list[bytes] = []
                total = 0
                async for piece in request.stream():
                    if not piece:
                        continue
                    total += len(piece)
                    if total > MAX_REQUEST_BODY_BYTES:
                        raise InvalidRequestError("Request body exceeds 1 MiB limit")
                    chunks.append(piece)
                return _parse_json_bytes(b"".join(chunks))

            async def _ack_ep(request: Request) -> JSONResponse:
                try:
                    code, data = await self.handle_ack(
                        await _read_bounded_json(request)
                    )
                except SentinelError as exc:
                    return JSONResponse(
                        content=exc.to_dict(), status_code=exc.status_code
                    )
                except Exception:
                    LOGGER.exception("Unhandled error in %s", ACK_PATH)
                    return JSONResponse(
                        content={
                            "error": "internal_error",
                            "message": "Internal server error",
                        },
                        status_code=500,
                    )
                return JSONResponse(content=data, status_code=code)

            async def _pause_get_ep(request: Request) -> JSONResponse:
                del request
                code, data = await self.handle_get_pause()
                return JSONResponse(content=data, status_code=code)

            async def _pause_post_ep(request: Request) -> JSONResponse:
                try:
                    code, data = await self.handle_post_pause(
                        await _read_bounded_json(request)
                    )
                except SentinelError as exc:
                    return JSONResponse(
                        content=exc.to_dict(), status_code=exc.status_code
                    )
                except Exception:
                    LOGGER.exception("Unhandled error in %s", PAUSE_PATH)
                    return JSONResponse(
                        content={
                            "error": "internal_error",
                            "message": "Internal server error",
                        },
                        status_code=500,
                    )
                return JSONResponse(content=data, status_code=code)

            async def _rc_get_ep(request: Request) -> JSONResponse:
                del request
                code, data = await self.handle_get_runtime_config()
                return JSONResponse(content=data, status_code=code)

            async def _rc_post_ep(request: Request) -> JSONResponse:
                try:
                    code, data = await self.handle_post_runtime_config(
                        await _read_bounded_json(request)
                    )
                except SentinelError as exc:
                    return JSONResponse(
                        content=exc.to_dict(), status_code=exc.status_code
                    )
                except Exception:
                    LOGGER.exception("Unhandled error in %s", RUNTIME_CONFIG_PATH)
                    return JSONResponse(
                        content={
                            "error": "internal_error",
                            "message": "Internal server error",
                        },
                        status_code=500,
                    )
                return JSONResponse(content=data, status_code=code)

            add_api_route(READY_PATH, _ready_ep, methods=["GET", "HEAD"])
            add_api_route(LIVE_PATH, _live_ep, methods=["GET", "HEAD"])
            add_api_route(ACK_PATH, _ack_ep, methods=["POST"])
            add_api_route(PAUSE_PATH, _pause_get_ep, methods=["GET", "HEAD"])
            add_api_route(PAUSE_PATH, _pause_post_ep, methods=["POST"])
            add_api_route(RUNTIME_CONFIG_PATH, _rc_get_ep, methods=["GET", "HEAD"])
            add_api_route(RUNTIME_CONFIG_PATH, _rc_post_ep, methods=["POST"])
            return

        LOGGER.warning(
            "Skipping sentinel route registration on non-FastAPI/Starlette app %s.%s",
            app_module,
            type(app).__name__,
        )


class SentinelASGIMiddleware:
    """Pure ASGI wrapper routing /gpustack/v1/* and rejecting inference with 503 while paused."""

    _gpustack_sentinel_installed = True

    def __init__(self, app: Any, *, control_plane: SentinelControlPlane) -> None:
        self.app = app
        self.control_plane = control_plane

    @staticmethod
    def _is_gpustack_path(norm_path: str) -> bool:
        return norm_path == "/gpustack" or norm_path.startswith("/gpustack/")

    async def __call__(self, scope: dict[str, Any], receive: Any, send: Any) -> None:
        scope_type = scope.get("type")
        if scope_type not in ("http", "websocket"):
            await self.app(scope, receive, send)
            return

        norm_path = normalize_request_path(scope)
        if scope_type == "http" and self._is_gpustack_path(norm_path):
            await self.control_plane.handle_asgi(scope, receive, send, norm_path)
            return

        if self.control_plane.paused and norm_path not in PAUSE_EXEMPT_PATHS:
            if scope_type == "websocket":
                await send({"type": "websocket.close", "code": 1013})
                return
            method = str(scope.get("method") or "GET").upper()
            await send_json_response(
                send,
                503,
                {
                    "error": "role_paused",
                    "message": "role is paused and not accepting new requests",
                },
                head_only=(method == "HEAD"),
            )
            return

        await self.app(scope, receive, send)


def ensure_outermost_sentinel_middleware(
    app: Any,
    *,
    control_plane: SentinelControlPlane,
) -> None:
    """Install or promote SentinelASGIMiddleware to index 0 (outermost) of app.user_middleware."""
    had_stack = getattr(app, "middleware_stack", None) is not None
    user_mw = getattr(app, "user_middleware", None)
    if isinstance(user_mw, list):
        existing_idx = None
        for idx, item in enumerate(user_mw):
            if getattr(item, "cls", None) is SentinelASGIMiddleware:
                existing_idx = idx
                break
        if existing_idx is not None:
            entry = user_mw.pop(existing_idx)
            user_mw.insert(0, entry)
            if hasattr(app, "middleware_stack"):
                app.middleware_stack = None
            if had_stack and callable(getattr(app, "build_middleware_stack", None)):
                app.middleware_stack = app.build_middleware_stack()
            return

    add_middleware = getattr(app, "add_middleware", None)
    if callable(add_middleware):
        if had_stack:
            app.middleware_stack = None
        add_middleware(SentinelASGIMiddleware, control_plane=control_plane)
        if had_stack and callable(getattr(app, "build_middleware_stack", None)):
            app.middleware_stack = app.build_middleware_stack()
