#!/usr/bin/env bash
#
# CASE 55 — The stamp that replaces isolation, and the control that makes it mean something
#   (MUTATING, self-recovering)
#
#   case-55.sh <NS>
#
# Goal:        This is the honesty gate. A Binding declares a reuse domain, and the webhook writes
#              that domain into every engine it configures - the vLLM family reads it as `tenant_id`
#              from its projected client config, SGLang as MOONCAKE_TENANT_ID from its environment.
#              The webhook never refuses over whether the engine BUILD will honour it - refusing would
#              mean one namespace's Binding stops another namespace's Pods, and a Pod's author caused
#              none of it - so the injection record is where the decision is visible.
#
#              What the record says is deliberately an ACTION and not an outcome: `tenantInjected`,
#              never "isolated". Whether an injected tenant takes effect depends on the engine BUILD,
#              and admission never inspects the image, so no engine version is stamped anywhere -
#              a record naming one would claim a fact nothing measured. A Pod handed a variable its
#              build predates would otherwise be stamped as isolated while sharing a cache.
#              Over-claiming in that direction is the failure this case exists to prevent.
#
#              The control is the ARTIFACT, per engine. Every engine now receives the tenant, so a
#              stamp reading "injected" everywhere is the correct answer rather than a suspicious one;
#              what separates it from a stamp hard-coded that way is that each engine's own vehicle
#              carries the Binding's domain, and the OTHER engine's vehicle does not. A webhook that
#              stamped the action without writing it, or wrote it where the engine does not read it,
#              fails on one side or the other.
#
# Environment: a cluster with the operator installed and a node able to run the Mooncake image. The
#              case stands up its own KVCacheBackend (multi-tenancy on, TCP), KVCachePool, namespace
#              and one KVCachePoolBinding, so it needs no pool to pre-exist. Never auto-skips.
# Inputs:      Pods on each accepted engine value against the real domain-carrying Binding. Nothing
#              is mocked.
# Expected:    a domain-carrying Binding is INJECTED, never refused; the record carries the domain and
#              no engine version; a vLLM container receives the Binding's domain as `tenant_id` in its
#              client config and no MOONCAKE_TENANT_ID, while an SGLang container receives it as
#              MOONCAKE_TENANT_ID; and for every engine this fixture can reach the stamp and the
#              artifact agree.
#
#              NOT COVERED HERE: the other answer, tenantInjected=false. It is reached only through a
#              pool whose master holds no tenant ledger (QuotaLedgerAvailable False for
#              MultiTenancyDisabled), and this fixture builds a multi-tenant one; the renderer and the
#              resolver pin it in TestRender_TenantOmittedForAnEmptyDomain and
#              TestPodKVCacheResolve_QuotaLedgerGate.
#
#              ONE SKIP, and it is MEASURED rather than declared: the vLLM-Ascend row of the control
#              loop. That value is no longer one the engine annotation takes, and the spelling that
#              replaces it needs an ascend-transport pool this fixture does not build - so the row is
#              unreachable rather than unlucky. The loop does not take that on trust: it submits the
#              row as a server-side dry run and skips only on an actual refusal, printing what the
#              server said, so the skip retires itself if either refusal is ever lifted. A dry run
#              that fails for any OTHER reason is a FAILURE, not a skip. The count the loop is held
#              to is derived from what was skipped for the same reason, and a count that does not add
#              up is recorded rather than passed over. An engine whose Pod never appears is still a
#              FAILURE.
# Cleanup:     the trap removes the Pods, the Binding, the namespace, the pool and the backend, in
#              that order and idempotently, on pass AND fail. The Binding is given 60s before its
#              finalizer is forced: a domain still holding objects makes the master refuse to drop
#              its quota, and forcing it earlier is how a run leaves a namespace Terminating forever.
#              It changes no shared baseline - every object it touches is one it created.
#
# The table has caught an engine moving sides three times, which is the whole point of it reading
# every engine: an engine that changes sides turns this red rather than sliding through. Every time
# the case failed before the oracle was updated, never after - the third was the tenant becoming
# unconditional for every engine, when the vLLM rows here still expected none.
#
# What this case does NOT check, and never did: which connector name is rendered. Every Pod here runs
# a stub image, so no engine ever resolves that name. The engine loop is three annotation values, not
# three engines. CASE 60 is where the name is checked against a real factory.
set -uo pipefail

