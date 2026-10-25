#!/usr/bin/env bash
#
# CASE 129 — a ModelDeploymentScaler acting on a live target: the signal it reads, the replica
#             count it moves, and the retirement that carries the count back down
#             (MUTATING, self-recovering; AUTO-SKIPS the rows that need a serving target)
#
#   case-129.sh <NS> [MD_NAME] [ROLE_NAME]
#
#   The namespace may also be named by E2E_C129_NS and the target by E2E_C129_MD, so a caller that
#   deploys the target into another namespace (the benchmark's manifests carry their own) can
#   point the case at it without moving the target.
#
# Prerequisites: nodes=1; gpu=ascend|nvidia|amd; cluster=real; inputs=E2E_C129_ENGINE_IMAGE,E2E_C129_MD,E2E_C129_MODEL,E2E_C129_NS,E2E_C129_PHASES,E2E_C129_PROBE_IMAGE,E2E_C129_PROBE_URL,E2E_C129_READY_BOUND,E2E_C129_RETIRE_BOUND,E2E_C129_ROLE,E2E_C129_STEP_BOUND,E2E_C129_WEIGHTS
# Goal:        A scaler that is allowed to act does so through a chain of observable steps, and
#              this case walks the chain against a deployment that really serves. The scaler reads
#              the engine's own Prometheus counters from the pods of its target, divides what it
#              read by the signal one replica is expected to carry, and commits a replica vector.
#              The worker applies that vector to the target, and a vector that shrinks is not a
#              delete: the replica that goes away is retired through the target's own retirement
#              protocol, and the reservation that protocol writes is what a reader watches.
#
#              THE PROBE RUNS INSIDE THE CLUSTER, because the signal is read per pod. A load from
#              outside would still be counted, but a probe outside could not tell the scaler that
#              the load stopped, and the scale-down is the half of this case that matters most.
#
#              THE PHASES ARE FIXED, so a reader can tell which claim each moment supports. The
#              probe sends a small load first (base), a heavy one next (up), keeps it heavy while
#              the second replica serves (two), eases off (down) and stays small (one). The scaler
#              is created before the probe starts, so its recommendation is already waiting when
#              the load arrives, and the case narrows the scaler during the easing phase, so the
#              scale-down is observed as the load falls.
#
#              A REQUEST COUNTS ONLY WHEN IT WAS ANSWERED. A 404 from the router, or a body that
#              carries an error, is a failed request and is recorded as one: a load of failures
#              teaches the scaler nothing, and a phase that failed is a phase the deployment could
#              not serve.
#
# Environment: A cluster with a serving ModelDeployment. When MD_NAME names one, the case uses
#              it: it must reach Ready, and its role must exist. When MD_NAME is omitted, the
#              case builds its own target, which needs a real engine and real weights, named by
#              the environment because the case cannot know them:
#
#                E2E_C129_ENGINE_IMAGE  the vLLM image the target runs (required to build one)
#                E2E_C129_WEIGHTS       the host path holding the weights (required to build one)
#                E2E_C129_MODEL         the served model name. It defaults to the target's own
#                                       .spec.model.name when a target is named, and to the mount
#                                       path when the case builds one, because a request for a
#                                       model the endpoint does not serve is refused with 404.
#
#              E2E_C129_PROBE_IMAGE    the image of the probe Pod (default
#                                       docker.io/curlimages/curl:8.10.1)
#              E2E_C129_PHASES         the phase list, as "name concurrency seconds" entries
#                                       separated by spaces (default
#                                       "base:2:30 up:24:90 two:24:90 down:2:90 one:2:30")
#              E2E_C129_ROLE           the role to scale, when ROLE_NAME is not given
#                                       (default server)
#
#              AUTO-SKIP: the rows record SKIP, naming what is missing, when the target cannot
#              be built or never reaches Ready, or when the probe Pod never starts. The case
#              never mocks a signal: a scaler that reads a number nobody produced is not the
#              thing under test.
#
# Inputs:      MOCKED: nothing. REAL: the engine's own metrics endpoint, the scaler's sampler
#              and planner, the worker's application of a committed vector, and the target's
#              retirement protocol.
#
# Expected:    - the target serves, and the probe gets an answer;
#              - the scaler reads the running requests of the target and publishes them;
#              - the target's replica count rises to two, its second Pod becomes Ready, and the
#                new Pod's own request counter grows while the heavy load runs;
#              - the scaler records the vector it committed;
#              - the count falls back to one when the scaler is narrowed, and the retirement
#                that carried it down is observed on the target;
#              - every phase answered at least one request and failed none.
#
# Cleanup:     Deletes the scaler, the probe Pod, and the target it built itself, on completion
#              and on exit. A target the caller named is never deleted.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${E2E_C129_NS:-${1:-}}"
if [ -z "$NS" ]; then
  echo "usage: case-129.sh <NS> [MD_NAME] [ROLE_NAME]" >&2
  exit 2
