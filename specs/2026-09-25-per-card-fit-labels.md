# Spec: Per-Card Fit Labels

Status: Building
Blocked on: nothing. It waited on #577, which makes Gate 3 judge a shared request on the node TAS
assigned. The shared label is equivalent to Gate 3 only under that judgment. #577 has merged, and the
build starts from `origin/main` after it.
Type: Feature

## Summary

Kueue's topology-aware scheduling (TAS) picks a node from its summed capacity, so on a multi-card
node whose free room is spread over cards none of which fits the request, it places a logical slice
or a multi-card shared request there. The node-devices AdmissionCheck then answers `Retry` every
30 s, and TAS returns the request to the same node, even while another node has room. This spec has
the worker publish, per node and per accelerator model, how much one card can still give: the most
free units on any one sliceable card, and the number of cards that still have a free ownership
share. A mutating webhook on Kueue `Workload` creation, and on a spec update before quota is reserved, adds a required node-affinity expression on
those labels to the Workload's PodSet templates, so TAS skips a fragmented node by itself. The
labels filter only. They are never charged against, and the expression never reaches a Pod.

## Motivation

### Goals

Every queue this operator derives is TAS-only, and its lowest topology level is
`kubernetes.io/hostname` (`pkg/worker/controllers/worker/node_topology.go:101`). At quota
reservation TAS picks one node from that node's `status.allocatable` scalars. The scalars sum the
node's cards and cannot show how the room is spread between them:

- `<base>.sliced.units` is the node's card count times 1600000;
- `<base>.shared` is the node's card count times 10.

The node-devices AdmissionCheck (Gate 3) judges each request against the per-card ledger of the node
TAS assigned. When no card on that node fits, it answers `Retry`. Kueue evicts the Workload, and 30 s
later TAS places it again from the same scalars on the same node. The admission page records this
as a Known behavior: a fragmented node livelocks. It was reproduced on a cluster of two four-card
nodes: one node had four 60 % slices, one per card, and the other node was empty. A 50 % slice
reserved quota on the fragmented node, retried, and was evicted every 30 s for as long as it was
observed.

Kueue v0.18.9, the deployed version, has no mechanism for an AdmissionCheck to move a Workload to
another node, exclude a node, or remember a rejected one. These findings come from reading its
source:

- TAS merges `podSetUpdates` only from checks in state `Ready`, and only their `nodeSelector` and
  `tolerations` (`pkg/scheduler/flavorassigner/tas_flavorassigner.go:116-124`). A `Retry` resets
  every check and clears the updates (`pkg/workload/admissionchecks.go:110-129`).
- The TAS ungater copies the TAS domain's labels into the Pod's `nodeSelector` with `maps.Copy`
  (`pkg/controller/tas/topology_ungater.go:288`), so a check could not override the node anyway.
- A requeued Workload is placed from scratch, and the node order is deterministic: least free
  capacity first (`pkg/cache/scheduler/tas_flavor_snapshot.go:1764-1790`).

The only per-node input TAS reads besides `allocatable` and taints is node labels, matched against
the PodSet's **required node affinity**, with `Gt` and `Lt` supported:

- TAS builds its node filter from the PodSet template's affinity through `podset.FromPodSet` and
  `nodeaffinity.NewNodeSelector` (`pkg/cache/scheduler/tas_flavor_snapshot.go:892,958-966`).
- A node label change is a node change to the TAS cache (`pkg/cache/scheduler/tas_nodes_cache.go:128-132`).
- When no node in the chosen flavor passes, TAS marks that assignment NoFit
  (`pkg/scheduler/flavorassigner/flavorassigner.go:856-890`). The flavor scan does not move on in the
  same pass; a later scheduling pass starts from the next flavor.

This spec uses that channel.

Success criteria:

1. On a node with a sliceable model, the worker publishes the largest free `units` on any one
   sliceable card of that model. On a node with a whole-card-capable model, it publishes the number
   of cards of that model with at least one free ownership share.
   - Both values are computed with the same per-card predicate Gate 3 uses, so for a single Pod
     asking for one card the label admits a node exactly when Gate 3 would.
   - This relies on Gate 3 judging the shared family on the node TAS assigned, which #577
     introduces. The build starts after #577 merges. Before it, Gate 3 read a shared request across
     the pool, and the shared label would have rejected nodes Gate 3 accepted.
2. A Workload whose PodSet requests a logical slice of `U` units per card, or a shared request of
   `N >= 2` cards, is created with the matching expression ANDed into every required node-selector
   term of that PodSet: `Gt U-1` for a slice, `Gt N-1` for a shared request.
3. In the fragmented-node shape above, the Workload is placed on the node with room in its first
   scheduling cycle, and the check answers `Ready` without a `Retry`.
4. The same shape with the webhook removed still livelocks on the fragmented node. This is the input
   that must fail, and it proves the fix comes from the pin.
5. When no node has a card that fits, the Workload stays pending on a TAS "doesn't fit" reason and
   holds no quota. It no longer reserves and releases quota every 30 s.
