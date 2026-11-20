"""Tests for fresh-interpreter SGLang spawn children, in-process internal-state controls, and HTTP frontend."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path
from typing import Any

REPO_PYTHON_ROOT = Path(__file__).resolve().parents[1]


def _create_fake_sglang_tree(base_dir: Path) -> Path:
    """Writes a minimal realistic sglang.srt package hierarchy so spawned child interpreters import it cleanly."""
    pkg = base_dir / "fake_sglang_pkg"
    (pkg / "sglang" / "srt" / "managers" / "scheduler_components").mkdir(parents=True)
    (pkg / "sglang" / "srt" / "entrypoints").mkdir(parents=True)
    (pkg / "sglang" / "srt" / "utils").mkdir(parents=True)

    for init_dir in (
        pkg / "sglang",
        pkg / "sglang" / "srt",
        pkg / "sglang" / "srt" / "managers",
        pkg / "sglang" / "srt" / "managers" / "scheduler_components",
        pkg / "sglang" / "srt" / "entrypoints",
        pkg / "sglang" / "srt" / "utils",
    ):
        (init_dir / "__init__.py").write_text("", encoding="utf-8")

    (pkg / "sglang" / "srt" / "runtime_context.py").write_text(
        """
from types import SimpleNamespace

class _Ctx:
    def __init__(self):
        self.schedule = SimpleNamespace(
            schedule_conservativeness=1.0,
            prefill_max_requests=None,
            chunked_prefill_size=2048,
            prefill_decode_interval=0,
        )
        self.parallel = SimpleNamespace(
            dp_size=2, tp_size=1, pp_size=2, dp_rank=0, pp_max_micro_batch_size=16
        )
        self.overrides = {
            "speculative_accept_threshold_single": 1.0,
            "speculative_accept_threshold_acc": 1.0,
        }

    def override(self, source="test", **kwargs):
        self.overrides.update(kwargs)
        for k, v in kwargs.items():
            if hasattr(self.schedule, k):
                setattr(self.schedule, k, v)
            if hasattr(self.parallel, k):
                setattr(self.parallel, k, v)

    def resolved_server_args_dict(self):
        return {
            "schedule_conservativeness": self.schedule.schedule_conservativeness,
            "prefill_max_requests": self.schedule.prefill_max_requests,
            "chunked_prefill_size": self.schedule.chunked_prefill_size,
            "prefill_decode_interval": self.schedule.prefill_decode_interval,
            "pp_max_micro_batch_size": self.parallel.pp_max_micro_batch_size,
            **self.overrides,
        }

_CTX = _Ctx()
def get_context():
    return _CTX
def get_schedule():
    return _CTX.schedule
def get_parallel():
    return _CTX.parallel
""",
        encoding="utf-8",
    )

    (pkg / "sglang" / "srt" / "managers" / "io_struct.py").write_text(
        """
from dataclasses import dataclass, field
from typing import Any

@dataclass
class GetInternalStateReq:
    pass

@dataclass
class GetInternalStateReqOutput:
    internal_state: dict[str, Any]

@dataclass
class SetInternalStateReq:
    server_args: dict[str, Any] = field(default_factory=dict)

@dataclass
class SetInternalStateReqOutput:
    updated: bool
""",
        encoding="utf-8",
    )

    (
        pkg
        / "sglang"
        / "srt"
        / "managers"
        / "scheduler_components"
        / "new_token_ratio_tracker.py"
    ).write_text(
        """
from dataclasses import dataclass
from sglang.srt.runtime_context import get_schedule

@dataclass
class NewTokenRatioTracker:
    init: float
    min: float
    decay: float
    current: float

    @classmethod
    def from_config(cls):
        init = min(0.7 * get_schedule().schedule_conservativeness, 1.0)
        min_r = min(init * 0.4, 1.0)
        decay = (init - min_r) / 600.0
        return cls(init=init, min=min_r, decay=decay, current=init)
""",
        encoding="utf-8",
    )

    (
        pkg
        / "sglang"
        / "srt"
        / "managers"
        / "scheduler_components"
        / "internal_state.py"
    ).write_text(
        """