fi

MD="${E2E_C129_MD:-${2:-}}"
ROLE="${3:-${E2E_C129_ROLE:-server}}"

SCALER_RESOURCE=modeldeploymentscalers.v1alpha1.worker.gpustack.ai
# THE DEPLOYMENT IS READ FROM ITS CRD, not from the aggregated v1 API. The aggregated API serves
# the same kind but proxies it through the operator's cached client, so a read through it can
# answer with a status the operator has not seen yet, and this case reads the status to time a
# scale-out and a scale-down. The CRD answers with what is stored.
MD_RESOURCE=modeldeployments.v1alpha1.worker.gpustack.ai
SCALER=case129-scaler
PROBE=case129-probe

ENGINE_IMAGE="${E2E_C129_ENGINE_IMAGE:-}"
WEIGHTS="${E2E_C129_WEIGHTS:-}"
MODEL="${E2E_C129_MODEL:-}"
PROBE_IMAGE="${E2E_C129_PROBE_IMAGE:-docker.io/curlimages/curl:8.10.1}"
# THE PHASES ARE A NAME, A CONCURRENCY AND A DURATION. The cooldown is the longest of them because
# the retirement it carries is not instant: a replica that is going away holds its card and its
# quota until it has left, and the last phase has to be served by a deployment whose retirement
# has settled, or it measures a deployment that is still taking a Pod away.
PHASES="${E2E_C129_PHASES:-base:2:30 up:24:90 two:24:90 down:2:120 one:2:60}"

READY_BOUND="${E2E_C129_READY_BOUND:-900}"
STEP_BOUND="${E2E_C129_STEP_BOUND:-300}"
RETIRE_BOUND="${E2E_C129_RETIRE_BOUND:-300}"

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_serving-lib.sh"

cleanup() {
  kubectl -n "$NS" delete "$SCALER_RESOURCE" "$SCALER" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
  if [ -n "$BUILT" ]; then
    kubectl -n "$NS" delete "$MD_RESOURCE" "$MD" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
  fi
  return 0
}
trap cleanup EXIT

# ---------------------------------------------------------------- the readers

# A value of a jq path on the target, or empty.
target() {
  kubectl -n "$NS" get "$MD_RESOURCE" "$MD" -o json 2>/dev/null | jq -r "$@"
}

# A value of a jq path on the scaler, or empty.
scaler() {
  kubectl -n "$NS" get "$SCALER_RESOURCE" "$SCALER" -o json 2>/dev/null | jq -r "$@"
}

# The running requests the scaler read for the role, or empty.
running_requests() {
  # shellcheck disable=SC2016  # the dollar name is a jq variable, and must not expand
  scaler --arg r "$ROLE" \
    '[(.status.roles // [])[] | select(.name == $r) | .runningRequests] | .[0] // ""'
}

# The replica count the scaler last committed for the role, or empty.
committed_vector() {
  # shellcheck disable=SC2016  # the dollar name is a jq variable, and must not expand
  scaler --arg r "$ROLE" '(.status.lastCommit.vector // {})[$r] // ""'
}

# The replica count the target declares for the role, or empty.
role_replicas() {
  # shellcheck disable=SC2016  # the dollar name is a jq variable, and must not expand
  target --arg r "$ROLE" '[(.spec.roles // [])[] | select(.name == $r) | .replicas] | .[0] // ""'
}

