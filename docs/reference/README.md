# LLM Reference

Flat lookup hub for `docs/reference/*.md`. Use these pages for exact flags, API schemas, condition
tables and deterministic simulations; public guides own operator procedures.

## Task routing

| Task | Reference page | Public owner |
| --- | --- | --- |
| Look up binary subcommands, flags and exit codes | [Command Reference](commands.md) | [Preflight Checks](../modules/devices/preflight.md), [Model Store Operations](../modules/model-delivery/operations.md) |
| Inspect `Setting` precedence and `GPUSTACK_*` variables | [Settings & Environment Variables](settings.md) | [Installation Modes](../operate/installation-modes.md) |
| Check derived `InstanceType` CPU and memory presets | [Instance Type Unit Resources Reference](instance-type-unit-resources.md) | [GPU Instances](../modules/instances/_index.md), [Heterogeneous Devices Walkthrough](../walkthroughs/devices/scheduling.md) |
| Trace NFD rules, `Devices` ledger and slicing injection | [Device Discovery](devices-discovery.md) | [Preflight Checks](../modules/devices/preflight.md), [Accelerator Requests](../modules/devices/requests.md) |
| Inspect capacity labels and Kueue queue controllers | [Scheduling Chain](devices-scheduling.md) | [Accelerator Requests](../modules/devices/requests.md) |
| Check five admission gates, four-view math and webhooks | [Admission](devices-admission.md) | [Accelerator Requests](../modules/devices/requests.md) |
| Inspect NIC inventory, PCI topology and *RDMA* link states | [Network Topology](rdma-network-topology.md) | [RDMA Operations](../modules/rdma/operations.md) |
| Map `TopologySource` snapshots into Kueue *TAS* profiles | [Topology-Aware Scheduling](topology-scheduling.md) | [Topology-Aware Scheduling Operations](../modules/topology/operations.md) |
| Inspect `Pod` *KV* cache injection keys and refusals | [KV Cache Injection Reference](kv-cache-injection.md) | [KV Cache Pools and Bindings](../modules/kv-cache/pool.md), [Shared Cache Walkthrough](../walkthroughs/kv-cache/shared-cache.md) |
| Query `ModelArtifact` and `NodeModelStore` public APIs | [Model Artifact API](model-delivery-artifact-api.md) | [Model Artifact](../modules/model-delivery/artifact.md) |
| Inspect `NodeModelStore` fields, CSI mounts and eviction | [Node Model Store](model-delivery-node-store.md) | [Model Store Operations](../modules/model-delivery/operations.md) |
| Check synthesized engine, router and transfer parameters | [Parameters](model-deployment-parameters.md) | [Configuration](../modules/model-deployment/configuration.md) |
| Query `ModelDeployment` metrics subresource and series | [Model Deployment Metrics](model-deployment-metrics.md) | [Model Deployment Scaling](../modules/model-deployment/scaling.md) |
| Look up `ModelDeployment` status fields and conditions | [Model Deployment Status](model-deployment-status.md) | [Model Deployment Routing](../modules/model-deployment/routing.md), [Online Runtime Control](../modules/model-deployment/runtime-control.md) |
| Check replica PreStop drain and retirement budgets | [Model Deployment Shutdown](model-deployment-shutdown.md) | [Configuration](../modules/model-deployment/configuration.md), [Model Deployment Scaling](../modules/model-deployment/scaling.md) |
| Compare scaler algorithms on deterministic synthetic load | [Model Deployment Scaling Simulation](model-deployment-scaling-simulation.md) | [Model Deployment Scaling Benchmark](../modules/model-deployment/scaling-benchmark.md) |
| Query `Instance` utilization subresource and node gauges | [Instance Metrics Reference](instances-metrics.md) | [GPU Instances](../modules/instances/_index.md) |

## Exact aliases

