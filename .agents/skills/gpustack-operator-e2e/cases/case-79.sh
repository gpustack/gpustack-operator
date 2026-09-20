#!/usr/bin/env bash
#
# CASE 79 — An instance of several Pods is admitted as one, addressable by name, and fronted by
#           its leader alone; scaling the role leaves it untouched   (MUTATING, self-cleaning)
#
#   case-79.sh <NS>
#
# Goal:        NOTHING BELOW E2E HAS THE TWO HALVES AT ONCE. A unit test can assert what the
#              renderer emits -- the names, the labels, the selector terms -- and none of it can
#              assert that Kueue composed ONE Workload of two from them, that cluster DNS answers
#              for the names, or that the role's Service ended up with one endpoint rather than
#              two. Each of those is a different system agreeing with the render, and this case is
#              where the agreement is observable.
#
#              THE ENDPOINT COUNT IS THE ROW THAT WOULD OTHERWISE BE SILENT. Every member carries
#              the role's identity labels, so a Service selector written when an instance was one
#              Pod fronts all of them -- and the members that are not the leader serve no API. The
#              failure is a Service that answers some requests and not others, interleaved, which
#              looks like an unhealthy engine rather than a wrong selector.
#
#              WHY THE GATED WINDOW IS NOT ASSERTED. Both members are scheduling-gated until the
#              group is admitted, and the window is seconds wide on an idle pool: a poll that
#              caught it would be timing-dependent, and one that missed it would report a pass.
#              What is asserted instead is the state that window exists to produce -- ONE Workload
#              whose single PodSet declares TWO -- which is a fact Kueue can only have reached by
#              waiting for both members to exist. NOT CLOSED BY: polling for a gated Pod, which
#              passes or fails on how busy the cluster was; nor by reading a Pod's scheduling gates
#              after the fact, since an admitted member has none left to read.
#
#              THE SCALE LEG IS CASE 78'S QUESTION AT A SIZE WHERE THE ANSWER COSTS MORE. There,
#              a rebuild would have cost one replica its weights; here it costs an instance whose
#              members had to find each other again, and the failure is likelier because a second
#              instance means a second headless Service and a second group name.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first), a Kueue whose
#              pod integration is enabled, and an operator image carrying the multi-member render.
#              The namespace must carry the pool's entrance LocalQueue. NO GPU is needed. EXITS 2
#              (input required) when the cluster has no InstanceType.
#
#              THE POOL MUST HAVE ROOM FOR FOUR PODS of the chosen InstanceType -- two instances of
#              two. A group that cannot be admitted is not a failure of this feature, and the row
#              that would report it says so rather than blaming the render.
#
# Inputs:      All real, nothing mocked. The image defaults to busybox rather than pause because
#              two rows exec into a member: pause has no shell, and a case that silently lost the
#              DNS assertions to an image choice would keep reporting the rows it can still run.
#              Override with E2E_MD_IMAGE (it must carry a shell and nslookup, or those rows SKIP),
#              the InstanceType with E2E_MD_INSTANCE_TYPE.
#
# Expected:    One instance of two members composes ONE Workload with ONE PodSet of two named for
#              the role; both members share a pod-group name and differ in their member index; the
#              headless Service publishes both and the role's Service exactly one; each member
#              resolves the leader by name. Scaling replicas to 2 leaves the first instance's
#              member UIDs untouched and adds a second instance with its own Workload and Service.
#
# Cleanup:     A trap deletes the deployment and, if any member is wedged past the bound, every
#              Workload still owning one -- the manual form of the release the operator performs.
#              Idempotent, runs on pass AND fail, creates no cluster-scoped object.
#
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-79.sh <NS>" >&2
  exit 2
fi

MD=case79-size
ROLE=server
SIZE=2
BINDING="${E2E_MD_BINDING:-case79-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-docker.io/library/busybox:1.36}"
SETTLE="${E2E_MD_SETTLE:-90}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

k() { kubectl "$@"; }

if [ -z "$IT" ]; then
  IT="$(k get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
if [ -z "$IT" ]; then
  echo "[case-79] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

pods_json() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" -o json 2>/dev/null
}

# Pod name=UID for every member, sorted. The UID is what tells a member that STAYED from one
# replaced by an identical render: everything else about the two agrees.
member_uids() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null \
    | grep -v '^$' | sort | tr '\n' ' '
}

member_count() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c . || true
}

# The distinct pod-group names across this deployment's members: one per INSTANCE, so its count is
# how many instances Kueue is being asked to admit.
group_names() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.labels.kueue\.x-k8s\.io/pod-group-name}{"\n"}{end}' \
    2>/dev/null | grep -v '^$' | sort -u
}

group_count() { group_names | grep -c . || true; }

# Every Workload owning one of this deployment's Pods. Matched on the ownerReference UID: Kueue
# names a Workload after the group and a group name is a hash, so nothing in the name can be
# matched on.
deployment_workloads() {
  local uids
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null | grep -v '^$')"
  [ -n "$uids" ] || return 0
  k -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}|{range .metadata.ownerReferences[*]}{.uid}{" "}{end}{"\n"}{end}' \
    2>/dev/null | while IFS='|' read -r name owners; do
      [ -n "$name" ] || continue
      for u in $uids; do
        case " $owners " in *" $u "*) echo "$name"; break ;; esac
      done
    done | sort -u
}