NS="${1:?usage: case-55.sh <NS>}"
CASE_ID=55
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# Armed BEFORE setup, not after: kvi_setup creates the cluster-scoped backend on its first line and
# has five failure exits after that, so arming afterwards leaks whatever a failed setup left behind.
trap kvi_teardown EXIT
kvi_setup || { kvi_results "$CASE_ID"; exit 1; }

kvi_pod_manifest stamped vllm | kubectl apply -f - >/dev/null 2>&1
if ! kvi_wait_for pods stamped '{.metadata.name}' stamped 60 "$TEST_NS" >/dev/null; then
  record FAIL "a domain-carrying Binding is injected, not refused" \
    "the Pod never appeared - if the webhook refused it, the isolation gap has become a denial of \
service for a Pod whose author did not create the domain"
  kvi_results "$CASE_ID"; exit 1
fi
record PASS "a domain-carrying Binding is injected, not refused" \
  "the Pod was admitted; the gap is reported rather than pushed onto its author"

domain="$(kvi_stamp stamped domain)"

# There is deliberately no assertion here that the domain "is" or "is not" enforced. The stamp used to
# carry that, and it could not be honest: whether an engine honours an injected tenant depends on its
# BUILD, and admission never looks at the image. A Pod handed a variable its build predates would
# have been stamped as isolated while sharing a cache. What is asserted instead is the action taken,
# below. The same honesty rule is why no engine version is read off the stamp either: the record
# carries what admission did, never what it measured nothing about.

if [ "$domain" = "$DOMAIN" ]; then
  record PASS "the stamp names the Binding's domain" "domain=${domain}"
else
  record FAIL "the stamp names the Binding's domain" \
    "domain='${domain:-<absent>}', expected '${DOMAIN}'"
fi

# The tenant as each vehicle carries it, "config=<tenant_id in the projected client config> env=<the
# container's MOONCAKE_TENANT_ID>", either side empty when absent. Both are read for every engine,
# because the control below is that each engine's own vehicle carries the domain AND the other one
# does not: a tenant written where the engine does not read it is decoration that reads as a
# guarantee.
tenant_vehicles() {
  local pod="$1" cfg env
  cfg="$(kubectl -n "$TEST_NS" get pod "$pod" \
    -o "jsonpath={.metadata.annotations.kvcache\.gpustack\.ai/client-config}" 2>/dev/null \
    | python3 -c "import json,sys; d=sys.stdin.read().strip(); print(json.loads(d).get('tenant_id','') if d else '')" 2>/dev/null)"
  env="$(kvi_env "$pod" MOONCAKE_TENANT_ID)"
  echo "config=${cfg} env=${env}"
}

# What was DONE about the tenant. The webhook writes a non-empty domain into every engine it
# configures and leaves compatibility with the image to whoever chose it; an engine build that reads
# no tenant writes under the store's own "default" name, which is why a pool serving one needs a
# Binding registering that name. The stamp records the write, never whether isolation resulted.
injected="$(kvi_stamp stamped tenantInjected)"
if [ "$injected" = True ] || [ "$injected" = true ]; then
  record PASS "the stamp records the tenant ACTION, and for vLLM it is 'written'" \
    "tenantInjected=${injected}: the domain was written into this engine's client config. The field \
says what was DONE, never whether isolation resulted - that depends on the engine build, which is not \
checked here"
else
  record FAIL "the stamp records the tenant ACTION, and for vLLM it is 'written'" \
    "tenantInjected='${injected:-<absent>}' on a vLLM Pod against a domain-carrying Binding; every \
engine is given the tenant"
fi

