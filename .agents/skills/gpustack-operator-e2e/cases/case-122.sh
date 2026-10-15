#!/usr/bin/env bash
#
# CASE 122 — SGLang's admitted encoder rows, and the pairings the matrix still refuses
#           (NON-MUTATING: server-side dry runs only)
#           AUTO-SKIPS the platform rows without an InstanceType reporting an admitted platform
#
#   case-122.sh <NS>
#
# Goal:        SGLang carries six admitted encoder rows: llm-d-router and sglang-gateway across
#              nvidia, amd and ascend, every one of them at floor 0.5.10, naming the address leg
#              (encoder-urls) and mounting no claim. This case reads that on a live API server rather
#              than in a table a user cannot see: on the platform it injects, both routers with
#              SGLang at a released version are ACCEPTED with no claim mounted, the pairing the
#              matrix still refuses is refused naming BOTH halves and the admitted rows, and a
#              version below the floor is refused naming the floor in SGLang's own numbering.
#
#              WHY THE FLOOR IS STATED AS SGLang 0.5.10 EVEN ON THE INJECTED ASCEND PLATFORM. The
#              engine is what a floor belongs to; the platform only decides which project publishes
#              the number for a vLLM row. A refusal naming vllm-ascend on an SGLang row would send a
#              user after a plugin release carrying no encoder-urls argument at all.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first). If an active
#              InstanceType already reports the target manufacturer (E2E_C122_VENDOR, default Ascend;
#              also supports nvidia or amd), it is used directly without mutating node labels or
#              settings. Otherwise, on a CPU-only cluster, the case injects the vendor label on one
#              Ready schedulable worker node and flips CPU-manufacturer awareness on in
#              gpustack-settings (E2E_SYSTEM_NS, default gpustack-system).
#
#              AUTO-SKIP: every admitted-row check below needs a usable InstanceType for the target
#              platform. If neither an existing pool nor the injected CPU vendor label yields one,
#              the rows record SKIP and the case exits 0.
#
# Inputs:      MOCKED (only when no matching InstanceType already exists): one node label (the CPU
#              vendor) injected through a NodeFeature object and one settings key (CPU-manufacturer
#              awareness) flipped on — both restored by the trap.
#              REAL: every assertion is a server-side dry run, so no ModelDeployment is stored.
#
# Expected:    - the vLLM baseline shape for the target platform is ACCEPTED;
#              - llm-d-router + SGLang at or above the floor, mounting no claim, is ACCEPTED on the
#                target platform, and sglang-gateway + SGLang is ACCEPTED the same way;
#              - sglang-gateway with vLLM is REFUSED naming both the router and the engine it is not
#                paired with, and listing the admitted rows a user can compare against;
#              - llm-d-router + SGLang one release below the floor is REFUSED naming encoder-urls
#                and the SGLang release that carries it.
#
# Cleanup:     If the fixture injected a NodeFeature object and flipped the settings key, the trap
#              deletes the NodeFeature, restores the settings key to its exact original value, and
#              waits for the derived type to settle back. Idempotent, runs on pass AND fail.
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
BASE_VERSION="${E2E_C122_VERSION:-0.30.0}"
# An assignment prefixed to a function call persists after it returns in bash, so the rows below
# restore this rather than leaving every later row reading whichever version the last one turned.
VERSION="$BASE_VERSION"
# SGLang numbers its own releases and carries its own floor, so the SGLang rows below take a separate
# default rather than reusing the vLLM one. 0.5.18 is a released SGLang tag at or above the 0.5.10
# floor, and is the release the render was written against.
SGLANG_VERSION="${E2E_C122_SGLANG_VERSION:-0.5.18}"
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
    if n.get("metadata",{}).get("labels",{}).get("feature.gpustack.ai/acceleratable") == "true":
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

