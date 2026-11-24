#!/usr/bin/env bash
#
# CASE 1 — CPU-only scheduling chain materializes   (READ-ONLY, mandatory)
#
#   case-1.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any
# Goal:        Prove the general (CPU-only) scheduling chain materializes end to end and
#              that NO Cohort is created — exactly one isolated ClusterQueue per pool.
# Environment: Any cluster with a materialized general pool (operator core healthy, NFD +
#              Worker running). No GPU is required — device-manager DaemonSets scheduling
#              zero pods on a GPU-less node is expected. Read-only, level-based, safe to
#              re-run. Waits for the chain to converge rather than reading it once: on a
#              fresh install the worker's first ResourceFlavor write can lose a race with
#              the Kueue webhook's endpoints ("connect: connection refused"), and the
#              reconciler retries on its own backoff (~40s).
# Inputs:      None injected — reads live cluster state (operator-core health delegated to
#              assert-core.sh), plus one server-side dry-run CREATE of an InstanceType, which
#              persists nothing. Nothing mocked.
# Expected:    - assert-core.sh passes (rollout / running revision == HEAD / apiservices /
#                CRDs / the four bundled applications in the operator's own release);
#              - NFD stamps feature.gpustack.ai/cpu-* + acceleratable labels;
#              - the Worker derives general.feature.gpustack.ai/* capacity labels;
#              - the general ResourceFlavor (name ...-<count>c[-fnv64-<hash>]), its ClusterQueue,
#                its InstanceType (Active + entrance LocalQueue), and its LocalQueue all exist
#                under the "gpustack-" prefix;
#              - the general InstanceType's spec.os/spec.arch equal its ClusterQueue's
#                kubernetes.io/os|arch schedule labels (read from labels, never blanked);
#              - the general InstanceType carries feature.gpustack.ai/acceleratable=false and a
#                schedule.gpustack.ai/queue-entrance label equal to its status.entrance, both of
#                which only the InstanceType Default webhook writes;
#              - a CREATE of an InstanceType with no unit spec is refused by the validating webhook;
#              - zero Cohort objects exist.
# Cleanup:     None — read-only, no trap.
set -uo pipefail

# shellcheck disable=SC2034 # Read by _rows-lib.sh.
CASE_ID=1
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"

NS="${1:?usage: case-1.sh <NS>}"
LIB="$(cd "${CASE_DIR}/../../_e2e-lib/scripts" && pwd)"

# Operator core first (rollout / revision==HEAD / apiservices / CRDs / bundled applications).
bash "${LIB}/assert-core.sh" "$NS" || exit 1

assert_nonempty() {
  local check="$1" val="$2" summary
  if [ -n "$val" ]; then
    summary="$(printf '%s' "$val" | tr '\n' ' ' | cut -c1-60)"
    record PASS "$check" "$summary"
  else
    record FAIL "$check" "none found"
  fi
}

# await_nonempty <fn-name> re-runs <fn-name> until it outputs non-empty text, up to ~2min.
await_nonempty() {
  local fn="$1" out=""
  for _ in $(seq 1 40); do
    out="$("$fn" 2>/dev/null)" || out=""
    [ -n "$out" ] && break
    sleep 3
  done
  printf '%s' "$out"
}

probe_general_rf() {
  kubectl get resourceflavors.kueue.x-k8s.io -o name | grep -E 'gpustack-.*-[0-9]+c(-fnv64-[0-9a-f]{16})?$'
}

probe_general_cq() {
  kubectl get clusterqueues.kueue.x-k8s.io -o name | grep 'gpustack-'
}

probe_general_lq() {
  kubectl get localqueues.kueue.x-k8s.io -A -o name | grep 'gpustack-fnv64-'
}

probe_active_it() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[?(@.status.phase=="Active")]}{.metadata.name}{" entrance="}{.status.entrance}{"\n"}{end}' \
    | grep 'gpustack-'
}

# NFD labeled the node(s) with CPU identity and marked GPU-less nodes (non-)acceleratable.
nfd="$(kubectl get nodes -o json | grep -Eo '"feature\.gpustack\.ai/(cpu-[a-z]+|acceleratable)"[^,]*' | sort -u)"
assert_nonempty "NFD cpu/acceleratable labels" "$nfd"

