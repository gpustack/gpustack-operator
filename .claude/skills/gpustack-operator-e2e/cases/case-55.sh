#!/usr/bin/env bash
#
# CASE 55 — The stamp that replaces isolation, and the control that makes it mean something
#   (MUTATING, self-recovering)
#
#   case-55.sh <NS>
#
# Goal:        This is the honesty gate. A Binding declares a reuse domain, and whether that domain
#              becomes real depends on the ENGINE: SGLang reads MOONCAKE_TENANT_ID and is given the
#              domain, while vLLM's configuration class has no tenant key at the version measured and
#              is given none. The webhook never refuses over this - refusing would mean one namespace's
#              Binding stops another namespace's Pods, and a Pod's author caused none of it - so the
#              injection record is where the decision is visible.
#
#              What the record says is deliberately an ACTION and not an outcome: `tenantInjected`,
#              never "isolated". Whether an injected tenant takes effect depends on the engine BUILD,
#              and admission never inspects the image - engineVersion on the stamp is the release the
#              facts table was measured at, not the one that will run. A Pod handed a variable its
#              build predates would otherwise be stamped as isolated while sharing a cache.
#              Over-claiming in that direction is the failure this case exists to prevent.
#
#              The control is the engine table itself. Both answers appear in it, and the loop fails
#              unless it observed both - a webhook that injected nothing, and one that injected
#              everywhere, each fail on one side. That is what a single-answer table could not do.
#
# Environment: a cluster with the operator installed and a node able to run the Mooncake image. The
#              case stands up its own KVCacheBackend (multi-tenancy on, TCP), KVCachePool, namespace
#              and one KVCachePoolBinding, so it needs no pool to pre-exist. Never auto-skips.
# Inputs:      Pods on each accepted engine value against the real domain-carrying Binding. Nothing
#              is mocked.
# Expected:    a domain-carrying Binding is INJECTED, never refused; the record carries the domain and
#              the engine version; a vLLM container receives no tenant key in any spelling while an
#              SGLang container receives the Binding's own domain as MOONCAKE_TENANT_ID; and across
#              the three accepted engines BOTH tenant answers appear.
#
#              SKIPS: none, and the control loop is written so it cannot skip by accident. An engine
#              whose Pod never appears is recorded as a FAILURE and the loop reports how many of the
#              three it actually observed, because a control that quietly did not run would otherwise
#              leave a smaller set checked while the summary still claimed all three.
# Cleanup:     the trap removes the Pods, the Binding, the namespace, the pool and the backend, in
#              that order and idempotently, on pass AND fail. The Binding is given 60s before its
#              finalizer is forced: a domain still holding objects makes the master refuse to drop
#              its quota, and forcing it earlier is how a run leaves a namespace Terminating forever.
#              It changes no shared baseline - every object it touches is one it created.
#
# EXERCISED 2026-09-04 on a three-node k3s cluster: 7 checks, all passing. NOT RE-RUN since the
# webhook began rendering AscendStoreConnector for vllm-ascend. That change does not move any
# expectation in this case - the stamp it reads is tenantInjected, and that engine still forwards no
# tenant - which is exactly why it must be re-run rather than reasoned about.
#
# The table has caught an engine moving sides TWICE, in opposite directions, which is the whole
# point of it carrying both answers: an engine that changes sides turns this red rather than sliding
# through. Both times the case failed before the oracle was updated, never after.
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
version="$(kvi_stamp stamped engineVersion)"

# There is deliberately no assertion here that the domain "is" or "is not" enforced. The stamp used to
# carry that, and it could not be honest: whether an engine honours an injected tenant depends on its
# BUILD, and admission never looks at the image - the engineVersion it stamps is the release our facts
# table was measured at, not the one the container will run. A Pod handed a variable its build
# predates would have been stamped as isolated while sharing a cache. What is asserted instead is the
# action taken, below.

if [ "$domain" = "$DOMAIN" ]; then
  record PASS "the stamp names the domain that is not in effect" "domain=${domain}"
else
  record FAIL "the stamp names the domain that is not in effect" \
    "domain='${domain:-<absent>}', expected '${DOMAIN}'"
fi

if [ -n "$version" ]; then
  record PASS "the stamp names the version the answer was measured at" \
    "engineVersion=${version}, so the record says why and not only what"
else
  record FAIL "the stamp names the version the answer was measured at" "engineVersion is absent"
fi

# What was DONE about the tenant. For an engine that reads none, the writes land on the store's own
# default, and on a multi-tenant master - the only kind a pool accepts - that name must be registered
# or every put fails with TENANT_NOT_REGISTERED rather than merely sharing a cache. Measured on a live
# cluster: omitting tenant_id and passing tenant_id="default" failed identically, a registered domain
# succeeded, and registering a Binding whose domain is "default" turned the same client's put into a
# success.
injected="$(kvi_stamp stamped tenantInjected)"
if [ "$injected" = False ] || [ "$injected" = false ]; then
  record PASS "the stamp records the tenant ACTION, and for vLLM it is 'none'" \
    "tenantInjected=${injected}: this engine reads no tenant, so its writes land on the store's own \
default and the pool needs a Binding registering that name. The field says what was DONE, never \
whether isolation resulted - that depends on the engine build, which is not checked here"
else
  record FAIL "the stamp records the tenant ACTION, and for vLLM it is 'none'" \
    "tenantInjected='${injected:-<absent>}' on a vLLM Pod, whose config class has no tenant key"
fi

