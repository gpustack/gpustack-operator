#!/usr/bin/env bash
#
# CASE 5: capacity comparison or legacy topology benchmark (mutating).
# Capacity comparison: --capacity-comparison --config FILE --out DIR --validate-only|--execute
# The description below applies to the legacy topology mode.
#
#   case-5-autoscaling.sh [OUT_DIR] [--parallel|--serial] [--arm <arm-name>]
#
# Goal:        Measure what a ModelDeploymentScaler does to a serving deployment, and what the
#              deployment costs while it does it. Five arms run four algorithms against four
#              topologies, and every arm is measured the same way: a small baseline, a ramp
#              that outruns the one replica, a sustain at the ramp's height, and a cooldown. A
#              sampler runs beside the load and records what the scaler decided, what the
#              deployment became, and which retirement carried the count back down, so the
#              latency of the arm can be read against the scaling it caused.
#
#              THE LOAD IS DRIVEN IN THE CLUSTER, through default/bench-driver, because a
#              port-forwarded SSE stream bottlenecks on the API server's SPDY proxy and drops
#              connections exactly when the arm is under the most load. The endpoint is the
#              cluster-internal Service DNS, so nothing is forwarded at all.
#
#              THE TWO WAVES SHARE THE EIGHT GPUs, and each wave splits them. Wave 1 runs two
#              4-GPU slots side by side: one on the vLLM prefill/decode pair, one on the SGLang
#              pair. Wave 2 runs a 6-GPU slot on the vLLM encode/prefill/decode triple beside a
#              2-GPU slot that runs the two E2E cases against a single server. A slot never
#              exceeds the GPUs it was given, and the slots of a wave are independent
#              deployments, so one arm's scaling never touches another's.
#
#              THE ARMS THAT SHARE A DEPLOYMENT RESET IT THROUGH ITS OWNER. A bound deployment
#              has one writer of its replica count, and that writer is the scaler, so the count
#              is put back by deleting the scaler and letting the binding go, never by patching
#              the count behind the fence.
#
#              THE ADMISSION CASE RUNS FIRST, on a cluster with nothing else on it. It builds
#              deployments of its own that ask for up to six GPUs, and a wave asks for eight, so
#              the two must not overlap. Its exit code is recorded next to the arms', so a summary
#              cannot look healthy over a broken case.
#
#              THE DRIVER IS NOT OPTIONAL. The load runs against a cluster-internal Service name
#              the host cannot resolve, so a case that could not make the driver ready would
#              measure the runner's network instead of the serving path. The case stops there.
#
# Environment: A cluster with 8 free GPUs on InstanceType bench-gpu, the NodeModelStore cache
#              warm for qwen3-8b and qwen2-5-vl-7b-instruct (manifests/10-model-delivery.yaml),
#              and an in-cluster default/bench-driver Pod carrying python3 and aiohttp. The
#              case creates the driver when it is missing.
#
# Inputs:      manifests/autoscaling-{vllm-pd,vllm-pd-b,sglang-pd,vllm-epd,e2e-server}.yaml,
#              scripts/bench_client.py and scripts/bench_vlm.py (both in-cluster), and the two
#              E2E cases in the gpustack-operator-e2e skill.
#
# Expected:    Every arm completes its four phases with the load answered in-cluster, a
#              timeline that shows the replica count rising under the ramp and the retirement
#              that carried it down, a per-arm summary, and a comparison across the arms. The
#              comparison carries the exit code of each E2E case the run drove.
#
# Cleanup:     Deletes every deployment and every scaler it created, on completion and on exit.
set -uo pipefail

CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ "${1:-}" == "--capacity-comparison" ]]; then
  shift
  exec bash "${CASE_DIR}/../scripts/autoscaling_rounds.sh" "$@"
fi
if [[ "${1:-}" == "--legacy-topology" ]]; then
  shift
fi
SKILL_DIR="$(cd "${CASE_DIR}/.." && pwd)"
REPO="$(cd "${SKILL_DIR}/../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"
export BENCH_INCLUSTER=1

NS="${BENCH_NS:-gpustack-bench}"
DRIVER_NS="${BENCH_DRIVER_NS:-default}"
DRIVER_POD="${BENCH_DRIVER:-bench-driver}"
DRIVER_IMAGE="${BENCH_C5_DRIVER_IMAGE:-docker.io/library/python:3.11-slim}"
SCALER_RESOURCE=modeldeploymentscalers.v1alpha1.worker.gpustack.ai
# THE DEPLOYMENT IS READ FROM ITS CRD, not from the aggregated v1 API. The aggregated API serves
# the same kind but proxies it through the operator's cached client, so a read through it can
# answer with a status the operator has not seen yet, and this benchmark samples the status every
# two seconds to time a scale-out. The CRD answers with what is stored.
MD_RESOURCE=modeldeployments.v1alpha1.worker.gpustack.ai
BINDING_KEY='modeldeployment.gpustack.ai/scaler-binding'

# The four phases of an arm: a name, a duration in seconds, and a concurrency. The baseline is
# small enough for one replica to serve, the ramp is not, the sustain holds the ramp's height,
# and the cooldown lets the count come back down.
PHASES="baseline:45:2 ramp:75:24 sustain:75:24 cooldown:90:2"
IN_TOK="${BENCH_C5_INPUT_TOKENS:-1024}"
OUT_TOK="${BENCH_C5_OUTPUT_TOKENS:-256}"
# A VLM request is counted, not timed, so the number of requests a phase carries is derived from
# its duration and a nominal per-request time.
VLM_REQUEST_SECONDS="${BENCH_C5_VLM_REQUEST_SECONDS:-10}"

