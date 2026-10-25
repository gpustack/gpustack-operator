#!/usr/bin/env bash
#
# CASE 50 — A short pool starves a P/D deployment whole, a change on the waiting replica reaches the
#   deployment, and the same holds on an administrator-authored InstanceType
#   (MUTATING, self-recovering)
#
#   case-50.sh <NS>
#
# Goal:        Three claims about a pool too small for the whole set, told on one shortage.
#
#              THE FIRST IS THE BARRIER. When the pool cannot hold every role, NOTHING starts: not
#              even the role that would have fit alone. For prefill/decode that is worse than
#              waiting -- prefill takes the accelerators, serves nothing without a decode to hand off
#              to, and holds the very quota decode is queued for. So the pool is shorted to a width
#              that fits ONE role and not two, and the row that carries the claim asserts that the
#              affordable role is gated as well.
#
#              THE SECOND IS THE WATCH. The deployment controller watches its replicas' Kueue
#              Workloads, so a verdict Kueue writes on a Workload wakes the deployment even while
#              nothing touches its Pods. Kueue does not touch a waiting replica's Pods while it
#              decides -- they sit gated from creation until admission -- so without that watch the
#              deployment's QuotaReserved condition goes on quoting a Workload message that has since
#              changed, until some unrelated Pod event happens to wake it. The shortage leaves a
#              replica waiting, which is exactly the replica the watch is measured on: the case
#              writes a marker of its own into that waiting Workload's QuotaReserved message
#              through the status subresource, touches no Pod, and reads the deployment's condition
#              for a bounded window. A Pod event then wakes the deployment the other way and the
#              status has to quote the marker there too -- the control that tells a miss in the
#              window from a status that quotes nothing at all.
#
#              THE THIRD IS THE MODE. With instance-type-derived-from-node off, the administrator
#              authors every InstanceType, and the queues used to reference no AdmissionCheck at
#              all: every replica of every role is its own Workload, so a short pool admitted the
#              role that fit and left the other waiting. The joint-admission check is referenced from
#              every operator-owned queue whatever the setting says. So the case creates a type of
#              its own while the setting is off, reads the queue born in that mode, and measures the
#              same shortage on it.
#
#              THE WIDTH IS MEASURED, NOT COMPUTED. The pool's own numbers do not say how many replicas
#              it takes: quota is per flavor and a replica can land on any flavor of the queue, and
#              topology-aware scheduling reserves nothing a node has no room for. So the case asks
#              Kueue. A probe of more replicas than every flavor's quota summed could hold is applied,
#              the number it reserves is the pool's capacity, and the filler takes all but one replica
#              of it. The width is then read again on the subject itself: exactly one of its two roles
#              must reserve quota. A shortage deep enough to starve both roles would make the headline
#              row pass with atomicity doing nothing, so that reading SKIPS with the numbers printed. A
#              row that cannot fail is worth less than a row that says it did not run.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), a Kueue whose pod
#              integration is enabled, and an operator image carrying the multi-role ModelDeployment.
#              The namespace must carry the pool's entrance LocalQueue. The pool may have any number
#              of flavors; its queue must be in no cohort, because borrowing makes its capacity another
#              queue's to change.
#
#              NO GPU is needed. Admission is decided on the Workload before any container starts, and
#              every reading here is taken from the Workloads and from the replicas' scheduling gates.
#              The probe holds up to E2E_C50_MAX_REPLICAS (default 128) placeholder replicas, and the
#              case SKIPS, printing the bound, when the pool's quota could hold more than that. The
#              authored InstanceType is built on the working type's own general group, OS and arch,
#              and its queue has to fill before anything below can reserve quota. EXITS 2 (input
#              required) when the cluster has no usable InstanceType, when that InstanceType names no
#              reachable ClusterQueue, or when it names no general group, OS and arch to author a type
#              on.
#
# Inputs:      All real, nothing mocked. ModelDeployments: a probe sized past every flavor's quota, a
#              filler sized from what Kueue reserved for the probe, and the two-role subject under
#              test -- once on the working InstanceType and once on the authored one. Each role
#              carries an explicit `image` because the replicas are placeholders here and nothing
#              asserted reads what they run. Override with E2E_MD_IMAGE, the InstanceType with
#              E2E_MD_INSTANCE_TYPE. The authored InstanceType is a real object, created while the
#              instance-type-derived-from-node key of the gpustack-settings Secret in E2E_SYSTEM_NS
#              (default gpustack-system) is false, and the marker on the waiting Workload is written
#              through the status subresource of a real Workload.
#
# Expected:    With the quota free the two-role subject is admitted (the baseline). With the pool
#              shorted to one replica, exactly one of the subject's roles reserves quota, every one of
#              its replicas is still gated -- including the role that reserved -- the reserved role's
#              Workload carries the joint check Pending and naming the waiting role, and the
#              deployment's status names the waiting role and not the holding one. A change written on
#              the waiting replica's Workload alone reaches the deployment's condition within the
#              window while no Pod of the deployment moves, and a Pod event reaches it as well.
#              Releasing the filler admits the whole group. The authored type's queue is filled and
#              references the joint check, and the shortage on the authored type behaves as it does on
#              the derived one.
#
# Cleanup:     A trap deletes every deployment the case created and releases any Workload still
#              holding their replicas, deletes the InstanceType it authored, and puts the
#              instance-type-derived-from-node key of the gpustack-settings Secret back to the value
#              it had. Idempotent, runs on pass AND fail, safe to re-run. It creates no
#              cluster-scoped object, changes no ClusterQueue and touches no other baseline: the
#              shortage is made by occupying the quota, never by editing it.
#
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-50.sh <NS>" >&2
  exit 2
fi

BINDING="${E2E_MD_BINDING:-case50-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
MAX_REPLICAS="${E2E_C50_MAX_REPLICAS:-128}"
GATE="kueue.x-k8s.io/admission"
WINDOW="${POOL_WINDOW:-30}"
# shellcheck disable=SC2034 # Read by the sourced library, which shellcheck does not follow.
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING=instance-type-derived-from-node
AUTHORED=case50-authored
JOINT=gpustack-model-deployment-joint
SETTLE="${E2E_MD_SETTLE:-120}"

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_quota-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

