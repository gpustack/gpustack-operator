#!/usr/bin/env bash
#
# CASE 3 — Release version survives a warm build cache   (OPTIONAL, MUTATING; local build only)
#
#   case-3.sh
#
# Goal:        A release-like version forced with the build cache already warm reaches BOTH the
#              binary and the bundled chart tgz — proving a version change busts the build cache key.
#              `make package` passes the resolved version as the GPUSTACK_GIT_VERSION build-arg, which
#              the builder stamps and folds into its cache key. See references/version-contract.md.
# Environment: A local Docker builder (docker + `make package`). No cluster and no GPU needed. Builds
#              a local image tagged dev-rel and never pushes. Run the install build once first so the
#              cache is warm (otherwise a clean build passes trivially).
# Inputs:      All real, nothing mocked — VERSION=v9.9.9 PACKAGE_TAG=dev-rel make package (a forced
#              release version over an already-warm cache).
# Expected:    - the built binary reports version v9.9.9 (the cache did not serve a stale binary);
#              - the bundled tgz is gpustack-operator-9.9.9.tgz (not 0.0.0 from a stale cache).
# Cleanup:     Nothing on a cluster — leaves only the local dev-rel image (never pushed).
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
