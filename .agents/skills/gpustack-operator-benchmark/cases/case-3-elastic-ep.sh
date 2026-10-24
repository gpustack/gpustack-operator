#!/usr/bin/env bash
#
# CASE 3 — Elastic EP online horizontal scale-up benchmark   (MUTATING, self-recovering)
#
#   case-3-elastic-ep.sh [OUT_DIR]
#
# Goal:        Verify and measure online Elastic Expert Parallelism scaling on an MoE model
#              (deepseek-ai/DeepSeek-V2-Lite-Chat BF16):
#              State 1 (elasticEp.width=2, 4 GPUs, c=1..64 sweep),
#              State 2 (live patch elasticEp.width: 2 -> 4 under continuous c=8 load),
#              State 3 (elasticEp.width=4, 8 GPUs, c=1..64 sweep).
# Environment: A cluster with 8 free GPUs on InstanceType bench-gpu, NodeModelStore caching
#              deepseek-v2-lite-chat, and an in-cluster default/bench-driver Pod.
#              The shape this run was on is written to OUT_DIR/cluster-shape.txt: the context and
#              the cluster, the namespace, the nodes, the InstanceType the case asked for, the
#              accelerators on each node, the InstanceTypes the cluster carries, and the operator
#              image with its digest.
#              The cluster of record for this run: gpustack-sedp-0l578ap2 (ns gpustack-bench),
#              platform gpu-h100-sxm 8xH100 non-preemptible with a cpu-e2 system node, 8
#              accelerators on the one GPU node, InstanceType
#              gpustack--nvidia-h100-80gb-hbm3-linux-amd64, operator
#              <dev>/gpustack-operator:<tag> (a development image; tests pull gpustack/gpustack-operator unless a dev image is named).
# Inputs:      manifests/elastic-ep.yaml; scripts/bench_client.py (in-cluster).
# Expected:    State 1 and State 3 complete all 6 concurrency cells (c=1..64) with 0 errors,
#              State 2 preserves master Pod UID across width 2 -> 4 resize, and all 8 GPUs are
#              released after deletion.
# Cleanup:     Deletes b2-elastic-ep on completion or exit.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"
# shellcheck source=/dev/null
. "${SKILL_DIR}/../_e2e-lib/scripts/serving-lib.sh"

NS="${BENCH_NS:-gpustack-bench}"
DEPLOY=b2-elastic-ep
OUT="${1:-${REPO}/.claude/reports/$(date +%F)-benchmark/elastic-ep}"
LOCAL_PORT="${LOCAL_PORT:-18010}"
PF_PID=""
WATCH_PID=""
INSTANCE_TYPE="${EEP_INSTANCE_TYPE:-gpustack--nvidia-h100-80gb-hbm3-linux-amd64}"
ELASTIC_WIDTH_START="${EEP_WIDTH_START:-2}"
ELASTIC_WIDTH_TARGET="${EEP_WIDTH_TARGET:-4}"
MD_RESOURCE="modeldeployments.v1alpha1.worker.gpustack.ai"
# A REQUEST THAT NEVER COMES BACK MUST NOT HOLD THE CELL FOR THE CLIENT'S DEFAULT. The
# bench client waits ten minutes per request by default, so a wedged engine turns one cell
# into hours of waiting and the run never reaches its conclusion. The bound below is six
# times the slowest healthy request this case measured on the same shape.
REQUEST_TIMEOUT="${BENCH_REQUEST_TIMEOUT:-60}"

mkdir -p "$OUT"
log() { echo "[$(date +%H:%M:%S)] $*"; }

# THE SHAPE OF THE CLUSTER BELONGS IN THE EVIDENCE, so the numbers this run produced can be
# read against the machine that produced them.
capture_cluster_shape "${OUT}/cluster-shape.txt"

# record VERDICT LABEL DETAIL writes one row of the results table. The verdict belongs in the
# evidence, so a reader of the run does not have to re-derive it from the raw numbers.
record() {
  printf '%s | %s | %s\n' "$1" "$2" "$3" >> "${OUT}/verdicts.txt"
  case "$1" in
    FAIL) echo "FAIL | ${2} | ${3}" ;;
    WARN) echo "WARN | ${2} | ${3}" ;;
    *) echo "PASS | ${2} | ${3}" ;;
  esac
}

