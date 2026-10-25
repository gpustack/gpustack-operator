#!/usr/bin/env bash
#
# CASE 78 — A scale moves its queue's admitted quota by exactly one replica, an instance of several
#           Pods is admitted as one group, and a shed surplus releases Kueue's finalizer
#   (MUTATING, self-cleaning)
#
#   case-78.sh <NS>
#
# Goal:        Three contracts of the ModelDeployment controller, told on one InstanceType, one
#              queue and one namespace.
#
#              THE SCALE IS A TRIM, NOT A REBUILD. While a role's replicas shared one Kueue group,
#              the declared total was a number every member carried, so changing it tore the group
#              down and recomposed it -- every replica of that role was deleted and remade, losing
#              its accelerators, its loaded weights and its cache to add or remove one sibling. Each
#              replica is admitted as its own Workload now, and the UID is the only observable that
#              answers whether a scale rebuilt anything: a replica that stayed and one replaced by
#              an identical render agree on everything else. Both legs read the queue as well,
#              because a replica whose Workload was deleted but whose quota was never returned looks
#              exactly like a correct scale from the Pod list alone.
#
#              THE GROUP IS ONE ADMISSION. An instance of several Pods is fate-sharing: Kueue
#              composes ONE Workload with ONE PodSet of `size`, the members share a pod-group name,
#              the instance publishes them under a headless Service of its own, and the role's
#              Service fronts the leader alone. A Service selector written when an instance was one
#              Pod fronts all of them -- and the members that are not the leader serve no API. The
#              failure is a Service that answers some requests and not others, interleaved, which
#              looks like an unhealthy engine rather than a wrong selector.
#
#              THE SURPLUS IS RELEASED, NOT STRANDED. A role can hold more live Pods than it
#              declares -- two Pods on one ordinal, left by a create whose response was lost and
#              retried. The controller sheds the excess and creates no replacement, because the
#              role is over its count rather than under it. Kueue holds a finalizer on every Pod of
#              a serving group and releases it only when that group's Workload is deleted, so a
#              shed Pod whose Workload nobody deletes stays terminating forever, holding its
#              quota, with nothing erroring. The controller deletes the shed Pod's Workload with
#              it, and only a live Kueue can show that: a fake client runs no Kueue controller,
#              and there a shed Pod simply disappears whether or not anything released it.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), a Kueue whose
#              pod integration is enabled, and an operator image carrying the per-replica admission
#              unit. The namespace must carry the pool's entrance LocalQueue. NO GPU is needed:
#              the quota this case reads is whatever its queue accounts in, and on a CPU-only pool
#              that is cpu. EXITS 2 (input required) when the cluster has no InstanceType.
#
#              THE POOL MUST HAVE ROOM. The scale leg needs room for THREE replicas of the chosen
#              InstanceType, the group leg for FOUR Pods of it -- two instances of two -- and the
#              surplus leg for the seated replica plus its twins. A workload that cannot be
#              admitted is not a failure of this feature, and the row that would report it says so
#              rather than blaming the scale or the render.
#
# Inputs:      All real, nothing mocked. The KVCachePoolBinding is not resolved by anything
#              asserted here, so it may be absent. The roles name an explicit image because the
#              replicas are placeholders and no assertion reads what they run; the group leg's
#              image defaults to busybox rather than pause because two of its rows exec into a
#              member, and pause has no shell. Override with E2E_MD_IMAGE (it must carry a shell
#              and nslookup, or those rows SKIP), the InstanceType with E2E_MD_INSTANCE_TYPE.
#
#              MOCKED: the surplus leg's twin Pods are hand-made copies of the seated Pod -- the
#              fixture that stands in for a retried create. Real: Kueue's admission, finalizer and
#              Workloads, and the controller's shed.
#
# Expected:    - the scale leg: two replicas admit; scaling to three keeps both original UIDs, adds
#              one Workload and raises the queue's admitted count by exactly one replica's worth;
#              scaling back to two keeps the two oldest UIDs, removes the third's Workload, and
#              returns the queue to exactly its starting numbers;
#              - the surplus leg: the seated replica runs and its Workload reads Admitted=True; an
#              admitted twin nobody releases still exists with Kueue's finalizer after the hold
#              window (E2E_C78_HOLD, default 20s) and goes once its Workload is deleted by hand; an
#              admitted twin joined to the deployment on the seated Pod's ordinal leaves within the
#              bound (E2E_C78_BOUND, default 60s), its Workload is deleted, and the one that stays
#              keeps its UID and its Workload's UID, still Admitted=True, with no third Pod
#              created; a twin in the seated Pod's own group is gone within the bound, and the
#              seated Pod and its Workload keep their UIDs;
#              - the group leg: one instance of two members composes ONE Workload with ONE PodSet
#              of two named for the role; both members share a pod-group name and differ in their
#              member index; the headless Service publishes both and the role's Service exactly
#              one; each member resolves the leader by name and is told who to talk to; adding a
#              second instance leaves the first instance's member UIDs untouched and brings a
#              Workload and a Service of its own.
#
# Cleanup:     A trap deletes both deployments, every twin, and every Workload still owning one of
#              their Pods -- the manual form of the release the operator performs. Idempotent, runs
#              on pass AND fail, safe to re-run. It creates no cluster-scoped object and changes no
#              baseline.
#
set -uo pipefail

# The kubectl shim bounds every request and retries a transport failure, so a jittering connection
# cannot stall a poll. It is a pass-through otherwise.
E2E_SHIM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-78.sh <NS>" >&2
  exit 2
fi

# The single-member deployment carries the scale leg and the surplus leg; the group leg's Pods are
# a different shape and run a different image, so it is an object of its own.
MD=case78-scale
MD_SIZE=case78-size
ROLE=server
SIZE=2
BINDING="${E2E_MD_BINDING:-case78-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
# The placeholder image of the single-member legs, and of the group leg unless it is overridden:
# that leg execs into a member, and pause has no shell.
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
MEMBER_IMAGE="${E2E_MD_IMAGE:-docker.io/library/busybox:1.36}"
SETTLE="${E2E_MD_SETTLE:-60}"
MEMBER_SETTLE="${E2E_MD_MEMBER_SETTLE:-${E2E_MD_SETTLE:-90}}"
HOLD="${E2E_C78_HOLD:-20}"
BOUND="${E2E_C78_BOUND:-60}"

# The twins of the surplus leg, by name and by UID: the names are what the trap deletes, the UIDs
# are what finds the Workloads those Pods own.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
HOLD_INSTANCE="case78-hold-${SFX}"
WORK="$(mktemp -d)"
TWINS=()
TWIN_UIDS=()

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"

