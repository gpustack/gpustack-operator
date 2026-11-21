"""Tests for universal uvicorn.Config fallback hook, idempotency with engine hooks, and pause/starting-aware readiness."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
from asgi_harness import FakeFastAPIApp, run_sync, send_asgi_request

REPO_PYTHON_ROOT = Path(__file__).resolve().parents[1]


def _write_fake_uvicorn_pkg(base_dir: Path) -> Path:
    """Create a realistic uvicorn + uvicorn.config package mirroring upstream Config.__init__ and Config.load."""
    pkg = base_dir / "fake_uvicorn_pkg"
    (pkg / "uvicorn").mkdir(parents=True)
    (pkg / "uvicorn" / "__init__.py").write_text(
        "from uvicorn.config import Config\n__all__ = ['Config']\n",
        encoding="utf-8",
    )
    (pkg / "uvicorn" / "config.py").write_text(
        """
import importlib


class ProxyHeadersMiddleware:
    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        await self.app(scope, receive, send)


class Config:
    def __init__(self, app, host="127.0.0.1", port=8000, factory=False, proxy_headers=True, interface="auto", **kwargs):
        self.app = app
        self.host = host
        self.port = port
        self.factory = factory
        self.proxy_headers = proxy_headers
        self.interface = interface
        self.loaded = False
        self.loaded_app = None

    def load_app(self):
        if isinstance(self.app, str):
            mod_name, _, attr_name = self.app.partition(":")
            mod = importlib.import_module(mod_name)
            return getattr(mod, attr_name)
        return self.app

    def load(self):
        assert not self.loaded
        self.loaded_app = self.load_app()
        try:
            self.loaded_app = self.loaded_app()
        except TypeError:
            if self.factory:
                raise
        if self.proxy_headers:
            self.loaded_app = ProxyHeadersMiddleware(self.loaded_app)
        self.loaded = True
""",
        encoding="utf-8",
    )
    return pkg


def test_uvicorn_config_fallback_hooks_standalone_fastapi_string_factory_and_raw_asgi(
    tmp_path: Path,
):
    fake_uvicorn = _write_fake_uvicorn_pkg(tmp_path)
    sglang_omni_dir = fake_uvicorn / "sglang_omni" / "serve"
    sglang_omni_dir.mkdir(parents=True)
    (fake_uvicorn / "sglang_omni" / "__init__.py").write_text("", encoding="utf-8")
    (sglang_omni_dir / "__init__.py").write_text("", encoding="utf-8")
    (sglang_omni_dir / "openai_api.py").write_text(
        """
import json
from asgi_harness import FakeFastAPIApp

async def _ok(scope, receive, send):
    body = json.dumps({"ok": True, "path": scope["path"]}).encode("utf-8")
    await send({"type": "http.response.start", "status": 200, "headers": [(b"content-type", b"application/json")]})
    await send({"type": "http.response.body", "body": body})

app = FakeFastAPIApp()
app.add_api_route("/v1/chat/completions", _ok, methods=["POST"])
app.add_api_route("/v1/images/generations", _ok, methods=["POST"])
app.add_api_route("/health", _ok, methods=["GET"])
app.add_api_route("/metrics", _ok, methods=["GET"])
""",
        encoding="utf-8",
    )

    driver = tmp_path / "run_uvicorn_fallback.py"
    driver.write_text(
        """
import json
from asgi_harness import FakeFastAPIApp, run_sync, send_asgi_request
import uvicorn

async def _ok(scope, receive, send):
    body = json.dumps({"ok": True, "path": scope["path"]}).encode("utf-8")
    await send({"type": "http.response.start", "status": 200, "headers": [(b"content-type", b"application/json")]})
    await send({"type": "http.response.body", "body": body})

# 1. Standalone FastAPI app (simulating renamed vLLM entrypoint or vllm-omni custom app) with REQUIRE_ACK=1:
standalone = FakeFastAPIApp()
standalone.add_api_route("/v1/chat/completions", _ok, methods=["POST"])
standalone.add_api_route("/health", _ok, methods=["GET"])
standalone.add_api_route("/metrics", _ok, methods=["GET"])

