#!/usr/bin/env bash
#
# CASE 64 — A hard-killed KV cache leader (no grace period, no farewell) still yields the Lease,
#   a standby takes over, and the members come through untouched   (MUTATING, self-recovering)
#
#   case-64.sh <NS>
#
# Goal:        The graceful-delete failover is the friendly path: the holder exits on its own
#              schedule and keeps serving through termination grace. The failure that matters
#              for availability is the rude one -- SIGKILL, power loss, a network partition --
#              where the holder never releases the Lease and the standby must notice an EXPIRED
#              lease rather than an answered handoff. This case kills the serving leader with
#              --force --grace-period=0 and proves the arrangement survives it: the Lease moves
#              to a different holder within the failover bound, a different replica becomes
#              ready, no member Pod restarts, no member fault is reported at any sample, and
#              the backend converges back to Ready.
#
# Environment: Any cluster; no GPU, no RDMA (members are DRAM over TCP). One node is enough.
#              NEEDS a store image built with the Kubernetes Lease leadership backend -- with a
#              published upstream image no replica can campaign at all and the "exactly one
#              ready" wait times out. That timeout is the signature of the wrong image, not a
#              flake. Override with E2E_MOONCAKE_IMAGE; the default below carries the backend.
#
# Inputs:      All real, nothing mocked. One KVCacheBackend (replicas 3, highAvailability, one
#              DRAM member group of 2Gi per node). The failure is injected with
#              `kubectl delete pod --force --grace-period=0` on the Lease holder: the API object
#              vanishes immediately and the container runtime SIGKILLs the process, so the
#              holder neither releases the Lease nor serves another request.
#
# Expected:    - the backend reaches Ready with exactly one of three leader replicas ready;
#              - after the hard kill: the Lease's holder changes WITHIN THE FAILOVER BOUND
#                (120s; a hard kill skips the 30s grace, so this path is expected FASTER than
#                the graceful one), a DIFFERENT replica becomes ready, every member Pod keeps
#                its UID and restartCount, MembersMounted never reports False at any sample,
#                and the backend's phase returns to Ready.
#
# Cleanup:     Trap deletes the KVCacheBackend; owner references cascade to the Deployment,
#              Service, Lease, and accounts. Nothing else is touched. Idempotent, runs on pass
#              AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-64.sh <NS>}"
CASE_ID=64
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

# The suffix keeps two runs of this case apart: the backend's name is cluster-scoped, and every
# object it renders -- the Lease included -- derives from it.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-hk-${SFX}"
LEADER="${BACKEND}-leader"

LEADER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader"
MEMBER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=member-0"

# A failover is a promise about SPEED, and a check that records the number without judging it
# waves a three-minute election through. Same tripwire as the graceful-failover case: 120s is
# ~2.5x the measured 43-44s graceful path, and a hard kill skips the grace period entirely.
FAILOVER_BOUND_SECS=120

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

results() {
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in ${ROWS[@]+"${ROWS[@]}"}; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
  [ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED"; return 1; }
  echo "[case-${CASE_ID}] all checks passed"
  return 0
}

teardown() {
  echo
  echo "[case-64] cleanup"
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

ready_leader_pod() {
  kubectl -n "$NS" get pod -l "$LEADER_SEL" \
    -o jsonpath='{range .items[?(@.status.containerStatuses[0].ready==true)]}{.metadata.name}{"\n"}{end}' \
    2>/dev/null | head -1
}

lease_holder() {
  kubectl -n "$NS" get leases.coordination.k8s.io "$LEADER" \
    -o jsonpath='{.spec.holderIdentity}' 2>/dev/null
}

lease_renew_time() {
  kubectl -n "$NS" get leases.coordination.k8s.io "$LEADER" \
    -o jsonpath='{.spec.renewTime}' 2>/dev/null
}

holder_pod_uid() {
  local holder="$1" holder_ip pod_uid pod_ip deleting phase
  case "$holder" in
    \[*\]:*)
      holder_ip="${holder#\[}"
      holder_ip="${holder_ip%%\]*}"
      ;;
    *:*) holder_ip="${holder%:*}" ;;
    *) holder_ip="$holder" ;;
  esac
  while IFS='|' read -r pod_uid pod_ip deleting phase; do
    if [ "$phase" != "Running" ] || [ -n "$deleting" ] || [ -z "$pod_ip" ] \
      || [ "$pod_ip" != "$holder_ip" ]; then
      continue
    fi
    echo "$pod_uid"
    return 0
  done < <(kubectl -n "$NS" get pod -l "$LEADER_SEL" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"|"}{.status.podIP}{"|"}{.metadata.deletionTimestamp}{"|"}{.status.phase}{"\n"}{end}' \
    2>/dev/null)
}

