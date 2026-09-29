# Spec: OCI image source for ModelArtifact (S7)

Status: Shipped
Type: Feature

## Summary

S7 adds an `image` source to `ModelArtifact`: the union's fourth field and the third member
admission accepts (after `huggingFace` and `persistentVolumeClaim`; `modelScope` stays reserved
and refused). Weights are identified by a digest-pinned OCI image reference and delivered by
mounting a Kubernetes image volume
(`volumes[].image`, the ImageVolume feature). Delivery is on demand — kubelet pulls the image on
the node that needs it — and nothing enters the model-manager plugin's cache chain: no
`NodeModelStore` entry, no manifest, no watermark accounting. The version floor for the feature is
its own, separate from the project's 1.29 functional floor, and admission refuses the source
where the cluster cannot serve it.

## Motivation

Weights already exist as OCI images in many estates: users build them from model repositories
(Dockerfile COPY, oras, skopeo) or receive them from a registry pipeline. Today such a user must
let the operator re-download the weights from a hub, or publish a PVC — both re-introduce a copy
step and a second source of truth. An `image` source lets the artifact point at the image the
user already trusts, and lets the platform deliver it with the runtime's own mechanics: content
addressing by digest, layer sharing across images on the node, and registry mirrors (pull-through
caches, P2P prefetchers) that the runtime already honors.

### Goals

- A tenant can declare an `image`-source `ModelArtifact` and mount it from a `ModelDeployment`
  and an `Instance`, read-only, at the fixed model path.
- The artifact's identity is the image reference the user pinned; one artifact always delivers
  the same bytes.
- Admission refuses the source where the platform cannot honor it, naming the version floor and
  the reason (two-layer-floor degradation, never a silent field drop).
- The double-storage cost, the kubelet image-GC interaction, and the CRI size accounting gap are
  documented with the measured numbers so operators can size disks and predict re-pulls.

### Non-Goals

- Image sources do **not** enter the node cache plugin chain: no `NodeModelStore` entry, no
  plugin mount, no `ModelPrefetch` — S6's warm-up path serves the plugin's cache only — no
  budget accounting, no peer sync (S5). Kubelet's own image store and image GC govern them.
- No registry client in the operator: no tag→digest resolution, no existence check, no content
  verification of the image. The user's build is the authority (see "The digest and the
  verification promise").
- No `path`/`subPath` inside the image (would raise the runtime floor to containerd ≥ 2.2); the
  image root is mounted whole, so the build must put the weights at the image root.
