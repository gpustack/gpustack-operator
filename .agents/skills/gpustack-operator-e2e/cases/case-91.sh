#!/usr/bin/env bash
#
# CASE 91 — A routed prefill/decode pair moves real KV bytes over its direct leg, and writes the
#           store when it has one.
#
# case-91.sh <NS> <MODEL_DEPLOYMENT>
#
# Goal:        Every routed request completes while the prefill half counts transfers it sent, the
#              decode half counts blocks it was handed, and no half counts a failed transfer — on
#              the transport the operator pinned rather than one the manifest set. With a store
#              bound, the requests also write it. Engine, store and router are read off the object;
#              where the evidence lives per vendor and engine is _serving-lib.sh's serving_profile,
#              so the same rows run on vLLM and SGLang, with or without a store.
# Environment: An already Ready ModelDeployment with one prefill and one decode role, one Ready Pod
#              each, on two different nodes, and a router Service. On NVIDIA the leg is tcp and the
#              engine runs vLLM E2E_PD_VLLM_VERSION (default 0.29.0), whose Mooncake client reads
#              MC_FORCE_TCP, or SGLang; the vLLM transfer series come from the gpustack runner image,
#              so an upstream vLLM image fails the transfer rows by carrying none. A bound store must
#              run on tcp and on the client's minor line; the store image is printed, not checked.
#              An SGLang store needs the prefill node's available host memory above SGLang's fixed
#              10 GiB reserve plus its host pool. Real accelerators are required.
#              E2E_PD_PROBE_IMAGE names a pullable curl image; E2E_PD_MODEL names the served model.
# Inputs:      All real. No role declares MC_FORCE_TCP or an SGLang cache switch itself, so each is
#              the operator's rendering. A temporary probe Pod sends E2E_PD_REQUESTS (default 20)
#              chat completions, alternating streaming and not, each prompt ending in a unique tail
#              so a store that already holds the shared prefix still takes new blocks.
# Expected:    Where the vendor pins the leg, both halves render the pin once and log it, and vLLM
#              logs no cross-node NVLink selection; every request completes; the prefill half's
#              transfer samples grew and their sum is positive; the decode half's received count
#              grew; no half counts a failed transfer; with a store, the master's batch-put item
#              count grew, and an SGLang pair runs the hierarchical cache on the prefill half only,
#              the decode half backs retractions with CPU tensors, and neither logs a host-memory
#              refusal; restart counts stay fixed. Image IDs are printed as INFO rows.
# Cleanup:     A trap removes the temporary probe Pod on pass and failure.
set -uo pipefail

NS="${1:-}"
MD="${2:-}"
IMAGE="${E2E_PD_PROBE_IMAGE:-}"
MODEL="${E2E_PD_MODEL:-}"
VERSION="${E2E_PD_VLLM_VERSION:-0.29.0}"
REQUESTS="${E2E_PD_REQUESTS:-20}"
if [ -z "$NS" ] || [ -z "$MD" ] || [ -z "$IMAGE" ] || [ -z "$MODEL" ]; then
  echo "usage: E2E_PD_PROBE_IMAGE=<curl-image> E2E_PD_MODEL=<model> case-91.sh <NS> <MODEL_DEPLOYMENT>" >&2
  exit 2
fi
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_serving-lib.sh"

md_json="$(kubectl -n "$NS" get modeldeployment "$MD" -o json)" || exit 2
if ! jq -e '([.spec.roles[] | select(.kind == "prefill")] | length) == 1 and
  ([.spec.roles[] | select(.kind == "decode")] | length) == 1 and
  .spec.router.name != null' <<<"$md_json" >/dev/null; then
  echo "case 91 needs a prefill/decode ModelDeployment with a router" >&2
  exit 2
fi
ENGINE="$(jq -r '.spec.engine.name' <<<"$md_json")"
VENDOR="$(serving_vendor "$md_json")" || { echo "cannot read the deployment's vendor" >&2; exit 2; }
serving_profile "$VENDOR" "$ENGINE" || exit 2
ROUTER="$(jq -r '.spec.router.name' <<<"$md_json")"
[[ " $SP_ROUTERS " == *" $ROUTER "* ]] ||
  { echo "router $ROUTER renders no transfer leg for $ENGINE on $VENDOR (one of: $SP_ROUTERS)" >&2; exit 2; }
