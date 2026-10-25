#!/usr/bin/env bash
#
# CASE 75 — The store's snapshot switches are refused at admission, and a leader's image rollout and
#            its rollback stay observable   (MUTATING, self-recovering)
#
#   case-75.sh <NS>
#
# Goal:        Three contracts on one HA KVCacheBackend. First, the store's snapshot is not offered:
#              restoring one can make the cache serve another key's bytes instead of a miss, so the
#              webhook must refuse the snapshot switches in the leader's extraArgs -- on create and
#              on an update to a running backend. Second, a rollout that replaces the leader must be
#              observable where an operator or a human can see it: the KVCacheLeaderHandover event
#              must exist, and its cumulative count must equal the lease's transitions. Third, an
#              update must be judgeable from the backend's own condition, because the standard
#              instrument is structurally blind here: the readiness gate withholds all but the lease
#              holder, so Deployment completion never arrives and `kubectl rollout status` times out
#              on EVERY such rollout. This case never calls that instrument; it proves the
#              operator's predicate stays truthful throughout, on the way forward and on the way
#              back, and that the member-churn dip an image bump causes clears inside its budget.
#
# Environment: Any cluster; no GPU, no RDMA (members are DRAM over TCP). <NS> must be the
#              operator's system namespace. THE IMAGE PAIR IS A DIRECTIONAL CONTRACT: the start and
#              the rollout target must BOTH be at least 0.3.12.post1 (older masters do not parse
#              this operator's rendered argv and exit at gflags parse) and BOTH must carry the
#              Kubernetes lease backend, and both must be CPU-capable -- a CUDA-only Mooncake tag
#              crashes members on CPU-only nodes with "Failed to start store service". Defaults pin
#              such a pair; override only with another pair that satisfies the same three clauses,
#              through E2E_MOONCAKE_IMAGE (the start image) and E2E_MOONCAKE_ROLLOUT_IMAGE (the
#              rollout target; the rollback returns to the start image). The trap this pair exists
#              to avoid encoding: querying the event in the operator's own namespace yields nothing,
#              because events for the cluster-scoped backend land in namespace default -- every
#              event read below is against default.
#
# Inputs:      All real, nothing mocked. One KVCacheBackend starts with the default single-replica
#              Kubernetes election, then scales to three without replacing the first leader Pod.
#              The admission checks are sent as server-side dry runs, which pass through admission
#              and persist nothing: three dry-run creates and two dry-run patches. After the backend
#              is Ready and steady, the only mutations are two image patches -- the forward bump to
#              the rollout target and the bump back to the start image. No pod is deleted by hand
#              at any point, so the handover event observed can only be a rollout's.
#
# Expected:    - the manifest without snapshot flags is accepted (the positive baseline: without it a
#              refusal of every backend would pass);
#              - `-enable_snapshot=true` in leader.extraArgs is refused on that entry with the
#              wrong-data reason, and `-snapshot_interval_seconds=60` with the reason that it is
#              read only under a refused switch;
#              - on the running backend, an update changing only the image is accepted, and an
#              update adding `-enable_snapshot_restore=true` is refused with the wrong-data reason;
#              - the default single leader holds a Lease; None with three replicas is refused;
#              - scaling to three preserves the original leader Pod UID, and exactly one of the
#              three leader replicas is Ready (the election gate);
#              - zero KVCacheLeaderHandover events for the backend at steady state (clean baseline);
#              - after the forward patch, the event exists, read from namespace default;
#              - the event's cumulative count equals the lease's leaseTransitions after the rollout
#                (on a fresh backend the baseline is 0, so the count also equals the delta);
#              - every leader pod runs the target image and is not one of the pods present before
#                the patch (all three are rollout replacements), with exactly one of them ready;
#              - the backend settles back to Ready with RolloutComplete=True/Complete and every
#                health condition True -- PoolWrites, which reports write activity rather than
#                health, reads Unknown/NoWritesObserved on this idle backend and is accepted so;
#              - on the way back, every sampled RolloutComplete is a benign state
#                (Unknown/UpdateNotObserved, False/Progressing, True/Complete) and never a
#                deadline-ish stall reason; the update converges with updated=3 and exactly one
#                ready; the phase dip the member re-registration causes clears within its window;
#                and the backend settles Ready with every health condition True.
#
# Cleanup:     Trap deletes the KVCacheBackend, whose owner references cascade to the Deployment,
#              Service and accounts, then deletes the leader Lease by name: the Lease carries no owner
#              reference. Nothing else is touched. Idempotent, runs on pass AND fail, safe to re-run.
#
# NOTE FOR ANYONE WIRING THIS INTO CI/CD: do not gate on `kubectl rollout status` (or an
# Argo/Flux-style wait on Deployment completion) for a multi-replica leader -- it times out on
# every rollout by construction. Gate on the backend's RolloutComplete condition, as this case
# does. The timeout shape is a documented limitation, not an expectation to encode; if the suite
# grows an XFAIL marker, that instrument is the candidate.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-75.sh <NS>}"
export CASE_ID=75

