# Spec: ModelArtifact and Weight References

Status: Shipped
Blocked on: nothing. T1 to T14 are delivered, unit-tested, and exercised end to end on kind at the
Kubernetes 1.29 floor and on a GPU cluster.
Type: Feature

## Summary

GPUStack Operator gains a namespaced `ModelArtifact` resource that owns where a model's weights come
from and which credential reads them: a Hugging Face repository pinned to one commit, or a
directory inside a PersistentVolumeClaim. The controller resolves a Hugging Face branch or tag to a
40-character commit once, lists the files at that commit, and publishes a content address, the
manifest digest, computed with a canonical manifest format this spec defines. A `ModelDeployment`
names its weights with `spec.model.artifactRef` and an `Instance` mounts them with a `model` volume
source; neither carries a URI, a revision or a token. A deployment's engine receives either a
read-only local path (PVC source) or the repository pinned with `--revision <commit>` and a token
read from a Secret (Hugging Face source, delivered by the engine), always serving the name in
`spec.model.name`. When the deployment shares a KV cache pool, the weight identity is rendered
into the engine's store key prefix, so two different weights never read each other's KV blocks.
Today a default installation gives a `ModelDeployment` no way to reuse weights at all (hostPath is
off by default and the role volume sources have no PVC), and a private token can only be written
into the object in plain text; this spec closes both gaps. Node-local caching, per-node state,
placement preference, node-to-node sync and prefetch are later work that builds on the identity
defined here.

## Motivation

### Goals

- A tenant declares a model's weights once, as a Kubernetes object in their namespace, and every
  consumer in that namespace references it by name. The source and the credential exist only on
  that object.
- The weights a deployment serves are reproducible: a branch or tag is resolved to an immutable
  commit exactly once, at creation, and the deployment runs that commit for its whole life.
- Weight identity is a content address that does not depend on where or how the bytes are
  fetched. Its format is public and versioned, so the node-side work that follows (materializing,
  verifying and naming published directories) can build on it without changing it.
- A default installation (hostPath off, a tenant namespace that may enforce Pod Security Admission
  `restricted`) can serve weights from a PVC, including an object-storage bucket exposed through
  the S3 CSI driver the chart bundles.
- A private or gated Hugging Face repository is read with a token from a Secret the tenant owns;
  the token never appears in the `ModelDeployment`, the Pod spec, status, events or logs.
- The engine's served model name stays `spec.model.name`, so routers, the router's tokenizer calls
  and metrics keep matching, and a role that would break that match is refused at admission.
- A deployment attached to a KV cache pool shares KV blocks only with deployments serving the same
  weights.
- Resolution, access loss and every wait (artifact missing, claim not bound, access denied) are
  visible in status with a reason, on the object the user is looking at.

Measurable success criteria are listed per feature under Core Features & Acceptance Criteria.

### Non-Goals

- Node-local materialization, the model CSI node plugin, per-node state objects, download progress,
  placement preference, node-to-node sync, prefetch, retention and tenant disk budgets. Those are
  later specs; this one defines only the identity they consume.
- Accepting ModelScope. The API reserves a `modelScope` source member, and admission refuses it
  until the conditions under Notes are met.
- Filtering a repository with allow or ignore patterns. The canonical manifest format defines the
  filter semantics (rule 5 below) so that adding the fields later does not change the format, but
  the API fields arrive with node delivery, the first delivery that can honor them.
- A `ModelArtifact` v1 aggregated view and a progress subresource. The v1 view arrives with the
  progress work that GPUStack server consumes.
- Cross-namespace sharing of an artifact or of a credential.
- LoRA adapters and speculative-decoding draft models. The shape leaves room for a second
  reference under `spec.model`; nothing is implemented.
