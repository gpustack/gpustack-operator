# GPUStack Operator Documentation

## Reading paths

- First workload: [Usage](../README.md#usage), [Accelerator Requests](modules/devices/requests.md), [Heterogeneous Devices Walkthrough](walkthroughs/devices/scheduling.md), and [RDMA Operations](modules/rdma/operations.md)
- Cluster setup: [Architecture](getting-started/architecture.md), [Installation Modes](operate/installation-modes.md), [Settings & Environment Variables](reference/settings.md), [Vendor Prerequisites](getting-started/vendor-prerequisites.md), and [Preflight Checks](modules/devices/preflight.md)
- Feature runbooks: [Topology-Aware Scheduling Operations](modules/topology/operations.md), [Model Store Operations](modules/model-delivery/operations.md), [High Availability Operations](operate/high-availability.md), [RDMA Operations](modules/rdma/operations.md), and MIG operations ([NVIDIA](modules/devices/nvidia-mig.md), [T-Head](modules/devices/thead-mig.md), [Hygon](modules/devices/hygon-mig.md))
- Serving examples: [Walkthroughs](walkthroughs/_index.md), [Model Deployment Scaling Walkthrough](walkthroughs/model-deployment/scaling.md), and [Model Deployment Scaling Benchmark](modules/model-deployment/scaling-benchmark.md)
- Upgrades: [Migrating to Bundled Subcharts](operate/migration/bundled-subcharts.md) and [Migration Troubleshooting](operate/migration/troubleshooting.md)
- Contributor entry: [Architecture](getting-started/architecture.md), [Module map](#module-map), [Internals](contribute/internals.md), [Development](contribute/development.md), and [`gpustack-operator-docs`](../.agents/skills/gpustack-operator-docs/SKILL.md)

## Module map

### Heterogeneous Devices

- ID: `devices`
- Use for: discover accelerators, map capacity labels to Kueue queues, enforce admission gates, and run MIG partitioning
- Aliases: `Devices`, `AcceleratorType`, `GPU`, `NPU`, `MIG`, `accelerator`, `device plugin`
- Guides: [Heterogeneous Devices](modules/devices/_index.md), [Accelerator Requests](modules/devices/requests.md), [Preflight Checks](modules/devices/preflight.md), [Device Discovery](reference/devices-discovery.md), [Scheduling Chain](reference/devices-scheduling.md), [Admission](reference/devices-admission.md)
- Specs: [Accelerator resource modes](../specs/2026-06-21-accelerator-resource-modes-refactor.md), [Soft-slicing runtime isolation](../specs/2026-06-25-accelerator-soft-slicing-runtime-isolation.md), [Unified pool](../specs/2026-06-29-instancetype-unified-pool-refactor.md), [Credit scoring](../specs/2026-06-24-unified-credit-base-scoring.md), [Preflight unhealthy accelerators](../specs/2026-10-29-preflight-unhealthy-accelerators.md), [Accelerator types and model resources](../specs/2026-11-11-accelerator-type-and-model-deployment-resources.md), [Ascend A5 communication resources and host root](../specs/2026-11-19-model-deployment-ascend-a5-comm-res.md)
- Code: [pkg/devicemanager](../pkg/devicemanager/), [pkg/deviceplugin](../pkg/deviceplugin/), [pkg/nodefeature](../pkg/nodefeature/), [pkg/utils/hostfs](../pkg/utils/hostfs/), [pkg/worker/controllers/worker](../pkg/worker/controllers/worker/)
- Related: [RDMA Networking](#rdma-networking), [Topology](#topology), [GPU Instances](#gpu-instances)
- Skills: [e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md), [xbuild-and-verify](../.agents/skills/gpustack-operator-xbuild-and-verify/SKILL.md)

### RDMA Networking

- ID: `rdma`
- Use for: discover NIC and RDMA link topology, select RDMA endpoints, and align requests with TopologyManager
- Aliases: `RDMA`, `NIC`, `RoCE`, `link state`, `rdma.capable`
- Guides: [RDMA Networking](modules/rdma/_index.md), [RDMA Operations](modules/rdma/operations.md), [Network Topology](reference/rdma-network-topology.md), [RDMA Networking Walkthrough](walkthroughs/rdma/network-endpoints.md)
- Specs: [NIC/RDMA topology](../specs/2026-09-02-devices-nic-rdma-topology.md), [Endpoint discovery and selection filters](../specs/2026-10-30-rdma-network-selection.md), [Extended resource](../specs/2026-09-20-rdma-extended-resource.md), [NVIDIA fabric domain](../specs/2026-09-20-nvidia-fabric-domain.md), [Fabric by protocol](../specs/2026-09-22-fabric-device-by-protocol.md), [Interface count](../specs/2026-09-22-fabric-interface-count.md)
- Code: [pkg/devicemanager/detector](../pkg/devicemanager/detector/), [detector/rdma.go](../pkg/devicemanager/detector/rdma.go), [pkg/device/rdma.go](../pkg/device/rdma.go), [api/worker/v1alpha1/devices.go](../api/worker/v1alpha1/devices.go), [api/worker/v1/devices_rdma_selection_policy.go](../api/worker/v1/devices_rdma_selection_policy.go), [extensionapis/worker/devices_rdma_selection_policy.go](../pkg/worker/extensionapis/worker/devices_rdma_selection_policy.go), [webhooks/worker/devices_rdma_selection_policy.go](../pkg/worker/webhooks/worker/devices_rdma_selection_policy.go), [pkg/deviceplugin/rdma_policy.go](../pkg/deviceplugin/rdma_policy.go), [rdma_selection_observer.go](../pkg/deviceplugin/rdma_selection_observer.go), [rdma_selection.go](../pkg/deviceplugin/rdma_selection.go), [rdma_allocate.go](../pkg/deviceplugin/rdma_allocate.go), [pkg/nodefeature/rdma.go](../pkg/nodefeature/rdma.go), [pkg/nodefeature/fabric.go](../pkg/nodefeature/fabric.go)
- Related: [Heterogeneous Devices](#heterogeneous-devices)
- Skills: [e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md)

### Topology

- ID: `topology`
- Use for: publish zone and rack topology domains, configure Kueue TAS placement, and diagnose pending groups
- Aliases: `TAS`, `Topograph`, `topology source`, `zone`, `placement`
- Guides: [Topology](modules/topology/_index.md), [Topology-Aware Scheduling Operations](modules/topology/operations.md), [Topology-Aware Scheduling](reference/topology-scheduling.md), [Hierarchical Placement Walkthrough](walkthroughs/topology/hierarchical-placement.md)
- Specs: [Topology discovery](../specs/2026-09-22-topology-discovery.md), [Model placement preference](../specs/2026-09-26-model-placement-preference.md)
- Code: [topology_source.go](../pkg/worker/controllers/worker/topology_source.go), [node_topology.go](../pkg/worker/controllers/worker/node_topology.go)
- Related: [Heterogeneous Devices](#heterogeneous-devices), [Model Deployment](#model-deployment)
- Skills: [e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md)

### KV Cache

- ID: `kv-cache`
- Use for: deploy shared KV cache backends, manage pool grants and quotas, and inject cache config into workloads
- Aliases: `KVCacheBackend`, `KVCachePool`, `Mooncake`, `prefix cache`, `injection`
- Guides: [KV Cache](modules/kv-cache/_index.md), [Shared Cache Walkthrough](walkthroughs/kv-cache/shared-cache.md), [KV Cache Backend](modules/kv-cache/backend.md), [KV Cache Pools and Bindings](modules/kv-cache/pool.md), [KV Cache Injection Reference](reference/kv-cache-injection.md)
- Specs: [Backend](../specs/2026-08-28-kv-cache-backend.md), [Pool](../specs/2026-08-28-kv-cache-pool.md), [Injection](../specs/2026-08-28-kv-cache-injection.md), [Media and scaling](../specs/2026-09-05-kv-cache-media-and-scaling.md), [High availability](../specs/2026-09-06-kv-cache-backend-high-availability.md), [Ascend A5 KV cache, ModelTransfer, and DRAM](../specs/2026-11-20-ascend-a5-kvcache-modeltransfer-and-dram-normalization.md)
- Code: [pkg/worker/kvcache](../pkg/worker/kvcache/), [pkg/worker/kvcache/inject](../pkg/worker/kvcache/inject/), [pkg/worker/controllers/worker](../pkg/worker/controllers/worker/)
- Related: [Model Deployment](#model-deployment)
- Skills: [e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md)

### Model Delivery

- ID: `model-delivery`
- Use for: resolve model artifacts, operate node caches, prefetch weights, and run remote or peer weight delivery
- Aliases: `ModelArtifact`, `NodeModelStore`, `ModelStore`, `ModelPrefetch`, `model cache`, `Hugging Face`, `ModelExpress`
- Guides: [Model Delivery](modules/model-delivery/_index.md), [Model Artifact](modules/model-delivery/artifact.md), [Node Model Store](reference/model-delivery-node-store.md), [Model Store Operations](modules/model-delivery/operations.md), [Remote Weight Delivery](modules/model-delivery/remote.md), [Model Prefetch Walkthrough](walkthroughs/model-delivery/prefetch.md)
- Specs: [Model artifact](../specs/2026-09-25-model-artifact.md), [Node model store](../specs/2026-09-25-node-model-store.md), [Prefetch](../specs/2026-09-27-model-prefetch.md), [Peer sync](../specs/2026-09-27-model-peer-sync.md), [Image source](../specs/2026-09-27-model-image-source.md), [Remote weight loading](../specs/2026-10-28-model-artifact-remote-weight-loading.md), [ModelExpress peer delivery](../specs/2026-11-09-modelexpress-peer-d2d-and-cache-sharing.md), [Settings sync semantics](../specs/2026-10-19-settings-env-sync-and-default-tracking.md), [Accelerator types and model resources](../specs/2026-11-11-accelerator-type-and-model-deployment-resources.md), [Ascend A5 KV cache, ModelTransfer, and DRAM](../specs/2026-11-20-ascend-a5-kvcache-modeltransfer-and-dram-normalization.md)
- Code: [pkg/modelartifact](../pkg/modelartifact/), [pkg/modelmanager](../pkg/modelmanager/), [pkg/modelstore](../pkg/modelstore/), [pkg/modeltransfer/artifact](../pkg/modeltransfer/artifact/), [model_deployment_transfer_source.go](../pkg/worker/controllers/worker/model_deployment_transfer_source.go), [model_deployment_modelexpress.go](../pkg/worker/controllers/worker/model_deployment_modelexpress.go), [pack/mirrored-modelexpress-server](../pack/mirrored-modelexpress-server/), [pkg/modeltransfer](../pkg/modeltransfer/), [model-transfer/python](../model-transfer/python/), [pyproject.toml](../model-transfer/python/pyproject.toml), [pack/model-transfer](../pack/model-transfer/), [acceptance](../model-transfer/python/gpustack_model_transfer/acceptance/)
- Related: [Model Deployment](#model-deployment), [RDMA Networking](#rdma-networking), [KV Cache](#kv-cache), [GPU Instances](#gpu-instances)
- Skills: [e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md), [benchmark](../.agents/skills/gpustack-operator-benchmark/SKILL.md)

### Model Deployment

- ID: `model-deployment`
- Use for: configure serving roles, disaggregated P/D and EPD topologies, routers, online runtime control, scaling, and drain lifecycles
- Aliases: `ModelDeployment`, `ModelDeploymentScaler`, `scaler`, `autoscaling`, `prefill`, `decode`, `router`, `vLLM`, `SGLang`, `replica`, `EPD`, `encode`
- Guides: [Model Deployment](modules/model-deployment/_index.md), [Configuration](modules/model-deployment/configuration.md), [Parameters](reference/model-deployment-parameters.md), [Model Deployment Scaling](modules/model-deployment/scaling.md), [Model Deployment Scaling Simulation](reference/model-deployment-scaling-simulation.md), [Model Deployment Scaling Benchmark](modules/model-deployment/scaling-benchmark.md), [Model Deployment Scaling Walkthrough](walkthroughs/model-deployment/scaling.md), [Online Runtime Control](modules/model-deployment/runtime-control.md), [Online Runtime Control Walkthrough](walkthroughs/model-deployment/runtime-control.md), [Engine Compatibility](modules/model-deployment/engine-versions.md), [Prefill and Decode](modules/model-deployment/prefill-decode.md), [Prefill and Decode Benchmark](modules/model-deployment/prefill-decode-benchmark.md), [Encode, Prefill and Decode](modules/model-deployment/encode-prefill-decode.md), [Encode, Prefill and Decode Benchmark](modules/model-deployment/encode-prefill-decode-benchmark.md), [Elastic EP](modules/model-deployment/elastic-ep.md), [Elastic EP Benchmark](modules/model-deployment/elastic-ep-benchmark.md), [External DP](modules/model-deployment/external-dp.md), [External DP Benchmark](modules/model-deployment/external-dp-benchmark.md), [Model Deployment Routing](modules/model-deployment/routing.md), [Model Deployment Metrics](reference/model-deployment-metrics.md), [Model Deployment Status](reference/model-deployment-status.md), [Model Deployment Shutdown](reference/model-deployment-shutdown.md), [Prefill and Decode Walkthrough](walkthroughs/model-deployment/prefill-decode.md), [Encode, Prefill and Decode Walkthrough](walkthroughs/model-deployment/encode-prefill-decode.md), [Elastic EP Walkthrough](walkthroughs/model-deployment/elastic-ep.md), [External DP Walkthrough](walkthroughs/model-deployment/external-dp.md), [Multi-Host Serving Walkthrough](walkthroughs/model-deployment/multi-host.md)
- Specs: [Consolidation](../specs/2026-09-18-kvcache-and-model-deployment-consolidation.md), [Role replica admission](../specs/2026-09-19-role-replica-admission-unit.md), [Role configuration mutability](../specs/2026-10-08-model-deployment-role-mutability.md), [P/D pairing and router](../specs/2026-09-12-model-deployment-pd-pairing-and-router.md), [Router implementations](../specs/2026-09-19-model-deployment-router-implementations.md), [Router qualification and drain](../specs/2026-10-01-s1-router-qualification-and-drain.md), [Elastic EP resize](../specs/2026-10-01-s2-elastic-ep-resize.md), [EPD disaggregation](../specs/2026-10-13-model-deployment-epd-disaggregation.md), [Unified port 8000](../specs/2026-10-18-model-deployment-unify-port-8000.md), [Render version](../specs/2026-10-18-model-deployment-render-version.md), [Compilation cache and startup timeout](../specs/2026-10-19-compilation-cache-and-startup-timeout.md), [Docs and benchmarks](../specs/2026-10-19-model-deployment-docs-and-benchmarks.md), [Scaler](../specs/2026-10-21-model-deployment-scaler.md), [Capacity priors and evaluation](../specs/2026-10-26-inferencex-model-deployment-scaling.md), [Whole-unit disaggregated priors](../specs/2026-11-13-whole-unit-disaggregated-capacity-priors.md), [DP group router supervisor](../specs/2026-10-22-dp-group-router-supervisor.md), [KVTransfer NIXL, HIXL, and EFA](../specs/2026-11-10-model-deployment-kvtransfer-nixl-hixl.md), [KVTransfer peer backends](../specs/2026-11-11-model-deployment-kvtransfer-peer-backends.md), [Accelerator types and model resources](../specs/2026-11-11-accelerator-type-and-model-deployment-resources.md), [Ascend A5 communication resources and host root](../specs/2026-11-19-model-deployment-ascend-a5-comm-res.md), [Online runtime control and router hash decoupling](../specs/2026-11-19-model-deployment-online-runtime-control.md), [Ascend A5 KV cache, ModelTransfer, and DRAM](../specs/2026-11-20-ascend-a5-kvcache-modeltransfer-and-dram-normalization.md)
- Code: [pkg/worker/controllers/worker](../pkg/worker/controllers/worker/), [pkg/worker/scaling](../pkg/worker/scaling/), [gen/priors](../gen/priors/), [pkg/worker/elasticengine](../pkg/worker/elasticengine/), [pkg/worker/webhooks/worker](../pkg/worker/webhooks/worker/), [pack/llm-router](../pack/llm-router/), [pkg/worker/runtimecontrol](../pkg/worker/runtimecontrol/), [engine-sentinel/python](../engine-sentinel/python/)
- Related: [KV Cache](#kv-cache), [Model Delivery](#model-delivery), [Topology](#topology)
- Skills: [e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md), [benchmark](../.agents/skills/gpustack-operator-benchmark/SKILL.md), [priors](../.agents/skills/gpustack-operator-priors/SKILL.md)

### GPU Instances

- ID: `instances`
- Use for: launch accelerator-backed container workspaces, configure SSH access, pin nodes, and read instance metrics
- Aliases: `Instance`, `InstanceType`, `SSH`, `workspace`
- Guides: [GPU Instances](modules/instances/_index.md), [Accelerator Requests](modules/devices/requests.md), [Instance Metrics Reference](reference/instances-metrics.md)
- Specs: [SSH instance slicing](../specs/2026-07-04-ssh-instance-accelerator-slicing.md), [Sidecar partition visibility](../specs/2026-07-26-ssh-sidecar-partition-visibility.md), [Node pinning and additional volumes](../specs/2026-07-31-instance-node-pinning-and-additional-volumes.md), [Utilization metrics](../specs/2026-08-07-instance-utilization-metrics.md), [Service preserved across stop](../specs/2026-10-19-preserve-instance-service-across-stop.md)
- Code: [pkg/worker/controllers/worker](../pkg/worker/controllers/worker/), [pkg/worker/extensionapis/worker](../pkg/worker/extensionapis/worker/), [pkg/devicemanager/exporter](../pkg/devicemanager/exporter/), [pkg/kubemetrics](../pkg/kubemetrics/)
- Related: [Heterogeneous Devices](#heterogeneous-devices), [Model Delivery](#model-delivery)
- Skills: [e2e](../.agents/skills/gpustack-operator-e2e/SKILL.md)

### API Changes

- ID: `api`
- Use for: evolve public `worker.gpustack.ai/v1` and internal `v1alpha1` types, extension API proxies, codegen, and RBAC
- Aliases: `worker.gpustack.ai/v1`, `v1alpha1`, `CRD`, `aggregated apiserver`
- Guides: [Development](contribute/development.md), [Model Artifact API](reference/model-delivery-artifact-api.md)
- Code: [api/worker/v1](../api/worker/v1/), [api/worker/v1alpha1](../api/worker/v1alpha1/), [pkg/worker/extensionapis](../pkg/worker/extensionapis/), [pkg/extensionapi](../pkg/extensionapi/), [pkg/systemkuberess](../pkg/systemkuberess/), [gen/api](../gen/api/), [serviceaccount.yaml](../deploy/gpustack-operator/chart/templates/worker/serviceaccount.yaml)
- Related: [Development](#development), [GPU Instances](#gpu-instances)
- Skills: [generate](../.agents/skills/gpustack-operator-generate/SKILL.md)

### Installation

- ID: `installation`
- Use for: choose chart or image installation, manage vendored subcharts and patches, verify prerequisites, and run migrations
- Aliases: `Helm chart`, `subchart`, `chart mode`, `image mode`, `values.yaml`
- Guides: [Installation Modes](operate/installation-modes.md), [Vendor Prerequisites](getting-started/vendor-prerequisites.md), [Migrating to Bundled Subcharts](operate/migration/bundled-subcharts.md)
- Specs: [Bundled apps subchart split](../specs/2026-07-28-bundled-apps-subchart-split.md), [Settings sync semantics](../specs/2026-10-19-settings-env-sync-and-default-tracking.md)
- Code: [deploy/gpustack-operator/chart](../deploy/gpustack-operator/chart/), [hack/deps.sh](../hack/deps.sh), [pkg/worker/kuberess](../pkg/worker/kuberess/), [pkg/setting](../pkg/setting/), [pkg/worker/settings](../pkg/worker/settings/)
- Related: [Development](#development), [Heterogeneous Devices](#heterogeneous-devices)
- Skills: [chart-e2e](../.agents/skills/gpustack-operator-chart-e2e/SKILL.md), [chart-subcharts-manage](../.agents/skills/gpustack-operator-chart-subcharts-manage/SKILL.md)

### Development

- ID: `development`
- Use for: build, lint, test, generate code, run test clusters, cut releases, and publish the documentation site
- Aliases: `make`, `lint`, `codegen`, `vendored dependencies`, `staging`, `Hugo`, `GitHub Pages`
- Guides: [Development](contribute/development.md), [Local Cluster Setup](contribute/local-cluster-setup.md), [Internals](contribute/internals.md), [Architecture](getting-started/architecture.md)
- Specs: [Modular Loopwright auto-sync](../specs/2026-11-17-modular-loopwright-auto-sync.md)
- Code: [Makefile](../Makefile), [hack](../hack/), [testing/infra/clusters/eks](../testing/infra/clusters/eks/README.md), [testing/infra/clusters/k3s](../testing/infra/clusters/k3s/README.md), [testing/infra/clusters/nebius](../testing/infra/clusters/nebius/README.md), [testing/infra/clusters/rke2](../testing/infra/clusters/rke2/README.md), [testing/infra/computes/nebius](../testing/infra/computes/nebius/README.md), [testing/infra/computes/spheron](../testing/infra/computes/spheron/README.md#machine-selection), [testing/infra/clusters/spheron](../testing/infra/clusters/spheron/README.md), [testing/infra/providers/spheron](../testing/infra/providers/spheron/README.md), [testing/infra/storages/incluster](../testing/infra/storages/incluster/README.md), [testing/infra/storages/s3/aws](../testing/infra/storages/s3/aws/README.md), [site.yml](../.github/workflows/site.yml), [publish.py](../hack/site/publish.py), [staging](../staging/), [gen](../gen/), [upgrade-compat.sh](../hack/check/upgrade-compat.sh), [.auto-sync](../.auto-sync/), [auto-sync.yml](../.github/workflows/auto-sync.yml), [autosync-selftest.sh](../hack/check/autosync-selftest.sh), [helm.sh](../hack/lib/helm.sh), [helm-chart-test-selftest.sh](../hack/check/helm-chart-test-selftest.sh), [build.sh](../hack/lib/build.sh), [build-pipeline-selftest.sh](../hack/check/build-pipeline-selftest.sh)
- Related: [API Changes](#api-changes), [Installation](#installation)
- Skills: [overview](../.agents/skills/gpustack-operator-overview/SKILL.md), [docs](../.agents/skills/gpustack-operator-docs/SKILL.md), [code-review](../.agents/skills/gpustack-operator-code-review/SKILL.md), [issue-pr](../.agents/skills/gpustack-operator-issue-pr/SKILL.md), [release](../.agents/skills/gpustack-operator-release/SKILL.md), [generate](../.agents/skills/gpustack-operator-generate/SKILL.md)

## All pages

### Getting Started

| Page | Description |
| --- | --- |
| [Architecture](getting-started/architecture.md) | Scheduling chain, workload flow, and core vocabulary |
| [Vendor Prerequisites](getting-started/vendor-prerequisites.md) | Third-party dependencies and per-vendor driver preparation |

### Walkthroughs

| Page | Description |
| --- | --- |
| [Walkthroughs](walkthroughs/_index.md) | Select a complete cross-module example |
| [Heterogeneous Devices Walkthrough](walkthroughs/devices/scheduling.md) | Follow a recorded accelerator scheduling run |
| [RDMA Networking Walkthrough](walkthroughs/rdma/network-endpoints.md) | Request and verify RDMA endpoints and NUMA alignment |
| [Hierarchical Placement Walkthrough](walkthroughs/topology/hierarchical-placement.md) | Place serving replicas across zones and racks |
| [Shared Cache Walkthrough](walkthroughs/kv-cache/shared-cache.md) | Stand up a shared KV cache from backend to workload |
| [Model Prefetch Walkthrough](walkthroughs/model-delivery/prefetch.md) | Cache model weights and verify captured CR status |
| [Model Deployment Walkthroughs](walkthroughs/model-deployment/_index.md) | Select a serving topology, hardware, and `AcceleratorType` example |
| [Prefill and Decode Walkthrough](walkthroughs/model-deployment/prefill-decode.md) | Deploy H100 prefill and decode roles with tested TCP transfer |
| [Encode, Prefill and Decode Walkthrough](walkthroughs/model-deployment/encode-prefill-decode.md) | Deploy encode, prefill, and decode roles for multimodal serving |
| [Model Deployment Scaling Walkthrough](walkthroughs/model-deployment/scaling.md) | Configure Shadow and Active scaling, algorithms, and retirement |
| [Online Runtime Control Walkthrough](walkthroughs/model-deployment/runtime-control.md) | Tune vLLM and SGLang live, pause roles, and resize Elastic EP |
| [Elastic EP Walkthrough](walkthroughs/model-deployment/elastic-ep.md) | Expand an H100 collective from four to eight GPUs in place |
| [External DP Walkthrough](walkthroughs/model-deployment/external-dp.md) | Deploy four fixed H100 ranks and verify Router coverage |
| [Multi-Host Serving Walkthrough](walkthroughs/model-deployment/multi-host.md) | Scale serving models across multiple GPU nodes |

### Heterogeneous Devices

| Page | Description |
| --- | --- |
| [Heterogeneous Devices](modules/devices/_index.md) | Choose an accelerator request mode |
| [Preflight Checks](modules/devices/preflight.md) | Validate node health before installation |
| [Accelerator Requests](modules/devices/requests.md) | Look up resource keys and request rules |
| [NVIDIA MIG Operations](modules/devices/nvidia-mig.md) | Enable and recover NVIDIA MIG partitions |
| [Hygon MIG Operations](modules/devices/hygon-mig.md) | Enable and recover Hygon partitions |
| [T-Head MIG Operations](modules/devices/thead-mig.md) | Enable and recover T-Head partitions |

### RDMA Networking

| Page | Description |
| --- | --- |
| [RDMA Networking](modules/rdma/_index.md) | Configure RDMA-backed workloads |
| [RDMA Operations](modules/rdma/operations.md) | Request and verify RDMA endpoints and TopologyManager |

### Topology

| Page | Description |
| --- | --- |
| [Topology](modules/topology/_index.md) | Constrain workload placement by topology domain |
| [Topology-Aware Scheduling Operations](modules/topology/operations.md) | Enable Topograph and diagnose pending placement |

### KV Cache

| Page | Description |
| --- | --- |
| [KV Cache](modules/kv-cache/_index.md) | Deploy shared inference prefix caches |
| [KV Cache Backend](modules/kv-cache/backend.md) | Configure Mooncake cache backends and capacity |
| [KV Cache Pools and Bindings](modules/kv-cache/pool.md) | Grant cache reuse domains and enforce pool quotas |
| [KV Cache Election and Failover](modules/kv-cache/leader.md) | Operate leader election, probes, and failover |
| [KV Cache Local Disk Tier](modules/kv-cache/local-disk-tier.md) | Configure local disk cache buckets and eviction |
| [KV Cache on Disk-Heavy Nodes](modules/kv-cache/disk-heavy-nodes.md) | Size memory floors on disk-heavy nodes |

### Model Delivery

| Page | Description |
| --- | --- |
| [Model Delivery](modules/model-delivery/_index.md) | Select how model weights reach workloads |
| [Model Artifact](modules/model-delivery/artifact.md) | Resolve, verify, and mount model weight artifacts |
| [Model Image Source](modules/model-delivery/image-source.md) | Package and deliver weights as OCI image volumes |
| [Model Prefetch](modules/model-delivery/prefetch.md) | Warm model weights on target nodes before launch |
| [Node-to-Node Sync](modules/model-delivery/node-to-node-sync.md) | Transfer cached weights between cluster nodes |
| [Remote Weight Delivery](modules/model-delivery/remote.md) | Load weights over the network from prepared sources |
| [Model Store Operations](modules/model-delivery/operations.md) | Operate node cache watermarks, placement, and cleanup |

### Model Deployment

| Page | Description |
| --- | --- |
| [Model Deployment](modules/model-deployment/_index.md) | Deploy managed model serving workloads |
| [Configuration](modules/model-deployment/configuration.md) | Configure roles, resources, overrides, and placement |
| [Engine Compatibility](modules/model-deployment/engine-versions.md) | Check engine releases, store versions, and transports |
| [Prefill and Decode](modules/model-deployment/prefill-decode.md) | Pair disaggregated prefill and decode roles |
| [Prefill and Decode Benchmark](modules/model-deployment/prefill-decode-benchmark.md) | Compare equal-GPU H100 aggregated and disaggregated serving |
| [Encode, Prefill and Decode](modules/model-deployment/encode-prefill-decode.md) | Disaggregate encode, prefill, and decode roles |
| [Encode, Prefill and Decode Benchmark](modules/model-deployment/encode-prefill-decode-benchmark.md) | Compare EPD, prefill/decode, and aggregated throughput |
| [Elastic EP](modules/model-deployment/elastic-ep.md) | Resize an expert-parallel collective in place |
| [Elastic EP Benchmark](modules/model-deployment/elastic-ep-benchmark.md) | Measure four- to eight-GPU H100 live expansion |
| [External DP](modules/model-deployment/external-dp.md) | Serve a fixed data-parallel group behind a managed router |
| [External DP Benchmark](modules/model-deployment/external-dp-benchmark.md) | Compare External DP ranks against wide tensor parallelism |
| [Model Deployment Scaling](modules/model-deployment/scaling.md) | Plan replica counts from serving signals and capacity priors |
| [Model Deployment Scaling Benchmark](modules/model-deployment/scaling-benchmark.md) | Measure H100 and H200 scaling latency and limits |
| [Online Runtime Control](modules/model-deployment/runtime-control.md) | Tune serving parameters, pause roles, and isolate managed routers live |
| [Model Deployment Routing](modules/model-deployment/routing.md) | Configure routing policies and verify router serving |

### GPU Instances

| Page | Description |
| --- | --- |
| [GPU Instances](modules/instances/_index.md) | Launch accelerator-backed container workspaces |

### Operations

| Page | Description |
| --- | --- |
| [Installation Modes](operate/installation-modes.md) | Choose Helm chart mode or image mode |
| [High Availability Operations](operate/high-availability.md) | Configure control-plane replicas and failover |
| [Migrating from v0.5.x](operate/migration/from-v0.5.md) | Upgrade across the queue refactor |
| [Migrating to Bundled Subcharts](operate/migration/bundled-subcharts.md) | Transfer ownership to bundled subcharts |
| [Upgrading to an Enforced Binding Dtype](operate/migration/enforced-binding-dtype.md) | Verify binding dtypes before upgrading |
| [Migration Troubleshooting](operate/migration/troubleshooting.md) | Recover stuck upgrades and namespace deletions |

## Reference

| Page | Description |
| --- | --- |
| [LLM Reference](reference/README.md) | Navigate flat reference contracts, exact aliases, and evidence labels |
| [Command Reference](reference/commands.md) | Look up `gpustack-operator` subcommands, flags, and exit codes |
| [Settings & Environment Variables](reference/settings.md) | Look up `Setting` resources and `GPUSTACK_*` variables |
| [Instance Type Unit Resources Reference](reference/instance-type-unit-resources.md) | Look up CPU and memory unit presets |
| [Device Discovery](reference/devices-discovery.md) | Trace hardware detection, slicing, and allocation |
| [Scheduling Chain](reference/devices-scheduling.md) | Map node capacity labels to Kueue queues |
| [Admission](reference/devices-admission.md) | Review webhook checks and admission gates |
| [Network Topology](reference/rdma-network-topology.md) | Inspect NIC discovery, PCI roots, and link states |
| [Topology-Aware Scheduling](reference/topology-scheduling.md) | Map topology snapshots into Kueue TAS placement |
| [KV Cache Injection Reference](reference/kv-cache-injection.md) | Attach a Pod to a KV cache pool |
| [Model Artifact API](reference/model-delivery-artifact-api.md) | Query public artifact and node cache APIs |
| [Node Model Store](reference/model-delivery-node-store.md) | Inspect node cache state, CSI mounts, and collection |
| [Parameters](reference/model-deployment-parameters.md) | Inspect synthesized engine, router, and transfer flags |
| [Model Deployment Metrics](reference/model-deployment-metrics.md) | Inspect serving, role, and router Prometheus series |
| [Model Deployment Status](reference/model-deployment-status.md) | Diagnose conditions, retirements, and scaler annotations |
| [Model Deployment Shutdown](reference/model-deployment-shutdown.md) | Follow replica retirement, drain, and unadmitted removal |
| [Model Deployment Scaling Simulation](reference/model-deployment-scaling-simulation.md) | Compare scaler algorithms on identical synthetic traffic |
| [Instance Metrics Reference](reference/instances-metrics.md) | Inspect per-instance accelerator utilization metrics |

## Contribute

| Page | Description |
| --- | --- |
| [Development](contribute/development.md) | Build, generate, test, and lint the repository |
| [Local Cluster Setup](contribute/local-cluster-setup.md) | Provision a local kind cluster for tests |
| [Internals](contribute/internals.md) | Review startup order, bindings, and naming limits |

## Conventions

- Public pages (`All pages`) open with a short intro, carry `## Contents`, stay within `H1`–`H3`, and end with `**See also**` / `**Next**` without linking to `reference/` or `contribute/`.
- Flat `reference/*.md` topic pages state an opening evidence label, carry `## Contents`, and end with `**See also**` only; `reference/README.md` is the flat navigation hub.
- Every `All pages`, `Reference`, and `Contribute` label matches its page H1 character for character; `All pages` follows site navigation order.
- Links in `Module map` route to evidence; update this index when adding or moving pages, then run `make lint docs`.
