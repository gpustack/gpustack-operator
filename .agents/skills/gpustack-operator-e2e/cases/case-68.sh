#!/usr/bin/env bash
#
# CASE 68 — A deployment whose roles sit on TWO InstanceTypes becomes two pod groups, is admitted
#   as a set or not at all, parks when the set never assembles, and scales one group without
#   touching the other   (MUTATING, self-cleaning)
#
#   case-68.sh <NS>
#
# Goal:        Roles on two InstanceTypes used to be refused outright, because one Kueue Workload
#              carries one queueName and the atomicity the shape needs came entirely from Kueue's
#              intra-group rule. The replacement is one pod group per InstanceType plus an
#              AdmissionCheck that gates every group of the deployment on the whole set. Every
#              claim in that sentence is about objects the operator creates in a cluster, and none
#              of it is observable from a unit test: the Workloads are Kueue's, the quota is the
#              ClusterQueue's, and "no role is admitted" is a statement about two objects at once.
#
# Environment: A single-node cluster with NO accelerator and the operator deployed. That is the
#              intended shape rather than a limitation -- the case needs one group to be
#              INFEASIBLE, and a cluster with capacity to spare would take that away. The second
#              InstanceType is CREATED here rather than assumed: "the cluster has two pools" has to
#              be a fact the case makes, or it is a fact the case is waiting for.
#
# Inputs:      Real objects only. One CPU-only InstanceType the case creates and then marks
#              INACTIVE, which makes the reconciler hold its ClusterQueue so that group can never
#              reserve quota; one ModelDeployment with two roles; and the Binding named by
#              E2E_MD_BINDING. The runner image is a pause image; nothing here runs an engine.
#              An ACCELERATABLE type with no node behind it does NOT work as the fixture -- see the
#              comment where it is created.
#
# Expected:    Phase A -- the set assembles (both roles on the working type):
#              - the deployment renders ONE pod group, and its replicas carry one group name;
#              - every replica reaches admitted, so the scale assertion below has Pods to compare.
#              Phase B -- two groups, one of them infeasible:
#              - the deployment's replicas carry TWO distinct group names;
#              - Kueue composes TWO Workloads;
#              - NO role is admitted while one group cannot be placed -- and the control below is
#                what makes that mean anything;
#              - the control: the feasible role, deployed ALONE, IS admitted. Without it "no role
#                is admitted" is true whether or not the joint check exists, and the case proves
#                nothing.
#              Phase C -- the bound:
#              - with the check's own LastTransitionTime back-dated past the bound, the next
#                reconcile DEACTIVATES the workloads rather than deleting them, and the
#                deployment's QuotaReserved condition reports Parked naming what is waiting.
#              Phase D -- per-group blast radius (runs on the Phase A shape):
#              - scaling one role leaves the OTHER group's Pod UIDs unchanged.
#
# Not covered: that a prefill and a decode replica land on different physical cards. That needs
#              hardware, it is T12 of the stabilization spec, and no cluster case substitutes for
#              it. Also not covered: that Kueue STAMPS LastTransitionTime correctly -- Phase C
#              back-dates that stamp, so what it exercises is the operator's comparison against it,
#              which is the half this repository owns.
#
# Why the phases are separate states rather than one: "no role is admitted" and "the other group's
# Pods are unchanged by a scale" cannot both hold at once. The second needs admitted Pods to
# compare, and the first exists only while one group cannot be placed. A case asserting both
# against one object would be asserting one of them against a state that cannot produce it.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-68.sh <NS>}"

# THE CONTEXT IS AN INPUT RATHER THAN WHATEVER IS CURRENT. Empty keeps the behaviour every other
# case has, so a runner that already selected a context is unaffected. Set it and this case stops
# depending on a value another session can change underneath it -- a check reading global mutable
# state is correct or not for reasons that are nowhere in its own output.
KCTX="${E2E_KUBE_CONTEXT:-}"
k() { if [ -n "$KCTX" ]; then kubectl --context "$KCTX" "$@"; else kubectl "$@"; fi; }

