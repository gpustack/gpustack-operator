#!/usr/bin/env bash
#
# CASE 58 — The installed selector is the inject label, read off the cluster rather than the source
#   (READ-ONLY)
#
#   case-58.sh <NS>
#
# Goal:        A unit test asserts what the GENERATOR produces. This one asserts what the API server
#              is actually running, which is a different claim: a chart that renders the wrong
#              configuration, an install that patched one by hand, or a webhook whose entry never got
#              registered all pass the unit test and fail here.
#
#              The selector reverting to the queue-name label is named in the spec's Risks, and it is
#              the failure that breaks nothing visible: every Pod the narrowed selector still matched
#              would still be injected correctly, and the ones that stopped being served would simply
#              never mention it.
#
# Environment: a cluster with the operator installed. Reads the installed
#              MutatingWebhookConfiguration and nothing else. Never auto-skips; an absent
#              configuration is a FAIL, not a skip.
# Inputs:      none - this case creates no object. It reads what the API server is actually running,
#              which is a different claim from what the generator produces.
# Expected:    the installed gpustack-worker-mutation carries two Pod entries with distinct names and
#              paths; the KV cache one selects kvcache.gpustack.ai/inject In [true]; it fails closed
#              and is never reinvoked; and no second configuration object exists to enter the API
#              server's ordering under its own name.
#
#              SKIPS: none. An absent MutatingWebhookConfiguration is a FAIL, not a skip: this case
#              exists to catch a webhook that was never registered, and skipping on exactly that
#              condition would make it blind to the thing it is for.
# Cleanup:     none needed; the case is read-only and installs no trap.
#
# EXERCISED 2026-09-04 on a three-node k3s cluster, deliberately in both directions. Run BEFORE the
# operator carrying this webhook was deployed, it failed at the first check and said the entry was
# absent - which is the one run that proves the guard below is doing something, since without it every
# later field would have read empty and been taken for an unset default. Run after, all eight pass.
set -uo pipefail

NS="${1:?usage: case-58.sh <NS>}"
CASE_ID=58
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# No kvi_setup and no trap: nothing here creates an object.
CFG=gpustack-worker-mutation
KVC=mutate.gpustack-worker-kvcache.core.v1.pod
ACC=mutate.gpustack-worker.core.v1.pod

if ! kubectl get mutatingwebhookconfiguration "$CFG" >/dev/null 2>&1; then
  record FAIL "the mutating configuration is installed" \
    "no MutatingWebhookConfiguration named ${CFG}; nothing below can be read"
  kvi_results "$CASE_ID"; exit 1
fi

# jp reads one field of the KV cache entry by NAME rather than by index. An index would silently move
# the day another webhook is added to the same object, and read a different entry's value as this
# one's.
jp() {
  kubectl get mutatingwebhookconfiguration "$CFG" \
    -o "jsonpath={.webhooks[?(@.name=='${KVC}')]$1}" 2>/dev/null
}

# The ENTRY has to be proven present before any field's emptiness can mean anything. Every jp below
# returns empty for a missing entry exactly as it does for an unset field, and one check downstream
# reads empty as "unset, so the default applies" - which would report PASS about a webhook that is not
# installed at all, in the one case whose whole purpose is catching that.
if [ -z "$(jp '.name')" ]; then
  record FAIL "the KV cache entry is registered" \
    "${CFG} exists but carries no entry named ${KVC}; the webhook is not installed, and every field \
read below would come back empty for that reason rather than because it was left unset"
  kvi_results "$CASE_ID"; exit 1
fi
record PASS "the KV cache entry is registered" "${CFG} carries ${KVC}"

key="$(jp '.objectSelector.matchExpressions[0].key')"
op="$(jp '.objectSelector.matchExpressions[0].operator')"
val="$(jp '.objectSelector.matchExpressions[0].values[0]')"