6. No Pod ever carries an expression on these labels.
7. Kueue may be shared with other tenants. The webhook changes only a Workload that this operator's
   InstanceType chain admits and that requests this operator's accelerator keys. Every other
   Workload is left byte-for-byte unchanged.
8. An administrator can turn the pin off at runtime while the labels keep being published, so a
   verification can ablate the pin alone.

### Non-Goals

- Changing `status.capacity` or `status.allocatable`, or adding a counting key. The labels filter
  only and are never charged; per-card counting keys are a separate design that is not pursued.
- Changing the TAS queues, their `kubernetes.io/hostname` lowest level, or the Kueue configuration.
- Labels for the exclusive family. A card another mode holds reports its whole-card token
  `Unhealthy`, which the kubelet subtracts from `allocatable`. A node's exclusive `allocatable` is
  therefore already the count of free cards.
- Labels for the partitioned family. A partition request is always one instance on one card, and the
  per-profile key is each node's sum of that profile's placeable instances. For a single Pod the
  summed key is exact.
- The structural case of a shared request on a node with fewer than `N` cards. The Pod webhook's
  static pin on `acceleratable.feature.gpustack.ai/<group>.count` handles it; this spec adds only the
  dynamic free-share count and composes with that pin.
- Fitting several Pods of one PodSet, or several PodSets of one Workload, on one node. See Risks.
- Pinning Workloads created before the upgrade that never change again. The webhook acts on creation,
  and on a spec update before quota is reserved.
- Repairing a Workload whose creation missed the pin, for example because the webhook was down. Kueue
  requeues the same Workload after a `Retry`, so it keeps today's behavior for its whole life.

## Proposal

The worker keeps two node labels per accelerator model on every managed node, derived from that
node's `Devices` ledger:

| Label | Value | Published when |
|---|---|---|
| `sliced-max-free-units.fit.gpustack.ai/<aKey>` | the largest `remaining` on any one card of the model that can serve a logical slice | the model has at least one such card |
| `shared-free-cards.fit.gpustack.ai/<aKey>` | the number of cards of the model that can serve a whole-card family and have `remaining >= 160000`, one ownership share | the model has at least one whole-card-capable card |

`<aKey>` is the accelerated device key, `<manufacturer>-<id>`, the same key as the
`acceleratable.feature.gpustack.ai/<aKey>` labels and an `InstanceType`'s `spec.acceleratorGroup`.
A value can be `0`. A label disappears when its model no longer has a card of that population, and
when the node is no longer managed.

A mutating webhook on `workloads.kueue.x-k8s.io` CREATE and UPDATE first decides whether the
Workload is this operator's.

UPDATE is included because Kueue rebuilds a Workload's spec from its job when a suspended job's
template changes, and writes the rebuilt spec with an update
(`pkg/controller/jobframework/reconciler.go:1494-1500`). That update would drop the pin. The webhook
acts on an UPDATE only while the old Workload holds no quota reservation. That is the same condition
under which Kueue's own validation lets PodSets change (`pkg/webhooks/workload_webhook.go:360-366`),
so the webhook never produces an update that Kueue must refuse. Kueue may serve other tenants too, so it follows the queue chain the operator itself
builds, and does not rely on a name alone:

1. The operator names every LocalQueue it creates `gpustack-fnv64-<hash of the ClusterQueue name>`
   (`nodefeature.FormatLocalQueueName`). The webhook registration carries a `matchConditions`
   expression on `object.spec.queueName.startsWith('gpustack-fnv64-')`, so other Workloads never
   reach the webhook. An API server too old for `matchConditions` drops the field and sends every
   Workload, and the checks below still hold.
2. The LocalQueue `<namespace>/<spec.queueName>` must exist, and its `spec.clusterQueue` names the
   ClusterQueue (`node_queue_entrance.go:101-112`).
3. That ClusterQueue must carry the operator's resource-type mark `instancetypes`
   (`systemmeta.MatchResource`; `instance_type.go:312-327`, `node_queue.go:74`).
4. An `InstanceType` with the ClusterQueue's name must exist, because the operator names each
   InstanceType's ClusterQueue after it. Its `spec.acceleratorGroup` is the `<group>`.
5. The PodSet must request a slice or shared key of a known accelerator base
   (`nodefeature.ResourceFamilyOf`).

A Workload that fails any step is returned without a patch. The pin is also skipped while the
runtime setting `workload-fit-affinity` is `false`; the default is `true`. For every remaining
PodSet, the webhook reads the per-Pod demand the way Gate 3 reads it:

- a logical slice of `U > 0` units per card is pinned with
  `sliced-max-free-units.fit.gpustack.ai/<group> Gt U-1`;
- a shared request of `N >= 2` cards is pinned with `shared-free-cards.fit.gpustack.ai/<group> Gt N-1`.
  `N` is the largest count any one container of the Pod asks for, not the sum: Gate 3 lets two
  containers of one Pod hold shares on the same card;
- anything else is left unchanged.

The expression is ANDed into every required node-selector term of the PodSet template, or added as
the only term when there is none. The Pod and its owner's template are never touched.

### User Stories

#### Story 1

