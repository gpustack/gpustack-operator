# Spec: Node Model Store and the Model CSI Node Plugin

Status: Shipped
Type: Feature

## Summary

GPUStack Operator gains a node component, `model-manager`, that delivers a Hugging Face
`ModelArtifact`'s weights from a node-local cache instead of having every engine Pod download them.
It is a vendor-neutral CSI node plugin serving inline ephemeral volumes: on a Pod's first mount of
an artifact on a node it downloads the files the artifact's manifest names, verifies every byte
against that manifest while downloading, publishes the verified tree atomically, and bind-mounts
it read-only; every later mount of the same content on that node is a constant-time check and a
bind mount, with zero bytes downloaded. A mount is authorized only by a resolved `ModelArtifact` in
the Pod's own namespace, never by the volume's attributes. A cluster-scoped `NodeModelStore` per
node carries both halves of the node's contract: its `spec` is the effective configuration the
worker computes from Settings (watermarks, download concurrency and bandwidth, Hub endpoint, proxy,
CA), and its `status` is what the plugin reports about the node (which content is ready, failing or
downloading, and the cache's capacity). A new delivery mode `Node` renders the plugin's volume into
`ModelDeployment` replicas in place of the engine's own download, and an `Instance` can now mount a
Hugging Face artifact. Download progress, placement preference, node-to-node sync and prefetch are
later work that builds on the per-node state defined here.

## Motivation

### Goals

- A second replica, a restart, or another deployment of the same weights on a node downloads zero
  bytes and starts without recomputing any hash.
- No consumer ever sees a partially written or unverified tree, whatever fails: the network, the
  Hub, the disk, or the plugin itself mid-download.
- A tenant can mount only content that a resolved `ModelArtifact` in their own namespace names. A
  hand-written Pod that forges the volume's attributes (another namespace's artifact, another
  digest) is refused, and a tenant cannot make an artifact look resolved.
- A tenant's Hub token is sent only with requests for that tenant's own repository, never
  persisted, and never appears in any status, event, log, metric or node ledger.
- The cache never pushes a node into kubelet disk-pressure eviction or image garbage collection:
  it stays under administrator-set watermarks, and only content no Pod references is ever
  removed.
- An administrator configures the node side in one place (Settings), and can read, per node, the
  configuration the plugin actually applied and what the node holds.
- A default installation, and a tenant namespace that enforces Pod Security Admission
  `restricted`, can use node delivery; every path works on Kubernetes 1.29.
- Deployments and Instances say in their status why weights are not mounted yet, on the object the
  user is looking at.

Measurable success criteria are listed per feature under Core Features & Acceptance Criteria.

### Non-Goals

- Download progress in `NodeModelStore` (`downloadedBytes`, `source`), aggregation of node counts
  into `ModelArtifact`, the `ModelArtifact` `Verified` condition, a `progress` subresource and the
  v1 aggregated views of both resources. They are the next spec's, which reads this spec's state.
- Placement preference toward nodes that hold the weights. Node delivery places Pods exactly as
  before; a cold node downloads.
- Fetching from other nodes. Every download in this spec comes from the Hub, directly or through
  the administrator's proxy.
- Prefetch, retention, pinned content, per-pool configuration (`ModelStore`) and tenant disk
  budgets.
- A periodic scrub of published trees. Published trees have one writer and are mounted read-only;
  the rule a scrub must follow when it is added is recorded under Notes.
- An external provider (Dragonfly, a customer platform) writing into the cache, and with it the
  question of publishing before an asynchronous verification. Every byte in this spec is hashed as
  it is downloaded, so verification always precedes publication.
- ModelScope. The source stays refused, as in the ModelArtifact spec.
- Node delivery for a PersistentVolumeClaim source, which is always mounted directly.
- A per-deployment choice of delivery. Delivery is a cluster Setting.

## Proposal

### Components and responsibilities

| Component | Owns | Never |
| --- | --- | --- |
| `model-manager` DaemonSet (one Pod per node, any vendor) | mount authorization, manifest recomputation, download, verification, atomic publication, read-only mounts, the reference ledger, garbage collection, `NodeModelStore.status` of its own node | reads a Secret, a Setting or another node's object; sends a token for another namespace's repository |
| worker | creating each node's `NodeModelStore`, computing and writing its `spec`, refusing status writes from the wrong writer, rendering Node delivery into consumers | downloads, touches node disks |
| kubelet | calling the plugin with the Pod's identity and the artifact's Secret on every mount attempt, retrying with backoff | — |

The plugin is its own DaemonSet rather than part of `device-manager`: the device manager is
deployed once per accelerator manufacturer, is absent from CPU nodes, would have several writers on
a mixed node, and upgrading it rolls the device plugin.

### Delivery mode Node

A new Setting, `model-artifact-delivery-mode`, chooses how a Hugging Face artifact's weights reach
a `ModelDeployment`'s engine: `Engine` (the engine downloads the resolved commit itself, as today)
or `Node` (the plugin materializes and mounts them). A PVC artifact is always mounted directly.

For every role that does not replace its command line, Node delivery renders:

```yaml
volumes:
  - name: gpustack-model
    csi:
      driver: model.csi.gpustack.ai
      readOnly: true
      volumeAttributes:               # hints only; the plugin trusts none of them
        artifact: qwen-7b
        artifactUID: 3f0c...
        manifestDigest: sha256:...
      nodePublishSecretRef: {name: hf-token}   # the artifact's secretRef; omitted without one
containers:
  - volumeMounts: [{name: gpustack-model, mountPath: /var/lib/gpustack/model, readOnly: true}]
```

The command line is the local-path form the PVC source already uses (`vllm serve
/var/lib/gpustack/model`, SGLang `--model-path /var/lib/gpustack/model`, the defaulted
`--served-model-name <spec.model.name>`). No Hub environment is added: the engine reads a local
directory. The Engine delivery's cache `emptyDir` and raised ephemeral-storage limit are not
rendered, because an inline CSI volume's bytes are not the Pod's ephemeral storage. A take-over
role gets the volume and mount and nothing else, as it does for a PVC. `status.model.delivery` is
`Node`.

An `Instance` `model` volume naming a Hugging Face artifact is now accepted and rendered the same
way at the entry's `mountPath`, whatever `model-artifact-delivery-mode` says: the Setting chooses
between two deliveries, and an Instance has only this one, because it has no engine to download.

**When the plugin is absent.** Node delivery needs the `model.csi.gpustack.ai` CSIDriver object.
While it does not exist, a consumer that would render Node delivery creates no Pod and reports
`WeightsReady=False` with reason `NodeDeliveryUnavailable` (an Instance says the same in its phase
message); running Pods are not touched. Writing `Node` into the Setting while the CSIDriver does
not exist is refused by the Setting's admission with the same explanation. The consumer check is
what catches the cases admission cannot see: a value seeded from the environment, and a plugin
uninstalled later.

