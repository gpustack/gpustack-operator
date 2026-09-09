#!/usr/bin/env bash
#
# CASE 2 — Uninstall leaves zero leftovers   (MUTATING; run LAST)
#
#   case-2.sh <NS>
#
# Goal:        The shared teardown (helm uninstall + the leftovers helm does not manage) leaves the
#              cluster clean — no leftover releases, CRDs, apiservices, or clusterrolebindings —
#              while the gpustack-system namespace is intentionally kept.
# Environment: Any reachable cluster with the chart installed. No GPU. DELETES the whole release,
#              which now includes Kueue / NFD / the CSI drivers and Kueue's CRDs, and with them
#              every ClusterQueue and Workload in the cluster — run ONLY as the final step.
# Inputs:      All real, nothing mocked — runs teardown.sh (helm uninstall; the releases the chart
#              does not own; CRDs, finalizers, APIServices/webhooks, migration-hook leftovers).
# Expected:    After teardown, zero leftover: helm releases (gpustack/kueue/nfd/csi), every
#              gpustack.ai CRD, every CRD this release owned, gpustack apiservices, and gpustack
#              clusterrolebindings.
# NOT every kueue/nfd CRD by name. The teardown delegates to the chart's own
#              cleanup.sh, which leaves NFD's CRDs in place entirely (its subchart ships them
#              unannotated, so there is nothing to read ownership from) and removes Kueue's only when
# THIS release owns them — so a cluster running its own Kueue keeps it. Asserting on the
#              group names would fail a CORRECT teardown, and demanding their removal is what the
#              old private copy of that cleanup did wrong.
# Cleanup:     This case IS the cleanup (teardown is idempotent, safe to re-run); the
#              gpustack-system namespace is kept on purpose.
set -uo pipefail

NS="${1:?usage: case-2.sh <NS>}"
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"

# The one place this file names the release, and it is FIXED — the same literal teardown.sh and
# deploy.sh use. Parameterizing it here made this case check ownership for a name the teardown never
# uninstalled, and chasing that further meant deriving the worker Certificate's Secret name from the
# chart's own `worker.fullname` in shell. One agreed literal is smaller and cannot drift.
RELEASE=gpustack-operator

# The pinned client, resolved exactly as teardown.sh resolves it and for the same reason: a 3.13 PATH
# helm lacks flags this suite needs. Asking with a DIFFERENT binary than the one that did the
# teardown is how an old client's refusal gets reported here as a leftover release rather than as the
# tooling problem it is.
REPO_ROOT="$(cd "$(dirname "$0")/../../../.." 2>/dev/null && pwd)"
HELM=helm
[ -n "$REPO_ROOT" ] && [ -x "${REPO_ROOT}/.sbin/helm" ] && HELM="${REPO_ROOT}/.sbin/helm"

# Tear everything down (delegates the cleanup to the chart's own files/cleanup.sh).
bash "$LIB/teardown.sh" "$NS"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
assert_empty() { # check  leftover-output
  if [ -z "$2" ]; then record PASS "$1" "none"; else record FAIL "$1" "$(echo "$2" | tr '\n' ' ' | cut -c1-60)"; fi
}

# EVERY leftover query goes through this, and that is the point. `cmd 2>/dev/null | grep` maps a
# transport error, an RBAC denial and a genuinely clean cluster onto the same empty string, and empty
# is exactly what assert_empty reads as PASS — so a failed question would report success over a wedged
# cluster. probe turns a failed query into a visible sentinel instead, which assert_empty then FAILs
# on. The ownership check below was written with sentinels for this reason; these four were not, which
# left two CRD assertions in one file disagreeing about whether a failed question counts as an answer.
probe() { # what-failed  grep-pattern  command...
  local what="$1" pattern="$2" out
  shift 2
  if ! out="$("$@" 2>&1)"; then
    echo "CHECK-BROKEN: ${what} failed: $(printf '%s' "$out" | head -1)"
    return 0
  fi
  printf '%s\n' "$out" | grep -E "$pattern" || true
}