cfg1 = uvicorn.Config(standalone, host="0.0.0.0", port=8000)
cfg1.load()
served1 = cfg1.loaded_app

r_live = run_sync(send_asgi_request(served1, "GET", "/gpustack/v1/live"))
assert r_live.status_code == 200
boot_id = r_live.json()["boot_id"]
assert r_live.json() == {"alive": True, "boot_id": boot_id, "paused": False}

# Fail-closed 503 awaiting_ack initially, then 200 after ack!
r_ready0 = run_sync(send_asgi_request(served1, "GET", "/gpustack/v1/ready"))
assert r_ready0.status_code == 503
assert r_ready0.json()["reason"] == "awaiting_ack"

assert run_sync(send_asgi_request(
    served1,
    "POST",
    "/gpustack/v1/ack",
    json_body={"boot_id": boot_id, "observed_generation": 1, "applied_config_digest": "sha256:empty"},
)).status_code == 200
assert run_sync(send_asgi_request(served1, "GET", "/gpustack/v1/ready")).status_code == 200

# Pause gate works on fallback app AND flips /gpustack/v1/ready to 503 role_paused!
assert run_sync(send_asgi_request(served1, "POST", "/gpustack/v1/pause", json_body={"paused": True})).status_code == 200
assert run_sync(send_asgi_request(served1, "POST", "/v1/chat/completions")).status_code == 503
r_ready_p = run_sync(send_asgi_request(served1, "GET", "/gpustack/v1/ready"))
assert r_ready_p.status_code == 503
assert r_ready_p.json()["reason"] == "role_paused"
assert run_sync(send_asgi_request(served1, "GET", "/gpustack/v1/live")).status_code == 200
assert run_sync(send_asgi_request(served1, "GET", "/health")).status_code == 200
assert run_sync(send_asgi_request(served1, "GET", "/metrics")).status_code == 200
assert run_sync(send_asgi_request(served1, "POST", "/gpustack/v1/pause", json_body={"paused": False})).status_code == 200
assert run_sync(send_asgi_request(served1, "GET", "/gpustack/v1/ready")).status_code == 200
assert run_sync(send_asgi_request(served1, "POST", "/v1/chat/completions")).status_code == 200

# runtime_config reports supported_fields=[] and 422 unsupported_fields cleanly!
r_rc = run_sync(send_asgi_request(served1, "GET", "/gpustack/v1/runtime_config"))
assert r_rc.status_code == 200
assert r_rc.json()["supported_fields"] == []
assert "scheduling.maxActiveRequests" in r_rc.json()["unsupported_fields"]

r_rc_post = run_sync(send_asgi_request(
    served1,
    "POST",
    "/gpustack/v1/runtime_config",
    json_body={
        "operation_id": "op-fb",
        "expected_boot_id": boot_id,
        "generation": 2,
        "applied_config_digest": "sha256:fb",
        "parameters": {"scheduling.maxActiveRequests": 8},
    },
))
assert r_rc_post.status_code == 422
assert r_rc_post.json()["unsupported_fields"] == ["scheduling.maxActiveRequests"]

# 2. String app path (simulating sglang_omni.serve.openai_api:app via uvicorn.Config("pkg:app")):
cfg_str = uvicorn.Config("sglang_omni.serve.openai_api:app", port=8001)
cfg_str.load()
assert run_sync(send_asgi_request(cfg_str.loaded_app, "GET", "/gpustack/v1/live")).status_code == 200
assert run_sync(send_asgi_request(cfg_str.loaded_app, "GET", "/gpustack/v1/runtime_config")).json()["engine"] == "sglang"