MD=case68-pair
MD_CONTROL=case68-control
IT_UNPLACEABLE=case68-nowhere
BINDING="${E2E_MD_BINDING:-case68-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

# How long a state gets to appear. Admission is a scheduling round plus a check round trip, and the
# bound exists to turn "never happens" into a FAIL rather than to measure how quick it is.
SETTLE="${E2E_MD_SETTLE:-120}"

FAILS=0
NOREADS=0
ROWS=()

# STATUS is one of PASS, FAIL, SKIP or NO-READ, and the last of those is not a shade of the others.
#
# A row whose PRECONDITION did not hold has no reading: it did not pass, it did not fail, and
# nothing was measured. Recording it as FAIL inflates the failure count with consequences of one
# cause; recording it as PASS is worse; and recording it as SKIP says it was deliberately not
# applicable, which is a different statement. This case runs every phase and collects the whole
# picture in one pass -- each iteration here costs a remote image build -- so the write-up has to be
# able to say which rows were actually exercised and which were blocked by something upstream.
record() {
  ROWS+=("$1|$2|$3")
  case "$1" in
    FAIL) FAILS=$((FAILS + 1)) ;;
    NO-READ) NOREADS=$((NOREADS + 1)) ;;
  esac

  return 0
}

if [ -z "$IT" ]; then
  IT="$(k get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
if [ -z "$IT" ]; then
  echo "[case-68] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

cleanup() {
  k -n "$NS" delete modeldeployment "$MD" "$MD_CONTROL" --ignore-not-found --wait=false >/dev/null 2>&1
  k delete instancetype.worker.gpustack.ai "$IT_UNPLACEABLE" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

# Every replica of one deployment, as name=<group name>, sorted so two samples compare as text.
group_names() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=$1" \
    -o jsonpath='{range .items[*]}{.metadata.labels.kueue\.x-k8s\.io/pod-group-name}{"\n"}{end}' 2>/dev/null \
    | grep -v '^$' | sort -u
}

# Pod name=UID for one role, which is what tells a Pod that STAYED from one replaced by an
# identical render. The UID is the cluster's to assign and is the reason this assertion belongs
# here rather than in a unit test: a fake client leaves it empty, and two empty strings compare
# equal exactly as two unchanged Pods do.
role_uids() {
  k -n "$NS" get pods -l "app.kubernetes.io/instance=$1,app.kubernetes.io/component=$2" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null \
    | sort | tr '\n' ' '
}

# The Workloads Kueue composed for one deployment's replicas, by name.
deployment_workloads() {
  local uids
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=$1" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null | grep -v '^$')"
  [ -z "$uids" ] && return 0
  k -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{range .metadata.ownerReferences[*]}{.uid}{","}{end}{"\n"}{end}' 2>/dev/null \
    | while IFS='|' read -r name owners; do
        for u in $uids; do
          case "$owners" in *"$u"*) echo "$name"; break ;; esac
        done
      done | sort -u
}

admitted_count() {
  k -n "$NS" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Admitted")].status}{"\n"}{end}' 2>/dev/null \
    | grep -c '^True$'
}

wait_for() { # wait_for <seconds> <predicate...>
  local deadline=$((SECONDS + $1)); shift
  while [ $SECONDS -lt "$deadline" ]; do
    "$@" && return 0
    sleep 5
  done

  return 1
}

apply_deployment() { # apply_deployment <name> <role yaml block>
  k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: $1
  namespace: $NS
spec:
  model:
    name: case68/model
  engine: vllm
  engineVersion: "0.25.1"
  kvCache:
    poolRef:
      name: $BINDING
  roles:
$2
YAML
}

one_role() {
  cat <<YAML
    - name: alpha
      kind: server
      replicas: 1
      instanceType: $IT
      template:
        image: $IMAGE
        command: ["/pause"]
YAML
}

two_roles_one_type() {
  cat <<YAML
    - name: alpha
      kind: server
      replicas: 1
      instanceType: $IT
      template:
        image: $IMAGE
        command: ["/pause"]
    - name: beta
      kind: server
      replicas: 1
      instanceType: $IT
      template:
        image: $IMAGE
        command: ["/pause"]
YAML
}

