"""Tests for /gpustack/v1/{ready,live,ack,pause,runtime_config} wire contract and ASGI pause gating."""

from __future__ import annotations

import json
import sys
import uuid

import pytest
from asgi_harness import FakeFastAPIApp, run_sync, send_asgi_request


class DummyBackend:
    def __init__(self) -> None:
        self.engine = "vllm"
        self.alive = True
        self.paused = False
        self.dead_error: Exception | None = None
        self.active_reqs = 32

    async def check_live(self) -> tuple[bool, bool, str | None]:
        if self.dead_error is not None:
            return False, self.paused, str(self.dead_error)
        return self.alive, self.paused, None

    async def get_runtime_config(self) -> dict:
        return {
            "membership_epoch": 1,
            "supported_fields": [
                "scheduling.maxActiveRequests",
                "scheduling.maxBatchedTokens",
            ],
            "unsupported_fields": [],
            "boot_defaults": {
                "scheduling.maxActiveRequests": 64,
                "scheduling.maxBatchedTokens": 4096,
            },
            "ranks": [
                {
                    "rank_id": 0,
                    "boot_id": "rank-boot-0",
                    "membership_epoch": 1,
                    "applied_digest": "sha256:init",
                    "values": {
                        "scheduling.maxActiveRequests": self.active_reqs,
                        "scheduling.maxBatchedTokens": 4096,
                    },
                }
            ],
            "values": {
                "scheduling.maxActiveRequests": self.active_reqs,
                "scheduling.maxBatchedTokens": 4096,
            },
        }

    async def apply_runtime_config(self, request_data: dict) -> dict:
        digest = request_data["applied_config_digest"]
        params = request_data.get("parameters", {})
        if "scheduling.maxActiveRequests" in params:
            self.active_reqs = int(params["scheduling.maxActiveRequests"])
        if "scheduling.maxActiveRequests" in request_data.get("reset_fields", []):
            self.active_reqs = 64
        return {
            "membership_epoch": 1,
            "supported_fields": [
                "scheduling.maxActiveRequests",
                "scheduling.maxBatchedTokens",
            ],
            "unsupported_fields": [],
            "boot_defaults": {
                "scheduling.maxActiveRequests": 64,
                "scheduling.maxBatchedTokens": 4096,
            },
            "ranks": [
                {
                    "rank_id": 0,
                    "boot_id": "rank-boot-0",
                    "membership_epoch": 1,
                    "applied_digest": digest,
                    "values": {
                        "scheduling.maxActiveRequests": self.active_reqs,
                        "scheduling.maxBatchedTokens": 4096,
                    },
                }
            ],
            "values": {
                "scheduling.maxActiveRequests": self.active_reqs,
                "scheduling.maxBatchedTokens": 4096,
            },
        }


def _build_app():
    from gpustack_engine_sentinel.latch import BootLatch
    from gpustack_engine_sentinel.server import (
        SentinelControlPlane,
        ensure_outermost_sentinel_middleware,
    )

    app = FakeFastAPIApp()
    backend = DummyBackend()
    latch = BootLatch()
    plane = SentinelControlPlane(backend=backend, latch=latch)

    async def ok_endpoint(scope, receive, send):
        body = json.dumps({"ok": True, "path": scope["path"]}).encode("utf-8")
        await send(
            {
                "type": "http.response.start",
                "status": 200,
                "headers": [(b"content-type", b"application/json")],
            }
        )
        await send({"type": "http.response.body", "body": body})

    for path in (
        "/v1/chat/completions",
        "/v1/completions",
        "/v1/models",
        "/v1/audio/speech",
        "/v1/images/generations",
        "/v1/videos",
        "/health",
        "/metrics",
    ):
        app.add_api_route(path, ok_endpoint, methods=["GET", "POST"])

    plane.install_routes(app)
    ensure_outermost_sentinel_middleware(app, control_plane=plane)
    return app, backend, latch


def test_no_fastapi_starlette_or_auth_module():
    import gpustack_engine_sentinel.server as srv

    assert srv is not None
    assert "fastapi" not in sys.modules
    assert "starlette" not in sys.modules
    assert not hasattr(sys.modules["gpustack_engine_sentinel"], "auth")


