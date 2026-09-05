#!/usr/bin/env bash
#
# CASE 51 — Every P/D refusal fires, from the layer that owns it, and a group shape change rebuilds
#           the group
#   (NON-MUTATING for the refusals, which are server-side dry-runs; one short-lived deployment for
#    the rebuild)
#
#   case-51.sh <NS>
#
# Goal:        CASE 45 pins the single-role admission surface. This pins the rules that arrived with
#              several roles, and it is built on the same distinction: not that a bad manifest is
#              rejected, but WHICH layer rejects it and WHAT the message says.
#
#                schema   — the role name's PodSetReference pattern, the closed `kind` enum, and
#                           uniqueness, which `roles` gets from being a list-map keyed on `name`;
#                webhook  — the count, one-instanceType, the kind combinations, and the
#                           acceleratorKey resolved against the pool's live flavors;
#                controller — the group rebuild, which is a convergence rather than a refusal.
#
#              THE TRAP IS CASE 45'S, AND IT IS WORSE HERE. Every manifest below carries several
#              roles, so a mistake in the SHARED part of the manifest refuses all of them — and a
#              row-per-rule table of refusals reads identically whether the rules exist or not. So
#              row 0 is a two-role deployment that must be ACCEPTED, and every refusal row asserts a
#              fragment of the operator's own wording rather than the bare fact of a rejection.
#
#              THE ACCEPTED BASELINE ALSO CARRIES THE HEADLINE. Two roles being accepted at all is
#              the rule this whole spec exists to lift: the single-role version refused them by
#              name, and case-45's own table has a row that says so.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and an operator
#              image carrying the multi-role ModelDeployment. NO GPU is needed and no
#              KVCachePoolBinding has to exist: nothing here schedules a replica, and the Binding is
#              only resolved by the controller.
#
#              THE acceleratorKey ROW READS THE POOL IT ACTUALLY FINDS. On a CPU-only cluster the
#              pool's flavors pin no accelerator at all, and the operator's message says exactly
#              that — which is a different sentence from "offers [nvidia-h20]" and a different rule
#              from the empty-read exemption. The row asserts the fragment common to both, and the
#              header says why: a case that hard-coded the accelerated wording would fail on the one
#              cluster this file is meant to run on.
#
# Inputs:      All real, nothing mocked. `--dry-run=server` runs the schema and the webhook and
#              persists nothing. The rebuild row creates one ModelDeployment and deletes it again.
#
# Deferred:    Whether the rebuilt group is then ADMITTED — one Workload, two PodSets — is case-49's,
#              and whether a short pool leaves both roles queued is case-50's. This file stops at
#              the operator's own writes, which is what it can assert without an engine.
#
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-51.sh <NS>" >&2
  exit 2
fi

BINDING="${E2E_MD_BINDING:-case51-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
if [ -z "$IT" ]; then
  echo "[case-51] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

# A SECOND InstanceType is what the one-instanceType row needs, and it may not exist: a CPU-only
# cluster materializes exactly one pool. The row reports SKIP rather than inventing a name, because
# a name no InstanceType carries would be refused by the CONTROLLER for not resolving, and this row
# is about the WEBHOOK refusing two of them.
IT2="$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[1].metadata.name}' 2>/dev/null)"

# Emit a manifest whose roles block is supplied whole by the caller. Unlike case-45's, the roles are
# the variable here: every rule below is about the SET of roles rather than about one role's fields.
manifest() {
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD_NAME:-case51-probe}
  namespace: ${NS}
spec:
  engine: ${ENGINE:-vllm}
  engineVersion: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${BINDING}
  roles:
${1}
YAML
}

# Two plain roles, prefill and decode, on one instanceType. The shape every row below varies from.
#
# The explicit image is plumbing, not part of any rule: a CPU-only InstanceType has observed no
# accelerator, so the operator can synthesize no engine image and the CONTROLLER would refuse the
# rebuild row for a reason none of the admission rows are about. Nothing asserted here runs.
two_roles() {
  cat <<YAML
  - name: prefill
    kind: prefill
    instanceType: ${IT}
    replicas: 1
    template:
      image: ${IMAGE}
      command: ["/pause"]
  - name: decode
    kind: decode
    instanceType: ${IT}
    replicas: 1
    template:
      image: ${IMAGE}
      command: ["/pause"]
YAML
}

# Assert that a roles block is refused AND that the refusal quotes $2. A refusal carrying somebody
# else's message is a FAIL: it means the manifest tripped a rule this row is not about.
refuses() {
  local check="$1" want="$2" roles="$3" out
  out="$(manifest "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  # An EMPTY $out must not pass: deleting anything from "" leaves "", so a `-z` test on the stripped
  # string is TRUE when the command produced nothing at all. See case-45 for the same guard.
  if [ -z "$out" ]; then
    record FAIL "$check" "the apply produced no output at all, so nothing was refused"

    return 0
  fi
  case "$out" in
    *"$want"*) record PASS "$check" "refused naming: ${want}" ;;
    *) record FAIL "$check" "refused with the wrong message, or accepted: ${out:0:220}" ;;
  esac
}

