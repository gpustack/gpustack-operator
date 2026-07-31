# Spec: Instance Node Pinning and Additional Volume Mounts

Status: Building
Type: Feature

## Summary

Two additive improvements to the `Instance` API. First, a new `spec.nodeName` pins an Instance to one
Kubernetes Node: the reconciler renders it as the backing Pod's `nodeSelector`
(`kubernetes.io/hostname`), never as `pod.spec.nodeName`, so placement stays mediated by the
scheduler and Kueue admission. Second, a new `spec.additionalVolumes[]` lifts the "one volume, always
the workspace" limit: an Instance can mount any number of extra volumes — an InstancePersistentVolume
reference, a ConfigMap, a Secret, or a host path — at arbitrary paths, with `readOnly` and `subPath`.
Because a host path (like `spec.privileged`) lets an Instance cross the node boundary, each is gated at
CREATE time behind its own default-off administrator setting — `instance-privileged-allowed` and
`instance-host-path-volume-allowed`; UPDATE is deliberately never gated, so flipping a setting off can
never block an already-deployed Instance from being edited or restarted. The existing `spec.volume` /
`spec.volumeMount` workspace contract is unchanged, so no downstream schema rename or data migration
is required.

## Motivation

### Goals

- **Target users** — platform users creating Instances (through the GPUStack product or directly as
  CRs) and cluster administrators governing what those Instances may request.
- Let a user pin an Instance to a named node while keeping the Kueue scheduling chain intact
  (`ResourceFlavor` → `ClusterQueue` → `LocalQueue`, quota admission, device-plugin allocation).
- Reject a node pin that can never schedule (unknown node, or a node outside the InstanceType's pool)
  at admission with an actionable field error, instead of leaving the Instance Pending forever.
- Let an Instance mount volumes beyond its workspace, from four source kinds, at arbitrary mount
  paths, read-only and/or by sub-path.
- Keep the workspace contract (`spec.volume` + `spec.volumeMount`, default `/workspace`) byte-identical
  when the new fields are unset, so existing Instances, the downstream Python `GPUInstanceSpec`, and its
  persisted rows need no migration.
- Make each of the two host-boundary escapes (`spec.privileged`, a `hostPath` additional volume) an
  explicit, auditable and **independent** cluster decision rather than something any Instance author can
  take — `privileged` grants strictly more than `hostPath`, so allowing one must not imply the other.

### Non-Goals

- No general Pod scheduling surface (`nodeSelector` maps, `affinity`, `tolerations`,
  `topologySpreadConstraints`). One node name is the whole placement feature.
- No per-node quota. Kueue quota stays pool-level; pinning does not reserve node capacity.
- No unification of `spec.volume` into the new list, and no change to its immutability.
- No additional ephemeral (`emptyDir`) source. The workspace stays the only ephemeral volume: an extra
  `emptyDir` would add nothing a subdirectory of the workspace does not already give, while its usage
  counts against the Pod's `ephemeral-storage` limit (which the Instance sizes from
  `resources.localStorage`) and so would evict the Pod at runtime for a reason nothing in the API
  explains.
- No new mounts on the `sshd` sidecar: it `nsenter`s into `main`'s mount namespace, so mounts added to
  `main` are already visible over SSH.
- No existence check for referenced InstancePersistentVolume / ConfigMap / Secret objects (matching
  today's `spec.volume.persistent` behavior).
- No change to the downstream Python product's schema in this repo (tracked as an Open Question).

## Proposal

An Instance gains two optional spec fields and the cluster gains two settings. Everything else about the
Instance lifecycle — stop/start, phases, the InstanceType and ClusterQueue watches, resource sizing —
is untouched.

### User Stories

#### Story 1
As a platform user, I want to pin an Instance to a specific node, so that it lands where a large image
or dataset is already cached, or where I am reproducing a node-specific problem.

#### Story 2
As a platform user, I want to mount existing persistent volumes at paths of my choosing (`/data`,
`/models`) besides the workspace, so that several Instances share one dataset without copying it into
each workspace.

#### Story 3
As a platform user, I want additional mounts to support read-only and sub-path, so that I can safely
expose one subdirectory of a shared volume, and inject configuration or credentials from a ConfigMap
or Secret.

#### Story 4
As a cluster administrator, I want host-path mounts and privileged mode to each require their own
explicit cluster-level opt-in, so that no Instance author can reach the node's filesystem or escape the
container boundary unless I have allowed that specific thing — I can hand out node-path mounts without
handing out privileged — while Instances already deployed keep working and stay editable if I later turn
a switch off.

### Core Features & Acceptance Criteria

#### F1 — Node pinning: `spec.nodeName`

A new optional string field naming a Kubernetes Node object.

- **AC1.1** Unset (the default) → the backing Pod carries no `nodeSelector`; the rendered Pod is
  identical to today's.
- **AC1.2** Set → the Pod's `spec.nodeSelector` carries exactly one entry, keyed
  `kubernetes.io/hostname` and valued from that **Node's own `kubernetes.io/hostname` label** (not
  assumed equal to the object name). `pod.spec.nodeName` is never written.