def test_boot_latch_and_live_contract_without_any_auth_tokens(
    monkeypatch: pytest.MonkeyPatch,
):
    monkeypatch.delenv("GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK", raising=False)
    app, backend, latch = _build_app()

    # Valid UUID boot_id, fail-closed 503 initially without any Authorization header!
    uuid.UUID(latch.boot_id)
    r0 = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready"))
    assert r0.status_code == 503
    assert r0.json() == {
        "ready": False,
        "boot_id": latch.boot_id,
        "observed_generation": None,
        "applied_config_digest": "",
        "reason": "awaiting_ack",
    }

    # Wrong boot_id on POST /gpustack/v1/ack returns 409 Conflict (boot_id_mismatch).
    r_conflict = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/ack",
            json_body={
                "boot_id": "00000000-0000-0000-0000-000000000000",
                "observed_generation": 2,
                "applied_config_digest": "sha256:abc",
            },
        )
    )
    assert r_conflict.status_code == 409
    assert r_conflict.json()["error"] == "boot_id_mismatch"
    assert (
        run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready")).status_code == 503
    )

    # Matching boot_id latches ready open -> 200 OK.
    r_ack = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/ack",
            json_body={
                "boot_id": latch.boot_id,
                "observed_generation": 5,
                "applied_config_digest": "sha256:converged",
            },
        )
    )
    assert r_ack.status_code == 200
    r_open = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready"))
    assert r_open.status_code == 200
    assert r_open.json() == {
        "ready": True,
        "boot_id": latch.boot_id,
        "observed_generation": 5,
        "applied_config_digest": "sha256:converged",
    }

    # GET /gpustack/v1/live returns clean {"alive": true, "boot_id": ..., "paused": false} (no sleeping field!).
    r_live = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/live"))
    assert r_live.status_code == 200
    assert r_live.json() == {
        "alive": True,
        "boot_id": latch.boot_id,
        "paused": False,
    }

    # EngineDeadError flips both /gpustack/v1/live and /gpustack/v1/ready to 503!
    class EngineDeadError(Exception):
        pass

    backend.dead_error = EngineDeadError("EngineCore died")
    r_dead = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/live"))
    assert r_dead.status_code == 503
    assert r_dead.json()["alive"] is False
    assert "EngineCore died" in r_dead.json()["error"]
    assert (
        run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready")).status_code == 503
    )

    # Explicit GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK=0 starts latched open.
    monkeypatch.setenv("GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK", "0")
    app_open, _, latch_open = _build_app()
    r_imm = run_sync(send_asgi_request(app_open, "GET", "/gpustack/v1/ready"))
    assert r_imm.status_code == 200
    assert r_imm.json()["boot_id"] == latch_open.boot_id


def test_asgi_pause_gate_rejects_inference_503_allows_control_health_metrics_and_resumes():
    app, _, latch = _build_app()

    # Latch ready open first so we can verify pause flips /gpustack/v1/ready to 503 role_paused.
    assert (
        run_sync(
            send_asgi_request(
                app,
                "POST",
                "/gpustack/v1/ack",
                json_body={
                    "boot_id": latch.boot_id,
                    "observed_generation": 1,
                    "applied_config_digest": "sha256:init",
                },
            )
        ).status_code
        == 200
    )
    assert (
        run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready")).status_code == 200
    )

    # Initially unpaused.
    r_p0 = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/pause"))
    assert r_p0.status_code == 200
    assert r_p0.json() == {"paused": False, "boot_id": latch.boot_id}
    assert (
        run_sync(send_asgi_request(app, "POST", "/v1/chat/completions")).status_code
        == 200
    )

    # Wrong expected_boot_id on POST /gpustack/v1/pause returns 409 Conflict!
    r_p_conflict = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/pause",
            json_body={"paused": True, "expected_boot_id": "wrong-boot-id"},
        )
    )
    assert r_p_conflict.status_code == 409
    assert (
        run_sync(send_asgi_request(app, "GET", "/gpustack/v1/pause")).json()["paused"]
        is False
    )

    # Pause role via POST /gpustack/v1/pause (zero VLLM_SERVER_DEV_MODE needed!).
    r_pause = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/pause",
            json_body={"paused": True, "expected_boot_id": latch.boot_id},
        )
    )
    assert r_pause.status_code == 200
    assert r_pause.json() == {"paused": True, "boot_id": latch.boot_id}

    # GET /gpustack/v1/ready returns 503 (reason: role_paused) while paused!
    r_ready_paused = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready"))
    assert r_ready_paused.status_code == 503
    assert r_ready_paused.json()["ready"] is False
    assert r_ready_paused.json()["reason"] == "role_paused"

    # Direct inference arrivals across standard & multimodal routes are rejected 503 role_paused!
    for method, inf_path in (
        ("POST", "/v1/chat/completions"),
        ("POST", "/v1/completions"),
        ("GET", "/v1/models"),
        ("POST", "/v1/audio/speech"),
        ("POST", "/v1/images/generations"),
        ("POST", "/v1/videos"),
    ):
        r_inf = run_sync(send_asgi_request(app, method, inf_path))
        assert r_inf.status_code == 503, f"{method} {inf_path} was {r_inf.status_code}"
        assert r_inf.json() == {
            "error": "role_paused",
            "message": "role is paused and not accepting new requests",
        }

    # /gpustack/v1/*, /health, and /metrics remain reachable while paused!
    assert run_sync(send_asgi_request(app, "GET", "/health")).status_code == 200
    assert run_sync(send_asgi_request(app, "GET", "/metrics")).status_code == 200
    r_live_paused = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/live"))
    assert r_live_paused.status_code == 200
    assert r_live_paused.json() == {
        "alive": True,
        "boot_id": latch.boot_id,
        "paused": True,
    }
    r_rc_paused = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
    assert r_rc_paused.status_code == 200
    assert r_rc_paused.json()["paused"] is True
    assert r_rc_paused.json()["ready"] is False
    assert r_rc_paused.json()["boot_defaults"] == {
        "scheduling.maxActiveRequests": 64,
        "scheduling.maxBatchedTokens": 4096,
    }

    # Resume cleanly with paused=false (omitting optional expected_boot_id also works).
    r_resume = run_sync(
        send_asgi_request(
            app, "POST", "/gpustack/v1/pause", json_body={"paused": False}
        )
    )
    assert r_resume.status_code == 200
    assert r_resume.json() == {"paused": False, "boot_id": latch.boot_id}
    assert (
        run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready")).status_code == 200
    )
    assert (
        run_sync(send_asgi_request(app, "POST", "/v1/chat/completions")).status_code
        == 200
    )


