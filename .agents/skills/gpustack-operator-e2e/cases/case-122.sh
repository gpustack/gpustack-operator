#!/usr/bin/env bash
#
# CASE 122 — SGLang has no admitted encoder row, and the refusal says which column is missing
#           (NON-MUTATING: server-side dry runs only)
#           AUTO-SKIPS the platform rows without an InstanceType reporting an admitted platform
#
#   case-122.sh <NS>
#
# Goal:        The admitted matrix has no SGLang row, and this case is the reading that says so on
#              a live API server rather than in a table a user cannot see. An encoder role under
#              llm-d-router on SGLang is refused naming the router AND the engine AND the admitted
#              rows, which is the one refusal message a user can act on. A second row covers the
#              other SGLang-family router: sglang-gateway names the ROUTER as the missing column
#              instead, because no row carries it at all. Both messages come from one rule reading
#              different facts, and a reader holding only one of them is left guessing which half to
#              change.
#
#              WHY THERE IS NO FUNCTIONAL HALF HERE. A passing SGLang encoder run would require a
#              proof of concept that has not happened: the pairing of SGLang --encoder-only and
#              --language-only behind both llm-d-router and sglang-gateway is spec task T3b, and it
#              is BLOCKED ON A CLUSTER. Inventing a functional path for a row the matrix does not
#              admit would assert a behavior the operator does not implement, and every row it
#              printed would be false the moment the row were admitted. So the functional half is
#              absent by construction, the header says so, and one row states it at the end so a
#              reader scanning only the table sees it too.
#
#              SGLANG'S OWN RENDER PATH HAS NO OBJECT-LEVEL READING, and the case says that rather
#              than substituting a weaker instrument. SGLang splits its encoder by ADDRESS
#              (--encoder-urls) rather than by a named connector, which is a render fact; the render
#              reaches that path only for a combination an admitted row covers, and none carries
#              SGLang. There is no object to submit and no Pod to read, so nothing about that path
#              is asserted here — only the refusal that keeps it unreachable.
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
#              override). Those rows then record a SKIP naming that and the case still exits 0; the
#              router-side row is refused before the platform is read and always runs.
#
# Inputs:      MOCKED: one node label (the CPU vendor) injected through a NodeFeature object and one
#              settings key (CPU-manufacturer awareness) flipped on — both restored by the trap.
#              REAL: nothing. Every assertion is a server-side dry run, so no object is ever stored.
#
# Expected:    - llm-d-router + SGLang + an admitted platform is REFUSED naming that the router is
#                not admitted with that engine, and listing the admitted rows;
#              - sglang-gateway with the same platform and engine is REFUSED naming the ROUTER as
#                the missing column instead, which is a different message from a different fact;
#              - an admitted combination (llm-d-router + vLLM) is ACCEPTED by the same instrument,
#                so the refusals above are each their own rule and not one blanket rejection;
#              - the address-transport path records a SKIP naming why it has no object to read, and
#                the functional half records a SKIP naming the proof of concept that would admit it.
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
  echo "usage: case-122.sh <NS>" >&2
  exit 2
fi

SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING=instance-type-aware-cpu-manufacturer
VENDOR="${E2E_C122_VENDOR:-Ascend}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"
NF=case122-cpu-vendor
MD=case122-epd
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
VERSION="${E2E_C122_VERSION:-0.30.0}"
SETTLE_ROUNDS="${E2E_C122_SETTLE:-60}"
ROUTER_NAME=llm-d-router
ENGINE=vLLM

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

command -v jq >/dev/null 2>&1 || { echo "[case-122] jq is required." >&2; exit 2; }

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
  echo "[case-122] cleanup"
  kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$NF" --ignore-not-found >/dev/null 2>&1
  restore_setting
  settle_back
}
trap cleanup EXIT

# ---------------------------------------------------------------- the fixture

NODE="$(pick_worker_node)"
if [ -z "$NODE" ]; then
  echo "[case-122] no Ready schedulable worker node; this case cannot place its vendor label" >&2
  exit 2
fi

echo "[case-122] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"

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
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived. The address-transport row below needs no platform and still ran"
else
  record PASS "a CPU-only pool reports an admitted platform" \
    "instancetype ${IT} reports manufacturer '${PLATFORM}' from the injected vendor label"
fi

# ---------------------------------------------------------------- the manifests

# The three roles and the engine and the router are the only dials this case turns; each is reset
# right after the row that turns it, because an assignment prefixed to a function call persists in
# bash after the call returns and would silently move every later row off the baseline.
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
          claimName: case122-shared
  - name: prefill
    kind: Prefill
    instanceType: ${IT:-unset}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case122-shared
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

# ---------------------------------------------------------------- the SGLang rows

if [ -n "$IT" ]; then
  # The headline of this case: the engine this design does not admit is refused naming BOTH the
  # router and the engine it is not admitted with, plus the admitted rows a user can compare
  # against. This is the exact message a user who wrote an SGLang encoder deployment will read.
  ENGINE=SGLang refuses "llm-d-router with SGLang is refused naming the engine" \
    'is not admitted with engine "SGLang"' \
    "$(baseline_roles)" \
    'the admitted rows are'
  ENGINE=""

  # The other SGLang-family router names a different column as missing: the router itself, because
  # no row carries it at all. Both messages are the same rule reading different facts, and a reader
  # who has only one of them is left guessing which half to change.
  ROUTER_NAME=sglang-gateway refuses "sglang-gateway with any admitted engine is refused naming the router" \
    'router "sglang-gateway" is not admitted for an encoder role' \
    "$(baseline_roles)"
  ROUTER_NAME=llm-d-router
else
  for label in \
    "llm-d-router with SGLang is refused naming the engine" \
    "sglang-gateway with any admitted engine is refused naming the router"; do
    record SKIP "$label" "needs the platform InstanceType this run did not build; see the baseline row"
  done
fi

# ---------------------------------------------------------------- the address-transport rule

# SGLANG REACHES ITS ENCODER BY ADDRESS rather than by a named connector: the renderer hands the
# language-only half --encoder-urls instead of a --ec-transfer-config document. That is a RENDER
# fact, and it has no object-level reading today: the render only reaches that path for a
# combination an admitted row covers, and no admitted row carries SGLang. There is therefore no
# submitted object whose render this case could read, and no Pod of this combination to exist.
#
# The row says so rather than grepping the renderer instead. Every other row in this suite reads an
# object on a live API server; a source grep would report the rule the code carries, which is a
# different claim from the one the rest of the case makes, and a case that quietly changed
# instrument would be the one row whose passing meant something else.
record SKIP "SGLang reaches its encoder by address rather than by a connector" \
  "the address transport is a render fact with no admitted row to render it, so no object and no Pod of this combination exists to read. What IS verified here is the refusal above, which is what keeps the path unreachable until a row is admitted"

# ---------------------------------------------------------------- why there is no functional half

# Stated as a row rather than left to the header, because a case that reads as a refusal-only case
# with no functional rows is otherwise indistinguishable from one that forgot to write them. The
# admission that would admit this combination is spec task T3b and it is blocked on a cluster.
record SKIP "SGLang serves a multimodal request through the router" \
  "there is no admitted SGLang encoder row, so there is no object to run. The functional reading is spec task T3b (SGLang --encoder-only and --language-only behind llm-d-router and behind sglang-gateway), which waits for a cluster; until its run passes and the matrix gains a row, this case asserts the refusal and nothing else"

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-122] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-122] all checks passed"