# The first InstanceType A DEPLOYMENT CAN ACTUALLY NAME, which is not the same as the first one the
# API returns.
#
# THE LIST COMES BACK SORTED BY NAME AND CARRIES TYPES ON THEIR WAY OUT. A case that creates an
# InstanceType of its own and deletes it without waiting puts that name before an ordinary derived
# type -- so a case running straight after it picks a type that is already terminating. Naming one is
# refused at admission, and the run then dies at fixture time for a reason that has nothing to do
# with what it measures. Inactive is excluded for the mirror reason: a deployment on one is admitted
# and then never scheduled, so the case waits out every timeout it has.
#
# The type this case authors is excluded too, and so is the one the retired sibling case authored:
# a leftover of either would be picked here and then measured as if it were a working type.
usable_instance_type() {
  md_usable_instance_type "$AUTHORED" case106-authored
}

if [ -z "$IT" ]; then
  IT="$(usable_instance_type)"
fi
if [ -z "$IT" ]; then
  echo "[case-50] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# THE QUEUE THIS DEPLOYMENT COMPETES IN, not the cluster's first. On a multi-pool cluster -- or
# whenever E2E_MD_INSTANCE_TYPE selects something other than items[0] -- sizing the shortage from an
# unrelated pool's quota produces a confident wrong number, and the headline row then passes or fails
# for a reason that has nothing to do with the deployment under test.
#
# The InstanceType's entrance LocalQueue names it, which is the same indirection the operator uses.
CQ="$(md_cq_of_it "$NS" "$IT")"
if [ -z "$CQ" ]; then
  echo "[case-50] InstanceType ${IT} names no reachable ClusterQueue in ${NS}; run case-1 first, and" >&2
  echo "          check that ${NS} carries the pool's entrance LocalQueue" >&2
  exit 2
fi

# The pool the authored type is built on: its own general group, OS and arch, read off the working
# type. A type on a group nobody serves has no queue and no replica, so the mode below would measure
# nothing.
GROUP=""
OS=""
ARCH=""
read -r GROUP OS ARCH <<<"$(kubectl get instancetypes.worker.gpustack.ai "$IT" \
  -o jsonpath='{.spec.generalGroup} {.spec.os} {.spec.arch}' 2>/dev/null)"
if [ -z "$GROUP" ] || [ -z "$OS" ] || [ -z "$ARCH" ]; then
  echo "[case-50] InstanceType ${IT} names no general group, OS and arch to author a type on" >&2
  exit 2
fi

# The Setting is read through the worker's thirty-second cache, so every change waits it out. Its
# value is remembered BEFORE the trap is armed, because the trap is the only thing that puts it back.
settings_save "$SETTING"

# One list call, for the same reason md_workload_verdicts takes one: this runs from the EXIT trap on
# every invocation of the case, pass or fail. md_force_release deletes every Workload owning one of
# the deployment's Pods, which is what releases Kueue's finalizer on them.

# The Setting, turned off and put back. It is read through the worker's thirty-second cache, so every
# change waits it out.
set_derived() { # true|false
  setting_set "$SETTING" "$1"
  settings_settle
}

# Every object this case put on the cluster, and the Setting it turned. The leftovers of the retired
# sibling cases are deleted here too: they are orphans of this same scope, and one of them holds
# quota on the very pool the shortage is measured against.
cleanup() {
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai \
    case50-probe case50-filler case50-subject \
    case50a-probe case50a-filler case50a-subject \
    case95-subject --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  md_force_release "$NS" case50-probe
  md_force_release "$NS" case50-filler
  md_force_release "$NS" case50-subject
  md_force_release "$NS" case50a-probe
  md_force_release "$NS" case50a-filler
  md_force_release "$NS" case50a-subject
  md_force_release "$NS" case95-subject
  kubectl delete instancetypes.worker.gpustack.ai "$AUTHORED" case106-authored \
    --ignore-not-found --wait=false >/dev/null 2>&1
  # The Setting goes back the way the case found it. The wait for the worker's read cache is spent
  # only when the case actually changed it.
  settings_restore
}
trap cleanup EXIT

# The apply's output is KEPT, in MD_APPLY_OUT. Discarding it turns a schema or webhook refusal -- a
# very plausible failure for this feature -- into a 90-second timeout whose FAIL row blames the wrong
# thing ("only N exist, so Kueue composes nothing") and never quotes the actual refusal.
# md_apply writes it there; md_role_block builds the roles it carries.

# Every Workload owning one of this deployment's Pods, one "name=status=role" line each: status is its
# QuotaReserved condition, "" while Kueue has not judged it yet, and role is the component of the Pod
# it owns. A group Workload carries plain owner references and no controller reference, so ownership
# is what identifies it. md_workload_verdicts in _md-lib.sh reads it.

# "RESERVED WAITING" for a deployment of WANT replicas, once every replica has a Workload and the
# reservation count has stopped moving: all of them reserved, or Kueue has turned at least one away
# and two readings five seconds apart agree. On a timeout it prints the last reading and returns 1.
#
# WAITING IS WHAT DID NOT RESERVE, NOT WHAT KUEUE JUDGED SHORT. Once one replica is turned away Kueue
# stops trying the identical ones behind it, and those carry no QuotaReserved condition at all -- a
# condition count never reaches WANT, and a case that waits for it times out on every full pool.
#
# ONE READING IS NOT YET THE ANSWER. Kueue works through a long queue one head at a time, so the
# first replica it turns away can be judged while a later pass is still reserving for others.
settle_verdicts() {
  local md="$1" want="$2" prev="" cur n r=0 w=0
  for _ in $(seq 1 60); do
    cur="$(md_workload_verdicts "$NS" "$md")"
    n="$(printf '%s' "$cur" | grep -c . || true)"
    r="$(printf '%s\n' "$cur" | grep -c '^[^=]*=True=' || true)"
    w="$(printf '%s\n' "$cur" | grep -c '^[^=]*=False=' || true)"
    if [ "$n" -eq "$want" ] && [ "$r" -eq "$want" ]; then
      echo "$r 0"
      return 0
    fi
    if [ "$n" -eq "$want" ] && [ "$w" -ge 1 ]; then
      [ "$prev" = "$r" ] && { echo "$r $((want - r))"; return 0; }
      prev="$r"
    else
      prev=""
    fi
    sleep 5
  done
  echo "$r $((want - r))"

  return 1
}

