# GPUStack Operator Documentation

Everything written about GPUStack Operator, and the order to read it in. Start at the
[project README](../README.md) if you have not installed it yet.

## Reading paths

**I want to run it** — 30 minutes, no Kubernetes internals required.

1. [README — Quick Start](../README.md#quick-start) — install the chart, verify the chain, run a sliced
   workload.
2. [Accelerator Requests](accelerator-requests.md) — the resource keys and the rules your Pod must obey.
3. [Walkthrough](walkthrough.md) — the same steps on a real four-node cluster, with real output.

**I run the cluster** — what to configure, and what to do when hardware changes.

1. [Architecture](architecture.md) — the 8-minute overview, so the objects make sense.
2. [Two install modes](architecture/install-modes.md) — what the chart owns and what the worker applies.
3. [High Availability](operation/high-availability.md) — the replica knob per component.
4. [Settings & Environment Variables](settings.md) — online-adjustable settings and every `GPUSTACK_*`.
5. [NVIDIA MIG Operations](operation/nvidia-mig.md) — the runbook for enabling/disabling MIG on a node.
6. [T-Head PPU Partitioning Operations](operation/thead-mig.md) — the same runbook for T-Head's own
   MIG-named partitioning.
7. Upgrading: [to the bundled subcharts](migration/to-subcharts.md) · [from
   v0.5.x](migration/from-v0.5.md).

**I change the code** — read the overview first; it is the map for everything else.

1. [Architecture](architecture.md) → [Device Discovery](architecture/discovery.md) → [Scheduling
   Chain](architecture/scheduling-chain.md) → [Admission](architecture/admission.md).
2. [Internals](architecture/internals.md) — the invariants that break quietly if you miss them.
3. [Development](development.md) — build, lint, test, code generation, vendored dependencies.
4. The [`gpustack-operator-docs`](../.claude/skills/gpustack-operator-docs/SKILL.md) skill — where a doc
   change belongs once the code change lands.

**I am an AI agent** — load in this order, stop as soon as the question is answered.

1. The `gpustack-operator-overview` skill (directory layout, naming conventions).
2. [Architecture](architecture.md) — one page, includes the vocabulary table and the request trace.
3. The one deep page the question maps to, from the table below. Do not load all five.

## All pages

| Page | What it answers | Audience | Read time |
|---|---|---|---|
| [Architecture](architecture.md) | What the operator builds, the four stages, the life of one sliced-GPU request, the vocabulary | everyone | ~8 min |
| [Device Discovery](architecture/discovery.md) | How NFD and the Device Manager turn hardware into labels and a per-card ledger; what the allocator injects | contributors | ~18 min |
| [Scheduling Chain](architecture/scheduling-chain.md) | How capacity labels become ResourceFlavors, ClusterQueues, LocalQueues and InstanceTypes | contributors | ~15 min |
| [Admission](architecture/admission.md) | The five gates, the four-view status, which field answers "what can I still get" | contributors, operators | ~15 min |
| [Two install modes](architecture/install-modes.md) | Chart mode vs image mode; which objects the worker must apply itself | operators, contributors | ~8 min |
| [Internals](architecture/internals.md) | Startup ordering, the gateway mirror, per-manufacturer packages, CGO bindings, the 63-char rule | contributors | ~10 min |
| [Accelerator Requests](accelerator-requests.md) | The resource keys per family and the seven rules admission enforces, with worked examples | users, contributors | ~15 min |
| [Walkthrough](walkthrough.md) | A recorded end-to-end run: every object, before/after each operation | everyone | ~20 min |
| [Settings & Environment Variables](settings.md) | Online-adjustable settings, every `GPUSTACK_*` env, per-manufacturer overrides, toolkit paths | operators | ~10 min |
| [Development](development.md) | Build, lint, test, code generation, vendored subcharts and dependencies | contributors | ~10 min |
| [High Availability](operation/high-availability.md) | Which knob to raise per control-plane component, and the one topology that cannot be redundant | operators | ~8 min |
| [NVIDIA MIG Operations](operation/nvidia-mig.md) | Enabling/disabling MIG, reboot recovery, and a recorded three-configuration walkthrough | operators | ~30 min |
| [T-Head PPU Partitioning Operations](operation/thead-mig.md) | Enabling/disabling T-Head's own MIG-named partitioning, the busy-mode-change prerequisite, and reboot recovery | operators | ~15 min |
| [Migrating to the bundled subcharts](migration/to-subcharts.md) | The one-time ownership transfer from the runtime-installed releases | operators | ~15 min |
| [Migrating from v0.5.x](migration/from-v0.5.md) | Upgrading across the scheduling-chain refactor | operators | ~10 min |
| [Instance Type Unit Resources Preset Reference](reference/instance-type-unit-resources.md) | The per-product CPU/RAM tier a derived InstanceType is sized with, and where each tier came from | operators | reference |
| [Instance Metrics Reference](reference/instance-metrics.md) | The `instances/<name>/metrics` subresource: one current CPU/memory/disk/GPU sample, its sources, scoping and limits | users, console developers | ~6 min |

## Conventions

Every page in this directory (this index excepted) carries:

- a **header block** — one-line purpose, then audience · prerequisites · read time;
- a **`## Contents`** list mirroring its `##` headings, in order;
- a **footer** — `**See also**` for sideways links and `**Next** →` for the next page on the path.

Deep rationale — the measured failure, the alternative that was rejected — is demoted into a
`> **Why**` note so the rule itself stays skimmable.

Adding or moving a page means adding a row to the table above. The
[`gpustack-operator-docs`](../.claude/skills/gpustack-operator-docs/SKILL.md) skill carries the routing
table (which change belongs on which page), the sync invariants (which page a Go test or a generator
pins), and `scripts/check-docs.sh`, which fails on a broken link, a stale `## Contents`, or a page
missing from this index.
