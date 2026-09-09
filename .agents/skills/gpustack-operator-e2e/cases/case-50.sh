#!/usr/bin/env bash
#
# CASE 50 — A short pool starves a P/D deployment WHOLE: the role that would have fit alone waits too
#   (MUTATING, self-recovering)
#
#   case-50.sh <NS>
#
# Goal:        CASE 49 proves the roles compose into one Workload. This proves what that buys, and it
#              is the only claim in the family that a shortage can demonstrate: when the pool cannot
#              hold every role, NOTHING starts.
#
#              THE MEASUREMENT IS BUILT AROUND THE FAILING SHAPE, NOT AROUND THE PASSING ONE. If each
#              role were its own Workload -- which is what the replicas were before they became a pod
#              group -- a shortage would admit the roles it can afford and queue the rest. For
#              prefill/decode that is worse than waiting: prefill takes the accelerators, serves
#              nothing without a decode to hand off to, and holds the very quota decode is queued for.
#              So the pool is shorted to a width that fits ONE role and not two, and the row that
#              carries the case asserts that the affordable role is gated as well.
#
#              THAT WIDTH IS ASSERTED, NOT ASSUMED. A shortage deep enough to starve both roles for
#              lack of room would make the headline row pass with atomicity doing nothing, so the case
#              computes the remaining quota, checks it against one replica's request, and SKIPS with
#              both numbers printed when the window could not be created. A row that cannot fail is
#              worth less than a row that says it did not run.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), a Kueue whose pod
#              integration is enabled, and an operator image carrying the multi-role ModelDeployment.
#              The namespace must carry the pool's entrance LocalQueue.
#
#              NO GPU is needed. Admission is decided on the Workload before any container starts, and
#              every reading here is taken from the Workload and from the replicas' scheduling gates.
#              The filler's replicas are not expected to be schedulable -- a quota reservation is held
#              whether or not the node has room, which is the property that makes this runnable on one
#              small node. EXITS 2 (input required) when the cluster has no InstanceType.
#
# Inputs:      All real, nothing mocked. Two ModelDeployments: a filler sized from the pool's own
#              numbers, and the two-role subject under test. Each role carries an explicit
#              `template.image` because a CPU-only InstanceType has observed no accelerator and the
#              operator can synthesize no engine image; nothing asserted here reads what they run.
#              Override with E2E_MD_IMAGE, the InstanceType with E2E_MD_INSTANCE_TYPE.
#
# Expected:    With the quota free the two-role subject is admitted (the baseline). With the pool
#              shorted to a one-role width the subject's Workload reserves no quota AND every one of
#              its replicas is still gated, including the role that would have fit. Releasing the
#              filler admits the whole group, both PodSets in one admission.
#
# Cleanup:     A trap deletes both deployments and releases any Workload still holding their replicas.
#              Idempotent, runs on pass AND fail, safe to re-run. It creates no cluster-scoped object,
#              changes no ClusterQueue and touches no baseline: the shortage is made by occupying the
#              quota, never by editing it.
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
GATE="kueue.x-k8s.io/admission"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
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

# Millicores from a Kubernetes CPU quantity, or 0 when the value is not one this can read.
#
# NOT BASH ARITHMETIC. A CPU quantity is legally fractional -- "1.5", "0.8" -- and `$(( 1.5 * 1000 ))`
# is a syntax error, not a wrong number: the case would die mid-run rather than take its SKIP path.
#
# THE SHAPE IS MATCHED BEFORE ANYTHING IS CONVERTED, because awk's `$0+0` does not fail on a value it
# does not understand -- it takes the numeric PREFIX. "1Ki" becomes 1 and "2e3" becomes 2000, a
# confident wrong number that the caller's `-le 0` guard cannot see, and a wrong number here silently
# mis-sizes the shortage the whole case is built on. Only a plain decimal, with an optional `m`, is
# accepted; everything else returns 0, which the caller already reads as "could not read the numbers".
millis() {
  case "$1" in
    *[!0-9.m]* | "" | m) echo 0 ;;
    *m) printf '%s' "${1%m}" | awk '{printf "%d", $0+0}' ;;
    *) printf '%s' "$1" | awk '{printf "%d", ($0+0)*1000}' ;;
  esac
}

# One list call, for the same reason group_workload takes one: this runs from the EXIT trap on every
# invocation of the case, pass or fail.
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
    case50-filler case50-subject --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  force_release case50-filler
  force_release case50-subject
}
trap cleanup EXIT

role_block() {
  printf '  - name: %s\n' "$1"
  [ -n "$2" ] && printf '    kind: %s\n' "$2"
  printf '    instanceType: %s\n    replicas: %s\n' "$IT" "$3"
  printf '    template:\n      image: %s\n      command: ["/pause"]\n' "$IMAGE"
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
  engine: vllm
  engineVersion: "0.11.0"
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

# The Workload owning any of this deployment's Pods. A group Workload carries plain owner references
# and no controller reference, so ownership is what identifies it.
# TWO API CALLS PER INVOCATION, NOT ONE PER WORKLOAD, and here that is correctness rather than
# tidiness. This is the hot path: assigned_sets calls it, wait_admitted calls assigned_sets in a 3s
# poll loop for up to 120s, and it runs twice per iteration. At one get per Workload, a namespace
# holding many of them spends the poll budget on round-trips and the case reports "the baseline did
# not admit" -- a FAIL naming the operator for what is this script's own latency. Names and owner
# uids come back together and the match happens locally.
group_workload() {
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
      case " $owners " in *" $u "*) echo "$wl"; return 0 ;; esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
}

