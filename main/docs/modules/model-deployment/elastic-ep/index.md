# Elastic EP

An elastic expert-parallel deployment keeps one vLLM serving instance and changes its GPU collective
while it runs. The operator adds or removes GPU members inside one Ray cluster.
The engine reconfigures while Kubernetes, Kueue, Ray and accelerator accounting converge.

## Contents

- [What the profile means](#what-the-profile-means)
- [Requirements](#requirements)
- [Shared memory for GPU members](#shared-memory-for-gpu-members)
- [A minimal manifest](#a-minimal-manifest)
- [Changing the width](#changing-the-width)
- [What you see while it runs](#what-you-see-while-it-runs)
- [When a resize holds](#when-a-resize-holds)
- [Interruption scope](#interruption-scope)
- [Diagnosis](#diagnosis)
- [Limits](#limits)

## What the profile means

`roles[].elasticEp` selects this mode for a role. Two fields matter:

| Field | Meaning |
|---|---|
| `width` | Total GPU engines in the collective, counting the reserved master. Mutable, from 2 to 64. |
| `headInstanceType` | The CPU-only InstanceType the Ray control plane runs on. Set once. |

`width` counts DP engines, including the master. Each member has its own GPU Pod.
Set TP through the role's `extraArgs`, using the engine's `--tensor-parallel-size` argument.
Omitting it uses TP=1. Total GPU demand is `width * TP`.
For TP=2, width 2 needs four GPUs; width 4 needs eight GPUs.

Change the width to grow the collective; do not use `replicas` or `size` for this.
This release accepts scale-up and unchanged width only. The admission webhook rejects a width decrease;
safe scale-down is tracked in [issue #741](https://github.com/gpustack/gpustack-operator/issues/741).

Once a role carries `elasticEp`, the profile is fixed for that deployment and the admission webhook
enforces the rest:

- the deployment has exactly one role, and it is a Server role;
- `replicas` and `size` are both 1, because each member is exactly one Pod;
- every member asks for exactly TP whole unsliced GPUs;
- each member's TP group stays inside one Pod and one Ray node;
- pipeline and prefill context parallel sizes stay 1;
- you cannot supply `command`, because the renderer owns the command line of the managed role;
- there is no initial fabric to declare.

Adding or removing the profile on an existing role is refused.
`headInstanceType` and the effective TP value are fixed after creation.
Equivalent TP argument spellings are accepted. Omitting TP and explicitly setting TP=1 are equivalent.
A width change adjusts DP and EP while preserving TP. Online TP changes are refused.

Only the master serves HTTP. The other members join the Ray cluster and hold collective state. That
is why the master is counted inside `width` and is never retired.

## Requirements

The engine image must be a vLLM 0.29.0 image that already contains a full Ray installation. The
official vLLM image of that version ships without Ray, and the profile will not start on it. The
pinned requirement in the resize spec is Ray 2.56.1 with the full default closure.

The TP size must fit the selected MoE model, engine kernels, and GPUs available on one node.
The operator does not limit TP to the size used by a particular test.

The Ray control plane needs its own Pod, which is why `headInstanceType` is required. It names an
InstanceType that must already exist and must be CPU-only.

The profile increases the number of GPU members. It does not resize the underlying nodes or claim
cloud node elasticity, and it does not patch anything upstream in the engine. It drives the engine
through the resize interface the engine already exposes.

## Shared memory for GPU members

Each Elastic GPU member gets a separate memory-backed `EmptyDir` mounted at `/dev/shm`.
Its size limit is 512 MiB. This includes the master and workers. The CPU head has no default mount.
The volume is isolated per Pod and uses the Pod's memory allocation.

An explicit `/dev/shm` mount takes precedence. Its capacity and backing remain the user's responsibility.
Sufficiency at 512 MiB is not established. A workload that needs more must declare its own `/dev/shm` mount.

## A minimal manifest

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelDeployment
metadata:
  name: qwen3-ep
  namespace: gpustack
spec:
  model:
    name: Qwen/Qwen3-30B-A3B
    artifactRef:
      name: qwen3-30b-a3b-artifact
  router:
    name: llm-d-router
  engine:
    name: vllm
    version: 0.29.0
  roles:
    - name: server
      kind: server
      replicas: 1
      size: 1
      instanceType: elastic-gpu
      image: your-registry/vllm-ray:0.29.0
      elasticEp:
        width: 2
        headInstanceType: elastic-cpu
      ports:
        - name: http
          port: 8000
          protocol: TCP
```

Weights reach the members through the same artifact and cache as any other deployment. See
[Model Delivery](/gpustack-operator/main/docs/modules/model-delivery/index.md).

To use TP=2, add `extraArgs: ["--tensor-parallel-size", "2"]` and request `resources.accelerator: "2"`.
If the accelerator quantity is omitted, admission defaults it to the TP size.
Explicit quantities must equal the TP size. Each member's CPU and memory scale with its GPU count.

## Changing the width

Change only `width`. Starting at 2, this patch requests 4. Wait for the resize to complete before
submitting another change. A patch that lowers the current width is rejected by the admission
webhook in this release; it does not reach the engine.

```bash
kubectl patch modeldeployment qwen3-ep -n gpustack --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/elasticEp/width","value":4}]'
```

The operator keeps the master. It keeps the master's command, its UID and its bootstrap width
annotation, so the running instance is not replaced. In this release it grows the Ray worker
members around that master; it does not shrink them.

Scaling up is refused while the capacity it needs is missing. If Kueue has no quota for the extra
members, or the members are admitted but hold no GPU allocation, or they have not joined Ray yet,
the resize holds and the already-serving master keeps serving. An independent width change can wait
without interrupting what is already running.

## What you see while it runs

The operator observes these values separately:

| Observation | Question it answers |
|---|---|
| desired | What width the spec asks for |
| admitted | How many members Kubernetes admitted |
| allocated | How many members hold a verified whole GPU allocation |
| Ray | How many members joined the Ray cluster |
| effective | What width runtime identities and successful rank requests prove |

They legitimately disagree during a resize. More members admitted does not mean more Ray members,
and a wider engine does not mean more admitted Pods. A layer that was not read is reported as
unknown rather than as a number borrowed from a neighbour.

The operator writes these into an internal ConfigMap owned by the deployment, whose data key is
`observation.json`. Its top-level fields are lower case and each layer object carries `Known`,
`Value` and `Reason`. This is diagnostic internal state that the operator writes for its own
reconciliation. It is not a new public status API, and it is not a field on the deployment's
status.

Native resize makes the engine answer 503 on its endpoints for the length of the window. The master
Pod's liveness probe accepts 200 and 503 so the kubelet does not restart an engine that is
mid-resize. Readiness still withdraws, which is how traffic is kept away from the window.

## When a resize holds

The operator records its intent before it sends one request to the engine. After that, a `500`, a
`408`, a timeout or a controller restart is not a reason to send another. An unanswered request and
a request that was never sent look identical from outside, and resending against an engine that
already applied the change is how a resize gets applied twice.

A definite refusal is a different answer. When the engine answers and declines — a `408`, a `5xx`,
its own busy `503`, or a command it rejected — the record keeps that refusal and the operator
re-issues the command with backoff: the first retry waits 15 seconds, each later one doubles up to
a 4-minute ceiling, and the bound is five attempts. A retry goes only to an engine that answered it
is not mid-resize.

When the bound is spent the operation is abandoned, and the deployment's `ElasticResize` condition
turns `False` with reason `ScaleRefused`, carrying the engine's own last refusal. An answer that
never arrived is still never resent: it consumes none of the bound, and the outcome is decided by
observation.

A resize can therefore stay unresolved. Common reasons:

- capacity for a wider target is not admitted, allocated or joined to Ray;
- a member's actor state cannot be read, so no member is proven free to remove;
- a later spec generation arrived while the earlier operation was still unresolved;
- the sent intent is ambiguous, so the operator waits rather than guessing.

A changed generation first triggers fresh native and resource observations. An unsent record can
clear after its old width is proved, while the desired target remains the same. A sent record clears
only after its target and any resource return are proved. This observation pass sends no new resize
and deletes no members. Missing proof keeps the record and capacity. Inspect the observation
ConfigMap to identify the missing proof and its cause.

Two stale shapes recover on their own. A narrower spec arriving behind an unresolved widening
retires the stale record instead of parking behind it. A node loss that takes every captured
member — replacement Pods carrying identities the record can never match — is re-derived from the
live world once the spec has moved on, with the native width proved over live members before any
record moves.

A partial survival still holds, because the captured member that lived may still have retirement
work pending. A same-generation replacement keeps the hold for sent or upward records, which may
still describe an in-flight operation. An unsent downward record with no captured members can
recover after the live world proves the current width.

## Interruption scope

New scale-down requests are not available in this release. The API rejects a width decrease before
reconciliation. An in-flight narrowing operation carried over from an earlier accepted request may
still complete through the controller. The safe withdrawal, actor-free retirement and
accelerator-return flow remains tracked in [issue #741](https://github.com/gpustack/gpustack-operator/issues/741).

The scope of the interruption is the deployment you patched. It does not extend to a separate
deployment. A second deployment keeps serving its own traffic while the first is resizing, but only
requests actually observed on the second deployment's own endpoint are evidence of that. The
number of deployments present says nothing about routing.

## Diagnosis

1. Read the deployment's `ElasticResize` condition: `False` with `ScaleRefused` means the engine's
   refusals spent the retry bound and the message carries the last refusal; `Unknown` with
   `WorldUnprovable` means the operation record is gone and the live world cannot be proved, so the
   corrective scale waits for observation to prove it. [Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md) gives
   the condition vocabulary; read the rest of the status for admission, startup and serving
   failures.
2. Read the deployment-owned observation ConfigMap. Its `reason` names an elastic hold.
   A layer with `Known` false carries its own `Reason`. Check the observed generation against the deployment.
3. Compare the five layers. A width that never became effective, with Ray holding the target width,
   points at the engine. An admitted count that never moved points at quota or scheduling. A Ray
   count that never moved points at members that have not joined.
4. On a hold after an ambiguous send, do not resend. Confirm the engine's own reported width first.

### Ray logs

The Ray head and every GPU member send Ray system logs to container stdout and stderr by default.
Read them with `kubectl logs` on each Pod. The default level is `info`.
Set these variables in the elastic role's `env`; the operator also passes them to the CPU head:

| Variable | Default | Scope |
|---|---|---|
| `RAY_LOG_TO_STDERR` | `1` | Ray system logs. Set `0` to keep Ray's log files instead. |
| `RAY_LOGGER_LEVEL` | `info` | Ray Python components, including the dashboard and runtime environment agents. |
| `RAY_BACKEND_LOG_LEVEL` | `info` | Ray C++ components, including GCS, raylet and the core worker. |
| `RAY_DEDUP_LOGS` | Ray's default | Set `0` to disable deduplication of logs forwarded to a driver. |

For more detail, set both level variables to `debug`. Ray's Python components accept
`debug`, `info`, `warning`, `error` and `critical`. The C++ components accept
`trace`, `debug`, `info`, `warning`, `error` and `fatal`.

Use `VLLM_LOGGING_LEVEL` separately for vLLM logs; it stays on the GPU members.

Set the variables before creating the deployment. Changes apply when new Pods are created.
The operator retains existing elastic Pods, so an env edit does not change their running loggers.

## Limits

- The engine must be a vLLM 0.29.0 image with a full Ray installation supplied by you.
- Width is bounded to 2 through 64.
- The profile cannot be added to or removed from an existing role, and `headInstanceType` cannot
  change.
- There is one serving instance per deployment. `replicas` and `size` are not a width mechanism.
- Only the master serves HTTP.
- No RDMA fabric is requested for this profile.
- Scale-down is not supported. A width decrease is rejected by admission; see [issue #741](https://github.com/gpustack/gpustack-operator/issues/741).

**See also** — [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md) for the role contract ·
[Prefill and Decode](/gpustack-operator/main/docs/modules/model-deployment/prefill-decode/index.md) for the other way to split a deployment ·
[Routing](/gpustack-operator/main/docs/modules/model-deployment/routing/index.md) for how replicas are selected and observed ·
[Shutdown](/gpustack-operator/main/docs/modules/model-deployment/shutdown/index.md) for retirement and drain ·
[Model Deployment Status](/gpustack-operator/main/docs/modules/model-deployment/status/index.md) for the published fields.

**Next** → [Model Deployment Configuration](/gpustack-operator/main/docs/modules/model-deployment/deployment/index.md)