# The suffix keeps two runs of this case apart: the backend's name is cluster-scoped, and every
# object it renders -- the Lease included -- derives from it.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-ro-${SFX}"
LEADER="${BACKEND}-leader"

LEADER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader"

# Every refusal is matched on its field AND its reason: extraArgs carries several refusals for other
# keys, so the path alone would pass on any of them.
SWITCH_RE='spec\.connection\.managed\.leader\.extraArgs\[0\]: Forbidden: snapshots are not supported: .*can serve another key.s bytes instead of a miss'
COMPANION_RE='spec\.connection\.managed\.leader\.extraArgs\[0\]: Forbidden: it is read only when enable_snapshot or enable_snapshot_restore is set'

# The measured dips: ~34-50s on a freshly rolled backend, ~2 min on a long-lived one. The bound
# takes the slow reading with margin, so a FAIL here means the dip genuinely outgrew its budget
# rather than a fast machine being slow.
DIP_BOUND_SECS=300

# The Lease readers, the rollout image pair and the teardown live in the shared HA library.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-ha-lib.sh"

# A DIRECTIONAL PAIR, not one image: the case rolls from the start tag to the target tag, and both
# ends of that roll must parse this operator's argv, campaign through a lease, and run on CPU-only
# nodes. See Environment for the three clauses a replacement pair must satisfy.
read -r IMAGE_START IMAGE_ROLL_TO <<<"$(kvha_resolve_rollout_images \
  "gpustack/mirrored-mooncake:0.3.12.post1-cpu" "gpustack/mirrored-mooncake:0.3.13.post1-cpu")"

teardown() {
  echo
  echo "[case-75] cleanup"
  kvha_teardown "$NS" "$BACKEND" "$LEADER"
}
trap teardown EXIT

# The event for the cluster-scoped backend lands in namespace default, NOT in the operator's own
# namespace -- querying $NS there yields nothing and reads like the feature is dead. The
# involvedObject.name selector keeps other backends' handovers out of this case's reading.
handover_events() {
  kubectl -n default get events --field-selector "reason=KVCacheLeaderHandover,involvedObject.name=${BACKEND}" \
    --sort-by=.lastTimestamp \
    -o jsonpath='{range .items[*]}{.lastTimestamp}{"|"}{.message}{"\n"}{end}' 2>/dev/null
}

# manifest <leader-extra-yaml> prints the backend this case creates. The argument lands under
# `leader:` to add an extraArgs list. The same manifest is the dry-run create the admission checks
# send and the live object the rollout acts on, so a refusal can only be a refusal of this shape.
manifest() {
  cat <<YAML
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
        replicas: 1
        multiTenancy: true
${1:-}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML
}

# expect_refused <label> <regex> <leader-extra-yaml> sends a server-side dry-run create and records
# whether it was refused for the reason the regex names, rather than for any other.
expect_refused() {
  local out
  if out="$(manifest "$3" | kubectl create --dry-run=server -f - 2>&1)"; then
    record FAIL "$1" "accepted: ${out}"
  elif [[ "$out" =~ $2 ]]; then
    record PASS "$1" "${BACKEND}"
  else
    record FAIL "$1" "refused by another rule: ${out}"
  fi
}

