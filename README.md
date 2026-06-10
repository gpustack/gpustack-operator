# GPUStack Operator

GPUStack Operator provides a fantastic way to manage accelerator resources in Kubernetes.

Built on top of [Node Feature Discovery](https://github.com/kubernetes-sigs/node-feature-discovery) and [Kueue](https://github.com/kubernetes-sigs/kueue), it discovers accelerators (GPU/NPU/TPU) on every node, profiles node capacity into normalized per-device units, and materializes the results into a Kueue-based scheduling chain (`ResourceFlavor` → `ClusterQueue` → `Cohort` / `LocalQueue`).

## Documentation

- [Architecture](./docs/architecture.md) — how device discovery, node capacity profiling, and the Kueue scheduling chain work, with a worked example cluster.

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
