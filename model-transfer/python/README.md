# GPUStack Model Transfer

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](./LICENSE)

`gpustack-model-transfer` is the portable Python library for GPUStack Operator's remote model weight delivery (`model-transfer`). It provides the reader and session contract, local reference reader, generation-bound source worker, transport backends (`nixl` for NVIDIA CUDA, `hixl` for Ascend CANN), inference engine adapters (`vllm`, `vllm-ascend`, `sglang`, and optional `modelexpress`), and the hardware acceptance runner.

---

## How It Works

Remote weight delivery separates **checkpoint metadata** from **tensor payload streaming** so target inference Pods can start without downloading or materializing a full local checkpoint on disk:

1. **Verified Metadata-Only Target Mount**
   The node-local `model-manager` CSI plugin mounts a digest-verified metadata view containing `manifest.json`, `config.json`, tokenizer assets, and `.safetensors` header-only stubs (without tensor payload bytes). Engine configuration and model initialization run against this verified metadata view before any weight mutation occurs.

2. **Authorized Source Generations & Read-Only Protection**
   On source nodes prepared by `ModelPrefetch` (`spec.source: {}`), the node-local `model-transfer` broker admits a pinned-memory reservation against the node's source capacity or the namespace's optional `ModelStoreBinding.spec.sourceMemory` grant and issues an immutable generation token. The source worker verifies the checkpoint tree, registers source memory, and maintains a generation-scoped health lease. Remote consumers authenticate via `TokenReview`-backed bearer tokens and cannot mutate source memory.

3. **Bounded-Window Streaming & Separate Memory Accounting**
   Target adapters open a `ReaderSession` parameterized by explicit `ReadBudgets`:
   - `window_bytes` bounds each reusable transfer window. The configured maximum
     defaults to 8 GiB, has a 1 GiB floor and a 1 TiB ceiling, and values below
     the floor resolve to 1 GiB. Tensors larger than one window use multiple
     reads. The lower-level `ReadBudgets` API accepts 1 byte through 1 TiB.
   - `max_inflight` bounds concurrent operations and accepts 1 through 256.
     The client defaults to the available CPU thread count, capped at 256,
     or 4 if the count cannot be detected.
   - `owned_output_bytes` bounds a single owned output tensor and is checked
     before reading or mutating model parameters.
   Reusable transfer windows and assembled model tensors are accounted separately so transfer buffers never inflate long-lived model allocations.

4. **Explicit Transport Negotiation (`nixl` / `hixl`)**
   Each backend reports `requested`, `applied`, and `effective` transport capabilities alongside hardware port counter attribution (InfiniBand HCA counters on CUDA/NIXL; RoCE counters on CANN/HIXL). If hardware transport counters cannot be established, the backend reports `functional_only` rather than silently claiming RDMA over a TCP fallback.

5. **Pre-KV-Profiling Completion & Release Fence**
   Transfer operations can complete, fail, or be cancelled; an unobservable outcome is `UNKNOWN`. Buffers and registrations for an `UNKNOWN` operation remain quarantined until a qualified completion fence applies. On normal completion, `session.close()` and adapter `finalize()` release all transfer windows and memory registrations **before** the inference engine profiles available accelerator memory for KV cache allocation.

---

## Package Layout

| Module | Description |
| --- | --- |
| `gpustack_model_transfer.core` | Wire protocol validation, `ArtifactIdentity`, `TensorDescriptor`, `ReadBudgets`, `LocalFileReader`, and `ReaderSession`. |
| `gpustack_model_transfer.backends.nixl` | API-checked NVIDIA NIXL transfer backend, IB port counter attribution, read-only source guard, and failure fence. |
| `gpustack_model_transfer.backends.hixl` | Ascend CANN HIXL transfer binding/native bridge, explicit transport negotiation, and quarantine fence. |
| `gpustack_model_transfer.source` | `ControlClient` and generation-bound `SourceWorker` speaking to the node-local `model-transfer` broker. |
| `gpustack_model_transfer.adapters.vllm` | Native vLLM weight-loader plugin and pre-KV-profiling release hook. |
| `gpustack_model_transfer.adapters.vllm_ascend` | Independent vLLM-Ascend weight-loader adapter preserving NPU format/contiguous finalization. |
| `gpustack_model_transfer.adapters.sglang` | SGLang single-rank and sharded load-group adapter with rank-ordered deterministic planning. |
| `gpustack_model_transfer.adapters.modelexpress` | Optional `GpustackLoadStrategy` adapter for ModelExpress-enabled images. |
| `gpustack_model_transfer.acceptance` | Fail-closed acceptance runner for local, hardware, and cluster checks. |

---

## Usage

### 1. Installation in Engine Containers

`gpustack-model-transfer` is built as a universal pure-Python wheel (`gpustack_model_transfer-<version>-py3-none-any.whl`) with zero hard runtime dependencies in `pyproject.toml`; vendor libraries (`torch`, `torch_npu`, `nixl`, `hixl`, `vllm`, `sglang`) are imported lazily only by their respective subpackages.

HIXL and NIXL runtime loading does not compare vendor version numbers. Required API methods are checked before native use; missing or incompatible APIs still refuse. Distribution versions are diagnostic metadata, and an unavailable version remains unknown. Image builds pin their dependency versions and source commits for reproducibility; those pins do not make other vendor builds hardware-qualified.


