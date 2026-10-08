#!/usr/bin/env bash
#
# CASE 19 — CPU-manufacturer awareness enriches the accelerated InstanceType; a real GPU Instance runs on it   (MUTATING, self-recovering; AUTO-SKIPS without a real accelerator)
#
#   case-19.sh <NS>
#
# Goal:        With instance-type-aware-cpu-manufacturer ON, the derived accelerated pool splits by CPU
#              (gpustack--${gKey}--${aKey}-${os}-${arch}) and its InstanceType status.detail carries BOTH
#              the correct GPU descriptors (product/memory/cores, from the real card) AND the folded CPU
#              detail (status.detail.cpu). Then a real GPU Instance deploys onto that aware type and its Pod runs
#              with the card visible — the full aware→derive→enrich→admit→schedule chain.
# Environment: Needs REAL accelerator hardware (a real card the Instance actually schedules onto).
#              AUTO-SKIPS (exit 0, prints why) on a GPU-less cluster. Flips a cluster-wide editable
#              setting and restarts the worker, then restores — run when the cluster is otherwise idle.
# Inputs:      All real, nothing mocked —
#              - patches the gpustack-settings Secret awareness key to "true" and restarts the worker to
#                force the aware re-derive;
#              - an Instance gpustack-e2e-case19 (accelerator=1, ubuntu sleep) on the aware accelerated type;
#              - on NVIDIA pools with two free cards, a second Instance requests accelerator=2.
# Expected:    - the aware type gpustack--${gKey}--${aKey}-${os}-${arch} materializes Active with
#                acceleratorGroup=${aKey}, generalGroup=${gKey}, GPU product/memory/cores == the flavor's,
#                and a non-empty status.detail.cpu (the awareness-gated CPU fold ran);
#              - the Instance reaches Ready and its Pod runs with nvidia-smi seeing the card;
#              - the two-card Instance sees exactly its two distinct allocated GPU UUIDs;
#              - each assigned card is exclusive with no remaining capacity in Devices.Status.
# Cleanup:     Trap deletes the Instance, restores the setting to its original value, restarts the worker,
#              verifies pre-existing unitResources/localStorage are unchanged, checks card claims were released,
#              and removes only derived types the aware window created (snapshot diff).
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-19.sh <NS>}"
AWARE_KEY=instance-type-aware-cpu-manufacturer
DERIVED_LABEL=schedule.gpustack.ai/derived-from-node
INST=gpustack-e2e-case19
MULTI_INST=${INST}-two
CREATED=()

set_aware() { local b64; b64=$(printf '%s' "$1" | base64 | tr -d '\n')
  kubectl -n "$NS" patch secret gpustack-settings --type=merge -p "{\"data\":{\"${AWARE_KEY}\":\"${b64}\"}}" >/dev/null 2>&1; }
derived_its() { kubectl get instancetypes.worker.gpustack.ai -l "${DERIVED_LABEL}=true" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null; }
note() { kubectl get resourceflavor "$1" -o jsonpath="{.metadata.annotations.note\.gpustack\.ai/$2}" 2>/dev/null; }
bounce_worker() { kubectl -n "$NS" rollout restart deploy/gpustack-operator-worker >/dev/null 2>&1;
  kubectl -n "$NS" rollout status deploy/gpustack-operator-worker --timeout=180s >/dev/null 2>&1; }

