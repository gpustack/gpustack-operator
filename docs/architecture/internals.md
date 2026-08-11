# Internals

> **Purpose** — the code map and the invariants a contributor has to keep: startup ordering, the
> gateway's hand-maintained mirror, the per-manufacturer split, the CGO bindings, and the recurring
> 63-character limit.
> **Audience** contributors · **Prerequisites** [Architecture](../architecture.md) ·
> **Read time** ~10 min

## Contents

- [One binary, three subcommands](#one-binary-three-subcommands)
- [Worker startup order matters](#worker-startup-order-matters)
- [The worker gateway mirrors the cluster API, it does not embed it](#the-worker-gateway-mirrors-the-cluster-api-it-does-not-embed-it)
- [Per-manufacturer device support](#per-manufacturer-device-support)
- [CGO bindings (`binding/`)](#cgo-bindings-binding)
- [The 63-character constraint, recurring](#the-63-character-constraint-recurring)

## One binary, three subcommands

`cmd/gpustack-operator/main.go` wires a single binary with three cobra subcommands:

- **`worker`** (alias `w`, `pkg/worker`) — the control-plane process. Runs an aggregated extension API
  server *and* a controller-runtime manager in one process, and runs the scheduling-chain controllers
  (see [Scheduling Chain](scheduling-chain.md)). It can also install the bundled operator chart itself —
  see [Two install modes](install-modes.md) — but does not by default.
- **`worker-gateway`** (`pkg/workergateway`) — aggregates resources from upstream Kubernetes clusters.
- **`device-manager`** (`pkg/devicemanager`) — the per-node DaemonSet. Subcommands `serve` / `detect` /
  `monitor`: detects and monitors local accelerators, reports a `NodeFeature` + `Devices` CR, and runs
  the device-plugin allocator for device injection.

## Worker startup order matters

`pkg/worker/worker.go` runs `Prepare` (install system namespace → CRDs → extension API services →
webhook configs → settings → applications → the `gpustack-cpu-info` NodeFeatureRule → the
`gpustack-node-devices` AdmissionCheck) then `Start`. In `Start`, the controller manager is deliberately
started **only after** the extension API services report ready, so controllers can index extension-API
resources. Preserve this ordering when adding startup steps.

The last two steps are the two ends of the scheduling chain, and both **retry until their CRD is
established** (5 min bound each): a worker booting alongside the rollout that brings those CRDs reaches
them before they are served, so it waits instead of failing. They are in Go rather than in the chart
because of the boundary in [Two install
modes](install-modes.md#the-chart-deploys-workloads-the-worker-applies-the-custom-resources).

### Every `Prepare` step runs in every replica

Every step of `Prepare` runs in **all** replicas, before leader election, so each one is either
conflict-tolerant or idempotent by construction. Keep it that way when adding a step — and note that a
rolling update overlaps two replicas even where `worker.replicas` is 1.

### The two ensurers, and where they run

`Prepare`'s installation cannot outlive the boot, so `Start` also runs two ensurers — `EnsureCRDs` and
`EnsureServices` in `pkg/api` — beside the controller manager, deliberately **not** behind the
services-ready wait. A CRD or an extension API service deleted afterwards would otherwise stay gone for
the life of the process: the CRD case takes down every controller watching it, and only a restart brings
it back.

Beside the controllers rather than before them is the only place they can run — a definition already
terminating drains only once the controllers release the finalizers its custom resources hold, so
waiting for it in `Prepare` would block the release it is waiting on.

Both repair **absence only**, never the spec of an object that is there, for the overlap above: an
outgoing replica would otherwise push its own version of every object back over the incoming one, once
per interval, for as long as it lives. Aligning the spec stays with `InstallCRDs` / `InstallServices`, on
the boot of the replica that carries that version. Neither ensurer reviews a permission of its own
either, and neither may return for any reason but the context being done: a task that returns early
leaves the process running with no repair loop and nothing to say so.

> **The one residual risk, taken deliberately** — an object deleted **inside** the overlap is recreated
> by whichever replica notices first, so if that is the outgoing one, the incoming replica keeps the
> older version until some replica boots again. Nothing in the chart puts a cluster there: Helm never
> deletes CRDs on upgrade and the operator's own are installed in Go rather than from `crds/`; the
> migration hooks prune only what carries a legacy sub-release's Helm labels, which the worker's own
> APIServices do not carry at all; and `cleanup.sh` reaches them only on uninstall, behind
> `cleanupOnUninstall` (default false), where there is no incoming replica to inherit anything. It takes
> an out-of-band delete landing in that window. The alternative — realigning the spec on every tick —
> would instead have an outgoing replica fight the incoming one through *every* rolling update, which is
> both far likelier and worse.

### The one step that takes a lock

Installing the applications is the one step for which neither property was available. Helm's release
storage is a single compare-and-create, not a mutex, and two Helm actions on one release can leave it in
a pending state that no later attempt gets past.

That step therefore holds a `coordination.k8s.io` Lease — `applications.worker.gpustack.ai` in the
system namespace, via `pkg/kubeapp`'s `Lock` — for its whole duration, so exactly one replica installs at
a time. The lock is a last resort, not a pattern to copy: reach for it only where idempotence really is
out of reach, and read `pkg/kubeapp/lock.go` first for what it does and does not guarantee.

## The worker gateway mirrors the cluster API, it does not embed it

`pkg/workergateway/service` folds the `InstanceType`s of many clusters into one fleet-wide
`AggregatedInstanceType`: candidates (one per cluster) group into tiers by accelerator `OnceMaxRequest`,
and each level carries an overview bundle — a single achievable allocation copied from the winning
member — plus a `Remaining` that is the per-dimension sum.

Those overview types **re-declare** the cluster `InstanceTypeStatus`'s resource views field by field
rather than embedding them, and no generator maintains them. A view added to the CRD therefore still
compiles while the gateway never ingests, sums or serves it, and the fleet reads as having no capacity on
that dimension.

Adding one means touching `types.go` plus every aggregation site in `helper.go` (`newAggregatedTier`,
`newAggregatedCandidate`, both `Recompute` methods, `overviewResourceIsZero`).
`TestAggregatedInstanceTypeMirrorsEveryStatusView` fails while the field sets differ, but it cannot see a
missed aggregation site — walk them.

## Per-manufacturer device support

Detection (`pkg/devicemanager/detector/<mfr>`) and allocation (`pkg/devicemanager/allocator/<mfr>`) have
one subpackage per manufacturer: nvidia, amd, ascend, cambricon, hygon, iluvatar, metax, mthreads, thead.
Platform-specific code is split into `_linux.go` / `_other.go` build-constrained files.

The set of supported manufacturers and their PCI vendor IDs / resource names live in `pkg/nodefeature`
(overridable via `GPUSTACK_*` env vars, which the chart fans out from `global.manufacturers`) — what each
row of that map decides is in [Device
Discovery](discovery.md#the-gpustack-cpu-info-nodefeaturerule).

## CGO bindings (`binding/`)

Generated Go bindings to the manufacturers' GPU runtime/management libraries (nvml,
rsmi/amdsmi/amdgpu, cndev, dcmi, hgml, ixml, mtml/mxsml, hsa, dl). The generators read
`gen/binding/<runtime>/config.yaml` and emit into `binding/<runtime>/` via `make generate binding`
(c-for-go is vendored in `.sbin/`). The top-level `binding/helper*.go` files are hand-written
CPU/NUMA topology helpers — those are *not* generated.

## The 63-character constraint, recurring

Kubernetes label *values* cap at 63 chars. Long names (ClusterQueue names, queue references) are stored
in `schedule.gpustack.ai/*` **annotations**, not labels; LocalQueues are named `gpustack-fnv64-<hash>`
(always 31 chars — see [Scheduling Chain](scheduling-chain.md#nodequeueentrancereconciler-node_queue_entrancego)).
When generating any name that flows into a label value, check this limit.

---

**See also** — [Development](../development.md) (build, lint, test, code generation) ·
[Two install modes](install-modes.md) · [Scheduling Chain](scheduling-chain.md)

**Next** → [Development](../development.md) — how to build and test what you just changed.