In a GPUStack-managed cluster, the `model-transfer` source DaemonSet's `stage-libs` initContainer stages the wheel on the host at `/var/lib/gpustack/operator/lib/model-transfer`, and the `ModelDeployment` controller mounts and installs it offline before starting the engine:

```sh
python3 -c 'import importlib.util, sys; sys.exit(0 if importlib.util.find_spec("gpustack_model_transfer") else 1)' || \
  python3 -m pip install --no-deps --no-index --find-links /var/lib/gpustack/operator/lib/model-transfer gpustack-model-transfer
```

### 2. Reading Weights via `ReaderSession`

```python
from pathlib import Path
from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import local_backend_capabilities
from gpustack_model_transfer.core.local_reader import LocalFileReader
from gpustack_model_transfer.core.session import ReaderSession

reader = LocalFileReader(Path("/model/manifest.json"))
try:
    session = ReaderSession(
        identity=reader.identity(),
        capabilities=local_backend_capabilities(),
        budgets=ReadBudgets(
            window_bytes=1024 * 1024 * 1024,
            max_inflight=2,
            owned_output_bytes=64 * 1024 * 1024,
        ),
        reader=reader,
    )
    try:
        for desc in session.descriptors:
            result = session.read_range(desc.name, offset=0, length=desc.byte_size)
            payload = result.payload
    finally:
        session.close()
finally:
    reader.close()
```

### 3. Engine Adapters

When remote weight delivery is enabled for a `ModelDeployment` role, the
controller configures the engine with `--load-format model-transfer` and injects
the artifact identity, model path, token path, backend, and endpoint configuration.
The client reads these variables: `GPUSTACK_MODEL_TRANSFER_ARTIFACT`,
`GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID`, `GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST`,
`GPUSTACK_MODEL_TRANSFER_MODEL_PATH`, `GPUSTACK_MODEL_TRANSFER_TOKEN`,
`GPUSTACK_MODEL_TRANSFER_BACKEND`, and `GPUSTACK_MODEL_TRANSFER_CONFIG`. It also
reads Pod identity variables injected by the controller. Optional settings include
`GPUSTACK_MODEL_TRANSFER_WORKERS`, `GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES`,
and `GPUSTACK_MODEL_TRANSFER_TIMEOUT`.

- **vLLM and vLLM-Ascend**: the general plugin entry point is
  `gpustack_model_transfer.adapters.vllm.plugin:register`.
- **SGLang**: the native loader is
  `gpustack_model_transfer.adapters.sglang.native:NativeSGLangModelTransferLoader`.
- **ModelExpress**: the optional strategy is
  `gpustack_model_transfer.adapters.modelexpress.entry:GpustackLoadStrategy`.

#### ModelExpress peer delivery and source fallback

For vLLM and SGLang, select `--load-format modelexpress` or `--load-format mx`.
The integration defaults `MX_LOAD_PLUGIN=gpustack` and `MX_LOAD_PLUGIN_BEFORE=server-cache`;
when `MX_SERVER_ADDRESS` is configured, it also defaults `MX_ARTIFACT_TRANSFER=1`.

The load chain tries a ready peer over RDMA first, then the GPUStack source,
then local cache or engine-volume fallbacks. Under `Prefer`, local fallbacks
remain available. Under `Require`, local fallbacks are removed and loading
fails if peer and source delivery fail. An unrecognized explicit
`GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY` value also fails closed as `Require`.

### 4. Running the Acceptance Suite

The runner reads a validated private configuration file passed with `--config`.
The recognized suite names are `contract`, `backend`, `model-load`,
`managed-startup`, `lifecycle`, `safety`, and `ep-coexistence`. Supported
backend and engine combinations vary by suite; unimplemented combinations are
refused. For example, with a suitable configuration file:

```sh
cd model-transfer/python
uv run python -m gpustack_model_transfer.acceptance \
  --config /path/to/acceptance.json \
  --backend nixl \
  --engine vllm \
  --suite managed-startup
```

---

## Build & Development

This package is managed with [`uv`](https://docs.astral.sh/uv/) and [`hatchling`](https://hatch.pypa.io/).

### Build the Universal Wheel and Sdist

```sh
cd model-transfer/python
uv build
```

Artifacts are written to `model-transfer/python/dist/`:
- `gpustack_model_transfer-<version>-py3-none-any.whl`
- `gpustack_model_transfer-<version>.tar.gz`

For container builds without a `.git` directory, set `SETUPTOOLS_SCM_PRETEND_VERSION=<x.y.z>` (or `SETUPTOOLS_SCM_PRETEND_VERSION_FOR_GPUSTACK_MODEL_TRANSFER=<x.y.z>`):

```sh
cd model-transfer/python
SETUPTOOLS_SCM_PRETEND_VERSION=0.1.0 python3 -m pip wheel --no-deps --no-build-isolation --wheel-dir ./dist .
```

### Run Tests

All unit and contract tests run on CPU without requiring CUDA, CANN, or RDMA hardware:

```sh
cd model-transfer/python
uv run pytest -W error::ResourceWarning -W error::pytest.PytestUnraisableExceptionWarning -v
```

### Run Linter and Formatter Checks

From the repository root:

```sh
make lint python
```

Or directly inside `model-transfer/python`:

```sh
cd model-transfer/python
uv run ruff check .
uv run ruff format --check .
```

---

## License

Copyright 2026 GPUStack, Inc.

Licensed under the [Apache License, Version 2.0](./LICENSE).