# The vLLM family's vehicle is the projected client config, and only that: MOONCAKE_TENANT_ID is
# SGLang's, and this engine does not read it.
got="$(tenant_vehicles stamped)"
if [ "$got" = "config=${DOMAIN} env=" ]; then
  record PASS "the reuse domain reaches a vLLM container through its client config" \
    "tenant_id=${DOMAIN} in the projected config, and no MOONCAKE_TENANT_ID - that variable is \
SGLang's vehicle and this engine does not read it"
else
  record FAIL "the reuse domain reaches a vLLM container through its client config" \
    "read ${got}; expected config=${DOMAIN} and no environment variable"
fi

# The paired half, on its own Pod because the engine is fixed per Pod.
kvi_pod_manifest sg-stamped sglang | kubectl apply -f - >/dev/null 2>&1
if ! kvi_wait_for pods sg-stamped '{.metadata.name}' sg-stamped 60 "$TEST_NS" >/dev/null; then
  record FAIL "the reuse domain reaches an SGLang container as its tenant" \
    "the Pod was never stored, so the paired half did not run and the vLLM half above is unpaired: \
nothing here shows the webhook routes the tenant by engine at all"
else
  sg_tenant="$(kvi_env sg-stamped MOONCAKE_TENANT_ID)"
  if [ "$sg_tenant" = "$DOMAIN" ]; then
    record PASS "the reuse domain reaches an SGLang container as its tenant" \
      "MOONCAKE_TENANT_ID=${sg_tenant}, which is the Binding's own reuse domain"
  else
    record FAIL "the reuse domain reaches an SGLang container as its tenant" \
      "MOONCAKE_TENANT_ID='${sg_tenant:-<absent>}', expected '${DOMAIN}'"
  fi
fi

