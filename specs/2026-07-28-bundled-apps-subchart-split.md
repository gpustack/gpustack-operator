# Spec: Ship Kueue / NFD / CSI as Helm Subcharts and Make the Control Plane HA

Status: Building
Type: Feature

## Summary

Move the four applications the worker installs at runtime — Kueue, Node Feature Discovery,
csi-driver-nfs and csi-driver-s3 — out of the Go-embedded Helm-install path in
`pkg/worker/kuberess` and into declared subcharts of `deploy/gpustack-operator/chart`. The
upstream charts are vendored **unpacked and patched** under `chart/charts/` by `make deps`,
mirroring how `staging/` vendors patched Kubernetes modules, so the chart's `global.*` image
knobs keep fanning out to every component.

The operator keeps **two delivery modes over one set of chart defaults**: a chart-driven install
(`helm install`, the worker installs nothing), and a self-contained image-driven install where
the worker deploys the operator chart bundled in its own image with a computed overlay. The
Go-side values templates disappear either way.

The move is taken together with a version refresh: Kueue to 0.18.4 and Node Feature Discovery
to **upstream** 0.19.0, dropping the
`thxCode/node-feature-discovery` fork by relocating the one capability it added — rendering the
`NodeFeatureRule` — into the operator chart itself. With every upstream value reachable from
`values.yaml`, this delivers
[issue #52](https://github.com/gpustack/gpustack-operator/issues/52): configurable replicas,
leader election, topology spread and PodDisruptionBudgets for `gpustack-operator-worker` and
`kueue-controller-manager` — and, at no extra cost, for the NFD master/gc and the CSI
controllers. Existing installs migrate in place through Helm's own `--take-ownership`, so no
cluster has to tear down its Kueue CRDs to upgrade.

## Motivation

### Goals

- **Deliver issue #52.** `gpustack-operator-worker` and `kueue-controller-manager` both run
  with an operator-chosen replica count, coordinate a single active leader, spread across
  nodes, and are protected by a PodDisruptionBudget. Verified by an e2e case that kills the
  leader and watches a standby take over. Defaults stay at **one replica with PDBs off** — HA
  is opt-in, so a single-node cluster is unaffected.
- **Make every bundled component configurable without touching Go.** Today `replicas`,
  `podDisruptionBudget` and `topologySpreadConstraints` are supported by the upstream Kueue and
  NFD charts but unreachable, because their values are frozen in Go string templates inside the
  operator binary. After this change they are ordinary `values.yaml` keys, schema-validated and
  documented by `make generate chart`.
- **One set of chart defaults, two delivery modes.** The chart-driven install and the
  self-contained image-driven install must both keep working, and neither may grow its own
  copy of the application configuration. The runtime installer collapses from four
  app-specific installers with four values templates into a single installer that deploys the
  bundled operator chart. Image mode consumes the chart's **defaults plus a computed overlay**;
  it deliberately has no user-values channel (see Non-Goals), so per-value tuning such as
  `kueue.controllerManager.replicas` is a chart-mode capability.
- **Refresh the pinned versions and drop the NFD fork.** Kueue chart 0.18.4, whose one CRD
  field no cluster below Kubernetes 1.31 accepts is patched behind a version guard so a single
  vendored tree serves every supported cluster; NFD 0.19.0 straight from
  `kubernetes-sigs/node-feature-discovery`. The only chart capability the fork added — a
  `NodeFeatureRule` template — moves into the operator chart, so the fork stops being a
  dependency.
- **Keep the airgap/mirror story intact.** `global.imageRegistry` / `global.imageNamespace` /
  `global.imagePullSecrets` still fan out to every workload, including subchart workloads and
  subchart hook Jobs.
- **Preserve upgrade continuity.** An existing v0.7.x install upgrades in place with no loss of
  ClusterQueues, Workloads or node labels.

Success is measurable as: `pkg/worker/kuberess` no longer contains a Kueue/NFD/CSI values
template; both install modes render the same objects from the same chart defaults; `--set
worker.replicas=3 --set kueue.controllerManager.replicas=3` yields a working, leader-elected,
node-spread control plane; and the v0.7.x → new-chart upgrade e2e case passes.

### Non-Goals

- HA for the GPUStack server (the Python application) — out of this repository's scope.
- A user-values channel for image mode (`-f values.yaml` or its equivalent). Image mode stays
  defaults-plus-overlay; anyone who needs to tune a value uses chart mode.
- Any change to the scheduling-chain semantics, the `pkg/nodefeature` label algebra, or the
  credit/quota model. Rendered manifests must be behaviour-equivalent to today's, modulo the
  version bumps above.
- Version bumps of the CSI drivers (csi-driver-nfs 4.13.2 and csi-driver-s3 0.43.7 carry over
  unchanged).
- Patching Kueue's visibility server out of the chart — see the caveat under
  "Notes / Constraints / Caveats".
- Maintaining upstream chart forks for anything beyond the `global.*` image plumbing, the
  `csi-driver-s3` rename and the Kueue `selectableFields` version guard.
- A non-Helm (pure `kubectl apply`) install path.
- Multi-cluster / MultiKueue, and any change to the device-manager DaemonSets (already per-node).

## Proposal

The operator chart becomes the single source of deployment configuration. It declares four
dependencies (Kueue 0.18.4, NFD 0.19.0, csi-driver-nfs 4.13.2, csi-driver-s3 0.43.7), each
vendored unpacked into `chart/charts/` and carrying a small patch
that teaches its image fields to honour the parent's `global.*` values. The parent
`values.yaml` carries today's opinionated defaults, so a default install produces the same
cluster state as today — but every one of those settings is now a value a user can override.

That chart is consumed two ways. In **chart mode** the user runs `helm install`; the chart
renders the worker, the device-managers, the `NodeFeatureRule` and every enabled subchart, and
starts the worker with `--disable-applications=*`. In **image mode** the operator image is run
directly with no chart driving it; `Prepare()` installs the operator chart bundled inside that
image as a single release with `worker.enabled=false` and each component enabled according to
`--disable-applications`.

Because a Helm subchart's objects belong to the **parent** release by construction, Kueue, NFD
and the CSI drivers stop having their own `gpustack-*` releases. Migrating an existing cluster
is therefore an ownership transfer, and Helm has a native mechanism for exactly that:
`--take-ownership`. The chart ships hooks only for the two things that mechanism does not
cover — reaping a stranded Kueue and applying subchart CRDs on upgrade — plus a post-upgrade
sweep that retires the stale legacy release records and prunes objects the new render no longer
contains.

The `gpustack-cpu-info` `NodeFeatureRule` becomes a first-class template of the operator chart
rather than a fork-only NFD chart feature, gated by its own switch that is **independent of
whether the NFD subchart is enabled** — an operator running their own NFD still needs the rule
for the scheduling chain to start.

On top of that surface, the chart exposes a coherent HA story: replicas, PDB and topology
spread for the worker, and every knob its own chart supports for Kueue, the NFD master and the two
CSI controllers — all declared in `values.yaml` at their single-replica defaults, so the surface is
discoverable where a user already edits, with the walkthrough in the docs rather than in a recipe
file to copy.

### User Stories

#### Story 1
As a cluster administrator running GPUStack on a multi-node control plane, I want to set
`worker.replicas=3` and `kueue.controllerManager.replicas=3` in one values file passed to
`helm install`, so that losing a control-plane node does not stop workload admission.

#### Story 2
As a platform operator, I want to tune Kueue's resources, tolerations, feature gates, probe
timings and PodDisruptionBudget from the operator chart's values, so that I do not have to fork
the operator or rebuild its image to change a Deployment setting.

#### Story 3
As someone bootstrapping GPUStack from the operator image alone — no Helm chart in hand — I want
the operator to install its whole application stack itself at its default configuration, so
that the image stays a self-contained installer.

#### Story 4
As an operator installing into an airgapped environment behind a private mirror, I want
`global.imageRegistry` and `global.imageNamespace` to still rewrite every image — operator,
device-manager, Kueue, NFD, both CSI drivers, and every hook Job — so that one knob remains
enough.

#### Story 5
As a maintainer bumping Kueue or NFD, I want to edit one version in `hack/deps.sh` and run
`make deps`, so that the bump is a vendoring change rather than an edit to a Go string template
plus a Dockerfile `ADD` line plus a comment cross-reference.

#### Story 6
As an existing v0.7.x user, I want one documented `helm upgrade --take-ownership` to keep my
ClusterQueues, Workloads and node labels, so that adopting this refactor is not a cluster
rebuild.

#### Story 7
As an operator whose cluster already runs its own Node Feature Discovery, I want to set
`node-feature-discovery.enabled=false` and still get the `gpustack-cpu-info` NodeFeatureRule, so
that the scheduling chain starts against my existing NFD instead of requiring a second one.

### Core Features & Acceptance Criteria

**F1 — Vendored, patched subcharts (`make deps`)**
- A new vendoring function in `hack/lib/helm.sh` mirrors `mod_staging`: pull each chart, unpack
  it into `deploy/gpustack-operator/chart/charts/<dir>/`, stamp `_VERSION_`, then apply
  `hack/deploy/gpustack-operator/chart/charts/<dir>/*.patch` with `patch -p1 -N --forward`. It
  replaces the three `helm dependency update` call sites (`gpustack::helm::deps`,
  `::lint`, `::test`), which would otherwise clobber the patched trees.
- Pinned sources:

  | dir | version | source |
  | --- | --- | --- |
  | `kueue` | 0.18.4 | `github.com/kubernetes-sigs/kueue` release `kueue-0.18.4.tgz` |
  | `node-feature-discovery` | 0.19.0 | `kubernetes-sigs/node-feature-discovery` release `node-feature-discovery-chart-0.19.0.tgz` (note the `-chart-` infix) |
  | `csi-driver-nfs` | 4.13.2 | `kubernetes-csi/csi-driver-nfs` charts tree |
  | `csi-driver-s3` | 0.43.7 | `thxcode.github.io/k8s-csi-s3/charts` (tarball and `metadata.name` are both `csi-s3`; renamed by patch) |

- `Chart.yaml` declares all four as `dependencies` with `repository: ""` — the Helm convention
  for "already present in `charts/`" — plus `condition`. Declaring them is mandatory:
  `condition` only applies to declared dependencies, and an undeclared tree under `charts/`
  always renders.
- **A `condition` alone cannot default a dependency off.** Helm resets every declared dependency
  to `enabled = true` before evaluating conditions, and a `condition` whose values path is
  *absent* leaves it enabled. So `enabled: false` in `Chart.yaml` is inert, and the only place the
  switches exist is the parent `values.yaml`. Verified by rendering: with the four declared and no
  parent keys, a default template emits all 111 subchart objects; with four `enabled: false`
  parent keys it emits none.
- The `csi-driver-s3` tree's upstream `metadata.name` is **`csi-s3`**, so declaring
  `name: csi-driver-s3` fails with "found in Chart.yaml, but missing in charts/ directory". A
  one-line `chart-rename.patch` renames it, which keeps the directory, the chart name, the values
  key and T4's patch directory all spelled the same. The `alias:` alternative was rejected on
  evidence: an alias whose directory name differs from the chart name breaks
  `helm dependency build`, i.e. `make lint chart`.
- AC: running `make deps` twice leaves `git status --porcelain` empty.
- AC: the unpacked, patched charts are committed (≈2.7 MB). `chart/.gitignore` keeps ignoring
  `charts/*.tgz` and `Chart.lock`, and the unpacked directories are tracked.
- AC: `helm template deploy/gpustack-operator/chart` and `helm lint` both succeed from a fresh
  clone with the network disabled (`unshare -n` or equivalent).
- AC: `make lint chart` / `make test chart` leave `git status --porcelain` empty and leave no
  `*.tgz` under `charts/` — i.e. `ct` does not run its own dependency build over the vendored
  trees.
- AC: bumping a pinned version requires editing only the version list.
- AC: `.gitattributes` marks `hack/deploy/**/*.patch` as `-text` so patch context stays
  byte-exact.
- AC: `pack/gpustack-operator/Dockerfile` drops the four upstream chart `ADD` lines; the
  packaged `gpustack-operator-<ver>.tgz` carries `charts/`, which is what makes image mode
  self-contained.
- AC (operational prerequisite): `docker.io/gpustack/mirrored-kueue:v0.18.4` and
  `docker.io/gpustack/mirrored-node-feature-discovery:v0.19.0` are mirrored via a manual
  `sync-image.yml` dispatch **before** the chart change merges; the e2e install proves both
  pull.

**F2 — Unified configuration surface**
- Parent `values.yaml` gains `kueue:`, `node-feature-discovery:`, `csi-driver-nfs:` and
  `csi-driver-s3:` blocks holding today's defaults. `csi-driver-nfs` and
  `csi-driver-s3` default to `enabled: true`, matching today's behaviour.
- Each vendored chart carries a `global-image.patch` resolving image references through
  `global.imageRegistry` / `global.imageNamespace` / `global.imagePullPolicy` /
  `global.imagePullSecrets`, each falling back to the chart's own value when the global is empty
  — mirroring the parent's existing `gpustack-operator.image` helper. NFD 0.19.0 already honours
  `global.imagePullSecrets` natively, so only the other three apply to it. **Hook Job images are
  in scope**, notably NFD's `templates/post-delete-job.yaml`.
- **`global.imagePullPolicy` is a new key** on the parent's `global:` block, alongside the
  `imageRegistry` / `imageNamespace` / `imagePullSecrets` / `nodeSelector` already there. It
  exists because today's Go path sets `pullPolicy` on **all five** applications from the
  `ImagePullPolicy` setting (confirmed against the T2 baseline), so without a global an image-mode
  user setting `GPUSTACK_IMAGE_PULL_POLICY=Always` would silently stop reaching Kueue, NFD and the
  CSI drivers. The parent's own `image.pullPolicy` is not under `global:` and can never reach a
  subchart.
- **`global.imageRegistry` replaces an existing registry segment rather than prefixing it.** A
  first path segment containing `.` or `:` is treated as a registry and dropped before the
  override is applied. This is what the parent's own doc comment already promises ("Image registry
  **override**… when empty, the registry encoded in `image.repository` is used as-is"); its
  implementation only looks correct today because `gpustack/gpustack-operator` carries no
  registry. The subchart repositories do carry one (`docker.io/gpustack/mirrored-kueue`), so
  prefixing would render `reg.local/docker.io/gpustack/…` whenever `imageRegistry` is set without
  `imageNamespace`. The parent helper takes the same change for one semantic behind one knob; it
  is a behavioural no-op there, provable by the default render staying byte-identical.
- The five charts expose **25** image sites, four of which the criteria below do not name because
  they sit behind default-off gates: kueueViz backend and frontend (in **both** Kueue trees), NFD's
  topology-updater, and csi-driver-nfs's `snapshot-controller` Deployment (distinct from the
  `csi-snapshotter` sidecar). They are patched too, and the verify script flips those gates on —
  a patch nothing renders is not verified. Two shape quirks matter: csi-driver-nfs resolves
  `image.baseRepo` plus a leading-slash repository suffix, and csi-driver-s3 stores full
  `registry/ns/name:tag` strings with no separate tag key.
