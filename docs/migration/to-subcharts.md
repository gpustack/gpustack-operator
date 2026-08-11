# Migrating an existing install to the bundled subcharts

> **Purpose** — the one-time ownership transfer that folds the runtime-installed Kueue / NFD / CSI
> releases into the operator release, and the four things it changes permanently.
> **Audience** operators on a v0.7.x-or-earlier install · **Prerequisites** [Two install
> modes](../architecture/install-modes.md) · **Read time** ~15 min

Up to and including v0.7.x, the operator chart deployed only the worker and the device managers.
Kueue, Node Feature Discovery and the two CSI drivers were installed by the **worker at runtime**,
each as a Helm release of its own:

| Runtime release | Now |
|---|---|
| `gpustack-kueue` | the `kueue` subchart of the operator release |
| `gpustack-node-feature-discovery` | the `node-feature-discovery` subchart |
| `gpustack-csi-driver-nfs` | the `csi-driver-nfs` subchart |
| `gpustack-csi-driver-s3` | the `csi-driver-s3` subchart |

From this version they are **subcharts of the operator release**, configured in the same
`values.yaml` as everything else, and the worker installs nothing at runtime by default
(`worker.disableApplications: ["*"]`).

Nothing in the cluster is torn down to get there. The objects those four releases created are
**adopted** by the operator release, in place, by Helm's own ownership transfer. Your ClusterQueues,
Workloads, ResourceFlavors, node labels and mounted volumes all survive.

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

`--take-ownership` is what makes the adoption legal: without it Helm refuses every object that
carries another release's ownership metadata. It is needed **once**. Every later upgrade is an
ordinary `helm upgrade` with no flag — and you should drop the flag again, because it is blunt: it
adopts *any* live object whose name the render resolves, including one you created by hand.

The upgrade must be reachable by Helm 3.21 or newer, which is the version this migration was
validated with and the one `hack/lib/helm.sh` pins. An older client has no `--take-ownership`.

### Keep the release name and the namespace

Use the release name **`gpustack-operator`** and the namespace **`gpustack-system`** — the same ones
the install instructions use. Both are load-bearing here:

- The **release name** is the base of `gpustack-operator-worker` and
  `gpustack-operator-device-manager-<manufacturer>`, which the docs, the chart's `files/cleanup.sh`
  fallback and the e2e suites all assume. The four subcharts are unaffected — each pins its own names
  through `fullnameOverride`, so their objects are adopted whatever the release is called — but a
  differently-named release renames the worker and its device managers, which is a rollout, not an
  adoption.
- The **namespace** is where your current install already is, and an upgrade cannot move a release
  between namespaces. Nothing in the chart requires this particular one any more: Kueue's
  `managedJobsNamespaceSelector` — which keeps Kueue from managing the operator's own Jobs,
  including the migration hooks below — is rendered from the release namespace, so a fresh install
  can go anywhere. Only state that selector yourself if you want a different rule, and remember
  Kueue refuses to start when it matches the namespace Kueue runs in.

## What runs during the upgrade

Two hook Jobs run the operator image (it bundles `helm`, `kubectl`, `jq` and the packaged chart).
Both are idempotent, and both report having had nothing to do on a healthy cluster.

**Before** the upgrade (`pre-upgrade`, and `pre-install` too):

1. **Reap a stranded Kueue.** A Kueue controller torn down while its custom resources still held the
   `kueue.x-k8s.io/resource-in-use` finalizer leaves its CRDs `Terminating` forever, and then every
   install of this chart fails on them. The Job deletes the Kueue webhook configurations first —
   their `failurePolicy: Fail` would otherwise reject the finalizer-clearing patch — then strips the
   finalizers and waits for the CRDs to drain. This also runs on a fresh install, because a first
   install onto a cluster left in that state has no other way forward.
2. **Apply the subcharts' `crds/`.** Helm applies `crds/` on install only; an upgrade never touches
   them. Without this, NFD's CRD schema changes would never land and a newly enabled NFD would have
   no CRD for the chart's `NodeFeatureRule`.

**After** it (`post-upgrade`, upgrades only):