As a platform operator, I want a logical-slice request that lands on a multi-card node whose cards
are each too full for it to be placed on another node with a card that fits, so that the request
runs instead of retrying the same node every 30 s.

#### Story 2

As a platform operator, I want a `.shared: N` request to skip a node that has N cards but fewer than
N with a free ownership share, so that it is placed on a node that can grant N distinct cards.

#### Story 3

As a platform operator, when no node has a card that fits, I want the Workload to stay pending with
a clear reason and hold no quota, so that it does not keep reserving and releasing quota.

### Core Features & Acceptance Criteria

**F1 — fit labels on the Node.**

- On a node whose ledger reports cards of a sliceable model with `remaining` values
  `{640000, 640000, 1600000}`, the node carries
  `sliced-max-free-units.fit.gpustack.ai/<aKey>=1600000`. With `{640000, 640000}`, the value is
  `640000`.
- A card that is partitioned, or cannot slice, is excluded from the sliced label. A card that cannot
  serve a whole-card family is excluded from the shared label. The exclusion is the same one Gate 3's
  `servesFamily` makes.
- A shared-label card needs `remaining >= 160000`, the per-share units of Gate 3's `unitsPerCardFor`.
- A node with two models carries one label pair per model, each computed from that model's own cards.
- The labels follow the ledger: after an allocation or a release that changes a value, the Node
  carries the new value.
- A reconcile that computes the current values writes nothing.
- A label for a model or population the node no longer has is removed. So are all fit labels when
  the node becomes unmanaged. No label outside the `fit.gpustack.ai` domains is written or removed.
- A change confined to fit labels does not trigger this operator's other Node-watching reconcilers.
  - The capacity, NodeFeature, flavor and Devices-sync predicates already watch other label
    prefixes only.
  - The node-topology and topology-source predicates fire on any label change today
    (`node_topology.go:258`, `topology_source.go:180`). They are narrowed to ignore changes in which
    only fit labels differ.

**F2 — the Workload pin.**

- A Workload for an operator queue whose PodSet requests `<base>.sliced.units: 800000` gets
  `sliced-max-free-units.fit.gpustack.ai/<group> Gt 799999` in every required term of that PodSet.
- A PodSet requesting `<base>.shared: 2` gets `shared-free-cards.fit.gpustack.ai/<group> Gt 1`.
  A PodSet requesting `<base>.shared: 1` gets nothing.
- Exclusive and partition PodSets, and PodSets without accelerator requests, are unchanged.
- A Workload with two PodSets is pinned per PodSet, each with its own demand.
- The per-Pod demand is read by Gate 3's own PodSet parser, so the webhook and Gate 3 derive the same
  demand for every shape: an init-container-only demand, a demand set only in limits, and two
  containers with different units, where the larger wins.
- Existing expressions and terms are kept. The pin is ANDed into each term. Applying the webhook
  twice leaves one copy.
- An UPDATE whose old Workload holds no quota reservation and whose new spec lacks the pin gets the
  pin again. An UPDATE whose old Workload holds a quota reservation gets an empty patch, even when its
  pin is missing.
- Shared Kueue: a Workload requesting `<base>.sliced.units` on a queue this operator did not build
  gets an empty patch. That covers a foreign LocalQueue name. It also covers a
  `gpustack-fnv64-` name whose LocalQueue is missing or points at an unmarked ClusterQueue, and a
  marked ClusterQueue with no same-named InstanceType. The same Workload on an operator queue is
  pinned.
- A Workload on an operator queue whose PodSets request no slice or shared key gets an empty patch.
  So does one whose `InstanceType` names no accelerator group.
- A lookup error leaves the Workload unchanged. The webhook never denies a Workload.
  Its registration uses `failurePolicy: Ignore`, so an unavailable webhook also leaves the Workload
  unchanged, and that Workload falls back to today's Gate 3 `Retry`.
- The registration carries the `matchConditions` expression on the operator's LocalQueue name
  prefix.
- The Pod the Workload was built from, and the Pod Kueue ungates, carry no `fit.gpustack.ai`
  expression.

**F3 — end-to-end on kind, without accelerators.** The cluster has two worker nodes in one flavor,
a mocked accelerator NodeFeature and a mocked per-card `Devices` ledger. Node A has four cards at
640000 each; node B has four cards at 1600000 each.

- Positive: a 50 % logical slice (800000 units) is admitted onto node B in its first cycle. Its
  topology assignment names B, the check is `Ready`, and no eviction is recorded.
- Must fail: the same shape with `workload-fit-affinity=false` reserves quota on node A. The check
  answers `Retry`, and the Workload is evicted and reserved on A again at least twice, while node A
  still carries its fit labels.
- No fit: with both nodes at 640000, the Workload is not admitted. It reserves no quota, and its
  pending reason names node affinity.
- The admitted Pod's spec contains no `fit.gpustack.ai` key.

**F4 — the ablation setting.**

- `workload-fit-affinity`, environment `GPUSTACK_WORKLOAD_FIT_AFFINITY`, default `true`, is an
  editable runtime setting read on every webhook call.
