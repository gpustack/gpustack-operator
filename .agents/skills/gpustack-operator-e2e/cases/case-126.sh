#!/usr/bin/env bash
#
# CASE 126 — Two admission rules on one ModelDeployment: the engine image the schema requires,
#            and the Router the SGLang external shape requires
#            (NON-MUTATING: server-side dry runs only)
#            AUTO-SKIPS the engine-image baseline row without a usable InstanceType reporting an
#            admitted platform
#
#   case-126.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_SYSTEM_NS
# Goal:        Two contracts, both answered at admission, both about the same object.
#
#              THE ENGINE IMAGE IS A SCHEMA OBLIGATION. spec.engine.image is a REQUIRED field: the
#              CRD lists it beside engine.name in the schema's Required set, and the webhook rule
#              that used to police the engine block was deleted with the version field because the
#              schema carries the obligation now. The schema reports the obligation in two
#              wordings: "Required value" when the key is absent, and "Invalid value: \"\"" when
#              a mutating patch has materialized the key as an empty string. The patch is
#              ordinary: the webhook defaults resources.accelerator for a role that states none,
#              the typed object is re-marshaled, and engine.image has no omitempty, so the patch
#              carries "image": "" before the schema runs. Schema validation runs BEFORE admission
#              webhooks, so neither wording can be a webhook refusal wearing a schema's clothes,
#              and the field path is load-bearing in both.
#
#              THE EXTERNAL SGLANG SHAPE IS A WEBHOOK OBLIGATION. An SGLang role that declares the
#              external shape — attention width at least two, a node count of at least two, and
#              the Rust server on — is served through a Router, so a deployment that declares one
#              without a Router is REFUSED naming the rule, and the same role WITH a Router is
#              ACCEPTED. The controls matter as much as the rows: a role that declares LESS stays
#              leader-served Internal and is accepted without a Router, which proves the refusal
#              is the external-shape rule and not a blanket rejection of SGLang multi-Pod roles.
#
#              THE ACCEPTED BASELINE IS WHAT MAKES THE ENGINE-IMAGE REFUSALS MEAN SOMETHING. The
#              same manifest with engine.image present is accepted by the same instrument, so a
#              refusal below is about the missing field and not about the deployment being wrong in
#              some other way.
#
#              A ROLE IMAGE IS NOT A SUBSTITUTE. A role's own image overrides the engine-wide one
#              at render time — a different field with a different job — so the requirement sits
#              on the engine object, and a deployment whose every role names an image while its
#              engine names none is still refused. That row exists because the override is
#              exactly the shape a reader could mistake for a substitute.
#
# Environment: Any reachable API server with the ModelDeployment CRD installed (run case-1 first
#              to materialize the scheduling chain). No accelerator, no engine image, no GPU, no
#              node mutation and no settings change: every assertion is a server-side dry run, so
#              nothing is stored and no Pod is ever started. The engine-image baseline row
#              additionally needs a usable InstanceType reporting an admitted platform (ascend,
#              nvidia or amd), because the webhook reads the type a role names; its refusal rows
#              never reach a webhook and run everywhere.
#
#              AUTO-SKIP: without such an InstanceType the baseline row records SKIP and the
#              refusal rows still run.
#
# Inputs:      MOCKED: nothing. REAL: the manifests below are the only input; the image is
#              E2E_MD_IMAGE (default registry.k8s.io/pause:3.10) and is never pulled — every
#              assertion is a server-side dry run and nothing is stored. The SGLang rows name the
#              InstanceType the caller picked (E2E_MD_INSTANCE_TYPE) or a name that resolves to
#              nothing, because they assert an admission rule and not a scheduling one.
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
#                REFUSED — a role image does not satisfy the engine-level requirement;
#              - the SGLang controls — attention width one, a node count of one, and the Rust
#                server off — are each ACCEPTED without a Router, because each of them stays
#                leader-served Internal;
#              - the external SGLang shape with NO router is REFUSED naming RouterRequired and
#                the role, and the same shape WITH sglang-gateway or with llm-d-router is
#                ACCEPTED.
#
# Cleanup:     Nothing to clean. No object is created, no label injected, no setting changed;
#              every assertion is a server-side dry run. Safe to re-run.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-126.sh <NS>" >&2
  exit 2
