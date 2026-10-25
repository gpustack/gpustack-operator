#!/usr/bin/env bash
#
# CASE 62 — A replicated KV cache leader elects exactly one of three through a Lease, the two
#   rendered accounts are sufficient and no more, and both ways of failing the holder -- a graceful
#   delete and a hard kill -- move the Lease without touching a member   (MUTATING, self-recovering)
#
#   case-62.sh <NS>
#
# <NS> is the operator's own namespace, as everywhere in this suite. The KVCacheBackend is
# cluster-scoped; the Deployment, Service, Lease, ServiceAccounts, Roles and RoleBindings it renders
# all live in <NS>.
#
# Prerequisites: nodes=1; gpu=none; features=mooncake-store; cluster=any; inputs=E2E_MOONCAKE_IMAGE
# Goal:        With leader.replicas=3 and leader.electionBackend=Kubernetes, three leader processes run and
#              exactly one serves, elected through a Kubernetes Lease. This proves on a real API
#              server what rendered objects and unit tests cannot:
#                (1) THE STEADY STATE IS AN EQUALITY: 3 desired / 1 ready. Three ready would mean
#                    the readiness gate does not gate and standbys sit in the Service's endpoints;
#                    zero ready means nobody won the Lease. Anything but 3/1 is a defect.
#                (2) THE LEASE NAMES THE SERVING POD. holderIdentity is non-empty and identifies the
#                    one ready replica, not a standby -- and the backend's ElectionObserved condition
#                    reports that election as True/Electing. The condition exists to flag an image
#                    that cannot elect (it then reads False/NoHolder), so it is only a verdict once
#                    it has also been seen to read True on an image that can.
#                (3) THE TWO ACCOUNTS ARE SUFFICIENT AND NO MORE THAN SUFFICIENT, asked of the API
#                    server's authorizer (kubectl auth can-i), not compared against a rendered Role:
#                    the leader's account may create/get/update leases and patch its own pods; the
#                    member's account may ONLY get leases -- `update leases` must print "no".
#                (4) A FAILOVER MOVES THE LEASE AND NOTHING ELSE, AND BOTH WAYS OF FAILING PASS. The
#                    serving replica is failed twice, on one cluster: first deleted gracefully, so
#                    the holder exits on its own schedule and keeps serving through termination
#                    grace; then hard-killed with --force --grace-period=0, so the holder never
#                    releases the Lease and a standby has to notice an EXPIRED lease rather than an
#                    answered handoff. Each time the holder changes, a different replica becomes
#                    ready, NO member Pod restarts (restartCount unchanged), and the backend reports
#                    no member fault throughout: a health rule that fires on a successful failover
#                    is worse than the one it replaced.
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
# Inputs:      All real, nothing mocked. One KVCacheBackend (replicas 3, Kubernetes election, one DRAM
#              member group of 2Gi per node) serves both failovers. They are induced by deleting the
#              ready leader Pod -- the one the Lease names: once with the grace period the kubelet
#              grants, once with no grace period at all.
#
# Expected:    - the backend reaches Ready;
#              - the leader Deployment: spec.replicas=3, three Pods, readyReplicas=1;
#              - the Lease <backend>-leader exists; holderIdentity non-empty and naming the ready
#                Pod's address; the backend's ElectionObserved condition reads True/Electing within
#                60s;
#              - leader SA: create/get/update leases=yes, patch pods=yes; watch/list/delete leases
#                and get/update pods=no;
#              - member SA: get leases=yes; update/create/delete leases and patch pods=no;
#              - after the graceful delete: the Lease's holder changes WITHIN THE FAILOVER
#                BOUND (120s; graceful failovers here measure 43-44s), a DIFFERENT replica
#                becomes ready, every member Pod keeps its UID and restartCount, MembersMounted
#                reads True/Mounted before the delete and no sample reports a member fault (the
#                leader-gap and re-registration reasons are data), and the phase returns to Ready;
#              - the leader Deployment is back to three running replicas before the hard kill, so
#                the second failover has standbys to campaign;
#              - after the hard kill: the holder changes within the same bound (a hard kill skips
#                the grace period, so this path is expected FASTER than the graceful one), a
#                DIFFERENT replica becomes ready, every member Pod keeps its UID and restartCount,
#                MembersMounted reads True/Mounted before the kill, no sample reports a member
#                fault, and the phase returns to Ready again.
#
# Cleanup:     Trap deletes the KVCacheBackend; owner references cascade to the Deployment, Service,
#              both ServiceAccounts, both Roles and both RoleBindings. The Lease is the store's, not
#              the operator's, carries no owner and is deleted by name once the backend is gone.
#              Nothing else is touched. Idempotent, runs on pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-62.sh <NS>}"
export CASE_ID=62
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

