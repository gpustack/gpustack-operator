#!/usr/bin/env bash
#
# CASE 75 — An image bump on a replicated leader emits the KVCacheLeaderHandover event, and its
#            cumulative count equals the lease's transitions   (MUTATING, self-recovering)
#
#   case-75.sh <NS>
#
# Goal:        A rollout that replaces the leader must be observable where an operator or a human
#              can see it, not only in the lease's own numbers. This case proves the event channel:
#              a steady 3-replica HA backend whose spec.image is patched -- and nothing else is
#              touched, no pod is deleted by hand -- must emit a KVCacheLeaderHandover event for
#              the backend, the event's cumulative count ("N handovers in total") must equal the
#              lease's leaseTransitions after the rollout, every leader pod must be a replacement
#              on the new image with exactly one of them ready, and the backend must settle back
#              to Ready with RolloutComplete=True/Complete.
#
# Environment: Any cluster; no GPU, no RDMA (members are DRAM over TCP). <NS> must be the
#              operator's system namespace. THE IMAGE PAIR IS A DIRECTIONAL CONTRACT: the start and
#              the rollout target must BOTH be at least 0.3.12.post1 (older masters do not parse
#              this operator's rendered argv and exit at gflags parse) and BOTH must carry the
#              Kubernetes lease backend, and both must be CPU-capable -- a CUDA-only Mooncake tag
#              crashes members on CPU-only nodes with "Failed to start store service". Defaults pin
#              such a pair; override only with another pair that satisfies the same three clauses,
#              through E2E_MOONCAKE_IMAGE (the start image) and E2E_MOONCAKE_ROLLOUT_IMAGE (the
#              rollout target). The trap this pair exists to avoid encoding: querying the event in
#              the operator's own namespace yields nothing, because events for the cluster-scoped
#              backend land in namespace default -- every event read below is against default.
#
# Inputs:      All real, nothing mocked. One KVCacheBackend (3-replica HA leader, multi-tenancy on,
#              no snapshot) created on the start image; after it is Ready and steady the ONLY
#              mutation is `kubectl patch` of spec.image to the rollout target. A clean baseline is
#              asserted first: zero KVCacheLeaderHandover events for this backend anywhere. The
#              pod-deletion trigger of the failover cases is deliberately NOT used here, so the
#              event observed can only be the rollout's.
#
# Expected:    - zero KVCacheLeaderHandover events for the backend at steady state (clean baseline);
#              - after the patch, the event exists, read from namespace default;
#              - the event's cumulative count equals the lease's leaseTransitions after the rollout
#                (on a fresh backend the baseline is 0, so the count also equals the delta);
#              - every leader pod runs the target image and is not one of the pods present before
#                the patch (all three are rollout replacements), with exactly one of them ready;
#              - the backend settles back to Ready with RolloutComplete=True/Complete and every
#                health condition True -- PoolWrites, which reports write activity rather than
#                health, reads Unknown/NoWritesObserved on this idle backend and is accepted so.
#
# Cleanup:     Trap deletes the KVCacheBackend; owner references cascade to the Deployment,
#              Service, Lease, and accounts. Nothing else is touched. Idempotent, runs on pass AND
#              fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-75.sh <NS>}"
CASE_ID=75

# A DIRECTIONAL PAIR, not one image: the case rolls from the start tag to the target tag, and both
# ends of that roll must parse this operator's argv, campaign through a lease, and run on CPU-only
# nodes. See Environment for the three clauses a replacement pair must satisfy.
IMAGE_START="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.12.post1-cpu}"
IMAGE_ROLL_TO="${E2E_MOONCAKE_ROLLOUT_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

# The suffix keeps two runs of this case apart: the backend's name is cluster-scoped, and every
# object it renders -- the Lease included -- derives from it.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-ro-${SFX}"
LEADER="${BACKEND}-leader"

LEADER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

results() {
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED"; return 1; }
  echo "[case-${CASE_ID}] all checks passed"
  return 0
}

