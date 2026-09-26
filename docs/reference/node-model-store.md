# Node Model Store Reference

> **Purpose** — the `NodeModelStore` resource and the `model-manager` node plugin behind it: what
> each field means and who writes it, when a mount is allowed, how content is downloaded, verified
> and published, why an attempt fails, and what the plugin measures.
> **Audience** operators, contributors · **Prerequisites** [Model Artifact
> Reference](model-artifact.md) · **Read time** ~12 min

Node delivery replaces an engine's own download with a node-local cache. The `model-manager` plugin
runs on every node as a CSI node plugin serving inline ephemeral volumes of the driver
`model.csi.gpustack.ai`. On the first mount of a digest on a node it downloads the files, verifies
every byte, publishes the tree in one step and bind-mounts it read-only; every later mount of that
digest on that node is one `stat` and a bind mount.

Enabling it and operating it are on [Model Store Operations](../operation/model-store.md).

## Contents

- [The resource](#the-resource)
- [Who writes what](#who-writes-what)
- [The volume a consumer mounts](#the-volume-a-consumer-mounts)
- [Mount authorization](#mount-authorization)
- [Materialization](#materialization)
- [Failure reasons](#failure-reasons)
- [References, restart and collection](#references-restart-and-collection)
- [Metrics](#metrics)
- [Requirements and limits](#requirements-and-limits)

## The resource

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: NodeModelStore                         # cluster-scoped, short name nms, category gpustack
metadata:
  name: gpu-node-01                          # the Node's name
  ownerReferences: [{apiVersion: v1, kind: Node, name: gpu-node-01}]
spec:                                        # the worker: this node's effective configuration
  watermarks: {highPercent: 80, lowPercent: 70}
  download: {concurrency: 8, bytesPerSecond: 0}      # 0 is unlimited
  hub:
    huggingFaceEndpoint: https://huggingface.co
    httpsProxy: ""
    noProxy: ""
    caBundleConfigMap: ""                    # a ConfigMap in the operator namespace, key ca.crt
  kubelet:                                   # the node's effective kubelet thresholds, from its configz
    nodefsAvailable: "10%"                   # evictionHard["nodefs.available"]; empty = kubelet sets none
    imagefsAvailable: ""                     # evictionHard["imagefs.available"]
    imageGCHighThresholdPercent: 85
status:                                      # the plugin on that node: its facts
  observedGeneration: 3                      # the spec generation the plugin applies
  capacity:
    totalBytes: 999641755648                 # the cache filesystem's size
    storedBytes: 15231233024                 # published trees and partial downloads
    usedPercent: 40                          # the filesystem's usage, rounded down to a multiple of 5
  models:                                    # keyed by digest, at most 256 entries
    - digest: sha256:0f3c...
      state: Ready                           # Downloading, Ready or Failed
      sizeBytes: 15231233024
      referenced: true                       # some Pod mounts it
      lastUsedTime: "2026-09-25T06:00:00Z"   # truncated to the hour
      reason: ""                             # Failed: see Failure reasons
      message: ""
      retryTime: null                        # Failed: the earliest next attempt
      source: Hub                            # where its bytes came from; none on older trees
    - digest: sha256:7a1e...
      state: Downloading
      sizeBytes: 145424101604                # set once the manifest is listed
      downloadedBytes: 61741236224           # what the attempt holds, a resume's checkpoints included
      source: Hub
  conditions:
    - {type: Ready, status: "True", reason: Serving}
    - {type: CapacityLow, status: "False", reason: WithinWatermarks}
```

`kubectl get nms` prints Ready, Used (`usedPercent`) and Age.

- **It never names a tenant.** It is cluster-scoped, so it carries a digest and sizes and never a
  namespace, an artifact, a Pod or a repository; those would leak across tenants. A failure's
  `message` says what its reason means and a detail such as the hub's HTTP status; the full error,
  with the file and the URL, is in the plugin's log.
- **`status` is rebuilt from the node**, never from the previous status: from what is on disk and
  what is mounted. A plugin restart rewrites it.
- **Download progress is written on thresholds.** The progress fields, `downloadedBytes` and,
  while a download runs, `storedBytes`, are written only once 30 seconds passed since the last write
  and some download moved by 5% of its size: at most twenty progress writes per download, and at
  most two a minute.
- **Everything else is written on change**, at once and with the current progress: an entry's state,
  `referenced`, the hour of `lastUsedTime`, `usedPercent`, a condition, `observedGeneration`. The
  plugin's own writes never trigger its next report, and a quiet node writes nothing.
- **More content than 256 entries**: the referenced ones are kept, then the most recently used; the
  `Ready` message counts what was left out.

| Condition | Reason | Meaning |
| --- | --- | --- |
| `Ready=True` | `Serving` | the plugin serves mounts and applies `observedGeneration`; the message says when the high watermark is capped |
| `Ready=False` | `InvalidConfiguration` | `spec` or its CA ConfigMap fails the plugin's own check; no download starts and nothing is collected, not under the previous `spec` either; mounts of published content still work |
| `CapacityLow=True` | `NothingToRemove` | usage is above the high watermark and every tree on the filesystem is in use |
| `CapacityLow=False` | `WithinWatermarks` | collection can keep usage within the watermarks |

## Who writes what

| Part | Writer | Rule |
| --- | --- | --- |
| the object | worker | created when the node's `CSINode` lists `model.csi.gpustack.ai`; removed with the Node, and all of them while the `CSIDriver` object does not exist |
| `spec` | worker | the merge of the configuration layers, rewritten within a minute of a Setting change; never a value that fails its check |
| `spec.kubelet` | worker | the node's kubelet thresholds, read through `nodes/<node>/proxy/configz` when the object is written and every 30 minutes; a failed read keeps the last reading, and a node never read has none. The plugin gets no `nodes/proxy` access |
| `status` | the plugin on that node | admitted only through the status webhook below |

With `kubectl`, a write to the object names `v1alpha1` (`nodemodelstores.v1alpha1.worker.gpustack.ai`):
the group's preferred version is the [`v1` view](model-artifact-views.md#the-v1-views), which serves
reads and `delete` only. A deleted object is created again by the worker while its node runs the
plugin.

The worker does **not** delete an object when the driver leaves `CSINode`, because every plugin
restart unregisters it for a moment. A node the plugin no longer runs on keeps a stale object whose
`Ready` condition stops changing.

A validating webhook on `nodemodelstores/status`, `failurePolicy: Fail`, admits an update only
when every rule holds, and names the rule it refused by:

| Rule | Refusal message begins |
| --- | --- |
| the requester is the plugin's ServiceAccount (`--model-manager-service-account` of the worker) | `only the model-manager plugin writes a NodeModelStore's status` |
| its token is bound to a Pod: `authentication.kubernetes.io/pod-name` and `pod-uid` are in the request's extra | `the plugin's token must be bound to its Pod` |
| that Pod exists in the operator namespace with that UID and runs on the object's node | `the plugin writes only the status of the node its Pod runs on` |

On Kubernetes 1.30 and later the token also carries `authentication.kubernetes.io/node-name`, and the
node comparison reads it without looking the Pod up. The guarantee is the same on 1.29.

`ModelArtifact` status has the same kind of guard: only the worker's own identity may update
`modelartifacts/status` (refusal: `only the worker writes a ModelArtifact's status`). The worker
learns it at startup with a `SelfSubjectReview`, or, where the API server does not serve one (before
1.28), with a `TokenReview` of its own token; with neither it does not start. Mount authorization trusts that status, so the guard is what keeps a
tenant from making an artifact look resolved.

## The volume a consumer mounts

A `ModelDeployment` under Node delivery and an `Instance` naming a Hugging Face artifact render:

```yaml
volumes:
  - name: gpustack-model
    csi:
      driver: model.csi.gpustack.ai
      readOnly: true
      volumeAttributes:                      # hints; the plugin checks each against the API
        artifact: qwen-7b
        artifactUID: 3f0c...
        manifestDigest: sha256:0f3c...
      nodePublishSecretRef: {name: hf-token} # the artifact's secretRef; absent without one
```

A hand-written Pod may mount the same volume; it is held to the same rules. The CSIDriver has
`attachRequired: false`, `podInfoOnMount: true`, `volumeLifecycleModes: [Ephemeral]` and
`fsGroupPolicy: None`, so the tree keeps the plugin's ownership and is readable by any user.

## Mount authorization

On every mount, before touching the disk, the plugin requires all of:

1. an inline ephemeral volume (`csi.storage.k8s.io/ephemeral=true`) with a mount capability and a
   target under the kubelet directory;
2. a `ModelArtifact` named `artifact` in the Pod's namespace, `csi.storage.k8s.io/pod.namespace`;
3. its UID equal to `artifactUID`, a Hugging Face source, `Resolved=True`, and
   `status.resolved.manifestDigest` equal to `manifestDigest`.

The namespace is the one kubelet adds, and kubelet writes its Pod keys over the Pod's own attributes,
so a tenant who sets `csi.storage.k8s.io/pod.namespace` is still judged in their own namespace.
**The digest is never authorization**: content already on the node is refused to a namespace that
has no resolved artifact naming it.

A refusal is `PermissionDenied` with the failed rule in the message, which kubelet records on the Pod
as a `FailedMount` event. An artifact that stops being resolved stops new mounts only; mounted Pods
keep theirs. Artifacts are read from an informer, and mounts answer `Unavailable` until it has
synced.

## Materialization

The first authorized mount of a digest the node does not hold starts one background attempt and
returns `Aborted` with the progress so far (`materializing sha256:0f3c…: 2.1 GiB of 15.2 GiB
received`). kubelet retries with its backoff, 0.5 s doubling to at most 2 min 2 s, and the first
call after publication mounts. After a download completes, a Pod starts at kubelet's next retry, up
to about two minutes later.

1. **Manifest.** The tree at `status.resolved.revision` is listed with the mount's credential and
   filtered by the artifact's patterns; its canonical digest must equal `manifestDigest`.
2. **Capacity.** The rest of the manifest's size, together with what every other running download
   has yet to write, is reserved against the high watermark, collecting first when it does not fit;
   two downloads that each fit and together do not are never both admitted. The reservation is held
   until the attempt ends.
3. **Download.** From `{endpoint}/{repository}/resolve/{commit}/{path}`, redirects followed, through
   `spec.hub`'s proxy and CA. At most `download.concurrency` requests run on the node across every
   download, under one `bytesPerSecond` limit, and large files are fetched as parallel byte ranges.
   A range that makes no progress for 30 seconds is retried from its last byte.
4. **Verification while downloading.** Each file is hashed in byte order as it arrives, `sha256` for
   an LFS file and the git blob SHA-1 otherwise, and its size must match. Nothing is read back from
   disk to verify, and a resumed file continues from a saved hash state.
5. **Publication.** Every file and a marker naming the digest are synced, then the directory is
   renamed to its published name in one step. A tree without its marker is never mounted.

Concurrent mounts of one digest join the running attempt. A credential is used only for the
repository of the artifact that presented it, kept in memory for the attempt, never written
anywhere, and never sent on a redirect to another host or from `https` to `http`.

**Backoff.** A failed attempt puts the digest in `Failed` with a `retryTime`: one minute after the
first failure, doubling to one hour, reset by a success and kept across a plugin restart. Until then
mounts of it return at once, `Unavailable`, without downloading. `AccessDenied` backs off too; the
next call brings the Secret again. `InvalidRequest` and `Canceled` do not back off: the next mount
starts again at once.

**Cancellation.** An attempt no mount has asked for in five minutes is canceled; its partial files
stay for a resume.

## Failure reasons

| Reason | Meaning | What happens |
| --- | --- | --- |
| `InvalidRequest` | the configuration cannot be executed: an invalid endpoint, proxy or CA | waits for the configuration to change |
| `AccessDenied` | the Hub refused the credential | backoff |
| `SourceUnavailable` | the Hub is unreachable, answers 5xx or 429, or a file's ranges keep failing | backoff |
| `IntegrityMismatch` | a file's hash or size, or the manifest's digest, does not match | the file is discarded; backoff |
| `InsufficientCapacity` | the reservation does not fit under the high watermark after collection | backoff |
| `Canceled` | no mount asked for the digest for five minutes | resumes on the next mount |

The mount's gRPC code is `InvalidArgument` for a malformed request, `PermissionDenied` for
authorization, `Unavailable` before the informer syncs and during backoff, `Aborted` while
materializing, `ResourceExhausted` for capacity and `Internal` otherwise. No message, event or log
line carries a token, an `Authorization` header or a signed URL.

## References, restart and collection

A reference is a mounted target. A mount records its reference before it binds, under the lock
collection removes trees under, so a tree is never removed between the check that it is published
and the mount; a reference that cannot be written fails the mount. Unmounting works from the target
path alone and succeeds for a target it never mounted.

On start the plugin rebuilds its references from `/proc/self/mountinfo` and its ledger, removes
partial directories that belong to no attempt, and rewrites `status`. A mounted Pod keeps running
while the plugin restarts or rolls: its mount is a kernel bind mount.

Collection runs when usage passes the high watermark, when a reservation does not fit, and every
five minutes. It removes, oldest `lastUsedTime` first and down to the low watermark:

- published trees no Pod references, unreferenced for at least ten minutes;
- partial downloads with no running attempt that no mount has asked for in 24 hours;
- failure records whose digest is neither published, partly on disk nor being materialized, 24 hours
  after the last failure, so `status.models` stops listing them.

It never removes a referenced tree or a partial being written, and it reads references from the
node's own mounts, never from the API. How the watermarks are capped when the cache shares kubelet's
filesystem is under [the capacity rule](../operation/model-store.md#the-capacity-rule).

When the references cannot be read, no configuration has been applied yet, or the node's `spec`
fails its check, a collection removes nothing, a stale partial included, and the `Ready` message
says `collection skipped` and why. A `spec` that fails after an earlier one applied does not leave
the earlier watermarks in force.

## Metrics

Served on the plugin's HTTPS port (`modelManager.securePort`, 32444), beside `/readyz` and `/livez`:

| Metric | Labels | Meaning |
| --- | --- | --- |
| `gpustack_model_manager_download_bytes_total` | `source="hub"` | bytes received |
| `gpustack_model_manager_mounts_total` | `result`: `hit`, `materialized`, `denied`, `pending` | mount calls |
| `gpustack_model_manager_materializations_total` | `result`: `published` or a failure reason | finished attempts |
| `gpustack_model_manager_publish_duration_seconds` | — | from an attempt's start to its publication |
| `gpustack_model_manager_gc_removed_bytes_total` | — | bytes collection removed |
| `gpustack_model_manager_stored_bytes`, `gpustack_model_manager_capacity_bytes` | — | the cache's bytes and its filesystem's size |

No label carries a namespace, an artifact, a repository or a Pod.

The same port serves `GET /model/downloads`: the running downloads, each a `digest`,
`downloadedBytes`, `sizeBytes` and `source`. The ModelArtifact
[progress](model-artifact-views.md#the-progress-subresource) subresource reads it for live bytes
between the status thresholds.

## Requirements and limits

- **Kubernetes 1.29** for node delivery, the floor the ModelArtifact reference states, though the
  chart itself admits older clusters. The status guard needs the Pod extras bound service-account
  tokens carry since 1.22, and the worker learns its own identity with a `SelfSubjectReview` (GA in
  1.28) or, before that, a `TokenReview`.
- **A Hugging Face source only.** A claim artifact is always mounted directly.
- **kubelet's configz**, served while its debugging handlers are enabled (the default). Without it
  a node has no `spec.kubelet` and its cap assumes kubelet's defaults, which the `Ready` message
  says.
- **No placement preference.** Node delivery schedules Pods as before; a node without the digest
  downloads it.
- **Every download comes from the Hub**, directly or through the proxy; nodes do not fetch from each
  other.

---

**See also** — [Model Artifact Reference](model-artifact.md) for the artifact and its other
deliveries · [Model Artifact Views Reference](model-artifact-views.md) for the `v1` views and
`progress` · [Model Store Operations](../operation/model-store.md) for enabling, configuring and
upgrading · [Settings](../settings.md#online-adjustable-settings) for the Settings `spec` is built
from.

**Next** → [Model Store Operations](../operation/model-store.md)
