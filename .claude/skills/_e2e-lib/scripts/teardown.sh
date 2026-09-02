#!/usr/bin/env bash
#
# Tear down an E2E deployment and remove every leftover `helm uninstall` does not take.
# MUTATING — the skill confirms before running this.
#
#   teardown.sh <NS> [WORKER_CERT_SECRET]
#
# The cleanup itself is DELEGATED to deploy/gpustack-operator/chart/files/cleanup.sh, the same file
# the chart's gated post-delete hook runs. This script adds only what is specific to a test run: the
# artifacts the cases create, the release uninstall, the Node extended resources GPUStack advertised,
# and a verdict on whether the gpustack CRDs actually drained.
#
# It used to carry its own copy of that logic, "so the skill does not depend on the deploy/ tree".
# The two drifted, and every difference favoured the copy being wrong:
#
#   - it read `.spec.versions[0].name` where cleanup.sh reads the STORAGE version, so on a CRD with
#     more than one version it addressed a kind that answers nothing and skipped the strip;
#   - it deleted every CRD matching `kueue.x-k8s.io` or `nfd.k8s-sigs.io` BY NAME, with no ownership
#     check — so on a cluster that already ran its own Kueue, an e2e teardown deleted that Kueue's
# CRDs and every custom resource under them. cleanup.sh requires BOTH Helm ownership annotations
#     to match this release before touching those groups, and leaves NFD's alone entirely because
#     its subchart ships them unannotated and there is nothing to read ownership from;
#   - it deleted APIServices and webhook configurations by name pattern, where cleanup.sh first
#     confirms the Service they point at lives in this namespace.
#
# Duplication was the cause, not the drift: the copy was written once and then improved on one side
# only. Delegating removes the second copy rather than re-synchronising it.
#
# Idempotent and safe to re-run. NEVER deletes namespaces — the gpustack-system namespace is
# intentionally KEPT, because deleting it can hang in Terminating on orphaned aggregated APIServices.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: teardown.sh <NS> [WORKER_CERT_SECRET]}"
WORKER_CERT_SECRET="${2:-gpustack-operator-worker-cert}"

# FIXED, deliberately, and case-2.sh fixes it the same way. An overridable release name reads like a
# courtesy and is not one: the chart derives other names from it — the worker Certificate and its
# Secret through `worker.fullname`, which `fullnameOverride` can move again — so honouring RELEASE
# here without resolving those leaves the renamed release's Secret behind on every teardown. Nothing
# in this suite passes an override, so the choice is between a name every script agrees on and a
# parameter that is correct in one script and silently wrong in the next.
RELEASE=gpustack-operator

# Resolved from this script's own location rather than from the working directory. The pinned helm
# lives at the repository root, and a relative `.sbin/helm` test silently falls through to a PATH
# helm whenever the caller runs from anywhere else — which is how a 3.13 client without
# --take-ownership gets used by a suite that needs it.
REPO_ROOT="$(cd "$(dirname "$0")/../../../.." 2>/dev/null && pwd)"
CLEANUP="${REPO_ROOT}/deploy/gpustack-operator/chart/files/cleanup.sh"

# Put the pinned helm ahead of PATH rather than passing a path around: cleanup.sh calls a bare
# `helm`, and prepending here is what makes the delegate use the same binary this script does.
HELM=helm
if [ -x "${REPO_ROOT}/.sbin/helm" ]; then
  PATH="${REPO_ROOT}/.sbin:$PATH"
  HELM="${REPO_ROOT}/.sbin/helm"
fi

echo "[teardown] namespace=${NS}"

# 0. E2E test artifacts this skill creates. Delete the test Instance before the
# NodeFeatures so its Pod/Workload drain cleanly. Deleting the Worker-authored
#    <node>-gpustack-worker NodeFeature also discards any injected label edit.
kubectl -n default delete instance gpustack-e2e-instance --ignore-not-found 2>/dev/null || true
kubectl -n "$NS" delete nodefeature --all 2>/dev/null || true

