# GPUStack Operator

[![CI](https://img.shields.io/github/actions/workflow/status/gpustack/gpustack-operator/ci.yml?label=ci&branch=main)](https://github.com/gpustack/gpustack-operator/actions)
[![License](https://img.shields.io/github/license/gpustack/gpustack-operator?label=license)](https://github.com/gpustack/gpustack-operator#license)
[![Docker Pulls](https://img.shields.io/docker/pulls/gpustack/gpustack-operator)](https://hub.docker.com/r/gpustack/gpustack-operator)

**Turn raw accelerator hardware into a ready-to-use, quota-managed scheduling chain — automatically.**

GPUStack Operator is a Kubernetes operator that discovers accelerators on every node, profiles their
capacity into normalized per-device units, and materializes them into a Kueue-based scheduling chain your
workloads can queue against. It supports **9 accelerator vendors** across GPU / NPU / MLU / DCU / PPU, and
adds **logical slicing** so a single card can be safely shared by multiple workloads with independent
compute and memory budgets.

Built on top of [Node Feature Discovery (NFD)](https://github.com/kubernetes-sigs/node-feature-discovery)
and [Kueue](https://github.com/kubernetes-sigs/kueue) — you install one chart, and the operator brings up
the rest.

## Features

- **Multi-vendor discovery** — auto-detects accelerators from 9 vendors (see [Supported Accelerators](#supported-accelerators)) and profiles each node's CPU / RAM / storage and per-card capacity, with no per-node configuration; scheduling pools can optionally split by CPU manufacturer (`instance-type-aware-cpu-manufacturer`).
- **Logical slicing with decoupled compute + memory isolation** — share one physical card among workloads, capping compute (SM / aicore %) independently of VRAM. Runtime isolation is enforced by a vendor preload library (NVIDIA HAMi-core `libvgpu.so`, Ascend vcann-rt `libvruntime.so`), not just by accounting.
- **Per-card over-admission protection** — a four-gate admission model (Kueue credits → scheduler/kubelet → per-card `AdmissionCheck` → the `Devices` ledger) catches the fragmentation cases a coarse quota total can't see, so a request never lands on a card that has no room.
- **Live capacity, as a real resource** — each pool surfaces as a materialized `InstanceType` CRD whose `.status` carries a three-view (free whole cards / shareable slots / sliceable VRAM units). `kubectl get instancetype -w` shows capacity move as workloads allocate and free. Admins can also **declare** `InstanceType`s directly (required, immutable-sized inputs, descriptors enriched at admission), and the list-only `InstanceTypeFlavor` catalog surfaces every pool's hardware profile.
- **SSH-enabled Instances** — launch a workload with an SSH sidecar that shares the same sliced card through a capability-stripped shell, for interactive development on a slice.
- **Multi-cluster aggregation** — the `worker-gateway` aggregates InstanceTypes and capacity across upstream clusters into a single view.

## Supported Accelerators

| Vendor | Kubernetes resource | Class |
|--------|---------------------|-------|
| NVIDIA | `nvidia.com/gpu` | GPU |
| AMD | `amd.com/gpu` | GPU |
| Huawei Ascend | `huawei.com/npu` | NPU |
| Cambricon | `cambricon.com/mlu` | MLU |
| Hygon | `hygon.com/dcu` | DCU |
| Iluvatar | `iluvatar.com/gpu` | GPU |
| MetaX | `metax-tech.com/gpu` | GPU |
| Moore Threads | `mthreads.com/gpu` | GPU |
| T-Head | `alibabacloud.com/ppu` | PPU |

Each vendor's PCI vendor ID, resource name, and runtime class can be overridden via `GPUSTACK_*` environment
variables — see [Settings & Environment Variables](./docs/settings.md#per-manufacturer-overrides).

## Quick Start

**Prerequisites:** Kubernetes `>= 1.23` (required by the bundled Kueue), Helm `3.8+`, and cluster-admin.
cert-manager is optional — with the default `worker.certmanager.enabled=auto`, its resources are created only
when cert-manager is detected; otherwise the worker self-signs.

Install the chart:

```bash
helm install gpustack-operator oci://docker.io/gpustack/charts/gpustack-operator \
  --namespace gpustack-system --create-namespace
```

Check the worker rollout:

```bash
kubectl -n gpustack-system rollout status deployment/gpustack-operator-worker
```

Uninstall (add `--set cleanupOnUninstall=true` at install time, or see the
[migration guide](./docs/migration/from-v0.5.md), to also remove runtime-installed leftovers):

```bash
helm uninstall gpustack-operator --namespace gpustack-system
```

> Node Feature Discovery, Kueue, and the CSI drivers are bundled into the operator image and installed by
> the worker at runtime — there is nothing else to deploy.

## How It Works

A single `gpustack-operator` binary exposes three subcommands — `worker` (control plane), `worker-gateway`
(cross-cluster aggregation), and `device-manager` (per-node DaemonSet) — that drive a four-stage chain:

1. **Bootstrap** — the Worker installs the NFD and Device Manager DaemonSets.
2. **Device discovery** — Node Feature Discovery labels nodes by PCI vendor and CPU identity; the Device Manager then detects accelerators, reports per-device feature labels, and maintains a per-card `Devices` ledger.
3. **Capacity profiling** — the Worker normalizes each node's CPU/RAM/storage and per-accelerator capacity into profile labels, keyed by the node's CPU identity.
4. **Queue construction & admission** — five Worker controllers (`NodeFlavorReconciler`, `InstanceTypeReconciler`, `NodeQueueReconciler`, `NodeQueueEntranceReconciler`, `NodeDevicesAdmissionReconciler`) materialize the labels into a Kueue `ResourceFlavor` → `ClusterQueue` (one isolated queue per pool, **no Cohort**) → `LocalQueue`, a materialized `InstanceType` CRD, and a per-card `gpustack-node-devices` `AdmissionCheck`. `InstanceTypeReconciler` owns the queue's existence + labels + status; `NodeQueueReconciler` owns its quota, `StopPolicy`, and admission-check reference.

See [Architecture](./docs/architecture.md) for the stage-by-stage detail, label/naming conventions, and a
worked example cluster.

## Documentation

- [Architecture](./docs/architecture.md) — how device discovery, node capacity profiling, and the Kueue scheduling chain work, with a worked example cluster.
- [Development](./docs/development.md) — build, lint, test, code generation, and dependency management commands.
- [Settings & Environment Variables](./docs/settings.md) — online-adjustable settings (`kubectl`) plus every `GPUSTACK_*` env, per-manufacturer overrides, and vendor toolkit paths.
- [NVIDIA MIG Operations](./docs/operation/nvidia-mig.md) — the administrator runbook for enabling/disabling NVIDIA MIG on a node, reboot recovery, and the no-automatic-descheduling policy.
- [Migrating from v0.5.x](./docs/migration/from-v0.5.md) — upgrading an existing install to a higher version across the scheduling-chain refactor.

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