observe_handoff_snapshot() {
  local observed_at="$1"
  if [ -z "$HOLDER_MOVED_AT" ] && [ -n "$NEW_HOLDER_POD_UID" ] \
    && [ "$NEW_HOLDER_POD_UID" != "$OLD_READY_UID" ]; then
    if [ "$NEW_HOLDER" != "$OLD_HOLDER" ]; then
      HOLDER_MOVED_AT="$observed_at"
      MOVED_HOLDER="$NEW_HOLDER"
      MOVED_HOLDER_POD_UID="$NEW_HOLDER_POD_UID"
      MOVE_EVIDENCE="holderIdentity changed"
    elif [ -n "$NEW_RENEW_TIME" ]; then
      if [ "$REUSED_HOLDER_POD_UID" != "$NEW_HOLDER_POD_UID" ] \
        || [ -z "$REUSED_HOLDER_RENEW_TIME" ]; then
        REUSED_HOLDER_POD_UID="$NEW_HOLDER_POD_UID"
        REUSED_HOLDER_RENEW_TIME="$NEW_RENEW_TIME"
      elif [ "$NEW_RENEW_TIME" != "$REUSED_HOLDER_RENEW_TIME" ]; then
        HOLDER_MOVED_AT="$observed_at"
        MOVED_HOLDER="$NEW_HOLDER"
        MOVED_HOLDER_POD_UID="$NEW_HOLDER_POD_UID"
        MOVE_EVIDENCE="replacement renewed the reused holderIdentity"
      fi
    fi
  fi
  if [ -n "$NEW_HOLDER_POD_UID" ] && [ "$NEW_HOLDER_POD_UID" != "$OLD_READY_UID" ] \
    && [ "$NEW_HOLDER_POD_UID" = "$NEW_READY_UID" ]; then
    HANDOFF_OBSERVED=1
    if [ -z "$HOLDER_MOVED_AT" ]; then
      HOLDER_MOVED_AT="$observed_at"
      MOVED_HOLDER="$NEW_HOLDER"
      MOVED_HOLDER_POD_UID="$NEW_HOLDER_POD_UID"
      MOVE_EVIDENCE="Ready replacement confirmed the reused holderIdentity"
    fi
  fi
}

record_failover_bound() {
  local moved_secs=$((HOLDER_MOVED_AT - DELETE_EPOCH))
  if [ "$moved_secs" -le "$FAILOVER_BOUND_SECS" ]; then
    record PASS "the Lease moves within the failover bound" "${moved_secs}s <= ${FAILOVER_BOUND_SECS}s"
  else
    record FAIL "the Lease moves within the failover bound" \
      "${moved_secs}s > ${FAILOVER_BOUND_SECS}s -- the election still works but no longer meets the failover budget"
  fi
}

# ---------------------------------------------------------------- setup

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
        highAvailability: {}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "the backend reaches Ready" \
    "phase never became Ready in 300s; nothing below can run. If no leader Pod is ever ready, the \
store image likely carries no Kubernetes Lease leadership backend (upstream images answer \
UNAVAILABLE_IN_CURRENT_MODE and campaign forever): $(kubectl -n "$NS" get pod -l "$LEADER_SEL" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{" "}{end}' 2>/dev/null)"
  results; exit 1
fi
record PASS "the backend reaches Ready" "phase=Ready for ${BACKEND}"

if wait_for deploy "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
  record PASS "exactly one leader replica is Ready" "status.readyReplicas=1 with 3 desired"
else
  record FAIL "exactly one leader replica is Ready" \
    "readyReplicas settled at '$(kubectl -n "$NS" get deploy "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' instead of 1"
  results; exit 1
fi

