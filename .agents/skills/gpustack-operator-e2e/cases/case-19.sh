#!/usr/bin/env bash
#
# CASE 19 — CPU-manufacturer awareness enriches the accelerated InstanceType; a real GPU Instance runs on it   (MUTATING, self-recovering; AUTO-SKIPS without a real accelerator)
#
#   case-19.sh <NS>
#
# Goal:        With instance-type-aware-cpu-manufacturer ON, the derived accelerated pool splits by CPU
#              (gpustack--${gKey}--${aKey}-${os}-${arch}) and its InstanceType spec carries BOTH the
#              correct GPU descriptors (product/memory/cores, from the real card) AND the folded CPU
#              detail (spec.cpu). Then a real GPU Instance deploys onto that aware type and its Pod runs
#              with the card visible — the full aware→derive→enrich→admit→schedule chain.
# Environment: Needs REAL accelerator hardware (a real card the Instance actually schedules onto).
#              AUTO-SKIPS (exit 0, prints why) on a GPU-less cluster. Flips a cluster-wide editable
#              setting and restarts the worker, then restores — run when the cluster is otherwise idle.
# Inputs:      All real, nothing mocked —
#              - patches the gpustack-settings Secret awareness key to "true" and restarts the worker to
#                force the aware re-derive;
#              - an Instance gpustack-e2e-case19 (accelerator=1, ubuntu sleep) on the aware accelerated type.
# Expected:    - the aware type gpustack--${gKey}--${aKey}-${os}-${arch} materializes Active with
#                acceleratorGroup=${aKey}, generalGroup=${gKey}, GPU product/memory/cores == the flavor's,
#                and a non-empty spec.cpu (the awareness-gated CPU fold ran);
#              - the Instance reaches Ready and its Pod runs with nvidia-smi seeing the card.
# Cleanup:     Trap deletes the Instance, restores the setting to its original value, restarts the worker,
#              and removes any derived type the aware window created (snapshot diff).
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

set_aware() { local b64; b64=$(printf '%s' "$1" | base64 | tr -d '\n')
  kubectl -n "$NS" patch secret gpustack-settings --type=merge -p "{\"data\":{\"${AWARE_KEY}\":\"${b64}\"}}" >/dev/null 2>&1; }
derived_its() { kubectl get instancetypes.worker.gpustack.ai -l "${DERIVED_LABEL}=true" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null; }
note() { kubectl get resourceflavor "$1" -o jsonpath="{.metadata.annotations.note\.gpustack\.ai/$2}" 2>/dev/null; }
bounce_worker() { kubectl -n "$NS" rollout restart deploy/gpustack-operator-worker >/dev/null 2>&1;
  kubectl -n "$NS" rollout status deploy/gpustack-operator-worker --timeout=180s >/dev/null 2>&1; }

# --- Skip gate: a real accelerated ResourceFlavor (device flavor gpustack--${gKey}--${aKey}-…-Nd). ---
ARF=$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null | sed 's#.*/##' | grep -E '^gpustack--.+--.+-[0-9]+d$' | head -1)
if [ -z "$ARF" ]; then
  echo "== CASE 19 — SKIPPED =="
  echo "No accelerated ResourceFlavor (gpustack--\${gKey}--\${aKey}-…-Nd) — this case needs real accelerator"
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

orig=$(kubectl -n "$NS" get secret gpustack-settings -o jsonpath="{.data.${AWARE_KEY}}" 2>/dev/null | base64 -d 2>/dev/null)
[ "$orig" = "true" ] || orig=false
before_its=" $(derived_its) "

cleanup() {
  echo
  echo "[case-19] cleanup: deleting Instance, restoring ${AWARE_KEY}=${orig}, restarting worker"
  kubectl -n default delete instance "$INST" --ignore-not-found >/dev/null 2>&1 || true
  set_aware "$orig"
  bounce_worker
  for it in $(derived_its); do
    case "$before_its" in
      *" $it "*) : ;;
      *) kubectl patch instancetype "$it" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
         kubectl delete instancetype "$it" --wait=false >/dev/null 2>&1 || true
         kubectl delete clusterqueue "$it" --wait=false >/dev/null 2>&1 || true ;;
    esac
  done
}
trap cleanup EXIT

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

