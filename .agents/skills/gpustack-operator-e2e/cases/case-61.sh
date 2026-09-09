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
#              THE FIGURE IS THE UID, AND THE ATTRIBUTION IS EVERY OTHER OBJECT THIS CONTROLLER IS
#              WOKEN BY. A row that only checked the Service exists at the end would pass if the
#              delete had silently failed, so the recreated Service must carry a DIFFERENT uid. And a
#              row that stopped there would pass with the watch removed on any cluster where some
#              other watched object happened to be written in the same window: the controller also
#              owns the replicas and watches the InstanceType, the KVCachePoolBinding, the
#              KVCachePool and the KVCacheBackend -- the last four with no generation predicate, so a
#              status write on any of them wakes it and the Service comes back for a reason this case
#              is not about. So all of them are sampled either side of the window, and the row says
#              only what that sample supports: nothing else the controller watches moved.
#
#              EVERY COMPARISON HERE IS ALSO ASKED WHAT IT RECORDS WHEN BOTH SIDES ARE EMPTY, because
#              an absence and a quiet object read alike. With no replica Pod at all the quiescence
#              loop would declare quiescence after 9s of nothing and the attribution would compare ""
#              to "", so a replica has to be OBSERVED before either reading means anything.
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
#              - nothing else this controller watches changed between the delete and the recreate,
#                which leaves the Service watch as what drove the reconcile. SKIP, not FAIL, when
#                something did move or when no replica was ever observed: neither is a defect in
#                anything, it just means that run cannot say which watch did the work.
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

# Every Pod of the group, as name=resourceVersion, sorted so two samples compare as text. Used for
# the quiescence loop, where the replicas are the only churn source worth waiting out.
replica_versions() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.resourceVersion}{"\n"}{end}' 2>/dev/null \
    | sort | tr '\n' ' '
}

# One kind's resourceVersions, or a loud sentinel line when they could not be observed.
#
# THE SENTINEL IS NOT A VALUE THE ATTRIBUTION MAY COMPARE, and the guard below enforces that. This
# helper exists because discarding a failed read would make an unreadable kind indistinguishable from
# an empty one, so it would drop silently out of both samples while the row went on claiming it --
# but emitting a sentinel only moves the problem unless the consumer refuses to compare it, because
# two sentinels are equal to each other. See the attribution row.
#
# Two failures are told apart, because only one of them is a fault:
#
#   NOT-FOUND    the object is gone. A named `kubectl get` exits nonzero with empty stdout for this
#                exactly as it does for an unreadable kind, so the error text is what separates them.
#   READ-FAILED  the kind itself could not be read -- CRD absent, RBAC, or a transport failure the
#                retrying shim ran out of attempts on.
#
# A LIST call over a kind with no objects is neither: it prints nothing and exits 0, which is an
# observation of zero objects and is left as such.
#
# STDERR IS KEPT SEPARATE RATHER THAN FOLDED IN WITH `2>&1`, which would be the way to avoid a
# scratch file entirely. The retrying kubectl shim writes to stderr on a call that eventually
# succeeds -- `kubectl-shim: transport failure, retrying ...` -- so folding it in would put that
# notice into the sample as a line pretending to be a resourceVersion.
#
# One scratch file for the whole case, not one per call: this runs on every quiescence poll and twice
# more for the attribution, so a per-call mktemp both multiplies its own failure path and leaks a
# file per in-flight call if the runner kills the case. An empty SAMPLE_ERR means mktemp failed; the
# redirect then goes to /dev/null and every failure classifies as READ-FAILED, which is the
# conservative sentinel rather than a wrong one.
SAMPLE_ERR="$(mktemp 2>/dev/null || true)"

sample() {
  local label="$1" out
  shift
  if out="$(kubectl "$@" 2>"${SAMPLE_ERR:-/dev/null}")"; then
    # Command substitution strips the trailing newline, so it is put back here rather than left to
    # glue this kind's last line onto the next kind's first one. A kind with no objects prints
    # nothing at all, which is why the empty case is skipped instead of emitting a blank line.
    if [ -n "$out" ]; then
      printf '%s\n' "$out"
    fi
  elif [ -n "$SAMPLE_ERR" ] && command grep -qi 'not found' "$SAMPLE_ERR"; then
    printf '%s=NOT-FOUND\n' "$label"
  else
    printf '%s=READ-FAILED\n' "$label"
  fi

  return 0
}

