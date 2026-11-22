# Migrating to Bundled Subcharts

Through v0.7.x, the Helm chart deployed only the worker and Device Managers while the worker installed
four separate Helm releases at runtime:

| Legacy runtime release | Bundled subchart |
|---|---|
| `gpustack-kueue` | `kueue` |
| `gpustack-node-feature-discovery` | `node-feature-discovery` |
| `gpustack-csi-driver-nfs` | `csi-driver-nfs` |
| `gpustack-csi-driver-s3` | `csi-driver-s3` |

Current releases bundle all four as subcharts under one `values.yaml` with
`worker.disableApplications: ["*"]`. Helm adopts existing resources in place, preserving live
`ClusterQueue`s, `Workload`s, `ResourceFlavor`s, `Node` labels, and mounted volumes.

## Contents

- [Helm ownership transfer](#helm-ownership-transfer)
- [Upgrade hooks](#upgrade-hooks)
- [Automatic image-mode migration](#automatic-image-mode-migration)
- [Troubleshooting](#troubleshooting)
- [Chart configuration and cleanup](#chart-configuration-and-cleanup)
- [Rollback data loss](#rollback-data-loss)
- [Verification](#verification)

## Helm ownership transfer

Before upgrading, snapshot your scheduling chain (see [Rollback data loss](#rollback-data-loss)), then
upgrade with **Helm 3.21+** and `--take-ownership`[^1]:

[^1]: `--take-ownership` requires Helm 3.21+, matching the client version pinned in `hack/lib/helm.sh`.

```bash
helm repo add gpustack https://docs.gpustack.ai/gpustack-operator/charts
helm repo update
helm upgrade gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system \
  --take-ownership
```

Pass `--take-ownership` **only once** so Helm can import objects owned by the four legacy releases.
Omit the flag on subsequent upgrades so Helm does not adopt unrelated hand-created resources.

### Release and namespace identity

Keep release name **`gpustack-operator`** and your existing namespace (**`gpustack-system`** by
default):

- **Release name**: `gpustack-operator-worker` and `gpustack-operator-device-manager-<manufacturer>`
  derive their names from the release, whereas bundled subcharts pin fixed names via `fullnameOverride`.
- **Namespace**: Helm cannot move an installed release across namespaces. Fresh installs can use any
  namespace except one matched by Kueue's `managedJobsNamespaceSelector`.

## Upgrade hooks

Two idempotent hook `Job`s run during `helm upgrade` (skipped only if you pass `--no-hooks`):

1. **`pre-upgrade` / `pre-install`**: clears any `Terminating` Kueue CRD deadlock (deleting Kueue
   webhook configurations before stripping `kueue.x-k8s.io/resource-in-use` finalizers) and applies
   subchart `crds/` updates that Helm normally skips on upgrade.
2. **`post-upgrade`**: deletes the four legacy Helm release `Secret`s (`kubectl delete secret -l owner=helm,name=<release>`)
   without uninstalling their adopted objects, then prunes obsolete legacy objects while preserving
   CRDs, `PersistentVolume`s, and `PersistentVolumeClaim`s.

Never run `helm uninstall gpustack-kueue` manually; uninstalling a legacy release deletes the objects
the operator release just adopted. Confirm that only one release remains:

```bash
helm list -n gpustack-system
```

## Automatic image-mode migration

In image mode, the worker detects its own legacy runtime release `Secret`s and enables ownership
transfer automatically for that single upgrade. No manual command is required.

## Troubleshooting

Helm hooks adopt only prior Helm releases. If Kueue or NFD was installed from raw manifests, kustomize,
or another operator, choose one of two paths:

- **Collision symptoms**: Helm rejects unowned Kueue CRDs with `invalid ownership metadata` (NFD's
  `crds/` skip when present). Worse, a manifest-installed Kueue in `kueue-system` does not collide on
  namespaced objects, so forcing `--take-ownership` leaves two Kueue controllers reconciling the same
  `Workload`s.

**Option 1: Keep the external Kueue and NFD** when other workloads depend on them:

```bash
helm upgrade --install gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system --create-namespace \
  --set kueue.enabled=false --set node-feature-discovery.enabled=false
```

The worker still applies `gpustack-cpu-info` against your existing NFD. Keep both components at
operator-compatible versions.

**Option 2: Hand Kueue over to the chart** by deleting the external Kueue's `Deployment`s, `Service`s,
RBAC, and webhook configurations (`failurePolicy: Fail`), keep its CRDs intact, and stamp Helm
ownership on those CRDs before installing:

```bash
for crd in $(kubectl get crd -o name | grep 'kueue\.x-k8s\.io'); do
  kubectl label "$crd" app.kubernetes.io/managed-by=Helm --overwrite
  kubectl annotate "$crd" meta.helm.sh/release-name=gpustack-operator \
                          meta.helm.sh/release-namespace=gpustack-system --overwrite
done
```

Review [Rollback data loss](#rollback-data-loss) first, because the chart release then owns Kueue's CRDs.

## Chart configuration and cleanup

### Manufacturer configuration

Top-level `manufacturers` moved to `global.manufacturers` so both the operator chart and bundled Kueue
read one identity map:

```yaml
global:
  manufacturers:
    nvidia:
      pciVendorID: "10de"
      resourceName: nvidia.com/gpu
      runtimeName: nvidia
      runtimeInjectsDriver: true
      partitionKind: mig
```

Default values moved automatically. If you customized top-level `manufacturers`, migrate those entries
under `global.manufacturers` and delete the old key so Device Managers do not fall back to defaults.

The chart creates `RuntimeClass` objects only where `runtimeInjectsDriver` or `runtimeInjectsDevices`
is true (`ascend`, `iluvatar`, `mthreads`, `nvidia`, and no longer `amd`). Set
`deviceManager.createRuntimeClasses=false` to opt out.

### Certificate configuration

`worker.certmanager` moved to `global.certmanager` (default `enabled: auto`) and now governs both the
worker and bundled Kueue:

```yaml
global:
  certmanager:
    enabled: auto  # enum: auto, true, false
    issuer:
      name: ""     # optional; covers both worker and Kueue without creating self-signed Issuers
      kind: Issuer
```

On clusters with cert-manager installed, `auto` switches Kueue from self-managed webhook certificates
to cert-manager `Certificate`s and `cainjector` CA bundles. To keep self-managed certificates across
all components, pass `--set global.certmanager.enabled=false` in the migration `helm upgrade` (legacy
`worker.certmanager` values are ignored).

### Disabling cert-manager

Enabling cert-manager later requires no extra steps. Disabling cert-manager after it was active fails
a plain `helm upgrade` twice: Helm rejects `Secret/kueue-webhook-server-cert` on missing ownership
metadata, and `visibility.kueue.x-k8s.io` `APIService`s reject `insecureSkipTLSVerify: true` while
`cainjector`'s `caBundle` remains set.

Switch cleanly from cert-manager to self-managed certificates in two steps:

```bash
# 1. Delete Kueue visibility APIServices carrying cainjector CA bundles (recreated by the upgrade):
kubectl delete apiservice v1beta1.visibility.kueue.x-k8s.io v1beta2.visibility.kueue.x-k8s.io

# 2. Adopt kueue-webhook-server-cert with Helm 3.21+:
helm upgrade gpustack-operator <chart> --namespace gpustack-system --reset-then-reuse-values \
  --set global.certmanager.enabled=false --take-ownership --wait
```

If cert-manager may be installed or removed independently, pin `global.certmanager.enabled` to `true`
or `false` rather than `auto`.

### Kueue removal on uninstall

Because the operator release now templates Kueue's CRDs, `helm uninstall gpustack-operator` deletes
**every `ClusterQueue`, `LocalQueue`, `ResourceFlavor`, `AdmissionCheck`, and `Workload` in the
cluster**. Set `--set kueue.enabled=false` if other workloads share Kueue.

### Leftover certificate Secrets

Older workers created one `gpustack-cert-<5-char-random>` `Secret` per restart instead of the current
content-hashed `gpustack-cert-<16-hex>` `Secret`. Inspect and optionally delete unused 5-character
`Secret`s:

```bash
kubectl -n gpustack-system get secret | grep '^gpustack-cert-'
```

## Rollback data loss

**Do not run `helm rollback` across the subchart migration.** Because pre-v0.8 revisions contain no
Kueue templates and legacy release records were retired, `helm rollback` deletes Kueue's templated
CRDs along with every `ClusterQueue`, `LocalQueue`, `ResourceFlavor`, `AdmissionCheck`, and queued
`Workload` (plus any custom `InstanceType`s if full cleanup follows).

Snapshot your scheduling objects before upgrading:

```bash
kubectl get clusterqueues,resourceflavors,admissionchecks,instancetypes,localqueues -A -o yaml \
  > gpustack-chain.yaml
```

If you must return to a pre-subchart version, uninstall and reinstall cleanly via
[Path A in Migrating from v0.5.x](/gpustack-operator/v0.12.0/docs/operate/migration/from-v0.5/index.md#path-a-uninstall-and-reinstall).

## Verification

Confirm single-release ownership, clean legacy labels, and intact scheduling resources:

```bash
NS=gpustack-system

# 1. Only gpustack-operator remains in Helm:
helm list -n "$NS"

# 2. Worker and adopted workloads are healthy under gpustack-operator:
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker
kubectl -n "$NS" get deploy,daemonset -l app.kubernetes.io/instance=gpustack-operator -o name

# 3. Zero objects retain legacy release instance labels (expect empty output for both):
LEGACY='app.kubernetes.io/instance in (gpustack-kueue,gpustack-node-feature-discovery,gpustack-csi-driver-nfs,gpustack-csi-driver-s3)'
kubectl get deploy,daemonset,svc,sa,cm,role,rolebinding -n "$NS" -o name -l "$LEGACY"
kubectl get clusterrole,clusterrolebinding,csidriver,storageclass -o name -l "$LEGACY"

# 4. Scheduling chain, Node labels, and Kueue CRDs remain healthy:
kubectl get clusterqueues,resourceflavors,instancetypes,admissioncheck
kubectl get workloads -A
kubectl get nodes -o json | grep -c 'feature.gpustack.ai/'
kubectl get crd -o custom-columns=NAME:.metadata.name,DELETING:.metadata.deletionTimestamp | grep kueue
```

---

**See also** — [Installation Modes](/gpustack-operator/v0.12.0/docs/operate/installation-modes/index.md) (mode ownership and runtime settings) ·
[High Availability Operations](/gpustack-operator/v0.12.0/docs/operate/high-availability/index.md) (replica configuration in `values.yaml`) ·
[Migrating from v0.5.x](/gpustack-operator/v0.12.0/docs/operate/migration/from-v0.5/index.md) (v0.5.x pool rename) ·
[Migration Troubleshooting](/gpustack-operator/v0.12.0/docs/operate/migration/troubleshooting/index.md) (recovering wedged upgrades)

**Next** → [Installation Modes](/gpustack-operator/v0.12.0/docs/operate/installation-modes/index.md) — configure chart values and runtime settings.