# A failover is a promise about SPEED, and a check that records the number without judging it
# waves a three-minute election through. Graceful-delete failovers measure 43-44s on this suite
# (termination grace + Lease expiry); 120s is a regression tripwire at ~2.5x that -- loose enough
# for scheduler noise, tight enough that a real slowdown cannot walk past. A hard kill skips the
# grace period entirely, so the same bound is the tripwire there too.
FAILOVER_BOUND_SECS=120

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

# The Lease readers, the member snapshot, the failover handoff and the teardown live in the shared HA library.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-ha-lib.sh"

teardown() {
  echo
  echo "[case-62] cleanup"
  kvha_teardown "$NS" "$BACKEND" "$LEADER"
}
trap teardown EXIT

# ---------------------------------------------------------------- readers
# What the shared HA library does not answer, this case asks for itself.

# running_leader_pods prints the number of leader Pods that are Running. The standbys are Running
# but NOT Ready -- the readiness gate admits only the leader -- so this, not readyReplicas, is the
# count that says the Deployment has a full set of candidates to campaign again.
running_leader_pods() {
  kubectl -n "$NS" get pod -l "$LEADER_SEL" \
    -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' \
    2>/dev/null | wc -l | tr -d ' '
}

# can_i asks the API server's authorizer -- not a rendered Role -- and prints the answer word.
# `auth` is not one of the shim's retried verbs, so the answer is the server's, once.
can_i() {
  local verb="$1" resource="$2" sa="$3" out
  out="$(kubectl auth can-i "$verb" "$resource" \
    --as="system:serviceaccount:${NS}:${sa}" -n "$NS" 2>/dev/null)"
  echo "${out:-<no answer>}"
}

# ---------------------------------------------------------------- failover machinery

# sample_lease_holder reads the Pod that is Ready, the Pod the Lease names, and their UIDs. Both
# failovers read it through this one helper: the second failover must target the holder the first
# one left behind, not the Pod the first one deleted.
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

# observe_lease_handoff watches the Lease until the move is observable and records when it
# happened. The watch and the detection live in the shared HA library, which returns one snapshot
# of what it saw; the graceful and the hard-kill failover each start from a clean slate through
# the same code.
observe_lease_handoff() {
  IFS='|' read -r HOLDER_MOVED_AT MOVED_HOLDER MOVED_HOLDER_POD_UID MOVE_EVIDENCE HANDOFF_OBSERVED \
    NEW_HOLDER NEW_HOLDER_POD_UID NEW_READY NEW_READY_UID HANDOFF_WAIT_SECS \
    < <(kvha_observe_handoff_snapshot "$NS" "$BACKEND" "$LEADER" "$LEADER_SEL" "$TRAJ_FILE" \
      "$DELETE_EPOCH" "$OLD_READY_UID" "$OLD_HOLDER")
}

# wait_for_new_ready_pod waits for a leader Pod that is Ready and is not the one that was deleted.
wait_for_new_ready_pod() {
  local i
  NEW_READY=""
  for ((i = 0; i < 180; i += 3)); do
    NEW_READY="$(kvha_ready_leader_pod "$NS" "$LEADER_SEL")"
    [ -n "$NEW_READY" ] && [ "$NEW_READY" != "$OLD_READY" ] && break
    sleep 3
  done
}

# record_failover_bound emits the "the Lease moves within the failover bound" row. The judgement
# itself is the shared HA library's; the row stays with the case.
record_failover_bound() {
  local verdict
  verdict="$(kvha_failover_bound_verdict "$HOLDER_MOVED_AT" "$DELETE_EPOCH" "$FAILOVER_BOUND_SECS" "$MOVE_EVIDENCE")"
  record "${verdict%%|*}" "the Lease moves within the failover bound" "${verdict#*|}"
}

# ---------------------------------------------------------------- row printers
#
# One wrapper per check that both failover paths read the same way. Each takes the CHECK label as
# its first argument and emits one row per call, so a reader sees the label at the call site and
# the wording in one place.