k() { kubectl "$@"; }

# The first InstanceType A DEPLOYMENT CAN ACTUALLY NAME, which is not the same as the first one the
# API returns.
#
# THE LIST COMES BACK SORTED BY NAME AND CARRIES TYPES ON THEIR WAY OUT. A case that creates an
# InstanceType of its own and deletes it without waiting leaves a name that sorts before an ordinary
# derived type -- so a case running straight after it picks a type that is already terminating.
# Naming one is refused at admission, and the run then dies at fixture time for a reason that has
# nothing to do with what it measures. Inactive is excluded for the mirror reason: a deployment on
# one is admitted and then never scheduled, so the case waits out every timeout it has.
if [ -z "$IT" ]; then
  IT="$(md_usable_instance_type)"
fi
if [ -z "$IT" ]; then
  echo "[case-78] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# How many Pods one deployment holds.
pod_count() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=$1" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c . || true
}

# How many of them are Running.
running_count() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=$1" \
    --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
    2>/dev/null | grep -c . || true
}

# Pod name=UID for every Pod of one deployment, sorted. The UID is what tells a member that STAYED
# from one replaced by an identical render: everything else about the two agrees.
# md_pod_uids in _md-lib.sh reads it, and md_surviving_uids counts what is still present.
pod_uids() {
  md_pod_uids "$NS" "$1"
}

# How many of a recorded UID set are still present.
surviving() {
  md_surviving_uids "$NS" "$2" "$1"
}

# How many UIDs a recorded set holds.
count_uids() {
  local n=0
  for _ in $1; do
    n=$((n + 1))
  done
  echo "$n"
}

# Every Workload owning one of this deployment's Pods, by name, sorted. Matched on the ownerReference
# UID: Kueue names a Workload after the group and a group name is a hash, so nothing in the name can
# be matched on. md_workloads in _md-lib.sh reads them; the sort is this case's.
deployment_workloads() {
  md_workloads "$NS" "$1" | sort -u
}

wl_count() { deployment_workloads "$1" | grep -c . || true; }

# THE QUEUE IS FOUND THROUGH AN ADMITTED WORKLOAD, not derived from the InstanceType's name. The
# derivation exists in the operator and repeating it here would make this case pass whenever both
# copies are wrong in the same way.
#
# IT WAITS FOR THE ADMISSION, because the settle this runs after does not. That settle returns as
# soon as the Pods and their Workloads EXIST, and a Workload Kueue has composed but not yet admitted
# carries no .status.admission at all -- so a single read lands on an empty string.
#
# MEASURED: the whole case once ran in six seconds and every queue reading below said "no-queue".
# The damage is not one wrong row. An empty queue name makes wait_queue_caught_up return without
# waiting, turns the up leg's comparison into a FAIL that blames the queue for having no count, and
# leaves the down leg comparing "no-queue" against "no-queue" -- which PASSES. A lost race thus
# reports as one product failure and one product success, neither of which happened.
#
# Timing out still yields the empty string rather than a guess: the rows below are written to report
# what they found, and a queue that never admits is a finding rather than something to wait out.
queue_of() {
  local wl cq i=0
  while [ "$i" -lt 30 ]; do
    i=$((i + 1))
    wl="$(deployment_workloads "$1" | head -1)"
    if [ -n "$wl" ]; then
      cq="$(k -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
        -o jsonpath='{.status.admission.clusterQueue}' 2>/dev/null)"
      if [ -n "$cq" ]; then
        printf '%s' "$cq"
        return 0
      fi
    fi
    sleep 2
  done

  return 0
}

# The queue's admitted count and its total usage of every resource it covers, as one string. Read
# together because they move together: a Workload admitted without its request being charged, or a
# charge left behind by a Workload that went, are both states the count alone reads as correct.
queue_reading() {
  local cq="$1"
  [ -n "$cq" ] || { echo "no-queue"; return 0; }
  printf 'admitted=%s usage=%s' \
    "$(k get clusterqueue "$cq" -o jsonpath='{.status.admittedWorkloads}' 2>/dev/null)" \
    "$(k get clusterqueue "$cq" \
      -o jsonpath='{range .status.flavorsUsage[*]}{range .resources[*]}{.name}:{.total}{" "}{end}{end}' 2>/dev/null)"
}

# The admitted count out of a reading, or empty when it carries none. Split out because the count is
# the one component of that string an equation can be written against: the usage half names whatever
# resources the pool accounts in, which differ per pool, so it is reported rather than predicted.
admitted_count() {
  local r="${1#admitted=}"
  r="${r%% *}"
  case "$r" in
    '' | *[!0-9]*) return 0 ;;
  esac
  printf '%s' "$r"
}

# THE QUEUE'S STATUS FLAGS THE WORKLOADS IT ACCOUNTS FOR, and every reading below has to wait for it.
# A Workload reads Admitted the moment Kueue admits it; the ClusterQueue republishes its counters on
# a pass of its own. Reading the two in one breath samples a queue that has not caught up.
#
# MEASURED, ON THE FIRST RUN OF THIS CASE: the baseline read `admitted=0` while both replicas were
# already admitted and holding their Workloads. Every later comparison was then against a number
# from before the case began -- the up leg read "0 -> 0" and reported that a scale which had
# actually happened had not been charged for.
#
# IT WAITS FOR "AT LEAST OUR OWN", NOT FOR AN EXACT NUMBER. The queue is shared, so a neighbour
# admitting into it inflates the count; an equality check would then run out the clock on a reading
# that was correct. This is the same limit the up-leg row states: the count belongs to the queue,
# not to this deployment.
wait_queue_caught_up() {
  local cq="$1" mine i=0 seen
  mine="$(pod_count "$2")"
  [ -n "$cq" ] && [ "${mine:-0}" -gt 0 ] || return 0
  while [ "$i" -lt 30 ]; do
    i=$((i + 1))
    seen="$(admitted_count "$(queue_reading "$cq")")"
    [ -n "$seen" ] && [ "$seen" -ge "$mine" ] && return 0
    sleep 2
  done

  return 1
}

