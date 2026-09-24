#!/usr/bin/env bash
#
# CASE 50 — A short pool starves a P/D deployment WHOLE: the role that would have fit alone waits too
#   (MUTATING, self-recovering)
#
#   case-50.sh <NS>
#
# Goal:        CASE 49 proves every replica composes a Workload of its own. This proves what holds
#              the set together once they are independent, and it is the only claim in the family a
#              shortage can demonstrate: when the pool cannot hold every role, NOTHING starts.
#
#              THE MEASUREMENT IS BUILT AROUND THE FAILING SHAPE, NOT AROUND THE PASSING ONE. If each
#              role were its own Workload -- which is what the replicas were before they became a pod
#              group -- a shortage would admit the roles it can afford and queue the rest. For
#              prefill/decode that is worse than waiting: prefill takes the accelerators, serves
#              nothing without a decode to hand off to, and holds the very quota decode is queued for.
#              So the pool is shorted to a width that fits ONE role and not two, and the row that
#              carries the case asserts that the affordable role is gated as well.
#
#              THAT WIDTH IS MEASURED, NOT COMPUTED. The pool's own numbers do not say how many replicas
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
#              case SKIPS, printing the bound, when the pool's quota could hold more than that. EXITS 2
#              (input required) when the cluster has no InstanceType.
#
# Inputs:      All real, nothing mocked. Three ModelDeployments: a probe sized past every flavor's
#              quota, a filler sized from what Kueue reserved for the probe, and the two-role subject
#              under test. Each role carries an explicit `template.image` because a CPU-only
#              InstanceType has observed no accelerator and the operator can synthesize no engine
#              image; nothing asserted here reads what they run. Override with E2E_MD_IMAGE, the
#              InstanceType with E2E_MD_INSTANCE_TYPE.
#
# Expected:    With the quota free the two-role subject is admitted (the baseline). With the pool
#              shorted to one replica, exactly one of the subject's roles reserves quota, every one of
#              its replicas is still gated -- including the role that reserved -- and the deployment's
#              status names the waiting role and not the holding one. Releasing the filler admits the
#              whole group.
#
# Cleanup:     A trap deletes all three deployments and releases any Workload still holding their
#              replicas. Idempotent, runs on pass AND fail, safe to re-run. It creates no
#              cluster-scoped object, changes no ClusterQueue and touches no baseline: the shortage is
#              made by occupying the quota, never by editing it.
#
set -o pipefail

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

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_quota-lib.sh"

# The first InstanceType A DEPLOYMENT CAN ACTUALLY NAME, which is not the same as the first one the
# API returns.
#
# THE LIST COMES BACK SORTED BY NAME AND CARRIES TYPES ON THEIR WAY OUT. Case 68 creates its own
# `case68-held` and deletes it without waiting, and that name sorts before an ordinary derived
# type -- so a case running straight after it picks a type that is already terminating. Naming one
# is refused at admission, and the run then dies at fixture time for a reason that has nothing to do
# with what it measures. Inactive is excluded for the mirror reason: a deployment on one is admitted
# and then never scheduled, so the case waits out every timeout it has.
usable_instance_type() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        echo "$name"
        break
      done
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
CQ="$(kubectl -n "$NS" get localqueue \
  "$(kubectl get instancetypes.worker.gpustack.ai "$IT" \
    -o jsonpath='{.status.entrance}' 2>/dev/null)" \
  -o jsonpath='{.spec.clusterQueue}' 2>/dev/null)"
if [ -z "$CQ" ]; then
  echo "[case-50] InstanceType ${IT} names no reachable ClusterQueue in ${NS}; run case-1 first, and" >&2
  echo "          check that ${NS} carries the pool's entrance LocalQueue" >&2
  exit 2
fi

