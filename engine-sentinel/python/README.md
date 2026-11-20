# GPUStack Engine Sentinel

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](./LICENSE)

`gpustack-engine-sentinel` is the zero-dependency, host-staged pure-Python runtime sentinel for GPUStack Operator's `ModelDeployment` workloads (`vLLM`, `vllm-ascend`, `vllm-omni`, `SGLang`, and `sglang-omni`). It provides unified readiness and liveness probes, an ASGI pause gate, in-process online parameter control, and the `PreStop` graceful drain CLI without mutating `VLLM_PLUGINS`, container `argv`, or `--load-format`.

---

## How It Works

`gpustack-engine-sentinel` runs inside inference engine containers purely from a read-only `PYTHONPATH` mount:

1. **Zero-Intrusion `sitecustomize.py` Hooking**
   The package stays completely dormant unless `GPUSTACK_ENGINE_SENTINEL_ENABLE=1` is set. When enabled, `sitecustomize.py` chains downstream `sitecustomize.py` modules with a visited-path anti-recursion guard (`sys._gpustack_sitecustomize_visited`) and registers lazy `sys.meta_path` post-import hooks. Because it never modifies `VLLM_PLUGINS` or wraps container `argv`, `vllm-ascend`, `vllm-omni` (`vllm_omni.general_plugins` / `vllm_omni.platform_plugins`), `model-transfer`, and `gpustack_modelexpress` remain untouched.

2. **Universal `uvicorn.Config` Fallback & Drift Resilience**
   In addition to engine-specific hooks for `vLLM` (`EngineCore` multi-rank `call_utility_all_async` RPC) and `SGLang` (`spawn`-safe `Scheduler` child hooks + in-process `SchedulerInternalStateController`), the sentinel idempotently hooks `uvicorn.config.Config`. Even if a future engine release refactors its FastAPI entrypoint—or runs `vllm-omni` / `sglang-omni`—`/gpustack/v1/{live,ready,pause}` remain mounted and never return `404`. Unsupported tuning fields degrade cleanly with HTTP `422` (`unsupported_fields`) and atomic rollback without crashing the engine.

3. **Unified Probes & Boot-Local Readiness Latch**
   - `GET /gpustack/v1/live`: Returns `200` while the engine process is healthy (including while starting or paused), and `503` on `EngineDeadError` or crashed scheduler subprocesses—without triggering SGLang token generation.
   - `GET /gpustack/v1/ready`: Returns `503` while the engine is starting (`engine_starting`), paused (`role_paused`), unhealthy (`engine_not_alive`), or awaiting controller acknowledgement (`awaiting_ack` when `GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK=1`), and `200` once ready, unpaused, and latched open via `POST /gpustack/v1/ack`.

4. **Pure-ASGI Role Pause Gate**
   `POST /gpustack/v1/pause` toggles role pause mode in-process (requiring zero `VLLM_SERVER_DEV_MODE=1`). While paused, direct inference requests outside `/gpustack/v1/*`, `/health`, and `/metrics` receive `503 Service Unavailable` (`role_paused`) and `/gpustack/v1/ready` returns `503`.

5. **PreStop Graceful Drain CLI**
   `python3 -m gpustack_engine_sentinel.drain` polls local `/metrics` after a settle window and exits `0` after two consecutive zero-in-flight reads or at the configured deadline, replacing inline Python `PreStop` scripts.

---

## Package Layout

| Module | Description |
| --- | --- |
| `sitecustomize.py` | Opt-in `PYTHONPATH` entrypoint with visited-path anti-recursion guard and lazy bootstrap. |
| `gpustack_engine_sentinel.protocol` | Frozen `/gpustack/v1/*` wire schema constants, canonical field registry, and request/response validators. |
| `gpustack_engine_sentinel.latch` | Thread-safe `BootLatch` minting per-process `boot_id` UUIDs and gating `/gpustack/v1/ready`. |
| `gpustack_engine_sentinel.server` | Pure-ASGI `SentinelASGIMiddleware` and `RuntimeControlPlane` serving `/gpustack/v1/{ready,live,ack,pause,runtime_config}`. |
| `gpustack_engine_sentinel.uvicorn_hook` | Universal idempotent `uvicorn.config.Config` fallback hook and `FallbackSentinelBackend`. |
| `gpustack_engine_sentinel.vllm` | `vLLM` FastAPI and `EngineCore` multi-rank utility RPC hooks (`max_num_active_reqs`, `max_num_scheduled_tokens`, `cache_aware_*`, KV-load caps). |
| `gpustack_engine_sentinel.sglang` | `spawn`-safe `SGLang` `Scheduler` and `SchedulerInternalStateController` hooks (live admission gate, `NewTokenRatioTracker` rebase, `_prefill_decode_interval_remaining` clamp, speculative thresholds). |
| `gpustack_engine_sentinel.drain` | `PreStop` graceful drain CLI polling `vLLM` and `SGLang` Prometheus `/metrics` gauges. |
| `stage_build.py` | Pure-stdlib unpacked staging builder stamping the `gpustack-operator` release version into `.dist-info`. |