from typing import Any, Callable, Dict, Optional
from sglang.srt.managers.io_struct import (
    GetInternalStateReq,
    GetInternalStateReqOutput,
    SetInternalStateReq,
    SetInternalStateReqOutput,
)
from sglang.srt.runtime_context import get_context, get_parallel

class UpdateRejected(Exception):
    pass

class _UpdateHandler:
    def __init__(self, validate: Callable[[str, Any], None], apply: Callable[[str, Any], Optional[Dict[str, Any]]]):
        self.validate = validate
        self.apply = apply

def _override(key: str, value: Any) -> Dict[str, Any]:
    return {key: value}

class SchedulerInternalStateController:
    def __init__(self, scheduler: Any, record_step_time: bool = False) -> None:
        self.scheduler = scheduler
        self.record_step_time = record_step_time

    def get_internal_state(self, recv_req: GetInternalStateReq) -> GetInternalStateReqOutput:
        state = dict(get_context().resolved_server_args_dict())
        state["effective_max_running_requests_per_dp"] = self.scheduler.max_running_requests
        return GetInternalStateReqOutput(internal_state=state)

    def set_internal_state(self, recv_req: SetInternalStateReq) -> SetInternalStateReqOutput:
        handlers = self._update_handlers()
        requested = recv_req.server_args
        try:
            for key, value in requested.items():
                handler = handlers.get(key)
                if handler is None:
                    raise UpdateRejected(f"Updating {key} is not supported.")
                handler.validate(key, value)
        except UpdateRejected:
            return SetInternalStateReqOutput(updated=False)
        overrides: dict[str, Any] = {}
        for key, value in requested.items():
            overrides.update(handlers[key].apply(key, value) or {})
        if overrides:
            get_context().override(source="update_server_args", **overrides)
        return SetInternalStateReqOutput(updated=True)

    def _update_handlers(self) -> Dict[str, _UpdateHandler]:
        return {
            "pp_max_micro_batch_size": _UpdateHandler(
                self._validate_pp_micro_batch_size, _override
            ),
            "speculative_accept_threshold_single": _UpdateHandler(
                self._validate_accept_threshold, self._apply_accept_threshold
            ),
            "speculative_accept_threshold_acc": _UpdateHandler(
                self._validate_accept_threshold, self._apply_accept_threshold
            ),
            "dspark_force_budget_frac": _UpdateHandler(
                self._validate_dspark_budget_frac, self._apply_dspark_budget_frac
            ),
        }

    def _validate_pp_micro_batch_size(self, key: str, value: Any) -> None:
        upper = self.scheduler.max_running_requests // get_parallel().pp_size
        if value < 1 or value > upper:
            raise UpdateRejected("pp_max_micro_batch_size out of range")

    def _validate_accept_threshold(self, key: str, value: Any) -> None:
        if not (0.0 <= float(value) <= 1.0):
            raise UpdateRejected("threshold out of range")

    def _apply_accept_threshold(self, key: str, value: Any) -> Dict[str, Any]:
        self.scheduler.metrics_reporter.spec_total_num_accept_tokens = 0
        self.scheduler.metrics_reporter.spec_total_num_forward_ct = 0
        return {key: value}

    def _validate_dspark_budget_frac(self, key: str, value: Any) -> None:
        if value is not None and not (0.0 < float(value) <= 1.0):
            raise UpdateRejected("out of range")

    def _apply_dspark_budget_frac(self, key: str, value: Any) -> None:
        self.scheduler.draft_worker.set_dspark_forced_budget_frac(
            None if value is None else float(value)
        )
""",
        encoding="utf-8",
    )

    (pkg / "sglang" / "srt" / "managers" / "scheduler.py").write_text(
        """
from types import SimpleNamespace
from sglang.srt.runtime_context import get_parallel, get_schedule
from sglang.srt.managers.scheduler_components.internal_state import SchedulerInternalStateController
from sglang.srt.managers.scheduler_components.new_token_ratio_tracker import NewTokenRatioTracker

class FakeDraftWorker:
    def __init__(self):
        self._verify_planner = SimpleNamespace(
            _budget_planner=SimpleNamespace(forced_budget_frac=0.45)
        )
    def set_dspark_forced_budget_frac(self, frac):
        self._verify_planner._budget_planner.forced_budget_frac = frac

