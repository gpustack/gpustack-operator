#!/usr/bin/env bash
#
# CASE 8 — Topograph AWS provider is ready through EKS Pod Identity   (READ-ONLY)
#
#   case-8.sh <NS>
#
# Goal:        Prove an enabled Topograph subchart is installed as part of the operator release and
#              its AWS API is ready with the dedicated Pod Identity ServiceAccount and its brokers
#              complete node-local IMDS discovery.
# Environment: An EKS cluster installed with topograph.enabled=true, provider.name=aws, and the
#              infrastructure module's optional Pod Identity association enabled. AUTO-SKIPS when
#              Topograph is disabled; FAILS when it is enabled but incomplete.
# Inputs:      The live Helm release and Pods only; nothing mocked and no cluster mutation.
# Expected:    Helm records the AWS provider, the Topograph API is
#              ready and receives the EKS Pod Identity credential endpoint, one broker is ready on
#              every Linux Node (including cordoned Nodes still targeted by the DaemonSet), and all
#              brokers complete their IMDS-backed startup
#              probe.
# Cleanup:     None; this case is read-only. The run's ordinary Helm teardown owns these resources.
set -uo pipefail

NS="${1:?usage: case-8.sh <NS>}"
RELEASE=gpustack-operator
API_SA=gpustack-operator-topograph
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"
HELM="$(bash "${LIB}/helm.sh")" || exit 1

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

report() {
  echo
  echo "== CASE 8 — Topograph AWS provider is ready through EKS Pod Identity =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
}

if ! values="$($HELM get values "$RELEASE" -n "$NS" -o json 2>/dev/null)"; then
  echo "CASE 8 SKIP — release ${RELEASE} is not readable in ${NS}"
  exit 0
fi
enabled="$(printf '%s' "$values" | jq -r '.topograph.enabled // false')"
if [ "$enabled" != true ]; then
  echo "CASE 8 SKIP — topograph.enabled is not true on release ${RELEASE}"
  exit 0
fi

provider="$(printf '%s' "$values" | jq -r '.topograph.provider.name // ""')"
if [ "$provider" = aws ]; then
  record PASS "Helm records the AWS provider" "provider=${provider}"
else
  record FAIL "Helm records the AWS provider" "provider=${provider:-missing}"
fi

if kubectl -n "$NS" rollout status deploy/gpustack-operator-topograph --timeout=180s >/dev/null 2>&1; then
  record PASS "Topograph API deployment is ready" "deploy/gpustack-operator-topograph"
else
  record FAIL "Topograph API deployment is ready" \
    "$(kubectl -n "$NS" get deploy gpustack-operator-topograph -o jsonpath='{.status.readyReplicas}/{.status.replicas}' 2>/dev/null || echo missing)"
fi

nodes="$(kubectl get nodes -o json | jq '[.items[] | select(.metadata.labels["kubernetes.io/os"] == "linux")] | length')"
desired="$(kubectl -n "$NS" get ds gpustack-operator-topograph-node-data-broker -o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null)"
ready="$(kubectl -n "$NS" get ds gpustack-operator-topograph-node-data-broker -o jsonpath='{.status.numberReady}' 2>/dev/null)"
if [ "${nodes:-0}" -gt 0 ] && [ "$desired" = "$nodes" ] && [ "$ready" = "$nodes" ]; then
  record PASS "one AWS node-data broker is ready on every eligible Node" "ready=${ready}, desired=${desired}, nodes=${nodes}"
else
  record FAIL "one AWS node-data broker is ready on every eligible Node" \
    "ready=${ready:-missing}, desired=${desired:-missing}, nodes=${nodes:-unknown}"
fi

api_identity="$(kubectl -n "$NS" get pods -l 'app.kubernetes.io/name=topograph,app.kubernetes.io/instance=gpustack-operator' -o json 2>/dev/null \
  | jq --arg sa "$API_SA" '[.items[] | select(.spec.serviceAccountName == $sa) | .spec.containers[].env[]? | select(.name=="AWS_CONTAINER_CREDENTIALS_FULL_URI")] | length')"
pod_rows="$(kubectl -n "$NS" get pods -l 'app.kubernetes.io/name=node-data-broker,app.kubernetes.io/instance=gpustack-operator' -o json 2>/dev/null \
  | jq -r '.items[] | [.metadata.name,.spec.nodeName,.spec.serviceAccountName,([.status.containerStatuses[]? | select(.ready==true)] | length)] | @tsv')"
pod_count="$(printf '%s\n' "$pod_rows" | grep -c . || true)"
not_ready="$(printf '%s\n' "$pod_rows" | awk -F '\t' '$4 < 1 {n++} END {print n+0}')"

if [ "$api_identity" -gt 0 ]; then
  record PASS "Topograph API uses the Pod Identity-bound ServiceAccount" "ServiceAccount=${API_SA}, credential-endpoint=${api_identity}"
else
  record FAIL "Topograph API uses the Pod Identity-bound ServiceAccount" "ServiceAccount=${API_SA}, credential-endpoint=${api_identity}"
fi
if [ "$pod_count" = "${nodes:-0}" ] && [ "$not_ready" = 0 ]; then
  record PASS "all provider-backed broker startup probes completed" "${pod_count} of ${nodes} broker containers Ready"
else
  record FAIL "all provider-backed broker startup probes completed" "pods=${pod_count}, not-ready=${not_ready}"
fi

report
[ "$FAILS" -eq 0 ] || exit 1
