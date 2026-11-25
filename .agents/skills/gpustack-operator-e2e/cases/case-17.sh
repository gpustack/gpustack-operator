#!/usr/bin/env bash
#
# CASE 17 — Replicated HA KVCacheBackend: Kubernetes Lease election, RBAC minimality, graceful and
#           hard failover, snapshot refusals, 1 -> 3 leader preservation, and image rollout/rollback
#           (MUTATING, self-recovering)
#
#   case-17.sh <NS>
#
# <NS> is the operator's own namespace, as everywhere in this suite. The KVCacheBackend is
# cluster-scoped; the Deployment, Service, Lease, ServiceAccounts, Roles and RoleBindings it renders
# all live in <NS>, while cluster-scoped KVCacheLeaderHandover events land in namespace default.
#
# Prerequisites: nodes=1; gpu=none; features=mooncake-store; cluster=any; inputs=E2E_MOONCAKE_IMAGE,E2E_MOONCAKE_ROLLOUT_IMAGE
# Goal:        Prove the HA KVCacheBackend contract end to end across two phases:
#                Phase 1 — Election, RBAC & failover:
#                  (1) With leader.replicas=3 and Kubernetes election, steady state is exactly 3
#                      desired / 1 ready, the Lease names the ready Pod, and ElectionObserved reads
#                      True/Electing.
#                  (2) Leader and member ServiceAccounts are sufficient and no more than sufficient
#                      under kubectl auth can-i (member update leases MUST print no).
#                  (3) Graceful delete and hard kill (--force --grace-period=0) of the serving Pod
#                      both move the Lease within FAILOVER_BOUND_SECS (120s) to a different ready
#                      replica with zero member restarts and zero member faults.
#                Phase 2 — Snapshot refusals, 1 -> 3 scaling & image rollout/rollback:
#                  (4) Snapshot switches in leader.extraArgs (-enable_snapshot,
#                      -snapshot_interval_seconds, -enable_snapshot_restore) are refused on create
#                      and update; None with 3 replicas is refused.
#                  (5) Default single leader already elects (-enable_ha=true + Lease holder), and
#                      scaling 1 -> 3 preserves the original leader Pod UID with readyReplicas=1.
#                  (6) Forward image rollout emits KVCacheLeaderHandover in namespace default with
#                      count equal to leaseTransitions, replaces all 3 leader Pods on the target
#                      image with 1 ready, and settles Ready with RolloutComplete=True/Complete.
#                  (7) Rollback image update never misreports a stall in RolloutComplete mid-flight,
#                      converges updated=3 ready=1, clears any member-churn dip within 300s, and
#                      settles Ready with every health condition True.
#
# Environment: Any cluster; no GPU, no RDMA (members are DRAM over TCP). NEEDS a directional CPU
#              store image pair (>= 0.3.12.post1 with Kubernetes Lease backend); override with
#              E2E_MOONCAKE_IMAGE and E2E_MOONCAKE_ROLLOUT_IMAGE.
#
# Inputs:      All real, nothing mocked. Phase 1 creates a 3-replica HA KVCacheBackend and deletes
#              the serving Pod gracefully then forcefully. Phase 2 tests dry-run snapshot refusals,
#              creates a 1-replica HA backend on IMAGE_START, scales 1 -> 3, patches spec.image to
#              IMAGE_ROLL_TO, then rolls back to IMAGE_START.
#
# Expected:    - Phase 1: 3 desired / 1 ready; Lease holder == ready Pod; ElectionObserved=True/Electing;
#                leader & member RBAC exact; graceful and hard failovers move Lease within 120s to a
#                new ready replica with zero member restarts and zero member faults; backend Ready.
#              - Phase 2: snapshot flags refused on create/update; default 1-replica elects; None+3
#                refused; 1 -> 3 preserves first leader UID; 0 baseline handover events; forward roll
#                emits KVCacheLeaderHandover in default matching leaseTransitions, 3/3 new image Pods,
#                1 ready, RolloutComplete=True/Complete; rollback RolloutComplete stays benign,
#                updated=3 ready=1, dip clears within 300s, all health conditions True.
#
# Cleanup:     Trap deletes both KVCacheBackends (cascading Deployment, Service, RBAC) and their
#              unowned leader Leases, plus any temporary trajectory file. Idempotent, on pass & fail.
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-ha-lib.sh"

NS="${1:?usage: case-17.sh <NS>}"
export CASE_ID=17
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

FAILOVER_BOUND_SECS=120
DIP_BOUND_SECS=300

SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"

# Phase 1 names.
BACKEND="kvcb-ha-${SFX}"
LEADER="${BACKEND}-leader"
MEMBER_SA="${BACKEND}-member"
LEADER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader"
MEMBER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=member-0"

# Phase 2 names.
BACKEND_RO="kvcb-ro-${SFX}"
LEADER_RO="${BACKEND_RO}-leader"
LEADER_RO_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND_RO},app.kubernetes.io/component=leader"

SWITCH_RE='spec\.connection\.managed\.leader\.extraArgs\[0\]: Forbidden: snapshots are not supported: .*can serve another key.s bytes instead of a miss'
COMPANION_RE='spec\.connection\.managed\.leader\.extraArgs\[0\]: Forbidden: it is read only when enable_snapshot or enable_snapshot_restore is set'