# The releases THIS harness installs, by exact name — not a `gpustack|kueue|nfd|csi` pattern.
# That pattern also matches a standalone `kueue`, `nfd` or CSI release the cluster brought itself,
# which the teardown deliberately preserves (see the header). It would therefore FAIL a CORRECT
# teardown on the very "cluster already runs its own Kueue" topology this file promises keeps
# working — the same mistake as deciding CRD ownership by group name, one object kind up, and the
# mistake teardown.sh's own header records as the reason its private copy was deleted.
OWNED_RELEASES=(
  "$RELEASE"
  "${RELEASE}-device-manager"
  gpustack-kueue
  gpustack-node-feature-discovery
  gpustack-csi-driver-nfs
  gpustack-csi-driver-s3
)
assert_empty "no leftover releases" "$(
  if ! helm_out="$("$HELM" list --all -q -n "$NS" 2>&1)"; then
    echo "CHECK-BROKEN: helm list failed: $(printf '%s' "$helm_out" | head -1)"
  else
    printf '%s\n' "$helm_out" \
      | grep -Fxf <(printf '%s\n' "${OWNED_RELEASES[@]}") || true
  fi
)"
assert_empty "no leftover gpustack CRDs" \
  "$(probe 'kubectl get crd' 'gpustack\.ai' kubectl get crd)"
# Ownership, not name pattern: this is what still catches a Kueue CRD the release installed and
# failed to remove, without failing on one the cluster brought itself. Read from the Helm annotation
# rather than the managed-by label, because only the annotation names WHICH release.
# BOTH annotations, matched as a PAIR, exactly as cleanup.sh's own owned_crds() decides ownership.
# CRDs are cluster-scoped, so unlike every other ownership probe in this suite there is no `-n $NS` on
# the query to pin the namespace for us. release-name alone is satisfied by a co-located Kueue
# installed as its own release that happens to be called gpustack-operator in another namespace —
# cleanup.sh correctly leaves that one alone, and this check would then report it as a leftover and
# FAIL a CORRECT teardown. That is the very "cluster running its own Kueue" case this file's header
# says must keep working. The two checks have to decide ownership by the same rule or one of them is
# wrong by construction.
# Every failure path here reports a SENTINEL, never silence. An empty result is what this assertion
# reads as success, so a missing python3, an unreachable API server or a non-JSON body would all have
# passed the check they exist to make — the same "a failed question answered as the answer" shape the
# deploy guard above is built to avoid.
owned_crds="$(
  if ! command -v python3 >/dev/null 2>&1; then
    echo "CHECK-BROKEN: python3 is not on PATH, so ownership could not be read"
  elif ! crd_json="$(kubectl get crd -o json 2>&1)"; then
    echo "CHECK-BROKEN: kubectl get crd failed: $(echo "$crd_json" | head -1)"
  else
    printf '%s' "$crd_json" | NS="$NS" RELEASE="$RELEASE" python3 -c '
import json, os, sys
try:
    doc = json.load(sys.stdin)
except Exception as e:
    print("CHECK-BROKEN: crd list is not valid json: %s" % e)
    sys.exit(0)
ns, release = os.environ["NS"], os.environ["RELEASE"]
for o in doc.get("items", []):
    meta = o.get("metadata", {})
    ann = meta.get("annotations") or {}
    if (ann.get("meta.helm.sh/release-name") == release
            and ann.get("meta.helm.sh/release-namespace") == ns):
        print(meta.get("name", ""))
'
  fi
)"
assert_empty "no CRDs still owned by this release" "$owned_crds"
assert_empty "no leftover apiservices" \
  "$(probe 'kubectl get apiservice' 'gpustack' kubectl get apiservice)"
assert_empty "no leftover rolebindings" \
  "$(probe 'kubectl get clusterrolebinding' 'gpustack' kubectl get clusterrolebinding)"

echo
echo "== CASE 2 — Uninstall leaves zero leftovers =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s) — re-run teardown.sh (idempotent), or see ../_e2e-lib/references/troubleshooting.md"
  echo "for stuck CRDs/finalizers."
  exit 1
fi
echo "CASE 2 PASS"
