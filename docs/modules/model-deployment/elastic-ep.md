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

`width` counts engines, not Pods you manage. A width of 2 is one master plus one worker, so two GPUs
in total. Change the width to change the collective; do not use `replicas` or `size` for this.

Once a role carries `elasticEp`, the profile is fixed for that deployment and the admission webhook
enforces the rest:

- the deployment has exactly one role, and it is a Server role;
- `replicas` and `size` are both 1, because each member is exactly one Pod;
- every member asks for one whole unsliced GPU;
- tensor and pipeline parallel sizes stay 1;
- you cannot supply `command`, because the renderer owns the command line of the managed role;
- there is no initial fabric to declare.

Adding or removing the profile on an existing role is refused. `headInstanceType` is also immutable
after creation, because the head is part of the deployment's identity.

Only the master serves HTTP. The other members join the Ray cluster and hold collective state. That
is why the master is counted inside `width` and is never retired.

## Requirements

The engine image must be a vLLM 0.29.0 image that already contains a full Ray installation. The
official vLLM image of that version ships without Ray, and the profile will not start on it. The
pinned requirement in the resize spec is Ray 2.56.1 with the full default closure.

The Ray control plane needs its own Pod, which is why `headInstanceType` is required. It names an
InstanceType that must already exist and must be CPU-only.

The profile changes the number of GPU members. It does not resize the underlying nodes or claim
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
[Model Delivery](../model-delivery/_index.md).

## Changing the width

Change only `width`. Starting at 2, these two patches request 4 and then 2.
Wait for the first resize to complete before submitting the second.

```bash
kubectl patch modeldeployment qwen3-ep -n gpustack --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/elasticEp/width","value":4}]'
# After the observations confirm completion:
kubectl patch modeldeployment qwen3-ep -n gpustack --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/elasticEp/width","value":2}]'
```

The operator keeps the master. It keeps the master's command, its UID and its bootstrap width
annotation, so the running instance is not replaced. It grows or shrinks the Ray worker members
around that master.

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

## Interruption scope

Narrowing a width first withdraws the master's API and master eligibility through the same writer,
then confirms the serving side withdrew, and only then asks the engine to shrink. Pods stay alive
through the engine's prepare, drain and commit phases. A member is removed only after its captured
identity is proven to hold no actor, and a same-name replacement is never removed in a captured
member's place.

The scope of the interruption is the deployment you patched. It does not extend to a separate
deployment. A second deployment keeps serving its own traffic while the first is resizing, but only
requests actually observed on the second deployment's own endpoint are evidence of that. The
number of deployments present says nothing about routing.

## Diagnosis

1. Read the deployment status and conditions for admission, startup and serving failures.
2. Read the deployment-owned observation ConfigMap. Its `reason` names an elastic hold.
   A layer with `Known` false carries its own `Reason`. Check the observed generation against the deployment.
3. Compare the five layers. A width that never became effective, with Ray holding the target width,
   points at the engine. An admitted count that never moved points at quota or scheduling. A Ray
   count that never moved points at members that have not joined.
4. On a hold after an ambiguous send, do not resend. Confirm the engine's own reported width first.

## Limits

- The engine must be a vLLM 0.29.0 image with a full Ray installation supplied by you.
- Width is bounded to 2 through 64.
- The profile cannot be added to or removed from an existing role, and `headInstanceType` cannot
  change.
- There is one serving instance per deployment. `replicas` and `size` are not a width mechanism.
- Only the master serves HTTP.
- No RDMA fabric is requested for this profile.
- Scale-down requires an owned Router with a readable serving view. A routerless deployment holds before narrowing.

**See also** — [Model Deployment Configuration](deployment.md) for the role contract ·
[Prefill and Decode](prefill-decode.md) for the other way to split a deployment ·
[Routing](routing.md) for how replicas are selected and observed ·
[Shutdown](shutdown.md) for retirement and drain ·
[Model Deployment Status](status.md) for the published fields.

**Next** → [Model Deployment Configuration](deployment.md)
