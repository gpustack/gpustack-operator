#!/usr/bin/env bash
#
# CASE 108 — Download progress: a node writes its progress on thresholds and nothing at rest, the
#            artifact counts its nodes in 5% steps, and the v1 progress subresource answers live,
#            to its own namespace only, naming no node (MUTATING, self-recovering)
#
#   case-108.sh <NS>
#
# Goal:        Prove the progress contract end to end on a download slow enough to show it: the
#              node's NodeModelStore carries downloadedBytes on 30-second, 5% thresholds and writes
#              nothing once the content is published; the ModelArtifact's status.nodes moves in
#              multiples of 5 and ends with the node counted ready; the v1 progress subresource
#              reads the running download live, is refused to a subject without its RBAC rule and
#              for another namespace, and names no node; the cluster-scoped NodeModelStore names no
#              tenant; the v1 views write nothing the worker's identity would.
# Environment: A cluster installed from this chart with modelManager.enabled (the default), a
#              schedulable worker, and the stock python image pullable. NO GPU and no InstanceType:
#              the consumer is a bare Pod.
# Inputs:      MOCKED: a test hub (_model-hub.py) in <NS> standing in for the Hugging Face Hub. The
#              node's bandwidth Setting is lowered to 128 KiB/s so a 24 MiB file downloads for about
#              three minutes. Real: the plugin, kubelet, the worker, the aggregated API server and
#              RBAC, with two service accounts impersonated.
# Expected:    - at least two writes carry an intermediate downloadedBytes; no two progress-only
#                writes are closer than 30 s; at most 20 progress-only writes;
#              - no write for one report interval (5 min) after the Pod runs;
#              - the artifact's status.nodes.downloadingPercent is a multiple of 5 while downloading,
#                and ends ready >= 1 with no percent;
#              - two progress reads during the download: live 1 and non-decreasing downloadingBytes;
#              - progress: allowed with get modelartifacts/progress in <NS>; refused for <NS>-b's
#                artifact and to a subject with only get modelartifacts; the allowed answer carries
#                no node name;
#              - no namespace, artifact, repository or Pod name of this case in any NodeModelStore;
#              - an update of status through the v1 ModelArtifact leaves the stored status; the v1
#                ModelArtifact has no status subresource; the v1 NodeModelStore refuses an update.
# Cleanup:     Trap stops the watch and deletes the Pods, artifacts, RBAC, the second namespace and
#              the hub, and puts back the Settings it changed.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-108.sh <NS>}"
NSB="${NS}-b"
P=c108
LABEL="e2e.gpustack.ai/case=108"
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
SCRATCH="$(mktemp -d)"
WATCH_PID=""
ORIG_ENDPOINT="$(setting_get model-artifact-huggingface-endpoint)"
ORIG_DELIVERY="$(setting_get model-artifact-delivery-mode)"
ORIG_BANDWIDTH="$(setting_get model-store-download-bandwidth)"
READER="system:serviceaccount:${NS}:${P}-reader"
PLAIN="system:serviceaccount:${NS}:${P}-plain"

restore() { # restore KEY VALUE
  if [ -n "$2" ]; then setting_set "$1" "$2"; else setting_unset "$1"; fi
}
cleanup() {
  echo
  echo "[case-108] cleanup"
  [ -n "$WATCH_PID" ] && kill "$WATCH_PID" >/dev/null 2>&1
  for ns in "$NS" "$NSB"; do
    kubectl -n "$ns" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
    kubectl -n "$ns" delete modelartifacts.worker.gpustack.ai -l "$LABEL" --ignore-not-found >/dev/null 2>&1
  done
  kubectl -n "$NS" delete role,rolebinding,serviceaccount -l "$LABEL" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete deploy,svc,configmap -l e2e.gpustack.ai/model-hub=true --ignore-not-found >/dev/null 2>&1
  kubectl -n "$NS" delete configmap "${P}-hub-script" --ignore-not-found >/dev/null 2>&1
  kubectl delete namespace "$NSB" --ignore-not-found --wait=false >/dev/null 2>&1
  restore model-artifact-huggingface-endpoint "$ORIG_ENDPOINT"
  restore model-artifact-delivery-mode "$ORIG_DELIVERY"
  restore model-store-download-bandwidth "$ORIG_BANDWIDTH"
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

get_uid() { kubectl -n "$1" get modelartifacts.worker.gpustack.ai "$2" -o jsonpath='{.metadata.uid}'; }
# nodes_of NS NAME: the artifact's status.nodes as "ready downloading failed percent".
nodes_of() {
  kubectl -n "$1" get modelartifacts.worker.gpustack.ai "$2" \
    -o jsonpath='{.status.nodes.ready} {.status.nodes.downloading} {.status.nodes.failed} {.status.nodes.downloadingPercent}'
}
# field JSON KEY: a top-level number of a progress answer, or "".
field() { python3 -c "import json,sys; print(json.loads(sys.argv[1]).get(sys.argv[2], ''))" "$1" "$2" 2>/dev/null; }

read -r -a WORKERS <<<"$(model_workers)"
[ "${#WORKERS[@]}" -ge 1 ] || { echo "[case-108] needs a worker; NOTHING WAS VERIFIED"; exit 2; }
W1="${WORKERS[0]}"
if ! kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1; then
  echo "[case-108] the model-manager plugin is not installed; NOTHING WAS VERIFIED"
  exit 2
fi

for ns in "$NS" "$NSB"; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: ${P}-reader, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "108"}}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: ${P}-plain, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "108"}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: ${P}-reader, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "108"}}
rules:
  - {apiGroups: [worker.gpustack.ai], resources: [modelartifacts, modelartifacts/progress], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: ${P}-plain, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "108"}}
