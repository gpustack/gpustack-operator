# Spec: Model Placement Preference

Status: Shipped
Blocked on: nothing. T1 to T8 are delivered, unit-tested, and exercised end to end on kind at
Kubernetes 1.29 and 1.36; the preference on accelerator nodes is not verified.
Type: Feature

## Summary

A `ModelDeployment` replica or an `Instance` whose weights the node delivers now prefers the nodes that
already hold those weights. When the worker creates such a Pod, it adds one preferred node-affinity term
per manifest digest the Pod mounts, naming the nodes whose `NodeModelStore` reports that digest `Ready`.
The chart turns on Kueue's `TASRespectNodeAffinityPreferred` gate, the only way a preference reaches a
placement decision in this repository: Kueue's topology-aware scheduling picks the node when it reserves
quota, before kube-scheduler sees the Pod. Compute always comes first. The preference only ranks nodes
that can already take the Pod, it is never a filter, and it never causes a Pod to be Pending, evicted or
rolled. When no hot node can take the Pod, the Pod goes where it would have gone before, and that node
downloads the weights. The preference is added when the Pod is created, outside the replica's spec hash,
so a change in which nodes hold the weights never replaces a running replica. The README also stops
mixing the chart's install floor (Kubernetes 1.23) with the floor the Kueue scheduling chain needs (1.29)
([#579](https://github.com/gpustack/gpustack-operator/issues/579)).

## Motivation

### Goals

- When a hot node can take a new replica, the replica lands there and downloads zero bytes. Measured by
  an end-to-end case with a random hot node per trial, so the baseline hit rate is exactly one over the
  number of candidate nodes. The case passes only when the hit count is statistically above that
  baseline.
- When no hot node can take the replica, it is admitted on another node without waiting. Across the
  trials of that row, no Workload ever reads `QuotaReserved=False` because of the preference.
- A change in which nodes hold a digest, whether a download finishing, a collection or a node leaving,
  never deletes, recreates or re-renders a running replica. Measured as unchanged Pod UIDs and spec-hash
  annotations across a change in `NodeModelStore` status.
- Every Pod of one replica group carries the same preference. Kueue builds a replica's PodSet from one
  member's spec and silently drops the others' preferences.
- Turning the gate on changes no placement of a Pod that carries no preferred node affinity. The
  topology-aware scheduling regression case produces the same rows with the gate on and with it off, on
  a cluster with native zone labels and two nodes per zone.
- The chart still installs, and the worker still starts, on every Kubernetes version the chart's CI
  matrix covers (1.23 to 1.35).
- A reader of the README can tell which Kubernetes version installs the chart (1.23), which version the
  Kueue scheduling chain needs (1.29), and that what works in between is intended but not verified.

### Non-Goals

- Download progress, its aggregation into `ModelArtifact`, and any new field in `NodeModelStore`. This
  spec reads the status that exists and writes nothing into it.
- Fetching weights from another node. A cold node still downloads from the Hub.
- Prefetch, retention, pinned content and tenant disk budgets.
- Any hard constraint on files: no required affinity, no admission check, no waiting for a hot node.
  Files can move, compute cannot.
- A preference for `Engine` delivery or for `PersistentVolumeClaim` artifacts. An engine's download
  lands in the Pod's own storage, and a claim's placement is already decided by its volume.
- Knowing which pool a hot node belongs to. The preference names hot nodes; the scheduler ignores
  those that cannot take the Pod.
- A kube-scheduler plugin. kube-scheduler receives these Pods with the node already chosen.
- Choosing between `TASBalancedPlacement` and a Pod that also asks for a preferred topology level. No
  Pod this operator renders asks for one today (see [Rules the preference follows](#rules-the-preference-follows)).
- Changing `Chart.yaml`'s `kubeVersion`. The 1.23 install floor is intentional.

## Proposal

### How a node is chosen in this repository

These facts decide where a preference can take effect:

- Every ClusterQueue the operator builds uses topology-aware scheduling (TAS), and every topology
  profile ends in `kubernetes.io/hostname`. When Kueue reserves quota for a Workload, TAS assigns the
  Pod to a hostname and injects it as a node selector. kube-scheduler then has one feasible node and no
  choice to score.
- Kueue's Pod integration copies the Pod's whole spec into the Workload's PodSet template, including
  `affinity`. It does this whether or not the gate is on. [Measured, Kueue v0.18.9: the template's
  affinity equals the Pod's field for field.] So a preference written into the Pod when it is created
  reaches TAS without any change to how Workloads are built.
- TAS reads a PodSet's preferred node affinity only under the `TASRespectNodeAffinityPreferred` gate.
  That gate is alpha and off by default in Kueue 0.18 and 0.19. Kueue then uses the preference as a
  node score that sorts ahead of capacity. A node that cannot fit is skipped, never waited for.
  [Read: `pkg/cache/scheduler/tas_flavor_snapshot.go` at v0.18.9, lines 966, 1353-1365, 1735, 1770 and
  1897.]
- A Pod this operator renders carries no topology annotation, so its PodSet is unconstrained and TAS
  packs it onto the most-requested feasible node (LeastFreeCapacity; ties go by node name). With the
  gate on, that order is still the tie-break among nodes with equal scores.

Measured on Kubernetes 1.35 with Kueue v0.18.9 and four CPU nodes, 20 serial trials per arm, each trial
with a hot node drawn uniformly at random so the no-effect baseline is exactly 1/4:

| Arm | Hits | One-sided binomial p against 1/4 |
| --- | --- | --- |
| gate off, `TASBalancedPlacement` off | 3/20 | 0.91 |
| gate off, `TASBalancedPlacement` on (the chart's current default) | 7/20 | 0.21 |
| gate on, `TASBalancedPlacement` off | 20/20 | 9.1e-13 |
| gate on, `TASBalancedPlacement` on, the operator's Pod shape | 20/20 | 9.1e-13 |
| gate on, `TASBalancedPlacement` on, Pod annotated `podset-preferred-topology` | 3/20, all on one node | 0.91 |

With the hot node filled to 50m of free CPU and the gate on, 20/20 trials were admitted on another node,
none read `QuotaReserved=False`, and the time to reservation matched the unfilled arm (Mann-Whitney
p = 0.88). Across 140 single-Pod trials, no Workload waited.

### The preference

For each distinct manifest digest a Pod mounts through node delivery, the worker adds one term to
`spec.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution`:

```yaml
- weight: 100
  preference:
    matchExpressions:
      - key: kubernetes.io/hostname
        operator: In
        values: [gpu-node-03, gpu-node-07]   # at most 16, never empty
```

- **Candidates.** A node is a candidate for a digest when all of the following hold. Its
  `NodeModelStore` lists the digest with state `Ready`. That object's `Ready` condition is `True`. The
  node's `CSINode` lists the driver `model.csi.gpustack.ai` now. The third check exists because
  `NodeModelStore` does not say when it went stale: a node the plugin left keeps its object and a
  `Ready` condition that stops changing, and the CSI driver's registration is the record that changes
  when the plugin leaves. A digest that is `Downloading` or `Failed` on a node makes that node no
  candidate.
- **The value is the node's hostname label**, read from the Node, not the Node's name. TAS keys its
  leaves by `kubernetes.io/hostname`, and some providers set that label to something other than the
  Node's name. A candidate whose Node cannot be read from the cache, or that carries no hostname label,
  is skipped, since TAS does not treat it as a leaf either.
- **At most 16 hostnames per term**, so the size a preference adds to a Pod, and to its Workload, stays
  bounded whatever the cluster's size. When more nodes qualify, the list keeps the first 16 in this
  order: nodes where a Pod currently mounts the digest (`referenced`), then the most recent
  `lastUsedTime`, then node name. Nodes serving the digest now are the ones most likely to be in the same
  pool as the deployment's other replicas. The order is deterministic for one snapshot of the
  `NodeModelStore`s, so the members of one replica built in one pass carry the same list.
- **No candidates, no term.** A digest with no hot node adds nothing, and the Pod is created exactly as
  before this spec.
- **One term per digest, weight 100 each.** A `ModelDeployment` mounts one artifact, so its Pods carry
  one term. An `Instance` can mount several artifact volumes. Summing one term per digest makes a node
  holding more of them score higher, and a node holding all of them score highest. Weight 100 is the
  weight the measurement used. With one term it has no effect on TAS, which compares scores only
  against each other.
- **Appended, never replacing.** Terms already on the Pod are kept. Neither `ModelDeployment` roles nor
  `Instance`s expose affinity today, so in practice the injected terms are the only ones.

### Where it is written, and why not in the spec hash

The worker adds the preference in its own controllers, at the moment it creates the Pod, and not in a
Pod admission webhook:

- **`ModelDeployment`.** The reconciler renders each replica member, stamps the spec-hash annotation,
  and then creates a copy of that render. The preference is added to the copy, next to where the
  claim's required affinity is added today. The spec hash is computed from the render before this
  point, so it never covers the preference, and the rollout comparison reads only that annotation. The
  reconciler computes the candidate list once per pass, before it creates any member, and not at all
  in a pass that creates none, since it walks every `NodeModelStore`. Every member of every replica
  that pass creates carries the same term, which is what Kueue's one-template-per-PodSet rule needs.
- **`Instance`.** The reconciler renders the Pod once, at creation, and never compares it again. The
  preference is added when the Pod is built, next to the claim's required affinity. It covers each of
  the Instance's node-delivered artifact volumes.
- **Watching.** A `ModelDeployment` is already enqueued when a `NodeModelStore` it depends on changes its
  models, and that pass must not touch running replicas. Nothing new is watched. A preference goes stale
  after its Pod is created, and that is harmless: it matters only once, when Kueue places the Pod. A Pod
  Kueue evicts and requeues keeps the old list, and a Pod the reconciler recreates gets a fresh one.

Why not a Pod webhook: it would add a synchronous dependency to the creation of every such Pod, and each
member of a replica would be admitted in its own call, so members could read different snapshots of the
`NodeModelStore`s. Why not in the render: the render feeds the hash, and a hash that covered the
preference would recreate every replica each time a download finished anywhere (see
[Alternatives](#alternatives)).

The preference also tells the tenant something new: its own Pod names nodes that hold weights its own
namespace resolved. It never names the artifact, repository or tenant that put them there, and it never
covers a digest the namespace did not resolve. This mirrors the mount rule, where a mount is authorized
by a resolved `ModelArtifact` in the Pod's namespace, never by what a Pod claims.

### Rules the preference follows

- **Node delivery only.** A `ModelDeployment` whose resolved delivery is `Engine`, or whose artifact is a
  claim, gets no term. An `Instance` gets terms only for its Hugging Face artifact volumes, which the
  node always delivers.
- **Never together with `kueue.x-k8s.io/podset-preferred-topology`.** With `TASBalancedPlacement` on, a
  PodSet carrying that annotation loses its node-affinity score (measured 3/20, with every trial on one
  node; the upstream issue is kubernetes-sigs/kueue#15334). The chart keeps `TASBalancedPlacement` on. A
  Pod that already carries the annotation gets no term, so its affinity never claims a preference TAS
  would ignore. No Pod this operator renders carries it: a `ModelDeployment` role writes only
  `kueue.x-k8s.io/podset-required-topology`, and only when `topology.requiredLevel` is set. A unit test
  pins both halves of this rule.
- **A replica group shares one preference.** Members of one replica created in one pass carry identical
  terms. A member created in a later pass, after an earlier create failed, can carry a newer list. Kueue
  then uses one member's list. Either list names nodes that were hot, so the result is still a valid
  preference and never a constraint.
- **Compute first.** The preference changes the order of nodes that already fit the Pod, never which
  nodes fit. When a group does not fit on one hot node, the part that fits goes there and the rest goes
  wherever capacity allows. That is Kueue's per-PodSet behavior, and this spec adds nothing to it.

### The Kueue gate

The chart's `kueue.managerConfig.controllerManagerConfigYaml` default gains
`TASRespectNodeAffinityPreferred: true` beside `TopologyAwareScheduling` and `TASBalancedPlacement`.
That string is a single value, so the change is to the chart's default itself, and the values schema
generated from it follows. The chart README shows the string by name only, so it does not change.

- **Image mode** installs the chart packaged in the worker's image with its default values, so it gets
  the gate with no change of its own.
- **An administrator who overrides the string** keeps their own gates: Helm replaces a string value
  whole. The upgrade note says to add the line.
- **A cluster that runs its own Kueue** (`kueue.enabled=false`) owns its own gates. With the gate off,
  the preference is inert: placement is exactly as before and the Pod carries terms nothing reads.
- **Turning the gate on also affects other Pods**: any Pod in a TAS queue whose author wrote a preferred
  node affinity gets it honored, where before it was silently ignored. This is the documented meaning of
  the field. The documentation states it.
- **Turning it off** (the administrator overriding the string with the line set to `false`) returns
  every placement to the measured gate-off behavior. It is the only switch for this feature: there is no
  Setting, and the cap of 16 hostnames is a constant. Turning it off also stops Kueue from honoring every
  other author's preferred node affinity in a TAS queue, since the gate is Kueue's, not this feature's.
- **The gate is alpha** in Kueue 0.18 and 0.19. The documentation and the upgrade note say so, and say
  how to turn it off.

### Two version floors

The README's prerequisites stop reading "Kubernetes `>= 1.23` (required by the bundled Kueue)", which
gets the attribution backwards. They state:

- **1.23 installs the chart.** `Chart.yaml` declares `>=1.23.0-0` deliberately, and the chart workflow
  installs it on kind images from 1.23.17 to 1.35.5.
- **1.29 runs the Kueue scheduling chain.** The bundled Kueue v0.18 requires Kubernetes 1.29 or newer.
- **Between 1.23 and 1.28** the intended use is simple allocation by the default scheduler with the
  device manager's allocator, without the Kueue chain. No test in this repository verifies what works
  there, and the README says that rather than listing capabilities.

### User Stories

#### Story 1

As a model deployment user, I want a new replica of my deployment to start on a node that already holds
its weights whenever such a node has room, so that scaling up and replacing replicas skip a download
that can take minutes.

#### Story 2

As a model deployment user, I want a replica to start on another node at once when the nodes holding the
weights are full, so that a warm cache never makes my deployment wait for GPUs.

#### Story 3

As a cluster administrator, I want the set of nodes that hold a model to change as the cache fills and
collects, without my running replicas being recreated, so that caching never costs a reload.

#### Story 4

As a cluster administrator, I want turning this on to leave the placement of every Pod that asks for no
preference as it was, and to have one switch that turns it off, so that I can adopt it without auditing
every queue.

#### Story 5

As an operator installing GPUStack on an older cluster, I want the README to tell me which Kubernetes
version installs the chart and which one the full scheduling chain needs, so that I know what I get on
my version.

### Core Features & Acceptance Criteria

#### F1 - The candidate list

- For one digest, the candidates are exactly the nodes whose `NodeModelStore` lists it `Ready`, whose
  `Ready` condition is `True`, and whose `CSINode` lists the driver. A table-driven test covers each of
  these conditions failing alone, with every other condition held true. One case is the stale object:
  the `NodeModelStore` still reads `Ready` with a `Ready` condition, and the node's `CSINode` no longer
  lists the driver, and no term is added.
- Values are hostname labels read from the Nodes. A Node whose label differs from its name yields the
  label. A missing Node, or one without the label, yields nothing.
- The list is capped at 16 in the stated order, and it is identical for identical inputs regardless of
  list order.
- An empty candidate list adds no term.

#### F2 - Injection into `ModelDeployment` replicas

- A node-delivered deployment's newly created members carry one term with weight 100 for the
  deployment's digest. `Engine` and claim deployments carry none.
- Every member of every replica created in one pass carries an identical term.
- The spec-hash annotation of a created member equals that of its render, whatever the candidate list
  is. A pass after the candidate list changed reports every existing replica current and deletes and
  creates nothing.
- A claim deployment's members carry the claim's required affinity and no preference.
- A Pod carrying `kueue.x-k8s.io/podset-preferred-topology` gets no term. No render of any role shape
  carries that annotation.

#### F3 - Injection into `Instance` Pods

- An Instance with one or more Hugging Face artifact volumes carries one term per distinct digest that
  has candidates. An Instance with only claim volumes, or none, carries none. An Instance with a claim
  volume and a Hugging Face volume carries the claim's required affinity and the artifact's term.

#### F4 - The chart

- The rendered Kueue configuration has `TASRespectNodeAffinityPreferred: true`, with
  `TopologyAwareScheduling`, `TASBalancedPlacement` and `QuotaCheckStrategy` unchanged. The generated
  schema and chart README agree with `values.yaml`. `make lint chart` passes.
- The chart workflow's install test passes on all seven of its node images, from 1.23.17 to 1.35.5,
  with the operator image built from this branch.

#### F5 - Documentation

- `docs/architecture/topology-aware-scheduling.md`, the page on how TAS places a replica, explains
  where the preference comes from, why TAS and not kube-scheduler consumes it, the gate, and the rule
  on `podset-preferred-topology`; `docs/architecture/scheduling-chain.md` points to it.
- `docs/operation/model-store.md` tells an administrator how to read where replicas landed and why, how
  to turn the preference off, and what the upgrade changes for Pods that already write a preferred
  affinity.
- The README states both floors per [Two version floors](#two-version-floors); #579 is closed by the PR.
- The docs index and the documentation skill's routing table list any new section or page.

#### F6 - End to end

On kind: one control-plane node and four workers, with zone and region labels set in the kind
configuration so that each zone has two workers. Each case runs on a Kubernetes 1.29 node image and on
the current default image.

- **Hot node preferred.** Twenty serial trials. Each trial warms a fresh digest on a worker drawn at
  random and then creates a consumer. The hit count must reject the 1/4 baseline with a one-sided exact
  binomial p below 0.001 (at least 12 of 20). As a check on the instrument, the same row is run once
  with the gate off, and it must fail. That run's output is kept as evidence, not as a standing row.
- **Hot node full.** The hot node is filled so the consumer cannot fit there. Every trial is admitted on
  another node, and no Workload ever reads `QuotaReserved=False`.
- **No rollout.** A running node-delivered `ModelDeployment`, with a replica of size two, sees its
  digest become `Ready` on another node. Across an interval afterwards, its Pod UIDs and spec-hash
  annotations are unchanged, and both members carry identical terms.
- **No `podset-preferred-topology`.** Every Pod that carries an injected term lacks the annotation.
- **Gate-on regression.** Case 87 runs with the gate on and with it off on the zoned cluster, and its
  rows match one for one.

### Notes / Constraints / Caveats

#### Platform capabilities and the version floor

| Capability | Earliest default-on version | Source |
| --- | --- | --- |
| Pod `preferredDuringSchedulingIgnoredDuringExecution` with `matchExpressions` | before 1.23 (core/v1) | Kubernetes API reference |
| `CSINode` `storage.k8s.io/v1` `spec.drivers` | 1.17 (GA) | Kubernetes API reference |
| `NodeModelStore` | this repository, with node delivery | the node model store spec |
| Kueue TAS copying Pod affinity into the PodSet template | Kueue v0.18.9, the bundled version | measured; `pkg/controller/jobs/pod/pod_controller.go:776` |
| `TASRespectNodeAffinityPreferred` | alpha, off by default, from Kueue 0.18; still alpha in 0.19 | `pkg/features/kube_features.go` at v0.18.9 and v0.19.5 |
| Kueue v0.18 itself | Kubernetes 1.29 | Kueue installation guide |

This spec adds no Kubernetes API the worker did not already read. The preference works where node
delivery works, which is 1.29 and later. On 1.23 to 1.28 the worker's new code only lists
`NodeModelStore`s and reads Nodes and `CSINode`s, all of which exist there. The chart's new default is
a Kueue-internal setting, and the chart workflow's seven images show whether Kueue still starts with it.

#### Implementation constraints

- The candidate list is a pure function of the `NodeModelStore`, `CSINode` and Node objects it is
  given. The reconcilers read those objects from the manager's cache, whose `NodeModelStore` and
  `CSINode` informers already exist for node delivery.
- The injection sits next to `injectModelArtifactAffinity` and follows its rule: added at creation, not
  rendered.
- The tests are table-driven with the fake client. The end-to-end cases reuse the test Hub of the node
  delivery cases, whose repositories are generated from their names, so every trial gets a fresh
  digest without network access.
- `pkg/worker/controllers/worker/instance.go` is also changed by the fix for #590, the persistent
  volume's node affinity. The Instance task is built after that fix merges, on a branch rebased onto it.

### Boundaries

- **Always:** add the preference at creation and outside the spec hash; keep compute first; leave every
  existing term on the Pod; skip a Pod that asks for a preferred topology level; read hot nodes only
  from `NodeModelStore` status; run every end-to-end row on 1.29 and on the default image.
- **Ask first:** any change that makes a file location a constraint, or that moves the injection into
  the hash; a new Setting; turning `TASBalancedPlacement` off; any change to `Chart.yaml`'s
  `kubeVersion`; a real cluster or GPU nodes.
- **Never:** write into `NodeModelStore`; name a tenant, artifact or repository in any cluster-scoped
  object; delete or recreate a Pod because of where weights are; wait for a hot node.

### Risks and Mitigations

- An alpha Kueue gate changes other placements → the regression case runs the TAS proofs with the gate
  on and off and compares them row for row; the documentation names the one switch that reverts it.
- The candidate list points at a node where the plugin no longer runs, so a Pod steered there could not
  mount → a candidate needs the driver in its `CSINode` now, not only a `Ready` object.
- More than 16 nodes hold a digest, and the 16 kept lie outside the deployment's pool → the order keeps
  nodes that serve the digest now; the worst case is no locality benefit, which is today's behavior.
- Members of one replica carry different lists after a partial create failure → Kueue uses one member's
  list, and either is a valid preference; documented.
- A Pod later written with `podset-preferred-topology` loses the preference silently → such a Pod gets
  no term, and a unit test pins that no render writes the annotation.
- The preference grows a Pod and its Workload → at most 16 hostnames per digest.
- The gate is alpha, so a Kueue upgrade can rename or remove it, and Kueue refuses to start on a
  feature gate it does not know → bumping the bundled Kueue already goes through the subchart-management
  procedure; its checklist gains reading this gate in the new version's `kube_features.go`, and the
  chart's comment beside the gate says why it is there.

## Design Details

### Commands

```sh
go test ./pkg/worker/...
make lint </dev/null                 # edit pass; compare the tree before and after
make lint docs </dev/null            # after docs/, specs/ or README edits
make lint chart </dev/null           # after chart edits
```

The end-to-end cases run on a kind cluster of this spec's own, with its kubeconfig in a scratch file so
the user's kubeconfig is never read or written, and an operator image built from the branch and loaded
into kind:

```sh
kind create cluster --name <cluster> --kubeconfig "$KCFG" --image kindest/node:v1.29.14 --config <four-worker, two-zone config>
KUBECONFIG="$KCFG" bash .agents/skills/_e2e-lib/scripts/build-load.sh <TAG>
KUBECONFIG="$KCFG" bash .agents/skills/_e2e-lib/scripts/deploy.sh gpustack-system <TAG>
KUBECONFIG="$KCFG" bash .agents/skills/gpustack-operator-e2e/cases/case-<N>.sh <NS>
kind delete cluster --name <cluster> --kubeconfig "$KCFG"
```

The chart workflow's install test is run locally on each of its seven node images, changing only the
image tag to this branch's build.

### Project Structure

- `pkg/worker/controllers/worker/model_placement_preference.go` — the candidate list and the injection;
  its test file beside it.
- `pkg/worker/controllers/worker/model_deployment.go` — the pass computes the list once and adds it to
  each created member.
- `pkg/worker/controllers/worker/instance.go` — the Pod built for an Instance gets its terms.
- `deploy/gpustack-operator/chart/values.yaml`, `values.schema.json`, `README.md` — the gate.
- `README.md`, `deploy/gpustack-operator/chart/README.md.gotmpl` — the two floors.
- `docs/architecture/topology-aware-scheduling.md`, `docs/architecture/scheduling-chain.md`,
  `docs/operation/model-store.md` — the documentation.
- `.agents/skills/gpustack-operator-e2e/cases/` — the new case and its kind configuration.

### Code Style

Decisions are pure functions of the objects they are given; the reconciler reads, calls, and writes.

```go
// modelPlacementPreference returns the preferred node-affinity term naming the nodes that hold digest
// ready to mount, or nil when none does.
//
// A PREFERENCE, NEVER A FILTER. Kueue's topology-aware scheduling reads the term as a node score among
// the nodes that already fit the Pod, so a wrong or stale entry costs at most one download on another
// node. For the same reason a read that fails yields no term rather than an error.
func modelPlacementPreference(ctx context.Context, cli ctrlcli.Reader, digest string) *core.PreferredSchedulingTerm

// modelPlacementHostnames orders candidates and returns the hostnames of the first 16.
func modelPlacementHostnames(candidates []modelPlacementCandidate) []string
```

Other conventions: comments state the rule and its reason, with no task identifiers; tests are
table-driven with fake clients and assert the created Pods; files are named in snake_case.

### Implementation Plan

Tasks are built one at a time in this order, each starting with `git fetch origin && git rebase
origin/main`, and each leaving the tree compiling with its tests green. Checkpoints: after T2 the
`ModelDeployment` path is complete and unit-proven; after T5 the gate-on regression has answered, and a
difference there stops the build (see Open Questions); T6 waits for the fix to #590 to merge.

The candidate list is a method of the resolved weights, `placementPreference`, which both consumers
call only when they create Pods: a `ModelDeployment` pass once before its first create, an `Instance`
when its Pod is built. A pass that creates nothing reads no `NodeModelStore`.

- [x] **T1 · The candidate list, carried on the resolved weights**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_placement_preference*.go`,
      `pkg/worker/controllers/worker/model_artifact_placement.go`,
      `pkg/worker/controllers/worker/model_artifact_placement_test.go`
      Gate: review
      Acceptance: `modelPlacementPreference` reads the `NodeModelStore`s, the `CSINode`s and the Nodes
      from the reader it is given, and the order and cap are the pure `modelPlacementHostnames`. It
      meets every F1 criterion, including
      the stale-object case. `injectModelPlacementPreference(pod, term)` appends the term, adds nothing for
      a nil term, and adds nothing to a Pod annotated `kueue.x-k8s.io/podset-preferred-topology`.
      `placementPreference` on the resolved weights returns a term for `Node` delivery only, reading the
      objects from the reader it is given, and nil for `Engine`, claims, blocked results and no
      candidates.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestModelPlacement'`,
      with each new test name appearing in `-v` output; `make lint`.
- [x] **T2 · `ModelDeployment` members carry the preference, outside the hash**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/model_deployment.go`,
      `pkg/worker/controllers/worker/model_deployment_placement_preference_test.go`
      Gate: review
      Acceptance: F2. The reconciler adds the pass's preference to each created member next to
      `injectModelArtifactAffinity`. The test drives a node-delivered deployment through the fake client:
      the members of a size-two replica created in one pass carry identical terms; the created Pod's
      spec-hash annotation equals the render's; after the `NodeModelStore`s change, the next pass
      deletes and creates nothing and reports the replicas current; a claim deployment's members carry the
      required affinity and no preference; no render of the role shapes in the existing render tests
      carries `podset-preferred-topology`.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestModelDeploymentPlacementPreference'`,
      names checked in `-v` output; `go test ./pkg/worker/...`; `make lint`.
- [x] **T3 · The chart turns the gate on**
      Blocked by: None
      Owns: `deploy/gpustack-operator/chart/values.yaml`, `deploy/gpustack-operator/chart/values.schema.json`,
      `deploy/gpustack-operator/chart/README.md`,
      `.agents/skills/gpustack-operator-chart-subcharts-manage/SKILL.md`
      Gate: review
      Acceptance: F4's first criterion. `TASRespectNodeAffinityPreferred: true` sits beside the other
      gates with a comment saying it is alpha, what reads it, and that turning it off also stops Kueue from
      honoring other authors' preferred affinity. `make generate chart` leaves the schema and README
      matching `values.yaml`. The subchart skill's Kueue-bump checklist names this gate as one to find in
      the new version before bumping.
      Verify: `make generate chart` then `git status --porcelain deploy/gpustack-operator/chart` lists only
      the intended files; `helm template` of the chart shows the gate in the rendered manager ConfigMap;
      `make lint chart`; `make lint docs` for the skill page.
- [x] **T4 · The end-to-end case**
      Blocked by: T2, T3
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-107.sh`,
      `.agents/skills/gpustack-operator-e2e/references/kind-two-zones.yaml`,
      `.agents/skills/gpustack-operator-e2e/SKILL.md`
      Gate: review
      Acceptance: case 107 follows the case header contract and prints the `STATUS | CHECK | OBJECT`
      table. Its rows are the F6 rows other than the case 87 regression: hot node preferred (20 trials,
      fresh digest per trial from the test Hub, hot worker drawn at random with a printed seed, a bare Pod
      pinned to that worker warms it, a single-replica `ModelDeployment` on `registry.k8s.io/pause`
      consumes it; PASS at 12 or more hits); hot node full (the hot worker filled by a placeholder Pod
      whose request leaves less than one consumer, every trial admitted elsewhere, no Workload ever
      `QuotaReserved=False`); no rollout (a size-two replica, its digest made `Ready` on another worker,
      Pod UIDs and spec-hash annotations unchanged across two intervals, identical terms on both members);
      no `podset-preferred-topology` on any Pod carrying a term. It prints the gate as read from Kueue's
      ConfigMap and the running controller's log, so a run states which arm it measured. The kind
      configuration gives four workers, two per zone, with region and zone labels. The skill's case table
      lists case 107 and what triggers it.
      Verify: `bash -n` on the new files; `make lint agents-shell`; one run on a kind cluster of the
      two-zone shape prints every row (the full runs are T5 and T7).
- [x] **T5 · Early evidence on kind: the gate-on regression and the first case 107 runs**
      Blocked by: T4
      Owns: nothing in the tree; evidence goes to the run's report directory
      Gate: review
      Acceptance: on kind 1.29.14 and on kind's default image, both with the two-zone configuration and
      an image built from the branch head: case 107 passes every row; the same case with the gate turned
      off by a values override that changes only that line fails the hot-node row (instrument check);
      case 87 runs with the gate on and with it off and its rows match one for one. The imageID of the
      worker Pod matches the image loaded. A difference in case 87 stops the build and is reported.
      Verify: each case's exit code and row table saved per run; a row-by-row diff of the two case 87
      tables is empty.
- [x] **T6 · `Instance` Pods carry the preference**
      Blocked by: T1, the merge of the fix for #590
      Owns: `pkg/worker/controllers/worker/instance.go`,
      `pkg/worker/controllers/worker/instance_placement_preference_test.go`,
      `.agents/skills/gpustack-operator-e2e/cases/case-107.sh`
      Gate: review
      Acceptance: F3. The Pod built for an Instance gets one term per distinct node-delivered digest with
      candidates, next to the claim's required affinity; claim-only and volume-less Instances get none.
      Case 107 gains one row: an Instance naming a warmed artifact carries the term for its digest.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestInstancePlacementPreference'`, names
      checked in `-v` output; `go test ./pkg/worker/...`; `make lint`; `make lint agents-shell`.
- [x] **T7 · Documentation and the two floors**
      Blocked by: T2, T3
      Owns: `README.md`, `deploy/gpustack-operator/chart/README.md.gotmpl`,
      `deploy/gpustack-operator/chart/README.md`, `docs/architecture/topology-aware-scheduling.md`,
      `docs/architecture/scheduling-chain.md`, `docs/operation/model-store.md`, `docs/README.md`,
      `.agents/skills/gpustack-operator-docs/SKILL.md`
      Gate: None
      Acceptance: F5. The README states the two floors and closes #579's "does not count" list item by
      item, and the chart README's prerequisites name the 1.29 floor too. `topology-aware-scheduling.md`
      gains a section on the preference within its `##` budget, and `scheduling-chain.md` points to it;
      `model-store.md` gains where replicas land and why, the switch and its side effect, and an upgrade
      note that the gate is alpha and on by default now. The index and routing table name any new section
      a reader would search for.
      Verify: `make lint docs`.
- [x] **T8 · Final verification on the rebased tree**
      Blocked by: T5, T6, T7
      Owns: nothing in the tree
      Gate: review
      Acceptance: rebased onto `origin/main`, on kind 1.29.14 and the default image with an image built
      from the head: case 107 passes every row, including the Instance row; the chart workflow's install
      test passes on all seven of its node images with `image.tag` set to this build and nothing else
      changed in the harness; `go test ./pkg/worker/...`, `make lint`, `make lint chart` and
      `make lint docs` pass. Case 87 is run again only if the chart or anything Kueue reads changed since
      T5.
      Verify: exit code 0 recorded per run and per image; the tree hash of the tested head recorded.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make
this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

- A kind configuration of one control plane and four workers, two per zone, with
  `topology.kubernetes.io/region` and `topology.kubernetes.io/zone` labels, which case 87 needs and no
  local cluster shape had until now.
- The test Hub of the node delivery cases is reused as it is; its repositories are generated from their
  names, so twenty trials get twenty digests.

#### Unit tests

- `gpustack.ai/gpustack/pkg/worker/controllers/worker`: `2026-09-26` - `model_placement_preference.go`
  93.8% of statements (every branch but the logging of a failed read), `resolveModelArtifactWeights`
  88.9%, `convertPodFromInstance` 87.8%.
  - The candidate filter, table-driven, one condition failing per case: entry not `Ready`
    (`Downloading`, `Failed`), object's `Ready` condition missing or `False`, the driver absent from
    `CSINode` while the object still reads `Ready` (the stale object), Node missing, hostname label
    missing, hostname label differing from the Node name.
  - The order and the cap: 20 candidates keep the 16 the order names; shuffled input gives the same
    output.
  - The injector: nil term, an existing preferred term kept, a Pod with `podset-preferred-topology`
    untouched.
  - Resolution: `Node` delivery yields a term; `Engine`, claims, blocked results and zero candidates
    yield none. A `ModelDeployment` pass that creates nothing reads no `NodeModelStore`, and the
    filter's condition type equals the one the plugin writes.
  - `ModelDeployment`: identical terms across the members of a replica created in one pass; the
    created Pod's hash equal to the render's; no delete or create after the candidates change, with
    every replica reported current; a claim deployment's required affinity and no preference; no render
    carries `podset-preferred-topology`.
  - `Instance`: one term per distinct node-delivered digest, in volume order; none for claims or for
    an Instance without volumes; a claim's required affinity beside a hub artifact's term.

#### Integration tests

None beyond the fake-client reconciler tests above; the repository has no envtest suite for these
controllers.

#### e2e tests

- Case 107 on kind, two zones, on 1.29.14 and the default image: hot node preferred (at least 12 of 20,
  one-sided exact binomial p < 0.001 against 1/4), hot node full (every trial elsewhere, never
  `QuotaReserved=False`), no rollout, no `podset-preferred-topology`, and the Instance row after T6.
- The instrument check: the hot-node row, run once with the gate off, fails. Its output is evidence, not
  a standing row.
- Case 87 with the gate on and off on the same cluster, rows compared one for one.
- The chart workflow's install test on its seven node images, 1.23.17 to 1.35.5.
- Not run: the TAS preference on accelerator nodes (case 88 needs them). The preference is not verified
  on accelerator nodes; that gap is recorded in the summary.

## Alternatives

### A Pod mutating webhook

A webhook would also see Pods the operator does not create. It was rejected for three reasons. It would
put a synchronous call into the creation of every such Pod. Each member of a replica would be admitted
in its own call and could read a different snapshot, which is exactly the case where Kueue drops all but
one member's preference. And it would have to learn the digest from the Pod's volume attributes, which a
tenant writes and which this design treats as hints only. The controllers already hold the resolved
artifact and are the only creators of these Pods.

### Render the preference into the Pod

Rendering is simpler and keeps the Pod equal to its render. But the render feeds the spec hash, so every
completed download, collection or node change anywhere would recreate every replica of every deployment
on that digest. This violates the rule that the preference never causes a rollout.

### Match the Node name with `matchFields`

`metadata.name` is exact without reading Nodes. But a node field selector with `In` accepts exactly one
value, so N nodes would need N terms, and the measured shape was a hostname `matchExpressions` term.
Reading the hostname label from the cached Node costs nothing.

### Prefer nodes that are still downloading

A second replica on a node that is already downloading would share that download. It was left out
because a failing download would then attract Pods to a node that may never serve them. The design
already states that only `Ready` content counts.

### A Setting to turn the preference off, or to set the cap

The earlier design listed `model-placement-preference`. The Kueue gate already turns the effect off, and
a Setting would add a second switch whose off state matters only when the gate is on and nothing else
uses preferred affinity. It is left out until someone needs a state the gate cannot express. See Open
Questions.

### Turn `TASBalancedPlacement` off

This is not needed. It affects only PodSets that ask for a preferred topology level, and no Pod this
operator renders does. If a role ever needs that annotation, the choice between turning the gate off and
waiting for the upstream fix (kubernetes-sigs/kueue#15335) belongs to that change.

## Open Questions

None open. The coordinator settled the draft's questions:

1. No Setting. The Kueue gate is the switch and the cap is a constant; turning the gate off also stops
   Kueue from honoring other authors' preferred affinity, and the documentation says so.
2. Case 88 is not run here. The preference on accelerator nodes stays unverified, and the summary says
   so.
3. If case 87's rows differ between the gate on and off, the build stops and reports the differing rows
   before anything ships, because the chart default would then change placements this spec promises to
   keep.