---

## Usage

### 1. Host Staging in Operator Containers

`gpustack-engine-sentinel` has zero third-party runtime dependencies in `pyproject.toml`, and its package version tracks `gpustack-operator` via `hatch-vcs` (matching `gpustack-model-transfer`). During the `gpustack-operator` image build (`pack/gpustack-operator/Dockerfile`), `stage_build.py` stages the flat unpacked directory into `/etc/gpustack/lib/engine-sentinel` (copied atomically onto each accelerator node at `/var/lib/gpustack/operator/lib/engine-sentinel` by the `device-manager` `stage-libs` initContainer).

Mount that directory read-only in the engine container and prepend it to `PYTHONPATH`:

```sh
export PYTHONPATH="/var/lib/gpustack/operator/lib/engine-sentinel:${PYTHONPATH:-}"
export GPUSTACK_ENGINE_SENTINEL_ENABLE=1
# Optional: set to 0 to open /gpustack/v1/ready without requiring POST /gpustack/v1/ack.
export GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK=1

vllm serve /models/qwen --port 8000
```

### 2. Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `GPUSTACK_ENGINE_SENTINEL_ENABLE` | `0` | Activates `sitecustomize.py` post-import hooks (`vLLM`, `SGLang`, and `uvicorn.Config` fallback) when set to `1`. |
| `GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK` | `1` | Keeps `GET /gpustack/v1/ready` fail-closed (`503 awaiting_ack`) until `POST /gpustack/v1/ack` confirms the current `boot_id`. Set to `0` to report ready immediately once the engine is ready and unpaused. |
| `GPUSTACK_ENGINE_SENTINEL_ENGINE` | `""` | Optional engine label override (`vllm` or `sglang`) for `FallbackSentinelBackend` when neither engine module is imported. |

### 3. Control-Plane HTTP Endpoints (`/gpustack/v1/*`)

All `/gpustack/v1/*` routes run inside the engine container (north-south isolation is enforced by the Managed Router Envoy inference allowlist; external Gateway owns caller authentication):

- `GET /gpustack/v1/live` — Liveness probe (`200` healthy/starting/paused; `503` dead engine/scheduler).
- `GET /gpustack/v1/ready` — Readiness probe (`503` starting/paused/awaiting ACK/dead; `200` ready).
- `POST /gpustack/v1/ack` — Confirms `{"boot_id", "observed_generation", "applied_config_digest"}` (`409 boot_id_mismatch` on stale `boot_id`).
- `GET /gpustack/v1/pause` & `POST /gpustack/v1/pause` — Reads or sets `{"paused": true|false, "expected_boot_id": "..."}` for the ASGI `503` pause gate.
- `GET /gpustack/v1/runtime_config` & `POST /gpustack/v1/runtime_config` — Reads `boot_defaults`, `supported_fields`, `values`, and per-rank receipts, or applies `{"operation_id", "expected_boot_id", "generation", "applied_config_digest", "parameters": {...}, "reset_fields": [...]}` atomically (`422 unsupported_fields` on drift).

### 4. PreStop Graceful Drain CLI

```sh
python3 -m gpustack_engine_sentinel.drain \
  --engine vLLM \
  --port 8000 \
  --scheme http \
  --settle 5 \
  --deadline 25
```

---

## Build & Development

This package is managed with [`uv`](https://docs.astral.sh/uv/) and [`hatchling`](https://hatch.pypa.io/).

### Build the Unpacked Host-Staged Bundle

```sh
cd engine-sentinel/python
# Produce the flat unpacked directory (gpustack_engine_sentinel/, sitecustomize.py, .dist-info):
python3 -B stage_build.py --output-dir ./dist/engine-sentinel --version "${GPUSTACK_GIT_VERSION:-0.0.0}"
```

### Build the Universal Wheel and Sdist

```sh
cd engine-sentinel/python
uv build
```

Artifacts are written to `engine-sentinel/python/dist/`:
- `gpustack_engine_sentinel-<version>-py3-none-any.whl`
- `gpustack_engine_sentinel-<version>.tar.gz`

### Run Tests

All unit, `uvicorn.Config` fallback, fresh-interpreter `spawn`, and drain CLI tests run on CPU without GPU or NPU hardware:

```sh
cd engine-sentinel/python
uv run pytest -v
```

### Run Linter and Formatter Checks

From the repository root:

```sh
make lint python
```

Or directly inside `engine-sentinel/python`:

```sh
cd engine-sentinel/python
uv run ruff check .
uv run ruff format --check .
```

---

## License

Copyright 2026 GPUStack, Inc.

Licensed under the [Apache License, Version 2.0](./LICENSE).