- **The parent exposes a subchart key only where it deliberately differs from that chart's own
  default.** A key that merely repeats upstream is noise, and a component an upstream chart ships
  switched off is one the operator does not deploy — so the parent neither exposes nor opens it.
  That removes `enableKueueViz`, `enablePrometheus`, `enableVisibilityAPF`,
  `enableCertManager` and `enableVisibilityAuthReaderRoleBinding` from the Kueue block, leaving it
  `enabled` + `fullnameOverride` + the settings that genuinely differ. It also keeps the verify
  render free of component switches: it asserts the defaults, which is what a user installs.
- **`enableVisibilityAuthReaderRoleBinding` is one of those, and it matters why.** An earlier
  draft pinned it `false` to keep the release from writing into `kube-system`. That is not
  hardening, it is an outage: the Kueue manager starts its visibility server unconditionally, and
  without the binding it cannot read `kube-system/extension-apiserver-authentication`, so startup
  aborts. Verified live — the controller crash-looped six times with the binding absent and went
  Ready within a minute of it being created. Upstream's default is `true`, so the correct action
  is for the parent to say nothing about it. The one RoleBinding outside the release namespace is
  the price of a running Kueue, and it is what ships today too.
- AC: `helm template --set global.imageRegistry=reg.local --set global.imageNamespace=mirror`
  renders **every** image under `reg.local/mirror/…` — worker, device-managers, Kueue manager,
  NFD master/worker/gc, NFD's post-delete hook, both CSI plugin + sidecar sets, and the chart's
  own hook Jobs. The AC is a script that extracts every `image:` value and asserts the prefix,
  so a missed field fails rather than being eyeballed.
- AC: `--set global.imagePullSecrets[0].name=ps` puts `ps` on every pod spec.
- AC: a golden-manifest test asserts parity with the captured baseline for image refs,
  tolerations, resources, priority classes, driver names, `fullnameOverride`, the NFD config
  and the Kueue `managerConfig`. CRDs are **excluded** from the comparison — `helm template`
  does not emit subchart `crds/`.
- AC: the rendered output contains no RoleBinding in the `kube-system` namespace.
- AC: `make generate chart` regenerates `README.md` and `values.schema.json`; the CI
  "Verify Generated" step passes.

**F3 — Go-derived values generated, one source of truth**
- The Kueue `managerConfig` `resources.transformations` block and the PCI vendor/class matchers
  that feed the operator chart's own `NodeFeatureRule` (F5) are emitted into `values.yaml` by a
  generator reading `pkg/nodefeature` (`CreditsPerCard`, `SharedResourceMaxSize`,
  `ResourceMaxUnits`, `GetAcceleratable*ResourceName`, `GetAcceleratablePciVendorIDs`,
  `GetPciVendorID`), between explicit begin/end markers.
- The generator runs from `generate_chart` in `hack/generate.sh`, ahead of helm-docs and
  helm-schema, so `make generate chart` is the single regeneration entry point.
- AC: changing `nodefeature.CreditsPerCard` and running `make generate chart` updates
  `values.yaml`; CI fails when the result is uncommitted.
- AC: a unit test asserts the committed block equals what `pkg/nodefeature` computes.
- AC: the generated transformations cover the chart's full `manufacturers` map; removing a
  manufacturer from values leaves inert transformations, which is documented as harmless.

**F4 — Two install modes, one set of chart defaults**

*Chart mode (default).* `helm install` renders the worker, the device-managers, the
`NodeFeatureRule` and every enabled subchart. The worker Deployment passes
`--disable-applications={{ join "," .Values.worker.disableApplications }}`, defaulting to `["*"]`,
so `pkg/worker/kuberess` performs **no application install at all**. The value is templated
rather than hard-coded because `--disable-applications` is a `pflag.StringSliceVar`: repeated
occurrences **accumulate**, so an entry appended via `worker.extraArgs` could only ever disable
more, never re-enable. `worker.disableApplications` is the only way to express a hybrid.

`--disable-applications` governs application installs only. The worker's own bootstrap stays
unconditional in both modes: the system namespace, the operator's own CRDs, the aggregated
APIServices, the admission webhook configurations, settings initialisation, and the
`gpustack-node-devices` AdmissionCheck. That last one is today nested inside `installKueue` and
must be lifted out — left under the disable gate it would be skipped by `*` in chart mode, the
`NodeQueueReconciler` would never see an Active check, and the scheduling chain would break.

