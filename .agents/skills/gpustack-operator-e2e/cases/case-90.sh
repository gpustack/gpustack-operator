#!/usr/bin/env bash
#
# CASE 90 — A ModelDeployment metrics snapshot reflects served traffic and Pod endpoints.
#
# case-90.sh <NS> <MODEL_DEPLOYMENT>
#
# Goal:        Read the aggregated metrics object twice and fetch every advertised native endpoint
#              from another Pod. This distinguishes a valid annotation from an unreachable port.
# Environment: An explicitly selected cluster and namespace with a serving ModelDeployment, its
#              managed engine and router images, and representative traffic maintained while this
#              case runs. A cache-hit window needs requests between the two reads. The namespace
#              must permit a temporary probe Pod. Exits 2 if inputs or a probe image are missing.
# Inputs:      All real, nothing mocked. E2E_MD_PROBE_IMAGE must name a pullable image with `curl`
#              and `sleep`; E2E_EXPECT_PARTIAL=1 checks an already degraded deployment. No Pod is
#              patched to manufacture a failed scrape.
# Expected:    Processing, queueing, cache-hit, and latency data have sources and timestamps;
#              the partial flag and missing list agree; each annotated endpoint returns Prometheus
#              text to the independent probe Pod. Raw counter and histogram series stay available.
# Cleanup:     A trap deletes the temporary probe Pod on pass and failure.
set -uo pipefail

NS="${1:-}"
MD="${2:-}"
IMAGE="${E2E_MD_PROBE_IMAGE:-}"
if [ -z "$NS" ] || [ -z "$MD" ] || [ -z "$IMAGE" ]; then
  echo "usage: E2E_MD_PROBE_IMAGE=<curl-image> case-90.sh <NS> <MODEL_DEPLOYMENT>" >&2
  exit 2
fi
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); }
print_rows() {
  echo "STATUS | CHECK | OBJECT"
  for row in "${ROWS[@]}"; do
    IFS='|' read -r status check object <<<"$row"
    printf '%s | %s | %s\n' "$status" "$check" "$object"
  done
}

url="/apis/worker.gpustack.ai/v1/namespaces/$NS/modeldeployments/$MD/metrics"
first="$(kubectl get --raw "$url" 2>/dev/null)" || {
  echo "metrics subresource is unavailable for $NS/$MD" >&2
  exit 2
}
if ! jq -e '.kind == "ModelDeploymentMetrics" and .timestamp != null' <<<"$first" >/dev/null; then
  echo "metrics response is not a typed ModelDeploymentMetrics object" >&2
  exit 1
fi
sleep 5
second="$(kubectl get --raw "$url" 2>/dev/null)" || {
  echo "second metrics read failed" >&2
  exit 1
}
for area in processing queueing cacheHits latency; do
  if jq -e --arg area "$area" '(.[$area] // [] | length) > 0 and
    all(.[$area][]; .source != null and .observedAt != null)' <<<"$second" >/dev/null; then
    record PASS "$area source and freshness" "$MD"
  else
    record FAIL "$area source and freshness" "$MD"
  fi
done
if [ "${E2E_EXPECT_PARTIAL:-0}" = 1 ]; then
  if jq -e '.partial == true and (.missing // [] | length > 0)' <<<"$second" >/dev/null; then
    record PASS "partial read names missing sources" "$MD"
  else
    record FAIL "partial read names missing sources" "$MD"
  fi
# A complete read may still list the entries that do not count as partial: a labeled series not yet
# exported beside its pair, a source this shape does not provide, and a source of a Pod that served
# no request between the two reads. Any other missing entry is a partial read.
elif jq -e '.partial == false and all(.missing // [] | .[];
  (.reason | startswith("labeled failure or error counter is not exported")) or
  (.reason | startswith("labeled histogram is not exported")) or
  (.reason | startswith("unsupported source:")) or
  (.reason | startswith("idle sampling window:")))' <<<"$second" >/dev/null; then
  record PASS "complete read" "$MD"