- While it is `false`, the webhook returns every Workload unchanged, and the fit labels are still
  published and kept current.

**F5 — documentation.**

- The admission page's Known behavior and the scheduling-chain page describe the labels, the pin,
  the shared-Kueue identification and the residuals below.
- The settings page lists the new setting.

### Notes / Constraints / Caveats

- **Why the pin is on the Workload and not the Pod.** The kubelet re-runs admission, node affinity
  included, for every non-terminal Pod it is handed. That happens on a kubelet restart
  (k8s v1.35.3 `pkg/kubelet/kubelet.go:2748-2755`, `pkg/kubelet/lifecycle/predicate.go:252-270`).
  It was measured on a kind v1.36.1 node:
  - A running Pod had a required affinity `Gt 799999` on a node label.
  - After the label changed to `0` and the kubelet restarted, the Pod was rejected with
    `Predicate NodeAffinity failed` and its container was killed.
  - A Pod on the same node without that affinity kept running.

  A fit label always drops below the threshold once the Pod has taken its card. A Pod-level pin
  would therefore kill every slice and shared Pod on a full node at the next kubelet restart.
- **Why the Workload pin survives.**
  - Kueue compares a job's PodSets with its Workload's without the affinity. For job integrations
    this is `pkg/util/equality/podset.go:42-60`, which compares tolerations, containers, resources
    and claims. For pod groups, `pkg/controller/jobs/pod/pod_controller.go:1386-1417` compares
    names and counts. So a pinned Workload is not rebuilt.
  - When Kueue starts a job, `podset.Merge` writes annotations, labels, `nodeSelector`, tolerations
    and scheduling gates, never affinity (`pkg/podset/podset.go:174-204`).
  - `podset.FromPodSet` is called only by TAS.
- **Which integrations reach the webhook.**
  - Every job-framework integration creates its Workload through the API server
    (`pkg/controller/jobframework/reconciler.go:1861`). That covers pod groups and single Pods,
    including Deployments and StatefulSets. It also covers the template-built ones: batch Job,
    JobSet, RayJob, RayCluster, the Kubeflow jobs, TrainJob and AppWrapper.
  - LeaderWorkerSet creates its own Workload (`pkg/controller/jobs/leaderworkerset/leaderworkerset_reconciler.go:319`).
  - Every integration the chart enables is therefore seen.
  - A template-built Workload is created before any Pod exists, so the Pod webhook has not folded
    `.sliced.units` into its template. Such a PodSet has no slice units to pin, and Gate 3 gives it
    no per-card budget either. Its shared requests are pinned, because `N` is in the template.
- **Label key shape.** The key's name part is exactly `<aKey>`, which is at most 63 characters
  (`pkg/device/helper.go:50-62`), so no key can be invalid.
  - A key of the form `acceleratable.feature.gpustack.ai/<aKey>.sliced.max-free-units` would exceed
    63 characters for a long `aKey`.
  - That form would also wake, on every allocation, the capacity, NodeFeature and flavor
    reconcilers, whose predicates watch the `acceleratable.` prefix.
  - The worker patches the labels onto the Node directly. It already has full access to Nodes.
    It does not route them through a NodeFeature, because nothing needs NFD to own them.
- **Write rate.**
  - The value is exact. Bucketing would reject nodes Gate 3 accepts, and would not cut writes,
    because almost every allocation moves the value.
  - A write happens only when a value changes, after the existing 3 s dedup window on `Devices`
    events.
  - The cost is at most one Node metadata patch per allocation or release burst per node, beside the
    `Devices` status write that burst already causes. Every Node watcher in the cluster, Kueue's TAS
    cache among them, receives that update.
- **Rollout.** Workloads created before the upgrade are not pinned and keep today's behavior until
  they are recreated.

### Boundaries

- **Always:**
  - compute the label values with the same per-card predicate Gate 3 uses;
  - write only keys under the `fit.gpustack.ai` domains;
  - leave a Workload unchanged on any doubt, and whenever it is not this operator's;
  - keep Gate 3 unchanged as the per-card authority.
- **Ask first:**
  - changing Gate 3's verdicts;
  - adding a label family beyond the two;
  - mutating a Workload UPDATE whose old object holds a quota reservation;
  - changing the Pod webhook's static `.count` pin.
- **Never:**
  - put a dynamic label into a Pod's or a job template's affinity;
  - change `status.capacity` or `status.allocatable`;
  - let the Workload webhook deny a request;
  - write labels under a prefix NFD or another controller owns.

### Risks and Mitigations

- **Stale label window.** A label is never fresher than the ledger. The ledger moves after the device
  plugin's `Allocate`, and the label follows within the dedup window. A Workload placed inside that
  window still reaches Gate 3, which answers `Retry`. → The next placement reads the refreshed label
  and moves on, so the Workload converges in one cycle instead of livelocking. Documented.
