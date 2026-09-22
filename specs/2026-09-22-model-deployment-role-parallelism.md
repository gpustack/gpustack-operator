# Spec: the transfer leg follows the parallelism declared in extraArgs

Status: Building
Blocked on: nothing — this revision was ratified on 2026-09-22, the parse-not-field direction
chosen on gpustack/gpustack-operator#501 and the plan below confirmed. Implementation proceeds
under this document.
Type: Feature

## Summary

A `ModelDeployment` role declares its parallelism the way it always has — through `extraArgs`
(and, for one vLLM degree, a literal `env` entry). What changes is that the operator now READS
that declaration. A per-engine parse of the role's own argument list extracts the declared
degrees, and the vLLM-Ascend prefill/decode transfer leg renders its `tp_size`/`dp_size` blocks
from the parse of BOTH roles instead of the hard-coded `1/1` it writes today
(`pkg/worker/kvcache/inject/vllm.go:247-248`).

The contract is one sentence: **only what is explicitly written on the books is honored.** A
degree that reaches the engine through any other path — a `--config` file, an image entrypoint,
a value the engine computes — is invisible to the operator, and the leg renders `1/1`. What
happens then depends on the shape, because the connector's startup assert compares the DOCUMENT
against itself — the keys exist, prefill ≥ decode — and never the document against the engine's
real degrees (`mooncake_connector.py:1982-1986`, `:2066-2084` at v0.23.0), while the pull layout
it computes mixes the engine's real TP with the document's prefill TP (`:2059-2062`). An
under-declared pair whose document decode exceeds its prefill trips that assert and crash-loops;
a SYMMETRICALLY under-declared pair passes it and pulls the wrong blocks, answering requests as
it does. The contract is therefore not a nicety in front of a safety net — it is the whole
guarantee. The operator composes nothing and guesses nothing; it relays the author's own numbers
into the one document that must agree with them, and reading the books honestly is the only
protection there is.

Both halves are filled from both roles in the same change — never the local side alone — because
a renderer that learns one side and assumes the other manufactures exactly the quiet shape
above: the assert sees a self-consistent document, the layout is wrong, and the same deployment
goes on answering requests while moving the wrong blocks.

This revision replaces the structured-field design this file previously held. The field was
rejected on the issue; Motivation carries the reasons and the evidence.

## Motivation

### The gap is structural, and the code now says so

The operator deliberately composes no parallelism argument
(`pkg/worker/controllers/worker/model_deployment_render.go:601`): the degrees do not decompose
from `size`, so a formula would be a guess that runs instead of an error. That rule is right, and
it is exactly what makes this a gap: the author says the degrees through `extraArgs`, the
operator does not read `extraArgs`, and `inject.Input` carries no parallelism — so the one
renderer that MUST know the numbers fills in a constant. The constant's own comment states the
limit plainly (`vllm.go:156-161`): "a role widened by hand through ExtraArgs makes this wrong,
which is a stated limit of the Ascend leg."

A role serving a model too large for one accelerator is the ordinary case. On Ascend today that
role's transfer leg contradicts the engine it serves, and the failure shape is worse than the
crash this document previously described: the connector's startup assert checks the document
against itself — the keys exist, prefill ≥ decode (`vllm_ascend` `mooncake_connector.py:1982-1986`,
`:2066-2084`, read at v0.23.0 and cited at `vllm.go:157-158`) — and a hard-coded `1/1` passes it
on any symmetrically widened pair. The pull layout is then computed from the engine's real TP
and the document's prefill TP mixed (`:2059-2062`): the pair comes up, answers requests, and
moves the wrong blocks. The crash-loop exists only where the document's decode exceeds its
prefill. So the gap is not a loud failure waiting to be silenced; it is a quiet wrong answer
already shipped, and any partial fix — one side learned, the other assumed — manufactures more
of exactly that shape.

### What the two designated precedents actually do

Both were read at the pins the task names. Neither declares a structured parallelism field; both
live on raw per-role args. The difference between them is what the operator does with the args.

**dynamo** (main @ f3c7c225aa) parses them back — the model this spec follows. Its only
`tensorParallelSize` fields are deprecated checkpoint-identity fields
(`deploy/operator/api/v1beta1/common.go:523-535`); the live declaration is free-form container
args per component, one component per role. The operator scans those args for
`--tensor-parallel-size`/`--pipeline-parallel-size`, computes a world size, and injects the
collective-wiring flags (`backend_vllm.go:680-692`, `:386-404`, `:600-654`). The maintenance tax
is visible in the same file: a targeted dedup against the profiler's own injection (`:622-625`),
last-wins-by-append for everything else, no arg-conflict gate, and no cross-check of declared
degrees against requested GPUs. The lesson this spec keeps and the tax this spec declines are
both in there. One more dynamo fact bears on the rejected alternative: its structured
checkpoint-identity fields were retired in favor of a hash OF THE RENDERED SPEC (commit
`1a0f6c5d60`) — a declared copy drifts; a read of the books cannot.

**llm-d** (main @ 1e9a86a3) is raw args with no reader at all, and its documents record what that
costs. Heterogeneous P/D parallelism is the documented norm (`guides/pd-disaggregation/
README.md:39`), the cross-side constraints exist only as README warnings (decode TP ≥ prefill TP
for NIXL, `docs/operations/disaggregation/vllm.md:213-237`; equal TP for Mooncake, `README.md:94`),
and where a second consumer needs the number a human copies it: the routing sidecar's
`--moriio-tp-size` carries the comment "MUST match the vLLM TP"
(`guides/pd-disaggregation/modelserver/amd/vllm/moriio/amd-ci-1p1d-tp8/patch-1p1d-tp8.yaml:79-96`).
That hand-copy is exactly the position this operator's renderer is in today — a second consumer
of the number, with no way to read it.

The three questions the task says the design must answer, answered per precedent:

- **Which layer**: both declare per role (dynamo per component, llm-d per role's container args).
  P and D legitimately differ, so nothing deployment-level can express them. The declaration
  therefore STAYS per role, in that role's own `extraArgs`; only the PAIR resolution happens at
  the deployment, the one place both roles are visible.
- **Both sides expressed**: yes, as two independent per-role declarations. dynamo's operator
  reads only the local component's args because its wiring is local; this operator's document
  names both halves, which is why F2 parses both roles.
- **Duplicate declaration**: dynamo appends after user args and dedups exactly one flag; llm-d
  relies on a comment. Neither refuses a contradiction. Under this spec "duplicate" has two
  shapes, each resolved exactly as the engine resolves it: the same flag twice in one list is
  last-wins — precisely what argparse does, so the relay and the engine cannot diverge — and DP
  stated both as a CLI flag and as `VLLM_DP_SIZE` follows vLLM's own precedence, where the CLI
  path wins and the variable is only the fallback (`parallel.py:937-943` at the pin). The parser
  mirrors that order rather than inventing a conflict rule, so a doubly stated degree still
  cannot put the relay and the engine on different numbers.

### Why parsing and not a field

The previous revision of this document proposed a structured `roles[].parallelism` field that
renders the engine flags. The issue's author rejected it, for two reasons that the inventory
below substantiates:

1. **The vocabulary moves.** Context parallelism in both its forms (DCP, PCP) is younger than
   this CRD. Expert parallelism is a boolean MODE in vLLM (`--enable-expert-parallel`, "instead
   of tensor parallelism for MoE layers", `vllm/config/parallel.py`) but an integer DEGREE in
   SGLang (`--ep-size`, default 1). A struct must pick one shape per concept and freeze it into a
   released API, while the engines keep moving.
2. **Support differs per version.** A field the API accepts but the deployed engine's version
   rejects is a new failure mode the operator invited. A parse never has this problem: a flag the
   table does not know passes through to the engine untouched — the same behavior as today —
   and recognizing a new flag is an internal table entry, not an API change.

The inventory also shows the cost parsing must NOT pay. dynamo's targeted dedup exists because
dynamo INJECTS flags into the user's list. This operator injects nothing: the books pass through
byte-for-byte, and the relay is one-way — books to connector document, never books to rewritten
books. That removes the dedup class of bug by construction.

### The recognition contract

"On the books" has a precise meaning, and it is the whole contract:

- **Recognized**: the tokens of the one argument stream the engine actually reads, matched
  against the engine's flag table — the role's `extraArgs` for a managed role, its `command`
  for a take-over one, never both, because the renderer appends nothing to a replaced command
  line and `extraArgs` is inert beside one — plus, vLLM only, a literal `VLLM_DP_SIZE` entry in
  the role's `env`.
- **Not recognized**: everything else. Engine `--config` file contents, image-baked entrypoints,
  engine-computed values, a take-over role's `extraArgs` list, and a degree hidden inside a
  shell string or script the take-over argv merely invokes. Parsing reads the same tokens the
  container runs; a degree reaching the engine by any other path is invisible and the leg
  renders `1/1` — with the failure shapes the Summary states.
- `env` needs no valueFrom exclusion: `ModelDeploymentEnvVar` is Name+Value by API shape
  (`api/worker/v1alpha1/model_deployment.go:527-534`), so every env entry is literal by
  construction. A ConfigMap or Secret cannot reach the role's environment through this API at
  all; one reached around it (an image that reads its own files) is simply not on the books.

The guarantee this buys: KV-transfer correctness has exactly one way in — the author writes the
degree, the operator parses it, the document carries it. Every other way is invisible, and its
failure shape is the one the Summary names honestly: loud only where the document's decode
exceeds its prefill, a wrong pull layout where the widening is symmetric. The contract exists
precisely because the assert cannot be that guarantee.

## The engine flag inventory

The parse tables below are built from the two upstream source trees the task names, read at the
pins in parentheses. Each row gives every accepted spelling, the kind, the default, and what the
degree means for the books. "Relay" marks the two degrees the connector document has keys for —
the only ones the leg consumes.

### vLLM (worktree @ 8da75d61; `vllm/engine/arg_utils.py:1056-1259`, `vllm/config/parallel.py`)

Serves both vLLM-flavored engines (`vllm`, `vllm-ascend`). vLLM's parser is
`FlexibleArgumentParser` (`vllm/utils/argparse_utils.py:119-120`): it rewrites UNDERSCORE
spellings to dashes before parsing (`argparse_utils.py:336-352`), accepts each long flag in
either spelling, `--flag value` and `--flag=value`, resolves a repeated flag last-wins, and —
being argparse underneath — accepts any UNIQUE-PREFIX abbreviation of a long flag
(`--tensor-parallel 2` parses as `--tensor-parallel-size 2`). The table must cover all of it;
dynamo's underscore-spelling miss is the documented cost of covering less.

| flag | aliases | kind | default | semantics |
|---|---|---|---|---|
| `--tensor-parallel-size` | `-tp`, underscore spelling | degree, RELAY | 1 | KV shard count; process world = TP×PP |
| `--pipeline-parallel-size` | `-pp`, underscore | degree | 1 | layer stages; each rank holds a device |
| `--data-parallel-size` | `-dp`, underscore | degree, RELAY | 1 | independent engine cores, each TP×PP wide |
| `--data-parallel-size-local` | `-dpl`, underscore | degree | engine-args `None`, then inferred | DP cores placed on THIS node; a declared value **above zero** IS the size check's DP factor (F5) |
| `--data-parallel-rank`, `--data-parallel-start-rank`, `--data-parallel-address`, `--data-parallel-rpc-port`, `--data-parallel-backend`, `--data-parallel-hybrid-lb`, `--data-parallel-external-lb`, `--data-parallel-multi-port-external-lb` | `-dpn`, `-dpr`, `-dpa`, `-dpp`, `-dpb`, `-dph`, `-dpe`, `-dpm`, underscore (arg_utils.py:1128-1187) | wiring | — | DP collective wiring; any of them present silences the size check |
| `--prefill-context-parallel-size` | `-pcp`, underscore | degree | 1 | expands the process world, NOT the KV shard count (parallel.py comment) |
| `--decode-context-parallel-size` | `-dcp`, underscore | degree | 1 | shards the decode KV, but reuses TP ranks — no world expansion |
| `--enable-expert-parallel` | `-ep` | mode (store_true) | off | EP INSTEAD of TP for MoE layers; no degree exists |
| `--nnodes`, `--node-rank`, `--master-addr`, `--master-port` | `-n`, `-r` | wiring | — | presence silences the size check (multi-Pod collective) |

Env: `VLLM_DP_SIZE` (`vllm/envs.py:177`, consumed in `parallel.py:937-943`: the CLI path takes
precedence outright, and the variable is read only as the fallback when no CLI DP arrived). It is
the only degree with an env spelling; TP and PP have none. A literal `VLLM_DP_SIZE` in
`roles[].env` is a DP declaration at the engine's own precedence — the parser mirrors that order
(CLI over env) instead of inventing a conflict rule, so a doubly stated DP cannot put the relay
and the engine on different numbers. A malformed env value is an error only when the env is the
effective DP source.

### SGLang (worktree @ 2dee23a8; `python/sglang/srt/arg_groups/fields/parallel.py`, `arg_utils.py:351-353`)

SGLang derives each flag as `--` + field name with dashes, plus the aliases each field lists;
there is no underscore acceptance and no degree-bearing env var (`python/sglang/srt/environ.py`
carries none). `--flag value`, `--flag=value`, last-wins, and unique-prefix abbreviation behave
as in plain argparse.

| flag | aliases | kind | default | semantics |
|---|---|---|---|---|
| `--tp-size` | `--tensor-parallel-size` | degree | 1 | under DP attention this IS the world size |
| `--pp-size` | `--pipeline-parallel-size` | degree | 1 | |
| `--dp-size` | `--data-parallel-size` | degree | 1 | full engine replicas WITHOUT DP attention |
| `--ep-size` | `--expert-parallel-size`, `--ep` | degree | 1 | a degree here — inside the TP world |
| `--dcp-size` | `--decode-context-parallel-size` | degree | 1 | |
| `--attn-cp-size` | `--attention-context-parallel-size` | degree | 1 | inside the TP world |
| `--moe-dp-size` | `--moe-data-parallel-size` | degree | 1 | inside the TP world |
| `--enable-dp-attention` | — | mode | off | "dp size should equal tp size"; tp_size is the world size |
| `--enable-prefill-cp` (+`--cp-strategy`) | — | mode | off | prefill context parallelism as a mode, not a degree |
| `--dwdp-size` | — | mode-bearing degree | 1 | must equal tp_size; forces dp-attention on (parallel_hook.py:306-394) |
| `--nnodes`, `--node-rank`, `--dist-init-addr` | `--nccl-init-addr` (alias of dist-init-addr, parallel.py:39-45) | wiring | — | presence silences the size check |

SGLang has no relay row: its disaggregation document carries no tp/dp keys today, so its parse
feeds validation and the size check only. The table exists anyway — the parse is per-engine and
symmetric, and a future SGLang document with parallel keys gets its relay as an additive change.

### What the parser is and is not

The token stream is the YAML list: each `extraArgs` entry is already one argv element, so there
is no quoting, escaping, or shell word-splitting to reproduce — the shell-like work the issue
names reduces to recognizing spellings, `--flag value` versus `--flag=value`, arity (degree
flags take a value, mode flags take none), and last-wins. Three more argparse behaviors are
reproduced exactly, because each decides whether a degree is seen:

- **Unique-prefix abbreviation.** A token that prefixes exactly one spelling in the engine's
  table parses as that flag — `--tensor-parallel 2` is TP=2 to vLLM, so it is TP=2 to the
  parser. A token prefixing MORE than one table spelling (`--data-parallel 2` in vLLM) is
  ambiguous and passes through unrecognized: the engine's own parser rejects it the same way,
  so pass-through can never become a quiet wrong answer. The table's spelling set is a subset
  of the engine's, so the parser may accept an abbreviation the engine finds ambiguous against
  flags the table does not list — that direction fails loudly at engine start, never quietly.
- **Single-dash aliases, abbreviations and concatenation.** A registered alias takes both value
  forms (`-tp 2`, `-tp=2`); `-tp2` is not an option argparse recognizes, so it passes through.
  The two single-LETTER registrations (`-n`, `-r`) also take a concatenated value (`-n2`) —
  argparse's short-option rule. Three tokens are no spellings of their own but unique
  single-dash abbreviations the engine resolves — `-t` of `-tp`, `-pc` of `-pcp`, `-e` of
  `-ep` — each verified unique against the engine's full single-dash option set, and each
  matched bare only: argparse does not split `=` for an unregistered key, so `-t=2` passes
  through to the engine's own refusal. `-dc` belongs to `--diffusion-config` outright (an exact
  spelling shadows the `-dcp` prefix) and `-d` prefixes a dozen options, so the table recognizes
  no single-dash abbreviation of its own. A future engine option colliding with one of the three
  turns the form into the engine's own ambiguous refusal — loud, never quiet.
- **The engine's integer alphabet.** Values parse as the engine's own `int()` parses them:
  whitespace-padded and single-underscore forms are accepted (`" 3 "` is 3, `"1_0"` is 10, `_3`
  and `3_` are refused), one leading sign is allowed, and a value beyond the host int is an
  out-of-range error. The same alphabet reads `VLLM_DP_SIZE` (`envs.py:1455` runs the value
  through the same `int()`).
- **A bare `--` ends flag recognition**, and a value token that looks like another flag is not
  consumed as a value (argparse refuses `--tensor-parallel-size -x` as a missing argument) — a
  known degree flag so orphaned is the missing-value error class. A token that looks like a
  NEGATIVE NUMBER IS consumed, exactly as argparse consumes it, and fails the below-1 check
  instead — the engine's own `ge=1` validation rejects the same token, so both readers refuse
  the same input for the same reason.

Unknown tokens are not the parser's business beyond this: they pass through to the engine
untouched, which is the property that makes this model age-proof.

## Proposal

### Core Features & Acceptance Criteria

**F1 — A per-engine parse of the books, built to be the only reader.**
A new internal unit beside the connector helpers
(`pkg/worker/controllers/worker/model_deployment_connector.go:339-388`, whose
`ModelDeploymentArgName` is the one-token version of this) holds three pieces, and the split is
what keeps the parse both robust and forward-looking:

- **The tables** — one per engine, from the inventory above. A row is one flag: every accepted
  spelling, its arity (degree flags take a value, mode flags take none), and its kind (degree,
  mode, wiring); the env table has the same shape keyed by variable name. The vLLM table serves
  both vLLM-flavored engines; an engine with no table parses to all ones without error.
  Adding an engine is adding a table, adding a flag is adding a row — nothing else in the parser
  changes.
- **The parser** — `modelDeploymentDeclaredParallelism(engine, extraArgs, env)`, implementing
  the inventory's semantics exactly: both value forms, last-wins, underscore spellings only
  where the engine's own parser accepts them, unique-prefix abbreviation with ambiguous-prefix
  pass-through, the single-dash abbreviations (`-t`, `-pc`, `-e`, matched bare only) and
  single-letter concatenation (`-n2`, `-r0`) the engines themselves resolve, values read in the
  engine's own integer alphabet, mode flags consuming no token, a bare `--` ending recognition,
  `VLLM_DP_SIZE`
  at the engine's own precedence below the CLI flag. Robustness is defined against the whole
  input space, not a happy path: the parser NEVER errors on a token it does not recognize — an
  unknown flag, an ambiguous prefix, a positional argument, anything after a bare `--`, an
  empty entry all pass through as the engine's own business — and it errors only on a KNOWN
  degree flag whose value is missing (a following token that looks like another flag is never
  consumed), non-integer or out of range, or below 1 (a negative number IS consumed, as argparse
  consumes it, and lands in this class), and on a `VLLM_DP_SIZE` that is malformed while being
  the effective
  DP source. Those two classes are the entire error set; a CLI-versus-env DP pair is not one,
  because the engine itself ranks them.
- **The result** — one struct carrying EVERY recognized declaration: all degrees (not only the
  two the relay consumes today), the mode flags seen, and the wiring-flag presence F5 reads. A
  future consumer — an SGLang relay when its document grows parallel keys, a new admission
  check — reads the struct and never touches the parser. The SGLang table ships from the start
  precisely so that day is a table-row day, not a parser day.

Acceptance: a table-driven suite over the spelling matrix — every alias of every row, both
value forms, underscore acceptance and rejection per engine, a unique prefix resolving and an
ambiguous one passing through, `-tp=2` accepted and `-tp2` passed through, the three single-dash
abbreviations bare-only (`-t 2` resolving, `-t=2` passing through), single-letter concatenation
(`-n2`, `-r0`), `-dc` shadowing the `-dcp` prefix, recognition stopping
at a bare `--`, a negative value refused as below 1, the integer alphabet (`" 3 "` and `"1_0"`
accepted, `_3` refused, a host-int overflow refused as out of range), a repeat resolving
last-wins, a mode flag
NOT consuming the next token, the pass-through rows (unknown flag, positional, empty entry),
each error class, CLI-over-env and env-fallback DP precedence with the local-share corners, and
a table-less engine parsing
to all ones.

**F2 — The Ascend leg renders both halves from both parses, in one change.**
`renderModelDeploymentPods` (`pkg/worker/controllers/worker/model_deployment.go:1024-1116`)
resolves the pair: the prefill kind's parse and the decode kind's, each read off its role's own
argument stream (`ModelDeploymentRoleArgs` — `command` for a take-over role, `extraArgs`
otherwise). Admission already refuses a second role of either kind (`validateModelDeploymentRoleKinds`),
and the resolver still pins its own rule — declaration-order first — so a render never leans on
a check it does not own; a half with no role or no declared degrees resolves to 1/1. The
resolution runs only when the deployment declares both halves — a one-half deployment renders no
transfer document, so the pair has no consumer and an unreadable declaration there stays the
engine's own startup refusal rather than failing the reconcile of every unrelated field;
admission refuses the same books on a new object all the same (F4). The pair
travels `ModelDeploymentConnectorInput` → `inject.Input` → `renderVLLM`, whose Ascend leg fills
the `Prefill` and `Decode` blocks from it instead of the inline literals at `vllm.go:247-248`.
The document a prefill Pod carries and the one a decode Pod carries hold IDENTICAL blocks — as
today; only `kv_role` differs. The zero pair renders `1/1`, so the annotation-driven Pod path
(`pod_kv_cache_resolve.go`), which has no roles to parse, renders exactly what it renders today.
The native vLLM leg's document has no parallel keys and does not change. Acceptance: unit pins
decode both Pods' `--kv-transfer-config` and assert the blocks equal — a mutation filling only
the local side turns that assertion red, not a compile error; a prefill-declared/decode-blank
pair renders `{2,1}/{1,1}`; the annotation path's render is byte-identical.

**F3 — The recognition contract is stated where a user reads it.**
`docs/reference/model-deployment.md` gains the contract paragraph, stated without a safety net
that does not exist: the transfer leg knows exactly the parallelism written in `extraArgs`
(and, for vLLM DP, a literal `VLLM_DP_SIZE` env); parallelism from any other path is invisible
to it and keeps the `1/1` behavior; and the connector's startup assert compares the document
against itself, so an invisibly widened pair fails loudly only when its document decode exceeds
its prefill — a symmetrically widened pair starts, answers, and pulls a wrong layout. The page's
now-false statements are repaired in the same change: that the operator composes no
`--tensor-parallel-size` (it still composes none — it READS the author's), that the leg renders
both halves `1/1` for every shape, and the framing of parallelism degrees as invisible to the
operator. Acceptance: no sentence on the page still contradicts the contract; the page's size
caps hold.

**F4 — Admission refuses books that cannot be read.**
`validateModelDeploymentRoleExtraArgs` (`pkg/worker/webhooks/worker/model_deployment.go:1365`)
gains the parse's error classes as refusals, each naming the role and the flag: a known degree
flag with a missing, non-integer, or sub-1 value, and a `VLLM_DP_SIZE` that is malformed while
being the effective DP source. The books read are the role's one argument stream
(`ModelDeploymentRoleArgs`): a take-over role's Command is parsed exactly like a managed role's
ExtraArgs — a broken degree on the very line that runs is refused the same way — while the
inert ExtraArgs beside a Command are parsed by nobody and refused nothing, consistent with F3.
A doubly stated DP (flag and env) is NOT refused — the engine's own precedence resolves it and
the relay follows the engine. The owned-key table (`ModelDeploymentOwnsArg`) is unchanged, and
unknown flags stay admitted exactly as today: `vllm serve` owns rejecting what IT does not know.
Acceptance: webhook cases, one per error class, plus the admitted rows — an unknown flag, an
inert ExtraArgs beside a take-over Command carrying anything, a role on an engine with no
table, a DP stated twice the engine's own way.

**F5 — `roles[].size` gets its first check, in the unambiguous subset.**
Today size is whatever the author writes, and nothing ties it to the degrees — the issue names
this as the parse's second payoff. The check is deliberately small, refusing only configurations
that cannot start, from numbers the author already wrote. For a role with no multi-node wiring
flag present, whose per-member accelerator request `C`
(`roles[].resources.accelerator`) is a single readable count, compute the per-member engine
width `R` from the parse: vLLM `R = TP × PP × PCP × DPW`, where `DPW` is the declared
`--data-parallel-size-local` when one is written above zero — with no `--data-parallel-*`
wiring flag present that share is exactly the DP ranks this member runs — else the full
`--data-parallel-size`; a declared ZERO local size is the engine's own sentinel for DP
specified externally, so it reads as undeclared and the full width still multiplies; any
wiring flag means some of the DP width may live off this member, the table does not model
placement, and the check stays silent (DCP reuses TP ranks; EP is a mode). SGLang
`R = TP × PP × DP`, or `TP × PP` when DP attention, DWDP, MoE-DP or attention-CP says the width
lives inside the TP world. `R > C` is refused, naming `R`, `C`, and the degrees it came from —
a product that overflows an int64 still refuses, pinning one past `C` rather than wrapping, and
names no figure arithmetic cannot hold.
Everything ambiguous stays silent: wiring flags present, a mode whose width semantics the table
does not model, `C` unreadable or zero. The books are the role's one argument stream, so the
inert ExtraArgs beside a take-over Command enter no width — the Command is that role's
declaration. The check never computes a "right"
size and never reads a pool: it is the author's own books, arithmetic, and a refusal only when
the arithmetic cannot run. Acceptance: webhook cases for each refusal and each silence rule; a
TP=2 role on a 2-card request is admitted, the same role on 1 card is refused, and `size`
rescues nothing — the width is per member, so TP=2 with `C=1` and `size: 2`, no wiring, is
refused.

**F6 — Edits stay ordinary, and a degree edit rolls both roles.**
Parsed degrees render into argv-adjacent documents covered by the Pod spec hash, so an
`extraArgs` edit is a rolling replacement as before — with one honest sharpening: a degree edit
rewrites the IDENTICAL blocks both Pods carry, so it rolls the pair, not only the role whose
args changed. The `ModelDeploymentRole` doc comment claiming a container-field edit rolls that
role's replicas only (`api/worker/v1alpha1/model_deployment.go:279-283`) becomes false here and
is repaired in T3, the change that makes it false. The window in which one role has rolled and
the other has not is the mixed-shape window `spec.kvTransfer.protocol` already documents; it
closes when the second role's replacement lands. Nothing here is a shape field: `size` stays
frozen, the parse follows the edit. Acceptance: stated in the docs paragraph; a render-level
test shows the hash moves on BOTH roles when a declared degree changes on one.

### Notes / Constraints / Caveats

- **No cross-role ratio rule at admission.** Which constraint applies — vllm-ascend's
  prefill ≥ decode, native Mooncake's equal-TP (llm-d documents both) — depends on the
  connector, which depends on the pool's manufacturer, which admission cannot observe. The
  relay carries the declared values verbatim; a declared pair that violates the connector's
  ratio renders a document whose decode exceeds its prefill, which is precisely the case the
  startup assert catches. This is unchanged from the field design; the constraint's owner is
  upstream.
- **The connector's assert is upstream's.** The prefill ≥ decode rule is vllm-ascend v0.23.0
  behavior cited at `vllm.go:157-158`; this change feeds the blocks and does not re-encode the
  rule, so an upstream that relaxes it does not meet a stale refusal here.
- **vllm-ascend adds no CLI flags of its own** (verified at 5c0470d9 against
  `vllm_ascend/` and the `additional_config.md` / feature-guide docs): every Ascend-specific
  parallel knob — the fine-grained TP component degrees, SP MoE (`enable_flashcomm1`), DSA-CP,
  shared-expert DP, KVPP — arrives inside the JSON value of the core `--additional-config`
  flag, a path the parse does not read by contract (an opaque value, like an env from a
  ConfigMap). The vLLM table therefore covers the Ascend engine's command line unchanged, and
  those degrees stay invisible to the relay and the size check — safely: they shard within the
  ranks the core TP/DP degrees already describe, so the document's tp/dp comparison is
  unaffected. At 5c0470d9 the connector's block reader sits at
  `kv_p2p/mooncake_connector.py:2214-2233` and also accepts optional `pp_size` /
  `pp_layer_partition` keys (defaulting to 1), so the tp/dp-only document this change renders
  stays valid there; relaying PP is a future consumer's key to add.
- **DCP and PCP are parsed but not relayed.** The connector document has keys for tp and dp
  alone (`vllm.go:162-165`), and vllm-ascend v0.23.0 predates both context-parallel modes. They
  are in the table so the books stay readable and the size check stays honest — PCP expands the
  process world, DCP does not — and declaring them on an Ascend pair is outside the contract:
  not refused (admission cannot see the manufacturer), not relayed (there is no key), and the
  connector's assert decides.
- **EP needs no refusal.** vLLM's `--enable-expert-parallel` is a mode with no degree, so there
  is nothing to relay and nothing to check; SGLang's `--ep-size` is a degree inside the TP
  world, so it feeds the size formula and nothing else. The per-engine tables carry the
  difference; no rule pretends the two spellings mean the same thing.
- **Two consumers, one read.** The relay (F2) and the checks (F4, F5) run the SAME parse over
  the same list. A future consumer does not get a second parser; it calls the one function and
  reads the one struct, which grows a field the day a table row lands — so a consumer written
  later starts from the full picture, and two readers of the books can never drift apart.

### Boundaries

- **No argument synthesis.** The operator writes no parallelism flag into argv, now or in this
  change's future: the author writes them, the relay reads them. The one-way rule is the design.
- **No multi-Pod collective reasoning.** Wiring flags (`--nnodes`, rank assignment) silence the
  size check and are otherwise passed through; rendering or validating a collective is a feature
  of its own.
- **The raw-Pod annotation path is untouched.** It has no roles and no books; its leg stays
  `1/1`, byte-identical.
- **No status surface.** The declaration lives in `extraArgs`, the relay in the connector
  document; both are readable on the Pod. No status field echoes either.
- **No scheduling change.** Nothing here places Pods or reads topology.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| The assert is document-local, so the contract is the only guard against a quiet mis-pull | Not a mitigation but the design: F3 states the assert's actual scope where users read it; the parse covers the whole on-book space so "the books" is never the hard way; and a take-over role's books are read off its own command line, so the document never claims 1/1 over a degree written on the line that runs |
| The table drifts from the engine's spellings | Every row cites its upstream file:line and repo pin; a missed NEW flag degrades to off-book behavior (pass-through, `1/1`, the contract's failure shapes), never to a mis-rendered document |
| A deployment sets parallelism off the books (config file, image default) | Off-book parallelism is invisible by design; the contract (F3) states the two failure shapes honestly — loud only when the document's decode exceeds its prefill, a wrong pull layout when symmetric. This predates the change: the parse only ADDS recognition, never removes it |
| The parse diverges from argparse on an edge (value forms, last-wins, underscore, prefix abbreviation, `--` termination) | The semantics are copied from the two parsers' own sources and pinned by the spelling-matrix suite, one row per behavior; ambiguous prefixes pass through to the engine's own rejection, so divergence degrades loud, not quiet |
| An author declares a relay degree on one half only | Declaring the prefill half alone is a legitimate shape (prefill ≥ decode is the connector's direction); declaring the decode half alone renders a document the startup assert catches. An admission ratio rule is impossible (manufacturer is invisible there), recorded in Open Questions |
| `main` advances under this branch before it opens | Process, not design: rebase before opening. The stacked arrangement this work started under is already resolved — PR #504 squash-merged as `9c64b7ec` and the branch now sits on `origin/main` directly |

## Design Details

### Commands

Environment: LOCAL for everything below (smoke-checked 2026-09-22 — the three test packages run
green on this machine; baselines inject 89.0%, controllers 80.6%, webhooks 91.4%). T6's
hardware check is the only REMOTE step: the cluster of the CURRENT RKE kube context, available
on demand (confirmed 2026-09-22), on its eight-NPU 910B2 host, NPUs 0–5 only.

```bash
go test ./pkg/worker/kvcache/inject/... \
        ./pkg/worker/controllers/worker/... ./pkg/worker/webhooks/worker/...
make lint
make lint docs < /dev/null
```

No API shape changes. T3 repairs one struct doc comment, which feeds the harvested CRD
descriptions, so that task runs `make generate` and verifies the tree comes back clean.

### Project Structure

```text
pkg/worker/controllers/worker/
  model_deployment_parallelism.go       # the two engine tables; modelDeploymentDeclaredParallelism
  model_deployment_parallelism_test.go  # the spelling matrix
  model_deployment.go                   # renderModelDeploymentPods resolves the pair off md.Spec.Roles
  model_deployment_connector.go         # ModelDeploymentConnectorInput carries the pair; threads it
pkg/worker/kvcache/inject/
  types.go                              # the pair type lives HERE — controllers imports inject, never the reverse; Input gains it; zero means 1/1
  vllm.go                               # the Ascend leg fills both blocks from Input
  inject_test.go                        # the both-blocks render pins
pkg/worker/webhooks/worker/
  model_deployment.go                   # F4's refusals; F5's size check
docs/reference/
  model-deployment.md                   # the contract paragraph; three statements repaired
```

### Code Style

```go
// ModelDeploymentDeclaredParallelism is the parallel shape one role's author wrote on the
// books: the degree flags of the role's engine parsed from the role's own argument stream --
// its ExtraArgs, or its Command when that replaces the line, never both -- plus the one degree
// the engine accepts as a literal environment entry. A degree nothing declares stays at its
// engine's own default of one -- the operator composes no degree of its own, because the
// transfer document must agree with the engine, and only the author's numbers can be right.
type ModelDeploymentDeclaredParallelism struct {
	TensorParallel   int
	PipelineParallel int
	DataParallel     int
	// DataParallelLocal is vLLM's --data-parallel-size-local: the share of the DP width placed
	// on this member. Zero means undeclared -- the engine then places the full width here and
	// the size check multiplies. A declared share above zero IS the per-member data-parallel
	// width -- with no wiring flag it is exactly the ranks this member runs -- so the check
	// multiplies that instead. A DECLARED zero stores the same zero: it is the engine's own
	// sentinel for DP specified externally, the check reads it as undeclared, and the env
	// precedence still honors it.
	DataParallelLocal int
	// The degrees below have no key in today's transfer document; they are carried so the size
	// check and any future consumer read the full picture without a parser change.
	PrefillContextParallel int
	DecodeContextParallel  int
	// ExpertParallel is SGLang's degree. vLLM's same-named concept is a mode, so it lands in
	// Modes instead -- the two spellings are never merged into one field. The three after it
	// are SGLang's remaining degrees: the size check branches on their presence, a future
	// consumer on their values.
	ExpertParallel           int
	AttentionContextParallel int
	MoEDataParallel          int
	DWDPSize                 int
	// Modes records the mode flags seen, by canonical spelling (expert-parallel, DP
	// attention, prefill CP); consumers branch on them, as the size check does.
	Modes []string
	// Wiring records the wiring flags seen, by canonical spelling (--nnodes and its family,
	// the --data-parallel-* collective flags, --dist-init-addr): any of them means some of
	// the width may live off this member, so the size check stays silent.
	Wiring []string
}
```

Conventions this follows: the reason for a prohibition sits beside it in the comment; refusal
messages name the working path; file names stay snake_case; no spec or issue identifiers appear
in Go comments; the vllm.go:156-161 comment is rewritten in the same change that makes it false.

### Implementation Plan

- [x] **T1 · The tables and the parser**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/model_deployment_parallelism.go`,
      `pkg/worker/controllers/worker/model_deployment_parallelism_test.go`
      Gate: review
      Acceptance: F1. The full spelling matrix passes — abbreviations long and single-dash
      (`-t`, `-pc`, `-e` bare-only), single-letter concatenation (`-n2`), the `-dc` shadow,
      `--` termination, negative values and the integer alphabet, env precedence included; the
      error classes return (missing, non-integer, out of range, below bound); an unknown flag
      and an engine with no table both parse to all-ones without error.
      Verify: `go test ./pkg/worker/controllers/worker/...`

- [x] **T2 · The renderer learns both halves**
      Blocked by: None — the pair type lives in `inject` (controllers imports inject, never the
      reverse), so this task does not touch T1's controller-side struct
      Owns: `pkg/worker/kvcache/inject/types.go`, `pkg/worker/kvcache/inject/vllm.go`,
      `pkg/worker/kvcache/inject/inject_test.go`, and the `vllm.go:156-161` comment it makes false
      Gate: review
      Acceptance: F2's renderer half. The Ascend leg writes both blocks from `Input`; the zero
      value renders `1/1`; every test row asserts BOTH blocks, never one.
      Verify: `go test ./pkg/worker/kvcache/inject/...`, and a mutation filling only the Prefill
      block turns a row red on the assertion.

- [x] **T3 · The pair is resolved and threaded**
      Blocked by: T1, T2
      Owns: `pkg/worker/controllers/worker/model_deployment.go`,
      `pkg/worker/controllers/worker/model_deployment_connector.go`, their tests, the
      `model_deployment_render.go:601` comment, and the `ModelDeploymentRole` doc comment
      (`api/worker/v1alpha1/model_deployment.go:279-283`) with the CRD descriptions a
      `make generate` run harvests from it
      Gate: review
      Acceptance: F2's threading half, F6's hash pin. Both Pods of a pair carry identical
      blocks; declaration-order first resolves a duplicated kind without leaning on admission; a
      native-vLLM pair's document is unchanged; the annotation path is byte-identical; a degree
      edit moves the hash on BOTH roles, and the doc comment says so.
      Verify: `go test ./pkg/worker/controllers/worker/...`, and `make generate` leaving the tree
      clean.

- [x] **T4 · The admission rules**
      Blocked by: T1
      Owns: `pkg/worker/webhooks/worker/model_deployment.go`,
      `pkg/worker/webhooks/worker/model_deployment_test.go`
      Gate: review
      Acceptance: F4 and F5, one case per refusal and per silence rule; unknown flags, inert
      ExtraArgs beside a take-over Command and table-less engines stay admitted; a doubly stated
      DP resolves the engine's way.
      Verify: `go test ./pkg/worker/webhooks/worker/...`

- [ ] **T5 · Repair the statements this work makes false**
      Blocked by: T2, T3, T4
      Owns: `docs/reference/model-deployment.md` alone — the code comments ride with T2/T3
      Acceptance: F3; no sentence still says the operator cannot see a role's parallelism or
      that the leg renders `1/1` for every shape; the assert's document-local scope is stated,
      and `--additional-config`'s JSON value is named as one of the invisible paths.
      The page's size caps hold.
      Verify: `make lint docs < /dev/null`

- [ ] **T6 · Gate C on hardware: an Ascend pair at tensor-parallel two, declared in extraArgs**
      Blocked by: T1–T5
      Owns: the e2e case under `.agents/skills/gpustack-operator-e2e/`
      Gate: review
      Acceptance: a prefill/decode pair whose roles carry `--tensor-parallel-size 2` comes up on
      the eight-NPU 910B2 host in the CURRENT RKE kube context (available on demand, confirmed
      2026-09-22) — no crash-loop, the document on each Pod carrying `2` in both halves;
      transfer readings present on both halves; NPUs 0–5 ONLY (indices 6 and 7 are another
      tenant's), verified by reading `npu-smi`'s process table against the operator's allocated
      annotation, two independent readings that must agree.
      Verify: the case's own report; this is the reading that promotes "above one" from inferred
      to measured.

### Test Plan

[ ] I/we understand the owners of the involved components may require updates to existing tests to make this
code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None — the three target packages already run table-driven suites with shared execution loops; this work adds
rows and cases to them rather than restructuring them.

#### Unit tests

Every added unit is covered inside its own task's suite:

- `pkg/worker/controllers/worker`: the F1 spelling matrix — every alias, both value forms, underscore per
  engine, unique-prefix resolving and ambiguous-prefix passing through, `-tp=2` versus `-tp2`, the
  single-dash abbreviations bare-only and single-letter concatenation (`-n2`, `-r0`), the `-dc` shadow,
  bare `--` termination, negative values, the integer alphabet and out-of-range refusal, last-wins, mode
  arity, boolean-wiring `=value` pass-through, CLI-over-env and env-fallback DP precedence with the
  local-share corners, each error class, the table-less engine — plus T3's thread pins: both Pods of a
  pair decode to identical
  blocks, declaration-order first on a duplicated kind, the native leg unchanged, the annotation path
  byte-identical, the F6 hash-moves-on-both pin. Baseline 2026-09-22 — 80.6%.
- `pkg/worker/kvcache/inject`: the render pins per declared pair, EVERY row asserting both blocks (the
  cross-side rule as a test property), a mutation filling only the local side shown red, the zero pair
  rendering `1/1`. Baseline 2026-09-22 — 89.0%.
- `pkg/worker/webhooks/worker`: the F4 refusal matrix (each error class, the take-over Command
  parsed as the role's books, the admitted rows — unknown flag, inert ExtraArgs beside a take-over
  Command carrying anything, table-less engine, doubly stated DP) and the
  F5 matrix (each refusal, each silence rule, `size` rescuing nothing). Baseline 2026-09-22 — 91.4%.

#### Integration tests

None — the render and admission paths are exercisable end to end in-process with fake clients, which the
unit suites already do; there is no mid-tier environment between that and a cluster.

#### e2e tests

T6 alone, on the eight-NPU 910B2 host in the current RKE kube context, NPUs 0–5 only: a prefill/decode pair
declaring `--tensor-parallel-size 2` in `extraArgs` comes up with no crash-loop and the document carrying
`2` in both halves; transfer readings present on both halves; card use cross-checked by reading `npu-smi`'s
process table against the operator's allocated annotation (two independent readings that must agree). NOT
covered on hardware: TP > 1 on a native vLLM pair (no connector-side assertion to observe beyond startup),
pipeline or context parallelism, any multi-Pod collective — all boundaries named in the design, not gaps in
the suite.

## Alternatives

### Option 3 — the structured `roles[].parallelism` field (this file's previous revision)

Rejected by the issue's author, on the two grounds Motivation substantiates from the inventory:
the vocabulary moves (DCP/PCP are younger than the CRD; EP is a boolean mode in vLLM and an
integer degree in SGLang — one struct cannot mirror both), and version-dependent support turns
an accepted-but-unrenderable value into a failure the operator invited. The precedents vote the
same way: dynamo RETIRED its structured parallel fields in favor of a hash of the rendered spec,
and llm-d never grew one. The field design also bought Gate B's single-declaration guarantee by
refusing restatements — but under the contract, restatement is not a second declaration: the
books are the only declaration, and the relay cannot disagree with them.

### Option 1 — document the limit and refuse above it

The issue's cheapest option, and weaker than it looks. The refusal it imagines cannot be aimed:
admission sees `extraArgs` but not the pool's manufacturer, so refusing the TP flag on every
vLLM prefill/decode role would break native pairs that work today, while refusing only Ascend
pairs is impossible at admission. What remains is a docs note — and TP > 1 still does not work.
Under this spec the informative half of option 1 survives as the contract paragraph (F3); the
refusal half is unnecessary, because undeclared parallelism is already loud at the connector.

### Rendering normalized flags from the parse

If the operator can read `--tensor_parallel_size 2`, why not rewrite argv to one canonical
spelling? Rejected: rewriting makes the operator a second author of the command line — it must
then own serialization of every spelling it accepts, dedup against itself, and answer for every
divergence from argparse. dynamo's dedup patch is that tax, photographed. Pass-through keeps the
engine the sole owner of its argv; the relay reads and never writes.

### A deployment-level parallelism declaration

One block beside `spec.engine` instead of per-role `extraArgs`. Rejected: prefill and decode
legitimately differ — heterogeneous parallelism is llm-d's documented norm, and vllm-ascend's
own constraint is a RATIO between the sides. Both precedents declare per role. A
deployment-level value could only express pairs that match, which is the special case, not the
rule.

## Open Questions

**Whether `size` should ever be DERIVED from the parse.** F5 checks; it does not default or
rewrite `size`. Deriving is compose-over-books — defensible, since the inputs are the author's —
but it changes what an empty `size` means, which is an API-behavior decision of its own.
Recorded; the check is the conservative first step.

**Whether admission should ever validate the pair ratio.** The connector's prefill ≥ decode rule
(and native Mooncake's equal-TP rule) is checkable on the object — both roles are right there —
but only once the manufacturer is known, which admission cannot observe. A render-time refusal
would be an error loop rather than an admission error. Recorded; a declared violation renders a
document whose decode exceeds its prefill, which is the case the startup assert catches.

**Whether `--config`-file parallelism should ever count.** The contract says no: the file's
contents are not on the books. This will be the first support question when a user sets TP in a
mounted vLLM config and the leg renders `1/1`. Recorded with its answer; the fix on that day is
education or an explicit widening of the contract, not a silent one.

**When the relay grows keys.** If the upstream connector's document grows a pp or context-
parallel key, the parse already carries the degrees and the relay becomes additive. Until then
nothing is relayed but tp and dp, because those are the only keys the document asserts.
