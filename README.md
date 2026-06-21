# GPUStack Operator

[![Go Report Card](https://goreportcard.com/badge/github.com/gpustack/gpustack-operator)](https://goreportcard.com/report/github.com/gpustack/gpustack-operator)
[![CI](https://img.shields.io/github/actions/workflow/status/gpustack/gpustack-operator/ci.yml?label=ci&branch=main)](https://github.com/gpustack/gpustack-operator/actions)
[![License](https://img.shields.io/github/license/gpustack/gpustack-operator?label=license)](https://github.com/gpustack/gpustack-operator#license)
[![Docker Pulls](https://img.shields.io/docker/pulls/gpustack/gpustack-operator)](https://hub.docker.com/r/gpustack/gpustack-operator)

GPUStack Operator provides a fantastic way to manage accelerator resources in Kubernetes.

Built on top of [Node Feature Discovery](https://github.com/kubernetes-sigs/node-feature-discovery) and [Kueue](https://github.com/kubernetes-sigs/kueue), it discovers accelerators (GPU/NPU/TPU) on every node, profiles node capacity into normalized per-device units, and materializes the results into a Kueue-based scheduling chain (`ResourceFlavor` → `ClusterQueue` → `Cohort` / `LocalQueue`).

## How It Works

A single `gpustack-operator` binary exposes three subcommands — `worker` (control plane), `worker-gateway` (cross-cluster aggregation), and `device-manager` (per-node DaemonSet) — that drive a four-stage chain:

1. **Bootstrap** — the Worker installs the NFD and Device Manager DaemonSets.
2. **Device discovery** — Node Feature Discovery labels nodes by PCI vendor and CPU identity; the Device Manager then detects accelerators and reports per-device feature labels.
3. **Capacity profiling** — the Worker normalizes each node's CPU/RAM/storage and per-accelerator capacity into profile labels, keyed by the node's CPU identity.
4. **Queue construction** — four Worker controllers materialize the labels into Kueue `ResourceFlavor`, `Cohort`, `ClusterQueue`, and `LocalQueue` objects.

See [Architecture](./docs/architecture.md) for the stage-by-stage detail, label/naming conventions, and a worked example cluster.

## Documentation

- [Architecture](./docs/architecture.md) — how device discovery, node capacity profiling, and the Kueue scheduling chain work, with a worked example cluster.
- [Development](./docs/development.md) — build, lint, test, code generation, and dependency management commands.
- [Environment Variables](./docs/environment-variables.md) — every `GPUSTACK_*` knob, per-manufacturer overrides, and vendor toolkit paths.

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