else
  record FAIL "complete read" "$(jq -c '{partial,missing}' <<<"$second")"
fi

md_json="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o json)" || exit 2
md_uid="$(jq -r '.metadata.uid' <<<"$md_json")"
managed_roles="$(jq -c '[.spec.roles[] | select((.command // []) | length == 0) | .name]' <<<"$md_json")"
router_name="$(jq -r '.spec.router.name // ""' <<<"$md_json")"
router_rs_uids='[]'
if [ -n "$router_name" ]; then
  deployment_json="$(kubectl -n "$NS" get deployment "$MD-router" -o json)" || exit 2
  router_deployment_uid="$(jq -r --arg uid "$md_uid" '
    select(any(.metadata.ownerReferences[]?; .kind == "ModelDeployment" and .uid == $uid)) | .metadata.uid
  ' <<<"$deployment_json")"
  [ -n "$router_deployment_uid" ] || { echo "router Deployment is not owned by $MD" >&2; exit 2; }
  replicasets_json="$(kubectl -n "$NS" get replicasets -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=$MD" -o json)" || exit 2
  router_rs_uids="$(jq -c --arg uid "$router_deployment_uid" '[.items[] |
    select(any(.metadata.ownerReferences[]?; .kind == "Deployment" and .uid == $uid)) | .metadata.uid
  ]' <<<"$replicasets_json")"
fi
pods_json="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=$MD" -o json)" || exit 2
managed_pods="$(jq -c --arg uid "$md_uid" --arg router "$router_name" \
  --argjson roles "$managed_roles" --argjson rs_uids "$router_rs_uids" '
  .items[] | select(
    ((.metadata.labels["app.kubernetes.io/component"] as $role |
      ($roles | index($role)) != null) and
      any(.metadata.ownerReferences[]?; .kind == "ModelDeployment" and .uid == $uid)) or
    ((.metadata.labels["modeldeployment.gpustack.ai/router"] == $router) and $router != "" and
      any(.metadata.ownerReferences[]?; .kind == "ReplicaSet" and (.uid as $owner | $rs_uids | index($owner)) != null))
  )
' <<<"$pods_json")"
if [ -z "$managed_pods" ]; then
  echo "no owned managed metrics Pods are visible" >&2
  exit 2
fi

PROBE="case90-metrics-$$"
cleanup() { kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1; }
trap cleanup EXIT
if ! kubectl -n "$NS" run "$PROBE" --image="$IMAGE" --restart=Never --command -- sleep 600 >/dev/null; then
  echo "cannot create temporary probe Pod" >&2
  exit 2
fi
if ! kubectl -n "$NS" wait "pod/$PROBE" --for=condition=Ready --timeout=120s >/dev/null; then
  echo "temporary probe Pod did not become Ready" >&2
  exit 2
fi

while IFS= read -r pod; do
  [ -n "$pod" ] || continue
  name="$(jq -r '.metadata.name' <<<"$pod")"
  ip="$(jq -r '.status.podIP // ""' <<<"$pod")"
  port="$(jq -r '.metadata.annotations["prometheus.io/port"] // ""' <<<"$pod")"
  scheme="$(jq -r '.metadata.annotations["prometheus.io/scheme"] // "http"' <<<"$pod")"
  if [ -z "$ip" ] || ! [[ "$port" =~ ^[0-9]+$ ]] ||
     { [ "$scheme" != http ] && [ "$scheme" != https ]; } ||
     ! jq -e --argjson port "${port:-0}" '
       .metadata.annotations["prometheus.io/scrape"] == "true" and
       .metadata.annotations["prometheus.io/path"] == "/metrics" and
       any([.spec.containers[], .spec.initContainers[]?][]; any(.ports[]?; .containerPort == $port))
     ' <<<"$pod" >/dev/null; then
    record FAIL "advertised listener" "$name"
    continue
  fi
  target="$ip"
  [[ "$ip" == *:* ]] && target="[$ip]"
  if body="$(kubectl -n "$NS" exec "$PROBE" -- curl -fsS --max-time 5 "$scheme://$target:$port/metrics" 2>/dev/null)"; then
    if [[ "$body" == *"# TYPE "* ]]; then
      record PASS "second-Pod metrics reachability" "$name:$port"
    else
      record FAIL "second-Pod metrics reachability" "$name:$port"
    fi
  else
    record FAIL "second-Pod metrics reachability" "$name:$port"
  fi