# THE DOWN LEG NEEDS THE OPPOSITE WAIT, and the difference is not a detail: the queue has to RELEASE
# a reservation, and "accounts for at least our own" is ALREADY TRUE while the shed replica's quota
# is still held. Waiting on that predicate would return immediately and read the pre-release number.
#
# It waits for the reading to reach the value the row is about to compare against, then gives up and
# lets the row report what it actually found. Waiting cannot turn a wrong number into a right one --
# a queue that never returns the quota times out here and FAILS below, printing both readings, which
# is the outcome that row exists to produce.
wait_queue_returns_to() {
  local cq="$1" want="$2" i=0
  [ -n "$cq" ] || return 0
  while [ "$i" -lt 30 ]; do
    i=$((i + 1))
    [ "$(queue_reading "$cq")" = "$want" ] && return 0
    sleep 2
  done

  return 1
}

# Wait until the deployment holds exactly $2 Pods AND $3 Workloads. Both, because the Pods appear
# first: a poll that stopped at the Pod count would read the queue mid-admission and report a
# shortfall that resolves a second later.
wait_settled() {
  local md="$1" want_pods="$2" want_wls="$3" i=0
  while [ "$i" -lt "$SETTLE" ]; do
    i=$((i + 1))
    if [ "$(pod_count "$md")" = "$want_pods" ] && [ "$(wl_count "$md")" = "$want_wls" ]; then
      return 0
    fi
    sleep 2
  done

  return 1
}

# Wait until the deployment's Pods are gone. A deleted replica holds Kueue's finalizer until its
# Workload is deleted, and the operator does that as part of the deployment's teardown.
wait_gone() {
  local i=0
  while [ "$i" -lt 30 ]; do
    i=$((i + 1))
    [ "$(pod_count "$1")" = 0 ] && return 0
    sleep 2
  done

  return 1
}

# The group leg's shape: $2 instances of $SIZE members each are running, with a Workload per
# instance. All three, because they settle in that order: the Pods appear, then Kueue composes, then
# the kubelet starts them -- and a poll stopping at the first would read the later two mid-flight.
wait_instances() {
  local md="$1" instances="$2" i=0 want running
  want=$((instances * SIZE))
  while [ "$i" -lt "$MEMBER_SETTLE" ]; do
    i=$((i + 1))
    running="$(running_count "$md")"
    if [ "$(pod_count "$md")" = "$want" ] && \
      [ "$(wl_count "$md")" = "$instances" ] && [ "$running" = "$want" ]; then
      return 0
    fi
    sleep 2
  done

  return 1
}

# Every Workload owning a Pod whose UID is listed, as "<name> <uid> <admitted>" lines. Found through
# the owner references because a Workload's name is Kueue's to choose.
workloads_of() {
  local uids=" $* "
  k -n "$NS" get workloads.kueue.x-k8s.io -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.uid}|{.status.conditions[?(@.type=="Admitted")].status}|{.metadata.ownerReferences[*].uid}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name uid admitted owners; do
        [ -n "$name" ] || continue
        for o in $owners; do
          case "$uids" in *" $o "*) echo "$name $uid ${admitted:-<none>}"; break ;; esac
        done
      done
}

pod_field() {
  k -n "$NS" get pod "$1" -o jsonpath="$2" 2>/dev/null
}

# A running Pod whose Workload is admitted -- the state the surplus rows start from.
wait_running_admitted() {
  local pod="$1" i=0 uid
  while [ "$i" -lt 120 ]; do
    i=$((i + 2))
    uid="$(pod_field "$pod" '{.metadata.uid}')"
    if [ "$(pod_field "$pod" '{.status.phase}')" = Running ] && [ -n "$uid" ] \
      && workloads_of "$uid" | grep -q ' True$'; then
      return 0
    fi
    sleep 2
  done
  return 1
}

gone_within() {
  local pod="$1" secs="$2" i=0
  while [ "$i" -lt "$secs" ]; do
    i=$((i + 1))
    k -n "$NS" get pod "$pod" >/dev/null 2>&1 || return 0
    sleep 1
  done
  return 1
}

# The distinct pod-group names across a deployment's members: one per INSTANCE, so its count is
# how many instances Kueue is being asked to admit. md_group_names in _md-lib.sh reads it.
group_names() {
  md_group_names "$NS" "$1"
}

group_count() { group_names "$1" | grep -c . || true; }

# Every Workload's PodSets as "<name>:<count>" lines, one Workload per line. This is the shape the
# group leg turns on: one PodSet of SIZE, named for the role.
podset_shapes() {
  local wl
  while IFS= read -r wl; do
    [ -n "$wl" ] || continue
    k -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
      -o jsonpath='{range .spec.podSets[*]}{.name}:{.count}{" "}{end}{"\n"}' 2>/dev/null
  done < <(deployment_workloads "$1")
}

pods_json() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=$1" -o json 2>/dev/null
}

# How many addresses a Service publishes, ready and not-ready together. Both, because a headless
# Service for a collective publishes unready members on purpose -- a member cannot become ready
# until it can reach the peers this record names.
endpoint_count() {
  k -n "$NS" get endpoints "$1" \
    -o jsonpath='{range .subsets[*]}{range .addresses[*]}{.ip}{"\n"}{end}{range .notReadyAddresses[*]}{.ip}{"\n"}{end}{end}' \
    2>/dev/null | grep -c . || true
}

service_names() {
  k -n "$NS" get services -l "app.kubernetes.io/instance=$1" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$' | sort
}

# A copy of the seated Pod: same spec, same seat labels, same fingerprint -- so the controller
# cannot tell the two apart by anything but the name -- with the group, the instance label and the
# owner chosen by the row that makes it. The node binding and the scheduling gates are dropped so
# Kueue gates and admits it afresh.
make_twin() {
  local name="$1" group="$2" instance="$3" owned="$4"
  k -n "$NS" get pod "$SEATED" -o json | python3 -c '
import json, sys
name, group, instance, owned = sys.argv[1:5]
p = json.load(sys.stdin); m = p["metadata"]
labels = dict(m["labels"]); labels["kueue.x-k8s.io/pod-group-name"] = group
labels["app.kubernetes.io/instance"] = instance
ann = {k: v for k, v in m.get("annotations", {}).items() if k != "kueue.x-k8s.io/workload"}
meta = {"name": name, "namespace": m["namespace"], "labels": labels, "annotations": ann}
if owned == "owned":
    meta["ownerReferences"] = m["ownerReferences"]
spec = p["spec"]; spec.pop("nodeName", None); spec.pop("schedulingGates", None)
print(json.dumps({"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": spec}))
' "$name" "$group" "$instance" "$owned" >"$WORK/${name}.json" \
    && k create -f "$WORK/${name}.json" >/dev/null
}