# The states RolloutComplete may legitimately hold DURING an update. Unknown/UpdateNotObserved is
# the pre-observation window; False/Progressing is an observed in-flight update; True/Complete is
# the settled truth. Everything else -- and any deadline-ish reason at any status -- is a
# misreport, and the row that samples for them FAILs.
allowed_rollout_state() {
  case "$1" in
    "True/Complete"|"False/Progressing"|"Unknown/UpdateNotObserved") return 0 ;;
    *) return 1 ;;
  esac
}

deadline_ish() {
  case "$1" in
    *[Dd]eadline*|*[Tt]imed[Oo]ut*|*[Tt]imeout*) return 0 ;;
    *) return 1 ;;
  esac
}

# ------------------------------------------------------------- admission: the snapshot is not offered

# A restore can serve another key's bytes instead of a miss, so the snapshot switches are refused.
# These three are dry-run creates: they pass through admission and persist nothing, which is why
# they run BEFORE the live backend exists.
if out="$(manifest | kubectl create --dry-run=server -f - 2>&1)"; then
  record PASS "create without snapshot flags is accepted" "${BACKEND}"
else
  record FAIL "create without snapshot flags is accepted" "${out}"
fi

expect_refused "-enable_snapshot in extraArgs is refused for serving wrong data" "$SWITCH_RE" \
  '        extraArgs: ["-enable_snapshot=true"]'
expect_refused "-snapshot_interval_seconds in extraArgs is refused as read only under a refused switch" "$COMPANION_RE" \
  '        extraArgs: ["-snapshot_interval_seconds=60"]'

# ------------------------------------------------------------- setup: one backend, on the start image

if ! out="$(manifest | kubectl apply -f - 2>&1)"; then
  record FAIL "the backend to update is created" "${out}"
  results; exit 1
fi

# The same two refusals against the LIVE object: an update to a running backend is refused for the
# same reason, and an update that changes only the image is still accepted. Both are server-side
# dry runs, so neither persists and neither touches the rollout below.
if out="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --dry-run=server --type=merge \
  -p "{\"spec\":{\"image\":\"${IMAGE_START%:*}:case-75\"}}" 2>&1)"; then
  record PASS "an update changing only the image is accepted" "${BACKEND}"
else
  record FAIL "an update changing only the image is accepted" "${out}"
fi

if out="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --dry-run=server --type=merge \
  -p '{"spec":{"connection":{"managed":{"leader":{"extraArgs":["-enable_snapshot_restore=true"]}}}}}' 2>&1)"; then
  record FAIL "an update adding -enable_snapshot_restore is refused" "accepted: ${out}"
elif [[ "$out" =~ $SWITCH_RE ]]; then
  record PASS "an update adding -enable_snapshot_restore is refused" "${BACKEND}"
else
  record FAIL "an update adding -enable_snapshot_restore is refused" "refused by another rule: ${out}"
fi

if ! kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "the backend reaches Ready on the start image" \
    "phase never became Ready in 300s; nothing below can run. If no leader Pod is ever ready, the start image likely fails this operator's argv contract (see the pair clauses in the header): $(kubectl -n "$NS" get pod -l "$LEADER_SEL" -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{" "}{end}' 2>/dev/null)"
  results; exit 1
fi
if ! kvha_wait_for "$NS" deployment "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
  record FAIL "exactly one leader replica is Ready before the rollout" \
    "readyReplicas settled at '$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' instead of 1"
  results; exit 1
fi

FIRST_UID="$(kubectl -n "$NS" get pod -l "$LEADER_SEL" -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null)"
HOLDER="$(kvha_lease_holder "$NS" "$LEADER")"
ARGS="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null)"
if [ -n "$FIRST_UID" ] && [ -n "$HOLDER" ] && [[ "$ARGS" == *-enable_ha=true* ]]; then
  record PASS "the default single leader already elects" "leader Pod UID=${FIRST_UID}; Lease holder=${HOLDER}"
else
  record FAIL "the default single leader already elects" "UID=${FIRST_UID:-<none>}; holder=${HOLDER:-<none>}; args=${ARGS:-<none>}"
  results; exit 1
fi

if REFUSAL="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --dry-run=server --type merge \
  -p '{"spec":{"connection":{"managed":{"leader":{"replicas":3,"electionBackend":"None"}}}}}' 2>&1)"; then
  record FAIL "None with three replicas is refused" "server accepted the invalid patch"
  results; exit 1