rules:
  - {apiGroups: [worker.gpustack.ai], resources: [modelartifacts], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: ${P}-reader, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "108"}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: ${P}-reader}
subjects: [{kind: ServiceAccount, name: ${P}-reader, namespace: ${NS}}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: ${P}-plain, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "108"}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: ${P}-plain}
subjects: [{kind: ServiceAccount, name: ${P}-plain, namespace: ${NS}}]
YAML

REPOS="$(python3 -c "
import json
mib = 1 << 20
print(json.dumps({'e2e/progress-model': {'files': {'config.json': {'size': 300},
                                                   'model.safetensors': {'size': 24 * mib, 'lfs': True}}}}))")"
HUB_URL="$(mh_deploy "$NS" "${P}-hub" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$HUB_URL"
setting_set model-artifact-delivery-mode Node
setting_set model-store-download-bandwidth 128Ki
settings_settle
echo "[case-108] hub at ${HUB_URL}; worker ${W1}"

artifact "$NS" "${P}-art" e2e/progress-model "" 108
artifact "$NSB" "${P}-other" e2e/progress-model "" 108
DIGEST="$(wait_resolved "$NS" "${P}-art" 120)"
[ -n "$(wait_resolved "$NSB" "${P}-other" 120)" ] || record FAIL "the second namespace's artifact resolves" "${NSB}/${P}-other"
if [ -z "$DIGEST" ]; then
  record FAIL "the artifact resolves against the test hub" "${P}-art"
  print_rows
  exit 1
fi
SIZE="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-art" -o jsonpath='{.status.resolved.sizeBytes}')"

# ---------------------------------------------------------------- the download, watched
WATCH_OUT="${SCRATCH}/nms-watch.jsonl"
WATCH_PID="$(nms_watch "$W1" "$WATCH_OUT")"
sleep 3
consumer "$NS" "${P}-pod" "$W1" "${P}-art" "$(get_uid "$NS" "${P}-art")" "$DIGEST"

