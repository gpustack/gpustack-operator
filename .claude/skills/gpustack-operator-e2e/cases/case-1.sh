#!/usr/bin/env bash
#
# CASE 1 — CPU-only scheduling chain materializes   (READ-ONLY, mandatory)
#
#   case-1.sh <NS>
#
# Asserts the general (CPU-only) chain end to end: the operator core is healthy
# (delegated to assert-core.sh), NFD labels the node with CPU identity, the
# Worker derives general capacity labels, and the pooling chain materializes the
# general ResourceFlavor → InstanceType (Active) → its backing ClusterQueue →
# LocalQueue. On a GPU-less cluster the device-manager DaemonSets schedule zero
# pods — expected; this case covers the general chain only.
#
# Post-refactor (specs/2026-06-29-instancetype-unified-pool-refactor.md): there is
# exactly ONE isolated ClusterQueue per pool and NO Cohort — this case asserts zero
# Cohort objects. Names are gpustack-${gKey}-${os}-${arch}[-${count}c] with full
# os/arch (the gKey is non-empty, e.g. "generic"), so the chain is matched by the
# "gpustack-" prefix, not a legacy empty-key "gpustack--". Level-based, safe to re-run.
set -uo pipefail

NS="${1:?usage: case-1.sh <NS>}"
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"

# Operator core first (rollout / revision==HEAD / apiservices / CRDs / sub-releases).
bash "$LIB/assert-core.sh" "$NS" || exit 1

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
assert_nonempty() { # check  command-output
  if [ -n "$2" ]; then record PASS "$1" "$(echo "$2" | tr '\n' ' ' | cut -c1-60)"; else record FAIL "$1" "none found"; fi
}

# NFD labeled the node(s) with CPU identity and marked GPU-less nodes (non-)acceleratable.
nfd=$(kubectl get nodes -o json | grep -Eo '"feature\.gpustack\.ai/(cpu-[a-z]+|acceleratable)"[^,]*' | sort -u)
assert_nonempty "NFD cpu/acceleratable labels" "$nfd"

# Worker derived general capacity labels on the <node>-gpustack-worker NodeFeature.
gen=$(kubectl get nodefeatures -A -o json | grep -Eo '"general\.feature\.gpustack\.ai/[^"]+"' | sort -u)
assert_nonempty "Worker general.* labels" "$gen"

# The pooling chain materialized the general objects (all prefixed "gpustack-"). A CPU
# flavor carries the -${count}c suffix; the CQ/InstanceType is the flavor name without it.
assert_nonempty "ResourceFlavor (general)" "$(kubectl get resourceflavors.kueue.x-k8s.io -o name | grep -E 'gpustack-.*-[0-9]+c$')"
assert_nonempty "ClusterQueue (general)"   "$(kubectl get clusterqueues.kueue.x-k8s.io   -o name | grep 'gpustack-')"
assert_nonempty "LocalQueue (general)"     "$(kubectl get localqueues.kueue.x-k8s.io -A  -o name | grep 'gpustack-fnv64-')"

# The InstanceType materialized and reports Active with an entrance LocalQueue (Story 1/2/6:
# the pool surfaces as a real CRD whose .status the reconciler writes).
itActive=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{range .items[?(@.status.phase=="Active")]}{.metadata.name}{" entrance="}{.status.entrance}{"\n"}{end}' 2>/dev/null | grep 'gpustack-')
assert_nonempty "InstanceType Active (+entrance)" "$itActive"

# Zero Cohort objects — Cohort was removed entirely; one isolated CQ per pool (F3c).
cohorts=$(kubectl get cohorts.kueue.x-k8s.io -A --no-headers 2>/dev/null | grep -c . || true)
[ "${cohorts:-0}" = "0" ] && record PASS "zero Cohort objects" "no cohorts.kueue.x-k8s.io" \
  || record FAIL "zero Cohort objects" "${cohorts} Cohort(s) present — CohortReconciler should be gone (F3c)"

echo
echo "== CASE 1 — CPU-only scheduling chain materializes =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} chain check(s). The chain is driven by the feature.gpustack.ai/cpu-* labels;"
  echo "confirm NFD and Kueue pods are Ready, then diagnose:"
  echo "  kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200"
  exit 1
fi
echo "CASE 1 PASS"
