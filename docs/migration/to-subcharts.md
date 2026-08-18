# Migrating to Bundled Subcharts

> **Purpose** — the one-time ownership transfer that folds the runtime-installed Kueue / NFD / CSI
> releases into the operator release, and the four things it changes permanently.
> **Audience** operators on a v0.7.x-or-earlier install · **Prerequisites** [Two install
> modes](../architecture/installation-modes.md) · **Read time** ~9 min

Through v0.7.x the chart deployed only the worker and device managers; the **worker installed**
Kueue, Node Feature Discovery and the two CSI drivers at runtime, each its own Helm release:

| Runtime release | Now |
|---|---|
| `gpustack-kueue` | the `kueue` subchart of the operator release |
| `gpustack-node-feature-discovery` | the `node-feature-discovery` subchart |
| `gpustack-csi-driver-nfs` | the `csi-driver-nfs` subchart |
| `gpustack-csi-driver-s3` | the `csi-driver-s3` subchart |

They are now **subcharts of the operator release**, sharing its `values.yaml`; the worker installs
nothing at runtime by default (`worker.disableApplications: ["*"]`).

Nothing is torn down: Helm's ownership transfer **adopts** those objects in place, so your
ClusterQueues, Workloads, ResourceFlavors, node labels and mounted volumes survive.

## Contents