# One list call, for the same reason workload_verdicts takes one: this runs from the EXIT trap on
# every invocation of the case, pass or fail.
force_release() {
  local md="$1" row wl uids owners u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    owners="${row#*=}"
    for u in $uids; do
      case " $owners " in
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
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai \
    case50-probe case50-filler case50-subject --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  force_release case50-probe
  force_release case50-filler
  force_release case50-subject
}
trap cleanup EXIT

role_block() {
  printf '  - name: %s\n' "$1"
  [ -n "$2" ] && printf '    kind: %s\n' "$2"
  printf '    instanceType: %s\n    replicas: %s\n' "$IT" "$3"
  printf '    image: %s\n    command: ["/pause"]\n' "$IMAGE"
}

# The apply's output is KEPT, in APPLY_OUT. Discarding it turns a schema or webhook refusal -- a very
# plausible failure for this feature -- into a 90-second timeout whose FAIL row blames the wrong thing
# ("only N exist, so Kueue composes nothing") and never quotes the actual refusal.
APPLY_OUT=""
apply_md() {
  local name="$1" roles="$2"
  APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
  namespace: ${NS}
spec:
  engine:
    name: vllm
    version: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
${roles}
YAML
)"
}

# Every Workload owning one of this deployment's Pods, one "name=status=role" line each: status is its
# QuotaReserved condition, "" while Kueue has not judged it yet, and role is the component of the Pod
# it owns. A group Workload carries plain owner references and no controller reference, so ownership
# is what identifies it.
#
# TWO API CALLS PER INVOCATION, NOT ONE PER WORKLOAD, and here that is correctness rather than
# tidiness. This is the hot path: settle_verdicts calls it every five seconds for up to five minutes,
# over a probe that can hold a hundred replicas. At one get per Workload the poll budget goes on
# round-trips and the case reports a pool Kueue never judged -- a verdict naming the operator for
# what is this script's own latency. Names, verdicts and owner uids come back together and the match
# happens locally.
workload_verdicts() {
  local md="$1" pods row wl rest status owners p
  pods="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}={.metadata.labels.app\.kubernetes\.io/component}{"\n"}{end}' \
    2>/dev/null)"
  [ -n "$pods" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    rest="${row#*=}"
    status="${rest%%=*}"
    owners="${rest#*=}"
    for p in $pods; do
      # Padded on both sides so a uid cannot match a longer one it is a prefix of.
      case " $owners " in *" ${p%%=*} "*) echo "${wl}=${status}=${p#*=}"; break ;; esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="QuotaReserved")].status}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
}

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
    cur="$(workload_verdicts "$md")"
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
# cannot answer a question about the set.
deployment_workloads() {
  local md="$1" uids row wl owners u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    owners="${row#*=}"
    for u in $uids; do
      # Padded on both sides so a uid cannot match a longer one it is a prefix of.
      case " $owners " in *" $u "*) echo "$wl"; break ;; esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
}