STORE="$(jq -r '.spec.kvCache.poolRef.name // empty' <<<"$md_json")"
if [ "$ENGINE" = vllm ] && [ "$SP_PIN" != none ]; then
  # The version is read off what each role runs: the engine version for a synthesized image, the
  # image reference for a role that names its own.
  if ! jq -e --arg v "$VERSION" '.spec.engine.version as $ev | all(.spec.roles[];
    if .image then (.image | contains("-vllm" + $v)) else $ev == $v end)' <<<"$md_json" >/dev/null; then
    echo "case 91 needs every role on vLLM $VERSION; older CUDA images embed a Mooncake client without MC_FORCE_TCP" >&2
    exit 2
  fi
fi
if [ "$SP_PIN" != none ] && ! jq -e '(.spec.kvTransfer.protocol // "tcp") == "tcp"' <<<"$md_json" >/dev/null; then
  echo "case 91 needs the direct leg on tcp" >&2
  exit 2
fi
if jq -e 'any(.spec.roles[]; any(.env[]?; .name == "MC_FORCE_TCP") or
  any(.extraArgs[]?; startswith("--enable-hierarchical-cache") or
    startswith("--disaggregation-decode-retraction-backup")))' <<<"$md_json" >/dev/null; then
  echo "a role declares the pin or a cache switch itself, so the Pod cannot show what the operator rendered" >&2
  exit 2
fi

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
metrics_url() {
  local ip port scheme
  ip="$(pod_field "$1" .status.podIP)"
  port="$(pod_field "$1" '.metadata.annotations["prometheus.io/port"] // empty')"
  scheme="$(pod_field "$1" '.metadata.annotations["prometheus.io/scheme"] // "http"')"
  [[ "$port" =~ ^[0-9]+$ ]] || return 1
  echo "$scheme://$ip:$port/metrics"
}
prefill_url="$(metrics_url "$prefill")" || { echo "prefill has no advertised metrics port" >&2; exit 2; }
decode_url="$(metrics_url "$decode")" || { echo "decode has no advertised metrics port" >&2; exit 2; }
store_url=""
if [ -n "$STORE" ]; then
  # A pin is process-wide, so it is withheld beside a store on any other transport.
  protocol="$(pod_field "$prefill" '((.metadata.annotations["kvcache.gpustack.ai/client-config"] // "{}" |
    fromjson | .protocol) // ([.spec.containers[] | select(.name == "main") | .env[]? |
    select(.name == "MOONCAKE_PROTOCOL") | .value][0])) // empty')"
  [ "$protocol" = tcp ] || { echo "case 91 needs a tcp store; the pin is withheld beside any other" >&2; exit 2; }
  # poolRef names the namespace's KVCachePoolBinding; the cluster-scoped pool is behind it.
  pool_name="$(kubectl -n "$NS" get kvcachepoolbinding "$STORE" -o jsonpath='{.spec.poolRef.name}')" || exit 2
  pool="$(kubectl get kvcachepool "$pool_name" -o json)" || exit 2
  endpoint="$(jq -r '.status.clientEndpoint // empty' <<<"$pool")"
  [ -n "$endpoint" ] || { echo "the pool publishes no client endpoint" >&2; exit 2; }
  store_url="http://${endpoint%:*}:9003/metrics"
  for backend in $(jq -r '.spec.backends[]' <<<"$pool"); do
    printf 'INFO | store backend %s image: %s | %s\n' "$backend" \
      "$(kubectl get kvcachebackend "$backend" -o jsonpath='{.spec.image}' 2>/dev/null)" "$MD"
  done
fi
jq -r '.items[] | .metadata.name as $p | .status.containerStatuses[]? |
  "INFO | \($p) \(.name) runs \(.imageID) | '"$MD"'"' <<<"$pods"
before_restarts="$(jq '[.items[].status.containerStatuses[]?.restartCount] | add // 0' <<<"$pods")"

PROBE="case91-transfer-$$"
cleanup() { kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1; }
trap cleanup EXIT
kubectl -n "$NS" run "$PROBE" --image="$IMAGE" --restart=Never --command -- sleep 900 >/dev/null || exit 2
kubectl -n "$NS" wait "pod/$PROBE" --for=condition=Ready --timeout=120s >/dev/null || exit 2