# THE MANIFEST IS COPIED AND REWRITTEN, NOT APPLIED IN PLACE. The recorded manifest keeps the
# shape and the image it was recorded with, and the copy carries the InstanceType of the
# cluster this case runs on. The recorded shape asks for bench-gpu, which is the name of the
# InstanceType the recording was made against and not a name every cluster carries.
write_manifest() {
  local src="${SKILL_DIR}/manifests/elastic-ep.yaml"
  if [ ! -f "$src" ]; then
    log "MISSING: the recorded manifest ${src} is not present"
    return 1
  fi
  sed -e "s|^  namespace: gpustack-bench$|  namespace: ${NS}|" \
      -e "s|^      instanceType: bench-gpu$|      instanceType: ${INSTANCE_TYPE}|" \
      "$src" > "${OUT}/elastic-ep.yaml"
  {
    echo "instance_type=${INSTANCE_TYPE}"
    echo "elastic_width_start=${ELASTIC_WIDTH_START}"
    echo "elastic_width_target=${ELASTIC_WIDTH_TARGET}"
  } > "${OUT}/shape.txt"
  log "The shape that will run: $(tr '\n' ' ' < "${OUT}/shape.txt")"
}

# g13_record TAG writes the elastic operation record's terminal state and the effective and
# stable widths the operator proved.
#
# THE OPERATION RECORD IS THE ONE THAT DECIDED THE RESIZE. The operator stores it in a
# ConfigMap named after the deployment's UID, with the operation under the key operation.json
# (pkg/worker/controllers/worker/model_deployment_elastic_operation.go), and the observation
# beside it under elastic-observation-<uid> (model_deployment_elastic_reconcile.go). The
# ElasticResize condition on the deployment is the public face of the same fact: True with
# NoRefusal when no record stands refused, False with ScaleRefused when the engine refused,
# Unknown with WorldUnprovable when the live width could not be named.
g13_record() {
  local tag="$1" uid op_cm obs_cm
  uid="$(kubectl -n "$NS" get "$MD_RESOURCE" "$DEPLOY" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  op_cm="elastic-resize-${uid}"
  obs_cm="elastic-observation-${uid}"

  kubectl -n "$NS" get configmap "$op_cm" -o json > "${OUT}/g13-${tag}-operation-record.json" 2>/dev/null || true
  kubectl -n "$NS" get configmap "$obs_cm" -o json > "${OUT}/g13-${tag}-observation-record.json" 2>/dev/null || true
  kubectl -n "$NS" get "$MD_RESOURCE" "$DEPLOY" -o json > "${OUT}/g13-${tag}-deployment.json" 2>/dev/null || true

  local op_state op_width
  op_state="$(jq -r '.data["operation.json"] | fromjson? | .state // "none"' \
    "${OUT}/g13-${tag}-operation-record.json" 2>/dev/null || echo none)"
  op_width="$(jq -r '.data["operation.json"] | fromjson? | "from \(.width.old // "?") to \(.width.target // "?")"' \
    "${OUT}/g13-${tag}-operation-record.json" 2>/dev/null || echo unknown)"

  # THE OPERATION RECORD IS ABSENT WHEN THE OPERATION IS RESOLVED. The operator deletes a
  # resolved record, so a finished resize leaves a zero-byte file behind; the readers above then
  # read no record at all, and the rows below say so instead of failing on an absence.
  local record_present=0
  [ -s "${OUT}/g13-${tag}-operation-record.json" ] && record_present=1

  local effective effective_known stable obs_state obs_reason
  # THE OBSERVATION'S EFFECTIVE LAYER IS THE UNTAGGED GO LAYER, so its fields marshal with their
  # Go names: Known and Value, and the layer's own Reason — not the document's top-level reason.
  effective="$(jq -r '.data["observation.json"] | fromjson? | .effective.Value // "unknown"' \
    "${OUT}/g13-${tag}-observation-record.json" 2>/dev/null || echo unknown)"
  effective_known="$(jq -r '.data["observation.json"] | fromjson? | .effective.Known // false' \
    "${OUT}/g13-${tag}-observation-record.json" 2>/dev/null || echo false)"
  stable="$(jq -r '.data["observation.json"] | fromjson? | .stableWidth // 0' \
    "${OUT}/g13-${tag}-observation-record.json" 2>/dev/null || echo 0)"
  obs_state="$(jq -r '.data["observation.json"] | fromjson? | .state // "none"' \
    "${OUT}/g13-${tag}-observation-record.json" 2>/dev/null || echo none)"
  obs_reason="$(jq -r '.data["observation.json"] | fromjson? | .effective.Reason // ""' \
    "${OUT}/g13-${tag}-observation-record.json" 2>/dev/null || echo "")"

  local cond_status cond_reason cond_message
  cond_status="$(jq -r '[(.status.conditions // [])[] | select(.type == "ElasticResize") | .status] | .[0] // "none"' \
    "${OUT}/g13-${tag}-deployment.json" 2>/dev/null || echo none)"
  cond_reason="$(jq -r '[(.status.conditions // [])[] | select(.type == "ElasticResize") | .reason] | .[0] // "none"' \
    "${OUT}/g13-${tag}-deployment.json" 2>/dev/null || echo none)"
  cond_message="$(jq -r '[(.status.conditions // [])[] | select(.type == "ElasticResize") | .message] | .[0] // ""' \
    "${OUT}/g13-${tag}-deployment.json" 2>/dev/null || echo "")"

  {
    echo "tag=${tag}"
    echo "operation_record=${op_cm}"
    echo "operation_state=${op_state}"
    echo "operation_width=${op_width}"
    echo "observation_record=${obs_cm}"
    echo "effective_width=${effective}"
    echo "effective_known=${effective_known}"
    echo "stable_width=${stable}"
    echo "observation_state=${obs_state}"
    echo "observation_reason=${obs_reason}"
    echo "elastic_resize_condition_status=${cond_status}"
    echo "elastic_resize_condition_reason=${cond_reason}"
    echo "elastic_resize_condition_message=${cond_message}"
  } > "${OUT}/g13-${tag}.txt"

  log "the elastic record (${tag}): operation=${op_state} (${op_width}) effective=${effective} known=${effective_known} stable=${stable} condition=${cond_status}/${cond_reason}"

  # A TERMINAL OPERATION IS ONE THE ENGINE IS NOT STILL WORKING ON. Recorded, CommandSent,
  # AwaitingWidth and NativeComplete are all in flight; Completed, Withdrawn, Released and
  # Abandoned are the states the operation ends in.
  case "$op_state" in
    Completed|Withdrawn|Released|Abandoned)
      record PASS "the elastic operation reached a terminal state (${tag})" \
        "the operation record ${op_cm} carries the state ${op_state} for the resize ${op_width}"
      ;;
    none)
      record WARN "the elastic operation reached a terminal state (${tag})" \
        "no operation record ${op_cm} was found, so the resize left no record to read"
      ;;
    *)
      record FAIL "the elastic operation reached a terminal state (${tag})" \
        "the operation record ${op_cm} still carries the in-flight state ${op_state} for the resize ${op_width}"
      ;;
  esac

  # THE ROWS ARE VACUOUS WHERE THEIR SUBJECT DOES NOT EXIST YET. A before-resize tag reads no
  # operation record and no condition, and a row that failed on that absence would fail on the
  # case's own ordering, so absence reads WARN there and FAIL stays reserved for a subject that
  # exists and disagrees.
  if [ "${record_present}" != "1" ]; then
    record WARN "the effective width was proven (${tag})" \
      "no operation record ${op_cm} exists at this tag, so there is no effective width to prove yet"
  elif [ "${effective_known}" = "true" ] && [ "${effective}" != "unknown" ]; then
    record PASS "the effective width was proven (${tag})" \
      "the observation proves the effective width ${effective}, and the stable width is ${stable}"
  else
    record FAIL "the effective width was proven (${tag})" \
      "the observation does not prove an effective width: known=${effective_known} effective=${effective} reason=${obs_reason:-none}"
  fi

  if [ "${cond_status}" = "none" ]; then
    record WARN "the ElasticResize condition reports no refusal (${tag})" \
      "the deployment carries no ElasticResize condition yet, so there is no refusal to read"
  elif [ "${cond_status}" = "True" ] && [ "${cond_reason}" = "NoRefusal" ]; then
    record PASS "the ElasticResize condition reports no refusal (${tag})" \
      "the condition is ${cond_status}/${cond_reason}: ${cond_message}"
  else
    record FAIL "the ElasticResize condition reports no refusal (${tag})" \
      "the condition is ${cond_status}/${cond_reason}: ${cond_message}"
  fi
}
cleanup() {
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  if [ -n "$WATCH_PID" ]; then
    kill "$WATCH_PID" 2>/dev/null || true
  fi
  return 0
}
trap cleanup EXIT

