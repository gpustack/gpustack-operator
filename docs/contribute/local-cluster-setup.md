# Local Cluster Setup

Use [kind](https://kind.sigs.k8s.io/docs/user/quick-start/) to provide a local Kubernetes
cluster for development and integration tests. Keep its kubeconfig separate from other clusters.

## Contents

- [Prerequisites](#prerequisites)
- [Step 1: Install tools](#step-1-install-tools)
- [Step 2: Create a cluster](#step-2-create-a-cluster)
- [Step 3: Verify access](#step-3-verify-access)
- [Existing clusters](#existing-clusters)
- [Mock device manager](#mock-device-manager)
- [Cleanup](#cleanup)

## Prerequisites

- A running container runtime supported by kind, such as Docker.
- Permission to create containers and download node images.

## Step 1: Install tools

On macOS with Homebrew:

```sh
brew install kind kubectl
```

For other platforms, follow the official [kind installation instructions](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
and [kubectl installation instructions](https://kubernetes.io/docs/tasks/tools/#kubectl).

```sh
kind version
kubectl version --client
```

## Step 2: Create a cluster

Choose an unused cluster name and kubeconfig path:

```sh
mkdir -p "$HOME/.kube"
kind create cluster --name gpustack-dev \
  --kubeconfig "$HOME/.kube/gpustack-kind.yaml" --wait 120s
```

This uses kind's default node image. Record the actual server version when testing.

## Step 3: Verify access

```sh
kubectl --kubeconfig "$HOME/.kube/gpustack-kind.yaml" \
  --context kind-gpustack-dev --request-timeout=15s cluster-info
kubectl --kubeconfig "$HOME/.kube/gpustack-kind.yaml" \
  --context kind-gpustack-dev --request-timeout=15s get nodes -o wide
```

Provide the kubeconfig path and context name to the local test runner. Keep the file private and
out of Git.

## Existing clusters

Export an existing cluster's kubeconfig to an unused file:

```sh
kind get kubeconfig --name gpustack-dev > "$HOME/.kube/gpustack-kind.yaml"
```

Then verify access using the preceding commands. For multi-node clusters on a remote Docker host
with any Kubernetes release, use the Terraform module in
[testing/infra/clusters/kind](../../testing/infra/clusters/kind/README.md).

## Mock device manager

Run `device-manager` in CPU-only mock mode on kind or k3s clusters to test scheduling, slicing,
partitioning, and fault recovery without physical accelerators or vendor drivers. Pass a YAML or
JSON specification via `--mock-devices-file` (or `GPUSTACK_MOCK_DEVICES_FILE`).

Mock mode bypasses vendor probes, host `sysfs` scans, and `/dev` injection, stamps
`device.gpustack.ai/mock: "true"` on each node's `Devices` object, and hot-reloads when the
specification file changes.

### Specification formats

Write a specification in one of two mutually exclusive top-level shapes: a flat single-node spec
applied to every node, or a multi-node map (`default` plus per-node overrides under `nodes`,
keyed by `KUBERNETES_NODE_NAME`). Do not mix flat node fields with `default` or `nodes`.

```yaml
# Flat form: one preset applied to every node.
preset: nvidia-h100
count: 4
```

```yaml
# Multi-node heterogeneous form: control-plane nodes receive zero cards via default.
default:
  groups: []
nodes:
  gpustack-dev-worker:
    preset: nvidia-h100
    count: 4
  gpustack-dev-worker2:
    preset: nvidia-h100-mig
    count: 2
  gpustack-dev-worker3:
    presets:
      - preset: amd-mi308x
        count: 2
      - preset: ascend-910b2
        count: 2
```

- Set `preset` for a single vendor per node, `presets` for multiple vendors on one node, or
  `manufacturers` when multiple vendors on one node also need per-vendor `processes` or
  `partitions`.
- Set `count` to truncate or expand the preset's default card count.
- Set `interfaces` and `rdmaDevices` at the node level to synthesize NIC and *RDMA* verbs
  inventories.

### Built-in presets

Embedded presets live under
[pkg/devicemanager/mockspec/presets](../../pkg/devicemanager/mockspec/presets/). The filename
without `.yaml` is the `preset` value.

| Preset | Manufacturer | Model | Memory (MiB) | Default cards | Slicing |
|---|---|---|---|---:|---|
| `amd-mi300x` | `amd` | AMD Instinct MI300X OAM | 196592 | 2 | logical |
| `amd-mi308x` | `amd` | AMD Instinct MI308X OAM | 196592 | 8 | logical |
| `amd-rx-7800-xt` | `amd` | AMD Radeon RX 7800 XT | 16368 | 2 | logical |
| `ascend-310p3` | `ascend` | 310P3 | 44280 | 2 | logical |
| `ascend-910b2` | `ascend` | 910B2 | 65536 | 8 | logical |
| `cambricon-mlu590` | `cambricon` | MLU590 | 81920 | 8 | logical |
| `hygon-k100-ai` | `hygon` | K100_AI | 65520 | 8 | logical |
| `hygon-k100-ai-mig` | `hygon` | K100_AI | 65520 | 8 | physical |
| `iluvatar-bi-v150` | `iluvatar` | Iluvatar BI-V150 | 32768 | 8 | logical |
| `metax-c500` | `metax` | MXC500 | 65536 | 8 | logical |
| `mthreads-s5000` | `mthreads` | MTT S5000 | 81920 | 8 | logical |
| `nvidia-gb10` | `nvidia` | NVIDIA GB10 | 138932 | 1 | logical |
| `nvidia-h100` | `nvidia` | NVIDIA H100 80GB HBM3 | 81559 | 8 | logical |
| `nvidia-h100-mig` | `nvidia` | NVIDIA H100 80GB HBM3 | 81559 | 8 | physical |
| `nvidia-h200` | `nvidia` | NVIDIA H200 | 143771 | 8 | logical |
| `nvidia-rtx-4090-48g` | `nvidia` | NVIDIA GeForce RTX 4090 | 49140 | 2 | logical |
| `nvidia-rtx-5090d` | `nvidia` | NVIDIA GeForce RTX 5090 D | 32607 | 1 | logical |
| `thead-ppu` | `thead` | PPU-ZW810E | 98304 | 16 | logical |
| `thead-ppu-mig` | `thead` | PPU-ZW810E | 98304 | 8 | physical |

### Overrides and faults

Layer `groupOverride`, `cardOverrides` (keyed by logical card index), and `fault` directly on top
of any preset.

```yaml
preset: nvidia-h100
count: 4
groupOverride:
  driverVersion: "575.57.08"
cardOverrides:
  1:
    queryState: unqueryable # ok, unqueryable, or fatalError
    metrics:
      temperature: 85
fault:
  initError:
    enabled: false          # explicit false keeps parameters for later toggling
    failCount: 2
    message: "transient mock driver warm-up"
```

- `unhealthy: true` marks a card unhealthy while keeping it queryable; `queryState: unqueryable`
  skips live reads and retains cached card state as unhealthy.
- `fault` supports `initError`, `countError`, and `totalFailure`. Omitting `enabled` auto-enables
  the fault when `failCount` or `message` is set; explicit `enabled: false` disables it.

### Cluster deployment

Store the specification in a `ConfigMap` in the operator namespace, then enable the mock
`DaemonSet` in the Helm chart:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: mock-devices
  namespace: gpustack-system
data:
  devices.yaml: |
    preset: nvidia-h100
    count: 4
```

```sh
helm upgrade --install gpustack-operator deploy/gpustack-operator/chart \
  --namespace gpustack-system --create-namespace \
  --set deviceManager.mock.enabled=true \
  --set deviceManager.mock.configMapName=mock-devices \
  --set deviceManager.mock.configMapKey=devices.yaml
```

The chart mounts the `ConfigMap` directory at `/etc/gpustack/mock` without `subPath`, so updating
the `ConfigMap` hot-reloads the specification without restarting `device-manager` `Pod`s. Set
`deviceManager.mock.manufacturers` to restrict which manufacturers the mock `DaemonSet` serves;
leave `deviceManager.mock.omitRuntimeClass=true` (default) on CPU-only clusters without GPU
container runtimes.

To inspect a specification offline without a cluster, run one-shot `detect` or `monitor`:

```sh
gpustack-operator device-manager detect --mock-devices-file=./devices.yaml
```

### Custom presets

Add a new embedded preset by dropping `<name>.yaml` under
`pkg/devicemanager/mockspec/presets/` with top-level `manufacturer` and `groups` fields (nested
`preset` references are rejected). Rebuild the binary and verify with
`go test -race ./pkg/devicemanager/mockspec/...`.

## Cleanup

The cluster owner removes the cluster when testing is complete:

```sh
kind delete cluster --name gpustack-dev \
  --kubeconfig "$HOME/.kube/gpustack-kind.yaml"
```

---

**See also** — [Development](development.md) (build and test commands) ·
[Settings](../reference/settings.md) (`GPUSTACK_MOCK_DEVICES_FILE` reference) ·
[Command Reference](../reference/commands.md) (`--mock-devices-file` flag) ·
[Installation Modes](../operate/installation-modes.md) (operator deployment).

**Next** → [Development](development.md) — run the project checks against the selected cluster.
