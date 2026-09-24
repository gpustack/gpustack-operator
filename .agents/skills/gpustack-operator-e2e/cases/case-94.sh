#!/usr/bin/env bash
#
# CASE 94 — Scaling the router or a role under continuous traffic drops no request and puts the new
#           Pod to work
#   (MUTATING, self-recovering)
#
# case-94.sh <NS> <MODEL_DEPLOYMENT> <router|prefill|decode|server>
#
# Goal:        A replica added while requests keep arriving is found by the router and does real
#              work, and removing it again fails no request in flight. Final replica counts alone
#              say nothing about either, so the rows read the traffic and the new Pod's own
#              counters: a router Pod's request counter, a prefill Pod's sent transfers, a decode
#              Pod's received blocks, a server Pod's finished requests. Where each lives per vendor,
#              engine and router is _serving-lib.sh's.
# Environment: An already Ready ModelDeployment with a router, whose target has one replica: the
#              router, or the one role of the named kind. Room for one more replica of it — one
#              more accelerator for a role. Real accelerators are required. E2E_PD_PROBE_IMAGE
#              names a pullable curl image; E2E_PD_MODEL names the served model;
#              E2E_SCALE_READY_BOUND (default 900) bounds the new Pod's start, E2E_SCALE_SETTLE
#              (default 60) is how long traffic runs against each shape before it is read.
#              E2E_SCALE_LOG_DIR, when set, receives the new Pod's logs before the scale-down removes
#              them, which is where a router records the endpoints it found.
# Inputs:      All real. A temporary probe Pod sends sequential chat completions through the
#              router the whole time, alternating streaming and not, each with a unique tail.
# Expected:    From one to two, the new Pod becomes Ready and its own counter grows while no
#              request fails; from two back to one, no request fails and one Pod of the target
#              remains Ready; restart counts of the Pods that stay do not move. The request totals
#              per phase and each failed request's status and body are printed as INFO rows. A new
#              server behind a router whose cache-aware policy keeps prefix-sharing traffic on one
#              worker may stay idle; that row SKIPs naming the policy instead of failing.
# Cleanup:     A trap stops the traffic, restores the target's replica count and removes the probe
#              Pod, on pass and failure.
set -uo pipefail

NS="${1:-}"
MD="${2:-}"
TARGET="${3:-}"
IMAGE="${E2E_PD_PROBE_IMAGE:-}"
MODEL="${E2E_PD_MODEL:-}"
READY_BOUND="${E2E_SCALE_READY_BOUND:-900}"
SETTLE="${E2E_SCALE_SETTLE:-60}"
if [ -z "$NS" ] || [ -z "$MD" ] || [ -z "$IMAGE" ] || [ -z "$MODEL" ] ||
   [[ ! "$TARGET" =~ ^(router|prefill|decode|server)$ ]]; then
  echo "usage: E2E_PD_PROBE_IMAGE=<curl-image> E2E_PD_MODEL=<model> case-94.sh <NS> <MODEL_DEPLOYMENT> <router|prefill|decode|server>" >&2
  exit 2
fi
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_serving-lib.sh"

md_json="$(kubectl -n "$NS" get modeldeployment "$MD" -o json)" || exit 2
ROUTER="$(jq -r '.spec.router.name // empty' <<<"$md_json")"
[ -n "$ROUTER" ] || { echo "case 94 needs a routed ModelDeployment" >&2; exit 2; }
[ "$(jq -r '.status.phase' <<<"$md_json")" = Ready ] || { echo "$MD is not Ready" >&2; exit 2; }
ENGINE="$(jq -r '.spec.engine.name' <<<"$md_json")"
VENDOR="$(serving_vendor "$md_json")" || { echo "cannot read the deployment's vendor" >&2; exit 2; }
serving_profile "$VENDOR" "$ENGINE" || exit 2
SPLIT="$(jq -r 'any(.spec.roles[]; .kind == "prefill")' <<<"$md_json")"
if [ "$TARGET" = router ]; then
  [ "$(jq -r '.spec.router.replicas // 1' <<<"$md_json")" = 1 ] || { echo "the router does not run one replica" >&2; exit 2; }
  PATCH_PATH=/spec/router/replicas
  SELECTOR="app.kubernetes.io/instance=$MD,modeldeployment.gpustack.ai/router=$ROUTER"
  COUNTER="$(serving_router_requests "$ROUTER" "$SPLIT")"
