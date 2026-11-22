# Devices Scheduling Capture

Evidence: recorded capture. Raw multi-object `kubectl` YAML captures recorded on a four-node
`linux/amd64` cluster (`node-cpu`, `node-a10g`, `node-t4-a`, `node-t4-b`) with
`instance-type-derived-from-node=true`. Object YAML retains *`metadata.labels`*, *`spec`* and
*`status`*.

This capture predates the shared card-count reading (its `SH` `onceMaxRequest` figures are shares)
and canonical shared pool entrances for custom sibling `InstanceType`s.

## Contents

- [Initial pool objects](#initial-pool-objects)
- [NodeFeature toggle](#nodefeature-toggle)
- [CPU awareness objects](#cpu-awareness-objects)

## Initial pool objects

Captured from `node-a10g` (4-core `amd-epyc-7r32`, 1 × NVIDIA A10G) and `node-cpu` (16-core
`amd-epyc-7r13`) with `instance-type-aware-cpu-manufacturer=false`.

### Node

Labeled by `node-a10g-gpustack-worker` (`general.*` + `managed`) and
`node-a10g-gpustack-device-manager` (`acceleratable.*`):

```yaml
apiVersion: v1
kind: Node
metadata:
  name: node-a10g
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    general.feature.gpustack.ai/amd-epyc-7r32.count: "4"
    acceleratable.feature.gpustack.ai/nvidia: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g.count: "1"
    acceleratable.feature.gpustack.ai/nvidia-a10g.memory: "24Gi"
    acceleratable.feature.gpustack.ai/nvidia-a10g.cores: "10240"
    acceleratable.feature.gpustack.ai/nvidia-a10g.family: "Ampere"
    acceleratable.feature.gpustack.ai/nvidia-a10g.product: "NVIDIA-A10G"
    acceleratable.feature.gpustack.ai/nvidia-a10g.comcap: "8.6"
    acceleratable.feature.gpustack.ai/nvidia.driver-version: "580.159.03"
    acceleratable.feature.gpustack.ai/nvidia.runtime-version: "13.0"
```

### Devices

Cluster-scoped ledger for `node-a10g`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: Devices
metadata:
  name: node-a10g
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
status:
  groups:
    - id: a10g
      manufacturer: nvidia
      accelerators:
        - id: GPU-e0587d2e-127c-4fb8-e2c1-6e517529f575
          index: 0
          mode: 0
          remaining: 1600000  # per-accelerator credit ledger the AdmissionCheck reads
```

### ResourceFlavor

Accelerated flavor (`gpustack--${gKey}--${aKey}-${os}-${arch}-${count}d`):

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g.count: "1"     # per-node accelerator count
    acceleratable.feature.gpustack.ai/nvidia-a10g.capacity: "1"  # pooled capacity (nodes × count)
    resource.gpustack.ai/type: nodes  # operator-owned marker
spec:
  nodeLabels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g.count: "1"
```

Non-accelerated CPU flavor (`gpustack--${gKey}-${os}-${arch}-${count}c`):

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: gpustack--amd-epyc-7r13-linux-amd64-16c
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "false"
    general.feature.gpustack.ai/amd-epyc-7r13: "true"
    general.feature.gpustack.ai/amd-epyc-7r13.count: "16"
    general.feature.gpustack.ai/amd-epyc-7r13.capacity: "16"
    resource.gpustack.ai/type: nodes
spec:
  nodeLabels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd-epyc-7r13: "true"
    general.feature.gpustack.ai/amd-epyc-7r13.count: "16"
```

### ClusterQueue

Collapsed accelerated pool queue covering `credits.gpustack.ai/nvidia`:

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: gpustack--nvidia-a10g-linux-amd64
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    resource.gpustack.ai/type: instancetypes
spec:
  admissionChecksStrategy:
    admissionChecks:
      - name: gpustack-node-devices
  resourceGroups:
    - coveredResources:
        - credits.gpustack.ai/nvidia
      flavors:
        - name: gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d
          resources:
            - name: credits.gpustack.ai/nvidia
              nominalQuota: 1600k
```

### InstanceType

Derived schedulable pool for `nvidia-a10g`:

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceType
metadata:
  name: gpustack--nvidia-a10g-linux-amd64
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    schedule.gpustack.ai/derived-from-node: "true"
    schedule.gpustack.ai/queue-entrance: gpustack-fnv64-c4680bb149644f1c
spec:
  acceleratable: true
  acceleratorGroup: nvidia-a10g
  generalGroup: generic  # aggregated because CPU awareness is disabled
  os: linux
  arch: amd64
  manufacturer: nvidia
  product: NVIDIA-A10G
  family: Ampere
  memory: 24Gi
  cores: "10240"
  feature:
    logicalSliced:
      maxSize: 128
      coresPercentageOvercommit: true
      memoryPercentageStep: 1
    physicalSliced:
      maxSize: 0
  unitResources:  # nvidia-a10g preset
    cpu: "8"
    ram: 64Gi
  localStorage: 100Gi
status:
  phase: Active
  accelerator:
    capacity: "1"
    onceMaxRequest: "1"
    remaining: "1"
  acceleratorShared:
    capacity: "10"
    onceMaxRequest: "10"
    remaining: "10"
  acceleratorSliced:
    capacity: "100"
    onceMaxRequest: "100"
    remaining: "100"
  acceleratorPartitioned:
    capacity: "0"
    onceMaxRequest: "0"
    remaining: "0"
  entrance: gpustack-fnv64-c4680bb149644f1c
```

### InstanceTypeFlavor

OS/architecture-agnostic catalog view (no *`metadata.labels`*):

```yaml
apiVersion: worker.gpustack.ai/v1
kind: InstanceTypeFlavor
metadata:
  name: gpustack--nvidia-a10g
spec:
  acceleratable: true
  acceleratorGroup: nvidia-a10g
  manufacturer: nvidia
  product: NVIDIA-A10G
  memory: 24Gi
  cores: "10240"
  sliceable: true
```

## NodeFeature toggle

Worker `NodeFeature` carrying `gpustack.ai/managed` and `general.*` labels for `node-a10g`:

```yaml
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: node-a10g-gpustack-worker
  namespace: gpustack-system
  labels:
    nfd.node.kubernetes.io/node-name: node-a10g
    app.kubernetes.io/part-of: gpustack-operator-worker
spec:
  labels:
    gpustack.ai/managed: "true"
    general.feature.gpustack.ai/amd: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"
    general.feature.gpustack.ai/amd-epyc-7r32.count: "4"
```

Setting `gpustack.ai/managed` to `"false"` retires `gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64-1d`
and drops generic CPU capacity from `48/72` to `48/68`; restoring `"true"` rebuilds the flavor on
the next reconcile.

## CPU awareness objects

Enabling `instance-type-aware-cpu-manufacturer=true` creates CPU-aware `ClusterQueue`, `InstanceType`
and `InstanceTypeFlavor` objects while keeping all seven `ResourceFlavor`s byte-identical.

Aware `ClusterQueue` carrying `general.feature.gpustack.ai/amd-epyc-7r32`:

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: gpustack--amd-epyc-7r32--nvidia-a10g-linux-amd64
  labels:
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    feature.gpustack.ai/acceleratable: "true"
    acceleratable.feature.gpustack.ai/nvidia-a10g: "true"
    general.feature.gpustack.ai/amd-epyc-7r32: "true"  # CPU key added when CPU awareness is enabled
    resource.gpustack.ai/type: instancetypes
```

Full `kubectl get instancetypeflavor` capture after enabling CPU awareness:

```text
NAME                                                    GENERALGROUP                 ACCELERATORGROUP   ACCELERATABLE   MANUFACTURER   PRODUCT                                          MEMORY   CORES   SLICEABLE
gpustack--amd-epyc-7r13                                 amd-epyc-7r13                                   false           amd            AMD EPYC 7R13 Processor
gpustack--amd-epyc-7r32                                 amd-epyc-7r32                                   false           amd            AMD EPYC 7R32
gpustack--intel-xeon-platinum-8259cl                    intel-xeon-platinum-8259cl                      false           intel          Intel(R) Xeon(R) Platinum 8259CL CPU @ 2.50GHz
gpustack--amd-epyc-7r32--nvidia-a10g                    amd-epyc-7r32                nvidia-a10g        true            nvidia         NVIDIA-A10G                                      24Gi     10240   true
gpustack--intel-xeon-platinum-8259cl--nvidia-tesla-t4   intel-xeon-platinum-8259cl   nvidia-tesla-t4    true            nvidia         Tesla-T4                                         16Gi     2560    true
```

---

**See also** — [Heterogeneous Devices Walkthrough](https://docs.gpustack.ai/gpustack-operator/v0.12.0/docs/walkthroughs/devices/scheduling/index.md) (reproducible step-by-step walkthrough) · [Scheduling Chain](devices-scheduling.md) (controller ownership and naming) · [Instance Type Unit Resources Reference](instance-type-unit-resources.md) (per-product CPU and RAM presets)
