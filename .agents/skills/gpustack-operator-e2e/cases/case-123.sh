#!/usr/bin/env bash
#
# CASE 123 — vllm-router has no admitted encoder row, and the refusal says so before the engine is
#           even consulted (NON-MUTATING: server-side dry runs only)
#           AUTO-SKIPS the platform rows without an InstanceType reporting an admitted platform
#
#   case-123.sh <NS>
#
# Goal:        vllm-router is a real, supported router for a plain prefill/decode deployment, and it
#              takes NO encoder role at all. This case is the reading that says which column of the
#              combination is missing, and it says it EARLY: the matrix is read router-first, so a
#              vllm-router encoder deployment is refused naming the router without ever consulting
#              the engine, the platform or the version. That ordering is the useful part — a user
#              who swaps the router gets one unambiguous message instead of a list of downstream
#              consequences they have to work out themselves.
#
#              The rows also pin that the refusal is about ENCODER ROLES ONLY: a vllm-router
#              deployment with no Encode role is admitted, which is what makes the encoder refusal
#              a rule about this feature rather than a deprecation of the router.
#
#              WHY THERE IS NO FUNCTIONAL HALF HERE. A passing vllm-router encoder run would
#              require a proof of concept that has not happened: the vllm-router image running
#              --epd-config against vLLM 0.30.0 is spec task T3c, and it is BLOCKED ON A CLUSTER.
#              Inventing a functional path for a row the matrix does not admit would assert a
#              behavior the operator does not implement, and every row it printed would be false the
#              moment the row were admitted. So the functional half is absent by construction, the
#              header says so, and one row states it at the end so a reader scanning only the table
#              sees it too.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and at least one
#              Ready, schedulable worker node that is not a control-plane node. NO accelerator: the
#              encoder shape reaches admission through the platform an injected CPU vendor label
#              builds ("Ascend" — an admitted row whose floor the pinned version clears). The
#              gpustack-settings Secret must exist in the system namespace (E2E_SYSTEM_NS, default
#              gpustack-system). The case creates nothing and changes nothing outside its own
#              NodeFeature and settings key.
#
#              AUTO-SKIP: the platform-bound rows need the platform InstanceType, and the vendor
#              label may not take (NFD owns cpu-model.vendor_id and may decline an injected
#              override). Those rows then record a SKIP naming that and the case still exits 0.
#
# Inputs:      MOCKED: one node label (the CPU vendor) injected through a NodeFeature object and one
#              settings key (CPU-manufacturer awareness) flipped on — both restored by the trap.
#              REAL: nothing. Every assertion is a server-side dry run, so no object is ever stored.
#
# Expected:    - llm-d-router + vLLM + an admitted platform is ACCEPTED, so the rows below are each
#                their own rule rather than one blanket rejection;
#              - vllm-router + vLLM + an admitted platform is REFUSED naming that the router is not
#                admitted for an encoder role, and listing the admitted rows;
#              - the same refusal holds whatever the engine, because the matrix is read router-first
#                and the engine is never consulted for this router;
#              - vllm-router with NO Encode role is ACCEPTED, which is what pins the refusal to
#                encoder roles rather than to the router;
#              - the functional half records a SKIP naming the proof of concept that would admit it.
#
# Cleanup:     The trap deletes the NodeFeature object and restores the settings key to its exact
#              original value, then waits for the chain to settle back: the derived type goes only
#              once the setting cache flips and the pool re-derives. Idempotent, runs on pass AND
#              fail, safe to re-run.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-123.sh <NS>" >&2
  exit 2
fi

SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING=instance-type-aware-cpu-manufacturer
VENDOR="${E2E_C123_VENDOR:-Ascend}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"
NF=case123-cpu-vendor
MD=case123-epd
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
VERSION="${E2E_C123_VERSION:-0.30.0}"
SETTLE_ROUNDS="${E2E_C123_SETTLE:-60}"
ROUTER_NAME=llm-d-router
ENGINE=vLLM

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

command -v jq >/dev/null 2>&1 || { echo "[case-123] jq is required." >&2; exit 2; }