| Alias | Category | Reference target |
| --- | --- | --- |
| `gpustack-operator`, `worker`, `device-manager`, `model-manager`, `model-transfer` | CLI | [Command Reference](commands.md#summary) |
| `Setting`, `gpustack-settings`, `settings-default-tracking`, `GPUSTACK_*` | Configuration | [Settings & Environment Variables](settings.md#value-ownership) |
| `spec.unitResources`, `instance-type-derived-from-node` | API field | [Instance Type Unit Resources Reference](instance-type-unit-resources.md#preset-scope) |
| `Devices`, `gpustack-cpu-info`, `LogicalSliced`, `GetPreferredAllocation` | API / Ledger | [Device Discovery](devices-discovery.md#stage-2-device-manager-dm) |
| `AcceleratorType`, `InstanceType`, `ResourceFlavor`, `ClusterQueue`, `LocalQueue` | Scheduling | [Scheduling Chain](devices-scheduling.md#stage-4-kueue-chain) |
| `gpustack-node-devices`, `OnceMaxRequest`, `EX` / `SH` / `SL` / `PT` | Admission | [Admission](devices-admission.md#four-view-status) |
| `Devices.spec.interfaces`, `pciRootId`, `pciSwitches`, `rdma.capable` | *RDMA* | [Network Topology](rdma-network-topology.md#interface-inventory) |
| `TopologySource`, `topology.gpustack.ai/profile` | *TAS* | [Topology-Aware Scheduling](topology-scheduling.md#topologysource-contract) |
| `roles[].topology.requiredLevel` | *TAS* | [Topology-Aware Scheduling](topology-scheduling.md#per-replica-topology-requests) |
| `gpustack-model-deployment-joint` | Admission | [Scheduling Chain](devices-scheduling.md#queue-quota-and-draining) |
| `kvcache.gpustack.ai/inject`, `kvcache.gpustack.ai/binding` | Webhook | [KV Cache Injection Reference](kv-cache-injection.md#contract) |
| `modelartifacts/<name>/progress`, `ModelArtifact`, `NodeModelStore` | Subresource | [Model Artifact API](model-delivery-artifact-api.md#progress-subresource) |
| `model.csi.gpustack.ai`, `NodeModelStore.status` | Storage | [Node Model Store](model-delivery-node-store.md#resource) |
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
| `Withdrawing`, `Draining`, `Settling`, `Aborted` | Lifecycle | [Model Deployment Shutdown](model-deployment-shutdown.md#retirement-protocol) |
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
| [Device Discovery](devices-discovery.md) | `contract` | NFD rules, `Devices` ledger and vendor slicing |
| [Scheduling Chain](devices-scheduling.md) | `contract` | Capacity profiling and Kueue resource materialization |
| [Admission](devices-admission.md) | `contract` | Five admission gates, four-view status and webhooks |
| [Network Topology](rdma-network-topology.md) | `contract` | NIC inventory, link checks and *RDMA* resource keys |
| [Topology-Aware Scheduling](topology-scheduling.md) | `contract` | `TopologySource` profiles and Kueue *TAS* placement |
| [KV Cache Injection Reference](kv-cache-injection.md) | `contract` | `Pod` mutating webhook contract and per-engine flags |
| [Model Artifact API](model-delivery-artifact-api.md) | `contract` | Public artifact APIs, `progress` subresource and RBAC |
| [Node Model Store](model-delivery-node-store.md) | `contract` | `NodeModelStore` status, CSI mounts and garbage collection |
| [Parameters](model-deployment-parameters.md) | `contract` | Synthesized engine, router, sidecar and transfer parameters |
| [Model Deployment Metrics](model-deployment-metrics.md) | `contract` | Aggregated metrics subresource and Prometheus series |
| [Model Deployment Status](model-deployment-status.md) | `contract` | Status fields, group eligibility and 10 condition types |
| [Model Deployment Shutdown](model-deployment-shutdown.md) | `contract` | PreStop drain window and replica retirement budgets |
| [Model Deployment Scaling Simulation](model-deployment-scaling-simulation.md) | `synthetic simulation` | Deterministic local three-algorithm scaler simulation |
| [Instance Metrics Reference](instances-metrics.md) | `contract` | `Instance` metrics subresource and node exporter gauges |

---

**See also** — [Architecture](../getting-started/architecture.md) (system overview and workload flow) · [Accelerator Requests](../modules/devices/requests.md) (public resource key contract) · [Configuration](../modules/model-deployment/configuration.md) (public `ModelDeployment` contract)