# Every Workload of this deployment, one name per line. THE PLURAL IS THE WHOLE CORRECTION: a
# replica is the admission unit, so a two-role deployment of one replica each has TWO Workloads of
# one PodSet each, where it once had ONE Workload of two PodSets. A reader that stops at the first
# match stays right where any Workload will do -- asking whether one was composed at all -- but it
# cannot answer a question about the set. md_workloads in _md-lib.sh reads it.

# Every PodSet this deployment holds an assignment for, across ALL of its Workloads, "" when it holds
# no admission anywhere. Counting words over the set is how "both roles admitted" is read now: each
# Workload contributes the one PodSet of the replica it answers for, so a two-role deployment of one
# replica each yields two words. Reading a single Workload yields one and looks exactly like a role
# that was starved.
assigned_sets() {
  local wl
  for wl in $(md_workloads "$NS" "$1"); do
    kubectl -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
      -o jsonpath='{range .status.admission.podSetAssignments[*]}{.name}{" "}{end}' 2>/dev/null
  done
}

wait_admitted() {
  local md="$1" want="$2" i got
  for i in $(seq 1 40); do
    got="$(assigned_sets "$md")"
    [ "$(echo "$got" | wc -w | tr -d ' ')" -ge "$want" ] && { echo "$got"; return 0; }
    sleep 3
  done
  echo "$(assigned_sets "$md")"

  return 1
}

# Deletes a deployment and waits for its Pods to go, then deletes any Workload that outlived them.
# The Workloads are named BEFORE the delete: md_force_release finds them through the Pods, so once the
# Pods are gone it finds nothing, and a Workload still holding quota then shrinks the pool the next
# step measures.
release_deployment() {
  local md="$1" wls wl
  wls="$(md_workload_verdicts "$NS" "$md" | cut -d= -f1)"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$md" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 60); do
    [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
      --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break
    sleep 2
  done
  md_force_release "$NS" "$md"
  for wl in $wls; do
    kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
      --ignore-not-found --wait=false >/dev/null 2>&1
  done

  return 0
}

# The deployment's QuotaReserved condition as "status|reason|message".
md_quota() {
  kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$1" \
    -o jsonpath='{range .status.conditions[?(@.type=="QuotaReserved")]}{.status}|{.reason}|{.message}{end}' 2>/dev/null
}

# A Workload's QuotaReserved condition as "status|message". The status is read beside the message
# because a replica judged short can still be reserved later -- Kueue works through a long queue a
# head at a time, and on a pool of several flavors a later pass finds room on another one -- and the
# status patch below replaces the message of whatever condition is there. A marker read back from a
# reserved Workload is one the deployment is right not to quote.
wl_reading() {
  kubectl -n "$NS" get workloads.kueue.x-k8s.io "$1" \
    -o jsonpath='{range .status.conditions[?(@.type=="QuotaReserved")]}{.status}|{.message}{end}' 2>/dev/null
}

# TRUE when the Workload still waits for quota, and still carries the given marker when one is named.
wl_waits() { # wl_waits <workload> [marker]
  case "$(wl_reading "$1")" in
    False\|*"$2"*) return 0 ;;
    *) return 1 ;;
  esac
}

# TRUE when the deployment quotes the Workload by name. The deployment names every waiting Workload
# it saw, so this is what makes the waiting replica the one the status talks about.
md_quotes() { # md_quotes <deployment> <workload>
  case "$(md_quota "$1")" in
    *"\"$2\""*) return 0 ;;
    *) return 1 ;;
  esac
}

pods_rv() {
  kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${1}" \
    -o jsonpath='{range .items[*]}{.metadata.name}:{.metadata.resourceVersion} {end}' 2>/dev/null
}

wait_for() { # wait_for <seconds> <predicate...>
  local deadline=$((SECONDS + $1))
  shift
  while [ $SECONDS -lt "$deadline" ]; do
    "$@" && return 0
    sleep 5
  done

  return 1
}

