# Internals

> **Purpose** — the code map and the invariants a contributor has to keep: startup ordering, the
> gateway's hand-maintained mirror, the per-manufacturer split, the CGO bindings, and the recurring
> 63-character limit.
> **Audience** contributors · **Prerequisites** [Architecture](../architecture.md) ·
> **Read time** ~4 min

## Contents

- [One binary, three subcommands](#one-binary-three-subcommands)
- [Worker startup order matters](#worker-startup-order-matters)
- [The worker gateway mirrors the cluster API, it does not embed it](#the-worker-gateway-mirrors-the-cluster-api-it-does-not-embed-it)
- [Per-manufacturer device support](#per-manufacturer-device-support)
- [CGO bindings (`binding/`)](#cgo-bindings-binding)
- [The 63-character constraint, recurring](#the-63-character-constraint-recurring)

## One binary, three subcommands

`cmd/gpustack-operator/main.go` wires one binary with the three cobra subcommands
[Architecture](../architecture.md#one-binary-three-subcommands) tabulates. Beyond that table:

- **`worker`** (alias `w`) runs an aggregated extension API server *and* a controller-runtime manager in
  one process, plus the scheduling-chain controllers ([Scheduling Chain](scheduling-chain.md)). It can
  install the bundled operator chart itself ([Two install modes](install-modes.md)), but does not by
  default.
- **`device-manager`** has subcommands `serve` / `detect` / `monitor`: it detects and monitors local
  accelerators, reports a `NodeFeature` + `Devices` CR, and runs the device-plugin allocator.

## Worker startup order matters

`pkg/worker/worker.go` runs `Prepare` (system namespace → CRDs → extension API services → webhook
configs → settings → applications → the `gpustack-cpu-info` NodeFeatureRule → the
`gpustack-node-devices` AdmissionCheck) then `Start`, where the controller manager starts **only after**
the extension API services report ready, so controllers can index extension-API resources. Preserve this
ordering when adding steps.

The last two steps are the chain's two ends, and both **retry until their CRD is established** (5 min
each): a worker booting alongside the rollout that brings those CRDs reaches them before they are
served, so it waits instead of failing. They are in Go, not the chart, per the boundary in [Two install
modes](install-modes.md#the-chart-deploys-workloads-the-worker-applies-the-custom-resources).

### Every `Prepare` step runs in every replica

Every step of `Prepare` runs in **all** replicas, before leader election, so each is conflict-tolerant
or idempotent by construction. Keep it that way when adding a step: a rolling update overlaps two
replicas even where `worker.replicas` is 1.

### The two ensurers, and where they run

`Prepare`'s installation cannot outlive the boot, so `Start` also runs `pkg/api`'s `EnsureCRDs` and
`EnsureServices` beside the controller manager, deliberately **not** behind the services-ready wait.

> **Why** — a CRD or extension API service deleted later would stay gone for the life of the process,
> and a deleted CRD takes down every controller watching it until a restart. Beside the controllers is
> also the only place they fit: a terminating definition drains only once the controllers release its
> resources' finalizers, so waiting in `Prepare` would block the release it waits on.

Both repair **absence only**, never the spec of an object that is there; aligning the spec stays with
`InstallCRDs` / `InstallServices`, on the boot of the replica carrying that version. Neither reviews a
permission of its own, and neither may return except on context done: returning early leaves the process
with no repair loop and nothing to say so.

> **Why absence only** — in that overlap an outgoing replica would otherwise push its own version of
> every object back over the incoming one, once per interval, for as long as it lives.

> **The one residual risk, taken deliberately** — an object deleted **inside** the overlap is recreated
> by whichever replica notices first, so the incoming replica can keep the older version until some
> replica boots again; nothing in the chart does that (Helm never deletes CRDs on upgrade, the
> operator's own come from Go rather than `crds/`, the migration hooks prune only what carries a legacy
> sub-release's Helm labels — which the worker's APIServices lack — and `cleanup.sh` runs only on
> uninstall behind `cleanupOnUninstall`, default false, with no incoming replica to inherit anything),
> so it takes an out-of-band delete in that window. Realigning the spec every tick would instead have
> the outgoing replica fight the incoming one through *every* rolling update: likelier, and worse.

### The one step that takes a lock

Installing the applications is the one step for which neither property was available, so it holds a
`coordination.k8s.io` Lease — `applications.worker.gpustack.ai` in the system namespace, via
`pkg/kubeapp`'s `Lock` — for its whole duration: exactly one replica installs at a time. The lock is a
last resort, not a pattern to copy; reach for it only where idempotence is out of reach, and read
`pkg/kubeapp/lock.go` for what it does and does not guarantee.

> **Why** — Helm's release storage is a compare-and-create, not a mutex, and two Helm actions on one
> release can leave it pending where no later attempt gets past.

## The worker gateway mirrors the cluster API, it does not embed it

`pkg/workergateway/service` folds many clusters' `InstanceType`s into one fleet-wide
`AggregatedInstanceType`: candidates (one per cluster) group into tiers by accelerator `OnceMaxRequest`,
and each level carries an overview bundle — one achievable allocation copied from the winning member —
plus a `Remaining` that is the per-dimension sum.

Those overview types **re-declare** the cluster `InstanceTypeStatus`'s resource views field by field
rather than embedding them, and no generator maintains them. A view added to the CRD therefore still
compiles while the gateway never ingests, sums or serves it, and the fleet reads as having no capacity
there.

Adding one means touching `types.go` and every aggregation site in `helper.go` (`newAggregatedTier`,
`newAggregatedCandidate`, both `Recompute` methods, `overviewResourceIsZero`).
`TestAggregatedInstanceTypeMirrorsEveryStatusView` fails while the field sets differ, but cannot see a
missed aggregation site — walk them.

## Per-manufacturer device support

Detection (`pkg/devicemanager/detector/<mfr>`) and allocation (`pkg/devicemanager/allocator/<mfr>`) have
one subpackage per manufacturer: nvidia, amd, ascend, cambricon, hygon, iluvatar, metax, mthreads, thead.
Platform-specific code splits into `_linux.go` / `_other.go` build-constrained files.

The supported manufacturers and their PCI vendor IDs / resource names live in `pkg/nodefeature`,
overridable via `GPUSTACK_*` env vars that the chart fans out from `global.manufacturers`. What each row
of that map decides is in [Device Discovery](device-discovery.md#the-gpustack-cpu-info-nodefeaturerule).

## CGO bindings (`binding/`)

Generated Go bindings to the manufacturers' GPU runtime/management libraries (nvml,
rsmi/amdsmi/amdgpu, cndev, dcmi, hgml, ixml, mtml/mxsml, hsa, dl). The generators read
`gen/binding/<runtime>/config.yaml` and emit into `binding/<runtime>/` via `make generate binding`
(c-for-go is vendored in `.sbin/`). The top-level `binding/helper*.go` files are hand-written CPU/NUMA
topology helpers — *not* generated.

## The 63-character constraint, recurring

Kubernetes label *values* cap at 63 chars. Long names (ClusterQueue names, queue references) live in
`schedule.gpustack.ai/*` **annotations**, not labels; LocalQueues are named `gpustack-fnv64-<hash>`
(always 31 chars — see [Scheduling Chain](scheduling-chain.md#nodequeueentrancereconciler-node_queue_entrancego)).
Check this limit for any name that flows into a label value.

---

**See also** — [Development](../development.md) (build, lint, test, code generation) ·
[Two install modes](install-modes.md) · [Scheduling Chain](scheduling-chain.md)

**Next** → [Development](../development.md) — how to build and test what you just changed.
