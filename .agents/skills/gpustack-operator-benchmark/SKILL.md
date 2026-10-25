---
name: gpustack-operator-benchmark
description: "Benchmark the operator on a GPU cluster — today the ModelDeployment serving topologies: warm NodeModelStore cache, drive in-cluster load across EPD, P/D, Elastic EP, and External DP, and collect stage and transfer evidence."
disable-model-invocation: true
allowed-tools: "Read, Agent, Bash(bash .claude/skills/_e2e-lib/scripts/preflight.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/assert-core.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/cluster-auth.sh*), Bash(bash .claude/skills/_e2e-lib/scripts/kube-context.sh*), Bash(bash .claude/skills/gpustack-operator-benchmark/cases/*), Bash(kubectl get*), Bash(kubectl describe*), Bash(kubectl logs*), Bash(kubectl cluster-info*), Bash(kubectl version*), Bash(kubectl config current-context), Bash(kubectl config get-contexts*), Bash(git diff*), Bash(git rev-parse*), Bash(command -v*), Bash(date*), Bash(mkdir -p .claude/reports/*), Bash(tee .claude/reports/*)"
---

# GPUStack Operator — Benchmark

Use [the shared module map](../../../docs/README.md#module-map) to locate the relevant guides,
owning specs, code entry points and related modules. [AGENTS.md](../../../AGENTS.md#finding-sources)
owns the retrieval and evidence rules; this skill owns its execution procedure.

Measure and compare the `ModelDeployment` serving scenarios on a real multi-GPU cluster (8 × GPU baseline) and collect both end-to-end latency/throughput distributions (`TTFT`, `ITL`, `TPOT`, `E2E`, `output_tok/s`, `req/s`) and per-stage execution / KV-transfer / EC-transfer evidence. The scenarios below are the suite's first five; a scenario outside serving joins this suite, not a new skill:

- **Scenario 1 (`cases/case-1-epd.sh`)** — [Encode, Prefill and Decode](../../../docs/walkthroughs/model-deployment/encode-prefill-decode.md) ([benchmark](../../../docs/modules/model-deployment/encode-prefill-decode-benchmark.md)): 3-arm comparison (`Aggregated` vs `P/D` vs `E/P/D`) on a 3-GPU budget with `Qwen/Qwen2.5-VL-7B-Instruct` (`NIXL` encoder cache + `Mooncake` KV cache).
- **Scenario 2 (`cases/case-2-prefill-decode.sh`)** — [Prefill and Decode](../../../docs/walkthroughs/model-deployment/prefill-decode.md) ([benchmark](../../../docs/modules/model-deployment/prefill-decode-benchmark.md)): 2-arm comparison (`Aggregated` 3 × `Server(TP=2)` vs `P/D` 1 × `Prefill(TP=2)` + 2 × `Decode(TP=2)`) on a 6-GPU budget with `Qwen/Qwen3-32B`.
- **Scenario 3 (`cases/case-3-elastic-ep.sh`)** — [Elastic EP](../../../docs/walkthroughs/model-deployment/elastic-ep.md) ([benchmark](../../../docs/modules/model-deployment/elastic-ep-benchmark.md)): 3-state online horizontal resize (`elasticEp.width: 2 -> 4`, 4 GPUs → 8 GPUs) under live load with `deepseek-ai/DeepSeek-V2-Lite-Chat`.
- **Scenario 4 (`cases/case-4-external-dp.sh`)** — [External DP](../../../docs/walkthroughs/model-deployment/external-dp.md) ([benchmark](../../../docs/modules/model-deployment/external-dp-benchmark.md)): 2-arm comparison (`Wide TP=8` vs `External DP=4 × TP=2 + EP`) on an 8-GPU budget with `deepseek-ai/DeepSeek-V2-Lite-Chat`.
- **Scenario 5 (`cases/case-5-autoscaling.sh`)** — [Autoscaling](../../../docs/modules/model-deployment/scaling.md): 5 arms over 4 algorithms (`Proportional`, `TokenThroughput`, `Predictive`) on 4 topologies, in two waves over 8 GPUs, each arm measured through a 4-phase load wave (baseline, ramp, sustain, cooldown) with a 2s sampler recording the replica count, the recommendation and the retirement that carried the count down. The admission case (`case-128.sh`) runs once before wave 1, on a cluster with nothing else on it, because it builds deployments of its own that ask for up to six GPUs; wave 1 then runs two 4-GPU slots side by side (vLLM P/D and SGLang P/D), and wave 2 runs a 6-GPU slot on the vLLM E/P/D triple beside a 2-GPU slot that runs the live-traffic E2E case (`case-129.sh`) against a single server.

## Orchestration & Hard Rules

Read [`orchestration.md`](../_e2e-lib/references/orchestration.md) and [`cluster-provisioning.md`](../_e2e-lib/references/cluster-provisioning.md) before starting.

1. **Drive load in-cluster via `default/bench-driver` (never over `kubectl port-forward`).**
   `scripts/bench_client.py` and `scripts/bench_vlm.py` detect `default/bench-driver` (`BENCH_INCLUSTER=1` default) and resolve the local forwarded port back to the cluster-internal Service DNS (`http://<svc>.<ns>.svc:8000`), executing the SSE streaming client inside `default/bench-driver`. Driving high-concurrency SSE streams across `kubectl port-forward` bottlenecks on the API server SPDY proxy and drops connections under load.
2. **Pre-warm `NodeModelStore` (`manifests/10-model-delivery.yaml`) before running any case.**
   Wait until `kubectl get nodemodelstore -o json` shows `state: Ready` for `qwen2-5-vl-7b-instruct`, `qwen3-32b`, and `deepseek-v2-lite-chat` so model loading comes from local NVMe/CSI cache rather than Hugging Face Hub network downloads.
3. **Share serving counter definitions via `../_e2e-lib/scripts/serving-lib.sh`.**
   `scripts/stage_counts.sh` sources `../_e2e-lib/scripts/serving-lib.sh` (extracted and shared with `gpustack-operator-e2e`) so router request counters (`llm_d_epp_request_total`, `vllm_router_pd_requests_total`, `vllm_router_requests_total`, `smg_router_requests_total`) and engine transfer series stay in one place.
4. **Operational safeguards validated on 8 × H100:**
   - **External DP (`manifests/external-dp-arm2-external-dp.yaml`)**: Always set `VLLM_PORT: "19500"` (outside the Linux ephemeral client port range `32768–60999`). Without `VLLM_PORT`, vLLM's `get_open_ports_list(5)` allocates ports from the ephemeral range; when `m0` connects as a TCP client to `DPCoordinator` (`29550`), the kernel can assign one of those 5 ephemeral ports as the client source port, causing `stateless_init_dp_group()` on `m0` to hit `EADDRINUSE` and roll to the next port while `m1..m3` wait forever on the original port.
   - **InfiniBand RDMA**: Set `spec.kvTransfer.protocol: RDMA` and request `resources.interface` on `Prefill` and `Decode` roles. For `Server` roles with `size > 1` or `elasticEp`, request `resources.interface` directly.
   - **Elastic EP (`cases/case-3-elastic-ep.sh`)**: During `elasticEp.width: 2 -> 4`, `.status.phase` remains `Ready` until the brief collective re-rendezvous window (where `elastic-watch-timeline.txt` may sample `Starting 0S` and the master `/health` returns temporary HTTP `503`s while `RoleKindsReady`/`EndpointEligibility` converge). Record both the harness `wait_ready` check and the full `RoleKindsReady` transition timestamp.

## Layout

- `../_e2e-lib/scripts/serving-lib.sh` — shared vendor/engine/router Prometheus counter and log-pattern library used by both `gpustack-operator-e2e` and `gpustack-operator-benchmark`.
- `scripts/bench_client.py` — text LLM SSE streaming load generator (`calibrate`, `run`, `mixed`, `load`) with automatic in-cluster execution inside `default/bench-driver`.
- `scripts/bench_vlm.py` — multimodal VLM PNG generator and SSE streaming harness (`run`, `burst`, `--shared-image`) with automatic in-cluster execution inside `default/bench-driver`.
- `scripts/stage_counts.sh` — collects per-role `POST /v1/chat/completions` access counts, `POST /v1/chat/completions/render` counts, and router access/metric series.
- `scripts/make_doc_tables.py` — formats raw JSON cell outputs into Markdown tables for the benchmark guides under `docs/modules/model-deployment/`.
- `manifests/` — declarative `ModelArtifact`, `ModelStore`, `InstanceType`, and `ModelDeployment` manifests for all 4 scenarios.
- `manifests/autoscaling-*.yaml` — the `ModelDeployment` manifests of Scenario 5, one per topology it measures: `autoscaling-vllm-pd.yaml`, `autoscaling-vllm-pd-b.yaml` (the same pair under a second name, so two slots never share a deployment), `autoscaling-sglang-pd.yaml`, `autoscaling-vllm-epd.yaml` and `autoscaling-e2e-server.yaml`.
- `cases/case-{1-epd,2-prefill-decode,3-elastic-ep,4-external-dp,5-autoscaling}.sh` — self-contained scenario runners.
- `cases/case-4b-external-dp-scaling.sh`, `cases/case-6-sedp.sh`, `cases/case-7-sglang-multi-server.sh` — the scaling and SGLang legs: the External DP group driven `replicas 1 -> 2 -> 1`, the SGLang External DP group, and the SGLang multi-Server cell. Each is self-contained and writes its own manifest into its own output directory.
- `cases/case-8-vs-content.sh`, `cases/case-9-vpd-scaler.sh`, `cases/case-10-spd.sh` — the assertion legs: the vLLM multi-Server content classes through the router, the vLLM prefill/decode pair driven by a `ModelDeploymentScaler`, and the SGLang prefill/decode pair behind the gateway with its own Scaler phase. Each is self-contained and writes its own manifest into its own output directory.
- `cases/case-5-autoscaling.sh <report-dir> --summary-only` — rebuilds `summary.tsv`, `summary.md` and `comparison.json` from the artifacts a run already left behind, so a report is regenerated without spending the cluster time again. The summary carries one row per arm and phase with the request count, the error count, the TTFT and ITL percentiles, the output token rate, the scale-out lead and the retirement states the arm went through, and the engine's own account of a failed phase when it has one.

## Cases

| Case | Scenario | Script | Topologies Compared | Model | GPUs |
|---|---|---|---|---|---|
| 1 | Encode, Prefill and Decode | `cases/case-1-epd.sh` | `3 × Server` vs `2P + 1D` vs `1E + 1P + 1D` | `Qwen/Qwen2.5-VL-7B-Instruct` | 3 |
| 2 | Prefill and Decode | `cases/case-2-prefill-decode.sh` | `3 × Server(TP=2)` vs `1 × Prefill(TP=2) + 2 × Decode(TP=2)` | `Qwen/Qwen3-32B` | 6 |
| 3 | Elastic EP | `cases/case-3-elastic-ep.sh` | `width=2` (4 GPUs) → live resize → `width=4` (8 GPUs) | `deepseek-ai/DeepSeek-V2-Lite-Chat` | 4 → 8 |
| 4 | External DP | `cases/case-4-external-dp.sh` | `1 × Server(TP=8)` vs `4 × Server(TP=2, DP=4, EP)` | `deepseek-ai/DeepSeek-V2-Lite-Chat` | 8 |
| 4b | External DP scaling | `cases/case-4b-external-dp-scaling.sh` | the same `size=4` External DP group, driven `replicas 1 -> 2 -> 1` | `deepseek-ai/DeepSeek-V2-Lite-Chat` | 8, then 16 |
| 6 | SGLang External DP | `cases/case-6-sedp.sh` | one group of `size=2` members under the Rust server, driven `replicas 1 -> 2 -> 1`, with a non-head member fault | `Qwen/Qwen3-8B` | 4 |
| 7 | SGLang multi-Server | `cases/case-7-sglang-multi-server.sh` | two independent SGLang Server replicas behind the gateway, driven `replicas 1 -> 2 -> 1` | `Qwen/Qwen3-8B` | 2 |
| 8 | vLLM multi-Server content | `cases/case-8-vs-content.sh` | two vLLM Server replicas behind `vllm-router`, with the arithmetic, stream, exact-Chinese and JSON classes asserted through the router | `Qwen/Qwen3-8B` | 2 |
| 9 | vLLM P/D under a Scaler | `cases/case-9-vpd-scaler.sh` | one vLLM Prefill and one vLLM Decode behind `llm-d-router`, driven by an Active `ModelDeploymentScaler` (`Proportional` on `RunningRequests`) up and back down through the retirement protocol | `Qwen/Qwen3-8B` | 2, then 4 |
| 10 | SGLang P/D behind the gateway | `cases/case-10-spd.sh` | one SGLang Prefill and one SGLang Decode behind `sglang-gateway`, with the same four content classes and its own Scaler phase | `Qwen/Qwen3-8B` | 2, then 4 |
| 5 | Autoscaling | `cases/case-5-autoscaling.sh` | `vLLM P/D` (`Proportional`, `TokenThroughput`, `Predictive`) vs `SGLang P/D` (`TokenThroughput`) vs `vLLM E/P/D` (`Predictive`) | `Qwen/Qwen3-8B`, `Qwen/Qwen2.5-VL-7B-Instruct` | 4 + 4, then 6 + 2 |
