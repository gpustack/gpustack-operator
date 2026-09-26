# Spec: Model Download Progress, Aggregation and v1 Views

Status: Shipped
Type: Feature

## Summary

The node model cache reports how far each download has come, and a `ModelArtifact` says how many
nodes hold its content and how far the others are. Each node's `NodeModelStore` gains a
`downloadedBytes` and a `source` per entry, written on thresholds (every 5% of the content, at most
once every 30 seconds) so a node writes about twenty times per download and nothing while nothing
changes. The `ModelArtifact` status gains the count of nodes that hold the content ready, that are
downloading it and that failed, and the mean progress of the nodes downloading it, written in 5%
steps. A `progress` subresource on a new `v1` view of `ModelArtifact` answers, when asked and
without writing anything, the same aggregate at full precision, reading the running downloads live
from the nodes; it names no node. `ModelArtifact` and `NodeModelStore` get `v1` views with printer columns, the read
surface GPUStack server migrates to, and this spec carries the table mapping every model-file
capability of GPUStack server to this API. It also fixes four rules of the cache's collection that
a real node showed wrong: a concurrent download could reserve past the high watermark, the kubelet
thresholds were read from a file kubelet may not use (#641), an invalid node configuration kept
collecting under the previous one (#633), and a byte-sized kubelet threshold larger than the disk
drove collection to its floor (#634).

## Motivation

GPUStack server tracks each model file per worker: its state, a download progress and a message.
The node cache that replaces the worker's downloads reports only states today: a 145 GB download is
`Downloading` for forty minutes with nothing to show how far it is, and a `ModelArtifact` says
nothing about where its content is. A reader that wants to show progress has to list every node's
`NodeModelStore` and match digests itself.

Progress must not cost what it measures. Kubernetes recommends custom resources for small,
infrequently updated declarative state; a status rewritten at every received byte would put a
download's throughput into etcd. A real node showed the risk: a prototype that added a progress
field without holding the other fields that move with a download wrote its node's status 828 times
in the first minute. The same prototype, with every download-moving field held to one threshold,
wrote 22 times across a 120 GB download.

The same real-node run (a two-node cluster whose cache shares the boot disk with kubelet) confirmed
the watermark rule and found four defects in how it is applied:

- Filled to 78.5% under an 80% high watermark, the node refused the next 65.5 GB download, never
  reported `DiskPressure`, evicted nothing and collected no image. But two downloads that each fit and
  together did not both passed their reservations, and usage reached 80.8%.
- kubelet ran with `--config=/etc/kubernetes/kubelet/config.yaml`; the plugin read
  `/var/lib/kubelet/config.yaml`, another file with different content, and reported it as the source
  (#641).
- With the node's configuration invalid (`Ready=False`), the plugin still removed a 29.55 GB tree
  under the previous configuration's watermarks (#633).
- A `nodefs.available: 300Gi` threshold on a 265 GB filesystem capped the high watermark at 2%, and
  the plugin removed every unreferenced tree the moment it started (#634).

### Goals

- A reader can tell, for any node and any content, whether it is ready, how many bytes of it a
  running download holds and where they came from; and, for any `ModelArtifact`, on how many nodes
  its content is ready, downloading or failed, and how far the downloading nodes are on average.
- The write cost is bounded and measured: during a download a node writes its `NodeModelStore` at
  most twice a minute and at most twenty times for progress alone per download, plus its state
  changes; a node or an artifact whose content is not changing writes nothing.
- A tenant reads the progress of the artifacts in the namespaces it may read, and nothing of any
  other namespace; the cluster-scoped `NodeModelStore` still names no tenant.
- GPUStack server has `v1` views of both resources to read, and a capability map that says, for each
  thing its model-file handling does, what replaces it here or which later spec will.
- The high watermark holds under concurrent downloads, comes from kubelet's effective thresholds,
  stops collecting while the node's configuration is invalid, and is never driven to its floor by a
  threshold that cannot describe the filesystem.

### Non-Goals

- Placement preference toward nodes holding the content (S4), node-to-node sync (S5: it adds the
  `Peer` source), prefetch, retention and tenant budgets (S6).
- Streaming progress (watch semantics on `progress`), and progress for a claim source: a claim is
  mounted from its volume and never downloaded.
- Aggregated `admin`/`edit`/`view` roles for the group: the chart grants tenants nothing today, and
  this spec keeps that pattern; the documentation gives the Role to create.
- Changing the capacity rule's numbers (margin 5, defaults 10/15/85), which the real node confirmed.

## Proposal

### NodeModelStore: progress on thresholds

Each `status.models[]` entry gains:

```yaml
    - digest: sha256:...
      state: Downloading
      sizeBytes: 145424101604        # now also set while Downloading, once the manifest is listed
      downloadedBytes: 61741236224   # Downloading only: bytes of the content on this node's disk
      source: Hub                    # where the bytes come from; S5 adds Peer
```

- `downloadedBytes` counts what the running attempt holds of the content: the verified checkpoints
  a resume carried over from a previous attempt, plus what arrived since; it never goes back on a
  resume. It is absent outside `Downloading`.
- `source` is `Hub` for a download from the Hub and for content published from one; content
  published before this field existed has none. It names no repository or endpoint.
- **The write rule.** Fields that move with every received byte are *progress fields*:
  `downloadedBytes` and, while some entry downloads, `capacity.storedBytes`. Every other field is a
  *state field*; `storedBytes` moving with no download running (a partial removed) is one too. A report whose
  only differences from the stored status are progress fields is written only when both hold: at
  least 30 seconds passed since this plugin last wrote the status, and some entry's
  `downloadedBytes` moved by at least 5% of its `sizeBytes` since the stored value. A report with a
  state difference is written at once and carries the current progress fields with it.
  `capacity.usedPercent` is already coarse (multiples of 5) and stays a state field.
- **Budget, as acceptance.** During downloads a node writes its `NodeModelStore` at most twice in
  any minute on average over the download, and at most 20 progress-only writes per download; a node
  whose content does not change writes nothing.
- The plugin reports on a 10-second tick while a download runs, so progress reaches the object on
  the threshold, not on the next unrelated event. A change of its own object triggers a report only
  when `metadata.generation` changed (the spec), never on its own status write.
- Nothing names a tenant: the entry is a digest, sizes, a state and a source.

### Kubelet thresholds from the effective configuration

- The worker reads each node's effective kubelet configuration through
  `GET /api/v1/nodes/<node>/proxy/configz` and writes the three values the cap uses into the node's
  spec:

  ```yaml
  spec:
    kubelet:                                # absent until read; the worker's reading, not a Setting
      nodefsAvailable: "10%"                # evictionHard["nodefs.available"], as kubelet reports it
      imagefsAvailable: ""                  # evictionHard["imagefs.available"]; empty = not set
      imageGCHighThresholdPercent: 85
  ```

  `configz` reports kubelet's merged configuration, flags and drop-ins included. The worker has the
  permission already; the plugin gets none, because `nodes/proxy` reaches every kubelet endpoint.
- The worker reads a node's `configz` on the reconcile that writes the node's object whenever it
  holds no reading younger than 30 minutes, and comes back every 30 minutes. A failed read keeps the
  last reading, or the value the object carries after a worker restart, so a transient failure does
  not flip the node's cap to kubelet's defaults and back. A change of the values changes the spec, so
  the plugin applies it without a restart. It carries no read time: a spec that changed at every read
  would bump the object's generation every 30 minutes.
- The plugin computes the cap from these values and its filesystem's size exactly as today: the
  lowest of `100 − nodefs − 5`, `100 − imagefs − 5` and `imageGCHigh − 5`, a value not set taking
  kubelet's default (10%, 15%, 85). The plugin no longer reads any kubelet file.
- When `spec.kubelet` is absent (the node was never read: kubelet's debugging handlers are
  disabled, the node is unreachable), the cap uses kubelet's defaults and the `Ready` message says the effective
  kubelet configuration could not be read. The worker logs why and retries on the next reconcile.
- A quantity threshold at or above the filesystem's size cannot be a share of it: it is treated as
  not set, the default applies, and the `Ready` message says which threshold was set aside and why,
  whether or not the cap lowers the watermark (#634). A percentage is bounded as today.

### Collection

- **Reservations count what is in flight** (the real-node overshoot). A reservation fits when current
  usage, plus what every running attempt reserved and has not yet written, plus the new bytes, stays
  under the high watermark. Two reservations that each fit and together do not: the second one is
  refused (or collects first), never both admitted. The periodic collection counts the running
  reservations the same way, so it makes room for what is in flight.
- **An invalid configuration stops collection** (#633). While the node's spec fails the plugin's
  check, collection is skipped and the `Ready` message says so, exactly as for a spec never applied;
  stale partials are not removed either. The previous watermarks are not used.

### ModelArtifact: aggregation in 5% steps

```yaml
status:
  nodes:                          # absent for a claim source and before resolution
    ready: 3                      # nodes whose NodeModelStore lists the digest Ready
    downloading: 2                # ... Downloading
    failed: 0                     # ... Failed
    downloadingPercent: 45        # mean over the downloading nodes, rounded down to a multiple of 5
```

- The ModelArtifact controller counts, over every `NodeModelStore`, the entries with the artifact's
  `manifestDigest`. `downloadingPercent` is the mean of `downloadedBytes / sizeBytes` over the nodes
  downloading it; it is absent when none is. Ready nodes are counted, not averaged: a node starting
  a download does not pull a ready count's 100% down.
- Every node downloads a whole copy; the mean is across nodes downloading at the same time, not a
  share of one download split between nodes.
- **The write rule.** The controller writes `status.nodes` when a count changes or the percent moves
  to another multiple of 5, and at most once every 30 seconds per artifact; a change inside the window
  is written when the window ends. The same digest may be named by artifacts in many namespaces;
  each artifact is throttled on its own, and a `NodeModelStore` change enqueues only the artifacts
  with a digest whose count or step it changed.
- The counts describe the content, not the artifact: two artifacts with the same digest see the
  same nodes. The digest is a content address and every count is a number, so no name crosses a
  namespace; that a node holds a content another tenant also uses is what a shared cache is.

### The `progress` subresource

`GET /apis/worker.gpustack.ai/v1/namespaces/<ns>/modelartifacts/<name>/progress`:

```yaml
kind: ModelArtifactProgress
apiVersion: worker.gpustack.ai/v1
metadata: {name: qwen-72b, namespace: team-a}
timestamp: "2026-09-26T00:00:00Z"
manifestDigest: sha256:...
sizeBytes: 145424101604
ready: 1                           # nodes holding the content ready
downloading: 2                     # nodes downloading it
failed: 1                          # nodes whose last attempt failed
downloadingPercent: 42             # mean over the downloading nodes, whole percent, not stepped
downloadingBytes: 123482472448     # the downloading nodes' bytes, summed
live: 2                            # downloading nodes read from their plugin just now; the rest are stored values
failureReasons:                    # reasons of the failed nodes, counted; no message, no node
  - {reason: SourceUnavailable, count: 1}
```

- It is computed on each request and never written: it reads the artifact, lists the
  `NodeModelStore`s from the worker's cache, and for each node downloading the digest asks that
  node's plugin for its running downloads over the plugin's existing secure port (a new read-only
  path answering digest, bytes and source, the same trust as the device manager's snapshot the
  Instance `metrics` subresource reads). A node that does not answer within 2 seconds contributes its
  stored value and is not counted in `live`; the whole request is bounded at 5 seconds.
- **It is an aggregate and names no node.** It is namespaced and a tenant reads it; node names would
  hand every tenant the cluster's topology. Readers that need the per-node view (administrators,
  GPUStack server) read the `v1` `NodeModelStore`s.
- Authorization is Kubernetes' own: `get` on `modelartifacts/progress` in the namespace, checked by
  the aggregated API server's delegated authorization, the same as `modeldeployments/metrics`. A
  tenant that may read one namespace's progress reads nothing about another namespace's artifacts.
- An artifact that is not resolved, or has a claim source, answers zero counts and a `reason`.

### The v1 views

- `ModelArtifact` (namespaced) proxies the `v1alpha1` resource, like `ModelDeployment`: get, list,
  watch, create, update, patch, delete, and the `progress` subresource. **It has no writable
  `status` subresource**: the proxy writes with the worker's identity, and the status guard lets
  exactly that identity write a `ModelArtifact`'s status, which is what mounts are authorized by.
  Status is read through the object. Printer columns: Source (the repository, or `claim/<name>`),
  Revision (12 characters), Size, Ready, Downloading, Resolved, and Age with `-o wide` (the
  aggregated API server's table puts Age last and at a lower priority). The `v1alpha1` CRD gains a
  Ready column too.
- `NodeModelStore` (cluster-scoped) serves get, list, watch and delete in `v1`, never create or
  update. Its writers keep using `v1alpha1`. Delete is for the garbage collector: it watches each
  resource at the group's preferred version, `v1` here, and only resources that version can delete,
  so without it no NodeModelStore is collected with its Node (measured on Kubernetes 1.36, where a
  NodeModelStore with a dangling owner stayed while 1.29 collected it). Printer columns: Ready,
  Used, Models (count), Downloading (count), and Age with `-o wide`.

### Capability map for GPUStack server

By capability, not by field (GPUStack server's `ModelFile` and its routes as read on 2026-09-24).

| GPUStack server | This API | Gap and its owner |
| --- | --- | --- |
| A model source: Hugging Face repo, ModelScope model, local path | `ModelArtifact.spec.source`: `huggingFace`; a local path is a `persistentVolumeClaim` source | ModelScope is reserved in the API and not opened (a later source change) |
| `huggingface_filename` / `model_scope_file_path` (one file of a repo, a GGUF) | `allowPatterns: ["<file>"]` | none |
| A model file per worker, its `state` (`downloading`, `ready`, `error`) and `state_message` | the node's `NodeModelStore.status.models[]` entry: `state` (`Downloading`, `Ready`, `Failed`), `reason`, `message` | none |
| `download_progress` and `size` | `downloadedBytes` / `sizeBytes` on the entry (5% steps); live through `progress`; the artifact's `status.nodes.downloadingPercent` | none |
| `resolved_paths` on the worker's disk | the fixed mount path in the consumer's container; the host path is never exposed | by design |
| `local_dir` (choose where a file lands) | none: the plugin owns the cache layout | by design |
| Listing a worker's model files | `v1` `NodeModelStore` get/list/watch | none |
| Creating a model file (download to a worker ahead of use) | `ModelPrefetch` naming nodes | S6 |
| Deleting with `cleanup_on_delete` | deleting the `ModelPrefetch`; collection after the last reference and its grace | S6; today only the watermark collects |
| `reset` (retry a failed file now) | automatic backoff to `retryTime` | an explicit retry is not planned; recorded as a gap |
| `source_index` (one row per source per worker) | the digest keys one entry per node; artifacts with the same content share it | none |
| The locality scorer (prefer workers holding the files) | placement preference, compute first | S4 |
| `is_lora` / `base_model`, draft-model files | not in this batch; `ModelDeployment` keeps room for them | later |
| Tenant scope (`owner_principal_id`, cluster) | the artifact's namespace; the node object names no tenant | none |

### User Stories

#### Story 1

As an operator of GPUStack server, I want each node's model content and its download progress in a
`v1` resource I can list and watch, so that the model-file view I show today keeps working after the
worker stops downloading files itself.

#### Story 2

As a tenant starting a deployment of a 145 GB model, I want to see how far its weights are on the
nodes fetching them, so that I know whether to wait or to look for a failure.

#### Story 3

As a cluster administrator, I want progress reporting to cost a bounded, small number of API writes
per download and nothing at rest, so that a fleet downloading large models does not load etcd.

#### Story 4

As a cluster administrator whose nodes keep the cache on the kubelet disk, I want the cache's high
watermark to follow the thresholds kubelet actually uses and to hold when several downloads start
together, so that the cache never causes an eviction or an image collection.

#### Story 5

As a cluster administrator, I want a node whose cache configuration broke, or whose kubelet
threshold is nonsensical, to stop collecting and say why, rather than delete content under rules
nobody configured.

### Core Features & Acceptance Criteria

#### F1 - NodeModelStore progress and source

- An entry of a running download carries `sizeBytes` once the manifest is listed and
  `downloadedBytes` that never decreases across a plugin restart and resume: what the attempt holds,
  the checkpoints a resume carried over plus what arrived since (it goes back only when the hub
  refuses byte ranges and a resumed file starts over); `source` is `Hub` for a Hub download and the content it published.
- Unit: a progress-only change within 30 seconds or under 5% writes nothing; one past both writes;
  a state change writes at once with the current progress; `storedBytes` moving alone during a
  download writes nothing;
  the object's own status update does not trigger a report, a spec change does. Each of these fails
  against the current reporter.
- e2e (kind, a download slowed with the bandwidth Setting to last at least three minutes, the
  object watched from the start): progress is visible in at least two intermediate writes, no two
  progress-only writes are less than 30 seconds apart, progress-only writes are at most 20, and the
  node writes nothing for one report interval (5 minutes) after the Pod runs.

#### F2 - Kubelet thresholds and the cap

- The worker writes `spec.kubelet` from `configz`; a node whose `configz` was never read has none,
  and its plugin's `Ready` message says the effective configuration could not be read and defaults
  apply; a failed refresh keeps the last reading. Unit tests cover these, with a reader and an API
  server that answer, fail and hang.
- A quantity threshold at or above the filesystem's size is ignored with a message naming it; a unit
  test on the cap fails against the current code (the #634 criterion).
- A change of `spec.kubelet` changes the plugin's effective watermarks without a restart.
- The plugin reads no kubelet file (`--kubelet-dir` stays, for the volume targets).
- `#641` closes with this: its criteria are the three bullets above.

#### F3 - Collection rules

- Unit: two reservations that each fit and together exceed the high watermark admit one; the test
  fails against the current `Reserve`.
- Unit: a valid spec applied, then made invalid, and an unreferenced tree past its grace above the
  high watermark stays; `Ready` says collection is skipped (the #633 criterion).

#### F4 - ModelArtifact aggregation

- `status.nodes` counts ready, downloading and failed nodes for the digest and the mean percent in
  steps of 5, absent for a claim; a unit test on a fake client with three stores covers counts, mean
  and rounding, and one that the mean ignores ready nodes.
- Writes: at most one per artifact in any 30 seconds, the first change at once and a later one when
  the window ends; a progress change within the same 5% step produces none; two artifacts sharing a
  digest are each enqueued and written.
- e2e: during the F1 download the artifact's `downloading` is 1 and its percent moves in multiples
  of 5; after publication `ready` counts the node, `downloading` is 0 and the percent absent.

#### F5 - v1 views and progress

- `kubectl get modelartifacts.v1.worker.gpustack.ai` and `nodemodelstores.v1.worker.gpustack.ai`
  list the printer columns above; `v1` NodeModelStore refuses create and update and serves delete.
- A status change sent to the `v1` ModelArtifact does not change the stored status (e2e: an attempt
  through the main resource leaves `status.resolved` unchanged; `status` is not a `v1` subresource).
- `progress` answers the counts and live bytes for a running download (e2e: two reads a few seconds
  apart during the F1 download show `live: 1` and non-decreasing `downloadingBytes`), and the stored
  value, not counted in `live`, when the plugin is unreachable (unit, with a server that hangs).
- e2e authorization: a service account with `get modelartifacts/progress` in namespace A reads A's
  progress, is refused for namespace B's artifact, and one with only `get modelartifacts` in A is
  refused `progress` in A.
- e2e: the tenant's `progress` response contains no node name of the cluster.

#### F6 - Lifecycle

- e2e: deleting a Node object removes its `NodeModelStore` through the owner reference (the node
  is registered again afterwards and gets a new one).
- e2e: after the plugin restarts on a node holding content, the node's status lists the same
  digests, states and sizes, rebuilt from disk, and deleting the object (the worker recreates it)
  ends with the same entries rewritten by the plugin.
- e2e: the JSON of every `NodeModelStore` contains none of the test namespaces, artifact names,
  repositories or Pod names.

#### F7 - Documentation

- A new reference page for the `v1` views, the `progress` subresource, its authorization and the
  Role a tenant needs, and the capability map; the NodeModelStore reference and the model-store
  operation page gain the progress fields, the write rule, the configz source of thresholds and the
  two collection rules; `docs/README.md` and the documentation skill's routing table list the page.

### Notes / Constraints / Caveats

#### Platform capabilities and the version floor

| Capability | Earliest default-on version | Source |
| --- | --- | --- |
| Aggregated API server, `apiregistration.k8s.io/v1` | 1.10 (GA) | Kubernetes API reference |
| Delegated authentication and authorization (`TokenReview`, `SubjectAccessReview` v1) | 1.6 (GA) | Kubernetes API reference |
| Subresource authorization (`resource/subresource` in RBAC) | 1.6 (GA) | RBAC docs |
| Node proxy `nodes/<n>/proxy` | before 1.23 | Kubernetes API reference |
| kubelet `/configz` | before 1.23; served only with `enableDebuggingHandlers` (default true) | `pkg/kubelet/server/server.go` at v1.23.0 (`InstallDebuggingHandlers`) and v1.35.0 (`InstallAuthRequiredHandlers`) |
| CRD `status` subresource, structural pruning | 1.16 (GA) | Kubernetes API reference |
| Owner-reference garbage collection of a cluster-scoped object owned by a Node | before 1.23 | Kubernetes GC docs |

Nothing here raises a floor: every capability exists at the chart's install floor 1.23, so the chart
installs and the worker starts on 1.23–1.28 as before, and node delivery keeps its 1.29 floor. A
cluster that disables kubelet's debugging handlers has no `configz`; its nodes use kubelet's default
thresholds and say so, which is the behavior today. The kind e2e runs on 1.29 and the current
default; the chart CI test runs locally on all seven of its node images.

#### Measurements this design rests on

All on one managed Kubernetes 1.35.7 cluster, two CPU nodes, the cache on a 265 GB ext4 boot disk
shared with kubelet and containerd, kubelet thresholds `nodefs.available` 10% and image collection
85/80:

- Filled to 78.46% under an 80% high watermark: no `DiskPressure`, no eviction, no image collection
  in 2734 lines of kubelet's log, the image list unchanged; a 65.5 GB download refused with
  `InsufficientCapacity`. Two concurrent 3.1 GB downloads with 4.09 GB free: usage 80.80%.
- Status writes of one node: the prototype holding every download-moving field, 22 writes over a
  36.8-minute, 119.7 GB download, at most 2 in any minute, a median of 110 seconds apart; the
  prototype holding only the progress field, 828 writes in its first minute, 0 to 10 ms apart; the
  released plugin, 4 writes for a 15.24 GB download; at rest, none in 10 minutes.
- The prototype's progress counted only the running attempt's bytes: after a resume its first value
  was 0.1 GB with 25.7 GB of the content already on disk.

#### Implementation constraints

- The progress write rule lives in the reporter; the plugin's cache watches the same objects as
  today. The worker's configz read goes through its existing clientset with a 5-second timeout and
  runs in the NodeModelStore controller.
- The ModelArtifact controller maps a NodeModelStore change to its artifacts by listing them and
  matching `status.resolved.manifestDigest`, for the digests whose state or 5% step the change moved
  on that node.
- `progress` reads at most 64 downloading nodes live per answer, 16 at a time; the rest contribute
  their stored values.
- The ModelDeployment controller's NodeModelStore predicate compares entries without the progress
  fields, so progress writes do not reconcile deployments.
- The plugin's new read-only path is on its existing secure port; the chart's port and probes do not
  change. `progress` reaches the plugin by the plugin Pod's IP, without a proxy, the way the Instance
  `metrics` subresource reaches the device manager.

### Boundaries

- **Always:** keep tenant names out of `NodeModelStore`, including messages; hold every
  download-moving field to one threshold; count bytes on disk; compute `progress` on read and write
  nothing; authorize `progress` by Kubernetes RBAC on the subresource; keep the capability map in this
  spec in sync with the code it names.
- **Ask first:** giving the plugin any new permission; a field on a tenant-writable object; opening
  a writable `status` on a `v1` view; aggregated roles for the group; a new port or Service.
- **Never:** name a node in `progress`; write progress at every byte; let the ModelArtifact status move below a 5% step;
  expose a host path; let the proxy's identity write a `ModelArtifact`'s status for a caller; collect
  under a configuration the node reports as not in force.

### Risks and Mitigations

- A `v1` subresource that proxies a write to `v1alpha1` writes with the worker's identity, which
  every identity-based guard of this group trusts → any such subresource added later answers first
  whether a guard keyed on the worker would let a caller through it; `status` on `ModelArtifact` is
  closed for exactly this reason.
- `v1` becomes the group's preferred version for both resources, so kubectl's unversioned names
  (`nms`, `modelartifacts.worker.gpustack.ai`) reach the `v1` views → a write to a NodeModelStore or
  to a ModelArtifact's status must name `v1alpha1`, which the operator's own clients always do, and so
  must the read of an object written back (a `v1` read carries `apiVersion: worker.gpustack.ai/v1`,
  which the `v1alpha1` endpoint refuses, measured in the status-guard case); the documentation says
  so and the e2e cases that probe the status guards name `v1alpha1`.
- A fleet of many nodes downloading one digest writes each artifact naming it often → 30-second
  window per artifact and 5% steps; the controller's writes per artifact are bounded by the window
  whatever the node count.
- The plugin's live path is unreachable (NetworkPolicy, a restart) → `progress` falls back to the
  stored value and is not counted in `live`; nothing depends on the live path.
- `configz` is disabled or slow on some nodes → defaults and a message, as today; the read is timed
  out and retried on the next reconcile, never blocking the spec's other fields.
- A kubelet threshold changed without a kubelet restart is not in `configz` → it is not in force
  either; `configz` shows what kubelet enforces.
- Counting in-flight reservations refuses a download that would have fit after another failed →
  the refusal backs off one minute and retries against the new usage.

## Design Details

### Commands

Unit tests and lint run locally (macOS); the plugin's Linux files build, vet and test in a Linux
container; the e2e runs on local kind clusters whose kubeconfig lives outside the repository and is
passed explicitly, never through the default context.

```bash
make generate                                  # after the API types change; commit only its paths
make lint                                      # Go and shell (an edit pass: run it alone)
make lint docs                                 # Markdown and specs
go test ./pkg/modelmanager/... ./pkg/worker/... ./pkg/modelstore/... ./api/...
docker run --rm -v "$PWD":/src -w /src golang:<go.mod version> \
  sh -c 'go build ./pkg/modelmanager/... && go vet ./pkg/modelmanager/... && go test ./pkg/modelmanager/...'
bash .agents/skills/_e2e-lib/scripts/build-load.sh dev-<short sha>        # kind: build here, load into the nodes
bash .agents/skills/_e2e-lib/scripts/deploy.sh gpustack-system dev-<short sha>
bash .agents/skills/gpustack-operator-e2e/cases/case-108.sh <ns>          # and case-109, case-101..103
```

### Project Structure

```text
api/worker/v1alpha1/node_model_store.go        downloadedBytes, source, spec.kubelet
api/worker/v1alpha1/model_artifact.go          status.nodes
api/worker/v1/model_artifact.go                the v1 view and ModelArtifactProgress
api/worker/v1/node_model_store.go              the v1 view: reads and delete
pkg/modelstore/                                spec.kubelet validation
pkg/modelmanager/report/                       the write rule, the tick, the cap from spec.kubelet
pkg/modelmanager/gc/                           in-flight reservations, the invalid-spec skip, the quantity bound
pkg/modelmanager/materialize/, store/          bytes on disk per attempt, the source in the marker
pkg/modelmanager/downloads.go                  GET /model/downloads on the secure port
pkg/worker/controllers/worker/node_model_store.go      configz into spec.kubelet
pkg/worker/controllers/worker/model_artifact_nodes.go  status.nodes, the one aggregation function
pkg/worker/extensionapis/worker/model_artifact*.go, node_model_store.go   v1 views, progress
docs/reference/model-artifact-views.md         new: v1 views, progress, the capability map
docs/reference/, docs/operation/model-store.md, docs/README.md
.agents/skills/gpustack-operator-e2e/cases/case-108.sh, case-109.sh
```

### Code Style

The reporter's existing shape, which the write rule extends: compute the whole status, compare it
semantically, and write only when it differs.

```go
// Semantically: a time read back from the API is in the local zone, and equal all the same.
if equality.Semantic.DeepEqual(status, nms.Status) {
	return nil
}
nms.Status = status
if err := r.Client.Status().Update(ctx, nms); err != nil {
```

Comments state the rule and its reason; table-driven tests with fake clients; no tenant string in a
cluster-scoped object or its messages.

### Implementation Plan

Tasks run in order, one at a time (the tasks share the reporter and the generated API, so no two run
together). Each task starts with `git fetch origin && git rebase origin/main` and the affected
packages' tests. Checkpoints: after T4 the node side is complete and runs on kind alone; after T6
every API surface exists; T8 proves it end to end.

- [x] **T1 · API fields and generation**
      Blocked by: None
      Owns: `api/worker/v1alpha1/node_model_store.go`, `api/worker/v1alpha1/model_artifact.go`,
      `api/**/zz_generated.*`, `api/**/generated.*`, `pkg/kubeclients/**`, `pkg/modelstore/**`
      Gate: review
      Acceptance: `status.models[].downloadedBytes` and `.source` (enum `Hub`), `spec.kubelet`
      (`nodefsAvailable`, `imagefsAvailable`, `imageGCHighThresholdPercent`) and
      `ModelArtifact.status.nodes` (`ready`, `downloading`, `failed`, `downloadingPercent`) exist in
      the CRDs with their bounds; `modelstore.Validate` accepts an absent `spec.kubelet` and rejects a
      threshold that is neither a percentage nor a quantity; no writer sets them yet, so behavior is
      unchanged.
      Verify: `make generate && git status --short` (only this task's paths), `go test ./api/... ./pkg/modelstore/...`

- [x] **T2 · Collection rules: in-flight reservations, invalid spec, oversized threshold**
      Blocked by: None
      Owns: `pkg/modelmanager/gc/**`, the watermark and apply parts of `pkg/modelmanager/report/report.go`
      Gate: review
      Acceptance: (#633) a valid spec applied then made invalid leaves an unreferenced tree past its
      grace above the high watermark, and `Ready` says collection is skipped; (3a) two reservations
      that each fit and together exceed the high watermark admit only one; (#634) a quantity
      threshold at or above the filesystem's size falls back to the default and the cap's source says
      which threshold was ignored. Each test is red on the base.
      Verify: `go test ./pkg/modelmanager/gc/... ./pkg/modelmanager/report/...`, each new test also run against the base code (red)

- [x] **T3 · Kubelet thresholds from configz**
      Blocked by: T1, T2
      Owns: `pkg/worker/controllers/worker/node_model_store*.go`, `pkg/modelmanager/manager.go`,
      `pkg/modelmanager/gc/watermark.go`, the cap part of `pkg/modelmanager/report/report.go`
      Gate: review
      Acceptance: the NodeModelStore controller reads `nodes/<n>/proxy/configz` (5-second timeout)
      when it writes a node's object without a reading younger than 30 minutes, and writes
      `spec.kubelet`; a failed read keeps the last reading (a node never read has none) and is logged; the plugin computes the cap from `spec.kubelet` and the
      filesystem's size, with kubelet's defaults and a `Ready` message when it is absent, and reads no
      kubelet file; a change of `spec.kubelet` changes the effective watermarks without a restart.
      Unit tests with a reader and an API server that answer, fail and hang.
      Verify: `go test ./pkg/worker/controllers/worker/... -run 'NodeModelStore' ./pkg/modelmanager/...`

- [x] **T4 · Plugin progress: bytes on disk, source, the write rule, the live path**
      Blocked by: T1, T2
      Owns: `pkg/modelmanager/report/**`, `pkg/modelmanager/materialize/**`, `pkg/modelmanager/store/**`,
      `pkg/modelmanager/manager.go`, a new `pkg/modelmanager/downloads.go`
      Gate: review
      Acceptance: a running entry carries `sizeBytes` and `downloadedBytes` counted from disk,
      including a resumed attempt's bytes; `source` is `Hub` and is kept in the published marker; the
      reporter writes a progress-only change only after 30 s and a 5% step, a state change at once,
      never on `storedBytes` alone; a 10-second tick while downloading; its own object triggers a
      report only on a generation change; `GET /model/downloads` on the secure port answers the
      running downloads (digest, bytes, size, source) and nothing else. Each write-rule test is red
      on the base reporter.
      Verify: `go test ./pkg/modelmanager/...`; then in a Linux container `go build ./pkg/modelmanager/... && go vet ./pkg/modelmanager/... && go test ./pkg/modelmanager/...`

- [x] **T5 · ModelArtifact aggregation**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/model_artifact.go`, a new
      `pkg/worker/controllers/worker/model_artifact_nodes.go` and its test, the NodeModelStore
      predicate in `pkg/worker/controllers/worker/model_artifact_placement.go`
      Gate: review
      Acceptance: `status.nodes` counts and the downloading mean in steps of 5 per the proposal; one
      write per artifact per 30-second window; a NodeModelStore change enqueues only artifacts whose
      digest's counts or step changed; absent for a
      claim; the ModelDeployment predicate ignores the progress fields. The aggregation is the pure
      function T6 wrote in `pkg/modelstore`; the artifacts a store change affects are found by listing
      them and matching the digest, not by a field index. The ModelDeployment predicate landed last,
      after the placement-preference spec that shares its file merged.
      Verify: `go test ./pkg/worker/controllers/worker/... -run 'ModelArtifact|NodeModelStore|ModelDeployment'`

- [x] **T6 · v1 views and the progress subresource**
      Blocked by: T4 (built before T5, which waits on another spec's merge; T6 wrote the shared aggregation in `pkg/modelstore` that T5 reuses)
      Owns: `api/worker/v1/model_artifact*.go`, `api/worker/v1/node_model_store.go`, `api/worker/v1/zz_generated.*`,
      `api/worker/v1/generated.*`, `api/worker/zz_generated.openapi*`, `pkg/kubeclients/**`,
      `pkg/worker/extensionapis/worker/model_artifact*.go`, `pkg/worker/extensionapis/worker/node_model_store.go`,
      `pkg/worker/extensionapis/setup.go`
      Gate: review
      Acceptance: `v1` ModelArtifact proxies CRUD without a `status` subresource and serves
      `progress`; `v1` NodeModelStore serves get, list, watch and delete only; printer columns as specified;
      `progress` returns the aggregate with no node name, reads each downloading node's plugin with a
      2-second bound and 5 seconds overall, and counts only the nodes read live in `live`. Unit tests:
      a fake store set with an answering, a failing and a hanging plugin; an unresolved and a claim
      artifact; a response searched for every node name.
      Verify: `make generate && go test ./pkg/worker/extensionapis/... ./api/...`

- [x] **T7 · Documentation**
      Blocked by: T3, T6
      Owns: `docs/reference/model-artifact-views.md` (new), `docs/reference/node-model-store.md`,
      `docs/reference/model-artifact.md`, `docs/operation/model-store.md`, `docs/README.md`,
      `.agents/skills/gpustack-operator-docs/SKILL.md`, `.claude/skills/gpustack-operator-overview/**`
      Gate: None
      Acceptance: the new page holds the `v1` views, `progress` (shape, authorization, the tenant
      Role) and the capability map; the other pages gain the progress fields, the write rule, the
      configz source and the two collection rules; every page stays under its limits (1000 lines, ten
      `##`); the README table and the routing table list the page.
      Verify: `make lint docs`

- [x] **T8 · End to end**
      Blocked by: T6
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-108.sh`, `.agents/skills/gpustack-operator-e2e/cases/case-109.sh`,
      `.agents/skills/gpustack-operator-e2e/cases/_model-hub-lib.sh`, `.agents/skills/gpustack-operator-e2e/SKILL.md`
      Gate: None
      Acceptance: case-108 (progress, throttle, aggregation, `progress` authorization, no tenant name
      in any NodeModelStore, no node name in the tenant's `progress`) and case-109 (Node deletion
      collects its object, plugin restart and object deletion rewrite the same entries from disk) pass
      on kind 1.29 and the current default with the image built from the branch head, its imageID
      checked; case-101 to case-103 still pass; the chart CI test passes locally on all seven node
      images.
      Verify: the commands under Commands, each run's output in the evidence directory

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/modelmanager/report`: the test environment gains a progress reading and a clock the cases
  advance, so a write-rule case states elapsed time and bytes as data.
- `pkg/modelmanager/gc`: a collector fixture with running attempts and their reserved bytes.
- `pkg/worker/controllers/worker`: a fake `configz` endpoint (answering, failing, hanging) behind the
  NodeModelStore controller's reader.
- `.agents/skills/gpustack-operator-e2e/cases/_model-hub-lib.sh`: a reader that records every write
  of one `NodeModelStore` (the raw watch the real-node run used) and a reader of an artifact's
  `progress` as a given service account.

#### Unit tests

Table-driven, fake clients, one behavior per case; every regression case is also run against the
base code and must fail there.

- `pkg/modelmanager/report`: 2026-09-26 - to be measured at T4 (write rule: time floor, 5% step,
  state change carries progress, `storedBytes` alone, own-status trigger, generation trigger;
  `downloadedBytes` after a resume; `source`; the invalid-spec skip message; the absent-configz
  message).
- `pkg/modelmanager/gc`: 2026-09-26 - to be measured at T2 (in-flight reservations; skip while
  invalid; quantity at, above and below the filesystem's size; percentage bounds unchanged).
- `pkg/modelmanager/materialize`, `pkg/modelmanager/store`: 2026-09-26 - to be measured at T4 (bytes
  on disk per attempt across a resume; the marker's source, old markers without it).
- `pkg/modelmanager` (the downloads path): 2026-09-26 - to be measured at T4 (answers running
  downloads only, no tenant field).
- `pkg/worker/controllers/worker`: 2026-09-26 - to be measured at T3/T5 (configz to `spec.kubelet`,
  refresh, failure; aggregation counts, mean, steps, window, digest mapping, claim; the deployment
  predicate ignoring progress).
- `pkg/worker/extensionapis/worker`: 2026-09-26 - to be measured at T6 (`progress` aggregate, live
  bound, fallbacks, unresolved and claim artifacts, no node name; v1 NodeModelStore refusing writes;
  v1 ModelArtifact without a status subresource).
- `pkg/modelstore`: 2026-09-26 - to be measured at T1 (`spec.kubelet` validation).

#### Integration tests

None beyond the e2e below: the aggregated API server, kubelet's `configz` and the plugin's live path
are exercised only against a real cluster.

#### e2e tests

On kind (one control plane, two workers) at 1.29 and the current default node image, with the image
built from the branch head and every worker and plugin Pod's imageID checked:

- case-108: a download from the test hub, throttled to last at least three minutes, watched from the
  start: at least two intermediate progress writes, none closer than 30 seconds, at most 20
  progress-only writes, nothing for one report interval after the Pod runs; the artifact's
  `status.nodes` moving in multiples of 5 and ending with the node counted ready; two `progress` reads during the
  download with `live: 1` and non-decreasing bytes; `progress` allowed with `get
  modelartifacts/progress` in namespace A, refused for namespace B and without the subresource rule;
  the tenant's response containing no node name; no test namespace, artifact, repository or Pod name
  in any `NodeModelStore`; a status change sent to the `v1` ModelArtifact leaving the stored status
  unchanged; `v1` NodeModelStore refusing an update.
- case-109: deleting a worker's Node object removes its `NodeModelStore` (the node registers again);
  a plugin restart and a deletion of the object each end with the same digests, states and sizes
  rewritten from disk.
- Regression: case-101, case-102, case-103.
- The chart CI test (`chart.yml`'s `ct install`) run locally on its seven node images, v1.23.17 to
  v1.35.5, with only `image.tag` changed to this branch's image, each rc=0: the chart must install
  and the worker must start on 1.23 to 1.28.

## Alternatives

### Progress only through the subresource, nothing stored

Rejected: GPUStack server lists and watches; a list that has to fan out to every node's plugin on
each read scales with the fleet, and a node that is not answering would show nothing. Stored
thresholds give watchers a cheap, bounded signal, and the subresource adds the live detail.

### A time floor alone (every 30 seconds)

Rejected: a slow download would write every 30 seconds for hours; the 5% step bounds a download to
20 progress writes whatever its speed, and the time floor bounds a fast one.

### Flat fields `readyNodes`, `downloadingNodes`

Equivalent in content. A `nodes` group keeps the counts and the mean that describes them together and
leaves room for S4 and S5 without more top-level fields.

### Node names in `progress`

Rejected: `progress` is namespaced and tenants read it, and a per-node list would give every tenant
the cluster's topology, against the rule that tenants see only the aggregate of their artifact. The
per-node view is the `v1` `NodeModelStore`, which only administrators and GPUStack server read.

### The plugin reads `configz` itself

Rejected: it needs `nodes/proxy`, which reaches exec and logs on every kubelet. Reading
`/proc/<kubelet>/cmdline` for `--config` needs the host's PID namespace and still misses flags and
drop-ins.

### Keep the previous watermarks while the spec is invalid

Rejected (#633): measured to delete 29.55 GB under a configuration the node reported as not in force.

### Run at the 2% floor for a threshold larger than the disk

Rejected (#634): measured to remove every unreferenced tree at start. Refusing to apply the whole
configuration would stop every download on the node for a kubelet value the cache cannot act on.

## Open Questions

None open. Decided while the spec was written, by the user (2026-09-27, each the recommended option)
and recorded here:

1. The mean covers only the nodes downloading; ready nodes are counted separately, so a node starting
   a download does not pull 100% down to 75%.
2. Each node downloads a whole copy; the mean is across concurrent downloads, not a split download.
3. The per-artifact aggregation is throttled on its own even when many artifacts share a digest: a
   write when a count or a 5% step changes, at most one every 30 seconds.
4. Readers that need finer progress read `progress`, computed on request and never stored; it reads
   the running downloads live from the nodes, with the stored value as the fallback.

Decided by the coordinator: `progress` names no node (see Alternatives), `status.nodes` is a group
rather than flat fields, and `v1` stays the group's preferred version with `delete` on the
NodeModelStore view (see Risks).