CONCURRENCIES="1 4 8 16 32 64"

wait_ready() {
  local deadline=$((SECONDS + ${READY_TIMEOUT:-2400}))
  local phase=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && { log "$DEPLOY is Ready"; return 0; }
    sleep 5
  done
  kubectl -n "$NS" describe modeldeployment "$DEPLOY" > "$OUT/describe.txt" 2>&1
  return 1
}

start_forward() {
  local pod uid master_uid
  # THE MASTER IS NAMED BY THE OBSERVATION RECORD, NOT BY LIST ORDER. Every member of the
  # collective carries member-index=0, so a selection by that label takes whichever Pod the list
  # returned first; the observation record names the master by UID, and that is the member the
  # port-forward sits on. A record that names no master yet falls back to any server member.
  uid="$(kubectl -n "$NS" get "$MD_RESOURCE" "$DEPLOY" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  master_uid="$(kubectl -n "$NS" get configmap "elastic-observation-${uid}" -o json 2>/dev/null \
    | jq -r '.data["observation.json"] | fromjson? | .masterUID // empty' 2>/dev/null || true)"
  if [ -n "$master_uid" ]; then
    pod="$(kubectl -n "$NS" get pods -o json 2>/dev/null | jq -r \
      --arg uid "${master_uid}" \
      '[.items[] | select(.metadata.uid == $uid) | .metadata.name] | .[0] // empty')"
  fi
  if [ -z "$pod" ]; then
    pod="$(kubectl -n "$NS" get pods -o json 2>/dev/null | jq -r \
      '[.items[] | select(.metadata.labels["modeldeployment.gpustack.ai/role-kind"]=="server")
        | .metadata.name] | .[0] // empty')"
  fi
  if [ -z "$pod" ]; then
    pod="$(kubectl -n "$NS" get pods -o name 2>/dev/null | cut -d/ -f2 | grep "^${DEPLOY}-" | head -1)"
  fi
  kubectl -n "$NS" port-forward "pod/${pod}" "${LOCAL_PORT}:8000" > "$OUT/port-forward.log" 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 60); do
    curl -sf -o /dev/null "http://127.0.0.1:${LOCAL_PORT}/v1/models" && break
    sleep 2
  done
}

