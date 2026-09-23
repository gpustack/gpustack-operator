#!/usr/bin/env bash
#
# CASE 92 — SGLang P/D with a store serves a request over a pinned TCP leg and writes the store.
#
# case-92.sh <NS> <MODEL_DEPLOYMENT>
#
# Goal:        A routed request completes on an SGLang prefill/decode pair whose halves sit on two
#              hosts without RDMA, with the leg pinned to TCP by the operator's mooncake_tcp, the
#              decode half keeping retractions in CPU tensors, and the prefill half's hierarchical
#              cache writing the bound store.
# Environment: An already Ready SGLang ModelDeployment with one prefill and one decode role on two
#              different nodes, a bound KVCache Store whose transport is tcp, and a router Service.
#              Every store backend names a spec.image on the 0.3.12 line, the client minor SGLang
#              0.5.18 embeds; another minor fails every write and is not evidence against this
#              case. Real GPUs are required, and the prefill node needs available host memory above
#              SGLang's fixed 10 GiB reserve plus its host pool. E2E_PD_PROBE_IMAGE names a
#              pullable curl image; E2E_PD_MODEL names the model served by the deployment.
# Inputs:      All real. No role declares MC_FORCE_TCP or either cache switch itself, so each is the
#              operator's rendering. A temporary probe Pod sends one chat completion.
# Expected:    Both halves render mooncake_tcp and log the TCP-only selection; the decode half runs
#              retraction backup cpu_tensor and no hierarchical cache, the prefill half runs the
#              hierarchical cache; neither logs a host-memory refusal; the request returns content;
#              the store master holds allocated bytes or counted a put; restart counts stay fixed.
# Cleanup:     A trap removes the temporary probe Pod on pass and failure.
set -uo pipefail

NS="${1:-}"
MD="${2:-}"
IMAGE="${E2E_PD_PROBE_IMAGE:-}"
MODEL="${E2E_PD_MODEL:-}"
if [ -z "$NS" ] || [ -z "$MD" ] || [ -z "$IMAGE" ] || [ -z "$MODEL" ]; then
  echo "usage: E2E_PD_PROBE_IMAGE=<curl-image> E2E_PD_MODEL=<model> case-92.sh <NS> <MODEL_DEPLOYMENT>" >&2
  exit 2
fi
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }

md_json="$(kubectl -n "$NS" get modeldeployment "$MD" -o json)" || exit 2
if ! jq -e '.spec.engine.name == "sglang" and .spec.kvCache.poolRef.name != null and
  ([.spec.roles[] | select(.kind == "prefill")] | length) == 1 and
  ([.spec.roles[] | select(.kind == "decode")] | length) == 1 and
  .spec.router.name != null and (.spec.kvTransfer.protocol // "tcp") == "tcp"' <<<"$md_json" >/dev/null; then
  echo "case 92 needs an SGLang Store plus P/D ModelDeployment with a router and a tcp leg" >&2
  exit 2
fi
if jq -e 'any(.spec.roles[]; any(.env[]?; .name == "MC_FORCE_TCP") or
  any(.extraArgs[]?; startswith("--enable-hierarchical-cache") or
    startswith("--disaggregation-decode-retraction-backup")))' <<<"$md_json" >/dev/null; then
  echo "a role declares the pin or a cache switch itself, so the Pod cannot show what the operator rendered" >&2
  exit 2
fi
pool="$(kubectl get kvcachepool "$(jq -r '.spec.kvCache.poolRef.name' <<<"$md_json")" -o json)" || exit 2
for backend in $(jq -r '.spec.backends[]' <<<"$pool"); do
  store_image="$(kubectl get kvcachebackend "$backend" -o jsonpath='{.spec.image}')" || exit 2
  [[ "$store_image" == *:0.3.12* ]] ||
    { echo "backend $backend runs '$store_image', not the 0.3.12 line SGLang 0.5.18's client speaks" >&2; exit 2; }
done
endpoint="$(jq -r '.status.clientEndpoint // empty' <<<"$pool")"
[ -n "$endpoint" ] || { echo "the pool publishes no client endpoint" >&2; exit 2; }
store_url="http://${endpoint%:*}:9003/metrics"

pods="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=$MD" -o json)" || exit 2
ready_pod() {
  jq -r --arg kind "$1" '[.items[] | select(.metadata.labels["app.kubernetes.io/component"] == $kind and
    any(.status.conditions[]?; .type == "Ready" and .status == "True"))] |
    if length == 1 then .[0].metadata.name else empty end' <<<"$pods"
}
prefill="$(ready_pod prefill)"
decode="$(ready_pod decode)"
if [ -z "$prefill" ] || [ -z "$decode" ]; then
  echo "exactly one Ready prefill and one Ready decode Pod are required" >&2
  exit 2
fi
pod_field() {
  jq -r --arg name "$1" ".items[] | select(.metadata.name == \$name) | $2" <<<"$pods"
}
if [ "$(pod_field "$prefill" .spec.nodeName)" = "$(pod_field "$decode" .spec.nodeName)" ]; then
  echo "prefill and decode share a node, so the leg would not cross hosts" >&2
  exit 2
