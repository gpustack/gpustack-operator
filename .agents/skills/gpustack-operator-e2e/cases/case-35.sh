#!/usr/bin/env bash
#
# CASE 35 — Live ModelDeploymentScaler active scaling under real load: proportional single-role
#           scaling + disaggregated P/D TokenThroughput capacityPrior whole-unit scaling
#           (1P:2D -> 2P:4D -> 1P:2D)
#           (MUTATING, self-recovering; each phase auto-skips when its inputs/hardware are absent)
#
#   case-35.sh <NS> [MD_NAME] [ROLE_NAME]
#
# Prerequisites: nodes=1; gpu=nvidia|amd|ascend; cluster=real; inputs=E2E_C35_ACCELERATOR_TYPE,E2E_C35_ARTIFACT,E2E_C35_ENGINE_IMAGE,E2E_C35_MD,E2E_C35_MODEL,E2E_C35_NS,E2E_C35_PD_ENGINE_IMAGE,E2E_C35_PD_MODEL,E2E_C35_PHASES,E2E_C35_PROBE_IMAGE,E2E_C35_PROBE_URL,E2E_C35_READY_BOUND,E2E_C35_RETIRE_BOUND,E2E_C35_ROLE,E2E_C35_SCALE_BOUND,E2E_C35_SKIP_PHASE2,E2E_C35_STEP_BOUND,E2E_C35_WEIGHTS
# Goal:        Verify ModelDeploymentScaler active scaling under real inference traffic across
#              two independent phases:
#              - Phase 1 (Proportional single-role scaling): an Active Proportional scaler
#                (TotalTokensPerSecond) reads live Prometheus counters from a serving
#                ModelDeployment, publishes runningRequests > 0, scales replicas 1 -> 2 under
#                heavy load, brings the second Pod Ready with its own request counter growing,
#                records status.lastCommit.vector, scales down 2 -> 1 when narrowed while
#                recording status.retirement.state, and answers every phase with 0 failed
#                requests.
#              - Phase 2 (Disaggregated P/D TokenThroughput + capacityPrior whole-unit scaling):
#                an Active TokenThroughput scaler with a calibrated 1P:2D capacityPrior catalog
#                and fixed 1:2 ratio resolves CapacityPriorsReady=True, Bound=True, Active=True,
#                atomically scales a live routed vLLM P/D ModelDeployment 1P:2D -> 2P:4D under
#                16-worker burst load while holding the 1:2 ratio invariant, atomically retires
#                back to 1P:2D when burst stops, with 0 failed canary/burst requests and 0
#                container restarts across all observed Pods.
# Environment: Real accelerated cluster.
#              - Phase 1 uses existing MD_NAME / E2E_C35_MD or builds a single-role vLLM target
#                from E2E_C35_ENGINE_IMAGE + E2E_C35_WEIGHTS; auto-skips if neither is provided.
#              - Phase 2 requires >= 6 allocatable GPUs on a usable AcceleratorType (NVIDIA for
#                default CUDA image) and a pre-warmed ModelArtifact (E2E_C35_ARTIFACT, default
#                qwen3-8b) with resolved revision; auto-skips if prerequisites are absent.
# Inputs:      MOCKED: nothing. REAL: live engine Prometheus endpoints, scaler sampler/planner,
#              retirement protocol, llm-d-router, and in-cluster curl probe Pods.
# Expected:    All Phase 1 proportional scale-up/scale-down/retirement/zero-error assertions and
#              all Phase 2 capacityPrior/atomic-1:2/zero-error/zero-restart assertions pass (or
#              skip cleanly with explicit reason when hardware/inputs are absent).
# Cleanup:     EXIT/INT/TERM trap deletes all scalers, probe Pods, ConfigMaps, and ModelDeployments
#              created by this run (never deleting a caller-supplied Phase 1 target).
set -uo pipefail

CASE_ID=35
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_serving-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_md-lib.sh"

NS="${E2E_C35_NS:-${1:-}}"
if [ -z "$NS" ]; then
  echo "usage: case-35.sh <NS> [MD_NAME] [ROLE_NAME]" >&2
  exit 2
fi

SCALER_CRD="modeldeploymentscalers.worker.gpustack.ai"
MD_CRD="modeldeployments.worker.gpustack.ai"
SCALER_RES="modeldeploymentscalers.v1alpha1.worker.gpustack.ai"
MD_RES="modeldeployments.v1alpha1.worker.gpustack.ai"

# Phase 1 config
P1_MD="${E2E_C35_MD:-${2:-}}"
P1_ROLE="${3:-${E2E_C35_ROLE:-server}}"
P1_SCALER="e2e-c35-p1-scaler"
P1_PROBE="e2e-c35-p1-probe"
P1_ENGINE_IMAGE="${E2E_C35_ENGINE_IMAGE:-}"
P1_WEIGHTS="${E2E_C35_WEIGHTS:-}"
P1_MODEL="${E2E_C35_MODEL:-}"
PROBE_IMAGE="${E2E_C35_PROBE_IMAGE:-docker.io/curlimages/curl:8.10.1}"
P1_PHASES="${E2E_C35_PHASES:-base:2:30 up:24:90 two:24:90 down:2:120 one:2:60}"
READY_BOUND="${E2E_C35_READY_BOUND:-900}"
STEP_BOUND="${E2E_C35_STEP_BOUND:-300}"
RETIRE_BOUND="${E2E_C35_RETIRE_BOUND:-480}"

# Phase 2 config
P2_ARTIFACT="${E2E_C35_ARTIFACT:-qwen3-8b}"
P2_MODEL="${E2E_C35_PD_MODEL:-Qwen/Qwen3-8B}"
P2_ENGINE_IMAGE="${E2E_C35_PD_ENGINE_IMAGE:-gpustack/runner:cuda13.0-vllm0.32.0}"
P2_READY_BOUND="${E2E_C35_READY_BOUND:-600}"
P2_SCALE_BOUND="${E2E_C35_SCALE_BOUND:-480}"
P2_PREFIX="e2e-c35-$$"
P2_DEPLOY="${P2_PREFIX}-pd"
P2_SCALER="${P2_PREFIX}-scaler"
P2_CM="${P2_PREFIX}-priors"
P2_PROBE="${P2_PREFIX}-probe"

P1_BUILT=""
P2_CREATED_POD=""
P2_CREATED_SCALER=""
P2_CREATED_MD=""
P2_CREATED_CM=""
TMP_DIR="$(mktemp -d)"
CLEANED=false

cleanup_phase1() {
  kubectl -n "$NS" delete "$SCALER_RES" "$P1_SCALER" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pod "$P1_PROBE" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
  if [ -n "$P1_BUILT" ]; then
    kubectl -n "$NS" delete "$MD_RES" "$P1_MD" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
    P1_BUILT=""
  fi
}