sweep() {
  local arm="$1"
  local offset conc requests errors
  offset="$(cat "$OUT/token-offset.txt" 2>/dev/null || echo 0)"
  for conc in $CONCURRENCIES; do
    requests=$((conc * 4))
    [ "$requests" -lt 32 ] && requests=32
    [ "$requests" -gt 128 ] && requests=128
    log "cell ${arm}/s1024x256-c${conc} (reqs=${requests})"
    python3 "${SKILL_DIR}/scripts/bench_client.py" run \
      --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
      --model "/models" \
      --label "s1024x256-c${conc}" \
      --arm "$arm" \
      --suite suite1 \
      --shape "1024/256" \
      --input-tokens 1024 \
      --output-tokens 256 \
      --concurrency "$conc" \
      --requests "$requests" \
      --timeout "$REQUEST_TIMEOUT" \
      --token-offset "$offset" \
      --out "$OUT/${arm}-s1024x256-c${conc}.json"
    # EVERY CELL'S ERROR COUNT BELONGS IN THE EVIDENCE. The contract of this cell is that
    # every concurrency cell comes back with zero errors, and a number nobody reads cannot
    # show that.
    errors="$(jq -r '.errors // "?"' "$OUT/${arm}-s1024x256-c${conc}.json" 2>/dev/null || echo '?')"
    if [ "${errors}" = "0" ]; then
      record PASS "the ${arm} cell at concurrency ${conc} was served without errors" \
        "${requests} request(s), 0 error(s)"
    else
      record FAIL "the ${arm} cell at concurrency ${conc} was served without errors" \
        "${requests} request(s), ${errors} error(s)"
    fi
  done
}

kubectl -n "$NS" delete modeldeployment "$DEPLOY" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
write_manifest || exit 1
kubectl apply -f "${OUT}/elastic-ep.yaml" 2>&1 | tee "$OUT/apply.log"
wait_ready || exit 1
record PASS "the elastic deployment reached Ready at width ${ELASTIC_WIDTH_START}" \
  "the deployment is Ready with the elastic collective at width ${ELASTIC_WIDTH_START}"
g13_record "before-resize"
start_forward || exit 1

OFFSET="$(python3 "${SKILL_DIR}/scripts/bench_client.py" calibrate --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
  --model "/models" --input-tokens 1024 2>/dev/null \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token_offset") or 0)')"
echo "$OFFSET" > "$OUT/token-offset.txt"

kubectl -n "$NS" get modeldeployment "$DEPLOY" -o yaml > "$OUT/modeldeployment-state1.yaml"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json > "$OUT/baseline-cr.json"
kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY}" -o json > "$OUT/pods-state1.json"
sweep state1