scrape() { kubectl -n "$NS" exec "$PROBE" -- curl -fsS --max-time 10 "$1"; }
# read_metric URL NAME sums every series of NAME; a labeled counter not yet exported reads as 0
# only when asked for with a default.
read_metric() { local body; body="$(scrape "$1")" || return 1; serving_sum "$body" "$2"; }
log_count() { kubectl -n "$NS" logs "$1" -c main 2>/dev/null | grep -cF "$2"; }
sent_before="" received_before="" put_before=0
if [ "$SP_TRANSFER" = prom ]; then
  sent_before="$(read_metric "$prefill_url" "$SP_SENT_COUNT")" ||
    { echo "prefill exports no $SP_SENT_COUNT; $SP_TRANSFER_SOURCE" >&2; exit 1; }
  received_before="$(read_metric "$decode_url" "$SP_RECEIVED" || echo 0)"
else
  sent_before="$(log_count "$prefill" "$SP_SENT_LOG")"
  received_before="$(log_count "$decode" "$SP_RECEIVED_LOG")"
fi
[ -z "$store_url" ] || put_before="$(read_metric "$store_url" master_batch_put_end_items_total || echo 0)"

completed=0
tail_id="$(date +%s)-$$"
for i in $(seq 1 "$REQUESTS"); do
  stream=false
  [ $((i % 2)) -eq 0 ] && stream=true
  payload="$(jq -nc --arg model "$MODEL" --argjson stream "$stream" --arg q "$tail_id-$i" '{model:$model,
    messages:[{role:"user",content:("Explain how a Kubernetes controller reconciles a desired state. Case 91 question " + $q + ".")}],
    max_tokens:48,stream:$stream}')"
  response="$(kubectl -n "$NS" exec "$PROBE" -- curl -sS --max-time 60 \
    -w '\nHTTP_STATUS:%{http_code}\n' -H 'Content-Type: application/json' \
    --data-binary "$payload" "http://$MD-router.$NS.svc:8081/v1/chat/completions")" || continue
  [[ "$response" == *'HTTP_STATUS:200'* ]] || continue
  # A streamed failure still ends in [DONE] with status 200: the error arrives as an SSE event, so a
  # stream counts only when it carried generated content and no error object.
  if [ "$stream" = true ]; then
    grep -q '"content": *"[^"]' <<<"$response" && ! grep -q '"error"' <<<"$response" &&
      [[ "$response" == *'[DONE]'* ]] && completed=$((completed + 1))
  elif [ -n "$(sed '/^HTTP_STATUS:/d' <<<"$response" | jq -r '.choices[0].message.content // empty' 2>/dev/null)" ]; then
    completed=$((completed + 1))
  fi
done

fails=0
check() {
  if [ "$1" = true ]; then
    printf 'PASS | %s | %s\n' "$2" "$MD"
  else
    printf 'FAIL | %s | %s\n' "$2" "$MD"
    fails=$((fails + 1))
  fi
}
grew() { awk -v a="$1" -v b="$2" 'BEGIN { print (b > a) ? "true" : "false" }'; }
# has_arg POD FLAG [VALUE] reports whether the main container's argv carries FLAG, followed by VALUE
# when one is given, in either spelling.
has_arg() {
  pod_field "$1" '.spec.containers[] | select(.name == "main") | (.command // []) + (.args // [])' |
    jq -r --arg flag "$2" --arg value "${3:-}" '. as $argv | [range(length) as $i |
      select($argv[$i] == $flag and ($value == "" or $argv[$i + 1] == $value)) ,
      select($value != "" and $argv[$i] == ($flag + "=" + $value))] | length > 0'
}