# The selector is asserted WHOLE, not just its first expression. matchExpressions are ANDed, so an
# installed selector carrying the expected first entry PLUS a queue-name requirement would satisfy an
# index-0 check while routing only Pods already in the scheduling chain - the exact regression this
# case exists to catch. Same for matchLabels, which ANDs with the expressions.
expr_count="$(jp '.objectSelector.matchExpressions[*].key' | wc -w | tr -d ' ')"
extra_labels="$(jp '.objectSelector.matchLabels')"
if [ "$expr_count" = 1 ] && [ -z "$extra_labels" ]; then
  record PASS "the selector carries nothing beyond the inject label" \
    "exactly one matchExpression and no matchLabels, so no second requirement narrows what reaches \
this webhook"
else
  record FAIL "the selector carries nothing beyond the inject label" \
    "found ${expr_count} matchExpression(s) and matchLabels '${extra_labels:-<none>}'; an ANDed \
second requirement would silently narrow the webhook to a subset of Pods"
fi

if [ "$key" = "kvcache.gpustack.ai/inject" ] && [ "$op" = "In" ] && [ "$val" = "true" ]; then
  record PASS "the installed selector is the inject label" "${key} ${op} [${val}]"
elif [ "$key" = "kueue.x-k8s.io/queue-name" ]; then
  record FAIL "the installed selector is the inject label" \
    "it is the QUEUE-NAME label - this webhook is now serving only Pods already in this project's \
scheduling chain, which is the exact case the spec exists to serve the opposite of"
else
  record FAIL "the installed selector is the inject label" \
    "selector is '${key:-<absent>} ${op:-} [${val:-}]'"
fi

fp="$(jp '.failurePolicy')"
if [ "$fp" = Fail ]; then
  record PASS "it fails closed" \
    "failurePolicy=Fail: with Ignore, a webhook outage starts opted-in Pods silently without a cache"
else
  record FAIL "it fails closed" "failurePolicy='${fp:-<absent>}'"
fi

# Empty here means "the field was not set, so the API server's default of Never applies" - and it can
# only mean that because the entry was proven present above. Without that guard the same emptiness
# would also be what a missing entry returns.
rp="$(jp '.reinvocationPolicy')"
if [ -z "$rp" ] || [ "$rp" = Never ]; then
  record PASS "it is never reinvoked" \
    "reinvocationPolicy='${rp:-Never (the default)}': a second pass would re-enter Default on this \
webhook's own output, which its conflict rule then refuses"
else
  record FAIL "it is never reinvoked" "reinvocationPolicy='${rp}'"
fi

se="$(jp '.sideEffects')"
if [ "$se" = None ]; then
  record PASS "it declares no side effects, truthfully" \
    "sideEffects=None, and the file it injects is a downwardAPI projection rather than a ConfigMap it \
would have to create"
else
  record FAIL "it declares no side effects, truthfully" "sideEffects='${se:-<absent>}'"
fi

kvc_path="$(jp '.clientConfig.service.path')"
acc_path="$(kubectl get mutatingwebhookconfiguration "$CFG" \
  -o "jsonpath={.webhooks[?(@.name=='${ACC}')].clientConfig.service.path}" 2>/dev/null)"
if [ -n "$kvc_path" ] && [ -n "$acc_path" ] && [ "$kvc_path" != "$acc_path" ]; then
  record PASS "the two Pod entries have distinct paths" "${kvc_path} and ${acc_path}"
else
  record FAIL "the two Pod entries have distinct paths" \
    "kvcache='${kvc_path:-<absent>}' accelerator='${acc_path:-<absent>}' - a shared path puts two \
handlers on one mux route"
fi

# A second configuration object would enter the API server's ordering under its own name, and the
# prefix on this one sorts before Kueue's deliberately.
others="$(kubectl get mutatingwebhookconfiguration -o name 2>/dev/null \
  | grep -c 'gpustack-worker' || true)"
if [ "${others:-0}" -eq 1 ]; then
  record PASS "there is exactly one gpustack-worker mutating configuration" \
    "both Pod entries live in ${CFG}, whose name sorts before kueue-"
else
  record FAIL "there is exactly one gpustack-worker mutating configuration" \
    "found ${others:-0}; a second one orders itself independently of this one"
fi

kvi_results "$CASE_ID"
