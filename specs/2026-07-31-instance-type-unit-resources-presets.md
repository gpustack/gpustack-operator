# Spec: InstanceType Unit-Resources Presets

Status: Building
Type: Feature

## Summary

A node-derived accelerator `InstanceType` is stamped today with one fixed unit spec — 4 CPU /
16Gi RAM per whole card — regardless of what card the pool actually holds. That single value is
meaningless for the hardware on both ends of the range: it over-states a T4 pool and badly
under-states an H100 pool, and because it is the value an Instance's CPU/RAM request is defaulted
from, an operator reading the pool's `InstanceType` sees a number that describes no real machine.
This change replaces the constant with a static, embedded preset table keyed on the pool's
accelerator **manufacturer** and **product**, sized from published per-card CPU/RAM ratios of real
cloud instances and OEM reference machines. A product the table does not recognise keeps today's
exact value, so the change is purely additive.

## Motivation

### Goals

- **Stamp a value that describes the hardware.** The unit spec an operator-derived `InstanceType`
  carries at creation should reflect the per-card CPU/RAM a real machine of that class provides, so
  the object is meaningful to read and so an Instance that omits `cpu`/`ram` is defaulted to a
  sensible share of the node rather than to a constant chosen for no card in particular.
- **Stay schedulable.** With no explicit `cpu`/`ram`, the unit spec *is* the Pod request. It must
  stay small enough that N single-card Pods still fit a real N-card node of that class — the sizing
  is therefore taken from the **low position** of each product's published ratio range, not its most
  generous one.
- **One product, one tier — structurally.** It must be impossible for a given accelerator product to
  resolve to more than one preset, and impossible for the outcome to depend on the order entries
  happen to appear in the table.
- **Stay auditable.** Every preset row traces to a named public source, documented under `docs/`, so
  an administrator can see why their pool got the number it got.
- **Zero regression.** A product with no entry, or a manufacturer outside the nine the operator can
  detect, is stamped with exactly today's value (4 CPU / 16Gi / 100Gi).

Target users: cluster administrators who let the operator derive InstanceTypes from node hardware
(`instance-type-derived-from-node`), and the workload owners whose Instances are sized by the result.

Success criteria (all testable):

1. `nodefeature.PresetUnitResources("nvidia", "NVIDIA H100 80GB HBM3")` and its label-sanitized twin
   `"NVIDIA-H100-80GB-HBM3"` both resolve to the flagship tier.
2. Every one of the nine detectable manufacturers has at least its mainstream products covered, and
   the table refuses to load if one is missing entirely.
3. An unrecognised product resolves to `4` / `16Gi`, byte-identical to today.
4. Every emitted preset satisfies the existing `validateInstanceTypeUnitSpec` rules **using the same
   validators**, so no table can produce an `InstanceType` the admission webhook would reject.
5. Shuffling the entries of the embedded table changes no lookup result.
6. `docs/` carries a per-product table whose every row names its source.

### Non-Goals

- **Changing admission validation.** This spec touches only the value stamped at *creation* time on
  an operator-derived `InstanceType`. `capResourcesToInstanceType`
  (`pkg/worker/webhooks/worker/instance.go:734`), the unit-spec validation rules, the immutability
  rule and the create-only rule are all left exactly as they are. Consequences of the new values
  flowing through them are recorded under Risks, not fixed here.
- **Re-sizing existing InstanceTypes.** `spec.unitResources` is immutable after creation and
  `authorDerivedInstanceType` is create-only. Presets take effect for pools authored *after* the
  upgrade. No migration, no backfill, no mutation of admin-created types.
- **Reading live node capacity.** The lookup stays a pure function of (manufacturer, product). It
  never consults `Node.status.allocatable`, card count, or the Devices ledger.
- **A VRAM-derived fallback.** An unmatched product falls back to the fixed default, not to a band
  computed from `NodeFlavor.Memory`. (See Alternatives.)
- **`localStorage` presets.** It stays `100Gi` for every tier.
- **CPU-only (non-acceleratable) types.** They keep `1` CPU / `2Gi`; the webhook pins their unit CPU
  to exactly 1 anyway.
- **New API fields, new settings, or administrator-supplied overrides of the table.** The table ships
  with the operator image; an administrator who needs a different size pre-creates the
  `InstanceType`.
- **Harvesting live per-vendor product-string fixtures.** Coverage for Iluvatar, MThreads and THead
  is written from vendor documentation because this repository pins no representative string for
  them; capturing real fixtures needs hardware access and is tracked as an Open Question.
- Manufacturers the operator cannot detect (Intel Gaudi/Max, Kunlun, Enflame, BiRen). They have no
  manufacturer key, so they cannot be table keys.

## Proposal

When the operator authors a derived accelerator `InstanceType`, it looks the pool's
(manufacturer, product) pair up in a static table embedded in the binary and stamps the matched
preset's per-card CPU and RAM. An unmatched pair keeps the current fixed default. Nothing else about
how the type is authored, validated, or reconciled changes.

The table lives in **`pkg/nodefeature`**, next to `knowns.go` — the package that already owns the
per-manufacturer registries and the node-label algebra. Per-product sizing knowledge is the same kind
of knowledge; the worker controller consumes it, it does not own it.

The table is **YAML embedded with `go:embed`**, not a Go literal, so a sizing change is a data edit
reviewed as a flat table rather than a code change. It is strictly decoded and validated at load
time.

Lookup is **two-level**, which is what makes the one-product-one-tier guarantee structural rather
than test-enforced:

```
product string ──normalize──▶ key ──prefix entry (at most one)──▶ ordered `by` walk ──▶ tier
```

Within one manufacturer a `prefix` is a unique key and no prefix may be a token-prefix of another,
so a normalized product key matches **at most one entry**. The scope is the manufacturer, not the
whole file, because the lookup selects the manufacturer's entry list first: only a same-manufacturer
collision could ever produce a multi-match, and a global rule would additionally reject two vendors
that legitimately ship a similarly named part. Within an entry, capacity/variant discrimination is an
**ordered `by` sub-table** whose order is declared, local and documented. Entry order in the file is
therefore irrelevant to the result.

### User Stories

#### Story 1
As a cluster administrator running an 8×H100 node, I want the derived `InstanceType` to carry a
per-card unit spec that reflects what an H100 host actually provides, so that the object I read
describes my hardware and an Instance that omits `cpu`/`ram` is sized like a real H100 workload.

#### Story 2
As a workload owner submitting an Instance with no explicit `cpu`/`ram`, I want the defaulted request
to be a realistic share of the node, so that my Pod neither starves on 16Gi nor sits `Pending`
because the default asked for more RAM than the node has per card.

#### Story 3
As a cluster administrator running cards the operator has never heard of, I want the derived type to
keep behaving exactly as it does today, so that upgrading the operator cannot change the sizing of
any pool it does not positively recognise.

#### Story 4
As a maintainer adding a newly released card to the table, I want the table to reject my edit if it
could make an existing product resolve to a second tier, or produce a value the admission webhook
would refuse, so that a one-line data change cannot silently re-size unrelated pools or leave a pool
with no `InstanceType` at all.