# The Pods of the role that are Ready, one per line, sorted. A Pod that is going away is not
# Ready for the purpose of this count.
ready_pods() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD},app.kubernetes.io/component=${ROLE}" \
    -o json 2>/dev/null \
    | jq -r '.items[] | select(.metadata.deletionTimestamp == null and
      any(.status.conditions[]?; .type == "Ready" and .status == "True")) | .metadata.name' | sort
}

# Wait until the role has exactly the wanted number of Ready Pods, and exactly that many Pods in
# all, or the bound expires.
wait_ready_count() { # <wanted> <seconds>
  local wanted="$1" bound="$2" deadline ready total
  deadline=$((SECONDS + bound))
  while [ "$SECONDS" -lt "$deadline" ]; do
    ready="$(ready_pods | grep -c . || true)"
    total="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD},app.kubernetes.io/component=${ROLE}" \
      --no-headers 2>/dev/null | grep -c . || true)"
    if [ "${ready:-0}" -eq "$wanted" ] && [ "${total:-0}" -eq "$wanted" ]; then
      return 0
    fi
    sleep 5
  done
  return 1
}

# The counter of a Pod's engine, read from the Pod's own Prometheus endpoint. The counter is the
# one the serving profile names for the vendor and engine, because a counter that another engine
# exports under another name is a zero here.
counter_of() { # <pod>
  local ip port scheme
  ip="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.podIP}' 2>/dev/null)"
  port="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.annotations.prometheus\.io/port}' 2>/dev/null)"
  scheme="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.annotations.prometheus\.io/scheme}' 2>/dev/null)"
  serving_sum "$(kubectl -n "$NS" exec "$PROBE" -- curl -fsS --max-time 10 \
    "${scheme:-http}://${ip}:${port}/metrics" 2>/dev/null)" "$SP_SERVED" || echo 0
}

# The phase the probe is in, or empty.
phase_now() {
  kubectl -n "$NS" exec "$PROBE" -- sh -c 'cat /tmp/probe.log 2>/dev/null' 2>/dev/null \
    | awk '/^start / { p = $2 } END { print p }'
}

# The result of a phase, as "ok bad", or empty.
phase_result() { # <name>
  kubectl -n "$NS" exec "$PROBE" -- sh -c "grep '^done $1 ' /tmp/probe.log 2>/dev/null | tail -1" \
    2>/dev/null | awk '{ print $3 " " $4 }'
}

# Wait until the probe is in a phase, or the bound expires.
wait_phase() { # <name> <seconds>
  local name="$1" bound="$2" deadline
  deadline=$((SECONDS + bound))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(phase_now)" = "$name" ] && return 0
    sleep 2
  done
  return 1
}

# Wait until a phase has FINISHED, or the bound expires. A phase that is in progress has no
# result yet, and a result read before the phase ends is a phase that answered nothing.
wait_phase_done() { # <name> <seconds>
  local name="$1" bound="$2" deadline
  deadline=$((SECONDS + bound))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ -n "$(phase_result "$name")" ] && return 0
    sleep 2
  done
  return 1
}

# The rows the rest of the case would have recorded, recorded as SKIP when the case cannot go
# that far. The names are the ones a reader looks for, so a SKIP row says which claim was not
# exercised and why.
skip_rest() { # <reason>
  local reason="$1"
  record SKIP "the scaler reads the running requests of the target" "$reason"
  record SKIP "the target's replica count rises under load" "$reason"
  record SKIP "the target's second Pod becomes Ready" "$reason"
  record SKIP "the new Pod's own request counter grows under the load" "$reason"
  record SKIP "the scaler records the vector it committed" "$reason"
  record SKIP "the replica count falls when the scaler is narrowed" "$reason"
  record SKIP "the retirement that carried the count down is observed" "$reason"
  record SKIP "one Ready Pod is left after the retirement" "$reason"
  record SKIP "every phase answers requests and fails none" "$reason"
}

# Print the rows and say the verdict. Every exit goes through here, so the case says the same
# thing wherever it stops.
finish() {
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-129] ${FAILS} check(s) FAILED"; exit 1; }
  echo "[case-129] all checks passed"
}

# ---------------------------------------------------------------- the target