elif [[ "$REFUSAL" == *"leader.electionBackend"* ]]; then
  record PASS "None with three replicas is refused" "$REFUSAL"
else
  record FAIL "None with three replicas is refused" "$REFUSAL"
  results; exit 1
fi

if ! kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type merge \
  -p '{"spec":{"connection":{"managed":{"leader":{"replicas":3}}}}}' >/dev/null 2>&1; then
  record FAIL "scaling to three is accepted" "replica patch failed"
  results; exit 1
fi
if ! kvha_wait_for "$NS" deployment "$LEADER" '{.status.replicas}' 3 300 >/dev/null; then
  record FAIL "scaling to three converges" "the Deployment never reached three replicas"
  results; exit 1
fi
if ! kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "scaling to three converges" "the backend did not return to Ready"
  results; exit 1
fi
SCALE_UIDS="$(kubectl -n "$NS" get pod -l "$LEADER_SEL" -o jsonpath='{range .items[*]}{.metadata.uid}{" "}{end}' 2>/dev/null)"
if [ -n "$FIRST_UID" ] && [[ " $SCALE_UIDS " == *" $FIRST_UID "* ]]; then
  record PASS "scaling to three preserves the first leader Pod" "first UID=${FIRST_UID}; current UIDs=${SCALE_UIDS}"
else
  record FAIL "scaling to three preserves the first leader Pod" "first UID=${FIRST_UID}; current UIDs=${SCALE_UIDS}"
  results; exit 1
fi

# Steady means the election has stopped moving: give a settled lease one more read after a short
# quiet window, so a provisioning-time flap is counted into the baseline instead of misread as the
# rollout's own handover.
sleep 20
TRANS_BEFORE="$(kvha_lease_transitions "$NS" "$LEADER")"

BASELINE_EVENTS="$(handover_events)"
if [ -z "$BASELINE_EVENTS" ]; then
  record PASS "steady state emits zero handover events (clean baseline)" \
    "lease transitions=${TRANS_BEFORE}; no KVCacheLeaderHandover event for ${BACKEND} in namespace default"
else
  record FAIL "steady state emits zero handover events (clean baseline)" \
    "pre-existing events would make the rollout's own event uncountable: $(echo "$BASELINE_EVENTS" | tr '\n' ' ' | cut -c1-200)"
  results; exit 1
fi

# ------------------------------------------------------------- the forward rollout: a spec patch and nothing else

# The election gate at three replicas: only the lease holder serves, so exactly one of the three
# is Ready. This is the shape the update below has to preserve, so it is checked last, right
# before the mutation that has to preserve it.
if ! kvha_wait_for "$NS" deployment "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
  record FAIL "exactly one leader replica is Ready before the updates" \
    "readyReplicas settled at '$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' instead of 1"
  results; exit 1
fi

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

TRANS_AFTER="$(kvha_lease_transitions "$NS" "$LEADER")"
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
# DaemonSet rolls onto the new image -- that churn is the rollback leg's subject; here it is
# recorded as trajectory, and what must hold is the settle itself.
if kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
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

# ------------------------------------------------------------- the rollback: the same update, measured mid-flight

# The measured leg is a SECOND update, so the backend must first live through one: the bump above
# brought it to the settled-on-the-target state this leg starts from. The pass signal here is the
# backend's own condition -- never `kubectl rollout status`, which cannot return for a
# multi-replica leader (see the header note).
FIRST_SETTLED=0
for ((i = 0; i < 300; i += 5)); do
  PH="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)"
  UPD="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null)"
  RDY="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
  ROL="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
    -o jsonpath='{.status.conditions[?(@.type=="RolloutComplete")].status}/{.status.conditions[?(@.type=="RolloutComplete")].reason}' 2>/dev/null)"
  if [ "$PH" = "Ready" ] && [ "$UPD" = "3" ] && [ "$RDY" = "1" ] && [ "$ROL" = "True/Complete" ]; then
    FIRST_SETTLED=1
    break
  fi
  sleep 5