MODE=parallel
ONLY_ARM=""
OUT_BASE=""
SUMMARY_ONLY=0

usage() {
  echo "usage: case-5-autoscaling.sh [OUT_DIR] [--parallel|--serial] [--arm <arm-name>] [--ns <namespace>] [--summary-only]" >&2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --parallel) MODE=parallel; shift ;;
    --serial) MODE=serial; shift ;;
    --arm) ONLY_ARM="${2:-}"; shift 2 ;;
    --ns) NS="${2:-}"; shift 2 ;;
    --summary-only) SUMMARY_ONLY=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *)
      # A positional that names a namespace is taken as the namespace, because the manifests
      # carry the benchmark's own namespace and a caller naming another one means the
      # namespace to work in. Anything else is the report directory.
      if kubectl get namespace "$1" >/dev/null 2>&1; then
        NS="$1"
      else
        OUT_BASE="$1"
      fi
      shift ;;
  esac
done

OUT_BASE="${OUT_BASE:-${REPO}/.claude/reports/$(date +%F)-benchmark/autoscaling}"
mkdir -p "$OUT_BASE"

ARMS=(vllm-pd-proportional vllm-pd-predictive vllm-pd-token-throughput sglang-pd-token-throughput vllm-epd-predictive)

log() { echo "[$(date +%H:%M:%S)] $*"; }

# The cleanup takes back everything the case deployed, on completion and on exit. The scalers go
# first, because a bound deployment cannot be deleted cleanly while its owner still holds it.
cleanup() {
  local arm
  for arm in "${ARMS[@]}"; do
    kubectl -n "$NS" delete "$SCALER_RESOURCE" "autoscale-${arm}" \
      --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  done
  kubectl -n "$NS" delete "$MD_RESOURCE" \
    autoscaling-vllm-pd autoscaling-vllm-pd-b autoscaling-sglang-pd \
    autoscaling-vllm-epd autoscaling-e2e-server \
    --ignore-not-found --wait=true --timeout=300s >/dev/null 2>&1 || true
  return 0
}
# THE CLEANUP TAKES BACK EVERYTHING THE CASE DEPLOYED, ON COMPLETION AND ON EXIT. A run that only
# rebuilds the summary deploys nothing, so it takes nothing back either.
[ "$SUMMARY_ONLY" = "1" ] || trap cleanup EXIT

# ---------------------------------------------------------------- the arms

# The deployment an arm runs on, and the manifest that carries it.
arm_deploy() {
  case "$1" in
    vllm-pd-proportional | vllm-pd-predictive) echo autoscaling-vllm-pd ;;
    vllm-pd-token-throughput) echo autoscaling-vllm-pd-b ;;
    sglang-pd-token-throughput) echo autoscaling-sglang-pd ;;
    vllm-epd-predictive) echo autoscaling-vllm-epd ;;
    *) return 1 ;;
  esac
}

arm_manifest() {
  case "$1" in
    vllm-pd-proportional | vllm-pd-predictive) echo autoscaling-vllm-pd.yaml ;;
    vllm-pd-token-throughput) echo autoscaling-vllm-pd-b.yaml ;;
    sglang-pd-token-throughput) echo autoscaling-sglang-pd.yaml ;;
    vllm-epd-predictive) echo autoscaling-vllm-epd.yaml ;;
    *) return 1 ;;
  esac
}

# Whether an arm is driven with text or with images.
arm_kind() {
  case "$1" in
    vllm-epd-predictive) echo vlm ;;
    *) echo text ;;
  esac
}

# The role bounds of an arm. Every role of every arm may double, so the arm's ceiling is the
# GPUs its slot was given.
arm_roles() {
  case "$1" in
    vllm-epd-predictive)
      local cap
      cap="$(kubectl get instancetype.worker.gpustack.ai -o jsonpath='{.items[?(@.spec.acceleratable==true)].status.accelerator.capacity}' 2>/dev/null | awk '{print $1}')"
      if [ "${cap:-8}" -lt 6 ] 2>/dev/null; then
        cat <<'ROLES'
    - name: encode
      minReplicas: 1
      maxReplicas: 1
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 1
      maxReplicas: 1
ROLES
      else
        cat <<'ROLES'
    - name: encode
      minReplicas: 1
      maxReplicas: 2
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 1
      maxReplicas: 2
ROLES
      fi
      ;;
    *)
      cat <<'ROLES'
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 1
      maxReplicas: 2
ROLES
      ;;
  esac
}

# The ratios of an arm. A prefill and a decode that hold the same number of replicas are
# coupled, and an encode role is scaled by the role it is coupled to.
arm_ratios() {
  case "$1" in
    vllm-epd-predictive)
      cat <<'RATIOS'
    - numeratorRole: prefill
      denominatorRole: decode
      target: "1"
    - numeratorRole: encode
      denominatorRole: prefill
      target: "1"
RATIOS
      ;;
    *)
      cat <<'RATIOS'
    - numeratorRole: prefill
      denominatorRole: decode
      target: "1"
RATIOS
      ;;
  esac
}

# The algorithm of an arm. The Proportional arm divides what it observed by the target of a
# role, the TokenThroughput arm compares the token rate against the rate one replica is
# expected to carry, and the Predictive arm forecasts the demand over a horizon.
arm_algorithm() {
  case "$1" in
    vllm-pd-proportional)
      cat <<'ALGO'
    type: Proportional
    proportional:
      metric: RunningRequests
      tolerance: "0.1"
      roleTargets:
        - name: prefill
          target: "3"
        - name: decode
          target: "3"