- No containerd node configuration changes (`discard_unpacked_layers` is documented as a node
  administrator's option, never touched by the operator).
- Dragonfly integration (S8). Mirrors are documented only.
- ModelScope and external providers unchanged.

## Proposal

One API extension and three wiring points:

- `ModelArtifactSource.Image *ModelArtifactImageSource` — the union's fourth field and the third
  member admission accepts:
  `{reference: "registry/repository@sha256:<64 hex>"}`. Admission requires the digest form (a
  mutable tag would let one artifact deliver different weights on different pulls, breaking the
  identity contract every frozen reference and the KV identity rely on) and refuses source
  selection patterns, as for a claim. Admission also refuses the source on any cluster whose
  apiserver is older than the ImageVolume default-on floor (see the capability gate).
- Delivery: a new `ModelDeploymentModelDeliveryImage` value. An image-source artifact always
  delivers `Image`, whatever `model-artifact-delivery-mode` says (the Setting governs hub
  sources' Engine/Node choice only). Both consumers render the image volume; `Instance`
  additionally pre-checks the pinned node's runtime (below).
- Resolution: no network, like a claim. The first reconcile sets `Resolved=True` with
  `status.resolved` carrying only `resolvedTime`; `revision`, `manifestDigest`, `fileCount` and
  `sizeBytes` stay absent, `status.nodes` stays absent, and there is no revalidation. The
  reference in the immutable spec is the only content identity.
- Placement preference: for `Image` delivery, the soft
  node preference is computed from `Node.status.images` — the nodes kubelet already reports
  holding the reference — reusing the digest preference's shape (weight 100, ≤16 hostnames,
  deterministic order, soft only).

### Measured facts this design relies on (PoC-K)

All `[跑]` readings were taken on Kubernetes 1.35.7 (apiserver 1.35.6) / containerd 2.2.6, Ubuntu
24.04, CPU nodes (2 vCPU, 8 GB, ~96 GiB ext4 root, nodefs and imagefs on one disk), an in-cluster
registry exposed to the node over hostNetwork; the model was `Qwen/Qwen2.5-7B-Instruct` at commit
`a09a35458c702b33eeacc393d103063234e8bc28` (16 files, 15,242,807,270 B), built one safetensors
shard per layer:

- **Image volumes work end to end (K-1).** A ~15 GB model image mounts through
  `volumes[].image`; every file matches the Hub copy byte for byte (sha256 equals the LFS oid).
  The mount is a read-only overlay of the layer snapshots.
- **Double storage (K-2).** The node keeps both the compressed layer blobs and the unpacked
  snapshots (`discard_unpacked_layers=false`, the default): content store +12,074,816,089 B and
  snapshots +15,242,807,270 B for the gzip image — **1.79× the raw weights**; an uncompressed-layer
  image measures **2.00×** (it saves decompression time, not disk). Pull of the 12.07 GB image
  took 12m43s (~20 MB/s over the in-cluster registry on a 2-vCPU node). Kubelet accounts the CRI
  image size — the compressed size — so its books show ~12.07 GB where the disk really holds
  ~27.3 GB (**~2.26×** under gzip). Capacity judgments must not use the CRI size.
- **PSA (K-3, supersedes the earlier "Restricted refuses image volumes" claim).** On an apiserver
  ≥ 1.33, PSA Restricted **admits** `image` volumes (kubernetes#130394, merged 2025-02-24,
  milestone 1.33, not backported); on ≤ 1.32 Restricted refuses them. `enforce-version` makes no
  difference either way (the check carries a single `MinimumVersion: 1.0`). Baseline admits the
  volume wherever the apiserver knows the field. On the clusters this feature targets (≥ 1.35
  apiserver), PSA is not an obstacle, including for restricted tenants.
- **Image GC interaction (K-4).** While a Pod mounts the image, kubelet does not collect it
  (verified 1.35.7 / 2.2.6; the in-use protection is in kubelet's image GC manager since 1.31 and
  needs containerd ≥ 2.1). After the last Pod referencing it goes away, the image can be collected
  in the next GC round (cycle: 5 minutes; measured release→collection: 4m2s) — `imageMinimumGCAge`
  does not protect it, because kubelet compares against the image's *first-detected* time, not its
  release time. Scaling a deployment to zero and back can therefore re-pull a dozen GB. The only
  retention is another reference (e.g. a resident Pod mounting it; a completed Pod does not count —
  the in-use check reads *running* containers). A model image pushing the disk past
  `imageGCHighThresholdPercent` (default 85) also drags down every other unused image on the node
  in the same rounds (measured: 18 images collected alongside).
- **Layer sharing.** Two images sharing layer digests share snapshots on the node and containerd
  skips the download (measured: 112 ms and +11.8 MB where a cold node paid 2m10s and +7.89 GB).
- **Version line.** ImageVolume: 1.31 alpha (off), 1.33 beta (off), **1.35 beta on by default**,
  1.36 GA (`LockToDefault`). containerd supports image volumes from **2.1** (subPath would need
  ≥ 2.2 — excluded by scope). Kubelet's in-use GC protection needs kubelet ≥ 1.31 and
  containerd ≥ 2.1.

### User Stories

#### Story 1
As an MLOps engineer, my weights are already in our internal registry as an OCI image, built and
scanned by our pipeline. I declare a `ModelArtifact` with that digest-pinned reference and serve it
with a `ModelDeployment`; the node pulls the image it already trusts, and nothing re-downloads
anything from a hub.

#### Story 2
As a platform operator, I run a pull-through registry mirror (Harbor) or a P2P prefetcher
(Dragonfly, Spegel) under containerd. Image-source deliveries ride the mirror like every other
pull, so fleet-wide cold starts amortize without any operator involvement.

#### Story 3
As a tenant in a Restricted namespace, I mount an image-source artifact without asking anyone:
on a supported apiserver, Restricted admits the volume.

#### Story 4
As a platform operator on a 1.32 cluster, I try to create an image-source artifact and get a
refusal that names the 1.35 floor and the alternatives, instead of a Pod that silently mounts
nothing.

### Core Features & Acceptance Criteria

**F1 API: the `image` source union member.** `ModelArtifactSource.Image`
(`*ModelArtifactImageSource`) with one required field, `Reference` (1–1024 characters). The
artifact's spec stays immutable; an image source is created, not edited, like every other member.

- AC1: the union still admits exactly one member; the exactly-one refusal message names the
  three admitted members (`huggingFace`, `persistentVolumeClaim`, `image`), while `modelScope`
  keeps its own reserved-and-refused refusal as today.
- AC2: a reference that is not `<registry/repo>@sha256:<64 lowercase hex>` is refused at
  admission, with the message naming why the digest is required (a tag is mutable, so one
  artifact could deliver different weights on different pulls; reopening tags later is a webhook
  change, not a schema change).
- AC3: `allowPatterns`/`ignorePatterns` on an image source are refused with the claim source's
  wording (an image is mounted whole; there is no listing to select from).
- AC4: **capability gate.** Creating an image-source artifact on a cluster whose apiserver
  version is below 1.35 is refused, with a message naming the floor, that 1.33/1.34 need the
  feature gate opened by an administrator (not offered as an opt-in in this version; reopening it
  later is a webhook change), and the alternatives (hub or PVC sources, or upgrading). At 1.35 or
  above the source is admitted. The check reads the worker's startup version snapshot
  (`system.LoopbackKubeVersion` + a new `kubediscovery.FeatureImageVolume`), costing no API call;
  both directions are unit-tested through the version seam.

**F2 Resolution without a registry client.** The reconciler resolves an image source on its first
pass, with no network I/O.

- AC5: `Resolved` becomes True with reason `Resolved` and the condition message stating that the
  image is the user's and the operator does not read it; `status.resolved` carries only
  `resolvedTime`; `revision`, `manifestDigest`, `fileCount`, `sizeBytes` stay absent (the CRD
  field docs are updated to say so for image sources).
- AC6: `status.nodes` stays absent; no revalidation pass is scheduled; `Degraded` is False
  (`Healthy`) from the first successful pass.

**F3 ModelDeployment delivery (`Image`).** A deployment referencing an image-source artifact
renders the weights as an image volume on every managed role's engine container.

- AC7: the weights volume is `volumes[].image` with `reference` = the spec's reference and the
  default pull policy (IfNotPresent, omitted), mounted read-only at
  `/var/lib/gpustack/model` under the existing weights volume name; the role volume admission
  rules around the model paths apply unchanged.
- AC8: no cache `emptyDir`, no `HF_*`/proxy env, no `--revision` argument, no ephemeral-storage
  raise; the served model path is the mount path, exactly as for a claim; `--served-model-name`
  rules unchanged.
- AC9: `status.model.delivery` = `Image`; `WeightsReady` follows the claim/node mounted path
  (`PodReadyToStartContainers`); a failed pull keeps `WeightsNotMounted` and kubelet's own event
  names the pull error (documented limitation: the condition does not distinguish a slow pull
  from a failed one).
- AC10: `model-artifact-delivery-mode` does not affect image sources (always `Image`); the
  Setting's change watch does not roll image-delivered deployments by itself.

**F4 Instance delivery.** An Instance model volume referencing an image-source artifact mounts
the image volume read-only, with no `subPath`.

- AC11: the CSIDriver check (node delivery's plugin dependency) does not run for image delivery —
  an Instance's image volume needs no plugin; the Instance creates its Pod while the reference is
  resolved.
- AC12: **node capability pre-check.** Before rendering, the Instance reconciler reads the pinned
  node's `status.nodeInfo.kubeletVersion` and `containerRuntimeVersion`; kubelet < 1.35 or
  containerd < 2.1 blocks Pod creation with a reason naming the node and the two floors (a Pod
  whose image volume the node cannot run otherwise fails late, in a kubelet event the Instance
  status does not surface). A node whose versions cannot be parsed blocks the same way. (Bodies
  of this check are read from a Node the reconciler already reads for hostname pinning; no extra
  watch.)

**F5 Placement preference from `Node.status.images`**

- AC13: for `Image` delivery, the soft preference names the hostnames of the nodes whose
  `status.images[].names` include the exact reference, reusing the digest preference's weight
  (100), cap (16 hostnames) and determinism; no required affinity is ever added. The order is
  hostname-sorted: `Node.status.images` carries no referenced-now or last-used signal, so the
  digest preference's serving-now ordering has nothing to sort on here, and sorting on the names
  is the only order that depends on nothing but the candidates. A stale positive (kubelet not yet
  reporting a collection) costs one re-pull, the same softness rationale as the digest
  preference.

**F6 ModelPrefetch refuses image sources.**

- AC14: creating a `ModelPrefetch` whose artifact's source is `image` is refused at admission,
  with a message that image sources deliver on demand through kubelet and do not enter the node
  cache this version warms (no budget, no pinning, no warm-up Pod).

**F7 Docs.**

- AC15: a new docs page (working title `docs/reference/model-image-source.md`, final name per the
  docs skill's routing) carries: the source's shape and the digest contract; the build recipe
  (weights at the image root, one shard per layer, pinned upstream commit) with the PoC-K
  byte-equality example; the version line and runtime floors; the PSA paragraph (≥ 1.33 Restricted
  admits, ≤ 1.32 refuses, `enforce-version` irrelevant; Baseline admits); double storage
  (1.79×/2.00×/2.26× with their exact conditions and why the CRI size must not be used for
  capacity); the three GC rules (in-use protection; release→next-round collection and the
  scale-to-zero re-pull; the 85% cascade clearing other unused images); registry mirrors
  (Harbor pull-through, Spegel and Dragonfly as configured containerd mirrors — documented as
  unmeasured, Dragonfly integration itself being S8 scope); private registries via the role's /
  Instance's existing `imagePullSecrets`.
- AC16: `docs/reference/model-artifact.md` gains the union member (resource example, patterns
  note, delivery table row, status row, requirements bullet) and links the new page;
  `docs/README.md` indexes the new page. No `docs/settings.md` change: the feature adds no
  Setting.

### Notes / Constraints / Caveats

- **Two-layer floor.** The project floors stay put: functional 1.29 (bundled Kueue), install 1.23.
  The image source is opt-in and carries its own floors — apiserver ≥ 1.35 for the default-on
  beta gate, kubelet ≥ 1.35 (1.33/1.34 with an admin-opened gate, not opt-in-able here),
  containerd ≥ 2.1 — and admission refuses below the apiserver floor, refusing the source and
  naming the reason rather than degrading silently; the core path is untouched.
- **The digest and the verification promise.** The OCI digest pins the *image's* manifest bytes.
  It is not the artifact manifest digest of a hub source (`status.resolved.manifestDigest` stays
  absent), and the two are never claimed equal. The operator verifies nothing about the image's
  content in this version: it does not read the registry, so "this image contains the weights"
  is the user's assertion, and the operator's guarantee is only that the Pod mounts exactly the
  image the immutable spec names. The documented build recipe (from the PoC) shows how to produce
  an image whose content matches a hub commit byte for byte, and the PoC verified that equality
  for its fixture, but no code enforces or checks it. KV identity therefore falls back to the
  artifact-UID hash (the claim source's behavior): two artifacts naming the same image never
  share KV blocks — safe, merely not deduplicated.
- **Weights at the image root.** No `path`/`subPath` is offered (subPath would floor containerd
  at 2.2); the mount is the image root, so the engine sees whatever the image's root holds. The
  build contract ("weights at `/`") is documentation, not validation.
- **Gate explicitly off on a ≥ 1.35 apiserver** (possible until 1.36 GA) would make the apiserver
  drop the volume field silently — the exact hazard `FeatureNativeSidecar` documents. This
  version does not probe for it (a dry-run canary was considered and rejected as v1
  over-engineering): the failure mode is a Pod without the weights volume, which the engine
  fails on visibly, and the consumer reports `WeightsNotMounted`. Documented as a known
  limitation; 1.36 GA removes it.
- **Mutability of `Node.status.images`.** The image list is kubelet's periodic report; a collected
  image can linger in it and a freshly pulled one can lag. It is also bounded: kubelet reports at
  most `--node-status-max-images` (default 50) entries, so a node running many images may never
  name the model image at all. The preference is soft exactly so that every one of these
  absences costs only this round's locality gain — one extra re-pull, never a placement veto —
  and no code compensates for them. `Node.status.images[].sizes` (CRI, compressed) is
  deliberately unused — PoC-K measured it at ~44% of real occupancy.
- **Names.** The delivery value is `Image`; no KV-cache vocabulary is touched.
- **The PoC environment is gone.** S5's test cluster was scheduled for deletion at S5's close; no
  reading above depends on it. The e2e environment is adjudicated: local kind, gated on
  verification (see Open Questions).

### Boundaries

- **Always:** keep the plugin chain out of image delivery (no `NodeModelStore` entry, no CSI
  volume, no budget); keep the reference digest-pinned; keep the preference soft; keep the
  capability refusal's message self-sufficient (floor + alternative).
- **Ask first:** any tag-reference support (needs resolution or a mutable-identity waiver —
  product behavior); any `path`/`subPath` (raises the runtime floor); any image-content
  verification (a registry client — new dependency surface).
- **Never:** let an image source produce a `NodeModelStore` write, a prefetch warm-up Pod, or a
  `status.resolved.manifestDigest`; put containerd configuration advice anywhere but docs;
  reference the task-tracking report from repo documents.

### Risks and Mitigations

- **Silent volume drop (gate off / very old apiserver)** → the create-time capability gate closes
  the ≤ 1.32 direction; the ≥ 1.35 gate-off direction is documented (above) and self-revealing
  (the engine cannot start without weights). Revisit a dry-run probe if it bites in the field.
- **GC re-pull churn on scale-to-zero** → documented with the measured numbers; the placement
  preference reduces cross-node re-pulls; retention via a resident reference is the user's
  documented option. A `pinned`-style retention would need CRI pinning or plugin ownership — out
  of scope (Non-Goals).
- **Disk sizing surprises (double storage)** → the docs page carries the three multipliers with
  their conditions and the CRI-size warning; nothing in code assumes image size.
- **Private registry pulls fail with imagePullBackOff** → documented: set the role's /
  Instance's `imagePullSecrets`; the consumer shows `WeightsNotMounted` with the Pod's events as
  the diagnostic path.
- **Mixed-version fleets** → the Instance pre-check converts a late kubelet error into a blocked
  Instance with a named node and floors; the MD path (node unknown at render) relies on the
  same kubelet event surfacing, documented.

## Design Details

### Commands

- `make generate` after the API edit (deepcopy, CRD, openapi, protobuf, applyconfigurations).
  The generator fails fast unless the checkout's path ends in `gpustack.ai/gpustack`, so it runs
  in a worktree laid out that way, never hand-editing generated files; a regeneration that leaves
  `git status` clean against the tested commit proves the committed generated tree is current.
- `make lint` (+ `make lint docs` for the docs change). The chart is untouched (CRDs are
  generated into the chart tree by `make generate` — regenerate, don't hand-edit; if the
  generated chart CRD diff trips `make lint chart`, run it too).
- Package tests: `go test ./api/worker/... ./pkg/worker/webhooks/worker/...
  ./pkg/worker/controllers/worker/... ./pkg/kubediscovery/...`.
- Environments. Unit tests and both lint targets run on the local host. e2e runs on a local kind
  cluster built by the repository's own e2e harness (it builds the operator image for the cluster
  and loads it); no remote build host is in this spec's path. The one remote-shaped possibility —
  a provider CPU cluster if the kind verification below fails — would be provisioned through the
  task's own channel, not by this spec's commands.

### Project Structure

- `api/worker/v1alpha1/model_artifact.go` — `ModelArtifactImageSource` + the union member
  (field docs: identity = the pinned reference; resolved fields absent).
- `api/worker/v1alpha1/model_deployment.go` — `ModelDeploymentModelDeliveryImage`.
- `pkg/kubediscovery/feature.go` — `FeatureImageVolume` (floor 1.35).
- `pkg/worker/webhooks/worker/model_artifact.go` — union, digest-pin, patterns, capability gate.
- `pkg/worker/webhooks/worker/model_prefetch.go` — image-source refusal.
- `pkg/worker/controllers/worker/model_artifact.go` — `reconcileImage` (no network).
- `pkg/worker/controllers/worker/model_artifact_placement.go` — weights resolution `Image` branch.
- `pkg/worker/controllers/worker/model_deployment_artifact.go` — `Image` render branch (volume +
  mount + model path), `ModelDeploymentArtifactRender.ImageReference`.
- `pkg/worker/controllers/worker/model_deployment.go` — pass-through wiring; status observation
  reuses the claim/node mounted path.
- `pkg/worker/controllers/worker/instance.go` — `convertAdditionalVolumes` `Image` branch; the
  node capability pre-check.
- `pkg/worker/controllers/worker/model_placement_preference.go` — `Node.status.images` preference.
- `docs/reference/model-image-source.md` (new), `docs/reference/model-artifact.md`,
  `docs/README.md`.
- `.agents/skills/gpustack-operator-e2e/cases/case-<NNN>.sh` + the SKILL.md row — `<NNN>` taken
  at my-ship as main's then-max + 1, after the final rebase.

### Code Style

Follow repo conventions; illustrative pieces only.

```go
// The union's fourth field, the third member admission accepts
// (api/worker/v1alpha1/model_artifact.go).
// Image is an OCI image reference holding the weights. The reference must pin a digest:
// "registry/repository@sha256:<64 hex>". The digest is the artifact's whole identity — the
// operator never reads the registry, so it verifies nothing about the image's content, and
// status.resolved stays claim-shaped (no revision, no manifestDigest).
//
// +optional
Image *ModelArtifactImageSource `json:"image,omitempty" protobuf:"bytes,4,opt,name=image"`
```

```go
// The renderer's branch (pkg/worker/controllers/worker/model_deployment_artifact.go).
case a.Delivery == workercore.ModelDeploymentModelDeliveryImage:
    return []core.Volume{{
        Name: modelDeploymentModelVolumeName,
        VolumeSource: core.VolumeSource{Image: &core.ImageVolumeSource{
            Reference: a.ImageReference,
        }},
    }}, []core.VolumeMount{{
        Name: modelDeploymentModelVolumeName, MountPath: ModelDeploymentModelMountPath, ReadOnly: true,
    }}
```

```go
// The capability gate reads the startup version snapshot through a swappable function, so a test
// can choose the cluster version without a cluster — the package's own seam for settings-shaped
// inputs (precedent: modelArtifactDeliveryMode). The snapshot's Configure ignores later calls,
// so pointing the tests at the snapshot itself could not test both gate directions in one
// package. SupportsFeature already answers false on a nil or unparsable version, so the
// fail-closed default falls out of the helper.
var modelArtifactClusterVersion = func() kubediscovery.Version {
	return system.LoopbackKubeVersion.Get()
}
```

### Implementation Plan

Tasks run sequentially in one seat. Every task starts with
`git fetch origin && git rebase origin/main`; images and e2e rebase immediately before they run;
every task lands its own `--signoff` commit (folded per module at ship; the spec file is the last
commit). Stage gates go to the coordinator: plan gate (this document), built gate, ship gate.
T1 is a spike and is run first on purpose: if the kind environment cannot serve image volumes,
the e2e environment decision escalates while the code tasks are still ahead of it, not after.

- [x] **T1 · e2e environment verification (spike)**
      Blocked by: —
      Gate: review
      Owns: nothing in the tree (a local kind cluster, a probe Pod, evidence files only).
      Acceptance: the candidate kind node image reports containerd ≥ 2.1 and the ImageVolume
      feature enabled; a digest-pinned image made visible to the node's containerd mounts through
      an image volume in a raw probe Pod (no operator code involved), under both delivery paths
      that matter for the case (a registry pull and a pre-loaded IfNotPresent image); the probe's
      readings are recorded. A failed check stops the task and escalates the e2e environment
      decision rather than improvising.
      Verify: the probe Pod reads the image's marker file; the readings name the node image's
      containerd version and the feature state.
- [x] **T2 · API + codegen**
      Blocked by: —
      Owns: `api/worker/v1alpha1/model_artifact.go`, `api/worker/v1alpha1/model_deployment.go`,
      generated trees (deepcopy, CRD, openapi, protobuf, applyconfigurations).
      Acceptance: the union field and the delivery value carry field docs that state the
      identity and the absent resolved fields; `make generate` leaves a clean tree in the gen
      tree.
      Verify: `go build ./... && go test ./api/worker/...`.
- [x] **T3 · Admission**
      Blocked by: T2
      Owns: `pkg/kubediscovery/feature.go`,
      `pkg/worker/webhooks/worker/model_artifact.go`,
      `pkg/worker/webhooks/worker/model_prefetch.go` (+ tests).
      Acceptance: AC1–AC4, AC14 — the union, digest-pin and patterns refusals with the message
      wording above; the capability gate refusing below the floor and admitting at/above it,
      both directions unit-tested through the swappable version seam; the prefetch refusal.
      Verify: `go test ./pkg/kubediscovery/... ./pkg/worker/webhooks/worker/...`.
- [x] **T4 · Resolution**
      Blocked by: T2
      Owns: `pkg/worker/controllers/worker/model_artifact.go` (+ tests).
      Acceptance: AC5, AC6 — first-pass resolution with a claim-shaped `resolved`; no nodes
      aggregation; no revalidation.
      Verify: `go test ./pkg/worker/controllers/worker/...`.
- [x] **T5 · Renderer + placement**
      Blocked by: T3, T4
      Owns: `pkg/worker/controllers/worker/model_artifact_placement.go`,
      `model_deployment_artifact.go`, `model_deployment.go`, `instance.go`,
      `model_placement_preference.go` (+ tests).
      Acceptance: AC7–AC13 — the volume/mount shape on both paths; no cache/env/args/ephemeral
      raise; `status.model` and `WeightsReady` behavior; the Setting-independence; the Instance
      node pre-check and the images-list preference as adjudicated (both in scope).
      Verify: `go test ./pkg/worker/controllers/worker/...`.
- [x] **T6 · Docs**
      Blocked by: T5
      Owns: the three docs paths of AC15/AC16.
      Acceptance: page routing, header/Contents/footer and the index entry per the docs skill;
      every number carries its version condition.
      Verify: `make lint docs`.
- [x] **T7 · e2e**
      Blocked by: T1, T5 (T6 not required)
      Owns: the case script + SKILL.md row + execution evidence under the task directory's
      `spec-7/raw/`.
      Acceptance: the cold-mount scenario below on the environment T1 verified (a failed T1
      escalates before this task starts); the capability-gate directions stay unit-tested (an
      old-version e2e cluster is out of scope).
      Verify: the case script against the cluster running the image built from the rebased head.
- [x] **T8 · Ship prep**
      Blocked by: T1–T7
      Owns: rebase, commit folding, chart-matrix local runs (Go change ⇒ REQUIRED: the CI chart
      matrix's 7 node images, locally, all rc=0), case number finalization, PR.
      Verify: `make lint`, `make lint docs`, the matrix runs, PR opened.

### Test Plan

#### Prerequisite testing updates

None: the existing suites stay green throughout; the touched packages all have table-driven
suites to extend.

#### Unit tests

- `pkg/kubediscovery`: `FeatureImageVolume` floor (1.34 false, 1.35 true, 1.36 true, unparsable
  false).
- `pkg/worker/webhooks/worker`: the union exactly-one with four members; digest-pin (tag refused,
  bare refused, uppercase hex refused, correct digest admitted); patterns refused; capability
  gate both directions through the swappable version seam (a 1.34 cluster refuses, a 1.35
  cluster admits, an unparsable version refuses); prefetch source refusal; the Instance webhook's
  admit-every-source baseline extended with an image artifact.
- `pkg/worker/controllers/worker`: `reconcileImage` (claim-shaped resolved, no nodes, healthy);
  weights resolution `Image` branch; MD render shape (volume reference, mount path, read-only,
  no cache volume, no env, no args, no ephemeral raise); Instance render shape; Setting
  independence; WeightsReady mounted path; the pre-check (old kubelet blocked, old containerd
  blocked, unparsable blocked, current admitted) and the preference (match, cap, order,
  determinism) as adjudicated.
- Every load-bearing new test is proven able to fail by a compile-safe mutation of the mechanism
  it guards (mutation → red → revert → green), recorded in the handback.

#### Integration tests

None as a separate tier; cluster behavior is the e2e case's.

#### e2e tests

One case (number taken at my-ship): on the kind environment after its verification (below),
build the fixture model
image per the documented recipe (small model, one shard per layer, weights at the root,
arch-matched builder; registry reachable from the nodes — in-cluster registry + socat forwarder
from the PoC prototype, or a kind-loaded image with IfNotPresent, per the plan's verification of
the node image's containerd/gate), then: create the image-source `ModelArtifact` → reference it
from a consumer → the Pod mounts the weights and reads them (fixture's expected sha256 checked)
→ delete the consumer. Documentation covers the double-storage and GC facts; if the environment
allows (root disk pressure control on a CPU node), one GC observation (release → collection on
the next round) is captured as evidence alongside, not as a gate.

Chart-matrix coverage follows the standing Go-change requirement at my-ship.

## Alternatives

- **Tag references resolved once by the operator.** Rejected for this version: it needs a
  registry client and a credentials story in the worker, a re-resolution hazard (the resolved
  digest would need its own immutability machinery in status), all to save the user one
  `crane digest`. Reopening later is additive.
- **Image sources through the plugin cache (materialize an image into the node cache).**
  Rejected: it duplicates the runtime's own content store, breaks the budget's meaning (the
  plugin's bytes would double-count the image's), and puts digest semantics the plugin verifies
  (file manifests) next to a source that has none — the boundary the Non-Goals fix.
- **`path`/`subPath` inside the image.** Rejected: floors containerd at 2.2 for everyone to
  spare one `COPY` in a Dockerfile.
- **No placement preference (scheduler spreads, re-pulls accepted).** Rejected: a first pull was
  measured at 12m43s; the soft preference's staleness costs one re-pull, the same trade the
  digest preference already made. Adjudicated: the preference is in scope (2026-09-27).
- **Capability opt-in Setting for 1.33/1.34.** Rejected: the Setting would claim a guarantee the
  operator cannot check (the gate state is invisible to it), and the population it serves
  (admins who opened the gate) can upgrade instead. The refusal message says so.

## Open Questions

None open. The five draft questions were adjudicated on 2026-09-27, each as recommended:

1. **Renderer scope — both `ModelDeployment` and `Instance`.** The resolution and render code
   paths are shared, the marginal cost of the second consumer is small, and an Instance-only
   image source would strand the MD path, the primary consumption surface.
2. **Placement preference from `Node.status.images` — in scope (AC13).** The pull cost justifies
   the bias and the softness bounds every staleness failure at one re-pull. The kubelet's image
   list is bounded (`--node-status-max-images`, default 50), so a busy node may never name the
   image; the preference stays soft and no code compensates for that absence.
3. **Digest-pinning required at admission — required (AC2).** The alternatives either import a
   registry client or break the identity contract. The spec commits to the honest mapping
   (image digest ≠ manifest digest; no content verification) rather than an unverifiable
   guarantee.
4. **Instance node capability pre-check — included (AC12).** The reconciler already reads the
   Node for the hostname pin, so the check is two parsed fields and one new blocked reason, and
   it converts the most confusing failure mode (mixed fleet, late kubelet error) into a named
   status.
5. **e2e environment — local kind, gated on verification.** The plan's first step verifies the
   candidate node image (`kindest/node` v1.35.x) ships containerd ≥ 2.1 with ImageVolume
   default-on, and that an image made visible to the node container's containerd
   (`kind load`-equivalent) satisfies an IfNotPresent image volume. A failed verification
   escalates to the coordinator for a CPU cluster per the task's cluster protocol, matching the
   PoC-K-verified versions (kubelet 1.35.7 / containerd 2.2.6); paid resources remain the user's
   call.