# The twins are released: the Pod is deleted and so is the Workload it owns, because a Pod holding
# Kueue's finalizer keeps its quota until the Workload goes.
release_twins() {
  local t w
  for t in ${TWINS[@]+"${TWINS[@]}"}; do
    k -n "$NS" delete pod "$t" --ignore-not-found --wait=false >/dev/null 2>&1
  done
  for w in $(workloads_of ${TWIN_UIDS[@]+"${TWIN_UIDS[@]}"} | awk '{print $1}'); do
    k -n "$NS" delete workloads.kueue.x-k8s.io "$w" --ignore-not-found --wait=false >/dev/null 2>&1
  done
}

cleanup() {
  local md pod row wl uids u
  # The Pods are read BEFORE they are deleted: a UID is what finds the Workload a deleted Pod owned.
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  uids="$uids $(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_SIZE}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  for pod in ${TWINS[@]+"${TWINS[@]}"}; do
    uids="$uids $(k -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  done

  for md in "$MD" "$MD_SIZE"; do
    k -n "$NS" delete modeldeployments.worker.gpustack.ai "$md" \
      --ignore-not-found --wait=false >/dev/null 2>&1
  done
  release_twins
  sleep 5

  if [ -n "${uids// /}" ]; then
    while IFS= read -r row; do
      [ -n "$row" ] || continue
      wl="${row%%=*}"
      for u in $uids; do
        case " ${row#*=} " in
          *" $u "*)
            k -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
              --ignore-not-found --wait=false >/dev/null 2>&1
            break
            ;;
        esac
      done
    done <<EOF
$(k -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
  fi
  rm -rf "$WORK"

  return 0
}
trap cleanup EXIT

# ------------------------------------------------------------------------------------------------
# THE SCALE LEG: one role, two replicas, then three, then two.
# ------------------------------------------------------------------------------------------------

APPLY_OUT="$(cat <<YAML | k apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
  - name: server
    kind: Server
    instanceType: ${IT}
    replicas: 2
    image: ${IMAGE}
    command: ["/pause"]
YAML
)"

# The leg is SETTLED OR NOT, and the rows below read the two outcomes apart: a leg whose fixture
# never settled has nothing to scale, so its remaining rows are not evaluated and the case moves on
# to the next leg rather than stopping there.
SCALE_SETTLED=""
CQ=""
BASE_UIDS=""
BASE_Q=""
if wait_settled "$MD" 2 2; then
  SCALE_SETTLED=yes
  CQ="$(queue_of "$MD")"
  BASE_UIDS="$(pod_uids "$MD")"
  wait_queue_caught_up "$CQ" "$MD"
  BASE_Q="$(queue_reading "$CQ")"
fi

if [ "$SCALE_SETTLED" = yes ]; then
  record PASS "two replicas admit, one Workload each" \
    "queue ${CQ:-<none>} reads: ${BASE_Q}"
else
  record FAIL "two replicas admit, one Workload each" \
    "reached $(pod_count "$MD") replica(s) and $(wl_count "$MD") Workload(s); apply said: ${APPLY_OUT:0:200}"
fi

if [ "$SCALE_SETTLED" = yes ]; then
  # --- up: 2 -> 3 ---

  # The patch output is KEPT rather than discarded: a refused edit and a slow one produce the same
  # silence downstream, and the message saying which is only available here.
  if ! UP_PATCH="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":3}]' 2>&1)"; then
    record FAIL "scaling up is accepted" \
      "the replicas edit was refused, so nothing below measured a scale: ${UP_PATCH:0:220}"
  else
    record PASS "scaling up is accepted" "replicas 2 -> 3"
  fi

  if ! wait_settled "$MD" 3 3; then
    record FAIL "the added replica gets a Workload of its own" \
      "reached $(pod_count "$MD") replica(s) and $(wl_count "$MD") Workload(s) — if the pool is full, this is capacity rather than the scale"
    record SKIP "scaling up leaves both original replicas untouched" "the scale never settled"
    record SKIP "the queue is charged for exactly one more replica" "the scale never settled"
  else
    record PASS "the added replica gets a Workload of its own" \
      "3 replicas, 3 Workloads: the new one was admitted on its own rather than with the others"

    kept="$(surviving "$BASE_UIDS" "$MD")"
    want="$(count_uids "$BASE_UIDS")"
    # THE ROW THIS CASE EXISTS FOR. Before the per-replica unit this was FALSE by construction: the
    # group's declared total moved, so every replica of the role was rebuilt to carry the new one.
    if [ "$kept" = "$want" ]; then
      record PASS "scaling up leaves both original replicas untouched" \
        "all ${want} kept their UIDs; the third was created beside them"
    else
      record FAIL "scaling up leaves both original replicas untouched" \
        "only ${kept}/${want} kept their UIDs — a scale rebuilt replicas it was asked only to add beside"
    fi

    wait_queue_caught_up "$CQ" "$MD"
    UP_Q="$(queue_reading "$CQ")"
    # ONE MORE, NOT MERELY DIFFERENT. A count that moved the wrong way, or by two, is a different
    # defect from one that did not move at all, and a check on inequality alone reports all three as
    # the same pass. The down leg already states its expectation as an equation; this is the same
    # discipline on the leg where the expected value has to be computed rather than remembered.
    #
    # WHAT THIS READING CANNOT SEPARATE: the count belongs to the whole ClusterQueue, which is the
    # only place the charge is visible at all. Another deployment admitting into the same queue inside
    # this window is counted here too. Nothing in this case can tell the two apart, so an off-by-more
    # reading is worth checking against the queue's other tenants before it is read as a scale defect.
    BASE_ADM="$(admitted_count "$BASE_Q")"
    UP_ADM="$(admitted_count "$UP_Q")"
    if [ -z "$BASE_ADM" ] || [ -z "$UP_ADM" ]; then
      record FAIL "the queue is charged for exactly one more replica" \
        "no admitted count to compare: before [${BASE_Q}] after [${UP_Q}]"
    elif [ "$UP_ADM" -eq "$((BASE_ADM + 1))" ]; then
      record PASS "the queue is charged for exactly one more replica" \
        "admitted ${BASE_ADM} -> ${UP_ADM}; usage before [${BASE_Q}] after [${UP_Q}]"
    else
      record FAIL "the queue is charged for exactly one more replica" \
        "admitted ${BASE_ADM} -> ${UP_ADM}, wanted $((BASE_ADM + 1)): before [${BASE_Q}] after [${UP_Q}]"
    fi
  fi

  # --- down: 3 -> 2 ---

  if ! DOWN_PATCH="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' 2>&1)"; then
    record FAIL "scaling down is accepted" "the replicas edit was refused: ${DOWN_PATCH:0:220}"
  else
    record PASS "scaling down is accepted" "replicas 3 -> 2"
  fi

  if ! wait_settled "$MD" 2 2; then
    record FAIL "the shed replica takes its Workload with it" \
      "settled at $(pod_count "$MD") replica(s) and $(wl_count "$MD") Workload(s): a Workload left behind holds quota for a replica that is gone"
    record SKIP "the two survivors are the two that were already there" "the scale-down never settled"
    record SKIP "the queue returns to exactly its starting numbers" "the scale-down never settled"
  else
    record PASS "the shed replica takes its Workload with it" \
      "2 replicas, 2 Workloads: the removed replica's Workload was deleted rather than stranded"

    # THE SHED DIRECTION IS WHERE A REBUILD SHOWS. Scaling up can only add, so an implementation that
    # rebuilt everything still ends with the right count; here it would end with two replicas that are
    # both new. Which two survive is also stated: a scale-down sheds the HIGHEST ordinals, so the two
    # the deployment started with are the two that must remain.
    kept_down="$(surviving "$BASE_UIDS" "$MD")"
    want_down="$(count_uids "$BASE_UIDS")"
    if [ "$kept_down" = "$want_down" ]; then
      record PASS "the two survivors are the two that were already there" \
        "both original UIDs outlived a scale up and back down; the replica shed is the one added"
    else
      record FAIL "the two survivors are the two that were already there" \
        "only ${kept_down}/${want_down} of the original UIDs survived — the scale-down shed a replica that predated it"
    fi

    wait_queue_returns_to "$CQ" "$BASE_Q"
    DOWN_Q="$(queue_reading "$CQ")"
    # EXACTLY THE STARTING NUMBERS, not merely "lower". A queue that returned some of the charge and
    # kept the rest reads as a correct scale to any check that only asserts a decrease, and the
    # leaked remainder is quota no later deployment can use and nothing reports.
    #
    # A READING WITH NO COUNT IN IT IS REFUSED FIRST, on the same grounds the up leg refuses one: two
    # readings that both say "no queue was ever found" are equal, so the comparison below would pass
    # without having compared any quota at all -- and that pass is indistinguishable from the one this
    # row exists to produce.
    if [ -z "$(admitted_count "$BASE_Q")" ] || [ -z "$(admitted_count "$DOWN_Q")" ]; then
      record FAIL "the queue returns to exactly its starting numbers" \
        "no admitted count to compare: before [${BASE_Q}] after [${DOWN_Q}]"
    elif [ "$DOWN_Q" = "$BASE_Q" ]; then
      record PASS "the queue returns to exactly its starting numbers" \
        "[${DOWN_Q}] — the shed replica's reservation was returned in full"
    else
      record FAIL "the queue returns to exactly its starting numbers" \
        "started [${BASE_Q}], ended [${DOWN_Q}] — the difference is quota held for a replica that no longer exists"
    fi
  fi

  # --- deferred: the serving half ---

  # A LEDGER ENTRY, stating what does NOT close it. A gap recorded as only "deferred" gets closed by
  # the first thing that looks like coverage.
  record SKIP "the surviving replicas keep serving across both scales" \
    "needs an engine that serves, which needs an accelerator. NOT closed by: the UID rows above, which prove the Pod object survived rather than that its engine never stopped answering; nor by a readiness probe on a pause image, which answers for a container that serves nothing"