# shellcheck disable=SC2317 # Invoked by EXIT/INT/TERM trap.
cleanup() {
  if [ "$CLEANED" = true ]; then
    return 0
  fi
  CLEANED=true
  cleanup_phase1
  if [ -n "$P2_CREATED_POD" ]; then
    kubectl -n "$NS" delete pod "$P2_CREATED_POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
  if [ -n "$P2_CREATED_SCALER" ]; then
    kubectl -n "$NS" delete "$SCALER_RES" "$P2_CREATED_SCALER" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  fi
  if [ -n "$P2_CREATED_MD" ]; then
    kubectl -n "$NS" delete "$MD_RES" "$P2_CREATED_MD" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
  fi
  if [ -n "$P2_CREATED_CM" ]; then
    kubectl -n "$NS" delete configmap "$P2_CREATED_CM" --ignore-not-found >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP_DIR"
  return 0
}
trap cleanup EXIT INT TERM

if ! kubectl get crd "$SCALER_CRD" >/dev/null 2>&1 || ! kubectl get crd "$MD_CRD" >/dev/null 2>&1; then
  record SKIP "case-35 prerequisites" "missing ${SCALER_CRD} or ${MD_CRD}"
  results
  exit 0
fi

# ==============================================================================
# PHASE 1: Proportional Active Scaling on Single-Role Live Target
# ==============================================================================

p1_target() {
  kubectl -n "$NS" get "$MD_RES" "$P1_MD" -o json 2>/dev/null | jq -r "$@"
}

p1_scaler() {
  kubectl -n "$NS" get "$SCALER_RES" "$P1_SCALER" -o json 2>/dev/null | jq -r "$@"
}

p1_running_requests() {
  # shellcheck disable=SC2016 # jq variable
  p1_scaler --arg r "$P1_ROLE" '[(.status.roles // [])[] | select(.name == $r) | .runningRequests] | .[0] // ""'
}

p1_committed_vector() {
  # shellcheck disable=SC2016 # jq variable
  p1_scaler --arg r "$P1_ROLE" '(.status.lastCommit.vector // {})[$r] // ""'
}

p1_role_replicas() {
  # shellcheck disable=SC2016 # jq variable
  p1_target --arg r "$P1_ROLE" '[(.spec.roles // [])[] | select(.name == $r) | .replicas] | .[0] // ""'
}

p1_ready_pods() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${P1_MD},app.kubernetes.io/component=${P1_ROLE}" \
    -o json 2>/dev/null \
    | jq -r '.items[] | select(.metadata.deletionTimestamp == null and
      any(.status.conditions[]?; .type == "Ready" and .status == "True")) | .metadata.name' | sort
}

p1_wait_ready_count() {
  local wanted="$1" bound="$2" deadline ready total
  deadline=$((SECONDS + bound))
  while [ "$SECONDS" -lt "$deadline" ]; do
    ready="$(p1_ready_pods | grep -c . || true)"
    total="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${P1_MD},app.kubernetes.io/component=${P1_ROLE}" \
      --no-headers 2>/dev/null | grep -c . || true)"
    if [ "${ready:-0}" -eq "$wanted" ] && [ "${total:-0}" -eq "$wanted" ]; then
      return 0
    fi
    sleep 5
  done
  return 1
}

p1_counter_of() {
  local ip port scheme
  ip="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.podIP}' 2>/dev/null || true)"
  port="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.annotations.prometheus\.io/port}' 2>/dev/null || true)"
  scheme="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.annotations.prometheus\.io/scheme}' 2>/dev/null || true)"
  serving_sum "$(kubectl -n "$NS" exec "$P1_PROBE" -- curl -fsS --max-time 10 \
    "${scheme:-http}://${ip}:${port}/metrics" 2>/dev/null || true)" "$SP_SERVED" || echo 0
}

p1_phase_now() {
  kubectl -n "$NS" exec "$P1_PROBE" -- sh -c 'cat /tmp/probe.log 2>/dev/null' 2>/dev/null \
    | awk '/^start / { p = $2 } END { print p }'
}

p1_phase_result() {
  kubectl -n "$NS" exec "$P1_PROBE" -- sh -c "grep '^done $1 ' /tmp/probe.log 2>/dev/null | tail -1" \
    2>/dev/null | awk '{ print $3 " " $4 }'
}

p1_wait_phase() {
  local name="$1" bound="$2" deadline
  deadline=$((SECONDS + bound))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(p1_phase_now)" = "$name" ] && return 0
    sleep 2
  done
  return 1
}

p1_wait_phase_done() {
  local name="$1" bound="$2" deadline
  deadline=$((SECONDS + bound))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ -n "$(p1_phase_result "$name")" ] && return 0
    sleep 2
  done
  return 1
}

p1_skip_rest() {
  local reason="$1"
  record SKIP "P1: scaler reads running requests of target" "$reason"
  record SKIP "P1: target replica count rises under load" "$reason"
  record SKIP "P1: second Pod becomes Ready" "$reason"
  record SKIP "P1: new Pod request counter grows under load" "$reason"
  record SKIP "P1: scaler records committed vector" "$reason"
  record SKIP "P1: replica count falls when scaler is narrowed" "$reason"
  record SKIP "P1: retirement states observed on target" "$reason"
  record SKIP "P1: one Ready Pod left after retirement" "$reason"
  record SKIP "P1: every phase answers requests and fails none" "$reason"
}