# record_backend_ready emits the "the backend reaches Ready" row from one probe of the backend's
# phase. A timeout is fatal: nothing below can run without it.
record_backend_ready() {
  local check="$1"
  if ! kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
    record FAIL "$check" \
      "phase never became Ready in 300s; nothing below can run. If no leader Pod is ever ready, the \
store image likely carries no Kubernetes Lease leadership backend (upstream images answer \
UNAVAILABLE_IN_CURRENT_MODE and campaign forever): $(kubectl -n "$NS" get pod -l "$LEADER_SEL" \
-o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{" "}{end}' 2>/dev/null)"
    results; exit 1
  fi
  record PASS "$check" "phase=Ready for ${BACKEND}"
}

# record_backend_ready_after_failover emits the "the backend is Ready after the failover" row. The
# new leader being Ready and every member having re-registered with it are different events:
# members remount one at a time, and the controller only re-reads the leader on its own cadence.
# Assert CONVERGENCE through the bounded poll, not a snapshot -- a snapshot cannot tell mid-flight
# from stuck.
record_backend_ready_after_failover() {
  local check="$1"
  if kvha_wait_for "$NS" kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
    record PASS "$check" "phase=Ready"
  else
    record FAIL "$check" \
      "phase='$(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phase}' 2>/dev/null)' after a 300s settle window: $(kubectl -n "$NS" get kvcachebackends.worker.gpustack.ai "$BACKEND" -o jsonpath='{.status.phaseMessage}' 2>/dev/null | cut -c1-160)"
  fi
}

# record_member_restarts emits the "no member Pod restarts across the failover" row: the member
# Pods are the same Pods, by UID and restartCount, before and after.
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

# record_member_faults emits the "no member fault is reported throughout the transition" row from
# the trajectory sampled across the failover.
#
# The trajectory: every sampled phase|MembersMounted pair, de-duplicated in order of first
# appearance. A member fault firing on a healthy failover is the failure this case exists to catch;
# a transient leader-unavailable reading during the gap is a true statement about the gap and is
# reported as data, not passed or failed. (awk joins instead of paste: paste -sd treats a multi-char
# delimiter as a SET of single-char separators used in rotation.)
#
# The reasons below are that gap and the members re-registering with the new leader
# (setMembersObservationFailed and observeMembers in the KVCacheBackend controller); any other False
# reason names a member fault. The pre-delete sample must read True/Mounted, which is the positive
# baseline that proves the reader returns the condition at all.
record_member_faults() {
  local check="$1" pre_delete_sample="$2"
  local gap_reasons='NoAdminEndpoint|LeaderStarting|LeaderUnreachable|LeaderUnschedulable|ServicePlaneNotActive|ListingFailed|NoSegments|SegmentsShort'
  local trajectory member_faults
  trajectory="$(awk '!seen[$0]++ {printf "%s%s", (NR>1 ? " -> " : ""), $0}' "$TRAJ_FILE")"
  member_faults="$(grep -E '[|]False/' "$TRAJ_FILE" | grep -vE "[|]False/(${gap_reasons})\$" | sort -u | tr '\n' ' ')"
  if [ "${pre_delete_sample#*|}" != "True/Mounted" ]; then
    record FAIL "$check" \
      "the pre-delete reading was '${pre_delete_sample:-<empty>}', not True/Mounted, so the samples below prove nothing; trajectory: ${trajectory}"
  elif [ -n "$member_faults" ]; then
    record FAIL "$check" \
      "MembersMounted sampled a member fault (${member_faults% }); trajectory: ${trajectory}"
  else
    record PASS "$check" \
      "True/Mounted before the delete, and no False reading outside the leader gap; trajectory: ${trajectory}"
  fi
}

# ---------------------------------------------------------------- setup

# One backend serves both failovers: the graceful one and the hard-kill one run against the same
# three replicas, so the second one starts from a cluster the first one already restored.
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

record_backend_ready "the backend reaches Ready"

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
if kvha_wait_for "$NS" deploy "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
  record PASS "exactly one leader replica is Ready" "status.readyReplicas=1 with 3 desired"
