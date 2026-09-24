#!/usr/bin/env bash
#
# CASE 91 — vLLM MultiConnector serves a P/D request over a pinned TCP leg and moves real bytes.
#
# case-91.sh <NS> <MODEL_DEPLOYMENT>
#
# Goal:        A completed routed request moves KV bytes over the direct leg between two hosts
#              without RDMA, on the TCP transport the operator pinned rather than one the manifest
#              set, and writes to the bound store, while every engine stays running.
# Environment: An already Ready vLLM ModelDeployment with one prefill and one decode role on two
#              different nodes with no RDMA device, a bound KVCache Store whose transport is tcp,
#              and a router Service. The engine runs vLLM E2E_PD_VLLM_VERSION (default 0.29.0),
#              whose Mooncake client reads MC_FORCE_TCP; older CUDA images embed a client that does
#              not. Real GPUs are required. E2E_PD_PROBE_IMAGE names a pullable curl image;
#              E2E_PD_MODEL names the model served by the deployment.
# Inputs:      All real. No role declares MC_FORCE_TCP itself, so its presence on the Pod is the
#              operator's rendering. A temporary probe Pod sends one streaming chat completion.
# Expected:    Both engine containers carry MC_FORCE_TCP=1 and log the TCP-only selection with no
#              cross-node NVLink line; the request finishes; the prefill engine's transferred-byte
#              sum is positive and its sample count increased; no engine counts a failed transfer;
#              the store master's batch-put item count increased; restart counts stay fixed.
# Cleanup:     A trap removes the temporary probe Pod on pass and failure.
set -uo pipefail

NS="${1:-}"
MD="${2:-}"
IMAGE="${E2E_PD_PROBE_IMAGE:-}"
MODEL="${E2E_PD_MODEL:-}"
VERSION="${E2E_PD_VLLM_VERSION:-0.29.0}"
if [ -z "$NS" ] || [ -z "$MD" ] || [ -z "$IMAGE" ] || [ -z "$MODEL" ]; then
  echo "usage: E2E_PD_PROBE_IMAGE=<curl-image> E2E_PD_MODEL=<model> case-91.sh <NS> <MODEL_DEPLOYMENT>" >&2
  exit 2
fi
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }

md_json="$(kubectl -n "$NS" get modeldeployment "$MD" -o json)" || exit 2
if ! jq -e '.spec.engine.name == "vllm" and .spec.kvCache.poolRef.name != null and
  ([.spec.roles[] | select(.kind == "prefill")] | length) == 1 and
  ([.spec.roles[] | select(.kind == "decode")] | length) == 1 and
  .spec.router.name != null' <<<"$md_json" >/dev/null; then
  echo "case 91 needs a vLLM Store plus P/D ModelDeployment with a router" >&2
  exit 2
fi
# The version is read off what each role runs: the engine version for a synthesized image, the
# image reference for a role that names its own.
if ! jq -e --arg v "$VERSION" '.spec.engine.version as $ev | all(.spec.roles[];
  if .image then (.image | contains("-vllm" + $v)) else $ev == $v end)' <<<"$md_json" >/dev/null; then
  echo "case 91 needs every role on vLLM $VERSION; older CUDA images embed a Mooncake client without MC_FORCE_TCP" >&2
  exit 2
fi
if ! jq -e '(.spec.kvTransfer.protocol // "tcp") == "tcp"' <<<"$md_json" >/dev/null; then
  echo "case 91 needs the direct leg on tcp" >&2
  exit 2
fi
if jq -e 'any(.spec.roles[].env[]?; .name == "MC_FORCE_TCP")' <<<"$md_json" >/dev/null; then
  echo "a role declares MC_FORCE_TCP itself, so the Pod cannot show what the operator rendered" >&2
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
store_protocol="$(pod_field "$prefill" '.metadata.annotations["kvcache.gpustack.ai/client-config"] // "{}" | fromjson | .protocol // empty')"
[ "$store_protocol" = "tcp" ] || { echo "case 91 needs a tcp store; the pin is withheld beside any other" >&2; exit 2; }
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
# poolRef names the namespace's KVCachePoolBinding; the cluster-scoped pool is behind it.
pool_name="$(kubectl -n "$NS" get kvcachepoolbinding "$(jq -r '.spec.kvCache.poolRef.name' <<<"$md_json")" \
  -o jsonpath='{.spec.poolRef.name}')" || exit 2
