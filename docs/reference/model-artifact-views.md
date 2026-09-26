# Model Artifact Views Reference

> **Purpose** — the `v1` views of `ModelArtifact` and `NodeModelStore`, the `progress` subresource
> that says where an artifact's content is, who may read it, and how GPUStack server's model-file
> handling maps onto this API.
> **Audience** users, operators, console developers · **Prerequisites** [Model Artifact
> Reference](model-artifact.md) · **Read time** ~8 min

The aggregated API serves `worker.gpustack.ai/v1` beside the `v1alpha1` resources: the read surface
GPUStack server and consoles use. A node's download progress is stored on thresholds, every 5% and
at most every 30 seconds; `progress` answers between them, on request, and stores nothing.

## Contents

- [The v1 views](#the-v1-views)
- [The progress subresource](#the-progress-subresource)
- [Authorization](#authorization)
- [Capability map for GPUStack server](#capability-map-for-gpustack-server)
- [Requirements and limits](#requirements-and-limits)

## The v1 views

| View | Scope | Verbs | Printer columns |
| --- | --- | --- | --- |
| `modelartifacts.v1.worker.gpustack.ai` | namespaced | every verb, proxied to `v1alpha1`; subresource `progress` | Name, Source, Revision (12 characters), Size, Ready, Downloading, Resolved; Age with `-o wide` |
| `nodemodelstores.v1.worker.gpustack.ai` | cluster | `get`, `list`, `watch`, `delete` | Name, Ready, Used, Models, Downloading; Age with `-o wide` |

```bash
kubectl get modelartifacts.v1.worker.gpustack.ai -n team-a
kubectl get nodemodelstores.v1.worker.gpustack.ai
```

`v1` is the group's preferred version, so a bare `kubectl get nms` or `modelartifacts.worker.gpustack.ai`
reaches the `v1` view. **A write to a NodeModelStore, or to a ModelArtifact's status, names
`v1alpha1`** (`nodemodelstores.v1alpha1.worker.gpustack.ai`), and so does the read of an object that
is written back: an object read through `v1` carries `apiVersion: worker.gpustack.ai/v1`, which the
`v1alpha1` endpoint refuses.

- **`v1` ModelArtifact has no `status` subresource.** The proxy writes with the worker's identity,
  and the status guard admits exactly that identity to a `ModelArtifact`'s status, which mounts are
  authorized by. An update through the main resource leaves the stored status as it is; read the
  status through the object.
- **`v1` NodeModelStore creates and updates nothing.** Its writers, the worker (`spec`) and each
  node's plugin (`status`), keep writing `v1alpha1`. It serves `delete` for the garbage collector,
  which watches the group's preferred version and collects only what that version can delete;
  without it no NodeModelStore went with its Node on Kubernetes 1.36. The worker creates a deleted
  object again while its node runs the plugin.
- Both views carry the same fields as `v1alpha1`: the [artifact](model-artifact.md#the-resource) and
  the [node store](node-model-store.md#the-resource).

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
  (`GET /model/downloads` on its HTTPS port) within 2 seconds, the whole answer within 5 seconds. A
  node that does not answer contributes the bytes it last wrote and is not counted in `live`.
- **It names no node.** It is namespaced and tenants read it; node names would give every tenant the
  cluster's topology. The per-node view is the `v1` NodeModelStore.
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
| Listing a worker's model files | `v1` NodeModelStore get, list, watch | none |
| Download to a worker ahead of use | a prefetch object naming nodes | a later spec |
| Delete with `cleanup_on_delete` | delete the prefetch; collection after the last reference and its grace | a later spec; today only the watermark collects |
| `reset` (retry now) | automatic backoff to `retryTime` | not planned |
| One row per source per worker | one entry per digest per node, shared by artifacts with the same content | none |
| Prefer workers holding the files | placement preference, compute first | a later spec |
| LoRA and draft-model files | not in this batch | later |
| Tenant scope | the artifact's namespace; the node store names no tenant | none |

## Requirements and limits

- **The aggregated API**, `apiregistration.k8s.io/v1`, and delegated authentication and
  authorization (`TokenReview`, `SubjectAccessReview`): available on every version the chart
  installs on.
- **Live bytes need the plugin's Pod reachable from the worker** on its HTTPS port. A NetworkPolicy
  that blocks it leaves the stored values, and `live` says how many nodes answered.
- **No watch on `progress`**: it answers one request. Watch the `v1` NodeModelStores for changes.

---

**See also** — [Model Artifact Reference](model-artifact.md) for the artifact itself ·
[Node Model Store Reference](node-model-store.md) for each node's entries and the progress write rule ·
[Model Deployment Metrics Reference](model-deployment-metrics.md) for the other subresource built
the same way.

**Next** → [Node Model Store Reference](node-model-store.md)