#### Story 5
As a cluster administrator auditing why my pool got 12 CPU / 192Gi, I want a document that lists the
preset per product together with the public configuration it was derived from, so that I can judge
whether it fits my hardware and decide whether to pre-create my own `InstanceType` instead.

### Core Features & Acceptance Criteria

#### F1 — The tier ladder

A small, fixed set of tiers. VRAM bands are the *starting heuristic* that explains the ladder's
shape; each family is assigned from its own published data and may deviate from its band, with the
deviation recorded in the docs table.

| Tier | VRAM band (heuristic) | unit CPU | unit RAM | Anchoring evidence |
|---|---|---|---|---|
| *(fallback)* | unmatched / unknown manufacturer | `4` | `16Gi` | today's value, unchanged |
| `small` | ≤ 16 GiB | `8` | `32Gi` | T4 on Tencent GN7 / Huawei pi2 = 8c/32g |
| `medium` | > 16, ≤ 48 GiB | `8` | `64Gi` | RTX 4090 8-card boxes 16c/64g; A30 8c/32g; L4 12c/48g |
| `large` | > 48, ≤ 96 GiB | `12` | `128Gi` | A100-80 GCP a2-ultragpu 12c/170g; MI210 / K100_AI / MTT S4000 / MetaX C500 all 8–16c/128g |
| `xlarge` | > 96 GiB | `12` | `192Gi` | H100 Azure ND96isr 12c/231g, DGX H100 14c/256g; MI300X 12c/231g; Ascend 910C 32c/256g |

Acceptance criteria:
- **AC1.1** — Exactly these five tiers exist; each tier's RAM is a positive integer with a
  case-sensitive `Gi` suffix and each tier's CPU is a unitless positive integer **within int32**, so
  every preset passes `validateInstanceTypeUnitSpec` unchanged.
- **AC1.2** — CPU is deliberately flat at `12` above 48 GiB, and the constraint that sets that
  ceiling **only binds when general-resource overcommit is disabled**.
  `instance-general-resources-overcommit` defaults to `true`
  (`pkg/worker/settings/value.go:64`), and with it on the Pod *request* is scaled by
  `ScaleToOvercommit` to `100m × cpu` and `128Mi × (ram / Gi)` — so an `xlarge` card requests 1.2
  CPU / 24Gi and eight of them cost 9.6 CPU / 192Gi on the node. With overcommit *off*, eight
  `xlarge` single-card Pods request 96 CPU / 1536Gi, which a nominally 96-vCPU host cannot satisfy
  once system reservation is taken out. `12` is chosen for the default-on case; the overcommit-off
  caveat is documented (AC5.4) rather than designed around. A test asserts no tier exceeds `12` CPU.
- **AC1.3** — The `small` tier is strictly above the fallback, and the ladder is monotonic
  (`fallback ≤ small ≤ medium ≤ large ≤ xlarge` on both axes). A positively-identified card must
  never resolve to a value at or below an unidentified one.

#### F2 — The embedded table and its shape

A YAML file embedded with `go:embed`, **strictly** decoded (`sigs.k8s.io/yaml.UnmarshalStrict`) once
at package load. It carries the tier ladder, the leading marketing token sequences to strip, and, per
manufacturer, the prefix entries. It carries **no** provenance field — the citations live in the docs
page (F5), keeping the data file to matching facts only.

```yaml
tiers:
  fallback: {cpu: 4,  ram: 16Gi}
  small:    {cpu: 8,  ram: 32Gi}
  medium:   {cpu: 8,  ram: 64Gi}
  large:    {cpu: 12, ram: 128Gi}
  xlarge:   {cpu: 12, ram: 192Gi}

# Leading marketing token SEQUENCES, stripped repeatedly until none matches.
strip: [tesla, geforce, instinct, quadro, radeon, moore-threads, meta-x]

families:
  nvidia:
    - {prefix: h100, family: h100, tier: xlarge}
    - {prefix: gb10, family: gb10, tier: small}
    - prefix: a100
      family: a100
      tier:   medium          # this prefix's catch-all
      by:                     # ordered; the first token present in the key wins
        - {token: 80gb, family: a100-80gb, tier: large}
        - {token: 40gb, family: a100-40gb, tier: medium}
  ascend:
    - {prefix: "910b3", family: "910b3", tier: large}
```

Acceptance criteria:
- **AC2.1** — Decoding is strict: an unknown or misspelled field (`require:` for `by:`) is a hard
  error, not a silent widening. A duplicate YAML mapping key must also fail — verified in T1 to be
  `yaml.UnmarshalStrict`'s own behaviour, so no supplementary load-time check is needed. A test
  pins both.
- **AC2.2** — `family` values are globally unique — including those inside a `by` list — and each
  carries exactly one `tier`. This is the one-product-one-tier guarantee's second half and holds by
  construction.