BUILT=""
ENV_REASON=""
if [ -z "$MD" ]; then
  MD=case129-target
  if [ -z "$ENGINE_IMAGE" ]; then
    ENV_REASON="E2E_C129_ENGINE_IMAGE is unset, so a target built by this case would run no engine and the scaler would read no signal"
  elif [ -z "$WEIGHTS" ]; then
    ENV_REASON="E2E_C129_WEIGHTS is unset, so the engine would have no weights to serve"
  fi
  if [ -z "$ENV_REASON" ]; then
    IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
      2>/dev/null \
      | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        case "$manufacturer" in
          ascend | nvidia | amd) echo "$name"; break ;;
        esac
      done)"
    if [ -z "$IT" ]; then
      ENV_REASON="no usable InstanceType reports an accelerated platform (ascend, nvidia, amd), so a target built by this case would never be admitted"
    fi
  fi
  # A target the case builds serves the model it mounts, so the mount path is the served name.
  MODEL="${MODEL:-/models/Qwen2.5-0.5B-Instruct}"
  if [ -z "$ENV_REASON" ]; then
    kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${ENGINE_IMAGE}
  model:
    name: ${MODEL}
  roles:
    - name: ${ROLE}
      kind: Server
      replicas: 1
      size: 1
      instanceType: ${IT}
      image: ${ENGINE_IMAGE}
      imagePullPolicy: IfNotPresent
      resources:
        accelerator: "1"
      additionalVolumes:
        - hostPath:
            path: ${WEIGHTS}
            type: Directory
          mountPath: ${MODEL}
          readOnly: true
      extraArgs:
        - --max-num-seqs
        - "64"
        - --gpu-memory-utilization
        - "0.90"
EOF
    BUILT=yes
  fi
fi

if [ -n "$ENV_REASON" ]; then
  record SKIP "the target serves and the probe gets an answer" "$ENV_REASON"
  skip_rest "$ENV_REASON"
  finish; exit 0
fi

# The target has to serve before anything is measured: a scaler that reads the counter of an
# engine that never started would be measuring a broken engine.
phase=""
deadline=$((SECONDS + READY_BOUND))
while [ "$SECONDS" -lt "$deadline" ]; do
  phase="$(target '.status.phase')"
  [ "$phase" = "Ready" ] && break
  sleep 10
done
if [ "$phase" != "Ready" ]; then
  pods="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --no-headers 2>/dev/null | grep -c . || true)"
  if [ "${pods:-0}" -gt 0 ]; then
    record FAIL "the target serves and the probe gets an answer" \
      "the target never reached Ready (its phase is ${phase:-unknown}) and its ${pods} Pod(s) are not serving"
  else
    record SKIP "the target serves and the probe gets an answer" \
      "the target never reached Ready (its phase is ${phase:-unknown}) and it has no Pod, so nothing was admitted"
  fi
  skip_rest "the target never reached Ready (its phase is ${phase:-unknown})"
  finish; exit 0
fi

# THE SERVED MODEL NAME. A request for a model the endpoint does not serve is refused with 404, so
# the name is the target's own unless the caller named one.
if [ -z "$MODEL" ]; then
  MODEL="$(target '.spec.model.name')"
  [ -n "$MODEL" ] || MODEL="/models/Qwen2.5-0.5B-Instruct"
fi
record PASS "the target serves and the probe gets an answer" \
  "the target ${MD} is Ready with the role ${ROLE} and the model ${MODEL}"

# THE PODS THE TARGET HAD BEFORE THE LOAD. The Pod that answers the load is the one that was
# born for it, and the Pods of the target are read here, before the scaler exists, because a
# Pod that has already come up by the time the load is over is a Pod the diff cannot name.
pods_before_load="$(ready_pods)"

# The URL the probe talks to. A routed deployment publishes its router, and the router is what
# spreads the requests, so the load has to go through it. The path is the chat endpoint, because
# the root of the router answers 404 and a 404 teaches the scaler nothing.
router="$(target '.spec.router.name')"
if [ -n "$router" ]; then
  PROBE_URL="${E2E_C129_PROBE_URL:-http://${MD}-router.${NS}.svc:8000/v1/chat/completions}"