# --- Skip gate: a real accelerated ResourceFlavor, identified by its controller-owned annotation. ---
ARF=$(kubectl get resourceflavors.kueue.x-k8s.io -o json 2>/dev/null | jq -r '
  [.items[]
   | select(.metadata.annotations["note.gpustack.ai/acceleratable"] == "true")
   | .metadata.name] | sort | .[0] // ""')
if [ -z "$ARF" ]; then
  echo "== CASE 19 — SKIPPED =="
  echo "No ResourceFlavor marked acceleratable — this case needs real accelerator"
  echo "hardware. Run it on a GPU cluster to exercise the aware accelerated derive + real GPU deploy."
  exit 0
fi
GKEY=$(note "$ARF" generalGroup); AKEY=$(note "$ARF" acceleratorGroup)
PRODUCT=$(note "$ARF" product); MEMORY=$(note "$ARF" memory); CORES=$(note "$ARF" cores)
OS=$(kubectl get resourceflavor "$ARF" -o jsonpath='{.metadata.labels.kubernetes\.io/os}' 2>/dev/null)
ARCH=$(kubectl get resourceflavor "$ARF" -o jsonpath='{.metadata.labels.kubernetes\.io/arch}' 2>/dev/null)
[ -n "$GKEY" ] && [ -n "$AKEY" ] && [ -n "$OS" ] && [ -n "$ARCH" ] || { echo "[case-19] flavor ${ARF} missing group/os/arch notes"; exit 1; }
AWARE_IT="gpustack--${GKEY}--${AKEY}-${OS}-${ARCH}"
echo "[case-19] accelerated flavor ${ARF} → aware type ${AWARE_IT} (product=${PRODUCT} memory=${MEMORY} cores=${CORES})"

# Capture the exact setting value, including an absent key, before any mutation.
orig=$(kubectl -n "$NS" get secret gpustack-settings -o json | jq -c --arg key "$AWARE_KEY" '.data[$key]') || exit 1
before_json=$(kubectl get instancetypes.worker.gpustack.ai -o json) || exit 1
before_type=$(printf '%s' "$before_json" | jq -c --arg name "$AWARE_IT" '.items[] | select(.metadata.name == $name)')
for name in "$INST" "$MULTI_INST"; do
  existing=$(kubectl -n default get instance "$name" --ignore-not-found -o name) || exit 1
  [ -z "$existing" ] || { echo "[case-19] refusing pre-existing ${existing}"; exit 1; }
done
before_devices=$(kubectl get devices -o json) || exit 1
ledger() { jq -cS '[.items[] | .metadata.name as $node | .status.groups[]? | .id as $group |
  .accelerators[]? | {node:$node, group:$group, id, allocated, remaining, allocatedSlices}] | sort_by(.node,.group,.id)'; }
before_ledger=$(printf '%s' "$before_devices" | ledger)
[ "$before_ledger" != '[]' ] || { echo "[case-19] no per-card baseline"; exit 1; }

cleanup() {
  local failed=0 current it settled="" after=""
  echo "[case-19] cleanup: release claims and restore the exact setting and type fields"
  for it in "${CREATED[@]}"; do
    kubectl -n default delete instance "$it" --ignore-not-found --timeout=60s >/dev/null || failed=1
    kubectl -n default wait --for=delete pod/"$it" --timeout=60s >/dev/null || failed=1
  done
  for _ in $(seq 1 20); do
    if current=$(kubectl get devices -o json); then
      after=$(printf '%s' "$current" | ledger)
      [ "$after" = "$before_ledger" ] && { settled=1; break; }
    fi
    sleep 3
  done
  [ -n "$settled" ] || failed=1
  echo "[case-19] per-card baseline=${before_ledger} after=${after}"
  if [ -n "$before_type" ]; then
    current=$(kubectl get instancetype "$AWARE_IT" -o json) || failed=1
    [ "$(printf '%s' "$current" | jq -cS '.spec | {unitResources,localStorage}')" = \
      "$(printf '%s' "$before_type" | jq -cS '.spec | {unitResources,localStorage}')" ] || failed=1
  fi
  kubectl -n "$NS" patch secret gpustack-settings --type=merge \
    -p "$(jq -nc --arg key "$AWARE_KEY" --argjson value "$orig" '{data:{($key):$value}}')" >/dev/null || failed=1
  bounce_worker || failed=1
  current=$(kubectl -n "$NS" get secret gpustack-settings -o json | jq -c --arg key "$AWARE_KEY" '.data[$key]') || failed=1
  # Startup initializes absent keys. Restore absence after the final restart as well.
  if [ "$orig" = null ] && [ "$current" != null ]; then
    kubectl -n "$NS" patch secret gpustack-settings --type=merge \
      -p "$(jq -nc --arg key "$AWARE_KEY" '{data:{($key):null}}')" >/dev/null || failed=1
    current=$(kubectl -n "$NS" get secret gpustack-settings -o json | jq -c --arg key "$AWARE_KEY" '.data[$key]') || failed=1
  fi
  [ "$current" = "$orig" ] || failed=1
  current=$(derived_its) || failed=1
  for it in $current; do
    if ! printf '%s' "$before_json" | jq -e --arg name "$it" '.items | any(.metadata.name == $name)' >/dev/null; then
      kubectl delete instancetype "$it" --wait=false >/dev/null || failed=1
      kubectl wait --for=delete instancetype/"$it" --timeout=60s >/dev/null || failed=1
    fi
  done
  return "$failed"
}
trap 'cleanup || exit 1' EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 1. Enable awareness and force the aware re-derive (setting cache + create-only authoring).
echo "[case-19] enabling awareness and restarting the worker to force the aware re-derive"
set_aware true
bounce_worker
active=""
for _ in $(seq 1 40); do
  [ "$(kubectl get instancetype "$AWARE_IT" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Active" ] && { active=1; break; }
  sleep 3
done
[ -n "$active" ] \
  && record PASS "aware accelerated type materializes" "${AWARE_IT} Active" \
  || record FAIL "aware accelerated type materializes" "${AWARE_IT} not Active — aware split derive did not converge"

# 2. Its identity and observed descriptors: split identity + correct GPU info + folded CPU detail.
it_json="$(kubectl get instancetype "$AWARE_IT" -o json 2>/dev/null)"
sAG="$(printf '%s' "$it_json" | jq -r '.spec.acceleratorGroup // ""')"
sGG="$(printf '%s' "$it_json" | jq -r '.spec.generalGroup // ""')"
sProd="$(printf '%s' "$it_json" | jq -r '.status.detail.product // ""')"
sMem="$(printf '%s' "$it_json" | jq -r '.status.detail.memory // ""')"
sCores="$(printf '%s' "$it_json" | jq -r '.status.detail.cores // ""')"
sHasCPU="$(printf '%s' "$it_json" | jq -r 'if ((.status.detail.cpu // {}) | length) > 0 then "yes" else "no" end')"
{ [ "$sAG" = "$AKEY" ] && [ "$sGG" = "$GKEY" ]; } \
  && record PASS "aware type splits by CPU" "acceleratorGroup=${sAG} generalGroup=${sGG}" \
  || record FAIL "aware type splits by CPU" "acceleratorGroup='${sAG}' generalGroup='${sGG}', want ${AKEY}/${GKEY}"
{ [ "$sProd" = "$PRODUCT" ] && [ "$sMem" = "$MEMORY" ] && [ "$sCores" = "$CORES" ]; } \
  && record PASS "GPU descriptors correct" "product=${sProd} memory=${sMem} cores=${sCores}" \
  || record FAIL "GPU descriptors correct" "status.detail=${sProd}/${sMem}/${sCores} != flavor ${PRODUCT}/${MEMORY}/${CORES}"
[ "$sHasCPU" = "yes" ] \
  && record PASS "CPU detail folded when aware" "status.detail.cpu present" \
  || record FAIL "CPU detail folded when aware" "status.detail.cpu empty — the awareness-gated cpuDetail fold did not run"

# 3. Use the type's creation-time resource units; they are immutable.
# Compare identity sets as well as cardinality; duplicates must never satisfy a two-card claim.
same_devices() {
  jq -ne --argjson count "$1" --argjson assigned "$2" --argjson visible "$3" '
    ($assigned | length) == $count and ($assigned | unique | length) == $count and
    ($visible | length) == $count and ($visible | unique | length) == $count and
    ($assigned | sort) == ($visible | sort)' >/dev/null
}

for count in 1 2; do
  name=$INST
  if [ "$count" -eq 2 ]; then
    name=$MULTI_INST
    if ! free=$(kubectl get instancetype "$AWARE_IT" -o json | jq -er '.status.accelerator.onceMaxRequest'); then
      record FAIL "two-card capacity read" "$AWARE_IT"
      continue
    fi
    if ! printf '%s' "$before_devices" | jq -e '.items[].spec.groups[] | select(.manufacturer == "nvidia")' >/dev/null ||
       [ "${free:-0}" -lt 2 ]; then
      record SKIP "two-card NVIDIA Instance" "requires two free NVIDIA cards in the aware pool; onceMaxRequest=${free}"
      continue
    fi
  fi
  echo "[case-19] deploying ${name} accelerator=${count}"
  CREATED+=("$name")
  if ! cat <<EOF | kubectl create -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${name}, namespace: default }
