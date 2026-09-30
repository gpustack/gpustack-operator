# GPUStack Operator Documentation

Start with the [project README](../README.md) to install the operator. This index groups the
remaining pages by task and lists every page below.

The seven capability guides are [Heterogeneous Devices](modules/devices/_index.md),
[RDMA Networking](modules/rdma/_index.md), [Topology Management](modules/topology/_index.md),
[KV Cache](modules/kv-cache/_index.md), [Model Delivery](modules/model-delivery/_index.md),
[Model Deployment](modules/model-deployment/_index.md) and
[Accelerated Instances](modules/instances/_index.md).

## Reading paths

For a first workload, follow [Usage](../README.md#usage), then use
[Accelerator Requests](modules/devices/requests.md) to choose its resource keys. The
[Walkthrough](getting-started/walkthrough.md) records a run on a four-node cluster. Workloads that
need RDMA also need [RDMA Operations](modules/rdma/operations.md).

For cluster operations, read [Architecture](getting-started/architecture.md) and [Installation
Modes](operate/installation-modes.md) first. The [settings](reference/settings.md), [vendor
prerequisites](getting-started/vendor-prerequisites.md) and
[preflight](modules/devices/preflight.md) pages cover installation decisions.

Use the [topology](modules/topology/operations.md),
[model store](modules/model-delivery/operations.md), [high availability](operate/high-availability.md)
and [RDMA](modules/rdma/operations.md) runbooks as those features are enabled. Hardware partitioning
has separate runbooks for [NVIDIA](modules/devices/nvidia-mig.md),
[T-Head](modules/devices/thead-mig.md) and [Hygon](modules/devices/hygon-mig.md).

Start an upgrade with the relevant page under [migration](operate/migration/to-subcharts.md).

For code changes, read [Architecture](getting-started/architecture.md), then the relevant deep page in the
table below. [Internals](contribute/internals.md) covers startup constraints, and
[Development](contribute/development.md) lists the build and validation commands. The
[`gpustack-operator-docs`](../.claude/skills/gpustack-operator-docs/SKILL.md) skill routes
documentation changes.

Agents should load `gpustack-operator-overview`, [Architecture](getting-started/architecture.md),
and only the deep page needed for the task.

## All pages

### Heterogeneous devices

| Page | Open it to… |
|---|---|
| [Heterogeneous Devices](modules/devices/_index.md) | Choose an accelerator request. |
| [Device Discovery](modules/devices/discovery.md) | Trace hardware detection and allocation. |
| [Scheduling Chain](modules/devices/scheduling.md) | Follow capacity from labels to queues. |
| [Admission](modules/devices/admission.md) | Understand the five request checks. |
| [Accelerator Requests](modules/devices/requests.md) | Look up resource keys and valid requests. |
| [NVIDIA MIG Operations](modules/devices/nvidia-mig.md) | Enable and recover NVIDIA partitions. |
| [T-Head MIG Operations](modules/devices/thead-mig.md) | Enable and recover T-Head partitions. |
| [Hygon MIG Operations](modules/devices/hygon-mig.md) | Enable and recover Hygon partitions. |
| [Preflight Operations](modules/devices/preflight.md) | Check a node before installation. |
| [Instance Type Unit Resources Reference](reference/instance-type-unit-resources.md) | Look up CPU and memory presets. |

### RDMA networking

| Page | Open it to… |
|---|---|
| [RDMA Networking](modules/rdma/_index.md) | Start with RDMA requests. |
| [Network Topology](modules/rdma/network-topology.md) | See how links and devices are discovered. |
| [RDMA Operations](modules/rdma/operations.md) | Request and check RDMA endpoints. |

### Topology management

| Page | Open it to… |
|---|---|
| [Topology Management](modules/topology/_index.md) | Start with placement by domain. |
| [Topology-Aware Scheduling](modules/topology/scheduling.md) | Follow topology data into scheduling. |
| [Topology-Aware Scheduling Operations](modules/topology/operations.md) | Enable and diagnose topology placement. |

### KV cache

| Page | Open it to… |
|---|---|
| [KV Cache](modules/kv-cache/_index.md) | Start with a shared inference cache. |
| [KV Cache Backend](modules/kv-cache/backend.md) | Understand the store and its capacity. |
| [KV Cache Leader](modules/kv-cache/leader.md) | Understand leader election and health. |
| [KV Cache Local Disk Tier](modules/kv-cache/local-disk-tier.md) | Configure local disk storage. |
| [KV Cache on Disk-Heavy Nodes](modules/kv-cache/disk-heavy-nodes.md) | Size memory on disk-heavy nodes. |
| [KV Cache Pool](modules/kv-cache/pool.md) | Grant and limit cache use. |
| [KV Cache Walkthrough](modules/kv-cache/walkthrough.md) | Create a working cache in four objects. |
| [KV Cache Injection Reference](modules/kv-cache/injection.md) | Attach a Pod to a pool. |

### Model delivery

| Page | Open it to… |
|---|---|
| [Model Delivery](modules/model-delivery/_index.md) | Choose how weights reach workloads. |
| [Model Artifact](modules/model-delivery/artifact.md) | Resolve and verify model weights. |
| [Model Image Source](modules/model-delivery/image-source.md) | Package weights in an image. |
| [Model Prefetch](modules/model-delivery/prefetch.md) | Warm weights before a workload starts. |
| [Node Model Store](modules/model-delivery/node-store.md) | Understand node cache state and collection. |
| [Node-to-Node Sync](modules/model-delivery/peer-sync.md) | Move cached weights between nodes. |
| [Model Artifact Views](modules/model-delivery/views.md) | Read artifact and node cache status. |
| [Model Store Operations](modules/model-delivery/operations.md) | Operate the node model cache. |

### Model deployment

| Page | Open it to… |
|---|---|
| [Model Deployment](modules/model-deployment/_index.md) | Start with managed model serving. |
| [Model Deployment](modules/model-deployment/deployment.md) | Configure serving roles and overrides. |
| [Model Deployment Prefill and Decode](modules/model-deployment/prefill-decode.md) | Pair serving roles. |
| [Engine Versions](modules/model-deployment/engine-versions.md) | Check supported engine versions. |
| [Model Deployment Routing](modules/model-deployment/routing.md) | Choose a routing policy. |
| [Model Deployment Metrics](modules/model-deployment/metrics.md) | Read serving metrics. |
| [Model Deployment Status](modules/model-deployment/status.md) | Diagnose deployment conditions. |
| [Model Deployment Shutdown](modules/model-deployment/shutdown.md) | Understand replica draining. |

### Accelerated instances

| Page | Open it to… |
|---|---|
| [Accelerated Instances](modules/instances/_index.md) | Start an accelerator-backed workspace. |
| [Instance Metrics Reference](reference/instance-metrics.md) | Read an Instance’s resource use. |

### Start here

| Page | Open it to… |
|---|---|
| [Architecture](getting-started/architecture.md) | See the operator’s four-stage path. |
| [Walkthrough](getting-started/walkthrough.md) | Follow a recorded cluster run. |
| [Vendor Prerequisites](getting-started/vendor-prerequisites.md) | Prepare each manufacturer’s driver. |

### Cluster operations and upgrades

| Page | Open it to… |
|---|---|
| [Installation Modes](operate/installation-modes.md) | Choose chart or image installation. |
| [High Availability Operations](operate/high-availability.md) | Set replica counts for control-plane parts. |
| [Migrating to Bundled Subcharts](operate/migration/to-subcharts.md) | Transfer chart ownership. |
| [Migrating from v0.5.x](operate/migration/from-v0.5.md) | Upgrade across the queue refactor. |
| [Upgrading to an Enforced Binding Dtype](operate/migration/kv-cache-dtype.md) | Check dtype before upgrading. |
| [Migration Troubleshooting](operate/migration/troubleshooting.md) | Recover a stuck upgrade. |

### Reference and contribution

| Page | Open it to… |
|---|---|
| [Internals](contribute/internals.md) | Review startup and naming constraints. |
| [Settings & Environment Variables](reference/settings.md) | Look up operator configuration. |
| [Development](contribute/development.md) | Build, generate and lint the project. |
| [Command Reference](reference/commands.md) | Look up binary commands and flags. |

## Conventions

Every page in this directory (this index excepted) carries:

- a short introduction that explains the subject or task;
- a **`## Contents`** list mirroring its `##` headings, in order;
- a **footer** — `**See also**` for sideways links and `**Next** →` for the next page on the path.

A page's file name, its H1 and its label above are the same words. Deep rationale is demoted into a
`> **Why**` note, so the rule stays skimmable. Length is not the defect, verbosity is.

Adding or moving a page means updating its row above. Run `make lint docs` to check links, Contents,
paragraph length, index labels, page coverage and paths named in skills.
Documentation changes do not require Go tests. The former Go tests that read Markdown pages were
removed; compare code-owned values with their reference pages when either changes.
