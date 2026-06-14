---
name: code-generate
description: "Run `make generate` to regenerate code after editing API types or webhooks. Invoke after changing files under api/ (api/v1, api/worker/v1, api/worker/v1alpha1) or webhook sources under pkg/*/webhooks/*/ — it regenerates deepcopy, register, apiservice, CRD, conversion, protobuf, and webhook code. Examples: \"regenerate the API\", \"I added a field to the Instance type\", \"update the generated code\"."
allowed-tools: "Bash(make generate*)"
---

# Regenerate API & webhook code

The `api/` types and the `pkg/*/webhooks/*/` webhook sources are hand-written; the deepcopy,
register, apiservice, CRD, conversion, protobuf, and webhook stubs are **generated** from them. After
editing either, regenerate so the generated code stays in sync.

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

2. Review that the diff is confined to source edits + generated files:

   ```bash
   git status --short
   ```

3. If generation fails, the error usually points at a malformed type marker or a missing
   `+kubebuilder`/`+k8s` comment in the edited source — fix the source `*.go` and rerun.

See [development.md](../../../docs/development.md) for the full code-generation pipeline and the
API group/version/kind table.