read -r IMAGE_START IMAGE_ROLL_TO <<<"$(kvha_resolve_rollout_images \
  "gpustack/mirrored-mooncake:0.3.12.post1-cpu" "gpustack/mirrored-mooncake:0.3.13.post1-cpu")"

TRAJ_FILE=""
teardown() {
  echo
  echo "[case-17] cleanup"
  if [ -n "$TRAJ_FILE" ]; then rm -f "$TRAJ_FILE" 2>/dev/null || true; fi
  kvha_teardown "$NS" "$BACKEND" "$LEADER"
  kvha_teardown "$NS" "$BACKEND_RO" "$LEADER_RO"
}
trap teardown EXIT

# ---------------------------------------------------------------- Phase 1 helpers

running_leader_pods() {
  kubectl -n "$NS" get pod -l "$LEADER_SEL" \
    -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' \
    2>/dev/null | wc -l | tr -d ' '
}

can_i() {
  local verb="$1" resource="$2" sa="$3" out
  out="$(kubectl auth can-i "$verb" "$resource" \
    --as="system:serviceaccount:${NS}:${sa}" -n "$NS" 2>/dev/null)"
  echo "${out:-<no answer>}"
}

sample_lease_holder() {
  local i
  for ((i = 0; i < 10; i++)); do
    OLD_READY="$(kvha_ready_leader_pod "$NS" "$LEADER_SEL")"
    OLD_READY_UID=""
    if [ -n "$OLD_READY" ]; then
      OLD_READY_UID="$(kvha_pod_uid "$NS" "$OLD_READY")"
    fi
    OLD_HOLDER="$(kvha_lease_holder "$NS" "$LEADER")"
    OLD_HOLDER_POD_UID="$(kvha_holder_pod_uid "$NS" "$LEADER_SEL" "$OLD_HOLDER")"
    [ -n "$OLD_READY_UID" ] && [ "$OLD_HOLDER_POD_UID" = "$OLD_READY_UID" ] && break
    sleep 1
  done
}

observe_lease_handoff() {
  IFS='|' read -r HOLDER_MOVED_AT _ MOVED_HOLDER_POD_UID MOVE_EVIDENCE HANDOFF_OBSERVED \
    NEW_HOLDER NEW_HOLDER_POD_UID NEW_READY NEW_READY_UID HANDOFF_WAIT_SECS \
    < <(kvha_observe_handoff_snapshot "$NS" "$BACKEND" "$LEADER" "$LEADER_SEL" "$TRAJ_FILE" \
      "$DELETE_EPOCH" "$OLD_READY_UID" "$OLD_HOLDER")
}

wait_for_new_ready_pod() {
  local i
  NEW_READY=""
  for ((i = 0; i < 180; i += 3)); do
    NEW_READY="$(kvha_ready_leader_pod "$NS" "$LEADER_SEL")"
    [ -n "$NEW_READY" ] && [ "$NEW_READY" != "$OLD_READY" ] && break
    sleep 3
  done
}

record_failover_bound() {
  local verdict
  verdict="$(kvha_failover_bound_verdict "$HOLDER_MOVED_AT" "$DELETE_EPOCH" "$FAILOVER_BOUND_SECS" "$MOVE_EVIDENCE")"
  record "${verdict%%|*}" "the Lease moves within the failover bound" "${verdict#*|}"
}

check_backend_ready() {
  local check="$1"
  if ! kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
    record FAIL "$check" \
      "phase never became Ready in 300s: $(kubectl -n "$NS" get pod -l "$LEADER_SEL" -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{" "}{end}' 2>/dev/null)"
    return 1
  fi
  record PASS "$check" "phase=Ready for ${BACKEND}"
  return 0
}

record_backend_ready_after_failover() {
  local check="$1"
  if kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
    record PASS "$check" "phase=Ready"
  else
    record FAIL "$check" \
      "phase='$(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)' after 300s: $(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
  fi
}

record_member_restarts() {
  local check="$1" before="$2" after="$3"
  if [ "$after" = "$before" ]; then
    record PASS "$check" \
      "every member kept its UID and restartCount: $(echo "$after" | tr '\n' ' ')"
  else
    record FAIL "$check" \
      "before: $(echo "$before" | tr '\n' ' ') | after: $(echo "$after" | tr '\n' ' ')"
  fi
}

record_member_faults() {
  local check="$1" pre_delete_sample="$2"
  local gap_reasons='NoAdminEndpoint|LeaderStarting|LeaderUnreachable|LeaderUnschedulable|ServicePlaneNotActive|ListingFailed|NoSegments|SegmentsShort'
  local trajectory member_faults
  trajectory="$(awk '!seen[$0]++ {printf "%s%s", (NR>1 ? " -> " : ""), $0}' "$TRAJ_FILE")"
  member_faults="$(grep -E '[|]False/' "$TRAJ_FILE" | grep -vE "[|]False/(${gap_reasons})\$" | sort -u | tr '\n' ' ')"
  if [ "${pre_delete_sample#*|}" != "True/Mounted" ]; then
    record FAIL "$check" \
      "the pre-delete reading was '${pre_delete_sample:-<empty>}', not True/Mounted; trajectory: ${trajectory}"
  elif [ -n "$member_faults" ]; then
    record FAIL "$check" \
      "MembersMounted sampled a member fault (${member_faults% }); trajectory: ${trajectory}"
  else
    record PASS "$check" \
      "True/Mounted before the delete, and no False reading outside the leader gap; trajectory: ${trajectory}"
  fi
}