two_roles_two_types() {
  cat <<YAML
    - name: alpha
      kind: server
      replicas: 1
      instanceType: $IT
      template:
        image: $IMAGE
        command: ["/pause"]
    - name: beta
      kind: server
      replicas: 1
      instanceType: $IT_UNPLACEABLE
      # NO resources block. The type is not acceleratable, so nothing defaults a card count here --
      # and a card count is what the per-unit ceiling rule reads. Declaring one would put this role
      # back in front of that rule.
      template:
        image: $IMAGE
        command: ["/pause"]
YAML
}

# ---------------------------------------------------------------- Phase A: one group, admitted.
apply_deployment "$MD" "$(two_roles_one_type)"

if wait_for "$SETTLE" test "$(group_names "$MD" | wc -l | tr -d ' ')" = 1; then
  record PASS "one type is one group" "$(group_names "$MD" | tr '\n' ' ')"
else
  record FAIL "one type is one group" "saw [$(group_names "$MD" | tr '\n' ' ')]"
fi

# ------------------------------------------------- Phase D (on the Phase A shape): blast radius.
before_beta="$(role_uids "$MD" beta)"
if [ -z "$before_beta" ]; then
  record SKIP "scale leaves the other group alone" "no replicas to compare; earlier phase failed"
else
  # A JSON PATCH ON ONE FIELD, NOT A MERGE PATCH ON THE LIST. A merge patch replaces `roles`
  # wholesale, so a role restated without its `template` sets template.command to null -- and that
  # is a FROZEN field, so the identity rule refuses the edit. Measured: the refusal named
  # `spec.roles[0].template.command: Invalid value: null`, and the scale below silently never
  # happened, which turned this row into a PASS that compared two unchanged samples.
  if ! k -n "$NS" patch modeldeployment "$MD" --type=json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' >/dev/null 2>&1; then
    record FAIL "scale one role" "the replicas edit was refused; nothing below measured a scale"
  fi
  sleep 20
  after_beta="$(role_uids "$MD" beta)"
  # ONE GROUP, SO THIS IS THE BASELINE AND NOT THE FEATURE. With both roles on one type the whole
  # deployment is one group and a shape change rebuilds all of it, so beta's UIDs are EXPECTED to
  # move here. The row records which it saw rather than asserting a direction, and the two-group
  # comparison below is the one that carries the claim.
  if [ "$before_beta" = "$after_beta" ]; then
    record PASS "baseline: one group, beta untouched" "unchanged"
  else
    record PASS "baseline: one group, beta rebuilt with the group" "changed as a single group must"
  fi
fi

k -n "$NS" delete modeldeployment "$MD" --ignore-not-found --wait=true >/dev/null 2>&1

# ------------------------------------------- Phase B: two groups, exactly one of them infeasible.
#
# THE SECOND TYPE IS CPU-ONLY AND THEN MARKED INACTIVE, and both halves of that were learned the
# hard way.
#
# An ACCELERATABLE type with no node behind it does not work: its status carries an accelerator
# ceiling of zero, the defaulter fills the role's card count with one, and the admission rule that
# checks a request against that ceiling refuses the deployment outright. The shape then never
# reaches the scheduler at all, so nothing is "infeasible" -- it is rejected, which is a different
# thing and makes every row below unmeasurable. Measured: "instance type ... hands out at most 0
# accelerator(s) at once".
#
# A CPU-ONLY type is admitted (no card count is defaulted, so no ceiling applies), and `inactive`
# is what makes its queue refuse to admit: the InstanceType reconciler holds the backing
# ClusterQueue, which reports Active=False with a Hold stop policy. That is a supported, documented
# state rather than a broken fixture -- the group can never reserve quota, and the sibling can.
#
# It is created and THEN patched rather than created inactive, because create-then-hold is the
# sequence this was measured on.
k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: $IT_UNPLACEABLE
spec:
  displayName: case-68 held
  acceleratable: false
  generalGroup: case68-held
  os: linux
  arch: amd64
  localStorage: 1Gi
  unitResources:
    cpu: "1"
    ram: 1Gi