- **AC1.3** CREATE naming a node that does not exist → rejected with `field.NotFound("spec.nodeName")`.
- **AC1.4** CREATE naming a node that exists but does not carry every label of its InstanceType's pool
  (`instanceTypeScheduleLabels` — `feature.gpustack.ai/acceleratable`, the accelerator/general feature
  key, `kubernetes.io/os|arch` — plus `gpustack.ai/managed=true`) → rejected with
  `field.Invalid("spec.nodeName")` naming the node and the InstanceType.
- **AC1.5** The same two checks re-run on the start (resume) transition (`stop: true → false`), exactly
  as resource validation already does, so a node unlabelled or unmanaged while the Instance was stopped
  cannot silently start into permanent Pending.
- **AC1.6** `spec.nodeName` is immutable while the Instance is not stopped (`field.Forbidden`, like
  `image` / `command` / `env`) and editable while stopped.
- **AC1.7** A CREATE with `stop: true` skips the node checks, mirroring how a stopped Instance already
  skips the InstanceType lookup.
- **AC1.8** A pinned Instance still carries the Kueue queue label and is admitted through Kueue like an
  unpinned one; when Kueue admits it but the scheduler cannot place it, the reason surfaces through the
  existing `apistatus.GetSummaryOfPod` phase message — no new mechanism.

#### F2 — Additional mounts: `spec.additionalVolumes[]`

An atomic list; each entry carries a mount target plus exactly one source.

| Field | Meaning |
| --- | --- |
| `mountPath` | required, absolute container path |
| `readOnly` | optional, default `false` |
| `subPath` | optional, relative path inside the volume |
| `persistent` | `LocalObjectReference` to an InstancePersistentVolume → PVC source |
| `configMap` | `LocalObjectReference` → ConfigMap source |
| `secret` | `LocalObjectReference` → Secret source |
| `hostPath` | `core.HostPathVolumeSource` (`{path, type?}`) → HostPath source (gated, see F3) |

- **AC2.1** Unset/empty → the rendered Pod is identical to today's (`workspace`, plus
  `sshd-authorized-keys` when an SSH key is set).
- **AC2.2** N entries → the Pod gains N volumes and N `volumeMounts` **on the `main` container only**,
  with deterministic names that can never collide with `workspace` or `sshd-authorized-keys`;
  re-rendering an unchanged Instance produces an identical Pod spec (idempotent reconcile).
- **AC2.3** `readOnly: true` renders a read-only mount; `subPath` renders `VolumeMount.SubPath`.
- **AC2.4** CREATE is rejected, with the offending index in the field path, for: a non-absolute or
  duplicated `mountPath`; a `mountPath` equal to `spec.volumeMount`; zero or more than one source; an
  absolute `subPath` or one containing `..`; an empty source name or host path.
- **AC2.5** Every reference is same-namespace by construction — no cross-namespace field exists in the
  API.