# =============================================================================================
# Phase 1: Replicated HA election, RBAC, graceful & hard failover
# =============================================================================================
echo "== Phase 1: replicated HA election, RBAC, graceful and hard failover =="
PHASE1_OK=1

kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND}
spec:
  type: Mooncake
  image: ${IMAGE}
  transport:
    protocol: TCP
  connection:
    managed:
      leader:
        replicas: 3
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

check_backend_ready "the HA backend reaches Ready" || PHASE1_OK=0

if [ "$PHASE1_OK" = 1 ]; then
  DESIRED="$(kubectl -n "$NS" get deploy "$LEADER" -o jsonpath='{.spec.replicas}' 2>/dev/null)"
  POD_COUNT="$(kubectl -n "$NS" get pod -l "$LEADER_SEL" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  if [ "$DESIRED" = "3" ] && [ "$POD_COUNT" = "3" ]; then
    record PASS "the leader runs three replicas" "spec.replicas=${DESIRED}, ${POD_COUNT} Pods"
  else
    record FAIL "the leader runs three replicas" \
      "spec.replicas=${DESIRED:-<absent>}, ${POD_COUNT:-0} Pods; expected 3 and 3"
  fi

  if kvha_wait_for "$NS" deploy "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
    record PASS "exactly one leader replica is Ready" "status.readyReplicas=1 with 3 desired"
  else
    record FAIL "exactly one leader replica is Ready" \
      "readyReplicas settled at '$(kubectl -n "$NS" get deploy "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' instead of 1"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  HOLDER="$(kvha_lease_holder "$NS" "$LEADER")"
  if [ -n "$HOLDER" ]; then
    record PASS "the Lease exists and names a holder" "lease/${LEADER} holderIdentity=${HOLDER}"
  else
    record FAIL "the Lease exists and names a holder" "lease/${LEADER} holderIdentity is empty or absent"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  READY_POD="$(kvha_ready_leader_pod "$NS" "$LEADER_SEL")"
  READY_IP="$(kubectl -n "$NS" get pod "$READY_POD" -o jsonpath='{.status.podIP}' 2>/dev/null)"
  if [ -n "$READY_IP" ] && echo "$HOLDER" | grep -qF "$READY_IP"; then
    record PASS "the Lease holder is the ready Pod" \
      "holderIdentity=${HOLDER} carries ${READY_POD}'s podIP ${READY_IP}"
  else
    record FAIL "the Lease holder is the ready Pod" \
      "holderIdentity='${HOLDER}' vs ready Pod ${READY_POD:-<none>} at ${READY_IP:-<no ip>}"
  fi

  ELECTION=""
  for ((i = 0; i < 60; i += 3)); do
    ELECTION="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
      -o jsonpath='{range .status.conditions[?(@.type=="ElectionObserved")]}{.status}/{.reason}{end}' 2>/dev/null)"
    [ "$ELECTION" = "True/Electing" ] && break
    sleep 3
  done
  if [ "$ELECTION" = "True/Electing" ]; then
    record PASS "the backend reports the election it observes" "ElectionObserved=${ELECTION}"
  else
    record FAIL "the backend reports the election it observes" \
      "ElectionObserved='${ELECTION:-<absent>}' after 60s, expected True/Electing"
  fi

  L_YES="create=$(can_i create leases.coordination.k8s.io "$LEADER") get=$(can_i get leases.coordination.k8s.io "$LEADER") update=$(can_i update leases.coordination.k8s.io "$LEADER") patch-pods=$(can_i patch pods "$LEADER")"
  L_NO="watch=$(can_i watch leases.coordination.k8s.io "$LEADER") list=$(can_i list leases.coordination.k8s.io "$LEADER") delete=$(can_i delete leases.coordination.k8s.io "$LEADER") get-pods=$(can_i get pods "$LEADER") update-pods=$(can_i update pods "$LEADER")"
  if [ "$L_YES" = "create=yes get=yes update=yes patch-pods=yes" ]; then
    record PASS "the leader's account is sufficient" "$L_YES"
  else
    record FAIL "the leader's account is sufficient" "$L_YES -- all four must be yes"
  fi
  if [ "$L_NO" = "watch=no list=no delete=no get-pods=no update-pods=no" ]; then
    record PASS "the leader's account is no more than sufficient" "$L_NO"
  else
    record FAIL "the leader's account is no more than sufficient" "$L_NO -- all five must be no"
  fi

  M_YES="get=$(can_i get leases.coordination.k8s.io "$MEMBER_SA")"
  M_NO="update=$(can_i update leases.coordination.k8s.io "$MEMBER_SA") create=$(can_i create leases.coordination.k8s.io "$MEMBER_SA") delete=$(can_i delete leases.coordination.k8s.io "$MEMBER_SA") patch-pods=$(can_i patch pods "$MEMBER_SA")"
  if [ "$M_YES" = "get=yes" ]; then
    record PASS "the member's account is sufficient" "$M_YES"
  else
    record FAIL "the member's account is sufficient" "$M_YES -- get must be yes"
  fi
  if [ "$M_NO" = "update=no create=no delete=no patch-pods=no" ]; then
    record PASS "the member's account is no more than sufficient" "$M_NO (update MUST print no)"
  else
    record FAIL "the member's account is no more than sufficient" "$M_NO -- all four must be no"
  fi

  # Graceful failover.
  MEMBERS_BEFORE="$(kvha_member_snapshot "$NS" "$MEMBER_SEL")"
  if [ -z "$MEMBERS_BEFORE" ]; then
    record FAIL "members exist to be observed" "no member Pod matched ${MEMBER_SEL}"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  OLD_READY="" OLD_READY_UID="" OLD_HOLDER="" OLD_HOLDER_POD_UID=""
  sample_lease_holder
  if [ -z "$OLD_READY_UID" ] || [ "$OLD_HOLDER_POD_UID" != "$OLD_READY_UID" ]; then
    record FAIL "the Lease holder is the serving Pod immediately before the delete" \
      "holderIdentity='${OLD_HOLDER:-<empty>}' maps to '${OLD_HOLDER_POD_UID:-<none>}', ready Pod '${OLD_READY:-<none>}' has UID '${OLD_READY_UID:-<none>}'"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  TRAJ_FILE="$(mktemp)"
  PRE_DELETE_SAMPLE="$(kvha_members_mounted_sample "$NS" "$BACKEND")"
  echo "$PRE_DELETE_SAMPLE" >>"$TRAJ_FILE"
  DELETE_EPOCH="$(date +%s)"
  if DELETE_OUT="$(kubectl -n "$NS" delete pod "$OLD_READY" --wait=false 2>&1)"; then
    record PASS "the serving Pod deletion is accepted" "${OLD_READY} (${OLD_READY_UID})"
  else
    record FAIL "the serving Pod deletion is accepted" \
      "delete of ${OLD_READY} failed: $(echo "$DELETE_OUT" | tr '\n' ' ' | cut -c1-200)"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  observe_lease_handoff

  if [ -n "$HOLDER_MOVED_AT" ]; then
    record PASS "the Lease moves to a different holder" \
      "holder Pod UID ${OLD_READY_UID} -> ${MOVED_HOLDER_POD_UID}, $((HOLDER_MOVED_AT - DELETE_EPOCH))s after the delete; ${MOVE_EVIDENCE}"
  else
    record FAIL "the Lease moves to a different holder" \
      "${HANDOFF_WAIT_SECS}s after deleting ${OLD_READY}: holderIdentity='${NEW_HOLDER:-<empty>}', mapped Pod UID='${NEW_HOLDER_POD_UID:-<none>}'"
  fi

  if [ "$HANDOFF_OBSERVED" = "1" ]; then
    record PASS "the new Lease holder is the Ready replica" \
      "ready Pod ${NEW_READY} has holder Pod UID ${NEW_HOLDER_POD_UID}"
  else
    record FAIL "the new Lease holder is the Ready replica" \
      "${HANDOFF_WAIT_SECS}s after delete: holder UID='${NEW_HOLDER_POD_UID:-<none>}', ready UID='${NEW_READY_UID:-<none>}'"
  fi

  if [ -n "$HOLDER_MOVED_AT" ]; then
    record_failover_bound
  fi

  wait_for_new_ready_pod
  if [ -n "$NEW_READY" ] && [ "$NEW_READY" != "$OLD_READY" ]; then
    record PASS "a different replica becomes Ready" "${OLD_READY} -> ${NEW_READY}"
  else
    record FAIL "a different replica becomes Ready" \
      "ready Pod is '${NEW_READY:-<none>}' 180s after Lease moved (deleted: ${OLD_READY})"
  fi

  MEMBERS_AFTER="$(kvha_member_snapshot "$NS" "$MEMBER_SEL")"
  record_member_restarts "no member Pod restarts across the graceful failover" "$MEMBERS_BEFORE" "$MEMBERS_AFTER"
  record_member_faults "no member fault is reported throughout the graceful transition" "$PRE_DELETE_SAMPLE"
  rm -f "$TRAJ_FILE"; TRAJ_FILE=""
  record_backend_ready_after_failover "the backend is Ready after the graceful failover"

  RUNNING_LEADERS=""
  for ((i = 0; i < 120; i += 3)); do
    RUNNING_LEADERS="$(running_leader_pods)"
    [ "$RUNNING_LEADERS" = "3" ] && break
    sleep 3
  done
  if [ "$RUNNING_LEADERS" = "3" ]; then
    record PASS "the leader deployment returns to three running replicas" "3 leader Pods Running"
  else
    record FAIL "the leader deployment returns to three running replicas" \
      "${RUNNING_LEADERS:-0} leader Pods Running after 360s"
    PHASE1_OK=0
  fi