else
  index="$(jq -r --arg k "$TARGET" '[.spec.roles | to_entries[] | select(.value.kind == $k) | .key] |
    if length == 1 then .[0] else empty end' <<<"$md_json")"
  [ -n "$index" ] || { echo "case 94 needs exactly one role of kind $TARGET" >&2; exit 2; }
  [ "$(jq -r ".spec.roles[$index].replicas" <<<"$md_json")" = 1 ] || { echo "the $TARGET role does not run one replica" >&2; exit 2; }
  PATCH_PATH="/spec/roles/$index/replicas"
  SELECTOR="app.kubernetes.io/instance=$MD,app.kubernetes.io/component=$(jq -r ".spec.roles[$index].name" <<<"$md_json")"
  case "$TARGET" in
    prefill) COUNTER="$SP_SENT_COUNT" ;;
    decode) COUNTER="$SP_RECEIVED" ;;
    server) COUNTER="$SP_SERVED" ;;
  esac
fi
if [ -z "$COUNTER" ]; then
  echo "no counter shows a new $TARGET Pod's work for $ENGINE on $VENDOR behind $ROUTER" >&2
  exit 2
fi

scale() {
  kubectl -n "$NS" patch modeldeployment "$MD" --type=json \
    -p "[{\"op\":\"add\",\"path\":\"$PATCH_PATH\",\"value\":$1}]" >/dev/null
}
ready_pods() {
  kubectl -n "$NS" get pods -l "$SELECTOR" -o json | jq -r '.items[] | select(.metadata.deletionTimestamp == null and
    any(.status.conditions[]?; .type == "Ready" and .status == "True")) | .metadata.name' | sort
}
wait_ready_count() {
  local deadline=$((SECONDS + $2))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(ready_pods | grep -c .)" -eq "$1" ] &&
      [ "$(kubectl -n "$NS" get pods -l "$SELECTOR" --no-headers 2>/dev/null | grep -c .)" -eq "$1" ] && return 0
    sleep 5
  done
  return 1
}