teardown() {
  echo
  echo "[case-75] cleanup"
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap teardown EXIT

# wait_for polls one jsonpath until it equals what is wanted, and prints the LAST value seen, so a
# timeout says what the object was doing rather than only that it timed out.
wait_for() {
  local kind="$1" name="$2" path="$3" want="$4" secs="${5:-180}"
  local got="" i
  for ((i = 0; i < secs; i += 3)); do
    got="$(kubectl -n "$NS" get "$kind" "$name" -o "jsonpath=$path" 2>/dev/null)"
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 3
  done
  echo "$got"
  return 1
}

lease_transitions() {
  local t
  t="$(kubectl -n "$NS" get leases.coordination.k8s.io "$LEADER" -o jsonpath='{.spec.leaseTransitions}' 2>/dev/null)"
  echo "${t:-0}"
}

# The event for the cluster-scoped backend lands in namespace default, NOT in the operator's own
# namespace -- querying $NS there yields nothing and reads like the feature is dead. The
# involvedObject.name selector keeps other backends' handovers out of this case's reading.
handover_events() {
  kubectl -n default get events --field-selector "reason=KVCacheLeaderHandover,involvedObject.name=${BACKEND}" \
    --sort-by=.lastTimestamp \
    -o jsonpath='{range .items[*]}{.lastTimestamp}{"|"}{.message}{"\n"}{end}' 2>/dev/null
}

# ------------------------------------------------------------- setup: steady on the start image

kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND}
spec:
  type: Mooncake
  image: ${IMAGE_START}
  transport:
    protocol: TCP
  connection:
    managed:
      leader:
        replicas: 3
        multiTenancy: true
        highAvailability: {}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "the backend reaches Ready on the start image" \
    "phase never became Ready in 300s; nothing below can run. If no leader Pod is ever ready, the start image likely fails this operator's argv contract (see the pair clauses in the header): $(kubectl -n "$NS" get pod -l "$LEADER_SEL" -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{" "}{end}' 2>/dev/null)"
  results; exit 1
fi
if ! wait_for deployment "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
  record FAIL "exactly one leader replica is Ready before the rollout" \
    "readyReplicas settled at '$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' instead of 1"
  results; exit 1
fi

# Steady means the election has stopped moving: give a settled lease one more read after a short
# quiet window, so a provisioning-time flap is counted into the baseline instead of misread as the
# rollout's own handover.
sleep 20
TRANS_BEFORE="$(lease_transitions)"

BASELINE_EVENTS="$(handover_events)"
if [ -z "$BASELINE_EVENTS" ]; then
  record PASS "steady state emits zero handover events (clean baseline)" \
    "lease transitions=${TRANS_BEFORE}; no KVCacheLeaderHandover event for ${BACKEND} in namespace default"
else
  record FAIL "steady state emits zero handover events (clean baseline)" \
    "pre-existing events would make the rollout's own event uncountable: $(echo "$BASELINE_EVENTS" | tr '\n' ' ' | cut -c1-200)"
  results; exit 1
fi

# ------------------------------------------------------------- the rollout: a spec patch and nothing else

# THE ONLY MUTATION. A hand-deleted pod is the OTHER trigger family (the failover cases own it);
# using it here would make every observation below attributable to two causes at once.
# The leader pods BEFORE the patch, by UID, which is what "a replacement" is measured against below.
# Start times are second-granular and a replacement can start in the very second of the patch, so a
# time comparison cannot tell it from a leftover; a UID can.
PRE_UIDS="$(kubectl -n "$NS" get pod -l "$LEADER_SEL" -o jsonpath='{range .items[*]}{.metadata.uid}{" "}{end}' 2>/dev/null)"
T0="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
if ! kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type merge \
  -p "{\"spec\":{\"image\":\"${IMAGE_ROLL_TO}\"}}" >/dev/null 2>&1; then
  record FAIL "the image patch is accepted" "kubectl patch of spec.image failed"
  results; exit 1
fi

TRAJ_FILE="$(mktemp)"
EVENT_MSG=""
EVENT_AT=""
for ((i = 0; i < 300; i += 5)); do
  {
    kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
      -o jsonpath='{.status.phase}|{.status.conditions[?(@.type=="RolloutComplete")].status}/{.status.conditions[?(@.type=="RolloutComplete")].reason}{"\n"}' 2>/dev/null
  } >>"$TRAJ_FILE"
  LATEST="$(handover_events | awk -F'|' 'NF {msg=$0} END {print msg}')"
  if [ -n "$LATEST" ]; then
    EVENT_AT="${LATEST%%|*}"
    EVENT_MSG="${LATEST#*|}"
  fi
  UPD="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null)"
  RDY="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
  [ -n "$EVENT_MSG" ] && [ "$UPD" = "3" ] && [ "$RDY" = "1" ] && break
  sleep 5
done
TRAJECTORY="$(awk -F'|' '!seen[$0]++ {printf "%s%s", (NR>1 ? " -> " : ""), $0}' "$TRAJ_FILE")"
rm -f "$TRAJ_FILE"

# ------------------------------------------------------------- the verdicts

if [ -n "$EVENT_MSG" ]; then
  record PASS "the rollout emits the KVCacheLeaderHandover event" \
    "read in namespace default (cluster-scoped backend), at ${EVENT_AT}: ${EVENT_MSG}"
else
  record FAIL "the rollout emits the KVCacheLeaderHandover event" \
    "no event for ${BACKEND} in namespace default within 300s of the patch; querying the operator namespace is the known trap and yields nothing by design"
fi