# The PodSets this deployment's Workload has been assigned a flavor for, "" when it holds no
# admission at all. Assignment is per PodSet, so this is also how "both roles, one admission" is read.
assigned_sets() {
  local wl
  wl="$(group_workload "$1")"
  [ -n "$wl" ] || return 0
  kubectl -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
    -o jsonpath='{range .status.admission.podSetAssignments[*]}{.name}{" "}{end}' 2>/dev/null
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

# One replica's CPU request, read from a Pod the operator actually rendered rather than from the
# InstanceType's unit resources: the request is what Kueue charges, and it is not the unit verbatim.
REQ_RAW="$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case50-subject \
  -o jsonpath='{.items[0].spec.containers[0].resources.requests.cpu}' 2>/dev/null)"
REQ="$(millis "$REQ_RAW")"

QUOTA_RAW="$(kubectl get clusterqueue "$CQ" \
  -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[?(@.name=="cpu")].nominalQuota}' 2>/dev/null)"
QUOTA="$(millis "$QUOTA_RAW")"

kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai case50-subject \
  --ignore-not-found --wait=false >/dev/null 2>&1
for _ in $(seq 1 30); do
  [ "$(kubectl -n "$NS" get pods -l app.kubernetes.io/instance=case50-subject \
    --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break
  sleep 2
done
force_release case50-subject

# --- size the shortage from the pool's own numbers ---

if [ "$REQ" -le 0 ] || [ "$QUOTA" -le 0 ]; then
  record SKIP "a shortage one role wide could be created" \
    "could not read the numbers: request='${REQ_RAW}' quota='${QUOTA_RAW}'"
  FILL=0
else
  FILL=$(((QUOTA / REQ) - 1))
  REMAIN=$((QUOTA - FILL * REQ))
fi

# GUARDED ON THE NUMBERS BEING READABLE, or this reports the same skip twice. The branch above
# already sets FILL=0 when it could not read them, and this message would then state "the pool holds
# 0m against a 0m replica" -- a second row for one cause, whose text is false.
if [ "${FILL:-0}" -lt 1 ] && [ "$REQ" -gt 0 ] && [ "$QUOTA" -gt 0 ]; then
  record SKIP "a shortage one role wide could be created" \
    "the pool holds ${QUOTA}m against a ${REQ}m replica, which leaves no room to occupy"
  FILL=0
fi

# THE WINDOW IS THE EXPERIMENT: one role must fit in what is left and two must not, or the headline
# rows below pass with atomicity doing nothing at all. That window is NOT asserted, because it cannot
# fail -- with FILL = floor(QUOTA/REQ) - 1, REMAIN is QUOTA - FILL*REQ = REQ + (QUOTA mod REQ), which
# is in [REQ, 2*REQ) whenever FILL >= 1 by construction. Asserting it would be checking this script's
# arithmetic while presenting itself as a discriminator.
#
# Two things CAN break the experiment, and both are about whether occupying `nominalQuota` really
# makes the pool short:
#
#   Borrowing. A queue in a cohort draws on another queue's unused quota, so the filler holding this
#   one's nominal amount leaves the subject admissible anyway.
#
#   A second flavor. Quota is scoped PER FLAVOR and each PodSet picks its own, so on a multi-flavor
#   pool the filler can saturate one flavor while the subject is admitted from another. REMAIN then
#   describes one flavor rather than the pool.
#
# Either one makes the headline rows below FAIL against a correct implementation, for a cause this
# case never looked at. THEY ARE CHECKED BEFORE THE FILLER IS APPLIED: finding out afterwards means
# occupying the pool for an experiment that is then abandoned, and recording a PASS for a filler
# whose purpose has already evaporated.
if [ "$FILL" -ge 1 ]; then
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
    -o jsonpath='{.apiVersion}|{.spec.cohort}{.spec.cohortName}|{range .spec.resourceGroups[*].flavors[*]}{.name}{" "}{end}' 2>/dev/null)"
  PRE_VER="${PRE_RAW%%|*}"
  PRE_REST="${PRE_RAW#*|}"
  PRE_COHORT="${PRE_REST%%|*}"
  PRE_FLAVORS="$(printf '%s' "${PRE_REST#*|}" | wc -w | tr -d ' ')"

  if [ -z "$PRE_RAW" ]; then
    record SKIP "the shortage cannot be relieved by borrowing or by another flavor" \
      "could not read cluster queue '${CQ}'"
    FILL=0
  elif [ "$PRE_VER" != "kueue.x-k8s.io/v1beta1" ] && [ "$PRE_VER" != "kueue.x-k8s.io/v1beta2" ]; then
    record SKIP "the shortage cannot be relieved by borrowing or by another flavor" \
      "cluster queue '${CQ}' is served as '${PRE_VER}', whose cohort field this case does not know how to read"
    FILL=0
  elif [ -n "$PRE_COHORT" ]; then
    record SKIP "the shortage cannot be relieved by borrowing or by another flavor" \
      "cluster queue '${CQ}' is in cohort '${PRE_COHORT}', so occupying its nominalQuota does not make the pool short"
    FILL=0
  elif [ "$PRE_FLAVORS" != 1 ]; then
    record SKIP "the shortage cannot be relieved by borrowing or by another flavor" \
      "cluster queue '${CQ}' offers ${PRE_FLAVORS} flavors and quota is per flavor, so a filler on one leaves the other free"
    FILL=0
  else
    record PASS "the shortage cannot be relieved by borrowing or by another flavor" \
      "cluster queue '${CQ}' (${PRE_VER}) is in no cohort and offers one flavor, so ${REMAIN}m against a ${REQ}m role is the whole story"
  fi
fi

if [ "$FILL" -ge 1 ]; then
  apply_md case50-filler "$(role_block bulk '' "$FILL")"

  FILLED="$(wait_admitted case50-filler 1)"
  if [ -z "$FILLED" ]; then
    record FAIL "the filler occupies the pool" \
      "the filler was never admitted, so the pool is not short and nothing below is under test. apply said: ${APPLY_OUT:0:200}"
    FILL=0
  else
    record PASS "the filler occupies the pool" \
      "${FILL} replica(s) of ${REQ}m hold ${QUOTA}m minus ${REMAIN}m"
  fi
fi

# --- the headline: nothing starts, including the role that would have fit ---

if [ "$FILL" -ge 1 ]; then
  apply_md case50-subject "$(role_block prefill prefill 1)
$(role_block decode decode 1)"

  # Wait for the group to be COMPOSED before reading its admission: an absent Workload and an
  # unadmitted one both read as "no assignment", and only the second is the state under test.
  SUBJ=""
  for _ in $(seq 1 40); do
    SUBJ="$(group_workload case50-subject)"
    [ -n "$SUBJ" ] && break
    sleep 3
  done

  if [ -z "$SUBJ" ]; then
    record FAIL "the short pool leaves the group unadmitted" \
      "no Workload was composed for the subject at all, so its admission was never decided. apply said: ${APPLY_OUT:0:200}"
  else
    # Held for a while rather than sampled once: an admission that arrives late would otherwise read
    # as a refusal.
    STILL=yes
    for _ in $(seq 1 8); do
      [ -n "$(assigned_sets case50-subject)" ] && { STILL=no; break; }
      sleep 3
    done
    if [ "$STILL" = yes ]; then
      record PASS "the short pool leaves the group unadmitted" \
        "${SUBJ} holds no podSetAssignments while ${REMAIN}m is free"
    else
      record FAIL "the short pool leaves the group unadmitted" \
        "assignments appeared: $(assigned_sets case50-subject)"
    fi
  fi

  # THE ROW THIS FILE EXISTS FOR. Independent Workloads would have ungated the role that fits and
  # queued the other; one Workload gates both. Counted over the replicas of EACH role, so a reading
  # of "some gated" cannot pass for "all gated".
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
      "all ${SEEN} replicas across both roles still carry ${GATE}"
  else
    record FAIL "the role that would have fit is gated too" \
      "ungated replicas by role: ${UNGATED}- this is the per-role admission the group exists to prevent"
  fi

  # THE OPERATOR'S OWN ACCOUNT OF THE SHORTAGE, which is a different subject from the two rows above.
  # Those read Kueue's Workload and the kubelet's gates -- state Kueue owns. This reads what
  # observeModelDeploymentQuota wrote onto the ModelDeployment, and a regression that stopped
  # reporting the wait entirely would leave both rows above green.
  #
  # Polled rather than sampled, because the condition is written by a reconcile that follows the
  # admission decision rather than accompanying it.
  QR=""
  for _ in $(seq 1 10); do
    QR="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai case50-subject \
      -o jsonpath='{range .status.conditions[?(@.type=="QuotaReserved")]}{.status}|{.reason}|{.message}{end}' 2>/dev/null)"
    case "$QR" in False\|Pending\|*) break ;; esac
    sleep 3
  done
  case "$QR" in
    False\|Pending\|*"$CQ"*)
      record PASS "the deployment reports the wait, naming the queue" \
        "QuotaReserved=False reason=Pending naming ${CQ}"
      ;;
    False\|Pending\|*)
      record FAIL "the deployment reports the wait, naming the queue" \
        "QuotaReserved=False reason=Pending but the message does not name ${CQ}: ${QR}"
      ;;
    "")
      record FAIL "the deployment reports the wait, naming the queue" \
        "no QuotaReserved condition was written at all while the group sat unadmitted"
      ;;
    *)
      record FAIL "the deployment reports the wait, naming the queue" \
        "expected False/Pending while the pool is short, got: ${QR}"
      ;;
  esac

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
echo
echo "STATUS | CHECK | OBJECT"
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-50] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-50] all checks passed"