# 2. Its descriptors: split identity + correct GPU info + folded CPU detail.
read -r sAG sGG sProd sMem sCores sHasCPU <<<"$(kubectl get instancetype "$AWARE_IT" -o json 2>/dev/null | python3 -c "
import json,sys
try: s=json.load(sys.stdin).get('spec',{})
except Exception: s={}
cpu=s.get('cpu') or {}
print(s.get('acceleratorGroup',''), s.get('generalGroup',''), s.get('product',''), s.get('memory',''), s.get('cores',''), ('yes' if cpu else 'no'))
")"
{ [ "$sAG" = "$AKEY" ] && [ "$sGG" = "$GKEY" ]; } \
  && record PASS "aware type splits by CPU" "acceleratorGroup=${sAG} generalGroup=${sGG}" \
  || record FAIL "aware type splits by CPU" "acceleratorGroup='${sAG}' generalGroup='${sGG}', want ${AKEY}/${GKEY}"
{ [ "$sProd" = "$PRODUCT" ] && [ "$sMem" = "$MEMORY" ] && [ "$sCores" = "$CORES" ]; } \
  && record PASS "GPU descriptors correct" "product=${sProd} memory=${sMem} cores=${sCores}" \
  || record FAIL "GPU descriptors correct" "spec=${sProd}/${sMem}/${sCores} != flavor ${PRODUCT}/${MEMORY}/${CORES}"
[ "$sHasCPU" = "yes" ] \
  && record PASS "CPU detail folded when aware" "spec.cpu present" \
  || record FAIL "CPU detail folded when aware" "spec.cpu empty — the awareness-gated cpuDetail fold did not run"

# 3. Deploy a real GPU Instance on the aware type: set a unit spec, then run one whole card.
for _ in $(seq 1 15); do
  kubectl patch instancetype "$AWARE_IT" --type=merge \
    -p '{"spec":{"unitResources":{"cpu":"2","ram":"4Gi"},"localStorage":"20Gi"}}' >/dev/null 2>&1
  [ -n "$(kubectl get instancetype "$AWARE_IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)" ] && break
  sleep 3
done
echo "[case-19] deploying GPU Instance ${INST} (accelerator=1) on ${AWARE_IT}"
cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INST}, namespace: default }
spec:
  type: ${AWARE_IT}
  image: ubuntu:24.04
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
  resources:
    accelerator: "1"
EOF
phase=""
for _ in $(seq 1 60); do
  phase=$(kubectl -n default get instance "$INST" -o jsonpath='{.status.phase}' 2>/dev/null)
  { [ "$phase" = "Ready" ] || [ "$phase" = "Running" ]; } && break
  sleep 3
done
{ [ "$phase" = "Ready" ] || [ "$phase" = "Running" ]; } \
  && record PASS "GPU Instance reaches Ready" "${INST} phase=${phase} (aware→derive→enrich→admit→schedule)" \
  || record FAIL "GPU Instance reaches Ready" "${INST} phase='${phase:-<none>}' — full accelerated deploy chain did not complete"

# The Pod is named after the Instance; confirm the card is actually visible inside it.
if [ "$(kubectl -n default get pod "$INST" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; then
  smi=$(kubectl -n default exec "$INST" -- nvidia-smi -L 2>/dev/null | grep -c '^GPU ' || true)
  [ "${smi:-0}" -ge 1 ] \
    && record PASS "card visible in the Instance Pod" "nvidia-smi -L lists ${smi} GPU(s)" \
    || record FAIL "card visible in the Instance Pod" "nvidia-smi -L saw no GPU (device plugin injection?)"
else
  record FAIL "card visible in the Instance Pod" "pod/${INST} not Running"
fi

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