def test_runtime_config_flat_parameters_schema_and_method_validation():
    app, _, latch = _build_app()

    # Apply flat parameters map and inspect response schema.
    r_apply = run_sync(
        send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-1",
                "expected_boot_id": latch.boot_id,
                "generation": 3,
                "applied_config_digest": "sha256:gen3",
                "parameters": {"scheduling.maxActiveRequests": 16},
                "reset_fields": [],
            },
        )
    )
    assert r_apply.status_code == 200
    body = r_apply.json()
    assert set(body.keys()) == {
        "engine",
        "boot_id",
        "membership_epoch",
        "observed_generation",
        "applied_config_digest",
        "last_operation_id",
        "ready",
        "paused",
        "supported_fields",
        "unsupported_fields",
        "boot_defaults",
        "ranks",
        "values",
    }
    assert body["values"]["scheduling.maxActiveRequests"] == 16
    assert body["boot_defaults"]["scheduling.maxActiveRequests"] == 64

    # Method & payload validation.
    assert (
        run_sync(send_asgi_request(app, "POST", "/gpustack/v1/ready")).status_code
        == 405
    )
    assert (
        run_sync(send_asgi_request(app, "POST", "/gpustack/v1/live")).status_code == 405
    )
    assert (
        run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ack")).status_code == 405
    )
    assert (
        run_sync(send_asgi_request(app, "DELETE", "/gpustack/v1/pause")).status_code
        == 405
    )
    assert (
        run_sync(send_asgi_request(app, "GET", "/gpustack/v1/unknown")).status_code
        == 404
    )
    assert (
        run_sync(
            send_asgi_request(
                app,
                "POST",
                "/gpustack/v1/pause",
                json_body={"paused": "not-a-bool"},
            )
        ).status_code
        == 400
    )
    assert (
        run_sync(
            send_asgi_request(
                app,
                "POST",
                "/gpustack/v1/runtime_config",
                json_body={
                    "operation_id": "op-bad",
                    "expected_boot_id": latch.boot_id,
                    "generation": 1,
                    "applied_config_digest": "d",
                    "parameters": {"unknown.knob": 1},
                },
            )
        ).status_code
        == 400
    )


def test_ensure_outermost_middleware_handles_prebuilt_middleware_stack() -> None:
    """vLLM build_app pre-builds app.middleware_stack via container standards; sentinel must rebuild safely."""
    from gpustack_engine_sentinel.latch import BootLatch
    from gpustack_engine_sentinel.server import (
        SentinelASGIMiddleware,
        SentinelControlPlane,
        ensure_outermost_sentinel_middleware,
    )

    app = FakeFastAPIApp()

    class OuterNoopMiddleware:
        def __init__(self, inner_app):
            self.app = inner_app

        async def __call__(self, scope, receive, send):
            await self.app(scope, receive, send)

    app.add_middleware(OuterNoopMiddleware)
    app.middleware_stack = app.build_middleware_stack()

    backend = DummyBackend()
    plane = SentinelControlPlane(backend=backend, latch=BootLatch())
    plane.install_routes(app)

    # First install must not raise Starlette's RuntimeError on a pre-built stack.
    ensure_outermost_sentinel_middleware(app, control_plane=plane)
    assert app.user_middleware[0].cls is SentinelASGIMiddleware
    assert app.middleware_stack is not None

    # Promotion after another middleware is appended also rebuilds safely.
    app.middleware_stack = None
    app.add_middleware(OuterNoopMiddleware)
    app.middleware_stack = app.build_middleware_stack()
    ensure_outermost_sentinel_middleware(app, control_plane=plane)
    assert app.user_middleware[0].cls is SentinelASGIMiddleware
    assert app.middleware_stack is not None

    resp = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/live"))
    assert resp.status_code == 200