INJECTED=false
cleanup() {
  echo
  echo "[case-122] cleanup"
  if [ "$INJECTED" = true ]; then
    kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$NF" --ignore-not-found >/dev/null 2>&1
    restore_setting
    settle_back
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------- the fixture

IT="$(epd_platform_type)"
if [ -n "$IT" ]; then
  record PASS "an active pool reports the target platform" \
    "instancetype ${IT} reports manufacturer '${PLATFORM}'"
else
  NODE="$(pick_worker_node)"
  if [ -z "$NODE" ]; then
    echo "[case-122] no active '${PLATFORM}' InstanceType and no Ready schedulable CPU worker node" >&2
    exit 2
  fi

  echo "[case-122] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"
  INJECTED=true

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
  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    IT="$(epd_platform_type)"
    [ -n "$IT" ] && break
    sleep 5
  done

  if [ -z "$IT" ]; then
    record SKIP "the platform rows could be read" \
      "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived"
  else
    record PASS "an active pool reports the target platform" \
      "instancetype ${IT} reports manufacturer '${PLATFORM}' from the injected vendor label"
  fi
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

# The baseline is the admitted vLLM row's shape for the target platform: on ascend, encode and
# prefill mount one claim at one path and decode mounts nothing; on nvidia and amd the leg is
# carried over the network and no role mounts a shared claim.
baseline_roles() {
  local claim=""
  if [ "$PLATFORM" = ascend ]; then
    claim="    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case122-shared"
  fi
  cat <<ROLES
  - name: encode
    kind: Encode
    instanceType: ${IT:-unset}
    image: ${IMAGE}
${claim}
  - name: prefill
    kind: Prefill
    instanceType: ${IT:-unset}
    image: ${IMAGE}
${claim}
  - name: decode
    kind: Decode
    instanceType: ${IT:-unset}
    image: ${IMAGE}
ROLES
}

# The address leg's roles are the baseline minus the claim. SGLang carries the encoder's output by
# address rather than through a mounted directory, so no role declares additionalVolumes at all —
# which is a field the address leg makes unneeded, not an omission this case forgot. Everything else
# about the shape is the baseline, so an acceptance below is about the leg and not about the roles.
address_leg_roles() {
  cat <<ROLES
  - name: encode
    kind: Encode
    instanceType: ${IT:-unset}
    image: ${IMAGE}
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

# ---------------------------------------------------------------- the SGLang rows

if [ -n "$IT" ]; then
  # The admitted SGLang rows, read through the same instrument as the vLLM baseline above. Both
  # routers are accepted at a released version with no claim mounted, because an address leg is what
  # carries the encoder's output and a connector there is not a thing a user attaches.
  ENGINE=SGLang VERSION="$SGLANG_VERSION" accepts "llm-d-router with SGLang is ACCEPTED" \
    "$(address_leg_roles)"
  ENGINE=SGLang VERSION="$SGLANG_VERSION" ROUTER_NAME=sglang-gateway \
    accepts "sglang-gateway with SGLang is ACCEPTED" \
    "$(address_leg_roles)"
  ROUTER_NAME=llm-d-router

  # The floor belongs to the engine rather than to the platform, so the refusal has to name SGLang's
  # own release here. Both fragments are asserted: the first says which connector the rule is talking
  # about and the second says which project publishes the number to go after.
  ENGINE=SGLang VERSION=0.5.9 refuses "an SGLang version below the row floor is refused" \
    'encoder-urls carries' \
    "$(address_leg_roles)" \
    'from SGLang 0.5.10'
  ENGINE=""

  # The pairing the matrix still refuses. sglang-gateway drives the encoder leg on SGLang alone, so
  # vLLM under it names both halves in one message along with the admitted rows.
  ROUTER_NAME=sglang-gateway ENGINE=vLLM refuses "sglang-gateway with vLLM is refused naming the pairing" \
    'router "sglang-gateway" is not admitted with engine "vLLM"' \
    "$(baseline_roles)" \
    'the admitted rows are'
  ROUTER_NAME=llm-d-router
  ENGINE=vLLM
  VERSION="$BASE_VERSION"
else
  for label in \
    "llm-d-router with SGLang is ACCEPTED" \
    "sglang-gateway with SGLang is ACCEPTED" \
    "an SGLang version below the row floor is refused" \
    "sglang-gateway with vLLM is refused naming the pairing"; do
    record SKIP "$label" "needs the platform InstanceType this run did not build; see the baseline row"
  done
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-122] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-122] all checks passed"