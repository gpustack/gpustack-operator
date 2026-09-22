#!/usr/bin/env bash
#
# CASE 86 — A prefill/decode pair's transfer document follows the tensor parallelism its roles
#           declare in extraArgs, and the pair actually runs at that width
#   (MUTATING, self-recovering)   AUTO-SKIPS without four free exclusive Ascend cards
#
#   case-86.sh <NS>
#
# Goal:        The transfer document of a vLLM-Ascend prefill/decode pair carries a per-half
#              parallel shape that the connector ASSERTS at worker start: a wrong block turns a
#              startup refusal into silently wrong block layouts, and the engine starts and serves
#              with them. That shape used to be rendered as an inline literal of one for every
#              deployment, so a pair whose roles ran tensor-parallel two was told it was one wide
#              on both sides. The shape is now parsed from each role's own declared engine
#              arguments (extraArgs), and this case is the reading that promotes "above one" from
#              inferred to measured: a pair declaring --tensor-parallel-size 2 on BOTH roles must
#              render 2 in BOTH halves of the document on EACH pod, must come up at that width
#              (two cards per role, four in all), and must still move KV blocks from prefill to
#              decode. Every document row asserts both halves, because a render that fills only
#              the pod's own side is the failure this feature exists to remove.
#
# Environment: A real Ascend accelerator pool whose InstanceType reports at least FOUR free
#              exclusive cards (two per role at the declared width), model weights staged at a
#              hostPath on the accelerator nodes, the vllm-ascend engine image pullable, and the
#              llm-d-router image pullable -- llm-d-router is the only router that drives the
#              Ascend transfer leg, so no other router can produce the transfer rows. The
#              namespace must carry the pool's entrance LocalQueue. Exits 2 (input required) when
#              E2E_RP_INSTANCE_TYPE or E2E_RP_NODE_SSH is unset -- the card cross-check reads
#              npu-smi on the accelerator node over SSH, and the address is passed inline at run
#              time, never hardcoded. AUTO-SKIPS (exit 0) when the pool has fewer free exclusive
#              cards than the pair needs, printing what it found.
#
# Inputs:      All real, nothing mocked. One ModelDeployment with a prefill and a decode role,
#              each requesting TWO exclusive cards and declaring --tensor-parallel-size 2 in
#              extraArgs; the load is one routed chat request above the router's disaggregation
#              threshold, driven from a throwaway probe Pod. E2E_RP_INSTANCE_TYPE (required),
#              E2E_RP_NODE_SSH / E2E_RP_NODE_SSH_OPTS (required / optional), E2E_RP_IMAGE (engine,
#              default quay.io/gpustack/runner:cann9.1-910b-vllm0.23.0-router),
#              E2E_RP_ROUTER_IMAGE (default docker.io/gpustack/llm-router:v0.1.0), E2E_RP_WEIGHTS
#              (default /data/models/model_scope/Qwen/Qwen2.5-0.5B-Instruct), E2E_RP_MODEL
#              (default /models/Qwen2.5-0.5B-Instruct), E2E_RP_TP (default 2),
#              E2E_RP_GPU_MEM_UTIL (default 0.4), E2E_RP_MAX_MODEL_LEN (default 4096),
#              E2E_RP_PROBE_IMAGE (default docker.io/library/busybox:1.37),
#              E2E_RP_READY_BOUND (default 900).
#
# Expected:    - both roles' pods reach Ready with zero restarts (no crash-loop), and the
#                deployment reports an endpoint;
#              - the --kv-transfer-config argument on EACH pod decodes with
#                kv_connector_extra_config.prefill.tp_size == 2 AND .decode.tp_size == 2 -- both
#                halves, on both pods -- and the undeclared degrees render the engine default 1;
#              - one request above the threshold is answered, and leaves the per-request transfer
#                lines on BOTH halves: "Delaying free of N blocks for request <id>" on the
#                prefill pod and "KV cache transfer for request <id> took N ms" on the decode
#                pod, the same request id. These are whole-line-absent instruments; the decode
#                engine's cumulative external prefix-cache hit rate is printed as corroboration
#                and is never a verdict on its own;
#              - the cards the operator's device.gpustack.ai/accelerator.allocated annotation
#                names for each pod are exactly the cards where npu-smi's process table carries
#                that pod's engine processes (pod uid read out of /proc/<pid>/cgroup) -- two
#                independent readings, asserted in both directions. The indexes the pair landed
#                on are printed as a reading.
#
# Cleanup:     A trap deletes the ModelDeployment and the probe Pod and, if the group wedges past
#              its bound, releases the Workload holding its replicas by hand (the manual form of
#              the release the operator is expected to do). Creates no cluster-scoped object and
#              changes no baseline; runs on pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-86.sh <NS>" >&2
  exit 2