fi

SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
MD=case126-probe
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
ENGINE=vLLM

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

# ---------------------------------------------------------------- the type the rows name

# The engine-image baseline row must pass BOTH the schema and the webhook, and the webhook reads
# the platform the InstanceType a role names reports, so the baseline needs a real type reporting a
# platform the matrix admits. The refusal rows below never reach the webhook — schema validation
# fails first — so they run with or without one. The type lookup is the library's:
# md_usable_type_for.
IT=""
MFG=""
for m in ascend nvidia amd; do
  found="$(md_usable_type_for "$m")"
  if [ -n "$found" ]; then
    IT="$found"
    MFG="$m"
    break
  fi
done

# The SGLang rows name the InstanceType the caller picked, and nothing else: they assert an
# admission rule about the role's shape, not a scheduling one, so a name that resolves to nothing
# is as good as a real type and never turns the row into a scheduling question.
SGLANG_IT="${E2E_MD_INSTANCE_TYPE:-unset}"

# ---------------------------------------------------------------- the manifests' parts

# The engine blocks. The key is DELETED, not emptied, in the no-image block: the schema answers a
# MISSING required property, and a missing property is what "Required value" names. The empty-image
# block writes out the value the defaulting patch materializes.
ENGINE_OK="    name: ${ENGINE}
    image: ${IMAGE}"
ENGINE_NO_IMAGE="    name: ${ENGINE}"
ENGINE_EMPTY="    name: ${ENGINE}
    image: \"\""
ENGINE_SGLANG="    name: SGLang
    image: ${IMAGE}"

# The decode role the engine-image rows vary. It names the engine image (or does not) and the
# accelerator count (or does not), and the pair of refusal rows below differ in exactly the thing a
# reader might think substitutes for the engine default, so their agreement is evidence.
ROLE_PLAIN="  - name: decode
    kind: Decode
    instanceType: ${IT:-case126-any}"

ROLE_WITH_IMAGE="  - name: decode
    kind: Decode
    instanceType: ${IT:-case126-any}
    image: ${IMAGE}"

# ROLE_PINNED is the no-patch shape: pinning resources.accelerator: "1" leaves the mutating
# webhook nothing to default, so it emits no patch and the manifest reaches the schema exactly as
# written. That is the shape whose refusal wording differs from the patched one.
ROLE_PINNED="  - name: decode
    kind: Decode
    instanceType: ${IT:-case126-any}
    resources:
      accelerator: \"1\""

# The Rust server switch. The external shape needs it on; the control that turns it off declares
# no env block at all, which is what "off" means here.
RUST_ENV='    env:
      - name: SGLANG_RUST_SERVER
        value: "1"'

# The SGLang role the external-shape rows vary. The external shape is three facts: the attention
# width is at least two, the node count is at least two, and the Rust server is on. Each control
# below removes exactly one of them.
sglang_role() { # attention-width node-count env-block
  cat <<ROLES
  - name: server
    kind: Server
    replicas: 1
    size: 2
    instanceType: ${SGLANG_IT}
    image: ${IMAGE}
    extraArgs:
      - --tp-size
      - "2"
      - --attn-dp-size
      - "$1"
      - --nnodes
      - "$2"
      - --node-rank
      - \$(GPUSTACK_MEMBER_INDEX)
      - --dist-init-addr
      - \$(GPUSTACK_REPLICA_LEADER_ADDRESS):29500
$3
ROLES
}

# ---------------------------------------------------------------- the manifest

# ONE builder for both surfaces. The engine block and the roles block are supplied whole by the
# caller, and the router is named only when the row under test declares one: a router block with
# an empty name would be a different object.
manifest() { # engine-block roles-block [router-name]
  local engine="$1" roles="$2" router="${3:-}"
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
${engine}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
$(if [ -n "$router" ]; then echo "  router:"; echo "    name: ${router}"; fi)
  roles:
${roles}
YAML
}

