#!/usr/bin/env bash
#
# CASE 1 — CPU-only scheduling chain materializes   (READ-ONLY, mandatory)
#
#   case-1.sh <NS>
#
# Goal:        Prove the general (CPU-only) scheduling chain materializes end to end and
#              that NO Cohort is created — exactly one isolated ClusterQueue per pool.
# Environment: Any cluster with a materialized general pool (operator core healthy, NFD +
#              Worker running). No GPU — device-manager DaemonSets scheduling zero pods on a
#              GPU-less node is expected. Read-only, level-based, safe to re-run. Waits for the
#              chain to converge rather than reading it once: on a fresh install the worker's
#              first ResourceFlavor write can lose a race with the Kueue webhook's endpoints
#              ("connect: connection refused"), and the reconciler then retries on its own
#              backoff — measured at ~40s. A single-shot read right after a deploy reports that
#              as a chain that never materialized.
# Inputs:      None injected — reads live cluster state (operator-core health delegated to
#              assert-core.sh), plus one server-side dry-run CREATE of an InstanceType, which
#              persists nothing. Nothing mocked.
# Expected:    - assert-core.sh passes (rollout / running revision == HEAD / apiservices /
#                CRDs / the four bundled applications in the operator's own release);
#              - NFD stamps feature.gpustack.ai/cpu-* + acceleratable labels;
#              - the Worker derives general.feature.gpustack.ai/* capacity labels;
#              - the general ResourceFlavor (name ...-<count>c[-fnv64-<hash>]), its ClusterQueue, its
#                InstanceType (Active + entrance LocalQueue), and its LocalQueue all exist
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

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-1.sh <NS>}"
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"

# Operator core first (rollout / revision==HEAD / apiservices / CRDs / bundled applications).
bash "$LIB/assert-core.sh" "$NS" || exit 1

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
assert_nonempty() { # check  command-output
  if [ -n "$2" ]; then record PASS "$1" "$(echo "$2" | tr '\n' ' ' | cut -c1-60)"; else record FAIL "$1" "none found"; fi
}

# await_nonempty re-runs a read until it returns something, up to ~2min. The chain is level-based
# and materializes asynchronously, so "not there yet" and "never going to be there" look identical
# in a single read — only the passage of time tells them apart.
await_nonempty() { # command-string
  local out=""
  for _ in $(seq 1 40); do
    out=$(eval "$1" 2>/dev/null)
    [ -n "$out" ] && break
    sleep 3
  done
  printf '%s' "$out"
}

# NFD labeled the node(s) with CPU identity and marked GPU-less nodes (non-)acceleratable.
nfd=$(kubectl get nodes -o json | grep -Eo '"feature\.gpustack\.ai/(cpu-[a-z]+|acceleratable)"[^,]*' | sort -u)
assert_nonempty "NFD cpu/acceleratable labels" "$nfd"

# Worker derived general capacity labels on the <node>-gpustack-worker NodeFeature.
gen=$(kubectl get nodefeatures -A -o json | grep -Eo '"general\.feature\.gpustack\.ai/[^"]+"' | sort -u)
assert_nonempty "Worker general.* labels" "$gen"

# The pooling chain materialized the general objects (all prefixed "gpustack-"). A CPU
# flavor carries the -${count}c suffix and, since flavors split by topology profile, a trailing
# -fnv64-<16 hex> profile; the CQ/InstanceType is the flavor name without them.
# The flavor is the head of the chain, so it is the one worth waiting on; the rest follow it
# within a reconcile and are read directly.
assert_nonempty "ResourceFlavor (general)" "$(await_nonempty "kubectl get resourceflavors.kueue.x-k8s.io -o name | grep -E 'gpustack-.*-[0-9]+c(-fnv64-[0-9a-f]{16})?\$'")"
assert_nonempty "ClusterQueue (general)"   "$(await_nonempty "kubectl get clusterqueues.kueue.x-k8s.io   -o name | grep 'gpustack-'")"
assert_nonempty "LocalQueue (general)"     "$(await_nonempty "kubectl get localqueues.kueue.x-k8s.io -A  -o name | grep 'gpustack-fnv64-'")"

# The InstanceType materialized and reports Active with an entrance LocalQueue (the pool
# surfaces as a real CRD whose .status the reconciler writes).
itActive=$(await_nonempty "kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{range .items[?(@.status.phase==\"Active\")]}{.metadata.name}{\" entrance=\"}{.status.entrance}{\"\n\"}{end}' | grep 'gpustack-'")
assert_nonempty "InstanceType Active (+entrance)" "$itActive"