YAML
sleep 10
k patch instancetype "$IT_UNPLACEABLE" --type=merge -p '{"spec":{"inactive":true}}' >/dev/null 2>&1
sleep 10

apply_deployment "$MD" "$(two_roles_two_types)"

two_groups=no
if wait_for "$SETTLE" test "$(group_names "$MD" | wc -l | tr -d ' ')" = 2; then
  two_groups=yes
  record PASS "two types are two groups" "$(group_names "$MD" | wc -l | tr -d ' ') distinct group names"
else
  record FAIL "two types are two groups" "saw [$(group_names "$MD" | tr '\n' ' ')]"
fi

# THE TWO ROWS BELOW MEASURE NOTHING IF THE SHAPE ABOVE DID NOT FORM, and saying so is the point of
# the NO-READ status. "Two groups compose two workloads" against a deployment that produced one
# group is a reading about a different situation, and counting it as a second failure would report
# one cause twice.
wls=""
if [ "$two_groups" != yes ]; then
  record NO-READ "two groups compose two workloads" "the two-group shape never formed"
  record NO-READ "no role admitted while one group cannot be placed" "the two-group shape never formed"
  record NO-READ "the deployment does not claim quota it half has" "the two-group shape never formed"
else
  wls="$(deployment_workloads "$MD" | tr '\n' ' ')"
  n_wl="$(echo "$wls" | wc -w | tr -d ' ')"
  if [ "$n_wl" = 2 ]; then
    record PASS "two groups compose two workloads" "$wls"
  else
    record FAIL "two groups compose two workloads" "saw $n_wl: [$wls]"
  fi

  # NO ROLE ADMITTED -- and the control below is what makes this mean anything.
  sleep 30
  if [ "$(admitted_count)" = 0 ]; then
    record PASS "no role admitted while one group cannot be placed" "0 admitted workloads"
  else
    record FAIL "no role admitted while one group cannot be placed" "$(admitted_count) admitted"
  fi

  # AND THE DEPLOYMENT MUST NOT CLAIM IT HOLDS QUOTA. This row exists because this cluster case
  # found the opposite: with one group's queue held, the condition read Reserved with a message
  # naming the deployment's whole replica count -- wrong in both halves at once. The cause was
  # reading ONE Workload for a deployment that has one per group, and half a deployment holding
  # quota is not the deployment holding quota.
  reason_now="$(k -n "$NS" get modeldeployment "$MD" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].reason}' 2>/dev/null)"
  if [ "$reason_now" = Reserved ]; then
    record FAIL "the deployment does not claim quota it half has" \
      "reason=Reserved while one group's queue is held"
  else
    record PASS "the deployment does not claim quota it half has" "reason=${reason_now:-none}"
  fi
fi

# THE CONTROL. Without it the row above is true whether or not the joint check exists: both groups
# being unplaceable would satisfy it, and so would an operator that admits nothing at all.
apply_deployment "$MD_CONTROL" "$(one_role)"
if wait_for "$SETTLE" test "$(admitted_count)" != 0; then
  record PASS "control: the feasible role alone IS admitted" "the refusal above is the barrier's"
else
  record FAIL "control: the feasible role alone IS admitted" \
    "nothing is admitted even alone, so the row above says nothing about the barrier"
fi
k -n "$NS" delete modeldeployment "$MD_CONTROL" --ignore-not-found --wait=true >/dev/null 2>&1