- **AC2.3** — Load-time validation rejects the table when any of the following hold. All failures are
  fatal at package initialisation and are individually driven by a unit test:
  - **V1** — a referenced tier name is undefined; the tier set is not exactly
    `{fallback, small, medium, large, xlarge}`; the `fallback` tier is not exactly `{cpu: 4, ram:
    16Gi}` (pinning the zero-regression promise as data, not convention); the ladder is not monotonic
    (AC1.3); or a tier's values do not pass **the very validators the admission webhook uses**
    (`strconvx.ParseInt[int32]` for CPU, positive integer + case-sensitive `Gi` for RAM). A tier that
    parses as a 64-bit integer but overflows int32 must fail here — see the Risk on the reconcile
    loop.
  - **V2** — a manufacturer key is not one of the nine in `pkg/nodefeature/knowns.go:20-30` (a typo
    such as `nvdiia:` must fail, not silently orphan its entries), **or** one of the nine has no
    entries at all (coverage is `=`, not `⊆`).
  - **V3** — within one manufacturer, two entries share a `prefix`, **or** one entry's `prefix` is
    a **token**-prefix of another entry's `prefix`. This is the guarantee's structural support: it
    makes "at most one entry matches" a property of the table, not of the resolver. The rule is
    scoped per manufacturer because that is the scope the lookup itself has; a global rule would be
    stricter than the guarantee needs. It must be implemented on token sequences — a
    `strings.HasPrefix` implementation would wrongly reject the legitimate pairs `h20`/`h200`,
    `a10`/`a100` and `l4`/`l40`/`l40s`, silently cutting NVIDIA coverage.
  - **V4** — a `family` name is duplicated anywhere, including across `by` lists.
  - **V5** — within an entry, `by` tokens are empty, repeated, or **equal to a token of the entry's
    own `prefix`** (such a token matches unconditionally, making the entry's base tier unreachable).
  - **V6** — a `prefix` is **empty** (the empty token sequence is a token-prefix of every key, so an
    empty prefix would swallow a whole manufacturer and reintroduce order dependence), or a `prefix`
    or `token` is not lowercase, or contains a character outside `[a-z0-9]` plus (for a `prefix`) the
    token separators.
  - **V7** — a `strip` entry is empty, repeated, not lowercase, or is a token-prefix of another
    `strip` entry.
  - **V8** — an entry's `prefix` begins with a `strip` entry or with its own manufacturer name; both
    are removed before matching, so such an entry could never match and is dead on arrival.
- **AC2.4** — Resolution is defined without reference to entry order, and a test asserts that
  shuffling the entries resolves the whole corpus identically (success criterion 5). The order of a
  `by` list *is* semantic — that is deliberate, and the docs table records each `by` order. Because
  two `by` tokens of one entry *can* co-occur in a synthetic key, a companion test pins the declared
  order as the tie-break, so reordering a `by` list is a visible behaviour change rather than a
  silent one. (This is the property the rejected specificity-score shape left undefined; see
  Alternatives.)
- **AC2.5** — `localStorage` is not in the table; it stays `100Gi` for every tier and manufacturer.

#### F3 — Matching pipeline

Acceptance criteria:
- **AC3.1** — `key = device.NormalizeName(product, manufacturer, 0, false)`. `maxLength` **must** be
  `0`: the 63-character budget `ConstructGroupID` passes truncates without preserving token
  boundaries (`pkg/device/helper.go:95-146`), which would silently drop a trailing `40gb`/`80gb`
  discriminator.
- **AC3.2** — After normalization, leading marketing token **sequences** listed in `strip` are removed
  **repeatedly** until none matches. One pass is not enough: AMD ships `"AMD Radeon Instinct MI300X
  OAM"`, whose key needs both `radeon` and `instinct` removed before a `mi300x` prefix can match.
  Sequences (not single tokens) are needed because multi-word brands survive the manufacturer strip —
  see AC3.3.
- **AC3.3** — The manufacturer strip inside `NormalizeName` fires only when the buffer length equals
  the prefix length at that exact instant (`pkg/device/helper.go:124-129`). A multi-word brand
  therefore never strips: `("metax", "Meta X C500")` reaches length 5 holding `"meta-"`, fails the
  compare once, and can never fire again, yielding `meta-x-c500`. `moore-threads` behaves the same
  way. Such brands must be covered by `strip` sequences, and the constraint is recorded as a code
  comment.
- **AC3.4** — Tokens are the key split on `-`, `_` **and** `.`. `NormalizeName` preserves `_` and
  `.` (`pkg/device/helper.go:107-116`), so Hygon's `K100_AI` normalizes to `k100_ai` and must
  tokenize as `[k100, ai]` for a `k100` prefix to match. `K100_AI` and `K100-AI` must resolve
  identically.
- **AC3.5** — `prefix` matches iff it is a **token**-prefix of the key (`p == k` or `k` starts with
  `p` followed by a separator), and a `by` token matches iff it appears as a **whole token** anywhere
  in the key. Substring matching is wrong in both places: `a100-140gb` must not match the token
  `40gb`, and `a10` must not match `a100`.
- **AC3.6** — Both the raw vendor string (`"NVIDIA A10G"`, `"Ascend910B2"`, `"Tesla T4"`) and the
  label-sanitized form the flavor actually carries (`"NVIDIA-A10G"`, `"Tesla-T4"`) produce the same
  key. Equivalence is exact only below the 63-character label cap, which is applied
  (`pkg/nodefeature/helper.go:91-94`) *before* the manufacturer is stripped, and which truncates
  mid-token. A product whose discriminating tail is cut must fall to its entry's **base** tier, never
  to a wrong variant tier.
- **AC3.7** — Matching tolerates real-world product suffixes and variants: `"NVIDIA H100 80GB
  HBM3"`, `"NVIDIA H100 PCIe"`, `"NVIDIA H100 NVL"` and bare `"H100"` all resolve to the same tier.
- **AC3.8** — Coverage: every product family named in the source ratio survey that belongs to one of
  the nine detectable manufacturers has an entry — NVIDIA data-center (Blackwell / Hopper / Ampere /
  Ada / Turing / Volta), NVIDIA RTX PRO + workstation + GeForce, NVIDIA Grace parts (GH200, GB200,
  GB300, GB10), AMD Instinct (MI2xx / MI3xx), Ascend (310P, 910B, 910C), Cambricon (MLU370 family),
  Hygon (Z100, K100), MThreads (MTT S4000), MetaX (C500, C550), Iluvatar (BI-V150, MR-V100), THead.
- **AC3.9** — Coverage accounts for what the detectors actually emit, not for marketing names:
  - **Ascend** — the product is the raw DCMI chip name. The repository's only real fixture is the
    **bare** `910B2` (`testing/sample/devices/ascend-910b.yaml:3`), while the SoC table also carries
    prefixed forms `Ascend910B4-1`, `Ascend910_9391`, `Ascend910_95`, `Ascend950`
    (`pkg/devicemanager/detector/ascend/device.go:426-458`), plus the alias shapes `A2G\d` (910B) and
    `I2\d?` (310P) at `:461-465`. Entries must cover bare and prefixed forms; note that `910_9391`
    tokenizes as `[910, 9391]` while `910B3` is `[910b3]`, so no single "910 family" prefix exists;
    and **no Ascend name carries a memory token**, so `by`-refinement on VRAM is structurally
    impossible there.
  - **AMD / Hygon** — the name comes from a three-source fallback (host pci.ids → HSA ProductName →
    marketing name, `pkg/devicemanager/detector/amd/device.go:152-160`,
    `hygon/device.go:150-156`), so the same silicon keys differently depending on the host's pci.ids
    vintage and which source fired, and the chain can yield `""`. pci.ids composites such as
    `"Navi 32 [Radeon RX 7700 XT / 7800 XT]"` fuse into a single leading token because dropped
    characters insert no separator (`pkg/kubemeta/label.go:142-182`); those are left to the fallback
    rather than forced into a tier.
  - **Cambricon** — one card yields up to three strings depending on which symbols the installed
    driver exports: a rich name, the enum table value (`"MLU370"`), or bare `"MLU"`
    (`binding/cndev/library_device.go:68-112`). A bare `mlu` catch-all entry is therefore
    **forbidden**: it would capture the unknown-card sentinel and break the zero-regression contract
    for every future Cambricon part.
  - **MetaX** — emits `MXC500` (`pkg/device/helper_test.go:23`); a `c500`-only entry silently misses.
  - **Hygon** — emits `K100_AI` → `k100_ai`.
  - **Iluvatar / MThreads / THead** — this repository pins no representative product string
    (`iluvatar/device.go:115`, `mthreads/device.go:127`, `thead/device.go:119`). Their entries are
    written from vendor documentation and the docs page marks them **unverified against this
    codebase**.
- **AC3.10** — Negative matching is asserted: an entry for a large card must not swallow a small one
  that shares a substring (`"RTX 4000 Ada"` must not match an `a100`/`rtx-4090` entry; `"310P3"` must
  not match a `910`-family entry). The `NormalizeName` prefix trim is **not** token-boundary aware —
  `("amd", "AMD64 Accelerator")` yields `64-accelerator` (`pkg/device/helper.go:123-133`) — and the
  table must not assume otherwise; this is recorded as a code comment.
- **AC3.11** — SKU granularity: where a chip ships in materially different VRAM capacities whose
  published host ratios differ by roughly 2× **and** the product string carries the capacity token,
  they are separate families inside one prefix entry's `by` list. A100/A800 `80gb` → `large`, `40gb`
  → `medium`, the entry's catch-all → `medium`. V100 16GB / 32GB follow the same rule.
- **AC3.12** — A structural limitation, accepted and documented: V3 forbids a separate entry for a
  variant whose key merely appends a token to another entry's prefix — `Ascend910B4-1` (`[910b4, 1]`)
  cannot have its own entry beside `910b4`, and neither can `GeForce RTX 4090 D` beside `rtx-4090`.
  Such a variant must be refined through its parent's `by` list, whose whole-token match is
  position-blind. Where the discriminating token is too weak to be safe (a bare `1`), the variant
  shares its parent's tier and the docs table says so.
- **AC3.13** — Special cases that contradict their VRAM band are explicit entries, not heuristics. In
  particular **GB10 (DGX Spark)** — 128 GB of *unified* CPU/GPU memory on a 20-core Grace — is
  assigned `small`, because its "VRAM" is the host RAM and its whole machine has fewer cores than the
  flagship tier would demand.
- **AC3.14** — An unknown manufacturer short-circuits to the fallback without reading the product at
  all. An empty product, a product equal to the manufacturer alone, a product equal to a lone `strip`
  token, and any product that normalizes to an empty key all yield the fallback without panicking.
  The empty case is reachable in production: the AMD/Hygon name chain can produce `""`, which is
  stamped as a label value and read back (`pkg/nodefeature/helper.go:75,93,522`).
- **AC3.15** — The lookup is a pure function: same inputs, same output, no I/O, no clock, no
  environment reads.

#### F4 — Wiring into the derived type

Acceptance criteria:
- **AC4.1** — `authorDerivedInstanceType` sizes an acceleratable type from
  `nodefeature.PresetUnitResources`, passing the flavor's `Manufacturer` and `Product`.
- **AC4.2** — A non-acceleratable flavor is unaffected: `1` CPU / `2Gi` / `100Gi`.
- **AC4.3** — No change to `InstanceTypeSpec`'s shape, the CRD schema, or protobuf. The **doc
  comments** on `api/worker/v1alpha1/instance_type.go:73-84` do change (they still promise "a derived
  InstanceType is stamped with the fixed default"), and those comments flow into four generated files
  — `api/worker/v1alpha1/zz_generated.crds.go:1292,1300`, `api/worker/zz_generated.openapi.go:4095,4102`
  and `pkg/kubeclients/applyconfiguration/worker/v1alpha1/instancetypespec.go:44,50` — so
  **`make generate` is required** and its diff must be committed.
- **AC4.4** — The same stale promise is retired from `pkg/worker/controllers/worker/node_flavor.go:330-333`
  and `pkg/nodefeature/helper.go:366-368`.
- **AC4.5** — Behaviour is otherwise unchanged: still create-only, still `AlreadyExists`-tolerant,
  still stamping the same identity/labels/DisplayName.

#### F5 — Documentation

A new `docs/` page, linked from the README doc index.

Acceptance criteria:
- **AC5.1** — It states what `unitResources` means operationally: per whole card, scaled by card
  count for a whole-card request and by the memory percentage for a slice/partition, and that it is
  read both as the defaulted request and — outside this change's scope — as an admission ceiling. It
  states explicitly that **the Kueue quota does not change**: an accelerated `ClusterQueue` covers
  only the manufacturer credits resource (`pkg/worker/controllers/worker/node_queue.go:260-277`), so
  a bigger unit spec buys no extra admission headroom.
- **AC5.2** — It carries the tier ladder with the reasoning for each value, including the flat-CPU
  rationale and its overcommit dependence (AC1.2).
- **AC5.3** — It carries a per-manufacturer table: family → tier → **the public configuration the
  assignment was anchored on**, marking rows that deviate from their VRAM band, rows written from
  vendor documentation rather than a verified product string (AC3.9), and each entry's `by` order.
  This is the only place provenance is recorded; the YAML carries none.
- **AC5.4** — It states the fallback, and the caveats an administrator will hit: presets apply only
  to InstanceTypes authored after the upgrade (immutable + create-only); deleting a derived
  `InstanceType` does **not** by itself re-author it, because `NodeFlavorReconciler` watches
  ResourceFlavors and Nodes only (`pkg/worker/controllers/worker/node_flavor.go:467-546`) — a
  subsequent flavor/node event or a restart is needed, and when it does re-author it uses the **new**
  values; a cluster running with `instance-general-resources-overcommit=false` should check that its
  nodes have the per-card CPU the tier assumes, or pre-create its own `InstanceType`; and the same
  GPU can appear twice in the aggregated cross-cluster view during the transition (see Risks).
- **AC5.5** — It names the manufacturers deliberately out of scope and why (not detectable).
- **AC5.6** — A test asserts every `family` in the YAML appears in the docs page, so a new entry
  cannot ship undocumented.

#### F6 — Tests

Acceptance criteria:
- **AC6.1** — Table-driven cases over `(manufacturer, product) → expected (cpu, ram)`, one case per
  behaviour, covering: each tier; each of the nine manufacturers; raw-vs-label-sanitized product
  forms; the variant-suffix cases; the negative-match cases; the `by`-order cases; whole-token vs
  substring (`a100-140gb` must not match `40gb`); `_`/`.` tokenization (`K100_AI` ≡ `K100-AI`); the
  A100 SKU split; the GB10 special case; an unknown product; an unknown manufacturer; an empty
  product; a product equal to the manufacturer alone; a product equal to a lone `strip` token; and a
  product that normalizes to an empty key.
- **AC6.2** — One negative test per validation rule V1–V8, each driving a purpose-built bad table and
  asserting the specific rejection — including an empty `prefix` (V6), a `by` token equal to a prefix
  token (V5), a tier CPU that parses as int64 but overflows int32 (V1), a mutated `fallback` (V1), a
  missing manufacturer (V2), a duplicate YAML mapping key (AC2.1) and a `strip`-shadowed prefix (V8).
- **AC6.3** — A **pipeline-level** corpus test: every product string named in the docs page is driven
  through the real path — detector product name → `ConstructAcceleratableNodeLabels` →
  `ExtractNodeFlavors` → `authorDerivedInstanceType` — and the stamped unit spec asserted. This is the
  only test that exercises the label sanitize-and-truncate step happening *before* normalization, and
  the only one that would catch a coverage entry written against a marketing name the detector never
  emits. It includes a product whose discriminating tail is cut by the 63-character cap, asserting the
  base tier rather than a wrong variant tier.
- **AC6.4** — An order-independence test resolves the whole corpus against an entry-shuffled copy of
  the table and asserts identical results, and a property test enumerates generated token keys
  asserting at most one entry ever matches.
- **AC6.5** — A webhook-parity gate: a table whose tier values the admission webhook would reject
  must fail **at table load**, proving a bad edit cannot reach `authorDerivedInstanceType` and turn
  into a reconcile-error requeue loop.
- **AC6.6** — Vendor-shape cases grounded in AC3.9: Ascend `910B2` (bare, from the repository
  fixture), `Ascend910_9391`, `Ascend910B4-1`, `A2G…`, `I2…`; NVIDIA sibling coexistence `H20`/`H200`,
  `A10`/`A100`, `L4`/`L40`/`L40S` (which proves V3 is token-prefix, not string-prefix); AMD
  `AMD Radeon Instinct MI300X OAM` (two marketing tokens) and a fused pci.ids-style composite;
  Cambricon `MLU370-X4` / `MLU370` / bare `MLU` (the last must be fallback); MetaX `MXC500` and
  `Meta X C500`; MThreads `Moore Threads MTT S4000`.
- **AC6.7** — The existing derived-InstanceType cases in `node_flavor_test.go` are corrected, not
  merely re-expected: the shared fixture writes the product label as `"NVIDIA A10G"`
  (`pkg/worker/controllers/worker/node_flavor_test.go:69`), a value the real label constructor cannot
  produce (it sanitizes to `"NVIDIA-A10G"`, `pkg/nodefeature/helper.go:91-94`). The fixture is fixed
  first, and once the table covers A10G the case's expected values move to that entry's tier. A
  sibling case keeps pinning the **fallback** at `4`/`16Gi` with a deliberately unknown product.
- **AC6.8** — A reconciler-level case asserts a second reconcile does not re-size an existing derived
  `InstanceType` (create-only + immutable), and that deleting it and re-reconciling re-authors it at
  the **new** values.

### Notes / Constraints / Caveats

- Go 1.25 (`.golangci.yaml`), controller-runtime; no new dependency — `go:embed` is stdlib and
  `sigs.k8s.io/yaml` is already a direct requirement (`go.mod:79`). The repo already has a `go:embed`
  precedent at `pkg/extensionroute/swagger/handler.go:34`.
- `pkg/nodefeature` already imports `pkg/device` (`pkg/nodefeature/helper.go:11`) and `pkg/device`
  does not import `pkg/nodefeature`, so hosting the resolver in `pkg/nodefeature` introduces no
  import cycle. The lookup is exported (`PresetUnitResources`) because its consumer is in another
  package, and must therefore be documented with its behaviour and constraints.
- A malformed embedded table is a programming error. Parse it in a package-level `Must`-style
  initialiser, matching the repo's existing `regexp.MustCompile` / `resource.MustParse` idiom. Note
  honestly what this buys: the failure is **fatal at package initialisation**, not at build time — a
  malformed table still compiles, so the AC6.2 tests running in `make test` and CI are what actually
  catch it before it ships.
- **The load runs before `knowns.go`'s `init()`.** Go initialises package-level variables ahead of
  every `init` function, so the manufacturer registries `pkg/nodefeature/knowns.go:130` builds are
  still empty when the table loads: calling `IsKnownAcceleratableManufacturer` from V2 makes every
  manufacturer look undetectable and the package panics on import. V2 therefore matches against a
  set built straight from the nine `Manufacturer*` constants, with a unit test asserting that set
  and `GetKnownAcceleratableManufacturers()` never drift apart.
- The webhook's unit-spec predicates cannot be shared: `pkg/worker/webhooks/worker/instance_type.go`
  imports `pkg/nodefeature`, so the dependency only runs one way. V1 restates the two rules using the
  same primitives (`strconvx.ParseInt[int32]`, case-sensitive `Gi`). The duplication is accepted
  deliberately; no cross-package parity test guards it.
- `InstanceTypeSpec` is used as a map key and must stay comparable — this change adds no field to it,
  so the constraint is met trivially.
- The RAM string must be an integer + `Gi`; several published ratios (`237.5gb`, `21.5c`) cannot be
  represented and are rounded down to the nearest tier.
- The preset must **not** key on the accelerator group ID / slug, which gains a `-<mem>` suffix when
  `GPUSTACK_DEVICES_GROUP_ID_WITH_MEMORY=true`. Key on the product.
- **`make generate` cannot run from this checkout.** It is a git worktree (`.git` is a file), and
  `go-to-protobuf` requires a working directory whose path ends in `gpustack.ai/gpustack`. Run it from
  the main checkout, or from a temporary worktree created at such a path.
- Lint constraints that shape the code: `lll` line-length 150; `godot` requires every comment to end
  in a period; `decorder` enforces init-func-first (avoided by using a package-level `var` rather than
  `init()`); `gochecknoglobals` is not enabled, so the package-level table var is fine.
- Go file naming is snake_case; generic helpers belong under `pkg/utils/*x`.

### Boundaries

- **Always:** keep the unmatched path byte-identical to today (`4` / `16Gi` / `100Gi`) and pin it in
  the table's own validation; keep the lookup pure and entry-order-independent; validate tier values
  with the admission webhook's own validators; cite a public source for every row in the docs table;
  run `make lint` after Go edits.
- **Ask first:** changing the fallback value; adding or removing a tier; adding an editable setting or
  env var to override presets; presetting `localStorage`; touching the create-only or immutability
  rules; widening scope into admission validation.
- **Never:** read node capacity, the Devices ledger, or any live cluster state inside the lookup; add
  a field to `InstanceTypeSpec`; mutate an existing `InstanceType`; invent a ratio with no public
  source; key the table on the memory-suffixable group slug; let a lookup result depend on entry
  order; use non-strict YAML decoding; add a catch-all entry whose prefix is a vendor's unknown-card
  sentinel (`mlu`).

### Risks and Mitigations

- **A table edit ships a tier the admission webhook rejects → the pool gets no `InstanceType` at
  all.** `authorDerivedInstanceType` returns the create error, `Reconcile` fails and requeues
  forever (`pkg/worker/controllers/worker/node_flavor.go:370-374`), so the whole scheduling chain for
  that pool never materializes. → V1 reuses the webhook's own validators, including the int32 domain,
  and AC6.5 gates it at load time.
- **With overcommit disabled, a preset can exceed what a node provides per card → single-card Pods
  sit `Pending`.** → `instance-general-resources-overcommit` defaults to `true`, under which an
  `xlarge` card requests only 1.2 CPU / 24Gi, so the risk is confined to clusters that turned it off.
  For those, values are taken from the low position of each product's published range, and AC5.4
  documents the check and the pre-create workaround.
- **The same GPU appears twice in the aggregated cross-cluster view.**
  `buildAggregatedInstanceTypeName` embeds the unit CPU and RAM in the aggregated identity
  (`pkg/workergateway/service/helper.go:40-48`), so after the upgrade a pre-existing pool (`4`/`16Gi`)
  and a new pool of the *same* accelerator group appear as two separate aggregated flavors with
  separate totals. → Unavoidable while old types stay immutable; documented in AC5.4 and pinned by an
  AC6.8-adjacent case so the split is tested behaviour rather than a surprise.
- **The admission ceiling widens as a side effect.** `capResourcesToInstanceType`
  (`pkg/worker/webhooks/worker/instance.go:739-760`) computes the cap from the accelerator *count*,
  which is 1 for a logical slice or a hardware partition — it is not percentage-scaled. Raising the
  unit therefore lets a 5 % slice of an `xlarge` type explicitly request up to 12 CPU / 192Gi where
  today it is capped at 4 CPU / 16Gi. This is a pre-existing modelling gap in code this spec does not
  touch; it is recorded here and left to a separate change.
- **Overcommit rewrites an explicit request.** With overcommit on (the default),
  `withGeneralOvercommit || instRess.CPU.IsZero()` (`pkg/worker/webhooks/worker/instance.go:383-392`,
  `:442-451`) means the unit-derived value replaces even a deliberately small explicit `cpu`/`ram`.
  Raising the unit therefore raises those workloads' limits. The overcommit *ratio* is unchanged
  (limit/request stays 8× for RAM), so scheduling pressure scales linearly; only the burst headroom
  grows. → Documented in F5, not designed around.
- **A vendor's product string is not what the table assumes → the whole manufacturer silently falls
  back.** Multi-word brands defeat the manufacturer strip (AC3.3), AMD/Hygon names vary with the
  host's pci.ids vintage, and three vendors have no verified string in this repository at all. → The
  fallback is exactly today's behaviour, so a miss is a non-event rather than a regression; AC3.9
  grounds every entry in what a detector actually emits, AC6.6 pins the awkward shapes, and the docs
  page marks unverified rows as such.
- **An entry over-matches and stamps a big preset on a small card.** → V3 makes multi-entry matching
  impossible, and AC3.10 negative cases plus the AC6.4 property test pin it.
- **A future data edit re-sizes an unrelated pool.** → V1–V8 reject the table at load and AC6.4 proves
  order-independence, so a bad edit fails the test suite rather than silently shifting a lookup.
- **Existing pools keep 16Gi indefinitely, and delete-to-re-author is not a clean procedure.** →
  Create-only plus immutability means an upgraded cluster carries mixed old/new sizing, and deleting
  the derived type does not enqueue the reconciler (it watches ResourceFlavors and Nodes only), so a
  flavor/node event or a restart is needed and the recreate silently jumps to the new values.
  Documented as a named caveat in AC5.4; no migration is offered.
- **A rolling upgrade races on a previously unseen pool.** Whichever binary first authors a new type
  permanently fixes its sizing, so a pool first seen by the old binary keeps `4`/`16Gi` forever. →
  Accepted; the window is one rollout and the outcome is the zero-regression value.
- **Normalization collisions can pool two products under one accelerator key**, after which the
  reconciler trusts the first contributor's product (`pkg/worker/controllers/worker/node_flavor.go:149-153`)
  and that arbitrary product picks an immutable tier for the whole pool. Character-dropping in the
  label sanitizer fuses tokens without inserting a separator (`pkg/kubemeta/label.go:142-182`), which
  makes such collisions more likely for composite pci.ids names. → Pre-existing behaviour of the
  grouping key, not introduced here; the AC6.3 pipeline test covers the realistic product corpus.
- **The table goes stale as new cards ship.** → It is additive and the fallback is safe; the docs page
  states the table is best-effort and lists the review trigger (a new detector or a new mainstream
  product).

## Design Details

### Commands

**Environment: local.** `darwin/arm64`, the working checkout at
`/Users/thxcode/orca/workspaces/gpustack/refactor-unitresource-preset`. The whole module — including
the CGO vendor detectors — builds and tests locally; this change touches no image build, no Helm
chart and no in-cluster behaviour, so no Linux host, no accelerator host and no Kubernetes cluster is
needed. Smoke-checked: `go test ./pkg/worker/controllers/worker/...` → `ok … 2.7s`.

```bash
# Inner loop.
go test ./pkg/nodefeature/... ./pkg/worker/controllers/worker/...
go test -run 'TestPresetUnitResources' ./pkg/nodefeature/...
go vet ./pkg/nodefeature/... ./pkg/worker/controllers/worker/...

# Gates (hack/*.sh behind the Makefile).
make lint     # golangci-lint ./... over the whole module; allow >2 min on a cold cache
make test     # go test -v -failfast -race -cover -timeout=30m ./...

# Required once, for the API doc-comment change only (AC4.3) — NOT from this worktree.
cd <a checkout whose path ends in gpustack.ai/gpustack> && make generate && git diff --exit-code
```

### Project Structure

```
pkg/nodefeature/
  knowns.go                           # the existing per-manufacturer registries this sits beside
  helper.go                           # retire the "fixed default" promise on the NodeFlavor comment
  unit_resources_preset.go            # go:embed + strict decode + V1..V8 + normalize + resolve
  unit_resources_preset.yaml          # the embedded table (tiers, strip, families)
  unit_resources_preset_test.go       # table-driven, validation, order-independence, property, vendor shapes
  unit_resources_preset_docs_test.go  # docs <-> table sync guard
pkg/worker/controllers/worker/
  node_flavor.go                      # authorDerivedInstanceType — call site; defaultResources replaced
  node_flavor_test.go                 # derived-InstanceType cases; fixture label corrected; pipeline corpus
api/worker/v1alpha1/
  instance_type.go                    # retire the "fixed default" doc comment (+ three generated files)
docs/
  instance-type-unit-resources.md     # ladder, per-product table with sources, caveats
README.md                             # doc-index entry
```

### Code Style

```go
//go:embed unit_resources_preset.yaml
var unitResourcesPresetYAML []byte

// unitResourcesPresetTable is the parsed and validated preset table. A malformed table is a
// programming error, so loading it panics rather than degrading silently; the validation tests are
// what catch such an edit before it ships.
var unitResourcesPresetTable = mustLoadUnitResourcesPresets(unitResourcesPresetYAML)

// PresetUnitResources returns the per-whole-card CPU/RAM to stamp on a derived acceleratable
// InstanceType for the given accelerator manufacturer and product. The product is normalized to a
// token key and matched against at most one prefix entry, whose ordered variant list selects the
// tier. An unknown manufacturer, an unmatched product or an empty input yields the fallback tier,
// which is what the operator stamped before presets existed. The returned values always satisfy the
// InstanceType admission webhook's unit-spec rules.
//
// Three normalization constraints are load-bearing and must not be "simplified". NormalizeName is
// called with maxLength 0, because a bounded budget truncates without preserving token boundaries
// and would drop a trailing capacity discriminator. Its manufacturer trim fires only on an exact
// buffer-length match, so a multi-word brand ("Meta X", "Moore Threads") is never trimmed and must
// be covered by a strip sequence instead. And that trim is not token-boundary aware, so a
// manufacturer name that is a leading substring of the product is removed regardless.
func PresetUnitResources(manufacturer, product string) (cpu, ram string)
```

Conventions: exported APIs documented with behaviour and constraints; concise domain names; errors
returned, never panics in control flow; comments end in a period (`godot`); lines under 150 columns
(`lll`); table-driven tests with a shared execution loop asserting observable final state.

### Worked Examples

```
("nvidia", "NVIDIA-H100-80GB-HBM3")
  normalize → h100-80gb-hbm3     strip → (none)
  entry prefix h100              no `by` → xlarge → 12 CPU / 192Gi

("nvidia", "NVIDIA A100-SXM4-40GB")
  normalize → a100-sxm4-40gb     entry prefix a100
  `by` walk: 80gb absent, 40gb present → medium → 8 CPU / 64Gi

("nvidia", "NVIDIA A100 80GB PCIe")
  normalize → a100-80gb-pcie     entry prefix a100
  `by` walk: 80gb present (first) → large → 12 CPU / 128Gi

("nvidia", "Tesla-T4")
  normalize → tesla-t4           strip → t4        (NormalizeName strips only "nvidia")
  entry prefix t4                → small → 8 CPU / 32Gi

("amd", "AMD Radeon Instinct MI300X OAM")
  normalize → radeon-instinct-mi300x-oam           (manufacturer "amd" trimmed)
  strip ×2  → instinct-mi300x-oam → mi300x-oam     (repeated stripping, AC3.2)
  entry prefix mi300x            → xlarge → 12 CPU / 192Gi

("metax", "Meta X C500")
  normalize → meta-x-c500        (the multi-word brand defeats the manufacturer trim, AC3.3)
  strip     → c500               ("meta-x" is a strip SEQUENCE)
  entry prefix c500              → large → 12 CPU / 128Gi

("hygon", "K100_AI")
  normalize → k100_ai            tokens [k100, ai]
  entry prefix k100              → large → 12 CPU / 128Gi

("ascend", "910B2")              # the repo's real fixture shape: bare, no "Ascend"
  normalize → 910b2              entry prefix 910b2 → large → 12 CPU / 128Gi

("cambricon", "MLU")             # the driver's unknown-card sentinel
  normalize → mlu                no entry (a bare `mlu` catch-all is forbidden)
                                 → fallback → 4 CPU / 16Gi

("nvidia", "NVIDIA RTX 5070")    # no entry
  normalize → rtx-5070           no prefix match → fallback → 4 CPU / 16Gi

("kunlun", "P800")               # manufacturer not detectable
  short-circuit, product never read → fallback → 4 CPU / 16Gi
```

### Implementation Plan

```
T1 ──┬─▶ T2 ──▶ T4
     └─▶ T3
```

T2 and T3 are independent: T3 documents the *mechanism*, which T1 fixes, while T2 only fills data.
T4 needs T2's final family set. T1 and T3 both touch `pkg/nodefeature`, but disjoint files.

- [x] **T1 · Preset resolver + wiring tracer bullet**
      Blocked by: None
      Owns: `pkg/nodefeature/unit_resources_preset.go`,
            `pkg/nodefeature/unit_resources_preset.yaml`,
            `pkg/nodefeature/unit_resources_preset_test.go`,
            `pkg/worker/controllers/worker/node_flavor.go`,
            `pkg/worker/controllers/worker/node_flavor_test.go`
      Gate: review
      Acceptance: the F2 table shape loads under strict decode and enforces V1–V8; the F3 pipeline
      (NormalizeName with maxLength 0 → repeated marketing-sequence strip → `-`/`_`/`.` tokenization
      → at-most-one prefix entry → ordered `by` walk) resolves; `nodefeature.PresetUnitResources`
      returns the fallback for an unknown manufacturer without reading the product, for an unmatched
      product, and for every degenerate input of AC3.14. `authorDerivedInstanceType` stamps the
      result; a non-acceleratable flavor still gets `1`/`2Gi`/`100Gi` and `localStorage` stays
      `100Gi`. The seed table carries only `nvidia` {h100, a100 with its `by` list, gb10, t4} and
      `ascend` {910b2} — deliberately **no** `a10g`, so the existing derived case keeps asserting
      `4`/`16Gi`; V2's all-nine-manufacturers rule is therefore introduced in T2, and T1's tests pin
      it against a fixture table rather than the shipped one. The A10G fixture's product label is
      corrected to the production-sanitized `"NVIDIA-A10G"` (node_flavor_test.go:69), and a new case
      asserts an H100 fixture is stamped `12`/`192Gi`. Tests cover AC6.1, AC6.2, AC6.4, AC6.5 and
      AC6.8. The stale "fixed default" sentence on `authorDerivedInstanceType` is rewritten.
      Verify: `go test ./pkg/nodefeature/... ./pkg/worker/controllers/worker/... && make lint`

- [ ] **T2 · Fill the table for all nine manufacturers**
      Blocked by: T1
      Owns: `pkg/nodefeature/unit_resources_preset.yaml`,
            `pkg/nodefeature/unit_resources_preset_test.go`,
            `pkg/worker/controllers/worker/node_flavor_test.go`
      Gate: review
      Acceptance: entries for all nine manufacturers per AC3.8, each tier assigned by the F1 method
      (low position of the published range; band as heuristic, entry as authority), including the
      AC3.11 SKU splits, the AC3.13 GB10 override, and every detector reality of AC3.9 — Ascend bare
      and prefixed forms plus the `a2g`/`i2` aliases and the `910_9391` token split, MetaX `mxc500`
      alongside `c500` and the `meta-x` strip sequence, Hygon `k100_ai`, MThreads `moore-threads`
      strip sequence, AMD's two-token marketing and its fused pci.ids composites left to fallback, and
      **no** bare `mlu` catch-all for Cambricon. V2's all-nine rule is enabled. AC6.3's pipeline-level
      corpus test and AC6.6's vendor-shape cases are added and pass in both raw and label-sanitized
      form; AC3.10 negative cases pass. Open Question 1 (the Ascend 910B outlier) is resolved and the
      choice recorded for the docs.
      Verify: `go test ./pkg/nodefeature/... ./pkg/worker/controllers/worker/... && make lint`

- [ ] **T3 · Retire the stale "fixed default" API contract**
      Blocked by: T1
      Owns: `api/worker/v1alpha1/instance_type.go`,
            `api/worker/v1alpha1/zz_generated.crds.go`,
            `api/worker/zz_generated.openapi.go`,
            `pkg/kubeclients/applyconfiguration/worker/v1alpha1/instancetypespec.go`,
            `pkg/nodefeature/helper.go`
      Gate: review
      Acceptance: the `UnitResources` and `LocalStorage` field comments
      (`api/worker/v1alpha1/instance_type.go:73-84`) and the `NodeFlavor` comment
      (`pkg/nodefeature/helper.go:366-368`) describe the preset behaviour instead of a fixed default;
      `make generate` has been run and its diff across the three generated files committed; the field
      shapes, CRD schema and protobuf are otherwise unchanged.
      Verify: from a checkout whose path ends in `gpustack.ai/gpustack`,
      `make generate && git diff --exit-code`, then `make lint`

- [ ] **T4 · Preset reference doc**
      Blocked by: T2
      Owns: `docs/instance-type-unit-resources.md`, `README.md`,
            `pkg/nodefeature/unit_resources_preset_docs_test.go`
      Gate: —
      Acceptance: the page satisfies AC5.1–AC5.5, the README doc index links it, and the sync test of
      AC5.6 asserts every `family` in the YAML appears in the page.
      Verify: `go test ./pkg/nodefeature/... && make lint`

Checkpoints: after T1 the system is working and behaviour changes only for the seeded families; after
T2 the feature is complete; after T3 and T4 the published contract and the documentation match it.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to
make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

`pkg/worker/controllers/worker/node_flavor_test.go:69` seeds the accelerator product label as
`"NVIDIA A10G"`, a value the real label constructor cannot produce — `kubemeta.SanitizeLabelValue`
turns spaces into `-` (`pkg/nodefeature/helper.go:91-94`). Every derived-InstanceType case therefore
exercises a shape production never sees. Correct the fixture to `"NVIDIA-A10G"` in T1, before any
preset entry depends on it, and add a sibling fixture with a deliberately unrecognised product so the
fallback stays pinned once A10G is covered.

#### Unit tests

- `pkg/nodefeature`: `2026-07-31` - `75.7%`
- `pkg/worker/controllers/worker`: `2026-07-31` - `63.4%`
- `pkg/device`: `2026-07-31` - `82.8%`

New unit coverage lands in `pkg/nodefeature/unit_resources_preset_test.go` (AC6.1, AC6.2, AC6.4,
AC6.5, AC6.6) and `unit_resources_preset_docs_test.go` (AC5.6), with the derived-type cases in
`pkg/worker/controllers/worker/node_flavor_test.go` extended for AC6.7 and AC6.8.

#### Integration tests

- **Pipeline corpus** (AC6.3) — drive every product string documented in the reference page through
  detector name → `ConstructAcceleratableNodeLabels` → `ExtractNodeFlavors` →
  `authorDerivedInstanceType`, asserting the stamped `spec.unitResources`. This is the only test that
  exercises the label sanitize-and-truncate step happening *before* normalization, and the only one
  that would catch a coverage entry written against a marketing name the detector never emits.
  Includes the 63-character truncation boundary case.
- **Create-only invariance and re-author** (AC6.8) — reconcile a pool twice and assert the second pass
  leaves an existing derived `InstanceType`'s unit spec untouched even when the table would now
  resolve a different tier; then delete it, drive a flavor event, and assert it re-authors at the new
  values.
- **Aggregated-identity split** — assert that two pools of the same accelerator group with different
  unit specs produce two distinct aggregated names via `buildAggregatedInstanceTypeName`
  (`pkg/workergateway/service/helper.go:40-48`), so the upgrade-transition split is tested behaviour
  rather than a field surprise.
- Concrete test names to be recorded after the implementation PR merges.

#### e2e tests

None required. The change alters one field's value at creation time and has no cluster-observable
behaviour beyond it; the existing `gpustack-operator-e2e` scheduling-chain assertions already cover
that a derived `InstanceType` materializes for a pool. When an e2e run happens on real accelerator
hardware for other reasons, two things are worth folding in opportunistically: a check that the
derived type's `spec.unitResources` equals the documented tier for that card, and a capture of the
detector's raw product string for that card into `testing/sample/devices/` — the latter is the only
way to convert an "unverified" docs row (AC3.9) into a verified one. Neither is worth provisioning a
GPU cluster on its own.

## Alternatives

- **Hosting the table in `pkg/worker/controllers/worker`, next to its only caller.** Lower ceremony
  and keeps the lookup unexported. Rejected: per-manufacturer/product knowledge is exactly what
  `pkg/nodefeature/knowns.go` already owns, and a controller package is the wrong home for a vendor
  data table. The cost is one exported symbol.
- **Ordered `(prefix, requires)` rule rows, most-specific-wins.** The first shape considered.
  Rejected: two rows sharing a prefix with different, co-occurring discriminators (`{h100, [80gb]}`
  and `{h100, [sxm]}`) both match `h100-sxm-80gb` with an identical specificity score, so the
  tie-break is undefined and no static validation catches it. Making `prefix` a unique key and moving
  discrimination into an ordered sub-table is both airtight and simpler.
- **Ordered rule list, first-match-wins.** Simpler still, rejected: correctness would rest on row
  order, so a contributor reordering or inserting a YAML row could silently re-tier an unrelated
  product.
- **A Go slice/map literal instead of embedded YAML.** Matches the repo's existing vendor-table style
  (`socNameVersionMap`, `_ManufacturerPciVendorIDMap`) and keeps compile-time type safety. Rejected in
  favour of YAML so the table reviews as data and a sizing change is not a code change; the type
  safety is recovered by strict decoding plus V1–V8.
- **Pure VRAM-derived sizing, no table.** `NodeFlavor.Memory` is already available, so a function from
  per-card VRAM to a tier would need no table and would cover unknown cards for free. Rejected: VRAM
  does not predict host RAM well (L40S at 48 GiB ships with 128–192 GiB/card, above its band),
  unified-memory parts invert the relationship entirely (GB10), some vendors report `0` cores/VRAM,
  and an implicit function is not auditable per product the way a cited table is.
- **VRAM-derived *second-level* fallback for unlisted products.** Attractive, but it would make the
  unmatched path differ from today's, breaking the zero-regression goal. Deferred; the table is
  additive, so this can be revisited once the miss rate is known in the field.
- **Faithfully reproducing the most common published ratio per product** (H100 → 24c/256Gi). Highest
  fidelity, rejected: the unit spec is also the defaulted Pod request, so with overcommit disabled a
  node leaner than the reference instance would leave the pool's Pods `Pending`.
- **Clamping the preset to the authoring node's actual per-card capacity.** Safest against `Pending`,
  rejected for this iteration: it makes the lookup impure and couples InstanceType authoring to node
  capacity, while a pool spans many nodes and the type is created once from whichever node was seen
  first. Revisit if `Pending` from over-sized defaults is observed.
- **Exact full-string matching on the raw vendor product.** Simplest, rejected: one chip shows up under
  many strings across drivers and SKUs, so the miss rate would be high.
- **Carrying the provenance citation in the YAML.** Rejected: it doubles the file with prose that
  belongs in the docs page; AC5.6 keeps the two in sync instead.
- **Percentage-scaling the admission ceiling for slices and partitions** so a 5 % slice cannot request
  a whole card's CPU/RAM. A real defect, but an admission behaviour change that would reject requests
  accepted today — out of this spec's scope, recorded under Risks.

## Open Questions

1. Ascend 910B tiering has one known outlier: the mainstream Atlas 800T A2 is 8 cards / 1536GB
   (24c/192g) but the Huawei KunLun G5680 V2 variant is 8 cards / 512GB (24c/64g). `large` (128Gi)
   fits the former and not the latter. Resolve in T2: either accept `large` and record the outlier in
   the docs table, or pull the whole 910B family down to `medium`.
2. Whether the docs page should recommend anything at all for an administrator stuck on an existing
   pool's `4`/`16Gi`. Deleting the derived type does not re-author it on its own, and no migration is
   offered, so the honest answer may be "pre-create the type on a fresh pool, otherwise live with it".
3. Iluvatar, MThreads and THead have no product string pinned anywhere in this repository, so their
   coverage is written from vendor documentation and cannot be verified here. Worth a follow-up that
   captures one `testing/sample/devices/` fixture per detector from live hardware, which would also
   let the AC6.3 corpus test cover them for real.
4. Out of scope but noticed while grounding this spec, and worth its own issue: the accelerator
   feature-label **key** is built as a group ID budgeted to 63 characters
   (`pkg/device/helper.go:51`) plus a `.product` / `.memory` / `.cores` / `.count` suffix
   (`pkg/nodefeature/helper.go:70-88`), which can exceed the 63-character limit on a label key's name
   segment. If it fires, the node update fails and the pool gets no acceleratable labels at all. Not
   verified against the label-patching call site.
