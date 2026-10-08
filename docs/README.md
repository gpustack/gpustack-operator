# GPUStack Operator Documentation

Start with the [project README](../README.md) to install the operator. This index groups the
remaining pages by task and lists every page below.

The capability guides are [Heterogeneous Devices](modules/devices/_index.md),
[RDMA Networking](modules/rdma/_index.md), [Topology Management](modules/topology/_index.md),
[KV Cache](modules/kv-cache/_index.md), [Model Delivery](modules/model-delivery/_index.md),
[Model Deployment](modules/model-deployment/_index.md) and
[Accelerated Instances](modules/instances/_index.md).

## Reading paths

For a first workload, follow [Usage](../README.md#usage), then use
[Accelerator Requests](modules/devices/requests.md) to choose its resource keys. The
[Device Scheduling Walkthrough](walkthroughs/devices/scheduling.md) records a run on a four-node cluster.
Workloads that need RDMA also need [RDMA Operations](modules/rdma/operations.md).

For cluster operations, read [Architecture](getting-started/architecture.md) and [Installation
Modes](operate/installation-modes.md) first. The [settings](reference/settings.md), [vendor
prerequisites](getting-started/vendor-prerequisites.md) and
[preflight](modules/devices/preflight.md) pages cover installation decisions.

Use the [topology](modules/topology/operations.md),
[model store](modules/model-delivery/operations.md), [high availability](operate/high-availability.md)
and [RDMA](modules/rdma/operations.md) runbooks as those features are enabled. Hardware partitioning
has separate runbooks for [NVIDIA](modules/devices/nvidia-mig.md),
[T-Head](modules/devices/thead-mig.md) and [Hygon](modules/devices/hygon-mig.md).

For complete examples, use the [Walkthroughs](walkthroughs/_index.md) entry point.

Start an upgrade with the relevant page under [migration](operate/migration/to-subcharts.md).

For code changes, read [Architecture](getting-started/architecture.md), then the relevant deep page in the
table below. [Internals](contribute/internals.md) covers startup constraints, and
[Development](contribute/development.md) lists the build and validation commands. The
[`gpustack-operator-docs`](../.agents/skills/gpustack-operator-docs/SKILL.md) skill routes
documentation changes.

Agents should load `gpustack-operator-overview`, [Architecture](getting-started/architecture.md),
and only the deep page needed for the task.

## Module map

Each entry routes one module or shared task to its guides, owning specs, code entry points, related
modules and skills. Links here are navigation, not proof: a spec records what was decided and
measured when it was written, so what the current checkout implements needs the current code.
A skill link names a procedure; the entry's other fields carry shared facts.

### Devices Manager

- ID: `devices`
- Use for: which resource key a workload asks for; how detection, capacity labels, queues and admission checks enforce it; MIG partitioning runbooks.
- Aliases: `Devices`, `GPU`, `NPU`, `MIG`, `accelerator`, `device plugin`
- Guides: [Heterogeneous Devices](modules/devices/_index.md); the request contract in [Accelerator Requests](modules/devices/requests.md); the gates in [Admission](modules/devices/admission.md).
- Specs: [accelerator resource modes](../specs/2026-06-21-accelerator-resource-modes-refactor.md) · [soft-slicing runtime isolation](../specs/2026-06-25-accelerator-soft-slicing-runtime-isolation.md) · [unified pool](../specs/2026-06-29-instancetype-unified-pool-refactor.md) · [credit scoring](../specs/2026-06-24-unified-credit-base-scoring.md)
- Code: [pkg/devicemanager](../pkg/devicemanager/) (detector, allocator, preflight) · [pkg/deviceplugin](../pkg/deviceplugin/) · [pkg/nodefeature](../pkg/nodefeature/) · chain reconcilers under [pkg/worker/controllers/worker](../pkg/worker/controllers/worker/)
- Related: [RDMA Manager](#rdma-manager) (interfaces in the same node inventory) · [Topology Aware](#topology-aware) (placement over pool capacity) · [GPU Instances](#gpu-instances) (the workload the chain admits)
- Skills: [gpustack-operator-e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md) (scheduling-chain cases) · [gpustack-operator-xbuild-and-verify](../.agents/skills/gpustack-operator-xbuild-and-verify/SKILL.md) (slicing builder stages and shims)

### RDMA Manager

- ID: `rdma`
- Use for: requesting RDMA endpoints for a workload; how the NIC inventory, link states and labels are discovered; aligning requests with the kubelet TopologyManager policy.
- Aliases: `RDMA`, `NIC`, `RoCE`, `link state`, `rdma.capable`
- Guides: [RDMA Networking](modules/rdma/_index.md); keys and policy in [RDMA Operations](modules/rdma/operations.md); inventory in [Network Topology](modules/rdma/network-topology.md).
- Specs: [NIC/RDMA topology](../specs/2026-09-02-devices-nic-rdma-topology.md) · [extended resource](../specs/2026-09-20-rdma-extended-resource.md) · [NVIDIA fabric domain](../specs/2026-09-20-nvidia-fabric-domain.md) · [fabric by protocol](../specs/2026-09-22-fabric-device-by-protocol.md) · [interface count](../specs/2026-09-22-fabric-interface-count.md)
- Code: NIC and link discovery in [pkg/devicemanager/detector](../pkg/devicemanager/detector/) (`network.go`, `link.go`) · request vocabulary in [pkg/nodefeature/rdma.go](../pkg/nodefeature/rdma.go) and [pkg/nodefeature/fabric.go](../pkg/nodefeature/fabric.go)
- Related: [Devices Manager](#devices-manager) (the interfaces are entries in the same per-node inventory)
- Skills: [gpustack-operator-e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md) (RDMA cases)

### Topology Aware

- ID: `topology`
- Use for: describing domains such as zones and racks; letting Kueue place replicas within a domain; enabling a topology source; diagnosing a Pending group.
- Aliases: `TAS`, `Topograph`, `topology source`, `zone`, `placement`
- Guides: [Topology Management](modules/topology/_index.md); semantics in [Topology-Aware Scheduling](modules/topology/scheduling.md); enablement in [Topology-Aware Scheduling Operations](modules/topology/operations.md).
- Specs: [topology discovery](../specs/2026-09-22-topology-discovery.md) · [model placement preference](../specs/2026-09-26-model-placement-preference.md)
- Code: [pkg/worker/controllers/worker/topology_source.go](../pkg/worker/controllers/worker/topology_source.go) and [pkg/worker/controllers/worker/node_topology.go](../pkg/worker/controllers/worker/node_topology.go)
- Related: [Devices Manager](#devices-manager) (placement constrains which pool serves a replica) · [Model Deployment](#model-deployment) (replicas can prefer nodes holding their model)
- Skills: [gpustack-operator-e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md) (TAS cases)

### KV Cache

- ID: `kv-cache`
- Use for: standing up a shared inference cache; pool grants and quotas; attaching a workload to a pool.
- Aliases: `KVCacheBackend`, `KVCachePool`, `Mooncake`, `prefix cache`, `injection`
- Guides: [KV Cache](modules/kv-cache/_index.md); setup order in [KV Cache Walkthrough](walkthroughs/kv-cache/shared-cache.md); store and grants in [KV Cache Backend](modules/kv-cache/backend.md) and [KV Cache Pool](modules/kv-cache/pool.md).
- Specs: [backend](../specs/2026-08-28-kv-cache-backend.md) · [pool](../specs/2026-08-28-kv-cache-pool.md) · [injection](../specs/2026-08-28-kv-cache-injection.md) · [media and scaling](../specs/2026-09-05-kv-cache-media-and-scaling.md) · [high availability](../specs/2026-09-06-kv-cache-backend-high-availability.md)
- Code: store rendering and read-back in [pkg/worker/kvcache](../pkg/worker/kvcache/) · per-engine client config in [pkg/worker/kvcache/inject](../pkg/worker/kvcache/inject/) · `kv_cache_*.go` reconcilers under [pkg/worker/controllers/worker](../pkg/worker/controllers/worker/)
- Related: [Model Deployment](#model-deployment) (deployments and Pods consume a pool through injection)
- Skills: [gpustack-operator-e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md) (cache and serving cases)

### Model Delivery

- ID: `model-delivery`
- Use for: giving model weights a stable identity; node caching and delivery; prefetch, peer sync, progress views and cache operations.
- Aliases: `ModelArtifact`, `NodeModelStore`, `ModelStore`, `ModelPrefetch`, `model cache`, `Hugging Face`
- Guides: [Model Delivery](modules/model-delivery/_index.md); the artifact contract in [Model Artifact](modules/model-delivery/artifact.md); the node cache in [Node Model Store](modules/model-delivery/node-store.md); the runbook in [Model Store Operations](modules/model-delivery/operations.md); a measured example in [Model Prefetch Walkthrough](walkthroughs/model-delivery/prefetch.md).
- Specs: [model artifact](../specs/2026-09-25-model-artifact.md) · [node model store](../specs/2026-09-25-node-model-store.md) · [prefetch](../specs/2026-09-27-model-prefetch.md) · [peer sync](../specs/2026-09-27-model-peer-sync.md) · [image source](../specs/2026-09-27-model-image-source.md)
- Code: Hub resolution in [pkg/modelartifact](../pkg/modelartifact/) · the per-node plugin and cache in [pkg/modelmanager](../pkg/modelmanager/) · cache configuration in [pkg/modelstore](../pkg/modelstore/)
- Related: [Model Deployment](#model-deployment) (deployments mount or download artifacts) · [KV Cache](#kv-cache) (the weight identity enters KV keys) · [GPU Instances](#gpu-instances) (an Instance can mount weights)
- Skills: [gpustack-operator-e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md) (node-delivery cases)

### Model Deployment

- ID: `model-deployment`
- Use for: describing a serving workload's model, engine and roles; prefill/decode pairing; replica routing, status and metrics.
- Aliases: `ModelDeployment`, `prefill`, `decode`, `router`, `vLLM`, `SGLang`, `replica`
- Guides: [Model Deployment](modules/model-deployment/_index.md); the role contract in [Model Deployment Configuration](modules/model-deployment/deployment.md); pairing in [Prefill and Decode](modules/model-deployment/prefill-decode.md); replica selection and Router observation in [Routing](modules/model-deployment/routing.md); what a status field means in [Status](modules/model-deployment/status.md); retirement and drain in [Shutdown](modules/model-deployment/shutdown.md); in-place collective resize in [Elastic EP](modules/model-deployment/elastic-ep.md); a recorded run in [Elastic EP Walkthrough](walkthroughs/model-deployment/elastic-ep.md); fixed per-rank HTTP serving in [External DP Walkthrough](walkthroughs/model-deployment/external-dp.md).
- Specs: [consolidation](../specs/2026-09-18-kvcache-and-model-deployment-consolidation.md) · [role replica admission](../specs/2026-09-19-role-replica-admission-unit.md) · [P/D pairing and router](../specs/2026-09-12-model-deployment-pd-pairing-and-router.md) · [router implementations](../specs/2026-09-19-model-deployment-router-implementations.md) · [router qualification and drain](../specs/2026-10-01-s1-router-qualification-and-drain.md) · [elastic EP resize](../specs/2026-10-01-s2-elastic-ep-resize.md)
- Code: `model_deployment_*.go` reconcilers under [pkg/worker/controllers/worker](../pkg/worker/controllers/worker/) · native resize client in [pkg/worker/elasticengine](../pkg/worker/elasticengine/) · webhooks in [pkg/worker/webhooks/worker](../pkg/worker/webhooks/worker/) · router image under [pack/llm-router](../pack/llm-router/)
- Related: [KV Cache](#kv-cache) (a deployment can attach a shared cache) · [Model Delivery](#model-delivery) (weights reach replicas through an artifact) · [Topology Aware](#topology-aware) (the placement field constrains replicas)
- Skills: [gpustack-operator-e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md) (serving cases)

### GPU Instances

- ID: `instances`
- Use for: launching an accelerator-backed container workspace; SSH access; node pinning and instance metrics.
- Aliases: `Instance`, `InstanceType`, `SSH`, `workspace`
- Guides: [Accelerated Instances](modules/instances/_index.md); the request form in [Accelerator Requests](modules/devices/requests.md); utilization in [Instance Metrics Reference](reference/instance-metrics.md).
- Specs: [SSH instance slicing](../specs/2026-07-04-ssh-instance-accelerator-slicing.md) · [sidecar partition visibility](../specs/2026-07-26-ssh-sidecar-partition-visibility.md) · [node pinning and additional volumes](../specs/2026-07-31-instance-node-pinning-and-additional-volumes.md) · [utilization metrics](../specs/2026-08-07-instance-utilization-metrics.md)
- Code: `instance*.go` reconcilers under [pkg/worker/controllers/worker](../pkg/worker/controllers/worker/) · API handlers in [pkg/worker/extensionapis/worker](../pkg/worker/extensionapis/worker/) · gauges in [pkg/devicemanager/exporter](../pkg/devicemanager/exporter/) and [pkg/kubemetrics](../pkg/kubemetrics/)
- Related: [Devices Manager](#devices-manager) (the request families and the admission chain) · [Model Delivery](#model-delivery) (mounting weights into an Instance)
- Skills: [gpustack-operator-e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md) (Instance cases)

### API Changes

- ID: `api`
- Use for: which version a manifest or call targets. User-facing resources are `worker.gpustack.ai/v1`; `worker.gpustack.ai/v1alpha1` is internal controller storage behind the public API, not a version users switch between. The layers: public types, the handlers that proxy them onto internal storage, code generation, and the RBAC that grants access.
- Aliases: `worker.gpustack.ai/v1`, `v1alpha1`, `CRD`, `aggregated apiserver`
- Guides: the version table in [Development](contribute/development.md); a public surface example with its grants in [Model Artifact API](modules/model-delivery/views.md).
- Code: public types in [api/worker/v1](../api/worker/v1/) and internal storage in [api/worker/v1alpha1](../api/worker/v1alpha1/) · handlers and proxies in [pkg/worker/extensionapis](../pkg/worker/extensionapis/) over the [pkg/extensionapi](../pkg/extensionapi/) plumbing · generators in [gen/api](../gen/api/) · worker RBAC in [deploy/gpustack-operator/chart/templates/worker/serviceaccount.yaml](../deploy/gpustack-operator/chart/templates/worker/serviceaccount.yaml)
- Related: [Development](#development) (`make generate` regenerates the API surface) · [GPU Instances](#gpu-instances) (Instance and InstanceType are the main public resources)
- Skills: [gpustack-operator-generate](../.agents/skills/gpustack-operator-generate/SKILL.md) (regenerate after type or webhook edits)

### Installation

- ID: `installation`
- Use for: choosing chart or image mode; vendored subcharts and their patches; vendor prerequisites; upgrade and migration paths.
- Aliases: `Helm chart`, `subchart`, `chart mode`, `image mode`, `values.yaml`
- Guides: choices in [Installation Modes](operate/installation-modes.md); node drivers in [Vendor Prerequisites](getting-started/vendor-prerequisites.md); transfer in [Migrating to Bundled Subcharts](operate/migration/to-subcharts.md).
- Specs: [bundled apps subchart split](../specs/2026-07-28-bundled-apps-subchart-split.md)
- Code: [deploy/gpustack-operator/chart](../deploy/gpustack-operator/chart/) · vendoring in [hack/deps.sh](../hack/deps.sh) · image-mode in-cluster install in [pkg/worker/kuberess](../pkg/worker/kuberess/)
- Related: [Development](#development) (chart targets and subchart patches) · [Devices Manager](#devices-manager) (the Device Manager DaemonSets the chart deploys)
- Skills: [gpustack-operator-chart-e2e](../.agents/skills/gpustack-operator-chart-e2e/SKILL.md) (install, rollout and uninstall verification) · [gpustack-operator-chart-subcharts-manage](../.agents/skills/gpustack-operator-chart-subcharts-manage/SKILL.md) (add, patch or bump a vendored subchart)

### Development

- ID: `development`
- Use for: build, lint, test and codegen commands; commit and PR conventions; contributor invariants; release steps; site publication and translations.
- Aliases: `make`, `lint`, `codegen`, `vendored dependencies`, `staging`, `Hugo`, `GitHub Pages`
- Guides: commands and targets in [Development](contribute/development.md); startup invariants in [Internals](contribute/internals.md); orientation in [Architecture](getting-started/architecture.md).
- Code: [Makefile](../Makefile) and the scripts behind it in [hack](../hack/) · site publication in [Site workflow](../.github/workflows/site.yml) and [publisher](../hack/site/publish.py) · patched modules in [staging](../staging/) · generators in [gen](../gen/)
- Related: [API Changes](#api-changes) (what `make generate` owns) · [Installation](#installation) (chart targets and vendoring)
- Skills: [gpustack-operator-overview](../.agents/skills/gpustack-operator-overview/SKILL.md) (codebase tour) · [gpustack-operator-docs](../.agents/skills/gpustack-operator-docs/SKILL.md) (documentation changes) · [gpustack-operator-code-review](../.agents/skills/gpustack-operator-code-review/SKILL.md) (PR review axes) · [gpustack-operator-issue-pr](../.agents/skills/gpustack-operator-issue-pr/SKILL.md) (issue and PR titles) · [gpustack-operator-release](../.agents/skills/gpustack-operator-release/SKILL.md) (cut a release) · [gpustack-operator-generate](../.agents/skills/gpustack-operator-generate/SKILL.md) (API codegen)

## All pages

### Heterogeneous devices

| Page | Description |
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

| Page | Description |
|---|---|
| [RDMA Networking](modules/rdma/_index.md) | Start with RDMA requests. |
| [Network Topology](modules/rdma/network-topology.md) | See how links and devices are discovered. |
| [RDMA Operations](modules/rdma/operations.md) | Request and check RDMA endpoints. |

### Topology management

| Page | Description |
|---|---|
| [Topology Management](modules/topology/_index.md) | Start with placement by domain. |
| [Topology-Aware Scheduling](modules/topology/scheduling.md) | Follow topology data into scheduling. |
| [Topology-Aware Scheduling Operations](modules/topology/operations.md) | Enable and diagnose topology placement. |

### KV cache

| Page | Description |
|---|---|
| [KV Cache](modules/kv-cache/_index.md) | Start with a shared inference cache. |
| [KV Cache Backend](modules/kv-cache/backend.md) | Understand the store and its capacity. |
| [KV Cache Leader](modules/kv-cache/leader.md) | Understand leader election and health. |
| [KV Cache Local Disk Tier](modules/kv-cache/local-disk-tier.md) | Configure local disk storage. |
| [KV Cache on Disk-Heavy Nodes](modules/kv-cache/disk-heavy-nodes.md) | Size memory on disk-heavy nodes. |
| [KV Cache Pool](modules/kv-cache/pool.md) | Grant and limit cache use. |
| [KV Cache Injection Reference](modules/kv-cache/injection.md) | Attach a Pod to a pool. |

### Model delivery

| Page | Description |
|---|---|
| [Model Delivery](modules/model-delivery/_index.md) | Choose how weights reach workloads. |
| [Model Artifact](modules/model-delivery/artifact.md) | Resolve and verify model weights. |
| [Model Image Source](modules/model-delivery/image-source.md) | Package weights in an image. |
| [Model Prefetch](modules/model-delivery/prefetch.md) | Warm weights before a workload starts. |
| [Node Model Store](modules/model-delivery/node-store.md) | Understand node cache state and collection. |
| [Node-to-Node Sync](modules/model-delivery/peer-sync.md) | Move cached weights between nodes. |
| [Model Artifact API](modules/model-delivery/views.md) | Read artifact and node cache status. |
| [Model Store Operations](modules/model-delivery/operations.md) | Operate the node model cache. |

### Model deployment

| Page | Description |
|---|---|
| [Model Deployment](modules/model-deployment/_index.md) | Start with managed model serving. |
| [Model Deployment Configuration](modules/model-deployment/deployment.md) | Configure serving roles and overrides. |
| [Model Deployment Prefill and Decode](modules/model-deployment/prefill-decode.md) | Pair serving roles. |
| [Elastic EP](modules/model-deployment/elastic-ep.md) | Resize one serving instance's collective in place. |
| [Engine Versions](modules/model-deployment/engine-versions.md) | Check supported engine versions. |
| [Model Deployment Routing](modules/model-deployment/routing.md) | Choose a routing policy, confirm a Router serves. |
| [Model Deployment Metrics](modules/model-deployment/metrics.md) | Read serving metrics. |
| [Model Deployment Status](modules/model-deployment/status.md) | Diagnose deployment conditions, follow a retirement. |
| [Model Deployment Shutdown](modules/model-deployment/shutdown.md) | Understand replica retirement and draining. |

### Accelerated instances

| Page | Description |
|---|---|
| [Accelerated Instances](modules/instances/_index.md) | Start an accelerator-backed workspace. |
| [Instance Metrics Reference](reference/instance-metrics.md) | Read an Instance’s resource use. |

### Walkthroughs

| Page | Description |
|---|---|
| [Walkthroughs](walkthroughs/_index.md) | Choose a complete example by module. |
| [Device Scheduling Walkthrough](walkthroughs/devices/scheduling.md) | Follow a recorded accelerator scheduling run. |
| [KV Cache Walkthrough](walkthroughs/kv-cache/shared-cache.md) | Create a shared cache from backend to workload. |
| [Model Prefetch Walkthrough](walkthroughs/model-delivery/prefetch.md) | Cache weights and verify captured model CR output. |
| [Model Deployment Walkthroughs](walkthroughs/model-deployment/_index.md) | Choose a serving shape and prepare GPUs. |
| [Elastic EP Walkthrough](walkthroughs/model-deployment/elastic-ep.md) | Grow DP width with TP fixed and verify inference. |
| [External DP Walkthrough](walkthroughs/model-deployment/external-dp.md) | Verify per-rank inference and Router coverage. |

### Start here

| Page | Description |
|---|---|
| [Architecture](getting-started/architecture.md) | See the operator’s four-stage path. |
| [Vendor Prerequisites](getting-started/vendor-prerequisites.md) | Prepare each manufacturer’s driver. |

### Cluster operations and upgrades

| Page | Description |
|---|---|
| [Installation Modes](operate/installation-modes.md) | Choose chart or image installation. |
| [High Availability Operations](operate/high-availability.md) | Set replica counts for control-plane parts. |
| [Migrating to Bundled Subcharts](operate/migration/to-subcharts.md) | Transfer chart ownership. |
| [Migrating from v0.5.x](operate/migration/from-v0.5.md) | Upgrade across the queue refactor. |
| [Upgrading to an Enforced Binding Dtype](operate/migration/kv-cache-dtype.md) | Check dtype before upgrading. |
| [Migration Troubleshooting](operate/migration/troubleshooting.md) | Recover a stuck upgrade. |

### Reference and contribution

| Page | Description |
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
