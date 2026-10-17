#!/usr/bin/env bash
#
# CASE 126 — a ModelDeployment without spec.engine.image is refused by the API server's schema,
#           before any webhook runs (NON-MUTATING: server-side dry runs only)
#           AUTO-SKIPS the accepted-baseline row without a usable InstanceType reporting an
#           admitted platform
#
#   case-126.sh <NS>
#
# Goal:        spec.engine.image is a REQUIRED field: the CRD lists it beside engine.name in the
#              schema's Required set, and the webhook rule that used to police the engine block
#              was deleted with the version field because the schema carries the obligation now.
#              The schema reports the obligation in two wordings: "Required value" when the key
#              is absent, and "Invalid value: \"\"" when a mutating patch has materialized the
#              key as an empty string. The patch is ordinary: the webhook defaults
#              resources.accelerator for a role that states none, the typed object is
#              re-marshaled, and engine.image has no omitempty, so the patch carries
#              "image": "" before the schema runs. Schema validation runs BEFORE admission
#              webhooks, so neither wording can be a webhook refusal wearing a schema's clothes,
#              and the field path is load-bearing in both.
#
#              THE ACCEPTED BASELINE IS WHAT MAKES THE REFUSALS MEAN SOMETHING. The same manifest
#              with engine.image present is accepted by the same instrument, so a refusal below is
#              about the missing field and not about the deployment being wrong in some other way.
#
#              A ROLE IMAGE IS NOT A SUBSTITUTE. A role's own image overrides the engine-wide one
#              at render time — a different field with a different job — so the requirement sits
#              on the engine object, and a deployment whose every role names an image while its
#              engine names none is still refused. That row exists because the override is
#              exactly the shape a reader could mistake for a substitute.
#
# Environment: Any reachable API server with the ModelDeployment CRD installed (run case-1 first
#              to materialize the scheduling chain). No accelerator, no node mutation, no
#              settings change. The accepted-baseline row additionally needs a usable
#              InstanceType reporting an admitted platform (ascend, nvidia or amd), because the
#              webhook reads the type a role names; the refusal rows never reach a webhook and
#              run everywhere.
#
#              AUTO-SKIP: without such an InstanceType the baseline row records SKIP and the
#              refusal rows still run.
#
# Inputs:      MOCKED: nothing. REAL: the manifests below are the only input; the image is
#              E2E_MD_IMAGE (default registry.k8s.io/pause:3.10) and is never pulled — every
#              assertion is a server-side dry run and nothing is stored.
#
# Expected:    - the deployed operator build is recorded, never asserted: the image tag is the
#                only handle this case has on which CRD the cluster installed;
#              - the accepted baseline: engine {name, image} with a role set is accepted
#                (created (server dry run)), so the refusals below are each their own rule;
#              - the same manifest with the image: key deleted from spec.engine is REFUSED, and
#                its ordinary shape (the role states no accelerator) is worded through the
#                defaulting patch: "spec.engine.image: Invalid value: \"\"";
#              - the same manifest with the role pinning resources.accelerator: "1" emits no
#                patch, the key stays absent, and the wording is
#                "spec.engine.image: Required value";
#              - engine.image present but EMPTY is a recorded row: an explicit "" is the value
#                the patch materializes, the schema's minimum length refuses it once that lands,
#                and the row carries the observed answer either way;
#              - an engine naming only its name, with every role carrying its own image, is STILL
#                REFUSED — a role image does not satisfy the engine-level requirement.
#
# Cleanup:     Nothing to clean. No object is created, no label injected, no setting changed;
#              every assertion is a server-side dry run. Safe to re-run.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-126.sh <NS>" >&2
  exit 2
fi

SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
MD=case126-epd
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
ENGINE=vLLM

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"


# ---------------------------------------------------------------- the type the baseline names

# The baseline row must pass BOTH the schema and the webhook, and the webhook reads the platform
# the InstanceType a role names reports, so the baseline needs a real type reporting a platform
# the matrix admits. The refusal rows below never reach the webhook — schema validation fails
# first — so they run with or without one.
usable_type_for() { # <manufacturer>
  kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
      [ -n "$name" ] || continue
      [ -z "$deleting" ] || continue
      [ "$inactive" = true ] && continue
      [ -n "$manufacturer" ] || continue
      if [ "$manufacturer" = "$1" ]; then
        echo "$name"
        break
      fi
    done
}