fi

# ------------------------------------------------------------------------------------------------
# THE SURPLUS LEG: a replica over its count, shed while it runs admitted.
# ------------------------------------------------------------------------------------------------

# THE SEAT IS ONE REPLICA. The surplus rows are about a role that is OVER its count, so the
# deployment the twins race is scaled down to the single replica they sit beside. The scale is
# setup, not a claim: the scale leg above already owns what a replicas edit does.
SEAT_PATCH="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type=json \
  -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":1}]' 2>&1)"

SEATED=""
SEATED_UID=""
SEATED_WL=""
SEATED_ADMITTED=""
if wait_settled "$MD" 1 1; then
  for ((i = 0; i < 90; i += 2)); do
    SEATED="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [ -n "$SEATED" ] && break
    sleep 2
  done
  if [ -n "$SEATED" ] && wait_running_admitted "$SEATED"; then
    SEATED_ADMITTED=yes
    SEATED_UID="$(pod_field "$SEATED" '{.metadata.uid}')"
    read -r SEATED_WL _ _ <<<"$(workloads_of "$SEATED_UID" | head -1)"
  fi
fi

if [ "$SEATED_ADMITTED" = yes ]; then
  record PASS "the seated replica runs and is admitted" "${SEATED} in Workload ${SEATED_WL} (Admitted=True)"
else
  record FAIL "the seated replica runs and is admitted" \
    "pod '${SEATED:-<none>}' never ran admitted; the scale to one replica said: ${SEAT_PATCH:0:200}"
fi

# --- the baseline: nobody releases it ---

BASE="${MD}-base-${SFX}"
TWINS+=("$BASE")
if make_twin "$BASE" "case78-base-${SFX}" "$HOLD_INSTANCE" unowned && wait_running_admitted "$BASE"; then
  TWIN_UIDS+=("$(pod_field "$BASE" '{.metadata.uid}')")
  k -n "$NS" delete pod "$BASE" --wait=false >/dev/null 2>&1
  sleep "$HOLD"
  BASE_FIN="$(pod_field "$BASE" '{.metadata.finalizers}')"
  if echo "$BASE_FIN" | grep -q 'kueue.x-k8s.io/managed'; then
    record PASS "an admitted Pod nobody releases stays held" \
      "${BASE} still present ${HOLD}s after its delete, finalizers=${BASE_FIN}"
  else
    record FAIL "an admitted Pod nobody releases stays held" \
      "${BASE} was released without its Workload being deleted (finalizers='${BASE_FIN:-<pod gone>}'); \
the surplus row below cannot tell the controller's release from Kueue's on this version"
  fi
  for w in $(workloads_of ${TWIN_UIDS[@]+"${TWIN_UIDS[@]}"} | awk '{print $1}'); do
    k -n "$NS" delete workloads.kueue.x-k8s.io "$w" --wait=false >/dev/null 2>&1
  done
  if gone_within "$BASE" "$BOUND"; then
    record PASS "deleting its Workload releases it" "${BASE} gone once its Workload was deleted"
  else
    record FAIL "deleting its Workload releases it" "${BASE} still present ${BOUND}s after its Workload was deleted"
  fi