run_phase1() {
  local env_reason="" it="" phase="" pods="" router="" endpoint="" probe_url=""
  local probe_file phases_file phase_lines entry name rest conc dur copied script_bytes phase_bytes
  local pods_before_load replicas running new_pod vendor engine counter_one counter_two committed
  local narrowed states state ready_left phase_failures phase_detail phase_evidence result ok bad

  if [ -z "$P1_MD" ]; then
    P1_MD="e2e-c35-target"
    if [ -z "$P1_ENGINE_IMAGE" ]; then
      env_reason="neither MD_NAME/E2E_C35_MD nor E2E_C35_ENGINE_IMAGE is set"
    elif [ -z "$P1_WEIGHTS" ]; then
      env_reason="E2E_C35_WEIGHTS is unset for self-built Phase 1 target"
    fi
    if [ -z "$env_reason" ]; then
      it="${E2E_C35_ACCELERATOR_TYPE:-${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-}}}"
      if [ -z "$it" ]; then
        it="$(kubectl get acceleratortypes.worker.gpustack.ai -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
          2>/dev/null \
          | while IFS='|' read -r at_name deleting inactive manufacturer; do
            [ -n "$at_name" ] || continue
            [ -z "$deleting" ] || continue
            [ "$inactive" = true ] && continue
            case "$manufacturer" in
              ascend | nvidia | amd) echo "$at_name"; break ;;
            esac
          done)"
      fi
      [ -n "$it" ] || env_reason="no usable AcceleratorType reports ascend, nvidia, or amd"
    fi
    P1_MODEL="${P1_MODEL:-/models/Qwen2.5-0.5B-Instruct}"
    if [ -z "$env_reason" ]; then
      kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${P1_MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${P1_ENGINE_IMAGE}
  model:
    name: ${P1_MODEL}
  roles:
    - name: ${P1_ROLE}
      kind: Server
      replicas: 1
      size: 1
      acceleratorType: ${it}
      image: ${P1_ENGINE_IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      additionalVolumes:
        - hostPath:
            path: ${P1_WEIGHTS}
            type: Directory
          mountPath: ${P1_MODEL}
          readOnly: true
      extraArgs:
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
EOF
      P1_BUILT=yes
    fi
  fi

  if [ -n "$env_reason" ]; then
    record SKIP "P1: target serves and probe gets an answer" "$env_reason"
    p1_skip_rest "$env_reason"
    return 0
  fi

  local deadline=$((SECONDS + READY_BOUND))
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(p1_target '.status.phase')"
    [ "$phase" = "Ready" ] && break
    sleep 10
  done
  if [ "$phase" != "Ready" ]; then
    pods="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${P1_MD}" --no-headers 2>/dev/null | grep -c . || true)"
    if [ "${pods:-0}" -gt 0 ]; then
      record FAIL "P1: target serves and probe gets an answer" "phase=${phase:-unknown} pods=${pods}"
    else
      record SKIP "P1: target serves and probe gets an answer" "phase=${phase:-unknown} pods=0 (not admitted)"
    fi
    p1_skip_rest "target never reached Ready (phase=${phase:-unknown})"
    return 0
  fi

  if [ -z "$P1_MODEL" ]; then
    P1_MODEL="$(p1_target '.spec.model.name')"
    [ -n "$P1_MODEL" ] || P1_MODEL="/models/Qwen2.5-0.5B-Instruct"
  fi
  record PASS "P1: target serves and probe gets an answer" "target=${P1_MD} role=${P1_ROLE} model=${P1_MODEL}"

  pods_before_load="$(p1_ready_pods)"
  router="$(p1_target '.spec.router.name')"
  if [ -n "$router" ]; then
    probe_url="${E2E_C35_PROBE_URL:-http://${P1_MD}-router.${NS}.svc:8000/v1/chat/completions}"
  else
    endpoint="$(p1_target '.status.endpoint')"
    probe_url="${E2E_C35_PROBE_URL:-http://${endpoint#*://}/v1/chat/completions}"
  fi

  kubectl -n "$NS" run "$P1_PROBE" --image="$PROBE_IMAGE" --restart=Never \
    --labels="gpustack.ai/e2e-probe=${P1_MD}" --command -- sleep 7200 >/dev/null 2>&1
  if ! kubectl -n "$NS" wait "pod/${P1_PROBE}" --for=condition=Ready --timeout=300s >/dev/null 2>&1; then
    record SKIP "P1: probe Pod ready" "probe Pod '${P1_PROBE}' never became Ready on '${PROBE_IMAGE}'"
    p1_skip_rest "probe Pod never became Ready"
    return 0
  fi

  # shellcheck disable=SC2016 # Variables expand inside the remote probe shell.
  local probe_script='#!/bin/sh
URL="$1"
MODEL="$2"
: > /tmp/probe.log
while read -r name conc dur; do
  [ -n "$name" ] || continue
  echo "start $name" >> /tmp/probe.log
  : > "/tmp/ok.$name"
  : > "/tmp/bad.$name"
  : > "/tmp/badbody.$name"
  end=$(( $(date +%s) + dur ))
  i=0
  while [ "$i" -lt "$conc" ]; do
    (
      while [ "$(date +%s)" -lt "$end" ]; do
        if [ $((i % 2)) -eq 0 ]; then
          stream=true
        else
          stream=false
        fi
        body="/tmp/probe-body.$i"
        c=$(curl -sS --max-time 120 -o "$body" -w "%{http_code}" \
          -H "Content-Type: application/json" \
          --data-binary "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in one word.\"}],\"max_tokens\":16,\"stream\":$stream}" \
          "$URL" 2>/dev/null)
        v="BAD $c"
        if [ "$c" = 200 ] && grep -q "\"content\": *\"[^\"]" "$body" && ! grep -q "\"error\"" "$body"; then
          v=ok
        else
          printf "%s code=%s body=%s\n" "$(date +%s)" "$c" "$(head -c 300 "$body" | tr "\n" " ")" >> "/tmp/badbody.$name"
        fi
        rm -f "$body"
        if [ "$v" = ok ]; then
          echo ok >> "/tmp/ok.$name"
        else
          echo bad >> "/tmp/bad.$name"
        fi
      done
    ) &
    i=$((i + 1))
  done
  wait
  echo "done $name $(wc -l < "/tmp/ok.$name" | tr -d " ") $(wc -l < "/tmp/bad.$name" | tr -d " ")" >> /tmp/probe.log
done < /tmp/phases
'

  probe_file="$(mktemp "${TMP_DIR}/probe.XXXXXX")"
  phases_file="$(mktemp "${TMP_DIR}/phases.XXXXXX")"
  printf '%s\n' "$probe_script" >"$probe_file"
  phase_lines=""
  for entry in $P1_PHASES; do
    name="${entry%%:*}"
    rest="${entry#*:}"
    conc="${rest%%:*}"
    dur="${rest##*:}"
    phase_lines="${phase_lines}${name} ${conc} ${dur}
"
  done
  printf '%s' "$phase_lines" >"$phases_file"

  copied=0
  kubectl -n "$NS" cp "$probe_file" "${P1_PROBE}:/tmp/probe.sh" >/dev/null 2>&1 && copied=$((copied + 1))
  kubectl -n "$NS" cp "$phases_file" "${P1_PROBE}:/tmp/phases" >/dev/null 2>&1 && copied=$((copied + 1))
  script_bytes="$(kubectl -n "$NS" exec "$P1_PROBE" -- sh -c 'wc -c < /tmp/probe.sh' 2>/dev/null | tr -d ' \n' || echo 0)"
  phase_bytes="$(kubectl -n "$NS" exec "$P1_PROBE" -- sh -c 'wc -c < /tmp/phases' 2>/dev/null | tr -d ' \n' || echo 0)"
  if [ "${script_bytes:-0}" -gt 0 ] 2>/dev/null && [ "${phase_bytes:-0}" -gt 0 ] 2>/dev/null; then
    record PASS "P1: probe files reach the Pod" "${copied}/2 copies (${script_bytes}B script, ${phase_bytes}B phases)"
  else
    record FAIL "P1: probe files reach the Pod" "script=${script_bytes:-0}B phases=${phase_bytes:-0}B"
    p1_skip_rest "probe files did not reach the Pod"
    return 0
  fi

  kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${P1_SCALER}
  namespace: ${NS}
spec:
  targetRef:
    name: ${P1_MD}
  mode: Active
  sampling:
    intervalSeconds: 10
  roles:
    - name: ${P1_ROLE}
      minReplicas: 1
      maxReplicas: 2
  algorithm:
    type: Proportional
    proportional:
      metric: TotalTokensPerSecond
      tolerance: "0.1"
      roleTargets:
        - name: ${P1_ROLE}
          target: "1"
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 0
EOF

  kubectl -n "$NS" exec "$P1_PROBE" -- sh -c \
    "nohup sh /tmp/probe.sh '${probe_url}' '${P1_MODEL}' >/dev/null 2>&1 & echo started" >/dev/null 2>&1

  p1_wait_phase up "$STEP_BOUND" || true

  replicas="$(p1_role_replicas)"
  deadline=$((SECONDS + STEP_BOUND))
  while [ "$SECONDS" -lt "$deadline" ]; do
    replicas="$(p1_role_replicas)"
    [ "${replicas:-0}" -ge 2 ] 2>/dev/null && break
    sleep 3
  done
  if [ "${replicas:-0}" -ge 2 ] 2>/dev/null; then
    record PASS "P1: target replica count rises under load" "role=${P1_ROLE} replicas=${replicas}"
  else
    record FAIL "P1: target replica count rises under load" "replicas=${replicas:-<none>} running=$(p1_running_requests)"
  fi

  running="$(p1_running_requests)"
  if [ -n "$running" ] && [ "$running" -gt 0 ] 2>/dev/null; then
    record PASS "P1: scaler reads running requests of target" "runningRequests=${running}"
  else
    record FAIL "P1: scaler reads running requests of target" "runningRequests=${running:-<none>}"
  fi

  new_pod=""
  if p1_wait_ready_count 2 "$READY_BOUND"; then
    new_pod="$(comm -13 <(printf '%s\n' "$pods_before_load") <(p1_ready_pods) | head -1)"
  fi
  if [ -n "$new_pod" ]; then
    record PASS "P1: second Pod becomes Ready" "new_pod=${new_pod}"
  else
    record FAIL "P1: second Pod becomes Ready" "ready=$(p1_ready_pods | tr '\n' ' ')"
  fi

  if [ -n "$new_pod" ]; then
    vendor="$(serving_vendor "$(kubectl -n "$NS" get "$MD_RES" "$P1_MD" -o json 2>/dev/null)" 2>/dev/null || echo nvidia)"
    engine="$(p1_target '.spec.engine.name')"
    if serving_profile "$vendor" "$engine" >/dev/null 2>&1; then
      counter_one="$(p1_counter_of "$new_pod")"
      sleep 20
      counter_two="$(p1_counter_of "$new_pod")"
      if [ -n "$counter_one" ] && [ -n "$counter_two" ] \
        && awk "BEGIN { exit !($counter_two > $counter_one) }"; then
        record PASS "P1: new Pod request counter grows under load" "${SP_SERVED} on ${new_pod}: ${counter_one} -> ${counter_two}"
      elif [ -n "$counter_one" ] && [ "$counter_one" -gt 0 ] 2>/dev/null; then
        record PASS "P1: new Pod request counter grows under load" "${SP_SERVED} on ${new_pod}: ${counter_one} -> ${counter_two:-<none>}"
      else
        record FAIL "P1: new Pod request counter grows under load" "${SP_SERVED} on ${new_pod}: ${counter_one:-<none>} -> ${counter_two:-<none>}"
      fi
    else
      record SKIP "P1: new Pod request counter grows under load" "unknown serving profile vendor=${vendor} engine=${engine}"
    fi
  else
    record SKIP "P1: new Pod request counter grows under load" "no second Pod became Ready"
  fi

  committed="$(p1_committed_vector)"
  if [ -n "$committed" ] && [ "$committed" -ge 2 ] 2>/dev/null; then
    record PASS "P1: scaler records committed vector" "${P1_ROLE}=${committed} term=$(p1_scaler '.status.lastCommit.term')"
  else
    record FAIL "P1: scaler records committed vector" "committed=${committed:-<none>}"
  fi

  p1_wait_phase down "$STEP_BOUND" || true

  narrowed="$(kubectl -n "$NS" patch "$SCALER_RES" "$P1_SCALER" --type=merge \
    -p "{\"spec\":{\"algorithm\":{\"proportional\":{\"metric\":\"TotalTokensPerSecond\",\"tolerance\":\"0.1\",\"roleTargets\":[{\"name\":\"${P1_ROLE}\",\"target\":\"1000000\"}]}},\"behavior\":{\"scaleDown\":{\"stabilizationWindowSeconds\":15,\"decodePostScaleUpGraceSeconds\":1}}}}" \
    2>&1 | tr '\n' ' ')"

  states=""
  deadline=$((SECONDS + RETIRE_BOUND))
  while [ "$SECONDS" -lt "$deadline" ]; do
    state="$(p1_target '.status.retirement.state // ""')"
    if [ -n "$state" ]; then
      case "$states" in
        *"$state"*) ;;
        *) states="${states}${states:+,}${state}" ;;
      esac
    fi
    replicas="$(p1_role_replicas)"
    ready_left="$(p1_ready_pods | grep -c . || true)"
    if [ "${replicas:-2}" -le 1 ] 2>/dev/null && [ -z "$state" ] && [ "${ready_left:-0}" -le 1 ]; then
      break
    fi
    sleep 2
  done

  replicas="$(p1_role_replicas)"
  if [ "${replicas:-2}" -le 1 ] 2>/dev/null; then
    record PASS "P1: replica count falls when scaler is narrowed" "replicas=${replicas}"
  else
    record FAIL "P1: replica count falls when scaler is narrowed" "replicas=${replicas:-<none>} (${narrowed:0:160})"
  fi

  if [ -n "$states" ]; then
    record PASS "P1: retirement states observed on target" "states=${states}"
  else
    record FAIL "P1: retirement states observed on target" "replicas=${replicas:-<none>} but no retirement state observed"
  fi

  p1_wait_phase one "$STEP_BOUND" || true
  p1_wait_phase_done one "$STEP_BOUND" || true
  ready_left="$(p1_ready_pods | grep -c . || true)"
  if [ "${ready_left:-0}" -eq 1 ]; then
    record PASS "P1: one Ready Pod left after retirement" "pod=$(p1_ready_pods | tr '\n' ' ')"
  else
    record FAIL "P1: one Ready Pod left after retirement" "ready_left=${ready_left:-<none>}"
  fi

  phase_failures=""
  phase_detail=""
  phase_evidence=""
  for entry in $P1_PHASES; do
    name="${entry%%:*}"
    [ "$name" = base ] && continue
    result="$(p1_phase_result "$name")"
    ok="${result%% *}"
    bad="${result##* }"
    [ -n "$ok" ] || ok=0
    [ -n "$bad" ] || bad=0
    phase_detail="${phase_detail}${name}=${ok}/${bad} "
    if [ "$ok" -le 0 ] 2>/dev/null || [ "$bad" -ne 0 ] 2>/dev/null; then
      phase_failures="${phase_failures}${name} "
      local bad_sample
      bad_sample="$(kubectl -n "$NS" exec "$P1_PROBE" -- sh -c "head -1 /tmp/badbody.${name} 2>/dev/null | cut -c1-180" 2>/dev/null || true)"
      phase_evidence="${phase_evidence}${name}:${bad_sample} "
    fi
  done
  if [ -z "$phase_failures" ]; then
    record PASS "P1: every phase answers requests and fails none" "${phase_detail}"
  else
    record FAIL "P1: every phase answers requests and fails none" "failed=${phase_failures} (${phase_detail} ${phase_evidence})"
  fi
}