else
  endpoint="$(target '.status.endpoint')"
  PROBE_URL="${E2E_C129_PROBE_URL:-http://${endpoint#*://}/v1/chat/completions}"
fi

# ---------------------------------------------------------------- the probe

kubectl -n "$NS" run "$PROBE" --image="$PROBE_IMAGE" --restart=Never \
  --labels="gpustack.ai/e2e-probe=${MD}" --command -- sleep 7200 >/dev/null 2>&1
if ! kubectl -n "$NS" wait "pod/$PROBE" --for=condition=Ready --timeout=300s >/dev/null 2>&1; then
  record SKIP "the target serves and the probe gets an answer" \
    "the probe Pod '${PROBE}' never became Ready on image '${PROBE_IMAGE}', so no request was sent"
  skip_rest "the probe Pod '${PROBE}' never became Ready on image '${PROBE_IMAGE}', so no request was sent"
  finish; exit 0
fi

# THE PROBE SCRIPT. It holds each phase for its whole duration: a round of concurrent requests
# that answers in a second is not a load, it is a burst, and a burst is over before the scaler has
# read anything. Every request is graded the way the serving cases grade one: a 200 with content
# and no error is an answer, anything else is a failure.
#
# THE SCRIPT AND THE PHASE LIST ARE WRITTEN THROUGH STDIN, and kubectl exec forwards stdin only
# with -i. Without it the remote cat reads EOF at once and both files land empty.
#
# shellcheck disable=SC2016  # the dollar names belong to the probe's shell, and must not expand
PROBE_SCRIPT='#!/bin/sh
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
  # THE WORKERS ARE CONTINUOUS, NOT A BARRIER PER ROUND. A client that waits for every request
  # of a round to finish before it sends the next one leaves the connection idle between rounds,
  # and an idle connection is one the server closes, so the next round starts on a closed
  # connection and the router answers it with a refusal. A client that keeps its workers busy is
  # also what a real client does, so the load this case measures is the load a real client puts
  # on the deployment.
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
          # THE FAILING BODY IS KEPT, WITH THE MOMENT IT FAILED, because a request that failed says
          # nothing about why, and a reader of the case needs the answer the server gave and the
          # time it gave it to tell a refusal from a timeout and a drop at a scale-up from a drop
          # that has nothing to do with one.
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

# THE PROBE FILES ARE COPIED, NOT FED THROUGH STDIN. kubectl does forward stdin to an exec, but
# it closes the stream as soon as the local stdin ends, and the shell in the Pod can read that
# close before it reads the bytes, so a file written that way arrives empty and the probe never
# runs at all. A copy carries the bytes with them.
#
# THE FILES ARE CHECKED AFTER THE COPY, because a file that did not arrive is a probe that does
# not run, and a case that waits for a phase that never starts reports the wrong thing.
probe_file="$(mktemp)"
phases_file="$(mktemp)"
printf '%s\n' "$PROBE_SCRIPT" >"$probe_file"
PHASE_LINES=""
for entry in $PHASES; do
  name="${entry%%:*}"
  rest="${entry#*:}"
  conc="${rest%%:*}"
  dur="${rest##*:}"
  PHASE_LINES="${PHASE_LINES}${name} ${conc} ${dur}
"
done
printf '%s' "$PHASE_LINES" >"$phases_file"

copied=0
kubectl -n "$NS" cp "$probe_file" "$PROBE:/tmp/probe.sh" >/dev/null 2>&1 && copied=$((copied + 1))
kubectl -n "$NS" cp "$phases_file" "$PROBE:/tmp/phases" >/dev/null 2>&1 && copied=$((copied + 1))
script_bytes="$(kubectl -n "$NS" exec "$PROBE" -- sh -c 'wc -c < /tmp/probe.sh' 2>/dev/null | tr -d ' \n')"
phase_bytes="$(kubectl -n "$NS" exec "$PROBE" -- sh -c 'wc -c < /tmp/phases' 2>/dev/null | tr -d ' \n')"
rm -f "$probe_file" "$phases_file"

if [ "${script_bytes:-0}" -gt 0 ] 2>/dev/null && [ "${phase_bytes:-0}" -gt 0 ] 2>/dev/null; then
  record PASS "the probe files reach the Pod" \
    "${copied} of 2 copies landed, the script is ${script_bytes} bytes and the phase list is ${phase_bytes} bytes"