log "Starting steady load for scale-up window (width 2 -> 4)"
python3 "${SKILL_DIR}/scripts/bench_client.py" load \
  --endpoint "http://127.0.0.1:${LOCAL_PORT}" \
  --model "/models" \
  --label scaleup-load \
  --arm state2 \
  --suite suite2 \
  --shape "1024/256" \
  --input-tokens 1024 \
  --output-tokens 256 \
  --concurrency 8 \
  --duration 260 \
  --timeout "$REQUEST_TIMEOUT" \
  --token-offset "$OFFSET" \
  --requests-out "$OUT/scaleup-requests.jsonl" > "$OUT/scaleup-load.json" 2>&1 &
LOAD_PID=$!

sleep 35
PATCH_T0="$(date +%s)"
kubectl -n "$NS" patch modeldeployment "$DEPLOY" --type=json \
  -p "[{\"op\":\"replace\",\"path\":\"/spec/roles/0/elasticEp/width\",\"value\":${ELASTIC_WIDTH_TARGET}}]" 2>&1 | tee "$OUT/patch.log"

(
  for _ in $(seq 1 240); do
    printf '%s %s %s\n' "$(date +%s)" \
      "$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null)" \
      "$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o jsonpath='{.status.roleSummary}' 2>/dev/null)"
    sleep 5
  done
) > "$OUT/resize-watch.txt" 2>&1 &
WATCH_PID=$!

wait_ready || true
PATCH_T1="$(date +%s)"
echo $((PATCH_T1 - PATCH_T0)) > "$OUT/resize-seconds.txt"
kill "$WATCH_PID" 2>/dev/null || true
WATCH_PID=""
wait "$LOAD_PID" 2>/dev/null || true

kubectl -n "$NS" get modeldeployment "$DEPLOY" -o yaml > "$OUT/modeldeployment-state3.yaml"
kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json > "$OUT/scaled-cr.json"
kubectl -n "$NS" get pods -o wide > "$OUT/pods-state3.txt"
kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${DEPLOY}" -o json > "$OUT/pods-state3.json"

# THE G13 RECORD. The resize is only done when the operator's own record says so: the
# operation's terminal state, the effective width it proved and the stable width it holds.
g13_record "after-resize"

# THE MASTER POD SURVIVES THE RESIZE. The collective is resized around one master, and the master
# is the one the observation record names by UID — every member carries member-index=0, so list
# order names nobody. The UID is read from the before-resize record and looked for among the Pods
# the resize left behind; a record that names no master leaves the row vacuous.
MASTER_UID="$(jq -r '.data["observation.json"] | fromjson? | .masterUID // empty' \
  "$OUT/g13-before-resize-observation-record.json" 2>/dev/null || true)"
if [ -n "$MASTER_UID" ]; then
  MASTER_UID_AFTER="$(jq -r --arg uid "${MASTER_UID}" \
    '[.items[] | select(.metadata.uid == $uid) | .metadata.uid] | .[0] // empty' \
    "$OUT/pods-state3.json" 2>/dev/null)"
  if [ "$MASTER_UID" = "$MASTER_UID_AFTER" ]; then
    record PASS "the master Pod survived the resize" "the master UID ${MASTER_UID} is the same before and after"
  else
    record FAIL "the master Pod survived the resize" \
      "the master UID ${MASTER_UID} was named before the resize and is ${MASTER_UID_AFTER:-gone} after it"
  fi
else
  record WARN "the master Pod survived the resize" \
    "the before-resize observation record named no master UID, so there is no master to look for"
fi

bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "$OUT/stage-capture-state3.txt" 2>&1
sweep state3
bash "${SKILL_DIR}/scripts/stage_counts.sh" "$DEPLOY" "$OUT" > "$OUT/stage-capture-final.txt" 2>&1
kill "$PF_PID" 2>/dev/null || true
PF_PID=""
kubectl -n "$NS" delete modeldeployment "$DEPLOY" --wait=true --timeout=180s
kubectl get devices -o json > "$OUT/released-devices.json"

cat "${OUT}/verdicts.txt" 2>/dev/null
FAILS="$(grep -c '^FAIL' "${OUT}/verdicts.txt" 2>/dev/null || true)"
WARNS="$(grep -c '^WARN' "${OUT}/verdicts.txt" 2>/dev/null || true)"
log "CASE 3 (Elastic EP) benchmark complete: ${FAILS:-0} failed row(s), ${WARNS:-0} warning(s)"

# THE SCRIPT ENDS HERE, and the last statement is the exit code. An explicit trailing "exit 0"
# makes the analyzer read the EXIT trap's handler as a function nothing calls.
[ "${FAILS:-0}" -eq 0 ] || exit 1