else
  record FAIL "exactly one leader replica is Ready" \
    "readyReplicas settled at '$(kubectl -n "$NS" get deploy "$LEADER" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' \
instead of 1 -- three ready means standbys are in the Service's endpoints, zero means no replica won \
the Lease. With an upstream store image this is the expected failure: nothing electable is compiled in"
  results; exit 1
fi

# ------------------------------------------------- (2) the Lease names the serving pod

HOLDER="$(kvha_lease_holder "$NS" "$LEADER")"
if [ -n "$HOLDER" ]; then
  record PASS "the Lease exists and names a holder" "lease/${LEADER} holderIdentity=${HOLDER}"
else
  record FAIL "the Lease exists and names a holder" \
    "lease/${LEADER} holderIdentity is empty or the Lease is absent"
  results; exit 1
fi

READY_POD="$(kvha_ready_leader_pod "$NS" "$LEADER_SEL")"
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

# The operator's own verdict about that Lease, read where it is written rather than inferred from the
# holder above. Waited for, because it is written by the reconcile the Lease change enqueues, not by
# the pass that made the backend Ready.
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
    "ElectionObserved='${ELECTION:-<absent>}' after 60s, expected True/Electing while lease/${LEADER} names ${HOLDER}"
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

# ------------------------------- (4) induce the graceful failover: delete the serving Pod

MEMBERS_BEFORE="$(kvha_member_snapshot "$NS" "$MEMBER_SEL")"
if [ -z "$MEMBERS_BEFORE" ]; then
  record FAIL "members exist to be observed" "no member Pod matched ${MEMBER_SEL}; the backend is Ready, so this is a defect"
  results; exit 1
fi

OLD_READY=""
OLD_READY_UID=""
OLD_HOLDER=""
OLD_HOLDER_POD_UID=""
sample_lease_holder
if [ -z "$OLD_READY_UID" ] || [ "$OLD_HOLDER_POD_UID" != "$OLD_READY_UID" ]; then
  record FAIL "the Lease holder is the serving Pod immediately before the delete" \
    "holderIdentity='${OLD_HOLDER:-<empty>}' maps to '${OLD_HOLDER_POD_UID:-<none>}', ready Pod '${OLD_READY:-<none>}' has UID '${OLD_READY_UID:-<none>}'"
  results; exit 1
fi

# Sampling the backend's own report THROUGH the transition is what "no fault throughout" means:
# a phase read only before and after would miss a health rule that fires mid-failover and clears.
TRAJ_FILE="$(mktemp)"
PRE_DELETE_SAMPLE="$(kvha_members_mounted_sample "$NS" "$BACKEND")"
echo "$PRE_DELETE_SAMPLE" >>"$TRAJ_FILE"
DELETE_EPOCH="$(date +%s)"
if DELETE_OUT="$(kubectl -n "$NS" delete pod "$OLD_READY" --wait=false 2>&1)"; then
  record PASS "the serving Pod deletion is accepted" "${OLD_READY} (${OLD_READY_UID})"
else
  record FAIL "the serving Pod deletion is accepted" \
    "delete of ${OLD_READY} (${OLD_READY_UID}) failed: $(echo "$DELETE_OUT" | tr '\n' ' ' | cut -c1-200)"
  results; exit 1
fi

observe_lease_handoff

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
  record_failover_bound
fi

wait_for_new_ready_pod
if [ -n "$NEW_READY" ] && [ "$NEW_READY" != "$OLD_READY" ]; then
  record PASS "a different replica becomes Ready" "${OLD_READY} -> ${NEW_READY}"
else
  record FAIL "a different replica becomes Ready" \
    "ready Pod is '${NEW_READY:-<none>}' 180s after the Lease moved (deleted: ${OLD_READY})"
fi

MEMBERS_AFTER="$(kvha_member_snapshot "$NS" "$MEMBER_SEL")"
record_member_restarts "no member Pod restarts across the failover" "$MEMBERS_BEFORE" "$MEMBERS_AFTER"

record_member_faults "no member fault is reported throughout the transition" "$PRE_DELETE_SAMPLE"
rm -f "$TRAJ_FILE"

record_backend_ready_after_failover "the backend is Ready after the failover"

# The hard kill below needs three candidates to campaign, and the graceful failover deleted one
# Pod: the Deployment has to bring the replacement back before the second failover starts. The
# standbys are Running but not Ready -- the readiness gate admits only the leader -- so this counts
# Running Pods, not ready ones.
RUNNING_LEADERS=""
for ((i = 0; i < 120; i += 3)); do
  RUNNING_LEADERS="$(running_leader_pods)"
  [ "$RUNNING_LEADERS" = "3" ] && break
  sleep 3