pick_worker_node() {
  kubectl get nodes -o json 2>/dev/null | python3 -c '
import json,sys
for n in json.load(sys.stdin).get("items",[]):
    cs=n.get("status",{}).get("conditions",[])
    if not any(c.get("type")=="Ready" and c.get("status")=="True" for c in cs):
        continue
    if n.get("spec",{}).get("unschedulable"):
        continue
    ts=n.get("spec",{}).get("taints",[])
    if any(str(t.get("key","")).startswith("node-role.kubernetes.io/") for t in ts):
        continue
    print(n["metadata"]["name"])
    break
'
}

epd_platform_type() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
      [ -n "$name" ] || continue
      [ -z "$deleting" ] || continue
      [ "$inactive" = true ] && continue
      if [ "$manufacturer" = "$PLATFORM" ]; then
        echo "$name"
        break
      fi
    done
}

ORIG_SETTING="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.${SETTING}}" 2>/dev/null)"
set_awareness() { # true|false
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":\"$(printf '%s' "$1" | base64)\"}}" >/dev/null 2>&1
}
restore_setting() {
  local v=null
  [ -n "$ORIG_SETTING" ] && v="\"${ORIG_SETTING}\""
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":${v}}}" >/dev/null 2>&1
}

# Restoring the value is not the end of the restore: the worker reads it through a thirty-second
# cache, and the type derived from it outlives the patch by that cache plus one reconcile. Wait for
# the derived type to go, so the next case in a serial run does not list an aware type this run left
# behind. A cluster that started with the setting already on keeps its derived type, so there is
# nothing to wait for.
settle_back() {
  [ "$(printf '%s' "$ORIG_SETTING" | base64 -d 2>/dev/null)" = "true" ] && return 0
  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    [ -z "$(epd_platform_type)" ] && return 0
    sleep 5
  done
  return 0
}

cleanup() {
  echo
  echo "[case-123] cleanup"
  kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$NF" --ignore-not-found >/dev/null 2>&1
  restore_setting
  settle_back
}
trap cleanup EXIT

# ---------------------------------------------------------------- the fixture

NODE="$(pick_worker_node)"
if [ -z "$NODE" ]; then
  echo "[case-123] no Ready schedulable worker node; this case cannot place its vendor label" >&2
  exit 2
fi

echo "[case-123] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"

kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${NF}
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${NODE}
spec:
  labels:
    feature.node.kubernetes.io/cpu-model.vendor_id: "${VENDOR}"
  features: {}
YAML
set_awareness true

# The poll IS the wait for the settings cache, the node flavor re-derivation and the type authoring.
# A timeout is a skip, not a failure: NFD owns cpu-model.vendor_id and may decline an injected
# override, which is an environment answer this case reports rather than argues with.
IT=""
for _ in $(seq 1 "$SETTLE_ROUNDS"); do
  IT="$(epd_platform_type)"
  [ -n "$IT" ] && break
  sleep 5
done

if [ -z "$IT" ]; then
  record SKIP "the platform rows could be read" \
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived. Every row below names that type in its manifest, so none of them ran"
else
  record PASS "a CPU-only pool reports an admitted platform" \
    "instancetype ${IT} reports manufacturer '${PLATFORM}' from the injected vendor label"
fi

# ---------------------------------------------------------------- the manifests

# The three roles and the engine and the router are the only dials this case turns; each is named on
# the row that turns it and reset right after, so a row reads on its own what it submitted instead of
# depending on the dial the previous row left behind.
manifest() { # roles-block
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: ${ENGINE:-vLLM}
    version: "${VERSION}"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: ${ROUTER_NAME:-llm-d-router}
  roles:
${1}
YAML
}

# The baseline is the ADMITTED Ascend row's shape: encode and prefill mount one claim at one path,
# the decode role mounts nothing. It carries no claim name that has to exist — the matrix is read
# from the object and the claim rules only compare the two mounts to each other.
baseline_roles() {
  cat <<ROLES
  - name: encode
    kind: Encode
    instanceType: ${IT:-unset}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case123-shared
  - name: prefill
    kind: Prefill
    instanceType: ${IT:-unset}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case123-shared
  - name: decode
    kind: Decode
    instanceType: ${IT:-unset}
    image: ${IMAGE}
ROLES
}