OLD_HOLDER=""
OLD_READY=""
OLD_READY_UID=""
OLD_HOLDER_POD_UID=""
for ((i = 0; i < 10; i++)); do
  OLD_READY="$(ready_leader_pod)"
  OLD_READY_UID=""
  if [ -n "$OLD_READY" ]; then
    OLD_READY_UID="$(kubectl -n "$NS" get pod "$OLD_READY" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  fi
  OLD_HOLDER="$(lease_holder)"
  OLD_HOLDER_POD_UID="$(holder_pod_uid "$OLD_HOLDER")"
  [ -n "$OLD_READY_UID" ] && [ "$OLD_HOLDER_POD_UID" = "$OLD_READY_UID" ] && break
  sleep 1
done
if [ -z "$OLD_READY_UID" ] || [ "$OLD_HOLDER_POD_UID" != "$OLD_READY_UID" ]; then
  record FAIL "the Lease holder is the serving Pod immediately before the kill" \
    "holderIdentity='${OLD_HOLDER:-<empty>}' maps to '${OLD_HOLDER_POD_UID:-<none>}', ready Pod '${OLD_READY:-<none>}' has UID '${OLD_READY_UID:-<none>}'"
  results; exit 1
fi

# ------------------------------------------------- induce the HARD failure

# The member snapshot is name=UID:restarts, so a replacement Pod -- which would legitimately carry
# restartCount 0 -- is still caught, by its UID.
member_snapshot() {
  kubectl -n "$NS" get pod -l "$MEMBER_SEL" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}:{.status.containerStatuses[0].restartCount}{"\n"}{end}' \
    2>/dev/null | sort
}
MEMBERS_BEFORE="$(member_snapshot)"
if [ -z "$MEMBERS_BEFORE" ]; then
  record FAIL "members exist to be observed" "no member Pod matched ${MEMBER_SEL}; the backend is Ready, so this is a defect"
  results; exit 1
fi

# --force --grace-period=0: the API object is deleted at once and the runtime SIGKILLs the holder,
# so the Lease is left held by a dead process -- exactly what a power loss or a partition looks
# like from the standby's side. The graceful path is covered elsewhere; this case exists because
# the two paths fail differently.
TRAJ_FILE="$(mktemp)"
DELETE_EPOCH="$(date +%s)"
if DELETE_OUT="$(kubectl -n "$NS" delete pod "$OLD_READY" --force --grace-period=0 --wait=false 2>&1)"; then
  record PASS "the serving Pod deletion is accepted" "${OLD_READY} (${OLD_READY_UID}), forced with no grace period"
else
  record FAIL "the serving Pod deletion is accepted" \
    "forced delete of ${OLD_READY} (${OLD_READY_UID}) failed: $(echo "$DELETE_OUT" | tr '\n' ' ' | cut -c1-200)"
  results; exit 1
fi

NEW_HOLDER=""
NEW_HOLDER_POD_UID=""
NEW_READY=""
NEW_READY_UID=""
NEW_RENEW_TIME=""
REUSED_HOLDER_POD_UID=""
REUSED_HOLDER_RENEW_TIME=""
HOLDER_MOVED_AT=""
MOVED_HOLDER=""
MOVED_HOLDER_POD_UID=""
MOVE_EVIDENCE=""
HANDOFF_OBSERVED=0
HANDOFF_DEADLINE=$((DELETE_EPOCH + 240))
# A changed identity or renewTime timestamps the move directly. A Ready holder is an upper bound:
# the Lease must have moved no later than the serving replacement became observable.
while [ "$(date +%s)" -lt "$HANDOFF_DEADLINE" ]; do
  kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" \
    -o jsonpath='{.status.phase}{"|"}{.conditions[?(@.type=="MembersMounted")].status}{"\n"}' \
    2>/dev/null >>"$TRAJ_FILE"
  NEW_HOLDER="$(lease_holder)"
  NEW_HOLDER_POD_UID="$(holder_pod_uid "$NEW_HOLDER")"
  NEW_RENEW_TIME=""
  if [ -n "$NEW_HOLDER_POD_UID" ] && [ "$NEW_HOLDER_POD_UID" != "$OLD_READY_UID" ] \
    && [ "$NEW_HOLDER" = "$OLD_HOLDER" ]; then
    NEW_RENEW_TIME="$(lease_renew_time)"
  fi
  NEW_READY="$(ready_leader_pod)"
  NEW_READY_UID=""
  if [ -n "$NEW_READY" ]; then
    NEW_READY_UID="$(kubectl -n "$NS" get pod "$NEW_READY" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  fi
  observe_handoff_snapshot "$(date +%s)"
  [ "$HANDOFF_OBSERVED" = "1" ] && break
  sleep 2