done
if [ "$RUNNING_LEADERS" = "3" ]; then
  record PASS "the leader deployment returns to three running replicas" \
    "3 leader Pods Running after the graceful failover"
else
  record FAIL "the leader deployment returns to three running replicas" \
    "${RUNNING_LEADERS:-0} leader Pods Running 360s after the graceful failover; the hard kill needs three candidates to campaign"
  results; exit 1
fi

# ------------------------------- (5) the same cluster, the hard kill: no grace, no farewell

# The graceful delete above is the friendly path: the holder exited on its own schedule and kept
# serving through termination grace. The failure that matters for availability is the rude one --
# SIGKILL, power loss, a network partition -- where the holder never releases the Lease and the
# standby must notice an EXPIRED lease rather than an answered handoff. The two paths fail
# differently, so both are exercised, on one backend.

# The new baseline is read from the cluster the graceful failover left behind, and the holder is
# RE-SAMPLED here: the graceful failover moved it, so the Pod the Lease names now is not the Pod
# the first failover deleted.
record_backend_ready "the backend reaches Ready"

if kvha_wait_for "$NS" deploy "$LEADER" '{.status.readyReplicas}' 1 300 >/dev/null; then
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
sample_lease_holder
if [ -z "$OLD_READY_UID" ] || [ "$OLD_HOLDER_POD_UID" != "$OLD_READY_UID" ]; then
  record FAIL "the Lease holder is the serving Pod immediately before the kill" \
    "holderIdentity='${OLD_HOLDER:-<empty>}' maps to '${OLD_HOLDER_POD_UID:-<none>}', ready Pod '${OLD_READY:-<none>}' has UID '${OLD_READY_UID:-<none>}'"
  results; exit 1
fi

MEMBERS_BEFORE="$(kvha_member_snapshot "$NS" "$MEMBER_SEL")"
if [ -z "$MEMBERS_BEFORE" ]; then
  record FAIL "members exist to be observed" "no member Pod matched ${MEMBER_SEL}; the backend is Ready, so this is a defect"
  results; exit 1
fi

# --force --grace-period=0: the API object is deleted at once and the runtime SIGKILLs the holder,
# so the Lease is left held by a dead process -- exactly what a power loss or a partition looks
# like from the standby's side. The graceful path is covered above; this one exists because the
# two paths fail differently.
TRAJ_FILE="$(mktemp)"
PRE_DELETE_SAMPLE="$(kvha_members_mounted_sample "$NS" "$BACKEND")"
echo "$PRE_DELETE_SAMPLE" >>"$TRAJ_FILE"
DELETE_EPOCH="$(date +%s)"
if DELETE_OUT="$(kubectl -n "$NS" delete pod "$OLD_READY" --force --grace-period=0 --wait=false 2>&1)"; then
  record PASS "the serving Pod deletion is accepted" "${OLD_READY} (${OLD_READY_UID}), forced with no grace period"
else
  record FAIL "the serving Pod deletion is accepted" \
    "forced delete of ${OLD_READY} (${OLD_READY_UID}) failed: $(echo "$DELETE_OUT" | tr '\n' ' ' | cut -c1-200)"
  results; exit 1
fi

observe_lease_handoff

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

wait_for_new_ready_pod
if [ -n "$NEW_READY" ] && [ "$NEW_READY" != "$OLD_READY" ]; then
  record PASS "a different replica becomes Ready" "${OLD_READY} -> ${NEW_READY}"
else
  record FAIL "a different replica becomes Ready" \
    "ready Pod is '${NEW_READY:-<none>}' 180s after the Lease moved (killed: ${OLD_READY})"
fi

MEMBERS_AFTER="$(kvha_member_snapshot "$NS" "$MEMBER_SEL")"
record_member_restarts "no member Pod restarts across the failover" "$MEMBERS_BEFORE" "$MEMBERS_AFTER"

record_member_faults "no member fault is reported throughout the transition" "$PRE_DELETE_SAMPLE"
rm -f "$TRAJ_FILE"

record_backend_ready_after_failover "the backend is Ready after the failover"

results