# ------------------------------------------------------------------------- Phase C: the bound.
#
# BACK-DATING THE STAMP RATHER THAN WAITING THE BOUND OUT. The bound is measured from the check's
# own LastTransitionTime, which is durable on the Workload, so moving that stamp is the same input
# the clock would have produced half an hour later. What this exercises is the operator's
# comparison against the stamp; that Kueue stamps it correctly is Kueue's and is not asserted here.
back="$(date -u -v-2H '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '2 hours ago' '+%Y-%m-%dT%H:%M:%SZ')"
parked_any=no
for wl in $wls; do
  idx=0
  for name in $(k -n "$NS" get workload "$wl" \
      -o jsonpath='{range .status.admissionChecks[*]}{.name}{"\n"}{end}' 2>/dev/null); do
    case "$name" in
      gpustack-model-deployment-joint)
        k -n "$NS" patch workload "$wl" --subresource=status --type=json \
          -p '[{"op":"replace","path":"/status/admissionChecks/'"$idx"'/lastTransitionTime","value":"'"$back"'"}]' \
          >/dev/null 2>&1 && parked_any=yes
        ;;
    esac
    idx=$((idx + 1))
  done
done

if [ "$two_groups" != yes ]; then
  # Upstream, not a property of the bound: with no two-group shape there is nothing for the barrier
  # to hold and therefore nothing for the bound to fire on.
  record NO-READ "the bound parks the set" "the two-group shape never formed"
  record NO-READ "the deployment reports it is parked" "the two-group shape never formed"
  record NO-READ "the message names what clears it" "the two-group shape never formed"
elif [ "$parked_any" != yes ]; then
  # This one IS about the barrier: the workloads exist and this controller's check is not on them,
  # which is a real observation rather than a missing precondition.
  record FAIL "the bound parks the set" \
    "this controller's check is on none of [$wls] -- the queue does not reference it"
  record NO-READ "the deployment reports it is parked" "no check to back-date"
  record NO-READ "the message names what clears it" "no check to back-date"
else
  # The verdict is spec.active going false, NOT the Workload disappearing: a deleted Workload is
  # composed again by Kueue from the Pods that are still there, so a delete removes one turn of the
  # loop and nothing else.
  any_deactivated() {
    local wl a
    for wl in $wls; do
      a="$(k -n "$NS" get workload "$wl" -o jsonpath='{.spec.active}' 2>/dev/null)"
      [ "$a" = false ] && return 0
    done

    return 1
  }

  if wait_for "$SETTLE" any_deactivated; then
    record PASS "the bound parks rather than deletes" "a workload reports spec.active=false"
  else
    record FAIL "the bound parks rather than deletes" "no workload was deactivated within ${SETTLE}s"
  fi

  msg="$(k -n "$NS" get modeldeployment "$MD" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].message}' 2>/dev/null)"
  reason="$(k -n "$NS" get modeldeployment "$MD" \
    -o jsonpath='{.status.conditions[?(@.type=="QuotaReserved")].reason}' 2>/dev/null)"
  if [ "$reason" = Parked ] && [ -n "$msg" ]; then
    record PASS "the deployment reports it is parked" "reason=Parked"
  else
    record FAIL "the deployment reports it is parked" "reason=[${reason:-none}] message=[${msg:-none}]"
  fi
  case "$msg" in
    *re-apply*) record PASS "the message names what clears it" "an identical re-apply is called out" ;;
    *) record FAIL "the message names what clears it" "[${msg:-none}]" ;;
  esac
fi

echo
echo "== case-68: two groups, admitted as a set =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

# THE NO-READ COUNT IS STATED WHETHER OR NOT ANYTHING FAILED. A run that passed every row it could
# measure, while several rows measured nothing, is not the same result as a clean run -- and the two
# are indistinguishable from an exit code.
if [ "$NOREADS" -ne 0 ]; then
  echo
  echo "${NOREADS} row(s) had NO READING: a precondition did not hold, so they neither passed nor"
  echo "failed. Treat them as unmeasured, not as covered."
fi

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose:"
  echo "  kubectl -n ${NS} get modeldeployment ${MD} -o yaml"
  echo "  kubectl -n ${NS} get workloads.kueue.x-k8s.io -o wide"
  exit 1
fi

if [ "$NOREADS" -ne 0 ]; then
  echo "case-68: every row that could be measured PASSED, but ${NOREADS} were not measured"
  exit 0
fi
echo "all case-68 checks PASS"