# 1. The operator's own release (worker, device-managers, RBAC, webhooks). cleanup.sh does not do
#    this and must not: it runs as the chart's post-delete hook, at which point the release is
#    already being removed and uninstalling it again from inside would deadlock.
#
# The uninstall's outcome is judged on the RELEASE RECORD, not on its exit code, and neither is
#    discarded. Discarding it let an API/RBAC/hook failure pass silently while the record survived,
#    and the next deploy.sh then refuses to install over it — a teardown that printed `done`. The exit
#    code alone is not the judge either: a failing post-delete hook makes helm exit non-zero after the
#    record is already gone, which is not something to fail a teardown over. So the command is run,
#    its output kept, and the question re-asked.
# Both readings of the record go through release_present, in THREE states and not two.
#    `helm status` has only two: it exits non-zero for "no such release" AND for a timeout, an RBAC
#    denial or an unreachable API server, so a guard built on it reads a failed QUESTION as the
#    answer "absent". Here that lands twice — it would skip the uninstall over a release that is
#    still installed, and then, after a failed uninstall, report the record as gone when nothing
#    could see it. deploy.sh's install guard is built on `list -q` for exactly this reason; this
#    file kept `status` and so kept the conflation that guard exists to avoid.
#
# Matched with `grep -Fx` rather than helm's own --filter because the name is now a variable: a
#    regex metacharacter in an overridden RELEASE would silently widen a --filter match.
release_present() { # 0 = the record is there, 1 = it is not, 2 = the question could not be answered
  local out err
  err="$(mktemp)"
  if ! out="$("$HELM" list --all --namespace "$NS" -q 2>"$err")"; then
    RELEASE_QUERY_ERR="$(head -1 "$err")"
    rm -f "$err"
    return 2
  fi
  rm -f "$err"
  printf '%s\n' "$out" | grep -Fxq "$RELEASE"
}

if ! command -v "$HELM" >/dev/null 2>&1; then
  echo "[teardown] FATAL: no helm to run (${HELM}); the release cannot be uninstalled" >&2
  echo "[teardown] continuing without it would run the cleanup under a release still installed." >&2
  exit 1
fi

release_present
case $? in
  2)
    echo "[teardown] FATAL: cannot tell whether ${RELEASE} is installed in ${NS}:" >&2
    echo "[teardown]   ${RELEASE_QUERY_ERR}" >&2
    echo "[teardown] treating that as 'not installed' is what leaves a release behind that the" >&2
    echo "[teardown] next install then refuses to run over, naming a cause that is not this." >&2
    exit 1
    ;;
  0)
    echo "[teardown] helm uninstall ${RELEASE}"
    if ! uninstall_out="$("$HELM" uninstall "$RELEASE" -n "$NS" 2>&1)"; then
      release_present
      case $? in
        0)
          echo "[teardown] FATAL: helm uninstall failed and the release record is still present:" >&2
          printf '%s\n' "$uninstall_out" | head -5 >&2
          echo "[teardown] the next install refuses to run over it, naming a cause that is not this." >&2
          echo "[teardown] (namespace ${NS} kept on purpose)" >&2
          exit 1
          ;;
        2)
          echo "[teardown] FATAL: helm uninstall failed and whether the record survived it could" >&2
          echo "[teardown] not be established: ${RELEASE_QUERY_ERR}" >&2
          printf '%s\n' "$uninstall_out" | head -5 >&2
          exit 1
          ;;
        *)
          echo "[teardown] helm uninstall exited non-zero but the release record is gone; continuing"
          printf '%s\n' "$uninstall_out" | head -3
          ;;
      esac
    fi
    ;;
esac