class Scheduler:
    def __init__(self, dp_rank: int = 0, max_running_requests: int = 32, page_size: int = 16, chunked_prefill_size: int = 2048):
        get_parallel().dp_rank = dp_rank
        self.dp_rank = dp_rank
        self.max_running_requests = max_running_requests
        self.page_size = page_size
        self.truncation_align_size = None
        self.max_prefill_tokens = 4096
        self.chunked_prefill_size = chunked_prefill_size
        self.prefill_decode_interval = get_schedule().prefill_decode_interval
        self._prefill_decode_interval_remaining = 6
        self.req_to_token_pool = SimpleNamespace(
            size=max_running_requests,
            available_size=lambda: max_running_requests - self._running_bs,
        )
        self._running_bs = 10
        self.running_batch = SimpleNamespace(batch_is_full=True)
        self.tp_worker = SimpleNamespace(
            model_runner=SimpleNamespace(max_running_requests=max_running_requests)
        )
        self.prefill_delayer = SimpleNamespace(_prefill_max_requests=None)
        self.new_token_ratio_tracker = NewTokenRatioTracker.from_config()
        self.spec_algorithm = SimpleNamespace(
            is_none=lambda: False,
            is_dflash=lambda: True,
            is_dflash_family=lambda: True,
        )
        self.metrics_reporter = SimpleNamespace(
            spec_total_num_accept_tokens=100,
            spec_total_num_forward_ct=25,
        )
        self.draft_worker = FakeDraftWorker()
        self.pause_state = SimpleNamespace(paused=False)
        self.internal_state_controller = SchedulerInternalStateController(self)

    def get_num_allocatable_reqs(self, running_bs: int, beam_width=None, running_batch=None, num_held_rows: int = 0, admitted_reqs=()):
        row_budget = max(self.max_running_requests - running_bs + num_held_rows, 0)
        if beam_width is not None:
            return row_budget // beam_width
        return row_budget
""",
        encoding="utf-8",
    )

    # Include realistic SGLang ADMIN_FORCE /set_internal_state guard when SGLANG_ADMIN_API_KEY is unset!
    (pkg / "sglang" / "srt" / "entrypoints" / "http_server.py").write_text(
        """
import json
import os
from asgi_harness import FakeFastAPIApp

class _SGLangAdminGuardMiddleware:
    def __init__(self, app):
        self.app = app
    async def __call__(self, scope, receive, send):
        if scope.get("type") == "http" and scope.get("path") == "/set_internal_state":
            if not os.environ.get("SGLANG_ADMIN_API_KEY"):
                body = json.dumps({"error": "ADMIN_API_KEY_MISSING"}).encode("utf-8")
                await send({"type": "http.response.start", "status": 403, "headers": [(b"content-type", b"application/json")]})
                await send({"type": "http.response.body", "body": body})
                return
        await self.app(scope, receive, send)

async def _ok_route(scope, receive, send):
    body = json.dumps({"ok": True}).encode("utf-8")
    await send({"type": "http.response.start", "status": 200, "headers": [(b"content-type", b"application/json")]})
    await send({"type": "http.response.body", "body": body})

app = FakeFastAPIApp()
app.add_api_route("/v1/chat/completions", _ok_route, methods=["POST"])
app.add_api_route("/health", _ok_route, methods=["GET"])
app.add_api_route("/metrics", _ok_route, methods=["GET"])
app.add_middleware(_SGLangAdminGuardMiddleware)

_global_state = None

def set_global_state(gs):
    global _global_state
    _global_state = gs

def get_global_state():
    return _global_state
