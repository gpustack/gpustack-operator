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

1. [Architecture](architecture.md) — the overview, so the objects make sense.
2. [Installation Modes](architecture/installation-modes.md) — what the chart owns, what the worker applies.
3. [High Availability Operations](operation/high-availability.md) — the replica knob per component.
4. [Settings & Environment Variables](settings.md) — online-adjustable settings and every `GPUSTACK_*`.
5. [Vendor Prerequisites](vendor-prerequisites.md) — what to install per manufacturer, and which vendor
   GPU Operator components to disable.
6. [Preflight Operations](operation/preflight.md) — one container run that says what a node can
   detect, slice and manage, before anything is installed on it.
7. [NVIDIA MIG Operations](operation/nvidia-mig.md), [T-Head MIG Operations](operation/thead-mig.md)
   and [Hygon MIG Operations](operation/hygon-mig.md) —
   the runbook for enabling and disabling partitioning on a node.
8. Upgrading: [Migrating to Bundled Subcharts](migration/to-subcharts.md) · [Migrating from
   v0.5.x](migration/from-v0.5.md) · [Migration Troubleshooting](migration/troubleshooting.md) when it
   goes wrong.

**I change the code** — read the overview first; it is the map for everything else.

1. [Architecture](architecture.md) → [Device Discovery](architecture/device-discovery.md) → [Network
   Topology](architecture/network-topology.md) → [Scheduling
   Chain](architecture/scheduling-chain.md) → [Admission](architecture/admission.md).
2. [Internals](architecture/internals.md) — the invariants that break quietly if you miss them.
3. [Development](development.md) — build, lint, test, code generation, vendored dependencies.
4. The [`gpustack-operator-docs`](../.claude/skills/gpustack-operator-docs/SKILL.md) skill — where a doc
   change belongs once the code change lands.

**I am an AI agent** — load in this order, stop as soon as the question is answered.

1. The `gpustack-operator-overview` skill (directory layout, naming conventions).
2. [Architecture](architecture.md) — one page, with the vocabulary table and the request trace.
3. The one deep page the question maps to, from the table below. Do not load all six.

## All pages

| Page | What it answers | Audience | Read time |
|---|---|---|---|
| [Architecture](architecture.md) | What the operator builds, the four stages, the life of one sliced-GPU request, the vocabulary | everyone | ~4 min |
| [Device Discovery](architecture/device-discovery.md) | How NFD and the Device Manager turn hardware into labels and a per-accelerator ledger; what the allocator injects | contributors | ~20 min |
| [Network Topology](architecture/network-topology.md) | The node's network interface inventory, how an RDMA link is verified, and which of those facts can reach a scheduling decision | contributors, operators | ~7 min |
| [Scheduling Chain](architecture/scheduling-chain.md) | How capacity labels become ResourceFlavors, ClusterQueues, LocalQueues and InstanceTypes | contributors | ~9 min |
| [Admission](architecture/admission.md) | The five gates, the four-view status, which field answers "what can I still get" | contributors, operators | ~8 min |
| [Installation Modes](architecture/installation-modes.md) | Chart mode vs image mode; which objects the worker must apply itself | operators, contributors | ~3 min |
| [Internals](architecture/internals.md) | Startup ordering, the gateway mirror, the device-plugin registration loop, per-manufacturer packages, CGO bindings, the 63-char rule | contributors | ~5 min |
| [KV Cache Backend](kv-cache/backend.md) | How a Mooncake store is run and observed; why capacity is read and not summed, and why shrinking a group drops its cache | operators, contributors | ~14 min |
| [KV Cache Pool](kv-cache/pool.md) | How a namespace is granted a quota on a store, what a quota ceiling buys, and why a full quota discards data instead of refusing writes | operators, contributors | ~12 min |
| [Accelerator Requests](accelerator-requests.md) | The resource keys per family and the seven rules admission enforces, with worked examples | users, contributors | ~11 min |
| [Walkthrough](walkthrough.md) | A recorded end-to-end run: every object, before/after each operation | everyone | ~12 min |
| [Settings & Environment Variables](settings.md) | Online-adjustable settings, every `GPUSTACK_*` env, per-manufacturer overrides, toolkit paths | operators | ~8 min |
| [Vendor Prerequisites](vendor-prerequisites.md) | What to install per manufacturer before GPUStack, and which vendor GPU Operator components to keep or disable | operators | ~10 min |
| [Development](development.md) | Build, lint, test, code generation, vendored subcharts and dependencies | contributors | ~5 min |
| [High Availability Operations](operation/high-availability.md) | Which knob to raise per control-plane component, and the one topology that cannot be redundant | operators | ~4 min |
| [NVIDIA MIG Operations](operation/nvidia-mig.md) | Enabling/disabling MIG, reboot recovery, and a recorded three-configuration walkthrough | operators | ~21 min |
| [T-Head MIG Operations](operation/thead-mig.md) | Enabling/disabling T-Head's own MIG-named partitioning, the busy-mode-change prerequisite, and reboot recovery | operators | ~10 min |
| [Hygon MIG Operations](operation/hygon-mig.md) | The same runbook for Hygon, whose mode is node-wide and whose partitioned nodes serve nothing else | operators | ~10 min |
| [Preflight Operations](operation/preflight.md) | Verifying on a bare host what a node can detect, slice and manage: the command line for both runtimes, every mount and flag, what it starts and removes | operators | ~9 min |
| [Migrating to Bundled Subcharts](migration/to-subcharts.md) | The one-time ownership transfer from the runtime-installed releases | operators | ~9 min |
| [Migrating from v0.5.x](migration/from-v0.5.md) | Upgrading across the scheduling-chain refactor | operators | ~5 min |
| [Migration Troubleshooting](migration/troubleshooting.md) | Recovering from a wedged upgrade (worker CrashLoopBackOff) or a namespace stuck Terminating | operators | ~8 min |
| [Instance Type Unit Resources Reference](reference/instance-type-unit-resources.md) | The per-product CPU/RAM tier a derived InstanceType is sized with, and where each tier came from | operators | reference |
| [Instance Metrics Reference](reference/instance-metrics.md) | The `instances/<name>/metrics` subresource and the Device Manager's Prometheus exporter: the fields, the gauges, their sources and limits | users, operators, console developers | ~9 min |
| [Command Reference](reference/commands.md) | Every command the binary offers: what each does, who runs it, its flags, and a runnable invocation | operators, developers | ~10 min |
| [KV Cache Injection Reference](reference/kv-cache-injection.md) | How any Pod joins a KV cache pool with one label: the contract, what is injected per engine, every refusal, and what a cache changes about a workload | users, operators | ~14 min |

## Conventions

Every page in this directory (this index excepted) carries:

- a **header block** — one-line purpose, then audience · prerequisites · read time;
- a **`## Contents`** list mirroring its `##` headings, in order;
- a **footer** — `**See also**` for sideways links and `**Next** →` for the next page on the path.

A page's file name, its H1 and its label above are the same words. Deep rationale is demoted into a
`> **Why**` note, so the rule stays skimmable. Length is not the defect, verbosity is.

Adding or moving a page means adding a row above. The
[`gpustack-operator-docs`](../.claude/skills/gpustack-operator-docs/SKILL.md) skill carries the routing
table, the sync invariants (which page a Go test or a generator pins), and `scripts/check-docs.sh`, which
fails on a broken link, a stale `## Contents`, a paragraph over the cap, a label that is not its page's
H1, or a page missing from this index.
