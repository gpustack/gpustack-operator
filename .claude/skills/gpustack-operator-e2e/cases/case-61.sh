#!/usr/bin/env bash
#
# CASE 61 — A Service deleted out of band comes back, and the watch is what brought it   (MUTATING, self-recovering)
#
#   case-61.sh <NS>
#
# Goal:        The ModelDeployment controller watches the Service it reconciles, so an externally
#              deleted or edited Service is corrected by the next reconcile rather than at a resync
#              hours away. The code path has no observable output of its own: a controller built
#              without the watch still creates the Service on first reconcile and still renders it
#              correctly, so every existing reading about that Service is identical with the watch
#              removed.
#
#              THE FIGURE IS THE UID, AND THE ATTRIBUTION IS THE REPLICAS' RESOURCE VERSIONS. A row
#              that only checked the Service exists at the end would pass if the delete had silently
#              failed, so the recreated Service must carry a DIFFERENT uid. And a row that stopped
#              there would pass with the watch removed on any cluster where a replica happened to
#              write its status in the same window -- the controller also owns the replicas, so a Pod
#              event wakes it and the Service comes back for a reason this case is not about. So the
#              replicas' resourceVersions are captured either side of the window: unchanged means no
#              Pod event could have driven the reconcile, and the Service watch is what is left.
#
#              A shortage, a filler and a quota edit are all deliberately absent. Whether the group
#              is admitted has nothing to do with Service reconciliation, and the quieter the
#              replicas are the sharper the attribution gets.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and an
#              InstanceType. NO GPU and no KV cache backend: the deployment names a
#              KVCachePoolBinding that deliberately does not exist, because nothing here reads a
#              connector, and its replicas are never expected to serve. EXITS 2 (input required)
#              when the cluster has no InstanceType.
#
# Inputs:      All real, nothing mocked. One single-role ModelDeployment. Its role carries an
#              explicit template.image because a CPU-only InstanceType has observed no accelerator
#              and the operator can synthesize no engine image; nothing here reads what it runs.
#              Override the image with E2E_MD_IMAGE, the InstanceType with E2E_MD_INSTANCE_TYPE, the
#              recreate bound with E2E_SVC_RECREATE_BOUND.
#
# Expected:    - the deployment's own Service is created;
#              - after `kubectl delete svc`, a Service of the same name exists again within the
#                bound, carrying a different uid;
#              - no replica Pod changed between the delete and the recreate, so the correction is
#                attributable to the Service watch and to nothing else.
#
# Cleanup:     Trap deletes the deployment and, if its replicas are wedged behind Kueue's finalizer,
#              the Workload that holds them. Idempotent, runs on pass AND fail, safe to re-run. It
#              creates no cluster-scoped object, changes no ClusterQueue and touches no baseline.
#
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-61.sh <NS>}"

MD=case61-svc
BINDING="${E2E_MD_BINDING:-case61-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

# How long the correction gets. The informer cache resyncs every hour by default
# (--informer-cache-resync-period), so anything inside this bound separates a watch from a resync by
# a wide margin; the bound exists to turn a missing watch into a FAIL rather than to measure how
# quick the watch is.
RECREATE_BOUND="${E2E_SVC_RECREATE_BOUND:-90}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
if [ -z "$IT" ]; then
  echo "[case-61] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# Every Pod of the group, as name=resourceVersion, sorted so two samples compare as text.
replica_versions() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.resourceVersion}{"\n"}{end}' 2>/dev/null \
    | sort | tr '\n' ' '
}

# Delete a wedged group's Workload by hand. Kueue holds a finalizer on every Pod of a group and
# releases it only when the group finishes or the Workload is deleted, and the group these render is
# annotated serving, which Kueue defines as never finished. Without this a failed run leaves
# finalizers that contaminate every case after it.
force_release() {
  local row wl uids u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    for u in $uids; do
      case " ${row#*=} " in
        *" $u "*)
          kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
            --ignore-not-found --wait=false >/dev/null 2>&1
          break
          ;;
      esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF

  return 0
}

cleanup() {
  echo
  echo "[case-61] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  force_release
}
trap cleanup EXIT

echo "== 1. a single-role deployment, and the Service it is reached through =="

