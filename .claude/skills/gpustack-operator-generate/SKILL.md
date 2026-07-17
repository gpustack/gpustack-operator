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
   checkout: WIP-commit the worktree, `git worktree add --detach /tmp/<x>/gpustack.ai/gpustack HEAD`,
   run `make generate` there, copy the regenerated files back into the worktree, `git worktree remove
   --force`, then `git reset` the WIP commit. (`go build`/`go test`/`make lint` all run fine from a
   worktree — only `make generate` is path-sensitive.)

2. Review that the diff is confined to source edits + generated files:

   ```bash
   git status --short
   ```

3. If generation fails, the error usually points at a malformed type marker or a missing
   `+kubebuilder`/`+k8s` comment in the edited source — fix the source `*.go` and rerun.

See [development.md](../../../docs/development.md) for the full code-generation pipeline and the
API group/version/kind table.