wl_count() { deployment_workloads | grep -c . || true; }

# Every Workload's PodSets as "<name>:<count>" lines, one Workload per line. This is the shape the
# whole case turns on: one PodSet of SIZE, named for the role.
podset_shapes() {
  local wl
  while IFS= read -r wl; do
    [ -n "$wl" ] || continue
    k -n "$NS" get workloads.kueue.x-k8s.io "$wl" \
      -o jsonpath='{range .spec.podSets[*]}{.name}:{.count}{" "}{end}{"\n"}' 2>/dev/null
  done < <(deployment_workloads)
}

# How many addresses a Service publishes, ready and not-ready together. Both, because a headless
# Service for a collective publishes unready members on purpose -- a member cannot become ready
# until it can reach the peers this record names.
endpoint_count() {
  local svc="$1"
  k -n "$NS" get endpoints "$svc" \
    -o jsonpath='{range .subsets[*]}{range .addresses[*]}{.ip}{"\n"}{end}{range .notReadyAddresses[*]}{.ip}{"\n"}{end}{end}' \
    2>/dev/null | grep -c . || true
}

service_names() {
  k -n "$NS" get services -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$' | sort
}

# How many of a recorded UID set are still present.
surviving() {
  local before="$1" now n=0 e
  now="$(member_uids)"
  for e in $before; do
    case " $now " in *" $e "*) n=$((n + 1)) ;; esac
  done
  echo "$n"
}

cleanup() {
  local row wl uids u
  k -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
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

  return 0
}
trap cleanup EXIT

# Wait until $1 instances of $SIZE members each are running, with a Workload per instance. All
# three, because they settle in that order: the Pods appear, then Kueue composes, then the kubelet
# starts them -- and a poll stopping at the first would read the later two mid-flight.
wait_settled() {
  local instances="$1" i want_members running
  want_members=$((instances * SIZE))
  for i in $(seq 1 "$SETTLE"); do
    running="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
      --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
      2>/dev/null | grep -c . || true)"
    if [ "$(member_count)" = "$want_members" ] &&
      [ "$(wl_count)" = "$instances" ] && [ "$running" = "$want_members" ]; then
      return 0
    fi
    sleep 2
  done

  return 1
}

# --- the deployment: one role, one instance, two members ---

APPLY_OUT="$(cat <<YAML | k apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
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
  - name: ${ROLE}
    kind: server
    instanceType: ${IT}
    replicas: 1
    size: ${SIZE}
    image: ${IMAGE}
    command: ["sh", "-c", "sleep 100000"]
YAML
)"

if ! wait_settled 1; then
  record FAIL "an instance of two members admits as one" \
    "reached $(member_count) member(s), $(wl_count) Workload(s); apply said: ${APPLY_OUT:0:240}"
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
  echo "[case-79] ${FAILS} check(s) FAILED"
  exit 1
fi

record PASS "an instance of two members admits as one" \
  "2 members running behind 1 Workload -- which is also the baseline every row below rests on"

# --- the admission shape: ONE group, ONE PodSet, count TWO ---

# NOT NAMED GROUPS: bash keeps an array of the caller's own unix groups under that name, and an
# assignment to it is DISCARDED WITHOUT AN ERROR. The row then compares the primary group id --
# 20 on a mac, 0 under a root CI -- against 1, so it could never pass and never said why.
GROUP_COUNT="$(group_count)"
if [ "$GROUP_COUNT" = 1 ]; then
  record PASS "the members of one instance share one pod group" \
    "1 group name across 2 members: they are admitted together or not at all"
else
  record FAIL "the members of one instance share one pod group" \
    "$GROUP_COUNT distinct pod-group names; a group each would make them 2 independent admissions"
fi

SHAPES="$(podset_shapes | tr -d '\n' | sed 's/ *$//')"
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
FAST="$(pods_json | grep -c 'pod-group-fast-admission' || true)"
if [ "$FAST" = 0 ]; then
  record PASS "no member carries the fast-admission annotation" \
    "absent on both: a group of two admitted on one runnable member is not fate-sharing"
else
  record FAIL "no member carries the fast-admission annotation" \
    "$FAST member(s) carry it; the group would admit before its members are all present"
fi

# --- addressability: who is published where ---

RSVC="${MD}-${ROLE}-r0"
SVCS="$(service_names | tr '\n' ' ')"
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
ROLE_EP="$(endpoint_count "${MD}-${ROLE}")"
if [ "$RSVC_EP" = "$SIZE" ] && [ "$ROLE_EP" = 1 ]; then
  record PASS "the instance publishes both members and the role fronts only the leader" \
    "${RSVC}=${RSVC_EP} endpoints, ${MD}-${ROLE}=${ROLE_EP} -- the non-leader members serve no API"
