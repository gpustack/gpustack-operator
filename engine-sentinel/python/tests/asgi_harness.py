"""Minimal pure-Python ASGI test client and Starlette/FastAPI-compatible app double."""

from __future__ import annotations

import asyncio
import json
from collections.abc import Callable
from dataclasses import dataclass, field
from types import SimpleNamespace
from typing import Any


@dataclass
class ASGIResponse:
    status_code: int
    headers: dict[str, str]
    body: bytes

    def json(self) -> Any:
        return json.loads(self.body.decode("utf-8"))


@dataclass
class FakeMiddlewareSpec:
    cls: Any
    args: tuple[Any, ...] = ()
    kwargs: dict[str, Any] = field(default_factory=dict)


@dataclass
class FakeRoute:
    path: str
    endpoint: Callable[..., Any]
    methods: set[str]


class FakeFastAPIApp:
    """Starlette/FastAPI-compatible ASGI app double with LIFO user_middleware."""

    def __init__(self, *, root_path: str = "") -> None:
        self.root_path = root_path
        self.state = SimpleNamespace()
        self.routes: list[FakeRoute] = []
        self.router = SimpleNamespace(routes=self.routes)
        self.user_middleware: list[FakeMiddlewareSpec] = []
        self.middleware_stack: Any = None
        self.openapi_schema: Any = {"openapi": "3.1.0"}

    def add_middleware(self, middleware_cls: Any, *args: Any, **kwargs: Any) -> None:
        if self.middleware_stack is not None:
            raise RuntimeError("Cannot add middleware after an application has started")
        self.user_middleware.insert(
            0, FakeMiddlewareSpec(cls=middleware_cls, args=args, kwargs=kwargs)
        )

    def add_api_route(
        self,
        path: str,
        endpoint: Callable[..., Any],
        *,
        methods: list[str] | tuple[str, ...] = ("GET",),
        **_: Any,
    ) -> None:
        self.routes.append(
            FakeRoute(
                path=path, endpoint=endpoint, methods={m.upper() for m in methods}
            )
        )

    def build_middleware_stack(self) -> Any:
        app: Any = self._dispatch_router
        for spec in reversed(self.user_middleware):
            app = spec.cls(app, *spec.args, **spec.kwargs)
        return app

    async def _dispatch_router(
        self, scope: dict[str, Any], receive: Any, send: Any
    ) -> None:
        if scope["type"] != "http":
            return
        root_path = scope.get("root_path", "")
        raw_path = scope.get("path", "/")
        path = raw_path.removeprefix(root_path) if root_path else raw_path
        normalized = "/" + "/".join(seg for seg in path.split("/") if seg)
        method = scope.get("method", "GET").upper()

        matched_path = False
        for route in self.routes:
            if route.path in (path, normalized):
                matched_path = True
                if method in route.methods or (
                    method == "HEAD" and "GET" in route.methods
                ):
                    result = route.endpoint(scope, receive, send)
                    if asyncio.iscoroutine(result):
                        await result
                    return
        status = 405 if matched_path else 404
        payload = json.dumps(
            {"error": "method_not_allowed" if status == 405 else "not_found"}
        ).encode("utf-8")
        await send(
            {
                "type": "http.response.start",
                "status": status,
                "headers": [(b"content-type", b"application/json")],
            }
        )
        await send({"type": "http.response.body", "body": payload})

    async def __call__(self, scope: dict[str, Any], receive: Any, send: Any) -> None:
        if self.middleware_stack is None:
            self.middleware_stack = self.build_middleware_stack()
        await self.middleware_stack(scope, receive, send)


async def send_asgi_request(
    app: Any,
    method: str,
    target: str,
    *,
    headers: dict[str, str] | None = None,
    json_body: Any = None,
    raw_body: bytes | None = None,
    root_path: str = "",
) -> ASGIResponse:
    raw_path_part, _, query_part = target.partition("?")
    path = raw_path_part or "/"
    if root_path and not path.startswith(root_path):
        full_path = root_path.rstrip("/") + path
    else:
        full_path = path

    if raw_body is not None:
        body_bytes = raw_body
    elif json_body is not None:
        body_bytes = json.dumps(json_body).encode("utf-8")
    else:
        body_bytes = b""

    raw_headers: list[tuple[bytes, bytes]] = []
    if json_body is not None:
        raw_headers.append((b"content-type", b"application/json"))
    for k, v in (headers or {}).items():
        raw_headers.append((k.lower().encode("latin1"), v.encode("latin1")))

    scope = {
        "type": "http",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "method": method.upper(),
        "scheme": "http",
        "path": full_path,
        "raw_path": full_path.encode("latin1"),
        "query_string": query_part.encode("latin1"),
        "root_path": root_path,
        "headers": raw_headers,
        "client": ("127.0.0.1", 50000),
        "server": ("127.0.0.1", 8000),
    }

    sent_body = False

    async def receive() -> dict[str, Any]:
        nonlocal sent_body
        if not sent_body:
            sent_body = True
            return {"type": "http.request", "body": body_bytes, "more_body": False}
        return {"type": "http.disconnect"}

    status_code = 500
    resp_headers: dict[str, str] = {}
    chunks: list[bytes] = []

    async def send(message: dict[str, Any]) -> None:
        nonlocal status_code
        if message["type"] == "http.response.start":
            status_code = int(message["status"])
            for k, v in message.get("headers", []):
                resp_headers[k.decode("latin1").lower()] = v.decode("latin1")
        elif message["type"] == "http.response.body":
            chunks.append(message.get("body", b""))

    await app(scope, receive, send)
    return ASGIResponse(
        status_code=status_code, headers=resp_headers, body=b"".join(chunks)
    )


def run_sync(coro: Any) -> Any:
    return asyncio.run(coro)