# 2. Everything else, from the chart's own cleanup: the releases the operator chart does not own,
#    the aggregated APIServices and webhooks, the finalizers that pin objects once their controllers
#    are gone, the CRDs this release owns, the runtime Secrets and Leases, a failed migration hook's
#    leftovers, and the orphaned cluster-scoped objects whose release record died with a namespace.
#
# Missing rather than optional. Running the rest of a teardown without it leaves a cluster that
#    looks torn down and refuses the next install, so a missing delegate is a hard failure and not a
#    skipped step.
# Invoked through `bash` and tested for READABILITY, not for the execute bit. The chart's own
#    hook runs it the same way (`/usr/bin/env bash /scripts/cleanup.sh`), the file is committed
#    without +x, and a `-x` test here would refuse to run a delegate that works perfectly.
if [ ! -r "$CLEANUP" ]; then
  echo "[teardown] FATAL: cannot read ${CLEANUP}" >&2
  echo "[teardown] the cleanup is delegated to the chart's own script; there is no local copy" >&2
  exit 1
fi
echo "[teardown] delegating to chart cleanup: ${CLEANUP}"
bash "$CLEANUP" "$NS" "$WORKER_CERT_SECRET" "$RELEASE" || {
  echo "[teardown] FATAL: chart cleanup failed; see its output above" >&2
  exit 1
}