# --- row 0: the baseline, and the headline ---

out="$(manifest "$(two_roles)" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
case "$out" in
  *"created (server dry run)"*|*"configured (server dry run)"*)
    record PASS "two roles are ACCEPTED" \
      "the single-role bound is lifted; every refusal below is therefore its own rule" ;;
  *)
    record FAIL "two roles are ACCEPTED" \
      "the baseline was refused, so every row below passes for the wrong reason: ${out:0:220}" ;;
esac

# --- the webhook's rules ---

eleven=""
for i in $(seq 0 10); do
  eleven+="  - name: role-${i}
    instanceType: ${IT}
    replicas: 1
"
done
refuses "eleven roles are refused naming KUEUE's cap" \
  "Kueue caps Workload.spec.podSets at 10" "$eleven"

# BY THE SCHEMA, and the attribution is the finding rather than a detail. `roles` is a list-map keyed
# on `name`, so the API server rejects the duplicate during validation and the webhook's own rule --
# which names the merge, and reads far better -- never runs. Asserting that better wording here would
# fail against a correct operator.
refuses "two roles sharing a name are refused BY THE SCHEMA" \
  'Duplicate value: {"name"' \
  "  - name: worker
    instanceType: ${IT}
    replicas: 1
  - name: worker
    instanceType: ${IT}
    replicas: 1"

# NO SECOND InstanceType HAS TO EXIST. This rule is answered from the submitted object alone --
# validateModelDeploymentRoleInstanceTypes only compares the strings in spec.roles -- and validate()
# runs it BEFORE any rule that reads the cluster, returning early when it speaks. So a name that
# resolves to nothing still exercises exactly this refusal, and gating the row on a second real
# InstanceType left it untested on the single-type cluster this PR was developed against.
#
# The second name is used when the cluster has one, because a row that refuses a real pair is the
# stronger evidence; the synthetic name is the fallback rather than the default.
refuses "two instanceTypes are refused, pointing at acceleratorKey" \
  "acceleratorKey" \
  "  - name: prefill
    instanceType: ${IT}
    replicas: 1
  - name: decode
    instanceType: ${IT2:-case51-no-such-instance-type}
    replicas: 1"

refuses "kind: server beside another kind is refused" \
  "cannot be combined with another kind" \
  "  - name: server
    kind: server
    instanceType: ${IT}
    replicas: 1
  - name: prefill
    kind: prefill
    instanceType: ${IT}
    replicas: 1"

ENGINE=sglang refuses "a kind the engine cannot be told is refused NAMING THE ENGINE" \
  "has no rendering term for kind" "$(two_roles)"

refuses "an acceleratorKey this pool does not offer is refused" \
  "is not a constraint that fails" \
  "  - name: prefill
    kind: prefill
    instanceType: ${IT}
    acceleratorKey: nvidia-no-such-model
    replicas: 1
  - name: decode
    kind: decode
    instanceType: ${IT}
    acceleratorKey: nvidia-no-such-model
    replicas: 1"

# --- the schema's rules, which run BEFORE the webhook and must not be confused with it ---

refuses "a role name that is not a PodSetReference is refused BY THE SCHEMA" \
  "spec.roles[0].name" \
  "  - name: Prefill
    instanceType: ${IT}
    replicas: 1"

refuses "a kind outside the enum is refused BY THE SCHEMA" \
  "spec.roles[0].kind" \
  "  - name: prefill
    kind: router
    instanceType: ${IT}
    replicas: 1"

# --- the controller: a shape change rebuilds the group ---

REBUILD_MD=case51-rebuild

# The two totals the rebuild moves between, named once. They appear in the poll loops, the patch and
# the stale-Pod filter, and three literals cannot be kept in step by anything but attention.
OLD_TOTAL=2   # prefill 1 + decode 1, the shape two_roles renders
NEW_TOTAL=3   # prefill 2 + decode 1, the shape the patch below asks for
# Deleted without waiting, then any Workload still holding the replicas is released by hand. Kueue
# keeps a finalizer on every Pod of a serving group and drops it only when that Workload goes, so a
# cleanup that blocks on the deployment would block for as long as the caller allows if the operator
# ever stopped deleting it -- turning somebody else's regression into this case's timeout.
cleanup() {
  local wl
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$REBUILD_MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  # One list call: this trap runs on every exit, pass or fail, so a get per Workload adds latency to
  # every invocation of the case on a busy namespace.
  local row
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    case "${row#*=}" in
      *"${REBUILD_MD}-"*)
        kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
          --ignore-not-found --wait=false >/dev/null 2>&1
        ;;
    esac
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].name}{"\n"}{end}' 2>/dev/null)
EOF
}
trap cleanup EXIT