# The apply's output is KEPT. Discarded, a schema or webhook refusal surfaces only as a timeout
# whose FAIL row blames the watch for an object that was never created.
apply_out="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine: vllm
  engineVersion: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
    - name: server
      instanceType: ${IT}
      replicas: 1
      template:
        image: ${IMAGE}
        command: ["/pause"]
YAML
)"
case "$apply_out" in
  *created* | *configured* | *unchanged*) ;;
  *)
    record FAIL "the deployment's Service is created" \
      "the deployment was not accepted, so nothing below has a subject: $(printf '%s' "$apply_out" | tr '\n' ' ' | cut -c1-220)"
    echo
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}" | column -t -s '|'
    exit 1
    ;;
esac

first_uid=""
for _ in $(seq 1 30); do
  first_uid="$(kubectl -n "$NS" get svc "$MD" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  [ -n "$first_uid" ] && break
  sleep 3
done
if [ -z "$first_uid" ]; then
  record FAIL "the deployment's Service is created" \
    "no svc/${MD} within 90s, so there is nothing to delete and nothing to correct"
  echo
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}" | column -t -s '|'
  exit 1
fi
record PASS "the deployment's Service is created" "svc/${MD} uid=${first_uid}"

echo "== 2. wait for the replicas to go quiet =="

# Quiescence is a precondition of the attribution, not a nicety. While the replicas are still being
# written the controller is being woken by its Pod watch several times a second, and a Service
# deleted in that window comes back for a reason this case is not about.
quiet_for=0
last="$(replica_versions)"
for _ in $(seq 1 40); do
  sleep 3
  now="$(replica_versions)"
  if [ "$now" = "$last" ]; then
    quiet_for=$((quiet_for + 3))
    [ "$quiet_for" -ge 9 ] && break
  else
    quiet_for=0
  fi
  last="$now"
done
echo "[case-61] replicas unchanged for ${quiet_for}s: ${last:-<none>}"
before="$last"

echo "== 3. delete the Service out of band =="
del_out="$(kubectl -n "$NS" delete svc "$MD" 2>&1)"
echo "$del_out"
started=$SECONDS

new_uid=""
elapsed=0
for _ in $(seq 1 "$RECREATE_BOUND"); do
  new_uid="$(kubectl -n "$NS" get svc "$MD" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  if [ -n "$new_uid" ] && [ "$new_uid" != "$first_uid" ]; then
    elapsed=$((SECONDS - started))
    break
  fi
  new_uid=""
  sleep 1
done

after="$(replica_versions)"

echo "== 4. the readings =="
if [ -n "$new_uid" ]; then
  record PASS "a Service of the same name exists again, with a different uid" \
    "uid ${first_uid} -> ${new_uid} in ${elapsed}s; a same-uid Service would have meant the delete never took effect"
else
  # Named apart from a rendering failure: the Service may be absent, or present under the ORIGINAL
  # uid, and only the first is a missing correction.
  still="$(kubectl -n "$NS" get svc "$MD" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  case "$still" in
    "$first_uid") record FAIL "a Service of the same name exists again, with a different uid" \
      "svc/${MD} still carries uid ${first_uid} after ${RECREATE_BOUND}s, so the delete never took effect and this case measured nothing" ;;
    "") record FAIL "a Service of the same name exists again, with a different uid" \
      "no svc/${MD} ${RECREATE_BOUND}s after the delete; the controller was not woken, which is what a missing Service watch looks like" ;;
    *) record FAIL "a Service of the same name exists again, with a different uid" \
      "svc/${MD} carries uid ${still}, which is neither the original nor one this poll observed" ;;
  esac
fi

# The attribution. A SKIP rather than a FAIL: a replica writing its status in the window is not a
# defect in anything, it just means this run cannot say WHICH watch did the work.
if [ "$before" = "$after" ]; then
  record PASS "nothing but the Service watch could have woken the controller" \
    "every replica's resourceVersion is unchanged across the window: ${after:-<no replicas>}"
else
  record SKIP "nothing but the Service watch could have woken the controller" \
    "a replica changed in the window, so the row above passes unattributed: before=[${before}] after=[${after}]"
fi

echo
echo "== CASE 61 — A Service deleted out of band comes back, and the watch is what brought it =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The Service the ModelDeployment controller reconciles must be"
  echo "watched by it, or an out-of-band delete stands until the next resync."
  echo "Diagnose: kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200 | grep -i modeldeployment"
  exit 1
fi
echo "CASE 61 PASS"
