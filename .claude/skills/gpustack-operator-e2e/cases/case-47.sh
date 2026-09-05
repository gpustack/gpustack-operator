#!/usr/bin/env bash
#
# CASE 47 — Two deployments on one Binding share a reuse domain; two on different Bindings do not
#   (MUTATING: three ModelDeployments, one extra Binding and one probe Pod, all in the fixture's
#    namespace, all removed by the shared teardown)
#
#   case-47.sh <NS>
#
# Goal:        The sharing claim this whole design rests on, asserted from the two ends that can be
#              observed without an accelerator: what the operator RENDERED onto each replica, and
#              what the store does with those values.
#
# THE ENGINE IS SGLANG AND THAT IS THE WHOLE DESIGN OF THIS CASE, not a preference. On the vLLM
# family no tenant travels to the store at the versions this project ships, so two deployments on one
# Binding render byte-identical configurations and both land in the tenant named `default`. "They
# share" would then pass with the Binding doing nothing whatsoever -- a tautology in the shape of an
# assertion. SGLang forwards the reuse domain as MOONCAKE_TENANT_ID, so same Binding means same
# tenant and different Bindings mean different tenants, and the two halves below become two
# observations instead of one.
#   Which engines forward a tenant is `inject.SupportsTenant`'s answer, carried beside the version it
#   was measured at. This case states no answer of its own; it asserts what came back for the engine
#   it uses, and it fails if that changes.
#
# THE REPLICAS ARE NOT EXPECTED TO RUN. They are rendered from a client image that carries no engine,
# so no container starts -- and nothing here needs one. Every value this case reads is on the Pod
# SPEC, written by the operator at render time, and the store side is driven by the store's own
# python client. That is what makes the case answerable on a cluster with no accelerator.
#
# THE GAP THIS LEAVES, STATED RATHER THAN IMPLIED. A real replica runs an engine, not this probe, so
# "the engine reads MOONCAKE_TENANT_ID and forwards the value to the store" is a DIFFERENT claim from
# anything below. It is measured in case-59, against a real SGLang image, and that measurement
# applies to the path this case exercises because T14 made both the injection webhook and this
# controller render through the same `pkg/worker/kvcache/inject`. A case that drove the probe and
# then claimed the engine's behaviour would be asserting something it never looked at.
#
# AND THE STORE ROW MEASURES DATA ISOLATION, NOT A QUOTA LEDGER. Those are different claims and the
# difference decides how strong a security statement may lean on this case. What is asserted below is
# that a payload written under one tenant is not readable under another. It is NOT that each tenant
# carries an independent quota account -- this store's quota behaves as an eviction trigger rather
# than a hard ledger, so "one domain, one account" stays an ASSUMPTION here and is tracked on the
# kv-cache side. Nothing in this case may be read as confirming that a workload able to mint domains
# could escape a namespace ceiling; it confirms only that the domains do not see each other's data.
#
# Environment: as CASE 53 — a cluster with no accelerator and a registry it can pull the Mooncake
#              image from. No GPU, no engine image.
# Needs:       a live Mooncake store (the shared fixture brings up backend, pool and Bindings)
#
set -uo pipefail

