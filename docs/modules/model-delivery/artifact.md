# Model Artifact

A `ModelArtifact` records a model's source, credentials, and verified identity once per namespace.
`ModelDeployment` references it via *`spec.model.artifactRef`*, and `Instance` mounts it via a
*`model`* volume, keeping URIs, revisions, and hub tokens out of workload specs.

## Contents

- [Resource](#resource)
- [Resolution and revalidation](#resolution-and-revalidation)
- [Manifest digest](#manifest-digest)
- [Workload consumption](#workload-consumption)
- [Engine delivery](#engine-delivery)
- [Claim delivery and placement](#claim-delivery-and-placement)
- [Image source](#image-source)
- [KV reuse domain](#kv-reuse-domain)
- [Status](#status)
- [Prerequisites and limitations](#prerequisites-and-limitations)

## Resource

Omit `status` when applying a `ModelArtifact` (status values below are illustrative). Set
*`expectedDigest`* only when anchoring a hub source to a known manifest digest:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelArtifact  # namespaced, short name mart
metadata:
  name: qwen-7b
  namespace: team-a
spec:  # immutable after creation
  source:  # required; set exactly one source
    huggingFace:
      repository: Qwen/Qwen2.5-7B-Instruct  # required
      revision: main  # optional, default: main (branch, tag, or commit)
      secretRef:  # optional Secret in this namespace; key: token
        name: hf-token
    # modelScope:
    #   repository: qwen/Qwen2.5-7B-Instruct
    #   revision: master  # optional, default: master
    #   secretRef:
    #     name: ms-token
    # persistentVolumeClaim:
    #   claimName: models  # PVC in this namespace
    #   path: qwen  # optional subdirectory; empty mounts volume root
    # image:
    #   reference: registry.example.com/team/qwen@sha256:669ed7b1...48  # digest-pinned OCI image
  allowPatterns:  # optional; hub sources only (1..32 entries, 1..256 chars each)
    - "*.safetensors"
    - "*.json"
    - "tokenizer*"
  ignorePatterns:  # optional; hub sources only, wins over allowPatterns
    - "original/"
  # expectedDigest: sha256:<64-lowercase-hex-characters>  # optional; hub sources only
status:
  resolved:
    revision: 7ae557604adf67be50417f59c2c2f167def9a775
    manifestDigest: sha256:669ed7b128b6ad1658d735326bd172a33497ecdb8bbd72dd0b23c98b58469448
    digestSource: Hub  # Hub or Expected
    fileCount: 10
    sizeBytes: 999604126
  nodes:  # hub sources only; NodeModelStore counts across cluster
    ready: 3
    downloading: 1
    failed: 0
    downloadingPercent: 45  # mean across downloading nodes, in 5% steps
  conditions:
    - type: Resolved
      status: "True"
      reason: Resolved
    - type: Degraded
      status: "False"
      reason: Healthy
```

- **Immutability and protection**: *`spec`* is immutable after creation. The
  `worker.gpustack.ai/model-artifact-protection` finalizer holds deletion while any same-namespace
  `ModelDeployment` or `Instance` references the artifact.
- **Filter matching**: Patterns use Python `fnmatch.fnmatchcase` (`*` and `?` cross `/`; trailing
  `/` matches subtree). Filters keeping zero files set `Resolved=False/EmptyManifest`, and filtered
  artifacts require `Node` delivery.
- **Missing references**: Referenced `Secret` or `PersistentVolumeClaim` objects need not exist at
  admission; missing objects report `SecretNotFound` or `ClaimNotFound` in *`status.conditions`*.

## Resolution and revalidation

Hub sources resolve **once** using the namespace's token, pinning branches or peeled tags to an
exact commit and building the file manifest:

- **Hugging Face**: Non-existent, private, or ungranted gated repositories (which mask file digests)
  all fail with `AccessDenied`. Missing refs fail with `RevisionNotFound`, and network/5xx/429
  errors fail with `SourceUnavailable`.
- **ModelScope**: Cross-checks resolved branches/tags against git refs (disagreement fails as
  `SourceUnavailable`), re-lists directories up to `3000` direct children (larger directories fail
  as `SourceUnavailable`), requires `sha256` per file, and emits an `InvalidToken` Warning Event
  when a token probe is rejected.

Access revalidates every `model-artifact-revalidate-interval` (default `24h`) and on `Secret` edits
via a single-file `HEAD` (redirects disabled):

- **Refusal staircase**: An access refusal sets `Degraded=True` first and `Resolved=False` if
  repeated `1m` later. Deleting the `Secret` or its `token` key sets `Resolved=False` immediately.
- **Outage resilience**: `SourceUnavailable` (or a transient `Secret` read error) sets only
  `Degraded=True` and retries every `1m` without revoking `Resolved=True`.
- **Expected digest anchor**: With *`expectedDigest`* (`sha256:<64-hex>`), resolution rejects
  mismatches (`DigestMismatch`) and revalidation re-lists the full tree. After two `SourceUnavailable`
  checks (`1m` apart) without a refusal, resolution sets `digestSource: Expected` (`Resolved=True`, no
  commit); `Engine` delivery is refused (`AnchorNeedsNodeDelivery`) while `Node` delivery pulls from
  peers. Never treat digests as secret (`NodeModelStore` digests are cluster-visible).

`Resolved=False` blocks new `Pod` creation, replacements, and scale-out without terminating running
`Pod`s. PVC sources resolve once the claim exists (no revision or digest).

## Manifest digest

For hub sources, *`manifestDigest`* is the SHA-256 of a canonical `gpustack-manifest v1` document
with one LF-terminated `<algorithm>:<hex> <size> <path>` line per sorted file (`sha256` for LFS and
all ModelScope files, `gitsha1` for non-LFS Hugging Face git blobs):

```text
gpustack-manifest v1
gitsha1:4ff64fe5... 807 config.json
sha256:8111d5af... 453864 model.safetensors
```

- **Authorization is per namespace**: Identical public and private repositories produce the same
  digest; mounting still requires `Resolved=True` in the consumer's namespace.
- **No cross-hub deduplication**: Non-LFS files hash differently on Hugging Face (`gitsha1`) and
  ModelScope (`sha256`), yielding distinct digests.

## Workload consumption

### ModelDeployment reference

Set *`spec.model.artifactRef`* when creating a `ModelDeployment` (*`spec.model`* is immutable):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: ModelDeployment
 metadata:
   name: qwen-7b-serving
 spec:
   model:
     name: qwen-7b  # served model name
+    artifactRef:
+      name: qwen-7b  # ModelArtifact in this namespace
```

Hub sources follow the `model-artifact-delivery-mode` `Setting` (`Engine` default; `Node` when the
chart enables `modelManager`, rolling hub deployments once on switch). PVC and OCI image sources
always mount directly:

| Aspect | PVC (`Pvc`) | Hub (`Engine`) | Hub (`Node`) | OCI image (`Image`) |
|---|---|---|---|---|
| Mount / cache | Read-only PVC at `/var/lib/gpustack/model` (*`subPath`* = *`path`*) | Engine downloads into `/var/lib/gpustack/model-cache` | Read-only CSI `model.csi.gpustack.ai` at `/var/lib/gpustack/model` | Read-only image volume at `/var/lib/gpustack/model` |
| Engine argv | `/var/lib/gpustack/model` | `<repository> --revision <commit>` | `/var/lib/gpustack/model` | `/var/lib/gpustack/model` |
| Served name | `--served-model-name <spec.model.name>` | Same | Same | Same |

Take-over roles (*`roles[].command`* set) receive PVC, `Node`, or `Image` volume mounts without argv
changes, and receive nothing under `Engine`. While *`artifactRef`* is set, admission refuses manual
model/revision/download flags, `HF_*` or `MODELSCOPE_*` variables, and custom mounts overlapping
`/var/lib/gpustack/model` or `/var/lib/gpustack/model-cache`. Keep `--served-model-name` equal to
*`spec.model.name`* so router routing and prefix scoring match.

### Instance model volumes

Mount a `ModelArtifact` into an `Instance` at creation or while stopped
([Additional volumes](../instances/configuration.md#additional-volumes) and
[Service lifecycle](../instances/operations.md#service-lifecycle)):

```diff
 apiVersion: worker.gpustack.ai/v1
 kind: Instance
 metadata:
   name: workspace-demo
 spec:
+  additionalVolumes:
+    - mountPath: /models/qwen
+      model:
+        artifactRef:
+          name: qwen-7b
```

The mount is always read-only and forbids *`subPath`*. Hub artifacts always use `Node` CSI delivery
on `Instance`s (waiting in `Starting` if `model.csi.gpustack.ai` is absent), PVC artifacts follow
claim placement, and OCI image artifacts use Kubernetes image volumes.

## Engine delivery

Under `Engine` delivery, the engine downloads the pinned commit into an `emptyDir` sized to
`max(sizeBytes * 1.1, sizeBytes + 1 GiB)`, raising the container's `ephemeral-storage` **limit**
(not `request`) by the same amount:

- **Injected environment**: Hugging Face sets owned `HF_HOME`, `HF_ENDPOINT`, and `HF_TOKEN`
  (`secretKeyRef` `token`). ModelScope sets owned `MODELSCOPE_CACHE`, `MODELSCOPE_DOMAIN` (bare
  host), `MODELSCOPE_API_TOKEN`, and `VLLM_USE_MODELSCOPE=true` / `SGLANG_USE_MODELSCOPE=true`.
  Defaulted `HTTPS_PROXY` and `NO_PROXY` yield to user *`roles[].env`*.
- **Rollouts and CA bundles**: Editing hub endpoint or proxy `Setting`s rolls `Engine`-delivered
  deployments (`model-artifact-ca-bundle` is not mounted into tenant namespaces; mount a tenant
  `ConfigMap` and set `REQUESTS_CA_BUNDLE` instead). Ensure any namespace `LimitRange` allows the
  raised `ephemeral-storage` limit.

## Claim delivery and placement

Because Kueue *topology-aware scheduling (TAS)* ignores `Pod` volumes, the operator aligns `Pod`
creation with PVC binding and node affinity:

| Claim state | Operator behavior |
|---|---|
| `Bound` | Injects the PV's required node affinity into every `Pod` at creation. |
| `Pending` (`WaitForFirstConsumer` with provisioner) | Creates `Pod`s so the first scheduled `Pod` binds the volume. |
| `Pending` (`no-provisioner` or `Immediate`) | Holds `Pod` creation with `ClaimNotBound` (pre-bind static local PVs via *`spec.volumeName`*). |
| Multi-`Pod` mount without `ROX` or `RWX` | Holds `Pod` creation with `AccessModeConflict`. |

On a single-accelerator node with cold page caches (`1000 GiB` boot SSD; `2 TiB` network disks), a
7B model loaded in `32.6 s` over S3 CSI (`geesefs`, `467 MB/s` cold, `~455 MB/s` warm without page
cache), `34.8–35.7 s` from boot disk (`479 MB/s` cold, `7.4–7.8 GB/s` warm), and `40.2 s` over NFS
CSI (`~312 MB/s` cold).

Point `geesefs` S3 PV endpoints at a ClusterIP instead of `*.svc.cluster.local`. Host `systemd`
mounts resolve names through the host DNS resolver rather than cluster DNS.

## Image source

Package weights into an OCI image pinned by manifest digest (`@sha256:...`). Mutable tags, file
patterns, and *`expectedDigest`* are refused at admission; *`status.resolved`* records only
*`resolvedTime`*.

```yaml
apiVersion: worker.gpustack.ai/v1
kind: ModelArtifact
metadata:
  name: qwen-image
  namespace: team-a
spec:
  source:
    image:
      reference: registry.example.com/team/qwen@sha256:669ed7b128b6ad1658d735326bd172a33497ecdb8bbd72dd0b23c98b58469448
```

### Image build and delivery

Place weight files at the **image root** (no `subPath` is supported). Verified recipe (measured
SHA-256 identical to LFS blobs on Kubernetes `1.35.7` / containerd `2.2.6`):

```bash
for f in *.safetensors tokenizer.json tokenizer_config.json config.json generation_config.json; do
  oras push "$IMAGE" --artifact-type application/vnd.gpustack.weights "$f:application/octet-stream"
done
```

Image sources always deliver as read-only Kubernetes `image` volumes at `/var/lib/gpustack/model`
without `model.csi.gpustack.ai` or `ephemeral-storage` adjustments. Pull errors surface on `Pod`
events while `WeightsReady` stays `False/WeightsNotMounted`. Node-pinned `Instance`s verify node
kubelet and containerd floors before creating a `Pod`.

### Image prerequisites

| Component | Minimum version | Reason |
|---|---|---|
| `kube-apiserver` | `1.35` | `ImageVolume` is beta on by default in `1.35` (GA in `1.36`); artifact creation is refused below `1.35`. |
| `kubelet` | `1.35` | Required to mount `image` volumes on target `Node`s. |
| `containerd` | `2.1` | Required runtime for `image` volumes (`subPath` would require `2.2`, so root-only mounts are used). |

Pod Security Admission `Restricted` permits `image` volumes on Kubernetes `>= 1.33`
(`kubernetes#130394`). Private registries use the workload's existing *`imagePullSecrets`*, and
standard containerd mirrors (Harbor, Spegel, Dragonfly) work transparently (overlapping layer
digests pulled in `112 ms` / `11.8 MB` versus `2m10s` / `7.89 GB` cold).

### Disk cost and GC

With containerd's default `discard_unpacked_layers = false`, a pulled model image stores both
compressed content blobs and unpacked snapshots (kubelet CRI image size reports only the compressed
blobs, undercounting a `15.24 GB` model by `~2.26×`):

| Layer format | Content store | Snapshots | Total disk | Ratio to raw |
|---|---|---|---|---|
| Gzip-compressed | `12.07 GB` | `15.24 GB` | `~27.3 GB` | **`1.79×`** |
| Uncompressed | `15.24 GB` | `15.24 GB` | `~30.5 GB` | **`2.00×`** |

Verified kubelet image GC behavior (`1.35.7` / `2.2.6`):

- **Protected while mounted**: Running containers protect mounted model images (`kubelet >= 1.31`,
  `containerd >= 2.1`); completed `Pod`s do not.
- **Collected after scale-to-zero**: Once the last `Pod` exits, the next `5m` GC pass can remove the
  image (`imageMinimumGCAge` checks first-detected time, not release time; measured `4m2s` to GC and
  `12m43s` cold re-pull at `~20 MB/s` on a 2-vCPU `Node`).
- **High-threshold cascade**: Pushing disk usage past *`imageGCHighThresholdPercent`* (`85%`
  default) triggers GC across all unused images on the `Node` (`18` unrelated images collected in
  testing).

## KV reuse domain

When both *`artifactRef`* and *`spec.kvCache`* are set (outside vLLM-Ascend), the operator prefixes
KV store keys (`cache_prefix` on vLLM, `extra_backend_tag` on SGLang) with `m-` plus 32 hex chars of
*`manifestDigest`* (or the SHA-256 of the artifact UID for PVC and OCI image sources).

- **PVC and image identity**: Replacing files inside an existing PVC keeps the same artifact UID and
  KV prefix; create a new `ModelArtifact` when changing PVC weights. Two `ModelArtifact`s naming the
  same OCI image have distinct UIDs and do not share KV blocks.

## Status

On `ModelDeployment`, *`status.model`* echoes the artifact delivery mode (`PVC`, `Engine`, `Node`,
or `Image`) and `WeightsReady` summarizes weight availability (holding rollouts with
`ReplicasUpToDate=False/RolloutHeldByWeights` when new `Pod`s are blocked):

| `WeightsReady` | Reason | Action |
|---|---|---|
| `True` | `NotApplicable`, `Mounted`, `Downloaded` | Healthy: no artifact configured, or all `Pod`s have weights. |
| `False` | `ArtifactNotFound`, `ArtifactNotResolved` | Create the `ModelArtifact` or resolve its hub/secret/claim error. |
| `False` | `ClaimNotBound`, `AccessModeConflict` | Bind the PVC or use `ReadOnlyMany` / `ReadWriteMany` for multi-`Pod` mounts. |
| `False` | `NodeDeliveryUnavailable` | Enable `modelManager` (`model.csi.gpustack.ai` `CSIDriver`) or switch delivery mode. |
| `False` | `FilterNeedsNodeDelivery`, `AnchorNeedsNodeDelivery` | Switch `model-artifact-delivery-mode` to `Node` for patterned or `expectedDigest` artifacts. |
| `False` | `Materializing`, `Downloading`, `WeightsNotMounted` | Wait for node cache download, engine download, or image pull; inspect `Pod` events if stalled. |
| `False` | `MaterializationFailed` | Read the `NodeModelStore` failure reason and *`retryTime`* ([Model Store Operations](operations.md#troubleshooting)). |

### Progress subresource

Query on-demand download progress across up to 64 downloading `Node`s (`13 s` timeout, node names
omitted for tenant safety; administrators inspect `kubectl get nms` directly):

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/modelartifacts/<name>/progress"
```

**Required RBAC:** Callers need `get` on `modelartifacts/progress` in `worker.gpustack.ai` (`get` on
`modelartifacts` alone is insufficient, and the chart grants no tenant RBAC by default):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: model-artifact-reader
  namespace: team-a
rules:
  - apiGroups:
      - worker.gpustack.ai
    resources:
      - modelartifacts
      - modelartifacts/progress
    verbs:
      - get
      - list
      - watch
```

Grant `nodemodelstores` (`get`, `list`, `watch`, `delete`) only to cluster administrators and the
GPUStack server via `ClusterRole`.

### Failed mount inspection

When `WeightsReady` stays `False`, inspect `Pod` `FailedMount` events (`kubectl -n <ns> describe pod <pod>`)
and `kubectl get nms <node> -o yaml`:

- **Permanent refusal (`PermissionDenied`)**: Namespace, artifact UID, or *`manifestDigest`*
  mismatch, or `Resolved!=True` (cached bytes on disk never authorize another namespace).
- **Transient wait (`Aborted` or `Unavailable`)**: Active background materialization, plugin informer
  startup sync, or failure backoff before *`retryTime`*
  ([Mount refusal vs unavailability](operations.md#mount-refusal-vs-unavailability)).

## Prerequisites and limitations

- **Kubernetes `1.29+`**: `PodReadyToStartContainers` (default on since `1.29`) drives
  `WeightsReady` on mounted deliveries.
- **ModelScope `Engine` SDK floor**: Requires runner `modelscope >= 1.39.1` for commit-pinned
  downloads (present in `cann9.1-*-vllm0.23.0` and `sglang 0.5.18`; older CUDA `vllm0.25.1` /
  `vllm0.29.0` runners fail with `NotExistError` unless using `Node` delivery or a custom image).
- **Engine quirks**: `sglang-gateway` logs one benign `404` tokenizer warning on local mount paths
  and routes by text; vLLM `0.29.0` requires `--enforce-eager` for InternLM2 `trust_remote_code`.

---

**See also** — [Model Store Operations](operations.md) (`Node` cache settings, watermarks, and mount diagnosis) · [Model Prefetch](prefetch.md) (cache budgets, warm-up, and peer sync) · [Model Deployment Configuration](../model-deployment/configuration.md) (role and rollout contract) · [KV Cache Pools and Bindings](../kv-cache/pool.md) (reuse domains prefixed by weight identity)

**Next** → [Model Prefetch](prefetch.md) — warm cached weights and synchronize peers across nodes.