endpoint="$(kubectl get kvcachepool "$pool_name" -o jsonpath='{.status.clientEndpoint}')" || exit 2
[ -n "$endpoint" ] || { echo "the pool publishes no client endpoint" >&2; exit 2; }
store_url="http://${endpoint%:*}:9003/metrics"
before_restarts="$(jq '[.items[].status.containerStatuses[]?.restartCount] | add // 0' <<<"$pods")"

PROBE="case91-transfer-$$"
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
before="$(read_metric "$prefill_url" vllm:mooncake_bytes_transferred_count)" ||
  { echo "native Mooncake transfer histogram is absent" >&2; exit 1; }
put_before="$(read_metric "$store_url" master_batch_put_end_items_total || echo 0)"
url="/apis/worker.gpustack.ai/v1/namespaces/$NS/modeldeployments/$MD/metrics"
kubectl get --raw "$url" >/dev/null || exit 2
payload="$(jq -nc --arg model "$MODEL" '{model:$model,messages:[{role:"user",content:
  "Explain how a Kubernetes controller reconciles a desired state."}],max_tokens:64,stream:true}')"
response="$(kubectl -n "$NS" exec "$PROBE" -- curl -sS --max-time 60 \
  -w '\nHTTP_STATUS:%{http_code}\n' -H 'Content-Type: application/json' \
  --data-binary "$payload" "http://$MD-router.$NS.svc:8081/v1/chat/completions")"
request_exit=$?
after="$(read_metric "$prefill_url" vllm:mooncake_bytes_transferred_count)" ||
  { echo "native Mooncake transfer histogram disappeared" >&2; exit 1; }
bytes="$(read_metric "$prefill_url" vllm:mooncake_bytes_transferred_sum || echo 0)"
failed="unread"
if f_prefill="$(read_metric "$prefill_url" vllm:mooncake_num_failed_transfers_total)" &&
  f_decode="$(read_metric "$decode_url" vllm:mooncake_num_failed_transfers_total)"; then
  failed=$((f_prefill + f_decode))
fi
put_after="$(read_metric "$store_url" master_batch_put_end_items_total || echo 0)"
allocated="$(read_metric "$store_url" master_allocated_bytes || echo 0)"
snapshot="$(kubectl get --raw "$url")" || exit 2
pods_after="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=$MD" -o json)" || exit 2
after_restarts="$(jq '[.items[].status.containerStatuses[]?.restartCount] | add // 0' <<<"$pods_after")"

fails=0
check() {
  if [ "$1" = true ]; then
    printf 'PASS | %s | %s\n' "$2" "$MD"
  else
    printf 'FAIL | %s | %s\n' "$2" "$MD"
    fails=$((fails + 1))
  fi
}
for pod in "$prefill" "$decode"; do
  check "$(pod_field "$pod" '[.spec.containers[] | select(.name == "main") | .env[]? |
    select(.name == "MC_FORCE_TCP")] | (length == 1 and .[0].value == "1")')" \
    "$pod carries exactly one MC_FORCE_TCP=1"
  logs="$(kubectl -n "$NS" logs "$pod" -c main 2>/dev/null)"
  check "$([[ "$logs" == *'MC_FORCE_TCP is set'* ]] && echo true || echo false)" \
    "$pod logs the TCP-only transport selection"
  check "$([[ "$logs" != *'Using cross-node NVLink'* ]] && echo true || echo false)" \
    "$pod logs no cross-node NVLink selection"
done
check "$([ "$request_exit" -eq 0 ] && [[ "$response" == *'HTTP_STATUS:200'* ]] &&
  [[ "$response" == *'[DONE]'* ]] && echo true || echo false)" "routed P/D request completed"
check "$(awk -v before="$before" -v after="$after" 'BEGIN { print (after > before) ? "true" : "false" }')" \
  "native Mooncake transfer samples increased"
check "$(awk -v bytes="$bytes" 'BEGIN { print (bytes > 0) ? "true" : "false" }')" \
  "native Mooncake transferred bytes are positive"
check "$([ "$failed" = 0 ] && echo true || echo false)" "no engine counted a failed transfer"
# The store writes through batch puts, so their item count is the one that grows with a request;
# allocated bytes are printed beside it and stay positive from any earlier write.
check "$(awk -v b="$put_before" -v p="$put_after" 'BEGIN { print (p > b) ? "true" : "false" }')" \
  "the store master counted batch-put items ($put_before to $put_after, allocated $allocated bytes)"
check "$(jq -r '[.latency[]? | select(.samples > 0)] | length > 0' <<<"$snapshot")" \
  "aggregated latency window has samples"
check "$([ "$after_restarts" -eq "$before_restarts" ] && echo true || echo false)" \
  "engine restart count unchanged"
[ "$fails" -eq 0 ]