fi

MD=case86-pd
TP="${E2E_RP_TP:-2}"
IT="${E2E_RP_INSTANCE_TYPE:-}"
IMAGE="${E2E_RP_IMAGE:-quay.io/gpustack/runner:cann9.1-910b-vllm0.23.0-router}"
ROUTER_IMAGE="${E2E_RP_ROUTER_IMAGE:-docker.io/gpustack/llm-router:v0.1.0}"
WEIGHTS="${E2E_RP_WEIGHTS:-/data/models/model_scope/Qwen/Qwen2.5-0.5B-Instruct}"
MODEL="${E2E_RP_MODEL:-/models/Qwen2.5-0.5B-Instruct}"
MEMUTIL="${E2E_RP_GPU_MEM_UTIL:-0.4}"
MAXLEN="${E2E_RP_MAX_MODEL_LEN:-4096}"
PROBE_IMAGE="${E2E_RP_PROBE_IMAGE:-docker.io/library/busybox:1.37}"
READY_BOUND="${E2E_RP_READY_BOUND:-900}"
NEED_CARDS=$((2 * TP))

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

print_rows() {
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in ${ROWS[@]+"${ROWS[@]}"}; do
    IFS="|" read -r s c o <<<"$r"
    printf "%s | %s | %s\n" "$s" "$c" "$o"
  done
}

skip() { echo "[case-86] SKIP: $1"; echo "NOTHING WAS VERIFIED"; exit 0; }

# --- input gates (exit 2) ---------------------------------------------------

command -v jq >/dev/null 2>&1 || { echo "[case-86] jq is required." >&2; exit 2; }

if [ -z "$IT" ]; then
  echo "[case-86] E2E_RP_INSTANCE_TYPE is required (an accelerated InstanceType with enough free" >&2
  echo "          exclusive cards); refusing to guess a pool." >&2
  exit 2
fi
if [ -z "${E2E_RP_NODE_SSH:-}" ]; then
  echo "[case-86] E2E_RP_NODE_SSH is required: the card cross-check reads npu-smi on the" >&2
  echo "          accelerator node over SSH. Pass it inline:" >&2
  echo "    E2E_RP_NODE_SSH=<user@host> E2E_RP_INSTANCE_TYPE=<type> bash $0 $NS" >&2
  exit 2
fi

node_ssh() {
  # shellcheck disable=SC2086
  ssh -o StrictHostKeyChecking=no -o ConnectTimeout=15 -o BatchMode=yes \
    ${E2E_RP_NODE_SSH_OPTS:-} "$E2E_RP_NODE_SSH" "$@"
}
if ! node_ssh true >/dev/null 2>&1; then
  echo "[case-86] cannot SSH to '${E2E_RP_NODE_SSH}' (BatchMode). Check address / key /" >&2
  echo "          E2E_RP_NODE_SSH_OPTS." >&2
  exit 2
fi

IT_JSON="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o json 2>/dev/null)"
if [ -z "$IT_JSON" ]; then
  echo "[case-86] InstanceType '${IT}' not found." >&2
  exit 2
fi
IT_PHASE="$(printf '%s' "$IT_JSON" | jq -r '.status.phase // ""')"
FREE_EX="$(printf '%s' "$IT_JSON" | jq -r '.status.accelerator.remaining // "0"')"
[ "$IT_PHASE" = Active ] || skip "InstanceType '${IT}' is not Active (phase '${IT_PHASE}')."
{ [ "$FREE_EX" -ge "$NEED_CARDS" ] 2>/dev/null; } \
  || skip "pool needs ${NEED_CARDS} free exclusive cards for a TP=${TP} pair, has ${FREE_EX}."