- **Several Pods of one PodSet, several PodSets, or one Pod asking for several sliced cards, on one
  node.** A template-built Workload can carry `<base>.sliced: 2`, which the Pod webhook would refuse
  on a Pod. The label admits a node when one
  card fits one Pod. TAS may put two Pods of a PodSet on a node where only one card fits. Gate 3 then
  retries, and TAS repeats the placement. → This does not converge by itself, and it is documented
  as a known limitation. Closing it needs per-card counting keys, which are out of scope.
- **Template-built Workloads and sliced requests.** Their templates carry no folded units, so they
  get no slice pin. → Documented. Gate 3 gives them no per-card budget either, so behavior is
  unchanged.
- **A shared Kueue with other tenants.** Mutating another tenant's Workload would change where TAS
  places it. → The `matchConditions` prefix stops foreign Workloads from reaching the webhook at all.
  In the webhook, the full LocalQueue, marked ClusterQueue and InstanceType chain must resolve, and
  the PodSet must request this operator's keys. Anything else gets an empty patch. Unit cases cover
  each broken link.
- **A Workload that misses its pin stays unpinned.** Kueue requeues the same object after a `Retry`,
  and a mutating webhook is not re-run on a status change. → Documented. Only a spec update before
  reservation, or a new Workload, can pick the pin up. A new Workload arrives when a job is recreated
  or a workload slice scales up.
- **Webhook unavailable.** `failurePolicy: Ignore` leaves the Workload unpinned. → Gate 3 `Retry`,
  today's behavior. Kueue can always create Workloads.
- **Effect on the kube-scheduler and the kubelet.** They never see the pin, because it stays on the
  Workload. → There is no new effect. That is the reason for the placement.
- **Node write load.** A write happens on each value change. → The dedup window coalesces bursts, and
  an unchanged value is never written. The cost is stated in the docs.
- **The label and Gate 3 drift apart.** → The labels use Gate 3's own predicate functions, not a
  copy, and a unit case checks them against each other.

## Design Details

### Commands

```sh
make lint </dev/null                      # edit pass; compare git diff before and after
make generate                             # after the webhook markers change
go test ./pkg/nodefeature/ ./pkg/worker/...   # the unit suites this spec touches
make lint docs </dev/null                 # after docs/ or specs/ edits
```

The environment is local. Unit tests, lint and generate run on the development machine. The spike
and the end-to-end case run on a kind cluster on the same machine's Docker: one control-plane node
and two workers. The kubeconfig is kept in a scratch file, so the user's own kubeconfig is never
read or written:

```sh
kind create cluster --name c51-e2e --kubeconfig "$KCFG" --config <two-worker kind config>
KUBECONFIG="$KCFG" bash .agents/skills/_e2e-lib/scripts/build-load.sh <TAG>   # native build, kind load
KUBECONFIG="$KCFG" bash .agents/skills/_e2e-lib/scripts/deploy.sh <NS> <TAG>
KUBECONFIG="$KCFG" bash .agents/skills/gpustack-operator-e2e/cases/case-96.sh <NS>
kind delete cluster --name c51-e2e --kubeconfig "$KCFG"
```

Every command against the cluster carries `KUBECONFIG="$KCFG"`.

### Project Structure

- `pkg/worker/controllers/worker/` — the reconciler that derives and patches the fit labels from the
  Node and its `Devices` ledger, next to `node_capacity.go` and `node_devices_admission.go`.
- `pkg/worker/webhooks/worker/` — the Workload mutating webhook, next to `pod.go`.
- `pkg/nodefeature/` — the label key constructors.
- `pkg/worker/settings/` — the `workload-fit-affinity` setting.
- `docs/architecture/admission.md`, `docs/architecture/scheduling-chain.md`, `docs/settings.md` —
  the documentation.
- `.agents/skills/gpustack-operator-e2e/cases/` — the kind case.

### Code Style

The existing capacity reconciler converges owned keys with a merge patch that sets changed keys,
nulls stale owned keys, and returns nil when nothing differs. The fit labels follow the same shape:

```go
func buildAcceleratorCapacityPatch(desired, current core.ResourceList) map[string]any {
	patch := make(map[string]any)
	for name, q := range desired {
		if cur, ok := current[name]; !ok || cur.Cmp(q) != 0 {
			patch[string(name)] = q.String()
		}
	}
	for name := range current {
		if !isOwnedCapacityKey(name) {
			continue
		}
		if _, ok := desired[name]; !ok {
			patch[string(name)] = nil
		}
	}
	if len(patch) == 0 {
		return nil
	}
	return patch
}
```

Other conventions:

- Go comments state the logic, with no task identifiers.
- Tests are table-driven and use fake clients.
- Files are named in snake_case.

### Implementation Plan

The build starts from the latest `origin/main` once #577 has merged, on the branch
`feat/per-card-fit-labels`. T6 changes no file in the repository and may run earlier.

Phase A has no dependency on another task and can run in parallel. Phase B builds on it. Phase C
proves the whole path.

**Phase A — foundations**