spec:
  type: ${AWARE_IT}
  image: ubuntu:24.04
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
  resources:
    accelerator: "${count}"
    localStorage: 1Gi
EOF
  then
    record FAIL "${count}-card Instance created" "$name"
    continue
  fi
  phase=""
  for _ in $(seq 1 60); do
    phase=$(kubectl -n default get instance "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
    { [ "$phase" = "Ready" ] || [ "$phase" = "Running" ]; } && break
    sleep 3
  done
  if [ "$phase" = "Ready" ] || [ "$phase" = "Running" ]; then
    record PASS "${count}-card Instance reaches Ready" "${name} phase=${phase}"
  else
    record FAIL "${count}-card Instance reaches Ready" "${name} phase='${phase:-<none>}'"
  fi
  kubectl -n default get instance "$name" -o json
  pod=$(kubectl -n default get pod "$name" -o json) || { record FAIL "${count}-card Pod read" "$name"; continue; }
  printf '%s\n' "$pod"
  if [ "$(printf '%s' "$pod" | jq -r '.status.phase')" = Running ]; then
    visible=$(kubectl -n default exec "$name" -c main -- nvidia-smi --query-gpu=uuid --format=csv,noheader) || visible=""
    visible=$(printf '%s\n' "$visible" | jq -Rsc 'split("\n") | map(gsub("^[[:space:]]+|[[:space:]]+$";"")) | map(select(length>0))')
    assigned=$(printf '%s' "$pod" | jq -c '[.metadata.annotations["device.gpustack.ai/accelerator.allocated"] |
      fromjson | .main.devices.groups[].accelerators[].id]') || assigned='[]'
    if same_devices "$count" "$assigned" "$visible"; then
      record PASS "${count}-card visible UUIDs equal allocation" "assigned=${assigned} visible=${visible}"
    else
      record FAIL "${count}-card visible UUIDs equal allocation" "assigned=${assigned} visible=${visible}"
    fi
    occupied=""
    node=$(printf '%s' "$pod" | jq -r '.spec.nodeName')
    for _ in $(seq 1 20); do
      if devices=$(kubectl get devices "$node" -o json); then
        if printf '%s' "$devices" | jq -e --arg node "$node" --argjson ids "$assigned" --argjson count "$count" '
          [.status.groups[]?.accelerators[]? | select(.id as $id | $ids | index($id))] as $cards |
          .metadata.name == $node and
          ($ids | length) == $count and ($ids | unique | length) == $count and
          ($cards | map(.id) | sort) == ($ids | sort) and
          all($cards[]; .mode == 1 and (.remaining // 0) == 0)' >/dev/null; then
          occupied=1
          break
        fi
      fi
      sleep 3
    done
    printf '%s\n' "$devices"
    if [ -n "$occupied" ]; then
      record PASS "${count}-card live ledger reserves each UUID" "node=${node} assigned=${assigned}"
    else
      record FAIL "${count}-card live ledger reserves each UUID" "node=${node} assigned=${assigned}"
    fi
  else
    record FAIL "${count}-card visible UUIDs equal allocation" "pod/${name} not Running"
  fi
done

if cleanup; then
  record PASS "cleanup restores settings and claims; preserves type fields" "$AWARE_IT"
else
  record FAIL "cleanup restores settings and claims; preserves type fields" "$AWARE_IT"
fi
trap - EXIT

echo
echo "== CASE 19 — CPU-manufacturer awareness enriches the accelerated InstanceType; a real GPU Instance runs on it =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). With awareness on the derived accelerated type must split by CPU and carry"
  echo "the real GPU descriptors + the folded CPU detail, and a real GPU Instance must run on it. Diagnose:"
  echo "kubectl -n default describe instance ${INST}; kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200"
  exit 1
fi
echo "CASE 19 PASS"