ALGO
      ;;
    vllm-pd-predictive | vllm-epd-predictive)
      cat <<'ALGO'
    type: Predictive
    predictive:
      horizonSeconds: 30
      qLevel: "0.5"
      qTrend: "0.3"
      minPoints: 2
      prefillTokenCapacityPerReplica: "2000"
      decodeTokenCapacityPerReplica: "2000"
      tokenThroughput:
        prefillTargetTokensPerSecond: "1200"
        decodeTargetTokensPerSecond: "300"
ALGO
      ;;
    vllm-pd-token-throughput | sglang-pd-token-throughput)
      cat <<'ALGO'
    type: TokenThroughput
    tokenThroughput:
      prefillTargetTokensPerSecond: "1200"
      decodeTargetTokensPerSecond: "300"
ALGO
      ;;
    *) return 1 ;;
  esac
}

scaler_manifest() { # <arm> <deploy>
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: autoscale-$1
  namespace: ${NS}
spec:
  targetRef:
    name: $2
  mode: Active
  sampling:
    intervalSeconds: 10
  roles:
$(arm_roles "$1")
  ratios:
$(arm_ratios "$1")
  algorithm:
$(arm_algorithm "$1")
YAML
}

# ---------------------------------------------------------------- the driver

# The in-cluster driver carries the load. It is created when it is missing, and the case STOPS
# when it cannot be made ready: the load runs against a cluster-internal Service name the host
# cannot resolve, so a fallback would measure the runner's network and every phase would fail on
# a name that never resolved.
ensure_driver() {
  local ready=""
  if kubectl -n "$DRIVER_NS" get pod "$DRIVER_POD" >/dev/null 2>&1; then
    kubectl -n "$DRIVER_NS" wait "pod/$DRIVER_POD" --for=condition=Ready --timeout=120s >/dev/null 2>&1 || true
    if kubectl -n "$DRIVER_NS" exec "$DRIVER_POD" -- python3 -c "import aiohttp" >/dev/null 2>&1; then
      ready=yes
    fi
  fi
  if [ -z "$ready" ]; then
    log "the driver $DRIVER_NS/$DRIVER_POD is not usable, creating it from $DRIVER_IMAGE"
    kubectl -n "$DRIVER_NS" delete pod "$DRIVER_POD" --ignore-not-found --wait=true >/dev/null 2>&1 || true
    kubectl -n "$DRIVER_NS" run "$DRIVER_POD" --image="$DRIVER_IMAGE" --restart=Never \
      --command -- sleep infinity >/dev/null 2>&1 || true
    kubectl -n "$DRIVER_NS" wait "pod/$DRIVER_POD" --for=condition=Ready --timeout=300s >/dev/null 2>&1 || true
    kubectl -n "$DRIVER_NS" exec "$DRIVER_POD" -- pip install --quiet aiohttp >/dev/null 2>&1 || true
    if kubectl -n "$DRIVER_NS" exec "$DRIVER_POD" -- python3 -c "import aiohttp" >/dev/null 2>&1; then
      ready=yes
    fi
  fi
  if [ -z "$ready" ]; then
    log "ERROR: the driver $DRIVER_NS/$DRIVER_POD could not be made ready; the load must run inside the cluster"
    return 1
  fi
  log "the driver $DRIVER_NS/$DRIVER_POD is ready"
  return 0
}

# ---------------------------------------------------------------- the cluster helpers