# The general InstanceType materializes spec.os/spec.arch from the backing ClusterQueue's
# kubernetes.io/os|arch labels: those live only as schedule labels, never in the CQ notes, so
# reading them from the notes used to blank spec.os/spec.arch on every reconcile. Assert the
# derived spec matches the InstanceType's OWN schedule labels — not a node's, since a
# heterogeneous cluster hosts several os/arch pools and the first node need not be this pool.
osarch=$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); l=it.get('metadata',{}).get('labels',{}); n=it['metadata']['name']
    if not n.startswith('gpustack-') or s.get('acceleratable'): continue
    los,larch=l.get('kubernetes.io/os',''),l.get('kubernetes.io/arch','')
    sos,sarch=s.get('os',''),s.get('arch','')
    ok=sos!='' and sarch!='' and sos==los and sarch==larch
    print(('PASS' if ok else 'FAIL')+'|'+'%s spec=%s/%s label=%s/%s'%(n,sos or '<empty>',sarch or '<empty>',los or '<empty>',larch or '<empty>'))
    break
else:
    print('FAIL|no general InstanceType found')
" 2>/dev/null)
if [ "${osarch%%|*}" = "PASS" ]; then
  record PASS "InstanceType materializes spec.os/arch" "${osarch#*|} (from CQ labels, not notes)"
else
  record FAIL "InstanceType materializes spec.os/arch" "${osarch#*|} — spec.os/arch must equal the CQ kubernetes.io/os|arch labels, not be blanked"
fi

# The InstanceType Default webhook stamps, on CREATE, the acceleratable boolean and the queue-entrance
# label naming the type's LocalQueue. The entrance label has no other writer
# (webhooks/worker/instance_type.go), so finding it on the derived general type proves that webhook is
# installed and ran; its value must equal the type's own status.entrance.
stampedIT=$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); st=it.get('status',{}) or {}; l=it.get('metadata',{}).get('labels',{}) or {}; n=it['metadata']['name']
    if not n.startswith('gpustack-') or s.get('acceleratable'): continue
    acc=l.get('feature.gpustack.ai/acceleratable',''); ent=l.get('schedule.gpustack.ai/queue-entrance','')
    ok=acc=='false' and ent!='' and ent==st.get('entrance','')
    print(('PASS' if ok else 'FAIL')+'|'+'%s acceleratable=%s queue-entrance=%s status.entrance=%s'%(n,acc or '<none>',ent or '<none>',st.get('entrance') or '<none>'))
    break
else:
    print('FAIL|no general InstanceType found')
" 2>/dev/null)
if [ "${stampedIT%%|*}" = "PASS" ]; then
  record PASS "InstanceType Default webhook stamped its labels" "${stampedIT#*|}"
else
  record FAIL "InstanceType Default webhook stamped its labels" "${stampedIT#*|} — the Default webhook must stamp acceleratable=false and a queue-entrance label equal to status.entrance"
fi

# The validating webhook refuses an InstanceType with no unit spec on CREATE. A server-side dry run
# against the v1alpha1 CRD persists nothing; the unversioned name would resolve to the aggregated v1
# API, which answers a dry run without running the CRD's admission.
errUnit=$(kubectl apply --dry-run=server -f - 2>&1 <<'EOF'
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: e2e-case1-nounit
spec:
  generalGroup: e2e1probe
  acceleratable: false
  os: linux
  arch: amd64
EOF
)
echo "$errUnit" | grep -qiE 'unitResources|localStorage' \
  && record PASS "CREATE rejects an InstanceType with no unit spec" "validating webhook: $(echo "$errUnit" | tr '\n' ' ' | cut -c1-90)" \
  || record FAIL "CREATE rejects an InstanceType with no unit spec" "not refused on the unit spec: $(echo "$errUnit" | tr '\n' ' ' | cut -c1-90)"

# Zero Cohort objects — Cohort was removed entirely; one isolated CQ per pool.
cohorts=$(kubectl get cohorts.kueue.x-k8s.io -A --no-headers 2>/dev/null | grep -c . || true)
[ "${cohorts:-0}" = "0" ] && record PASS "zero Cohort objects" "no cohorts.kueue.x-k8s.io" \
  || record FAIL "zero Cohort objects" "${cohorts} Cohort(s) present — CohortReconciler should be gone"

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
