# LLM Reference

Flat lookup hub for `docs/reference/*.md`. Use these pages for exact flags, API schemas, condition
tables, raw captures and deterministic simulations; public guides own operator procedures.

## Task routing

| Task | Reference page | Public owner |
| --- | --- | --- |
| Look up binary subcommands, flags and exit codes | [Command Reference](commands.md) | [Preflight Checks](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/devices/preflight/index.md), [Model Store Operations](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-delivery/operations/index.md) |
| Inspect `Setting` precedence and `GPUSTACK_*` variables | [Settings & Environment Variables](settings.md) | [Installation Modes](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/operate/installation-modes/index.md) |
| Check derived `InstanceType` CPU and memory presets | [Instance Type Unit Resources Reference](instance-type-unit-resources.md) | [GPU Instances](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/instances/index.md), [Heterogeneous Devices Walkthrough](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/walkthroughs/devices/scheduling/index.md) |
| Look up Ascend A5 UB, Host RoCE and mount matrices | [Ascend A5 Matrix](ascend-a5-matrix.md) | [Vendor Prerequisites](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/getting-started/vendor-prerequisites/index.md), [Preflight Checks](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/devices/preflight/index.md), [Prefill and Decode](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/prefill-decode/index.md), [KV Cache Backend](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/kv-cache/backend/index.md), [Remote Weight Delivery](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-delivery/remote/index.md) |
| Trace NFD rules, `Devices` ledger and slicing injection | [Device Discovery](devices-discovery.md) | [Preflight Checks](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/devices/preflight/index.md), [Accelerator Requests](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/devices/requests/index.md) |
| Inspect capacity labels and Kueue queue controllers | [Scheduling Chain](devices-scheduling.md) | [Accelerator Requests](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/devices/requests/index.md) |
| Check five admission gates, four-view math and webhooks | [Admission](devices-admission.md) | [Accelerator Requests](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/devices/requests/index.md) |
| Read verbatim three-configuration H100 MIG transcripts | [NVIDIA MIG Transcripts](devices-mig-transcripts.md) | [NVIDIA MIG Operations](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/devices/nvidia-mig/index.md) |
| Inspect raw multi-object scheduling `kubectl` captures | [Devices Scheduling Capture](devices-scheduling-capture.md) | [Heterogeneous Devices Walkthrough](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/walkthroughs/devices/scheduling/index.md) |
| Inspect NIC inventory, PCI topology and *RDMA* link states | [Network Topology](rdma-network-topology.md) | [RDMA Operations](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/rdma/operations/index.md) |
| Map `TopologySource` snapshots into Kueue *TAS* profiles | [Topology-Aware Scheduling](topology-scheduling.md) | [Topology-Aware Scheduling Operations](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/topology/operations/index.md) |
| Inspect Mooncake rendering, condition algebra and teardown | [KV Cache Backend Internals](kv-cache-backend-internals.md) | [KV Cache Backend](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/kv-cache/backend/index.md) |
| Inspect `Pod` *KV* cache injection keys and refusals | [KV Cache Injection Reference](kv-cache-injection.md) | [KV Cache Pools and Bindings](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/kv-cache/pool/index.md), [Shared Cache Walkthrough](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/walkthroughs/kv-cache/shared-cache/index.md) |
| Query `ModelArtifact` and `NodeModelStore` public APIs | [Model Artifact API](model-delivery-artifact-api.md) | [Model Artifact](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-delivery/artifact/index.md) |
| Inspect `NodeModelStore` fields, CSI mounts and eviction | [Node Model Store](model-delivery-node-store.md) | [Model Store Operations](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-delivery/operations/index.md) |
| Check source RAM math, reader protocol and ModelExpress env | [Remote Weight Delivery Internals](model-delivery-remote-internals.md) | [Remote Weight Delivery](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-delivery/remote/index.md) |
| Inspect owned keys, render versions and refusal catalog | [Model Deployment Internals](model-deployment-internals.md) | [Configuration](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/configuration/index.md) |
| Check synthesized engine, router and transfer parameters | [Parameters](model-deployment-parameters.md) | [Configuration](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/configuration/index.md) |
| Query `ModelDeployment` metrics subresource and series | [Model Deployment Metrics](model-deployment-metrics.md) | [Model Deployment Scaling](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/scaling/index.md) |
| Look up `ModelDeployment` status fields and conditions | [Model Deployment Status](model-deployment-status.md) | [Model Deployment Routing](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/routing/index.md), [Online Runtime Control](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/runtime-control/index.md) |
| Check replica PreStop drain and retirement budgets | [Model Deployment Shutdown](model-deployment-shutdown.md) | [Configuration](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/configuration/index.md), [Model Deployment Scaling](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/scaling/index.md) |
| Check capacity-prior math, normalization and status schemas | [Scaling Internals](model-deployment-scaling-internals.md) | [Model Deployment Scaling](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/scaling/index.md) |
| Compare scaler algorithms on deterministic synthetic load | [Model Deployment Scaling Simulation](model-deployment-scaling-simulation.md) | [Model Deployment Scaling Benchmark](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/scaling-benchmark/index.md) |
| Query `Instance` utilization subresource and node gauges | [Instance Metrics Reference](instances-metrics.md) | [GPU Instance Operations](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/instances/operations/index.md) |

