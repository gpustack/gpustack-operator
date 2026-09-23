#!/usr/bin/env bash
#
# Delete the custom resources this operator owns, in consumption order, and wait for their
# finalizers to clear WHILE THE CONTROLLERS THAT CLEAR THEM ARE STILL RUNNING.
#
#   drain.sh <NS> [BUDGET_SECONDS]
#
# This is the half cleanup.sh structurally cannot do. cleanup.sh runs as a post-delete hook, after
# the release's workloads are gone, so by the time it reaches a finalizer there is no controller
# left to honour it and it strips the finalizer instead. Stripping SKIPS whatever that finalizer was
# holding for, and one of those is not bookkeeping: a KVCacheBackend's teardown starts a Pod on
# every node that empties that node's cache tier, so a stripped finalizer leaves the cached blocks
# sitting on the disk with nothing left that knows they are there. A KVCachePoolBinding's teardown
# deletes that tenant's entry from the master's quota ledger. Neither has another trigger.
#
# Used in two places, both reusing this one file as the single source of truth:
#   - the chart's gated pre-delete hook Job (cleanupOnUninstall=true) runs it in-cluster with the
#     operator image, before helm removes the release's own resources;
#   - the e2e teardown.sh runs it on the host, before its `helm uninstall`.
#
# IT ALWAYS EXITS 0. As a pre-delete hook a non-zero exit aborts the uninstall, which would leave an
# operator unable to remove a release because some object would not drain - the opposite of what
# this is for. What it cannot drain it reports and leaves to cleanup.sh, which strips and deletes
# regardless. A caller that needs a verdict re-asks the cluster itself.
#
# Idempotent and safe to re-run. It NEVER deletes namespaces and never touches a CRD; deleting the
# CRDs is cleanup.sh's step 4, and doing it here would destroy the objects this is waiting on.
set -uo pipefail

NS="${1:-${GPUSTACK_NAMESPACE:-gpustack-system}}"

# The whole-run budget, not a per-kind one. A KVCacheBackend's tier cleanup is unbounded by nature -
# it waits on a Pod per node, and a node that is unreachable holds the object until that node's own
# deadline expires - so a per-kind timeout large enough for the slow case multiplies across six
# kinds into a wait no uninstall should take. One budget spent in order means the kinds that drain
# in seconds leave the rest of it to the one that does not.
#
# The DEFAULT is the host caller's, where nothing else is counting. The chart's pre-delete hook
# passes a much smaller one and does not inherit this: helm waits for a delete hook for --timeout,
# five minutes by default, and then FAILS the uninstall - so a budget past that turns a slow drain
# into a release that cannot be removed. Either way, reaching the budget must produce a REPORT from
# this script rather than a killed Pod, because a killed hook Pod is a failed hook.
BUDGET="${2:-600}"
DEADLINE=$(( $(date +%s) + BUDGET ))

echo "[drain] namespace=${NS} budget=${BUDGET}s"

# Preflight. Every step below swallows errors by design and the script always exits 0, so against a
# wrong or absent context it would report "done" having drained nothing - and the uninstall would
# then proceed to strip finalizers that never ran. Refuse to run blind, and name the context.
# The check carries NO --request-timeout. That flag, like --server or --token, changes the
# client configuration, and kubectl then stops falling back to the in-cluster one: inside the
# hook Pod it would dial localhost:8080 and fail every time. timeout(1) bounds it instead,
# where there is one.
healthz=(kubectl get --raw=/healthz)
if command -v timeout >/dev/null 2>&1; then
  healthz=(timeout 10 "${healthz[@]}")
fi
if ! "${healthz[@]}" >/dev/null 2>&1; then
  echo "[drain] SKIPPED: cannot reach the API server of the current kubectl context" \
    "($(kubectl config current-context 2>/dev/null || echo 'none set')); nothing was drained" >&2
  exit 0
fi
echo "[drain] context=$(kubectl config current-context 2>/dev/null || echo unknown)"

