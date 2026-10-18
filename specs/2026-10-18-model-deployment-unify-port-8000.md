# Spec: Unify ModelDeployment Serving and Router Endpoints on Port 8000

Status: Building
Blocked on: Tasks T1 and T2 in progress
Type: Feature
Created: 2026-10-18
Updated: 2026-10-18
Issue: [764](https://github.com/gpustack/gpustack-operator/issues/764)

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [User Stories](#user-stories)
  - [Story 1](#story-1)
  - [Story 2](#story-2)
- [Proposal](#proposal)
  - [Features](#features)
  - [Notes, Constraints and Caveats](#notes-constraints-and-caveats)
  - [Boundaries](#boundaries)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Project Structure](#project-structure)
  - [Commands](#commands)
  - [Code Style](#code-style)
  - [Implementation Plan](#implementation-plan)
  - [Test Plan](#test-plan)
- [Alternatives](#alternatives)
- [Open Questions](#open-questions)

## Summary

`ModelDeployment` currently uses two different default HTTP ports for inference traffic: role Pods and role Services default to port `8000` (`modelDeploymentDefaultPort = 8000`), while the managed router (`llm-d-router`, `vllm-router`, and `sglang-gateway`) and its `Service/<md>-router` listen on port `8081` (`modelDeploymentRouterHTTPPort = 8081`). This enhancement moves the managed router HTTP port from `8081` to `8000` so that `ModelDeployment.status.endpoint` defaults to port `8000` whether `spec.router` is configured or not.

## Motivation

When a user or upstream gateway connects to a `ModelDeployment`, the default HTTP port changes depending on whether `spec.router` is present:

- Without `spec.router`, [`modelDeploymentDefaultPort`](../pkg/worker/controllers/worker/model_deployment_render.go#L40) is `8000`, so `Service/<md>` and `status.endpoint` publish `http://<md>.<ns>.svc:8000`.
- With `spec.router`, [`modelDeploymentRouterHTTPPort`](../pkg/worker/controllers/worker/model_deployment_router.go#L39) is `8081`, so `Service/<md>-router` and `status.endpoint` publish `http://<md>-router.<ns>.svc:8081`.

Inference engines (`vLLM`, `SGLang`) default to port `8000`, and `KVCacheBackend` Mooncake Store already reserves `8080+` on host networks. Aligning the managed router HTTP listener and `Service/<md>-router` with `8000` removes the `8000`-vs-`8081` split while preserving the existing `8000` engine default.

### Goals

- Change the managed router HTTP port (`modelDeploymentRouterHTTPPort`) and Envoy static listener (`modelDeploymentRouterEnvoyConfig`) from `8081` to `8000` across `llm-d-router`, `vllm-router`, and `sglang-gateway`.
- Keep the default role serving port (`modelDeploymentDefaultPort`) at `8000` so unrouted (`http://<md>.<ns>.svc:8000`) and routed (`http://<md>-router.<ns>.svc:8000`) deployments both publish port `8000` in `status.endpoint`.
- Update unit tests, end-to-end test cases, walkthroughs, and module index entries that assert the default router port.

### Non-Goals

- Changing internal non-HTTP or sidecar ports: `modelDeploymentInternalPort` (`8200` for the engine behind the `llm-d-router` decode proxy), `modelDeploymentRouterEPPPort` (`9002`), `epp` gRPC health (`9003`), `modelDeploymentRouterMetricsPort` (`9090`), ZMQ KV events (`5557`/`5558`), or Mooncake bootstrap (`8998`).
- Changing `KVCacheBackend` Mooncake store ports (`8080 + group index`).
- Adding new CRD fields to `ModelDeployment` or `ModelDeploymentRouter`.

## User Stories

### Story 1

As a platform engineer operating `ModelDeployment` workloads with and without `spec.router`, I want both unrouted `Service/<md>` and routed `Service/<md>-router` (`status.endpoint`) to listen on port `8000` by default, so that client configurations, NetworkPolicies, and port-forwarding scripts use one consistent port `8000`.

### Story 2

As a user adding or removing `spec.router` on a `ModelDeployment`, I want `status.endpoint` to keep the same default port `:8000` across both configurations (`http://<md>.<ns>.svc:8000` and `http://<md>-router.<ns>.svc:8000`).

## Proposal

### Features

#### F1 — Managed router HTTP listener, Service, and observer port on `8000`

Update [`pkg/worker/controllers/worker/model_deployment_router.go`](../pkg/worker/controllers/worker/model_deployment_router.go):

- Set `modelDeploymentRouterHTTPPort int32 = 8000` (was `8081`).
- In `modelDeploymentRouterEnvoyConfig`, set the `listener_0` socket address `port_value` to `8000` (was `8081`).
- Keep `modelDeploymentDefaultPort` at `8000` in [`pkg/worker/controllers/worker/model_deployment_render.go`](../pkg/worker/controllers/worker/model_deployment_render.go#L40).

Because all router rendering and status helpers derive from `modelDeploymentRouterHTTPPort`, this single constant + Envoy config change automatically updates:

1. **`Service/<md>-router`**: `spec.ports[0].port = 8000` and `targetPort = "http"`.
2. **`llm-d-router` Deployment**: Envoy container port `http = 8000`, TCP readiness probe on `8000`, and Envoy listener on `0.0.0.0:8000`.
3. **`vllm-router` and `sglang-gateway` Deployments**: `router.RenderArgv` receives `ListenPort: 8000`, rendering `--port=8000`, container port `http = 8000`, and HTTP `/readiness` probe on `8000`.
4. **Router endpoint observation**: [`modelDeploymentRouterObserverPort`](../pkg/worker/controllers/worker/model_deployment_status.go#L350-L355) returns `modelDeploymentRouterHTTPPort` (`8000`) for `vllm-router` and `sglang-gateway`, and `modelDeploymentRouterMetricsPort` (`9090`) for `llm-d-router`.
5. **Published status**: `status.router.endpoint` and `status.endpoint` publish `http://<md>-router.<ns>.svc:8000` when the router is ready.

#### F2 — Test suite, e2e cases, and documentation alignment

Update all tests, e2e scripts, and documentation pages that reference the router port `8081`:

- Unit tests in [`pkg/worker/kvcache/router`](../pkg/worker/kvcache/router/), [`pkg/worker/controllers/worker`](../pkg/worker/controllers/worker/), and [`pkg/worker/extensionapis/worker`](../pkg/worker/extensionapis/worker/).
- End-to-end test cases in [`.agents/skills/gpustack-operator-e2e/cases`](../.agents/skills/gpustack-operator-e2e/cases/) (`case-71.sh`, `case-91.sh`, `case-94.sh`, `case-120.sh`, `case-121.sh`).
- Documentation in [`docs/walkthroughs/model-deployment/epd.md`](../docs/walkthroughs/model-deployment/epd.md), [`docs/walkthroughs/model-deployment/external-dp.md`](../docs/walkthroughs/model-deployment/external-dp.md), and [`docs/README.md`](../docs/README.md).

### Notes, Constraints and Caveats

- **No in-Pod port collision on any router.** `ModelDeployment` router Pods run in their own Pod network namespace (`hostNetwork: false`), separate from the role Pods:
  - In `llm-d-router`, `envoy` binds `0.0.0.0:8000`, while `epp` binds `9002` (ext_proc gRPC), `9003` (gRPC health), and `9090` (metrics + `/observer/endpoints`).
  - In `vllm-router` and `sglang-gateway`, the single `router` container binds `0.0.0.0:8000` (HTTP API + `/observer/endpoints`) and `0.0.0.0:9090` (Prometheus metrics), and dials backend role Pods on `<pod-ip>:8000`.

### Boundaries

- **Always:** Keep `modelDeploymentInternalPort` (`8200`), `modelDeploymentRouterEPPPort` (`9002`), `modelDeploymentRouterMetricsPort` (`9090`), and `KVCacheBackend` ports (`8080+`) unchanged. Run `make lint` and `make lint docs` before completion.
- **Ask first:** Changing any CRD schema under `api/` or altering `Service/<md>` selector semantics.
- **Never:** Modify `staging/`, `binding/`, or unrelated `KVCacheBackend` / `DeviceManager` controllers.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| `modelDeploymentRouterEnvoyConfig` is a multi-line YAML string literal; changing `modelDeploymentRouterHTTPPort` without updating `port_value` in `modelDeploymentRouterEnvoyConfig` would break `llm-d-router` readiness | Update both `modelDeploymentRouterHTTPPort` and `modelDeploymentRouterEnvoyConfig` (`port_value: 8000`) in [`pkg/worker/controllers/worker/model_deployment_router.go`](../pkg/worker/controllers/worker/model_deployment_router.go) and assert both in `model_deployment_router_test.go` |

## Design Details

### Project Structure

```text
pkg/worker/
├── kvcache/router/
│   ├── router_test.go                            # ListenPort test inputs updated from 8081 to 8000
│   └── argv_test.go                              # ListenPort / --port=8000 assertions
├── controllers/worker/
│   ├── model_deployment_router.go                # modelDeploymentRouterHTTPPort = 8000, Envoy port_value: 8000
│   ├── model_deployment_connector_epd.go         # Comment update (port 8081 -> 8000)
│   ├── model_deployment_router_test.go           # Router Service / Endpoint assertions on 8000
│   └── model_deployment_status_test.go           # Status endpoint test fixture on 8000
└── extensionapis/worker/
    └── model_deployment_test.go                  # Table printer endpoint fixture on 8000
.agents/skills/gpustack-operator-e2e/cases/
├── case-71.sh                                    # Router endpoint assertion on 8000
├── case-91.sh                                    # Router chat/completions URL on 8000
├── case-94.sh                                    # Router chat/completions URL on 8000
├── case-120.sh                                   # EPD router chat/completions URL on 8000
└── case-121.sh                                   # EPD router chat/completions URL on 8000
docs/
├── README.md                                     # Spec index entry under Model Deployment
└── walkthroughs/model-deployment/
    ├── epd.md                                    # Router endpoint port 8000
    └── external-dp.md                            # Router port-forward 18000/18080:8000
```

### Commands

Execution environment: **local** (`/Users/thxcode/Documents/gpustack.ai/gpustack/.claude/worktrees/gpustack/snakehead`).

- **Unit tests (targeted packages):**
  ```bash
  GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/router/... ./pkg/worker/controllers/worker/... ./pkg/worker/extensionapis/worker/...
  ```
- **Code & shell lint:**
  ```bash
  make lint
  make lint agents-shell
  ```
- **Documentation lint:**
  ```bash
  make lint docs
  ```

### Code Style

Keep constants typed and commented in plain prose without task identifiers:

```go
const (
	modelDeploymentRouterHTTPPort int32 = 8000
)
```

### Implementation Plan

- [x] **T1 · Move managed router HTTP port to `8000` and update unit tests**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_router.go`, `pkg/worker/controllers/worker/model_deployment_connector_epd.go`, `pkg/worker/controllers/worker/model_deployment_router_test.go`, `pkg/worker/controllers/worker/model_deployment_status_test.go`, `pkg/worker/kvcache/router/router_test.go`, `pkg/worker/kvcache/router/argv_test.go`, `pkg/worker/extensionapis/worker/model_deployment_test.go`
      Acceptance: `modelDeploymentRouterHTTPPort` is `8000`, `modelDeploymentRouterEnvoyConfig` binds `port_value: 8000`, and all unit tests in `pkg/worker/kvcache/router`, `pkg/worker/controllers/worker`, and `pkg/worker/extensionapis/worker` assert `8000` for router HTTP listener and `status.endpoint` (`http://<md>-router.<ns>.svc:8000`).
      Verify: `GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/worker/kvcache/router/... ./pkg/worker/controllers/worker/... ./pkg/worker/extensionapis/worker/...`

- [ ] **T2 · Update end-to-end test cases and walkthrough documentation to port `8000`**
      Blocked by: T1
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-71.sh`, `.agents/skills/gpustack-operator-e2e/cases/case-91.sh`, `.agents/skills/gpustack-operator-e2e/cases/case-94.sh`, `.agents/skills/gpustack-operator-e2e/cases/case-120.sh`, `.agents/skills/gpustack-operator-e2e/cases/case-121.sh`, `docs/walkthroughs/model-deployment/epd.md`, `docs/walkthroughs/model-deployment/external-dp.md`, `docs/README.md`
      Acceptance: Every end-to-end script and walkthrough targeting `Service/<md>-router` or router `Pod` HTTP port uses `8000` instead of `8081`, `docs/README.md` indexes this spec under `Model Deployment`, and both `make lint agents-shell` and `make lint docs` pass.
      Verify: `make lint agents-shell && make lint docs`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- None. Role Pod golden fixtures under `pkg/worker/controllers/worker/testdata/legacy-render/` already use port `8000`.

#### Unit tests

Coverage baselines measured on 2026-10-18 with Go (`GOTOOLCHAIN=local`):

- `pkg/worker/kvcache/router`: 2026-10-18 - 95.5%
- `pkg/worker/extensionapis/worker`: 2026-10-18 - 55.0%
- `pkg/worker/controllers/worker`: 2026-10-13 - 84.3%

Key unit test scenarios:

- Router Service port `8000`, container port `8000`, readiness probe `8000`, Envoy listener `port_value: 8000`, observer port `8000`, and `status.endpoint` (`http://<md>-router.<ns>.svc:8000`) across `llm-d-router`, `vllm-router`, and `sglang-gateway`.
- Router argv rendering (`--port=8000 --service-discovery-port=8000`) in `TestRenderArgv`.

#### Integration tests

Reconcile-level tests run against the controller-runtime fake client in `pkg/worker/controllers/worker`:

- Unrouted `ModelDeployment` reconcile produces `Service/<md>` on port `8000` and sets `status.endpoint` to `http://<md>.<ns>.svc:8000`.
- Routed `ModelDeployment` reconcile produces `Service/<md>-router` on port `8000` and sets `status.endpoint` to `http://<md>-router.<ns>.svc:8000` once the router Deployment is ready.

#### e2e tests

- `case-71.sh`: routed `ModelDeployment` publishes `http://<md>-router.<ns>.svc:8000` in `status.endpoint`.
- `case-91.sh`, `case-94.sh`, `case-120.sh`, `case-121.sh`: routed serving requests succeed against `http://<md>-router.<ns>.svc:8000/v1/chat/completions`.

## Alternatives

- **Move role Pods and router Pods to `8080`.**
  Rejected because inference engines (`vLLM`, `SGLang`) default to `8000`, `modelDeploymentDefaultPort` is already `8000`, and `KVCacheBackend` uses `8080` on host networks. Moving only the router from `8081` to `8000` unifies all `ModelDeployment` endpoints on `8000` with minimal churn.

## Open Questions

None.