fi

# Hard kill failover.
if [ "$PHASE1_OK" = 1 ]; then
  check_backend_ready "the backend is Ready before the hard kill" || PHASE1_OK=0
fi

if [ "$PHASE1_OK" = 1 ]; then
  if kvha_wait_for "$NS" deploy "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
    record PASS "exactly one leader replica is Ready before the hard kill" "status.readyReplicas=1 with 3 desired"
  else
    record FAIL "exactly one leader replica is Ready before the hard kill" \
      "readyReplicas='$(kubectl -n "$NS" get deploy "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)'"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  OLD_HOLDER="" OLD_READY="" OLD_READY_UID="" OLD_HOLDER_POD_UID=""
  sample_lease_holder
  MEMBERS_BEFORE="$(kvha_member_snapshot "$NS" "$MEMBER_SEL")"
  if [ -z "$OLD_READY_UID" ] || [ "$OLD_HOLDER_POD_UID" != "$OLD_READY_UID" ] || [ -z "$MEMBERS_BEFORE" ]; then
    record FAIL "the Lease holder and members are ready before the hard kill" \
      "holder='${OLD_HOLDER:-<empty>}' ready='${OLD_READY:-<none>}'"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  TRAJ_FILE="$(mktemp)"
  PRE_DELETE_SAMPLE="$(kvha_members_mounted_sample "$NS" "$BACKEND")"
  echo "$PRE_DELETE_SAMPLE" >>"$TRAJ_FILE"
  DELETE_EPOCH="$(date +%s)"
  if DELETE_OUT="$(kubectl -n "$NS" delete pod "$OLD_READY" --force --grace-period=0 --wait=false 2>&1)"; then
    record PASS "the forced Pod deletion is accepted" "${OLD_READY} (${OLD_READY_UID}), forced with no grace period"
  else
    record FAIL "the forced Pod deletion is accepted" \
      "forced delete of ${OLD_READY} failed: $(echo "$DELETE_OUT" | tr '\n' ' ' | cut -c1-200)"
    PHASE1_OK=0
  fi