- Fixing the pre-existing Instance `persistent` volume path, which has the same PV node-affinity
  failure described under the PVC source (#590).
- Object storage as a source of its own: a bucket is consumed through a PVC.

## Proposal

### The ModelArtifact resource

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata:
  name: qwen-7b
  namespace: team-a
spec:                                   # immutable after creation
  source:                               # exactly one member
    huggingFace:
      repository: Qwen/Qwen2.5-7B-Instruct
      revision: main                    # branch, tag or commit; default "main"
      secretRef: {name: hf-token}       # optional; same namespace; key "token"
    # persistentVolumeClaim:
    #   claimName: models               # same namespace
    #   path: qwen                      # relative directory inside the volume; empty is the root
    # modelScope: {...}                 # reserved; refused by admission in this version
status:
  observedGeneration: 1
  resolved:
    revision: 0123456789abcdef0123456789abcdef01234567    # Hugging Face only
    manifestDigest: sha256:...                            # Hugging Face only
    fileCount: 17
    sizeBytes: 15231233024
    resolvedTime: "2026-09-25T06:00:00Z"
    lastValidatedTime: "2026-09-25T06:00:00Z"
  conditions:
    - type: Resolved
    - type: Degraded
```

It is namespaced, in the `worker.gpustack.ai/v1alpha1` group, stored as a CRD with a `status`
subresource. Printer columns: Revision, Size, Resolved, Age (a printer column takes one JSON path,
so neither the source kind of a union nor a shortened commit can be one). Short name `mart`,
category `gpustack`.

An artifact is an identity, so its whole `spec` is immutable after creation: a different source
or revision is a different artifact, created rather than edited. This is also what makes a frozen
reference meaningful; a reference that cannot change to an object that can would pin nothing.

#### Hugging Face resolution

The controller resolves in the tenant's namespace, with the tenant's credential, and never
caches the token beyond one reconcile:

1. `GET {endpoint}/api/models/{repository}/revision/{revision}` (the revision URL-encoded), with
   `Authorization: Bearer <token>` when `secretRef` is set. The response's `sha` is the commit.
   This endpoint is used because it peels an annotated tag to the commit it points at; the
   `/api/models/{repository}/refs` endpoint returns the tag object for an annotated tag, not the
   commit. It also accepts a short commit and `refs/pr/<n>`, which are therefore accepted too; the
   stored `status.resolved.revision` is always the full 40-character commit.
2. `GET {endpoint}/api/models/{repository}/tree/{commit}?recursive=true`, following every `Link`
   header with `rel="next"` (directories count as entries; the server pages at 1000). The
   `siblings` list of the model-info endpoint is not used: it is not paginated, and
   `huggingface_hub` itself stops trusting it above 1000 files.
3. The file entries become the canonical manifest defined below; its SHA-256 is the digest.
   `fileCount` and `sizeBytes` are the manifest's file count and total size.
4. When `secretRef` is set, `GET {endpoint}/api/whoami-v2` is called once. A 401 emits a Warning
   event on the artifact saying the token is not valid. A wrong token is silently ignored by the
   Hub on a public repository (the answers are identical to no token at all), so without this
   call a mistyped token would never surface; it does not change `Resolved`.

`endpoint` is the `model-artifact-huggingface-endpoint` Setting. Resolution happens once. After
`Resolved=True`, the commit and digest never change, and nothing follows the branch.

Status reasons, as measured against the Hugging Face Hub:

| Hub answer | `Resolved` reason |
| --- | --- |
| 404 with `X-Error-Code: RevisionNotFound` | `RevisionNotFound` |
| 401 (with or without an error code), 403, 404 `RepoNotFound`, `X-Error-Code: GatedRepo` | `AccessDenied` |
| tree answered 200 but a file's `lfs.oid` is 64 `*` characters | `AccessDenied` |
| 5xx, 429, network error, unparseable body | `SourceUnavailable` |

The masked `lfs.oid` row exists because a gated repository answers 200 on the revision and tree
endpoints to a caller that has not been granted access, and only masks every LFS file's SHA-256
(and its `xetHash`) with 64 `*` characters; checking status codes alone would report such an
artifact as resolved. A repository that does not exist and a private repository the caller cannot
read both answer 401 without a token, so the `AccessDenied` message says "does not exist or is not
accessible" rather than guessing which.

Other reasons: `SecretNotFound` (the Secret or its `token` key is missing), `InvalidManifest` (an
entry violates the path or digest rules below), `EmptyManifest` (the commit has no files),
`ManifestTooLarge` (the tree exceeds the controller's fixed entry bound). `Resolved=Unknown` with
reason `Resolving` is written before the first answer.

A change to the referenced Secret triggers a reconcile, the same way `TopologySource` watches the
Secret it references (key name `token`, the same convention). A Secret or a `token` key that goes
missing sets `Resolved=False` at once: the engine that would read it could not download either. A
Secret that cannot be read (an API error) only degrades, like a source that cannot be reached. A
refused resolution is asked again every ten minutes as well, since a grant made on the Hub's side
changes no object this controller watches. The token check against `whoami-v2` runs when the token
is new, not on every revalidation.

#### Revalidation and revocation

Access can be lost after resolution: a token is revoked, a repository becomes private or gated,
or the Secret is replaced. Every `model-artifact-revalidate-interval` (default `24h`) and on every
Secret change, the controller sends one `HEAD {endpoint}/{repository}/resolve/{commit}/{file}`,
with the token, without following redirects. The file is the first file by path at the commit's
root, read from one page of the root tree listing (a root holding no file falls back to the
recursive listing), so the check costs two requests whatever the repository's size. A 2xx or 3xx
answer passes and updates `lastValidatedTime`. Checking the revision or tree endpoint instead
would miss the gated case above.

- `AccessDenied` or `RevisionNotFound` on a revalidation is confirmed by one more check a minute
  later. If the second check fails the same way, `Resolved=False` with that reason. A single
  failure only sets `Degraded=True` with the reason and message.
- `SourceUnavailable` never changes `Resolved`. It sets `Degraded=True` and is asked again every
  minute, so a Hub outage cannot revoke weights that are already pinned.
- `Resolved` returns to `True` when a later check passes. The commit and digest are unchanged,
  because they were never re-resolved.

`Resolved=False` stops new consumption, not running consumption: the deployment and Instance
controllers create no new Pod for an artifact that is not resolved (a new replica, a replacement,
a scale-up), and they never delete a running Pod because of it.

#### PVC source

`persistentVolumeClaim.claimName` names a claim in the artifact's namespace; `path` is a relative
directory inside the volume, which must not be absolute and must not contain a `..` element.
`Resolved=True` means the claim exists; its absence is `Resolved=False` with reason
`ClaimNotFound`, and the controller watches claims so creating it later resolves the artifact.
The operator never reads the claim's content, so a PVC artifact has no revision and no digest;
its content, and any change to it, belongs to the user.

#### Deletion protection

The controller adds the finalizer `worker.gpustack.ai/model-artifact-protection` on first
reconcile. While any `ModelDeployment` or `Instance` in the namespace references the artifact,
deletion leaves it `Terminating`; the finalizer is removed once no reference remains. This is the
shape of PVC protection. Running Pods are never affected by an artifact's deletion.

### Canonical manifest format v1

The manifest digest is the content address of a Hugging Face artifact. It is published here
because node-side delivery will name its published directories by it and verify downloads against
it, and because a user-supplied expected digest may later be compared with it; none of those may
depend on this operator's implementation details.

1. **Input.** Every entry of the tree at the resolved 40-character commit, all pages. Only files
   are kept (`type: file`); directories are skipped; any other entry type fails resolution.
2. **Per-file digest.**
   - An entry with `lfs`: `sha256:<lfs.oid>`, and `lfs.size` must equal `size`.
   - An entry without `lfs`: `gitsha1:<oid>`, the git blob SHA-1.
   - A future ModelScope source uses `sha256:<Sha256>` for every file, and fails when it is
     missing.
   - Only `sha256` (64 hexadecimal digits) and `gitsha1` (40) are allowed, lowercase; any other
     length fails. An `lfs.oid` of 64 `*` characters fails with `AccessDenied`.
3. **Size.** Decimal bytes, no leading zeros, non-negative.
4. **Path.** The bytes the Hub returned, with no Unicode normalization. It must be valid UTF-8,
   contain no control character (U+0000 to U+001F and U+007F), not begin or end with `/`, and
   contain no empty, `.` or `..` segment. A duplicate path after filtering fails.
5. **Filtering.** Filtering happens before canonicalization. Allow and ignore patterns use the
   semantics of `huggingface_hub`'s `filter_repo_objects`: `fnmatchcase` (case-sensitive; `*` and
   `?` cross `/`; `[!...]` negates; an unclosed `[` is literal), and a pattern ending in `/` becomes
   `<pattern>*`. An empty allow list keeps everything; ignore wins over allow. The patterns
   themselves are never part of the digest, so two artifacts whose filters select the same files
   share one digest. This version of the API exposes no patterns, so every artifact's manifest,
   and therefore its digest, covers the whole commit. Adding the pattern fields later changes
   neither this format nor the digest of an artifact that sets no pattern.
6. **Order.** Ascending by the UTF-8 bytes of the path (equivalently, by code point).
7. **Encoding.** The first line is `gpustack-manifest v1`. Each file follows as one line,
   `<algorithm>:<hex> <size> <path>`, fields separated by one space, the path last (so it may
   contain spaces), every line terminated by LF including the last, with no other whitespace and
   no comments.
8. **Digest.** `sha256:` followed by the lowercase hexadecimal SHA-256 of those bytes. It is
   `status.resolved.manifestDigest`.
9. **Not in the digest.** The source kind, the repository name, the commit, the filter patterns,
   `xetHash` and timestamps. They are recorded elsewhere in status.
10. **Version.** Any change to the format changes the first line's version, so two versions can
    never produce the same digest.
11. **Not handled.** Two paths differing only in case collide on a case-insensitive filesystem.
    Nodes are Linux, so this version does not handle it.

Consequences stated for users:

- The digest is a pure content address. A private and a public repository holding the same files
  have the same digest, so the digest is never evidence of authorization: authorization always
  comes from resolving with the namespace's own credential.
- The same files on Hugging Face and on ModelScope have different digests, because non-LFS files
  use `gitsha1` on one and `sha256` on the other, so content is not deduplicated across hubs.
- `.gitattributes` is an ordinary file at the commit and is part of the manifest.
- The digest contains SHA-1 components for non-LFS files (configuration and tokenizer files). A
  download is still verified by recomputing each file's hash against the manifest.

The manifest itself is not stored: a large repository's manifest (thousands of lines) does not
belong in an object's status. Anything that needs it recomputes it from the Hub at the commit and
compares the result with `manifestDigest`.

### ModelDeployment integration

```yaml
spec:
  model:
    name: qwen-7b                     # the served name; unchanged meaning
    artifactRef: {name: qwen-7b}      # optional; LocalObjectReference
status:
  model:                              # echo; absent without artifactRef
    artifact: qwen-7b
    revision: 0123456789abcdef0123456789abcdef01234567
    manifestDigest: sha256:...
    delivery: Engine                  # Pvc | Engine
  conditions:
    - type: WeightsReady
```

`artifactRef` has the shape of `spec.kvCache.poolRef`: a `LocalObjectReference`, so a reference to
another namespace is unrepresentable rather than refused. The deployment still provisions nothing;
it references an object provisioned elsewhere. The type comment on `ModelDeploymentModel` is
rewritten to say exactly that, and keeps the prohibition it already states: sources, URIs,
credentials and download policy never enter the `ModelDeployment`.

`spec.model` is already frozen after creation by the rule that a field answering "which
deployment is this" is immutable; the weights a deployment serves are that answer, so
`artifactRef` is frozen on every deployment. Serving other weights means creating another
deployment.

A reference to an artifact that does not exist yet, or is not resolved, is admitted. The
deployment waits in status (`WeightsReady=False`) and creates no Pod until the artifact resolves,
so a GitOps tool does not have to order the two objects.

#### Rendering

For every role that does not replace its command line:

| | PVC source | Hugging Face source (delivery `Engine`) |
| --- | --- | --- |
| Volume | the claim, `readOnly: true`, mounted read-only at `/var/lib/gpustack/model` with `subPath` = the artifact's `path` | an `emptyDir` with `sizeLimit` (below), mounted at `/var/lib/gpustack/model-cache` |
| vLLM argv | `vllm serve /var/lib/gpustack/model` | `vllm serve <repository> --revision <commit>` |
| SGLang argv | `... --model-path /var/lib/gpustack/model` | `... --model-path <repository> --revision <commit>` |
| Served name | `--served-model-name <spec.model.name>` in the defaulted arguments | same |
| Environment | none added | `HF_HOME=/var/lib/gpustack/model-cache`, `HF_ENDPOINT` from the Setting, `HF_TOKEN` from `secretKeyRef` (key `token`) when the artifact has a `secretRef`; `HTTPS_PROXY` and `NO_PROXY` from the Settings when they are set |
| Scheduling | the bound PV's required node affinity (below) | none added |

Behavior measured on vLLM 0.29.0 and SGLang 0.5.18, a single 48 GB accelerator, behind
`llm-d-router` and `sglang-gateway`:

- A local path plus `--served-model-name` makes `/v1/models` report exactly `spec.model.name`,
  and the metrics' `model_name` label carries only that value, on both engines. Both routers route
  successfully; `llm-d-router`'s token producer calls the engine's render endpoint by
  `spec.model.name` and succeeds.
- `--revision <commit>` pins both the weights and the tokenizer on both engines; no separate
  tokenizer revision is needed. A token supplied through an environment variable that references a
  Secret downloads a gated repository; without it the engine fails with 401.
- A model with `trust_remote_code` loads from a read-only directory. The remote code is copied
  into `$HF_HOME/modules`, which must be writable; with `HF_HOME` on the cache volume (Engine) or on
  the container's writable root filesystem (PVC, today's Pods set no `readOnlyRootFilesystem`) no
  extra volume is needed.

Argument and environment ownership, which admission and the renderer read from one table:

- While `artifactRef` is set, these become owned keys: on vLLM `--model`, `--revision`,
  `--tokenizer-revision`, `--download-dir`; on SGLang `--model-path`, `--revision`,
  `--download-dir`. A role's `extraArgs` naming one is refused, because the operator's positional
  model and pin would otherwise be silently overridden by a later flag. The environment names
  `HF_TOKEN`, `HF_ENDPOINT` and `HF_HOME` are owned too, whatever the source: admission cannot know
  the delivery of an artifact that has not resolved yet. `HTTPS_PROXY` and
  `NO_PROXY` are defaulted, so a role's own value wins.
- `--served-model-name` is defaulted, not owned: a role may state it, but for every managed role
  (with or without `artifactRef`) its value must be exactly `spec.model.name`. An update that leaves
  a role's served names as they were is not judged again, so an object stored before the rule still
  takes other edits. A mismatch was
  measured to cost two silent failures: requests by `spec.model.name` answer 404 through the
  router, and requests by the other name succeed while `llm-d-router`'s token producer fails on
  every request and its prefix-cache scoring falls to zero, with the deployment reporting Ready.
  Only admission can stop that.
- While `artifactRef` is set, `/var/lib/gpustack/model` and `/var/lib/gpustack/model-cache` are
  reserved mount paths: a role's `additionalVolumes[].mountPath` equal to, inside, or containing
  either is refused.

A role that replaces its command line (the take-over tier) gets the PVC volume and mount and
nothing else: no argument, no environment. With a Hugging Face source it gets nothing, because the
command's author downloads the weights; this is stated in the documentation.

#### Engine delivery sizing

The download lands in the cache volume, and kubelet counts an `emptyDir`'s usage toward the Pod's
ephemeral-storage limit (the sum of its containers' limits) as well as toward the volume's own
`sizeLimit`. Today a replica's containers carry the InstanceType's local storage as their
ephemeral-storage limit, so a model larger than that would be evicted mid-download even with a
large `sizeLimit`. Therefore:

- `sizeLimit` is the manifest's `sizeBytes` plus a headroom of 10%, and at least 1 GiB.
- The main container's ephemeral-storage **limit** is raised by the same amount. Its request is
  unchanged, so Kueue quota and scheduling are unchanged.

Because the manifest is the whole commit, `sizeBytes` is an upper bound on what the engine
downloads (an engine downloads a subset: vLLM skips `.bin` files when `.safetensors` exist). A
download that exceeds the limit anyway was measured to evict the Pod, which then ends `Succeeded`
and is not recreated by the current controller; that pre-existing defect is #587,
and the status surfaces the eviction message (below).

#### PVC placement

Kueue topology-aware scheduling does not read a PVC's volume. A PV that carries node affinity
(a local volume, a hostPath PV, a node-attached disk) was measured, on Kueue 0.18.9, to fail
silently: TAS assigned another node, the Pod stayed Pending forever, and the Workload stayed
Admitted and kept its quota, with nothing on the Workload saying why. Injecting the PV's required
node affinity into the Pod was measured to fix it: TAS honored it, and when the PV's node was
full the Workload waited before quota (`QuotaReserved=False`) and admitted itself once room
appeared. So:

- **Bound claim.** Each Pod is created with the bound PV's `spec.nodeAffinity.required` as its own
  required node affinity. It is added at Pod creation, outside the Pod-spec fingerprint, so
  learning the affinity after the first binding does not roll replicas. A PV without node affinity
  adds nothing.
- **Pending claim whose StorageClass binds `WaitForFirstConsumer` through a dynamic
  provisioner.** Pods are created without affinity; the provisioner creates the volume on the first
  Pod's assigned node, and later Pods read the bound PV.
- **Pending claim whose StorageClass binds `WaitForFirstConsumer` with no provisioner**
  (`kubernetes.io/no-provisioner`, the usual static local-volume class). Treated like immediate
  binding below. TAS picks a node first and the volume binder then looks for a matching PV on that
  node only, so a node without one would reproduce the silent Pending that holds quota. This case
  is inferred from the binding order and was not measured; an end-to-end case covers it where kind
  can build a static local PV. The message tells the user to bind the claim to its PV first
  (`spec.volumeName`).
- **Pending claim with immediate binding, or a missing claim.** No Pod is created;
  `WeightsReady=False` with reason `ClaimNotBound` (or `ArtifactNotResolved` for a missing claim).
- **More than one Pod on a claim that is not shared.** When the deployment would mount the claim
  in more than one Pod (several replicas, a replica of several Pods, or several roles) and the
  claim's access modes include neither `ReadOnlyMany` nor `ReadWriteMany`, no Pod is created and
  `WeightsReady=False` with reason `AccessModeConflict`. `ReadWriteOnce` would crowd every replica
  onto one node, and `ReadWriteOncePod` admits only one.

Measured object-storage and file-storage throughput, for the documentation (one node with one
48 GB accelerator; the node's boot disk is a 1000 GiB network SSD; the object-storage and NFS
servers each run on a CPU node with a 2 TiB network disk of the default class; page caches dropped
on both ends before every read):

- S3 CSI (geesefs) cold sequential read 467 MB/s for a 7B model, against 479 MB/s from the node's
  boot disk; both sit at the network disk's ceiling (about 470 MB/s for that disk class and size),
  so the CSI driver's own ceiling was not reached. vLLM loaded the weights 6-9% faster from S3
  (7B: 32.6 s against about 35 s). Performance is not a reason to avoid an S3 PVC.
- S3 CSI has no warm-read benefit: a second read on the same node ran at about 455 MB/s, against
  7.4-7.8 GB/s from the boot disk's page cache. Every restart and every replica on the node
  re-reads object storage.
- NFS CSI cold read about 312 MB/s, weight loading 24% slower than S3; the NFS client's page
  cache makes warm reads fast.
- Disk throughput of that class grows with disk size; any figure quoted carries the disk's type
  and size. The S3 endpoint of a CSI PV must be a ClusterIP address rather than an in-cluster DNS
  name.

#### KV cache reuse domain

With the Mooncake store, vLLM 0.29.0 builds each block key as
`[cache_prefix@]<model_name>@tp_rank:N@...@<chunk hash>`, where `model_name` is the last path
segment of `--model` and the chunk hash chains token ids; SGLang 0.5.18 prefixes its HiCache keys
with `<extra_backend_tag>_<served model name with "/" replaced by "-">`. Neither includes the
revision or any weight digest, and a Binding contributes only the store tenant. Two deployments
serving different commits of one repository under one Binding therefore hit each other's blocks.
This was measured on vLLM 0.29.0 with Mooncake 0.3.13.post1: a deployment of commit B found all
1008 keys commit A had written before serving, took 16128 prompt tokens from the store on replay,
and produced different greedy output than when computing its own KV. A local-path delivery makes
it worse: every model's last path segment is `model`.

So when a deployment has both `artifactRef` and `spec.kvCache`, the operator renders a weight
identity into the key namespace it already owns:

- vLLM: `kv_connector_extra_config.cache_prefix` on the store connector inside
  `--kv-transfer-config`.
- SGLang: `{"extra_backend_tag": "<identity>"}` as `--hicache-storage-backend-extra-config`. In
  SGLang 0.5.18 an extra configuration without `master_server_address` or `client_server_address`
  does not select the extra-configuration loader, so the store keeps reading its configuration
  from the environment the operator renders.

The identity is `m-` followed by the first 32 hexadecimal digits of the manifest digest (Hugging
Face), or of the SHA-256 of the artifact's UID (PVC, whose content has no address). It contains no
`@`, `_` or `:`, which the two engines use as key separators, and it adds 34 characters to every
key. Deployments with the same identity keep sharing blocks; deployments with different identities
never do. A deployment without `artifactRef` renders no prefix, which is today's behavior.

A PVC artifact's identity is its UID, not its content, so replacing the files inside the claim
under the same artifact keeps the identity and new deployments would hit KV blocks computed from
the old files. The rule, stated in the documentation, is that new weights on a PVC are a new
`ModelArtifact`. An explicit revision field on the PVC source would express the same thing with an
API field a user must remember to bump, and would fail the same way when forgotten; the new-object
rule needs no field and matches how a Hugging Face artifact changes weights.

The prefix renders on vLLM's native store connector and on SGLang. The vLLM-Ascend store
connector keys by rules this spec did not read, so it gets no prefix: a key it might ignore would
look like isolation without being it.

The per-request `cache_salt` is not used: the operator cannot set it. The Binding's `blockSize`
and `dtype` are not part of either engine's key either; that is a KV-cache concern independent of
weight identity and is not handled here (#591).

#### Status

`status.model` echoes the resolved artifact and is written only when it changes; it is absent
without `artifactRef`. `delivery` is `Pvc` or `Engine`.

`WeightsReady` reports whether every engine role's weights are available:

| Status | Reason | When |
| --- | --- | --- |
| True | `NotApplicable` | the deployment names no artifact |
| False | `ArtifactNotFound` | the artifact does not exist |
| False | `ArtifactNotResolved` | the artifact is not `Resolved`; the message carries its reason |
| False | `ClaimNotBound`, `AccessModeConflict` | the PVC rules above |
| False | `WeightsNotMounted` | PVC: a created Pod does not yet have `PodReadyToStartContainers=True` (kubelet sets it after the Pod's volumes are mounted) |
| False | `Downloading` | Engine: a created Pod's main container is not yet Ready; the engine downloads the weights itself and reports no progress |
| True | `Mounted` / `Downloaded` | every created Pod passed the check above |

The condition is derived from Pod conditions, never from Events. While no Pod exists because the
deployment is waiting, the phase message carries the `WeightsReady` message. When a replica Pod has
been evicted, the phase message carries the Pod's own `status.message` (for example, the kubelet's
"Usage of EmptyDir volume ... exceeds the limit"), which was measured to be the only place that
reason survives.

Kueue's `waitForPodsReady` is off in the chart. If an administrator enables it, a download that
outlasts its timeout evicts and requeues the replica; the documentation states this.

### Instance integration

```yaml
spec:
  additionalVolumes:
    - mountPath: /models/qwen
      model: {artifactRef: {name: qwen-7b}}   # exclusive with persistent/configMap/secret/hostPath
```

- A `model` entry is always mounted read-only and takes no `subPath` (the artifact's `path` is the
  sub-path). An Instance may mount several, each at its own path.
- Only a PVC artifact is accepted in this version. A Hugging Face artifact is served to an Instance
  by node delivery, which does not exist yet: admission refuses a reference to an existing Hugging
  Face artifact, and the Instance controller reports and waits on one it discovers later.
- The PVC placement rules above apply to the Instance's Pod.
- The Instance controller creates no Pod while the artifact is missing or not resolved, and reports
  why in the Instance phase message.

### Configuration

New Settings, read directly by the controller and the renderer (they follow the existing
kebab-case names, and are seeded once from `GPUSTACK_*` environment variables):

| Setting | Default | Read by |
| --- | --- | --- |
| `model-artifact-huggingface-endpoint` | `https://huggingface.co` | resolution, revalidation, `HF_ENDPOINT` of Engine delivery |
| `model-artifact-https-proxy` | blank | resolution and revalidation; `HTTPS_PROXY` of Engine delivery; only an `http` or `https` URL without credentials, since the value reaches tenant Pods: admission refuses another on write, and the worker refuses to start on one from the environment, which no admission reads |
| `model-artifact-no-proxy` | blank | the same; `NO_PROXY` |
| `model-artifact-ca-bundle` | blank | resolution and revalidation only: the name of a ConfigMap in the worker's namespace, key `ca.crt` |
| `model-artifact-revalidate-interval` | `24h` (minimum `1m`) | revalidation |

Endpoint, proxy and CA are administrator configuration only; no tenant object can change where the
controller connects. The CA bundle is not given to engine Pods: they run in tenant namespaces,
which cannot mount a ConfigMap from the worker's namespace. A tenant who needs a private CA in an
Engine-delivered Pod mounts their own ConfigMap and sets `REQUESTS_CA_BUNDLE`; the documentation
says so.

No delivery-mode Setting is added: Engine is the only delivery for a Hugging Face source in this
version, and a Setting with one legal value offers no choice.

### ModelScope

The `modelScope` member is in the API and admission refuses it with a message naming the opening
conditions. Measured differences from Hugging Face that an opening must handle:

- Branch resolution depends on an undocumented endpoint, `GET /api/v1/models/<id>/commits?Ref=<rev>&PageSize=1`,
  which returns master when the parameter name is misspelled; resolution must cross-check with
  `git ls-remote` (user name `oauth2`, the token as password).
- `repo/files` silently truncates at 3000 entries; a listing of exactly 3000 must be re-listed per
  directory with `Root=<dir>`, and a directory with 3000 or more direct children must fail.
- Every access failure is a 404; errors are classified by the envelope `Code` (10010205001 not
  found, 10010200001 no access, including gated).
- The ModelScope SDK 1.37.1 inside the vLLM 0.29.0 runner image accepts only branch and tag names
  and fails on a commit (`NotExistError`); SDK 1.39.1 pins a commit, and SGLang 0.5.18 pins one.
  Opening needs a vLLM runner with SDK 1.39.1 or later and an end-to-end check that it pins.

### User Stories

#### Story 1

As a model deployer, I want to declare a model's weights once and reference them from my
deployment by name, so that I never assemble a hostPath or paste a revision into engine arguments,
and every restart serves the same commit.

#### Story 2

As a model deployer with a private or gated Hugging Face model, I want the token to live in a
Secret in my namespace and be used for resolution and download, so that it never appears in my
deployment, in a Pod spec or in any status.

#### Story 3

As a platform operator without node caching, I want deployments and Instances to load weights from
a PVC, including an object-storage bucket through the bundled S3 CSI driver, so that a default
installation can reuse weights instead of every Pod pulling from the Hub.

#### Story 4

As a security administrator, I want access loss (a revoked token, a repository made private) to
stop new consumption of an artifact without killing running replicas, so that revocation is
effective and a Hub outage is not an outage.

#### Story 5

As a user of a shared KV cache pool, I want deployments of different weights to never read each
other's KV blocks while deployments of the same weights keep sharing them, so that the cache never
serves wrong tensors.

#### Story 6

As an operator reading status, I want to see why a deployment is not starting (artifact missing,
access denied, claim not bound, weights still downloading, a Pod evicted for its cache size), on
the deployment itself, so that I do not have to read container logs.

### Core Features & Acceptance Criteria

#### F1 - The ModelArtifact API and admission

- The CRD exists in `worker.gpustack.ai/v1alpha1`, namespaced, with the status subresource,
  printer columns and short name above; generated clients, deepcopy and protobuf are regenerated.
- Admission refuses: no source or more than one; a `modelScope` source; an empty or malformed
  Hugging Face repository (`owner/name` or a bare canonical name, each part matching the Hub's
  name rules); a revision that is empty after defaulting, longer than 255 characters or containing
  whitespace or control characters; an absolute `path` or one with a `..` element; any change to
  `spec` on update.
- Admission defaults `revision` to `main`. It does not require the Secret or the claim to exist.

#### F2 - Resolution, the manifest and revalidation

- A branch, an annotated tag, a full commit and a short commit resolve to the same 40-character
  commit as `git ls-remote` (peeled for a tag).
- The manifest of a repository with more than 1000 tree entries has the digest three independent
  implementations computed at that commit over every page, whose paths matched `git ls-tree -r`
  (case-97 on `rhasspy/piper-voices`, 3922 entries in four pages).
- Two independent computations over the same tree, in shuffled order, produce the same digest; a
  one-byte change in a size, a digest or a path, or a removed file, produces a different one. A
  second implementation of the format (a short script in the test data) agrees byte for byte.
- Each row of the reason table maps as stated, including a gated repository whose tree is masked.
- A token that the Hub rejects produces the Warning event on a public repository.
- A revalidation `AccessDenied` confirmed by the second check sets `Resolved=False`; a single
  failure sets only `Degraded=True`; `SourceUnavailable` never changes `Resolved`; a later pass
  restores `Resolved=True` with the commit and digest unchanged.
- No token appears in status, events or controller logs (a scan with a known token value finds
  zero matches, after the same scan is shown to find a planted one).

#### F3 - Deletion protection

- Deleting a referenced artifact leaves it `Terminating` until the last `ModelDeployment` or
  `Instance` referencing it is deleted; an unreferenced artifact is deleted at once.

#### F4 - ModelDeployment references and rendering

- `spec.model.artifactRef` is a `LocalObjectReference`, frozen with the rest of `spec.model`.
- A deployment referencing a missing or unresolved artifact is admitted and creates no Pod until
  the artifact resolves.
- The rendered Pods match the rendering table for both engines and both sources; the take-over
  tier gets only the PVC mount.
- Admission refuses the owned keys listed above, a `--served-model-name` other than
  `spec.model.name` on any managed role, and an additional volume at or around the reserved paths.
- The Engine delivery's `sizeLimit` and the main container's raised ephemeral-storage limit follow
  the sizing rule, and its request is unchanged.
- The PVC placement rules hold: a bound PV's required node affinity is on every created Pod and is
  not part of the Pod-spec fingerprint; the three waiting cases create no Pod and report their
  reasons.
- An artifact that stops being resolved stops replacements and scale-ups; no running Pod is
  deleted. While the weights are blocked, a spec edit's rollout is held as well: an outdated replica
  is not deleted while its replacement could not be created.

#### F5 - KV reuse domain

- With `artifactRef` and `spec.kvCache`, vLLM's `--kv-transfer-config` carries `cache_prefix` and
  SGLang carries the `extra_backend_tag` extra configuration, with the identity defined above;
  without `artifactRef` the rendered configuration is unchanged byte for byte.
- On a real store, two deployments of the same identity under one Binding hit each other's blocks,
  and two of different identities do not (measured with the engine's store hit counter, every
  send proven by its finished-request counter, with a must-hit and a must-miss control).

#### F6 - Status

- `status.model` and `WeightsReady` follow the tables above; the phase message carries the waiting
  reason and an evicted Pod's message.

#### F7 - Instance

- The `model` volume source mounts a PVC artifact read-only at its path with the artifact's
  sub-path, applies the PVC placement rules, and waits in status for a missing or unresolved
  artifact; a Hugging Face artifact is refused or reported.

#### F8 - Documentation

- A new reference page covers the resource, the manifest format, both sources, both deliveries,
  status, the Settings, the measured PVC throughput and the engine and router behavior;
  `docs/README.md`'s page table, the documentation skill's routing table and `docs/settings.md`
  are updated; `make lint docs` passes.

#### F9 - End-to-end

- A PVC source; a Hugging Face source through Engine delivery; one deployment on vLLM and one on
  SGLang; one behind `llm-d-router`; a private repository refused without a token and resolved with
  one; revocation (the Secret's token replaced by an invalid one) stopping new Pods without deleting
  the running one; the KV pair of F5. Each has recorded evidence.

### Notes / Constraints / Caveats

#### Platform capabilities and the version floor

| Capability | Earliest default-on version | Source |
| --- | --- | --- |
| Admission webhooks `admissionregistration.k8s.io/v1` | 1.16 (GA) | Kubernetes API reference |
| CRD `status` subresource, `emptyDir.sizeLimit`, `secretKeyRef` env | 1.22 or earlier | Kubernetes API reference |
| PV `spec.nodeAffinity` | 1.10 (beta), 1.14 local volumes GA | Kubernetes storage docs |
| `ReadWriteOncePod` access mode | 1.29 (GA) | `pkg/features/kube_features.go` at v1.29.0 |
| `PodReadyToStartContainers` Pod condition | 1.29 (beta, default on) | `pkg/features/kube_features.go` at v1.29.0 |
| Kubelet counting `emptyDir` usage toward the Pod's ephemeral-storage limit | 1.29 (read) | `pkg/kubelet/eviction/eviction_manager.go` at v1.29.0, `podEphemeralStorageLimitEviction` |
| Kueue TAS honoring a Pod's required node affinity | Kueue 0.18.9 (measured), which requires Kubernetes 1.29 | Kueue `pkg/cache/scheduler/tas_flavor_snapshot.go` |

The floor is Kubernetes 1.29, which is already this repository's effective floor because the
bundled Kueue v0.18 requires it. Nothing here uses ValidatingAdmissionPolicy (1.30) or node
identity in a bound service-account token (1.30); CRD CEL rules (1.25) are not needed because
immutability is a webhook rule, as for every other type in this group. On a cluster where the
`PodReadyToStartContainers` condition is off, the PVC `WeightsReady` check stays `False`
(`WeightsNotMounted`) while the replicas run; the documentation says so. Evidence gathered on a
newer cluster states its version.

#### Engine and router caveats for the documentation

- `sglang-gateway` registers each worker's tokenizer from the worker's reported `model_path`. With
  a local path it tries to fetch that path from the Hub, gets 404, logs one warning and routes
  anyway (its `cache_aware` policy works on text); features relying on the gateway's own tokenizer
  have none. With Engine delivery it would fetch the repository at `main` without a token; that
  combination was not measured.
- vLLM 0.29.0 needs `--enforce-eager` for InternLM2 with `trust_remote_code`; an engine defect,
  unrelated to delivery.
- A future `readOnlyRootFilesystem` would need `$HF_HOME/modules`, `/root/.cache/vllm` and
  `/root/.cache/flashinfer` on writable volumes.

#### Implementation constraints

- The render stays a pure function of its input: the reconciler reads the artifact, the claim and
  the PV, and passes their facts in.
- The canonical manifest code lives in a package of its own, importable by node-side code later
  without importing the controller.
- The resolution HTTP client has per-request timeouts, retries with backoff only on
  `SourceUnavailable`, and a fixed bound on tree entries; the controller runs a small number of
  concurrent reconciles, so a cluster of artifacts cannot flood the Hub.
- Settings are read at reconcile time; changing the endpoint does not re-resolve resolved
  artifacts.

### Boundaries

- **Always:** keep source and credential on the artifact only; resolve with the namespace's own
  Secret; keep status free of tokens; keep `--served-model-name` equal to `spec.model.name`; keep
  running Pods running on access loss; derive status from Pod conditions, not Events; test with
  fake clients and a fake Hub server; follow the repository's comment and naming conventions.
- **Ask first:** any change to the chart or `hack/deps.sh`; opening ModelScope; any new tenant-
  writable field that influences where the controller connects; a GPU cluster window; creating or
  changing Hugging Face test repositories.
- **Never:** put a URI, revision or token in `ModelDeployment` or `Instance`; follow a branch after
  resolution; use the digest as authorization; cache a token across reconciles; copy an
  administrator ConfigMap or Secret into a tenant namespace; delete a running Pod because an
  artifact lost access; add a Setting with a single legal value.

### Risks and Mitigations

- The Hub changes an endpoint's shape (pagination, masking, error codes) → the classification and
  pagination live in one client with table-driven tests against recorded answers, and the e2e
  resolves real repositories.
- An engine downloads more than the manifest's size → the manifest is the whole commit, which
  bounds any subset; the e2e records the cache volume's usage against `sizeLimit`.
- `extra_backend_tag` or `cache_prefix` behaves differently than read → both are exercised at
  runtime by the KV e2e with must-hit and must-miss controls before the spec is shipped.
- Raising the ephemeral-storage limit meets a namespace `LimitRange` maximum → the Pod create is
  refused by the API server with that reason, which the existing create-failure event surfaces;
  documented on the reference page.
- A tenant creates many artifacts to make the controller call the Hub → resolution happens once
  per artifact, revalidation once per interval, with bounded concurrency and timeouts.
- Revocation cannot be tested with a valid-but-unauthorized token (both test tokens belong to one
  account, and an owner always has access) → revocation is tested by replacing the Secret's token
  with an invalid one, which exercises the same 401 path; the gap is recorded.

## Design Details

### Commands

```sh
go test ./pkg/modelartifact/... ./pkg/worker/... ./api/...   # unit suites this spec touches
make lint </dev/null                                         # edit pass; compare git diff before and after
make lint docs </dev/null                                    # after docs/ or specs/ edits
```

`make generate` refuses a worktree whose absolute path does not end in `/gpustack.ai/gpustack`.
Run it in a disposable copy with that suffix after changing types under `api/` or webhook markers,
then compare and copy the generated outputs back.

The environment has two halves:

- **Local.** Unit tests, lint and generate run on the development machine. The kind end-to-end
  cases run on a kind cluster of this spec's own on the same machine's Docker (one control plane,
  two workers), with its kubeconfig in a scratch file so the user's kubeconfig is never read or
  written, and an operator image built natively and loaded into kind:

  ```sh
  kind create cluster --name mam-s1 --kubeconfig "$KCFG" --config <two-worker kind config>
  KUBECONFIG="$KCFG" bash .agents/skills/_e2e-lib/scripts/build-load.sh <TAG>
  KUBECONFIG="$KCFG" bash .agents/skills/_e2e-lib/scripts/deploy.sh <NS> <TAG>
  KUBECONFIG="$KCFG" bash .agents/skills/gpustack-operator-e2e/cases/case-<N>.sh <NS>
  kind delete cluster --name mam-s1 --kubeconfig "$KCFG"
  ```

- **Remote GPU cluster.** The engine, router, Engine-delivery and KV cases run on a shared test
  cluster with a GPU node group, reached through its named kube context passed explicitly to every
  `kubectl` and `helm` call (the default kubeconfig's current context is never changed), under a
  cluster lock granted for the run. The operator image is an amd64 `dev-<short sha>` build of a
  clean, committed tree, and its digest is checked against the running Pods' `imageID`.

The Hugging Face token for the private-repository cases is read from the environment at run time
into a Secret (`kubectl create secret generic ... --from-literal=token="$HF_TOKEN_READONLY"`) and
never written to a file.

### Project Structure

- `api/worker/v1alpha1/model_artifact.go` — the `ModelArtifact` type; `model_deployment.go` and
  `instance.go` gain the references.
- `pkg/modelartifact/` — the canonical manifest format and the Hugging Face resolution client,
  independent of the controller.
- `pkg/worker/controllers/worker/model_artifact.go` — resolution, revalidation, conditions and the
  protection finalizer.
- `pkg/worker/controllers/worker/model_deployment*.go` — artifact resolution into the render input,
  rendering, PVC placement, status.
- `pkg/worker/controllers/worker/instance.go` — the `model` volume source.
- `pkg/worker/kvcache/inject/` — the store key prefix for both engines.
- `pkg/worker/webhooks/worker/` — `model_artifact.go`, and the rules added to `model_deployment.go`
  and `instance.go`.
- `pkg/worker/settings/` — the five Settings.
- `docs/reference/model-artifact.md`, `docs/README.md`, `docs/settings.md`, the documentation
  skill's routing table.
- `.agents/skills/gpustack-operator-e2e/cases/` — the end-to-end cases.

### Code Style

Pure render input carries resolved facts as values, the way the connector does today:

```go
// ModelDeploymentRenderInput is everything one replica's Pod is rendered from.
//
// The InstanceType and the connector arrive as values rather than being read here, because the
// render must stay a pure function of its inputs: the reconciler converges the same object on every
// pass, so a render that reached for a client could return two different Pods for one spec and roll
// the deployment forever.
type ModelDeploymentRenderInput struct {
	Deployment *workercore.ModelDeployment
	Role       *workercore.ModelDeploymentRole
	// ...
	Connector ModelDeploymentConnectorRender
}
```

Other conventions:

- Comments state the rule and its reason, with no task identifiers.
- Owned and defaulted keys are data read by both the webhook and the renderer.
- Tests are table-driven, use fake clients and an `httptest` Hub, and assert final objects.
- Files are named in snake_case.

### Implementation Plan

Build order is the task order below; every task leaves the tree compiling with its tests green.
Checkpoints: after T6 the resource works on its own; after T11 every consumer renders; after T13
the kind evidence exists; T14 is the GPU evidence.

- [x] **T1 · Canonical manifest format**
      Blocked by: None
      Owns: `pkg/modelartifact/manifest*.go`, `pkg/modelartifact/testdata/manifest/**`
      Gate: review
      Acceptance: builds the v1 encoding and digest from file entries per rules 1-11; refuses every
      invalid path, digest, size and duplicate; shuffled input gives the same bytes; each single
      mutation (size, digest, path, removed file) changes the digest; a Python reference script in
      testdata computes the same digest for a recorded tree. Filtering (rule 5) has no caller in
      this version and is not implemented; it arrives with the pattern fields.
      Verify: `go test ./pkg/modelartifact/ -run '^TestManifest' -v`

- [x] **T2 · Hugging Face resolution client**
      Blocked by: T1
      Owns: `pkg/modelartifact/huggingface*.go`, `pkg/modelartifact/client*.go`,
      `pkg/modelartifact/testdata/huggingface/**`
      Gate: review
      Acceptance: against an `httptest` Hub: passes any revision, escaped as one path segment, to
      `/revision/` (the Hub peels a tag, which case-97 measures live); follows `Link rel="next"` across pages; maps every row of the reason
      table, including the masked `lfs.oid`; `whoami-v2` reports an invalid token; revalidation
      `HEAD` passes on 2xx/3xx without following redirects; the transport honors proxy, no-proxy
      and a CA bundle; per-request timeouts and the entry bound hold; no token appears in any
      returned error string.
      Verify: `go test ./pkg/modelartifact/ -run '^Test(HuggingFace|Client)' -v`

- [x] **T3 · API types and generation**
      Blocked by: None
      Owns: `api/worker/v1alpha1/**`, `pkg/kubeclients/**`, generated files under `pkg/**/zz_generated*`
      Gate: review
      Acceptance: `ModelArtifact` (spec union with the reserved `modelScope` member, status,
      printer columns, short name, category), `ModelDeploymentModel.ArtifactRef` with the rewritten
      type comment keeping the prohibition, `ModelDeploymentStatus.Model`, and
      `InstanceAdditionalVolume.Model`; `make generate` output is committed and a rerun changes
      nothing.
      Verify: `go build ./... && go test ./api/...` and a second `make generate` with empty `git status`

- [x] **T4 · Settings**
      Blocked by: None
      Owns: `pkg/worker/settings/**`
      Acceptance: the five Settings with their defaults, blank-allowance and validation (endpoint is
      an `http` or `https` URL; interval parses as a duration of at least one minute); seeded from
      `GPUSTACK_*` like the others. The validation is the existing admission helpers, and the
      package has no test harness of its own, so the Settings are exercised through their readers'
      tests in T6 and T8. The proxy is the exception: its check is `modelartifact.ValidateProxy`,
      shared with the resolution client, and tested there.
      Verify: `go build ./pkg/worker/...`

- [x] **T5 · ModelArtifact admission**
      Blocked by: T3
      Owns: `pkg/worker/webhooks/worker/model_artifact*.go`, the webhook registration it needs
      Acceptance: every refusal in F1 fires with its field path, `revision` defaults to `main`,
      and a Secret or claim that does not exist is admitted.
      Verify: `go test ./pkg/worker/webhooks/worker/ -run '^TestModelArtifactWebhook' -v`

- [x] **T6 · ModelArtifact controller**
      Blocked by: T2, T3, T4
      Owns: `pkg/worker/controllers/worker/model_artifact*.go`, `pkg/worker/controllers/setup.go`
      Gate: review
      Acceptance: resolves Hugging Face artifacts through T2 with the namespace's Secret and PVC
      artifacts by the claim's existence; writes `status.resolved` once and never re-resolves;
      revalidates on the interval and on Secret change with the one-minute confirmation;
      `SourceUnavailable` only degrades; watches Secrets and claims it references; adds and removes
      the protection finalizer from a namespace list of referencing ModelDeployments and Instances;
      emits the invalid-token Warning when the token is new; writes `Resolving` before the first
      answer and status only on change; records its pacing only once the status write lands.
      Verify: `go test ./pkg/worker/controllers/worker/ -run '^TestModelArtifactReconcile' -v`

- [x] **T7 · ModelDeployment admission and ownership table**
      Blocked by: T3
      Owns: `pkg/worker/webhooks/worker/model_deployment*.go`,
      `pkg/worker/controllers/worker/model_deployment_connector.go` (the owned and defaulted tables)
      Gate: review
      Acceptance: the artifact-conditional owned arguments and environment are refused while
      `artifactRef` is set and accepted without it; `--served-model-name` other than
      `spec.model.name` is refused on every managed role (both spellings, both engines, vLLM's
      multi-value form); the reserved mount paths are refused at, inside and around; `artifactRef`
      is frozen with `spec.model`.
      Verify: `go test ./pkg/worker/webhooks/worker/ -run '^TestValidateModelDeployment(ArtifactOwnedKeys|ServedModelName|ReservedMountPaths)$' -v`

- [x] **T8 · ModelDeployment rendering**
      Blocked by: T4, T7
      Owns: `pkg/worker/controllers/worker/model_deployment_render*.go`,
      `pkg/worker/controllers/worker/model_deployment_artifact*.go`
      Acceptance: a new artifact block in the render input produces the rendering table for both
      engines and both sources; the served name is defaulted only with `artifactRef`; the Engine
      cache `sizeLimit` and the raised ephemeral-storage limit follow the sizing rule with the
      request unchanged; the take-over tier gets only the PVC mount; a deployment without
      `artifactRef` renders byte for byte what it rendered before.
      Verify: `go test ./pkg/worker/controllers/worker/ -run '^TestRenderModelDeploymentArtifact' -v`

- [x] **T9 · ModelDeployment reconcile, placement and status**
      Blocked by: T6, T8
      Owns: `pkg/worker/controllers/worker/model_deployment.go`,
      `pkg/worker/controllers/worker/model_deployment_status*.go`,
      `pkg/worker/controllers/worker/model_deployment_rollout*.go`,
      `pkg/worker/controllers/worker/model_artifact_placement*.go`
      Gate: review
      Acceptance: the reconciler resolves artifact, claim, PV and StorageClass into the render
      input; creates no Pod in any waiting case (missing or unresolved artifact, the three PVC
      cases) and never deletes a running Pod for them; injects the bound PV's required node
      affinity at creation outside the fingerprint (a replica created before binding is not rolled
      after it); watches the ModelArtifacts and claims it depends on (a PV's affinity is immutable and its binding
      arrives as a claim update); writes `status.model` and
      `WeightsReady` per the tables and carries the waiting reason and an evicted Pod's message in
      the phase message.
      Verify: `go test ./pkg/worker/controllers/worker/ -run '^TestModelDeployment(ArtifactWait|ArtifactPlacement|WeightsReady)' -v`

- [x] **T10 · KV identity in the store key**
      Blocked by: T9
      Owns: `pkg/worker/kvcache/inject/**`, `pkg/worker/controllers/worker/model_deployment_binding*.go`,
      `pkg/worker/controllers/worker/model_deployment_connector.go` (the connector input; after T7)
      Gate: review
      Acceptance: the identity string per the rule; vLLM's store connector (alone and inside the
      composite with the point-to-point leg) carries `cache_prefix`; SGLang carries the
      `extra_backend_tag` extra configuration and still loads its store configuration from the
      environment; nothing changes without `artifactRef`; the SGLang ownership comment states when
      the extra configuration does and does not select a loader; a prefix holding one of the engines'
      key separators is refused.
      Verify: `go test ./pkg/worker/kvcache/inject/ -run '^TestRender(CachePrefix|BackendTag|RefusesACachePrefixWithASeparator)' -v` and `go test ./pkg/worker/controllers/worker/ -run '^TestModelDeploymentKVIdentity' -v`

- [x] **T11 · Instance model volume**
      Blocked by: T6, T9
      Owns: `pkg/worker/controllers/worker/instance*.go`, `pkg/worker/webhooks/worker/instance*.go`
      Acceptance: the `model` source is exclusive with the others, takes no `subPath`, is mounted
      read-only with the artifact's path; a reference to an existing Hugging Face artifact is
      refused and a later-discovered one reported; the PVC placement rules and the waits hold; the
      existing `persistent` path is unchanged.
      Verify: `go test ./pkg/worker/webhooks/worker/ ./pkg/worker/controllers/worker/ -run '^TestInstanceModelVolume' -v`

- [x] **T12 · Documentation**
      Blocked by: T9, T10, T11
      Owns: `docs/**`, `.agents/skills/gpustack-operator-docs/references/page-map.md`,
      `.agents/skills/gpustack-operator-overview/**`
      Acceptance: F8; the ModelDeployment reference page links the new page from its model section
      without adding a `##` heading.
      Verify: `make lint docs </dev/null`

- [x] **T13 · kind end-to-end cases**
      Blocked by: T11
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-97.sh`,
      `.agents/skills/gpustack-operator-e2e/cases/case-98.sh`, `.agents/skills/gpustack-operator-e2e/SKILL.md`
      Acceptance: the kind cases in the Test Plan pass with recorded evidence.
      Verify: `KUBECONFIG="$KCFG" bash .agents/skills/gpustack-operator-e2e/cases/case-97.sh <NS>` and the same for case 98

- [x] **T14 · GPU end-to-end cases**
      Blocked by: T12, T13
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-99.sh`,
      `.agents/skills/gpustack-operator-e2e/cases/case-100.sh`
      Gate: review
      Acceptance: the GPU cases in the Test Plan pass with recorded evidence, on an image whose
      digest matches the Pods' `imageID`.
      Verify: `bash .agents/skills/gpustack-operator-e2e/cases/case-99.sh <NS>` and case 100, with the GPU cluster's context


### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- The render golden tests of existing deployments stay unchanged; T8 adds the byte-for-byte
  check that a deployment without `artifactRef` renders what it rendered before.
- The ModelDeployment admission tests that pass `--served-model-name` with another value, if any,
  are updated to the new refusal.

#### Unit tests

Baselines measured on the base commit (`go test -cover`), with the target for this spec:

- `gpustack.ai/gpustack/pkg/modelartifact`: 2026-09-25 - new package, target 90%
- `gpustack.ai/gpustack/pkg/worker/controllers/worker`: 2026-09-25 - 82.6%, no decrease
- `gpustack.ai/gpustack/pkg/worker/webhooks/worker`: 2026-09-25 - 91.1%, no decrease
- `gpustack.ai/gpustack/pkg/worker/kvcache/inject`: 2026-09-25 - 91.5%, no decrease
- `gpustack.ai/gpustack/pkg/worker/settings`: 2026-09-25 - 0.0% (no test files); the new Settings
  are exercised through their readers' tests

Named behaviors, each a table case: the manifest mutations and the cross-implementation fixture;
every reason-table row; the revalidation confirmation and the outage that never revokes; the
finalizer with a referencing deployment, a referencing Instance and none; every waiting case
creating no Pod; the PV affinity outside the fingerprint; the ephemeral-storage limit; the KV
identity present with `artifactRef` and absent without; the served-name refusal on both engines.

#### Integration tests

None beyond the fake-client controller tests: the repository has no envtest suite, and the
behaviors that need a real API server, kubelet or Kueue are in the end-to-end cases.

#### e2e tests

kind (no accelerator; replicas are not expected to serve, every asserted value is on an object):

- **case-97 · ModelArtifact resolution and admission.** A public repository resolves by branch and
  by tag to the `git ls-remote` commit, with a digest equal to one computed by the reference
  script from the same tree; the private test repository is `AccessDenied` without a Secret and
  `Resolved` with one; a gated repository without a Secret is `AccessDenied`; replacing the
  Secret's token with an invalid one flips `Resolved=False` after the confirming check while a
  referencing Pod created before keeps running and no new Pod is created; the invalid-token
  Warning appears on a public repository; every F1 refusal fires; deleting a referenced artifact
  stays `Terminating` until its last reference is gone; a scan of the artifact's status, events
  and the worker log for the token value finds zero matches, after a planted sample is found.
- **case-98 · Consumers.** A ModelDeployment and an Instance on a PVC artifact backed by a
  node-pinned local PV get the PV's required node affinity and are placed on its node; a replica
  created before a WaitForFirstConsumer claim binds is not rolled after it; a static
  no-provisioner claim and an immediate-binding pending claim create no Pod and report
  `ClaimNotBound`; a multi-replica deployment on a ReadWriteOnce claim reports
  `AccessModeConflict`; a missing artifact creates no Pod and resolves once created; an Engine
  deployment's Pod carries the argv, owned environment, cache volume `sizeLimit` and raised
  ephemeral-storage limit of the tables, with no token value in the Pod spec; the served-name,
  owned-key and reserved-path refusals fire; an Instance naming a Hugging Face artifact is refused.

The version floor has evidence of its own: case-97 and the accelerator-free part of case-98 run
twice on kind, once with a Kubernetes 1.29 node image and once with the current default.

GPU cluster (one node with two accelerators or two single-accelerator nodes, plus a KV cache pool
for case 100):

- **case-99 · Serving.** vLLM on a PVC artifact behind `llm-d-router`: `/v1/models` is
  `spec.model.name`, the metrics' `model_name` is that value, a routed chat request succeeds and the
  router's token producer logs no failure. SGLang with Engine delivery of a gated test repository whose
  pinned commit is a copy of a small public model and whose branch head carries a modified
  tokenizer, read with a token from a Secret: the cache holds only the resolved commit, the
  tokenizer file's hash equals the pinned commit's rather than the branch head's, and a chat
  request succeeds. The other GPU deployments use public models. The cache volume's usage stays under its
  `sizeLimit` and the ratio is recorded.
- **case-100 · KV identity.** Two vLLM deployments of the same artifact on one Binding hit each
  other's blocks; a deployment of a different commit on the same Binding does not; each side has a
  must-hit and a must-miss control, measured by the engine's `external_prefix_cache_hits_total`,
  every send proven by `request_success_total`. The same pair on SGLang runs if the GPU window allows, and its
  absence is recorded otherwise.


## Alternatives

### Inline the source in each role's volume

Every role of a prefill/decode deployment would repeat it and admission would have to prove the
copies equal; sources and credentials would spread across objects. Rejected in favor of one
deployment-level reference.

### A provisioning block under `spec.model`

The source, revision and credential inside the deployment. This is the general-purpose serving
resource the type comment deliberately refuses, and it would make the deployment a credential
holder. Rejected.

### Restore a general `persistent` role volume

A PVC volume source on roles would bring back the mount without identity, status or placement
rules, and a second PVC path beside the artifact. Rejected: the PVC is a source of the artifact.

### Let server-only deployments change `artifactRef`

It would roll every role like an image change. Rejected for now because `spec.model` is already
frozen by the identity rule and a prefill/decode deployment must never mix weights across a KV
handoff; relaxing one field of `spec.model` for one shape is a later, additive decision.

### Refuse at admission when a PVC cannot be placed

Admission cannot see a claim's binding reliably (it changes after admission and may not exist yet),
so a refusal would be order-dependent. Rejected in favor of waiting in status without creating
Pods, which is also what the artifact reference does.

### Per-deployment Bindings instead of a key prefix

Refusing several digests under one Binding would isolate through the store tenant, but it depends
on the pool's master holding a tenant ledger and makes a user create a Binding and quota per
weight version. Rejected in favor of the prefix, which also fixes the local-path collision.

### A digest source field and a Verified condition now

`digestSource` would be a function of the source kind in this version, and `Verified` could never
be `True` before node delivery verifies content. Both are additive later.

### A ModelScope endpoint Setting now

It has no reader until ModelScope opens. Added with the opening.

## Open Questions

None.