# THE CONTROL. It used to require both stamp answers to appear, because the compiled table then held
# both - some engines were given a tenant and some were not. The tenant is unconditional now, so
# "injected" on every engine is the correct reading and the table has one answer; requiring two would
# fail against a correct webhook. What still separates a real decision from a hard-coded one is the
# artifact: each engine carries the domain in its OWN vehicle and not in the other's, so the loop
# requires the stamp and the artifact to agree per engine, and requires BOTH vehicles to have been
# observed across the engines it reached.
ok=1
checked=0
skipped=0
seen_config=0
seen_env=0
for engine in vllm vllm-ascend sglang; do
  pod="ctl-${engine//-/}"
  # vLLM-Ascend is not reachable from this fixture, and TWO refusals stand in front of it - neither
  # of them about the launch:
  #
  #  1. The engine annotation does not take the value. `vllm_ascend` was ruled the package the
  #     runner installs when the accelerator backend is CANN rather than an engine anybody names, so
  #     ParseEngine refuses it and names `engine: vllm` + `manufacturer: ascend` as the spelling.
  #  2. Spelled that way it is refused one layer down: that runtime accepts only the `ascend`
  #     transport, and this family's pool is built on a TCP backend.
  #
  # THE SKIP IS ASKED FOR, NOT ASSERTED. Both refusals live in the webhook, which is the moving part
  # here - so this submits the row's own manifest as a server-side dry run and skips only on an
  # actual refusal. Hard-coding it would have written a claim about admission into this file, where
  # it would go stale silently: the row would keep skipping, `checked` would keep adding up, and the
  # case would stay green while covering one engine less than it says. Asked this way the skip
  # retires itself the day either refusal is lifted, and the reason it prints is the server's own.
  #
  # It is asked before the expectation below is consulted, because the expectation is not what stops
  # the row - and it is a SKIP rather than a FAIL naming a Pod that never appeared, which is what a
  # reader would otherwise go and chase.
  #
  # The Go side pins the answer where the engine IS reachable: TestRender_TenantGoesToEveryEngineThatReadsOne
  # renders every engine, vLLM-Ascend included. What no test replaces is the cluster half.
  if [ "$engine" = vllm-ascend ]; then
    why="$(kvi_admission_refuses "$(kvi_pod_manifest "$pod" "$engine")")" && rc=0 || rc=$?
    case "$rc" in
      0)
        skipped=$((skipped + 1))
        record SKIP "the stamp and the artifact agree per engine (${engine})" \
          "admission refuses this row on this fixture, so its tenant is UNVERIFIED on a cluster - it \
is pinned only in the inject package's own test (TestRender_TenantGoesToEveryEngineThatReadsOne). The \
server said: ${why}"
        continue
        ;;
      2)
        ok=0
        record FAIL "the stamp and the artifact agree per engine" \
          "engine ${engine}: the dry run failed for something other than this webhook's refusal, so \
whether the row is reachable was never established and it was neither run nor skipped: ${why}"
        continue
        ;;
    esac
    # rc=1: admission accepts it now, so the row RUNS like any other - which is the whole point of
    # asking rather than deciding here.
  fi
  # The vehicle this engine reads the tenant from; the other one must stay empty.
  case "$engine" in
    sglang) want="config= env=${DOMAIN}" ;;
    *)      want="config=${DOMAIN} env=" ;;
  esac
  kvi_pod_manifest "$pod" "$engine" | kubectl apply -f - >/dev/null 2>&1
  if ! kvi_wait_for pods "$pod" '{.metadata.name}' "$pod" 60 "$TEST_NS" >/dev/null; then
    # A control that did not run is recorded, never passed over. Skipping it silently would leave a
    # smaller set of engines checked while the summary below still claimed all of them.
    ok=0
    record FAIL "the stamp and the artifact agree per engine" \
      "engine ${engine} never produced a Pod, so its answer was not observed at all"
    continue
  fi
  checked=$((checked + 1))
  stamp="$(kvi_stamp "$pod" tenantInjected)"
  got="$(tenant_vehicles "$pod")"
  case "$stamp" in
    True | true) ;;
    *) ok=0; record FAIL "the stamp and the artifact agree per engine" \
         "engine ${engine} stamped tenantInjected='${stamp:-<absent>}' against a domain-carrying Binding"
       continue ;;
  esac
  if [ "$got" != "$want" ]; then
    ok=0
    record FAIL "the stamp and the artifact agree per engine" \
      "engine ${engine} stamped the write, but its vehicles read ${got}; expected ${want}"
    continue
  fi
  case "$engine" in
    sglang) seen_env=1 ;;
    *)      seen_config=1 ;;
  esac
done
# The count the loop is held to is DERIVED from what was skipped, never written as a constant. The
# skip above is asked of admission and can stop firing, and a hard-coded 2 would then leave a third
# row running with nothing holding the loop to it - no PASS, no FAIL, the case green by omission.
if [ "$ok" -eq 1 ] && [ "$checked" -eq $((3 - skipped)) ]; then
  if [ "$seen_config" -eq 1 ] && [ "$seen_env" -eq 1 ]; then
    record PASS "the stamp and the artifact agree per engine" \
      "${checked} of 3 engines observed and ${skipped} skipped as unreachable; each stamped the write \
and carried the domain in its own vehicle only, and BOTH vehicles appeared - which is what separates a \
per-engine write from a stamp hard-coded to 'injected'. A skipped engine is NOT part of this claim"
  else
    record FAIL "the stamp and the artifact agree per engine" \
      "all ${checked} observed but only one vehicle appeared, so this run cannot tell a per-engine \
write from one written the same way everywhere"
  fi
elif [ "$ok" -eq 1 ]; then
  # Counts that do not add up, with no FAIL from the loop to explain them. Recorded rather than
  # passed over, because the loop's own rule is that a control which did not run is never dropped in
  # silence - and this is the one arm where that could otherwise happen.
  record FAIL "the stamp and the artifact agree per engine" \
    "observed ${checked} engines and skipped ${skipped} of 3, which do not add up: the reachable set \
changed under the loop and no row reported it"
fi

kvi_results "$CASE_ID"