done
if [ "$FIRST_SETTLED" = "1" ]; then
  record PASS "the first update settles on the target image" \
    "phase=Ready updated=3 ready=1 RolloutComplete=True/Complete on ${IMAGE_ROLL_TO}"
else
  record FAIL "the first update settles on the target image" \
    "after 300s: phase='$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)' updated='${UPD}' ready='${RDY}' rollout='${ROL}'"
  results; exit 1
fi

sleep 20

# T1 anchors the dip bound. From here the ONLY mutation is the patch back to the start image; the
# sampler below is read-only against the backend, the deployment and nothing else.
T1_EPOCH="$(date +%s)"
# The leader Deployment's generation BEFORE the patch. The sampler below refuses to conclude until
# the operator has actually rewritten that Deployment, because the window this case measures does
# not exist until it has: sampled immediately after the patch, every reading is still the PREVIOUS
# settled state -- Ready, updated=3, ready=1, True/Complete -- which is exactly the loop's own exit
# condition. Without this gate the loop can exit on its first iteration having measured the state
# the patch was supposed to disturb, and the row would pass on an empty window.
LEADER_GEN_BEFORE="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.metadata.generation}' 2>/dev/null)"

kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type merge \
  -p "{\"spec\":{\"image\":\"${IMAGE_START}\"}}" >/dev/null 2>&1

TRAJ_FILE="$(mktemp)"
BAD_STATES=""
ROLLOUT_SEEN=0
DIP_STARTED=0
DIP_START_AT=""
DIP_CLEARED_AT=""
for ((i = 0; i < DIP_BOUND_SECS; i += 5)); do
  NOW="$(date +%s)"
  PH="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)"
  ROL="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
    -o jsonpath='{.status.conditions[?(@.type=="RolloutComplete")].status}/{.status.conditions[?(@.type=="RolloutComplete")].reason}' 2>/dev/null)"
  UPD="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null)"
  RDY="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
  echo "t+$((NOW - T1_EPOCH))s phase=${PH} rollout=${ROL} updated=${UPD} ready=${RDY}" >>"$TRAJ_FILE"

  # The predicate's truthfulness, judged on EVERY sample, not only the settled one. An unreadable
  # sample is skipped (a transport blip is not the operator's verdict), an empty rollout pair is
  # the backend object mid-write and is skipped too; anything readable but outside the allowed set
  # or carrying a deadline-ish reason is a misreport and fails the row.
  if [ -n "$ROL" ] && [ "$ROL" != "/" ]; then
    if ! allowed_rollout_state "$ROL"; then
      BAD_STATES="${BAD_STATES} ${ROL}"
    elif deadline_ish "$ROL"; then
      BAD_STATES="${BAD_STATES} ${ROL}(deadline-ish)"
    fi
  fi

  if [ "$DIP_STARTED" = "0" ] && [ -n "$PH" ] && [ "$PH" != "Ready" ]; then
    DIP_STARTED=1
    DIP_START_AT="$((NOW - T1_EPOCH))s"
  fi
  if [ "$DIP_STARTED" = "1" ] && [ -z "$DIP_CLEARED_AT" ] && [ "$PH" = "Ready" ]; then
    DIP_CLEARED_AT="$((NOW - T1_EPOCH))s"
  fi

  # The window opens only once the operator has rewritten the leader Deployment. Until then a
  # settled-looking sample is the state from BEFORE the patch, and concluding on it would report a
  # measurement that never happened.
  LEADER_GEN_NOW="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.metadata.generation}' 2>/dev/null)"
  if [ -n "$LEADER_GEN_NOW" ] && [ -n "$LEADER_GEN_BEFORE" ] && [ "$LEADER_GEN_NOW" -gt "$LEADER_GEN_BEFORE" ] 2>/dev/null; then
    ROLLOUT_SEEN=1
  fi

  if [ "$ROLLOUT_SEEN" = "1" ] && [ "$PH" = "Ready" ] && [ "$UPD" = "3" ] && [ "$RDY" = "1" ] && [ "$ROL" = "True/Complete" ]; then
    break
  fi
  sleep 5