""",
        encoding="utf-8",
    )

    return pkg


def _spawned_scheduler_worker(dp_rank: int, cmd_conn: Any) -> None:
    """Runs inside a freshly spawned child interpreter where sitecustomize.py activated automatically!"""
    from sglang.srt.managers.io_struct import GetInternalStateReq, SetInternalStateReq
    from sglang.srt.managers.scheduler import Scheduler
    from sglang.srt.runtime_context import get_context, get_parallel, get_schedule

    sched = Scheduler(
        dp_rank=dp_rank,
        max_running_requests=32,
        page_size=16,
        chunked_prefill_size=2048,
    )
    while True:
        msg = cmd_conn.recv()
        op = msg["op"]
        if op == "stop":
            break
        if op == "get_internal_state":
            out = sched.internal_state_controller.get_internal_state(
                GetInternalStateReq()
            )
            cmd_conn.send(out.internal_state)
        elif op == "set_internal_state":
            if msg.get("force_fail_rank") == dp_rank:
                cmd_conn.send(False)
                continue
            out = sched.internal_state_controller.set_internal_state(
                SetInternalStateReq(server_args=msg["server_args"])
            )
            cmd_conn.send(out.updated)
        elif op == "probe_mechanics":
            running_bs = int(msg["running_bs"])
            sched._running_bs = running_bs
            resolved = get_context().resolved_server_args_dict()
            cmd_conn.send(
                {
                    "allocatable": sched.get_num_allocatable_reqs(running_bs),
                    "physical_max_running": sched.max_running_requests,
                    "runner_max_running": sched.tp_worker.model_runner.max_running_requests,
                    "pool_size": sched.req_to_token_pool.size,
                    "batch_is_full": sched.running_batch.batch_is_full,
                    "chunked_prefill_size": sched.chunked_prefill_size,
                    "prefill_max_requests": get_schedule().prefill_max_requests,
                    "delayer_prefill_max_requests": sched.prefill_delayer._prefill_max_requests,
                    "schedule_conservativeness": get_schedule().schedule_conservativeness,
                    "tracker_init": sched.new_token_ratio_tracker.init,
                    "tracker_current": sched.new_token_ratio_tracker.current,
                    "prefill_decode_interval": sched.prefill_decode_interval,
                    "prefill_decode_remaining": sched._prefill_decode_interval_remaining,
                    "pp_max_micro_batch_size": get_parallel().pp_max_micro_batch_size,
                    "accept_single": resolved.get(
                        "speculative_accept_threshold_single"
                    ),
                    "accept_acc": resolved.get("speculative_accept_threshold_acc"),
                    "spec_forward_ct": sched.metrics_reporter.spec_total_num_forward_ct,
                    "dspark_budget": sched.draft_worker._verify_planner._budget_planner.forced_budget_frac,
                }
            )


def test_sglang_fresh_spawn_children_inprocess_controls_zero_admin_api_key(
    tmp_path: Path,
):
    staged_dir = tmp_path / "stage_out"
    subprocess.run(
        [
            sys.executable,
            str(REPO_PYTHON_ROOT / "stage_build.py"),
            "--output-dir",
            str(staged_dir),
            "--version",
            "0.12.0",
        ],
        cwd=REPO_PYTHON_ROOT,
        check=True,
    )
    fake_sglang_dir = _create_fake_sglang_tree(tmp_path)

    driver_script = tmp_path / "run_spawn_test.py"
    driver_script.write_text(
        """
import multiprocessing
import os
from types import SimpleNamespace

from asgi_harness import run_sync, send_asgi_request
from test_sglang_spawn import _spawned_scheduler_worker
import sglang.srt.entrypoints.http_server as http_server
from sglang.srt.managers.io_struct import SetInternalStateReq