## Exact aliases

| Alias | Category | Reference target |
| --- | --- | --- |
| `gpustack-operator`, `worker`, `device-manager`, `model-manager`, `model-transfer` | CLI | [Command Reference](commands.md#summary) |
| `Setting`, `gpustack-settings`, `settings-default-tracking`, `GPUSTACK_*` | Configuration | [Settings & Environment Variables](settings.md#value-ownership) |
| `spec.unitResources`, `instance-type-derived-from-node` | API field | [Instance Type Unit Resources Reference](instance-type-unit-resources.md#preset-scope) |
| `ascend-docker-runtime`, `hccl-ranktable`, `/etc/hccl_rootinfo.json`, `/lib/route.conf`, `/etc/hixlep` | Ascend A5 | [Ascend A5 Matrix](ascend-a5-matrix.md#conditional-host-mounts) |
| `ASCEND_LOCAL_COMM_RES`, `ASCEND_GLOBAL_RESOURCE_CONFIG`, `roce:host`, `ub_ctp`, `ub_rtp:device`, `uboe:device` | Ascend A5 | [Ascend A5 Matrix](ascend-a5-matrix.md#modeldeployment-matrix) |
| `Devices`, `gpustack-cpu-info`, `LogicalSliced`, `GetPreferredAllocation` | API / Ledger | [Device Discovery](devices-discovery.md#stage-2-device-manager-dm) |
| `device.gpustack.ai/accelerator.preferred-id`, `device.gpustack.ai/accelerator.preferred-index` | Placement | [Device Discovery](devices-discovery.md#preferred-card-annotations) |
| `AcceleratorType`, `InstanceType`, `ResourceFlavor`, `ClusterQueue`, `LocalQueue` | Scheduling | [Scheduling Chain](devices-scheduling.md#stage-4-kueue-chain) |
| `gpustack-node-devices`, `OnceMaxRequest`, `EX` / `SH` / `SL` / `PT` | Admission | [Admission](devices-admission.md#four-view-status) |
| `nvidia.com/gpu.partitioned.mig-*`, `mig-3g.40gb`, `device.gpustack.ai/accelerator.allocated` | Capture | [NVIDIA MIG Transcripts](devices-mig-transcripts.md#all-physical-configuration) |
| `InstanceTypeFlavor`, `node-a10g-gpustack-worker`, `gpustack.ai/managed` | Capture | [Devices Scheduling Capture](devices-scheduling-capture.md#initial-pool-objects) |
| `Devices.spec.interfaces`, `pciRootId`, `pciSwitches`, `rdma.capable` | *RDMA* | [Network Topology](rdma-network-topology.md#interface-inventory) |
| `TopologySource`, `topology.gpustack.ai/profile` | *TAS* | [Topology-Aware Scheduling](topology-scheduling.md#topologysource-contract) |
| `roles[].topology.requiredLevel` | *TAS* | [Topology-Aware Scheduling](topology-scheduling.md#per-replica-topology-requests) |
| `gpustack-model-deployment-joint` | Admission | [Scheduling Chain](devices-scheduling.md#queue-quota-and-draining) |
| `MOONCAKE_TE_META_DATA_SERVER`, `ListingTooLarge`, `AmbiguousMemberIdentity`, `SegmentListingNotServed`, `PoolWrites` | *KV* Backend | [KV Cache Backend Internals](kv-cache-backend-internals.md#member-rendering) |
| `kvcache.gpustack.ai/inject`, `kvcache.gpustack.ai/binding` | Webhook | [KV Cache Injection Reference](kv-cache-injection.md#contract) |
| `modelartifacts/<name>/progress`, `ModelArtifact`, `NodeModelStore` | Subresource | [Model Artifact API](model-delivery-artifact-api.md#progress-subresource) |
| `model.csi.gpustack.ai`, `NodeModelStore.status` | Storage | [Node Model Store](model-delivery-node-store.md#resource) |
| `sourceMemory`, `model_loader_extra_config`, `POST /v1/sessions`, `sourceWindowBytes` | Remote transfer | [Remote Weight Delivery Internals](model-delivery-remote-internals.md#source-memory-arithmetic) |
| `MX_SERVER_ADDRESS`, `MX_ARTIFACT_TRANSFER`, `MX_LOAD_PLUGIN`, `RdmaStrategy`, `GpustackLoadStrategy` | ModelExpress | [Remote Weight Delivery Internals](model-delivery-remote-internals.md#modelexpress-internals) |
| `modeldeployment.gpustack.ai/pod-spec-hash`, `render-version`, `min-render-version`, `ASCEND_BASE_PORT` | Internals | [Model Deployment Internals](model-deployment-internals.md#pod-labels-and-annotations) |
| `modelDeploymentOwnedKeys`, `extraArgs`, `roles[].command` | Rendering | [Parameters](model-deployment-parameters.md#override-tiers) |
| `modeldeployments/<name>/metrics` | Subresource | [Model Deployment Metrics](model-deployment-metrics.md#subresource) |
| `status.phase`, `status.roleSummary`, `status.retirement`, `status.roles[].groups` | Status field | [Model Deployment Status](model-deployment-status.md#status) |
| `DomainRegistered`, `BindingNotFound`, `BindingNotReady` | Condition | [Model Deployment Status](model-deployment-status.md#domainregistered) |
| `QuotaReserved`, `PodGroupIncomplete`, `PreemptedInPart`, `Parked` | Condition | [Model Deployment Status](model-deployment-status.md#quotareserved) |
| `CacheAttached`, `CacheActive`, `CacheOperationsFailing` | Condition | [Model Deployment Status](model-deployment-status.md#cacheattached) |
| `ReplicasUpToDate`, `RolloutHeldByCache`, `RolloutHeldByWeights` | Condition | [Model Deployment Status](model-deployment-status.md#replicasuptodate) |
| `RoleKindsReady`, `AllKindsReady`, `KindsNotReady` | Condition | [Model Deployment Status](model-deployment-status.md#rolekindsready) |
| `KVEventsPublishing`, `PublisherDisabled`, `NoRouter` | Condition | [Model Deployment Status](model-deployment-status.md#kveventspublishing) |
| `RouterReady`, `RenderFailed`, `NoReadyReplicas` | Condition | [Model Deployment Status](model-deployment-status.md#routerready) |
| `EndpointEligibility`, `NotObserved` | Condition | [Model Deployment Status](model-deployment-status.md#endpointeligibility) |
| `ElasticResize`, `ScaleRefused`, `WorldUnprovable` | Condition | [Model Deployment Status](model-deployment-status.md#elasticresize) |
| `RuntimeControlSynced` | Condition | [Model Deployment Status](model-deployment-status.md#runtimecontrolsynced) |
| `gpustack-engine-sentinel`, `/gpustack/v1/ready`, `gpustack_engine_sentinel.drain` | Runtime | [Model Deployment Shutdown](model-deployment-shutdown.md#drain-window), [Online Runtime Control](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/runtime-control/index.md) |
| `Withdrawing`, `Draining`, `Settling`, `Aborted` | Lifecycle | [Model Deployment Shutdown](model-deployment-shutdown.md#retirement-protocol) |
| `ModelDeploymentScalerCapacityPriorCatalog`, `compatibilityFingerprint`, `resolvedCapacityPriors` | Scaling | [Scaling Internals](model-deployment-scaling-internals.md#capacity-prior-catalog) |
| `Proportional`, `TokenThroughput`, `Predictive` | Simulation | [Model Deployment Scaling Simulation](model-deployment-scaling-simulation.md#fixture-and-provenance) |
| `instances/<name>/metrics`, `gpustack_instance_*` | Subresource | [Instance Metrics Reference](instances-metrics.md#subresource) |

## Evidence labels

Every reference topic page declares `contract`, `recorded capture`, or `synthetic simulation` in its
opening paragraph.

| Reference page | Evidence label | Scope |
| --- | --- | --- |
| [Command Reference](commands.md) | `contract` | CLI subcommands, flags and exit codes |
| [Settings & Environment Variables](settings.md) | `contract` | `Setting` alignment rules and `GPUSTACK_*` readers |
| [Instance Type Unit Resources Reference](instance-type-unit-resources.md) | `contract` | Derived `InstanceType` CPU and RAM preset table |
| [Ascend A5 Matrix](ascend-a5-matrix.md) | `contract` | Ascend A5 UB, Host RoCE, ranktable and host mount matrices |
| [Device Discovery](devices-discovery.md) | `contract` | NFD rules, `Devices` ledger and vendor slicing |
| [Scheduling Chain](devices-scheduling.md) | `contract` | Capacity profiling and Kueue resource materialization |
| [Admission](devices-admission.md) | `contract` | Five admission gates, four-view status and webhooks |
| [NVIDIA MIG Transcripts](devices-mig-transcripts.md) | `recorded capture` | Verbatim three-configuration H100 MIG transcripts |
| [Devices Scheduling Capture](devices-scheduling-capture.md) | `recorded capture` | Raw multi-object `kubectl` YAML scheduling captures |
| [Network Topology](rdma-network-topology.md) | `contract` | NIC inventory, link checks and *RDMA* resource keys |
| [Topology-Aware Scheduling](topology-scheduling.md) | `contract` | `TopologySource` profiles and Kueue *TAS* placement |
| [KV Cache Backend Internals](kv-cache-backend-internals.md) | `contract` | Mooncake env/argv mapping, fabric privileges and condition catalog |
| [KV Cache Injection Reference](kv-cache-injection.md) | `contract` | `Pod` mutating webhook contract and per-engine flags |
| [Model Artifact API](model-delivery-artifact-api.md) | `contract` | Public artifact APIs, `progress` subresource and RBAC |
| [Node Model Store](model-delivery-node-store.md) | `contract` | `NodeModelStore` status, CSI mounts and garbage collection |
| [Remote Weight Delivery Internals](model-delivery-remote-internals.md) | `contract` | Source RAM math, reader protocol, ModelExpress env and capture sweep details |
| [Model Deployment Internals](model-deployment-internals.md) | `contract` | `Pod` metadata, owned keys, render versions and full refusal catalog |
| [Parameters](model-deployment-parameters.md) | `contract` | Synthesized engine, router, sidecar and transfer parameters |
| [Model Deployment Metrics](model-deployment-metrics.md) | `contract` | Aggregated metrics subresource and Prometheus series |
| [Model Deployment Status](model-deployment-status.md) | `contract` | Status fields, group eligibility and 10 condition types |
| [Model Deployment Shutdown](model-deployment-shutdown.md) | `contract` | PreStop drain window and replica retirement budgets |
| [Scaling Internals](model-deployment-scaling-internals.md) | `contract` | Capacity-prior catalog schema, disaggregated math and normalization |
| [Model Deployment Scaling Simulation](model-deployment-scaling-simulation.md) | `synthetic simulation` | Deterministic local three-algorithm scaler simulation |
| [Instance Metrics Reference](instances-metrics.md) | `contract` | `Instance` metrics subresource and node exporter gauges |

---

**See also** — [Architecture](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/getting-started/architecture/index.md) (system overview and workload flow) · [Accelerator Requests](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/devices/requests/index.md) (public resource key contract) · [Configuration](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/modules/model-deployment/configuration/index.md) (public `ModelDeployment` contract)
