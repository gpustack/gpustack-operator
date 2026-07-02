# Settings & Environment Variables

GPUStack Operator is configured two ways, and the distinction matters operationally:

- **[Online-adjustable Settings](#online-adjustable-settings)** — a fixed catalog of named values surfaced
  as the `Setting` aggregated API resource (backed by a delegated Kubernetes Secret). An administrator
  reads and changes them at runtime with `kubectl`; the operator picks the new value up on its next
  reconcile. Each Setting is **seeded once** from a `GPUSTACK_*` environment variable on first deploy, then
  lives in the cluster.
- **[Deploy-time environment variables](#deploy-time-environment-variables)** — read **once at process
  startup** via the environment. Changing one requires restarting/redeploying the affected component. The
  Worker (WK) copies every `GPUSTACK_`-prefixed variable from its own Pod spec onto the Device Manager (DM)
  DaemonSets, so setting a `GPUSTACK_*` variable on the Worker Deployment propagates to the DMs
  automatically.

> **First-deploy seeding vs. runtime changes.** On first deploy, `settings.Initialize` creates the
> delegated Secret `gpustack-settings` (in the system namespace) and seeds every Setting from
> `GPUSTACK_<UPPER_SNAKE_NAME>` — e.g. `node-management-manual` ← `GPUSTACK_NODE_MANAGEMENT_MANUAL` — or the
> built-in default when that variable is unset. On later restarts the operator only **backfills Settings
> missing** from the Secret; it **never overwrites** a value already stored there. So a `GPUSTACK_*`
> variable is an *initial seed only* — after the first deploy, change a Setting through the `Setting`
> resource (below), not by editing the environment (an env edit does not re-apply while a stored value
> exists).

## Online-Adjustable Settings

Settings are exposed as the namespaced `Setting` resource (`gpustack.ai/v1`, short name `set`, category
`gpustack`) in the system namespace, and persist in the delegated `gpustack-settings` Secret. The catalog is
fixed — the resource serves `get,list,watch,apply,update,patch` (no create/delete); you edit a Setting's
**value**, not its existence.

```bash
# List every setting and its current value
kubectl -n gpustack-system get settings          # or: kubectl -n gpustack-system get set

# Inspect / change one
kubectl -n gpustack-system get setting node-management-manual -o yaml
kubectl -n gpustack-system edit setting node-management-manual
kubectl -n gpustack-system patch setting instance-type-derived-from-node --type merge -p '{"spec":{"value":"false"}}'
```

| Setting | Env seed (first deploy) | Default | Effect |
|---------|-------------------------|---------|--------|
| `container-registry` | `GPUSTACK_CONTAINER_REGISTRY` | *(blank)* | Registry to pull images from, for all built-in applications' deployments. |
| `container-namespace` | `GPUSTACK_CONTAINER_NAMESPACE` | *(blank)* | Namespace to pull images from, for all built-in applications' deployments. |
| `image-pull-secrets` | `GPUSTACK_IMAGE_PULL_SECRETS` | *(blank)* | Image pull secret for pulling images, for all built-in applications' deployments. |
| `image-pull-policy` | `GPUSTACK_IMAGE_PULL_POLICY` | `IfNotPresent` | Image pull policy for all built-in applications' deployments. |
| `instance-general-resources-overcommit` | `GPUSTACK_INSTANCE_GENERAL_RESOURCES_OVERCOMMIT` | `true` | Overcommit an Instance's general resources: when enabled, a general unit requests 800m CPU / 128Mi RAM and one-eighth local storage, an accelerated unit 100m CPU / 128Mi RAM and one-eighth local storage — so e.g. a 1C/4Gi + 128Gi type requesting 2 accelerators and 64Gi storage resolves to 200m/256Mi + 8Gi. |
| `instance-ssh-server-image` | `GPUSTACK_INSTANCE_SSH_SERVER_IMAGE` | `gpustack/ssh-server:v1.1.0` | Image of the SSH server used when deploying Instances. |
| `instance-access-static-address` | `GPUSTACK_INSTANCE_ACCESS_STATIC_ADDRESS` | *(blank)* | Static access address for all Instances; when unset, the access address is generated from host IPs. |
| `instance-access-wildcard-dns` | `GPUSTACK_INSTANCE_ACCESS_WILDCARD_DNS` | *(blank)* | Wildcard DNS for all Instances (e.g. `traefik.me`), used to build a per-Instance domain `<instance-host-ip>.<wildcard-dns>`. Only effective when `instance-access-static-address` is not set. |
| `node-management-manual` | `GPUSTACK_NODE_MANAGEMENT_MANUAL` | `false` | Skip auto-managing nodes. When `false`, the operator auto-onboards discovered nodes by injecting the `gpustack.ai/managed=true` label; when `true`, an administrator must opt nodes in manually. Read per-reconcile. |
| `instance-type-mixed-on-node` | `GPUSTACK_INSTANCE_TYPE_MIXED_ON_NODE` | `true` | Whether one node may surface both a GPU and a CPU-only InstanceType. When `true`, a node is summarized into every type it can serve; when `false`, a node with accelerators yields only a GPU InstanceType and a CPU-only node only a general one. Read per-reconcile. |
| `instance-type-derived-from-node` | `GPUSTACK_INSTANCE_TYPE_DERIVED_FROM_NODE` | `true` | Whether the operator auto-derives the InstanceType and its backing ClusterQueue from node hardware. When `true`, the operator derives both; when `false`, it only aligns the ResourceFlavor and the administrator defines the ClusterQueue via the InstanceType API. Read per-reconcile. |

The last three (`node-management-manual`, `instance-type-mixed-on-node`, `instance-type-derived-from-node`)
are read **per-reconcile** (`ShouldValueBool(ctx)`), so flipping one re-converges the scheduling chain on the
next reconcile without restarting the operator.

## Deploy-Time Environment Variables

These are read once at process startup; changing a value requires restarting the affected component. The
Worker copies every `GPUSTACK_*` variable onto the Device Manager DaemonSets, so a `GPUSTACK_*` variable set
on the Worker Deployment propagates to the DMs automatically.

### Configuration Knobs

| Variable | Default | Component | Effect |
|----------|---------|-----------|--------|
| `GPUSTACK_DATA_DIR` | `/var/lib/gpustack` | all | Root directory for data storage. |
| `GPUSTACK_CONF_DIR` | `/etc/gpustack` | all | Root directory for configuration and metadata, e.g. bundled Helm charts. |
| `GPUSTACK_PCI_CLASS_PREFIXES` | `02,03,0b,12` | WK + DM | Comma-separated PCI class prefixes treated as display/accelerator devices (see the [PCI class registry](https://admin.pci-ids.ucw.cz/read/PD)). Read in two places with identical parsing: the WK injects it into the NFD chart's `deviceClassWhitelist` and the acceleratable-detection NodeFeatureRule, and the DM applies it to its local sysfs PCI scan. |
| `GPUSTACK_DEVICES_GROUP_ID_WITH_MEMORY` | `false` | DM | When `true`, the devices group ID gains a memory-size suffix (e.g. `nvidia-tesla-t4-16g` instead of `nvidia-tesla-t4`), so same-model devices with different VRAM sizes form distinct groups. |
| `GPUSTACK_GENERAL_NODE_KEY_WITH_CPU_NAME` | `false` | WK | When `true`, the general(CPU) node key blends the CPU identity — the sanitized CPU name, or the NFD cpu-model family/id as a fallback — e.g. `intel-xeon-platinum-8358`, so Kueue flavors/queues subdivide by CPU model. When `false`, every node shares the `generic` key, pooling all CPUs together. The key never encodes os/arch; that is appended in full to the ResourceFlavor/ClusterQueue names (e.g. `-linux-arm64`) and pinned on the flavor's `spec.nodeLabels`. |

### Per-Manufacturer Overrides

Three override patterns are expanded for every known manufacturer (`amd`, `ascend`, `cambricon`, `hygon`, `iluvatar`, `metax`, `mthreads`, `nvidia`, `thead`). They are read by both the WK and the DM, so the WK-to-DM propagation described above keeps the two sides consistent.

- `GPUSTACK_${MANUFACTURER}_PCI_VENDOR_ID` — overrides the PCI vendor ID used for NFD node selection and device scanning. Accepts either `${vendor}` or `${class}_${vendor}`.
- `GPUSTACK_${MANUFACTURER}_ACCELERATABLE_RESOURCE_NAME` — overrides the extended resource name the scheduling chain allocates against.
- `GPUSTACK_${MANUFACTURER}_ACCELERATABLE_RUNTIME_NAME` — overrides the container runtime class name used for accelerated workloads.

Defaults:

| Manufacturer | PCI vendor ID | Resource name | Runtime name |
|--------------|---------------|---------------|--------------|
| `amd` | `1002` | `amd.com/gpu` | `amd` |
| `ascend` | `19e5` | `huawei.com/npu` | `ascend` |
| `cambricon` | `cabc` | `cambricon.com/mlu` | `cambricon` |
| `hygon` | `1d94` | `hygon.com/dcu` | `hygon` |
| `iluvatar` | `1e3e` | `iluvatar.com/gpu` | `iluvatar` |
| `metax` | `9999` | `metax-tech.com/gpu` | `metax` |
| `mthreads` | `1ed5` | `mthreads.com/gpu` | `mthreads` |
| `nvidia` | `10de` | `nvidia.com/gpu` | `nvidia` |
| `thead` | `1ded` | `alibabacloud.com/ppu` | — (none) |

> T-Head has no default runtime name, but `GPUSTACK_THEAD_ACCELERATABLE_RUNTIME_NAME` is still honored and can supply one.

### Vendor Toolkit Paths

The DM device bindings locate vendor libraries through conventional toolkit-home variables. Each falls back to the listed default directory when unset.

| Variable | Default | Manufacturer | Effect |
|----------|---------|--------------|--------|
| `ROCM_HOME`, then `ROCM_PATH` | `/opt/rocm` | AMD | ROCm root, searched for `librocm_smi64.so` / `libamd_smi.so` / `libhsa-runtime64.so`. |
| `ROCM_SMI_LIB_PATH` | — | AMD | Extra directory searched for `librocm_smi64.so` before the ROCm root. |
| `AMD_SMI_LIB_PATH` | — | AMD | Extra directory searched for `libamd_smi.so` before the ROCm root. |
| `CANN_HOME` | `/usr/local/Ascend` | Ascend | Driver root, searched for `libdcmi.so`. |
| `ASCEND_TOOLKIT_HOME` | `/usr/local/Ascend/cann`, falling back to `/usr/local/Ascend/ascend-toolkit/latest/runtime` | Ascend | CANN toolkit root used by the Ascend detector. |
| `NEUWARE_HOME` | `/usr/local/neuware` | Cambricon | Neuware root, searched for `libcndev.so`. |
| `PPU_HOME` | `/usr/local/PPU_SDK` | Hygon | PPU SDK root, searched for `libhgml.so`. |
| `COREX_HOME` | `/usr/local/corex` | Iluvatar | CoreX root, searched for `libixml.so`. |
| `MACA_HOME` | `/opt/maca` | MetaX | MACA root, searched for `libmxsml.so`. |
| `LD_LIBRARY_PATH` | — | all | Standard library search path, consulted as an additional source of candidate library directories. |

### Kubernetes-Injected Variables

These are populated by the Pod specs that GPUStack Operator itself renders (Downward API or Service environment). They are not user-facing knobs — listed here only for completeness.

| Variable | Default | Effect |
|----------|---------|--------|
| `KUBERNETES_NODE_NAME` | — (required) | Name of the node the Pod runs on; the DM uses it to name its NodeFeature/Devices objects. |
| `KUBERNETES_POD_NAME` | — | The WK's own Pod name, used to read back its container spec (image, pull policy, `GPUSTACK_*` env) for rendering the DM DaemonSets. |
| `KUBERNETES_POD_NAMESPACE` | `gpustack-system` | System namespace where managed resources live. |
| `KUBERNETES_POD_IP` | — | Overrides the auto-detected primary host IP in topology discovery. |
| `KUBERNETES_SERVICE_NAME` | `gpustack-operator-worker` | Service name used for system routing. |
| `KUBERNETES_SERVICE_HOST` | — | Standard in-cluster marker; its presence tells the embedded runner it is inside a cluster. |

### Proxy and Internal Flags

| Variable | Default | Effect |
|----------|---------|--------|
| `ALL_PROXY` / `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | — | Standard proxy settings, passed through to the embedded Kubernetes installer. |
| `NO_PROXY` / `no_proxy` | — | Also parsed (hosts, IPs, CIDRs) to bypass the proxy on direct HTTP calls. |
| `_RUNNING_INSIDE_CONTAINER_` | `false` | Internal marker baked into the container image; switches data/conf paths to their absolute in-container locations. Not intended to be set by users. |
