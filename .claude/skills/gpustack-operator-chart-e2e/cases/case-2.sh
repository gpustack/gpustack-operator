#!/usr/bin/env bash
#
# CASE 2 — Uninstall leaves zero leftovers   (MUTATING; run LAST)
#
#   case-2.sh <NS>
#
# Goal:        The shared teardown (helm uninstall + the runtime leftovers helm does not manage)
#              leaves the cluster clean — no leftover releases, CRDs, apiservices, or
#              clusterrolebindings — while the gpustack-system namespace is intentionally kept.
# Environment: Any reachable cluster with the chart installed. No GPU. DELETES the operator
#              deployment + worker-installed sub-releases — run ONLY as the final step.
# Inputs:      All real, nothing mocked — runs teardown.sh (helm uninstall; worker-installed
#              sub-releases, their CRDs, finalizers, and runtime APIServices/webhooks).
# Expected:    After teardown, zero leftover: helm releases (gpustack/kueue/nfd/csi), CRDs
#              (gpustack.ai / kueue.x-k8s.io / nfd.k8s-sigs.io), gpustack apiservices, and gpustack
#              clusterrolebindings.
# Cleanup:     This case IS the cleanup (teardown is idempotent, safe to re-run); the
#              gpustack-system namespace is kept on purpose.
set -uo pipefail

NS="${1:?usage: case-2.sh <NS>}"
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"

# Tear everything down (self-contained; mirrors the chart's cleanup.sh).
bash "$LIB/teardown.sh" "$NS"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
assert_empty() { # check  leftover-output
  if [ -z "$2" ]; then record PASS "$1" "none"; else record FAIL "$1" "$(echo "$2" | tr '\n' ' ' | cut -c1-60)"; fi
}

assert_empty "no leftover releases"     "$(helm list -n "$NS" 2>/dev/null | grep -E 'gpustack|kueue|nfd|node-feature|csi')"
assert_empty "no leftover CRDs"         "$(kubectl get crd 2>/dev/null | grep -E 'gpustack\.ai|kueue\.x-k8s\.io|nfd\.k8s-sigs\.io')"
assert_empty "no leftover apiservices"  "$(kubectl get apiservice 2>/dev/null | grep gpustack)"
assert_empty "no leftover rolebindings" "$(kubectl get clusterrolebinding 2>/dev/null | grep gpustack)"

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