else
  record FAIL "the probe files reach the Pod" \
    "the script is ${script_bytes:-0} bytes and the phase list is ${phase_bytes:-0} bytes in the Pod, so no request was sent"
  skip_rest "the probe files did not reach the Pod, so no request was sent"
  finish; exit 0
fi

# ---------------------------------------------------------------- the scaler

# THE SCALER IS CREATED BEFORE THE PROBE STARTS. Its recommendation is waiting when the load
# arrives, and the scale-up is then observed under load rather than after it.
kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeploymentScaler
metadata:
  name: ${SCALER}
  namespace: ${NS}
spec:
  targetRef:
    name: ${MD}
  mode: Active
  sampling:
    intervalSeconds: 10
  roles:
    - name: ${ROLE}
      minReplicas: 1
      maxReplicas: 2
  algorithm:
    type: Proportional
    proportional:
      metric: TotalTokensPerSecond
      tolerance: "0.1"
      roleTargets:
        - name: ${ROLE}
          target: "1"
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 0
EOF

# The probe runs inside the Pod and stays there, so the case can stop it by name. A probe started
# from the runner would die with the runner's connection, and the scale-down is the half of this
# case that needs the load to stop on purpose.
kubectl -n "$NS" exec "$PROBE" -- sh -c \
  "nohup sh /tmp/probe.sh '${PROBE_URL}' '${MODEL}' >/dev/null 2>&1 & echo started" >/dev/null 2>&1

# ---------------------------------------------------------------- the scale up

# The heavy phase is where the count has to rise.
wait_phase up "$STEP_BOUND" || true

replicas="$(role_replicas)"
deadline=$((SECONDS + STEP_BOUND))
while [ "$SECONDS" -lt "$deadline" ]; do
  replicas="$(role_replicas)"
  [ "${replicas:-0}" -ge 2 ] 2>/dev/null && break
  sleep 3
done
if [ "${replicas:-0}" -ge 2 ] 2>/dev/null; then
  record PASS "the target's replica count rises under load" \
    "the count of the role ${ROLE} is ${replicas} while the heavy phase runs"
else
  record FAIL "the target's replica count rises under load" \
    "the count of the role ${ROLE} stayed at ${replicas:-<none>}; the scaler read $(running_requests) running requests"
fi

running="$(running_requests)"
if [ -n "$running" ] && [ "$running" -gt 0 ] 2>/dev/null; then
  record PASS "the scaler reads the running requests of the target" \
    "the scaler published ${running} running requests for the role ${ROLE}"
else
  record FAIL "the scaler reads the running requests of the target" \
    "the scaler published ${running:-<none>} running requests for the role ${ROLE}, so the signal never arrived"
fi

# The second Pod has to serve, not merely exist: a Pod that never becomes Ready is a Pod the
# scaler counted as a replica it cannot use. The Pods that were Ready before the load are
# remembered, so the one that answers is the one that was born for the load.
new_pod=""
if wait_ready_count 2 "$READY_BOUND"; then
  new_pod="$(comm -13 <(printf '%s\n' "$pods_before_load") <(ready_pods) | head -1)"
fi
if [ -n "$new_pod" ]; then
  record PASS "the target's second Pod becomes Ready" \
    "the Pod ${new_pod} of the role ${ROLE} is Ready and is the one that was not there before the load"
else
  record FAIL "the target's second Pod becomes Ready" \
    "the role ${ROLE} has $(ready_pods | grep -c . || true) Ready Pod(s) and the Pods are $(ready_pods | tr '\n' ' '), and the Pods before the load were $(printf '%s\n' "$pods_before_load" | tr '\n' ' ')"
fi