# The same deployment with the encoder REMOVED: an ordinary prefill/decode pair, which is the shape
# vllm-router has always served. It carries no claim and no version floor concern, so what it proves
# is narrow and exact — that the encoder refusal above is about the Encode role.
pair_roles() {
  cat <<ROLES
  - name: prefill
    kind: Prefill
    instanceType: ${IT:-unset}
    image: ${IMAGE}
  - name: decode
    kind: Decode
    instanceType: ${IT:-unset}
    image: ${IMAGE}
ROLES
}

# Assert that a roles block is refused AND that the refusal quotes every fragment after the first
# one named. An empty apply output is a FAIL on its own: deleting anything from "" leaves "". A
# refusal carrying other messages too passes — several rules aggregate into one response — but a
# MISSING fragment means the manifest tripped a rule this row is not about.
refuses() {
  local check="$1" want="$2" roles="$3" out w
  shift 3
  out="$(manifest "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
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

accepts() {
  local check="$1" roles="$2" out
  out="$(manifest "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"created (server dry run)"* | *"configured (server dry run)"*)
      record PASS "$check" "accepted by both the schema and the webhook" ;;
    *) record FAIL "$check" "refused: ${out:0:220}" ;;
  esac
  return 0
}

# ---------------------------------------------------------------- row 0: the accepted baseline

# Three roles being accepted at all is the shape this whole rule set exists to guard, so the
# baseline proves the refusals below are each their own rule rather than one blanket rejection.
if [ -n "$IT" ]; then
  accepts "the admitted combination is ACCEPTED by the same instrument" "$(baseline_roles)"
else
  record SKIP "the admitted combination is ACCEPTED by the same instrument" \
    "no platform InstanceType exists, so there is no shape to accept and every refusal below would pass for the wrong reason"
fi

# ---------------------------------------------------------------- the vllm-router rows

if [ -n "$IT" ]; then
  # The headline of this case. The matrix is read ROUTER-FIRST, so this refusal names the router
  # alone and never reaches the engine, the platform or the version: swapping the router is the one
  # change that fixes it, and the message says so instead of listing consequences of the swap.
  ROUTER_NAME=vllm-router refuses "vllm-router with an encoder role is refused naming the router" \
    'router "vllm-router" is not admitted for an encoder role' \
    "$(baseline_roles)" \
    'the admitted rows are'
  ROUTER_NAME=llm-d-router

  # The same refusal under a different engine, which is what proves the engine was never consulted.
  # If the rule read the engine first, this manifest would be refused with the ENGINE column named
  # instead, and the row would fail for the right reason on the wrong message.
  ROUTER_NAME=vllm-router ENGINE=SGLang refuses "vllm-router is refused the same way whatever the engine" \
    'router "vllm-router" is not admitted for an encoder role' \
    "$(baseline_roles)"
  ROUTER_NAME=llm-d-router

  # THE REFUSAL IS ABOUT ENCODER ROLES, NOT ABOUT THE ROUTER. vllm-router is a supported router for
  # a plain prefill/decode deployment, so the same router with no Encode role is admitted. Without
  # this row the case could not tell a rule about encoder roles from a deprecation of the router.
  # The router is named again here because the row is only about vllm-router: llm-d-router is
  # admitted in every shape, so submitting it would pass even if vllm-router had started refusing a
  # plain prefill/decode pair.
  ROUTER_NAME=vllm-router accepts "vllm-router with NO encoder role is still admitted" "$(pair_roles)"
  ROUTER_NAME=llm-d-router
else
  for label in \
    "vllm-router with an encoder role is refused naming the router" \
    "vllm-router is refused the same way whatever the engine" \
    "vllm-router with NO encoder role is still admitted"; do
    record SKIP "$label" "needs the platform InstanceType this run did not build; see the baseline row"
  done
fi

# ---------------------------------------------------------------- why there is no functional half

# Stated as a row rather than left to the header, because a case that reads as a refusal-only case
# with no functional rows is otherwise indistinguishable from one that forgot to write them. The
# admission that would admit this combination is spec task T3c and it is blocked on a cluster.
record SKIP "vllm-router serves a multimodal request through the router" \
  "there is no admitted vllm-router encoder row, so there is no object to run. The functional reading is spec task T3c (the vllm-router image running --epd-config against vLLM 0.30.0), which waits for a cluster; until its run passes and the matrix gains a row, this case asserts the refusal and nothing else"

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-123] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-123] all checks passed"