# ---------------------------------------------------------------- the instrument

# ONE dry run and ONE reading of its answer. Every row below submits a manifest to the API server
# with --dry-run=server, so the schema and the webhook both run and nothing is stored.
dry_run() { # engine-block roles-block [router-name]
  manifest "$1" "$2" "${3:-}" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' '
}

# accepts asserts that the server ACCEPTED the manifest: both the schema and the webhook took it.
# An empty apply output is a FAIL on its own: "created" must be observed, because a row that
# passes on no output at all reports an acceptance nothing produced. The instrument is the
# library's: it reads the manifest, prints the text of the row and returns the verdict.
accepts() { # check engine roles [router]
  local check="$1" engine="$2" roles="$3" router="${4:-}" obj status=0
  obj="$(md_dryrun_accepts < <(manifest "$engine" "$roles" "$router"))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
  return 0
}

# refuses asserts that the server REFUSED the manifest AND that the refusal names EVERY fragment
# the row is about. A refusal carrying somebody else's message too still passes — several rules
# aggregate into one response — but a MISSING fragment means the manifest tripped a rule this row
# is not about.
#
# AN EMPTY OUTPUT MUST NOT PASS. The refusal is read out of the apply's output, and an output that
# is empty at all means nothing was refused: the row would report a refusal it never saw.
refuses() { # check engine roles router fragment...
  local check="$1" engine="$2" roles="$3" router="$4" want="" obj status=0
  shift 4
  if [ "$#" -gt 0 ]; then
    want="$1"
    shift
  fi
  obj="$(md_dryrun_refuses "$want" "$@" < <(manifest "$engine" "$roles" "$router"))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
  return 0
}

# refuses_one_of asserts that the server REFUSED the manifest with ONE of the two wordings the
# schema can answer, and the row says which wording it answered. The two shapes the server can
# answer produce two wordings: "spec.engine.image: Required value" when the key reaches the schema
# absent (the no-patch shape), and "spec.engine.image: Invalid value: \"\"" when a defaulting patch
# has materialized it as an empty string. Either one is the same refusal, so either one passes; the
# field path is load-bearing in both, so a refusal that says "Required value" about some other
# property of this object fails even though it carries the phrase.
refuses_one_of() { # check engine roles router wording wording
  local check="$1" engine="$2" roles="$3" router="$4" first="$5" second="$6" out w
  out="$(dry_run "$engine" "$roles" "$router")"
  if [ -z "$out" ]; then
    record FAIL "$check" "the apply produced no output at all, so nothing was refused"
    return 0
  fi
  for w in "$first" "$second"; do
    case "$out" in
      *"$w"*)
        record PASS "$check" "refused with: ${w}"
        return 0
        ;;
    esac
  done
  record FAIL "$check" \
    "not refused with either '${first}' or '${second}' (got: ${out:0:200})"
  return 0
}

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

# ---------------------------------------------------------------- the engine-image surface

# Row 0: the accepted baseline. Without this row the refusals below prove nothing: a manifest
# wrong in some unrelated way would refuse too. The baseline is the SAME manifest with engine.image
# present, accepted by the same instrument. It names a real platform type when the cluster has one,
# because the webhook reads the type a role names; the refusal rows below fail schema validation
# before any webhook is called, so they run whether or not a type exists.
if [ -n "$IT" ]; then
  accepts "an engine.image baseline with a role set is ACCEPTED (${MFG})" "$ENGINE_OK" "$ROLE_WITH_IMAGE"
else
  record SKIP "an engine.image baseline with a role set is ACCEPTED" \
    "no usable InstanceType reports an admitted platform (ascend, nvidia, amd), so there is no shape the webhook would admit; the refusal rows below do not need one"
fi