kubectl -n "$NS" get localqueue --no-headers 2>/dev/null | grep -q . || {
  echo "[case-86] namespace '${NS}' carries no LocalQueue; run where the pool's entrance" >&2
  echo "          LocalQueue exists, or the group is created and never admitted." >&2
  exit 2
}

echo "[case-86] pool '${IT}': ${FREE_EX} free exclusive cards, need ${NEED_CARDS} (TP=${TP})"

# --- cleanup ----------------------------------------------------------------

force_release() {
  local md="$1" row wl uids u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    for u in $uids; do
      case " ${row#*=} " in
        *" $u "*)
          kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
            --ignore-not-found --wait=false >/dev/null 2>&1
          break
          ;;
      esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
  return 0
}

cleanup() {
  kubectl -n "$NS" delete pod -l "gpustack.ai/e2e-probe=${MD}" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  force_release "$MD"
}
trap cleanup EXIT

# --- fixture ----------------------------------------------------------------

APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vllm
    version: "0.23.0"
  model:
    name: ${MODEL}
  roles:
  - name: prefill
    kind: prefill
    instanceType: ${IT}
    replicas: 1
    size: 1
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    resources:
      accelerator: "${TP}"
    extraArgs:
    - --max-model-len=${MAXLEN}
    - --gpu-memory-utilization=${MEMUTIL}
    - --tensor-parallel-size=${TP}
    additionalVolumes:
    - hostPath:
        path: ${WEIGHTS}
        type: Directory
      mountPath: ${MODEL}
      readOnly: true
  - name: decode
    kind: decode
    instanceType: ${IT}
    replicas: 1
    size: 1
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    resources:
      accelerator: "${TP}"
    extraArgs:
    - --max-model-len=${MAXLEN}
    - --gpu-memory-utilization=${MEMUTIL}
    - --tensor-parallel-size=${TP}
    additionalVolumes:
    - hostPath:
        path: ${WEIGHTS}
        type: Directory
      mountPath: ${MODEL}
      readOnly: true
  router:
    name: llm-d-router
    image: ${ROUTER_IMAGE}
YAML
)"

if kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" >/dev/null 2>&1; then
  record PASS "apply" "the TP=${TP} pair was admitted by the API (webhook surface accepted the declaration)"
else
  record FAIL "apply" "refused: ${APPLY_OUT}"
  print_rows
  exit 1
fi

# --- wait: pods admitted, Ready, zero restarts -------------------------------

wait_pods() { # <component> -> pod name on stdout once Ready, rc!=0 on timeout
  local comp="$1" deadline=$((SECONDS + READY_BOUND)) pods ready
  while [ "$SECONDS" -lt "$deadline" ]; do
    pods="$(kubectl -n "$NS" get pods \
      -l "app.kubernetes.io/instance=${MD},app.kubernetes.io/component=${comp}" \
      -o json 2>/dev/null)"
    ready="$(printf '%s' "$pods" | jq -r \
      '[.items[] | select([.status.conditions // [] | .[] | select(.type=="Ready" and .status=="True")] | length > 0)] | length')"
    if [ "${ready:-0}" -ge 1 ]; then
      printf '%s' "$pods" | jq -r '.items[0].metadata.name'
      return 0
    fi
    sleep 15
  done
  return 1
}

PF_POD=""; DC_POD=""
if PF_POD="$(wait_pods prefill)"; then
  record PASS "prefill pod" "${PF_POD} Ready"
else
  record FAIL "prefill pod" "no Ready prefill pod within ${READY_BOUND}s"
fi
if DC_POD="$(wait_pods decode)"; then
  record PASS "decode pod" "${DC_POD} Ready"
else
  record FAIL "decode pod" "no Ready decode pod within ${READY_BOUND}s"
fi

RESTARTS="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
  -o jsonpath='{range .items[*]}{.status.containerStatuses[*].restartCount}{" "}{end}' 2>/dev/null)"
if [ -n "$RESTARTS" ] && ! printf '%s' "$RESTARTS" | grep -qE '[^0 ]'; then
  record PASS "no crash-loop" "every container restartCount is 0 (${RESTARTS})"