- [x] **T1 · Fit label keys**
      Blocked by: None
      Owns: `pkg/nodefeature/fit.go`, `pkg/nodefeature/fit_test.go`
      Acceptance:
      - `FitSlicedMaxFreeUnitsLabelKey(aKey)` returns `sliced-max-free-units.fit.gpustack.ai/<aKey>`.
      - `FitSharedFreeCardsLabelKey(aKey)` returns `shared-free-cards.fit.gpustack.ai/<aKey>`.
      - `IsFitLabelKey` accepts exactly those two shapes. It rejects `fit.gpustack.ai/x`, `x.fit.gpustack.ai.evil/y` and an empty name part.
      - `EqualIgnoringFitLabels(a, b)` compares two label maps with every fit key left out.
      - A table case builds a key from a 63-character `aKey` and checks it with `validation.IsQualifiedName`.
      Verify: `go test ./pkg/nodefeature/ -run 'TestFitLabelKeys$|TestIsFitLabelKey$|TestEqualIgnoringFitLabels$' -v`

- [x] **T0 · Gate 3's per-Pod demand, exported**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/fit_demand.go`, `pkg/worker/controllers/worker/fit_demand_test.go`
      Acceptance:
      - `PodSetFitDemand(ps *kueue.PodSet) (slicedUnits, sharedCards int32)` returns the per-Pod demand read by `podSetFamilyDemands(ps, 1)`, Gate 3's own parser.
      - `node_devices_admission.go` is not edited.
      - The table covers these shapes: a slice, a shared request of 1 and of 3, two shared containers (the larger count wins, not the sum), exclusive, a partition, no accelerator, an init-container-only demand, a limits-only demand, two containers with different units (the larger wins), and units above `MaxInt32` (clamped).
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestPodSetFitDemand$' -v`

- [x] **T4 · The ablation setting**
      Blocked by: None
      Owns: `pkg/worker/settings/value.go`, `pkg/worker/settings/value_test.go`, `docs/settings.md`
      Acceptance:
      - `WorkloadFitAffinity` is an editable bool named `workload-fit-affinity`, initialized from `GPUSTACK_WORKLOAD_FIT_AFFINITY`, default `true`.
      - The settings table documents it, including that a change takes up to 30 s to reach the webhook.
      Verify: `go test ./pkg/worker/settings/ -run 'TestWorkloadFitAffinitySetting$' -v`, then `make lint docs </dev/null`

- [x] **T6 · Spike: Kueue honors a Workload-only affinity**
      Blocked by: None
      Owns: nothing in the repository; scripts and output stay in the scratchpad
      Gate: review
      Acceptance:
      - The spike runs on a throwaway kind cluster with two workers, created with `--kubeconfig <scratch>`, and the upstream Kueue v0.18.9 chart with TAS enabled.
      - It sets up a hostname Topology, a TAS ResourceFlavor, a ClusterQueue and a LocalQueue. Each worker gets a fake extended resource and a label `e2e-fit.example/free`.
      - It creates a batch Job with no affinity, and a prebuilt Workload whose PodSet template is the Job's defaulted template plus `e2e-fit.example/free Gt 0`. A single Pod cannot be used: Kueue's Pod integration accepts only a Workload named after the Pod's derived name (`pod_controller.go:1389`).
      - The Workload's topology assignment follows the label. The Pod binds there, and neither the Pod nor the Job template carries the affinity.
      - A run without the affinity, and a mirror run with the labels swapped, show that the label decided and not the node order.
      - The kind cluster is deleted afterwards, and `~/.kube/config` is unchanged: its checksum before and after is recorded.
      Verify: the spike script's final table in the scratchpad.
      Result: passed on four runs.

      | Run | Worker A label | Worker B label | Workload affinity | TAS assignment and Pod node | Pod or Job template affinity |
      |---|---|---|---|---|---|
      | control | 9 | 9 | none | worker A, Kueue's default order | none |
      | pinned | 0 | 9 | `Gt 0` | worker B | none |
      | mirror | 9 | 0 | `Gt 0` | worker A | none |
      | no fit | 0 | 0 | `Gt 0` | not admitted, no quota reserved | none |

      - In the pinned run, the stored Workload kept its affinity, so Kueue did not rebuild it from the Job.
      - In the no-fit run, the pending message was `topology "spike-topo" doesn't allow to fit any of 1 pod(s). Total nodes: 2; excluded: affinity: 2`, and the flavor reservation was 0.
      - The kubeconfig checksum was the same before and after.

**Checkpoint A:** `go test ./pkg/nodefeature/ ./pkg/worker/controllers/worker/ ./pkg/worker/settings/`. Then run `make lint </dev/null` and compare `git status --porcelain` and `git diff` before and after.

**Phase B — the two halves**