fi
if [ "$(pod_field "$prefill" '.spec.containers[] | select(.name == "main") | .env[]? |
  select(.name == "MOONCAKE_PROTOCOL") | .value')" != "tcp" ]; then
  echo "case 92 needs a tcp store; the pin is withheld beside any other" >&2
  exit 2
fi
before_restarts="$(jq '[.items[].status.containerStatuses[]?.restartCount] | add // 0' <<<"$pods")"

PROBE="case92-transfer-$$"
cleanup() { kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1; }
trap cleanup EXIT
kubectl -n "$NS" run "$PROBE" --image="$IMAGE" --restart=Never --command -- sleep 600 >/dev/null || exit 2
kubectl -n "$NS" wait "pod/$PROBE" --for=condition=Ready --timeout=120s >/dev/null || exit 2

# read_metric URL NAME sums every series of NAME and fails when none is exposed.
read_metric() {
  kubectl -n "$NS" exec "$PROBE" -- curl -fsS --max-time 10 "$1" |
    awk -v name="$2" 'index($1, name) == 1 && (length($1) == length(name) ||
      substr($1, length(name) + 1, 1) == "{") { total += $2; found = 1 }
      END { if (!found) exit 1; printf "%.0f\n", total }'
}
put_before="$(read_metric "$store_url" master_put_end_requests_total || echo 0)"
payload="$(jq -nc --arg model "$MODEL" '{model:$model,messages:[{role:"user",content:
  "Explain how a Kubernetes controller reconciles a desired state."}],max_tokens:64}')"
response="$(kubectl -n "$NS" exec "$PROBE" -- curl -sS --max-time 60 \
  -w '\nHTTP_STATUS:%{http_code}\n' -H 'Content-Type: application/json' \
  --data-binary "$payload" "http://$MD-router.$NS.svc:8081/v1/chat/completions")"
request_exit=$?
content="$(sed '/^HTTP_STATUS:/d' <<<"$response" | jq -r '.choices[0].message.content // empty' 2>/dev/null)"
put_after="$(read_metric "$store_url" master_put_end_requests_total || echo 0)"
allocated="$(read_metric "$store_url" master_allocated_bytes || echo 0)"
pods_after="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=$MD" -o json)" || exit 2
after_restarts="$(jq '[.items[].status.containerStatuses[]?.restartCount] | add // 0' <<<"$pods_after")"
# Recorded for the reader, not asserted: the host pool is already built, so what is available now
# is not what SGLang checked. The file is read inside the engine, where it is host-wide.
mem_available="$(kubectl -n "$NS" exec "$prefill" -c main -- cat /proc/meminfo 2>/dev/null |
  awk '/^MemAvailable:/ { print $2 }')"
printf 'INFO | prefill node MemAvailable after startup: %s kB | %s\n' "${mem_available:-unread}" "$MD"

fails=0
check() {
  if [ "$1" = true ]; then
    printf 'PASS | %s | %s\n' "$2" "$MD"
  else
    printf 'FAIL | %s | %s\n' "$2" "$MD"
    fails=$((fails + 1))
  fi
}
# has_arg POD FLAG [VALUE] reports whether the main container's argv carries FLAG, followed by VALUE
# when one is given, in either spelling.
has_arg() {
  pod_field "$1" '.spec.containers[] | select(.name == "main") | (.command // []) + (.args // [])' |
    jq -r --arg flag "$2" --arg value "${3:-}" '. as $argv | [range(length) as $i |
      select($argv[$i] == $flag and ($value == "" or $argv[$i + 1] == $value)) ,
      select($value != "" and $argv[$i] == ($flag + "=" + $value))] | length > 0'
}
for pod in "$prefill" "$decode"; do
  check "$(has_arg "$pod" --disaggregation-transfer-backend mooncake_tcp)" \
    "$pod renders transfer backend mooncake_tcp"
  logs="$(kubectl -n "$NS" logs "$pod" -c main 2>/dev/null)"
  check "$([[ "$logs" == *'MC_FORCE_TCP is set'* ]] && echo true || echo false)" \
    "$pod logs the TCP-only transport selection"
  check "$([[ "$logs" != *'Not enough host memory'* ]] && echo true || echo false)" \
    "$pod logs no host-memory refusal"
done
decode_logs="$(kubectl -n "$NS" logs "$decode" -c main 2>/dev/null)"
check "$([[ "$decode_logs" == *"disaggregation_decode_retraction_backup='cpu_tensor'"* ]] && echo true || echo false)" \
  "decode runs retraction backup cpu_tensor"
check "$([ "$(has_arg "$decode" --enable-hierarchical-cache)" = false ] && echo true || echo false)" \
  "decode renders no hierarchical cache"
check "$(has_arg "$prefill" --enable-hierarchical-cache)" "prefill renders the hierarchical cache"
check "$([ "$request_exit" -eq 0 ] && [[ "$response" == *'HTTP_STATUS:200'* ]] && [ -n "$content" ] &&
  echo true || echo false)" "routed P/D request returned content"
check "$(awk -v a="$allocated" -v b="$put_before" -v p="$put_after" \
  'BEGIN { print (a > 0 || p > b) ? "true" : "false" }')" "the store master holds or counted a write"
check "$([ "$after_restarts" -eq "$before_restarts" ] && echo true || echo false)" \
  "engine restart count unchanged"
[ "$fails" -eq 0 ]