*Image mode (self-contained).* The operator image is run with no chart driving it. `Prepare()`
installs `${GPUSTACK_CONF_DIR}/charts/gpustack-operator-<ver>.tgz` as a single release, keeping
today's release name `gpustack-operator-device-manager` unchanged, with a computed overlay:
`worker.enabled=false`; each component enabled iff it is absent from `--disable-applications`;
`global.imageRegistry` / `imageNamespace` / `imagePullSecrets` / `imagePullPolicy` from the
worker's settings; `manufacturers` from `--manufacturer`; and the running worker's own image
reused for the device-managers (today's `extractWorkerImage`, unchanged). The pull policy must go
onto `global.imagePullPolicy`, not the parent's `image.pullPolicy` — the latter never reaches a
subchart, so setting only it would silently drop `GPUSTACK_IMAGE_PULL_POLICY` for Kueue, NFD and
the CSI drivers, which today's Go path does honour.

- The four app installers collapse into one. `installKueue`,
  `installNodeFeatureDiscovery`, `installCSIDriverNFS`, `installCSIDriverS3` and their values
  templates and template func-maps are deleted; `installGPUStackDeviceManager` generalises into
  the single bundled-chart installer.
- `reapOrphanedKueue` and its helpers move to the chart's `pre-upgrade` hook script (F8), so
  they outlive the installer collapse and are deleted with T15, not T12 — until the hook
  exists, image mode would otherwise lose its self-heal. The reaper's webhook-configuration
  selector must narrow with the move: `app.kubernetes.io/instance=gpustack-kueue` no longer
  matches a Kueue the parent release owns. It becomes an `instance in (…)` set over the
  releases this operator installs Kueue under. Selecting on the chart-name label instead would
  also match a Kueue a user brought themselves, and the reaper deletes what it selects.
- The `gpustack-node-devices` AdmissionCheck apply stays a runtime step in Go — Kueue ships its
  CRDs in `templates/crd/`, not `crds/`, so Helm cannot reliably create a Kueue CR in the same
  pass that creates its CRD — and moves out of `installKueue` into `Prepare()`'s unconditional
  sequence. It needs **new** retry-until-established behaviour:
  `kubeappyaml.ApplyWithRestClientGetter` hard-fails today when the CRD's RESTMapping is absent
  and nothing retries; it only works now because of the ordering inside `installKueue`.
- `--disable-applications` accepts exactly `*`, `kueue`, `node-feature-discovery`,
  `csi-driver-nfs`, `csi-driver-s3`, `device-manager`; an unknown name is rejected at flag-parse
  time with the valid list. There is deliberately no name for the `NodeFeatureRule` — F5 makes
  it unconditional.
- AC: no Kueue/NFD/CSI chart-values template remains in `pkg/worker/kuberess`.
- AC: `CSIProvisionerNFS` / `CSIProvisionerS3` driver-name constants stay in Go (other packages
  consume them) and are asserted to match the chart's `driver.name` values by a test.
- AC (chart mode): a default `helm install` creates exactly one Helm release; the worker
  installs no application at runtime, yet the `gpustack-node-devices` AdmissionCheck still
  reaches `Active` and the accelerated ClusterQueue references it.
- AC (image mode): a bare `gpustack-operator worker` against an empty cluster brings up NFD,
  Kueue, both CSI drivers, the device-managers and the `NodeFeatureRule`, and the scheduling
  chain materializes.
- AC (image mode): `--disable-applications=csi-driver-nfs,csi-driver-s3` leaves those two
  subcharts off in the installed release and everything else up.
- AC (parity): a test renders chart mode at defaults and image mode's computed overlay and
  diffs them; the two agree apart from the worker Deployment and release-name labels. The test
  is explicitly a *defaults* parity check — it cannot detect a value the overlay fails to
  forward, because image mode forwards none by design.
- AC (image mode): no Kubernetes-version branch is needed for Kueue — the one vendored tree
  installs on any supported cluster (F6), so the overlay carries no version-dependent switch.
- AC: on worker restart with changed computed values, the release is upgraded in place (the
  existing `nextStep` DeepEqual path), not reinstalled.
- AC (behaviour change): in chart mode, `deviceManager.enabled=false` no longer causes a
  runtime install — it means no device-managers. Recovering the old hybrid requires
  `worker.disableApplications`, documented in `docs/architecture.md` and the migration doc.

**F5 — `NodeFeatureRule` owned by the operator chart**
- The `gpustack-cpu-info` rule (CPU-identity annotations, the `has-acceleratable-devices`
  detection rule, and the negative `feature.gpustack.ai/acceleratable=false` marker) is rendered
  by `chart/templates/nodefeaturerule.yaml`, replacing the `nodeFeatureRule` /
  `nodeFeatureRules` values that only the `thxCode` NFD fork understood.
- The rule carries **no switch at all** and is **independent of
  `node-feature-discovery.enabled`**: the scheduling chain starts at this rule, so a release
  without it classifies no node and nothing downstream ever materializes — there is no
  configuration in which omitting it is correct. An operator running their own NFD still gets
  it. The consequence is deliberate: on a cluster with no NFD API whatsoever the install fails
  on the missing CRD instead of silently deploying a chain that can never start.
- Neither matcher list is stated twice. The PCI class prefixes are read from the NFD subchart's
  own `worker.config.sources.pci.deviceClassWhitelist`; the vendor IDs are the values of
  `manufacturers`, which already maps every managed manufacturer to its PCI vendor ID for the
  device-managers and the worker's environment. So the rule matches exactly the devices NFD was
  told to label, for exactly the vendors this release manages, and a manufacturer added to
  `manufacturers` is detected with no second edit.
- AC: `helm template --set node-feature-discovery.enabled=false` still renders the
  `NodeFeatureRule`, and no value can remove it.
- AC: the rendered vendor list equals the sorted PCI vendor IDs of `manufacturers`.
- AC: `helm install` with the NFD subchart enabled succeeds in one pass — Helm applies `crds/`
  before building capabilities and rendering templates, so the CR's CRD exists.
- AC: `helm upgrade` that newly enables the NFD subchart also succeeds — Helm never applies
  `crds/` on upgrade, so the F8 pre-upgrade hook must have applied the vendored CRDs first.
- AC: the rendered rule is semantically identical to what `nfdChartValuesTemplate` produces
  today (asserted against the captured baseline).
- AC: the NFD subchart carries **no** template patch — only `global-image.patch` — so an NFD
  bump is a re-vendor with nothing to re-align.
- **Two deliberate divergences from the captured NFD baseline**, so the parity diff is expected
  rather than a failure:
  1. **`fullnameOverride: node-feature-discovery`** — without it the subchart renders
     release-prefixed names that do not collide with the runtime install's, and the two NFD
     deployments then run side by side with no error (observed live). The collision is the point:
     it is what `--take-ownership` turns into an adoption.
  2. **`master.config.nodeFeatureNamespaceSelector` is dropped, not reproduced.** Today's Go
     template pins it to the install namespace, which means NFD master ignores every
     `NodeFeature` created elsewhere. That defeats the takeover this spec is built on: a cluster
     already running NVIDIA gpu-operator has its NFD — and its `NodeFeature` objects — in the
     `gpu-operator` namespace, and adopting that NFD while keeping the selector would silently
     discard exactly the features being adopted. Upstream 0.19.0 ships the key commented out
     (unset means all namespaces), so dropping it also satisfies the "only diverge deliberately"
     rule in F2.
     - Trust-surface note: unset, combined with the `allowOverwrite: true` /
       `denyNodeFeatureLabels: false` this chart already carries, means a `NodeFeature` in any
       namespace can influence node labels. That is inherent to taking over cluster-wide NFD; it
       is recorded here rather than mitigated, and `denyNodeFeatureLabels` remains the lever if a
       deployment needs to tighten it.

**F6 — One Kueue tree across every supported Kubernetes version**
- A single `kueue` dependency (chart 0.18.4) declared with `condition: kueue.enabled` and
  `fullnameOverride: kueue`, so the subchart and the runtime install render identical object
  names and either can be adopted into the other.
- **Why a second vendored tree was considered, and why it is not needed.** The only thing about
  chart 0.18.4 that a cluster below Kubernetes 1.31 rejects is the `selectableFields` its
  `Workload` CRD declares. That is **two lines in one file**,
  `templates/crd/kueue.x-k8s.io_workloads.yaml`, and Kueue ships its CRDs under `templates/`
  rather than `crds/`, so they are rendered — and therefore conditionalable. An earlier draft
  pinned a whole second tree (chart 0.17.8, `kueue-legacy`) to dodge those two lines.
- `selectable-fields.patch` wraps them in
  `{{- if semverCompare ">=1.31.0-0" .Capabilities.KubeVersion.Version }}`. The field is an API
  server indexing optimisation for the `status.admission.clusterQueue` field selector; the
  v0.18.4 controller runs fine without it, which the status quo already proves — today's code
  pairs the v0.18.2 image with the 0.17.6 chart, whose CRDs also lack the field.
- Measured, not assumed. On kind `kindest/node:v1.29.14` with
  `kubectl apply --server-side --force-conflicts --dry-run=server`:

  | render | objects accepted | failures |
  | --- | --- | --- |
  | chart 0.17.8 (the dropped legacy line) | 68 | 0 |
  | chart 0.18.4 without `selectableFields` | 68 | 0 |
  | chart 0.18.4 unmodified | 67 | 1 — `.spec.versions[1].selectableFields: field not declared in schema` |

  A direct apply of the unmodified CRD fails the same way
  (`strict decoding error: unknown field "spec.versions[1].selectableFields"`), so the field is
  **rejected, not pruned** — which is what makes the guard load-bearing rather than cosmetic.
- **Caveat on verifying it offline.** `helm template` with no `--kube-version` defaults
  `.Capabilities.KubeVersion` to the helm binary's own built-in (high) version, so a bare render
  still emits the field. Only a real install, or an explicit `--kube-version`, exercises the
  sub-1.31 branch — every render AC below therefore pins the version.
- AC: `helm template --kube-version 1.29.14 --set kueue.enabled=true` renders zero
  `selectableFields` lines; the same render at `--kube-version 1.33.12` renders one.
- AC: the two renders differ **only** in those two lines, and both emit the same document count.
- AC: `make deps` over a removed `charts/kueue` re-applies `selectable-fields.patch` and
  reproduces the committed tree byte for byte.
- AC: the `ci-chart.yml` 1.23→1.35 kind matrix passes with no per-leg Kueue switch — one set of
  values installs everywhere.

**F7 — Control-plane HA (issue #52)**
- Worker: keep `worker.replicas` (default `1`); add
  `worker.podDisruptionBudget.{enabled,minAvailable}` (default disabled) and
  `worker.topologySpreadConstraints`. The pod anti-affinity stays **`preferred`** (a 2-node
  cluster must still be able to schedule 3 replicas) and becomes overridable via
  `worker.affinity`; node spread is therefore expressed by `topologySpreadConstraints` in the
  HA recipe, not by anti-affinity.
- Every HA knob is **declared in `values.yaml`** at its component's default, so a user finds the
  whole surface in the file they already edit and needs no recipe file to copy from. This is a
  deliberate exception to F2's surface rule — these keys restate a subchart default instead of
  differing from it — and it is priced: a subchart bump that changes one of these defaults is
  pinned to the old value until the parent block is re-aligned, which `chart_staging`'s drift
  check does not catch because the value is legal either way. Accepted because a knob a user
  cannot find is a knob that does not exist.
- Kueue: `kueue.controllerManager.{replicas,podDisruptionBudget.{enabled,minAvailable},
  topologySpreadConstraints}`. Its spread constraints render verbatim, so a `DoNotSchedule` spread
  needs its own `labelSelector` — stated in the key's own documentation.
- NFD master: `node-feature-discovery.master.{replicaCount,podDisruptionBudget.{enable,
  minAvailable}}` — NFD spells the switch `enable`, and above one replica its chart adds
  `-enable-leader-election` itself. NFD renders **no** topology spread constraints, so spread goes
  through `master.affinity`, which replaces NFD's own control-plane node preference rather than
  adding to it. The NFD **gc** is left alone: a stalled garbage collector only delays cleanup.
- The CSI controllers: `csi-driver-{nfs,s3}.controller.{replicas,strategyType}`. Both charts run
  their sidecars with `--leader-election`, so replicas do buy failover, but neither renders a PDB
  or spread constraints and both honour `controller.affinity` **only when it carries
  `nodeSelectorTerms`** — a pod anti-affinity is silently dropped, so replicas may share a node.
  `strategyType` is exposed because the charts default to `Recreate`, which takes every replica
  down on upgrade and gives back the failover `replicas` just bought. All three limitations are
  stated in the values file rather than patched around: a provisioner outage delays volume
  operations while mounted volumes keep working, since the mounting side is the node DaemonSet.
- Both CSI drivers also declare the placement surface of **both** their workloads —
  `controller.{name,dnsPolicy,affinity,nodeSelector,priorityClassName,tolerations}` and the same
  for `node` — at each chart's own defaults. The node DaemonSet has no replicas or budget to
  configure, but it is the mounting side, so which nodes it runs on and with what DNS policy is
  exactly what an operator needs to reach; leaving those keys undeclared while documenting
  `controller.affinity` in prose would have been the worst of both. Two asymmetries are stated
  where they are: the S3 chart renders no `dnsPolicy` for its controller, so no key exists for
  it, and `node.affinity` is rendered as given while `controller.affinity` is honoured only when
  it carries `nodeSelectorTerms`.
- Leader election is already **on by default** for the worker (`pkg/manager/option.go`
  `KubeLeaderElection: true`, ID overridden to `worker.gpustack.ai` in `pkg/worker/option.go`);
  no flag is passed and none is added. Kueue's `managerConfig` already sets `leaderElect: true`.
  Both are asserted, not introduced.
- **No HA recipe file ships with the chart.** Every knob being declared in `values.yaml` makes a
  second file redundant: it would only restate keys the user can now see and read about in place,
  and it would drift from them. The HA walkthrough — which knobs, which values, how many nodes —
  lives in `docs/operation/high-availability.md` (T17), which `values.yaml` points at.
- Concurrent-start correctness. `Prepare()` runs before leader election and four of its steps
  genuinely race with N replicas. None needs a distributed lock; each needs a targeted fix:
  1. **CRD and webhook-configuration applies do not retry conflicts.** `pkg/kubeclientset`'s
     update path only retries on conflict when an `AlignFunc` is supplied; the CRD and webhook
     installers supply none, so a losing replica returns the conflict and `Prepare` hard-fails
     into CrashLoopBackOff. → supply align functions.
  1b. **The aggregated-APIService apply cannot retry at all.** `InstallServices` does supply an
     align function, but it goes through `kubeclientset.Create` + `WithUpdateIfExisted`, whose
     conflict retry is **unreachable dead code**: `operate.go:184` (and its
     `CreateWithCtrlClient` twin at `:311`) guard with
     `err == nil || !IsConflict(err) || !IsNotAcceptable(err) || !isRetryError(err)`, so the
     retry runs only for an error that is simultaneously 409 and 406 — mutually exclusive.
     Compare `Update`'s correct form at `:459`. The victims are the two aggregated APIServices
     the whole extension API depends on. → fix the grouping at both sites, with a regression
     test, since a one-character-class slip is exactly what a later edit reverts by accident.
  2. **The certificate cache deletes its peers' Secrets.** Cached certs are created with
     `GenerateName: "gpustack-cert-"`, so N replicas create N Secrets, and the lookup path
     deletes *all* duplicates it finds and returns nothing — mutual deletion and cert churn on
     every boot. → key the cache on a fixed-name create-or-update Secret.
  3. **The image-mode Helm install is a get-then-act with no cross-process lock.** → treat an
     already-`deployed` release at the expected values as converged; the existing `nextStep`
     DeepEqual path covers the rest.
  Settings-Secret note churn (a fresh UUID per boot) is benign and left alone.
- AC (e2e, 3-node kind): an HA values file written by the test with `worker.replicas=3` and
  `kueue.controllerManager.replicas=3` → all six pods Ready; exactly one `worker.gpustack.ai`
  Lease holder and one `c1f6bfd2.kueue.x-k8s.io` Lease holder; spread asserted through the
  topology-spread constraint, not through `preferred` anti-affinity.
- AC (e2e): deleting the current worker leader pod → a standby acquires the Lease within 60 s
  and the NFD → Worker → Kueue chain still materializes; same for the Kueue leader.
- AC (e2e, image mode): three worker instances booting concurrently produce exactly one
  `gpustack-operator-device-manager` release in a `deployed` state, no replica CrashLoops, and
  exactly one `gpustack-cert-*` Secret survives. The one-Secret assertion is scoped to a
  **fresh** install: switching from `GenerateName` to a fixed name leaves an upgraded cluster's
  pre-existing `gpustack-cert-<generated>` Secrets in place, unindexed and never read or written
  again. They are deliberately not pruned — a prune would put deletion logic back into the file
  this fix removes it from — so the residual is recorded in the migration doc and swept by
  `cleanup.sh`'s prefix match at uninstall.
- AC: `kubectl get pdb` shows `ALLOWED DISRUPTIONS ≥ 1` for both when enabled.
- AC: defaults stay `replicas: 1` with PDBs disabled, so single-node installs are unchanged.

**F8 — In-place upgrade migration**

Kueue, NFD and the CSI drivers stop having their own Helm releases: a subchart's objects belong
to the parent release by construction. Migrating an existing cluster is an ownership transfer,
and Helm performs it natively — `existingResourceConflict()` is replaced by `requireAdoption()`
when `TakeOwnership` is set, which adopts the matching live objects and rewrites their ownership
metadata as part of the apply. A hook cannot do this job: the ownership check runs *before*
pre-install/pre-upgrade hooks execute.

- **Chart mode** — one documented, one-time command:
  `helm upgrade gpustack-operator ./chart -n gpustack-system --take-ownership`. Subsequent
  upgrades are ordinary. The flag never appears in the default install instructions.
- **Image mode** — a new `TakeOwnership` field on `helm.Chart`, plumbed into the Install/Upgrade
  actions in `pkg/kubeapp/helm`. The bundled-chart installer sets it **only when it detects one
  of its own legacy releases** (`gpustack-kueue`, `gpustack-node-feature-discovery`,
  `gpustack-csi-driver-nfs`, `gpustack-csi-driver-s3`), never unconditionally — the flag is
  blunt and would otherwise silently adopt a user's hand-rolled Kueue.
- **`pre-install,pre-upgrade` hook Job**, running the operator image, which bundles `helm`,
  `kubectl`, `jq` and the packaged chart itself:
  1. Reap a stranded Kueue — list `*.kueue.x-k8s.io` CRDs carrying a `deletionTimestamp`; if any
     exist, delete the Kueue webhook configurations selected by
     `app.kubernetes.io/instance in (<release>,gpustack-operator-device-manager,gpustack-kueue)`
     **first** (their `failurePolicy: Fail` would otherwise reject the finalizer-clearing patch),
     strip finalizers from the Terminating CRs using each CRD's storage version, and wait up to
     90 s for them to drain. No-op on a healthy cluster. The drain poll asks for one CRD at a
     time so it stays a table lookup: re-listing every CRD every three seconds would re-download
     Kueue's schemas, megabytes a round.
  2. Server-side-apply the vendored subchart `crds/` (NFD's `nfd-api-crds.yaml`), with
     `--force-conflicts`, since Helm's client-side apply owns those fields until this hand-over.
     Helm applies `crds/` only on install; an upgrade never touches them, so without this step a
     newly enabled NFD subchart has no CRD for the parent's `NodeFeatureRule`, and NFD CRD schema
     changes never land. The files come from the packaged chart inside the image, because a parent
     chart's `.Files` **cannot** reach `charts/**` (verified: `.Files.Get` on a subchart path
     returns empty and `.Files.Glob "charts/**"` matches nothing), so a ConfigMap of CRD YAML
     rendered by the parent is not available. Skipped on install, where Helm has just applied them.
- **Why the reap also runs on install.** A first install onto a cluster whose previous Kueue was
  torn down mid-flight fails on its Terminating CRDs — forever, with no way forward — and this is
  the live failure the worker's Go reaper was written for. Deleting that reaper (T15) while gating
  the hook on upgrades would have lost the rescue, and in chart mode it never existed at all. The
  cost is one Job on every fresh install, which reports having had nothing to do; the alternative,
  rendering the Job only when a `lookup` finds a stranded CRD, buys nothing, since that lookup
  costs the same cluster-wide CRD read the Job itself performs.
- **`post-upgrade` hook Job** — runs only after the adoption succeeded:
  1. Delete the stale legacy release records (`kubectl delete secret -l owner=helm,name=<rel>`),
     never `helm uninstall`, which would delete the adopted resources. Otherwise `helm list`
     keeps showing releases that point at objects the parent now owns, and a later
     `helm uninstall gpustack-kueue` would destroy them.
  2. Prune objects that the legacy releases created but the new render does not contain.
     `--take-ownership` never visits these, so they would linger unowned. The rule needs no list
     of names: all four subcharts label their objects `app.kubernetes.io/instance:
     {{ .Release.Name }}`, and the adopting apply rewrites that label on everything the new
     render resolves — so an object still carrying a legacy release's instance label afterwards is
     exactly one the new render never mentions. `app.kubernetes.io/managed-by=Helm` is required
     alongside it, so a hand-labelled object is never swept. **CRDs are excluded** (deleting one
     takes every custom resource with it), as are PVs and PVCs, and namespaced kinds are swept
     inside the release namespace only. The visibility auth-reader RoleBinding is **not** such a
     case: both the old and the new render create it, and it lives in `kube-system`, which the
     sweep never enters.
- Hook plumbing: the Jobs honour `global.imagePullSecrets` and the resolved registry/namespace;
  the image is overridable through `migrate.image` so an upgrade is not blocked by an unmirrored
  new tag — at the cost that the CRDs the hook applies are then the ones vendored by the image it
  does run. Hook RBAC and the script ConfigMap carry a **distinct, lower** `hook-weight` than the
  Jobs (-11 vs -10) rather than relying on Helm's same-weight tie-break, which orders by name and
  would happily schedule a Job before the ServiceAccount it runs as. `helm upgrade --no-hooks` is
  the escape hatch, so no values switch is added for one.
- AC (e2e): install the last released chart on kind, then
  `helm upgrade --take-ownership` → the upgrade succeeds, no ClusterQueue / Workload /
  ResourceFlavor is lost, NFD node labels survive, and `helm list -A` afterwards shows exactly
  one release.
- AC: an equivalent upgrade **without** `--take-ownership` fails with `invalid ownership
  metadata` — proving the flag is what does the work and the hook never could.
- AC: the NFD 0.19.0 CRD schema is present after the upgrade (proving hook step 2 ran); this is
  explicitly **not** something Helm does for us.
- AC: re-running the upgrade is a no-op, and `helm list -A` still shows exactly one release.
- AC: on a fresh install the post-upgrade hook is not rendered at all, and the pre-install hook
  reports having had nothing to reap.
- AC: with the hook image set to a non-pullable tag, the upgrade aborts cleanly and the cluster
  is left in its pre-upgrade state.
- AC (documented constraint): the migration assumes the parent release is named
  `gpustack-operator`. Under any other release name the parent renders differently-prefixed
  object names, and the adopted objects become orphans that `helm uninstall` will not remove.
  The migration doc states the required release name, and the post-upgrade prune deletes
  adopted objects absent from the new render.

**F9 — Uninstall and cleanup**
- In chart mode, `helm uninstall gpustack-operator` now removes the Kueue/NFD/CSI workloads as
  part of the release. `files/cleanup.sh` keeps finalizer stripping, CRD draining and
  APIService/webhook removal; its per-release `helm uninstall` loop keeps targeting
  `gpustack-operator-device-manager` (now the image-mode release) plus a best-effort
  compatibility pass over the other pre-upgrade release names. Its APIService and webhook sweep
  matches the same `gpustack|kueue|nfd` name pattern as its CRD step, so Kueue's visibility
  APIService and `kueue-*` webhook configurations are in scope; and it clears the objects a
  failed F8 hook leaves behind, one of which is a binding to cluster-admin.
- The chart NOTES and README warn that uninstalling now deletes the Kueue CRDs — and therefore
  every ClusterQueue and Workload — unless the release was installed with `kueue.enabled=false`.
  This is a deliberate widening of the blast radius relative to today, where `gpustack-kueue`
  survived an operator uninstall.
- AC: chart-mode install → uninstall → cleanup leaves zero gpustack / kueue / nfd objects,
  including the `*.visibility.kueue.x-k8s.io` APIServices.
- AC: image-mode install → cleanup leaves the same zero state.
- AC: NFD's `post-delete` hook still fires on uninstall (its node labels are removed).

**F10 — Docs and e2e synchronised**
- `docs/architecture.md`: the two install modes and what each renders, where the
  `NodeFeatureRule` now comes from, the `--disable-applications` scope and accepted names, the
  `worker.disableApplications` value, and the `deviceManager.enabled=false` behaviour change.
- `docs/development.md`: `make deps` now vendors charts; how to add/refresh a chart patch; the
  mirrored-image prerequisite when bumping a pinned version.
- New `docs/operation/high-availability.md` (including the single-URL webhook topology
  restriction) and `docs/migration/to-subcharts.md` (the `--take-ownership` command, the
  required release name, and the widened uninstall blast radius).
- Chart `README.md` regenerated from `values.yaml` annotations.
- e2e: `_e2e-lib/scripts/assert-core.sh` replaces its sub-release loop with in-release workload
  assertions; `deploy.sh` header, `teardown.sh` and `cleanup.sh` updated; new HA,
  upgrade-adoption and image-mode cases added to `gpustack-operator-chart-e2e`; an observation
  step records the `*.visibility.kueue.x-k8s.io` APIServices' `Available` condition and the
  `kubectl api-resources` round-trip time (see the visibility caveat).
- AC: `make lint chart`, `make test chart`, `gpustack-operator-chart-e2e` and
  `gpustack-operator-e2e` all pass.

### Notes / Constraints / Caveats

- **Helm cannot template subchart values.** This is the constraint that shapes the whole
  design: a parent chart's `values.yaml` is merged, never rendered, so `global.imageRegistry`
  cannot be composed into a subchart's `image.repository` from the parent. The chosen escape is
  to vendor the charts unpacked and patch their templates to read `.Values.global.*` — Helm
  does propagate `global` into every subchart's values.
- **A subchart's objects belong to the parent release.** Verified by rendering: the Kueue
  subchart's Deployment carries `app.kubernetes.io/instance: <parent release>`. `gpustack-kueue`
  and the other per-app releases therefore disappear by construction, which is what makes the
  one-time ownership transfer necessary — and what widens the uninstall blast radius (F9).
- **Helm validates ownership before hooks run.** `existingResourceConflict()` is called at
  `install.go:354` / `upgrade.go:350`, while pre-install/pre-upgrade hooks execute at
  `install.go:448` / `upgrade.go:421`. Any migration that relies on a hook to fix ownership is
  ordered backwards. `--take-ownership` (`install.go:115`, `upgrade.go:121`) switches the check
  to `requireAdoption()` and is the sanctioned mechanism.
- **`requireAdoption()` does not inspect ownership metadata at all.** At
  `pkg/action/validate.go:41` it appends every live object that merely resolves by name,
  namespace and REST mapping — it cannot tell a former release's object from one a user created
  by hand. That is the precise sense in which the flag is blunt, and why image mode may only set
  it once a gpustack legacy release has been positively identified. On the install path the
  adopting apply is additionally routed through `UpdateThreeWayMerge` rather than a plain update
  (`install.go:459`), which is what gives a live object's unmanaged fields a chance to survive.
- **Helm never applies `crds/` on upgrade.** `installCRDs` is referenced only from the install
  path; `upgrade.go` has no CRD handling at all. Every subchart CRD update is therefore a
  manual/hook step forever, not just during this migration.
- **Vendoring follows the `staging/` precedent**: pull → unpack → `_VERSION_` stamp → apply
  `hack/<dest>/*.patch`, committed to the repository. This keeps `helm install` working from a
  bare clone and keeps CI offline-capable. Total cost ≈4.5 MB unpacked, ≈450 KB in the packaged
  tgz — the same tgz the image bundles, which is what keeps image mode self-contained.
- **`repository: ""` on a vendored dependency is valid** and renders correctly under Helm
  3.21 — verified by rendering a probe chart with two vendored Kueue trees.
- **Image mode reuses the release-convergence machinery already in `helm.Client`** — `nextStep`,
  `RepairViaUpgradeOnly`, the rollback of a wedged pending release. The values overlay and the
  new `TakeOwnership` field are the only additions.
- **The image-mode release keeps its existing name, `gpustack-operator-device-manager`**, even
  though its scope widens from the device-manager DaemonSets to the whole application set. The
  name is deliberately not renamed so no cluster needs a release migration.
- **Image mode has no user-values channel** and cannot express `kueue.controllerManager.replicas`
  or any other per-value override. The F4 parity test compares defaults against defaults and
  structurally cannot detect a value the overlay fails to forward — that is by design, not an
  oversight.
- **Multi-replica is unsupported in the single-URL webhook topology.** When
  `!LoopbackKubeInside && LoopbackKubeNearby`, the worker registers its webhooks against a
  single node IP URL (`pkg/worker/worker.go`, whose own comment reads "launch multiple
  instances, only one takes working"). Chart mode always runs in-cluster and is service-backed,
  so it is unaffected; image mode outside the cluster must stay at one replica.
- **A digest-pinned worker image degrades the device-manager image.** `splitImageReference`
  returns empty fields for a digest reference, so the chart composes its own default tag
  instead — a known failure mode that now rolls back the *whole* application release rather
  than just the device-managers, because the bundled-chart install is atomic.
- **Kueue visibility cannot be disabled at all.** The two aggregated APIServices
  (`v1beta1`/`v1beta2.visibility.kueue.x-k8s.io`), the `kueue-visibility-server` Service and the
  manager's `:8082` listener are **unconditional** in the 0.18.4 chart — only the auth-reader
  RoleBinding has a switch, and turning that off aborts manager startup rather than trimming the
  feature. So the chart ships the whole visibility surface, including one RoleBinding in
  `kube-system`, and this spec deliberately does not patch it out. The residual is recorded as a
  risk with an e2e observation step.
- **The Kueue subchart keeps `fullnameOverride: kueue`.** Its rendered object names then match
  what the runtime install produces, which is what makes the adoption path in F8 possible at all.
- **The NFD subchart needs the same treatment, and for a sharper reason.** Without a
  `fullnameOverride` its objects render release-prefixed
  (`gpustack-operator-node-feature-discovery-master`) while the runtime install renders
  `node-feature-discovery-master`. Those names do not collide, so enabling both produces **two
  live NFD masters and no error at all** — observed on a live cluster, where Kueue and both CSI
  drivers failed loudly on ownership metadata while NFD quietly doubled. Two masters contend over
  the same node labels, which is a known way to lose worker-contributed labels permanently. So the
  NFD block must pin `fullnameOverride: node-feature-discovery`: a collision that fails is the
  desired behaviour, because it is what `--take-ownership` then converts into an adoption.
- **NFD 0.19.0 is a straight upstream chart.** The `thxCode/node-feature-discovery` fork's chart
  delta (commit `5c898b2`) was: a `NodeFeatureRule` template, its value stubs, a Chart.yaml
  rebrand and a fork-local default image. Only the first is load-bearing, and it moves to the
  parent chart (F5). Every value the operator sets (`master.config.restrictions`,
  `worker.config.core.labelWhiteList`, `worker.config.sources.pci.*`, `*.enable`, `gc.*`,
  `fullnameOverride`) exists unchanged in 0.19.0, and the NFD API is still `v1alpha1`.
- **NFD 0.19.0 ships `values.schema.json`**, so Helm validates the subchart's values. Its
  top-level `additionalProperties` is unset (permissive), and F5 removes the need to add keys
  anyway. Its four new NetworkPolicy templates (master / worker / gc / topologyUpdater) all
  default to `enabled: false` and are inert; topologyUpdater itself stays disabled as today.
- **`enableCertManager` is pinned to `false`.** Kueue then manages its own webhook certificates
  and injects `caBundle` into its webhook configurations at runtime. Those fields are not in the
  rendered manifest, so an adopting apply can transiently clear them until Kueue's cert
  controller re-injects — during which its `failurePolicy: Fail` webhooks reject traffic. The
  upgrade e2e must observe this window.
- **`namespaceOverride` disappears** from the subchart values: as dependencies they render into
  the release namespace already.
- **The generators must be told to ignore the vendored trees, and `--no-dependencies` is not
  enough.** Once NFD is vendored, `helm-schema` **fails the build** — `error parsing comment of
  key pullPolicy: unclosed schema block`, exit 1 — because it walks the subcharts' own
  `values.yaml`. `--no-dependencies` does not prevent that walk; `--dependencies-filter=none`
  does, and leaves the parent's generated schema byte-identical. Separately, `helm-docs` writes a
  `README.md` into all five vendored trees unless given `--chart-to-generate=<parent>`. Both
  flags live in `hack/lib/helm.sh`. The consequence for values is unchanged: subchart values are
  not schema-validated by our generator, so a typo under `kueue:` is silently ignored by Helm —
  mitigated by golden-manifest tests over the keys the parent defaults.
- **`helm.Chart.SkippedCRDsInstallation` is a misnomer** — it sets Helm's `IncludeCRDs` (a
  render/manifest flag), not `SkipCRDs`, so NFD's `crds/` are installed today and will continue
  to be installed as a subchart. Behaviour is unchanged; the misnomer is out of scope.
- **`managedJobsNamespaceSelector` names `gpustack-system` literally**, and cannot do otherwise:
  Helm merges subchart values instead of rendering them, so the release namespace cannot reach
  that string — today's Go template interpolated it. Faithful to the baseline and correct for
  every install into `gpustack-system`, but an install into a different namespace would not
  exclude its own namespace from Kueue's managed-jobs selector, so Kueue would gate the
  operator's own workloads. This joins the required-release-name constraint in F8: the chart
  expects release `gpustack-operator` in namespace `gpustack-system`, and the docs must say so.
- **Block-scalar values keys need `# @default --` or helm-docs inlines them.** Without it the
  whole 200-line Kueue config string lands in one README table cell and the file goes from 11 KB
  to 33 KB. The same applies to the NFD and CSI blocks that follow.
- **A `null` subchart block re-enables a dependency.** `kueue: null` in a user values file
  deletes the block, and per Helm's dependency handling an absent condition path leaves the
  dependency *enabled*. Pathological input; recorded rather than coded around.
- **`mod_staging` carries the same two latent patch problems, deliberately unfixed.** Its
  `patch -p1 -N --forward --silent <"${patch_dir}"/*.patch` neither checks the exit code nor
  survives a second patch file (ambiguous redirect), and because it runs inside
  `pushd … && patch … && popd` a failure short-circuits the `&&` chain *silently* rather than
  aborting. Today every staging tree carries at most one patch, so neither has bitten. Fixing it
  is out of scope here — staging is not what this spec touches — but `chart_staging` next door now
  shows what the guarded form looks like.
- Go conventions from `CLAUDE.md` apply to the shrunken `kuberess` package and the new
  generator; shell code follows the existing `hack/lib/*.sh` style.

### Boundaries

- **Always:** keep both install modes working and rendering from the same chart defaults; keep
  default-values rendering behaviour-equivalent to today's Go-rendered manifests apart from the
  agreed version bumps; keep `make deps` idempotent; mirror a new upstream image tag before
  pinning it; regenerate `README.md` / `values.schema.json` via `make generate chart` and never
  hand-edit them; sign off every commit; run `make lint` after Go changes and `make lint chart`
  after chart changes.
- **Ask first:** dropping support for any Kubernetes minor currently in the `ci-chart.yml`
  matrix; changing a pinned upstream chart version beyond the ones agreed here; changing a
  default that alters an existing cluster's rendered manifest (replica counts, resources,
  tolerations, driver names).
- **Never:** reintroduce a second configuration surface for the bundled applications; set
  `TakeOwnership` unconditionally in image mode, or put `--take-ownership` in the default
  install instructions; run more than one worker replica in the single-URL webhook topology;
  make `helm upgrade` a destructive path for an existing install; delete or re-create Kueue CRDs
  as part of the migration; hand-edit the vendored chart trees in place (changes belong in a
  patch file); make the `NodeFeatureRule` conditional on the NFD subchart; raise any component's
  default replica count above 1; commit generated values that drift from `pkg/nodefeature`.

### Risks and Mitigations

- **A hook-based ownership rewrite can never run** — Helm checks ownership before hooks →
  migration uses `--take-ownership` / the `TakeOwnership` field; an e2e case asserts that the
  same upgrade *without* it fails, so the mechanism can never silently regress to the broken
  ordering.
- **`--take-ownership` is blunt** and will adopt any name-matching object, including a user's
  own Kueue → it is a documented one-time chart-mode command, never a default, and image mode
  sets it only when a gpustack legacy release is actually present.
- **Adopted objects orphan under a non-default release name** → the migration doc mandates the
  release name `gpustack-operator`, and the post-upgrade prune deletes adopted objects absent
  from the new render.
- **Kueue's runtime-injected `caBundle` can be cleared by the adopting apply**, breaking its
  `failurePolicy: Fail` webhooks until the cert controller re-injects → the upgrade e2e measures
  the window and asserts recovery; if it is not self-healing, pin `enableCertManager` or add a
  wait to the post-upgrade hook. Note the two paths are **not** symmetric: install-path adoption
  goes through `UpdateThreeWayMerge` (`install.go:459`), which gives an unmanaged live field a
  chance to survive, while upgrade-path adoption swaps only the validation function and keeps its
  normal apply. So the risk is the worse one on `helm upgrade` — the documented chart-mode
  migration — and an observation on one path does not carry over to the other. Image mode reaches
  the **install** action whenever `nextStep` returns install or reinstall, so it can hit either.
- **Subchart CRDs are never upgraded by Helm** → the pre-upgrade hook server-side-applies the
  vendored `crds/`; the F8 AC asserts the 0.19.0 schema is present afterwards.
- **Every conflict retry in `pkg/kubeclientset` is unbounded recursion with no backoff** — a
  pre-existing shape in `Update`, `UpdateStatus`, the `Patch` pair and their `*WithCtrlClient`
  twins, which `Create` now reaches too. Two realistic hot paths: a retry re-reads with
  `ResourceVersion: "0"` from the API server's watch cache, which can lag the write that caused
  the conflict, so a loser can spin on the same stale object with no sleep (`isRetryError` backs
  off only for 429/410/timeout); and if a mutating webhook rewrites the object on every update,
  two writers livelock. Because it recurses rather than loops, the failure is stack growth and a
  crash, not a returned error — reproduced during T7, where the old certs-cache `Put` blew the
  stack in seconds. → **not fixed here**: T7 keeps `Create` consistent with its siblings rather
  than inventing a bound at one call site. Converting all of them to a bounded loop with backoff
  is a contained follow-up task, recorded here so it is not lost.
- **Concurrent worker boots break four ways** (non-retrying CRD/webhook applies, an unreachable
  conflict retry on the aggregated-APIService apply, mutual deletion of `gpustack-cert-*`
  Secrets, the Helm get-then-act) → four targeted fixes in F7, each with a unit test, plus a
  three-replica e2e.
- **Chart 0.18.4's `selectableFields` is rejected outright by a pre-1.31 API server**, so one
  vendored tree could not serve every supported cluster → `selectable-fields.patch` puts the two
  offending lines behind a `.Capabilities.KubeVersion` guard, verified by rendering at both
  versions and applied server-side against a real 1.29 cluster (F6).
- **The two install modes drift apart** → the F4 defaults-parity test plus both modes exercised
  end to end in CI. The known, accepted limit is that image mode forwards no user values.
- **NFD 0.18.3 → 0.19.0 changes the label surface** the algebra depends on
  (`feature.node.kubernetes.io/pci-<vendor>.present`, `cpu.model.*`, the
  `^(pci-|cpu-model\.|acceleratable)` whitelist) → the scheduling-chain e2e is the gate; the
  value shapes were verified identical up front, so the residual risk is runtime behaviour.
- **Kueue's visibility APIServices are registered unconditionally**; a degraded aggregated
  APIService can slow or pollute cluster-wide discovery → e2e records their `Available` condition
  and the `kubectl api-resources` round-trip; if it degrades, a follow-up spec adds the gating
  patch. Note the auth-reader RoleBinding is not the lever here — removing it stops the manager,
  not the APIServices.
- **Missing mirrored image tags** would make every install `ImagePullBackOff`, and would also
  block the migration because the hook Job runs the new image → mirror-first is an F1 AC, and
  the hook image is overridable.
- **Uninstall becomes destructive** (Kueue CRDs now belong to the operator release) → NOTES +
  README warning, a documented `kueue.enabled=false` escape, and the migration doc.
- **Patch drift on an upstream bump** → `chart_staging` checks `patch`'s exit code **and** asserts
  no `*.rej`/`*.orig` remain, `_VERSION_` forces a re-stage, and CI runs `make deps` before
  asserting the tree is clean. The original wording — "`patch --forward` fails loudly" — was
  disproven: `--silent` prints no name, `.gitignore` hides the rejects, and a caller wrapping the
  call in `if ! …` suspends `errexit` for the whole callee. Both guards are covered by tests,
  including the reject branch, which BSD `patch` never reaches and which would otherwise have
  shipped unexercised. F5 keeps NFD down to a single
  patch to limit this.
- **Silent config regressions** (a value that used to be set by Go is dropped in translation) →
  golden-manifest parity against a baseline captured from `main` before the cut-over.
- **`preferred` anti-affinity makes a "three distinct nodes" assertion flaky** → the HA recipe
  expresses spread with `topologySpreadConstraints`, and the e2e asserts on that.
- **`ct` may run its own dependency build** over the vendored trees (unverified offline) → an F1
  AC asserts `make lint chart` / `make test chart` leave the tree clean and `charts/` free of
  `*.tgz`.
- **Chart CI slows down** with a 4.5 MB chart across a 7-way kind matrix → measure; if
  material, trim `README.md`/test fixtures from the vendored trees during vendoring, as
  `mod_staging` already trims `docs/` and `.github/`.

## Design Details

### Commands

Build, lint and test run **locally on darwin** — the whole module, including the CGO vendor
detectors, builds and tests there. Chart tests and every e2e case run against a **local 3-node
kind cluster** created for the run: nothing in this spec needs a real accelerator, and a local
cluster avoids both cloud cost and the flaky short-lived-token kubeconfig that turns timeouts
into false product failures.

Two environment caveats for whoever builds this: the working tree is a **git worktree**, so
`make generate` (the `api` task, which shells out to `go-to-protobuf`) will not run here — this
spec touches no API types, and `make generate chart` is unaffected. And `make package` from a
worktree fails in the image build's commit linter; build the image from the main checkout or a
fresh clone if an e2e run needs it.

```bash
make deps                 # vendor patched k8s modules AND the five upstream charts
make generate chart       # run the chart-values generator, then README.md + values.schema.json
make lint                 # golangci-lint over the module (allow a long timeout on a cold cache)
make lint chart           # ct lint against the vendored chart
make test                 # go test -race ./...
make test chart           # ct install onto the current cluster
make build                # cross build

# Render checks used by the acceptance criteria. Helm must be >=3.21 for `--take-ownership`, and
# `make deps` does NOT install it: `gpustack::helm::helm::validate` returns early whenever any
# `helm` is on PATH, so a system helm (3.13.3 here) silently shadows the pinned version. Fetch
# it into the gitignored `.sbin/` yourself and assert what you got with
# `.sbin/helm version --short`. Note `make lint chart` / `make test chart` run chart-testing in
# a container with its own bundled helm, which this pin does not govern.
.sbin/helm template gpustack-operator deploy/gpustack-operator/chart
# The Kueue CRD selectable fields are version-guarded, so pin --kube-version on both sides of
# the 1.31 boundary; without the flag helm substitutes its own built-in (high) version.
.sbin/helm template gpustack-operator deploy/gpustack-operator/chart --kube-version 1.29.14 \
  --set kueue.enabled=true | grep -c selectableFields   # 0
.sbin/helm template gpustack-operator deploy/gpustack-operator/chart --kube-version 1.33.12 \
  --set kueue.enabled=true | grep -c selectableFields   # 1
.sbin/helm template gpustack-operator deploy/gpustack-operator/chart \
  --set global.imageRegistry=reg.local --set global.imageNamespace=mirror
.sbin/helm template gpustack-operator deploy/gpustack-operator/chart \
  --set node-feature-discovery.enabled=false   # NodeFeatureRule must still render
unshare -n .sbin/helm lint deploy/gpustack-operator/chart   # offline guarantee (linux CI leg)

# Local 3-node kind cluster for chart tests and e2e. The 3-node config and its local render
# path are delivered by T14 — the committed `.github/configs/kind-config.yaml.tmpl` is 2-node
# and is only ever substituted inside the CI job.
kind create cluster --name gpustack-ha --config .github/configs/kind-config-ha.yaml

# Chart mode
helm install gpustack-operator deploy/gpustack-operator/chart -n gpustack-system \
  --create-namespace
# ... highly available (the knobs are all declared in values.yaml; see
# docs/operation/high-availability.md), passed as the user's own values file
helm install gpustack-operator deploy/gpustack-operator/chart -n gpustack-system \
  --create-namespace -f my-ha-values.yaml

# One-time migration from a pre-subchart release (chart mode)
helm upgrade gpustack-operator deploy/gpustack-operator/chart -n gpustack-system --take-ownership

# Image mode (worker installs the bundled chart itself)
gpustack-operator worker --manufacturer=nvidia
gpustack-operator worker --manufacturer=nvidia --disable-applications=csi-driver-nfs,csi-driver-s3
```

E2E: the `gpustack-operator-chart-e2e` skill (chart contract, install/upgrade/uninstall, image
mode) and the `gpustack-operator-e2e` skill (scheduling chain).

### Project Structure

```
deploy/gpustack-operator/chart/
├── Chart.yaml                     # + dependencies (repository: "") + conditions
├── values.yaml                    # + per-subchart blocks; HA knobs; generated derived values
├── values.schema.json             # generated
├── README.md                      # generated
├── files/
│   ├── cleanup.sh                 # post-delete: finalizers, CRDs, APIServices, webhooks
│   ├── migrate-pre.sh             # pre-upgrade: Kueue reap + subchart crds/ apply
│   └── migrate-post.sh            # post-upgrade: retire legacy release records + prune
├── templates/
│   ├── worker/                    # + poddisruptionbudget.yaml, topology spread
│   ├── device-manager/
│   ├── nodefeaturerule.yaml       # gpustack-cpu-info, independent of the NFD subchart
│   └── migrate/                   # hook Jobs + scoped RBAC (RBAC weight -11, Jobs -10)
└── charts/                        # vendored + patched, COMMITTED (~2.7 MB)
    ├── kueue/                     # 0.18.4, selectableFields version-guarded
    ├── node-feature-discovery/    # 0.19.0, upstream (fork dropped)
    ├── csi-driver-nfs/            # 4.13.2
    └── csi-driver-s3/             # 0.43.7

hack/
├── lib/helm.sh                    # vendoring fn replaces the `helm dependency update` sites;
│                                  # also carries the helm-docs/helm-schema subchart-ignore flags
├── deps.sh                        # calls it from mod()
└── deploy/gpustack-operator/chart/charts/<name>/*.patch   # mirrors hack/staging/<dest>/

gen/chartvalues/                   # emits the nodefeature-derived values blocks
testing/chart-baseline/            # values captured from `main` — the parity oracle

pkg/worker/kuberess/
├── apps.go                        # installs = [operator chart]; the disable-name table
├── apps_gpustack_operator.go      # the single bundled-chart installer (image mode)
├── apps_kueue_admission_check.go  # AdmissionCheck retry-apply, lifted out of installKueue
└── apps_kueue_reap.go             # the stranded-Kueue reaper, until T15's hook replaces it
```

Deleted: `apps_node_feature_discovery.go`, `apps_csi_driver_nfs.go`, `apps_csi_driver_s3.go`
and their tests. `apps_kueue.go` keeps only its reaper, renamed `apps_kueue_reap.go` to pair
with the test that was already named for it. `apps_gpustack_device_manager.go` is generalised
into `apps_gpustack_operator.go`.

### Code Style

The vendoring function mirrors the existing `mod_staging` in `hack/deps.sh` — same `_VERSION_`
guard, same `hack/<dest>` patch convention, same idempotency contract — and lives in
`hack/lib/helm.sh` so `deps.sh`, `generate.sh` and the lint/test helpers all share it:

```bash
function gpustack::helm::vendor() {
  local charts_dir="${ROOT_DIR}/deploy/gpustack-operator/chart/charts"
  mkdir -p "${charts_dir}"

  while read -r line; do
    IFS=' ' read -r url version dest <<<"${line}"
    local patch_dir="${ROOT_DIR}/hack/${dest#"${ROOT_DIR}/"}"
    local chart_version
    chart_version="$(cat "${dest}/_VERSION_" 2>/dev/null || echo "")"
    if [[ "${chart_version}" == "${version}" ]]; then
      gpustack::log::info "chart $(basename "${dest}") is up to date"
      continue
    fi
    rm -rf "${dest}" || true
    curl --retry 3 --retry-all-errors --retry-delay 3 -sSfL "${url}" -o /tmp/chart.tgz
    # Every upstream tarball has exactly one top-level directory, so strip it rather
    # than reading the name back through a `tar -tzf | head` pipeline, which would
    # trip `set -o pipefail`.
    mkdir -p "${dest}"
    tar -xzf /tmp/chart.tgz --directory "${dest}" --strip-components 1 --no-same-owner
    rm -f "${dest}/README.md"
    echo -n "${version}" >"${dest}/_VERSION_"
    # One `patch` call per file: a single `<"${patch_dir}"/*.patch` redirect is an
    # `ambiguous redirect` error the moment a tree carries a second patch.
    for p in "${patch_dir}"/*.patch; do
      [[ -f "${p}" ]] || continue
      gpustack::log::info "applying ${p}"
      patch -p1 -N --forward --silent --directory "${dest}" <"${p}"
    done
  done < <(
    cat <<EOF
https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.4/kueue-0.18.4.tgz 0.18.4 ${charts_dir}/kueue
https://github.com/kubernetes-sigs/node-feature-discovery/releases/download/v0.19.0/node-feature-discovery-chart-0.19.0.tgz 0.19.0 ${charts_dir}/node-feature-discovery
https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/refs/heads/master/charts/v4.13.2/csi-driver-nfs-4.13.2.tgz 4.13.2 ${charts_dir}/csi-driver-nfs
https://thxcode.github.io/k8s-csi-s3/charts/csi-s3-0.43.7.tgz 0.43.7 ${charts_dir}/csi-driver-s3
EOF
  )
}
```

Conventions: patches are minimal and single-purpose (one file per concern, named for what it
does — `global-image.patch`, `chart-rename.patch`); vendored trees are never edited in place;
the parent chart's own templates follow the existing `_helpers.tpl` include style; Go changes
follow `CLAUDE.md` (snake_case multi-word files, explicit error handling, exported symbols
documented).

### Implementation Plan

Six tasks are unblocked at the head and have disjoint `Owns:` — `/my-build <title> team` can
build them concurrently. `deploy/gpustack-operator/chart/values.yaml` is a single-writer file,
so T8 → T9 → T10 → T11 is a **file-ownership chain, not a logical dependency**; it is split into
three so each translation fits one context window and can be verified against its own slice of
the baseline.

- [x] **T1 · Mirror the upstream images**
      Blocked by: None
      Owns: `(none — operational)`
      Gate: review
      Acceptance: `docker.io/gpustack/mirrored-kueue:v0.18.4` and
      `docker.io/gpustack/mirrored-node-feature-discovery:v0.19.0` exist and are pullable for
      both `linux/amd64` and `linux/arm64`, mirrored via a manual `sync-image.yml` dispatch.
      Verify: `docker manifest inspect docker.io/gpustack/mirrored-kueue:v0.18.4 && docker manifest inspect docker.io/gpustack/mirrored-node-feature-discovery:v0.19.0`

- [x] **T2 · Capture the pre-change values baseline**
      Blocked by: None
      Owns: `pkg/worker/kuberess/baseline_dump_test.go`, `testing/chart-baseline/**`
      Gate: review
      Acceptance: a `-update`-guarded test renders today's Go values templates for kueue, nfd,
      csi-driver-nfs, csi-driver-s3 and the device-manager, writing one YAML per app under
      `testing/chart-baseline/`. Output is byte-stable across runs. This is the parity oracle
      for T8–T10 and must be captured while the Go templates still exist. Without `-update` the
      test **verifies** instead of writing, so it keeps guarding the oracle until T12 deletes it.
      Verify: `go test ./pkg/worker/kuberess/ -run TestDumpChartValuesBaseline -args -update`,
      then re-run it and diff `shasum testing/chart-baseline/*.yaml` across the two runs → identical.
      (The `git status --porcelain testing/chart-baseline` form only works post-commit: new files
      stay `??` while the builder holds them, and the builder may not stage.)

- [x] **T3 · Vendoring machinery + four subchart trees, all disabled**
      Blocked by: None
      Owns: `hack/lib/helm.sh`, `hack/deps.sh`, `.gitattributes`,
      `deploy/gpustack-operator/chart/Chart.yaml`, `deploy/gpustack-operator/chart/.gitignore`,
      `deploy/gpustack-operator/chart/values.yaml` (switch stanza only),
      `deploy/gpustack-operator/chart/README.md`,
      `deploy/gpustack-operator/chart/values.schema.json`,
      `deploy/gpustack-operator/chart/charts/**`,
      `hack/deploy/gpustack-operator/chart/charts/csi-driver-s3/chart-rename.patch`
      Gate: review
      Acceptance: `gpustack::helm::vendor` replaces two of the three `helm dependency update` call
      sites (`generate_chart` is T5's handoff, so `gpustack::helm::deps` stays defined until then);
      the four trees are vendored, patched and committed; `Chart.yaml` declares them with
      `repository: ""` and `condition`, and the parent `values.yaml` carries a **switch-only**
      stanza — four `enabled: false` keys plus `fullnameOverride: kueue` — because a `condition`
      alone cannot default a dependency off. The s3 tree's `metadata.name` is renamed from
      `csi-s3`. `helm-docs` gets `--chart-to-generate` and `helm-schema`
      `--dependencies-filter=none`, without which the vendored trees gain generated READMEs and
      `make generate chart` exits 1.
      Verify: `make deps` twice leaves the `charts/**` checksum and the untracked-file list
      identical (the `git status --porcelain | wc -l` → `0` form only holds after the trees are
      committed); a default render emits **zero** subchart objects; then
      `.sbin/helm template x deploy/gpustack-operator/chart --set kueue.enabled=true --kube-version 1.33.0 | grep -cE '^  name: kueue-controller-manager$'` → `1`
      (the loose `grep -c 'name: kueue-controller-manager'` reads `2` — it substring-matches
      `…-metrics-service`)

- [x] **T4 · `global-image` patches on the four trees**
      Blocked by: T3
      Owns: `hack/deploy/gpustack-operator/chart/charts/*/global-image.patch`,
      `deploy/gpustack-operator/chart/charts/**`, `hack/verify-chart-images.sh`,
      `deploy/gpustack-operator/chart/templates/_helpers.tpl`,
      `deploy/gpustack-operator/chart/values.yaml` (the `global.imagePullPolicy` key only),
      `deploy/gpustack-operator/chart/README.md`,
      `deploy/gpustack-operator/chart/values.schema.json`
      Gate: review
      Acceptance: all **22** image references in the four vendored trees — workloads, sidecars,
      hook Jobs including NFD's `post-delete-job`, and the three behind default-off gates
      (kueueViz, NFD topology-updater, csi-driver-nfs's `snapshot-controller`) —
      resolve through `global.imageRegistry` / `global.imageNamespace` /
      `global.imagePullPolicy` / `global.imagePullSecrets`, falling back to the chart's own value
      when empty. `global.imageRegistry` **replaces** an existing registry segment, and the parent
      helper takes the same change. The check script is committed, not run from `/tmp`: an
      assertion that never lands guards nothing. Patches only
      apply on a fresh unpack, so the edit loop is
      `rm -rf deploy/gpustack-operator/chart/charts/<tree> && make deps`. `csi-driver-s3` and
      `kueue` then carry **two** patches each, applied in name order — the case the original
      single-redirect skeleton could not handle.
      Verify: a script that renders with all subcharts enabled and
      `--set global.imageRegistry=reg.local --set global.imageNamespace=mirror`, extracts every
      `image:` value, and exits non-zero on any value not prefixed `reg.local/mirror/`

- [x] **T5 · `gen/chartvalues` generator**
      Blocked by: None logically, but **commits after T3**: switching `generate_chart` to
      `gpustack::helm::vendor` references a function T3 introduces, so landing T5 first would
      leave `make generate chart` calling an undefined function. The two can be *built*
      concurrently; only the commit order is constrained.
      Owns: `gen/chartvalues/**`, `hack/generate.sh`, `hack/lib/helm.sh` (deleting the orphaned
      `gpustack::helm::deps` only — T5's switch is what makes it dead, and T3's commit still
      needs it)
      Acceptance: a `go run`-able generator emits the Kueue `transformations` block and the NFD
      PCI vendor/class matcher block from `pkg/nodefeature`, between begin/end markers;
      `generate_chart` invokes it ahead of helm-docs and helm-schema and no longer calls
      `helm dependency update`. A golden test in `gen/chartvalues/testdata` pins the output.
      Verify: `go test ./gen/chartvalues/... && go run ./gen/chartvalues -stdout | head -40`

- [x] **T6 · `TakeOwnership` plumbing in `pkg/kubeapp/helm`**
      Blocked by: None
      Owns: `pkg/kubeapp/helm/chart.go`, `pkg/kubeapp/helm/client.go`,
      `pkg/kubeapp/helm/client_test.go`, `pkg/kubeapp/helm/chart_test.go`
      Gate: review
      Acceptance: a documented `TakeOwnership bool` field on `helm.Chart`, plumbed onto the
      Install and Upgrade actions. Default `false` — behaviour is unchanged unless set. The
      actions are only constructed past a cluster round-trip inside `InstallWith`, so the
      assignment is lifted onto two unexported `Chart.configure{Install,Upgrade}` seam methods
      (which also absorb today's `IncludeCRDs` line) to make "reaches both actions" assertable.
      Verify: `go test ./pkg/kubeapp/... && make lint`

- [x] **T7 · `Prepare()` concurrent-boot fixes**
      Blocked by: None
      Owns: `pkg/api/helper.go`, `pkg/api/helper_test.go`, `pkg/webhook/helper.go`,
      `pkg/webhook/helper_test.go`, `pkg/utils/certs/cache/kubernetes.go`,
      `pkg/utils/certs/cache/kubernetes_test.go`, `pkg/kubeclientset/operate.go`,
      `pkg/kubeclientset/operate_test.go`
      Gate: review
      Acceptance: CRD and webhook-configuration applies supply align functions so a conflict
      retries instead of erroring; the unreachable conflict retry on `kubeclientset.Create`'s
      `WithUpdateIfExisted` path is fixed at both sites, which is what unblocks the
      aggregated-APIService apply; the certificate cache uses a fixed-name create-or-update
      Secret instead of `GenerateName` + delete-all-duplicates, leaving an upgraded cluster's
      pre-existing Secrets unpruned. Both `pkg/kubeclientset` and `pkg/utils/certs/cache`
      currently have **zero** test coverage, so each fix ships with a fake-client unit test that
      drives the concurrent path — including tests on the two installers themselves, since a
      generic `pkg/kubeclientset` test cannot prove that *they* pass an align function.
      Verify: `go test -race ./pkg/api/... ./pkg/webhook/... ./pkg/utils/certs/... ./pkg/kubeclientset/... && make lint`

- [x] **T8 · Kueue values block + `selectableFields` version guard**
      Blocked by: T2, T4, T5
      Owns: `deploy/gpustack-operator/chart/values.yaml`,
      `hack/deploy/gpustack-operator/chart/charts/kueue/selectable-fields.patch`
      Gate: review
      Acceptance: the `kueue:` block reproduces the baseline's Kueue values, growing T3's switch
      stanza rather than replacing it — `fullnameOverride: kueue` is load-bearing for the
      immutable Deployment selector and must survive (managerConfig, generated transformations,
      tolerations, resources); keys that merely repeat an upstream default are not exposed —
      notably `enableVisibilityAuthReaderRoleBinding`, whose upstream `true` must not be
      overridden because turning it off aborts the Kueue manager's startup.
      `selectable-fields.patch` puts the `Workload` CRD's two `selectableFields` lines behind
      `semverCompare ">=1.31.0-0" .Capabilities.KubeVersion.Version`, which is what lets one
      vendored tree serve every supported cluster (F6).
      Verify: `.sbin/helm template x deploy/gpustack-operator/chart --set kueue.enabled=true --kube-version 1.33.0` diffed against the baseline slice; the same render at
      `--kube-version 1.29.14` differs **only** by the two `selectableFields` lines; and
      `rm -rf deploy/gpustack-operator/chart/charts/kueue && make deps` reproduces the committed
      tree byte for byte

- [x] **T9 · NFD values block + `NodeFeatureRule` template**
      Blocked by: T8
      Owns: `deploy/gpustack-operator/chart/values.yaml`,
      `deploy/gpustack-operator/chart/templates/nodefeaturerule.yaml`, and — as built —
      `gen/chartvalues/**` with its test and golden file, plus `pkg/worker/kuberess/apps.go`,
      `apps_gpustack_operator.go` and `chart_test.go`: the `nfd-pci-vendor-ids` generator block
      is **deleted**, because `manufacturers` already carries every vendor ID the rule needs and
      a generated second copy could only drift from it. What the generator guaranteed —
      chart defaults that match `pkg/nodefeature` — is now asserted by
      `TestChartManufacturersMatchNodeFeature` instead. The two Go files lose the
      `node-feature-rule` switch F5 retired, and with it the overlay's `nodeFeatureRule` block.
      Acceptance: the `node-feature-discovery:` block reproduces the baseline's NFD values apart
      from F5's two deliberate divergences — it adds `fullnameOverride: node-feature-discovery`
      and it does **not** carry `master.config.nodeFeatureNamespaceSelector`; the
      `NodeFeatureRule` carries no switch, renders even with the NFD subchart disabled, and
      takes its vendor IDs from `manufacturers`.
      The rule's PCI class matchers are **read from** the subchart's own
      `worker.config.sources.pci.deviceClassWhitelist` rather than restated: a rule can only
      match devices NFD was told to label, and a disabled subchart's values stay readable from
      the parent (verified), so the one list survives either switch. Applying F2's surface rule
      leaves the block at the keys that genuinely differ from NFD 0.19.0 — the mirrored image,
      the tolerate-everything tolerations, the managed-by annotations, the worker's
      source/label configuration, and the `restrictions` block, which restates NFD's own
      defaults on purpose because F5's trust-surface note makes them the levers.
      Verify: `.sbin/helm template x deploy/gpustack-operator/chart --set node-feature-discovery.enabled=false | grep -c 'kind: NodeFeatureRule'` → `1`; the enabled render diffed against the baseline slice

- [x] **T10 · CSI values blocks (nfs + s3)**
      Blocked by: T9
      Owns: `deploy/gpustack-operator/chart/values.yaml`,
      `pkg/worker/kuberess/chart_test.go`
      Acceptance: both `csi-driver-*` blocks reproduce the baseline, which under F2's surface
      rule is a much shorter list than this task assumed: the vendored trees already default to
      the baseline's tolerations, priority classes, resource names, health ports,
      `storageClass.create: false`, `secret.create: false` and `volumeSnapshotClass.create:
      false`, so only the gpustack driver names, the mirrored images, `rbac.name` and
      `nodeDriverRegistrar.livenessProbe.enabled: false` are stated. The NFS driver image stays
      pinned at v4.13.0, the tag this operator has always run and mirrored, rather than
      following the chart's 4.13.2 — a bump would need a new mirror and is a Non-Goal. Every other
      `csi-driver-nfs` sidecar tag is spelled out too, at the value chart 4.13.2 ships, so the tags
      this release pulls are all readable in one place and a subchart bump cannot move one without
      showing up in the diff — the same shape `csi-driver-s3` already has, where the chart stores
      whole `name:tag` references. `externalSnapshotter` stays absent: it is off, and turning it on
      needs a mirror first. A Go test
      asserts `CSIProvisionerNFS` / `CSIProvisionerS3` (now in `kuberess/alias.go`) equal the
      chart's `driver.name` values, and T12's deferred defaults-parity render lands here too,
      rendering both passes through Helm's own Go SDK so it needs no `helm` binary.
      Verify: rendered output diffed against the baseline slices; `go test ./pkg/worker/kuberess/ -run 'TestCSIDriverNamesMatchChart|TestChartDefaultsMatchImageModeOverlay'`

- [x] **T11 · HA knobs (worker + every subchart controller) + `worker.disableApplications`**
      Blocked by: T10
      Owns: `deploy/gpustack-operator/chart/values.yaml`,
      `deploy/gpustack-operator/chart/templates/worker/poddisruptionbudget.yaml`,
      `deploy/gpustack-operator/chart/templates/worker/deployment.yaml`
      Acceptance: the worker gets `podDisruptionBudget.{enabled,minAvailable}` (default off), its
      own PDB template, `topologySpreadConstraints` (an entry that omits `labelSelector` is given
      the worker's own, so no hand-written selector can be broken by a non-default release name)
      and an `affinity` override for the deliberately-`preferred` default anti-affinity. **Every
      subchart controller's HA knobs are declared in `values.yaml` too**, at each chart's own
      default, per F7's exception to the surface rule: `kueue.controllerManager.{replicas,
      podDisruptionBudget.{enabled,minAvailable},topologySpreadConstraints}`,
      `node-feature-discovery.master.{replicaCount,podDisruptionBudget.{enable,minAvailable}}`, and
      `csi-driver-{nfs,s3}.controller.{replicas,strategyType}`. Each key's documentation states what
      its chart cannot do — Kueue's spread needs a hand-written `labelSelector`, NFD renders no
      spread at all (use `master.affinity`, which replaces its control-plane preference), the CSI
      controllers render neither a PDB nor spread and drop a pod anti-affinity unless it carries
      `nodeSelectorTerms`. No `values-ha.yaml`: declaring the knobs where the user already reads
      makes a recipe file redundant, and the walkthrough belongs in the docs (T17). Plus
      `worker.disableApplications` (default `["*"]`) rendered into the container args, replacing
      the conditional `--disable-applications=device-manager` that let chart mode install the
      other three at runtime — which is what made the enabled subcharts collide. Adding the knobs
      at their own defaults leaves the default render **byte-identical**, which is the check that
      they were restated faithfully.
      Verify: `.sbin/helm template x deploy/gpustack-operator/chart` byte-identical to the pre-change render, and `grep -c 'kind: PodDisruptionBudget'` → `0`; with an HA values file → `3` PDBs, `replicas: 3` on worker/Kueue/NFD-master, `2` on both CSI controllers, `-enable-leader-election` on the NFD master, `minAvailable: "50%"` accepted by the generated schema

- [x] **T12 · Collapse `kuberess` to the bundled-chart installer**
      Blocked by: T6, T10
      Owns: `pkg/worker/kuberess/**`, `pkg/worker/worker.go`, `pkg/worker/option.go`,
      `pkg/system/control.go`
      Gate: review
      Acceptance: the four app installers and their values templates are deleted — but
      `testing/chart-baseline/**` is **not** T12's to delete: `baseline_dump_test.go` dies with
      the templates it renders, while the captured YAML stays as T8–T10's parity oracle;
      `apps_gpustack_operator.go` installs the bundled chart with the computed overlay and sets
      `TakeOwnership` only when a gpustack legacy release is detected; the AdmissionCheck apply
      moves into `Prepare()` with new retry-until-established behaviour; the
      `--disable-applications` name set is validated at flag-parse time; the Helm step treats an
      already-converged release as success.
      Built ahead of T9/T10, so two of its checks move to the tasks that make them
      expressible: the defaults-parity render, and the `driver.name` assertion (both
      `csi-driver-*` blocks still carry nothing but `enabled`, so the assertion could only
      fail). `pkg/system/control.go` needed no change — the name validation belongs to the
      flag that carries it.
      Verify: `go test -race ./pkg/worker/... ./pkg/system/... && make lint`. Run
      `golangci-lint run ./pkg/worker/kuberess/...` **early**, not at the end: `unparam`'s
      `_test.go` exclusion is keyed on where a finding *lands*, so a test-only call site can push
      a finding into a non-test file, and deleting the installers reshuffles which helpers have
      only-constant callers.

- [ ] **T13 · Atomic cut-over**
      Blocked by: T11, T12
      Owns: `deploy/gpustack-operator/chart/values.yaml`
      Gate: review
      Acceptance: chart mode becomes authoritative. The four subcharts already default to
      `enabled: true`; the ordering constraint that flip carries is that **T12 must land with or
      before it**, because until the worker stops installing at runtime the two owners collide.
      Measured on kind 1.23.17: with the defaults on and the runtime installer still live, the
      worker's `Prepare()` fails all three colliding installs (`must equal "gpustack-kueue":
      current value is "gpustack-operator"`) and crash-loops, while NFD silently runs twice.
      Verify: on kind — `helm install` then `helm list -n gpustack-system` → exactly one release,
      and `bash .claude/skills/_e2e-lib/scripts/assert-core.sh gpustack-system`

- [x] **T14 · Dockerfile + CI wiring**
      Blocked by: T3
      Owns: `pack/gpustack-operator/Dockerfile`, `.github/workflows/ci-chart.yml`, `hack/ci.sh`,
      `.github/configs/**`
      Acceptance: the four upstream chart `ADD` lines are gone; the packaged operator chart
      carries `charts/`; the chart CI job vendors before generating and asserts the tree stays
      clean and `charts/` stays free of `*.tgz`. It also runs T4's
      `hack/verify-chart-images.sh` as a cluster-free render check — once, not per Kubernetes
      version — and the workflow's paths filter reaches that script. Additionally a **3-node** kind config and a
      local render path: today `.github/configs/kind-config.yaml.tmpl` is a 2-node
      (control-plane + worker) template substituted only inside `ci-chart.yml` from
      `$RUNNER_TEMP`, so there is no file a developer can pass to `kind create cluster`, and
      2 nodes cannot satisfy F7's `DoNotSchedule` topology spread over three replicas — one pod
      stays Pending and the HA assertion fails for the wrong reason.
      Verify: `make package` from a clean clone, then
      `tar -tzf <image-extracted>/gpustack-operator-*.tgz | grep -c 'charts/kueue/Chart.yaml'` → `1`;
      plus the 3-node cluster comes up and `kubectl get nodes --no-headers | wc -l` → `3`

- [x] **T15 · Upgrade migration hooks**
      Blocked by: T13
      Owns: `deploy/gpustack-operator/chart/files/migrate-pre.sh`,
      `deploy/gpustack-operator/chart/files/migrate-post.sh`,
      `deploy/gpustack-operator/chart/templates/migrate/**`, and — as built —
      `deploy/gpustack-operator/chart/values.yaml` for the `migrate.image` override plus
      `pkg/worker/kuberess/apps_kueue_reap.go`, its test and the call site in
      `apps_gpustack_operator.go`
      Gate: review
      Acceptance: `pkg/worker/kuberess/apps_kueue_reap.go` and its test are deleted here, once
      the hook covers both modes — which it does because the Go Helm client never disables hooks,
      so the worker's own install of the bundled chart runs the same Jobs a `helm` invocation
      would. The reap therefore had to move to `pre-install,pre-upgrade`, not upgrade alone: the
      stranded-Kueue rescue it replaces ran before *any* install (see F8's note). The pre Job
      reaps a stranded Kueue and, on an upgrade only, server-side-applies the vendored subchart
      `crds/` out of the packaged chart in its own image, since a parent chart's `.Files` cannot
      reach `charts/**`. The post-upgrade Job retires the legacy release records and prunes what
      the new render does not contain, keyed on the instance label the adopting apply rewrites.
      RBAC and the script ConfigMap at weight -11, Jobs at -10. Both Jobs honour
      `global.imagePullSecrets` and `migrate.image`. The post Job is not rendered on a fresh
      install at all.
      Verify: `bash -n` on both scripts; `helm template` renders the pre Job with
      `GPUSTACK_PHASE=install` and no post Job, `--is-upgrade` renders both with
      `GPUSTACK_PHASE=upgrade`; hook weights read -11/-10; `go build ./...` after the reaper's
      deletion; the upgrade e2e case (T18) is what exercises the scripts against a cluster

- [x] **T16 · Uninstall/cleanup + NOTES warning**
      Blocked by: T13
      Owns: `deploy/gpustack-operator/chart/files/cleanup.sh`,
      `deploy/gpustack-operator/chart/templates/NOTES.txt`
      Acceptance: `cleanup.sh`'s release loop already named the image-mode release and the four
      legacy ones, so what it needed was reach: the APIService and webhook sweep now matches by
      the same `gpustack|kueue|nfd` name pattern its CRD step has always used, which is what
      reaches Kueue's `*.visibility.kueue.x-k8s.io` APIService and its `kueue-*` webhook
      configurations — the old sweep grepped for `gpustack` alone and matched neither. It also
      deletes what a **failed** migration hook leaves behind (T15's Jobs, ConfigMap,
      ServiceAccount and its cluster-admin ClusterRoleBinding, which Helm keeps on failure on
      purpose), selected by label and then by name so it never deletes the binding the cleanup
      hook is itself running under. NOTES lists what the release now deploys, warns that
      uninstalling it deletes the Kueue CRDs and therefore every ClusterQueue and Workload, and
      points at `cleanup.sh` for what `helm uninstall` cannot reach.
      Verify: `bash -n`; the rendered NOTES read back through Helm's Go SDK at defaults and with
      `kueue.enabled=false` / `node-feature-discovery.enabled=false` / `cleanupOnUninstall=true`,
      which covers every branch; on kind (T18) — install, `helm uninstall`, run `cleanup.sh`, then
      `kubectl get crd -o name | grep -cE 'gpustack|kueue|nfd'` → `0`

- [ ] **T17 · Docs**
      Blocked by: T13, T15, T16
      Owns: `docs/**`, `deploy/gpustack-operator/chart/README.md`,
      `deploy/gpustack-operator/chart/README.md.gotmpl`
      Acceptance: architecture (two modes, NodeFeatureRule owner, `--disable-applications`
      scope and names, the `deviceManager.enabled=false` change) — including the
      `NodeDevicesAdmissionReconciler` bullet, which still says `installKueue` applies the
      AdmissionCheck "right after the Kueue install"; T12 moves that apply into `Prepare()`, so
      the sentence goes stale. Also: development (vendoring and
      patch workflow, mirror-first), new `docs/operation/high-availability.md` — the walkthrough
      `values.yaml` already points at, so until this task lands that reference dangles: which knob
      to raise per component (worker, `kueue.controllerManager`, `node-feature-discovery.master`,
      both `csi-driver-*.controller`), the values to give them, the node count a hard spread needs,
      and what each subchart cannot do (no spread for NFD, no PDB or spread for the CSI
      controllers, `Recreate` by default) — including the
      single-URL webhook restriction; and `docs/migration/to-subcharts.md` (the
      `--take-ownership` command, the required release name **and namespace** — Kueue's
      `managedJobsNamespaceSelector` hard-codes `gpustack-system` because Helm cannot template a
      subchart value — and the widened uninstall blast radius). Chart README regenerated, never hand-edited.
      Verify: `make generate chart && git diff --exit-code deploy/gpustack-operator/chart/README.md deploy/gpustack-operator/chart/values.schema.json`

- [ ] **T18 · e2e sync + new cases**
      Blocked by: T13, T15, T16
      Owns: `.claude/skills/_e2e-lib/**`, `.claude/skills/gpustack-operator-chart-e2e/**`,
      `.claude/skills/gpustack-operator-e2e/**`
      Acceptance: `assert-core.sh` asserts in-release workloads instead of sub-releases; new
      cases for HA failover, upgrade adoption (with and without `--take-ownership`), image mode,
      and the legacy Kueue line's scheduling chain; a visibility-APIService observation step.
      Verify: `bash .claude/skills/gpustack-operator-chart-e2e/cases/case-*.sh gpustack-system`
      on the 3-node kind cluster, all green

**A transitional state to expect, not to fix.** Between T14 and T12, the Dockerfile has stopped
pre-baking the four upstream chart archives while `pkg/worker/kuberess/apps_*.go` still points at
those paths. `helm.Chart.Install` falls back to `DownloadURL`, so image-mode application installs
degrade to a network `helm pull` rather than failing — image mode keeps working but stops being
airgapped for those four apps until T12 deletes the code and T13 cuts over. This is acceptable
because no commit on this branch is independently releasable; the branch ships as one change.

**Checkpoints.** **CP-A** after T3/T4/T5 — the chart resolves offline, both Kueue lines render,
`global.*` reaches every image. **CP-B** after T10 — the full stack renders at parity with the
baseline, subcharts still disabled, nothing in the cluster has changed. **CP-C** after T13 — the
cut-over; both modes install cleanly on kind. **CP-D** after T18 — docs and e2e green.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- `pkg/kubeclientset` and `pkg/utils/certs/cache` have **no test files at all** (0.0% coverage).
  T7 changes concurrency-sensitive code in both, so a fake-client test harness has to be created
  before those fixes can be trusted.
- The values baseline (T2) must be captured from `main` **before** any Go values template is
  deleted; without it there is no oracle for the T8–T10 translation.
- `.github/configs/kind-config.yaml.tmpl` currently drives the CI matrix as a 2-node template
  rendered only inside the CI job; a 3-node variant plus a local render path is needed for every
  HA assertion, and is delivered by T14.

#### Unit tests

- `gen/chartvalues`: new — target ≥80%. Golden test over the emitted Kueue transformations and
  NFD matcher blocks; a test that a change to `nodefeature.CreditsPerCard` changes the output.
- `pkg/worker/kuberess`: 2026-07-28 - 37.6% → target ≥45% after the collapse. Overlay
  computation per `--disable-applications` set; Kueue line selection by Kubernetes version;
  `TakeOwnership` set only when a legacy release is present; `--disable-applications` name
  validation; `CSIProvisioner*` constants equal the chart's `driver.name`.
- `pkg/kubeapp/helm`: 2026-07-28 - 10.6% → target ≥25%. `TakeOwnership` reaches both actions;
  default stays `false`.
- `pkg/utils/certs/cache`: 2026-07-28 - 0.0% → **78.4%** (target ≥60%, met). Two concurrent
  writers converge on one Secret; two separate cache instances share one Secret, which is the
  per-replica case; no delete-all-duplicates path remains.
- `pkg/kubeclientset`: 2026-07-28 - 0.0% → **27.4%** (target was ≥40%; **not met**, and
  deliberately not padded). A conflicting update retries when an align function is supplied, and
  does **not** when none is — the second case is the guard that pins why the installers must
  supply one. Plus the `Create` + `WithUpdateIfExisted` conflict retry, which was dead code. The
  covered functions are the two T7 edits: `Update` 83.3%, `Create` 66.1%. The uncovered remainder
  — `UpdateStatus`, `Patch`, `Delete`, their `*WithCtrlClient` twins and the RBAC compare/align
  factories — is roughly half the package and untouched by this spec, so reaching 40% means
  testing unrelated functions. Left as its own task rather than written for the number.
  Conflicts cannot arise naturally against client-go's fake `ObjectTracker` (no optimistic
  concurrency, no `GenerateName`), so they are injected with `PrependReactor`. A fake-backed
  informer additionally needs `SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)`,
  or the reflector's watch-list probe hangs the test for 10 s and fails on `sync informer`.
- `pkg/api`: 2026-07-28 - 0.0% → 64.5%. `pkg/webhook`: 2026-07-28 - 0.0% → 51.7%.
- `pkg/nodefeature`: 2026-07-28 - 76.0% — unchanged; the generator reads it, it is not modified.

#### Integration tests

Chart-render level, runnable without a cluster (`make test chart` plus a render harness);
concrete test names to be added after the implementation PR merges.

- Defaults parity: chart-mode render at defaults vs the captured baseline, CRDs excluded.
- Global image fan-out: every `image:` under a rewritten registry/namespace, including hook Jobs.
- Global pull secrets on every pod spec.
- Kueue CRD version guard: the render at `--kube-version 1.33.12` carries `selectableFields`,
  the one at `1.29.14` does not, and the two differ in nothing else.
- `NodeFeatureRule` renders with the NFD subchart disabled, no value removes it, and its vendor
  matcher equals the sorted PCI vendor IDs of `manufacturers`.
- No RoleBinding is rendered into `kube-system`.
- HA knobs: an HA values file yields three PDBs (worker, Kueue, NFD master), the requested replica
  count on all five controllers, a `DoNotSchedule` spread on the worker and Kueue, and a default
  render that is byte-identical to the one before the knobs were declared.
- Offline guarantee: `helm template` **and** `helm lint` from a fresh clone with networking
  disabled.
- Vendoring hygiene: `make lint chart` / `make test chart` leave the tree clean and `charts/`
  free of `*.tgz`.
- Mode parity: chart-mode defaults vs the image-mode computed overlay, diffed.

#### e2e tests

On a local 3-node kind cluster, via the two e2e skills.

- **Chart mode, defaults** — one Helm release; worker and device-managers Ready; the
  `gpustack-node-devices` AdmissionCheck reaches `Active` and the accelerated ClusterQueue
  references it; the NFD → Worker → Kueue chain materializes.
- **Image mode** — a bare worker against an empty cluster installs the whole stack and the chain
  materializes; `--disable-applications=csi-driver-nfs,csi-driver-s3` omits exactly those two.
- **Concurrent image-mode startup** — three workers boot simultaneously: exactly one release in
  `deployed`, no replica CrashLoops, exactly one `gpustack-cert-*` Secret on a fresh cluster, and no transient
  AdmissionCheck apply failure in the logs.
- **HA failover** — an HA values file the case writes itself; six pods Ready, one Lease holder each, spread satisfied
  via the topology-spread constraint; delete the worker leader → a standby takes the Lease within
  60 s and the chain still materializes; repeat for the Kueue leader; PDBs allow ≥1 disruption.
- **Upgrade adoption, negative** — install the last released chart, then `helm upgrade` **without**
  `--take-ownership`: must fail with `invalid ownership metadata`, proving the hook ordering
  claim and that the flag is load-bearing.
- **Upgrade adoption, positive** — the same upgrade **with** `--take-ownership`: succeeds; no
  ClusterQueue / Workload / ResourceFlavor lost; NFD node labels survive; `helm list -A` shows
  exactly one release; the NFD 0.19.0 CRD schema is present (hook step 2 ran); Kueue's webhook
  `caBundle` is either preserved or re-injected within the measured window and admission
  recovers; re-running the upgrade is a no-op.
- **Hook image unavailable** — set the migration hook image to a non-pullable tag: the upgrade
  aborts cleanly and the cluster is unchanged.
- **Sub-1.31 Kueue** — a full scheduling-chain run on a <1.31 kind node with the single Kueue
  line enabled, proving the version-guarded CRD is accepted and admission works; not merely a
  render check.
- **Digest-pinned worker image** — image mode with a digest-pinned worker: assert the failure is
  loud (the device-manager image falls back to a composed tag) rather than a silent atomic
  rollback of the whole application release.
- **Uninstall completeness** — chart mode install → `helm uninstall` → `cleanup.sh`: zero
  gpustack / kueue / nfd CRDs, zero visibility APIServices, and NFD's `post-delete` hook
  observed to have removed its node labels.
- **Visibility residual observation** — record the `Available` condition of both
  `*.visibility.kueue.x-k8s.io` APIServices and the `kubectl api-resources` round-trip time;
  informational, feeds the follow-up decision on whether to patch them out.

## Alternatives

- **Plain `dependencies:` on the published chart repositories (no vendoring).** Simplest and
  zero maintenance on the upstream side — all five repositories already serve a valid
  `index.yaml`, so it works today. Rejected because Helm cannot template subchart values, so
  `global.imageRegistry` / `imageNamespace` would stop reaching Kueue, NFD and the CSI drivers;
  airgap and private-mirror users — a core GPUStack audience — would have to override six-plus
  value paths by hand.
- **Keep Kueue on its own `gpustack-kueue` release.** Would preserve today's narrower uninstall
  blast radius. Rejected: a Helm subchart's objects belong to the parent release by
  construction, so this means Kueue is not a subchart — reinstating the split deployment model
  and leaving issue #52's Kueue half without a configuration channel.
- **Patch `helm.sh/resource-policy: keep` onto the Kueue CRDs** so uninstall cannot delete them.
  Recovers today's safety margin, but adds a patch to re-align on every Kueue bump and makes
  `helm uninstall` leave state behind that `cleanup.sh` must then remove anyway.
- **A pre-upgrade hook that rewrites `meta.helm.sh/*` ownership annotations.** The original
  design here. Rejected on evidence: Helm validates ownership at `install.go:354` /
  `upgrade.go:350`, before hooks run at `install.go:448` / `upgrade.go:421`, so the hook can
  never execute in the failure case it exists to fix.
- **A standalone, scoped adoption script run before the upgrade** (rewriting only the objects in
  `helm get manifest <legacy release>`). Correct, and more precise than `--take-ownership`
  because it cannot touch a user's unrelated same-named object. Rejected as the primary path:
  it is a script to write, maintain and parse defensively, for the same one-command friction as
  the native flag. Kept in the migration doc as the cautious option if a user reports a
  name collision.
- **Keep the four app-specific Go installers for image mode**, pointed at the vendored subchart
  directories. Closest to today's behaviour, but the values for each would have to be written a
  second time in Go — reinstating exactly the configuration surface this work removes.
- **Ship a full `values.yaml` inside the image for image mode** and have the worker read it,
  overlay runtime detections, and install. Would give image mode the per-value tuning it now
  lacks, but adds a third artifact to keep in sync with the chart's own defaults.
- **Keep the runtime installer, but source its values from chart values via a ConfigMap.**
  Preserves every dynamic behaviour and needs no migration. Rejected because it keeps two
  install phases, keeps `helm uninstall` incomplete, and adds a Go merge layer on top of the
  templates it was supposed to remove.
- **Subcharts for NFD/CSI only, Kueue stays runtime-installed.** Lowest risk, but permanently
  splits the deployment model in two and leaves issue #52's Kueue half needing bespoke code.
- **Stay on the `thxCode/node-feature-discovery` fork, or carry its `NodeFeatureRule` template
  as an NFD subchart patch.** Both keep the rule inside the NFD chart. Rejected because the rule
  must survive `node-feature-discovery.enabled=false` — an operator with their own NFD still
  needs it — and because a template patch is one more thing to re-align on every NFD bump.
- **Apply the `NodeFeatureRule` from the worker at runtime**, like the Kueue AdmissionCheck.
  Rejected: it pulls a declarative object back into Go and works against the one-source goal.
  The AdmissionCheck stays in Go only because Kueue ships its CRDs in `templates/crd/`, where
  the same-pass ordering does not hold.
- **Patch Kueue's visibility APIServices and Service out of the chart.** Would fully honour
  "no visibility capability" and avoid a registered-but-broken aggregated APIService. Rejected
  for this cycle in favour of the zero-patch route; e2e observes whether the residual actually
  harms discovery.
- **Vendor a second Kueue tree (chart 0.17.8, `kueue-legacy`) for pre-1.31 clusters.** The
  original design, carried through T3 and T8 and then withdrawn: it bought a two-line CRD
  difference with 106 files, a `chart-rename.patch`, a duplicated 290-line values block, a
  `nameOverride` subtlety load-bearing for an immutable selector, a both-enabled fail guard, and
  a per-CI-leg switch. F6's version-guard patch obtains the same compatibility for two lines of
  template.
- **Drop Kubernetes <1.31 and ship chart 0.18.4 unmodified.** Rejected: the chart advertises
  `kubeVersion: ">=1.23.0-0"` and CI tests 1.23→1.35, so this is a public compatibility break,
  and the guard costs two lines.
- **Promote the worker's pod anti-affinity to `required` when `replicas > 1`.** Rejected: a
  2-node cluster asking for 3 replicas would leave one permanently Pending. Node spread is
  expressed with `topologySpreadConstraints` in the HA recipe instead.
- **A distributed lock around the whole of `Prepare()`.** Rejected: the three real races each
  have a targeted, lock-free fix (conflict retry, fixed-name cert Secret, converged-release
  check), and a lock would serialise every boot for no additional safety.

## Open Questions

None outstanding. The design was red-teamed by two independent second-opinion toolchains; both
independently identified the hook-ordering blocker, which is fixed above and covered by a
negative e2e case. Every other finding was verified against the Helm 3.21 source or this
repository and either folded into the features above or recorded in Notes / Risks.