# 3. E2E-ONLY: reverse-patch the Node extended resources GPUStack advertised, so the NEXT case sees a
#    pristine node. cleanup.sh deliberately does not do this — a post-delete hook has no business
#    editing Node status, and outside a test the keys are harmless.
#
# Node extended resources do not self-remove when their advertiser is gone, and the two families
#    clear differently — the removal below is genuinely needed for one and only cosmetic for the other:
#      - reconciler-owned counting keys ("<vendor>.com/gpu.sliced.units|.cores-percentage|
#        .memory-percentage|.memory-mib", "<vendor>.com/gpu.partitioned.units",
#        "<vendor>.com/gpu.partitioned.<kind>-<profile>") are written by the NodeCapacityReconciler.
# Nothing removes them once the worker is uninstalled, so this JSON patch is the ONLY thing that
#        clears them;
#      - device-plugin-owned pool keys ("<vendor>.com/gpu.shared", "<vendor>.com/gpu.sliced",
#        "<vendor>.com/gpu.partitioned", "device.gpustack.ai/<vendor>.visibility") only ZERO OUT when the
#        plugin exits. The patch removes the entry, but the kubelet re-adds a zero-valued one on its next
#        status sync — full removal needs a kubelet restart, which this script deliberately does not do.
# So the sweep leaves the node clean for the next case, NOT provably key-free.
# Sweep the GPUStack-OWNED keys from status.capacity+allocatable on every node: device.gpustack.ai/*,
#    any "/gpu.sliced" key, any "/gpu.partitioned" key, and "*/gpu.shared". The "/gpu.sliced" prefix match
#    also catches the pre-split per-profile MIG shape — a "mig-<profile>" segment appended to the
# LOGICAL family's key, which the split replaced and which no component owns any more, so a
#    development node an earlier build wrote it onto keeps it otherwise. The bare
#    whole-card "<vendor>.com/gpu" is deliberately LEFT ALONE — it is name-identical to a real vendor
#    device-plugin's resource, so removing it generically is unsafe; it zeroes out on the GPUStack
#    plugin's exit. Requires python3 (already a hard dependency of the case scripts) and a kubectl new
#    enough for --subresource.
for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
  patch=$(kubectl get node "$node" -o json 2>/dev/null | python3 -c '
import json, sys
o = json.load(sys.stdin); ops = []
esc = lambda k: k.replace("~", "~0").replace("/", "~1")
owned = lambda k: (
    k.startswith("device.gpustack.ai/")
    or "/gpu.sliced" in k
    or "/gpu.partitioned" in k
    or k.endswith("/gpu.shared")
)
for sect in ("capacity", "allocatable"):
    for k in o.get("status", {}).get(sect, {}):
        if owned(k):
            ops.append({"op": "remove", "path": "/status/%s/%s" % (sect, esc(k))})
print(json.dumps(ops))
' 2>/dev/null)
  if [ -n "$patch" ] && [ "$patch" != "[]" ]; then
    echo "[teardown] reverse-patching gpustack extended resources on node ${node}"
    kubectl patch node "$node" --subresource=status --type=json -p "$patch" >/dev/null 2>&1 || true
  fi
done

# 4. Verdict on the gpustack CRDs, which cleanup.sh cannot give: it runs as a post-delete hook, where
#    failing would leave the release stuck, so it drains what it can and returns success either way.
# A test harness needs the opposite — a CRD still Terminating here is not a slow delete that will
#    finish on its own. It is waiting on customresourcecleanup for CRs whose finalizers nothing is
#    left to remove, and it waits forever. Passing over it leaves a WEDGED cluster that every signal
#    calls clean, and the next install walks into Terminating CRDs with no idea why.
#
# ONLY the gpustack groups are judged. The Kueue CRDs are deleted by cleanup.sh only when this
#    release owns them and NFD's are never deleted at all, so their presence here is not a failure —
#    and asserting on them is what the old copy of this logic did wrong in the first place.
# The LISTING is checked before its result is. `kubectl get crd 2>/dev/null | grep || true`
#    reports an empty set for a transport error, an RBAC denial and a genuinely clean cluster alike —
#    and empty is what this verdict reads as success, so a failed query would print "done" over a
#    wedged cluster. That is the same shape this file's own guards exist to prevent.
if ! crd_list="$(kubectl get crd -o name 2>&1)"; then
  echo "[teardown] FATAL: could not list CRDs, so the drain cannot be judged:" >&2
  echo "$crd_list" | head -3 >&2
  echo "[teardown] (namespace ${NS} kept on purpose)" >&2
  exit 1
fi
remaining="$(printf '%s\n' "$crd_list" | grep -E '\.(worker\.)?gpustack\.ai$' || true)"

# A CRD still here after cleanup.sh's own ~60s drain loop is given one more window before it is
#    called wedged: under load a large CR set can genuinely still be finalizing, and failing on that
#    turns a slow teardown into a reported wedge. Progress is what distinguishes them — a set that is
#    still shrinking is draining, one that has not moved is waiting on finalizers nothing will remove.
# Three rules this loop is built on, all three learned the hard way:
#      * EVERY re-list is checked, not just the first. `| grep || true` on a failed query yields the
#        empty string, and empty is what the exit below reads as clean — so an API server that went
#        away mid-drain would print "done" over the wedged cluster the first check exists to catch.
#      * The full window is always waited out. Breaking out early on an unchanged listing shortened
#        the documented 30s to 6s and turned a slow drain into a reported wedge — a CRD stays listed
#        while any number of its CRs are still finalizing, so an unchanged NAME LIST is not proof
#        that nothing is happening. Only reaching zero ends the wait early.
#      * The verdict reports what was actually measured. `|| continue` was a no-op — `continue` is
#        the last statement in the body either way — so nothing was ever compared, and every timeout
#        was announced as "stopped making progress". `moved` now records whether the set changed at
#        any point in the window, and the two outcomes are worded differently because they send an
#        operator to different places.
moved=""
if [ -n "$remaining" ]; then
  for _ in $(seq 1 10); do
    sleep 3
    previous="$remaining"
    if ! crd_list="$(kubectl get crd -o name 2>&1)"; then
      echo "[teardown] FATAL: could not re-list CRDs while waiting for the drain:" >&2
      echo "$crd_list" | head -3 >&2
      echo "[teardown] (namespace ${NS} kept on purpose)" >&2
      exit 1
    fi
    remaining="$(printf '%s\n' "$crd_list" | grep -E '\.(worker\.)?gpustack\.ai$' || true)"
    [ "$remaining" = "$previous" ] || moved=yes
    [ -z "$remaining" ] && break
  done
fi

if [ -n "$remaining" ]; then
  if [ -n "$moved" ]; then
    echo "[teardown] INCOMPLETE: gpustack CRDs were still draining when the 30s window closed:" >&2
  else
    echo "[teardown] INCOMPLETE: gpustack CRDs did not change over the whole 30s window:" >&2
  fi
  echo "$remaining" >&2
  echo "[teardown] an install against them fails in ways that do not name this as the cause." >&2
  echo "[teardown] (namespace ${NS} kept on purpose)" >&2
  exit 1
fi

echo "[teardown] done (namespace ${NS} kept on purpose)"