done
TRAJECTORY="$(awk '!seen[$0]++ {printf "%s%s", (NR>1 ? " -> " : ""), $0}' "$TRAJ_FILE")"
OBSERVED_ROLLOUT_STATES="$(awk '{sub(/.*rollout=/, ""); sub(/ updated=.*/, ""); print}' "$TRAJ_FILE" | awk '!seen[$0]++' | tr '\n' ' ')"
rm -f "$TRAJ_FILE"
# Three outcomes, not two. "No bad sample" is vacuously true over an empty window, and an empty
# window is reachable two ways -- the operator never acted on the patch, or nothing readable came
# back -- so each is reported as its own failure rather than passing by absence.
READABLE_STATES="$(printf '%s' "$OBSERVED_ROLLOUT_STATES" | tr -d ' /')"
if [ "$ROLLOUT_SEEN" != "1" ]; then
  record FAIL "RolloutComplete never misreports a stall mid-update" \
    "the leader Deployment's generation never moved past ${LEADER_GEN_BEFORE} within ${DIP_BOUND_SECS}s, so the patch opened no window to sample; trajectory: ${TRAJECTORY}"
elif [ -z "$READABLE_STATES" ]; then
  record FAIL "RolloutComplete never misreports a stall mid-update" \
    "the window opened but no RolloutComplete sample was readable, so the row has nothing to judge; trajectory: ${TRAJECTORY}"
elif [ -z "$BAD_STATES" ]; then
  record PASS "RolloutComplete never misreports a stall mid-update" \
    "sampled states: ${OBSERVED_ROLLOUT_STATES}; trajectory: ${TRAJECTORY}"
else
  record FAIL "RolloutComplete never misreports a stall mid-update" \
    "illegal or deadline-ish samples:${BAD_STATES}; trajectory: ${TRAJECTORY}"
fi

UPD_FINAL="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null)"
RDY_FINAL="$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
if [ "$UPD_FINAL" = "3" ] && [ "$RDY_FINAL" = "1" ]; then
  record PASS "the update converges with the election gate intact" \
    "updated=3 ready=1 (only the lease holder serves; that shape is the gate, not a fault)"
else
  record FAIL "the update converges with the election gate intact" \
    "updated='${UPD_FINAL}' ready='${RDY_FINAL}'"
fi

if [ "$DIP_STARTED" = "0" ]; then
  record PASS "the member-churn dip clears within its window" \
    "no dip observed this run (every sample Ready); the bound ${DIP_BOUND_SECS}s holds trivially"
elif [ -n "$DIP_CLEARED_AT" ]; then
  record PASS "the member-churn dip clears within its window" \
    "dip started ~${DIP_START_AT}, phase Ready again at ${DIP_CLEARED_AT} after the patch, within ${DIP_BOUND_SECS}s"
else
  record FAIL "the member-churn dip clears within its window" \
    "phase never returned to Ready within ${DIP_BOUND_SECS}s of the patch; trajectory: ${TRAJECTORY}"
fi

# The settle of every condition is a separate question from the phase: give the member
# re-registration its own bounded wait (gating only -- the verdict is the row below, so the check
# is judged once, on fresh reads), so a stuck-but-Ready lie cannot pass on the phase alone.
kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.conditions[?(@.type=="RolloutComplete")].status}' True 300 >/dev/null
# PoolWrites is the one condition that is not a health verdict: it reports write activity since the
# current leader process started, and a backend nothing writes to reads Unknown/NoWritesObserved. That
# exact reading is the expected one here, because this case writes nothing; any other non-True
# PoolWrites -- a revoked write, counters the leader could not report -- is still a finding.
NOT_TRUE="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{" "}{end}' 2>/dev/null \
  | tr ' ' '\n' | /usr/bin/grep -v -e '=True/' -e '^PoolWrites=Unknown/NoWritesObserved$' -e '^$' | tr '\n' ' ')"
PH_FINAL="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)"
if [ "$PH_FINAL" = "Ready" ] && [ -z "$NOT_TRUE" ]; then
  record PASS "the backend settles Ready with every health condition True" \
    "conditions: $(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{" "}{end}' 2>/dev/null)"
else
  record FAIL "the backend settles Ready with every health condition True" \
    "phase='${PH_FINAL}'; conditions not True (PoolWrites=Unknown/NoWritesObserved excepted): ${NOT_TRUE:-<none>}"
fi

results