percents="" reads=()
for _ in $(seq 1 90); do
  sleep 5
  [ "$(kubectl -n "$NS" get pod "${P}-pod" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
  read -r _ dl _ pct <<<"$(nodes_of "$NS" "${P}-art")"
  [ "${dl:-0}" = 1 ] && [ -n "${pct:-}" ] && percents="${percents} ${pct}"
  if [ "${#reads[@]}" -lt 2 ] && [ "${dl:-0}" = 1 ]; then
    ans="$(progress_as "$NS" "${P}-art" "$READER")" && reads+=("$ans")
    sleep 5
  fi
done
pod_ready "$NS" "${P}-pod" 120 || record FAIL "the consumer runs after its download" "$(pod_mount_events "$NS" "${P}-pod" | tail -1)"
RAN_AT=$(date +%s)

# ---------------------------------------------------------------- the artifact's nodes
bad="$(for p in $percents; do [ $((p % 5)) -eq 0 ] || echo "$p"; done)"
if [ -n "$percents" ] && [ -z "$bad" ]; then
  record PASS "the artifact's downloading percent moved in steps of 5 while one node downloaded" "${P}-art:${percents}"
else
  record FAIL "the artifact's percent is a multiple of 5 while downloading" "${P}-art: seen '${percents}', off-step '${bad}'"
fi
final=""
for _ in $(seq 1 20); do
  final="$(nodes_of "$NS" "${P}-art")"
  read -r rd dl _ pct <<<"$final"
  [ "${rd:-0}" -ge 1 ] && [ "${dl:-1}" = 0 ] && [ -z "${pct:-}" ] && break
  sleep 3
done
read -r rd dl _ pct <<<"$final"
if [ "${rd:-0}" -ge 1 ] && [ "${dl:-1}" = 0 ] && [ -z "${pct:-}" ]; then
  record PASS "after publication the artifact counts the node ready and no percent" "${P}-art: ${final}"
else
  record FAIL "after publication the node is counted ready (got '${final}')" "${P}-art"
fi

# ---------------------------------------------------------------- progress, live
if [ "${#reads[@]}" -eq 2 ]; then
  l1="$(field "${reads[0]}" live)" l2="$(field "${reads[1]}" live)"
  b1="$(field "${reads[0]}" downloadingBytes)" b2="$(field "${reads[1]}" downloadingBytes)"
  if [ "$l1" = 1 ] && [ "$l2" = 1 ] && [ "${b2:-0}" -ge "${b1:-1}" ] && [ "${b1:-0}" -gt 0 ]; then
    record PASS "two progress reads during the download are live and never go back" "live ${l1},${l2}; bytes ${b1} -> ${b2}"
  else
    record FAIL "progress reads are live and non-decreasing" "live ${l1:-?},${l2:-?}; bytes ${b1:-?} -> ${b2:-?}"
  fi
else
  record FAIL "two progress reads were taken during the download" "${#reads[@]} taken: ${reads[*]:-none}"
fi

# ---------------------------------------------------------------- progress, authorization
ans="$(progress_as "$NS" "${P}-art" "$READER")"; rc=$?
leak=""
for n in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
  case "$ans" in *"$n"*) leak="${leak} $n" ;; esac
done
if [ "$rc" -eq 0 ] && [ "$(field "$ans" ready)" -ge 1 ] 2>/dev/null; then
  record PASS "a subject with get modelartifacts/progress reads its namespace's progress" "${READER}: ready $(field "$ans" ready)"
else
  record FAIL "the reader reads progress (rc ${rc})" "$(echo "$ans" | head -c 300)"
fi
if [ "$rc" -eq 0 ] && [ -z "$leak" ]; then
  record PASS "the tenant's progress names no node of the cluster" "${P}-art"
else
  record FAIL "the tenant's progress names no node" "found:${leak:- <no answer>}"
fi
ans="$(progress_as "$NSB" "${P}-other" "$READER")"; rc=$?
if [ "$rc" -ne 0 ] && echo "$ans" | grep -qi forbidden; then
  record PASS "the same subject is refused another namespace's progress" "${NSB}/${P}-other"
else
  record FAIL "another namespace's progress is refused (rc ${rc})" "$(echo "$ans" | head -c 300)"
fi
ans="$(progress_as "$NS" "${P}-art" "$PLAIN")"; rc=$?
if [ "$rc" -ne 0 ] && echo "$ans" | grep -qi forbidden; then
  record PASS "get modelartifacts alone does not grant progress" "$PLAIN"
else
  record FAIL "a subject without the subresource rule is refused (rc ${rc})" "$(echo "$ans" | head -c 300)"
fi

# ---------------------------------------------------------------- no tenant in the node objects
dump="$(kubectl get nodemodelstores.worker.gpustack.ai -o json)"
found=""
for s in "$NS" "$NSB" "${P}-art" "${P}-other" "e2e/progress-model" "${P}-pod"; do
  case "$dump" in *"\"$s"*|*"$s\""*|*"/$s"*) found="${found} $s" ;; esac
done
if [ -n "$dump" ] && [ -z "$found" ] && echo "$dump" | grep -q "$DIGEST"; then
  record PASS "no namespace, artifact, repository or Pod of this case is in any NodeModelStore" "the digest is"
else
  record FAIL "the NodeModelStores name no tenant" "found:${found:- <no dump or no digest>}"
fi

# ---------------------------------------------------------------- the v1 views write nothing as the worker
before="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-art" -o jsonpath='{.status.resolved.manifestDigest}')"
kubectl -n "$NS" patch modelartifacts.v1.worker.gpustack.ai "${P}-art" --type=merge \
  -p '{"status":{"resolved":{"manifestDigest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}}}' >/dev/null 2>&1
