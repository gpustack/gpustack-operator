# Model Artifact Reference

> **Purpose** — how a `ModelArtifact` names a model's weights, how the operator resolves and
> revalidates it, and how a `ModelDeployment` or an `Instance` consumes it.
> **Audience** users, operators · **Prerequisites** [Model Deployment
> Reference](model-deployment.md) · **Read time** ~16 min

A `ModelArtifact` is the one object that says where a model's weights come from and which credential
reads them. A `ModelDeployment` names it with `spec.model.artifactRef`, an `Instance` with a `model`
volume, and neither carries a URI, a revision or a token of its own.

## Contents

- [The resource](#the-resource)
- [Resolution and revalidation](#resolution-and-revalidation)
- [The manifest digest](#the-manifest-digest)
- [Referencing it from a ModelDeployment](#referencing-it-from-a-modeldeployment)
- [Engine delivery](#engine-delivery)
- [Claim delivery and placement](#claim-delivery-and-placement)
- [The KV reuse domain](#the-kv-reuse-domain)
- [Status](#status)
- [Instance model volumes](#instance-model-volumes)
- [Requirements and limits](#requirements-and-limits)

## The resource

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact                      # namespaced, short name mart
metadata:
  name: qwen-7b
  namespace: team-a
spec:                                    # immutable after creation
  source:                                # exactly one member
    huggingFace:
      repository: Qwen/Qwen2.5-7B-Instruct
      revision: main                     # branch, tag or commit; defaults to main
      secretRef: {name: hf-token}        # optional; this namespace; key "token"
    # persistentVolumeClaim:
    #   claimName: models                # this namespace
    #   path: qwen                       # directory inside the volume; empty is the root
  allowPatterns: ["*.safetensors", "*.json", "tokenizer*"]   # optional; Hugging Face only
  ignorePatterns: ["original/"]                              # optional; wins over allowPatterns
status:
  resolved:
    revision: 7ae557604adf67be50417f59c2c2f167def9a775
    manifestDigest: sha256:669ed7b128b6ad1658d735326bd172a33497ecdb8bbd72dd0b23c98b58469448
    fileCount: 10
    sizeBytes: 999604126
  nodes:                                 # Hugging Face only; where the content is across nodes
    ready: 3
    downloading: 1
    failed: 0
    downloadingPercent: 45               # the downloading nodes' mean, in steps of 5
  conditions:
    - {type: Resolved, status: "True", reason: Resolved}
    - {type: Degraded, status: "False", reason: Healthy}
```

- **The spec is immutable.** An artifact is an identity: other weights, or another revision, are a
  new artifact. That is also what lets a deployment's frozen reference pin anything.
- **The Secret and the claim need not exist yet.** Admission does not read them; their absence is a
  reason in status (`SecretNotFound`, `ClaimNotFound`).
- **A `modelScope` member exists and is refused.** Opening it needs branch resolution checked
  against git, a listing that recovers from the API's silent truncation at 3000 entries, and a vLLM
  runner whose ModelScope SDK accepts a commit (1.39.1 or later).
- **Patterns select the files.** They follow Python's `fnmatch.fnmatchcase`: case-sensitive, `*`
  and `?` cross `/`, a trailing `/` means everything under it, an empty allow list keeps every file,
  and an ignored file is dropped even when allowed. At most 32 per list, 1 to 256 characters each,
  refused on a claim source. A filter that keeps no file is `Resolved=False`, `EmptyManifest`. A
  filtered artifact needs [Node delivery](#referencing-it-from-a-modeldeployment).
- **`status.nodes` counts the content, not the artifact.** Nodes whose `NodeModelStore` lists the
  digest `Ready`, `Downloading` or `Failed`; artifacts with the same digest see the same nodes, and
  only numbers cross namespaces. The mean covers the downloading nodes only, each a whole copy. It is
  written when a count changes or the mean reaches another step, at most every 30 seconds. The
  `v1` view's [progress](model-artifact-views.md#the-progress-subresource) answers the same at full
  precision.
- **Deletion waits for the last reference.** The finalizer `worker.gpustack.ai/model-artifact-protection`
  holds a referenced artifact in `Terminating` until no `ModelDeployment` or `Instance` in the
  namespace names it. Running Pods are never affected.

## Resolution and revalidation

A Hugging Face source is resolved **once**, with the namespace's own token: the revision becomes a
40-character commit through `/api/models/<repo>/revision/<rev>`, which peels an annotated tag, and
every page of `/api/models/<repo>/tree/<commit>?recursive=true` becomes the manifest. Nothing follows
the branch afterwards.

| The Hub answers | `Resolved` reason |
| --- | --- |
| 404 with `X-Error-Code: RevisionNotFound` | `RevisionNotFound` |
| 401, 403, 404 `RepoNotFound`, `GatedRepo`, or a tree whose LFS digests are masked with `*` | `AccessDenied` |
| 5xx, 429, a network error | `SourceUnavailable` |

A repository that does not exist and a private one the token cannot read answer alike, so the
message says "does not exist or is not accessible". A gated repository answers 200 on the revision
and tree endpoints and only masks its digests, which is why the masked tree is its own row.

**A mistyped token is silent on a public repository**: the Hub answers as if no token had been sent.
The controller therefore checks each new token with `/api/whoami-v2` and emits a Warning event
`InvalidToken` when the Hub rejects it; `Resolved` is unchanged.

**Access is revalidated** every `model-artifact-revalidate-interval` (default `24h`) and whenever the
Secret changes, with one `HEAD` of a file at the resolved commit, redirects not followed:

- a refusal sets `Degraded=True` and is checked again a minute later; the same refusal then sets
  `Resolved=False`;
- `SourceUnavailable` sets only `Degraded` and is retried every minute, so a Hub outage never
  revokes pinned weights; an unreadable Secret is treated the same way;
- a deleted Secret, or one without its `token` key, sets `Resolved=False` at once;
- a later pass restores `Resolved=True`, with the commit and digest unchanged.

`Resolved=False` stops **new** consumption: no new replica, replacement or scale-up. It never
deletes a running Pod. A claim source is resolved by the claim existing; the operator never reads
its content, so it has no revision and no digest.

## The manifest digest

The digest is the content address of a Hugging Face artifact: the SHA-256 of a canonical manifest,
one line per file of the commit. The format is `gpustack-manifest v1`:

```text
gpustack-manifest v1
gitsha1:4ff64fe5… 807 config.json
sha256:8111d5af… 453864 model.safetensors
```

- A line is `<algorithm>:<hex> <size> <path>`, sorted by the path's UTF-8 bytes, every line ending
  in LF. An LFS file uses its `sha256`, any other file its git blob `gitsha1`.
- A path must be valid UTF-8, relative, with no control character and no empty, `.` or `..`
  segment; the whole resolution fails on one that is not.
- The source, the repository, the commit and the patterns are **not** part of it, so the same files
  have the same digest wherever they live. A filter changes the digest only through the files it
  keeps, and an artifact without patterns has the digest it always had. The reference
  implementation is `pkg/modelartifact`.

Two consequences to keep in mind:

- **The digest is never evidence of access.** A public and a private repository holding the same
  files have the same digest; authorization always comes from resolving with the namespace's token.
- The same files on Hugging Face and ModelScope have different digests, because non-LFS files are
  hashed differently there. Content is not deduplicated across hubs.

## Referencing it from a ModelDeployment

```yaml
spec:
  model:
    name: qwen-7b                        # the served name, unchanged
    artifactRef: {name: qwen-7b}         # a ModelArtifact in this namespace
```

`artifactRef` is frozen with the rest of `spec.model`: other weights are another deployment. A
reference to an artifact that does not exist or has not resolved is admitted, and the deployment
creates no Pod until it resolves.

A claim source is always mounted directly. A Hugging Face source takes the delivery the
`model-artifact-delivery-mode` Setting names, `Engine` by default and `Node` where the chart deploys
the node plugin ([switching it](../operation/model-store.md#switch-delivery) rolls each such
deployment once):

| | Claim source (`Pvc`) | Hugging Face, `Engine` | Hugging Face, `Node` |
| --- | --- | --- | --- |
| Weights | the claim, read-only, at `/var/lib/gpustack/model`, `subPath` = `path` | downloaded by the engine into `/var/lib/gpustack/model-cache` | the node's verified copy, read-only, at `/var/lib/gpustack/model` |
| vLLM | `vllm serve /var/lib/gpustack/model` | `vllm serve <repository> --revision <commit>` | as a claim |
| SGLang | `--model-path /var/lib/gpustack/model` | `--model-path <repository> --revision <commit>` | as a claim |
| Both | `--served-model-name <spec.model.name>`, unless the role states it | same | same |

`--revision` pins the weights and the tokenizer together on both engines. A take-over role (one
with `command`) gets the claim or node mount and nothing else, and nothing at all under `Engine`.

**Node delivery** mounts an inline CSI volume of the driver `model.csi.gpustack.ai`: the node's
`model-manager` plugin downloads the digest once per node, verifies every byte against the manifest
before anything is mounted, and mounts only for a resolved artifact in the Pod's own namespace. The
plugin and its resource are on the [Node Model Store Reference](node-model-store.md).

No Hub variable and no cache `emptyDir` is rendered under Node delivery, and the ephemeral-storage
limit is not raised: the volume's bytes are not the Pod's.

While `artifactRef` is set, admission refuses:

- on vLLM `--model`, `--revision`, `--tokenizer-revision` and `--download-dir`, on SGLang
  `--model-path`, `--revision` and `--download-dir`, and `HF_TOKEN`, `HF_ENDPOINT` and `HF_HOME` in
  `env`, whatever the source — a later value would silently replace the artifact's;
- a role volume at, inside or around `/var/lib/gpustack/model` or `/var/lib/gpustack/model-cache`.

On **every** managed role, with or without an artifact, `--served-model-name` must be exactly
`spec.model.name`. Any other name was measured to fail silently: requests by `spec.model.name`
answer 404 through the router, and requests by the other name succeed while the router's
prefix-cache scoring falls to zero, with the deployment reporting `Ready`.

## Engine delivery

Under `Engine`, the engine downloads the pinned commit itself, with:

| Variable | Value | Owned |
| --- | --- | --- |
| `HF_HOME` | `/var/lib/gpustack/model-cache` | yes |
| `HF_ENDPOINT` | the `model-artifact-huggingface-endpoint` Setting | yes |
| `HF_TOKEN` | a `secretKeyRef` to the artifact's Secret, key `token`; the value never enters the Pod spec | yes |
| `HTTPS_PROXY`, `NO_PROXY` | the proxy Settings, when set | no — a role's own value wins |

The cache is an `emptyDir` with a `sizeLimit` of the manifest's size plus a tenth, and at least
1 GiB more. The manifest is the whole commit, so it bounds whatever subset the engine downloads (vLLM
skips `.bin` files when `.safetensors` exist).

**The container's ephemeral-storage limit is raised by the same amount; its request is not.**
Kubelet counts an `emptyDir` toward the Pod's ephemeral-storage limit as well as its own
`sizeLimit`, so a model larger than the InstanceType's local storage would otherwise be evicted
mid-download. Kueue and the scheduler read the request, which stays the InstanceType's.

A namespace `LimitRange` whose maximum is below the raised limit makes the API server refuse the Pod,
which the deployment reports as a create failure.

A download that still outgrows the cache is evicted; the Pod ends `Succeeded`, and the deployment's
phase message carries the kubelet's own reason, which is the only place it survives.

- **Changing an endpoint or proxy Setting rolls every Engine-delivered deployment**, because the
  values are rendered into the Pods. It does not re-resolve an artifact. For the same reason the
  proxy Setting accepts no credentials ([Settings](../settings.md)).
- **The CA bundle Setting is not given to engine Pods.** They run in tenant namespaces, which cannot
  mount a ConfigMap from the worker's. Mount your own ConfigMap and set `REQUESTS_CA_BUNDLE`.
- A `trust_remote_code` model writes its code under `$HF_HOME/modules`, which is on the cache.

## Claim delivery and placement

Kueue's topology-aware scheduling does not read a Pod's volumes. A claim bound to a PV with node
affinity was measured to fail silently without help: the Pod was assigned elsewhere and stayed
Pending, while its Workload stayed Admitted holding quota. So:

| The claim | What the operator does |
| --- | --- |
| Bound | adds the PV's required node affinity to every Pod at creation, outside the Pod fingerprint |
| Pending, `WaitForFirstConsumer` with a provisioner | creates the Pods; the first one's node decides the binding |
| Pending, a class with `kubernetes.io/no-provisioner`, or immediate binding | creates nothing: `ClaimNotBound` |
| Mounted by more than one Pod, with neither `ReadOnlyMany` nor `ReadWriteMany` | creates nothing: `AccessModeConflict` |

With the affinity, the Workload waits before quota when the PV's node is full and admits itself when
room appears. For a static local volume, bind the claim to its PV first (`spec.volumeName`). The
static-class row is inferred from the binding order rather than measured.

Measured throughput, for choosing a claim (one node with one 48 GB accelerator and a 1000 GiB network
SSD boot disk; object-storage and NFS servers on CPU nodes with 2 TiB network disks of the default
class; page caches dropped before every read):

| Source | Cold read, 7B | Weight loading, vLLM 7B | Warm read |
| --- | --- | --- | --- |
| S3 CSI (geesefs) | 467 MB/s | 32.6 s | about 455 MB/s: no page-cache benefit |
| The node's boot disk | 479 MB/s | 34.8–35.7 s | 7.4–7.8 GB/s |
| NFS CSI | about 312 MB/s | 40.2 s | page-cached |

Both S3 and the boot disk sat at the network disk's ceiling, so the S3 CSI driver's own ceiling was
not reached; performance is not a reason to avoid an S3 claim. Every restart on the same node reads
object storage again. That disk class's throughput grows with its size, so any figure you quote
needs the disk's type and size. Point an S3 PV's endpoint at a ClusterIP, not an in-cluster DNS name.

## The KV reuse domain

Neither engine's store key names the weights: vLLM's carries the last path segment of `--model`,
SGLang's the served name. Two deployments of different commits under one Binding were measured to
read each other's KV blocks, and a claim delivery names every model `model`.

With `artifactRef` and `spec.kvCache`, the operator prefixes the keys with the weight identity:
vLLM's `kv_connector_extra_config.cache_prefix` on its store connector, and SGLang's
`extra_backend_tag` in `--hicache-storage-backend-extra-config`. The identity is `m-` and 32
hexadecimal digits of the manifest digest, or of the SHA-256 of the artifact's UID for a claim.

- Deployments of one identity share blocks; deployments of two never do.
- **A claim's identity is the artifact, not its content.** Replacing the files under one artifact
  keeps the identity, and new replicas would read blocks of the old files. Put new weights in a new
  `ModelArtifact`.
- The vLLM-Ascend store connector gets no prefix: its key layout was not read.
- The Binding's `blockSize` and `dtype` are not part of either key. The `dtype` is
  [handed to the engine](../kv-cache/pool.md#the-dtype-is-handed-to-the-engine) instead.

## Status

`status.model` echoes the artifact, its `revision` and `manifestDigest`, and the `delivery`, `Pvc`,
`Engine` or `Node`. `WeightsReady` says whether every engine role's weights are there:

| Status | Reason | Meaning |
| --- | --- | --- |
| True | `NotApplicable` | the deployment names no artifact |
| False | `ArtifactNotFound`, `ArtifactNotResolved` | no new Pod is created; the message carries the artifact's reason |
| False | `ClaimNotBound`, `AccessModeConflict` | no new Pod is created; see the placement table |
| False | `NodeDeliveryUnavailable` | `Node` delivery, and the CSIDriver `model.csi.gpustack.ai` does not exist; no new Pod is created |
| False | `FilterNeedsNodeDelivery` | an artifact with patterns under `Engine` delivery, which cannot honor them; no new Pod is created |
| False | `Materializing` | a node Pod is not mounted yet and its node lists the digest `Downloading` |
| False | `MaterializationFailed` | the same, and the node lists it `Failed`; the message carries the node's reason and retry time |
| False | `WeightsNotMounted` | a claim or node Pod's `PodReadyToStartContainers` is not True yet |
| False | `Downloading` | an engine Pod is not Ready yet; the engine reports no progress of its own |
| True | `Mounted`, `Downloaded` | every Pod has its weights |

While nothing is created, the phase message is the `WeightsReady` message. A blocked deployment
also holds its rollouts: an edited replica is not deleted while its replacement could not be made,
and `ReplicasUpToDate` reports that hold with reason `RolloutHeldByWeights`.

If Kueue's `waitForPodsReady` is enabled (the chart leaves it off), a download that outlasts its
timeout evicts and requeues the replica.

## Instance model volumes

```yaml
spec:
  additionalVolumes:
    - mountPath: /models/qwen
      model: {artifactRef: {name: qwen-7b}}
```

The volume is always read-only and takes no `subPath`. A claim artifact's `path` is the sub-path and
the claim placement rules above apply. A Hugging Face artifact is mounted through the node plugin
whatever `model-artifact-delivery-mode` says, since an Instance has no engine to download it; while
the CSIDriver does not exist the Instance creates no Pod and says so in its phase message.

## Requirements and limits

- **Kubernetes 1.29**, the floor the bundled Kueue already sets. `PodReadyToStartContainers` (beta,
  on by default since 1.29) feeds `WeightsReady`; with it off, a claim deployment's `WeightsReady`
  stays `WeightsNotMounted` while its replicas run.
- **`sglang-gateway` fetches a tokenizer by the worker's `model_path`.** With a claim that path is
  local, so it logs one 404 warning and routes by text; with Engine delivery it would fetch `main`
  without a token (not measured).
- **vLLM 0.29.0 needs `--enforce-eager` for InternLM2** with `trust_remote_code`, an engine defect.
- **Node delivery downloads from the Hub on every cold node.** It does not prefer nodes that hold
  the weights.
- Settings: [Settings & Environment Variables](../settings.md#online-adjustable-settings) carries the
  endpoint, proxy, no-proxy, CA bundle, revalidation interval, delivery mode and the node cache's
  watermarks and download limits.

---

**See also** — [Model Deployment Reference](model-deployment.md) for the rest of the deployment
contract · [Node Model Store Reference](node-model-store.md) for Node delivery ·
[Model Artifact Views Reference](model-artifact-views.md) for the `v1` view and `progress` · [KV Cache Injection Reference](kv-cache-injection.md) for the store connector this
prefixes · [Model Deployment Status Reference](model-deployment-status.md) for the other conditions.

**Next** → [Model Deployment Status Reference](model-deployment-status.md)