else
  record FAIL "no crash-loop" "restartCounts: ${RESTARTS:-<none>}"
fi

ENDPOINT="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" \
  -o jsonpath='{.status.endpoint}' 2>/dev/null)"
if [ -n "$ENDPOINT" ]; then
  record PASS "endpoint" "${ENDPOINT}"
else
  record FAIL "endpoint" "the deployment reports no endpoint"
fi

# --- the document on each pod, both halves -----------------------------------

check_document() { # <pod> <want kv_role>
  local pod="$1" want_role="$2" args doc
  args="$(kubectl -n "$NS" get pod "$pod" -o json 2>/dev/null \
    | jq -r '[.spec.containers[] | select(.name=="main") | (.command // []) + (.args // [])][0]' 2>/dev/null)"
  [ -n "$args" ] || { record FAIL "document ${pod}" "no main container argv"; return 0; }
  doc="$(printf '%s' "$args" | jq -r --arg f "--kv-transfer-config" \
    'index($f) as $i | if $i == null then "" else .[$i + 1] end')"
  [ -n "$doc" ] || { record FAIL "document ${pod}" "no --kv-transfer-config argument"; return 0; }
  if printf '%s' "$doc" | jq -e \
    --arg c "MooncakeConnectorV1" --arg r "$want_role" --argjson tp "$TP" \
    '.kv_connector == $c and .kv_role == $r
     and .kv_connector_extra_config.prefill.tp_size == $tp
     and .kv_connector_extra_config.decode.tp_size == $tp
     and .kv_connector_extra_config.prefill.dp_size == 1
     and .kv_connector_extra_config.decode.dp_size == 1' >/dev/null 2>&1; then
    record PASS "document ${pod}" "both halves tp_size=${TP}, dp_size=1, role ${want_role}"
  else
    record FAIL "document ${pod}" "got: ${doc}"
  fi
  return 0
}
[ -z "$PF_POD" ] || check_document "$PF_POD" kv_producer
[ -z "$DC_POD" ] || check_document "$DC_POD" kv_consumer

# --- card cross-check: the operator's annotation vs npu-smi's process table --

# One SSH round trip: "<card> <pid> <cgroup>" for every pid npu-smi lists. The awk splits on runs
# of '|' or space: a process row reads $2=card $4=pid $5=name; the memory/bus-id rows above the
# process table never have a pure-numeric $4 beside an alphabetic $5, so they never parse as one.
NPU_LEDGER="$(node_ssh bash -s <<'REMOTE'
npu-smi info 2>/dev/null | awk -F'[| ]+' '$4 ~ /^[0-9]+$/ && $5 ~ /[A-Za-z]/ {print $2, $4}' |
while read -r card pid; do
  echo "$card $pid $(tr '\n' ' ' < /proc/$pid/cgroup 2>/dev/null)"
done
REMOTE
)"

card_agreement() { # <pod>
  local pod="$1" uid cards card pids stray
  uid="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}' 2>/dev/null | tr '-' '_')"
  cards="$(kubectl -n "$NS" get pod "$pod" -o json 2>/dev/null \
    | jq -r '.metadata.annotations["device.gpustack.ai/accelerator.allocated"] // ""' \
    | jq -r '[.main.devices.groups[].accelerators[].index] | sort | join(",")' 2>/dev/null)"
  [ -n "$cards" ] || { record FAIL "cards ${pod}" "no allocated annotation"; return 0; }
  # The operator's claim, confirmed by the host: every annotated card carries a process of this pod.
  local ok=true
  for card in ${cards//,/ }; do
    pids="$(printf '%s\n' "$NPU_LEDGER" | awk -v c="$card" -v u="$uid" \
      '$1 == c && index($0, u) > 0 {print $2}')"
    [ -n "$pids" ] || { ok=false; break; }
  done
  # The host's claim, confirmed by the operator: no process of this pod sits on a card it was not given.
  stray="$(printf '%s\n' "$NPU_LEDGER" | awk -v u="$uid" -v cs="$cards" \
    'index($0, u) > 0 { split(cs, a, ","); for (i in a) if (a[i] == $1) next; print $1 }' | head -1)"
  [ -z "$stray" ] || ok=false
  if $ok; then
    record PASS "cards ${pod}" "annotation cards {${cards}} == npu-smi placement of this pod's processes"
  else
    record FAIL "cards ${pod}" "annotation {${cards}} but npu-smi disagrees (stray card: ${stray:-none})"
  fi
  return 0
}
[ -z "$PF_POD" ] || card_agreement "$PF_POD"
[ -z "$DC_POD" ] || card_agreement "$DC_POD"

LANDED="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" -o json 2>/dev/null \
  | jq -r '[.items[].metadata.annotations["device.gpustack.ai/accelerator.allocated"]
            | select(. != null) | fromjson | .main.devices.groups[].accelerators[].index]
           | unique | join(",")' 2>/dev/null)"
