# Spec: ModelDeployment Encoder-Prefill-Decode Disaggregation

Status: Building
Blocked on: nothing is blocked by an unmade decision. T1, T0 and T2 start the work, T11 waits for the published router image, and T18 waits for Ascend and ROCm hardware that the project must supply.
Type: Feature

## Summary

Add an `Encode` role kind to `ModelDeployment`.
A deployment declares `Encode`, `Prefill` and `Decode` roles, and each role runs in its own Pods.
The operator renders the engine arguments, the encoder-cache transfer leg, the router configuration, the Services and the status for the three roles.
A proof-of-concept phase runs each NVIDIA candidate on real accelerators before the operator admits it.
vLLM-Ascend and vLLM on ROCm are implemented from the upstream code and admitted at once.
Their verification is the last step, and their known limits are recorded in the documentation.
The change also adds a way to mount a shared PersistentVolumeClaim into role Pods, benchmark evidence that compares aggregated, prefill/decode and encoder-prefill-decode serving, and an end-to-end test plan sized for the hardware that is available.

## Motivation

### Goals

1. A `ModelDeployment` can declare an `Encode` role beside `Prefill` and `Decode`.
   Each of the three roles runs in separate Pods.
2. The operator admits an NVIDIA router and engine combination only after the proof of concept has shown that it works.
   It refuses every combination outside the admitted matrix and names both sides in the message.
3. vLLM, SGLang, vLLM-Ascend and vLLM on ROCm are covered.
   The last two are implemented from the upstream code and admitted at once.
   Their verification is the last step, and the documentation records what is unverified and what limits apply.
4. The repository holds benchmark evidence for three arms: aggregated, prefill/decode, and encoder-prefill-decode.
5. End-to-end cases exist and fit the Nebius and AWS machines that can run them.
6. A walkthrough shows EPD on a live cluster, with captured resource output and completed image requests.
7. The `llm-router` image carries the router versions that EPD needs, and the settings default points at it.

Success criteria:

- For each admitted combination, a chat request that carries an image returns a correct answer.
  The test shows that the encoder Pod processed the image and that the encoder output crossed a Pod boundary.
- A deployment without an `Encode` role renders the same Pods as before, except for the image defaults that this change moves.
  The release notes name each moved image default.
  Rendering that is not tied to an image default stays identical, and a fixture test proves it.
- `make lint` passes, and the new unit and end-to-end cases pass.

### Non-Goals

- Encoder and prefill in one Pod, and encoder with a combined `Server` role (E+PD).
  Both are later work.
- Elastic EP, External DP and multi-Pod replicas on the `Encode` role.
- A shared multimodal embedding cache across encoders, and attaching a `KVCachePoolBinding` to the `Encode` role.
- MindIE-Motor integration.
  Its coordinator is Ascend-only and the operator does not need it.
- Changes to upstream routers or engines.
  A gap in an upstream project is recorded, not patched here.
- Validated audio and video input.
  Only image input is in the acceptance criteria.
- A storage class.
  The cluster provides the ReadWriteMany storage that a user attaches.