def main():
    assert "SGLANG_ADMIN_API_KEY" not in os.environ
    ctx = multiprocessing.get_context("spawn")
    conns = []
    procs = []
    for rank in (0, 1):
        parent_conn, child_conn = ctx.Pipe()
        p = ctx.Process(target=_spawned_scheduler_worker, args=(rank, child_conn))
        p.start()
        conns.append(parent_conn)
        procs.append(p)

    force_fail_rank = None

    class ParentTokenizerManager:
        def __init__(self):
            self.is_pause = False
            self._subprocess_watchdog = SimpleNamespace(_processes=procs)

        async def get_internal_state(self):
            for c in conns:
                c.send({"op": "get_internal_state"})
            return [c.recv() for c in conns]

        async def set_internal_state(self, obj: SetInternalStateReq):
            for c in conns:
                c.send({
                    "op": "set_internal_state",
                    "server_args": obj.server_args,
                    "force_fail_rank": force_fail_rank,
                })
            return [c.recv() for c in conns]

    try:
        tm = ParentTokenizerManager()
        http_server.set_global_state(SimpleNamespace(tokenizer_manager=tm, scheduler_info={}))
        app = http_server.app

        # 1. Fail-closed readiness initially, live is 200, /set_internal_state over HTTP is 403 without SGLANG_ADMIN_API_KEY!
        assert run_sync(send_asgi_request(app, "GET", "/gpustack/v1/ready")).status_code == 503
        assert run_sync(send_asgi_request(app, "GET", "/gpustack/v1/live")).status_code == 200
        assert run_sync(send_asgi_request(app, "POST", "/set_internal_state")).status_code == 403

        # 2. GET /gpustack/v1/runtime_config reads boot_defaults and live values from spawned children!
        r_info = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
        assert r_info.status_code == 200, r_info.body
        info = r_info.json()
        assert info["engine"] == "sglang"
        assert len(info["ranks"]) == 2
        assert info["boot_defaults"]["scheduling.maxActiveRequests"] == 32
        assert info["boot_defaults"]["scheduling.maxBatchedTokens"] == 2048
        assert info["boot_defaults"]["scheduling.ppMaxMicroBatchSize"] == 16
        assert abs(info["values"]["speculative.draftBudgetFraction"] - 0.45) < 1e-6
        boot_id = info["boot_id"]

        # 3. Non-page-aligned maxBatchedTokens (1000 % 16 != 0) is rejected cleanly!
        r_unaligned = run_sync(send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-unaligned",
                "expected_boot_id": boot_id,
                "generation": 2,
                "applied_config_digest": "sha256:bad",
                "parameters": {"scheduling.maxBatchedTokens": 1000},
            },
        ))
        assert r_unaligned.status_code == 400, r_unaligned.body

        # 4. Apply all 9 SGLang live controls (including pp_max_micro_batch_size, speculative_accept_threshold_*,
        # and dspark_force_budget_frac) IN-PROCESS across both spawned ranks with ZERO SGLANG_ADMIN_API_KEY!
        r_apply = run_sync(send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-apply-1",
                "expected_boot_id": boot_id,
                "generation": 3,
                "applied_config_digest": "sha256:sglang-gen3",
                "parameters": {
                    "scheduling.maxActiveRequests": 6,
                    "scheduling.maxBatchedTokens": 1024,
                    "scheduling.prefillMaxRequests": 2,
                    "scheduling.scheduleConservativeness": "0.5",
                    "scheduling.prefillDecodeInterval": 3,
                    "scheduling.ppMaxMicroBatchSize": 8,
                    "speculative.acceptThresholdSingle": "0.9",
                    "speculative.acceptThresholdAcc": "0.85",
                    "speculative.draftBudgetFraction": "0.8",
                },
            },
        ))
        assert r_apply.status_code == 200, r_apply.body
        applied = r_apply.json()
        assert applied["applied_config_digest"] == "sha256:sglang-gen3"
        assert all(rk["applied_digest"] == "sha256:sglang-gen3" for rk in applied["ranks"])

        for c in conns:
            c.send({"op": "probe_mechanics", "running_bs": 10})
            m = c.recv()
            assert m["allocatable"] == 0
            assert m["physical_max_running"] == 32
            assert m["runner_max_running"] == 32
            assert m["pool_size"] == 32
            assert m["chunked_prefill_size"] == 1024
            assert m["prefill_max_requests"] == 2
            assert m["delayer_prefill_max_requests"] == 2
            assert abs(m["schedule_conservativeness"] - 0.5) < 1e-6
            assert abs(m["tracker_init"] - 0.35) < 1e-6
            assert abs(m["tracker_current"] - 0.35) < 1e-6
            assert m["prefill_decode_interval"] == 3
            assert m["prefill_decode_remaining"] == 3
            assert m["pp_max_micro_batch_size"] == 8
            assert abs(m["accept_single"] - 0.9) < 1e-6
            assert abs(m["accept_acc"] - 0.85) < 1e-6
            assert m["spec_forward_ct"] == 0
            assert abs(m["dspark_budget"] - 0.8) < 1e-6

            c.send({"op": "probe_mechanics", "running_bs": 4})
            assert c.recv()["allocatable"] == 2

        # 5. Setting prefillDecodeInterval=0 clears _prefill_decode_interval_remaining to 0 immediately,
        # and resetting fields restores boot defaults!
        r_zero = run_sync(send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-zero",
                "expected_boot_id": boot_id,
                "generation": 4,
                "applied_config_digest": "sha256:sglang-gen4",
                "parameters": {"scheduling.prefillDecodeInterval": 0},
                "reset_fields": [
                    "scheduling.maxActiveRequests",
                    "scheduling.prefillMaxRequests",
                    "scheduling.scheduleConservativeness",
                    "scheduling.ppMaxMicroBatchSize",
                    "speculative.acceptThresholdSingle",
                ],
            },
        ))
        assert r_zero.status_code == 200, r_zero.body
        for c in conns:
            c.send({"op": "probe_mechanics", "running_bs": 10})
            m2 = c.recv()
            assert m2["prefill_decode_interval"] == 0
            assert m2["prefill_decode_remaining"] == 0
            assert m2["allocatable"] == 22
            assert m2["batch_is_full"] is False
            assert m2["prefill_max_requests"] is None
            assert m2["pp_max_micro_batch_size"] == 16
            assert abs(m2["accept_single"] - 1.0) < 1e-6
            assert abs(m2["tracker_init"] - 0.7) < 1e-6

        # 6. Per-rank boolean check: if rank 1 returns False, convergence is rejected and rank 0 rolls back!
        force_fail_rank = 1
        r_partial = run_sync(send_asgi_request(
            app,
            "POST",
            "/gpustack/v1/runtime_config",
            json_body={
                "operation_id": "op-fail-rank1",
                "expected_boot_id": boot_id,
                "generation": 5,
                "applied_config_digest": "sha256:should-not-stick",
                "parameters": {"scheduling.maxActiveRequests": 3},
            },
        ))
        assert r_partial.status_code >= 400
        force_fail_rank = None

        conns[0].send({"op": "probe_mechanics", "running_bs": 10})
        assert conns[0].recv()["allocatable"] == 22
        r_after_fail = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/runtime_config"))
        assert r_after_fail.json()["applied_config_digest"] == "sha256:sglang-gen4"
        assert all(rk["applied_digest"] == "sha256:sglang-gen4" for rk in r_after_fail.json()["ranks"])

        # 7. ASGI /gpustack/v1/pause rejects /v1/chat/completions with 503 while /gpustack/v1/live stays 200;
        # crashed scheduler child flips /gpustack/v1/live to 503!
        assert run_sync(send_asgi_request(app, "POST", "/gpustack/v1/pause", json_body={"paused": True})).status_code == 200
        assert run_sync(send_asgi_request(app, "POST", "/v1/chat/completions")).status_code == 503
        r_paused_live = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/live"))
        assert r_paused_live.status_code == 200
        assert r_paused_live.json()["paused"] is True

        tm._subprocess_watchdog._processes = [SimpleNamespace(is_alive=lambda: False, exitcode=1)]
        r_crashed_live = run_sync(send_asgi_request(app, "GET", "/gpustack/v1/live"))
        assert r_crashed_live.status_code == 503
        assert r_crashed_live.json()["alive"] is False

        print("SPAWN_TEST_OK")
    finally:
        for c in conns:
            try:
                c.send({"op": "stop"})
            except Exception:
                pass
        for p in procs:
            p.join(timeout=5)
            if p.is_alive():
                p.terminate()

if __name__ == "__main__":
    main()
""",
        encoding="utf-8",
    )

    env = dict(os.environ)
    env["GPUSTACK_ENGINE_SENTINEL_ENABLE"] = "1"
    env.pop("SGLANG_ADMIN_API_KEY", None)
    env.pop("VLLM_PLUGINS", None)
    tests_dir = str(Path(__file__).resolve().parent)
    env["PYTHONPATH"] = os.pathsep.join(
        [str(staged_dir), str(fake_sglang_dir), tests_dir]
    )

    res = subprocess.run(
        [sys.executable, str(driver_script)],
        env=env,
        capture_output=True,
        text=True,
        check=False,
    )
    assert res.returncode == 0, f"STDOUT:\n{res.stdout}\nSTDERR:\n{res.stderr}"
    assert "SPAWN_TEST_OK" in res.stdout