TRANS_AFTER="$(lease_transitions)"
COUNT_IN_MSG="$(printf '%s' "$EVENT_MSG" | sed -n 's/.*recorded \([0-9][0-9]*\) handovers in total.*/\1/p')"
if [ -n "$COUNT_IN_MSG" ] && [ "$COUNT_IN_MSG" = "$TRANS_AFTER" ]; then
  if [ "$TRANS_BEFORE" = "0" ]; then
    record PASS "the event's cumulative count equals the lease's transitions" \
      "message says ${COUNT_IN_MSG}, leaseTransitions=${TRANS_AFTER} (baseline 0, so the count is also this rollout's delta)"
  else
    record PASS "the event's cumulative count equals the lease's transitions" \
      "message says ${COUNT_IN_MSG}, leaseTransitions=${TRANS_AFTER}; baseline before the patch was ${TRANS_BEFORE} (a provisioning-time flap, counted before the roll)"
  fi
else
  record FAIL "the event's cumulative count equals the lease's transitions" \
    "message count '${COUNT_IN_MSG:-<none parsed>}' vs leaseTransitions='${TRANS_AFTER}' (baseline ${TRANS_BEFORE})"
fi

# Every pod must be a rollout replacement: on the target image AND not one of the pods that existed
# before the patch. A pre-patch set that could not be read makes "none of them survived" true of
# nothing, so it fails the row rather than passing it.
POD_LINES="$(kubectl -n "$NS" get pod -l "$LEADER_SEL" \
  -o jsonpath='{range .items[*]}{.metadata.uid}{"|"}{.spec.containers[0].image}{"|"}{.status.startTime}{"\n"}{end}' 2>/dev/null)"
NEW_COUNT=0
while IFS='|' read -r uid img _; do
  [ -z "$uid" ] && continue
  case " $PRE_UIDS " in *" $uid "*) continue ;; esac
  [ "$img" = "$IMAGE_ROLL_TO" ] && NEW_COUNT=$((NEW_COUNT + 1))
done <<EOF2
$POD_LINES
EOF2
PRE_COUNT="$(echo "$PRE_UIDS" | wc -w | tr -d ' ')"
if [ "$PRE_COUNT" = "0" ]; then
  record FAIL "every leader pod is a replacement on the new image" \
    "the leader pods before the patch could not be read, so no pod can be shown to be a replacement"
elif [ "$NEW_COUNT" = "3" ]; then
  record PASS "every leader pod is a replacement on the new image" \
    "3 of 3 pods on ${IMAGE_ROLL_TO}, none of them among the ${PRE_COUNT} pods present before the patch (${T0}); no pod was deleted by hand"
else
  record FAIL "every leader pod is a replacement on the new image" \
    "${NEW_COUNT} of 3 pods are post-patch replacements on ${IMAGE_ROLL_TO}; pods (uid image startTime): $(echo "$POD_LINES" | tr '|' ' ' | tr '\n' ' ')"
fi

RDY_FINAL="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
if [ "$RDY_FINAL" = "1" ]; then
  record PASS "exactly one leader replica is Ready after the rollout" \
    "readyReplicas=1 with 3 desired -- the election gate survived the roll"
else
  record FAIL "exactly one leader replica is Ready after the rollout" \
    "readyReplicas='${RDY_FINAL}' instead of 1"
fi

# Convergence, not a snapshot: members remount one at a time behind a new leader, so the settle is
# polled through the bound. The phase may legitimately dip (Provisioning/Degraded) while the member
# DaemonSet rolls onto the new image -- that churn is case-76's subject; here it is recorded as
# trajectory, and what must hold is the settle itself.
if wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record PASS "the backend settles back to Ready" "phase=Ready; rollout trajectory: ${TRAJECTORY}"
else
  record FAIL "the backend settles back to Ready" \
    "phase='$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)' after a 300s settle window: $(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
fi

ROLL_STATUS="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
  -o jsonpath='{.status.conditions[?(@.type=="RolloutComplete")].status}/{.status.conditions[?(@.type=="RolloutComplete")].reason}' 2>/dev/null)"
# PoolWrites is the one condition that is not a health verdict: it reports write activity since the
# current leader process started, and a backend nothing writes to reads Unknown/NoWritesObserved. That
# exact reading is the expected one here, because this case writes nothing; any other non-True
# PoolWrites -- a revoked write, counters the leader could not report -- is still a finding.
NOT_TRUE="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{" "}{end}' 2>/dev/null \
  | tr ' ' '\n' | /usr/bin/grep -v -e '=True/' -e '^PoolWrites=Unknown/NoWritesObserved$' -e '^$' | tr '\n' ' ')"
if [ "$ROLL_STATUS" = "True/Complete" ] && [ -z "$NOT_TRUE" ]; then
  record PASS "RolloutComplete=True/Complete and every health condition True" \
    "conditions: $(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{" "}{end}' 2>/dev/null)"
else
  record FAIL "RolloutComplete=True/Complete and every health condition True" \
    "RolloutComplete='${ROLL_STATUS}'; conditions not True (PoolWrites=Unknown/NoWritesObserved excepted): ${NOT_TRUE:-<none>}"
fi

results