3. **Retire the four legacy release records** (`kubectl delete secret -l owner=helm,name=<release>`)
   — never `helm uninstall`, which would delete the very objects the operator release just adopted.
   Left in place, `helm list` would keep reporting releases that point at the parent's objects, and a
   later `helm uninstall gpustack-kueue` would destroy them.
4. **Prune what the legacy releases created and the new render does not contain.** The adoption
   rewrites `app.kubernetes.io/instance` on every object the new render resolves, so anything still
   carrying a legacy release's instance label afterwards is exactly what the new render never
   mentions — unowned from then on, and invisible to `helm uninstall`. CRDs are excluded (deleting
   one takes every custom resource with it), as are PersistentVolumes and PersistentVolumeClaims.

`helm upgrade --no-hooks` skips both, if you would rather do this by hand.

Verify afterwards that `helm list` shows one release, not five:

```bash
helm list -n gpustack-system     # expect: gpustack-operator only
```

## Image mode migrates itself

When the worker installs the bundled chart from inside its own image, it detects one of its own
legacy releases and sets the ownership transfer for that install only — never unconditionally, for
the same reason you should drop the flag again after the chart-mode upgrade. There is nothing to run.

## If Kueue or NFD was not installed by Helm

Everything above assumes you are coming from Helm releases. Kueue and NFD are also commonly installed
from raw manifests, by kustomize, or by another operator that bundles them — and nothing adopts those.
The detection in image mode looks for a Helm release record (a Secret labelled `owner=helm,name=…`), so
an install that left none is never detected and the ownership transfer is never set. In chart mode
`--take-ownership` would get past the error below, but the error is not the problem worth solving.

- **Kueue collides, on its CRDs.** This chart templates them, so they carry ownership metadata, and
  their names come from the API group rather than from any release — `workloads.kueue.x-k8s.io` is
  `workloads.kueue.x-k8s.io` however it was installed. Helm refuses an object carrying *no* ownership
  metadata as firmly as one carrying another release's: `invalid ownership metadata`, this time for a
  missing `app.kubernetes.io/managed-by: Helm` label and a missing `meta.helm.sh/release-name`
  annotation.
- **NFD's CRDs do not collide.** They ship in the subchart's `crds/` directory, which Helm applies on
  install, skips when the object already exists, and never records — so they carry no ownership
  metadata and are never ownership-checked.
- **The namespaced objects not colliding is the real hazard.** A manifest-installed Kueue lives in
  `kueue-system` while this chart puts its own in the release namespace. Different namespace, no
  conflict, no error — so an install forced through leaves two Kueue controllers running, both watching
  the whole cluster and both reconciling the same Workloads.

So pick a starting point rather than forcing it through.

**Keep what you have** — the right answer whenever something else in the cluster depends on that Kueue:

```bash
helm upgrade --install gpustack-operator gpustack/gpustack-operator \
  --namespace gpustack-system --create-namespace \
  --set kueue.enabled=false --set node-feature-discovery.enabled=false
```

Both switches are supported paths, not workarounds — and NFD off still leaves you the
`gpustack-cpu-info` `NodeFeatureRule` the scheduling chain starts from, because the worker applies it
unconditionally against whichever NFD is present. What you take on is keeping both components at
versions the operator works with, since the chart no longer states their configuration for you.

**Or hand them over.** Delete the non-Helm install's workloads, Services, RBAC and — importantly — its
webhook configurations, whose `failurePolicy: Fail` would otherwise reject every Workload write once
their Service is gone. Leave the CRDs alone: deleting one deletes every custom resource of that kind,
which is what you are trying to keep. Then give those CRDs the ownership Helm looks for:

```bash
for crd in $(kubectl get crd -o name | grep 'kueue\.x-k8s\.io'); do
  kubectl label    "$crd" app.kubernetes.io/managed-by=Helm --overwrite
  kubectl annotate "$crd" meta.helm.sh/release-name=gpustack-operator \
                          meta.helm.sh/release-namespace=gpustack-system --overwrite
done
```

