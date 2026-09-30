# Model Artifact API

GPUStack server and consoles use `worker.gpustack.ai/v1` to manage model artifacts and inspect
node caches. A node's download progress [is stored on
thresholds](node-store.md#the-resource); `progress` answers between them, on request, and
stores nothing.

## Contents

- [Resources](#resources)
- [The progress subresource](#the-progress-subresource)
- [Authorization](#authorization)
- [Capability map for GPUStack server](#capability-map-for-gpustack-server)
- [Requirements and limits](#requirements-and-limits)
- [Table columns](#table-columns)

## Resources

| Resource | Scope | Verbs | Printer columns |
| --- | --- | --- | --- |
| `modelartifacts.v1.worker.gpustack.ai` | namespaced | create, get, list, watch, update, patch, delete; subresource `progress` | Name, Source, Revision (12 characters), Size, Ready, Downloading, Resolved; Age with `-o wide` |
| `nodemodelstores.v1.worker.gpustack.ai` | cluster | `get`, `list`, `watch`, `delete` | Name, Ready, Used, Models, Downloading; Age with `-o wide` |

```bash
kubectl get modelartifacts.v1.worker.gpustack.ai -n team-a
kubectl get nodemodelstores.v1.worker.gpustack.ai
```

`worker.gpustack.ai/v1` is the public API. A bare `kubectl get nms` or
`kubectl get modelartifacts.worker.gpustack.ai` uses it.

- ModelArtifact exposes no writable `status` subresource. The worker owns its status, which is
  used to authorize model mounts. Updating the main resource preserves the stored status.
- NodeModelStore supports reads and deletion. The worker manages its `spec`, and each node's plugin
  reports `status`. Deletion supports Kubernetes garbage collection; the worker recreates the object
  while its node runs the plugin.
- Resource fields are described in [Model Artifact](artifact.md#the-resource) and
  [Node Model Store](node-store.md#the-resource).

## The progress subresource

```bash
kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/<ns>/modelartifacts/<name>/progress"
```

```yaml
kind: ModelArtifactProgress
apiVersion: worker.gpustack.ai/v1
metadata: {name: qwen-72b, namespace: team-a}
timestamp: "2026-09-26T00:00:00Z"    # when it was computed
manifestDigest: sha256:...
sizeBytes: 145424101604
ready: 1                             # nodes holding the content published
downloading: 2                       # nodes downloading it
failed: 1                            # nodes waiting to retry a failed attempt
downloadingPercent: 42               # the downloading nodes' mean, whole percent
downloadingBytes: 123482472448       # what the downloading nodes hold, summed
live: 2                              # downloading nodes read from their plugin for this answer
failureReasons:
  - {reason: SourceUnavailable, count: 1}
```

- **It is computed on each request and never written.** The worker reads the artifact and the
  `NodeModelStore`s listing its digest; for each node downloading it, it reads that node's plugin
  (`GET /model/downloads` on its HTTPS port) within 2 seconds, the fixed part within 5 seconds, the
  whole answer within 13 seconds. Live reads cover at most 64 downloading nodes; a node past that
  cap, or one that does not answer, contributes the bytes it last wrote and is not counted in
  `live`.
- **It names no node.** It is namespaced and tenants read it; node names would give every tenant the
  cluster's topology. Read NodeModelStore for the node-level details.
- **The mean covers the downloading nodes only**, each downloading a whole copy; a node starting a
  download does not pull the ready ones down. `downloadingPercent` is absent while none downloads.
- **Nothing to count** answers zeros and a `reason`: the artifact is not resolved yet, or its claim
  source is mounted from its volume and never downloaded.
- **The counts describe the content.** Artifacts with the same digest, in any namespace, see the same
  nodes; only numbers cross.

## Authorization

The aggregated API server authorizes each request with Kubernetes RBAC through delegated
authorization: `get` on `modelartifacts/progress` in the artifact's namespace. `get modelartifacts`
alone does not grant it. The chart grants tenants nothing in this group; an administrator gives a
tenant's subjects a Role such as:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: model-artifact-reader, namespace: team-a}
rules:
  - apiGroups: [worker.gpustack.ai]
    resources: [modelartifacts, modelartifacts/progress]
    verbs: [get, list, watch]
```

A subject with that Role in `team-a` reads `team-a`'s progress and is refused another namespace's.
`nodemodelstores` is cluster-scoped: grant it with a ClusterRole to administrators and GPUStack
server only.

## Capability map for GPUStack server

By capability, not by field.

| GPUStack server | This API | Gap and owner |
| --- | --- | --- |
| A source: Hugging Face repository, ModelScope model, local path | `ModelArtifact.spec.source`: `huggingFace`; a local path is a `persistentVolumeClaim` source | ModelScope is reserved and refused, a later source change |
| One file of a repository (`huggingface_filename`, a GGUF) | `allowPatterns: ["<file>"]` | none |
| A model file per worker, its `state` and `state_message` | the node's `status.models[]` entry: `state`, `reason`, `message` | none |
| `download_progress` and `size` | the entry's `downloadedBytes` and `sizeBytes`; live through `progress`; the artifact's `status.nodes` | none |
| `resolved_paths` on the worker | the fixed mount path in the consumer's container; host paths are never exposed | by design |
| `local_dir` | none: the plugin owns the cache layout | by design |
| Listing a worker's model files | NodeModelStore get, list, watch | none |
| Download to a worker ahead of use | a [ModelPrefetch](prefetch.md) naming nodes | none |
| Delete with `cleanup_on_delete` | delete the prefetch; collection after the last reference and its grace | none |
| `reset` (retry now) | automatic backoff to `retryTime` | not planned |
| One row per source per worker | one entry per digest per node, shared by artifacts with the same content | none |
| Prefer workers holding the files | [placement preference](../topology/scheduling.md#a-node-delivered-model-prefers-the-nodes-holding-it), compute first | none |
| LoRA and draft-model files | not in this batch | later |
| Tenant scope | the artifact's namespace; the node store names no tenant | none |

## Requirements and limits

- **The aggregated API**, `apiregistration.k8s.io/v1`, and delegated authentication and
  authorization (`TokenReview`, `SubjectAccessReview`): available on every version the chart
  installs on.
- **Live bytes need the plugin's Pod reachable from the worker** on its HTTPS port. A NetworkPolicy
  that blocks it leaves the stored values, and `live` says how many nodes answered.
- **No watch on `progress`**: it answers one request. Watch NodeModelStores for changes.

---

**See also** — [Model Artifact](artifact.md) for the artifact itself ·
[Node Model Store](node-store.md) for each node's entries and the progress write rule ·
[Model Deployment Metrics](../model-deployment/metrics.md) for the other subresource built
the same way.

**Next** → [Node Model Store](node-store.md)

## Table columns

The aggregated API server renders the printer columns with its own `TableConvertor`
(`NewJSONPathTemplateTableConvertor`, under `pkg/worker/extensionapis/worker/`). The columns in the
resource table above describe the output of `kubectl get`.

**See also** — [Node-to-Node Sync](peer-sync.md) for where a node's bytes come from,
and [Node Model Store](node-store.md) for the status the views project.