- **AC2.6** `spec.additionalVolumes` is immutable while the Instance is not stopped and editable while
  stopped. (`spec.volume` stays fully immutable — the workspace is the Instance's own disk identity.)
- **AC2.7** A mount added to `main` is reachable over SSH with no sidecar change (the sidecar
  `nsenter`s into `main`'s mount namespace) — verified end to end, not by unit test.
- **AC2.8** The referenced InstancePersistentVolume / ConfigMap / Secret is **not** checked for
  existence at admission (consistent with `spec.volume.persistent`); a missing reference surfaces as
  the Pod's own event and Pending reason.

#### F3 — Administrator gates: `instance-privileged-allowed` and `instance-host-path-volume-allowed` (CREATE only)

Two new editable boolean Settings, seeded from `GPUSTACK_INSTANCE_PRIVILEGED_ALLOWED` and
`GPUSTACK_INSTANCE_HOST_PATH_VOLUME_ALLOWED`, both default `false`. They are independent: `privileged`
grants strictly more than `hostPath` (devices and the kernel surface on top of the filesystem), and the
downstream product exposes `additionalVolumes` but not `privileged`, so one combined switch would force
an administrator opening node-path mounts to also open privileged to every CR author.

- **AC3.1** `instance-privileged-allowed` `false` + CREATE with `spec.privileged: true` →
  `field.Forbidden("spec.privileged")` naming that setting.
- **AC3.2** `instance-host-path-volume-allowed` `false` + CREATE with any
  `additionalVolumes[i].hostPath` → `field.Forbidden("spec.additionalVolumes[i].hostPath")` naming that
  setting.
- **AC3.3** Each setting gates only its own field: `privileged` allowed + `hostPath` denied rejects only
  the hostPath entry, and the reverse rejects only `spec.privileged`. With both `true` neither is
  rejected and rendering is unchanged from an ungated build.
- **AC3.4** UPDATE never enforces either gate — with both `false`, an existing Instance can still be
  updated, edited while stopped, and restarted without a gate rejecting it.
- **AC3.5** Both settings appear in the `docs/settings.md` online-adjustable table and in the settings
  catalog test.

### Notes / Constraints / Caveats

- Go + controller-runtime; the Instance surface spans the CRD type (`api/worker/v1alpha1`), the
  aggregated projection (`api/worker/v1`), the admission webhook, and the reconciler. Any API edit is
  followed by `make generate` (the `gpustack-operator-generate` skill).
- Protobuf numbering is **appended only**: nothing is removed, so no renumbering and no reserved gaps.
  `spec.nodeName` is `InstanceSpec` field **8** (1-7 are taken); `additionalVolumes` is
  `InstanceTemplate` field **10** (1-9 are taken). `InstanceTemplate` is the inlined struct whose
  fields already share the "immutable while running, editable while stopped" rule, and the YAML
  surface is `spec.additionalVolumes` either way. `api/worker/v1` carries only a wrapper
  `message Instance`, so the numbering lives in one place.
- `InstanceStatus.nodeName` (field 3) already exists and reports where the Pod actually landed; the new
  `spec.nodeName` is the request side of the same identity, so the two read symmetrically.
- Source-kind shapes are deliberately asymmetric: `hostPath` uses `core.HostPathVolumeSource` verbatim
  because its two fields (`path`, `type`) are exactly what is needed, while `configMap` / `secret` are
  narrowed to a `core.LocalObjectReference` so `items` / `defaultMode` / `optional` stay out of the
  API surface. A missing ConfigMap/Secret surfaces as the Pod's own event, per AC2.8.
- `pkg/setting` caches a resolved value for 30s in a package-level cache with **no exported flush**
  (`pkg/setting/types.go:112`), and the default path never populates it. So the F3 rule is written as a
  pure helper over the two already-resolved booleans: seeding a setting to `true` in one test would
  otherwise leak into every later test in the package.
- The webhook and reconciler both read Nodes through the manager's cached client, and the worker's
  ServiceAccount binds `cluster-admin`, so the node lookups need no RBAC change.
- `InstanceSpec` is not used as a map key (unlike `InstanceTypeSpec`), so a slice field is admissible.
- Fully additive by design: the downstream Python `GPUInstanceSpec` mirrors this spec 1:1 and dumps it
  straight into the CR, so keeping `volume` / `volumeMount` intact means only optional new fields there
  and no data migration.
- The membership check reuses `instanceTypeScheduleLabels` + `systemname.ManagedLabelKey`; it does not
  duplicate the label algebra.
- `spec.privileged` is not exposed by the downstream product's Instance spec today, so defaulting its
  gate to off affects only direct CR authors. That asymmetry — the product will surface
  `additionalVolumes` but not `privileged` — is also why the two gates are separate settings: a combined
  switch would make "let product users mount a node path" imply "let any CR author run privileged".
- A Setting's persisted value is keyed by its name in the `gpustack-settings` Secret and the catalog is
  fixed, so a name that leaves the catalog leaves an orphaned key and its `true` value does not carry
  over to a replacement name. Splitting the gate later would therefore have silently tightened policy on
  upgrade; splitting up front avoids that migration entirely.
- Pool-level quota is unchanged: pinning narrows placement, it does not reserve.

### Boundaries

- **Always:** keep the rendered Pod byte-identical when both new fields are unset; validate at
  admission (fail fast with typed `field` errors) rather than in the reconciler; keep the reconcile
  level-based and idempotent; run `make generate` and `make lint` after API edits; mount additional
  volumes into `main` only.
- **Ask first:** extending either gate to the UPDATE path; restricting `hostPath` to an allowlist of node
  paths; exposing any further Pod scheduling knob (affinity/tolerations/nodeSelector map); mounting
  additional volumes into the `sshd` sidecar; validating referenced volume objects' existence.
- **Never:** write `pod.spec.nodeName` (it bypasses the scheduler and Kueue gating); introduce a
  cross-namespace volume reference; change `spec.volume`'s full immutability or `spec.volumeMount`'s
  default; reuse or renumber an existing protobuf field number; touch the InstanceType / Kueue chain.

### Risks and Mitigations

- A `hostPath` mount, or `privileged`, lets an Instance read the node's filesystem and other tenants'
  data → one default-off administrator setting each, CREATE-time rejection, documented in
  `docs/settings.md`.
- The `main` container runs as root (`RunAsUser: 0`), so an allowed `hostPath` mount of `/` is already
  total over the node's filesystem — the second gate buys separation of *kind* (filesystem vs. devices
  and kernel), not a weaker grant → recorded as an Ask-first follow-up (a `hostPath` path allowlist),
  deliberately out of scope here.
- The CREATE-only gates leave a bypass: an existing Instance can be stopped, patched to
  `privileged: true` or given a `hostPath` mount, and restarted while its gate is off → recorded as
  Open Question 1; the narrow fix (reject only `false → true` transitions on UPDATE) still never blocks
  an already-deployed Instance.
- A pinned Instance can pass pool-level Kueue quota and then sit Pending because the pinned node is
  full → surfaced through the existing Pod-summary phase message; no per-node quota is introduced.
- On some providers a Node's `kubernetes.io/hostname` label differs from its object name → the selector
  value is read from the Node's label, not assumed.
- A transient node-label gap (e.g. an `nfd-master` restart dropping custom labels) could make the
  membership check reject a legitimate pin → the check runs only at admission, so running Instances are
  unaffected, and the operator's existing recovery procedure applies.
- User-derived additional-volume names could collide with `workspace` / `sshd-authorized-keys` and
  silently shadow the workspace → names are derived by the controller (index- or hash-based), with a
  collision unit test.

## Design Details

### Commands

**Environment.** Build, test and lint all run **locally on darwin** — the whole module, CGO vendor
detectors included, compiles and tests without a Linux host. Verified while planning: the three touched
packages pass with baseline coverage 78.9% (`pkg/worker/webhooks/worker`), 63.4%
(`pkg/worker/controllers/worker`) and n/a (`pkg/worker/settings`, declaration-only). The e2e checkpoint
needs a reachable cluster, read through a copy of the kubeconfig so the user's current context is never
switched.

```bash
GODEBUG=gotypesalias=0 go test -count=1 -race \
  ./pkg/worker/webhooks/worker/... ./pkg/worker/controllers/worker/... ./pkg/worker/settings/...
make lint      # whole-module golangci-lint; a cold run needs a long timeout (~2min), ~20s warm
make build     # cross-build cmd/gpustack-operator
```

**API regeneration.** `make generate` must run from a directory whose **real** path ends in the module
import path: `gen/api` sets `ProjectDir = os.Getwd()` and the generator derives its output base by
trimming `gpustack.ai/gpustack` off it, so any other path makes protoc fail. This working tree does not
satisfy that, and the shared main checkout must not be commandeered, so generation runs in a throwaway
worktree at a **physical** path (on darwin `/tmp` is itself a symlink, which reintroduces the same
mismatch):

```bash
git diff HEAD > /tmp/improve-instance.patch
git worktree add --detach /private/tmp/gen-instance/gpustack.ai/gpustack HEAD
cd /private/tmp/gen-instance/gpustack.ai/gpustack && git apply /tmp/improve-instance.patch && make generate
# copy back into the working tree:
#   api/worker/v1alpha1/{generated.pb.go,generated.proto,generated.protomessage.pb.go,zz_generated.*}
#   api/worker/v1/{generated.pb.go,generated.proto,generated.protomessage.pb.go,zz_generated.*}
#   pkg/kubeclients/**            (one new applyconfiguration file per new struct, plus utils.go)
git worktree remove --force /private/tmp/gen-instance/gpustack.ai/gpustack
```

The fresh worktree re-downloads the gitignored `.sbin` toolchain (~30s); `staging/` is tracked, so no
`make deps` is needed. End-to-end verification of the rendered Pod, the SSH visibility of an additional
mount, and the node pin uses the `gpustack-operator-e2e` skill.

### Project Structure

```
api/worker/v1alpha1/instance.go                 # InstanceSpec.NodeName (pb 8);
                                                # InstanceTemplate.AdditionalVolumes (pb 10);
                                                # + InstanceAdditionalVolume
api/worker/v1alpha1/{generated.*,zz_generated.*}# regenerated (proto, deepcopy, CRDs, model names)
api/worker/v1/{generated.*,zz_generated.*}      # regenerated; v1.Instance is a type conversion of the
                                                # v1alpha1 struct, so no hand edit to api/worker/v1/instance.go
pkg/kubeclients/**                              # regenerated applyconfigurations for the new struct
pkg/worker/controllers/worker/instance.go       # convertPodFromInstance: one main-container builder (T1),
                                                # nodeSelector (T3), additional volumes + mounts (T4)
pkg/worker/controllers/worker/instance_type.go  # instanceTypeScheduleLabels → exported
                                                # InstanceTypeScheduleLabels, so the webhook shares the
                                                # pool-label algebra instead of restating it
pkg/worker/webhooks/worker/instance.go          # node-pin validation (T3), volume validation (T4),
                                                # the CREATE-only host-access gate (T5)
pkg/worker/settings/value.go                    # the instance-privileged-allowed and
                                                # instance-host-path-volume-allowed Settings
docs/settings.md                                # the two new Setting rows
docs/walkthrough.md                             # a worked pinned + extra-mount Instance example
```

### Code Style

The new API surface, with the declarative validation the CRD schema can carry on its own:

```go
// NodeName pins the Instance to one Kubernetes Node. The reconciler renders it as the backing Pod's
// nodeSelector on kubernetes.io/hostname — never as pod.spec.nodeName — so the scheduler and Kueue
// admission still mediate placement. The node must exist and belong to the InstanceType's pool.
//
// Immutable while the Instance is running; editable while stopped.
//
// +k8s:validation:maxLength=253
NodeName string `json:"nodeName,omitempty" protobuf:"bytes,8,opt,name=nodeName"`

// InstanceAdditionalVolume is one extra volume mounted into the Instance's main container beside its
// workspace. Exactly one source must be set.
type InstanceAdditionalVolume struct {
	// MountPath is the absolute in-container path to mount the volume at. It must not duplicate
	// another entry's path or the workspace's VolumeMount.
	//
	// +required
	// +k8s:validation:pattern="^(/[^/]+)+$"
	// +k8s:validation:maxLength=1024
	MountPath string `json:"mountPath" protobuf:"bytes,1,name=mountPath"`

	// ReadOnly mounts the volume read-only.
	ReadOnly bool `json:"readOnly,omitempty" protobuf:"varint,2,opt,name=readOnly"`

	// SubPath mounts a relative path inside the volume rather than its root.
	//
	// +k8s:validation:maxLength=1024
	SubPath string `json:"subPath,omitempty" protobuf:"bytes,3,opt,name=subPath"`

	// Persistent references an InstancePersistentVolume in the same namespace.
	Persistent *core.LocalObjectReference `json:"persistent,omitempty" protobuf:"bytes,4,opt,name=persistent"`

	// ConfigMap references a ConfigMap in the same namespace.
	ConfigMap *core.LocalObjectReference `json:"configMap,omitempty" protobuf:"bytes,5,opt,name=configMap"`

	// Secret references a Secret in the same namespace.
	Secret *core.LocalObjectReference `json:"secret,omitempty" protobuf:"bytes,6,opt,name=secret"`

	// HostPath mounts a path from the node. It crosses the node boundary, so creating an Instance
	// that uses it requires the instance-host-path-volume-allowed Setting.
	HostPath *core.HostPathVolumeSource `json:"hostPath,omitempty" protobuf:"bytes,7,opt,name=hostPath"`
}
```

The new Settings follow the existing catalog declaration verbatim:

```go
// InstancePrivilegedAllowed indicates to allow Instances to request privileged mode,
// which escapes the container boundary and exposes the node's devices and kernel surface.
// Enforced when creating an Instance only: turning it off never blocks an existing
// Instance from being updated, edited while stopped, or restarted.
InstancePrivilegedAllowed = settings.NewEditable(
	"instance-privileged-allowed",
	"Indicates to allow Instances to request privileged mode. "+
		"Enforced when creating an Instance only, "+
		"so disabling it never blocks an existing Instance from being updated or restarted.",
	setting.InitializeFromEnv("false"),
	setting.AllowBool(),
)

// InstanceHostPathVolumeAllowed indicates to allow Instances to mount hostPath volumes,
// which reaches the node's filesystem. It is separate from InstancePrivilegedAllowed
// because it grants strictly less: the filesystem, but not the node's devices or kernel.
// Enforced when creating an Instance only, like InstancePrivilegedAllowed.
InstanceHostPathVolumeAllowed = settings.NewEditable(
	"instance-host-path-volume-allowed",
	"Indicates to allow Instances to mount hostPath volumes. "+
		"Enforced when creating an Instance only, "+
		"so disabling it never blocks an existing Instance from being updated or restarted.",
	setting.InitializeFromEnv("false"),
	setting.AllowBool(),
)
```

Conventions carried from the surrounding code: exported API fields documented with behavior and
mutability; validation returns typed `field.ErrorList` entries aggregated into
`kerrors.NewInvalid`; the reconciler builds the whole Pod in one pass and stays idempotent; test files
are table-driven with a shared execution loop.

### Implementation Plan

The DAG is mostly a chain, and the edges are real rather than narrative: T3, T4 and T5 all edit the
same API type file, the same batch of regenerated artifacts, and the same webhook file, so their owned
paths intersect by nature of the work. The only genuine parallelism is **T1 ∥ T2** (and T2 ∥ T3).
Buying more would mean landing no-op stubs in separate files purely so two workers could fill them
concurrently — scaffolding for parallelism's sake, deliberately not done.

- [x] **T1 · Prefactor: one main-container builder**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/instance.go`
      Acceptance: `convertPodFromInstance` builds the `main` container exactly once — today the two
        branches of `if needSSHD` hold byte-identical copies of it — and appends the `sshd` sidecar
        when an SSH key is set. No behavior change: the rendered Pod is identical for both shapes.
        This is what makes T3 and T4 one-place edits instead of two.
      Verify: `go test -race -count=1 ./pkg/worker/controllers/worker/... -run 'TestConvertPodFromInstance|TestInstanceReconciler|TestGetResourceRequirements'`

- [x] **T2 · Settings `instance-privileged-allowed` and `instance-host-path-volume-allowed`**
      Blocked by: None
      Owns: `pkg/worker/settings/value.go`, `pkg/worker/settings/value_test.go`, `docs/settings.md`
      Acceptance: AC3.5. Two editable boolean Settings, `instance-privileged-allowed` and
        `instance-host-path-volume-allowed`, both default `"false"`, seeded from
        `GPUSTACK_INSTANCE_PRIVILEGED_ALLOWED` / `GPUSTACK_INSTANCE_HOST_PATH_VOLUME_ALLOWED`; one
        table-driven catalog test pins both names, defaults, editability and env mapping the way the
        existing switches are pinned; `docs/settings.md` gains a row for each in the online-adjustable
        table, each stating that enforcement is CREATE-only.
      Verify: `go test -race -count=1 ./pkg/worker/settings/...`

- [ ] **T3 · Node pinning tracer bullet**
      Blocked by: T1
      Owns: `api/worker/v1alpha1/instance.go`, `api/worker/v1alpha1/generated.*`,
        `api/worker/v1alpha1/zz_generated.*`, `api/worker/v1/generated.*`,
        `api/worker/v1/zz_generated.*`, `pkg/kubeclients/**`,
        `pkg/worker/controllers/worker/instance.go`, `pkg/worker/controllers/worker/instance_test.go`,
        `pkg/worker/controllers/worker/instance_type.go`,
        `pkg/worker/webhooks/worker/instance.go`, `pkg/worker/webhooks/worker/instance_test.go`
      Gate: review
      Acceptance: AC1.1-AC1.8. `spec.nodeName` added as `InstanceSpec` field 8 and regenerated;
        `instanceTypeScheduleLabels` exported as `InstanceTypeScheduleLabels` so the webhook shares it;
        the webhook rejects an unknown node with `NotFound` and an out-of-pool node with `Invalid` on
        CREATE and on the stop→start transition, and skips both when `stop: true`; the field is
        forbidden to change while running and free to change while stopped; the reconciler renders
        exactly one `kubernetes.io/hostname` selector entry from the Node's own hostname label
        (falling back to the object name when the label is absent) and never writes
        `pod.spec.nodeName`.
      Verify: the regeneration recipe above → `go test -race -count=1 ./pkg/worker/... -run Instance` → `make lint`

- [ ] **T4 · Additional volumes tracer bullet**
      Blocked by: T3
      Owns: the same paths as T3
      Gate: review
      Acceptance: AC2.1-AC2.6 and AC2.8. `additionalVolumes` added as `InstanceTemplate` field 10 with
        `InstanceAdditionalVolume` and regenerated; the webhook rejects a non-absolute or duplicated
        `mountPath`, a `mountPath` equal to `spec.volumeMount`, zero or many sources, an absolute or
        `..`-bearing `subPath`, and an empty source name or host path — each with the entry index in
        the field path; the reconciler appends N volumes and N mounts to `main` only, with names that
        cannot collide with `workspace` or `sshd-authorized-keys`, and re-renders identically; the
        field is immutable while running and editable while stopped.
      Verify: the regeneration recipe above → `go test -race -count=1 ./pkg/worker/... -run Instance` → `make lint`

- [ ] **T5 · CREATE-only host-access gates**
      Blocked by: T2, T4
      Owns: `pkg/worker/webhooks/worker/instance.go`, `pkg/worker/webhooks/worker/instance_test.go`
      Gate: review
      Acceptance: AC3.1-AC3.4. The rule is a pure helper over the two already-resolved booleans, so it
        can be table-tested exhaustively without touching the un-flushable 30s setting cache; each
        setting gates only its own field, and the wiring is covered by one test on the default-`false`
        path, which never populates that cache. UPDATE is never gated — a stopped edit and a restart
        both pass with both settings off.
      Verify: `go test -race -count=1 ./pkg/worker/webhooks/worker/...`

- [ ] **T6 · Checkpoint: docs + live e2e**
      Blocked by: T3, T4, T5
      Owns: `docs/walkthrough.md`
      Acceptance: the walkthrough gains one Instance example carrying `nodeName` and two additional
        mounts. On a live cluster: the Pod lands on the pinned node carrying the expected single
        selector while still being Kueue-admitted (AC1.2, AC1.8); both mounts exist with `readOnly`
        and `subPath` honored (AC2.2, AC2.3); a mount is visible over SSH with no sidecar change
        (AC2.7); with both settings off a privileged / hostPath CREATE is rejected (AC3.1, AC3.2),
        allowing only `instance-host-path-volume-allowed` lets a hostPath Instance through while a
        privileged one is still rejected (AC3.3), and an Instance created while they were on still
        updates and restarts (AC3.4); teardown leaves no orphan Pod or Service.
      Verify: `gpustack-operator-e2e` skill run, plus `kubectl get pod <instance> -o yaml` assertions

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None. `newInstanceWebhook(objs...)` already builds a fake client that accepts arbitrary extra objects,
so a Node fixture needs no new scaffolding, and `convertPodFromInstance` is already unit-tested
directly. T1's prefactor must keep both green **unchanged** — that requirement is the refactor's own
regression guard.

#### Unit tests

- `pkg/worker/webhooks/worker`: `2026-07-31` - baseline `78.9%`, target ≥ `80%`
- `pkg/worker/controllers/worker`: `2026-07-31` - baseline `63.4%`, target ≥ `65%`
- `pkg/worker/settings`: `2026-07-31` - declaration-only package (`0.0%` of statements); covered by the
  catalog assertions in `value_test.go` rather than by a coverage number.

Cases to cover: `nodeName` unset vs set rendering; a Node whose `kubernetes.io/hostname` label differs
from its object name; a Node missing the label entirely; an unknown node; an out-of-pool node with each
missing pool label as its own case; the `stop: true` create skip; the stop→start re-check; immutability
while running and editability while stopped — for both new fields; zero, one and many volume sources;
each of the four sources rendering its Pod volume; `mountPath` non-absolute, duplicated, and equal to
`spec.volumeMount`; `subPath` absolute and `..`-bearing; an empty source name and an empty host path;
a volume name that would collide with `workspace` or `sshd-authorized-keys`; an unchanged Instance
re-rendering identically; the gate helper across the full cross-product of `{privileged allowed?} ×
{hostPath allowed?} × {requests privileged?} × {requests hostPath?}` — which is what proves each setting
gates only its own field; and UPDATE passing while both gates are off.

#### Integration tests

None as a separate suite: the repository has no envtest harness, so the webhook and reconciler paths are
covered by the fake-client unit tests above, and the one genuinely cross-component behavior — Kueue
admitting a pinned Pod, then the scheduler placing or failing to place it — is only observable end to
end. Concrete test names are added after the implementation lands.

#### e2e tests

Run through the `gpustack-operator-e2e` skill against a reachable cluster:

1. A pinned Instance lands on the named node; its Pod carries exactly one `kubernetes.io/hostname`
   selector entry, no `pod.spec.nodeName`, and is still admitted through Kueue.
2. An Instance with a persistent mount and a hostPath mount shows both inside `main` and over SSH, with
   `readOnly` actually refusing a write and `subPath` exposing only the subdirectory.
3. Pinning to a node outside the InstanceType's pool is rejected at creation, naming the node and type.
4. With both settings off, creating a privileged or hostPath Instance is rejected; allowing only
   `instance-host-path-volume-allowed` admits the hostPath one and still rejects the privileged one; an
   Instance created while they were on can still be stopped, edited and restarted.
5. Teardown leaves no orphan Pod or Service.

## Alternatives

- **Unify into `spec.volumes[]`, dropping `volume`/`volumeMount`.** Rejected: a breaking rename that
  forces a downstream schema rename plus a data migration of persisted specs for zero functional gain,
  and the workspace's distinct lifecycle would come straight back as an `isWorkspace` flag.
- **Assign `pod.spec.nodeName` directly.** Rejected: it bypasses the scheduler, so Kueue's gating and
  the node's predicate checks never run.
- **A `spec.nodeSelector` label map instead of one node name.** Rejected: not asked for, and arbitrary
  label pinning cannot be validated against pool membership the way a node name can. A plausible later
  extension.
- **Restrict additional sources to persistent volumes only.** Considered and explicitly rejected by the
  user: ConfigMap, Secret and hostPath are all wanted, with hostPath gated.
- **Offer an ephemeral (`emptyDir`) additional source, capped by a `sum(capacity) ≤ localStorage`
  validation.** Rejected at planning time: the check would exist only to stop a runtime eviction the
  API cannot otherwise explain, and the source itself adds nothing over a subdirectory of the
  workspace. Dropping the source removes both the trap and the check.
- **No administrator gate at all.** Rejected: hostPath in a multi-tenant GPU-instance product is a node
  escape, and tightening it later would be a breaking change.
- **One combined setting `instance-privileged-host-access` for both escapes.** Initially planned, then
  rejected during the build: `privileged` grants strictly more than `hostPath` (devices and the kernel
  surface on top of the filesystem), and the downstream product exposes `additionalVolumes` but not
  `privileged` — so one switch would force an administrator opening node-path mounts to also open
  privileged to every CR author. Deferring the split was not free either: a Setting's value is persisted
  under its name in the `gpustack-settings` Secret, so a later rename would silently reset a
  previously-allowed cluster to denied on upgrade. Two switches cost one extra catalog row, one extra
  docs row and one extra table case.
- **Validate referenced volume objects' existence at admission.** Rejected: inconsistent with today's
  `spec.volume.persistent`, and it would reject a legitimate concurrent create.

## Open Questions

1. The CREATE-only gates leave a stop → patch → start bypass. Keep them exactly as decided, or tighten
   UPDATE to reject only a `false → true` transition (which still never blocks an already-deployed
   Instance)?
2. Does the downstream Python `GPUInstanceSpec` expose `nodeName` / `additionalVolumes` in the same
   release (separate repo), or is this operator-side only for now?
3. Any cap on the number of additional volumes? Proposal: none (atomic list).
4. A design cross-check (read-only Kimi red-team) was started while planning and stalled without a
   verdict, so the design has not had an independent second reading — chiefly on the Kueue/nodeSelector
   interaction and on paths that reach a running privileged/hostPath Pod without a fresh CREATE. Worth
   re-running before T5 lands.