# ------------------------------------------------------------------------------------------------
# THE SHORTAGE, ON ONE INSTANCE TYPE.
#
# <it> is the type the deployment names, <cq> the queue it competes in, and <prefix> the name the
# three deployments carry -- one prefix per run, so the two runs never read each other's Pods.
#
# IT LEAVES THE POOL SHORTED: the filler still holds the quota and one role of the subject waits for
# it. The release is a separate call, because the watch is measured in between.
# ------------------------------------------------------------------------------------------------
run_shortage_suite() {
  local it="$1" cq="$2" prefix="$3"
  local probe="${prefix}-probe" filler="${prefix}-filler" subject="${prefix}-subject"

  # Everything the run reads is reset here, so the second run cannot read the first one's numbers.
  PROBE_N=0
  FILL=0
  WIDE=no
  BASE=""
  CAP=""
  FILLED=""
  SUBJ=""
  VERDICTS=""
  HOLDING=""
  WAITING=""
  HOLD_ROLE=""
  WAIT_ROLE=""
  SUITE_SUBJECT="$subject"
  SUITE_WAIT_WL=""

  # --- row 0: the baseline, on a free pool ---

  md_apply "$subject" "$NS" "$BINDING" "$(md_role_block prefill Prefill "$it" 1 "$IMAGE")
$(md_role_block decode Decode "$it" 1 "$IMAGE")" vLLM "$IMAGE"

  BASE="$(wait_admitted "$subject" 2)"
  if [ "$(echo "$BASE" | wc -w | tr -d ' ')" = 2 ]; then
    record PASS "with the pool free, the two-role group is admitted" \
      "podSetAssignments: ${BASE}"
  else
    record FAIL "with the pool free, the two-role group is admitted" \
      "assignments: '${BASE}' - the baseline did not admit, so every row below would starve for the wrong reason. apply said: ${MD_APPLY_OUT:0:200}"
  fi

  # What Kueue charged ONE replica, read off a baseline Workload while it still holds its reservation.
  # It is the unit the pool's quota is written in, which the Pod's own requests are not on an
  # accelerated pool, and it is what the request costs rather than the InstanceType's unit verbatim.
  CHARGE="$(workload_charge "$NS" \
    "$(md_workload_verdicts "$NS" "$subject" | sed -n 's/=True=.*$//p' | head -n 1)")"

  release_deployment "$subject"

  # --- bound the probe by every flavor's quota ---

  # The bound only has to be at least the capacity: the probe it sizes is what measures the pool, and
  # a bound that is too high costs Pods, not correctness. See _quota-lib.sh for why every flavor.
  BOUND=0
  FLAVORS=""
  BOUND_DETAIL=""
  IFS='|' read -r BOUND FLAVORS BOUND_DETAIL <<<"$(pool_replica_bound "$cq" "$CHARGE")"
  BOUND="${BOUND:-0}"
  if [ "$BOUND" -le 0 ]; then
    record SKIP "the pool's capacity could be measured" \
      "could not bound a probe from cluster queue '${cq}': ${BOUND_DETAIL:-the queue could not be read}; one replica is charged ${CHARGE:-nothing readable}"
  elif [ $((BOUND + 1)) -gt "$MAX_REPLICAS" ]; then
    record SKIP "the pool's capacity could be measured" \
      "cluster queue '${cq}' could hold ${BOUND} replicas (${BOUND_DETAIL}), so a probe past it exceeds E2E_C50_MAX_REPLICAS=${MAX_REPLICAS}"
  else
    PROBE_N=$((BOUND + 1))
  fi

  # Borrowing is the one thing that can still break the experiment: a queue in a cohort draws on
  # another queue's unused quota, so what the probe measures is that queue's to change, and the pool
  # the filler shorts can be relieved while the subject waits. A second flavor no longer can -- the
  # probe fills every flavor, and quota per flavor is exactly what it measures.
  #
  # CHECKED BEFORE THE PROBE IS APPLIED: finding out afterwards means occupying the pool for an
  # experiment that is then abandoned.
  if [ "$PROBE_N" -ge 1 ]; then
    # BOTH COHORT SPELLINGS ARE READ. The bundled CRD serves v1beta1 and v1beta2, and the field is
    # `spec.cohort` in the first and `spec.cohortName` in the second. `kubectl get` returns whichever
    # version is served, so asking for one spelling alone yields "" on the other -- indistinguishable
    # from a queue that genuinely has no cohort, which would record PASS exactly when borrowing IS
    # possible. Only one of the two can be set, so concatenating them is the value.
    #
    # The apiVersion is read beside them so the pair is falsifiable: a third spelling in some later
    # version would otherwise read as "no cohort" here forever, which is the same silent failure one
    # layer up.
    PRE_RAW="$(kubectl get clusterqueue "$cq" \
      -o jsonpath='{.apiVersion}|{.spec.cohort}{.spec.cohortName}' 2>/dev/null)"
    PRE_VER="${PRE_RAW%%|*}"
    PRE_COHORT="${PRE_RAW#*|}"

    if [ -z "$PRE_RAW" ]; then
      record SKIP "the shortage cannot be relieved by borrowing" \
        "could not read cluster queue '${cq}'"
      PROBE_N=0
    elif [ "$PRE_VER" != "kueue.x-k8s.io/v1beta1" ] && [ "$PRE_VER" != "kueue.x-k8s.io/v1beta2" ]; then
      record SKIP "the shortage cannot be relieved by borrowing" \
        "cluster queue '${cq}' is served as '${PRE_VER}', whose cohort field this case does not know how to read"
      PROBE_N=0
    elif [ -n "$PRE_COHORT" ]; then
      record SKIP "the shortage cannot be relieved by borrowing" \
        "cluster queue '${cq}' is in cohort '${PRE_COHORT}', so occupying it does not make the pool short"
      PROBE_N=0
    else
      record PASS "the shortage cannot be relieved by borrowing" \
        "cluster queue '${cq}' (${PRE_VER}) is in no cohort; its ${FLAVORS} flavor(s) are measured together below"
    fi
  fi

  # --- measure the pool: how many replicas does Kueue reserve? ---

  # THE CAPACITY IS KUEUE'S ANSWER, NOT THIS SCRIPT'S ARITHMETIC. The bound above ignores what other
  # workloads hold, a flavor the replica cannot match, and the node room topology-aware scheduling
  # checks before it reserves -- each of them admits fewer. A probe one past the bound must leave at
  # least one replica waiting, and the number reserved when it does is the capacity every step below
  # depends on.
  if [ "$PROBE_N" -ge 1 ]; then
    md_apply "$probe" "$NS" "$BINDING" "$(md_role_block bulk '' "$it" "$PROBE_N" "$IMAGE")" vLLM "$IMAGE"
    PROBED="$(settle_verdicts "$probe" "$PROBE_N")"
    PROBE_RC=$?
    CAP="${PROBED%% *}"
    PROBE_WAIT="${PROBED##* }"

    if [ "$PROBE_RC" -ne 0 ]; then
      record FAIL "the pool's capacity could be measured" \
        "Kueue did not settle a verdict on all ${PROBE_N} probe replicas: ${CAP} reserved, ${PROBE_WAIT} waiting. apply said: ${MD_APPLY_OUT:0:200}"
    elif [ "$PROBE_WAIT" -lt 1 ]; then
      record SKIP "the pool's capacity could be measured" \
        "all ${PROBE_N} probe replicas reserved quota past a bound of ${BOUND_DETAIL}, so the capacity was not reached and one replica of room cannot be left"
    elif [ "$CAP" -lt 2 ]; then
      record SKIP "the pool's capacity could be measured" \
        "the pool reserves ${CAP} replica(s), so no filler can leave it exactly one replica short"
    else
      FILL=$((CAP - 1))
      record PASS "the pool's capacity could be measured" \
        "Kueue reserved ${CAP} of ${PROBE_N} probe replicas and left ${PROBE_WAIT} waiting; the quota bound was ${BOUND_DETAIL}"
    fi

    release_deployment "$probe"
  fi

  if [ "$FILL" -ge 1 ]; then
    md_apply "$filler" "$NS" "$BINDING" "$(md_role_block bulk '' "$it" "$FILL" "$IMAGE")" vLLM "$IMAGE"

    FILLED="$(wait_admitted "$filler" "$FILL")"
    if [ "$(echo "$FILLED" | wc -w | tr -d ' ')" -lt "$FILL" ]; then
      record FAIL "the filler occupies the pool" \
        "$(echo "$FILLED" | wc -w | tr -d ' ') of ${FILL} filler replicas reserved quota where the probe reserved ${CAP}, so the pool is not one replica short and nothing below is under test. apply said: ${MD_APPLY_OUT:0:200}"
      FILL=0
    else
      record PASS "the filler occupies the pool" \
        "${FILL} replica(s) hold all but one replica of the ${CAP} the pool reserves"
    fi
  fi

  # --- the headline: nothing starts, including the role that would have fit ---

  if [ "$FILL" -ge 1 ]; then
    md_apply "$subject" "$NS" "$BINDING" "$(md_role_block prefill Prefill "$it" 1 "$IMAGE")
$(md_role_block decode Decode "$it" 1 "$IMAGE")" vLLM "$IMAGE"

    # THE WIDTH IS READ OFF THE SUBJECT, not trusted from the probe. Each replica reserves on its own,
    # so a one-replica window shows as exactly one role holding quota and the other waiting for it. No
    # role holding means the room closed before the subject arrived, and every row below would then
    # pass with atomicity doing nothing -- the shape this case exists to rule out. Both holding means
    # the pool was not short at all. Neither is a verdict on the operator, and both stop the rows below.
    SUBJ="$(settle_verdicts "$subject" 2)"
    SUBJ_RC=$?
    VERDICTS="$(md_workload_verdicts "$NS" "$subject")"
    HOLDING="$(printf '%s\n' "$VERDICTS" | sed -n 's/^[^=]*=True=//p' | sort -u | tr '\n' ' ')"
    WAITING="$(printf '%s\n' "$VERDICTS" | sed -n 's/^[^=]*=False=//p' | sort -u | tr '\n' ' ')"
    if [ "$SUBJ_RC" -ne 0 ]; then
      record FAIL "the short pool reserves quota for exactly one role" \
        "Kueue did not settle a verdict on both of the subject's replicas: '${VERDICTS}'. apply said: ${MD_APPLY_OUT:0:200}"
    elif [ "${SUBJ%% *}" = 1 ]; then
      WIDE=yes
      record PASS "the short pool reserves quota for exactly one role" \
        "${HOLDING}holds quota and ${WAITING}waits, beside ${FILL} filler replica(s) of the ${CAP} the pool reserves"
    elif [ "${SUBJ%% *}" = 0 ]; then
      record SKIP "the short pool reserves quota for exactly one role" \
        "neither role reserved quota, so the pool had no room left for one and the rows below would pass with atomicity doing nothing"
    else
      record SKIP "the short pool reserves quota for exactly one role" \
        "both roles reserved quota, so the pool was not short: its capacity grew past the ${CAP} the probe measured"
    fi
    HOLD_ROLE="${HOLDING% }"
    WAIT_ROLE="${WAITING% }"

    # THE WAITING REPLICA, named. The shortage left one role holding quota and one waiting for it, and
    # the waiting one is the replica the watch below is measured on.
    SUITE_WAIT_WL="$(printf '%s\n' "$VERDICTS" | sed -n 's/^[^=]*=False=//p' | head -n 1)"
  fi

  if [ "${WIDE:-no}" = yes ]; then
    # THE ROW THIS FILE EXISTS FOR, and it is a stronger claim than it used to be. The Workloads ARE
    # independent now -- one per replica -- so nothing in Kueue's own atomicity keeps the role that
    # fits from starting: it has already reserved quota. What holds it is the joint-admission check this
    # operator references from the queue. Counted over the replicas of EACH role, so a reading of "some
    # gated" cannot pass for "all gated".
    UNGATED=""
    SEEN=0
    for role in prefill decode; do
      gates="$(kubectl -n "$NS" get pods \
        -l "app.kubernetes.io/instance=${subject},app.kubernetes.io/component=${role}" \
        -o jsonpath="{range .items[*]}{.metadata.name}={.spec.schedulingGates[?(@.name=='${GATE}')].name}{\"\n\"}{end}" \
        2>/dev/null)"
      seen="$(printf '%s' "$gates" | grep -c . || true)"
      SEEN=$((SEEN + seen))
      n="$(printf '%s' "$gates" | grep -c -v "=${GATE}$" || true)"
      [ "${n:-0}" -gt 0 ] && UNGATED="${UNGATED}${role}:${n} "
    done

    # NO REPLICA AT ALL PASSES THE GATE TEST FOR FREE, so the count is asserted before the verdict:
    # "none of them is ungated" is true of an empty set, and an empty set is also what a deployment that
    # never rendered anything produces.
    if [ "$SEEN" -lt 2 ]; then
      record FAIL "the role that would have fit is gated too" \
        "only ${SEEN} replica(s) exist to read a gate from, so the check had nothing to discriminate"
    elif [ -z "$UNGATED" ]; then
      record PASS "the role that would have fit is gated too" \
        "all ${SEEN} replicas across both roles still carry ${GATE}, ${HOLD_ROLE}'s while it holds quota"
    else
      record FAIL "the role that would have fit is gated too" \
        "ungated replicas by role: ${UNGATED}- this is the per-role admission the group exists to prevent"
    fi

    # WHAT HOLDS THE ROLE THAT FIT, read off its own Workload. The row above sees the gate and not who
    # keeps it: a queue that referenced no joint check would leave the gate on only until Kueue admits,
    # and the reading above can land before that. This one names the check, its Pending state, and the
    # role it is waiting for, which is the answer an operator reading the Workload gets.
    HOLD_WL="$(printf '%s\n' "$VERDICTS" | sed -n 's/=True=.*$//p' | head -n 1)"
    JC=""
    for _ in $(seq 1 10); do
      JC="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$HOLD_WL" \
        -o jsonpath='{range .status.admissionChecks[?(@.name=="gpustack-model-deployment-joint")]}{.state}|{.message}{end}' \
        2>/dev/null)"
      case "$JC" in Pending\|*"roles ${WAIT_ROLE}"*) break ;; esac
      sleep 3
    done
    case "$JC" in
      Pending\|*"roles ${WAIT_ROLE}"*)
        record PASS "the joint check holds the role that fit, naming the waiting role" \
          "${HOLD_WL}: Pending, waiting for ${WAIT_ROLE}"
        ;;
      "")
        record FAIL "the joint check holds the role that fit, naming the waiting role" \
          "${HOLD_WL} carries no gpustack-model-deployment-joint check, so its queue does not reference it"
        ;;
      *)
        record FAIL "the joint check holds the role that fit, naming the waiting role" \
          "${HOLD_WL}: expected Pending naming ${WAIT_ROLE}, got: ${JC}"
        ;;
    esac

    # THE OPERATOR'S OWN ACCOUNT OF THE SHORTAGE, which is a different subject from the two rows above.
    # Those read Kueue's Workloads and the kubelet's gates -- state Kueue owns. This reads what
    # observeModelDeploymentQuota wrote onto the ModelDeployment, and a regression that stopped
    # reporting the wait entirely would leave both rows above green.
    #
    # IT NAMES THE WAITING ROLE, AND ONLY THAT ONE. Each role names its own instanceType and so its own
    # queue, so a multi-role deployment has no single queue to name. The role holding quota is not
    # waiting for any, and a message naming it would send an operator to free quota for a replica that
    # already has it. Which role holds is Kueue's pick, so both are taken from the reading above.
    #
    # Polled rather than sampled, because the condition is written by a reconcile that follows the
    # admission decision rather than accompanying it -- and an earlier reconcile, from before either
    # role reserved, names both.
    QR=""
    QR_ROLES=""
    HAS_W=no
    HAS_H=no
    for _ in $(seq 1 10); do
      QR="$(md_quota "$subject")"
      QR_MSG="${QR#*|*|}"
      QR_ROLES="$(printf '%s' "$QR_MSG" | sed -n 's/.*the replicas of roles \([^.]*\)\..*/\1/p')"
      # Word-padded so each role is matched whole: "prefill" must not be found inside a longer name.
      QR_WORDS=" $(printf '%s' "$QR_ROLES" | tr ',' ' ' | tr -s ' ') "
      case "$QR_WORDS" in *" ${WAIT_ROLE} "*) HAS_W=yes ;; *) HAS_W=no ;; esac
      case "$QR_WORDS" in *" ${HOLD_ROLE} "*) HAS_H=yes ;; *) HAS_H=no ;; esac
      case "$QR" in False\|Pending\|*) [ "$HAS_W" = yes ] && [ "$HAS_H" = no ] && break ;; esac
      sleep 3
    done
    case "$QR" in
      False\|Pending\|*)
        if [ "$HAS_W" = yes ] && [ "$HAS_H" = no ]; then
          record PASS "the deployment reports the wait, naming the waiting role" \
            "QuotaReserved=False reason=Pending naming roles ${QR_ROLES}, not ${HOLD_ROLE} which holds quota"
        else
          record FAIL "the deployment reports the wait, naming the waiting role" \
            "QuotaReserved=False reason=Pending but the waiting roles read '${QR_ROLES}' where ${WAIT_ROLE} waits and ${HOLD_ROLE} holds: ${QR_MSG}"
        fi
        ;;
      "")
        record FAIL "the deployment reports the wait, naming the waiting role" \
          "no QuotaReserved condition was written at all while the group sat unadmitted"
        ;;
      *)
        record FAIL "the deployment reports the wait, naming the waiting role" \
          "expected False/Pending while the pool is short, got: ${QR}"
        ;;
    esac
  fi
}