NS="${1:?usage: case-47.sh <NS>}"
CASE_ID=47

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# THE SHARED TEARDOWN IS WRAPPED, NOT REPLACED, AND THE REASON IS THE WARNING RATHER THAN THE
# CLEANUP. That teardown already knows it may have to force a Binding's finalizer off, says so in
# its own comments, calls it the right trade, and prints loudly -- because "a forced action that
# prints nothing reads as a clean one". All of that is correct for the workload it was written for:
# bare Pods that hold nothing, where reaching that branch means something is genuinely stuck.
#
# A ModelDeployment CLAIMS its Binding, and the Binding's finalizer refuses to release while a claim
# stands. So without this wrapper the forced branch is not the exception, it is EVERY RUN -- measured
# on the first run of this case: 60s wait, `Releasable=False(HeldByWorkloads)` naming case47-a and
# case47-b, then the force. And a warning that prints every time is a warning that prints never.
#
# => THE RULE, for whoever writes the next case here: what decides this is whether the workload
# CLAIMS ITS BINDING, not whether the case uses this library. A bare Pod needs nothing; anything the
# Binding's `usedBy` can name has to be removed before the fixture, or it converts that library's
# rare-and-loud path into a routine one.
#
# THAT RULE WAS NECESSARY AND NOT SUFFICIENT. The wrapper removed all three deployments and the
# forced branch fired anyway, and it took two more runs to learn why, for two different reasons.
#
# The first of those runs printed the warning with NO name under it, and the diagnosis written here
# from the shape of the code alone -- the SECOND BINDING this case creates, which `kvi_teardown`
# never deletes because it deletes its own two BY NAME -- was a guess dressed as a finding. It is a
# real leak and the delete below closes it, but it was not the object being forced.
#
# => THE SHARED TEARDOWN DELETES BY NAME. Whatever this case created in that namespace, this case
# removes -- claimant or not.
#
# The second run, with that delete in, NAMED the object: `bind-i`, the fixture's own Binding, held at
# `Releasable=False(LedgerNotReleased)` -- "the master will not remove the quota of reuse domain
# ... while it still holds objects". The cause is this case's own store probe. It writes a payload to
# prove the tenant partitions, and a domain holding an object is exactly what the master will not
# release. Nothing to do with a claim, and nothing the delete above could have reached.
#
# => THE PROBE DRAINS WHAT IT WROTE, at the end of the probe below, for the same reason this wrapper
# exists at all: a warning that fires on every run stops being read. Measured on the third run, with
# the drain in: the teardown printed `cleanup` and nothing after it.
#
# Measured after both runs of this case: zero KVCachePoolBindings, KVCachePools, KVCacheBackends and
# ModelDeployments left, and no fixture namespace -- the forced release does not accumulate here,
# because this fixture deletes the backend too and the master goes with it. That is a property of
# THIS fixture and not of forcing a finalizer in general, and it is why the leak above cost a
# printed warning rather than a poisoned cluster.
#
# Wrapping keeps the shared teardown's own order intact rather than reimplementing it, and keeps the
# knowledge of ModelDeployment out of a library that deliberately does not know the CR layer.
case47_teardown() {
  if [ -n "${TEST_NS:-}" ]; then
    kubectl -n "$TEST_NS" delete modeldeployments.worker.gpustack.ai \
      case47-a case47-b case47-c --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1
    # Issued without waiting: the deployments above are gone by now, so nothing holds this one, and
    # kvi_teardown's own loop is already a wait over every Binding left in the namespace.
    if [ -n "${OTHER_BINDING:-}" ]; then
      kubectl -n "$TEST_NS" delete kvcachepoolbindings.worker.gpustack.ai "$OTHER_BINDING" \
        --ignore-not-found --wait=false >/dev/null 2>&1
    fi
  fi
  kvi_teardown
}

trap case47_teardown EXIT   # ARM FIRST - kvi_setup can fail with objects already created
kvi_setup

IT="${E2E_MD_INSTANCE_TYPE:-$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)}"
if [ -z "$IT" ]; then
  record FAIL "an instance type exists" "no InstanceType in the cluster; run case-1 first"
  kvi_results "$CASE_ID"
  exit 1
fi

# A SECOND REUSE DOMAIN, which is what the isolation half needs and what the shared fixture does not
# provide. The fixture's other Binding registers the literal `default` so that an engine forwarding
# no tenant can write at all; that is a different job from being a second domain.
OTHER_DOMAIN="case47-other-${SFX}"
OTHER_BINDING="case47-other-${SFX}"
kubectl apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePoolBinding
metadata:
  name: ${OTHER_BINDING}
  namespace: ${TEST_NS}
spec:
  poolRef:
    name: ${POOL}
  domain:
    name: ${OTHER_DOMAIN}
    blockSize: 16
    dtype: bfloat16
  quotaCeiling: 256Mi
YAML
if ! kvi_wait_for kvcachepoolbindings.worker.gpustack.ai "$OTHER_BINDING" \
  '{.status.phase}' Ready 180 "$TEST_NS" >/dev/null; then
  record FAIL "the second reuse domain is registered" \
    "binding ${OTHER_BINDING} did not reach Ready in 180s, so the isolation half has no second tenant to test"
  kvi_results "$CASE_ID"
  exit 1
fi

# --- three deployments: two on one Binding, one on the other ---

# The output is READ, not discarded. A refused manifest -- a bad InstanceType name, a validation
# error, a typo -- is otherwise discovered 120 seconds later as an empty tenant, three times over,
# and the FAIL that follows guesses among three causes that have nothing to do with the real one.
deploy() {
  kubectl apply -f - <<YAML 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${1}
  namespace: ${TEST_NS}
spec:
  engine: sglang
  engineVersion: "0.5.18"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${2}
  roles:
  - name: server
    instanceType: ${IT}
    replicas: 1
    template:
      image: ${CLIENT_IMAGE}
YAML
}

