# GPUStack Operator

[![CI](https://img.shields.io/github/actions/workflow/status/gpustack/gpustack-operator/ci.yml?label=ci&branch=main)](https://github.com/gpustack/gpustack-operator/actions)
[![License](https://img.shields.io/github/license/gpustack/gpustack-operator?label=license)](https://github.com/gpustack/gpustack-operator#license)
[![Docker Pulls](https://img.shields.io/docker/pulls/gpustack/gpustack-operator)](https://hub.docker.com/r/gpustack/gpustack-operator)

**Share one accelerator between many workloads — safely, on nine manufacturers, with one Helm install.**

GPUStack Operator discovers every node's accelerators, profiles them into normalized per-device units,
and materializes a Kueue-based scheduling chain your workloads queue against. On top of
whole-accelerator and shared allocation it adds:

- **Logical (software) slicing** — one accelerator serves many workloads, each with an **independent
  compute budget and VRAM budget**, applied at runtime by the manufacturer's own facility, not just in
  accounting.
- **Physical (hardware) partitioning** — NVIDIA MIG and T-Head's own MIG-named partitioning, as a
  resource family of its own: the device plugin materializes the instance at allocation and reclaims it
  when the Pod exits.

Built on [Node Feature Discovery](https://github.com/kubernetes-sigs/node-feature-discovery) and
[Kueue](https://github.com/kubernetes-sigs/kueue), both vendored into this chart: install one thing and
the operator brings up the rest.

## Quick Start

**Prerequisites.**

- Kubernetes `>= 1.23` (required by the bundled Kueue), Helm `3.8+`, cluster-admin.
- On accelerator nodes: the manufacturer driver and container runtime. The operator brings the device
  plugin, not the driver.
- cert-manager is optional — the default `global.certmanager.enabled=auto` uses it when detected, and
  every component self-signs otherwise.

**1. Install the chart.**

```bash
helm repo add gpustack https://docs.gpustack.ai/gpustack-operator/charts
helm install gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system --create-namespace
```

**2. Watch the scheduling chain materialize.** Nothing to configure: every accelerator node reports a
`Devices` ledger, and one pool appears per accelerator model (plus one collapsed CPU pool).

```bash
kubectl -n gpustack-system rollout status deployment/gpustack-operator-worker
kubectl get devices          # one per accelerator node, from the Device Manager
kubectl get clusterqueues    # one isolated queue per pool
kubectl get instancetypes    # the pool itself, with live capacity
```

```console
$ kubectl get instancetypes
NAME                                ENTRANCE                          UNIT(CPU/RAM)/STORAGE   ACCELERATOR(EX/SH/SL/PT)   CPU     PHASE
gpustack--generic-linux-amd64       gpustack-fnv64-3b93966fd73eb9ec   1/2Gi/100Gi             0/0 0/0 0/0 0/0            48/72   Active
gpustack--nvidia-a10g-linux-amd64   gpustack-fnv64-c4680bb149644f1c   8/64Gi/100Gi            1/1 10/10 100/100 0/0      0/0     Active
```

- **ENTRANCE** is the `LocalQueue` a workload submits against (`kueue.x-k8s.io/queue-name`).
- **ACCELERATOR(EX/SH/SL/PT)** is live capacity as `onceMaxRequest/remaining`: **EX**clusive whole
  accelerators, **SH**ared ownership slots, logically **SL**iced VRAM budget, physically
  **P**ar**T**itioned instances. Run it with `-w` to watch it move as workloads come and go.

**3. Run your first sliced workload** — 20 % of one accelerator's VRAM, on a pool from step 2 whose
`SL` view is non-zero. Substitute your own `InstanceType` name; the `nvidia-smi` check is
NVIDIA-specific:

```bash
kubectl apply -f - <<'EOF'
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: sliced-demo
  namespace: default
spec:
  type: gpustack--nvidia-a10g-linux-amd64      # an InstanceType name from step 2
  image: ubuntu:24.04
  command: ["sleep", "86400"]
  resources:
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: 20      # 20% of its VRAM
    acceleratorSlicedCoresPercentage: 100      # and up to 100% of its compute
  volume:
    ephemeral:
      capacity: 1Gi
EOF
kubectl wait --for=jsonpath='{.status.phase}'=Ready instance/sliced-demo --timeout=10m
```

The container sees only its slice of a 24 GiB A10G:

```console
$ kubectl exec sliced-demo -- nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
NVIDIA A10G, 4912 MiB
```

...and the pool's capacity moves from `1/1 10/10 100/100 0/0` to `0/0 0/0 80/80 0/0`: the accelerator
is now 80 % sliceable, and no longer available whole. `kubectl delete instance sliced-demo` releases it.

**4. Or request an accelerator straight from a Pod.** Any Pod carrying the pool's entrance label
(`kueue.x-k8s.io/queue-name`, the **ENTRANCE** column from step 2) asks in one of four shapes.
`nvidia.com/gpu` below is the manufacturer's base resource, so an Ascend workload asks for
`huawei.com/npu`, `huawei.com/npu.sliced`, and so on:

| Ask | `resources.limits` |
|---|---|
| **Exclusive** — 2 whole accelerators | `nvidia.com/gpu: "2"` |
| **Shared** — 3 of an accelerator's 10 ownership slots | `nvidia.com/gpu.shared: "3"` |
| **Logical slice** — 20 % of one accelerator's VRAM at 40 % of its compute | `nvidia.com/gpu.sliced: "1"`<br>`nvidia.com/gpu.sliced.memory-percentage: "20"`<br>`nvidia.com/gpu.sliced.cores-percentage: "40"` |
| **Physical partition** — one MIG `3g.40gb` instance | `nvidia.com/gpu.partitioned: "1"`<br>`nvidia.com/gpu.partitioned.mig-3g.40gb: "1"` |

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: sliced-pod
  labels:
    kueue.x-k8s.io/queue-name: gpustack-fnv64-c4680bb149644f1c   # ENTRANCE from step 2
spec:
  restartPolicy: Never
  containers:
    - name: main
      image: ubuntu:24.04
      command: ["sleep", "86400"]
      resources:
        limits:
          nvidia.com/gpu.sliced: "1"                     # always exactly 1 accelerator
          nvidia.com/gpu.sliced.memory-percentage: "20"  # or .sliced.memory-mib, never both
          nvidia.com/gpu.sliced.cores-percentage: "40"   # defaults to 100 when omitted
```

One Pod asks for one family, in one container group, and never sets the `.units` keys — the webhook
derives those. All seven rules, with an accepted and a rejected example each, are in [Accelerator
Requests](./docs/accelerator-requests.md).

**Next.** [Walkthrough](./docs/walkthrough.md) for the same run on a four-node cluster ·
[Architecture](./docs/architecture.md) for what the operator built · [all
documentation](./docs/README.md).

**Uninstall.**

```bash
helm uninstall gpustack-operator --namespace gpustack-system
```

> Node Feature Discovery, Kueue and the NFS/S3 CSI drivers are **vendored subcharts**, each behind an
> `enabled` switch: nothing else to deploy, one `values.yaml` for all of it. Three consequences:
>
> - `helm uninstall` also deletes Kueue's CRDs, and with them every ClusterQueue, ResourceFlavor and
>   Workload in the cluster — install with `--set kueue.enabled=false` to bring your own instead.
> - `--set cleanupOnUninstall=true` at install time also removes the leftovers the worker creates at
>   runtime.
> - Upgrading from v0.7.x or earlier is a one-time ownership transfer — see [Migrating to the bundled
>   subcharts](./docs/migration/to-subcharts.md).

## Accelerator support

Every manufacturer below supports **whole-accelerator** (`<base>`) and **shared** (`<base>.shared`, 10
ownership slots each) requests. What differs is whether one accelerator can be *split*, and how:

| Manufacturer | Class | Kubernetes resource | Logical slicing (`.sliced`) | Physical partitioning (`.partitioned`) |
|---|---|---|---|---|
| **AMD** | GPU | `amd.com/gpu` | ✅ |  |
| **Cambricon** | MLU | `cambricon.com/mlu` | ✅ |  |
| **Huawei Ascend** | NPU | `huawei.com/npu` | ✅ |  |
| **Hygon** | DCU | `hygon.com/dcu` | ✅ |  |
| **Iluvatar** | GPU | `iluvatar.com/gpu` | ✅ |  |
| **MetaX** | GPU | `metax-tech.com/gpu` | ✅ |  |
| **Moore Threads** | GPU | `mthreads.com/gpu` | ✅ |  |
| **NVIDIA** | GPU | `nvidia.com/gpu` | ✅ | ✅ **MIG** |
| **T-Head** | PPU | `alibabacloud.com/ppu` | ✅ | ✅ **MIG** |

- **Logical slicing is software.** The accelerator stays whole; each container gets the manufacturer's
  own sharing facility — a preload library, a kernel module, a sub-device — budgeting its compute
  (SM / aicore %) and its VRAM separately, so `50 %` of the memory at `40 %` of the compute is a valid
  ask. The VRAM budget is a hard cap everywhere; the compute budget is not one kind of thing:
  - **Moore Threads** — a scheduling weight.
  - **T-Head** — a duty-cycle share of a short window, so a slice may exceed its percentage
    instantaneously and not across the window.
  - **AMD** — a hardware compute-unit mask, a *ceiling* rather than a QoS: it carries no
    memory-bandwidth isolation at all, so a bandwidth-hungry neighbour still costs a slice throughput.
- **Physical partitioning is hardware.** A driver-level configuration mode an administrator enables on
  the accelerator; the operator observes it, never flips it. See [NVIDIA MIG
  Operations](./docs/operation/nvidia-mig.md) and [T-Head MIG
  Operations](./docs/operation/thead-mig.md).
- Override any manufacturer's PCI vendor ID, resource name and runtime class — see [Settings &
  Environment Variables](./docs/settings.md#per-manufacturer-overrides).

## Features

- **Multi-manufacturer discovery** — auto-detects accelerators from 9 manufacturers and profiles each
  node's CPU / RAM / storage and per-accelerator capacity, with no per-node configuration; pools
  optionally split by CPU manufacturer (`instance-type-aware-cpu-manufacturer`).
- **Slicing with decoupled compute + memory isolation** — see [Accelerator
  support](#accelerator-support).
- **Per-accelerator over-admission protection** — a [five-gate admission
  model](./docs/architecture/admission.md) (Pod webhook → Kueue credits → per-accelerator
  `AdmissionCheck` → scheduler/kubelet → device-plugin allocator), backed by the authoritative
  `Devices` ledger, catches the fragmentation a coarse quota total cannot see, so a request never lands
  on an accelerator with no room.
- **Live capacity, as a real resource** — each pool is a materialized `InstanceType` CRD whose
  `.status` carries the four views above. A cluster admin can also **declare** `InstanceType`s directly
  (immutable-sized inputs, descriptors enriched at admission); the list-only `InstanceTypeFlavor`
  catalog surfaces every pool's hardware profile.
- **SSH-enabled Instances** — launch a workload with an SSH sidecar that shares its sliced accelerator
  through a capability-stripped shell: interactive development on a slice.
- **Multi-cluster aggregation** — the `worker-gateway` aggregates InstanceTypes and capacity across
  upstream clusters into a single view.

## Documentation

The most-used pages, for running the operator, operating a cluster, and changing the code:

| Page | What it answers |
|---|---|
| [Architecture](./docs/architecture.md) | What the operator builds, and the vocabulary the rest assumes |
| [Accelerator Requests](./docs/accelerator-requests.md) | Every resource key, and the rules admission enforces |
| [Walkthrough](./docs/walkthrough.md) | A recorded end-to-end run with real output |
| [Settings & Environment Variables](./docs/settings.md) | Runtime settings and every `GPUSTACK_*` |
| [NVIDIA MIG Operations](./docs/operation/nvidia-mig.md) | The MIG runbook, mode changes included |
| [T-Head MIG Operations](./docs/operation/thead-mig.md) | The same runbook for T-Head's own partitioning |
| [High Availability Operations](./docs/operation/high-availability.md) | The replica knob per component |
| [Development](./docs/development.md) | Build, lint, test, code generation |

## License

Copyright (c) 2026 The GPUStack Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at [LICENSE](./LICENSE) file for details.

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