else
  record FAIL "the instance publishes both members and the role fronts only the leader" \
    "${RSVC}=${RSVC_EP} (wanted ${SIZE}), ${MD}-${ROLE}=${ROLE_EP} (wanted 1); a role Service holding ${SIZE} round-robins onto ranks that answer nothing"
fi

# --- the names resolve, which is what the whole shape was bought for ---

LEADER="${MD}-${ROLE}-r0-m0"
WORKER="${MD}-${ROLE}-r0-m1"
LEADER_FQDN="${LEADER}.${RSVC}.${NS}.svc.cluster.local"

if k -n "$NS" exec "$WORKER" -- sh -c 'command -v nslookup' >/dev/null 2>&1; then
  RESOLVED="$(k -n "$NS" exec "$WORKER" -- nslookup "$LEADER_FQDN" 2>&1)"
  LEADER_IP="$(k -n "$NS" get pod "$LEADER" -o jsonpath='{.status.podIP}' 2>/dev/null)"
  # EVERY TERM IS REQUIRED TO BE NON-EMPTY. `${RESOLVED##*$LEADER_IP*}` deletes the longest match,
  # and deleting anything from "" leaves "" -- so on an exec that produced nothing at all, and on a
  # Pod with no IP yet, the -z test is TRUE and the row reports a resolution it never saw.
  if [ -n "$LEADER_IP" ] && [ -n "$RESOLVED" ] && [ -z "${RESOLVED##*$LEADER_IP*}" ]; then
    record PASS "a member resolves its leader by a name derived before either existed" \
      "${LEADER_FQDN} -> ${LEADER_IP} from inside ${WORKER}"
  else
    record FAIL "a member resolves its leader by a name derived before either existed" \
      "wanted ${LEADER_IP} for ${LEADER_FQDN}, got: $(echo "$RESOLVED" | tr '\n' ' ' | cut -c1-200)"
  fi
else
  record SKIP "a member resolves its leader by a name derived before either existed" \
    "deferred: E2E_MD_IMAGE (${IMAGE}) carries no nslookup. NOT closed by asserting the Endpoints above -- those say the record's INPUTS exist, not that cluster DNS answers"
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
    "deferred: E2E_MD_IMAGE (${IMAGE}) carries no env. NOT closed by reading the Pod spec, which shows the fieldRef was declared and not that the kubelet resolved it"
fi

# --- the scale leg: a second instance leaves the first alone ---

BEFORE_UIDS="$(member_uids)"
SCALE_OUT="$(k -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type=merge \
  -p '{"spec":{"roles":[{"name":"'"$ROLE"'","kind":"server","instanceType":"'"$IT"'","replicas":2,"size":'"$SIZE"',"image":"'"$IMAGE"'","command":["sh","-c","sleep 100000"]}]}}' \
  2>&1)"

if wait_settled 2; then
  KEPT="$(surviving "$BEFORE_UIDS")"
  if [ "$KEPT" = "$SIZE" ]; then
    record PASS "adding an instance leaves the first one's members alone" \
      "both original member UIDs survive: a scale is a trim, not a rebuild, at instance size too"
  else
    record FAIL "adding an instance leaves the first one's members alone" \
      "$KEPT of $SIZE original UIDs survive; the rest were replaced by a scale that only added"
  fi

  GROUP_COUNT2="$(group_count)"
  if [ "$GROUP_COUNT2" = 2 ]; then
    record PASS "the second instance is a pod group of its own" \
      "2 distinct group names across 4 members: each instance is admitted independently"
  else
    record FAIL "the second instance is a pod group of its own" \
      "$GROUP_COUNT2 distinct group name(s) across 4 members; 1 would mean the two instances share an admission"
  fi

  SVCS2="$(service_names | tr '\n' ' ')"
  case " $SVCS2 " in
    *" ${MD}-${ROLE}-r1 "*)
      record PASS "the second instance brings its own headless Service" \
        "services are [${SVCS2}] -- addresses are derived per instance, not per role"
      ;;
    *)
      record FAIL "the second instance brings its own headless Service" \
        "services are [${SVCS2}], expected one named ${MD}-${ROLE}-r1; its members have no subdomain to resolve under"
      ;;
  esac

  ROLE_EP2="$(endpoint_count "${MD}-${ROLE}")"
  if [ "$ROLE_EP2" = 2 ]; then
    record PASS "the role's Service grows by one endpoint, not by one instance" \
      "${ROLE_EP2} endpoints for 2 instances of ${SIZE}: one leader each"
  else
    record FAIL "the role's Service grows by one endpoint, not by one instance" \
      "${ROLE_EP2} endpoints for 2 instances of ${SIZE}, wanted 2"
  fi
else
  record FAIL "adding an instance leaves the first one's members alone" \
    "never reached 2 instances of ${SIZE}: $(member_count) member(s), $(wl_count) Workload(s); patch said: ${SCALE_OUT:0:200}"
fi

echo
echo "STATUS | CHECK | OBJECT"
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done

if [ "$FAILS" -gt 0 ]; then
  echo "[case-79] ${FAILS} check(s) FAILED"
  exit 1
fi

echo "[case-79] all checks passed"