# --- releasing the pool admits the WHOLE group ---
#
# The second half of the shortage, kept apart from the first so the watch is measured in between:
# the subject is still shorted while the marker is written on it, and only then is the filler let go.
finish_shortage_suite() {
  local prefix="$1"
  local filler="${prefix}-filler" subject="${prefix}-subject"

  if [ "$FILL" -ge 1 ]; then
    kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$filler" \
      --ignore-not-found --wait=false >/dev/null 2>&1
    md_force_release "$NS" "$filler"

    AFTER="$(wait_admitted "$subject" 2)"
    if [ "$(echo "$AFTER" | wc -w | tr -d ' ')" = 2 ]; then
      record PASS "releasing the pool admits both roles at once" \
        "podSetAssignments: ${AFTER}"
    else
      record FAIL "releasing the pool admits both roles at once" \
        "assignments: '${AFTER}' - the group was starved by something other than the shortage"
    fi
  fi
}

# ------------------------------------------------------------------------------------------------
# THE SHORTAGE ON THE WORKING INSTANCE TYPE.
#
# It runs first, on an unmutated cluster: the mode below turns a Setting off, and a check that reads
# a pool wants that pool as it is.
# ------------------------------------------------------------------------------------------------
echo
echo "== the shortage on ${IT} =="
run_shortage_suite "$IT" "$CQ" case50