# Everything this controller is woken by EXCEPT the Service under test, as
# kind/name=resourceVersion. The controller Owns the replica Pods and the Service, and Watches the
# InstanceType, the KVCachePoolBinding, the KVCachePool and the KVCacheBackend; its own spec is
# filtered by a GenerationChangedPredicate and nothing here edits it, but it is sampled anyway so
# the list needs no argument about that.
watched_versions() {
  {
    sample pods -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
      -o jsonpath="{range .items[*]}pod/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample instancetype get instancetypes.worker.gpustack.ai "$IT" \
      -o jsonpath="instancetype/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}"
    sample modeldeployment -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" \
      -o jsonpath="modeldeployment/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}"
    sample kvcachepoolbindings get kvcachepoolbindings.worker.gpustack.ai -A \
      -o jsonpath="{range .items[*]}kvcachepoolbinding/{.metadata.namespace}/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample kvcachepools get kvcachepools.worker.gpustack.ai \
      -o jsonpath="{range .items[*]}kvcachepool/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
    sample kvcachebackends get kvcachebackends.worker.gpustack.ai \
      -o jsonpath="{range .items[*]}kvcachebackend/{.metadata.name}={.metadata.resourceVersion}{\"\\n\"}{end}"
  } | sort | tr '\n' ' '
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
  [ -n "$SAMPLE_ERR" ] && rm -f "$SAMPLE_ERR"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  # Retried rather than done once. force_release finds the Workload through the Pods it owns, so a
  # single shot right after the delete finds nothing when admission is still in flight or when the
  # Pod list races the deletion -- and the wedged finalizers it exists to clear then survive, which
  # is exactly the contamination it exists to prevent. Each round is cheap and the loop ends as soon
  # as the Pods are gone.
  for _ in $(seq 1 12); do
    sleep 5
    force_release
    [ -z "$(replica_versions)" ] && break
  done
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
# The gate is that the object is THERE, not that the apply printed one of three words. A substring
# match would take a refusal whose text happens to contain "created" for success, and matching the
# exact word instead would make the gate depend on whether this run is the first over a surviving
# namespace. Reading the object back answers the question the checks below actually need.
if ! kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o name >/dev/null 2>&1; then
  record FAIL "the deployment's Service is created" \
    "no modeldeployment/${MD} after the apply, so nothing below has a subject. The apply said: $(printf '%s' "${apply_out:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  echo
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}" | column -t -s '|'
  exit 1
fi

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

echo "== 2. wait for a replica to appear, then for the replicas to go quiet =="

# A replica has to be OBSERVED before "unchanged" carries any information. With no Pod at all
# replica_versions returns the empty string forever, the loop below compares "" to "" and declares
# quiescence after 9s of nothing, and the attribution row would then compare two empty samples and
# pass. Whether Kueue admits this group is not this case's subject and not something it arranges, so
# the absence is recorded and carried rather than treated as a failure.
replicas_seen=false
for _ in $(seq 1 40); do
  if [ -n "$(replica_versions)" ]; then
    replicas_seen=true
    break
  fi
  sleep 3
done

# Quiescence is a precondition of the attribution, not a nicety. While the replicas are still being
# written the controller is being woken by its Pod watch several times a second, and a Service
# deleted in that window comes back for a reason this case is not about.
#
# Skipped outright when no replica was ever observed: with nothing to sample this loop would compare
# "" to "" for its full 40 rounds and then declare quiescence, spending two minutes to produce a
# reading whose value is already decided. The attribution is disqualified either way below.
quiet_for=0
last="$(replica_versions)"
if [ "$replicas_seen" = true ]; then
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
fi
echo "[case-61] replica(s) observed: ${replicas_seen}; unchanged for ${quiet_for}s: ${last:-<none>}"

# Whether the attribution row can say anything, decided BEFORE the window and from the preconditions
# rather than from the reading. Exhausting the loop without ever reaching 9 quiet seconds used to
# fall through here silently: the sample was taken mid-churn, the row would SKIP or - worse - pass on
# two samples that merely happened to match, and nothing said the precondition itself had failed.
attributable=true
attribution_note=""
if [ "$replicas_seen" != true ]; then
  attributable=false
  attribution_note="no replica Pod was ever observed, so an unchanged sample would be a reading about an absence rather than about a watch"
elif [ "$quiet_for" -lt 9 ]; then
  attributable=false
  attribution_note="the replicas never went 9s without a write (${quiet_for}s at best), so the controller was being woken throughout the window"
fi

before="$(watched_versions)"

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

after="$(watched_versions)"

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

# The attribution, and it claims only what the sample supports: no OTHER object this controller
# watches moved, which leaves the Service watch. Every failure mode is a SKIP rather than a FAIL --
# a watched object being written in the window is not a defect in anything, it just means this run
# cannot say which watch did the work, and the uid row above stands on its own either way.
# A SENTINEL IN EITHER SAMPLE DISQUALIFIES THE COMPARISON, and this guard is the point of the
# sentinel existing. `sample` emits NOT-FOUND or READ-FAILED for a kind it could not observe, so that
# such a kind cannot drop silently out of the sample -- but a kind that is unobservable for the whole
# window emits the SAME sentinel on both sides, and `before = after` is then true. The row would
# record PASS while naming a kind it never saw, which is the exact false pass the sentinel was added
# to prevent, reintroduced one layer up. An equality test is always true when both sides are the
# marker for "no data", so the marker has to be excluded before the test rather than compared by it.
if printf '%s\n%s\n' "$before" "$after" | command grep -Eq '=(NOT-FOUND|READ-FAILED)( |$)'; then
  record SKIP "no other object this controller watches changed in the window" \
    "at least one watched kind could not be observed across the window, so an unchanged sample would not mean it did not move: before=[${before}] after=[${after}]"
elif [ "$attributable" != true ]; then
  record SKIP "no other object this controller watches changed in the window" \
    "the precondition failed, so the row above passes unattributed: ${attribution_note}"
elif [ "$before" = "$after" ]; then
  record PASS "no other object this controller watches changed in the window" \
    "identical either side of the delete, so no Pod, InstanceType, Binding, pool or backend event could have driven the reconcile: ${after}"
else
  record SKIP "no other object this controller watches changed in the window" \
    "something moved, so the row above passes unattributed: before=[${before}] after=[${after}]"
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