fi

if [ "$PHASE1_OK" = 1 ]; then
  observe_lease_handoff

  if [ -n "$HOLDER_MOVED_AT" ]; then
    record PASS "the Lease moves off the dead holder" \
      "holder Pod UID ${OLD_READY_UID} -> ${MOVED_HOLDER_POD_UID}, $((HOLDER_MOVED_AT - DELETE_EPOCH))s after hard kill; ${MOVE_EVIDENCE}"
  else
    record FAIL "the Lease moves off the dead holder" \
      "${HANDOFF_WAIT_SECS}s after force-deleting ${OLD_READY}: holderIdentity='${NEW_HOLDER:-<empty>}'"
  fi

  if [ "$HANDOFF_OBSERVED" = "1" ]; then
    record PASS "the new Lease holder is the Ready replica after hard kill" \
      "ready Pod ${NEW_READY} has holder Pod UID ${NEW_HOLDER_POD_UID}"
  else
    record FAIL "the new Lease holder is the Ready replica after hard kill" \
      "${HANDOFF_WAIT_SECS}s after hard kill: holder UID='${NEW_HOLDER_POD_UID:-<none>}', ready UID='${NEW_READY_UID:-<none>}'"
  fi

  if [ -n "$HOLDER_MOVED_AT" ]; then
    record_failover_bound
  fi

  wait_for_new_ready_pod
  if [ -n "$NEW_READY" ] && [ "$NEW_READY" != "$OLD_READY" ]; then
    record PASS "a different replica becomes Ready after hard kill" "${OLD_READY} -> ${NEW_READY}"
  else
    record FAIL "a different replica becomes Ready after hard kill" \
      "ready Pod is '${NEW_READY:-<none>}' 180s after Lease moved (killed: ${OLD_READY})"
  fi

  MEMBERS_AFTER="$(kvha_member_snapshot "$NS" "$MEMBER_SEL")"
  record_member_restarts "no member Pod restarts across the hard kill" "$MEMBERS_BEFORE" "$MEMBERS_AFTER"
  record_member_faults "no member fault is reported throughout the hard-kill transition" "$PRE_DELETE_SAMPLE"
  rm -f "$TRAJ_FILE"; TRAJ_FILE=""
  record_backend_ready_after_failover "the backend is Ready after the hard kill"
fi

# Tear down Phase 1 backend before Phase 2 starts.
kvha_teardown "$NS" "$BACKEND" "$LEADER"

# =============================================================================================
# Phase 2: Snapshot refusals, 1 -> 3 leader preservation, and image rollout / rollback
# =============================================================================================
echo "== Phase 2: snapshot refusals, 1 -> 3 scaling, and image rollout / rollback =="

handover_events() {
  kubectl -n default get events --field-selector "reason=KVCacheLeaderHandover,involvedObject.name=${BACKEND_RO}" \
    --sort-by=.lastTimestamp \
    -o jsonpath='{range .items[*]}{.lastTimestamp}{"|"}{.message}{"\n"}{end}' 2>/dev/null
}

manifest_ro() {
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND_RO}
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

expect_refused() {
  local out
  if out="$(manifest_ro "$3" | kubectl create --dry-run=server -f - 2>&1)"; then
    record FAIL "$1" "accepted: ${out}"
  elif [[ "$out" =~ $2 ]]; then
    record PASS "$1" "${BACKEND_RO}"
  else
    record FAIL "$1" "refused by another rule: ${out}"
  fi
}

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

if out="$(manifest_ro | kubectl create --dry-run=server -f - 2>&1)"; then
  record PASS "create without snapshot flags is accepted" "${BACKEND_RO}"
else
  record FAIL "create without snapshot flags is accepted" "${out}"
fi

expect_refused "-enable_snapshot in extraArgs is refused for serving wrong data" "$SWITCH_RE" \
  '        extraArgs: ["-enable_snapshot=true"]'
expect_refused "-snapshot_interval_seconds in extraArgs is refused as read only under a refused switch" "$COMPANION_RE" \
  '        extraArgs: ["-snapshot_interval_seconds=60"]'