# Whether a tenant is written is per ENGINE, and both directions are checked here because neither
# means anything alone: the negative half passes against a webhook that injects nothing at all, the
# positive one against a webhook that injects everywhere.
#
# vLLM's config class has no tenant key at the version measured, so writing one would be decoration
# that reads as a guarantee. SGLang's does read MOONCAKE_TENANT_ID, so withholding it would throw away
# an isolation this stack can actually have.
config="$(kubectl -n "$TEST_NS" get pod stamped \
  -o "jsonpath={.metadata.annotations.kvcache\.gpustack\.ai/client-config}" 2>/dev/null)"
whole="$(kubectl -n "$TEST_NS" get pod stamped -o json 2>/dev/null)"
if echo "${config}${whole}" | grep -qi 'tenant_id\|MOONCAKE_TENANT_ID'; then
  record FAIL "no tenant key reaches a vLLM container" \
    "a tenant appears in the injected artifacts; this engine reads none, so it would be a guarantee \
nothing keeps"
else
  record PASS "no tenant key reaches a vLLM container" \
    "neither tenant_id nor MOONCAKE_TENANT_ID appears in the config or anywhere on the Pod"
fi

# The paired half, on its own Pod because the engine is fixed per Pod.
kvi_pod_manifest sg-stamped sglang | kubectl apply -f - >/dev/null 2>&1
if ! kvi_wait_for pods sg-stamped '{.metadata.name}' sg-stamped 60 "$TEST_NS" >/dev/null; then
  record FAIL "the reuse domain reaches an SGLang container as its tenant" \
    "the Pod was never stored, so the paired half did not run and the vLLM half above is unverified: \
nothing here shows the webhook can emit a tenant at all"
else
  sg_tenant="$(kvi_env sg-stamped MOONCAKE_TENANT_ID)"
  if [ "$sg_tenant" = "$DOMAIN" ]; then
    record PASS "the reuse domain reaches an SGLang container as its tenant" \
      "MOONCAKE_TENANT_ID=${sg_tenant}, which is the Binding's own reuse domain - this engine reads \
it, so the domain is real here rather than merely declared"
  else
    record FAIL "the reuse domain reaches an SGLang container as its tenant" \
      "MOONCAKE_TENANT_ID='${sg_tenant:-<absent>}', expected '${DOMAIN}'"
  fi
fi

# THE CONTROL, and it is a real one now. This text used to say the discriminating control could not be
# built from outside the binary, because every shipped engine answered false and the table is compiled
# in - so the case settled for asserting UNIFORMITY. That stopped being true when two engines were
# re-measured as forwarding: the compiled table now holds BOTH answers, so the loop below requires
# both to appear, and a stamp hard-coded either way fails it.
#
# The description outlived the situation by one change, which is its own lesson: a note explaining why
# something is impossible does not notice when it becomes possible.
ok=1
checked=0
seen_true=0
seen_false=0
for engine in vllm vllm-ascend sglang; do
  pod="ctl-${engine//-/}"
  # What this engine is expected to have injected. Two answers appear in this table, and that is what
  # gives the loop its discriminating power: a webhook that injected nothing, or one that injected
  # everywhere, fails on one side or the other. A table with one answer could not tell either from a
  # correct one.
  case "$engine" in
    # Only SGLang reads a tenant on the path this webhook renders. vLLM-Ascend does NOT, and the
    # reason changed without the expectation changing: the webhook now renders that engine's own
    # AscendStoreConnector, and v0.19.1rc1 has no tenant anywhere (grep over vllm_ascend/, tests
    # excluded: zero hits). An earlier version of this comment said the store was tenant-aware and
    # merely unselected - both halves were wrong, and the second half predicted this row would flip
    # once we selected it. It did not.
    sglang) want=false_is_wrong; expect=True ;;
    *)      want=true_is_wrong;  expect=False ;;
  esac
  kvi_pod_manifest "$pod" "$engine" | kubectl apply -f - >/dev/null 2>&1
  if ! kvi_wait_for pods "$pod" '{.metadata.name}' "$pod" 60 "$TEST_NS" >/dev/null; then
    # A control that did not run is recorded, never passed over. Skipping it silently would leave a
    # smaller set of engines checked while the summary below still claimed all three.
    ok=0
    record FAIL "the tenant action follows the engine" \
      "engine ${engine} never produced a Pod, so its answer was not observed at all"
    continue
  fi
  checked=$((checked + 1))
  got="$(kvi_stamp "$pod" tenantInjected)"
  case "$got" in
    True | true)   seen_true=1;  [ "$expect" = True ]  || { ok=0; record FAIL "the tenant action follows the engine" \
                     "engine ${engine} stamped tenantInjected=${got}, but it reads no tenant (${want})" ; } ;;
    False | false) seen_false=1; [ "$expect" = False ] || { ok=0; record FAIL "the tenant action follows the engine" \
                     "engine ${engine} stamped tenantInjected=${got}, but it does read one (${want})" ; } ;;
    *) ok=0; record FAIL "the tenant action follows the engine" \
         "engine ${engine} stamped tenantInjected='${got:-<absent>}', which is neither answer" ;;
  esac
done
if [ "$ok" -eq 1 ] && [ "$checked" -eq 3 ]; then
  if [ "$seen_true" -eq 1 ] && [ "$seen_false" -eq 1 ]; then
    record PASS "the tenant action follows the engine" \
      "all ${checked} of 3 observed, and BOTH answers appeared - sglang injected a tenant, vllm and \
vllm-ascend did not. Seeing both is what separates this from a stamp hard-coded either way"
  else
    record FAIL "the tenant action follows the engine" \
      "all ${checked} observed but only one answer appeared, so this run cannot tell a per-engine \
decision from a constant"
  fi
fi

kvi_results "$CASE_ID"
