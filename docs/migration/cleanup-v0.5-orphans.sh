#!/usr/bin/env bash
#
# cleanup-v0.5-orphans.sh — remove the v0.5.x Kueue objects orphaned by an in-place
# `helm upgrade` of the GPUStack Operator to v0.6.x (or later).
#
# WHY THIS IS NEEDED
#   v0.6.x renamed every materialized scheduling object and dropped Cohort entirely:
#     ResourceFlavor / ClusterQueue  gpustack--<genKey>-<cpu>c-<ram>g...   (v0.5.x, DOUBLE dash)
#                                 -> gpustack-<key>-<os>-<arch>[-<n>c|-<n>d] (v0.6.x, single dash)
#     Cohort                         one per pool (v0.5.x)  ->  removed (v0.6.x)
#     InstanceType                   aggregated virtual API ->  real CRD
#   The upgraded operator indexes objects by their NEW names, so it never reconciles or
#   garbage-collects the v0.5.x-named set. `helm upgrade` runs no cleanup hook (the chart's
#   post-delete `cleanup.sh` only fires on uninstall), so the old objects leak in place —
#   dead ResourceFlavors, ClusterQueues, Cohorts and LocalQueues accumulate alongside the
#   working v0.6.x set. This script removes ONLY those orphans.
#
# WHAT IT TOUCHES
#   Only objects whose name begins with "gpustack--" (DOUBLE dash) — the v0.5.x naming.
#   v0.6.x objects are "gpustack-<key>-..." (single dash) and are never matched. It never
#   deletes namespaces, Instances, or any of the user's own resources directly. If an old
#   ClusterQueue still holds admitted workloads (a v0.5.x Instance was running across the
#   upgrade), it is drained via HoldAndDrain first — which EVICTS those workloads — before the
#   queue is removed; because the queue names changed in v0.6.x, evicted workloads must be
#   re-created under the new pool's queue. An old queue with no workloads is deleted directly.
#
# NODE LABELS
#   The stale v0.5.x node labels (.z-flavor/.z-queue/.z-cohort, generic-ln-x64, per-unit
#   .cpu/.ram/.storage) self-heal: the same-named <node>-gpustack-worker NodeFeature is
#   overwritten on upgrade and NFD drops the removed labels. This script only verifies that.
#
# USAGE
#   cleanup-v0.5-orphans.sh [--dry-run]
#     --dry-run   list what would be deleted, change nothing.
#
# Run it AFTER the v0.6.x worker is healthy (`kubectl -n gpustack-system rollout status
# deploy/gpustack-operator-worker`). Idempotent and safe to re-run.
set -uo pipefail

DRY_RUN=false
[ "${1:-}" = "--dry-run" ] && DRY_RUN=true

OLD_RE='^gpustack--'   # v0.5.x double-dash orphans only; v0.6.x single-dash never matches

run() { # echo + execute (or just echo under --dry-run)
  echo "  + $*"
  $DRY_RUN || "$@" >/dev/null 2>&1 || true
}

echo "[migrate] scanning for v0.5.x orphans (names matching ${OLD_RE})$($DRY_RUN && echo '  [DRY-RUN]')"

# 1. LocalQueues that point at an old (gpustack--) ClusterQueue, across all namespaces.
echo "[migrate] (1/4) orphaned LocalQueues"
kubectl get localqueue -A -o json 2>/dev/null | python3 -c '
import json, sys
for lq in json.load(sys.stdin).get("items", []):
    if lq.get("spec", {}).get("clusterQueue", "").startswith("gpustack--"):
        print(lq["metadata"]["namespace"], lq["metadata"]["name"])
' | while read -r ns name; do
  run kubectl -n "$ns" delete localqueue "$name" --ignore-not-found --timeout=60s
done