IT=""
MFG=""
for m in ascend nvidia amd; do
  found="$(usable_type_for "$m")"
  if [ -n "$found" ]; then
    IT="$found"
    MFG="$m"
    break
  fi
done

# ---------------------------------------------------------------- the precondition

# THE INSTRUMENT IS A DEPLOYED BUILD, and this row is what says which one. A CRD is installed by
# the release under test, so a cluster still carrying the previous schema would answer every row
# below about the previous schema: the accepted baseline would be refused for naming a field the
# old CRD does not have, and every refusal would quote "unknown field" instead of the missing
# property. The image tag is the only handle this case has on that, so it is read, recorded, and
# never asserted — pinning a tag here would break the moment the image is rebuilt.
WORKER_DEPLOY="$(kubectl -n "$SYSTEM_NS" get deploy gpustack-operator-worker -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
if [ -n "$WORKER_DEPLOY" ]; then
  record PASS "the deployed operator build is recorded" "worker image ${WORKER_DEPLOY}"
else
  record SKIP "the deployed operator build is recorded" \
    "no gpustack-operator-worker Deployment in ${SYSTEM_NS}, so the rows below depend on whichever CRD this cluster installed without this case saying which"
fi

# ---------------------------------------------------------------- the manifests

# ROLE_IMAGE is the second dial, and the pair of refusal rows below is the reason it exists. With
# the role image present in one row and absent in the other, the two rows differ in exactly the
# thing a reader might think substitutes for the engine default, so their agreement is evidence.
# Two rows rendering the SAME manifest would agree for the trivial reason and prove nothing.
WITH_IMAGE="    image: ${IMAGE}"
NO_IMAGE=""
ROLE_IMAGE="$WITH_IMAGE"
# ROLE_RESOURCES is the third dial. Pinning resources.accelerator: "1" in the role leaves the
# mutating webhook nothing to default, so it emits no patch and the manifest reaches the schema
# exactly as written. That is the no-patch shape, and its refusal wording differs from the
# patched one. ACCELERATOR_PIN is its non-empty value; empty is the ordinary shape, whose webhook
# defaults the accelerator and emits its patch.
ROLE_RESOURCES=""
ACCELERATOR_PIN='    resources:
      accelerator: "1"'

manifest() { # engine-block
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
$1
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
  - name: decode
    kind: Decode
    instanceType: ${IT:-case126-any}
${ROLE_IMAGE}
${ROLE_RESOURCES}
YAML
}

ENGINE_OK="    name: ${ENGINE:-vLLM}
    image: ${IMAGE}"
# The key is deleted, not emptied: the schema answers a MISSING required property, and a missing
# property is what "Required value" names.
ENGINE_NO_IMAGE="    name: ${ENGINE:-vLLM}"

# Assert that a manifest is refused with the schema's own wording, naming the field path. An
# empty apply output is a FAIL on its own: deleting anything from "" leaves "".
#
# THE WORDING IS MEASURED, NOT GUESSED, and the two shapes the server can answer produce two
# wordings: "spec.engine.image: Required value" when the key reaches the schema absent (the
# no-patch shape), and "spec.engine.image: Invalid value: \"\"" when a defaulting patch has
# materialized it as an empty string. Either passes. The field path is load-bearing in both, so a
# refusal that says "Required value" about some other property of this object fails even though
# it carries the phrase — this case would otherwise claim a requirement it never read.
refuses() {
  local check="$1" engine_block="$2" role_block="$3" resources_block="${4:-}" out
  out="$(ROLE_IMAGE="$role_block" ROLE_RESOURCES="$resources_block" manifest "$engine_block" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  if [ -z "$out" ]; then
    record FAIL "$check" "the apply produced no output at all, so nothing was refused"
    return 0
  fi
  case "$out" in
    *"spec.engine.image: Required value"*)
      record PASS "$check" "refused with: spec.engine.image: Required value"
      ;;
    *'spec.engine.image: Invalid value: ""'*)
      record PASS "$check" 'refused with: spec.engine.image: Invalid value: ""'
      ;;
    *)
      record FAIL "$check" \
        "not refused with either 'spec.engine.image: Required value' or 'spec.engine.image: Invalid value: \"\"' (got: ${out:0:200})"
      ;;
  esac
  return 0
}