PHASE2_OK=1
if ! out="$(manifest_ro | kubectl apply -f - 2>&1)"; then
  record FAIL "the rollout backend is created" "${out}"
  PHASE2_OK=0
fi

if [ "$PHASE2_OK" = 1 ]; then
  if out="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND_RO" --dry-run=server --type=merge \
    -p "{\"spec\":{\"image\":\"${IMAGE_START%:*}:case-17\"}}" 2>&1)"; then
    record PASS "an update changing only the image is accepted" "${BACKEND_RO}"
  else
    record FAIL "an update changing only the image is accepted" "${out}"
  fi

  if out="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND_RO" --dry-run=server --type=merge \
    -p '{"spec":{"connection":{"managed":{"leader":{"extraArgs":["-enable_snapshot_restore=true"]}}}}}' 2>&1)"; then
    record FAIL "an update adding -enable_snapshot_restore is refused" "accepted: ${out}"
  elif [[ "$out" =~ $SWITCH_RE ]]; then
    record PASS "an update adding -enable_snapshot_restore is refused" "${BACKEND_RO}"
  else
    record FAIL "an update adding -enable_snapshot_restore is refused" "refused by another rule: ${out}"
  fi

  if ! kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND_RO" '{.status.phase}' Ready 300 >/dev/null; then
    record FAIL "the backend reaches Ready on the start image" \
      "phase never became Ready in 300s: $(kubectl -n "$NS" get pod -l "$LEADER_RO_SEL" -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{" "}{end}' 2>/dev/null)"
    PHASE2_OK=0
  elif ! kvha_wait_for "$NS" deployment "$LEADER_RO" '{.status.readyReplicas}' 1 300 >/dev/null; then
    record FAIL "exactly one leader replica is Ready before the rollout" \
      "readyReplicas='$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)'"
    PHASE2_OK=0
  fi
fi

if [ "$PHASE2_OK" = 1 ]; then
  FIRST_UID="$(kubectl -n "$NS" get pod -l "$LEADER_RO_SEL" -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null)"
  HOLDER_RO="$(kvha_lease_holder "$NS" "$LEADER_RO")"
  ARGS="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null)"
  if [ -n "$FIRST_UID" ] && [ -n "$HOLDER_RO" ] && [[ "$ARGS" == *-enable_ha=true* ]]; then
    record PASS "the default single leader already elects" "leader Pod UID=${FIRST_UID}; Lease holder=${HOLDER_RO}"
  else
    record FAIL "the default single leader already elects" "UID=${FIRST_UID:-<none>}; holder=${HOLDER_RO:-<none>}"
    PHASE2_OK=0
  fi

  if REFUSAL="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND_RO" --dry-run=server --type merge \
    -p '{"spec":{"connection":{"managed":{"leader":{"replicas":3,"electionBackend":"None"}}}}}' 2>&1)"; then
    record FAIL "None with three replicas is refused" "server accepted the invalid patch"
    PHASE2_OK=0
  elif [[ "$REFUSAL" == *"leader.electionBackend"* ]]; then
    record PASS "None with three replicas is refused" "$REFUSAL"
  else
    record FAIL "None with three replicas is refused" "$REFUSAL"
    PHASE2_OK=0
  fi
fi

if [ "$PHASE2_OK" = 1 ]; then
  if ! kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND_RO" --type merge \
    -p '{"spec":{"connection":{"managed":{"leader":{"replicas":3}}}}}' >/dev/null 2>&1; then
    record FAIL "scaling to three is accepted" "replica patch failed"
    PHASE2_OK=0
  elif ! kvha_wait_for "$NS" deployment "$LEADER_RO" '{.status.replicas}' 3 300 >/dev/null \
    || ! kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND_RO" '{.status.phase}' Ready 300 >/dev/null; then
    record FAIL "scaling to three converges" "Deployment or backend did not converge to 3 / Ready"
    PHASE2_OK=0
  else
    SCALE_UIDS="$(kubectl -n "$NS" get pod -l "$LEADER_RO_SEL" -o jsonpath='{range .items[*]}{.metadata.uid}{" "}{end}' 2>/dev/null)"
    if [ -n "$FIRST_UID" ] && [[ " $SCALE_UIDS " == *" $FIRST_UID "* ]]; then
      record PASS "scaling to three preserves the first leader Pod" "first UID=${FIRST_UID}; current UIDs=${SCALE_UIDS}"
    else
      record FAIL "scaling to three preserves the first leader Pod" "first UID=${FIRST_UID}; current UIDs=${SCALE_UIDS}"
      PHASE2_OK=0
    fi
  fi
fi

if [ "$PHASE2_OK" = 1 ]; then
  sleep 20
  TRANS_BEFORE="$(kvha_lease_transitions "$NS" "$LEADER_RO")"
  BASELINE_EVENTS="$(handover_events)"
  if [ -z "$BASELINE_EVENTS" ]; then
    record PASS "steady state emits zero handover events (clean baseline)" \
      "lease transitions=${TRANS_BEFORE}; no KVCacheLeaderHandover event for ${BACKEND_RO} in default"
  else
    record FAIL "steady state emits zero handover events (clean baseline)" \
      "pre-existing events: $(echo "$BASELINE_EVENTS" | tr '\n' ' ' | cut -c1-200)"
    PHASE2_OK=0
  fi