- [ ] **T2 · Fit labels on the Node**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/node_fit_label.go`, `pkg/worker/controllers/worker/node_fit_label_test.go`, `pkg/worker/controllers/setup.go`
      Gate: review
      Acceptance:
      - `desiredFitLabels(nd, devs)` joins each `Devices` status card with its spec capability by ID, as `collectCards` does, per `<manufacturer>-<id>`.
      - The sliced label is the maximum `remaining` over cards where `cardLedger.servesFamily(Sliced)` holds.
      - The shared label counts cards where `servesFamily(Shared)` holds and `remaining >= unitsPerCardFor(shared)`.
      - A population with no card emits no key.
      - An unmanaged node, or a nil `Devices`, yields no fit labels.
      - `buildFitLabelPatch(desired, current)` sets changed keys and nulls stale fit keys. It returns nil when they are equal, and it never names a key outside `IsFitLabelKey`.
      - `NodeFitLabelReconciler` patches `metadata.labels` with a merge patch on a fresh Node object. It is registered in `setup.go`.
      - It watches Nodes when the managed label, an `acceleratable.` label or a fit label changes.
      - It watches `Devices` create and delete, and updates whose fit signature changes. The signature is the per-group desired values. The watch uses a 3 s dedup window.
      - `TestFitLabelsAgreeWithNodeDevicesFeasibility` is a parity table. For each ledger and demand, `sliced-max-free-units > U-1` must equal a Ready verdict from `nodeDevicesFeasibility` for one Pod with one card on that node. `shared-free-cards > N-1` must equal a Ready verdict for one shared Pod with N cards. The table includes a mixed-mode ledger and a partitioned card.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestDesiredFitLabels$|TestBuildFitLabelPatch$|TestFitLabelsAgreeWithNodeDevicesFeasibility$|TestNodeFitLabelReconciler_Reconcile$|TestFitSignatureChanged$' -v`

- [x] **T3 · Topology predicates ignore fit-only label changes**
      Blocked by: T1
      Owns: `pkg/worker/controllers/worker/node_topology.go`, `pkg/worker/controllers/worker/node_topology_test.go`, `pkg/worker/controllers/worker/topology_source.go`
      Acceptance:
      - The two inline `!kubemeta.DeepEqual(old.Labels, new.Labels)` predicates become one named function, `nodeLabelsChangedIgnoringFit`, which uses `nodefeature.EqualIgnoringFitLabels`.
      - Its table covers four cases. A change only in fit labels is false. Any other label change is true. A fit change together with another change is true. No change is false.
      Verify: `go test ./pkg/worker/controllers/worker/ -run 'TestNodeLabelsChangedIgnoringFit$' -v`

- [ ] **T5 · The Workload webhook**
      Blocked by: T0, T1, T4
      Owns: `pkg/worker/webhooks/worker/workload.go`, `pkg/worker/webhooks/worker/workload_test.go`, `pkg/worker/webhooks/setup.go`, `pkg/worker/webhooks/worker/zz_generated.webhooks.go`
      Gate: review
      Acceptance:
      - `WorkloadWebhook` implements only `Default`, and it returns nil on every path, so it never denies.
      - Markers:
        - group `kueue.x-k8s.io`, version `v1beta2`, resource `workloads`, scope Namespaced;
        - operations CREATE and UPDATE;
        - `failurePolicy: Ignore`, `sideEffects: None`, `matchPolicy: Equivalent`, `timeoutSeconds` 10;
        - `matchConditions` with `object.spec.queueName.startsWith('gpustack-fnv64-')`;
        - the name prefix `gpustack-worker`.
      - Ownership follows the Proposal's chain: the LocalQueue, then the ClusterQueue carrying the `instancetypes` mark, then the same-named InstanceType and its `spec.acceleratorGroup`. Reads use the cached client.
      - The pin per PodSet comes from `PodSetFitDemand`. It is ANDed into every required term, deduplicated.
      - The setting is read on each call.
      - An UPDATE is acted on only when `workload.HasQuotaReservation(old)` is false.
      - `make generate` regenerates `zz_generated.webhooks.go`.
      - The tests cover:
        - `TestWorkloadWebhook_Default`: a slice pinned `Gt 799999`; a shared request of 2 pinned `Gt 1`; a shared request of 1, exclusive, a partition and a plain PodSet unchanged; two PodSets pinned each with its own demand; existing terms ANDed into; a second pass that adds nothing.
        - `TestWorkloadWebhook_ForeignWorkloadsUntouched`: a Workload on a foreign queue name; a `gpustack-fnv64-` name without a LocalQueue; a LocalQueue on an unmarked ClusterQueue; a marked ClusterQueue without an InstanceType; an InstanceType without a group. Each requests `.sliced.units` and must come out deep-equal to its input.
        - `TestWorkloadWebhook_Update`: an unreserved old Workload is pinned; a reserved one is left unchanged.
        - `TestWorkloadWebhook_SettingOff`: with the setting off, nothing is pinned.
        - `TestWorkloadWebhook_PatchTouchesOnlyAffinity`: runs the controller-runtime handler on a raw v1beta2 Workload JSON that also carries a field unknown to the vendored type. Every patch operation's path is under `/spec/podSets/<i>/template/spec/affinity`. A foreign Workload gets no patch at all.
        - `TestWorkloadWebhookRegistration`: reads the generated `MutatingWebhook` and asserts the operations, the failure policy and the match condition.
      Verify: `go test ./pkg/worker/webhooks/... -run 'TestWorkloadWebhook_Default$|TestWorkloadWebhook_ForeignWorkloadsUntouched$|TestWorkloadWebhook_Update$|TestWorkloadWebhook_SettingOff$|TestWorkloadWebhook_PatchTouchesOnlyAffinity$|TestWorkloadWebhookRegistration$' -v`