PROBE="case94-scale-$$"
TRAFFIC=/tmp/case94-traffic
cleanup() {
  kubectl -n "$NS" exec "$PROBE" -- touch "$TRAFFIC.stop" >/dev/null 2>&1
  scale 1 2>/dev/null
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT
kubectl -n "$NS" run "$PROBE" --image="$IMAGE" --restart=Never --command -- sleep 3600 >/dev/null || exit 2
kubectl -n "$NS" wait "pod/$PROBE" --for=condition=Ready --timeout=120s >/dev/null || exit 2

# One line per request in the probe Pod: "<epoch> <phase> ok|BAD". The phase is whatever the case
# last wrote to $TRAFFIC.phase, so each request is attributed to the shape it ran against. A stream
# counts only with generated content and no error event: a streamed failure still ends in [DONE].
kubectl -n "$NS" exec "$PROBE" -- sh -c "echo base >$TRAFFIC.phase; nohup sh -c '
  i=0
  while [ ! -e $TRAFFIC.stop ]; do
    i=\$((i + 1)); s=false; [ \$((i % 2)) -eq 0 ] && s=true
    b=\"{\\\"model\\\":\\\"$MODEL\\\",\\\"messages\\\":[{\\\"role\\\":\\\"user\\\",\\\"content\\\":\\\"Explain how a controller reconciles state. Case 94 question \$i \$(date +%s%N).\\\"}],\\\"max_tokens\\\":32,\\\"stream\\\":\$s}\"
    c=\$(curl -sS --max-time 60 -o /tmp/case94-r -w %{http_code} -H Content-Type:application/json --data-binary \"\$b\" http://$MD-router.$NS.svc:8081/v1/chat/completions 2>/dev/null)
    v=\"BAD \$c \$(head -c 200 /tmp/case94-r | tr -d \"\\n\")\"
    if [ \"\$c\" = 200 ] && grep -q \"\\\"content\\\": *\\\"[^\\\"]\" /tmp/case94-r && ! grep -q \"\\\"error\\\"\" /tmp/case94-r; then v=ok; fi
    echo \"\$(date +%s) \$(cat $TRAFFIC.phase) \$v\" >>$TRAFFIC.log
  done' >/dev/null 2>&1 &" || exit 2
phase() { kubectl -n "$NS" exec "$PROBE" -- sh -c "echo $1 >$TRAFFIC.phase"; }
tally() {
  kubectl -n "$NS" exec "$PROBE" -- sh -c "cat $TRAFFIC.log 2>/dev/null" |
    awk -v p="$1" -v want="$2" '$2 == p && $3 == want { n++ } END { print n + 0 }'
}
counter_of() {
  local ip port scheme
  ip="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.podIP}')"
  port="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.annotations.prometheus\.io/port}')"
  scheme="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.annotations.prometheus\.io/scheme}')"
  serving_sum "$(kubectl -n "$NS" exec "$PROBE" -- curl -fsS --max-time 10 \
    "${scheme:-http}://$ip:$port/metrics" 2>/dev/null)" "$COUNTER" || echo 0
}

fails=0
check() {
  if [ "$1" = true ]; then
    printf 'PASS | %s | %s\n' "$2" "$MD"
  else
    printf 'FAIL | %s | %s\n' "$2" "$MD"
    fails=$((fails + 1))
  fi
}
restarts() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=$MD" -o json |
    jq -c '[.items[] | {key: .metadata.name, value: ([.status.containerStatuses[]?.restartCount] | add // 0)}] | from_entries'
}

sleep "$SETTLE"
old="$(ready_pods)"
before_restarts="$(restarts)"
phase up
scale 2
if ! wait_ready_count 2 "$READY_BOUND"; then
  check false "a second $TARGET Pod becomes Ready within ${READY_BOUND}s"
  exit 1
fi
new="$(comm -13 <(printf '%s\n' "$old") <(ready_pods) | head -1)"
check "$([ -n "$new" ] && echo true || echo false)" "a second $TARGET Pod becomes Ready ($new)"
phase two
sleep "$SETTLE"
work="$(counter_of "$new")"
concentrates=""
[ "$TARGET" = server ] && concentrates="$(serving_router_concentrates "$ROUTER")"
if [ -n "$concentrates" ] && awk -v w="$work" 'BEGIN { exit !(w == 0) }'; then
  printf 'SKIP | the new %s Pod did work (%s=0 on %s): %s | %s\n' "$TARGET" "$COUNTER" "$new" "$concentrates" "$MD"
else
  check "$(awk -v w="$work" 'BEGIN { print (w > 0) ? "true" : "false" }')" \
    "the new $TARGET Pod did work ($COUNTER=$work on $new)"
fi
if [ -n "${E2E_SCALE_LOG_DIR:-}" ]; then
  mkdir -p "$E2E_SCALE_LOG_DIR"
  kubectl -n "$NS" logs "$new" --all-containers --prefix >"$E2E_SCALE_LOG_DIR/$new.log" 2>&1
  printf 'INFO | %s logs saved: %s lines | %s\n' "$new" "$(wc -l <"$E2E_SCALE_LOG_DIR/$new.log" | tr -d ' ')" "$MD"
fi
phase down
scale 1
wait_ready_count 1 "$READY_BOUND"
check "$([ "$(ready_pods | grep -c .)" -eq 1 ] && echo true || echo false)" "one $TARGET Pod remains Ready"
phase one
sleep "$SETTLE"
kubectl -n "$NS" exec "$PROBE" -- touch "$TRAFFIC.stop"
sleep 5
for p in base up two down one; do
  printf 'INFO | %s: %s ok, %s failed | %s\n' "$p" "$(tally "$p" ok)" "$(tally "$p" BAD)" "$MD"
done
# Each failed request's epoch, phase, status and the head of its body, since the probe goes with the trap.
kubectl -n "$NS" exec "$PROBE" -- sh -c "grep ' BAD ' $TRAFFIC.log" 2>/dev/null | head -20 |
  while IFS= read -r line; do printf 'INFO | failed request: %s | %s\n' "$line" "$MD"; done
for p in up two; do
  check "$([ "$(tally "$p" BAD)" -eq 0 ] && [ "$(tally "$p" ok)" -gt 0 ] && echo true || echo false)" \
    "no request failed while scaling up ($p)"
done
for p in down one; do
  check "$([ "$(tally "$p" BAD)" -eq 0 ] && [ "$(tally "$p" ok)" -gt 0 ] && echo true || echo false)" \
    "no request failed while scaling down ($p)"
done
# A Pod the scale-down removed has no restart count left, and the Pod it added had none before, so
# the check covers the Pods present at both ends.
after_restarts="$(restarts)"
check "$(jq -rn --argjson a "$before_restarts" --argjson b "$after_restarts" \
  '[$a | keys[] | select($b[.] != null) | $a[.] == $b[.]] | all')" \
  "restart counts of the Pods present throughout unchanged"
[ "$fails" -eq 0 ]