fi

if [ "$PHASE2_OK" = 1 ]; then
  if ! kvha_wait_for "$NS" deployment "$LEADER_RO" '{.status.readyReplicas}' 1 300 >/dev/null; then
    record FAIL "exactly one leader replica is Ready before the updates" \
      "readyReplicas='$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)'"
    PHASE2_OK=0
  fi
fi

if [ "$PHASE2_OK" = 1 ]; then
  PRE_UIDS="$(kubectl -n "$NS" get pod -l "$LEADER_RO_SEL" -o jsonpath='{range .items[*]}{.metadata.uid}{" "}{end}' 2>/dev/null)"
  T0="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if ! kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND_RO" --type merge \
    -p "{\"spec\":{\"image\":\"${IMAGE_ROLL_TO}\"}}" >/dev/null 2>&1; then
    record FAIL "the image patch is accepted" "kubectl patch of spec.image failed"
    PHASE2_OK=0
  fi
fi

if [ "$PHASE2_OK" = 1 ]; then
  TRAJ_FILE="$(mktemp)"
  EVENT_MSG=""
  EVENT_AT=""
  for ((i = 0; i < 300; i += 5)); do
    kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" \
      -o jsonpath='{.status.phase}|{.status.conditions[?(@.type=="RolloutComplete")].status}/{.status.conditions[?(@.type=="RolloutComplete")].reason}{"\n"}' 2>/dev/null >>"$TRAJ_FILE"
    LATEST="$(handover_events | awk -F'|' 'NF {msg=$0} END {print msg}')"
    if [ -n "$LATEST" ]; then
      EVENT_AT="${LATEST%%|*}"
      EVENT_MSG="${LATEST#*|}"
    fi
    UPD="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null)"
    RDY="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
    [ -n "$EVENT_MSG" ] && [ "$UPD" = "3" ] && [ "$RDY" = "1" ] && break
    sleep 5
  done
  TRAJECTORY="$(awk -F'|' '!seen[$0]++ {printf "%s%s", (NR>1 ? " -> " : ""), $0}' "$TRAJ_FILE")"
  rm -f "$TRAJ_FILE"; TRAJ_FILE=""

  if [ -n "$EVENT_MSG" ]; then
    record PASS "the rollout emits the KVCacheLeaderHandover event" \
      "read in namespace default at ${EVENT_AT}: ${EVENT_MSG}"
  else
    record FAIL "the rollout emits the KVCacheLeaderHandover event" \
      "no event for ${BACKEND_RO} in namespace default within 300s"
  fi

  TRANS_AFTER="$(kvha_lease_transitions "$NS" "$LEADER_RO")"
  COUNT_IN_MSG="$(printf '%s' "$EVENT_MSG" | sed -n 's/.*recorded \([0-9][0-9]*\) handovers in total.*/\1/p')"
  if [ -n "$COUNT_IN_MSG" ] && [ "$COUNT_IN_MSG" = "$TRANS_AFTER" ]; then
    record PASS "the event's cumulative count equals the lease's transitions" \
      "message says ${COUNT_IN_MSG}, leaseTransitions=${TRANS_AFTER} (baseline ${TRANS_BEFORE})"
  else
    record FAIL "the event's cumulative count equals the lease's transitions" \
      "message count '${COUNT_IN_MSG:-<none parsed>}' vs leaseTransitions='${TRANS_AFTER}' (baseline ${TRANS_BEFORE})"
  fi

  POD_LINES="$(kubectl -n "$NS" get pod -l "$LEADER_RO_SEL" \
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
    record FAIL "every leader pod is a replacement on the new image" "could not read pre-patch leader pods"
  elif [ "$NEW_COUNT" = "3" ]; then
    record PASS "every leader pod is a replacement on the new image" \
      "3 of 3 pods on ${IMAGE_ROLL_TO}, none among ${PRE_COUNT} pre-patch pods (${T0})"
  else
    record FAIL "every leader pod is a replacement on the new image" \
      "${NEW_COUNT} of 3 pods are post-patch replacements on ${IMAGE_ROLL_TO}"
  fi

  RDY_FINAL="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
  if [ "$RDY_FINAL" = "1" ]; then
    record PASS "exactly one leader replica is Ready after the rollout" "readyReplicas=1 with 3 desired"
  else
    record FAIL "exactly one leader replica is Ready after the rollout" "readyReplicas='${RDY_FINAL}' instead of 1"
  fi

  if kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND_RO" '{.status.phase}' Ready 300 >/dev/null; then
    record PASS "the backend settles back to Ready" "phase=Ready; rollout trajectory: ${TRAJECTORY}"
  else
    record FAIL "the backend settles back to Ready" \
      "phase='$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" -o jsonpath='{.status.phase}' 2>/dev/null)' after 300s"
  fi

  ROLL_STATUS="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" \
    -o jsonpath='{.status.conditions[?(@.type=="RolloutComplete")].status}/{.status.conditions[?(@.type=="RolloutComplete")].reason}' 2>/dev/null)"
  NOT_TRUE="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{" "}{end}' 2>/dev/null \
    | tr ' ' '\n' | /usr/bin/grep -v -e '=True/' -e '^PoolWrites=Unknown/NoWritesObserved$' -e '^$' | tr '\n' ' ')"
  if [ "$ROLL_STATUS" = "True/Complete" ] && [ -z "$NOT_TRUE" ]; then
    record PASS "RolloutComplete=True/Complete and every health condition True" \
      "conditions: $(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{" "}{end}' 2>/dev/null)"
  else
    record FAIL "RolloutComplete=True/Complete and every health condition True" \
      "RolloutComplete='${ROLL_STATUS}'; non-True: ${NOT_TRUE:-<none>}"
  fi

  # Rollback leg.
  FIRST_SETTLED=0
  for ((i = 0; i < 300; i += 5)); do
    PH="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" -o jsonpath='{.status.phase}' 2>/dev/null)"
    UPD="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null)"
    RDY="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
    ROL="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" \
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
      "after 300s: phase='${PH}' updated='${UPD}' ready='${RDY}' rollout='${ROL}'"
    PHASE2_OK=0
  fi