after="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-art" -o jsonpath='{.status.resolved.manifestDigest}')"
if [ -n "$before" ] && [ "$before" = "$after" ]; then
  record PASS "a status change sent through the v1 ModelArtifact leaves the stored status" "${P}-art"
else
  record FAIL "the v1 view does not write status (before ${before}, after ${after})" "${P}-art"
fi
out="$(kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/${NS}/modelartifacts/${P}-art/status" 2>&1)"
if echo "$out" | grep -Eqi 'not ?found'; then
  record PASS "the v1 ModelArtifact has no status subresource" "${P}-art"
else
  record FAIL "the v1 ModelArtifact serves no status" "$(echo "$out" | head -c 200)"
fi
out="$(kubectl patch nodemodelstores.v1.worker.gpustack.ai "$W1" --type=merge -p '{"metadata":{"labels":{"e2e.gpustack.ai/c108":"x"}}}' 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] && [ -z "$(kubectl get nodemodelstores.worker.gpustack.ai "$W1" -o jsonpath='{.metadata.labels.e2e\.gpustack\.ai/c108}')" ]; then
  record PASS "the v1 NodeModelStore refuses an update" "$(echo "$out" | head -c 120)"
else
  record FAIL "the v1 NodeModelStore is read-only (rc ${rc})" "$(echo "$out" | head -c 200)"
fi
# has_columns HEADER COLUMN...: whether the header line names every column, in order.
has_columns() {
  local header="$1" col rest
  shift
  rest=" $(echo "$header" | tr -s ' ') "
  for col in "$@"; do
    case "$rest" in *" $col "*) rest="${rest#* "$col" }"; rest=" $rest" ;; *) return 1 ;; esac
  done
}
cols="$(kubectl -n "$NS" get modelartifacts.v1.worker.gpustack.ai 2>&1 | head -1)"
if has_columns "$cols" NAME SOURCE REVISION SIZE READY DOWNLOADING RESOLVED; then
  record PASS "the v1 ModelArtifact prints its columns" "$(echo "$cols" | tr -s ' ')"
else
  record FAIL "the v1 ModelArtifact prints Name, Source, Revision, Size, Ready, Downloading, Resolved" "$cols"
fi
cols="$(kubectl get nodemodelstores.v1.worker.gpustack.ai 2>&1 | head -1)"
if has_columns "$cols" NAME READY USED MODELS DOWNLOADING; then
  record PASS "the v1 NodeModelStore prints its columns" "$(echo "$cols" | tr -s ' ')"
else
  record FAIL "the v1 NodeModelStore prints Name, Ready, Used, Models, Downloading" "$cols"
fi

# ---------------------------------------------------------------- the writes, then the quiet node
QUIET=330
left=$(( RAN_AT + QUIET - $(date +%s) ))
[ "$left" -gt 0 ] && sleep "$left"
stats="$(nms_write_stats "$WATCH_OUT" "$DIGEST")"
kill "$WATCH_PID" >/dev/null 2>&1
WATCH_PID=""
progress="${stats#progress=}" progress="${progress%% *}"
mingap="${stats#*mingap=}" mingap="${mingap%% *}"
inter="${stats#*intermediate=}" inter="${inter%% *}"
last="${stats##*last=}"
if [ "${inter:-0}" -ge 2 ]; then
  record PASS "the node's writes carried at least two intermediate progress values" "${W1}: ${stats}"
else
  record FAIL "at least two intermediate progress values were written" "${W1}: ${stats} (size ${SIZE})"
fi
if [ "${progress:-99}" -le 20 ] && { [ "${mingap:--1}" -lt 0 ] || [ "${mingap}" -ge 29 ]; }; then
  record PASS "progress-only writes: at most 20, none closer than 30 s" "${W1}: ${stats}"
else
  record FAIL "progress writes keep their thresholds" "${W1}: ${stats}"
fi
if [ "${last:-0}" -gt 0 ] && [ "$last" -le $((RAN_AT + 30)) ]; then
  record PASS "the node wrote nothing for a report interval after the Pod ran" "${W1}: last write $((last - RAN_AT))s after, quiet ${QUIET}s"
else
  record FAIL "a quiet node writes nothing" "${W1}: last write $((last - RAN_AT))s after the Pod ran"
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-108] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-108] PASS"
