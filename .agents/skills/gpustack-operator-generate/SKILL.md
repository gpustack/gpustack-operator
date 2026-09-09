---
name: gpustack-operator-generate
description: "Run `make generate` to regenerate code after editing API types or webhooks. Invoke after changing files under api/ (api/v1, api/worker/v1, api/worker/v1alpha1) or webhook sources under pkg/*/webhooks/*/ — it regenerates deepcopy, register, apiservice, CRD, conversion, protobuf, and webhook code. Examples: \"regenerate the API\", \"I added a field to the Instance type\", \"update the generated code\"."
allowed-tools: "Bash(make generate*)"
model: haiku
---

# Regenerate API & webhook code

The `api/` types and `pkg/*/webhooks/*/` sources are hand-written; the deepcopy, register, apiservice,
CRD, conversion, protobuf, and webhook stubs are **generated** from them. After editing either,
regenerate to keep the generated code in sync.

## When to run

Run this skill after editing any of:

- `api/v1/*.go` — `gpustack.ai/v1` extension API types
- `api/worker/v1/*.go` — `worker.gpustack.ai/v1` extension API types
- `api/worker/v1alpha1/*.go` — `worker.gpustack.ai/v1alpha1` CRD types
- `pkg/*/webhooks/*/*.go` — admission webhook sources (e.g. `pkg/worker/webhooks/worker/`)
- `gen/api/main.go` or `gen/api/generator/*` — the generator configuration itself

Do **not** hand-edit generated files (`zz_generated.*`, `generated.pb.go`, `generated.proto`); edit
the source types above and regenerate.

## Steps

1. Run the generator:

   ```bash
   make generate
   ```

   (`make generate binding` is a separate target for the CGO bindings under `binding/` — only needed
   when you change `gen/binding/<runtime>/config.yaml`, not for API/webhook changes.)

   **Worktree caveat.** `make generate` only works when the checkout directory's **real** path ends in
   the module import path `gpustack.ai/gpustack` — go-to-protobuf derives its output base by trimming
   that suffix off the cwd. It therefore **fails from a git worktree** (e.g. `.claude/worktrees/<name>`)
   with `Could not make proto path relative … No such file or directory`. Run it from the **main
   checkout** instead. A symlink does not help (the Go toolchain canonicalizes it back to the real
   worktree path). To generate against uncommitted worktree changes without touching the shared main
   checkout, WIP-commit the worktree and generate from a throwaway worktree **under `$HOME`**:

   ```bash
   GEN="${HOME}/.gpustack-gen/gpustack.ai/gpustack"
   git -C <repo> worktree prune
   git -C <repo> worktree add --detach "$GEN" HEAD
   real=$( cd "$GEN" && pwd -P )
   case "$real" in */gpustack.ai/gpustack) ;; *) echo "REFUSING: $real"; exit 1 ;; esac
   ( cd "$GEN" && make generate )
   ```

   Then copy the regenerated files back into the worktree, `git worktree remove --force`, and
   `git reset` the WIP commit.

   Two reasons the path is `$HOME` and not `/tmp`, and a third why the `case` line stays:

   - On darwin `/tmp` is a symlink to `/private/tmp`, so `pwd -P` and the logical cwd disagree while
     **both** end in the module import path. The suffix check passes and the run still dies with
     `directory /private/tmp/…/gen/api outside main module or its selected dependencies`.
   - `/private/tmp` is swept. Measured 2026-09-06: a worktree there was created, verified (suffix,
     clean `git status`, expected `HEAD`) and was gone by the next command, listed as `prunable`.
     There is no signal between verifying the path and losing it.
   - A failed run is destructive, so the path has to be rejected by something that exits non-zero
     **before** the generator starts. go-to-protobuf wipes and partially rewrites every
     `generated.pb.go` / `generated.proto` before it fails on the path arithmetic — roughly 19k
     deleted lines across `api/{v1,worker/v1,worker/v1alpha1}`, silent until the next `git diff`.

   (`go build`/`go test`/`make lint` all run fine from a worktree — only `make generate` is
   path-sensitive.)

2. Review that the diff is confined to source edits + generated files:

   ```bash
   git status --short
   ```

   **Run the generator last.** `make lint` is an edit pass, not a check — `goimports-reviser
   -output=file` and `golangci-lint --fix` both rewrite the source. Generating before it produces
   artifacts for a source that no longer exists, and nothing downstream notices: the build passes,
   the tests pass, `git status` is clean. So if you lint after generating, generate again.
   `.github/workflows/api.yml` fails the PR when the committed artifacts do not match a fresh run.

3. If generation fails, the error usually points at a malformed type marker or a missing
   `+kubebuilder`/`+k8s` comment in the edited source — fix the source `*.go` and rerun.

## Hand-written mirrors the generator does NOT update

Some packages re-declare an API type's fields instead of embedding it. The generator does not touch
them, and a newly added field is not a compile error there — it is simply never carried, so the
feature silently does nothing in that layer. After adding a field, check each mirror below and wire
it in the same change:

| Mirror | Mirrors | Wire up |
| --- | --- | --- |
| `pkg/workergateway/service` — `AggregatedInstanceTypeOverviewResource`, `AggregatedInstanceTypeOnceMaxRequestCandidate` | `InstanceTypeStatus`'s resource views (`Accelerator*`, `CPU`) | the field on both types, plus `newAggregatedTier`, `newAggregatedCandidate`, both `Recompute` methods and `overviewResourceIsZero` in `helper.go` |

`TestAggregatedInstanceTypeMirrorsEveryStatusView` (in `pkg/workergateway/service`) fails while that
mirror is out of step, so `make test` catches the omission — but only the missing *field*, not a
missed aggregation site. Walk the "Wire up" column too.

Found another mirror of this shape? Add a row — the table is the reminder, and it is only as good as
its coverage.

See [development.md](../../../docs/development.md) for the full code-generation pipeline and the
API group/version/kind table.