# ------------------------------------------------------------------------------------------------
# THE WATCH, ON THE REPLICA THE SHORTAGE LEFT WAITING.
#
# There is no sizing cycle here: the shortage already produced the replica the watch is measured on,
# so the deployment that would have been sized past the quota does not exist.
# ------------------------------------------------------------------------------------------------
echo
echo "== the watch: a change on the waiting replica's Workload alone =="

WAIT_WL="$SUITE_WAIT_WL"
if [ -z "$WAIT_WL" ] || ! md_quotes "$SUITE_SUBJECT" "$WAIT_WL"; then
  record SKIP "a replica of the deployment waits for quota" \
    "no Workload of the shorted subject waited with its message quoted; md reads: $(md_quota "$SUITE_SUBJECT" | cut -c1-200)"
else
  record PASS "a replica of the deployment waits for quota" \
    "${WAIT_WL} is waiting and quoted by ${SUITE_SUBJECT}"
fi

if [ -n "$WAIT_WL" ]; then
  # QUIET FIRST. Admitted replicas start and their Pods move for a while; a Pod event inside the
  # window would wake the deployment through the Pod watch and pass for the Workload watch.
  RV0="$(pods_rv "$SUITE_SUBJECT")"
  for _ in $(seq 1 24); do
    sleep 5
    RV1="$(pods_rv "$SUITE_SUBJECT")"
    [ "$RV0" = "$RV1" ] && break
    RV0="$RV1"
  done

  # The quiet wait above is long enough for Kueue to reserve a replica it judged short, so the pick is
  # checked again right before the marker is written onto it.
  RESERVED_BEFORE=no
  wl_waits "$WAIT_WL" || RESERVED_BEFORE=yes

  MARK="case50-marker-$(date +%s)"
  IDX="$(kubectl -n "$NS" get workloads.kueue.x-k8s.io "$WAIT_WL" \
    -o jsonpath='{range .status.conditions[*]}{.type}{"\n"}{end}' 2>/dev/null \
    | awk '$0 == "QuotaReserved" {print NR - 1; exit}')"
  PATCH_OUT="$(kubectl -n "$NS" patch workloads.kueue.x-k8s.io "$WAIT_WL" --subresource=status --type=json \
    -p "[{\"op\":\"test\",\"path\":\"/status/conditions/${IDX}/type\",\"value\":\"QuotaReserved\"},{\"op\":\"replace\",\"path\":\"/status/conditions/${IDX}/message\",\"value\":\"${MARK}\"}]" 2>&1)"

  MARK_SEEN=""
  KEPT=yes
  START=$(date +%s)
  while [ $(($(date +%s) - START)) -lt "$WINDOW" ]; do
    wl_waits "$WAIT_WL" "$MARK" || KEPT=no
    if [ -z "$MARK_SEEN" ]; then
      case "$(md_quota "$SUITE_SUBJECT")" in *"$MARK"*) MARK_SEEN=$(($(date +%s) - START)) ;; esac
    fi
    sleep 2
  done
  RV2="$(pods_rv "$SUITE_SUBJECT")"

  if [ "$RESERVED_BEFORE" = yes ]; then
    record SKIP "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "Kueue reserved quota for ${WAIT_WL} while the Pods settled, so no waiting replica was left to write the marker on"
  elif [ -z "$IDX" ] || ! printf '%s' "$PATCH_OUT" | grep -q patched; then
    record FAIL "the Workload carries the case's marker" \
      "the status patch did not land (condition index '${IDX}'): ${PATCH_OUT:0:200}"
  elif [ "$KEPT" = no ]; then
    record SKIP "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "Kueue rewrote ${WAIT_WL}'s message or reserved quota for it inside the window, so a waiting marker was not there to be read throughout"
  elif [ "$RV0" != "$RV2" ]; then
    record SKIP "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "a Pod of the deployment moved inside the window, so a wake cannot be told from the Pod watch's"
  elif [ -n "$MARK_SEEN" ]; then
    record PASS "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "QuotaReserved quoted ${MARK} after ${MARK_SEEN}s; ${WAIT_WL} kept it and no Pod moved"
  else
    record FAIL "a Workload-only change reaches the deployment within ${WINDOW}s" \
      "QuotaReserved never quoted ${MARK} while ${WAIT_WL} carried it and no Pod moved: nothing woke the deployment on the Workload"
  fi

  # --- the control: a Pod event wakes it, and the status then quotes the marker ---

  # ONLY WHILE THE MARKER STILL SITS ON A WAITING REPLICA. The deployment quotes waiting Workloads
  # alone, so once Kueue has reserved the chosen one or rewritten its message there is nothing the
  # status should quote, and a miss here would blame the status for the race the headline already
  # skipped on.
  POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${SUITE_SUBJECT}" -o name 2>/dev/null | head -n 1)"
  STILL=no
  wl_waits "$WAIT_WL" "$MARK" && STILL=yes
  [ "$STILL" = yes ] && kubectl -n "$NS" annotate "$POD" case50-wake="$MARK" --overwrite >/dev/null 2>&1
  CTRL=no
  for _ in $(seq 1 10); do
    [ "$STILL" = yes ] || break
    case "$(md_quota "$SUITE_SUBJECT")" in *"$MARK"*) CTRL=yes; break ;; esac
    sleep 1
  done
  if [ "$STILL" = no ]; then
    record SKIP "after a Pod event the deployment quotes the marker" \
      "${WAIT_WL} no longer waits with ${MARK}, so there is nothing the status should quote"
  elif [ "$CTRL" = yes ]; then
    record PASS "after a Pod event the deployment quotes the marker" \
      "annotating ${POD#pod/} woke it and QuotaReserved quotes ${MARK}"
  else
    record FAIL "after a Pod event the deployment quotes the marker" \
      "the status does not quote the Workload's message at all, so the headline row measured nothing: $(md_quota "$SUITE_SUBJECT" | cut -c1-200)"
  fi