**Switching delivery.** The Setting is explicit and never inferred from whether the plugin is
installed. Changing it changes every Hugging Face deployment's rendered Pod, so each rolls once, the
way an image change does. During the roll a replica on the old delivery and one on the new serve
the same weights (the same digest, the same served name, the same KV identity); vLLM's store key
also contains the last path segment of `--model`, which differs between the two (`model` against
the repository's name), so KV blocks written before the switch are not hit after it. This is
documented.

**Seeding.** The Setting defaults to `Engine`. The chart seeds `Node` (through
`GPUSTACK_MODEL_ARTIFACT_DELIVERY_MODE`) when it deploys the plugin. A seed fills only a key the
Settings store does not have yet, so a cluster upgraded from a version without this Setting gets the
chart's seed once, and a later `helm upgrade` never overrides an administrator's choice. Image mode
seeds nothing, so its default stays `Engine`.

**Upgrading from the version before this one.** With the plugin enabled, that upgrade seeds `Node`
and every `ModelDeployment` on a Hugging Face artifact rolls once, to Node delivery. An
administrator who does not want the roll sets `model-artifact-delivery-mode` to `Engine` before
upgrading (a stored value is never overridden by a seed), or upgrades with
`modelManager.enabled=false`. The upgrade notes of the operations page state both, not only this
section.

### Filtering: allowPatterns and ignorePatterns

`ModelArtifact.spec` gains two optional lists, the fields the canonical manifest format already
defines the semantics of (rule 5 of format v1: `fnmatchcase`, `*` and `?` cross `/`, a trailing `/`
becomes `<pattern>*`, an empty allow list keeps everything, ignore wins over allow):

```yaml
spec:
  source:
    huggingFace: {repository: meta-llama/Llama-3.1-8B-Instruct, secretRef: {name: hf-token}}
  allowPatterns: ["*.safetensors", "*.json", "tokenizer*"]
  ignorePatterns: ["original/"]
```

- Hugging Face sources only; admission refuses them on a PVC source. At most 32 patterns per
  list, each 1 to 256 characters, no control characters. Immutable with the rest of `spec`.
- Resolution filters the tree before canonicalization, so `manifestDigest`, `fileCount` and
  `sizeBytes` describe the filtered set. An artifact that sets no pattern keeps exactly the digest
  it has today. A filter that keeps no file is `Resolved=False`, reason `EmptyManifest`.
- Node delivery downloads exactly the filtered manifest. Engine delivery cannot honor a filter (the
  engine chooses its own files, and the cache would be sized to the filtered total), so a
  deployment on a filtered artifact under Engine delivery creates no Pod and reports
  `WeightsReady=False` with reason `FilterNeedsNodeDelivery`.

Filtering is what makes node delivery affordable for common repositories: many carry both
`.safetensors` and `.bin` copies, or an `original/` directory of consolidated checkpoints, which
otherwise double the bytes downloaded and kept.

### The NodeModelStore resource

```yaml
apiVersion: worker.gpustack.ai/v1alpha1
kind: NodeModelStore
metadata:
  name: gpu-node-01                          # the Node's name
  ownerReferences: [{apiVersion: v1, kind: Node, name: gpu-node-01, uid: ...}]
spec:                                        # written by the worker: this node's effective configuration
  watermarks: {highPercent: 80, lowPercent: 70}
  download: {concurrency: 8, bytesPerSecond: 0}      # 0 = unlimited
  hub:
    huggingFaceEndpoint: https://huggingface.co
    httpsProxy: ""
    noProxy: ""
    caBundleConfigMap: ""                    # a ConfigMap in the operator's namespace, key ca.crt
status:                                      # written by the plugin: this node's facts
  observedGeneration: 3                      # the spec generation the plugin applies
  capacity:
    totalBytes: 999641755648                 # the cache filesystem's size
    storedBytes: 15231233024                 # published trees and partial downloads
    usedPercent: 40                          # the filesystem's usage, rounded down to a multiple of 5
  models:                                    # a map keyed by digest, at most 256 entries
    - digest: sha256:...
      state: Ready                           # Downloading | Ready | Failed
      sizeBytes: 15231233024
      referenced: true                       # some Pod mounts it; no Pod or tenant is named
      lastUsedTime: "2026-09-25T06:00:00Z"   # truncated to the hour
      reason: ""                             # Failed: a reason from the table below
      message: ""
      retryTime: null                        # Failed: the earliest next attempt
  conditions:
    - type: Ready                            # the plugin is serving and applies spec's generation
    - type: CapacityLow                      # above the high watermark with nothing it may remove
```

It is cluster-scoped, one per node, named after the Node, in `worker.gpustack.ai/v1alpha1`, a CRD
with a `status` subresource. Short name `nms`, category `gpustack`. Printer columns: Ready, Used
(`status.capacity.usedPercent`), Age; a count of models cannot be a printer column, which takes one
JSON path. The shape is
Kubernetes' own `Node`: the control plane writes `spec`, the node component writes `status`, and
`status.models` reports content the way `Node.status.images` reports images.

- **Creation.** The worker creates a node's object when that node's `CSINode` lists the driver
  `model.csi.gpustack.ai`, so the object's existence is evidence, from kubelet's own record, that
  the plugin registered there. The owner reference to the Node lets the garbage collector delete
  it with the Node. The worker does not delete it when the driver leaves `CSINode`: a plugin
  restart or a rolling upgrade unregisters the driver for a moment, and deleting on that would
  erase and rewrite the object on every upgrade. A node the plugin no longer runs on keeps a stale
  object whose `Ready` condition stops being refreshed; the documentation says so. The component's
  removal is a cluster-level signal instead: while the CSIDriver object does not exist (the
  component disabled), the worker deletes every `NodeModelStore`, and an uninstall with the chart's
  cleanup hook removes the CRD with its objects, as it does for every CRD of this group.
- **No tenant names.** It is cluster-scoped, so it never carries a namespace, an artifact name, a
  Pod or a repository: a digest and sizes only. Writing a tenant's names into it would leak them
  across tenants.
- **Write budget.** The plugin writes status on a state change of an entry, a flip of
  `referenced`, a change of the hour in `lastUsedTime`, a change of `usedPercent` or `storedBytes`,
  a condition change, and a new `observedGeneration`; nothing else. A node whose content is not
  changing writes nothing.
- **Size.** 256 entries of a few hundred bytes keep the object far below 100 KB. When more content
  is on disk than fits, the entries kept are the referenced ones and then the most recently used;
  the omission is counted in the `Ready` condition's message.
- **Restart.** A plugin that starts rewrites its status from what is on disk and what is mounted,
  never from the previous status, so the object is always reconstructible.

#### Only the plugin on a node writes that node's status

The plugin's RBAC lets it update `nodemodelstores/status`, and RBAC cannot restrict that to one
object name, so a validating webhook on the `status` subresource of `nodemodelstores` admits an
update only when:

1. the requester is the plugin's ServiceAccount, `system:serviceaccount:<operator namespace>:<plugin
   service account>`; and
2. the requester's token is bound to a Pod, whose name and UID the API server places in the
   request's `userInfo.extra` (`authentication.kubernetes.io/pod-name` and `pod-uid`, present for
   every bound service-account token since Kubernetes 1.22); and
3. that Pod exists in the operator namespace with that UID, and its `spec.nodeName` equals the
   object's name. On Kubernetes 1.30 and later the extra also carries
   `authentication.kubernetes.io/node-name`, and the webhook compares that directly and skips the
   Pod lookup.

A request without those extras (a legacy, non-bound token) is refused, so the guarantee does not
silently weaken. The guarantee is the same on 1.29 and on later versions; 1.30 only removes a Pod
read. `spec` and the object itself are written by the worker, and no role in the chart grants
tenants anything on this resource. The webhook's `failurePolicy` is `Fail`: a guard that let writes
through while the worker is unreachable would guard nothing. Each refusal names the rule that
refused it (not the plugin's identity, no Pod binding, a Pod on another node), so a test can assert
which rule fired rather than only that the write failed.

#### Only the worker writes a ModelArtifact's status

Mount authorization trusts `ModelArtifact.status`, so it is only as strong as the rule that tenants
cannot write it. [Searched] no role in the chart or its bundled subcharts grants anything on
`worker.gpustack.ai` resources, and none aggregates into `admin` or `edit`; but the ModelArtifact
spec did not guarantee it, and a cluster administrator's broad role would silently void it. So the
ModelArtifact webhook gains the `status` subresource: an update of `modelartifacts/status` is
admitted only from the worker's own identity, which the worker learns at startup with a
`SelfSubjectReview` (so it holds in image mode, where the worker may run with a kubeconfig user
rather than a ServiceAccount). An API server that does not serve it (before 1.28) is asked with a
`TokenReview` of the worker's own bearer token instead; with neither, the worker does not start,
rather than run with the rule off. The main resource needs no rule: with a status subresource the API
server ignores `status` on create and update of the main resource. This webhook's `failurePolicy`
is `Fail` as well, and its refusal names the rule.

### Effective configuration

Configuration is layered, and each lower layer may override only fields the layer above allows.
The merge happens in one place, the worker, and its result is each node's `NodeModelStore.spec`;
the plugin reads only that object (and the CA ConfigMap it names). In this spec there is one runtime
layer, the cluster Settings; a per-pool layer is later work, so the merge is written from the start
as a field-level overlay of an ordered list of layers, each of which may leave any field unset.

| Layer | Carrier | Who changes it | What | When it takes effect |
| --- | --- | --- | --- | --- |
| Deploy time | chart values (image mode: the worker's overlay) | installer | component switch, cache root path, kubelet directory, images, DaemonSet resources and placement, the delivery-mode seed | `helm upgrade` rolls the DaemonSet; existing mounts are unaffected |
| Cluster runtime | Settings in the operator namespace | administrator | delivery mode, Hub endpoint, proxy, CA, watermarks, download concurrency and bandwidth | the worker rewrites every `NodeModelStore.spec` within one minute; the plugin applies it to the next download and the next collection |
| Object | `ModelArtifact` | tenant | source, revision, filter, Secret | immutable |

Endpoint, proxy, CA, watermarks and bandwidth exist on no tenant-writable object: a tenant who could
choose where a privileged node process connects could make it request any address.

**Settings added by this spec** (kebab-case, seeded once from `GPUSTACK_*`):

| Setting | Default | Meaning | Checked at use |
| --- | --- | --- | --- |
| `model-artifact-delivery-mode` | `Engine` (the chart seeds `Node` with the plugin) | how a Hugging Face artifact reaches a ModelDeployment | `Engine` or `Node`; `Node` needs the CSIDriver |
| `model-store-high-watermark` | `80` | filesystem usage percent above which the plugin removes unreferenced content | integer, `lowWatermark < high <= 95` |
| `model-store-low-watermark` | `70` | the usage percent collection removes down to | integer, `1 <= low < high` |
| `model-store-download-concurrency` | `8` | concurrent HTTP requests per node, across all downloads | integer, 1 to 64 |
| `model-store-download-bandwidth` | `0` | per-node download rate limit, a quantity of bytes per second (`200Mi`); `0` is unlimited | a non-negative quantity |

The existing `model-artifact-huggingface-endpoint`, `model-artifact-https-proxy`,
`model-artifact-no-proxy` and `model-artifact-ca-bundle` also feed `spec.hub`, so the controller's
resolution, the engine's download and the plugin's download reach the same Hub the same way.

**A Setting's default does not pass admission.** Admission runs on a write through the API, but a
default is read from a `GPUSTACK_*` variable. Every value above is therefore checked again where it
is used: the worker refuses to start on an invalid value taken from the environment (as it already
does for the proxy), and never writes a value into `NodeModelStore.spec` that fails its check; the
plugin checks `spec` again before using it (a hand edit of the object also bypasses the worker), and
refuses to start a download while its configuration is invalid, with `Ready=False`, reason
`InvalidConfiguration`, rather than falling back to a value nobody chose.

**Watermarks and kubelet.** Watermarks are percentages of the cache filesystem's usage by
everything on it, not only the cache. When the cache root shares a filesystem with the kubelet root
directory, the high watermark must stay below kubelet's hard eviction threshold and image garbage
collection threshold with room to spare, or the cache would make kubelet evict serving Pods or
delete engine images. The recommended layout is a dedicated filesystem mounted at the cache root,
where the Setting applies as written. On a shared filesystem (the two paths report the same device)
the plugin caps the effective high watermark:

- It reads three values from the kubelet configuration file at `<kubeletDir>/config.yaml` (where
  kubeadm writes it): `evictionHard["nodefs.available"]`, `evictionHard["imagefs.available"]`, and
  `imageGCHighThresholdPercent`. A threshold given as a quantity is converted to a percentage of the
  filesystem's size.
- The cap is the lowest of `100 - nodefs.available - 5`, `100 - imagefs.available - 5`, and
  `imageGCHighThresholdPercent - 5`, where 5 is the margin. The image values count because the
  plugin cannot see whether the image store is on the same filesystem, so it assumes it is.
- A value the file does not set, a file that is absent, and a file the plugin cannot parse all take
  kubelet's default: `nodefs.available` 10%, `imagefs.available` 15%, image collection from 85%.
  With the defaults the cap is 80. The `Ready` condition's message says whether the cap came from
  the file or from the defaults. Kubelet flags that override the file are not seen; the
  documentation says so.

This cap is this spec's rule. The real-node acceptance (a cache filled to the watermark with no
`DiskPressure` and no image collection) belongs to the progress spec, which may change the rule on
its evidence.

### Mount authorization

On every `NodePublishVolume`, before touching the disk, the plugin requires all of:

1. The volume is an inline ephemeral volume (`csi.storage.k8s.io/ephemeral=true`) with a mount
   capability and a target path under the kubelet directory.
2. A `ModelArtifact` named by the `artifact` attribute exists in the Pod's namespace
   (`csi.storage.k8s.io/pod.namespace`).
3. Its UID equals `artifactUID`, its source is Hugging Face, `Resolved=True`, and
   `status.resolved.manifestDigest` equals `manifestDigest`.

The namespace is the one kubelet adds because the CSIDriver sets `podInfoOnMount`. A tenant cannot
forge it: kubelet merges the Pod-information keys over the Pod's own `volumeAttributes`, so a
`csi.storage.k8s.io/pod.namespace` written by the tenant is overwritten ([read] Kubernetes v1.29.0
`pkg/volume/csi/csi_mounter.go:233-234` and `mergeMap` at `:607-614`). Everything else in the
attributes is a hint that must agree with the API. The digest is a pure content address and is never
authorization by itself: a namespace mounts content only through its own artifact, resolved with its
own credential.

The plugin reads artifacts from an informer of `modelartifacts` (cluster-wide get, list and watch),
never with a GET per call, and answers `Unavailable` until the informer has synced. A refusal is
`PermissionDenied` with a message naming the failed rule. An artifact that stops being resolved (a
revoked token, a deleted artifact) stops new mounts only; mounted Pods keep their mount.

The plugin reads `spec.source.huggingFace.repository` and `spec.allowPatterns` and
`spec.ignorePatterns`, which are immutable after creation, and `status.resolved`, which only the
worker writes.

### Materialization

When a node does not hold the digest, the first authorized mount starts one background
materialization per digest and returns `Aborted` at once; kubelet retries the mount with its backoff
(0.5 s doubling, capped at 2 min 2 s; one call times out at 2 min: [read] Kubernetes v1.29.0
`pkg/util/goroutinemap/exponentialbackoff/exponential_backoff.go:31,37` and
`pkg/volume/csi/csi_plugin.go:53`), and a later call mounts the published tree. A download cannot run
inside one call: the call's timeout is two minutes. Measured on kubelet 1.35.7: a 480-second cold
materialization downloaded once for two Pods retrying together, and the first call after publication
mounted.

1. **Manifest.** The plugin lists the tree at `status.resolved.revision` from the Hub with the
   mount's credential, applies the artifact's filter, and builds the canonical manifest with the same
   code the controller uses. Its digest must equal `manifestDigest`, else the attempt fails with
   `IntegrityMismatch`. The manifest's paths already satisfy format v1's path rules (no absolute
   path, no empty, `.` or `..` segment, no control character), and only regular files are created:
   no symbolic link, device or other special file.
2. **Capacity.** Before downloading, the plugin reserves the manifest's size against the high
   watermark. If the projected usage would exceed it, collection runs first (below); if it still
   would, the attempt fails with `InsufficientCapacity`.
3. **Download.** Files come from `{endpoint}/{repository}/resolve/{commit}/{path}`, the Hub
   redirects included, through the configured proxy and CA. At most `download.concurrency` requests
   run on the node across all downloads, sharing one `bytesPerSecond` limit. A large file is
   fetched as byte ranges in parallel, because a single connection was measured to set the whole
   download's wall time (in eight downloads of a 15 GB model on one node, the slowest single file's
   single connection accounted for all but 0.1 to 4 seconds of each download's time). A range that
   makes no progress for 30 seconds is retried from its last received byte; a file whose ranges fail
   repeatedly fails the attempt with `SourceUnavailable`.
4. **Verification while downloading.** Each file's hash is computed in byte order as its bytes
   arrive: `sha256` for an LFS file, the git blob SHA-1 (`blob <size>\0` followed by the content)
   otherwise, and its size must match. Ranges are issued only within a bounded window ahead of the
   hashed position, so the bytes the hash reads back are still in memory; a download is never read
   a second time from disk. This is why verification costs little: on a node whose disk sustained
   0.47 GB/s, reading a finished download again to hash it added 44 to 46% to the time of a download
   at the site's bandwidth, while one Go SHA-256 thread hashes 1.16 GB/s, several times the rate one
   connection delivers. Resuming after a restart restores a saved hash state or re-hashes the part
   already on disk; bytes that were never hashed are never trusted. A mismatch fails the attempt
   with `IntegrityMismatch` and discards the file.
5. **Publication.** The files are written under the attempt's partial directory and each is synced.
   Then a marker recording the digest, the attempt and the manifest format version is written and
   synced, the partial directory is synced, renamed to the published name in one step, and the
   parent directory is synced. A tree without its marker is not published, and a consumer is only
   ever mounted from a published tree. Measured: publication took 1.7 ms at the median and 6.9 ms at
   worst over 200 small trees, and 3.3 ms at the median for a 141 GB tree.
6. **Mount.** Later calls see the marker (one `stat`), bind-mount the tree read-only onto the target,
   and record the reference. They do not hash, list, or contact the Hub, so a Hub outage or an
   air-gapped node still mounts content it holds.

**One writer per digest.** A digest has at most one attempt at a time on a node; concurrent mounts
of it join the running attempt. The attempt number is local to the node and increases per digest;
it fences a late write from a superseded attempt and names the partial directory.

**Credentials stay with their namespace.** kubelet hands the artifact's Secret to every call, so a
rotated token reaches the next request. The running attempt uses a credential only for its own
repository: when mounts from several namespaces join one digest (the same content, each namespace
authorized by its own artifact), each credential is kept with the repository of the artifact that
presented it, and a request is always sent with the credential of the repository it names. When the
first source's credential is refused, the attempt tries the next source with its own credential. A token
is held in memory for the attempt's life and dropped when the attempt ends; it is never written to
disk, to the ledger, to status, to an event or to a log line, and it is never sent on a redirect to
another host or from `https` to `http`.

**Failure backoff per digest.** kubelet retries every failed mount, and without a limit each retry
would start a full download again (measured: five retries against a corrupting source were five
complete downloads). After a failed attempt the digest enters `Failed` in `NodeModelStore.status`
with its reason, message and `retryTime`, and mounts of it return at once without downloading until
`retryTime`: one minute after the first failure, doubling to at most one hour, reset by a success.
The backoff is kept in the node's ledger, so a plugin restart does not reset it. `AccessDenied` is
retried the same way, because the Secret a tenant fixes is only seen on the next call.
`InvalidRequest` and `Canceled` do not back off: a configuration the node cannot use yet changes by
itself when the worker writes the node's spec, and a backoff would hold every mount for a minute
after it did.

**No waiters, no download.** kubelet stops calling once the Pod that mounts the volume is deleted.
An attempt that no mount has asked for in five minutes (more than kubelet's longest retry
interval of 2 min 2 s) is canceled with reason `Canceled`; its partial files stay for a later
resume and are collected like any partial.

### Unmount, references and restart

`NodeUnpublishVolume` works from the target path alone and is idempotent: it unmounts the target
if it is a mount point, removes the ledger entry, and succeeds for a target it never mounted
(kubelet calls it for a mount it had marked uncertain). A reference is the pair (volume ID, target
path); the digest a target mounts is read from the bind mount's root in `/proc/self/mountinfo`, and
the ledger adds only the Pod's identity. On start the plugin rebuilds its references from
mountinfo and the ledger: an entry in both is kept, one only in mountinfo is adopted, one only in the
ledger is dropped, and one whose digest differs from what mountinfo shows mounted is corrected to it.
A mount records its reference before it binds, under the lock collection removes trees under, so a
tree is never removed between the check that it is published and the mount, and a reference that
cannot be written fails the mount. It removes partial directories that belong to no attempt, and rewrites
`NodeModelStore.status` from disk. A Pod keeps running while the plugin restarts or upgrades: its
mount is a kernel bind mount, independent of the plugin process.

### Garbage collection

Collection removes only published trees with no reference that have not been referenced for ten
minutes (a Pod being replaced keeps its content), oldest `lastUsedTime` first, and partial
directories with no running attempt that no mount has asked for in 24 hours, and failure records whose
digest is no longer published, on disk or being materialized, 24 hours after the last failure. It
never removes a referenced tree or a partial being written. It runs when usage exceeds the high watermark (down to
the low one), every five minutes, and when a reservation does not fit. Usage that stays above the
high watermark with nothing removable sets `CapacityLow=True`. Nothing about collection is ever
decided from API state: references come from the node's own mounts. References that cannot be
read, and a node whose configuration has not been applied yet (there are no watermarks), are never
taken as "nothing is mounted": the collection removes no tree and the `Ready` message says it was
skipped and why.

### Status of consumers

`WeightsReady` gains reasons for Node delivery, read from the Pod and from the node's
`NodeModelStore`, never from events:

| Status | Reason | When |
| --- | --- | --- |
| False | `NodeDeliveryUnavailable` | the CSIDriver does not exist; no Pod is created |
| False | `FilterNeedsNodeDelivery` | a filtered artifact under Engine delivery; no Pod is created |
| False | `Materializing` | a created Pod is not yet `PodReadyToStartContainers` and its node's store lists the digest `Downloading` |
| False | `MaterializationFailed` | the same, and the store lists it `Failed`; the message carries the store's reason, message and retry time |
| False | `WeightsNotMounted` | not yet `PodReadyToStartContainers`, and the store says neither (the plugin has not reported, or refused the mount) |
| True | `Mounted` | every created Pod is `PodReadyToStartContainers` |

A cold mount's progress appears, as bytes of total, only in the mount error kubelet records on the
Pod; progress in the API is later work. After a download completes, a Pod starts at kubelet's next
mount retry, up to about two minutes later (measured worst case 119.6 seconds after publication);
the plugin cannot shorten it, and the documentation says so. If an administrator enables Kueue's
`waitForPodsReady`, a cold download that outlasts its timeout evicts and requeues the replica, as
the Engine delivery already documents.

### Error classification

The plugin classifies every failed attempt with one reason, used in `NodeModelStore.status`, in
metrics and in the mount error message; the message is for people and nothing parses it.

| Reason | Meaning | What happens |
| --- | --- | --- |
| `InvalidRequest` | the configuration cannot be executed (an invalid endpoint, proxy or CA) | waits for the configuration to change |
| `AccessDenied` | the Hub refused the credential | backoff; the next call brings the Secret again |
| `SourceUnavailable` | the Hub is unreachable, answers 5xx or 429, or a file's ranges keep failing | backoff |
| `IntegrityMismatch` | a file's hash or size, or the manifest's digest, does not match | the file is discarded; backoff |
| `InsufficientCapacity` | the reservation does not fit under the high watermark after collection | backoff; collection may free room |
| `Canceled` | no mount asked for the digest for five minutes | resumes on the next mount |

A later spec's peer reason (`BackendUnavailable`) joins this table without changing any row. The
mount's gRPC code: `InvalidArgument` for a malformed request, `PermissionDenied` for authorization,
`Unavailable` before the informer syncs and during backoff, `Aborted` while materializing,
`ResourceExhausted` for capacity, `Internal` otherwise. No message, event or log line carries a
token, an `Authorization` header or a signed URL.

### Observability

The plugin serves Prometheus metrics, readiness and liveness on its HTTPS port, the way the device
manager does:

- `gpustack_model_manager_download_bytes_total` (label `source="hub"`), the bytes received;
- `gpustack_model_manager_mounts_total` (label `result="hit|materialized|denied|pending"`);
- `gpustack_model_manager_materializations_total` (label `result`, the reason table's reasons or
  `published`) and `gpustack_model_manager_publish_duration_seconds`;
- `gpustack_model_manager_gc_removed_bytes_total`, and gauges of stored bytes and capacity.

Labels never carry a namespace, an artifact, a repository or a Pod.

### Deployment

**Chart.** A `modelManager` values block beside `deviceManager`:

```yaml
modelManager:
  enabled: true
  rootPath: /var/lib/gpustack/models   # the node's cache directory, a hostPath
  kubeletDir: /var/lib/kubelet         # the convention the bundled CSI drivers follow
  securePort: 32444
  image: {}                            # defaults to the operator image
  registrar:                           # the same mirrored image the bundled CSI drivers pin
    image: {repository: gpustack/mirrored-csi-node-driver-registrar, tag: v2.17.0}
  resources: {}
  nodeSelector: {}
  tolerations: [{operator: Exists}]
  priorityClassName: system-node-critical
```

It renders the CSIDriver (`attachRequired: false`, `podInfoOnMount: true`,
`volumeLifecycleModes: [Ephemeral]`, `fsGroupPolicy: None`), a DaemonSet running `gpustack-operator
model-manager` with the `node-driver-registrar` sidecar, a privileged plugin container with
`mountPropagation: Bidirectional` on the kubelet directory (its pods directory holds the targets,
its plugins directory the socket, and its `config.yaml` the thresholds the watermark cap reads), and
without `hostPID`, `hostIPC` or `hostNetwork`, a
ServiceAccount, and a ClusterRole holding only: get, list and watch on
`modelartifacts` and `nodemodelstores`; update on `nodemodelstores/status`; get, list and
watch on ConfigMaps in the operator namespace (the CA). It has no access to Secrets, Settings, Pods
or anything else, unlike the device manager's `cluster-admin`. The registrar image is the one the
bundled NFS and S3 drivers already pin, so no dependency pin changes. The `values.schema.json`
annotations constrain the new keys. With `modelManager.enabled` the worker's Deployment receives
the delivery-mode seed and the plugin's ServiceAccount name, which the status guard checks.

**Image mode.** `--disable-applications` gains the key `model-manager` (values key
`modelManager`), and the worker's overlay switches it like every other component.

**Namespace.** The plugin is privileged, so the operator namespace must admit privileged Pods: an
enforced `pod-security.kubernetes.io/enforce` label on it must be `privileged` (the device manager
already needs this). Tenant namespaces may enforce `restricted`, which admits `csi` volumes.

**Uninstall.** The cache directory on each node is a hostPath and stays after uninstall; the
documentation gives the command to remove it. Changing `rootPath` points the plugin at an empty
cache: nothing is migrated or removed from the old path, and the documentation describes the change
as an explicit migration.

**Upgrade.** The DaemonSet rolls one node at a time. Mounted Pods keep running; a mount requested
while a node's plugin is restarting fails and is retried by kubelet, and succeeds once the new Pod
serves.

### User Stories

#### Story 1

As a model deployer, I want every replica of my deployment on a node, and every restart, to reuse
the weights already on that node, so that scaling out and recovering does not download the model
again.

#### Story 2

As a model deployer, I want a replica never to start on a partial or corrupted copy of the weights,
whatever fails during the download, so that a flaky network or a crashed node component cannot
serve wrong tensors.

#### Story 3

As a security administrator, I want a tenant's Pod to mount only content that the tenant's own
resolved artifact names, and a tenant's token to be used only for the tenant's own repository, so
that sharing the node cache across namespaces does not share access.

#### Story 4

As a cluster administrator, I want to set cache watermarks, download concurrency, bandwidth, the Hub
endpoint and proxy once, and see on each node what the plugin applied and what it holds, so that the
cache stays within what the node can afford and I can audit it.

#### Story 5

As a user of Instances, I want to mount a Hugging Face model into an Instance, so that a
non-engine workload gets pinned, verified weights without assembling a hostPath.

#### Story 6

As an operator, I want a deployment whose weights are not mounted yet to say whether they are
downloading, failing and why, or waiting for a missing plugin, so that I do not have to read node
logs.

### Core Features & Acceptance Criteria

#### F1 - The NodeModelStore API and its writers

- The CRD exists in `worker.gpustack.ai/v1alpha1`, cluster-scoped, with the status subresource,
  printer columns, short name and category above; `status.models` is a map keyed by digest with at
  most 256 items; generated clients, deepcopy and protobuf are regenerated.
- The worker creates one object per node whose `CSINode` lists the driver, owned by the Node, and
  does not delete it when the driver is briefly unregistered.
- A status update from the plugin's ServiceAccount through a Pod on another node, from any other
  identity, or with a token lacking Pod extras is refused; one from the plugin Pod on the object's
  node is admitted; the check passes on Kubernetes 1.29 (Pod lookup) and on 1.30 or later
  (node-name extra).
- A `modelartifacts/status` update from any identity other than the worker's is refused; the
  worker's own updates are admitted.
- Both status webhooks are registered with `failurePolicy: Fail`. On kind with a Kubernetes 1.29
  node image, each has a positive baseline beside its refusals: the plugin Pod updating its own
  node's status is admitted and updating another node's is refused; a non-worker identity updating
  a `modelartifacts/status` is refused and the worker's own update is admitted. Every refusal is
  asserted by the rule its message names, not only by the write failing.

#### F2 - Effective configuration

- The five Settings exist with their defaults and checks; admission refuses an invalid value and
  `Node` without the CSIDriver; the worker refuses to start on an invalid value from the
  environment.
- Every node's `spec` equals the merge of the Settings; changing a Setting rewrites every node's
  `spec` within one minute and every plugin reports the new `observedGeneration`.
- The merge is a field-level overlay of an ordered list of layers, tested with a second layer that
  sets some fields and leaves others unset.
- The plugin refuses to download with an invalid `spec` (`Ready=False`, `InvalidConfiguration`).
- On a filesystem shared with kubelet the high watermark is capped by the rule above: with a
  kubelet configuration file that sets the three thresholds, from them (a percentage and a quantity
  threshold each covered); with a file that sets none, an absent file and an unparseable file, from
  kubelet's defaults, giving 80; the `Ready` message names the source. On a dedicated filesystem
  the Setting applies uncapped.

#### F3 - Filtering

- `allowPatterns` and `ignorePatterns` follow rule 5 of format v1; the filtered manifest's digest
  equals the one a reference implementation computes with Python's `fnmatch.fnmatchcase` for the
  same tree and patterns. The cross-check covers at least `**`, `original/*`, a pattern differing
  from a path only in case, and a pattern ending in `/`, each on the allow and on the ignore side.
- An artifact without patterns keeps its digest byte for byte: the ModelArtifact spec's existing
  manifest test vectors produce the same encoding and digest through the filtering code path with
  empty pattern lists.
- Admission refuses patterns on a PVC source, over the count or length bounds, or changed after
  creation.
- A filtered artifact under Engine delivery creates no Pod and reports `FilterNeedsNodeDelivery`.

#### F4 - Mount authorization

- A mount is refused for each failed rule (missing artifact in the Pod's namespace, UID mismatch,
  digest mismatch, not resolved, not a Hugging Face source), with `PermissionDenied` and the rule in
  the message. A request that is not an ephemeral inline volume is malformed for this driver and is
  refused with `InvalidArgument`, by the gRPC-code rule of the error classification.
- A Pod that writes another namespace's name into `csi.storage.k8s.io/pod.namespace` is still judged
  in its own namespace.
- A mount of content already on the node is refused all the same when the Pod's namespace has no
  matching resolved artifact.

#### F5 - Materialization, verification and publication

- A cold mount downloads each file of the filtered manifest once, however many Pods mount it
  meanwhile, and publishes a tree whose files, recomputed one by one, match the manifest.
- A second mount of the same digest on the node downloads zero bytes (by the download metric) and
  reads no file of the tree.
- A corrupted byte, a wrong size, or a manifest that differs from the artifact's digest is never
  published; the digest is `Failed` with `IntegrityMismatch`, and no Pod is mounted.
- Killing the plugin mid-download never leaves a consumer mounted on a partial tree; after the
  restart the download resumes or restarts and publishes.
- The failure backoff, its persistence across a restart, and cancellation after five minutes
  without a mount behave as specified.
- A request is sent only with the credential of the repository it names; no token reaches a
  redirect to another host.
- Range downloads, the concurrency limit and the bandwidth limit hold (unit tests against a test
  server that records every request and its timing).

#### F6 - Unmount, restart and collection

- Unmounting an unknown target succeeds; unmounting twice succeeds; the reference ledger rebuild
  classifies the three cases as specified.
- Collection removes only unreferenced content past its grace period, down to the low watermark,
  oldest first, and never a referenced tree or an active partial; `CapacityLow` is set when nothing
  can be removed.
- `NodeModelStore.status` is rewritten from disk after a restart and is written zero times while
  nothing changes.

#### F7 - Consumers

- Under Node delivery, a ModelDeployment's Pods carry the CSI volume, mount and command line of the
  rendering above, no Engine cache or raised limit, and `status.model.delivery=Node`; the take-over
  tier gets only the mount; a deployment without `artifactRef` renders byte for byte what it did.
- Switching the Setting rolls each Hugging Face deployment once; Engine delivery is unchanged.
- An Instance mounts a Hugging Face artifact through the plugin; the admission refusal is gone.
- Every `WeightsReady` reason in the table is produced by its case.

#### F8 - Deployment

- The chart renders the CSIDriver, DaemonSet, ServiceAccount and ClusterRole above, and nothing
  when `modelManager.enabled=false`; `make lint chart` passes, including the image-rewrite check.
- `--disable-applications` accepts `model-manager`; image mode's overlay switches it; chart
  defaults and the image-mode overlay render the same objects.

#### F9 - Documentation

- A new reference page for node delivery and `NodeModelStore`, and a new operations page for the
  administrator (the layers, how to read a node's effective configuration and state, the capacity
  rule, switching delivery, the namespace requirement, uninstall and root-path migration, and upgrade
  notes stating that upgrading from the previous version rolls every Hugging Face deployment once
  and the two ways to avoid it); the
  ModelArtifact reference page, `docs/settings.md`, `docs/architecture.md` (a fourth subcommand),
  the installation-modes page, `docs/README.md`, the documentation skill's routing table and the
  overview skill are updated; `make lint docs` passes.

#### F10 - End to end

On kind, including a Kubernetes 1.29 node image: a cold mount, a warm mount with zero bytes, a
corrupting source, the plugin killed mid-download, a `restricted` namespace, forged volume
attributes, the two status guards with their positive baselines (F1), a credential scan, a cold
download through an HTTPS proxy whose every file request reaches the Hub from the proxy (with a
no-proxy control), a
rolling upgrade of the plugin with a mounted Pod, a Setting change reaching every node's `spec` and
`observedGeneration`, and collection at the watermark. On one real GPU node, a large model served
through Node delivery. Each has recorded evidence.

### Notes / Constraints / Caveats

#### Platform capabilities and the version floor

| Capability | Earliest default-on version | Source |
| --- | --- | --- |
| CSI inline ephemeral volumes, with `nodePublishSecretRef` | 1.16 (beta), 1.25 (GA) | Kubernetes storage docs, KEP-596 |
| `CSIDriver` `storage.k8s.io/v1` with `podInfoOnMount`, `volumeLifecycleModes` | 1.18 (GA) | Kubernetes API reference |
| `CSIDriver.fsGroupPolicy` | 1.20 (beta), 1.23 (GA) | KEP-1682 |
| `CSINode` `spec.drivers` | 1.17 (GA) | Kubernetes API reference |
| `mountPropagation: Bidirectional` | 1.12 (GA) | Kubernetes volumes docs |
| kubelet merging Pod information over `volumeAttributes` | read at 1.29.0 | `pkg/volume/csi/csi_mounter.go:233-234,607-614` |
| kubelet mount timeout 2 min, retry backoff capped at 2 min 2 s | read at 1.29.0 | `pkg/volume/csi/csi_plugin.go:53`, `exponential_backoff.go:31,37` |
| Pod name and UID in a bound token's `userInfo.extra` | 1.22 or earlier | `staging/src/k8s.io/apiserver/pkg/authentication/serviceaccount/util.go` at v1.22.0 |
| Node name in that extra | 1.30 (beta, default on) | the same file at v1.30.0; used only when present |
| Admission webhooks on a subresource | 1.16 (GA) | `admissionregistration.k8s.io/v1` |
| `SelfSubjectReview` `authentication.k8s.io/v1` | 1.28 (GA) | KEP-3325 |
| `TokenReview` `authentication.k8s.io/v1`, the fallback before 1.28 | 1.6 (GA) | Kubernetes API reference |
| Pod Security `restricted` admitting `csi` volumes | 1.23 (beta), 1.25 (GA) | Pod Security Standards |
| `PodReadyToStartContainers` Pod condition | 1.29 (beta, default on) | the ModelArtifact spec |

The floor is Kubernetes 1.29, this repository's effective floor because the bundled Kueue v0.18
requires it. Nothing here needs ValidatingAdmissionPolicy (1.30): the rules are webhook rules, as
for every other type in this group. The node-name extra (1.30) is an optimization, not a
requirement. Evidence gathered on a newer cluster states its version.

#### Measurements this design rests on

- The CSI inline form (kubelet 1.35.7, four CPU nodes): a Pod with the volume and
  `nodePublishSecretRef` was admitted in a namespace enforcing `restricted:latest`, while the same
  Pod with a hostPath volume was refused; a 480-second cold materialization (four times a mount
  call's timeout) downloaded once for two Pods; every mount call carried the Secret and a replaced
  Secret reached the next call; a plugin killed mid-download resumed with a `Range` request and no
  sample ever showed a consumer mounted before publication; forged attributes naming another
  namespace's artifact, or the tenant's own artifact with another digest, were refused while the
  matching Pods mounted; references rebuilt from mountinfo and a damaged ledger matched the running
  Pods exactly.
- Verification cost (one node with a 40-thread CPU and a network disk capped near 0.47 GB/s): Go
  SHA-256 at 1.16 GB/s on one thread; hashing 141 GB took the same wall time with 1, 2, 4 or 8
  threads, bound by the disk; reading a finished download again to hash it cost 44 to 46% of the
  download's time; publication in milliseconds.

#### Implementation constraints

- The node component is the `model-manager` subcommand (alias `mm`) of the operator binary,
  package `pkg/modelmanager`, the operator image. It reuses the manager and web server the device
  manager uses, without leader election.
- The manifest, the filter and the Hub client stay in `pkg/modelartifact`, shared by the controller
  and the plugin, so both compute the same digest from the same code.
- The on-disk layout is versioned by a file at the cache root, so a later layout is never read as
  this one:

  ```text
  <root>/layout                              "gpustack-model-store v1"
  <root>/published/<hex>/marker              digest, attempt, manifest format version
  <root>/published/<hex>/tree/...            the files; the directory a consumer mounts
  <root>/partial/<hex>/<attempt>/            one attempt's marker and tree, renamed whole
  <root>/trash/                              removed trees, renamed out of published/ first
  <root>/ledger/                             references and per-digest backoff
  ```

  The marker sits beside the tree rather than in it, so no repository file can collide with it.
  Published files are mode 0444 and directories 0555, owned by root, readable by any user a
  `restricted` Pod runs as.
- The plugin's informers are the cluster's `modelartifacts`, its own `NodeModelStore` by name, and
  the CA ConfigMap; nothing else.
- Settings are read through the existing Settings package, whose reads are cached for 30 seconds;
  the worker also watches the Settings store so a change reaches every node within a minute.

#### Rules recorded for later work

- A scrub, when added, is rate-limited and runs at low priority: a network disk's limit is shared
  by reads and writes, and one pass over 141 GB occupies such a disk for about five minutes.
- A provider that writes into the cache must hash in its write stream, or its content is verified
  by a second read before publication; publishing before verification would change the rule that
  consumers only see verified trees, and is a decision for that spec.

### Boundaries

- **Always:** authorize from the API in the Pod's namespace; hash while downloading and publish only
  verified trees by rename; keep one writer per digest; send a credential only for its own
  repository; keep tokens out of every persisted or emitted artifact; keep running Pods mounted
  whatever the plugin or the Hub does; check every Setting value where it is used; test with fake
  clients, a test Hub server and table-driven cases.
- **Ask first:** a new dependency pin in `hack/deps.sh` or a patch under `hack/deploy/`; a change to
  the operator image's build; a test cluster; any tenant-writable field that influences where the
  plugin connects; opening ModelScope.
- **Never:** trust `volumeAttributes` for authorization; let a consumer see a partial tree; give the
  plugin Secret, Setting or cluster-admin access; write a namespace, artifact or Pod name into a
  cluster-scoped object; remove a referenced tree; infer the delivery mode from the plugin's
  presence; delete a running Pod because of the node cache.

### Risks and Mitigations

- Hub redirects or CDN behavior change (range support, signed URLs) → the downloader follows the
  Hub's redirects as a normal HTTP client and treats a range the server does not honor as one full
  request; the end-to-end case downloads a real repository once.
- A plugin bug publishes a wrong tree → publication requires every file's hash and the manifest's
  digest; the kind cases corrupt bytes, sizes and the tree listing.
- A node's disk fills during a download from outside the cache → the reservation is checked before
  downloading and collection keeps usage under the high watermark; writes that fail with no space
  fail the attempt with `InsufficientCapacity` and discard nothing published.
- The cache shares the kubelet filesystem with a non-default eviction threshold → the documentation
  states the rule and recommends a dedicated filesystem; the cap reads the kubelet configuration
  file and falls back to kubelet's defaults.
- SELinux-enforcing nodes deny a container reading the bind-mounted tree → the tree is read-only and
  world-readable; SELinux relabeling is not handled, and the documentation says so.
- A stale `NodeModelStore` on a node the plugin left → documented; its `Ready` condition stops
  being refreshed, and later readers (placement, sync) must require `Ready`.
- The switch to Node delivery rolls every Hugging Face deployment → the Setting is explicit and
  documented; upgrades seed it only when the key is new.
- A tenant creates many artifacts to make nodes download → a download starts only for a mount of a
  resolved artifact, per digest once, under the node's concurrency, bandwidth and watermark limits,
  and failing digests back off.

## Design Details

### Commands

```sh
go test ./pkg/modelmanager/... ./pkg/modelartifact/... ./pkg/worker/... ./api/...
make lint </dev/null                 # edit pass; compare the tree before and after
make lint docs </dev/null            # after docs/ or specs/ edits
make lint chart </dev/null           # after chart edits
```

`make generate` refuses a worktree whose absolute path does not end in `/gpustack.ai/gpustack`;
run it in a disposable copy with that suffix after changing types under `api/` or webhook markers,
then copy the generated outputs back.

The environment has three parts:

- **Local, the development machine (macOS).** Unit tests, lint and generate. The plugin's mount
  code is Linux-only (`*_linux.go`, with a stub elsewhere), and neither this machine nor CI builds
  it for Linux in a test run, so the plugin's packages are also built, vetted and tested inside a
  Linux container on every task that touches them:

  ```sh
  docker run --rm --privileged -v "$PWD":/src -w /src golang:1.26 \
    sh -c 'go vet ./pkg/modelmanager/... && go test ./pkg/modelmanager/...'
  ```

  `--privileged` lets the bind-mount tests mount inside the container.
- **Local kind.** The end-to-end cases run on a kind cluster of this spec's own on the same
  machine's Docker (one control plane, two workers), its kubeconfig in a scratch file so the user's
  kubeconfig is never read or written, an operator image built natively and loaded into kind, a test
  Hub and a forward proxy deployed by the case (the Python standard library in a stock Python image,
  the script mounted from a ConfigMap). Every kind case runs twice, once on a Kubernetes 1.29 node
  image and once on the current default:

  ```sh
  kind create cluster --name <cluster> --kubeconfig "$KCFG" --image kindest/node:v1.29.14 --config <two-worker config>
  KUBECONFIG="$KCFG" bash .agents/skills/_e2e-lib/scripts/build-load.sh <TAG>
  KUBECONFIG="$KCFG" bash .agents/skills/_e2e-lib/scripts/deploy.sh gpustack-system <TAG>
  KUBECONFIG="$KCFG" bash .agents/skills/gpustack-operator-e2e/cases/case-<N>.sh <NS>
  kind delete cluster --name <cluster> --kubeconfig "$KCFG"
  ```

- **Remote GPU cluster.** The large-model case runs on the shared test cluster with one GPU node
  (created by the coordinator on request, under a cluster lock), through its named context passed
  explicitly to every `kubectl` and `helm` call, on an amd64 `dev-<short sha>` image built on the
  build machine from a clean committed tree, whose digest is checked against the running Pods'
  `imageID`.

### Project Structure

- `api/worker/v1alpha1/node_model_store.go` — the `NodeModelStore` type; `model_artifact.go` gains
  the patterns; `model_deployment.go` gains the `Node` delivery value.
- `pkg/modelartifact/` — the filter; the manifest and Hub client gain what the plugin reuses
  (listing with a caller's credential, file download URLs).
- `pkg/modelstore/` — the effective configuration: the check of a `NodeModelStoreSpec` and the
  layered merge, imported by the worker (which writes it) and the plugin (which checks it again).
- `pkg/modelmanager/` — the subcommand (`cmd.go`, `option.go`, `config.go`, `manager.go`, the
  device manager's shape), and below it `store/` (layout, attempts, publication, ledger, reference
  rebuild), `download/` (ranges, hashing, limits, credentials), `driver/` (CSI Identity and Node,
  authorization), `materialize/` (per-digest attempts, backoff, cancellation), `gc/` (capacity,
  collection, the watermark cap) and `report/` (status and metrics).
- `pkg/worker/controllers/worker/node_model_store*.go` — creation and the effective configuration.
- `pkg/worker/controllers/worker/model_artifact*.go`, `model_deployment*.go`, `instance*.go` — the
  filter in resolution, Node delivery rendering and `WeightsReady`.
- `pkg/worker/webhooks/worker/` — `node_model_store.go` (the status guard), and the status guard,
  patterns and delivery rules added to `model_artifact.go` and `instance.go`.
- `pkg/worker/settings/` — the five Settings.
- `pkg/worker/kuberess/` — the `model-manager` application key.
- `cmd/gpustack-operator/main.go` — registers the subcommand.
- `deploy/gpustack-operator/chart/` — `templates/model-manager/`, values, schema.
- `docs/reference/node-model-store.md`, `docs/operation/model-store.md` and the updated pages.
- `.agents/skills/gpustack-operator-e2e/cases/` — the end-to-end cases.

### Code Style

The plugin keeps the render-style separation the worker uses: decisions are pure functions of
values, and effects happen at the edges.

```go
// authorizeMount decides whether a Pod may mount a digest, from the Pod's namespace as kubelet
// reports it and the artifact as the API reports it.
//
// THE VOLUME'S ATTRIBUTES ARE HINTS. A tenant can hand-write a Pod with any artifact name and any
// digest, so every attribute must agree with a resolved artifact in the Pod's own namespace, and
// the namespace is the one kubelet writes over the Pod's attributes.
func authorizeMount(podNamespace string, attrs mountAttributes, ma *workercore.ModelArtifact) (rule string, ok bool)
```

Other conventions:

- Comments state the rule and its reason, with no task identifiers.
- Tests are table-driven, use fake clients and an `httptest` Hub that can throttle, corrupt and
  record requests, and assert final state on disk and in objects.
- Files are named in snake_case.

### Implementation Plan

Tasks are built one at a time in this order; every task leaves the tree compiling with its tests
green, and starts with `git fetch origin && git rebase origin/main`. Checkpoints: after T7 the
control-plane contract is complete (API, Settings, both guards, per-node objects); after T12 the
plugin works on its own against a test Hub; after T14 the component installs; after T16 the kind
evidence exists; T17 is the GPU evidence. A `go test -run` pattern that matches nothing exits 0, so
every Verify below is judged by the named tests appearing in the `-v` output, not by the exit code.

- [x] **T1 · Manifest filter**
      Blocked by: None
      Owns: `pkg/modelartifact/filter*.go`, `pkg/modelartifact/testdata/filter/**`,
      `pkg/modelartifact/testdata/manifest/canonical_manifest.py`
      Gate: review
      Acceptance: `fnmatchcase` semantics (`*` and `?` cross `/`, `[...]` and `[!...]`, an unclosed
      `[` literal, case-sensitive) and the filter of rule 5 (trailing `/` becomes `<pattern>*`, empty
      allow keeps all, ignore wins); a fixture generated by the reference script with Python's
      `fnmatch.fnmatchcase` agrees on every case, covering `**`, `original/*`, a case-only
      difference and a trailing `/`, each as allow and as ignore; the existing manifest test vector
      through the filter with empty lists gives the same bytes and digest as today.
      Verify: `go test ./pkg/modelartifact/ -run '^TestFilter' -v`

- [x] **T2 · API types and generation**
      Blocked by: None
      Owns: `api/worker/v1alpha1/**`, `api/worker/zz_generated.openapi.go`, `pkg/kubeclients/**`,
      `pkg/worker/webhooks/worker/zz_generated.webhooks.go`
      Gate: review
      Acceptance: `NodeModelStore` and its list (cluster scope, status subresource, printer
      columns, short name, category, `status.models` a map keyed by digest with at most 256 items,
      the fields and enums of the Proposal); `ModelArtifactSpec.AllowPatterns` and `IgnorePatterns`
      with schema bounds (32 items, 1 to 256 characters); `ModelDeploymentModelDeliveryNode`; type
      comments state the writers and the rules; `make generate` output committed and a second run
      changes nothing.
      Verify: `go build ./... && go test ./api/...`, and a second `make generate` in the disposable
      copy with an empty diff

- [x] **T3 · Settings and the effective configuration**
      Blocked by: T2
      Owns: `pkg/worker/settings/**`, `pkg/modelstore/**`, the startup check in `pkg/worker/option.go`
      Acceptance: the five Settings with defaults and admissions (the delivery mode's `Node` is
      refused while the CSIDriver does not exist, read through the loopback client); `modelstore`
      checks a `NodeModelStoreSpec` (endpoint scheme, `ValidateProxy`, watermark order and range,
      concurrency range, non-negative bandwidth) and merges an ordered list of layers field by field,
      tested with a second layer that sets some fields and leaves others unset; the worker's startup
      refuses an invalid value from the environment.
      Verify: `go test ./pkg/modelstore/ -run '^Test(Merge|Validate)' -v`

- [x] **T4 · Patterns in admission and resolution**
      Blocked by: T1, T2
      Owns: `pkg/worker/webhooks/worker/model_artifact.go`, `pkg/worker/webhooks/worker/model_artifact_test.go`,
      `pkg/worker/controllers/worker/model_artifact.go`, `pkg/worker/controllers/worker/model_artifact_test.go`,
      `pkg/modelartifact/huggingface*.go`
      Acceptance: admission refuses patterns on a PVC source, beyond the bounds, with control
      characters, and changed on update; resolution filters before canonicalization and a filter
      keeping nothing is `EmptyManifest`; an artifact without patterns resolves to the same digest as
      before; `pkg/modelartifact` exports the filtered tree listing and a file's download URL for the
      plugin, with the token per call.
      Verify: `go test ./pkg/worker/webhooks/worker/ -run '^TestModelArtifactWebhook' -v`,
      `go test ./pkg/worker/controllers/worker/ -run '^TestModelArtifactReconcile' -v`,
      `go test ./pkg/modelartifact/ -run '^TestHuggingFace' -v`

- [x] **T5 · ModelArtifact status guard**
      Blocked by: T4
      Owns: `pkg/worker/webhooks/worker/model_artifact_status*.go`, the `subResources` marker and the
      identity lookup in `pkg/worker/webhooks/worker/model_artifact.go`,
      `pkg/worker/webhooks/worker/deletion_guard_test.go`, `pkg/worker/webhooks/worker/zz_generated.webhooks.go`
      Gate: review
      Acceptance: the webhook covers `modelartifacts/status` with `failurePolicy: Fail`; an update
      of the subresource from any identity other than the worker's `SelfSubjectReview` username is
      refused with a message naming the rule, and the worker's own is admitted; main-resource
      requests keep their existing rules; the guard also holds on a Terminating artifact (the
      framework's deletion guard skips validation of an object with a deletion timestamp, so the
      handler opts out of it for the status path). Every existing webhook is also read for
      validation the deletion guard skips on a Terminating object; any that a tenant could use to
      write a harmful state gets an issue, and is not fixed here. Read: every handler that
      validates UPDATE is already classified in `TestDeletionGuardClassification`; of those that
      keep the guard, Instance is the only tenant-writable one, and its controller only deletes the
      Pod of a Terminating Instance and renders nothing from its spec, so a spec edit skipped there
      has no effect; TopologySource is cluster-scoped and administrator-only. No issue was filed.
      Verify: `go test ./pkg/worker/webhooks/worker/ -run '^TestModelArtifactStatusGuard' -v`

- [x] **T6 · NodeModelStore creation and configuration**
      Blocked by: T2, T3
      Owns: `pkg/worker/controllers/worker/node_model_store*.go`, `pkg/worker/controllers/setup.go`
      Gate: review
      Acceptance: one object per node whose `CSINode` lists the driver, owned by the Node, created
      and aligned with create-or-update; not deleted when the driver leaves `CSINode`; every object
      deleted while the CSIDriver does not exist; `spec` is the merge of the Settings; a change of
      the Settings store reconciles every object and converges within one minute despite the
      Settings read cache; status is never written by this controller.
      Verify: `go test ./pkg/worker/controllers/worker/ -run '^TestNodeModelStore' -v`

- [x] **T7 · NodeModelStore status guard**
      Blocked by: T2
      Owns: `pkg/worker/webhooks/worker/node_model_store*.go`, `pkg/worker/webhooks/setup.go`,
      the worker flag carrying the plugin's ServiceAccount name in `pkg/worker/option.go` and
      `pkg/system/control.go`
      Gate: review
      Acceptance: `nodemodelstores/status` is guarded with `failurePolicy: Fail`; admitted only for
      the plugin's ServiceAccount whose token names a Pod with the matching UID in the operator
      namespace on the object's node, or whose node-name extra equals the object's name; refused,
      with the rule named, for another identity, missing extras, a UID mismatch, a missing Pod, and
      a Pod on another node; a Terminating object is guarded too.
      Verify: `go test ./pkg/worker/webhooks/worker/ -run '^TestNodeModelStoreStatusGuard' -v`

- [x] **T8 · Plugin store**
      Blocked by: T1
      Owns: `pkg/modelmanager/store/**`
      Gate: review
      Acceptance: the versioned layout; per-digest attempt numbers; publication (files synced, marker
      beside the tree written and synced, directory synced, one rename, parent synced) and the
      constant-time published check that reads only the marker; published modes 0444 and 0555; a
      tree without a marker is never reported published; the ledger of references and per-digest
      backoff; the rebuild classifying ledger-and-mountinfo, mountinfo only (adopted) and ledger only
      (dropped) from a mountinfo fixture; orphan partial removal; no path escapes its partial
      directory.
      Verify: `go test ./pkg/modelmanager/store/ -v`, and the Linux container run

- [x] **T9 · Downloader**
      Blocked by: T1
      Owns: `pkg/modelmanager/download/**`
      Gate: review
      Acceptance: against an `httptest` Hub that throttles, stalls, corrupts, redirects to another
      host and records every request: files verified in byte order (`sha256`, git blob SHA-1, size)
      while downloading; large files in parallel ranges within a window ahead of the hash position;
      a stalled range retried from its last byte after 30 seconds (a test clock), repeated failure
      classified `SourceUnavailable`; resume from a saved hash state and from a re-hashed prefix;
      the node-wide request limit and bytes-per-second limit hold; each request carries only the
      credential of its repository; `Authorization` never reaches the other host; no token in any
      error string.
      Verify: `go test ./pkg/modelmanager/download/ -v`

- [x] **T10 · CSI driver and the subcommand**
      Blocked by: T2, T8
      Owns: `pkg/modelmanager/*.go`, `pkg/modelmanager/driver/**`, `pkg/modelmanager/metrics/**`,
      `cmd/gpustack-operator/main.go`, the cache selection in `pkg/manager/config.go`, `go.mod`, `go.sum`
      Gate: review
      Acceptance: `model-manager` (alias `mm`) serves CSI Identity and Node on a Unix socket under
      the kubelet plugins directory, and metrics, readiness and liveness on its HTTPS port; its
      informers are the cluster's `modelartifacts`, its own `NodeModelStore` and the CA ConfigMap;
      `Unavailable` until they sync; every authorization rule of F4 refuses with `PermissionDenied`
      and its rule, including a forged `pod.namespace` key (the request as kubelet builds it); an
      already published digest mounts read-only by bind mount and records the reference; unmount is
      idempotent and works from the target alone; start-up rebuilds references.
      Verify: `go test ./pkg/modelmanager/driver/ -v`, `go build ./cmd/...`, and the Linux container
      run

- [x] **T11 · Materialization**
      Blocked by: T4, T8, T9, T10
      Owns: `pkg/modelmanager/materialize/**`, the manifest's entries in `pkg/modelartifact/manifest.go`,
      a file's receive callback in `pkg/modelmanager/download/download.go`
      Gate: review
      Acceptance: one attempt per digest that concurrent mounts join; the manifest recomputed with
      the mount's credential and compared with the artifact's digest; the capacity reservation; the
      backoff (one minute doubling to one hour, reset on success, kept across a restart); cancellation
      after five minutes without a mount; each credential kept with its artifact's repository when
      mounts from two namespaces join; the reason table and gRPC codes; the progress message with
      bytes of total; a cold mount publishes and the next call mounts.
      Verify: `go test ./pkg/modelmanager/materialize/ -v`, and the Linux container run

- [x] **T12 · Capacity, collection, status and metrics**
      Blocked by: T3, T11
      Owns: `pkg/modelmanager/gc/**`, `pkg/modelmanager/report/**`, the wiring in `pkg/modelmanager/manager.go`,
      the client swap and pre-header stall timer in `pkg/modelmanager/download/download.go`
      Gate: review
      Acceptance: collection by the watermarks, the ten-minute grace, the 24-hour partial rule,
      oldest first, never a referenced tree or an active partial, `CapacityLow`; the watermark cap
      from a kubelet configuration file (percent and quantity thresholds), and from defaults for an
      absent, empty and unparseable file, with the source in the `Ready` message, and none on a
      dedicated filesystem; status rebuilt from disk, written only on the listed changes (zero writes
      over a quiet period, counted on a fake client), the 256-entry order, `Ready` and
      `InvalidConfiguration`; the metrics with the names and labels of the Proposal.
      Verify: `go test ./pkg/modelmanager/gc/ ./pkg/modelmanager/report/ -v`, and the Linux container
      run

- [x] **T13 · Node delivery in consumers**
      Blocked by: T2, T3, T4
      Owns: `pkg/worker/controllers/worker/model_deployment*.go`,
      `pkg/worker/controllers/worker/model_artifact_placement*.go`,
      `pkg/worker/controllers/worker/instance*.go`, `pkg/worker/webhooks/worker/instance*.go`
      Gate: review
      Acceptance: under `Node` a ModelDeployment's Pods carry the CSI volume, attributes, Secret
      reference, mount and local-path command line, with no Engine cache or raised limit, and
      `status.model.delivery=Node`; the take-over tier gets only the mount; a deployment without
      `artifactRef` renders byte for byte as before; switching the Setting changes the Pod
      fingerprint once; an Instance mounts a Hugging Face artifact through the plugin and the
      admission refusal is removed; `NodeDeliveryUnavailable`, `FilterNeedsNodeDelivery`,
      `Materializing`, `MaterializationFailed`, `WeightsNotMounted` and `Mounted` each come from their
      case, reading the node's `NodeModelStore` and never an event.
      Verify: `go test ./pkg/worker/controllers/worker/ -run '^Test(RenderModelDeploymentArtifact|ModelDeploymentWeightsReady|InstanceModelVolume)' -v`,
      `go test ./pkg/worker/webhooks/worker/ -run '^TestInstanceModelVolume' -v`

- [x] **T14 · Chart and the application table**
      Blocked by: T7, T10
      Owns: `deploy/gpustack-operator/chart/**`, `pkg/worker/kuberess/**`
      Gate: review
      Acceptance: F8: the CSIDriver, DaemonSet (registrar sidecar, privileged plugin with
      Bidirectional propagation, the kubelet configuration file read-only, no host PID, IPC or
      network), ServiceAccount and the narrow ClusterRole; nothing rendered when disabled; the
      worker receives the delivery-mode seed and the plugin's ServiceAccount name; `values.yaml`
      schema annotations and a regenerated `values.schema.json` and chart README;
      `--disable-applications` accepts `model-manager`; the image-mode overlay switches it and
      renders what the chart defaults render.
      Verify: `go test ./pkg/worker/kuberess/ -v`, `make generate chart`, `make lint chart </dev/null`

- [x] **T15 · Documentation**
      Blocked by: T13, T14
      Owns: `docs/**`, `.agents/skills/gpustack-operator-docs/references/page-map.md`,
      `.agents/skills/gpustack-operator-overview/**`, `AGENTS.md` (its subcommand sentence)
      Acceptance: F9, including the upgrade notes; the ModelArtifact reference page gains Node
      delivery and the patterns inside its existing sections, without an eleventh `##` heading.
      Verify: `make lint docs </dev/null`

- [x] **T16 · kind end-to-end cases**
      Blocked by: T12, T13, T14
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-101.sh`, `case-102.sh`, `case-103.sh`,
      `.agents/skills/gpustack-operator-e2e/cases/_model-hub-lib.sh`, `_model-hub.py`,
      `.agents/skills/gpustack-operator-e2e/cases/case-97.sh`, `case-98.sh`,
      `.agents/skills/gpustack-operator-e2e/SKILL.md`,
      `.agents/skills/gpustack-operator-chart-e2e/cases/case-1.sh`, `case-6.sh`
      Acceptance: the kind cases of the Test Plan pass on the 1.29 node image and on the current
      default with recorded evidence; the chart suite's install, uninstall and image-mode cases pass
      on the new chart. case-97 and case-98 prove Engine delivery and pin it for their run, since the
      chart seeds Node with the plugin; their assertions are unchanged, except that case-98 no longer
      expects an Instance naming a hub artifact to be refused, which this spec reverses. The chart
      suite's install case judges the default tag on the operator image only, since the registrar
      sidecar is a pinned mirror, and its image-mode case requires the model-manager DaemonSet.
      Verify: `KUBECONFIG="$KCFG" bash .agents/skills/gpustack-operator-e2e/cases/case-101.sh <NS>`,
      and the same for 102 and 103

- [x] **T17 · GPU end-to-end case**
      Blocked by: T15, T16
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-104.sh`, its row in
      `.agents/skills/gpustack-operator-e2e/SKILL.md`
      Gate: review
      Acceptance: the GPU case of the Test Plan passes with recorded evidence on an image whose digest
      matches the Pods' `imageID`; case-101 also passes on that cluster, because a kind node is a
      container whose mount propagation and kubelet directory differ from a real host's, so the
      plugin's mount path is only proven on a real kubelet.
      Verify: `bash .agents/skills/gpustack-operator-e2e/cases/case-104.sh <NS>` with the GPU
      cluster's context

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- The Instance admission test that expects a Hugging Face artifact to be refused is replaced by the
  acceptance case (T13).
- The application-table tests that hard-code the names and switches (`Test_ApplicationNames`,
  `Test_componentSwitches`, the overlay-switch test, the chart-defaults-match-overlay test) gain the
  `model-manager` entry (T14).
- The render golden tests of existing deployments stay unchanged; T13 adds the byte-for-byte check
  for a deployment without `artifactRef` under both delivery Settings.

#### Unit tests

Baselines measured on the base commit with `go test -cover`, with the target for this spec:

- `gpustack.ai/gpustack/pkg/modelartifact`: 2026-09-25 - 91.6%, no decrease
- `gpustack.ai/gpustack/pkg/worker/controllers/worker`: 2026-09-25 - 82.7%, no decrease
- `gpustack.ai/gpustack/pkg/worker/webhooks/worker`: 2026-09-25 - 91.1%, no decrease
- `gpustack.ai/gpustack/pkg/worker/kuberess`: 2026-09-25 - 31.5%, no decrease
- `gpustack.ai/gpustack/pkg/worker/settings`: 2026-09-25 - 0.0% (no test files); the new Settings
  are exercised through `pkg/modelstore` and their readers' tests
- `gpustack.ai/gpustack/pkg/modelstore`: 2026-09-25 - new package, target 90%
- `gpustack.ai/gpustack/pkg/modelmanager/...`: 2026-09-25 - new packages, target 85% each, measured
  in the Linux container (the mount paths are Linux-only)

Named behaviors, each a table case: every filter class against the Python fixture and the unchanged
no-pattern digest; every Setting check; the merge with a partial second layer; both status guards'
admit and each refusal rule, including a Terminating object; the three reference-rebuild classes;
publication never exposing an unmarked tree; the range window, stall retry, resume, limits and the
per-repository credential; every authorization rule including the forged namespace key; the backoff
and its persistence; cancellation; the collection order and exclusions; the watermark cap's four
sources; the write budget's zero writes; every `WeightsReady` reason.

#### Integration tests

None beyond the fake-client controller tests and the plugin's tests against a real filesystem and
real bind mounts in the Linux container: the repository has no envtest suite, and the behaviors that
need a real kubelet, CSI registration or API server admission are in the end-to-end cases.

#### e2e tests

kind (no accelerator; every asserted value is on an object, a file, a metric or a log), each case on
a Kubernetes 1.29 node image and on the current default, against a test Hub the case deploys (it
serves the revision, tree, whoami and resolve endpoints with ranges, and can throttle, corrupt and
record requests by the hash of their `Authorization` header):

- **case-101 · Node delivery.**
  - A cold mount by two Pods of a throttled repository lasting longer than one mount call's
    timeout downloads each file once, and both Pods reach Running with the tree's files matching
    the manifest one by one.
  - A third Pod on the node downloads zero bytes (`download_bytes_total` unchanged).
  - A corrupting source is never published: the digest is `Failed` with `IntegrityMismatch`, and
    the Pod stays unmounted.
  - The plugin killed mid-download: no sample before publication shows the Pod mounted, and the
    download resumes.
  - A namespace enforcing `restricted` mounts.
  - Forged attributes are refused, each with its rule in the Pod's mount events, beside the
    matching Pods that mount: another namespace's artifact, the tenant's own artifact with another
    digest, and a `pod.namespace` key naming the other namespace.
  - One real public Hugging Face repository of a few megabytes mounts through the real Hub's
    redirects.
  - A scan of the plugin log, the worker log, events, both resources' status and the metrics for the
    token value finds zero matches, after the same scan finds a planted sample.
- **case-102 · Guards and configuration.**
  - The plugin Pod's own-node status update is admitted and its update of the other worker's object
    is refused by the node rule.
  - A non-worker identity's `modelartifacts/status` update is refused by the identity rule, and the
    worker's own update is admitted.
  - A Setting change reaches every node's `spec` and `observedGeneration` within one minute.
  - Writing `Node` with the CSIDriver absent is refused.
  - A cold download through the forward proxy, from a test Hub served over HTTPS with its CA given
    through the CA bundle Setting: every file's GET the Hub records arrives from the proxy's address.
    The proxy is configured as an HTTPS proxy, which, like the controller's and the engine's, applies
    to HTTPS URLs only, so the proxy itself sees tunnels rather than paths. The same case holds its
    control: a cold download of another digest without the proxy reaches the Hub from the plugin's
    node address. When the two addresses are the same the row is reported as undecidable, not as a
    pass.
- **case-103 · Lifecycle.**
  - A rolling restart of the plugin DaemonSet keeps a mounted Pod reading its files throughout,
    and a mount requested during the roll succeeds after it.
  - Collection on a node whose cache root is a small tmpfs removes the oldest unreferenced digest at
    the high watermark and never the referenced one.
  - A filtered artifact downloads only the filtered files; under Engine delivery its deployment
    reports `FilterNeedsNodeDelivery` and creates no Pod, and starts, unedited, once the Setting is
    back to Node.
  - Without the CSIDriver a deployment reports `NodeDeliveryUnavailable` and creates no Pod, and
    starts, unedited, once the CSIDriver is back.
  - An Instance mounts a Hugging Face artifact.
  - A ModelDeployment on Node delivery renders the CSI volume and reports `WeightsReady` through
    `Materializing` to `Mounted`.

GPU cluster (one node with one accelerator):

- **case-104 · Serving from the node cache.** vLLM serves a 7B model (about 15 GB) through Node
  delivery: `/v1/models` is `spec.model.name` and a chat request succeeds; the download's wall time
  and publication time are recorded; the replica that replaces the first on the node (the node has
  one accelerator, so a second replica is its replacement) downloads zero bytes and starts from the
  published tree. case-101 runs on the same cluster too, to prove the mount path on a real
  kubelet.

## Alternatives

### Download in an init container or a warm-up Pod

The Pod or a helper would download, and the plugin would only mount published trees. Rejected:
the download would run with the tenant's security context in the tenant's namespace, could not share
one download across namespaces, and would still need the plugin to mount. The measured inline form
holds a download of any length.

### Refuse at admission when the plugin is missing

The ModelDeployment webhook would refuse Hugging Face deployments while the CSIDriver is absent.
Rejected: it is order-dependent (the chart and the deployment may be applied together) and cannot
see a plugin uninstalled later. The consumer waits in status instead, and only the Setting write is
refused.

### Trust the node's status without a webhook

Rely on the plugin being a trusted system component. Rejected: RBAC cannot scope a status update to
one object, and the bound token's Pod extras, available on the floor version, make a real check
possible.

### Put the CA bundle's PEM in NodeModelStore.spec

It would spare the plugin a ConfigMap read. Rejected: a bundle can be hundreds of kilobytes, copied
into every node's object, above the object's size budget; the plugin reads the one ConfigMap the
Setting names instead.

### Hash after the download with parallel threads

Rejected by measurement: on a disk-bound node the second read costs 44 to 46% of the download's
time whatever the thread count, while hashing in the stream costs almost nothing.

### Defer the patterns

Node delivery would download whole commits. Rejected: repositories with duplicate weight formats or
consolidated checkpoints would double the bytes on every node, and the format already fixed the
semantics.

## Open Questions

None.
