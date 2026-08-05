# GPUStack Operator

[![CI](https://img.shields.io/github/actions/workflow/status/gpustack/gpustack-operator/ci.yml?label=ci&branch=main)](https://github.com/gpustack/gpustack-operator/actions)
[![License](https://img.shields.io/github/license/gpustack/gpustack-operator?label=license)](https://github.com/gpustack/gpustack-operator#license)
[![Docker Pulls](https://img.shields.io/docker/pulls/gpustack/gpustack-operator)](https://hub.docker.com/r/gpustack/gpustack-operator)

**Share one accelerator card between many workloads — safely, on nine vendors, with one Helm install.**

GPUStack Operator discovers the accelerators on every node, profiles them into normalized per-device
units, and materializes a Kueue-based scheduling chain your workloads queue against. On top of
whole-card and shared allocation it adds:

- **Logical (software) slicing on 6 vendors** — one card serves many workloads, each with its own
  **compute budget and VRAM budget, set independently**. The limits are applied at runtime by the
  vendor's own facility — a preload library, a kernel module, a sub-device — not just by accounting.
- **Physical (hardware) partitioning** — NVIDIA MIG and T-Head's own MIG-named partitioning, as a
  resource family of its own: the device plugin materializes the instance during allocation and reclaims
  it once the Pod exits.

Built on [Node Feature Discovery](https://github.com/kubernetes-sigs/node-feature-discovery) and
[Kueue](https://github.com/kubernetes-sigs/kueue) — both vendored into this chart, so you install one
thing and the operator brings up the rest.

## Accelerator support

Every vendor below supports **whole-card** (`<base>`) and **shared** (`<base>.shared`, 10 ownership
slots per card) requests. What differs is how — and whether — a single card can be *split*:

| Vendor | Class | Kubernetes resource | Logical slicing (`.sliced`) | Physical partitioning (`.partitioned`) |
|---|---|---|---|---|
| **AMD** | GPU | `amd.com/gpu` | — | — |
| **Cambricon** | MLU | `cambricon.com/mlu` | ✅ | — |
| **Huawei Ascend** | NPU | `huawei.com/npu` | ✅ | — |
| **Hygon** | DCU | `hygon.com/dcu` | ✅ | — |
| **Iluvatar** | GPU | `iluvatar.com/gpu` | — | — |
| **MetaX** | GPU | `metax-tech.com/gpu` | ✅ | — |
| **Moore Threads** | GPU | `mthreads.com/gpu` | ✅ | — |
| **NVIDIA** | GPU | `nvidia.com/gpu` | ✅ | ✅ **MIG** |
| **T-Head** | PPU | `alibabacloud.com/ppu` | ✅ | ✅ **MIG** |

- **Logical slicing is software.** The card stays whole; each container gets the vendor's own sharing
  facility applied to it — a preload library, a kernel module, a sub-device — budgeting its compute
  (SM / aicore %) and its VRAM separately, so `50 %` of the memory at `40 %` of the compute is a valid
  ask. The VRAM budget is a hard cap everywhere; the compute budget is not one kind of thing — on Moore
  Threads it is a scheduling weight, and on T-Head it is a duty-cycle share of a short window, so a
  slice may exceed its percentage instantaneously and not across the window.
- **Physical partitioning is hardware.** It is a driver-level configuration mode an administrator
  enables on the card — the operator observes it, never flips it. See [NVIDIA MIG
  Operations](./docs/operation/nvidia-mig.md) and [T-Head PPU Partitioning
  Operations](./docs/operation/thead-mig.md).
- Each vendor's PCI vendor ID, resource name, and runtime class can be overridden — see [Settings &
  Environment Variables](./docs/settings.md#per-manufacturer-overrides).

## Quick Start

**Prerequisites.** Kubernetes `>= 1.23` (required by the bundled Kueue), Helm `3.8+`, and
cluster-admin. Your accelerator nodes need the vendor driver and container runtime already installed —
the operator brings the device plugin, not the driver. cert-manager is optional: with the default
`global.certmanager.enabled=auto`, its resources are created only when cert-manager is detected;
otherwise every component self-signs.

**1. Install the chart.**

```bash
helm repo add gpustack https://docs.gpustack.ai/gpustack-operator/charts
helm install gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system --create-namespace
```

**2. Wait for the control plane.**

```bash
kubectl -n gpustack-system rollout status deployment/gpustack-operator-worker
kubectl -n gpustack-system get pods
```

**3. Watch the scheduling chain materialize.** Nothing to configure — every accelerator node reports a
`Devices` ledger, and one pool appears per accelerator model (plus one collapsed CPU pool):

```bash
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
  cards, **SH**ared ownership slots, logically **SL**iced VRAM budget, physically **P**ar**T**itioned
  instances. Run it with `-w` and watch it move as workloads come and go.

**4. Run your first sliced workload** — 20 % of one card's VRAM, on a pool from step 3 whose `SL` view
is non-zero (substitute your own `InstanceType` name; the `nvidia-smi` check below is NVIDIA-specific):

```bash
kubectl apply -f - <<'EOF'
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: sliced-demo
  namespace: default
spec:
  type: gpustack--nvidia-a10g-linux-amd64      # an InstanceType name from step 3
  image: ubuntu:24.04
  command: ["sleep", "86400"]
  resources:
    accelerator: "1"
    acceleratorSlicedMemoryPercentage: 20      # 20% of the card's VRAM
    acceleratorSlicedCoresPercentage: 100      # and up to 100% of its compute
  volume:
    ephemeral:
      capacity: 1Gi
EOF
```

Wait for it to come up:

```bash
kubectl wait --for=jsonpath='{.status.phase}'=Ready instance/sliced-demo --timeout=10m
```

Then look inside — the container sees only its slice, on a 24 GiB A10G:

```console
$ kubectl exec sliced-demo -- nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
NVIDIA A10G, 4912 MiB
```

...and the pool's capacity moves from `1/1 10/10 100/100 0/0` to `0/0 0/0 80/80 0/0` — the card is now
80 % sliceable, and no longer available whole. `kubectl delete instance sliced-demo` releases it.

**5. Or request a card straight from a Pod.** Any Pod that carries the pool's entrance label
(`kueue.x-k8s.io/queue-name`, the **ENTRANCE** column from step 3) can ask for a card in one of four
shapes — `nvidia.com/gpu` below is the manufacturer's base resource, so an Ascend workload asks for
`huawei.com/npu`, `huawei.com/npu.sliced`, and so on:

| Ask | `resources.limits` |
|---|---|
| **Exclusive** — 2 whole cards | `nvidia.com/gpu: "2"` |
| **Shared** — 3 of a card's 10 ownership slots | `nvidia.com/gpu.shared: "3"` |
| **Logical slice** — 20 % of one card's VRAM at 40 % of its compute | `nvidia.com/gpu.sliced: "1"` + `nvidia.com/gpu.sliced.memory-percentage: "20"` + `nvidia.com/gpu.sliced.cores-percentage: "40"` |
| **Physical partition** — one MIG `3g.40gb` instance | `nvidia.com/gpu.partitioned: "1"` + `nvidia.com/gpu.partitioned.mig-3g.40gb: "1"` |

The logical slice above, as a whole Pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: sliced-pod
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: gpustack-fnv64-c4680bb149644f1c   # ENTRANCE from step 3
spec:
  restartPolicy: Never
  containers:
    - name: main
      image: ubuntu:24.04
      command: ["sleep", "86400"]
      resources:
        limits:
          nvidia.com/gpu.sliced: "1"                     # always exactly 1 card
          nvidia.com/gpu.sliced.memory-percentage: "20"  # or .sliced.memory-mib, never both
          nvidia.com/gpu.sliced.cores-percentage: "40"   # defaults to 100 when omitted
```

One Pod asks for one family, in one container group; the `.units` keys are derived by the admission
webhook and must never be set by hand. All seven rules, with an accepted and a rejected example each,
are in [Accelerator Requests](./docs/accelerator-requests.md).

**6. Where to go next.** [Walkthrough](./docs/walkthrough.md) for the same run on a four-node cluster ·
[Accelerator Requests](./docs/accelerator-requests.md) for the request contract ·
[Architecture](./docs/architecture.md) for how it all works · [all
documentation](./docs/README.md).

**Uninstall.**

```bash
helm uninstall gpustack-operator --namespace gpustack-system
```

> Node Feature Discovery, Kueue, and the NFS/S3 CSI drivers are **vendored subcharts** of this chart,
> each behind an `enabled` switch — there is nothing else to deploy, and one `values.yaml` configures
> all of it. Because they belong to this release, `helm uninstall` also deletes Kueue's CRDs and
> therefore every ClusterQueue, ResourceFlavor and Workload in the cluster; install with
> `--set kueue.enabled=false` to bring your own instead. To also remove the leftovers the worker
> creates at runtime, add `--set cleanupOnUninstall=true` at install time. Upgrading an install from
> v0.7.x or earlier is a one-time ownership transfer — see [Migrating to the bundled
> subcharts](./docs/migration/to-subcharts.md).

## Features

- **Multi-vendor discovery** — auto-detects accelerators from 9 vendors and profiles each node's
  CPU / RAM / storage and per-card capacity, with no per-node configuration; pools can optionally split
  by CPU manufacturer (`instance-type-aware-cpu-manufacturer`).
- **Slicing with decoupled compute + memory isolation** — see [Accelerator
  support](#accelerator-support) above.
- **Per-card over-admission protection** — a [five-gate admission
  model](./docs/architecture/admission.md) (Pod webhook → Kueue credits → per-card `AdmissionCheck` →
  scheduler/kubelet → device-plugin allocator), backed by the authoritative `Devices` ledger, catches
  the fragmentation cases a coarse quota total cannot see, so a request never lands on a card that has
  no room.
- **Live capacity, as a real resource** — each pool is a materialized `InstanceType` CRD whose
  `.status` carries the four views above. Admins can also **declare** `InstanceType`s directly
  (immutable-sized inputs, descriptors enriched at admission), and the list-only `InstanceTypeFlavor`
  catalog surfaces every pool's hardware profile.
- **SSH-enabled Instances** — launch a workload with an SSH sidecar that shares the same sliced card
  through a capability-stripped shell, for interactive development on a slice.
- **Multi-cluster aggregation** — the `worker-gateway` aggregates InstanceTypes and capacity across
  upstream clusters into a single view.

## How it works

One `gpustack-operator` binary, three subcommands — `worker` (control plane), `worker-gateway`
(cross-cluster aggregation), `device-manager` (per-node DaemonSet) — driving four stages:

1. **Bootstrap** — the chart deploys NFD, Kueue, the CSI drivers and the Device Manager DaemonSets as
   subcharts of one release.
2. **Device discovery** — NFD labels nodes by PCI vendor and CPU identity; the Device Manager detects
   accelerators and maintains a per-card `Devices` ledger.
3. **Capacity profiling** — the worker normalizes each node's CPU/RAM/storage and per-card capacity
   into labels.
4. **Queue construction & admission** — five controllers materialize those labels into a Kueue
   `ResourceFlavor` → `ClusterQueue` → `LocalQueue` chain plus an `InstanceType` CRD, gated per card by
   an `AdmissionCheck`.

Read [Architecture](./docs/architecture.md) for the 8-minute version, including the life of one sliced
request.

## Documentation

For running the operator, for operating a cluster, and for changing the code, the most-used pages:

| Page | What it answers |
|---|---|
| [Architecture](./docs/architecture.md) | What the operator builds, and the vocabulary the rest assumes |
| [Accelerator Requests](./docs/accelerator-requests.md) | Every resource key, and the rules admission enforces |
| [Walkthrough](./docs/walkthrough.md) | A recorded end-to-end run with real output |
| [Settings & Environment Variables](./docs/settings.md) | Runtime settings and every `GPUSTACK_*` |
| [NVIDIA MIG Operations](./docs/operation/nvidia-mig.md) | The MIG runbook, mode changes included |
| [T-Head PPU Partitioning Operations](./docs/operation/thead-mig.md) | The same runbook for T-Head's own MIG-named partitioning |
| [High Availability](./docs/operation/high-availability.md) | The replica knob per component |
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