# Worker derived general capacity labels on the <node>-gpustack-worker NodeFeature.
gen="$(kubectl get nodefeatures -A -o json | grep -Eo '"general\.feature\.gpustack\.ai/[^"]+"' | sort -u)"
assert_nonempty "Worker general.* labels" "$gen"

# The pooling chain materialized the general objects (all prefixed "gpustack-").
assert_nonempty "ResourceFlavor (general)" "$(await_nonempty probe_general_rf)"
assert_nonempty "ClusterQueue (general)"   "$(await_nonempty probe_general_cq)"
assert_nonempty "LocalQueue (general)"     "$(await_nonempty probe_general_lq)"

# The InstanceType materialized and reports Active with an entrance LocalQueue.
assert_nonempty "InstanceType Active (+entrance)" "$(await_nonempty probe_active_it)"

# The general InstanceType materializes spec.os/spec.arch from the backing ClusterQueue's
# kubernetes.io/os|arch schedule labels.
osarch="$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json, sys
for it in json.load(sys.stdin).get('items', []):
    s = it.get('spec', {})
    l = it.get('metadata', {}).get('labels', {})
    n = it['metadata']['name']
    if not n.startswith('gpustack-') or s.get('acceleratable'):
        continue
    los, larch = l.get('kubernetes.io/os', ''), l.get('kubernetes.io/arch', '')
    sos, sarch = s.get('os', ''), s.get('arch', '')
    ok = sos != '' and sarch != '' and sos == los and sarch == larch
    print(('PASS' if ok else 'FAIL') + '|' + '%s spec=%s/%s label=%s/%s' % (
        n, sos or '<empty>', sarch or '<empty>', los or '<empty>', larch or '<empty>'))
    break
else:
    print('FAIL|no general InstanceType found')
" 2>/dev/null)"
if [ "${osarch%%|*}" = "PASS" ]; then
  record PASS "InstanceType materializes spec.os/arch" "${osarch#*|} (from CQ labels, not notes)"
else
  record FAIL "InstanceType materializes spec.os/arch" "${osarch#*|} — spec.os/arch must equal the CQ kubernetes.io/os|arch labels, not be blanked"
fi

# The InstanceType Default webhook stamps, on CREATE, the acceleratable boolean and the
# queue-entrance label naming the type's LocalQueue.
stampedIT="$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json, sys
for it in json.load(sys.stdin).get('items', []):
    s = it.get('spec', {})
    st = it.get('status', {}) or {}
    l = it.get('metadata', {}).get('labels', {}) or {}
    n = it['metadata']['name']
    if not n.startswith('gpustack-') or s.get('acceleratable'):
        continue
    acc = l.get('feature.gpustack.ai/acceleratable', '')
    ent = l.get('schedule.gpustack.ai/queue-entrance', '')
    ok = acc == 'false' and ent != '' and ent == st.get('entrance', '')
    print(('PASS' if ok else 'FAIL') + '|' + '%s acceleratable=%s queue-entrance=%s status.entrance=%s' % (
        n, acc or '<none>', ent or '<none>', st.get('entrance') or '<none>'))
    break
else:
    print('FAIL|no general InstanceType found')
" 2>/dev/null)"
if [ "${stampedIT%%|*}" = "PASS" ]; then
  record PASS "InstanceType Default webhook stamped its labels" "${stampedIT#*|}"
else
  record FAIL "InstanceType Default webhook stamped its labels" "${stampedIT#*|} — the Default webhook must stamp acceleratable=false and a queue-entrance label equal to status.entrance"
fi

# The validating webhook refuses an InstanceType with no unit spec on CREATE.
errUnit="$(kubectl apply --dry-run=server -f - 2>&1 <<'EOF'
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: e2e-c1-nounit
spec:
  generalGroup: e2ec1probe
  acceleratable: false
  os: linux
  arch: amd64
EOF
)"
errSummary="$(printf '%s' "$errUnit" | tr '\n' ' ' | cut -c1-90)"
if printf '%s' "$errUnit" | grep -qiE 'unitResources|localStorage'; then
  record PASS "CREATE rejects an InstanceType with no unit spec" "validating webhook: ${errSummary}"
else
  record FAIL "CREATE rejects an InstanceType with no unit spec" "not refused on the unit spec: ${errSummary}"
fi

# Zero Cohort objects — one isolated ClusterQueue per pool.
cohorts="$(kubectl get cohorts.kueue.x-k8s.io -A --no-headers 2>/dev/null | grep -c . || true)"
if [ "${cohorts:-0}" = "0" ]; then
  record PASS "zero Cohort objects" "no cohorts.kueue.x-k8s.io"
else
  record FAIL "zero Cohort objects" "${cohorts} Cohort(s) present — CohortReconciler should be gone"
fi

echo
echo "== CASE 1 — CPU-only scheduling chain materializes =="
print_rows

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} chain check(s). The chain is driven by the feature.gpustack.ai/cpu-* labels;"
  echo "confirm NFD and Kueue pods are Ready, then diagnose:"
  echo "  kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200"
  exit 1
fi
echo "CASE 1 PASS"