# The order is DECLARED and not discovered, and that is the one thing this script cannot infer.
# cleanup.sh discovers its kinds precisely because order does not matter there - it strips and
# deletes everything. Here order IS the semantics: a KVCacheBackend refuses to finish while its
# status.usedBy still names a pool, and a pool while a Binding still names it, so deleting a backend
# first produces an object that waits for consumers nobody has asked to leave. Consumers first,
# then what they consume.
#
# A kind missing from this list is NOT missed entirely - drain_remaining below sweeps whatever is
# left in the gpustack groups, unordered. That fallback is why forgetting to add a kind here costs
# ordering rather than correctness, and it is deliberately not a reason to skip adding one.
DRAIN_ORDER="
instances.worker.gpustack.ai
modeldeployments.worker.gpustack.ai
kvcachepoolbindings.worker.gpustack.ai
kvcachepools.worker.gpustack.ai
kvcachebackends.worker.gpustack.ai
instancetypes.worker.gpustack.ai
"

# Address every kind as plural.VERSION.group read off its own CRD, never as a bare plural.
#
# The worker registers an aggregated worker.gpustack.ai/v1 API, and an unversioned name resolves to
# THAT rather than to the CRD behind it. Here the worker is still running, so the proxy answers -
# which is worse than cleanup.sh's case, where it is merely unreachable: the answer comes from a
# different surface than the one whose objects carry the finalizers this script is waiting on.
# Reading the storage version off the CRD and putting it back into the name addresses the CRD.
resource_of() { # crd-name -> plural.VERSION.group, empty when the CRD is absent
  kubectl get crd "$1" \
    -o jsonpath='{.spec.names.plural}.{.spec.versions[?(@.storage==true)].name}.{.spec.group}' \
    2>/dev/null
}

# List one kind as namespace/name lines.
#
# Separated by "/", which neither a namespace nor a name may contain, and NOT by a space: a
# cluster-scoped object has an empty namespace, so a space-separated line starts with a blank field
# that `read` folds away, putting the NAME into the namespace variable and dropping the object.
# KVCacheBackend, KVCachePool and InstanceType are all cluster-scoped, so that is three of the six.
# Its EXIT STATUS is load-bearing and every caller checks it. A failed list prints nothing, and
# nothing is also what a drained kind prints - so reading the output alone turns a transient API
# error, an expired token or a mid-run RBAC change into the answer "this kind is empty", and the run
# reports `done` over objects that still hold live finalizers. The preflight cannot cover this: it
# proves the API server answered once, at the start, not that it keeps answering.
list_objects() { # resource [selector] -> "ns/name" lines; NON-ZERO when the list itself failed
  # shellcheck disable=SC2086
  kubectl get "$1" -A ${2:+-l "$2"} \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null
}

remaining_left() { # seconds left in the budget, never negative
  local left=$(( DEADLINE - $(date +%s) ))
  [ "${left}" -gt 0 ] && echo "${left}" || echo 0
}

# Delete every object of one kind and wait for the kind to empty.
#
# Deleted with --wait=false and waited for separately, because `kubectl delete --wait` blocks per
# object: a hundred Instances would be waited for one after another, and the first slow one would
# consume a budget the rest never get to use. Issuing every delete first lets the controllers work
# on all of them at once, which is how they run anyway.
#
# The optional selector narrows only the WAIT, never the delete: every object of the kind is
# deleted, and the kind counts as drained once nothing matching the selector is left.
drain_kind() { # resource [wait-selector]
  local res="$1" wait_sel="${2:-}" objects listing left refused=0 ns name
  if ! objects="$(list_objects "${res}")"; then
    echo "[drain] INCOMPLETE: ${res} could not be listed, so whether it drained is unknown" >&2
    return 1
  fi
  [ -n "$(printf '%s' "${objects}" | tr -d '[:space:]')" ] || return 0

  # A REFUSED DELETE AND A SLOW ONE ARE THE SAME THING FROM THE WAIT LOOP, which is why the refusals
  # are counted here. An RBAC error or an admission webhook rejecting the delete leaves the object in
  # place forever; the loop below then spends the entire budget and reports "still has objects", and
  # an operator reading that goes looking at finalizers. Counted and named, the report can say which
  # of the two it was.
  #
  # Fed by a here-document rather than a pipe, because a pipeline runs the loop in a subshell where
  # the counter it increments does not survive.
  echo "[drain] deleting $(printf '%s\n' "${objects}" | grep -c . ) ${res}"
  while IFS='/' read -r ns name; do
    [ -n "${name}" ] || continue
    # shellcheck disable=SC2086
    if ! kubectl delete "${res}" "${name}" ${ns:+-n "${ns}"} \
        --ignore-not-found --wait=false >/dev/null 2>&1; then
      refused=$(( refused + 1 ))
      echo "[drain] the delete of ${ns:+${ns}/}${name} was refused; it will not drain" >&2
    fi
  done <<EOF
${objects}
EOF

  while :; do
    left="$(remaining_left)"
    [ "${left}" -gt 0 ] || break
    # A failed question is not an answer, so keep asking rather than reading the empty output as
    # "drained". The budget is what ends this either way.
    #
    # Captured into a separate variable first, so that `objects` still holds the LAST SUCCESSFUL
    # listing when the report below prints it. Assigning the call's output directly would overwrite
    # it with the empty string a failed kubectl produces, and the operator would get an INCOMPLETE
    # header naming nothing - the one moment the names matter most.
    if listing="$(list_objects "${res}" "${wait_sel}")"; then
      objects="${listing}"
      [ -n "$(printf '%s' "${objects}" | tr -d '[:space:]')" ] || return 0
    fi
    sleep 3
  done

  if [ "${refused}" -gt 0 ]; then
    echo "[drain] INCOMPLETE: ${res} had ${refused} delete(s) refused, so it was never going to" \
      "drain; the objects still present are:" >&2
  else
    echo "[drain] INCOMPLETE: ${res} still has objects when the budget ran out:" >&2
  fi
  printf '%s\n' "${objects}" | sed 's|^/||; s|^|[drain]   |' >&2
  return 1
}

