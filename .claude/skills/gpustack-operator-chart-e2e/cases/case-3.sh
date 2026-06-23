#!/usr/bin/env bash
#
# CASE 3 — Release version survives a warm build cache   (OPTIONAL, MUTATING)
#
#   case-3.sh
#
# The chart version is only as trustworthy as the build. This forces a release-like
# version with the build cache already warm and confirms the version reaches BOTH
# the binary and the bundled chart tgz. `make package` passes the resolved version
# as the GPUSTACK_GIT_VERSION build-arg, which the builder stamps and folds into its
# cache key — so a version change forces a rebuild. See references/version-contract.md.
#
# Builds a local image tagged dev-rel; never pushes. Run §1 build once first so the
# cache is warm.
set -uo pipefail

IMG=gpustack/gpustack-operator:dev-rel

echo "== build with a forced release version (cache warm) =="
VERSION=v9.9.9 PACKAGE_TAG=dev-rel make package

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

verline=$(docker run --rm "$IMG" gpustack-operator --version 2>/dev/null)
case "$verline" in
  *v9.9.9*) record PASS "binary stamped with forced version" "v9.9.9" ;;
  *)        record FAIL "binary stamped with forced version" "got [${verline:-none}] — cache served a stale binary (version did not bust the cache key)" ;;
esac

charts=$(docker run --rm "$IMG" ls /etc/gpustack/charts/ 2>/dev/null)
case "$charts" in
  *gpustack-operator-9.9.9.tgz*) record PASS "tgz packaged at forced version" "gpustack-operator-9.9.9.tgz" ;;
  *)                             record FAIL "tgz packaged at forced version" "got [${charts:-none}] — expected gpustack-operator-9.9.9.tgz (0.0.0 means stale cache)" ;;
esac

echo
echo "== CASE 3 — Release version survives a warm build cache =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The realistic release trigger is a git tag: on a CLEAN tree"
  echo "'git tag v9.9.9 && make package' (then 'git tag -d v9.9.9') reproduces the same path."
  echo "See references/version-contract.md."
  exit 1
fi
echo "CASE 3 PASS"