# THE TOTAL IS WHAT IS OBSERVED, not the Pod count, and that is the point of the row. Every Pod of
# the group carries the group's declared total and Kueue requires them all to agree; a rebuild is
# how the operator keeps that true when the number moves. Counting Pods alone would pass against an
# implementation that added one Pod beside three declaring the old total -- which is the exact state
# that composes no Workload at all.
totals() {
  kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance=${REBUILD_MD}" \
    -o jsonpath='{range .items[*]}{.metadata.annotations.kueue\.x-k8s\.io/pod-group-total-count}{"\n"}{end}' \
    2>/dev/null | sort -u | tr '\n' ' '
}

# KEPT, unlike a dry-run row's: every rebuild row below depends on this object existing, so a refusal
# here would otherwise surface as three timeouts with three wrong diagnoses instead of one message.
REBUILD_APPLY="$(MD_NAME="$REBUILD_MD" manifest "$(two_roles)" | kubectl apply -f - 2>&1)"

for _ in $(seq 1 30); do
  [ "$(totals)" = "${OLD_TOTAL} " ] && break
  sleep 2
done
if [ "$(totals)" = "${OLD_TOTAL} " ]; then
  record PASS "the group is created carrying one declared total" \
    "every Pod of the two-role group declares ${OLD_TOTAL}"
else
  record FAIL "the group is created carrying one declared total" \
    "observed totals: '$(totals)'; apply said: ${REBUILD_APPLY:0:200}"
fi

TPL='"template":{"image":"'"$IMAGE"'","command":["/pause"]}'
kubectl -n "$NS" patch modeldeployments.worker.gpustack.ai "$REBUILD_MD" --type=merge \
  -p '{"spec":{"roles":[{"name":"prefill","kind":"prefill","instanceType":"'"$IT"'","replicas":2,'"$TPL"'},{"name":"decode","kind":"decode","instanceType":"'"$IT"'","replicas":1,'"$TPL"'}]}}' \
  >/dev/null 2>&1

CONVERGED=no
MIXED=no
for _ in $(seq 1 45); do
  t="$(totals)"
  # A mixed reading is the failure this row exists to catch, and it is caught by OBSERVING rather
  # than by reasoning: two live Pods disagreeing on the total is the state Kueue refuses to compose
  # a Workload for, and it looks like nothing at all from the outside.
  case "$t" in
    *2*3*|*3*2*) MIXED=yes ;;
  esac
  [ "$t" = "${NEW_TOTAL} " ] && { CONVERGED=yes; break; }
  sleep 2
done

if [ "$CONVERGED" = yes ]; then
  record PASS "a replicas change rebuilds the group under the new total" \
    "every Pod of the group ends up declaring ${NEW_TOTAL}"
else
  record FAIL "a replicas change rebuilds the group under the new total" \
    "observed totals: '$(totals)'"
fi

# WHAT THIS ROW CAN AND CANNOT SAY. Polling every 2s cannot see a mixed state that appears and
# resolves between two samples, so a PASS here is "no mixed state was OBSERVED", never "none existed"
# -- and the row is worded that way rather than claiming the stronger thing. What makes it more than
# a coin flip is the second reading below, taken from the final state: the rebuild is expected to
# delete every old-total Pod before creating any new-total one, so no Pod of the old total may
# survive into the converged group. That one does not depend on sampling luck.
if [ "$MIXED" = no ]; then
  record PASS "no mixed-total state was OBSERVED during the rebuild" \
    "polled every 2s and never sampled ${OLD_TOTAL} and ${NEW_TOTAL} together; a window shorter than the interval is not visible to this row"
else
  record FAIL "no mixed-total state was OBSERVED during the rebuild" \
    "sampled a mixed reading, which is the state that composes no Workload at all"
fi

# ONLY MEANINGFUL IF A REBUILD HAPPENED. Run unconditionally, this row fires whenever the rebuild
# never started -- a refused patch leaves every Pod declaring the old total -- and reports it as a
# failure of rebuild ATOMICITY. That is one FAIL for two very different causes, naming the wrong one,
# which is the same misdiagnosis capturing the apply output was meant to end.
if [ "$CONVERGED" != yes ]; then
  record SKIP "no replica of the old total survives into the rebuilt group" \
    "the group never converged to ${NEW_TOTAL}, so there is no rebuilt group to read; see the row above for why"
else
  STALE="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${REBUILD_MD}" \
    -o jsonpath="{range .items[?(@.metadata.annotations.kueue\\.x-k8s\\.io/pod-group-total-count==\"${OLD_TOTAL}\")]}{.metadata.name}{\" \"}{end}" \
    2>/dev/null)"
  if [ -z "$STALE" ]; then
    record PASS "no replica of the old total survives into the rebuilt group" \
      "read from the converged state, so it does not depend on catching the window"
  else
    record FAIL "no replica of the old total survives into the rebuilt group" \
      "still declaring ${OLD_TOTAL}: ${STALE}"
  fi
fi

# Results.
echo
echo "STATUS | CHECK | OBJECT"
for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
[ "$FAILS" -eq 0 ] || { echo "[case-51] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-51] all checks passed"