# The new Pod's own counter is the evidence that the new replica took part: a count that rose
# while the load ran, on the Pod that was born for it.
#
# THE POD WAS BORN FOR THE LOAD, so any count on it is traffic it served, and a count above zero is
# that evidence. A second sample is taken as well, and a count that grows between the two is the
# stronger form of the same evidence, but the growth is not required: the router decides how the
# traffic is shared, and a router that keeps sending every request to the replica that was already
# there leaves the new Pod's count where it was without the new Pod having served nothing.
if [ -n "$new_pod" ]; then
  vendor="$(serving_vendor "$(kubectl -n "$NS" get "$MD_RESOURCE" "$MD" -o json 2>/dev/null)" 2>/dev/null || echo nvidia)"
  engine="$(target '.spec.engine.name')"
  if serving_profile "$vendor" "$engine" >/dev/null 2>&1; then
    counter_one="$(counter_of "$new_pod")"
    sleep 20
    counter_two="$(counter_of "$new_pod")"
    if [ -n "$counter_one" ] && [ -n "$counter_two" ] \
      && awk "BEGIN { exit !($counter_two > $counter_one) }"; then
      record PASS "the new Pod's own request counter grows under the load" \
        "the counter ${SP_SERVED} on the Pod ${new_pod} grew from ${counter_one} to ${counter_two}"
    elif [ -n "$counter_one" ] && [ "$counter_one" -gt 0 ] 2>/dev/null; then
      record PASS "the new Pod's own request counter grows under the load" \
        "the counter ${SP_SERVED} on the Pod ${new_pod} read ${counter_one} and then ${counter_two:-<none>}: the Pod was born for this load, so every request it counted is a request it served, and the router shared no more of the load with it"
    else
      record FAIL "the new Pod's own request counter grows under the load" \
        "the counter ${SP_SERVED} on the Pod ${new_pod} read ${counter_one:-<none>} and then ${counter_two:-<none>}"
    fi
  else
    record SKIP "the new Pod's own request counter grows under the load" \
      "the serving profile for vendor ${vendor} and engine ${engine} is unknown, so the counter to read is unknown"
  fi
else
  record SKIP "the new Pod's own request counter grows under the load" \
    "no second Pod of the role ${ROLE} became Ready, so there was no new Pod to read"
fi

committed="$(committed_vector)"
if [ -n "$committed" ] && [ "$committed" -ge 2 ] 2>/dev/null; then
  record PASS "the scaler records the vector it committed" \
    "the scaler committed ${ROLE}=${committed} on term $(scaler '.status.lastCommit.term')"
else
  record FAIL "the scaler records the vector it committed" \
    "the scaler committed ${committed:-<none>} for the role ${ROLE}"
fi

# ---------------------------------------------------------------- the scale down

# The scaler is narrowed while the load eases off, so the count has to come back down.
#
# THE CEILING STAYS WHERE THE COUNT IS, and the target is what moves. A scaler whose ceiling is
# below the count it holds is a scaler whose current vector is outside its own policy, and it
# holds its planning rather than bringing the count down, so narrowing the ceiling to one while
# the count is two would ask the scaler to do the one thing it refuses to do.
#
# THE WHOLE ALGORITHM IS NAMED IN THE PATCH, because a merge patch replaces the object it names:
# a patch that carried only the targets would leave the algorithm without a metric, and the
# scaler would plan on a signal nobody asked for.
#
# THE SCALE-DOWN WINDOW IS FIFTEEN SECONDS, NOT ZERO AND NOT ONE. A zero is not "no window": the
# webhook fills an unset window with its default of three hundred seconds, and a window that long
# holds the count at its high-water mark for as long as the load was high. A window shorter than
# the sampling interval does not work either: the solver refuses to act when the history of its
# own recommendations does not span the window, and a window shorter than the interval between
# two samples is never spanned. Fifteen seconds is longer than the ten seconds this scaler
# samples at, and short enough that the count comes down while the case is waiting for it.
wait_phase down "$STEP_BOUND" || true

narrowed="$(kubectl -n "$NS" patch "$SCALER_RESOURCE" "$SCALER" --type=merge \
  -p "{\"spec\":{\"algorithm\":{\"proportional\":{\"metric\":\"TotalTokensPerSecond\",\"tolerance\":\"0.1\",\"roleTargets\":[{\"name\":\"${ROLE}\",\"target\":\"1000000\"}]}},\"behavior\":{\"scaleDown\":{\"stabilizationWindowSeconds\":15,\"decodePostScaleUpGraceSeconds\":1}}}}" \
  2>&1 | tr '\n' ' ')"