done
HANDOFF_FINISHED_AT="$(date +%s)"
HANDOFF_WAIT_SECS=$((HANDOFF_FINISHED_AT - DELETE_EPOCH))

if [ -n "$HOLDER_MOVED_AT" ]; then
  record PASS "the Lease moves off the dead holder" \
    "holder Pod UID ${OLD_READY_UID} -> ${MOVED_HOLDER_POD_UID}, $((HOLDER_MOVED_AT - DELETE_EPOCH))s after the hard kill; ${MOVE_EVIDENCE}, holderIdentity=${MOVED_HOLDER}"
else
  record FAIL "the Lease moves off the dead holder" \
    "${HANDOFF_WAIT_SECS}s after force-deleting ${OLD_READY} (${OLD_READY_UID}): holderIdentity='${NEW_HOLDER:-<empty>}', mapped Pod UID='${NEW_HOLDER_POD_UID:-<none>}'"
fi

if [ "$HANDOFF_OBSERVED" = "1" ]; then
  record PASS "the new Lease holder is the Ready replica" \
    "ready Pod ${NEW_READY} has holder Pod UID ${NEW_HOLDER_POD_UID}"
else
  record FAIL "the new Lease holder is the Ready replica" \
    "${HANDOFF_WAIT_SECS}s after the hard kill: holder Pod UID='${NEW_HOLDER_POD_UID:-<none>}', ready Pod UID='${NEW_READY_UID:-<none>}'"
fi

if [ -n "$HOLDER_MOVED_AT" ]; then
  record_failover_bound
fi

NEW_READY=""
for ((i = 0; i < 180; i += 3)); do
  NEW_READY="$(ready_leader_pod)"
  [ -n "$NEW_READY" ] && [ "$NEW_READY" != "$OLD_READY" ] && break
  sleep 3
done
if [ -n "$NEW_READY" ] && [ "$NEW_READY" != "$OLD_READY" ]; then
  record PASS "a different replica becomes Ready" "${OLD_READY} -> ${NEW_READY}"
else
  record FAIL "a different replica becomes Ready" \
    "ready Pod is '${NEW_READY:-<none>}' 180s after the Lease moved (killed: ${OLD_READY})"
fi

MEMBERS_AFTER="$(member_snapshot)"
if [ "$MEMBERS_AFTER" = "$MEMBERS_BEFORE" ]; then
  record PASS "no member Pod restarts across the failover" \
    "every member kept its UID and restartCount: $(echo "$MEMBERS_AFTER" | tr '\n' ' ')"
else
  record FAIL "no member Pod restarts across the failover" \
    "before: $(echo "$MEMBERS_BEFORE" | tr '\n' ' ') | after: $(echo "$MEMBERS_AFTER" | tr '\n' ' ')"
fi

# The trajectory: every sampled phase|MembersMounted pair, de-duplicated in order of first
# appearance. A member fault firing on a healthy failover is the failure this case exists to
# catch; a transient leader-unavailable reading during the gap is a true statement about the gap
# and is reported as data, not passed or failed.
TRAJECTORY="$(awk '!seen[$0]++ {printf "%s%s", (NR>1 ? " -> " : ""), $0}' "$TRAJ_FILE")"
if ! grep -q '|False' "$TRAJ_FILE"; then
  record PASS "no member fault is reported throughout the transition" \
    "MembersMounted never sampled False; phase trajectory: ${TRAJECTORY}"
else
  record FAIL "no member fault is reported throughout the transition" \
    "MembersMounted sampled False during the failover; trajectory: ${TRAJECTORY}"
fi
rm -f "$TRAJ_FILE"

# The new leader being Ready and every member having re-registered with it are different events:
# members remount one at a time, and the controller only re-reads the leader on its own cadence.
# Assert CONVERGENCE through the bounded poll, not a snapshot -- a snapshot cannot tell mid-flight
# from stuck.
if wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record PASS "the backend is Ready after the failover" "phase=Ready"
else
  record FAIL "the backend is Ready after the failover" \
    "phase='$(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)' after a 300s settle window: $(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
fi

results