# Every PodSet this deployment holds an assignment for, across ALL of its Workloads, "" when it holds
# no admission anywhere. Counting words over the set is how "both roles admitted" is read now: each
# Workload contributes the one PodSet of the replica it answers for, so a two-role deployment of one
# replica each yields two words. Reading a single Workload yields one and looks exactly like a role
# that was starved.
assigned_sets() {
  local wl
  for wl in $(deployment_workloads "$1"); do
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

# --- row 0: the baseline, on a free pool ---

apply_md case50-subject "$(role_block prefill prefill 1)
$(role_block decode decode 1)"

BASE="$(wait_admitted case50-subject 2)"
if [ "$(echo "$BASE" | wc -w | tr -d ' ')" = 2 ]; then
  record PASS "with the pool free, the two-role group is admitted" \
    "podSetAssignments: ${BASE}"
else
  record FAIL "with the pool free, the two-role group is admitted" \
    "assignments: '${BASE}' - the baseline did not admit, so every row below would starve for the wrong reason. apply said: ${APPLY_OUT:0:200}"
fi

# What Kueue charged ONE replica, read off a baseline Workload while it still holds its reservation.
# It is the unit the pool's quota is written in, which the Pod's own requests are not on an
# accelerated pool, and it is what the request costs rather than the InstanceType's unit verbatim.
CHARGE="$(workload_charge "$NS" \
  "$(workload_verdicts case50-subject | sed -n 's/=True=.*$//p' | head -n 1)")"

# Deletes a deployment and waits for its Pods to go, then deletes any Workload that outlived them.
# The Workloads are named BEFORE the delete: force_release finds them through the Pods, so once the
# Pods are gone it finds nothing, and a Workload still holding quota then shrinks the pool the next
# step measures.
release_deployment() {
  local md="$1" wls wl
  wls="$(workload_verdicts "$md" | cut -d= -f1)"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$md" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 60); do
    [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
      --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break
    sleep 2
  done
  force_release "$md"
  for wl in $wls; do
    kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
      --ignore-not-found --wait=false >/dev/null 2>&1
  done

  return 0
}

release_deployment case50-subject

# --- bound the probe by every flavor's quota ---

# The bound only has to be at least the capacity: the probe it sizes is what measures the pool, and
# a bound that is too high costs Pods, not correctness. See _quota-lib.sh for why every flavor.
PROBE_N=0
IFS='|' read -r BOUND FLAVORS BOUND_DETAIL <<<"$(pool_replica_bound "$CQ" "$CHARGE")"
BOUND="${BOUND:-0}"
if [ "$BOUND" -le 0 ]; then
  record SKIP "the pool's capacity could be measured" \
    "could not bound a probe from cluster queue '${CQ}': ${BOUND_DETAIL:-the queue could not be read}; one replica is charged ${CHARGE:-nothing readable}"
elif [ $((BOUND + 1)) -gt "$MAX_REPLICAS" ]; then
  record SKIP "the pool's capacity could be measured" \
    "cluster queue '${CQ}' could hold ${BOUND} replicas (${BOUND_DETAIL}), so a probe past it exceeds E2E_C50_MAX_REPLICAS=${MAX_REPLICAS}"
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
  PRE_RAW="$(kubectl get clusterqueue "$CQ" \
    -o jsonpath='{.apiVersion}|{.spec.cohort}{.spec.cohortName}' 2>/dev/null)"
  PRE_VER="${PRE_RAW%%|*}"
  PRE_COHORT="${PRE_RAW#*|}"

  if [ -z "$PRE_RAW" ]; then
    record SKIP "the shortage cannot be relieved by borrowing" \
      "could not read cluster queue '${CQ}'"
    PROBE_N=0
  elif [ "$PRE_VER" != "kueue.x-k8s.io/v1beta1" ] && [ "$PRE_VER" != "kueue.x-k8s.io/v1beta2" ]; then
    record SKIP "the shortage cannot be relieved by borrowing" \
      "cluster queue '${CQ}' is served as '${PRE_VER}', whose cohort field this case does not know how to read"
    PROBE_N=0
  elif [ -n "$PRE_COHORT" ]; then
    record SKIP "the shortage cannot be relieved by borrowing" \
      "cluster queue '${CQ}' is in cohort '${PRE_COHORT}', so occupying it does not make the pool short"
    PROBE_N=0
  else
    record PASS "the shortage cannot be relieved by borrowing" \
      "cluster queue '${CQ}' (${PRE_VER}) is in no cohort; its ${FLAVORS} flavor(s) are measured together below"
  fi
fi

# --- measure the pool: how many replicas does Kueue reserve? ---

# THE CAPACITY IS KUEUE'S ANSWER, NOT THIS SCRIPT'S ARITHMETIC. The bound above ignores what other
# workloads hold, a flavor the replica cannot match, and the node room topology-aware scheduling
# checks before it reserves -- each of them admits fewer. A probe one past the bound must leave at
# least one replica waiting, and the number reserved when it does is the capacity every step below
# depends on.
FILL=0
if [ "$PROBE_N" -ge 1 ]; then
  apply_md case50-probe "$(role_block bulk '' "$PROBE_N")"
  PROBED="$(settle_verdicts case50-probe "$PROBE_N")"
  PROBE_RC=$?
  CAP="${PROBED%% *}"
  PROBE_WAIT="${PROBED##* }"

  if [ "$PROBE_RC" -ne 0 ]; then
    record FAIL "the pool's capacity could be measured" \
      "Kueue did not settle a verdict on all ${PROBE_N} probe replicas: ${CAP} reserved, ${PROBE_WAIT} waiting. apply said: ${APPLY_OUT:0:200}"
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

  release_deployment case50-probe
fi

if [ "$FILL" -ge 1 ]; then
  apply_md case50-filler "$(role_block bulk '' "$FILL")"

  FILLED="$(wait_admitted case50-filler "$FILL")"
  if [ "$(echo "$FILLED" | wc -w | tr -d ' ')" -lt "$FILL" ]; then
    record FAIL "the filler occupies the pool" \
      "$(echo "$FILLED" | wc -w | tr -d ' ') of ${FILL} filler replicas reserved quota where the probe reserved ${CAP}, so the pool is not one replica short and nothing below is under test. apply said: ${APPLY_OUT:0:200}"
    FILL=0
  else
    record PASS "the filler occupies the pool" \
      "${FILL} replica(s) hold all but one replica of the ${CAP} the pool reserves"
  fi
fi

# --- the headline: nothing starts, including the role that would have fit ---

if [ "$FILL" -ge 1 ]; then
  apply_md case50-subject "$(role_block prefill prefill 1)
$(role_block decode decode 1)"

  # THE WIDTH IS READ OFF THE SUBJECT, not trusted from the probe. Each replica reserves on its own,
  # so a one-replica window shows as exactly one role holding quota and the other waiting for it. No
  # role holding means the room closed before the subject arrived, and every row below would then
  # pass with atomicity doing nothing -- the shape this case exists to rule out. Both holding means
  # the pool was not short at all. Neither is a verdict on the operator, and both stop the rows below.
  SUBJ="$(settle_verdicts case50-subject 2)"
  SUBJ_RC=$?
  VERDICTS="$(workload_verdicts case50-subject)"
  HOLDING="$(printf '%s\n' "$VERDICTS" | sed -n 's/^[^=]*=True=//p' | sort -u | tr '\n' ' ')"
  WAITING="$(printf '%s\n' "$VERDICTS" | sed -n 's/^[^=]*=False=//p' | sort -u | tr '\n' ' ')"
  WIDE=no
  if [ "$SUBJ_RC" -ne 0 ]; then
    record FAIL "the short pool reserves quota for exactly one role" \
      "Kueue did not settle a verdict on both of the subject's replicas: '${VERDICTS}'. apply said: ${APPLY_OUT:0:200}"
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
      -l "app.kubernetes.io/instance=case50-subject,app.kubernetes.io/component=${role}" \
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
    QR="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case50-subject \
      -o jsonpath='{range .status.conditions[?(@.type=="QuotaReserved")]}{.status}|{.reason}|{.message}{end}' 2>/dev/null)"
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

if [ "$FILL" -ge 1 ]; then

  # --- releasing the pool admits the WHOLE group ---

  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai case50-filler \
    --ignore-not-found --wait=false >/dev/null 2>&1
  force_release case50-filler

  AFTER="$(wait_admitted case50-subject 2)"
  if [ "$(echo "$AFTER" | wc -w | tr -d ' ')" = 2 ]; then
    record PASS "releasing the pool admits both roles at once" \
      "podSetAssignments: ${AFTER}"
  else
    record FAIL "releasing the pool admits both roles at once" \
      "assignments: '${AFTER}' - the group was starved by something other than the shortage"
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-50] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-50] all checks passed"