- A structure baseline for rendered Pods, so that a change to the Pod shape does not roll running replicas.
  It is a separate spec that follows this one, and it is tracked in [issue 760](https://github.com/gpustack/gpustack-operator/issues/760).
- A sidecar allow list for encoder targets.
  The sidecar runs with its target check off today, for prefill/decode as well, and this change keeps that state.
  The limit is documented, and a follow-up issue tracks the allow list.
- A new status condition for encoder-leg failures.
  The visible signal in this change is the sidecar log and trace.
- `spec.kvCache` on an EPD deployment, and moving an existing prefill/decode deployment to EPD in place.
  The role set of a deployment is frozen after creation, so a user creates a new deployment to move.

## Proposal

A user writes one `ModelDeployment` with three roles.
The operator wires them together, so a request that carries an image reaches an encoder, then a prefiller, then a decoder.
A request without media skips the encoder.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen-epd
  namespace: team-a
spec:
  model:
    name: Qwen/Qwen3-VL-8B-Instruct
  engine:
    name: vLLM
    version: "0.30.0"
  router:
    name: llm-d-router
  roles:
    - name: encode
      kind: Encode
      replicas: 1
      instanceType: gpustack-nvidia-l40s-linux-amd64
      resources:
        accelerator: 1
    - name: prefill
      kind: Prefill
      replicas: 1
      instanceType: gpustack-nvidia-l40s-linux-amd64
      resources:
        accelerator: 1
    - name: decode
      kind: Decode
      replicas: 1
      instanceType: gpustack-nvidia-l40s-linux-amd64
      resources:
        accelerator: 1
```

The manifest above is illustrative.
The proof of concept decides which combinations the operator admits.

### User Stories

#### Story 1
As a platform administrator, I want to declare `Encode`, `Prefill` and `Decode` roles in one `ModelDeployment`, with one Pod set per role, so that visual encoding scales on its own and does not compete with the language model for the same accelerator.

#### Story 2
As a platform administrator, I want the operator to refuse unsupported engine, router, platform and version combinations and to say why, so that a silent wrong combination never reaches the cluster.

#### Story 3
As an operator on call, I want status and metrics to show the `Encode` role and its transfer state, so that I can tell whether the encoder is the bottleneck.

#### Story 4
As a platform developer, I want repeatable end-to-end cases and a three-arm benchmark, so that the answer to "is EPD worth it" rests on measured data and not on papers alone.

### Core Features & Acceptance Criteria

**F1. API.**
The kind enum gains `Encode`, on both the spec role and the status role.
`roles[].additionalVolumes` gains a `persistentVolumeClaim` source.
Acceptance:

- A manifest with `kind: Encode` is accepted by the API server, and the status echoes `Encode`.
- The kind stays frozen after creation, as the other kinds are.
- `status.roleSummary` shows `E` for an `Encode` role, in the order `E`, `P`, `D`.
- An `additionalVolumes` entry can name a `persistentVolumeClaim` by claim name, with the existing mount path, sub-path and read-only fields.
  The rendered Pod mounts that claim.
- Both API changes land first.
  `make generate` runs in the main checkout before any other code depends on them.

**F2. Admission.**
Acceptance:

- `Encode` requires both `Prefill` and `Decode`, and at most one role of each kind.
  `Encode` with `Server` is refused.
- `Encode` requires `spec.router`.
  Without a router, nothing pairs the roles, and the operator does not render an unpaired shape.
- An EPD deployment refuses `spec.kvCache`, because that field is deployment-wide and the `Encode` role cannot take part in it.
- An EPD deployment refuses a role `command` on all three roles.
  A command replaces the managed arguments and the routing sidecar, so the roles would no longer pair.
  Deployments without `Encode` keep their current command behavior.
- The three roles must run on one platform, and the router, engine and platform combination must be in the admitted matrix.
  Admission reads the platform of each role from its instance type.
  Any other combination, including a mixed-platform one or one with an unknown platform, is refused.
  The message names the router, the engine, the platform and the missing support.
- An EPD deployment must set `engine.version`.
  The same checks run again when a role instance type, image, engine version or router changes, so an update cannot move an admitted deployment outside the matrix.
  The floors apply to EPD deployments only, and the legacy floors stay as they are.
- The engine version must meet the floor for the encoder transfer leg.
  vLLM needs `0.30.0` for the network transfer connectors.
  SGLang needs `0.5.10` or later, and the operator's own SGLang floor is higher.
  vLLM on ROCm needs `0.30.0` for the same connectors.
  vLLM-Ascend uses the floor that its upstream example records, and the documentation states it.
- A `persistentVolumeClaim` source names a claim in the deployment namespace.
  An `additionalVolumes` entry sets exactly one source, and the claim name is not empty.
- When the encoder leg is shared storage, `Encode` and `Prefill` must mount the same claim with the same sub-path at the same mount path.
  The `Encode` mount and claim must not be read-only.
  The operator renders the connector's shared directory as that mount path.
- `Encode` replicas have size 1.
  The role refuses `spec.kvCache` attachment, Elastic EP and External DP.
- NVIDIA combinations are admitted only after their proof of concept passes.
  vLLM-Ascend and vLLM on ROCm are admitted once implemented.
  The documentation marks them unverified until their last-stage verification passes, and it lists their limits next to the admission rule.

**F3. Engine rendering.**
For each admitted engine, the operator renders arguments, environment and ports for the three roles.
It owns the transfer keys, so a user value for an owned key is refused, as it is today for the KV transfer keys.
Acceptance:

- vLLM on NVIDIA and on ROCm:
  - The `Encode` Pod runs as an encoder-cache producer with no KV cache.
  - The `Prefill` Pod is an encoder-cache consumer and a KV producer.
  - The `Decode` Pod is a KV consumer.
  - The encoder-cache connector is `ECCPUConnector` with NIXL transfer.
    The operator sets the V2 model runner variable and the side-channel address and port.
    No shared volume is needed.
- vLLM-Ascend:
  - The encoder-cache connector is `ECExampleConnector`, backed by a shared ReadWriteMany claim that the user attaches through `additionalVolumes`.
  - The KV leg stays `MooncakeConnectorV1`.
    The layerwise connector of the upstream Ascend example needs a metaserver endpoint that the decode sidecar cannot provide.
- SGLang:
  - The `Encode` Pod runs encoder-only.
  - The `Prefill` Pod runs language-only and reaches the `Encode` replicas through the per-role Service.
  - The encoder pushes its output back to the `Prefill` Pod, so the operator opens the needed ports between the two roles.
- A deployment without `Encode` renders as before, apart from the moved image defaults that the release notes name.

**F4. Router rendering.**
Acceptance:

- `llm-d-router`:
  - The endpoint-picker document gains an encode filter, an encode scheduling profile and the multimodal decider, and `Encode` Pods carry the role label that the filter selects.
  - The decode sidecar receives `--ec-connector=ec-nixl` or `--ec-connector=ec-example`, matching the engine leg.
  - The request lands on the decode Pod's sidecar first, and the sidecar calls the encoders.
  - A request that carries an image goes through the encoder, then the prefiller, then the decoder.
    The router flow is designed from the upstream code, and the proof of concept confirms it on a live cluster.
    The cases to confirm are an image request whose prefix is cached, a short prompt, and a zero disaggregation threshold.
    Only the prefiller consumes the encoder output, so a flow that skips the prefiller for a media request does not pass.
  - A request without media skips the encoder.
  - The `Encode` profile uses no KV or prefix scorer.
    `Encode` Pods are in stage discovery, and they are not in the answering set or in the set of KV event publishers.
  - The router, the picker document and the sidecar move to one version together.
- `vllm-router`: admitted only after upstream tags a release that contains the EPD feature and the proof of concept passes.
  It then receives its encoder list from the operator.
- `sglang-gateway`: renders prefill/decode as it does today.
  `Encode` Pods are excluded from every selector it receives.
- No router fronts an `Encode` Pod as an answering endpoint.
  The deployment-wide Service and `status.endpoint` never point at it, whatever the position of the `Encode` role in the list.
- Each role uses ports by purpose: one HTTP port for the router, a metrics port, and the transfer ports of its connector.
  Admission reserves the transfer ports, and no two purposes share one port.

**F5. Services, status and metrics.**
Acceptance:

- The `Encode` role gets a `<deployment>-<role>` Service.
- `RoleKindsReady` and the role status include `Encode`.
- The metrics snapshot does not mark an `Encode` Pod as partial for lacking series it does not publish.
  A failed scrape of an `Encode` Pod still marks the snapshot partial.
- Drain and retirement never classify an `Encode` replica as safe to remove on a gauge it does not publish.
  The controller first withdraws the replica from discovery, then waits a bounded window, then removes it.
  The documentation states that a transfer still in flight at that point fails its request visibly.
- The `Encode` Pod has a startup and a readiness probe on its engine health route, and a liveness probe.
  A dead encoder leaves the endpoint set and the picker stops choosing it.

**F6. Scheduling.**
Each `Encode` replica is its own Kueue pod group, admitted jointly with the `Prefill` and `Decode` groups.
Acceptance: at initial admission, when one group cannot reserve quota, none of the three starts.
Replacement and scale-up keep today's behavior: admitted groups keep serving while a new group waits for quota.

**F7. Proof of concept and verification.**
A documented procedure runs each candidate on real accelerators.
Acceptance for a combination to pass:

- An image request returns a correct answer.
- Encoder-side evidence shows that the image was encoded.
- Transfer evidence shows encoder output crossing a Pod boundary.
  A router that falls back to a mode with no transfer does not pass.
- A text-only request skips the encoder.
- The image request cases of F4 pass: a cached prefix, a short prompt and a zero threshold.
- A negative control shows the signal: an encoder response with missing transfer data appears in the sidecar log, and the test reads it there.
- The combination is recorded as passed or failed in the engine-version documentation, with the conditions of the run.

The NVIDIA combinations run first and gate their own admission.
vLLM-Ascend and vLLM on ROCm run last, on hardware that the project must supply.
Their result updates the documentation but does not gate admission.

**F8. Benchmark.**
Acceptance: a report with three arms (aggregated, prefill/decode, encoder-prefill-decode) per admitted engine.
It records time to first token, time per output token, end-to-end latency and throughput under low, medium and high concurrency.
It states the hardware, the model and the workload, and it states when EPD does not help.

**F9. End-to-end cases.**
Acceptance:

- Non-accelerator cases cover admission refusals, three-group joint admission, role Services and the claim mount.
- Accelerator cases cover one functional run per admitted combination.
- Each case states its hardware need and its skip condition.
- Cases use non-preemptible machines.

**F10. Documentation.**
Acceptance:

- A new module page, a walkthrough, and updates to the engine-version, routing, status and module-map pages.
- The page states that `ECExampleConnector` needs a ReadWriteMany claim that the user provides, that the connector has no locking or expiry, and that some network filesystems delay the visibility of new files.
- The verification status and the limits of each admitted combination sit beside its admission rule, including the unverified status of vLLM-Ascend and vLLM on ROCm.
- `make lint docs` passes.

**F11. Walkthrough.**
After the online-cluster test, a walkthrough for EPD joins the model-deployment walkthroughs.
Acceptance:

- It follows the style of the prefill/decode walkthrough: steps, captured resource output, completed image requests, and a troubleshooting section.
- Every output block comes from a real run on the cluster, and it states the engine version, the model and the hardware.
- It is linked from the module page, the walkthrough index and the module map.

**F12. Router image.**
The router versions that EPD needs reach users through the `llm-router` image.
Acceptance:

- A pull request bumps the version arguments and patches under `pack/llm-router`, so that the packaging pipeline publishes a new image.
  It covers `llm-d-router` at `v0.11.0`.
  It also covers `vllm-router` with the EPD feature from upstream PR 266 (commit `516f1cb`), because no tag contains that commit yet.
  Comments in the Dockerfile and in each carried patch state this, and say that the image moves to the first tag that contains the commit.
- The image build fails loudly if a patch no longer applies.
- A separate change updates the default image in the settings after the image is published.
  The EPD rendering that needs the new router lands after that change.

### Notes / Constraints / Caveats

- **Candidate combinations.**
  The router and engine candidates are `vllm-router` with vLLM, `sglang-gateway` with SGLang, `llm-d-router` with vLLM, and `llm-d-router` with SGLang.
  `llm-d-router` also fronts vLLM-Ascend and vLLM on ROCm.
  The proof of concept decides which combinations the operator admits.
- **Traffic flow differs by engine.**
  Only SGLang matches the model "the request goes to the prefiller, which calls the encoder":

  | Stack | First receiver | Caller of the encoder | How the prefiller gets the embedding |
  | --- | --- | --- | --- |
  | vLLM example proxy | proxy | proxy | reads shared files, receives a push, or pulls, by connector |
  | `llm-d-router` with vLLM | sidecar in the decode Pod | sidecar | `ec-example`: reads shared files; `ec-nixl`: pulls over NIXL |
  | `vllm-router` EPD | router | router | through the encoder-cache connector |
  | SGLang | prefiller, through a plain router | prefiller | the encoder pushes it |
  | vLLM-Ascend example proxy | proxy, then the decoder | proxy | reads shared files; the prefiller derives the content hash from the raw request |

- **Encoder-cache connectors by platform:**

  | Connector | NVIDIA | ROCm | Ascend | Shared volume | First vLLM release |
  | --- | --- | --- | --- | --- | --- |
  | `ECExampleConnector` | works | works by code, unverified | works (official example) | required | `0.13.0` |
  | `ECCPUConnector` with NIXL | works | works by code (`nixl_rocm`), unverified | not supported, no NPU copy path | not needed | `0.30.0` |
  | `ECMooncakeConnector` | works | works by code, unverified | unknown | not needed | `0.30.0` |

  The decode sidecar supports only `ec-example` and `ec-nixl`.
  `ECMooncakeConnector` needs a router that registers each prefiller address one by one, which fits `vllm-router` only and does not fit Kubernetes discovery.
- **Upstream evidence at the time of writing:**
  - `llm-d-router` carries encode filters, an encode decider and a sidecar that calls the encoders, in [v0.10.0](https://github.com/llm-d/llm-d-router/blob/v0.10.0/docs/disaggregation.md).
    The project labels the feature a proof of concept.
    The operator bundles the v0.10.0 sidecar, and v0.11.0 adds a fix that keeps the API type through the encoder stage.
    The operator already renders the `llm-d.ai/v1alpha1` picker document, and it uses no plugin that v0.11.0 removes.
  - `vllm-router` merged EPD routing in [PR 266](https://github.com/vllm-project/router/pull/266) (commit `516f1cb`).
    No tag contains it.
    The operator bundles `v0.1.15` and builds the router itself from a tag with three patches.
    The feature takes a static encoder list, does not discover or health-check encoders, and needs vLLM `0.30.0` or later ([docs](https://github.com/vllm-project/router/blob/516f1cb/docs/epd.md)).
  - `sglang-gateway` has only regular, prefill and decode workers and no encoder flag.
    SGLang `0.5.10` and later let the prefill engine call encoders through `--language-only` and `--encoder-urls` ([guide](https://github.com/sgl-project/sglang/blob/v0.5.18/docs/docs/advanced_features/epd_disaggregation.mdx)).
    `llm-d-router` with SGLang supports only E+PD upstream, and the engine manages the encoders.
  - vLLM ships the network encoder-cache connectors from `0.30.0` ([docs](https://github.com/vllm-project/vllm/blob/v0.30.0/docs/features/ec_cpu_connector.md)).
    The operator's current vLLM floor is `0.29.0`, so EPD needs its own floor.
  - vLLM-Ascend uses the filesystem connector between encoder and prefill, and `MooncakeLayerwiseConnector` for KV ([guide](https://github.com/vllm-project/vllm-ascend/blob/v0.23.0/docs/source/user_guide/feature_guide/epd_disaggregation.md)).
    It has no recommended router.
    Its example proxy is part of the data plane, because the decoder calls a metaserver endpoint on the proxy to start prefill.
    The operator's Ascend prefill/decode path relays a different handshake in the `nixlv2` sidecar mode and renders `MooncakeConnectorV1`.
    Aligning the two paths is part of the work, and the combination of `ec-example` with `MooncakeConnectorV1` has no upstream test.
    Ascend's EC and KV paths use files, MemCache and Mooncake with the `ascend` protocol, and none uses UCX or NIXL.
  - NIXL supports ROCm from [v1.3.0](https://github.com/ai-dynamo/nixl/releases/tag/v1.3.0).
    No required ROCm GPU test of its UCX backend was found upstream.
- **Sidecar fallback and target check.**
  The `llm-d-router` sidecar at `v0.11.0` drops an encoder target that its target check refuses, and then falls back to prefill/decode or to decode alone.
  The `ec-nixl` mode forwards a request without transfer data when every encoder response omits it, and when only some do.
  These paths leave a log line and a trace attribute, and the response is still a normal answer.
  The target check is off by default, and the operator renders no flag for it.
  A health probe cannot see these per-request paths, so the log is the signal in this change.
- **Where the encoder runs.**
  The vLLM encoder keeps no KV cache and drops language-model weight shards at load.
  The SGLang encoder builds no language model.
  The prefill Pod still loads the full model in vLLM.
- **Transport.**
  The Nebius and AWS machines in scope have no RDMA, so every leg uses `tcp`.
  SGLang's default encoder transfer (`zmq_to_scheduler`) is plain TCP.
- **Runner images.**
  The upstream build recipes of the NVIDIA and ROCm vLLM and SGLang runner images include NIXL and UCX.
  The CANN images show none.
  The final published images were not inspected.
- **Test model.**
  `Qwen/Qwen3-VL-8B-Instruct` is ungated and Apache-2.0 licensed, and both engines support it.
  Its bf16 weights are about 17 GB.
  A 48 GB card holds every role of a three-card test.
- **Machines.**
  A functional test needs three accelerators, one per role, on non-preemptible capacity.
  The L40S project offers a four-card single-node preset.
  The RTX PRO 6000 project offers only one-card and eight-card presets.
  Neither project has AMD accelerators or Ascend devices.
  On AWS, `g6e.12xlarge` and `g7e.24xlarge` give four cards.
  The cost of an L40S four-card node is about nine US dollars per hour on demand.
- **Order of work.**
  The API changes land first and `make generate` runs in the main checkout.
  The router image pull request goes out early, because the image must exist before the settings change.
  Proof of concept and engine rendering follow on NVIDIA.
  vLLM-Ascend and ROCm verification is the last step.
  The walkthrough is written from the online-cluster run.
- **Ascend and ROCm admission.**
  They are admitted before verification, by the project's decision.
  The cost is that an admitted deployment can run with a silent transfer failure until verification finds it.
  The documentation carries that warning, and the proof-of-concept checks for transfer evidence apply to them once hardware exists.

### Boundaries

- **Always:**
  - Keep rendering of every deployment without `Encode` unchanged, apart from the image defaults that the release notes name.
  - Land API changes first, and run `make generate` in the main checkout.
  - Move `llm-d-router`, its picker document and its sidecar to one version together.
  - Write table-driven tests for every new admission rule and rendering rule.
  - Record each upstream claim with its version, and mark unverified claims as unverified.
  - Freeze the rendered fixtures of deployments without `Encode` before any render change, and compare against them.
  - Keep Pod render changes that are not needed for `Encode` out of this change.
- **Ask first:**
  - Raising an engine version floor for shapes that have no `Encode` role.
  - Creating cloud resources beyond the agreed node shapes.
- **Never:**
  - Admit an NVIDIA combination that has no passing proof of concept.
  - Hide an unverified status or a known limit of an admitted combination.
  - Use preemptible machines for end-to-end runs.
  - Hand-edit a generated file.
  - Let a failed encoder leg degrade into a correct-looking answer with no encoder involved, without a visible signal.

### Risks and Mitigations

- Upstream support is experimental, and `vllm-router` has no release with EPD → Admit by verification only, and document the status.
- `ECCPUConnector` needs the V2 model runner and NIXL in the image → The proof of concept checks the image first, and an image gap becomes a runner image change.
- `llm-d-router` can silently fall back to a mode with no transfer when an encoder response lacks transfer parameters → The proof of concept requires transfer evidence, and the end-to-end case asserts it.
- `ECExampleConnector` has no locking, no expiry, and a hard failure when a promised file cannot be read; some network filesystems also delay new files → Document the contract, require the same claim on both roles, and leave expiry to the user.
- The `additionalVolumes` claim source cannot check the storage class → The documentation states the ReadWriteMany need, and the proof of concept uses a class that supports it.
- The decode sidecar runs with its target check off, so a request header can name any host → This is the current state for prefill/decode too. The documentation states the limit, and a follow-up issue tracks an allow list with its own credentials and permissions.
- The sidecar can fall back from the encoder leg and still answer → The visible signal is the sidecar log. The end-to-end cases inject a response with missing transfer data and read the log. A health probe cannot detect it.
- An image request can skip the prefiller, and only the prefiller consumes the encoder output → The router flow is designed to send media requests through all three roles, and the proof of concept tests a cached prefix, a short prompt and a zero threshold.
- Encode scale-down can cut a transfer that is still in flight → The controller withdraws the replica, waits a bounded window and removes it. The documentation states the effect.
- A shared claim gives no isolation between deployments → Admission requires the same claim, sub-path and mount path on both roles. The documentation advises one sub-path per deployment.
- The image defaults move with this change, so legacy prefill/decode Pods roll once → The release notes name each moved default. The structure baseline spec removes this class of roll later.
- A single `Encode` replica is a single point of failure for media requests → Document it, and allow more than one replica.
- Role-kind logic elsewhere treats "not prefill, not decode" as "server-like" → Inventory every such branch and cover each with a test.
- vLLM-Ascend and ROCm are admitted before verification, so a silent transfer failure can ship → State the unverified status next to the admission rule, and run the transfer-evidence checks as soon as hardware exists.
- The router image must exist before the settings change → Open the image pull request first, and gate the EPD rendering that needs it on the settings change.
- Moving `vllm-router` or `llm-d-router` can break the patches the operator applies → The image build fails on a patch mismatch, so a bump shows up before release.

## Design Details

### Commands

Build and unit tests run on the local macOS host.
The local `go` is 1.27, and it refuses the `GODEBUG=gotypesalias=0` value that `hack/generate.sh` and `hack/test.sh` set.
Put the cached Go 1.26.4 toolchain first in `PATH`, and set `GOTOOLCHAIN=local`:

```bash
TC="$(go env GOMODCACHE)/golang.org/toolchain@v0.0.1-go1.26.4.darwin-arm64"
export PATH="$TC/bin:$PATH" GOTOOLCHAIN=local

make generate                       # run in the main checkout, after API edits
make lint                           # Go and shell sources
make lint docs                      # Markdown and the docs index
go test ./pkg/worker/... ./api/...  # unit tests for the touched packages
```

Accelerator runs and end-to-end cases run on a remote cluster that the lead builds with `testing/infra`, with the L40S four-card node shape on non-preemptible capacity.
The workers reach it through a kubectl context.
The end-to-end commands are in [the end-to-end skill](../.agents/skills/gpustack-operator-e2e/SKILL.md).

### Project Structure

- `api/worker/v1alpha1/model_deployment.go`: the kind constants, the enum markers and the additional volume type.
- `pkg/worker/webhooks/worker/model_deployment.go`: role-kind, router, engine and volume admission.
- `pkg/worker/controllers/worker/model_deployment_connector.go`: kind mapping and owned keys.
- `pkg/worker/controllers/worker/model_deployment_render.go`: Pod rendering, the decode sidecar and additional volumes.
- `pkg/worker/kvcache/inject/`: engine argument rendering (`vllm.go`, `sglang.go`).
- `pkg/worker/kvcache/router/`: router documents and command lines (`router.go`, `argv.go`).
- `pkg/worker/controllers/worker/model_deployment_service.go`: Services and the front door.
- `pkg/worker/controllers/worker/model_deployment_status.go`: role summary and role conditions.
- `pkg/worker/webhooks/worker/model_deployment_epd.go`: the admitted matrix, as a data table, and the EPD admission rules.
- `pkg/worker/controllers/worker/testdata/`: frozen render fixtures of deployments without `Encode`.
- `pkg/worker/settings/value.go`: the router and sidecar image defaults.
- `pack/llm-router/`: the router image build, its version arguments and its patches.
- `.agents/skills/gpustack-operator-e2e/cases/`: the end-to-end cases and their index table.
- `docs/modules/model-deployment/`: the module pages.

### Code Style

The kind constants follow this form, and `Encode` joins them with a doc comment in the same voice:

```go
// ModelDeploymentRoleKindDecode is a role that consumes KV blocks a prefiller produced and
// generates tokens from them.
ModelDeploymentRoleKindDecode ModelDeploymentRoleKind = "Decode"
```

Conventions that apply:

- Tests are table-driven and assert the observable final state.
- Files use snake_case names.
- Comments are plain and short, with no spec task identifiers.

### Source Baseline

The baseline is commit `1259d0f1`.
These places assume that a role is exactly `Server`, `Prefill` or `Decode`, and each needs a check or a change:

- The kind enum markers on the spec and the status ([API](../api/worker/v1alpha1/model_deployment.go)).
- The role-kind validation that refuses `Server` beside other kinds and a repeated non-`Server` kind ([webhook](../pkg/worker/webhooks/worker/model_deployment.go)).
- The mapping from kind to the upstream spelling and the label value, the owned-key table, and the "declares both halves" test ([connector](../pkg/worker/controllers/worker/model_deployment_connector.go)).
- The branches that treat any kind other than `Decode` as a publisher of KV events, and any kind other than `Prefill` or `Decode` as server-like.
- The deployment-wide Service, which selects the first listed role ([service](../pkg/worker/controllers/worker/model_deployment_service.go)).
  The tokenizer role choice is in the router controller, sorts by name, and prefers `Prefill`, then `Server` ([router controller](../pkg/worker/controllers/worker/model_deployment_router.go)).
- The KV event publisher check and the llm-d publisher selector, which exclude `Decode` only, so `Encode` would count as a KV publisher ([kv events](../pkg/worker/controllers/worker/model_deployment_kv_events.go)).
- Drain and retirement, which recognize vLLM load-balance modes and SGLang prefill/decode flags, and know no encoder-cache mode ([drain collector](../pkg/worker/controllers/worker/model_deployment_drain_collector.go)).
- The metrics snapshot, which asks every engine Pod for running and waiting gauges and cache-hit counters, and treats a missing series as partial ([metrics](../pkg/worker/extensionapis/worker/model_deployment.metrics.go)).
- The accelerator-sharing refusal, which compares `Prefill` with `Decode` only, and the parallelism pair, which must stay prefill/decode only.
- The routing sidecar renderer, which receives one role and renders no target check ([render](../pkg/worker/controllers/worker/model_deployment_render.go)).
- The router documents, which build one filter and one profile per kind but wire the disaggregation handler for prefill and decode only ([router](../pkg/worker/kvcache/router/router.go), [argv](../pkg/worker/kvcache/router/argv.go)).
- The additional volume renderer, which handles ConfigMap, Secret and host path sources only ([render](../pkg/worker/controllers/worker/model_deployment_render.go)).

### Implementation Plan

Every task is a narrow path through the layers it touches.
`Owns` lists the paths that only that task edits.
Three edges come from a shared file, not from a data dependency:
T9 follows T6 (`model_deployment_render.go`), T7b follows T7a (`inject/vllm.go`), and T14b follows T14a, T17 follows T16 (shared index files).

**Phase 0. Start in parallel.**

- [x] **T0 · Freeze the render of deployments without `Encode`**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_legacy_render_test.go`, `pkg/worker/controllers/worker/testdata/legacy-render/**`
      Acceptance: golden fixtures cover a defaulted `Server`, an explicit `Server`, a lone prefill/decode pair, a managed pair, a shared-pool pair, an Ascend pair and a role with `command`.
      Each fixture holds the full rendered Pods and their `pod-spec-hash` annotations.
      The test fails when any byte changes.
      Verify: `go test ./pkg/worker/controllers/worker/ -count=1`

- [x] **T1 · API: `Encode` kind and `persistentVolumeClaim` volume source**
      Blocked by: None
      Owns: `api/**`, `pkg/kubeclients/**`
      Gate: review
      Acceptance: both enum markers (spec and status) list `Encode`.
      `ModelDeploymentAdditionalVolume` gains a `persistentVolumeClaim` source with a local claim name.
      `make generate` ran in the main checkout, and the diff holds generated files only beside the two type edits.
      Verify: `make generate && go build ./... && go test ./api/... -count=1`

- [x] **T2 · Router image: bump `llm-d-router`**
      Blocked by: None
      Owns: `pack/llm-router/**`, `.github/workflows/base-image.yml`
      Gate: review
      Acceptance: `llm-d-router` builds at `v0.11.0`, and each of its patches applies.
      A deliberately broken patch fails the build.
      `vllm-router` stays at its current tag in this task.
      Verify: `git apply --check` of each patch on its pinned upstream ref.

- [x] **T2b · Router image: `vllm-router` with the EPD feature**
      Blocked by: T2
      Owns: `pack/llm-router/**`, `.github/workflows/base-image.yml`
      Gate: review
      Acceptance: the image builds `vllm-router` with upstream PR 266 (commit `516f1cb`), either from that commit or from a tag plus a carried patch.
      The agent picks the way and records the evidence.
      Every patch applies there, and a broken patch fails the build.
      Comments in the Dockerfile and in each carried or rebased patch say why this is not a tag and what to do when a tag appears.
      The report lists the router flags and the encoder list format that enable EPD.
      Verify: `git apply --check` and a real apply of each patch on the pinned state, and `cargo check` on the patched tree when `cargo` exists.

- [ ] **T3a · Proof of concept: vLLM with `llm-d-router`**
      Blocked by: None
      Owns: None (cluster only; the findings go to T13 and T15)
      Gate: review
      Acceptance: `ec-nixl` and `ec-example` each pass the F7 checks on three L40S cards from hand-written manifests.
      The run covers a cached image prefix, a short prompt, a zero threshold, a text-only request, and a response with missing transfer data.
      The report states the model, the versions, the image contents (V2 runner variable and NIXL) and every failure.
      Verify: the report holds captured request output and the sidecar log lines that show the encoder call and the transfer.

- [ ] **T3b · Proof of concept: SGLang**
      Blocked by: None
      Owns: None (cluster only)
      Gate: review
      Acceptance: SGLang `--encoder-only` and `--language-only` pass the F7 checks behind `sglang-gateway` and behind `llm-d-router`.
      The report says which pairing works, and which is refused.
      Verify: as T3a.

- [ ] **T3c · Proof of concept: `vllm-router` with vLLM**
      Blocked by: T2b
      Owns: None (cluster only)
      Gate: review
      Acceptance: the T2b image runs `--epd-config` against vLLM `0.30.0` and passes the F7 checks, or the report says why it does not.
      Verify: as T3a.

**Phase 1. Role plumbing.**

- [x] **T4 · Role-kind plumbing and guard test**
      Blocked by: T0, T1
      Owns: `pkg/worker/controllers/worker/model_deployment_connector*.go`, `pkg/worker/controllers/worker/model_deployment.go`, `pkg/worker/kvcache/inject/types*.go`
      Gate: review
      Acceptance: `Encode` has its own role value and is never mapped to `RoleNone`.
      `ModelDeploymentDeclaresBothHalves` and `ParallelismPair` still read prefill/decode only.
      The controller skips KV connection and KV event synthesis for `Encode`.
      A guard test runs every role kind through every kind-dependent function and asserts its class, so a new kind that misses a site fails the test.
      The T0 fixtures still pass.
      Verify: `go test ./pkg/worker/controllers/worker/ ./pkg/worker/kvcache/... -count=1`

**Phase 2. Admission, volume, engines, routers.**

- [x] **T5 · Admission rules and the admitted matrix**
      Blocked by: T1, T4
      Owns: `pkg/worker/webhooks/worker/model_deployment*.go`
      Gate: review
      Acceptance: every F2 rule has table-driven cases for accept and for refuse, on create and on update.
      The matrix is a data table.
      It starts with no NVIDIA entry, and it holds vLLM-Ascend and vLLM on ROCm.
      Role-name-set and kind immutability still hold, and a pure reorder stays legal.
      Verify: `go test ./pkg/worker/webhooks/worker/ -count=1`

- [x] **T6 · Mount a claim through `additionalVolumes`**
      Blocked by: T0, T1
      Owns: `pkg/worker/controllers/worker/model_deployment_render.go`, `pkg/worker/controllers/worker/model_deployment_render_test.go`
      Acceptance: a `persistentVolumeClaim` source renders a Pod volume and mount with the sub-path and read-only fields.
      A same-name claim in another namespace is never referenced.
      Verify: `go test ./pkg/worker/controllers/worker/ -count=1`

- [ ] **T7a · vLLM engine leg on NVIDIA and ROCm**
      Blocked by: T4
      Owns: `pkg/worker/kvcache/inject/vllm*.go`
      Gate: review
      Acceptance: `Encode` renders as a producer with no KV store, no KV events and no token-generation flags.
      `Prefill` renders an encoder-cache consumer composed with its unchanged KV producer.
      The `ECCPUConnector` path sets the V2 runner variable and the side-channel address and port.
      The `ECExampleConnector` path sets the shared directory to the mount path.
      A user value for an owned key is refused, in every spelling of the flag.
      Verify: `go test ./pkg/worker/kvcache/inject/ -count=1`

- [ ] **T7b · vLLM-Ascend engine leg**
      Blocked by: T7a
      Owns: `pkg/worker/kvcache/inject/vllm*.go`
      Gate: review
      Acceptance: Ascend uses `ECExampleConnector` with the user-attached claim and keeps `MooncakeConnectorV1` for KV.
      Verify: `go test ./pkg/worker/kvcache/inject/ -count=1`

- [ ] **T8 · SGLang engine leg**
      Blocked by: T4
      Owns: `pkg/worker/kvcache/inject/sglang*.go`
      Gate: review
      Acceptance: `Encode` renders `--encoder-only`, and `Prefill` renders `--language-only` with the encoder Service address.
      The encoder reply and bootstrap ports are distinct from the HTTP port.
      Prefill/decode rendering without `Encode` is unchanged.
      Verify: `go test ./pkg/worker/kvcache/inject/ -count=1`

- [ ] **T9 · `llm-d-router` rendering**
      Blocked by: T4, T6
      Owns: `pkg/worker/kvcache/router/router*.go`, `pkg/worker/controllers/worker/model_deployment_router.go`, `pkg/worker/controllers/worker/model_deployment_render.go`
      Gate: review
      Acceptance: the picker document has an encode filter, an encode profile with no KV or prefix scorer, and the multimodal decider.
      The handler wires the encode stage.
      `Encode` Pods are in stage discovery and are not in the KV publisher set.
      The decode sidecar gets `--ec-connector` for the engine leg.
      Verify: `go test ./pkg/worker/kvcache/router/ ./pkg/worker/controllers/worker/ -count=1`

- [ ] **T10 · `vllm-router` and `sglang-gateway` rendering**
      Blocked by: T4, T2b
      Owns: `pkg/worker/kvcache/router/argv*.go`
      Gate: review
      Acceptance: `vllm-router` receives its encoder list, behind the T3c result.
      `sglang-gateway` renders prefill/decode as before, and `Encode` is in none of its selectors, in every role order.
      Verify: `go test ./pkg/worker/kvcache/router/ -count=1`

- [ ] **T11 · Settings: router and sidecar image defaults**
      Blocked by: T2, T2b (the image is published and the sidecar is mirrored)
      Owns: `pkg/worker/settings/value.go`, `docs/reference/settings.md`
      Acceptance: the defaults point at the published `llm-router` image and the mirrored sidecar at `v0.11.0`.
      The change lists each moved default for the release notes.
      Verify: `make lint && go test ./pkg/worker/settings/... -count=1`

- [x] **T12a · Services, front door, status and joint admission**
      Blocked by: T4
      Owns: `pkg/worker/controllers/worker/model_deployment_service.go`, `pkg/worker/controllers/worker/model_deployment_status.go`, `pkg/worker/controllers/worker/model_deployment_kv_events.go`, `pkg/worker/controllers/worker/model_deployment_joint_admission_test.go`
      Acceptance: `<deployment>-<role>` Service exists for `Encode`.
      The deployment Service and `status.endpoint` never point at `Encode`, with `Encode` first, middle or last.
      `roleSummary` reads `E`, `P`, `D` in that order, and `RoleKindsReady` counts `Encode`.
      The KV events condition ignores `Encode`.
      A three-group initial admission test holds all groups back when one lacks quota.
      Verify: `go test ./pkg/worker/controllers/worker/ -count=1`

- [ ] **T12b · Metrics, drain and retirement of `Encode`**
      Blocked by: T4
      Owns: `pkg/worker/extensionapis/worker/model_deployment.metrics.go`, `pkg/worker/controllers/worker/model_deployment_drain*.go`, `pkg/worker/controllers/worker/model_deployment_retirement*.go`, `pkg/worker/controllers/worker/model_deployment_group_forward_probe.go`
      Acceptance: a healthy `Encode` Pod is not partial for missing series, and a failed scrape is partial.
      Retirement withdraws the replica, waits the bounded window and removes it, and never reads a missing gauge as idle.
      The group-forward probe treats `Encode` as not applicable.
      The `Encode` Pod has startup, readiness and liveness probes.
      Verify: `go test ./pkg/worker/extensionapis/worker/ ./pkg/worker/controllers/worker/ -count=1`

**Phase 3. Documents and cases.**

- [ ] **T13 · Documents**
      Blocked by: T5, T7a, T8, T9
      Owns: `docs/modules/model-deployment/epd.md`, `docs/modules/model-deployment/engine-versions.md`, `docs/modules/model-deployment/routing.md`, `docs/modules/model-deployment/status.md`, `docs/modules/model-deployment/_index.md`, `docs/README.md`
      Acceptance: F10 holds, and the limits of F10 sit beside each admission rule.
      `make lint docs` passes.
      Verify: `make lint docs`

- [ ] **T14a · End-to-end cases without an accelerator (116 to 119)**
      Blocked by: T5, T6, T12a
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-116.sh` to `case-119.sh`, `.agents/skills/gpustack-operator-e2e/SKILL.md`
      Acceptance: cases cover admission refusals, three-group joint admission, role Services and the claim mount.
      Each header states its hardware need and skip condition, and the case table row matches the header.
      Verify: `make lint agents-shell` and a run on the cluster through the end-to-end skill.

- [ ] **T14b · End-to-end cases with an accelerator (120 to 125)**
      Blocked by: T14a, T7a, T8, T9, T11
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-120.sh` to `case-125.sh`, `.agents/skills/gpustack-operator-e2e/SKILL.md`
      Acceptance: one functional case per combination: vLLM with `llm-d-router`, with `ec-example`, SGLang, `vllm-router`, Ascend and ROCm.
      The Ascend and ROCm cases skip with a named reason when the hardware is absent.
      Verify: `make lint agents-shell`

**Phase 4. Live runs and evidence.**

- [ ] **T15 · Live run, matrix update and no-roll check**
      Blocked by: T3a, T3b, T5, T14b
      Owns: `pkg/worker/webhooks/worker/model_deployment_epd.go`
      Gate: review
      Acceptance: the lead builds the cluster with `testing/infra` and runs T14a and T14b.
      The matrix table gains an NVIDIA entry only for a combination with a passing run.
      A legacy prefill/decode deployment under pinned image settings keeps its Pod UIDs across the operator upgrade.
      Verify: the case results from the end-to-end skill, and `go test ./pkg/worker/webhooks/worker/ -count=1`

- [ ] **T16 · Three-arm benchmark report**
      Blocked by: T15, T13
      Owns: `docs/modules/model-deployment/epd-benchmark.md`, `docs/README.md`
      Acceptance: F8 holds, with fixed concurrency values, warm-up and repetitions in the page.
      Verify: `make lint docs`

- [ ] **T17 · EPD walkthrough**
      Blocked by: T15, T13, T16
      Owns: `docs/walkthroughs/model-deployment/epd.md`, `docs/walkthroughs/model-deployment/_index.md`, `docs/README.md`
      Acceptance: F11 holds, and every output block comes from the T15 run.
      Verify: `make lint docs`

- [ ] **T18 · Ascend and ROCm verification (last)**
      Blocked by: T7b, T13, T15
      Owns: `docs/modules/model-deployment/engine-versions.md`
      Gate: review
      Acceptance: the F7 checks run on Ascend and ROCm hardware that the project supplies, and the page records pass or fail with the conditions of the run.
      Admission does not change.
      Verify: `make lint docs`

**Agent assignment (proposed).**
The lead runs T1, T15 and T17, and owns every commit and merge.
mcode takes T2, T2b, T3a to T3c, T5, T7a, T7b, T9 and T14b.
qwen takes T0, T4, T8 and T14a, and reviews every omp task.
omp takes T6, T10, T11, T12a and T12b, and qwen redoes any omp work that review rejects.
agy takes T13 and T16.
The T1 task runs in the main checkout, and every other task runs in its own worktree.

### Test Plan
[ ] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- T0 freezes the render of deployments without `Encode`.
  The later render tasks must keep those fixtures green.
- The `Needs` column of the end-to-end case table repeats the case headers and nothing checks it.
  T14a and T14b update the column in the same commit as each case.
- The local Go toolchain needs the 1.26.4 `PATH` setting under Commands.

#### Unit tests

Coverage was measured on 2026-10-13 with Go 1.26.4.
Each package keeps at least its current coverage, and every new branch has a table-driven case.

- `api/worker/v1alpha1`: 2026-10-13 - 4.9% (mostly generated code; the task adds the kind and volume type tests)
- `pkg/worker/kvcache`: 2026-10-13 - 100.0%
- `pkg/worker/kvcache/inject`: 2026-10-13 - 91.6%
- `pkg/worker/kvcache/mooncake`: 2026-10-13 - 93.7%
- `pkg/worker/kvcache/router`: 2026-10-13 - 93.8%
- `pkg/worker/webhooks/worker`: 2026-10-13 - 90.2%
- `pkg/worker/extensionapis/worker`: 2026-10-13 - 54.7%
- `pkg/worker/controllers/worker`: 2026-10-13 - 84.3%

Cases to cover:

- Legacy fixtures: complete rendered Pods and hashes for the seven shapes of T0, unchanged after every task.
- Guard test: every role kind through every kind-dependent function.
- Rendering: `Encode` has no KV store, KV events, bootstrap or token flags. `Prefill` carries both legs. Owned keys are refused in every flag spelling and in dotted vLLM keys.
- Router: the encode profile has no KV or prefix scorer and is wired into the handler. `Encode` is in no publisher selector and in no `sglang-gateway` selector. Every role order is covered.
- Services: the deployment Service and endpoint skip `Encode` with `Encode` first, middle and last.
- Volume: one source only, a non-empty claim name, matching claim, sub-path and mount path, a writable `Encode` mount, duplicate paths and traversal.
- Admission: exact triple, `Server` mix, no router, `kvCache`, `command`, platform tuples, unknown platform, missing version, version floor, custom image with a version, size above one, External DP, Elastic EP, and revalidation on update.
- Status and metrics: `E`, `P`, `D` order; `Encode` not partial for missing series; a failed scrape partial.
- Drain: withdraw, bounded wait, removal; a missing gauge never reads as idle.
- Generated code: deep copy and conversion of the new volume source without aliasing.

#### Integration tests

No separate integration suite exists for this area.
Reconcile-level tests use fake clients in `pkg/worker/controllers/worker`, which is the repository convention.
Names are added after the implementation pull requests merge.
Planned scenarios:

- Three groups stay pending when one lacks quota, with a shared queue and with separate queues.
- Admitted siblings keep serving during an `Encode` replacement or a quota-blocked scale-up.
- A same-name claim in another namespace is never resolved.
- Encode membership survives a replacement, watch lag and a controller restart.

#### e2e tests

Cases use non-preemptible machines.
The cluster comes from `testing/infra`.

- Without an accelerator:
  - Case 116: admission refusals.
  - Case 117: role Services and the front door.
  - Case 118: three-group joint admission on a CPU-only queue.
  - Case 119: the shared claim mounts the same files in two Pods.
- With an accelerator (three L40S cards per run):
  - Case 120: vLLM with `llm-d-router`, `ec-nixl`.
  - Case 121: vLLM with `llm-d-router`, `ec-example` on a ReadWriteMany claim.
  - Case 122: SGLang.
  - Case 123: `vllm-router` with vLLM, only if T3c passes.
  - Case 124: vLLM-Ascend, skipped when no hardware exists.
  - Case 125: vLLM on ROCm, skipped when no hardware exists.
- Every accelerator case sends an uncached image request, then a cached prefix, a short prompt and a text-only request.
  It reads the encoder log, the transfer evidence and the answer, and one run injects a response with missing transfer data and reads the sidecar log.
- A legacy prefill/decode deployment under pinned image settings keeps its Pod UIDs across the operator upgrade.

## Alternatives

- **Add an "encoder" switch to the `Prefill` role.**
  It is the E+P-in-one-Pod shape, which is out of scope, and it hides a separate scaling unit.
- **Admit every candidate combination now and mark them unverified.**
  It is faster, but "admitted" reads as "works", and prefill/decode already shows that a wrong pairing can answer normally while moving no data.
  The project chose this path for vLLM-Ascend and vLLM on ROCm only, and kept the proof-of-concept gate for NVIDIA.
- **Ship an operator-owned proxy that drives the encoder.**
  It avoids waiting for upstream routers.
  It adds a component this repository must maintain, and it duplicates the sidecar in `llm-d-router`.
  It is the fallback if the proof of concept fails for every vLLM combination.
- **Wait for the `vllm-router` release.**
  The operator then ships no vLLM pair on that router until a release exists.
  The spec keeps the pair as a candidate, gated by release and proof of concept.
- **A dedicated `roles[].sharedVolume` field for the shared claim.**
  Its meaning is clearer, but it adds a type and a render path that serve one connector.
  Extending `additionalVolumes` costs one field and one render branch, and other features can use it.
- **Use the claim source of `ModelArtifact` for the shared volume.**
  It mounts read-only, so the encoder cannot write.

## Open Questions

1. Does the `ECCPUConnector` path with NIXL work end to end on the operator's runner images, and does a published image carry the V2 runner setting and NIXL?
   The proof of concept answers this, and a gap may require a runner image change.
2. What is the vLLM floor for EPD?
   The network connectors start at `0.30.0`, but the `llm-d` guide still pins a patched image.
3. When does verification of vLLM-Ascend and vLLM on ROCm run, and who supplies the Ascend and AMD accelerators?
   Admission does not wait for it.
4. Does the `vllm-router` image keep its three patches applied on the EPD commit?
   The project decided to take upstream commit `516f1cb` now and move to a tag later.
   T2b chooses between building from the commit and carrying it as a patch on a tag.
5. Does the `Encode` role need tensor parallelism inside one Pod?
   The upstream Mooncake encoder-cache connector needs tensor, pipeline and data parallel size 1 on the producer.
6. Which `Encode` metrics belong in the deployment metrics snapshot beyond scrape health?
   Upstream engine metrics for the encoder-cache connector are not in a released vLLM at the time of writing.
7. How long is the bounded wait before an `Encode` replica is removed?
   The proof of concept measures how long the consumer pulls after the encoder response.
8. Which test model and which environment decide the image request cases?
   The router flow is designed from the upstream code first.
   The final run on the cluster confirms or corrects it.
9. A later change may add an allow list for encoder targets, or an operator condition for encoder-leg failures.
   Both are out of scope here.
10. The admission rule reads the claim name only.
    Admission does not check the access mode of the claim, and the documentation states the ReadWriteMany need.