wait_ready() { # <deploy>
  local deploy="$1" deadline phase=""
  deadline=$((SECONDS + ${READY_TIMEOUT:-1800}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get "$MD_RESOURCE" "$deploy" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = "Ready" ] && return 0
    sleep 5
  done
  log "TIMEOUT: $deploy phase=${phase:-unknown}"
  return 1
}

deploy_arm() { # <manifest> <deploy>
  local manifest="$1" deploy="$2"
  kubectl -n "$NS" delete "$MD_RESOURCE" "$deploy" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
  apply_manifest "$manifest" || return 1
  wait_ready "$deploy" || return 1
  return 0
}

# The manifests name the benchmark's namespace, and a caller may name another one. The namespace
# in the file is rewritten on the way in, because kubectl refuses an object whose namespace
# differs from the one it was told to use.
apply_manifest() { # <manifest>
  local inst_type="${BENCH_QUAL_INSTANCE_TYPE:-bench-gpu}"
  if ! kubectl get instancetype.worker.gpustack.ai "$inst_type" >/dev/null 2>&1; then
    inst_type="$(kubectl get instancetype.worker.gpustack.ai -o jsonpath='{.items[?(@.spec.acceleratable==true)].metadata.name}' 2>/dev/null | awk '{print $1}')"
    inst_type="${inst_type:-bench-gpu}"
  fi
  sed -e "s/^  namespace: gpustack-bench$/  namespace: ${NS}/" \
      -e "s/^      instanceType: bench-gpu$/      instanceType: ${inst_type}/" \
      "${SKILL_DIR}/manifests/$1" | kubectl apply -f - >/dev/null 2>&1
}

drop_arm() { # <deploy> [scaler]
  local deploy="$1" scaler="${2:-}"
  if [ -n "$scaler" ]; then
    kubectl -n "$NS" delete "$SCALER_RESOURCE" "$scaler" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
  fi
  kubectl -n "$NS" delete "$MD_RESOURCE" "$deploy" --ignore-not-found --wait=true --timeout=300s >/dev/null 2>&1 || true
  return 0
}

# The count of a bound deployment belongs to its scaler, so the count is put back by letting the
# binding go first and patching afterwards.
#
# THE RESET WAITS FOR THE RETIREMENT TO SETTLE. A replica that is going away holds its card and
# its quota until it has left, so the next arm would otherwise start while the previous arm's
# drain was still running, and its baseline would be a cluster that is already busy.
reset_replicas() { # <deploy>
  local deploy="$1" deadline binding="" patch="" state=""
  deadline=$((SECONDS + ${BINDING_TIMEOUT:-300}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    binding="$(kubectl -n "$NS" get "$MD_RESOURCE" "$deploy" -o json 2>/dev/null \
      | jq -r --arg k "$BINDING_KEY" '.metadata.annotations[$k] // ""')"
    [ -z "$binding" ] && break
    sleep 3
  done
  patch="$(kubectl -n "$NS" get "$MD_RESOURCE" "$deploy" -o json 2>/dev/null \
    | jq -c '[.spec.roles | to_entries[] | {op: "replace", path: ("/spec/roles/" + (.key | tostring) + "/replicas"), value: 1}]')"
  if [ -n "$patch" ]; then
    kubectl -n "$NS" patch "$MD_RESOURCE" "$deploy" --type=json -p "$patch" >/dev/null 2>&1 || true
  fi
  # The reservation is cleared when the retirement settles, and the roles are ready again when
  # the replacements have come up. Both are waited for, so the next arm starts from one replica
  # per role on a quiet cluster.
  deadline=$((SECONDS + ${BINDING_TIMEOUT:-300}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    state="$(kubectl -n "$NS" get "$MD_RESOURCE" "$deploy" -o json 2>/dev/null \
      | jq -r 'if (.status.retirement // null) == null then "clear" else "retiring" end')"
    ready_all="$(kubectl -n "$NS" get "$MD_RESOURCE" "$deploy" -o json 2>/dev/null \
      | jq -r '[(.status.roles // [])[] | .ready] as $r | if ($r | length) == 0 then "0" else (($r | min) | tostring) end')"
    if [ "$state" = "clear" ] && [ "${ready_all:-0}" -ge 1 ] 2>/dev/null; then
      break
    fi
    sleep 5
  done
  return 0
}

# ---------------------------------------------------------------- the sampler

# The sampler records what the arm looked like every two seconds, so the scaling of the
# deployment can be read against the latency of the phase it happened in.
sampler_loop() { # <deploy> <scaler> <phase-file> <timeline>
  local deploy="$1" scaler="$2" phase_file="$3" timeline="$4"
  while :; do
    local md_json sc_json phase
    md_json="$(kubectl -n "$NS" get "$MD_RESOURCE" "$deploy" -o json 2>/dev/null || true)"
    sc_json="$(kubectl -n "$NS" get "$SCALER_RESOURCE" "$scaler" -o json 2>/dev/null || true)"
    phase="$(cat "$phase_file" 2>/dev/null || true)"
    jq -nc \
      --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --arg phase "$phase" \
      --argjson md "${md_json:-null}" \
      --argjson sc "${sc_json:-null}" \
      '{
        ts: $ts,
        phase: $phase,
        desired_by_role: (($md.status.roles // []) | map({key: .name, value: .desired}) | from_entries),
        ready_by_role: (($md.status.roles // []) | map({key: .name, value: .ready}) | from_entries),
        retiring_by_role: (if ($md.status.retirement // null) == null then {} else {($md.status.retirement.roleName): ($md.status.retirement.replicaOrdinal)} end),
        retirement_state: ($md.status.retirement.state // ""),
        recommended_by_role: (($sc.status.roles // []) | map({key: .name, value: .rawRecommendedReplicas}) | from_entries),
        running_requests_by_role: (($sc.status.roles // []) | map({key: .name, value: .runningRequests}) | from_entries),
        conditions: (($sc.status.conditions // []) | map({type: .type, status: .status, reason: .reason}))
      }' >> "$timeline" 2>/dev/null || true
    sleep 2
  done
}

# ---------------------------------------------------------------- the load

# One phase of an arm. The text arms are driven by bench_client.py and the image arm by
# bench_vlm.py; both run inside the driver against the cluster-internal endpoint.
run_phase() { # <arm> <deploy> <arm_dir> <phase> <duration> <concurrency>
  local arm="$1" deploy="$2" arm_dir="$3" phase="$4" dur="$5" conc="$6"
  local endpoint="http://${deploy}-router.${NS}.svc:8000"
  local out="${arm_dir}/${phase}.json"
  case "$(arm_kind "$arm")" in
    vlm)
      local reqs
      reqs=$((conc * dur / VLM_REQUEST_SECONDS))
      [ "$reqs" -lt "$conc" ] && reqs="$conc"
      python3 "${SKILL_DIR}/scripts/bench_vlm.py" run \
        --endpoint "$endpoint" \
        --model "/models" \
        --arm "$arm" \
        --suite autoscaling \
        --label "$phase" \
        --image-size 560 \
        --text-tokens "$IN_TOK" \
        --output-tokens "$OUT_TOK" \
        --concurrency "$conc" \
        --requests "$reqs" \
        --warmup 0 \
        --out "$out" > "${arm_dir}/${phase}.log" 2>&1
      ;;
    *)
      python3 "${SKILL_DIR}/scripts/bench_client.py" load \
        --endpoint "$endpoint" \
        --model "/models" \
        --arm "$arm" \
        --suite autoscaling \
        --label "$phase" \
        --input-tokens "$IN_TOK" \
        --output-tokens "$OUT_TOK" \
        --concurrency "$conc" \
        --duration "$dur" \
        --out "$out" \
        --requests-out "${arm_dir}/${phase}_reqs.jsonl" > "${arm_dir}/${phase}.log" 2>&1
      ;;
  esac
}

# ---------------------------------------------------------------- the arm

# One arm: the scaler is created, the four phases run beside the sampler, and the arm's
# summary is written.
run_arm() { # <arm> <deploy>
  local arm="$1" deploy="$2"
  local arm_dir="${OUT_BASE}/${arm}"
  local scaler="autoscale-${arm}"
  mkdir -p "$arm_dir"

  log "arm ${arm}: applying the scaler on ${deploy}"
  scaler_manifest "$arm" "$deploy" | kubectl apply -f - > "${arm_dir}/scaler-apply.log" 2>&1 || {
    log "arm ${arm}: the scaler was refused"
    return 1
  }

  local phase_file="${arm_dir}/phase"
  local timeline="${arm_dir}/timeline.jsonl"
  echo "idle" > "$phase_file"
  : > "$timeline"

  sampler_loop "$deploy" "$scaler" "$phase_file" "$timeline" &
  local sampler_pid=$!

  local spec name dur conc
  for spec in $PHASES; do
    IFS=':' read -r name dur conc <<< "$spec"
    echo "$name" > "$phase_file"
    log "arm ${arm}: phase ${name} (${dur}s, c=${conc})"
    run_phase "$arm" "$deploy" "$arm_dir" "$name" "$dur" "$conc"
  done
  echo "idle" > "$phase_file"

  kill "$sampler_pid" 2>/dev/null || true
  wait "$sampler_pid" 2>/dev/null || true

  kubectl -n "$NS" get "$SCALER_RESOURCE" "$scaler" -o json > "${arm_dir}/scaler.json" 2>/dev/null || true
  kubectl -n "$NS" get "$MD_RESOURCE" "$deploy" -o json > "${arm_dir}/modeldeployment.json" 2>/dev/null || true

  collect_failure_signature "$deploy" "$arm_dir"

  aggregate_arm "$arm" "$deploy" "$arm_dir"
  kubectl -n "$NS" delete "$SCALER_RESOURCE" "$scaler" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
  return 0
}

# THE ENGINE'S OWN ACCOUNT OF A FAILED PHASE. The table says how many requests failed, and that
# number does not say why; the engine's log does, and a reader of the report needs both to tell a
# slow arm from a broken one.
collect_failure_signature() { # <deploy> <arm_dir>
  local deploy="$1" arm_dir="$2" pod line
  : > "${arm_dir}/failure.log"
  for pod in $(kubectl -n "$NS" get pods -o name 2>/dev/null | sed 's|pod/||' | grep "^${deploy}-"); do
    line="$(kubectl -n "$NS" logs "$pod" --all-containers=true --tail=4000 2>/dev/null \
      | grep -m 1 -i -E "Failed to get kvcache|No available prefill|local port range|Cannot assign requested address" || true)"
    [ -n "$line" ] && printf '%s\n' "$line" >> "${arm_dir}/failure.log"
  done
  return 0
}

# The summary of an arm: what each phase answered, and what the timeline saw.
aggregate_arm() { # <arm> <deploy> <arm_dir>
  local arm="$1" deploy="$2" arm_dir="$3"
  local phases="" name metrics
  for spec in $PHASES; do
    name="${spec%%:*}"
    if [ -s "${arm_dir}/${name}.json" ]; then
      # THE METRICS OF THE PHASE TRAVEL WITH IT, so the summary table and the comparison document
      # read the same numbers and neither has to reach for the per-request record again.
      metrics="$(phase_metrics "$arm_dir" "$name")"
      [ -n "$metrics" ] || metrics='{}'
      phases="${phases}$(jq -c --arg phase "$name" --argjson metrics "$metrics" \
        '{phase: $phase} + . + $metrics' "${arm_dir}/${name}.json" 2>/dev/null || echo "{\"phase\":\"${name}\"}")
"
    fi
  done
  # THE ENGINE'S ACCOUNT OF A FAILURE TRAVELS WITH THE ARM, so the report can say why a phase
  # failed instead of only how many requests it lost.
  [ -f "${arm_dir}/failure.log" ] || : > "${arm_dir}/failure.log"
  jq -n \
    --arg arm "$arm" \
    --arg deploy "$deploy" \
    --argjson phases "$(printf '%s' "$phases" | jq -s '.' 2>/dev/null || echo '[]')" \
    --slurpfile timeline "${arm_dir}/timeline.jsonl" \
    --rawfile failure "${arm_dir}/failure.log" \
    '{
      arm: $arm,
      deploy: $deploy,
      phases: $phases,
      engine_failure: ($failure | split("\n") | map(select(. != "")) | .[0] // ""),
      timeline: {
        samples: ($timeline | length),
        desired_max_by_role: (reduce $timeline[] as $s ({}; reduce ($s.desired_by_role // {} | to_entries[]) as $e (.;
          if (.[$e.key] // 0) < $e.value then .[$e.key] = $e.value else . end))),
        ready_max_by_role: (reduce $timeline[] as $s ({}; reduce ($s.ready_by_role // {} | to_entries[]) as $e (.;
          if (.[$e.key] // 0) < $e.value then .[$e.key] = $e.value else . end))),
        retirement_states: ($timeline | map(.retirement_state // "" | select(. != "")) | unique),
        scaled: (($timeline | map(.desired_by_role // {} | to_entries[] | .value) | max) // 0) > 1
      }
    }' > "${arm_dir}/summary.json" 2>/dev/null || true
  return 0
}

# ---------------------------------------------------------------- the slots

# The slot that runs the two text arms of wave 1 on the vLLM prefill/decode pair.
wave1_slot_a() {
  local deploy=autoscaling-vllm-pd
  deploy_arm autoscaling-vllm-pd.yaml "$deploy" || return 1
  run_arm vllm-pd-proportional "$deploy" || true
  log "slot A: putting the count back to one replica per role"
  reset_replicas "$deploy"
  run_arm vllm-pd-predictive "$deploy" || true
  drop_arm "$deploy"
  return 0
}

# The slot that runs the SGLang arm and then the second vLLM arm of wave 1.
wave1_slot_b() {
  local deploy=autoscaling-sglang-pd
  deploy_arm autoscaling-sglang-pd.yaml "$deploy" || return 1
  run_arm sglang-pd-token-throughput "$deploy" || true
  drop_arm "$deploy"
  deploy=autoscaling-vllm-pd-b
  deploy_arm autoscaling-vllm-pd-b.yaml "$deploy" || return 1
  run_arm vllm-pd-token-throughput "$deploy" || true
  drop_arm "$deploy"
  return 0
}

# The slot that runs the image arm of wave 2.
wave2_slot_a() {
  local deploy=autoscaling-vllm-epd
  deploy_arm autoscaling-vllm-epd.yaml "$deploy" || return 1
  run_arm vllm-epd-predictive "$deploy" || true
  drop_arm "$deploy"
  return 0
}

# The slot that runs the live-traffic E2E case of wave 2 against a single server.
#
# CASE 128 DOES NOT RUN HERE. It builds deployments of its own that request up to six GPUs, and
# slot A of this wave holds six, so running both at once would queue case-128's Pods behind the
# EPD arm's and pollute the arm's timeline with a count that is not ready. Case 128 runs once,
# before wave 1, where the cluster is free.
wave2_slot_b() {
  local deploy=autoscaling-e2e-server
  local rc=0
  deploy_arm autoscaling-e2e-server.yaml "$deploy" || return 1
  bash "${REPO}/.agents/skills/gpustack-operator-e2e/cases/case-129.sh" "$NS" "$deploy" server \
    > >(tee "${OUT_BASE}/e2e-case-129.log") 2>&1 || rc=$?
  echo "$rc" > "${OUT_BASE}/e2e-case-129.rc"
  drop_arm "$deploy"
  return 0
}

# The admission case, run once before the waves so that the GPUs it asks for are not contending
# with an arm's.
run_case_128() {
  local rc=0
  bash "${REPO}/.agents/skills/gpustack-operator-e2e/cases/case-128.sh" "$NS" \
    > >(tee "${OUT_BASE}/e2e-case-128.log") 2>&1 || rc=$?
  echo "$rc" > "${OUT_BASE}/e2e-case-128.rc"
  log "case-128 exited with ${rc}"
  return 0
}

# ---------------------------------------------------------------- the report

# THE SECONDS BETWEEN THE LOAD RAMPING UP AND THE COUNT RISING, or empty. The timeline records
# when the deployment wanted what, and the phase record says when the load asked for more, so the
# difference is how long the scale-out took to answer the load.
#
# THE TIMELINE IS A RECORD PER SAMPLE, so it is read with jq's slurp; without it every line is a
# document of its own and the filter below cannot see the samples as one list.
scale_out_lead() { # <arm_dir>
  local timeline="${1}/timeline.jsonl"
  [ -s "$timeline" ] || { echo ""; return 0; }
  jq -s -r '
    [ .[] | select(.phase == "ramp") | .ts ] as $ramp
    | [ .[] | select(((.desired_by_role // {}) | to_entries | map(.value) | max) > 1) | .ts ] as $up
    | if ($ramp | length) == 0 or ($up | length) == 0 then ""
      else (($up[0] | fromdateiso8601) - ($ramp[0] | fromdateiso8601)) end
  ' "$timeline" 2>/dev/null | head -1
}

# WHETHER THE COUNT WAS ALREADY ABOVE ONE REPLICA WHEN THE RAMP BEGAN. An arm that scaled during
# its baseline answered the ramp before the ramp asked, so its lead is zero and this says so.
scaled_before_ramp() { # <arm_dir>
  local timeline="${1}/timeline.jsonl"
  [ -s "$timeline" ] || { echo ""; return 0; }
  jq -s -r '
    [ .[] | select(.phase == "ramp") | .ts ] as $ramp
    | [ .[] | select(((.desired_by_role // {}) | to_entries | map(.value) | max) > 1) | .ts ] as $up
    | if ($ramp | length) == 0 or ($up | length) == 0 then ""
      else (($up[0] | fromdateiso8601) < ($ramp[0] | fromdateiso8601)) end
  ' "$timeline" 2>/dev/null | head -1
}

# THE METRICS OF A PHASE. The continuous load driver records every request but summarises none of
# them, so the latencies of a text arm are computed here from the per-request record it left
# behind. A phase whose own driver summarised it already carries them.
#
# THE PERCENTILE IS THE NEAREST RANK THE LOAD DRIVER USES, so a number read here and a number read
# from the driver's own summary are the same number. That driver rounds a half to the even
# neighbour, which jq's round does not, so the rounding is spelled out here.
phase_metrics() { # <arm_dir> <phase>
  local arm_dir="$1" phase="$2"
  local json="${arm_dir}/${phase}.json"
  [ -s "$json" ] || { echo '{}'; return 0; }
  local reqs="${arm_dir}/${phase}_reqs.jsonl"
  if [ -s "$reqs" ]; then
    jq -cs --argjson wall "$(jq -c '.wall_seconds // 0' "$json" 2>/dev/null || echo 0)" '
      def pyround: . as $x | ($x | floor) as $f | ($x - $f) as $frac
        | if $frac > 0.5 then $f + 1 elif $frac < 0.5 then $f
          elif ($f % 2) == 0 then $f else $f + 1 end;
      def pct($v; $p):
        ($v | sort) as $s | ($s | length) as $n
        | if $n == 0 then null
          else ($s[(( (($p / 100) * $n + 0.5) | pyround | if . < 1 then 1 elif . > $n then $n else . end) - 1)])
          end;
      def r3: if . == null then null else ((. * 1000 | round) / 1000) end;
      [ .[] | select(.error) ] as $bad
      | [ .[] | select(.error | not) ] as $good
      | [ $good[] | (.itl_ms // [])[] ] as $itl
      | [ $good[] | .ttft_ms | select(. != null) ] as $ttft
      | ([ $good[] | (.output_tokens // 0) ] | add // 0) as $out
      | { requests: length, errors: ($bad | length),
          ttft_p50_ms: (pct($ttft; 50) | r3), ttft_p99_ms: (pct($ttft; 99) | r3),
          itl_p50_ms: (pct($itl; 50) | r3), itl_p99_ms: (pct($itl; 99) | r3),
          output_tok_per_s: (if $wall > 0 then ((($out / $wall) * 1000 | round) / 1000) else null end) }
    ' "$reqs" 2>/dev/null || echo '{}'
  else
    jq -c '{
      requests: (.completed // .requests // 0),
      errors: (.errors // 0),
      ttft_p50_ms: (.ttft_ms.p50 // null), ttft_p99_ms: (.ttft_ms.p99 // null),
      itl_p50_ms: (.itl_ms.p50 // null), itl_p99_ms: (.itl_ms.p99 // null),
      output_tok_per_s: (.output_tok_per_s // null)
    }' "$json" 2>/dev/null || echo '{}'
  fi
}

# THE SUMMARY IS ONE ROW PER ARM AND PHASE, because a reader asks about one phase at a time: the
# latency the ramp carried is not the latency the cooldown carried, and the errors of the whole
# arm are not the errors of the phase that carried them.
write_summary() {
  local arm name spec arm_dir deploy lead early desired_max scaled states
  {
    printf 'arm\tdeploy\tphase\trequests\terrors\tttft_p50_ms\tttft_p99_ms\titl_p50_ms\titl_p99_ms\toutput_tok_per_s\tscale_out_lead_s\tscaled_before_ramp\tdesired_max\tscaled\tretirement_states\n'
    for arm in "${ARMS[@]}"; do
      arm_dir="${OUT_BASE}/${arm}"
      [ -s "${arm_dir}/summary.json" ] || continue
      deploy="$(jq -r '.deploy // ""' "${arm_dir}/summary.json" 2>/dev/null)"
      lead="$(scale_out_lead "$arm_dir")"
      early="$(scaled_before_ramp "$arm_dir")"
      # An arm that scaled before the ramp began answered the ramp early, so its lead is zero.
      [ "$early" = "true" ] && lead=0
      desired_max="$(jq -r '[.timeline.desired_max_by_role // {} | to_entries[].value] | max // 0' "${arm_dir}/summary.json" 2>/dev/null)"
      scaled="$(jq -r '.timeline.scaled // false' "${arm_dir}/summary.json" 2>/dev/null)"
      states="$(jq -r '(.timeline.retirement_states // []) | join(",")' "${arm_dir}/summary.json" 2>/dev/null)"
      # THE PHASE ROWS COME FROM THE ARM SUMMARY, which carries the metrics of every phase beside
      # the phase itself, so the table and the comparison document cannot disagree.
      jq -r --arg arm "$arm" --arg deploy "$deploy" --arg lead "$lead" --arg early "$early" \
        --arg desired_max "$desired_max" --arg scaled "$scaled" --arg states "$states" '
        .phases[] | [ $arm, $deploy, .phase, (.requests // 0), (.errors // 0),
          (.ttft_p50_ms // ""), (.ttft_p99_ms // ""), (.itl_p50_ms // ""), (.itl_p99_ms // ""),
          (.output_tok_per_s // ""), $lead, $early, $desired_max, $scaled, $states ] | @tsv
      ' "${arm_dir}/summary.json" >> "${OUT_BASE}/summary.tsv" 2>/dev/null || true
    done
  } > "${OUT_BASE}/summary.tsv" 2>/dev/null || true
}

# THE MARKDOWN SUMMARY is the same table, with the two exit codes of the E2E cases the run drove
# above it, because a summary that cannot see a broken case is not a summary of the run.
write_summary_md() {
  {
    echo "# Autoscaling benchmark: legacy topology"
    echo
    echo "Generated at $(date -u +%Y-%m-%dT%H:%M:%SZ), mode ${MODE}."
    echo
    echo "The E2E cases this run drove: case-128 exited ${case128_rc}, case-129 exited ${case129_rc}."
    echo
    echo "| arm | deploy | phase | requests | errors | TTFT p50 (ms) | TTFT p99 (ms) | ITL p50 (ms) | ITL p99 (ms) | output tok/s | scale-out lead (s) | scaled before ramp | desired max | scaled | retirement states |"
    echo "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |"
    awk -F'\t' 'NR>1 { printf "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15 }' "${OUT_BASE}/summary.tsv" 2>/dev/null
    echo
    echo "The arms are measured in four phases: a baseline small enough for one replica, a ramp that"
    echo "outruns it, a sustain at the ramp's height and a cooldown that lets the count come down."
    echo "The scale-out lead is the seconds between the ramp starting and the count rising. An arm"
    echo "whose count was already above one replica when the ramp began answered the ramp early, so"
    echo "its lead is zero and the column beside it says so."
    local arm
    for arm in "${ARMS[@]}"; do
      arm_dir="${OUT_BASE}/${arm}"
      [ -s "${arm_dir}/summary.json" ] || continue
      local failed signature
      failed="$(jq -r '[.phases[] | select((.errors // 0) > 0) | .phase] | join(", ")' "${arm_dir}/summary.json" 2>/dev/null)"
      signature="$(jq -r '.engine_failure // ""' "${arm_dir}/summary.json" 2>/dev/null)"
      [ -n "$failed" ] || continue
      echo
      echo "The ${arm} arm lost requests in the phases ${failed}."
      [ -n "$signature" ] && echo "The engine said: ${signature}"
    done
  } > "${OUT_BASE}/summary.md" 2>/dev/null || true
}

# ---------------------------------------------------------------- the main

if [ "$SUMMARY_ONLY" = "1" ]; then
  # THE SUMMARY IS REBUILT FROM THE ARTIFACTS A RUN ALREADY LEFT BEHIND, so a report can be
  # regenerated without spending the cluster time again. The arms are read from the files the run
  # wrote, not from the cluster, so nothing is deployed and nothing is torn down.
  log "rebuilding the summary of ${OUT_BASE} from the artifacts it already holds"
  for arm in "${ARMS[@]}"; do
    arm_dir="${OUT_BASE}/${arm}"
    [ -d "$arm_dir" ] || continue
    aggregate_arm "$arm" "$(arm_deploy "$arm" 2>/dev/null || echo "")" "$arm_dir"
  done
elif [ -n "$ONLY_ARM" ]; then
  ensure_driver || exit 1
  deploy="$(arm_deploy "$ONLY_ARM" 2>/dev/null || true)"
  manifest="$(arm_manifest "$ONLY_ARM" 2>/dev/null || true)"
  if [ -z "$deploy" ] || [ -z "$manifest" ]; then
    echo "unknown arm '${ONLY_ARM}'; the arms are ${ARMS[*]}" >&2
    usage
    exit 2
  fi
  log "running only the arm ${ONLY_ARM} on ${deploy}"
  deploy_arm "$manifest" "$deploy"
  run_arm "$ONLY_ARM" "$deploy"
  drop_arm "$deploy" "autoscale-${ONLY_ARM}"
else
  ensure_driver || exit 1
  # THE ADMISSION CASE RUNS FIRST, on a cluster with nothing else on it. Its own targets ask for
  # up to six GPUs, and a wave asks for eight, so the two must not overlap.
  run_case_128

  if [ "$MODE" = "serial" ]; then
    log "wave 1 (serial)"
    wave1_slot_a
    wave1_slot_b
    log "wave 2 (serial)"
    wave2_slot_a
    wave2_slot_b
  else
    log "wave 1 (parallel)"
    wave1_slot_a &
    slot_a_pid=$!
    wave1_slot_b &
    slot_b_pid=$!
    wait "$slot_a_pid"
    wait "$slot_b_pid"

    log "wave 2 (parallel)"
    wave2_slot_a &
    slot_a_pid=$!
    wave2_slot_b &
    slot_b_pid=$!
    wait "$slot_a_pid"
    wait "$slot_b_pid"
  fi
fi

# The exit code of each E2E case the run drove, so a benchmark summary cannot look healthy over
# a broken case.
case128_rc="$(cat "${OUT_BASE}/e2e-case-128.rc" 2>/dev/null || echo null)"
case129_rc="$(cat "${OUT_BASE}/e2e-case-129.rc" 2>/dev/null || echo null)"

# The comparison is every arm's summary, in one document.
jq -n \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg mode "$MODE" \
  --argjson case128 "$case128_rc" \
  --argjson case129 "$case129_rc" \
  --argjson arms "$(for arm in "${ARMS[@]}"; do
    if [ -s "${OUT_BASE}/${arm}/summary.json" ]; then
      jq -c --arg arm "$arm" '{key: $arm, value: .}' "${OUT_BASE}/${arm}/summary.json" 2>/dev/null
    fi
  done | jq -s 'from_entries' 2>/dev/null || echo '{}')" \
  '{comparisonKind: "legacy-topology", generated_at: $generated_at, mode: $mode, e2e: {case128: $case128, case129: $case129}, arms: $arms}' \
  > "${OUT_BASE}/comparison.json"

# The summary a reader opens first: one row per arm and phase, and the exit code of each E2E
# case the run drove.
write_summary
write_summary_md

kubectl get devices -o json > "${OUT_BASE}/released-devices.json" 2>/dev/null || true
log "CASE 5 (autoscaling) benchmark complete: ${OUT_BASE}"
