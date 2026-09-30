---
name: gpustack-operator-docs
description: "Write or update the GPUStack Operator documentation: route a new fact to the page that owns it, and keep each page's title, Contents, footer and the docs/README.md index in sync. Also adding a page or reviewing a docs diff."
---

# GPUStack Operator — documentation

The docs have seven capability guides, with technical pages linked from each guide. Everything in
`docs/` follows one shape and one index. Generated chart documentation is still pinned to code.
This skill routes a change to the page that owns it.

**Read the index first**: [`docs/README.md`](../../../docs/README.md) — it carries the reading paths and
the page table you must update when adding a page.

## The rule that keeps the overview small

`docs/getting-started/architecture.md` is the **front door**: what the operator builds, the four stages, one worked
request trace, the vocabulary. It is capped at ~200 lines and must stay readable in under 10 minutes.

**Never add a new mechanism, rationale or table to it.** New detail goes on a deep page; the overview
gains at most one clause and a link. If you cannot find a deep page that fits, that is a signal to add
one, not to widen the overview.

## Where a fact belongs

| Topic | Page |
|---|---|
| NFD labels, the `gpustack-cpu-info` rule, the manufacturer map | `docs/modules/devices/discovery.md` |
| Device Manager detection, the `Devices` ledger, allocator injection, cross-mode exclusion, placement | `docs/modules/devices/discovery.md` |
| The NIC/RDMA interface inventory, `pciRootId`/`pciSwitches`, the three link states, the `rdma.*` node labels, the RDMA resource keys | `docs/modules/rdma/network-topology.md` |
| Capacity labels, flavor/queue/InstanceType naming and grouping, the five reconcilers | `docs/modules/devices/scheduling.md` |
| Topograph's boundary, `TopologySource`, topology profiles, Kueue Topologies and TAS capacity semantics, and the placement preference toward nodes holding a model | `docs/modules/topology/scheduling.md` |
| Any admission gate, the four-view status, InstanceType/Instance/Pod webhook rules, drain-stop | `docs/modules/devices/admission.md` |
| Chart mode vs image mode, `disableApplications`, what the worker applies itself | `docs/operate/installation-modes.md` |
| Startup ordering, the gateway mirror, the device-plugin registration loop, per-manufacturer packages, CGO bindings, the 63-char rule | `docs/contribute/internals.md` |
| A `KVCacheBackend`'s workloads, its admin surface, its phase or capacity | `docs/modules/kv-cache/backend.md` |
| The leader process itself: its Deployment, Service, probes, health document and Lease election | `docs/modules/kv-cache/leader.md` |
| The local disk tier itself: what it renders, its bucket, its eviction, its host directory | `docs/modules/kv-cache/local-disk-tier.md` |
| Configuring a node that is mostly disk: why a group is never disk alone, the thin-segment shape, the memory floor | `docs/modules/kv-cache/disk-heavy-nodes.md` |
| A `KVCachePool` or `KVCachePoolBinding`: the grant, the reuse domain, a quota ceiling or grant, what a full quota does | `docs/modules/kv-cache/pool.md` |
| Standing a cache up end to end, or which object comes first: the pasteable four-object sequence | `docs/modules/kv-cache/walkthrough.md` |
| How a **Pod** consumes a pool: the inject label and annotations, the injected keys per engine, a refusal, the isolation record | `docs/modules/kv-cache/injection.md` |
| A `ModelArtifact`: its sources, resolution and revalidation, the manifest digest, how a `ModelDeployment` or an `Instance` mounts or downloads it, claim placement, the weight identity in KV keys | `docs/modules/model-delivery/artifact.md` |
| The `image` source of a `ModelArtifact`: the digest contract, building weights into an image, image-volume delivery, the version floors, double storage, kubelet image GC, registry mirrors | `docs/modules/model-delivery/image-source.md` |
| A `ModelStore`, `ModelStoreBinding` or `ModelPrefetch`: the grant, the budget, pinning, TTL expiry, the warm-up pod and why it is label-free | `docs/modules/model-delivery/prefetch.md` |
| The public APIs for `ModelArtifact` and `NodeModelStore`, the `progress` subresource and who may read it, the GPUStack server capability map | `docs/modules/model-delivery/views.md` |
| A `NodeModelStore` or the `model-manager` plugin: a field and its writer, the status guard, mount authorization, materialization, a failure reason, collection, a metric | `docs/modules/model-delivery/node-store.md` |
| Running node delivery: the chart values, where the node's configuration comes from, reading a node, the watermark cap, switching delivery, where replicas land and turning the preference off, upgrading, removing the cache | `docs/modules/model-delivery/operations.md` |
| The `ModelDeployment` contract: the inherited reuse domain, the three override tiers, the owned-key table, the runner-image formula, prefill/decode pairing, the topology-placement field contract | `docs/modules/model-deployment/deployment.md` |
| A `ModelDeployment` metrics snapshot, which series each field reads per engine, role and router, cache-hit scope or Pod scrape annotation | `docs/modules/model-deployment/metrics.md` |
| What a `ModelDeployment` status condition or published field means, and how to read them when a deployment misbehaves | `docs/modules/model-deployment/status.md` |
| How a prefill role and a decode role are paired: the connector each engine and router renders, `spec.router` and its fields, `spec.kvTransfer`, roles on different hardware, a role's own Service | `docs/modules/model-deployment/prefill-decode.md` |
| Which replica a managed router picks, its default routing policy, switching it through `spec.router.extraArgs`, the router's own per-replica series | `docs/modules/model-deployment/routing.md` |
| What a `ModelDeployment` replica does between its Pod's delete and its engine's exit: the drain hook, its timings, what it does not cover | `docs/modules/model-deployment/shutdown.md` |
| The lowest engine release a deployment shape runs on, the Mooncake client its runner image carries, the store line it needs, which transport each engine can use on each leg | `docs/modules/model-deployment/engine-versions.md` |
| A resource key, a request rule, a request example | `docs/modules/devices/requests.md` |
| How many RDMA endpoints a workload asks for, setting or reading the kubelet TopologyManager policy, what to do about RDMA keys no queue meters, the engine image an EFA leg needs | `docs/modules/rdma/operations.md` |
| Enabling Topograph, publishing topology snapshots, webhook trust, requesting a level, TAS diagnosis or EKS validation | `docs/modules/topology/operations.md` |
| A `Setting` or a `GPUSTACK_*` variable | `docs/reference/settings.md` |
| A make target, a subchart patch, code generation, a vendored dependency | `docs/contribute/development.md` |
| An administrator procedure (MIG mode, replicas) | `docs/modules/devices/*-mig.md`, `docs/operate/*.md` |
| An upgrade path between versions | `docs/operate/migration/*.md` |
| Recovery from a wedged upgrade or a stuck namespace deletion | `docs/operate/migration/troubleshooting.md` |
| A per-product preset value | `docs/reference/instance-type-unit-resources.md` |
| Instance resource-use metrics | `docs/reference/instance-metrics.md` |
| A subcommand, one of its flags, its exit codes, its invocation | `docs/reference/commands.md` |
| A user-visible capability, a vendor's slicing support, the install flow | `README.md` |
| Vendor prerequisites, vendor GPU Operator coexistence | `docs/getting-started/vendor-prerequisites.md` |
| A recorded run with real output | `docs/getting-started/walkthrough.md`, or the walkthrough section of `docs/modules/devices/nvidia-mig.md` |

