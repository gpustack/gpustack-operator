#!/usr/bin/env bash
#
# CASE 69 — A live ModelDeployment refuses an edit to a field that says WHICH deployment it is, and
#   accepts one that says how it is being run   (MUTATING, self-cleaning)
#
#   case-69.sh <NS>
#
# Goal:        The identity rule is a criterion rather than a list -- a field is frozen when it
#              answers "which deployment is this" and editable when it answers "how is this
#              deployment being run right now". The unit tests walk that criterion field by field.
#              What they cannot reach is the API server: the rule lives in a validating webhook, so
#              whether it is REGISTERED, whether it runs on UPDATE, and whether its message survives
#              the round trip are facts about a deployed cluster. A webhook that is correct and not
#              registered passes every unit test in the repository.
#
# Environment: Any cluster with the operator deployed. No accelerator needed and no second
#              InstanceType: this case is about admission, and nothing here has to be schedulable.
#
# Inputs:      One ModelDeployment, created and then edited twice through the API server.
#
# Expected:    - an edit to a frozen field is REFUSED, and the refusal names the field path;
#              - the message states the RULE and not a mechanism -- it says the edit describes a
#                different deployment, and it does not say conflict, concurrent, lock or race. That
#                wording is asserted because the earlier framing of this freeze was about concurrent
#                writers, and a message carrying it sends an operator hunting for a locking problem
#                that does not exist;
#              - a `replicas` edit on the same object is ACCEPTED. Without this the case passes
#                against a webhook that refuses every update, which is the failure a
#                refusal-only assertion cannot see.
#
# Not covered: what the refusal costs. That an operator has to create a second deployment rather
#              than edit this one is a documented consequence, not something a case can observe.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-69.sh <NS>}"

# The context is an input rather than whatever is current; see case-68 for why.
KCTX="${E2E_KUBE_CONTEXT:-}"
k() { if [ -n "$KCTX" ]; then kubectl --context "$KCTX" "$@"; else kubectl "$@"; fi; }

MD=case69-frozen
BINDING="${E2E_MD_BINDING:-case69-no-such-binding}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

FAILS=0
NOREADS=0
ROWS=()

# NO-READ is a row whose precondition did not hold: nothing was measured, so it neither passed nor
# failed. The three wording rows below all read ONE refusal string, so an edit that was accepted
# leaves them with nothing to inspect -- reporting three more failures there would report one cause
# four times.
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
  echo "[case-69] no InstanceType in the cluster; run case-1 first" >&2
  exit 2
fi

cleanup() {
  k -n "$NS" delete modeldeployment "$MD" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

k apply -f - >/dev/null 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: $MD
  namespace: $NS
spec:
  model:
    name: case69/model
  engine: vllm
  engineVersion: "0.25.1"
  kvCache:
    poolRef:
      name: $BINDING
  roles:
    - name: alpha
      kind: server
      replicas: 1
      instanceType: $IT
      template:
        image: $IMAGE
        command: ["/pause"]
YAML

if ! k -n "$NS" get modeldeployment "$MD" >/dev/null 2>&1; then
  echo "[case-69] the fixture deployment was not created; nothing to edit" >&2
  exit 1
fi

# THE OBJECT HAS TO BE LIVE FOR THIS TO MEAN ANYTHING. The rule runs on UPDATE, so a create-time
# assertion would exercise a different code path and would pass against a webhook registered for
# CREATE alone.
refusal="$(k -n "$NS" patch modeldeployment "$MD" --type=merge \
  -p '{"spec":{"model":{"name":"case69/another-model"}}}' 2>&1)"
rc=$?

if [ "$rc" -ne 0 ]; then
  record PASS "a frozen field is refused on a live object" "spec.model"
else
  record FAIL "a frozen field is refused on a live object" \
    "the edit was ACCEPTED — the webhook is not registered for UPDATE, or the rule is not reached"
fi

if [ "$rc" -eq 0 ]; then
  # There is no refusal to inspect. These three rows are about the WORDING of one, so an accepted
  # edit leaves them unmeasured rather than failed.
  record NO-READ "the refusal names the field path" "the edit was accepted; no refusal to read"
  record NO-READ "the message states the rule" "the edit was accepted; no refusal to read"
  record NO-READ "the message states no mechanism" "the edit was accepted; no refusal to read"
else
  case "$refusal" in
    *spec.model*) record PASS "the refusal names the field path" "spec.model is named" ;;
    *) record FAIL "the refusal names the field path" "[$refusal]" ;;
  esac

  case "$refusal" in
    *"describes a different deployment"*)
      record PASS "the message states the rule" "a different value describes a different deployment" ;;
    *)
      record FAIL "the message states the rule" "[$refusal]" ;;
  esac

  # THE WORDING IT MUST NOT CARRY. An earlier framing of this freeze was "shrink the surface two
  # writers can disagree on"; a message carrying that sends an operator looking for a locking
  # problem that does not exist, which costs them the time to find that it never existed.
  wrong=""
  for word in conflict concurrent lock race; do
    case "$refusal" in *"$word"*) wrong="$wrong $word" ;; esac
  done
  if [ -z "$wrong" ]; then
    record PASS "the message states no mechanism" "none of conflict/concurrent/lock/race"
  else
    record FAIL "the message states no mechanism" "carries:$wrong"
  fi
fi

# THE POSITIVE SIDE. Without it every row above passes against a webhook that refuses every update.
if k -n "$NS" patch modeldeployment "$MD" --type=merge \
  -p '{"spec":{"roles":[{"name":"alpha","kind":"server","replicas":2,"instanceType":"'"$IT"'"}]}}' \
  >/dev/null 2>&1; then
  record PASS "an editable field is accepted" "replicas 1 -> 2"
else
  record FAIL "an editable field is accepted" \
    "a replicas edit was refused too — the rows above say nothing about the criterion"
fi

got="$(k -n "$NS" get modeldeployment "$MD" -o jsonpath='{.spec.roles[0].replicas}' 2>/dev/null)"
if [ "$got" = 2 ]; then
  record PASS "the accepted edit reached storage" "replicas=$got"
else
  record FAIL "the accepted edit reached storage" "replicas=[${got:-none}]"
fi

echo
echo "== case-69: the identity freeze, on a live object =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$NOREADS" -ne 0 ]; then
  echo
  echo "${NOREADS} row(s) had NO READING: a precondition did not hold, so they neither passed nor"
  echo "failed. Treat them as unmeasured, not as covered."
fi

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose:"
  echo "  kubectl -n ${NS} get modeldeployment ${MD} -o yaml"
  echo "  kubectl get validatingwebhookconfigurations -o name | grep gpustack"
  exit 1
fi

if [ "$NOREADS" -ne 0 ]; then
  echo "case-69: every row that could be measured PASSED, but ${NOREADS} were not measured"
  exit 0
fi
echo "all case-69 checks PASS"