# 2. Old ClusterQueues. If one still holds workloads (a v0.5.x Instance was running across the
#    upgrade), drain it gracefully first — set StopPolicy=HoldAndDrain so Kueue evicts the admitted
#    workloads and releases the kueue.x-k8s.io/resource-in-use finalizer on its own, then delete once
#    drained. This mirrors the operator's own InstanceType retirement
#    (pkg/worker/controllers/worker/instancetype.go: HoldAndDrain -> wait !hasReserved -> delete).
#    Empty queues skip straight to delete (no needless wait). NOTE: draining EVICTS those workloads;
#    because the queue names changed in v0.6.x they must be re-created under the new pool's queue.
echo "[migrate] (2/4) orphaned ClusterQueues (drain first if they still hold workloads)"
for name in $(kubectl get clusterqueue -o name 2>/dev/null | sed 's#.*/##' | grep -E "$OLD_RE"); do
  reserving=$(kubectl get clusterqueue "$name" -o jsonpath='{.status.reservingWorkloads}' 2>/dev/null)
  admitted=$(kubectl get clusterqueue "$name" -o jsonpath='{.status.admittedWorkloads}' 2>/dev/null)
  if [ "${reserving:-0}" != "0" ] || [ "${admitted:-0}" != "0" ]; then
    echo "  ${name}: holds workloads (reserving=${reserving:-0}, admitted=${admitted:-0}) -> HoldAndDrain, wait, delete"
    run kubectl patch clusterqueue "$name" --type=merge -p '{"spec":{"stopPolicy":"HoldAndDrain"}}'
    if ! $DRY_RUN; then
      for _ in $(seq 1 20); do   # up to ~2min for Kueue to evict and release reservations
        r=$(kubectl get clusterqueue "$name" -o jsonpath='{.status.reservingWorkloads}' 2>/dev/null)
        a=$(kubectl get clusterqueue "$name" -o jsonpath='{.status.admittedWorkloads}' 2>/dev/null)
        [ "${r:-0}" = "0" ] && [ "${a:-0}" = "0" ] && break
        sleep 6
      done
    fi
  fi
  run kubectl delete clusterqueue "$name" --ignore-not-found --timeout=60s
done

# 3. Old Cohorts, 4. Old ResourceFlavors. ResourceFlavors LAST so Kueue has already released the
#    resource-in-use finalizer their (now deleted) ClusterQueues held on them.
for kind in cohort resourceflavor; do
  echo "[migrate] ($( [ "$kind" = cohort ] && echo 3/4 || echo 4/4 )) orphaned ${kind}s"
  for name in $(kubectl get "$kind" -o name 2>/dev/null | sed 's#.*/##' | grep -E "$OLD_RE"); do
    run kubectl delete "$kind" "$name" --ignore-not-found --timeout=60s
  done
done

# 5. Anything still Terminating carries a lingering finalizer (Kueue's resource-in-use, e.g. when
#    a workload was still admitted, or after a transient API blip). Strip it and let the delete finish.
if ! $DRY_RUN; then
  echo "[migrate] (5) re-stripping finalizers on anything still stuck Terminating"
  for kind in localqueue clusterqueue cohort resourceflavor; do
    if [ "$kind" = localqueue ]; then
      kubectl get localqueue -A -o json 2>/dev/null | python3 -c '
import json, sys
for lq in json.load(sys.stdin).get("items", []):
    if lq["metadata"]["name"].startswith("gpustack--") or lq.get("spec", {}).get("clusterQueue", "").startswith("gpustack--"):
        print(lq["metadata"]["namespace"], lq["metadata"]["name"])
' | while read -r ns name; do
        echo "  ~ strip localqueue ${ns}/${name}"
        kubectl -n "$ns" patch localqueue "$name" --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
      done
    else
      for name in $(kubectl get "$kind" -o name 2>/dev/null | sed 's#.*/##' | grep -E "$OLD_RE"); do
        echo "  ~ strip ${kind}/${name}"
        kubectl patch "$kind" "$name" --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
      done
    fi
  done
fi

# 6. Report residue and check node-label self-heal.
echo "[migrate] residue check"
residue=$(kubectl get resourceflavor,clusterqueue,cohort -o name 2>/dev/null | sed 's#.*/##' | grep -Ec "$OLD_RE" || true)
residue_lq=$(kubectl get localqueue -A -o json 2>/dev/null | python3 -c '
import json, sys
print(sum(1 for lq in json.load(sys.stdin).get("items", []) if lq.get("spec", {}).get("clusterQueue", "").startswith("gpustack--")))
' 2>/dev/null || echo "?")
echo "  gpustack-- RF/CQ/Cohort remaining: ${residue}"
echo "  LocalQueues pointing at old CQ:    ${residue_lq}"

stale_labels=$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
n = 0
for node in json.load(sys.stdin).get("items", []):
    for k in node["metadata"].get("labels", {}):
        if ".z-" in k or "generic-ln-x64" in k:
            n += 1
print(n)
' 2>/dev/null || echo "?")
if [ "${stale_labels}" != "0" ]; then
  echo "  WARNING: ${stale_labels} stale v0.5.x node label(s) still present — NFD should drop them;"
  echo "           if they persist, delete the <node>-gpustack-worker NodeFeature and let the worker re-report."
else
  echo "  node labels: clean (v0.5.x .z-*/generic-ln-x64 self-healed)"
fi

if $DRY_RUN; then
  echo "[migrate] DRY-RUN complete — nothing was changed."
elif [ "${residue}" = "0" ] && [ "${residue_lq}" = "0" ]; then
  echo "[migrate] done — no v0.5.x orphans remain."
else
  echo "[migrate] some orphans still remain — re-run this script (a transient API error can skip a delete)."
fi