fi

finish_shortage_suite case50

# ------------------------------------------------------------------------------------------------
# THE ADMINISTRATOR-AUTHORED MODE.
#
# The Setting is turned off here and nowhere else, and the queue that is born in that mode is the one
# that has to reference the joint check. Each precondition ends this section on its own: a section
# that stops does not take the rows of another section with it.
# ------------------------------------------------------------------------------------------------
echo
echo "== the administrator-authored type =="

AUTHORED_STATE=go
ACTIVE="$(kubectl get admissionchecks.kueue.x-k8s.io "$JOINT" \
  -o jsonpath='{.status.conditions[?(@.type=="Active")].status}' 2>/dev/null)"
if [ "$ACTIVE" != True ]; then
  record FAIL "the joint check is Active" "${JOINT} Active=[${ACTIVE:-absent}]; a queue references only an Active check"
  AUTHORED_STATE=stop
else
  record PASS "the joint check is Active" "$JOINT"
fi

if [ "$AUTHORED_STATE" = go ]; then
  # THE TYPE IS CREATED AFTER THE FLIP, NOT BEFORE. Flipping the setting re-reads nothing on an existing
  # queue until its resource groups are refilled, so a queue filled before the flip says what the
  # default mode wrote and nothing about this one.
  set_derived false
  kubectl delete instancetypes.worker.gpustack.ai "$AUTHORED" --ignore-not-found --wait=true >/dev/null 2>&1
  APPLY_AUTHORED_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${AUTHORED}