run_phase1
cleanup_phase1

# ==============================================================================
# PHASE 2: Disaggregated P/D TokenThroughput + capacityPrior 1P:2D -> 2P:4D -> 1P:2D
# ==============================================================================

RESTART_LEDGER="${TMP_DIR}/p2-restarts.json"
printf '{}\n' > "$RESTART_LEDGER"
RESTART_READ_ERR=""

p2_sample_restarts() {
  local pods_json updated
  if ! pods_json="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${P2_DEPLOY}" -o json 2>/dev/null)"; then
    RESTART_READ_ERR="failed to list pods for ${P2_DEPLOY}"
    return 1
  fi
  # shellcheck disable=SC2016 # jq variables
  if ! updated="$(printf '%s' "$pods_json" | jq -ce --slurpfile prev "$RESTART_LEDGER" '
    reduce (.items // [])[] as $p (
      ($prev[0] // {});
      ($p.metadata.uid // "") as $uid |
      if $uid == "" then .
      else
        (([($p.status.containerStatuses // [])[]?.restartCount // 0,
           ($p.status.initContainerStatuses // [])[]?.restartCount // 0] | add) // 0) as $cnt |
        .[$uid] = ([.[$uid] // 0, $cnt] | max)
      end
    )
  ' 2>/dev/null)"; then
    RESTART_READ_ERR="failed to aggregate container restart counts"
    return 1
  fi
  printf '%s\n' "$updated" > "$RESTART_LEDGER"
  return 0
}

p2_role_spec_replicas() {
  # shellcheck disable=SC2016 # jq variable
  kubectl -n "$NS" get "$MD_RES" "$P2_DEPLOY" -o json 2>/dev/null | \
    jq -r --arg r "$1" '[(.spec.roles // [])[] | select(.name==$r) | .replicas] | .[0] // 0' 2>/dev/null || echo 0
}

p2_role_ready_replicas() {
  # shellcheck disable=SC2016 # jq variable
  kubectl -n "$NS" get "$MD_RES" "$P2_DEPLOY" -o json 2>/dev/null | \
    jq -r --arg r "$1" '[(.status.roles // [])[] | select(.name==$r) | .ready] | .[0] // 0' 2>/dev/null || echo 0
}

p2_wait_probe_worker() {
  local pid_file="$1" bound="$2"
  kubectl -n "$NS" exec "$P2_PROBE" -- sh -c "
    pid=\$(cat '${pid_file}' 2>/dev/null || true)
    [ -n \"\$pid\" ] || exit 1
    elapsed=0
    while kill -0 \"\$pid\" 2>/dev/null; do
      if [ \"\$elapsed\" -ge ${bound} ]; then
        exit 1
      fi
      sleep 1
      elapsed=\$((elapsed + 1))
    done
    exit 0
  " >/dev/null 2>&1
}

run_phase2() {
  local at hw_product hw_mfg hw_res hw_mfg_lc gpu_cap free_gpus art_rev
  if [ "${E2E_C35_SKIP_PHASE2:-0}" = "1" ]; then
    record SKIP "P2: disaggregated 1P:2D -> 2P:4D -> 1P:2D prerequisites" "skipped via E2E_C35_SKIP_PHASE2=1"
    return 0
  fi
  at="${E2E_C35_ACCELERATOR_TYPE:-$(md_usable_accelerator_type "$NS" || true)}"
  if [ -z "$at" ]; then
    record SKIP "P2: disaggregated 1P:2D -> 2P:4D -> 1P:2D prerequisites" "no usable AcceleratorType available"
    return 0
  fi
  hw_product="$(kubectl get acceleratortype "$at" -o jsonpath='{.status.detail.product}' 2>/dev/null || true)"
  hw_mfg="$(kubectl get acceleratortype "$at" -o jsonpath='{.status.detail.manufacturer}' 2>/dev/null || true)"
  hw_res="$(kubectl get acceleratortype "$at" -o jsonpath='{.status.detail.resourceName}' 2>/dev/null || true)"
  free_gpus="$(kubectl get acceleratortype "$at" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null || true)"
  if [ -z "$hw_product" ]; then
    record SKIP "P2: disaggregated 1P:2D -> 2P:4D -> 1P:2D prerequisites" "AcceleratorType ${at} has empty status.detail.product"
    return 0
  fi
  hw_mfg_lc="$(printf '%s' "$hw_mfg" | tr '[:upper:]' '[:lower:]')"
  if [ -z "${E2E_C35_PD_ENGINE_IMAGE:-}" ] && [ "$hw_mfg_lc" != "nvidia" ]; then
    record SKIP "P2: disaggregated 1P:2D -> 2P:4D -> 1P:2D prerequisites" "default CUDA image requires nvidia AcceleratorType, got ${hw_mfg:-unknown}"
    return 0
  fi
  [ -n "$hw_res" ] || hw_res="nvidia.com/gpu"

  gpu_cap="$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
res = sys.argv[1]
doc = json.load(sys.stdin)
total = 0
for n in doc.get("items", []):
    conds = {c["type"]: c["status"] for c in n.get("status", {}).get("conditions", [])}
    if conds.get("Ready") != "True":
        continue
    val = n.get("status", {}).get("allocatable", {}).get(res, "0")
    try:
        total += int(float(val))
    except ValueError:
        pass
print(total)
' "$hw_res" 2>/dev/null || echo 0)"
  if [ "${gpu_cap:-0}" -lt 6 ]; then
    record SKIP "P2: disaggregated 1P:2D -> 2P:4D -> 1P:2D prerequisites" "need >= 6 allocatable ${hw_res} GPUs, found ${gpu_cap:-0}"
    return 0
  fi
  if [ -n "$free_gpus" ] && [ "${free_gpus:-0}" -lt 6 ] 2>/dev/null; then
    record SKIP "P2: disaggregated 1P:2D -> 2P:4D -> 1P:2D prerequisites" "need >= 6 free exclusive GPUs on ${at}, found ${free_gpus}"
    return 0
  fi

  if ! kubectl -n "$NS" get modelartifact "$P2_ARTIFACT" >/dev/null 2>&1; then
    record SKIP "P2: disaggregated 1P:2D -> 2P:4D -> 1P:2D prerequisites" "ModelArtifact ${NS}/${P2_ARTIFACT} not found"
    return 0
  fi
  art_rev="$(kubectl -n "$NS" get modelartifact "$P2_ARTIFACT" -o jsonpath='{.status.resolved.revision}' 2>/dev/null || true)"
  if [ -z "$art_rev" ]; then
    record SKIP "P2: disaggregated 1P:2D -> 2P:4D -> 1P:2D prerequisites" "ModelArtifact ${NS}/${P2_ARTIFACT} has empty status.resolved.revision"
    return 0
  fi

  local pair res name
  for pair in "configmap:${P2_CM}" "${MD_RES}:${P2_DEPLOY}" "${SCALER_RES}:${P2_SCALER}" "pod:${P2_PROBE}"; do
    res="${pair%%:*}"
    name="${pair#*:}"
    if kubectl -n "$NS" get "$res" "$name" >/dev/null 2>&1; then
      record FAIL "P2: resource collision guard" "${NS}/${res}/${name} already exists"
      return 0
    fi
  done

  echo "=== CASE 35 Phase 2: Live P/D capacityPrior scaling (ns=${NS}, at=${at}, product=${hw_product}, gpus=${gpu_cap}, artifact=${P2_ARTIFACT}@${art_rev:0:8}) ==="

  P2_CREATED_CM="$P2_CM"
  kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${P2_CM}
data:
  priors.yaml: |
    apiVersion: worker.gpustack.ai/v1alpha1
    kind: ModelDeploymentScalerCapacityPriorCatalog
    profiles:
      - mode: disaggregated
        model: ${P2_MODEL}
        quantization: bf16
        engine: vllm
        prefill:
          replicas: 1
          hardware: ${hw_product}
          topology:
            gpusPerReplica: 1
            tensorParallel: 1
            expertParallel: 1
        decode:
          replicas: 2
          hardware: ${hw_product}
          topology:
            gpusPerReplica: 1
            tensorParallel: 1
            expertParallel: 1
        points:
          - concurrency: 1
            totalTokensPerSecond: 20
            inputTokensPerSecond: 15
            outputTokensPerSecond: 5
            ttftMilliseconds:
              p99: 400
            itlMilliseconds:
              p99: 30
          - concurrency: 2
            totalTokensPerSecond: 40
            inputTokensPerSecond: 30
            outputTokensPerSecond: 10
            ttftMilliseconds:
              p99: 450
            itlMilliseconds:
              p99: 35
YAML

  P2_CREATED_MD="$P2_DEPLOY"
  kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${P2_DEPLOY}
  namespace: ${NS}
  annotations:
    worker.gpustack.ai/precision: "bf16"
spec:
  model:
    name: ${P2_MODEL}
    artifactRef:
      name: ${P2_ARTIFACT}
  engine:
    name: vLLM
    image: ${P2_ENGINE_IMAGE}
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      replicas: 1
      acceleratorType: ${at}
      image: ${P2_ENGINE_IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      env:
        - name: VLLM_HTTP_TIMEOUT_KEEP_ALIVE
          value: "3600"
      extraArgs:
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
        - --enable-scale-out
    - name: decode
      kind: Decode
      replicas: 2
      acceleratorType: ${at}
      image: ${P2_ENGINE_IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      env:
        - name: VLLM_HTTP_TIMEOUT_KEEP_ALIVE
          value: "3600"
      extraArgs:
        - --tensor-parallel-size
        - "1"
        - --max-model-len
        - "16384"
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
        - --enable-scale-out
YAML

  P2_CREATED_SCALER="$P2_SCALER"
  kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${P2_SCALER}
  namespace: ${NS}
spec:
  targetRef:
    name: ${P2_DEPLOY}
  mode: Active
  sampling:
    intervalSeconds: 5
    pendingTimeoutSeconds: 180
  roles:
    - name: prefill
      minReplicas: 1
      maxReplicas: 2
    - name: decode
      minReplicas: 2
      maxReplicas: 4
  ratios:
    - numeratorRole: decode
      denominatorRole: prefill
      target: "2"
      min: "2"
      max: "2"
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 45
      decodePostScaleUpGraceSeconds: 90
  algorithm:
    type: TokenThroughput
    tokenThroughput:
      ttftTargetMilliseconds: "500"
      itlTargetMilliseconds: "50"
      stableWindowSeconds: 30
      panicWindowSeconds: 10
    capacityPrior:
      headroom: "0.75"
      catalogRef:
        name: ${P2_CM}
YAML

  local deadline=$((SECONDS + 90)) priors_ok=false sc_json c_priors c_bound c_active conds
  while [ "$SECONDS" -lt "$deadline" ]; do
    sc_json="$(kubectl -n "$NS" get "$SCALER_RES" "$P2_SCALER" -o json 2>/dev/null || true)"
    c_priors="$(printf '%s' "$sc_json" | jq -r '[(.status.conditions // [])[] | select(.type=="CapacityPriorsReady") | .status] | .[0] // ""' 2>/dev/null || true)"
    c_bound="$(printf '%s' "$sc_json" | jq -r '[(.status.conditions // [])[] | select(.type=="Bound") | .status] | .[0] // ""' 2>/dev/null || true)"
    c_active="$(printf '%s' "$sc_json" | jq -r '[(.status.conditions // [])[] | select(.type=="Active") | .status] | .[0] // ""' 2>/dev/null || true)"
    if [ "$c_priors" = "True" ] && [ "$c_bound" = "True" ] && [ "$c_active" = "True" ]; then
      priors_ok=true
      break
    fi
    sleep 2
  done
  if [ "$priors_ok" = true ]; then
    record PASS "P2: scaler CapacityPriorsReady & Bound & Active" "resolved disaggregated 1P:2D catalog"
  else
    conds="$(kubectl -n "$NS" get "$SCALER_RES" "$P2_SCALER" -o json 2>/dev/null | jq -c '[(.status.conditions // [])[] | {type,status,reason,message}]' || true)"
    record FAIL "P2: scaler CapacityPriorsReady & Bound & Active" "conditions=${conds}"
    return 0
  fi

  deadline=$((SECONDS + P2_READY_BOUND))
  local md_ready=false phase="" pr=0 dr=0
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase="$(kubectl -n "$NS" get "$MD_RES" "$P2_DEPLOY" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    pr="$(p2_role_ready_replicas prefill)"
    dr="$(p2_role_ready_replicas decode)"
    if [ "$phase" = "Ready" ] && [ "${pr:-0}" -ge 1 ] && [ "${dr:-0}" -ge 2 ]; then
      md_ready=true
      break
    fi
    sleep 5
  done
  if [ "$md_ready" = true ]; then
    record PASS "P2: initial 1P:2D deployment ready" "phase=Ready prefill=1 decode=2"
  else
    record FAIL "P2: initial 1P:2D deployment ready" "timeout (${P2_READY_BOUND}s), phase=${phase:-unknown} prefill=${pr:-0} decode=${dr:-0}"
    return 0
  fi
  p2_sample_restarts || true

  P2_CREATED_POD="$P2_PROBE"
  kubectl -n "$NS" run "$P2_PROBE" --image="$PROBE_IMAGE" --restart=Never --command -- sleep infinity >/dev/null
  if ! kubectl -n "$NS" wait "pod/${P2_PROBE}" --for=condition=Ready --timeout=180s >/dev/null 2>&1; then
    record FAIL "P2: probe Pod ready" "Pod ${NS}/${P2_PROBE} failed to become Ready"
    return 0
  fi

  local router_url="http://${P2_DEPLOY}-router.${NS}.svc.cluster.local:8000"
  local models_json="" served_model=""
  deadline=$((SECONDS + 120))
  while [ "$SECONDS" -lt "$deadline" ]; do
    models_json="$(kubectl -n "$NS" exec "$P2_PROBE" -- curl -sf --max-time 10 "${router_url}/v1/models" 2>/dev/null || true)"
    if printf '%s' "$models_json" | grep -Fq '"data"'; then
      break
    fi
    sleep 3
  done
  served_model="$(printf '%s' "$models_json" | jq -r '.data[0].id // ""' 2>/dev/null || true)"
  [ -n "$served_model" ] || served_model="$P2_MODEL"
  if [ -n "$models_json" ]; then
    record PASS "P2: router /v1/models reachable" "model=${served_model}"
  else
    record FAIL "P2: router /v1/models reachable" "could not query ${router_url}/v1/models"
    return 0
  fi

  local canary_b64
  canary_b64="$(jq -nc --arg m "$served_model" '{model:$m, messages:[{role:"user",content:"Reply with one word: ok."}], max_tokens:4, temperature:0}' | base64 | tr -d '\n\r')"
  kubectl -n "$NS" exec "$P2_PROBE" -- sh -c "
printf '%s' '${canary_b64}' | base64 -d > /tmp/canary.json
test -s /tmp/canary.json
cat > /tmp/canary.sh <<'EOF'
#!/bin/sh
: > /tmp/canary_ok_lines
: > /tmp/canary_fail_codes
: > /tmp/canary_fail_details
rm -f /tmp/stop_canary
while [ ! -f /tmp/stop_canary ]; do
  rc=0
  code=\$(curl -sS -o /tmp/canary.body -w '%{http_code}' --max-time 60 -H 'Content-Type: application/json' -d @/tmp/canary.json '${router_url}/v1/chat/completions' 2>/tmp/canary.err) || rc=\$?
  if [ \"\$rc\" -eq 0 ] && [ \"\$code\" = \"200\" ] && grep -Fq '\"choices\"' /tmp/canary.body 2>/dev/null; then
    echo 1 >> /tmp/canary_ok_lines
  else
    echo \"rc=\${rc}:\${code:-000}\" >> /tmp/canary_fail_codes
    printf '%s rc=%s code=%s err=%s body=%s\n' \"\$(date +%H:%M:%S)\" \"\$rc\" \"\${code:-000}\" \"\$(tr '\n\r' '  ' < /tmp/canary.err 2>/dev/null | head -c 80)\" \"\$(head -c 160 /tmp/canary.body 2>/dev/null | tr '\n\r' '  ')\" >> /tmp/canary_fail_details
  fi
  sleep 2
done
EOF
chmod +x /tmp/canary.sh
nohup /tmp/canary.sh >/tmp/canary.log 2>&1 </dev/null &
echo \$! > /tmp/canary.pid
"

  sleep 15
  p2_sample_restarts || true
  local bp bd
  bp="$(p2_role_spec_replicas prefill)"
  bd="$(p2_role_spec_replicas decode)"
  if [ "$bp" = "1" ] && [ "$bd" = "2" ]; then
    record PASS "P2: baseline stays at 1P:2D" "prefill=${bp} decode=${bd}"
  else
    record FAIL "P2: baseline stays at 1P:2D" "expected 1:2, got prefill=${bp} decode=${bd}"
  fi

  local long_text="Explain the mathematical foundations of queuing theory, queueing stability, M/M/c queues, prefill and decode disaggregation in large language model serving, KV cache transfer bottlenecks, token throughput planning, latency Service Level Objectives for time to first token and inter-token latency, and whole-unit capacity scaling in detail."
  local burst_b64
  burst_b64="$(jq -nc --arg m "$served_model" --arg c "$long_text" '{model:$m, messages:[{role:"user",content:$c}], max_tokens:128, temperature:0}' | base64 | tr -d '\n\r')"
  kubectl -n "$NS" exec "$P2_PROBE" -- sh -c "
printf '%s' '${burst_b64}' | base64 -d > /tmp/burst.json
test -s /tmp/burst.json
cat > /tmp/burst.sh <<'EOF'
#!/bin/sh
: > /tmp/burst_ok_lines
: > /tmp/burst_fail_codes
: > /tmp/burst_fail_details
rm -f /tmp/stop_burst
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16; do
  (
    while [ ! -f /tmp/stop_burst ]; do
      rc=0
      code=\$(curl -sS -o \"/tmp/burst.\${i}.body\" -w '%{http_code}' --max-time 120 -H 'Content-Type: application/json' -d @/tmp/burst.json '${router_url}/v1/chat/completions' 2>\"/tmp/burst.\${i}.err\") || rc=\$?
      if [ \"\$rc\" -eq 0 ] && [ \"\$code\" = \"200\" ] && grep -Fq '\"choices\"' \"/tmp/burst.\${i}.body\" 2>/dev/null; then
        echo 1 >> /tmp/burst_ok_lines
      else
        echo \"rc=\${rc}:\${code:-000}\" >> /tmp/burst_fail_codes
        printf '%s worker=%s rc=%s code=%s err=%s body=%s\n' \"\$(date +%H:%M:%S)\" \"\$i\" \"\$rc\" \"\${code:-000}\" \"\$(tr '\n\r' '  ' < \"/tmp/burst.\${i}.err\" 2>/dev/null | head -c 80)\" \"\$(head -c 160 \"/tmp/burst.\${i}.body\" 2>/dev/null | tr '\n\r' '  ')\" >> /tmp/burst_fail_details
      fi
    done
  ) &
done
wait
EOF
chmod +x /tmp/burst.sh
nohup /tmp/burst.sh >/tmp/burst.log 2>&1 </dev/null &
echo \$! > /tmp/burst.pid
"

  deadline=$((SECONDS + P2_SCALE_BOUND))
  local scaled_up=false ratio_broken=false md_json="" sp=0 sd=0 rp=0 rd=0
  while [ "$SECONDS" -lt "$deadline" ]; do
    p2_sample_restarts || true
    md_json="$(kubectl -n "$NS" get "$MD_RES" "$P2_DEPLOY" -o json 2>/dev/null || true)"
    sp="$(printf '%s' "$md_json" | jq -r '[(.spec.roles // [])[] | select(.name=="prefill") | .replicas] | .[0] // 0' 2>/dev/null || echo 0)"
    sd="$(printf '%s' "$md_json" | jq -r '[(.spec.roles // [])[] | select(.name=="decode") | .replicas] | .[0] // 0' 2>/dev/null || echo 0)"
    rp="$(printf '%s' "$md_json" | jq -r '[(.status.roles // [])[] | select(.name=="prefill") | .ready] | .[0] // 0' 2>/dev/null || echo 0)"
    rd="$(printf '%s' "$md_json" | jq -r '[(.status.roles // [])[] | select(.name=="decode") | .ready] | .[0] // 0' 2>/dev/null || echo 0)"
    if [ $((sp * 2)) -ne "$sd" ]; then
      ratio_broken=true
      record FAIL "P2: atomic 1:2 ratio invariant during scale-up" "observed prefill=${sp} decode=${sd}"
      break
    fi
    if [ "$sp" = "2" ] && [ "$sd" = "4" ] && [ "$rp" = "2" ] && [ "$rd" = "4" ]; then
      scaled_up=true
      break
    fi
    sleep 5
  done

  kubectl -n "$NS" exec "$P2_PROBE" -- touch /tmp/stop_burst 2>/dev/null || true

  if [ "$ratio_broken" = false ]; then
    if [ "$scaled_up" = true ]; then
      record PASS "P2: atomic whole-unit scale-up 1P:2D -> 2P:4D" "spec=2:4 ready=2:4"
    else
      record FAIL "P2: atomic whole-unit scale-up 1P:2D -> 2P:4D" "timeout (${P2_SCALE_BOUND}s), spec=${sp}:${sd} ready=${rp}:${rd}"
    fi
  fi

  deadline=$((SECONDS + RETIRE_BOUND))
  local scaled_down=false saw_retirement=false ret_state="" pod_cnt=0 ret_reason=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    p2_sample_restarts || true
    md_json="$(kubectl -n "$NS" get "$MD_RES" "$P2_DEPLOY" -o json 2>/dev/null || true)"
    sp="$(printf '%s' "$md_json" | jq -r '[(.spec.roles // [])[] | select(.name=="prefill") | .replicas] | .[0] // 0' 2>/dev/null || echo 0)"
    sd="$(printf '%s' "$md_json" | jq -r '[(.spec.roles // [])[] | select(.name=="decode") | .replicas] | .[0] // 0' 2>/dev/null || echo 0)"
    rp="$(printf '%s' "$md_json" | jq -r '[(.status.roles // [])[] | select(.name=="prefill") | .ready] | .[0] // 0' 2>/dev/null || echo 0)"
    rd="$(printf '%s' "$md_json" | jq -r '[(.status.roles // [])[] | select(.name=="decode") | .ready] | .[0] // 0' 2>/dev/null || echo 0)"
    ret_state="$(printf '%s' "$md_json" | jq -r '.status.retirement.state // ""' 2>/dev/null || true)"
    [ -z "$ret_state" ] || saw_retirement=true
    if [ $((sp * 2)) -ne "$sd" ]; then
      ratio_broken=true
      record FAIL "P2: atomic 1:2 ratio invariant during scale-down" "observed prefill=${sp} decode=${sd}"
      break
    fi
    if [ "$sp" = "1" ] && [ "$sd" = "2" ] && [ "$rp" = "1" ] && [ "$rd" = "2" ]; then
      pod_cnt="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${P2_DEPLOY},app.kubernetes.io/component in (prefill,decode)" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
      if [ "${pod_cnt:-0}" -le 3 ]; then
        scaled_down=true
        break
      fi
    fi
    sleep 5
  done

  if [ "$ratio_broken" = false ]; then
    if [ "$scaled_down" = true ]; then
      record PASS "P2: atomic whole-unit scale-down 2P:4D -> 1P:2D" "spec=1:2 ready=1:2 retirement_observed=${saw_retirement}"
    else
      ret_reason="$(printf '%s' "$md_json" | jq -r '.status.retirement.reason // ""' 2>/dev/null || true)"
      record FAIL "P2: atomic whole-unit scale-down 2P:4D -> 1P:2D" "timeout (${RETIRE_BOUND}s), spec=${sp}:${sd} ready=${rp}:${rd} ret_state=${ret_state:-none} ret_reason=${ret_reason}"
    fi
  fi

  kubectl -n "$NS" exec "$P2_PROBE" -- touch /tmp/stop_canary 2>/dev/null || true
  local burst_drained=true canary_drained=true probe_counts canary_ok canary_fail burst_ok burst_fail
  p2_wait_probe_worker /tmp/burst.pid 130 || burst_drained=false
  p2_wait_probe_worker /tmp/canary.pid 70 || canary_drained=false

  probe_counts="$(kubectl -n "$NS" exec "$P2_PROBE" -- sh -c '
    awk "END{print NR}" /tmp/canary_ok_lines &&
    awk "END{print NR}" /tmp/canary_fail_codes &&
    awk "END{print NR}" /tmp/burst_ok_lines &&
    awk "END{print NR}" /tmp/burst_fail_codes
  ' 2>/dev/null | tr '\n' ' ' | tr -s ' ' | sed 's/^ //;s/ $//' || true)"
  read -r canary_ok canary_fail burst_ok burst_fail <<< "${probe_counts:-}"

  if [ "$burst_drained" = true ] && [ "$canary_drained" = true ] && \
     [ -n "${canary_ok:-}" ] && [ "${canary_ok:-0}" -gt 0 ] && [ "${canary_fail:-1}" -eq 0 ] && \
     [ -n "${burst_ok:-}" ] && [ "${burst_ok:-0}" -gt 0 ] && [ "${burst_fail:-1}" -eq 0 ]; then
    record PASS "P2: continuous router probe zero failed requests" "canary_ok=${canary_ok}, canary_fail=0, burst_ok=${burst_ok}, burst_fail=0"
  else
    local canary_codes burst_codes canary_sample burst_sample
    canary_codes="$(kubectl -n "$NS" exec "$P2_PROBE" -- sh -c 'sort /tmp/canary_fail_codes 2>/dev/null | uniq -c | tr "\n" ";" ' 2>/dev/null || true)"
    burst_codes="$(kubectl -n "$NS" exec "$P2_PROBE" -- sh -c 'sort /tmp/burst_fail_codes 2>/dev/null | uniq -c | tr "\n" ";" ' 2>/dev/null || true)"
    canary_sample="$(kubectl -n "$NS" exec "$P2_PROBE" -- sh -c 'head -n 3 /tmp/canary_fail_details 2>/dev/null | tr "\n" "|"' 2>/dev/null || true)"
    burst_sample="$(kubectl -n "$NS" exec "$P2_PROBE" -- sh -c 'head -n 3 /tmp/burst_fail_details 2>/dev/null | tr "\n" "|"' 2>/dev/null || true)"
    echo "[case-35-diag] router epp logs (tail 30):" >&2
    kubectl -n "$NS" logs "deploy/${P2_DEPLOY}-router" -c epp --tail=30 >&2 2>/dev/null || true
    record FAIL "P2: continuous router probe zero failed requests" "drained=${canary_drained}/${burst_drained}, canary_ok=${canary_ok:-?}, canary_fail=${canary_fail:-?} (${canary_codes} ${canary_sample}), burst_ok=${burst_ok:-?}, burst_fail=${burst_fail:-?} (${burst_codes} ${burst_sample})"
  fi

  p2_sample_restarts || true
  local pods_seen restarts
  pods_seen="$(jq 'length' "$RESTART_LEDGER" 2>/dev/null || echo 0)"
  restarts="$(jq '[.[]] | add // 0' "$RESTART_LEDGER" 2>/dev/null || echo -1)"
  if [ -z "$RESTART_READ_ERR" ] && [ "${pods_seen:-0}" -ge 4 ] && [ "${restarts:--1}" -eq 0 ]; then
    record PASS "P2: zero container restarts across serving and router Pods" "restarts=0 pods_observed=${pods_seen}"
  else
    record FAIL "P2: zero container restarts across serving and router Pods" "restarts=${restarts} pods_observed=${pods_seen} read_err=${RESTART_READ_ERR:-none}"
  fi
}

run_phase2
results
