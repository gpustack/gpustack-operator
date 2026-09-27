# Spec: Closure verification for the model artifact line

Status: Planned
Blocked on: the five verification rows each carrying evidence or a written narrowing reason, and every issue listed below reaching its recorded disposition
Type: Bug fix

## Summary

The model artifact line (model artifact, node model store, placement preference, download
progress, prefetch, peer sync, OCI image source) closes with a verification stage: every behavior
the earlier stages shipped is re-verified on the final tree, and every defect the review rounds
surfaced is either fixed, filed as an issue, or explicitly parked with its reason written down.
Five verification rows run on real nodes — a kind cluster does not count, because its kubelet is
a container — and eight open issues get a disposition.

## Motivation

Each earlier stage verified its own head, but main moved after every merge, and two rows of the
line's acceptance matrix were verified against trees or environments that predate later work:
the claim-before-bind ordering fix was never re-run against a real kubelet, and the KV-cache
dtype and fit-label checks predate the stages that landed after them. The line's standing rule is
that every claim carries evidence on the final tree or a written reason it does not. Separately,
the review rounds and stage handbacks accumulated small defects and hardening ideas; each one
needs a disposition — fixed, filed, or parked — instead of living in a handback.

### Goals

- Re-verify the five matrix rows on real nodes and record the evidence.
- Fix the small defects the review rounds filed, each in its own pull request.
- Diagnose the two Settings-propagation reports and the chart-e2e flake far enough to fix or
  park them with the phenomenon written down.
- File the four hardening items that this stage does not fix.

### Non-Goals

- No new features. The single exception is the possible `Lapsed` condition for prefetch TTL
  expiry (issue #661), which is an Open Question below with a recorded default.
- No re-verification of rows that already carry evidence on the final tree.
- No fixes for the four hardening items themselves; this stage only files them.

## Proposal

### Verification matrix

Five rows, all on one nebius cluster with real nodes: two GPU nodes
(`gpu-h100-sxm` / `1gpu-16vcpu-200gb`) and two CPU nodes (`cpu-e2` / `2vcpu-8gb`), brought up
with the terraform under `testing/infra/clusters/nebius` and destroyed when the rows are done.
Each row's evidence lands in the stage's task directory.

| Row | Claim under test | Why it needs a re-run | Nodes | Pass criterion |
|-----|------------------|-----------------------|-------|----------------|
| A1 | A persistent-volume-claim artifact is claimed before it is bound | The ordering fix was verified on kind, whose kubelet runs in a container; a VM kubelet is the real path | CPU | The claim-bind ordering holds on a real kubelet, observed through the artifact's status transitions |
| A2 | GPU end-to-end (case-104) | The case last ran before the image-source and peer-sync stages merged | GPU ×1 | case-104 passes against a build of the final tree |
| A3 | TAS preference on accelerator nodes (case 88) | Same — predates later merges | GPU ×2 | The placement preference holds in both the fits and the does-not-fit directions |
| A4 | KV-cache dtype mixing across an engine pair with a real Mooncake store (#591) | The recheck was never run against a real store with a mixed engine pair | GPU or CPU, see the narrowing clause | The store behaves correctly under mixed dtypes and the engine pair's dtype constraint holds |
| A5 | Per-card fit labels on a real accelerator node, and NodeFeature cleanup on device-manager teardown (#625) | Fit labels were checked against a synthetic device set, not a real NVIDIA node | GPU | Labels match the device's real properties; after teardown, every label the manager wrote is gone, asserted label by label |

A4 carries a narrowing clause: if SGLang cannot run on the available nodes, the row narrows to
the store's behavior plus one engine pair's dtype constraint, and the reason is written into the
row's evidence note. A row is never silently dropped — it is either evidenced or its reason is on
record.

### Issue dispositions

One issue is a product-semantics question and is listed under Open Questions: #661 (a prefetch
whose TTL expired stays counted against `MinReady` forever).

Fixed in this stage, each in its own pull request through the merge gate:

- #662 — a warm-up pod deleted by its controller can be recreated under the same name while the
  old pod is still terminating, losing roughly ten seconds to self-healing. Fix with
  `generateName` or by waiting for deletion finality.
- #663 — a `ModelStore` does not watch its sibling stores, so `SelectorOverlap` lags until the
  next resync. Add the watch with a narrow predicate.
- #595 — an `Instance` requesting less than one CPU is admitted but never renders a pod. Either
  admission refuses it or rendering rounds it; pick one and record the choice.
- #587 — a replica that ends `Succeeded` after an eviction is never rebuilt and the deployment
  stays `Starting`. The controller covers the state.
- #583 — an S3 CSI endpoint written as an in-cluster DNS name never mounts, because geesefs
  resolves with the host resolver. Document the constraint and validate at the chart level; do
  not patch geesefs.

Diagnosed in this stage, then fixed or parked:

- #630 and #653 are the same family — a Settings change does not always reach the nodes that a
  delivery decision depends on. Diagnose the propagation timing first; the fix follows the root
  cause.
- #648 — chart-e2e case 4's `csi-nfs-controller` stalls at 1/2 on kind. Time-boxed to two hours:
  pin the subchart version or correct the assertion if the cause is found, otherwise record the
  phenomenon and park it.

Scale guard: any item that outgrows its estimate returns to the scope draft for a new
disposition instead of expanding this stage.

Filed as issues, not fixed in this stage:

- The plugin requesting `SYS_ADMIN` instead of `privileged`.
- Mounting only the kubelet subdirectories the plugin needs.
- The Collector releasing its lock when an entry is deleted.
- A scheduling constraint for workloads landing on nodes where the plugin is not registered.

### Execution shape

The coordinator runs the verification matrix directly, because cluster bring-up and teardown are
coordinator-only operations. The fixes go to implementation seats — one or two issues per seat,
each landing as its own pull request — with the coordinator reviewing the output, or are made by
the coordinator directly when the change is small. Every pull request passes the standing merge
gate: green checks, all review threads resolved, no fixup commits, and a clean merge-tree against
main.

## Acceptance Criteria

- AC1: Each of A1–A5 has evidence recorded in the stage's task directory, or a written narrowing
  or parking reason in the row's evidence note.
- AC2: #661 has a recorded decision. If the default lands, the `Lapsed` condition is implemented
  with tests and documented.
- AC3: #662, #663, #595, #587, and #583 are fixed and merged, or re-dispositioned with the reason
  recorded.
- AC4: #630 and #653 have a written root cause; the fix is merged or a follow-up issue is filed.
- AC5: #648 has a recorded outcome: fixed, pinned, or parked with the phenomenon described.
- AC6: All four hardening items exist as issues.

## Open Questions

- #661: when a prefetch's TTL expires, the prefetch stops counting toward `MinReady` and its pins
  are released, which today leaves the object indistinguishable from one that never ran. The
  default, applied if no decision arrives before execution: add a `Lapsed` condition so expiry is
  observable, and do not add re-warming — a pin means "until used", and silent re-warming would
  turn expiry into a refresh. The alternative is a `Lapsed` condition plus an opt-in re-warm
  field.
