#!/usr/bin/env bash
#
# CASE 76 — RolloutComplete stays truthful mid-update on a replicated leader, the update converges
#            with the election gate intact, and the member-churn dip clears on budget
#   (MUTATING, self-recovering)
#
#   case-76.sh <NS>
#
# Goal:        An update to a 3-replica HA leader must be judgeable from the backend's own
#              condition, because the standard instrument is structurally blind here: the readiness
#              gate withholds two of the three replicas forever (only the lease holder serves), so
#              Deployment completion can never be reached and `kubectl rollout status` times out on
#              EVERY such rollout (measured: 421.6s, exit 1, while the rollout had long finished).
#              This case never calls that instrument. Instead it drives a second mid-update on its
#              own backend and proves the operator's predicate stays truthful throughout: every
#              sampled RolloutComplete is a benign state -- Unknown/UpdateNotObserved (the update
#              not yet observed), False/Progressing (observed, in flight), True/Complete -- and
#              never a deadline-ish stall reason; the update converges with updated=3 and exactly
#              one ready; every health condition settles True; and the transient phase dip the
#              image bump causes (the member DaemonSet rolls too, and re-registration briefly
#              degrades the backend) clears within its bounded window.
#
# Environment: Any cluster; no GPU, no RDMA (members are DRAM over TCP). <NS> must be the
#              operator's system namespace. THE IMAGE PAIR IS A DIRECTIONAL CONTRACT, shared with
#              case-75: start and target must BOTH be at least 0.3.12.post1 (older masters exit at
#              gflags parse on this operator's argv), BOTH must carry the lease backend, and BOTH
#              must be CPU-capable (a CUDA-only Mooncake tag crashes members on CPU-only nodes).
#              Defaults pin such a pair; override only with a pair satisfying the same clauses,
#              through E2E_MOONCAKE_IMAGE (the creation image) and E2E_MOONCAKE_ROLLOUT_IMAGE (the
#              first bump's target; the measured leg then bumps back to the creation image).
#
# Inputs:      All real, nothing mocked. One KVCacheBackend (3-replica HA leader, multi-tenancy on,
#              no snapshot) created on the start image, then updated TWICE by spec.image patch
#              alone: first to the target image (bringing the backend to the steady state the
#              measured leg starts from), then back to the start image while the sampler runs --
#              the second mid-update, sampled every ~5s, is the case's subject. No pod is deleted
#              by hand at any point.
#
# Expected:    - the first update settles (the measured leg's clean starting point);
#              - during the second update, every sampled RolloutComplete is one of
#                Unknown/UpdateNotObserved, False/Progressing, True/Complete, and no sample carries
#                a deadline-ish reason -- the predicate never misreports a stall;
#              - final: updated=3, exactly one ready (the election gate intact);
#              - final: backend Ready and every health condition True -- PoolWrites, which reports
#                write activity rather than health, reads Unknown/NoWritesObserved on this idle
#                backend and is accepted so;
#              - the phase dip (Provisioning/Degraded while members re-register) clears within
#                DIP_BOUND_SECS of the second patch -- measured ~34-50s here, ~2 min on an older
#                backend, so the bound carries margin rather than the fastest reading.
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
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-76.sh <NS>}"
CASE_ID=76

# A DIRECTIONAL PAIR, not one image; see the header's Environment for the three clauses any
# replacement pair must satisfy. The measured leg rolls target -> start, mirroring the reading this
# case was authored from.
IMAGE_START="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.12.post1-cpu}"
IMAGE_ROLL_TO="${E2E_MOONCAKE_ROLLOUT_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

# The suffix keeps two runs of this case apart: the backend's name is cluster-scoped, and every
# object it renders derives from it.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-rc-${SFX}"
LEADER="${BACKEND}-leader"

LEADER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader"

# The measured dips: ~34-50s on a freshly rolled backend, ~2 min on a long-lived one. The bound
# takes the slow reading with margin, so a FAIL here means the dip genuinely outgrew its budget
# rather than a fast machine being slow.
DIP_BOUND_SECS=300

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
  echo "[case-76] cleanup"
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # The leader Lease carries no owner reference, so deleting the backend leaves it behind. Delete it
  # by name after the backend is gone, because a standby still running would campaign it back.
  kubectl wait --for=delete "kvcachebackends.worker.gpustack.ai/${BACKEND}" --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete leases.coordination.k8s.io "$LEADER" --ignore-not-found --wait=false >/dev/null 2>&1 || true
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
    "phase never became Ready in 300s; nothing below can run. Check the image pair's three clauses in the header first: $(kubectl -n "$NS" get pod -l "$LEADER_SEL" -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{" "}{end}' 2>/dev/null)"
  results; exit 1
fi
if ! wait_for deployment "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
  record FAIL "exactly one leader replica is Ready before the updates" \
    "readyReplicas settled at '$(kubectl -n "$NS" get deployment "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' instead of 1"
  results; exit 1
fi

# ------------------------------------------------------------- first update: reach the steady half

# The measured leg is a SECOND mid-update, so the backend must first live through one: this bump
# brings it to the settled-on-the-target state that leg starts from. The pass signal here is the
# backend's own condition -- never `kubectl rollout status`, which cannot return for a
# multi-replica leader (see the header note).
kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --type merge \
  -p "{\"spec\":{\"image\":\"${IMAGE_ROLL_TO}\"}}" >/dev/null 2>&1

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

# ------------------------------------------------------------- the measured leg: second mid-update

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
wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.conditions[?(@.type=="RolloutComplete")].status}' True 300 >/dev/null
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