else
  record FAIL "an admitted Pod nobody releases stays held" "the baseline twin ${BASE} never ran admitted"
fi

# --- a running surplus on the seat ---

TWIN="${MD}-surplus-${SFX}"
TWINS+=("$TWIN")
if make_twin "$TWIN" "case78-surplus-${SFX}" "$HOLD_INSTANCE" unowned && wait_running_admitted "$TWIN"; then
  TWIN_UID="$(pod_field "$TWIN" '{.metadata.uid}')"
  TWIN_UIDS+=("$TWIN_UID")
  read -r TWIN_WL _ _ <<<"$(workloads_of "$TWIN_UID" | head -1)"
  OWNERS="$(pod_field "$SEATED" '{.metadata.ownerReferences}')"
  k -n "$NS" patch pod "$TWIN" --type=merge \
    -p "{\"metadata\":{\"labels\":{\"app.kubernetes.io/instance\":\"${MD}\"},\"ownerReferences\":${OWNERS}}}" >/dev/null

  # Whichever of the two the controller keeps, exactly one leaves; which one is its tie-break on
  # the name, and nothing here depends on it.
  LEFT=""
  i=0
  while [ "$i" -lt "$BOUND" ]; do
    i=$((i + 1))
    s=0; t=0
    k -n "$NS" get pod "$SEATED" >/dev/null 2>&1 && s=1
    k -n "$NS" get pod "$TWIN" >/dev/null 2>&1 && t=1
    if [ $((s + t)) -eq 1 ]; then
      [ "$s" -eq 0 ] && LEFT="$SEATED" || LEFT="$TWIN"
      break
    fi
    sleep 1
  done
  if [ -z "$LEFT" ]; then
    record FAIL "the shed surplus releases Kueue's finalizer" \
      "both ${SEATED} and ${TWIN} still present ${BOUND}s after the twin joined; seated finalizers=$(pod_field "$SEATED" '{.metadata.finalizers}') deletion=$(pod_field "$SEATED" '{.metadata.deletionTimestamp}'); twin finalizers=$(pod_field "$TWIN" '{.metadata.finalizers}') deletion=$(pod_field "$TWIN" '{.metadata.deletionTimestamp}')"
  else
    if [ "$LEFT" = "$TWIN" ]; then
      KEPT="$SEATED"; KEPT_UID="$SEATED_UID"; KEPT_WL="$SEATED_WL"; LEFT_UID="$TWIN_UID"; LEFT_WL="$TWIN_WL"
    else
      KEPT="$TWIN"; KEPT_UID="$TWIN_UID"; KEPT_WL="$TWIN_WL"; LEFT_UID="$SEATED_UID"; LEFT_WL="$SEATED_WL"
    fi
    record PASS "the shed surplus releases Kueue's finalizer" "${LEFT} left within ${i}s of the twin joining"
    if [ -z "$(workloads_of "$LEFT_UID")" ] \
      && ! k -n "$NS" get workloads.kueue.x-k8s.io "$LEFT_WL" >/dev/null 2>&1; then
      record PASS "the shed surplus takes its Workload with it" "Workload ${LEFT_WL} is gone"
    else
      record FAIL "the shed surplus takes its Workload with it" "Workload ${LEFT_WL} still exists"
    fi
    sleep 5
    NOW_UID="$(pod_field "$KEPT" '{.metadata.uid}')"
    KEPT_ROW="$(workloads_of "$KEPT_UID" | head -1)"
    if [ "$NOW_UID" = "$KEPT_UID" ] && [ "$(echo "$KEPT_ROW" | awk '{print $1}')" = "$KEPT_WL" ] \
      && [ "$(echo "$KEPT_ROW" | awk '{print $3}')" = True ]; then
      record PASS "the replica left beside it keeps its Pod and its Workload" \
        "${KEPT} (${KEPT_UID}) in ${KEPT_WL}, Admitted=True"
    else
      record FAIL "the replica left beside it keeps its Pod and its Workload" \
        "${KEPT} uid now '${NOW_UID:-<gone>}' (was ${KEPT_UID}); its Workload row '${KEPT_ROW:-<none>}' (was ${KEPT_WL} Admitted=True)"
    fi
    N="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [ "$N" = 1 ]; then
      record PASS "no replacement is created for a surplus" "1 Pod for 1 declared replica"
    else
      record FAIL "no replacement is created for a surplus" "${N} Pods for 1 declared replica"
    fi
    SEATED="$KEPT"; SEATED_UID="$KEPT_UID"; SEATED_WL="$KEPT_WL"
  fi
else
  record FAIL "the shed surplus releases Kueue's finalizer" "the surplus twin ${TWIN} never ran admitted"
fi

# --- a twin in the seated Pod's group ---

SAME="${MD}-samegroup-${SFX}"
TWINS+=("$SAME")
SEATED_GROUP="$(pod_field "$SEATED" '{.metadata.labels.kueue\.x-k8s\.io/pod-group-name}')"
SEATED_WL_UID_NOW="$(k -n "$NS" get workloads.kueue.x-k8s.io "$SEATED_WL" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
if make_twin "$SAME" "$SEATED_GROUP" "$MD" owned; then
  TWIN_UIDS+=("$(pod_field "$SAME" '{.metadata.uid}')")
  if gone_within "$SAME" "$BOUND"; then
    record PASS "a twin in the seated group is removed" "${SAME} gone within the bound"
  else
    record FAIL "a twin in the seated group is removed" \
      "${SAME} still present ${BOUND}s later: finalizers=$(pod_field "$SAME" '{.metadata.finalizers}') gates=$(pod_field "$SAME" '{.spec.schedulingGates}')"
  fi
  sleep 5
  if [ "$(pod_field "$SEATED" '{.metadata.uid}')" = "$SEATED_UID" ] \
    && [ "$(k -n "$NS" get workloads.kueue.x-k8s.io "$SEATED_WL" -o jsonpath='{.metadata.uid}' 2>/dev/null)" = "$SEATED_WL_UID_NOW" ]; then
    record PASS "the seated replica survives a twin in its own group" "${SEATED} and ${SEATED_WL} keep their UIDs"
  else
    record FAIL "the seated replica survives a twin in its own group" \
      "${SEATED} uid now '$(pod_field "$SEATED" '{.metadata.uid}')' (was ${SEATED_UID}); ${SEATED_WL} uid now '$(k -n "$NS" get workloads.kueue.x-k8s.io "$SEATED_WL" -o jsonpath='{.metadata.uid}' 2>/dev/null)' (was ${SEATED_WL_UID_NOW})"
  fi
