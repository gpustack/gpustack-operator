#!/usr/bin/env bash
#
# CASE 62 — A replicated KV cache leader elects exactly one of three through a Lease, the two
#   rendered accounts are sufficient and no more, and an induced failover moves the Lease without
#   touching a member   (MUTATING, self-recovering)
#
#   case-62.sh <NS>
#
# <NS> is the operator's own namespace, as everywhere in this suite. The KVCacheBackend is
# cluster-scoped; the Deployment, Service, Lease, ServiceAccounts, Roles and RoleBindings it renders
# all live in <NS>.
#
# Goal:        With leader.replicas=3 and leader.highAvailability={}, three leader processes run and
#              exactly one serves, elected through a Kubernetes Lease. This proves on a real API
#              server what rendered objects and unit tests cannot:
#                (1) THE STEADY STATE IS AN EQUALITY: 3 desired / 1 ready. Three ready would mean
#                    the readiness gate does not gate and standbys sit in the Service's endpoints;
#                    zero ready means nobody won the Lease. Anything but 3/1 is a defect.
#                (2) THE LEASE NAMES THE SERVING POD. holderIdentity is non-empty and identifies the
#                    one ready replica, not a standby.
#                (3) THE TWO ACCOUNTS ARE SUFFICIENT AND NO MORE THAN SUFFICIENT, asked of the API
#                    server's authorizer (kubectl auth can-i), not compared against a rendered Role:
#                    the leader's account may create/get/update leases and patch its own pods; the
#                    member's account may ONLY get leases -- `update leases` must print "no".
#                (4) A FAILOVER MOVES THE LEASE AND NOTHING ELSE. Deleting the serving replica
#                    changes the holder, a different replica becomes ready, NO member Pod restarts
#                    (restartCount unchanged), and the backend reports no member fault throughout:
#                    a health rule that fires on a successful failover is worse than the one it
#                    replaced.
#
# Environment: Any cluster; no GPU, no RDMA (members are DRAM over TCP). One node is enough: the
#              leader replicas are a Deployment and need not be spread, and the member DaemonSet
#              needs one node to have a segment to register.
#              NEEDS a store image built with the Kubernetes Lease leadership backend -- with a
#              published upstream image every replica campaigns unanswered and stays a permanent
#              standby, so the "exactly one ready" wait times out. That timeout is the signature of
#              the wrong image, not a flake. Override with E2E_MOONCAKE_IMAGE; the default below
#              carries the backend.
#
# Inputs:      All real, nothing mocked. One KVCacheBackend (replicas 3, highAvailability, one DRAM
#              member group of 2Gi per node). The failover is induced by deleting the ready leader
#              Pod -- the one the Lease names.
#
# Expected:    - the backend reaches Ready;
#              - the leader Deployment: spec.replicas=3, three Pods, readyReplicas=1;
#              - the Lease <backend>-leader exists; holderIdentity non-empty and naming the ready
#                Pod's address;
#              - leader SA: create/get/update leases=yes, patch pods=yes; watch/list/delete leases
#                and get/update pods=no;
#              - member SA: get leases=yes; update/create/delete leases and patch pods=no;
#              - after deleting the serving Pod: the Lease's holder changes WITHIN THE FAILOVER
#                BOUND (120s; graceful failovers here measure 43-44s), a DIFFERENT replica
#                becomes ready, every member Pod keeps its UID and restartCount, MembersMounted
#                never reports False at any sample, and the backend's phase returns to Ready.
#
# Cleanup:     Trap deletes the KVCacheBackend; owner references cascade to the Deployment, Service,
#              Lease, both ServiceAccounts, both Roles and both RoleBindings. Nothing else is
#              touched. Idempotent, runs on pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-62.sh <NS>}"
CASE_ID=62
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