# WHY `created` ONLY HERE, WHERE case-45 ACCEPTS `configured` AND `unchanged` TOO. The difference is
# the namespace, not an oversight. case-45 applies into a caller-supplied $NS that outlives the run,
# so an object can survive a delete that timed out and a re-apply legitimately reports the one it
# found. Everything here lives in TEST_NS="kvc-i-${SFX}", created and destroyed per run with a random
# suffix, and the cluster-scoped objects carry that suffix in their names -- so an object under one of
# these names can only be one this run made. `created` is therefore the exact assertion, and widening
# it would accept a collision that should never happen.
deploy_out="$(deploy case47-a "$BINDING"; deploy case47-b "$BINDING"; deploy case47-c "$OTHER_BINDING")"
deploy_created="$(printf '%s\n' "$deploy_out" | grep -c ' created$' || true)"
if [ "${deploy_created:-0}" -ne 3 ]; then
  record SKIP "the three deployments are admitted" \
    "${deploy_created:-0} of 3 were created, so every row below would report an absence produced by \
the manifest rather than by the operator: $(echo "$deploy_out" | tr '\n' ' ' | cut -c1-220)"
  kvi_results "$CASE_ID"
  exit $?
fi

# The value is read off the Pod SPEC, which the operator writes at render time -- not off a running
# container. A Pod that never starts still carries everything this case asserts.
rendered_tenant() {
  local md="$1" pod=""
  for _ in $(seq 1 40); do
    pod="$(kubectl -n "$TEST_NS" get pods \
      -l app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance="$md" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [ -n "$pod" ] && break
    sleep 3
  done
  [ -z "$pod" ] && return 1

  kubectl -n "$TEST_NS" get pod "$pod" -o \
    jsonpath='{.spec.containers[?(@.name=="main")].env[?(@.name=="MOONCAKE_TENANT_ID")].value}' 2>/dev/null
}

TA="$(rendered_tenant case47-a)"
TB="$(rendered_tenant case47-b)"
TC="$(rendered_tenant case47-c)"

# EVERY ASSERTION BELOW GUARDS FOR EMPTY FIRST. An engine that stopped forwarding a tenant, a
# renderer that stopped emitting one, or a Pod that was never created all produce an empty string --
# and "" equals "" would report the sharing half as PASS while nothing had been rendered at all.
if [ -z "$TA" ] || [ -z "$TB" ]; then
  record FAIL "the operator renders a tenant for an engine that reads one" \
    "MOONCAKE_TENANT_ID is empty on case47-a ('${TA}') or case47-b ('${TB}'); either the renderer \
stopped emitting it, the facts table flipped for sglang, or no replica was rendered"
elif [ "$TA" = "$TB" ] && [ "$TA" = "$DOMAIN" ]; then
  record PASS "two deployments on one Binding are rendered the same reuse domain" \
    "both carry MOONCAKE_TENANT_ID=${TA}, which is the Binding's own domain — sharing is a rendered \
consequence of the Binding rather than a coincidence of defaults"
else
  record FAIL "two deployments on one Binding are rendered the same reuse domain" \
    "case47-a='${TA}' case47-b='${TB}' expected both to be '${DOMAIN}'"
fi

if [ -z "$TC" ]; then
  record FAIL "a deployment on a different Binding is rendered a different domain" \
    "MOONCAKE_TENANT_ID is empty on case47-c, so this row has no value to compare"
elif [ "$TC" = "$OTHER_DOMAIN" ] && [ "$TC" != "$TA" ]; then
  record PASS "a deployment on a different Binding is rendered a different domain" \
    "case47-c carries ${TC} against ${TA} on the pair above — the domain follows the Binding, so the \
API's own semantics are visible on the Pod before any engine runs"
else
  record FAIL "a deployment on a different Binding is rendered a different domain" \
    "case47-c='${TC}' expected '${OTHER_DOMAIN}' and expected it to differ from '${TA}'"
fi

# --- the store side: does the tenant the operator rendered actually partition anything ---