else
  record FAIL "a twin in the seated group is removed" "the same-group twin could not be created"
fi

# ------------------------------------------------------------------------------------------------
# THE GROUP LEG: one instance of two members, then two instances.
# ------------------------------------------------------------------------------------------------

# The quota the legs above held is released before this leg is created: its four Pods are the most
# this case ever asks for, and a pool that cannot hold them would report a capacity shortage as a
# render failure.
k -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" --ignore-not-found --wait=false >/dev/null 2>&1
release_twins
wait_gone "$MD"

APPLY_SIZE_OUT="$(cat <<YAML | k apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_SIZE}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${MEMBER_IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
  - name: ${ROLE}
    kind: Server
    instanceType: ${IT}
    replicas: 1
    size: ${SIZE}
    image: ${MEMBER_IMAGE}
    command: ["sh", "-c", "sleep 100000"]
YAML
)"

GROUP_SETTLED=""
if wait_instances "$MD_SIZE" 1; then
  GROUP_SETTLED=yes
fi

if [ "$GROUP_SETTLED" = yes ]; then
  record PASS "an instance of two members admits as one" \
    "2 members running behind 1 Workload -- which is also the baseline every row below rests on"
else
  record FAIL "an instance of two members admits as one" \
    "reached $(pod_count "$MD_SIZE") member(s), $(wl_count "$MD_SIZE") Workload(s); apply said: ${APPLY_SIZE_OUT:0:240}"
fi