A decision *record* — why an approach was chosen over another — belongs in `specs/`, not in `docs/`.
The docs state the rule that resulted; a `> **Why**` note carries only as much rationale as a reader
needs to not undo it.

Full routing, including what does **not** belong on a page, is in
[references/page-map.md](references/page-map.md).

## Public API versions

User-facing manifests and API calls use `worker.gpustack.ai/v1`. The `v1alpha1` types are internal
controller storage; do not ask users to switch between versions or describe public resources as
"v1 views". Document internal types only when explaining implementation under `docs/contribute/`.
An inventory snapshot's format version is separate from a Kubernetes resource API version.

## YAML examples

REQUIRED: write Pretty YAML in `README.md` and `docs/`. Expand nonempty mappings and sequences
into block style with two-space indentation, one field or list item per line. Do not embed
JSON-style objects or arrays in YAML examples, including commented alternatives. Empty collections
use `{}` or `[]`; removing their delimiters would change them to null.

When formatting is requested, preserve field order, values, scalar types, quoting, comments and
intentional invalid examples. Review the YAML diff field by field before finishing. A string whose
consumer requires JSON and a captured JSON response keep that format.

## Reader documentation and technical ownership

Apply the reader boundary in `references/conventions.md` before adding implementation detail.
User guides retain public configuration, operational behavior and constraints; the owning `specs/`
document retains source analysis, internal routes and measured implementation evidence. Compare the
actual spec contents before moving a fact, supplement missing material, and update skills that need
that technical input. A guide must not become a source-file walkthrough.

## Site rendering