**Checkpoint B:** `make lint </dev/null`, then `make generate`, with the before and after comparison. Then `go test ./pkg/...`.

**Phase C — proof and record**

- [ ] **T7 · Documentation**
      Blocked by: T2, T5
      Owns: `docs/architecture/admission.md`, `docs/architecture/scheduling-chain.md`
      Acceptance:
      - The admission page's Known behavior says a fragmented node is now skipped by TAS through the fit labels. It keeps the three residuals: the refresh window, several Pods on one node, and an unpinned Workload.
      - The scheduling-chain page gains a short section on the two labels, their writer, the Workload pin, the shared-Kueue identification, and why the pin never reaches a Pod.
      Verify: `make lint docs </dev/null`

- [ ] **T8 · End-to-end case on kind**
      Blocked by: T2, T3, T4, T5, T6
      Owns: `.agents/skills/gpustack-operator-e2e/cases/case-96.sh`, `.agents/skills/gpustack-operator-e2e/SKILL.md`
      Gate: review
      Acceptance:
      - `case-96.sh <NS>` mocks, on two kind workers, the accelerator NodeFeature (`nvidia-e2emock`, four cards), the bare `.sliced` pool capacity through the status subresource, and a node-named `Devices` ledger.
      - It asserts the fit labels the worker publishes: A=640000, B=1600000.
      - It then runs the four F3 legs: positive, must-fail with the setting off, no fit, and no fit key on the Pod.
      - Rows follow `_rows-lib.sh`. Cleanup restores the setting and removes every mock.
      - The SKILL.md case table gains row 96 with its trigger paths.
      - The case has been run on a local kind cluster built from the branch image, and its table is recorded in the PR.
      Verify: `KUBECONFIG=<scratch> bash .agents/skills/gpustack-operator-e2e/cases/case-96.sh <NS>`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None. The existing Gate 3, capacity and Pod-webhook suites stay as they are. The parity table in T2
calls `nodeDevicesFeasibility` directly rather than copying its logic.

#### Unit tests

Baseline coverage, measured before any change:

- `pkg/nodefeature`: `2026-09-25` - `92.4%`
- `pkg/worker/controllers/worker`: `2026-09-25` - `82.3%`
- `pkg/worker/webhooks/worker`: `2026-09-25` - `90.8%`
- `pkg/worker/settings`: `2026-09-25` - `0.0%` (the new setting test is the package's first coverage)

Each target test name must appear in the `-v` output. An exit code of 0 is not enough, because a
`-run` pattern that matches nothing also exits 0. Every new predicate is shown to fail on an input it
must reject before it is trusted:

- The `IsFitLabelKey` cases include the look-alike keys.
- The parity table includes a node where the sum fits and no card does.
- The foreign-Workload table includes each broken link of the chain.
- Mutations are checked to go red on an assertion, not on a build failure:
  - `Gt U` in place of `Gt U-1`;
  - pinning only the first term;
  - skipping the reservation check on UPDATE;
  - dropping the `instancetypes` mark check.

#### Integration tests

None beyond the unit suites. The fake-client reconciler test in T2 and the controller-runtime handler
test in T5 are the integration points this repository tests without an API server. The API-server
behavior is covered by T6 and T8.

#### e2e tests

- **T6** is a kind and Kueue-only spike. A Workload-only affinity steers TAS, and the Pod never
  carries it.
- **T8**, `case-96.sh` on kind with a mocked ledger, needs no GPU and covers these legs:
  - positive: the Workload lands on the node with room in its first cycle;
  - must fail: with the setting off, it livelocks on the fragmented node for two or more cycles;
  - no fit: no quota is held;
  - the Pod carries no fit key.
- A kubelet-restart leg is not repeated. That mechanism was measured while writing this spec, and
  the pin never reaching the Pod is asserted directly.

## Alternatives

- **Pin the Pod through the Pod webhook.** Rejected. The kubelet re-admits running Pods on restart
  against current labels, and a fit label drops below the threshold once the Pod holds its card. The
  measurement and the coordinates are under Notes.
- **Publish per-card counting keys in `status.capacity`.** Two variants were considered: a largest
  free card key, and bucketed counts. Both change what Kueue and the scheduler charge. The first key
  is not conserved, so two Pods on one node would be charged twice. Not pursued.
- **Have Gate 3 steer TAS.** Not possible. `podSetUpdates` apply only from `Ready` checks, and the
  ungater overwrites the hostname.
- **Claim the provisioning-request controller name to defer TAS to a second pass.** Rejected. It
  depends on a reserved name and on unpromised behavior, and it conflicts with Kueue's own ProvReq
  controller when that CRD is installed.
- **Use a lowest topology level other than hostname.** Rejected. The kube-scheduler would pick the
  node inside the domain from summed capacity, and the slice oversubscription would return.
- **Bucket the label values.** Rejected. It errs toward rejection and does not reduce writes.

## Open Questions

None.