if [ "$GROUP_SETTLED" = yes ]; then
  # --- the admission shape: ONE group, ONE PodSet, count TWO ---

  # NOT NAMED GROUPS: bash keeps an array of the caller's own unix groups under that name, and an
  # assignment to it is DISCARDED WITHOUT AN ERROR. The row then compares the primary group id --
  # 20 on a mac, 0 under a root CI -- against 1, so it could never pass and never said why.
  GROUP_COUNT="$(group_count "$MD_SIZE")"
  if [ "$GROUP_COUNT" = 1 ]; then
    record PASS "the members of one instance share one pod group" \
      "1 group name across 2 members: they are admitted together or not at all"
  else
    record FAIL "the members of one instance share one pod group" \
      "$GROUP_COUNT distinct pod-group names; a group each would make them 2 independent admissions"
  fi

  SHAPES="$(podset_shapes "$MD_SIZE" | tr -d '\n' | sed 's/ *$//')"
  if [ "$SHAPES" = "${ROLE}:${SIZE}" ]; then
    record PASS "Kueue composes one PodSet of two, named for the role" \
      "podSets read [${SHAPES}] -- the count is the instance's size and the name is the role-hash"
  else
    record FAIL "Kueue composes one PodSet of two, named for the role" \
      "podSets read [${SHAPES}], wanted [${ROLE}:${SIZE}]; a name of 'main' means a workload controller claimed it"
  fi

  # THE FAST-ADMISSION ANNOTATION IS A Never, and its absence is asserted rather than assumed: it
  # admits a group on the strength of ONE runnable member, which is the opposite of what the total
  # expresses. Nothing in this operator writes it; what this row catches is something else adding it.
  FAST="$(pods_json "$MD_SIZE" | grep -c 'pod-group-fast-admission' || true)"
  if [ "$FAST" = 0 ]; then
    record PASS "no member carries the fast-admission annotation" \
      "absent on both: a group of two admitted on one runnable member is not fate-sharing"
  else
    record FAIL "no member carries the fast-admission annotation" \
      "$FAST member(s) carry it; the group would admit before its members are all present"
  fi

  # --- addressability: who is published where ---

  RSVC="${MD_SIZE}-${ROLE}-r0"
  SVCS="$(service_names "$MD_SIZE" | tr '\n' ' ')"
  case " $SVCS " in
    *" $RSVC "*)
      record PASS "the instance owns a headless Service of its own" \
        "services are [${SVCS}] -- ${RSVC} is the subdomain its members are published under"
      ;;
    *)
      record FAIL "the instance owns a headless Service of its own" \
        "services are [${SVCS}], expected one named ${RSVC}"
      ;;
  esac

  CLUSTER_IP="$(k -n "$NS" get service "$RSVC" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
  if [ "$CLUSTER_IP" = "None" ]; then
    record PASS "the instance's Service is headless" \
      "clusterIP is None: a virtual address would load-balance between ranks, which is the one thing they must not get"
  else
    record FAIL "the instance's Service is headless" \
      "clusterIP is '${CLUSTER_IP}', so the members resolve to a balancer rather than to each other"
  fi

  # BOTH MEMBERS BEHIND THE INSTANCE'S SERVICE, EXACTLY ONE BEHIND THE ROLE'S. The pair is the point:
  # the first number alone passes for a Service that fronts everything, and the second alone passes
  # for one that fronts nothing.
  RSVC_EP="$(endpoint_count "$RSVC")"
  ROLE_EP="$(endpoint_count "${MD_SIZE}-${ROLE}")"
  if [ "$RSVC_EP" = "$SIZE" ] && [ "$ROLE_EP" = 1 ]; then
    record PASS "the instance publishes both members and the role fronts only the leader" \
      "${RSVC}=${RSVC_EP} endpoints, ${MD_SIZE}-${ROLE}=${ROLE_EP} -- the non-leader members serve no API"
  else
    record FAIL "the instance publishes both members and the role fronts only the leader" \
      "${RSVC}=${RSVC_EP} (wanted ${SIZE}), ${MD_SIZE}-${ROLE}=${ROLE_EP} (wanted 1); a role Service holding ${SIZE} round-robins onto ranks that answer nothing"
  fi

  # --- the names resolve, which is what the whole shape was bought for ---

  LEADER="${MD_SIZE}-${ROLE}-r0-m0"
  WORKER="${MD_SIZE}-${ROLE}-r0-m1"
  LEADER_FQDN="${LEADER}.${RSVC}.${NS}.svc.cluster.local"

  if k -n "$NS" exec "$WORKER" -- sh -c 'command -v nslookup' >/dev/null 2>&1; then
    RESOLVED="$(k -n "$NS" exec "$WORKER" -- nslookup "$LEADER_FQDN" 2>&1)"
    LEADER_IP="$(k -n "$NS" get pod "$LEADER" -o jsonpath='{.status.podIP}' 2>/dev/null)"
    # EVERY TERM IS REQUIRED TO BE NON-EMPTY. `${RESOLVED##*"$LEADER_IP"*}` deletes the longest match,
    # and deleting anything from "" leaves "" -- so on an exec that produced nothing at all, and on a
    # Pod with no IP yet, the -z test is TRUE and the row reports a resolution it never saw.
    if [ -n "$LEADER_IP" ] && [ -n "$RESOLVED" ] && [ -z "${RESOLVED##*"$LEADER_IP"*}" ]; then
      record PASS "a member resolves its leader by a name derived before either existed" \
        "${LEADER_FQDN} -> ${LEADER_IP} from inside ${WORKER}"
    else
      record FAIL "a member resolves its leader by a name derived before either existed" \
        "wanted ${LEADER_IP} for ${LEADER_FQDN}, got: $(echo "$RESOLVED" | tr '\n' ' ' | cut -c1-200)"
    fi
  else
    record SKIP "a member resolves its leader by a name derived before either existed" \
      "deferred: E2E_MD_IMAGE (${MEMBER_IMAGE}) carries no nslookup. NOT closed by asserting the Endpoints above -- those say the record's INPUTS exist, not that cluster DNS answers"
  fi

  # --- the rank layout, which is the half addressability was bought for ---
  #
  # NOT CLOSED BY THE DNS ROW ABOVE. That one proves a member CAN reach the leader; this one proves it
  # is told to. The two fail independently: a render that resolves names and publishes nothing leaves
  # every member idle with a working address it was never given.
  #
  # THE INDEX IS READ FROM THE KUBELET'S RESOLUTION, not from the Pod spec, because the declaration
  # and the resolution fail apart: a fieldRef naming a label that is not there is accepted by the API
  # server and fails the container at start, so a spec-level check passes on a Pod that cannot run.
  # Asked through `env` inside the container, which is the only place the resolved value exists.
  if k -n "$NS" exec "$WORKER" -- sh -c 'command -v env' >/dev/null 2>&1; then
    RANK="$(k -n "$NS" exec "$WORKER" -- env 2>/dev/null | grep '^GPUSTACK_' | sort | tr '\n' ' ')"
    WANT_LEADER="GPUSTACK_REPLICA_LEADER_ADDRESS=${LEADER}.${RSVC}"
    case " $RANK " in
      *" $WANT_LEADER "*) LEADER_OK=yes ;;
      *) LEADER_OK=no ;;
    esac
    case " $RANK " in
      *" GPUSTACK_REPLICA_SIZE=${SIZE} "*) SIZE_OK=yes ;;
      *) SIZE_OK=no ;;
    esac
    # Member one, because member zero's index is zero -- and zero is also what an unresolved variable
    # and an empty label would read as through a test that only checked the leader.
    case " $RANK " in
      *" GPUSTACK_MEMBER_INDEX=1 "*) INDEX_OK=yes ;;
      *) INDEX_OK=no ;;
    esac

    if [ "$LEADER_OK" = yes ] && [ "$SIZE_OK" = yes ] && [ "$INDEX_OK" = yes ]; then
      record PASS "a member is told who to talk to, how many there are and which one it is" \
        "inside ${WORKER}: [${RANK}] -- the index resolved through the downward API to this member's own rank"
    else
      record FAIL "a member is told who to talk to, how many there are and which one it is" \
        "wanted ${WANT_LEADER}, GPUSTACK_REPLICA_SIZE=${SIZE} and GPUSTACK_MEMBER_INDEX=1; got: [${RANK:-<nothing>}]"
    fi
  else
    record SKIP "a member is told who to talk to, how many there are and which one it is" \
      "deferred: E2E_MD_IMAGE (${MEMBER_IMAGE}) carries no env. NOT closed by reading the Pod spec, which shows the fieldRef was declared and not that the kubelet resolved it"
  fi

  # --- the scale leg: a second instance leaves the first alone ---

  BEFORE_UIDS="$(pod_uids "$MD_SIZE")"
  SCALE_OUT="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD_SIZE" --type=merge \
    -p '{"spec":{"roles":[{"name":"'"$ROLE"'","kind":"Server","instanceType":"'"$IT"'","replicas":2,"size":'"$SIZE"',"image":"'"$MEMBER_IMAGE"'","command":["sh","-c","sleep 100000"]}]}}' \
    2>&1)"

  if wait_instances "$MD_SIZE" 2; then
    KEPT="$(surviving "$BEFORE_UIDS" "$MD_SIZE")"
    if [ "$KEPT" = "$SIZE" ]; then
      record PASS "adding an instance leaves the first one's members alone" \
        "both original member UIDs survive: a scale is a trim, not a rebuild, at instance size too"
    else
      record FAIL "adding an instance leaves the first one's members alone" \
        "$KEPT of $SIZE original UIDs survive; the rest were replaced by a scale that only added"
    fi

    GROUP_COUNT2="$(group_count "$MD_SIZE")"
    if [ "$GROUP_COUNT2" = 2 ]; then
      record PASS "the second instance is a pod group of its own" \
        "2 distinct group names across 4 members: each instance is admitted independently"
    else
      record FAIL "the second instance is a pod group of its own" \
        "$GROUP_COUNT2 distinct group name(s) across 4 members; 1 would mean the two instances share an admission"
    fi

    SVCS2="$(service_names "$MD_SIZE" | tr '\n' ' ')"
    case " $SVCS2 " in
      *" ${MD_SIZE}-${ROLE}-r1 "*)
        record PASS "the second instance brings its own headless Service" \
          "services are [${SVCS2}] -- addresses are derived per instance, not per role"
        ;;
      *)
        record FAIL "the second instance brings its own headless Service" \
          "services are [${SVCS2}], expected one named ${MD_SIZE}-${ROLE}-r1; its members have no subdomain to resolve under"
        ;;
    esac

    ROLE_EP2="$(endpoint_count "${MD_SIZE}-${ROLE}")"
    if [ "$ROLE_EP2" = 2 ]; then
      record PASS "the role's Service grows by one endpoint, not by one instance" \
        "${ROLE_EP2} endpoints for 2 instances of ${SIZE}: one leader each"
    else
      record FAIL "the role's Service grows by one endpoint, not by one instance" \
        "${ROLE_EP2} endpoints for 2 instances of ${SIZE}, wanted 2"
    fi
  else
    record FAIL "adding an instance leaves the first one's members alone" \
      "never reached 2 instances of ${SIZE}: $(pod_count "$MD_SIZE") member(s), $(wl_count "$MD_SIZE") Workload(s); patch said: ${SCALE_OUT:0:200}"
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-78] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-78] all checks passed (the serving half is deferred; see the SKIP row)"