Keep diagrams in fenced `mermaid` blocks so GitHub and the Hugo site render the same source.
The site shows the diagram with an expandable source block. `docs/README.md` remains the agent
index and is excluded from the site; link site readers to the module landing pages.
`make lint docs` checks source links, page structure and the rendered site's internal links.
When adding a page, add its order and short menu label to `site/data/navigation.yaml`. Put setup
and configuration before operations and diagnosis; keep related vendor procedures together.
Menu labels omit the parent module's name. Article titles and the agent index keep the full names.

## Page shape

Every page in `docs/` (the index excepted) opens with a short introduction, has a `## Contents` list
mirroring its `##` headings and a `**See also**` / `**Next**` footer. Templates and the writing rules
are in
[references/conventions.md](references/conventions.md).

## Sync invariants

These pages are not free text. Changing one side without the other breaks a build or a reader's trust.

| Doc | Pinned to | How it breaks |
|---|---|---|
| `docs/reference/instance-type-unit-resources.md` | `pkg/nodefeature/unit_resources_preset.yaml` | compare the product rows with the preset data when either changes; no Go documentation test pins this table |
| `deploy/gpustack-operator/chart/README.md`, `values.schema.json` | `values.yaml` + `README.md.gotmpl` via `make generate chart` | generated — never hand-edit; a doc path quoted in a `values.yaml` comment needs a regenerate, and `chart.yml` fails on drift |
| `README.md` accelerator matrix | `pkg/nodefeature/knowns.go` (resource names, `SharedResourceMaxSize`, `_ManufacturerPartitionKindMap`) and **whether** each `pkg/devicemanager/detector/<mfr>/device.go` sets `LogicalSliced` at all | nothing fails; the table silently lies about what a vendor can do. The matrix is deliberately Yes/— only — per-card slice counts and the per-vendor isolation mechanism live in `docs/modules/devices/discovery.md`, not on the front page |
| `README.md` Usage accelerator examples | `docs/modules/devices/requests.md` — *The resource keys* and *Worked example per family* | nothing fails; the front page and the normative contract drift apart. This copy is the one sanctioned exception to "state a fact once" (the README is the shop window) — change both together |
| `docs/modules/model-deployment/deployment.md` owned-key table | `modelDeploymentOwnedKeys` | compare the owned keys with each engine's table row when either changes; the Go documentation test was removed |
| `docs/reference/settings.md` tables | `pkg/worker/settings` and the `GPUSTACK_*` readers | nothing fails; an operator configures something that no longer exists |
| `docs/README.md` page table | the set of files under `docs/` | `check-docs.sh` fails |
| `docs/README.md` page **labels** | each page's `#` H1, character for character | `check-docs.sh` fails; a page's file name, H1 and index label are one set of words |

## Before you finish

```bash
make lint docs                                                             # the gate, as CI runs it
bash .claude/skills/gpustack-operator-docs/scripts/check-docs.sh --report   # while writing
```

It verifies relative links and `#anchor`s across `README.md`, `AGENTS.md`, `docs/**` and
`.claude/skills/**`; and — for `docs/**` only — each page's `## Contents` against its headings, a
`**See also**` footer at the end, registration in the `## All pages` table of `docs/README.md`, the
label there against the page's H1, and the three size caps (paragraph, page length, `##` count; see
`references/conventions.md`). `--report` demotes the caps to warnings and
prints the per-page metrics. The gate also resolves literal `docs/*.md` paths named across skills,
so a moved page cannot leave a skill reading a missing file.

`make lint docs` checks source links and page shape. `.github/workflows/docs.yml` also builds
the Hugo site and checks links and anchors in rendered HTML.

It still does **not** read prose: everything below is on you.

- [ ] `wc -l docs/getting-started/architecture.md` is still ≤ ~200.
- [ ] The new fact is stated **once**; every other page links to it.
- [ ] Read the diff once for the prose tells in `references/conventions.md` (*Prose tells*): not-X-but-Y
      contrasts, one-line closers, forced triads, dashes as connectors, stock AI words, bold labels.
- [ ] Touched `docs/reference/instance-type-unit-resources.md`? Compare its rows with
      `pkg/nodefeature/unit_resources_preset.yaml`.
- [ ] Touched `values.yaml`? Run `make generate chart` and commit the regenerated chart README/schema.
- [ ] Moved or renamed a page? Search tracked files for the old path and update each active reference.
      Historical specs may keep their original wording, but a path that points to a moved page must
      resolve to its new location.

## Keeping this skill honest

Found a doc that a test, a generator or a CI job pins, and it is not in the invariants table? Add the
row. Split or added a page? Add it to the routing table above and to `docs/README.md`. The tables are
the memory — they are only as good as their coverage.
