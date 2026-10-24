#!/usr/bin/env bash
#
# CASE 130 — SGLang External DP at admission: the Router is required   (NON-MUTATING: server-side dry runs only)
#
#   case-130.sh <NS>
#
# Goal:        Read the SGLang External DP admission rule on a live API server rather than in a
#              table a user cannot see. An SGLang role that declares the external shape — attention
#              width at least two, a node count of at least two, and the Rust server on — is served
#              through a Router, so a deployment that declares one without a Router is REFUSED
#              naming the rule, and the same role WITH a Router is ACCEPTED. The controls matter
#              as much as the rows: a role that declares LESS stays leader-served Internal and is
#              accepted without a Router, which proves the refusal is the external-shape rule and
#              not a blanket rejection of SGLang multi-Pod roles.
# Environment: Any cluster with a materialized scheduling chain (run case-1 first). No accelerator,
#              no engine image and no GPU are needed: every assertion is a server-side dry run, so
#              nothing is stored and no Pod is ever started.
# Inputs:      REAL: the assertions are server-side dry runs of the ModelDeployment the case builds.
#              MOCKED: nothing.
# Expected:    - the external SGLang shape with NO router is REFUSED naming RouterRequired and the
#                role;
#              - the same shape WITH sglang-gateway is ACCEPTED, and with llm-d-router too;
#              - the CONTROL rows — attention width one, and a node count of one, and the Rust
#                server off — are each ACCEPTED without a Router, because each of them stays
#                leader-served Internal.
# Cleanup:     Nothing to clean up: no object is stored, no node is touched and no setting is
#              changed. The case exits 0 when every check passed.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-130.sh <NS>" >&2
  exit 2
fi

MD=case130-sedp
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
IT="${E2E_MD_INSTANCE_TYPE:-unset}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

command -v jq >/dev/null 2>&1 || { echo "[case-130] jq is required." >&2; exit 2; }

# ---------------------------------------------------------------- the manifests

# manifest builds the deployment under test. The router block is omitted when the router name is
# empty, because the row under test is the one where there IS no router, and a router block with
# an empty name would be a different object.
manifest() { # router-name roles-block
  local router="$1"
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: SGLang
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
$(if [ -n "$router" ]; then echo "  router:"; echo "    name: ${router}"; fi)
  roles:
${2}
YAML
}

# The external SGLang shape: one attention-DP group per member, two members, the Rust server on.
# These are the four facts the classification reads, and each of the controls below removes exactly
# one of them.
external_roles() {
  cat <<ROLES
  - name: server
    kind: Server
    replicas: 1
    size: 2
    instanceType: ${IT}
    image: ${IMAGE}
    extraArgs:
      - --tp-size
      - "2"
      - --attn-dp-size
      - "2"
      - --nnodes
      - "2"
      - --node-rank
      - \$(GPUSTACK_MEMBER_INDEX)
      - --dist-init-addr
      - \$(GPUSTACK_REPLICA_LEADER_ADDRESS):29500
    env:
      - name: SGLANG_RUST_SERVER
        value: "1"
ROLES
}

# The same role with the attention width at one: one attention group, so the launch is not the
# external shape and stays leader-served Internal.
attn_width_one_roles() {
  cat <<ROLES
  - name: server
    kind: Server
    replicas: 1
    size: 2
    instanceType: ${IT}
    image: ${IMAGE}
    extraArgs:
      - --tp-size
      - "2"
      - --attn-dp-size
      - "1"
      - --nnodes
      - "2"
      - --node-rank
      - \$(GPUSTACK_MEMBER_INDEX)
      - --dist-init-addr
      - \$(GPUSTACK_REPLICA_LEADER_ADDRESS):29500
    env:
      - name: SGLANG_RUST_SERVER
        value: "1"
ROLES
}

# The same role with one node: the attention groups are not on different members, so the launch is
# not the external shape.
one_node_roles() {
  cat <<ROLES
  - name: server
    kind: Server
    replicas: 1
    size: 2
    instanceType: ${IT}
    image: ${IMAGE}
    extraArgs:
      - --tp-size
      - "2"
      - --attn-dp-size
      - "2"
      - --nnodes
      - "1"
      - --node-rank
      - \$(GPUSTACK_MEMBER_INDEX)
      - --dist-init-addr
      - \$(GPUSTACK_REPLICA_LEADER_ADDRESS):29500
    env:
      - name: SGLANG_RUST_SERVER
        value: "1"
ROLES
}

# The same role with the Rust server off: the groups share one frontend, so the group is balanced
# inside itself and the launch is internal data parallelism.
no_rust_server_roles() {
  cat <<ROLES
  - name: server
    kind: Server
    replicas: 1
    size: 2
    instanceType: ${IT}
    image: ${IMAGE}
    extraArgs:
      - --tp-size
      - "2"
      - --attn-dp-size
      - "2"
      - --nnodes
      - "2"
      - --node-rank
      - \$(GPUSTACK_MEMBER_INDEX)
      - --dist-init-addr
      - \$(GPUSTACK_REPLICA_LEADER_ADDRESS):29500
ROLES
}

# ---------------------------------------------------------------- the two instruments

# refuses asserts that the apply was REFUSED and that the message carries every fragment the
# check is about. A refusal carrying other messages too passes — several rules aggregate into one
# response — but a MISSING fragment means the manifest tripped a rule this row is not about.
refuses() {
  local check="$1" want="$2" router="$3" roles="$4" out w
  shift 4
  out="$(manifest "$router" "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  if [ -z "$out" ]; then
    record FAIL "$check" "the apply produced no output at all, so nothing was refused"
    return 0
  fi
  for w in "$want" "$@"; do
    case "$out" in
      *"$w"*) ;;
      *) record FAIL "$check" "refused with the wrong message, or accepted (missing '${w}'): ${out:0:220}"; return 0 ;;
    esac
  done
  record PASS "$check" "refused naming: ${want}${*:+ and $*}"
  return 0
}

# accepts asserts that the apply was ACCEPTED by both the schema and the webhook.
accepts() {
  local check="$1" router="$2" roles="$3" out
  out="$(manifest "$router" "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"created (server dry run)"* | *"configured (server dry run)"*)
      record PASS "$check" "accepted by both the schema and the webhook" ;;
    *) record FAIL "$check" "refused: ${out:0:220}" ;;
  esac
  return 0
}

# ---------------------------------------------------------------- row 0: the controls

# The controls come FIRST. Each of them declares less than the external shape and is therefore
# leader-served Internal, so each is accepted with NO router. Without these rows every refusal
# below would pass for the wrong reason: a blanket rejection of SGLang multi-Pod roles would look
# exactly like the rule under test.
accepts "the control: attention width one is ACCEPTED without a router" "" "$(attn_width_one_roles)"
accepts "the control: one node is ACCEPTED without a router" "" "$(one_node_roles)"
accepts "the control: the Rust server off is ACCEPTED without a router" "" "$(no_rust_server_roles)"

# ---------------------------------------------------------------- row 1: the refusal

refuses "the external SGLang shape with no router is REFUSED naming the rule" \
  "RouterRequired" "" "$(external_roles)" \
  'spec.router'

# ---------------------------------------------------------------- row 2: the acceptance

accepts "the external SGLang shape with sglang-gateway is ACCEPTED" "sglang-gateway" "$(external_roles)"
accepts "the external SGLang shape with llm-d-router is ACCEPTED" "llm-d-router" "$(external_roles)"

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-130] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-130] all checks passed"