# GUARDED ON THE VALUES IT IS ABOUT TO USE. With an empty TA or TC the probe below would call setup()
# with an empty tenant id, and the client either errors -- reported as "nothing was measured" -- or
# succeeds under the store's own default and produces a FAIL row about ISOLATION that is really the
# render failure above wearing a different name. Two failures conflated into one row is worse than
# one row that says it did not run.
if [ -z "$TA" ] || [ -z "$TC" ]; then
  record SKIP "the rendered tenant partitions the store" \
    "no tenant was rendered (case47-a='${TA}', case47-c='${TC}'), so the store side has nothing to \
partition on — this is the row above failing, not an isolation result"
  kvi_results "$CASE_ID"
  exit $?
fi

kubectl apply -f - <<YAML >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: case47-probe
  namespace: ${TEST_NS}
spec:
  restartPolicy: Never
  containers:
  - name: client
    image: ${CLIENT_IMAGE}
    command: ["python3", "-c", "import time; time.sleep(3600)"]
YAML
if ! kvi_wait_for pods case47-probe '{.status.phase}' Running 240 "$TEST_NS" >/dev/null; then
  record SKIP "the rendered tenant partitions the store" \
    "the probe Pod did not reach Running in 240s, so the store side was never exercised — this is \
an environment failure and NOT evidence either way about partitioning"
else
  # Written under the tenant the operator rendered for the PAIR, then read back under that same
  # tenant and again under the tenant it rendered for the third deployment. The second read is the
  # discriminating one: a store that ignores tenants returns the payload both times.
  PAYLOAD="case47-$(date -u +%s)"
  LOG="$(mktemp)"
  kubectl -n "$TEST_NS" exec case47-probe -c client -- python3 -c "
from mooncake.store import MooncakeDistributedStore as S
def cli(t):
    s = S()
    rc = s.setup('probe-%s' % t, 'P2PHANDSHAKE', 0, 128*1024*1024, 'tcp', '', '${ENDPOINT}', tenant_id=t)
    print('SETUP %s rc=%d' % (t, rc))
    return s
a = cli('${TA}')
print('PUT rc=%d' % a.put('${PAYLOAD}', b'${PAYLOAD}'))
print('SAME len=%d' % len(a.get('${PAYLOAD}') or b''))
c = cli('${TC}')
print('OTHER len=%d' % len(c.get('${PAYLOAD}') or b''))
# The drain, and it belongs to the teardown rather than to any assertion above: a reuse domain that
# still holds an object is one the master refuses to release the quota of, which is what makes the
# shared teardown force a finalizer on every run of this case. Retried on -706 (the write's lease has
# not expired) rather than slept through, because that TTL is a master startup parameter; -704 is
# already-absent and is done, not a failure.
# Every exit from this loop says which one it was. A silent break makes a connection error, an
# unexpected error code and a successful drain look identical -- and the one that matters is
# invisible: the object stays, the shared teardown goes back to forcing a finalizer on every run,
# and nothing in the log says why.
import time
deadline = time.time() + 60
while True:
    rc = a.remove('${PAYLOAD}')
    if rc == 0 or rc == -704:
        print('DRAIN ok rc=%d' % rc)
        break
    if rc != -706:
        print('DRAIN unexpected rc=%d' % rc)
        break
    if time.time() > deadline:
        print('DRAIN gave-up rc=-706 after 60s; the write lease had not expired')
        break
    time.sleep(3)
" >"$LOG" 2>&1

  same="$(grep -m1 '^SAME len=' "$LOG" | sed 's/.*len=//')"
  other="$(grep -m1 '^OTHER len=' "$LOG" | sed 's/.*len=//')"
  if [ -z "$same" ] || [ -z "$other" ]; then
    record SKIP "the rendered tenant partitions the store" \
      "the client produced no SAME/OTHER line, so nothing was measured: $(tr '\n' ' ' <"$LOG" | cut -c1-220)"
  elif [ "$same" -gt 0 ] && [ "$other" -eq 0 ]; then
    record PASS "the rendered tenant partitions the store" \
      "a payload written under ${TA} read back ${same} bytes under that tenant and 0 under ${TC} — \
the value the operator rendered is what the store partitions on"
  elif [ "$same" -gt 0 ]; then
    record FAIL "the rendered tenant partitions the store" \
      "the payload was readable under BOTH ${TA} and ${TC} (${other} bytes), so a second Binding is \
not an isolation boundary at this store version — the API semantics hold and the enforcement does not"
  else
    record FAIL "the rendered tenant partitions the store" \
      "the write did not round-trip under its own tenant (${same} bytes), so this run says nothing \
about isolation: $(tr '\n' ' ' <"$LOG" | cut -c1-220)"
  fi
  rm -f "$LOG"
fi

kvi_results "$CASE_ID"