and install normally. That is the same adoption `--take-ownership` performs, narrowed to the objects
that need it. Read [Do not roll back](#do-not-roll-back-with-helm-rollback) first: from here on the
release owns those CRDs, so `helm uninstall` and `helm rollback` take every ClusterQueue, Workload and
ResourceFlavor with them.

## Four things that change permanently

### `manufacturers` moved to `global.manufacturers`, and each entry became a row

The map that used to be a top-level `manufacturers: {nvidia: "10de", ...}` is now
`global.manufacturers`, and each entry carries a manufacturer's whole identity rather than only its
PCI vendor ID:

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

It sits under `global` because the bundled Kueue subchart reads it too, and `global` is the only
channel Helm gives a subchart to the parent's values. If you never set `manufacturers`, nothing is
required of you — the defaults moved with it. If you did, translate your override into a row under
`global.manufacturers`, and drop the old key — nothing reads a top-level `manufacturers` any more,
so one left behind is ignored and its device-managers fall back to the default vendor IDs.

The chart now creates **`amd`, `mthreads` and `nvidia`** — `amd` is new in this release, not a
consequence of the subchart migration — and still only where the class is absent or already belongs to
this release. A row's `runtimeName` says which class the operator will *use* for that manufacturer, and
most rows state one; creating a class is gated on the narrower `runtimeInjectsDriver` or
`runtimeInjectsDevices` instead, because those are the cases where the container runtime is certain to
be installed. `deviceManager.createRuntimeClasses=false` remains the way to opt out.

### `worker.certmanager` moved to `global.certmanager`, and now answers for Kueue too

The block that used to sit under `worker` is `global.certmanager`, with the same keys and the same
`auto` default:

```yaml
global:
  certmanager:
    enabled: auto
    issuer:
      name: ""
      kind: Issuer
```

It moved for the reason `manufacturers` did: the bundled Kueue reads it, and `global` is the only
channel Helm gives a subchart. That closes a split the old layout could not. The worker decided
cert-manager for itself while Kueue decided separately — and always said no, its upstream default
being off — so a cluster with cert-manager installed ran two components' certificates two different
ways, and the CRD detection behind `auto` never reached Kueue at all.

**On a cluster that has cert-manager, this upgrade therefore moves Kueue onto it.** Kueue stops
generating and rotating its own webhook certificate and starts consuming a `Certificate` this chart
creates, with cert-manager's cainjector filling in the CA bundles its webhooks and CRD conversion
carry. Decide before you upgrade: pass `--set global.certmanager.enabled=false` in the same command
to keep every component self-managing instead — that is one answer for the whole release, so it
takes the worker with it. A `worker.certmanager` left in your values is ignored.

Naming an existing issuer (`global.certmanager.issuer.name`) now points Kueue's certificates at it
too, instead of only the worker's, and no self-signed Issuer is created for either.

#### Turning cert-manager back off is not a plain upgrade

Turning it **on** later is: `helm upgrade --set global.certmanager.enabled=auto` (or `true`) needs
no flags, because everything it undoes — the `insecureSkipTLSVerify` on the visibility APIServices,
Kueue's self-managed Secret — is something Helm itself wrote and therefore knows how to remove.

Going the other way has to unwind what **cert-manager** wrote, which Helm never recorded, and it
fails in two places:

- `Secret "kueue-webhook-server-cert" ... cannot be imported into the current release: invalid
  ownership metadata`. Kueue's chart templates that Secret when it self-manages and cert-manager
  creates it when it does not — same name, no Helm ownership. This one is checked **before** any
  hook runs, so no amount of chart machinery can clear it, and deleting the Secret by hand does not
  help either: the `Certificate` is still live, so cert-manager reissues it within seconds.
- Past that, `spec.insecureSkipTLSVerify: Invalid value: true: may not be true if caBundle is
  present` on both `visibility.kueue.x-k8s.io` APIServices. Self-management sets that field while
  the live object still carries the CA bundle cainjector wrote. **This failure is not atomic** —
  the release lands in `failed` with the visibility API `FailedDiscoveryCheck`.

The sequence that completes cleanly, verified on a cluster:

```bash
# 1. Drop the two APIServices carrying cert-manager's CA bundle. The release recreates them.
kubectl delete apiservice v1beta1.visibility.kueue.x-k8s.io v1beta2.visibility.kueue.x-k8s.io

# 2. --take-ownership lets Helm adopt the Secret cert-manager owns (Helm 3.21+).
helm upgrade gpustack-operator <chart> --namespace gpustack-system --reuse-values \
  --set global.certmanager.enabled=false --take-ownership --wait
```

If you have already hit the second failure, that same sequence recovers the release.

**This is also what happens if cert-manager is uninstalled from the cluster** while
`global.certmanager.enabled` is `auto`: the answer flips with nobody editing a value, and the next
upgrade — even one that only bumps the image — fails the same way. On a cluster where cert-manager
comes and goes, state the answer (`"true"` or `"false"`) instead of leaving it to `auto`.

### `helm uninstall` now takes Kueue with it

This is the significant behavioural change, and it is worth understanding before you upgrade.

Kueue used to be a release of its own, so it **outlived** an operator uninstall. Now the operator
release owns Kueue and its CRDs, and deleting a CRD deletes every custom resource of that kind — so
`helm uninstall gpustack-operator` deletes **every ClusterQueue, LocalQueue, ResourceFlavor,
AdmissionCheck and Workload in the cluster**, not only the ones the operator created.

If something else in your cluster depends on Kueue, install with `kueue.enabled=false` and bring
your own. The same applies to `node-feature-discovery.enabled=false`, which is supported and still
gives you the `gpustack-cpu-info` NodeFeatureRule the scheduling chain starts from — the rule is
rendered unconditionally, against whichever NFD is present.

The uninstall notes are printed by the chart at install time, so you do not have to remember this.

### A handful of old certificate Secrets are left behind

Older workers cached their generated certificates in Secrets created with
`generateName: gpustack-cert-`, so each restart added another one. The cache is now a fixed,
content-derived name (`gpustack-cert-<hash>`), which is what stops the churn — but it also means the
randomly-named ones are no longer looked up by anything. They are inert, and Helm never owned them,
so nothing removes them for you:

```bash
# Inspect first — these are Secrets, and a hand-created one could match.
kubectl -n gpustack-system get secret | grep '^gpustack-cert-'
```

The one the worker still uses is the one whose name is a 16-character hex hash; the others carry
Kubernetes' 5-character random suffix. Delete those at your leisure, or leave them.

## Do not roll back with `helm rollback`

**`helm rollback` is destructive here, and more destructive than the upgrade was.** A rollback deletes
every resource that the current revision contains and the target revision does not — and the target
revision, being from before the subchart layout, contains no Kueue at all. So it would delete the
Kueue objects the release just adopted, **including Kueue's CRDs**, which Kueue ships as templates
rather than under `crds/`. Deleting a CRD deletes every custom resource of that kind: your
ClusterQueues, LocalQueues, ResourceFlavors, AdmissionChecks and Workloads go with it. The legacy
release records are gone too, so nothing hands those objects back.

If you need to return to the split-release layout, do it deliberately: uninstall and reinstall the
version you want — [Migrating from v0.5.x](./from-v0.5.md)'s Path A applies unchanged.

So before you upgrade, snapshot the chain. It costs one command and it is the only thing that makes a
mistake recoverable:

```bash
kubectl get clusterqueues,resourceflavors,admissionchecks,instancetypes,localqueues -A -o yaml \
  > gpustack-chain.yaml
```

Running Pods are not at risk either way — they are never touched by any of this. What a bad rollback
costs you is the scheduling chain's own objects, and the worker re-materializes most of them from the
nodes on its next reconcile. The ones it cannot re-derive are the `InstanceType`s an administrator
authored or edited by hand, and any queued (not yet admitted) `Workload`.

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

**See also** — [Two install modes](../architecture/install-modes.md) (what the two modes own after the
transfer) · [High Availability](../operation/high-availability.md) (the replica knobs now live in one
`values.yaml`) · [Migrating from v0.5.x](from-v0.5.md)

**Next** → [Settings](../settings.md) — the configuration surface the transfer unifies.