- [The one-time upgrade](#the-one-time-upgrade)
- [What runs during the upgrade](#what-runs-during-the-upgrade)
- [Image mode migrates itself](#image-mode-migrates-itself)
- [If Kueue or NFD was not installed by Helm](#if-kueue-or-nfd-was-not-installed-by-helm)
- [Four things that change permanently](#four-things-that-change-permanently)
- [Do not roll back with helm rollback](#do-not-roll-back-with-helm-rollback)
- [Verify](#verify)

## The one-time upgrade

```bash
helm repo add gpustack https://docs.gpustack.ai/gpustack-operator/charts
helm repo update
helm upgrade gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system \
  --take-ownership
```

`--take-ownership` legalizes the adoption: Helm otherwise refuses every object carrying another
release's ownership metadata. Needed **once** — drop it afterwards, since it adopts *any* live
object the render names, hand-created ones included.

Helm 3.21+ only — the version this migration was validated with, and the one `hack/lib/helm.sh`
pins; older clients have no such flag.

### Keep the release name and the namespace

Keep the install instructions' release name **`gpustack-operator`** and namespace
**`gpustack-system`**. Both are load-bearing:

- The **release name** bases `gpustack-operator-worker` and
  `gpustack-operator-device-manager-<manufacturer>`, which the docs, the chart's `files/cleanup.sh`
  fallback and the e2e suites assume; the subcharts pin their own through `fullnameOverride`.
  Renaming the release renames the worker and its device managers — a rollout, not an adoption.
- The **namespace** is where your install is, and an upgrade cannot move a release. Nothing in the
  chart requires this one: Kueue's `managedJobsNamespaceSelector`, which keeps Kueue off the
  operator's own Jobs (migration hooks included), renders from the release namespace, so a fresh
  install can go anywhere. State it only for a different rule — Kueue refuses to start when it
  matches its own namespace.

## What runs during the upgrade

Two hook Jobs run the operator image, which bundles `helm`, `kubectl`, `jq` and the packaged chart.
Both are idempotent and no-ops on a healthy cluster.

**Before** the upgrade (`pre-upgrade`, and `pre-install` too):

1. **Reap a stranded Kueue.** Custom resources still holding `kueue.x-k8s.io/resource-in-use` when
   their controller is torn down strand Kueue's CRDs `Terminating` forever, failing every install of
   this chart. The Job deletes the Kueue webhook configurations first (`failurePolicy: Fail` would
   reject the finalizer-clearing patch), strips the finalizers, and drains the CRDs — on fresh
   installs too, the only way onto such a cluster.
2. **Apply the subcharts' `crds/`.** Helm applies `crds/` on install only; without this NFD's CRD
   schema changes never land and a newly enabled NFD has no CRD for the chart's `NodeFeatureRule`.

**After** it (`post-upgrade`, upgrades only):

3. **Retire the four legacy release records** (`kubectl delete secret -l owner=helm,name=<release>`),
   never `helm uninstall` — that deletes the objects just adopted. Left in place, a later
   `helm uninstall gpustack-kueue` destroys them.
4. **Prune what the legacy releases created and the new render does not contain.** Adoption rewrites
   `app.kubernetes.io/instance` on everything the render resolves, so a surviving legacy instance
   label marks what the render never mentions — unowned, invisible to `helm uninstall`. Excluded:
   CRDs (a deleted CRD takes its custom resources), PersistentVolumes, PersistentVolumeClaims.

`helm upgrade --no-hooks` skips both, to do this by hand. Afterwards `helm list` must show one
release, not five:

```bash
helm list -n gpustack-system     # expect: gpustack-operator only
```

## Image mode migrates itself

Installing the bundled chart from inside its own image, the worker detects its own legacy release
and sets the transfer for that install only, never unconditionally. Nothing to run.

## If Kueue or NFD was not installed by Helm

Everything above assumes Helm releases. Kueue and NFD also come from raw manifests, kustomize, or
another operator bundling them; nothing adopts those, and image-mode detection (a Secret labelled
`owner=helm,name=…`) never sees them.

- **Kueue collides, on its CRDs.** This chart templates them under API-group names, not
  release-derived ones, and Helm refuses *missing* ownership metadata as firmly as another release's:
  `invalid ownership metadata`, for the absent `app.kubernetes.io/managed-by: Helm` label and
  `meta.helm.sh/release-name` annotation.
- **NFD's CRDs do not collide.** Its `crds/` are applied on install, skipped when present, never
  recorded — no ownership metadata, never checked.
- **The namespaced objects not colliding is the real hazard.** A manifest-installed Kueue lives in
  `kueue-system`, this chart's in the release namespace: no conflict, no error — a forced install
  then leaves two Kueue controllers reconciling the same Workloads cluster-wide.

`--take-ownership` gets past that error, but the error is not the problem worth solving. Pick a
starting point instead.

**Keep what you have** — right whenever something else depends on that Kueue:

```bash
helm upgrade --install gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system --create-namespace \
  --set kueue.enabled=false --set node-feature-discovery.enabled=false
```

Both switches are supported paths, not workarounds; NFD off still leaves the `gpustack-cpu-info`
`NodeFeatureRule` the scheduling chain starts from, applied unconditionally by the worker against
whichever NFD is present. You then keep both at operator-compatible versions.

**Or hand them over.** Delete the non-Helm install's workloads, Services, RBAC and — importantly —
its webhook configurations, whose `failurePolicy: Fail` would reject every Workload write once their
Service is gone. Leave the CRDs alone (deleting one deletes every custom resource of that kind) and
give them the ownership Helm looks for:

```bash
for crd in $(kubectl get crd -o name | grep 'kueue\.x-k8s\.io'); do
  kubectl label    "$crd" app.kubernetes.io/managed-by=Helm --overwrite
  kubectl annotate "$crd" meta.helm.sh/release-name=gpustack-operator \
                          meta.helm.sh/release-namespace=gpustack-system --overwrite
done
```

then install normally — `--take-ownership`'s adoption, narrowed to what needs it. Read [Do not roll
back](#do-not-roll-back-with-helm-rollback) first: the release then owns those CRDs.

## Four things that change permanently

### `manufacturers` moved to `global.manufacturers`, and each entry became a row

Top-level `manufacturers: {nvidia: "10de", ...}` is now `global.manufacturers`, each entry carrying
a manufacturer's whole identity, not only its PCI vendor ID:

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

It sits under `global` because the bundled Kueue subchart reads it too — Helm's only channel from
parent values to a subchart. Unset needs nothing, the defaults moved with it; an override becomes a
row, and drop the old key, since nothing reads top-level `manufacturers` any more: one left behind
is ignored, its device-managers falling back to the default vendor IDs.

The chart now creates **`ascend`, `iluvatar`, `mthreads` and `nvidia`**, and no longer `amd`, whose
allocator injects its own device nodes. Neither is a consequence of this migration.

Creation still happens only where the class is absent or already belongs to this release.
`runtimeName`, which most rows state, names the class the operator will *use*; creating one is gated
on the narrower `runtimeInjectsDriver` or `runtimeInjectsDevices`, where the runtime is certainly
installed. `deviceManager.createRuntimeClasses=false` remains the opt-out.

### `worker.certmanager` moved to `global.certmanager`, and now answers for Kueue too

The block that sat under `worker` is `global.certmanager`, same keys, same `auto` default:

```yaml
global:
  certmanager:
    enabled: auto
    issuer:
      name: ""
      kind: Issuer
```

It moved for the reason `manufacturers` did: the bundled Kueue reads it, ending a split where the
worker decided cert-manager for itself while Kueue, defaulting off upstream, never saw `auto`'s CRD
detection.

**On a cluster that has cert-manager, this upgrade therefore moves Kueue onto it.** Kueue stops
generating and rotating its own webhook certificate and consumes a `Certificate` this chart creates,
cainjector filling the CA bundles its webhooks and CRD conversion carry.

Decide before upgrading, and pass it **in the same `helm upgrade`**: `--set
global.certmanager.enabled=false` keeps every component, worker included, self-managing. A
`worker.certmanager` left in your values is ignored.

Naming an existing issuer (`global.certmanager.issuer.name`) now covers Kueue's certificates as well
as the worker's, and no self-signed Issuer is created for either.

#### Turning cert-manager back off is not a plain upgrade

Turning it **on** later needs no flags: everything `--set global.certmanager.enabled=auto` (or
`true`) undoes — `insecureSkipTLSVerify` on the visibility APIServices, Kueue's self-managed Secret
— Helm wrote and knows how to remove.

The other way unwinds what **cert-manager** wrote, which Helm never recorded, and fails twice:

- `Secret "kueue-webhook-server-cert" ... cannot be imported into the current release: invalid
  ownership metadata`. Kueue's chart templates that Secret when self-managing, cert-manager creates
  it otherwise: same name, no Helm ownership. Checked **before** any hook runs, and deleting it by
  hand fails too — the live `Certificate` is reissued within seconds.
- Past that, `spec.insecureSkipTLSVerify: Invalid value: true: may not be true if caBundle is
  present` on both `visibility.kueue.x-k8s.io` APIServices: self-management sets that field while
  the live object carries cainjector's CA bundle. **Not atomic** — the release lands in `failed`
  with the visibility API `FailedDiscoveryCheck`.

The sequence that completes cleanly, verified on a cluster:

```bash
# 1. Drop the two APIServices carrying cert-manager's CA bundle. The release recreates them.
kubectl delete apiservice v1beta1.visibility.kueue.x-k8s.io v1beta2.visibility.kueue.x-k8s.io

# 2. --take-ownership lets Helm adopt the Secret cert-manager owns (Helm 3.21+).
helm upgrade gpustack-operator <chart> --namespace gpustack-system --reuse-values \
  --set global.certmanager.enabled=false --take-ownership --wait
```

It also recovers the release if you have already hit the second failure.

**The same happens if cert-manager is uninstalled** while `global.certmanager.enabled` is `auto`:
the answer flips with nobody editing a value, and the next upgrade — even an image bump — fails
identically. Where cert-manager comes and goes, state `"true"` or `"false"` rather than `auto`.

### `helm uninstall` now takes Kueue with it

Kueue used to be its own release and **outlived** an operator uninstall. The release now owns Kueue
and its CRDs, and a deleted CRD takes every custom resource of that kind — so
`helm uninstall gpustack-operator` deletes **every ClusterQueue, LocalQueue, ResourceFlavor,
AdmissionCheck and Workload in the cluster**, not only the operator's.

If something else depends on Kueue, keep it and disable the subcharts — `--set kueue.enabled=false`,
whether or not your Kueue came from Helm ([the not-installed-by-Helm
case](#if-kueue-or-nfd-was-not-installed-by-helm) needs extra steps). The chart prints the uninstall
notes at install time, so you need not remember this.

### A handful of old certificate Secrets are left behind

Older workers cached certificates in Secrets named by `generateName: gpustack-cert-`, one per
restart. The cache is now a fixed `gpustack-cert-<hash>` derived from content, so the churn stops
and the randomly-named ones go unread — inert, never Helm-owned, nothing removes them:

```bash
# Inspect first — these are Secrets, and a hand-created one could match.
kubectl -n gpustack-system get secret | grep '^gpustack-cert-'
```

The live one has a 16-character hex hash; the others carry Kubernetes' 5-character random suffix —
delete them at your leisure, or leave them.

## Do not roll back with `helm rollback`

**`helm rollback` is destructive here, more so than the upgrade was.** It deletes every resource the
current revision contains and the target does not — and the target, predating the subchart layout,
contains no Kueue. So the Kueue objects just adopted go, **including Kueue's CRDs** (shipped
as templates, not under `crds/`), and with them every ClusterQueue, LocalQueue, ResourceFlavor,
AdmissionCheck and Workload. The legacy release records are gone, so nothing hands them back.

To return to the split-release layout, do it deliberately: uninstall and reinstall the version you
want — [Migrating from v0.5.x](./from-v0.5.md)'s Path A, unchanged.

So snapshot the chain first — the only thing that makes a mistake recoverable:

```bash
kubectl get clusterqueues,resourceflavors,admissionchecks,instancetypes,localqueues -A -o yaml \
  > gpustack-chain.yaml
```

Running Pods are never touched. A bad rollback costs the chain's own objects; the worker
re-materializes most from the nodes on its next reconcile, but not `InstanceType`s an administrator
authored or edited by hand, nor any queued (not yet admitted) `Workload`.

## Verify

```bash
NS=gpustack-system

# One release, and the four runtime ones are gone.
helm list -n "$NS"

# The worker is up on the new version.
kubectl -n "$NS" rollout status deploy/gpustack-operator-worker

# Kueue, NFD and the CSI drivers now belong to the operator release.
kubectl -n "$NS" get deploy,daemonset \
  -l app.kubernetes.io/instance=gpustack-operator -o name

# Nothing still carries a legacy release's instance label (expect empty for both).
LEGACY='app.kubernetes.io/instance in (gpustack-kueue,gpustack-node-feature-discovery,gpustack-csi-driver-nfs,gpustack-csi-driver-s3)'
kubectl get deploy,daemonset,svc,sa,cm,role,rolebinding -n "$NS" -o name -l "$LEGACY"
kubectl get clusterrole,clusterrolebinding,csidriver,storageclass -o name -l "$LEGACY"

# The scheduling chain survived: same counts as before the upgrade.
kubectl get clusterqueues,resourceflavors,instancetypes
kubectl get admissioncheck                    # expect: gpustack-node-devices
kubectl get workloads -A                       # admitted workloads still admitted

# NFD node labels survived.
kubectl get nodes -o json | grep -c 'feature.gpustack.ai/'

# No Kueue CRD is wedged Terminating (DELETING is <none> for every row).
kubectl get crd -o custom-columns=NAME:.metadata.name,DELETING:.metadata.deletionTimestamp | grep kueue
```

---

**See also** — [Installation Modes](../architecture/installation-modes.md) (what the two modes own after the
transfer) · [High Availability Operations](../operation/high-availability.md) (the replica knobs now live in one
`values.yaml`) · [Migrating from v0.5.x](from-v0.5.md)

**Next** → [Settings](../settings.md) — the configuration surface the transfer unifies.
