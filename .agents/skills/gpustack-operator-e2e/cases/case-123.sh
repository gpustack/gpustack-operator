#!/usr/bin/env bash
#
# CASE 123 — vllm-router admits vLLM encoder deployments on NVIDIA and AMD, refuses unadmitted
#           engines and user-supplied --epd-config
#           (MUTATING, self-recovering: the platform fixture on CPU-only clusters, restored by
#            the trap — GPU clusters need no fixture, and every verdict is a server-side dry run)
#           AUTO-SKIPS the platform rows without an InstanceType reporting nvidia or amd
#
#   case-123.sh <NS>
#
# Goal:        vllm-router drives three-stage EPD disaggregation for vLLM on NVIDIA and AMD via
#              --epd-config={"encoder_urls":[...]} while the engine roles exchange vision embeddings
#              over NIXL (ECCPUConnector) and KV cache over MooncakeConnector.
#              This case pins the admission contract for vllm-router encoder deployments:
#                - vllm-router + vLLM on nvidia or amd is ACCEPTED;
#                - vllm-router + SGLang is REFUSED naming that the router and engine do not pair;
#                - user-supplied --epd-config in spec.router.extraArgs is REFUSED as operator-owned;
#                - vllm-router with NO Encode role (plain prefill/decode) is ACCEPTED.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first). When the cluster
#              already carries an active nvidia or amd InstanceType, the case reads that type
#              directly and mutates nothing. On a CPU-only cluster it injects a CPU vendor label
#              ("NVIDIA") on one non-acceleratable worker node and flips CPU-manufacturer awareness
#              on, restoring both on exit.
#
#              AUTO-SKIP: if no nvidia/amd InstanceType exists and NFD declines the injected CPU
#              vendor label, the platform-bound rows record SKIP and the case exits 0.
#
# Inputs:      Every ModelDeployment assertion is a server-side dry run (`kubectl apply --dry-run=server`),
#              so no ModelDeployment is persisted.
#
# Expected:    - vllm-router + vLLM 0.30.0 on nvidia/amd with Encode/Prefill/Decode is ACCEPTED;
#              - vllm-router + SGLang with an Encode role is REFUSED naming the router/engine pair;
#              - vllm-router passing --epd-config in spec.router.extraArgs is REFUSED as owned;
#              - vllm-router with NO Encode role (plain prefill/decode) is ACCEPTED.
#
# Cleanup:     If the CPU-vendor fixture was used, the trap deletes the NodeFeature object, restores
#              the settings key, and waits for the derived InstanceType to settle back.
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
VENDOR="${E2E_C123_VENDOR:-NVIDIA}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"
NF=case123-cpu-vendor
MD=case123-epd
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
SETTLE_ROUNDS="${E2E_C123_SETTLE:-60}"
ROUTER_NAME=vllm-router
ENGINE=vLLM
ROUTER_EXTRA_ARGS=""

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

command -v jq >/dev/null 2>&1 || { echo "[case-123] jq is required." >&2; exit 2; }

usable_type_for() { # <manufacturer>
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
      [ -n "$name" ] || continue
      [ -z "$deleting" ] || continue
      [ "$inactive" = true ] && continue
      if [ "$manufacturer" = "$1" ]; then
        echo "$name"
        break
      fi
    done
}

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
  usable_type_for "$PLATFORM"
}

ORIG_SETTING="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.${SETTING}}" 2>/dev/null)"
INJECTED_FIXTURE=false

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

settle_back() {
  [ "$(printf '%s' "$ORIG_SETTING" | base64 -d 2>/dev/null)" = "true" ] && return 0
  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    [ -z "$(epd_platform_type)" ] && return 0
    sleep 5
  done
  return 0
}

cleanup() {
  [ "$INJECTED_FIXTURE" = true ] || return 0
  echo
  echo "[case-123] cleanup"
  kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$NF" --ignore-not-found >/dev/null 2>&1
  restore_setting
  settle_back
}
trap cleanup EXIT

# ---------------------------------------------------------------- the platform InstanceType

IT=""
for m in nvidia amd; do
  IT="$(usable_type_for "$m")"
  if [ -n "$IT" ]; then
    PLATFORM="$m"
    break
  fi
done

if [ -n "$IT" ]; then
  record PASS "an admitted platform InstanceType is available" \
    "instancetype ${IT} reports manufacturer '${PLATFORM}'"
else
  NODE="$(pick_worker_node)"
  if [ -n "$NODE" ]; then
    INJECTED_FIXTURE=true
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
    for _ in $(seq 1 "$SETTLE_ROUNDS"); do
      IT="$(epd_platform_type)"
      [ -n "$IT" ] && break
      sleep 5
    done
  fi

  if [ -z "$IT" ]; then
    record SKIP "the platform rows could be read" \
      "no InstanceType reported manufacturer 'nvidia' or 'amd' and no CPU-vendor fixture could be derived; every row below names that type in its manifest, so none of them ran"
  else
    record PASS "a CPU-only pool reports an admitted platform" \
      "instancetype ${IT} reports manufacturer '${PLATFORM}' from the injected vendor label"
  fi
fi

# ---------------------------------------------------------------- the manifests

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
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: ${ROUTER_NAME:-vllm-router}
${ROUTER_EXTRA_ARGS:-}
  roles:
${1}
YAML
}

# The network-leg baseline on nvidia/amd: encode, prefill and decode carry no shared-claim mount.
baseline_roles() {
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

# The same deployment with the encoder REMOVED: an ordinary prefill/decode pair.
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

# ---------------------------------------------------------------- the vllm-router rows

if [ -n "$IT" ]; then
  # Row 0: the admitted vllm-router + vLLM EPD shape on nvidia/amd is accepted.
  ROUTER_NAME=vllm-router accepts \
    "vllm-router + vLLM with an encoder role is ACCEPTED on ${PLATFORM}" \
    "$(baseline_roles)"

  # Row 2: vllm-router paired with SGLang is refused naming the router and engine.
  ROUTER_NAME=vllm-router ENGINE=SGLang refuses \
    "vllm-router paired with SGLang is refused" \
    'router "vllm-router" is not admitted with engine "SGLang"' \
    "$(baseline_roles)" \
    'the admitted rows are'
  ENGINE=vLLM

  # Row 3: --epd-config is owned by the operator for vllm-router and refused in extraArgs.
  ROUTER_EXTRA_ARGS="$(printf '    extraArgs:\n      - --epd-config={"encoder_urls":[]}\n')" \
    ROUTER_NAME=vllm-router refuses \
    "user-supplied --epd-config on vllm-router is refused as operator-owned" \
    '"--epd-config" is set by the operator for router "vllm-router"' \
    "$(baseline_roles)"
  ROUTER_EXTRA_ARGS=""

  # Row 4: vllm-router with no Encode role (plain prefill/decode) remains accepted.
  ROUTER_NAME=vllm-router accepts \
    "vllm-router with NO encoder role is admitted" \
    "$(pair_roles)"
else
  for label in \
    "vllm-router + vLLM with an encoder role is ACCEPTED" \
    "vllm-router paired with SGLang is refused" \
    "user-supplied --epd-config on vllm-router is refused as operator-owned" \
    "vllm-router with NO encoder role is admitted"; do
    record SKIP "$label" "needs an nvidia or amd InstanceType this run did not have; see the first row"
  done
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-123] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-123] all checks passed"