# The suffix keeps two runs of this case apart: the backend's name is cluster-scoped, and every
# object it renders -- the Lease included -- derives from it.
#
# LC_ALL=C and the disabled pipefail are both load-bearing: under a UTF-8 locale tr dies on
# /dev/urandom, and with pipefail on the SIGPIPE from head turns a trailing fallback into an append.
# The measurements behind both halves are recorded once, at the same idiom in
# _kvcache-inject-lib.sh.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-ha-${SFX}"
LEADER="${BACKEND}-leader"   # Deployment, Service, Lease, and the leader's ServiceAccount
MEMBER_SA="${BACKEND}-member"

LEADER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader"
MEMBER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=member-0"

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
  echo "[case-62] cleanup"
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

# ready_leader_pod prints the name of the one leader Pod that is Ready, or nothing. With the
# readiness gate working there is exactly one; an empty result and a two-line result are both
# answers the checks below report rather than reinterpret.
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

# can_i asks the API server's authorizer -- not a rendered Role -- and prints the answer word.
# `auth` is not one of the shim's retried verbs, so the answer is the server's, once.
can_i() {
  local verb="$1" resource="$2" sa="$3" out
  out="$(kubectl auth can-i "$verb" "$resource" \
    --as="system:serviceaccount:${NS}:${sa}" -n "$NS" 2>/dev/null)"
  echo "${out:-<no answer>}"
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

# ------------------------------------------------- (1) the steady state is an equality

DESIRED="$(kubectl -n "$NS" get deploy "$LEADER" -o jsonpath='{.spec.replicas}' 2>/dev/null)"
POD_COUNT="$(kubectl -n "$NS" get pod -l "$LEADER_SEL" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
if [ "$DESIRED" = "3" ] && [ "$POD_COUNT" = "3" ]; then
  record PASS "the leader runs three replicas" "spec.replicas=${DESIRED}, ${POD_COUNT} Pods"
else
  record FAIL "the leader runs three replicas" \
    "spec.replicas=${DESIRED:-<absent>}, ${POD_COUNT:-0} Pods; expected 3 and 3"
fi

# The wait is part of the check, not a preamble: it is where a standby that passes readiness, or a
# winner that never does, shows up. 300s covers a first pull of the store image.
if wait_for deploy "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
  record PASS "exactly one leader replica is Ready" "status.readyReplicas=1 with 3 desired"
else
  record FAIL "exactly one leader replica is Ready" \
    "readyReplicas settled at '$(kubectl -n "$NS" get deploy "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' \
instead of 1 -- three ready means standbys are in the Service's endpoints, zero means no replica won \
the Lease. With an upstream store image this is the expected failure: nothing electable is compiled in"
  results; exit 1
fi

# ------------------------------------------------- (2) the Lease names the serving pod

HOLDER="$(lease_holder)"
if [ -n "$HOLDER" ]; then
  record PASS "the Lease exists and names a holder" "lease/${LEADER} holderIdentity=${HOLDER}"
else
  record FAIL "the Lease exists and names a holder" \
    "lease/${LEADER} holderIdentity is empty or the Lease is absent"
  results; exit 1
fi

READY_POD="$(ready_leader_pod)"
READY_IP="$(kubectl -n "$NS" get pod "$READY_POD" -o jsonpath='{.status.podIP}' 2>/dev/null)"
# The leader campaigns with its own rpc address, so the holder string should carry the serving
# Pod's IP. Naming a standby here would put the Service's one endpoint and the Lease's one holder
# on two different processes -- the split this whole arrangement exists to prevent.
if [ -n "$READY_IP" ] && echo "$HOLDER" | grep -qF "$READY_IP"; then
  record PASS "the Lease holder is the ready Pod" \
    "holderIdentity=${HOLDER} carries ${READY_POD}'s podIP ${READY_IP}"
else
  record FAIL "the Lease holder is the ready Pod" \
    "holderIdentity='${HOLDER}' vs ready Pod ${READY_POD:-<none>} at ${READY_IP:-<no ip>}"
fi

# ------------------------------------------------- (3) sufficient, and no more than sufficient

# Each vector is asked of the authorizer as one batch, then printed verbatim: a FAIL shows every
# answer, so which grant is wrong is in the table rather than in a rerun.
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
# `update` is the one the design calls out by name: a member that could update the Lease could take
# leadership from the leader, so the denial is asserted on its own line before the rest of the
# over-grant probes.
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

# ------------------------------------------------- (4) induce the failover

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

OLD_READY=""
OLD_READY_UID=""
OLD_HOLDER=""
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
  record FAIL "the Lease holder is the serving Pod immediately before the delete" \
    "holderIdentity='${OLD_HOLDER:-<empty>}' maps to '${OLD_HOLDER_POD_UID:-<none>}', ready Pod '${OLD_READY:-<none>}' has UID '${OLD_READY_UID:-<none>}'"
  results; exit 1
fi

# Sampling the backend's own report THROUGH the transition is what "no fault throughout" means:
# a phase read only before and after would miss a health rule that fires mid-failover and clears.
TRAJ_FILE="$(mktemp)"
DELETE_EPOCH="$(date +%s)"
if DELETE_OUT="$(kubectl -n "$NS" delete pod "$OLD_READY" --wait=false 2>&1)"; then
  record PASS "the serving Pod deletion is accepted" "${OLD_READY} (${OLD_READY_UID})"
else
  record FAIL "the serving Pod deletion is accepted" \
    "delete of ${OLD_READY} (${OLD_READY_UID}) failed: $(echo "$DELETE_OUT" | tr '\n' ' ' | cut -c1-200)"
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
# A changed identity timestamps the move directly. If the replacement reuses the same IP, a later
# renewTime change after that Pod appears proves the new process has taken ownership.
while [ "$(date +%s)" -lt "$HANDOFF_DEADLINE" ]; do
  kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" \
    -o jsonpath='{.status.phase}{"|"}{.conditions[?(@.type=="MembersMounted")].status}{"\n"}' \
    2>/dev/null >>"$TRAJ_FILE"
  NEW_HOLDER="$(lease_holder)"
  NEW_HOLDER_POD_UID="$(holder_pod_uid "$NEW_HOLDER")"
  if [ -z "$HOLDER_MOVED_AT" ] && [ -n "$NEW_HOLDER_POD_UID" ] \
    && [ "$NEW_HOLDER_POD_UID" != "$OLD_READY_UID" ]; then
    if [ "$NEW_HOLDER" != "$OLD_HOLDER" ]; then
      HOLDER_MOVED_AT="$(date +%s)"
      MOVED_HOLDER="$NEW_HOLDER"
      MOVED_HOLDER_POD_UID="$NEW_HOLDER_POD_UID"
      MOVE_EVIDENCE="holderIdentity changed"
    else
      NEW_RENEW_TIME="$(lease_renew_time)"
      if [ -n "$NEW_RENEW_TIME" ]; then
        if [ "$REUSED_HOLDER_POD_UID" != "$NEW_HOLDER_POD_UID" ] \
          || [ -z "$REUSED_HOLDER_RENEW_TIME" ]; then
          REUSED_HOLDER_POD_UID="$NEW_HOLDER_POD_UID"
          REUSED_HOLDER_RENEW_TIME="$NEW_RENEW_TIME"
        elif [ "$NEW_RENEW_TIME" != "$REUSED_HOLDER_RENEW_TIME" ]; then
          HOLDER_MOVED_AT="$(date +%s)"
          MOVED_HOLDER="$NEW_HOLDER"
          MOVED_HOLDER_POD_UID="$NEW_HOLDER_POD_UID"
          MOVE_EVIDENCE="replacement renewed the reused holderIdentity"
        fi
      fi
    fi
  fi
  NEW_READY="$(ready_leader_pod)"
  NEW_READY_UID=""
  if [ -n "$NEW_READY" ]; then
    NEW_READY_UID="$(kubectl -n "$NS" get pod "$NEW_READY" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  fi
  if [ -n "$HOLDER_MOVED_AT" ] && [ -n "$NEW_HOLDER_POD_UID" ] \
    && [ "$NEW_HOLDER_POD_UID" != "$OLD_READY_UID" ] \
    && [ "$NEW_HOLDER_POD_UID" = "$NEW_READY_UID" ]; then
    HANDOFF_OBSERVED=1
    break
  fi
  sleep 2
done
HANDOFF_FINISHED_AT="$(date +%s)"
HANDOFF_WAIT_SECS=$((HANDOFF_FINISHED_AT - DELETE_EPOCH))

if [ -n "$HOLDER_MOVED_AT" ]; then
  record PASS "the Lease moves to a different holder" \
    "holder Pod UID ${OLD_READY_UID} -> ${MOVED_HOLDER_POD_UID}, $((HOLDER_MOVED_AT - DELETE_EPOCH))s after the delete; ${MOVE_EVIDENCE}, holderIdentity=${MOVED_HOLDER}"
else
  record FAIL "the Lease moves to a different holder" \
    "${HANDOFF_WAIT_SECS}s after deleting ${OLD_READY} (${OLD_READY_UID}): holderIdentity='${NEW_HOLDER:-<empty>}', mapped Pod UID='${NEW_HOLDER_POD_UID:-<none>}'"
fi

if [ "$HANDOFF_OBSERVED" = "1" ]; then
  record PASS "the new Lease holder is the Ready replica" \
    "ready Pod ${NEW_READY} has holder Pod UID ${NEW_HOLDER_POD_UID}"
else
  record FAIL "the new Lease holder is the Ready replica" \
    "${HANDOFF_WAIT_SECS}s after the delete: holder Pod UID='${NEW_HOLDER_POD_UID:-<none>}', ready Pod UID='${NEW_READY_UID:-<none>}'"
fi

# A failover is a promise about SPEED, and a check that records the number without judging it
# waves a three-minute election through. Graceful-delete failovers measure 43-44s on this
# suite (termination grace + Lease expiry); 120s is a regression tripwire at ~2.5x that --
# loose enough for scheduler noise, tight enough that a real slowdown cannot walk past.
if [ -n "$HOLDER_MOVED_AT" ]; then
  MOVED_SECS=$((HOLDER_MOVED_AT - DELETE_EPOCH))
  if [ "$MOVED_SECS" -le 120 ]; then
    record PASS "the Lease moves within the failover bound" "${MOVED_SECS}s <= 120s"
  else
    record FAIL "the Lease moves within the failover bound" \
      "${MOVED_SECS}s > 120s -- the election still works but no longer meets the failover budget"
  fi
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
    "ready Pod is '${NEW_READY:-<none>}' 180s after the Lease moved (deleted: ${OLD_READY})"
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
# appearance. A member fault firing on a healthy failover is the failure this case exists to catch;
# a transient leader-unavailable reading during the gap is a true statement about the gap and is
# reported as data, not passed or failed. (awk joins instead of paste: paste -sd treats a multi-char
# delimiter as a SET of single-char separators used in rotation.)
TRAJECTORY="$(awk '!seen[$0]++ {printf "%s%s", (NR>1 ? " -> " : ""), $0}' "$TRAJ_FILE")"
if ! grep -q '|False' "$TRAJ_FILE"; then
  record PASS "no member fault is reported throughout the transition" \
    "MembersMounted never sampled False; phase trajectory: ${TRAJECTORY}"
else
  record FAIL "no member fault is reported throughout the transition" \
    "MembersMounted sampled False during the failover; trajectory: ${TRAJECTORY}"
fi
rm -f "$TRAJ_FILE"

# The new leader being Ready and every member having re-registered with it are different
# events: members remount one at a time (observed: 0/3 segments listed right at the new
# leader's readiness, 1/3 ten seconds later, full remount tens of seconds after that), and
# the controller only re-reads the leader on its own cadence. Reading phase once, right at
# readiness, races that remount and reports a mid-flight Degraded as a verdict. Assert
# CONVERGENCE instead, through the suite's bounded-poll idiom -- a single snapshot cannot
# distinguish mid-flight from stuck.
if wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record PASS "the backend is Ready after the failover" "phase=Ready"
else
  record FAIL "the backend is Ready after the failover" \
    "phase='$(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)' after a 300s settle window: $(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
fi

results