done <<<"$managed_pods"

if [ -n "$router_name" ]; then
  split="$(jq -r 'any(.spec.roles[]; .kind == "prefill")' <<<"$md_json")"
  engine="$(jq -r '.spec.engine.name' <<<"$md_json")"
  # The sources a router does not provide for this shape are a decided contract, so a complete read
  # lists exactly those as unsupported on its router Pods: the vLLM router's P/D mode exports no
  # processing gauge. An engine Pod's own unsupported source is not the router's and is not counted.
  want='[]'
  [ "$router_name/$split" = vllm-router/true ] && want='["vllm_router_active_workers","vllm_router_running_requests"]'
  router_pods="$(jq -sc '[.[] | select(.metadata.labels["modeldeployment.gpustack.ai/router"] != null) | .metadata.name]' <<<"$managed_pods")"
  got="$(jq -c --argjson routers "$router_pods" '[.missing[]? | select(.pod as $pod | $routers | index($pod) != null) |
    select(.reason | startswith("unsupported source:")) | .source] | unique' <<<"$second")"
  if [ "$got" = "$want" ] && jq -e '(.traffic // []) | length > 0' <<<"$second" >/dev/null &&
     { [ "$engine/$split" != sglang/true ] || jq -e '(.transfer // []) | length > 0' <<<"$second" >/dev/null; }; then
    record PASS "router contract for this shape" "$router_name, unsupported $got"
  else
    record FAIL "router contract for this shape" "$router_name: unsupported $got, want $want; traffic $(jq -c '.traffic // [] | length' <<<"$second"), transfer $(jq -c '.transfer // [] | length' <<<"$second")"
  fi
  # Every Ready endpoint the router is meant to reach: the leader member of each role replica.
  endpoints="$(jq -r --arg label "modeldeployment.gpustack.ai/member-index" 'select(.metadata.labels["app.kubernetes.io/component"] != null and
    (.metadata.labels[$label] // "0") == "0" and any(.status.conditions[]?; .type == "Ready" and .status == "True")) |
    .status.podIP' <<<"$managed_pods" | sort -u)"
  want_n="$(grep -c . <<<"$endpoints")"
  seen=""
  if [ "$router_name/$split" = vllm-router/true ]; then
    # This mode exports no worker gauge; the per-worker request counter names every worker that
    # served, so this read needs traffic.
    router_pod="$(jq -r 'select(.metadata.labels["modeldeployment.gpustack.ai/router"] != null) | .metadata.name + " " + .status.podIP' <<<"$managed_pods" | head -1)"
    seen="$(kubectl -n "$NS" exec "$PROBE" -- curl -fsS --max-time 5 "http://${router_pod#* }:9090/metrics" 2>/dev/null |
      grep -o '^vllm_router_processed_requests_total{worker="[^"]*"' | sed 's/.*:\/\///; s/:[0-9]*"$//' | sort -u | grep -cxF -f <(printf '%s\n' "$endpoints"))"
  else
    seen="$(jq -r '[.processing[]? | select(.name == "router-backends" or .name == "router-reported-workers") | .value] | add // empty' <<<"$second")"
  fi
  if [ "${seen:-0}" = "$want_n" ]; then
    record PASS "router sees every serving endpoint" "$seen of $want_n"
  else
    record FAIL "router sees every serving endpoint" "router reports ${seen:-nothing}, $want_n Ready endpoint Pods"
  fi
fi
while IFS= read -r line; do
  record INFO "image" "$line"
done < <(jq -r '.metadata.name as $p | .status.containerStatuses[]? | "\($p) \(.name) runs \(.imageID)"' <<<"$managed_pods")

print_rows
[ "$FAILS" -eq 0 ]