# spec.engine.image absent, in its ordinary shape. The engine names its engine and nothing else,
# and the role states no accelerator, so the mutating webhook defaults it and emits a patch that
# materializes the image key as an empty string before the schema runs. Schema validation still
# runs BEFORE admission webhooks, so this refusal cannot be the deleted webhook rule wearing a
# schema's clothes; the wording this shape answers is the Invalid value one.
refuses_one_of "an engine with no image key is refused by the schema" \
  "$ENGINE_NO_IMAGE" "$ROLE_PLAIN" "" \
  "spec.engine.image: Required value" 'spec.engine.image: Invalid value: ""'

# spec.engine.image empty. RECORDED, NOT ASSERTED. An explicit "" is the value the defaulting patch
# materializes, written out in the manifest itself: once the schema carries a minimum length on
# engine.image, the server refuses it with Invalid value; on a schema without the minimum, the key
# is present and the server accepts. The row records whichever answer this build gives and carries
# it in its message. It is the one row here whose verdict is an observation, not a criterion: the
# case must read the same whether or not that minimum has landed.
out="$(dry_run "$ENGINE_EMPTY" "$ROLE_WITH_IMAGE")"
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

# A role image is not a substitute. The tempting shortcut: every role names its own image, so
# what is the engine's for? A role's image OVERRIDES the engine-wide one at render time — a
# different field with a different job — so a deployment whose every role is pinned still has no
# engine-wide default, and that is refused. This row and the headline render DIFFERENT manifests —
# the headline's role names no image at all, this row's role does. That difference is the whole
# point: the two agree on the refusal while differing in exactly the field a reader might think
# substitutes for the engine default.
refuses_one_of "an engine with no image key is refused even when every role names an image" \
  "$ENGINE_NO_IMAGE" "$ROLE_WITH_IMAGE" "" \
  "spec.engine.image: Required value" 'spec.engine.image: Invalid value: ""'

# The no-patch shape. The ordinary shape above is worded through the defaulting patch. This row
# differs from the headline by exactly one thing: the role pins resources.accelerator: "1", so the
# webhook has nothing to default and emits no patch. The image key then reaches the schema still
# absent, and the wording is the other one refuses_one_of accepts: Required value. The headline and
# this row cover both shapes the server can answer, because the two shapes produce different
# wordings.
refuses_one_of "an engine with no image key is refused even when the role pins its accelerator" \
  "$ENGINE_NO_IMAGE" "$ROLE_PINNED" "" \
  "spec.engine.image: Required value" 'spec.engine.image: Invalid value: ""'

# ---------------------------------------------------------------- the SGLang external shape

# THE CONTROLS COME FIRST. Each of them declares less than the external shape and is therefore
# leader-served Internal, so each is accepted with NO router. Without these rows every refusal
# below would pass for the wrong reason: a blanket rejection of SGLang multi-Pod roles would look
# exactly like the rule under test.
accepts "the control: attention width one is ACCEPTED without a router" \
  "$ENGINE_SGLANG" "$(sglang_role 1 2 "$RUST_ENV")" ""
accepts "the control: one node is ACCEPTED without a router" \
  "$ENGINE_SGLANG" "$(sglang_role 2 1 "$RUST_ENV")" ""
accepts "the control: the Rust server off is ACCEPTED without a router" \
  "$ENGINE_SGLANG" "$(sglang_role 2 2 "")" ""

# The refusal: the external shape with no router names the rule and the field it is about.
refuses "the external SGLang shape with no router is REFUSED naming the rule" \
  "$ENGINE_SGLANG" "$(sglang_role 2 2 "$RUST_ENV")" "" \
  "RouterRequired" 'spec.router'

# The acceptances: the same external shape served through either router.
accepts "the external SGLang shape with sglang-gateway is ACCEPTED" \
  "$ENGINE_SGLANG" "$(sglang_role 2 2 "$RUST_ENV")" "sglang-gateway"
accepts "the external SGLang shape with llm-d-router is ACCEPTED" \
  "$ENGINE_SGLANG" "$(sglang_role 2 2 "$RUST_ENV")" "llm-d-router"

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-126] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-126] all checks passed"