# 3. Raw ASGI3 function callable (no FastAPI/Starlette methods at all):
cfg_raw = uvicorn.Config(_ok, port=8002)
cfg_raw.load()
assert run_sync(send_asgi_request(cfg_raw.loaded_app, "GET", "/gpustack/v1/live")).status_code == 200
assert run_sync(send_asgi_request(cfg_raw.loaded_app, "POST", "/gpustack/v1/pause", json_body={"paused": True})).status_code == 200
assert run_sync(send_asgi_request(cfg_raw.loaded_app, "POST", "/v1/chat/completions")).status_code == 503
assert run_sync(send_asgi_request(cfg_raw.loaded_app, "GET", "/metrics")).status_code == 200

print("UVICORN_FALLBACK_OK")
""",
        encoding="utf-8",
    )

    env = dict(os.environ)
    env["GPUSTACK_ENGINE_SENTINEL_ENABLE"] = "1"
    env.pop("GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK", None)
    env["PYTHONPATH"] = os.pathsep.join(
        [
            str(REPO_PYTHON_ROOT),
            str(fake_uvicorn),
            str(Path(__file__).resolve().parent),
        ]
    )

    res = subprocess.run(
        [sys.executable, str(driver)],
        env=env,
        capture_output=True,
        text=True,
        check=False,
    )
    assert res.returncode == 0, f"STDOUT:\n{res.stdout}\nSTDERR:\n{res.stderr}"
    assert "UVICORN_FALLBACK_OK" in res.stdout


def test_uvicorn_config_is_idempotent_when_app_already_wrapped_by_vllm_or_sglang(
    tmp_path: Path,
):
    from gpustack_engine_sentinel.sglang.hooks import patch_sglang_http_server_module
    from gpustack_engine_sentinel.uvicorn_hook import patch_uvicorn_config_module
    from gpustack_engine_sentinel.vllm.hooks import install_vllm_sentinel_on_app

    fake_uvicorn = _write_fake_uvicorn_pkg(tmp_path)
    sys.path.insert(0, str(fake_uvicorn))
    try:
        import importlib

        uvicorn_config_mod = importlib.import_module("uvicorn.config")
        patch_uvicorn_config_module(uvicorn_config_mod)

        # 1. Pre-hooked vLLM app keeps VLLMSentinelBackend and identical boot_id + single middleware entry!
        vllm_app = FakeFastAPIApp()
        vllm_plane = install_vllm_sentinel_on_app(vllm_app)
        orig_boot_id = vllm_plane.latch.boot_id
        orig_mw_len = len(vllm_app.user_middleware)

        cfg_vllm = uvicorn_config_mod.Config(vllm_app)
        cfg_vllm.load()
        assert vllm_app._gpustack_sentinel_plane is vllm_plane
        assert len(vllm_app.user_middleware) == orig_mw_len
        r_live = run_sync(
            send_asgi_request(cfg_vllm.loaded_app, "GET", "/gpustack/v1/live")
        )
        assert r_live.json()["boot_id"] == orig_boot_id

        # 2. Pre-hooked SGLang app keeps SGLangSentinelBackend and identical boot_id!
        sgl_app = FakeFastAPIApp()
        fake_sgl_mod = SimpleNamespace(
            app=sgl_app,
            get_global_state=lambda: SimpleNamespace(
                tokenizer_manager=SimpleNamespace(is_pause=False)
            ),
        )
        patch_sglang_http_server_module(fake_sgl_mod)
        sgl_plane = sgl_app._gpustack_sentinel_plane
        sgl_boot_id = sgl_plane.latch.boot_id

        cfg_sgl = uvicorn_config_mod.Config(sgl_app)
        cfg_sgl.load()
        assert sgl_app._gpustack_sentinel_plane is sgl_plane
        r_sgl = run_sync(
            send_asgi_request(cfg_sgl.loaded_app, "GET", "/gpustack/v1/live")
        )
        assert r_sgl.json()["boot_id"] == sgl_boot_id
    finally:
        sys.path.remove(str(fake_uvicorn))
        sys.modules.pop("uvicorn.config", None)
        sys.modules.pop("uvicorn", None)


def test_ready_503_when_sglang_starting_and_starts_open_with_require_ack_zero(
    monkeypatch: pytest.MonkeyPatch,
):
    from gpustack_engine_sentinel.sglang.hooks import patch_sglang_http_server_module

    monkeypatch.setenv("GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK", "0")

    state_holder: dict[str, Any] = {"tm": None}

    class FakeTM:
        def __init__(self) -> None:
            self.ready_flag = False
            self.is_pause = False
            self.gracefully_exit = False
            self.server_status = "Starting"

        def is_ready(self) -> bool:
            return not self.is_pause and self.ready_flag

    sgl_app = FakeFastAPIApp()
    fake_http_mod = SimpleNamespace(
        app=sgl_app,
        get_global_state=lambda: (
            None
            if state_holder["tm"] is None
            else SimpleNamespace(tokenizer_manager=state_holder["tm"])
        ),
    )
    patch_sglang_http_server_module(fake_http_mod)

    # 1. Before tokenizer_manager is initialized: live is 200, ready is 503 (engine_starting)!
    assert (
        run_sync(send_asgi_request(sgl_app, "GET", "/gpustack/v1/live")).status_code
        == 200
    )
    r_uninit = run_sync(send_asgi_request(sgl_app, "GET", "/gpustack/v1/ready"))
    assert r_uninit.status_code == 503
    assert r_uninit.json()["reason"] == "engine_starting"

    # 2. TokenizerManager exists with is_ready() == False (ServerStatus.Starting): ready stays 503 (engine_starting)!
    tm = FakeTM()
    state_holder["tm"] = tm
    assert (
        run_sync(send_asgi_request(sgl_app, "GET", "/gpustack/v1/live")).status_code
        == 200
    )
    r_starting = run_sync(send_asgi_request(sgl_app, "GET", "/gpustack/v1/ready"))
    assert r_starting.status_code == 503
    assert r_starting.json()["reason"] == "engine_starting"

    # 3. Engine finishes startup (is_ready() == True) and REQUIRE_ACK=0 -> ready opens to 200 automatically!
    tm.server_status = "Up"
    tm.ready_flag = True
    r_up = run_sync(send_asgi_request(sgl_app, "GET", "/gpustack/v1/ready"))
    assert r_up.status_code == 200
    assert r_up.json()["ready"] is True

    # 4. Pausing via /gpustack/v1/pause flips /gpustack/v1/ready to 503 (role_paused) while /live stays 200!
    assert (
        run_sync(
            send_asgi_request(
                sgl_app, "POST", "/gpustack/v1/pause", json_body={"paused": True}
            )
        ).status_code
        == 200
    )
    r_paused = run_sync(send_asgi_request(sgl_app, "GET", "/gpustack/v1/ready"))
    assert r_paused.status_code == 503
    assert r_paused.json()["reason"] == "role_paused"
    assert (
        run_sync(send_asgi_request(sgl_app, "GET", "/gpustack/v1/live")).status_code
        == 200
    )

    # 5. Unpausing restores /gpustack/v1/ready to 200 immediately!
    assert (
        run_sync(
            send_asgi_request(
                sgl_app, "POST", "/gpustack/v1/pause", json_body={"paused": False}
            )
        ).status_code
        == 200
    )
    assert (
        run_sync(send_asgi_request(sgl_app, "GET", "/gpustack/v1/ready")).status_code
        == 200
    )


def test_uvicorn_config_load_skips_wsgi_interface():
    from gpustack_engine_sentinel.uvicorn_hook import (
        is_sentinel_installed,
        patch_uvicorn_config_module,
    )

    def wsgi_app(environ: Any, start_response: Any) -> list[bytes]:
        del environ, start_response
        return [b"ok"]

    class FakeConfig:
        def __init__(self, app: Any, *, interface: str = "auto") -> None:
            self.app = app
            self.interface = interface
            self.loaded_app: Any = None

        def load(self) -> None:
            self.loaded_app = self.app

    fake_mod = SimpleNamespace(Config=FakeConfig)
    patch_uvicorn_config_module(fake_mod)

    cfg = FakeConfig(wsgi_app, interface="wsgi")
    cfg.load()
    assert cfg.loaded_app is wsgi_app
    assert not is_sentinel_installed(cfg.loaded_app)