accepts() {
  local check="$1" engine_block="$2" role_block="$3" out
  out="$(ROLE_IMAGE="$role_block" manifest "$engine_block" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"created (server dry run)"* | *"configured (server dry run)"*)
      record PASS "$check" "accepted by both the schema and the webhook" ;;
    *) record FAIL "$check" "refused: ${out:0:220}" ;;
  esac
  return 0
}

# ---------------------------------------------------------------- row 0: the accepted baseline

# Without this row the refusals below prove nothing: a manifest wrong in some unrelated way would
# refuse too. The baseline is the SAME manifest with engine.image present, accepted by the same
# instrument. It names a real platform type when the cluster has one, because the webhook reads the
# type a role names; the refusal rows below fail schema validation before any webhook is called, so
# they run whether or not a type exists.
if [ -n "$IT" ]; then
  accepts "an engine.image baseline with a role set is ACCEPTED (${MFG})" "$ENGINE_OK" "$WITH_IMAGE"
else
  record SKIP "an engine.image baseline with a role set is ACCEPTED" \
    "no usable InstanceType reports an admitted platform (ascend, nvidia, amd), so there is no shape the webhook would admit; the refusal rows below do not need one"
fi

# ---------------------------------------------------------------- spec.engine.image absent

# THE HEADLINE OF THIS CASE, IN ITS ORDINARY SHAPE. The engine names its engine and nothing else,
# and the role states no accelerator, so the mutating webhook defaults it and emits a patch that
# materializes the image key as an empty string before the schema runs. Schema validation still
# runs BEFORE admission webhooks, so this refusal cannot be the deleted webhook rule wearing a
# schema's clothes; the wording this shape answers is the Invalid value one.
refuses "an engine with no image key is refused by the schema" "$ENGINE_NO_IMAGE" "$NO_IMAGE"

# ---------------------------------------------------------------- spec.engine.image empty

# RECORDED, NOT ASSERTED. An explicit "" is the value the defaulting patch materializes, written
# out in the manifest itself: once the schema carries a minimum length on engine.image, the server
# refuses it with Invalid value; on a schema without the minimum, the key is present and the
# server accepts. The row records whichever answer this build gives and carries it in its message.
# It is the one row here whose verdict is an observation, not a criterion: the case must read the
# same whether or not that minimum has landed.
out="$(ROLE_IMAGE="$WITH_IMAGE" manifest '    name: vLLM
    image: ""' | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
if [ -z "$out" ]; then
  record SKIP "an empty engine.image is recorded" \
    "the apply produced no output at all, so there was no outcome to record"
else
  case "$out" in
    *"created (server dry run)"* | *"configured (server dry run)"*)
      record PASS "an empty engine.image is recorded" \
        "RECORDED: the server ACCEPTED an empty engine.image (no minimum length in this schema): ${out:0:160}" ;;
    *)
      record PASS "an empty engine.image is recorded" \
        "RECORDED: the server refused an empty engine.image: ${out:0:160}" ;;
  esac
fi

# ------------------------------------------------- a role image is not a substitute

# The tempting shortcut: every role names its own image, so what is the engine's for? A role's
# image OVERRIDES the engine-wide one at render time — a different field with a different job — so
# a deployment whose every role is pinned still has no engine-wide default, and that is refused.
# This row and the headline render DIFFERENT manifests — the headline's role names no image at all,
# this row's role does. That difference is the whole point: the two agree on the refusal while
# differing in exactly the field a reader might think substitutes for the engine default.
refuses "an engine with no image key is refused even when every role names an image" "$ENGINE_NO_IMAGE" "$WITH_IMAGE"

# ------------------------------------------------- the no-patch shape

# The ordinary shape above is worded through the defaulting patch. This row differs from the
# headline by exactly one thing: the role pins resources.accelerator: "1", so the webhook has
# nothing to default and emits no patch. The image key then reaches the schema still absent, and
# the wording is the other one refuses() accepts: Required value. The headline and this row cover
# both shapes the server can answer, because the two shapes produce different wordings.
refuses "an engine with no image key is refused even when the role pins its accelerator" "$ENGINE_NO_IMAGE" "$NO_IMAGE" "$ACCELERATOR_PIN"

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-126] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-126] all checks passed"