spec:
  displayName: case-50 authored
  acceleratable: false
  generalGroup: ${GROUP}
  os: ${OS}
  arch: ${ARCH}
  localStorage: 1Gi
  unitResources:
    cpu: "1"
    ram: 1Gi
YAML
)"
  APPLY_AUTHORED_RC=$?
  if [ "$APPLY_AUTHORED_RC" -ne 0 ]; then
    record FAIL "the authored InstanceType is accepted" \
      "applying ${AUTHORED} was refused: ${APPLY_AUTHORED_OUT:0:200}"
    AUTHORED_STATE=stop
  fi
fi

queue_filled() {
  [ -n "$(kubectl get clusterqueue "$AUTHORED" -o jsonpath='{.spec.resourceGroups[*].flavors[*].name}' 2>/dev/null)" ]
}
queue_references_joint() {
  kubectl get clusterqueue "$AUTHORED" \
    -o jsonpath='{range .spec.admissionChecksStrategy.admissionChecks[*]}{.name}{"\n"}{end}' 2>/dev/null \
    | grep -qxF "$JOINT"
}
entrance_ready() {
  local lq
  lq="$(kubectl get instancetypes.worker.gpustack.ai "$AUTHORED" -o jsonpath='{.status.entrance}' 2>/dev/null)"
  [ -n "$lq" ] && kubectl -n "$NS" get localqueue "$lq" >/dev/null 2>&1
}

if [ "$AUTHORED_STATE" = go ]; then
  if ! wait_for "$SETTLE" queue_filled; then
    record FAIL "the authored type's queue is filled" \
      "ClusterQueue ${AUTHORED} carries no flavor after ${SETTLE}s, so nothing below can reserve quota"
    AUTHORED_STATE=stop
  else
    record PASS "the authored type's queue is filled" "$(kubectl get clusterqueue "$AUTHORED" \
      -o jsonpath='{.spec.resourceGroups[*].flavors[*].name}' 2>/dev/null)"
  fi
fi

if [ "$AUTHORED_STATE" = go ]; then
  # THE QUEUE ROW IS WHAT DISCRIMINATES THIS MODE. A queue filled while the setting is off carried no
  # AdmissionCheck at all before this change, and the check is referenced only once it is Active --
  # which the precondition above established -- so a missing reference here is the operator's.
  if wait_for 30 queue_references_joint; then
    record PASS "the authored type's queue references the joint check" "setting off, ${JOINT} referenced"
  else
    record FAIL "the authored type's queue references the joint check" \
      "references [$(kubectl get clusterqueue "$AUTHORED" \
        -o jsonpath='{.spec.admissionChecksStrategy.admissionChecks[*].name}' 2>/dev/null)] with the setting off"
  fi

  if ! wait_for "$SETTLE" entrance_ready; then
    record FAIL "the authored type's entrance LocalQueue reaches ${NS}" "no LocalQueue after ${SETTLE}s"
    AUTHORED_STATE=stop
  fi
fi

# ------------------------------------------------------------------------------------------------
# THE SAME SHORTAGE ON THE AUTHORED TYPE.
#
# A SKIP IN THE SHORTAGE EXITS 0, and a skipped shortage is exactly the run in which nothing was
# gated. So its table is read as well as its exit code, and a skipped row makes the row below a SKIP
# rather than a PASS: the claim was not measured.
# ------------------------------------------------------------------------------------------------
if [ "$AUTHORED_STATE" = go ]; then
  echo
  echo "== the same shortage on ${AUTHORED} =="
  ROWS_BEFORE="${#ROWS[@]}"
  run_shortage_suite "$AUTHORED" "$AUTHORED" case50a
  finish_shortage_suite case50a

  SUITE_FAILS=0
  SUITE_SKIPS=0
  i=0
  for r in ${ROWS[@]+"${ROWS[@]}"}; do
    if [ "$i" -ge "$ROWS_BEFORE" ]; then
      case "$r" in
        FAIL\|*) SUITE_FAILS=$((SUITE_FAILS + 1)) ;;
        SKIP\|*) SUITE_SKIPS=$((SUITE_SKIPS + 1)) ;;
      esac
    fi
    i=$((i + 1))
  done

  if [ "$SUITE_FAILS" -gt 0 ]; then
    record FAIL "case-50 passes on the authored type" \
      "the shortage on ${AUTHORED} failed ${SUITE_FAILS} row(s); see its table above"
  elif [ "$SUITE_SKIPS" -gt 0 ]; then
    record SKIP "case-50 passes on the authored type" \
      "the shortage on ${AUTHORED} skipped ${SUITE_SKIPS} row(s), so it was not measured; see its table above"
  else
    record PASS "case-50 passes on the authored type" \
      "every row of the shortage on ${AUTHORED} above passed"
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-50] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-50] all checks passed"