echo "[case-86] reading: the pair landed on cards {${LANDED:-?}} of the host's eight"

# --- the request and the transfer lines on both halves ------------------------

PROMPT="Describe in detail a long train journey through high mountains, naming the passes, the weather at each one, the towns in the valleys between them, and what a traveler sees from the window as the light changes through a whole day."
REQ_ID=""
if [ -n "$ENDPOINT" ]; then
  PROBE="${MD}-probe"
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=true >/dev/null 2>&1
  cat <<YAML | kubectl apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${NS}
  labels:
    gpustack.ai/e2e-probe: ${MD}
spec:
  restartPolicy: Never
  activeDeadlineSeconds: 300
  containers:
  - name: probe
    image: ${PROBE_IMAGE}
    command:
    - sh
    - -c
    - |
      wget -qO- --header='Content-Type: application/json' \
        --post-data='{"model":"${MODEL}","messages":[{"role":"user","content":"${PROMPT}"}],"max_tokens":24,"temperature":0}' \
        ${ENDPOINT}/v1/chat/completions
YAML
  deadline=$((SECONDS + 300))
  phase=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get pod "$PROBE" -o jsonpath='{.status.phase}' 2>/dev/null)"
    case "$phase" in Succeeded|Failed) break ;; esac
    sleep 10
  done
  RESP="$(kubectl -n "$NS" logs "$PROBE" 2>/dev/null)"
  REQ_ID="$(printf '%s' "$RESP" | jq -r '.id // empty' 2>/dev/null)"
  if [ -n "$REQ_ID" ]; then
    record PASS "request" "answered as ${REQ_ID} (probe ${phase})"
  else
    record FAIL "request" "probe ${phase:-timeout}; response: $(printf '%s' "$RESP" | head -c 300)"
  fi
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1
fi

if [ -n "$REQ_ID" ] && [ -n "$PF_POD" ] && [ -n "$DC_POD" ]; then
  sleep 5
  SEND_LINE="$(kubectl -n "$NS" logs "$PF_POD" -c main --tail=3000 2>/dev/null \
    | grep "Delaying free of" | grep "$REQ_ID" | tail -1)"
  if [ -n "$SEND_LINE" ]; then
    record PASS "prefill half" "${SEND_LINE}"
  else
    record FAIL "prefill half" "no 'Delaying free of N blocks' line names ${REQ_ID}"
  fi
  PULL_LINE="$(kubectl -n "$NS" logs "$DC_POD" -c main --tail=3000 2>/dev/null \
    | grep "KV cache transfer for request" | grep "$REQ_ID" | tail -1)"
  if [ -n "$PULL_LINE" ]; then
    record PASS "decode half" "${PULL_LINE}"
  else
    record FAIL "decode half" "no 'KV cache transfer took N ms' line names ${REQ_ID}"
  fi
  HIT="$(kubectl -n "$NS" logs "$DC_POD" -c main --tail=200 2>/dev/null \
    | grep "External prefix cache hit rate" | tail -1 | sed 's/^.*External prefix/External prefix/')"
  echo "[case-86] corroboration (cumulative, never a verdict alone): ${HIT:-<no stats line>}"
fi

# --- table -------------------------------------------------------------------

print_rows
if [ "$FAILS" -gt 0 ]; then
  echo
  echo "[case-86] FAIL (${FAILS} row(s))"
  exit 1
fi
echo
echo "[case-86] PASS"