fi

if [ "$PHASE2_OK" = 1 ]; then
  sleep 20
  T1_EPOCH="$(date +%s)"
  LEADER_GEN_BEFORE="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.metadata.generation}' 2>/dev/null)"

  kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND_RO" --type merge \
    -p "{\"spec\":{\"image\":\"${IMAGE_START}\"}}" >/dev/null 2>&1

  TRAJ_FILE="$(mktemp)"
  BAD_STATES=""
  ROLLOUT_SEEN=0
  DIP_STARTED=0
  DIP_START_AT=""
  DIP_CLEARED_AT=""
  for ((i = 0; i < DIP_BOUND_SECS; i += 5)); do
    NOW="$(date +%s)"
    PH="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" -o jsonpath='{.status.phase}' 2>/dev/null)"
    ROL="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" \
      -o jsonpath='{.status.conditions[?(@.type=="RolloutComplete")].status}/{.status.conditions[?(@.type=="RolloutComplete")].reason}' 2>/dev/null)"
    UPD="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null)"
    RDY="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
    echo "t+$((NOW - T1_EPOCH))s phase=${PH} rollout=${ROL} updated=${UPD} ready=${RDY}" >>"$TRAJ_FILE"

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

    LEADER_GEN_NOW="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.metadata.generation}' 2>/dev/null)"
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
  rm -f "$TRAJ_FILE"; TRAJ_FILE=""
  READABLE_STATES="$(printf '%s' "$OBSERVED_ROLLOUT_STATES" | tr -d ' /')"
  if [ "$ROLLOUT_SEEN" != "1" ]; then
    record FAIL "RolloutComplete never misreports a stall mid-update" \
      "leader Deployment generation never moved past ${LEADER_GEN_BEFORE} within ${DIP_BOUND_SECS}s"
  elif [ -z "$READABLE_STATES" ]; then
    record FAIL "RolloutComplete never misreports a stall mid-update" \
      "window opened but no RolloutComplete sample was readable"
  elif [ -z "$BAD_STATES" ]; then
    record PASS "RolloutComplete never misreports a stall mid-update" \
      "sampled states: ${OBSERVED_ROLLOUT_STATES}; trajectory: ${TRAJECTORY}"
  else
    record FAIL "RolloutComplete never misreports a stall mid-update" \
      "illegal or deadline-ish samples:${BAD_STATES}; trajectory: ${TRAJECTORY}"
  fi

  UPD_FINAL="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null)"
  RDY_FINAL="$(kubectl -n "$NS" get deployment "$LEADER_RO" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
  if [ "$UPD_FINAL" = "3" ] && [ "$RDY_FINAL" = "1" ]; then
    record PASS "the update converges with the election gate intact" "updated=3 ready=1"
  else
    record FAIL "the update converges with the election gate intact" "updated='${UPD_FINAL}' ready='${RDY_FINAL}'"
  fi

  if [ "$DIP_STARTED" = "0" ]; then
    record PASS "the member-churn dip clears within its window" \
      "no dip observed this run (every sample Ready); bound ${DIP_BOUND_SECS}s holds"
  elif [ -n "$DIP_CLEARED_AT" ]; then
    record PASS "the member-churn dip clears within its window" \
      "dip started ~${DIP_START_AT}, phase Ready again at ${DIP_CLEARED_AT} within ${DIP_BOUND_SECS}s"
  else
    record FAIL "the member-churn dip clears within its window" \
      "phase never returned to Ready within ${DIP_BOUND_SECS}s; trajectory: ${TRAJECTORY}"
  fi

  kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND_RO" '{.status.conditions[?(@.type=="RolloutComplete")].status}' True 300 >/dev/null
  NOT_TRUE="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{" "}{end}' 2>/dev/null \
    | tr ' ' '\n' | /usr/bin/grep -v -e '=True/' -e '^PoolWrites=Unknown/NoWritesObserved$' -e '^$' | tr '\n' ' ')"
  PH_FINAL="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" -o jsonpath='{.status.phase}' 2>/dev/null)"
  if [ "$PH_FINAL" = "Ready" ] && [ -z "$NOT_TRUE" ]; then
    record PASS "the backend settles Ready with every health condition True" \
      "conditions: $(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND_RO" -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}{" "}{end}' 2>/dev/null)"
  else
    record FAIL "the backend settles Ready with every health condition True" \
      "phase='${PH_FINAL}'; non-True: ${NOT_TRUE:-<none>}"
  fi
fi

results