# THE COUNT FALLS BEFORE THE RETIREMENT ENDS. The reservation is written when the vector is
# committed and cleared when the retirement settles, so the count reaching one is not the end of
# the story: the states between the two are what a reader of the retirement protocol watches, and
# they are collected until the reservation is gone and one Pod is left.
states=""
deadline=$((SECONDS + RETIRE_BOUND))
while [ "$SECONDS" -lt "$deadline" ]; do
  state="$(target '.status.retirement.state // ""')"
  if [ -n "$state" ]; then
    case "$states" in
      *"$state"*) ;;
      *) states="${states}${states:+,}${state}" ;;
    esac
  fi
  replicas="$(role_replicas)"
  ready_left="$(ready_pods | grep -c . || true)"
  if [ "${replicas:-2}" -le 1 ] 2>/dev/null && [ -z "$state" ] && [ "${ready_left:-0}" -le 1 ]; then
    break
  fi
  sleep 2
done

replicas="$(role_replicas)"
if [ "${replicas:-2}" -le 1 ] 2>/dev/null; then
  record PASS "the replica count falls when the scaler is narrowed" \
    "the count of the role ${ROLE} is ${replicas} after the scaler was narrowed"
else
  record FAIL "the replica count falls when the scaler is narrowed" \
    "the count of the role ${ROLE} stayed at ${replicas:-<none>} after the scaler was narrowed (${narrowed:0:160})"
fi

if [ -n "$states" ]; then
  record PASS "the retirement that carried the count down is observed" \
    "the target carried the retirement states ${states}"
else
  record FAIL "the retirement that carried the count down is observed" \
    "the count fell to ${replicas:-<none>} but the target carried no retirement state, so the count went down without one"
fi

# One Pod is left serving.
wait_phase one "$STEP_BOUND" || true
# THE LAST PHASE IS WAITED FOR TOO. A phase that has started has not answered yet, and the
# result of a phase that is still running reads as a phase that answered nothing.
wait_phase_done one "$STEP_BOUND" || true
ready_left="$(ready_pods | grep -c . || true)"
if [ "${ready_left:-0}" -eq 1 ]; then
  record PASS "one Ready Pod is left after the retirement" \
    "the role ${ROLE} has one Ready Pod, $(ready_pods | tr '\n' ' ')"
else
  record FAIL "one Ready Pod is left after the retirement" \
    "the role ${ROLE} has ${ready_left:-<none>} Ready Pod(s) after the retirement"
fi

# ---------------------------------------------------------------- the phases

# The first failing body of a phase, or empty. A request that failed says nothing about why
# without the answer the server gave, and the answer is what tells a refusal from a timeout.
bad_body() { # <phase>
  kubectl -n "$NS" exec "$PROBE" -- sh -c "head -1 /tmp/badbody.$1 2>/dev/null | cut -c1-180" 2>/dev/null
}

# Every phase has to have answered and none may have failed. A phase that answered nothing is a
# phase the scaler never saw, and a phase that failed is a phase the deployment could not serve,
# and neither is a state this case is willing to call a pass.
phase_failures=""
phase_detail=""
phase_evidence=""
for entry in $PHASES; do
  name="${entry%%:*}"
  case "$name" in
    base) continue ;;
  esac
  result="$(phase_result "$name")"
  ok="${result%% *}"
  bad="${result##* }"
  [ -n "$ok" ] || ok=0
  [ -n "$bad" ] || bad=0
  phase_detail="${phase_detail}${name}=${ok}/${bad} "
  if [ "$ok" -le 0 ] 2>/dev/null || [ "$bad" -ne 0 ] 2>/dev/null; then
    phase_failures="${phase_failures}${name} "
    phase_evidence="${phase_evidence}${name}: $(bad_body "$name") "
  fi
done
if [ -z "$phase_failures" ]; then
  record PASS "every phase answers requests and fails none" "the phases answered ${phase_detail}"
else
  record FAIL "every phase answers requests and fails none" \
    "the phases ${phase_failures} did not answer without a failure, and the phases answered ${phase_detail} ${phase_evidence}"
fi

# Results.
finish