for pod in "$prefill" "$decode"; do
  logs="$(kubectl -n "$NS" logs "$pod" -c main 2>/dev/null)"
  case "$SP_PIN" in
    env)
      check "$(pod_field "$pod" '[.spec.containers[] | select(.name == "main") | .env[]? |
        select(.name == "MC_FORCE_TCP")] | (length == 1 and .[0].value == "1")')" \
        "$pod carries exactly one MC_FORCE_TCP=1"
      check "$([[ "$logs" != *'Using cross-node NVLink'* ]] && echo true || echo false)" \
        "$pod logs no cross-node NVLink selection" ;;
    argv)
      # shellcheck disable=SC2086,SC2153
      check "$(has_arg "$pod" $SP_PIN_ARG)" "$pod renders $SP_PIN_ARG" ;;
    none)
      printf 'SKIP | %s pins no transport: not applicable on %s | %s\n' "$pod" "$VENDOR" "$MD" ;;
  esac
  [ -z "$SP_PIN_LOG" ] ||
    check "$([[ "$logs" == *"$SP_PIN_LOG"* ]] && echo true || echo false)" "$pod logs the TCP-only transport selection"
done

check "$([ "$completed" -eq "$REQUESTS" ] && echo true || echo false)" \
  "routed P/D requests completed ($completed of $REQUESTS)"
if [ "$SP_TRANSFER" = prom ]; then
  sent_after="$(read_metric "$prefill_url" "$SP_SENT_COUNT" || echo "$sent_before")"
  sent_sum="$(read_metric "$prefill_url" "$SP_SENT_SUM" || echo 0)"
  received_after="$(read_metric "$decode_url" "$SP_RECEIVED" || echo "$received_before")"
  failed="$(awk -v a="$(read_metric "$prefill_url" "$SP_FAILED" || echo 0)" \
    -v b="$(read_metric "$decode_url" "$SP_FAILED" || echo 0)" 'BEGIN { print a + b }')"
  check "$(grew "$sent_before" "$sent_after")" "prefill transfer samples grew ($sent_before to $sent_after)"
  check "$(grew 0 "$sent_sum")" "prefill transferred size is positive ($sent_sum)"
  check "$(grew "$received_before" "$received_after")" \
    "decode received KV ($SP_RECEIVED $received_before to $received_after)"
  check "$(awk -v f="$failed" 'BEGIN { print (f == 0) ? "true" : "false" }')" \
    "no half counted a failed transfer ($failed)"
else
  check "$(grew "$sent_before" "$(log_count "$prefill" "$SP_SENT_LOG")")" "prefill logged a sent transfer"
  check "$(grew "$received_before" "$(log_count "$decode" "$SP_RECEIVED_LOG")")" "decode logged a received transfer"
fi
if [ -n "$store_url" ]; then
  put_after="$(read_metric "$store_url" master_batch_put_end_items_total || echo 0)"
  allocated="$(read_metric "$store_url" master_allocated_bytes || echo 0)"
  # The store writes through batch puts, so their item count is the one that grows with a request;
  # allocated bytes are printed beside it and stay positive from any earlier write.
  check "$(grew "$put_before" "$put_after")" \
    "the store master counted batch-put items ($put_before to $put_after, allocated $allocated bytes)"
  if [ "$ENGINE" = sglang ]; then
    decode_logs="$(kubectl -n "$NS" logs "$decode" -c main 2>/dev/null)"
    # 0.5.18 prints server_args as key='value' and 0.5.19 as a dict, 'key': 'value'.
    check "$([[ "$decode_logs" == *"disaggregation_decode_retraction_backup='cpu_tensor'"* ||
      "$decode_logs" == *"'disaggregation_decode_retraction_backup': 'cpu_tensor'"* ]] && echo true || echo false)" \
      "decode runs retraction backup cpu_tensor"
    check "$([ "$(has_arg "$decode" --enable-hierarchical-cache)" = false ] && echo true || echo false)" \
      "decode renders no hierarchical cache"
    check "$(has_arg "$prefill" --enable-hierarchical-cache)" "prefill renders the hierarchical cache"
    for pod in "$prefill" "$decode"; do
      check "$([[ "$(kubectl -n "$NS" logs "$pod" -c main 2>/dev/null)" != *'Not enough host memory'* ]] &&
        echo true || echo false)" "$pod logs no host-memory refusal"
    done
  fi
fi
pods_after="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=$MD" -o json)" || exit 2
after_restarts="$(jq '[.items[].status.containerStatuses[]?.restartCount] | add // 0' <<<"$pods_after")"
check "$([ "$after_restarts" -eq "$before_restarts" ] && echo true || echo false)" \
  "engine restart count unchanged"
[ "$fails" -eq 0 ]