incomplete=""
for crd in ${DRAIN_ORDER}; do
  res="$(resource_of "${crd}")"
  # An absent CRD is the ordinary case for a release that never used that feature, and a CRD whose
  # storage version could not be read is not something to guess a name for.
  case "${res}" in
    ""|.*|*..*) continue ;;
  esac
  # A derived InstanceType is deleted once and not waited for. While the worker runs it authors the
  # type again from its Node a few seconds after the delete completes, so the kind never empties
  # and the wait would only spend the whole budget. The delete still starts the type's teardown
  # while the worker runs. The recreated type is left to the later steps: drain-kueue.sh, where this
  # release ships Kueue, stops the worker and deletes the queue it backs, and cleanup.sh strips its
  # finalizer.
  wait_sel=""
  if [ "${crd}" = "instancetypes.worker.gpustack.ai" ]; then
    wait_sel="schedule.gpustack.ai/derived-from-node!=true"
  fi
  drain_kind "${res}" "${wait_sel}" || incomplete=yes
done

# Sweep whatever is left in the gpustack groups, in whatever order `kubectl get crd` returns.
#
# Kueue and NFD are NOT included, and the exclusion is not symmetry with cleanup.sh's ownership
# check: those controllers are subcharts with their own teardown, and deleting a user's Workloads
# and ClusterQueues is not something an operator uninstall was asked to do. cleanup.sh reaches them
# only to strip finalizers off objects whose CRDs it is about to delete as their owner.
#
# The list is BUILT FIRST and consumed after, rather than piped into the loop. A pipeline puts the
# loop in a subshell, where setting `incomplete` has no effect outside it - the earlier revision
# worked around that by having the loop print a verdict word for the parent to match, a protocol
# that held only as long as nothing else in drain_kind ever reached stdout. Building the list first
# removes the protocol instead of documenting it.
seen=""
for crd in ${DRAIN_ORDER}; do seen="${seen} ${crd}"; done
remaining_crds="$(kubectl get crd -o name 2>/dev/null \
  | sed 's|^customresourcedefinition\(s\)\{0,1\}\.apiextensions\.k8s\.io/||' \
  | grep -E '\.(worker\.)?gpustack\.ai$')"
while read -r crd; do
  [ -n "${crd}" ] || continue
  case " ${seen} " in *" ${crd} "*) continue ;; esac
  res="$(resource_of "${crd}")"
  case "${res}" in
    ""|.*|*..*) continue ;;
  esac
  drain_kind "${res}" || incomplete=yes
done <<EOF
${remaining_crds}
EOF

if [ -n "${incomplete}" ]; then
  echo "[drain] INCOMPLETE after ${BUDGET}s: the objects above were not drained, so the teardown" >&2
  echo "[drain] their finalizers were holding for did not run. The uninstall continues and" >&2
  echo "[drain] cleanup.sh will strip and delete them; anything they would have released on a node" >&2
  echo "[drain] or in a cache master's ledger stays there." >&2
  exit 0
fi

echo "[drain] done"
